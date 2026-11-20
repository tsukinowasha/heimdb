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
// Modified by Tsukinowa Inc.: ゼロの日付（'0000-00-00'、0、false）に対する日時の関数の値を MySQL と同じ 0 か NULL にする（上流の #3305。docs/engine.md）
// Modified by Tsukinowa Inc.: 列と CAST の DATETIME(n)、TIMESTAMP(n) の小数の桁を、精度の分だけ表示する（上流の #3476。docs/engine.md）
// Modified by Tsukinowa Inc.: 数を MySQL の number_to_datetime と同じく日時として読む（datetime_number_heimdb.go。段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時の文字列の前後の空白を除き、'2024/01/05 3:4:5' の形を読み、後ろの余計な文字を切り詰めて値と一緒に誤りを返す（上流の #3276、#3280、#3675、#3723。docs/engine.md）

package types

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/shopspring/decimal"
	"gopkg.in/src-d/go-errors.v1"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

const ZeroDateStr = "0000-00-00"

const ZeroTimestampDatetimeStr = "0000-00-00 00:00:00"

var (
	// ErrConvertingToTime is thrown when a value cannot be converted to a Time
	ErrConvertingToTime = errors.NewKind("Incorrect datetime value: '%v'")

	ErrConvertingToTimeOutOfRange = errors.NewKind("value %q is outside of %v range")

	// ErrTruncatedDatetime は、日時の文字列の後ろに余計な文字があり、前の部分だけを読んだ（上流の #3276、#3280 の
	// sql.ErrTruncatedIncorrect）。HeimDB: Convert は読んだ値と一緒にこの誤りを返す。CAST と比べる側は警告 1292 に、
	// 非 STRICT の INSERT は警告 1265 にし、STRICT の INSERT はエラー 1292 にする（sql.ErrTruncatedIncorrect に包む）。
	ErrTruncatedDatetime = sql.ErrTruncatedIncorrect

	// datetimeTypeMaxDatetime is the maximum representable Datetime/Date value.
	datetimeTypeMaxDatetime = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)

	// datetimeTypeMinDatetime is the minimum representable Datetime/Date value.
	datetimeTypeMinDatetime = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)

	// datetimeTypeMaxTimestamp is the maximum representable Timestamp value, which is the maximum 32-bit integer as a Unix time.
	datetimeTypeMaxTimestamp = time.Unix(math.MaxInt32, 999999000)

	// datetimeTypeMinTimestamp is the minimum representable Timestamp value, which is one second past the epoch.
	datetimeTypeMinTimestamp = time.Unix(1, 0)

	DateOnlyLayouts = []string{
		"20060102",
		"2006-1-2",
		"2006-01-02",
		"2006/01/02",
	}

	// TimestampDatetimeLayouts hold extra timestamps allowed for parsing. It does
	// not have all the layouts supported by mysql. Missing are two digit year
	// versions of common cases and dates that use non common separators.
	//
	// https://github.com/MariaDB/server/blob/mysql-5.5.36/sql-common/my_time.c#L124
	TimestampDatetimeLayouts = append([]string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:4",
		"2006-01-02 15:04",
		"2006-01-02 15:04:",
		"2006-01-02 15:04:.",
		"2006-01-02 15:04:05.",
		"2006-01-02 15:04:05.999999",
		"2006-1-2 15:4:5.999999",
		"2006-01-02T15:04:05",
		"20060102150405",
		"2006-01-02 15:04:05.999999999 -0700 MST", // represents standard Time.time.UTC()
	}, DateOnlyLayouts...)

	// zeroTime is 0000-01-01 00:00:00 UTC which is the closest Go can get to 0000-00-00 00:00:00
	zeroTime = time.Unix(-62167219200, 0).UTC()

	// ZeroTime は、ゼロの日時（'0000-00-00 00:00:00'）の内部の値。日時の関数がゼロの日付を見分けるのに使う（上流の
	// #3305）。HeimDB: 上流はこの値を -0001-11-30 に変えるが、保存した値と段階の前からの比較に響くので 0000-01-01 の
	// ままにする。そのため本物の '0000-01-01' もゼロの日付として扱う（表示は前からゼロ）
	ZeroTime = zeroTime

	// Date is a date with day, month and year.
	Date = MustCreateDatetimeType(sqltypes.Date, 0)
	// Datetime is a date and a time with default precision (no fractional seconds).
	Datetime = MustCreateDatetimeType(sqltypes.Datetime, 0)
	// DatetimeMaxPrecision is a date and a time with maximum precision
	DatetimeMaxPrecision = newDatetimeType(sqltypes.Datetime, 6)
	// Timestamp is a UNIX timestamp with default precision (no fractional seconds).
	Timestamp = MustCreateDatetimeType(sqltypes.Timestamp, 0)
	// TimestampMaxPrecision is a UNIX timestamp with maximum precision
	TimestampMaxPrecision = newDatetimeType(sqltypes.Timestamp, 6)

	datetimeValueType = reflect.TypeOf(time.Time{})
)

