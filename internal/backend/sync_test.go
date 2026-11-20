package backend

import (
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// syncFixture は、本番（main）と 3 つのブランチ a、b、c を作る（docs/plan.md の段階 94）。
func syncFixture(t *testing.T) (mainDB *sql.DB, dsn string, branches map[string]*sql.DB, stop func()) {
	t.Helper()
	m, dsn, stopServer := branchServer(t)
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717'), ('rewrite_rules', 'a:1:{}')",
		"INSERT INTO wp_users (ID, user_login, user_registered) VALUES (1, 'admin', '2026-01-01 00:00:00')",
		insertPost + "(2, 1, 'About', 'publish', 'page', 'about v1', '', '', '', ''), (3, 1, 'Contact', 'publish', 'page', 'c', '', '', '', '')",
		"INSERT INTO wp_terms (term_id, name, slug) VALUES (1, 'Uncategorized', 'uncategorized')",
		"INSERT INTO wp_term_taxonomy (term_taxonomy_id, term_id, taxonomy, description, count) VALUES (1, 1, 'category', '', 1)",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (3, 1)",
	} {
		mustExec(t, m, q)
	}
	branches = map[string]*sql.DB{}
	for _, name := range []string{"a", "b", "c"} {
		mustExec(t, m, "CALL heimdb_branch_create('"+name+"')")
		branches[name] = openBranch(t, dsn, name)
	}
	return m, dsn, branches, func() {
		for _, b := range branches {
			b.Close()
		}
		stopServer()
	}
}

// sqlErrNum は、エラーの MySQL の番号（なければ 0）。
func sqlErrNum(err error) uint16 {
	if e, ok := err.(*mysql.MySQLError); ok {
		return e.Number
	}
	return 0
}

