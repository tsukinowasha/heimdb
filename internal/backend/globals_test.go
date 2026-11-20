package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// WordPress が使わない全体の操作を塞ぐ（docs/plan.md の段階 35）。サイト用のユーザーは SET GLOBAL ができず（エラーは
// MySQL と同じ）、processlist では自分の接続しか見えない。
func TestGlobalOperationsNeedPrivileges(t *testing.T) {
	addr, _, st, srv := startReview19Server(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	root := openRoot(t, addr)
	defer root.Close()
	for _, q := range []string{
		"CREATE USER 'wordpress'@'%' IDENTIFIED BY 'pw'",
		"GRANT ALL ON wordpress.* TO 'wordpress'@'%'",
	} {
		if _, err := root.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	site, err := sql.Open("mysql", "wordpress:pw@tcp("+addr+")/wordpress")
	if err != nil {
		t.Fatal(err)
	}
	defer site.Close()
	site.SetMaxOpenConns(1)
	ctx := context.Background()
	c, err := site.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	globals := func() string {
		t.Helper()
		var a, l, z, w string
		if err := root.QueryRow("SELECT @@GLOBAL.autocommit, @@GLOBAL.sql_select_limit, @@GLOBAL.time_zone, @@GLOBAL.wait_timeout").Scan(&a, &l, &z, &w); err != nil {
			t.Fatal(err)
		}
		return strings.Join([]string{a, l, z, w}, " ")
	}
	before := globals()
	const denied = "Error 1227 (42000): Access denied; you need (at least one of) the SUPER or SYSTEM_VARIABLES_ADMIN privilege(s) for this operation"
	for q, want := range map[string]string{
		"SET GLOBAL autocommit = 0":                          denied,
		"SET @@GLOBAL.sql_select_limit = 1":                  denied,
		"SET @@global.time_zone = '+09:00'":                  denied,
		"SET PERSIST wait_timeout = 5":                       denied,
		"SET @@PERSIST.wait_timeout = 5":                     denied,
		"SET SESSION sql_mode = '', GLOBAL wait_timeout = 5": denied,
		"SET PERSIST_ONLY wait_timeout = 5":                  "Error 3630 (42000): Access denied; you need SYSTEM_VARIABLES_ADMIN and PERSIST_RO_VARIABLES_ADMIN privileges for this operation",
		// ない名前と読み取り専用の値は、権限より先にそのエラー（MySQL と同じ）。
		"SET GLOBAL nosuchvar = 1":             "Unknown system variable 'nosuchvar'",
		"SET GLOBAL secure_file_priv = '/tmp'": "is a read only variable",
	} {
		_, err := c.ExecContext(ctx, q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
	// バイナリプロトコル（引数付きのプリペア）でも止まる。
	if _, err := c.ExecContext(ctx, "SET GLOBAL wait_timeout = ?", 5); err == nil || !strings.Contains(err.Error(), "1227") {
		t.Errorf("binary protocol SET GLOBAL: %v", err)
	}
	// PREPARE は通り、実行で止まる（MySQL と同じ）。
	if _, err := c.ExecContext(ctx, "PREPARE p FROM 'SET GLOBAL autocommit = 0'"); err != nil {
		t.Errorf("prepare: %v", err)
	}
	if _, err := c.ExecContext(ctx, "EXECUTE p"); err == nil || !strings.Contains(err.Error(), "1227") {
		t.Errorf("execute: %v", err)
	}
	if got := globals(); got != before {
		t.Errorf("global values changed by the site user: %s -> %s", before, got)
	}
	// 接続ごとの設定（WordPress の接続の初めの文）は使え、接続は切れていない。
	var mode string
	for _, q := range []string{"SET NAMES utf8mb4", "SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION'", "SET time_zone = '+00:00'", "SET @@SESSION.sql_select_limit = DEFAULT"} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	if err := c.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err != nil || mode != "NO_ENGINE_SUBSTITUTION" {
		t.Errorf("session sql_mode = %q %v", mode, err)
	}
	// root は変えられる。
	if _, err := root.Exec("SET GLOBAL wait_timeout = 100"); err != nil {
		t.Errorf("root: %v", err)
	}
	if !strings.HasSuffix(globals(), " 100") {
		t.Errorf("root could not change wait_timeout: %s", globals())
	}
	if _, err := root.Exec("SET GLOBAL wait_timeout = DEFAULT"); err != nil {
		t.Fatal(err)
	}

	// processlist: 実行中の root の文は、サイト用のユーザーには見えず、root には見える。
	done := make(chan error, 1)
	go func() {
		other, err := sql.Open("mysql", "root@tcp("+addr+")/")
		if err != nil {
			done <- err
			return
		}
		defer other.Close()
		_, err = other.Exec("SELECT SLEEP(1.5), 'root-secret'")
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	list := func(db interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT USER, COALESCE(INFO, '') FROM information_schema.processlist ORDER BY ID")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var u, info string
			if err := rows.Scan(&u, &info); err != nil {
				t.Fatal(err)
			}
			out = append(out, u+": "+info)
		}
		return out
	}
	siteRows, rootRows := strings.Join(list(c), "\n"), strings.Join(list(root), "\n")
	if strings.Contains(siteRows, "root-secret") || !strings.Contains(siteRows, "wordpress: SELECT USER") {
		t.Errorf("site user sees:\n%s", siteRows)
	}
	// データベースを選ばずにつないだ接続も、ユーザーの名前で出る（unauthenticated user ではない）。
	if !strings.Contains(rootRows, "root: SELECT SLEEP(1.5), 'root-secret'") || !strings.Contains(rootRows, "\nwordpress: ") {
		t.Errorf("root sees:\n%s", rootRows)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