type datetimeType struct {
	baseType  query.Type
	precision int
	// fsp は、小数の桁を precision の分だけ（0 も）表示するか。列と CAST で精度を書いた型（CreateDatetimeType）は
	// true（MySQL と同じく DATETIME(3) の 56.1 は 56.100。上流の #3476）。
	fsp bool
}

var _ sql.DatetimeType = datetimeType{}
var _ sql.CollationCoercible = datetimeType{}

// CreateDatetimeType creates a Type dealing with all temporal types that are not TIME nor YEAR.
func CreateDatetimeType(baseType query.Type, precision int) (sql.DatetimeType, error) {
	switch baseType {
	case sqltypes.Date, sqltypes.Datetime, sqltypes.Timestamp:
		if precision < 0 || precision > 6 {
			return nil, fmt.Errorf("precision must be between 0 and 6, got %d", precision)
		}
		return datetimeType{
			baseType:  baseType,
			precision: precision,
			fsp:       precision > 0 && baseType != sqltypes.Date,
		}, nil
	}
	return nil, sql.ErrInvalidBaseType.New(baseType.String(), "datetime")
}

// newDatetimeType は、式の結果の型（DatetimeMaxPrecision など）を作る。小数の桁は値のとおりに表示する（fsp が
// false。HeimDB: 日時の関数の多くは DatetimeMaxPrecision を返すので、.000000 を付けない）。
func newDatetimeType(baseType query.Type, precision int) sql.DatetimeType {
	return datetimeType{baseType: baseType, precision: precision}
}

// MustCreateDatetimeType is the same as CreateDatetimeType except it panics on errors.
func MustCreateDatetimeType(baseType query.Type, precision int) sql.DatetimeType {
	dt, err := CreateDatetimeType(baseType, precision)
	if err != nil {
		panic(err)
	}
	return dt
}

func (t datetimeType) Precision() int {
	return t.precision
}

// Compare implements Type interface.
func (t datetimeType) Compare(ctx context.Context, a interface{}, b interface{}) (int, error) {
	if hasNulls, res := CompareNulls(a, b); hasNulls {
		return res, nil
	}

	var at time.Time
	var bt time.Time
	var ok bool
	var err error
	if at, ok = a.(time.Time); !ok {
		at, err = ConvertToTime(ctx, a, t)
		if err != nil && !TruncatedDatetime(err) {
			return 0, err
		}
	} else if t.baseType == sqltypes.Date {
		at = at.Truncate(24 * time.Hour)
	}
	if bt, ok = b.(time.Time); !ok {
		// 後ろを切り詰めて読んだ値は、CAST と同じくその値で比べる（上流の #3276）
		bt, err = ConvertToTime(ctx, b, t)
		if err != nil && !TruncatedDatetime(err) {
			return 0, err
		}

	} else if t.baseType == sqltypes.Date {
		bt = bt.Truncate(24 * time.Hour)
	}

	if at.Before(bt) {
		return -1, nil
	} else if at.After(bt) {
		return 1, nil
	}
	return 0, nil
}