// TestBranchSync は、段階 94 の完了条件の流れ: A と B をマージした後、検証中の C に本番の最新を取り込み、C をマージしても
// A と B の変更が壊れず、C の変更が入る。
func TestBranchSync(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	a, b, c := br["a"], br["b"], br["c"]
	// C の検証中の変更（取り込みの前に書いた、まだマージしていない変更）。
	for _, q := range []string{
		"UPDATE wp_posts SET post_content = 'contact (c)' WHERE ID = 3",
		"UPDATE wp_options SET option_value = 'https://c.example.com' WHERE option_name = 'siteurl'",
		insertPost + "(NULL, 1, 'C page', 'publish', 'page', 'from c', '', '', '', '')",
	} {
		mustExec(t, c, q)
	}
	// A はオプションと固定ページ、B は新しい記事。その間に本番にコメント。
	mustExec(t, a, "UPDATE wp_options SET option_value = 'Prod (a)' WHERE option_name = 'blogname'")
	mustExec(t, a, "UPDATE wp_posts SET post_title = 'About (a)' WHERE ID = 2")
	mustExec(t, b, insertPost+"(NULL, 1, 'B post', 'publish', 'post', 'from b', '', '', '', '')")
	mustExec(t, b, "INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (20000000000, 1)")
	mustExec(t, mainDB, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (2, 'reader', 'nice', '1')")
	mustExec(t, mainDB, "UPDATE wp_posts SET comment_count = 1 WHERE ID = 2")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('a')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('b')")

	// 書かずに確かめる。
	dry := diffRows(t, mainDB, "CALL heimdb_branch_sync('c', 'dry_run')")
	if lastRow(dry) != "*|dry run|0|0|0|0|0|0|0|nothing was committed" {
		t.Errorf("dry run: %v", dry)
	}
	if got := one(t, c, "SELECT post_title FROM wp_posts WHERE ID = 2"); got != "About" {
		t.Errorf("dry run wrote: %s", got)
	}

	synced := diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	got := strings.Join(synced[:len(synced)-1], "\n")
	want := strings.Join([]string{
		"wp_comments|synced|1|0|0|0|0|0|0|",
		"wp_options|synced|0|1|0|1|0|0|0|",
		"wp_posts|synced|1|1|0|0|0|0|0|",
		"wp_term_relationships|synced|1|0|0|0|0|0|0|",
	}, "\n")
	if got != want {
		t.Errorf("sync:\n%s\nwant:\n%s", got, want)
	}
	if undo := lastRow(synced); !strings.HasPrefix(undo, "*|undo|0|0|0|0|0|0|0|CALL heimdb_restore('c', '") ||
		!strings.HasSuffix(undo, "', 'wp_comments,wp_options,wp_posts,wp_term_relationships,wp_term_taxonomy')") {
		t.Errorf("undo: %s", undo)
	}
	for q, want := range map[string]string{
		"SELECT CONCAT(post_title, '/', post_content, '/', comment_count) FROM wp_posts WHERE ID = 2": "About (a)/about v1/1",
		"SELECT post_content FROM wp_posts WHERE ID = 3":                                              "contact (c)",
		"SELECT post_title FROM wp_posts WHERE ID = 20000000000":                                      "B post",
		"SELECT post_title FROM wp_posts WHERE ID = 30000000000":                                      "C page",
		"SELECT option_value FROM wp_options WHERE option_name = 'blogname'":                          "Prod (a)",
		"SELECT option_value FROM wp_options WHERE option_name = 'siteurl'":                           "https://c.example.com",
		"SELECT COUNT(*) FROM wp_comments":                                                            "1",
		"SELECT count FROM wp_term_taxonomy WHERE term_taxonomy_id = 1":                               "2",
	} {
		if got := one(t, c, q); got != want {
			t.Errorf("c: %s = %q, want %q", q, got, want)
		}
	}
	// 取り込んだ後の差分は、C の変更だけ。
	diff := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('c')"), "\n")
	if diff != "wp_options|changed|0|0|0|1|0|0|0|\nwp_posts|changed|1|1|0|0|0|0|0|" {
		t.Errorf("diff after sync:\n%s", diff)
	}
	// もう一度取り込んでも、何もしない。
	if again := diffRows(t, mainDB, "CALL heimdb_branch_sync('c')"); len(again) != 0 {
		t.Errorf("second sync: %v", again)
	}

	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	for q, want := range map[string]string{
		"SELECT CONCAT(post_title, '/', post_content, '/', comment_count) FROM wp_posts WHERE ID = 2": "About (a)/about v1/1",
		"SELECT post_content FROM wp_posts WHERE ID = 3":                                              "contact (c)",
		"SELECT post_title FROM wp_posts WHERE ID = 20000000000":                                      "B post",
		"SELECT post_title FROM wp_posts WHERE ID = 30000000000":                                      "C page",
		"SELECT option_value FROM wp_options WHERE option_name = 'blogname'":                          "Prod (a)",
		"SELECT option_value FROM wp_options WHERE option_name = 'siteurl'":                           "http://prod.example",
		"SELECT COUNT(*) FROM wp_comments":                                                            "1",
		"SELECT COUNT(*) FROM wp_posts":                                                               "5",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("main: %s = %q, want %q", q, got, want)
		}
	}
}

