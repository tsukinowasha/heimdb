package backend

import (
	"math"
	"strings"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/stats"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// orderByIndexWithFixedPrefixId は HeimDB が足す解析ルールの ID。go-mysql-server のルールと重ならない値にする。
const orderByIndexWithFixedPrefixId analyzer.RuleId = 10000

// narrowIndexRangesId は、インデックスの範囲を後ろの列の条件で狭める解析ルールの ID。
const narrowIndexRangesId analyzer.RuleId = 10009

// NewEngine は、HeimDB の解析ルールを足した go-mysql-server のエンジンを作る（sqle.NewDefault と同じ構成に追加する）。
func NewEngine(pro sql.DatabaseProvider) *sqle.Engine {
	a := analyzer.NewBuilder(pro).
		AddPreAnalyzeRule(likeExplicitEscapeId, likeExplicitEscape).
		AddPreAnalyzeRule(fulltextBeforeAnalysisId, fulltextBeforeAnalysis).
		AddPreAnalyzeRule(setNamesCollateId, setNamesCollate).
		AddPreAnalyzeRule(simplifyOuterJoinsId, simplifyOuterJoins).
		AddPostAnalyzeRule(fullScanForWideRangeId, fullScanForWideRange).
		AddPostAnalyzeRule(orderByIndexWithFixedPrefixId, orderByIndexWithFixedPrefix).
		AddPostAnalyzeRule(coveringIndexForSortId, coveringIndexForSort).
		AddPostAnalyzeRule(narrowIndexRangesId, narrowIndexRanges).
		AddPostAnalyzeRule(dropRangeFiltersId, dropRangeFilters).
		AddPostAnalyzeRule(checkLookupKeysId, checkLookupKeys).
		AddPostAnalyzeRule(columnTypedLiteralsId, columnTypedLiterals).
		AddPostAnalyzeRule(showIndexesWithSubPartId, showIndexesWithSubPart).
		AddPostAnalyzeRule(showColumnsMySQLId, showColumnsMySQL).
		AddPostAnalyzeRule(fulltextIndexesId, fulltextIndexes).
		AddPostAnalyzeRule(showCreateTableFulltextId, showCreateTableFulltext).
		AddPostAnalyzeRule(foundRowsWithOffsetId, foundRowsWithOffset).
		AddPostAnalyzeRule(tableRowsId, tableRows).
		AddPostAnalyzeRule(tableRowsOnlyNamedId, tableRowsOnlyNamed).
		Build()
	e := sqle.New(a, nil)
	e.Analyzer.ExecBuilder = execBuilder{e.Analyzer.ExecBuilder}
	e.Analyzer.Coster = coster{e.Analyzer.Coster}
	e.Analyzer.Catalog.StatsProvider = statsProvider{e.Analyzer.Catalog.StatsProvider}
	e.Analyzer.Catalog.MySQLDb.SetPersister(noAuthPersister{})
	if p, ok := pro.(*Provider); ok {
		p.users = e.Analyzer.Catalog.MySQLDb
		p.engineMemory = e.MemoryManager
	}
	e.Parser = likeEscapeParser{Parser: e.Parser}
	return e
}

// orderByIndexWithFixedPrefix は、ORDER BY をインデックスの走査に置き換える。go-mysql-server の replaceIdxSort は
// ソート列がインデックスの先頭列と一致するときしか置き換えないが、MySQL は WHERE の等号で先頭の列が 1 つの値に
// 固定されていれば、その次の列による並べ替えにもインデックスを使う。
//
// 例: WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC は、MySQL では
// type_status_date (post_type, post_status, post_date, ID) の逆方向走査になり、post_date が同じ行は ID の降順で返る。
// go-mysql-server のままでは Sort が残り、同順位の行が入力順（ID の昇順）になる。
//
// Sort（または TopN）の下に Project と Filter を挟んでテーブルがあり、次の条件を満たすインデックスがあれば、
// そのインデックスの走査（DESC なら逆方向）に置き換えて Sort を外す。
//   - インデックスの先頭の k 列（k >= 1）が、Filter の等号（列 = 定数）ですべて固定されている
//   - ソート列（向きはすべて同じ）が、その次の列と順に一致し、プレフィックス長がない
//
// LIMIT があれば（TopN か、Limit の下の Sort）、先頭の列がソート列のインデックス（k = 0）も、統計で少しの行で
// LIMIT に届くと分かるときに使う（sortIndexForLimit）。
//
// Filter は残すので、結果の行は変わらず、並び順だけが MySQL と同じになる。
func orderByIndexWithFixedPrefix(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		switch node := node.(type) {
		case *plan.Sort:
			child, ok, err := orderedChild(ctx, node.SortFields, nil, false, node.Child)
			if err != nil || !ok {
				return node, transform.SameTree, err
			}
			return child, transform.NewTree, nil
		case *plan.TopN:
			child, ok, err := orderedChild(ctx, node.Fields, node.Limit, node.CalcFoundRows, node.Child)
			if err != nil || !ok {
				return node, transform.SameTree, err
			}
			return plan.NewLimit(node.Limit, child).WithCalcFoundRows(node.CalcFoundRows), transform.NewTree, nil
		case *plan.Limit:
			// Limit の下に Project を挟んで Sort があれば、Sort を LIMIT の行数つきで置き換える。LIMIT m, n は、Limit(n) の
			// 下に Offset(m) があり、m + n 行で止まる（SQL_CALC_FOUND_ROWS なら Offset(m) の下の Limit(m + n)）。
			// どのページも同じ読み方にする（ページごとに読み方が違うと、同順位の行の並びが変わって、ページの境目の
			// 行が重なったり抜けたりする）。
			limit := node.Limit
			cur := node.Child
			if off, ok := cur.(*plan.Offset); ok {
				limit = expression.NewPlus(node.Limit, off.Offset)
				cur = off.Child
			}
			var projects []*plan.Project
			for {
				p, ok := cur.(*plan.Project)
				if !ok {
					break
				}
				projects = append(projects, p)
				cur = p.Child
			}
			sort, ok := cur.(*plan.Sort)
			if !ok {
				return node, transform.SameTree, nil
			}
			calcFoundRows := node.CalcFoundRows
			child, ok, err := orderedChild(ctx, sort.SortFields, limit, node.CalcFoundRows, sort.Child)
			if err != nil {
				return node, transform.SameTree, err
			}
			if !ok {
				if len(projects) == 0 {
					return node, transform.SameTree, nil // Limit の直下の Sort は、go-mysql-server が TopN にする
				}
				// Limit と Sort の間に Project があると、go-mysql-server の insertTopNNodes は TopN にせず、全部の行を
				// 並べ替えてから LIMIT の行を取る（GROUP BY の後の ORDER BY wp_postmeta.meta_value+0 DESC LIMIT 20 は、
				// 2 万行を並べ替え、比べるたびに文字列から数を作っていた。段階 83）。Sort を TopN にする。TopN は入力の
				// 順を最後のキーにするので、同順位の行の並びは Sort と同じ。SQL_CALC_FOUND_ROWS は TopN が数える
				// （insertTopNNodes と同じ）。
				child = plan.NewTopN(sort.SortFields, limit, sort.Child).WithCalcFoundRows(node.CalcFoundRows)
				calcFoundRows = false
			}
			for i := len(projects) - 1; i >= 0; i-- {
				if child, err = projects[i].WithChildren(child); err != nil {
					return nil, transform.SameTree, err
				}
			}
			if off, ok := node.Child.(*plan.Offset); ok {
				if child, err = off.WithChildren(child); err != nil {
					return nil, transform.SameTree, err
				}
			}
			newNode, err := node.WithCalcFoundRows(calcFoundRows).WithChildren(child)
			return newNode, transform.NewTree, err
		}
		return node, transform.SameTree, nil
	})
}

