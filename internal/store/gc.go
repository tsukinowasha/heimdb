package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
)

// 古いバージョンの削除（GC、docs/plan.md の段階 9 と「タイムトラベルの時刻と保持期間」）。

// ErrBeforeRetention は、GC の境界より前の時刻を読もうとしたことを表す。
var ErrBeforeRetention = errors.New("HeimDB: the time is before the retention boundary (older versions were removed)")

// GCStats は 1 回の GC の結果。
type GCStats struct {
	// Boundary は境界の時刻。これより前は読めなくなった。Target は保持期間から求めた境界で、開いている
	// トランザクションや使用中の時刻のために Boundary がそれより前に止まることがある。
	Boundary, Target uint64
	// Versions は消したバージョン（行、インデックスのエントリ、テーブル定義）の数。
	Versions int
	// Tables と Indexes は、範囲で消した、使われなくなったテーブルとインデックスの数。
	Tables, Indexes int
	// MergeRecords は消した古いマージの基準の記録の数。
	MergeRecords int
}

// gcBatchSize は、削除を Pebble に書く単位。
const gcBatchSize = 10000

// registerSnapshot は、トランザクション tx のスナップショットを今の最後のコミット時刻に決めて登録する。
func (s *Store) registerSnapshot(tx *Txn) uint64 {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	ts := s.LastCommitted()
	s.snapshots[tx] = ts
	return ts
}

func (s *Store) unregisterSnapshot(tx *Txn) {
	s.snapMu.Lock()
	delete(s.snapshots, tx)
	s.snapMu.Unlock()
}

// GCBoundary は、最後の GC の境界を返す。これより前の時刻のバージョンは消えているかもしれない。
func (s *Store) GCBoundary() uint64 {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	return s.gcBoundary
}

// Pin は、時刻 ts の状態を読み書きに使う間（復元、過去の時点からのブランチ作成、AS OF の読み取り）、GC が境界を ts より
// 先に進めないようにする。ts が境界より前ならエラー。確かめることと登録することは不可分。使い終わったら release を呼ぶ。
func (s *Store) Pin(ts uint64) (release func(), err error) {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if ts < s.gcBoundary {
		return nil, s.beforeRetention(ts)
	}
	id := new(int)
	s.pins[id] = ts
	var once sync.Once
	return func() {
		once.Do(func() {
			s.snapMu.Lock()
			delete(s.pins, id)
			s.snapMu.Unlock()
		})
	}, nil
}

func (s *Store) beforeRetention(ts uint64) error {
	return fmt.Errorf("%w: %s is before %s", ErrBeforeRetention,
		time.UnixMicro(int64(ts)).UTC().Format(time.RFC3339Nano), time.UnixMicro(int64(s.gcBoundary)).UTC().Format(time.RFC3339Nano))
}

// CheckReadable は、時刻 ts の状態を読めるか（GC の境界以降か）を確かめる。
func (s *Store) CheckReadable(ts uint64) error {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if ts < s.gcBoundary {
		return s.beforeRetention(ts)
	}
	return nil
}

// GC は、保持期間 retention を過ぎたバージョンのうち、もう誰にも見えないものを消す。書き込みロックは取らず、通常の読み書きと
// 並行して動く。
func (s *Store) GC(retention time.Duration) (GCStats, error) {
	if retention <= 0 {
		return GCStats{}, errors.New("HeimDB: GC is disabled (retention is 0)")
	}
	start := time.Now()
	stats, err := s.gcUntil(uint64(max(s.now().Add(-retention).UnixMicro(), 0)))
	s.recordGC(start, stats, err)
	return stats, err
}

// gcUntil は、境界を target（開いているスナップショットがそれより古ければ、そこまで下げる）にして GC を動かす。
func (s *Store) gcUntil(target uint64) (GCStats, error) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.closed {
		return GCStats{}, errors.New("HeimDB: the store is closed")
	}

	// GC の途中で払い出されるテーブル ID は対象にしない。
	nextTable, err := s.getUint64(seqKey(seqTable))
	if err != nil {
		return GCStats{}, err
	}

	// 境界を決めて記録してから消す。境界より前を新しく読み始めることはできなくなる。
	s.snapMu.Lock()
	b := min(target, s.LastCommitted())
	for _, ts := range s.snapshots {
		b = min(b, ts)
	}
	for _, ts := range s.pins {
		b = min(b, ts)
	}
	if b < s.gcBoundary {
		b = s.gcBoundary
	}
	s.gcBoundary = b
	s.snapMu.Unlock()
	// 境界はそのマシンだけのもので、コミットを通さない（記録してレプリカに送らない。レプリカも自分の GC で書く）。
	if err := s.db.Set(gcKey(), binary.BigEndian.AppendUint64(nil, b), pebble.Sync); err != nil {
		return GCStats{}, err
	}

	stats := GCStats{Boundary: b, Target: target}
	points, roots := s.preservedPoints(b)
	d := &deleter{s: s}
	for _, info := range s.allBranches() {
		if err := s.gcBranch(d, info.ID, b, points[info.ID], roots[info.ID], &stats); err != nil {
			return stats, err
		}
	}
	if err := s.gcMergeRecords(d, b, &stats); err != nil {
		return stats, err
	}
	if err := d.flush(); err != nil {
		return stats, err
	}
	// バージョンを消した後のテーブル定義から、使われなくなったテーブルとインデックスを探す。
	if err := s.gcUnreferenced(d, nextTable, &stats); err != nil {
		return stats, err
	}
	return stats, d.flush()
}