// TestBranchSyncConflicts: 両側が同じ列を違う値に変えた行は、何も書かずに拒否する。'branch' はブランチの値を残し、
// 後のマージで本番に入る。'parent' は親の値を採る。
func TestBranchSyncConflicts(t *testing.T) {
	for _, side := range []string{"branch", "parent"} {
		t.Run(side, func(t *testing.T) {
			mainDB, _, br, stop := syncFixture(t)
			defer stop()
			c := br["c"]
			mustExec(t, c, "UPDATE wp_posts SET post_title = 'About (c)', post_content = 'body (c)' WHERE ID = 2")
			mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'About (main)', post_excerpt = 'excerpt (main)' WHERE ID = 2")
			_, err := mainDB.Exec("CALL heimdb_branch_sync('c')")
			if sqlErrNum(err) != 51004 || !strings.Contains(err.Error(), "HeimDB: sync refused (unresolved_conflicts): conflicts: wp_posts ID=2;") {
				t.Fatalf("sync with a conflict: %v", err)
			}
			if got := one(t, c, "SELECT post_excerpt FROM wp_posts WHERE ID = 2"); got != "" {
				t.Fatalf("refused sync wrote: %q", got)
			}
			rows := diffRows(t, mainDB, "CALL heimdb_branch_sync('c', '"+side+"')")
			if rows[0] != "wp_posts|synced|0|1|0|0|0|1|0|" {
				t.Errorf("sync: %v", rows)
			}
			title := map[string]string{"branch": "About (c)", "parent": "About (main)"}[side]
			want := title + "/body (c)/excerpt (main)"
			q := "SELECT CONCAT(post_title, '/', post_content, '/', post_excerpt) FROM wp_posts WHERE ID = 2"
			if got := one(t, c, q); got != want {
				t.Errorf("c after sync: %q, want %q", got, want)
			}
			// 残したブランチの値は、次のマージで本番に入る（競合にならない）。
			diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
			if got := one(t, mainDB, q); got != want {
				t.Errorf("main after merge: %q, want %q", got, want)
			}
		})
	}
}

// TestBranchSyncUndo: 取り込みの結果の戻す文で、ブランチも基準も取り込みの前に戻る。戻した後のマージは、取り込まずに
// マージしたのと同じ（A の変更を消さない）。
func TestBranchSyncUndo(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	a, c := br["a"], br["c"]
	mustExec(t, c, "UPDATE wp_posts SET post_content = 'contact (c)' WHERE ID = 3")
	mustExec(t, a, "UPDATE wp_posts SET post_title = 'About (a)' WHERE ID = 2")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('a')")
	synced := diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	undo := strings.TrimPrefix(lastRow(synced), "*|undo|0|0|0|0|0|0|0|")
	diffRows(t, mainDB, undo)
	if got := one(t, c, "SELECT CONCAT(post_title, '/', post_content) FROM wp_posts WHERE ID IN (2, 3) ORDER BY ID LIMIT 1"); got != "About/about v1" {
		t.Errorf("c after undo: %s", got)
	}
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(CONCAT(post_title, '/', post_content) ORDER BY ID) FROM wp_posts"); got != "first/c,About (a)/about v1,Contact/contact (c)" {
		t.Errorf("main after merge: %s", got)
	}
}

// TestBranchSyncAfterMerge: C を一度マージした後の取り込みは、そのマージの前に本番に入った変更（コメント）も拾う
// （基準はマージの時点のブランチ、候補は分岐の後に本番が書いた行）。
func TestBranchSyncAfterMerge(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, mainDB, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (2, 'reader', 'before the merge', '1')")
	mustExec(t, c, "UPDATE wp_posts SET post_content = 'about (c)' WHERE ID = 2")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	mustExec(t, c, "UPDATE wp_posts SET post_content = 'about (c2)' WHERE ID = 2")
	rows := diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	// 記事の行は書かない（本番の記事 2 はマージで入れた C の値のまま）。コメント数は数え直す。rewrite_rules は環境ごとの値。
	if len(rows) != 3 || rows[0] != "wp_comments|synced|1|0|0|0|0|0|0|" || rows[1] != "wp_options|synced|0|0|0|1|0|0|0|" {
		t.Errorf("sync: %v", rows)
	}
	if got := one(t, c, "SELECT CONCAT(post_content, '/', comment_count) FROM wp_posts WHERE ID = 2"); got != "about (c2)/1" {
		t.Errorf("c: %s", got)
	}
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	if got := one(t, mainDB, "SELECT post_content FROM wp_posts WHERE ID = 2"); got != "about (c2)" {
		t.Errorf("main: %s", got)
	}
}

