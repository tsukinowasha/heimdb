package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// 耐久性の試験（docs/plan.md の段階 18）。HeimDB を別のプロセスで動かし、書き込み、DDL、バックアップ、GC が続いている
// 最中に kill -9 または SIGTERM で止めて起動し直し、応答を返した書き込みがすべて残り、途中までのトランザクションが
// 残らないことを確かめる。

const (
	accounts = 10
	initial  = 100
	writers  = 4
)

// heimdbBinary は、テスト用に HeimDB をビルドする。
func heimdbBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "heimdb")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

type proc struct {
	cmd  *exec.Cmd
	addr string
	log  *syncBuffer
	done chan error
	// exited は、プロセスが終わると閉じる（done の結果を誰かが受け取った後でもわかる）。
	exited chan struct{}
}

// syncBuffer は、HeimDB の出力を集める。出力はプロセスを読む別の goroutine が書くので、排他する。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start は、データディレクトリ dir で HeimDB を起動し、接続を受け付けるまで待つ。
func start(t *testing.T, bin, dir, backupDir string) *proc {
	t.Helper()
	return startArgs(t, bin, "-data", dir, "-backup-dir", backupDir, "-root-password", "pw",
		"-retention", "1s", "-gc-interval", "200ms", "-shutdown-timeout", "10s")
}

// startArgs は、引数 args（-addr は空いたポートを足す）で HeimDB を起動し、接続を受け付けるまで待つ。
//
// 接続できたかだけでなく、起動した HeimDB が受け付けを始めたと出力するまで待つ。空いたポートを選んでから HeimDB が
// 使うまでの間に、並行して走るほかのパッケージのテストのサーバーが同じポートを取ると、そちらにつながってしまう
// （パスワードが違い Access denied になった）。HeimDB がポートを使えずに終わったら、別のポートで起動し直す。
func startArgs(t *testing.T, bin string, args ...string) *proc {
	t.Helper()
	return startArgsEnv(t, bin, nil, args...)
}

// startArgsEnv は startArgs と同じ。env（"名前=値"）を環境変数に足す。
func startArgsEnv(t *testing.T, bin string, env []string, args ...string) *proc {
	t.Helper()
	for attempt := 1; ; attempt++ {
		s := &proc{addr: freeAddr(t), log: &syncBuffer{}, done: make(chan error, 1), exited: make(chan struct{})}
		s.cmd = exec.Command(bin, append([]string{"-addr", s.addr}, args...)...)
		if env != nil {
			s.cmd.Env = append(os.Environ(), env...)
		}
		s.cmd.Stdout, s.cmd.Stderr = s.log, s.log
		if err := s.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			s.done <- s.cmd.Wait()
			close(s.exited)
		}()
		// テストが途中で落ちても残さない
		t.Cleanup(func() { stopProc(s) })
		if waitReady(t, s) {
			return s
		}
		if attempt == 3 || !strings.Contains(s.log.String(), "address already in use") {
			t.Fatalf("heimdb exited before accepting connections:\n%s", s.log)
		}
	}
}

