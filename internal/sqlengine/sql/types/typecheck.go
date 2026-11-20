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
//
// Modified by Tsukinowa Inc.: IsSigned と IsUnsigned を、型の判定 1 回と元の型の種類の switch にする（docs/engine.md）
// Modified by Tsukinowa Inc.: IsFloat を元の型の種類で調べ、(M,D) を持つ FLOAT と DOUBLE も含める（段階 120。docs/engine.md）

package types

import (
	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// IsBoolean checks if t is a boolean type.
func IsBoolean(t sql.Type) bool {
	return t == Boolean
}

// IsBlobType checks if t is BLOB
func IsBlobType(t sql.Type) bool {
	if t == nil {
		return false
	}
	switch t.Type() {
	case sqltypes.Blob:
		return true
	default:
		return false
	}
}

// IsBinaryType checks if t is BINARY, VARBINARY, or BLOB
func IsBinaryType(t sql.Type) bool {
	if t == nil {
		return false
	}
	switch t.Type() {
	case sqltypes.Binary, sqltypes.VarBinary, sqltypes.Blob, sqltypes.TypeJSON, sqltypes.Geometry:
		return true
	default:
		return false
	}
}

// IsDecimal checks if t is a DECIMAL type.
func IsDecimal(t sql.Type) bool {
	_, ok := t.(DecimalType_)
	return ok
}

// IsBit checks if t is a BIT type.
func IsBit(t sql.Type) bool {
	_, ok := t.(BitType_)
	return ok
}

// IsFloat64 は、t が DOUBLE（(M,D) と UNSIGNED と小数の桁の決まった式の型を含む）かを返す（HeimDB: 段階 120。前は
// t == Float64 で比べていた）。
func IsFloat64(t sql.Type) bool {
	n, ok := t.(NumberTypeImpl_)
	return ok && n.baseType == sqltypes.Float64
}

// IsFloat checks if t is float type.
func IsFloat(t sql.Type) bool {
	// HeimDB: (M,D) と UNSIGNED を持つ FLOAT と DOUBLE も含める（元の型の種類で調べる。段階 120）
	if n, ok := t.(NumberTypeImpl_); ok {
		return n.baseType == sqltypes.Float32 || n.baseType == sqltypes.Float64
	}
	return false
}

// IsInteger checks if t is an integer type.
func IsInteger(t sql.Type) bool {
	return IsSigned(t) || IsUnsigned(t)
}

// IsJSON returns true if the specified type is a JSON type.
func IsJSON(t sql.Type) bool {
	_, ok := t.(JsonType)
	return ok
}

// IsGeometry returns true if the specified type is a Geometry type.
func IsGeometry(t sql.Type) bool {
	switch t.(type) {
	case GeometryType, PointType, LineStringType, PolygonType:
		return true
	default:
		return false
	}
}

// IsNull returns true if expression is nil or is Null Type, otherwise false.
func IsNull(ex sql.Expression) bool {
	return ex == nil || ex.Type() == Null
}

// IsNumber checks if t is a number type
func IsNumber(t sql.Type) bool {
	switch typ := t.(type) {
	case sql.SystemVariableType:
		return IsNumber(typ.UnderlyingType())
	case NumberTypeImpl_, DecimalType_, BitType_, YearType_:
		return true
	default:
		return false
	}
}

// IsSigned checks if t is a signed type.
func IsSigned(t sql.Type) bool {
	if svt, ok := t.(sql.SystemVariableType); ok {
		t = svt.UnderlyingType()
	}
	// 型をインターフェースの比較で 1 つずつ調べると、そのたびに構造体を比べて遅い（HeimDB が主キーで引くたびに呼ぶ）。
	// 数の型は CreateNumberTypeWithDisplayWidth だけが作り、表示幅は 0 か TINYINT(1)（Boolean）の 1 なので、
	// 元の型の種類だけで同じ結果になる。
	if n, ok := t.(NumberTypeImpl_); ok {
		switch n.baseType {
		case sqltypes.Int8, sqltypes.Int16, sqltypes.Int24, sqltypes.Int32, sqltypes.Int64:
			return true
		}
	}
	return false
}

// IsText checks if t is a CHAR, VARCHAR, TEXT, BINARY, VARBINARY, or BLOB (including TEXT and BLOB variants).
func IsText(t sql.Type) bool {
	switch typ := t.(type) {
	case sql.SystemVariableType:
		return IsText(typ.UnderlyingType())
	case StringType:
		return true
	case ExtendedType:
		_, isString := typ.Zero().(string)
		return isString
	default:
		return false
	}
}

// IsTextBlob checks if t is one of the TEXTs or BLOBs.
func IsTextBlob(t sql.Type) bool {
	if t == nil {
		return false
	}
	switch t.Type() {
	case sqltypes.Text, sqltypes.Blob:
		return true
	default:
		return false
	}
}

// IsTextOnly checks if t is CHAR, VARCHAR, or one of the TEXTs.
func IsTextOnly(t sql.Type) bool {
	if t == nil {
		return false
	}
	switch t.Type() {
	case sqltypes.Char, sqltypes.VarChar, sqltypes.Text:
		return true
	default:
		return false
	}
}

// IsTimespan checks if t is a time (timespan)
func IsTimespan(t sql.Type) bool {
	_, ok := t.(TimespanType_)
	return ok
}

// IsTime checks if t is a timestamp, date or datetime
func IsTime(t sql.Type) bool {
	_, ok := t.(datetimeType)
	return ok
}

// IsDateType checks if t is a date
func IsDateType(t sql.Type) bool {
	dt, ok := t.(datetimeType)
	return ok && dt.baseType == sqltypes.Date
}

// IsDatetimeType checks if t is a datetime
func IsDatetimeType(t sql.Type) bool {
	dt, ok := t.(datetimeType)
	return ok && dt.baseType == sqltypes.Datetime
}

// IsTimestampType checks if t is a timestamp
func IsTimestampType(t sql.Type) bool {
	dt, ok := t.(datetimeType)
	return ok && dt.baseType == sqltypes.Timestamp
}

// IsEnum checks if t is a enum
func IsEnum(t sql.Type) bool {
	switch typ := t.(type) {
	case sql.SystemVariableType:
		return IsEnum(typ.UnderlyingType())
	case EnumType:
		return true
	default:
		return false
	}
}

// IsSet checks if t is a set
func IsSet(t sql.Type) bool {
	switch typ := t.(type) {
	case sql.SystemVariableType:
		return IsSet(typ.UnderlyingType())
	case SetType:
		return true
	default:
		return false
	}
}

// IsTuple checks if t is a tuple type.
// Note that TupleType instances with just 1 value are not considered
// as a tuple, but a parenthesized value.
func IsTuple(t sql.Type) bool {
	v, ok := t.(TupleType)
	return ok && len(v) > 1
}

// IsUnsigned checks if t is an unsigned type.
func IsUnsigned(t sql.Type) bool {
	if svt, ok := t.(sql.SystemVariableType); ok {
		t = svt.UnderlyingType()
	}
	// IsSigned と同じく、元の型の種類で調べる。
	if n, ok := t.(NumberTypeImpl_); ok {
		switch n.baseType {
		case sqltypes.Uint8, sqltypes.Uint16, sqltypes.Uint24, sqltypes.Uint32, sqltypes.Uint64:
			return true
		}
	}
	return false
}

// IsYear checks if t is a year type.
func IsYear(t sql.Type) bool {
	_, ok := t.(YearType_)
	return ok
}
