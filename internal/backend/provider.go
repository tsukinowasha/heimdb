// Package backend は HeimDB のストアを go-mysql-server のデータベースとして見せる。
package backend

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/mysql_db"

	"github.com/tsukinowasha/heimdb/internal/metrics"
	"github.com/tsukinowasha/heimdb/internal/replication"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// Provider は sql.DatabaseProvider の実装。データベース名でブランチを選ぶ: base（例: wordpress）は main ブランチ、
// base@<ブランチ名>（例: wordpress@staging）はそのブランチ（docs/plan.md の段階 6）。
type Provider struct {
	st    *store.Store
	base  string
	procs sql.ExternalStoredProcedureRegistry
	// Retention は古いバージョンを残す期間（docs/plan.md の段階 9）。0 なら消さない。heimdb_gc で使う。
	Retention time.Duration
	// BackupDir は heimdb_backup がバックアップを置くディレクトリ（docs/plan.md の段階 16）。空ならバックアップできない。
	BackupDir string
	// backupKeys は、暗号化した退避の鍵（-backup-key。backupkey.go。docs/plan.md の段階 87）。
	backupKeys *backupKeys
	// gate は、実行中のコマンドを数え、停止の後は新しいコマンドを拒否する（shutdown.go）。
	gate gate
	// branchOps は、ブランチごとの、マージ、取り込み、復元の鍵（ブランチ ID → *sync.Mutex）。同じブランチの基準の記録を
	// 読んで書く操作を、同時に流さない（docs/plan.md の段階 94）。
	branchOps sync.Map
	// connections は、開いている接続（セッション）の数（status.go）。connectionsTotal は起動からの数、stats は文の数と
	// 時間、started は起動の時刻（metrics.go）。
	connections      atomic.Int64
	connectionsTotal atomic.Int64
	stats            *stmtStats
	started          time.Time
	// Version は HeimDB のバージョン（指標の heimdb_build_info）。
	Version string
	// SlowQueryRedact は、遅い文のログの文の値を ? に畳む（-slow-query-redact。docs/plan.md の段階 81）。閾値は
	// グローバル変数の slow_query_log と long_query_time（slowlog.go）。
	SlowQueryRedact bool
	// ReplicationStatus は、heimdb_status に足すレプリケーションの状態（docs/plan.md の段階 24）。nil なら足さない。
	ReplicationStatus func() []replication.StatusItem
	// ReplicationMetrics は、指標に足すレプリケーションの値（docs/plan.md の段階 73）。nil なら足さない。
	ReplicationMetrics func(*metrics.Writer)
	// TLS は MySQL の接続の暗号化（off、on、required）、TLSNotAfter は証明書の期限（docs/plan.md の段階 26）。
	TLS         string
	TLSNotAfter time.Time
	// watch は、危ない変更の見張りで見つけた変更（watch.go）。
	watch watcher
	// BranchProcedures は、ブランチのユーザーに自分のブランチの手続きを開けること（-branch-procedures。branchuser.go）。
	BranchProcedures bool
	// users は、エンジンのユーザーと権限（NewEngine が入れる）。ブランチの上限をユーザーの権限で決める（limit.go）。
	users *mysql_db.MySQLDb
	// engineMemory は、エンジンの文の途中結果のキャッシュ（NewEngine が入れる）。指標の heimdb_engine_caches（段階 109）。
	engineMemory *sql.MemoryManager
	// stopWarm は、起動した後に裏で統計を作るのを止める（warm.go）。Shutdown が呼ぶ。
	stopWarm context.CancelFunc
	// warm は、その様子（heimdb_status と指標に出す。段階 106）。
	warm warmState
	// Processes と KillConnection は、エンジンの接続の一覧と、接続を切る口（cmd/heimdb が入れる。lockwatch.go）。
	// IdleLockTimeout は、何もしないまま書き込みのロックを持つ接続を切る時間（-idle-lock-timeout。0 なら切らない）。
	Processes       func() []sql.Process
	KillConnection  func(connID uint32) error
	IdleLockTimeout time.Duration
	// sessions は、接続 ID ごとのセッション（ロックの持ち主の最後の文を引く）。lockStats は lockwatch.go の数。
	sessions  sync.Map
	lockStats lockWatchStats
	stopWatch context.CancelFunc
	// rebuild は、起動の後に main の古いインデックスを裏で作り直す様子（autorebuild.go。段階 121）。stopRebuild は
	// それを止める（Shutdown が呼ぶ）。
	rebuild     rebuildState
	stopRebuild context.CancelFunc
}

