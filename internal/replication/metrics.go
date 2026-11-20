package replication

import (
	"sort"
	"time"

	"github.com/tsukinowasha/heimdb/internal/metrics"
)

// 指標の口に足す値（docs/plan.md の段階 73）。Status と同じ値を数で書く。

// lagSeconds は、本番の最後のコミット committed と位置 position の時刻の差（秒）を返す。
func lagSeconds(committed, position uint64) float64 {
	if position >= committed {
		return 0
	}
	return float64(committed-position) / 1e6
}

// WriteMetrics は、本番の側の値（同期、レプリカごとの遅れと最後の連絡）を w に書く。
func (p *Primary) WriteMetrics(w *metrics.Writer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	on := 0.0
	if p.mode == syncOn {
		on = 1
	}
	w.Gauge("heimdb_replication_sync", "1 while commits wait for a replica (synchronous), 0 otherwise.", on)
	w.Counter("heimdb_replication_async_switches_total", "Times the primary switched to asynchronous because no replica answered in time.", float64(p.asyncCount))
	names := make([]string, 0, len(p.replicas))
	for n := range p.replicas {
		names = append(names, n)
	}
	sort.Strings(names)
	committed := p.st.Written()
	for _, n := range names {
		w.Gauge("heimdb_replica_behind_seconds", "How far a replica is behind the last commit.", lagSeconds(committed, p.replicas[n].position), "replica", n)
	}
	for _, n := range names {
		w.Gauge("heimdb_replica_last_seen_seconds", "Time since a replica last asked for commit records.", time.Since(p.replicas[n].at).Seconds(), "replica", n)
	}
}

// WriteMetrics は、レプリカの側の値（本番からの遅れ、最後の連絡、エラー）を w に書く。
func (r *Replica) WriteMetrics(w *metrics.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primaryLast != 0 {
		w.Gauge("heimdb_replication_behind_seconds", "How far this replica is behind the primary's last commit it has heard of.", lagSeconds(r.primaryLast, r.st.LastCommitted()))
	}
	if !r.lastContact.IsZero() {
		w.Gauge("heimdb_replication_last_contact_seconds", "Time since this replica last heard from the primary.", time.Since(r.lastContact).Seconds())
	}
	failing := 0.0
	if r.lastErr != "" || r.stopped {
		failing = 1
	}
	w.Gauge("heimdb_replication_failing", "1 if the last request to the primary failed or replication stopped.", failing)
}
