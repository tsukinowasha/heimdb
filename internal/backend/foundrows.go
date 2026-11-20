package backend

import (
	"fmt"
	"math"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/iters"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// foundRowsWithOffsetId は HeimDB が足す解析ルールの ID。
const foundRowsWithOffsetId analyzer.RuleId = 10008

// foundRowsWithOffset は、SQL_CALC_FOUND_ROWS の Limit(n, Offset(k, 子)) を Offset(k, Limit(n + k, 子)) に組み替える。
//
// go-mysql-server の Limit は、子から受け取った行の数を FOUND_ROWS() にするので、子が Offset だと読み飛ばした k 行が
// 数えられない（5 件のうち LIMIT 2, 2 で FOUND_ROWS() が 3 になる）。WordPress はこの値でページの数を決めるので、
// 2 ページ目以降で総数が合わなかった。組み替えても返る行は同じで、Limit は全件を数える。
func foundRowsWithOffset(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		l, ok := node.(*plan.Limit)
		if !ok || !l.CalcFoundRows {
			return node, transform.SameTree, nil
		}
		off, ok := l.Child.(*plan.Offset)
		if !ok {
			return node, transform.SameTree, nil
		}
		limit := plan.NewLimit(&limitPlusOffset{limit: l.Limit, offset: off.Offset}, off.Child).WithCalcFoundRows(true)
		return plan.NewOffset(off.Offset, limit), transform.NewTree, nil
	})
}

// limitPlusOffset は、LIMIT と OFFSET の値の和（実行時に評価する。プリペアドステートメントの引数でもよい）。
type limitPlusOffset struct {
	limit, offset sql.Expression
}

var _ sql.Expression = (*limitPlusOffset)(nil)

func (e *limitPlusOffset) Resolved() bool             { return e.limit.Resolved() && e.offset.Resolved() }
func (e *limitPlusOffset) String() string             { return fmt.Sprintf("(%s + %s)", e.limit, e.offset) }
func (e *limitPlusOffset) Type() sql.Type             { return types.Int64 }
func (e *limitPlusOffset) IsNullable() bool           { return false }
func (e *limitPlusOffset) Children() []sql.Expression { return []sql.Expression{e.limit, e.offset} }

func (e *limitPlusOffset) Eval(ctx *sql.Context, _ sql.Row) (any, error) {
	limit, err := iters.GetInt64Value(ctx, e.limit)
	if err != nil {
		return nil, err
	}
	offset, err := iters.GetInt64Value(ctx, e.offset)
	if err != nil {
		return nil, err
	}
	// 「offset から最後まで」の LIMIT（18446744073709551615。go-mysql-server は MaxInt64 に丸める）でも溢れないように、
	// 上限で止める。
	if limit > math.MaxInt64-offset {
		return int64(math.MaxInt64), nil
	}
	return limit + offset, nil
}

func (e *limitPlusOffset) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 2)
	}
	return &limitPlusOffset{limit: children[0], offset: children[1]}, nil
}
