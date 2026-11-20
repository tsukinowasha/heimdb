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
// Modified by Tsukinowa Inc.: ゼロの日付（'0000-00-00'、0、false）に対する日時の関数の値を MySQL と同じ 0 か NULL にする（上流の #3305。docs/engine.md）
// Modified by Tsukinowa Inc.: 日付の引数の YYYYMMDD の数と後ろを切り詰めた文字列を読み、HOUR、MINUTE、SECOND は文字列を TIME として読む（上流の #3276、#3723。docs/engine.md）
// Modified by Tsukinowa Inc.: DATE のゼロの日付を 0000-00-00 にし、TIME の時刻だけの文字列を時刻として読む（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: MICROSECOND と TIME_TO_SEC の TIME と時刻だけの文字列を時刻として読む（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: NOW(n)、UTC_TIMESTAMP(n) を小数が n 桁の DATETIME にし、UTC_TIMESTAMP と CURTIME(n) の値を n 桁にする（段階 116 の 4 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: WEEK(NULL) と YEARWEEK(NULL) を NULL にし、ゼロの日付の警告を MySQL と同じ形にする（zeroDateWarning。段階 118。docs/engine.md）
// Modified by Tsukinowa Inc.: YEARWEEK の文字列で mode の式を %s で書く（%d は型の構造体を書いた。段階 120 のレビュー。docs/engine.md）

package function

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/src-d/go-errors.v1"

	gmstime "github.com/tsukinowasha/heimdb/internal/sqlengine/internal/time"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// ErrTimeUnexpectedlyNil is thrown when a function encounters and unexpectedly nil time
var ErrTimeUnexpectedlyNil = errors.NewKind("time in function '%s' unexpectedly nil")

// ErrUnknownType is thrown when a function encounters and unknown type
var ErrUnknownType = errors.NewKind("function '%s' encountered unknown type %T")

var ErrTooHighPrecision = errors.NewKind("Too-big precision %d for '%s'. Maximum is %d.")

func getDate(ctx *sql.Context,
	u expression.UnaryExpression,
	row sql.Row) (interface{}, error) {

	val, err := u.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	return getDateOf(ctx, val)
}

// getDateOf は getDate と同じで、評価した引数の値 val を受け取る。
func getDateOf(ctx *sql.Context, val interface{}) (interface{}, error) {
	if val == nil {
		return nil, nil
	}

	// YYYYMMDD と YYYYMMDDhhmmss の数（DATE(20010203)。上流の #3723）
	if t, ok, isNumber := numberToDatetime(val); isNumber {
		if !ok {
			ctx.Warn(1292, "Incorrect datetime value: '%v'", val)
			return nil, nil
		}
		return t, nil
	}

	date, err := types.DatetimeMaxPrecision.ConvertWithoutRangeCheck(ctx, val)
	if types.TruncatedDatetime(err) {
		// 後ろの余計な文字を切り詰めて読んだ（上流の #3276）
		ctx.Warn(1292, "%s", err.Error())
		return date, nil
	}
	if err != nil {
		ctx.Warn(1292, "Incorrect datetime value: '%s'", val)
		return nil, nil
		//date = types.DatetimeMaxPrecision.Zero().(time.Time)
	}

	return date, nil
}

// isTimeOnly は、t が TIME として読む文字列（'13:04:05'、'-838:59:59'、'1:2'、'130405'、'13:04:05.5'）か。正規表現
// ^-?\d+(:\d{1,2}){0,2}(\.\d*)?$ と同じで、行ごとに正規表現を走らせない（段階 114）。
func isTimeOnly(t string) bool {
	i := 0
	if i < len(t) && t[i] == '-' {
		i++
	}
	digits := func(max int) int {
		n := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' && (max == 0 || n < max) {
			i++
			n++
		}
		return n
	}
	if digits(0) == 0 {
		return false
	}
	for k := 0; k < 2 && i < len(t) && t[i] == ':'; k++ {
		i++
		if digits(2) == 0 {
			return false
		}
	}
	if i < len(t) && t[i] == '.' {
		i++
		digits(0)
	}
	return i == len(t)
}

// isDatetimeWithTime は、t が日付と空白の後に時刻の続く文字列（MySQL の HOUR などはこれを日時として読む）か。正規表現
// ^\d{2,4}[[:punct:]]\d{1,2}[[:punct:]]\d{1,2} +\d と同じ（段階 114）。
func isDatetimeWithTime(t string) bool {
	i := 0
	digits := func(min, max int) bool {
		n := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' && n < max {
			i++
			n++
		}
		return n >= min
	}
	punct := func() bool {
		if i < len(t) && t[i] > ' ' && t[i] < 0x7f && !(t[i] >= '0' && t[i] <= '9') && !(t[i] >= 'a' && t[i] <= 'z') && !(t[i] >= 'A' && t[i] <= 'Z') {
			i++
			return true
		}
		return false
	}
	if !digits(2, 4) || !punct() || !digits(1, 2) || !punct() || !digits(1, 2) {
		return false
	}
	spaces := 0
	for i < len(t) && t[i] == ' ' {
		i++
		spaces++
	}
	return spaces > 0 && i < len(t) && t[i] >= '0' && t[i] <= '9'
}

// timePartOf は、HOUR、MINUTE、SECOND の引数を、MySQL と同じく TIME として読み、part で時、分、秒を取る（上流の
// #3723 の再現した形）。'13:04:05' と 130405 は TIME、'2024-01-02 13:04:05' は日時（handled が false で、今までの
// 日時の読み方に任せる）、'2024-01-02' と '2007-12-11T20:21:22Z' は先頭の数字だけを TIME として読み（00:20:24、
// 00:20:07）、警告 1292 を出す。負の TIME の時は絶対値（HOUR('-13:04:05') は 13）。
func timePartOf(ctx *sql.Context, val interface{}, part func(h, m, s int) int) (interface{}, bool) {
	var ts types.Timespan
	var err error
	switch v := val.(type) {
	case nil:
		return nil, true
	case []byte:
		val = string(v)
	}
	switch v := val.(type) {
	case string:
		t := strings.TrimLeft(v, " \t\n\r\v\f")
		switch {
		case isDatetimeWithTime(t):
			return nil, false
		case isTimeOnly(t):
			if !strings.Contains(t, ":") && len(strings.TrimLeft(t, "-")) > 7 {
				return nil, false // YYYYMMDDhhmmss などは日時
			}
			if ts, err = types.Time.ConvertToTimespan(t); err != nil {
				ctx.Warn(1292, "Truncated incorrect time value: '%s'", v)
				return nil, true
			}
		default:
			n := 0
			for n < len(t) && t[n] >= '0' && t[n] <= '9' {
				n++
			}
			if n == 0 || n > 7 {
				if n == 0 {
					ctx.Warn(1292, "Truncated incorrect time value: '%s'", v)
					return nil, true
				}
				return nil, false
			}
			if ts, err = types.Time.ConvertToTimespan(t[:n]); err != nil {
				ctx.Warn(1292, "Truncated incorrect time value: '%s'", v)
				return nil, true
			}
			ctx.Warn(1292, "Truncated incorrect time value: '%s'", v)
		}
	case int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint, float32, float64, decimal.Decimal:
		if ts, err = types.Time.ConvertToTimespan(v); err != nil {
			return nil, false // YYYYMMDDhhmmss などは日時
		}
	default:
		return nil, false
	}
	us := ts.AsMicroseconds()
	if us < 0 {
		us = -us
	}
	sec := us / 1000000
	return int32(part(int(sec/3600), int(sec/60%60), int(sec%60))), true
}

