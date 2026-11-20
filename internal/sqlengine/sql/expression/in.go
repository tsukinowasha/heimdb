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

// Modified by Tsukinowa Inc.: IN の値を数にするとき、文字列の先頭の数の部分を読む（docs/engine.md）
// Modified by Tsukinowa Inc.: 文字列と数の IN を、値の組ごとに数として比べる（docs/engine.md）
// Modified by Tsukinowa Inc.: IN の日時の文字列の後ろの余計な文字を切り詰めて比べる（上流の #3276。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL の IN を、どちらかの型の桁に丸めずに DECIMAL で比べる（上流の #4054 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL の IN の定数でない文字列は DOUBLE で比べる（段階 115 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL と文字列の混ざる 2 つ以上の値の IN は、MySQL と同じくすべて DOUBLE で比べる（段階 115 の 2 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: 2 つ以上の値の IN で、日時の列と日時として読めない文字列をゼロの日時として比べる（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 行の値の IN の NULL を MySQL と同じく NULL にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM の IN の '' を空の値（番号 0）にする（段階 116 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM の IN の値を比べるときの変換（ConvertToCompare）で変える（段階 116 の 2 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM と SET の比べを、COLLATE を付けた側の照合順序で比べる（段階 116 の 3 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列と数の IN で警告 1292 を出さない（doubleForIn。段階 119。docs/engine.md）

package expression

