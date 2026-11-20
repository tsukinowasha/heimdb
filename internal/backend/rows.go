package backend

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync/atomic"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// partition は Table の唯一のパーティション。lookup が nil なら全件、そうでなければインデックスによる検索。
type partition struct {
	lookup *sql.IndexLookup
}

func (partition) Key() []byte { return []byte("heimdb") }

// eachRow は、符号化した主キーが [lo, hi) にある行を、集めずに 1 行ずつ fn に渡す。
func (t *Table) eachRow(ctx *sql.Context, lo, hi []byte, reverse bool, want []bool, fn func(sql.Row) error) error {
	n := 0
	each := func(r store.RowVersion) (bool, error) {
		n++
		if err := interrupted(ctx, n); err != nil {
			return false, err
		}
		row, err := t.decodeRowColumns(r.Value, want)
		if err != nil {
			return false, err
		}
		return true, fn(row)
	}
	if t.asOf != store.Latest {
		return t.db.st.ScanRowsAsOf(t.db.branch, t.def.ID, t.asOf, lo, hi, reverse, each)
	}
	txn, done := txnFor(ctx, t.db.st)
	defer done()
	return txn.ScanRows(t.db.branch, t.def.ID, lo, hi, reverse, each)
}

func (t *Table) decodeRow(value []byte) (sql.Row, error) {
	return t.decodeRowColumns(value, nil)
}

// rowsDecoded は、復号した行の数（LIMIT で読むのをやめることのテスト用。docs/plan.md の段階 55）。
var rowsDecoded atomic.Int64

// decodeRowColumns は、行の値のうち want の列だけを復号する（want が nil ならすべて）。
func (t *Table) decodeRowColumns(value []byte, want []bool) (sql.Row, error) {
	return t.decodeRowColumnsInto(value, want, nil)
}

// decodeRowColumnsInto は decodeRowColumns と同じだが、dst の容量が足りれば行を dst の上に作る（dst が nil なら
// 新しく作る）。返した行は、次に同じ dst で復号するまでしか使えない（docs/plan.md の段階 125。rowReuser を参照）。
func (t *Table) decodeRowColumnsInto(value []byte, want []bool, dst sql.Row) (sql.Row, error) {
	rowsDecoded.Add(1)
	var row sql.Row
	var err error
	if dst == nil {
		_, row, err = rowenc.DecodeRowColumns(value, want)
	} else {
		_, row, err = rowenc.DecodeRowColumnsInto(value, want, dst)
	}
	if err != nil {
		return nil, err
	}
	// 列を変える ALTER TABLE にはまだ対応していないので、どのバージョンで書いた行も今のスキーマと同じ列数になる。
	if len(row) != len(t.sch.Schema) {
		return nil, fmt.Errorf("HeimDB: table %s: row has %d columns, schema has %d (column changes are not supported yet)", t.def.Name, len(row), len(t.sch.Schema))
	}
	t.scaleDecimals(row)
	return row, nil
}

// scaleDecimals は、行の DECIMAL の列の値を列の小数の桁で表す（10.5 を 10.50 に。値は変わらない）。
func (t *Table) scaleDecimals(row sql.Row) {
	for _, dc := range t.decimals {
		if dc.ord < len(row) {
			if d, ok := row[dc.ord].(decimal.Decimal); ok && d.Exponent() > -dc.scale {
				row[dc.ord] = d.Round(dc.scale)
			}
		}
	}
}

