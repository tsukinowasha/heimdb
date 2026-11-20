package backend

import (
	"fmt"
	"strconv"
	"time"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 運用のための状態（docs/plan.md の段階 19）。CALL heimdb_status() は、項目と値の行を返す。

var statusSchema = sql.Schema{
	{Name: "name", Type: types.LongText},
	{Name: "value", Type: types.LongText, Nullable: true},
}

func (p *Provider) status(ctx *sql.Context) (sql.RowIter, error) {
	st := p.st.Status()
	now := time.Now()
	age := func(ts uint64) any {
		if ts == 0 {
			return nil
		}
		return strconv.FormatInt(int64(now.Sub(time.UnixMicro(int64(ts))).Seconds()), 10)
	}
	at := func(ts uint64) any {
		if ts == 0 {
			return nil
		}
		return formatRestoreTime(ts)
	}
	p.gate.mu.Lock()
	running := p.gate.active
	p.gate.mu.Unlock()
	rows := []sql.Row{
		{"disk_bytes", strconv.FormatUint(st.DiskBytes, 10)},
		{"branches", strconv.Itoa(len(p.st.Branches()))},
		{"connections", strconv.FormatInt(p.connections.Load(), 10)},
		{"running_statements", strconv.Itoa(running)}, // この呼び出しを含む
		{"open_transactions", strconv.Itoa(st.OpenTransactions)},
		// トランザクションを開いてからの時間ではなく、そのスナップショット（開いた時点の最後のコミット）の古さ。
		// GC の境界をどこまで止めているかを表す。
		{"oldest_snapshot_age_seconds", age(st.OldestTransaction)},
		{"pinned_times", strconv.Itoa(st.Pins)},
		{"oldest_pinned_time_age_seconds", age(st.OldestPin)},
		{"gc_retention", p.Retention.String()},
		{"gc_boundary", at(st.GCBoundary)},
		{"gc_boundary_age_seconds", age(st.GCBoundary)},
	}
	// 起動した後に裏で作る統計（docs/plan.md の段階 106）。stats は、作っている間は building、作り終えたら ready、
	// 表の一覧を読めなければ failed（WarmStats を呼ばなければ off）。時刻、時間、表の数は ready のときだけ、stats_error は
	// failed のときだけ出す（ほかは NULL）。
	state, readyAt, d, built, failed, warmErr := p.warm.snapshot()
	if state == "" {
		state = "off"
	}
	var statsAt, statsDur, statsTables, statsErr any
	if state == "ready" {
		statsAt, statsDur = at(uint64(readyAt.UnixMicro())), d.Round(time.Millisecond).String()
		statsTables = fmt.Sprintf("%d built, %d failed", built, failed)
	}
	if warmErr != "" {
		statsErr = warmErr
	}
	rows = append(rows, sql.Row{"stats", state}, sql.Row{"stats_ready_at", statsAt}, sql.Row{"stats_build_duration", statsDur},
		sql.Row{"stats_tables", statsTables}, sql.Row{"stats_error", statsErr})
	// 起動の後の古いインデックスの作り直し（docs/plan.md の段階 121）。index_rebuild は、動かさなければ（-rebuild-indexes=false、
	// レプリカ）off、作っている間は running、1 回を終えたら done。時刻、時間、数は done のときだけ、index_rebuild_error は
	// 最後に飛ばした理由（ほかは NULL）。数と時間は最後の 1 回のもの（main の復元の後にもう 1 回動く）。
	rb := p.rebuild.snapshot()
	rbState := rb.state
	if rbState == "" {
		rbState = "off"
	}
	var rbAt, rbDur, rbIndexes, rbErr any
	if rbState == "done" {
		rbAt, rbDur = at(uint64(rb.doneAt.UnixMicro())), rb.duration.Round(time.Millisecond).String()
		rbIndexes = fmt.Sprintf("%d rebuilt, %d skipped", rb.rebuilt, rb.skipped)
	}
	if rb.err != "" {
		rbErr = rb.err
	}
	rows = append(rows, sql.Row{"index_rebuild", rbState}, sql.Row{"index_rebuild_done_at", rbAt}, sql.Row{"index_rebuild_duration", rbDur},
		sql.Row{"index_rebuild_indexes", rbIndexes}, sql.Row{"index_rebuild_error", rbErr})
	// 遅い文のログ（docs/plan.md の段階 81）。閾値は SET GLOBAL long_query_time と slow_query_log で変わる。
	slow := "off"
	if d, on := slowThreshold(); on {
		slow = d.String()
	}
	rows = append(rows, sql.Row{"slow_query", slow}, sql.Row{"slow_query_redact", strconv.FormatBool(p.SlowQueryRedact)})
	// 書き込みのロックの持ち主（段階 82）
	rows = append(rows, sql.Row{"idle_lock_timeout", p.IdleLockTimeout.String()}, sql.Row{"idle_lock_disconnects", strconv.FormatUint(p.lockStats.idleKills.Load(), 10)})
	holders, waiters := p.lockHolders()
	for _, h := range holders {
		rows = append(rows, sql.Row{"write_lock " + h.branch, h.String()})
	}
	for b, n := range waiters {
		rows = append(rows, sql.Row{"write_lock_waiters " + b, strconv.Itoa(n)})
	}
	// メモリ（docs/plan.md の段階 58）。memory_live_bytes は直前の GC で生きていたヒープで、文を止めるかどうかを
	// 最初に比べる値。memory_abort_bytes を超えると文がエラー 1041 になる（上限がなければ、上限と境目は 0）。
	// ブロックキャッシュは、cgo なしで建てると（本番のイメージ）生きているヒープに含まれる。
	rows = append(rows,
		sql.Row{"memory_limit_bytes", strconv.FormatInt(MemoryLimit(), 10)},
		sql.Row{"memory_abort_bytes", strconv.FormatInt(MemoryAbortBytes(), 10)},
		sql.Row{"memory_live_bytes", strconv.FormatInt(liveHeap(), 10)},
		sql.Row{"memory_aborted_statements", strconv.FormatInt(memAborted.Load(), 10)},
		sql.Row{"cache_size_bytes", strconv.FormatInt(st.CacheSize, 10)},
		sql.Row{"cache_used_bytes", strconv.FormatInt(st.CacheUsed, 10)},
	)
	// GC の記録は、まだ GC をしていなければ値が NULL。
	var runAt, duration, removed, errText any
	if g := st.LastGC; g != nil {
		runAt = g.At.UTC().Format("2006-01-02 15:04:05.000000-07:00")
		duration = g.Duration.Round(time.Millisecond).String()
		removed = fmt.Sprintf("%d versions, %d tables, %d indexes, %d merge records", g.Stats.Versions, g.Stats.Tables, g.Stats.Indexes, g.Stats.MergeRecords)
		if g.Err != nil {
			errText = g.Err.Error()
		}
	}
	rows = append(rows,
		sql.Row{"gc_last_run_at", runAt},
		sql.Row{"gc_last_duration", duration},
		sql.Row{"gc_last_removed", removed},
		sql.Row{"gc_last_error", errText},
	)
	// 通信の暗号化（docs/plan.md の段階 26）。connection_tls は、この手続きを呼んだ接続が TLS か。
	tlsMode := p.TLS
	if tlsMode == "" {
		tlsMode = "off"
	}
	var notAfter any
	if !p.TLSNotAfter.IsZero() {
		notAfter = formatRestoreTime(uint64(p.TLSNotAfter.UnixMicro()))
	}
	connTLS := "no"
	if ctx.Session.Client().Capabilities&mysql.CapabilityClientSSL != 0 {
		connTLS = "yes"
	}
	rows = append(rows, sql.Row{"tls", tlsMode}, sql.Row{"tls_cert_not_after", notAfter}, sql.Row{"connection_tls", connTLS})
	// 危ない変更の見張り（docs/plan.md の段階 33）。起動からの件数と最後の 1 件。
	p.watch.mu.Lock()
	var last, lastAt any
	if p.watch.count > 0 {
		last, lastAt = p.watch.last, at(uint64(p.watch.lastAt.UnixMicro()))
	}
	rows = append(rows, sql.Row{"watch_changes", strconv.FormatInt(p.watch.count, 10)}, sql.Row{"watch_last_change", last}, sql.Row{"watch_last_change_at", lastAt})
	p.watch.mu.Unlock()
	// レプリケーション（docs/plan.md の段階 24）。記録の時刻は、残っている最も古いものと最も新しいもの。
	role := "primary"
	if p.st.IsReplica() {
		role = "replica"
	}
	rows = append(rows, sql.Row{"role", role}, sql.Row{"last_commit", at(p.st.LastCommitted())})
	// 同期のレプリカの知らせを待っている（まだ見せていない）コミット（段階 25）。レプリカでも、記録の適用の後に
	// メモリの内容を作り直す間は Written の方が先にある（段階 30）が、知らせを待つコミットではないので出さない。
	if w := p.st.Written(); !p.st.IsReplica() && w > p.st.LastCommitted() {
		rows = append(rows, sql.Row{"pending_commit", at(w)})
	}
	if p.st.Logging() {
		oldest, newest, err := p.st.LogRange()
		if err != nil {
			return nil, err
		}
		rows = append(rows, sql.Row{"replication_log_oldest", at(oldest)}, sql.Row{"replication_log_newest", at(newest)})
	}
	if p.ReplicationStatus != nil {
		for _, it := range p.ReplicationStatus() {
			rows = append(rows, sql.Row{it.Name, it.Value})
		}
	}
	return &rowIter{rows: rows}, nil
}
