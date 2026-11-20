package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 起動の後に、main の水準の古いインデックスを裏で作り直す（docs/plan.md の段階 121）。0.2.38 以前に作った DECIMAL、
// FLOAT、DOUBLE、ENUM の列を含むインデックスと、0.2.40 以前に作った SET と BIT の列を含むインデックスはエントリを
// 持たず、引くたびに全件を読む（staleIndexes）。heimdb_rebuild_indexes() と同じ書き込み（raiseKeyLevels）を、
// インデックスを 1 つずつ、ロックを放しながら行う。
//
// 形は WarmStats（warm.go）と同じ。ストアを読み書きする間（一覧を読む間と、1 つのインデックスを作る間）は実行中の
// コマンドとして数え（gate）、停止が始まったら（stopRebuild、ErrShuttingDown）静かにやめる。作りかけのエントリは
// raiseKeyLevels が消す（消し切れなければ、次の起動で消える）。レプリカでは動かさない（本番の作ったエントリが記録で
// 届く）。ブランチは作り直さない（利用者が手続きを呼ぶ）。

// 裏の作り直しの上限（テストで小さくする）。rebuildLockWait は書き込みのロックを待つ時間（長いトランザクションが持って
// いれば飛ばし、次の起動でやり直す）。rebuildMaxRows と rebuildMaxUniqueRows は、見積もった行の数がこれを超える
// インデックスを飛ばす上限（作る間、main への書き込みを待たせるので。macOS で 100 万行の一意でないインデックスが
// 1.0〜1.7 秒、一意のインデックスが 5.4 秒）。
var (
	rebuildLockWait      = 5 * time.Second
	rebuildMaxRows       = uint64(5_000_000)
	rebuildMaxUniqueRows = uint64(1_000_000)
)

// rebuildOp は、裏の作り直しが持つ書き込みのロックの持ち主の名前（heimdb_status の write_lock）。
const rebuildOp = "rebuild indexes"

// rebuildState は、裏の作り直しの様子。state は、動かさなければ空（heimdb_status では off）、作っている間は running、
// 1 回を終えたら done。停止で途中でやめたときは running のまま。数と時間と誤りは最後の 1 回のもの。
type rebuildState struct {
	mu sync.Mutex
	// ctx は、StartIndexRebuild で動かすと決めたときの文脈（停止で取り消す）。nil なら動かさない。
	ctx context.Context
	// running は、ゴルーチンが動いているか。again は、動いている間にもう 1 回を頼まれたか（main の復元の後）。
	running, again bool
	state          string
	doneAt         time.Time
	duration       time.Duration
	rebuilt        int
	skipped        int
	err            string
}

// rebuildSnapshot は、rebuildState の写し（mu を写さない）。
type rebuildSnapshot struct {
	state            string
	running          bool
	doneAt           time.Time
	duration         time.Duration
	rebuilt, skipped int
	err              string
}

func (r *rebuildState) snapshot() rebuildSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return rebuildSnapshot{state: r.state, running: r.running, doneAt: r.doneAt, duration: r.duration, rebuilt: r.rebuilt, skipped: r.skipped, err: r.err}
}

