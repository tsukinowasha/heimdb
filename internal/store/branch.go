package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// hiddenBranchBase は、一時テーブルの隠しブランチの ID の始まり。ユーザーのブランチ ID（1 から順に払い出す）と
// 分けて、ブランチ ID を AUTO_INCREMENT の番号帯に使えるようにする。
const hiddenBranchBase uint64 = 1 << 62

const seqHidden = "hidden"

// IsHiddenBranch は、ID が一時テーブルの隠しブランチのものかを返す。
func IsHiddenBranch(id uint64) bool {
	return id >= hiddenBranchBase
}

var branchNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ErrBranchExists などは、ブランチの操作の失敗を表す。
var (
	ErrBranchExists      = errors.New("HeimDB: branch already exists")
	ErrBranchNotFound    = errors.New("HeimDB: branch not found")
	ErrBranchHasChildren = errors.New("HeimDB: branch has child branches")
	ErrInvalidBranchName = errors.New("HeimDB: invalid branch name (use lowercase letters, digits, '_' and '-')")
	ErrBranchInUse       = errors.New("HeimDB: branch is in use by a writing transaction")
)

// branchDeleteWait は、ブランチを消すときに書き込み中のトランザクションの終わりを待つ時間。
const branchDeleteWait = 2 * time.Second

// loadBranches は m/branch の一覧をメモリに読み込む。
func (s *Store) loadBranches() error {
	prefix := []byte{tagMeta, tagBranchM}
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return err
	}
	defer iter.Close()
	branches := map[uint64]BranchInfo{}
	for valid := iter.First(); valid; valid = iter.Next() {
		var info BranchInfo
		if err := json.Unmarshal(iter.Value(), &info); err != nil {
			return fmt.Errorf("HeimDB: corrupt branch info at %x: %w", iter.Key(), err)
		}
		branches[info.ID] = info
	}
	if err := iter.Error(); err != nil {
		return err
	}
	s.branchesMu.Lock()
	s.branches = branches
	s.branchesMu.Unlock()
	return nil
}

// Branches は、ブランチの一覧を ID の順に返す。
func (s *Store) Branches() []BranchInfo {
	s.branchesMu.RLock()
	defer s.branchesMu.RUnlock()
	out := make([]BranchInfo, 0, len(s.branches))
	for _, b := range s.branches {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// BranchByName は、名前でブランチを探す。
func (s *Store) BranchByName(name string) (BranchInfo, bool) {
	s.branchesMu.RLock()
	defer s.branchesMu.RUnlock()
	for _, b := range s.branches {
		if b.Name == name {
			return b, true
		}
	}
	return BranchInfo{}, false
}

// CreateBranch は、親 parent の時刻 at の状態から分岐するブランチ name を作る。at が 0 なら、今の時点（最後のコミット時刻）。
func (s *Store) CreateBranch(name string, parent uint64, at uint64) (BranchInfo, error) {
	if !branchNamePattern.MatchString(name) {
		return BranchInfo{}, ErrInvalidBranchName
	}
	s.branchOpMu.Lock()
	defer s.branchOpMu.Unlock()
	// 分岐時刻のバージョンを、ブランチの一覧に載る（GC が分岐時刻を残すべき時点として知る）まで GC に消させない。
	pinAt := at
	if pinAt == 0 || pinAt > s.LastCommitted() {
		pinAt = s.LastCommitted()
	}
	release, err := s.Pin(pinAt)
	if err != nil {
		return BranchInfo{}, err
	}
	defer release()
	var info BranchInfo
	err = s.Update(func(tx *WriteTx) error {
		s.branchesMu.RLock()
		_, parentOK := s.branches[parent]
		s.branchesMu.RUnlock()
		if !parentOK {
			return ErrBranchNotFound
		}
		if _, exists := s.BranchByName(name); exists {
			return ErrBranchExists
		}
		last := s.lastTS
		if at == 0 || at > last {
			at = last
		}
		id, err := tx.NextID(seqBranch)
		if err != nil {
			return err
		}
		info = BranchInfo{ID: id, Name: name, Parent: parent, ForkedAt: at}
		return tx.setJSON(branchMetaKey(id), info)
	})
	if err != nil {
		return BranchInfo{}, err
	}
	s.branchesMu.Lock()
	s.branches[info.ID] = info
	s.branchesMu.Unlock()
	return info, nil
}

// DeleteBranch はブランチ name とそのデータを消す。main と、子ブランチのあるブランチは消せない。
// 書き込み中のトランザクションと重ならないよう、ブランチの書き込みロックを取ってから消す。書き込み中のトランザクションが
// 短い時間で終わらなければ ErrBranchInUse を返す。
func (s *Store) DeleteBranch(name string) error {
	s.branchOpMu.Lock()
	defer s.branchOpMu.Unlock()
	info, ok := s.BranchByName(name)
	if !ok {
		return ErrBranchNotFound
	}
	if info.ID == MainBranchID {
		return fmt.Errorf("HeimDB: the main branch cannot be deleted")
	}
	for _, b := range s.Branches() {
		if b.Parent == info.ID {
			return ErrBranchHasChildren
		}
	}
	release, err := s.LockBranchFor(info.ID, min(s.LockTimeout, branchDeleteWait), LockOwner{Op: "drop branch"}, nil)
	if err == ErrLockWaitTimeout {
		return ErrBranchInUse
	} else if err != nil {
		return err
	}
	defer release()
	s.branchesMu.Lock()
	delete(s.branches, info.ID)
	s.branchesMu.Unlock()
	s.usageMu.Lock()
	delete(s.usage, info.ID)
	s.usageMu.Unlock()
	if err := s.Update(func(tx *WriteTx) error {
		for _, mp := range [][]byte{mergePrefix(info.ID), syncPrefix(info.ID), resolvePrefix(info.ID)} {
			if err := tx.batch.DeleteRange(mp, keyenc.PrefixEnd(mp), nil); err != nil {
				return err
			}
		}
		return tx.batch.Delete(branchMetaKey(info.ID), nil)
	}); err != nil {
		return err
	}
	return s.DropBranchData(info.ID)
}
