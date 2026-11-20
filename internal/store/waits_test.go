package store

import (
	"testing"
	"time"
)

// トランザクションの Waits に、書き込みロックを待った時間と、同期のレプリカの知らせを待った時間が入る
// （docs/plan.md の段階 81）。
func TestTxnWaits(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetCommitWait(func(uint64) { time.Sleep(50 * time.Millisecond) })

	holder := s.Begin()
	if err := holder.LockForWrite(MainBranchID); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = holder.Rollback()
	}()

	var w Waits
	tx := s.Begin()
	tx.Waits = &w
	if err := tx.LockForWrite(MainBranchID); err != nil {
		t.Fatal(err)
	}
	if err := tx.PutRow(MainBranchID, 1, []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if w.Lock < 80*time.Millisecond || w.Lock > 2*time.Second {
		t.Errorf("lock wait = %v", w.Lock)
	}
	if w.Ack < 50*time.Millisecond || w.Ack > 2*time.Second {
		t.Errorf("ack wait = %v", w.Ack)
	}
}