// lookupRows は、インデックスによる検索の結果を、そのインデックスの順（IsReverse なら逆順）に返す。
//
// 主キーとキーを持つセカンダリインデックスは、範囲をキーの区間に変換して走査する。キーにできない型を含む
// インデックスは、全件を走査して範囲で絞り、インデックスの列と主キーの順に並べ替えることで模擬する。
// どれも区間に変換しきれない条件があるため、最後に範囲の条件で行を絞る。
//
// セカンダリインデックスでは、エントリ（主キー）を先に読み、行は返すときに 1 行ずつ引く。ORDER BY ... LIMIT を
// インデックスの順で読む文（記事の一覧、前後の記事、最近のコメント）は、LIMIT の行数を返したところで止まる。
func (t *Table) lookupRows(ctx *sql.Context, lookup sql.IndexLookup) (sql.RowIter, error) {
	idx := lookup.Index.(*Index)
	ranges, ok := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok {
		return nil, fmt.Errorf("HeimDB: unsupported index lookup ranges %T", lookup.Ranges)
	}
	// どの行にも当たらない範囲（lookup JOIN の、列の型に入らないキー。整数の列の '0.01' や DECIMAL の列の '1e30'）は
	// 読まない。区間にすると端がないので、表かインデックスの全体を読んで範囲の条件で捨てていた（左の行ごとに全体を読む
	// ので、行数の 2 乗の時間がかかった。段階 115 の 2 度目のレビュー）。
	if ranges = nonEmptyRanges(ranges); len(ranges) == 0 {
		return &rowIter{}, nil
	}
	if idx.ID() == "PRIMARY" && t.asOf == store.Latest {
		if pk, ok := t.pointKey(ctx, ranges); ok {
			return t.getPoint(ctx, pk)
		}
	}
	// 範囲の条件と並べ替えに使う、インデックスの列に主キーを足した式。要るときだけ作る（lookup JOIN は左の行ごとに
	// lookupRows を呼ぶ。docs/plan.md の段階 102）。
	var exprs []sql.Expression
	extended := func() []sql.Expression {
		if exprs == nil {
			exprs = t.extendedExprs(idx)
		}
		return exprs
	}
	// 範囲の条件は、区間が範囲をちょうど表さないときだけ作る（lookup JOIN は左の行ごとに作る。docs/plan.md の段階 96）。
	newFilter := func() (sql.Expression, error) { return rangeFilter(extended(), ranges) }

	var rows []sql.Row
	var err error
	switch ki, keyed := t.findKeyIndex(idx.ID()); {
	case idx.ID() == "PRIMARY":
		cols := make([]*sql.Column, len(t.def.PKOrdinals))
		for i, ord := range t.def.PKOrdinals {
			cols[i] = t.sch.Schema[ord]
		}
		intervals := mergeIntervals(ranges, func(r sql.MySQLRange) (lo, hi []byte) {
			return indexInterval(ctx, cols, func(i int, v any) ([]byte, bool, error) {
				b, err := rowenc.EncodePK(nil, sql.NewPrimaryKeySchema(sql.Schema{cols[i]}, 0), sql.Row{v})
				return b, false, err
			}, r)
		})
		// 区間は重ならず lo の順で、主キーの順は値の順なので、読む順がそのまま並びになる（集めずに 1 行ずつ返す。
		// docs/plan.md の段階 55）。
		if len(intervals) == 0 {
			return &rowIter{}, nil
		}
		// 区間が範囲をちょうど表すなら、行ごとに範囲の条件を評価しない（JOIN で左の行ごとに主キーの範囲が 100 個並ぶ
		// 記事の term など。docs/plan.md の段階 78）。
		var filter sql.Expression
		if !exactIntervals(ctx, false, cols, ranges) {
			if filter, err = newFilter(); err != nil {
				return nil, err
			}
		}
		return t.newScanRowIter(ctx, intervals, lookup.IsReverse, t.readColumns(idx), filter)
	case keyed:
		cols := make([]*sql.Column, len(ki.ords))
		for i, ord := range ki.ords {
			cols[i] = t.sch.Schema[ord]
		}
		var intervals []interval
		var exact, sorted bool
		if pl, ok := pointLookup(ctx, ki, cols, ranges); ok {
			// lookup JOIN が引く形（段階 102）
			pointLookups.Add(1)
			intervals = []interval{pl.iv}
			exact = pl.exact && (!ki.hasPrefix || pl.shortRanged)
			sorted = !ki.hasPrefix || pl.shortAll
		} else {
			intervals = mergeIntervals(ranges, func(r sql.MySQLRange) (lo, hi []byte) {
				return indexInterval(ctx, cols, func(i int, v any) ([]byte, bool, error) {
					k, truncated, err := rowenc.AppendIndexValue(nil, ki.cols[i], v)
					// FLOAT の列の端は、単精度に丸めた値なので、含む側に広げる（段階 117。範囲の条件で絞り直す）
					return k, truncated || ki.cols[i].Type.Type() == sqltypes.Float32, err
				}, r)
			})
			// プレフィックス長のある列は、範囲の付いた列がどれも短い点なら、ない列と同じく区間で判断できる（段階 96）。
			exact = exactIntervals(ctx, ki.hasPrefix && !shortPrefixPoints(ctx, ki, ranges, true), cols, ranges)
			sorted = !ki.hasPrefix || shortPrefixPoints(ctx, ki, ranges, false)
		}
		var filter sql.Expression
		if !exact {
			if filter, err = newFilter(); err != nil {
				return nil, err
			}
		}
		want := t.readColumns(idx)
		if filter == nil && !ki.hasPrefix {
			want = t.readColumns(nil) // インデックスの列は、範囲の条件にも並べ替えにも使わない
		}
		// エントリは集めずに、少しずつ読む（docs/plan.md の段階 55）。
		it, err := t.newPKRowIter(ctx, ki, intervals, lookup.IsReverse, filter, want)
		if err != nil {
			return nil, err
		}
		if sorted {
			return it, nil
		}
		// プレフィックス長のある列を含むと、キーの順が値の順と一致しないので、読み込んで並べ替える。
		prefixSorts.Add(1)
		if rows, err = sql.RowIterToRows(ctx, it); err != nil {
			return nil, err
		}
		if rows, err = sortRows(ctx, rows, extended(), lookup.IsReverse); err != nil {
			return nil, err
		}
		return &rowIter{rows: rows}, nil
	default:
		// キーを持たないインデックス（FULLTEXT）: 全行を読み、範囲の条件に当たる行だけを集めて並べ替える。
		filter, err := newFilter()
		if err != nil {
			return nil, err
		}
		it, err := t.newScanRowIter(ctx, nil, false, t.readColumns(idx), filter)
		if err != nil {
			return nil, err
		}
		if rows, err = sql.RowIterToRows(ctx, it); err != nil {
			return nil, err
		}
		if rows, err = sortRows(ctx, rows, extended(), lookup.IsReverse); err != nil {
			return nil, err
		}
		return t.loadedRows(ctx, rows)
	}
}