// TestBranchSyncRevivedDependents: マージで方針により飛ばした記事（自動下書き）を、取り込みの後に公開してマージすると、
// 前に付けたメタも入る。
func TestBranchSyncRevivedDependents(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, c, insertPost+"(NULL, 1, 'Draft', 'auto-draft', 'post', '', '', '', '', '')")
	mustExec(t, c, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (30000000000, 'hero', 'red')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	mustExec(t, c, "UPDATE wp_posts SET post_status = 'publish' WHERE ID = 30000000000")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'About (main)' WHERE ID = 2")
	diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	if got := one(t, mainDB, "SELECT CONCAT(p.post_status, '/', m.meta_value) FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID WHERE p.ID = 30000000000"); got != "publish/red" {
		t.Errorf("main: %s", got)
	}
}

// TestBranchSyncIDCollision: 前のマージで飛ばした C の行と同じ ID の別の行を本番が足すと、黙って置き換えずに競合にする。
func TestBranchSyncIDCollision(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, c, insertPost+"(500, 1, 'C draft', 'auto-draft', 'post', '', '', '', '', '')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	mustExec(t, mainDB, insertPost+"(500, 1, 'Main post', 'publish', 'post', '', '', '', '', '')")
	_, err := mainDB.Exec("CALL heimdb_branch_sync('c')")
	if sqlErrNum(err) != 51004 || !strings.Contains(err.Error(), "wp_posts ID=500") {
		t.Fatalf("sync: %v", err)
	}
	// ブランチの行は残せない（残すと、次のマージで本番の行を黙って上書きする）。
	if _, err := mainDB.Exec("CALL heimdb_branch_sync('c', 'branch')"); sqlErrNum(err) != 51004 || !strings.Contains(err.Error(), "wp_posts ID=500 (only 'parent' can resolve this") {
		t.Fatalf("sync with 'branch': %v", err)
	}
	rows := diffRows(t, mainDB, "CALL heimdb_branch_sync('c', 'parent')")
	if !strings.Contains(strings.Join(rows, "\n"), "ID=500: the branch's row with the same ID was replaced by the parent's") {
		t.Errorf("sync: %v", rows)
	}
	if got := one(t, c, "SELECT post_title FROM wp_posts WHERE ID = 500"); got != "Main post" {
		t.Errorf("c: %s", got)
	}
}

// TestBranchSyncNaturalKey: 本番に入った記事と同じスラッグの記事を C も足していれば、本番の記事（とそのメタ）は写さず、
// 警告に出す。
func TestBranchSyncNaturalKey(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, c, "INSERT INTO wp_posts (post_author, post_title, post_name, post_status, post_type, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES (1, 'Styles (c)', 'wp-global-styles', 'publish', 'wp_global_styles', '{}', '', '', '', '')")
	mustExec(t, mainDB, "INSERT INTO wp_posts (ID, post_author, post_title, post_name, post_status, post_type, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES (9, 1, 'Styles', 'wp-global-styles', 'publish', 'wp_global_styles', '{}', '', '', '', '')")
	mustExec(t, mainDB, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (9, 'k', 'v')")
	rows := diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	all := strings.Join(rows, "\n")
	if !strings.Contains(all, "wp_postmeta|synced|0|0|0|1|0|0|0|") || !strings.Contains(all, "ID=9: post_type=wp_global_styles,post_name=wp-global-styles was also added on the branch as ID=30000000000; not synced") {
		t.Errorf("sync:\n%s", all)
	}
	if got := one(t, c, "SELECT COUNT(*) FROM wp_posts WHERE post_type = 'wp_global_styles'"); got != "1" {
		t.Errorf("c: %s", got)
	}
}

