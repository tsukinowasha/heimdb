package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// マージと復元は、変わる行を集めて計画を作る間と、列を変えたテーブルを書き直す間も、メモリの上限を確かめる。上限で
// 止まると 1041 を返して何も変えず、上限を外せば同じ操作が通る（docs/plan.md の段階 58。GCE の 310 万行で、上限を
// 付けても 1041 にならずに OOM で落ちた）。
func memoryTable(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	mustExec(t, db, "CREATE TABLE big (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(50) NOT NULL, KEY v (v))")
	var vals []string
	for i := 1; i <= n; i++ {
		vals = append(vals, fmt.Sprintf("(%d, 'v%d')", i, i))
	}
	mustExec(t, db, "INSERT INTO big (id, v) VALUES "+strings.Join(vals, ","))
}

func expectOutOfMemory(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	SetMemoryLimit(1 << 20)
	_, err := db.Exec(q)
	SetMemoryLimit(0)
	if err == nil || !strings.Contains(err.Error(), "Error 1041") || !strings.Contains(err.Error(), "HeimDB: out of memory") {
		t.Errorf("%s: %v", q, err)
	}
}

func TestMergeMemoryLimit(t *testing.T) {
	var st *store.Store
	mainDB, dsn, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	defer SetMemoryLimit(0)
	memoryTable(t, mainDB, 3000)
	mustExec(t, mainDB, "CALL heimdb_branch_create('rows')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('cols')")
	rows := openBranch(t, dsn, "rows")
	defer rows.Close()
	mustExec(t, rows, "UPDATE big SET v = CONCAT(v, 'x')")
	cols := openBranch(t, dsn, "cols")
	defer cols.Close()
	mustExec(t, cols, "ALTER TABLE big ADD COLUMN extra int NOT NULL DEFAULT 0")

	// 計画を作る間（書かない dry_run も）。store の変わった行の一覧の確かめを外しても、行を読む側で止まる。
	for _, readCheck := range []bool{true, false} {
		if !readCheck {
			st.ReadCheck = nil
		}
		for _, b := range []string{"rows", "cols"} {
			expectOutOfMemory(t, mainDB, "CALL heimdb_branch_merge('"+b+"', 'dry_run')")
			expectOutOfMemory(t, mainDB, "CALL heimdb_branch_merge('"+b+"')")
		}
	}
	st.ReadCheck = checkMemory
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%x'"); got != "0" {
		t.Errorf("rows changed by a stopped merge: %s", got)
	}
	if _, err := mainDB.Exec("SELECT extra FROM big LIMIT 1"); err == nil {
		t.Error("a stopped merge added the column")
	}
	// 次の文も、上限を外した同じマージも通る
	mustExec(t, mainDB, "CALL heimdb_branch_merge('rows')")
	mustExec(t, mainDB, "CALL heimdb_branch_merge('cols')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%x' AND extra = 0"); got != "3000" {
		t.Errorf("rows after the merges: %s", got)
	}
}

func TestRestoreMemoryLimit(t *testing.T) {
	var st *store.Store
	mainDB, _, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	defer SetMemoryLimit(0)
	memoryTable(t, mainDB, 3000)
	time.Sleep(10 * time.Millisecond)
	at := timeArg()
	time.Sleep(10 * time.Millisecond)
	mustExec(t, mainDB, "UPDATE big SET v = CONCAT(v, 'x')")
	for _, readCheck := range []bool{true, false} {
		if !readCheck {
			st.ReadCheck = nil
		}
		expectOutOfMemory(t, mainDB, "CALL heimdb_restore_preview('main', '"+at+"', 'big')")
		expectOutOfMemory(t, mainDB, "CALL heimdb_restore('main', '"+at+"', 'big')")
	}
	st.ReadCheck = checkMemory
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%x'"); got != "3000" {
		t.Errorf("rows restored by a stopped restore: %s", got)
	}
	mustExec(t, mainDB, "CALL heimdb_restore('main', '"+at+"', 'big')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%x'"); got != "0" {
		t.Errorf("rows after the restore: %s", got)
	}
}