// readColumns は、インデックス idx で読む行のうち復号する列を返す。文が使う列（WithProjections）と、範囲の条件と
// 並べ替えに使う列（インデックスの列と主キー）。idx が nil なら文が使う列と主キー（返す値は書き換えない）。
// 列を絞っていない文と、ALTER の書き直しの途中は nil（すべて）。
func (t *Table) readColumns(idx *Index) []bool {
	if t.proj == nil || t.nullFill != nil {
		return nil
	}
	if idx == nil {
		return t.proj.want
	}
	want := slices.Clone(t.proj.want)
	for _, c := range idx.columns {
		if ord := t.def.ColumnIndex(c.Name); ord >= 0 {
			want[ord] = true
		}
	}
	return want
}

// covers は、文が使う列（WithProjections）がすべてインデックス ki のエントリの値にあり、本体の行を引かずに
// エントリだけで読めるか（docs/plan.md の段階 27）。列を絞っていない文と、ALTER の書き直しの途中は読めない。
func (t *Table) covers(ki keyIndex) bool {
	if t.proj == nil || t.nullFill != nil {
		return false
	}
	for _, ord := range t.proj.ords {
		if !slices.Contains(ki.cover, ord) {
			return false
		}
	}
	return true
}

// coveredRow は、インデックスのエントリの値から行を作る。行は全体のスキーマの幅で、ki.cover の列のうち want の列
// （nil ならすべて）だけに値が入る（ほかは NULL。文は使わない。want は範囲の条件が残るならインデックスの列を含む）。
func (t *Table) coveredRow(ki keyIndex, v []byte, want []bool) (sql.Row, error) {
	return t.coveredRowInto(ki, v, want, nil)
}

// coveredRowInto は coveredRow と同じだが、dst の容量が足りれば行を dst の上に作る（dst が nil なら新しく作る。
// 返した行は、次に同じ dst で作るまでしか使えない。docs/plan.md の段階 125）。
func (t *Table) coveredRowInto(ki keyIndex, v []byte, want []bool, dst sql.Row) (sql.Row, error) {
	var row sql.Row
	if n := len(t.sch.Schema); cap(dst) >= n {
		row = dst[:n]
		clear(row)
	} else {
		row = make(sql.Row, n)
	}
	if err := rowenc.DecodeValuesTo(v, row, ki.cover, want); err != nil {
		return nil, fmt.Errorf("HeimDB: index %s of %s has a corrupt entry (want %d values): %w", ki.def.Name, t.def.Name, len(ki.cover), err)
	}
	t.scaleDecimals(row)
	return row, nil
}

// pointKey は、ranges が主キーのすべての列の 1 つの値（列の型にそのまま入る値）だけを表すなら、その主キーの
// 符号化を返す。JOIN で左の行ごとに主キーで 1 行を引く（wp_term_relationships の (object_id, term_taxonomy_id)、
// メタの JOIN の wp_posts の ID）とき、区間に変換して走査せずに引く（docs/plan.md の段階 27）。
func (t *Table) pointKey(ctx *sql.Context, ranges sql.MySQLRangeCollection) ([]byte, bool) {
	if len(ranges) != 1 || len(ranges[0]) != len(t.def.PKOrdinals) {
		return nil, false
	}
	var pk []byte
	for i, ord := range t.def.PKOrdinals {
		ce := ranges[0][i]
		typ := t.sch.Schema[ord].Type
		if _, ok := ce.LowerBound.(sql.Below); !ok {
			return nil, false
		}
		if _, ok := ce.UpperBound.(sql.Above); !ok {
			return nil, false
		}
		lo, hi := sql.GetMySQLRangeCutKey(ce.LowerBound), sql.GetMySQLRangeCutKey(ce.UpperBound)
		if !exactKey(ctx, typ, lo) || !exactKey(ctx, typ, hi) {
			return nil, false
		}
		if c, err := typ.Compare(ctx, lo, hi); err != nil || c != 0 {
			return nil, false
		}
		v, ok := convertKey(ctx, typ, lo)
		if !ok {
			return nil, false
		}
		var err error
		if pk, err = rowenc.AppendPKValue(pk, typ, v); err != nil {
			return nil, false
		}
	}
	return pk, true
}

