package backend

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	vtlog "github.com/dolthub/vitess/go/vt/log"
	"github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// SQL とプロトコルのファズ（docs/plan.md の段階 105）。落ちない、止まらない、接続を切らないことだけを見る（結果が
// MySQL と同じかはゴールデンと PHPUnit の役目）。ふつうの go test は種の一部と testdata/fuzz の再現の入力だけを流す。
//
//	go test ./internal/backend -run '^$' -fuzz '^FuzzQuery$' -fuzztime 1h -parallel 4
//
// 入力は WordPress と同じ管理者でないユーザーで、使い捨てのブランチ（wordpress@f）に流す。入力ごとにブランチを
// 作り直し、接続も新しくするので、どの入力も同じ状態から始まる。不具合とみなすのは、プロセスが落ちる、回復した
// panic（HeimDB: internal error と、vitessKnown にない vitess のもの）、接続が切れる、fuzzHang を過ぎても終わらない、
// 入力の後に書き込みのロックが残る・つなげない・main に書けない、のどれか。
//
// 見つかると、入力が testdata/fuzz/<対象>/ に書かれ、ふつうの go test で流れ続ける。直したら、何の再現かわかる名前
// （stage105-empty-sysvar など）に変えて残す。vitess の中で起きて接続 1 本を閉じるだけのものは、WordPress が送らない
// 形なら vitessKnown に足して記録だけにする（直すにはフォークが要る）。worker は fuzz の終わりに止められるので、
// $TMPDIR に一時ディレクトリ（FuzzQuery*、FuzzPackets*）が残ることがある。

// fuzzExecLimit は、ブランチに掛ける文の実行時間の上限。fuzzHang を過ぎても終わらなければ、上限で止められない
// 不具合として見張りが落とす（Go の fuzz には入力 1 件の時間の上限がない）。
const (
	fuzzExecLimit = 3 * time.Second
	fuzzHang      = 20 * time.Second
	fuzzBranch    = "f"
	fuzzUser      = "fz"
)

// fuzzServer は、ファズの対象のサーバ。worker のプロセスごとに 1 つ作る。
type fuzzServer struct {
	addr string
	pro  *Provider
	st   *store.Store
	root *sql.DB
	site *sql.DB // wordpress@f に fuzzUser でつなぐ。接続は使い回さない
	base uint64  // ブランチを分岐させる main の時刻（準備の後。main はこの後も変わるが、ブランチは常にここから）

	mu     sync.Mutex
	panics []string // 回復した panic のログ（HeimDB と、vitessKnown にない vitess のもの）
}

// vitessKnown は、vitess（フォークしていない）の中で起き、vitess が回復してその接続を閉じるだけの panic（panic した
// vitess の行）。WordPress が送らない形なので、記録だけにする（docs/plan.md の段階 105）。vitess を上げたら、行を
// 直す（ずれると、この 3 つが不具合として出る）。
var vitessKnown = map[string]bool{
	"go/mysql/conn.go:1295":    true, // COM_STMT_RESET の文の ID が欠けている
	"go/mysql/encoding.go:180": true, // 短いハンドシェイクの応答（認証の前）
	"go/mysql/conn.go:959":     true, // 空のコマンドのパケット
}

// vitessFrame は、スタックの中の最初の vitess の行。
var vitessFrame = regexp.MustCompile(`dolthub/vitess@[^/]+/(go/\S+\.go:\d+)`)

// panicHook は、回復した panic のログを集める。
type panicHook struct{ s *fuzzServer }

func (h panicHook) Levels() []logrus.Level { return []logrus.Level{logrus.ErrorLevel} }

func (h panicHook) Fire(e *logrus.Entry) error {
	if strings.Contains(e.Message, "recovered from a panic") {
		h.s.addPanic(e.Message)
	}
	return nil
}

func (s *fuzzServer) addPanic(msg string) {
	s.mu.Lock()
	s.panics = append(s.panics, msg)
	s.mu.Unlock()
}

func (s *fuzzServer) takePanics() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.panics
	s.panics = nil
	return p
}

