package backend

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"

	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// Session は HeimDB の接続ごとの状態。実行中のトランザクションを持つ。
type Session struct {
	// rewriters は、この文で始めて、まだ終わっていない表の書き直し（ALTER TABLE。alter.go の rewriter）。
	rewriters []*rewriter
	// closers は、この文で開いて、まだ閉じていない走査などの後始末（scan.go の stmtCloser）。
	closers []*stmtCloser
	// discardErr は、この文の書き込みの取り消しがメモリの上限で止まり、トランザクションごと取り消したときのエラー
	// （editor.DiscardChanges）。go-mysql-server は取り消しのエラーを捨てるので、TranslateQueryError で返す。
	discardErr error
	// maintenance は、このコマンドで最後に構文解析した表の手入れの文の種類（maintenance.go。ANALYZE とほかの文なら ""）。
	// コマンドの始めに消す（準備した文の実行は構文解析を通らないので、前のコマンドの種類を使わない）。
	maintenance string
	*sql.BaseSession
	st  *store.Store
	pro *Provider
	txn *store.Txn
	// cmdStart と cmdQuery は、実行中のコマンドの始まりの時刻と文（遅い文のログに使う）。
	cmdStart time.Time
	cmdQuery string
	// cmdErr は、実行中のコマンドが接続先に返すエラー（TranslateQueryError の後。文の数の指標に使う）。
	cmdErr error
	// cmdRead は、実行中のコマンドが読んだ行と使ったインデックス、cmdWaits は待った時間（遅い文のログ。slowlog.go）。
	cmdRead  readStats
	cmdWaits store.Waits
	// cmdStatsWait は、実行中のコマンドが表の統計を作るのを待った時間（rowCountCache.build）。
	cmdStatsWait time.Duration
	// cmdScans は、実行中の SELECT の文で使い回す走査（scan.go の scanPool）。SELECT でなければ nil。
	cmdScans *scanPool
	// lastQuery は、最後に終わった文（書き込みのロックの持ち主をログと heimdb_status に出す。lockwatch.go）。
	// ロックの見張りが別のゴルーチンから読む。
	lastQuery atomic.Pointer[string]
	// writeStmt は、実行中の文がデータを書き換える文（INSERT、UPDATE、DELETE、REPLACE）かどうか。
	writeStmt bool
	// autocommit は、実行中の文を始めたときの @@autocommit。
	autocommit bool
	// tempBranch は一時テーブルを置く隠しブランチ。0 ならまだ一時テーブルを作っていない。
	tempBranch uint64
	// remoteHost は、接続してきた相手のアドレス（ポートを除く）。変更の見張りのログに出す（watch.go）。
	remoteHost string
	// pins は、実行中の文が AS OF で読む時刻の登録（GC がその時刻より先に境界を進めないように）を解く関数。文の終わりに解く。
	pins []func()
	// limitMu から timedOut は、ブランチの実行時間の上限の時計（limit.go）。limitGen は文ごとに進め、終わった文の時計が
	// 次の文を止めないようにする。
	limitMu    sync.Mutex
	limitTimer *time.Timer
	limitGen   uint64
	timedOut   bool
	// cmdTxn と cmdMark は、コマンドの始まりのトランザクションと書き込みの位置（実行時間の上限で止めた文や panic した
	// 文の書き込みを取り消す。rollbackCommand）。
	cmdTxn  *store.Txn
	cmdMark int
	// limited は、実行中の文が上限の付いたブランチを使う接続のものかどうか（一時テーブルを拒否する。limit.go）。
	limited bool
}

// pinAsOf は、実行中の文が時刻 ts を AS OF で読むことを GC に知らせる。境界より前ならエラー。
func (s *Session) pinAsOf(ts uint64) error {
	release, err := s.st.Pin(ts)
	if err != nil {
		return err
	}
	s.pins = append(s.pins, release)
	return nil
}

func (s *Session) releasePins() {
	for _, release := range s.pins {
		release()
	}
	s.pins = nil
}

