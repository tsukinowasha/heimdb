package store

import (
	"slices"
	"testing"
)

// 取り込みの記録（docs/plan.md の段階 94）: 直近の出来事はマージの記録と取り込みの記録の時刻で決まる。ブランチを戻すと
// その後の取り込みの記録が消え、親を戻しても消えない。GC は、境界より後の記録と境界以前で最新の記録を残し、残した記録の
// 時点のバージョンも残す。ブランチを消すと記録も消える。
func TestSyncRecords(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	merge := func(n int64) uint64 {
		return writeOn(t, s, MainBranchID, func(tx *Txn) {
			if err := tx.SetMergeBase(dev.ID, s.LastCommitted()); err != nil {
				t.Fatal(err)
			}
			bput(t, tx, MainBranchID, n, "merged")
		})
	}
	sync := func(n int64) (at, parentTS uint64) {
		parentTS = s.LastCommitted()
		at = writeOn(t, s, dev.ID, func(tx *Txn) {
			if err := tx.RecordSync(dev.ID, parentTS); err != nil {
				t.Fatal(err)
			}
			bput(t, tx, dev.ID, n, "synced")
		})
		return at, parentTS
	}
	write := func(branch uint64, n int64) uint64 {
		return writeOn(t, s, branch, func(tx *Txn) { bput(t, tx, branch, n, "w") })
	}
	bases := func() Bases {
		t.Helper()
		b, err := s.Bases(dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	commit := func(fn func(tx *Txn) error) {
		t.Helper()
		tx := s.Begin()
		if err := fn(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	if b := bases(); b.HasSync || b.SyncLatest() || b.Key() != dev.ForkedAt {
		t.Fatalf("new branch: %+v", b)
	}
	write(MainBranchID, 1)
	s1, p1 := sync(2)
	if b := bases(); !b.SyncLatest() || b.Sync != (SyncRecord{At: s1, ParentTS: p1}) || b.Key() != p1 {
		t.Fatalf("after sync: %+v", b)
	}
	m1 := merge(3)
	if b := bases(); b.SyncLatest() || b.Key() != m1 || b.Sync.At != s1 {
		t.Fatalf("after merge: %+v", b)
	}
	w1 := write(MainBranchID, 4)
	s2, p2 := sync(5)
	if b := bases(); !b.SyncLatest() || b.Key() != p2 {
		t.Fatalf("after the second sync: %+v", b)
	}

	// 親を戻しても、取り込みの記録は消えない（マージの記録は消える）。
	commit(func(tx *Txn) error { return tx.DropMergeBasesAfter(MainBranchID, w1-1) })
	if b := bases(); !b.SyncLatest() || b.Sync.At != s2 || b.Merge.ParentTS != m1 {
		t.Fatalf("after restoring the parent past w1: %+v", b)
	}
	// ブランチを戻すと、その後の取り込みの記録が消え、直近はマージに戻る。
	commit(func(tx *Txn) error { return tx.DropSyncsAfter(dev.ID, s2-1) })
	if b := bases(); b.SyncLatest() || b.Sync.At != s1 || b.Key() != m1 {
		t.Fatalf("after restoring the branch past s2: %+v", b)
	}

	// GC: 境界より後の記録と境界以前で最新の記録を残し、残した記録の時点を残す点に足す。
	s3, p3 := sync(6)
	s4, p4 := sync(7)
	m2 := merge(8)                                                          // 直近のマージの記録は m2。境界 s3 以前で最新の m1 も残し、その時点も残す（m2 は境界より後）。
	if stats, err := s.gcUntil(s3); err != nil || stats.MergeRecords != 1 { // s1 を消し、s3 と s4 を残す
		t.Fatalf("GC at s3: %+v, %v", stats, err)
	}
	points, _ := s.preservedPoints(s3)
	for _, want := range []uint64{p3, m1} {
		if !slices.Contains(points[MainBranchID], want) {
			t.Errorf("preserved points on main %v lack %d", points[MainBranchID], want)
		}
	}
	commit(func(tx *Txn) error { return tx.DropSyncsAfter(dev.ID, s4-1) })
	if b := bases(); b.Sync != (SyncRecord{At: s3, ParentTS: p3}) || b.Merge.ParentTS != m2 {
		t.Fatalf("after GC and restoring past s4: %+v (p4 %d)", b, p4)
	}

	if err := s.DeleteBranch("dev"); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range [][]byte{syncPrefix(dev.ID), mergePrefix(dev.ID)} {
		n := 0
		if err := s.eachRetainedRecord(prefix, 0, func(uint64, []byte) error { n++; return nil }, nil); err != nil || n != 0 {
			t.Errorf("records left after deleting the branch: %d, %v", n, err)
		}
	}
}