// waitReady は、HeimDB が受け付けを始めたと出力し、接続できるまで待つ。その前にプロセスが終われば false を返す。
func waitReady(t *testing.T, s *proc) bool {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if strings.Contains(s.log.String(), "Accepting connections") {
			if c, err := net.Dial("tcp", s.addr); err == nil {
				c.Close()
				return true
			}
		}
		select {
		case <-s.exited:
			return false
		default:
		}
		if time.Now().After(deadline) {
			s.cmd.Process.Kill()
			t.Fatalf("heimdb did not start:\n%s", s.log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *proc) db(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", "root:pw@tcp("+s.addr+")/wordpress")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// ledger は、各書き込み手が応答を受け取った（コミットした）記録の番号と、試みた番号。
type ledger struct {
	mu        sync.Mutex
	acked     map[string]bool
	attempted map[string]bool
}

// workload は、srv が止まるまで書き込みを続ける。残高の移動と記録の追加を 1 つのトランザクションで行い、DDL、
// バックアップも並行して行う。
func workload(t *testing.T, srv *proc, cycle int, lg *ledger) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			db := srv.db(t)
			defer db.Close()
			db.SetMaxOpenConns(1)
			r := rand.New(rand.NewSource(int64(cycle*100 + w)))
			for n := 0; ctx.Err() == nil; n++ {
				key := fmt.Sprintf("%d-%d-%d", cycle, w, n)
				lg.mu.Lock()
				lg.attempted[key] = true
				lg.mu.Unlock()
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					return
				}
				a, b, amt := r.Intn(accounts)+1, r.Intn(accounts)+1, r.Intn(5)+1
				_, err1 := tx.Exec("UPDATE acct SET bal = bal - ? WHERE id = ?", amt, a)
				_, err2 := tx.Exec("INSERT INTO ledger (k) VALUES (?)", key)
				_, err3 := tx.Exec("UPDATE acct SET bal = bal + ? WHERE id = ?", amt, b)
				if err := errors.Join(err1, err2, err3); err != nil {
					tx.Rollback()
					if isLockError(err) {
						continue
					}
					return
				}
				if err := tx.Commit(); err != nil {
					return // 応答が返らなかった（コミットされたかもしれない）
				}
				lg.mu.Lock()
				lg.acked[key] = true
				lg.mu.Unlock()
			}
		}(w)
	}
	// DDL（テーブルの書き直し）とバックアップ。
	wg.Add(1)
	go func() {
		defer wg.Done()
		db := srv.db(t)
		defer db.Close()
		for i := 0; ctx.Err() == nil; i++ {
			col := fmt.Sprintf("c%d_%d", cycle, i)
			if _, err := db.Exec("ALTER TABLE big ADD COLUMN " + col + " int NOT NULL DEFAULT 1"); err != nil {
				return
			}
			if _, err := db.Exec("ALTER TABLE big DROP COLUMN " + col); err != nil {
				return
			}
			if _, err := db.Exec(fmt.Sprintf("CALL heimdb_backup('b%d_%d')", cycle, i)); err != nil {
				return
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

func isLockError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "1205") || strings.Contains(s, "1213")
}

// verify は、起動し直した HeimDB で、合計が保たれ、応答を返した記録がすべてあり、試みていない記録がないことを確かめる。
func verify(t *testing.T, srv *proc, lg *ledger, bigRows int) {
	t.Helper()
	db := srv.db(t)
	defer db.Close()
	var sum, n int
	if err := db.QueryRow("SELECT SUM(bal), COUNT(*) FROM acct").Scan(&sum, &n); err != nil {
		t.Fatalf("read acct: %v\n%s", err, srv.log)
	}
	if sum != accounts*initial || n != accounts {
		t.Errorf("acct: sum %d over %d rows, want %d over %d", sum, n, accounts*initial, accounts)
	}
	rows, err := db.Query("SELECT k FROM ledger")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var k string
		rows.Scan(&k)
		have[k] = true
	}
	rows.Close()
	lg.mu.Lock()
	defer lg.mu.Unlock()
	for k := range lg.acked {
		if !have[k] {
			t.Errorf("acknowledged commit %s is missing", k)
		}
	}
	for k := range have {
		if !lg.attempted[k] {
			t.Errorf("ledger has %s that was never written", k)
		}
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM big").Scan(&n); err != nil || n != bigRows {
		t.Errorf("big: %d rows (%v), want %d", n, err, bigRows)
	}
	// そのまま書き込める（AUTO_INCREMENT が重複しない）。
	if _, err := db.Exec("INSERT INTO ledger (k) VALUES ('after-restart')"); err != nil {
		t.Errorf("write after restart: %v", err)
	}
	if _, err := db.Exec("DELETE FROM ledger WHERE k = 'after-restart'"); err != nil {
		t.Error(err)
	}
	if strings.Contains(srv.log.String(), "panic") {
		t.Errorf("heimdb log has a panic:\n%s", srv.log)
	}
}

func setup(t *testing.T, srv *proc) int {
	t.Helper()
	db := srv.db(t)
	defer db.Close()
	// 名乗るバージョンと製品名（main.go の serverVersion）。
	var version, comment, fn string
	if err := db.QueryRow("SELECT @@version, @@version_comment, VERSION()").Scan(&version, &comment, &fn); err != nil {
		t.Fatal(err)
	}
	if version != serverVersion || comment != "HeimDB" || fn != serverVersion {
		t.Errorf("version %q, version_comment %q, VERSION() %q", version, comment, fn)
	}
	for _, q := range []string{
		"CREATE TABLE acct (id int NOT NULL PRIMARY KEY, bal int NOT NULL)",
		"CREATE TABLE ledger (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, k varchar(64) NOT NULL, UNIQUE KEY k (k))",
		"CREATE TABLE big (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(100) NOT NULL)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= accounts; i++ {
		if _, err := db.Exec("INSERT INTO acct VALUES (?, ?)", i, initial); err != nil {
			t.Fatal(err)
		}
	}
	const bigRows = 2000
	var vals []string
	for i := 0; i < bigRows; i++ {
		vals = append(vals, fmt.Sprintf("('row %d')", i))
	}
	if _, err := db.Exec("INSERT INTO big (v) VALUES " + strings.Join(vals, ",")); err != nil {
		t.Fatal(err)
	}
	return bigRows
}

