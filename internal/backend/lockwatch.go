package backend

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 書き込みのロックの持ち主（docs/plan.md の段階 82）。
//
// 書き込むトランザクションはブランチごとに 1 つで、ロックを COMMIT か ROLLBACK まで持つ。トランザクションの中で
// 何もしないまま止まった接続（PHP がエラーで止まり、接続だけが残るなど）は、ロックを持ち続け、ほかの書き込みを
// -lock-wait-timeout ごとにエラー 1205 にし続ける（ホスティングの本番で 3 時間）。持ち主を指標、heimdb_status、1205 のときの
// ログに出し、何もしないまま -idle-lock-timeout より長く持つ接続を切る（切るとセッションの終わりでロールバックし、
// ロックが外れる）。見張りは、ストアのロックの持ち主の記録と、エンジンの接続の一覧（ProcessList）だけを読む。

// lockWatchStats は、見張りの数（指標）。
type lockWatchStats struct {
	idleKills  atomic.Uint64
	lastLogged atomic.Int64 // 1205 のときに持ち主をログに出した時刻（Unix ナノ秒。間隔を空ける）
}

// lockHolderInfo は、書き込みのロックの持ち主の今の様子。
type lockHolderInfo struct {
	branch string
	holder store.LockHolder
	held   time.Duration
	// state は、接続なら running（文を実行中）か idle（トランザクションの中で何もしていない）。内部の操作なら空。
	state string
	idle  time.Duration
	user  string
	last  string // 最後に終わった文（隠すか畳んだもの）
}

func (h lockHolderInfo) String() string {
	who := h.holder.Owner.Op
	if c := h.holder.Owner.Conn; c != 0 {
		who = fmt.Sprintf("connection %d", c)
		if h.user != "" {
			who += " (" + h.user + ")"
		}
		if h.holder.Owner.Op != "" {
			who += " " + h.holder.Owner.Op
		}
	}
	s := fmt.Sprintf("the write lock of branch %s is held by %s for %s", h.branch, who, h.held.Round(time.Second))
	switch h.state {
	case "idle":
		s += fmt.Sprintf(", idle for %s", h.idle.Round(time.Second))
	case "running":
		s += ", running a statement"
	}
	if h.last != "" {
		s += ", last statement: " + h.last
	}
	return s
}

// lockHolders は、書き込みのロックの持ち主と、ブランチ名ごとの待っている文の数を返す（ブランチ名の順）。
func (p *Provider) lockHolders() ([]lockHolderInfo, map[string]int) {
	holders, waiters := p.st.LockHolders()
	names := map[uint64]string{}
	for _, b := range p.st.Branches() {
		names[b.ID] = b.Name
	}
	name := func(id uint64) string {
		if n, ok := names[id]; ok {
			return n
		}
		if store.IsHiddenBranch(id) {
			return "temporary"
		}
		return strconv.FormatUint(id, 10)
	}
	var procs map[uint32]sql.Process
	if p.Processes != nil {
		procs = map[uint32]sql.Process{}
		for _, pr := range p.Processes() {
			procs[pr.Connection] = pr
		}
	}
	now := time.Now()
	var out []lockHolderInfo
	for _, h := range holders {
		info := lockHolderInfo{branch: name(h.Branch), holder: h, held: now.Sub(h.Since)}
		if c := h.Owner.Conn; c != 0 {
			if pr, ok := procs[c]; ok {
				info.user = pr.User
				switch pr.Command {
				case sql.ProcessCommandSleep:
					info.state, info.idle = "idle", now.Sub(pr.StartedAt)
				case sql.ProcessCommandQuery:
					info.state = "running"
				}
			}
			if v, ok := p.sessions.Load(c); ok {
				if q := v.(*Session).lastQuery.Load(); q != nil {
					info.last = p.logText(*q, 200)
				}
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b lockHolderInfo) int {
		if a.branch < b.branch {
			return -1
		} else if a.branch > b.branch {
			return 1
		}
		return 0
	})
	byName := map[string]int{}
	for b, n := range waiters {
		byName[name(b)] = n
	}
	return out, byName
}

// logText は、ログに出す文。-slow-query-redact なら値を畳み、そうでなければパスワードを含みうる文を隠す。
func (p *Provider) logText(q string, limit int) string {
	if p.SlowQueryRedact {
		q = redactQuery(q)
	} else {
		q = hidePassword(q)
	}
	if len(q) > limit {
		q = q[:limit] + "..."
	}
	return q
}

// lockWaitLogInterval は、1205 のときに持ち主をログに出す間隔（詰まっている間、1 秒に何件も出さない）。
const lockWaitLogInterval = 10 * time.Second

// logLockHoldersOnTimeout は、書き込みがロックを待ちきれなかった（1205）ときに、持ち主をログに出す。
func (p *Provider) logLockHoldersOnTimeout() {
	now := time.Now().UnixNano()
	last := p.lockStats.lastLogged.Load()
	if now-last < int64(lockWaitLogInterval) || !p.lockStats.lastLogged.CompareAndSwap(last, now) {
		return
	}
	holders, _ := p.lockHolders()
	if len(holders) == 0 {
		logrus.Warn("HeimDB: lock wait timeout; the write lock is free now")
		return
	}
	for _, h := range holders {
		logrus.Warnf("HeimDB: lock wait timeout; %s", h)
	}
}

// WatchIdleLocks は、何もしないまま書き込みのロックを IdleLockTimeout より長く持つ接続を切る見張りを始める。Shutdown
// で止める。
func (p *Provider) WatchIdleLocks() {
	if p.IdleLockTimeout <= 0 || p.KillConnection == nil || p.Processes == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stopWatch = cancel
	every := min(max(p.IdleLockTimeout/4, 100*time.Millisecond), time.Second)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.killIdleLockHolders()
			}
		}
	}()
}

// killIdleLockHolders は、何もしないまま書き込みのロックを IdleLockTimeout より長く持つ接続を切る。
func (p *Provider) killIdleLockHolders() {
	holders, _ := p.lockHolders()
	for _, h := range holders {
		if h.holder.Owner.Conn == 0 || h.state != "idle" || h.idle < p.IdleLockTimeout || h.held < p.IdleLockTimeout {
			continue
		}
		logrus.Warnf("HeimDB: closing a connection idle in a transaction with the write lock (-idle-lock-timeout %s); %s", p.IdleLockTimeout, h)
		if err := p.KillConnection(h.holder.Owner.Conn); err != nil {
			logrus.Warnf("HeimDB: close connection %d: %v", h.holder.Owner.Conn, err)
			continue
		}
		p.lockStats.idleKills.Add(1)
	}
}
