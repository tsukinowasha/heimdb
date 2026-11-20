package backend

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// WarmStats は、起動した後に main のすべての表の行数と統計を裏で作り始める（docs/plan.md の段階 80）。統計は表を
// 初めて使う文が作るので、作るまで、その文は表を全部読むのを待つ（ホスティングで、起動し直した直後の 311 万行の wp_postmeta
// の最初の SELECT が 4.9 秒）。作っている表を文が使い始めたら、文はそれが終わるのを待つ（rowCountCache.build）。
// ストアを読む間（表の一覧と、1 つの表を作る間）は実行中のコマンドとして数え、停止が始まったら作るのをやめる。
// セッションは接続として数えない（NewSession を通さない）。
//
// 作り終えたかは heimdb_status の stats と指標の heimdb_stats_ready に出す（段階 106）。起動したときの 1 回が終わった
// ことを表し、その後の書き込みや ALTER で統計を作り直すときは戻さない（それは heimdb_stats_wait_seconds_total に出る）。
func (p *Provider) WarmStats() {
	ctx, cancel := context.WithCancel(context.Background())
	p.stopWarm = cancel
	p.warm.set(func(w *warmState) { w.state = "building" })
	go func() {
		start := time.Now()
		sctx := sql.NewContext(ctx, sql.WithSession(&Session{BaseSession: sql.NewBaseSession(), st: p.st, pro: p}))
		db, ok := p.database(p.base)
		if !ok || p.gate.enter() != nil {
			return
		}
		names, err := db.GetTableNames(sctx)
		p.gate.leave()
		if err != nil {
			logrus.Warnf("statistics: %v", err)
			p.warm.set(func(w *warmState) { w.state, w.err = "failed", err.Error() })
			return
		}
		n, failed := 0, 0
		for _, name := range names {
			if p.gate.enter() != nil {
				return
			}
			err := p.warmTable(sctx, db, name)
			p.gate.leave()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// 失敗した表は、それを使う最初の文が作る。
				logrus.Warnf("statistics: table %s: %v", name, err)
				failed++
				continue
			}
			n++
		}
		d := time.Since(start)
		p.warm.set(func(w *warmState) {
			w.state, w.readyAt, w.duration, w.built, w.failed = "ready", time.Now(), d, n, failed
		})
		logrus.Infof("statistics: built for %d tables in %s", n, d.Round(time.Millisecond))
	}()
}

// warmState は、起動した後に裏で統計を作る様子。state は、WarmStats の前は空、作っている間は building、作り終えたら
// ready、表の一覧を読めなければ failed。停止で途中でやめたときは building のまま。
type warmState struct {
	mu       sync.Mutex
	state    string
	readyAt  time.Time
	duration time.Duration
	built    int
	failed   int
	err      string
}

func (w *warmState) set(f func(*warmState)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

// snapshot は、今の様子の写しを返す（mu は写さない）。
func (w *warmState) snapshot() (state string, readyAt time.Time, duration time.Duration, built, failed int, err string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state, w.readyAt, w.duration, w.built, w.failed, w.err
}

func (p *Provider) warmTable(ctx *sql.Context, db sql.Database, name string) error {
	tbl, ok, err := db.GetTableInsensitive(ctx, name)
	if err != nil || !ok {
		return err
	}
	if t := unwrapTable(tbl); t != nil {
		_, err = rowCounts.stats(ctx, t)
	}
	return err
}
