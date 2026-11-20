package main

import (
	"database/sql"
	"strings"
	"testing"
)

// 環境変数 HEIMDB_BRANCH_PROCEDURES は、フラグを書かないときだけ効く（-branch-procedures=false で切れる。段階 63）。
func TestBranchProceduresFlag(t *testing.T) {
	bin := heimdbBinary(t)
	dir := t.TempDir()
	t.Setenv("HEIMDB_BRANCH_PROCEDURES", "1")
	for _, c := range []struct {
		args []string
		want string // 空なら呼べる
	}{
		{nil, ""},
		{[]string{"-branch-procedures=false"}, "command denied"},
		{[]string{"-branch-procedures"}, ""},
	} {
		srv := startArgs(t, bin, append([]string{"-data", dir, "-root-password", "pw"}, c.args...)...)
		root, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress")
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, root, "CREATE TABLE IF NOT EXISTS wp_options (option_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name VARCHAR(191), option_value LONGTEXT)")
		if _, err := root.Exec("CALL heimdb_branch_create('staging')"); err != nil && !strings.Contains(err.Error(), "exists") {
			t.Fatal(err)
		}
		mustExec(t, root, "CREATE USER IF NOT EXISTS 'stg'@'%' IDENTIFIED BY 'pw'")
		mustExec(t, root, "GRANT ALL ON `wordpress@staging`.* TO 'stg'@'%'")
		stg, err := sql.Open("mysql", "stg:pw@tcp("+srv.addr+")/wordpress@staging")
		if err != nil {
			t.Fatal(err)
		}
		_, err = stg.Exec("CALL heimdb_branch_diff('')")
		if (c.want == "" && err != nil) || (c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want))) {
			t.Errorf("%v: %v", c.args, err)
		}
		stg.Close()
		root.Close()
		stopProc(srv)
	}
}