// 変わった行の一覧は、集める間に ReadCheck を呼び、そのエラーで止まる。
func TestChangedRowsReadCheck(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE c (id bigint NOT NULL PRIMARY KEY)")
	var vals []string
	for i := 1; i <= 3000; i++ {
		vals = append(vals, fmt.Sprintf("(%d)", i))
	}
	s.mustQuery(t, "INSERT INTO c VALUES "+strings.Join(vals, ","))
	s.ctx.Session.(*Session).CommandEnd()
	def, ok, err := s.st.GetTable(store.MainBranchID, "c", store.Latest)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	calls := 0
	stop := fmt.Errorf("stop")
	s.st.ReadCheck = func() error {
		calls++
		if calls == 2 {
			return stop
		}
		return nil
	}
	defer func() { s.st.ReadCheck = checkMemory }()
	if _, err := s.st.ChangedRowsSince(store.MainBranchID, def.ID, 0, s.st.LastCommitted()); err != stop || calls != 2 {
		t.Errorf("err %v after %d calls", err, calls)
	}
}

// マージと復元が自分のトランザクションに書く editor（tx を持つ）も、書くたびにメモリの上限を確かめる（GCE の 310 万行の
// 復元で、計画を作った後の適用の間に確かめず、上限を付けても OOM で落ちた）。
func TestOwnTxnEditorMemoryLimit(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	defer SetMemoryLimit(0)
	s.mustQuery(t, "CREATE TABLE w (id bigint NOT NULL PRIMARY KEY, v varchar(10))")
	s.ctx.Session.(*Session).CommandEnd()
	pro := s.ctx.Session.(*Session).pro
	db, ok := pro.database(testDB)
	if !ok {
		t.Fatal("no database")
	}
	def, ok, err := s.st.GetTable(store.MainBranchID, "w", store.Latest)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	tbl, err := newTable(s.ctx, db, def)
	if err != nil {
		t.Fatal(err)
	}
	tx := s.st.Begin()
	defer tx.Rollback()
	ed := &editor{t: tbl, tx: tx, noWatch: true}
	SetMemoryLimit(1 << 20)
	var werr error
	for i := 1; i <= 2*interruptEvery && werr == nil; i++ {
		werr = ed.Insert(s.ctx, gsql.Row{int64(i), "x"})
	}
	if !isOutOfMemory(werr) {
		t.Errorf("writes with the transaction of a merge or restore: %v", werr)
	}
}

// コミットしていない書き込みが上限（store.MaxTxnBytes）を超えると、文は 1197 で取り消され、接続もトランザクションの
// それまでの書き込みも残る（docs/plan.md の段階 59。Pebble のバッチは 4 GiB で panic する）。
func TestUncommittedWritesLimit(t *testing.T) {
	defer func(n int) { store.MaxTxnBytes = n }(store.MaxTxnBytes)
	store.MaxTxnBytes = 1 << 20
	mainDB, _, stop := branchServer(t)
	defer stop()
	mainDB.SetMaxOpenConns(1)
	mustExec(t, mainDB, "CREATE TABLE big (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(200) NOT NULL)")
	mustExec(t, mainDB, "SET autocommit = 0")
	mustExec(t, mainDB, "INSERT INTO big (v) VALUES ('kept')")
	var vals []string
	for i := 0; i < 2000; i++ {
		vals = append(vals, "(REPEAT('x', 200))")
	}
	stmt := "INSERT INTO big (v) VALUES " + strings.Join(vals, ",")
	var err error
	for i := 0; i < 20 && err == nil; i++ {
		_, err = mainDB.Exec(stmt)
	}
	if err == nil || !strings.Contains(err.Error(), "Error 1197") || !strings.Contains(err.Error(), "--skip-no-autocommit") {
		t.Fatalf("growing the transaction: %v", err)
	}
	mustExec(t, mainDB, "COMMIT")
	mustExec(t, mainDB, "SET autocommit = 1")
	n := one(t, mainDB, "SELECT COUNT(*) FROM big")
	if n == "0" || (atoi(t, n)-1)%2000 != 0 {
		t.Errorf("rows after the refused statement: %s (the refused statement left rows)", n)
	}
	if got := one(t, mainDB, "SELECT v FROM big WHERE id = 1"); got != "kept" {
		t.Errorf("first row: %s", got)
	}
}