// writeBranch は、書き込む文がロックするブランチ（接続中のデータベースのブランチ）。
func (s *Session) writeBranch(ctx *sql.Context) uint64 {
	if s.pro == nil {
		return store.MainBranchID
	}
	return s.pro.branchOf(ctx.GetCurrentDatabase())
}

var (
	_ sql.Session               = (*Session)(nil)
	_ sql.TransactionSession    = (*Session)(nil)
	_ sql.LifecycleAwareSession = (*Session)(nil)
)

// NewSession は pro のストアを使うセッションを作る。
func NewSession(base *sql.BaseSession, pro *Provider) *Session {
	pro.connections.Add(1)
	pro.connectionsTotal.Add(1)
	return &Session{BaseSession: base, st: pro.st, pro: pro}
}

// NewSessionBuilder は server.SessionBuilder を返す。memory.NewSessionBuilder と同じ手順でクライアント情報を取り出す。
func NewSessionBuilder(pro *Provider) func(ctx context.Context, conn *mysql.Conn, addr string) (sql.Session, error) {
	return func(ctx context.Context, conn *mysql.Conn, addr string) (sql.Session, error) {
		host, user := "", ""
		if u, ok := conn.UserData.(sql.MysqlConnectionUser); ok {
			host, user = u.Host, u.User
		}
		client := sql.Client{Address: host, User: user, Capabilities: conn.Capabilities}
		s := NewSession(sql.NewBaseSessionWithClientServer(addr, client, conn.ConnectionID), pro)
		if err := s.SetSessionVariable(sql.NewEmptyContext(), "collation_connection", handshakeCollation(conn.CharacterSet).Name()); err != nil {
			return nil, err
		}
		pro.sessions.Store(s.ID(), s) // 書き込みのロックの持ち主の最後の文を引く（lockwatch.go）
		if a := conn.RemoteAddr(); a != nil {
			s.remoteHost = a.String()
			if h, _, err := net.SplitHostPort(s.remoteHost); err == nil {
				s.remoteHost = h
			}
		}
		return s, nil
	}
}

// handshakeCollation は、クライアントが接続のときに伝えた照合順序（id）から、接続の照合順序（collation_connection）を
// 決める（docs/plan.md の段階 90）。MySQL と同じく伝えられた照合順序を使い、go-mysql-server の既定（utf8mb4_0900_bin。
// リテラル同士の 'a'='A' が 0 になる）にはしない。HeimDB の文字セットは常に utf8mb4 なので、utf8mb3 と ascii の
// 照合順序は utf8mb4 の同じ名前のもの（utf8mb3_general_ci は utf8mb4_general_ci）にする。HeimDB の挨拶は vitess の
// 定数の 33（utf8mb3_general_ci）を名乗るので、サーバに合わせるクライアントはこれを返す。知らない id や utf8mb4 に
// ならない文字セットは、MySQL 8.0 の既定の utf8mb4_0900_ai_ci にする。
func handshakeCollation(id uint8) sql.CollationID {
	c := sql.CollationID(id)
	if c == sql.Collation_Unspecified || c.Name() == "" {
		return defaultCollation
	}
	if m, ok, err := schema.MapToUTF8MB4(c); ok && err == nil {
		c = m
	}
	if c.CharacterSet() != sql.CharacterSet_utf8mb4 {
		return defaultCollation
	}
	return c
}

// Transaction は sql.Transaction の実装。
type Transaction struct {
	txn      *store.Txn
	readOnly bool
	// savepoints は、作った順のセーブポイント。
	savepoints []savepoint
}

func (t *Transaction) String() string   { return "heimdb transaction" }
func (t *Transaction) IsReadOnly() bool { return t.readOnly }

