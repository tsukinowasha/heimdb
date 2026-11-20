package store

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// scanAllRows は、走査 sc の残りを「主キー=値」で返す。
func scanAllRows(t *testing.T, sc *Scan) []string {
	t.Helper()
	var out []string
	for {
		entry, value, ok, err := sc.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return out
		}
		n, _, _ := keyenc.DecodeInt64(entry)
		out = append(out, fmt.Sprintf("%d=%s", n, value))
	}
}

// 走査は、開いた時点のトランザクションの変更までを見る。開いた後に同じトランザクションが書いた行、消した行は、
// 区間を移しても見えない（同じ文が書いた行を読まない。docs/plan.md の段階 55）。前の変更は見える。
func TestRowScanIgnoresLaterWrites(t *testing.T) {
	s, _ := openTemp(t)
	writeOn(t, s, MainBranchID, func(tx *Txn) {
		for i := int64(1); i <= 4; i++ {
			bput(t, tx, MainBranchID, i, fmt.Sprint("c", i))
		}
	})
	tx := s.Begin()
	defer tx.Rollback()
	bput(t, tx, MainBranchID, 5, "u5") // 走査より前の自分の変更
	bdel(t, tx, MainBranchID, 1)
	sc, err := tx.RowScan(MainBranchID, tbl, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	first, _, _, err := sc.Next()
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := keyenc.DecodeInt64(first); n != 2 {
		t.Fatalf("first row %d, want 2", n)
	}
	// 走査の途中で書く（UPDATE と同じ）
	bput(t, tx, MainBranchID, 3, "changed")
	bdel(t, tx, MainBranchID, 4)
	bput(t, tx, MainBranchID, 6, "u6")
	mark := tx.Mark()
	bput(t, tx, MainBranchID, 7, "u7")
	if err := tx.RollbackTo(mark); err != nil { // 文の取り消しはバッチを作り直す
		t.Fatal(err)
	}
	if got, want := scanAllRows(t, sc), []string{"3=c3", "4=c4", "5=u5"}; !slices.Equal(got, want) {
		t.Fatalf("rest of scan = %v, want %v", got, want)
	}
	sc.Reset(pk(1), pk(5))
	if got, want := scanAllRows(t, sc), []string{"2=c2", "3=c3", "4=c4"}; !slices.Equal(got, want) {
		t.Fatalf("after Reset = %v, want %v", got, want)
	}
	sc.Reset(pk(4), nil)
	if got, want := scanAllRows(t, sc), []string{"4=c4", "5=u5"}; !slices.Equal(got, want) {
		t.Fatalf("after Reset to the end = %v, want %v", got, want)
	}
	// 新しく開いた走査は、今の変更を見る
	sc2, err := tx.RowScan(MainBranchID, tbl, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := scanAllRows(t, sc2), []string{"2=c2", "3=changed", "5=u5", "6=u6"}; !slices.Equal(got, want) {
		t.Fatalf("new scan = %v, want %v", got, want)
	}
	// 閉じずに終えた走査は、トランザクションの終わりに閉じる（閉じないと Store.Close が panic する）
}

// 走査は、ScanRows と同じ行を同じ順に返す（ブランチの層の重ね合わせ、削除の印、逆順、区間、区間の移し替え）。
func TestScanMatchesScanRows(t *testing.T) {
	s, _ := openTemp(t)
	rnd := rand.New(rand.NewSource(55))
	write := func(branch uint64) {
		writeOn(t, s, branch, func(tx *Txn) {
			for range 40 {
				n := rnd.Int63n(60)
				if rnd.Intn(4) == 0 {
					bdel(t, tx, branch, n)
				} else {
					bput(t, tx, branch, n, fmt.Sprint(branch, "-", rnd.Intn(1000)))
				}
			}
		})
	}
	write(MainBranchID)
	b1, err := s.CreateBranch("b1", MainBranchID, s.LastCommitted())
	if err != nil {
		t.Fatal(err)
	}
	write(MainBranchID)
	write(b1.ID)
	b2, err := s.CreateBranch("b2", b1.ID, s.LastCommitted())
	if err != nil {
		t.Fatal(err)
	}
	write(b1.ID)
	write(b2.ID)
	for _, branch := range []uint64{MainBranchID, b1.ID, b2.ID} {
		tx := s.Begin()
		bput(t, tx, branch, 100, "uncommitted")
		bdel(t, tx, branch, rnd.Int63n(60))
		for range 30 {
			lo, hi := pk(rnd.Int63n(70)), pk(rnd.Int63n(70))
			if rnd.Intn(4) == 0 {
				lo = nil
			}
			if rnd.Intn(4) == 0 {
				hi = nil
			}
			reverse := rnd.Intn(2) == 0
			var want []string
			if err := tx.ScanRows(branch, tbl, lo, hi, reverse, func(r RowVersion) (bool, error) {
				n, _, _ := keyenc.DecodeInt64(r.PK)
				want = append(want, fmt.Sprintf("%d=%s", n, r.Value))
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			// 別の区間で開いてから、目的の区間に移す
			sc, err := tx.RowScan(branch, tbl, pk(rnd.Int63n(70)), nil, reverse)
			if err != nil {
				t.Fatal(err)
			}
			scanAllRows(t, sc)
			sc.Reset(lo, hi)
			got := scanAllRows(t, sc)
			if !slices.Equal(got, want) {
				t.Fatalf("branch %d [%x, %x) reverse=%v: scan %v, ScanRows %v", branch, lo, hi, reverse, got, want)
			}
			sc.Close()
		}
		asOf := s.LastCommitted()
		var want []string
		if err := s.ScanRowsAsOf(branch, tbl, asOf, nil, nil, false, func(r RowVersion) (bool, error) {
			n, _, _ := keyenc.DecodeInt64(r.PK)
			want = append(want, fmt.Sprintf("%d=%s", n, r.Value))
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
		sc, err := s.RowScanAsOf(branch, tbl, asOf, nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := scanAllRows(t, sc); !slices.Equal(got, want) {
			t.Fatalf("AS OF: scan %v, ScanRowsAsOf %v", got, want)
		}
		sc.Close()
		tx.Rollback()
	}
}

// 閉じていない走査は、トランザクションの終わりに閉じる（Store.Close が開いたイテレータで panic しない）。
func TestOpenScanClosedWithTransaction(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a") })
	tx := s.Begin()
	if _, err := tx.RowScan(MainBranchID, tbl, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.IndexScan(MainBranchID, tbl, 1, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx2 := s.Begin()
	if _, err := tx2.RowScan(MainBranchID, tbl, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // 開いたままのトランザクションはロールバックされる
		t.Fatal(err)
	}
}

// CommitCheck がエラーを返すと、コミットをやめて何も書かない（メモリの上限。docs/plan.md の段階 55）。
func TestCommitCheckStopsCommit(t *testing.T) {
	s, _ := openTemp(t)
	calls := 0
	s.CommitCheck = func() error {
		calls++
		return fmt.Errorf("out of memory")
	}
	tx := s.Begin()
	for i := int64(0); i < 3000; i++ {
		bput(t, tx, MainBranchID, i, "v")
	}
	if err := tx.Commit(); err == nil || err.Error() != "out of memory" {
		t.Fatalf("commit: %v", err)
	}
	if calls != 1 {
		t.Errorf("CommitCheck called %d times", calls)
	}
	s.CommitCheck = nil
	if rows := branchRows(t, s, MainBranchID); len(rows) != 0 {
		t.Fatalf("rows after the stopped commit: %d", len(rows))
	}
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a") })
	if rows := branchRows(t, s, MainBranchID); len(rows) != 1 {
		t.Fatalf("rows after the next commit: %v", rows)
	}
}
