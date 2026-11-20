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

// Modified by Tsukinowa Inc.: 文字列どうしの比較で、正しい UTF-8 の値は文字列への変換を省く（docs/engine.md）
// Modified by Tsukinowa Inc.: 日時と、日時として読めない文字列を、ゼロの日時として比べる（上流の #3276 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL との比較を、DECIMAL の側の桁に丸めずに比べる（上流の #4054。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL と定数でない文字列は DOUBLE で比べる（段階 115 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL と文字列は、定数でない DECIMAL と定数の文字列のときだけ DECIMAL で比べる（段階 115 の 2 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: constantExpr を ConstantExpr として外に出す（ROUND の型。段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時と数を MySQL と同じく、日時として読める数は日時で、ほかの数は数で比べる（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 行の値の = の NULL を MySQL と同じく NULL にする（TupleEquals。段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM か SET と定数でない文字列か定義の違う ENUM か SET を、値の文字列で比べる（段階 116 の 3 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM と SET の比べを、COLLATE を付けた側の照合順序で比べる（段階 116 の 3 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列と数の比べで警告 1292 を出さない（quietTextDoubles。段階 118。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM と SET を値の文字列で比べるかを、行ごとでなく式ごとに一度だけ決める（段階 122。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列の参照かは、値が文字列のときだけ調べる（quietTextDoubles。段階 122。docs/engine.md）
// Modified by Tsukinowa Inc.: 両辺の型が同じかと、= の両辺が行の値かも、式ごとに一度だけ決める（段階 123。docs/engine.md）

package expression

