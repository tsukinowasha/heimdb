package backend

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 危ない変更の見張り（docs/plan.md の段階 33）。
//
// WordPress が乗っ取られるときに変わる行（管理者の追加、default_role、siteurl、プラグインの有効化など）が変わったら、
// ログと heimdb_status に出す。止めはしない。エディタが見張るテーブルの行を書くたびにトランザクションに印を残し
// （store.Txn.Note。文の取り消しやセーブポイントへのロールバックで印も消える）、コミットの後に、行ごとに最初の
// 前の値と最後の後の値を比べる。テーブルは名前の終わりと列の名前で見分ける（接頭辞を問わない）。

// watchKind は、見張るテーブルの種類。
type watchKind int

const (
	watchNone watchKind = iota
	watchOptions
	watchUsers
	watchUsermeta
	watchSitemeta
)

// watchTables は、見張るテーブルの名前の終わりと、持っているはずの列。
var watchTables = []struct {
	suffix string
	kind   watchKind
	cols   []string
}{
	{"usermeta", watchUsermeta, []string{"user_id", "meta_key", "meta_value"}},
	{"sitemeta", watchSitemeta, []string{"meta_key", "meta_value"}},
	{"options", watchOptions, []string{"option_name", "option_value"}},
	{"users", watchUsers, []string{"id", "user_login", "user_pass", "user_email"}},
}

// watchedOptions は、値の前と後をログに出すオプション。active_plugins と *user_roles は別に扱う。
var watchedOptions = map[string]bool{
	"siteurl": true, "home": true, "admin_email": true, "users_can_register": true,
	"default_role": true, "template": true, "stylesheet": true,
}

// watchTable は、テーブル t の見張りの種類と、見張りに使う列の位置（小文字の名前から）を返す。
func watchTable(t *Table) (watchKind, map[string]int) {
	if t.db.temporary {
		return watchNone, nil
	}
	return watchDef(t.def)
}

// watchDef は、テーブル定義 def の見張りの種類と、見張りに使う列の位置を返す。
func watchDef(def *schema.Table) (watchKind, map[string]int) {
	name := strings.ToLower(def.Name)
	for _, w := range watchTables {
		if !strings.HasSuffix(name, w.suffix) {
			continue
		}
		cols := map[string]int{}
		for i, c := range def.Columns {
			cols[strings.ToLower(c.Name)] = i
		}
		for _, c := range w.cols {
			if _, ok := cols[c]; !ok {
				return watchNone, nil
			}
		}
		return w.kind, cols
	}
	return watchNone, nil
}

// watchDescribe は、テーブル def の行が old から new に変わったことが見張りの対象なら、その説明を返す（変更の一覧と
// 復元の警告、docs/plan.md の段階 34）。
func watchDescribe(def *schema.Table, old, new sql.Row) []string {
	kind, cols := watchDef(def)
	if kind == watchNone {
		return nil
	}
	n := &watchNote{table: def.Name, kind: kind, cols: cols, old: old, new: new}
	return n.changes()
}

// watchNote は、見張るテーブルの 1 行の変更（store.Txn に添える印）。挿入では old、削除では new が nil。
type watchNote struct {
	db    string
	table string
	kind  watchKind
	cols  map[string]int
	pk    string
	old   sql.Row
	new   sql.Row
}

// watch は、見張るテーブルの見張る行の変更なら、位置 mark からの書き込みに印を添える。テーブルの種類は、エディタ
// （文ごとに作られる）で最初に 1 回だけ調べる。
func (e *editor) watch(txn *store.Txn, mark int, pk []byte, old, new sql.Row) {
	if e.noWatch {
		return
	}
	if !e.watchChecked {
		e.watchKind, e.watchCols = watchTable(e.t)
		e.watchChecked = true
	}
	kind, cols := e.watchKind, e.watchCols
	if kind == watchNone {
		return
	}
	// 見張る行だけに印を付ける（一時データやセッションの情報などの書き込みでは、行を持ち続けない）。
	var watched func(string) bool
	var col string
	switch kind {
	case watchOptions:
		watched, col = watchedOption, "option_name"
	case watchUsermeta:
		watched, col = func(k string) bool { return strings.HasSuffix(k, "capabilities") }, "meta_key"
	case watchSitemeta:
		watched, col = watchedSitemeta, "meta_key"
	}
	if watched != nil && !watched(watchString(old, cols, col)) && !watched(watchString(new, cols, col)) {
		return
	}
	txn.Note(mark, &watchNote{db: e.t.db.name, table: e.t.def.Name, kind: kind, cols: cols, pk: string(pk), old: old, new: new})
}