// TestBranchSyncTables: 本番で作られた表は C に作って行を写す。定義の違う表があれば、何も書かずに拒否する。
func TestBranchSyncTables(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, mainDB, "CREATE TABLE wp_plugin (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10), KEY v (v))")
	mustExec(t, mainDB, "INSERT INTO wp_plugin (v) VALUES ('a'), ('b')")
	rows := diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	if rows[0] != "wp_plugin|new table|2|0|0|0|0|0|0|" {
		t.Errorf("sync: %v", rows)
	}
	if got := one(t, c, "SELECT GROUP_CONCAT(v ORDER BY v) FROM wp_plugin WHERE v >= 'a'"); got != "a,b" {
		t.Errorf("c: %s", got)
	}
	mustExec(t, c, "INSERT INTO wp_plugin (v) VALUES ('c')")
	if got := one(t, c, "SELECT MAX(id) FROM wp_plugin"); got != "30000000000" {
		t.Errorf("c auto increment: %s", got)
	}
	// 本番では、足して消したインデックスの分だけ、新しいインデックスの ID が進む（ID と次の ID は比べない）。
	mustExec(t, mainDB, "ALTER TABLE wp_plugin ADD INDEX tmp (id)")
	mustExec(t, mainDB, "ALTER TABLE wp_plugin DROP INDEX tmp")
	mustExec(t, mainDB, "ALTER TABLE wp_plugin ADD INDEX v2 (v, id)")
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'x' WHERE option_name = 'blogname'")
	_, err := mainDB.Exec("CALL heimdb_branch_sync('c')")
	if sqlErrNum(err) != 51010 || !strings.Contains(err.Error(), "HeimDB: sync refused (schema_differs): the definitions of wp_plugin differ") {
		t.Fatalf("sync with a different definition: %v", err)
	}
	mustExec(t, c, "ALTER TABLE wp_plugin ADD INDEX v2 (v, id)")
	diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	if got := one(t, c, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "x" {
		t.Errorf("c: %s", got)
	}
}

// TestBranchSyncParentRestore: 取り込みの後に本番を取り込みの前に戻しても、C のマージは本番を戻した状態のまま
// （写した A の変更をもう一度本番に入れない）。
func TestBranchSyncParentRestore(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	a, c := br["a"], br["c"]
	mustExec(t, a, "UPDATE wp_posts SET post_title = 'About (a)' WHERE ID = 2")
	merged := diffRows(t, mainDB, "CALL heimdb_branch_merge('a')")
	diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	mustExec(t, c, "UPDATE wp_posts SET post_content = 'contact (c)' WHERE ID = 3")
	diffRows(t, mainDB, strings.TrimPrefix(lastRow(merged), "*|undo|0|0|0|0|0|0|0|"))
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(CONCAT(post_title, '/', post_content) ORDER BY ID) FROM wp_posts"); got != "first/c,About/about v1,Contact/contact (c)" {
		t.Errorf("main: %s", got)
	}
}

// TestBranchSyncRefusals: 取り込みの拒否の番号（マージと共通の語）。
func TestBranchSyncRefusals(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	for q, want := range map[string]uint16{
		"CALL heimdb_branch_sync('nope')": 51001,
		"CALL heimdb_branch_sync('main')": 51002,
	} {
		_, err := mainDB.Exec(q)
		if sqlErrNum(err) != want || !strings.Contains(err.Error(), "HeimDB: sync refused (") {
			t.Errorf("%s: %v", q, err)
		}
	}
	mustExec(t, br["c"], "UPDATE wp_options SET option_value = '99999' WHERE option_name = 'db_version'")
	if _, err := mainDB.Exec("CALL heimdb_branch_sync('c')"); sqlErrNum(err) != 51005 || !strings.Contains(err.Error(), "before syncing") {
		t.Errorf("db_version: %v", err)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_sync('c', 'both')"); err == nil || !strings.Contains(err.Error(), "unknown sync mode") {
		t.Errorf("mode: %v", err)
	}
}

// syncWorld は、プロパティテストの 1 つの世界（本番とブランチ a、c）。
type syncWorld struct {
	main, a, c *sql.DB
	st         *store.Store
	stop       func()
}

func newSyncWorld(t *testing.T) *syncWorld {
	t.Helper()
	w := &syncWorld{}
	m, dsn, stop := branchServer(t, func(s *store.Store) { w.st = s })
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')",
		"INSERT INTO wp_users (ID, user_login, user_registered) VALUES (1, 'admin', '2026-01-01 00:00:00')",
		insertPost + "(2, 1, 't2', 'publish', 'post', 'c2', 'e2', '', '', ''), (3, 1, 't3', 'publish', 'post', 'c3', 'e3', '', '', ''), (4, 1, 't4', 'publish', 'post', 'c4', 'e4', '', '', '')",
	} {
		mustExec(t, m, q)
	}
	mustExec(t, m, "CALL heimdb_branch_create('a')")
	mustExec(t, m, "CALL heimdb_branch_create('c')")
	w.main, w.a, w.c = m, openBranch(t, dsn, "a"), openBranch(t, dsn, "c")
	w.stop = func() {
		w.a.Close()
		w.c.Close()
		stop()
	}
	return w
}

