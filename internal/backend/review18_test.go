package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gmsserver "github.com/tsukinowasha/heimdb/internal/sqlengine/server"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 18 のレビュー。

// startShutdownServer は、Provider と Store を返すサーバ（停止の手順を試すため）。停止は呼び出し側が行う。
func startShutdownServer(t *testing.T, dir string) (addr string, pro *Provider, st *store.Store, srv *gmsserver.Server) {
	t.Helper()
	return serve(t, dir, serverConfig{})
}

func openRoot(t *testing.T, addr string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// 停止が始まったら、ロックを待っている文は 1053 で終わり、実行中の文は終わるまで待ち、開いたままのトランザクションの
// 次の文は 1053 で拒否され、その後 Store を閉じても（開いたままのトランザクションがあっても）エラーにならず、
// 閉じた後の切断でも panic しない。durability_test.go の TestGracefulShutdown は、プロセスが終わった後に
// 「次の文がエラーになる」ことしか見ていない（1053 ではなく接続が切れたエラー）ので、ここで中身を確かめる。
func TestReview18ShutdownRefusesWith1053AndWaitsForRunningStatements(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	setup := openRoot(t, addr)
	mustExec(t, setup, "CREATE TABLE t (id int NOT NULL PRIMARY KEY, v int NOT NULL)")
	mustExec(t, setup, "INSERT INTO t VALUES (1, 0)")
	setup.Close()

	// A: 書き込みロックを持ったままのトランザクション。
	a := openRoot(t, addr)
	defer a.Close()
	atx, err := a.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := atx.Exec("UPDATE t SET v = 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	// B: A のロックを待つ文。
	b := openRoot(t, addr)
	defer b.Close()
	bErr := make(chan error, 1)
	go func() {
		_, err := b.Exec("UPDATE t SET v = 2 WHERE id = 1")
		bErr <- err
	}()
	// C: 実行中の長い文（停止はこれが終わるのを待つ）。
	c := openRoot(t, addr)
	defer c.Close()
	cDone := make(chan error, 1)
	go func() {
		_, err := c.Exec("SELECT SLEEP(1)")
		cDone <- err
	}()
	time.Sleep(200 * time.Millisecond)

	srv.Close()
	start := time.Now()
	ok := pro.Shutdown(10 * time.Second)
	waited := time.Since(start)
	if !ok {
		t.Fatal("Shutdown timed out although only SLEEP(1) was running")
	}
	if waited < 500*time.Millisecond {
		t.Errorf("Shutdown returned after %s without waiting for the running SLEEP(1)", waited)
	}
	select {
	case err := <-bErr:
		if mysqlErrNo(err) != 1053 {
			t.Errorf("statement waiting for the lock got %v, want error 1053", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the statement waiting for the lock was not aborted")
	}
	if err := <-cDone; err != nil {
		t.Errorf("running statement failed: %v", err)
	}
	// 開いたままのトランザクションの次の文は 1053。
	if _, err := atx.Exec("SELECT 1"); mysqlErrNo(err) != 1053 {
		t.Errorf("statement after shutdown: %v, want error 1053", err)
	}
	// A のトランザクションはロックを持ったまま（SessionEnd も何もしない）。それでも Store は閉じられる。
	if err := st.Close(); err != nil {
		t.Errorf("Close with an open transaction: %v", err)
	}
	// 閉じた後の切断（SessionEnd）と、閉じた後の文が panic しない。
	if _, err := atx.Exec("SELECT 1"); mysqlErrNo(err) != 1053 {
		t.Errorf("statement after close: %v, want error 1053", err)
	}
	a.Close()
	b.Close()
	c.Close()
	time.Sleep(200 * time.Millisecond)
}

// StopLockWaits の後でも、空いているロックは取れる（以前は select が無作為に道を選び、停止の前に始まった文が競合
// していないのに 1053 で失敗することがあった）。
func TestReview18LockBranchAfterStopIsNondeterministicWhenFree(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.StopLockWaits()
	var got, refused int
	for i := 0; i < 200; i++ {
		release, err := st.LockBranch(store.MainBranchID, time.Second)
		if err == nil {
			got++
			release()
		} else if errors.Is(err, store.ErrShuttingDown) {
			refused++
		} else {
			t.Fatal(err)
		}
	}
	t.Logf("free lock after StopLockWaits: acquired %d, refused %d", got, refused)
	if refused > 0 {
		t.Errorf("LockBranch on a free lock after StopLockWaits: acquired %d, refused %d of 200", got, refused)
	}
}

// go-mysql-server が実装していない文字セットへの CONVERT ... USING は、結果の列の型が nil になり、handler.go の
// schemaToFields で panic していた（PHPUnit の wpdb グループ。WHERE の定数や UPDATE では接続が切れた）。構文解析の
// 直後に明示的なエラー（1235）にしていた（parser.go の checkConvertUsing）。段階 46 からはエンジン（planbuilder と
// ConvertUsing.Eval）がエラー 1235 を返す。
func TestReview18ConvertUsingUnsupportedCharset(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, cs := range []string{"ujis", "koi8r", "big5", "cp1251", "tis620", "hebrew"} {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf("SELECT CONVERT( LEFT( CONVERT( 'abc' USING %s ), 4 ) USING utf8 ) AS x_0", cs)
		var x string
		err = conn.QueryRowContext(ctx, q).Scan(&x)
		if err == nil {
			t.Logf("%s: %q", cs, x)
		} else {
			t.Logf("%s: %v", cs, err)
		}
		if mysqlErrNo(err) != 1235 {
			t.Errorf("%s: err = %v, want error 1235", cs, err)
		}
		// 同じ接続が生きている。
		var one int
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Errorf("%s: the connection was dropped: %v", cs, err)
		}
		conn.Close()
	}
	// 評価だけでも panic する（結果の型が決まる前に行を読む経路: WHERE の中）。
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT 1 AS a) s WHERE CONVERT('abc' USING ujis) = 'abc'").Scan(&n)
	t.Logf("eval in WHERE: n=%d err=%v", n, err)
	if mysqlErrNo(err) != 1235 {
		t.Errorf("eval: err = %v, want error 1235", err)
	}
	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Errorf("eval: the connection was dropped: %v", err)
	}
	// 回復のない経路（UPDATE、INSERT）、サブクエリ、プリペアドステートメント。知らない文字セットは MySQL と同じ
	// 「Unknown character set」（go-mysql-server は 1105 で包む）。
	if _, err := conn.ExecContext(ctx, "CREATE TABLE cu (id int NOT NULL PRIMARY KEY, v varchar(10) NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]uint16{
		"UPDATE cu SET v = CONVERT(v USING hebrew)":                          1235,
		"INSERT INTO cu VALUES (1, CONVERT('a' USING big5))":                 1235,
		"SELECT (SELECT CONVERT('a' USING koi8r)) AS x":                      1235,
		"SELECT v FROM cu ORDER BY CONVERT(v USING tis620)":                  1235,
		"SELECT CONVERT('a' USING nosuchcharset)":                            1105,
		"SELECT CONVERT('a' USING utf8mb4), CONVERT('a' USING latin1) AS ok": 0,
	} {
		_, err := conn.ExecContext(ctx, q)
		if got := mysqlErrNo(err); got != want {
			t.Errorf("%s: err = %v, want error %d", q, err, want)
		}
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("%s: the connection was dropped: %v", q, err)
		}
	}
	if _, err := db.PrepareContext(ctx, "SELECT CONVERT(? USING ujis)"); err == nil {
		t.Errorf("prepared CONVERT USING ujis was accepted")
	}
	// 接続の文字セットが実装されていないとき（PHPUnit の wpdb グループが SET NAMES big5 の後に送る）。段階 46 で
	// checkConvertUsing をエンジンに移したとき、中の文字列のリテラルを先に組み立てて panic していた（最後の守りで
	// エラー 1105 の「internal error」になった）。文字列のリテラルも、panic ではなく文字セットのエラーにする。
	if _, err := conn.ExecContext(ctx, "INSERT INTO cu VALUES (1, 'abc')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "SET NAMES big5"); err != nil {
		t.Fatal(err)
	}
	// 段階 46 のレビュー: 数のリテラル（DECIMAL に入らないもの）、表の列（結果の文字セットが big5）、
	// CAST ... CHARACTER SET と CHAR ... USING も、panic ではなくエラーにする。
	for q, want := range map[string]uint16{
		"SELECT CONVERT( LEFT( CONVERT( 'abc' USING big5 ), 4 ) USING utf8mb4 )": 1235,
		"SELECT CONVERT( CONVERT( 'abc' USING koi8r ) USING utf8mb4 )":           1235,
		"SELECT 'abc'": 1105,
		"SELECT 1." + strings.Repeat("0", 70) + "1":         1105,
		"SELECT v FROM cu":                                  1105,
		"SELECT CAST(v AS CHAR CHARACTER SET big5) FROM cu": 1235,
		"SELECT CHAR(65 USING big5)":                        1235,
	} {
		rows, err := conn.QueryContext(ctx, q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if got := mysqlErrNo(err); got != want || strings.Contains(err.Error(), "internal error") {
			t.Errorf("after SET NAMES big5: %s: err = %v, want error %d (not an internal error)", q, err, want)
		}
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("%s: the connection was dropped: %v", q, err)
		}
	}
	// 準備した文の文字列の引数（planbuilder の SetBindings）。
	var x string
	err = conn.QueryRowContext(ctx, "SELECT ?", "abc").Scan(&x)
	if mysqlErrNo(err) != 1105 || strings.Contains(err.Error(), "internal error") {
		t.Errorf("after SET NAMES big5: prepared string parameter: err = %v, want error 1105 (not an internal error)", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("prepared string parameter: the connection was dropped: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SET NAMES utf8mb4"); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT 'abc'").Scan(&x); err != nil || x != "abc" {
		t.Errorf("after SET NAMES utf8mb4: %q, %v", x, err)
	}
	// 表の列を、使えない文字セットへ CAST しても、panic ではなくエラー（utf8mb4 の接続でも）。
	for _, q := range []string{"SELECT CAST(v AS CHAR CHARACTER SET ujis) FROM cu", "SELECT CAST('a' AS CHAR CHARACTER SET big5)", "SELECT CHAR(65 USING koi8r)"} {
		if _, err := conn.ExecContext(ctx, q); mysqlErrNo(err) != 1235 {
			t.Errorf("%s: err = %v, want error 1235", q, err)
		}
	}
	if err := conn.QueryRowContext(ctx, "SELECT CAST('a' AS CHAR CHARACTER SET latin1)").Scan(&x); err != nil || x != "a" {
		t.Errorf("CAST to latin1: %q, %v", x, err)
	}
	conn.Close()
}
