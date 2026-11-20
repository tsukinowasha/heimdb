package rowenc

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// IndexColumn は、セカンダリインデックスのキーに入れる 1 つの列。
type IndexColumn struct {
	Type sql.Type
	// Prefix はプレフィックス長（文字列は文字数、バイナリはバイト数）。0 なら全体。
	Prefix int64
}

// KeyLevel は、今のこのバージョンがインデックスの定義に付けるキーの水準（docs/plan.md の「キー設計」と段階 117）。
// 水準は、エントリに入れられる型の範囲を示す。
//   - 0: 整数、日時、文字列、バイト列（段階 117 より前に作った定義。印なし）。
//   - 1: 0 に加えて DECIMAL、FLOAT、DOUBLE、ENUM。
//   - 2: 1 に加えて SET と BIT（段階 120）。
//
// 水準を上げると、前からあるインデックスは古い水準のまま（エントリを持たない型を含めば全件を読む）で、新しく作る
// インデックスと heimdb_rebuild_indexes() で作り直したものだけが新しい水準になる。
const KeyLevel = 2

// KeyableType は、型 t の値を、キーの水準 level のセカンダリインデックスのキーに入れられるかを返す。
// 入れられない型を含むインデックスは、キーを持たずに模擬する。
func KeyableType(t sql.Type, level int) bool {
	switch {
	case types.IsInteger(t), types.IsTime(t):
		return true
	case level >= 1 && (types.IsDecimal(t) || isFloat(t) || types.IsEnum(t)):
		return true
	case level >= 2 && (types.IsSet(t) || types.IsBit(t)):
		return true
	case isBinaryString(t):
		return true
	case isText(t):
		c := t.(types.StringType).Collation()
		cs := c.CharacterSet()
		return c.Sorter() != nil && (cs == sql.CharacterSet_utf8mb4 || cs == sql.CharacterSet_utf8mb3)
	}
	return false
}

// isFloat は FLOAT と DOUBLE を表す。
func isFloat(t sql.Type) bool {
	switch t.Type() {
	case sqltypes.Float32, sqltypes.Float64:
		return true
	}
	return false
}

// isBinaryString は BINARY、VARBINARY、BLOB を表す。JSON などは文字列型に見えても含めない。
func isBinaryString(t sql.Type) bool {
	_, ok := t.(types.StringType)
	return ok && types.IsBinaryType(t)
}

// isText は CHAR、VARCHAR、TEXT を表す。
func isText(t sql.Type) bool {
	_, ok := t.(types.StringType)
	return ok && !types.IsBinaryType(t) && types.IsTextOnly(t)
}

// AppendIndexValue は、列 col の値 v をセカンダリインデックスのキー用に符号化して追記する。
// 先頭に NULL の印を付ける。truncated は、プレフィックス長で値を切ったかどうか（切ると、キーが等しくても
// 値が等しいとは限らない）。
func AppendIndexValue(b []byte, col IndexColumn, v any) (out []byte, truncated bool, err error) {
	if v == nil {
		return keyenc.AppendNull(b), false, nil
	}
	b = keyenc.AppendNotNull(b)
	t := col.Type
	switch {
	case types.IsInteger(t), types.IsEnum(t), types.IsSet(t), types.IsBit(t):
		// ENUM はエンジンが行に入れる番号（uint16）を整数と同じ形で入れる（段階 117。主キーの ENUM と同じ）。SET は番号の組の
		// ビット、BIT は値（どちらも uint64）を、符号なしの数の順で入れる（段階 120。MySQL も数の順に並べる）
		out, err = appendKeyInt(b, v)
		return out, false, err
	case types.IsDecimal(t):
		out, err = appendKeyDecimal(b, t, v)
		return out, false, err
	case isFloat(t):
		out, err = appendKeyFloat(b, t, v)
		return out, false, err
	case types.IsTime(t):
		tm, ok := v.(time.Time)
		if !ok {
			return nil, false, fmt.Errorf("HeimDB: expected time.Time for %s, got %T", t, v)
		}
		return appendKeyTime(b, tm), false, nil
	case isBinaryString(t):
		raw, err := bytesValue(t, v)
		if err != nil {
			return nil, false, err
		}
		if col.Prefix > 0 && int64(len(raw)) > col.Prefix {
			raw, truncated = raw[:col.Prefix], true
		}
		return keyenc.AppendBytes(b, raw), truncated, nil
	case isText(t):
		return appendTextKey(b, t, v, col.Prefix)
	}
	return nil, false, fmt.Errorf("HeimDB: type %s cannot be used in an index key", t)
}

