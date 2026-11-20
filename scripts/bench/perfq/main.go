// perfq は、記事 API の形のデータ（種類 news の記事と、記事あたり 16 行のメタ）を入れ、WP_Query の meta_query が作る文の
// 時間と、同時に流したときの件数を測る（docs/bench.md、docs/plan.md の段階 100）。
//
// 文の名前（category など）は、WordPress の WP_Query が LIMIT のある文を分けて流す 4 本の組（split_the_query）を表す:
// ID だけを返す文、SELECT FOUND_ROWS()、記事を ID で引く文、メタを ID で引く文。IN の ID は 1 本目が返した順に並べる
// （_prime_post_caches と update_meta_cache は並べ替えない）。名前に _all を付けると、段階 99 までのベンチの形
// （wp_posts.* を返す 1 本と FOUND_ROWS()）を流す。
//
//	perfq load <dsn> <schema.sql> <posts> [apiindex]
//	perfq bench <dsn> <runs> [names...]
//	perfq conc <dsn> <workers> <seconds> [names...]
//	perfq check <dsn> <mysql-dsn> [names...]
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const sqlMode = "SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"

// WP_Query が分けずに組み立てる文（fields が wp_posts.*）。分けるときは SELECT の列だけが wp_posts.ID に替わる。
var shapes = map[string]string{
	"list20":        "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts WHERE 1=1 AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 20",
	"category":      "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT 0, 20",
	"range":         "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'event_date' AND CAST(mt1.meta_value AS SIGNED) >= '20250101' ) AND ( mt2.meta_key = 'event_date' AND CAST(mt2.meta_value AS SIGNED) <= '20250331' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value ASC, wp_posts.ID DESC LIMIT 0, 20",
	"priority":      "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'priority' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value DESC, wp_posts.ID DESC LIMIT 0, 20",
	"two":           "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'category' AND mt1.meta_value = 'cat3' ) AND ( mt2.meta_key = 'featured' AND CAST(mt2.meta_value AS SIGNED) = '1' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value DESC, wp_posts.ID DESC LIMIT 0, 20",
	"two0":          "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'event_date' AND ( ( mt1.meta_key = 'category' AND mt1.meta_value = 'cat3' ) AND ( mt2.meta_key = 'featured' AND CAST(mt2.meta_value AS SIGNED) = '0' ) ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value DESC, wp_posts.ID DESC LIMIT 0, 20",
	"range_t":       "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'event_date' AND wp_postmeta.meta_value BETWEEN '20250101' AND '20250331' ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(wp_postmeta.meta_value AS CHAR) ASC, wp_posts.ID DESC LIMIT 0, 20",
	"priority_t":    "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'priority' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(wp_postmeta.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 0, 20",
	"two_filters_t": "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) AND ( mt1.meta_key = 'featured' AND mt1.meta_value = '1' ) AND mt2.meta_key = 'event_date' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(mt2.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 0, 20",
}

var order = []string{"list20", "category", "range", "priority", "two", "two0", "range_t", "priority_t", "two_filters_t"}

// 組の 4 本の名前（bench が時間を出す列の順）
var steps = []string{"ids", "found", "posts", "meta"}

// result は 1 つの組を流した結果。old の形では ids が返した行の ID、posts と meta は 0。
type result struct {
	ids                []string
	found, posts, meta int
	took               [4]time.Duration
}

func idQuery(name string) string {
	return strings.Replace(shapes[name], "SQL_CALC_FOUND_ROWS wp_posts.*", "SQL_CALC_FOUND_ROWS wp_posts.ID", 1)
}

// run は名前の組を流す。_all の名前は段階 99 までの形。
func run(c *sql.Conn, name string) result {
	var r result
	base, old := strings.CutSuffix(name, "_all")
	q, ok := shapes[base]
	if !ok {
		fail("unknown query %q", name)
	}
	if !old {
		q = idQuery(base)
	}
	t := time.Now()
	r.ids = firstColumn(c, q)
	r.took[0] = time.Since(t)
	t = time.Now()
	if err := c.QueryRowContext(context.Background(), "SELECT FOUND_ROWS()").Scan(&r.found); err != nil {
		fail("%v: SELECT FOUND_ROWS()", err)
	}
	r.took[1] = time.Since(t)
	if old || len(r.ids) == 0 {
		return r
	}
	in := strings.Join(r.ids, ",")
	t = time.Now()
	r.posts = len(firstColumn(c, "SELECT wp_posts.* FROM wp_posts WHERE ID IN ("+in+")"))
	r.took[2] = time.Since(t)
	t = time.Now()
	r.meta = len(firstColumn(c, "SELECT post_id, meta_key, meta_value FROM wp_postmeta WHERE post_id IN ("+in+") ORDER BY meta_id ASC"))
	r.took[3] = time.Since(t)
	return r
}

