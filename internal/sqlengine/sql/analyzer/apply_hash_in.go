// Copyright 2021 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 文字列と数の混ざる IN をハッシュにしない（docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL と文字列、浮動小数点数、桁の少ない DECIMAL の IN をハッシュにしない（上流の #4054 の取り込みで。docs/engine.md）

package analyzer

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func applyHashIn(ctx *sql.Context, a *Analyzer, n sql.Node, scope *plan.Scope, sel RuleSelector, qFlags *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		filter, ok := node.(*plan.Filter)
		if !ok {
			return node, transform.SameTree, nil
		}

		e, same, err := transform.Expr(filter.Expression, func(expr sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
			if e, ok := expr.(*expression.InTuple); ok &&
				hasSingleOutput(e.Left()) &&
				isStatic(e.Right()) &&
				!textInNumbers(e) {
				newe, err := expression.NewHashInTuple(ctx, e.Left(), e.Right())
				if err != nil {
					return nil, transform.SameTree, err
				}
				return newe, transform.NewTree, nil
			}
			return expr, transform.SameTree, nil
		})
		if err != nil {
			return nil, transform.SameTree, err
		}
		if same {
			return node, transform.SameTree, nil
		}
		ret, err := filter.WithExpressions(e)
		if err != nil {
			return node, transform.SameTree, nil
		}
		return ret, transform.NewTree, err
	})
}

// hasSingleOutput checks if an expression evaluates to a single output
func hasSingleOutput(e sql.Expression) bool {
	return !transform.InspectExpr(e, func(expr sql.Expression) bool {
		switch expr.(type) {
		case expression.Tuple, *expression.Literal, *expression.GetField,
			expression.Comparer, *expression.Convert, sql.FunctionExpression,
			*expression.IsTrue, *expression.IsNull, expression.ArithmeticOp:
			return false
		default:
			return true
		}
		return false
	})
}

// isStatic checks if an expression is static
func isStatic(e sql.Expression) bool {
	return !transform.InspectExpr(e, func(expr sql.Expression) bool {
		switch expr.(type) {
		case expression.Tuple, *expression.Literal:
			return false
		default:
			return true
		}
	})
}

// textInNumbers は、IN の左が文字列で、右に数があるか（HeimDB）。値の組ごとに数として比べる（InTuple.Eval）ので、
// 左の型に揃えて値をハッシュにする HashInTuple にはしない。左が DECIMAL で、右に文字列、浮動小数点数か、左より
// 小数の桁の少ない DECIMAL があるときも同じ（文字列と浮動小数点数は DOUBLE で比べ、桁の少ない型に揃えると左が
// 丸められる。段階 115）。
func textInNumbers(e *expression.InTuple) bool {
	tup, ok := e.Right().(expression.Tuple)
	if !ok {
		return false
	}
	if lt, ok := e.Left().Type().(sql.DecimalType); ok {
		for _, el := range tup {
			et := el.Type()
			if types.IsInteger(et) || et == types.Null {
				continue
			}
			if dt, ok := et.(sql.DecimalType); ok && dt.Scale() >= lt.Scale() {
				continue
			}
			return true
		}
		return false
	}
	if !types.IsTextOnly(e.Left().Type()) {
		return false
	}
	for _, el := range tup {
		if types.IsNumber(el.Type()) {
			return true
		}
	}
	return false
}
