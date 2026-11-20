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
// Modified by Tsukinowa Inc.: go vet の書式の文字列の確認に合わせた（docs/engine.md）
// Modified by Tsukinowa Inc.: CAST で文字列の先頭の数の部分を読み、警告 1292 を出す（docs/engine.md）
// Modified by Tsukinowa Inc.: CAST(... AS DECIMAL(M)) を DECIMAL(M,0) にし、範囲の外を端の値と警告 1264 にする（docs/engine.md）
// Modified by Tsukinowa Inc.: CAST で日時の文字列の後ろの余計な文字を切り詰め、警告 1292 を出す（上流の #3276、#3280。docs/engine.md）
// Modified by Tsukinowa Inc.: 数の CAST AS DATE と DATETIME を MySQL の number_to_datetime と同じく読む（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: CAST AS CHAR の日付と日時を型の形の文字列にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日付、日時、TIME を数にする CAST を MySQL と同じ数の形にする（前は UNIX 時刻。段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 文に書いた CAST(... AS CHAR) の照合順序を接続の照合順序にする（WithCollation。段階 116 の 3 度目のレビュー。docs/engine.md）

package expression

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"gopkg.in/src-d/go-errors.v1"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// ErrConvertExpression is returned when a conversion is not possible.
var ErrConvertExpression = errors.NewKind("expression '%v': couldn't convert to %v")

const (
	// ConvertToBinary is a conversion to binary.
	ConvertToBinary = "binary"
	// ConvertToChar is a conversion to char.
	ConvertToChar = "char"
	// ConvertToNChar is a conversion to nchar.
	ConvertToNChar = "nchar"
	// ConvertToDate is a conversion to date.
	ConvertToDate = "date"
	// ConvertToDatetime is a conversion to datetime.
	ConvertToDatetime = "datetime"
	// ConvertToDecimal is a conversion to decimal.
	ConvertToDecimal = "decimal"
	// ConvertToFloat is a conversion to float.
	ConvertToFloat = "float"
	// ConvertToDouble is a conversion to double.
	ConvertToDouble = "double"
	// ConvertToJSON is a conversion to json.
	ConvertToJSON = "json"
	// ConvertToReal is a conversion to double.
	ConvertToReal = "real"
	// ConvertToSigned is a conversion to signed.
	ConvertToSigned = "signed"
	// ConvertToTime is a conversion to time.
	ConvertToTime = "time"
	// ConvertToYear isa convert to year.
	ConvertToYear = "year"
	// ConvertToUnsigned is a conversion to unsigned.
	ConvertToUnsigned = "unsigned"
)

// Convert represent a CAST(x AS T) or CONVERT(x, T) operation that casts x expression to type T.
type Convert struct {
	UnaryExpression
	// castToType is a string representation of the base type to which we are casting (e.g. "char", "float", "decimal")
	castToType string
	// typeLength is the optional length parameter for types that support it (e.g. "char(10)")
	typeLength int
	// typeScale is the optional scale parameter for types that support it (e.g. "decimal(10, 2)")
	typeScale int
	// cachedDecimalType is the cached Decimal type for this convert expression. Because new Decimal types
	// must be created with their specific scale and precision values, unlike other types, we cache the created
	// type to avoid re-creating it on every call to Type().
	cachedDecimalType sql.DecimalType
	// name は、SELECT の項目のこの CAST の名前（別名か、書いたとおりの式）。DECIMAL の範囲の外の警告 1264 に出す
	// （HeimDB）。
	name string
	// explicit は、文に書いた CAST と CONVERT（HeimDB。NO_ZERO_DATE のゼロの日付を NULL にするのはこれだけ）
	explicit bool
	// collation は、文に書いた CAST(... AS CHAR) の照合順序（HeimDB。MySQL と同じく接続の照合順序 collation_connection
	// で、ORDER BY と GROUP BY もこれで比べる。前は utf8mb4_0900_bin の LONGTEXT で、ORDER BY CAST(e AS CHAR) が
	// 'Dd' を 'a' より前にした。段階 116 の 3 度目のレビュー）。0 なら決めていない。
	collation sql.CollationID
}