// snapshot は、本番の記事、メタ、コメント、オプションを 1 つの文字列にする。
func (w *syncWorld) snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		"SELECT GROUP_CONCAT(CONCAT_WS('/', ID, post_title, post_content, post_excerpt, post_status) ORDER BY ID SEPARATOR ';') FROM wp_posts",
		"SELECT GROUP_CONCAT(CONCAT_WS('/', post_id, meta_key, meta_value) ORDER BY post_id, meta_key SEPARATOR ';') FROM wp_postmeta",
		"SELECT GROUP_CONCAT(CONCAT_WS('/', comment_post_ID, comment_content) ORDER BY comment_ID SEPARATOR ';') FROM wp_comments",
		"SELECT GROUP_CONCAT(CONCAT_WS('/', option_name, option_value) ORDER BY option_name SEPARATOR ';') FROM wp_options WHERE option_name NOT IN ('rewrite_rules', 'siteurl')",
	} {
		parts = append(parts, one(t, db, q))
	}
	return strings.Join(parts, "\n")
}

// TestBranchSyncProperty は、段階 94 の完了条件のプロパティ: 競合のない書き込みを本番、a、c に入れ、a のマージ、c のマージ、
// GC を挟みながら、片方の世界でだけ c に取り込む。最後に c をマージした本番は、両方の世界で同じ。取り込んだ直後の c は、
// c だけが変える列（本文）は取り込まない世界の c と同じ、本番だけが変える列（題名）は本番と同じ。
func TestBranchSyncProperty(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			worlds := [2]*syncWorld{newSyncWorld(t), newSyncWorld(t)}
			defer worlds[0].stop()
			defer worlds[1].stop()
			// 題名は本番だけ、本文と c の記事は c だけ、抜粋は a だけが変える（競合しない）。本番が消すのは本番が足した記事だけ、
			// c が変えるのは最初の記事と c が足した記事だけ。
			seedPosts := []string{"2", "3", "4"}
			var mainPosts, cPosts []string
			nextMain, nextC := 5, int64(20000000000)
			exec := func(db func(*syncWorld) *sql.DB, q string) {
				for _, w := range worlds {
					mustExec(t, db(w), q)
				}
			}
			mainOf := func(w *syncWorld) *sql.DB { return w.main }
			aOf := func(w *syncWorld) *sql.DB { return w.a }
			cOf := func(w *syncWorld) *sql.DB { return w.c }
			pick := func(ids []string) string { return ids[rng.Intn(len(ids))] }
			for step := 0; step < 40; step++ {
				n := fmt.Sprint(step)
				switch op := rng.Intn(12); op {
				case 0, 1:
					exec(mainOf, "UPDATE wp_posts SET post_title = 'tm"+n+"' WHERE ID = "+pick(append(append([]string{}, seedPosts...), cPosts...)))
				case 2:
					id := fmt.Sprint(nextMain)
					nextMain++
					exec(mainOf, insertPost+"("+id+", 1, 'm"+n+"', 'publish', 'post', 'mc"+n+"', '', '', '', '')")
					mainPosts = append(mainPosts, id)
				case 3:
					exec(mainOf, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES ("+pick(seedPosts)+", 'r', 'k"+n+"', '1')")
				case 4:
					if len(mainPosts) > 0 {
						i := rng.Intn(len(mainPosts))
						exec(mainOf, "DELETE FROM wp_posts WHERE ID = "+mainPosts[i])
						mainPosts = append(mainPosts[:i], mainPosts[i+1:]...)
					}
				case 5:
					exec(mainOf, "INSERT INTO wp_options (option_name, option_value) VALUES ('m_"+n+"', 'v') ON DUPLICATE KEY UPDATE option_value = 'w'")
				case 6, 7:
					exec(cOf, "UPDATE wp_posts SET post_content = 'cc"+n+"' WHERE ID = "+pick(append(append([]string{}, seedPosts...), cPosts...)))
				case 8:
					id := fmt.Sprint(nextC)
					nextC++
					exec(cOf, insertPost+"(NULL, 1, 'c"+n+"', 'publish', 'post', 'cc"+n+"', '', '', '', '')")
					exec(cOf, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES ("+id+", 'k', 'v"+n+"')")
					cPosts = append(cPosts, id)
				case 9:
					exec(aOf, "UPDATE wp_posts SET post_excerpt = 'ea"+n+"' WHERE ID = "+pick(seedPosts))
					exec(mainOf, "CALL heimdb_branch_merge('a')")
				case 10:
					exec(mainOf, "CALL heimdb_branch_merge('c')")
				case 11:
					time.Sleep(2 * time.Millisecond)
					for _, w := range worlds {
						if _, err := w.st.GC(time.Millisecond); err != nil {
							t.Fatal(err)
						}
					}
				}
				// 取り込むのは片方の世界だけ。
				if rng.Intn(4) == 0 {
					mustExec(t, worlds[0].main, "CALL heimdb_branch_sync('c')")
					// 最初の記事と c の記事の本文は、取り込まない世界の c と同じ（c の変更を消さない）。
					q := "SELECT GROUP_CONCAT(CONCAT(ID, '/', post_content) ORDER BY ID) FROM wp_posts WHERE ID IN (2, 3, 4) OR ID >= 20000000000"
					if got, want := one(t, worlds[0].c, q), one(t, worlds[1].c, q); got != want {
						t.Fatalf("step %d: content on c after sync:\n%s\nwant:\n%s", step, got, want)
					}
					// 本番にある記事（マージで入った c の記事を含む）の題名と抜粋は、本番と同じ。本番が足した記事と消した記事も同じ。
					ids := one(t, worlds[0].main, "SELECT GROUP_CONCAT(ID) FROM wp_posts")
					if got, want := one(t, worlds[0].c, "SELECT GROUP_CONCAT(ID ORDER BY ID) FROM wp_posts WHERE ID < 20000000000"),
						one(t, worlds[0].main, "SELECT GROUP_CONCAT(ID ORDER BY ID) FROM wp_posts WHERE ID < 20000000000"); got != want {
						t.Fatalf("step %d: posts on c after sync: %s, want %s", step, got, want)
					}
					q = "SELECT GROUP_CONCAT(CONCAT(ID, '/', post_title, '/', post_excerpt) ORDER BY ID) FROM wp_posts WHERE ID IN (" + ids + ")"
					if got, want := one(t, worlds[0].c, q), one(t, worlds[0].main, q); got != want {
						t.Fatalf("step %d: titles on c after sync:\n%s\nwant (main):\n%s", step, got, want)
					}
				}
			}
			exec(mainOf, "CALL heimdb_branch_merge('c')")
			if got, want := worlds[0].snapshot(t, worlds[0].main), worlds[1].snapshot(t, worlds[1].main); got != want {
				t.Errorf("main after the last merge:\n%s\nwant (without syncing):\n%s", got, want)
			}
		})
	}
}

