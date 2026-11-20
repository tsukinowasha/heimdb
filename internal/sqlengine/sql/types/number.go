// Copyright 2022 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 文字列を数にするとき、先頭の数の部分を読んだ値を、変換の誤り（ErrInvalidValue）と一緒に返す（docs/engine.md）
// Modified by Tsukinowa Inc.: DOUBLE と FLOAT の値を MySQL と同じ形の文字列にする（docs/engine.md）
// Modified by Tsukinowa Inc.: 符号なしの整数を小さい符号なしの型にするとき、範囲の外を端の値と OutOfRange にする（上流の #3103。docs/engine.md）
// Modified by Tsukinowa Inc.: 整数にする文字列の指数の数字のない '1E' を 1 として読み、DOUBLE を超える文字列を端の値にする（上流の #3920。docs/engine.md）
// Modified by Tsukinowa Inc.: 'nan'、'inf'、16 進の浮動小数点数の文字列を数として読まない（MySQL と同じ。段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: バイナリプロトコルでは DOUBLE の無限大と NaN を値のまま送る（段階 116 の 3 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT と DOUBLE の型が (M,D) と UNSIGNED と式の小数の桁を持ち、D 桁で表す（段階 120。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の列に入れる値を MySQL と同じく丸めて範囲を確かめる（段階 120。docs/engine.md）

package types

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/values"
)

var (
	// Boolean is a synonym for TINYINT(1)
	Boolean = MustCreateNumberTypeWithDisplayWidth(sqltypes.Int8, 1)
	// Int8 is an integer of 8 bits
	Int8 = MustCreateNumberType(sqltypes.Int8)
	// Uint8 is an unsigned integer of 8 bits
	Uint8 = MustCreateNumberType(sqltypes.Uint8)
	// Int16 is an integer of 16 bits
	Int16 = MustCreateNumberType(sqltypes.Int16)
	// Uint16 is an unsigned integer of 16 bits
	Uint16 = MustCreateNumberType(sqltypes.Uint16)
	// Int24 is an integer of 24 bits.
	Int24 = MustCreateNumberType(sqltypes.Int24)
	// Uint24 is an unsigned integer of 24 bits.
	Uint24 = MustCreateNumberType(sqltypes.Uint24)
	// Int32 is an integer of 32 bits.
	Int32 = MustCreateNumberType(sqltypes.Int32)
	// Uint32 is an unsigned integer of 32 bits.
	Uint32 = MustCreateNumberType(sqltypes.Uint32)
	// Int64 is an integer of 64 bytes.
	Int64 = MustCreateNumberType(sqltypes.Int64)
	// Uint64 is an unsigned integer of 64 bits.
	Uint64 = MustCreateNumberType(sqltypes.Uint64)
	// Float32 is a floating point number of 32 bits.
	Float32 = MustCreateNumberType(sqltypes.Float32)
	// Float64 is a floating point number of 64 bits.
	Float64 = MustCreateNumberType(sqltypes.Float64)

	// decimal that represents the max value an uint64 can hold
	dec_uint64_max = decimal.NewFromInt(math.MaxInt64).Mul(decimal.NewFromInt(2).Add(decimal.NewFromInt(1)))
	dec_uint32_max = decimal.NewFromInt(math.MaxInt32).Mul(decimal.NewFromInt(2).Add(decimal.NewFromInt(1)))
	dec_uint16_max = decimal.NewFromInt(math.MaxInt16).Mul(decimal.NewFromInt(2).Add(decimal.NewFromInt(1)))
	dec_uint8_max  = decimal.NewFromInt(math.MaxInt8).Mul(decimal.NewFromInt(2).Add(decimal.NewFromInt(1)))
	// decimal that represents the max value an int64 can hold
	dec_int64_max = decimal.NewFromInt(math.MaxInt64)
	// decimal that represents the min value an int64 can hold
	dec_int64_min = decimal.NewFromInt(math.MinInt64)
	// decimal that represents the zero value
	dec_zero = decimal.NewFromInt(0)

	numberInt8ValueType    = reflect.TypeOf(int8(0))
	numberInt16ValueType   = reflect.TypeOf(int16(0))
	numberInt32ValueType   = reflect.TypeOf(int32(0))
	numberInt64ValueType   = reflect.TypeOf(int64(0))
	numberUint8ValueType   = reflect.TypeOf(uint8(0))
	numberUint16ValueType  = reflect.TypeOf(uint16(0))
	numberUint32ValueType  = reflect.TypeOf(uint32(0))
	numberUint64ValueType  = reflect.TypeOf(uint64(0))
	numberFloat32ValueType = reflect.TypeOf(float32(0))
	numberFloat64ValueType = reflect.TypeOf(float64(0))
)

const (
	// intCutSet is the set of characters that should be trimmed from the beginning and end of a string
	//   when converting to a signed or unsigned integer
	intCutSet = " \t"

	// numericCutSet is the set of characters to trim from a string before converting it to a number.
	numericCutSet = " \t\n\r"
)

type NumberTypeImpl_ struct {
	baseType     query.Type
	displayWidth int
	// HeimDB: FLOAT と DOUBLE の、宣言した (M,D) の M（0 なら宣言なし）、小数の桁 D と、桁が決まっているか（宣言した
	// (M,D) と、その列から求めた式の型。MySQL の decimals が NOT_FIXED_DEC でないもの）、UNSIGNED（段階 120）。
	// 宣言した型だけが書き込みで丸めて範囲を確かめ（Convert）、式の型は表示（SQL）と列の情報の桁だけに使う。
	precision int
	scale     int
	fixed     bool
	unsigned  bool
}

var _ sql.Type = NumberTypeImpl_{}
var _ sql.Type2 = NumberTypeImpl_{}
var _ sql.CollationCoercible = NumberTypeImpl_{}
var _ sql.NumberType = NumberTypeImpl_{}

// CreateNumberType creates a NumberType.
func CreateNumberType(baseType query.Type) (sql.NumberType, error) {
	return CreateNumberTypeWithDisplayWidth(baseType, 0)
}

// CreateNumberTypeWithDisplayWidth creates a NumberType that includes optional |displayWidth| metadata. Note that
// MySQL only allows a |displayWidth| of 1 for Int8 (i.e. TINYINT(1)); any other combination of |displayWidth| and
// |baseType| is not supported and will cause this function to return an error.
func CreateNumberTypeWithDisplayWidth(baseType query.Type, displayWidth int) (sql.NumberType, error) {
	switch baseType {
	case sqltypes.Int8, sqltypes.Uint8, sqltypes.Int16, sqltypes.Uint16, sqltypes.Int24, sqltypes.Uint24,
		sqltypes.Int32, sqltypes.Uint32, sqltypes.Int64, sqltypes.Uint64, sqltypes.Float32, sqltypes.Float64:

		// displayWidth of 0 is valid for all types, displayWidth of 1 is only valid for Int8
		if displayWidth == 0 || (displayWidth == 1 && baseType == sqltypes.Int8) {
			return NumberTypeImpl_{
				baseType:     baseType,
				displayWidth: displayWidth,
			}, nil
		}
		return nil, fmt.Errorf("display width of %d is not valid for type %s", displayWidth, baseType.String())
	}
	return nil, fmt.Errorf("%v is not a valid number base type", baseType.String())
}

// RealMaxPrecision と RealMaxScale は、FLOAT(M,D) と DOUBLE(M,D) の M と D の上限（MySQL と同じ。HeimDB: 段階 120）。
const (
	RealMaxPrecision = 255
	RealMaxScale     = 30
)

// CreateRealType は、FLOAT か DOUBLE の、宣言した (M,D) を持つ型を作る。precision が 0 なら桁のない型
// （FLOAT UNSIGNED など）。M と D の範囲は呼び出し側が確かめる（HeimDB: 段階 120）。
func CreateRealType(baseType query.Type, precision, scale int, unsigned bool) sql.NumberType {
	if baseType != sqltypes.Float32 && baseType != sqltypes.Float64 {
		panic(fmt.Sprintf("%v is not a floating point type", baseType.String()))
	}
	return NumberTypeImpl_{baseType: baseType, precision: precision, scale: scale, fixed: precision > 0, unsigned: unsigned}
}

// RealResultType は、小数の桁が決まった DOUBLE の式の型を作る（f + 1 の FLOAT(7,2) の f なら 2 桁。HeimDB: 段階 120）。
// 表示と列の情報の桁だけに使い、値は丸めない。scale が RealMaxScale を超えるなら、MySQL と同じく桁の決まらない DOUBLE。
func RealResultType(scale int) sql.Type {
	if scale > RealMaxScale {
		return Float64
	}
	return NumberTypeImpl_{baseType: sqltypes.Float64, scale: scale, fixed: true}
}

// RealExprType は、元の型 baseType（FLOAT か DOUBLE）で小数の桁 scale の式の型を作る（値は丸めない。HeimDB: 段階 120）。
func RealExprType(baseType query.Type, scale int) sql.Type {
	return NumberTypeImpl_{baseType: baseType, scale: scale, fixed: true}
}

// RealScale は、t が小数の桁の決まった FLOAT か DOUBLE（宣言した (M,D) か、その列から求めた式）なら、その桁を返す。
func RealScale(t sql.Type) (int, bool) {
	if n, ok := t.(NumberTypeImpl_); ok && n.fixed {
		return n.scale, true
	}
	return 0, false
}