// numberToDatetime は、日付を受け取る関数の数の引数を、MySQL の number_to_datetime と同じく YYMMDD、YYYYMMDD、
// YYMMDDhhmmss、YYYYMMDDhhmmss として読む（上流の #3723）。isNumber は v が数か（真偽値と文字列は false）。0 は
// ゼロの日付。小数の部分は秒の端数にする。
func numberToDatetime(v interface{}) (t time.Time, ok bool, isNumber bool) {
	var nr int64
	var frac decimal.Decimal
	switch v := v.(type) {
	case int8:
		nr = int64(v)
	case int16:
		nr = int64(v)
	case int32:
		nr = int64(v)
	case int64:
		nr = v
	case int:
		nr = int64(v)
	case uint8:
		nr = int64(v)
	case uint16:
		nr = int64(v)
	case uint32:
		nr = int64(v)
	case uint64:
		if v > math.MaxInt64 {
			return time.Time{}, false, true
		}
		nr = int64(v)
	case uint:
		nr = int64(v)
	case float32, float64, decimal.Decimal:
		var d decimal.Decimal
		switch v := v.(type) {
		case float32:
			d = decimal.NewFromFloat32(v)
		case float64:
			d = decimal.NewFromFloat(v)
		case decimal.Decimal:
			d = v
		}
		if d.IsNegative() || d.GreaterThan(decimal.NewFromInt(99991231235959)) {
			return time.Time{}, false, true
		}
		nr = d.IntPart()
		frac = d.Sub(decimal.NewFromInt(nr))
	default:
		return time.Time{}, false, false
	}
	if nr == 0 && frac.IsZero() {
		return types.ZeroTime, true, true
	}
	const yyPartYear = 70
	switch {
	case nr < 0:
		return time.Time{}, false, true
	case nr >= 10000101000000:
	case nr < 101:
		return time.Time{}, false, true
	case nr <= (yyPartYear-1)*10000+1231:
		nr = (nr + 20000000) * 1000000
	case nr < yyPartYear*10000+101:
		return time.Time{}, false, true
	case nr <= 991231:
		nr = (nr + 19000000) * 1000000
	case nr < 10000101:
		return time.Time{}, false, true
	case nr <= 99991231:
		nr = nr * 1000000
	case nr < 101000000:
		return time.Time{}, false, true
	case nr <= (yyPartYear-1)*10000000000+1231235959:
		nr = nr + 20000000000000
	case nr < yyPartYear*10000000000+101000000:
		return time.Time{}, false, true
	case nr <= 991231235959:
		nr = nr + 19000000000000
	}
	if nr > 99991231235959 {
		return time.Time{}, false, true
	}
	part1, part2 := nr/1000000, nr%1000000
	year, month, day := int(part1/10000), int(part1/100%100), int(part1%100)
	hour, minute, sec := int(part2/10000), int(part2/100%100), int(part2%100)
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false, true
	}
	t = time.Date(year, time.Month(month), day, hour, minute, sec, 0, time.UTC)
	if t.Day() != day {
		return time.Time{}, false, true // 2 月 30 日など
	}
	usec := frac.Mul(decimal.NewFromInt(1000000)).Round(0).IntPart()
	return t.Add(time.Duration(usec) * time.Microsecond), true, true
}

func getDatePart(ctx *sql.Context,
	u expression.UnaryExpression,
	row sql.Row,
	f func(interface{}) interface{}) (interface{}, error) {

	val, err := u.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	return getDatePartOf(ctx, u.Child, val, f)
}

// getDatePartOf は getDatePart と同じで、評価した引数の値 val（式 e の値。nil でもよい）を受け取る。
func getDatePartOf(ctx *sql.Context, e sql.Expression, val interface{}, f func(interface{}) interface{}) (interface{}, error) {
	date, err := getDateOf(ctx, val)
	if err != nil {
		return nil, err
	}
	if date == nil {
		return nil, nil
	}

	part := f(date)
	if part == nil {
		// ゼロの日付の曜日などは NULL（MySQL と同じく警告を出す。上流の #3305。列には出さず、文は引数の形。段階 118）
		zeroDateWarning(ctx, e, val)
	}
	return part, nil
}

// zeroDateWarning は、ゼロの日付になった引数（式 e の値 val）に、MySQL と同じ警告 1292 を出す（HeimDB: 段階 118）。
// MySQL は日付と日時の列のゼロの値には出さず、文字列と数にはその値を、ほかの日付の値には '0000-00-00' を書く（前は
// Go の時刻の形 '0000-01-01 00:00:00 +0000 UTC' で、列にも出した）。
func zeroDateWarning(ctx *sql.Context, e sql.Expression, val interface{}) {
	switch v := val.(type) {
	case time.Time:
		if _, ok := e.(*expression.GetField); ok {
			return
		}
		ctx.Warn(1292, "Incorrect datetime value: '%s'", types.ZeroDateStr)
	case []byte:
		ctx.Warn(1292, "Incorrect datetime value: '%s'", string(v))
	default:
		ctx.Warn(1292, "Incorrect datetime value: '%v'", v)
	}
}

// zeroDateOf は、引数の値 arg を変換した t がゼロの日付か。文字列は '0000-00-00' で始まるときだけで、本物の
// '0000-01-01' は前と同じく日付として読む（内部の値はゼロの日付と同じ。HeimDB: 段階 118）。
func zeroDateOf(arg interface{}, t time.Time) bool {
	if !isZeroDate(t) {
		return false
	}
	switch v := arg.(type) {
	case string:
		return strings.HasPrefix(strings.TrimLeft(v, " "), types.ZeroDateStr)
	case []byte:
		return strings.HasPrefix(strings.TrimLeft(string(v), " "), types.ZeroDateStr)
	}
	return true
}

// isZeroDate は、t がゼロの日付（'0000-00-00'、0、false を変換した値）か（上流の #3305）。
func isZeroDate(t time.Time) bool {
	return t.Equal(types.ZeroTime)
}

// zeroDatePartFunc は datePartFunc と同じで、ゼロの日付には zero（0 か NULL）を返す。
func zeroDatePartFunc(zero interface{}, fn func(time.Time) int) func(interface{}) interface{} {
	return func(v interface{}) interface{} {
		if v == nil {
			return nil
		}
		t := v.(time.Time)
		if isZeroDate(t) {
			return zero
		}
		return int32(fn(t))
	}
}

// Year is a function that returns the year of a date.
type Year struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Year)(nil)
var _ sql.CollationCoercible = (*Year)(nil)

// NewYear creates a new Year UDF.
func NewYear(date sql.Expression) sql.Expression {
	return &Year{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (y *Year) FunctionName() string {
	return "year"
}

// Description implements sql.FunctionExpression
func (y *Year) Description() string {
	return "returns the year of the given date."
}

func (y *Year) String() string { return fmt.Sprintf("%s(%s)", y.FunctionName(), y.Child) }

// Type implements the Expression interface.
func (y *Year) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Year) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (y *Year) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, y.UnaryExpression, row, year)
}

// WithChildren implements the Expression interface.
func (y *Year) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(y, len(children), 1)
	}
	return NewYear(children[0]), nil
}

type Quarter struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Quarter)(nil)
var _ sql.CollationCoercible = (*Quarter)(nil)

