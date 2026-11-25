package backend

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/vitess/go/mysql"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

func TestLikeMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"wp_yoast_%", "wp_yoast_indexable", true},
		{"wp_yoast_%", "WP_YOAST_MIGRATIONS", true},
		{"wp_yoast_%", "wp_yoast_", true},
		{"wp_yoast_%", "wp_yoast", false},
		{"wp_yoast_%", "wp_redirection_items", false},
		{"wp_yoast_indexable", "wp_yoast_indexable", true},
		{"wp_yoast_indexable", "wp_yoast_indexable_hierarchy", false},
		{"%_404", "wp_redirection_404", true},
		{"%_404", "wp_redirection_4040", false},
		{"wp_%_logs", "wp_redirection_logs", true},
		{"wp_%_logs", "wp_logs", false},
		{"wp_%%s", "wp_s", true},
		{"wp__", "wp_x", true},
		{"wp__", "wp_xy", false},
		{"%", "", true},
		{"", "", true},
		{"a%b%c", "abxbc", true},
		{"a%b%c", "abxbcd", false},
	} {
		if got := likeMatch(c.pattern, c.name); got != c.want {
			t.Errorf("likeMatch(%q, %q) = %v", c.pattern, c.name, got)
		}
	}
}

func TestIsCoreTable(t *testing.T) {
	for _, c := range []struct {
		prefix, name string
		want         bool
	}{
		{"wp_", "wp_posts", true},
		{"wp_", "WP_Options", true},
		{"wp_", "wp_usermeta", true},
		{"wp_", "wp_foo_posts", false},
		{"wp_", "wp_yoast_indexable", false},
		{"wp_", "abc_posts", false},
		{"abc_", "abc_posts", true},
		{"abc_", "wp_posts", false},
		{"wp_", "wp_2_posts", false},
	} {
		if got := isCoreTable(c.prefix, c.name); got != c.want {
			t.Errorf("isCoreTable(%q, %q) = %v", c.prefix, c.name, got)
		}
	}
}

// yoastTables は、Yoast SEO の索引の表に似た、int の AUTO_INCREMENT の表と、移行の記録の表。
var yoastTables = []string{
	"CREATE TABLE wp_yoast_indexable (id int(11) unsigned NOT NULL AUTO_INCREMENT, object_id bigint(20), permalink varchar(255), PRIMARY KEY (id))",
	"CREATE TABLE wp_yoast_migrations (id int(11) unsigned NOT NULL AUTO_INCREMENT, version varchar(191), PRIMARY KEY (id))",
	"INSERT INTO wp_yoast_indexable (object_id, permalink) VALUES (1, '/a'), (2, '/b'), (3, '/c')",
	"INSERT INTO wp_yoast_migrations (version) VALUES ('20200617122511')",
	"CREATE TABLE wp_yoast_seo_links (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, url varchar(255))",
	"INSERT INTO wp_yoast_seo_links (url) VALUES ('/x')",
}

// excludedTableState は、表の定義と全行（主キーの順）。
func excludedTableState(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	return strings.Join(diffRows(t, db, "SHOW CREATE TABLE "+table), "\n") + "\n" +
		strings.Join(diffRows(t, db, "SELECT * FROM "+table+" ORDER BY 1"), "\n")
}

// マージから外した表は、ブランチで行、列、インデックスを変えても、表を作っても消しても、本番の定義とデータのまま
// （docs/plan.md の段階 60）。外さなければ、同じ変更はマージを止める（同じ行を両側で変えた）。
func TestMergeExcludedTables(t *testing.T) {
	testMergeExcludedTables(t)
}

// 行が多いときの読み方（時点ごとの走査）でも、外した表の件数は同じ（docs/plan.md の段階 59 の rowCursor）。
func TestMergeExcludedTablesWithCursors(t *testing.T) {
	defer func(n int) { cursorMinRows = n }(cursorMinRows)
	cursorMinRows = 1
	testMergeExcludedTables(t)
}