// narrowIndexRanges は、go-mysql-server が選んだインデックスの範囲を、Filter に残る条件で、後ろの列まで狭める。
//
// HeimDB の統計はインデックスの先頭の列だけなので（statistics.go）、go-mysql-server は先頭の列の条件しか範囲に
// 入れない。post_type = 'post' AND post_status = 'publish' で type_status_date を選んでも、範囲は post_type だけになり、
// 下書きなども読んでから Filter で捨てる。MySQL は、先頭から等号の列が続く間と、その次の列の範囲まで使う。
//
// 条件に、インデックスの整数の列と列の型に入らない定数の比較があれば、その列から後ろはすべての値を読む
// （outOfRangeColumn）。
//
// 範囲がどれも「先頭からの等号の列、残りはすべての値」の形なら、残りの列を、先頭から順に、条件の等号（列 = 定数）で
// 固定し、等号のない最初の列には条件の範囲（columnRange）を付ける。条件は AND の項だけを見るので、どの行も満たし、
// 範囲を狭めても読む行が減るだけで、結果と並び順は変わらない（Filter も残る）。
func narrowIndexRanges(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		filter, ok := node.(*plan.Filter)
		if !ok {
			return node, transform.SameTree, nil
		}
		alias := ""
		cur := filter.Child
		if ta, ok := cur.(*plan.TableAlias); ok {
			alias = ta.Name()
			cur = ta.Child
		}
		access, ok := cur.(*plan.IndexedTableAccess)
		if !ok || !access.IsStatic() {
			return node, transform.SameTree, nil
		}
		var table *Table
		switch u := access.UnderlyingTable().(type) {
		case *Table:
			table = u
		case *IndexedTable:
			table = u.Table
		default:
			return node, transform.SameTree, nil
		}
		name := table.Name()
		if alias != "" {
			name = alias
		}
		lookup, err := access.GetLookup(ctx, nil)
		if err != nil {
			return node, transform.SameTree, nil
		}
		idx, ok := lookup.Index.(*Index)
		ranges, ok2 := lookup.Ranges.(sql.MySQLRangeCollection)
		if !ok || !ok2 || idx.fulltext {
			return node, transform.SameTree, nil
		}
		widened := false
		if pos := outOfRangeColumn(ctx, table, idx, name, filter.Expression); pos >= 0 {
			// go-mysql-server が範囲を作るときに定数を列の型に丸めている。その列から後ろはすべての値を読み（並び順は
			// 変わらない）、Filter で絞る。後ろの列は、下で条件の等号と範囲（列の型に入る定数だけ）で狭め直す。
			ranges = widenRanges(ctx, table, idx, ranges, pos)
			lookup.IsEmptyRange = false
			lookup.IsPointLookup = false
			widened = true
		} else if lookup.IsEmptyRange {
			return node, transform.SameTree, nil
		}
		conds := expression.SplitConjunction(filter.Expression)
		fixed := fixedColumns(ctx, table, name, conds)
		narrowed := make(sql.MySQLRangeCollection, len(ranges))
		changed := widened
		for i, r := range ranges {
			nr, ok := narrowRange(ctx, table, idx, name, r, fixed, conds)
			if !ok {
				nr = nil
			}
			narrowed[i] = nr
			changed = changed || nr != nil
		}
		if !changed {
			return node, transform.SameTree, nil
		}
		for i := range narrowed {
			if narrowed[i] == nil {
				narrowed[i] = ranges[i]
			}
		}
		lookup.Ranges = narrowed
		newAccess, err := plan.NewStaticIndexedAccessForTableNode(ctx, access.TableNode, lookup)
		if err != nil {
			return nil, transform.SameTree, err
		}
		var child sql.Node = newAccess
		if alias != "" {
			if child, err = filter.Child.WithChildren(child); err != nil {
				return nil, transform.SameTree, err
			}
		}
		newFilter, err := filter.WithChildren(child)
		return newFilter, transform.NewTree, err
	})
}

