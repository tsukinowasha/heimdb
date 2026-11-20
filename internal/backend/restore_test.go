package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// markLocal は mark と同じだが、サーバの地域時刻（セッションの time_zone の既定値 SYSTEM）で返す。
func markLocal(t *testing.T) string {
	t.Helper()
	time.Sleep(2 * time.Millisecond)
	ts := time.Now().Format("2006-01-02 15:04:05.000000")
	time.Sleep(2 * time.Millisecond)
	return ts
}

// tableState は、テーブルの全行を主キーの順に 1 つの文字列にする（at が空でなければ AS OF at）。
func tableState(t *testing.T, db *sql.DB, table, at string) string {
	t.Helper()
	q := "SELECT * FROM " + table
	if at != "" {
		q += " AS OF '" + at + "'"
	}
	return strings.Join(diffRows(t, db, q), "\n")
}

func TestRestore(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_links (link_id, link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES (1, 'u', 'a', '', '', '', 'Y', 1, 0, '', '', '')")
	mustExec(t, mainDB, "INSERT INTO wp_termmeta (term_id, meta_key, meta_value) VALUES (1, 'k', 'v')")
	restored := []string{"wp_options", "wp_posts", "wp_postmeta", "wp_terms", "wp_links", "wp_term_taxonomy", "wp_term_relationships"}
	at := map[string]string{}
	tAt := markLocal(t)
	for _, n := range restored {
		at[n] = tableState(t, mainDB, n, "")
	}
	termmetaAt := tableState(t, mainDB, "wp_termmeta", "")

	// プラグインの更新で壊れたような変更と、その間に付いたコメント。
	for _, q := range []string{
		"UPDATE wp_posts SET post_title = 'About (broken)', post_name = 'about-broken' WHERE ID = 2",
		"DELETE FROM wp_posts WHERE ID = 3",
		insertPost + "(NULL, 1, 'New', 'publish', 'post', '', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (2, 'k', 'v')",
		// option_name の一意制約: 消して同じ名前で作り直す（option_id が変わる）。
		"DELETE FROM wp_options WHERE option_name = 'blogname'",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('blogname', 'Broken'), ('plugin_x', '1')",
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content) VALUES (4, 'reader', 'on a post the restore deletes')",
		"UPDATE wp_posts SET comment_count = 1 WHERE ID = 4", // WordPress はコメント数も更新する
		"CREATE TABLE wp_plugin (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_plugin (v) VALUES ('a')",
		"TRUNCATE wp_terms",
		"INSERT INTO wp_terms (term_id, name, slug) VALUES (5, 'x', 'x')",
		"ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0",
		"INSERT INTO wp_links (link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES ('u', 'b', '', '', '', 'Y', 1, 0, '', '', '')",
		"DROP TABLE wp_termmeta",
		// インデックスを消した後で行を変える（T の定義に戻すとき、インデックスを作り直す必要がある）。
		"ALTER TABLE wp_term_taxonomy DROP INDEX term_id_taxonomy",
		"UPDATE wp_term_taxonomy SET taxonomy = 'post_tag' WHERE term_taxonomy_id = 1",
	} {
		mustExec(t, mainDB, q)
	}
	before := map[string]string{}
	for _, n := range append(restored, "wp_plugin", "wp_comments") {
		before[n] = tableState(t, mainDB, n, "")
	}

	preview := strings.Join(diffRows(t, mainDB, "CALL heimdb_restore_preview('main', '"+tAt+"')"), "\n")
	want := strings.Join([]string{
		"wp_links|restore definition|0|0|0|",
		"wp_options|restore rows|1|0|2|",
		"wp_plugin|drop|0|0|0|",
		"wp_postmeta|restore rows|0|0|1|",
		"wp_posts|restore rows|1|1|1|1 comments remain on post 4, which the restore deletes",
		"wp_term_taxonomy|restore definition|0|1|0|",
		"wp_termmeta|restore definition|0|0|0|",
		"wp_terms|restore definition|0|0|0|",
	}, "\n")
	if preview != want {
		t.Errorf("preview:\n%s\nwant:\n%s", preview, want)
	}

	out := diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"')")
	undo := out[len(out)-1]
	if !strings.HasPrefix(undo, "*|undo|0|0|0|CALL heimdb_restore('main', '") {
		t.Fatalf("undo row: %s", undo)
	}
	for _, n := range restored {
		if got := tableState(t, mainDB, n, ""); got != at[n] {
			t.Errorf("%s after restore:\n%s\nwant:\n%s", n, got, at[n])
		}
	}
	if got := tableState(t, mainDB, "wp_termmeta", ""); got != termmetaAt {
		t.Errorf("wp_termmeta after restore: %s", got)
	}
	if _, err := mainDB.Exec("SELECT * FROM wp_plugin"); err == nil {
		t.Error("wp_plugin should be dropped")
	}
	if got := tableState(t, mainDB, "wp_comments", ""); got != before["wp_comments"] {
		t.Errorf("comments should be kept:\n%s", got)
	}
	// 作り直したインデックスで引ける。
	for q, want := range map[string]string{
		"SELECT term_taxonomy_id FROM wp_term_taxonomy WHERE term_id = 1 AND taxonomy = 'category'": "1",
		"SELECT COUNT(*) FROM wp_term_taxonomy WHERE term_id = 1 AND taxonomy = 'post_tag'":         "0",
		"SELECT COUNT(*) FROM wp_posts WHERE post_name = ''":                                        "3",
		"SELECT COUNT(*) FROM wp_posts WHERE post_name = 'about-broken'":                            "0",
		"SELECT option_value FROM wp_options WHERE option_name = 'blogname'":                        "Prod",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	// AUTO_INCREMENT は戻らない（T の後に払い出した ID を再利用しない）。
	mustExec(t, mainDB, insertPost+"(NULL, 1, 'After', 'publish', 'post', '', '', '', '', '')")
	mustExec(t, mainDB, "INSERT INTO wp_links (link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES ('u', 'c', '', '', '', 'Y', 1, 0, '', '', '')")
	for q, want := range map[string]string{
		"SELECT ID FROM wp_posts WHERE post_title = 'After'": "5",
		"SELECT link_id FROM wp_links WHERE link_name = 'c'": "3",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	mustExec(t, mainDB, "DELETE FROM wp_posts WHERE post_title = 'After'")
	mustExec(t, mainDB, "DELETE FROM wp_links WHERE link_name = 'c'")

	// 復元を取り消すと、復元の直前の状態に戻る。
	undoCall := strings.TrimPrefix(undo, "*|undo|0|0|0|")
	diffRows(t, mainDB, undoCall)
	for _, n := range append(restored, "wp_plugin", "wp_comments") {
		if got := tableState(t, mainDB, n, ""); got != before[n] {
			t.Errorf("%s after undo:\n%s\nwant:\n%s", n, got, before[n])
		}
	}
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_term_taxonomy WHERE term_id = 1 AND taxonomy = 'post_tag'"); got != "1" {
		t.Errorf("index after undo: %s", got)
	}
}

