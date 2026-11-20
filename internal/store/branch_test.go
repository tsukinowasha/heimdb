package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
)

// branchRows は、ブランチ branch で見える行を「主キー=値」で返す（正順と逆順が一致することも確かめる）。
func branchRows(t *testing.T, s *Store, branch uint64) []string {
	t.Helper()
	tx := s.Begin()
	defer tx.Rollback()
	read := func(reverse bool) []string {
		var out []string
		err := tx.ScanRows(branch, tbl, nil, nil, reverse, func(r RowVersion) (bool, error) {
			n, _, _ := keyenc.DecodeInt64(r.PK)
			out = append(out, fmt.Sprintf("%d=%s", n, r.Value))
			return true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	fwd, rev := read(false), read(true)
	slices.Reverse(rev)
	if !slices.Equal(fwd, rev) {
		t.Fatalf("forward %v and reverse %v differ", fwd, rev)
	}
	return fwd
}

// writeOn は branch に fn の書き込みをコミットし、コミット時刻を返す。
func writeOn(t *testing.T, s *Store, branch uint64, fn func(tx *Txn)) uint64 {
	t.Helper()
	tx := s.Begin()
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return s.LastCommitted()
}

func bput(t *testing.T, tx *Txn, branch uint64, n int64, v string) {
	t.Helper()
	if err := tx.PutRow(branch, tbl, pk(n), []byte(v)); err != nil {
		t.Fatal(err)
	}
}

func bdel(t *testing.T, tx *Txn, branch uint64, n int64) {
	t.Helper()
	if err := tx.DeleteRow(branch, tbl, pk(n)); err != nil {
		t.Fatal(err)
	}
}

func TestBranchOverlay(t *testing.T) {
	s, dir := openTemp(t)
	writeOn(t, s, MainBranchID, func(tx *Txn) {
		bput(t, tx, MainBranchID, 1, "m1")
		bput(t, tx, MainBranchID, 2, "m2")
		bput(t, tx, MainBranchID, 3, "m3")
	})

	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1=m1", "2=m2", "3=m3"}; !slices.Equal(branchRows(t, s, dev.ID), want) {
		t.Fatalf("new branch sees %v, want %v", branchRows(t, s, dev.ID), want)
	}

	// ブランチでの更新・削除・追加は main に影響しない。
	writeOn(t, s, dev.ID, func(tx *Txn) { bput(t, tx, dev.ID, 2, "d2"); bdel(t, tx, dev.ID, 3); bput(t, tx, dev.ID, 4, "d4") })
	// 分岐後の main の変更はブランチに見えない。
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "m1b"); bput(t, tx, MainBranchID, 5, "m5") })

	if got, want := branchRows(t, s, MainBranchID), []string{"1=m1b", "2=m2", "3=m3", "5=m5"}; !slices.Equal(got, want) {
		t.Errorf("main = %v, want %v", got, want)
	}
	if got, want := branchRows(t, s, dev.ID), []string{"1=m1", "2=d2", "4=d4"}; !slices.Equal(got, want) {
		t.Errorf("dev = %v, want %v", got, want)
	}

	// 孫ブランチは、dev の分岐時点と main の（dev の分岐時刻以前の）バージョンを重ねて見る。
	feature, err := s.CreateBranch("feature", dev.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, dev.ID, func(tx *Txn) { bput(t, tx, dev.ID, 4, "d4b") })
	writeOn(t, s, feature.ID, func(tx *Txn) { bdel(t, tx, feature.ID, 1) })
	if got, want := branchRows(t, s, feature.ID), []string{"2=d2", "4=d4"}; !slices.Equal(got, want) {
		t.Errorf("feature = %v, want %v", got, want)
	}

	// 子ブランチのあるブランチは消せない。
	if err := s.DeleteBranch("dev"); err != ErrBranchHasChildren {
		t.Errorf("delete dev: err = %v", err)
	}
	if err := s.DeleteBranch("feature"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.BranchByName("feature"); ok {
		t.Error("feature still listed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 再起動後もブランチと重ね合わせが保たれる。
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, want := branchRows(t, s, dev.ID), []string{"1=m1", "2=d2", "4=d4b"}; !slices.Equal(got, want) {
		t.Errorf("dev after reopen = %v, want %v", got, want)
	}
	if names := branchNames(s); !slices.Equal(names, []string{"main", "dev"}) {
		t.Errorf("branches = %v", names)
	}
}

func branchNames(s *Store) []string {
	var names []string
	for _, b := range s.Branches() {
		names = append(names, b.Name)
	}
	return names
}

func TestBranchFromPastAndAsOf(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	t1 := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "v1") })
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "v2") })

	// 過去の時点から分岐すると、その時点の状態を見る。
	old, err := s.CreateBranch("old", MainBranchID, t1)
	if err != nil {
		t.Fatal(err)
	}
	if got := branchRows(t, s, old.ID); !slices.Equal(got, []string{"1=v1"}) {
		t.Errorf("branch from past = %v", got)
	}
	tb := writeOn(t, s, old.ID, func(tx *Txn) { bput(t, tx, old.ID, 2, "b2") })
	writeOn(t, s, old.ID, func(tx *Txn) { bdel(t, tx, old.ID, 1) })

	// ブランチの AS OF も、各層に効く。
	var got []string
	err = s.ScanRowsAsOf(old.ID, tbl, tb, nil, nil, false, func(r RowVersion) (bool, error) {
		n, _, _ := keyenc.DecodeInt64(r.PK)
		got = append(got, fmt.Sprintf("%d=%s", n, r.Value))
		return true, nil
	})
	if err != nil || !slices.Equal(got, []string{"1=v1", "2=b2"}) {
		t.Errorf("AS OF on branch = %v, %v", got, err)
	}
}