// outOfRangeColumn は、条件 e に、インデックス idx の整数の列と、列の型にそのまま入らない定数の比較（IN、BETWEEN を
// 含む）があれば、そういう列のうちインデックスで最も前のものの位置を返す（なければ -1）。テーブルの名前は name。
// go-mysql-server は、範囲を作るときに定数を列の型に丸める（int unsigned の列で k > -1 は (4294967295, ∞)、
// k > -0.5 は (2147483646, ∞)、k < 2^63+1 は 2^63+1 を下位のビットに切り詰めて (NULL, 1)）ので、範囲が狭すぎて
// 行を落とす（MySQL はすべての行）。
func outOfRangeColumn(ctx *sql.Context, table *Table, idx *Index, name string, e sql.Expression) int {
	pos := -1
	sql.Inspect(e, func(e sql.Expression) bool {
		var operands []sql.Expression
		switch e := e.(type) {
		case expression.Comparer:
			operands = []sql.Expression{e.Left(), e.Right()}
		case *expression.Between:
			operands = []sql.Expression{e.Val, e.Lower, e.Upper}
		case *expression.InTuple, *expression.HashInTuple:
			in := e.Children()
			operands = []sql.Expression{in[0]}
			if t, ok := in[1].(expression.Tuple); ok {
				operands = append(operands, t...)
			}
		default:
			return true
		}
		gf, ok := operands[0].(*expression.GetField)
		if !ok {
			gf, ok = operands[len(operands)-1].(*expression.GetField)
		}
		if !ok || !strings.EqualFold(gf.Table(), name) {
			return true
		}
		col := -1
		for i, c := range idx.columns {
			if strings.EqualFold(c.Name, gf.Name()) {
				col = i
				break
			}
		}
		if col < 0 || (pos >= 0 && col >= pos) {
			return true
		}
		typ := table.sch.Schema[table.def.ColumnIndex(gf.Name())].Type
		if !types.IsInteger(typ) {
			return true
		}
		for _, op := range operands {
			if lit, ok := op.(*expression.Literal); ok && keyChanged(ctx, typ, lit.Value()) {
				pos = col
				break
			}
		}
		return true
	})
	return pos
}

// widenRanges は、範囲 ranges の、インデックスの位置 pos から後ろの列を、すべての値にする（outOfRangeColumn）。
// pos より前でも、1 つの値でない列がある範囲があれば、どの範囲もその列から広げる（広げた範囲が重ならないように、
// 残す列はどの範囲も 1 つの値にする）。同じ値の範囲は 1 つにまとめる（並びは同じ）。
func widenRanges(ctx *sql.Context, table *Table, idx *Index, ranges sql.MySQLRangeCollection, pos int) sql.MySQLRangeCollection {
	all := make(sql.MySQLRange, len(idx.columns))
	for i, c := range idx.columns {
		all[i] = sql.AllRangeColumnExpr(table.sch.Schema[table.def.ColumnIndex(c.Name)].Type)
	}
	for _, r := range ranges {
		if len(r) != len(idx.columns) {
			return sql.MySQLRangeCollection{all}
		}
		m := 0
		for m < pos && isPoint(ctx, r[m]) {
			m++
		}
		pos = m
	}
	if pos == 0 {
		return sql.MySQLRangeCollection{all}
	}
	var out sql.MySQLRangeCollection
	for _, r := range ranges {
		nr := append(append(sql.MySQLRange(nil), r[:pos]...), all[pos:]...)
		if len(out) > 0 && samePoints(ctx, out[len(out)-1][:pos], nr[:pos]) {
			continue
		}
		out = append(out, nr)
	}
	return out
}

// isPoint は、列の範囲 ce が 1 つの値（NULL でない）か。
func isPoint(ctx *sql.Context, ce sql.MySQLRangeColumnExpr) bool {
	_, lowerIncl := ce.LowerBound.(sql.Below)
	_, upperIncl := ce.UpperBound.(sql.Above)
	if !lowerIncl || !upperIncl {
		return false
	}
	lo, hi := sql.GetMySQLRangeCutKey(ce.LowerBound), sql.GetMySQLRangeCutKey(ce.UpperBound)
	if lo == nil || hi == nil {
		return false
	}
	c, err := ce.Typ.Compare(ctx, lo, hi)
	return err == nil && c == 0
}

// samePoints は、1 つの値の列の範囲の並び a と b が、同じ値か。
func samePoints(ctx *sql.Context, a, b sql.MySQLRange) bool {
	for i := range a {
		c, err := a[i].Typ.Compare(ctx, sql.GetMySQLRangeCutKey(a[i].LowerBound), sql.GetMySQLRangeCutKey(b[i].LowerBound))
		if err != nil || c != 0 {
			return false
		}
	}
	return true
}

