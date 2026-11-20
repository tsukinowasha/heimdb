package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// コミットの記録とレプリカ（docs/plan.md の段階 24）。
//
// Update はコミットのバッチの中身を、同じバッチで l/<コミット時刻> に記録として書く。レプリカはその記録を同じ順に
// 適用し（ApplyRecords）、自分も同じ記録を持つ。レプリカの位置は、最後に適用した記録の時刻（= 自分の m/clock）。

// DefaultLogRetention は、コミットの記録を残す期間の既定値。
const DefaultLogRetention = 24 * time.Hour

// ErrReadOnlyReplica は、レプリカに書こうとしたことを表す。
var ErrReadOnlyReplica = errors.New("HeimDB: this server is a read-only replica")

// ErrNoLog は、コミットの記録を残していない（-replication-log-retention が 0）ことを表す。
var ErrNoLog = errors.New("HeimDB: commit records are not kept (replication log retention is 0)")

// ErrRecordGone は、求められた位置の記録がないことを表す（消えた、または違うデータの位置）。
var ErrRecordGone = errors.New("HeimDB: the commit record at this position is not in the log")

// Record は 1 つのコミットの記録。TS はコミット時刻、Data はそのコミットの Pebble のバッチの中身。
type Record struct {
	TS   uint64
	Data []byte
}

// Applied は、ApplyRecords で適用した記録が変えたもの。メモリに持っている内容の作り直しに使う。
type Applied struct {
	// Through は最後に適用した記録の時刻。
	Through uint64
	// Users はユーザーと権限（m/users）が変わったか。
	Users bool
	// Rows は、テーブル（ブランチ ID とテーブル ID）ごとの、書いた行のバージョンの数（挿入、更新、削除）。
	Rows map[TableRef]int
}

// TableRef はブランチの中のテーブル。
type TableRef struct{ Branch, Table uint64 }

func logPrefix() []byte { return []byte{tagLog} }

func logKey(ts uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{tagLog}, ts)
}

// LogChunkBytes は、コミットの記録を分けて書く大きさ（docs/plan.md の段階 59）。1 つの大きな値は、Pebble がファイルに
// 書き出すときに、丸ごとと圧縮した写しをメモリに持つ。テストで変える。
var LogChunkBytes = 1 << 20

// logChunkKey は、記録の 2 つ目からの部分のキー（l/<時刻>/<番号>。番号は 1 から）。1 つ目は logKey。
func logChunkKey(ts uint64, n uint32) []byte {
	return binary.BigEndian.AppendUint32(logKey(ts), n)
}

// decodeLogKey は、記録のキーの時刻と部分の番号（logKey なら 0）を読む。
func decodeLogKey(key []byte) (ts uint64, n uint32, err error) {
	switch len(key) {
	case 9:
		return binary.BigEndian.Uint64(key[1:]), 0, nil
	case 13:
		if n = binary.BigEndian.Uint32(key[9:]); n > 0 {
			return binary.BigEndian.Uint64(key[1:9]), n, nil
		}
	}
	return 0, 0, fmt.Errorf("HeimDB: corrupt commit record key %x", key)
}

// logChunks は、data を記録に書くときの部分の数。
func logChunks(data []byte) int {
	return max(1, (len(data)+LogChunkBytes-1)/LogChunkBytes)
}

// putLog は、時刻 ts の記録 data を、LogChunkBytes ずつに分けて b に書く。b が先に確保した大きさに収まれば、
// data（b 自身の Repr でもよい）を読む間に b の領域は変わらない。
func putLog(b *pebble.Batch, ts uint64, data []byte) error {
	for i := 0; i < logChunks(data); i++ {
		key := logKey(ts)
		if i > 0 {
			key = logChunkKey(ts, uint32(i))
		}
		part := data[i*LogChunkBytes : min(len(data), (i+1)*LogChunkBytes)]
		op := b.SetDeferred(len(key), len(part))
		copy(op.Key, key)
		copy(op.Value, part)
		if err := op.Finish(); err != nil {
			return err
		}
	}
	return nil
}

