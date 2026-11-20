package store

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/schema"
)

// 段階 78: 復号した定義はキャッシュするが、GetTable と ListTables は呼ぶたびに別の写しを返し、バージョン（Version）はバージョンごと。
func TestTableDefCacheCopies(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(def *schema.Table) {
		t.Helper()
		if err := s.Update(func(tx *WriteTx) error { return tx.PutTable(MainBranchID, def) }); err != nil {
			t.Fatal(err)
		}
	}
	def := &schema.Table{ID: 7, Name: "wp_a", Columns: []schema.Column{{Name: "id", Type: "bigint"}}, PKOrdinals: []int{0},
		Indexes: []schema.Index{{ID: 1, Name: "k", Columns: []schema.IndexColumn{{Name: "id"}}}}}
	put(def)
	a, ok, err := s.GetTable(MainBranchID, "wp_a", Latest)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	v1 := a.Version
	a.Name = "changed"
	a.Columns[0].Name = "changed"
	a.Indexes[0].Columns[0].Name = "changed"
	b, _, _ := s.GetTable(MainBranchID, "wp_a", Latest)
	ls, _ := s.ListTables(MainBranchID, Latest)
	for _, got := range []*schema.Table{b, ls[0]} {
		if got == a || got.Name != "wp_a" || got.Columns[0].Name != "id" || got.Indexes[0].Columns[0].Name != "id" || got.Version != v1 {
			t.Fatalf("got %+v (version %d), want the stored definition (version %d)", got, got.Version, v1)
		}
	}
	// 同じ中身を別の時刻に書いても、バージョンはその時刻
	put(def)
	c, _, _ := s.GetTable(MainBranchID, "wp_a", Latest)
	if c.Version <= v1 {
		t.Fatalf("version %d, want > %d", c.Version, v1)
	}
	if old, _, _ := s.GetTable(MainBranchID, "wp_a", v1); old.Version != v1 {
		t.Fatalf("as of %d: version %d", v1, old.Version)
	}
}
