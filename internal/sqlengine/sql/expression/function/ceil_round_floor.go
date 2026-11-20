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
// Modified by Tsukinowa Inc.: ROUND、CEIL、FLOOR の文字列の引数を先頭の数の部分で読み、警告 1292 を出す（上流の #3920 の問題。docs/engine.md）
// Modified by Tsukinowa Inc.: ROUND の型と DOUBLE の丸めを MySQL と同じにし、TRUNCATE() を足す（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: ROUND、FLOOR、CEIL の日付、日時、TIME を数の形にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列の ROUND、TRUNCATE、CEIL、FLOOR で警告 1292 を出さない（段階 119。docs/engine.md）
// Modified by Tsukinowa Inc.: 文字列の CEIL と FLOOR を MySQL と同じく DOUBLE にする（段階 119。docs/engine.md）

package function

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Ceil returns the smallest integer value not less than X.
type Ceil struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Ceil)(nil)
var _ sql.CollationCoercible = (*Ceil)(nil)

// NewCeil creates a new Ceil expression.
func NewCeil(num sql.Expression) sql.Expression {
	return &Ceil{expression.UnaryExpression{Child: num}}
}

// FunctionName implements sql.FunctionExpression
func (c *Ceil) FunctionName() string {
	return "ceil"
}

// Description implements sql.FunctionExpression
func (c *Ceil) Description() string {
	return "returns the smallest integer value that is greater than or equal to number."
}

// Type implements the Expression interface.
func (c *Ceil) Type() sql.Type {
	childType := c.Child.Type()
	if types.IsInteger(childType) {
		return childType
	}
	if types.IsText(childType) {
		// HeimDB: a string is a DOUBLE, as in MySQL (stage 119; INT was too small
		// for '3000000000')
		return types.Float64
	}
	return types.Int32
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Ceil) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (c *Ceil) String() string {
	return fmt.Sprintf("%s(%s)", c.FunctionName(), c.Child)
}

// WithChildren implements the Expression interface.
func (c *Ceil) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(c, len(children), 1)
	}
	return NewCeil(children[0]), nil
}

// Eval implements the Expression interface.
func (c *Ceil) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	child, err := c.Child.Eval(ctx, row)

	if err != nil {
		return nil, err
	}

	if child == nil {
		return nil, nil
	}

	if n, ok := types.TemporalNumber(ctx, c.Child.Type(), child); ok {
		// HeimDB: a date, datetime or TIME is its number (stage 116)
		f64, _ := n.Float64()
		child = f64
	}
	// non number type will be caught here
	if !types.IsNumber(c.Child.Type()) {
		orig := child
		child, _, err = types.Float64.Convert(ctx, child)
		if err != nil {
			// 先頭の数の部分（'2.1e+' は 2.1）と警告 1292（上流の #3920 の問題。前は 0）
			if !types.TruncatedString(orig, err) || child == nil {
				return int32(0), nil
			}
			if !expression.IsTextColumn(c.Child) { // HeimDB: not for a TEXT column, as in MySQL (stage 119)
				ctx.Warn(1292, "Truncated incorrect DOUBLE value: '%v'", orig)
			}
		}

		if types.IsText(c.Child.Type()) {
			return math.Ceil(child.(float64)), nil
		}
		return int32(math.Ceil(child.(float64))), nil
	}

	// if it's number type and not float value, it does not need ceil-ing
	switch num := child.(type) {
	case float64:
		return math.Ceil(num), nil
	case float32:
		return float32(math.Ceil(float64(num))), nil
	case decimal.Decimal:
		return num.Ceil(), nil
	default:
		return child, nil
	}
}

// Floor returns the biggest integer value not less than X.
type Floor struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Floor)(nil)
var _ sql.CollationCoercible = (*Floor)(nil)

// NewFloor returns a new Floor expression.
func NewFloor(num sql.Expression) sql.Expression {
	return &Floor{expression.UnaryExpression{Child: num}}
}

