package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// 段階 80: サブクエリから作る半結合と反結合は、右の条件の定数だけで引いて左の行ごとに読み直さず、サブクエリを 1 回
// 読む（MySQL の実体化）。WordPress 本体の meta_query の NOT EXISTS（LEFT JOIN ... IS NULL）は、MySQL と同じく
// post_id で引く。

// subqueryData は、記事 300 件（種類と状態は 10 通り）と、記事ごとにメタ 20 件を 2 つの文で入れる。どの記事にも
// bzb_include_rss があり（テーマが記事を保存するたびに書く）、12 件に 1 件が 'none'。そのキーの行が記事と同じ数あると、
// 記事ごとに post_id で引く lookup JOIN（記事 1 件あたりメタを 20 行読んで捨てる）が、引いて読む行を数えないと
// ハッシュ JOIN より安く見える。記事の日時は重ならない（meta_query の文の、同じ日時の
// 行の並びは段階 80 より前から MySQL と違う。MySQL は GROUP BY の後の並べ替えで決まった順にならない。記録だけ）。
func subqueryData() []string {
	return []string{
		"INSERT INTO wp_posts (post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) " +
			"WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) " +
			"SELECT '2024-01-01 00:00:00' + INTERVAL i MINUTE, 'c', CONCAT('P', i), '', " +
			"ELT(1 + i % 10, 'publish', 'publish', 'publish', 'publish', 'publish', 'inherit', 'inherit', 'draft', 'publish', 'inherit'), CONCAT('p-', i), '', '', '', " +
			"ELT(1 + i % 10, 'post', 'post', 'page', 'post', 'nav_menu_item', 'attachment', 'revision', 'post', 'post', 'attachment') FROM n ORDER BY i",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) " +
			"WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300), k (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j < 19) " +
			"SELECT i, IF(j = 19, 'bzb_include_rss', CONCAT('_k', j)), IF(j = 19, IF(i % 12 = 0, 'none', ''), CONCAT('v', i, '-', j)) " +
			"FROM n, k ORDER BY i, j",
	}
}

