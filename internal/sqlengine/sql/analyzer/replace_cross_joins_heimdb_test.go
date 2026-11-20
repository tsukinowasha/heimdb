// Copyright 2026 Tsukinowa Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modified by Tsukinowa Inc.: 列と、もう一方の表の列の式との等号を結合の条件に移すことのテスト（docs/engine.md）

package analyzer

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression/function"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 等号の片側が一方の表の列で、もう片側が他方の表の列だけの式なら、カンマで並べた表（CrossJoin）を、その等号を条件に
// した InnerJoin にする（WordPress の期限切れの transient を消す DELETE の形）。両方の表の列が混ざる式、評価する
// たびに値の変わる式、副問い合わせ、定数だけの辺、3 つ目の表の列を含む式は移さない。
func TestConvertCrossJoinExpressionEquality(t *testing.T) {
	db := memory.NewDatabase("db")
	pro := memory.NewDBProvider(db)
	ctx := newContext(pro)
	sch := func(src string) sql.PrimaryKeySchema {
		return sql.NewPrimaryKeySchema(sql.Schema{
			{Name: "k", Type: types.LongText, Source: src},
			{Name: "v", Type: types.LongText, Source: src},
		})
	}
	tbl := memory.NewTable(db, "t", sch("t"), nil)
	alias := func(name string, id sql.TableId) sql.Node {
		rt := plan.NewResolvedTable(tbl, nil, nil).WithId(id).(*plan.ResolvedTable)
		return plan.NewTableAlias(name, rt).WithId(id).(sql.Node)
	}
	col := func(id int, table, name string) *expression.GetField {
		return expression.NewGetFieldWithTable(0, id, types.LongText, "db", table, name, false)
	}
	must := func(e sql.Expression, err error) sql.Expression {
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	ak, bk, bv, ck := col(1, "a", "k"), col(2, "b", "k"), col(2, "b", "v"), col(3, "c", "k")
	timeout := func(args ...sql.Expression) sql.Expression {
		return must(function.NewConcat(append([]sql.Expression{expression.NewLiteral("_transient_timeout_", types.LongText)}, args...)...))
	}
	sub := must(function.NewSubstring(ak, expression.NewLiteral(12, types.Int64)))
	run := func(cond sql.Expression, cross sql.Node) sql.Node {
		t.Helper()
		out, _, err := replaceCrossJoins(ctx, nil, plan.NewFilter(cond, cross), nil, DefaultRuleSelector, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	hasInner := func(n sql.Node) bool {
		found := false
		transform.Inspect(n, func(n sql.Node) bool {
			if j, ok := n.(*plan.JoinNode); ok && j.Op == plan.JoinTypeInner {
				found = true
			}
			return true
		})
		return found
	}
	ab := plan.NewCrossJoin(alias("a", 1), alias("b", 2))
	for _, c := range []struct {
		name  string
		cond  sql.Expression
		moved bool
	}{
		{"column = expression of the other table", expression.NewEquals(bk, timeout(sub)), true},
		{"expression = column", expression.NewEquals(timeout(sub), bk), true},
		{"null-safe equality", expression.NewNullSafeEquals(bk, timeout(sub)), true},
		{"expression mixing both tables", expression.NewEquals(bk, timeout(sub, bv)), false},
		{"non-deterministic expression", expression.NewEquals(bk, timeout(must(function.NewRand()))), false},
		{"constant side", expression.NewEquals(bk, timeout()), false},
		{"inequality with an expression", expression.NewGreaterThan(bk, timeout(sub)), false},
		{"subquery", expression.NewEquals(bk, plan.NewSubquery(plan.NewResolvedTable(tbl, nil, nil), "select k from t")), false},
	} {
		if got := hasInner(run(c.cond, ab)); got != c.moved {
			t.Errorf("%s: moved into an inner join = %v, want %v", c.name, got, c.moved)
		}
	}
	// 3 つ目の表の列を含む式は、内側の結合（a, b）にも外側の結合（(a, b), c）にも移さない
	abc := plan.NewCrossJoin(plan.NewCrossJoin(alias("a", 1), alias("b", 2)), alias("c", 3))
	if hasInner(run(expression.NewEquals(ak, timeout(bk, ck)), abc)) {
		t.Error("an expression over two other tables was moved")
	}
}