// FunctionName implements sql.FunctionExpression
func (f *Floor) FunctionName() string {
	return "floor"
}

// Description implements sql.FunctionExpression
func (f *Floor) Description() string {
	return "returns the largest integer value that is less than or equal to number."
}

// Type implements the Expression interface.
func (f *Floor) Type() sql.Type {
	childType := f.Child.Type()
	if types.IsInteger(childType) {
		return childType
	}
	if types.IsText(childType) {
		// HeimDB: a string is a DOUBLE, as in MySQL (stage 119; INT was too small
		// for '3000000000')
		return types.Float64
	}
	return types.Int32
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Floor) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (f *Floor) String() string {
	return fmt.Sprintf("%s(%s)", f.FunctionName(), f.Child)
}

// WithChildren implements the Expression interface.
func (f *Floor) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(f, len(children), 1)
	}
	return NewFloor(children[0]), nil
}

// Eval implements the Expression interface.
func (f *Floor) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	child, err := f.Child.Eval(ctx, row)

	if err != nil {
		return nil, err
	}

	if child == nil {
		return nil, nil
	}

	if n, ok := types.TemporalNumber(ctx, f.Child.Type(), child); ok {
		// HeimDB: a date, datetime or TIME is its number (stage 116)
		f64, _ := n.Float64()
		child = f64
	}
	// non number type will be caught here
	if !types.IsNumber(f.Child.Type()) {
		orig := child
		child, _, err = types.Float64.Convert(ctx, child)
		if err != nil {
			// 先頭の数の部分（'2.1e+' は 2.1）と警告 1292（上流の #3920 の問題。前は 0）
			if !types.TruncatedString(orig, err) || child == nil {
				return int32(0), nil
			}
			if !expression.IsTextColumn(f.Child) { // HeimDB: not for a TEXT column, as in MySQL (stage 119)
				ctx.Warn(1292, "Truncated incorrect DOUBLE value: '%v'", orig)
			}
		}

		if types.IsText(f.Child.Type()) {
			return math.Floor(child.(float64)), nil
		}
		return int32(math.Floor(child.(float64))), nil
	}

	// if it's number type and not float value, it does not need floor-ing
	switch num := child.(type) {
	case float64:
		return math.Floor(num), nil
	case float32:
		return float32(math.Floor(float64(num))), nil
	case decimal.Decimal:
		return num.Floor(), nil
	default:
		return child, nil
	}
}

// Round returns the number (x) with (d) requested decimal places.
// If d is negative, the number is returned with the (abs(d)) least significant
// digits of it's integer part set to 0. If d is not specified or nil/null
// it defaults to 0.
type Round struct {
	expression.BinaryExpressionStub
	// truncate は TRUNCATE(X, D)（HeimDB。丸めずに 0 の方へ切る）
	truncate bool
}

var _ sql.FunctionExpression = (*Round)(nil)
var _ sql.CollationCoercible = (*Round)(nil)

// NewRound returns a new Round expression.
func NewRound(args ...sql.Expression) (sql.Expression, error) {
	argLen := len(args)
	if argLen == 0 || argLen > 2 {
		return nil, sql.ErrInvalidArgumentNumber.New("ROUND", "1 or 2", argLen)
	}

	var right sql.Expression
	if len(args) == 2 {
		right = args[1]
	}

	return &Round{BinaryExpressionStub: expression.BinaryExpressionStub{LeftChild: args[0], RightChild: right}}, nil
}

// NewTruncate returns TRUNCATE(X, D), which is ROUND(X, D) cutting towards zero (HeimDB).
func NewTruncate(args ...sql.Expression) (sql.Expression, error) {
	if len(args) != 2 {
		return nil, sql.ErrInvalidArgumentNumber.New("TRUNCATE", "2", len(args))
	}
	return &Round{BinaryExpressionStub: expression.BinaryExpressionStub{LeftChild: args[0], RightChild: args[1]}, truncate: true}, nil
}

