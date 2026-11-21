package backend

import (
	"context"
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
	time.Sleep(300 * time.Millisecond) // INSERT が値を取り、ロックを待つまで

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
