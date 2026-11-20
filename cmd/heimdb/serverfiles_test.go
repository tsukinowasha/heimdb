package main

import (
	"strings"
	"testing"
)

// サーバのファイルを読み書きする文は構文解析の直後に拒否し、二重の守りとして secure_file_priv をディレクトリに
// なりえないパスにする（docs/plan.md の段階 31）。backend のテストのサーバは main を通らないので、ここで確かめる。
func TestSecureFilePriv(t *testing.T) {
	srv := startArgs(t, heimdbBinary(t), "-data", t.TempDir(), "-root-password", "pw")
	db := srv.db(t)
	defer db.Close()

	var v string
	if err := db.QueryRow("SELECT @@secure_file_priv").Scan(&v); err != nil || v != "/dev/null/heimdb" {
		t.Errorf("@@secure_file_priv = %q, %v; want /dev/null/heimdb", v, err)
	}
	if _, err := db.Exec("SELECT LOAD_FILE('/etc/hosts')"); err == nil || !strings.Contains(err.Error(), "server files") {
		t.Errorf("LOAD_FILE: %v", err)
	}
	stopProc(srv)
	if strings.Contains(srv.log.String(), "secure_file_priv is set to") {
		t.Errorf("startup warns about secure_file_priv:\n%s", srv.log)
	}
}