// AsExplicit は、文に書いた CAST の印を付けた写しを返す（HeimDB）。
func (c *Convert) AsExplicit() *Convert {
	nc := *c
	nc.explicit = true
	return &nc
}

// WithCollation は、CAST(... AS CHAR) の結果の照合順序を collation にした写しを返す（HeimDB）。
func (c *Convert) WithCollation(collation sql.CollationID) *Convert {
	nc := *c
	nc.collation = collation
	return &nc
}

// WithName は、SELECT の項目としての名前 name を付けた写しを返す（HeimDB）。
func (c *Convert) WithName(name string) *Convert {
	nc := *c
	nc.name = name
	return &nc
}

var _ sql.Expression = (*Convert)(nil)
var _ sql.CollationCoercible = (*Convert)(nil)

// NewConvert creates a new Convert expression that will attempt to convert the specified expression |expr| into the
// |castToType| type. All optional parameters (i.e. typeLength, typeScale, and charset) are omitted and initialized
// to their zero values.
func NewConvert(expr sql.Expression, castToType string) *Convert {
	disableRounding(expr)
	return &Convert{
		UnaryExpression: UnaryExpression{Child: expr},
		castToType:      strings.ToLower(castToType),
	}
}

// NewConvertWithLengthAndScale creates a new Convert expression that will attempt to convert |expr| into the
// |castToType| type, with |typeLength| specifying a length constraint of the converted type, and |typeScale| specifying
// a scale constraint of the converted type.
func NewConvertWithLengthAndScale(expr sql.Expression, castToType string, typeLength, typeScale int) *Convert {
	disableRounding(expr)
	return &Convert{
		UnaryExpression: UnaryExpression{Child: expr},
		castToType:      strings.ToLower(castToType),
		typeLength:      typeLength,
		typeScale:       typeScale,
	}
}

// GetConvertToType returns which type the both left and right values should be converted to.
// If neither sql.Type represent number, then converted to string. Otherwise, we try to get
// the appropriate type to avoid any precision loss.
func GetConvertToType(l, r sql.Type) string {
	if types.Null == l {
		return GetConvertToType(r, r)
	}
	if types.Null == r {
		return GetConvertToType(l, l)
	}

	if !types.IsNumber(l) || !types.IsNumber(r) {
		return ConvertToChar
	}

	if types.IsDecimal(l) || types.IsDecimal(r) {
		return ConvertToDecimal
	}
	if types.IsUnsigned(l) && types.IsUnsigned(r) {
		return ConvertToUnsigned
	}
	if types.IsSigned(l) && types.IsSigned(r) {
		return ConvertToSigned
	}
	if types.IsInteger(l) && types.IsInteger(r) {
		return ConvertToSigned
	}

	return ConvertToChar
}

// IsNullable implements the Expression interface.
func (c *Convert) IsNullable() bool {
	switch c.castToType {
	case ConvertToDate, ConvertToDatetime:
		return true
	default:
		return c.Child.IsNullable()
	}
}

// Type implements the Expression interface.
func (c *Convert) Type() sql.Type {
	switch c.castToType {
	case ConvertToBinary:
		return types.LongBlob
	case ConvertToChar, ConvertToNChar:
		if c.collation != sql.Collation_Unspecified {
			return types.CreateLongText(c.collation)
		}
		return types.LongText
	case ConvertToDate:
		return types.Date
	case ConvertToDatetime:
		return types.MustCreateDatetimeType(sqltypes.Datetime, c.typeLength)
	case ConvertToDecimal:
		if c.cachedDecimalType == nil {
			c.cachedDecimalType = createConvertedDecimalType(c.typeLength, c.typeScale, true)
		}
		return c.cachedDecimalType
	case ConvertToFloat:
		return types.Float32
	case ConvertToDouble, ConvertToReal:
		return types.Float64
	case ConvertToJSON:
		return types.JSON
	case ConvertToSigned:
		return types.Int64
	case ConvertToTime:
		return types.Time
	case ConvertToUnsigned:
		return types.Uint64
	case ConvertToYear:
		return types.Year
	default:
		return types.Null
	}
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (c *Convert) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	switch c.castToType {
	case ConvertToBinary:
		return sql.Collation_binary, 2
	case ConvertToChar, ConvertToNChar:
		if c.collation != sql.Collation_Unspecified {
			return c.collation, 2
		}
		return ctx.GetCollation(), 2
	case ConvertToDate:
		return sql.Collation_binary, 5
	case ConvertToDatetime:
		return sql.Collation_binary, 5
	case ConvertToDecimal:
		return sql.Collation_binary, 5
	case ConvertToDouble, ConvertToReal, ConvertToFloat:
		return sql.Collation_binary, 5
	case ConvertToJSON:
		return ctx.GetCharacterSet().BinaryCollation(), 2
	case ConvertToSigned:
		return sql.Collation_binary, 5
	case ConvertToTime:
		return sql.Collation_binary, 5
	case ConvertToUnsigned:
		return sql.Collation_binary, 5
	case ConvertToYear:
		return sql.Collation_binary, 5
	default:
		return sql.Collation_binary, 7
	}
}