// マージと復元のトランザクションは、接続のトランザクションと別の上限（store.MaxInternalTxnBytes）で止まり、包んだ
// エラーも接続先には 1197 で返る。案内はその操作のもので、何も変えない（docs/plan.md の段階 59）。
func TestInternalWritesLimit(t *testing.T) {
	defer func(n, m int) { store.MaxTxnBytes, store.MaxInternalTxnBytes = n, m }(store.MaxTxnBytes, store.MaxInternalTxnBytes)
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	memoryTable(t, mainDB, 3000)
	at := timeArg()
	mustExec(t, mainDB, "UPDATE big SET v = CONCAT(v, 'y')")

	expect1197 := func(q, want string) {
		t.Helper()
		// 接続のトランザクションの上限（1 KiB）は効かない
		store.MaxTxnBytes, store.MaxInternalTxnBytes = 1<<10, 64<<10
		_, err := mainDB.Exec(q)
		store.MaxTxnBytes, store.MaxInternalTxnBytes = 1536<<20, 1900<<20
		if err == nil || !strings.Contains(err.Error(), "Error 1197") || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", q, err)
		}
	}
	expect1197("CALL heimdb_restore('main', '"+at+"', 'big')", "restore fewer tables at a time")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%y'"); got != "3000" {
		t.Errorf("rows restored by a stopped restore: %s", got)
	}
	mustExec(t, mainDB, "CALL heimdb_restore('main', '"+at+"', 'big')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%y'"); got != "0" {
		t.Errorf("rows after the restore: %s", got)
	}

	mustExec(t, mainDB, "CALL heimdb_branch_create('rows')")
	rows := openBranch(t, dsn, "rows")
	defer rows.Close()
	mustExec(t, rows, "UPDATE big SET v = CONCAT(v, 'x')")
	expect1197("CALL heimdb_branch_merge('rows')", "HeimDB: the merge writes more than")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%x'"); got != "0" {
		t.Errorf("rows changed by a stopped merge: %s", got)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_merge('rows')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM big WHERE v LIKE '%x'"); got != "3000" {
		t.Errorf("rows after the merge: %s", got)
	}
}

// 大きなバッチを確保する前の確かめ（checkAlloc）は、確保する分を足して境目を超えるなら 1041 を返す。上限がなければ
// 確かめない（docs/plan.md の段階 59）。
func TestCheckAlloc(t *testing.T) {
	defer SetMemoryLimit(0)
	if err := checkAlloc(1 << 40); err != nil {
		t.Errorf("without a limit: %v", err)
	}
	SetMemoryLimit(4 << 30)
	if err := checkAlloc(1 << 20); err != nil {
		t.Errorf("1 MiB under a 4 GiB limit: %v", err)
	}
	err := checkAlloc(3 << 30)
	if !isOutOfMemory(err) || !strings.Contains(err.Error(), "needs 3072 MiB more") {
		t.Errorf("3 GiB under a 4 GiB limit: %v", err)
	}
}

// 失敗した文（重複）の取り消しが、写す分の確保でメモリの上限に当たると、トランザクションごと取り消し、接続先には
// 元のエラーではなく 1041 を返す。go-mysql-server は取り消しのエラーを捨てるので、セッションが覚えて返す
// （docs/plan.md の段階 59）。
func TestDiscardOutOfMemory(t *testing.T) {
	var st *store.Store
	mainDB, _, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	defer func() { st.AllocCheck = checkAlloc }()
	mainDB.SetMaxOpenConns(1)
	mustExec(t, mainDB, "CREATE TABLE d (id bigint NOT NULL PRIMARY KEY, v varchar(200) NOT NULL)")
	mustExec(t, mainDB, "INSERT INTO d VALUES (1000000, 'committed')")
	mustExec(t, mainDB, "START TRANSACTION")
	var vals []string
	for i := 1; i <= 12000; i++ {
		vals = append(vals, fmt.Sprintf("(%d, REPEAT('x', 100))", i))
	}
	mustExec(t, mainDB, "INSERT INTO d VALUES "+strings.Join(vals, ","))
	// バッチが伸びるときの確かめは 2 MiB 以上を尋ねるので、2 MiB 未満（文の取り消しで写す分）だけを拒否する
	st.AllocCheck = func(n int) error {
		if n < 2<<20 {
			return errOutOfMemoryFor(0, int64(n), 1<<30)
		}
		return nil
	}
	_, err := mainDB.Exec("INSERT INTO d VALUES (2000000, 'a'), (2000001, 'b'), (1000000, 'dup')")
	if err == nil || !strings.Contains(err.Error(), "Error 1041") {
		t.Fatalf("the failed statement: %v", err)
	}
	st.AllocCheck = checkAlloc
	// トランザクションは取り消されていて、COMMIT しても何も残らない
	mustExec(t, mainDB, "COMMIT")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM d"); got != "1" {
		t.Errorf("rows after the refused discard: %s", got)
	}
	mustExec(t, mainDB, "INSERT INTO d VALUES (1, 'next')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM d"); got != "2" {
		t.Errorf("rows after the next statement: %s", got)
	}
}