import (
	"fmt"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// InTuple is an expression that checks an expression is inside a list of expressions.
type InTuple struct {
	BinaryExpressionStub
}

// We implement Comparer because we have a Left() and a Right(), but we can't be Compare()d
var _ Comparer = (*InTuple)(nil)
var _ sql.CollationCoercible = (*InTuple)(nil)

func (in *InTuple) Compare(ctx *sql.Context, row sql.Row) (int, error) {
	panic("Compare not implemented for InTuple")
}

func (in *InTuple) Type() sql.Type {
	return types.Boolean
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*InTuple) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (in *InTuple) Left() sql.Expression {
	return in.BinaryExpressionStub.LeftChild
}

func (in *InTuple) Right() sql.Expression {
	return in.BinaryExpressionStub.RightChild
}

// NewInTuple creates an InTuple expression.
func NewInTuple(left sql.Expression, right sql.Expression) *InTuple {
	disableRounding(left)
	disableRounding(right)
	return &InTuple{BinaryExpressionStub{left, right}}
}

// Eval implements the Expression interface.
func (in *InTuple) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	typ := in.Left().Type().Promote()
	leftElems := types.NumColumns(typ)
	originalLeft, err := in.Left().Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if originalLeft == nil {
		return nil, nil
	}

	// The NULL handling for IN expressions is tricky. According to
	// https://dev.mysql.com/doc/refman/8.0/en/comparison-operators.html#operator_in:
	// To comply with the SQL standard, IN() returns NULL not only if the expression on the left hand side is NULL, but
	// also if no match is found in the list and one of the expressions in the list is NULL.
	rightNull := false

	left, _, err := typ.Convert(ctx, originalLeft)
	if err != nil && !types.TruncatedDatetime(err) {
		return nil, err
	}

	var leftDouble float64
	var leftDoubleOK, leftDoubleDone bool
	var leftDec decimal.Decimal
	var leftDecOK, leftDecDone bool
	switch right := in.Right().(type) {
	case Tuple:
		for _, el := range right {
			if types.NumColumns(el.Type()) != leftElems {
				return nil, sql.ErrInvalidOperandColumns.New(leftElems, types.NumColumns(el.Type()))
			}
		}

		// HeimDB: in a list of two or more values, MySQL compares a DECIMAL with
		// each string as a double, even a constant one (n IN ('10.5000000000000000001',
		// '0.3') matches 10.50 and 0.30), and with each number as a decimal
		// (n IN (10.5000000000000000001, 'x') does not match 10.50); a single
		// value compares as = does (decimalStringAsDecimal) (stage 115)
		leftDecimal := types.IsDecimal(in.Left().Type())
		stringsReal := leftDecimal && len(right) > 1
		// HeimDB: ENUM と SET は、COLLATE を付けた値があればその照合順序で比べる（e IN ('a ' COLLATE utf8mb4_bin, 'c')
		// は PAD SPACE で 'a' に合う。= と同じ。段階 116 の 3 度目のレビュー）
		if twc, ok := typ.(sql.TypeWithCollation); ok && (types.IsEnum(typ) || types.IsSet(typ)) {
			for _, el := range right {
				if coll, coer := sql.GetCoercibility(ctx, el); coer == 0 && coll != twc.Collation() {
					if nt, err := twc.WithNewCollation(coll); err == nil {
						typ = nt
					}
					break
				}
			}
		}
		for _, el := range right {
			originalRight, err := el.Eval(ctx, row)
			if err != nil {
				return nil, err
			}

			if !rightNull && originalRight == nil {
				rightNull = true
				continue
			}
			if lt, ok := originalLeft.([]interface{}); ok {
				// HeimDB: rows compare as =, with NULL for a NULL in a pair that does not differ (stage 116)
				if rt, ok := originalRight.([]interface{}); ok && types.IsTuple(el.Type()) {
					res, err := TupleEquals(ctx, in.Left().Type(), el.Type(), lt, rt)
					if err != nil {
						return nil, err
					}
					if res == true {
						return true, nil
					}
					if res == nil {
						rightNull = true
					}
					continue
				}
			}

			var cmp int
			elType := el.Type()
			if stringsReal && types.IsText(elType) || types.IsTextOnly(in.Left().Type()) && types.IsNumber(elType) &&
				!(len(right) == 1 && types.IsDecimal(elType) && decimalStringAsDecimal(in.Left(), el)) {
				// HeimDB: 文字列と数は、MySQL と同じく値の組ごとに数（double）として比べる（meta_value IN (12, 9) は
				// '12abc' と ' 12 ' にも当たる。前は左の文字列の型に揃えて文字列として比べた）。数にできない文字列は、
				// 等号と同じく警告 1292 を出す。
				// 左は 1 度だけ数にする（警告も 1 度。MySQL と同じ）
				if !leftDoubleDone {
					leftDouble, leftDoubleOK = doubleForIn(ctx, originalLeft, IsTextColumn(in.Left()))
					leftDoubleDone = true
				}
				l, lok := leftDouble, leftDoubleOK
				r, rok := doubleForIn(ctx, originalRight, IsTextColumn(el))
				if lok && rok && l == r {
					return true, nil
				}
				continue
			}
			if leftDecimal && types.IsText(elType) && !decimalStringAsDecimal(in.Left(), el) {
				// HeimDB: a string column or other non-constant string, or any string
				// with a constant DECIMAL, compares with a DECIMAL as a double, as in
				// the comparison operators (stage 115's reviews)
				if !leftDoubleDone {
					leftDouble, leftDoubleOK = doubleForIn(ctx, originalLeft, IsTextColumn(in.Left()))
					leftDoubleDone = true
				}
				r, rok := doubleForIn(ctx, originalRight, IsTextColumn(el))
				if leftDoubleOK && rok && leftDouble == r {
					return true, nil
				}
				continue
			}
			if leftDecimal && !types.IsTuple(elType) ||
				len(right) == 1 && types.IsText(in.Left().Type()) && types.IsDecimal(elType) {
				// HeimDB: a DECIMAL compares with each value as an exact decimal, as
				// the comparison operators do (stage 115, with upstream #4054;
				// converting to one side's scale rounded 123.45 IN ('123.451') and
				// 123.45 IN (123.5) to true); so does a constant string with a single
				// DECIMAL value, as '10.5000000000000000001' = n does
				if !leftDecDone {
					l, err := convertValue(ctx, originalLeft, ConvertToDecimal, nil, 0, 0)
					leftDec, leftDecOK = l.(decimal.Decimal)
					leftDecOK = leftDecOK && err == nil
					leftDecDone = true
				}
				r, rerr := convertValue(ctx, originalRight, ConvertToDecimal, nil, 0, 0)
				rd, rok := r.(decimal.Decimal)
				if leftDecOK && rerr == nil && rok {
					if leftDec.Equal(rd) {
						return true, nil
					}
					continue
				}
			}
			if types.IsDecimal(elType) || types.IsFloat(elType) {
				rtyp := el.Type().Promote()
				left, err := convertOrTruncate(ctx, left, rtyp)
				if err != nil {
					return nil, err
				}
				right, err := convertOrTruncate(ctx, originalRight, rtyp)
				if err != nil {
					return nil, err
				}
				cmp, err = rtyp.Compare(ctx, left, right)
				if err != nil {
					return nil, err
				}
			} else {
				var right interface{}
				var err error
				if len(in.Right().(Tuple)) > 1 && types.IsTime(typ) && types.IsText(elType) {
					// HeimDB: in a list of two or more values, MySQL compares a date with a
					// string that is not a date as the zero date, with warning 1292
					// (post_date IN ('garbage', ...) matches the zero dates; one value or =
					// is error 1525) (stage 116)
					var ok bool
					if right, ok = datetimeOrZero(ctx, originalRight); !ok {
						right, err = convertOrTruncate(ctx, originalRight, typ)
					}
				} else {
					right, err = convertOrTruncateQuiet(ctx, originalRight, typ, IsTextColumn(el))
				}
				if err != nil {
					return nil, err
				}
				cmp, err = typ.Compare(ctx, left, right)
				if err != nil {
					return nil, err
				}
			}

			if cmp == 0 {
				return true, nil
			}
		}

		if rightNull {
			return nil, nil
		}

		return false, nil
	default:
		return nil, ErrUnsupportedInOperand.New(right)
	}
}