// NewQuarter creates a new Month UDF.
func NewQuarter(date sql.Expression) sql.Expression {
	return &Quarter{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (q *Quarter) FunctionName() string {
	return "quarter"
}

// Description implements sql.FunctionExpression
func (q *Quarter) Description() string {
	return "returns the quarter of the given date."
}

func (q *Quarter) String() string { return fmt.Sprintf("%s(%s)", q.FunctionName(), q.Child) }

// Type implements the Expression interface.
func (q *Quarter) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (q *Quarter) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (q *Quarter) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	mon, err := getDatePart(ctx, q.UnaryExpression, row, month)
	if err != nil {
		return nil, err
	}

	if mon == nil {
		return nil, nil
	}
	if mon.(int32) == 0 {
		// ゼロの日付の四半期は 0（上流の #3305）
		return int32(0), nil
	}

	return (mon.(int32)-1)/3 + 1, nil
}

// WithChildren implements the Expression interface.
func (q *Quarter) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(q, len(children), 1)
	}
	return NewQuarter(children[0]), nil
}

// Month is a function that returns the month of a date.
type Month struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Month)(nil)
var _ sql.CollationCoercible = (*Month)(nil)

// NewMonth creates a new Month UDF.
func NewMonth(date sql.Expression) sql.Expression {
	return &Month{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (m *Month) FunctionName() string {
	return "month"
}

// Description implements sql.FunctionExpression
func (m *Month) Description() string {
	return "returns the month of the given date."
}

func (m *Month) String() string { return fmt.Sprintf("%s(%s)", m.FunctionName(), m.Child) }

// Type implements the Expression interface.
func (m *Month) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Month) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (m *Month) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, m.UnaryExpression, row, month)
}

// WithChildren implements the Expression interface.
func (m *Month) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(m, len(children), 1)
	}
	return NewMonth(children[0]), nil
}

// Day is a function that returns the day of a date.
type Day struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Day)(nil)
var _ sql.CollationCoercible = (*Day)(nil)

// NewDay creates a new Day UDF.
func NewDay(date sql.Expression) sql.Expression {
	return &Day{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (d *Day) FunctionName() string {
	return "day"
}

// Description implements sql.FunctionExpression
func (d *Day) Description() string {
	return "returns the day of the month (0-31)."
}

func (d *Day) String() string { return fmt.Sprintf("%s(%s)", d.FunctionName(), d.Child) }

// Type implements the Expression interface.
func (d *Day) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Day) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *Day) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, d.UnaryExpression, row, day)
}

// WithChildren implements the Expression interface.
func (d *Day) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewDay(children[0]), nil
}

// Weekday is a function that returns the weekday of a date where 0 = Monday,
// ..., 6 = Sunday.
type Weekday struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Weekday)(nil)
var _ sql.CollationCoercible = (*Weekday)(nil)

// NewWeekday creates a new Weekday UDF.
func NewWeekday(date sql.Expression) sql.Expression {
	return &Weekday{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (d *Weekday) FunctionName() string {
	return "weekday"
}

// Description implements sql.FunctionExpression
func (d *Weekday) Description() string {
	return "returns the weekday of the given date."
}

func (d *Weekday) String() string { return fmt.Sprintf("%s(%s)", d.FunctionName(), d.Child) }

// Type implements the Expression interface.
func (d *Weekday) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Weekday) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *Weekday) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, d.UnaryExpression, row, weekday)
}

// WithChildren implements the Expression interface.
func (d *Weekday) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewWeekday(children[0]), nil
}

// Hour is a function that returns the hour of a date.
type Hour struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Hour)(nil)
var _ sql.CollationCoercible = (*Hour)(nil)

// NewHour creates a new Hour UDF.
func NewHour(date sql.Expression) sql.Expression {
	return &Hour{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (h *Hour) FunctionName() string {
	return "hour"
}

// Description implements sql.FunctionExpression
func (h *Hour) Description() string {
	return "returns the hours of the given date."
}

func (h *Hour) String() string { return fmt.Sprintf("%s(%s)", h.FunctionName(), h.Child) }

// Type implements the Expression interface.
func (h *Hour) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Hour) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (h *Hour) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	// 文字列と数は TIME として読む（HOUR('13:04:05') は 13。上流の #3723）
	val, err := h.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if v, handled := timePartOf(ctx, val, func(h, m, s int) int { return h }); handled {
		return v, nil
	}
	return getDatePartOf(ctx, nil, val, hour)
}

// WithChildren implements the Expression interface.
func (h *Hour) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(h, len(children), 1)
	}
	return NewHour(children[0]), nil
}

// Minute is a function that returns the minute of a date.
type Minute struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Minute)(nil)
var _ sql.CollationCoercible = (*Minute)(nil)

// NewMinute creates a new Minute UDF.
func NewMinute(date sql.Expression) sql.Expression {
	return &Minute{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (m *Minute) FunctionName() string {
	return "minute"
}

// Description implements sql.FunctionExpression
func (m *Minute) Description() string {
	return "returns the minutes of the given date."
}

func (m *Minute) String() string { return fmt.Sprintf("%s(%d)", m.FunctionName(), m.Child) }

// Type implements the Expression interface.
func (m *Minute) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Minute) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (m *Minute) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	// 文字列と数は TIME として読む（HOUR('13:04:05') は 13。上流の #3723）
	val, err := m.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if v, handled := timePartOf(ctx, val, func(h, m, s int) int { return m }); handled {
		return v, nil
	}
	return getDatePartOf(ctx, nil, val, minute)
}

// WithChildren implements the Expression interface.
func (m *Minute) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(m, len(children), 1)
	}
	return NewMinute(children[0]), nil
}

// Second is a function that returns the second of a date.
type Second struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Second)(nil)
var _ sql.CollationCoercible = (*Second)(nil)

// NewSecond creates a new Second UDF.
func NewSecond(date sql.Expression) sql.Expression {
	return &Second{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (s *Second) FunctionName() string {
	return "second"
}

// Description implements sql.FunctionExpression
func (s *Second) Description() string {
	return "returns the seconds of the given date."
}

func (s *Second) String() string { return fmt.Sprintf("%s(%s)", s.FunctionName(), s.Child) }

// Type implements the Expression interface.
func (s *Second) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Second) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (s *Second) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	// 文字列と数は TIME として読む（HOUR('13:04:05') は 13。上流の #3723）
	val, err := s.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if v, handled := timePartOf(ctx, val, func(h, m, s int) int { return s }); handled {
		return v, nil
	}
	return getDatePartOf(ctx, nil, val, second)
}

// WithChildren implements the Expression interface.
func (s *Second) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(s, len(children), 1)
	}
	return NewSecond(children[0]), nil
}

// DayOfWeek is a function that returns the day of the week from a date where
// 1 = Sunday, ..., 7 = Saturday.
type DayOfWeek struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*DayOfWeek)(nil)
var _ sql.CollationCoercible = (*DayOfWeek)(nil)

// NewDayOfWeek creates a new DayOfWeek UDF.
func NewDayOfWeek(date sql.Expression) sql.Expression {
	return &DayOfWeek{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (d *DayOfWeek) FunctionName() string {
	return "dayofweek"
}

// Description implements sql.FunctionExpression
func (d *DayOfWeek) Description() string {
	return "returns the day of the week of the given date."
}

func (d *DayOfWeek) String() string { return fmt.Sprintf("DAYOFWEEK(%s)", d.Child) }

// Type implements the Expression interface.
func (d *DayOfWeek) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*DayOfWeek) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *DayOfWeek) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, d.UnaryExpression, row, dayOfWeek)
}

// WithChildren implements the Expression interface.
func (d *DayOfWeek) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewDayOfWeek(children[0]), nil
}

// DayOfYear is a function that returns the day of the year from a date.
type DayOfYear struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*DayOfYear)(nil)
var _ sql.CollationCoercible = (*DayOfYear)(nil)