// firstColumn は文を流し、各行の 1 列目を文字列で返す（残りの列も受け取る）。
func firstColumn(c *sql.Conn, q string) []string {
	rows, err := c.QueryContext(context.Background(), q)
	if err != nil {
		fail("%v: %s", err, q)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		fail("%v", err)
	}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var out []string
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			fail("%v: %s", err, q)
		}
		out = append(out, string(vals[0]))
	}
	if err := rows.Err(); err != nil {
		fail("%v: %s", err, q)
	}
	return out
}

func main() {
	need := map[string]int{"load": 5, "bench": 4, "conc": 5, "check": 4}
	if len(os.Args) < 2 || need[os.Args[1]] == 0 || len(os.Args) < need[os.Args[1]] {
		fail("usage: perfq load|bench|conc|check ... (see the comment at the top of main.go)")
	}
	switch os.Args[1] {
	case "load":
		load(os.Args[2], os.Args[3], atoi(os.Args[4]), len(os.Args) > 5 && os.Args[5] == "apiindex")
	case "bench":
		bench(os.Args[2], atoi(os.Args[3]), names(os.Args[4:]))
	case "conc":
		conc(os.Args[2], atoi(os.Args[3]), atoi(os.Args[4]), names(os.Args[5:]))
	case "check":
		check(os.Args[2], os.Args[3], names(os.Args[4:]))
	default:
		fail("unknown command %q", os.Args[1])
	}
}

func names(args []string) []string {
	if len(args) == 0 {
		return order
	}
	return args
}

// bench は名前ごとに 3 回流してから runs 回測り、組の合計の中央値と、小さい方から 9 割の位置の値（10 回なら最大）、
// 4 本それぞれの中央値を ms で出す。
func bench(dsn string, runs int, names []string) {
	if runs < 1 {
		fail("runs must be at least 1")
	}
	c := session(open(dsn, 1))
	fmt.Printf("name\ttotal\tp90\t%s\n", strings.Join(steps, "\t"))
	for _, name := range names {
		for range 3 {
			run(c, name)
		}
		var total []time.Duration
		var each [4][]time.Duration
		for range runs {
			r := run(c, name)
			var sum time.Duration
			for i, d := range r.took {
				each[i] = append(each[i], d)
				sum += d
			}
			total = append(total, sum)
		}
		line := fmt.Sprintf("%s\t%.2f\t%.2f", name, ms(median(total)), ms(pct(total, 9)))
		for _, ds := range each {
			line += fmt.Sprintf("\t%.2f", ms(median(ds)))
		}
		fmt.Println(line)
	}
}

// conc は workers 本の接続で名前を順に回して流し、終えた組の数を毎秒で出す（終わりの時刻の後に終えた組も数え、
// 最後の組が終わるまでの時間で割る）。
func conc(dsn string, workers, secs int, names []string) {
	db := open(dsn, workers)
	var done atomic.Int64
	var wg sync.WaitGroup
	begin := time.Now()
	end := begin.Add(time.Duration(secs) * time.Second)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := session(db)
			for i := w; time.Now().Before(end); i++ {
				run(c, names[i%len(names)])
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("conc\t%d workers\t%.2f per second\n", workers, float64(done.Load())/time.Since(begin).Seconds())
}

// check は 2 つの DB で組を流し、FOUND_ROWS() と各文の行数が違えば止まる。ID の集まりと並びの違いは出すだけ
// （priority と range は、同じ値の行の並びが MySQL と違う）。
func check(dsn, mysqlDSN string, names []string) {
	a, b := session(open(dsn, 1)), session(open(mysqlDSN, 1))
	bad := false
	for _, name := range names {
		x, y := run(a, name), run(b, name)
		if x.found != y.found || len(x.ids) != len(y.ids) || x.posts != y.posts || x.meta != y.meta {
			fmt.Printf("%s\tDIFFERENT\tfound %d/%d ids %d/%d posts %d/%d meta %d/%d\n", name, x.found, y.found, len(x.ids), len(y.ids), x.posts, y.posts, x.meta, y.meta)
			bad = true
			continue
		}
		status := "same"
		if !slices.Equal(x.ids, y.ids) {
			xs, ys := slices.Sorted(slices.Values(x.ids)), slices.Sorted(slices.Values(y.ids))
			if slices.Equal(xs, ys) {
				status = "order differs"
			} else {
				status = "ids differ"
			}
		}
		fmt.Printf("%s\t%s\tfound %d ids %d posts %d meta %d\n", name, status, x.found, len(x.ids), x.posts, x.meta)
		if status != "same" {
			fmt.Printf("  %s\n  %s\n", strings.Join(x.ids, ","), strings.Join(y.ids, ","))
		}
	}
	if bad {
		os.Exit(1)
	}
}

func open(dsn string, n int) *sql.DB {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fail("%v", err)
	}
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
	return db
}