// allBranches は、データを持ちうるブランチ（登録されたブランチと、一時テーブルの隠しブランチ）を返す。
func (s *Store) allBranches() []BranchInfo {
	out := s.Branches()
	seen := map[uint64]bool{}
	for _, b := range out {
		seen[b.ID] = true
	}
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{tagBranch}, UpperBound: []byte{tagBranch + 1}})
	if err != nil {
		return out
	}
	defer iter.Close()
	for valid := iter.First(); valid; {
		id, _, err := keyenc.DecodeUint64(iter.Key()[1:])
		if err != nil {
			break
		}
		if !seen[id] && IsHiddenBranch(id) {
			out = append(out, BranchInfo{ID: id})
		}
		next := keyenc.PrefixEnd(branchPrefix(id))
		if next == nil {
			break
		}
		valid = iter.SeekGE(next)
	}
	return out
}

// preservedPoints は、ブランチごとに、境界 b より前で見えるバージョンを残すべき時点（昇順）を返す。子ブランチが親を層として
// 読む上限（分岐時刻）と、マージの基準（親の時刻とブランチの時刻）で、それぞれ祖先の層にも伝える。roots は親のないブランチ（下に隠すものがないので削除の印を消せる）。
func (s *Store) preservedPoints(b uint64) (map[uint64][]uint64, map[uint64]bool) {
	points := map[uint64][]uint64{}
	// add は、ブランチ branch を時刻 ts に読むときに使うバージョンを残すよう、branch とその祖先（層の上限で切った時刻）に時点を加える。
	add := func(branch, ts uint64) {
		ls, err := s.layers(branch)
		if err != nil {
			return
		}
		for _, l := range ls {
			p := min(l.maxTS, ts)
			if p < b && !slices.Contains(points[l.branch], p) {
				points[l.branch] = append(points[l.branch], p)
			}
		}
	}
	roots := map[uint64]bool{}
	for _, info := range s.allBranches() {
		if info.Parent == 0 {
			roots[info.ID] = true
		}
		if info.Parent != 0 {
			// 子孫の層の上限（分岐時刻）。祖先には、層の上限で切った時刻が加わる。
			add(info.Parent, info.ForkedAt)
			if base, err := s.MergeBase(info.ID); err == nil {
				add(info.Parent, base.ParentTS)
				add(info.ID, base.BranchTS)
			}
			// 残すマージの基準と取り込みの記録の時点も残す。ブランチを戻して取り込みの記録が消えると、一つ前の記録（直近が
			// マージなら親の時刻とブランチの時刻）を基準に読むため（docs/plan.md の段階 94）。
			id, parent := info.ID, info.Parent
			_ = s.eachRetainedRecord(mergePrefix(id), b, func(ts uint64, v []byte) error {
				add(parent, ts)
				if len(v) == 9 {
					add(id, binary.BigEndian.Uint64(v[1:]))
				}
				return nil
			}, nil)
			_ = s.eachRetainedRecord(syncPrefix(id), b, func(_ uint64, v []byte) error {
				if len(v) == 9 {
					add(parent, binary.BigEndian.Uint64(v[1:]))
				}
				return nil
			}, nil)
		}
	}
	for _, p := range points {
		sort.Slice(p, func(i, j int) bool { return p[i] < p[j] })
	}
	return points, roots
}

// deleter は、削除をまとめて Pebble に書く。
type deleter struct {
	s     *Store
	batch *pebble.Batch
	n     int
}

func (d *deleter) delete(key []byte) error {
	if d.batch == nil {
		d.batch = d.s.db.NewBatch()
	}
	if err := d.batch.Delete(key, nil); err != nil {
		return err
	}
	d.n++
	if d.n >= gcBatchSize {
		return d.flush()
	}
	return nil
}