// NewDayOfYear creates a new DayOfYear UDF.
func NewDayOfYear(date sql.Expression) sql.Expression {
	return &DayOfYear{expression.UnaryExpression{Child: date}}
}

// FunctionName implements sql.FunctionExpression
func (d *DayOfYear) FunctionName() string {
	return "dayofyear"
}

// Description implements sql.FunctionExpression
func (d *DayOfYear) Description() string {
	return "returns the day of the year of the given date."
}

func (d *DayOfYear) String() string { return fmt.Sprintf("DAYOFYEAR(%s)", d.Child) }

// Type implements the Expression interface.
func (d *DayOfYear) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*DayOfYear) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *DayOfYear) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, d.UnaryExpression, row, dayOfYear)
}

// WithChildren implements the Expression interface.
func (d *DayOfYear) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewDayOfYear(children[0]), nil
}

func datePartFunc(fn func(time.Time) int) func(interface{}) interface{} {
	return func(v interface{}) interface{} {
		if v == nil {
			return nil
		}

		return int32(fn(v.(time.Time)))
	}
}

// YearWeek is a function that returns year and week for a date.
// The year in the result may be different from the year in the date argument for the first and the last week of the year.
// Details: https://dev.mysql.com/doc/refman/5.5/en/date-and-time-functions.html#function_yearweek
type YearWeek struct {
	date sql.Expression
	mode sql.Expression
}

var _ sql.FunctionExpression = (*YearWeek)(nil)
var _ sql.CollationCoercible = (*YearWeek)(nil)

// NewYearWeek creates a new YearWeek UDF
func NewYearWeek(args ...sql.Expression) (sql.Expression, error) {
	if len(args) == 0 {
		return nil, sql.ErrInvalidArgumentNumber.New("YEARWEEK", "1 or more", 0)
	}

	yw := &YearWeek{date: args[0]}
	if len(args) > 1 && args[1].Resolved() && types.IsInteger(args[1].Type()) {
		yw.mode = args[1]
	} else if len(args) > 1 && expression.IsBindVar(args[1]) {
		yw.mode = args[1]
	} else {
		yw.mode = expression.NewLiteral(0, types.Int64)
	}

	return yw, nil
}

// FunctionName implements sql.FunctionExpression
func (d *YearWeek) FunctionName() string {
	return "yearweek"
}

// Description implements sql.FunctionExpression
func (d *YearWeek) Description() string {
	return "returns year and week for a date. The year in the result may be different from the year in the date argument for the first and the last week of the year."
}

func (d *YearWeek) String() string { return fmt.Sprintf("YEARWEEK(%s, %s)", d.date, d.mode) } // HeimDB: %s（段階 120 のレビュー）

// Type implements the Expression interface.
func (d *YearWeek) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*YearWeek) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *YearWeek) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	arg, err := d.date.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	date, err := getDateOf(ctx, arg)
	if err != nil {
		return nil, err
	}
	if date == nil {
		return nil, nil
	}
	if isZeroDate(date.(time.Time)) {
		zeroDateWarning(ctx, d.date, arg) // HeimDB: not for a column, as in MySQL (stage 118)
		return nil, nil
	}
	yyyy, ok := year(date).(int32)
	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("YEARWEEK", "invalid year")
	}
	mm, ok := month(date).(int32)
	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("YEARWEEK", "invalid month")
	}
	dd, ok := day(date).(int32)
	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("YEARWEEK", "invalid day")
	}

	mode := int64(0)
	val, err := d.mode.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if val != nil {
		if i64, _, err := types.Int64.Convert(ctx, val); err == nil {
			if mode, ok = i64.(int64); ok {
				mode %= 8 // mode in [0, 7]
			}
		}
	}
	yyyy, week := calcWeek(yyyy, mm, dd, weekMode(mode)|weekBehaviourYear)

	return (yyyy * 100) + week, nil
}

// Resolved implements the Expression interface.
func (d *YearWeek) Resolved() bool {
	return d.date.Resolved() && d.mode.Resolved()
}

// Children implements the Expression interface.
func (d *YearWeek) Children() []sql.Expression { return []sql.Expression{d.date, d.mode} }

// IsNullable implements the Expression interface.
func (d *YearWeek) IsNullable() bool {
	return d.date.IsNullable()
}

// WithChildren implements the Expression interface.
func (*YearWeek) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewYearWeek(children...)
}

// Week is a function that returns year and week for a date.
// The year in the result may be different from the year in the date argument for the first and the last week of the year.
// Details: https://dev.mysql.com/doc/refman/5.5/en/date-and-time-functions.html#function_yearweek
type Week struct {
	date sql.Expression
	mode sql.Expression
}

var _ sql.FunctionExpression = (*Week)(nil)
var _ sql.CollationCoercible = (*Week)(nil)

// NewWeek creates a new Week UDF
func NewWeek(args ...sql.Expression) (sql.Expression, error) {
	if len(args) == 0 {
		return nil, sql.ErrInvalidArgumentNumber.New("YEARWEEK", "1 or more", 0)
	}

	w := &Week{date: args[0]}
	if len(args) > 1 && args[1].Resolved() && types.IsInteger(args[1].Type()) {
		w.mode = args[1]
	} else {
		w.mode = expression.NewLiteral(0, types.Int64)
	}

	return w, nil
}

// FunctionName implements sql.FunctionExpression
func (d *Week) FunctionName() string {
	return "week"
}

// Description implements sql.FunctionExpression
func (d *Week) Description() string {
	return "returns the week number."
}

func (d *Week) String() string { return fmt.Sprintf("WEEK(%s, %s)", d.date, d.mode.String()) }

func (d *Week) DebugString() string {
	return fmt.Sprintf("WEEK(%s, %s)", sql.DebugString(d.date), sql.DebugString(d.mode))
}

// Type implements the Expression interface.
func (d *Week) Type() sql.Type { return types.Int32 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Week) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *Week) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	arg, err := d.date.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	date, err := getDateOf(ctx, arg)
	if err != nil {
		return nil, err
	}
	if date == nil {
		// HeimDB: WEEK(NULL) is NULL, as in MySQL (it failed with 1105 invalid year; stage 118)
		return nil, nil
	}
	if t, ok := date.(time.Time); ok && isZeroDate(t) {
		zeroDateWarning(ctx, d.date, arg) // HeimDB: not for a column, as in MySQL (stage 118)
		return nil, nil
	}

	yyyy, ok := year(date).(int32)
	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("WEEK", "invalid year")
	}
	mm, ok := month(date).(int32)
	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("WEEK", "invalid month")
	}
	dd, ok := day(date).(int32)
	if !ok {
		return nil, sql.ErrInvalidArgumentDetails.New("WEEK", "invalid day")
	}

	mode := int64(0)
	val, err := d.mode.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if val != nil {
		if i64, _, err := types.Int64.Convert(ctx, val); err == nil {
			if mode, ok = i64.(int64); ok {
				mode %= 8 // mode in [0, 7]
			}
		}
	}

	yearForWeek, week := calcWeek(yyyy, mm, dd, weekMode(mode)|weekBehaviourYear)

	if yearForWeek < yyyy {
		week = 0
	} else if yearForWeek > yyyy {
		week = 53
	}

	return week, nil
}

// Resolved implements the Expression interface.
func (d *Week) Resolved() bool {
	return d.date.Resolved() && d.mode.Resolved()
}

// Children implements the Expression interface.
func (d *Week) Children() []sql.Expression { return []sql.Expression{d.date, d.mode} }