// WithChildren implements the Expression interface.
func (in *InTuple) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(in, len(children), 2)
	}
	return NewInTuple(children[0], children[1]), nil
}

func (in *InTuple) String() string {
	// scalar expression must round-trip
	return fmt.Sprintf("(%s IN %s)", in.Left(), in.Right())
}

func (in *InTuple) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("IN")
	children := []string{fmt.Sprintf("left: %s", sql.DebugString(in.Left())), fmt.Sprintf("right: %s", sql.DebugString(in.Right()))}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

// Children implements the Expression interface.
func (in *InTuple) Children() []sql.Expression {
	return []sql.Expression{in.Left(), in.Right()}
}

// NewNotInTuple creates a new NotInTuple expression.
func NewNotInTuple(left sql.Expression, right sql.Expression) sql.Expression {
	return NewNot(NewInTuple(left, right))
}

// HashInTuple is an expression that checks an expression is inside a list of expressions using a hashmap.
type HashInTuple struct {
	in      *InTuple
	cmp     map[uint64]sql.Expression
	hasNull bool
}

var _ Comparer = (*HashInTuple)(nil)
var _ sql.CollationCoercible = (*HashInTuple)(nil)
var _ sql.Expression = (*HashInTuple)(nil)

// NewHashInTuple creates an InTuple expression.
func NewHashInTuple(ctx *sql.Context, left, right sql.Expression) (*HashInTuple, error) {
	rightTup, ok := right.(Tuple)
	if !ok {
		return nil, ErrUnsupportedInOperand.New(right)
	}

	cmp, hasNull, err := newInMap(ctx, rightTup, left.Type())
	if err != nil {
		return nil, err
	}

	return &HashInTuple{in: NewInTuple(left, right), cmp: cmp, hasNull: hasNull}, nil
}

// newInMap hashes static expressions in the right child Tuple of a InTuple node
func newInMap(ctx *sql.Context, right Tuple, lType sql.Type) (map[uint64]sql.Expression, bool, error) {
	if lType == types.Null {
		return nil, true, nil
	}

	elements := make(map[uint64]sql.Expression)
	hasNull := false
	lColumnCount := types.NumColumns(lType)

	for _, el := range right {
		rType := el.Type().Promote()
		rColumnCount := types.NumColumns(rType)
		if rColumnCount != lColumnCount {
			return nil, false, sql.ErrInvalidOperandColumns.New(lColumnCount, rColumnCount)
		}

		if rType == types.Null {
			hasNull = true
			continue
		}
		i, err := el.Eval(ctx, sql.Row{})
		if err != nil {
			return nil, hasNull, err
		}
		if i == nil {
			hasNull = true
			continue
		}

		if len(right) > 1 && types.IsTime(lType) && types.IsText(rType) {
			// HeimDB: a string that is not a date is the zero date in a list of
			// two or more values, as in InTuple.Eval (stage 116)
			if z, ok := datetimeOrZero(ctx, i); ok && z != nil {
				i = z
			}
		}
		var key uint64
		if types.IsDecimal(rType) || types.IsFloat(rType) {
			key, err = hashOfSimple(ctx, i, rType)
		} else {
			key, err = hashOfSimple(ctx, i, lType)
		}
		if err != nil {
			return nil, false, err
		}
		elements[key] = el
	}

	return elements, hasNull, nil
}

