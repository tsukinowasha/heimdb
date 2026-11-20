package backend

import (
	"bytes"
	"database/sql"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// stage98Options は、ホスティング側の報告と同じ定義の wp_options に、autoload の値の行を入れる文を返す。dom は 9 割を超える
// 値の名前で、rows は全体の行数、share はその値の割合。ほかの値は yes 211 : no 41 : on 7 : auto 5 の比で割り振る。
func stage98Options(dom string, rows int, share float64) []string {
	stmts := []string{"CREATE TABLE `wp_options` (`option_id` bigint unsigned NOT NULL AUTO_INCREMENT, `option_name` varchar(191) NOT NULL DEFAULT '', `option_value` longtext NOT NULL, `autoload` varchar(20) NOT NULL DEFAULT 'yes', PRIMARY KEY (`option_id`), UNIQUE KEY `option_name` (`option_name`), KEY `autoload` (`autoload`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci"}
	rest := rows - int(float64(rows)*share+0.5)
	type count struct {
		v string
		n int
	}
	counts := []count{{"yes", rest * 211 / 264}, {"no", rest * 41 / 264}, {"on", rest * 7 / 264}, {"auto", max(rest*5/264, 1)}}
	others := 0
	for _, c := range counts {
		others += c.n
	}
	counts = append(counts, count{dom, rows - others})
	id := 0
	var buf []string
	flush := func() {
		if len(buf) > 0 {
			stmts = append(stmts, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES "+strings.Join(buf, ","))
			buf = nil
		}
	}
	for _, c := range counts {
		for range c.n {
			id++
			buf = append(buf, fmt.Sprintf("('opt_%06d','x','%s')", id, c.v))
			if len(buf) == 1000 {
				flush()
			}
		}
	}
	flush()
	return stmts
}

// 9 割を超える値（期限切れの transient の残骸で増えた autoload の off）の手前にある少ない値も、等号と IN で
// autoload の索引を使う（docs/plan.md の段階 98）。前は auto と no が off と同じバケットに入り、表を全部読んでいた。
// 本物の MySQL 8.0.46 も、どの分布でもすべての値で索引を使う（auto 5 行を ref、IN を range）。多い値そのもの
// （off）と、それを含む IN は今までどおり表を全部読む。
// 53,078 行は間引いた標本になる（主キーのハッシュで決まり、auto は 5 行のうち 2 行が残る）。auto が標本から消えても、
// バケット [auto, no] の上端の no の等号が同じ戻りを捕まえる。
func TestStage98DominantValuePlans(t *testing.T) {
	for _, tc := range []struct {
		dom   string
		rows  int
		share float64
	}{
		{"aaa", 53078, 0.995},
		{"off", 53078, 0.995}, // 報告の形（間引いた標本）
		{"zzz", 53078, 0.995},
		{"off", 53078, 0.95},
		{"off", 53078, 0.90},
		{"off", 10000, 0.995}, // 間引かない
		{"zzz", 10000, 0.995},
	} {
		name := fmt.Sprintf("%s/%d/%g", tc.dom, tc.rows, tc.share)
		dsn, stop := startServer(t, t.TempDir())
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		for _, q := range stage98Options(tc.dom, tc.rows, tc.share) {
			mustExec(t, db, q)
		}
		mustExec(t, db, "ANALYZE TABLE wp_options")
		for _, w := range []string{"autoload = 'auto'", "autoload = 'no'", "autoload = 'on'", "autoload = 'yes'",
			"autoload IN ( 'yes', 'on', 'auto-on', 'auto' )", "autoload IN ('yes','on','auto','yes')"} {
			checkPlan(t, db, name+" "+w, "SELECT option_name, option_value FROM wp_options WHERE "+w,
				[]string{"index: [wp_options.autoload]"}, []string{"Table\n"})
		}
		if tc.share > 0.9 {
			for _, w := range []string{"autoload = '" + tc.dom + "'", "autoload IN ('" + tc.dom + "','auto')"} {
				checkPlan(t, db, name+" "+w, "SELECT option_name, option_value FROM wp_options WHERE "+w,
					[]string{"Table\n"}, []string{"IndexedTableAccess"})
			}
		}
		db.Close()
		stop()
	}
}

// 結果は本物の MySQL のゴールデンと同じ。
func TestStage98MatchMySQL(t *testing.T) {
	stmts := append([]string{wpSQLMode}, stage98Options("off", 53078, 0.995)...)
	stmts = append(stmts, "ANALYZE TABLE wp_options",
		"SELECT option_name, option_value FROM wp_options WHERE autoload IN ( 'yes', 'on', 'auto-on', 'auto' )",
		"SELECT option_name FROM wp_options WHERE autoload = 'auto'",
		"SELECT option_name FROM wp_options WHERE autoload = 'no'",
		"SELECT autoload, COUNT(*) FROM wp_options GROUP BY autoload ORDER BY autoload")
	checkGolden(t, "../testdata/stage98_golden.txt", "heimdb_golden98", stmts)
}

// bucketEnds の切り方の性質: どのバケットも空でなく、同じ値が 2 つのバケットにまたがらず、バケットの途中から
// 始まる並びはそのバケットに入れない（並びの手前で閉じる）。数は約 2 × 行数 / per を超えない。
func TestBucketEnds(t *testing.T) {
	r := rand.New(rand.NewSource(98))
	check := func(name string, sorted [][]byte, per int) {
		t.Helper()
		ends := bucketEnds(sorted, per)
		start := 0
		for i, end := range ends {
			if end <= start || end > len(sorted) {
				t.Fatalf("%s: bucket %d is [%d, %d)", name, i, start, end)
			}
			if end < len(sorted) && bytes.Equal(sorted[end], sorted[end-1]) {
				t.Fatalf("%s: value %x spans buckets at %d", name, sorted[end], end)
			}
			// 切れ目（start+per）をまたぐ並びは、バケットの先頭から始まるときだけ入れる
			if end > start+per && !bytes.Equal(sorted[start], sorted[end-1]) {
				t.Fatalf("%s: bucket [%d, %d) took a run past the cut %d", name, start, end, start+per)
			}
			start = end
		}
		if len(sorted) > 0 && start != len(sorted) {
			t.Fatalf("%s: buckets end at %d of %d", name, start, len(sorted))
		}
		if limit := 2*len(sorted)/per + 2; len(ends) > limit {
			t.Fatalf("%s: %d buckets, more than %d", name, len(ends), limit)
		}
	}
	keysOf := func(vals []int) [][]byte {
		slices.Sort(vals)
		out := make([][]byte, len(vals))
		for i, v := range vals {
			out[i] = []byte{byte(v >> 8), byte(v)}
		}
		return out
	}
	repeat := func(v, n int) []int { return slices.Repeat([]int{v}, n) }

	// 報告の形: 少ない値 2 つの後に 99.5% の値、その後に少ない値 2 つ。少ない値は多い値と同じバケットに入らない。
	report := keysOf(slices.Concat(repeat(1, 1), repeat(2, 10), repeat(3, 13200), repeat(4, 2), repeat(5, 53)))
	check("report", report, 133)
	if ends := bucketEnds(report, 133); ends[0] != 11 || ends[1] != 11+13200 {
		t.Errorf("report: ends %v, want [11 13211 ...]", ends)
	}
	// 1 行の値と per を超える並びの繰り返し（バケットの数が最も増える形）
	var worst []int
	for i := range 50 {
		worst = append(worst, slices.Concat([]int{2 * i}, repeat(2*i+1, 120))...)
	}
	check("worst", keysOf(worst), 100)
	check("empty", nil, 10)
	check("one value", keysOf(repeat(7, 500)), 10)
	for i := range 300 {
		n := r.Intn(3000) + 1
		vals := make([]int, n)
		kinds := r.Intn(40) + 1
		for j := range vals {
			// 偏った分布（小さい番号の値ほど多い）
			vals[j] = int(float64(kinds) * r.Float64() * r.Float64() * r.Float64())
		}
		check(fmt.Sprintf("random %d", i), keysOf(vals), (n+99)/100)
	}
}
