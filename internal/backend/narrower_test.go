package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// 段階 79: 等号で決まる行がずっと少ないインデックスがあれば、並べ替えのインデックスを順に読まずに、そのインデックスで
// 引いて並べ替える（リビジョンの一覧。MySQL は post_parent で引いて filesort）。

// revisionData は、記事 300 件（日時は 50 通り）と、記事ごとに 10 件のリビジョン（日時は記事ごとに 3 通り）を 1 つの文で
// 入れる。i 番目の記事の ID は (i - 1) * 11 + 1 で、そのすぐ後にリビジョンが続く（ID は自動で振る。MySQL は
// INSERT ... SELECT の文ごとに ID をまとめて取るので、文を分けると次の文の ID が飛ぶ。ID を明示した INSERT ... SELECT の
// OK パケットの insert_id は MySQL と違う。段階 79 より前から。記録だけ）。
func revisionData() []string {
	return []string{"INSERT INTO wp_posts (post_date, post_content, post_title, post_excerpt, post_status, post_parent, post_name, to_ping, pinged, post_content_filtered, post_type) " +
		"WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300), r (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM r WHERE j < 10) " +
		"SELECT IF(j = 0, '2024-01-01 00:00:00' + INTERVAL i % 50 MINUTE, '2024-02-01 00:00:00' + INTERVAL (j - 1) % 3 MINUTE), 'c', CONCAT('P', i), '', " +
		"IF(j = 0, 'publish', 'inherit'), IF(j = 0, 0, (i - 1) * 11 + 1), IF(j = 0, CONCAT('p-', i), CONCAT(i, '-revision-v', j)), '', '', '', IF(j = 0, 'post', 'revision') " +
		"FROM n, r ORDER BY i, j"}
}

var revisionScript = []string{
	// REST API が記事ごとに流すリビジョンの一覧（同順位は ID で決まる）
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.post_parent = 1640 AND wp_posts.post_type = 'revision' AND ((wp_posts.post_status = 'inherit')) ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 1",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.post_parent = 67 AND wp_posts.post_type = 'revision' AND ((wp_posts.post_status = 'inherit')) ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 100",
	"SELECT FOUND_ROWS()",
	// 同順位の行の並び（日時だけで並べる）
	"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.post_parent = 1640 AND wp_posts.post_type = 'revision' AND wp_posts.post_status = 'inherit' ORDER BY wp_posts.post_date DESC LIMIT 0, 5",
	"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.post_parent = 1640 AND wp_posts.post_type = 'revision' AND wp_posts.post_status = 'inherit' ORDER BY wp_posts.post_date ASC",
	// 当てはまる行のない親
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.post_parent = 2 AND wp_posts.post_type = 'revision' AND ((wp_posts.post_status = 'inherit')) ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 1",
	"SELECT FOUND_ROWS()",
	// 親の記事はほとんどの行が post_parent = 0 なので、並べ替えのインデックスのまま
	"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.post_parent = 0 AND wp_posts.post_type = 'post' AND wp_posts.post_status = 'publish' ORDER BY wp_posts.post_date DESC LIMIT 0, 5",
}

// リビジョンの一覧の結果（FOUND_ROWS() と同順位の行の並びを含む）が、本物の MySQL と同じ。
func TestRevisionQueriesMatchMySQL(t *testing.T) {
	stmts := append(append(append([]string{wpSQLMode}, wpSchema()...), revisionData()...), revisionScript...)
	checkGolden(t, "../testdata/revision_golden.txt", "heimdb_golden79", stmts)
}

// リビジョンの一覧は post_parent のインデックスで引いて並べ替え、記事の一覧は type_status_date を逆に読む。
func TestRevisionQueryPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), revisionData()...) {
		mustExec(t, db, q)
	}
	rev := explain(t, db, revisionScript[0])
	if !strings.Contains(rev, "index: [wp_posts.post_parent]") || !strings.Contains(rev, "Sort(") {
		t.Errorf("revision list does not look up post_parent:\n%s", rev)
	}
	posts := explain(t, db, revisionScript[len(revisionScript)-1])
	if !strings.Contains(posts, "wp_posts.post_type,wp_posts.post_status,wp_posts.post_date") || strings.Contains(posts, "Sort(") {
		t.Errorf("post list does not read type_status_date:\n%s", posts)
	}
}