// narrowRange は、インデックス idx の範囲 r を、条件で狭めた範囲を返す（narrowIndexRanges）。狭められなければ nil、
// 範囲が「先頭からの等号の列、残りはすべての値」の形でなければ ok が false。
func narrowRange(ctx *sql.Context, table *Table, idx *Index, name string, r sql.MySQLRange, fixed map[string]any, conds []sql.Expression) (sql.MySQLRange, bool) {
	if len(r) != len(idx.columns) {
		return nil, false
	}
	m := 0
	for m < len(r) && isPoint(ctx, r[m]) {
		m++
	}
	if m == 0 {
		return nil, false
	}
	for _, ce := range r[m:] {
		_, lowerAll := ce.LowerBound.(sql.BelowNull)
		_, upperAll := ce.UpperBound.(sql.AboveAll)
		if !lowerAll || !upperAll {
			return nil, false
		}
	}
	nr := append(sql.MySQLRange(nil), r...)
	changed := false
	for j := m; j < len(nr); j++ {
		col := idx.columns[j]
		typ := table.sch.Schema[table.def.ColumnIndex(col.Name)].Type
		if v, ok := fixed[strings.ToLower(col.Name)]; ok {
			nr[j] = sql.ClosedRangeColumnExpr(v, v, typ)
			changed = true
			continue
		}
		rng := columnRange(ctx, name, col.Name, typ, conds)
		_, lowerAll := rng.LowerBound.(sql.BelowNull)
		_, upperAll := rng.UpperBound.(sql.AboveAll)
		if !lowerAll || !upperAll {
			nr[j] = rng
			changed = true
		}
		break
	}
	if !changed {
		return nil, true
	}
	return nr, true
}

// constInt は、定数だけからなる式 e（LIMIT の行数、LIMIT m, n の m + n）の値を返す。
func constInt(ctx *sql.Context, e sql.Expression) (int64, bool) {
	if e == nil {
		return 0, false
	}
	constant := true
	sql.Inspect(e, func(e sql.Expression) bool {
		switch e.(type) {
		case *expression.Literal, *expression.Arithmetic:
		default:
			constant = false
		}
		return constant
	})
	if !constant {
		return 0, false
	}
	v, err := e.Eval(ctx, nil)
	if err != nil {
		return 0, false
	}
	n, _, err := types.Int64.Convert(ctx, v)
	if err != nil || n == nil || n.(int64) < 0 {
		// 負は、LIMIT m, n の m + n が溢れたとき（OFFSET 18446744073709551615）。
		return 0, false
	}
	return n.(int64), true
}

// orderedChild は、child の下のテーブルをインデックスの走査に置き換えた木を返す。置き換えられなければ ok が false。
// limit は TopN か Limit の行数（LIMIT のない Sort なら nil）、foundRows は SQL_CALC_FOUND_ROWS か。
func orderedChild(ctx *sql.Context, fields sql.SortFields, limit sql.Expression, foundRows bool, child sql.Node) (sql.Node, bool, error) {
	if len(fields) == 0 {
		return nil, false, nil
	}
	order := fields[0].Order
	for _, f := range fields {
		if f.Order != order || f.NullOrdering != sql.NullsFirst {
			return nil, false, nil
		}
	}

	// Project と Filter（と別名）をたどってテーブルまで下りる。
	var filters []sql.Expression
	var leaf sql.TableNode
	var access *plan.IndexedTableAccess
	alias := ""
	for cur := child; leaf == nil; {
		switch c := cur.(type) {
		case *plan.Project:
			cur = c.Child
		case *plan.TableAlias:
			alias = c.Name()
			cur = c.Child
		case *plan.Filter:
			filters = append(filters, expression.SplitConjunction(c.Expression)...)
			cur = c.Child
		case *plan.IndexedTableAccess:
			if !c.IsStatic() {
				return nil, false, nil
			}
			leaf, access = c.TableNode, c
		case *plan.ResolvedTable:
			leaf = c
		default:
			return nil, false, nil
		}
	}
	var table *Table
	switch u := leaf.UnderlyingTable().(type) {
	case *Table:
		table = u
	case *IndexedTable:
		table = u.Table
	default:
		return nil, false, nil
	}

	// 列はテーブル名（別名があれば別名）で修飾されている。
	name := table.Name()
	if alias != "" {
		name = alias
	}
	sortCols := make([]string, len(fields))
	for i, f := range fields {
		gf, ok := f.Column.(*expression.GetField)
		if !ok || !strings.EqualFold(gf.Table(), name) {
			return nil, false, nil
		}
		sortCols[i] = gf.Name()
	}
	fixed := fixedColumns(ctx, table, name, filters)
	idx, k := bestIndex(ctx, table, fixed, sortCols)
	if idx != nil {
		if points, ok := pointListRows(ctx, access); ok {
			// go-mysql-server が一意のインデックスの点（post__in の ID IN (...)）で読むと決めていて、点の数が並べ替えの
			// インデックスで読む行より十分少なければ、置き換えずに、MySQL と同じく点で引いて並べ替える（段階 84。
			// type_status_date を逆に読むと、その種類と状態の記事をすべて読んでいた）。
			if sortRows, ok := prefixRows(ctx, table, idx, k, fixed); ok && cheaperThanSortIndex(ctx, sortRows, points, limit, foundRows) {
				return nil, false, nil
			}
		}
		if alt, altK, ok := narrowerIndex(ctx, table, fixed, idx, k, limit, foundRows); ok {
			// 等号で決まる行が見積もりで少ないインデックスがあれば、MySQL と同じくそれで引いて並べ替える（リビジョンの
			// 一覧の post_parent = N AND post_type = 'revision' ... ORDER BY post_date DESC は、post_parent で引く。
			// type_status_date を逆に読むと、サイトのすべてのリビジョンを読む）。
			access, err := pointAccess(ctx, table, leaf, alt, altK, fixed)
			if err != nil {
				return nil, false, err
			}
			newChild, err := replaceLeaf(child, access)
			if err != nil {
				return nil, false, err
			}
			return plan.NewSort(fields, newChild), true, nil
		}
	}
	if idx == nil {
		if idx = sortIndexForLimit(ctx, table, access, limit, sortCols); idx == nil {
			return nil, false, nil
		}
		if foundRows {
			// SQL_CALC_FOUND_ROWS はすべての行を数えるので、LIMIT で止まらない。MySQL は、条件のインデックスで大半の行を
			// 読むなら、表を主キーの順に全部読んで並べ替える（同順位の行は主キーの昇順）。同じにする。
			newChild, err := replaceLeaf(child, leaf)
			if err != nil {
				return nil, false, err
			}
			return plan.NewSort(fields, newChild), true, nil
		}
	}
	rng := make(sql.MySQLRange, len(idx.columns))
	for i, c := range idx.columns {
		typ := table.sch.Schema[table.def.ColumnIndex(c.Name)].Type
		switch {
		case i < k:
			v := fixed[strings.ToLower(c.Name)]
			rng[i] = sql.ClosedRangeColumnExpr(v, v, typ)
		case i == k:
			// 最初のソート列は、条件の範囲（前後の記事の post_date < X OR (post_date = X AND ID < N) なら post_date <= X）
			// から読み始める。
			rng[i] = columnRange(ctx, name, c.Name, typ, filters)
		default:
			rng[i] = sql.AllRangeColumnExpr(typ)
		}
	}
	lookup := sql.NewIndexLookup(idx, sql.MySQLRangeCollection{rng}, false, false, false, order == sql.Descending)
	access, err := plan.NewStaticIndexedAccessForTableNode(ctx, leaf, lookup)
	if err != nil {
		return nil, false, err
	}

	newChild, err := replaceLeaf(child, access)
	if err != nil {
		return nil, false, err
	}
	return newChild, true, nil
}

