package backend

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/schema"
)

// 変更の一覧（docs/plan.md の段階 34）。
//
//	CALL heimdb_changes('main', '2026-10-12 03:00:00');                 -- すべてのテーブル
//	CALL heimdb_changes('main', '2026-10-12 03:00:00', 'wp_users,wp_usermeta');
//
// 時刻から今までに変わった行を、行が最後に変わった時刻の順に返す。何も書かない。見張り（watch.go）の対象の変化には
// その説明を detail に入れる。毎分のように変わる行は除き、その数を最後の行に出す。

var changesSchema = sql.Schema{
	{Name: "table_name", Type: types.LongText},
	{Name: "id", Type: types.LongText},
	{Name: "change", Type: types.LongText},
	{Name: "columns", Type: types.LongText, Nullable: true},
	{Name: "changed_at", Type: types.LongText, Nullable: true},
	{Name: "detail", Type: types.LongText, Nullable: true},
}

// テーブルごとの変化。
const (
	changeTableCreated = "table created"
	changeTableDropped = "table dropped"
	changeTableChanged = "table changed"
	// changeDefinitionChanged は、テーブル ID は同じまま定義が変わったこと（インデックス、コメント、既定値、列の名前）。
	// 行の形は同じなので、行は続けて比べる（docs/plan.md の段階 43）。
	changeDefinitionChanged = "definition changed"
)

type changeRow struct {
	ts  uint64
	row sql.Row
}

func (p *Provider) changes(ctx *sql.Context, branchName, at, tablesArg string) (sql.RowIter, error) {
	b, ok := p.st.BranchByName(branchName)
	if !ok {
		return nil, fmt.Errorf("HeimDB: branch %q not found", branchName)
	}
	ts, err := parseAsOf(ctx, at)
	if err != nil {
		return nil, err
	}
	if ts > uint64(time.Now().UnixMicro()) {
		return nil, fmt.Errorf("HeimDB: time %s is in the future", at)
	}
	// 時刻 ts のバージョンを読み終えるまで、GC に消させない（保持期間の外ならエラー）。
	release, err := p.st.Pin(ts)
	if err != nil {
		return nil, err
	}
	defer release()
	now := p.st.LastCommitted()

	defsAt, err := p.st.ListTables(b.ID, ts)
	if err != nil {
		return nil, err
	}
	defsNow, err := p.st.ListTables(b.ID, now)
	if err != nil {
		return nil, err
	}
	atByName, nowByName := map[string]*schema.Table{}, map[string]*schema.Table{}
	names := map[string]bool{}
	for _, d := range defsAt {
		atByName[strings.ToLower(d.Name)] = d
		names[strings.ToLower(d.Name)] = true
	}
	for _, d := range defsNow {
		nowByName[strings.ToLower(d.Name)] = d
		names[strings.ToLower(d.Name)] = true
	}
	if strings.TrimSpace(tablesArg) == "" {
		tablesArg = "*"
	}
	selected, err := p.restoreSelection(b.ID, tablesArg, names)
	if err != nil {
		return nil, err
	}

	var out []changeRow
	routine := 0
	d := &differ{st: p.st, ctx: ctx}
	for name := range selected {
		def0, def1 := atByName[name], nowByName[name]
		switch {
		case def0 == nil && def1 == nil:
			continue
		case def0 == nil:
			out = append(out, changeRow{def1.Version, sql.Row{def1.Name, "*", changeTableCreated, nil, formatRestoreTime(def1.Version), nil}})
			continue
		case def1 == nil:
			// 消した時刻は定義に残らないので、一覧の最後に置く。
			out = append(out, changeRow{now, sql.Row{def0.Name, "*", changeTableDropped, nil, nil, nil}})
			continue
		case def0.ID != def1.ID:
			// 列の変更や TRUNCATE で書き直したテーブル（新しいテーブル ID）は、行を比べず 1 行にする（行の形が違う）。
			out = append(out, changeRow{def1.Version, sql.Row{def1.Name, "*", changeTableChanged, nil, formatRestoreTime(def1.Version), nil}})
			continue
		case !sameDefinition(def0, def1):
			// テーブル ID が同じなら行の形は同じで、どちらの定義でも読める（列を足す、型を変えるなどは書き直しになる）。
			out = append(out, changeRow{def1.Version, sql.Row{def1.Name, "*", changeDefinitionChanged, nil, formatRestoreTime(def1.Version), nil}})
		}
		def := def1
		rows, err := p.st.ChangedRowTimesSince(b.ID, def.ID, ts, now)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			old, err := d.getRow(b.ID, def, ts, r.PK)
			if err != nil {
				return nil, err
			}
			cur, err := d.getRow(b.ID, def, now, r.PK)
			if err != nil {
				return nil, err
			}
			change, cols := rowChangeOf(def, old, cur)
			if change == "" {
				continue
			}
			if routineRow(def, old) || routineRow(def, cur) {
				routine++
				continue
			}
			var colsV, detail any
			if cols != "" {
				colsV = cols
			}
			if w := watchDescribe(def, old, cur); len(w) > 0 {
				detail = strings.Join(w, "; ")
			}
			out = append(out, changeRow{r.TS, sql.Row{def.Name, changeID(def, old, cur), change, colsV, formatRestoreTime(r.TS), detail}})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ts != out[j].ts {
			return out[i].ts < out[j].ts
		}
		if a, b := out[i].row[0].(string), out[j].row[0].(string); a != b {
			return a < b
		}
		return out[i].row[1].(string) < out[j].row[1].(string)
	})
	rows := make([]sql.Row, 0, len(out)+1)
	for _, r := range out {
		rows = append(rows, r.row)
	}
	if routine > 0 {
		rows = append(rows, sql.Row{"*", "*", "not shown", nil, nil, fmt.Sprintf("%d rows that change routinely (transients, cron, edit locks)", routine)})
	}
	return &rowIter{rows: rows}, nil
}

