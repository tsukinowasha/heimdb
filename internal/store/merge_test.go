package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

// マージから外す表のパターンは、足した順に残り、バックアップとレプリカに付いていく。レプリカでは変えられない
// （docs/plan.md の段階 60）。
func TestMergeExcludesFollowBackupAndReplica(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	add := func(s *Store, pat string) error {
		return s.UpdateMergeExcludes(func(cur []string) ([]string, error) { return append(cur, pat), nil })
	}
	if got, err := p.MergeExcludes(); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	if err := add(p, "wp_yoast_%"); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	if err := add(p, "wp_redirection_404"); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, _ := r.MergeExcludes(); !slices.Equal(got, []string{"wp_yoast_%"}) {
		t.Errorf("backup: %v", got)
	}
	catchUp(t, p, r)
	if got, _ := r.MergeExcludes(); !slices.Equal(got, []string{"wp_yoast_%", "wp_redirection_404"}) {
		t.Errorf("replica: %v", got)
	}
	if err := add(r, "x"); !errors.Is(err, ErrReadOnlyReplica) {
		t.Errorf("replica write: %v", err)
	}
	// 空にすると、キーを消す（空の一覧と同じに読める）。
	if err := p.UpdateMergeExcludes(func([]string) ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := p.MergeExcludes(); err != nil || got != nil {
		t.Errorf("cleared: %v %v", got, err)
	}
	catchUp(t, p, r)
	if got, err := r.MergeExcludes(); err != nil || got != nil {
		t.Errorf("cleared on the replica: %v %v", got, err)
	}
}