// IsReplica は、レプリカとして開いたか（書き込みはすべてエラー）。
func (s *Store) IsReplica() bool { return s.replica }

// Logging は、コミットの記録を残しているか。
func (s *Store) Logging() bool { return s.logRetention > 0 }

// OnApply は、ApplyRecords が記録を適用した後に呼ぶ関数を足す。
func (s *Store) OnApply(fn func(Applied)) {
	s.applyMu.Lock()
	s.onApply = append(s.onApply, fn)
	s.applyMu.Unlock()
}

// SetCommitWait は、コミットを Pebble に書いた後、ほかの接続に見せる前に呼ぶ関数を決める（同期のレプリカ、段階 25）。
// fn は書き込みのロックの中で呼ばれ、エラーを返さない（待ちきれなければ、そのまま見せる）。サーバを始める前に呼ぶこと。
func (s *Store) SetCommitWait(fn func(ts uint64)) { s.commitWait = fn }

// Written は、Pebble に書いた最後のコミットの時刻を返す。同期のレプリカの知らせを待つ間は LastCommitted より後。
func (s *Store) Written() uint64 { return s.written.Load() }

// notifyCommit は、コミットを待っている WaitCommit を起こす。written を進めた後に呼ぶ。
func (s *Store) notifyCommit() {
	s.notifyMu.Lock()
	close(s.notify)
	s.notify = make(chan struct{})
	s.notifyMu.Unlock()
}

// WaitCommit は、after より後のコミットを書くまで（または ctx が終わるまで）待ち、あれば true を返す。まだ見せて
// いない（知らせを待っている）コミットも含む。
func (s *Store) WaitCommit(ctx context.Context, after uint64) bool {
	for {
		s.notifyMu.Lock()
		ch := s.notify
		s.notifyMu.Unlock()
		if s.Written() > after {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// ReadRecords は、時刻 after より後の記録を古い順に、合わせて maxBytes を超えるまで（少なくとも 1 つ）返す。
// after の記録が自分の記録にない（消えた、まだない、違うデータの位置）なら ErrRecordGone。ただし after が最後に
// 消した記録の時刻（m/logtrim）なら、その後の記録はすべて残っているので続きを返す。確かめることと読むことは、同じ
// スナップショットで行う（間に TrimLog が消すと、記録を飛ばして返してしまう）。
func (s *Store) ReadRecords(after uint64, maxBytes int) ([]Record, error) {
	if !s.Logging() {
		return nil, ErrNoLog
	}
	snap := s.db.NewSnapshot()
	defer snap.Close()
	iter, err := snap.NewIter(&pebble.IterOptions{LowerBound: logKey(after), UpperBound: keyenc.PrefixEnd(logPrefix())})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	valid := iter.First()
	if valid && bytes.Equal(iter.Key(), logKey(after)) {
		// after の記録の続きの部分も飛ばす
		for valid = iter.Next(); valid && bytes.HasPrefix(iter.Key(), logKey(after)); valid = iter.Next() {
		}
	} else if err := iter.Error(); err != nil {
		return nil, err
	} else if trimmed, err := snapUint64(snap, logTrimKey()); err != nil {
		return nil, err
	} else if trimmed == 0 || trimmed != after {
		return nil, fmt.Errorf("%w: %s", ErrRecordGone, formatTS(after))
	}
	var out []Record
	size := 0
	var next uint32 // 今の記録の次の部分の番号
	for ; valid; valid = iter.Next() {
		ts, n, err := decodeLogKey(iter.Key())
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// 記録の区切りでだけ止める
			if len(out) > 0 && size >= maxBytes {
				break
			}
			out = append(out, Record{TS: ts, Data: bytes.Clone(iter.Value())})
		} else if last := len(out) - 1; last < 0 || out[last].TS != ts || n != next {
			return nil, fmt.Errorf("HeimDB: corrupt commit record %s: part %d out of order", formatTS(ts), n)
		} else {
			out[last].Data = append(out[last].Data, iter.Value()...)
		}
		next = n + 1
		size += len(iter.Value())
	}
	return out, iter.Error()
}

func snapUint64(snap *pebble.Snapshot, key []byte) (uint64, error) {
	v, closer, err := snap.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0, fmt.Errorf("HeimDB: corrupt uint64 value at %x", key)
	}
	return binary.BigEndian.Uint64(v), nil
}

// LogRange は、残っている記録の最も古い時刻と最も新しい時刻を返す（なければ 0）。
func (s *Store) LogRange() (oldest, newest uint64, err error) {
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: logPrefix(), UpperBound: keyenc.PrefixEnd(logPrefix())})
	if err != nil {
		return 0, 0, err
	}
	defer iter.Close()
	if iter.First() {
		oldest = binary.BigEndian.Uint64(iter.Key()[1:9])
	}
	if iter.Last() {
		newest = binary.BigEndian.Uint64(iter.Key()[1:9])
	}
	return oldest, newest, iter.Error()
}

