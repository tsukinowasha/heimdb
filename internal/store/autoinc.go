package store

import (
	"encoding/binary"
	"errors"
	"sync"
)

// AutoIncrement は 1 つのテーブルの AUTO_INCREMENT のカウンタ。
//
// MySQL と同じく、カウンタはトランザクションに属さない。ロールバックしても払い出した値は戻らず、番号に欠けが出る。
// 進めた値は、次にいずれかのトランザクション（または DDL）がコミットするときに保存する。
// 段階 6 でブランチごとの採番に差し替えられるよう、払い出しはこの型に閉じ込める。
//
// カウンタは番号帯 [lo, hi) の中で動く（docs/plan.md の段階 6）。帯の外の値（他のブランチの番号帯の ID がマージで
// 入った場合など）は観測せず、帯を使い切ったら払い出しをエラーにする。
type AutoIncrement struct {
	mu     sync.Mutex
	next   uint64
	lo, hi uint64
	dirty  bool
}

// ErrAutoIncrementExhausted は、AUTO_INCREMENT の番号帯を使い切ったことを表す。
var ErrAutoIncrementExhausted = errors.New("HeimDB: AUTO_INCREMENT range is exhausted")

type autoIncKeyT struct{ branch, tableID uint64 }

// AutoIncrement は、番号帯 [lo, hi) で動くテーブルのカウンタを返す。初めて使うときは、保存した値、initial()、lo の
// 最も大きい値から始める。initial は、既存の行のうち帯の中にある AUTO_INCREMENT 列の最大値 + 1 を返す。
func (s *Store) AutoIncrement(branch, tableID, lo, hi uint64, initial func() (uint64, error)) (*AutoIncrement, error) {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	k := autoIncKeyT{branch, tableID}
	if c, ok := s.autoIncs[k]; ok {
		return c, nil
	}
	saved, err := s.getUint64(autoIncKey(branch, tableID))
	if err != nil {
		return nil, err
	}
	fromRows, err := initial()
	if err != nil {
		return nil, err
	}
	if saved >= hi {
		saved = 0 // 以前のバージョンで帯の外まで進んでいた値は使わない
	}
	c := &AutoIncrement{next: max(saved, fromRows, lo, 1), lo: lo, hi: hi}
	s.autoIncs[k] = c
	return c, nil
}

// Next は次の値を払い出す。
func (c *AutoIncrement) Next() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next >= c.hi {
		return 0, ErrAutoIncrementExhausted
	}
	v := c.next
	c.next++
	c.dirty = true
	return v, nil
}

// Peek は、次に払い出す値を返す。払い出しはしない。
func (c *AutoIncrement) Peek() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.next
}

// Observe は、明示的に指定された値 v を見て、v 以下を払い出さないようカウンタを進める。帯の外の値は無視する。
func (c *AutoIncrement) Observe(v uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v >= c.next && v < c.hi {
		c.next = v + 1
		c.dirty = true
	}
}

// Set はカウンタを v にする（ALTER TABLE ... AUTO_INCREMENT = v）。
// MySQL と同じく、既存の値より小さくする指定は、既存の最大値 + 1 までしか下げない。下限は呼び出し側が渡す。
// 帯の下限より下には下げない。帯の上の値は、Observe と同じく無視する（ほかのブランチの番号帯の値を含むダンプを
// 流したときに、カウンタを帯の上限に張り付かせて止めない）。
func (c *AutoIncrement) Set(v, floor uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v >= c.hi {
		return
	}
	c.next = min(max(v, floor, c.lo, 1), c.hi)
	c.dirty = true
}

// savedAutoIncrement は、保存しようとしたカウンタとその値。
type savedAutoIncrement struct {
	c *AutoIncrement
	v uint64
}

// writeDirtyAutoIncrements は、進んだカウンタを tx に書き、書いた値を返す。s.writeMu を持った状態で呼ぶ。
// 保存済みの印は、コミットが成功してから markAutoIncrementsSaved で付ける。
func (s *Store) writeDirtyAutoIncrements(tx *WriteTx) []savedAutoIncrement {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	var saved []savedAutoIncrement
	for k, c := range s.autoIncs {
		c.mu.Lock()
		if c.dirty {
			if err := tx.batch.Set(autoIncKey(k.branch, k.tableID), binary.BigEndian.AppendUint64(nil, c.next), nil); err != nil {
				panic(err) // Batch.Set が失敗するのは Batch を閉じた後だけ。
			}
			saved = append(saved, savedAutoIncrement{c, c.next})
		}
		c.mu.Unlock()
	}
	return saved
}

// markAutoIncrementsSaved は、コミットで保存できたカウンタに保存済みの印を付ける。
// 保存の後にさらに進んだカウンタは、次のコミットで保存するので印を付けない。
func markAutoIncrementsSaved(saved []savedAutoIncrement) {
	for _, sv := range saved {
		sv.c.mu.Lock()
		if sv.c.next == sv.v {
			sv.c.dirty = false
		}
		sv.c.mu.Unlock()
	}
}

// FlushAutoIncrements は、進んだ AUTO_INCREMENT のカウンタを保存する（止めるときに、同期のレプリカに送れるうちに）。
func (s *Store) FlushAutoIncrements() error { return s.flushAutoIncrements() }

func (s *Store) flushAutoIncrements() error {
	if s.replica || !s.autoIncrementsDirty() {
		return nil // レプリカでは書けない（払い出した値は、どの行にも使われていない）
	}
	return s.Update(func(*WriteTx) error { return nil })
}

// tryFlushAutoIncrements は flushAutoIncrements と同じだが、書き込みのロックが持たれていれば待たずに何もしない
// （書き込みのないコミットから呼ぶ。docs/stages/131.md）。ロックを持つコミット、またはその後のコミットが
// writeDirtyAutoIncrements で保存する。保存の前に落ちても、カウンタは行の最大値 + 1 から始まる。
func (s *Store) tryFlushAutoIncrements() error {
	if s.replica || !s.autoIncrementsDirty() {
		return nil
	}
	if !s.writeMu.TryLock() {
		return nil
	}
	defer s.writeMu.Unlock()
	return s.commitLocked(0, nil, func(*WriteTx) error { return nil })
}

// autoIncrementsDirty は、保存していないカウンタがあるかを返す。
func (s *Store) autoIncrementsDirty() bool {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	for _, c := range s.autoIncs {
		c.mu.Lock()
		dirty := c.dirty
		c.mu.Unlock()
		if dirty {
			return true
		}
	}
	return false
}