import (
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	errors "gopkg.in/src-d/go-errors.v1"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

var ErrInvalidRegexp = errors.NewKind("Invalid regular expression: %s")

// Comparer implements a comparison expression.
type Comparer interface {
	sql.Expression
	Compare(ctx *sql.Context, row sql.Row) (int, error)
	Left() sql.Expression
	Right() sql.Expression
}

// ErrNilOperand ir returned if some or both of the comparison's operands is nil.
var ErrNilOperand = errors.NewKind("nil operand found in comparison")

// PreciseComparison searches an expression tree for comparison
// expressions that require a conversion or type promotion.
// This utility helps determine if filter predicates can be pushed down.
func PreciseComparison(e sql.Expression) bool {
	var imprecise bool
	sql.Inspect(e, func(expr sql.Expression) bool {
		if cmp, ok := expr.(Comparer); ok {
			left, right := cmp.Left().Type(), cmp.Right().Type()

			// integer comparisons are exact
			if types.IsInteger(left) && types.IsInteger(right) {
				return true
			}

			// string type comparisons are exact
			if types.IsText(left) && types.IsText(right) {
				return true
			}

			if tupType, ok := right.(types.TupleType); ok {
				if tupleTypesMatch(left, tupType, types.IsInteger) || tupleTypesMatch(left, tupType, types.IsText) {
					return true
				}
				for _, right := range tupType {
					if !left.Equals(right) {
						imprecise = true
						return false
					}
				}
				return true
			}

			// comparisons with type conversions are sometimes imprecise
			if !left.Equals(right) {
				imprecise = true
				return false
			}
		}
		return true
	})
	return !imprecise
}

func tupleTypesMatch(left sql.Type, tup types.TupleType, typeCb func(t sql.Type) bool) bool {
	if !typeCb(left) {
		return false
	}
	for _, right := range tup {
		if !typeCb(right) {
			return false
		}
	}
	return true
}

type comparison struct {
	BinaryExpressionStub
	// typeMode は、両辺の型だけで決まる比べ方（typeModeDecided に、enumSetAsText の答え typeModeEnumSet、
	// TypesEqual の答え typeModeEqual、両辺が行の値か typeModeTuples を足したもの。0 はまだ決めていない）。答えは
	// 両辺の式の形と型だけで決まるので、最初の Compare か Equals の Eval で一度だけ決めて覚える（HeimDB: 段階 122 で
	// enumSetAsText を、段階 123 で TypesEqual と行の値の確かめを足した。前は行ごとに両辺の型を調べ、全件を読む WHERE
	// の比べが遅くなった）。式は並行して評価されることがあるので atomic で読み書きする。作るとき（newComparison）には
	// 決めない。解決する前の列（UnresolvedColumn）の Type は使えない。WithChildren は New* で新しい式を作るので、0 から
	// 決め直す。
	typeMode uint32
}

const (
	typeModeDecided uint32 = 1 << iota
	typeModeEnumSet
	typeModeEqual
	typeModeTuples
)

// modeOfTypes は、この比べの typeMode を返す（まだなら両辺の型から決めて覚える）。
func (c *comparison) modeOfTypes() uint32 {
	if m := atomic.LoadUint32(&c.typeMode); m != 0 {
		return m
	}
	lt, rt := c.Left().Type(), c.Right().Type()
	m := typeModeDecided
	if enumSetAsText(lt, rt, c.Left(), c.Right()) {
		m |= typeModeEnumSet
	}
	if types.TypesEqual(lt, rt) {
		m |= typeModeEqual
	}
	if types.IsTuple(lt) && types.IsTuple(rt) {
		m |= typeModeTuples
	}
	atomic.StoreUint32(&c.typeMode, m)
	return m
}

// useEnumSetText は、この比べが ENUM か SET を値の文字列で比べるか（enumSetAsText。答えを覚える）。
func (c *comparison) useEnumSetText() bool {
	return c.modeOfTypes()&typeModeEnumSet != 0
}

// disableRounding disables rounding for the given expression.
func disableRounding(expr sql.Expression) {
	setArithmeticOps(expr, -1)
	setDivOps(expr, -1)
}

func newComparison(left, right sql.Expression) comparison {
	disableRounding(left)
	disableRounding(right)
	return comparison{BinaryExpressionStub: BinaryExpressionStub{left, right}}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (c *comparison) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	leftCollation, leftCoercibility := sql.GetCoercibility(ctx, c.Left())
	rightCollation, rightCoercibility := sql.GetCoercibility(ctx, c.Right())
	return sql.ResolveCoercibility(leftCollation, leftCoercibility, rightCollation, rightCoercibility)
}

// Compare the two given values using the types of the expressions in the comparison.
// Since both types should be equal, it does not matter which type is used, but for
// reference, the left type is always used.
func (c *comparison) Compare(ctx *sql.Context, row sql.Row) (int, error) {
	left, right, err := c.evalLeftAndRight(ctx, row)
	if err != nil {
		return 0, err
	}

	if left == nil || right == nil {
		return 0, ErrNilOperand.New()
	}

	left, err = sql.UnwrapAny(ctx, left)
	if err != nil {
		return 0, err
	}

	right, err = sql.UnwrapAny(ctx, right)
	if err != nil {
		return 0, err
	}

	// HeimDB: ENUM か SET と、定数でない文字列か定義の違う ENUM か SET は、値の文字列を照合順序で比べる（段階 116 の
	// 3 度目のレビュー。前は ENUM の番号で比べ、結合の a.e = b.v で 'a' が '1' に合い、'a' に合わなかった）
	// （段階 122: 比べるかは式ごとに一度だけ決める。段階 123: 型が同じかも。modeOfTypes）
	mode := c.modeOfTypes()
	if mode&typeModeEnumSet != 0 {
		coll, _ := c.CollationCoercibility(ctx)
		return CompareEnumSetAsText(ctx, coll, c.Left().Type(), c.Right().Type(), left, right)
	}

	if mode&typeModeEqual != 0 {
		return c.Left().Type().Compare(ctx, left, right)
	}

	// ENUM, SET, and TIME must be excluded when doing comparisons, as they're too restrictive to use as a comparison
	// base.
	//
	// The best overall method would be to assign type priority. For example, INT would have a higher priority than
	// TINYINT. This could then be combined with the origin of the value (table column, procedure param, etc.) to
	// determine the best type for any comparison (tie-breakers can be simple rules such as the current left preference).
	var compareType sql.Type
	collationPreference := sql.Collation_Default
	switch c.Left().(type) {
	case *GetField, *UserVar, *SystemVar, *ProcedureParam:
		compareType = c.Left().Type()
		if twc, ok := compareType.(sql.TypeWithCollation); ok {
			collationPreference = twc.Collation()
		}
	default:
		switch c.Right().(type) {
		case *GetField, *UserVar, *SystemVar, *ProcedureParam:
			compareType = c.Right().Type()
			if twc, ok := compareType.(sql.TypeWithCollation); ok {
				collationPreference = twc.Collation()
			}
		}
	}
	if compareType != nil {
		_, isEnum := compareType.(sql.EnumType)
		_, isSet := compareType.(sql.SetType)
		_, isTime := compareType.(types.TimeType)
		if !isEnum && !isSet && !isTime {
			compareType = nil
		}
		// HeimDB: ENUM と SET は、比べの照合順序（COLLATE を付けた側が勝つ）で比べる（段階 116 の 3 度目のレビュー。
		// e = 'a ' COLLATE utf8mb4_bin は PAD SPACE で 'a' に合う。前は列の NO PAD の照合順序で比べた）
		if twc, ok := compareType.(sql.TypeWithCollation); ok && (isEnum || isSet) {
			if coll, _ := c.CollationCoercibility(ctx); coll != sql.Collation_Unspecified && coll != twc.Collation() {
				if nt, err := twc.WithNewCollation(coll); err == nil {
					compareType = nt
				}
			}
		}
	}
	if compareType == nil && (isRealComparand(c.Left()) || isRealComparand(c.Right())) &&
		types.IsText(c.Left().Type()) && types.IsText(c.Right().Type()) {
		// HeimDB: two strings of a BETWEEN that also has a number compare as doubles (stage 116)
		l, r, err := convertLeftAndRight(ctx, left, right, ConvertToDouble)
		if err != nil {
			return 0, err
		}
		return types.Float64.Compare(ctx, l, r)
	}
	if compareType == nil {
		if textValues(c.Left().Type(), c.Right().Type(), left, right) {
			// castLeftAndRight は両辺を LONGTEXT に変換するが、正しい UTF-8 の文字列は写すだけで値が変わらない
			// （HeimDB の段階 96。不正な UTF-8 は変換で NULL になるので、今までの経路に回す）。
			compareType = types.LongText
		} else {
			left, right, compareType, err = c.castLeftAndRight(ctx, left, right)
			if err != nil {
				return 0, err
			}
		}
	}
	if _, isSet := compareType.(sql.SetType); !isSet && types.IsTextOnly(compareType) {
		collationPreference, _ = c.CollationCoercibility(ctx)
		stringCompareType := compareType.(sql.StringType)
		compareType = types.MustCreateString(stringCompareType.Type(), stringCompareType.Length(), collationPreference)
	}

	return compareType.Compare(ctx, left, right)
}

// datetimeOrZero は、比べる値 v を日時にする。後ろを切り詰めて読んだ文字列はその値、日時として読めない文字列はゼロの日時に
// して、どちらも警告 1292 を出す。ok が false なら、ほかの理由で変えられなかった。
func datetimeOrZero(ctx *sql.Context, v interface{}) (interface{}, bool) {
	if v == nil {
		return nil, true
	}
	d, _, err := types.DatetimeMaxPrecision.Convert(ctx, v)
	switch {
	case err == nil:
		return d, true
	case types.TruncatedDatetime(err):
		ctx.Warn(1292, "%s", err.Error())
		return d, true
	case types.ErrConvertingToTime.Is(err):
		ctx.Warn(1292, "Incorrect datetime value: '%v'", v)
		return types.ZeroTime, true
	}
	return nil, false
}

// textValues は、両辺の型が文字列（バイナリでない）で、値がどちらも正しい UTF-8 の文字列か（HeimDB の段階 96）。
func textValues(leftType, rightType sql.Type, left, right interface{}) bool {
	if !types.IsTextOnly(leftType) || !types.IsTextOnly(rightType) {
		return false
	}
	l, ok := left.(string)
	if !ok {
		return false
	}
	r, ok := right.(string)
	return ok && utf8.ValidString(l) && utf8.ValidString(r)
}

func (c *comparison) evalLeftAndRight(ctx *sql.Context, row sql.Row) (interface{}, interface{}, error) {
	left, err := c.Left().Eval(ctx, row)
	if err != nil {
		return nil, nil, err
	}

	right, err := c.Right().Eval(ctx, row)
	if err != nil {
		return nil, nil, err
	}

	return left, right, nil
}

func (c *comparison) castLeftAndRight(ctx *sql.Context, left, right interface{}) (interface{}, interface{}, sql.Type, error) {
	leftType := c.Left().Type()
	rightType := c.Right().Type()
	if types.IsTuple(leftType) && types.IsTuple(rightType) {
		return left, right, c.Left().Type(), nil
	}

	if types.IsTime(leftType) && types.IsNumber(rightType) {
		if l, r, typ, ok := datetimeNumber(ctx, c.Left(), c.Right(), left, right); ok {
			return l, r, typ, nil
		}
	} else if types.IsNumber(leftType) && types.IsTime(rightType) {
		if r, l, typ, ok := datetimeNumber(ctx, c.Right(), c.Left(), right, left); ok {
			return l, r, typ, nil
		}
	}

	if types.IsTime(leftType) || types.IsTime(rightType) {
		l, r, err := convertLeftAndRight(ctx, left, right, ConvertToDatetime)
		if err != nil {
			// 日時として読めない文字列は、MySQL と同じくゼロの日時として比べ、警告 1292 を出す（'2024-01-01' > 'garbage'
			// は真。前は 1105 で文が止まった。段階 114）
			l, lok := datetimeOrZero(ctx, left)
			r, rok := datetimeOrZero(ctx, right)
			if !lok || !rok {
				return nil, nil, nil, err
			}
			return l, r, types.DatetimeMaxPrecision, nil
		}

		return l, r, types.DatetimeMaxPrecision, nil
	}

	// Rely on types.JSON.Compare to handle JSON comparisons
	if types.IsJSON(leftType) || types.IsJSON(rightType) {
		return left, right, types.JSON, nil
	}

	if types.IsBinaryType(leftType) || types.IsBinaryType(rightType) {
		l, r, err := convertLeftAndRight(ctx, left, right, ConvertToBinary)
		if err != nil {
			return nil, nil, nil, err
		}
		return l, r, types.LongBlob, nil
	}

	if types.IsNumber(leftType) || types.IsNumber(rightType) {
		// HeimDB: MySQL compares a DECIMAL with a string as doubles, except a
		// non-constant DECIMAL (a column, n + 0) with a constant string, which it
		// compares as decimals (n = '10.5000000000000000001' is false for 10.50).
		// A string column or other non-constant string (CONCAT(s, ...)), a
		// constant DECIMAL (10.50 = '10.5000000000000000001' is true) and a
		// BETWEEN's string (RealComparand) take the double comparison below
		// (stage 115's reviews).
		if (types.IsDecimal(leftType) || types.IsDecimal(rightType)) && decimalStringAsDecimal(c.Left(), c.Right()) {
			l, r, err := convertLeftAndRight(ctx, left, right, ConvertToDecimal)
			if err != nil {
				return nil, nil, nil, err
			}

			// HeimDB: compare in the internal DECIMAL(65,30), not in the DECIMAL
			// side's type, whose Compare rounds both values to its scale
			// (d = 123.45000001 and d = '123.451' were true for a DECIMAL(10,2) of
			// 123.45). Upstream #4054 widens the type to both sides' digits.
			return l, r, types.InternalDecimalType, nil
		}

		// HeimDB: a TEXT column compares as a double without warning 1292, as in MySQL
		// (stage 118)
		left, right = c.quietTextDoubles(ctx, left, right)
		if types.IsFloat(leftType) || types.IsFloat(rightType) {
			l, r, err := convertLeftAndRight(ctx, left, right, ConvertToDouble)
			if err != nil {
				return nil, nil, nil, err
			}

			return l, r, types.Float64, nil
		}

		if types.IsSigned(leftType) && types.IsSigned(rightType) {
			l, r, err := convertLeftAndRight(ctx, left, right, ConvertToSigned)
			if err != nil {
				return nil, nil, nil, err
			}

			return l, r, types.Int64, nil
		}

		if types.IsUnsigned(leftType) && types.IsUnsigned(rightType) {
			l, r, err := convertLeftAndRight(ctx, left, right, ConvertToUnsigned)
			if err != nil {
				return nil, nil, nil, err
			}

			return l, r, types.Uint64, nil
		}

		l, r, err := convertLeftAndRight(ctx, left, right, ConvertToDouble)
		if err != nil {
			return nil, nil, nil, err
		}

		return l, r, types.Float64, nil
	}

	left, right, err := convertLeftAndRight(ctx, left, right, ConvertToChar)
	if err != nil {
		return nil, nil, nil, err
	}

	return left, right, types.LongText, nil
}

// quietTextDoubles は、比べる側の TEXT の列の文字列の値を、警告を出さずに DOUBLE にする（数との比べは DOUBLE で
// 比べる。HeimDB: 段階 118）。
// 文字列でない値は quietDouble が変えないので、列を調べない（段階 122。行ごとに両辺の型を見ないため）。
func (c *comparison) quietTextDoubles(ctx *sql.Context, left, right interface{}) (interface{}, interface{}) {
	if _, ok := left.(string); ok && IsTextColumn(c.Left()) {
		left = quietDouble(ctx, left)
	}
	if _, ok := right.(string); ok && IsTextColumn(c.Right()) {
		right = quietDouble(ctx, right)
	}
	return left, right
}

// datetimeNumber gives the values with which a date or time te (value tv)
// compares with a number ne (value nv), as MySQL compares them (HeimDB, stage
// 116): a number that reads as a date (0, 20240101, 20240101100000) compares
// as that datetime, rounded to a column's precision when the number is
// constant (d = 20240101100000.6 is 10:00:01 for a DATETIME, dd = 20240101.5
// is 2024-01-01), and any other number (1, -1, 20241301) compares with the
// date as a number (YYYYMMDD, YYYYMMDDhhmmss), so d < 1 is the zero date.
// Before, a number was converted to a datetime, which was NULL for any number
// but 0, so d > 0 was false for every row and d > 20240101 failed.
func datetimeNumber(ctx *sql.Context, te, ne sql.Expression, tv, nv interface{}) (interface{}, interface{}, sql.Type, bool) {
	if tv == nil || nv == nil {
		return nil, nil, nil, false
	}
	if t, ok := types.NumberToDatetime(nv); ok {
		var n interface{} = t
		if _, isField := te.(*GetField); isField && constantExpr(ne) {
			if cv, _, err := te.Type().Convert(ctx, t); err == nil && cv != nil {
				n = cv
			}
		}
		l, _, err := types.DatetimeMaxPrecision.Convert(ctx, tv)
		if err != nil && !types.TruncatedDatetime(err) {
			return nil, nil, nil, false
		}
		r, _, err := types.DatetimeMaxPrecision.Convert(ctx, n)
		if err != nil {
			return nil, nil, nil, false
		}
		return l, r, types.DatetimeMaxPrecision, true
	}
	tn, ok := types.DatetimeToNumber(tv, te.Type())
	if !ok {
		return nil, nil, nil, false
	}
	n, _, err := types.InternalDecimalType.Convert(ctx, nv)
	if err != nil {
		return nil, nil, nil, false
	}
	return tn, n, types.InternalDecimalType, true
}

// decimalStringAsDecimal reports whether a comparison of left and right, one
// of them a DECIMAL, compares as decimals: always when neither is a string,
// and with a string only when the string is constant and the DECIMAL is not,
// as MySQL does (HeimDB, stage 115).
func decimalStringAsDecimal(left, right sql.Expression) bool {
	lt, rt := left.Type(), right.Type()
	switch {
	case types.IsText(lt) && types.IsDecimal(rt):
		return constantExpr(left) && !constantExpr(right)
	case types.IsText(rt) && types.IsDecimal(lt):
		return constantExpr(right) && !constantExpr(left)
	}
	return true
}

// constantExpr reports whether e has the same value for every row: a literal,
// or a deterministic expression of literals (HeimDB).
func constantExpr(e sql.Expression) bool {
	switch e.(type) {
	case *Literal:
		return true
	case *GetField, *UserVar, *SystemVar, *ProcedureParam, *BindVar, *RealComparand:
		return false
	}
	if nd, ok := e.(sql.NonDeterministicExpression); ok && nd.IsNonDeterministic() {
		return false
	}
	children := e.Children()
	if len(children) == 0 {
		return false
	}
	for _, ch := range children {
		if !constantExpr(ch) {
			return false
		}
	}
	return true
}

func convertLeftAndRight(ctx *sql.Context, left, right interface{}, convertTo string) (interface{}, interface{}, error) {
	l, err := convertValue(ctx, left, convertTo, nil, 0, 0)
	if err != nil {
		return nil, nil, err
	}

	r, err := convertValue(ctx, right, convertTo, nil, 0, 0)
	if err != nil {
		return nil, nil, err
	}

	return l, r, nil
}

// Type implements the Expression interface.
func (*comparison) Type() sql.Type {
	return types.Boolean
}

// Left implements Comparer interface
func (c *comparison) Left() sql.Expression { return c.BinaryExpressionStub.LeftChild }

// Right implements Comparer interface
func (c *comparison) Right() sql.Expression { return c.BinaryExpressionStub.RightChild }

// Equality is an expression that may represent equality between two parameters (the equals operator is one such example).
type Equality interface {
	BinaryExpression
	RepresentsEquality() bool
	SwapParameters(ctx *sql.Context) (Equality, error)
	ToComparer() (Comparer, error)
}

// Equals is a comparison that checks an expression is equal to another.
type Equals struct {
	comparison
}

var _ sql.Expression = (*Equals)(nil)
var _ sql.CollationCoercible = (*Equals)(nil)
var _ Equality = (*Equals)(nil)

// NewEquals returns a new Equals expression.
func NewEquals(left sql.Expression, right sql.Expression) *Equals {
	return &Equals{newComparison(left, right)}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (e *Equals) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (e *Equals) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	// HeimDB: 両辺が行の値かは式ごとに一度だけ決める（段階 123）
	if e.modeOfTypes()&typeModeTuples != 0 {
		l, err := e.Left().Eval(ctx, row)
		if err != nil {
			return nil, err
		}
		r, err := e.Right().Eval(ctx, row)
		if err != nil {
			return nil, err
		}
		if lt, ok := l.([]interface{}); ok {
			if rt, ok := r.([]interface{}); ok {
				return TupleEquals(ctx, e.Left().Type(), e.Right().Type(), lt, rt)
			}
		}
	}
	result, err := e.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == 0, nil
}

// WithChildren implements the Expression interface.
func (e *Equals) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 2)
	}
	return NewEquals(children[0], children[1]), nil
}

