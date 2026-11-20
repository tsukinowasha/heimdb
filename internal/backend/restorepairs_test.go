package backend

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	logtest "github.com/sirupsen/logrus/hooks/test"
)

// 組の表（記事とメタ、記事と分類、コメントとメタ、ユーザーとメタ）の片方だけを戻すときの警告（docs/plan.md の段階 62）。
func TestRestorePairedTables(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	for _, q := range []string{
		insertPost + "(4, 1, 'Edited', 'publish', 'post', '', '', '', '', ''), (5, 1, 'Locked', 'publish', 'post', '', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (3, '_thumbnail_id', '9'), (3, 'hero', 'red'), (2, '_builder_data', 'v1'), (4, 'views', '1')",
		"INSERT INTO wp_comments (comment_ID, comment_post_ID, comment_author, comment_content) VALUES (1, 2, 'r', 'c')",
		"INSERT INTO wp_commentmeta (comment_id, meta_key, meta_value) VALUES (1, 'rating', '3')",
		"INSERT INTO wp_usermeta (user_id, meta_key, meta_value) VALUES (1, 'nickname', 'admin')",
	} {
		mustExec(t, mainDB, q)
	}
	tAt := markLocal(t)
	for _, q := range []string{
		// 記事 3 を消す（wp_delete_post はメタと分類も消す）。
		"DELETE FROM wp_postmeta WHERE post_id = 3",
		"DELETE FROM wp_term_relationships WHERE object_id = 3",
		"DELETE FROM wp_posts WHERE ID = 3",
		// 記事 10〜16 を作る（10 は分類も付ける）。
		insertPost + "(10, 1, 'New', 'publish', 'post', '', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10, '_thumbnail_id', '9')",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (10, 1)",
		// 中身をメタに持つ記事 2 を編集する。
		"UPDATE wp_posts SET post_title = 'About v2' WHERE ID = 2",
		"UPDATE wp_postmeta SET meta_value = 'v2' WHERE post_id = 2 AND meta_key = '_builder_data'",
		// 記事 4 は記事だけ、記事 5 は記事と、編集や保存のたびに変わるメタだけが変わる（数えない）。
		"UPDATE wp_posts SET post_title = 'Edited v2' WHERE ID = 4",
		"UPDATE wp_postmeta SET meta_value = '2' WHERE post_id = 4", // 変えて戻したメタは数えない
		"UPDATE wp_postmeta SET meta_value = '1' WHERE post_id = 4",
		"UPDATE wp_posts SET post_title = 'Locked v2' WHERE ID = 5",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (5, '_edit_lock', '1:1'), (5, '_edit_last', '1'), (5, '_encloseme', '1'), (5, '_pingme', '1')",
		// インデックスを足しても、テーブル ID は同じなので行を比べる。
		"ALTER TABLE wp_posts ADD INDEX heimdb_test (menu_order)",
		// 復元で消える記事 10 に付いたコメント（コメントの警告。ログにも出る）。
		"INSERT INTO wp_comments (comment_ID, comment_post_ID, comment_author, comment_content) VALUES (2, 10, 'r', 'c')",
		// コメントとユーザーも、行とメタの両方が変わる。
		"UPDATE wp_comments SET comment_content = 'c2' WHERE comment_ID = 1",
		"UPDATE wp_commentmeta SET meta_value = '5' WHERE comment_id = 1",
		"UPDATE wp_users SET display_name = 'Admin' WHERE ID = 1",
		"UPDATE wp_usermeta SET meta_value = 'boss' WHERE user_id = 1",
	} {
		mustExec(t, mainDB, q)
	}
	for id := 11; id <= 16; id++ {
		mustExec(t, mainDB, insertPost+fmt.Sprintf("(%d, 1, 'More', 'publish', 'post', '', '', '', '', '')", id))
		mustExec(t, mainDB, fmt.Sprintf("INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (%d, 'k', 'v')", id))
	}

	at := regexp.MustCompile(`'\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{6}\+00:00'`)
	detail := func(call, table string) string {
		t.Helper()
		for _, r := range diffRows(t, mainDB, call) {
			if f := strings.Split(r, "|"); f[0] == table {
				return at.ReplaceAllString(f[5], "'T'")
			}
		}
		t.Fatalf("%s: no row for %s", call, table)
		return ""
	}
	const (
		postmeta = "posts 2, 3, 10, 11, 12 (and 4 more) also changed in wp_postmeta after the restore time, which is not restored"
		terms    = "posts 3, 10 also changed in wp_term_relationships after the restore time, which is not restored"
		together = "; to restore them together: CALL heimdb_restore('main', 'T', "
		remain   = "1 comments remain on post 10, which the restore deletes"
	)
	for _, c := range []struct{ tables, table, want string }{
		{"wp_posts", "wp_posts", remain + "; " + postmeta + "; " + terms + together + "'wp_posts,wp_postmeta,wp_term_relationships')"},
		{"wp_postmeta", "wp_postmeta", "posts 2, 3, 10, 11, 12 (and 4 more) also changed in wp_posts after the restore time, which is not restored" + together + "'wp_postmeta,wp_posts')"},
		{"wp_posts,wp_postmeta", "wp_posts", remain + "; " + terms + together + "'wp_postmeta,wp_posts,wp_term_relationships')"},
		{"wp_posts,wp_postmeta,wp_term_relationships", "wp_posts", remain},
		{"", "wp_posts", remain}, // 既定の復元はコメントとユーザーを組ごと残す
		{"*", "wp_posts", ""},
		{"wp_comments", "wp_comments", "comment 1 also changed in wp_commentmeta after the restore time, which is not restored" + together + "'wp_comments,wp_commentmeta')"},
		// 親を 2 つ選ぶと、当たった組の表をすべて加えた同じ呼び出しが付く。
		{"wp_posts,wp_comments", "wp_comments", "comment 1 also changed in wp_commentmeta after the restore time, which is not restored" + together + "'wp_comments,wp_posts,wp_postmeta,wp_term_relationships,wp_commentmeta')"},
		{"wp_usermeta", "wp_usermeta", "user 1 also changed in wp_users after the restore time, which is not restored" + together + "'wp_usermeta,wp_users')"},
	} {
		if got := detail("CALL heimdb_restore_preview('main', '"+tAt+"', '"+c.tables+"')", c.table); got != c.want {
			t.Errorf("preview %q:\n%s\nwant:\n%s", c.tables, got, c.want)
		}
	}

	// 書き直した子の表とは比べず、そのことを出す。
	mustExec(t, mainDB, "ALTER TABLE wp_commentmeta ADD COLUMN extra int NOT NULL DEFAULT 0")
	if got, want := detail("CALL heimdb_restore_preview('main', '"+tAt+"', 'wp_comments')", "wp_comments"), "wp_commentmeta is not restored and was rewritten, created or dropped after the restore time, so rows were not compared"+together+"'wp_comments,wp_commentmeta')"; got != want {
		t.Errorf("rewritten:\n%s\nwant:\n%s", got, want)
	}

	mustExec(t, mainDB, "ALTER TABLE wp_usermeta ADD COLUMN extra int NOT NULL DEFAULT 0")
	if got, want := detail("CALL heimdb_restore_preview('main', '"+tAt+"', 'wp_usermeta')", "wp_usermeta"), "wp_users is not restored, and wp_usermeta was rewritten, created or dropped after the restore time, so rows were not compared"+together+"'wp_usermeta,wp_users')"; got != want {
		t.Errorf("rewritten parent:\n%s\nwant:\n%s", got, want)
	}

	// 復元も同じ警告を出し、記事 3 はメタのない姿で戻る。警告の呼び出しで組の表も戻すと、メタと分類も戻る。
	hook := logtest.NewGlobal()
	defer hook.Reset()
	out := diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', 'wp_posts')")
	logged := false
	for _, e := range hook.AllEntries() {
		logged = logged || strings.HasPrefix(e.Message, "HeimDB: restore of main to ") && strings.Contains(e.Message, ": wp_posts: "+remain+"; posts 2, 3, 10")
	}
	if !logged {
		t.Error("the restore should log the warning")
	}
	if got := at.ReplaceAllString(strings.Split(out[0], "|")[5], "'T'"); got != remain+"; "+postmeta+"; "+terms+together+"'wp_posts,wp_postmeta,wp_term_relationships')" {
		t.Errorf("restore detail: %s", got)
	}
	if got := one(t, mainDB, "SELECT CONCAT((SELECT post_title FROM wp_posts WHERE ID = 3), '/', (SELECT COUNT(*) FROM wp_postmeta WHERE post_id = 3))"); got != "Contact/0" {
		t.Errorf("post 3 after restoring wp_posts: %s", got)
	}
	diffRows(t, mainDB, strings.SplitN(strings.Split(out[0], "|")[5], "to restore them together: ", 2)[1])
	if got := one(t, mainDB, "SELECT CONCAT((SELECT GROUP_CONCAT(meta_key ORDER BY meta_key) FROM wp_postmeta WHERE post_id = 3), '/', (SELECT COUNT(*) FROM wp_term_relationships WHERE object_id IN (3, 10)))"); got != "_thumbnail_id,hero/1" {
		t.Errorf("post 3 after restoring together: %s", got)
	}
}