// getPoint は、主キー pk の行（なければ 0 行）を返す。トランザクションの中で使い回す RowReader で引く。
func (t *Table) getPoint(ctx *sql.Context, pk []byte) (sql.RowIter, error) {
	txn, done := txnFor(ctx, t.db.st)
	defer done()
	rd, err := txn.PointReader(t.db.branch, t.def.ID)
	if err != nil {
		return nil, err
	}
	v, ok, err := rd.Peek(pk) // 復号で値を写すので、写さずに読む
	if err != nil {
		return nil, err
	}
	if r := statementReads(ctx); r != nil {
		r.noteIndex(t.def.Name, "PRIMARY")
		if ok {
			r.rows.Add(1)
		}
	}
	if !ok {
		return &rowIter{}, nil
	}
	row, err := t.decodeRowColumns(v, t.readColumns(nil))
	if err != nil {
		return nil, err
	}
	return t.loadedRows(ctx, []sql.Row{row})
}

// prefixSorts は、プレフィックス長のあるインデックスで読んだ行を並べ替えた回数（テスト用）。
var prefixSorts atomic.Int64

// shortPrefixPoints は、インデックス ki のプレフィックス長のある列が、どの範囲でもプレフィックス長より短い文字列の 1 つの
// 値（点）か（段階 83）。そうなら、キーにその値がそのまま入るのでキーの順が値の順と一致し、範囲の条件で絞った行は
// 読んだ順のまま返せる（meta_key = 'category' の 2 万行を、毎回集めて並べ替えていた）。
//
// ranged なら、範囲が付いている列だけを見る（docs/plan.md の段階 96）。範囲がその列の手前で終わるか、すべての値の列
// （lookup JOIN が埋める）は区間に効かないので問わない。キーは文字ごとの重みを並べて 0x00 で終えるので、短い点のキーは、
// プレフィックス長で切った長い値のキー（重みがプレフィックス長の数だけ並ぶ）と一致しない。そうなら、区間が範囲をちょうど
// 表すかは、プレフィックス長のない列と同じに判断できる（並べ替えは、範囲の付かない後ろの列があると要る）。エンジンは
// PAD SPACE を持たないので、末尾の空白も別の値になる（キーと同じ）。
func shortPrefixPoints(ctx *sql.Context, ki keyIndex, ranges sql.MySQLRangeCollection, ranged bool) bool {
	for _, r := range ranges {
		for i, c := range ki.cols {
			if c.Prefix == 0 {
				continue
			}
			if ranged {
				if i >= len(r) {
					continue
				}
				_, lowerAll := r[i].LowerBound.(sql.BelowNull)
				_, upperAll := r[i].UpperBound.(sql.AboveAll)
				if lowerAll && upperAll {
					continue
				}
			}
			if i >= len(r) || !isPoint(ctx, r[i]) {
				return false
			}
			s, ok := sql.GetMySQLRangeCutKey(r[i].LowerBound).(string)
			if !ok || int64(utf8.RuneCountInString(s)) >= c.Prefix {
				return false
			}
		}
	}
	return true
}

// pointLookups は、lookupRows が pointLookup で区間を作った数（テスト用）。
var pointLookups atomic.Int64

// pointResult は pointLookup の結果。
type pointResult struct {
	iv interval
	// exact は、点の値がどれも列の型にそのまま入るか（exactIntervals。プレフィックス長の列のことは含まない）。
	exact bool
	// shortRanged と shortAll は、shortPrefixPoints の ranged が true と false の結果。
	shortRanged, shortAll bool
}

// pointLookup は、lookup JOIN が左の行ごとに引く形の範囲（範囲が 1 つで、先頭から点の列が 1 つ以上続き、残りの列が
// すべての値）なら、点の値を列ごとに 1 回だけ変換して符号化し、区間（mergeIntervals と indexInterval）、ちょうどか
// （exactIntervals）、短い点か（shortPrefixPoints）を返す（docs/plan.md の段階 102）。今までの経路は、同じ値を下限と
// 上限で別々に、区間、判定、短い点の確かめごとに変換し直していた（two_filters_t の CPU の 15% ほど）。点は、下限と
// 上限が同じ Go の値のものだけ見る（go-mysql-server の lookup は両端に同じ値を置く。照合順序で同じ重みの違う文字列など、
// 比べないと分からないものは今までの経路）。変換できない値と、ほかの形も今までの経路で、ok が false。
func pointLookup(ctx *sql.Context, ki keyIndex, cols []*sql.Column, ranges sql.MySQLRangeCollection) (pointResult, bool) {
	if len(ranges) != 1 || len(ranges[0]) != len(cols) || len(cols) != len(ki.cols) {
		return pointResult{}, false
	}
	r := ranges[0]
	res := pointResult{exact: true, shortRanged: true, shortAll: true}
	var prefix []byte
	points := 0
	for i, ce := range r {
		lower, lok := ce.LowerBound.(sql.Below)
		upper, uok := ce.UpperBound.(sql.Above)
		if !lok || !uok {
			_, lowerAll := ce.LowerBound.(sql.BelowNull)
			_, upperAll := ce.UpperBound.(sql.AboveAll)
			if !lowerAll || !upperAll || i == 0 {
				return pointResult{}, false
			}
			if ki.cols[i].Prefix > 0 {
				res.shortAll = false // shortPrefixPoints は ranged でなければ、すべての値の列も点を求める
			}
			continue
		}
		if i != points || lower.Key == nil || !sameKey(lower.Key, upper.Key) {
			return pointResult{}, false // すべての値の列の後の点と、両端が違う値の点
		}
		points++
		v, ok := convertKey(ctx, cols[i].Type, lower.Key)
		if !ok {
			return pointResult{}, false
		}
		var k []byte
		var err error
		if k, _, err = rowenc.AppendIndexValue(nil, ki.cols[i], v); err != nil {
			return pointResult{}, false
		}
		prefix = append(prefix, k...)
		res.exact = res.exact && exactKeyKind(cols[i].Type, lower.Key)
		if p := ki.cols[i].Prefix; p > 0 {
			s, ok := lower.Key.(string)
			if !ok || int64(utf8.RuneCountInString(s)) >= p {
				res.shortRanged, res.shortAll = false, false
			}
		}
	}
	res.iv = interval{lo: prefix, hi: keyenc.PrefixEnd(prefix)}
	return res, true
}

