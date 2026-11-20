package main

import (
	"context"
	"database/sql"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// メモリの上限とブロックキャッシュ（docs/plan.md の段階 58）。-cache-size を指定しなければ、キャッシュは上限の 8 分の
// 1 になり、heimdb_status に出る。起動直後の分とキャッシュに足りない上限では、要る上限を添えて起動しない（ホスティング側の報告
// で、64M の上限では、どの文も 1041 になり続けた）。
func TestMemoryLimitStartup(t *testing.T) {
	bin := heimdbBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-addr", freeAddr(t), "-data", t.TempDir(), "-root-password", "pw", "-memory-limit", "64M").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "the memory limit (-memory-limit) of 64 MiB is too small") || !strings.Contains(string(out), "use a memory limit of at least") {
		t.Errorf("64M: err=%v\n%s", err, out)
	}

	for _, c := range []struct {
		args  []string
		cache string
	}{
		{[]string{"-memory-limit", "256M"}, "33554432"},
		{[]string{"-memory-limit", "256M", "-cache-size", "100000000"}, "100000000"},
		{[]string{"-memory-limit", "0"}, "134217728"},
	} {
		srv := startArgs(t, bin, append([]string{"-data", t.TempDir(), "-root-password", "pw"}, c.args...)...)
		db, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress")
		if err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query("CALL heimdb_status()")
		if err != nil {
			t.Fatal(err)
		}
		st := map[string]string{}
		for rows.Next() {
			var name string
			var value sql.NullString
			if err := rows.Scan(&name, &value); err != nil {
				t.Fatal(err)
			}
			st[name] = value.String
		}
		rows.Close()
		db.Close()
		stopProc(srv)
		if st["cache_size_bytes"] != c.cache {
			t.Errorf("%v: cache_size_bytes = %s, want %s", c.args, st["cache_size_bytes"], c.cache)
		}
		if c.args[1] == "256M" && st["memory_limit_bytes"] != "268435456" {
			t.Errorf("%v: memory_limit_bytes = %s", c.args, st["memory_limit_bytes"])
		}
	}
}
