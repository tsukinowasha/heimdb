package store

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// oldNext は、段階 102 までの Scan.Next（写した値を返し、返す前にカーソルを進める）。NextView と比べる。
func oldNext(sc *Scan) (entry, value []byte, ok bool, err error) {
	if sc.err != nil {
		return nil, nil, false, sc.err
	}
	if sc.closed {
		return nil, nil, false, nil
	}
	for {
		best := -1
		for i, c := range sc.cursors {
			if c.err != nil {
				sc.err = c.err
				return nil, nil, false, c.err
			}
			if !c.valid {
				continue
			}
			if best < 0 {
				best = i
				continue
			}
			cmp := bytes.Compare(c.entry, sc.cursors[best].entry)
			if (!sc.reverse && cmp < 0) || (sc.reverse && cmp > 0) {
				best = i
			}
		}
		if best < 0 {
			return nil, nil, false, nil
		}
		e, v := sc.cursors[best].entry, sc.cursors[best].value
		buf := make([]byte, len(e)+len(v))
		entry, value := buf[:copy(buf, e):len(e)], buf[len(e):]
		copy(value, v)
		for _, c := range sc.cursors {
			if c.valid && bytes.Equal(c.entry, entry) {
				c.advance()
			}
		}
		if value[0] == valTombstone {
			continue
		}
		return entry, value[1:], true, nil
	}
}

// NextView（写さずに返し、返したエントリのカーソルを次に呼ばれたときに進める）は、前の Next と同じ行を同じ順に
// 返す（docs/plan.md の段階 103）。ブランチの層の重ね合わせ（上の層が下の層と同じエントリを持つ、上の層の削除の印）、
// 同じエントリの複数のバージョン、トランザクションの中の未コミットの変更、順方向と逆順、読んでいる途中のランダムな位置での
// Reset。逆順は、同じ区間の順方向を逆にしたものとも同じ。
func TestStage103NextView(t *testing.T) {
	s, _ := openTemp(t)
	rnd := rand.New(rand.NewSource(103))
	write := func(branch uint64) {
		for range 4 { // コミットを分けて、同じエントリのバージョンを重ねる
			writeOn(t, s, branch, func(tx *Txn) {
				for range 25 {
					n := rnd.Int63n(50)
					if rnd.Intn(4) == 0 {
						bdel(t, tx, branch, n)
					} else {
						bput(t, tx, branch, n, fmt.Sprint(branch, "-", rnd.Intn(1000), "-", string(make([]byte, rnd.Intn(40)))))
					}
				}
			})
		}
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
	row := func(entry, value []byte) string {
		n, _, _ := keyenc.DecodeInt64(entry)
		return fmt.Sprintf("%d=%q", n, value)
	}
	bound := func() []byte {
		if rnd.Intn(4) == 0 {
			return nil
		}
		return pk(rnd.Int63n(60))
	}
	resets := 0
	for _, branch := range []uint64{MainBranchID, b1.ID, b2.ID} {
		tx := s.Begin()
		bput(t, tx, branch, 7, "uncommitted")
		bdel(t, tx, branch, rnd.Int63n(50))
		for range 60 {
			lo, hi, reverse := bound(), bound(), rnd.Intn(2) == 0
			a, err := tx.RowScan(branch, tbl, lo, hi, reverse)
			if err != nil {
				t.Fatal(err)
			}
			b, err := tx.RowScan(branch, tbl, lo, hi, reverse)
			if err != nil {
				t.Fatal(err)
			}
			var got, want []string
			for step := 0; ; step++ {
				if rnd.Intn(8) == 0 { // 読んでいる途中で、両方の区間を同じところに移す
					lo, hi = bound(), bound()
					a.Reset(lo, hi)
					b.Reset(lo, hi)
					resets++
				}
				ae, av, aok, err := oldNext(a)
				if err != nil {
					t.Fatal(err)
				}
				be, bv, bok, err := b.NextView()
				if err != nil {
					t.Fatal(err)
				}
				if aok != bok {
					t.Fatalf("branch %d reverse=%v step %d: old ok %v, view ok %v", branch, reverse, step, aok, bok)
				}
				if !aok {
					break
				}
				want, got = append(want, row(ae, av)), append(got, row(be, bv))
			}
			if !slices.Equal(got, want) {
				t.Fatalf("branch %d reverse=%v:\n view %v\n  old %v", branch, reverse, got, want)
			}
			a.Close()
			b.Close()
			// 逆順は、同じ区間の順方向を逆にしたもの
			lo, hi = bound(), bound()
			var fwd, rev []string
			for _, r := range []bool{false, true} {
				sc, err := tx.RowScan(branch, tbl, lo, hi, r)
				if err != nil {
					t.Fatal(err)
				}
				for {
					e, v, ok, err := sc.NextView()
					if err != nil {
						t.Fatal(err)
					}
					if !ok {
						break
					}
					if r {
						rev = append(rev, row(e, v))
					} else {
						fwd = append(fwd, row(e, v))
					}
				}
				sc.Close()
			}
			slices.Reverse(fwd)
			if !slices.Equal(fwd, rev) {
				t.Fatalf("branch %d [%x, %x): reverse %v, forward reversed %v", branch, lo, hi, rev, fwd)
			}
		}
		tx.Rollback()
	}
	if resets < 100 {
		t.Errorf("only %d resets", resets)
	}
}
