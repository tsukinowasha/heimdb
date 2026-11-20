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

// Modified by Tsukinowa Inc.: 型の文字列で、値の ' と \ をエスケープする（上流の #2995 に \ を足した。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM の空の値（番号 0）を '' と等しくする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM に入れる文字列の後ろの空白を除く（段階 116 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM と文字列を比べるときは後ろの空白を PAD SPACE の照合順序でだけ除き、定義にない文字列とは値の文字列として比べる（段階 116 の 2 度目のレビュー。docs/engine.md）

package types

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/shopspring/decimal"
	"gopkg.in/src-d/go-errors.v1"

	"github.com/dolthub/go-mysql-server/sql/encodings"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

const (
	// EnumTypeMinElements returns the minimum number of enumerations for the Enum type.
	EnumTypeMinElements = 1
	// EnumTypeMaxElements returns the maximum number of enumerations for the Enum type.
	EnumTypeMaxElements = 65535
	// / An ENUM column can have a maximum of 65,535 distinct elements.
)

var (
	ErrConvertingToEnum = errors.NewKind("value %v is not valid for this Enum")

	ErrDataTruncatedForColumn = errors.NewKind("Data truncated for column '%s'")

	enumValueType = reflect.TypeOf(uint16(0))
)

type EnumType struct {
	collation             sql.CollationID
	hashedValToIdx        map[uint64]int
	valToIdx              map[string]int
	idxToVal              []string
	maxResponseByteLength uint32
}

var _ sql.EnumType = EnumType{}
var _ sql.CollationCoercible = EnumType{}
var _ sql.TypeWithCollation = EnumType{}

// CreateEnumType creates a EnumType.
func CreateEnumType(values []string, collation sql.CollationID) (sql.EnumType, error) {
	if len(values) < EnumTypeMinElements {
		return nil, fmt.Errorf("number of values may not be zero")
	}
	if len(values) > EnumTypeMaxElements {
		return nil, fmt.Errorf("number of values is too large")
	}

	// maxResponseByteLength for an enum type is the bytes required to send back the largest enum value,
	// including accounting for multibyte character representations.
	var maxResponseByteLength uint32
	maxCharLength := collation.Collation().CharacterSet.MaxLength()
	hashedValToIndex := make(map[uint64]int)
	valToIdx := make(map[string]int)
	for i, value := range values {
		if !collation.Equals(sql.Collation_binary) {
			// Trailing spaces are automatically deleted from ENUM member values in the table definition when a table
			// is created, unless the binary charset and collation is in use
			value = strings.TrimRight(value, " ")
		}
		values[i] = value
		hashedVal, err := collation.HashToUint(value)
		if err != nil {
			return nil, err
		}
		if _, ok := hashedValToIndex[hashedVal]; ok {
			return nil, fmt.Errorf("duplicate entry: %v", value)
		}
		// The elements listed in the column specification are assigned index numbers, beginning with 1.
		hashedValToIndex[hashedVal] = i + 1
		valToIdx[value] = i + 1

		byteLength := uint32(utf8.RuneCountInString(value) * int(maxCharLength))
		if byteLength > maxResponseByteLength {
			maxResponseByteLength = byteLength
		}
	}
	return EnumType{
		collation:             collation,
		hashedValToIdx:        hashedValToIndex,
		idxToVal:              values,
		valToIdx:              valToIdx,
		maxResponseByteLength: maxResponseByteLength,
	}, nil
}

// MustCreateEnumType is the same as CreateEnumType except it panics on errors.
func MustCreateEnumType(values []string, collation sql.CollationID) sql.EnumType {
	et, err := CreateEnumType(values, collation)
	if err != nil {
		panic(err)
	}
	return et
}

// MaxTextResponseByteLength implements the Type interface
func (t EnumType) MaxTextResponseByteLength(*sql.Context) uint32 {
	return t.maxResponseByteLength
}

// Compare implements Type interface.
func (t EnumType) Compare(ctx context.Context, a interface{}, b interface{}) (int, error) {
	if hasNulls, res := CompareNulls(a, b); hasNulls {
		return res, nil
	}

	// HeimDB: a string is compared through ConvertToCompare (no trailing-space trim
	// under a NO PAD collation), and a string that is not a value is compared with
	// the other side's value text under the collation, as MySQL compares an ENUM
	// with a string (e > 'zz', e = 'a '; the empty value, index 0, is ''; stage 116
	// second review)
	ai, err := t.ConvertToCompare(ctx, a)
	if err != nil && !ErrConvertingToEnum.Is(err) {
		return 0, err
	}
	aerr := err
	bi, err := t.ConvertToCompare(ctx, b)
	if err != nil && !ErrConvertingToEnum.Is(err) {
		return 0, err
	}
	if aerr != nil || err != nil {
		if res, ok := t.compareAsStrings(ctx, a, b); ok {
			return res, nil
		}
	}

	if ai == nil && bi == nil {
		return 0, nil
	} else if ai == nil {
		return -1, nil
	} else if bi == nil {
		return 1, nil
	}

	au := ai.(uint16)
	bu := bi.(uint16)

	if au < bu {
		return -1, nil
	} else if au > bu {
		return 1, nil
	}
	return 0, nil
}

