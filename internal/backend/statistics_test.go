package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// statsData は、段階 23 のテストのデータ: optimizeData に、固定ページなどの投稿 6 件と、コメント 600 件を足す（記事は ID を 40 で割った余り + 1、
// 20 件に 1 件は未承認、50 件に 1 件はスパム、30 件に 1 件はピンバック、7 件に 1 件はユーザー 1 のコメント、
// 日時は 1 分ずつ）。
func statsData() []string {
	var comments []string
	for i := 1; i <= 600; i++ {
		approved := "1"
		switch {
		case i%50 == 0:
			approved = "spam"
		case i%20 == 0:
			approved = "0"
		}
		typ := "comment"
		if i%30 == 0 {
			typ = "pingback"
		}
		user := 0
		if i%7 == 0 {
			user = 1
		}
		date := fmt.Sprintf("2024-02-%02d %02d:%02d:00", 1+i/(24*60), i/60%24, i%60)
		comments = append(comments, fmt.Sprintf("(%d, 'a', '', '', '%s', '%s', 'c', '%s', '%s', %d)", i%40+1, date, date, approved, typ, user))
	}
	var others []string
	for i, ts := range [][2]string{{"page", "publish"}, {"page", "draft"}, {"attachment", "inherit"}, {"attachment", "inherit"}, {"revision", "inherit"}, {"page", "publish"}} {
		others = append(others, fmt.Sprintf("(1, '2024-03-%02d 10:00:00', 'c', 'Other %d', '', '%s', 'other-%d', '', '', '', '%s')", i+1, i, ts[1], i, ts[0]))
	}
	return append(optimizeData(),
		"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES "+strings.Join(others, ", "),
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_author_email, comment_author_IP, comment_date, comment_date_gmt, comment_content, comment_approved, comment_type, user_id) VALUES "+strings.Join(comments, ", "),
	)
}