// StartTransaction は sql.TransactionSession の実装。
func (s *Session) StartTransaction(ctx *sql.Context, c sql.TransactionCharacteristic) (sql.Transaction, error) {
	if s.txn != nil {
		// エンジンは前のトランザクションをコミットしてから新しく始めるので、ここに残っているのは異常。
		_ = s.txn.Rollback()
	}
	s.txn = s.st.Begin()
	s.txn.Waits, s.txn.Owner, s.txn.Cancel = &s.cmdWaits, s.ID(), ctx.Done()
	if s.writeStmt {
		// 自動コミットの書き込む文は、行を読む前にロックを取る（ValidateSession を参照）。
		if err := s.txn.LockForWrite(s.writeBranch(ctx)); err != nil {
			_ = s.txn.Rollback()
			s.txn = nil
			return nil, err
		}
	}
	return &Transaction{txn: s.txn, readOnly: c == sql.ReadOnly}, nil
}

// ValidateSession は sql.Session の実装。go-mysql-server が各クエリの解析の前（トランザクションを始める前）に呼ぶ。
//
// データを書き換える文なら、行を読む前に書き込みロックを取る。トランザクションのスナップショットは最初の読み取りで
// 決まるので、ロックの後に決まる。そうすれば、待っている間に他のトランザクションがコミットした変更の上に書くことになり、
// 自動コミットの文が競合（エラー 1213）で失敗しない。go-mysql-server は単純な INSERT や UPDATE を独自の解析ルールを
// 通さずに処理するので、文の種類はクエリの文字列で判定する。判定できなかった書き込みも、エディタが書く前にロックを取る。
func (s *Session) ValidateSession(ctx *sql.Context) error {
	// 一時テーブルにだけ書く文は、ブランチのロックを取らない（一時テーブルの隠しブランチのロックはエディタが取る）。
	s.cmdQuery = ctx.Query()
	s.writeStmt = isWriteQuery(ctx.Query()) && !s.writesOnlyTemporaryTables(ctx)
	s.cmdScans = nil
	if readOnlyQuery(ctx.Query()) {
		s.cmdScans = &scanPool{idle: map[scanKey][]*scanHandle{}}
	}
	if ac, err := plan.IsSessionAutocommit(ctx); err == nil {
		s.autocommit = ac
	}
	execMS, limited := s.sessionLimits(ctx)
	s.limited = limited
	if ms := sessionMaxExecutionTime(ctx); ms > 0 && (execMS == 0 || ms < execMS) {
		execMS = ms
	}
	s.startTimeLimit(ctx, execMS)
	if s.txn != nil {
		s.txn.Cancel = ctx.Done() // この文が止められたら、書き込みのロックを待つのをやめる
	}
	if s.writeStmt && s.txn != nil {
		if err := s.txn.LockForWrite(s.writeBranch(ctx)); err != nil {
			return err
		}
	}
	return s.BaseSession.ValidateSession(ctx)
}

// isWriteQuery は、クエリがデータを書き換える文（INSERT、UPDATE、DELETE、REPLACE）かどうかを、先頭の語で判定する。
// readOnlyQuery は、文 q が書かない文か（SELECT か、かっこで始まる UNION）。文の中で走査と行の読み手を使い回して
// よい文（scanPool、pkRowIter.openReader）。WITH で始まる文は、後ろに UPDATE や DELETE が続きうるので含めない
// （WordPress は WITH を出さない。段階 99）。
func readOnlyQuery(q string) bool {
	q = skipSpaceAndComments(q)
	if strings.HasPrefix(q, "(") {
		return true
	}
	end := strings.IndexFunc(q, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(q)
	}
	return strings.EqualFold(q[:end], "SELECT")
}

func isWriteQuery(q string) bool {
	q = skipSpaceAndComments(q)
	end := strings.IndexFunc(q, func(r rune) bool { return !unicode.IsLetter(r) })
	if end < 0 {
		end = len(q)
	}
	switch strings.ToUpper(q[:end]) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE":
		return true
	}
	return false
}

