package function

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// ゼロの日付（'0000-00-00'、0、false）の日時の関数の値は、MySQL と同じく年、月、日、四半期が 0 で、曜日、週、名前、
// 差、足し算は NULL（上流の #3305）。
func TestZeroDateFunctions(t *testing.T) {
	ctx := sql.NewEmptyContext()
	zero := expression.NewLiteral("0000-00-00", types.LongText)
	zeroDatetime := expression.NewLiteral("0000-00-00 00:00:00", types.LongText)
	other := expression.NewLiteral("2020-10-10", types.LongText)
	interval := expression.NewInterval(expression.NewLiteral(int64(1), types.Int64), "DAY")
	dateAdd, err := NewDateAdd(zero, interval)
	require.NoError(t, err)
	dateAddInt, err := NewDateAdd(expression.NewLiteral(0, types.Int64), interval)
	require.NoError(t, err)
	dateAddFalse, err := NewDateAdd(expression.NewLiteral(false, types.Boolean), interval)
	require.NoError(t, err)
	dateSub, err := NewDateSub(zero, interval)
	require.NoError(t, err)
	for _, c := range []struct {
		name string
		e    sql.Expression
		want interface{}
	}{
		{"year", NewYear(zero), int32(0)},
		{"month", NewMonth(zero), int32(0)},
		{"day", NewDay(zero), int32(0)},
		{"quarter", NewQuarter(zero), int32(0)},
		{"month of 0", NewMonth(expression.NewLiteral(0, types.Int64)), int32(0)},
		{"month of false", NewMonth(expression.NewLiteral(false, types.Boolean)), int32(0)},
		{"dayofweek", NewDayOfWeek(zero), nil},
		{"weekday", NewWeekday(zero), nil},
		{"dayofyear", NewDayOfYear(zero), nil},
		{"dayname", NewDayName(zero), nil},
		{"monthname", NewMonthName(zero), nil},
		{"weekofyear", NewWeekOfYear(zero), nil},
		{"datediff left", NewDateDiff(zero, other), nil},
		{"datediff right", NewDateDiff(other, zeroDatetime), nil},
		{"date_add", dateAdd, nil},
		{"date_add 0", dateAddInt, nil},
		{"date_add false", dateAddFalse, nil},
		{"date_sub", dateSub, nil},
		{"date_format", NewDateFormat(zeroDatetime, expression.NewLiteral("%Y-%m-%d %H:%i:%s", types.LongText)), "0000-00-00 00:00:00"},
		{"date_format %D %y %c %e", NewDateFormat(zero, expression.NewLiteral("%D %y %c %e %%", types.LongText)), "0th 00 0 0 %"},
		{"date_format name", NewDateFormat(zero, expression.NewLiteral("%W", types.LongText)), nil},
		{"date_format %j %U %v %X %x", NewDateFormat(zero, expression.NewLiteral("%j %U %v %X %x", types.LongText)), "000 613566757 01 0000 0001"},
		{"str_to_date zero with NO_ZERO_DATE", NewStrToDate(zero, expression.NewLiteral("%Y-%m-%d", types.LongText)), nil},
		{"str_to_date year only", NewStrToDate(expression.NewLiteral("0", types.LongText), expression.NewLiteral("%Y", types.LongText)), nil},
		{"extract month", NewExtract(expression.NewLiteral("MONTH", types.LongText), zero), 0},
		{"extract year_month", NewExtract(expression.NewLiteral("YEAR_MONTH", types.LongText), zero), 0},
		// ゼロでない日付は前と同じ
		{"month 2020", NewMonth(other), int32(10)},
		{"quarter 2020", NewQuarter(other), int32(4)},
		{"dayofweek 2020", NewDayOfWeek(other), int32(7)},
		{"date_format 2020", NewDateFormat(other, expression.NewLiteral("%Y-%m-%d %W", types.LongText)), "2020-10-10 Saturday"},
	} {
		got, err := c.e.Eval(ctx, nil)
		require.NoError(t, err, c.name)
		require.Equal(t, c.want, got, c.name)
	}
}

