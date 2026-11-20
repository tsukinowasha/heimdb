package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"

	"github.com/tsukinowasha/heimdb/internal/metrics"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 82: 書き込みのロックを持ったまま止まった接続を見つけて外す。

// lockServer は、ロックを待つ時間 wait、何もしない接続を切る時間 idle（0 なら見張らない）で、本番と同じく接続の
// 一覧と接続を切る口をつないだサーバを起動する。
func lockServer(t *testing.T, wait, idle time.Duration, auth bool) (addr string, pro *Provider, stop func()) {
	t.Helper()
	var engine *sqle.Engine
	addr, pro, st, srv := serve(t, t.TempDir(), serverConfig{
		auth: auth, rootPassword: "rootpw",
		store:  []func(*store.Store){func(s *store.Store) { s.LockTimeout = wait }},
		engine: func(e *sqle.Engine) { engine = e },
	})
	pro.Processes, pro.KillConnection, pro.IdleLockTimeout = engine.ProcessList.Processes, srv.SessionManager().KillConnection, idle
	pro.WatchIdleLocks()
	return addr, pro, func() {
		pro.Shutdown(time.Second)
		srv.Close()
		st.Close()
	}
}

func lockDB(t *testing.T, addr, user, password string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s", user, password, addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// heldTxn は、BEGIN して書き込み、書き込みのロックを持ったまま何もしない接続と、その接続 ID を返す。
func heldTxn(t *testing.T, db *sql.DB, q string) (*sql.Conn, uint32) {
	t.Helper()
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id uint32
	if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"BEGIN", q} {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return c, id
}

// 何もしないまま書き込みのロックを持つ接続は -idle-lock-timeout で切られ、その書き込みはロールバックされ、待っていた
// 書き込みは 1205 にならずに通る。実行中の文がロックを持つ接続は切らない。
func TestIdleLockTimeout(t *testing.T) {
	addr, pro, stop := lockServer(t, 5*time.Second, 500*time.Millisecond, false)
	defer stop()
	hook := logtest.NewGlobal()
	defer hook.Reset()
	a, b := lockDB(t, addr, "root", ""), lockDB(t, addr, "root", "")
	defer a.Close()
	defer b.Close()
	mustExec(t, b, "CREATE TABLE o (id INT PRIMARY KEY, v INT)")
	mustExec(t, b, "INSERT INTO o VALUES (1, 1), (2, 2)")

	held, _ := heldTxn(t, a, "UPDATE o SET v = 10 WHERE id = 1")
	start := time.Now()
	mustExec(t, b, "UPDATE o SET v = 20 WHERE id = 2")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the waiting write took %s", d)
	}
	if got := one(t, b, "SELECT v FROM o WHERE id = 1"); got != "1" {
		t.Errorf("the closed connection's write was kept: v = %s", got)
	}
	if _, err := held.ExecContext(context.Background(), "COMMIT"); err == nil {
		t.Error("the closed connection still works")
	}
	held.Close()
	if n := pro.lockStats.idleKills.Load(); n != 1 {
		t.Errorf("disconnects = %d", n)
	}
	var logged bool
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "idle in a transaction") && strings.Contains(e.Message, "last statement: UPDATE o SET v = 10 WHERE id = 1") {
			logged = true
		}
	}
	if !logged {
		t.Error("the disconnect was not logged with the last statement")
	}

	// 実行中の文がロックを持つ接続は、時間を過ぎても切らない
	ctx := context.Background()
	c, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{"BEGIN", "UPDATE o SET v = 30 WHERE id = 1", "SELECT SLEEP(1.2)", "COMMIT"} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if got := one(t, b, "SELECT v FROM o WHERE id = 1"); got != "30" {
		t.Errorf("the running transaction was cut: v = %s", got)
	}
}