func skipSpaceAndComments(q string) string {
	for {
		q = strings.TrimLeftFunc(q, unicode.IsSpace)
		switch {
		case strings.HasPrefix(q, "/*"):
			end := strings.Index(q, "*/")
			if end < 0 {
				return ""
			}
			q = q[end+2:]
		case strings.HasPrefix(q, "-- "), strings.HasPrefix(q, "#"):
			end := strings.IndexByte(q, '\n')
			if end < 0 {
				return ""
			}
			q = q[end+1:]
		default:
			return q
		}
	}
}

// CommandBegin は sql.LifecycleAwareSession の実装。前のコマンドで対にならなかった文の開始の記録を残さない。
func (s *Session) CommandBegin() error {
	// 停止が始まっていれば拒否する（shutdown.go）。始めたら CommandEnd で終える。
	if err := s.pro.gate.enter(); err != nil {
		return err
	}
	s.cmdStart, s.cmdQuery, s.cmdErr = time.Now(), "", nil
	s.cmdRead.reset()
	s.cmdWaits, s.cmdStatsWait = store.Waits{}, 0
	s.discardErr, s.maintenance = nil, ""
	s.cmdTxn, s.cmdMark = s.txn, 0
	if s.txn != nil {
		s.txn.ResetStatement()
		s.cmdMark = s.txn.Mark()
	}
	return nil
}

// CommandEnd は sql.LifecycleAwareSession の実装。
//
// 自動コミットの文がエラーで終わると、go-mysql-server はトランザクションをロールバックしない。手放す場合
// （engine.go の clearAutocommitTransaction）と、保持したまま次の文で使い回す場合がある。どちらも、書き込みロックを
// 持ったまま接続が待機することになるので、MySQL と同じくその文をロールバックしてロックを解放する。
// 明示的なトランザクション（START TRANSACTION、または @@autocommit = 0）は、COMMIT か ROLLBACK まで残す。
func (s *Session) CommandEnd() {
	defer s.pro.gate.leave()
	// 終わっていない表の書き直し（エンジンが取り消しも Close も呼ばずに返した）を取り消す。書きかけのデータと印を消し、
	// 読み出しのスナップショットを閉じる（閉じないと GC の境界が止まる）。docs/plan.md の段階 53。
	for _, r := range slices.Clone(s.rewriters) {
		_ = r.DiscardChanges(nil, nil)
	}
	s.rewriters = nil
	// エンジンが Close を呼ばずに返した走査を閉じる（Pebble のイテレータとスナップショットを残さない。段階 55）。
	for _, c := range slices.Clone(s.closers) {
		c.close()
	}
	s.closers = nil
	s.stopTimeLimit()
	s.observeCommand()
	s.writeStmt = false
	s.releasePins()
	if s.txn == nil {
		// コミットに失敗すると、go-mysql-server は終わったトランザクションを持ち続け、次の文で新しく始めない。
		if tx, ok := s.GetTransaction().(*Transaction); ok && tx.txn.Done() {
			s.SetTransaction(nil)
		}
		return
	}
	// DDL 用のトランザクション（beginDDL）は、ふつうは文の終わりに go-mysql-server がコミットする。DDL が失敗すると
	// コミットされずに残るので、ここで片付ける。行の書き込みは持たない（DDL は直接コミットする）のでコミットしてよい。
	if s.txn.DDL {
		_ = s.commit(s.txn)
		s.txn = nil
		s.SetTransaction(nil)
		return
	}
	abandoned := s.GetTransaction() == nil
	unfinishedAutocommit := s.autocommit && !s.GetIgnoreAutoCommit()
	if abandoned || unfinishedAutocommit {
		_ = s.txn.Rollback()
		s.txn = nil
		s.SetTransaction(nil)
	}
}

// rollbackCommand は、このコマンド（文）の書き込みを、コマンドの始まりの位置まで取り消す。明示的なトランザクションの
// 前の文の書き込みは残す。コマンドの中で始まったトランザクションは、すべて取り消す。DDL 用のトランザクションは行を
// 持たないので触らない（CommandEnd が片付ける）。
func (s *Session) rollbackCommand() error {
	if s.txn == nil || s.txn.Done() || s.txn.DDL {
		return nil
	}
	mark := 0
	if s.txn == s.cmdTxn {
		mark = s.cmdMark
	}
	return s.txn.RollbackTo(mark)
}

