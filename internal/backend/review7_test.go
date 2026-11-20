package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// レビュー用の再現テスト（段階 7）。

// レビュー指摘 1: 1 回目のマージで方針により反映しなかった行（auto-draft）を、その後ブランチで公開すると、
// 2 回目の差分は insert ではなく「changed on the branch, absent on the parent」の競合になる。
func TestReview7AutoDraftPublishedAfterMerge(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	mustExec(t, staging, insertPost+"(NULL, 1, 'Auto Draft', 'auto-draft', 'post', '', '', '', '', '')")
	// 自動下書きの間に付いたメタ（その後は変わらない）。
	mustExec(t, staging, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000000, 'early', 'x')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_posts WHERE ID = 10000000000"); got != "0" {
		t.Fatalf("auto-draft merged: %s", got)
	}
	// WordPress は「新規追加」で作った auto-draft の行を UPDATE して公開する。
	mustExec(t, staging, "UPDATE wp_posts SET post_status = 'publish', post_title = 'Real post', post_name = 'real-post' WHERE ID = 10000000000")
	mustExec(t, staging, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000000, '_wp_page_template', 'x')")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
	for _, want := range []string{
		"wp_posts|ID=10000000000|insert|none|insert|",
		"wp_postmeta|meta_id=10000000000|insert|none|insert|",
		"wp_postmeta|meta_id=10000000001|insert|none|insert|",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("diff lacks %q:\n%s", want, rows)
		}
	}
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT CONCAT(post_title, '/', (SELECT GROUP_CONCAT(meta_key ORDER BY meta_id) FROM wp_postmeta WHERE post_id = 10000000000)) FROM wp_posts WHERE ID = 10000000000"); got != "Real post/early,_wp_page_template" {
		t.Errorf("published post after merge: %q", got)
	}
}

// 前のマージで親の側で解決した（作らなかった）重複の記事を、ブランチで後から編集しても、重複として競合のまま。
// ぶら下がるメタも反映しない。
func TestReview7ParentResolvedDuplicateEditedLater(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	mustExec(t, staging, insertPost+"(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{}', '', '', '', '')")
	mustExec(t, staging, "UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE ID = 10000000000")
	mustExec(t, mainDB, insertPost+"(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{\"p\":1}', '', '', '', '')")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE ID = 4")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=10000000000', 'parent')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	mustExec(t, staging, "UPDATE wp_posts SET post_content = '{\"v\":2}' WHERE ID = 10000000000")
	mustExec(t, staging, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000000, 'm', 'v')")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
	for _, want := range []string{
		"wp_posts|ID=10000000000|insert|insert|conflict||",
		"wp_postmeta|meta_id=10000000000|insert|none|skip|||belongs to a post that is not merged",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("diff lacks %q:\n%s", want, rows)
		}
	}
}

// レビュー指摘 2: 競合を解決した後に、親で比べない列（comment_count）だけが変わると、解決が stale になりマージが拒否される。
// 本番でコメントが付くたびに解決し直すことになる。
func TestReview7ResolutionStaleOnExcludedColumn(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'Contact (staging)' WHERE ID = 3")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'Contact (prod)' WHERE ID = 3")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=3', 'branch')")
	// 本番にコメントが付いた（WordPress は comment_count を更新する）。
	mustExec(t, mainDB, "UPDATE wp_posts SET comment_count = 1 WHERE ID = 3")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_posts')"), "\n")
	if !strings.Contains(rows, "wp_posts|ID=3|update|update|conflict|branch|") {
		t.Errorf("resolution should still apply (only an excluded column changed):\n%s", rows)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Errorf("merge: %v", err)
	}
}

// レビュー指摘 3: 両側で同じ ALTER TABLE ... ADD COLUMN ... DEFAULT を実行すると（dbDelta）、ブランチの基準の行には新しい列が
// なく（nil）、今の行には既定値があるので、ブランチが触っていない行がすべて「ブランチで変更」になる。
// 親がその行を消していれば競合になる。
func TestReview7SameAlterOnBothSides(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, mainDB, "INSERT INTO wp_links (link_id, link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES (1, 'u', 'a', '', '', '', 'Y', 1, 0, '', '', ''), (2, 'u', 'b', '', '', '', 'Y', 1, 0, '', '', '')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0")
	mustExec(t, mainDB, "ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0")
	mustExec(t, mainDB, "DELETE FROM wp_links WHERE link_id = 1")
	mustExec(t, mainDB, "UPDATE wp_links SET extra = 5 WHERE link_id = 2")
	rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')")
	if len(rows) != 0 {
		t.Errorf("the branch changed nothing, but the diff is:\n%s", strings.Join(rows, "\n"))
	}
}