// Convert implements Type interface.
func (t datetimeType) Convert(ctx context.Context, v interface{}) (interface{}, sql.ConvertInRange, error) {
	if v == nil {
		return nil, sql.InRange, nil
	}
	res, err := ConvertToTime(ctx, v, t)
	if err != nil && !TruncatedDatetime(err) {
		return nil, sql.OutOfRange, err
	}
	return res, sql.InRange, err
}

// TruncatedDatetime は、err が日時の文字列の後ろを切り詰めた誤り（値と一緒に返したもの）か（上流の #3276）。
func TruncatedDatetime(err error) bool {
	return err != nil && ErrTruncatedDatetime.Is(err)
}

// precisionConversion is a conversion ratio to divide time.Second by to truncate the appropriate amount for the
// precision of a type with time info
var precisionConversion = [7]int{
	1, 10, 100, 1_000, 10_000, 100_000, 1_000_000,
}

func ConvertToTime(ctx context.Context, v interface{}, t datetimeType) (time.Time, error) {
	if v == nil {
		return time.Time{}, nil
	}

	res, err := t.ConvertWithoutRangeCheck(ctx, v)
	if err != nil && !TruncatedDatetime(err) {
		return time.Time{}, err
	}
	// truncated は、後ろを切り詰めて読んだ（値と一緒に返す。上流の #3276）
	truncated := err

	if res.Equal(zeroTime) {
		return zeroTime, truncated
	}

	// Round the date to the precision of this type
	truncationDuration := time.Second
	truncationDuration /= time.Duration(precisionConversion[t.precision])
	res = res.Round(truncationDuration)

	switch t.baseType {
	case sqltypes.Date:
		if res.Year() < 0 || res.Year() > 9999 {
			return time.Time{}, ErrConvertingToTimeOutOfRange.New(res.Format(sql.DateLayout), t.String())
		}
	case sqltypes.Datetime:
		if res.Year() < 0 || res.Year() > 9999 {
			return time.Time{}, ErrConvertingToTimeOutOfRange.New(res.Format(sql.TimestampDatetimeLayout), t.String())
		}
	case sqltypes.Timestamp:
		if res.Before(datetimeTypeMinTimestamp) || res.After(datetimeTypeMaxTimestamp) {
			return time.Time{}, ErrConvertingToTimeOutOfRange.New(res.Format(sql.TimestampDatetimeLayout), t.String())
		}
	}
	return res, truncated
}

