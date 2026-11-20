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
//
// Modified by Tsukinowa Inc.: 結果の型の組み合わせを IF からも使えるように出した（上流の #3025 の取り込み。docs/engine.md）
// Modified by Tsukinowa Inc.: 結果が文字列の CASE の日付と日時の値を型の形の文字列にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時か ENUM と数の CASE を文字列にし、DECIMAL の枝の型を桁の大きい方にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時の枝の結果の型を、MySQL と同じく枝の中で最も多い小数の桁の DATETIME にする（types.TemporalBranchType。段階 116 の 4 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の枝の型を、MySQL と同じ小数の桁にする（RealBranchType。段階 120。docs/engine.md）

package expression

import (
	"bytes"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// CaseBranch is a single branch of a case expression.
type CaseBranch struct {
	Cond  sql.Expression
	Value sql.Expression
}

// Case is an expression that returns the value of one of its branches when a
// condition is met.
type Case struct {
	Expr     sql.Expression
	Branches []CaseBranch
	Else     sql.Expression
}

var _ sql.Expression = (*Case)(nil)
var _ sql.CollationCoercible = (*Case)(nil)

// NewCase returns an new Case expression.
func NewCase(expr sql.Expression, branches []CaseBranch, elseExpr sql.Expression) *Case {
	return &Case{expr, branches, elseExpr}
}

// From the description of operator typing here:
// https://dev.mysql.com/doc/refman/8.0/en/flow-control-functions.html#operator_case
func combinedCaseBranchType(left, right sql.Type) sql.Type {
	if left == types.Null {
		return right
	}
	if right == types.Null {
		return left
	}
	if MixedBranchTypesAreText(left, right) {
		return types.LongText
	}

	// Our current implementation of StringType.Convert(enum), does not match MySQL's behavior.
	// So, we make sure to return Enums in this particular case.
	// More details: https://github.com/dolthub/dolt/issues/8598
	if types.IsEnum(left) && types.IsEnum(right) {
		return right
	}
	if types.IsSet(left) && types.IsSet(right) {
		return right
	}
	if types.IsTextOnly(left) && types.IsTextOnly(right) {
		return types.LongText
	}
	if types.IsTextBlob(left) && types.IsTextBlob(right) {
		return types.LongBlob
	}
	if types.IsTime(left) && types.IsTime(right) {
		if left == right {
			return left
		}
		// HeimDB: DATETIME of the largest fractional digits of the branches, as in MySQL
		// (stage 116, fourth review)
		if t, ok := types.TemporalBranchType(left, right); ok {
			return t
		}
		return types.DatetimeMaxPrecision
	}
	if types.IsNumber(left) && types.IsNumber(right) {
		// HeimDB: FLOAT(M,D) と DOUBLE(M,D) の枝は、MySQL と同じく枝の小数の桁の最大（段階 120）
		if t, ok := RealBranchType(left, right); ok {
			return t
		}
		if types.IsFloat64(left) || types.IsFloat64(right) {
			return types.Float64
		}
		if types.IsFloat(left) || types.IsFloat(right) {
			return types.Float32
		}
		if types.IsDecimal(left) || types.IsDecimal(right) {
			// HeimDB: the most integer digits and decimals of the branches, as in
			// MySQL (it was DECIMAL(65,10), so SUM(CASE ... THEN 1.5 ELSE 0 END)
			// showed 10 decimals; stage 116)
			if lp, ls, lok := decimalDigits(left); lok {
				if rp, rs, rok := decimalDigits(right); rok {
					scale := max(ls, rs)
					return types.MustCreateDecimalType(min(max(lp-ls, rp-rs)+scale, 65), scale)
				}
			}
			return types.MustCreateDecimalType(65, 10)
		}
		if left == types.Uint64 && types.IsSigned(right) ||
			right == types.Uint64 && types.IsSigned(left) {
			return types.MustCreateDecimalType(65, 10)
		}
		if !types.IsSigned(left) && !types.IsSigned(right) {
			return types.Uint64
		} else {
			return types.Int64
		}
	}
	if types.IsJSON(left) && types.IsJSON(right) {
		return types.JSON
	}
	return types.LongText
}

// RealBranchType は、IF、CASE、COALESCE、IFNULL の枝の型 ts のどれかが小数の桁の決まった FLOAT か DOUBLE で、ほかも
// 桁の決まった数（ほかの FLOAT(M,D) と DOUBLE(M,D)、DECIMAL、整数）なら、MySQL と同じく枝の小数の桁の最大の型を返す
// （DOUBLE の枝があれば DOUBLE、ほかは FLOAT。MySQL の FLOAT(7,2) と 0 の IFNULL は FLOAT。値は丸めない。HeimDB: 段階 120）。
func RealBranchType(ts ...sql.Type) (sql.Type, bool) {
	t := types.RealResultOf(0, ts...)
	scale, ok := types.RealScale(t)
	if !ok {
		return nil, false
	}
	for _, a := range ts {
		if a != nil && types.IsFloat64(a) {
			return t, true
		}
	}
	return types.RealExprType(sqltypes.Float32, scale), true
}

// CombinedBranchType は、型 left と right の値を返す CASE や IF の結果の型（HeimDB: IF の数の型にも使う。上流の #3025 の
// 取り込みで出した）。
func CombinedBranchType(left, right sql.Type) sql.Type {
	return combinedCaseBranchType(left, right)
}

// Type implements the sql.Expression interface.
func (c *Case) Type() sql.Type {
	curr := types.Null
	for _, b := range c.Branches {
		curr = combinedCaseBranchType(curr, b.Value.Type())
	}
	if c.Else != nil {
		curr = combinedCaseBranchType(curr, c.Else.Type())
	}
	return curr
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (c *Case) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	// This should be calculated during the expression's evaluation, but that's not possible with the
	// current abstraction
	return c.Type().CollationCoercibility(ctx)
}

// IsNullable implements the sql.Expression interface.
func (c *Case) IsNullable() bool {
	for _, b := range c.Branches {
		if b.Value.IsNullable() {
			return true
		}
	}

	return c.Else == nil || c.Else.IsNullable()
}

// Resolved implements the sql.Expression interface.
func (c *Case) Resolved() bool {
	if (c.Expr != nil && !c.Expr.Resolved()) ||
		(c.Else != nil && !c.Else.Resolved()) {
		return false
	}

	for _, b := range c.Branches {
		if !b.Cond.Resolved() || !b.Value.Resolved() {
			return false
		}
	}

	return true
}

// Children implements the sql.Expression interface.
func (c *Case) Children() []sql.Expression {
	var children []sql.Expression

	if c.Expr != nil {
		children = append(children, c.Expr)
	}

	for _, b := range c.Branches {
		children = append(children, b.Cond, b.Value)
	}

	if c.Else != nil {
		children = append(children, c.Else)
	}

	return children
}

// Eval implements the sql.Expression interface.
func (c *Case) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	span, ctx := ctx.Span("expression.Case")
	defer span.End()

	t := c.Type()

	for _, b := range c.Branches {
		var cond sql.Expression
		if c.Expr != nil {
			cond = NewEquals(c.Expr, b.Cond)
		} else {
			cond = b.Cond
		}

		res, err := sql.EvaluateCondition(ctx, cond, row)
		if err != nil {
			return nil, err
		}

		if sql.IsTrue(res) {
			bval, err := b.Value.Eval(ctx, row)
			if err != nil {
				return nil, err
			}
			bval = textOfDate(t, b.Value.Type(), bval)
			bval = BranchDecimal(b.Value.Type(), bval)
			// When unable to convert to the type of the case, return the original value
			// A common error here is "Out of bounds value for decimal type"
			if ret, _, err := t.Convert(ctx, bval); err == nil {
				return ret, nil
			}
			return bval, nil
		}
	}

	if c.Else != nil {
		val, err := c.Else.Eval(ctx, row)
		if err != nil {
			return nil, err
		}
		val = textOfDate(t, c.Else.Type(), val)
		val = BranchDecimal(c.Else.Type(), val)
		// When unable to convert to the type of the case, return the original value
		// A common error here is "Out of bounds value for decimal type"
		if ret, _, err := t.Convert(ctx, val); err == nil {
			return ret, nil
		}
		return val, nil

	}

	return nil, nil
}

