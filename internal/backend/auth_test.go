package backend

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// startAuthServer は、認証を有効にしたサーバをデータディレクトリ dir で起動し、アドレスを返す。opts は開いたストアに使う。
func startAuthServer(t *testing.T, dir, rootPassword string, opts ...func(*store.Store)) (addr string, stop func()) {
	t.Helper()
	addr, _, st, srv := serve(t, dir, serverConfig{auth: true, rootPassword: rootPassword, store: opts})
	return addr, stopServer(t, st, srv)
}

// connectAs は、user:password でデータベース db に接続し、接続できたかを確かめる。
func connectAs(t *testing.T, addr, user, password, db string) (*sql.DB, error) {
	t.Helper()
	conn, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s", user, password, addr, db))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// mysqlErrNo は、MySQL のエラー番号（MySQL のエラーでなければ 0）。
func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// dbAccessDenied は、データベースへのアクセスを拒否したエラーか。MySQL はエラー 1044 だが、go-mysql-server は 1105
// （メッセージは同じ）を返す（docs/plan.md の段階 14）。
func dbAccessDenied(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Access denied for user") && strings.Contains(err.Error(), "to database")
}

func TestAuth(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startAuthServer(t, dir, "rootpw")

	for _, c := range []struct{ user, password string }{{"root", ""}, {"root", "wrong"}} {
		if _, err := connectAs(t, addr, c.user, c.password, testDB); mysqlErrNo(err) != 1045 {
			t.Errorf("%s:%q: err = %v, want 1045", c.user, c.password, err)
		}
	}
	// 存在しないユーザーも、MySQL と同じく 1045（前は 2012 で、ユーザーの有無が外から分かった。段階 111、上流の #3451）。
	for _, password := range []string{"rootpw", ""} {
		if _, err := connectAs(t, addr, "nobody", password, testDB); mysqlErrNo(err) != 1045 {
			t.Errorf("unknown user with password %q: err = %v, want 1045", password, err)
		}
	}
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...),
		"INSERT INTO wp_options (option_name, option_value) VALUES ('blogname', 'Prod')",
		"CALL heimdb_branch_create('staging')",
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'",
		"GRANT ALL ON wordpress.* TO 'wp'@'%'",
		"CREATE USER 'staging'@'%' IDENTIFIED BY 'stpw'",
		"GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%'",
	) {
		mustExec(t, root, q)
	}
	root.Close()

	// 本番のユーザーは本番を読み書きでき、テーブルを作って変えられる（dbDelta、一時テーブル）。ブランチには入れない。
	wp, err := connectAs(t, addr, "wp", "wppw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE wp_options SET option_value = 'Prod 2' WHERE option_name = 'blogname'",
		"CREATE TABLE wp_plug (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a int NOT NULL)",
		"ALTER TABLE wp_plug ADD COLUMN b varchar(10) NOT NULL DEFAULT ''",
		"CREATE INDEX ka ON wp_plug (a)",
		"CREATE TEMPORARY TABLE tmp (id int NOT NULL PRIMARY KEY)",
		"DROP TABLE wp_plug",
	} {
		mustExec(t, wp, q)
	}
	if got := one(t, wp, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod 2" {
		t.Errorf("wp: blogname = %q", got)
	}
	// HeimDB の手続きは管理者だけ。
	for _, q := range []string{"CALL heimdb_branch_list()", "CALL heimdb_branch_merge('staging')", "CALL heimdb_gc()"} {
		if _, err := wp.Exec(q); err == nil || !strings.Contains(err.Error(), "command denied") {
			t.Errorf("wp: %s: err = %v, want command denied", q, err)
		}
	}
	wp.Close()
	if _, err := connectAs(t, addr, "wp", "wppw", testDB+"@staging"); !dbAccessDenied(err) {
		t.Errorf("wp on the branch: err = %v, want access denied", err)
	}

	// ステージングのユーザーはブランチだけ。
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, stg, "UPDATE wp_options SET option_value = 'Staging' WHERE option_name = 'blogname'")
	if got := one(t, stg, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Staging" {
		t.Errorf("staging: blogname = %q", got)
	}
	if _, err := stg.Exec("SELECT COUNT(*) FROM wordpress.wp_options"); !dbAccessDenied(err) {
		t.Errorf("staging read the main branch: err = %v", err)
	}
	if _, err := stg.Exec("CALL heimdb_branch_merge('staging')"); err == nil {
		t.Errorf("staging merged itself")
	}
	stg.Close()
	if _, err := connectAs(t, addr, "staging", "stpw", testDB); !dbAccessDenied(err) {
		t.Errorf("staging on main: err = %v, want access denied", err)
	}

	// 再起動してもユーザーと権限が残る。root のパスワードは最初の起動でだけ使う。
	stop()
	addr, stop = startAuthServer(t, dir, "ignored")
	defer stop()
	if _, err := connectAs(t, addr, "root", "ignored", testDB); mysqlErrNo(err) != 1045 {
		t.Errorf("root with the new password: err = %v, want 1045", err)
	}
	root, err = connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if got := one(t, root, "SELECT COUNT(*) FROM mysql.user WHERE user IN ('wp', 'staging')"); got != "2" {
		t.Errorf("users after restart: %s", got)
	}
	stg, err = connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer stg.Close()
	if got := one(t, stg, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Staging" {
		t.Errorf("staging after restart: blogname = %q", got)
	}
	if _, err := connectAs(t, addr, "staging", "stpw", testDB); !dbAccessDenied(err) {
		t.Errorf("staging on main after restart: err = %v, want access denied", err)
	}
}

// ユーザーがまだいなくて、root のパスワードもなければ、認証を有効にできない（起動を拒否する）。
func TestAuthRequiresRootPassword(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := EnableAuth(NewEngine(NewProvider(st, testDB)), st, ""); err == nil {
		t.Fatal("EnableAuth without users or a root password succeeded")
	}
}