// String implements the Stringer interface.
func (c *Convert) String() string {
	extraTypeInfo := ""
	if c.typeLength > 0 {
		if c.typeScale > 0 {
			extraTypeInfo = fmt.Sprintf("(%d,%d)", c.typeLength, c.typeScale)
		} else {
			extraTypeInfo = fmt.Sprintf("(%d)", c.typeLength)
		}
	}
	return fmt.Sprintf("convert(%v, %v%s)", c.Child, c.castToType, extraTypeInfo)
}

// DebugString implements the Expression interface.
func (c *Convert) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("convert")
	children := []string{
		fmt.Sprintf("type: %v", c.castToType),
	}

	if c.typeLength > 0 {
		children = append(children, fmt.Sprintf("typeLength: %v", c.typeLength))
	}

	if c.typeScale > 0 {
		children = append(children, fmt.Sprintf("typeScale: %v", c.typeScale))
	}

	children = append(children, fmt.Sprintf("%s", sql.DebugString(c.Child)))

	_ = pr.WriteChildren(children...)
	return pr.String()
}

// WithChildren implements the Expression interface.
func (c *Convert) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(c, len(children), 1)
	}
	nc := NewConvertWithLengthAndScale(children[0], c.castToType, c.typeLength, c.typeScale)
	nc.name = c.name
	nc.explicit = c.explicit
	nc.collation = c.collation
	return nc, nil
}

// Eval implements the Expression interface.
func (c *Convert) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	val, err := c.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	// Should always return nil, and a warning instead
	var casted interface{}
	if c.castToType == ConvertToDecimal {
		casted, err = castToDecimal(ctx, val, c.Child.Type(), c.typeLength, c.typeScale, c.name)
	} else {
		casted, err = convertValue(ctx, val, c.castToType, c.Child.Type(), c.typeLength, c.typeScale)
	}
	if err != nil {
		if c.castToType == ConvertToJSON {
			return nil, ErrConvertExpression.Wrap(err, c.String(), c.castToType)
		}
		ctx.Warn(1292, "Incorrect %s value: %v", c.castToType, val)
		return nil, nil
	}
	if c.explicit && (c.castToType == ConvertToDate || c.castToType == ConvertToDatetime) {
		return zeroDateOrNull(ctx, casted, val), nil
	}

	return casted, nil
}