// RealPrecision は、t が (M,D) を宣言した FLOAT か DOUBLE の列の型なら、M と D を返す（式の型は false）。
func RealPrecision(t sql.Type) (precision, scale int, ok bool) {
	if n, ok := t.(NumberTypeImpl_); ok && n.precision > 0 {
		return n.precision, n.scale, true
	}
	return 0, 0, false
}

// RealUnsigned は、t が UNSIGNED の FLOAT か DOUBLE の列の型かを返す。
func RealUnsigned(t sql.Type) bool {
	n, ok := t.(NumberTypeImpl_)
	return ok && n.unsigned
}

// RealDecimals は、式の小数の桁を MySQL の decimals と同じく求める（HeimDB: 段階 120）。桁の決まった FLOAT と
// DOUBLE はその桁、DECIMAL はスケール、整数とビットは 0。桁のない FLOAT と DOUBLE、文字列、そのほかの型は
// fixed が false（MySQL の NOT_FIXED_DEC）。real は、t が桁の決まった FLOAT か DOUBLE か。
func RealDecimals(t sql.Type) (decimals int, fixed, real bool) {
	if n, ok := t.(NumberTypeImpl_); ok {
		switch n.baseType {
		case sqltypes.Float32, sqltypes.Float64:
			return n.scale, n.fixed, n.fixed
		}
		return 0, true, false
	}
	if d, ok := t.(sql.DecimalType); ok {
		return int(d.Scale()), true, false
	}
	if IsBit(t) {
		return 0, true, false
	}
	return 0, false, false
}

// RealResultOf は、引数の型 args の DOUBLE の式（+、-、*、%、IF、COALESCE など）の型を返す。引数のどれかが桁の
// 決まった FLOAT か DOUBLE で、ほかもすべて桁が決まっていれば、桁の最大に inc を足した桁の型（/ は 4）、そうでなければ
// 桁の決まらない Float64（前と同じ。HeimDB: 段階 120）。
func RealResultOf(inc int, args ...sql.Type) sql.Type {
	maxDec, anyReal := 0, false
	for _, a := range args {
		if a == nil || a == Null {
			continue
		}
		d, fixed, real := RealDecimals(a)
		if !fixed {
			return Float64
		}
		anyReal = anyReal || real
		maxDec = max(maxDec, d)
	}
	if !anyReal {
		return Float64
	}
	return RealResultType(maxDec + inc)
}

// MustCreateNumberType is the same as CreateNumberType except it panics on errors.
func MustCreateNumberType(baseType query.Type) sql.NumberType {
	nt, err := CreateNumberType(baseType)
	if err != nil {
		panic(err)
	}
	return nt
}

// MustCreateNumberTypeWithDisplayWidth is the same as CreateNumberTypeWithDisplayWidth except it panics on errors.
func MustCreateNumberTypeWithDisplayWidth(baseType query.Type, displayWidth int) sql.NumberType {
	nt, err := CreateNumberTypeWithDisplayWidth(baseType, displayWidth)
	if err != nil {
		panic(err)
	}
	return nt
}

func NumericUnaryValue(t sql.Type) interface{} {
	nt := t.(NumberTypeImpl_)
	switch nt.baseType {
	case sqltypes.Int8:
		return int8(1)
	case sqltypes.Uint8:
		return uint8(1)
	case sqltypes.Int16:
		return int16(1)
	case sqltypes.Uint16:
		return uint16(1)
	case sqltypes.Int24:
		return int32(1)
	case sqltypes.Uint24:
		return uint32(1)
	case sqltypes.Int32:
		return int32(1)
	case sqltypes.Uint32:
		return uint32(1)
	case sqltypes.Int64:
		return int64(1)
	case sqltypes.Uint64:
		return uint64(1)
	case sqltypes.Float32:
		return float32(1)
	case sqltypes.Float64:
		return float64(1)
	default:
		panic(fmt.Sprintf("%v is not a valid number base type", nt.baseType.String()))
	}
}

// Compare implements Type interface.
func (t NumberTypeImpl_) Compare(s context.Context, a interface{}, b interface{}) (int, error) {
	if hasNulls, res := CompareNulls(a, b); hasNulls {
		return res, nil
	}

	switch t.baseType {
	case sqltypes.Uint8, sqltypes.Uint16, sqltypes.Uint24, sqltypes.Uint32, sqltypes.Uint64:
		ca, _, err := convertToUint64(t, a)
		if err != nil {
			return 0, err
		}
		cb, _, err := convertToUint64(t, b)
		if err != nil {
			return 0, err
		}

		if ca == cb {
			return 0, nil
		}
		if ca < cb {
			return -1, nil
		}
		return +1, nil
	case sqltypes.Float32, sqltypes.Float64:
		ca, err := convertToFloat64(t, a)
		if err != nil {
			return 0, err
		}
		cb, err := convertToFloat64(t, b)
		if err != nil {
			return 0, err
		}

		if ca == cb {
			return 0, nil
		}
		if ca < cb {
			return -1, nil
		}
		return +1, nil
	default:
		ca, _, err := convertToInt64(t, a)
		if err != nil {
			ca = 0
		}
		cb, _, err := convertToInt64(t, b)
		if err != nil {
			cb = 0
		}

		if ca == cb {
			return 0, nil
		}
		if ca < cb {
			return -1, nil
		}
		return +1, nil
	}
}

// Convert implements Type interface.
func (t NumberTypeImpl_) Convert(c context.Context, v interface{}) (interface{}, sql.ConvertInRange, error) {
	var err error
	if v == nil {
		return nil, sql.InRange, nil
	}

	if ti, ok := v.(time.Time); ok {
		v = ti.UTC().Unix()
	}

	if jv, ok := v.(sql.JSONWrapper); ok {
		v, err = jv.ToInterface()
		if err != nil {
			return nil, sql.OutOfRange, err
		}
	}

	switch t.baseType {
	case sqltypes.Int8:
		num, _, err := convertToInt64(t, v)
		if err != nil && !truncatedString(v, err) {
			return nil, sql.OutOfRange, err
		}
		if num > math.MaxInt8 {
			return int8(math.MaxInt8), sql.OutOfRange, err
		} else if num < math.MinInt8 {
			return int8(math.MinInt8), sql.OutOfRange, err
		}
		return int8(num), sql.InRange, err
	case sqltypes.Uint8:
		return convertToUint8(t, v)
	case sqltypes.Int16:
		num, _, err := convertToInt64(t, v)
		if err != nil && !truncatedString(v, err) {
			return nil, sql.OutOfRange, err
		}
		if num > math.MaxInt16 {
			return int16(math.MaxInt16), sql.OutOfRange, err
		} else if num < math.MinInt16 {
			return int16(math.MinInt16), sql.OutOfRange, err
		}
		return int16(num), sql.InRange, err
	case sqltypes.Uint16:
		return convertToUint16(t, v)
	case sqltypes.Int24:
		num, _, err := convertToInt64(t, v)
		if err != nil && !truncatedString(v, err) {
			return nil, sql.OutOfRange, err
		}
		if num > (1<<23 - 1) {
			return int32(1<<23 - 1), sql.OutOfRange, err
		} else if num < (-1 << 23) {
			return int32(-1 << 23), sql.OutOfRange, err
		}
		return int32(num), sql.InRange, err
	case sqltypes.Uint24:
		num, _, err := convertToInt64(t, v)
		if err != nil && !truncatedString(v, err) {
			return nil, sql.OutOfRange, err
		}
		if num >= (1 << 24) {
			return uint32(1<<24 - 1), sql.OutOfRange, err
		} else if num < 0 {
			return uint32(1<<24 - int32(-num)), sql.OutOfRange, err
		}
		return uint32(num), sql.InRange, err
	case sqltypes.Int32:
		num, _, err := convertToInt64(t, v)
		if err != nil && !truncatedString(v, err) {
			return nil, sql.OutOfRange, err
		}
		if num > math.MaxInt32 {
			return int32(math.MaxInt32), sql.OutOfRange, err
		} else if num < math.MinInt32 {
			return int32(math.MinInt32), sql.OutOfRange, err
		}
		return int32(num), sql.InRange, err
	case sqltypes.Uint32:
		return convertToUint32(t, v)
	case sqltypes.Int64:
		return convertToInt64(t, v)
	case sqltypes.Uint64:
		return convertToUint64(t, v)
	case sqltypes.Float32:
		num, err := convertToFloat64(t, v)
		if err != nil && !truncatedString(v, err) {
			return nil, sql.OutOfRange, err
		}
		if t.precision > 0 || t.unsigned {
			// HeimDB: FLOAT(M,D) と FLOAT UNSIGNED は MySQL と同じく丸めて範囲を確かめ、その後に単精度にする（段階 120）
			_, single := v.(float32)
			r, inRange := t.truncateReal(num, math.MaxFloat32, single)
			return float32(r), inRange, err
		}
		if num > math.MaxFloat32 {
			return float32(math.MaxFloat32), sql.OutOfRange, err
		} else if num < -math.MaxFloat32 {
			return float32(-math.MaxFloat32), sql.OutOfRange, err
		}
		return float32(num), sql.InRange, err
	case sqltypes.Float64:
		ret, err := convertToFloat64(t, v)
		if (t.precision > 0 || t.unsigned) && (err == nil || truncatedString(v, err)) {
			// HeimDB: DOUBLE(M,D) と DOUBLE UNSIGNED は MySQL と同じく丸めて範囲を確かめる（段階 120）
			r, inRange := t.truncateReal(ret, math.MaxFloat64, false)
			return r, inRange, err
		}
		return ret, sql.InRange, err
	default:
		return nil, sql.OutOfRange, sql.ErrInvalidType.New(t.baseType.String())
	}
}