// Convert implements Type interface.
func (t EnumType) Convert(ctx context.Context, v interface{}) (interface{}, sql.ConvertInRange, error) {
	if v == nil {
		return nil, sql.InRange, nil
	}

	switch value := v.(type) {
	case int:
		if _, ok := t.At(value); ok {
			return uint16(value), sql.InRange, nil
		}
	case uint:
		return t.Convert(ctx, int(value))
	case int8:
		return t.Convert(ctx, int(value))
	case uint8:
		return t.Convert(ctx, int(value))
	case int16:
		return t.Convert(ctx, int(value))
	case uint16:
		return t.Convert(ctx, int(value))
	case int32:
		return t.Convert(ctx, int(value))
	case uint32:
		return t.Convert(ctx, int(value))
	case int64:
		return t.Convert(ctx, int(value))
	case uint64:
		return t.Convert(ctx, int(value))
	case float32:
		return t.Convert(ctx, int(value))
	case float64:
		return t.Convert(ctx, int(value))
	case decimal.Decimal:
		return t.Convert(ctx, value.IntPart())
	case decimal.NullDecimal:
		if !value.Valid {
			return nil, sql.InRange, nil
		}
		return t.Convert(ctx, value.Decimal.IntPart())
	case string:
		if index := t.IndexOf(value); index != -1 {
			return uint16(index), sql.InRange, nil
		}
		// HeimDB: MySQL's Field_enum::store drops trailing spaces ('a ' is 'a'), as
		// SET's members already do (stage 116 review)
		if trimmed := strings.TrimRight(value, " "); trimmed != value && t.collation != sql.Collation_binary {
			if index := t.IndexOf(trimmed); index != -1 {
				return uint16(index), sql.InRange, nil
			}
		}
	case []byte:
		return t.Convert(ctx, string(value))
	}

	return nil, sql.InRange, ErrConvertingToEnum.New(v)
}

// Equals implements the Type interface.
func (t EnumType) Equals(otherType sql.Type) bool {
	if ot, ok := otherType.(EnumType); ok && t.collation.Equals(ot.collation) && len(t.idxToVal) == len(ot.idxToVal) {
		for i, val := range t.idxToVal {
			if ot.idxToVal[i] != val {
				return false
			}
		}
		return true
	}
	return false
}

// Promote implements the Type interface.
func (t EnumType) Promote() sql.Type {
	return t
}

// SQL implements Type interface.
func (t EnumType) SQL(ctx *sql.Context, dest []byte, v interface{}) (sqltypes.Value, error) {
	if v == nil {
		return sqltypes.NULL, nil
	}
	convertedValue, _, err := t.Convert(ctx, v)
	if err != nil {
		return sqltypes.Value{}, err
	}
	value, _ := t.At(int(convertedValue.(uint16)))

	resultCharset := ctx.GetCharacterSetResults()
	if resultCharset == sql.CharacterSet_Unspecified || resultCharset == sql.CharacterSet_binary {
		resultCharset = t.collation.CharacterSet()
	}
	encodedBytes, ok := resultCharset.Encoder().Encode(encodings.StringToBytes(value))
	if !ok {
		snippet := value
		if len(snippet) > 50 {
			snippet = snippet[:50]
		}
		snippet = strings.ToValidUTF8(snippet, string(utf8.RuneError))
		return sqltypes.Value{}, sql.ErrCharSetFailedToEncode.New(resultCharset.Name(), utf8.ValidString(value), snippet)
	}
	val := encodedBytes

	return sqltypes.MakeTrusted(sqltypes.Enum, val), nil
}

// String implements Type interface.
func (t EnumType) String() string {
	return t.StringWithTableCollation(sql.Collation_Default)
}

// Type implements Type interface.
func (t EnumType) Type() query.Type {
	return sqltypes.Enum
}

// ValueType implements Type interface.
func (t EnumType) ValueType() reflect.Type {
	return enumValueType
}

// CollationCoercibility implements sql.CollationCoercible interface.
func (t EnumType) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return t.collation, 4
}

// Zero implements Type interface.
func (t EnumType) Zero() interface{} {
	// TODO: If an ENUM column is declared NOT NULL, its default value is the first element of the list of permitted values.
	return uint16(0)
}

// At implements EnumType interface.
func (t EnumType) At(idx int) (string, bool) {
	// for index zero, the value is empty. It's used for insert ignore.
	if idx < 0 {
		return "", false
	}
	if idx == 0 {
		return "", true
	}
	if idx > len(t.idxToVal) {
		return "", false
	}
	// The elements listed in the column specification are assigned index numbers, beginning with 1.
	return t.idxToVal[idx-1], true
}

// CharacterSet implements EnumType interface.
func (t EnumType) CharacterSet() sql.CharacterSetID {
	return t.collation.CharacterSet()
}

// Collation implements EnumType interface.
func (t EnumType) Collation() sql.CollationID {
	return t.collation
}

