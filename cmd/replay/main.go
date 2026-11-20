// replay は、HeimDB のクエリログ（-debug で起動したときの「Starting query」の行）から 1 本の接続のクエリを
// 順に取り出し、本物の MySQL と HeimDB の両方に流して、結果が食い違ったクエリを報告する。
// -conn に複数の接続 ID をカンマ区切りで渡すと、その順に続けて流す（テストの前にインストール用の接続がある場合など）。
//
//	go run ./cmd/replay -log heimdb.log                      # 接続ごとのクエリ数を一覧にする
//	go run ./cmd/replay -log heimdb.log -conn 1,2,3 \
//	    -mysql 'root:root@tcp(127.0.0.1:3307)/heimdb_replay' \
//	    -heimdb 'root@tcp(127.0.0.1:3306)/wordpress'
//
// どちらのデータベースも、リプレイの前に中のテーブルをすべて削除する。大事なデータのあるデータベースに向けないこと。
// 既定では最初の食い違いで止まる。-all ですべての食い違いを報告する。
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlcompare"
)

var queryLine = regexp.MustCompile(`msg="Starting query".*connectionID=(\d+) query="((?:[^"\\]|\\.)*)"`)

func main() {
	logPath := flag.String("log", "", "HeimDB debug log file")
	connIDs := flag.String("conn", "", "comma-separated connection IDs to replay in order (empty lists connections)")
	mysqlDSN := flag.String("mysql", "", "DSN of the real MySQL database (will be emptied)")
	heimdbDSN := flag.String("heimdb", "", "DSN of the heimdb database (will be emptied)")
	all := flag.Bool("all", false, "report every mismatch instead of stopping at the first")
	ignore := flag.String("ignore", `(?i)\b(NOW|SYSDATE|RAND|UUID|CONNECTION_ID)\s*\(`, "regexp of queries whose results are not compared")
	contextLines := flag.Int("context", 5, "number of preceding queries to show for a mismatch")
	flag.Parse()

	queries, err := readLog(*logPath)
	if err != nil {
		fail(err)
	}
	if *connIDs == "" {
		listConnections(queries)
		return
	}
	var qs []string
	for _, s := range strings.Split(*connIDs, ",") {
		id, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || len(queries[id]) == 0 {
			fail(fmt.Errorf("no queries for connection %q", s))
		}
		qs = append(qs, queries[id]...)
	}
	if *mysqlDSN == "" || *heimdbDSN == "" {
		fail(fmt.Errorf("-mysql and -heimdb are required"))
	}
	ignoreRe := regexp.MustCompile(*ignore)

	ctx := context.Background()
	real, err := openEmptied(ctx, *mysqlDSN)
	if err != nil {
		fail(fmt.Errorf("mysql: %w", err))
	}
	defer real.Close()
	hdb, err := openEmptied(ctx, *heimdbDSN)
	if err != nil {
		fail(fmt.Errorf("HeimDB: %w", err))
	}
	defer hdb.Close()

	mismatches := 0
	for i, q := range qs {
		want := sqlcompare.Run(ctx, real, q)
		got := sqlcompare.Run(ctx, hdb, q)
		if want == got || ignoreRe.MatchString(q) {
			continue
		}
		mismatches++
		fmt.Printf("=== mismatch at query %d of %d (connections %s)\n", i+1, len(qs), *connIDs)
		for j := max(0, i-*contextLines); j < i; j++ {
			fmt.Printf("  [%d] %s\n", j+1, oneLine(qs[j]))
		}
		fmt.Printf("> [%d] %s\n--- MySQL\n%s\n--- HeimDB\n%s\n", i+1, qs[i], indent(want), indent(got))
		if !*all {
			os.Exit(1)
		}
	}
	fmt.Printf("replayed %d queries, %d mismatches\n", len(qs), mismatches)
	if mismatches > 0 {
		os.Exit(1)
	}
}

// readLog は、ログから接続ごとのクエリを順に読み出す。
func readLog(path string) (map[int][]string, error) {
	if path == "" {
		return nil, fmt.Errorf("-log is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[int][]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		m := queryLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		id, _ := strconv.Atoi(m[1])
		q, err := strconv.Unquote(`"` + m[2] + `"`)
		if err != nil {
			return nil, fmt.Errorf("cannot unquote query %q: %w", m[2], err)
		}
		out[id] = append(out[id], q)
	}
	return out, sc.Err()
}

func listConnections(queries map[int][]string) {
	ids := make([]int, 0, len(queries))
	for id := range queries {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		fmt.Printf("connection %d: %d queries, first: %s\n", id, len(queries[id]), oneLine(queries[id][0]))
	}
}

// openEmptied は 1 本の接続を開き、データベースの中のテーブルをすべて削除する。
func openEmptied(ctx context.Context, dsn string) (*sql.Conn, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, "SHOW TABLES")
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	for _, t := range tables {
		if _, err := conn.ExecContext(ctx, "DROP TABLE `"+t+"`"); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

func oneLine(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 160 {
		q = q[:160] + "..."
	}
	return q
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "replay:", err)
	os.Exit(2)
}
