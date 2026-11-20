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
// Modified by Tsukinowa Inc.: ゼロの日付（'0000-00-00'、0、false）に対する日時の関数の値を MySQL と同じ 0 か NULL にする（上流の #3305。docs/engine.md）
// Modified by Tsukinowa Inc.: 時刻の付いたゼロの日付と YYYYMMDD の数を読み、読めない値は NULL と警告 1292 にする（段階 114。docs/engine.md）

package function

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lestrrat-go/strftime"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func panicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}

func monthNum(t time.Time) string {
	return strconv.FormatInt(int64(t.Month()), 10)
}

func dayWithSuffix(t time.Time) string {
	suffix := "th"
	day := int64(t.Day())
	if day < 4 || day > 20 {
		switch day % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}

	return strconv.FormatInt(day, 10) + suffix
}

func dayOfMonth(t time.Time) string {
	return strconv.FormatInt(int64(t.Day()), 10)
}

func microsecondsStr(t time.Time) string {
	micros := t.Nanosecond() / int(time.Microsecond)
	return fmt.Sprintf("%06d", micros)
}

func minutesStr(t time.Time) string {
	return fmt.Sprintf("%02d", t.Minute())
}

func twelveHour(t time.Time) (int, string) {
	ampm := "AM"
	if t.Hour() >= 12 {
		ampm = "PM"
	}

	hour := t.Hour() % 12
	if hour == 0 {
		hour = 12
	}

	return hour, ampm
}

func twelveHourPadded(t time.Time) string {
	hour, _ := twelveHour(t)
	return fmt.Sprintf("%02d", hour)
}

func twelveHourNoPadding(t time.Time) string {
	hour, _ := twelveHour(t)
	return fmt.Sprintf("%d", hour)
}

func twentyFourHourNoPadding(t time.Time) string {
	return fmt.Sprintf("%d", t.Hour())
}

func fullMonthName(t time.Time) string {
	return t.Month().String()
}

func ampmClockStr(t time.Time) string {
	hour, ampm := twelveHour(t)
	return fmt.Sprintf("%02d:%02d:%02d %s", hour, t.Minute(), t.Second(), ampm)
}

func secondsStr(t time.Time) string {
	return fmt.Sprintf("%02d", t.Second())
}

func yearWeek(mode int32, t time.Time) (int32, int32) {
	yw := YearWeek{expression.NewLiteral(t, types.Datetime), expression.NewLiteral(mode, types.Int32)}
	res, _ := yw.Eval(nil, nil)
	yr := res.(int32) / 100
	wk := res.(int32) % 100

	return yr, wk
}

func weekMode0(t time.Time) string {
	yr, wk := yearWeek(0, t)

	if yr < int32(t.Year()) {
		wk = 0
	} else if yr > int32(t.Year()) {
		wk = 53
	}

	return fmt.Sprintf("%02d", wk)
}

func weekMode1(t time.Time) string {
	yr, wk := yearWeek(1, t)

	if yr < int32(t.Year()) {
		wk = 0
	} else if yr > int32(t.Year()) {
		wk = 53
	}

	return fmt.Sprintf("%02d", wk)
}

func weekMode2(t time.Time) string {
	_, wk := yearWeek(2, t)
	return fmt.Sprintf("%02d", wk)
}

func weekMode3(t time.Time) string {
	_, wk := yearWeek(3, t)
	return fmt.Sprintf("%02d", wk)
}

func yearMode0(t time.Time) string {
	yr, _ := yearWeek(0, t)
	return strconv.FormatInt(int64(yr), 10)
}

func yearMode1(t time.Time) string {
	yr, _ := yearWeek(1, t)
	return strconv.FormatInt(int64(yr), 10)
}

func dayName(t time.Time) string {
	return t.Weekday().String()
}

func yearTwoDigit(t time.Time) string {
	return strconv.FormatInt(int64(t.Year())%100, 10)
}

type AppendFuncWrapper struct {
	fn func(time.Time) string
}

