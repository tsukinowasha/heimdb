package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// stage101Queries は、段階 101 の文。WP_Query の meta_query で、category と featured の条件に、別のキー（event_date）の
// 並べ替えを組み合わせた two_filters_t（記事 API の形）。featured を '1'（1 割）と '0'（9 割）で比べる形と、featured と
// event_date の別名を入れ替えた形（見積もりで決まり、別名の番号の巡り合わせで決まっていないことを確かめる）。段階 95 の
// CAST の形も同じ順になる。
var stage101Queries = []string{
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) AND ( mt1.meta_key = 'featured' AND mt1.meta_value = '1' ) AND mt2.meta_key = 'event_date' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(mt2.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) AND ( mt1.meta_key = 'featured' AND mt1.meta_value = '0' ) AND mt2.meta_key = 'event_date' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(mt2.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 20, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) AND ( mt2.meta_key = 'featured' AND mt2.meta_value = '1' ) AND mt1.meta_key = 'event_date' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(mt1.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
}

// 結果（並びと FOUND_ROWS() を含む）が、標準の索引でも API 用の索引でも本物の MySQL と同じ。
func TestStage101MatchMySQL(t *testing.T) {
	stmts := append(append([]string{wpSQLMode}, wpSchema()...), metaQueryDataN(2000)...)
	stmts = append(stmts, stage101Queries...)
	stmts = append(stmts, stage101Range, "SELECT FOUND_ROWS()")
	stmts = append(stmts, apiIndexes, "ANALYZE TABLE wp_postmeta")
	stmts = append(stmts, stage101Queries...)
	stmts = append(stmts, stage101Range, "SELECT FOUND_ROWS()")
	checkGolden(t, "../testdata/stage101_golden.txt", "heimdb_golden101", stmts)
}

// category の点（記事の 1 割）から読み、MySQL と同じく featured を event_date より先に引く（段階 101）。前は JOIN の出力の
// 行数の見積もりが featured の条件を数えず、2 つの順が同点で、別名の番号で event_date を先に引いていた。
func TestStage101JoinOrder(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), metaQueryDataN(2000)...) {
		mustExec(t, db, q)
	}
	queries := []string{stage101Queries[0], stage101Queries[2], stage101Queries[4], stage95Queries[0], stage95Queries[2]}
	check := func(name string) {
		t.Helper()
		for i, q := range queries {
			plan := explain(t, db, q)
			f, e := strings.Index(plan, "meta_key = 'featured')"), strings.Index(plan, "meta_key = 'event_date')")
			if !strings.Contains(plan, "category") || f < 0 || e < 0 || f > e {
				t.Errorf("%s, query %d: featured is not looked up before event_date:\n%s", name, i, plan)
			}
		}
	}
	// event_date の範囲を 3 本つなぐ range は、片側の範囲から記事ごとに引く lookup JOIN だけになる（前は、2 本の範囲をそれぞれ
	// 読んでハッシュ JOIN でつないでいた。MySQL は記事を索引だけで読み、記事ごとに 3 本を引く）
	rangeCheck := func(name string) {
		t.Helper()
		if plan := explain(t, db, stage101Range); strings.Contains(plan, "HashJoin") || strings.Count(plan, "LookupJoin") != 3 {
			t.Errorf("%s: range is not a chain of lookup joins:\n%s", name, plan)
		}
	}
	check("standard indexes")
	rangeCheck("standard indexes")
	mustExec(t, db, apiIndexes)
	mustExec(t, db, "ANALYZE TABLE wp_postmeta")
	check("API indexes")
	rangeCheck("API indexes")
}

// stage101Range は、段階 83 からのベンチの range（WP_Query の NUMERIC の比べで作る event_date の範囲の JOIN 3 本）の ID の形。
const stage101Range = "SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'event_date' AND CAST(mt1.meta_value AS SIGNED) >= '20240301' ) AND ( mt2.meta_key = 'event_date' AND CAST(mt2.meta_value AS SIGNED) <= '20240531' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value ASC, wp_posts.ID DESC LIMIT 0, 20"
