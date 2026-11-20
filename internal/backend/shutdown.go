package backend

import (
	"sync"
	"time"

	"github.com/dolthub/vitess/go/mysql"
)

// 停止の手順（docs/plan.md の段階 18）。
//
// go-mysql-server の Server.Close は新しい接続の受け付けを止めるだけで、実行中の文も、つながったままの接続の次の文も
// 待たない。その後で Pebble を閉じると、実行中の書き込みや切断時の後片付けが閉じた Pebble に触って panic する。
// そこで、接続ごとのコマンドの始まりと終わりで実行中の数を数え、停止が始まったら新しいコマンドを拒否する。

// gate は、実行中のコマンドを数え、停止の後は新しいコマンドを拒否する。
type gate struct {
	mu     sync.Mutex
	active int
	closed bool
}

// errShutdown は MySQL のエラー 1053（ER_SERVER_SHUTDOWN）。
var errShutdown = mysql.NewSQLError(1053, "08S01", "Server shutdown in progress")

// enter は、コマンドを始める。停止が始まっていればエラー。始めたら leave を呼ぶ。
func (g *gate) enter() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errShutdown
	}
	g.active++
	return nil
}

// leave は、enter で始めたコマンドを終える。
func (g *gate) leave() {
	g.mu.Lock()
	g.active--
	g.mu.Unlock()
}

// Shutdown は、新しいコマンドを拒否し、実行中のコマンドが終わるのを timeout まで待つ。すべて終われば true（その後は
// ストアを閉じてよい）。待ちきれなければ false（ストアを閉じずにプロセスを終える。コミット済みのデータは同期書き込み
// 済みなので、クラッシュと同じで安全）。
func (p *Provider) Shutdown(timeout time.Duration) bool {
	g := &p.gate
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	if p.stopWarm != nil {
		p.stopWarm()
	}
	if p.stopRebuild != nil {
		p.stopRebuild()
	}
	if p.stopWatch != nil {
		p.stopWatch()
	}
	// 次の文を拒否された接続のトランザクションは、書き込みロックを持ったまま残る。それを待つ文を待たせ続けない。
	p.st.StopLockWaits()
	deadline := time.Now().Add(timeout)
	for {
		g.mu.Lock()
		active := g.active
		g.mu.Unlock()
		if active == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