// QueryPanicked は server.QueryPanicHandler の実装。エンジンが文の処理の panic を回復したとき、その文の書き込みを
// 取り消す（実行時間の上限で止めた文と同じ）。取り消せなければ、トランザクションをすべて捨て、明示的なトランザクション
// の状態も終える（続きの文が新しいトランザクションを黙って始め、COMMIT が後半だけを確定しないように）。
func (s *Session) QueryPanicked() {
	if err := s.rollbackCommand(); err != nil {
		s.abortTransaction()
	}
}

// abortTransaction は、トランザクションをすべて捨て、明示的なトランザクションの状態も終える（続きの文が新しい
// トランザクションを黙って始め、COMMIT が後半だけを確定しないように）。トランザクションがもうなくても（COMMIT が
// 失敗した。CommitTransaction は先に s.txn を外す）、明示的なトランザクションの状態は終える。
func (s *Session) abortTransaction() {
	if s.txn != nil {
		_ = s.txn.Rollback()
		s.txn = nil
	}
	s.SetTransaction(nil)
	s.SetIgnoreAutoCommit(false)
}

// Warnings は sql.Session の実装。MySQL と同じく、警告を出た順に返す（go-mysql-server の BaseSession.Warnings は
// 新しい順に返し、SHOW WARNINGS の並びと LIMIT が MySQL と逆になる）。
func (s *Session) Warnings() []*sql.Warning {
	w := s.BaseSession.Warnings()
	slices.Reverse(w)
	return w
}

// beginDDL は、DDL の文を始めるときに、MySQL と同じく実行中のトランザクションを暗黙にコミットし、DDL 用の新しい
// トランザクションでブランチの書き込みロックを取る。go-mysql-server は DDL の後にしかコミットしない
// （sql/rowexec/transaction_iters.go）ので、そのままでは DDL が古いスナップショットで行を読み、自分の未コミットの行を
// 見落とす。同じ文の中で何度呼んでもよい（2 回目からは、前の DDL の結果が見えるようにスナップショットを取り直す。
// 複数の操作がある ALTER TABLE は、操作ごとにテーブルを書き直す）。DDL 用のトランザクションは、文の終わりに
// go-mysql-server がコミットする。
func (s *Session) beginDDL(branch uint64) error {
	if s.txn.DDL {
		s.txn.RefreshSnapshot()
	} else {
		if err := s.commit(s.txn); err != nil {
			return err
		}
		// MySQL と同じく、暗黙のコミットで明示的なトランザクション（START TRANSACTION）も終わる。go-mysql-server は
		// COMMIT と ROLLBACK のときだけ印を外すので、外さないと DDL の後の書き込みがコミットされずに残る。
		s.SetIgnoreAutoCommit(false)
		s.txn = s.st.Begin()
		s.txn.Waits, s.txn.Owner = &s.cmdWaits, s.ID()
		s.txn.DDL = true
		if tx, ok := s.GetTransaction().(*Transaction); ok {
			tx.txn = s.txn
		}
	}
	return s.txn.LockForWrite(branch)
}

// observeCommand は、終わった文を指標に数え（docs/plan.md の段階 73）、遅い文をログに出す（段階 19、段階 81。slowlog.go）。
// 解析の前にエラーで止まった文は、種類を other として数える。文でないコマンド（準備など）は数えない。
func (s *Session) observeCommand() {
	if s.cmdQuery == "" && s.cmdErr == nil {
		return
	}
	if s.cmdQuery != "" {
		q := s.cmdQuery
		s.lastQuery.Store(&q)
	}
	d := time.Since(s.cmdStart)
	threshold, on := slowThreshold()
	slow := on && d >= threshold
	s.pro.stats.observe(s.cmdQuery, s.cmdErr, d, slow)
	s.pro.stats.observeWaits(s.cmdWaits, s.cmdStatsWait)
	if !slow || s.cmdQuery == "" {
		return
	}
	s.logSlow(d)
}

