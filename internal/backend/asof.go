package backend

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

var _ sql.VersionedDatabase = (*Database)(nil)

// GetTableInsensitiveAsOf は sql.VersionedDatabase の実装。時刻 asOf の時点のテーブル定義と行を見せる。
func (d *Database) GetTableInsensitiveAsOf(ctx *sql.Context, name string, asOf interface{}) (sql.Table, bool, error) {
	ts, err := d.asOfTS(ctx, asOf)
	if err != nil {
		return nil, false, err
	}
	if err := d.pinAsOf(ctx, ts); err != nil {
		return nil, false, err
	}
	def, ok, err := d.st.GetTable(d.branch, name, ts)
	if err != nil || !ok {
		return nil, false, err
	}
	t, err := newTable(ctx, d, def)
	if err != nil {
		return nil, false, err
	}
	t.asOf = ts
	return t, true, nil
}

// asOfTS は AS OF の時刻を、最後のコミット（ほかの接続に見せた最後の時刻）までにして返す。同期のレプリカの知らせを
// 待っているコミット（docs/plan.md の段階 25）は、Pebble にあっても今や未来の時刻で読めないようにする。
func (d *Database) asOfTS(ctx *sql.Context, asOf interface{}) (uint64, error) {
	ts, err := parseAsOf(ctx, asOf)
	if err != nil {
		return 0, err
	}
	return min(ts, d.st.LastCommitted()), nil
}

// pinAsOf は、文が時刻 ts を読み終わるまで GC がその時刻のバージョンを消さないようにする（Session.pinAsOf）。境界より前ならエラー。
func (d *Database) pinAsOf(ctx *sql.Context, ts uint64) error {
	if s, ok := ctx.Session.(*Session); ok {
		return s.pinAsOf(ts)
	}
	return d.st.CheckReadable(ts)
}

// GetTableNamesAsOf は sql.VersionedDatabase の実装。
func (d *Database) GetTableNamesAsOf(ctx *sql.Context, asOf interface{}) ([]string, error) {
	ts, err := d.asOfTS(ctx, asOf)
	if err != nil {
		return nil, err
	}
	if err := d.pinAsOf(ctx, ts); err != nil {
		return nil, err
	}
	defs, err := d.st.ListTables(d.branch, ts)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(defs))
	for i, def := range defs {
		names[i] = def.Name
	}
	return names, nil
}

// asOfOffsetLayouts は、UTC からのオフセット付きの日時の書式。
var asOfOffsetLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
}

// asOfLayouts は AS OF に文字列で渡せる日時の書式。
var asOfLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02",
}

// parseAsOf は AS OF の値を内部の時刻（UTC のマイクロ秒）に変換する。
//
// go-mysql-server は文字列リテラルを文字列のまま、TIMESTAMP(...) や NOW() を「セッションのタイムゾーンでの
// 日時を UTC と称した time.Time」として渡す。どちらも日時の各欄を、セッションの time_zone で読み直す。
func parseAsOf(ctx *sql.Context, asOf interface{}) (uint64, error) {
	var wall time.Time
	switch v := asOf.(type) {
	case time.Time:
		wall = v
	case string:
		// オフセット付き（'2026-11-01 06:30:00.000000+00:00'、復元の取り消しの時刻）なら、その時刻そのもの。
		for _, layout := range asOfOffsetLayouts {
			if t, err := time.Parse(layout, v); err == nil {
				return checkAsOfTime(t)
			}
		}
		var err error
		for _, layout := range asOfLayouts {
			if wall, err = time.Parse(layout, v); err == nil {
				break
			}
		}
		if err != nil {
			return 0, fmt.Errorf("HeimDB: invalid AS OF time %q (use 'YYYY-MM-DD HH:MM:SS')", v)
		}
	case nil:
		return 0, fmt.Errorf("HeimDB: AS OF requires a time literal (placeholders are not supported)")
	default:
		return 0, fmt.Errorf("HeimDB: invalid AS OF value of type %T", asOf)
	}

	loc, err := sessionLocation(ctx)
	if err != nil {
		return 0, err
	}
	return checkAsOfTime(time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), loc))
}

// checkAsOfTime は時刻を内部の時刻（UTC のマイクロ秒）にする。
func checkAsOfTime(t time.Time) (uint64, error) {
	us := t.UnixMicro()
	if us < 0 {
		return 0, fmt.Errorf("HeimDB: AS OF time %s is before 1970-01-01 UTC", t)
	}
	if uint64(us) >= store.Latest {
		return store.Latest - 1, nil
	}
	return uint64(us), nil
}

var offsetPattern = regexp.MustCompile(`^([+-])(\d{1,2}):(\d{2})$`)

// sessionLocation はセッションの time_zone をタイムゾーンに変換する。
// SYSTEM はサーバの地域時刻、"+09:00" の形式は固定のオフセット、それ以外はタイムゾーン名として読む。
func sessionLocation(ctx *sql.Context) (*time.Location, error) {
	v, err := ctx.GetSessionVariable(ctx, "time_zone")
	if err != nil {
		return nil, err
	}
	name, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("HeimDB: invalid @@time_zone value %v", v)
	}
	if name == "SYSTEM" {
		return time.Local, nil
	}
	if m := offsetPattern.FindStringSubmatch(name); m != nil {
		h, _ := strconv.Atoi(m[2])
		min, _ := strconv.Atoi(m[3])
		secs := h*3600 + min*60
		if m[1] == "-" {
			secs = -secs
		}
		return time.FixedZone(name, secs), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("HeimDB: unknown time zone %q", name)
	}
	return loc, nil
}