// ロックを待ちきれなかった書き込み（1205）のときに、持ち主をログに出す。指標と heimdb_status にも持ち主が出る。
func TestLockHolderVisible(t *testing.T) {
	addr, pro, stop := lockServer(t, 500*time.Millisecond, 0, false)
	defer stop()
	hook := logtest.NewGlobal()
	defer hook.Reset()
	a, b := lockDB(t, addr, "root", ""), lockDB(t, addr, "root", "")
	defer a.Close()
	defer b.Close()
	mustExec(t, b, "CREATE TABLE o (id INT PRIMARY KEY, v INT)")
	mustExec(t, b, "INSERT INTO o VALUES (1, 1), (2, 2)")
	held, holder := heldTxn(t, a, "UPDATE o SET v = 10 WHERE id = 1")
	defer held.Close()

	_, err := b.Exec("UPDATE o SET v = 20 WHERE id = 2")
	if mysqlErrNo(err) != 1205 {
		t.Fatalf("err = %v", err)
	}
	want := fmt.Sprintf("the write lock of branch main is held by connection %d (root) for", holder)
	var logged bool
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "lock wait timeout") && strings.Contains(e.Message, want) && strings.Contains(e.Message, "idle for") {
			logged = true
		}
	}
	if !logged {
		for _, e := range hook.AllEntries() {
			t.Log(e.Message)
		}
		t.Error("the lock holder was not logged with the 1205")
	}

	var mw metrics.Writer
	pro.WriteMetrics(&mw)
	body := mw.Bytes()
	for _, w := range []string{fmt.Sprintf("heimdb_branch_write_lock_holder{branch=\"main\"} %d", holder), "heimdb_branch_write_lock_held_seconds{branch=\"main\"}", "heimdb_branch_write_lock_idle_seconds{branch=\"main\"}", "heimdb_connection_write_lock_held_seconds{branch=\"main\"} "} {
		if !strings.Contains(string(body), w) {
			t.Errorf("metrics have no %s", w)
		}
	}
	// 誰もロックを持っていなくても、接続の持っている時間（0）と、見張りの切った数（0）は出る
	if _, err := held.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	held.Close()
	var free metrics.Writer
	pro.WriteMetrics(&free)
	for _, w := range []string{"heimdb_connection_write_lock_held_seconds{branch=\"main\"} 0\n", "heimdb_idle_lock_disconnects_total 0\n"} {
		if !strings.Contains(string(free.Bytes()), w) {
			t.Errorf("metrics without a holder have no %q", w)
		}
	}
	held, holder = heldTxn(t, a, "UPDATE o SET v = 10 WHERE id = 1")
	defer held.Close()
	var status string
	rows, err := b.Query("CALL heimdb_status()")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var value sql.NullString
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatal(err)
		}
		if name == "write_lock main" {
			status = value.String
		}
	}
	rows.Close()
	if !strings.Contains(status, want) || !strings.Contains(status, "last statement: UPDATE o SET v = 10 WHERE id = 1") {
		t.Errorf("heimdb_status write_lock main = %q", status)
	}
}