// WithChildren implements the Expression interface.
func (c *Case) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	var expected = len(c.Branches) * 2
	if c.Expr != nil {
		expected++
	}

	if c.Else != nil {
		expected++
	}

	if len(children) != expected {
		return nil, sql.ErrInvalidChildrenNumber.New(c, len(children), expected)
	}

	var expr, elseExpr sql.Expression
	if c.Expr != nil {
		expr = children[0]
		children = children[1:]
	}

	if c.Else != nil {
		elseExpr = children[len(children)-1]
		children = children[:len(children)-1]
	}

	var branches []CaseBranch
	for i := 0; i < len(children); i += 2 {
		branches = append(branches, CaseBranch{
			Cond:  children[i],
			Value: children[i+1],
		})
	}

	return NewCase(expr, branches, elseExpr), nil
}

func (c *Case) String() string {
	var buf bytes.Buffer

	buf.WriteString("CASE ")
	if c.Expr != nil {
		buf.WriteString(c.Expr.String())
	}

	for _, b := range c.Branches {
		buf.WriteString(" WHEN ")
		buf.WriteString(b.Cond.String())
		buf.WriteString(" THEN ")
		buf.WriteString(b.Value.String())
	}

	if c.Else != nil {
		buf.WriteString(" ELSE ")
		buf.WriteString(c.Else.String())
	}

	buf.WriteString(" END")
	return buf.String()
}

