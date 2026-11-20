package backend

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// ブランチの上限（docs/plan.md の段階 45）。検証環境のブランチが、本番と同じ HeimDB のディスクと時間を食い尽くさない
// ようにする。
//
//	CALL heimdb_branch_limit('stg1', '2G', '30s');  -- 大きさの上限と、1 つの文の実行時間の上限
//	CALL heimdb_branch_limit('stg1', '0', '0');     -- 外す
//
// 大きさの上限は、書き込みの始まり（エディタが文の中で最初に書く前、DDL の前）に確かめる。始まった文は止めない
// （実行時間の上限で抑える）。表を消す DDL（DROP TABLE、TRUNCATE）は、満杯でも通す。コミットしていない書き込みは
// メモリに持つので、上限の付いたブランチに書くトランザクションの大きさも抑える（txnLimit）。
//
// 実行時間の上限は、認証が有効なら、ユーザーが権限を持つデータベースのブランチの上限の最小に従う（接続中のデータベース
// だけで決めると、データベースを指定せずにつないで完全な名前で書けば外せる）。管理者（SUPER 権限。手続きを呼べる
// ユーザーと同じ判定。段階 63）には効かない。認証のないときは、接続中のデータベースのブランチに従い、heimdb_* の手続きには効かない。接続ごとの設定
// （max_execution_time）にしないのは、信頼しない PHP が SET で外せるため。

func (p *Provider) branchLimit(ctx *sql.Context, name, size, timeout string) (sql.RowIter, error) {
	maxBytes, err := parseSize(size)
	if err != nil {
		return nil, err
	}
	maxExec, err := parseTimeLimit(timeout)
	if err != nil {
		return nil, err
	}
	b, err := p.st.SetBranchLimits(name, maxBytes, maxExec)
	if err != nil {
		return nil, err
	}
	row, err := p.branchRow(ctx, b)
	if err != nil {
		return nil, err
	}
	return &rowIter{rows: []sql.Row{row}}, nil
}

// parseSize は、'2G'、'500M'、'64K'、'1048576' のような大きさ（1024 倍の単位）をバイトにする。'0' は上限なし。
func parseSize(s string) (uint64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	mult := uint64(1)
	switch {
	case strings.HasSuffix(t, "K"):
		mult = 1 << 10
	case strings.HasSuffix(t, "M"):
		mult = 1 << 20
	case strings.HasSuffix(t, "G"):
		mult = 1 << 30
	case strings.HasSuffix(t, "T"):
		mult = 1 << 40
	}
	if mult > 1 {
		t = t[:len(t)-1]
	}
	n, err := strconv.ParseUint(t, 10, 64)
	if err != nil || n > (1<<63)/mult {
		return 0, fmt.Errorf("HeimDB: invalid size %q (use a number of bytes or K, M, G, T, such as '2G'; '0' for no limit)", s)
	}
	return n * mult, nil
}

// parseTimeLimit は、'30s'、'500ms'、'2m' のような時間をミリ秒にする。'0' は上限なし。
func parseTimeLimit(s string) (uint64, error) {
	t := strings.TrimSpace(s)
	if t == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(t)
	if err != nil || d < time.Millisecond {
		return 0, fmt.Errorf("HeimDB: invalid time limit %q (use ms, s or m, such as '30s'; '0' for no limit)", s)
	}
	return uint64(d / time.Millisecond), nil
}

// checkBranchSize は、ブランチ branch が大きさの上限を超えていれば、MySQL で表領域が尽きたときと同じエラー 1114 を返す。
func checkBranchSize(st *store.Store, branch uint64, table string) error {
	over, usage, limit, err := st.BranchOverSize(branch)
	if err != nil || !over {
		return err
	}
	if warnOnce(branch) {
		name := fmt.Sprint(branch)
		if b, ok := st.Branch(branch); ok {
			name = b.Name
		}
		logrus.Warnf("HeimDB: branch %s is over its size limit (%d of %d bytes); writes are refused", name, usage, limit)
	}
	return mysql.NewSQLError(mysql.ERRecordFileFull, mysql.SSUnknownSQLState, "The table '%s' is full", table)
}

// warnInterval は、上限で拒否したことをログに出す間隔（ブランチごと）。上限に当たった検証環境で、WordPress が書き込みを
// 繰り返してもログを埋めないようにする。
const warnInterval = time.Minute

var (
	warnedMu sync.Mutex
	warnedAt = map[uint64]time.Time{}
)

// warnOnce は、ブランチ branch について、warnInterval の間に初めて呼ばれたときだけ true を返す。
func warnOnce(branch uint64) bool {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if t, ok := warnedAt[branch]; ok && time.Since(t) < warnInterval {
		return false
	}
	warnedAt[branch] = time.Now()
	return true
}

