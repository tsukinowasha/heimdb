package store

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// SyncRecord は、親の変更をブランチに取り込んだ記録（docs/plan.md の段階 94）。
type SyncRecord struct {
	// At は取り込みのコミット時刻（ブランチに書いた時刻）。
	At uint64
	// ParentTS は、取り込みが読んだ親のスナップショットの時刻。
	ParentTS uint64
}

// Bases は、ブランチの今の基準。直近の出来事がマージか取り込みかで、差分の基準の中身が変わる（docs/plan.md の段階 94）。
type Bases struct {
	Merge MergeBase
	// Sync は最新の取り込みの記録。HasSync が偽なら、まだ取り込んでいない。
	Sync    SyncRecord
	HasSync bool
}

// SyncLatest は、直近の出来事が取り込みかどうか。マージの記録のキー（親に書いた時刻、なければ分岐時刻）と取り込みの
// コミット時刻を比べる（時計はすべてのブランチで共通）。
func (b Bases) SyncLatest() bool {
	return b.HasSync && b.Sync.At > b.Merge.ParentTS
}

// Key は、競合の解決の記録が属する基準の時刻（Resolution.Base）。直近が取り込みなら、取り込みが読んだ親の時刻。
func (b Bases) Key() uint64 {
	if b.SyncLatest() {
		return b.Sync.ParentTS
	}
	return b.Merge.ParentTS
}

// Bases は、ブランチ branch のマージの基準と最新の取り込みの記録を返す。
func (s *Store) Bases(branch uint64) (Bases, error) {
	mb, err := s.MergeBase(branch)
	if err != nil {
		return Bases{}, err
	}
	out := Bases{Merge: mb}
	prefix := syncPrefix(branch)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return Bases{}, err
	}
	defer iter.Close()
	// 記録は新しい順に並ぶ（キーの末尾が反転した時刻）。
	if !iter.First() {
		return out, iter.Error()
	}
	if out.Sync, err = decodeSyncRecord(iter.Key()[len(prefix):], iter.Value()); err != nil {
		return Bases{}, err
	}
	out.HasSync = true
	return out, nil
}

func decodeSyncRecord(keyTS, v []byte) (SyncRecord, error) {
	at, _, err := keyenc.DecodeInvertedTime(keyTS)
	if err != nil {
		return SyncRecord{}, err
	}
	if len(v) != 9 || v[0] != valData {
		return SyncRecord{}, fmt.Errorf("HeimDB: corrupt sync record")
	}
	return SyncRecord{At: at, ParentTS: binary.BigEndian.Uint64(v[1:])}, nil
}

// RecordSync は、このトランザクションのコミットで、ブランチ branch に親の時刻 parentTS の変更を取り込んだと記録する。
// キーの時刻はコミット時刻（取り込みを書いた時刻）になる。
func (tx *Txn) RecordSync(branch, parentTS uint64) error {
	key := keyenc.AppendInvertedTime(syncPrefix(branch), uncommittedTS)
	return tx.set(key, append([]byte{valData}, binary.BigEndian.AppendUint64(nil, parentTS)...))
}

// DropSyncsAfter は、このトランザクションのコミットで、ブランチ branch の取り込みの記録のうち、取り込みの時刻が ts より
// 後のものを消す。ブランチを時刻 ts の状態に戻す（取り込みを戻す）と、写した行がブランチの変更に見えないように、一つ前の
// 基準に戻す（docs/plan.md の段階 94）。呼び出し側はブランチの書き込みロックを持つ。
func (tx *Txn) DropSyncsAfter(branch, ts uint64) error {
	if tx.done {
		return ErrTxnDone
	}
	prefix := syncPrefix(branch)
	iter, err := tx.s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return err
	}
	defer iter.Close()
	// 記録は新しい順に並ぶ。
	for valid := iter.First(); valid; valid = iter.Next() {
		at, _, err := keyenc.DecodeInvertedTime(iter.Key()[len(prefix):])
		if err != nil {
			return err
		}
		if at <= ts {
			break
		}
		if err := tx.deleteCommitted(bytes.Clone(iter.Key())); err != nil {
			return err
		}
	}
	return iter.Error()
}

// eachRetainedRecord は、記録の接頭辞 prefix（マージの基準か取り込みの記録）のうち、GC の境界 b の後も残す記録を
// fn に渡す。キーの時刻が b より後の記録と、b 以前で最新の記録を残す（ブランチや親を b より後に戻すと、その時刻より後の
// 記録を消して一つ前の記録に戻るため）。残さない記録は drop に渡す（nil なら渡さない）。
func (s *Store) eachRetainedRecord(prefix []byte, b uint64, fn func(keyTS uint64, value []byte) error, drop func(key []byte) error) error {
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return err
	}
	defer iter.Close()
	kept := false // b 以前で最新の記録を残したか
	for valid := iter.First(); valid; valid = iter.Next() {
		ts, _, err := keyenc.DecodeInvertedTime(iter.Key()[len(prefix):])
		if err != nil {
			return err
		}
		if ts > b || !kept {
			kept = kept || ts <= b
			if fn != nil {
				if err := fn(ts, iter.Value()); err != nil {
					return err
				}
			}
			continue
		}
		if drop != nil {
			if err := drop(bytes.Clone(iter.Key())); err != nil {
				return err
			}
		}
	}
	return iter.Error()
}
