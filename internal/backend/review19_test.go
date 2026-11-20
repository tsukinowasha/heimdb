package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"

	gmsserver "github.com/tsukinowasha/heimdb/internal/sqlengine/server"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 19 のレビュー。

// slowEntries は、フックに溜まった遅い文のログのメッセージを返す。
func slowEntries(hook *logtest.Hook) []string {
	var out []string
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "slow query:") {
			out = append(out, e.Message)
		}
	}
	return out
}

// startReview19Server は startShutdownServer と同じだが、ユーザーの変更を保存できる（EnableAuth と同じ persister）。
func startReview19Server(t *testing.T, dir string) (addr string, pro *Provider, st *store.Store, srv *gmsserver.Server) {
	t.Helper()
	return serve(t, dir, serverConfig{persist: true})
}

// -no-auth（EnableAuth を呼ばない。startShutdownServer と同じ構成）で CREATE USER / ALTER USER / GRANT を実行すると、
// go-mysql-server が persister の nil を参照して panic し、接続が切れていた（mysql_db.go の Persist）。今は明示的な
// エラーになり、接続は切れない。段階 14 からある問題だが、段階 19 のテストで見つかった。
func TestReview19CreateUserWithoutAuthDoesNotDropConnection(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	db := openRoot(t, addr)
	defer db.Close()
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'",
		"GRANT ALL ON wordpress.* TO 'wp'@'%'",
	} {
		if _, err := c.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "-no-auth") {
			t.Errorf("%s: err = %v, want an error about -no-auth", q, err)
		}
		if err := c.PingContext(ctx); err != nil {
			t.Fatalf("connection dropped after %s: %v", q, err)
		}
	}
}