// IsNullable implements the Expression interface.
func (d *Week) IsNullable() bool {
	return d.date.IsNullable()
}

// WithChildren implements the Expression interface.
func (*Week) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewWeek(children...)
}

// Following solution of YearWeek was taken from tidb: https://github.com/pingcap/tidb/blob/master/types/mytime.go
type weekBehaviour int64

const (
	// weekBehaviourMondayFirst set Monday as first day of week; otherwise Sunday is first day of week
	weekBehaviourMondayFirst weekBehaviour = 1 << iota
	// If set, Week is in range 1-53, otherwise Week is in range 0-53.
	// Note that this flag is only relevant if WEEK_JANUARY is not set.
	weekBehaviourYear
	// If not set, Weeks are numbered according to ISO 8601:1988.
	// If set, the week that contains the first 'first-day-of-week' is week 1.
	weekBehaviourFirstWeekday
)

func (v weekBehaviour) test(flag weekBehaviour) bool {
	return (v & flag) != 0
}

func weekMode(mode int64) weekBehaviour {
	weekFormat := weekBehaviour(mode & 7)
	if (weekFormat & weekBehaviourMondayFirst) == 0 {
		weekFormat ^= weekBehaviourFirstWeekday
	}
	return weekFormat
}

// calcWeekday calculates weekday from daynr, returns 0 for Monday, 1 for Tuesday ...
func calcWeekday(daynr int32, sundayFirstDayOfWeek bool) int32 {
	daynr += 5
	if sundayFirstDayOfWeek {
		daynr++
	}
	return daynr % 7
}

// calcWeek calculates week and year for the time.
func calcWeek(yyyy, mm, dd int32, wb weekBehaviour) (int32, int32) {
	daynr := calcDaynr(yyyy, mm, dd)
	firstDaynr := calcDaynr(yyyy, 1, 1)
	mondayFirst := wb.test(weekBehaviourMondayFirst)
	weekYear := wb.test(weekBehaviourYear)
	firstWeekday := wb.test(weekBehaviourFirstWeekday)
	weekday := calcWeekday(firstDaynr, !mondayFirst)

	week, days := int32(0), int32(0)
	if mm == 1 && dd <= 7-weekday {
		if !weekYear &&
			((firstWeekday && weekday != 0) || (!firstWeekday && weekday >= 4)) {
			return yyyy, week
		}
		weekYear = true
		yyyy--
		days = calcDaysInYear(yyyy)
		firstDaynr -= days
		weekday = (weekday + 53*7 - days) % 7
	}

	if (firstWeekday && weekday != 0) ||
		(!firstWeekday && weekday >= 4) {
		days = daynr - (firstDaynr + 7 - weekday)
	} else {
		days = daynr - (firstDaynr - weekday)
	}

	if weekYear && days >= 52*7 {
		weekday = (weekday + calcDaysInYear(yyyy)) % 7
		if (!firstWeekday && weekday < 4) ||
			(firstWeekday && weekday == 0) {
			yyyy++
			week = 1
			return yyyy, week
		}
	}
	if days < 0 {
		// HeimDB: MySQL's days is unsigned, so the zero date's EXTRACT(WEEK), from
		// calc_week(0, 0, 0) with days -1, is 613566757 (stage 118)
		return yyyy, int32(uint32(days)/7 + 1)
	}
	week = days/7 + 1
	return yyyy, week
}

// calcDaysInYear calculates days in one year, it works with 0 <= yyyy <= 99.
func calcDaysInYear(yyyy int32) int32 {
	if (yyyy&3) == 0 && (yyyy%100 != 0 || (yyyy%400 == 0 && (yyyy != 0))) {
		return 366
	}
	return 365
}

// calcDaynr calculates days since 0000-00-00.
func calcDaynr(yyyy, mm, dd int32) int32 {
	if yyyy == 0 && mm == 0 {
		return 0
	}

	delsum := 365*yyyy + 31*(mm-1) + dd
	if mm <= 2 {
		yyyy--
	} else {
		delsum -= (mm*4 + 23) / 10
	}
	return delsum + yyyy/4 - ((yyyy/100+1)*3)/4
}

var (
	year      = datePartFunc((time.Time).Year)
	month     = zeroDatePartFunc(int32(0), func(t time.Time) int { return int(t.Month()) })
	day       = zeroDatePartFunc(int32(0), (time.Time).Day)
	weekday   = zeroDatePartFunc(nil, func(t time.Time) int { return (int(t.Weekday()) + 6) % 7 })
	hour      = datePartFunc((time.Time).Hour)
	minute    = datePartFunc((time.Time).Minute)
	second    = datePartFunc((time.Time).Second)
	dayOfWeek = zeroDatePartFunc(nil, func(t time.Time) int { return int(t.Weekday()) + 1 })
	dayOfYear = zeroDatePartFunc(nil, (time.Time).YearDay)
)

const maxCurrTimestampPrecision = 6

// Now is a function that returns the current time.
type Now struct {
	// prec stores the requested precision for fractional seconds.
	prec sql.Expression
	// alwaysUseExactTime controls whether the NOW() function gets the current time, or
	// uses a cached value that records the starting time of the query. By default, a
	// cached time is used, but in some cases (such as the SYSDATE() function), the func
	// needs to always return the exact current time of each function invocation().
	alwaysUseExactTime bool
}

func (n *Now) IsNonDeterministic() bool {
	return true
}

var _ sql.FunctionExpression = (*Now)(nil)
var _ sql.CollationCoercible = (*Now)(nil)

// NewNow returns a new Now node.
func NewNow(args ...sql.Expression) (sql.Expression, error) {
	n := &Now{}
	// parser should make it impossible to pass in more than one argument
	if len(args) > 0 {
		n.prec = args[0]
	}
	return n, nil
}

func subSecondPrecision(t time.Time, precision int) string {
	if precision == 0 {
		return ""
	}

	s := fmt.Sprintf(".%09d", t.Nanosecond())
	return s[:precision+1]
}

func fractionOfSecString(t time.Time) string {
	s := fmt.Sprintf("%09d", t.Nanosecond())
	s = s[:6]

	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '0' {
			break
		}

		s = s[:i]
	}

	if len(s) == 0 {
		return ""
	}

	return "." + s
}

// FunctionName implements sql.FunctionExpression
func (n *Now) FunctionName() string {
	return "now"
}

// Description implements sql.FunctionExpression
func (n *Now) Description() string {
	return "returns the current timestamp."
}

// Type implements the sql.Expression interface.
func (n *Now) Type() sql.Type {
	// TODO: precision
	if n.prec == nil {
		return types.Datetime
	}
	// HeimDB: DATETIME of a literal precision, as in MySQL (NOW(3) is DATETIME(3), shown
	// with 3 digits; it was DATETIME(6), so BIT_LENGTH(NOW(3)) was 208; stage 116, fourth review)
	if lit, ok := n.prec.(*expression.Literal); ok {
		if fsp, ok := types.CoalesceInt(lit.Value()); ok && fsp >= 0 && fsp <= 6 {
			return types.DatetimeWithFsp(fsp)
		}
	}
	return types.DatetimeMaxPrecision
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Now) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// String implements the sql.Expression interface.
func (n *Now) String() string {
	if n.prec == nil {
		return "NOW()"
	}

	return fmt.Sprintf("NOW(%s)", n.prec.String())
}

// IsNullable implements the sql.Expression interface.
func (n *Now) IsNullable() bool { return false }

// Resolved implements the sql.Expression interface.
func (n *Now) Resolved() bool {
	if n.prec == nil {
		return true
	}
	return n.prec.Resolved()
}