// statsQueries は、段階 23 で実行計画が変わる文（コメントの数、最近のコメント、term 数、メタの JOIN）と、区間で読んだ
// 行を範囲の条件で絞り直さない場合（exactIntervals）、256 行より多くをインデックスで読む場合（先読み）。
var statsQueries = []string{
	// 記事ごとの承認済みのコメントの数（コメントの追加、記事の表示）
	"SELECT COUNT(*) FROM wp_comments WHERE comment_post_ID = 7 AND comment_approved = '1' AND comment_type != 'note'",
	// ダッシュボードのコメントの数
	"SELECT COUNT(*) FROM wp_comments WHERE ( comment_approved = '1' ) AND comment_type NOT IN ('note')",
	"SELECT COUNT(*) FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') AND user_id = 1",
	// 最近のコメント
	"SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 0, 25",
	"SELECT comment_ID FROM wp_comments WHERE comment_approved = 'spam' ORDER BY comment_date_gmt DESC LIMIT 5",
	// term 数（カテゴリ、タグ、表にない ID）
	"SELECT COUNT(*) FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = 2",
	"SELECT COUNT(*) FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = 5",
	"SELECT COUNT(*) FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = 99",
	// メニューの項目のメタの JOIN（表にないキー）と、アイキャッチ画像のメタの JOIN
	"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = '_menu_item_object_id' AND wp_postmeta.meta_value = '5' ) ) AND wp_posts.post_type = 'nav_menu_item' AND ((wp_posts.post_status <> 'trash' AND wp_posts.post_status <> 'auto-draft')) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC",
	"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE wp_postmeta.meta_key = '_thumbnail_id' AND wp_posts.post_type = 'post' ORDER BY wp_posts.ID",
	// 区間で読んだ行を絞り直さない場合と、絞り直す場合（大文字、IN、範囲、小数、日時）。末尾の空白（'1 ' = '1'）は、
	// go-mysql-server が照合順序の PAD SPACE を扱わないので MySQL と違う（段階 23 より前からの差。記録だけ）。
	"SELECT COUNT(*) FROM wp_comments WHERE comment_approved IN ('0', 'SPAM')",
	"SELECT COUNT(*) FROM wp_posts WHERE post_type = 'post' AND post_status = 'PUBLISH'",
	"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date >= '2024-01-15' AND post_date < '2024-01-17 00:00:00.5' ORDER BY ID",
	"SELECT comment_ID FROM wp_comments WHERE comment_post_ID BETWEEN 3 AND 4 ORDER BY comment_ID",
	"SELECT comment_ID FROM wp_comments WHERE comment_post_ID IN (2, 40) ORDER BY comment_ID",
	"SELECT COUNT(*) FROM wp_comments WHERE comment_post_ID = 2.0",
	"SELECT COUNT(*) FROM wp_comments WHERE comment_post_ID > 1.5 AND comment_post_ID < 3.5",
	// 検索（OR の条件の見積もり。go-mysql-server の stats.Union は、複数の列のバケットで範囲の外を読んで panic した。
	// PHPUnit の Tests_General_wpGetDocumentTitle::test_search_title）
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND (((wp_posts.post_title LIKE '%Post 1%') OR (wp_posts.post_excerpt LIKE '%Post 1%') OR (wp_posts.post_content LIKE '%Post 1%'))) AND (wp_posts.post_password = '') AND ((wp_posts.post_type = 'attachment' AND (wp_posts.post_status = 'publish')) OR (wp_posts.post_type = 'page' AND (wp_posts.post_status = 'publish')) OR (wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) ORDER BY wp_posts.post_title LIKE '%Post 1%' DESC, wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 10",
	"SELECT FOUND_ROWS()",
	"SELECT ID FROM wp_posts WHERE (post_type = 'attachment' AND post_status = 'inherit') OR (post_type = 'page' AND post_status = 'draft') OR (post_type = 'post' AND post_status = 'draft') OR (post_type = 'revision' AND post_status = 'inherit') ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE (post_type = 'post' AND post_status = 'publish' AND post_author = 2) OR (post_type = 'post' AND post_status = 'draft' AND post_author = 1) ORDER BY ID",
	// 256 行より多くをインデックスの順で読む（先読みしても順は変わらない）
	"SELECT comment_ID FROM wp_comments WHERE comment_approved = '1'",
	// 範囲を後ろの列まで狭める（narrowIndexRanges）
	"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date > '2024-01-20' ORDER BY ID",
	"SELECT comment_ID FROM wp_comments WHERE comment_approved = '0' AND comment_date_gmt >= '2024-02-01 05:00:00' ORDER BY comment_ID",
	"SELECT ID FROM wp_posts WHERE post_type IN ('page', 'post') AND post_status = 'draft' ORDER BY ID",
	// コメントの一覧のページ送り。2 ページ目も 1 ページ目と同じく日時のインデックスを逆に読む（MySQL は、表が小さいと
	// 2 ページ目を並べ替えで読み、同順位の行の並びが再現できないので、同じ日時の行がない状態で比べる）。
	"SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 25, 25",
	// 同じ日時のコメントがページの境目をまたぐ。インデックスを逆に読むと、同順位の行は ID の降順。SQL_CALC_FOUND_ROWS
	// では、MySQL は表を全部読んで並べ替えるので、同順位の行は ID の昇順。
	"UPDATE wp_comments SET comment_date_gmt = '2024-02-01 09:30:00' WHERE comment_ID BETWEEN 560 AND 590",
	"SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 0, 25",
	"SELECT SQL_CALC_FOUND_ROWS wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 0, 25",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 25, 25",
	"SELECT FOUND_ROWS()",
	// JOIN の検索のキーが、列の型に入らない（int unsigned に 2^63+1 と -1）
	"CREATE TABLE lookup_src (id bigint unsigned NOT NULL PRIMARY KEY, v bigint NOT NULL, w bigint unsigned NOT NULL, f double NOT NULL, d decimal(14,2) NOT NULL, s varchar(20) NOT NULL)",
	"CREATE TABLE lookup_dst (id bigint unsigned NOT NULL PRIMARY KEY, k int unsigned NOT NULL, KEY k (k))",
	"INSERT INTO lookup_src VALUES (1, -1, 9223372036854775809, 2.5, 2.5, '2.5'), (2, 2, 2, 2, 2, '2'), (3, 3, 3, 4294967296, 4294967296, '4294967296')",
	"INSERT INTO lookup_dst VALUES " + lookupDstRows(),
	"SELECT lookup_src.id, lookup_dst.id FROM lookup_src JOIN lookup_dst ON lookup_dst.k = lookup_src.w ORDER BY lookup_src.id",
	"SELECT lookup_src.id, lookup_dst.id FROM lookup_src JOIN lookup_dst ON lookup_dst.k = lookup_src.v ORDER BY lookup_src.id",
	// 小数、decimal、文字列のキー
	"SELECT lookup_src.id, lookup_dst.id FROM lookup_src JOIN lookup_dst ON lookup_dst.k = lookup_src.f ORDER BY lookup_src.id",
	"SELECT lookup_src.id, lookup_dst.id FROM lookup_src JOIN lookup_dst ON lookup_dst.k = lookup_src.d ORDER BY lookup_src.id",
	"SELECT lookup_src.id, lookup_dst.id FROM lookup_src JOIN lookup_dst ON lookup_dst.k = lookup_src.s ORDER BY lookup_src.id",
	// 範囲の端が、列の型に入らない
	"SELECT COUNT(*) FROM lookup_dst WHERE k > -1",
	"SELECT COUNT(*) FROM lookup_dst WHERE k < 9223372036854775809",
	"SELECT COUNT(*) FROM lookup_dst WHERE k BETWEEN -5 AND 3",
	"SELECT COUNT(*) FROM lookup_dst WHERE k != -1",
	"SELECT id FROM lookup_dst WHERE k IN (-1, 4294967295, 4294967296)",
	"SELECT id FROM lookup_dst WHERE k > -1 ORDER BY k DESC LIMIT 3",
	"SELECT id FROM lookup_dst WHERE k BETWEEN -5 AND 3 ORDER BY k",
	// 小数と文字列の定数
	"SELECT id FROM lookup_dst WHERE k > -0.5 ORDER BY id LIMIT 3",
	"SELECT id FROM lookup_dst WHERE k < 1e10 ORDER BY id DESC LIMIT 3",
	"SELECT id FROM lookup_dst WHERE k BETWEEN -0.5 AND 2.5 ORDER BY id",
	"SELECT COUNT(*) FROM lookup_dst WHERE k < '4294967296'",
	"SELECT id FROM lookup_dst WHERE k > 1.5 ORDER BY id LIMIT 3",
	// 後ろの列だけが列の型に入らない（前の列の範囲は使う）
	"SELECT object_id, term_taxonomy_id FROM wp_term_relationships WHERE object_id IN (3, 5) AND term_taxonomy_id > -1 ORDER BY object_id, term_taxonomy_id",
	"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_author > -1 ORDER BY ID",
	"SELECT ID FROM wp_posts WHERE post_type IN ('page', 'post') AND post_status = 'draft' AND post_author < 4294967296 ORDER BY ID",
	// LIMIT m, n の m + n が溢れる
	"SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 18446744073709551615, 25",
}

// lookupDstRows は、lookup_dst の行（k は 1 から 2000 と、int unsigned の最大値）。go-mysql-server が JOIN で
// lookup_dst をインデックスで引くだけの行数にする。
func lookupDstRows() string {
	var rows []string
	for i := 1; i <= 2000; i++ {
		rows = append(rows, fmt.Sprintf("(%d, %d)", i, i))
	}
	rows = append(rows, "(2001, 4294967295)")
	return strings.Join(rows, ", ")
}

// 段階 23 の文の結果が、本物の MySQL と同じ。
func TestStatisticsQueriesMatchMySQL(t *testing.T) {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	stmts = append(append(stmts, statsData()...), statsQueries...)
	checkGolden(t, "../testdata/stats_golden.txt", "heimdb_golden23", stmts)
}

// explain は、文の実行計画を 1 つの文字列で返す。
func explain(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN PLAN " + q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// 統計があると、go-mysql-server が読む行の少ないインデックスと JOIN の順を選ぶ。
func TestStatisticsChoosePlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), statsData()...) {
		mustExec(t, db, q)
	}
	termCount := func(tt int) string {
		return fmt.Sprintf("SELECT wp_posts.ID FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = %d", tt)
	}
	for _, tc := range []struct {
		name, q   string
		want      []string
		forbidden []string
	}{
		{"comments of a post use comment_post_ID", "SELECT comment_ID FROM wp_comments WHERE comment_post_ID = 7 AND comment_approved = '1' AND comment_type != 'note'",
			[]string{"index: [wp_comments.comment_post_ID]"}, []string{"comment_approved,"}},
		{"recent comments read the date index backwards", "SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 0, 25",
			[]string{"index: [wp_comments.comment_date_gmt]", "reverse: true"}, []string{"Sort"}},
		{"rare status keeps its index", "SELECT comment_ID FROM wp_comments WHERE comment_approved = 'spam' ORDER BY comment_date_gmt DESC LIMIT 5",
			[]string{"index: [wp_comments.comment_approved,wp_comments.comment_date_gmt]"}, nil},
		{"small tag starts from term_relationships", termCount(5),
			[]string{"index: [wp_term_relationships.term_taxonomy_id]", "index: [wp_posts.ID]"}, nil},
		{"new tag starts from term_relationships", termCount(99),
			[]string{"index: [wp_term_relationships.term_taxonomy_id]", "index: [wp_posts.ID]"}, nil},
		{"later pages read the date index backwards too", "SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 25, 25",
			[]string{"index: [wp_comments.comment_date_gmt]", "reverse: true"}, []string{"Sort"}},
		{"found rows read the whole table and sort", "SELECT SQL_CALC_FOUND_ROWS wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 25, 25",
			[]string{"Sort", "Table\n"}, []string{"IndexedTableAccess"}},
		{"a third of the rows keeps the filter index", "SELECT comment_ID FROM wp_comments WHERE comment_post_ID IN (1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13) ORDER BY comment_date_gmt DESC LIMIT 5",
			nil, []string{"index: [wp_comments.comment_date_gmt]"}},
		{"ranges cover later index columns", "SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date > '2024-01-20'",
			[]string{"[post, post], [publish, publish]"}, nil},
		{"ranges end with a range on the next column", "SELECT comment_ID FROM wp_comments WHERE comment_approved = '0' AND comment_date_gmt >= '2024-02-01 05:00:00' AND comment_post_ID > 0",
			nil, []string{"[0, 0], [NULL, ∞)"}},
		{"an out-of-range later column keeps the earlier ranges", "SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_author > -1",
			[]string{"[post, post], [publish, publish], [NULL, ∞)"}, nil},
		{"an out-of-range column outside the index keeps the ranges", "SELECT comment_ID FROM wp_comments WHERE comment_post_ID = 7 AND user_id > -1",
			[]string{"index: [wp_comments.comment_post_ID]", "[7, 7]"}, nil},
		{"an overflowing offset keeps the sort", "SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 18446744073709551615, 25",
			[]string{"TopN"}, []string{"index: [wp_comments.comment_date_gmt]"}}, // 段階 83 から Project 越しの TopN
		{"meta join looks up posts by ID", "SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE wp_postmeta.meta_key = '_menu_item_object_id' AND wp_posts.post_type = 'nav_menu_item'",
			[]string{"LookupJoin"}, []string{"MergeJoin"}},
	} {
		checkPlan(t, db, tc.name, tc.q, tc.want, tc.forbidden)
	}

	// インデックスを足しても、表の統計を作り直して使い続ける（一部のインデックスにだけ統計があると、
	// go-mysql-server はその表の統計をすべて使わず、名前の順で承認の状態のインデックスを選ぶ）。
	mustExec(t, db, "ALTER TABLE wp_comments ADD INDEX comment_karma (comment_karma)")
	checkPlan(t, db, "stats after adding an index", "SELECT comment_ID FROM wp_comments WHERE comment_post_ID = 7 AND comment_approved = '1' AND comment_type != 'note'",
		[]string{"index: [wp_comments.comment_post_ID]"}, nil)
	mustExec(t, db, "ALTER TABLE wp_comments DROP INDEX comment_karma")
	checkPlan(t, db, "stats after dropping an index", "SELECT comment_ID FROM wp_comments WHERE comment_post_ID = 7 AND comment_approved = '1' AND comment_type != 'note'",
		[]string{"index: [wp_comments.comment_post_ID]"}, nil)
}

