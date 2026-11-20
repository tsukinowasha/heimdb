package store

import (
	"fmt"
	"slices"
	"testing"
)

// readerRows は、RowReader で主キー 1..n を 1 つずつ引いた結果を「主キー=値」で返す。
func readerRows(t *testing.T, r *RowReader, n int64) []string {
	t.Helper()
	var out []string
	for i := int64(1); i <= n; i++ {
		v, ok, err := r.Get(pk(i))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			out = append(out, fmt.Sprintf("%d=%s", i, v))
		}
	}
	return out
}

func txnReaderRows(t *testing.T, tx *Txn, branch uint64, n int64) []string {
	t.Helper()
	r, err := tx.RowReader(branch, tbl)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	return readerRows(t, r, n)
}

// RowReader は、ブランチの重ね合わせ（上の層の更新と削除の印）で ScanRows と同じ行を返す。
func TestRowReaderMatchesScan(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	writeOn(t, s, MainBranchID, func(tx *Txn) {
		for i := int64(1); i <= 4; i++ {
			bput(t, tx, MainBranchID, i, fmt.Sprintf("m%d", i))
		}
	})
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 2, "m2b") })
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, dev.ID, func(tx *Txn) { bput(t, tx, dev.ID, 2, "d2"); bdel(t, tx, dev.ID, 3); bput(t, tx, dev.ID, 5, "d5") })
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "m1b"); bdel(t, tx, MainBranchID, 4) })
	feature, err := s.CreateBranch("feature", dev.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, feature.ID, func(tx *Txn) { bdel(t, tx, feature.ID, 1); bput(t, tx, feature.ID, 3, "f3") })

	for _, b := range []uint64{MainBranchID, dev.ID, feature.ID} {
		tx := s.Begin()
		got, want := txnReaderRows(t, tx, b, 6), branchRows(t, s, b)
		tx.Rollback()
		if !slices.Equal(got, want) {
			t.Errorf("branch %d: reader %v, scan %v", b, got, want)
		}
	}
}

// RowReader は、トランザクションの未コミットの変更を見て、作った後にコミットされた変更は見ない。AS OF は時点のバージョンを見る。
func TestRowReaderSnapshot(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	ts1 := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a"); bput(t, tx, MainBranchID, 2, "b") })
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a2"); bdel(t, tx, MainBranchID, 2) })

	tx := s.Begin()
	defer tx.Rollback()
	bput(t, tx, MainBranchID, 3, "c")
	bdel(t, tx, MainBranchID, 1)
	if got, want := txnReaderRows(t, tx, MainBranchID, 3), []string{"3=c"}; !slices.Equal(got, want) {
		t.Errorf("own changes: %v, want %v", got, want)
	}

	reader := s.Begin()
	defer reader.Rollback()
	r, err := reader.RowReader(MainBranchID, tbl)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, want := readerRows(t, r, 3), []string{"1=a2"}; !slices.Equal(got, want) {
		t.Errorf("before commit: %v, want %v", got, want)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, want := readerRows(t, r, 3), []string{"1=a2"}; !slices.Equal(got, want) {
		t.Errorf("after another commit: %v, want %v (snapshot)", got, want)
	}

	old, err := s.RowReaderAsOf(MainBranchID, tbl, ts1)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if got, want := readerRows(t, old, 3), []string{"1=a", "2=b"}; !slices.Equal(got, want) {
		t.Errorf("as of: %v, want %v", got, want)
	}
}

// PointReader は、トランザクションの中で使い回すが、書き込み、文の取り消し、トランザクションの終わりで作り直す
// （作ったときの変更までしか見ないので）。
func TestPointReaderRenewedOnWrite(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a") })

	tx := s.Begin()
	get := func(n int64) string {
		t.Helper()
		r, err := tx.PointReader(MainBranchID, tbl)
		if err != nil {
			t.Fatal(err)
		}
		v, ok, err := r.Get(pk(n))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return "-"
		}
		return string(v)
	}
	r1, err := tx.PointReader(MainBranchID, tbl)
	if err != nil {
		t.Fatal(err)
	}
	if r2, _ := tx.PointReader(MainBranchID, tbl); r2 != r1 {
		t.Error("PointReader was not reused")
	}
	if got := get(1) + get(2); got != "a-" {
		t.Errorf("before writes: %s", got)
	}
	bput(t, tx, MainBranchID, 2, "b")
	bdel(t, tx, MainBranchID, 1)
	if got := get(1) + get(2); got != "-b" {
		t.Errorf("after own writes: %s, want -b", got)
	}
	tx.StatementBegin()
	bput(t, tx, MainBranchID, 3, "c")
	if got := get(3); got != "c" {
		t.Errorf("in statement: %s, want c", got)
	}
	if err := tx.DiscardStatement(); err != nil {
		t.Fatal(err)
	}
	if got := get(1) + get(2) + get(3); got != "-b-" {
		t.Errorf("after discarded statement: %s, want -b-", got)
	}
	mark := tx.Mark()
	bput(t, tx, MainBranchID, 4, "d")
	if got := get(4); got != "d" {
		t.Errorf("after savepoint: %s, want d", got)
	}
	if err := tx.RollbackTo(mark); err != nil {
		t.Fatal(err)
	}
	if got := get(2) + get(4); got != "b-" {
		t.Errorf("after rollback to savepoint: %s, want b-", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.PointReader(MainBranchID, tbl); err != ErrTxnDone {
		t.Errorf("after commit: %v, want ErrTxnDone", err)
	}
	if len(tx.readers) != 0 {
		t.Errorf("%d readers left open after commit", len(tx.readers))
	}
}

func TestPointReaderRenewedOnRefreshSnapshot(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a") })

	tx := s.Begin()
	defer tx.Rollback()
	get := func() string {
		t.Helper()
		r, err := tx.PointReader(MainBranchID, tbl)
		if err != nil {
			t.Fatal(err)
		}
		v, ok, err := r.Get(pk(1))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return "-"
		}
		return string(v)
	}
	if got := get(); got != "a" {
		t.Fatalf("before: %s, want a", got)
	}
	writeOn(t, s, MainBranchID, func(w *Txn) { bput(t, w, MainBranchID, 1, "b") })
	if got := get(); got != "a" {
		t.Errorf("same snapshot: %s, want a", got)
	}
	tx.RefreshSnapshot()
	if got := get(); got != "b" {
		t.Errorf("after RefreshSnapshot: %s, want b", got)
	}
}