// replaceLeaf は、child の下のテーブル（IndexedTableAccess か ResolvedTable）を leaf に置き換えた木を返す。
func replaceLeaf(child sql.Node, leaf sql.Node) (sql.Node, error) {
	newChild, _, err := transform.Node(child, func(n sql.Node) (sql.Node, transform.TreeIdentity, error) {
		switch n.(type) {
		case *plan.IndexedTableAccess, *plan.ResolvedTable:
			return leaf, transform.NewTree, nil
		}
		return n, transform.SameTree, nil
	})
	return newChild, err
}

// sortIndexForLimit は、ORDER BY ... LIMIT n で、先頭の列がソート列と一致するインデックスを順に読めば、少しの行で
// LIMIT に届くと統計から分かるとき、そのインデックスを返す（最近のコメントの
// comment_approved IN ('0', '1') ... ORDER BY comment_date_gmt DESC LIMIT 25 は、MySQL と同じく日付のインデックスを
// 逆に読んで 25 行で止まる）。
//
// go-mysql-server が条件のために選んだインデックスの範囲（access）が、どれも先頭の列の等号なら、統計で条件に
// 当てはまる行の割合 f を見積もる。ソート列のインデックスを読むと約 n / f 行、今のままだと約 f × 行数を読むので、
// 前の方が少ないときだけ置き換える。ただし f が 2 分の 1 より小さいときは置き換えない（MySQL は、条件で 3 分の 1 に
// 絞れるなら、LIMIT が小さくても条件のインデックスを読んで並べ替える。置き換えると同順位の行の並びが MySQL と
// 変わる）。
func sortIndexForLimit(ctx *sql.Context, table *Table, access *plan.IndexedTableAccess, limit sql.Expression, sortCols []string) *Index {
	if limit == nil || access == nil {
		return nil
	}
	n, ok := constInt(ctx, limit)
	if !ok {
		return nil
	}
	f, total, ok := pointRangeFraction(ctx, table, access)
	if !ok {
		return nil
	}
	if f < 0.5 || float64(n)/f >= f*total {
		return nil
	}

	indexes, err := table.GetIndexes(ctx)
	if err != nil {
		return nil
	}
	for _, i := range indexes {
		idx := i.(*Index)
		if idx.fulltext || len(idx.columns) < len(sortCols) {
			continue
		}
		match := true
		for j, col := range sortCols {
			if c := idx.columns[j]; !strings.EqualFold(c.Name, col) || c.Length > 0 {
				match = false
				break
			}
		}
		if match {
			return idx
		}
	}
	return nil
}

// fixedColumns は、条件のうち「このテーブルの列 = 定数」の形のものから、列名（小文字）と値（列の型に変換したもの）を返す。
// name は、条件の列を修飾するテーブル名（別名があれば別名）。
func fixedColumns(ctx *sql.Context, table *Table, name string, conds []sql.Expression) map[string]any {
	fixed := map[string]any{}
	for _, cond := range conds {
		var gf *expression.GetField
		var lit *expression.Literal
		switch c := cond.(type) {
		case *expression.Equals:
			gf, lit = columnAndLiteral(c.Left(), c.Right())
			if gf == nil {
				gf, lit = columnAndLiteral(c.Right(), c.Left())
			}
		case *expression.InTuple, *expression.HashInTuple:
			// 値が 1 つの IN は等号と同じ（MySQL も同じ扱い）。go-mysql-server は IN を HashInTuple にする。
			in := c.(sql.Expression).Children()
			if t, ok := in[1].(expression.Tuple); ok && len(t) == 1 {
				gf, lit = columnAndLiteral(in[0], t[0])
			}
		}
		if gf == nil || !strings.EqualFold(gf.Table(), name) || lit.Value() == nil {
			continue
		}
		ord := table.def.ColumnIndex(gf.Name())
		if ord < 0 {
			continue
		}
		// 文字列の列と数は数として比べる（meta_key = 12 は '12abc' にも当たる）ので、1 つの値に決まらない（段階 88）
		if types.IsTextOnly(table.sch.Schema[ord].Type) && types.IsNumber(lit.Type()) {
			continue
		}
		v, ok := convertKey(ctx, table.sch.Schema[ord].Type, lit.Value())
		if !ok {
			continue
		}
		fixed[strings.ToLower(gf.Name())] = v
	}
	return fixed
}

