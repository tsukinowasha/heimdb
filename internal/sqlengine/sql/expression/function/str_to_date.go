// Modified by Tsukinowa Inc.: go vet の書式の文字列の確認に合わせた（docs/engine.md）
// Modified by Tsukinowa Inc.: 書式と同じ指定子のない文字列をすべて 0 の日付にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: ゼロの日付を、NO_ZERO_DATE のない sql_mode ではゼロの日付として返す（上流の #3305 の問題。docs/engine.md）
// Modified by Tsukinowa Inc.: NULL の引数は NULL、文字列でない引数は文字列にして読み、空の文字列（指定子のない書式でも）をゼロの日付、%f の型を DATETIME(6)、指定子のない書式の型を DATE にし、警告の文を MySQL と同じにする（上流の #3761。docs/engine.md）

package function

import (
	"fmt"
	"strings"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/planbuilder/dateparse"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// StrToDate defines the built-in function STR_TO_DATE(str, format)
type StrToDate struct {
	Date   sql.Expression
	Format sql.Expression
}

var _ sql.FunctionExpression = (*StrToDate)(nil)
var _ sql.CollationCoercible = (*StrToDate)(nil)

// NewStrToDate constructs a new function expression from the given child expressions.
func NewStrToDate(arg1, arg2 sql.Expression) sql.Expression {
	return &StrToDate{
		Date:   arg1,
		Format: arg2,
	}
}

func (s *StrToDate) FunctionName() string {
	return "str_to_date"
}

// Description implements sql.FunctionExpression
func (s *StrToDate) Description() string {
	return "parses the date/datetime/timestamp expression according to the format specifier."
}

// Resolved returns whether the node is resolved.
func (s *StrToDate) Resolved() bool {
	dateResolved := s.Date == nil || s.Date.Resolved()
	formatResolved := s.Format == nil || s.Format.Resolved()
	return dateResolved && formatResolved
}

func (s *StrToDate) String() string {
	return fmt.Sprintf("%s(%s,%s)", s.FunctionName(), s.Date, s.Format)
}

// Type returns the expression type.
func (s *StrToDate) Type() sql.Type {
	// TODO: depending on the format, the return type can be a date, datetime or timestamp
	//  just make best guess for now
	formatLit, isLit := s.Format.(*expression.Literal)
	if !isLit {
		return types.Datetime
	}
	format, err := formatLit.Eval(nil, nil)
	if err != nil {
		return types.Datetime
	}
	formatStr, isStr := format.(string)
	if !isStr {
		return types.Datetime
	}

	hasDate, hasTime, err := dateparse.HasDateOrTime(formatStr)
	if err != nil {
		return types.Datetime
	}
	if hasDate && hasTime {
		// %f があれば、MySQL と同じく DATETIME(6)（小数の桁を 6 つ表示する）
		if strings.Contains(formatStr, "%f") {
			return strToDateDatetime6
		}
		return types.Datetime
	}
	if hasDate {
		return types.Date
	}
	if hasTime {
		return types.Time
	}
	// 指定子のない書式は、MySQL と同じく DATE（段階 114 のレビュー）
	return types.Date
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*StrToDate) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// IsNullable returns whether the expression can be null.
func (s *StrToDate) IsNullable() bool {
	return true
}

// Children returns the children expressions of this expression.
func (s *StrToDate) Children() []sql.Expression {
	return []sql.Expression{s.Date, s.Format}
}

func (s *StrToDate) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidArgumentNumber.New("STR_TO_DATE", 2, len(children))
	}
	return NewStrToDate(children[0], children[1]), nil
}

// Eval evaluates the given row and returns a result.
func (s *StrToDate) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	date, err := s.Date.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	// NULL の引数は NULL（上流の #3761。前は型の確かめで 1105）
	if date == nil {
		return nil, nil
	}
	format, err := s.Format.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if format == nil {
		return nil, nil
	}

	// 文字列でない引数（日時の列、数）は、MySQL と同じく文字列にして読む（段階 113 で回した 1105）
	dateStr, ok := strToDateArg(ctx, s.Date.Type(), date)
	if !ok {
		// TODO: improve this error
		return nil, sql.ErrInvalidType.New(fmt.Sprintf("%T", date))
	}
	formatStr, ok := strToDateArg(ctx, s.Format.Type(), format)
	if !ok {
		// TODO: improve this error
		return nil, sql.ErrInvalidType.New(fmt.Sprintf("%T", format))
	}

	if isZeroDateString(dateStr, formatStr) || blankDateString(dateStr, formatStr) || literalDateString(dateStr, formatStr) {
		// ゼロの日付（'0000-00-00'、'0000-00-00 00:00:00'）は、NO_ZERO_DATE のない sql_mode（WordPress）では MySQL と
		// 同じくゼロの日付を返し、NO_ZERO_DATE があれば NULL（上流の #3305 の問題。DATE_FORMAT(post_date_gmt, ...) を
		// 読み戻す形）。dateparse は 0 月 0 日を読めないか、%c と %e では -0001-11-30 に読んで 1105 になるので先に見る
		if sql.LoadSqlMode(ctx).ModeEnabled("NO_ZERO_DATE") {
			ctx.Warn(1411, "%s", fmt.Sprintf("Incorrect datetime value: '%s' for function %s", dateStr, s.FunctionName()))
			return nil, nil
		}
		return types.ZeroTime, nil
	}

	goTime, err := dateparse.ParseDateWithFormat(dateStr, formatStr)
	if err != nil {
		ctx.Warn(1411, "%s", fmt.Sprintf("Incorrect datetime value: '%s' for function %s", dateStr, s.FunctionName()))
		return nil, nil
	}

	// zero dates '0000-00-00' and '2010-00-13' are allowed,
	// but depends on strict sql_mode with NO_ZERO_DATE or NO_ZERO_IN_DATE modes enabled.
	return goTime, nil
}

