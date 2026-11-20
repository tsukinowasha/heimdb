// Package rowenc は、行の値（Pebble の値の側）と主キー（キーの側）の符号化を提供する。
//
// 行の値は、go-mysql-server が sql.Row に入れる Go の値を「型の印 + 中身」でそのまま保存する。
// 列の型（sql.Type）を経由した変換をしないので、memory パッケージと同じ値が失われずに戻る。
package rowenc

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// ErrCorrupt は、復号しようとしたバイト列が形式に合わないことを表す。
var ErrCorrupt = errors.New("rowenc: corrupt row encoding")

// 値の型の印。値を保存した後で番号を変えてはいけない。
const (
	tNull byte = iota
	tInt8
	tInt16
	tInt32
	tInt64
	tUint8
	tUint16
	tUint32
	tUint64
	tFloat32
	tFloat64
	tString
	tBytes
	tTime
	tDecimal
	tJSON
	tTimespan
	tBool
)

// EncodeRow は、テーブル定義のバージョン schemaVersion と行 row を値のバイト列にする。
func EncodeRow(schemaVersion uint64, row sql.Row) ([]byte, error) {
	b := binary.BigEndian.AppendUint64(nil, schemaVersion)
	b = binary.AppendUvarint(b, uint64(len(row)))
	for i, v := range row {
		var err error
		if b, err = appendValue(b, v); err != nil {
			return nil, fmt.Errorf("column %d: %w", i, err)
		}
	}
	return b, nil
}

// DecodeRow は EncodeRow で作ったバイト列を読む。
func DecodeRow(b []byte) (schemaVersion uint64, row sql.Row, err error) {
	if len(b) < 8 {
		return 0, nil, ErrCorrupt
	}
	schemaVersion = binary.BigEndian.Uint64(b)
	b = b[8:]
	n, k := binary.Uvarint(b)
	if k <= 0 {
		return 0, nil, ErrCorrupt
	}
	b = b[k:]
	row = make(sql.Row, n)
	for i := range row {
		if row[i], b, err = decodeValue(b); err != nil {
			return 0, nil, err
		}
	}
	if len(b) != 0 {
		return 0, nil, ErrCorrupt
	}
	return schemaVersion, row, nil
}

// DecodeRowColumns は DecodeRow と同じだが、want が false の列は読み飛ばして NULL のままにする（文が使う列だけを
// 復号する。docs/plan.md の段階 27）。want が nil ならすべての列を復号する。
func DecodeRowColumns(b []byte, want []bool) (schemaVersion uint64, row sql.Row, err error) {
	if want == nil {
		return DecodeRow(b)
	}
	if len(b) < 8 {
		return 0, nil, ErrCorrupt
	}
	schemaVersion = binary.BigEndian.Uint64(b)
	b = b[8:]
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return 0, nil, ErrCorrupt
	}
	b = b[k:]
	row = make(sql.Row, n)
	if err := decodeColumnsTo(b, row, want); err != nil {
		return 0, nil, err
	}
	return schemaVersion, row, nil
}

// DecodeRowColumnsInto は DecodeRowColumns と同じだが、dst の容量が列の数以上なら、行を dst の上に作る（want が false
// の列は NULL にする）。全件の走査で、読んだ行の列をすぐに写す側（backend の projectIter）のために、行ごとに行を
// 確保しない（docs/plan.md の段階 125）。返した行の値は dst の前の値と共有しない（文字列とバイト列は値ごとに作る）。
func DecodeRowColumnsInto(b []byte, want []bool, dst sql.Row) (schemaVersion uint64, row sql.Row, err error) {
	if len(b) < 8 {
		return 0, nil, ErrCorrupt
	}
	schemaVersion = binary.BigEndian.Uint64(b)
	b = b[8:]
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return 0, nil, ErrCorrupt
	}
	b = b[k:]
	if uint64(cap(dst)) >= n {
		row = dst[:n]
		clear(row)
	} else {
		row = make(sql.Row, n)
	}
	if err := decodeColumnsTo(b, row, want); err != nil {
		return 0, nil, err
	}
	return schemaVersion, row, nil
}

// decodeColumnsTo は、行の列の値の並び b を row に読む（want が nil ならすべて。false の列は読み飛ばして row の値の
// まま）。
func decodeColumnsTo(b []byte, row sql.Row, want []bool) error {
	var err error
	for i := range row {
		if want == nil || (i < len(want) && want[i]) {
			row[i], b, err = decodeValue(b)
		} else {
			b, err = skipValue(b)
		}
		if err != nil {
			return err
		}
	}
	if len(b) != 0 {
		return ErrCorrupt
	}
	return nil
}

