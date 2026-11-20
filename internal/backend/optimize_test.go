package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// optimizeData は、段階 22 のテストのデータ: 記事 40 件（10 件ごとに下書き、日付は 2 件ずつ同じ）、カテゴリ 3 つとタグ 2 つ、
// 記事ごとにカテゴリ 1 つ（ID を 3 で割った余り）、偶数の記事にタグ alpha、5 の倍数の記事にタグ beta、4 の倍数の記事に
// アイキャッチ画像のメタ、作成者は ID を 3 で割った余り + 1。
func optimizeData() []string {
	stmts := []string{
		// ID は AUTO_INCREMENT に任せる（1 から順）。主キーを明示した複数行の INSERT の insert_id は MySQL と違う（記録だけ）。
		"INSERT INTO wp_terms (name, slug) VALUES ('Uncategorized', 'uncategorized'), ('News', 'news'), ('Tech', 'tech'), ('alpha', 'alpha'), ('beta', 'beta')",
		"INSERT INTO wp_term_taxonomy (term_id, taxonomy, description) VALUES (1, 'category', ''), (2, 'category', ''), (3, 'category', ''), (4, 'post_tag', ''), (5, 'post_tag', '')",
	}
	var posts, rels, meta []string
	for id := 1; id <= 40; id++ {
		status := "publish"
		if id%10 == 0 {
			status = "draft"
		}
		posts = append(posts, fmt.Sprintf("(%d, '2024-01-%02d 10:00:00', 'c', 'Post %d', '', '%s', 'post-%d', '', '', '', 'post')", id%3+1, (id+1)/2, id, status, id))
		rels = append(rels, fmt.Sprintf("(%d, %d)", id, id%3+1))
		if id%2 == 0 {
			rels = append(rels, fmt.Sprintf("(%d, 4)", id))
		}
		if id%5 == 0 {
			rels = append(rels, fmt.Sprintf("(%d, 5)", id))
		}
		if id%4 == 0 {
			meta = append(meta, fmt.Sprintf("(%d, '_thumbnail_id', '%d')", id, id*100))
		}
	}
	return append(stmts,
		"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES "+strings.Join(posts, ", "),
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES "+strings.Join(rels, ", "),
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES "+strings.Join(meta, ", "),
	)
}