// rangeBound は、列の値の範囲の片側の端。set が false なら端がない。
type rangeBound struct {
	key  any
	incl bool
	set  bool
}

// columnRange は、条件 conds（AND）が、テーブル name の列 col の値に課す範囲を返す。範囲を決められない条件は
// 無視する（範囲が広がるだけで、条件は Filter に残る）。
func columnRange(ctx *sql.Context, name, col string, typ sql.Type, conds []sql.Expression) sql.MySQLRangeColumnExpr {
	var lo, hi rangeBound
	for _, c := range conds {
		l, h := exprBounds(ctx, name, col, typ, c)
		lo, hi = pickBound(ctx, typ, lo, l, true, false), pickBound(ctx, typ, hi, h, false, false)
	}
	r := sql.AllRangeColumnExpr(typ)
	if lo.set {
		if lo.incl {
			r.LowerBound = sql.Below{Key: lo.key}
		} else {
			r.LowerBound = sql.Above{Key: lo.key}
		}
	}
	if hi.set {
		if hi.incl {
			r.UpperBound = sql.Above{Key: hi.key}
		} else {
			r.UpperBound = sql.Below{Key: hi.key}
		}
	}
	return r
}

// exprBounds は、条件 e が真になるときの列の値の下端と上端を返す。比較（列と定数）、AND、OR だけを見る。
func exprBounds(ctx *sql.Context, name, col string, typ sql.Type, e sql.Expression) (lo, hi rangeBound) {
	switch e := e.(type) {
	case *expression.And:
		l1, h1 := exprBounds(ctx, name, col, typ, e.LeftChild)
		l2, h2 := exprBounds(ctx, name, col, typ, e.RightChild)
		return pickBound(ctx, typ, l1, l2, true, false), pickBound(ctx, typ, h1, h2, false, false)
	case *expression.Or:
		l1, h1 := exprBounds(ctx, name, col, typ, e.LeftChild)
		l2, h2 := exprBounds(ctx, name, col, typ, e.RightChild)
		return pickBound(ctx, typ, l1, l2, true, true), pickBound(ctx, typ, h1, h2, false, true)
	case expression.Comparer:
		gf, lit := columnAndLiteral(e.Left(), e.Right())
		flipped := false
		if gf == nil {
			gf, lit = columnAndLiteral(e.Right(), e.Left())
			flipped = true
		}
		if gf == nil || !strings.EqualFold(gf.Table(), name) || !strings.EqualFold(gf.Name(), col) || lit.Value() == nil {
			return
		}
		// 変換で値が丸められると端がずれて行を落としうるので、日時の列と、整数の列と整数の定数の比較と、文字列の列と
		// 同じ文字セットの文字列の定数の比較（列の照合順序で比べる。数との比較は MySQL が数として比べるので見ない）だけを
		// 見る（文字列は段階 84。meta_key = 'event_date' AND meta_value BETWEEN ... が meta_value の範囲を読まなかった）。
		_, _, text := columnLiteral(gf, lit)
		if !types.IsTime(typ) && !(types.IsInteger(typ) && types.IsInteger(lit.Type())) && !text {
			return
		}
		v, ok := convertKey(ctx, typ, lit.Value())
		if !ok {
			return
		}
		b := rangeBound{key: v, set: true}
		switch e.(type) {
		case *expression.Equals:
			b.incl = true
			return b, b
		case *expression.LessThan, *expression.LessThanOrEqual:
			_, b.incl = e.(*expression.LessThanOrEqual)
			if flipped {
				return b, hi
			}
			return lo, b
		case *expression.GreaterThan, *expression.GreaterThanOrEqual:
			_, b.incl = e.(*expression.GreaterThanOrEqual)
			if flipped {
				return lo, b
			}
			return b, hi
		}
	}
	return
}

// pickBound は、2 つの端のうち、lower（下端か）と union（OR か）に応じたものを返す。AND では狭い方、OR では広い方
// （どちらかに端がなければ端なし）。
func pickBound(ctx *sql.Context, typ sql.Type, a, b rangeBound, lower, union bool) rangeBound {
	if union {
		if !a.set || !b.set {
			return rangeBound{}
		}
	} else if !a.set {
		return b
	} else if !b.set {
		return a
	}
	cmp, err := typ.Compare(ctx, a.key, b.key)
	if err != nil {
		if union {
			return rangeBound{}
		}
		return a
	}
	if cmp == 0 {
		// 同じ値なら、OR では含む方、AND では含まない方。
		if a.incl == union {
			return a
		}
		return b
	}
	// 下端は AND なら大きい方、OR なら小さい方。上端はその逆。
	if (cmp > 0) == (lower != union) {
		return a
	}
	return b
}

func columnAndLiteral(a, b sql.Expression) (*expression.GetField, *expression.Literal) {
	gf, ok1 := a.(*expression.GetField)
	lit, ok2 := b.(*expression.Literal)
	if ok1 && ok2 {
		return gf, lit
	}
	return nil, nil
}

// bestIndex は、先頭の k 列が fixed で固定され、その次の列が sortCols と一致するインデックスのうち、k が最大のものを返す。
func bestIndex(ctx *sql.Context, table *Table, fixed map[string]any, sortCols []string) (*Index, int) {
	indexes, err := table.GetIndexes(ctx)
	if err != nil {
		return nil, 0
	}
	var best *Index
	bestK := 0
	for _, i := range indexes {
		idx := i.(*Index)
		if idx.fulltext {
			continue
		}
		k := 0
		for k < len(idx.columns) {
			if _, ok := fixed[strings.ToLower(idx.columns[k].Name)]; !ok {
				break
			}
			k++
		}
		if k == 0 || k+len(sortCols) > len(idx.columns) {
			continue
		}
		match := true
		for j, col := range sortCols {
			c := idx.columns[k+j]
			if !strings.EqualFold(c.Name, col) || c.Length > 0 {
				match = false
				break
			}
		}
		if match && k > bestK {
			best, bestK = idx, k
		}
	}
	return best, bestK
}