// truncateReal は、FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の列に入れる値 nr を、MySQL の Field_real::truncate と同じく丸めて
// 範囲を確かめる（HeimDB: 段階 120）。UNSIGNED の負の値は 0、D 桁は floor(nr) + rint((nr - floor(nr)) * 10^D) / 10^D、
// 上限は 10^(M-D) - 10^-D（(M,D) がなければ max）で、MySQL と同じく丸めた倍精度の値で比べ、範囲の外は端の値と
// OutOfRange（FLOAT(10,7) の 1000 と 999.99999999 は範囲の外）。single は、FLOAT の列に単精度の値（ほかの FLOAT の列の
// 値。MySQL は同じ型の列どうしを丸めずに写す）を入れるときで、単精度にした値で比べる（保存した端の値 1000 を写せる）。
func (t NumberTypeImpl_) truncateReal(nr, max float64, single bool) (float64, sql.ConvertInRange) {
	if math.IsNaN(nr) {
		return 0, sql.OutOfRange
	}
	if t.unsigned && nr < 0 {
		return 0, sql.OutOfRange
	}
	if t.precision > 0 {
		scale := math.Pow10(t.scale)
		max = math.Pow10(t.precision-t.scale) - 1/scale
		if !math.IsInf(nr, 0) {
			f := math.Floor(nr)
			nr = f + math.RoundToEven((nr-f)*scale)/scale
		}
	}
	if single {
		if float32(nr) < float32(-max) {
			return -max, sql.OutOfRange
		} else if float32(nr) > float32(max) {
			return max, sql.OutOfRange
		}
		return nr, sql.InRange
	}
	if nr < -max {
		return -max, sql.OutOfRange
	} else if nr > max {
		return max, sql.OutOfRange
	}
	return nr, sql.InRange
}

// RealKeyChanged は、結合の索引の lookup のキー orig を FLOAT か DOUBLE の列の型 typ にした値 conv が、元の値と倍精度で
// 違うかを返す（DOUBLE のキーを FLOAT の列にすると単精度に、DOUBLE(10,3) の列にすると 3 桁に変わる）。違うキーは、索引の
// ない比べ（倍精度）と同じくどの行にも合わない（HeimDB: 段階 120 の 2 回目のレビュー）。文字列のキーと、数にできない
// キーは false（呼び出し側の前からの扱い）。
func RealKeyChanged(ctx context.Context, typ sql.Type, orig, conv interface{}) bool {
	if !IsFloat(typ) || orig == nil || conv == nil {
		return false
	}
	switch orig.(type) {
	case string, []byte:
		return false
	}
	a, _, err := Float64.Convert(ctx, orig)
	if err != nil {
		return false
	}
	b, _, err := Float64.Convert(ctx, conv)
	if err != nil {
		return false
	}
	return a.(float64) != b.(float64)
}

// KeyType は、索引の範囲の端の値を変換する型（HeimDB: 段階 120）。FLOAT(M,D) と DOUBLE(M,D) と UNSIGNED は、丸めず範囲も
// 確かめない元の型で変換する（f > 1.555 の端を 1.56 に丸めると、1.56 の行を落とす）。
func (t NumberTypeImpl_) KeyType() sql.Type {
	if t.precision > 0 || t.unsigned {
		return NumberTypeImpl_{baseType: t.baseType}
	}
	return t
}

// MaxTextResponseByteLength implements the Type interface
func (t NumberTypeImpl_) MaxTextResponseByteLength(*sql.Context) uint32 {
	// MySQL integer type limits: https://dev.mysql.com/doc/refman/8.0/en/integer-types.html
	// This is for a text response format, NOT a binary encoding
	switch t.baseType {
	case sqltypes.Uint8:
		return 3
	case sqltypes.Int8:
		return 4
	case sqltypes.Uint16:
		return 5
	case sqltypes.Int16:
		return 6
	case sqltypes.Uint24:
		return 8
	case sqltypes.Int24:
		return 9
	case sqltypes.Uint32:
		return 10
	case sqltypes.Int32:
		return 11
	case sqltypes.Uint64:
		return 20
	case sqltypes.Int64:
		return 20
	case sqltypes.Float32:
		return 12
	case sqltypes.Float64:
		return 22
	default:
		panic(fmt.Sprintf("%v is not a valid number base type", t.baseType.String()))
	}
}

// Equals implements the Type interface.
func (t NumberTypeImpl_) Equals(otherType sql.Type) bool {
	// HeimDB: FLOAT と DOUBLE の (M,D) と UNSIGNED も比べる（ALTER の MODIFY で桁を変えると、行を書き直す。段階 120）
	if o, ok := otherType.(NumberTypeImpl_); ok {
		return t.baseType == o.baseType && t.precision == o.precision && t.scale == o.scale && t.fixed == o.fixed && t.unsigned == o.unsigned
	}
	return t.baseType == otherType.Type()
}

// Promote implements the Type interface.
func (t NumberTypeImpl_) Promote() sql.Type {
	switch t.baseType {
	case sqltypes.Int8, sqltypes.Int16, sqltypes.Int24, sqltypes.Int32, sqltypes.Int64:
		return Int64
	case sqltypes.Uint8, sqltypes.Uint16, sqltypes.Uint24, sqltypes.Uint32, sqltypes.Uint64:
		return Uint64
	case sqltypes.Float32, sqltypes.Float64:
		return Float64
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "number"))
	}
}

func (t NumberTypeImpl_) SQLInt8(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToInt64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxInt8 {
		num = int64(math.MaxInt8)
	} else if num < math.MinInt8 {
		num = int64(math.MinInt8)
	}
	dest = strconv.AppendInt(dest, num, 10)
	return dest, nil
}

func (t NumberTypeImpl_) SQLInt16(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToInt64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxInt16 {
		num = int64(math.MaxInt16)
	} else if num < math.MinInt16 {
		num = int64(math.MinInt16)
	}
	dest = strconv.AppendInt(dest, num, 10)
	return dest, nil
}

func (t NumberTypeImpl_) SQLInt24(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToInt64(t, v)
	if err != nil {
		return nil, err
	}
	if num > (1<<23 - 1) {
		num = int64(1<<23 - 1)
	} else if num < (-1 << 23) {
		num = int64(-1 << 23)
	}
	dest = strconv.AppendInt(dest, num, 10)
	return dest, nil
}

func (t NumberTypeImpl_) SQLInt32(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToInt64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxInt32 {
		num = math.MaxInt32
	} else if num < math.MinInt32 {
		num = math.MinInt32
	}
	dest = strconv.AppendInt(dest, num, 10)
	return dest, nil
}

func (t NumberTypeImpl_) SQLInt64(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	vt, _, err := convertToInt64(t, v)
	if err != nil {
		return nil, err
	}
	dest = strconv.AppendInt(dest, vt, 10)
	return dest, nil
}

func (t NumberTypeImpl_) SQLUint8(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToUint64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxUint8 {
		num = uint64(math.MaxUint8)
	}
	dest = strconv.AppendUint(dest, num, 10)

	return dest, nil
}

func (t NumberTypeImpl_) SQLUint16(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToUint64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxUint16 {
		num = uint64(math.MaxUint16)
	}
	dest = strconv.AppendUint(dest, num, 10)

	return dest, nil
}

func (t NumberTypeImpl_) SQLUint24(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToUint64(t, v)
	if err != nil {
		return nil, err
	}
	if num > (1 << 24) {
		num = uint64((1 << 24))
	}
	dest = strconv.AppendUint(dest, num, 10)

	return dest, nil
}

func (t NumberTypeImpl_) SQLUint32(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToUint64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxUint32 {
		num = uint64(math.MaxUint32)
	}
	dest = strconv.AppendUint(dest, num, 10)

	return dest, nil
}

func (t NumberTypeImpl_) SQLUint64(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, _, err := convertToUint64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxUint64 {
		num = uint64(math.MaxUint64)
	}
	dest = strconv.AppendUint(dest, num, 10)

	return dest, nil
}

func (t NumberTypeImpl_) SQLFloat64(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, err := convertToFloat64(t, v)
	if err != nil {
		return nil, err
	}
	// HeimDB: バイナリプロトコルでは、範囲を超えた値（無限大と NaN）を MySQL と同じく IEEE の値のまま送る。vitess が
	// この文字列を strconv.ParseFloat で読み直す（テキストプロトコルは MySQL と同じく '0'。段階 116 の 3 度目のレビュー）
	if (math.IsInf(num, 0) || math.IsNaN(num)) && sql.IsBinaryProtocol(ctx) {
		return strconv.AppendFloat(dest, num, 'g', -1, 64), nil
	}
	if t.fixed && !math.IsInf(num, 0) && !math.IsNaN(num) {
		// HeimDB: 小数の桁が決まった DOUBLE は、MySQL と同じくその桁で表す（段階 120）
		return strconv.AppendFloat(dest, num, 'f', t.scale, 64), nil
	}
	// HeimDB: MySQL と同じ形にする（20250331、0.00000015、1e15。float_format.go）
	dest = AppendMySQLDouble(dest, num, 0)
	return dest, nil
}