func (e *Equals) String() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("(%s = %s)", e.Left(), e.Right())
}

func (e *Equals) DebugString() string {
	if e == nil {
		return ""
	}
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("Eq")
	children := []string{sql.DebugString(e.Left()), sql.DebugString(e.Right())}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

// RepresentsEquality implements the Equality interface.
func (e *Equals) RepresentsEquality() bool {
	return true
}

// SwapParameters implements the Equality interface.
func (e *Equals) SwapParameters(ctx *sql.Context) (Equality, error) {
	return NewEquals(e.RightChild, e.LeftChild), nil
}

// ToComparer implements the Equality interface.
func (e *Equals) ToComparer() (Comparer, error) {
	return e, nil
}

// NullSafeEquals is a comparison that checks an expression is equal to
// another, where NULLs do not coalesce to NULL and two NULLs compare equal to
// each other.
type NullSafeEquals struct {
	comparison
}

var _ sql.Expression = (*NullSafeEquals)(nil)
var _ sql.CollationCoercible = (*NullSafeEquals)(nil)

// NewNullSafeEquals returns a new NullSafeEquals expression.
func NewNullSafeEquals(left sql.Expression, right sql.Expression) *NullSafeEquals {
	return &NullSafeEquals{newComparison(left, right)}
}

// Type implements the Expression interface.
func (e *NullSafeEquals) Type() sql.Type {
	return types.Boolean
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (e *NullSafeEquals) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (e *NullSafeEquals) Compare(ctx *sql.Context, row sql.Row) (int, error) {
	left, right, err := e.evalLeftAndRight(ctx, row)
	if err != nil {
		return 0, err
	}

	if left == nil && right == nil {
		return 0, nil
	} else if left == nil {
		return 1, nil
	} else if right == nil {
		return -1, nil
	}

	if types.TypesEqual(e.Left().Type(), e.Right().Type()) {
		return e.Left().Type().Compare(ctx, left, right)
	}

	var compareType sql.Type
	left, right, compareType, err = e.castLeftAndRight(ctx, left, right)
	if err != nil {
		return 0, err
	}

	return compareType.Compare(ctx, left, right)
}

// Eval implements the Expression interface.
func (e *NullSafeEquals) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := e.Compare(ctx, row)
	if err != nil {
		return nil, err
	}

	return result == 0, nil
}

// WithChildren implements the Expression interface.
func (e *NullSafeEquals) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 2)
	}
	return NewNullSafeEquals(children[0], children[1]), nil
}