// wordpressJoins は、WordPress が出す JOIN の形（docs/bench.md で遅かったもの）。
var wordpressJoins = []string{
	// 記事の作成と削除のたびの term 数の数え直し（_update_post_term_count）
	"SELECT COUNT(*) FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = 2",
	// 記事の term の読み出し（wp_get_object_terms）。並びは同順位を避けて term_id と object_id にする
	"SELECT DISTINCT t.term_id, tr.object_id FROM wp_terms AS t INNER JOIN wp_term_taxonomy AS tt ON t.term_id = tt.term_id INNER JOIN wp_term_relationships AS tr ON tr.term_taxonomy_id = tt.term_taxonomy_id WHERE tt.taxonomy IN ('category', 'post_tag', 'post_format') AND tr.object_id IN (1, 2, 3, 4, 5, 6) ORDER BY t.term_id, tr.object_id",
	// term のキャッシュの読み込み（_prime_term_caches）
	"SELECT t.*, tt.* FROM wp_terms AS t INNER JOIN wp_term_taxonomy AS tt ON t.term_id = tt.term_id WHERE t.term_id IN (1, 2, 4) ORDER BY t.term_id",
	// カテゴリのアーカイブ（WP_Query の tax_query）
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = wp_term_relationships.object_id) WHERE 1=1 AND ( wp_term_relationships.term_taxonomy_id IN (2) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC LIMIT 0, 5",
	"SELECT FOUND_ROWS()",
	// タグを除くアーカイブ（tax_query の NOT IN）
	"SELECT wp_posts.ID FROM wp_posts WHERE 1=1 AND ( wp_posts.ID NOT IN ( SELECT object_id FROM wp_term_relationships WHERE term_taxonomy_id IN (4) ) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 5",
	// メタの条件（WP_Query の meta_query）
	"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = '_thumbnail_id' AND wp_postmeta.meta_value = '800' ) ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
	"SELECT wp_posts.ID, mt1.meta_value FROM wp_posts LEFT JOIN wp_postmeta AS mt1 ON (wp_posts.ID = mt1.post_id AND mt1.meta_key = '_thumbnail_id') WHERE wp_posts.post_type = 'post' AND wp_posts.ID <= 8 ORDER BY wp_posts.ID",
	// WHERE が右の表の行を求める LEFT JOIN は INNER JOIN にする（simplifyOuterJoins。段階 51）。タグ、複数のカテゴリ、
	// 2 つのタクソノミーの AND（別名の表）、メタの値の比較（<>。メタのない記事を落とす）
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = wp_term_relationships.object_id) WHERE 1=1 AND ( wp_term_relationships.term_taxonomy_id IN (5) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 10",
	"SELECT FOUND_ROWS()",
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = wp_term_relationships.object_id) WHERE 1=1 AND ( wp_term_relationships.term_taxonomy_id IN (1,3) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 10",
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = wp_term_relationships.object_id) LEFT JOIN wp_term_relationships AS tt1 ON (wp_posts.ID = tt1.object_id) WHERE 1=1 AND ( wp_term_relationships.term_taxonomy_id IN (2) AND tt1.term_taxonomy_id IN (4) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 10",
	"SELECT wp_posts.ID, mt1.meta_value FROM wp_posts LEFT JOIN wp_postmeta AS mt1 ON (wp_posts.ID = mt1.post_id AND mt1.meta_key = '_thumbnail_id') WHERE wp_posts.post_type = 'post' AND mt1.meta_value <> '800' ORDER BY wp_posts.ID",
	// INNER JOIN にした表の、値が 1 つの IN は等号にする（singleInEquals。段階 51）。ほかの表の IN はそのまま
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships AS tr ON (wp_posts.ID = tr.object_id) WHERE tr.term_taxonomy_id IN (2) AND wp_posts.post_author IN (2) ORDER BY wp_posts.ID",
	// INNER JOIN にしない形: メタがない記事（meta_query の NOT EXISTS）、IS NULL と OR、<=>、COALESCE。右に行のない記事が残る
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_postmeta AS mt1 ON (wp_posts.ID = mt1.post_id AND mt1.meta_key = '_thumbnail_id') WHERE 1=1 AND ( mt1.post_id IS NULL ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID",
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_postmeta AS mt1 ON (wp_posts.ID = mt1.post_id AND mt1.meta_key = '_thumbnail_id') WHERE ( mt1.meta_value = '800' OR mt1.post_id IS NULL ) AND wp_posts.ID <= 10 ORDER BY wp_posts.ID",
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_postmeta AS mt1 ON (wp_posts.ID = mt1.post_id AND mt1.meta_key = '_thumbnail_id') WHERE mt1.meta_value <=> NULL AND wp_posts.ID <= 10 ORDER BY wp_posts.ID",
	"SELECT wp_posts.ID FROM wp_posts LEFT JOIN wp_postmeta AS mt1 ON (wp_posts.ID = mt1.post_id AND mt1.meta_key = '_thumbnail_id') WHERE COALESCE(mt1.meta_value, '0') = '0' AND wp_posts.ID <= 10 ORDER BY wp_posts.ID",
	// 文字列の列の JOIN は照合順序で比べる（ハッシュ JOIN にすると 'Red' と 'red' がつながらない。段階 22 のレビュー）
	"INSERT INTO wp_terms (name, slug) VALUES ('Red', 'red'), ('RED', 'red-2')",
	"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'color', 'Red'), (2, 'color', 'red'), (3, 'color', 'RED')",
	"SELECT pm.post_id, t.term_id FROM wp_postmeta pm JOIN wp_terms t ON t.name = pm.meta_value WHERE pm.meta_key = 'color' ORDER BY pm.post_id, t.term_id",
	// 小数秒つきの日時と DATETIME(0) の列の比較で、ORDER BY のインデックスの範囲が境目の行を落とさない（段階 22 のレビュー）
	"SELECT post_date FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date < '2024-01-03 10:00:00.4' ORDER BY post_date DESC LIMIT 10",
	"SELECT post_date FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date > '2024-01-18 09:59:59.5' ORDER BY post_date LIMIT 10",
	// simplifyOuterJoins の形（段階 51 のレビュー）: 入れ子の LEFT JOIN、RIGHT JOIN（替えない）、USING、符号なしの BIGINT の
	// 2^53 を超える値の IN（等号にするとき、定数を列の型にする）、JOIN を含む UPDATE と DELETE
	"SELECT p.ID, tt.taxonomy FROM wp_posts AS p LEFT JOIN (wp_term_relationships AS tr LEFT JOIN wp_term_taxonomy AS tt ON tr.term_taxonomy_id = tt.term_taxonomy_id) ON p.ID = tr.object_id WHERE tt.taxonomy = 'post_tag' AND p.ID <= 12 ORDER BY p.ID, tt.taxonomy",
	"SELECT p.ID, tr.term_taxonomy_id FROM wp_term_relationships AS tr RIGHT JOIN wp_posts AS p ON p.ID = tr.object_id AND tr.term_taxonomy_id = 5 WHERE p.ID <= 12 ORDER BY p.ID",
	"CREATE TABLE heimdb_u (id bigint unsigned NOT NULL PRIMARY KEY, v bigint unsigned NOT NULL)",
	"INSERT INTO heimdb_u VALUES (1, 9007199254740992), (2, 9007199254740993), (3, 9007199254740994), (4, 18446744073709551615)",
	"SELECT a.id FROM heimdb_u AS a LEFT JOIN heimdb_u AS b ON a.id = b.id WHERE b.v IN (9007199254740993) ORDER BY a.id",
	"SELECT a.id FROM heimdb_u AS a LEFT JOIN heimdb_u AS b ON a.id = b.id + 1 WHERE b.v IN (9007199254740993) ORDER BY a.id",
	"SELECT a.id FROM heimdb_u AS a LEFT JOIN heimdb_u AS b ON a.id = b.id WHERE b.v IN (18446744073709551615) ORDER BY a.id",
	"SELECT a.id, b.v FROM heimdb_u AS a LEFT JOIN heimdb_u AS b USING (id) WHERE b.v IN (9007199254740994) ORDER BY a.id",
	"UPDATE heimdb_u AS a LEFT JOIN heimdb_u AS b ON a.id = b.id + 1 SET a.v = a.v - 1 WHERE b.v IN (9007199254740992)",
	"DELETE a FROM heimdb_u AS a LEFT JOIN heimdb_u AS b ON a.id = b.id + 1 WHERE b.v IN (9007199254740994)",
	"SELECT id, v FROM heimdb_u ORDER BY id",
}

func optimizeStatements(t *testing.T) []string {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	stmts = append(stmts, optimizeData()...)
	return append(stmts, wordpressJoins...)
}

// WordPress の JOIN の形の結果が、本物の MySQL と同じ。
func TestWordPressJoinsMatchMySQL(t *testing.T) {
	checkGolden(t, "../testdata/joins_golden.txt", "heimdb_golden22", optimizeStatements(t))
}

func optimizeServer(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	dsn, stop := startServer(t, t.TempDir())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), optimizeData()...) {
		mustExec(t, db, q)
	}
	return db, func() { db.Close(); stop() }
}