func (t NumberTypeImpl_) SQLFloat32(ctx *sql.Context, dest []byte, v interface{}) ([]byte, error) {
	num, err := convertToFloat64(t, v)
	if err != nil {
		return nil, err
	}
	if num > math.MaxFloat32 {
		num = float64(math.MaxFloat32)
	} else if num < -math.MaxFloat32 {
		num = float64(-math.MaxFloat32)
	}
	if t.fixed {
		// HeimDB: FLOAT(M,D) は、MySQL と同じく単精度の値を D 桁で表す（段階 120。FLOAT(10,7) の 35.6812363）
		return strconv.AppendFloat(dest, float64(float32(num)), 'f', t.scale, 64), nil
	}
	// HeimDB: MySQL と同じ形にする（6 桁。float_format.go）
	dest = AppendMySQLFloat(dest, float32(num), 0)
	return dest, nil
}

// SQL implements Type interface.
func (t NumberTypeImpl_) SQL(ctx *sql.Context, dest []byte, v interface{}) (sqltypes.Value, error) {
	if v == nil {
		return sqltypes.NULL, nil
	}

	var err error
	if jv, ok := v.(sql.JSONWrapper); ok {
		v, err = jv.ToInterface()
		if err != nil {
			return sqltypes.Value{}, err
		}
	}

	stop := len(dest)

	switch t.baseType {
	case sqltypes.Int8:
		dest, err = t.SQLInt8(ctx, dest, v)
	case sqltypes.Int16:
		dest, err = t.SQLInt16(ctx, dest, v)
	case sqltypes.Int24:
		dest, err = t.SQLInt24(ctx, dest, v)
	case sqltypes.Int32:
		dest, err = t.SQLInt32(ctx, dest, v)
	case sqltypes.Int64:
		dest, err = t.SQLInt64(ctx, dest, v)
	case sqltypes.Uint8:
		dest, err = t.SQLUint8(ctx, dest, v)
	case sqltypes.Uint16:
		dest, err = t.SQLUint16(ctx, dest, v)
	case sqltypes.Uint24:
		dest, err = t.SQLUint24(ctx, dest, v)
	case sqltypes.Uint32:
		dest, err = t.SQLUint32(ctx, dest, v)
	case sqltypes.Uint64:
		dest, err = t.SQLUint64(ctx, dest, v)
	case sqltypes.Float32:
		dest, err = t.SQLFloat32(ctx, dest, v)
	case sqltypes.Float64:
		dest, err = t.SQLFloat64(ctx, dest, v)
	default:
		return sqltypes.Value{}, sql.ErrInvalidType.New(t.baseType.String())
	}

	if sql.ErrInvalidValue.Is(err) {
		switch str := v.(type) {
		case []byte:
			dest = str
		case string:
			dest = []byte(str)
		default:
			return sqltypes.Value{}, err
		}
	} else if err != nil {
		return sqltypes.Value{}, err
	}

	val := dest[stop:]
	return sqltypes.MakeTrusted(t.baseType, val), nil
}

func (t NumberTypeImpl_) Compare2(a sql.Value, b sql.Value) (int, error) {
	switch t.baseType {
	case sqltypes.Uint8, sqltypes.Uint16, sqltypes.Uint24, sqltypes.Uint32, sqltypes.Uint64:
		ca, err := convertValueToUint64(t, a)
		if err != nil {
			return 0, err
		}
		cb, err := convertValueToUint64(t, b)
		if err != nil {
			return 0, err
		}

		if ca == cb {
			return 0, nil
		}
		if ca < cb {
			return -1, nil
		}
		return +1, nil
	case sqltypes.Float32, sqltypes.Float64:
		ca, err := convertValueToFloat64(t, a)
		if err != nil {
			return 0, err
		}
		cb, err := convertValueToFloat64(t, b)
		if err != nil {
			return 0, err
		}

		if ca == cb {
			return 0, nil
		}
		if ca < cb {
			return -1, nil
		}
		return +1, nil
	default:
		ca, err := convertValueToInt64(t, a)
		if err != nil {
			return 0, err
		}
		cb, err := convertValueToInt64(t, b)
		if err != nil {
			return 0, err
		}

		if ca == cb {
			return 0, nil
		}
		if ca < cb {
			return -1, nil
		}
		return +1, nil
	}
}

func (t NumberTypeImpl_) Convert2(value sql.Value) (sql.Value, error) {
	panic("implement me")
}

func (t NumberTypeImpl_) Zero2() sql.Value {
	switch t.baseType {
	case sqltypes.Int8:
		x := values.WriteInt8(make([]byte, values.Int8Size), 0)
		return sql.Value{
			Typ: query.Type_INT8,
			Val: x,
		}
	case sqltypes.Int16:
		x := values.WriteInt16(make([]byte, values.Int16Size), 0)
		return sql.Value{
			Typ: query.Type_INT16,
			Val: x,
		}
	case sqltypes.Int24:
		x := values.WriteInt24(make([]byte, values.Int24Size), 0)
		return sql.Value{
			Typ: query.Type_INT24,
			Val: x,
		}
	case sqltypes.Int32:
		x := values.WriteInt32(make([]byte, values.Int32Size), 0)
		return sql.Value{
			Typ: query.Type_INT32,
			Val: x,
		}
	case sqltypes.Int64:
		x := values.WriteInt64(make([]byte, values.Int64Size), 0)
		return sql.Value{
			Typ: query.Type_INT64,
			Val: x,
		}
	case sqltypes.Uint8:
		x := values.WriteUint8(make([]byte, values.Uint8Size), 0)
		return sql.Value{
			Typ: query.Type_UINT8,
			Val: x,
		}
	case sqltypes.Uint16:
		x := values.WriteUint16(make([]byte, values.Uint16Size), 0)
		return sql.Value{
			Typ: query.Type_UINT16,
			Val: x,
		}
	case sqltypes.Uint24:
		x := values.WriteUint24(make([]byte, values.Uint24Size), 0)
		return sql.Value{
			Typ: query.Type_UINT24,
			Val: x,
		}
	case sqltypes.Uint32:
		x := values.WriteUint32(make([]byte, values.Uint32Size), 0)
		return sql.Value{
			Typ: query.Type_UINT32,
			Val: x,
		}
	case sqltypes.Uint64:
		x := values.WriteUint64(make([]byte, values.Uint64Size), 0)
		return sql.Value{
			Typ: query.Type_UINT64,
			Val: x,
		}
	case sqltypes.Float32:
		x := values.WriteFloat32(make([]byte, values.Float32Size), 0)
		return sql.Value{
			Typ: query.Type_FLOAT32,
			Val: x,
		}
	case sqltypes.Float64:
		x := values.WriteUint64(make([]byte, values.Uint64Size), 0)
		return sql.Value{
			Typ: query.Type_UINT64,
			Val: x,
		}
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "number"))
	}
}

// SQL2 implements Type2 interface.
func (t NumberTypeImpl_) SQL2(v sql.Value) (sqltypes.Value, error) {
	if v.IsNull() {
		return sqltypes.NULL, nil
	}

	var val []byte
	switch t.baseType {
	case sqltypes.Int8:
		x := values.ReadInt8(v.Val)
		val = []byte(strconv.FormatInt(int64(x), 10))
	case sqltypes.Int16:
		x := values.ReadInt16(v.Val)
		val = []byte(strconv.FormatInt(int64(x), 10))
	case sqltypes.Int24:
		x := values.ReadInt24(v.Val)
		val = []byte(strconv.FormatInt(int64(x), 10))
	case sqltypes.Int32:
		x := values.ReadInt32(v.Val)
		val = []byte(strconv.FormatInt(int64(x), 10))
	case sqltypes.Int64:
		x := values.ReadInt64(v.Val)
		val = []byte(strconv.FormatInt(x, 10))
	case sqltypes.Uint8:
		x := values.ReadUint8(v.Val)
		val = []byte(strconv.FormatUint(uint64(x), 10))
	case sqltypes.Uint16:
		x := values.ReadUint16(v.Val)
		val = []byte(strconv.FormatUint(uint64(x), 10))
	case sqltypes.Uint24:
		x := values.ReadUint24(v.Val)
		val = []byte(strconv.FormatUint(uint64(x), 10))
	case sqltypes.Uint32:
		x := values.ReadUint32(v.Val)
		val = []byte(strconv.FormatUint(uint64(x), 10))
	case sqltypes.Uint64:
		x := values.ReadUint64(v.Val)
		val = []byte(strconv.FormatUint(x, 10))
	case sqltypes.Float32:
		x := values.ReadFloat32(v.Val)
		if t.fixed {
			val = strconv.AppendFloat(nil, float64(x), 'f', t.scale, 64) // HeimDB: D 桁（段階 120）
		} else {
			val = AppendMySQLFloat(nil, x, 0) // HeimDB: MySQL と同じ形
		}
	case sqltypes.Float64:
		x := values.ReadFloat64(v.Val)
		if t.fixed && !math.IsInf(x, 0) && !math.IsNaN(x) {
			val = strconv.AppendFloat(nil, x, 'f', t.scale, 64) // HeimDB: D 桁（段階 120）
		} else {
			val = AppendMySQLDouble(nil, x, 0) // HeimDB: MySQL と同じ形
		}
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "number"))
	}

	return sqltypes.MakeTrusted(t.baseType, val), nil
}

