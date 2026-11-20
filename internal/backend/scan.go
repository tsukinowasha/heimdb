package backend

import (
	"io"
	"slices"
	"sync/atomic"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 行とインデックスのエントリを、集めずに 1 件ずつ読む走査（docs/plan.md の段階 55）。
//
// 走査は文の始め（PartitionRows）に開く。トランザクションのバッチを含む走査は開いた時点の変更だけを見るので、同じ文が
// 後から書いた行は見えない（INSERT ... SELECT で同じ表を読む、主キーを変える UPDATE）。前は当たる行をすべて集めてから
// 返していたので、見え方は同じ。区間が複数あるときも、同じ走査の区間を移して読む（store.Scan.Reset）。

// stmtCloser は、文の間だけ開いておくもの（走査、RowReader、それを読むトランザクション）の後始末。エンジンが Close を
// 呼ばずに文を終えても、文の終わり（Session.CommandEnd）で閉じる（閉じないと、Pebble のイテレータとスナップショット
// が残り、GC の境界が止まる）。
type stmtCloser struct {
	fn   func()
	sess *Session
}

// onStatementEnd は、fn を文の終わりまでに 1 回呼ぶ stmtCloser を作る。先に close を呼べば、そのときに呼ぶ。
func onStatementEnd(ctx *sql.Context, fn func()) *stmtCloser {
	c := &stmtCloser{fn: fn}
	if s, ok := ctx.Session.(*Session); ok {
		c.sess = s
		s.closers = append(s.closers, c)
	}
	return c
}

// close は fn を呼ぶ。2 回目からは何もしない。
func (c *stmtCloser) close() {
	if c == nil || c.fn == nil {
		return
	}
	fn := c.fn
	c.fn = nil
	fn()
	if c.sess != nil {
		c.sess.closers = slices.DeleteFunc(c.sess.closers, func(x *stmtCloser) bool { return x == c })
	}
}

// scanHandle は、開いた走査（閉じると読んでいたトランザクションも手放す）。reads は、読んだエントリを数える文の
// readStats（遅い文のログ。段階 81。HeimDB のセッションの外なら nil）。
type scanHandle struct {
	sc     *store.Scan
	closer *stmtCloser
	reads  *readStats
	// pool は、閉じたら文の終わりまで使い回すときの置き場所（SELECT の文だけ。scanPool）。nil なら閉じる。
	pool *scanPool
	key  scanKey
}

// scansOpened は、新しく開いた走査の数（使い回した走査は数えない。テスト用）。
var scansOpened atomic.Int64

// scanKey は、使い回せる走査の見分け（同じ表の同じインデックスを同じ向きに、同じ時点で読む）。
type scanKey struct {
	branch, table, index uint64 // index は、主キーの行の走査なら 0
	asOf                 uint64
	reverse              bool
}

// scanPool は、1 つの文の中で閉じた走査を、同じ文の次の走査に使い回す置き場所（docs/plan.md の段階 83）。JOIN で左の
// 行ごとに右のインデックスを引くと、そのたびに Pebble のイテレータを作って閉じていた（記事 2 万件の meta_query で
// CPU の 1 割強）。走査の区間を移す store.Scan.Reset は同じ見え方のままなので、文の中で書かない SELECT の文に限って
// 使い回す（書く文は、文の途中の書き込みを見るために、引くたびに開き直す）。文の終わりに閉じる（stmtCloser）。
type scanPool struct {
	idle map[scanKey][]*scanHandle
}

// openScan は、表 t の走査を開く。open はトランザクションで、openAsOf は AS OF で開く。文の中で使い回せる走査が
// あれば、区間を lo, hi に移して使う。
func (t *Table) openScan(ctx *sql.Context, key scanKey, lo, hi []byte, open func(*store.Txn) (*store.Scan, error), openAsOf func() (*store.Scan, error)) (*scanHandle, error) {
	var pool *scanPool
	if s, ok := ctx.Session.(*Session); ok && s.cmdScans != nil {
		pool = s.cmdScans
		if hs := pool.idle[key]; len(hs) > 0 {
			h := hs[len(hs)-1]
			pool.idle[key] = hs[:len(hs)-1]
			h.sc.Reset(lo, hi)
			return h, nil
		}
	}
	scansOpened.Add(1)
	release := func() {}
	var sc *store.Scan
	var err error
	if t.asOf != store.Latest {
		sc, err = openAsOf()
	} else {
		var txn *store.Txn
		txn, release = txnFor(ctx, t.db.st)
		if sc, err = open(txn); err != nil {
			release()
		}
	}
	if err != nil {
		return nil, err
	}
	h := &scanHandle{sc: sc, reads: statementReads(ctx), pool: pool, key: key}
	h.closer = onStatementEnd(ctx, func() {
		_ = sc.Close()
		h.sc = nil
		release()
	})
	return h, nil
}

// close は走査を閉じる（使い回すなら、文の終わりまで置き場所に戻す）。2 回目からは何もしない。
func (h *scanHandle) close() {
	if h.pool != nil && h.sc != nil {
		if !slices.Contains(h.pool.idle[h.key], h) {
			h.pool.idle[h.key] = append(h.pool.idle[h.key], h)
		}
		return
	}
	h.closer.close()
}

// openRowScan は、主キーの区間 ivs（lo の順。reverse なら逆順に読む）の行の走査を開く。ivs が空なら nil。
func (t *Table) openRowScan(ctx *sql.Context, ivs []interval, reverse bool) (*scanHandle, []interval, error) {
	ivs = orderIntervals(ivs, reverse)
	if len(ivs) == 0 {
		return nil, nil, nil
	}
	first := ivs[0]
	if r := statementReads(ctx); r != nil {
		r.noteIndex(t.def.Name, "PRIMARY")
	}
	h, err := t.openScan(ctx, scanKey{t.db.branch, t.def.ID, 0, t.asOf, reverse}, first.lo, first.hi, func(txn *store.Txn) (*store.Scan, error) {
		return txn.RowScan(t.db.branch, t.def.ID, first.lo, first.hi, reverse)
	}, func() (*store.Scan, error) {
		return t.db.st.RowScanAsOf(t.db.branch, t.def.ID, t.asOf, first.lo, first.hi, reverse)
	})
	return h, ivs, err
}

// openIndexScan は、インデックス ki のエントリの区間 ivs の走査を開く（openRowScan を参照）。
func (t *Table) openIndexScan(ctx *sql.Context, ki keyIndex, ivs []interval, reverse bool) (*scanHandle, []interval, error) {
	ivs = orderIntervals(ivs, reverse)
	if len(ivs) == 0 {
		return nil, nil, nil
	}
	first := ivs[0]
	if r := statementReads(ctx); r != nil {
		r.noteIndex(t.def.Name, ki.def.Name)
	}
	h, err := t.openScan(ctx, scanKey{t.db.branch, t.def.ID, ki.def.ID, t.asOf, reverse}, first.lo, first.hi, func(txn *store.Txn) (*store.Scan, error) {
		return txn.IndexScan(t.db.branch, t.def.ID, ki.def.ID, first.lo, first.hi, reverse)
	}, func() (*store.Scan, error) {
		return t.db.st.IndexScanAsOf(t.db.branch, t.def.ID, ki.def.ID, t.asOf, first.lo, first.hi, reverse)
	})
	return h, ivs, err
}

// orderIntervals は、重ならず lo の順の区間を、読む順に並べる（逆順なら後ろの区間から読めば、全体がキーの逆順になる）。
func orderIntervals(ivs []interval, reverse bool) []interval {
	if reverse {
		ivs = slices.Clone(ivs)
		slices.Reverse(ivs)
	}
	return ivs
}

// intervalScan は、区間を順に移しながら走査のエントリを返す。
type intervalScan struct {
	h    *scanHandle
	ivs  []interval
	i    int
	done bool // 閉じた（使い回しの置き場所に戻した走査は、同じ文のほかの検索が読み直すので、もう読まない。段階 97）
}

// next は次のエントリと値を返す。終わりなら ok が false。返したものは写していないので、次に next か close を呼ぶまで
// しか使えない（store.Scan.NextView。持ち続けるなら写す。docs/plan.md の段階 103）。
func (s *intervalScan) next() (entry, value []byte, ok bool, err error) {
	for !s.done && s.h != nil && s.h.sc != nil {
		entry, value, ok, err = s.h.sc.NextView()
		if ok && s.h.reads != nil {
			s.h.reads.rows.Add(1)
		}
		if err != nil || ok {
			return entry, value, ok, err
		}
		s.i++
		if s.i >= len(s.ivs) {
			return nil, nil, false, nil
		}
		s.h.sc.Reset(s.ivs[s.i].lo, s.ivs[s.i].hi)
	}
	return nil, nil, false, nil
}

func (s *intervalScan) close() {
	if s.done {
		return // 2 回目は戻さない（ほかの検索が使っている走査を、置き場所に重ねて戻さない）
	}
	s.done = true
	if s.h != nil {
		s.h.close()
	}
}

// scanRowIter は、主キーの区間の行を 1 行ずつ復号して返す sql.RowIter。filter があれば真の行だけを返す。
type scanRowIter struct {
	t      *Table
	scan   intervalScan
	want   []bool
	filter sql.Expression
	read   int // 読んだ行の数
	n      int // 返した行の数（ALTER の途中の警告の行番号）
	// scratch は、復号する行を使い回す置き場所（reuse のときだけ。rowReuser を参照）。
	scratch sql.Row
	reuse   bool
}

// newScanRowIter は、主キーの区間 ivs の行を返す sql.RowIter を作る（ivs が nil なら表のすべての行。空の区間の
// 並びは渡さない）。
func (t *Table) newScanRowIter(ctx *sql.Context, ivs []interval, reverse bool, want []bool, filter sql.Expression) (*scanRowIter, error) {
	if ivs == nil {
		ivs = []interval{{}}
	}
	h, ivs, err := t.openRowScan(ctx, ivs, reverse)
	if err != nil {
		return nil, err
	}
	return &scanRowIter{t: t, scan: intervalScan{h: h, ivs: ivs}, want: want, filter: filter}, nil
}

func (it *scanRowIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		_, value, ok, err := it.scan.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			it.scan.close()
			return nil, io.EOF
		}
		it.read++
		if err := interrupted(ctx, it.read); err != nil {
			return nil, err
		}
		var row sql.Row
		if it.reuse {
			row, err = it.t.decodeRowColumnsInto(value, it.want, it.scratch)
			it.scratch = row
		} else {
			row, err = it.t.decodeRowColumns(value, it.want)
		}
		if err != nil {
			return nil, err
		}
		if it.filter != nil {
			v, err := it.filter.Eval(ctx, row)
			if err != nil {
				return nil, err
			}
			if b, ok := v.(bool); !ok || !b {
				continue
			}
		}
		it.n++
		if it.t.nullFill != nil {
			if err := it.t.nullFill.applyRow(ctx, row, it.n); err != nil {
				return nil, err
			}
		}
		return row, nil
	}
}

