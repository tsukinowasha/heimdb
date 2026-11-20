package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 121: ブランチの作成は親の書き込みロックを取る。分けてコミットする作り直し（裏の処理、手続き、CREATE INDEX、
// ALTER と同じ）の途中に分岐点が入ると、ブランチは水準 0 の定義のまま main の作りかけのエントリを見て、後でブランチで
// 作り直すと、ブランチで消した行の main のエントリが残った。作成は作り直しが終わるのを待ち、ブランチは作った後の
// 定義とエントリを持つ。
func TestStage121BranchDuringRebuild(t *testing.T) {
	dir := t.TempDir()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	stage121Table(t, st, db, "br", 3000)
	pro.Shutdown(time.Second)
	db.Close()
	stopServer(t, st, srv)()

	oldChunk := rewriteChunkBytes
	rewriteChunkBytes = 512
	defer func() { rewriteChunkBytes = oldChunk }()
	var mu sync.Mutex
	var addrNow string
	var once sync.Once
	created := make(chan error, 1)
	testHookIndexChunk = func(table, index string) {
		once.Do(func() {
			mu.Lock()
			a := addrNow
			mu.Unlock()
			// 作り直しの最初の分をコミットした後に、別の接続でブランチを作り始め、作り直しを少し止める
			go func() {
				c, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", a, testDB))
				if err == nil {
					_, err = c.Exec("CALL heimdb_branch_create('b1')")
					c.Close()
				}
				created <- err
			}()
			time.Sleep(300 * time.Millisecond)
		})
	}
	defer func() { testHookIndexChunk = nil }()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	addr, pro, srv = serveOn(t, st, serverConfig{rebuild: true})
	addrNow = addr
	mu.Unlock()
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	if s := waitIndexRebuild(t, pro); s.rebuilt != 2 {
		t.Errorf("rebuild %+v", s)
	}
	select {
	case err := <-created:
		if err != nil {
			t.Fatalf("branch create: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("branch create did not finish")
	}
	testHookIndexChunk = nil
	b1 := branchID(t, st, "b1")
	// 作成は kd を作り終えるのを待ち、kd と kg の間（ロックを放した間）に分かれる
	if got := keyLevels(t, st, b1, "br"); got != fmt.Sprintf("kd=%d,kg=0", rowenc.KeyLevel) {
		t.Errorf("b1 levels %s", got)
	}
	db = openRoot(t, addr)
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "USE `wordpress@b1`")
	mustExec(t, db, "DELETE FROM br WHERE id < 1500")
	mustExec(t, db, "CALL heimdb_rebuild_indexes()")
	for _, pair := range [][2]string{
		{"SELECT COUNT(*) FROM br WHERE d >= 0", "SELECT COUNT(*) FROM br WHERE d + 0 >= 0"},
		{"SELECT COUNT(*) FROM br WHERE g >= 0", "SELECT COUNT(*) FROM br WHERE g + 0 >= 0"},
		{"SELECT MIN(id) FROM br WHERE d = 0.25", "SELECT MIN(id) FROM br WHERE d + 0 = 0.25"},
	} {
		got := queryRows(t, db, pair[0], false)
		if want := queryRows(t, db, pair[1], false); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("b1: %s\ngot  %v\nwant %v", pair[0], got, want)
		}
	}
}

// ブランチを作る接続のトランザクションが親のロックを持っていれば、自分を待たずに作る（分岐点はそのトランザクションの
// コミットの前）。ほかの接続のトランザクションが持っていれば、ロックの待ちの上限で 1205 になる。
func TestStage121BranchCreateInTransaction(t *testing.T) {
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{store: []func(*store.Store){func(s *store.Store) { s.LockTimeout = 300 * time.Millisecond }}})
	defer stopServer(t, st, srv)()
	db := openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "CREATE TABLE tx (id INT PRIMARY KEY)")
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{"BEGIN", "INSERT INTO tx VALUES (1)", "CALL heimdb_branch_create('in_txn')"} {
		if _, err := c.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	other := openRoot(t, addr)
	defer other.Close()
	if _, err := other.Exec("CALL heimdb_branch_create('other')"); err == nil || !strings.Contains(err.Error(), "1205") {
		t.Errorf("branch create while another transaction holds main: %v", err)
	}
	if _, err := c.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	mustExec(t, other, "CALL heimdb_branch_create('other')")
	if got := queryRows(t, db, "SELECT COUNT(*) FROM `wordpress@in_txn`.tx", false); strings.Join(got, ",") != "0" {
		t.Errorf("in_txn rows %v", got)
	}
	if got := queryRows(t, db, "SELECT COUNT(*) FROM `wordpress@other`.tx", false); strings.Join(got, ",") != "1" {
		t.Errorf("other rows %v", got)
	}
}