func (d *deleter) deleteRange(lo, hi []byte) error {
	if d.batch == nil {
		d.batch = d.s.db.NewBatch()
	}
	d.n++
	return d.batch.DeleteRange(lo, hi, nil)
}

func (d *deleter) flush() error {
	if d.batch == nil {
		return nil
	}
	err := d.batch.Commit(pebble.Sync)
	d.batch.Close()
	d.batch, d.n = nil, 0
	return err
}

// version は、バージョン付きのエントリの 1 つのバージョン。
type version struct {
	key       []byte
	ts        uint64
	tombstone bool
}

// gcBranch は、ブランチ branch のキー空間のバージョン付きのエントリ（テーブル定義、行、インデックスのエントリ）から、
// 境界 b より前で、b の時点にも残すべき時点 points にも見えないバージョンを消す。root なら、残った最古のバージョンが b 以前の
// 削除の印のとき、それも消す。
func (s *Store) gcBranch(d *deleter, branch, b uint64, points []uint64, root bool, stats *GCStats) error {
	prefix := branchPrefix(branch)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return err
	}
	defer iter.Close()
	var entry, arena []byte
	var versions []version
	process := func() error {
		for _, v := range gcVictims(versions, b, points, root) {
			if err := d.delete(v.key); err != nil {
				return err
			}
			stats.Versions++
		}
		return nil
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		key := iter.Key()
		if isAutoIncKey(prefix, key) || len(key) < len(prefix)+9 {
			continue
		}
		e := key[:len(key)-8]
		ts, _, err := keyenc.DecodeInvertedTime(key[len(key)-8:])
		if err != nil {
			return fmt.Errorf("HeimDB: corrupt versioned key %x: %w", key, err)
		}
		if !bytes.Equal(e, entry) {
			if err := process(); err != nil {
				return err
			}
			// キーの写しは、エントリごとに使い回す領域に置く（全キーを毎回確保しないため）。
			entry, versions, arena = append(entry[:0], e...), versions[:0], arena[:0]
		}
		v := iter.Value()
		start := len(arena)
		arena = append(arena, key...)
		versions = append(versions, version{key: arena[start:len(arena):len(arena)], ts: ts, tombstone: len(v) > 0 && v[0] == valTombstone})
	}
	if err := iter.Error(); err != nil {
		return err
	}
	return process()
}

// isAutoIncKey は、キーが AUTO_INCREMENT の値（バージョンのないキー b/<ブランチ>/t/<テーブル>/ai）かを返す。
func isAutoIncKey(prefix, key []byte) bool {
	rest := key[len(prefix):]
	if len(rest) == 0 || rest[0] != tagTable {
		return false
	}
	_, tail, err := keyenc.DecodeUint64(rest[1:])
	return err == nil && len(tail) == 1 && tail[0] == tagAutoInc
}

// gcVictims は、1 つのエントリのバージョン（新しい順）のうち、消してよいものを返す。b より新しいバージョンと、b と points の各時点で
// 見えるバージョン（その時点以前で最新のバージョン）は残す。root なら、残るバージョンのうち古い方から続く b 以前の削除の印も消す（その下に
// バージョンが残っていなければ、削除の印があってもなくても同じに見える）。
func gcVictims(versions []version, b uint64, points []uint64, root bool) []version {
	keep := make([]bool, len(versions))
	prev := Latest // 1 つ新しいバージョンの時刻（そのバージョンが見え始める時刻）
	for i, v := range versions {
		switch {
		case v.ts > b:
			keep[i] = true
		case prev > b:
			keep[i] = true // b の時点で見える
		default:
			// v が見えるのは [v.ts, prev) の時刻。そこに残すべき時点があれば残す。
			j := sort.Search(len(points), func(k int) bool { return points[k] >= v.ts })
			keep[i] = j < len(points) && points[j] < prev
		}
		prev = v.ts
	}
	if root {
		for i := len(versions) - 1; i >= 0; i-- {
			if !keep[i] {
				continue
			}
			if !versions[i].tombstone || versions[i].ts > b {
				break
			}
			keep[i] = false
		}
	}
	var out []version
	for i, v := range versions {
		if !keep[i] {
			out = append(out, v)
		}
	}
	return out
}