var subqueryScript = []string{
	// MySQL は、入れた直後と統計を作り直した後で計画（行の並び）が変わるので、先に作り直す
	"ANALYZE TABLE wp_posts, wp_postmeta",
	// ホスティングのテーマの文（同じ順位の並びは MySQL に合わせる）
	"select ID from wp_posts where post_status = 'publish' AND post_type not in ('attachment','nav_menu_item','revision') and ID not in (select post_id from wp_postmeta where meta_key = 'bzb_include_rss' and meta_value = 'none')",
	"select ID from wp_posts where post_status = 'publish' and ID in (select post_id from wp_postmeta where meta_key = 'bzb_include_rss' and meta_value = 'none')",
	// post_type の条件のない文は、MySQL が文の列をすべて持つインデックス（type_status_author）を全部読み、並びが主キーの
	// 順と違う（段階 80 より前から。WordPress は並べ替えのない、この形の文を出さない。記録だけ）
	"select ID from wp_posts where post_status = 'publish' AND post_type not in ('attachment','nav_menu_item','revision') and not exists (select 1 from wp_postmeta where post_id = wp_posts.ID and meta_key = 'bzb_include_rss' and meta_value = 'none')",
	"select ID from wp_posts where post_status = 'publish' AND post_type not in ('attachment','nav_menu_item','revision') and exists (select 1 from wp_postmeta where post_id = wp_posts.ID and meta_key = 'bzb_include_rss' and meta_value = 'none')",
	// WordPress 本体の meta_query の 'compare' => 'NOT EXISTS'
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts LEFT JOIN wp_postmeta ON (wp_posts.ID = wp_postmeta.post_id AND wp_postmeta.meta_key = 'bzb_include_rss') WHERE 1=1 AND ( wp_postmeta.post_id IS NULL ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
	"SELECT FOUND_ROWS()",
	// サブクエリが NULL を返すと NOT IN はどの行にも当たらない。サブクエリが空なら全部の行
	"select ID from wp_posts where post_type = 'page' and ID not in (select IF(meta_value = 'none', post_id, NULL) from wp_postmeta where meta_key = 'bzb_include_rss')",
	"select ID from wp_posts where post_type = 'page' and ID not in (select post_id from wp_postmeta where meta_key = 'nothing')",
}

// サブクエリの文の結果（同じ順位の並びと FOUND_ROWS() を含む）が、本物の MySQL と同じ。
func TestSubqueryJoinsMatchMySQL(t *testing.T) {
	stmts := append(append(append([]string{wpSQLMode}, wpSchema()...), subqueryData()...), subqueryScript...)
	checkGolden(t, "../testdata/subquery_golden.txt", "heimdb_golden80", stmts)
}

// NOT IN と NOT EXISTS はサブクエリを 1 回読むハッシュ JOIN、IN は定数のキーだけで引かない、meta_query の NOT EXISTS は
// post_id で引く。
func TestSubqueryJoinPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), subqueryData()...) {
		mustExec(t, db, q)
	}
	for _, c := range []struct {
		q    string
		want []string
	}{
		{subqueryScript[1], []string{"HashJoin"}},
		{subqueryScript[3], []string{"HashJoin"}},
		{subqueryScript[5], []string{"LeftOuterLookupJoin", "index: [wp_postmeta.post_id]", "keys: wp_posts.ID"}},
	} {
		plan := explain(t, db, c.q)
		for _, w := range c.want {
			if !strings.Contains(plan, w) {
				t.Errorf("%s\nplan has no %q:\n%s", c.q, w, plan)
			}
		}
	}
	// どの文も、右の定数だけをキーにした lookup JOIN にならない
	for _, q := range subqueryScript[1:6] {
		if plan := explain(t, db, q); strings.Contains(plan, "keys: 'bzb_include_rss'") {
			t.Errorf("%s\nlooks up the constant key for every row:\n%s", q, plan)
		}
	}
}

// metaQueryData は、種類 news の記事 600 件と、記事ごとにメタ 16 件（ACF のフィールドとその _ の組など）を入れる
// （ヘッドレスの API の検証と同じ形。category は 10 通り、priority と event_date は記事ごとに違う値）。
func metaQueryData() []string {
	return []string{
		"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) " +
			"WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 600) " +
			"SELECT 1, '2024-01-01 00:00:00' + INTERVAL i * 7 MINUTE, 'c', CONCAT('News ', i), '', 'publish', CONCAT('news-', i), '', '', '', 'news' FROM n ORDER BY i",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) " +
			"WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 600), k (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j < 15) " +
			"SELECT i, ELT(j + 1, 'category', '_category', 'priority', '_priority', 'event_date', '_event_date', 'featured', '_featured', 'summary', '_summary', 'source', '_source', '_edit_lock', '_edit_last', '_thumbnail_id', '_wp_old_slug'), " +
			"ELT(j + 1, CONCAT('cat', (i * 7) % 10), 'field_category', (i * 37) % 1000, 'field_priority', DATE_FORMAT('2024-01-01' + INTERVAL (i * 13) % 730 DAY, '%Y%m%d'), 'field_event_date', IF(i % 9 = 0, '1', '0'), 'field_featured', CONCAT('Summary ', i), 'field_summary', 'wire', 'field_source', '1700000000:1', '1', '0', CONCAT('news-old-', i)) " +
			"FROM n, k ORDER BY i, j",
		"ANALYZE TABLE wp_posts, wp_postmeta",
	}
}