// convertValue only returns an error if converting to JSON, Date, and Datetime;
// the zero value is returned for float types. Nil is returned in all other cases.
// If |typeLength| and |typeScale| are 0, they are ignored, otherwise they are used as constraints on the
// converted type where applicable (e.g. Char conversion supports only |typeLength|, Decimal conversion supports
// |typeLength| and |typeScale|).
func convertValue(ctx *sql.Context, val interface{}, castTo string, originType sql.Type, typeLength, typeScale int) (interface{}, error) {
	if val == nil {
		return nil, nil
	}
	switch strings.ToLower(castTo) {
	case ConvertToDecimal, ConvertToFloat, ConvertToDouble, ConvertToReal, ConvertToSigned, ConvertToUnsigned:
		// HeimDB: a date, datetime or TIME is its number (CAST(d AS SIGNED) is
		// 20240101100000, not the Unix time; stage 116)
		if originType != nil {
			if n, ok := types.TemporalNumber(ctx, originType, val); ok {
				val = n
			}
		}
	}
	switch strings.ToLower(castTo) {
	case ConvertToBinary:
		b, _, err := types.LongBlob.Convert(ctx, val)
		if err != nil {
			return nil, nil
		}
		if types.IsTextOnly(originType) {
			// For string types we need to re-encode the string as we want the binary representation of the character set
			encoder := originType.(sql.StringType).Collation().CharacterSet().Encoder()
			encodedBytes, ok := encoder.Encode(b.([]byte))
			if !ok {
				return nil, fmt.Errorf("unable to re-encode string to convert to binary")
			}
			b = encodedBytes
		}
		if bb, ok := b.([]byte); ok && len(bb) < typeLength {
			b = append(bb, make([]byte, typeLength-len(bb))...)
		}
		return truncateConvertedValue(b, typeLength)
	case ConvertToChar, ConvertToNChar:
		if ts, ok := types.ValueString(originType, val); ok {
			// HeimDB: a date as its type's text ('2024-01-01' for a DATE), an ENUM or SET as its value (stage 116)
			return truncateConvertedValue(ts, typeLength)
		}
		s, _, err := types.LongText.Convert(ctx, val)
		if err != nil {
			return nil, nil
		}
		return truncateConvertedValue(s, typeLength)
	case ConvertToDate:
		_, isTime := val.(time.Time)
		_, isString := val.(string)
		_, isBinary := val.([]byte)
		if types.IsNumberValue(val) {
			// HeimDB: a number is read as MySQL's number_to_datetime does
			// (CAST(20240101 AS DATE) is 2024-01-01); one of another form is NULL
			// with warning 1292 (CAST(1 AS DATETIME)) (stage 116)
			d, _, err := types.Date.Convert(ctx, val)
			if err != nil {
				ctx.Warn(mysql.ERTruncatedWrongValue, "Incorrect datetime value: '%v'", val)
				return nil, nil
			}
			return d, nil
		}
		if !(isTime || isString || isBinary) {
			return nil, nil
		}
		d, _, err := types.Date.Convert(ctx, val)
		if err != nil {
			// 後ろの余計な文字は切り詰めて警告（'2020-01-01 a' は 2020-01-01。上流の #3280）
			if !types.TruncatedDatetime(err) {
				return nil, err
			}
			ctx.Warn(mysql.ERTruncatedWrongValue, "%s", err.Error())
		}
		return d, nil
	case ConvertToDatetime:
		_, isTime := val.(time.Time)
		_, isString := val.(string)
		_, isBinary := val.([]byte)
		if types.IsNumberValue(val) {
			// HeimDB: a number is read as MySQL's number_to_datetime does
			// (CAST(20240101 AS DATE) is 2024-01-01); one of another form is NULL
			// with warning 1292 (CAST(1 AS DATETIME)) (stage 116)
			d, _, err := types.DatetimeMaxPrecision.Convert(ctx, val)
			if err != nil {
				ctx.Warn(mysql.ERTruncatedWrongValue, "Incorrect datetime value: '%v'", val)
				return nil, nil
			}
			return d, nil
		}
		if !(isTime || isString || isBinary) {
			return nil, nil
		}
		d, _, err := types.DatetimeMaxPrecision.Convert(ctx, val)
		if err != nil {
			// 上流の #3276
			if !types.TruncatedDatetime(err) {
				return nil, err
			}
			ctx.Warn(mysql.ERTruncatedWrongValue, "%s", err.Error())
		}
		return d, nil
	case ConvertToDecimal:
		return castToDecimal(ctx, val, originType, typeLength, typeScale, "")
	case ConvertToFloat:
		value, err := convertHexBlobToDecimalForNumericContext(val, originType)
		if err != nil {
			return nil, err
		}
		d, _, err := types.Float32.Convert(ctx, value)
		if err != nil {
			if types.TruncatedString(value, err) && d != nil {
				truncatedWarning(ctx, "DOUBLE", value)
				return d, nil
			}
			return types.Float32.Zero(), nil
		}
		return d, nil
	case ConvertToDouble, ConvertToReal:
		value, err := convertHexBlobToDecimalForNumericContext(val, originType)
		if err != nil {
			return nil, err
		}
		d, _, err := types.Float64.Convert(ctx, value)
		if err != nil {
			// HeimDB: 文字列は先頭の数の部分を読んだ値にし、MySQL と同じく警告を出す。文字列と数の比べもここを通る
			// （'12abc' = 12 は 12 = 12）。
			if types.TruncatedString(value, err) && d != nil {
				truncatedWarning(ctx, "DOUBLE", value)
				return d, nil
			}
			return types.Float64.Zero(), nil
		}
		return d, nil
	case ConvertToJSON:
		js, _, err := types.JSON.Convert(ctx, val)
		if err != nil {
			return nil, err
		}
		return js, nil
	case ConvertToSigned:
		value, err := convertHexBlobToDecimalForNumericContext(val, originType)
		if err != nil {
			return nil, err
		}
		if s, ok := value.(string); ok {
			n, _ := castStringToInteger(ctx, s)
			return n, nil
		}
		num, _, err := types.Int64.Convert(ctx, value)
		if err != nil {
			return types.Int64.Zero(), nil
		}

		return num, nil
	case ConvertToTime:
		t, _, err := types.Time.Convert(ctx, val)
		if err != nil {
			return nil, nil
		}
		return t, nil
	case ConvertToUnsigned:
		value, err := convertHexBlobToDecimalForNumericContext(val, originType)
		if err != nil {
			return nil, err
		}
		if s, ok := value.(string); ok {
			n, u := castStringToInteger(ctx, s)
			// MySQL は、負の値を符号なしにしたときだけ知らせる（範囲の外の '-99999999999999999999' では知らせない）
			if n < 0 && n != math.MinInt64 && strings.HasPrefix(strings.Trim(s, types.IntCutSet), "-") && ctx != nil && ctx.Session != nil {
				// HeimDB: MySQL と同じ警告
				ctx.Session.Warn(&sql.Warning{Level: "Warning", Code: mysql.ERUnknownError, Message: "Cast to unsigned converted negative integer to its positive complement"})
			}
			return u, nil
		}
		num, _, err := types.Uint64.Convert(ctx, value)
		if err != nil {
			num, _, err = types.Int64.Convert(ctx, value)
			if err != nil {
				return types.Uint64.Zero(), nil
			}
			return uint64(num.(int64)), nil
		}
		return num, nil
	case ConvertToYear:
		value, err := convertHexBlobToDecimalForNumericContext(val, originType)
		if err != nil {
			return nil, err
		}
		num, _, err := types.Uint64.Convert(ctx, value)
		if err != nil {
			num, _, err = types.Int64.Convert(ctx, value)
			if err != nil {
				return types.Uint64.Zero(), nil
			}
			return uint64(num.(int64)), nil
		}
		return num, nil
	default:
		return nil, nil
	}
}