// skipValue は、値を 1 つ読み飛ばす。
func skipValue(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, ErrCorrupt
	}
	tag, b := b[0], b[1:]
	switch tag {
	case tNull:
		return b, nil
	case tBool:
		if len(b) < 1 {
			return nil, ErrCorrupt
		}
		return b[1:], nil
	case tInt8, tInt16, tInt32, tInt64, tTimespan:
		_, k := binary.Varint(b)
		if k <= 0 {
			return nil, ErrCorrupt
		}
		return b[k:], nil
	case tUint8, tUint16, tUint32, tUint64:
		_, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, ErrCorrupt
		}
		return b[k:], nil
	case tFloat32:
		if len(b) < 4 {
			return nil, ErrCorrupt
		}
		return b[4:], nil
	case tFloat64:
		if len(b) < 8 {
			return nil, ErrCorrupt
		}
		return b[8:], nil
	case tString, tBytes, tTime, tDecimal, tJSON:
		n, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < n {
			return nil, ErrCorrupt
		}
		return b[k+int(n):], nil
	}
	return nil, ErrCorrupt
}

// EncodeValues は、値の並び vals を行の値と同じ形で符号化する（バージョンを持たない）。インデックスのエントリに列の値を
// 持たせるのに使う（docs/plan.md の段階 27）。
func EncodeValues(vals []any) ([]byte, error) {
	b := binary.AppendUvarint(nil, uint64(len(vals)))
	for i, v := range vals {
		var err error
		if b, err = appendValue(b, v); err != nil {
			return nil, fmt.Errorf("value %d: %w", i, err)
		}
	}
	return b, nil
}

// DecodeValues は EncodeValues で作ったバイト列を読む。
func DecodeValues(b []byte) ([]any, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return nil, ErrCorrupt
	}
	b = b[k:]
	vals := make([]any, n)
	for i := range vals {
		var err error
		if vals[i], b, err = decodeValue(b); err != nil {
			return nil, err
		}
	}
	if len(b) != 0 {
		return nil, ErrCorrupt
	}
	return vals, nil
}

// DecodeValuesTo は EncodeValues で作ったバイト列を読み、i 番目の値を row[ords[i]] に置く。want が nil でなく
// want[ords[i]] が false の値は読み飛ばす（row はそのまま）。値の数が ords と違えば ErrCorrupt。インデックスの
// エントリだけで行を作るときに、値の並びを別に作らずに済ませる（docs/plan.md の段階 78）。
func DecodeValuesTo(b []byte, row sql.Row, ords []int, want []bool) error {
	n, k := binary.Uvarint(b)
	if k <= 0 || n != uint64(len(ords)) {
		return ErrCorrupt
	}
	b = b[k:]
	for _, ord := range ords {
		var err error
		if want == nil || want[ord] {
			row[ord], b, err = decodeValue(b)
		} else {
			b, err = skipValue(b)
		}
		if err != nil {
			return err
		}
	}
	if len(b) != 0 {
		return ErrCorrupt
	}
	return nil
}

func appendValue(b []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return append(b, tNull), nil
	case bool:
		if v {
			return append(b, tBool, 1), nil
		}
		return append(b, tBool, 0), nil
	case int8:
		return binary.AppendVarint(append(b, tInt8), int64(v)), nil
	case int16:
		return binary.AppendVarint(append(b, tInt16), int64(v)), nil
	case int32:
		return binary.AppendVarint(append(b, tInt32), int64(v)), nil
	case int64:
		return binary.AppendVarint(append(b, tInt64), v), nil
	case int:
		return binary.AppendVarint(append(b, tInt64), int64(v)), nil
	case uint8:
		return binary.AppendUvarint(append(b, tUint8), uint64(v)), nil
	case uint16:
		return binary.AppendUvarint(append(b, tUint16), uint64(v)), nil
	case uint32:
		return binary.AppendUvarint(append(b, tUint32), uint64(v)), nil
	case uint64:
		return binary.AppendUvarint(append(b, tUint64), v), nil
	case uint:
		return binary.AppendUvarint(append(b, tUint64), uint64(v)), nil
	case float32:
		return binary.BigEndian.AppendUint32(append(b, tFloat32), math.Float32bits(v)), nil
	case float64:
		return binary.BigEndian.AppendUint64(append(b, tFloat64), math.Float64bits(v)), nil
	case string:
		return appendLenPrefixed(append(b, tString), []byte(v)), nil
	case []byte:
		return appendLenPrefixed(append(b, tBytes), v), nil
	case time.Time:
		tb, err := v.MarshalBinary()
		if err != nil {
			return nil, err
		}
		return appendLenPrefixed(append(b, tTime), tb), nil
	case decimal.Decimal:
		return appendLenPrefixed(append(b, tDecimal), []byte(v.String())), nil
	case types.JSONDocument:
		jb, err := json.Marshal(v.Val)
		if err != nil {
			return nil, err
		}
		return appendLenPrefixed(append(b, tJSON), jb), nil
	case types.Timespan:
		return binary.AppendVarint(append(b, tTimespan), int64(v)), nil
	default:
		return nil, fmt.Errorf("HeimDB: unsupported value type %T", v)
	}
}

