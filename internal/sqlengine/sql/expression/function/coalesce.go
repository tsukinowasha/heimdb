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

// Modified by Tsukinowa Inc.: 文字列の関数の結果の型に、引数の照合順序を付ける（docs/engine.md）
// Modified by Tsukinowa Inc.: 結果が文字列の COALESCE の日付と日時を型の形の文字列にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時か ENUM と数の COALESCE を文字列にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時の枝の結果の型を、MySQL と同じく枝の中で最も多い小数の桁の DATETIME にする（types.TemporalBranchType。段階 116 の 4 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の引数の型を、MySQL と同じ小数の桁にする（段階 120。docs/engine.md）

package function

import (
	"fmt"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Coalesce returns the first non-NULL value in the list, or NULL if there are no non-NULL values.
type Coalesce struct {
	args []sql.Expression
	typ  sql.Type
}

var _ sql.FunctionExpression = (*Coalesce)(nil)
var _ sql.CollationCoercible = (*Coalesce)(nil)

// NewCoalesce creates a new Coalesce sql.Expression.
func NewCoalesce(args ...sql.Expression) (sql.Expression, error) {
	if len(args) == 0 {
		return nil, sql.ErrInvalidArgumentNumber.New("COALESCE", "1 or more", 0)
	}

	return &Coalesce{args: args}, nil
}

// FunctionName implements sql.FunctionExpression
func (c *Coalesce) FunctionName() string {
	return "coalesce"
}

// Description implements sql.FunctionExpression
func (c *Coalesce) Description() string {
	return "returns the first non-null value in a list."
}

// Type implements the sql.Expression interface.
// The return type of Type() is the aggregated type of the argument types.
func (c *Coalesce) Type() sql.Type {
	if c.typ != nil {
		return c.typ
	}
	// HeimDB: FLOAT(M,D) と DOUBLE(M,D) の引数は、MySQL と同じく引数の小数の桁の最大（段階 120。整数とは前は文字列の型）
	argTypes := make([]sql.Type, 0, len(c.args))
	for _, arg := range c.args {
		if arg != nil {
			argTypes = append(argTypes, arg.Type())
		}
	}
	if t, ok := expression.RealBranchType(argTypes...); ok {
		c.typ = t
		return t
	}
	retType := types.Null
	for i, arg := range c.args {
		if arg == nil {
			continue
		}
		argType := arg.Type()
		if sysVarType, ok := argType.(sql.SystemVariableType); ok {
			argType = sysVarType.UnderlyingType()
		}
		if i == 0 {
			retType = argType
			continue
		}
		if argType == nil || argType == types.Null {
			continue
		}
		if retType.Equals(argType) {
			continue
		}
		// HeimDB: dates and datetimes are DATETIME of the largest fractional digits of the
		// arguments, as in MySQL (COALESCE(d0, d3) was DATETIME; stage 116, fourth review)
		if t, ok := types.TemporalBranchType(retType, argType); ok {
			retType = t
			continue
		}
		if expression.MixedBranchTypesAreText(retType, argType) {
			// HeimDB: a date or ENUM with a number is a string, as in MySQL (stage 116)
			return collatedText(types.LongText, c.args...)
		}

		// special case for signed and unsigned integers
		if (types.IsSigned(retType) && types.IsUnsigned(argType)) || (types.IsUnsigned(retType) && types.IsSigned(argType)) {
			retType = types.MustCreateDecimalType(20, 0)
			continue
		}

		convType := expression.GetConvertToType(retType, argType)
		switch convType {
		case expression.ConvertToChar:
			// special case for float64s
			if (types.IsFloat64(argType) || types.IsFloat64(retType)) && !types.IsText(argType) && !types.IsText(retType) {
				retType = types.Float64
				continue
			}
			// Can't get any larger than this
			return collatedText(types.LongText, c.args...)
		case expression.ConvertToDecimal:
			if types.IsFloat64(retType) || types.IsFloat64(argType) {
				retType = types.Float64
			} else if types.IsDecimal(argType) {
				retType = argType
			} else if !types.IsDecimal(retType) {
				retType = types.MustCreateDecimalType(10, 0)
			}
		case expression.ConvertToUnsigned:
			if retType == types.Uint64 || argType == types.Uint64 {
				retType = types.Uint64
			} else {
				retType = types.Uint32
			}
		case expression.ConvertToSigned:
			if retType == types.Int64 || argType == types.Int64 {
				retType = types.Int64
			} else {
				retType = types.Int32
			}
		case expression.ConvertToFloat:
			if types.IsFloat64(retType) || types.IsFloat64(argType) {
				retType = types.Float64
			} else {
				retType = types.Float32
			}
		default:
		}
	}

	c.typ = retType
	return retType
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (c *Coalesce) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	// Preferably, this would be done during evaluation, but that's not possible with the current abstraction
	if typ := c.Type(); typ != nil {
		return typ.CollationCoercibility(ctx)
	}
	return sql.Collation_binary, 6
}

// IsNullable implements the sql.Expression interface.
// Returns true if all arguments are nil
// or of the first non-nil argument is nullable, otherwise false.
func (c *Coalesce) IsNullable() bool {
	for _, arg := range c.args {
		if arg == nil {
			continue
		}
		return arg.IsNullable()
	}
	return true
}

func (c *Coalesce) String() string {
	var args = make([]string, len(c.args))
	for i, arg := range c.args {
		args[i] = arg.String()
	}
	return fmt.Sprintf("%s(%s)", c.FunctionName(), strings.Join(args, ","))
}

func (c *Coalesce) DebugString() string {
	var args = make([]string, len(c.args))
	for i, arg := range c.args {
		args[i] = sql.DebugString(arg)
	}
	return fmt.Sprintf("%s(%s)", c.FunctionName(), strings.Join(args, ","))
}

// WithChildren implements the Expression interface.
func (*Coalesce) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewCoalesce(children...)
}

// Resolved implements the sql.Expression interface.
// The function checks if first non-nil argument is resolved.
func (c *Coalesce) Resolved() bool {
	for _, arg := range c.args {
		if arg == nil {
			continue
		}
		if !arg.Resolved() {
			return false
		}
	}
	return true
}

// Children implements the sql.Expression interface.
func (c *Coalesce) Children() []sql.Expression { return c.args }

// Eval implements the sql.Expression interface.
// The function evaluates the first non-nil argument. If the value is nil,
// then we keep going, otherwise we return the first non-nil value.
func (c *Coalesce) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	for _, arg := range c.args {
		if arg == nil {
			continue
		}

		val, err := arg.Eval(ctx, row)
		if err != nil {
			return nil, err
		}

		if val == nil {
			continue
		}

		if types.IsText(c.Type()) {
			// HeimDB: a date as its type's text (stage 116)
			if ts, ok := types.ValueString(arg.Type(), val); ok {
				val = ts
			}
		}
		if !types.IsEnum(c.Type()) && !types.IsSet(c.Type()) {
			val, _, err = c.Type().Convert(ctx, val)
			if err != nil {
				return nil, err
			}
		}
		return val, nil
	}

	return nil, nil
}