// Children implements the sql.Expression interface.
func (n *Now) Children() []sql.Expression {
	if n.prec == nil {
		return nil
	}
	return []sql.Expression{n.prec}
}

// Eval implements the sql.Expression interface.
func (n *Now) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	// Cannot evaluate with nil context
	if ctx == nil {
		return nil, fmt.Errorf("cannot Eval Now with nil context")
	}

	// The timestamp must be in the session time zone
	sessionTimeZone, err := SessionTimeZone(ctx)
	if err != nil {
		return nil, err
	}

	// For NOW(), we use the cached QueryTime, so that all NOW() calls in a query return the same value.
	// SYSDATE() requires that we use the *exact* current time, and not use the cached version.
	currentTime := ctx.QueryTime()
	if n.alwaysUseExactTime {
		currentTime = sql.Now()
	}

	// If no arguments, just return with 0 precision
	// The way the parser is implemented 0 should always be passed in; have this here just in case
	if n.prec == nil {
		t, ok := gmstime.ConvertTimeZone(currentTime, gmstime.SystemTimezoneOffset(), sessionTimeZone)
		if !ok {
			return nil, fmt.Errorf("invalid time zone: %s", sessionTimeZone)
		}
		tt := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
		return tt, nil
	}

	// Should syntax error before this; check anyway
	if types.IsNull(n.prec) {
		return nil, ErrTimeUnexpectedlyNil.New(n.FunctionName())
	}

	// Evaluate precision
	prec, err := n.prec.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	// Should syntax error before this; check anyway
	if prec == nil {
		return nil, ErrTimeUnexpectedlyNil.New(n.FunctionName())
	}

	// Must receive integer
	// Should syntax error before this; check anyway
	fsp, ok := types.CoalesceInt(prec)
	if !ok {
		return nil, sql.ErrInvalidArgumentType.New(n.FunctionName())
	}

	// Parse and return answer
	if fsp > maxCurrTimestampPrecision {
		return nil, ErrTooHighPrecision.New(fsp, n.FunctionName(), maxCurrTimestampPrecision)
	} else if fsp < 0 {
		// Should syntax error before this; check anyway
		return nil, sql.ErrInvalidArgumentType.New(n.FunctionName())
	}

	// Get the timestamp
	t, ok := gmstime.ConvertTimeZone(currentTime, gmstime.SystemTimezoneOffset(), sessionTimeZone)
	if !ok {
		return nil, fmt.Errorf("invalid time zone: %s", sessionTimeZone)
	}

	// Calculate precision
	precision := 1
	for i := 0; i < 9-fsp; i++ {
		precision *= 10
	}

	// Round down nano based on precision
	nano := precision * (t.Nanosecond() / precision)

	// Generate a new timestamp
	tt := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), nano, time.UTC)

	return tt, nil
}

// WithChildren implements the Expression interface.
func (n *Now) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewNow(children...)
}

// NewSysdate returns a new SYSDATE() function, using the supplied |args| for an
// optional value for fractional second precision. The SYSDATE() function is a synonym
// for NOW(), but does NOT use the query's cached start time, and instead always returns
// the current time, even when executed multiple times in a query or stored procedure.
// https://dev.mysql.com/doc/refman/8.0/en/date-and-time-functions.html#function_sysdate
func NewSysdate(args ...sql.Expression) (sql.Expression, error) {
	n, err := NewNow(args...)
	n.(*Now).alwaysUseExactTime = true
	return n, err
}

// SessionTimeZone returns a MySQL timezone offset string for the value of @@session_time_zone. If the session
// timezone is set to SYSTEM, then the system timezone offset is calculated and returned.
func SessionTimeZone(ctx *sql.Context) (string, error) {
	sessionTimeZoneVar, err := ctx.GetSessionVariable(ctx, "time_zone")
	if err != nil {
		return "", err
	}

	sessionTimeZone, ok := sessionTimeZoneVar.(string)
	if !ok {
		return "", fmt.Errorf("invalid type for @@session.time_zone: %T", sessionTimeZoneVar)
	}

	if sessionTimeZone == "SYSTEM" {
		sessionTimeZone = gmstime.SystemTimezoneOffset()
	}
	return sessionTimeZone, nil
}

// UTCTimestamp is a function that returns the current time.
type UTCTimestamp struct {
	precision *int
}

var _ sql.FunctionExpression = (*UTCTimestamp)(nil)
var _ sql.CollationCoercible = (*UTCTimestamp)(nil)

// NewUTCTimestamp returns a new UTCTimestamp node.
func NewUTCTimestamp(args ...sql.Expression) (sql.Expression, error) {
	// TODO: Add context parameter
	ctx := context.Background()
	var precision *int
	if len(args) > 1 {
		return nil, sql.ErrInvalidArgumentNumber.New("UTC_TIMESTAMP", 1, len(args))
	} else if len(args) == 1 {
		argType := args[0].Type().Promote()
		if argType != types.Int64 && argType != types.Uint64 {
			return nil, sql.ErrInvalidType.New(args[0].Type().String())
		}
		// todo: making a context here is expensive
		val, err := args[0].Eval(sql.NewEmptyContext(), nil)
		if err != nil {
			return nil, err
		}
		precisionArg, _, err := types.Int32.Convert(ctx, val)

		if err != nil {
			return nil, err
		}

		n := int(precisionArg.(int32))
		if n < 0 || n > 6 {
			return nil, sql.ErrValueOutOfRange.New("precision", "utc_timestamp")
		}
		precision = &n
	}

	return &UTCTimestamp{precision}, nil
}

// FunctionName implements sql.FunctionExpression
func (ut *UTCTimestamp) FunctionName() string {
	return "utc_timestamp"
}

// Description implements sql.FunctionExpression
func (ut *UTCTimestamp) Description() string {
	return "returns the current UTC timestamp."
}

// Type implements the sql.Expression interface.
func (ut *UTCTimestamp) Type() sql.Type {
	// HeimDB: DATETIME of the precision argument, as in MySQL (stage 116, fourth review)
	if ut.precision == nil {
		return types.Datetime
	}
	return types.DatetimeWithFsp(*ut.precision)
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*UTCTimestamp) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (ut *UTCTimestamp) String() string {
	if ut.precision == nil {
		return "UTC_TIMESTAMP()"
	}

	return fmt.Sprintf("UTC_TIMESTAMP(%d)", *ut.precision)
}

// IsNullable implements the sql.Expression interface.
func (ut *UTCTimestamp) IsNullable() bool { return false }

// Resolved implements the sql.Expression interface.
func (ut *UTCTimestamp) Resolved() bool { return true }

// Children implements the sql.Expression interface.
func (ut *UTCTimestamp) Children() []sql.Expression { return nil }

// Eval implements the sql.Expression interface.
func (ut *UTCTimestamp) Eval(ctx *sql.Context, _ sql.Row) (interface{}, error) {
	t := ctx.QueryTime()
	// HeimDB: cut to the precision argument (0 without one), as in MySQL; it kept the
	// microseconds, so UTC_TIMESTAMP() showed them (stage 116, fourth review)
	unit := 1000000000
	if ut.precision != nil {
		for i := 0; i < *ut.precision; i++ {
			unit /= 10
		}
	}
	nano := unit * (t.Nanosecond() / unit)
	tt := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), nano, t.Location())
	return tt.UTC(), nil
}

// WithChildren implements the Expression interface.
func (ut *UTCTimestamp) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewUTCTimestamp(children...)
}

// Date a function takes the DATE part out from a datetime expression.
type Date struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Date)(nil)
var _ sql.CollationCoercible = (*Date)(nil)