// sameKey は、範囲の端の値 a と b が同じ Go の値か（比べられない型と、型が違うものは false）。
func sameKey(a, b any) bool {
	switch a := a.(type) {
	case string:
		b, ok := b.(string)
		return ok && a == b
	case []byte:
		b, ok := b.([]byte)
		return ok && bytes.Equal(a, b)
	case int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint:
		return a == b
	case float64:
		b, ok := b.(float64)
		return ok && a == b
	case float32:
		b, ok := b.(float32)
		return ok && a == b
	case decimal.Decimal:
		b, ok := b.(decimal.Decimal)
		return ok && a.Equal(b)
	}
	return false
}

// rangeFilterEvals は、pkRowIter が範囲の条件を評価した行の数（テスト用）。
var rangeFilterEvals atomic.Int64

// readersOpened は、pkRowIter が新しく開いた行の読み手の数（トランザクションの PointReader を使ったときは数えない。テスト用）。
var readersOpened atomic.Int64

// pkRowsRead は、pkRowIter が引いた行の数（LIMIT で読むのをやめることのテスト用）。
var pkRowsRead atomic.Int64

// pkRowIter は、セカンダリインデックスのエントリを少しずつ読み、主キーの行を返すときに 1 行ずつ引く sql.RowIter。
// 文が使う列がすべてエントリの値にあれば（covered）、本体の行を引かずにエントリの値から行を作る。
type pkRowIter struct {
	t       *Table
	ki      keyIndex
	scan    intervalScan
	covered bool
	pks     [][]byte       // 読んだエントリの主キー（まだ返していないもの）
	vals    [][]byte       // pks と同じ並びのエントリの値（covered のときだけ）
	rows    []sql.Row      // 先に読んで復号した行（pks と同じ並び。prefetch を参照）
	filter  sql.Expression // 範囲の条件（nil なら絞らない）
	want    []bool         // 復号する列（nil ならすべて）
	rd      *store.RowReader
	shared  bool        // rd はトランザクションの PointReader（閉じない）
	closeRd *stmtCloser // rd と、それを読むトランザクションの後始末
	entries int         // 読んだエントリの数
	chunk   int         // 次の fill で読むエントリの数（firstFill から倍にしていく）
	read    int         // 引いた行の数
	n       int         // 返した行の数（ALTER の途中の警告の行番号）
	// buf は、fill が主キーとエントリの値を写すバッファ（keep）。fill ごとに新しく作る。
	buf []byte
	// scratch は、エントリの値から作る行を使い回す置き場所（reuse のときだけ。rowReuser を参照）。
	scratch sql.Row
	reuse   bool
}

const (
	// prefetchAfter は、1 行ずつ引くのをやめて、まとめて先読みに切り替えるまでに引く行の数。LIMIT のある文は、
	// たいていこれより前に読むのをやめる。
	prefetchAfter = 256
	// prefetchChunk は、まとめて先読みする行の数。エントリも最大でこの数ずつ読む。
	prefetchChunk = 4096
	// firstFill は、最初に読むエントリの数。次からは倍にしていき、prefetchChunk で止める。LIMIT の小さい文が、
	// 返さないエントリまで読まないようにする（段階 78 までは最初から 4096 件読み、最近の記事 5 件で約 2 ms かかった）。
	firstFill = 32
)