// WP_Query の meta_query と meta_value での並べ替えが作る、wp_posts と wp_postmeta の INNER JOIN（ヘッドレスの API の検証）。
var metaQueryScript = []string{
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID, wp_posts.post_title FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID, wp_posts.post_title FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'priority' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value+0 DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID, wp_posts.post_title FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'event_date' AND mt1.meta_value >= '20250101' ) AND ( mt2.meta_key = 'event_date' AND mt2.meta_value <= '20250331' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value ASC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID, wp_posts.post_title FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'category' AND mt1.meta_value = 'cat3' ) AND ( mt2.meta_key = 'featured' AND mt2.meta_value = '1' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	// 段階 83: 同じ値の多い meta_value+0 の並べ替え（Project 越しの TopN。同じ順位の行の並び）と、2 ページ目
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'featured' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value+0 DESC, wp_posts.post_date DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'priority' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value+0 DESC LIMIT 20, 20",
	"SELECT FOUND_ROWS()",
	// ホスティング側が索引を足して組み立て直した two_filters（同じフィールドの条件を 1 つの句に、CAST せずに並べる）
	"ALTER TABLE wp_postmeta ADD INDEX meta_key_value (meta_key(191), meta_value(64)), ADD INDEX post_meta_key (post_id, meta_key(191))",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) AND ( mt1.meta_key = 'featured' AND mt1.meta_value = '1' ) AND mt2.meta_key = 'event_date' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(mt2.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
}

// meta_query の JOIN の結果（並びと FOUND_ROWS() を含む）が、本物の MySQL と同じ。
func TestMetaQueryJoinsMatchMySQL(t *testing.T) {
	stmts := append(append(append([]string{wpSQLMode}, wpSchema()...), metaQueryData()...), metaQueryScript...)
	checkGolden(t, "../testdata/metaquery_golden.txt", "heimdb_golden80m", stmts)
}

// meta_query の JOIN は、右の定数（meta_key）だけで引いて記事ごとに読み直さない。段階 83 から、記事から post_id で
// メタを引いて捨てる向きではなく、meta_key の範囲から記事を主キーで引く（MySQL と同じ）。並べ替えは TopN。
func TestMetaQueryJoinPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), metaQueryData()...) {
		mustExec(t, db, q)
	}
	// two_filters は、条件の強い category と featured から読み、並べ替えに使うだけの event_date（全記事）からは読まない
	if plan := explain(t, db, metaQueryScript[6]); strings.Contains(plan, "[{[event_date, event_date]}]") {
		t.Errorf("two_filters reads the event_date range:\n%s", plan)
	}
	// プレフィックスのインデックス（meta_key(191)）を 1 つの短い値で引くときは、読んだ行を並べ替えない。SELECT の文は、
	// JOIN で記事ごとに引く走査を使い回す
	sorts, opened := prefixSorts.Load(), scansOpened.Load()
	mustExec(t, db, metaQueryScript[0])
	if n := prefixSorts.Load() - sorts; n != 0 {
		t.Errorf("category sorted the prefix index rows %d times", n)
	}
	mustExec(t, db, "SELECT COUNT(m.meta_id) FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_edit_lock' WHERE p.post_type = 'news'")
	if n := scansOpened.Load() - opened; n > 20 {
		t.Errorf("opened %d scans for 600 lookups", n)
	}
	for _, q := range []string{metaQueryScript[2], metaQueryScript[4]} { // priority と range
		plan := explain(t, db, q)
		if strings.Contains(plan, "keys: wp_posts.ID") || !strings.Contains(plan, "TopN") {
			t.Errorf("%s\nreads posts and looks up their meta by post_id, or sorts all rows:\n%s", q, plan)
		}
	}
	for _, q := range metaQueryScript {
		if !strings.HasPrefix(q, "SELECT SQL_CALC_FOUND_ROWS") {
			continue
		}
		plan := explain(t, db, q)
		for _, key := range []string{"'category'", "'priority'", "'event_date'", "'featured'"} {
			if strings.Contains(plan, "keys: "+key) {
				t.Errorf("%s\nlooks up the constant key %s for every row:\n%s", q, key, plan)
			}
		}
	}
}
