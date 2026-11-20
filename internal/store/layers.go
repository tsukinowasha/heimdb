package store

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// バージョン付きのキー空間（行、インデックスのエントリ、テーブル定義）の読み取り。
//
// キーは「<prefix><エントリ><反転時刻>」の形で、同じエントリのバージョンは新しい順に並ぶ。ブランチでは、自分のキー空間と、
// 親の分岐時刻以前のバージョン、その親のさらに分岐時刻以前のバージョン…を層として重ね、同じエントリは上の層（自分に近い層）が勝つ。
// 削除の印も勝つので、ブランチで消した行が親から透けて見えることはない（docs/plan.md の段階 6）。

// layer は重ね合わせの 1 層。maxTS より新しいバージョンは見えない（自分の層は Latest、親の層は分岐時刻）。
type layer struct {
	branch uint64
	maxTS  uint64
}

// layers は、ブランチ branch を読むときの層を、上（自分）から順に返す。
func (s *Store) layers(branch uint64) ([]layer, error) {
	s.branchesMu.RLock()
	defer s.branchesMu.RUnlock()
	out := []layer{{branch: branch, maxTS: Latest}}
	if IsHiddenBranch(branch) {
		return out, nil
	}
	cap := uint64(Latest)
	for b := branch; ; {
		info, ok := s.branches[b]
		if !ok {
			return nil, fmt.Errorf("HeimDB: unknown branch %d", b)
		}
		if info.Parent == 0 {
			return out, nil
		}
		cap = min(cap, info.ForkedAt)
		out = append(out, layer{branch: info.Parent, maxTS: cap})
		b = info.Parent
	}
}

// versionCursor は、1 つの層のキー空間を走査し、エントリごとに「見えるバージョンのうち最新のもの」を 1 つずつ返す。
// 削除の印も返す（上の層で下の層を隠すため）。
type versionCursor struct {
	iter    *pebble.Iterator
	prefix  []byte
	visible func(ts uint64) bool
	reverse bool

	// 今の位置のエントリと値（値は先頭の印を含む）と、そのバージョンの時刻。valid が false なら終わり。
	entry, value []byte
	ts           uint64
	valid        bool
	err          error

	last    []byte // 順方向で、返し終えたエントリ（残りのバージョンを読み飛ばす）
	hasLast bool

	// 区間の境界とキー空間の上限を作るバッファ（docs/plan.md の段階 103）。
	bounds, prefixEnd []byte
}

func newVersionCursor(newIter func(*pebble.IterOptions) (*pebble.Iterator, error), prefix []byte, visible func(uint64) bool, lo, hi []byte, reverse bool) (*versionCursor, error) {
	c := &versionCursor{prefix: prefix, visible: visible, reverse: reverse}
	lower, upper := c.cursorBounds(lo, hi)
	iter, err := newIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	c.iter = iter
	c.start()
	return c, nil
}

// cursorBounds は、キー空間 prefix のエントリの区間 [lo, hi)（hi が nil なら上限なし）のキーの境界を、カーソルの
// バッファに作る（Pebble は NewIter と SetBounds で境界を写すので、返った後に書き換えてよい。段階 103）。
func (c *versionCursor) cursorBounds(lo, hi []byte) (lower, upper []byte) {
	b := append(append(c.bounds[:0], c.prefix...), lo...)
	n := len(b)
	if hi != nil {
		b = append(append(b, c.prefix...), hi...)
	} else {
		if c.prefixEnd == nil {
			c.prefixEnd = keyenc.PrefixEnd(c.prefix)
		}
		b = append(b, c.prefixEnd...)
	}
	c.bounds = b
	return b[:n:n], b[n:]
}

// start は、区間の最初（reverse なら最後）のエントリに位置を合わせる。
func (c *versionCursor) start() {
	c.hasLast = false
	if c.reverse {
		c.iter.Last()
	} else {
		c.iter.First()
	}
	c.advance()
}

// reset は、同じイテレータのまま区間を [lo, hi) に移す。Pebble の SetBounds はバッチの見え方を新しくしない
// （SetOptions とは違う）ので、作った後のバッチへの書き込みは見えないまま。
func (c *versionCursor) reset(lo, hi []byte) {
	c.iter.SetBounds(c.cursorBounds(lo, hi))
	c.err = nil
	c.start()
}

func (c *versionCursor) close() error {
	return c.iter.Close()
}

// split はキーをエントリと時刻に分ける。
func (c *versionCursor) split(key []byte) (entry []byte, ts uint64, err error) {
	if len(key) < len(c.prefix)+8 {
		return nil, 0, fmt.Errorf("HeimDB: corrupt versioned key %x", key)
	}
	ts, _, err = keyenc.DecodeInvertedTime(key[len(key)-8:])
	return key[len(c.prefix) : len(key)-8], ts, err
}

