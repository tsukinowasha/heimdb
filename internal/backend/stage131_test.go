package backend

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 書き込みのロックが長く持たれている間（別のファイルシステムへのバックアップなど）に、AUTO_INCREMENT の値を取った
// INSERT がロックを待っていても、SELECT は待たない（docs/stages/131.md）。
func TestSelectDoesNotWaitBehindInsert(t *testing.T) {
	var st *store.Store
	mainDB, _, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	ctx := context.Background()
	b, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// バックアップの代わりに、何も書かないコミットの中で書き込みのロックを持ったまま止まる。
	held, release := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- st.Update(func(*store.WriteTx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	bDone := make(chan error, 1)
	go func() {
		_, err := b.ExecContext(ctx, "INSERT INTO wp_posts (post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES ('second', 'c', '', '', '', '')")
		bDone <- err
	}()
	// INSERT が値を取り、書き込みのロックを待つまで待つ（待つ前に SELECT を流すと、直す前のコードでも通ってしまう）。
	for deadline := time.Now().Add(10 * time.Second); !waitingForWriteLock(); {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("the INSERT did not reach the write lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cDone := make(chan error, 1)
	go func() {
		var n int
		cDone <- c.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_posts").Scan(&n)
	}()
	select {
	case err := <-cDone:
		if err != nil {
			t.Errorf("SELECT: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("SELECT waited for the write lock behind the INSERT")
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Error(err)
	}
	if err := <-bDone; err != nil {
		t.Errorf("INSERT: %v", err)
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_posts").Scan(&n); err != nil || n != 2 {
		t.Errorf("posts = %d, %v; want 2", n, err)
	}
}

// waitingForWriteLock は、Txn.Commit の中で書き込みのロックを待っている goroutine があるかを返す。
func waitingForWriteLock() bool {
	buf := make([]byte, 1<<22)
	for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "sync.(*Mutex).Lock") && strings.Contains(g, "store.(*Store).updateWaits") &&
			strings.Contains(g, "store.(*Txn).Commit") {
			return true
		}
	}
	return false
}