// 遅い文のログに、CREATE USER / ALTER USER / SET PASSWORD のパスワードがそのまま出てはいけない（MySQL のスロー
// ログはパスワードを書き換える）。
func TestReview19SlowQueryLogHidesPasswords(t *testing.T) {
	addr, _, st, srv := startReview19Server(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	if err := SetSlowQuery(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	defer SetSlowQuery(0)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	db := openRoot(t, addr)
	defer db.Close()
	for _, q := range []string{
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'hunter2secret'",
		"ALTER USER 'wp'@'%' IDENTIFIED BY 'hunter3secret'",
		"SET PASSWORD FOR 'wp'@'%' = 'hunter4secret'",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Logf("%s: %v", q, err) // 対応していない文でも、文自体はログに出る
		}
	}
	entries := slowEntries(hook)
	if len(entries) == 0 {
		t.Fatal("no slow query log entries")
	}
	for _, m := range entries {
		if strings.Contains(m, "secret") {
			t.Errorf("slow query log leaks a password: %s", m)
		}
	}
}

// プリペアドステートメント（バイナリプロトコル）の遅い文もログに出る。
func TestReview19SlowQueryLogPreparedStatement(t *testing.T) {
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
	// 引数があると go-sql-driver は COM_STMT_PREPARE / COM_STMT_EXECUTE を使う。
	rows, err := db.Query("SELECT SLEEP(?) AS slow_prep", 0.4)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	entries := slowEntries(hook)
	if len(entries) != 1 || !strings.Contains(entries[0], "slow_prep") {
		t.Errorf("slow query log entries: %q", entries)
	}
}

// connections は、認証に失敗した接続、KILL された接続、プリペアドステートメントやトランザクションを開いたまま
// 切った接続の後でも、開いている接続の数に戻る。
func TestReview19ConnectionsStayBalanced(t *testing.T) {
	addr, stop := startAuthServer(t, t.TempDir(), "rootpw")
	defer stop()
	ctx := context.Background()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.SetMaxOpenConns(1)
	admin, err := root.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, "CREATE TABLE t (id int NOT NULL PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	// 切断はサーバ側で非同期に片付くので、少し待つ。
	waitConnections := func(want string, what string) {
		t.Helper()
		var got string
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if got = statusMap(t, admin)["connections"]; got == want {
				return
			}
		}
		t.Errorf("%s: connections = %s, want %s", what, got, want)
	}
	waitConnections("1", "initial")

	// 認証に失敗した接続。
	for _, c := range []struct{ user, password string }{{"root", "wrong"}, {"nobody", ""}} {
		if db, err := connectAs(t, addr, c.user, c.password, testDB); err == nil {
			db.Close()
			t.Fatalf("%s:%q connected", c.user, c.password)
		}
	}
	waitConnections("1", "after failed logins")

	// プリペアドステートメントとトランザクションを開いたままの接続。
	other, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	other.SetMaxOpenConns(3)
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		c, err := other.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		if _, err := c.ExecContext(ctx, "INSERT INTO t VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	// サーバ側だけが知る開いたままのトランザクション（database/sql の Tx は Close を待ってしまうので、文で始める）。
	for _, q := range []string{"START TRANSACTION", "UPDATE t SET id = 10 WHERE id = 0"} {
		if _, err := conns[0].ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	waitConnections("4", "with three more connections")
	for _, c := range conns {
		c.Close() // トランザクションを開いたまま
	}
	other.Close()
	waitConnections("1", "after closing them")

	// KILL された接続。
	victim, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer victim.Close()
	victim.SetMaxOpenConns(1)
	id := one(t, victim, "SELECT CONNECTION_ID()")
	waitConnections("2", "with the victim")
	if _, err := admin.ExecContext(ctx, "KILL "+id); err != nil {
		t.Fatal(err)
	}
	waitConnections("1", "after KILL")
}

// 管理者でないユーザーは heimdb_status と heimdb_branch_list（大きさの列）を呼べず、接続は切れない。
func TestReview19StatusDeniedForNonAdmin(t *testing.T) {
	addr, stop := startAuthServer(t, t.TempDir(), "rootpw")
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'",
		"GRANT ALL ON wordpress.* TO 'wp'@'%'",
	} {
		mustExec(t, root, q)
	}
	root.Close()
	wp, err := connectAs(t, addr, "wp", "wppw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer wp.Close()
	for _, q := range []string{"CALL heimdb_status()", "CALL heimdb_branch_list()", "CALL heimdb_branch_create('x')"} {
		if _, err := wp.Exec(q); err == nil || !strings.Contains(err.Error(), "command denied") {
			t.Errorf("wp: %s: err = %v, want command denied", q, err)
		}
	}
	if err := wp.Ping(); err != nil {
		t.Errorf("wp connection dropped: %v", err)
	}
}

// oldest_snapshot_age_seconds は、トランザクションを開いてからの時間ではなく、スナップショット（開いた時点の
// 最後のコミット時刻）の古さ。ストアがしばらく静かだった後に開いたばかりのトランザクションでも、静かだった時間が
// 出る（以前の名前 oldest_snapshot_age_seconds は「開いてからの時間」に見えたので改めた）。
func TestReview19OldestSnapshotAge(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	pro.Retention = time.Hour
	ctx := context.Background()
	db := openRoot(t, addr)
	defer db.Close()
	admin, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, "CREATE TABLE t (id int NOT NULL PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // 最後のコミットから時間が経つ

	other := openRoot(t, addr)
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT COUNT(*) FROM t"); err != nil {
		t.Fatal(err)
	}
	s := statusMap(t, admin)
	if s["open_transactions"] != "1" {
		t.Fatalf("open_transactions = %s: %v", s["open_transactions"], s)
	}
	age := atoi(t, s["oldest_snapshot_age_seconds"])
	t.Logf("a transaction opened just now reports oldest_snapshot_age_seconds = %d", age)
	if age < 1 {
		t.Errorf("oldest_snapshot_age_seconds = %d; expected the idle time (snapshot age), not the transaction age", age)
	}
}