// ロックを待っている文は、KILL QUERY で待ち切らずに止まる（エラー 1317）。
func TestKillQueryStopsLockWait(t *testing.T) {
	addr, _, stop := lockServer(t, 30*time.Second, 0, false)
	defer stop()
	a, b, k := lockDB(t, addr, "root", ""), lockDB(t, addr, "root", ""), lockDB(t, addr, "root", "")
	defer a.Close()
	defer b.Close()
	defer k.Close()
	mustExec(t, b, "CREATE TABLE o (id INT PRIMARY KEY, v INT)")
	mustExec(t, b, "INSERT INTO o VALUES (1, 1), (2, 2)")
	held, _ := heldTxn(t, a, "UPDATE o SET v = 10 WHERE id = 1")
	defer held.Close()
	var waiter uint32
	if err := b.QueryRow("SELECT CONNECTION_ID()").Scan(&waiter); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := b.Exec("UPDATE o SET v = 20 WHERE id = 2")
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	mustExec(t, k, fmt.Sprintf("KILL QUERY %d", waiter))
	select {
	case err := <-done:
		if mysqlErrNo(err) != 1317 || time.Since(start) > 5*time.Second {
			t.Errorf("err = %v after %s", err, time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("KILL QUERY did not stop the lock wait")
	}
}

// PROCESS 権限のないユーザーは、SHOW PROCESSLIST で自分のユーザーの接続だけを見て、それを KILL できる。ほかの
// ユーザーの接続は 1095、ない接続は 1094（本物の MySQL 8.0 と同じ）。
func TestOwnProcessListAndKill(t *testing.T) {
	addr, _, stop := lockServer(t, 5*time.Second, 0, true)
	defer stop()
	root := lockDB(t, addr, "root", "rootpw")
	defer root.Close()
	mustExec(t, root, "CREATE USER 'site'@'%' IDENTIFIED BY 'sitepw'")
	mustExec(t, root, "GRANT ALL ON "+testDB+".* TO 'site'@'%'")
	site, other := lockDB(t, addr, "site", "sitepw"), lockDB(t, addr, "site", "sitepw")
	defer site.Close()
	defer other.Close()
	var rootID, otherID uint32
	if err := root.QueryRow("SELECT CONNECTION_ID()").Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	if err := other.QueryRow("SELECT CONNECTION_ID()").Scan(&otherID); err != nil {
		t.Fatal(err)
	}

	rows, err := site.Query("SHOW PROCESSLIST")
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for rows.Next() {
		var id int64
		var user, host, command, state, info sql.NullString
		var db sql.NullString
		var tm int64
		if err := rows.Scan(&id, &user, &host, &db, &command, &tm, &state, &info); err != nil {
			t.Fatal(err)
		}
		users = append(users, user.String)
	}
	rows.Close()
	if len(users) != 2 || users[0] != "site" || users[1] != "site" {
		t.Errorf("site sees %v", users)
	}

	for _, c := range []struct {
		q    string
		code uint16
	}{
		{fmt.Sprintf("KILL %d", rootID), 1095},
		{fmt.Sprintf("KILL QUERY %d", rootID), 1095},
		{"KILL 999999", 1094},
		{fmt.Sprintf("KILL QUERY %d", otherID), 0},
		{fmt.Sprintf("KILL %d", otherID), 0},
	} {
		_, err := site.Exec(c.q)
		if mysqlErrNo(err) != c.code || (c.code == 0 && err != nil) {
			t.Errorf("%s: %v", c.q, err)
		}
	}
	// database/sql は切られた接続の代わりを開くので、接続 ID が変わったかで確かめる
	var after uint32
	_ = other.QueryRow("SELECT CONNECTION_ID()").Scan(&after)
	if after == otherID {
		t.Error("the killed connection still works")
	}
	if _, err := root.Exec("KILL 999999"); mysqlErrNo(err) != 1094 {
		t.Errorf("root KILL of an unknown thread: %v", err)
	}
}

// main には実行時間の上限だけを付けられ、管理者でない接続の SELECT と書き込みに効く（DDL と手続きには効かない）。
// 時間の上限だけでは一時テーブルを拒否しない。
func TestMainExecutionLimit(t *testing.T) {
	addr, _, stop := lockServer(t, 5*time.Second, 0, true)
	defer stop()
	root := lockDB(t, addr, "root", "rootpw")
	defer root.Close()
	mustExec(t, root, "CREATE USER 'site'@'%' IDENTIFIED BY 'sitepw'")
	mustExec(t, root, "GRANT ALL ON "+testDB+".* TO 'site'@'%'")
	mustExec(t, root, "CREATE TABLE a (i INT PRIMARY KEY)")
	mustExec(t, root, "INSERT INTO a WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) SELECT i FROM n")
	mustExec(t, root, "CREATE TABLE r (c BIGINT)")
	if _, err := root.Exec("CALL heimdb_branch_limit('main', '1G', '0')"); err == nil {
		t.Error("a size limit on main was accepted")
	}
	mustExec(t, root, "CALL heimdb_branch_limit('main', '0', '100ms')")
	site := lockDB(t, addr, "site", "sitepw")
	defer site.Close()
	const heavy = "SELECT COUNT(*) FROM a x, a y, a z WHERE x.i + y.i + z.i > 0"
	for _, c := range []struct {
		db      *sql.DB
		q       string
		stopped bool
	}{
		{site, heavy, true},
		{site, "INSERT INTO r " + heavy, true},
		{site, "CREATE TEMPORARY TABLE tmp (i INT)", false},
		{root, heavy, false},
	} {
		_, err := c.db.Exec(c.q)
		if stopped := mysqlErrNo(err) == 3024; stopped != c.stopped || (!c.stopped && err != nil) {
			t.Errorf("%s: %v", c.q, err)
		}
	}
	if got := one(t, root, "SELECT COUNT(*) FROM r"); got != "0" {
		t.Errorf("the stopped INSERT left %s rows", got)
	}
}

// HeimDB が読まない変数を接続ごとに SET すると、変数ごとに 1 回、警告が出る（受け付けはする）。
func TestUnreadVariableWarning(t *testing.T) {
	addr, _, stop := lockServer(t, 5*time.Second, 0, false)
	defer stop()
	unreadWarned.Delete("wait_timeout")
	hook := logtest.NewGlobal()
	defer hook.Reset()
	db := lockDB(t, addr, "root", "")
	defer db.Close()
	mustExec(t, db, "SET SESSION wait_timeout = 60")
	mustExec(t, db, "SET SESSION wait_timeout = 30")
	mustExec(t, db, "SET SESSION sql_mode = ''")
	var n int
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "SET wait_timeout is accepted but has no effect") {
			n++
		}
		if strings.Contains(e.Message, "sql_mode") {
			t.Errorf("warned about a variable HeimDB reads: %s", e.Message)
		}
	}
	if n != 1 {
		t.Errorf("warnings = %d", n)
	}
}

