package store

import (
	"time"

	"github.com/dolthub/vitess/go/mysql"
)

// ErrLockWaitTimeout は、書き込みロックを待つ時間が上限を超えたことを表す（MySQL のエラー 1205）。
var ErrLockWaitTimeout = mysql.NewSQLError(mysql.ERLockWaitTimeout, mysql.SSUnknownSQLState, "Lock wait timeout exceeded; try restarting transaction")

// ErrShuttingDown は、停止が始まったのでロックを待つのをやめたことを表す（MySQL のエラー 1053）。
var ErrShuttingDown = mysql.NewSQLError(1053, "08S01", "Server shutdown in progress")

// StopLockWaits は、書き込みロックを待っている文と、これから待つ文をエラー（ErrShuttingDown）にする（docs/plan.md の
// 段階 18）。停止のとき、次の文を拒否された接続のトランザクションがロックを持ったまま残るので、それを待つ文が
// 終わらずに停止が進まなくなるのを防ぐ。
func (s *Store) StopLockWaits() {
	s.stopWaitOnce.Do(func() { close(s.stopWait) })
}

// branchLock は、ブランチの書き込みロック。容量 1 のチャネルで、送れたら取得、受け取ったら解放。
// 書き込むトランザクションはブランチごとに一度に 1 つだけにする（docs/plan.md の段階 5）。
func (s *Store) branchLock(branch uint64) chan struct{} {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	l, ok := s.locks[branch]
	if !ok {
		l = make(chan struct{}, 1)
		s.locks[branch] = l
	}
	return l
}

// LockOwner は、書き込みロックを持つもの（docs/plan.md の段階 82）。Conn は接続 ID（0 なら接続でない）、Op は内部の操作の
// 名前（マージ、復元、DDL など。接続の文なら空）。
type LockOwner struct {
	Conn uint32
	Op   string
}

// LockHolder は、ブランチの書き込みロックの持ち主と、取った時刻。
type LockHolder struct {
	Branch uint64
	Owner  LockOwner
	Since  time.Time
}

// ErrLockWaitCanceled は、書き込みロックを待っている文が止められた（KILL、実行時間の上限）ことを表す。
var ErrLockWaitCanceled = mysql.NewSQLError(mysql.ERQueryInterrupted, mysql.SSUnknownSQLState, "Query execution was interrupted")

// LockBranch は、ブランチの書き込みロックを timeout まで待って取る。取れたら解放する関数を返す。
func (s *Store) LockBranch(branch uint64, timeout time.Duration) (release func(), err error) {
	return s.LockBranchFor(branch, timeout, LockOwner{}, nil)
}

// LockBranchFor は LockBranch と同じ。持ち主 owner を記録し（LockHolders）、cancel が閉じたら待つのをやめる
// （ErrLockWaitCanceled。KILL された文が、ロックを待ち切るまで返らないことがないように）。
func (s *Store) LockBranchFor(branch uint64, timeout time.Duration, owner LockOwner, cancel <-chan struct{}) (release func(), err error) {
	l := s.branchLock(branch)
	acquired := func() func() {
		s.locksMu.Lock()
		s.holders[branch] = LockHolder{Branch: branch, Owner: owner, Since: time.Now()}
		s.locksMu.Unlock()
		return func() {
			s.locksMu.Lock()
			delete(s.holders, branch)
			s.locksMu.Unlock()
			<-l
		}
	}
	// 空いていれば、停止の後でも取る（select は準備のできた道をランダムに選ぶので、先に待たずに試す）。停止の前に
	// 始まった文が、競合していないのに ErrShuttingDown にならないように。
	select {
	case l <- struct{}{}:
		return acquired(), nil
	default:
	}
	s.locksMu.Lock()
	s.waiters[branch]++
	s.locksMu.Unlock()
	defer func() {
		s.locksMu.Lock()
		if s.waiters[branch]--; s.waiters[branch] <= 0 {
			delete(s.waiters, branch)
		}
		s.locksMu.Unlock()
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case l <- struct{}{}:
		return acquired(), nil
	case <-t.C:
		return nil, ErrLockWaitTimeout
	case <-s.stopWait:
		return nil, ErrShuttingDown
	case <-cancel:
		return nil, ErrLockWaitCanceled
	}
}

// LockHolders は、書き込みロックを持っているブランチの持ち主と、ブランチごとの待っている数を返す。
func (s *Store) LockHolders() ([]LockHolder, map[uint64]int) {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	hs := make([]LockHolder, 0, len(s.holders))
	for _, h := range s.holders {
		hs = append(hs, h)
	}
	ws := make(map[uint64]int, len(s.waiters))
	for b, n := range s.waiters {
		ws[b] = n
	}
	return hs, ws
}