// FunctionName implements sql.FunctionExpression
func (r *Round) FunctionName() string {
	if r.truncate {
		return "truncate"
	}
	return "round"
}

// Description implements sql.FunctionExpression
func (r *Round) Description() string {
	if r.truncate {
		return "truncates the number to decimals decimal places."
	}
	return "rounds the number to decimals decimal places."
}

// Children implements the Expression interface.
func (r *Round) Children() []sql.Expression {
	if r.RightChild == nil {
		return []sql.Expression{r.LeftChild}
	}

	return r.BinaryExpressionStub.Children()
}

// Eval implements the Expression interface. As MySQL's Item_func_round: an
// integer gives an integer, a DECIMAL a DECIMAL rounded half away from zero,
// and a DOUBLE or a string a DOUBLE rounded as my_double_round does (rint, so
// half to even on the scaled double: ROUND(2.45e0, 1) is 2.4, ROUND('2.55', 1)
// is 2.6). HeimDB; upstream typed a string's result as an INT, so
// ROUND('2.55', 1) was 3, and rounded doubles as decimals.
func (r *Round) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	val, err := r.LeftChild.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	prec := int64(0)
	if r.RightChild != nil {
		tmp, err := r.RightChild.Eval(ctx, row)
		if err != nil {
			return nil, err
		}
		if tmp == nil {
			return nil, nil
		}
		tmp, _, err = types.Int64.Convert(ctx, tmp)
		if err != nil {
			// TODO: truncate
			return nil, err
		}
		prec = tmp.(int64)
	}

	lt := r.LeftChild.Type()
	if n, ok := types.TemporalNumber(ctx, lt, val); ok {
		// HeimDB: a date, datetime or TIME is its number (stage 116)
		val = n
	}
	if !types.IsInteger(lt) && !types.IsDecimal(lt) {
		f, ok, err := roundArgAsDouble(ctx, val, expression.IsTextColumn(r.LeftChild))
		if !ok {
			return nil, err
		}
		return roundDouble(f, prec, r.truncate), nil
	}

	decType := types.MustCreateDecimalType(types.DecimalTypeMaxPrecision, types.DecimalTypeMaxScale)
	val, _, err = decType.Convert(ctx, val)
	if err != nil {
		return nil, err
	}
	d := val.(decimal.Decimal)
	// MySQL cuts off at 30 for larger values
	if prec > types.DecimalTypeMaxScale {
		prec = types.DecimalTypeMaxScale
	}
	if prec < -types.DecimalTypeMaxPrecision {
		prec = -types.DecimalTypeMaxPrecision
	}
	places := int32(prec)
	if dt, ok := lt.(sql.DecimalType); ok && types.IsDecimal(lt) && places > int32(dt.Scale()) {
		places = int32(dt.Scale())
	}
	var tmp decimal.Decimal
	if r.truncate {
		tmp = d.Shift(places).Truncate(0).Shift(-places)
	} else {
		tmp = d.Round(places)
	}
	// the result has the type's decimals (with a D that is not constant, the argument's: ROUND(10.50, id) is 10.50)
	if rt, ok := r.Type().(sql.DecimalType); ok && types.IsDecimal(rt) {
		tmp = tmp.Round(int32(rt.Scale()))
	} else if places < 0 {
		tmp = tmp.Round(0)
	}

	var res interface{}
	if types.IsSigned(lt) {
		res, _, err = types.Int64.Convert(ctx, tmp)
	} else if types.IsUnsigned(lt) {
		res, _, err = types.Uint64.Convert(ctx, tmp)
	} else {
		res = tmp
	}
	return res, err
}