// 組の片方の表がないサイトでは、比べずに戻す。T の後に作った表を消すときは、そのことを出す。
func TestRestorePairMissingTable(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	staging.Close()
	defer stop()
	mustExec(t, mainDB, "DROP TABLE wp_commentmeta")
	mustExec(t, mainDB, "INSERT INTO wp_comments (comment_ID, comment_post_ID, comment_author, comment_content) VALUES (1, 2, 'r', 'c')")
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_comments SET comment_content = 'c2' WHERE comment_ID = 1")
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_restore_preview('main', '"+tAt+"', 'wp_comments')"), "\n"); got != "wp_comments|restore rows|0|1|0|" {
		t.Errorf("preview: %s", got)
	}
	// T の後に作った表だけを戻す（消す）ときは、作ったことを出す。
	mustExec(t, mainDB, "CREATE TABLE wp_commentmeta (meta_id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, comment_id bigint unsigned NOT NULL DEFAULT 0, meta_key varchar(255), meta_value longtext)")
	got := diffRows(t, mainDB, "CALL heimdb_restore_preview('main', '"+tAt+"', 'wp_commentmeta')")
	at := regexp.MustCompile(`'\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{6}\+00:00'`)
	if want := "wp_commentmeta|drop|0|0|0|wp_comments is not restored, and wp_commentmeta was rewritten, created or dropped after the restore time, so rows were not compared; to restore them together: CALL heimdb_restore('main', 'T', 'wp_commentmeta,wp_comments')"; len(got) != 1 || at.ReplaceAllString(got[0], "'T'") != want {
		t.Errorf("preview of a table created after T: %q", got)
	}
}