// hidePassword は、パスワードを含みうる文（ユーザーの管理）を、先頭の 2 語だけにする（MySQL のスローログは
// パスワードを書き換えるが、ここでは単純に本文を隠す）。
func hidePassword(q string) string {
	u := strings.ToUpper(q)
	words := strings.Fields(u)
	if len(words) < 2 {
		return q
	}
	head := words[0] + " " + words[1]
	switch {
	case head == "CREATE USER", head == "ALTER USER", head == "SET PASSWORD", words[0] == "GRANT", strings.Contains(u, "IDENTIFIED"):
		return head + " ... (text hidden)"
	}
	return q
}

// SessionEnd は sql.LifecycleAwareSession の実装。切断されたら未コミットのトランザクションを捨て、一時テーブルを消す。
func (s *Session) SessionEnd() {
	s.pro.connections.Add(-1)
	// COM_RESET_CONNECTION は同じ ID の新しいセッションを作ってから古いものを終えるので、自分のときだけ消す
	s.pro.sessions.CompareAndDelete(s.ID(), s)
	// 停止の後はストアを閉じているかもしれないので、何もしない（ロールバックはコミットしないのと同じ。一時テーブルの
	// 隠しブランチは、次の起動で片付ける）。
	if err := s.pro.gate.enter(); err != nil {
		return
	}
	defer s.pro.gate.leave()
	s.releasePins()
	if s.txn != nil {
		_ = s.txn.Rollback()
		s.txn = nil
	}
	if s.tempBranch != 0 {
		_ = s.st.DropBranchData(s.tempBranch)
		rowCounts.forget(s.st, s.tempBranch)
		s.tempBranch = 0
	}
}

// CommitTransaction は sql.TransactionSession の実装。
func (s *Session) CommitTransaction(ctx *sql.Context, tx sql.Transaction) error {
	t := tx.(*Transaction)
	if s.txn == t.txn {
		s.txn = nil
	}
	return s.commit(t.txn)
}

// commit は、トランザクションをコミットし、見張る行の変更をログに出す（watch.go）。
func (s *Session) commit(txn *store.Txn) error {
	if s.pro == nil {
		return txn.Commit()
	}
	return s.pro.commitWatched(s, txn, "")
}

// Rollback は sql.TransactionSession の実装。
func (s *Session) Rollback(ctx *sql.Context, tx sql.Transaction) error {
	t := tx.(*Transaction)
	if s.txn == t.txn {
		s.txn = nil
	}
	return t.txn.Rollback()
}

// セーブポイント（docs/plan.md の段階 17）。mysqldump --single-transaction がテーブルごとに使う。トランザクションの
// 書き込みの記録の位置を名前で覚え、ROLLBACK TO でそこまで切り詰める（文の失敗の取り消しと同じ仕組み）。名前は
// MySQL と同じく大文字と小文字を区別しない。

// savepoint は、名前と書き込みの記録の位置。
type savepoint struct {
	name string
	mark int
}

// findSavepoint は、名前のセーブポイントの位置（t.savepoints の添字）を返す。なければ -1。
func (t *Transaction) findSavepoint(name string) int {
	for i, sp := range t.savepoints {
		if strings.EqualFold(sp.name, name) {
			return i
		}
	}
	return -1
}

func errSavepointDoesNotExist(name string) error {
	return mysql.NewSQLError(1305, "42000", "SAVEPOINT %s does not exist", name)
}

// CreateSavepoint は sql.TransactionSession の実装。同じ名前があれば置き換える。
func (s *Session) CreateSavepoint(_ *sql.Context, tx sql.Transaction, name string) error {
	t := tx.(*Transaction)
	if i := t.findSavepoint(name); i >= 0 {
		t.savepoints = append(t.savepoints[:i], t.savepoints[i+1:]...)
	}
	t.savepoints = append(t.savepoints, savepoint{name: name, mark: t.txn.Mark()})
	return nil
}

