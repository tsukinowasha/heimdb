package store

import (
	"path/filepath"
	"testing"
)

// ブランチの上限は、開き直しても残り、レプリカに届く。main には付けられない。大きさの見積もりが上限を超えれば
// BranchOverSize が知らせる（docs/plan.md の段階 45）。
func TestBranchLimits(t *testing.T) {
	s, dir := openTemp(t)
	stg, err := s.CreateBranch("stg", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := s.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBranchLimits("main", 1, 0); err == nil {
		t.Error("limits on main should fail")
	}
	if _, err := s.SetBranchLimits("nope", 1, 0); err != ErrBranchNotFound {
		t.Errorf("unknown branch: %v", err)
	}

	if over, _, _, err := s.BranchOverSize(stg.ID); err != nil || over {
		t.Fatalf("no limit: %v, %v", over, err)
	}
	writeRows(t, s, stg.ID, 0, 200)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBranchLimits("stg", 100, 30000); err != nil {
		t.Fatal(err)
	}
	over, usage, limit, err := s.BranchOverSize(stg.ID)
	if err != nil || !over || usage <= 100 || limit != 100 {
		t.Fatalf("over limit: %v %d %d %v", over, usage, limit, err)
	}
	// 上限を上げれば超えない（見積もりは覚えたものを使う）。
	if _, err := s.SetBranchLimits("stg", usage+1, 30000); err != nil {
		t.Fatal(err)
	}
	if over, _, _, _ := s.BranchOverSize(stg.ID); over {
		t.Error("over a raised limit")
	}
	if over, _, _, _ := s.BranchOverSize(MainBranchID); over {
		t.Error("main has no limit")
	}

	// レプリカに届く。
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	catchUp(t, s, r)
	if b, _ := r.Branch(stg.ID); b.MaxBytes != usage+1 || b.MaxExecutionMS != 30000 {
		t.Errorf("replica limits: %+v", b)
	}

	// 開き直しても残る。外せば 0。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if b, _ := s.Branch(stg.ID); b.MaxBytes != usage+1 || b.MaxExecutionMS != 30000 {
		t.Errorf("after reopen: %+v", b)
	}
	if b, err := s.SetBranchLimits("stg", 0, 0); err != nil || b.MaxBytes != 0 || b.MaxExecutionMS != 0 {
		t.Errorf("clear limits: %+v, %v", b, err)
	}
}
