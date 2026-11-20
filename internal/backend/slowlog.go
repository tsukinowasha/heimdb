package backend

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dolthub/vitess/go/vt/sqlparser"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 遅い文のログ（docs/plan.md の段階 19、段階 81）。
//
// 閾値はグローバル変数の slow_query_log と long_query_time（秒）で決め、管理者が SET GLOBAL で止めずに変えられる
// （-slow-query は起動時の値。SetSlowQuery）。接続ごとの long_query_time は読まない（サイトの PHP が自分の接続で
// ログをあふれさせないように）。ログには、文が読んだ行（本体の行とインデックスのエントリ）、使ったインデックス、
// 待った時間（書き込みのロック、同期のレプリカの知らせ、表の統計を作るのを待った時間）を添える。

// SetSlowQuery は、遅い文の閾値の起動時の値を、グローバル変数に入れる。d が 0 なら遅い文をログに出さない。
func SetSlowQuery(d time.Duration) error {
	ctx := sql.NewEmptyContext()
	if d <= 0 {
		return sql.SystemVariables.SetGlobal(ctx, "slow_query_log", int8(0))
	}
	if err := sql.SystemVariables.SetGlobal(ctx, "long_query_time", d.Seconds()); err != nil {
		return err
	}
	return sql.SystemVariables.SetGlobal(ctx, "slow_query_log", int8(1))
}

// slowThreshold は、遅い文の閾値と、遅い文をログに出すかどうかを返す。
func slowThreshold() (time.Duration, bool) {
	_, on, _ := sql.SystemVariables.GetGlobal("slow_query_log")
	if v, ok := on.(int8); !ok || v == 0 {
		return 0, false
	}
	_, secs, _ := sql.SystemVariables.GetGlobal("long_query_time")
	f, ok := secs.(float64)
	if !ok {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// readStats は、1 つの文が読んだ行の数と、使ったインデックス（「表.インデックス」。主キーは PRIMARY）。走査は
// エンジンの別のゴルーチンからも読むので、数は atomic、インデックスは mu で守る。
type readStats struct {
	rows    atomic.Int64
	mu      sync.Mutex
	indexes []string
}

func (r *readStats) reset() {
	r.rows.Store(0)
	r.mu.Lock()
	r.indexes = r.indexes[:0]
	r.mu.Unlock()
}

// noteIndex は、文が表 table のインデックス index を読み始めたことを記録する。
func (r *readStats) noteIndex(table, index string) {
	name := table + "." + index
	r.mu.Lock()
	if !slices.Contains(r.indexes, name) {
		r.indexes = append(r.indexes, name)
	}
	r.mu.Unlock()
}

func (r *readStats) indexList() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.indexes, ",")
}

// statementReads は、ctx の文の readStats を返す（HeimDB のセッションでなければ nil）。
func statementReads(ctx *sql.Context) *readStats {
	if ctx == nil {
		return nil
	}
	if s, ok := ctx.Session.(*Session); ok {
		return &s.cmdRead
	}
	return nil
}

// logSlow は、時間 d かかった今の文を、遅い文としてログに出す。
func (s *Session) logSlow(d time.Duration) {
	q := s.cmdQuery
	if s.pro.SlowQueryRedact {
		q = redactQuery(q)
	} else {
		q = hidePassword(q)
	}
	if len(q) > 1000 {
		q = q[:1000] + "..."
	}
	fields := logrus.Fields{
		"connectionID":  s.ID(),
		"user":          s.Client().User,
		"database":      s.GetCurrentDatabase(),
		"duration":      logDuration(d),
		"rows_examined": s.cmdRead.rows.Load(),
		"indexes":       s.cmdRead.indexList(),
	}
	for name, w := range map[string]time.Duration{"lock_wait": s.cmdWaits.Lock, "replica_wait": s.cmdWaits.Ack, "stats_wait": s.cmdStatsWait} {
		if w > 0 {
			fields[name] = logDuration(w)
		}
	}
	logrus.WithFields(fields).Warnf("slow query: %s", q)
}

// logDuration は、遅い文のログの時間の書き方。1 µs 刻み（312µs、1.234ms。小さい文の差を測れるように。段階 84）。
func logDuration(d time.Duration) string {
	return d.Round(time.Microsecond).String()
}

// redactedArg と redactedList は、RedactSQLQuery が値と値の並び（IN (1, 2, 3)）の代わりに置く引数の名前。
var (
	redactedArg  = regexp.MustCompile(`:redacted[0-9]+`)
	redactedList = regexp.MustCompile(`::redacted[0-9]+`)
)

// redactQuery は、文の値（文字列、数、16 進）を ? に畳む（-slow-query-redact。訪問者が入れた検索語やフォームの値を
// ログに残さない）。構文解析できない文は、先頭の 2 語だけにする。
func redactQuery(q string) string {
	r, err := sqlparser.RedactSQLQuery(q)
	if err != nil {
		words := strings.Fields(q)
		if len(words) > 2 {
			words = words[:2]
		}
		return strings.Join(words, " ") + " ... (text hidden)"
	}
	// RedactSQLQuery は値を :redacted1、値の並びを ::redacted2 のような名前の付いた引数にし、文を小文字で書き直す。
	return redactedArg.ReplaceAllString(redactedList.ReplaceAllString(r, "(?)"), "?")
}
