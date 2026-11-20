package backend

import (
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/tsukinowasha/heimdb/internal/metrics"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 指標の口（docs/plan.md の段階 73）。heimdb_status と同じ値に、文の数と時間を足して Prometheus の形式で書く。
// 文の中身、ユーザー名、データベース名、表の名前は出さない。

// stmtTypes は、文の数を分ける種類（文の先頭の語で決める）。
var stmtTypes = []string{"select", "insert", "update", "delete", "replace", "call", "ddl", "transaction", "set", "show", "other"}

// stmtStats は、起動からの文の数、エラー、時間。
type stmtStats struct {
	byType   [11]atomic.Uint64 // stmtTypes の順
	slow     atomic.Uint64
	duration *metrics.Histogram
	// lockWait、replicaWait、statsWait は、文が書き込みのロック、同期のレプリカの知らせ、表の統計の作成を待った時間の
	// 合計（ナノ秒。段階 81）。
	lockWait, replicaWait, statsWait atomic.Int64
	mu                               sync.Mutex
	errors                           map[uint16]uint64 // MySQL のエラー番号ごと
}

func newStmtStats() *stmtStats {
	return &stmtStats{
		duration: metrics.NewHistogram(0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 60),
		// 知らせの規則に使う番号は、起きる前から 0 で出す（Prometheus の increase は、新しく現れた系列の最初の値を増えた分と数えない）。
		errors: map[uint16]uint64{1041: 0, 1205: 0, 1213: 0, 3024: 0},
	}
}

// stmtType は、文 q の種類の stmtTypes での位置を返す。
func stmtType(q string) int {
	q = skipSpaceAndComments(q)
	if strings.HasPrefix(q, "(") {
		return 0 // (SELECT ...) UNION ...
	}
	end := strings.IndexFunc(q, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(q)
	}
	switch strings.ToUpper(q[:end]) {
	case "SELECT", "WITH":
		return 0
	case "INSERT":
		return 1
	case "UPDATE":
		return 2
	case "DELETE":
		return 3
	case "REPLACE":
		return 4
	case "CALL":
		return 5
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME":
		return 6
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT":
		return 7
	case "SET":
		return 8
	case "SHOW", "DESCRIBE", "DESC", "EXPLAIN":
		return 9
	}
	return 10
}

// observeWaits は、終わった文が待った時間を足す。
func (st *stmtStats) observeWaits(w store.Waits, stats time.Duration) {
	st.lockWait.Add(int64(w.Lock))
	st.replicaWait.Add(int64(w.Ack))
	st.statsWait.Add(int64(stats))
}

// observe は、終わった文を数える。query は文（エラーで解析の前に止まったなら空）、err は接続先に返したエラー。
func (st *stmtStats) observe(query string, err error, d time.Duration, slow bool) {
	typ := len(stmtTypes) - 1
	if query != "" {
		typ = stmtType(query)
	}
	st.byType[typ].Add(1)
	st.duration.Observe(d)
	if slow {
		st.slow.Add(1)
	}
	if err != nil {
		code := uint16(sql.CastSQLError(err).Num)
		st.mu.Lock()
		st.errors[code]++
		st.mu.Unlock()
	}
}

// WriteMetrics は、指標を w に書く。
func (p *Provider) WriteMetrics(w *metrics.Writer) {
	now := time.Now()
	seconds := func(ts uint64) float64 { return float64(ts) / 1e6 }
	age := func(ts uint64) float64 {
		if ts == 0 {
			return 0
		}
		return now.Sub(time.UnixMicro(int64(ts))).Seconds()
	}
	version := p.Version
	if version == "" {
		version = "dev"
	}
	w.Gauge("heimdb_build_info", "HeimDB version (always 1).", 1, "version", version)
	w.Gauge("heimdb_start_time_seconds", "Start time of the process (Unix time).", float64(p.started.UnixMicro())/1e6)
	replica := 0.0
	if p.st.IsReplica() {
		replica = 1
	}
	w.Gauge("heimdb_replica", "1 on a replica, 0 on a primary.", replica)

	// 接続と文
	w.Gauge("heimdb_connections", "Open connections.", float64(p.connections.Load()))
	w.Counter("heimdb_connections_total", "Connections opened since the start.", float64(p.connectionsTotal.Load()))
	p.gate.mu.Lock()
	running := p.gate.active
	p.gate.mu.Unlock()
	w.Gauge("heimdb_running_statements", "Statements running now.", float64(running))
	for i, t := range stmtTypes {
		w.Counter("heimdb_statements_total", "Statements finished since the start, by the first word.", float64(p.stats.byType[i].Load()), "type", t)
	}
	p.stats.mu.Lock()
	errs := maps.Clone(p.stats.errors)
	p.stats.mu.Unlock()
	for _, c := range slices.Sorted(maps.Keys(errs)) {
		w.Counter("heimdb_statement_errors_total", "Statements that returned an error, by MySQL error code.", float64(errs[c]), "code", strconv.Itoa(int(c)))
	}
	w.Histogram("heimdb_statement_duration_seconds", "Time to run a statement and send its result.", p.stats.duration)
	w.Counter("heimdb_slow_statements_total", "Statements logged as slow (slow_query_log and long_query_time; -slow-query at startup).", float64(p.stats.slow.Load()))
	w.Counter("heimdb_lock_wait_seconds_total", "Time statements waited for a branch's write lock.", time.Duration(p.stats.lockWait.Load()).Seconds())
	w.Counter("heimdb_replica_wait_seconds_total", "Time commits waited for a synchronous replica to acknowledge.", time.Duration(p.stats.replicaWait.Load()).Seconds())
	w.Counter("heimdb_stats_wait_seconds_total", "Time statements waited for a table's statistics to be built (first use after a restart).", time.Duration(p.stats.statsWait.Load()).Seconds())
	// 起動した後に裏で作る統計（段階 106）
	state, _, d, _, _, _ := p.warm.snapshot()
	ready := 0.0
	if state == "ready" {
		ready = 1
		w.Gauge("heimdb_stats_build_seconds", "Time the startup pass took to build the statistics of the tables on main.", d.Seconds())
	}
	w.Gauge("heimdb_stats_ready", "1 once the startup pass has built the statistics of the tables on main (also when some tables failed; stays 1 when later writes rebuild them).", ready)
	// 起動の後の古いインデックスの作り直し（段階 121）
	rb := p.rebuild.snapshot()
	rbReady := 0.0
	if rb.state == "done" {
		rbReady = 1
		w.Gauge("heimdb_index_rebuild_seconds", "Time the last background pass took to rebuild the indexes on main made by an older HeimDB.", rb.duration.Seconds())
	}
	w.Gauge("heimdb_index_rebuild_ready", "1 once the background pass has rebuilt the indexes on main made by an older HeimDB (0 while it runs, with -rebuild-indexes=false and on a replica).", rbReady)
	w.Gauge("heimdb_index_rebuild_skipped", "Indexes the last background pass skipped (duplicate unique values, too many rows or a busy write lock); build them with CALL heimdb_rebuild_indexes().", float64(rb.skipped))

	// メモリ（段階 58）
	st := p.st.Status()
	w.Gauge("heimdb_memory_limit_bytes", "Memory limit (0 without a limit).", float64(MemoryLimit()))
	w.Gauge("heimdb_memory_abort_bytes", "Live heap above which statements fail with error 1041 (0 without a limit).", float64(MemoryAbortBytes()))
	w.Gauge("heimdb_memory_live_bytes", "Live heap after the last Go GC.", float64(liveHeap()))
	if p.engineMemory != nil {
		w.Gauge("heimdb_engine_caches", "Intermediate-result caches the SQL engine holds (GROUP BY, DISTINCT, sorts, subqueries); 0 when no statement runs.", float64(p.engineMemory.NumCaches()))
	}
	w.Counter("heimdb_memory_aborted_statements_total", "Statements stopped near the memory limit (error 1041).", float64(memAborted.Load()))
	w.Gauge("heimdb_cache_size_bytes", "Size of the block cache.", float64(st.CacheSize))
	w.Gauge("heimdb_cache_used_bytes", "Used part of the block cache.", float64(st.CacheUsed))
	w.Gauge("go_goroutines", "Goroutines that currently exist.", float64(runtime.NumGoroutine()))

	// 書き込みのロック（段階 82）。heimdb_branch_write_lock_* は持ち主のいるブランチだけ出す。
	holders, waiters := p.lockHolders()
	// 接続が持っている書き込みのロックの時間（マージや復元など内部の操作は含まない）。持ち主の接続 ID と突き合わせずに
	// 「接続が長く持っている」を見張れる（Cloud Monitoring のように 2 つの指標を突き合わせられない監視のため。段階 83）。
	// 検証環境のブランチと本番（main）を分けて見張れるよう、ブランチごとに出し、接続が持っていないブランチも 0 で
	// いつも出す（ホスティング側の依頼。段階 83 の後）。一時テーブルの隠しブランチは出さない。
	connHeld := map[string]time.Duration{}
	for _, b := range p.st.Branches() {
		connHeld[b.Name] = 0
	}
	for _, h := range holders {
		// 一時テーブルの隠しブランチのロックは、その接続しか待たないので数えない。
		if h.holder.Owner.Conn != 0 && !store.IsHiddenBranch(h.holder.Branch) {
			connHeld[h.branch] = max(connHeld[h.branch], h.held)
		}
	}
	for _, b := range slices.Sorted(maps.Keys(connHeld)) {
		w.Gauge("heimdb_connection_write_lock_held_seconds", "Seconds a connection has held the branch's write lock (0 if none; internal operations such as merges are not counted).", connHeld[b].Seconds(), "branch", b)
	}
	for _, h := range holders {
		w.Gauge("heimdb_branch_write_lock_held_seconds", "Seconds the branch's write lock has been held.", h.held.Seconds(), "branch", h.branch)
		w.Gauge("heimdb_branch_write_lock_holder", "Connection ID holding the branch's write lock (0 for an internal operation such as a merge).", float64(h.holder.Owner.Conn), "branch", h.branch)
		w.Gauge("heimdb_branch_write_lock_idle_seconds", "Seconds the connection holding the branch's write lock has been idle in its transaction (0 while it runs a statement).", h.idle.Seconds(), "branch", h.branch)
	}
	for b, n := range waiters {
		w.Gauge("heimdb_branch_write_lock_waiters", "Statements waiting for the branch's write lock.", float64(n), "branch", b)
	}
	var longest time.Duration
	if p.Processes != nil {
		for _, pr := range p.Processes() {
			if pr.Command == sql.ProcessCommandQuery {
				longest = max(longest, time.Since(pr.StartedAt))
			}
		}
	}
	w.Gauge("heimdb_longest_statement_seconds", "Seconds the longest running statement has run.", longest.Seconds())
	w.Counter("heimdb_idle_lock_disconnects_total", "Connections closed by -idle-lock-timeout.", float64(p.lockStats.idleKills.Load()))

	// ストア
	branches := p.st.Branches()
	w.Gauge("heimdb_disk_bytes", "Disk space used by the data.", float64(st.DiskBytes))
	w.Gauge("heimdb_branches", "Branches, including main.", float64(len(branches)))
	for _, b := range branches {
		if size, err := p.st.BranchDiskUsage(b.ID); err == nil {
			w.Gauge("heimdb_branch_size_bytes", "Estimated disk space of a branch's own data.", float64(size), "branch", b.Name)
		}
	}
	if ts := p.st.LastCommitted(); ts != 0 {
		w.Gauge("heimdb_last_commit_timestamp_seconds", "Time of the last commit (Unix time).", seconds(ts))
	}

	// トランザクションと GC（段階 19）
	w.Gauge("heimdb_open_transactions", "Open transactions with a snapshot.", float64(st.OpenTransactions))
	w.Gauge("heimdb_oldest_snapshot_age_seconds", "Age of the oldest open snapshot, which holds back GC (0 if none). Read-only transactions such as a logical dump count too.", age(st.OldestTransaction))
	w.Gauge("heimdb_pinned_times", "Times in use by AS OF, restores and branches from the past.", float64(st.Pins))
	if st.GCBoundary != 0 {
		w.Gauge("heimdb_gc_boundary_age_seconds", "Age of the GC boundary (versions before it are removed).", age(st.GCBoundary))
	}
	if g := st.LastGC; g != nil {
		failed := 0.0
		if g.Err != nil {
			failed = 1
		}
		w.Gauge("heimdb_gc_last_run_timestamp_seconds", "Start of the last GC (Unix time).", float64(g.At.UnixMicro())/1e6)
		w.Gauge("heimdb_gc_last_duration_seconds", "Duration of the last GC.", g.Duration.Seconds())
		w.Gauge("heimdb_gc_last_failed", "1 if the last GC failed.", failed)
	}

	// レプリケーション（段階 24）
	if p.ReplicationMetrics != nil {
		p.ReplicationMetrics(w)
	}

	// そのほか
	if !p.TLSNotAfter.IsZero() {
		w.Gauge("heimdb_tls_cert_expiry_timestamp_seconds", "Expiry of the TLS certificate (Unix time).", float64(p.TLSNotAfter.Unix()))
	}
	p.watch.mu.Lock()
	changes := p.watch.count
	p.watch.mu.Unlock()
	w.Counter("heimdb_watch_changes_total", "Risky changes found by the watch since the start.", float64(changes))
}