func testMergeExcludedTables(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	for _, q := range yoastTables {
		mustExec(t, mainDB, q)
	}
	// 作り直す前のブランチ（fixture の staging はこの表の前に作った）。
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	for _, q := range []string{
		"INSERT INTO wp_yoast_indexable (object_id, permalink) VALUES (10, '/staged')",
		"UPDATE wp_yoast_indexable SET permalink = '/a2' WHERE id = 1",
		"DELETE FROM wp_yoast_indexable WHERE id = 2",
		"ALTER TABLE wp_yoast_indexable ADD COLUMN extra int NOT NULL DEFAULT 0",
		"CREATE INDEX object_id ON wp_yoast_indexable (object_id)",
		"INSERT INTO wp_yoast_migrations (version) VALUES ('20260325155530')",
		"ALTER TABLE wp_yoast_migrations MODIFY version varchar(255)",
		"DROP TABLE wp_yoast_seo_links",
		"CREATE TABLE wp_yoast_new (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_yoast_new (v) VALUES ('x')",
		"UPDATE wp_posts SET post_content = 'about v2' WHERE ID = 2",
	} {
		mustExec(t, staging, q)
	}
	// 本番でも索引が増え、ブランチが変えた行を本番も変えた。本番だけで作った表（ブランチにはない）は、外しても行を
	// 出さない。
	mustExec(t, mainDB, "INSERT INTO wp_yoast_indexable (object_id, permalink) VALUES (20, '/prod')")
	mustExec(t, mainDB, "UPDATE wp_yoast_indexable SET permalink = '/a-prod' WHERE id = 1")
	mustExec(t, mainDB, "CREATE TABLE wp_yoast_prod_only (id bigint unsigned NOT NULL PRIMARY KEY)")

	// 外さなければ、マージは止まる。
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging', 'dry_run')"); err == nil {
		t.Fatal("merge without exclusions succeeded")
	}
	if got := diffRows(t, mainDB, "CALL heimdb_merge_exclude('wp_yoast_%')"); strings.Join(got, ",") != "wp_yoast_%" {
		t.Errorf("exclude: %v", got)
	}
	before := map[string]string{}
	for _, tb := range []string{"wp_yoast_indexable", "wp_yoast_migrations", "wp_yoast_seo_links"} {
		before[tb] = excludedTableState(t, mainDB, tb)
	}

	// 件数は、ブランチの側で変わった行（列を足したので、変えなかった行 id=3 も形が変わって変更に数える）。
	summary := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"), "\n")
	for _, want := range []string{
		"wp_yoast_indexable|excluded|1|2|1|0|0|0|0|excluded by the pattern 'wp_yoast_%'",
		"wp_yoast_seo_links|excluded|0|0|1|0|0|0|0|excluded by the pattern 'wp_yoast_%'",
		"wp_yoast_migrations|excluded|1|0|0|0|0|0|0|excluded by the pattern 'wp_yoast_%'",
		"wp_yoast_new|excluded|1|0|0|0|0|0|0|excluded by the pattern 'wp_yoast_%'",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("diff: missing %s in\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "wp_yoast_prod_only") {
		t.Errorf("diff: a table only on the parent is listed:\n%s", summary)
	}
	if rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n"); !strings.Contains(rows, "wp_yoast_indexable|*|||excluded|||excluded by the pattern 'wp_yoast_%'") || strings.Contains(rows, "id=") {
		t.Errorf("diff rows:\n%s", rows)
	}

	merged := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	got := strings.Join(merged, "\n")
	for _, want := range []string{
		"wp_posts|merged|0|1|0|0|0|0|0|",
		"wp_yoast_indexable|excluded|1|2|1|0|0|0|0|excluded by the pattern 'wp_yoast_%'",
		"wp_yoast_new|excluded|1|0|0|0|0|0|0|",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("merge: missing %s in\n%s", want, got)
		}
	}
	// マージは rewrite_rules を空にするので wp_options も入る。外した表は入らない。
	if strings.Contains(got, "wp_yoast_prod_only") {
		t.Errorf("merge: a table only on the parent is listed:\n%s", got)
	}
	if undo := lastRow(merged); !strings.HasSuffix(undo, "', 'wp_options,wp_posts')") {
		t.Errorf("undo: %s", undo)
	}
	for tb, want := range before {
		if got := excludedTableState(t, mainDB, tb); got != want {
			t.Errorf("%s changed by the merge:\n%s\nwant:\n%s", tb, got, want)
		}
	}
	if _, err := mainDB.Exec("SELECT * FROM wp_yoast_new"); err == nil {
		t.Error("an excluded new table was created on the parent")
	}
	if got := one(t, mainDB, "SELECT post_content FROM wp_posts WHERE ID = 2"); got != "about v2" {
		t.Errorf("wp_posts not merged: %s", got)
	}

	// ブランチがあれば消せない（段階 132）。ないパターンを消すのは何もしない。
	if _, err := mainDB.Exec("CALL heimdb_merge_include('wp_yoast_%')"); sqlErrNum(err) != 51011 ||
		!strings.Contains(err.Error(), "HeimDB: merge refused (branches_exist)") || !strings.HasSuffix(err.Error(), ": staging") {
		t.Errorf("include with a branch: %v", err)
	}
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_merge_include('wp_none')"), ","); got != "wp_yoast_%" {
		t.Errorf("include of an unknown pattern: %s", got)
	}
	staging.Close()
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")

	// 消すと、一覧から消える。同じものを足しても消しても 1 つ。
	mustExec(t, mainDB, "CALL heimdb_merge_exclude('WP_YOAST_%')")
	mustExec(t, mainDB, "CALL heimdb_merge_exclude('wp_redirection_404')")
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_merge_include('wp_yoast_%')"), ","); got != "wp_redirection_404" {
		t.Errorf("include: %s", got)
	}
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_merge_include('wp_yoast_%')"), ","); got != "wp_redirection_404" {
		t.Errorf("include twice: %s", got)
	}
	if _, err := mainDB.Exec("CALL heimdb_merge_exclude(' ')"); err == nil {
		t.Error("empty pattern accepted")
	}
}