// truncateConvertedValue truncates |val| to the specified |typeLength| if |val|
// is a string or byte slice. If the typeLength is 0, or if it is greater than
// the length of |val|, then |val| is simply returned as is. If |val| is not a
// string or []byte, then an error is returned.
func truncateConvertedValue(val interface{}, typeLength int) (interface{}, error) {
	if typeLength <= 0 {
		return val, nil
	}

	switch v := val.(type) {
	case []byte:
		if len(v) <= typeLength {
			typeLength = len(v)
		}
		return v[:typeLength], nil
	case string:
		if len(v) <= typeLength {
			typeLength = len(v)
		}
		return v[:typeLength], nil
	default:
		return nil, fmt.Errorf("unsupported type for truncation: %T", val)
	}
}

// createConvertedDecimalType creates a new Decimal type with the specified |precision| and |scale|. If a Decimal
// type cannot be created from the values specified, the internal Decimal type is returned. If |logErrors| is true,
// an error will also logged to the standard logger. (Setting |logErrors| to false, allows the caller to prevent
// spurious error message from being logged multiple times for the same error.) This function is intended to be
// used in places where an error cannot be returned (e.g. Node.Type() implementations), hence why it logs an error
// instead of returning one.
func createConvertedDecimalType(length, scale int, logErrors bool) sql.DecimalType {
	// HeimDB: DECIMAL(M) は DECIMAL(M,0)（MySQL と同じ。前は桁を決めない内部の型）。CAST(... AS DECIMAL) の M の
	// 既定（10）は planbuilder が入れる。
	if length > 0 {
		dt, err := types.CreateColumnDecimalType(uint8(length), uint8(scale))
		if err != nil {
			if logErrors {
				logrus.StandardLogger().Errorf("unable to create decimal type with length %d and scale %d: %v", length, scale, err)
			}
			return types.InternalDecimalType
		}
		return dt
	}
	return types.InternalDecimalType
}

