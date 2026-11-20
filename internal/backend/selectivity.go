package backend

import (
	"math"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/memo"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
)

func init() {
	memo.FilterSelectivity = filterSelectivity
	memo.JoinKeyDistinct = joinKeyDistinct
}

// joinKeyDistinct は、内部 JOIN のキー（両側の列の等号）の列の値の種類の数を、表の統計（その列から始まるインデックスの
// 先頭の列の値の種類の数）で見積もる（docs/plan.md の段階 101）。等号が複数あれば、いちばん多いもの。分からなければ 0。
//
// go-mysql-server は JOIN の出力の行数を「左の行数 × 右の行数 ÷ 値の種類の数」で見積もるが、Filter を通った子は値の
// 種類の数を持たないので、大きい方の子の行数の 8 割を値の種類とみなす。そうすると出力は小さい方の子の行数で決まり、
// 大きい方の子の条件で絞っても変わらない。meta_query の two_filters_t（category = 'cat3' の 2,000 行に、featured の
// meta_value = '1' で 1 割に絞るメタと、絞らない event_date のメタを記事ごとに引く）で、2 つの順が同点になり、
// event_date を先に引いていた（MySQL は featured を先に引いて 223 行に絞る）。キーの値の種類（記事の数）で割ると、
// featured との JOIN の方が小さくなる。
func joinKeyDistinct(ctx *sql.Context, rel memo.JoinRel) float64 {
	jp := rel.JoinPrivate()
	switch jp.Op {
	case plan.JoinTypeInner, plan.JoinTypeLookup, plan.JoinTypeHash, plan.JoinTypeMerge:
	default:
		return 0
	}
	if ctx == nil {
		ctx = sql.NewEmptyContext()
	}
	left, right := map[sql.TableId]plan.TableIdNode{}, map[sql.TableId]plan.TableIdNode{}
	for _, n := range jp.Left.RelProps.TableIdNodes() {
		left[n.Id()] = n
	}
	for _, n := range jp.Right.RelProps.TableIdNodes() {
		right[n.Id()] = n
	}
	var d float64
	for _, f := range jp.Filter {
		for _, c := range expression.SplitConjunction(f) {
			eq, ok := c.(*expression.Equals)
			if !ok {
				continue
			}
			a, ok1 := eq.Left().(*expression.GetField)
			b, ok2 := eq.Right().(*expression.GetField)
			if !ok1 || !ok2 {
				continue
			}
			if _, ok := left[a.TableId()]; !ok {
				a, b = b, a
			}
			la, ok1 := left[a.TableId()]
			rb, ok2 := right[b.TableId()]
			if !ok1 || !ok2 {
				continue
			}
			d = math.Max(d, math.Max(columnDistinct(ctx, la, a.Name()), columnDistinct(ctx, rb, b.Name())))
		}
	}
	return d
}

// columnDistinct は、表 n の列 col から始まるインデックスの統計の、値の種類の数（なければ 0）。
func columnDistinct(ctx *sql.Context, n plan.TableIdNode, col string) float64 {
	var table sql.Table
	switch n := n.(type) {
	case *plan.TableAlias:
		if tn, ok := n.Child.(sql.TableNode); ok {
			table = tn.UnderlyingTable()
		}
	case sql.TableNode:
		table = n.UnderlyingTable()
	}
	t := tableOf(table)
	if t == nil {
		return 0
	}
	statistics, err := rowCounts.stats(ctx, t)
	if err != nil {
		return 0
	}
	for _, st := range statistics {
		if cols := st.Columns(); len(cols) > 0 && strings.EqualFold(cols[0], col) {
			return float64(st.DistinctCount())
		}
	}
	return 0
}

// filterSelectivity は、Filter の条件で残る行の割合の見積もり（docs/plan.md の段階 83、84）。go-mysql-server は条件の種類や
// 数によらず 0.85 を 1 回掛けるだけなので、meta_query の JOIN の meta_value = 'cat3'（記事の 1 割）と、並べ替えに使うだけの
// meta_key = 'event_date'（全記事）が同じ行数になり、絞り込みの強い表から読み始めなかった（ヘッドレスの API の two_filters）。
//
// 子がインデックスの範囲で読み、範囲が先頭の列から点で決まり（m 列）、条件の等号がその次の列から続く（k 列まで）なら、
// 標本で数えた「先頭の k 列の組の行数 / 先頭の m 列の組の行数」を掛ける（段階 84。MySQL は定数の等号ではインデックスを
// 読んで数える。API 用の索引 key_value (meta_key, meta_value) で meta_value = 'cat3' を数えなかった）。元の 0.85 より
// 絞るときだけ使う。
//
// 残りの条件は、インデックスのない列（wp_postmeta.meta_value など）の条件ごとに、MySQL の既定の割合（condition filtering
// の COND_FILTER_EQUALITY 0.1、不等号 1/3）を掛ける。範囲が点で決める列だけを見る条件は、範囲で使ったものとして数えない
// （プレフィックス長のあるインデックスでは条件が Filter にも残る）。どれかのインデックスの先頭の列を見る条件も数えない。
// 数える条件がなければ元の 0.85。
func filterSelectivity(rel *memo.Filter) float64 {
	sel, _ := filterParts(nil, rel)
	return sel
}