func appendLenPrefixed(b, v []byte) []byte {
	return append(binary.AppendUvarint(b, uint64(len(v))), v...)
}

func decodeValue(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, ErrCorrupt
	}
	tag, b := b[0], b[1:]
	switch tag {
	case tNull:
		return nil, b, nil
	case tBool:
		if len(b) < 1 {
			return nil, nil, ErrCorrupt
		}
		return b[0] != 0, b[1:], nil
	case tInt8, tInt16, tInt32, tInt64:
		v, k := binary.Varint(b)
		if k <= 0 {
			return nil, nil, ErrCorrupt
		}
		switch tag {
		case tInt8:
			return int8(v), b[k:], nil
		case tInt16:
			return int16(v), b[k:], nil
		case tInt32:
			return int32(v), b[k:], nil
		}
		return v, b[k:], nil
	case tUint8, tUint16, tUint32, tUint64:
		v, k := binary.Uvarint(b)
		if k <= 0 {
			return nil, nil, ErrCorrupt
		}
		switch tag {
		case tUint8:
			return uint8(v), b[k:], nil
		case tUint16:
			return uint16(v), b[k:], nil
		case tUint32:
			return uint32(v), b[k:], nil
		}
		return v, b[k:], nil
	case tFloat32:
		if len(b) < 4 {
			return nil, nil, ErrCorrupt
		}
		return math.Float32frombits(binary.BigEndian.Uint32(b)), b[4:], nil
	case tFloat64:
		if len(b) < 8 {
			return nil, nil, ErrCorrupt
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b)), b[8:], nil
	case tTimespan:
		v, k := binary.Varint(b)
		if k <= 0 {
			return nil, nil, ErrCorrupt
		}
		return types.Timespan(v), b[k:], nil
	}

	n, k := binary.Uvarint(b)
	if k <= 0 || uint64(len(b)-k) < n {
		return nil, nil, ErrCorrupt
	}
	payload, rest := b[k:k+int(n)], b[k+int(n):]
	switch tag {
	case tString:
		return string(payload), rest, nil
	case tBytes:
		return append([]byte{}, payload...), rest, nil
	case tTime:
		var t time.Time
		if err := t.UnmarshalBinary(payload); err != nil {
			return nil, nil, ErrCorrupt
		}
		return t, rest, nil
	case tDecimal:
		d, err := decimal.NewFromString(string(payload))
		if err != nil {
			return nil, nil, ErrCorrupt
		}
		return d, rest, nil
	case tJSON:
		var v any
		if err := json.Unmarshal(payload, &v); err != nil {
			return nil, nil, ErrCorrupt
		}
		return types.JSONDocument{Val: v}, rest, nil
	}
	return nil, nil, ErrCorrupt
}

// EncodePK は行の主キーの列をキー用に符号化する。主キーの列の型は PKType を満たす（docs/plan.md の「順序を保つ
// 符号化」）。
func EncodePK(b []byte, sch sql.PrimaryKeySchema, row sql.Row) ([]byte, error) {
	for _, ord := range sch.PkOrdinals {
		var err error
		if b, err = AppendPKValue(b, sch.Schema[ord].Type, row[ord]); err != nil {
			return nil, fmt.Errorf("primary key column %s: %w", sch.Schema[ord].Name, err)
		}
	}
	return b, nil
}

