package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// panicExpr は、値が 3 の行を評価すると panic する式（テスト用）。
type panicExpr struct{ child gsql.Expression }

func (e panicExpr) Resolved() bool              { return e.child.Resolved() }
func (e panicExpr) String() string              { return fmt.Sprintf("test_panic(%s)", e.child) }
func (e panicExpr) Type() gsql.Type             { return types.Int64 }
func (e panicExpr) IsNullable() bool            { return true }
func (e panicExpr) Children() []gsql.Expression { return []gsql.Expression{e.child} }
func (e panicExpr) WithChildren(c ...gsql.Expression) (gsql.Expression, error) {
	return panicExpr{c[0]}, nil
}
func (e panicExpr) Eval(ctx *gsql.Context, row gsql.Row) (interface{}, error) {
	v, err := e.child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	n, _, err := types.Int64.Convert(ctx, v)
	if err != nil {
		return nil, err
	}
	if n == int64(3) {
		panic("test panic in Eval")
	}
	return n, nil
}

// startPanicServer は、panic する関数 test_panic(x)（x が 3 なら評価で panic）と test_panic_build()（文の組み立てで
// panic）を足したサーバ。
func startPanicServer(t *testing.T) (dsn string, stop func()) {
	t.Helper()
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{engine: func(e *sqle.Engine) {
		e.Analyzer.Catalog.RegisterFunction(gsql.NewEmptyContext(),
			gsql.Function1{Name: "test_panic", Fn: func(e gsql.Expression) gsql.Expression { return panicExpr{e} }},
			gsql.Function0{Name: "test_panic_build", Fn: func() gsql.Expression { panic("test panic in planning") }},
		)
	}})
	return fmt.Sprintf("root@tcp(%s)/%s", addr, testDB), stopServer(t, st, srv)
}

// エンジンが文の処理の panic を回復し、エラー 1105 にして接続を保つ（段階 46）。その文の書き込みは取り消し、明示的な
// トランザクションの前の文の書き込みは残す。
func TestQueryPanicKeepsConnection(t *testing.T) {
	dsn, stop := startPanicServer(t)
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var connID int
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID); err != nil {
		t.Fatal(err)
	}
	// panic の後も同じ接続（同じ接続 ID）で続けられる。
	wantPanic := func(q string, args ...any) {
		t.Helper()
		_, err := conn.ExecContext(ctx, q, args...)
		if mysqlErrNo(err) != 1105 || !strings.Contains(err.Error(), "HeimDB: internal error") {
			t.Errorf("%s: err = %v, want error 1105 (internal error)", q, err)
		}
		var id int
		if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil || id != connID {
			t.Fatalf("%s: the connection was dropped: id %d, %v", q, id, err)
		}
	}

	exec("CREATE TABLE src (id bigint NOT NULL PRIMARY KEY)")
	exec("INSERT INTO src VALUES (1), (2), (3), (4)")
	exec("CREATE TABLE dst (id bigint NOT NULL PRIMARY KEY, v bigint)")

	wantPanic("SELECT test_panic(3)")
	wantPanic("SELECT test_panic_build()")
	// 準備した文（組み立てと実行）。
	wantPanic("SELECT test_panic(?)", 3)
	wantPanic("SELECT test_panic_build() + ?", 1)

	count := func(q string) int {
		t.Helper()
		var n int
		if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	// 自動コミット: 行を 2 つ書いた後に panic しても、何も残らない。
	wantPanic("INSERT INTO dst SELECT id, test_panic(id) FROM src ORDER BY id")
	if n := count("SELECT COUNT(*) FROM dst"); n != 0 {
		t.Errorf("autocommit: %d rows left after the panic", n)
	}
	// 明示的なトランザクション: panic した文の行だけを取り消し、前の文の行はコミットできる。
	exec("BEGIN")
	exec("INSERT INTO dst VALUES (100, 0)")
	wantPanic("INSERT INTO dst SELECT id, test_panic(id) FROM src ORDER BY id")
	if n := count("SELECT COUNT(*) FROM dst"); n != 1 {
		t.Errorf("transaction: %d rows after the panic, want 1", n)
	}
	exec("INSERT INTO dst VALUES (101, 0)")
	exec("COMMIT")
	if n := count("SELECT COUNT(*) FROM dst WHERE id IN (100, 101)"); n != 2 {
		t.Errorf("after commit: %d of the rows 100 and 101", n)
	}
	if n := count("SELECT COUNT(*) FROM dst"); n != 2 {
		t.Errorf("after commit: %d rows, want 2", n)
	}
	// 書き込みのロックが残らない（別の接続から書ける）。
	if _, err := db.ExecContext(ctx, "INSERT INTO dst VALUES (102, 0)"); err != nil {
		t.Errorf("another connection cannot write: %v", err)
	}
	// 複数の行を返す文（行を送るゴルーチンで回復する経路）。行を送り始める前の panic なら、接続は続く。
	rows, err := conn.QueryContext(ctx, "SELECT test_panic(id) FROM src ORDER BY id")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if mysqlErrNo(err) != 1105 || !strings.Contains(err.Error(), "HeimDB: internal error: test panic in Eval") {
		t.Errorf("multi-row SELECT: err = %v, want error 1105 (internal error)", err)
	}
	var id int
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil || id != connID {
		t.Fatalf("multi-row SELECT: the connection was dropped: id %d, %v", id, err)
	}
}

// 複数の文の接続で、エラー（panic と実行時のエラー）の文の後の文は実行しない（MySQL と同じ。段階 46 のレビュー）。
func TestMultiStatementStopsAtError(t *testing.T) {
	dsn, stop := startPanicServer(t)
	defer stop()
	db, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE dst (id bigint NOT NULL PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	for i, mid := range []string{"SELECT test_panic(3)", "SELECT nosuchcol FROM dst", "SELECT CONVERT('a' USING ujis)"} {
		first, last := 10*i+1, 10*i+2
		q := fmt.Sprintf("INSERT INTO dst VALUES (%d); %s; INSERT INTO dst VALUES (%d)", first, mid, last)
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := conn.QueryContext(ctx, q)
		for err == nil {
			for rows.Next() {
			}
			if !rows.NextResultSet() {
				err = rows.Err()
				break
			}
		}
		if rows != nil {
			rows.Close()
		}
		if err == nil {
			t.Errorf("%s: no error", q)
		}
		var n int
		if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM dst WHERE id = %d", first)).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: the first statement: %d rows, %v", q, n, err)
		}
		if err := conn.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM dst WHERE id = %d", last)).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: the statement after the error ran: %d rows, %v", q, n, err)
		}
		conn.Close()
	}
}
