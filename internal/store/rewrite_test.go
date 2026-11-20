package store

import (
	"path/filepath"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/schema"
)

// 書き直しの途中の印の付いたテーブル ID は、どのテーブル定義も指さなくても GC が消さない。印を外すと消す（docs/plan.md の
// 段階 53）。
func TestGCKeepsRewritingTable(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	const id = 5
	if err := s.Update(func(tx *WriteTx) error {
		tx.setUint64(seqKey(seqTable), id)
		if err := tx.MarkRewrite(MainBranchID, id); err != nil {
			return err
		}
		if err := tx.PutRow(MainBranchID, id, pk(1), []byte("x")); err != nil {
			return err
		}
		return tx.PutIndexEntry(MainBranchID, id, 1, pk(1), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if stats, err := s.gcUntil(s.LastCommitted()); err != nil || stats.Tables != 0 {
		t.Fatalf("gc while rewriting: %+v, %v", stats, err)
	}
	if n := countKeys(t, s, tablePrefix(MainBranchID, id)); n != 2 {
		t.Fatalf("keys of the rewriting table after gc: %d, want 2", n)
	}
	if err := s.Update(func(tx *WriteTx) error { return tx.UnmarkRewrite(MainBranchID, id) }); err != nil {
		t.Fatal(err)
	}
	if stats, err := s.gcUntil(s.LastCommitted()); err != nil || stats.Tables != 1 {
		t.Fatalf("gc after unmarking: %+v, %v", stats, err)
	}
	if n := countKeys(t, s, tablePrefix(MainBranchID, id)); n != 0 {
		t.Errorf("keys of the unreferenced table: %d", n)
	}
}

// DropRewrite は、書きかけのデータと印を消す。本番の起動時には、残った書きかけ（プロセスが途中で落ちた）を消す。
// ほかのテーブルのデータは消さない。
func TestDropAndCleanupRewrites(t *testing.T) {
	s, dir := openTemp(t)
	write := func(id uint64) {
		t.Helper()
		if err := s.Update(func(tx *WriteTx) error {
			if err := tx.MarkRewrite(MainBranchID, id); err != nil {
				return err
			}
			return tx.PutRow(MainBranchID, id, pk(1), []byte("x"))
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(7)
	write(8)
	defineTable(t, s, MainBranchID, 9, "live")
	writeOn(t, s, MainBranchID, func(tx *Txn) {
		if err := tx.PutRow(MainBranchID, 9, pk(1), []byte("y")); err != nil {
			t.Fatal(err)
		}
	})
	if err := s.Update(func(tx *WriteTx) error { return tx.DropRewrite(MainBranchID, 7) }); err != nil {
		t.Fatal(err)
	}
	if n := countKeys(t, s, tablePrefix(MainBranchID, 7)); n != 0 {
		t.Errorf("keys of the dropped rewrite: %d", n)
	}
	if got, err := s.rewrites(); err != nil || len(got) != 1 || !got[rewriteID{MainBranchID, 8, 0}] {
		t.Fatalf("rewrites after drop: %v, %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.rewrites(); err != nil || len(got) != 0 {
		t.Errorf("rewrites after restart: %v, %v", got, err)
	}
	if n := countKeys(t, s, tablePrefix(MainBranchID, 8)); n != 0 {
		t.Errorf("keys of the unfinished rewrite after restart: %d", n)
	}
	if n := countKeys(t, s, tablePrefix(MainBranchID, 9)); n != 1 {
		t.Errorf("keys of the live table after restart: %d, want 1", n)
	}
}

// PendingRewrites は、書き直しの途中の印の数（テストで、失敗した ALTER が片付いたかを確かめる）。
func TestPendingRewrites(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	if err := s.Update(func(tx *WriteTx) error { return tx.MarkRewrite(MainBranchID, 3) }); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PendingRewrites(); err != nil || n != 1 {
		t.Errorf("PendingRewrites = %d, %v", n, err)
	}
}

// 書きかけの印はレプリカに写り、レプリカの GC も書きかけを消さない。本番が書き直しを終えると（印を外してテーブル定義を
// 書く）、レプリカも同じになる。レプリカは起動しても書きかけを消さない（本番が書き直しの途中かもしれない）。
func TestReplicaKeepsRewritingTable(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	const id = 4
	if err := p.Update(func(tx *WriteTx) error {
		tx.setUint64(seqKey(seqTable), id)
		if err := tx.MarkRewrite(MainBranchID, id); err != nil {
			return err
		}
		return tx.PutRow(MainBranchID, id, pk(1), []byte("x"))
	}); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	catchUp(t, p, r)
	if stats, err := r.gcUntil(r.LastCommitted()); err != nil || stats.Tables != 0 {
		t.Fatalf("gc on the replica while rewriting: %+v, %v", stats, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if r, err = OpenWith(backup, Options{Replica: true}); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n := countKeys(t, r, tablePrefix(MainBranchID, id)); n != 1 {
		t.Fatalf("keys of the rewriting table on the replica: %d, want 1", n)
	}
	if err := p.Update(func(tx *WriteTx) error {
		if err := tx.UnmarkRewrite(MainBranchID, id); err != nil {
			return err
		}
		return tx.PutTable(MainBranchID, &schema.Table{ID: id, Name: "done"})
	}); err != nil {
		t.Fatal(err)
	}
	catchUp(t, p, r)
	if got, err := r.rewrites(); err != nil || len(got) != 0 {
		t.Errorf("rewrites on the replica after finishing: %v, %v", got, err)
	}
	if stats, err := r.gcUntil(r.LastCommitted()); err != nil || stats.Tables != 0 {
		t.Errorf("gc on the replica after finishing: %+v, %v", stats, err)
	}
	if n := countKeys(t, r, tablePrefix(MainBranchID, id)); n != 1 {
		t.Errorf("keys of the finished table on the replica: %d, want 1", n)
	}
}

// 子ブランチの CREATE INDEX の途中（エントリはあるが定義にまだない）に GC が走っても、作成の途中の印があれば
// エントリを消さない。親の定義の次のインデックス ID が子より進んでいて、その ID がどの定義にもないときでも
// （段階 53 のレビュー）。起動時には、印の付いた作りかけのエントリを消す。
func TestGCKeepsIndexBeingBuilt(t *testing.T) {
	s, dir := openTemp(t)
	const id = 5
	if err := s.Update(func(tx *WriteTx) error {
		tx.setUint64(seqKey(seqTable), id)
		return tx.PutTable(MainBranchID, &schema.Table{ID: id, Name: "t", NextIndexID: 5, Indexes: []schema.Index{{ID: 1, Name: "i"}}})
	}); err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateBranch("child", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 親はその後インデックス 5 を作って消した（次のインデックス ID は 7、定義に 5 はない）。
	if err := s.Update(func(tx *WriteTx) error {
		return tx.PutTable(MainBranchID, &schema.Table{ID: id, Name: "t", NextIndexID: 7, Indexes: []schema.Index{{ID: 1, Name: "i"}}})
	}); err != nil {
		t.Fatal(err)
	}
	// 子の CREATE INDEX は ID 5 を払い出し、最初の単位をコミットした（定義はまだ）。
	if err := s.Update(func(tx *WriteTx) error {
		if err := tx.MarkIndexBuild(child.ID, id, 5); err != nil {
			return err
		}
		return tx.PutIndexEntry(child.ID, id, 5, pk(1), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if stats, err := s.gcUntil(s.LastCommitted()); err != nil || stats.Indexes != 0 {
		t.Fatalf("gc while building: %+v, %v", stats, err)
	}
	if n := countKeys(t, s, IndexPrefix(child.ID, id, 5)); n != 1 {
		t.Fatalf("entries of the index being built after gc: %d, want 1", n)
	}
	// 落ちた後の起動で、作りかけのエントリと印を消す。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := countKeys(t, s, IndexPrefix(child.ID, id, 5)); n != 0 {
		t.Errorf("entries of the unfinished index after restart: %d", n)
	}
	if got, err := s.rewrites(); err != nil || len(got) != 0 {
		t.Errorf("marks after restart: %v, %v", got, err)
	}
}
