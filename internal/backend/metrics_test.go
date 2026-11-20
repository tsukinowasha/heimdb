package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/tsukinowasha/heimdb/internal/metrics"
)

// scrape は、/metrics を読み、解析器に通して、名前とラベルの組（例: heimdb_statements_total{type=select}）から値への
// マップと、読んだ文を返す。
func scrape(t *testing.T, url string) (map[string]float64, string) {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	fams, err := textParser().TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	out := map[string]float64{}
	for name, f := range fams {
		for _, m := range f.Metric {
			key := name
			var labels []string
			for _, l := range m.Label {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			if len(labels) > 0 {
				key += "{" + strings.Join(labels, ",") + "}"
			}
			switch f.GetType() {
			case dto.MetricType_GAUGE:
				out[key] = m.GetGauge().GetValue()
			case dto.MetricType_COUNTER:
				out[key] = m.GetCounter().GetValue()
			case dto.MetricType_HISTOGRAM:
				out[key+"_count"] = float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return out, string(body)
}

// 指標の値が実際の状態と合い、文の中身、ユーザー名、表の名前が出ない（docs/plan.md の段階 73）。
func TestMetrics(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	pro.Version = "9.9.9"
	hs := httptest.NewServer(metrics.Handler(pro.WriteMetrics))
	defer hs.Close()
	db := openRoot(t, addr)
	defer db.Close()
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE TABLE secret_tbl (id INT PRIMARY KEY, v TEXT)")

	m0, _ := scrape(t, hs.URL)
	for _, k := range []string{"heimdb_build_info{version=9.9.9}", "heimdb_start_time_seconds", "heimdb_replica", "heimdb_disk_bytes",
		"heimdb_branch_size_bytes{branch=main}", "heimdb_memory_live_bytes", "heimdb_engine_caches", "heimdb_cache_size_bytes", "go_goroutines",
		"heimdb_last_commit_timestamp_seconds", "heimdb_statement_duration_seconds_count", "heimdb_watch_changes_total"} {
		if _, ok := m0[k]; !ok {
			t.Errorf("no %s", k)
		}
	}
	if m0["heimdb_branches"] != 1 || m0["heimdb_open_transactions"] != 0 || m0["heimdb_running_statements"] != 0 || m0["heimdb_engine_caches"] != 0 || m0["heimdb_replica"] != 0 || m0["heimdb_connections"] < 1 {
		t.Errorf("initial: %v", m0)
	}
	if now := float64(time.Now().Unix()); m0["heimdb_last_commit_timestamp_seconds"] < now-60 || m0["heimdb_last_commit_timestamp_seconds"] > now+1 {
		t.Errorf("last commit %v, now %v", m0["heimdb_last_commit_timestamp_seconds"], now)
	}

	// 文の種類ごとの数、エラーの番号、時間
	for i := 0; i < 3; i++ {
		exec("SELECT * FROM secret_tbl")
	}
	exec("/* c */ INSERT INTO secret_tbl VALUES (1, 'password123')")
	exec("UPDATE secret_tbl SET v = 'x'")
	// 準備した文は、実行を 1 つと数える（準備は数えない）。
	if _, err := db.ExecContext(ctx, "UPDATE secret_tbl SET v = ? WHERE id = ?", "z", 1); err != nil {
		t.Fatal(err)
	}
	exec("CALL heimdb_branch_create('feature')")
	if _, err := db.ExecContext(ctx, "SELECT * FROM no_such_table"); err == nil {
		t.Fatal("no error")
	}
	// 構文のエラーは、接続先に返す番号（go-mysql-server は 1105）で数える。
	_, err := db.ExecContext(ctx, "SELEC 1")
	var syntaxErr *mysql.MySQLError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("SELEC 1: %v", err)
	}
	m1, body := scrape(t, hs.URL)
	diff := func(k string) float64 { return m1[k] - m0[k] }
	for k, want := range map[string]float64{
		"heimdb_statements_total{type=select}": 4, "heimdb_statements_total{type=insert}": 1, "heimdb_statements_total{type=update}": 2,
		"heimdb_statements_total{type=call}": 1, "heimdb_statements_total{type=other}": 1,
		"heimdb_statement_errors_total{code=1146}": 1, fmt.Sprintf("heimdb_statement_errors_total{code=%d}", syntaxErr.Number): 1,
		"heimdb_statement_duration_seconds_count": 9, "heimdb_branches": 1,
	} {
		if got := diff(k); got != want {
			t.Errorf("%s grew by %v, want %v\n%s", k, got, want, body)
		}
	}
	if v, ok := m0["heimdb_statement_errors_total{code=1205}"]; !ok || v != 0 {
		t.Errorf("no zero count of 1205 before it happens\n%s", body)
	}
	if _, ok := m1["heimdb_branch_size_bytes{branch=feature}"]; !ok {
		t.Error("no size of the branch feature")
	}
	for _, s := range []string{"secret_tbl", "password123", "no_such_table", "SELEC", "root", testDB} {
		if strings.Contains(body, s) {
			t.Errorf("metrics show %q\n%s", s, body)
		}
	}

	// 開いたままのトランザクション（書き込みのロックを持つ）と実行中の文があっても返り、数に出る。
	txdb := openRoot(t, addr)
	defer txdb.Close()
	tx, err := txdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO secret_tbl VALUES (2, 'y')"); err != nil {
		t.Fatal(err)
	}
	sleep := openRoot(t, addr)
	defer sleep.Close()
	done := make(chan error, 1)
	go func() { _, err := sleep.ExecContext(ctx, "SELECT SLEEP(1)"); done <- err }()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	m2, _ := scrape(t, hs.URL)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("scrape took %s", d)
	}
	if m2["heimdb_open_transactions"] < 1 || m2["heimdb_running_statements"] != 1 || m2["heimdb_connections"] < 3 || m2["heimdb_connections_total"] < 3 {
		t.Errorf("while running: %v", m2)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// 遅い文
	if err := SetSlowQuery(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer SetSlowQuery(0)
	exec("SELECT SLEEP(0.1)")
	exec("SELECT 1")
	if m3, _ := scrape(t, hs.URL); m3["heimdb_slow_statements_total"]-m2["heimdb_slow_statements_total"] != 1 {
		t.Errorf("slow statements %v -> %v", m2["heimdb_slow_statements_total"], m3["heimdb_slow_statements_total"])
	}
}

// 文の先頭の語で種類を決める。
func TestStmtType(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT 1": "select", " /* x */ select 1": "select", "(SELECT 1) UNION (SELECT 2)": "select", "WITH a AS (SELECT 1) SELECT * FROM a": "select",
		"INSERT INTO t VALUES (1)": "insert", "update t set a=1": "update", "DELETE FROM t": "delete", "REPLACE INTO t VALUES (1)": "replace",
		"CALL heimdb_status()": "call", "CREATE TABLE t (a INT)": "ddl", "ALTER TABLE t ADD b INT": "ddl", "DROP TABLE t": "ddl",
		"TRUNCATE t": "ddl", "RENAME TABLE a TO b": "ddl", "START TRANSACTION": "transaction", "COMMIT": "transaction", "ROLLBACK": "transaction",
		"SET NAMES utf8mb4": "set", "SHOW TABLES": "show", "DESCRIBE t": "show", "EXPLAIN SELECT 1": "show", "OPTIMIZE TABLE t": "other", "": "other",
	} {
		if got := stmtTypes[stmtType(q)]; got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
}

// textParser は、Prometheus の文字の形を読む。名前は旧来の規則（英数字と _ と :）で確かめる。
func textParser() *expfmt.TextParser {
	p := expfmt.NewTextParser(model.LegacyValidation)
	return &p
}