func watchedOption(name string) bool {
	return watchedOptions[name] || name == "active_plugins" || strings.HasSuffix(name, "user_roles")
}

func watchedSitemeta(key string) bool {
	return key == "site_admins" || key == "active_sitewide_plugins"
}

// watchString は、行 row の列 col の値を文字列で返す。行がなければ空。
func watchString(row sql.Row, cols map[string]int, col string) string {
	if row == nil {
		return ""
	}
	v, err := sql.UnwrapAny(context.Background(), row[cols[col]])
	if err != nil || v == nil {
		return ""
	}
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return fmt.Sprint(v)
}

// watcher は、見張りで見つけた変更の数と最後の 1 件（heimdb_status に出す）。
type watcher struct {
	mu     sync.Mutex
	count  int64
	last   string
	lastAt time.Time
}

// commitWatched は、トランザクション tx をコミットし、見張る行の変更をログに出す。via は、セッションのコミット
// 以外の経路（マージ）の説明。
func (p *Provider) commitWatched(sess sql.Session, tx *store.Txn, via string) error {
	notes := tx.Notes()
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(notes) > 0 {
		who := "?"
		if sess != nil {
			c := sess.Client()
			who = c.User + "@" + c.Address
			if s, ok := sess.(*Session); ok && s.remoteHost != "" {
				who = c.User + "@" + s.remoteHost
			}
		}
		p.watch.report(who, notes, via)
	}
	return nil
}

// report は、印 notes を行ごとにまとめ、変わったものを、つないだユーザー who とともにログに出して数える。
func (w *watcher) report(who string, notes []any, via string) {
	type rowKey struct{ db, table, pk string }
	var order []rowKey
	rows := map[rowKey]*watchNote{}
	for _, n := range notes {
		n, ok := n.(*watchNote)
		if !ok {
			continue
		}
		k := rowKey{n.db, strings.ToLower(n.table), n.pk}
		if r, ok := rows[k]; ok {
			r.new = n.new // 最初の前の値と、最後の後の値を比べる
			continue
		}
		c := *n
		rows[k] = &c
		order = append(order, k)
	}
	for _, k := range order {
		n := rows[k]
		for _, msg := range n.changes() {
			line := fmt.Sprintf("%s on %s: %s", who, n.db, msg)
			if via != "" {
				line += " (" + via + ")"
			}
			logrus.Warnf("HeimDB: watch: %s", line)
			w.mu.Lock()
			w.count++
			w.last, w.lastAt = line, time.Now()
			w.mu.Unlock()
		}
	}
}