// 確認用（通ることを期待）: 入れ子のブランチ、option_name での対応づけ（delete + add で option_id が変わる）、
// 削除と変更の競合を親の側で解決、2 回目の差分が空、本番の option_id の番号帯。
func TestReview7SanityNestedAndOptions(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717'), ('plugin_x', '1'), ('plugin_y', '1')",
		"INSERT INTO wp_users (ID, user_login, user_registered) VALUES (1, 'admin', '2026-01-01 00:00:00')",
		insertPost + "(2, 1, 'About', 'publish', 'page', 'about v1', '', '', '', ''), (3, 1, 'Contact', 'publish', 'page', 'c', '', '', '', '')",
		"CALL heimdb_branch_create('staging')",
		"CALL heimdb_branch_create('dev', 'staging')",
	} {
		mustExec(t, mainDB, q)
	}
	staging, dev := openBranch(t, dsn, "staging"), openBranch(t, dsn, "dev")
	defer staging.Close()
	defer dev.Close()
	for _, q := range []string{
		"DELETE FROM wp_options WHERE option_name = 'plugin_x'",
		"DELETE FROM wp_options WHERE option_name = 'plugin_y'",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('plugin_y', '2')",
		"UPDATE wp_posts SET post_title = 'About (dev)' WHERE ID = 2",
		"DELETE FROM wp_posts WHERE ID = 3",
	} {
		mustExec(t, dev, q)
	}
	mustExec(t, mainDB, "UPDATE wp_posts SET post_excerpt = 'prod' WHERE ID = 3")
	const state = "SELECT CONCAT((SELECT COUNT(*) FROM wp_options WHERE option_name = 'plugin_x'), '/', (SELECT option_value FROM wp_options WHERE option_name = 'plugin_y'), '/', (SELECT post_title FROM wp_posts WHERE ID = 2), '/', (SELECT COUNT(*) FROM wp_posts WHERE ID = 3))"
	mustExec(t, staging, "CALL heimdb_branch_merge('dev')")
	if got := one(t, staging, state); got != "0/2/About (dev)/0" {
		t.Errorf("dev -> staging: %s", got)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "wp_posts ID=3") {
		t.Fatalf("expected a delete/modify conflict: %v", err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=3', 'parent')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, state); got != "0/2/About (dev)/1" {
		t.Errorf("staging -> main: %s", got)
	}
	if rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"); len(rows) != 0 {
		t.Errorf("diff after merge:\n%s", strings.Join(rows, "\n"))
	}
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('after', '1')")
	if got := one(t, mainDB, "SELECT option_id < 10000000000 FROM wp_options WHERE option_name = 'after'"); got != "1" {
		t.Errorf("option_id after merge: %s", got)
	}
}

// レビュー指摘 4: 他の列でも競合している記事の post_author を、親にいないユーザーに変えたとき、著者の検査が行われず、
// ブランチの側で解決すると存在しない著者が本番に書かれる。
func TestReview7MissingAuthorOnConflictingUpdate(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'Contact (staging)', post_author = 9 WHERE ID = 3")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'Contact (prod)' WHERE ID = 3")
	if _, err := mainDB.Exec("CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=3', 'branch')"); err == nil {
		diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
		if got := one(t, mainDB, "SELECT post_author FROM wp_posts WHERE ID = 3"); got == "9" {
			t.Errorf("a missing author was written to the parent")
		}
	}
}

// 両側で同じ列を追加した後、ブランチがその列を変えた行は反映する（既定値と違うので変更とみなす）。
func TestReview7SameAlterBranchChangesNewColumn(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, mainDB, "INSERT INTO wp_links (link_id, link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES (1, 'u', 'a', '', '', '', 'Y', 1, 0, '', '', '')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0")
	mustExec(t, mainDB, "ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0")
	mustExec(t, staging, "UPDATE wp_links SET extra = 7 WHERE link_id = 1")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
	if rows != "wp_links|link_id=1|update|none|update||extra|" {
		t.Errorf("diff:\n%s", rows)
	}
}

// テーブルの接頭辞が wp_ でないサイト（$table_prefix）でも、WordPress の方針が効く。
func TestReview7CustomTablePrefix(t *testing.T) {
	dsn, stopServer := startServer(t, t.TempDir())
	defer stopServer()
	mainDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer mainDB.Close()
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		mustExec(t, mainDB, strings.ReplaceAll(q, "wp_", "blog_"))
	}
	mustExec(t, mainDB, "INSERT INTO blog_options (option_name, option_value) VALUES ('db_version', '60717'), ('siteurl', 'http://prod')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE blog_options SET option_value = 'http://staging' WHERE option_name = 'siteurl'")
	mustExec(t, staging, "INSERT INTO blog_comments (comment_post_ID, comment_author, comment_content) VALUES (1, 'a', 'x')")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
	for _, want := range []string{
		"blog_comments|comment_ID=10000000000|insert|none|skip|||branch changes to this table are not merged",
		"blog_options|option_name=siteurl|update|none|skip|||environment-specific option",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("diff lacks %q:\n%s", want, rows)
		}
	}
}
