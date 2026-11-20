package store

import (
	"time"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// 運用のための状態（docs/plan.md の段階 19）。

// GCRun は、最後に動いた GC の記録（起動してからのもの）。
type GCRun struct {
	At       time.Time
	Duration time.Duration
	Stats    GCStats
	Err      error
}

// Status は、ストア全体の状態。
type Status struct {
	// DiskBytes は、Pebble が使っているディスクの大きさ。
	DiskBytes uint64
	// GCBoundary は、GC の境界（これより前は読めない）。0 なら GC はまだ消していない。
	GCBoundary uint64
	// LastGC は、最後の GC。起動してから動いていなければ nil。
	LastGC *GCRun
	// OpenTransactions と OldestTransaction は、読み取りの時点が決まった開いているトランザクションの数と、その中で
	// 最も古い時点（なければ 0）。GC の境界はこれより先に進まない。
	OpenTransactions  int
	OldestTransaction uint64
	// Pins と OldestPin は、使用中の時刻（AS OF、復元、過去の時点からのブランチ作成）の数と、最も古いもの。
	Pins      int
	OldestPin uint64
	// CacheSize と CacheUsed は、Pebble のブロックキャッシュの大きさと、今使っている分（docs/plan.md の段階 58）。
	CacheSize int64
	CacheUsed int64
}

// Status は、ストアの今の状態を返す。
func (s *Store) Status() Status {
	m := s.db.Metrics()
	st := Status{DiskBytes: m.DiskSpaceUsage(), CacheSize: s.cacheSize, CacheUsed: m.BlockCache.Size}
	s.snapMu.Lock()
	st.GCBoundary = s.gcBoundary
	st.OpenTransactions = len(s.snapshots)
	for _, ts := range s.snapshots {
		if st.OldestTransaction == 0 || ts < st.OldestTransaction {
			st.OldestTransaction = ts
		}
	}
	st.Pins = len(s.pins)
	for _, ts := range s.pins {
		if st.OldestPin == 0 || ts < st.OldestPin {
			st.OldestPin = ts
		}
	}
	s.snapMu.Unlock()
	s.lastGCMu.Lock()
	if s.lastGC != nil {
		run := *s.lastGC
		st.LastGC = &run
	}
	s.lastGCMu.Unlock()
	return st
}

// BranchDiskUsage は、ブランチ自身のデータ（テーブル定義、行、インデックス）が使っているディスクの大きさの推定を返す。
// 書いた直後でまだメモリにある分は数えない。分岐の時点のために残している親のバージョンは、親の大きさに入る。
func (s *Store) BranchDiskUsage(branch uint64) (uint64, error) {
	prefix := branchPrefix(branch)
	return s.db.EstimateDiskUsage(prefix, keyenc.PrefixEnd(prefix))
}

// recordGC は、GC の結果を覚える。
func (s *Store) recordGC(start time.Time, stats GCStats, err error) {
	s.lastGCMu.Lock()
	s.lastGC = &GCRun{At: start, Duration: time.Since(start), Stats: stats, Err: err}
	s.lastGCMu.Unlock()
}