// 見張りが接続を切るのと、その接続の COMMIT が同時に来ても、書き込みは全部入ったか全部ないかのどちらかで、ロックは
// 外れる（-race で流す）。
func TestIdleLockTimeoutRacesCommit(t *testing.T) {
	addr, _, stop := lockServer(t, 5*time.Second, 60*time.Millisecond, false)
	defer stop()
	setup := lockDB(t, addr, "root", "")
	defer setup.Close()
	mustExec(t, setup, "CREATE TABLE o (id INT PRIMARY KEY, a INT, b INT)")
	mustExec(t, setup, "INSERT INTO o VALUES (1, 0, 0)")
	for i := range 20 {
		db := lockDB(t, addr, "root", "")
		c, _ := heldTxn(t, db, fmt.Sprintf("UPDATE o SET a = %d WHERE id = 1", i+1))
		ctx := context.Background()
		_, _ = c.ExecContext(ctx, fmt.Sprintf("UPDATE o SET b = %d WHERE id = 1", i+1))
		time.Sleep(time.Duration(40+i*3) * time.Millisecond)
		_, _ = c.ExecContext(ctx, "COMMIT")
		c.Close()
		db.Close()
		// どちらの場合も、ロックは外れていて、2 つの列は揃っている
		mustExec(t, setup, "UPDATE o SET a = a, b = b WHERE id = 1")
		var a, b int
		if err := setup.QueryRow("SELECT a, b FROM o WHERE id = 1").Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("round %d: a = %d, b = %d", i, a, b)
		}
	}
}

// 接続が持っている書き込みのロックの時間は、ブランチごとに出る。検証環境のブランチでロックを持っても、main は 0 の
// まま。誰も持っていないブランチも 0 で出る。マージ（内部の操作）が持つロックは数えない。
func TestConnectionWriteLockByBranch(t *testing.T) {
	addr, pro, stop := lockServer(t, 5*time.Second, 0, false)
	defer stop()
	a := lockDB(t, addr, "root", "")
	defer a.Close()
	mustExec(t, a, "CREATE TABLE o (id INT PRIMARY KEY, v INT)")
	mustExec(t, a, "INSERT INTO o VALUES (1, 1)")
	mustExec(t, a, "CALL heimdb_branch_create('stg1')")
	mustExec(t, a, "CALL heimdb_branch_create('stg2')")
	stg1, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(%s)/%s@stg1", addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	defer stg1.Close()
	stg1.SetMaxOpenConns(1)

	metricsOf := func() string {
		var mw metrics.Writer
		pro.WriteMetrics(&mw)
		return string(mw.Bytes())
	}
	value := func(body, branch string) string {
		prefix := fmt.Sprintf("heimdb_connection_write_lock_held_seconds{branch=%q} ", branch)
		for _, l := range strings.Split(body, "\n") {
			if v, ok := strings.CutPrefix(l, prefix); ok {
				return v
			}
		}
		return ""
	}
	body := metricsOf()
	for _, b := range []string{"main", "stg1", "stg2"} {
		if v := value(body, b); v != "0" {
			t.Errorf("no holder: branch %s = %q, want 0", b, v)
		}
	}

	held, _ := heldTxn(t, stg1, "UPDATE o SET v = 10 WHERE id = 1")
	defer held.Close()
	// 一時テーブルへの書き込みは隠しブランチのロックを取るが、その接続しか待たないので出さない
	for _, q := range []string{"CREATE TEMPORARY TABLE tmp (id INT PRIMARY KEY)", "INSERT INTO tmp VALUES (1)"} {
		if _, err := held.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	body = metricsOf()
	if v := value(body, "stg1"); v == "" || v == "0" {
		t.Errorf("stg1 held by a connection = %q, want > 0", v)
	}
	for _, b := range []string{"main", "stg2"} {
		if v := value(body, b); v != "0" {
			t.Errorf("stg1 held: branch %s = %q, want 0", b, v)
		}
	}
	if v := value(body, "temporary"); v != "" {
		t.Errorf("a temporary branch is listed: %q", v)
	}
	if !strings.Contains(body, "heimdb_branch_write_lock_held_seconds{branch=\"temporary\"}") {
		t.Error("the temporary table's write lock was not taken (the check above tests nothing)")
	}
	if strings.Contains(body, "\nheimdb_connection_write_lock_held_seconds ") {
		t.Error("the unlabeled series is written")
	}
	if strings.Count(body, "# TYPE heimdb_connection_write_lock_held_seconds ") != 1 {
		t.Errorf("TYPE line is not written once:\n%s", body)
	}

	// マージのような内部の操作が持つロックは数えない（branch_write_lock_held_seconds には出る）
	b2, ok := pro.st.BranchByName("stg2")
	if !ok {
		t.Fatal("no stg2")
	}
	release, err := pro.st.LockBranchFor(b2.ID, time.Second, store.LockOwner{Op: "merge"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	time.Sleep(50 * time.Millisecond)
	body = metricsOf()
	if v := value(body, "stg2"); v != "0" {
		t.Errorf("stg2 held by a merge = %q, want 0", v)
	}
	if !strings.Contains(body, "heimdb_branch_write_lock_held_seconds{branch=\"stg2\"}") {
		t.Error("the merge's lock is not in heimdb_branch_write_lock_held_seconds")
	}
}
