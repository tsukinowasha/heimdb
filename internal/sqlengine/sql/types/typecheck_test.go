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
// Modified by Tsukinowa Inc.: IsSigned と IsUnsigned が元の比較と同じ結果になるテストを足す（docs/engine.md）

package types

import (
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/stretchr/testify/assert"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

func TestIsGeometry(t *testing.T) {
	assert.True(t, IsGeometry(GeometryType{}))
	assert.True(t, IsGeometry(PointType{}))
	assert.True(t, IsGeometry(LineStringType{}))
	assert.True(t, IsGeometry(PolygonType{}))
	assert.False(t, IsGeometry(StringType{}))
	assert.False(t, IsGeometry(JSON))
	assert.False(t, IsGeometry(Blob))
}

func TestIsJSON(t *testing.T) {
	assert.True(t, IsJSON(JSON))
	assert.False(t, IsJSON(Blob))
	assert.False(t, IsJSON(NumberTypeImpl_{}))
	assert.False(t, IsJSON(StringType{}))
}

func TestSystemTypesIsNumber(t *testing.T) {
	assert.True(t, IsNumber(SystemBoolType{}))
	assert.True(t, IsNumber(systemIntType{}))
	assert.True(t, IsNumber(systemUintType{}))
	assert.True(t, IsNumber(systemDoubleType{}))
	assert.False(t, IsNumber(systemEnumType{}))
	assert.False(t, IsNumber(systemSetType{}))
	assert.False(t, IsNumber(systemStringType{}))
}

func TestSystemTypesIsSigned(t *testing.T) {
	assert.True(t, IsSigned(SystemBoolType{}))
	assert.True(t, IsSigned(systemIntType{}))
	assert.False(t, IsSigned(systemUintType{}))
	assert.False(t, IsSigned(systemDoubleType{}))
	assert.False(t, IsSigned(systemEnumType{}))
	assert.False(t, IsSigned(systemSetType{}))
	assert.False(t, IsSigned(systemStringType{}))
}

func TestSystemTypesIsUnsigned(t *testing.T) {
	assert.False(t, IsUnsigned(SystemBoolType{}))
	assert.False(t, IsUnsigned(systemIntType{}))
	assert.True(t, IsUnsigned(systemUintType{}))
	assert.False(t, IsUnsigned(systemDoubleType{}))
	assert.False(t, IsUnsigned(systemEnumType{}))
	assert.False(t, IsUnsigned(systemSetType{}))
	assert.False(t, IsUnsigned(systemStringType{}))
}

func TestSystemTypesIsText(t *testing.T) {
	assert.False(t, IsText(SystemBoolType{}))
	assert.False(t, IsText(systemIntType{}))
	assert.False(t, IsText(systemUintType{}))
	assert.False(t, IsText(systemDoubleType{}))
	assert.False(t, IsText(systemEnumType{}))
	assert.False(t, IsText(systemSetType{}))
	assert.True(t, IsText(systemStringType{}))
}

func TestSystemTypesIsEnum(t *testing.T) {
	assert.False(t, IsEnum(SystemBoolType{}))
	assert.False(t, IsEnum(systemIntType{}))
	assert.False(t, IsEnum(systemUintType{}))
	assert.False(t, IsEnum(systemDoubleType{}))
	assert.True(t, IsEnum(systemEnumType{}))
	assert.False(t, IsEnum(systemSetType{}))
	assert.False(t, IsEnum(systemStringType{}))
}

func TestSystemTypesIsSet(t *testing.T) {
	assert.False(t, IsSet(SystemBoolType{}))
	assert.False(t, IsSet(systemIntType{}))
	assert.False(t, IsSet(systemUintType{}))
	assert.False(t, IsSet(systemDoubleType{}))
	assert.False(t, IsSet(systemEnumType{}))
	assert.True(t, IsSet(NewSystemSetType("", sql.Collation_Default, "")))
	assert.False(t, IsSet(systemStringType{}))
}

// IsSigned と IsUnsigned は、元の実装（型をインターフェースの比較で調べる）と同じ結果になる。すべての数の型、
// 数でない型、SystemVariableType で包んだものを確かめる（表示幅が 0 か TINYINT(1) の 1 しかないことは、HeimDB の
// internal/schema の TestNumberTypeDisplayWidths が、WordPress の DDL の型で確かめる）。
func TestIsSignedUnsignedMatchEquality(t *testing.T) {
	oldSigned := func(typ sql.Type) bool {
		if svt, ok := typ.(sql.SystemVariableType); ok {
			typ = svt.UnderlyingType()
		}
		return typ == Int8 || typ == Int16 || typ == Int24 || typ == Int32 || typ == Int64 || typ == Boolean
	}
	oldUnsigned := func(typ sql.Type) bool {
		if svt, ok := typ.(sql.SystemVariableType); ok {
			typ = svt.UnderlyingType()
		}
		return typ == Uint8 || typ == Uint16 || typ == Uint24 || typ == Uint32 || typ == Uint64
	}
	int64Impl := Int64.(NumberTypeImpl_)
	typs := []sql.Type{
		Int8, Int16, Int24, Int32, Int64, Boolean, Uint8, Uint16, Uint24, Uint32, Uint64, Float32, Float64,
		MustCreateNumberTypeWithDisplayWidth(sqltypes.Int8, 1),
		MustCreateDecimalType(10, 2), InternalDecimalType, LongText, Text, Blob, Datetime, Timestamp, Date, Time, Year,
		JSON, Null, MustCreateBitType(8), MustCreateEnumType([]string{"a"}, sql.Collation_Default),
		NewSystemBoolType("b"), NewSystemIntType("i", -10, 10, false), NewSystemUintType("u", 0, 10),
		NewSystemDoubleType("d", 0, 1), NewSystemStringType("s"), NewSystemEnumType("e", "a", "b"),
		NewSystemSetType("st", sql.Collation_Default, "a", "b"),
		// 実際には作られない値（nil、空の値、ポインタ）も、元と同じく偽になる。
		nil, NumberTypeImpl_{}, &int64Impl,
	}
	for _, base := range []query.Type{sqltypes.Int8, sqltypes.Int16, sqltypes.Int24, sqltypes.Int32, sqltypes.Int64,
		sqltypes.Uint8, sqltypes.Uint16, sqltypes.Uint24, sqltypes.Uint32, sqltypes.Uint64, sqltypes.Float32, sqltypes.Float64} {
		typs = append(typs, MustCreateNumberType(base))
	}
	nSigned, nUnsigned := 0, 0
	for _, typ := range typs {
		assert.Equal(t, oldSigned(typ), IsSigned(typ), "IsSigned(%v)", typ)
		assert.Equal(t, oldUnsigned(typ), IsUnsigned(typ), "IsUnsigned(%v)", typ)
		if IsSigned(typ) {
			nSigned++
		}
		if IsUnsigned(typ) {
			nUnsigned++
		}
	}
	// 数の型と、整数のシステム変数の型の両方が真になる（比較が全部偽になる誤りを避ける）。
	assert.Greater(t, nSigned, 7)
	assert.Greater(t, nUnsigned, 6)
}