// filterParts は、filterSelectivity の割合と、そのうち範囲に入らずに読んだ後で捨てる条件（インデックスのない列の条件。
// `CAST(meta_value AS SIGNED) = '1'` など）の割合（residual。なければ 1）を返す（段階 95。coster.go の discardedRowsCost）。
// ctx が nil なら空の文脈を作る（作るのは重いので、JOIN のコストから呼ぶときは文の文脈を渡す）。
func filterParts(ctx *sql.Context, rel *memo.Filter) (sel, residual float64) {
	var covered sql.ColSet
	var table *Table
	var scan *memo.IndexScan
	alias := ""
	switch s := rel.Child.First.(type) {
	case *memo.IndexScan:
		scan = s
		if s.Table != nil {
			table = tableOf(s.Table.UnderlyingTable())
		}
		alias = s.Alias
	case *memo.TableScan:
		if tn, ok := s.Table.(sql.TableNode); ok {
			table = tableOf(tn.UnderlyingTable())
		}
	case *memo.TableAlias:
		if tn, ok := s.Table.Child.(sql.TableNode); ok {
			table = tableOf(tn.UnderlyingTable())
		}
	}
	if table == nil {
		return memo.DefaultFilterSelectivity, 1
	}
	var conds []sql.Expression
	for _, f := range rel.Filters {
		conds = append(conds, expression.SplitConjunction(f)...)
	}
	sel, residual = 1.0, 1.0
	counted := false
	used, inRange := map[sql.Expression]bool{}, map[sql.Expression]bool{}
	if scan != nil && scan.Index != nil && scan.Table != nil {
		if ctx == nil {
			ctx = sql.NewEmptyContext()
		}
		name := table.Name()
		if alias != "" {
			name = alias
		}
		idx, points := leadingPoints(ctx, scan.Table)
		for i, c := range scan.Index.Cols() {
			if i < len(points) {
				covered.Add(c)
			}
		}
		if idx != nil && len(points) > 0 && len(points) < len(idx.columns) {
			// 点で決まる列の次の列の上か下を決める条件は、範囲で読む行を切る（go-mysql-server の範囲か、後で狭める
			// narrowRange。API 用の索引の key_value の meta_value BETWEEN、type_status_date の post_date の範囲）ので、読んだ後で
			// 捨てる条件に数えない
			col := idx.columns[len(points)]
			typ := table.sch.Schema[table.def.ColumnIndex(col.Name)].Type
			for _, c := range conds {
				if lo, hi := exprBounds(ctx, name, col.Name, typ, c); lo.set || hi.set {
					inRange[c] = true
				}
			}
		}
		if idx != nil && len(points) > 0 {
			// 元の 0.85 より絞るときだけ使う（post_type = 'post' の後の post_status = 'publish' は 0.98 だが、JOIN の順の
			// 見積もりは 0.85 のままで MySQL と同じ順になっている。TestCategoryQueryPlans）。
			if s, eqs, ok := samplePrefixSelectivity(ctx, table, name, idx, points, conds); ok {
				for _, e := range eqs {
					used[e] = true
				}
				if s < memo.DefaultFilterSelectivity {
					sel, counted = s, true
				}
			}
		}
	}
	leading := table.leadingIndexColumns()
	for _, c := range conds {
		if used[c] {
			continue
		}
		cols := conditionColumns(c)
		if cols.Len() > 0 && cols.SubsetOf(covered) {
			continue
		}
		// インデックスの先頭の列を見る条件は、統計のある列なので、割合を決め打ちしない（元の 0.85 のまま。
		// post_type = 'post' のように値の種類の少ない列に 0.1 を掛けると、行数を少なく見積もりすぎる）。
		if referencesAny(c, leading) {
			continue
		}
		counted = true
		sel *= conditionSelectivity(c)
		if !inRange[c] {
			residual *= conditionSelectivity(c)
		}
	}
	if !counted {
		return memo.DefaultFilterSelectivity, residual
	}
	return sel, residual
}