// startFuzzServer は、ゴールデンの文で作った DB を main に持つサーバを立てる。
func startFuzzServer(f *testing.F) *fuzzServer {
	f.Helper()
	s := &fuzzServer{}
	// メモリを食う入力は 1041 で止める（worker が OOM で落ちると、無関係な入力が crasher に記録される）。上限は fuzz の
	// worker（-fuzz で入力を作るプロセス）だけに掛ける。ふつうの go test は種の入力を、ほかのテストと同じプロセスで流す
	// ので、プロセス全体のメモリ（-race ではさらに多い）が 1 GiB の 75% を超えて、準備の文や種の入力が 1041 で落ちる
	// ことがあった（CI。段階 114）。
	if fuzzWorker() {
		prevLimit := MemoryLimit()
		SetMemoryLimit(1 << 30)
		f.Cleanup(func() { SetMemoryLimit(prevLimit) })
	}
	// 回復した panic はスタック付きのログで出る。出力は捨て、フックで集める。
	logger := logrus.StandardLogger()
	prevOut, prevHooks := logger.Out, logger.ReplaceHooks(logrus.LevelHooks{})
	logger.AddHook(panicHook{s})
	logger.SetOutput(io.Discard)
	prevVt := vtlog.Errorf
	vtlog.Errorf = func(format string, args ...any) {
		if msg := fmt.Sprintf(format, args...); strings.Contains(msg, "caught panic") {
			if m := vitessFrame.FindStringSubmatch(msg); m == nil || !vitessKnown[m[1]] {
				s.addPanic(msg)
			}
		}
	}
	f.Cleanup(func() {
		logger.ReplaceHooks(prevHooks)
		logger.SetOutput(prevOut)
		vtlog.Errorf = prevVt
	})

	addr, pro, st, srv := serve(f, f.TempDir(), serverConfig{auth: true, rootPassword: "rootpw",
		store: []func(*store.Store){func(st *store.Store) { st.LockTimeout = 2 * time.Second }}})
	// 本番（cmd/heimdb）と同じく、実行中の文と切断の後片付けが終わってからストアを閉じる（worker の終わりに、閉じた
	// ストアに後片付けが書いて panic しないように）。
	f.Cleanup(func() {
		srv.Close()
		pro.Shutdown(10 * time.Second)
		st.Close()
	})
	s.addr, s.pro, s.st = addr, pro, st
	root, err := sql.Open("mysql", fmt.Sprintf("root:rootpw@tcp(%s)/%s", addr, testDB))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { root.Close() })
	root.SetMaxOpenConns(1) // 準備の文を、sql_mode を設定した 1 本の接続で流す
	s.root = root
	for _, q := range []string{wpSQLMode} {
		if _, err := root.Exec(q); err != nil {
			f.Fatal(err)
		}
	}
	for _, q := range fuzzSetup() {
		root.Exec(q) // 失敗はかまわない（ゴールデンにはエラーを確かめる文もある）
	}
	for _, q := range []string{
		"CREATE TABLE fuzz_canary (id int NOT NULL, n bigint NOT NULL, PRIMARY KEY (id))",
		"CREATE USER '" + fuzzUser + "'@'%'", // パスワードなし（FuzzPackets の手組みの認証のため）
		"GRANT ALL ON `" + testDB + "@" + fuzzBranch + "`.* TO '" + fuzzUser + "'@'%'",
	} {
		if _, err := root.Exec(q); err != nil {
			f.Fatalf("%s: %v", q, err)
		}
	}
	s.base = st.LastCommitted()
	if err := s.resetBranch(); err != nil {
		f.Fatal(err)
	}
	site, err := sql.Open("mysql", fmt.Sprintf("%s@tcp(%s)/%s@%s", fuzzUser, addr, testDB, fuzzBranch))
	if err != nil {
		f.Fatal(err)
	}
	site.SetMaxIdleConns(0) // 閉じた接続を使い回さない（接続ごとの状態を次の入力に持ち越さない）
	f.Cleanup(func() { site.Close() })
	s.site = site
	return s
}