func (it *scanRowIter) Close(*sql.Context) error {
	it.scan.close()
	return nil
}

// rowReuser は、返す行を次の Next で使い回せるイテレータ（scanRowIter と、エントリだけで行を作る pkRowIter）。
//
// 全件を読む文（承認済みのコメントの数、記事の年と月など）は、行ごとに全体のスキーマの幅の行を復号し、projectIter が
// 文の使う列だけの行に写していた。projectIter は子の行から値を写してすぐに捨てるので、子の行は使い回してよい
// （docs/plan.md の段階 125）。使い回すのは、projectRows が projectIter の子にしたイテレータだけ。列を絞らない文
// （行をそのまま返す）、ALTER の途中（NULL を埋める行に警告とエラーを付ける）、行を集める経路（キーを持たない
// インデックス、プレフィックス長の列の並べ替え。rowIter に集めてから返すので、ここには来ない）と、Truncate の
// 1 行ずつの削除（projectRows を通らない）では使い回さない。範囲の条件（rangeFilter）は、評価の間だけ行を見て
// 持たない。値（文字列とバイト列）は行ごとに作るので、使い回す行と値を共有しない。
type rowReuser interface {
	reuseRows()
}

func (it *scanRowIter) reuseRows() { it.reuse = true }

func (it *pkRowIter) reuseRows() { it.reuse = true }