// String implements Type interface.
func (t NumberTypeImpl_) String() string {
	switch t.baseType {
	case sqltypes.Int8:
		// MySQL 8.1.0 only honors display width for signed TINYINT fields
		if t.displayWidth != 0 {
			return fmt.Sprintf("tinyint(%d)", t.displayWidth)
		}
		return "tinyint"
	case sqltypes.Uint8:
		return "tinyint unsigned"
	case sqltypes.Int16:
		return "smallint"
	case sqltypes.Uint16:
		return "smallint unsigned"
	case sqltypes.Int24:
		return "mediumint"
	case sqltypes.Uint24:
		return "mediumint unsigned"
	case sqltypes.Int32:
		return "int"
	case sqltypes.Uint32:
		return "int unsigned"
	case sqltypes.Int64:
		return "bigint"
	case sqltypes.Uint64:
		return "bigint unsigned"
	case sqltypes.Float32, sqltypes.Float64:
		// HeimDB: MySQL と同じく (M,D) と unsigned を付ける（float(7,2)、double(10,3) unsigned、float unsigned。段階 120）。
		// 桁の決まった式の型は、MySQL の CREATE TABLE ... SELECT と同じく double(22,D)
		s := "float"
		if t.baseType == sqltypes.Float64 {
			s = "double"
		}
		if t.precision > 0 {
			s += fmt.Sprintf("(%d,%d)", t.precision, t.scale)
		} else if t.fixed && t.baseType == sqltypes.Float32 {
			s += fmt.Sprintf("(%d,%d)", max(12, t.scale+1), t.scale)
		} else if t.fixed {
			s += fmt.Sprintf("(%d,%d)", max(22, t.scale+1), t.scale)
		}
		if t.unsigned {
			s += " unsigned"
		}
		return s
	default:
		panic(fmt.Sprintf("%v is not a valid number base type", t.baseType.String()))
	}
}

// Type implements Type interface.
func (t NumberTypeImpl_) Type() query.Type {
	return t.baseType
}

// ValueType implements Type interface.
func (t NumberTypeImpl_) ValueType() reflect.Type {
	switch t.baseType {
	case sqltypes.Int8:
		return numberInt8ValueType
	case sqltypes.Uint8:
		return numberUint8ValueType
	case sqltypes.Int16:
		return numberInt16ValueType
	case sqltypes.Uint16:
		return numberUint16ValueType
	case sqltypes.Int24:
		return numberInt32ValueType
	case sqltypes.Uint24:
		return numberUint32ValueType
	case sqltypes.Int32:
		return numberInt32ValueType
	case sqltypes.Uint32:
		return numberUint32ValueType
	case sqltypes.Int64:
		return numberInt64ValueType
	case sqltypes.Uint64:
		return numberUint64ValueType
	case sqltypes.Float32:
		return numberFloat32ValueType
	case sqltypes.Float64:
		return numberFloat64ValueType
	default:
		panic(fmt.Sprintf("%v is not a valid number base type", t.baseType.String()))
	}
}

// Zero implements Type interface.
func (t NumberTypeImpl_) Zero() interface{} {
	switch t.baseType {
	case sqltypes.Int8:
		return int8(0)
	case sqltypes.Uint8:
		return uint8(0)
	case sqltypes.Int16:
		return int16(0)
	case sqltypes.Uint16:
		return uint16(0)
	case sqltypes.Int24:
		return int32(0)
	case sqltypes.Uint24:
		return uint32(0)
	case sqltypes.Int32:
		return int32(0)
	case sqltypes.Uint32:
		return uint32(0)
	case sqltypes.Int64:
		return int64(0)
	case sqltypes.Uint64:
		return uint64(0)
	case sqltypes.Float32:
		return float32(0)
	case sqltypes.Float64:
		return float64(0)
	default:
		panic(fmt.Sprintf("%v is not a valid number base type", t.baseType.String()))
	}
}

// CollationCoercibility implements sql.CollationCoercible interface.
func (NumberTypeImpl_) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// IsFloat implements NumberType interface.
func (t NumberTypeImpl_) IsFloat() bool {
	switch t.baseType {
	case sqltypes.Float32, sqltypes.Float64:
		return true
	}
	return false
}

// IsSigned implements NumberType interface.
func (t NumberTypeImpl_) IsSigned() bool {
	switch t.baseType {
	case sqltypes.Int8, sqltypes.Int16, sqltypes.Int24, sqltypes.Int32, sqltypes.Int64, sqltypes.Float32, sqltypes.Float64:
		return true
	}
	return false
}

// DisplayWidth() implements NumberType inteface.
func (t NumberTypeImpl_) DisplayWidth() int {
	return t.displayWidth
}

func convertToInt64(t NumberTypeImpl_, v interface{}) (int64, sql.ConvertInRange, error) {
	switch v := v.(type) {
	case time.Time:
		return v.UTC().Unix(), sql.InRange, nil
	case int:
		return int64(v), sql.InRange, nil
	case int8:
		return int64(v), sql.InRange, nil
	case int16:
		return int64(v), sql.InRange, nil
	case int32:
		return int64(v), sql.InRange, nil
	case int64:
		return v, sql.InRange, nil
	case uint:
		return int64(v), sql.InRange, nil
	case uint8:
		return int64(v), sql.InRange, nil
	case uint16:
		return int64(v), sql.InRange, nil
	case uint32:
		return int64(v), sql.InRange, nil
	case uint64:
		if v > math.MaxInt64 {
			return math.MaxInt64, sql.OutOfRange, nil
		}
		return int64(v), sql.InRange, nil
	case float32:
		if v > float32(math.MaxInt64) {
			return math.MaxInt64, sql.OutOfRange, nil
		} else if v < float32(math.MinInt64) {
			return math.MinInt64, sql.OutOfRange, nil
		}
		return int64(math.Round(float64(v))), sql.InRange, nil
	case float64:
		if v > float64(math.MaxInt64) {
			return math.MaxInt64, sql.OutOfRange, nil
		} else if v < float64(math.MinInt64) {
			return math.MinInt64, sql.OutOfRange, nil
		}
		return int64(math.Round(v)), sql.InRange, nil
	case decimal.Decimal:
		if v.GreaterThan(dec_int64_max) {
			return dec_int64_max.IntPart(), sql.OutOfRange, nil
		} else if v.LessThan(dec_int64_min) {
			return dec_int64_min.IntPart(), sql.OutOfRange, nil
		}
		return v.Round(0).IntPart(), sql.InRange, nil
	case []byte:
		i, err := strconv.ParseInt(hex.EncodeToString(v), 16, 64)
		if err != nil {
			return 0, sql.OutOfRange, sql.ErrInvalidValue.New(v, t.String())
		}
		return i, sql.InRange, nil
	case string:
		v = danglingExponent(strings.Trim(v, intCutSet))
		if v == "" {
			// StringType{}.Zero() returns empty string, but should represent "0" for number value
			return 0, sql.InRange, nil
		}
		// Parse first an integer, which allows for more values than float64
		i, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return i, sql.InRange, nil
		}
		// If that fails, try as a float and truncate it to integral
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			// HeimDB: MySQL と同じく、先頭の数の部分（'12abc' なら 12）を読み、誤りと一緒に返す。
			f, _ = truncatedStringFloat(v)
			n, inRange, _ := convertToInt64(t, f)
			return n, inRange, sql.ErrInvalidValue.New(v, t.String())
		}
		// HeimDB: 範囲の外の値（'99999999999999999999'）は、端の値にする（Go の int64(f) は値が決まらない）。
		return convertToInt64(t, f)
	case bool:
		if v {
			return 1, sql.InRange, nil
		}
		return 0, sql.InRange, nil
	case nil:
		return 0, sql.InRange, nil
	default:
		return 0, sql.OutOfRange, sql.ErrInvalidValueType.New(v, t.String())
	}
}

func convertValueToInt64(t NumberTypeImpl_, v sql.Value) (int64, error) {
	switch v.Typ {
	case query.Type_INT8:
		return int64(values.ReadInt8(v.Val)), nil
	case query.Type_INT16:
		return int64(values.ReadInt16(v.Val)), nil
	case query.Type_INT24:
		return int64(values.ReadInt24(v.Val)), nil
	case query.Type_INT32:
		return int64(values.ReadInt32(v.Val)), nil
	case query.Type_INT64:
		return values.ReadInt64(v.Val), nil
	case query.Type_UINT8:
		return int64(values.ReadUint8(v.Val)), nil
	case query.Type_UINT16:
		return int64(values.ReadUint16(v.Val)), nil
	case query.Type_UINT24:
		return int64(values.ReadUint24(v.Val)), nil
	case query.Type_UINT32:
		return int64(values.ReadUint32(v.Val)), nil
	case query.Type_UINT64:
		v := values.ReadUint64(v.Val)
		if v > math.MaxInt64 {
			return math.MaxInt64, nil
		}
		return int64(v), nil
	case query.Type_FLOAT32:
		v := values.ReadFloat32(v.Val)
		if v > float32(math.MaxInt64) {
			return math.MaxInt64, nil
		} else if v < float32(math.MinInt64) {
			return math.MinInt64, nil
		}
		return int64(math.Round(float64(v))), nil
	case query.Type_FLOAT64:
		v := values.ReadFloat64(v.Val)
		if v > float64(math.MaxInt64) {
			return math.MaxInt64, nil
		} else if v < float64(math.MinInt64) {
			return math.MinInt64, nil
		}
		return int64(math.Round(v)), nil
		// TODO: add more conversions
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "number"))
	}
}