// FunctionName implements sql.FunctionExpression
func (d *Date) FunctionName() string {
	return "date"
}

// Description implements sql.FunctionExpression
func (d *Date) Description() string {
	return "returns the date part of the given date."
}

// NewDate returns a new Date node.
func NewDate(date sql.Expression) sql.Expression {
	return &Date{expression.UnaryExpression{Child: date}}
}

func (d *Date) String() string { return fmt.Sprintf("DATE(%s)", d.Child) }

// Type implements the Expression interface.
func (d *Date) Type() sql.Type { return types.Date }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Date) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (d *Date) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	return getDatePart(ctx, d.UnaryExpression, row, func(v interface{}) interface{} {
		if v == nil {
			return nil
		}

		// HeimDB: the zero date is '0000-00-00' (it was '0000-01-01'; stage 116)
		if v.(time.Time).Equal(types.ZeroTime) {
			return types.ZeroDateStr
		}
		return v.(time.Time).Format("2006-01-02")
	})
}

// WithChildren implements the Expression interface.
func (d *Date) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewDate(children[0]), nil
}

// UnaryDatetimeFunc is a sql.Function which takes a single datetime argument
type UnaryDatetimeFunc struct {
	expression.UnaryExpression
	// Name is the name of the function
	Name string
	// SQLType is the return type of the function
	SQLType sql.Type
}

func NewUnaryDatetimeFunc(arg sql.Expression, name string, sqlType sql.Type) *UnaryDatetimeFunc {
	return &UnaryDatetimeFunc{expression.UnaryExpression{Child: arg}, name, sqlType}
}

// FunctionName implements sql.FunctionExpression
func (dtf *UnaryDatetimeFunc) FunctionName() string {
	return dtf.Name
}

func (dtf *UnaryDatetimeFunc) EvalChild(ctx *sql.Context, row sql.Row) (interface{}, error) {
	val, err := dtf.Child.Eval(ctx, row)

	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	ret, _, err := types.DatetimeMaxPrecision.Convert(ctx, val)
	return ret, err
}

// evalChildArg は EvalChild と同じで、変換する前の引数の値 arg も返す（ゼロの日付の警告の文に使う。HeimDB: 段階 118）。
func (dtf *UnaryDatetimeFunc) evalChildArg(ctx *sql.Context, row sql.Row) (arg, val interface{}, err error) {
	arg, err = dtf.Child.Eval(ctx, row)
	if err != nil || arg == nil {
		return nil, nil, err
	}
	val, _, err = types.DatetimeMaxPrecision.Convert(ctx, arg)
	return arg, val, err
}

// String implements the fmt.Stringer interface.
func (dtf *UnaryDatetimeFunc) String() string {
	return fmt.Sprintf("%s(%s)", strings.ToUpper(dtf.Name), dtf.Child.String())
}

// Type implements the Expression interface.
func (dtf *UnaryDatetimeFunc) Type() sql.Type {
	return dtf.SQLType
}

// DayName implements the DAYNAME function
type DayName struct {
	*UnaryFunc
}

var _ sql.FunctionExpression = (*DayName)(nil)

func NewDayName(arg sql.Expression) sql.Expression {
	return &DayName{NewUnaryFunc(arg, "DAYNAME", types.Text)}
}

// FunctionName implements sql.FunctionExpression
func (d *DayName) FunctionName() string {
	return "dayname"
}

// Description implements sql.FunctionExpression
func (d *DayName) Description() string {
	return "returns the name of the weekday."
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*DayName) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return ctx.GetCollation(), 4
}

func (d *DayName) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	val, err := d.EvalChild(ctx, row)
	arg := val
	if val == nil && err == nil {
		return nil, nil // HeimDB: DAYNAME(NULL) gives no warning, as in MySQL (stage 118)
	}
	if err != nil {
		ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
		return nil, nil
	}

	if s, ok := val.(string); ok {
		val, _, err = types.DatetimeMaxPrecision.Convert(ctx, s)
		if err != nil {
			ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
			return nil, nil
		}
	}

	t, ok := val.(time.Time)
	if !ok {
		ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
		return nil, nil
	}
	if isZeroDate(t) {
		zeroDateWarning(ctx, d.Child, arg) // HeimDB: stage 118
		return nil, nil
	}

	return t.Weekday().String(), nil
}

func (d *DayName) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewDayName(children[0]), nil
}

// Microsecond implements the MICROSECOND function
type Microsecond struct {
	*UnaryDatetimeFunc
}

var _ sql.FunctionExpression = (*Microsecond)(nil)
var _ sql.CollationCoercible = (*Microsecond)(nil)

// Description implements sql.FunctionExpression
func (m *Microsecond) Description() string {
	return "returns the microseconds from argument."
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Microsecond) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func NewMicrosecond(arg sql.Expression) sql.Expression {
	return &Microsecond{NewUnaryDatetimeFunc(arg, "MICROSECOND", types.Uint64)}
}

func (m *Microsecond) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	if ts, ok, err := timeArg(ctx, m.Child, row); err != nil || ok {
		if err != nil || ts == nil {
			return nil, err
		}
		us := ts.AsMicroseconds()
		if us < 0 {
			us = -us
		}
		return uint64(us % 1000000), nil
	}
	val, err := m.EvalChild(ctx, row)
	if err != nil {
		if types.ErrConvertingToTime.Is(err) {
			// HeimDB: NULL with warning 1292, as in MySQL (MICROSECOND('x'); stage 116)
			ctx.Warn(1292, "%s", err.Error())
			return nil, nil
		}
		return nil, err
	}

	switch v := val.(type) {
	case time.Time:
		return uint64(v.Nanosecond()) / uint64(time.Microsecond), nil
	case nil:
		return nil, nil
	default:
		ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
		return nil, nil
	}
}

func (m *Microsecond) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(m, len(children), 1)
	}
	return NewMicrosecond(children[0]), nil
}

// MonthName implements the MONTHNAME function
type MonthName struct {
	*UnaryDatetimeFunc
}

var _ sql.FunctionExpression = (*MonthName)(nil)
var _ sql.CollationCoercible = (*MonthName)(nil)

func NewMonthName(arg sql.Expression) sql.Expression {
	return &MonthName{NewUnaryDatetimeFunc(arg, "MONTHNAME", types.Text)}
}

// Description implements sql.FunctionExpression
func (d *MonthName) Description() string {
	return "returns the name of the month."
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*MonthName) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return ctx.GetCollation(), 4
}

func (d *MonthName) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	val, err := d.EvalChild(ctx, row)
	if err != nil {
		return nil, err
	}

	switch v := val.(type) {
	case time.Time:
		if isZeroDate(v) {
			// HeimDB: MONTHNAME of the zero date gives no warning in MySQL (stage 118)
			return nil, nil
		}
		return v.Month().String(), nil
	case nil:
		return nil, nil
	default:
		ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
		return nil, nil
	}
}

func (d *MonthName) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(d, len(children), 1)
	}
	return NewMonthName(children[0]), nil
}

// TimeToSec implements the time_to_sec function
type TimeToSec struct {
	*UnaryDatetimeFunc
}

var _ sql.FunctionExpression = (*TimeToSec)(nil)
var _ sql.CollationCoercible = (*TimeToSec)(nil)

func NewTimeToSec(arg sql.Expression) sql.Expression {
	return &TimeToSec{NewUnaryDatetimeFunc(arg, "TIME_TO_SEC", types.Int64)} // HeimDB: signed, as a negative TIME gives a negative number (stage 116)
}