// newPKRowIter は、インデックス ki のエントリの区間 ivs（lo の順）の行を返す sql.RowIter を作る。エントリの走査と、
// 行を引く RowReader は、ここで開く（前はここでエントリをすべて読み、同じ時点で RowReader を開いていた。見え方は同じ）。
func (t *Table) newPKRowIter(ctx *sql.Context, ki keyIndex, ivs []interval, reverse bool, filter sql.Expression, want []bool) (sql.RowIter, error) {
	h, ivs, err := t.openIndexScan(ctx, ki, ivs, reverse)
	if err != nil {
		return nil, err
	}
	it := &pkRowIter{t: t, ki: ki, scan: intervalScan{h: h, ivs: ivs}, covered: t.covers(ki), filter: filter, want: want}
	if err := it.fill(ctx); err != nil {
		it.scan.close()
		return nil, err
	}
	if len(it.pks) == 0 {
		it.scan.close()
		return &rowIter{}, nil
	}
	if !it.covered {
		if err := it.openReader(ctx); err != nil {
			it.scan.close()
			return nil, err
		}
	}
	return it, nil
}

// fill は、次のエントリを最大 chunk 件読んで pks（と vals）に置く。走査はエントリを写さずに返すので、主キー（と
// covered ならエントリの値）を 1 回の fill ごとに 1 つのバッファに写す（エントリごとに写していた。docs/plan.md の
// 段階 103）。走査は読み終えると使い回しの置き場所に戻り、同じ文のほかの検索が読み直すので、読んだその場で写す。
// バッファは fill ごとに新しく作る（前の fill の pks を、返した行がまだ持っていることがある）。
func (it *pkRowIter) fill(ctx *sql.Context) error {
	it.pks, it.vals = it.pks[:0], it.vals[:0]
	it.chunk = min(max(it.chunk*2, firstFill), prefetchChunk)
	it.buf = nil
	for len(it.pks) < it.chunk {
		entry, value, ok, err := it.scan.next()
		if err != nil {
			return err
		}
		if !ok {
			it.scan.close()
			return nil
		}
		it.entries++
		if err := interrupted(ctx, it.entries); err != nil {
			return err
		}
		pk, err := it.t.pkOfEntry(it.ki, entry)
		if err != nil {
			return err
		}
		if !it.covered {
			value = nil
		}
		pk, value = it.keep(pk, value)
		it.pks = append(it.pks, pk)
		if it.covered {
			it.vals = append(it.vals, value)
		}
	}
	return nil
}

// keepBlock は、keep が 1 度に作るバッファの大きさの上限（covered のエントリの値は索引の列の元の値をすべて持つので、
// 長い文字列の列で残りのエントリの数を掛けると大きくなりすぎる）。
const keepBlock = 64 << 10

// keep は、pk と value を fill のバッファに写して返す。足りなければ、この fill で残りのエントリが同じ大きさとして
// 新しいバッファを作る（keepBlock まで。前のバッファは写さない。写したものはそのまま使える）。
func (it *pkRowIter) keep(pk, value []byte) ([]byte, []byte) {
	n := len(pk) + len(value)
	if cap(it.buf)-len(it.buf) < n {
		it.buf = make([]byte, 0, max(n, min(n*(it.chunk-len(it.pks)), keepBlock)))
	}
	start := len(it.buf)
	it.buf = append(append(it.buf, pk...), value...)
	mid := start + len(pk)
	return it.buf[start:mid:mid], it.buf[mid:len(it.buf):len(it.buf)]
}

// openReader は、主キーで行を引く RowReader を開く。
func (it *pkRowIter) openReader(ctx *sql.Context) error {
	t := it.t
	// 書かない文では、主キーの点で引くのと同じトランザクションの PointReader を使う（lookup JOIN は左の行ごとに
	// 右のインデックスを引くので、引くたびに層の数だけ Pebble のイテレータを作っていた。two_filters_t の CPU の
	// 5%。docs/plan.md の段階 99）。PointReader はトランザクションが書くと閉じるので、書く文では使わない。
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil && s.cmdScans != nil && t.asOf == store.Latest {
		rd, err := s.txn.PointReader(t.db.branch, t.def.ID)
		if err != nil {
			return err
		}
		it.rd, it.shared = rd, true
		return nil
	}
	readersOpened.Add(1)
	release := func() {}
	var rd *store.RowReader
	var err error
	if t.asOf != store.Latest {
		rd, err = t.db.st.RowReaderAsOf(t.db.branch, t.def.ID, t.asOf)
	} else {
		var txn *store.Txn
		txn, release = txnFor(ctx, t.db.st)
		if rd, err = txn.RowReader(t.db.branch, t.def.ID); err != nil {
			release()
		}
	}
	if err != nil {
		return err
	}
	it.rd = rd
	it.closeRd = onStatementEnd(ctx, func() {
		_ = rd.Close()
		it.rd = nil
		release()
	})
	return nil
}