// WordPress の sql_mode（NO_ZERO_DATE がない）では、STR_TO_DATE はゼロの日付を返す（MySQL と同じ）。
func TestStrToDateZeroDate(t *testing.T) {
	ctx := sql.NewEmptyContext()
	require.NoError(t, ctx.SetSessionVariable(ctx, "sql_mode", "NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"))
	got, err := NewStrToDate(expression.NewLiteral("0000-00-00", types.LongText), expression.NewLiteral("%Y-%m-%d", types.LongText)).Eval(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, types.ZeroTime, got)
	for _, c := range []struct {
		s, format string
		zero      bool
	}{
		{"2024-00-00", "%Y-%m-%d", false},
		// %y と 2 桁以下の %Y は 2 桁の年（00 は 2000 年）なので、MySQL でも NULL
		{"000000", "%y%m%d", false},
		{"00-00-00", "%y-%m-%d", false},
		{"0-0-0", "%Y-%m-%d", false},
		{"00000-00-00", "%Y-%m-%d", false},
		{"0000-00-00", "%d-%m-%Y", false},
		{"000-00-00", "%Y-%m-%d", true},
		{"0000-0-0", "%Y-%m-%d", true},
		{"0000-00-00", "%Y-%c-%e", true},
		{"00000000", "%Y%m%d", true},
		{"0000-00-00", "%Y-%m-%d %H:%i:%s", true},
		{"0000-00-00abc", "%Y-%m-%d", true},
		{"00", "%m", true},
	} {
		got, err := NewStrToDate(expression.NewLiteral(c.s, types.LongText), expression.NewLiteral(c.format, types.LongText)).Eval(ctx, nil)
		require.NoError(t, err, c.s)
		if c.zero {
			require.Equal(t, types.ZeroTime, got, "%s %s", c.s, c.format)
		} else {
			require.Nil(t, got, "%s %s", c.s, c.format)
		}
	}
}

