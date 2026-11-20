package store

import "testing"

// 親を時刻 ts に戻すと、子のマージの基準は ts 以前で最新の記録（なければ分岐時刻）に戻る。GC は、境界より後の記録と、
// 境界以前で最新の記録を残す（docs/plan.md の段階 44）。
func TestDropMergeBasesAfter(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateBranch("other", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	merge := func(child uint64, n int64) uint64 {
		return writeOn(t, s, MainBranchID, func(tx *Txn) {
			if err := tx.SetMergeBase(child, s.LastCommitted()); err != nil {
				t.Fatal(err)
			}
			bput(t, tx, MainBranchID, n, "merged")
		})
	}
	write := func(n int64) uint64 {
		return writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, n, "w") })
	}
	parentTS := func(child uint64) uint64 {
		t.Helper()
		base, err := s.MergeBase(child)
		if err != nil {
			t.Fatal(err)
		}
		return base.ParentTS
	}
	drop := func(ts uint64) {
		t.Helper()
		tx := s.Begin()
		if err := tx.DropMergeBasesAfter(MainBranchID, ts); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	m1 := merge(dev.ID, 1)
	w1 := write(2)
	m2 := merge(dev.ID, 3)
	w2 := write(4)
	m3 := merge(dev.ID, 5)
	o1 := merge(other.ID, 6)

	// 境界 w1 の GC は、m3、m2（境界より後）と m1（境界以前で最新）を残す。
	stats, err := s.gcUntil(w1)
	if err != nil || stats.MergeRecords != 0 {
		t.Fatalf("GC at w1: %+v, %v", stats, err)
	}
	if parentTS(dev.ID) != m3 || parentTS(other.ID) != o1 {
		t.Fatalf("merge bases: %d, %d", parentTS(dev.ID), parentTS(other.ID))
	}
	drop(w2) // m3 と、別の子の o1 を消す
	if got := parentTS(dev.ID); got != m2 {
		t.Errorf("after dropping past w2: %d, want m2 %d", got, m2)
	}
	if got := parentTS(other.ID); got != other.ForkedAt {
		t.Errorf("other branch's merge after w2 should be dropped too: %d, want fork %d", got, other.ForkedAt)
	}
	drop(w1) // m2 を消す
	if got := parentTS(dev.ID); got != m1 {
		t.Errorf("after dropping past w1: %d, want m1 %d", got, m1)
	}
	drop(m1) // m1 は ts 以前なので残る
	if got := parentTS(dev.ID); got != m1 {
		t.Errorf("record at ts was dropped: %d", got)
	}
	drop(m1 - 1) // すべて消え、分岐時刻に戻る
	if got := parentTS(dev.ID); got != dev.ForkedAt {
		t.Errorf("after dropping all: %d, want fork %d", got, dev.ForkedAt)
	}

	// 記録が増えた後の GC は、境界以前で最新の 1 つだけを残す。
	merge(dev.ID, 7) // m4
	w3 := write(8)
	m5 := merge(dev.ID, 9)
	merge(dev.ID, 10)
	stats, err = s.gcUntil(w3)
	if err != nil || stats.MergeRecords != 0 {
		t.Fatalf("GC at w3: %+v, %v", stats, err)
	}
	stats, err = s.gcUntil(m5)
	if err != nil || stats.MergeRecords != 1 { // m4 を消し、m5 と最新を残す
		t.Fatalf("GC at m5: %+v, %v", stats, err)
	}
	drop(m5)
	if got := parentTS(dev.ID); got != m5 {
		t.Errorf("after GC and drop: %d, want m5 %d", got, m5)
	}
}
