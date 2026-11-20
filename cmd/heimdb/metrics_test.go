package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// -metrics-addr を指定したときだけ指標の口を開き、レプリケーションの口と同じアドレスは起動しない（docs/plan.md の
// 段階 73）。止めるときは指標の口も閉じる。
func TestMetricsFlag(t *testing.T) {
	bin := heimdbBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	same := freeAddr(t)
	out, err := exec.CommandContext(ctx, bin, "-addr", freeAddr(t), "-data", t.TempDir(), "-no-auth", "-metrics-addr", same,
		"-replication-addr", same, "-replication-token", "tok").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "-metrics-addr must differ from -replication-addr") {
		t.Errorf("same address: err=%v\n%s", err, out)
	}

	plain := startArgs(t, bin, "-data", t.TempDir(), "-no-auth")
	if strings.Contains(plain.log.String(), "metrics") {
		t.Errorf("metrics without -metrics-addr:\n%s", plain.log)
	}
	stopProc(plain)

	maddr := freeAddr(t)
	srv := startArgs(t, bin, "-data", t.TempDir(), "-no-auth", "-metrics-addr", maddr)
	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + maddr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, `heimdb_build_info{version="dev"} 1`) || !strings.Contains(body, "heimdb_branches 1") {
		t.Errorf("/metrics: %d\n%s", code, body)
	}
	if code, _ := get("/"); code != 404 {
		t.Errorf("/: %d", code)
	}
	stopProc(srv)
	if _, err := http.Get("http://" + maddr + "/metrics"); err == nil {
		t.Error("the metrics listener is still open after the shutdown")
	}
}
