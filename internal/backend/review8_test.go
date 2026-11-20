package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// レビュー指摘 4: 取り消しの時刻は、夏時間の終わりの重なる 1 時間でも、別の time_zone のセッションでも、元の時刻に戻る
// （UTC のオフセット付きで出す）。
func TestReview8RestoreUndoTimeDST(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "SET time_zone = 'America/New_York'")
	_, _ = time.LoadLocation("America/New_York")
	// 2026-11-01 01:30 EST（夏時間が終わった後の 2 回目の 01:30）。
	now := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // = 01:30 EST
	formatted := formatRestoreTime(uint64(now.UnixMicro()))
	ts, err := parseAsOf(s.ctx, formatted)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.UnixMicro(int64(ts)).UTC(); !got.Equal(now) {
		t.Errorf("undo time %q round-trips to %s, want %s (off by %s)", formatted, got, now, got.Sub(now))
	}
}

// レビュー指摘 1: T の後に一意な値を 2 行で入れ替えても、復元が途中で一意制約に当たらない。
func TestReview8RestoreUniqueSwap(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('a', '1'), ('b', '2')")
	tAt := markLocal(t)
	want := tableState(t, mainDB, "wp_options", "")
	for _, q := range []string{
		"UPDATE wp_options SET option_name = 'tmp' WHERE option_name = 'a'",
		"UPDATE wp_options SET option_name = 'a' WHERE option_name = 'b'",
		"UPDATE wp_options SET option_name = 'b' WHERE option_name = 'tmp'",
	} {
		mustExec(t, mainDB, q)
	}
	if _, err := mainDB.Exec("CALL heimdb_restore('main', '" + tAt + "', 'wp_options')"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := tableState(t, mainDB, "wp_options", ""); got != want {
		t.Errorf("after restore:\n%s\nwant:\n%s", got, want)
	}
}

// レビュー指摘 2: T の後に RENAME したテーブルの旧名だけを復元すると、2 つの名前が同じテーブル ID を指してしまうので、
// 両方の名前を指定するよう求めるエラーにする。両方を指定すれば戻る。
func TestReview8RestoreRenamedPartial(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_links (link_id, link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES (1, 'u', 'a', '', '', '', 'Y', 1, 0, '', '', '')")
	tAt := markLocal(t)
	mustExec(t, mainDB, "RENAME TABLE wp_links TO wp_links_old")
	if _, err := mainDB.Exec("CALL heimdb_restore('main', '" + tAt + "', 'wp_links')"); err == nil || !strings.Contains(err.Error(), "include both") {
		t.Fatalf("restoring one name of a renamed table: %v", err)
	}
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', 'wp_links,wp_links_old')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_links"); got != "1" {
		t.Errorf("wp_links after restore: %s", got)
	}
	if _, err := mainDB.Exec("SELECT * FROM wp_links_old"); err == nil {
		t.Error("wp_links_old should be gone")
	}
}

// レビュー指摘 3: 何も変わらない復元は、何も書かず、取り消しの行も出さない。
func TestReview8RestoreNoopUndo(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	tAt := markLocal(t)
	if out := diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', 'wp_options')"); len(out) != 0 {
		t.Errorf("no-op restore: %v", out)
	}
}

