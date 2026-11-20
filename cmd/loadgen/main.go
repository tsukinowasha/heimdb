// loadgen は、WordPress のページの文の並びを多くの接続から同時に流し、MySQL 互換のサーバーの負荷への強さを測る
// （docs/bench.md の段階 126）。1 つのページは、1 本の接続で順に流す決まった文の並び（WordPress の SET NAMES と
// sql_mode の後に、そのページで WordPress が流した文）。読みのページ（scripts/bench/load/pages-read.json）と
// 書き込みのページ（scripts/bench/load/pages-write.json）を -write で決めた割合で混ぜる。
//
//	loadgen -dsn 'root:rootpw@tcp(10.0.0.2:3316)/wpbench' -pages pages-read.json -conns 32 -duration 60s
//	loadgen -dsn ... -pages pages-read.json,pages-write.json -write 0.05 -conns 32 -duration 60s
//	loadgen -dsn ... -pages pages-read.json -rate 120 -conns 128 -duration 60s   # 決まった速さ（ページ毎秒）
//
// -rate を付けないときは、各接続が前のページの応答を待ってすぐ次を流す（閉じた形。速さの上限を測る）。
// -rate を付けると、ページの予定の時刻を前の応答を待たずに決め（k 番目のページは始めから k/rate 秒）、空いた接続が
// その時刻に始める。接続が足りずに遅れたページは、時間に予定の時刻からの遅れを含める（応答の遅さに隠れて負荷が
// 減って見えることを避ける）。遅れ（lateness_ms）と、始めてから終えるまで（service_ms）も別に書く。
//
// 結果は JSON で、ページ毎秒と文毎秒、ページの種類ごとと全体の時間の p50、p95、p99、最大、MySQL の誤りの番号ごとの数、
// 接続の誤り、接続を作れなかった数、-interval ごとの値（series）、Linux なら負荷の側の CPU を書く。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

func main() {
	dsn := flag.String("dsn", "root:rootpw@tcp(127.0.0.1:3360)/wpbench", "DSN of the server (go-sql-driver/mysql form)")
	pages := flag.String("pages", "", "comma-separated workload JSON files (see workload.go)")
	conns := flag.Int("conns", 8, "number of connections, each running one page at a time")
	rate := flag.Float64("rate", 0, "pages per second for an open loop (0: closed loop, as fast as possible)")
	duration := flag.Duration("duration", 60*time.Second, "measured time")
	warmup := flag.Duration("warmup", 10*time.Second, "time run before measuring (not counted)")
	write := flag.Float64("write", 0, "fraction of pages that are writes (0.05 for read:write 95:5)")
	seed := flag.Uint64("seed", 1, "seed of the read/write choice")
	interval := flag.Duration("interval", 10*time.Second, "length of the intervals in the series (0: none)")
	stmtTimeout := flag.Duration("stmt-timeout", 60*time.Second, "time limit of one statement (0: none); a statement over it closes its connection")
	dialTimeout := flag.Duration("dial-timeout", 10*time.Second, "time limit of connecting and the session statements")
	label := flag.String("label", "", "label copied to the result")
	out := flag.String("out", "", "file for the JSON result (default stdout)")
	flag.Parse()
	if *pages == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -pages is required")
		os.Exit(2)
	}
	var w *Workload
	for _, f := range strings.Split(*pages, ",") {
		x, err := LoadWorkload(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: %s: %v\n", f, err)
			os.Exit(2)
		}
		if w == nil {
			w = x
		} else {
			w.Merge(x)
		}
	}
	if *write > 0 && len(w.writes) == 0 {
		fmt.Fprintln(os.Stderr, "loadgen: -write needs a file with write pages")
		os.Exit(2)
	}
	if *write < 1 && len(w.reads) == 0 {
		fmt.Fprintln(os.Stderr, "loadgen: -write below 1 needs a file with read pages")
		os.Exit(2)
	}
	mc, err := mysql.ParseDSN(*dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(2)
	}
	if mc.Timeout == 0 {
		mc.Timeout = *dialTimeout
	}
	dial, err := MySQLDialer(mc.FormatDSN())
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg := Config{Conns: *conns, Rate: *rate, Warmup: *warmup, Duration: *duration, WriteFrac: *write, Seed: *seed,
		UniqueBase: time.Now().Unix() * 100000, Interval: *interval, StmtTimeout: *stmtTimeout, DialTimeout: *dialTimeout}
	res, err := Run(ctx, w, dial, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
	res.Label = *label
	b, _ := json.MarshalIndent(res, "", "  ")
	b = append(b, '\n')
	if *out == "" {
		os.Stdout.Write(b)
	} else if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
	all := res.Latency["all"]
	fmt.Fprintf(os.Stderr, "%s %s conns=%d pages/s=%.1f stmts/s=%.0f p50=%.1fms p99=%.1fms failed=%d errors=%v conn_errors=%v connect_failures=%v not_started=%d\n",
		*label, res.Mode, res.Conns, res.PagesPerS, res.StmtsPerS, all.P50, all.P99, res.FailedPages, res.Errors, res.ConnErrors, res.ConnectFails, res.NotStarted)
}
