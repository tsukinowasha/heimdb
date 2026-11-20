package store

import (
	"fmt"
	"time"
)

// ブランチの上限（docs/plan.md の段階 45）。検証環境のブランチが、本番と同じ HeimDB のディスクや時間を食い尽くさない
// ようにする。上限はブランチの情報（m/branch）に入れるので、レプリカにも届く。

// usageTTL は、ブランチの大きさの見積もりを覚えておく時間。書くたびに見積もり直さない。
const usageTTL = time.Second

type usageEntry struct {
	at    time.Time
	bytes uint64
}

// SetBranchLimits は、ブランチ name の大きさの上限 maxBytes と、1 つの文の実行時間の上限 maxExecutionMS を設定する
// （0 なら上限なし）。main には実行時間の上限だけを付けられる（大きさの上限は誤って本番を止めないよう付けない。
// docs/plan.md の段階 82）。
func (s *Store) SetBranchLimits(name string, maxBytes, maxExecutionMS uint64) (BranchInfo, error) {
	s.branchOpMu.Lock()
	defer s.branchOpMu.Unlock()
	info, ok := s.BranchByName(name)
	if !ok {
		return BranchInfo{}, ErrBranchNotFound
	}
	if info.ID == MainBranchID && maxBytes != 0 {
		return BranchInfo{}, fmt.Errorf("HeimDB: a size limit cannot be set on the main branch (only an execution time limit)")
	}
	info.MaxBytes, info.MaxExecutionMS = maxBytes, maxExecutionMS
	if err := s.Update(func(tx *WriteTx) error { return tx.setJSON(branchMetaKey(info.ID), info) }); err != nil {
		return BranchInfo{}, err
	}
	s.branchesMu.Lock()
	s.branches[info.ID] = info
	s.branchesMu.Unlock()
	return info, nil
}

// Branch は、ID でブランチを探す。
func (s *Store) Branch(id uint64) (BranchInfo, bool) {
	s.branchesMu.RLock()
	defer s.branchesMu.RUnlock()
	b, ok := s.branches[id]
	return b, ok
}

// BranchOverSize は、ブランチ id に大きさの上限があり、その大きさの見積もり（BranchDiskUsage。ディスクに書き出した
// 分だけ）が上限を超えていれば、over を true にして見積もりと上限を返す。見積もりは usageTTL の間覚えておく。
func (s *Store) BranchOverSize(id uint64) (over bool, usage, limit uint64, err error) {
	b, ok := s.Branch(id)
	if !ok || b.MaxBytes == 0 {
		return false, 0, 0, nil
	}
	s.usageMu.Lock()
	e, cached := s.usage[id]
	s.usageMu.Unlock()
	if !cached || time.Since(e.at) > usageTTL {
		bytes, err := s.BranchDiskUsage(id)
		if err != nil {
			return false, 0, 0, err
		}
		e = usageEntry{at: time.Now(), bytes: bytes}
		s.usageMu.Lock()
		if s.usage == nil {
			s.usage = map[uint64]usageEntry{}
		}
		s.usage[id] = e
		s.usageMu.Unlock()
	}
	return e.bytes > b.MaxBytes, e.bytes, b.MaxBytes, nil
}

// Flush は、メモリ上の書き込みをディスクのファイルに書き出す（大きさの見積もりに入るようにする。テスト用）。
func (s *Store) Flush() error {
	return s.db.Flush()
}
