// Copyright 2026 Tsukinowa Inc.
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

// HeimDB: 数と日時の変換（docs/engine.md の段階 116）。

package types

import (
	"context"
	"math"
	"time"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// numberValue returns v as a decimal if it is a Go number (an integer, a floating point number or a decimal).
func numberValue(v interface{}) (decimal.Decimal, bool) {
	switch n := v.(type) {
	case int:
		return decimal.NewFromInt(int64(n)), true
	case int8:
		return decimal.NewFromInt(int64(n)), true
	case int16:
		return decimal.NewFromInt(int64(n)), true
	case int32:
		return decimal.NewFromInt(int64(n)), true
	case int64:
		return decimal.NewFromInt(n), true
	case uint:
		return decimal.NewFromUint64(uint64(n)), true
	case uint8:
		return decimal.NewFromInt(int64(n)), true
	case uint16:
		return decimal.NewFromInt(int64(n)), true
	case uint32:
		return decimal.NewFromInt(int64(n)), true
	case uint64:
		return decimal.NewFromUint64(n), true
	case float32:
		if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
			return decimal.Decimal{}, false
		}
		return decimal.NewFromFloat32(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return decimal.Decimal{}, false
		}
		return decimal.NewFromFloat(n), true
	case decimal.Decimal:
		return n, true
	case decimal.NullDecimal:
		return n.Decimal, n.Valid
	}
	return decimal.Decimal{}, false
}

// IsNumberValue reports whether v is a Go number that NumberToDatetime reads.
func IsNumberValue(v interface{}) bool {
	_, ok := numberValue(v)
	return ok
}

// NumberToDatetime reads a number as MySQL's number_to_datetime does (with
// TIME_FUZZY_DATE, as CAST): 0 is the zero date, YYYYMMDD, YYMMDD (YY below 70
// is 20YY), YYYYMMDDhhmmss and YYMMDDhhmmss, with the fraction as microseconds
// for the forms with a time (CAST(20240101103000.5 AS DATETIME) is 10:30:00.5
// before the type's rounding, 20240101.5 is 2024-01-01). ok is false for a
// negative number, a number of another form (1, 100) and an invalid date or
// time (20240230, 20241301, a zero month or day).
func NumberToDatetime(v interface{}) (time.Time, bool) {
	d, ok := numberValue(v)
	if !ok || d.Sign() < 0 {
		return time.Time{}, false
	}
	ip := d.Truncate(0)
	if !ip.LessThanOrEqual(decimal.NewFromInt(99999999999999)) {
		return time.Time{}, false
	}
	nr := ip.IntPart()
	if nr == 0 {
		return zeroTime, true
	}
	frac := d.Sub(ip)
	hasTime := false
	switch {
	case nr >= 10000101000000:
		hasTime = true
	case nr < 101:
		return time.Time{}, false
	case nr <= 69*10000+1231:
		nr = (nr + 20000000) * 1000000
	case nr < 70*10000+101:
		return time.Time{}, false
	case nr <= 991231:
		nr = (nr + 19000000) * 1000000
	case nr <= 99991231:
		// below 10000101 needs TIME_FUZZY_DATE, which CAST has (9991231 is 0999-12-31)
		nr = nr * 1000000
	case nr < 101000000:
		return time.Time{}, false
	case nr <= 69*10000000000+1231235959:
		nr = nr + 20000000000000
		hasTime = true
	case nr < 70*10000000000+101000000:
		return time.Time{}, false
	case nr <= 991231235959:
		nr = nr + 19000000000000
		hasTime = true
	default:
		return time.Time{}, false
	}
	date, tod := nr/1000000, nr%1000000
	year, month, day := int(date/10000), int(date/100%100), int(date%100)
	hour, minute, second := int(tod/10000), int(tod/100%100), int(tod%100)
	if month < 1 || month > 12 || day < 1 || day > daysIn(time.Month(month), year) || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	micro := 0
	if hasTime {
		micro = int(frac.Shift(6).Round(0).IntPart())
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	return t.Add(time.Duration(micro) * time.Microsecond), true
}

func daysIn(m time.Month, year int) int {
	return time.Date(year, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// DatetimeToNumber returns a date or time value t of type typ as MySQL's
// number: YYYYMMDD for a DATE, YYYYMMDDhhmmss for a DATETIME or TIMESTAMP
// with the type's fraction, and 0 for the zero date. ok is false for a value
// that is not a time.Time.
func DatetimeToNumber(t interface{}, typ sql.Type) (decimal.Decimal, bool) {
	tt, ok := t.(time.Time)
	if !ok {
		return decimal.Decimal{}, false
	}
	tt = tt.UTC()
	if tt.Equal(zeroTime) {
		if dt, ok := typ.(sql.DatetimeType); ok && dt.Precision() > 0 && typ.Type() != sqltypes.Date {
			return decimal.New(0, -int32(dt.Precision())), true
		}
		return decimal.Zero, true
	}
	date := int64(tt.Year())*10000 + int64(tt.Month())*100 + int64(tt.Day())
	if typ != nil && typ.Type() == sqltypes.Date {
		return decimal.NewFromInt(date), true
	}
	n := decimal.NewFromInt(date*1000000 + int64(tt.Hour())*10000 + int64(tt.Minute())*100 + int64(tt.Second()))
	prec := 0
	if dt, ok := typ.(sql.DatetimeType); ok {
		prec = dt.Precision()
	}
	if prec > 0 {
		micro := decimal.NewFromInt(int64(tt.Nanosecond() / 1000)).Shift(-6)
		n = n.Add(micro).Round(int32(prec))
	}
	return n, true
}

// TemporalNumber returns the number of a date, datetime or TIME value v of type
// typ, as MySQL uses it in a numeric context (CAST AS SIGNED, d + 0, ABS):
// YYYYMMDD for a DATE, YYYYMMDDhhmmss for a DATETIME or TIMESTAMP and
// [-]hhmmss for a TIME, with the type's fraction. A string of such a type (the
// result of DATE()) is read first. ok is false for any other type.
func TemporalNumber(ctx context.Context, typ sql.Type, v interface{}) (decimal.Decimal, bool) {
	if v == nil {
		return decimal.Decimal{}, false
	}
	if ts, ok := v.(Timespan); ok {
		return timespanNumber(ts), true
	}
	if _, isTime := typ.(TimespanType_); isTime {
		cv, _, err := typ.Convert(ctx, v)
		if ts, ok := cv.(Timespan); ok && err == nil {
			return timespanNumber(ts), true
		}
		return decimal.Decimal{}, false
	}
	dt, ok := typ.(sql.DatetimeType)
	if !ok {
		return decimal.Decimal{}, false
	}
	if _, isTime := v.(time.Time); !isTime {
		cv, _, err := dt.Convert(ctx, v)
		if err != nil && !TruncatedDatetime(err) {
			return decimal.Decimal{}, false
		}
		v = cv
	}
	return DatetimeToNumber(v, typ)
}

func timespanNumber(ts Timespan) decimal.Decimal {
	neg, h, m, s, us := ts.timespanToUnits()
	n := decimal.NewFromInt(int64(h)*10000 + int64(m)*100 + int64(s))
	if us != 0 {
		n = n.Add(decimal.New(int64(us), -6))
	}
	if neg {
		n = n.Neg()
	}
	return n
}