// zeroDateOrNull returns the CAST AS DATE or DATETIME result d, or NULL with
// warning 1292 for the zero date when the sql_mode has NO_ZERO_DATE, as MySQL
// does (CAST(0 AS DATETIME) is NULL under the default sql_mode and the zero
// date under WordPress's) (HeimDB, stage 116).
func zeroDateOrNull(ctx *sql.Context, d, val interface{}) interface{} {
	if t, ok := d.(time.Time); ok && t.Equal(types.ZeroTime) && sql.LoadSqlMode(ctx).ModeEnabled("NO_ZERO_DATE") {
		ctx.Warn(mysql.ERTruncatedWrongValue, "Incorrect datetime value: '%v'", val)
		return nil
	}
	return d
}

// convertHexBlobToDecimalForNumericContext converts byte array value to unsigned int value if originType is BLOB type.
// This function is called when convertTo type is number type only. The hex literal values are parsed into blobs as
// binary string as default, but for numeric context, the value should be a number.
// Byte arrays of other SQL types are not handled here.
func convertHexBlobToDecimalForNumericContext(val interface{}, originType sql.Type) (interface{}, error) {
	if bin, isBinary := val.([]byte); isBinary && types.IsBlobType(originType) {
		stringVal := hex.EncodeToString(bin)
		decimalNum, err := strconv.ParseUint(stringVal, 16, 64)
		if err != nil {
			return nil, errors.NewKind("failed to convert hex blob value to unsigned int").New()
		}
		val = decimalNum
	}
	return val, nil
}

// castToDecimal は CAST(val AS DECIMAL(typeLength, typeScale))。name は、範囲の外のときの警告 1264 に出す列の名前
// （SELECT の項目の別名か、書いたとおりの式。項目でなければ空で、MySQL と同じく (null)）。HeimDB。
func castToDecimal(ctx *sql.Context, val interface{}, originType sql.Type, typeLength, typeScale int, name string) (interface{}, error) {
	if originType != nil {
		// HeimDB: a date, datetime or TIME is its number (stage 116)
		if n, ok := types.TemporalNumber(ctx, originType, val); ok {
			val = n
		}
	}
	value, err := convertHexBlobToDecimalForNumericContext(val, originType)
	if err != nil {
		return nil, err
	}
	dt := createConvertedDecimalType(typeLength, typeScale, false)
	if s, ok := value.(string); ok && strings.Trim(s, types.NumericCutSet) == "" {
		// HeimDB: 空の文字列は 0 だが、MySQL は DECIMAL にするときだけ警告を出す
		truncatedWarning(ctx, "DECIMAL", value)
	}
	d, _, err := dt.Convert(ctx, value)
	if err != nil {
		// HeimDB: 文字列は先頭の数の部分を読んだ値にし、MySQL と同じく警告を出す（'12.7abc' は 12.7）。
		if types.TruncatedString(value, err) && d != nil {
			truncatedWarning(ctx, "DECIMAL", value)
			return d, nil
		}
		// HeimDB: 型の範囲の外は、MySQL と同じく型の端の値（DECIMAL(5,1) なら ±9999.9）にし、警告 1264 を出す
		// （前は 0）。
		if types.ErrConvertToDecimalLimit.Is(err) {
			// 数の後ろに文字の続く文字列なら、MySQL と同じく先に 1292 も出す（'123456789abc'）
			if s, ok := value.(string); ok {
				if prefix, truncated := types.TruncateStringToDouble(s, false); truncated || prefix != strings.Trim(s, types.NumericCutSet) {
					truncatedWarning(ctx, "DECIMAL", value)
				}
			}
			return clampDecimal(ctx, dt, value, name), nil
		}
		return dt.Zero(), nil
	}
	return d, nil
}