// ConvertWithoutRangeCheck converts the parameter to time.Time without checking the range.
func (t datetimeType) ConvertWithoutRangeCheck(ctx context.Context, v interface{}) (time.Time, error) {
	var res time.Time

	var err error
	v, err = sql.UnwrapAny(ctx, v)
	if err != nil {
		return time.Time{}, err
	}
	if bs, ok := v.([]byte); ok {
		v = string(bs)
	}
	var truncated error
	switch value := v.(type) {
	case string:
		// 前後の空白は除く（MySQL は注意 4096 だけで読む。上流の #3675）
		value = strings.Trim(value, dateSpaces)
		if value == ZeroDateStr || value == ZeroTimestampDatetimeStr {
			return zeroTime, nil
		}
		if cut, ok := zeroDatePrefix(value); ok {
			// HeimDB: the zero date with a zero time of any length or extra
			// characters ('0000-00-00 00:00', '0000-00-00x'), as MySQL reads it
			// (stage 116)
			if cut {
				return zeroTime, ErrTruncatedDatetime.New("datetime", v)
			}
			return zeroTime, nil
		}
		// TODO: consider not using time.Parse if we want to match MySQL exactly ('2010-06-03 11:22.:.:.:.:' is a valid timestamp)
		var parsed bool
		res, parsed = parseDatetime(value)
		if !parsed {
			// '2024/01/05 3:4:5' のような区切りと 1 桁の部分の形と、後ろに余計な文字のある形（上流の #3723、#3276）
			var cut, hasTime bool
			res, cut, hasTime, parsed = parseDelimitedDatetime(value)
			if !parsed {
				return zeroTime, ErrConvertingToTime.New(v)
			}
			if cut {
				// MySQL は、時刻まで読めたときは datetime、日付だけなら date と書く
				kind := "date"
				if hasTime {
					kind = "datetime"
				}
				truncated = ErrTruncatedDatetime.New(kind, v)
			}
		}
	case time.Time:
		res = value.UTC()
		// For most integer values, we just return an error (but MySQL is more lenient for some of these). A special case
		// is zero values, which are important when converting from postgres defaults.
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, decimal.Decimal, decimal.NullDecimal:
		// HeimDB: a number is read as MySQL's number_to_datetime does (20240101,
		// 240101, 20240101103000.5; stage 116). Before, any number but 0 was an
		// error, so CAST(20240101 AS DATETIME) was NULL and d > 20240101 failed.
		t, ok := NumberToDatetime(value)
		if !ok {
			return zeroTime, ErrConvertingToTime.New(v)
		}
		if t.Equal(zeroTime) {
			return zeroTime, nil
		}
		res = t
	case Timespan:
		// when receiving TIME, MySQL fills in date with today
		nowTimeStr := sql.Now().Format("2006-01-02")
		nowTime, err := time.Parse("2006-01-02", nowTimeStr)
		if err != nil {
			return zeroTime, ErrConvertingToTime.New(v)
		}
		return nowTime.Add(value.AsTimeDuration()), nil
	case bool:
		// false は 0 と同じくゼロの日付（上流の #3305）
		if !value {
			return zeroTime, nil
		}
		return zeroTime, ErrConvertingToTime.New(v)
	default:
		return zeroTime, sql.ErrConvertToSQL.New(value, t)
	}

	if t.baseType == sqltypes.Date {
		res = res.Truncate(24 * time.Hour)
	}

	return res, truncated
}

// zeroDatePrefix reports whether s is the zero date '0000-00-00' followed by
// nothing, a zero time ('00:00', '00:00:00.000') or extra characters (cut is
// then true). A non-zero time ('0000-00-00 10:00:00') is not the zero date.
func zeroDatePrefix(s string) (cut, ok bool) {
	if !strings.HasPrefix(s, ZeroDateStr) {
		return false, false
	}
	r := s[len(ZeroDateStr):]
	if r == "" {
		return false, true
	}
	if r[0] != ' ' && r[0] != 'T' {
		// '0000-00-00x'; a digit continues the date ('0000-00-001'), which is not the zero date
		if r[0] >= '0' && r[0] <= '9' {
			return false, false
		}
		return true, true
	}
	t := r[1:]
	i := 0
	for i < len(t) && (t[i] == '0' || t[i] == ':' || t[i] == '.') {
		i++
	}
	if i < len(t) && t[i] >= '1' && t[i] <= '9' {
		return false, false
	}
	return i < len(t), true
}

// fspLayouts は、精度ごとの日時の表示の形（小数の桁を 0 でも出す。上流の #3476）。
var fspLayouts = [7]string{
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05.0",
	"2006-01-02 15:04:05.00",
	"2006-01-02 15:04:05.000",
	"2006-01-02 15:04:05.0000",
	"2006-01-02 15:04:05.00000",
	"2006-01-02 15:04:05.000000",
}

// dateSpaces は、日時の文字列の前後から除く空白（上流の #3675）。
const dateSpaces = " \t\n\r\v\f"