// appendKeyDecimal は、DECIMAL の値を、列の桁（スケール）によらない形で追記する（docs/plan.md の「順序を保つ符号化」。
// 段階 117）。符号の 1 バイト（負 0x01、0 は 0x02、正 0x03）の後に、正なら整数部の桁数の 1 バイト、整数部の各桁、末尾の
// 0 を除いた小数部の各桁（数字に 1 を足した 0x01 から 0x0A）、終端の 0x00 を置き、負なら絶対値の正の形を全バイト
// 反転して置く。1.5 と 1.50 は同じキー。
func appendKeyDecimal(b []byte, t sql.Type, v any) ([]byte, error) {
	d, ok := v.(decimal.Decimal)
	if !ok {
		return nil, fmt.Errorf("HeimDB: expected a decimal for %s, got %T", t, v)
	}
	switch d.Sign() {
	case 0:
		return append(b, 0x02), nil
	case 1:
		return appendDecimalDigits(append(b, 0x03), d), nil
	}
	b = append(b, 0x01)
	start := len(b)
	b = appendDecimalDigits(b, d.Neg())
	for i := start; i < len(b); i++ {
		b[i] = ^b[i]
	}
	return b, nil
}

// appendDecimalDigits は、正の DECIMAL d の、整数部の桁数、各桁、終端を追記する。
func appendDecimalDigits(b []byte, d decimal.Decimal) []byte {
	// 係数と指数で表し、10 進の数字の列と小数点の位置にする（d = coef × 10^exp）
	coef := d.Coefficient().String()
	exp := int(d.Exponent())
	var intPart, fracPart string
	if exp >= 0 {
		intPart = coef + strings.Repeat("0", exp)
	} else if n := len(coef) + exp; n > 0 {
		intPart, fracPart = coef[:n], coef[n:]
	} else {
		fracPart = strings.Repeat("0", -n) + coef
	}
	intPart = strings.TrimLeft(intPart, "0")
	fracPart = strings.TrimRight(fracPart, "0")
	// DECIMAL は 65 桁までなので、桁数は 1 バイトに入る（範囲の端の値は列の型に変換してから来る）
	b = append(b, byte(min(len(intPart), 255)))
	for _, c := range intPart + fracPart {
		b = append(b, byte(c-'0')+1)
	}
	return append(b, 0x00)
}

// skipKeyDecimal は、appendKeyDecimal が書いた値を読み飛ばす。
func skipKeyDecimal(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, ErrCorrupt
	}
	var end byte
	switch b[0] {
	case 0x02:
		return b[1:], nil
	case 0x03:
		end = 0x00
	case 0x01:
		end = 0xFF
	default:
		return nil, ErrCorrupt
	}
	// 符号と桁数の 2 バイトの後、終端まで（桁は終端の値にならない）
	if len(b) < 2 {
		return nil, ErrCorrupt
	}
	for i := 2; i < len(b); i++ {
		if b[i] == end {
			return b[i+1:], nil
		}
	}
	return nil, ErrCorrupt
}

