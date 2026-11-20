package backend

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// columnTypedLiteralsId は、列と定数の比較を軽くする解析ルールの ID。
const columnTypedLiteralsId analyzer.RuleId = 10012

// columnTypedLiterals は、WHERE の条件（Filter）のうち、文字列の列と文字列の定数、整数の列と整数の定数の比較を、
// 行ごとの型の変換がいらない形にする（docs/plan.md の段階 28、77）。
//
//   - 列 = '定数'（<>、<、<=、>、>=、<=> も）の定数の型を、列の型にする。go-mysql-server は、両辺の型が違うと行ごとに
//     両方の値を LONGTEXT に変換し、列の照合順序の型を作り直して比べる。列と定数の比較は、MySQL でも列の照合順序で
//     比べる（定数より列の方が強い）ので、結果は変わらない。
//   - 列 = 整数の定数も、定数が列の型にそのまま入るなら、定数の型を列の型にする。go-mysql-server は、符号付きと
//     符号なし（BIGINT UNSIGNED の user_id と 1）のように型が違うと、行ごとに両方を DOUBLE に変換して比べる。整数
//     どうしは MySQL も値で比べるので、結果は変わらない。範囲の外の定数（unsigned の列と -1）は今のまま。
//   - 値が 1 つの 列 IN ('定数') を 列 = '定数' に、列 NOT IN ('定数') を NOT (列 = '定数') にする（行ごとにハッシュを
//     作らない。NULL の扱いも同じ）。WordPress の comment_type NOT IN ('note') など。
//
// 文字列は、列が文字列だけの型（CHAR、VARCHAR、TEXT）、定数が同じ文字セットの文字列の型（_binary や COLLATE の付いた
// 定数、数値は除く）に限る。整数は、列も定数も整数の型（小数、文字列の数、NULL は除く）に限る。
func columnTypedLiterals(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		f, ok := node.(*plan.Filter)
		if !ok {
			return node, transform.SameTree, nil
		}
		cond, same, err := transform.Expr(f.Expression, func(e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
			switch e := e.(type) {
			case *expression.Equals, *expression.NullSafeEquals, *expression.LessThan, *expression.LessThanOrEqual,
				*expression.GreaterThan, *expression.GreaterThanOrEqual:
				return typedComparison(ctx, e)
			case *expression.InTuple:
				return singleIn(ctx, e, e.Left(), e.Right())
			case *expression.HashInTuple:
				return singleIn(ctx, e, e.Left(), e.Right())
			}
			return e, transform.SameTree, nil
		})
		if err != nil || same {
			return node, transform.SameTree, err
		}
		return plan.NewFilter(cond, f.Child), transform.NewTree, nil
	})
}

// typedComparison は、比較 e の片方が列、もう片方が列の型にできる定数なら、定数の型を列の型にした比較を返す。
func typedComparison(ctx *sql.Context, e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
	ch := e.Children()
	if col, lit, ok := typedLiteral(ctx, ch[0], ch[1]); ok {
		ch = []sql.Expression{col, lit}
	} else if col, lit, ok := typedLiteral(ctx, ch[1], ch[0]); ok {
		ch = []sql.Expression{lit, col}
	} else {
		return e, transform.SameTree, nil
	}
	ne, err := e.WithChildren(ch...)
	if err != nil {
		return nil, transform.SameTree, err
	}
	return ne, transform.NewTree, nil
}

// singleIn は、列 IN (定数) の右辺が列の型にできる定数 1 つなら、列 = 定数（定数は列の型）を返す。NOT IN は、
// go-mysql-server が NOT と IN に分けるので、IN を置き換えれば NOT (列 = 定数) になる。
func singleIn(ctx *sql.Context, in, left, right sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
	tup, ok := right.(expression.Tuple)
	if !ok || len(tup) != 1 {
		return in, transform.SameTree, nil
	}
	l, r, ok := typedLiteral(ctx, left, tup[0])
	if !ok {
		return in, transform.SameTree, nil
	}
	return expression.NewEquals(l, r), transform.NewTree, nil
}

// typedLiteral は、col が列で、lit がその型にできる定数（columnLiteral の文字列か、列の型にそのまま入る整数）なら、
// col と、型を列の型にした定数を返す。
func typedLiteral(ctx *sql.Context, col, lit sql.Expression) (sql.Expression, sql.Expression, bool) {
	if c, l, ok := columnLiteral(col, lit); ok {
		return c, l, true
	}
	gf, ok := col.(*expression.GetField)
	if !ok || !types.IsInteger(gf.Type()) {
		return nil, nil, false
	}
	if v, ok := exactLiteral(ctx, gf, gf.Type(), lit); ok {
		return gf, expression.NewLiteral(v, gf.Type()), true
	}
	return nil, nil, false
}

// columnLiteral は、col が文字列だけの型の列で、lit が同じ文字セットの文字列の定数なら、col と、値はそのままで
// 型を列の型にした定数を返す。
func columnLiteral(col, lit sql.Expression) (sql.Expression, sql.Expression, bool) {
	gf, ok := col.(*expression.GetField)
	if !ok {
		return nil, nil, false
	}
	l, ok := lit.(*expression.Literal)
	if !ok {
		return nil, nil, false
	}
	s, ok := l.Value().(string)
	if !ok {
		return nil, nil, false
	}
	ct, lt := gf.Type(), l.Type()
	if !types.IsTextOnly(ct) || !types.IsTextOnly(lt) {
		return nil, nil, false
	}
	cs, ok1 := ct.(sql.StringType)
	ls, ok2 := lt.(sql.StringType)
	if !ok1 || !ok2 || cs.CharacterSet() != ls.CharacterSet() {
		return nil, nil, false
	}
	return gf, expression.NewLiteral(s, ct), true
}