func (c *Case) DebugString() string {
	var buf bytes.Buffer

	buf.WriteString("CASE ")
	if c.Expr != nil {
		buf.WriteString(sql.DebugString(c.Expr))
	}

	for _, b := range c.Branches {
		buf.WriteString(" WHEN ")
		buf.WriteString(sql.DebugString(b.Cond))
		buf.WriteString(" THEN ")
		buf.WriteString(sql.DebugString(b.Value))
	}

	if c.Else != nil {
		buf.WriteString(" ELSE ")
		buf.WriteString(sql.DebugString(c.Else))
	}

	buf.WriteString(" END")
	return buf.String()
}

// textOfDate returns a date value v of type vt as its type's text when the
// result type t is text (CASE ... THEN dd ELSE 'x' END shows '2024-01-01' for a
// DATE; HeimDB, stage 116).
func textOfDate(t, vt sql.Type, v interface{}) interface{} {
	if types.IsText(t) {
		if s, ok := types.ValueString(vt, v); ok {
			return s
		}
	}
	return v
}

// MixedBranchTypesAreText reports whether a CASE, IF or COALESCE of branches
// of types a and b is a string, as in MySQL: a date or time, an ENUM or a SET
// with a number, or an ENUM or SET with a date or time.
// IF(c, d, 0) shows '0' or the datetime, IF(c, e, 1) the ENUM's value (HeimDB,
// stage 116; they took the first branch's type).
func MixedBranchTypesAreText(a, b sql.Type) bool {
	special := func(t sql.Type) bool {
		return types.IsTime(t) || types.IsTimespan(t) || types.IsEnum(t) || types.IsSet(t)
	}
	if a == types.Null || b == types.Null {
		return false
	}
	switch {
	case special(a) && types.IsNumber(b), special(b) && types.IsNumber(a):
		return true
	case (types.IsEnum(a) || types.IsSet(a)) != (types.IsEnum(b) || types.IsSet(b)) && (special(a) || special(b)):
		return true
	}
	return false
}

// decimalDigits returns the precision and scale of a DECIMAL or integer type.
func decimalDigits(t sql.Type) (precision, scale uint8, ok bool) {
	if dt, isDec := t.(sql.DecimalType); isDec && types.IsDecimal(t) {
		return dt.Precision(), dt.Scale(), true
	}
	if !types.IsInteger(t) {
		return 0, 0, false
	}
	switch t.Type() {
	case sqltypes.Int8, sqltypes.Uint8:
		return 3, 0, true
	case sqltypes.Int16, sqltypes.Uint16:
		return 5, 0, true
	case sqltypes.Int24, sqltypes.Uint24:
		return 8, 0, true
	case sqltypes.Int32, sqltypes.Uint32:
		return 10, 0, true
	case sqltypes.Uint64:
		return 20, 0, true
	}
	return 19, 0, true
}

// BranchDecimal shows a DECIMAL branch's value with its type's decimals (a
// DECIMAL(10,2) column's 0.3 is 0.30), as MySQL's CASE returns each branch's
// decimal (HeimDB, stage 116).
func BranchDecimal(bt sql.Type, v interface{}) interface{} {
	if d, ok := v.(decimal.Decimal); ok {
		if dt, isDec := bt.(sql.DecimalType); isDec && types.IsDecimal(bt) && d.Exponent() > -int32(dt.Scale()) {
			return d.Round(int32(dt.Scale()))
		}
	}
	return v
}