func TestRestoreSelectedTablesAndAll(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'X' WHERE option_name = 'blogname'")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'X' WHERE ID = 2")
	mustExec(t, mainDB, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content) VALUES (2, 'a', 'c')")
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', 'wp_options')")
	if got := one(t, mainDB, "SELECT CONCAT((SELECT option_value FROM wp_options WHERE option_name = 'blogname'), '/', (SELECT post_title FROM wp_posts WHERE ID = 2))"); got != "Prod/X" {
		t.Errorf("only wp_options should be restored: %s", got)
	}
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', '*')")
	if got := one(t, mainDB, "SELECT CONCAT((SELECT post_title FROM wp_posts WHERE ID = 2), '/', (SELECT COUNT(*) FROM wp_comments))"); got != "About/0" {
		t.Errorf("'*' should restore all tables: %s", got)
	}
	if _, err := mainDB.Exec("CALL heimdb_restore('main', '" + tAt + "', 'wp_nope')"); err == nil {
		t.Error("unknown table should fail")
	}
	if _, err := mainDB.Exec("CALL heimdb_restore('main', '2999-01-01 00:00:00')"); err == nil {
		t.Error("a future time should fail")
	}
}

// ブランチの復元。分岐より前の時刻なら、親のその時刻の状態に戻る。親は変わらない。
func TestRestoreBranch(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'Main2' WHERE option_name = 'blogname'")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "INSERT INTO wp_options (option_name, option_value) VALUES ('staging_only', '1')")
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'Main3' WHERE option_name = 'blogname'")
	diffRows(t, staging, "CALL heimdb_restore('staging', '"+tAt+"')")
	if got := one(t, staging, "SELECT CONCAT(option_value, '/', (SELECT COUNT(*) FROM wp_options WHERE option_name = 'staging_only')) FROM wp_options WHERE option_name = 'blogname'"); got != "Prod/0" {
		t.Errorf("staging after restore: %s", got)
	}
	if got := one(t, mainDB, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Main3" {
		t.Errorf("main changed: %s", got)
	}
}

// ブランチの書き込みロックを持ったトランザクションの中からは復元できない。
func TestRestoreInsideTransaction(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	tAt := markLocal(t)
	ctx := context.Background()
	c, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{"BEGIN", "UPDATE wp_posts SET post_title = 'y' WHERE ID = 3"} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ExecContext(ctx, "CALL heimdb_restore('main', '"+tAt+"')"); err == nil || !strings.Contains(err.Error(), "commit or roll back") {
		t.Errorf("restore inside a transaction: %v", err)
	}
	if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

// コメントを戻さずに記事を戻すと、戻した記事のコメント数を今のコメントで数え直す。
func TestRestoreRecountsComments(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'Broken' WHERE ID = 2")
	mustExec(t, mainDB, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (2, 'r', 'ok', '1'), (2, 'r', 'spam', 'spam')")
	mustExec(t, mainDB, "UPDATE wp_posts SET comment_count = 1 WHERE ID = 2")
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"')")
	if got := one(t, mainDB, "SELECT CONCAT(post_title, '/', comment_count) FROM wp_posts WHERE ID = 2"); got != "About/1" {
		t.Errorf("post 2 after restore: %s", got)
	}
}
