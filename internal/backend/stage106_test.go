package backend

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/metrics"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// warmProbe は、表 w（100 行）を作り、その統計を作るロックを先に取っておく。WarmStats は、unlock を呼ぶまで w で止まる。
func warmProbe(t *testing.T, s *server) (unlock func()) {
	t.Helper()
	s.mustQuery(t, "CREATE TABLE w (id bigint NOT NULL AUTO_INCREMENT, k int, PRIMARY KEY (id), KEY k (k))")
	s.mustQuery(t, "INSERT INTO w (k) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) SELECT i % 7 FROM n")
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, _, err := db.GetTableInsensitive(s.ctx, "w")
	if err != nil {
		t.Fatal(err)
	}
	table := tbl.(*Table)
	key := rowCountKey{s.st, table.db.branch, table.def.ID}
	m := &sync.Mutex{}
	m.Lock()
	rowCounts.mu.Lock()
	rowCounts.building[key] = m
	rowCounts.mu.Unlock()
	return m.Unlock
}

// warmStatus は、heimdb_status の stats の行を返す（NULL は "NULL"）。
func warmStatus(t *testing.T, s *server, p *Provider) map[string]string {
	t.Helper()
	iter, err := p.status(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := sql.RowIterToRows(s.ctx, iter)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		name := r[0].(string)
		switch name {
		case "stats", "stats_ready_at", "stats_build_duration", "stats_tables", "stats_error":
			if r[1] == nil {
				out[name] = "NULL"
			} else {
				out[name] = r[1].(string)
			}
		}
	}
	return out
}

func warmMetrics(t *testing.T, p *Provider) map[string]float64 {
	t.Helper()
	srv := httptest.NewServer(metrics.Handler(p.WriteMetrics))
	defer srv.Close()
	m, _ := scrape(t, srv.URL)
	return m
}

func waitWarmState(t *testing.T, p *Provider, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if state, _, _, _, _, _ := p.warm.snapshot(); state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statistics did not become %s", want)
		}
	}
}

// 起動した後に裏で統計を作る間は building、作り終えたら ready を、heimdb_status と指標に出す（docs/plan.md の段階 106）。
func TestWarmStatsReady(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	unlock := warmProbe(t, s)
	pro := NewProvider(s.st, testDB)
	if got := warmStatus(t, s, pro)["stats"]; got != "off" {
		t.Errorf("stats before warming = %s, want off", got)
	}
	pro.WarmStats()
	got := warmStatus(t, s, pro)
	if got["stats"] != "building" || got["stats_ready_at"] != "NULL" || got["stats_tables"] != "NULL" {
		t.Errorf("while building: %v", got)
	}
	m := warmMetrics(t, pro)
	if v, ok := m["heimdb_stats_ready"]; !ok || v != 0 {
		t.Errorf("heimdb_stats_ready while building = %v (present %v), want 0", v, ok)
	}
	if _, ok := m["heimdb_stats_build_seconds"]; ok {
		t.Error("heimdb_stats_build_seconds is shown while building")
	}
	unlock()
	waitWarmState(t, pro, "ready")
	got = warmStatus(t, s, pro)
	if got["stats_tables"] == "NULL" || got["stats_ready_at"] == "NULL" || got["stats_build_duration"] == "NULL" || got["stats_error"] != "NULL" {
		t.Errorf("after building: %v", got)
	}
	if got["stats_tables"][len(got["stats_tables"])-len(", 0 failed"):] != ", 0 failed" {
		t.Errorf("stats_tables = %s", got["stats_tables"])
	}
	m = warmMetrics(t, pro)
	if m["heimdb_stats_ready"] != 1 {
		t.Errorf("heimdb_stats_ready = %v, want 1", m["heimdb_stats_ready"])
	}
	if _, ok := m["heimdb_stats_build_seconds"]; !ok {
		t.Error("heimdb_stats_build_seconds is missing")
	}
	// 後の書き込みで作り直すときは戻さない。
	s.mustQuery(t, "INSERT INTO w (k) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 500) SELECT i FROM n")
	s.mustQuery(t, "SELECT id FROM w WHERE k = 3")
	if got := warmStatus(t, s, pro)["stats"]; got != "ready" {
		t.Errorf("stats after rebuilding = %s, want ready", got)
	}
	if !pro.Shutdown(time.Second) {
		t.Error("shutdown waited")
	}
}

// 停止で途中でやめたら ready にしない。
func TestWarmStatsCancelled(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	unlock := warmProbe(t, s)
	pro := NewProvider(s.st, testDB)
	pro.WarmStats()
	// 裏の作成が始まる（表の一覧か w を読む間は実行中のコマンドとして数える）まで待つ。どちらで止めても、ready にはならない。
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		pro.gate.mu.Lock()
		active := pro.gate.active
		pro.gate.mu.Unlock()
		if active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warming did not reach the table")
		}
	}
	done := make(chan bool)
	go func() { done <- pro.Shutdown(10 * time.Second) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		pro.gate.mu.Lock()
		closed := pro.gate.closed
		pro.gate.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not start")
		}
	}
	unlock()
	if !<-done {
		t.Fatal("shutdown waited for warming")
	}
	time.Sleep(50 * time.Millisecond)
	if got := warmStatus(t, s, pro); got["stats"] != "building" || got["stats_tables"] != "NULL" {
		t.Errorf("after a cancelled warm-up: %v", got)
	}
	if m := warmMetrics(t, pro); m["heimdb_stats_ready"] != 0 {
		t.Errorf("heimdb_stats_ready = %v, want 0", m["heimdb_stats_ready"])
	}
}