// kill -9 で何度も落とし、そのたびに起動し直して確かめる。
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs heimdb")
	}
	bin := heimdbBinary(t)
	dir, backupDir := t.TempDir(), t.TempDir()
	lg := &ledger{acked: map[string]bool{}, attempted: map[string]bool{}}
	srv := start(t, bin, dir, backupDir)
	bigRows := setup(t, srv)
	r := rand.New(rand.NewSource(1))
	for cycle := 0; cycle < 6; cycle++ {
		stop := workload(t, srv, cycle, lg)
		time.Sleep(time.Duration(300+r.Intn(1200)) * time.Millisecond)
		srv.cmd.Process.Signal(syscall.SIGKILL)
		<-srv.done
		stop()
		srv = start(t, bin, dir, backupDir)
		verify(t, srv, lg, bigRows)
		if t.Failed() {
			break
		}
	}
	lg.mu.Lock()
	t.Logf("acknowledged commits: %d", len(lg.acked))
	lg.mu.Unlock()
	srv.cmd.Process.Signal(syscall.SIGKILL)
	<-srv.done
}

// SIGTERM で止めると、実行中の文を待ってから閉じ、panic せず、新しい文はエラー 1053 で拒否する。
func TestGracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs heimdb")
	}
	bin := heimdbBinary(t)
	dir, backupDir := t.TempDir(), t.TempDir()
	lg := &ledger{acked: map[string]bool{}, attempted: map[string]bool{}}
	srv := start(t, bin, dir, backupDir)
	bigRows := setup(t, srv)
	r := rand.New(rand.NewSource(2))
	for cycle := 0; cycle < 4; cycle++ {
		// 開いたままのトランザクションを持つ接続（停止の後の次の文は拒否される）。主キーで 1 行を引き、トランザクションに
		// Pebble のイテレータ（PointReader）を持たせる。止めるときに片付けないと、ストアを閉じるときに Pebble が panic
		// する（docs/plan.md の段階 41。起動し直した後は行が SST にあり、イテレータがファイルを開く）。
		idle := srv.db(t)
		idle.SetMaxOpenConns(1)
		tx, err := idle.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("SELECT COUNT(*) FROM acct"); err != nil {
			t.Fatal(err)
		}
		var bal int
		if err := tx.QueryRow("SELECT bal FROM acct WHERE id = 1").Scan(&bal); err != nil {
			t.Fatal(err)
		}
		stop := workload(t, srv, cycle, lg)
		time.Sleep(time.Duration(300+r.Intn(800)) * time.Millisecond)
		// 実行中の長い文（停止はこれを待つので、その間プロセスは生きている）。
		sleeper := srv.db(t)
		slept := make(chan error, 1)
		go func() {
			_, err := sleeper.Exec("SELECT SLEEP(2)")
			slept <- err
		}()
		time.Sleep(200 * time.Millisecond)
		srv.cmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(300 * time.Millisecond)
		// プロセスが生きている間に、開いたままのトランザクションの次の文はエラー 1053 で拒否される。
		if _, err := tx.Exec("SELECT 1"); mysqlErrNo(err) != 1053 {
			t.Errorf("cycle %d: a statement after SIGTERM: %v, want error 1053", cycle, err)
		}
		if err := <-slept; err != nil {
			t.Errorf("cycle %d: the running statement did not finish: %v", cycle, err)
		}
		sleeper.Close()
		err = <-srv.done
		stop()
		if err != nil {
			t.Errorf("cycle %d: heimdb exited with %v:\n%s", cycle, err, srv.log)
		}
		if !strings.Contains(srv.log.String(), "stopped") {
			t.Errorf("cycle %d: heimdb did not close the store:\n%s", cycle, srv.log)
		}
		idle.Close()
		srv = start(t, bin, dir, backupDir)
		verify(t, srv, lg, bigRows)
		if t.Failed() {
			break
		}
	}
	srv.cmd.Process.Signal(syscall.SIGTERM)
	<-srv.done
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
