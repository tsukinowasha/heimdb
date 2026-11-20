package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
)

// MergeBase は、ブランチを親にマージするときの三方向比較の基準（docs/plan.md の段階 7）。
type MergeBase struct {
	// ParentTS は親の基準の時刻。最初は分岐時刻、マージの後はそのマージで親に書いた時刻。
	ParentTS uint64
	// BranchTS はブランチの基準の時刻。最初は分岐時刻、マージの後はそのマージが読んだブランチのスナップショットの時刻。
	BranchTS uint64
}

// MergeBase は、ブランチ branch の今のマージの基準を返す。まだマージしていなければ、両側とも分岐時刻。
func (s *Store) MergeBase(branch uint64) (MergeBase, error) {
	s.branchesMu.RLock()
	info, ok := s.branches[branch]
	s.branchesMu.RUnlock()
	if !ok || info.Parent == 0 {
		return MergeBase{}, ErrBranchNotFound
	}
	prefix := mergePrefix(branch)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return MergeBase{}, err
	}
	defer iter.Close()
	// 記録は新しい順に並ぶ（キーの末尾が反転した時刻）。
	if !iter.First() {
		return MergeBase{ParentTS: info.ForkedAt, BranchTS: info.ForkedAt}, iter.Error()
	}
	parentTS, _, err := keyenc.DecodeInvertedTime(iter.Key()[len(prefix):])
	if err != nil {
		return MergeBase{}, err
	}
	v := iter.Value()
	if len(v) != 9 || v[0] != valData {
		return MergeBase{}, fmt.Errorf("HeimDB: corrupt merge base at %x", iter.Key())
	}
	return MergeBase{ParentTS: parentTS, BranchTS: binary.BigEndian.Uint64(v[1:])}, nil
}

// SetMergeBase は、このトランザクションのコミットでブランチ branch のマージの基準を記録する。キーの時刻はコミット時刻
// （親に書いた時刻）になり、branchTS（マージが読んだブランチのスナップショットの時刻）を値に持つ。
func (tx *Txn) SetMergeBase(branch, branchTS uint64) error {
	key := keyenc.AppendInvertedTime(mergePrefix(branch), uncommittedTS)
	return tx.set(key, append([]byte{valData}, binary.BigEndian.AppendUint64(nil, branchTS)...))
}

// DropMergeBasesAfter は、このトランザクションのコミットで、親が parent のブランチのマージの基準の記録のうち、親の
// 時刻が ts より後のものを消す。親を時刻 ts の状態に戻す（マージを戻す）と、その後のマージの基準は親にない状態を
// 指すので、一つ前の基準（なければ分岐時刻）に戻し、戻した後にもう一度マージできるようにする（docs/plan.md の段階 44）。
// 呼び出し側は親の書き込みロックを持つ（子のマージも親のロックを取るので、記録はその間に増えない）。
func (tx *Txn) DropMergeBasesAfter(parent, ts uint64) error {
	if tx.done {
		return ErrTxnDone
	}
	for _, info := range tx.s.Branches() {
		if info.Parent != parent {
			continue
		}
		prefix := mergePrefix(info.ID)
		iter, err := tx.s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
		if err != nil {
			return err
		}
		// 記録は新しい順に並ぶ。
		for valid := iter.First(); valid; valid = iter.Next() {
			parentTS, _, err := keyenc.DecodeInvertedTime(iter.Key()[len(prefix):])
			if err != nil {
				iter.Close()
				return err
			}
			if parentTS <= ts {
				break
			}
			if err := tx.deleteCommitted(bytes.Clone(iter.Key())); err != nil {
				iter.Close()
				return err
			}
		}
		if err := iter.Close(); err != nil {
			return err
		}
	}
	return nil
}

// ChangedRows は、ブランチ branch の自分のキー空間で、テーブル tableID の行のうち時刻 (after, upTo] に書かれたバージョンを持つ
// 主キーを、主キーの順に返す。親の層は見ない（ブランチが変えた行の候補を、親の全件を読まずに求めるため）。
func (s *Store) ChangedRows(branch, tableID, after, upTo uint64) ([][]byte, error) {
	rows, err := s.changedRows(branch, tableID, after, upTo)
	out := make([][]byte, len(rows))
	for i, r := range rows {
		out[i] = r.PK
	}
	return out, err
}

// ChangedRow は、変わった行の主キーと、(after, upTo] の中で最後に書いたバージョンの時刻。
type ChangedRow struct {
	PK []byte
	TS uint64
}