// IndexOf implements EnumType interface.
func (t EnumType) IndexOf(v string) int {
	if idx, ok := t.valToIdx[v]; ok {
		return idx
	}
	hashedVal, err := t.collation.HashToUint(v)
	if err == nil {
		if index, ok := t.hashedValToIdx[hashedVal]; ok {
			return index
		}
	}
	// / ENUM('0','1','2')
	// / If you store '3', it does not match any enumeration value, so it is treated as an index and becomes '2' (the value with index 3).
	if parsedIndex, err := strconv.ParseInt(v, 10, 32); err == nil {
		if _, ok := t.At(int(parsedIndex)); ok {
			return int(parsedIndex)
		}
	}
	return -1
}

// IsSubsetOf implements the sql.EnumType interface.
func (t EnumType) IsSubsetOf(otherType sql.EnumType) bool {
	if ot, ok := otherType.(EnumType); ok && t.collation.Equals(ot.collation) && len(t.idxToVal) <= len(ot.idxToVal) {
		for i, val := range t.idxToVal {
			if ot.idxToVal[i] != val {
				return false
			}
		}
		return true
	}
	return false
}

// NumberOfElements implements EnumType interface.
func (t EnumType) NumberOfElements() uint16 {
	return uint16(len(t.idxToVal))
}

// Values implements EnumType interface.
func (t EnumType) Values() []string {
	vals := make([]string, len(t.idxToVal))
	copy(vals, t.idxToVal)
	return vals
}

// WithNewCollation implements sql.TypeWithCollation interface.
func (t EnumType) WithNewCollation(collation sql.CollationID) (sql.Type, error) {
	return CreateEnumType(t.idxToVal, collation)
}

// enumValueQuoter escapes an ENUM or SET value for a single-quoted string in a type string by doubling single quotes and
// backslashes, as MySQL's SHOW CREATE TABLE does. The type string is parsed again when a table definition is read back.
var enumValueQuoter = strings.NewReplacer(`\`, `\\`, "'", "''")

func quoteEnumValue(v string) string {
	return enumValueQuoter.Replace(v)
}

// StringWithTableCollation implements sql.TypeWithCollation interface.
func (t EnumType) StringWithTableCollation(tableCollation sql.CollationID) string {
	escapedValues := make([]string, len(t.idxToVal))
	for i, value := range t.idxToVal {
		escapedValues[i] = quoteEnumValue(value)
	}
	s := fmt.Sprintf("enum('%s')", strings.Join(escapedValues, `','`))
	if t.CharacterSet() != tableCollation.CharacterSet() {
		s += " CHARACTER SET " + t.CharacterSet().String()
	}
	if t.collation != tableCollation {
		s += " COLLATE " + t.collation.String()
	}
	return s
}

// ConvertToCompare converts v to an ENUM index for a comparison (HeimDB, stage 116
// second review). Unlike Convert, which drops trailing spaces as MySQL's
// Field_enum::store does, it drops them only under a PAD SPACE collation, so that
// e = 'a ' does not match 'a' under utf8mb4_0900_ai_ci (NO PAD), as in MySQL. A
// []byte (BINARY 'a') matches a value's bytes exactly. An empty string is the empty value
// (index 0), which a non-strict INSERT stores for an invalid value.
func (t EnumType) ConvertToCompare(ctx context.Context, v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case string:
		if index := t.IndexOf(x); index != -1 {
			return uint16(index), nil
		}
		if t.padSpace() {
			if trimmed := strings.TrimRight(x, " "); trimmed != x {
				if trimmed == "" {
					return uint16(0), nil
				}
				if index := t.IndexOf(trimmed); index != -1 {
					return uint16(index), nil
				}
			}
		}
		if x == "" {
			return uint16(0), nil
		}
		return nil, ErrConvertingToEnum.New(v)
	case []byte:
		for i, val := range t.idxToVal {
			if val == string(x) {
				return uint16(i + 1), nil
			}
		}
		if len(x) == 0 {
			return uint16(0), nil
		}
		return nil, ErrConvertingToEnum.New(v)
	}
	converted, _, err := t.Convert(ctx, v)
	return converted, err
}

// padSpace reports whether the collation ignores trailing spaces (HeimDB).
func (t EnumType) padSpace() bool {
	return t.collation != sql.Collation_binary && t.collation.PadAttribute() == "PAD SPACE"
}

// compareAsStrings compares an ENUM value (index) or a string with a string that is
// not a value as strings under the collation, as MySQL does (a []byte compares as
// bytes). ok is false unless both sides have a text (HeimDB, stage 116 second review).
func (t EnumType) compareAsStrings(ctx context.Context, a, b interface{}) (int, bool) {
	binary := t.collation == sql.Collation_binary
	str := func(v interface{}) (string, bool) {
		switch x := v.(type) {
		case string:
			return x, true
		case []byte:
			binary = true
			return string(x), true
		case uint16:
			return t.At(int(x))
		}
		return "", false
	}
	as, aok := str(a)
	bs, bok := str(b)
	if !aok || !bok {
		return 0, false
	}
	if binary {
		return strings.Compare(as, bs), true
	}
	res, err := CreateLongText(t.collation).Compare(ctx, as, bs)
	if err != nil {
		return 0, false
	}
	return res, true
}
