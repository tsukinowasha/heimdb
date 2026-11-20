package main

import (
	"context"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 同期のレプリカ（docs/plan.md の段階 25）。同期の間に書き込みを続けて本番を強制終了すると、OK を受け取った
// コミットがすべてレプリカにある。
func TestSyncReplicaSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	replAddr := freeAddr(t)
	backups := t.TempDir()
	primary := startArgs(t, bin, "-data", t.TempDir(), "-backup-dir", backups, "-root-password", "pw",
		"-replication-addr", replAddr, "-replication-token", replToken, "-replication-sync-timeout", "5s")
	defer stopProc(primary)
	pdb := primary.db(t)
	defer pdb.Close()
	mustExec(t, pdb, "CREATE TABLE acks (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, w int NOT NULL, v longtext)")
	var backupDir, at string
	var size int64
	if err := pdb.QueryRow("CALL heimdb_backup('replica')").Scan(&backupDir, &at, &size); err != nil {
		t.Fatal(err)
	}
	replica := startArgs(t, bin, "-data", backupDir, "-replicate-from", replAddr, "-replication-token", replToken,
		"-replica-name", "r1")
	defer stopProc(replica)
	// 途中で落ちても、一時停止したレプリカを残さない
	t.Cleanup(func() { replica.cmd.Process.Signal(syscall.SIGCONT) })
	deadline := time.Now().Add(10 * time.Second)
	for statusValue(t, pdb, "replication_sync") != "sync" {
		if time.Now().After(deadline) {
			t.Fatalf("the primary did not become sync:\n%s", queryAll(t, pdb, "CALL heimdb_status()"))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 書き込み手は OK を受け取った ID を覚え、エラー（本番が落ちた）で止まる。
	var mu sync.Mutex
	var acked []int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for ctx.Err() == nil {
				res, err := pdb.ExecContext(ctx, "INSERT INTO acks (w, v) VALUES (?, REPEAT('x', 200))", w)
				if err != nil {
					return
				}
				id, _ := res.LastInsertId()
				mu.Lock()
				acked = append(acked, id)
				mu.Unlock()
			}
		}(w)
	}
	time.Sleep(2 * time.Second)
	// 同期のまま（非同期に切り替わっていない）ことを確かめる
	sdb := primary.db(t)
	if m, n := statusValue(t, sdb, "replication_sync"), statusValue(t, sdb, "replication_sync_async_switches"); m != "sync" || n != "0" {
		t.Fatalf("the primary was not sync before the kill: %s, %s switches", m, n)
	}
	sdb.Close()
	// レプリカを一時停止して、レプリカに届かないコミットを作る（同期なら OK は返らない）。待ち時間（5 秒）より前に
	// 本番を強制終了し、レプリカを再開する。
	mu.Lock()
	beforePause := len(acked)
	mu.Unlock()
	replica.cmd.Process.Signal(syscall.SIGSTOP)
	time.Sleep(time.Second)
	primary.cmd.Process.Kill()
	replica.cmd.Process.Signal(syscall.SIGCONT)
	wg.Wait()
	cancel()

	mu.Lock()
	ids := append([]int64(nil), acked...)
	mu.Unlock()
	if len(ids) < 50 {
		t.Fatalf("only %d commits", len(ids))
	}
	rdb := replica.db(t)
	defer rdb.Close()
	have := map[int64]bool{}
	rows, err := rdb.Query("SELECT id FROM acks")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		have[id] = true
	}
	rows.Close()
	missing := 0
	for _, id := range ids {
		if !have[id] {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d of %d acknowledged commits are not on the replica", missing, len(ids))
	}
	// レプリカにあって OK を受け取っていない行は、書き込み手ごとに高々 1 つ（知らせを待っていたコミットと、OK が
	// 届く前に本番が落ちたコミット）
	if extra := len(have) - (len(ids) - missing); extra > 4 {
		t.Errorf("%d rows on the replica were not acknowledged", extra)
	}
	if m := statusValue(t, rdb, "replication_primary_sync"); m != "sync" {
		t.Errorf("replica sees the primary as %q", m)
	}
	t.Logf("%d acknowledged commits (%d before pausing the replica), %d rows on the replica", len(ids), beforePause, len(have))
}

// レプリカを止めても、本番は待ち時間の後に書ける。止めるときは、最後のコミットがレプリカに届いたかをログに出す。
func TestSyncReplicaStopsAndShutdownLog(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	replAddr := freeAddr(t)
	backups := t.TempDir()
	primary := startArgs(t, bin, "-data", t.TempDir(), "-backup-dir", backups, "-root-password", "pw",
		"-replication-addr", replAddr, "-replication-token", replToken, "-replication-sync-timeout", "1s")
	defer stopProc(primary)
	pdb := primary.db(t)
	defer pdb.Close()
	mustExec(t, pdb, "CREATE TABLE t (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v int)")
	var backupDir, at string
	var size int64
	if err := pdb.QueryRow("CALL heimdb_backup('replica')").Scan(&backupDir, &at, &size); err != nil {
		t.Fatal(err)
	}
	replArgs := []string{"-data", backupDir, "-replicate-from", replAddr, "-replication-token", replToken, "-replica-name", "r1"}
	replica := startArgs(t, bin, replArgs...)
	waitStatus := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for statusValue(t, pdb, "replication_sync") != want {
			if time.Now().After(deadline) {
				t.Fatalf("the primary did not become %s:\n%s", want, queryAll(t, pdb, "CALL heimdb_status()"))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitStatus("sync")
	stopProc(replica)
	start := time.Now()
	mustExec(t, pdb, "INSERT INTO t (v) VALUES (1)")
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Errorf("the write without a replica took %s", d)
	}
	waitStatus("async")
	mustExec(t, pdb, "INSERT INTO t (v) VALUES (2)")

	replica = startArgs(t, bin, replArgs...)
	defer stopProc(replica)
	waitStatus("sync")
	mustExec(t, pdb, "INSERT INTO t (v) VALUES (3)")
	rdb := replica.db(t)
	defer rdb.Close()
	if n := queryAll(t, rdb, "SELECT COUNT(*) FROM t"); strings.TrimSpace(n) != "3" {
		t.Errorf("rows on the replica right after a sync commit: %s", n)
	}

	stopProc(primary)
	if log := primary.log.String(); !strings.Contains(log, "was acknowledged by replica r1 (sync)") ||
		!strings.Contains(log, "switched to async") || !strings.Contains(log, "switched to sync") {
		t.Errorf("primary log:\n%s", log)
	}
}