func (e *NullSafeEquals) String() string {
	return fmt.Sprintf("(%s <=> %s)", e.Left(), e.Right())
}

func (e *NullSafeEquals) DebugString() string {
	return fmt.Sprintf("(%s <=> %s)", sql.DebugString(e.Left()), sql.DebugString(e.Right()))
}

// GreaterThan is a comparison that checks an expression is greater than another.
type GreaterThan struct {
	comparison
}

var _ sql.Expression = (*GreaterThan)(nil)
var _ sql.CollationCoercible = (*GreaterThan)(nil)

// NewGreaterThan creates a new GreaterThan expression.
func NewGreaterThan(left sql.Expression, right sql.Expression) *GreaterThan {
	return &GreaterThan{newComparison(left, right)}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (gt *GreaterThan) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (gt *GreaterThan) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := gt.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == 1, nil
}

// WithChildren implements the Expression interface.
func (gt *GreaterThan) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(gt, len(children), 2)
	}
	return NewGreaterThan(children[0], children[1]), nil
}

func (gt *GreaterThan) String() string {
	return fmt.Sprintf("(%s > %s)", gt.Left(), gt.Right())
}

func (gt *GreaterThan) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("GreaterThan")
	children := []string{sql.DebugString(gt.Left()), sql.DebugString(gt.Right())}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