// TrimLog は、保持期間を過ぎた記録を消し、消した数を返す。最新の記録は残す（書き込みのない間に、追いついた
// レプリカの位置が消えないように）。消した最後の記録の時刻を m/logtrim に残し、その位置からの続きも返せるように
// する（長く書き込みがなかった後の 1 回の書き込みで、追いついていたレプリカの位置が消えても続けられる）。
// 記録の削除は記録しない（そのマシンだけの手入れ）。
func (s *Store) TrimLog() (int, error) {
	if !s.Logging() {
		return 0, nil
	}
	// Close の後は動かさない（閉じた Pebble を触ると panic するため）。GC と同じロックを使う。
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.closed {
		return 0, errors.New("HeimDB: the store is closed")
	}
	_, newest, err := s.LogRange()
	if err != nil || newest == 0 {
		return 0, err
	}
	cut := min(uint64(max(s.now().Add(-s.logRetention).UnixMicro(), 0)), newest)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: logPrefix(), UpperBound: logKey(cut)})
	if err != nil {
		return 0, err
	}
	n := 0
	var last uint64
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) == 9 { // 続きの部分は数えない
			n++
		}
		last = binary.BigEndian.Uint64(iter.Key()[1:9])
	}
	if err := iter.Close(); err != nil || n == 0 {
		return 0, err
	}
	b := s.db.NewBatch()
	defer b.Close()
	if err := b.DeleteRange(logPrefix(), logKey(last+1), nil); err != nil {
		return 0, err
	}
	if err := b.Set(logTrimKey(), binary.BigEndian.AppendUint64(nil, last), nil); err != nil {
		return 0, err
	}
	return n, b.Commit(pebble.Sync)
}