// advance は次のエントリに進む。
func (c *versionCursor) advance() {
	c.valid = false
	if c.err != nil {
		return
	}
	if !c.reverse {
		// 同じエントリのバージョンは新しい順なので、見える最初のバージョンがそのエントリの値。
		for ; c.iter.Valid(); c.iter.Next() {
			entry, ts, err := c.split(c.iter.Key())
			if err != nil {
				c.err = err
				return
			}
			if c.hasLast && bytes.Equal(entry, c.last) {
				continue
			}
			if !c.visible(ts) {
				continue
			}
			c.last = append(c.last[:0], entry...)
			c.hasLast = true
			c.entry = append(c.entry[:0], entry...)
			c.value = append(c.value[:0], c.iter.Value()...)
			c.ts = ts
			c.valid = true
			c.iter.Next()
			return
		}
		c.err = c.iter.Error()
		return
	}
	// 逆方向では同じエントリのバージョンを古い順に見るので、見えるバージョンのうち最後に見たものがそのエントリの値。読んでいる
	// エントリと値は、返すエントリと値のバッファにそのまま置く（エントリごとに確保しない。段階 103。前に返した
	// エントリは、Scan が次に呼ばれてからカーソルを進めるので、もう使われていない）。エントリは空にならない（主キーを
	// 含む）ので、group が空なら、まだエントリを読んでいない。
	group, cand := c.entry[:0], c.value[:0]
	var candTS uint64
	hasCand := false
	for c.iter.Valid() {
		entry, ts, err := c.split(c.iter.Key())
		if err != nil {
			c.err = err
			return
		}
		if len(group) > 0 && !bytes.Equal(entry, group) {
			if hasCand {
				c.entry, c.value, c.ts, c.valid = group, cand, candTS, true
				return // イテレータは次のエントリの最初のバージョンにいる
			}
			group = group[:0]
		}
		if len(group) == 0 {
			group = append(group, entry...)
		}
		if c.visible(ts) {
			cand = append(cand[:0], c.iter.Value()...)
			candTS = ts
			hasCand = true
		}
		c.iter.Prev()
	}
	c.entry, c.value = group, cand
	if c.err = c.iter.Error(); c.err != nil {
		return
	}
	if hasCand {
		c.ts, c.valid = candTS, true
	}
}

// Scan は、層ごとのカーソルを重ねて、エントリの順（reverse なら逆順）に 1 件ずつ返す走査。同じエントリは上の層
// （cursors の先頭に近い方）が勝つ。勝ったバージョンが削除の印なら返さない。使い終わったら Close する。
//
// トランザクションのバッチを含む走査は、作った時点のバッチの変更だけを見る（Pebble のバッチのイテレータの性質）。
// 文の始めに作れば、同じ文が後から書いた行は見えない（docs/plan.md の段階 55）。
type Scan struct {
	cursors []*versionCursor
	reverse bool
	err     error
	closed  bool
	onClose func(*Scan)
	// pending は、NextView が返したエントリにいるカーソル（次に呼ばれたときに進める）。
	pending []int
}

// Next は次のエントリと値（先頭の印を除く）を返す。返す値は呼び出し側が持ち続けてよい写し。終わりなら ok が false。
func (sc *Scan) Next() (entry, value []byte, ok bool, err error) {
	e, v, ok, err := sc.NextView()
	if !ok || err != nil {
		return nil, nil, ok, err
	}
	// 返すエントリと値は、呼び出し側が持ち続けてよい写し（1 回の確保で両方を作る）
	buf := make([]byte, len(e)+len(v))
	entry, value = buf[:copy(buf, e):len(e)], buf[len(e):]
	copy(value, v)
	return entry, value, true, nil
}