// nestedLoopJoins は、実行計画のうち、内側を外側の行ごとに読み直す JOIN の行を返す。
func nestedLoopJoins(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	var bad []string
	rows, err := db.Query("EXPLAIN PLAN " + q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		switch strings.TrimLeft(line, " │├└─") {
		case "InnerJoin", "LeftOuterJoin", "LeftOuterJoinExcludingNulls", "SemiJoin", "AntiJoin":
			bad = append(bad, line)
		}
	}
	return bad
}

// WordPress の JOIN は、どれも入れ子のループではなく、lookup かハッシュ（かマージ）の JOIN になる。
func TestWordPressJoinsAvoidNestedLoops(t *testing.T) {
	db, stop := optimizeServer(t)
	defer stop()
	for _, q := range wordpressJoins {
		if !strings.Contains(q, "JOIN") && !strings.Contains(q, "wp_term_relationships, wp_posts") && !strings.Contains(q, "NOT IN (") {
			continue
		}
		// 段階 51 のレビューで足した表（optimizeServer は作らない）
		if strings.Contains(q, "heimdb_u") {
			continue
		}
		if bad := nestedLoopJoins(t, db, q); len(bad) > 0 {
			t.Errorf("nested loop join in plan of %s: %v", q, bad)
		}
	}
}