// narrowerIndexFactor は、narrowerIndex が並べ替えのインデックスをやめる、読む行の数の差の倍率。差がはっきりしている
// ときだけ替える（境目の近くでは MySQL の選び方と食い違い、同順位の行の並びが変わりうる）。
const narrowerIndexFactor = 2

// narrowerIndex は、並べ替えのインデックス sortIdx（先頭の k 列が等号で決まる）を順に読むより、別のインデックスの
// 等号で決まる行（先頭の列から fixed で決まる分）を引いて並べ替えるほうが、統計の見積もりで narrowerIndexFactor 倍
// 以上少なく読むとき、そのインデックスと、等号で決まる先頭の列の数を返す。LIMIT n（SQL_CALC_FOUND_ROWS でない）なら、
// 並べ替えのインデックスは、条件に当てはまる行が n 行見つかるところまで読むとみる。
func narrowerIndex(ctx *sql.Context, table *Table, fixed map[string]any, sortIdx *Index, k int, limit sql.Expression, foundRows bool) (*Index, int, bool) {
	sortRows, ok := prefixRows(ctx, table, sortIdx, k, fixed)
	if !ok {
		return nil, 0, false
	}
	indexes, err := table.GetIndexes(ctx)
	if err != nil {
		return nil, 0, false
	}
	var best *Index
	bestK, bestRows := 0, 0.0
	for _, i := range indexes {
		idx := i.(*Index)
		if idx.ID() == sortIdx.ID() || idx.fulltext { // GetIndexes は呼ぶたびに作るので、名前で比べる
			continue
		}
		n := 0
		for n < len(idx.columns) && idx.columns[n].Length == 0 {
			if _, ok := fixed[strings.ToLower(idx.columns[n].Name)]; !ok {
				break
			}
			n++
		}
		if n == 0 {
			continue
		}
		rows, ok := prefixRows(ctx, table, idx, n, fixed)
		if ok && (best == nil || rows < bestRows) {
			best, bestK, bestRows = idx, n, rows
		}
	}
	if best == nil {
		return nil, 0, false
	}
	if !cheaperThanSortIndex(ctx, sortRows, bestRows, limit, foundRows) {
		return nil, 0, false
	}
	return best, bestK, true
}

// cheaperThanSortIndex は、rows 行を引いて並べ替えるほうが、並べ替えのインデックスで sortRows 行の範囲を順に読むより
// narrowerIndexFactor 倍以上少なく読むか。LIMIT n（SQL_CALC_FOUND_ROWS でない）なら、並べ替えのインデックスは、条件に
// 当てはまる行が n 行見つかるところまで読むとみる。
func cheaperThanSortIndex(ctx *sql.Context, sortRows, rows float64, limit sql.Expression, foundRows bool) bool {
	read := sortRows
	if l, ok := constInt(ctx, limit); ok && !foundRows && rows > 0 {
		read = math.Min(sortRows, float64(l)*sortRows/rows)
	}
	return math.Max(rows, 1)*narrowerIndexFactor <= read
}

// pointListRows は、access が一意のインデックス（主キーを含む）のすべての列を点で決める範囲だけで読むなら、その点の
// 数（読む行の上限）を返す。post__in の ID IN (...)。
func pointListRows(ctx *sql.Context, access *plan.IndexedTableAccess) (float64, bool) {
	if access == nil {
		return 0, false
	}
	lookup, err := access.GetLookup(ctx, nil)
	if err != nil {
		return 0, false
	}
	idx, ok := lookup.Index.(*Index)
	ranges, ok2 := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok || !ok2 || !idx.IsUnique() || idx.fulltext || len(ranges) == 0 {
		return 0, false
	}
	for _, r := range ranges {
		if len(r) != len(idx.columns) {
			return 0, false
		}
		for i, ce := range r {
			if idx.columns[i].Length > 0 || !isPoint(ctx, ce) {
				return 0, false
			}
		}
	}
	return float64(len(ranges)), true
}

// prefixRows は、インデックス idx の先頭の n 列が fixed の値に決まる行の数を、統計から見積もる。統計のヒストグラムは
// 先頭の列だけなので（buildStats）、先頭の列の値で決まる行の数（2 列目からの等号は数えない多めの見積もり）になる。
func prefixRows(ctx *sql.Context, table *Table, idx *Index, n int, fixed map[string]any) (float64, bool) {
	statistics, err := rowCounts.stats(ctx, table)
	if err != nil {
		return 0, false
	}
	for _, s := range statistics {
		if !strings.EqualFold(s.Qualifier().Index(), idx.ID()) {
			continue
		}
		n = min(n, len(s.Types()))
		if s.RowCount() == 0 || n == 0 {
			return 0, false
		}
		key := make([]any, n)
		for i := range key {
			key[i] = fixed[strings.ToLower(idx.columns[i].Name)]
		}
		buckets, err := stats.PrefixKey(ctx, s.Histogram(), s.Types()[:n], key)
		if err != nil {
			return 0, false
		}
		rows, _, _ := stats.GetNewCounts(buckets)
		return float64(rows), true
	}
	return 0, false
}