// rowChangeOf は、行が old から cur に変わったことを insert、update、delete と、更新なら変わった列（カンマ区切り）で返す。
// 変わっていなければ空。
func rowChangeOf(def *schema.Table, old, cur sql.Row) (change, cols string) {
	switch {
	case old == nil && cur == nil:
		return "", ""
	case old == nil:
		return "insert", ""
	case cur == nil:
		return "delete", ""
	}
	var names []string
	for i, c := range def.Columns {
		if !merge.Equal(old[i], cur[i]) {
			names = append(names, c.Name)
		}
	}
	if len(names) == 0 {
		return "", ""
	}
	return "update", strings.Join(names, ",")
}

// changeID は、行の名前（heimdb_branch_diff と同じ書き方。オプションは option_name）。
func changeID(def *schema.Table, old, cur sql.Row) string {
	row := cur
	if row == nil {
		row = old
	}
	if kind, cols := watchDef(def); kind == watchOptions {
		return "option_name=" + watchString(row, cols, "option_name")
	}
	return pkString(def, def, cur, old)
}

// routineRow は、毎分のように変わる行（一時データ、cron の予定、記事の編集ロック）かどうか。一覧から除く。
func routineRow(def *schema.Table, row sql.Row) bool {
	if row == nil {
		return false
	}
	m := toMergeRow(def, row)
	name := strings.ToLower(def.Name)
	switch {
	case strings.HasSuffix(name, "options"):
		n, _ := m["option_name"].(string)
		return strings.HasPrefix(n, "_transient_") || strings.HasPrefix(n, "_site_transient_") || n == "cron"
	case strings.HasSuffix(name, "sitemeta"):
		k, _ := m["meta_key"].(string)
		return strings.HasPrefix(k, "_site_transient_")
	case strings.HasSuffix(name, "postmeta"):
		k, _ := m["meta_key"].(string)
		return k == "_edit_lock"
	}
	return false
}