// ApplyRecords は、本番から受け取った記録を順に適用する（レプリカだけ）。記録はそれぞれ 1 つのバッチで書き、最後だけ
// 同期書き込みにする（途中で落ちても、前から順に適用した分だけが残る）。記録の時刻は、今の位置より後で増えていること。
// 位置（LastCommitted）は、メモリの内容を作り直して適用を知らせてから進める（位置が追いついたのに、本番で作った
// ブランチやユーザーがまだ見えない間を作らない。docs/plan.md の段階 30）。位置は書き込みのロックの外で進めるので、
// 同時に呼ばないこと（呼ぶのはレプリカの受け取りのループ 1 つだけ）。記録の Data は適用に使って書き換えることがある
// ので、呼んだ後は使わないこと。
func (s *Store) ApplyRecords(recs []Record) error {
	if !s.replica {
		return errors.New("HeimDB: commit records can be applied only on a replica")
	}
	if len(recs) == 0 {
		return nil
	}
	applied := Applied{Rows: map[TableRef]int{}}
	// n は適用した記録の数、synced はそれがディスクに確定したか、through は適用した最後の記録の時刻。
	n, synced := 0, true
	var through uint64
	err := func() error {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		for i, r := range recs {
			if r.TS <= s.lastTS {
				return fmt.Errorf("HeimDB: commit record %s is not after the replica position %s", formatTS(r.TS), formatTS(s.lastTS))
			}
			// 記録は写さずにバッチの中身にする（大きな記録で、受け取った分の写しを作らない。docs/plan.md の段階 59）。
			// src を閉じると記録の頭が書き換わりうるので、記録を読むのは閉じる前に済ませ、閉じた後の r.Data は使わない。
			src := s.db.NewBatch()
			if err := src.SetRepr(r.Data); err != nil {
				src.Close()
				return fmt.Errorf("HeimDB: corrupt commit record %s: %w", formatTS(r.TS), err)
			}
			clock, err := noteChanges(src, &applied)
			if err != nil {
				src.Close()
				return fmt.Errorf("HeimDB: corrupt commit record %s: %w", formatTS(r.TS), err)
			}
			if clock != r.TS {
				src.Close()
				return fmt.Errorf("HeimDB: commit record %s has commit time %s", formatTS(r.TS), formatTS(clock))
			}
			// 書くバッチは要る大きさで先に確保する（中身と、残すなら記録。倍々に伸ばすと 4 倍近くを確保する）
			size := len(r.Data) + 64<<10
			if s.Logging() {
				size += len(r.Data) + (len(r.Data)/LogChunkBytes+1)*32
			}
			b := s.db.NewBatchWithSize(size)
			err = b.Apply(src, nil)
			if err == nil && s.Logging() {
				err = putLog(b, r.TS, r.Data)
			}
			src.Close()
			if err == nil {
				opts := pebble.NoSync
				if i == len(recs)-1 {
					opts = pebble.Sync
				}
				if testHookBeforeCommit != nil {
					testHookBeforeCommit(b, size)
				}
				n := b.Len()
				if err = b.Commit(opts); err == nil {
					s.flushLargeCommit(n)
				}
			}
			b.Close()
			if err != nil {
				return err
			}
			s.lastTS = r.TS
			s.written.Store(r.TS)
			n, synced, through = n+1, i == len(recs)-1, r.TS
		}
		return nil
	}()
	if n == 0 {
		return err
	}
	// 途中で失敗したら、それまでに同期せずに書いた分を確定させる（読み手に見せた状態を、落ちたときに失わないように）。
	if !synced {
		if serr := s.db.LogData(nil, pebble.Sync); serr != nil {
			err = errors.Join(err, serr)
		}
	}
	applied.Through = through
	if rerr := s.refreshAfterApply(); rerr != nil && err == nil {
		err = rerr
	}
	s.applyMu.Lock()
	fns := s.onApply
	s.applyMu.Unlock()
	for _, fn := range fns {
		fn(applied)
	}
	s.committed.Store(through)
	s.notifyCommit()
	return err
}

// noteChanges は、バッチ b が書いたもののうち、メモリの内容の作り直しが要るものを applied に足し、
// バッチに入っているコミット時刻（m/clock）を返す。
func noteChanges(b *pebble.Batch, applied *Applied) (clock uint64, err error) {
	r := b.Reader()
	for {
		_, key, value, ok, err := r.Next()
		if err != nil {
			return 0, err
		}
		if !ok {
			return clock, nil
		}
		switch {
		case bytes.Equal(key, clockKey()):
			if len(value) == 8 {
				clock = binary.BigEndian.Uint64(value)
			}
		case bytes.Equal(key, usersKey()):
			applied.Users = true
		case len(key) > 0 && key[0] == tagBranch:
			branch, rest, err := keyenc.DecodeUint64(key[1:])
			if err != nil || len(rest) == 0 || rest[0] != tagTable {
				continue
			}
			table, rest, err := keyenc.DecodeUint64(rest[1:])
			if err != nil || len(rest) == 0 || rest[0] != tagRow {
				continue
			}
			applied.Rows[TableRef{branch, table}]++
		}
	}
}

// refreshAfterApply は、記録の適用で古くなったメモリの内容を作り直す。ブランチの一覧を読み直し、AUTO_INCREMENT の
// カウンタを捨てる（次に使うときに保存した値から読み直す。レプリカでは払い出さないので、進めた値はない）。
func (s *Store) refreshAfterApply() error {
	s.aiMu.Lock()
	clear(s.autoIncs)
	s.aiMu.Unlock()
	return s.loadBranches()
}

func formatTS(ts uint64) string {
	return time.UnixMicro(int64(ts)).UTC().Format("2006-01-02 15:04:05.000000")
}