// clampDecimal は、DECIMAL の型 dt の範囲の外の値 value を、同じ符号の端の値（10^(M-D) - 10^-D）にし、警告 1264 を
// 出す（MySQL と同じ）。HeimDB。
func clampDecimal(ctx *sql.Context, dt sql.DecimalType, value interface{}, name string) interface{} {
	max := decimal.New(1, int32(dt.Precision()-dt.Scale())).Sub(decimal.New(1, -int32(dt.Scale())))
	if negativeNumber(value) {
		max = max.Neg()
	}
	if name == "" {
		name = "(null)"
	}
	if ctx != nil && ctx.Session != nil {
		ctx.Session.Warn(&sql.Warning{Level: "Warning", Code: mysql.ERWarnDataOutOfRange, Message: fmt.Sprintf("Out of range value for column '%s' at row 1", name)})
	}
	return max.Round(int32(dt.Scale()))
}

// negativeNumber は、数か数の文字列 v が負か（DECIMAL の範囲の外の符号。型の範囲で読み直さずに決める。HeimDB）。
func negativeNumber(v interface{}) bool {
	switch v := v.(type) {
	case string:
		prefix, _ := types.TruncateStringToDouble(v, false)
		return strings.HasPrefix(prefix, "-")
	case decimal.Decimal:
		return v.Sign() < 0
	case float64:
		return v < 0
	case float32:
		return v < 0
	case int64:
		return v < 0
	case int32:
		return v < 0
	case int16:
		return v < 0
	case int8:
		return v < 0
	case int:
		return v < 0
	}
	return false
}

// truncatedWarning は、文字列を数にするとき先頭の数の部分だけを読んだことを、MySQL と同じ警告 1292 で知らせる。
// HeimDB: 前は 0 にして警告も出さなかった。
func truncatedWarning(ctx *sql.Context, typ string, v interface{}) {
	if ctx == nil || ctx.Session == nil {
		return
	}
	ctx.Session.Warn(&sql.Warning{
		Level:   "Warning",
		Code:    mysql.ERTruncatedWrongValue,
		Message: fmt.Sprintf("Truncated incorrect %s value: '%v'", typ, v),
	})
}

// castStringToInteger は、CAST(s AS SIGNED) と CAST(s AS UNSIGNED) の値を返す。HeimDB: MySQL（my_strtoll10）と同じく、
// 前後の空白を除いた先頭の整数の部分だけを読む（'12.7' は 12、'1e3' は 1、'-5x' は -5）。大きすぎる値は符号なしの
// 最大値にし、SIGNED ではその 64 ビットを符号ありとみる（MySQL も '99999999999999999999' は -1）。先頭から読めない
// 文字が残れば、警告 1292 を出す。
func castStringToInteger(ctx *sql.Context, s string) (int64, uint64) {
	trimmed := strings.Trim(s, types.IntCutSet)
	prefix, _ := types.TruncateStringToInt(trimmed)
	var signed int64
	var unsigned uint64
	overflow := false
	if strings.HasPrefix(prefix, "-") {
		n, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			overflow = true
		}
		signed, unsigned = n, uint64(n)
	} else {
		u, err := strconv.ParseUint(strings.TrimPrefix(prefix, "+"), 10, 64)
		if err != nil {
			overflow = true
		}
		signed, unsigned = int64(u), u
	}
	if prefix != trimmed || overflow {
		truncatedWarning(ctx, "INTEGER", s)
	}
	return signed, unsigned
}

// truncatedTypeName は、警告 1292 に出す型の名前（MySQL と同じ INTEGER、DOUBLE、DECIMAL）。
func truncatedTypeName(typ sql.Type) string {
	switch {
	case types.IsFloat(typ):
		return "DOUBLE"
	case types.IsDecimal(typ):
		return "DECIMAL"
	case types.IsInteger(typ):
		return "INTEGER"
	}
	return typ.String()
}
