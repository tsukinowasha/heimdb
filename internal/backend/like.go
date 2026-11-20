package backend

import (
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// likeExplicitEscapeId は HeimDB が足す解析ルールの ID。
const likeExplicitEscapeId analyzer.RuleId = 10005

// likeExplicitEscape は、エスケープ（\）を含みうる定数のパターンの LIKE に、既定と同じ ESCAPE '\' を明示する。
//
// go-mysql-server の simplifyFilters（sql/analyzer/optimization_rules.go）は、ワイルドカードのない LIKE を等号に、
// 末尾だけが % の LIKE を範囲に置き換えるが、\_ や \% のエスケープを外さずに比べる。そのため WordPress の
// $wpdb->esc_like を使ったパターン（option_name LIKE '\_transient\_%'、SHOW TABLES LIKE 'wp\_foo' など）が
// 何にも一致しない。simplifyFilters は ESCAPE を明示した LIKE を置き換えないので、その前に ESCAPE を付ける。
//
// simplifyFilters は派生テーブル、WITH、UNION（不透明なノード）とサブクエリの中にも入り、CONCAT などの定数の式を
// 畳んでから置き換えるので、ここでも同じところまで入り、列を参照しない式のパターンには ESCAPE を付ける（値は
// まだ分からないため。ESCAPE '\' は既定と同じなので、結果は変わらない）。
func likeExplicitEscape(ctx *sql.Context, a *analyzer.Analyzer, n sql.Node, scope *plan.Scope, sel analyzer.RuleSelector, qFlags *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.NodeExprsWithOpaque(n, func(e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
		switch e := e.(type) {
		case *plan.Subquery:
			q, same, err := likeExplicitEscape(ctx, a, e.Query, scope, sel, qFlags)
			if err != nil || same {
				return e, transform.SameTree, err
			}
			return e.WithQuery(q), transform.NewTree, nil
		case *expression.Like:
			pattern, changed, err := withTrailingEscape(ctx, e)
			if err != nil {
				return nil, transform.SameTree, err
			}
			if e.Escape != nil {
				if !changed {
					return e, transform.SameTree, nil
				}
				return expression.NewLike(e.LeftChild, pattern, e.Escape), transform.NewTree, nil
			}
			if !changed && !mayContainEscape(e.RightChild) {
				return e, transform.SameTree, nil
			}
			return expression.NewLike(e.LeftChild, pattern, expression.NewLiteral(`\`, types.LongText)), transform.NewTree, nil
		}
		return e, transform.SameTree, nil
	})
}

// mayContainEscape は、LIKE のパターンが、エスケープ（\）を含む定数か、列を参照しない（定数に畳まれうる）式かを返す。
func mayContainEscape(pattern sql.Expression) bool {
	if lit, ok := pattern.(*expression.Literal); ok {
		s, ok := lit.Value().(string)
		return ok && strings.Contains(s, `\`)
	}
	constant := true
	transform.InspectExpr(pattern, func(e sql.Expression) bool {
		switch e.(type) {
		case *expression.GetField, *expression.UnresolvedColumn, *plan.Subquery:
			constant = false
		}
		return !constant
	})
	return constant
}

// withTrailingEscape は、定数のパターンが単独のエスケープ文字で終わるなら、そのエスケープ文字をエスケープしたパターン
// （'x\' なら 'x\\'）を返す。MySQL は末尾の単独のエスケープ文字をその文字そのものとして扱うが、go-mysql-server は
// エラーにする。
func withTrailingEscape(ctx *sql.Context, like *expression.Like) (sql.Expression, bool, error) {
	lit, ok := like.RightChild.(*expression.Literal)
	if !ok {
		return like.RightChild, false, nil
	}
	pattern, ok := lit.Value().(string)
	if !ok {
		return like.RightChild, false, nil
	}
	escape := `\`
	if like.Escape != nil {
		el, ok := like.Escape.(*expression.Literal)
		if !ok {
			return like.RightChild, false, nil
		}
		v, err := el.Eval(ctx, nil)
		s, isStr := v.(string)
		if err != nil || !isStr || len([]rune(s)) != 1 {
			return like.RightChild, false, err
		}
		escape = s
	}
	// 末尾から続くエスケープ文字の数が奇数なら、最後の 1 つは何もエスケープしていない。
	n := 0
	for rest := pattern; strings.HasSuffix(rest, escape); rest = strings.TrimSuffix(rest, escape) {
		n++
	}
	if n%2 == 0 {
		return like.RightChild, false, nil
	}
	return expression.NewLiteral(pattern+escape, lit.Type()), true, nil
}