// NextView は Next と同じだが、エントリと値を写さずに返す。返したものは、次に Next、NextView、Reset、Close を呼ぶまで
// しか使えない（docs/plan.md の段階 103。lookup JOIN でエントリから主キーを取るだけでも、エントリごとに写していた）。
// 返したエントリにいるカーソルは、次に呼ばれたときに進める。
func (sc *Scan) NextView() (entry, value []byte, ok bool, err error) {
	if sc.err != nil {
		return nil, nil, false, sc.err
	}
	if sc.closed {
		return nil, nil, false, nil
	}
	for {
		for _, i := range sc.pending {
			sc.cursors[i].advance()
		}
		sc.pending = sc.pending[:0]
		best := -1
		for i, c := range sc.cursors {
			if c.err != nil {
				sc.err = c.err
				return nil, nil, false, c.err
			}
			if !c.valid {
				continue
			}
			if best < 0 {
				best = i
				continue
			}
			cmp := bytes.Compare(c.entry, sc.cursors[best].entry)
			if (!sc.reverse && cmp < 0) || (sc.reverse && cmp > 0) {
				best = i
			}
		}
		if best < 0 {
			return nil, nil, false, nil
		}
		entry, value := sc.cursors[best].entry, sc.cursors[best].value
		for i, c := range sc.cursors {
			if c.valid && (i == best || bytes.Equal(c.entry, entry)) {
				sc.pending = append(sc.pending, i)
			}
		}
		if len(value) == 0 {
			sc.err = fmt.Errorf("HeimDB: empty value for entry %x", entry)
			return nil, nil, false, sc.err
		}
		if value[0] == valTombstone {
			continue
		}
		return entry, value[1:], true, nil
	}
}

// Reset は、同じ見え方のまま、走査の区間を [lo, hi)（hi が nil なら上限なし）に移す。
func (sc *Scan) Reset(lo, hi []byte) {
	if sc.closed || sc.err != nil {
		return
	}
	sc.pending = sc.pending[:0]
	for _, c := range sc.cursors {
		c.reset(lo, hi)
	}
}

// Close は走査を閉じる。2 回目からは何もしない。
func (sc *Scan) Close() error {
	if sc.closed {
		return nil
	}
	sc.closed = true
	var err error
	for _, c := range sc.cursors {
		if e := c.close(); e != nil && err == nil {
			err = e
		}
	}
	if sc.onClose != nil {
		sc.onClose(sc)
	}
	return err
}

// scanLayered は、層ごとのカーソルを重ねて、エントリの順（reverse なら逆順）に fn へ渡す（Scan を参照）。
func scanLayered(cursors []*versionCursor, reverse bool, fn func(entry, value []byte) (bool, error)) error {
	sc := &Scan{cursors: cursors, reverse: reverse}
	defer sc.Close()
	for {
		entry, value, ok, err := sc.Next()
		if err != nil || !ok {
			return err
		}
		if cont, err := fn(entry, value); err != nil || !cont {
			return err
		}
	}
}

// scanBranch は、ブランチ branch の層を重ねて、キー空間 prefixOf(層のブランチ) を走査する。
// 自分の層は ownIter（トランザクションのバッチを含むことがある）、親の層は確定済みのデータから読む。
// visible は各層に共通の見える条件（スナップショットや AS OF）で、親の層ではさらに分岐時刻以前に限る。
func (s *Store) scanBranch(branch uint64, ownIter func(*pebble.IterOptions) (*pebble.Iterator, error), prefixOf func(branch uint64) []byte, visible func(uint64) bool, lo, hi []byte, reverse bool, fn func(entry, value []byte) (bool, error)) error {
	cursors, err := s.branchCursors(branch, ownIter, prefixOf, visible, lo, hi, reverse)
	if err != nil {
		return err
	}
	return scanLayered(cursors, reverse, fn)
}

// openBranchScan は scanBranch と同じ走査を、1 件ずつ進める Scan として返す。
func (s *Store) openBranchScan(branch uint64, ownIter func(*pebble.IterOptions) (*pebble.Iterator, error), prefixOf func(branch uint64) []byte, visible func(uint64) bool, lo, hi []byte, reverse bool) (*Scan, error) {
	cursors, err := s.branchCursors(branch, ownIter, prefixOf, visible, lo, hi, reverse)
	if err != nil {
		return nil, err
	}
	return &Scan{cursors: cursors, reverse: reverse}, nil
}

// branchCursors は、ブランチ branch の層ごとのカーソルを、上の層から順に作る。
func (s *Store) branchCursors(branch uint64, ownIter func(*pebble.IterOptions) (*pebble.Iterator, error), prefixOf func(branch uint64) []byte, visible func(uint64) bool, lo, hi []byte, reverse bool) ([]*versionCursor, error) {
	ls, err := s.layers(branch)
	if err != nil {
		return nil, err
	}
	var cursors []*versionCursor
	for i, l := range ls {
		newIter := s.db.NewIter
		vis := visible
		if i == 0 {
			newIter = ownIter
		} else {
			maxTS := l.maxTS
			vis = func(ts uint64) bool { return ts <= maxTS && visible(ts) }
		}
		c, err := newVersionCursor(newIter, prefixOf(l.branch), vis, lo, hi, reverse)
		if err != nil {
			for _, c := range cursors {
				c.close()
			}
			return nil, err
		}
		cursors = append(cursors, c)
	}
	return cursors, nil
}