func hashOfSimple(ctx *sql.Context, i interface{}, t sql.Type) (uint64, error) {
	if i == nil {
		return 0, nil
	}

	var str string
	coll := sql.Collation_Default
	if types.IsTuple(t) {
		tup := i.([]interface{})
		tupType := t.(types.TupleType)
		hashes := make([]uint64, len(tup))
		for idx, v := range tup {
			h, err := hashOfSimple(ctx, v, tupType[idx])
			if err != nil {
				return 0, err
			}
			hashes[idx] = h
		}
		str = fmt.Sprintf("%v", hashes)
	} else if types.IsTextOnly(t) {
		coll = t.(sql.StringType).Collation()
		if s, ok := i.(string); ok {
			str = s
		} else {
			converted, err := convertOrTruncate(ctx, i, t)
			if err != nil {
				return 0, err
			}
			str, _, err = sql.Unwrap[string](ctx, converted)
			if err != nil {
				return 0, err
			}
		}
	} else {
		x, err := convertOrTruncate(ctx, i, t.Promote())
		if err != nil {
			return 0, err
		}

		// Remove trailing 0s from floats
		switch v := x.(type) {
		case float32:
			str = strconv.FormatFloat(float64(v), 'f', -1, 32)
			if str == "-0" {
				str = "0"
			}
		case float64:
			str = strconv.FormatFloat(v, 'f', -1, 64)
			if str == "-0" {
				str = "0"
			}
		default:
			str = fmt.Sprintf("%v", v)
		}
	}

	// Collated strings that are equivalent may have different runes, so we must make them hash to the same value
	return coll.HashToUint(str)
}

// Eval implements the Expression interface.
func (hit *HashInTuple) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	leftElems := types.NumColumns(hit.in.Left().Type().Promote())

	leftVal, err := hit.in.Left().Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if leftVal == nil {
		return nil, nil
	}

	key, err := hashOfSimple(ctx, leftVal, hit.in.Left().Type())
	if err != nil {
		return nil, err
	}

	right, ok := hit.cmp[key]
	if !ok {
		if hit.hasNull {
			return nil, nil
		}
		return false, nil
	}

	if types.NumColumns(right.Type().Promote()) != leftElems {
		return nil, sql.ErrInvalidOperandColumns.New(leftElems, types.NumColumns(right.Type().Promote()))
	}

	return true, nil
}

// doubleForIn は、IN で文字列と数を比べるときの値（double。文字列は先頭の数の部分で、警告 1292 を出す）。HeimDB。
// quiet（TEXT の列。IsTextColumn）なら、MySQL と同じく警告を出さない（段階 119）。
func doubleForIn(ctx *sql.Context, v interface{}, quiet bool) (float64, bool) {
	f, _, err := types.Float64.Convert(ctx, v)
	if err != nil {
		if !types.TruncatedString(v, err) {
			return 0, false
		}
		if !quiet {
			truncatedWarning(ctx, "DOUBLE", v)
		}
	}
	d, ok := f.(float64)
	return d, ok
}

// convertOrTruncate converts the value |i| to type |t| and returns the converted value; if the value does not convert
// cleanly and the type is automatically coerced (i.e. string and numeric types), then a warning is logged and the
// value is truncated to the Zero value for type |t|. If the value does not convert and the type is not automatically
// coerced, then an error is returned.
func convertOrTruncate(ctx *sql.Context, i interface{}, t sql.Type) (interface{}, error) {
	return convertOrTruncateQuiet(ctx, i, t, false)
}