// LessThan is a comparison that checks an expression is less than another.
type LessThan struct {
	comparison
}

var _ sql.Expression = (*LessThan)(nil)
var _ sql.CollationCoercible = (*LessThan)(nil)

// NewLessThan creates a new LessThan expression.
func NewLessThan(left sql.Expression, right sql.Expression) *LessThan {
	return &LessThan{newComparison(left, right)}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (lt *LessThan) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the expression interface.
func (lt *LessThan) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := lt.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == -1, nil
}

// WithChildren implements the Expression interface.
func (lt *LessThan) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(lt, len(children), 2)
	}
	return NewLessThan(children[0], children[1]), nil
}

func (lt *LessThan) String() string {
	return fmt.Sprintf("(%s < %s)", lt.Left(), lt.Right())
}

func (lt *LessThan) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("LessThan")
	children := []string{sql.DebugString(lt.Left()), sql.DebugString(lt.Right())}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

// GreaterThanOrEqual is a comparison that checks an expression is greater or equal to
// another.
type GreaterThanOrEqual struct {
	comparison
}

var _ sql.Expression = (*GreaterThanOrEqual)(nil)
var _ sql.CollationCoercible = (*GreaterThanOrEqual)(nil)

// NewGreaterThanOrEqual creates a new GreaterThanOrEqual
func NewGreaterThanOrEqual(left sql.Expression, right sql.Expression) *GreaterThanOrEqual {
	return &GreaterThanOrEqual{newComparison(left, right)}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (gte *GreaterThanOrEqual) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (gte *GreaterThanOrEqual) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := gte.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result > -1, nil
}