// StartIndexRebuild は、起動時の古いインデックスの警告を出し、enabled（-rebuild-indexes）で本番なら、main の古い
// インデックスを裏で作り直し始める（cmd/heimdb が、接続を受け付け始めるときに WarmStats と並べて呼ぶ）。
func (p *Provider) StartIndexRebuild(enabled bool) {
	on := enabled && !p.st.IsReplica()
	p.logStaleIndexes(on)
	if !on {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stopRebuild = cancel
	p.rebuild.mu.Lock()
	p.rebuild.ctx = ctx
	p.rebuild.mu.Unlock()
	p.kickIndexRebuild()
}

// kickIndexRebuild は、裏の作り直しを 1 回動かす（動いていれば、終わった後にもう 1 回動かす）。StartIndexRebuild で
// 動かすと決めていなければ何もしない。main を復元した後にも呼ぶ（戻した定義に古いインデックスがありうるので）。
func (p *Provider) kickIndexRebuild() {
	r := &p.rebuild
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil || r.ctx.Err() != nil {
		return
	}
	if r.running {
		r.again = true
		return
	}
	r.running, r.state = true, "running"
	go p.runIndexRebuild(r.ctx)
}

func (p *Provider) runIndexRebuild(ctx context.Context) {
	r := &p.rebuild
	for {
		p.rebuildPass(ctx)
		r.mu.Lock()
		if !r.again || ctx.Err() != nil {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.again, r.state = false, "running"
		r.mu.Unlock()
	}
}

// rebuildJob は、作り直す 1 つのインデックス（表の ID とインデックスの ID で指す。名前は表示のため）。
type rebuildJob struct {
	table   string
	tableID uint64
	index   string
	indexID uint64
}

// rebuildPass は、main の今の定義の古いインデックスを 1 つずつ作り直す。
func (p *Provider) rebuildPass(ctx context.Context) {
	start := time.Now()
	sctx := sql.NewContext(ctx, sql.WithSession(&Session{BaseSession: sql.NewBaseSession(), st: p.st, pro: p}))
	if p.gate.enter() != nil {
		return
	}
	jobs, err := p.staleMainIndexes()
	p.gate.leave()
	if err != nil {
		logrus.Warnf("HeimDB: rebuild indexes: %v", err)
		p.finishRebuildPass(start, 0, 0, err.Error())
		return
	}
	if len(jobs) == 0 {
		p.finishRebuildPass(start, 0, 0, "")
		return
	}
	logrus.Infof("HeimDB: rebuilding %d indexes on %s made by an older HeimDB in the background, one at a time", len(jobs), p.base)
	db := &Database{name: p.base, st: p.st, branch: store.MainBranchID, ddlOp: rebuildOp, ddlWait: rebuildLockWait}
	rebuilt, skipped, lastErr := 0, 0, ""
	for _, j := range jobs {
		if p.gate.enter() != nil {
			return
		}
		t0 := time.Now()
		rows, built, err := p.rebuildOneIndex(sctx, db, j)
		p.gate.leave()
		if ctx.Err() != nil || errors.Is(err, store.ErrShuttingDown) || errors.Is(err, errShutdown) {
			return // 停止。作りかけは raiseKeyLevels が消し、次の起動で続きを作る
		}
		if err != nil {
			skipped++
			reason := rebuildSkipReason(err)
			lastErr = fmt.Sprintf("%s.%s: %s", j.table, j.index, reason)
			logrus.Warnf("HeimDB: skipped rebuilding index %s.%s on %s: %s", j.table, j.index, p.base, reason)
			continue
		}
		if built {
			rebuilt++
			logrus.Infof("HeimDB: rebuilt index %s.%s on %s (about %d rows) in %s", j.table, j.index, p.base, rows, time.Since(t0).Round(time.Millisecond))
		}
	}
	d := p.finishRebuildPass(start, rebuilt, skipped, lastErr)
	if skipped > 0 {
		logrus.Warnf("HeimDB: rebuilt %d indexes on %s in %s; skipped %d (see the warnings above)", rebuilt, p.base, d.Round(time.Millisecond), skipped)
	} else {
		logrus.Infof("HeimDB: rebuilt %d indexes on %s in %s", rebuilt, p.base, d.Round(time.Millisecond))
	}
}

func (p *Provider) finishRebuildPass(start time.Time, rebuilt, skipped int, err string) time.Duration {
	d := time.Since(start)
	p.rebuild.mu.Lock()
	r := &p.rebuild
	r.state, r.doneAt, r.duration, r.rebuilt, r.skipped, r.err = "done", time.Now(), d, rebuilt, skipped, err
	p.rebuild.mu.Unlock()
	return d
}

// staleMainIndexes は、main の今の定義の古いインデックスを、表の名前の順に返す。
func (p *Provider) staleMainIndexes() ([]rebuildJob, error) {
	defs, err := p.st.ListTables(store.MainBranchID, store.Latest)
	if err != nil {
		return nil, err
	}
	var jobs []rebuildJob
	for _, def := range defs {
		stale, err := staleIndexes(def)
		if err != nil {
			logrus.Warnf("HeimDB: rebuild indexes: %v", err)
			continue
		}
		for _, i := range stale {
			jobs = append(jobs, rebuildJob{table: def.Name, tableID: def.ID, index: def.Indexes[i].Name, indexID: def.Indexes[i].ID})
		}
	}
	return jobs, nil
}

// errRebuildTooLarge は、見積もった行の数が裏の作り直しの上限を超えたこと。
type errRebuildTooLarge struct{ rows, limit uint64 }

func (e errRebuildTooLarge) Error() string {
	return fmt.Sprintf("about %d rows, more than %d for a background rebuild, which would hold the write lock too long", e.rows, e.limit)
}

// rebuildSkipReason は、飛ばした理由と、利用者がすることの文。
func rebuildSkipReason(err error) string {
	var large errRebuildTooLarge
	switch {
	case sql.ErrUniqueKeyViolation.Is(err):
		msg, ok := sql.DuplicateEntryMessage(err)
		if !ok {
			msg = err.Error()
		}
		return msg + "; fix the duplicate rows and run CALL heimdb_rebuild_indexes()"
	case errors.Is(err, store.ErrLockWaitTimeout):
		return fmt.Sprintf("the write lock was not free within %s (a transaction holds it); it is tried again at the next start, or run CALL heimdb_rebuild_indexes()", rebuildLockWait)
	case errors.As(err, &large):
		return large.Error() + "; run CALL heimdb_rebuild_indexes() at a quiet time"
	}
	return err.Error()
}

// rebuildOneIndex は、job のインデックスを作り直す。表が消えたか作り直されたか、インデックスがもう古くなければ何も
// しない（built が偽）。rows は見積もった行の数。
func (p *Provider) rebuildOneIndex(ctx *sql.Context, db *Database, j rebuildJob) (rows uint64, built bool, err error) {
	def, ok, err := p.st.GetTable(store.MainBranchID, j.table, store.Latest)
	if err != nil || !ok || def.ID != j.tableID {
		return 0, false, err
	}
	unique, found := false, false
	for _, idx := range def.Indexes {
		if idx.ID == j.indexID {
			unique, found = idx.Unique, true
		}
	}
	if !found {
		return 0, false, nil
	}
	t, err := newTable(ctx, db, def)
	if err != nil {
		return 0, false, err
	}
	if rows, err = rowCounts.get(ctx, t); err != nil {
		return 0, false, err
	}
	limit := rebuildMaxRows
	if unique {
		limit = rebuildMaxUniqueRows
	}
	if rows > limit {
		return rows, false, errRebuildTooLarge{rows: rows, limit: limit}
	}
	names, err := t.raiseKeyLevels(ctx, []uint64{j.indexID})
	if sql.ErrTableNotFound.Is(err) {
		return rows, false, nil // ロックを待つ間に消えたか作り直された
	}
	return rows, len(names) > 0, err
}