func (it *pkRowIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		if len(it.pks) == 0 {
			if it.scan.h == nil || it.scan.h.sc == nil {
				return nil, io.EOF
			}
			if err := it.fill(ctx); err != nil {
				return nil, err
			}
			if len(it.pks) == 0 {
				return nil, io.EOF
			}
		}
		var row sql.Row
		if it.covered && len(it.vals[0]) > 0 {
			var err error
			if it.reuse {
				if row, err = it.t.coveredRowInto(it.ki, it.vals[0], it.want, it.scratch); err != nil {
					return nil, err
				}
				it.scratch = row
			} else if row, err = it.t.coveredRow(it.ki, it.vals[0], it.want); err != nil {
				return nil, err
			}
			it.pks, it.vals = it.pks[1:], it.vals[1:]
		} else {
			// 段階 27 より前に書いたエントリは値を持たないので、本体の行を引く
			if it.rd == nil {
				if err := it.openReader(ctx); err != nil {
					return nil, err
				}
			}
			if len(it.rows) == 0 && it.read >= prefetchAfter && len(it.pks) > 1 && !it.covered {
				if err := it.prefetch(); err != nil {
					return nil, err
				}
			}
			pk := it.pks[0]
			it.pks = it.pks[1:]
			if it.covered {
				it.vals = it.vals[1:]
			}
			it.read++
			if err := interrupted(ctx, it.read); err != nil {
				return nil, err
			}
			pkRowsRead.Add(1)
			if !it.covered && it.scan.h != nil && it.scan.h.reads != nil {
				it.scan.h.reads.rows.Add(1) // エントリから引いた本体の行（エントリは走査が数える）
			}
			if len(it.rows) > 0 {
				row, it.rows = it.rows[0], it.rows[1:]
			} else {
				var err error
				if row, err = it.get(pk); err != nil {
					return nil, err
				}
			}
		}
		if it.filter != nil {
			rangeFilterEvals.Add(1)
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

// CountRest は sql.RowCounter の実装。SQL_CALC_FOUND_ROWS の LIMIT が返さない行を、本体の行を引かずにエントリの数で
// 数える（記事の一覧で、残りの約 1 万行の本体を引いて全列を復号していた。docs/plan.md の段階 97）。エントリの走査は
// ブランチの重ね合わせ、削除の印、読む時点をもう済ませているので、数は Next が返す行と同じ。範囲の条件で絞るときと、
// ALTER の途中で NULL を埋めるとき（警告とエラーを出す）は数えない（範囲の条件は、区間が範囲をちょうど表さないときに
// 付く。そのときエンジンは Filter を残すか、長さを指定した列の長い点なら並べ替えのために集めるので、今は LIMIT の直下に
// 来ない）。数えるときは本体の行があるかを確かめない。
func (it *pkRowIter) CountRest(ctx *sql.Context) (int64, bool, error) {
	if it.filter != nil || it.t.nullFill != nil {
		return 0, false, nil
	}
	n := int64(len(it.pks)) // 先読みした rows は pks と同じ並びなので、足さない
	it.pks, it.vals, it.rows = nil, nil, nil
	for {
		_, _, ok, err := it.scan.next()
		if err != nil {
			return n, true, err
		}
		if !ok {
			break
		}
		it.entries++
		if err := interrupted(ctx, it.entries); err != nil {
			return n, true, err
		}
		n++
	}
	it.scan.close() // 走査は読み終えたので、この後の Next は io.EOF
	return n, true, nil
}

// prefetch は、次の prefetchChunk 行を主キーの順に引いて rows に置く。インデックスの順の主キーは表の中で飛び飛び
// なので、1 行ずつ引くとイテレータが前後に飛ぶ。主キーの順に引くと前に進むだけになる（ほとんどの行が当てはまる
// 条件でインデックスを使う文。承認済みのコメント 2 万行の数で約 2 割速くなった）。
func (it *pkRowIter) prefetch() error {
	n := min(prefetchChunk, len(it.pks))
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return bytes.Compare(it.pks[order[a]], it.pks[order[b]]) < 0 })
	it.rows = make([]sql.Row, n)
	for _, i := range order {
		row, err := it.get(it.pks[i])
		if err != nil {
			return err
		}
		it.rows[i] = row
	}
	return nil
}

// get は、主キー pk の行を引いて復号する。
func (it *pkRowIter) get(pk []byte) (sql.Row, error) {
	// 書かない文の中では閉じないが、接続が切れたときのロールバックは別のゴルーチンから閉じうる。閉じた RowReader は
	// 行がないと答えるので、行が消えたと誤らずにエラーにする。
	if it.shared && it.rd.Closed() {
		return nil, fmt.Errorf("HeimDB: the row reader of %s was closed by a write in the same statement", it.t.def.Name)
	}
	v, ok, err := it.rd.Peek(pk) // 復号で値を写すので、写さずに読む
	if err != nil {
		return nil, err
	} else if !ok {
		return nil, it.missing()
	}
	return it.t.decodeRowColumns(v, it.want)
}

func (it *pkRowIter) missing() error {
	return fmt.Errorf("HeimDB: index %s of %s points to a missing row", it.ki.def.Name, it.t.def.Name)
}

