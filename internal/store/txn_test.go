package store

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

const tbl = 7

func pk(n int64) []byte { return keyenc.AppendInt64(nil, n) }

// rows は、tx から見える行を「主キー=値」の文字列で返す。
func rows(t *testing.T, tx *Txn, reverse bool) []string {
	t.Helper()
	var out []string
	err := tx.ScanRows(MainBranchID, tbl, nil, nil, reverse, func(r RowVersion) (bool, error) {
		n, _, err := keyenc.DecodeInt64(r.PK)
		if err != nil {
			return false, err
		}
		out = append(out, fmt.Sprintf("%d=%s", n, r.Value))
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustPut(t *testing.T, tx *Txn, n int64, v string) {
	t.Helper()
	if err := tx.PutRow(MainBranchID, tbl, pk(n), []byte(v)); err != nil {
		t.Fatal(err)
	}
}

func expectRows(t *testing.T, tx *Txn, want ...string) {
	t.Helper()
	if got := rows(t, tx, false); !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	rev := slices.Clone(want)
	slices.Reverse(rev)
	if got := rows(t, tx, true); !slices.Equal(got, rev) {
		t.Errorf("reverse rows = %v, want %v", got, rev)
	}
}

func TestTxnVisibilityAndCommit(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()

	a := s.Begin()
	mustPut(t, a, 1, "a1")
	mustPut(t, a, 2, "a2")
	mustPut(t, a, 1, "a1b") // 同じトランザクション内の上書き
	expectRows(t, a, "1=a1b", "2=a2")

	b := s.Begin()
	expectRows(t, b) // 未コミットの変更は他から見えない
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	expectRows(t, b) // b のスナップショットは最初の読み取りで決まったので、a のコミットは見えない
	if err := b.Rollback(); err != nil {
		t.Fatal(err)
	}

	c := s.Begin()
	mustPut(t, c, 2, "c2")
	mustPut(t, c, 0, "c0")
	if err := c.DeleteRow(MainBranchID, tbl, pk(1)); err != nil {
		t.Fatal(err)
	}
	expectRows(t, c, "0=c0", "2=c2")
	if err := c.Rollback(); err != nil {
		t.Fatal(err)
	}

	d := s.Begin()
	defer d.Rollback()
	expectRows(t, d, "1=a1b", "2=a2")
	if v, ok, err := d.GetRowLatest(MainBranchID, tbl, pk(2)); err != nil || !ok || string(v) != "a2" {
		t.Errorf("GetRow(2) = %q, %v, %v", v, ok, err)
	}
	if _, ok, _ := d.GetRowLatest(MainBranchID, tbl, pk(3)); ok {
		t.Error("GetRow(3) should not find a row")
	}
}

func TestTxnVersionsAcrossCommits(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	for i, v := range []string{"v1", "v2", "v3"} {
		tx := s.Begin()
		mustPut(t, tx, 5, v)
		mustPut(t, tx, int64(10+i), "x")
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	tx := s.Begin()
	if err := tx.DeleteRow(MainBranchID, tbl, pk(11)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = s.Begin()
	defer tx.Rollback()
	expectRows(t, tx, "5=v3", "10=x", "12=x")

	// 範囲の指定
	var got []string
	err := tx.ScanRows(MainBranchID, tbl, pk(6), pk(12), false, func(r RowVersion) (bool, error) {
		n, _, _ := keyenc.DecodeInt64(r.PK)
		got = append(got, fmt.Sprint(n))
		return true, nil
	})
	if err != nil || !slices.Equal(got, []string{"10"}) {
		t.Errorf("range scan = %v, %v", got, err)
	}
}

func TestTxnDiscardStatement(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	tx.StatementBegin()
	mustPut(t, tx, 1, "ok")
	tx.StatementComplete()

	tx.StatementBegin()
	tx.StatementBegin() // 同じ文の 2 つ目のテーブルエディタ
	mustPut(t, tx, 1, "bad")
	mustPut(t, tx, 2, "bad")
	tx.StatementComplete()
	if err := tx.DiscardStatement(); err != nil {
		t.Fatal(err)
	}
	expectRows(t, tx, "1=ok")

	tx.StatementBegin()
	mustPut(t, tx, 3, "ok")
	tx.StatementComplete()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = s.Begin()
	defer tx.Rollback()
	expectRows(t, tx, "1=ok", "3=ok")
}

func TestTxnDoneErrors(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.PutRow(MainBranchID, tbl, pk(1), nil); err != ErrTxnDone {
		t.Errorf("PutRow after commit: %v", err)
	}
	if err := tx.Commit(); err != ErrTxnDone {
		t.Errorf("second Commit: %v", err)
	}
}

func TestAutoIncrementPersists(t *testing.T) {
	s, dir := openTemp(t)
	c, err := s.AutoIncrement(MainBranchID, tbl, 1, math.MaxUint64, func() (uint64, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Next(); v != 1 {
		t.Errorf("first Next = %d", v)
	}
	c.Observe(10)
	// ロールバックしても値は戻らない。
	tx := s.Begin()
	if v, _ := c.Next(); v != 11 {
		t.Errorf("Next after Observe(10) = %d", v)
	}
	tx.Rollback()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err = s.AutoIncrement(MainBranchID, tbl, 1, math.MaxUint64, func() (uint64, error) { return 5, nil })
	if err != nil {
		t.Fatal(err)
	}
	if v := c.Peek(); v != 12 {
		t.Errorf("Peek after reopen = %d, want 12", v)
	}
	c2, _ := s.AutoIncrement(MainBranchID, tbl+1, 1, math.MaxUint64, func() (uint64, error) { return 100, nil })
	if v, _ := c2.Next(); v != 100 {
		t.Errorf("counter initialized from rows = %d, want 100", v)
	}
}

// asOfRows は、時刻 asOf の時点で確定していた行を返す。
func asOfRows(t *testing.T, s *Store, asOf uint64, reverse bool) []string {
	t.Helper()
	var out []string
	err := s.ScanRowsAsOf(MainBranchID, tbl, asOf, nil, nil, reverse, func(r RowVersion) (bool, error) {
		n, _, err := keyenc.DecodeInt64(r.PK)
		if err != nil {
			return false, err
		}
		out = append(out, fmt.Sprintf("%d=%s", n, r.Value))
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanRowsAsOf(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()

	// commit は fn を 1 つのトランザクションで実行し、そのコミット時刻を返す。
	commit := func(fn func(tx *Txn)) uint64 {
		t.Helper()
		tx := s.Begin()
		fn(tx)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return s.lastTS
	}
	t1 := commit(func(tx *Txn) { mustPut(t, tx, 1, "v1"); mustPut(t, tx, 2, "w1") })
	t2 := commit(func(tx *Txn) { mustPut(t, tx, 1, "v2"); mustPut(t, tx, 3, "x1") })
	t3 := commit(func(tx *Txn) {
		if err := tx.DeleteRow(MainBranchID, tbl, pk(2)); err != nil {
			t.Fatal(err)
		}
	})
	// 未コミットの変更は過去の読み取りにも、最新の読み取り（Latest）にも出ない。
	pending := s.Begin()
	mustPut(t, pending, 1, "uncommitted")
	defer pending.Rollback()

	cases := []struct {
		asOf uint64
		want []string
	}{
		{t1 - 1, nil},
		{t1, []string{"1=v1", "2=w1"}},
		{t2 - 1, []string{"1=v1", "2=w1"}},
		{t2, []string{"1=v2", "2=w1", "3=x1"}},
		{t3, []string{"1=v2", "3=x1"}},
		{Latest, []string{"1=v2", "3=x1"}},
	}
	for _, c := range cases {
		if got := asOfRows(t, s, c.asOf, false); !slices.Equal(got, c.want) {
			t.Errorf("asOf %d: rows = %v, want %v", c.asOf, got, c.want)
		}
		rev := slices.Clone(c.want)
		slices.Reverse(rev)
		if got := asOfRows(t, s, c.asOf, true); !slices.Equal(got, rev) {
			t.Errorf("asOf %d: reverse rows = %v, want %v", c.asOf, got, rev)
		}
	}
}

func TestTxnWriteLock(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()

	a := s.Begin()
	mustPut(t, a, 1, "a")
	b := s.Begin()
	b.LockTimeout = 50 * time.Millisecond
	if err := b.PutRow(MainBranchID, tbl, pk(2), []byte("b")); err != ErrLockWaitTimeout {
		t.Fatalf("second writer: err = %v, want lock wait timeout", err)
	}
	// 読み取りだけならロックを待たない。
	expectRows(t, b)

	// ロックを持つトランザクションが終われば、次の書き手が取れる。
	done := make(chan error)
	c := s.Begin()
	go func() { done <- c.PutRow(MainBranchID, tbl, pk(3), []byte("c")) }()
	time.Sleep(20 * time.Millisecond)
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("writer after commit: %v", err)
	}
	if err := c.Rollback(); err != nil {
		t.Fatal(err)
	}
	// ロールバックでもロックは解放される。
	d := s.Begin()
	d.LockTimeout = 50 * time.Millisecond
	if err := d.LockForWrite(MainBranchID); err != nil {
		t.Fatalf("lock not released after rollback: %v", err)
	}
	if !d.HoldsLock(MainBranchID) {
		t.Error("HoldsLock should be true after LockForWrite")
	}
	d.Rollback()
	b.Rollback()
}

func TestTxnSnapshotAndConflict(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	setup := s.Begin()
	mustPut(t, setup, 1, "v1")
	mustPut(t, setup, 2, "w1")
	if err := setup.Commit(); err != nil {
		t.Fatal(err)
	}

	reader := s.Begin()
	expectRows(t, reader, "1=v1", "2=w1") // ここでスナップショットが決まる

	writer := s.Begin()
	mustPut(t, writer, 1, "v2")
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}

	// 同じトランザクションの中では、何度読んでも同じ結果になる。
	expectRows(t, reader, "1=v1", "2=w1")
	// 最新の読み取り（制約の検査用）は、コミット済みの変更を見る。
	if v, ok, _ := reader.GetRowLatest(MainBranchID, tbl, pk(1)); !ok || string(v) != "v2" {
		t.Errorf("GetRowLatest = %q, %v", v, ok)
	}
	// スナップショットより後に変更された行を書くと、競合になる。
	err := reader.PutRow(MainBranchID, tbl, pk(1), []byte("stale"))
	if !sql.ErrLockDeadlock.Is(err) {
		t.Errorf("write to changed row: err = %v, want deadlock error", err)
	}
	// 変更されていない行は書ける。
	if err := reader.PutRow(MainBranchID, tbl, pk(2), []byte("w2")); err != nil {
		t.Errorf("write to unchanged row: %v", err)
	}
	reader.Rollback()

	// 読む前にロックを取れば、スナップショットはロックの後に決まり、競合しない。
	late := s.Begin()
	if err := late.LockForWrite(MainBranchID); err != nil {
		t.Fatal(err)
	}
	expectRows(t, late, "1=v2", "2=w1")
	if err := late.PutRow(MainBranchID, tbl, pk(1), []byte("v3")); err != nil {
		t.Errorf("write after lock: %v", err)
	}
	if err := late.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAutoIncrementRange(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	c, err := s.AutoIncrement(MainBranchID, tbl, 100, 110, func() (uint64, error) { return 5, nil })
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Next(); v != 100 {
		t.Errorf("first value = %d, want the range start 100", v)
	}
	c.Observe(5000) // 帯の外の値は無視する
	c.Observe(3)
	if v := c.Peek(); v != 101 {
		t.Errorf("Peek after out-of-range observations = %d, want 101", v)
	}
	c.Set(1, 0) // 帯の下限より下げない
	if v := c.Peek(); v != 100 {
		t.Errorf("Peek after Set(1) = %d, want 100", v)
	}
	c.Set(105, 0)
	c.Set(110, 0) // 帯の上の値は無視する
	if v := c.Peek(); v != 105 {
		t.Errorf("Peek after Set(110) = %d, want 105", v)
	}
	c.Observe(109)
	if _, err := c.Next(); err != ErrAutoIncrementExhausted {
		t.Errorf("Next past the range: err = %v", err)
	}
}

// 文と文の間で開いたままのトランザクション（主キーで 1 行を引くイテレータや、書き込みとロックを持つ）が残っていても、
// Close は panic せずに閉じ、そのトランザクションは終わったものになる（docs/plan.md の段階 41。Pebble は、イテレータが
// 残ったまま閉じると「element has outstanding references」で panic する）。
func TestCloseRollsBackOpenTransactions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	setup := s.Begin()
	for _, k := range []string{"a", "b"} {
		if err := setup.InsertRow(MainBranchID, 1, []byte(k), []byte{valData, 0x00}); err != nil {
			t.Fatal(err)
		}
	}
	if err := setup.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Flush(); err != nil { // 行を SST にして、イテレータがファイルを開くようにする
		t.Fatal(err)
	}

	// 1 行を引いた読み手を持つトランザクション（読み取りだけ）を 2 つ。
	var readers []*Txn
	for range 2 {
		tx := s.Begin()
		r, err := tx.PointReader(MainBranchID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := r.Peek([]byte("a")); err != nil || !ok {
			t.Fatalf("peek: %v %v", ok, err)
		}
		readers = append(readers, tx)
	}
	// 書き込みとブランチの書き込みロックを持つトランザクション。
	writer := s.Begin()
	if err := writer.LockForWrite(MainBranchID); err != nil {
		t.Fatal(err)
	}
	if r, err := writer.PointReader(MainBranchID, 1); err != nil {
		t.Fatal(err)
	} else if _, _, err := r.Peek([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := writer.PutRow(MainBranchID, 1, []byte("c"), []byte{valData, 0x00}); err != nil {
		t.Fatal(err)
	}
	// 終わったトランザクションは数えない。
	done := s.Begin()
	if err := done.Rollback(); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 閉じた後は、終わったトランザクションとして扱う（触っても panic しない）。
	if err := writer.Commit(); err != ErrTxnDone {
		t.Errorf("commit after close: %v", err)
	}
	for _, tx := range append(readers, writer) {
		if err := tx.Rollback(); err != nil {
			t.Errorf("rollback after close: %v", err)
		}
		if _, err := tx.PointReader(MainBranchID, 1); err != ErrTxnDone {
			t.Errorf("point reader after close: %v", err)
		}
	}
	// コミットしていない書き込みは残らない。
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx := s.Begin()
	defer tx.Rollback()
	if _, ok, err := tx.GetRowLatest(MainBranchID, 1, []byte("c")); err != nil || ok {
		t.Errorf("uncommitted row c after reopening: %v %v", ok, err)
	}
	if _, ok, err := tx.GetRowLatest(MainBranchID, 1, []byte("a")); err != nil || !ok {
		t.Errorf("committed row a after reopening: %v %v", ok, err)
	}
}
