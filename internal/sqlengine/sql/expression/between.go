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
// Modified by Tsukinowa Inc.: DECIMAL と文字列の混ざる BETWEEN を、MySQL と同じく DOUBLE で比べる（段階 115 の 2 度目のレビュー。docs/engine.md）

package expression

import (
	"fmt"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Between checks a value is between two given values.
type Between struct {
	Val   sql.Expression
	Lower sql.Expression
	Upper sql.Expression
}

var _ sql.Expression = (*Between)(nil)
var _ sql.CollationCoercible = (*Between)(nil)

// NewBetween creates a new Between expression.
func NewBetween(val, lower, upper sql.Expression) *Between {
	return &Between{val, lower, upper}
}

func (b *Between) String() string {
	return fmt.Sprintf("(%s BETWEEN %s AND %s)", b.Val, b.Lower, b.Upper)
}

func (b *Between) DebugString() string {
	return fmt.Sprintf("(%s BETWEEN %s AND %s)", sql.DebugString(b.Val), sql.DebugString(b.Lower), sql.DebugString(b.Upper))
}

// Children implements the Expression interface.
func (b *Between) Children() []sql.Expression {
	return []sql.Expression{b.Val, b.Lower, b.Upper}
}

// Type implements the Expression interface.
func (*Between) Type() sql.Type { return types.Boolean }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (b *Between) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, b.Val)
}

// IsNullable implements the Expression interface.
func (b *Between) IsNullable() bool {
	return b.Val.IsNullable() || b.Lower.IsNullable() || b.Upper.IsNullable()
}

// Resolved implements the Expression interface.
func (b *Between) Resolved() bool {
	return b.Val.Resolved() && b.Lower.Resolved() && b.Upper.Resolved()
}

// Eval implements the Expression interface.
func (b *Between) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	// TODO: Delete Between expression entirely?
	// Reuse type conversion logic in comparison expressions by creating a logically equivalent expression
	val, lower, upper := BetweenOperands(b)
	return NewAnd(NewLessThanOrEqual(lower, val), NewGreaterThanOrEqual(upper, val)).Eval(ctx, row)
}

// BetweenOperands returns b's value and bounds for the comparisons that b
// becomes (here and in the analyzer's filter rewrites). MySQL compares a
// BETWEEN in the type that all three operands agree on, which is a double when
// a DECIMAL meets a string, even a constant one ('10.5000000000000000001'
// BETWEEN matches 10.50); a plain comparison compares a DECIMAL with a
// constant string as decimals. So when the operands mix a DECIMAL and a
// string, the strings are wrapped in RealComparand, which the comparisons
// treat as a non-constant string and compare as doubles (HeimDB, stage 115).
func BetweenOperands(b *Between) (val, lower, upper sql.Expression) {
	val, lower, upper = b.Val, b.Lower, b.Upper
	ops := []sql.Expression{val, lower, upper}
	hasDecimal, hasText, hasNumber, texts := false, false, false, 0
	for _, e := range ops {
		t := e.Type()
		hasDecimal = hasDecimal || types.IsDecimal(t)
		hasText = hasText || types.IsText(t)
		hasNumber = hasNumber || types.IsNumber(t)
		if types.IsText(t) {
			texts++
		}
	}
	// HeimDB: a string column between a string and a number (name BETWEEN '1'
	// AND 7) compares as doubles, as MySQL does; the strings' comparison with
	// each other was a string comparison (stage 116)
	if !(hasDecimal && hasText) && !(hasNumber && texts >= 2) {
		return
	}
	for i, e := range ops {
		if _, ok := e.(*RealComparand); !ok && types.IsText(e.Type()) {
			ops[i] = &RealComparand{Child: e}
		}
	}
	return ops[0], ops[1], ops[2]
}

// RealComparand wraps a string operand of a BETWEEN that also has a DECIMAL
// operand (BetweenOperands), so the comparisons that the BETWEEN becomes
// compare it with the DECIMAL as doubles, as MySQL does, and not as decimals,
// as they do with a constant string. It evaluates to its child, and prints as
// its child, so plans read as before (HeimDB, stage 115).
type RealComparand struct {
	Child sql.Expression
}

var _ sql.Expression = (*RealComparand)(nil)
var _ sql.CollationCoercible = (*RealComparand)(nil)

func (r *RealComparand) String() string { return r.Child.String() }

func (r *RealComparand) DebugString() string { return sql.DebugString(r.Child) }

// Children implements the Expression interface.
func (r *RealComparand) Children() []sql.Expression { return []sql.Expression{r.Child} }

// Type implements the Expression interface.
func (r *RealComparand) Type() sql.Type { return r.Child.Type() }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (r *RealComparand) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, r.Child)
}

// IsNullable implements the Expression interface.
func (r *RealComparand) IsNullable() bool { return r.Child.IsNullable() }

// Resolved implements the Expression interface.
func (r *RealComparand) Resolved() bool { return r.Child.Resolved() }

// Eval implements the Expression interface.
func (r *RealComparand) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return r.Child.Eval(ctx, row)
}

// WithChildren implements the Expression interface.
func (r *RealComparand) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(r, len(children), 1)
	}
	return &RealComparand{Child: children[0]}, nil
}

// WithChildren implements the Expression interface.
func (b *Between) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 3 {
		return nil, sql.ErrInvalidChildrenNumber.New(b, len(children), 3)
	}
	return NewBetween(children[0], children[1], children[2]), nil
}