// convertOrTruncateQuiet は convertOrTruncate と同じ。quiet（TEXT の列。IsTextColumn）なら、文字列を数にするときに
// 警告 1292 を出さない（12 IN (v, 13)。MySQL と同じ。HeimDB: 段階 119）。
func convertOrTruncateQuiet(ctx *sql.Context, i interface{}, t sql.Type, quiet bool) (interface{}, error) {
	if et, ok := t.(types.EnumType); ok {
		// HeimDB: an ENUM is compared through ConvertToCompare: trailing spaces are
		// dropped only under a PAD SPACE collation (e IN ('a ', 'b') does not match 'a'
		// under utf8mb4_0900_ai_ci), and '' is the empty value, index 0, which a
		// non-strict INSERT stores; a string that is not a value matches nothing
		// (stage 116 second review)
		converted, err := et.ConvertToCompare(ctx, i)
		if err != nil {
			if types.ErrConvertingToEnum.Is(err) {
				return nil, nil
			}
			return nil, err
		}
		return converted, nil
	}
	converted, _, err := t.Convert(ctx, i)
	if err == nil {
		return converted, nil
	}

	// HeimDB: 日時の文字列の後ろの余計な文字は切り詰めた値にし、警告 1292 を出す（CAST と同じ。上流の #3276）。
	if types.TruncatedDatetime(err) && converted != nil {
		ctx.Warn(1292, "%s", err.Error())
		return converted, nil
	}

	// If a value can't be converted to an enum or set type, truncate it to a value that is guaranteed
	// to not match any enum value.
	if types.IsEnum(t) || types.IsSet(t) {
		return nil, nil
	}

	// Values for numeric and string types are automatically coerced. For all other types, if they
	// don't convert cleanly, it's an error.
	if err != nil && !(types.IsNumber(t) || types.IsTextOnly(t)) {
		return nil, err
	}

	// HeimDB: 文字列を数にするときは、先頭の数の部分を読んだ値にする（MySQL と同じ。'12abc' は 12）。
	if types.IsNumber(t) && types.TruncatedString(i, err) && converted != nil {
		if !quiet {
			truncatedWarning(ctx, "DOUBLE", i)
		}
		return converted, nil
	}

	// For numeric and string types, if the value can't be cleanly converted, truncate to the zero value for
	// the type and log a warning in the session.
	warning := sql.Warning{
		Level:   "Warning",
		Message: fmt.Sprintf("Truncated incorrect %s value: %v", t.String(), i),
		Code:    1292,
	}

	if ctx != nil && ctx.Session != nil && !(quiet && types.IsNumber(t)) {
		ctx.Session.Warn(&warning)
	}

	return t.Zero(), nil
}

func (hit *HashInTuple) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return hit.in.CollationCoercibility(ctx)
}

func (hit *HashInTuple) Resolved() bool {
	return hit.in.Resolved()
}

func (hit *HashInTuple) Type() sql.Type {
	return hit.in.Type()
}

func (hit *HashInTuple) IsNullable() bool {
	return hit.in.IsNullable()
}

func (hit *HashInTuple) Children() []sql.Expression {
	return hit.in.Children()
}

func (hit *HashInTuple) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(hit, len(children), 2)
	}
	ret := *hit
	newIn, err := ret.in.WithChildren(children...)
	ret.in = newIn.(*InTuple)
	return &ret, err
}

func (hit *HashInTuple) Compare(ctx *sql.Context, row sql.Row) (int, error) {
	return hit.in.Compare(ctx, row)
}

func (hit *HashInTuple) Left() sql.Expression {
	return hit.in.Left()
}

func (hit *HashInTuple) Right() sql.Expression {
	return hit.in.Right()
}

func (hit *HashInTuple) String() string {
	return fmt.Sprintf("(%s HASH IN %s)", hit.in.Left(), hit.in.Right())
}

func (hit *HashInTuple) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("HashIn")
	children := []string{sql.DebugString(hit.in.Left()), sql.DebugString(hit.in.Right())}
	_ = pr.WriteChildren(children...)
	return pr.String()
}
