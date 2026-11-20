package backend

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// インデックスのエントリだけで読む（covered）走査で、pkRowIter.fill が写した主キーとエントリの値が壊れない（docs/plan.md
// の段階 103。走査はエントリを写さずに返す）。エントリの大きさがまちまちで fill のバッファが途中で足りなくなる形、
// 1 回の fill がバッファの上限（keepBlock）を超える形（多バイトの長い値）、同じ文で同じインデックスを 2 回引く形（読み
// 終えた走査を同じ文のほかの検索が使い回す）。結果は、インデックスを使わない同じ文と同じ。
func TestStage103CoveredEntriesKept(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE t103 (id bigint unsigned NOT NULL AUTO_INCREMENT, a int, s varchar(255), PRIMARY KEY (id), KEY k (a, s(10))) DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_520_ci")
	var vals []string
	for i := 1; i <= 900; i++ {
		// 先の行ほど短く、後の行は多バイトで長い（750 バイト近く）
		s := fmt.Sprintf("v%d-%s", i, strings.Repeat("日", (i*7)%250*i/900))
		vals = append(vals, fmt.Sprintf("(%d, '%s')", i%3, s))
	}
	mustExec(t, db, "INSERT INTO t103 (a, s) VALUES "+strings.Join(vals, ", "))
	mustExec(t, db, "ANALYZE TABLE t103")
	sorted := func(q string) []string {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, a+"|"+b)
		}
		slices.Sort(out)
		return out
	}
	for _, q := range []string{
		"SELECT id, s FROM t103%s WHERE a = 1",
		"SELECT x.id, y.s FROM t103 x%s INNER JOIN t103 y%s ON y.a = x.a WHERE x.a = 2 AND x.id < 40",
	} {
		withIndex := strings.ReplaceAll(q, "%s", "")
		if plan := explain(t, db, withIndex); !strings.Contains(plan, "index: [t103.a,t103.s]") {
			t.Fatalf("%s does not read index k:\n%s", withIndex, plan)
		}
		got, want := sorted(withIndex), sorted(strings.ReplaceAll(q, "%s", " IGNORE INDEX (k)"))
		if len(want) < 300 || !slices.Equal(got, want) {
			t.Errorf("%s: %d rows, want %d rows (first difference near %v)", withIndex, len(got), len(want), firstDiff(got, want))
		}
	}
}

func firstDiff(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return a[i] + " / " + b[i]
		}
	}
	return ""
}
