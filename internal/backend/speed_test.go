package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// 段階 28: 小さな速さの改善（文字列の定数との比較、インデックスの選び直し、ハッシュ JOIN のコスト）。

var speedScript = []string{
	// 文字列の列と文字列の定数の比較（定数を列の型にする）。照合順序で等しい違う値、NULL、定数が左
	"SELECT ID FROM wp_posts WHERE post_type = 'POST' ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE 'POST' = post_type ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_status <> 'PUBLISH' ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_status < 'p' ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_status >= 'Publish' ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_status <= 'inherit' ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_status > 'D' ORDER BY ID",
	"SELECT meta_id FROM wp_postmeta WHERE meta_key <=> '_THUMB' ORDER BY meta_id",
	"SELECT meta_id FROM wp_postmeta WHERE meta_key <> '_thumb' ORDER BY meta_id",
	"SELECT meta_id FROM wp_postmeta WHERE meta_value = 'A' ORDER BY meta_id",
	// 値が 1 つの IN と NOT IN（NULL の列を含む）
	"SELECT ID FROM wp_posts WHERE post_type IN ('POST') ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_type NOT IN ('post') ORDER BY ID",
	"SELECT meta_id FROM wp_postmeta WHERE meta_key NOT IN ('_thumb') ORDER BY meta_id",
	"SELECT meta_id FROM wp_postmeta WHERE meta_key IN ('_Thumb') ORDER BY meta_id",
	"SELECT meta_id FROM wp_postmeta WHERE meta_key NOT IN (NULL) ORDER BY meta_id",
	"SELECT ID FROM wp_posts WHERE post_type IN ('page', 'POST') ORDER BY ID",
	"SELECT COUNT(*) FROM wp_posts WHERE ( post_status = 'publish' ) AND post_type NOT IN ('page')",
	// 置き換えない比較（COLLATE の付いた定数、数値の定数、数値の列）
	"SELECT ID FROM wp_posts WHERE post_type = 'POST' COLLATE utf8mb4_bin ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_type = 'Post' COLLATE utf8mb4_bin ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_name = 1 ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_author = '1' ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_author IN ('2') ORDER BY ID",
	// UPDATE と DELETE の条件
	"UPDATE wp_posts SET post_title = 'U' WHERE post_status = 'PUBLISH' AND post_type NOT IN ('page')",
	"SELECT ID, post_title FROM wp_posts ORDER BY ID",
	"DELETE FROM wp_postmeta WHERE meta_key IN ('_THUMB')",
	"SELECT meta_id FROM wp_postmeta ORDER BY meta_id",
	// 投稿一覧（インデックスだけで読める type_status_date に替える）。同じ日時の記事の並び
	"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES " +
		"(3, '2024-02-01 00:00:00', 'c', 'S9', '', 'publish', 's9', '', '', '', 'post'), (1, '2024-02-01 00:00:00', 'c', 'S10', '', 'draft', 's10', '', '', '', 'post'), " +
		"(2, '2024-02-01 00:00:00', 'c', 'S11', '', 'future', 's11', '', '', '', 'post'), (1, '2024-02-01 00:00:00', 'c', 'S12', '', 'publish', 's12', '', '', '', 'post'), " +
		"(0, '2024-02-01 00:00:00', 'c', 'S13', '', 'trash', 's13', '', '', '', 'post'), (5, '2024-02-01 00:00:00', 'c', 'S14', '', 'Draft', 's14', '', '', '', 'post')",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.post_type = 'post' AND ((wp_posts.post_status <> 'trash' AND wp_posts.post_status <> 'auto-draft')) ORDER BY wp_posts.post_date DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish' OR wp_posts.post_status = 'draft')) ORDER BY wp_posts.post_date ASC LIMIT 0, 20",
	"SELECT ID, post_date FROM wp_posts WHERE post_type = 'post' ORDER BY post_date DESC, ID ASC",
}

