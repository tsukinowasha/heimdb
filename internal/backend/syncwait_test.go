package backend

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 同期のレプリカの知らせを待っている（Pebble に書いたが見せていない）コミットは、ほかの接続から、ふつうの読み取りでも
// 今や未来の時刻の AS OF でも見えない（docs/plan.md の段階 25）。
func TestPendingCommitInvisible(t *testing.T) {
	var st *store.Store
	var hold atomic.Bool
	release := make(chan struct{})
	db, _, stop := branchServer(t, func(s *store.Store) {
		st = s
		s.SetCommitWait(func(uint64) {
			if hold.Load() {
				<-release
			}
		})
	})
	defer stop()

	hold.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := db.Exec("INSERT INTO wp_options (option_name, option_value) VALUES ('pending', 'x')")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for st.Written() == st.LastCommitted() {
		if time.Now().After(deadline) {
			t.Fatal("the commit was not written")
		}
		time.Sleep(time.Millisecond)
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM wp_options WHERE option_name = 'pending'",
		"SELECT COUNT(*) FROM wp_options AS OF NOW(6) WHERE option_name = 'pending'",
		"SELECT COUNT(*) FROM wp_options AS OF '2099-01-01' WHERE option_name = 'pending'",
	} {
		if n := one(t, db, q); n != "0" {
			t.Errorf("%s: %s (the pending commit is visible)", q, n)
		}
	}
	c := conn(t, db)
	defer c.Close()
	if s := statusMap(t, c); s["pending_commit"] == "" || s["pending_commit"] == "NULL" {
		t.Errorf("status has no pending commit: %v", s)
	}
	hold.Store(false)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := one(t, db, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'pending'"); n != "1" {
		t.Errorf("after the acknowledgement: %s", n)
	}
	if s := statusMap(t, c); s["pending_commit"] != "" {
		t.Errorf("status still has a pending commit: %v", s)
	}
}