// changes は、行の変更を説明する文を返す。見張る変更がなければ空。
func (n *watchNote) changes() []string {
	if n.old == nil && n.new == nil {
		return nil // 同じトランザクションで足して消した行
	}
	get := func(row sql.Row, col string) string { return watchString(row, n.cols, col) }
	switch n.kind {
	case watchOptions:
		name := get(n.new, "option_name")
		if n.new == nil {
			name = get(n.old, "option_name")
		}
		before, after := get(n.old, "option_value"), get(n.new, "option_value")
		if n.old != nil && n.new != nil && before == after {
			return nil
		}
		switch {
		case name == "active_plugins":
			return pluginChanges("plugins", before, after)
		case strings.HasSuffix(name, "user_roles"):
			return []string{fmt.Sprintf("option %s (role definitions) %s", watchName(name), rowVerb(n.old, n.new))}
		case watchedOptions[name]:
			return []string{fmt.Sprintf("option %s: %s -> %s", name, watchValue(n.old, before), watchValue(n.new, after))}
		}
	case watchUsers:
		id, login := get(n.new, "id"), get(n.new, "user_login")
		if n.new == nil {
			id, login = get(n.old, "id"), get(n.old, "user_login")
		}
		user := fmt.Sprintf("%s (ID %s)", strconv.Quote(login), id)
		switch {
		case n.old == nil:
			return []string{"user added: " + user}
		case n.new == nil:
			return []string{"user deleted: " + user}
		}
		var out []string
		if get(n.old, "user_pass") != get(n.new, "user_pass") {
			out = append(out, "password changed: "+user)
		}
		if b, a := get(n.old, "user_email"), get(n.new, "user_email"); b != a {
			out = append(out, fmt.Sprintf("email changed: %s: %s -> %s", user, strconv.Quote(b), strconv.Quote(a)))
		}
		return out
	case watchUsermeta:
		key := get(n.new, "meta_key")
		if n.new == nil {
			key = get(n.old, "meta_key")
		}
		if !strings.HasSuffix(key, "capabilities") {
			return nil
		}
		if hasAdministrator(get(n.new, "meta_value")) && !hasAdministrator(get(n.old, "meta_value")) {
			return []string{fmt.Sprintf("administrator role given: user ID %s (%s)", watchName(get(n.new, "user_id")), watchName(key))}
		}
	case watchSitemeta:
		key := get(n.new, "meta_key")
		if n.new == nil {
			key = get(n.old, "meta_key")
		}
		before, after := get(n.old, "meta_value"), get(n.new, "meta_value")
		if n.old != nil && n.new != nil && before == after {
			return nil
		}
		switch key {
		case "active_sitewide_plugins":
			return pluginChanges("network plugins", before, after)
		case "site_admins":
		default:
			return nil
		}
		return []string{fmt.Sprintf("network option site_admins (super admins): %s -> %s", watchValue(n.old, strings.Join(phpStrings(before), ", ")), watchValue(n.new, strings.Join(phpStrings(after), ", ")))}
	}
	return nil
}

// pluginChanges は、有効なプラグインの一覧（PHP で直列化した配列）が before から after に変わったことを説明する。
// active_sitewide_plugins はファイル名を配列のキーに持つが、同じく文字列として取り出せる。
func pluginChanges(what, before, after string) []string {
	added, removed := diffStrings(phpStrings(before), phpStrings(after))
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	msg := what
	if len(added) > 0 {
		msg += " activated: " + watchNames(added)
		if len(removed) > 0 {
			msg += ";"
		}
	}
	if len(removed) > 0 {
		msg += " deactivated: " + watchNames(removed)
	}
	return []string{msg}
}

// watchName は、ログに出す名前（オプション名、メタのキー、プラグインのファイル名）。空白や制御文字を含む名前は
// 引用符で囲んでエスケープし、ログの行を偽れないようにする。
func watchName(s string) string {
	for _, r := range s {
		if r <= ' ' || r > '~' || r == '"' || r == '\\' {
			return strconv.Quote(s)
		}
	}
	if s == "" {
		return `""`
	}
	return s
}

func watchNames(names []string) string {
	out := make([]string, len(names))
	for i, s := range names {
		out[i] = watchName(s)
	}
	return strings.Join(out, ", ")
}

func rowVerb(old, new sql.Row) string {
	switch {
	case old == nil:
		return "added"
	case new == nil:
		return "deleted"
	}
	return "changed"
}

// watchValue は、ログに出す値。行がなければ (none)。長い値は切る。
func watchValue(row sql.Row, v string) string {
	if row == nil {
		return "(none)"
	}
	const max = 200
	if len(v) > max {
		return strconv.Quote(v[:max]) + "..."
	}
	return strconv.Quote(v)
}

// administratorPattern は、PHP で直列化した役割の配列（a:1:{s:13:"administrator";b:1;}）の administrator。
var administratorPattern = regexp.MustCompile(`s:13:"administrator";b:1;`)

func hasAdministrator(v string) bool { return administratorPattern.MatchString(v) }

// phpStringPattern は、PHP で直列化した文字列（s:長さ:"値";）。
var phpStringPattern = regexp.MustCompile(`s:\d+:"([^"]*)";`)

// phpStrings は、PHP で直列化した配列に含まれる文字列を返す（active_plugins はプラグインのファイル名の配列）。
func phpStrings(v string) []string {
	var out []string
	for _, m := range phpStringPattern.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1])
	}
	return out
}

// diffStrings は、before になく after にあるものと、before にあり after にないものを返す。
func diffStrings(before, after []string) (added, removed []string) {
	for _, s := range after {
		if !slices.Contains(before, s) {
			added = append(added, s)
		}
	}
	for _, s := range before {
		if !slices.Contains(after, s) {
			removed = append(removed, s)
		}
	}
	return added, removed
}