// WithChildren implements the Expression interface.
func (gte *GreaterThanOrEqual) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(gte, len(children), 2)
	}
	return NewGreaterThanOrEqual(children[0], children[1]), nil
}

func (gte *GreaterThanOrEqual) String() string {
	return fmt.Sprintf("(%s >= %s)", gte.Left(), gte.Right())
}

func (gte *GreaterThanOrEqual) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("GreaterThanOrEqual")
	children := []string{sql.DebugString(gte.Left()), sql.DebugString(gte.Right())}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

// LessThanOrEqual is a comparison that checks an expression is equal or lower than
// another.
type LessThanOrEqual struct {
	comparison
}

var _ sql.Expression = (*LessThanOrEqual)(nil)
var _ sql.CollationCoercible = (*LessThanOrEqual)(nil)

// NewLessThanOrEqual creates a LessThanOrEqual expression.
func NewLessThanOrEqual(left sql.Expression, right sql.Expression) *LessThanOrEqual {
	return &LessThanOrEqual{newComparison(left, right)}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (lte *LessThanOrEqual) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (lte *LessThanOrEqual) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := lte.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result < 1, nil
}

// WithChildren implements the Expression interface.
func (lte *LessThanOrEqual) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(lte, len(children), 2)
	}
	return NewLessThanOrEqual(children[0], children[1]), nil
}