func convertValueToUint64(t NumberTypeImpl_, v sql.Value) (uint64, error) {
	switch v.Typ {
	case query.Type_INT8:
		return uint64(values.ReadInt8(v.Val)), nil
	case query.Type_INT16:
		return uint64(values.ReadInt16(v.Val)), nil
	case query.Type_INT24:
		return uint64(values.ReadInt24(v.Val)), nil
	case query.Type_INT32:
		return uint64(values.ReadInt32(v.Val)), nil
	case query.Type_INT64:
		return uint64(values.ReadInt64(v.Val)), nil
	case query.Type_UINT8:
		return uint64(values.ReadUint8(v.Val)), nil
	case query.Type_UINT16:
		return uint64(values.ReadUint16(v.Val)), nil
	case query.Type_UINT24:
		return uint64(values.ReadUint24(v.Val)), nil
	case query.Type_UINT32:
		return uint64(values.ReadUint32(v.Val)), nil
	case query.Type_UINT64:
		return values.ReadUint64(v.Val), nil
	case query.Type_FLOAT32:
		v := values.ReadFloat32(v.Val)
		if v >= float32(math.MaxUint64) {
			return math.MaxUint64, nil
		}
		return uint64(math.Round(float64(v))), nil
	case query.Type_FLOAT64:
		v := values.ReadFloat64(v.Val)
		if v >= float64(math.MaxUint64) {
			return math.MaxUint64, nil
		}
		return uint64(math.Round(v)), nil
		// TODO: add more conversions
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "number"))
	}
}

