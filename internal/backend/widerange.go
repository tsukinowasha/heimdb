package backend

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
)

// fullScanForWideRangeId は、ほぼ全件の範囲のインデックスの読み出しを表の全件の読み出しに戻す解析ルールの ID。
const fullScanForWideRangeId analyzer.RuleId = 10016

// wideRangeFraction は、範囲に当てはまる行の割合がこれ以上なら、表の全件を読む。段階 77 で手元で測ると、2 万行の
// wp_comments の割合 1.0 で、全件の読み出しが 1 行ずつ引くより 3 割速く、行の細い表（4 列）では 0.8 で同じくらい、
// 0.9 で全件の方が速かった。
var wideRangeFraction = 0.9

// fullScanForWideRange は、条件のために go-mysql-server が選んだインデックスの範囲が、表のほぼ全件に当てはまり、
// インデックスが文の使う列を持たない（1 行ずつ本体を引く）なら、表の全件の読み出しに戻す（docs/plan.md の段階 77）。
// WordPress のコメントの数（comment_approved IN ('0', '1') AND user_id = 1）など。1 行ずつ主キーで引くより、表を
// 順に読む方が速い。Filter には元の条件がすべて残っている（Table.PreciseMatch が false）ので、結果は変わらない。
//
// ORDER BY、LIMIT、JOIN、副問い合わせ、書き込みのある文は戻さない（インデックスを選ばないと、ORDER BY の文で
// sortIndexForLimit が動かず、エンジンが統計を見ずに並べ替えの列のインデックスを選ぶ）。範囲の割合は、先頭の列の
// 等号の範囲だけを統計で見積もる（pointRangeFraction）。この時点では文の印（QueryFlags の Sort や Limit）が立って
// いないことがあるので、木を見て決める（plainRead）。
func fullScanForWideRange(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	if !plainRead(n) {
		return n, transform.SameTree, nil
	}
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		f, ok := node.(*plan.Filter)
		if !ok {
			return node, transform.SameTree, nil
		}
		access, ok := f.Child.(*plan.IndexedTableAccess)
		if !ok || !access.IsStatic() || access.Index() == nil {
			return node, transform.SameTree, nil
		}
		var table *Table
		switch u := access.TableNode.UnderlyingTable().(type) {
		case *Table:
			table = u
		case *IndexedTable:
			table = u.Table
		default:
			return node, transform.SameTree, nil
		}
		ki, keyed := table.findKeyIndex(access.Index().ID())
		if !keyed || table.covers(ki) {
			return node, transform.SameTree, nil // 主キーと FULLTEXT（キーを持たない）、エントリだけで読めるインデックス
		}
		if frac, _, ok := pointRangeFraction(ctx, table, access); !ok || frac < wideRangeFraction {
			return node, transform.SameTree, nil
		}
		return plan.NewFilter(f.Expression, access.TableNode), transform.NewTree, nil
	})
}

// plainRead は、文の木に、並べ替え、LIMIT、JOIN、集合演算、書き込み、副問い合わせがないか。
func plainRead(n sql.Node) bool {
	plain := true
	transform.Inspect(n, func(node sql.Node) bool {
		switch node.(type) {
		case *plan.Sort, *plan.TopN, *plan.Limit, *plan.Offset, *plan.JoinNode, *plan.SetOp,
			*plan.InsertInto, *plan.Update, *plan.DeleteFrom:
			plain = false
		}
		return plain
	})
	if plain {
		transform.InspectExpressions(n, func(e sql.Expression) bool {
			if _, ok := e.(*plan.Subquery); ok {
				plain = false
			}
			return plain
		})
	}
	return plain
}