func TestBranchCatalogOverlay(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	var idA uint64
	err := s.Update(func(tx *WriteTx) error {
		var err error
		if idA, err = tx.NextTableID(); err != nil {
			return err
		}
		return tx.PutTable(MainBranchID, &schema.Table{ID: idA, Name: "wp_a"})
	})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// ブランチでテーブルを消し、別のテーブルを作る。main には影響しない。
	err = s.Update(func(tx *WriteTx) error {
		if err := tx.DeleteTable(dev.ID, "wp_a"); err != nil {
			return err
		}
		return tx.PutTable(dev.ID, &schema.Table{ID: 99, Name: "wp_b"})
	})
	if err != nil {
		t.Fatal(err)
	}
	names := func(branch uint64) []string {
		ts, err := s.ListTables(branch, Latest)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, t := range ts {
			out = append(out, t.Name)
		}
		return out
	}
	if got := names(MainBranchID); !slices.Equal(got, []string{"wp_a"}) {
		t.Errorf("main tables = %v", got)
	}
	if got := names(dev.ID); !slices.Equal(got, []string{"wp_b"}) {
		t.Errorf("dev tables = %v", got)
	}
	if _, ok, _ := s.GetTable(dev.ID, "WP_A", Latest); ok {
		t.Error("dropped table visible on branch")
	}
	if tbl, ok, _ := s.GetTable(MainBranchID, "wp_a", Latest); !ok || tbl.ID != idA {
		t.Error("main lost its table")
	}
}

