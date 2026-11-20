package store

import (
	"errors"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/schema"
)

// レビュー指摘 1: 復元は戻す時刻 T を登録（Pin）してから読み書きする。GC は境界を T より先に進めないので、T の定義
// （古いテーブル ID）を残し、その ID を「使われていない」として範囲で消さない。復元が GC の後にコミットしても、
// 書いた行は残る。
func TestReview9GCDoesNotDropTablePinnedByRestore(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	const oldID, newID = 5, 6
	if err := s.Update(func(tx *WriteTx) error {
		tx.setUint64(seqKey(seqTable), newID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	defineTable(t, s, MainBranchID, oldID, "t")
	tAt := writeOn(t, s, MainBranchID, func(tx *Txn) {
		if err := tx.PutRow(MainBranchID, oldID, pk(1), []byte("v1")); err != nil {
			t.Fatal(err)
		}
	})
	defineTable(t, s, MainBranchID, newID, "t") // ALTER TABLE: 新しいテーブル ID
	b := s.LastCommitted()

	release, err := s.Pin(tAt) // 復元が T を読み始める
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.gcUntil(b)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Boundary != tAt || stats.Tables != 0 {
		t.Errorf("stats = %+v, want the boundary held at %d and no table dropped", stats, tAt)
	}
	// 復元が T の定義（古いテーブル ID）と行をコミットする。
	if err := s.Update(func(tx *WriteTx) error {
		if err := tx.PutTable(MainBranchID, &schema.Table{ID: oldID, Name: "t", NextIndexID: 1}); err != nil {
			return err
		}
		return tx.PutRow(MainBranchID, oldID, pk(1), []byte("restored"))
	}); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := s.gcUntil(s.LastCommitted()); err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, s, MainBranchID, oldID, Latest); got != "1=restored" {
		t.Errorf("rows of the restored table after GC: %q", got)
	}
}

// Pin は境界より前の時刻を拒み、登録した時刻は境界を止め、解くと境界が進む。
func TestReview9Pin(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	defineTable(t, s, MainBranchID, tbl, "t")
	t1 := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a") })
	t2 := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "b") })
	release, err := s.Pin(t1)
	if err != nil {
		t.Fatal(err)
	}
	if stats, _ := s.gcUntil(t2); stats.Boundary != t1 {
		t.Errorf("boundary with a pin at %d: %d", t1, stats.Boundary)
	}
	release()
	release() // 2 回呼んでもよい
	if stats, _ := s.gcUntil(t2); stats.Boundary != t2 {
		t.Errorf("boundary after release: %d, want %d", stats.Boundary, t2)
	}
	if _, err := s.Pin(t1); !errors.Is(err, ErrBeforeRetention) {
		t.Errorf("pin before the boundary: %v", err)
	}
}

// 境界は m/gc に保存され、開き直しても残る。閉じた後の GC は panic せずエラーを返す。終わったトランザクションの
// ReadTS は境界を止めない。
func TestReview9BoundarySurvivesReopen(t *testing.T) {
	s, dir := openTemp(t)
	defineTable(t, s, MainBranchID, tbl, "t")
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a") })
	done := s.Begin()
	_ = done.Rollback()
	b := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "b") })
	done.ReadTS()
	if stats, err := s.gcUntil(b); err != nil || stats.Boundary != b {
		t.Fatalf("GC: %+v, %v (a finished transaction must not hold the boundary)", stats, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.gcUntil(b); err == nil {
		t.Error("GC after Close should fail")
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.GCBoundary(); got != b {
		t.Errorf("boundary after reopen: %d, want %d", got, b)
	}
	if err := s2.CheckReadable(b - 1); !errors.Is(err, ErrBeforeRetention) {
		t.Errorf("CheckReadable after reopen: %v", err)
	}
}