// resetBranch は、ブランチ f を消して、main の準備の後の時刻から作り直す。消すときは heimdb_branch_delete と同じく
// 統計も忘れる（新しいブランチは別の ID になり、忘れなければ入力ごとに統計が残る）。
func (s *fuzzServer) resetBranch() error {
	if b, ok := s.st.BranchByName(fuzzBranch); ok {
		if err := s.st.DeleteBranch(fuzzBranch); err != nil {
			return fmt.Errorf("delete the branch: %w", err)
		}
		rowCounts.forget(s.st, b.ID)
	}
	if _, err := s.st.CreateBranch(fuzzBranch, store.MainBranchID, s.base); err != nil {
		return fmt.Errorf("create the branch: %w", err)
	}
	if _, err := s.st.SetBranchLimits(fuzzBranch, 256<<20, uint64(fuzzExecLimit/time.Millisecond)); err != nil {
		return err
	}
	return nil
}

// watch は、fn が fuzzHang を過ぎても終わらなければ、入力を出してプロセスを落とす（fuzz の coordinator が入力を
// testdata/fuzz に書く）。
func watch(input string, fn func()) {
	timer := time.AfterFunc(fuzzHang, func() {
		fmt.Fprintf(os.Stderr, "HeimDB fuzz: the input did not finish in %s: %q\n", fuzzHang, input)
		panic("HeimDB fuzz: hang")
	})
	defer timer.Stop()
	fn()
}

// check は、入力の後にサーバが無事かを確かめ、ブランチを作り直す。panics は入力の間に回復した panic のログ。
func (s *fuzzServer) check(t *testing.T, input string) {
	t.Helper()
	ctx := context.Background()
	// 書き込みのロックが残っていないか（切断の後片付けは接続が閉じた後に走るので、少し待つ）。
	b, ok := s.st.BranchByName(fuzzBranch)
	if !ok {
		t.Fatalf("the branch %s is gone after %q", fuzzBranch, input)
	}
	release, err := s.st.LockBranchFor(b.ID, 5*time.Second, store.LockOwner{Op: "fuzz check"}, nil)
	if err != nil {
		t.Fatalf("the write lock of the branch is still held after %q: %v", input, err)
	}
	release()
	conn, err := s.site.Conn(ctx)
	if err != nil {
		t.Fatalf("cannot connect after %q: %v", input, err)
	}
	var n int
	err = conn.QueryRowContext(ctx, "SELECT 1").Scan(&n)
	conn.Close()
	if err != nil {
		t.Fatalf("SELECT 1 fails after %q: %v", input, err)
	}
	if _, err := s.root.ExecContext(ctx, "REPLACE INTO fuzz_canary VALUES (1, UNIX_TIMESTAMP())"); err != nil {
		t.Fatalf("a write to main fails after %q: %v", input, err)
	}
	if p := s.takePanics(); len(p) > 0 {
		t.Fatalf("recovered a panic for %q:\n%s", input, strings.Join(p, "\n"))
	}
	if err := s.resetBranch(); err != nil {
		t.Fatalf("after %q: %v", input, err)
	}
}

// fuzzSkip は、ファズで流さない入力。自分の接続を切る文（切断を不具合と見分けられない）。
var fuzzSkip = regexp.MustCompile(`(?i)\bkill\b`)

// connLost は、接続が切れたことを表すエラーか。
func connLost(err error) bool {
	return errors.Is(err, mysql.ErrInvalidConn) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(err.Error(), "bad connection") || strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "connection reset")
}

// runFuzzQuery は、1 つの文を新しい接続で流し、結果を読み捨てる。
func (s *fuzzServer) runFuzzQuery(t *testing.T, q string) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.site.Conn(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	timeouts := s.timeouts()
	rows, err := conn.QueryContext(ctx, q)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "HeimDB: internal error") {
		t.Fatalf("internal error for %q: %v\n%s", q, err, strings.Join(s.takePanics(), "\n"))
	}
	// 行を送り始めた後のエラー（実行時間の上限で止めたものを含む）では、vitess が接続を閉じる（docs/engine.md の
	// 段階 46。直していない）。この文を上限で止めた（3024 の数が増えた）ときの切断は、それとみなす。
	if connLost(err) && !s.timedOutSince(timeouts) {
		t.Fatalf("the connection was lost for %q: %v", q, err)
	}
}

