package main

import (
	"context"
	"database/sql"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// -site-password で、WordPress がつなぐ、本番のデータベースだけの権限のユーザーを作る（docs/plan.md の段階 32）。
// すでにいれば変えない。
func TestSiteUser(t *testing.T) {
	bin := heimdbBinary(t)
	dir := t.TempDir()
	srv := startArgs(t, bin, "-data", dir, "-root-password", "pw", "-site-password", "s'p\\w")
	open := func(user, pass, db string) *sql.DB {
		t.Helper()
		d, err := sql.Open("mysql", user+":"+pass+"@tcp("+srv.addr+")/"+db)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	root := open("root", "pw", "wordpress")
	defer root.Close()
	mustExec(t, root, "CREATE TABLE s (id INT PRIMARY KEY)")
	mustExec(t, root, "CALL heimdb_branch_create('staging')")

	site := open("wordpress", "s'p\\w", "wordpress")
	defer site.Close()
	// WordPress がすること: テーブルを作り、読み書きし、列を足す。
	for _, q := range []string{
		"CREATE TABLE t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, v TEXT)",
		"INSERT INTO t (v) VALUES ('a')",
		"UPDATE t SET v = 'b'",
		"ALTER TABLE t ADD COLUMN w INT NOT NULL DEFAULT 0",
		"SELECT COUNT(*) FROM t",
		"DELETE FROM t",
	} {
		if _, err := site.Exec(q); err != nil {
			t.Errorf("site user: %s: %v", q, err)
		}
	}
	// 管理者の操作と、ほかのブランチは拒否する。
	for _, q := range []string{
		"CALL heimdb_branch_list()",
		"CALL heimdb_restore('main', '2026-01-01 00:00:00')",
		"CREATE USER 'x'@'%' IDENTIFIED BY 'x'",
		"GRANT ALL ON *.* TO 'wordpress'@'%'",
		"SELECT * FROM `wordpress@staging`.s",
	} {
		if _, err := site.Exec(q); err == nil {
			t.Errorf("site user: %s: allowed", q)
		}
	}
	other := open("wordpress", "s'p\\w", "wordpress@staging")
	defer other.Close()
	if err := other.Ping(); err == nil {
		t.Error("site user: connected to wordpress@staging")
	}
	var grants []string
	rows, err := root.Query("SHOW GRANTS FOR 'wordpress'@'%'")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var g string
		rows.Scan(&g)
		grants = append(grants, g)
	}
	rows.Close()
	// *.* には USAGE（権限なし）だけ。本番のデータベースには ALL を展開した一覧が出る。
	if len(grants) != 2 || grants[0] != "GRANT USAGE ON *.* TO `wordpress`@`%`" ||
		!strings.Contains(grants[1], "SELECT, INSERT, UPDATE, DELETE, CREATE, DROP") || !strings.Contains(grants[1], " ON `wordpress`.* TO `wordpress`@`%`") {
		t.Errorf("grants:\n%s", strings.Join(grants, "\n"))
	}
	if !strings.Contains(srv.log.String(), "created the site user wordpress@'%' with privileges on wordpress.* only") {
		t.Errorf("no log about the site user:\n%s", srv.log)
	}
	site.Close()
	other.Close()
	root.Close()
	stopProc(srv)

	// すでにいれば、違うパスワードを渡しても変えない。
	srv = startArgs(t, bin, "-data", dir, "-site-password", "other")
	if err := open("wordpress", "s'p\\w", "wordpress").Ping(); err != nil {
		t.Errorf("the original password stopped working: %v", err)
	}
	if err := open("wordpress", "other", "wordpress").Ping(); err == nil {
		t.Error("the site password was changed")
	}
	stopProc(srv)

	// パスワードがなければ作らない。名前の誤りは起動のエラー。
	srv = startArgs(t, bin, "-data", t.TempDir(), "-root-password", "pw")
	if err := open("wordpress", "", "wordpress").Ping(); err == nil {
		t.Error("a site user exists without -site-password")
	}
	stopProc(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-addr", freeAddr(t), "-data", t.TempDir(), "-root-password", "pw",
		"-site-user", "a b", "-site-password", "x").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "invalid site user name") {
		t.Errorf("invalid name: err=%v\n%s", err, out)
	}
}
