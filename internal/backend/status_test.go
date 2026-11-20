package backend

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// statusMap は、CALL heimdb_status() の結果を項目名から値へのマップにする（NULL は "NULL"）。
func statusMap(t *testing.T, c *sql.Conn) map[string]string {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), "CALL heimdb_status()")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name string
		var value *string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatal(err)
		}
		out[name] = "NULL"
		if value != nil {
			out[name] = *value
		}
	}
	return out
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%q is not a number", s)
	}
	return n
}

// heimdb_status の値が実際の状態と合う（docs/plan.md の段階 19）。
func TestStatus(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	pro.Retention = time.Hour
	db := openRoot(t, addr)
	defer db.Close()
	ctx := context.Background()
	admin, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	c := admin

	s := statusMap(t, c)
	for _, k := range []string{"disk_bytes", "branches", "connections", "running_statements", "open_transactions", "gc_retention", "gc_boundary", "gc_last_run_at", "gc_last_duration", "gc_last_removed", "gc_last_error"} {
		if _, ok := s[k]; !ok {
			t.Errorf("status lacks %s: %v", k, s)
		}
	}
	if s["branches"] != "1" || s["running_statements"] != "1" || s["gc_retention"] != "1h0m0s" || s["gc_last_run_at"] != "NULL" || s["gc_last_removed"] != "NULL" || s["open_transactions"] != "0" {
		t.Errorf("initial status: %v", s)
	}
	if atoi(t, s["disk_bytes"]) <= 0 {
		t.Errorf("disk_bytes = %s", s["disk_bytes"])
	}

	// 開いたままのトランザクションと接続が見える。
	other := openRoot(t, addr)
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT COUNT(*) FROM information_schema.tables"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	s = statusMap(t, c)
	if s["open_transactions"] != "1" || atoi(t, s["oldest_snapshot_age_seconds"]) < 1 || atoi(t, s["connections"]) < 2 {
		t.Errorf("with an open transaction: %v", s)
	}
	tx.Rollback()

	// GC の記録。
	if _, err := admin.ExecContext(ctx, "CALL heimdb_gc()"); err != nil {
		t.Fatal(err)
	}
	s = statusMap(t, c)
	if s["gc_last_run_at"] == "NULL" || s["gc_last_error"] != "NULL" || !strings.Contains(s["gc_last_removed"], "versions") || s["open_transactions"] != "0" {
		t.Errorf("after GC: %v", s)
	}

	// ブランチの一覧に大きさが出る。
	if _, err := admin.ExecContext(ctx, "CALL heimdb_branch_create('staging')"); err != nil {
		t.Fatal(err)
	}
	rows, err := admin.QueryContext(ctx, "CALL heimdb_branch_list()")
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	if strings.Join(cols, ",") != "branch,parent,forked_at,database,size_bytes,max_size_bytes,max_execution_ms" {
		t.Errorf("branch list columns: %v", cols)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != 2 {
		t.Errorf("branch list rows = %d", n)
	}
}

// -slow-query を超えた文だけがログに出る。
func TestSlowQueryLog(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	if err := SetSlowQuery(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	defer SetSlowQuery(0)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	db := openRoot(t, addr)
	defer db.Close()
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("SELECT SLEEP(0.4) AS slow_one"); err != nil {
		t.Fatal(err)
	}
	var slow []*logrus.Entry
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "slow query:") {
			slow = append(slow, e)
		}
	}
	if len(slow) != 1 || !strings.Contains(slow[0].Message, "slow_one") || slow[0].Data["user"] != "root" || slow[0].Data["database"] != testDB {
		for _, e := range slow {
			t.Logf("%s %v", e.Message, e.Data)
		}
		t.Fatalf("slow query log entries: %d", len(slow))
	}
	if d, err := time.ParseDuration(slow[0].Data["duration"].(string)); err != nil || d < 400*time.Millisecond {
		t.Errorf("duration = %v", slow[0].Data["duration"])
	}
}