// strToDateDatetime6 は、%f のある書式の STR_TO_DATE の型（DATETIME(6)）。
var strToDateDatetime6 = types.MustCreateDatetimeType(sqltypes.Datetime, 6)

// strToDateArg は、STR_TO_DATE の引数の値 v を文字列にする。文字列とバイト列はそのまま、ほかは型 typ の表示の形
// （日時の列は '2024-01-02 03:04:05'、DATE は '2024-01-02'、数は '20240102'）。
func strToDateArg(ctx *sql.Context, typ sql.Type, v interface{}) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	if typ != nil {
		if sv, err := typ.SQL(ctx, nil, v); err == nil {
			return sv.ToString(), true
		}
	}
	s, _, err := types.LongText.Convert(ctx, v)
	if err != nil {
		return "", false
	}
	str, ok := s.(string)
	return str, ok
}

// literalDateString は、指定子のない書式 format と同じ文字列 s か（STR_TO_DATE('x', 'x')。MySQL はすべて 0 の日付と
// みなす。段階 116）。
func literalDateString(s, format string) bool {
	return format != "" && !strings.Contains(format, "%") && s == format
}

// blankDateString は、空か空白だけの s を、日付の指定子のある書式か指定子のない書式で読むか（MySQL はすべて 0 の日付と
// みなす）。
func blankDateString(s, format string) bool {
	if strings.TrimSpace(s) != "" {
		return false
	}
	hasDate, hasTime, err := dateparse.HasDateOrTime(format)
	// 指定子のない書式（''、'x'）も、MySQL と同じくすべて 0 の日付（段階 114 のレビュー。前のバージョンは -0001-11-30 で 1105）
	return err == nil && (hasDate || !hasTime)
}

// isZeroDateString は、s を書式 format で読むと、年、月、日（と時刻）がすべて 0 になるか。MySQL の STR_TO_DATE と同じく
// 書式の順に読み、文字列が先に尽きたらそこまでで、書式が先に尽きたら残りは見ない。%Y は 4 桁まで読み、2 桁以下は
// 2 桁の年（00 は 2000 年）なのでゼロの日付にならない。%y も同じ（STR_TO_DATE('000000', '%y%m%d') は MySQL でも
// NULL）。名前の指定子（%M %b %W %a %p）や 12 時間の時（%h %I %l）など、0 にならないか扱わない指定子があれば偽。
// 日付の指定子（%Y %m %c %d %e）を 1 つも読まなければ偽（時刻だけの書式は dateparse で読む）。
func isZeroDateString(s, format string) bool {
	read := func(max int) (int, bool) {
		n := 0
		for n < max && n < len(s) && s[n] >= '0' && s[n] <= '9' {
			if s[n] != '0' {
				return 0, false
			}
			n++
		}
		s = s[n:]
		return n, n > 0
	}
	dateFields := 0
	for i := 0; i < len(format) && len(s) > 0; i++ {
		c := format[i]
		if c != '%' || i+1 >= len(format) {
			if s[0] != c {
				return false
			}
			s = s[1:]
			continue
		}
		i++
		switch format[i] {
		case 'Y':
			if n, ok := read(4); !ok || n <= 2 {
				return false
			}
			dateFields++
		case 'm', 'c', 'd', 'e':
			if _, ok := read(2); !ok {
				return false
			}
			dateFields++
		case 'H', 'k', 'i', 's', 'S':
			if _, ok := read(2); !ok {
				return false
			}
		case 'f':
			if _, ok := read(6); !ok {
				return false
			}
		case '%':
			if s[0] != '%' {
				return false
			}
			s = s[1:]
			continue
		default:
			return false
		}
	}
	return dateFields > 0
}