func (s *Store) changedRows(branch, tableID, after, upTo uint64) ([]ChangedRow, error) {
	prefix := RowPrefix(branch, tableID)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var out []ChangedRow
	var last []byte
	for valid := iter.First(); valid; valid = iter.Next() {
		key := iter.Key()
		if len(key) < len(prefix)+8 {
			return nil, fmt.Errorf("HeimDB: corrupt row key %x", key)
		}
		pk := key[len(prefix) : len(key)-8]
		if last != nil && string(pk) == string(last) {
			continue
		}
		ts, _, err := keyenc.DecodeInvertedTime(key[len(key)-8:])
		if err != nil {
			return nil, err
		}
		if ts > after && ts <= upTo {
			last = append([]byte(nil), pk...)
			out = append(out, ChangedRow{PK: last, TS: ts})
			if check := s.ReadCheck; check != nil && len(out)%1024 == 0 {
				if err := check(); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, iter.Error()
}

// Resolution は、マージの競合の解決の記録（docs/plan.md の段階 7b）。
type Resolution struct {
	// Table はテーブル名（小文字）か "*"、ID は行の id（差分の表示と同じ「列=値」）か "*"。
	Table string `json:"table"`
	ID    string `json:"id"`
	// Choice は "branch" か "parent"。
	Choice string `json:"choice"`
	// Hash は、行を指定したときに記録した時点の両側の行の値のハッシュ。ワイルドカードでは空。
	Hash string `json:"hash,omitempty"`
	// Base は記録した時点の基準の時刻（Bases.Key）。基準が変わった（マージした、取り込んだ）記録は使わない。
	Base uint64 `json:"base"`
	// By と At は、記録したユーザー（'user'@'host'）と時刻（UNIX マイクロ秒）。段階 63 より前の記録では空。
	By string `json:"by,omitempty"`
	At uint64 `json:"at,omitempty"`
}

// PutResolution は、ブランチ branch の競合の解決を記録する。r.Choice が "none" なら記録を消す。
func (s *Store) PutResolution(branch uint64, r Resolution) error {
	key := resolveKey(branch, r.Table, r.ID)
	return s.Update(func(tx *WriteTx) error {
		if r.Choice == "none" {
			return tx.batch.Delete(key, nil)
		}
		return tx.setJSON(key, r)
	})
}

// Resolutions は、ブランチ branch の競合の解決のうち、今の基準（Bases.Key）で記録したものを返す。取り込みの前に記録した
// 解決は、取り込んだ後は使わない（段階 94）。
func (s *Store) Resolutions(branch uint64) ([]Resolution, error) {
	bases, err := s.Bases(branch)
	if err != nil {
		return nil, err
	}
	prefix := resolvePrefix(branch)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var out []Resolution
	for valid := iter.First(); valid; valid = iter.Next() {
		var r Resolution
		if err := json.Unmarshal(iter.Value(), &r); err != nil {
			return nil, fmt.Errorf("HeimDB: corrupt resolution at %x: %w", iter.Key(), err)
		}
		if r.Base == bases.Key() {
			out = append(out, r)
		}
	}
	return out, iter.Error()
}

// ClearResolutions は、ブランチ branch の競合の解決の記録をすべて消す。
func (s *Store) ClearResolutions(branch uint64) error {
	prefix := resolvePrefix(branch)
	return s.Update(func(tx *WriteTx) error {
		return tx.batch.DeleteRange(prefix, keyenc.PrefixEnd(prefix), nil)
	})
}

// ChangedRowsSince は、ブランチ branch を読むときの各層（自分、親の分岐時刻以前、…）で、テーブル tableID の行のうち
// 時刻 (after, upTo] に書かれたバージョンを持つ主キーを、主キーの順に返す。時刻 after の状態から今の状態までに見え方が
// 変わりうる行の候補（復元、docs/plan.md の段階 8）。
func (s *Store) ChangedRowsSince(branch, tableID, after, upTo uint64) ([][]byte, error) {
	rows, err := s.ChangedRowTimesSince(branch, tableID, after, upTo)
	out := make([][]byte, len(rows))
	for i, r := range rows {
		out[i] = r.PK
	}
	return out, err
}

// ChangedRowTimesSince は ChangedRowsSince と同じ行を、見えるバージョンを書いた時刻とともに返す（変更の一覧、docs/plan.md の
// 段階 34）。行が複数の層で変わっていれば、上の層（ブランチ自身）の時刻。
func (s *Store) ChangedRowTimesSince(branch, tableID, after, upTo uint64) ([]ChangedRow, error) {
	ls, err := s.layers(branch)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []ChangedRow
	for _, l := range ls {
		hi := min(l.maxTS, upTo)
		if hi <= after {
			continue
		}
		rows, err := s.changedRows(l.branch, tableID, after, hi)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if !seen[string(r.PK)] {
				seen[string(r.PK)] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].PK) < string(out[j].PK) })
	return out, nil
}

// PutTable は、このトランザクションのコミットでテーブル定義の新しいバージョンを t.Name の名前で書く（復元）。
// 書き込みロックは呼び出し側が持っている。
func (tx *Txn) PutTable(branch uint64, t *schema.Table) error {
	b, err := t.Marshal()
	if err != nil {
		return err
	}
	tx.wrote[branch] = true
	return tx.set(catalogKey(branch, t.Name, uncommittedTS), append([]byte{valData}, b...))
}

// DeleteTable は、このトランザクションのコミットでテーブル名 name に削除の印を書く（復元）。
func (tx *Txn) DeleteTable(branch uint64, name string) error {
	tx.wrote[branch] = true
	return tx.set(catalogKey(branch, name, uncommittedTS), []byte{valTombstone})
}

// MergeExcludes は、マージから外す表のパターン（docs/plan.md の段階 60）を、足した順に返す。バージョンを持たないメタデータで、
// コミットを通して書くので、レプリカとバックアップに付いていく。
func (s *Store) MergeExcludes() ([]string, error) {
	v, ok, err := s.get(mergeExcludeKey())
	if err != nil || !ok {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal(v, &out); err != nil {
		return nil, fmt.Errorf("HeimDB: corrupt merge exclusions: %w", err)
	}
	return out, nil
}

// UpdateMergeExcludes は、マージから外す表のパターンを fn で書き換える。読んで書くまでを書き込みの中で行うので、
// 同時に足しても消えない。
func (s *Store) UpdateMergeExcludes(fn func([]string) ([]string, error)) error {
	return s.Update(func(tx *WriteTx) error {
		cur, err := s.MergeExcludes()
		if err != nil {
			return err
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			return tx.batch.Delete(mergeExcludeKey(), nil)
		}
		return tx.setJSON(mergeExcludeKey(), next)
	})
}