// parseDelimitedDatetime は、MySQL と同じく、4 桁か 2 桁の年と月と日を任意の区切りの記号で分けた日付と、空白か
// 'T' の後の時、分、秒（途中まででよい）と小数を読む（月から秒は続く数字をすべて読んで範囲を確かめる。'2024-011-01'
// は 11 月、'2012-01-01 103' は誤り）（'2024/01/05 3:4:5'、'2024.01.05'、
// '2020-01-01 12'。上流の #3723 の再現した形だけを移した）。日付か時刻の後ろに余計な文字があれば、そこまでを読んで
// cut を true にする（'2020-01-01 a' は 2020-01-01。上流の #3276、#3280）。月、日、時刻が範囲の外なら ok が false。
// HeimDB: 区切りのない数字だけの形は今までの parseDatetime に任せる（'20200101abc' は MySQL でも誤り）。
func parseDelimitedDatetime(s string) (res time.Time, cut, hasTime, ok bool) {
	i := 0
	digits := func(max int) (int, bool) {
		n, start := 0, i
		for i < len(s) && i-start < max && s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
			i++
		}
		return n, i > start
	}
	punct := func() bool {
		if i < len(s) && isDatePunct(s[i]) {
			i++
			return true
		}
		return false
	}
	year, okY := digits(4)
	if i < len(s) && s[i] >= '0' && s[i] <= '9' {
		return time.Time{}, false, false, false // 5 桁以上の年
	}
	switch {
	case !okY || (i != 4 && i != 2) || !punct():
		return time.Time{}, false, false, false
	case i == 3:
		// 2 桁の年は、MySQL と同じく 70 から 99 を 1900 年代、ほかを 2000 年代にする（'13:04:05' は 2013-04-05）
		if year < 70 {
			year += 2000
		} else {
			year += 1900
		}
	}
	month, okM := digits(9)
	if !okM || !punct() {
		return time.Time{}, false, false, false
	}
	day, okD := digits(9)
	if !okD {
		return time.Time{}, false, false, false
	}
	var hour, minute, sec, nsec int
	if i < len(s) {
		// 時刻は 'T' か、空白と記号の並び（MySQL は '2024-01-05+09:00' を 09:00、'2024-01-05 -1' を 01:00 に読む）の後
		j := i
		if s[j] == 'T' {
			j++
		} else {
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || isDatePunct(s[j])) {
				j++
			}
		}
		if j > i && j < len(s) && s[j] >= '0' && s[j] <= '9' {
			i = j
			hour, _ = digits(9)
			hasTime = true
			if save := i; punct() {
				var okMin bool
				if minute, okMin = digits(9); !okMin {
					i = save
				} else if save := i; punct() {
					var okSec bool
					if sec, okSec = digits(9); !okSec {
						i = save
					} else if i < len(s) && s[i] == '.' {
						i++
						start := i
						for i < len(s) && s[i] >= '0' && s[i] <= '9' {
							if i-start < 9 {
								nsec = nsec*10 + int(s[i]-'0')
							}
							i++
						}
						for k := i - start; k < 9; k++ {
							nsec *= 10
						}
					}
				}
			}
		}
	}
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false, false, false
	}
	res = time.Date(year, time.Month(month), day, hour, minute, sec, nsec, time.UTC)
	if res.Day() != day {
		return time.Time{}, false, false, false // 2 月 30 日など
	}
	// 秒の後ろの時差（'+09:00'）は、今までの RFC3339 の形と同じく UTC にする。時差として読めない符号と数字
	// （'12:34:56-123456'）は、MySQL と同じく誤り
	rest := s[i:]
	signed := len(rest) >= 2 && (rest[0] == '+' || rest[0] == '-') && rest[1] >= '0' && rest[1] <= '9'
	if len(rest) == 6 && signed && rest[3] == ':' {
		if oh, err1 := strconv.Atoi(rest[1:3]); err1 == nil {
			if om, err2 := strconv.Atoi(rest[4:6]); err2 == nil && oh <= 14 && om <= 59 {
				off := time.Duration(oh)*time.Hour + time.Duration(om)*time.Minute
				if rest[0] == '-' {
					off = -off
				}
				return res.Add(-off), false, true, true
			}
		}
	}
	if signed {
		return time.Time{}, false, false, false
	}
	return res, i < len(s), hasTime, true
}

// isDatePunct は、日付の区切りに使える記号（MySQL は英数字でも空白でもない ASCII の記号を区切りにする）。
func isDatePunct(c byte) bool {
	return c > ' ' && c < 0x7f && !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z')
}