// leadingPoints は、access の範囲がどれも同じ値で先頭から点で決める列の値（インデックスの列の順）と、インデックスを返す。
func leadingPoints(ctx *sql.Context, access *plan.IndexedTableAccess) (*Index, []any) {
	if !access.IsStatic() {
		return nil, nil
	}
	lookup, err := access.GetLookup(ctx, nil)
	if err != nil {
		return nil, nil
	}
	idx, ok := lookup.Index.(*Index)
	ranges, ok2 := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok || !ok2 || len(ranges) == 0 {
		return nil, nil
	}
	var points []any
	for i := range idx.columns {
		var v any
		for j, r := range ranges {
			if i >= len(r) || !isPoint(ctx, r[i]) {
				return idx, points
			}
			k := sql.GetMySQLRangeCutKey(r[i].LowerBound)
			if j == 0 {
				v = k
			} else if c, err := r[i].Typ.Compare(ctx, v, k); err != nil || c != 0 {
				return idx, points
			}
		}
		points = append(points, v)
	}
	return idx, points
}

// samplePrefixSelectivity は、インデックス idx の先頭の列が points に決まる行のうち、条件 conds の等号（列 = 定数）が
// その次の列から続けて決める行の割合を、統計の標本で見積もる。使った等号の条件も返す。次の列の等号がないか、標本に
// 行がなければ ok が false。
func samplePrefixSelectivity(ctx *sql.Context, table *Table, name string, idx *Index, points []any, conds []sql.Expression) (float64, []sql.Expression, bool) {
	vals := append([]any(nil), points...)
	var eqs []sql.Expression
	for len(vals) < len(idx.columns) {
		col := idx.columns[len(vals)]
		var found sql.Expression
		var v any
		for _, c := range conds {
			fixed := fixedColumns(ctx, table, name, []sql.Expression{c})
			if fv, ok := fixed[strings.ToLower(col.Name)]; ok && len(fixed) == 1 {
				found, v = c, fv
				break
			}
		}
		if found == nil {
			break
		}
		vals = append(vals, v)
		eqs = append(eqs, found)
	}
	if len(eqs) == 0 {
		return 0, nil, false
	}
	all, ok1 := samplePrefixRows(table, idx, points)
	some, ok2 := samplePrefixRows(table, idx, vals)
	if !ok1 || !ok2 || all <= 0 {
		return 0, nil, false
	}
	return math.Min(some/all, 1), eqs, true
}

// conditionColumns は、条件 e が読む列の集合。
func conditionColumns(e sql.Expression) sql.ColSet {
	var cols sql.ColSet
	transform.InspectExpr(e, func(e sql.Expression) bool {
		if gf, ok := e.(*expression.GetField); ok {
			cols.Add(gf.Id())
		}
		return false
	})
	return cols
}

// conditionSelectivity は、1 つの条件で残る行の割合（MySQL の condition filtering の既定の値）。
func conditionSelectivity(e sql.Expression) float64 {
	switch e.(type) {
	case *expression.Equals, *expression.NullSafeEquals:
		return 0.1
	case *expression.GreaterThan, *expression.GreaterThanOrEqual, *expression.LessThan, *expression.LessThanOrEqual:
		return 1.0 / 3
	case *expression.Between:
		return 1.0 / 9
	}
	return memo.DefaultFilterSelectivity
}

// tableOf は、t が HeimDB の表ならそれを返す。
func tableOf(t sql.Table) *Table {
	if it, ok := t.(*IndexedTable); ok {
		return it.Table
	}
	return unwrapTable(t)
}

// leadingIndexColumns は、主キーとインデックスの先頭の列の名前（小文字）。
func (t *Table) leadingIndexColumns() map[string]bool {
	cols := map[string]bool{}
	if len(t.def.PKOrdinals) > 0 {
		cols[strings.ToLower(t.def.Columns[t.def.PKOrdinals[0]].Name)] = true
	}
	for _, ki := range t.keyIndexes() {
		if len(ki.ords) > 0 {
			cols[strings.ToLower(t.def.Columns[ki.ords[0]].Name)] = true
		}
	}
	return cols
}

// referencesAny は、条件 e が名前 cols のどれかの列を読むか。
func referencesAny(e sql.Expression, cols map[string]bool) bool {
	return transform.InspectExpr(e, func(e sql.Expression) bool {
		gf, ok := e.(*expression.GetField)
		return ok && cols[strings.ToLower(gf.Name())]
	})
}
