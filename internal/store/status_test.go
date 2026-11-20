package store

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// ブランチの大きさは、そのブランチに書いた分だけ増え、ほかのブランチには数えない。最後の GC が記録される
// （docs/plan.md の段階 19）。
func TestBranchDiskUsageAndStatus(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	child, err := s.CreateBranch("child", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx := s.Begin()
	if err := tx.LockForWrite(child.ID); err != nil {
		t.Fatal(err)
	}
	value := append([]byte{valData}, bytes.Repeat([]byte("x"), 1000)...)
	for i := 0; i < 2000; i++ {
		if err := tx.PutRow(child.ID, 10, []byte(fmt.Sprintf("pk%06d", i)), value); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Flush(); err != nil { // 推定はディスク上のファイルだけを数える
		t.Fatal(err)
	}
	c, err := s.BranchDiskUsage(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.BranchDiskUsage(MainBranchID)
	if err != nil {
		t.Fatal(err)
	}
	if c < 10000 || m >= c { // 同じ文字の繰り返しは圧縮される
		t.Errorf("disk usage: child %d, main %d", c, m)
	}
	if st := s.Status(); st.DiskBytes < c || st.LastGC != nil {
		t.Errorf("status before GC: %+v", st)
	}
	if _, err := s.GC(time.Hour); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.LastGC == nil || st.LastGC.Err != nil || st.LastGC.At.IsZero() {
		t.Errorf("status after GC: %+v", st.LastGC)
	}
}

// 指標の口が読む値は、バックアップ（書き込みのロックを持つ）の間も待たずに返る（docs/plan.md の段階 73）。
func TestStatusDuringBackup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Status()
		for _, b := range s.Branches() {
			if _, err := s.BranchDiskUsage(b.ID); err != nil {
				t.Error(err)
			}
		}
		s.LastCommitted()
		s.Written()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the status waited for the write lock")
	}
}