// 段階 118: ゼロの日付の TO_DAYS、LAST_DAY、CONVERT_TZ は NULL、EXTRACT(WEEK) は MySQL の calc_week(0, 0, 0) の
// 613566757、WEEK(NULL) と YEARWEEK(NULL) は NULL。警告 1292 は、日付と日時の列（GetField の time.Time のゼロ値）には
// 出さず、文字列にはその値を書く。LAST_DAY だけは列にも '0000-00-00' で出し、MONTHNAME は文字列にも出さない（本物の
// MySQL 8.0 の WordPress の sql_mode で確かめた）。
func TestZeroDateFunctionsStage118(t *testing.T) {
	zeroStr := expression.NewLiteral("0000-00-00", types.LongText)
	zeroDatetimeStr := expression.NewLiteral("0000-00-00 00:00:00", types.LongText)
	dateCol := expression.NewGetField(0, types.Date, "d", true)
	datetimeCol := expression.NewGetField(1, types.Datetime, "dt", true)
	row := sql.Row{types.ZeroTime, types.ZeroTime}
	null := expression.NewLiteral(nil, types.Null)
	tz := func(s string) sql.Expression { return expression.NewLiteral(s, types.LongText) }
	week := func(args ...sql.Expression) sql.Expression {
		e, err := NewWeek(args...)
		require.NoError(t, err)
		return e
	}
	yearWeek := func(args ...sql.Expression) sql.Expression {
		e, err := NewYearWeek(args...)
		require.NoError(t, err)
		return e
	}
	weekUnit := expression.NewLiteral("WEEK", types.LongText)
	for _, c := range []struct {
		name    string
		e       sql.Expression
		want    interface{}
		warning string // 警告 1292 の文（空なら警告なし）
	}{
		{"to_days string", NewToDays(zeroStr), nil, "Incorrect datetime value: '0000-00-00'"},
		{"to_days datetime string", NewToDays(zeroDatetimeStr), nil, "Incorrect datetime value: '0000-00-00 00:00:00'"},
		{"to_days date column", NewToDays(dateCol), nil, ""},
		{"to_days datetime column", NewToDays(datetimeCol), nil, ""},
		{"to_days 0", NewToDays(expression.NewLiteral(0, types.Int64)), nil, "Incorrect datetime value: '0'"},
		{"last_day string", NewLastDay(zeroStr), nil, "Incorrect datetime value: '0000-00-00'"},
		{"last_day with a time", NewLastDay(tz("0000-00-00 10:00:00")), nil, "Incorrect datetime value: '0000-00-00'"},
		{"last_day date column", NewLastDay(dateCol), nil, "Incorrect datetime value: '0000-00-00'"},
		{"last_day datetime column", NewLastDay(datetimeCol), nil, "Incorrect datetime value: '0000-00-00'"},
		{"extract week string", NewExtract(weekUnit, zeroStr), 613566757, ""},
		{"extract week column", NewExtract(weekUnit, datetimeCol), 613566757, ""},
		{"convert_tz string", NewConvertTz(zeroDatetimeStr, tz("+00:00"), tz("+09:00")), nil, "Incorrect datetime value: '0000-00-00 00:00:00'"},
		{"convert_tz column", NewConvertTz(datetimeCol, tz("+00:00"), tz("+09:00")), nil, ""},
		{"week null", week(null), nil, ""},
		{"week null mode", week(null, expression.NewLiteral(3, types.Int64)), nil, ""},
		{"yearweek null", yearWeek(null), nil, ""},
		{"week string", week(zeroStr), nil, "Incorrect datetime value: '0000-00-00'"},
		{"week column", week(dateCol), nil, ""},
		{"yearweek column", yearWeek(datetimeCol, expression.NewLiteral(1, types.Int64)), nil, ""},
		{"dayofweek string", NewDayOfWeek(zeroStr), nil, "Incorrect datetime value: '0000-00-00'"},
		{"dayofweek column", NewDayOfWeek(dateCol), nil, ""},
		{"dayname column", NewDayName(datetimeCol), nil, ""},
		{"dayname null", NewDayName(null), nil, ""},
		{"monthname string", NewMonthName(zeroStr), nil, ""},
		{"weekofyear string", NewWeekOfYear(zeroStr), nil, "Incorrect datetime value: '0000-00-00'"},
		{"weekofyear column", NewWeekOfYear(datetimeCol), nil, ""},
		// ゼロでない日付と、本物の '0000-01-01' の文字列は前と同じ（MySQL も TO_DAYS は 1、LAST_DAY は 0000-01-31、EXTRACT(WEEK) は 1）
		{"to_days 0000-01-01", NewToDays(tz("0000-01-01")), 1, ""},
		{"last_day 0000-01-15", NewLastDay(tz("0000-01-15")), time.Date(0, 1, 31, 0, 0, 0, 0, time.UTC), ""},
		{"extract week 0000-01-01", NewExtract(weekUnit, tz("0000-01-01")), 1, ""},
		{"to_days 2024", NewToDays(tz("2024-02-10")), 739291, ""},
		{"last_day 2024", NewLastDay(tz("2024-02-10")), time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), ""},
	} {
		ctx := sql.NewEmptyContext()
		got, err := c.e.Eval(ctx, row)
		assert.NoError(t, err, c.name)
		assert.Equal(t, c.want, got, c.name)
		var msgs []string
		for _, w := range ctx.Session.Warnings() {
			msgs = append(msgs, w.Message)
		}
		if c.warning == "" {
			assert.Empty(t, msgs, c.name)
		} else {
			assert.Equal(t, []string{c.warning}, msgs, c.name)
		}
	}
}
