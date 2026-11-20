package store

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
)

func TestGCVictims(t *testing.T) {
	v := func(ts uint64, tomb bool) version {
		return version{key: []byte(fmt.Sprint(ts)), ts: ts, tombstone: tomb}
	}
	names := func(vs []version) string {
		var out []string
		for _, x := range vs {
			out = append(out, string(x.key))
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name     string
		versions []version // 新しい順
		b        uint64
		points   []uint64
		root     bool
		want     string
	}{
		{"keep newer than b and visible at b", []version{v(50, false), v(40, false), v(30, false), v(20, false)}, 35, nil, false, "20"},
		{"all newer", []version{v(50, false), v(40, false)}, 35, nil, false, ""},
		{"preserved point keeps the version visible then", []version{v(50, false), v(30, false), v(20, false), v(10, false)}, 45, []uint64{25}, false, "10"},
		{"point exactly at a version", []version{v(30, false), v(20, false), v(10, false)}, 35, []uint64{20}, false, "10"},
		{"point before every version keeps nothing extra", []version{v(30, false), v(20, false)}, 35, []uint64{5}, false, "20"},
		{"root drops an old tombstone with nothing below", []version{v(30, true), v(20, false)}, 35, nil, true, "30,20"},
		{"branch keeps the tombstone", []version{v(30, true), v(20, false)}, 35, nil, false, "20"},
		{"root keeps data visible at a point under a tombstone", []version{v(30, true), v(20, false)}, 35, []uint64{25}, true, ""},
		{"root keeps a tombstone newer than b", []version{v(40, true), v(20, false)}, 35, nil, true, ""},
	}
	for _, c := range cases {
		if got := names(gcVictims(c.versions, c.b, c.points, c.root)); got != c.want {
			t.Errorf("%s: victims = %q, want %q", c.name, got, c.want)
		}
	}
}

// defineTable は、ブランチにテーブル ID id の定義を書く（GC は定義から参照されないテーブルを消すため）。
func defineTable(t *testing.T, s *Store, branch, id uint64, name string) uint64 {
	t.Helper()
	var ts uint64
	if err := s.Update(func(tx *WriteTx) error {
		ts = tx.TS
		return tx.PutTable(branch, &schema.Table{ID: id, Name: name, NextIndexID: 2, Indexes: []schema.Index{{ID: 1, Name: "i"}}})
	}); err != nil {
		t.Fatal(err)
	}
	return ts
}

// readAt は、ブランチの時刻 ts の時点の行を「主キー=値」で返す。
func readAt(t *testing.T, s *Store, branch, tableID, ts uint64) string {
	t.Helper()
	var out []string
	if err := s.ScanRowsAsOf(branch, tableID, ts, nil, nil, false, func(r RowVersion) (bool, error) {
		n, _, err := keyenc.DecodeInt64(r.PK)
		out = append(out, fmt.Sprintf("%d=%s", n, r.Value))
		return true, err
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, ",")
}

// indexAt は、ブランチの時刻 ts の時点のインデックス 1 のエントリ（主キー）を返す。
func indexAt(t *testing.T, s *Store, branch, ts uint64) string {
	t.Helper()
	var out []string
	if err := s.ScanIndexAsOf(branch, tbl, 1, ts, nil, nil, false, func(e, _ []byte) (bool, error) {
		n, _, err := keyenc.DecodeInt64(e)
		out = append(out, fmt.Sprint(n))
		return true, err
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, ",")
}

func countKeys(t *testing.T, s *Store, prefix []byte) int {
	t.Helper()
	it, err := s.db.NewIter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	n := 0
	for valid := it.SeekGE(prefix); valid && strings.HasPrefix(string(it.Key()), string(prefix)); valid = it.Next() {
		n++
	}
	return n
}

func TestGCBranchesAndBoundary(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	defineTable(t, s, MainBranchID, tbl, "t")
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a1"); bput(t, tx, MainBranchID, 2, "b1") })
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a2") })
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	atFork := readAt(t, s, dev.ID, tbl, Latest)
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a3"); bdel(t, tx, MainBranchID, 2) })
	b := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a4") })
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "a5") })
	before := readAt(t, s, MainBranchID, tbl, b)
	latest := readAt(t, s, MainBranchID, tbl, Latest)

	stats, err := s.gcUntil(b)
	if err != nil {
		t.Fatal(err)
	}
	// 行 1: a5、a4（b の時点）、a2（dev の分岐時刻）を残し、a1 と a3 を消す。行 2: b1（分岐時刻）と削除の印は残る。
	if stats.Versions != 2 || stats.Boundary != b {
		t.Errorf("stats = %+v, want 2 versions at boundary %d", stats, b)
	}
	if got := readAt(t, s, dev.ID, tbl, Latest); got != atFork {
		t.Errorf("dev after GC: %s, want %s", got, atFork)
	}
	if got := readAt(t, s, MainBranchID, tbl, b); got != before {
		t.Errorf("main at the boundary: %s, want %s", got, before)
	}
	if got := readAt(t, s, MainBranchID, tbl, Latest); got != latest {
		t.Errorf("main latest: %s, want %s", got, latest)
	}
	// 境界より前は読めない。境界より前からブランチも作れない。
	if err := s.CheckReadable(b - 1); !errors.Is(err, ErrBeforeRetention) {
		t.Errorf("CheckReadable before the boundary: %v", err)
	}
	if err := s.CheckReadable(b); err != nil {
		t.Errorf("CheckReadable at the boundary: %v", err)
	}
	if _, err := s.CreateBranch("old", MainBranchID, b-1); !errors.Is(err, ErrBeforeRetention) {
		t.Errorf("branch before the boundary: %v", err)
	}
	// 境界は保存され、開き直しても残る。境界を前に戻す GC はしない。
	if _, err := s.gcUntil(b - 10); err != nil {
		t.Fatal(err)
	}
	if got := s.GCBoundary(); got != b {
		t.Errorf("boundary moved back: %d", got)
	}
	// dev を消すと、分岐時刻のバージョンも消せるようになる。行 2 は削除の印ごと消える（main には下に何もない）。
	if err := s.DeleteBranch("dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.gcUntil(b); err != nil {
		t.Fatal(err)
	}
	if got := countKeys(t, s, RowPrefix(MainBranchID, tbl)); got != 2 {
		t.Errorf("row versions left on main: %d, want 2 (a5 and a4)", got)
	}
}

