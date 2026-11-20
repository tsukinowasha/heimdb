package backend

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/memo"
)

// stage95Queries は、段階 95 の文。WP_Query の meta_query が NUMERIC の比べで作る two_filters（featured を CAST で比べる。
// WordPress 本体の素の形で、ヘッドレスの API の比べるための形）と、絞り込みの弱い featured = '0'。標準の索引と、API 用の索引を
// 足した後の両方で流す。
var stage95Queries = []string{
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID, wp_posts.post_title FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'category' AND mt1.meta_value = 'cat3' ) AND ( mt2.meta_key = 'featured' AND CAST(mt2.meta_value AS SIGNED) = '1' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value DESC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID, wp_posts.post_title FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'category' AND mt1.meta_value = 'cat3' ) AND ( mt2.meta_key = 'featured' AND CAST(mt2.meta_value AS SIGNED) = '0' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value DESC, wp_posts.ID DESC LIMIT 20, 20",
	"SELECT FOUND_ROWS()",
}

// metaQueryDataN は、metaQueryData と同じ形の記事 n 件（HeimDB の再帰の CTE の上限の 1 万件まで。MySQL の再帰の深さの
// 既定は 1,000）とそのメタ。600 件では、
// category の点から読むのと featured の範囲を読むのの差が小さく、読み始めが決まらない。
func metaQueryDataN(n int) []string {
	stmts := metaQueryData()
	for i := range stmts {
		stmts[i] = strings.ReplaceAll(stmts[i], "i < 600", fmt.Sprintf("i < %d", n))
	}
	return append([]string{fmt.Sprintf("SET cte_max_recursion_depth = %d", n)}, stmts...)
}

// CAST で比べる two_filters の結果（並びと FOUND_ROWS() を含む）が、標準の索引でも API 用の索引でも本物の MySQL と同じ。
func TestStage95MatchMySQL(t *testing.T) {
	stmts := append(append([]string{wpSQLMode}, wpSchema()...), metaQueryDataN(2000)...)
	stmts = append(stmts, stage95Queries...)
	stmts = append(stmts, apiIndexes, "ANALYZE TABLE wp_postmeta")
	stmts = append(stmts, stage95Queries...)
	checkGolden(t, "../testdata/stage95_golden.txt", "heimdb_golden95", stmts)
}

// CAST で比べる two_filters は、MySQL と同じく category の点から読み、featured の範囲（記事の数の行）を読んで CAST で
// 捨てない（段階 95）。API 用の索引では、featured と event_date を (post_id, meta_key) で記事ごとに 1 行ずつ引く。
// category の値の割合が CAST の条件の見積もり（0.1）より大きくても（10 通り、8 通り、5 通り）同じ。
func TestStage95Plans(t *testing.T) {
	for _, kinds := range []int{10, 8, 5} {
		t.Run(fmt.Sprint(kinds), func(t *testing.T) { stage95Plans(t, kinds) })
	}
}

func stage95Plans(t *testing.T, kinds int) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	data := metaQueryDataN(2000)
	for i := range data {
		data[i] = strings.Replace(data[i], "CONCAT('cat', (i * 7) % 10)", fmt.Sprintf("CONCAT('cat', (i * 7) %% %d)", kinds), 1)
	}
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), data...) {
		mustExec(t, db, q)
	}
	for _, q := range []string{stage95Queries[0], stage95Queries[2]} {
		// 標準の索引: category の範囲から読み、event_date の範囲（全記事）からは読まない
		if plan := explain(t, db, q); !strings.Contains(plan, "[{[category, category]}]") || strings.Contains(plan, "[{[event_date, event_date]}]") {
			t.Errorf("standard indexes:\n%s", plan)
		}
	}
	mustExec(t, db, apiIndexes)
	mustExec(t, db, "ANALYZE TABLE wp_postmeta")
	for _, q := range []string{stage95Queries[0], stage95Queries[2]} {
		plan := explain(t, db, q)
		for _, want := range []string{"[{[category, category], [cat3, cat3]}]", ", 'featured'", ", 'event_date'"} {
			if !strings.Contains(plan, want) {
				t.Errorf("API indexes: plan has no %q:\n%s", want, plan)
			}
		}
		if strings.Contains(plan, "[{[featured, featured], [NULL, ∞)}]") {
			t.Errorf("API indexes: reads the featured range:\n%s", plan)
		}
	}
}

// 読んだ後で捨てる条件の割合（filterParts の residual）は、範囲に入らない条件だけを数える（段階 95）。範囲が点で決める列、
// 標本で数える次の列の等号、その列の上か下を決める条件（API 用の索引の key_value の meta_value の BETWEEN。後で narrowRange が
// 範囲に入れる）は数えない。CAST した列の条件と、インデックスのない列の条件は数える。
func TestStage95Residual(t *testing.T) {
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
	var mu sync.Mutex
	got := map[string]float64{}
	orig := memo.FilterSelectivity
	memo.FilterSelectivity = func(rel *memo.Filter) float64 {
		sel, residual := filterParts(nil, rel)
		var conds []string
		for _, f := range rel.Filters {
			conds = append(conds, f.String())
		}
		mu.Lock()
		got[strings.Join(conds, " AND ")] = residual
		mu.Unlock()
		return sel
	}
	defer func() { memo.FilterSelectivity = orig }()
	check := func(q string, want map[string]float64) {
		t.Helper()
		mu.Lock()
		clear(got)
		mu.Unlock()
		explain(t, db, q)
		mu.Lock()
		defer mu.Unlock()
		for cond, w := range want {
			r, ok := got[cond]
			if !ok || math.Abs(r-w) > 1e-9 {
				t.Errorf("%s: residual %v (%v), want %v; all: %v", cond, r, ok, w, got)
			}
		}
	}
	check(stage95Queries[0], map[string]float64{"(mt1.meta_key = 'category') AND (mt1.meta_value = 'cat3')": 0.1})
	mustExec(t, db, apiIndexes)
	mustExec(t, db, "ANALYZE TABLE wp_postmeta")
	check(stage95Queries[0], map[string]float64{
		"(mt1.meta_key = 'category') AND (mt1.meta_value = 'cat3')":               1,
		"(mt2.meta_key = 'featured') AND (convert(mt2.meta_value, signed) = '1')": 0.1,
	})
	check(stage84Queries[4], map[string]float64{
		"(wp_postmeta.meta_key = 'event_date') AND (wp_postmeta.meta_value >= '20250101') AND (wp_postmeta.meta_value <= '20250331')": 1,
	})
}