// レビューの確認用: ブランチで T の後にインデックスを消して行を変え、T に戻す。親の層の古いエントリが隠れ、
// 作り直したインデックスで引ける。
func TestReview8RestoreBranchRebuildsIndex(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	_ = mainDB
	tAt := markLocal(t)
	for _, q := range []string{
		"ALTER TABLE wp_term_taxonomy DROP INDEX term_id_taxonomy",
		"UPDATE wp_term_taxonomy SET taxonomy = 'post_tag' WHERE term_taxonomy_id = 1",
		"INSERT INTO wp_term_taxonomy (term_taxonomy_id, term_id, taxonomy, description, count) VALUES (2, 1, 'category', '', 0)",
	} {
		mustExec(t, staging, q)
	}
	diffRows(t, staging, "CALL heimdb_restore('staging', '"+tAt+"')")
	for q, want := range map[string]string{
		"SELECT GROUP_CONCAT(term_taxonomy_id) FROM wp_term_taxonomy WHERE term_id = 1 AND taxonomy = 'category'": "1",
		"SELECT COUNT(*) FROM wp_term_taxonomy WHERE term_id = 1 AND taxonomy = 'post_tag'":                       "0",
		"SELECT COUNT(*) FROM wp_term_taxonomy":                                                                   "1",
	} {
		if got := one(t, staging, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	// 復元の後にも一意インデックスが効く。
	if _, err := staging.Exec("INSERT INTO wp_term_taxonomy (term_taxonomy_id, term_id, taxonomy, description, count) VALUES (3, 1, 'category', '', 0)"); err == nil {
		t.Error("unique index term_id_taxonomy should reject a duplicate after restore")
	}
}

// レビューの確認用: 親で列を変えた（テーブル ID が変わった）後に分岐し、分岐より前の時刻に戻す。
func TestReview8RestoreBranchBeforeForkOldID(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_links (link_id, link_url, link_name, link_image, link_target, link_description, link_visible, link_owner, link_rating, link_rel, link_notes, link_rss) VALUES (1, 'u', 'a', '', '', '', 'Y', 1, 0, '', '', '')")
	tAt := markLocal(t)
	want := tableState(t, mainDB, "wp_links", "")
	mustExec(t, mainDB, "UPDATE wp_links SET link_name = 'a2' WHERE link_id = 1")
	mustExec(t, mainDB, "ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE wp_links SET link_name = 'a3' WHERE link_id = 1")
	diffRows(t, staging, "CALL heimdb_restore('staging', '"+tAt+"', 'wp_links')")
	if got := tableState(t, staging, "wp_links", ""); got != want {
		t.Errorf("staging after restore:\n%s\nwant:\n%s", got, want)
	}
	if got := tableState(t, staging, "wp_links", tAt); got != want {
		t.Errorf("AS OF on staging:\n%s\nwant:\n%s", got, want)
	}
}

// レビュー指摘 5: コメントだけを戻すと、記事のコメント数を戻した後のコメントで数え直す。
func TestReview8RestoreCommentsOnlyRecounts(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	tAt := markLocal(t)
	mustExec(t, mainDB, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (2, 'r', 'spam that got approved', '1')")
	mustExec(t, mainDB, "UPDATE wp_posts SET comment_count = 1 WHERE ID = 2")
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', 'wp_comments')")
	if got := one(t, mainDB, "SELECT CONCAT((SELECT COUNT(*) FROM wp_comments), '/', comment_count) FROM wp_posts WHERE ID = 2"); got != "0/0" {
		t.Errorf("comments/comment_count after restoring comments only: %s", got)
	}
}

// 復元をまたいで開いているトランザクションは、戻した行への書き込みで 1213、定義を戻したテーブルへの書き込みで 1412 になる。
func TestReview8RestoreAcrossOpenTransaction(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'changed' WHERE ID = 2")
	mustExec(t, mainDB, "TRUNCATE wp_terms")
	ctx := context.Background()
	open := func() *sql.Conn {
		c := conn(t, mainDB)
		exec(t, c, "BEGIN")
		queryString(t, c, "SELECT COUNT(*) FROM wp_posts")
		queryString(t, c, "SELECT COUNT(*) FROM wp_terms")
		return c
	}
	c1, c2 := open(), open()
	defer c1.Close()
	defer c2.Close()
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"')")
	if _, err := c1.ExecContext(ctx, "UPDATE wp_posts SET post_content = 'x' WHERE ID = 2"); mysqlErrNo(err) != 1213 {
		t.Errorf("write to a restored row: %v", err)
	}
	exec(t, c1, "ROLLBACK") // 書き込みロックを放す
	if _, err := c2.ExecContext(ctx, "INSERT INTO wp_terms (term_id, name, slug) VALUES (9, 'n', 'n')"); mysqlErrNo(err) != 1412 {
		t.Errorf("write to a table whose definition was restored: %v", err)
	}
}

// 親を復元した後の子ブランチの差分とマージ: 親の復元は親の変更として扱われ、別の列なら両方が残る。
func TestReview8RestoreParentThenMergeChild(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, mainDB, insertPost+"(3, 0, 'Contact', 'publish', 'page', 'c', '', '', '', '')")
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'Contact (broken)' WHERE ID = 3")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE wp_posts SET post_content = 'staging body' WHERE ID = 3")
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"')")
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_posts')"), "\n"); got != "wp_posts|ID=3|update|update|update||post_content|" {
		t.Errorf("diff after restoring the parent:\n%s", got)
	}
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT CONCAT(post_title, '/', post_content) FROM wp_posts WHERE ID = 3"); got != "Contact/staging body" {
		t.Errorf("main after merge: %s", got)
	}
}

// 定義を戻す復元で、インデックスを消す前に変えた行も、作り直したインデックスで戻した値で引ける（行を読み直す問い合わせ、
// インデックスだけで答える問い合わせ、一意の確かめ）。作り直しを、エントリを集めずに流す形にした（docs/plan.md の段階 59）。
func TestRestoreRebuildRemovesStaleEntries(t *testing.T) {
	_, staging, stop := mergeFixture(t)
	defer stop()
	tAt := markLocal(t)
	for _, q := range []string{
		"UPDATE wp_term_taxonomy SET taxonomy = 'post_tag' WHERE term_taxonomy_id = 1", // インデックスのエントリも変わる
		"ALTER TABLE wp_term_taxonomy DROP INDEX term_id_taxonomy",
	} {
		mustExec(t, staging, q)
	}
	diffRows(t, staging, "CALL heimdb_restore('staging', '"+tAt+"')")
	for q, want := range map[string]string{
		"SELECT COUNT(*) FROM wp_term_taxonomy FORCE INDEX (term_id_taxonomy) WHERE term_id = 1 AND taxonomy = 'post_tag'": "0",
		"SELECT COUNT(*) FROM wp_term_taxonomy FORCE INDEX (term_id_taxonomy) WHERE term_id = 1 AND taxonomy = 'category'": "1",
		// インデックスだけで答える（行を読み直さない）問い合わせ
		"SELECT GROUP_CONCAT(taxonomy) FROM wp_term_taxonomy FORCE INDEX (term_id_taxonomy) WHERE term_id = 1": "category",
		"SELECT COUNT(*) FROM wp_term_taxonomy FORCE INDEX (term_id_taxonomy) WHERE term_id = 1":               "1",
	} {
		if got := one(t, staging, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	if _, err := staging.Exec("INSERT INTO wp_term_taxonomy (term_taxonomy_id, term_id, taxonomy, description, count) VALUES (3, 1, 'post_tag', '', 0)"); err != nil {
		t.Errorf("a stale entry of the rebuilt unique index refused a new row: %v", err)
	}
}