// appendKeyFloat は、FLOAT か DOUBLE の値を、倍精度にした IEEE 754 のビット列で順序を保つ 8 バイトにして追記する
// （段階 117）。正（+0 を含む）は符号ビットを立て、負は全ビットを反転する。-0 は +0 にする（MySQL も一意インデックスで
// 0 と -0 を重複とする）。NaN と無限大は列に入らないので、来たらエラー。
func appendKeyFloat(b []byte, t sql.Type, v any) ([]byte, error) {
	var f float64
	switch v := v.(type) {
	case float64:
		f = v
		if t.Type() == sqltypes.Float32 {
			f = float64(float32(v)) // FLOAT の列の値は単精度（エンジンは float32 を入れる。ほかの経路の倍精度もそろえる）
		}
	case float32:
		f = float64(v)
	default:
		return nil, fmt.Errorf("HeimDB: expected a float for %s, got %T", t, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("HeimDB: %v cannot be stored in an index key of %s", f, t)
	}
	if f == 0 {
		f = 0 // -0 を +0 に
	}
	bits := math.Float64bits(f)
	if bits&(1<<63) != 0 {
		bits = ^bits
	} else {
		bits |= 1 << 63
	}
	return binary.BigEndian.AppendUint64(b, bits), nil
}

// appendKeyTime は日時を、UNIX 秒（appendInt64）とナノ秒の 4 バイトで追記する。
func appendKeyTime(b []byte, tm time.Time) []byte {
	b = keyenc.AppendInt64(b, tm.Unix())
	return binary.BigEndian.AppendUint32(b, uint32(tm.Nanosecond()))
}

// bytesValue は、バイナリの文字列の列の値を返す（エンジンは []byte も string も入れる）。
func bytesValue(t sql.Type, v any) ([]byte, error) {
	switch v := v.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	}
	return nil, fmt.Errorf("HeimDB: expected bytes for %s, got %T", t, v)
}

// appendTextKey は、文字列 v を、型 t の照合順序の重みを文字ごとに並べ（0x01 と 4 バイト）、0x00 で終えて追記する。
// 継続の 0x01 は終端の 0x00 より後に並ぶので、短い方が先に並び、ある値のキーが別の値のキーの前方になることもない。
// prefix が正なら、その文字数で切る（truncated）。インデックスと文字列の主キー（docs/plan.md の段階 38）で使う。
func appendTextKey(b []byte, t sql.Type, v any, prefix int64) (out []byte, truncated bool, err error) {
	s, ok := v.(string)
	if !ok {
		return nil, false, fmt.Errorf("HeimDB: expected string for %s, got %T", t, v)
	}
	weight := t.(types.StringType).Collation().Sorter()
	n := int64(0)
	for len(s) > 0 {
		if prefix > 0 && n == prefix {
			truncated = true
			break
		}
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size <= 1 {
			return nil, false, fmt.Errorf("HeimDB: malformed string in a key column")
		}
		b = binary.BigEndian.AppendUint32(append(b, 0x01), uint32(weight(r))^(1<<31))
		s = s[size:]
		n++
	}
	return append(b, 0x00), truncated, nil
}

// skipTextKey は、appendTextKey が書いた値を読み飛ばす。
func skipTextKey(b []byte) ([]byte, error) {
	for len(b) > 0 {
		switch b[0] {
		case 0x00:
			return b[1:], nil
		case 0x01:
			if len(b) < 5 {
				return nil, ErrCorrupt
			}
			b = b[5:]
		default:
			return nil, ErrCorrupt
		}
	}
	return nil, ErrCorrupt
}

// SkipIndexValue は、AppendIndexValue が列 col について書いた値を読み飛ばし、残りを返す（エントリのキーから、
// 後ろに続く主キーを取り出すため。docs/plan.md の段階 38）。
func SkipIndexValue(col IndexColumn, b []byte) ([]byte, error) {
	isNull, b, err := keyenc.DecodeNullTag(b)
	if err != nil || isNull {
		return b, err
	}
	t := col.Type
	switch {
	case types.IsInteger(t), types.IsEnum(t), types.IsSet(t), types.IsBit(t):
		if len(b) < 9 {
			return nil, ErrCorrupt
		}
		return b[9:], nil
	case types.IsDecimal(t):
		return skipKeyDecimal(b)
	case isFloat(t):
		if len(b) < 8 {
			return nil, ErrCorrupt
		}
		return b[8:], nil
	case types.IsTime(t):
		if len(b) < 12 {
			return nil, ErrCorrupt
		}
		return b[12:], nil
	case isBinaryString(t):
		_, rest, err := keyenc.DecodeBytes(b)
		return rest, err
	case isText(t):
		return skipTextKey(b)
	}
	return nil, fmt.Errorf("HeimDB: type %s cannot be used in an index key", t)
}