// TestBranchSyncConcurrent: 本番に書き込みながら取り込み、同じブランチのマージとも同時に流す。取り込みは本番をロック
// しないので本番の書き込みは止まらず、ブランチごとの鍵でマージと取り込みの基準の記録は食い違わない。最後に取り込むと、
// 本番のコメントがすべてブランチに入り、ブランチの変更はすべて本番に入る。
func TestBranchSyncConcurrent(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	done := make(chan error, 3)
	go func() {
		for i := 0; i < 40; i++ {
			if _, err := mainDB.Exec(fmt.Sprintf("INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (2, 'r', 'k%d', '1')", i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		for i := 0; i < 8; i++ {
			if _, err := c.Exec(fmt.Sprintf("UPDATE wp_posts SET post_content = 'c%d' WHERE ID = 3", i)); err != nil {
				done <- err
				return
			}
			if _, err := mainDB.Exec("CALL heimdb_branch_sync('c')"); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		for i := 0; i < 8; i++ {
			if _, err := mainDB.Exec("CALL heimdb_branch_merge('c')"); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	if got := one(t, c, "SELECT CONCAT(COUNT(*), '/', MAX(comment_content)) FROM wp_comments"); got != "40/k9" {
		t.Errorf("comments on c: %s", got)
	}
	if got := one(t, mainDB, "SELECT post_content FROM wp_posts WHERE ID = 3"); got != "c7" {
		t.Errorf("main: %s", got)
	}
	if rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('c')"); len(rows) != 0 {
		t.Errorf("diff after the last sync and merge: %v", rows)
	}
}

// TestBranchSyncStaleResolutions: 取り込みの前に記録した解決（ワイルドカード）は、取り込んだ後の競合には使わない。
func TestBranchSyncStaleResolutions(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('c', 'wp_posts', '*', 'branch')")
	diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	mustExec(t, c, "UPDATE wp_posts SET post_title = 'About (c)' WHERE ID = 2")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'About (main)' WHERE ID = 2")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('c')"); sqlErrNum(err) != 51004 {
		t.Fatalf("merge with a resolution recorded before the sync: %v", err)
	}
	// 取り込みの後に記録した解決は使う。
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('c', 'wp_posts', 'ID=2', 'branch')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('c')")
	if got := one(t, mainDB, "SELECT post_title FROM wp_posts WHERE ID = 2"); got != "About (c)" {
		t.Errorf("main: %s", got)
	}
}

// TestBranchSyncDependents: 競合でブランチの側を採って親の記事を入れなかったら（ブランチで消した記事を本番が変えた）、
// その記事に本番で付いたメタとコメントも写さない。マージから外した表は excluded の行に出す。
func TestBranchSyncDependents(t *testing.T) {
	mainDB, _, br, stop := syncFixture(t)
	defer stop()
	c := br["c"]
	mustExec(t, mainDB, "CREATE TABLE wp_cache (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))")
	diffRows(t, mainDB, "CALL heimdb_branch_sync('c')")
	mustExec(t, c, "DELETE FROM wp_posts WHERE ID = 3")
	for _, q := range []string{
		"UPDATE wp_posts SET post_title = 'Contact (main)' WHERE ID = 3",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (3, 'k', 'v'), (2, 'k', 'v')",
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (3, 'r', 'on 3', '1'), (2, 'r', 'on 2', '1')",
		"INSERT INTO wp_cache (v) VALUES ('x')",
		"CALL heimdb_merge_exclude('wp_cache')",
	} {
		mustExec(t, mainDB, q)
	}
	rows := diffRows(t, mainDB, "CALL heimdb_branch_sync('c', 'branch')")
	got := strings.Join(rows[:len(rows)-1], "\n")
	want := strings.Join([]string{
		"wp_cache|excluded|0|0|0|0|0|0|0|matches the pattern 'wp_cache'; the branch's table is kept as it is",
		"wp_comments|synced|1|0|0|1|0|0|0|",
		"wp_postmeta|synced|1|0|0|1|0|0|0|",
		"wp_posts|synced|0|0|0|0|0|1|0|",
	}, "\n")
	if got != want {
		t.Errorf("sync:\n%s\nwant:\n%s", got, want)
	}
	if got := one(t, c, "SELECT CONCAT((SELECT COUNT(*) FROM wp_postmeta WHERE post_id = 3), '/', (SELECT COUNT(*) FROM wp_comments WHERE comment_post_ID = 3), '/', (SELECT COUNT(*) FROM wp_cache))"); got != "0/0/0" {
		t.Errorf("c: %s", got)
	}
}