// gcMergeRecords は、ブランチごとに、要らなくなったマージの基準と取り込みの記録を消す。親（取り込みならブランチ）を境界
// b より後の時刻に戻すと、その時刻より後の記録を消して一つ前の記録に戻す（DropMergeBasesAfter、DropSyncsAfter）ので、
// キーの時刻が b より後の記録と、b 以前で最新の記録を残す。
func (s *Store) gcMergeRecords(d *deleter, b uint64, stats *GCStats) error {
	for _, info := range s.Branches() {
		for _, prefix := range [][]byte{mergePrefix(info.ID), syncPrefix(info.ID)} {
			if err := s.eachRetainedRecord(prefix, b, nil, func(key []byte) error {
				stats.MergeRecords++
				return d.delete(key)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// gcUnreferenced は、残ったテーブル定義（すべてのブランチのすべてのバージョン）のどれからも参照されないテーブル ID の
// キー空間と、インデックス ID のエントリを消す。GC の開始時に払い出し済みでなかったテーブル ID（nextTable より大きい）と、
// 定義の次のインデックス ID 以上のインデックスは、GC の途中で作られたかもしれないので対象にしない。
func (s *Store) gcUnreferenced(d *deleter, nextTable uint64, stats *GCStats) error {
	// 書き直しの途中のテーブル ID と作成の途中のインデックス ID は、まだどのテーブル定義も指さないが消さない
	// （rewrite.go）。印はテーブル定義より先に読む。印を外すコミットはテーブル定義を同時に書くので、印を読んだ後に
	// 外れたなら、定義の方で見つかる。
	rewriting, err := s.rewrites()
	if err != nil {
		return err
	}
	tables := map[uint64]bool{}
	indexes := map[uint64]map[uint64]bool{}
	nextIndex := map[uint64]uint64{}
	for _, info := range s.allBranches() {
		prefix := catalogPrefix(info.ID)
		iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
		if err != nil {
			return err
		}
		for valid := iter.First(); valid; valid = iter.Next() {
			v := iter.Value()
			if len(v) == 0 || v[0] != valData {
				continue
			}
			t, err := schema.Unmarshal(v[1:])
			if err != nil {
				iter.Close()
				return fmt.Errorf("HeimDB: corrupt table definition at %x: %w", iter.Key(), err)
			}
			tables[t.ID] = true
			if indexes[t.ID] == nil {
				indexes[t.ID] = map[uint64]bool{}
			}
			for _, idx := range t.Indexes {
				indexes[t.ID][idx.ID] = true
			}
			nextIndex[t.ID] = max(nextIndex[t.ID], t.NextIndexID)
		}
		if err := iter.Close(); err != nil {
			return err
		}
	}
	for _, info := range s.allBranches() {
		for _, id := range s.tableIDsIn(info.ID) {
			switch {
			case id > nextTable:
				// GC の途中で作られたかもしれない
			case rewriting[rewriteID{info.ID, id, 0}]:
			case !tables[id]:
				p := tablePrefix(info.ID, id)
				if err := d.deleteRange(p, keyenc.PrefixEnd(p)); err != nil {
					return err
				}
				stats.Tables++
			default:
				for _, iid := range s.indexIDsIn(info.ID, id) {
					if iid < nextIndex[id] && !indexes[id][iid] && !rewriting[rewriteID{info.ID, id, iid}] {
						p := IndexPrefix(info.ID, id, iid)
						if err := d.deleteRange(p, keyenc.PrefixEnd(p)); err != nil {
							return err
						}
						stats.Indexes++
					}
				}
			}
		}
	}
	return nil
}

// tableIDsIn は、ブランチ branch のキー空間にあるテーブル ID を返す。
func (s *Store) tableIDsIn(branch uint64) []uint64 {
	return s.idsUnder(append(branchPrefix(branch), tagTable))
}

// indexIDsIn は、ブランチ branch のテーブル tableID のキー空間にあるインデックス ID を返す。
func (s *Store) indexIDsIn(branch, tableID uint64) []uint64 {
	return s.idsUnder(append(tablePrefix(branch, tableID), tagIndex))
}

// idsUnder は、prefix の直後に 8 バイトの ID が続くキーの ID を、重複なしで返す。
func (s *Store) idsUnder(prefix []byte) []uint64 {
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return nil
	}
	defer iter.Close()
	var out []uint64
	for valid := iter.First(); valid; {
		id, _, err := keyenc.DecodeUint64(iter.Key()[len(prefix):])
		if err != nil {
			break
		}
		out = append(out, id)
		next := keyenc.PrefixEnd(keyenc.AppendUint64(slices.Clone(prefix), id))
		if next == nil {
			break
		}
		valid = iter.SeekGE(next)
	}
	return out
}

// UsedIndexIDs は、ブランチ branch を読むときの各層で、テーブル tableID のキー空間にエントリのあるインデックス ID を返す
// （消したインデックスのエントリも含む。復元でインデックス ID を再利用しないため）。
func (s *Store) UsedIndexIDs(branch, tableID uint64) ([]uint64, error) {
	ls, err := s.layers(branch)
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, l := range ls {
		for _, id := range s.indexIDsIn(l.branch, tableID) {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out, nil
}
