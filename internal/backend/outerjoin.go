package backend

import (
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
)

// simplifyOuterJoinsId は、WHERE が右の表の行を必ず求める LEFT JOIN を INNER JOIN にする解析ルールの ID。
const simplifyOuterJoinsId analyzer.RuleId = 10015

// simplifyOuterJoins は、WHERE（Filter）の AND の項に、LEFT JOIN の右の表の列そのものを片方に持つ比較（=、<>、<、<=、
// >、>=、IN）があれば、その LEFT JOIN を INNER JOIN にする（docs/plan.md の段階 51。MySQL の外部結合の単純化と同じ）。
// 右に行がない行では右の列が NULL になり、その比較は NULL で真にならないので、WHERE で必ず落ちる。結果は変わらない。
//
// WordPress のタクソノミーの絞り込み（カテゴリ、タグのページ）は wp_posts LEFT JOIN wp_term_relationships ON (...)
// WHERE term_taxonomy_id IN (5) を出す。go-mysql-server は LEFT JOIN の左から読むしかないので、記事の全件を読み、
// 記事ごとに wp_term_relationships を引いていた。INNER JOIN なら、JOIN の順を決めるときに、記事の少ないカテゴリでは
// term_taxonomy_id のインデックスから読める。
//
// INNER JOIN にした表の列の、値が 1 つの 列 IN (定数) は、列 = 定数 にする（MySQL では同じ意味）。等号なら、JOIN で
// 表を引くときのキーに定数が入る（記事の多いカテゴリの term_taxonomy_id IN (1) で、wp_term_relationships を主キーの
// 1 点で引く）。ほかの表の IN は替えない（1 件の記事の term の読み込みの tr.object_id IN (5003) を等号にすると、
// go-mysql-server が term_taxonomy の全件から引く順を選び、遅くなった）。
//
// 右が 1 つの表（別名も）の LEFT JOIN だけを替える。IS NULL、<=>、IFNULL、COALESCE、OR、NOT の中の項は、NULL でも
// 真になりうるので見ない。HAVING は Filter ではないので見ない。JOIN の順を決める前（解析の前）に置く。
func simplifyOuterJoins(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.NodeWithOpaque(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		f, ok := node.(*plan.Filter)
		if !ok {
			return node, transform.SameTree, nil
		}
		if _, ok := f.Child.(*plan.JoinNode); !ok {
			return node, transform.SameTree, nil
		}
		var rejected []string
		for _, c := range expression.SplitConjunction(f.Expression) {
			rejected = append(rejected, nullRejectedTables(c)...)
		}
		if len(rejected) == 0 {
			return node, transform.SameTree, nil
		}
		var converted []string
		child, same, err := innerJoins(f.Child, rejected, &converted)
		if err != nil || same {
			return node, transform.SameTree, err
		}
		conds := expression.SplitConjunction(f.Expression)
		for i, c := range conds {
			conds[i] = singleInEquals(ctx, c, converted)
		}
		return plan.NewFilter(expression.JoinAnd(conds...), child), transform.NewTree, nil
	})
}

// singleInEquals は、項 c が tables のどれかの表の列の 列 IN (定数) で、定数が 1 つなら、列 = 定数 を返す。列は、
// 文字列だけの型なら同じ文字セットの文字列の定数（columnLiteral と同じ）、整数の型ならその型にそのまま入る整数の
// 定数に限る（go-mysql-server の IN は定数を列の型に変換して比べ、等号は数として比べるので、どちらでも同じになる形
// だけ）。定数は列の型にする（go-mysql-server の等号は、符号つきの定数と符号なしの列を小数に直して比べるので、
// 2^53 を超える値が別の値と等しくなる。同じ型なら列の型で比べる）。
func singleInEquals(ctx *sql.Context, c sql.Expression, tables []string) sql.Expression {
	in, ok := c.(*expression.InTuple)
	if !ok {
		return c
	}
	tup, ok := in.Right().(expression.Tuple)
	if !ok || len(tup) != 1 {
		return c
	}
	gf, ok := in.Left().(*expression.GetField)
	if !ok || !containsFold(tables, gf.Table()) {
		return c
	}
	v, ok := exactLiteral(ctx, gf, gf.Type(), tup[0])
	if !ok {
		return c
	}
	return expression.NewEquals(gf, expression.NewLiteral(v, gf.Type()))
}

// nullRejectedTables は、項 c が比較なら、その両辺のうち列そのものである辺の表の名前（別名）を返す。どの列も、NULL
// なら c は NULL になる。
func nullRejectedTables(c sql.Expression) []string {
	var operands []sql.Expression
	switch e := c.(type) {
	case *expression.Equals, *expression.LessThan, *expression.LessThanOrEqual, *expression.GreaterThan,
		*expression.GreaterThanOrEqual:
		operands = e.Children()
	case *expression.Not:
		// <> は NOT (=) になる。
		if eq, ok := e.Child.(*expression.Equals); ok {
			operands = eq.Children()
		}
	case *expression.InTuple, *expression.HashInTuple:
		operands = c.Children()[:1]
	}
	var tables []string
	for _, o := range operands {
		if gf, ok := o.(*expression.GetField); ok && gf.Table() != "" {
			tables = append(tables, gf.Table())
		}
	}
	return tables
}

// innerJoins は、JOIN の木の中で、右が rejected のどれかの表である LEFT JOIN を INNER JOIN にし、その表の名前を
// converted に足す。JOIN の下だけをたどる（派生テーブルやサブクエリの中の表は、外の WHERE の列にならない）。
func innerJoins(n sql.Node, rejected []string, converted *[]string) (sql.Node, bool, error) {
	j, ok := n.(*plan.JoinNode)
	if !ok {
		return n, true, nil
	}
	left, sameL, err := innerJoins(j.Left(), rejected, converted)
	if err != nil {
		return nil, false, err
	}
	right, sameR, err := innerJoins(j.Right(), rejected, converted)
	if err != nil {
		return nil, false, err
	}
	name, single := tableName(j.Right())
	toInner := j.Op == plan.JoinTypeLeftOuter && single && containsFold(rejected, name)
	if sameL && sameR && !toInner {
		return n, true, nil
	}
	nj := *j
	if toInner {
		nj.Op = plan.JoinTypeInner
		*converted = append(*converted, name)
	}
	out, err := nj.WithChildren(left, right)
	return out, false, err
}

// tableName は、n が 1 つの表（別名も）なら、その名前を返す。
func tableName(n sql.Node) (string, bool) {
	switch t := n.(type) {
	case *plan.TableAlias:
		if _, ok := t.Child.(*plan.ResolvedTable); ok {
			return t.Name(), true
		}
	case *plan.ResolvedTable:
		return t.Name(), true
	}
	return "", false
}

// containsFold は、names に、大文字と小文字を区別せずに name があるか。
func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