// roundArgAsDouble reads ROUND's first argument as a DOUBLE: a string by its
// leading number with warning 1292, as MySQL does (upstream #3920's issue);
// quiet (a TEXT column) gives no warning, as in MySQL (HeimDB, stage 119).
func roundArgAsDouble(ctx *sql.Context, val interface{}, quiet bool) (float64, bool, error) {
	if b, ok := val.([]byte); ok {
		val = string(b)
	}
	f, _, err := types.Float64.Convert(ctx, val)
	if err != nil {
		if !types.TruncatedString(val, err) || f == nil {
			return 0, false, err
		}
		if !quiet {
			ctx.Warn(1292, "Truncated incorrect DOUBLE value: '%v'", val)
		}
	}
	return f.(float64), true, nil
}

// roundDouble is MySQL's my_double_round.
func roundDouble(value float64, dec int64, truncate bool) float64 {
	abs := dec
	if abs < 0 {
		abs = -abs
	}
	tmp := math.Inf(1)
	if abs <= 308 {
		tmp = math.Pow10(int(abs))
	}
	divTmp, mulTmp := value/tmp, value*tmp
	switch {
	case dec < 0 && math.IsInf(tmp, 0):
		return 0
	case dec >= 0 && math.IsInf(mulTmp, 0):
		return value
	case truncate:
		if value >= 0 {
			if dec < 0 {
				return math.Floor(divTmp) * tmp
			}
			return math.Floor(mulTmp) / tmp
		}
		if dec < 0 {
			return math.Ceil(divTmp) * tmp
		}
		return math.Ceil(mulTmp) / tmp
	case dec < 0:
		return math.RoundToEven(divTmp) * tmp
	default:
		return math.RoundToEven(mulTmp) / tmp
	}
}

// IsNullable implements the Expression interface.
func (r *Round) IsNullable() bool {
	return r.LeftChild.IsNullable()
}

func (r *Round) String() string {
	if r.RightChild == nil {
		return fmt.Sprintf("%s(%s,0)", r.FunctionName(), r.LeftChild.String())
	}

	return fmt.Sprintf("%s(%s,%s)", r.FunctionName(), r.LeftChild.String(), r.RightChild.String())
}

// Resolved implements the Expression interface.
func (r *Round) Resolved() bool {
	return r.LeftChild.Resolved() && (r.RightChild == nil || r.RightChild.Resolved())
}

// Type implements the Expression interface: an integer type for an integer, a
// DECIMAL with the rounded decimals (a constant D, at most the argument's) for
// a DECIMAL, and a DOUBLE for anything else, as in MySQL.
func (r *Round) Type() sql.Type {
	leftChildType := r.LeftChild.Type()
	if types.IsUnsigned(leftChildType) {
		return types.Uint64
	}
	if types.IsInteger(leftChildType) {
		return types.Int64
	}
	dt, ok := leftChildType.(sql.DecimalType)
	if !ok || !types.IsDecimal(leftChildType) {
		return types.Float64
	}
	scale := int64(dt.Scale())
	switch {
	case r.RightChild == nil:
		scale = 0
	case expression.ConstantExpr(r.RightChild):
		if v, err := r.RightChild.Eval(sql.NewEmptyContext(), nil); err == nil && v != nil {
			if d, _, err := types.Int64.Convert(sql.NewEmptyContext(), v); err == nil {
				scale = max(0, min(d.(int64), scale))
			}
		}
	default:
		// a D that is not constant keeps the argument's type
		return types.MustCreateDecimalType(dt.Precision(), dt.Scale())
	}
	// ROUND can carry into one more digit; TRUNCATE cannot
	precision := int64(dt.Precision()) - int64(dt.Scale()) + scale
	if !r.truncate {
		precision++
	}
	precision = min(max(precision, 1), types.DecimalTypeMaxPrecision)
	return types.MustCreateDecimalType(uint8(precision), uint8(scale))
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Round) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// WithChildren implements the Expression interface.
func (r *Round) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if r.truncate {
		return NewTruncate(children...)
	}
	return NewRound(children...)
}