func convertToUint64(t NumberTypeImpl_, v interface{}) (uint64, sql.ConvertInRange, error) {
	switch v := v.(type) {
	case time.Time:
		return uint64(v.UTC().Unix()), sql.InRange, nil
	case int:
		if v < 0 {
			return uint64(math.MaxUint64 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint64(v), sql.InRange, nil
	case int8:
		if v < 0 {
			return uint64(math.MaxUint64 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint64(v), sql.InRange, nil
	case int16:
		if v < 0 {
			return uint64(math.MaxUint64 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint64(v), sql.InRange, nil
	case int32:
		if v < 0 {
			return uint64(math.MaxUint64 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint64(v), sql.InRange, nil
	case int64:
		if v < 0 {
			return uint64(math.MaxUint64 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint64(v), sql.InRange, nil
	case uint:
		return uint64(v), sql.InRange, nil
	case uint8:
		return uint64(v), sql.InRange, nil
	case uint16:
		return uint64(v), sql.InRange, nil
	case uint32:
		return uint64(v), sql.InRange, nil
	case uint64:
		return v, sql.InRange, nil
	case float32:
		if v > float32(math.MaxInt64) {
			return math.MaxUint64, sql.OutOfRange, nil
		} else if v < 0 {
			return uint64(math.MaxUint64 - v), sql.OutOfRange, nil
		}
		return uint64(math.Round(float64(v))), sql.InRange, nil
	case float64:
		if v >= float64(math.MaxUint64) {
			return math.MaxUint64, sql.OutOfRange, nil
		} else if v <= 0 {
			return uint64(math.MaxUint64 - v), sql.OutOfRange, nil
		}
		return uint64(math.Round(v)), sql.InRange, nil
	case decimal.Decimal:
		if v.GreaterThan(dec_uint64_max) {
			return math.MaxUint64, sql.OutOfRange, nil
		} else if v.LessThan(dec_zero) {
			ret, _ := dec_uint64_max.Sub(v).Float64()
			return uint64(math.Round(ret)), sql.OutOfRange, nil
		}
		// TODO: If we ever internally switch to using Decimal for large numbers, this will need to be updated
		f, _ := v.Float64()
		return uint64(math.Round(f)), sql.InRange, nil
	case []byte:
		i, err := strconv.ParseUint(hex.EncodeToString(v), 16, 64)
		if err != nil {
			return 0, sql.OutOfRange, sql.ErrInvalidValue.New(v, t.String())
		}
		return i, sql.InRange, nil
	case string:
		v = danglingExponent(strings.Trim(v, intCutSet))
		if i, err := strconv.ParseUint(v, 10, 64); err == nil {
			return i, sql.InRange, nil
		} else if err == strconv.ErrRange {
			// Number is too large for uint64, return max value and OutOfRange
			return math.MaxUint64, sql.OutOfRange, nil
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if val, inRange, err := convertToUint64(t, f); err == nil && inRange {
				return val, inRange, err
			}
		}
		// HeimDB: MySQL と同じく、先頭の数の部分を読み、誤りと一緒に返す。
		f, _ := truncatedStringFloat(v)
		if f <= 0 {
			// float から符号なしへの変換は 0 以下を折り返すので、先に 0 にする（'abc' は 0、'-5x' は範囲の外の 0）。
			inRange := sql.InRange
			if f < 0 {
				inRange = sql.OutOfRange
			}
			return 0, inRange, sql.ErrInvalidValue.New(v, t.String())
		}
		val, inRange, _ := convertToUint64(t, f)
		return val, inRange, sql.ErrInvalidValue.New(v, t.String())
	case bool:
		if v {
			return 1, sql.InRange, nil
		}
		return 0, sql.InRange, nil
	case nil:
		return 0, sql.InRange, nil
	default:
		return 0, sql.OutOfRange, sql.ErrInvalidValueType.New(v, t.String())
	}
}

func convertToUint32(t NumberTypeImpl_, v interface{}) (uint32, sql.ConvertInRange, error) {
	switch v := v.(type) {
	case int:
		if v < 0 {
			return uint32(math.MaxUint32 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint32 {
			return uint32(math.MaxUint32), sql.OutOfRange, nil
		}
		return uint32(v), sql.InRange, nil
	case int8:
		if v < 0 {
			return uint32(math.MaxUint32 - uint(-v-1)), sql.OutOfRange, nil
		} else if int(v) > math.MaxUint32 {
			return uint32(math.MaxUint32), sql.OutOfRange, nil
		}
		return uint32(v), sql.InRange, nil
	case int16:
		if v < 0 {
			return uint32(math.MaxUint32 - uint(-v-1)), sql.OutOfRange, nil
		} else if int(v) > math.MaxUint32 {
			return uint32(math.MaxUint32), sql.OutOfRange, nil
		}
		return uint32(v), sql.InRange, nil
	case int32:
		if v < 0 {
			return uint32(math.MaxUint32 - uint(-v-1)), sql.OutOfRange, nil
		} else if int(v) > math.MaxUint32 {
			return uint32(math.MaxUint32), sql.OutOfRange, nil
		}
		return uint32(v), sql.InRange, nil
	case int64:
		if v < 0 {
			return uint32(math.MaxUint32 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint32 {
			return uint32(math.MaxUint32), sql.OutOfRange, nil
		}
		return uint32(v), sql.InRange, nil
	case uint:
		return convertUintToUint32(uint64(v))
	case uint8:
		return uint32(v), sql.InRange, nil
	case uint16:
		return uint32(v), sql.InRange, nil
	case uint32:
		return v, sql.InRange, nil
	case uint64:
		return convertUintToUint32(v)
	case float64:
		if float32(v) > float32(math.MaxInt32) {
			return math.MaxUint32, sql.OutOfRange, nil
		} else if v < 0 {
			return uint32(math.MaxUint32 - v), sql.OutOfRange, nil
		}
		return uint32(math.Round(float64(v))), sql.InRange, nil
	case float32:
		if v >= float32(math.MaxUint32) {
			return math.MaxUint32, sql.OutOfRange, nil
		} else if v <= 0 {
			return uint32(math.MaxUint32 - v), sql.OutOfRange, nil
		}
		return uint32(math.Round(float64(v))), sql.InRange, nil
	case decimal.Decimal:
		if v.GreaterThan(dec_uint32_max) {
			return math.MaxUint32, sql.InRange, nil
		} else if v.LessThan(dec_zero) {
			ret, _ := dec_uint32_max.Sub(v).Float64()
			return uint32(math.Round(ret)), sql.OutOfRange, nil
		}
		// TODO: If we ever internally switch to using Decimal for large numbers, this will need to be updated
		f, _ := v.Float64()
		return uint32(math.Round(f)), sql.InRange, nil
	case []byte:
		i, err := strconv.ParseUint(hex.EncodeToString(v), 16, 32)
		if err != nil {
			return 0, sql.OutOfRange, sql.ErrInvalidValue.New(v, t.String())
		}
		return uint32(i), sql.InRange, nil
	case string:
		v = danglingExponent(strings.Trim(v, intCutSet))
		if i, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(i), sql.InRange, nil
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if val, inRange, err := convertToUint32(t, f); err == nil && inRange {
				return val, inRange, err
			}
		}
		// HeimDB: MySQL と同じく、先頭の数の部分を読み、誤りと一緒に返す。
		f, _ := truncatedStringFloat(v)
		if f <= 0 {
			// float から符号なしへの変換は 0 以下を折り返すので、先に 0 にする（'abc' は 0、'-5x' は範囲の外の 0）。
			inRange := sql.InRange
			if f < 0 {
				inRange = sql.OutOfRange
			}
			return 0, inRange, sql.ErrInvalidValue.New(v, t.String())
		}
		val, inRange, _ := convertToUint32(t, f)
		return val, inRange, sql.ErrInvalidValue.New(v, t.String())
	case bool:
		if v {
			return 1, sql.InRange, nil
		}
		return 0, sql.InRange, nil
	case nil:
		return 0, sql.InRange, nil
	default:
		return 0, sql.OutOfRange, sql.ErrInvalidValueType.New(v, t.String())
	}
}

func convertToUint16(t NumberTypeImpl_, v interface{}) (uint16, sql.ConvertInRange, error) {
	switch v := v.(type) {
	case int:
		if v < 0 {
			return uint16(math.MaxUint16 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint16 {
			return uint16(math.MaxUint16), sql.OutOfRange, nil
		}
		return uint16(v), sql.InRange, nil
	case int8:
		if v < 0 {
			return uint16(math.MaxUint16 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint16(v), sql.InRange, nil
	case int16:
		if v < 0 {
			return uint16(math.MaxUint16 - uint(-v-1)), sql.OutOfRange, nil
		}
		return uint16(v), sql.InRange, nil
	case int32:
		if v < 0 {
			return uint16(math.MaxUint16 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint16 {
			return uint16(math.MaxUint16), sql.OutOfRange, nil
		}
		return uint16(v), sql.InRange, nil
	case int64:
		if v < 0 {
			return uint16(math.MaxUint16 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint16 {
			return uint16(math.MaxUint16), sql.OutOfRange, nil
		}
		return uint16(v), sql.InRange, nil
	case uint:
		return convertUintToUint16(uint64(v))
	case uint8:
		return uint16(v), sql.InRange, nil
	case uint64:
		return convertUintToUint16(v)
	case uint32:
		return convertUintToUint16(uint64(v))
	case uint16:
		return v, sql.InRange, nil
	case float32:
		if v > float32(math.MaxInt16) {
			return math.MaxUint16, sql.OutOfRange, nil
		} else if v < 0 {
			return uint16(math.MaxUint16 - v), sql.OutOfRange, nil
		}
		return uint16(math.Round(float64(v))), sql.InRange, nil
	case float64:
		if v >= float64(math.MaxUint16) {
			return math.MaxUint16, sql.OutOfRange, nil
		} else if v <= 0 {
			return uint16(math.MaxUint16 - v), sql.OutOfRange, nil
		}
		return uint16(math.Round(v)), sql.InRange, nil
	case decimal.Decimal:
		if v.GreaterThan(dec_uint16_max) {
			return math.MaxUint16, sql.InRange, nil
		} else if v.LessThan(dec_zero) {
			ret, _ := dec_uint16_max.Sub(v).Float64()
			return uint16(math.Round(ret)), sql.OutOfRange, nil
		}
		// TODO: If we ever internally switch to using Decimal for large numbers, this will need to be updated
		f, _ := v.Float64()
		return uint16(math.Round(f)), sql.InRange, nil
	case []byte:
		i, err := strconv.ParseUint(hex.EncodeToString(v), 16, 16)
		if err != nil {
			return 0, sql.OutOfRange, sql.ErrInvalidValue.New(v, t.String())
		}
		return uint16(i), sql.InRange, nil
	case string:
		v = danglingExponent(strings.Trim(v, intCutSet))
		if i, err := strconv.ParseUint(v, 10, 16); err == nil {
			return uint16(i), sql.InRange, nil
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if val, inRange, err := convertToUint16(t, f); err == nil && inRange {
				return val, inRange, err
			}
		}
		// HeimDB: MySQL と同じく、先頭の数の部分を読み、誤りと一緒に返す。
		f, _ := truncatedStringFloat(v)
		if f <= 0 {
			// float から符号なしへの変換は 0 以下を折り返すので、先に 0 にする（'abc' は 0、'-5x' は範囲の外の 0）。
			inRange := sql.InRange
			if f < 0 {
				inRange = sql.OutOfRange
			}
			return 0, inRange, sql.ErrInvalidValue.New(v, t.String())
		}
		val, inRange, _ := convertToUint16(t, f)
		return val, inRange, sql.ErrInvalidValue.New(v, t.String())
	case bool:
		if v {
			return 1, sql.InRange, nil
		}
		return 0, sql.InRange, nil
	case nil:
		return 0, sql.InRange, nil
	default:
		return 0, sql.OutOfRange, sql.ErrInvalidValueType.New(v, t.String())
	}
}

func convertToUint8(t NumberTypeImpl_, v interface{}) (uint8, sql.ConvertInRange, error) {
	switch v := v.(type) {
	case int:
		if v < 0 {
			return uint8(math.MaxUint8 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint8 {
			return uint8(math.MaxUint8), sql.OutOfRange, nil
		}
		return uint8(v), sql.InRange, nil
	case int16:
		if v < 0 {
			return uint8(math.MaxUint8 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint8 {
			return uint8(math.MaxUint8), sql.OutOfRange, nil
		}
		return uint8(v), sql.InRange, nil
	case int8:
		if v < 0 {
			return uint8(math.MaxUint8 - uint(-v-1)), sql.OutOfRange, nil
		} else if int(v) > math.MaxUint8 {
			return uint8(math.MaxUint8), sql.OutOfRange, nil
		}
		return uint8(v), sql.InRange, nil
	case int32:
		if v < 0 {
			return uint8(math.MaxUint8 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint8 {
			return uint8(math.MaxUint8), sql.OutOfRange, nil
		}
		return uint8(v), sql.InRange, nil
	case int64:
		if v < 0 {
			return uint8(math.MaxUint8 - uint(-v-1)), sql.OutOfRange, nil
		} else if v > math.MaxUint8 {
			return uint8(math.MaxUint8), sql.OutOfRange, nil
		}
		return uint8(v), sql.InRange, nil
	case uint:
		return convertUintToUint8(uint64(v))
	case uint16:
		return convertUintToUint8(uint64(v))
	case uint64:
		return convertUintToUint8(v)
	case uint32:
		return convertUintToUint8(uint64(v))
	case uint8:
		return v, sql.InRange, nil
	case float32:
		if v > float32(math.MaxInt8) {
			return math.MaxUint8, sql.OutOfRange, nil
		} else if v < 0 {
			return uint8(math.MaxUint8 - v), sql.OutOfRange, nil
		}
		return uint8(math.Round(float64(v))), sql.InRange, nil
	case float64:
		if v >= float64(math.MaxUint8) {
			return math.MaxUint8, sql.OutOfRange, nil
		} else if v <= 0 {
			return uint8(math.MaxUint8 - v), sql.OutOfRange, nil
		}
		return uint8(math.Round(v)), sql.InRange, nil
	case decimal.Decimal:
		if v.GreaterThan(dec_uint8_max) {
			return math.MaxUint8, sql.InRange, nil
		} else if v.LessThan(dec_zero) {
			ret, _ := dec_uint8_max.Sub(v).Float64()
			return uint8(math.Round(ret)), sql.OutOfRange, nil
		}
		// TODO: If we ever internally switch to using Decimal for large numbers, this will need to be updated
		f, _ := v.Float64()
		return uint8(math.Round(f)), sql.InRange, nil
	case []byte:
		i, err := strconv.ParseUint(hex.EncodeToString(v), 8, 8)
		if err != nil {
			return 0, sql.OutOfRange, sql.ErrInvalidValue.New(v, t.String())
		}
		return uint8(i), sql.InRange, nil
	case string:
		v = danglingExponent(strings.Trim(v, intCutSet))
		if i, err := strconv.ParseUint(v, 10, 8); err == nil {
			return uint8(i), sql.InRange, nil
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if val, inRange, err := convertToUint8(t, f); err == nil && inRange {
				return val, inRange, err
			}
		}
		// HeimDB: MySQL と同じく、先頭の数の部分を読み、誤りと一緒に返す。
		f, _ := truncatedStringFloat(v)
		if f <= 0 {
			// float から符号なしへの変換は 0 以下を折り返すので、先に 0 にする（'abc' は 0、'-5x' は範囲の外の 0）。
			inRange := sql.InRange
			if f < 0 {
				inRange = sql.OutOfRange
			}
			return 0, inRange, sql.ErrInvalidValue.New(v, t.String())
		}
		val, inRange, _ := convertToUint8(t, f)
		return val, inRange, sql.ErrInvalidValue.New(v, t.String())
	case bool:
		if v {
			return 1, sql.InRange, nil
		}
		return 0, sql.InRange, nil
	case nil:
		return 0, sql.InRange, nil
	default:
		return 0, sql.OutOfRange, sql.ErrInvalidValueType.New(v, t.String())
	}
}

func convertToFloat64(t NumberTypeImpl_, v interface{}) (float64, error) {
	switch v := v.(type) {
	case time.Time:
		return float64(v.UTC().Unix()), nil
	case int:
		return float64(v), nil
	case int8:
		return float64(v), nil
	case int16:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case uint:
		return float64(v), nil
	case uint8:
		return float64(v), nil
	case uint16:
		return float64(v), nil
	case uint32:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case float32:
		return float64(v), nil
	case float64:
		return v, nil
	case decimal.Decimal:
		f, _ := v.Float64()
		return f, nil
	case []byte:
		i, err := strconv.ParseUint(hex.EncodeToString(v), 16, 64)
		if err != nil {
			return 0, sql.ErrInvalidValue.New(v, t.String())
		}
		return float64(i), nil
	case string:
		v = strings.Trim(v, numericCutSet)
		if v == "" {
			// HeimDB: 空の文字列は 0（MySQL は警告を出さない。整数の変換と同じ）
			return 0, nil
		}
		i, err := strconv.ParseFloat(v, 64)
		if err == nil && !decimalFloatString(v) {
			// HeimDB: Go reads 'nan', 'inf', 'infinity' and hexadecimal floats, which MySQL does not ('inf' + 0 is 0; stage 116)
			err = strconv.ErrSyntax
		}
		if err != nil {
			// parse the first longest valid numbers
			// HeimDB: 符号と指数も読む（'-5x' は -5、'3.5e1abc' は 35。前は正規表現 numre で符号と指数を読まなかった）。
			i, _ = truncatedStringFloat(v)
			return i, sql.ErrInvalidValue.New(v, t.String())
		}
		return i, nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case nil:
		return 0, nil
	default:
		return 0, sql.ErrInvalidValueType.New(v, t.String())
	}
}

func convertValueToFloat64(t NumberTypeImpl_, v sql.Value) (float64, error) {
	switch v.Typ {
	case query.Type_INT8:
		return float64(values.ReadInt8(v.Val)), nil
	case query.Type_INT16:
		return float64(values.ReadInt16(v.Val)), nil
	case query.Type_INT24:
		return float64(values.ReadInt24(v.Val)), nil
	case query.Type_INT32:
		return float64(values.ReadInt32(v.Val)), nil
	case query.Type_INT64:
		return float64(values.ReadInt64(v.Val)), nil
	case query.Type_UINT8:
		return float64(values.ReadUint8(v.Val)), nil
	case query.Type_UINT16:
		return float64(values.ReadUint16(v.Val)), nil
	case query.Type_UINT24:
		return float64(values.ReadUint24(v.Val)), nil
	case query.Type_UINT32:
		return float64(values.ReadUint32(v.Val)), nil
	case query.Type_UINT64:
		return float64(values.ReadUint64(v.Val)), nil
	case query.Type_FLOAT32:
		return float64(values.ReadFloat32(v.Val)), nil
	case query.Type_FLOAT64:
		return values.ReadFloat64(v.Val), nil
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "number"))
	}
}

func mustInt64(v interface{}) int64 {
	switch tv := v.(type) {
	case int:
		return int64(tv)
	case int8:
		return int64(tv)
	case int16:
		return int64(tv)
	case int32:
		return int64(tv)
	case int64:
		return tv
	case uint:
		return int64(tv)
	case uint8:
		return int64(tv)
	case uint16:
		return int64(tv)
	case uint32:
		return int64(tv)
	case uint64:
		return int64(tv)
	case bool:
		if tv {
			return int64(1)
		}
		return int64(0)
	case float32:
		return int64(tv)
	case float64:
		return int64(tv)
	default:
		panic(fmt.Sprintf("unexpected type %v", v))
	}
}

func mustUint64(v interface{}) uint64 {
	switch tv := v.(type) {
	case uint:
		return uint64(tv)
	case uint8:
		return uint64(tv)
	case uint16:
		return uint64(tv)
	case uint32:
		return uint64(tv)
	case uint64:
		return tv
	case int:
		return uint64(tv)
	case int8:
		return uint64(tv)
	case int16:
		return uint64(tv)
	case int32:
		return uint64(tv)
	case int64:
		return uint64(tv)
	case bool:
		if tv {
			return uint64(1)
		}
		return uint64(0)
	case float32:
		return uint64(tv)
	case float64:
		return uint64(tv)
	default:
		panic(fmt.Sprintf("unexpected type %v", v))
	}
}

func mustFloat64(v interface{}) float64 {
	switch tv := v.(type) {
	case uint:
		return float64(tv)
	case uint8:
		return float64(tv)
	case uint16:
		return float64(tv)
	case uint32:
		return float64(tv)
	case uint64:
		return float64(tv)
	case int:
		return float64(tv)
	case int8:
		return float64(tv)
	case int16:
		return float64(tv)
	case int32:
		return float64(tv)
	case int64:
		return float64(tv)
	case bool:
		if tv {
			return float64(1)
		}
		return float64(0)
	case float32:
		return float64(tv)
	case float64:
		return tv
	default:
		panic(fmt.Sprintf("unexpected type %v", v))
	}
}

// CoalesceInt converts a int8/int16/... to int
func CoalesceInt(val interface{}) (int, bool) {
	switch v := val.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		return int(v), true
	default:
		return 0, false
	}
}

// convertUintToUint8 converts a uint64 value to uint8 with overflow checking.
// Returns the converted value, range status, and any error.
func convertUintToUint8(v uint64) (uint8, sql.ConvertInRange, error) {
	if v > math.MaxUint8 {
		return uint8(math.MaxUint8), sql.OutOfRange, nil
	}
	return uint8(v), sql.InRange, nil
}

// convertUintToUint16 converts a uint64 value to uint16 with overflow checking.
// Returns the converted value, range status, and any error.
func convertUintToUint16(v uint64) (uint16, sql.ConvertInRange, error) {
	if v > math.MaxUint16 {
		return uint16(math.MaxUint16), sql.OutOfRange, nil
	}
	return uint16(v), sql.InRange, nil
}

// convertUintToUint32 converts a uint64 value to uint32 with overflow checking.
// Returns the converted value, range status, and any error.
func convertUintToUint32(v uint64) (uint32, sql.ConvertInRange, error) {
	if v > math.MaxUint32 {
		return uint32(math.MaxUint32), sql.OutOfRange, nil
	}
	return uint32(v), sql.InRange, nil
}

// truncatedString は、v が文字列で、err が数への変換の誤り（先頭の数の部分を読んだ値と一緒に返したもの）か。
// HeimDB: 比べる、CAST する、索引の範囲にするなど、MySQL が警告で済ませる側は、その値を使う。
func truncatedString(v interface{}, err error) bool {
	_, ok := v.(string)
	return ok && sql.ErrInvalidValue.Is(err)
}

// TruncatedString は truncatedString と同じ（HeimDB: エンジンのほかのパッケージと internal/backend から使う）。
func TruncatedString(v interface{}, err error) bool {
	return truncatedString(v, err)
}

// decimalFloatString reports whether v, a string that strconv.ParseFloat reads, is a decimal number as MySQL
// reads it: an optional sign, then a digit or a point (not 'nan', 'inf', 'infinity' or '0x1p-2') (HeimDB).
func decimalFloatString(v string) bool {
	if v != "" && (v[0] == '+' || v[0] == '-') {
		v = v[1:]
	}
	if v == "" || !(v[0] >= '0' && v[0] <= '9' || v[0] == '.') {
		return false
	}
	return len(v) < 2 || v[0] != '0' || (v[1] != 'x' && v[1] != 'X')
}

// danglingExponent は、指数の数字のない '1E'、'3e+' を '1'、'3' にする。MySQL は整数の列への '1E' を誤りにも警告にも
// しない（上流の #3920。DOUBLE と DECIMAL では切り詰めとして扱う）。ほかの文字列はそのまま。
func danglingExponent(v string) string {
	if n := len(v); n < 2 || (v[n-1] != 'e' && v[n-1] != 'E' && v[n-1] != '+' && v[n-1] != '-') {
		return v
	}
	if prefix, truncated := TruncateStringToDouble(v, false); !truncated {
		return prefix
	}
	return v
}

// truncatedStringFloat は、文字列 s の先頭の数の部分（TruncateStringToDouble）を float64 にする。
func truncatedStringFloat(s string) (float64, bool) {
	prefix, _ := TruncateStringToDouble(s, false)
	f, err := strconv.ParseFloat(prefix, 64)
	if err != nil {
		// 大きすぎる値は ParseFloat が ±Inf と誤りを返す。MySQL と同じく DOUBLE の端の値にする（'1e400' は
		// 1.7976931348623157e308。段階 114）
		if math.IsInf(f, 1) {
			f = math.MaxFloat64
		} else if math.IsInf(f, -1) {
			f = -math.MaxFloat64
		}
		return f, false
	}
	return f, true
}

// StringOverflowsDouble は、文字列 s の先頭の数の部分が DOUBLE の範囲を超えるか（'1e400'）。
func StringOverflowsDouble(s string) bool {
	prefix, _ := TruncateStringToDouble(s, false)
	_, err := strconv.ParseFloat(prefix, 64)
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// IntCutSet と NumericCutSet は、文字列を整数と数にするときに前後から除く字。
const (
	IntCutSet     = sql.IntCutSet
	NumericCutSet = sql.NumericCutSet
)

// TruncateStringToInt と TruncateStringToDouble は sql の同名の関数（HeimDB: 索引の範囲の組み立てからも使うので sql に置いた）。
func TruncateStringToInt(s string) (string, bool) { return sql.TruncateStringToInt(s) }

func TruncateStringToDouble(s string, truncExp bool) (string, bool) {
	return sql.TruncateStringToDouble(s, truncExp)
}
