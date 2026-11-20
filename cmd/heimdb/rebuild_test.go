package main

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 段階 121: バージョン 6 のデータ（0.2.38 の DECIMAL などのインデックス）を、HEIMDB_REBUILD_INDEXES=false の本番で開き、
// そのバックアップからレプリカを作る。本番を既定（-rebuild-indexes は true）で起動し直すと、main の古いインデックスを
// 裏で作り直し、そのエントリが記録でレプリカに届いて、レプリカも範囲で引く。
func TestRebuildIndexesReachReplica(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	extractTarGz(t, "../../internal/testdata/format6_data.tar.gz", dir)
	replAddr := freeAddr(t)
	backups := t.TempDir()
	primaryArgs := []string{"-data", dir, "-backup-dir", backups, "-root-password", "pw",
		"-replication-addr", replAddr, "-replication-token", replToken, "-replication-sync-timeout", "0", "-shutdown-timeout", "10s"}
	primary := startArgsEnv(t, bin, []string{"HEIMDB_REBUILD_INDEXES=false"}, primaryArgs...)
	if log := primary.log.String(); !strings.Contains(log, "to build them, run CALL heimdb_rebuild_indexes() on the primary while connected to each of these databases") ||
		strings.Contains(log, "rebuilding") {
		t.Errorf("primary with HEIMDB_REBUILD_INDEXES=false:\n%s", log)
	}
	pdb := primary.db(t)
	if got := statusValue(t, pdb, "index_rebuild"); got != "off" {
		t.Errorf("index_rebuild with HEIMDB_REBUILD_INDEXES=false: %s", got)
	}
	var backupDir, backedUpAt string
	var size int64
	if err := pdb.QueryRow("CALL heimdb_backup('replica')").Scan(&backupDir, &backedUpAt, &size); err != nil {
		t.Fatal(err)
	}
	pdb.Close()
	replica := startArgs(t, bin, "-data", backupDir, "-replicate-from", replAddr, "-replication-token", replToken,
		"-replica-name", "r1", "-shutdown-timeout", "10s")
	defer stopProc(replica)
	if log := replica.log.String(); !strings.Contains(log, "the primary rebuilds wordpress's in the background after it starts (unless -rebuild-indexes=false), and the entries reach this replica through its commit records; to build the others,") {
		t.Errorf("replica startup:\n%s", log)
	}
	stopProc(primary)

	primary = startArgs(t, bin, primaryArgs...)
	defer stopProc(primary)
	pdb = primary.db(t)
	defer pdb.Close()
	deadline := time.Now().Add(30 * time.Second)
	for statusValue(t, pdb, "index_rebuild") != "done" {
		if time.Now().After(deadline) {
			t.Fatalf("the rebuild did not finish:\n%s", primary.log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := statusValue(t, pdb, "index_rebuild_indexes"); got != "6 rebuilt, 0 skipped" {
		t.Errorf("index_rebuild_indexes %s\n%s", got, primary.log)
	}
	for _, want := range []string{"wordpress's are rebuilt in the background now; to build the others,", "HeimDB: rebuilt 6 indexes on wordpress in "} {
		if !strings.Contains(primary.log.String(), want) {
			t.Errorf("primary log has no %q:\n%s", want, primary.log)
		}
	}
	rdb := replica.db(t)
	defer rdb.Close()
	waitCaughtUp(t, pdb, rdb, replica)
	if got := statusValue(t, rdb, "index_rebuild"); got != "off" {
		t.Errorf("index_rebuild on the replica: %s", got)
	}
	const q = "SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.995 AND max_price <= 12.5"
	for name, db := range map[string]*sql.DB{"primary": pdb, "replica": rdb} {
		if got := queryAll(t, db, q+" ORDER BY product_id"); got != "1\n2\n6" {
			t.Errorf("%s: %q", name, got)
		}
		if plan := queryAll(t, db, "EXPLAIN PLAN "+q); !strings.Contains(plan, "filters: [{[9.995, ∞), [NULL, ∞)}]") {
			t.Errorf("%s plan:\n%s", name, plan)
		}
	}
}

// extractTarGz は、データディレクトリの中身の tar.gz の path を dir に展開する。
func extractTarGz(t *testing.T, path, dir string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.Create(filepath.Join(dir, filepath.Base(h.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
		out.Close()
	}
}
