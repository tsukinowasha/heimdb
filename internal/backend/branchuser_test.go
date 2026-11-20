package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// startBranchUserServer は、認証を有効にしたサーバをデータディレクトリ dir で起動する。branchProcs は -branch-procedures。
func startBranchUserServer(t *testing.T, dir string, branchProcs bool) (addr string, pro *Provider, stop func()) {
	t.Helper()
	addr, pro, st, srv := serve(t, dir, serverConfig{auth: true, rootPassword: "rootpw", provider: func(p *Provider) { p.BranchProcedures = branchProcs }})
	return addr, pro, stopServer(t, st, srv)
}

// connectOK は、connectAs で接続し、できなければテストを止める。
func connectOK(t *testing.T, addr, user, password, db string) *sql.DB {
	t.Helper()
	conn, err := connectAs(t, addr, user, password, db)
	if err != nil {
		t.Fatalf("%s to %s: %v", user, db, err)
	}
	conn.SetMaxOpenConns(1)
	return conn
}

// execErr は、q を実行し、エラーが want を含むことを確かめる。
func execErr(t *testing.T, db *sql.DB, q, want string) {
	t.Helper()
	if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: got %v, want %q", q, err, want)
	}
}

// -branch-procedures で、ブランチのユーザーが自分のブランチの差分、解決、変更の一覧、復元を呼べる。対象は接続から導き、
// 別のブランチと main は拒否する。マージなどは管理者だけのまま（docs/plan.md の段階 63）。
func TestBranchUserProcedures(t *testing.T) {
	dir := t.TempDir()
	stagingDB, stg2DB := testDB+"@staging", testDB+"@stg2"

	addr, _, stop := startBranchUserServer(t, dir, false)
	root := connectOK(t, addr, "root", "rootpw", testDB)
	seedWordPress(t, root)
	beforeFork := one(t, root, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f+00:00')")
	time.Sleep(10 * time.Millisecond)
	for _, q := range []string{
		insertPost + "(2, 0, 'second', 'publish', 'post', 'c', '', '', '', '')",
		"CALL heimdb_branch_create('staging')",
		"CALL heimdb_branch_create('stg2')",
		"CREATE USER 'stg'@'%' IDENTIFIED BY 'pw'",
		"GRANT ALL ON `" + stagingDB + "`.* TO 'stg'@'%'",
		"CREATE USER 'enum'@'%' IDENTIFIED BY 'pw'", // 権限を列挙して付けた（EXECUTE がない）
		"GRANT SELECT, INSERT, UPDATE, DELETE ON `" + stagingDB + "`.* TO 'enum'@'%'",
		"CREATE USER 'x'@'%' IDENTIFIED BY 'pw'", // staging は読むだけ、EXECUTE は stg2 にだけある
		"GRANT SELECT ON `" + stagingDB + "`.* TO 'x'@'%'",
		"GRANT EXECUTE ON `" + stg2DB + "`.* TO 'x'@'%'",
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'pw'", // 本番のサイトのユーザー
		"GRANT ALL ON `" + testDB + "`.* TO 'wp'@'%'",
	} {
		mustExec(t, root, q)
	}
	stg := connectOK(t, addr, "stg", "pw", stagingDB)
	execErr(t, stg, "CALL heimdb_branch_diff('')", "command denied") // フラグがなければ今のまま
	stg.Close()
	root.Close()
	stop()

	addr, pro, stop := startBranchUserServer(t, dir, true)
	defer stop()
	root = connectOK(t, addr, "root", "rootpw", testDB)
	defer root.Close()
	rootStg := connectOK(t, addr, "root", "rootpw", stagingDB)
	defer rootStg.Close()
	before := one(t, root, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f+00:00')")
	time.Sleep(10 * time.Millisecond)
	for _, q := range []string{
		"UPDATE wp_posts SET post_title = 'main title' WHERE ID = 1",
		"UPDATE wp_posts SET post_title = 'main second' WHERE ID = 2",
		"UPDATE wp_options SET option_value = 'Main' WHERE option_name = 'blogname'",
	} {
		mustExec(t, root, q)
	}
	for _, q := range []string{
		"UPDATE wp_posts SET post_title = 'staging title' WHERE ID = 1",
		"DELETE FROM wp_posts WHERE ID = 2",
		"UPDATE wp_options SET option_value = 'Staging' WHERE option_name = 'blogname'",
	} {
		mustExec(t, rootStg, q)
	}
	// 両側で同じスラッグのタームを足した（重複の競合。相手は親の別の主キーの行）。
	mustExec(t, root, "INSERT INTO wp_terms (name, slug) VALUES ('News main', 'news')")
	mustExec(t, rootStg, "INSERT INTO wp_terms (name, slug) VALUES ('News stg', 'news')")

	stg = connectOK(t, addr, "stg", "pw", stagingDB)
	defer stg.Close()
	// 対象は接続から導く。名前を書いてもよいが、接続中のブランチでなければ拒否する。
	if got := strings.Join(diffRows(t, stg, "CALL heimdb_branch_diff('')"), "\n"); !strings.Contains(got, "wp_posts|changed|0|0|0|0|2|0|0|") {
		t.Errorf("summary:\n%s", got)
	}
	if got := len(diffRows(t, stg, "CALL heimdb_branch_diff('staging', 'wp_posts')")); got != 2 {
		t.Errorf("named: %d rows", got)
	}
	for _, q := range []string{
		"CALL heimdb_branch_diff('stg2')",
		"CALL heimdb_branch_diff('main')",
		"CALL heimdb_changes('stg2', '" + before + "')",
		"CALL heimdb_restore('stg2', '" + before + "')",
		"CALL heimdb_branch_resolve('stg2', 'wp_posts', 'ID=1', 'branch')",
	} {
		execErr(t, stg, q, "only on the branch it is connected to")
	}
	for _, q := range []string{
		"CALL heimdb_branch_merge('staging')",
		"CALL heimdb_branch_merge('staging', 'dry_run')",
		"CALL heimdb_branch_limit('staging', '0', '0')",
		"CALL heimdb_branch_create('mine')",
		"CALL heimdb_branch_delete('stg2')",
		"CALL heimdb_merge_exclude('wp_x')",
	} {
		execErr(t, stg, q, "command denied")
	}

	// 競合した列の両側の値（ブランチのユーザーは親を読めない）。削除と変更の競合では、変更した側が変えた列。
	posts := diffRowsAll(t, stg, "CALL heimdb_branch_diff('', 'wp_posts')")
	if want := []string{
		`wp_posts|ID=1|update|update|conflict||post_title|the same columns were changed on both sides|||{"post_title":{"branch":"staging title","parent":"main title"}}`,
		`wp_posts|ID=2|delete|update|conflict|||deleted on the branch, changed on the parent|||{"post_title":{"branch":null,"parent":"main second"}}`,
	}; strings.Join(posts, "\n") != strings.Join(want, "\n") {
		t.Errorf("posts:\n%s", strings.Join(posts, "\n"))
	}
	if got := diffRowsAll(t, stg, "CALL heimdb_branch_diff('', 'wp_options')"); len(got) != 1 ||
		!strings.HasSuffix(got[0], `|||{"option_value":{"branch":"Staging","parent":"Main"}}`) {
		t.Errorf("options: %q", got)
	}
	if got := diffRowsAll(t, stg, "CALL heimdb_branch_diff('', 'wp_terms')"); len(got) != 1 ||
		!regexp.MustCompile(`\|conflict\|\|\|slug=news was also added on the parent as term_id=\d+\|\|\|\{"name":\{"branch":"News stg","parent":"News main"\},"term_id":\{"branch":"\d+","parent":"\d+"\}\}$`).MatchString(got[0]) {
		t.Errorf("terms: %q", got)
	}

	// ブランチのユーザーは、行を指定した解決だけ。記録したユーザーと時刻が差分に出て、ログにも出る。
	for _, q := range []string{
		"CALL heimdb_branch_resolve('', 'wp_posts', '*', 'branch')",
		"CALL heimdb_branch_resolve('', '*', '*', 'parent')",
		"CALL heimdb_branch_resolve('', '*', '*', 'none')",
	} {
		execErr(t, stg, q, "only an administrator can resolve with '*'")
	}
	hook := logtest.NewGlobal()
	mustExec(t, stg, "CALL heimdb_branch_resolve('', 'wp_posts', 'ID=1', 'branch')")
	found := false
	for _, e := range hook.AllEntries() {
		found = found || e.Message == "HeimDB: 'stg'@'%' resolved wp_posts ID=1 on branch staging: branch"
	}
	hook.Reset()
	if !found {
		t.Error("no log of the resolution")
	}
	resolvedAt := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{6}\+00:00$`)
	row := strings.Split(diffRowsAll(t, root, "CALL heimdb_branch_diff('staging', 'wp_posts')")[0], "|")
	if row[5] != "branch" || row[8] != "'stg'@'%'" || !resolvedAt.MatchString(row[9]) {
		t.Errorf("resolved row: %q", row)
	}

	// 記録の後に本番の側で値が変わったら、記録は使われず、管理者のマージは拒否される。
	mustExec(t, root, "UPDATE wp_posts SET post_title = 'main title 2' WHERE ID = 1")
	if row := strings.Split(diffRowsAll(t, root, "CALL heimdb_branch_diff('staging', 'wp_posts')")[0], "|"); row[5] != "stale" || row[8] != "'stg'@'%'" {
		t.Errorf("stale row: %q", row)
	}
	mustExec(t, root, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=2', 'parent')")
	mustExec(t, root, "CALL heimdb_branch_resolve('staging', 'wp_options', '*', 'parent')")
	mustExec(t, root, "CALL heimdb_branch_resolve('staging', 'wp_terms', '*', 'parent')")
	if _, err := root.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("merge with a stale resolution: %v", err)
	}
	mustExec(t, stg, "CALL heimdb_branch_resolve('', 'wp_posts', 'ID=1', 'branch')")

	// ワイルドカードの解決は、記録した管理者が、当たった行に出る。段階 63 より前の記録（ユーザーと時刻がない）は NULL。
	if got := diffRowsAll(t, stg, "CALL heimdb_branch_diff('', 'wp_options')"); len(got) != 1 || !strings.Contains(got[0], "|parent|option_value|the same columns were changed on both sides|'root'@'%'|") {
		t.Errorf("wildcard: %q", got)
	}
	b, _ := pro.st.BranchByName("staging")
	base, err := pro.st.MergeBase(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pro.st.PutResolution(b.ID, store.Resolution{Table: "wp_options", ID: "*", Choice: "parent", Base: base.ParentTS}); err != nil {
		t.Fatal(err)
	}
	if got := diffRowsAll(t, stg, "CALL heimdb_branch_diff('', 'wp_options')"); len(got) != 1 || !strings.Contains(got[0], "|parent|option_value|the same columns were changed on both sides|||{") {
		t.Errorf("old record: %q", got)
	}

	// 変更の一覧と復元。
	if got := strings.Join(diffRows(t, stg, "CALL heimdb_changes('', '"+before+"', 'wp_posts')"), "\n"); !strings.Contains(got, "wp_posts|ID=1|update|post_title|") || !strings.Contains(got, "wp_posts|ID=2|delete|") {
		t.Errorf("changes:\n%s", got)
	}
	if got := len(diffRows(t, stg, "CALL heimdb_restore_preview('', '"+before+"', 'wp_posts')")); got == 0 {
		t.Error("no preview")
	}
	// ブランチを作る前の時刻は、ブランチのユーザーには使えない（親の履歴になる）。管理者は使える。
	for _, q := range []string{
		"CALL heimdb_changes('', '" + beforeFork + "')",
		"CALL heimdb_changes('', '" + beforeFork + "', 'wp_posts')",
		"CALL heimdb_restore_preview('', '" + beforeFork + "')",
		"CALL heimdb_restore_preview('', '" + beforeFork + "', 'wp_posts')",
		"CALL heimdb_restore('', '" + beforeFork + "')",
		"CALL heimdb_restore('', '" + beforeFork + "', 'wp_posts')",
	} {
		execErr(t, stg, q, "a branch user cannot use a time before the branch was created")
	}
	if got := len(diffRows(t, rootStg, "CALL heimdb_changes('', '"+beforeFork+"', 'wp_posts')")); got == 0 {
		t.Error("admin before the fork")
	}

	// 管理者がマージする。ブランチのユーザーが記録した側が入る。
	mustExec(t, root, "CALL heimdb_branch_merge('staging')")
	if got := rowsOf(t, root, "SELECT post_title FROM wp_posts WHERE ID IN (1, 2) ORDER BY ID"); got != "staging title,main second" {
		t.Errorf("merged: %s", got)
	}
	mustExec(t, stg, "CALL heimdb_restore('', '"+before+"', 'wp_posts')")
	if got := rowsOf(t, stg, "SELECT post_title FROM wp_posts WHERE ID IN (1, 2) ORDER BY ID"); got != "first,second" {
		t.Errorf("restored: %s", got)
	}

	// 本番のサイトのユーザーは、本番を対象にできない（名前を書いても '' でも）。
	wp := connectOK(t, addr, "wp", "pw", testDB)
	defer wp.Close()
	for _, q := range []string{"CALL heimdb_changes('', '" + before + "')", "CALL heimdb_changes('main', '" + before + "')", "CALL heimdb_restore('', '" + before + "')"} {
		execErr(t, wp, q, "only an administrator can call")
	}
	// EXECUTE がなければ呼べない。CALL を別のデータベースで修飾しても、接続中のデータベースの EXECUTE が要る。
	enum := connectOK(t, addr, "enum", "pw", stagingDB)
	defer enum.Close()
	execErr(t, enum, "CALL heimdb_branch_diff('')", "command denied")
	x := connectOK(t, addr, "x", "pw", stagingDB)
	defer x.Close()
	execErr(t, x, "CALL `"+stg2DB+"`.heimdb_branch_diff('')", "needs the EXECUTE privilege on "+stagingDB)

	// 管理者も '' を使える。データベースを選んでいなければ拒否する。
	if got := len(diffRows(t, rootStg, "CALL heimdb_branch_diff('')")); got == 0 {
		t.Error("admin with ''")
	}
	none := connectOK(t, addr, "root", "rootpw", "")
	defer none.Close()
	execErr(t, none, "CALL heimdb_branch_diff('')", "needs a connection to a branch's database")
}

// 行ごとの差分は、開始と件数で区切って返し、続きがあれば more の行で知らせる（docs/plan.md の段階 63）。
func TestBranchDiffPaging(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	for i := 1; i <= 5; i++ {
		mustExec(t, staging, fmt.Sprintf("INSERT INTO wp_options (option_name, option_value) VALUES ('o%d', 'v')", i))
	}
	all := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options')")
	if len(all) != 5 {
		t.Fatalf("all: %q", all)
	}
	page := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', 0, 2)")
	if want := append(all[:2:2], "*|*|||more|||3 more rows; next: CALL heimdb_branch_diff('staging', 'wp_options', 2, 2)"); strings.Join(page, "\n") != strings.Join(want, "\n") {
		t.Errorf("first page:\n%s", strings.Join(page, "\n"))
	}
	if page := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', 2, 2)"); strings.Join(page, "\n") != strings.Join(append(all[2:4:4], "*|*|||more|||1 more rows; next: CALL heimdb_branch_diff('staging', 'wp_options', 4, 2)"), "\n") {
		t.Errorf("second page:\n%s", strings.Join(page, "\n"))
	}
	if page := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', 4, 2)"); strings.Join(page, "\n") != all[4] {
		t.Errorf("last page:\n%s", strings.Join(page, "\n"))
	}
	if page := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', 1, 9223372036854775807)"); strings.Join(page, "\n") != strings.Join(all[1:], "\n") {
		t.Errorf("largest count:\n%s", strings.Join(page, "\n"))
	}
	if page := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', 9, 2)"); len(page) != 0 {
		t.Errorf("past the end: %q", page)
	}
	execErr(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', 0, 0)", "the count must be 1 or more")
	execErr(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_options', -1, 2)", "the offset must be 0 or more")
	execErr(t, mainDB, "CALL heimdb_branch_diff('staging', '', 0, 2)", "name a table")
}

// ブランチのユーザーが呼んだ差分にも、ブランチの実行時間の上限が効く。差分、変更の一覧、復元は、読みながら文が
// 止められたかを確かめ、最後まで読まずに止まる（docs/plan.md の段階 63）。
func TestBranchUserTimeLimit(t *testing.T) {
	addr, pro, stop := startBranchUserServer(t, t.TempDir(), true)
	defer stop()
	root := connectOK(t, addr, "root", "rootpw", testDB)
	defer root.Close()
	seedWordPress(t, root)
	for _, q := range []string{
		"CALL heimdb_branch_create('staging')",
		"CREATE USER 'stg'@'%' IDENTIFIED BY 'pw'",
		"GRANT ALL ON `" + testDB + "@staging`.* TO 'stg'@'%'",
	} {
		mustExec(t, root, q)
	}
	rootStg := connectOK(t, addr, "root", "rootpw", testDB+"@staging")
	defer rootStg.Close()
	before := one(t, root, "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f+00:00')")
	time.Sleep(10 * time.Millisecond)
	var values []string
	for i := range 20000 {
		values = append(values, fmt.Sprintf("('o%d', 'v')", i))
	}
	mustExec(t, rootStg, "INSERT INTO wp_options (option_name, option_value) VALUES "+strings.Join(values, ","))
	mustExec(t, root, "CALL heimdb_branch_limit('staging', '0', '1ms')")
	stg := connectOK(t, addr, "stg", "pw", testDB+"@staging")
	defer stg.Close()
	for _, q := range []string{"CALL heimdb_branch_diff('')", "CALL heimdb_branch_diff('', '*', 0, 10)"} {
		if _, err := stg.Exec(q); mysqlErrNo(err) != 3024 {
			t.Errorf("%s: %v", q, err)
		}
	}
	// 管理者には効かない。
	if got := len(diffRows(t, rootStg, "CALL heimdb_branch_diff('', '*', 0, 10)")); got != 11 {
		t.Errorf("admin: %d rows", got)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	ctx := gsql.NewContext(canceled)
	for name, call := range map[string]func() (gsql.RowIter, error){
		"diff":    func() (gsql.RowIter, error) { return pro.branchDiff(ctx, "staging", "", 0, 0) },
		"changes": func() (gsql.RowIter, error) { return pro.changes(ctx, "staging", before, "") },
		"preview": func() (gsql.RowIter, error) { return pro.restorePreview(ctx, "staging", before, "") },
		"restore": func() (gsql.RowIter, error) { return pro.restore(ctx, "staging", before, "") },
	} {
		if _, err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := pro.branchResolve(ctx, "staging", "wp_options", "option_name=o1", "parent", caller{admin: true}); !errors.Is(err, context.Canceled) {
		t.Errorf("resolve: %v", err)
	}
}

// 競合した列の値は、MySQL のテキストの結果と同じく文字列で書く（NULL と行のない側は null）。競合でなければ空。
func TestConflictValues(t *testing.T) {
	m := [4]merge.Row{
		1: {"a": []byte("x"), "b": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "c": int64(7), "d": nil},
		3: {"a": "y", "b": nil, "c": int64(8), "d": "z"},
	}
	dec := merge.Decision{Action: merge.Conflict, Columns: []string{"a", "b", "c", "d"}}
	// 型がわからなければ fmt の書き方
	want := `{"a":{"branch":"x","parent":"y"},"b":{"branch":"2026-01-02 03:04:05 +0000 UTC","parent":null},"c":{"branch":"7","parent":"8"},"d":{"branch":null,"parent":"z"}}`
	ctx := gsql.NewEmptyContext()
	if got := conflictValues(ctx, dec, nil, m, [2]map[string]gsql.Type{}); got != want {
		t.Errorf("got %s", got)
	}
	dec.Action, dec.Kind = merge.Apply, merge.Update
	if got := conflictValues(ctx, dec, nil, m, [2]map[string]gsql.Type{}); got != "" {
		t.Errorf("not a conflict: %s", got)
	}
}

// TestConflictValuesText は、conflict_values の値が SELECT の結果と同じ書き方になることを確かめる（DATE は日付だけ、
// DECIMAL は桁をそろえ、ゼロの日時は 0000-00-00 00:00:00）。
func TestConflictValuesText(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE wp_lookup (id bigint unsigned NOT NULL PRIMARY KEY, d date, p decimal(19,4), dt datetime NOT NULL DEFAULT '0000-00-00 00:00:00')")
	mustExec(t, mainDB, "INSERT INTO wp_lookup VALUES (1, '2026-03-04', 12.5, '2026-01-01 00:00:00')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE wp_lookup SET d = '2026-03-05', p = 1.25, dt = '0000-00-00 00:00:00' WHERE id = 1")
	mustExec(t, mainDB, "UPDATE wp_lookup SET d = '2026-03-06', p = 3, dt = '2026-02-02 10:00:00' WHERE id = 1")
	if got := diffRowsAll(t, staging, "SELECT d, p, dt FROM wp_lookup"); len(got) != 1 || got[0] != "2026-03-05|1.2500|0000-00-00 00:00:00" {
		t.Fatalf("select: %q", got)
	}
	got := diffRowsAll(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_lookup')")
	want := `{"d":{"branch":"2026-03-05","parent":"2026-03-06"},"dt":{"branch":"0000-00-00 00:00:00","parent":"2026-02-02 10:00:00"},"p":{"branch":"1.2500","parent":"3.0000"}}`
	if len(got) != 1 || !strings.HasSuffix(got[0], "|"+want) {
		t.Errorf("got %q", got)
	}
}