// WordPress 本体の表は外せない。接頭辞が決まっていれば足すときにエラーにし、決まる前（インストールの前）に足した
// パターンは、マージのときに本体の表を外さず、結果に not excluded の行を出す。
func TestMergeExcludeCoreTables(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// インストールの前は、接頭辞が決まらないので受け付ける。
	mustExec(t, db, "CALL heimdb_merge_exclude('wp_%')")
	seedWordPress(t, db)
	mustExec(t, db, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	for _, pat := range []string{"wp_posts", "WP_OPTIONS", "wp_term%", "%meta", "wp_users"} {
		if _, err := db.Exec("CALL heimdb_merge_exclude(?)", pat); mysqlErrNo(err) != 51009 || !strings.Contains(err.Error(), "merge refused (core_table): ") || !strings.Contains(err.Error(), "WordPress core table") {
			t.Errorf("%s: %v", pat, err)
		}
	}
	mustExec(t, db, "CALL heimdb_merge_exclude('wp_foo_posts')")
	mustExec(t, db, "CREATE TABLE wp_foo_posts (id bigint NOT NULL PRIMARY KEY, v varchar(10))")
	mustExec(t, db, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE wp_options SET option_value = 'Staged' WHERE option_name = 'blogname'")
	mustExec(t, staging, "INSERT INTO wp_foo_posts VALUES (1, 'x')")

	summary := strings.Join(diffRows(t, db, "CALL heimdb_branch_diff('staging')"), "\n")
	for _, want := range []string{
		"wp_options|not excluded|0|0|0|0|0|0|0|matches the pattern 'wp_%', but WordPress core tables are always merged",
		"wp_options|changed|0|1|0|",
		"wp_foo_posts|excluded|1|0|0|0|0|0|0|excluded by the pattern 'wp_%'",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("diff: missing %s in\n%s", want, summary)
		}
	}
	got := strings.Join(diffRows(t, db, "CALL heimdb_branch_merge('staging')"), "\n")
	for _, want := range []string{"wp_options|not excluded|", "wp_options|merged|0|1|0|", "wp_posts|not excluded|", "wp_foo_posts|excluded|1|"} {
		if !strings.Contains(got, want) {
			t.Errorf("merge: missing %s in\n%s", want, got)
		}
	}
	if got := one(t, db, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Staged" {
		t.Errorf("wp_options not merged: %s", got)
	}
	if n := one(t, db, "SELECT COUNT(*) FROM wp_foo_posts"); n != "0" {
		t.Errorf("wp_foo_posts merged: %s", n)
	}
}

// 接頭辞が wp_ でなければ、その接頭辞の表が本体（wp_posts はプラグインの表と同じ扱い）。
func TestMergeExcludeOtherPrefix(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE abc_options (option_id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name varchar(191), option_value longtext)")
	mustExec(t, db, "CREATE TABLE abc_posts (ID bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title text)")
	if _, err := db.Exec("CALL heimdb_merge_exclude('abc_posts')"); err == nil {
		t.Error("abc_posts excluded")
	}
	mustExec(t, db, "CALL heimdb_merge_exclude('wp_posts')")
}

// 接頭辞の組が 2 つ以上あれば、差分とマージを ambiguous_prefix（51006）で拒否する。
func TestMergeAmbiguousPrefix(t *testing.T) {
	mainDB, _, stop := mergeFixture(t)
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE wp_old_options (option_id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name varchar(191), option_value longtext)")
	mustExec(t, mainDB, "CREATE TABLE wp_old_posts (ID bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title text)")
	for _, q := range []string{"CALL heimdb_branch_diff('staging')", "CALL heimdb_branch_merge('staging')", "CALL heimdb_branch_merge('staging', 'dry_run')", "CALL heimdb_branch_resolve('staging', '*', '*', 'parent')"} {
		_, err := mainDB.Exec(q)
		if mysqlErrNo(err) != 51006 || !strings.Contains(err.Error(), "merge refused (ambiguous_prefix)") || !strings.Contains(err.Error(), "wp_, wp_old_") {
			t.Errorf("%s: %v", q, err)
		}
	}
	// 片方の組がそろわなければ（posts がない）、接頭辞は 1 つ。
	mustExec(t, mainDB, "DROP TABLE wp_old_posts")
	mustExec(t, mainDB, "CALL heimdb_branch_diff('staging')")
	// マルチサイトでなければ（wp_blogs がない）、数字の接頭辞 wp_2_ も 2 つ目の組（数字だけの接頭辞のサイトに古い
	// インストールが残った形）。
	mustExec(t, mainDB, "DROP TABLE wp_blogs")
	mustExec(t, mainDB, "CREATE TABLE wp_2_options (option_id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name varchar(191), option_value longtext)")
	mustExec(t, mainDB, "CREATE TABLE wp_2_posts (ID bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title text)")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); mysqlErrNo(err) != 51006 || !strings.Contains(err.Error(), "wp_, wp_2_") {
		t.Errorf("wp_2_ without wp_blogs: %v", err)
	}
}