// pointAccess は、インデックス idx の先頭の n 列を fixed の値に決め、残りの列をすべてにした範囲で leaf を読む
// IndexedTableAccess を返す。
func pointAccess(ctx *sql.Context, table *Table, leaf sql.TableNode, idx *Index, n int, fixed map[string]any) (*plan.IndexedTableAccess, error) {
	rng := make(sql.MySQLRange, len(idx.columns))
	for i, c := range idx.columns {
		typ := table.sch.Schema[table.def.ColumnIndex(c.Name)].Type
		if i < n {
			v := fixed[strings.ToLower(c.Name)]
			rng[i] = sql.ClosedRangeColumnExpr(v, v, typ)
		} else {
			rng[i] = sql.AllRangeColumnExpr(typ)
		}
	}
	lookup := sql.NewIndexLookup(idx, sql.MySQLRangeCollection{rng}, false, false, false, false)
	return plan.NewStaticIndexedAccessForTableNode(ctx, leaf, lookup)
}

// showIndexesWithSubPartId は、SHOW INDEX にプレフィックス長を返させる解析ルールの ID。
const showIndexesWithSubPartId analyzer.RuleId = 10001

// showIndexesWithSubPart は、HeimDB のテーブルに対する SHOW INDEX を showIndexes に置き換える。
// go-mysql-server の SHOW INDEX は Sub_part（プレフィックス長）を常に NULL で返す（sql/rowexec/show_iters.go）。
// WordPress の dbDelta は Sub_part で既存のインデックスの定義を組み立てて比べるので、meta_key(191) のような
// インデックスを別物と判定してしまう。
func showIndexesWithSubPart(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		si, ok := node.(*plan.ShowIndexes)
		if !ok {
			return node, transform.SameTree, nil
		}
		rt, ok := si.Child.(*plan.ResolvedTable)
		if !ok {
			return node, transform.SameTree, nil
		}
		if _, ok := rt.UnderlyingTable().(*Table); !ok {
			return node, transform.SameTree, nil
		}
		return &showIndexes{ShowIndexes: si}, transform.NewTree, nil
	})
}

// showIndexes は、Sub_part を返す SHOW INDEX。行の形は go-mysql-server の showIndexesIter と同じ。
type showIndexes struct {
	*plan.ShowIndexes
}

var _ sql.ExecSourceRel = (*showIndexes)(nil)

func (n *showIndexes) WithChildren(children ...sql.Node) (sql.Node, error) {
	si, err := n.ShowIndexes.WithChildren(children...)
	if err != nil {
		return nil, err
	}
	return &showIndexes{ShowIndexes: si.(*plan.ShowIndexes)}, nil
}

func (n *showIndexes) String() string { return "heimdb." + n.ShowIndexes.String() }

// RowIter は sql.ExecSourceRel の実装。
func (n *showIndexes) RowIter(ctx *sql.Context, _ sql.Row) (sql.RowIter, error) {
	rt := n.Child.(*plan.ResolvedTable)
	var rows []sql.Row
	for _, idx := range n.IndexesToShow {
		prefixes := idx.PrefixLengths()
		for pos, expr := range idx.Expressions() {
			var columnName, expression any = nil, expr
			nullable := ""
			if col := plan.GetColumnFromIndexExpr(expr, rt); col != nil {
				columnName, expression = col.Name, nil
				if col.Nullable {
					nullable = "YES"
				}
			}
			var subPart any
			if pos < len(prefixes) && prefixes[pos] > 0 {
				subPart = int64(prefixes[pos])
			}
			nonUnique := 1
			if idx.IsUnique() {
				nonUnique = 0
			}
			rows = append(rows, sql.NewRow(
				idx.Table(), nonUnique, idx.ID(), pos+1, columnName, nil, int64(0), subPart,
				nil, nullable, idx.IndexType(), idx.Comment(), "", "YES", expression,
			))
		}
	}
	return &rowIter{rows: rows}, nil
}

// pointRangeFraction は、インデックスの範囲 access がどれも先頭の列の等号なら、統計で見積もった、範囲に当てはまる
// 行の割合と、表の行数を返す（sortIndexForLimit と fullScanForWideRange。docs/plan.md の段階 77）。
func pointRangeFraction(ctx *sql.Context, table *Table, access *plan.IndexedTableAccess) (f, total float64, ok bool) {
	lookup, err := access.GetLookup(ctx, nil)
	if err != nil {
		return 0, 0, false
	}
	ranges, ok := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok || len(ranges) == 0 {
		return 0, 0, false
	}
	statistics, err := rowCounts.stats(ctx, table)
	if err != nil {
		return 0, 0, false
	}
	var stat sql.Statistic
	for _, s := range statistics {
		if strings.EqualFold(s.Qualifier().Index(), lookup.Index.ID()) {
			stat = s
		}
	}
	if stat == nil || stat.RowCount() == 0 {
		return 0, 0, false
	}
	var matched uint64
	for _, r := range ranges {
		ce := r[0]
		_, lowerIncl := ce.LowerBound.(sql.Below)
		_, upperIncl := ce.UpperBound.(sql.Above)
		if !lowerIncl || !upperIncl {
			return 0, 0, false
		}
		v := sql.GetMySQLRangeCutKey(ce.LowerBound)
		if c, err := stat.Types()[0].Compare(ctx, v, sql.GetMySQLRangeCutKey(ce.UpperBound)); err != nil || c != 0 {
			return 0, 0, false
		}
		buckets, err := stats.PrefixKey(ctx, stat.Histogram(), stat.Types()[:1], []any{v})
		if err != nil {
			return 0, 0, false
		}
		rows, _, _ := stats.GetNewCounts(buckets)
		matched += rows
	}
	total = float64(stat.RowCount())
	return float64(matched) / total, total, true
}