func parseDatetime(value string) (time.Time, bool) {
	for _, fmt := range TimestampDatetimeLayouts {
		if t, err := time.Parse(fmt, value); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Equals implements the Type interface.
func (t datetimeType) Equals(otherType sql.Type) bool {
	if dtType, isDtType := otherType.(sql.DatetimeType); isDtType {
		return t.baseType == dtType.Type() && t.precision == dtType.Precision()
	}
	return false
}

// MaxTextResponseByteLength implements the Type interface
func (t datetimeType) MaxTextResponseByteLength(*sql.Context) uint32 {
	switch t.baseType {
	case sqltypes.Date:
		return uint32(len(sql.DateLayout))
	case sqltypes.Datetime, sqltypes.Timestamp:
		return uint32(len(sql.TimestampDatetimeLayout))
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "datetime"))
	}
}

// Promote implements the Type interface.
func (t datetimeType) Promote() sql.Type {
	return DatetimeMaxPrecision
}

// SQL implements Type interface.
func (t datetimeType) SQL(ctx *sql.Context, dest []byte, v interface{}) (sqltypes.Value, error) {
	if v == nil {
		return sqltypes.NULL, nil
	}

	vt, err := ConvertToTime(ctx, v, t)
	if err != nil {
		return sqltypes.Value{}, err
	}

	var typ query.Type
	var val []byte

	switch t.baseType {
	case sqltypes.Date:
		typ = sqltypes.Date
		if vt.Equal(zeroTime) {
			val = vt.AppendFormat(dest, ZeroDateStr)
		} else {
			val = vt.AppendFormat(dest, sql.DateLayout)
		}
	case sqltypes.Datetime, sqltypes.Timestamp:
		typ = t.baseType
		layout := sql.TimestampDatetimeLayout
		if t.fsp {
			layout = fspLayouts[t.precision]
		}
		if vt.Equal(zeroTime) {
			val = append(dest, ZeroTimestampDatetimeStr...)
			if t.fsp {
				val = append(val, fspLayouts[t.precision][len(ZeroTimestampDatetimeStr):]...)
			}
		} else {
			val = vt.AppendFormat(dest, layout)
		}
	default:
		return sqltypes.Value{}, sql.ErrInvalidBaseType.New(t.baseType.String(), "datetime")
	}

	valBytes := val

	return sqltypes.MakeTrusted(typ, valBytes), nil
}

func (t datetimeType) String() string {
	switch t.baseType {
	case sqltypes.Date:
		return "date"
	case sqltypes.Datetime:
		if t.precision > 0 {
			return fmt.Sprintf("datetime(%d)", t.precision)
		}
		return "datetime"
	case sqltypes.Timestamp:
		if t.precision > 0 {
			return fmt.Sprintf("timestamp(%d)", t.precision)
		}
		return "timestamp"
	default:
		panic(sql.ErrInvalidBaseType.New(t.baseType.String(), "datetime"))
	}
}

// Type implements Type interface.
func (t datetimeType) Type() query.Type {
	return t.baseType
}

// ValueType implements Type interface.
func (t datetimeType) ValueType() reflect.Type {
	return datetimeValueType
}

func (t datetimeType) Zero() interface{} {
	return zeroTime
}

// CollationCoercibility implements sql.CollationCoercible interface.
func (datetimeType) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// MaximumTime is the latest accepted time for this type.
func (t datetimeType) MaximumTime() time.Time {
	if t.baseType == sqltypes.Timestamp {
		return datetimeTypeMaxTimestamp
	}
	return datetimeTypeMaxDatetime
}

// MinimumTime is the earliest accepted time for this type.
func (t datetimeType) MinimumTime() time.Time {
	if t.baseType == sqltypes.Timestamp {
		return datetimeTypeMinTimestamp
	}
	return datetimeTypeMinDatetime
}

// ValidateTime receives a time and returns either that time or nil if it's
// not a valid time.
func ValidateTime(t time.Time) interface{} {
	if t.After(time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)) {
		return nil
	}
	return t
}