// 表の行数の見積もり（SHOW TABLE STATUS の Rows にも出る）は、数えた後の挿入と削除が少ないうちは前の値のまま、
// 多くなると数え直す。COUNT(*) は見積もりを使わず、いつも正確。
func TestRowCountEstimate(t *testing.T) {
	db, stop := optimizeServer(t)
	defer stop()
	statusRows := func() string {
		t.Helper()
		rows, err := db.Query("SHOW TABLE STATUS WHERE Name = 'wp_postmeta'")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if !rows.Next() {
			t.Fatal("no table status")
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, c := range cols {
			if c == "Rows" {
				return string(vals[i])
			}
		}
		t.Fatal("no Rows column")
		return ""
	}
	insertMeta := func(n int) {
		t.Helper()
		var vals []string
		for i := 0; i < n; i++ {
			vals = append(vals, fmt.Sprintf("(1, 'k%d', 'v')", i))
		}
		mustExec(t, db, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES "+strings.Join(vals, ", "))
	}

	if got := statusRows(); got != "10" {
		t.Errorf("first estimate = %s, want 10", got)
	}
	insertMeta(30)
	if got := statusRows(); got != "10" {
		t.Errorf("estimate after 30 inserts = %s, want 10 (not recounted yet)", got)
	}
	if got := column(t, db, "SELECT COUNT(*) FROM wp_postmeta"); got != "40" {
		t.Errorf("COUNT(*) = %s, want 40", got)
	}
	insertMeta(40)
	if got := statusRows(); got != "80" {
		t.Errorf("estimate after 70 inserts = %s, want 80 (recounted)", got)
	}
	// TRUNCATE はテーブルを作り直すので、行数を数え直す。
	entries := func() int {
		rowCounts.mu.Lock()
		defer rowCounts.mu.Unlock()
		return len(rowCounts.entries)
	}
	before := entries()
	mustExec(t, db, "TRUNCATE TABLE wp_postmeta")
	if got := statusRows(); got != "0" {
		t.Errorf("estimate after truncate = %s, want 0", got)
	}
	// 作り直したテーブルと消したテーブルの行数は忘れる（覚える数はテーブルの数だけ）。
	if got := entries(); got != before {
		t.Errorf("entries after truncate = %d, want %d", got, before)
	}
	mustExec(t, db, "DROP TABLE wp_postmeta")
	if got := entries(); got != before-1 {
		t.Errorf("entries after drop = %d, want %d", got, before-1)
	}
}

// ORDER BY ... LIMIT をインデックスの順で読む文は、LIMIT の行数を返したところで行を読むのをやめる。
func TestIndexScanStopsAtLimit(t *testing.T) {
	db, stop := optimizeServer(t)
	defer stop()
	for _, tc := range []struct {
		q, want string
	}{
		// 記事の一覧（type_status_date の逆順の走査。同じ日付は ID の降順）
		{"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC LIMIT 3", "39,38,37"},
		// 前の記事のリンク（get_adjacent_post）
		{"SELECT p.ID FROM wp_posts AS p WHERE (p.post_date < '2024-01-10 10:00:00' OR (p.post_date = '2024-01-10 10:00:00' AND p.ID < 19)) AND p.post_type = 'post' AND p.post_status = 'publish' ORDER BY p.post_date DESC, p.ID DESC LIMIT 1", "18"},
	} {
		before := pkRowsRead.Load()
		if got := column(t, db, tc.q); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.q, got, tc.want)
		}
		if read := pkRowsRead.Load() - before; read > 5 {
			t.Errorf("%s read %d rows", tc.q, read)
		}
	}
}