// checkPlan は、文 q の実行計画が want をすべて含み、forbidden をどれも含まないことを確かめる。
func checkPlan(t *testing.T, db *sql.DB, name, q string, want, forbidden []string) {
	t.Helper()
	plan := explain(t, db, q)
	for _, w := range want {
		if !strings.Contains(plan, w) {
			t.Errorf("%s: plan lacks %q:\n%s", name, w, plan)
		}
	}
	for _, f := range forbidden {
		if strings.Contains(plan, f) {
			t.Errorf("%s: plan has %q:\n%s", name, f, plan)
		}
	}
}

// 段階 29: 2 万行を超える表で、記事ごとに続けて挿入されたメタの組（_pingme と _encloseme）のどちらも統計に残り、
// メタのキーと記事の ID で引く文（add_metadata の重複の確認の形。EXPLAIN PLAN は COUNT(*) を受け付けない）が
// 記事の ID のインデックスを使う。主キーの順に等間隔に間引くと _encloseme が残らず、0 行と見積もって meta_key の
// インデックスで 5,000 行を読んでいた。
func TestStatisticsSamplePairedMeta(t *testing.T) {
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
	mustExec(t, db, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (2, '_wp_page_template', 'default'), (3, '_wp_page_template', 'default')")
	for _, group := range [][]string{{"_pingme", "_encloseme"}, {"bench_views", "bench_color", "bench_note"}} {
		for p := 1; p <= 5000; p += 500 {
			var vals []string
			for i := p; i < p+500; i++ {
				for _, k := range group {
					vals = append(vals, fmt.Sprintf("(%d, '%s', '1')", i, k))
				}
			}
			mustExec(t, db, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES "+strings.Join(vals, ", "))
		}
	}
	for _, k := range []string{"_pingme", "_encloseme", "bench_views", "bench_color", "bench_note"} {
		for _, id := range []int{4000, 6000} {
			checkPlan(t, db, fmt.Sprintf("%s of post %d", k, id), fmt.Sprintf("SELECT meta_id FROM wp_postmeta WHERE meta_key = '%s' AND post_id = %d", k, id),
				[]string{"index: [wp_postmeta.post_id]"}, []string{"index: [wp_postmeta.meta_key]"})
		}
	}
}