func wrap(fn func(time.Time) string) strftime.Appender {
	return AppendFuncWrapper{fn}
}

func (af AppendFuncWrapper) Append(bytes []byte, t time.Time) []byte {
	s := af.fn(t)
	return append(bytes, []byte(s)...)
}

var mysqlDateFormatSpec = strftime.NewSpecificationSet()
var dateFormatSpecifierToFunc = map[byte]func(time.Time) string{
	'a': nil,
	'b': nil,
	'c': monthNum,
	'D': dayWithSuffix,
	'd': nil,
	'e': dayOfMonth,
	'f': microsecondsStr,
	'H': nil,
	'h': twelveHourPadded,
	'I': twelveHourPadded,
	'i': minutesStr,
	'j': nil,
	'k': twentyFourHourNoPadding,
	'l': twelveHourNoPadding,
	'M': fullMonthName,
	'm': nil,
	'p': nil,
	'r': ampmClockStr,
	'S': nil,
	's': secondsStr,
	'T': nil,
	'U': weekMode0,
	'u': weekMode1,
	'V': weekMode2,
	'v': weekMode3,
	'W': dayName,
	'w': nil,
	'X': yearMode0,
	'x': yearMode1,
	'Y': nil,
	'y': yearTwoDigit,
}

func init() {
	for specifier, fn := range dateFormatSpecifierToFunc {
		if fn != nil {
			panicIfErr(mysqlDateFormatSpec.Set(specifier, wrap(fn)))
		}
	}

	// replace any strftime specifiers that aren't supported
	fn := func(b byte) {
		if _, ok := dateFormatSpecifierToFunc[b]; !ok {
			panicIfErr(mysqlDateFormatSpec.Set(b, wrap(func(time.Time) string {
				return string(b)
			})))
		}
	}

	capToLower := byte('a' - 'A')
	for i := byte('A'); i <= 'Z'; i++ {
		fn(i)
		fn(i + capToLower)
	}
}

func formatDate(format string, t time.Time) (string, error) {
	formatter, err := strftime.New(format, strftime.WithSpecificationSet(mysqlDateFormatSpec))

	if err != nil {
		return "", err
	}

	return formatter.FormatString(t), nil
}

// DateFormat function returns a string representation of the date specified in the format specified
type DateFormat struct {
	expression.BinaryExpressionStub
}

var _ sql.FunctionExpression = (*DateFormat)(nil)
var _ sql.CollationCoercible = (*DateFormat)(nil)

// FunctionName implements sql.FunctionExpression
func (f *DateFormat) FunctionName() string {
	return "date_format"
}

// Description implements sql.FunctionExpression
func (f *DateFormat) Description() string {
	return "format date as specified."
}

// NewDateFormat returns a new DateFormat UDF
func NewDateFormat(ex, value sql.Expression) sql.Expression {
	return &DateFormat{
		expression.BinaryExpressionStub{
			LeftChild:  ex,
			RightChild: value,
		},
	}
}

// Eval implements the Expression interface.
func (f *DateFormat) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	if f.LeftChild == nil || f.RightChild == nil {
		return nil, nil
	}

	left, err := f.LeftChild.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if left == nil {
		return nil, nil
	}

	// YYYYMMDD と YYYYMMDDhhmmss の数は、DATE() と同じく日付として読む（MySQL と同じ。段階 114）
	if nt, ok, isNumber := numberToDatetime(left); isNumber {
		if !ok {
			ctx.Warn(1292, "Incorrect datetime value: '%v'", left)
			return nil, nil
		}
		left = nt
	}
	timeVal, _, err := types.DatetimeMaxPrecision.Convert(ctx, left)
	var t time.Time
	zeroWithTime := false
	switch {
	case err == nil:
		t = timeVal.(time.Time)
	case types.TruncatedDatetime(err):
		// 後ろの余計な文字は切り詰めて読む（上流の #3276）
		ctx.Warn(1292, "%s", err.Error())
		t = timeVal.(time.Time)
	default:
		// 時刻の付いたゼロの日付（'0000-00-00 12:34:56'）は、日付を 0 にして時刻の指定子だけ読む（段階 113 で回した
		// 1105）。ほかの読めない値は、MySQL と同じく NULL と警告 1292
		zt, ok := zeroDateWithTime(left)
		if !ok {
			ctx.Warn(1292, "Incorrect datetime value: '%v'", left)
			return nil, nil
		}
		t, zeroWithTime = zt, true
	}

	right, err := f.RightChild.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if right == nil {
		return nil, nil
	}

	formatStr, ok := right.(string)

	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("DATE_FORMAT", "format must be a string")
	}

	if zeroWithTime || isZeroDate(t) {
		return formatZeroDate(formatStr, t), nil
	}

	return formatDate(formatStr, t)
}