// Description implements sql.FunctionExpression
func (m *TimeToSec) Description() string {
	return "returns the argument converted to seconds."
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*TimeToSec) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (m *TimeToSec) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	if ts, ok, err := timeArg(ctx, m.Child, row); err != nil || ok {
		if err != nil || ts == nil {
			return nil, err
		}
		return ts.AsMicroseconds() / 1000000, nil
	}
	val, err := m.EvalChild(ctx, row)
	if err != nil {
		return nil, err
	}

	switch v := val.(type) {
	case time.Time:
		return int64(v.Hour()*3600 + v.Minute()*60 + v.Second()), nil
	case nil:
		return nil, nil
	default:
		ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
		return nil, nil
	}
}

func (m *TimeToSec) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(m, len(children), 1)
	}
	return NewTimeToSec(children[0]), nil
}

// WeekOfYear implements the weekofyear function
type WeekOfYear struct {
	*UnaryDatetimeFunc
}

var _ sql.FunctionExpression = (*WeekOfYear)(nil)
var _ sql.CollationCoercible = (*WeekOfYear)(nil)

func NewWeekOfYear(arg sql.Expression) sql.Expression {
	return &WeekOfYear{NewUnaryDatetimeFunc(arg, "WEEKOFYEAR", types.Uint64)}
}

// Description implements sql.FunctionExpression
func (m *WeekOfYear) Description() string {
	return "returns the calendar week of the date (1-53)."
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*WeekOfYear) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

func (m *WeekOfYear) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	arg, val, err := m.evalChildArg(ctx, row)
	if err != nil {
		return nil, err
	}

	switch v := val.(type) {
	case time.Time:
		if isZeroDate(v) {
			zeroDateWarning(ctx, m.Child, arg) // HeimDB: stage 118
			return nil, nil
		}
		_, wk := v.ISOWeek()
		return wk, nil
	case nil:
		return nil, nil
	default:
		ctx.Warn(1292, "%s", types.ErrConvertingToTime.New(val).Error())
		return nil, nil
	}
}

func (m *WeekOfYear) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(m, len(children), 1)
	}
	return NewWeekOfYear(children[0]), nil
}

type CurrTime struct {
	prec sql.Expression
}

func (c CurrTime) IsNonDeterministic() bool {
	return true
}

var _ sql.FunctionExpression = (*CurrTime)(nil)
var _ sql.CollationCoercible = (*CurrTime)(nil)

func NewCurrTime(args ...sql.Expression) (sql.Expression, error) {
	c := &CurrTime{}
	// parser should make it impossible to pass in more than one argument
	if len(args) > 0 {
		c.prec = args[0]
	}
	return c, nil
}

// FunctionName implements sql.FunctionExpression
func (c *CurrTime) FunctionName() string {
	return "current_time"
}

// Description implements sql.FunctionExpression
func (c *CurrTime) Description() string {
	return "returns the current time."
}

// Type implements the sql.Expression interface.
func (c *CurrTime) Type() sql.Type {
	return types.Time
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*CurrTime) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// String implements the sql.Expression interface.
func (c *CurrTime) String() string {
	if c.prec == nil {
		return "CURRENT_TIME()"
	}

	return fmt.Sprintf("CURRENT_TIME(%s)", c.prec.String())
}

// IsNullable implements the sql.Expression interface.
func (c *CurrTime) IsNullable() bool { return false }

// Resolved implements the sql.Expression interface.
func (c *CurrTime) Resolved() bool {
	if c.prec == nil {
		return true
	}
	return c.prec.Resolved()
}

// Children implements the sql.Expression interface.
func (c *CurrTime) Children() []sql.Expression {
	if c.prec == nil {
		return nil
	}
	return []sql.Expression{c.prec}
}

// Eval implements sql.Expression
func (c *CurrTime) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	newNow, err := NewNow(c.prec)
	if err != nil {
		return nil, err
	}

	result, err := newNow.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if t, ok := result.(time.Time); ok {
		// HeimDB: with the fraction of the precision argument, as in MySQL (CURTIME(2) is
		// '10:00:00.12'; it had no fraction; stage 116, fourth review)
		fsp := 0
		if lit, ok := c.prec.(*expression.Literal); ok {
			if n, ok := types.CoalesceInt(lit.Value()); ok && n > 0 && n <= 6 {
				fsp = n
			}
		}
		return fmt.Sprintf("%02d:%02d:%02d", t.Hour(), t.Minute(), t.Second()) + subSecondPrecision(t, fsp), nil
	} else {
		return nil, fmt.Errorf("unexpected type %T for NOW() result", result)
	}
}

// WithChildren implements sql.Expression
func (c *CurrTime) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NoArgFuncWithChildren(c, children)
}

// Time is a function takes the Time part out from a datetime expression.
type Time struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Time)(nil)
var _ sql.CollationCoercible = (*Time)(nil)

// NewTime returns a new Date node.
func NewTime(time sql.Expression) sql.Expression {
	return &Time{expression.UnaryExpression{Child: time}}
}

func (t *Time) FunctionName() string {
	return "time"
}

func (t *Time) Description() string {
	return "extracts the time part of a time or datetime expression and returns it as a string"
}

func (t *Time) String() string {
	return fmt.Sprintf("TIME(%s)", t.Child)
}

// Type implements the Expression interface.
func (t *Time) Type() sql.Type {
	return types.Time
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*Time) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// Eval implements the Expression interface.
func (t *Time) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	v, err := t.UnaryExpression.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}

	// HeimDB: a string of a time only ('13:04:05') is a time; it was read as the
	// date 2013-04-05, so TIME('13:04:05') was 00:00:00 (stage 116)
	if str, ok := v.(string); ok && isTimeOnly(strings.TrimSpace(str)) {
		if val, _, err := types.Time.Convert(ctx, str); err == nil {
			return val, nil
		}
	}

	// convert to date
	date, err := types.DatetimeMaxPrecision.ConvertWithoutRangeCheck(ctx, v)
	if err == nil {
		h, m, s := date.Clock()
		us := date.Nanosecond() / 1000
		return types.Timespan(1000000*(3600*h+60*m+s) + us), nil
	}

	// convert to time
	val, _, err := types.Time.Convert(ctx, v)
	if err != nil {
		ctx.Warn(1292, "%s", err.Error())
		return nil, nil
	}
	return val, nil
}

// WithChildren implements the Expression interface.
func (t *Time) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(t, len(children), 1)
	}
	return NewTime(children[0]), nil
}

// timeArg evaluates e and, for a TIME value or a string of a time only
// ('13:04:05.5'), returns it as a time (ok is true; ts is nil for NULL). The
// date functions read such a string as a date and failed with 1105
// (MICROSECOND('13:04:05.123456')) or read 0 (HeimDB, stage 116).
func timeArg(ctx *sql.Context, e sql.Expression, row sql.Row) (*types.Timespan, bool, error) {
	if !types.IsTimespan(e.Type()) && !types.IsText(e.Type()) {
		return nil, false, nil
	}
	v, err := e.Eval(ctx, row)
	if err != nil {
		return nil, true, err
	}
	if v == nil {
		return nil, types.IsTimespan(e.Type()), nil
	}
	if ts, ok := v.(types.Timespan); ok {
		return &ts, true, nil
	}
	str, ok := v.(string)
	if !ok || !isTimeOnly(strings.TrimSpace(str)) {
		return nil, false, nil
	}
	cv, _, err := types.Time.Convert(ctx, str)
	ts, ok := cv.(types.Timespan)
	if err != nil || !ok {
		return nil, false, nil
	}
	return &ts, true, nil
}
