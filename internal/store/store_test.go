package store

import (
	"slices"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/schema"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func tableNames(t *testing.T, s *Store, asOf uint64) []string {
	t.Helper()
	tables, err := s.ListTables(MainBranchID, asOf)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tbl := range tables {
		names = append(names, tbl.Name)
	}
	return names
}

// put は新しいテーブルを作り、そのコミット時刻とテーブル ID を返す。
func put(t *testing.T, s *Store, name string) (ts, id uint64) {
	t.Helper()
	err := s.Update(func(tx *WriteTx) error {
		var err error
		if id, err = tx.NextTableID(); err != nil {
			return err
		}
		ts = tx.TS
		return tx.PutTable(MainBranchID, &schema.Table{ID: id, Name: name})
	})
	if err != nil {
		t.Fatal(err)
	}
	return ts, id
}

func TestCatalogVersions(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()

	tsA, idA := put(t, s, "wp_a")
	tsB, idB := put(t, s, "wp_b")
	if idA == idB {
		t.Fatalf("table IDs must differ: %d %d", idA, idB)
	}

	// wp_a を wp_c に名前変更する。
	var tsRename uint64
	err := s.Update(func(tx *WriteTx) error {
		tsRename = tx.TS
		if err := tx.DeleteTable(MainBranchID, "wp_a"); err != nil {
			return err
		}
		return tx.PutTable(MainBranchID, &schema.Table{ID: idA, Name: "wp_c"})
	})
	if err != nil {
		t.Fatal(err)
	}

	// wp_b を削除する。
	var tsDrop uint64
	err = s.Update(func(tx *WriteTx) error {
		tsDrop = tx.TS
		return tx.DeleteTable(MainBranchID, "wp_b")
	})
	if err != nil {
		t.Fatal(err)
	}

	if !(tsA < tsB && tsB < tsRename && tsRename < tsDrop) {
		t.Fatalf("timestamps must increase: %d %d %d %d", tsA, tsB, tsRename, tsDrop)
	}

	cases := []struct {
		asOf uint64
		want []string
	}{
		{tsA - 1, nil},
		{tsA, []string{"wp_a"}},
		{tsB, []string{"wp_a", "wp_b"}},
		{tsRename, []string{"wp_b", "wp_c"}},
		{tsDrop, []string{"wp_c"}},
		{Latest, []string{"wp_c"}},
	}
	for _, c := range cases {
		if got := tableNames(t, s, c.asOf); !slices.Equal(got, c.want) {
			t.Errorf("ListTables(asOf=%d) = %v, want %v", c.asOf, got, c.want)
		}
	}

	tbl, ok, err := s.GetTable(MainBranchID, "WP_C", Latest)
	if err != nil || !ok || tbl.ID != idA {
		t.Fatalf("GetTable(WP_C) = %+v, %v, %v; want ID %d", tbl, ok, err, idA)
	}
	if _, ok, _ := s.GetTable(MainBranchID, "wp_a", Latest); ok {
		t.Error("wp_a should not exist after rename")
	}
	if tbl, ok, _ := s.GetTable(MainBranchID, "wp_a", tsB); !ok || tbl.ID != idA {
		t.Errorf("wp_a should exist as of %d", tsB)
	}

	// 削除したテーブルと同名で作り直すと、新しい ID になる。
	_, idB2 := put(t, s, "wp_b")
	if idB2 == idB {
		t.Errorf("recreated table must get a new ID, got %d again", idB2)
	}
}

func TestReopenKeepsClockAndSequences(t *testing.T) {
	s, dir := openTemp(t)
	// 時計が巻き戻っても、時刻は単調に増える。
	future := time.Now().Add(time.Hour)
	s.now = func() time.Time { return future }
	ts1, id1 := put(t, s, "wp_a")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts2, id2 := put(t, s, "wp_b")
	if ts2 <= ts1 {
		t.Errorf("timestamp went backwards after reopen: %d then %d", ts1, ts2)
	}
	if id2 <= id1 {
		t.Errorf("table ID reused after reopen: %d then %d", id1, id2)
	}
	if got := tableNames(t, s, Latest); !slices.Equal(got, []string{"wp_a", "wp_b"}) {
		t.Errorf("tables after reopen = %v", got)
	}
}

func TestFailedUpdateWritesNothing(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	err := s.Update(func(tx *WriteTx) error {
		if err := tx.PutTable(MainBranchID, &schema.Table{ID: 99, Name: "wp_x"}); err != nil {
			return err
		}
		return errTest
	})
	if err != errTest {
		t.Fatalf("Update error = %v", err)
	}
	if got := tableNames(t, s, Latest); len(got) != 0 {
		t.Errorf("tables = %v, want none", got)
	}
}

type testError struct{}

func (testError) Error() string { return "test error" }

var errTest error = testError{}
