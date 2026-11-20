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

// HeimDB が足したファイル（段階 120 のレビュー。docs/engine.md）。

package expression

import (
	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// RealSetOpType は、UNION の両側の型 l と r の列の、MySQL と同じ FLOAT か DOUBLE の型を返す（段階 120 のレビュー）。
// 両側が FLOAT か DOUBLE か、片側が小数の桁の決まった FLOAT(M,D) か DOUBLE(M,D) で他方が整数か DECIMAL のときだけ ok。
// 両側が FLOAT なら FLOAT、ほかは DOUBLE で、小数の桁は両側が決まっていればその最大（MySQL の CREATE TABLE ... SELECT の
// double(17,6)、double(13,2)）。
func RealSetOpType(l, r sql.Type) (sql.Type, bool) {
	if !types.IsNumber(l) || !types.IsNumber(r) {
		return nil, false
	}
	if !(types.IsFloat(l) && types.IsFloat(r)) {
		_, lfixed := types.RealScale(l)
		_, rfixed := types.RealScale(r)
		if !lfixed && !rfixed {
			return nil, false
		}
	}
	t := types.RealResultOf(0, l, r)
	if l.Type() == sqltypes.Float32 && r.Type() == sqltypes.Float32 {
		if scale, ok := types.RealScale(t); ok {
			return types.RealExprType(sqltypes.Float32, scale), true
		}
		return types.Float32, true
	}
	return t, true
}

// RealResult は、子の値をそのまま返し、型を typ とする式（UNION の FLOAT と DOUBLE の列。段階 120 のレビュー）。
type RealResult struct {
	UnaryExpression
	typ sql.Type
}

var _ sql.Expression = (*RealResult)(nil)
var _ sql.CollationCoercible = (*RealResult)(nil)

// NewRealResult は、child を型 typ の値として返す式を作る。値は FLOAT なら float32、DOUBLE なら float64 にする。
func NewRealResult(child sql.Expression, typ sql.Type) *RealResult {
	return &RealResult{UnaryExpression: UnaryExpression{Child: child}, typ: typ}
}

func (e *RealResult) Type() sql.Type { return e.typ }

func (e *RealResult) IsNullable() bool { return e.Child.IsNullable() }

func (e *RealResult) String() string { return e.Child.String() }

func (e *RealResult) CollationCoercibility(*sql.Context) (sql.CollationID, byte) {
	return sql.Collation_binary, 5
}

func (e *RealResult) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	v, err := e.Child.Eval(ctx, row)
	if err != nil || v == nil {
		return v, err
	}
	if e.typ.Type() == sqltypes.Float32 {
		if f, ok := v.(float32); ok {
			return f, nil
		}
		f, _, err := types.Float32.Convert(ctx, v)
		return f, err
	}
	f, _, err := types.Float64.Convert(ctx, v)
	return f, err
}

func (e *RealResult) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 1)
	}
	return NewRealResult(children[0], e.typ), nil
}