func (lte *LessThanOrEqual) String() string {
	return fmt.Sprintf("(%s <= %s)", lte.Left(), lte.Right())
}

func (lte *LessThanOrEqual) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("LessThanOrEqual")
	children := []string{sql.DebugString(lte.Left()), sql.DebugString(lte.Right())}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

var (
	// ErrUnsupportedInOperand is returned when there is an invalid righthand
	// operand in an IN operator.
	ErrUnsupportedInOperand = errors.NewKind("right operand in IN operation must be tuple, but is %T")
)

// ConstantExpr reports whether e has the same value for every row (constantExpr; HeimDB).
func ConstantExpr(e sql.Expression) bool {
	return constantExpr(e)
}

func isEnumOrSet(t sql.Type) bool {
	return types.IsEnum(t) || types.IsSet(t)
}

// enumSetAsText は、ENUM か SET の値を値の文字列で比べるか（HeimDB、段階 116 の 3 度目のレビュー）。MySQL は ENUM と
// SET を文字列と比べるとき、定義の違う ENUM か SET どうしを比べるときは値の文字列で比べる。定数の文字列
// （e = 'a'）は、索引の範囲（convertKey）と同じく EnumType.Compare で比べる（数の形の '1' を番号として読むのは前の
// まま）。
func enumSetAsText(lt, rt sql.Type, l, r sql.Expression) bool {
	le, re := isEnumOrSet(lt), isEnumOrSet(rt)
	switch {
	case le && re:
		return !lt.Equals(rt)
	case le:
		return types.IsText(rt) && !constantExpr(r)
	case re:
		return types.IsText(lt) && !constantExpr(l)
	}
	return false
}