// RollbackToSavepoint は sql.TransactionSession の実装。それより後の書き込みを取り消し、それより後のセーブポイントを
// 忘れる（そのセーブポイントは残る）。
func (s *Session) RollbackToSavepoint(_ *sql.Context, tx sql.Transaction, name string) error {
	t := tx.(*Transaction)
	i := t.findSavepoint(name)
	if i < 0 {
		return errSavepointDoesNotExist(name)
	}
	if err := t.txn.RollbackTo(t.savepoints[i].mark); err != nil {
		return err
	}
	t.savepoints = t.savepoints[:i+1]
	return nil
}

// ReleaseSavepoint は sql.TransactionSession の実装。そのセーブポイントと、それより後のものを忘れる。
func (s *Session) ReleaseSavepoint(_ *sql.Context, tx sql.Transaction, name string) error {
	t := tx.(*Transaction)
	i := t.findSavepoint(name)
	if i < 0 {
		return errSavepointDoesNotExist(name)
	}
	t.savepoints = t.savepoints[:i]
	return nil
}

// txnFor は、ctx のセッションで実行中のトランザクションを返す。
// トランザクションの外からの読み取り（エンジンの内部処理など）では、一時的なトランザクションを作り、
// 使い終わったら done を呼んで捨てる。
func txnFor(ctx *sql.Context, st *store.Store) (txn *store.Txn, done func()) {
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil {
		return s.txn, func() {}
	}
	txn = st.Begin()
	return txn, func() { _ = txn.Rollback() }
}

// writesOnlyTemporaryTables は、書き込む文の対象のテーブルが、すべてこのセッションの一時テーブルかどうかを返す。
// 一時テーブルがなければ、文を構文解析せずに false を返す。対象が分からなければ false（ロックを取る）。
func (s *Session) writesOnlyTemporaryTables(ctx *sql.Context) bool {
	if s.tempBranch == 0 {
		return false
	}
	stmt, err := sqlparser.Parse(ctx.Query())
	if err != nil {
		return false
	}
	var names []sqlparser.TableName
	// byAlias は、FROM 句のテーブルを別名（なければ名前）で引く（DELETE t FROM wp_options AS t の t を解決するため）。
	byAlias := map[string]sqlparser.TableName{}
	tableExprNames := func(exprs sqlparser.TableExprs) bool {
		for _, e := range exprs {
			a, ok := e.(*sqlparser.AliasedTableExpr)
			if !ok {
				return false // 結合など
			}
			n, ok := a.Expr.(sqlparser.TableName)
			if !ok {
				return false // サブクエリなど
			}
			names = append(names, n)
			key := n.Name.String()
			if !a.As.IsEmpty() {
				key = a.As.String()
			}
			byAlias[strings.ToLower(key)] = n
		}
		return true
	}
	switch st := stmt.(type) {
	case *sqlparser.Insert:
		names = append(names, st.Table)
	case *sqlparser.Update:
		if !tableExprNames(st.TableExprs) {
			return false
		}
	case *sqlparser.Delete:
		if !tableExprNames(st.TableExprs) {
			return false
		}
		if len(st.Targets) > 0 {
			// 消す対象は FROM 句の別名か名前。FROM 句のテーブルに解決して、それだけを確かめる（読むだけのテーブルは
			// ロックが要らない）。
			names = nil
			for _, tgt := range st.Targets {
				n, ok := byAlias[strings.ToLower(tgt.Name.String())]
				if !ok || tgt.DbQualifier.String() != "" {
					return false
				}
				names = append(names, n)
			}
		}
	default:
		return false
	}
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if q := n.DbQualifier.String(); q != "" && !strings.EqualFold(q, ctx.GetCurrentDatabase()) {
			return false
		}
		if _, ok, err := s.st.GetTable(s.tempBranch, n.Name.String(), store.Latest); err != nil || !ok {
			return false
		}
	}
	return true
}
