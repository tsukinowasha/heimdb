package backend

import (
	"slices"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// dropRangeFiltersId は、インデックスの範囲がそのまま表す条件を Filter から外す解析ルールの ID。
const dropRangeFiltersId analyzer.RuleId = 10014

// dropRangeFilters は、インデックスで読む表の上の Filter から、インデックスの範囲がそのまま表す条件を外す（docs/plan.md の
// 段階 50）。HeimDB は PreciseMatch を false にしているので、go-mysql-server はインデックスで絞った後にも条件をすべて
// 行に掛ける。WordPress のカテゴリの term 数（post_status IN ('publish') AND post_type IN ('post')）では、範囲で読んだ
// 記事のすべての行で、同じ文字列の比較をもう一度していた。
//
// 外すのは、AND の項のうち「列 = 定数」と「列 IN (定数, ...)」で、次をすべて満たすものだけ。
//   - 列はインデックスの k 番目の列。プレフィックス長があってもよい（段階 83。プレフィックス長のあるインデックスは
//     区間がちょうどにならないので、lookupRows が読んだ行に範囲の条件を本体の値で掛け直す。wp_postmeta の
//     meta_key(191) で引く meta_query は、同じ meta_key の比較を範囲と Filter で 2 回していた）。
//   - どの範囲でも、先頭から k 番目までの列が 1 つの値（点）で、k 番目の値が、項の定数のどれかと列の型で等しい。
//   - 定数は、文字列の列なら同じ文字セットの文字列（columnLiteral と同じ。COLLATE や _binary の付いた定数は除く）、
//     整数の列なら列の型にそのまま入る整数。NULL は除く。
//
// HeimDB がインデックスで返す行は、範囲の中にある（キーの区間がちょうどならキーで、そうでなければ範囲の条件で
// 絞り直す。lookupRows）。その行の k 番目の列は、範囲の点の値と列の型で等しく、文字列の列と定数の比較は列の照合
// 順序で行う（MySQL も同じ）ので、外した項はどの行でも真になる。結果も並び順も変わらない。
func dropRangeFilters(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
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
		if err != nil || lookup.IsEmptyRange {
			return node, transform.SameTree, nil
		}
		idx, ok := lookup.Index.(*Index)
		ranges, ok2 := lookup.Ranges.(sql.MySQLRangeCollection)
		if !ok || !ok2 || idx.fulltext || len(ranges) == 0 {
			return node, transform.SameTree, nil
		}
		conds := expression.SplitConjunction(filter.Expression)
		kept := conds[:0:0]
		for _, c := range conds {
			if !coveredByRanges(ctx, table, idx, name, ranges, c) {
				kept = append(kept, c)
			}
		}
		if len(kept) == len(conds) {
			return node, transform.SameTree, nil
		}
		if len(kept) == 0 {
			return filter.Child, transform.NewTree, nil
		}
		return plan.NewFilter(expression.JoinAnd(kept...), filter.Child), transform.NewTree, nil
	})
}

// coveredByRanges は、条件の項 c が、インデックス idx の範囲 ranges で読んだどの行でも真になるか（dropRangeFilters）。
func coveredByRanges(ctx *sql.Context, table *Table, idx *Index, name string, ranges sql.MySQLRangeCollection, c sql.Expression) bool {
	var col sql.Expression
	var lits []sql.Expression
	switch e := c.(type) {
	case *expression.Equals:
		if _, ok := e.Left().(*expression.GetField); ok {
			col, lits = e.Left(), []sql.Expression{e.Right()}
		} else {
			col, lits = e.Right(), []sql.Expression{e.Left()}
		}
	case *expression.InTuple, *expression.HashInTuple:
		ch := c.Children()
		t, ok := ch[1].(expression.Tuple)
		if !ok {
			return false
		}
		col, lits = ch[0], t
	default:
		return false
	}
	gf, ok := col.(*expression.GetField)
	if !ok || !strings.EqualFold(gf.Table(), name) {
		return false
	}
	k := -1
	for i, ic := range idx.columns {
		if strings.EqualFold(ic.Name, gf.Name()) {
			k = i
			break
		}
	}
	if k < 0 {
		return false
	}
	ord := table.def.ColumnIndex(gf.Name())
	if ord < 0 {
		return false
	}
	typ := table.sch.Schema[ord].Type
	// 項の定数を、列の型の値にする。
	var vals []any
	for _, l := range lits {
		v, ok := exactLiteral(ctx, gf, typ, l)
		if !ok {
			return false
		}
		vals = append(vals, v)
	}
	// 値を列の型で並べ、範囲ごとに二分探索で引く（2 万個の IN の範囲を値ごとに比べると、解析に秒単位かかった。段階 111）
	var cmpErr error
	slices.SortFunc(vals, func(a, b any) int {
		c, err := typ.Compare(ctx, a, b)
		if err != nil {
			cmpErr = err
		}
		return c
	})
	if cmpErr != nil {
		return false
	}
	for _, r := range ranges {
		if len(r) <= k {
			return false
		}
		for i := 0; i <= k; i++ {
			if !isPoint(ctx, r[i]) {
				return false
			}
		}
		p := sql.GetMySQLRangeCutKey(r[k].LowerBound)
		if _, found := slices.BinarySearchFunc(vals, p, func(v, p any) int {
			c, err := typ.Compare(ctx, v, p)
			if err != nil {
				cmpErr = err
			}
			return c
		}); !found || cmpErr != nil {
			return false
		}
	}
	return true
}

// exactLiteral は、列 gf（型 typ）と比べる定数 l を、比較と同じ意味の列の型の値にする。文字列の列は同じ文字セットの
// 文字列の定数、整数の列は列の型にそのまま入る整数の定数だけ。
func exactLiteral(ctx *sql.Context, gf *expression.GetField, typ sql.Type, l sql.Expression) (any, bool) {
	lit, ok := l.(*expression.Literal)
	if !ok || lit.Value() == nil {
		return nil, false
	}
	switch {
	case types.IsTextOnly(typ):
		if _, typed, ok := columnLiteral(gf, lit); ok {
			return typed.(*expression.Literal).Value(), true
		}
		return nil, false
	case types.IsInteger(typ):
		if !types.IsInteger(lit.Type()) {
			return nil, false
		}
		return convertKey(ctx, typ, lit.Value())
	}
	return nil, false
}