// speedJoinData は、ハッシュ JOIN を選ぶ量の記事（300 件。公開と下書き）と term の関係（カテゴリ 1 に全部、
// タグ 2 に 3 件、ほかのタグに 3 件ずつ）。記事の ID は自動で振る（主キーを明示した複数行の INSERT の insert_id は
// MySQL と違う。段階 22）。
func speedJoinData() []string {
	var posts []string
	for i := 1; i <= 300; i++ {
		status := "publish"
		if i%4 == 0 {
			status = "draft"
		}
		posts = append(posts, fmt.Sprintf("(1, '2024-03-01 00:00:00', 'c', 'J%d', '', '%s', 'j%d', '', '', '', 'post')", i, status, i))
	}
	return []string{
		"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES " + strings.Join(posts, ", "),
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) SELECT ID, 1 FROM wp_posts WHERE post_name LIKE 'j%'",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) SELECT ID, 2 FROM wp_posts WHERE post_name IN ('j1', 'j2', 'j4')",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) SELECT ID, 10 + ID % 100 FROM wp_posts WHERE post_name LIKE 'j%'",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) SELECT ID, 10 + (ID + 1) % 100 FROM wp_posts WHERE post_name LIKE 'j%'",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) SELECT ID, 10 + (ID + 2) % 100 FROM wp_posts WHERE post_name LIKE 'j%'",
	}
}

var speedJoinQueries = []string{
	termCountQuery(1),
	termCountQuery(2),
	// ORDER BY のない JOIN の行の並び（term_relationships から読む。term 数の形の文は、MySQL は wp_posts から読むので
	// 並びが違う。段階 27 から）
	"SELECT wp_term_relationships.object_id, wp_posts.post_status FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_type = 'post' AND term_taxonomy_id = 1 AND post_name LIKE 'j29%'",
}

// 段階 28 の文の結果が、本物の MySQL と同じ。
func TestSpeedQueriesMatchMySQL(t *testing.T) {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	stmts = append(stmts, coveringScript[:3]...)
	stmts = append(stmts, speedScript...)
	stmts = append(stmts, speedJoinData()...)
	stmts = append(stmts, speedJoinQueries...)
	checkGolden(t, "../testdata/speed_golden.txt", "heimdb_golden28", stmts)
}

// 投稿一覧はインデックスだけで読める type_status_date を読み、記事の本体を引かない。term 数は、作る側（記事）を
// インデックスだけで読むハッシュ JOIN にし、少ない行のタグは主キーで引く lookup JOIN のまま。
func TestSpeedPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append(append([]string{wpSQLMode}, wpSchema()...), coveringScript[:3]...), speedJoinData()...) {
		mustExec(t, db, q)
	}
	postList := "SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.post_type = 'post' AND ((wp_posts.post_status <> 'trash' AND wp_posts.post_status <> 'auto-draft')) ORDER BY wp_posts.post_date DESC LIMIT 0, 20"
	checkPlan(t, db, "post list reads type_status_date", postList, []string{"index: [wp_posts.post_type,wp_posts.post_status,wp_posts.post_date,wp_posts.ID]", "TopN"}, nil) // 段階 83 から Project 越しの TopN
	before := pkRowsRead.Load()
	rows, err := db.Query(postList)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if read := pkRowsRead.Load() - before; read != 0 {
		t.Errorf("post list read %d rows, want 0 (index only)", read)
	}
	checkPlan(t, db, "a post title keeps the index of the statement", "SELECT post_title FROM wp_posts WHERE post_type = 'post' AND post_status <> 'trash' ORDER BY post_date DESC LIMIT 5",
		[]string{"index: [wp_posts.post_type,wp_posts.post_status,wp_posts.post_author]"}, nil)
	checkPlan(t, db, "category term count hashes the covered posts", termCountQuery(1), []string{"HashJoin", "index: [wp_posts.post_type,wp_posts.post_status,wp_posts.post_author]"}, []string{"LookupJoin"})
	checkPlan(t, db, "tag term count looks up posts", termCountQuery(2), []string{"LookupJoin", "index: [wp_posts.ID]"}, []string{"HashJoin"})
}