// maxTxnBytes は、上限の付いたブランチに書くトランザクションの大きさの上限の最大。
const maxTxnBytes = 256 << 20

// txnLimit は、ブランチ branch に書くトランザクションの大きさ（書き込みのバッチ）の上限を返す。大きさの上限がなければ 0。
func txnLimit(st *store.Store, branch uint64) int {
	b, ok := st.Branch(branch)
	if !ok || b.MaxBytes == 0 {
		return 0
	}
	return int(min(b.MaxBytes, maxTxnBytes))
}

// errTxnTooLarge は MySQL のエラー 1197（ER_TRANS_CACHE_FULL）。
var errTxnTooLarge = mysql.NewSQLError(1197, mysql.SSUnknownSQLState, "Multi-statement transaction required more than 'max_binlog_cache_size' bytes of storage; increase this mysqld variable and try again")

// errLimitedTemporaryTable は、上限の付いたブランチを使う接続で一時テーブルを作ろうとしたときのエラー。一時テーブルは
// 上限のない隠しブランチに置くので、大きさの上限を外せてしまう。
var errLimitedTemporaryTable = mysql.NewSQLError(mysql.ERNotSupportedYet, mysql.SSUnknownSQLState, "HeimDB: temporary tables are not supported on a connection to a branch with limits")

// sessionLimits は、この文に効く実行時間の上限 execMS（0 なら上限なし）と、上限の付いたブランチを使う接続かどうか
// （limited。一時テーブルを拒否する）を返す。
func (s *Session) sessionLimits(ctx *sql.Context) (execMS uint64, limited bool) {
	if s.pro == nil {
		return 0, false
	}
	typ := stmtType(ctx.Query())
	add := func(dbName string) {
		b, ok := s.st.Branch(s.pro.branchOf(dbName))
		if !ok || (b.MaxBytes == 0 && b.MaxExecutionMS == 0) {
			return
		}
		// 一時テーブルを拒否するのは大きさの上限のときだけ（上限の外の隠しブランチに書けば大きさの上限を外せるため）。
		limited = limited || b.MaxBytes != 0
		// main の実行時間の上限は、SELECT と書き込みの文（INSERT、UPDATE、DELETE、REPLACE）にだけ効かせる（段階 82）。
		// プラグインの更新の大きな ALTER TABLE や手続きを止めると、更新や操作が終わらなくなる。
		if b.ID == store.MainBranchID && typ > 4 {
			return
		}
		if b.MaxExecutionMS > 0 && (execMS == 0 || b.MaxExecutionMS < execMS) {
			execMS = b.MaxExecutionMS
		}
	}
	if users := s.pro.users; users != nil && users.Enabled() {
		ps := users.UserActivePrivilegeSet(ctx)
		if ps.Has(sql.PrivilegeType_Super) {
			return 0, false // 管理者
		}
		for _, db := range ps.GetDatabases() {
			add(db.Name())
		}
		add(ctx.GetCurrentDatabase())
		return execMS, limited
	}
	add(ctx.GetCurrentDatabase())
	if isHeimDBCall(ctx.Query()) {
		execMS = 0
	}
	return execMS, limited
}

// sessionMaxExecutionTime は、この文に効く接続ごとの max_execution_time（ミリ秒。0 なら上限なし）を返す
// （docs/plan.md の段階 81）。MySQL と同じく SELECT の文（WITH ... SELECT、かっこで始まる UNION を含む）にだけ効き、
// INSERT ... SELECT、UPDATE、SET、DO の中の SELECT には効かない。管理者にも効く。ブランチの上限より長い値で上限を
// 外すことはできない（短い方に従う）。
func sessionMaxExecutionTime(ctx *sql.Context) uint64 {
	if stmtType(ctx.Query()) != 0 {
		return 0
	}
	v, err := ctx.GetSessionVariable(ctx, "max_execution_time")
	if err != nil {
		return 0
	}
	switch ms := v.(type) {
	case int64:
		if ms > 0 {
			return uint64(ms)
		}
	case uint64:
		return ms
	}
	return 0
}

// isHeimDBCall は、クエリが HeimDB の手続きを呼ぶ文（CALL heimdb_...）かどうかを、先頭の語で判定する。
func isHeimDBCall(q string) bool {
	q = skipSpaceAndComments(q)
	if len(q) < 5 || !strings.EqualFold(q[:4], "CALL") {
		return false
	}
	q = skipSpaceAndComments(q[4:])
	return len(q) >= 7 && strings.EqualFold(q[:7], "heimdb_")
}

// errQueryTimeout は MySQL のエラー 3024（ER_QUERY_TIMEOUT）。
var errQueryTimeout = mysql.NewSQLError(3024, mysql.SSUnknownSQLState, "Query execution was interrupted, maximum statement execution time exceeded")