func (it *pkRowIter) Close(*sql.Context) error {
	it.scan.close()
	it.closeRd.close()
	return nil
}

// interval は、キーの区間 [lo, hi)。hi が nil なら上限なし。
type interval struct {
	lo, hi []byte
}

// mergeIntervals は、範囲をキーの区間に変換し、重なる区間とつながる区間をまとめて、lo の順に返す。
//
// indexInterval は、等号で固定されていない列より後ろの条件を区間に入れずに広げるので、別々の範囲が同じ区間に
// なることがある（インデックス (post_type, post_status, ...) で post_status IN ('publish', 'future') だけなら、
// どちらの範囲もインデックス全体）。まとめずに走査すると、同じ行を範囲の数だけ返していた。
func mergeIntervals(ranges sql.MySQLRangeCollection, toInterval func(sql.MySQLRange) (lo, hi []byte)) []interval {
	if len(ranges) == 1 {
		lo, hi := toInterval(ranges[0])
		return []interval{{lo: lo, hi: hi}}
	}
	ivs := make([]interval, len(ranges))
	for i, r := range ranges {
		ivs[i].lo, ivs[i].hi = toInterval(r)
	}
	sort.Slice(ivs, func(i, j int) bool { return bytes.Compare(ivs[i].lo, ivs[j].lo) < 0 })
	var out []interval
	for _, iv := range ivs {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.hi == nil || bytes.Compare(iv.lo, last.hi) <= 0 {
				if last.hi != nil && (iv.hi == nil || bytes.Compare(iv.hi, last.hi) > 0) {
					last.hi = iv.hi
				}
				continue
			}
		}
		out = append(out, iv)
	}
	return out
}

// nonEmptyRanges は、どの値も入らない列のある範囲（下の端が sql.AboveAll。GetLookup の EmptyRangeColumnExpr）を除いた
// 範囲を返す。除くものがなければ ranges をそのまま返す（lookup JOIN は左の行ごとに呼ぶので、作り直さない）。
func nonEmptyRanges(ranges sql.MySQLRangeCollection) sql.MySQLRangeCollection {
	empty := func(r sql.MySQLRange) bool {
		for _, ce := range r {
			if _, ok := ce.LowerBound.(sql.AboveAll); ok {
				return true
			}
		}
		return false
	}
	for i, r := range ranges {
		if !empty(r) {
			continue
		}
		out := append(sql.MySQLRangeCollection(nil), ranges[:i]...)
		for _, r := range ranges[i+1:] {
			if !empty(r) {
				out = append(out, r)
			}
		}
		return out
	}
	return ranges
}

// extendedExprs は、インデックスの列に、含まれていない主キーの列を足した式を返す（InnoDB のセカンダリインデックスと同じ並び）。
func (t *Table) extendedExprs(idx *Index) []sql.Expression {
	var exprs []sql.Expression
	seen := map[int]bool{}
	add := func(ord int) {
		if seen[ord] {
			return
		}
		seen[ord] = true
		c := t.sch.Schema[ord]
		exprs = append(exprs, expression.NewGetFieldWithTable(ord, 0, c.Type, t.db.name, t.def.Name, c.Name, c.Nullable))
	}
	for _, c := range idx.columns {
		add(t.def.ColumnIndex(c.Name))
	}
	for _, ord := range t.def.PKOrdinals {
		add(ord)
	}
	return exprs
}

// rangeFilter は、ranges のいずれかに入る行で真になる式を返す。範囲は exprs の先頭から順に列へ当てはめる。
// 範囲がなければ偽の式、絞る必要がなければ nil。
func rangeFilter(exprs []sql.Expression, ranges sql.MySQLRangeCollection) (sql.Expression, error) {
	if len(ranges) == 0 {
		return expression.NewLiteral(false, types.Boolean), nil
	}
	return expression.NewRangeFilterExpr(exprs, ranges)
}

// rowIter は、あらかじめ読み込んだ行を返す sql.RowIter。
type rowIter struct {
	rows []sql.Row
	i    int
}

func (it *rowIter) Next(*sql.Context) (sql.Row, error) {
	if it.i >= len(it.rows) {
		return nil, io.EOF
	}
	it.i++
	return it.rows[it.i-1], nil
}

func (it *rowIter) Close(*sql.Context) error { return nil }

// sortRows は、行を exprs の昇順（reverse なら降順）に安定ソートする。
func sortRows(ctx *sql.Context, rows []sql.Row, exprs []sql.Expression, reverse bool) ([]sql.Row, error) {
	order := sql.Ascending
	if reverse {
		order = sql.Descending
	}
	fields := make(sql.SortFields, len(exprs))
	for i, e := range exprs {
		fields[i] = sql.SortField{Column: e, Column2: e.(sql.Expression2), Order: order}
	}
	sorter := &expression.Sorter{SortFields: fields, Rows: rows, Ctx: ctx}
	sort.Stable(sorter)
	return rows, sorter.LastError
}