func session(db *sql.DB) *sql.Conn {
	c, err := db.Conn(context.Background())
	if err != nil {
		fail("%v", err)
	}
	for _, q := range []string{"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci", sqlMode} {
		if _, err := c.ExecContext(context.Background(), q); err != nil {
			fail("%v: %s", err, q)
		}
	}
	return c
}

func median(ds []time.Duration) time.Duration { return pct(ds, 5) }

// pct は並べた値の、小さい方から tenths 割の位置（添字 len*tenths/10）の値を返す（5 なら中央値）。
func pct(ds []time.Duration, tenths int) time.Duration {
	s := slices.Sorted(slices.Values(ds))
	return s[len(s)*tenths/10]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fail("%v", err)
	}
	return n
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "perfq: "+format+"\n", args...)
	os.Exit(1)
}

// load は schema.sql（internal/testdata/wp_schema.sql）の表を作り、記事 posts 件（1000 の倍数）とメタ 16 × posts 行を
// 入れる。apiindex なら、ヘッドレスの API が wp_postmeta に足した 2 つの索引も付ける。related と topics の値は PHP の serialize の
// 長さが合っていないが、段階 83 からのデータと同じにするため直さない。
func load(dsn, schema string, posts int, apiIndex bool) {
	if posts < 1000 || posts%1000 != 0 {
		fail("posts must be a multiple of 1000")
	}
	conn := session(open(dsn, 1))
	b, err := os.ReadFile(schema)
	if err != nil {
		fail("%v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "--") {
			lines = append(lines, l)
		}
	}
	var stmts []string
	for _, s := range strings.Split(strings.Join(lines, "\n"), ";\n") {
		if s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";")); s != "" {
			stmts = append(stmts, s)
		}
	}
	stmts = append(stmts, "SET cte_max_recursion_depth = 10000")
	body := "CONCAT('<p>', REPEAT('News API の検証用の本文です。HeimDB から読んで JSON を作る時間を測ります。', 12), '</p>')"
	for c := 0; c < posts/1000; c++ {
		lo, hi := c*1000+1, c*1000+1000
		stmts = append(stmts,
			fmt.Sprintf("INSERT INTO wp_posts (ID, post_author, post_date, post_date_gmt, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type, post_modified, post_modified_gmt) "+
				"WITH RECURSIVE n (i) AS (SELECT %d UNION ALL SELECT i + 1 FROM n WHERE i < %d) "+
				"SELECT i, 1, '2020-01-01 00:00:00' + INTERVAL i * 97 MINUTE, '2020-01-01 00:00:00' + INTERVAL i * 97 MINUTE, %s, CONCAT('News ', i), '', 'publish', CONCAT('news-', i), '', '', '', 'news', '2025-01-01', '2025-01-01' FROM n", lo, hi, body),
			fmt.Sprintf("INSERT INTO wp_postmeta (meta_id, post_id, meta_key, meta_value) "+
				"WITH RECURSIVE n (i) AS (SELECT %d UNION ALL SELECT i + 1 FROM n WHERE i < %d), k (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j < 15) "+
				"SELECT (i - 1) * 16 + j + 1, i, ELT(j + 1, 'category', '_category', 'priority', '_priority', 'event_date', '_event_date', 'featured', '_featured', 'summary', '_summary', 'body', '_body', 'related', '_related', 'topics', '_topics'), "+
				"ELT(j + 1, CONCAT('cat', (i * 7) %% 10), 'field_category', (i * 37) %% 100 + 1, 'field_priority', DATE_FORMAT('2020-01-01' + INTERVAL (i * 13) %% 2400 DAY, '%%Y%%m%%d'), 'field_event_date', IF(i %% 9 = 0, '1', '0'), 'field_featured', CONCAT('News ', i, ' の要約'), 'field_summary', %s, 'field_body', CONCAT('a:1:{i:0;s:3:\"', (i * 11) %% %d + 1, '\";}'), 'field_related', CONCAT('a:1:{i:0;s:2:\"', i %% 20 + 1, '\";}'), 'field_topics') "+
				"FROM n, k", lo, hi, body, posts))
	}
	if apiIndex {
		stmts = append(stmts, "ALTER TABLE wp_postmeta ADD INDEX key_value (meta_key(191), meta_value(64)), ADD INDEX post_key (post_id, meta_key(191))")
	}
	stmts = append(stmts, "ANALYZE TABLE wp_posts, wp_postmeta")
	for _, q := range stmts {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			fail("%v: %s", err, q[:min(len(q), 200)])
		}
	}
	fmt.Println("loaded", posts)
}