// 開いているトランザクションのスナップショットより古いバージョンは消さない。
func TestGCKeepsOpenSnapshots(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	defineTable(t, s, MainBranchID, tbl, "t")
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "old") })
	reader := s.Begin()
	defer reader.Rollback()
	if got := strings.Join(rows(t, reader, false), ","); got != "1=old" {
		t.Fatalf("reader: %s", got)
	}
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "new") })
	stats, err := s.gcUntil(s.LastCommitted())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Boundary != reader.ReadTS() || stats.Versions != 0 {
		t.Errorf("stats = %+v, want the boundary held at the reader's snapshot %d", stats, reader.ReadTS())
	}
	if got := strings.Join(rows(t, reader, false), ","); got != "1=old" {
		t.Errorf("reader after GC: %s", got)
	}
}

// 定義から参照されなくなったテーブルとインデックスは範囲で消す。ブランチが分岐時刻に参照していれば残す。
func TestGCUnreferencedTablesAndIndexes(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	const gone, kept = 20, 21
	// 採番器を進める（GC は、開始時に払い出し済みのテーブル ID だけを対象にする）。
	if err := s.Update(func(tx *WriteTx) error {
		tx.setUint64(seqKey(seqTable), kept)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	defineTable(t, s, MainBranchID, gone, "gone")
	if err := s.Update(func(tx *WriteTx) error {
		return tx.PutTable(MainBranchID, &schema.Table{ID: kept, Name: "kept", NextIndexID: 3, Indexes: []schema.Index{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}})
	}); err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, MainBranchID, func(tx *Txn) {
		if err := tx.PutRow(MainBranchID, gone, pk(1), []byte("x")); err != nil {
			t.Fatal(err)
		}
		for _, iid := range []uint64{1, 2} {
			if err := tx.PutIndexEntry(MainBranchID, kept, iid, pk(1), nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// DROP TABLE gone と DROP INDEX b（インデックスのエントリは残る）。
	if err := s.Update(func(tx *WriteTx) error {
		if err := tx.DeleteTable(MainBranchID, "gone"); err != nil {
			return err
		}
		return tx.PutTable(MainBranchID, &schema.Table{ID: kept, Name: "kept", NextIndexID: 3, Indexes: []schema.Index{{ID: 1, Name: "a"}}})
	}); err != nil {
		t.Fatal(err)
	}
	b := s.LastCommitted()
	if stats, err := s.gcUntil(b); err != nil || stats.Tables != 0 || stats.Indexes != 0 {
		t.Fatalf("with dev referencing the old definitions: %+v, %v", stats, err)
	}
	if err := s.DeleteBranch("dev"); err != nil {
		t.Fatal(err)
	}
	_ = dev
	stats, err := s.gcUntil(b)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Tables != 1 || stats.Indexes != 1 {
		t.Errorf("stats = %+v, want 1 table and 1 index", stats)
	}
	if n := countKeys(t, s, tablePrefix(MainBranchID, gone)); n != 0 {
		t.Errorf("keys of the dropped table: %d", n)
	}
	if n := countKeys(t, s, IndexPrefix(MainBranchID, kept, 2)); n != 0 {
		t.Errorf("entries of the dropped index: %d", n)
	}
	if n := countKeys(t, s, IndexPrefix(MainBranchID, kept, 1)); n != 1 {
		t.Errorf("entries of the live index: %d", n)
	}
	if n := countKeys(t, s, catalogNamePrefix(MainBranchID, "gone")); n != 0 {
		t.Errorf("catalog versions of the dropped table: %d", n)
	}
}

// プロパティテスト: ランダムな書き込み、ブランチ、マージの基準の後で GC を動かしても、境界以降の任意の時刻と、
// 残すべき時点の読み取りは GC の前と同じ。
func TestGCPreservesReads(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			s, _ := openTemp(t)
			defer s.Close()
			defineTable(t, s, MainBranchID, tbl, "t")
			branches := []uint64{MainBranchID}
			times := []uint64{s.LastCommitted()}
			for i := 0; i < 120; i++ {
				switch r := rng.Intn(20); {
				case r == 0 && len(branches) < 4:
					parent := branches[rng.Intn(len(branches))]
					at := uint64(0)
					if rng.Intn(2) == 0 {
						at = times[rng.Intn(len(times))] // 過去の時点から分岐
					}
					info, err := s.CreateBranch(fmt.Sprintf("b%d", i), parent, at)
					if err != nil {
						t.Fatal(err)
					}
					branches = append(branches, info.ID)
				case r == 1 && len(branches) > 1:
					child := branches[1+rng.Intn(len(branches)-1)]
					info := s.branches[child]
					times = append(times, writeOn(t, s, info.Parent, func(tx *Txn) {
						if err := tx.SetMergeBase(child, times[rng.Intn(len(times))]); err != nil {
							t.Fatal(err)
						}
					}))
				default:
					br := branches[rng.Intn(len(branches))]
					times = append(times, writeOn(t, s, br, func(tx *Txn) {
						for j := 0; j <= rng.Intn(3); j++ {
							k := int64(1 + rng.Intn(5))
							if rng.Intn(4) == 0 {
								bdel(t, tx, br, k)
								if err := tx.DeleteIndexEntry(br, tbl, 1, pk(k)); err != nil {
									t.Fatal(err)
								}
							} else {
								bput(t, tx, br, k, fmt.Sprintf("v%d", i))
								if err := tx.PutIndexEntry(br, tbl, 1, pk(k), nil); err != nil {
									t.Fatal(err)
								}
							}
						}
					}))
				}
			}
			b := times[len(times)/2+rng.Intn(len(times)/2)]
			points, _ := s.preservedPoints(b)
			check := map[uint64][]uint64{}
			before := map[string]string{}
			for _, br := range branches {
				for _, ts := range append(slices.Clone(times), Latest) {
					if ts >= b {
						check[br] = append(check[br], ts)
					}
				}
				check[br] = append(check[br], points[br]...)
				for _, ts := range check[br] {
					before[fmt.Sprint(br, "@", ts)] = readAt(t, s, br, tbl, ts) + "|" + indexAt(t, s, br, ts)
				}
			}
			stats, err := s.gcUntil(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, br := range branches {
				for _, ts := range check[br] {
					if got, want := readAt(t, s, br, tbl, ts)+"|"+indexAt(t, s, br, ts), before[fmt.Sprint(br, "@", ts)]; got != want {
						t.Fatalf("branch %d at %d (boundary %d, removed %d): %q, want %q", br, ts, b, stats.Versions, got, want)
					}
				}
			}
			// もう一度動かしても何も消えない。
			if again, err := s.gcUntil(b); err != nil || again.Versions != 0 {
				t.Errorf("second GC: %+v, %v", again, err)
			}
		})
	}
}