// マージを拒否するエラーは、理由の語と HeimDB の番号を持つ（docs/procedures.md の「マージを拒否する理由」）。
func TestMergeRefusalNumbers(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	check := func(db *sql.DB, q string, num uint16, reason string) {
		t.Helper()
		_, err := db.Exec(q)
		if mysqlErrNo(err) != num || !strings.Contains(err.Error(), "HeimDB: merge refused ("+reason+"): ") {
			t.Errorf("%s: %v", q, err)
		}
	}
	check(mainDB, "CALL heimdb_branch_merge('nope')", 51001, "branch_not_found")
	check(mainDB, "CALL heimdb_branch_diff('nope')", 51001, "branch_not_found")
	check(mainDB, "CALL heimdb_branch_resolve('nope', '*', '*', 'parent')", 51001, "branch_not_found")
	check(mainDB, "CALL heimdb_branch_merge('main')", 51002, "no_parent")

	mustExec(t, staging, "UPDATE wp_posts SET post_content = 'staged' WHERE ID = 2")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_content = 'prod' WHERE ID = 2")
	check(mainDB, "CALL heimdb_branch_merge('staging')", 51004, "unresolved_conflicts")
	check(mainDB, "CALL heimdb_branch_merge('staging', 'dry_run')", 51004, "unresolved_conflicts")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', '*', '*', 'parent')")

	conn, err := mainDB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{"START TRANSACTION", "UPDATE wp_posts SET post_title = 'x' WHERE ID = 3"} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	_, err = conn.ExecContext(t.Context(), "CALL heimdb_branch_merge('staging')")
	if mysqlErrNo(err) != 51003 || !strings.Contains(err.Error(), "merge refused (in_transaction)") {
		t.Errorf("in transaction: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	mustExec(t, staging, "UPDATE wp_options SET option_value = '60718' WHERE option_name = 'db_version'")
	check(mainDB, "CALL heimdb_branch_merge('staging')", 51005, "db_version_differs")
	mustExec(t, staging, "UPDATE wp_options SET option_value = '60717' WHERE option_name = 'db_version'")
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
}

// レプリカでは、マージを read_only_replica（51008）で拒否する。
func TestMergeRefusedOnReplica(t *testing.T) {
	p, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.CreateBranch("staging", store.MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	r, err := store.OpenWith(backup, store.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = NewProvider(r, testDB).branchMerge(gsql.NewEmptyContext(), "staging", false)
	if sqlErr, ok := asRefusal(err); !ok || sqlErr.Num != 51008 || !strings.Contains(sqlErr.Message, "merge refused (read_only_replica)") {
		t.Errorf("replica: %v", err)
	}
	if _, err := NewProvider(r, testDB).mergeExclude("wp_yoast_%", true); err == nil {
		t.Error("exclude on a replica")
	}
}

// 除外を変えられるのは管理者だけ。一覧は、サイトのユーザーとブランチだけの権限のユーザーも読める。
func TestMergeExcludePrivileges(t *testing.T) {
	addr, stop := startAuthServer(t, t.TempDir(), "rootpw")
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	seedWordPress(t, root)
	mustExec(t, root, "CALL heimdb_merge_exclude('wp_yoast_%')")
	mustExec(t, root, "CALL heimdb_branch_create('staging')")
	mustExec(t, root, "CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'")
	mustExec(t, root, "GRANT ALL ON "+testDB+".* TO 'wp'@'%'")
	mustExec(t, root, "CREATE USER 'stg'@'%' IDENTIFIED BY 'stgpw'")
	mustExec(t, root, "GRANT ALL ON `"+testDB+"@staging`.* TO 'stg'@'%'")
	for _, u := range []struct{ user, pw, db string }{{"wp", "wppw", testDB}, {"stg", "stgpw", testDB + "@staging"}} {
		db, err := connectAs(t, addr, u.user, u.pw, u.db)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(diffRows(t, db, "CALL heimdb_merge_excludes()"), ","); got != "wp_yoast_%" {
			t.Errorf("%s: list: %s", u.user, got)
		}
		for _, q := range []string{"CALL heimdb_merge_include('wp_yoast_%')", "CALL heimdb_merge_exclude('wp_x')", "CALL heimdb_branch_merge('staging')"} {
			if _, err := db.Exec(q); err == nil || !strings.Contains(err.Error(), "command denied") {
				t.Errorf("%s: %s: %v", u.user, q, err)
			}
		}
		db.Close()
	}
	if got := strings.Join(diffRows(t, root, "CALL heimdb_merge_excludes()"), ","); got != "wp_yoast_%" {
		t.Errorf("after: %s", got)
	}
}

// マージが包んだ拒否（適用の途中の parent_changed）も、接続先には理由の番号で返す（包んだままだと 1105）。
func TestRefusalUnwrapped(t *testing.T) {
	wrapped := fmt.Errorf("HeimDB: merge wp_posts: %w", refuseParentChanged.errorf("the parent's row changed while merging"))
	// go-mysql-server（sql.CastSQLError）は、いちばん外側が *mysql.SQLError のときだけ番号を使う。
	got := (&Session{}).TranslateQueryError(wrapped)
	if sqlErr, ok := got.(*mysql.SQLError); !ok || sqlErr.Num != 51007 ||
		sqlErr.Message != "HeimDB: merge refused (parent_changed): the parent's row changed while merging" {
		t.Errorf("translated: %T %v", got, got)
	}
}

// マルチサイト（ネットワークの表 wp_blogs がある）のサブサイトの表（wp_2_options と wp_2_posts）は 2 つ目の接頭辞の
// 組と数えず、ふつうの表としてマージする。
func TestMergeMultisitePrefix(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	if one(t, mainDB, "SHOW TABLES LIKE 'wp_blogs'") != "wp_blogs" { // テストの表（wpSchema）にネットワークの表がある
		t.Fatal("no wp_blogs")
	}
	for _, q := range []string{
		"CREATE TABLE wp_2_options (option_id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name varchar(191), option_value longtext)",
		"CREATE TABLE wp_2_posts (ID bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title text)",
		"CREATE TABLE wp_10_options (option_id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name varchar(191), option_value longtext)",
		"CREATE TABLE wp_10_posts (ID bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title text)",
	} {
		mustExec(t, mainDB, q)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	mustExec(t, staging, "INSERT INTO wp_2_posts (post_title) VALUES ('subsite')")
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')"), "\n"); !strings.Contains(got, "wp_2_posts|merged|1|") {
		t.Errorf("merge:\n%s", got)
	}
	if got := one(t, mainDB, "SELECT post_title FROM wp_2_posts"); got != "subsite" {
		t.Errorf("wp_2_posts: %s", got)
	}
}

func TestIsSubsitePrefix(t *testing.T) {
	for _, c := range []struct {
		p, q string
		want bool
	}{
		{"wp_", "wp_2_", true},
		{"wp_", "wp_123_", true},
		{"wp_", "wp_old_", false},
		{"wp_", "wp__", false},
		{"wp_", "wp_2", false},
		{"wp_", "wp_2x_", false},
		{"abc_", "wp_2_", false},
	} {
		if got := isSubsitePrefix(c.p, c.q); got != c.want {
			t.Errorf("isSubsitePrefix(%q, %q) = %v", c.p, c.q, got)
		}
	}
}