var (
	_ sql.DatabaseProvider                = (*Provider)(nil)
	_ sql.ExternalStoredProcedureProvider = (*Provider)(nil)
)

// NewProvider は st のブランチを、base とそれに「@ブランチ名」を付けた名前のデータベースとして見せる Provider を作る。
func NewProvider(st *store.Store, base string) *Provider {
	p := &Provider{st: st, base: base, procs: sql.NewExternalStoredProcedureRegistry(), stats: newStmtStats(), started: time.Now()}
	st.CommitCheck = checkCommitMemory
	st.ReadCheck = checkMemory
	st.AllocCheck = checkAlloc
	procs := append(p.branchProcedures(), p.mergeExcludeProcedures()...)
	for _, proc := range append(procs, p.rebuildIndexesProcedures()...) {
		// 認証が有効なら、SUPER 権限のユーザー（root か GRANT ALL ON *.*）だけが実行できる（docs/plan.md の段階 14）。
		// 読むだけの一部の手続きは、そのデータベースの EXECUTE 権限で呼べる（段階 60）。-branch-procedures で開ける
		// 手続きもある（段階 63）ので、引いたときに決める（adminOnly）。
		p.procs.Register(proc)
	}
	// レプリカが本番の記録を適用したら、行数の見積もりに書いた数を足す（行の編集を通らないので）。
	st.OnApply(func(a store.Applied) {
		for t, n := range a.Rows {
			rowCounts.changedBy(st, t.Branch, t.Table, n)
		}
	})
	return p
}

// database は、データベース名に対応するブランチのデータベースを返す。
func (p *Provider) database(name string) (*Database, bool) {
	lower := strings.ToLower(name)
	base := strings.ToLower(p.base)
	if lower == base {
		return &Database{name: p.base, st: p.st, branch: store.MainBranchID}, true
	}
	branchName, ok := strings.CutPrefix(lower, base+"@")
	if !ok {
		return nil, false
	}
	b, ok := p.st.BranchByName(branchName)
	if !ok {
		return nil, false
	}
	return &Database{name: p.dbName(b), st: p.st, branch: b.ID}, true
}

// lockBranchOp は、ブランチ id のマージ、取り込み、復元の鍵を取り、外す関数を返す。
func (p *Provider) lockBranchOp(id uint64) func() {
	v, _ := p.branchOps.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// dbName は、ブランチのデータベース名を返す。
func (p *Provider) dbName(b store.BranchInfo) string {
	if b.ID == store.MainBranchID {
		return p.base
	}
	return p.base + "@" + b.Name
}

// Database は sql.DatabaseProvider の実装。
func (p *Provider) Database(_ *sql.Context, name string) (sql.Database, error) {
	if db, ok := p.database(name); ok {
		return db, nil
	}
	return nil, sql.ErrDatabaseNotFound.New(name)
}

// HasDatabase は sql.DatabaseProvider の実装。
func (p *Provider) HasDatabase(_ *sql.Context, name string) bool {
	_, ok := p.database(name)
	return ok
}

// AllDatabases は sql.DatabaseProvider の実装。すべてのブランチのデータベースを返す。
func (p *Provider) AllDatabases(*sql.Context) []sql.Database {
	var dbs []sql.Database
	for _, b := range p.st.Branches() {
		dbs = append(dbs, &Database{name: p.dbName(b), st: p.st, branch: b.ID})
	}
	return dbs
}

// branchOf は、データベース名のブランチ ID を返す。分からなければ main。
func (p *Provider) branchOf(name string) uint64 {
	if db, ok := p.database(name); ok {
		return db.branch
	}
	return store.MainBranchID
}

// ExternalStoredProcedure は sql.ExternalStoredProcedureProvider の実装。
func (p *Provider) ExternalStoredProcedure(_ *sql.Context, name string, numOfParams int) (*sql.ExternalStoredProcedureDetails, error) {
	proc, err := p.procs.LookupByNameAndParamCount(name, numOfParams) // 登録の写し
	if proc != nil {
		proc.AdminOnly = p.adminOnly(proc.Name)
	}
	return proc, err
}

// ExternalStoredProcedures は sql.ExternalStoredProcedureProvider の実装。
func (p *Provider) ExternalStoredProcedures(_ *sql.Context, name string) ([]sql.ExternalStoredProcedureDetails, error) {
	return p.procs.LookupByName(name) // SHOW CREATE PROCEDURE だけが使う（AdminOnly は見ない）
}