// AppendPKValue は、型 t の主キーの列の値 v を EncodePK と同じ形で追記する（行を作らずに主キーを符号化する）。
// 文字列、バイト列、日時はインデックスと同じ形で、NULL の印は付けない（段階 38、70）。
func AppendPKValue(b []byte, t sql.Type, v any) ([]byte, error) {
	if v == nil {
		return nil, errors.New("HeimDB: NULL in primary key")
	}
	switch {
	case types.IsInteger(t), types.IsEnum(t):
		// ENUM の値は、エンジンが行に入れる番号（uint16）。MySQL も番号の順に並べる
		return appendKeyInt(b, v)
	case types.IsTime(t):
		tm, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("HeimDB: expected time.Time for %s, got %T", t, v)
		}
		return appendKeyTime(b, tm), nil
	case isBinaryString(t):
		raw, err := bytesValue(t, v)
		if err != nil {
			return nil, err
		}
		return keyenc.AppendBytes(b, raw), nil
	case isText(t):
		out, _, err := appendTextKey(b, t, v, 0)
		return out, err
	}
	return nil, fmt.Errorf("HeimDB: type %s cannot be used in a primary key", t)
}

// PKType は、t が主キーに使える型かどうか（段階 70）。整数、文字列（StringPKType）、BINARY、VARBINARY、DATE、
// DATETIME、TIMESTAMP、ENUM。BINARY(n) の値は、エンジンが書くときに末尾を 0x00 で埋めて n バイトにそろえる。
func PKType(t sql.Type) bool {
	if isBinaryString(t) {
		return t.Type() == sqltypes.Binary || t.Type() == sqltypes.VarBinary
	}
	return types.IsInteger(t) || types.IsTime(t) || types.IsEnum(t) || StringPKType(t)
}

// PKKeyKind は、主キーの列の型 t のキーの形を表す文字列（段階 70）。キーのバイト列の比べ方が同じ型の変更（int を
// bigint に、varchar(20) を varchar(30) に、datetime を datetime(6) に）では変わらない。整数はどれも同じ形、文字列は
// CHAR か VARCHAR と照合順序、BINARY は長さ（値を埋める長さ）、ENUM は値の並び（番号で持つ）で区別する。
func PKKeyKind(t sql.Type) string {
	switch {
	case types.IsInteger(t):
		return "int"
	case types.IsEnum(t):
		values, _ := json.Marshal(t.(types.EnumType).Values())
		return "enum" + string(values)
	case isBinaryString(t):
		if t.Type() == sqltypes.Binary {
			return t.String()
		}
		return "varbinary"
	case isText(t):
		return fmt.Sprintf("%s %s", t.Type(), t.(sql.TypeWithCollation).Collation())
	}
	return t.Type().String()
}

// StringPKType は、t が文字列の主キーに使える型（utf8mb4 か utf8mb3 の照合順序の CHAR か VARCHAR）かどうか（段階 38）。
func StringPKType(t sql.Type) bool {
	st, ok := t.(types.StringType)
	if !ok || !isText(t) || (st.Type() != sqltypes.Char && st.Type() != sqltypes.VarChar) {
		return false
	}
	c := st.Collation()
	// utf8mb3 は保存のときに utf8mb4 の同じ名前の照合順序になる（段階 37）。
	return c.Sorter() != nil && (c.CharacterSet() == sql.CharacterSet_utf8mb4 || c.CharacterSet() == sql.CharacterSet_utf8mb3)
}

// appendKeyInt は整数の値をキー用に符号化する。符号付きと符号なしの値を同じ順序で比べられるよう、
// 符号付きの値の符号化（keyenc.AppendInt64）にそろえる。uint64 で int64 に収まらない値は、
// 非負の値のさらに後ろに並ぶよう 0x01 の印を付けて区別する。
func appendKeyInt(b []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return nil, errors.New("HeimDB: NULL in primary key")
	case int8:
		return appendSigned(b, int64(v)), nil
	case int16:
		return appendSigned(b, int64(v)), nil
	case int32:
		return appendSigned(b, int64(v)), nil
	case int64:
		return appendSigned(b, v), nil
	case int:
		return appendSigned(b, int64(v)), nil
	case uint8:
		return appendSigned(b, int64(v)), nil
	case uint16:
		return appendSigned(b, int64(v)), nil
	case uint32:
		return appendSigned(b, int64(v)), nil
	case uint64:
		if v > math.MaxInt64 {
			return keyenc.AppendUint64(append(b, 0x01), v), nil
		}
		return appendSigned(b, int64(v)), nil
	case uint:
		return appendKeyInt(b, uint64(v))
	}
	return nil, fmt.Errorf("HeimDB: expected an integer, got %T", v)
}

func appendSigned(b []byte, v int64) []byte {
	return keyenc.AppendInt64(append(b, 0x00), v)
}
