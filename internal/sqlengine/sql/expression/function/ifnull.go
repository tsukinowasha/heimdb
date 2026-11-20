// Copyright 2020-2021 Dolthub, Inc.
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
// Modified by Tsukinowa Inc.: 結果が文字列の IFNULL の日時と ENUM の値を文字列にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時、ENUM、SET と数か文字列、数と文字列の IFNULL を文字列にする（段階 116 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時の枝の結果の型を、MySQL と同じく枝の中で最も多い小数の桁の DATETIME にする（types.TemporalBranchType。段階 116 の 4 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の型を、MySQL と同じ小数の桁にする（段階 120。docs/engine.md）

package function

import (
	"fmt"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// IfNull function returns the specified value IF the expression is NULL, otherwise return the expression.
type IfNull struct {
	expression.BinaryExpressionStub
}

var _ sql.FunctionExpression = (*IfNull)(nil)
var _ sql.CollationCoercible = (*IfNull)(nil)

// NewIfNull returns a new IFNULL UDF
func NewIfNull(ex, value sql.Expression) sql.Expression {
	return &IfNull{
		expression.BinaryExpressionStub{
			LeftChild:  ex,
			RightChild: value,
		},
	}
}

// FunctionName implements sql.FunctionExpression
func (f *IfNull) FunctionName() string {
	return "ifnull"
}

// Description implements sql.FunctionExpression
func (f *IfNull) Description() string {
	return "if expr1 is not NULL, it returns expr1; otherwise it returns expr2."
}

// Eval implements the Expression interface.
func (f *IfNull) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	left, err := f.LeftChild.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if left != nil {
		// HeimDB: with a string result, a date's or ENUM's value is its text (IFNULL(d, ''); stage 116 review)
		if types.IsText(f.Type()) && !types.IsText(f.LeftChild.Type()) {
			if str, ok := types.ValueString(f.LeftChild.Type(), left); ok {
				return str, nil
			}
		}
		return left, nil
	}

	right, err := f.RightChild.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	// HeimDB: with a string result, a date's or ENUM's value is its text (IFNULL(s, d), IFNULL(s, e); stage 116)
	if right != nil && types.IsText(f.Type()) {
		if str, ok := types.ValueString(f.RightChild.Type(), right); ok {
			return str, nil
		}
	}
	return right, nil
}

// Type implements the Expression interface.
func (f *IfNull) Type() sql.Type {
	if types.IsNull(f.LeftChild) {
		if types.IsNull(f.RightChild) {
			return types.Null
		}
		return f.RightChild.Type()
	}
	// HeimDB: a date, TIME, ENUM or SET with a number or a string, and a number with
	// a string, is a string, as in MySQL and COALESCE (IFNULL(d, '') failed with
	// 1105, IFNULL(dd, 0) showed 0000-00-00, IFNULL(i, '') 0; stage 116 review)
	l, r := f.LeftChild.Type(), f.RightChild.Type()
	if !types.IsNull(f.RightChild) && !l.Equals(r) {
		special := func(t sql.Type) bool {
			return types.IsTime(t) || types.IsTimespan(t) || types.IsEnum(t) || types.IsSet(t)
		}
		if expression.MixedBranchTypesAreText(l, r) || (special(l) && types.IsText(r)) || (special(r) && types.IsText(l)) ||
			(types.IsNumber(l) && types.IsText(r)) {
			return collatedText(types.LongText, f.LeftChild, f.RightChild)
		}
	}
	// HeimDB: dates and datetimes are DATETIME of the largest fractional digits of the
	// two, as in MySQL (IFNULL(d0, d3) was DATETIME; stage 116, fourth review)
	if !types.IsNull(f.RightChild) {
		if t, ok := types.TemporalBranchType(l, r); ok {
			return t
		}
		// HeimDB: FLOAT(M,D) と DOUBLE(M,D) は、MySQL と同じく 2 つの小数の桁の最大（段階 120）
		if types.IsFloat(l) || types.IsFloat(r) {
			if t, ok := expression.RealBranchType(l, r); ok {
				return t
			}
		}
	}
	return l
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (f *IfNull) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	if types.IsNull(f.LeftChild) {
		if types.IsNull(f.RightChild) {
			return sql.Collation_binary, 6
		}
		return sql.GetCoercibility(ctx, f.RightChild)
	}
	return sql.GetCoercibility(ctx, f.LeftChild)
}

// IsNullable implements the Expression interface.
func (f *IfNull) IsNullable() bool {
	if types.IsNull(f.LeftChild) {
		if types.IsNull(f.RightChild) {
			return true
		}
		return f.RightChild.IsNullable()
	}
	return f.LeftChild.IsNullable()
}

func (f *IfNull) String() string {
	return fmt.Sprintf("ifnull(%s, %s)", f.LeftChild, f.RightChild)
}

// WithChildren implements the Expression interface.
func (f *IfNull) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(f, len(children), 2)
	}
	return NewIfNull(children[0], children[1]), nil
}