// 複数の範囲のインデックスを逆順に読むと、範囲を後ろから読むので、並べ替えなしでキーの逆順になる（段階 22 で並べ替えを
// やめた）。記録だけの差: MySQL は等号の範囲の中を ID の昇順で返す（2,5,8,...,3,6,9,...。MySQL 8.0 で確認）。HeimDB は
// 段階 22 より前から ID の降順。
func TestReverseMultiRangeOrder(t *testing.T) {
	db, stop := optimizeServer(t)
	defer stop()
	got := column(t, db, "SELECT ID FROM wp_posts WHERE post_author IN (1, 3) ORDER BY post_author DESC")
	want := "38,35,32,29,26,23,20,17,14,11,8,5,2,39,36,33,30,27,24,21,18,15,12,9,6,3"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// ブランチを消すと、そのブランチで覚えた表の行数を忘れる。
func TestRowCountForgottenWithBranch(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	entries := func() int {
		rowCounts.mu.Lock()
		defer rowCounts.mu.Unlock()
		return len(rowCounts.entries)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	before := entries()
	mustExec(t, staging, "SHOW TABLE STATUS")
	if entries() <= before {
		t.Fatal("SHOW TABLE STATUS on the branch did not count rows")
	}
	staging.Close()
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")
	if got := entries(); got != before {
		t.Errorf("entries after branch delete = %d, want %d", got, before)
	}
}

// カテゴリの記事の一覧は、post_date を持つ type_status_date をインデックスだけで読み（swapCoveringIndex が GroupBy と
// JOIN の下も替える）、カテゴリの term 数は、インデックスの範囲が表す記事の条件を Filter に残さない
// （dropRangeFilters。docs/plan.md の段階 50）。計画は記事の数で変わるので、ベンチマークのデータ（docs/bench.md）に
// 近い形で、ほとんどの記事が 1 つのカテゴリに入る 3,000 件にする。結果は TestWordPressJoinsMatchMySQL が本物の
// MySQL と比べる。
func TestCategoryQueryPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		mustExec(t, db, q)
	}
	mustExec(t, db, "INSERT INTO wp_terms (name, slug) VALUES ('Uncategorized', 'uncategorized'), ('Tag A', 'tag-a'), ('Tag B', 'tag-b')")
	mustExec(t, db, "INSERT INTO wp_term_taxonomy (term_id, taxonomy, description) VALUES (1, 'category', ''), (2, 'post_tag', ''), (3, 'post_tag', '')")
	for b := 0; b < 3; b++ {
		var posts, rels []string
		for i := 1; i <= 1000; i++ {
			id := b*1000 + i
			typ, status := "post", "publish"
			if id%30 == 0 {
				typ = "page"
			}
			if id%50 == 0 {
				status = "draft"
			}
			posts = append(posts, fmt.Sprintf("(%d, %d, '2024-01-01 00:00:00' + INTERVAL %d MINUTE, 'c', 'P%d', '', '%s', 'p-%d', '', '', '', '%s')", id, id%5+1, id, id, status, id, typ))
			rels = append(rels, fmt.Sprintf("(%d, 1), (%d, 2), (%d, 3)", id, id, id))
		}
		mustExec(t, db, "INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES "+strings.Join(posts, ", "))
		mustExec(t, db, "INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES "+strings.Join(rels, ", "))
	}
	// 記事の少ないカテゴリ（30 件）
	mustExec(t, db, "INSERT INTO wp_terms (name, slug) VALUES ('Small', 'small')")
	mustExec(t, db, "INSERT INTO wp_term_taxonomy (term_id, taxonomy, description) VALUES (4, 'category', '')")
	var small []string
	for id := 100; id < 3000; id += 100 {
		small = append(small, fmt.Sprintf("(%d, 4)", id+1))
	}
	mustExec(t, db, "INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES "+strings.Join(small, ", "))
	mustExec(t, db, "ANALYZE TABLE wp_posts, wp_term_relationships")
	// LEFT JOIN を INNER JOIN にし（simplifyOuterJoins）、記事の少ないカテゴリは term_taxonomy_id のインデックスから読み、
	// 多いカテゴリは、記事から読む（段階 51 は記事ごとに主キーの 1 点で引き、段階 77 からハッシュ JOIN）。1 件の記事の term の
	// 読み込み（INNER JOIN だけの文）の IN は替えず、wp_term_relationships の object_id から読む。段階 51。
	smallList := explain(t, db, "SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = wp_term_relationships.object_id) WHERE 1=1 AND ( wp_term_relationships.term_taxonomy_id IN (4) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC LIMIT 0, 10")
	if strings.Contains(smallList, "LeftOuter") || !strings.Contains(smallList, "index: [wp_term_relationships.term_taxonomy_id]") {
		t.Errorf("small category list does not start from term_taxonomy_id:\n%s", smallList)
	}
	list := explain(t, db, "SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts LEFT JOIN wp_term_relationships ON (wp_posts.ID = wp_term_relationships.object_id) WHERE 1=1 AND ( wp_term_relationships.term_taxonomy_id IN (1) ) AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC LIMIT 10, 10")
	if !strings.Contains(list, "wp_posts.post_type,wp_posts.post_status,wp_posts.post_date") || strings.Contains(list, "post_author") {
		t.Errorf("category list does not read type_status_date:\n%s", list)
	}
	// 記事ごとに主キーで引く代わりに、カテゴリの関係を term_taxonomy_id のインデックスのエントリだけで読むハッシュ JOIN
	// にする（主キーの term_taxonomy_id は範囲で 1 つに決まる。段階 77）。読み始めるのは記事のまま
	if !strings.Contains(list, "HashJoin") || !strings.Contains(list, "index: [wp_term_relationships.term_taxonomy_id]") ||
		!strings.Contains(list, "filters: [{[1, 1]}]") || strings.Index(list, "wp_posts.post_type") > strings.Index(list, "HashLookup") {
		t.Errorf("category list does not build a hash table from the term_taxonomy_id index:\n%s", list)
	}
	terms := explain(t, db, "SELECT DISTINCT t.term_id, tr.object_id FROM wp_terms AS t INNER JOIN wp_term_taxonomy AS tt ON t.term_id = tt.term_id INNER JOIN wp_term_relationships AS tr ON tr.term_taxonomy_id = tt.term_taxonomy_id WHERE tt.taxonomy IN ('category', 'post_tag', 'post_format') AND tr.object_id IN (101) ORDER BY t.name ASC")
	if !strings.Contains(terms, "filters: [{[101, 101], [NULL, ∞)}]") {
		t.Errorf("post terms do not start from wp_term_relationships.object_id:\n%s", terms)
	}
	count := explain(t, db, "SELECT COUNT(*) FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = 1")
	if strings.Contains(count, "wp_posts.post_status = ") || strings.Contains(count, "post_status HASH IN") || strings.Contains(count, "term_taxonomy_id = 1") {
		t.Errorf("term count keeps a filter that the index ranges cover:\n%s", count)
	}
}
