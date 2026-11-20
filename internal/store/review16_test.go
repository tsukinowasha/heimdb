package store

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 段階 16 のレビュー: GC（書き込みのロックの外で、削除をいくつものバッチに分けて書く）が動いている最中に取った
// Checkpoint が開けて、元の AS OF <返した時刻> と同じ内容で、GC の境界が返した時刻を超えていないこと。
func TestReview16CheckpointDuringGC(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	defineTable(t, s, MainBranchID, tbl, "t")
	// 子ブランチも作る（GC が祖先の層に残すべき時点を使う経路を通す）。
	child, err := s.CreateBranch("child", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(16))
	var mu sync.Mutex
	var times []uint64
	write := func() {
		br := MainBranchID
		if rng.Intn(3) == 0 {
			br = child.ID
		}
		ts := writeOn(t, s, br, func(tx *Txn) {
			for j := 0; j <= rng.Intn(3); j++ {
				k := int64(1 + rng.Intn(8))
				if rng.Intn(4) == 0 {
					bdel(t, tx, br, k)
					if err := tx.DeleteIndexEntry(br, tbl, 1, pk(k)); err != nil {
						t.Fatal(err)
					}
				} else {
					bput(t, tx, br, k, fmt.Sprintf("v%d", rng.Int()))
					if err := tx.PutIndexEntry(br, tbl, 1, pk(k), nil); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
		mu.Lock()
		times = append(times, ts)
		mu.Unlock()
	}
	for i := 0; i < 400; i++ {
		write()
	}

	// GC を、境界を少しずつ進めながら繰り返し動かす。
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			mu.Lock()
			i = min(i+5, len(times)-1)
			target := times[i]
			mu.Unlock()
			if _, err := s.gcUntil(target); err != nil {
				t.Error(err)
				return
			}
		}
	}()

	type ck struct {
		dir string
		ts  uint64
	}
	var cks []ck
	base := t.TempDir()
	for i := 0; i < 8; i++ {
		for j := 0; j < 20; j++ {
			write()
		}
		dir := filepath.Join(base, fmt.Sprint("ck", i))
		ts, err := s.Checkpoint(dir)
		if err != nil {
			t.Fatal(err)
		}
		if ts != s.LastCommitted() {
			t.Errorf("checkpoint %d: returned %d, last committed %d", i, ts, s.LastCommitted())
		}
		cks = append(cks, ck{dir, ts})
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	for _, c := range cks {
		b, err := Open(c.dir)
		if err != nil {
			t.Fatalf("open %s: %v", c.dir, err)
		}
		if b.LastCommitted() != c.ts {
			t.Errorf("%s: last committed %d, want %d", c.dir, b.LastCommitted(), c.ts)
		}
		if b.GCBoundary() > c.ts {
			t.Errorf("%s: GC boundary %d is after the backup time %d", c.dir, b.GCBoundary(), c.ts)
		}
		for _, br := range []uint64{MainBranchID, child.ID} {
			// 元の AS OF <バックアップの時刻> は、GC の境界がそれを超えていなければ読める。
			if err := s.CheckReadable(c.ts); err == nil {
				if got, want := readAt(t, b, br, tbl, Latest)+"|"+indexAt(t, b, br, Latest), readAt(t, s, br, tbl, c.ts)+"|"+indexAt(t, s, br, c.ts); got != want {
					t.Errorf("%s branch %d: %q, want %q", c.dir, br, got, want)
				}
			}
			// バックアップの中でも、その境界以降の時刻は読める。
			if got, want := readAt(t, b, br, tbl, b.GCBoundary()), readAt(t, b, br, tbl, b.GCBoundary()); got != want {
				t.Errorf("%s: %q != %q", c.dir, got, want)
			}
		}
		// バックアップにも書ける。
		writeOn(t, b, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "after") })
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}
}

// DeleteBranch は 2 回のコミット（登録と基準の削除、データの削除）なので、その間に取った Checkpoint は、登録のない
// ブランチのデータを含む。開くときに dropUnregisteredBranches が片付け、ブランチは見えない。
func TestReview16CheckpointBetweenBranchDeleteCommits(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	defineTable(t, s, MainBranchID, tbl, "t")
	info, err := s.CreateBranch("gone", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, info.ID, func(tx *Txn) { bput(t, tx, info.ID, 1, "x") })
	// DeleteBranch の 1 回目のコミットと同じ状態にする。
	if err := s.Update(func(tx *WriteTx) error { return tx.batch.Delete(branchMetaKey(info.ID), nil) }); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "ck")
	if _, err := s.Checkpoint(dir); err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()
	if _, ok := b.BranchByName("gone"); ok {
		t.Error("half-deleted branch is listed in the backup")
	}
	if n := countKeys(t, b, branchPrefix(info.ID)); n != 0 {
		t.Errorf("%d keys of the half-deleted branch remain in the backup", n)
	}
	if n := countKeys(t, s, branchPrefix(info.ID)); n == 0 {
		t.Error("the source lost the branch data (hard links are shared!)")
	}
}

// 2 回目の Checkpoint は、同じディレクトリでは明示的なエラー（1 回目を消さない）。
func TestReview16CheckpointExistingDir(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	dir := filepath.Join(t.TempDir(), "ck")
	if _, err := s.Checkpoint(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(dir); err == nil {
		t.Fatal("second checkpoint into the same directory succeeded")
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatalf("the first checkpoint was damaged by the second attempt: %v", err)
	}
	b.Close()
}