func TestBranchNamesAndHiddenIDs(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	for _, bad := range []string{"", "Dev", "a/b", "-x", "space name"} {
		if _, err := s.CreateBranch(bad, MainBranchID, 0); err != ErrInvalidBranchName {
			t.Errorf("CreateBranch(%q) err = %v", bad, err)
		}
	}
	if _, err := s.CreateBranch("dev", MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBranch("dev", MainBranchID, 0); err != ErrBranchExists {
		t.Errorf("duplicate branch: %v", err)
	}
	if _, err := s.CreateBranch("x", 12345, 0); err != ErrBranchNotFound {
		t.Errorf("unknown parent: %v", err)
	}
	h, err := s.NewHiddenBranch()
	if err != nil || !IsHiddenBranch(h) {
		t.Fatalf("hidden branch id %d, %v", h, err)
	}
	b, err := s.CreateBranch("next", MainBranchID, 0)
	if err != nil || b.ID != 3 {
		t.Errorf("user branch IDs must stay dense: got %d, %v", b.ID, err)
	}
}

func TestMergeBaseAndChangedRows(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "m1"); bput(t, tx, MainBranchID, 2, "m2") })
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.MergeBase(dev.ID)
	if err != nil || base.ParentTS != dev.ForkedAt || base.BranchTS != dev.ForkedAt {
		t.Fatalf("initial merge base = %+v, %v; want fork time %d", base, err, dev.ForkedAt)
	}
	t1 := writeOn(t, s, dev.ID, func(tx *Txn) { bput(t, tx, dev.ID, 2, "d2"); bput(t, tx, dev.ID, 3, "d3") })
	t2 := writeOn(t, s, dev.ID, func(tx *Txn) { bdel(t, tx, dev.ID, 3); bput(t, tx, dev.ID, 4, "d4") })

	keys := func(after, upTo uint64) []string {
		pks, err := s.ChangedRows(dev.ID, tbl, after, upTo)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, pk := range pks {
			n, _, _ := keyenc.DecodeInt64(pk)
			out = append(out, fmt.Sprint(n))
		}
		return out
	}
	if got := keys(base.BranchTS, t2); !slices.Equal(got, []string{"2", "3", "4"}) {
		t.Errorf("changed since fork = %v", got)
	}
	if got := keys(t1, t2); !slices.Equal(got, []string{"3", "4"}) {
		t.Errorf("changed since t1 = %v", got)
	}
	if got := keys(base.BranchTS, t1); !slices.Equal(got, []string{"2", "3"}) {
		t.Errorf("changed up to t1 = %v", got)
	}

	// マージの基準を記録すると、キーの時刻（コミット時刻）が親の基準、値がブランチの基準になる。
	tx := s.Begin()
	if err := tx.SetMergeBase(dev.ID, t2); err != nil {
		t.Fatal(err)
	}
	bput(t, tx, MainBranchID, 2, "merged")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	merged := s.LastCommitted()
	base, err = s.MergeBase(dev.ID)
	if err != nil || base.ParentTS != merged || base.BranchTS != t2 {
		t.Errorf("merge base after merge = %+v, %v; want {%d %d}", base, err, merged, t2)
	}
}

func TestResolutions(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.MergeBase(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	put := func(r Resolution) {
		t.Helper()
		if err := s.PutResolution(dev.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	list := func() string {
		t.Helper()
		rs, err := s.Resolutions(dev.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.Table+"/"+r.ID+"="+r.Choice)
		}
		return fmt.Sprint(out)
	}
	put(Resolution{Table: "wp_posts", ID: "ID=4", Choice: "branch", Hash: "h", Base: base.ParentTS})
	put(Resolution{Table: "*", ID: "*", Choice: "parent", Base: base.ParentTS})
	put(Resolution{Table: "wp_posts", ID: "ID=4", Choice: "parent", Hash: "h2", Base: base.ParentTS}) // 上書き
	put(Resolution{Table: "wp_terms", ID: "*", Choice: "parent", Base: base.ParentTS - 1})            // 古い基準
	if got := list(); got != "[*/*=parent wp_posts/ID=4=parent]" {
		t.Errorf("resolutions = %s", got)
	}
	put(Resolution{Table: "*", ID: "*", Choice: "none"})
	if got := list(); got != "[wp_posts/ID=4=parent]" {
		t.Errorf("after none: %s", got)
	}
	if err := s.ClearResolutions(dev.ID); err != nil {
		t.Fatal(err)
	}
	if got := list(); got != "[]" {
		t.Errorf("after clear: %s", got)
	}
	put(Resolution{Table: "wp_posts", ID: "*", Choice: "parent", Base: base.ParentTS})
	if err := s.DeleteBranch("dev"); err != nil {
		t.Fatal(err)
	}
	it, err := s.db.NewIter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	prefix := resolvePrefix(dev.ID)
	for valid := it.SeekGE(prefix); valid && strings.HasPrefix(string(it.Key()), string(prefix)); valid = it.Next() {
		t.Errorf("resolution left after the branch was deleted: %x", it.Key())
	}
}

// ChangedRowsSince は、ブランチを読むときの層ごとに、時刻 after より後で、その層が見える範囲に書かれた行を返す。
func TestChangedRowsSince(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	t0 := writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 1, "m1") })
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 2, "m2") })
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeOn(t, s, MainBranchID, func(tx *Txn) { bput(t, tx, MainBranchID, 3, "after fork") }) // dev からは見えない
	writeOn(t, s, dev.ID, func(tx *Txn) { bput(t, tx, dev.ID, 4, "d4") })
	pks, err := s.ChangedRowsSince(dev.ID, tbl, t0, s.LastCommitted())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range pks {
		n, _, _ := keyenc.DecodeInt64(k)
		got = append(got, fmt.Sprint(n))
	}
	if !slices.Equal(got, []string{"2", "4"}) {
		t.Errorf("ChangedRowsSince = %v, want [2 4]", got)
	}
}
