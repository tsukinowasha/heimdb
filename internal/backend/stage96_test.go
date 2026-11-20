package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// 長さを指定した索引（API 用の索引の key_value と post_key）を、長さより短い点で引くときは、範囲の条件を行ごとに評価し直さない
// （docs/plan.md の段階 96）。結果は、索引のない同じ表と同じ。大小文字と全角（unicode_520_ci で同じ重み）、末尾の空白
// （HeimDB は PAD SPACE を持たないので別の値）、ちょうど長さの値と、先頭が同じ長い値、多バイトの値を入れる。
func TestStage96PrefixPoints(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	k191, a190, a191 := strings.Repeat("k", 191), strings.Repeat("あ", 190), strings.Repeat("あ", 191)
	keys := []string{"abc", "ABC", "ａｂｃ", "abc ", "abcd", "ab", k191, k191 + "x", k191 + "y", a190, a190 + "い", a191, a191 + "い"}
	for _, tbl := range []string{"pm", "pmn"} {
		mustExec(t, db, "CREATE TABLE "+tbl+" (meta_id bigint unsigned NOT NULL AUTO_INCREMENT, post_id bigint unsigned NOT NULL DEFAULT 0, meta_key varchar(255) DEFAULT NULL, meta_value longtext, PRIMARY KEY (meta_id)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	}
	mustExec(t, db, "ALTER TABLE pm ADD INDEX key_value (meta_key(191), meta_value(64)), ADD INDEX post_key (post_id, meta_key(191))")
	mustExec(t, db, "CREATE TABLE p (id bigint unsigned NOT NULL, PRIMARY KEY (id))")
	var vals []string
	for i := 0; i < 400; i++ {
		v := fmt.Sprintf("'v%d'", i%3)
		k := "'" + keys[i%len(keys)] + "'"
		if i%17 == 0 {
			k = "NULL"
		}
		if i%11 == 0 {
			v = "'" + strings.Repeat("w", 64) + fmt.Sprint(i%2) + "'"
		}
		vals = append(vals, fmt.Sprintf("(%d, %s, %s)", i%40+1, k, v))
	}
	for _, tbl := range []string{"pm", "pmn"} {
		mustExec(t, db, "INSERT INTO "+tbl+" (post_id, meta_key, meta_value) VALUES "+strings.Join(vals, ", "))
	}
	mustExec(t, db, "INSERT INTO p SELECT DISTINCT post_id FROM pm")
	mustExec(t, db, "ANALYZE TABLE pm")

	// filtered は、短い点で引けず、範囲の条件を評価し直す文
	cases := []struct {
		where    string
		filtered bool
	}{
		{"meta_key = 'abc'", false},
		{"meta_key = 'ａｂｃ'", false},
		{"meta_key = 'abc '", false},
		{"meta_key = 'abc' AND meta_value = 'v1'", false},
		{"meta_key IN ('ab', 'abcd', 'zzz')", false},
		{"meta_key = '" + a190 + "'", false},
		{"post_id = 3 AND meta_key = 'abc'", false},
		{"meta_key = '" + k191 + "'", true},
		{"meta_key = '" + a191 + "'", true},
		{"meta_key = 'abc' AND meta_value BETWEEN 'v0' AND 'v1'", true},
		{"meta_key = 'abc' AND meta_value = '" + strings.Repeat("w", 64) + "1'", true},
		{"meta_key IS NULL", true},
	}
	for _, c := range cases {
		for _, sel := range []string{"meta_id", "post_id"} { // post_id はエントリの値で返せる（covered）
			q := "SELECT " + sel + " FROM %s WHERE " + c.where + " ORDER BY meta_id"
			want := rowsOf(t, db, fmt.Sprintf(q, "pmn"))
			if plan := explain(t, db, fmt.Sprintf(q, "pm")); !strings.Contains(plan, "IndexedTableAccess(pm)") {
				t.Fatalf("%s does not use an index:\n%s", c.where, plan)
			}
			evals := rangeFilterEvals.Load()
			if got := rowsOf(t, db, fmt.Sprintf(q, "pm")); got != want {
				t.Errorf("%s: %s, want %s", fmt.Sprintf(q, "pm"), got, want)
			}
			if n := rangeFilterEvals.Load() - evals; (n > 0) != c.filtered {
				t.Errorf("%s evaluated the range filter on %d rows", c.where, n)
			}
		}
	}
	// 索引の逆順（ORDER BY の向き）
	q := "SELECT meta_value FROM %s WHERE meta_key = 'abc' ORDER BY meta_key DESC, meta_value DESC, meta_id"
	want := rowsOf(t, db, fmt.Sprintf(q, "pmn"))
	evals := rangeFilterEvals.Load()
	if got := rowsOf(t, db, fmt.Sprintf(q, "pm")); got != want || want == "" {
		t.Errorf("%s: %s, want %s", fmt.Sprintf(q, "pm"), got, want)
	}
	if n := rangeFilterEvals.Load() - evals; n != 0 {
		t.Errorf("%s evaluated the range filter on %d rows", fmt.Sprintf(q, "pm"), n)
	}

	// lookup JOIN: CAST で比べる two_filters は、API 用の索引で featured と event_date を post_key の post_id と短い点で
	// 引く（段階 95）。category の点も含めて、範囲の条件を評価しない。
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), metaQueryDataN(2000)...) {
		mustExec(t, db, q)
	}
	mustExec(t, db, apiIndexes)
	mustExec(t, db, "ANALYZE TABLE wp_postmeta")
	if plan := explain(t, db, stage95Queries[0]); !strings.Contains(plan, "keys: mt1.post_id, 'event_date'") {
		t.Fatalf("does not look up post_key:\n%s", plan)
	}
	evals = rangeFilterEvals.Load()
	if got := queryRows(t, db, stage95Queries[0], false); len(got) == 0 {
		t.Error("no rows")
	}
	if n := rangeFilterEvals.Load() - evals; n != 0 {
		t.Errorf("two_filters evaluated the range filter on %d rows", n)
	}
}