// startTimeLimit は、実行時間の上限 execMS（ミリ秒。0 なら上限なし）を過ぎたら文を止める（KILL QUERY と同じく、文の
// context を取り消す）時計を動かす。CommandEnd で止める。
func (s *Session) startTimeLimit(ctx *sql.Context, execMS uint64) {
	if execMS == 0 || ctx.ProcessList == nil {
		return
	}
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	if s.limitTimer != nil {
		return
	}
	gen, pl, id := s.limitGen, ctx.ProcessList, s.ID()
	s.limitTimer = time.AfterFunc(time.Duration(execMS)*time.Millisecond, func() {
		s.limitMu.Lock()
		defer s.limitMu.Unlock()
		if s.limitGen != gen {
			return // 文はもう終わった
		}
		s.timedOut = true
		pl.Kill(id)
	})
}

// stopTimeLimit は、文の終わりに時計を止める。
func (s *Session) stopTimeLimit() {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	if s.limitTimer != nil {
		s.limitTimer.Stop()
		s.limitTimer = nil
	}
	s.limitGen++
	s.timedOut = false
}

// TranslateQueryError は server.QueryErrorTranslator の実装。接続先に返すエラーを、文の数の指標のために覚える
// （docs/plan.md の段階 73）。
func (s *Session) TranslateQueryError(err error) error {
	err = s.translateQueryError(err)
	s.cmdErr = err
	return err
}

// translateQueryError は、実行時間の上限で止めた文のエラー（取り消し）を、MySQL の
// max_execution_time と同じエラー 3024 にし、その文の書き込みを取り消す。文が書き終えた後に止まると、go-mysql-server は
// 文の取り消し（DiscardChanges）を呼ばないので、明示的なトランザクションに書き込みが残ってしまう（自動コミットの文は
// CommandEnd がロールバックする）。
func (s *Session) translateQueryError(err error) error {
	if s.discardErr != nil {
		// 文の取り消しがメモリの上限で止まり、トランザクションごと取り消した（editor.DiscardChanges）。元のエラー
		// （重複など）より、トランザクションが消えたことを伝える。
		err, s.discardErr = s.discardErr, nil
	}
	if isOutOfMemory(err) {
		// メモリの上限で止めた文は、トランザクションごと取り消してメモリとロックを手放す（文だけの取り消しでは、
		// 使わなくなったバッチをトランザクションの終わりまで持つ。docs/plan.md の段階 55）。
		s.abortTransaction()
		memoryReleased()
		// マージや復元が包んだエラーも、接続先には 1041 のまま返す（包んだままだと 1105 になる。段階 58）。
		var sqlErr *mysql.SQLError
		errors.As(sql.UnwrapError(err), &sqlErr)
		return sqlErr
	}
	if store.IsTxnTooLarge(sql.UnwrapError(err)) {
		// 書き込みの上限で止めた文（docs/plan.md の段階 59）。マージや復元が包んだエラーも 1197 のまま返す。文の書き込みは
		// 取り消してあり、トランザクションは残す（COMMIT か ROLLBACK は接続先が選ぶ）。
		var sqlErr *mysql.SQLError
		errors.As(sql.UnwrapError(err), &sqlErr)
		return sqlErr
	}
	if sqlErr, ok := asRefusal(err); ok {
		// マージを拒否する理由の番号（段階 60）。包んだままだと 1105 になる。
		return sqlErr
	}
	if errors.Is(sql.UnwrapError(err), store.ErrLockWaitTimeout) {
		s.pro.logLockHoldersOnTimeout() // 誰が書き込みのロックを持っているか（段階 82）
	}
	s.limitMu.Lock()
	timedOut := s.timedOut
	s.limitMu.Unlock()
	if !timedOut {
		return err
	}
	var sqlErr *mysql.SQLError
	if errors.As(err, &sqlErr) && sqlErr.Num != mysql.ERUnknownError {
		return err // 取り消しより前に起きた別のエラー
	}
	if rerr := s.rollbackCommand(); rerr != nil {
		return rerr
	}
	return errQueryTimeout
}

// interruptEvery は、長い読み書きの繰り返しで、文が止められた（実行時間の上限、KILL QUERY）かを確かめる間隔。
const interruptEvery = 1024

// interrupted は、n 回目の繰り返しで、interruptEvery 回に 1 回、文の context が取り消されているか、メモリの上限に
// 近づいていれば（checkMemory）そのエラーを返す。
func interrupted(ctx *sql.Context, n int) error {
	if n%interruptEvery != 0 {
		return nil
	}
	if err := checkMemory(); err != nil {
		return err
	}
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