// CompareEnumSetAsText は、ENUM か SET の値（番号かビット）を値の文字列にして、もう一方の値の文字列と照合順序 coll で
// 比べる（HeimDB、段階 116 の 3 度目のレビュー）。
func CompareEnumSetAsText(ctx *sql.Context, coll sql.CollationID, lt, rt sql.Type, left, right interface{}) (int, error) {
	ls, err := enumSetText(ctx, lt, left)
	if err != nil {
		return 0, err
	}
	rs, err := enumSetText(ctx, rt, right)
	if err != nil {
		return 0, err
	}
	if coll != sql.Collation_binary && coll.PadAttribute() == "PAD SPACE" {
		// 文字列の比べは後ろの空白を見る（PAD SPACE は既知の違い）が、ENUM と SET は MySQL と同じく PAD SPACE で比べる
		// （EnumType.ConvertToCompare と同じ）
		ls, rs = strings.TrimRight(ls, " "), strings.TrimRight(rs, " ")
	}
	if coll == sql.Collation_binary {
		switch {
		case ls < rs:
			return -1, nil
		case ls > rs:
			return 1, nil
		}
		return 0, nil
	}
	return types.CreateLongText(coll).Compare(ctx, ls, rs)
}

func enumSetText(ctx *sql.Context, typ sql.Type, v interface{}) (string, error) {
	if s, ok := types.ValueString(typ, v); ok {
		return s, nil
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case []byte:
		return string(x), nil
	}
	s, _, err := types.LongText.Convert(ctx, v)
	if err != nil {
		return "", err
	}
	str, _ := s.(string)
	return str, nil
}

func isRealComparand(e sql.Expression) bool {
	_, ok := e.(*RealComparand)
	return ok
}

// TupleEquals compares two rows of values as MySQL's =: false if a pair of
// values that are not NULL differs, NULL if none differs but one is NULL, and
// true otherwise ((1, NULL) = (1, NULL) is NULL; it was true, and
// (1, NULL) IN ((1, 2)) failed with 1105) (HeimDB, stage 116).
func TupleEquals(ctx *sql.Context, lt, rt sql.Type, l, r []interface{}) (interface{}, error) {
	if len(l) != len(r) {
		return nil, sql.ErrInvalidOperandColumns.New(len(l), len(r))
	}
	lts, _ := lt.(types.TupleType)
	rts, _ := rt.(types.TupleType)
	hasNull := false
	for i := range l {
		if l[i] == nil || r[i] == nil {
			hasNull = true
			continue
		}
		var a, b sql.Expression
		if i < len(lts) && i < len(rts) {
			a, b = NewLiteral(l[i], lts[i]), NewLiteral(r[i], rts[i])
		} else {
			a, b = NewLiteral(l[i], types.ApproximateTypeFromValue(l[i])), NewLiteral(r[i], types.ApproximateTypeFromValue(r[i]))
		}
		res, err := NewEquals(a, b).Eval(ctx, nil)
		if err != nil {
			return nil, err
		}
		if res == nil {
			hasNull = true
		} else if res == false {
			return false, nil
		}
	}
	if hasNull {
		return nil, nil
	}
	return true, nil
}