// zeroDateWithTime は、v が時刻の付いたゼロの日付（'0000-00-00 12:34:56'）なら、ゼロの日付にその時刻を足した値を返す。
func zeroDateWithTime(v interface{}) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, types.ZeroDateStr) || len(s) < len(types.ZeroDateStr)+2 || (s[10] != ' ' && s[10] != 'T') {
		return time.Time{}, false
	}
	t, _, err := types.DatetimeMaxPrecision.Convert(context.Background(), "2000-01-01"+s[10:])
	if err != nil && !types.TruncatedDatetime(err) {
		return time.Time{}, false
	}
	tt := t.(time.Time)
	return types.ZeroTime.Add(tt.Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))), true
}

// zeroDateSpecifiers は、ゼロの日付（'0000-00-00'）での日付の部分の指定子の値。時刻の指定子は時刻から作る。
var zeroDateSpecifiers = map[byte]string{
	'Y': "0000",
	'y': "00",
	'm': "00",
	'c': "0",
	'd': "00",
	'e': "0",
	'D': "0th",
	// MySQL 8.0 がゼロの日付で返す値（週と年の中の日の計算が 0 年 0 月 0 日をそのまま使う値で、意味はないが合わせる）
	'j': "000",
	'U': "613566757",
	'u': "613566757",
	'V': "613566757",
	'v': "01",
	'X': "0000",
	'x': "0001",
}

// formatZeroDate は、ゼロの日付の DATE_FORMAT を MySQL と同じく年、月、日を 0 にして作る（'%Y-%m-%d' は
// '0000-00-00'）。月の名前と曜日（%a %b %M %W %w）があれば、MySQL と同じく NULL を返す。指定子の値は MySQL 8.0 で
// 確かめた（上流の #3305 の問題。上流は扱っていない）。
func formatZeroDate(format string, t time.Time) interface{} {
	var b []byte
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' || i+1 >= len(format) {
			b = append(b, c)
			continue
		}
		i++
		spec := format[i]
		if v, ok := zeroDateSpecifiers[spec]; ok {
			b = append(b, v...)
			continue
		}
		switch spec {
		case 'a', 'b', 'M', 'W', 'w':
			return nil
		}
		part, err := formatDate("%"+string(spec), t)
		if err != nil {
			return nil
		}
		b = append(b, part...)
	}
	return string(b)
}

// Type implements the Expression interface.
func (f *DateFormat) Type() sql.Type {
	return types.Text
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*DateFormat) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return ctx.GetCollation(), 4
}

// IsNullable implements the Expression interface.
func (f *DateFormat) IsNullable() bool {
	if types.IsNull(f.LeftChild) {
		if types.IsNull(f.RightChild) {
			return true
		}
		return f.RightChild.IsNullable()
	}
	return f.LeftChild.IsNullable()
}

func (f *DateFormat) String() string {
	return fmt.Sprintf("date_format(%s, %s)", f.LeftChild, f.RightChild)
}

// WithChildren implements the Expression interface.
func (f *DateFormat) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(f, len(children), 2)
	}
	return NewDateFormat(children[0], children[1]), nil
}
