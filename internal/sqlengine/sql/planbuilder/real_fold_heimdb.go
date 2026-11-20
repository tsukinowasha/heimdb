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

// HeimDB が足したファイル（段階 120 の 2 回目のレビュー。docs/engine.md）。

package planbuilder

import (
	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// foldRealConstants は、WHERE の条件の、FLOAT(M,D) の列と定数のリテラルの直接の比べ（=、<=>、<、<=、>、>=、その NOT と、
// 1 つだけの値の IN）で、MySQL と同じくリテラルを列の単精度の値にする（MySQL は WHERE の定数を列の型に入れ直して比べる。
// FLOAT(10,7) の h = 35.6812363 と h = 35.6812363e0 は真）。AND、OR、NOT の中まで見る。BETWEEN、CASE、IF、SELECT の値、
// 定数の式（CONCAT など）、2 つ以上の値の IN は MySQL も畳み込まないので、そのまま（倍精度か DECIMAL で比べる）。
func foldRealConstants(e sql.Expression) sql.Expression {
	n, _ := foldReal(e)
	return n
}

// foldReal は foldRealConstants の本体。changed は e を変えたか（式の値は == で比べられないものがある）。
func foldReal(e sql.Expression) (sql.Expression, bool) {
	switch x := e.(type) {
	case *expression.And, *expression.Or, *expression.Not:
		children := e.Children()
		changed := false
		for i, c := range children {
			if f, ok := foldReal(c); ok {
				children[i], changed = f, true
			}
		}
		if !changed {
			return e, false
		}
		if n, err := e.WithChildren(children...); err == nil {
			return n, true
		}
	case *expression.Equals, *expression.NullSafeEquals, *expression.LessThan, *expression.LessThanOrEqual,
		*expression.GreaterThan, *expression.GreaterThanOrEqual:
		children := e.Children()
		l, r := children[0], children[1]
		if lit, ok := foldRealLiteral(l, r); ok {
			r = lit
		} else if lit, ok := foldRealLiteral(r, l); ok {
			l = lit
		} else {
			return e, false
		}
		if n, err := e.WithChildren(l, r); err == nil {
			return n, true
		}
	case *expression.InTuple:
		if tup, ok := x.Right().(expression.Tuple); ok && len(tup) == 1 {
			if lit, ok := foldRealLiteral(x.Left(), tup[0]); ok {
				return expression.NewEquals(x.Left(), lit), true
			}
		}
	}
	return e, false
}

// foldRealLiteral は、col が FLOAT(M,D) の列で other が数にできるリテラルなら、other を単精度にしたリテラルを返す。
func foldRealLiteral(col, other sql.Expression) (sql.Expression, bool) {
	if _, ok := col.(*expression.GetField); !ok {
		return nil, false
	}
	t := col.Type()
	if _, _, declared := types.RealPrecision(t); !declared || t.Type() != sqltypes.Float32 {
		return nil, false
	}
	lit, ok := other.(*expression.Literal)
	if !ok {
		return nil, false
	}
	v := lit.Value()
	if v == nil || !(types.IsNumber(lit.Type()) || types.IsText(lit.Type())) {
		return nil, false
	}
	f, _, err := types.Float64.Convert(nil, v)
	if err != nil {
		return nil, false // 数として読めない文字列は前のまま
	}
	return expression.NewLiteral(float32(f.(float64)), types.Float32), true
}