// timeouts は、実行時間の上限で止めた文の数（文の指標のエラー 3024）。
func (s *fuzzServer) timeouts() uint64 {
	s.pro.stats.mu.Lock()
	defer s.pro.stats.mu.Unlock()
	return s.pro.stats.errors[3024]
}

// timedOutSince は、数が before から増えたかを返す（文の数は接続を閉じた後に数えることがあるので、少し待つ）。
func (s *fuzzServer) timedOutSince(before uint64) bool {
	for range 20 {
		if s.timeouts() > before {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// FuzzQuery は、テキストのプロトコルの 1 つの文を流す。
func FuzzQuery(f *testing.F) {
	for _, q := range fuzzSeeds() {
		f.Add(q)
	}
	s := startFuzzServer(f)
	f.Fuzz(func(t *testing.T, q string) {
		if fuzzSkip.MatchString(q) {
			t.Skip()
		}
		defer func() {
			if t.Failed() {
				s.takePanics() // 失敗で途中に終わった入力のものを、次の入力に持ち越さない
			}
		}()
		if p := s.takePanics(); len(p) > 0 { // 前の入力の確かめの後に（切断の後片付けなどで）起きたもの
			t.Fatalf("recovered a panic after the previous input:\n%s", strings.Join(p, "\n"))
		}
		watch(q, func() {
			s.runFuzzQuery(t, q)
			s.check(t, q)
		})
	})
}

// goldenStatements は、ゴールデンの文を返す。文は "-- " の行から、空白か ")" で始まる続きの行まで。
func goldenStatements() [][]string {
	files, err := filepath.Glob("../testdata/*_golden.txt")
	if err != nil {
		panic(err)
	}
	var all [][]string
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			panic(err)
		}
		var stmts []string
		var cur []string
		flush := func() {
			if cur != nil {
				stmts = append(stmts, strings.Join(cur, "\n"))
				cur = nil
			}
		}
		for _, line := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(line, "-- "):
				flush()
				cur = []string{strings.TrimPrefix(line, "-- ")}
			case cur != nil && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, ")")):
				cur = append(cur, line)
			default:
				flush()
			}
		}
		flush()
		all = append(all, stmts)
	}
	return all
}

// fuzzSetup は、main に流す準備の文（ゴールデンの文をファイルごとに順に。表と行がそろう）。
func fuzzSetup() []string {
	var out []string
	for _, stmts := range goldenStatements() {
		out = append(out, stmts...)
	}
	return out
}

var fuzzing = func() bool { f := flag.Lookup("test.fuzz"); return f != nil && f.Value.String() != "" }

// literal は、文の数と文字列の定数。
var literal = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|\b\d+\b`)

// boundaryValues は、定数と置き換える境界の値。
var boundaryValues = []string{"NULL", "-1", "18446744073709551616", "-9223372036854775809", "''", "0x00ff", "1e308",
	"'" + strings.Repeat("あ", 300) + "'", "'0000-00-00 00:00:00'", "'%_\\\\'", "_binary'\\0'"}

// fuzzSeeds は、ゴールデンの文と、その定数を境界の値に替えたバージョンを返す。ファズでないときは間引く。
func fuzzSeeds() []string {
	seen := map[string]bool{}
	var out []string
	add := func(q string) {
		if !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	i := 0
	for _, stmts := range goldenStatements() {
		for _, q := range stmts {
			i++
			if !fuzzing() && i%40 != 0 {
				continue
			}
			add(q)
			locs := literal.FindAllStringIndex(q, -1)
			for j, loc := range locs {
				if j >= 3 {
					break
				}
				v := boundaryValues[(i+j)%len(boundaryValues)]
				add(q[:loc[0]] + v + q[loc[1]:])
			}
		}
	}
	return out
}

// fuzzWorker は、このプロセスが go test -fuzz の worker か（go のコマンドが worker に -test.fuzzworker を渡す）。
func fuzzWorker() bool {
	f := flag.Lookup("test.fuzzworker")
	return f != nil && f.Value.String() == "true"
}
