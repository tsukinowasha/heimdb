package backend

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// concServer は、並行テスト用に HeimDB を TCP のサーバとして起動し、WordPress のテーブルを作る。
func concServer(t *testing.T, opts ...func(*store.Store)) (db *sql.DB, dir string, stop func()) {
	t.Helper()
	dir = t.TempDir()
	dsn, stopServer := startServer(t, dir, opts...)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(32)
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db, dir, func() {
		db.Close()
		stopServer()
	}
}

// conn は 1 本の接続を開く（セッションの状態を持つ操作に使う）。
func conn(t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func exec(t *testing.T, c *sql.Conn, q string) {
	t.Helper()
	if _, err := c.ExecContext(context.Background(), q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func queryString(t *testing.T, c *sql.Conn, q string) string {
	t.Helper()
	var s sql.NullString
	if err := c.QueryRowContext(context.Background(), q).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

// レビューで再現した問題: 2 つのトランザクションが同じ一意キーの行を入れて、両方コミットできてしまう。
// 書き込みは一度に 1 トランザクションなので、後の INSERT は先のコミットを待ち、重複のエラー（1062）になる。
func TestConcurrentUniqueInsert(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	a, b := conn(t, db), conn(t, db)
	defer a.Close()
	defer b.Close()

	exec(t, a, "START TRANSACTION")
	exec(t, b, "START TRANSACTION")
	exec(t, a, "INSERT INTO wp_options (option_name, option_value) VALUES ('dup', 'a')")
	errCh := make(chan error)
	go func() {
		_, err := b.ExecContext(context.Background(), "INSERT INTO wp_options (option_name, option_value) VALUES ('dup', 'b')")
		errCh <- err
	}()
	select {
	case err := <-errCh:
		t.Fatalf("second INSERT should wait for the lock, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	exec(t, a, "COMMIT")
	if err := <-errCh; mysqlErrNo(err) != 1062 {
		t.Fatalf("second INSERT: err = %v, want 1062", err)
	}
	exec(t, b, "COMMIT")
	if got := queryString(t, a, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'dup'"); got != "1" {
		t.Errorf("rows with option_name 'dup' = %s, want 1", got)
	}
}

// 自動コミットの更新を並行に繰り返しても、取りこぼしが出ない。
func TestConcurrentIncrements(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	if _, err := db.Exec("INSERT INTO wp_options (option_name, option_value) VALUES ('counter', '0')"); err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if _, err := db.Exec("UPDATE wp_options SET option_value = option_value + 1 WHERE option_name = 'counter'"); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("UPDATE: %v", err)
	}
	var v string
	if err := db.QueryRow("SELECT option_value FROM wp_options WHERE option_name = 'counter'").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != fmt.Sprint(workers*each) {
		t.Errorf("counter = %s, want %d", v, workers*each)
	}
}

// トランザクションの中では、他の接続がコミットしても同じ結果が読める。読んだ行を後から書こうとして、
// その行が他で変更されていたらエラー 1213 になる。
func TestSnapshotAndWriteConflict(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	a, b := conn(t, db), conn(t, db)
	defer a.Close()
	defer b.Close()
	exec(t, b, "INSERT INTO wp_options (option_name, option_value) VALUES ('x', '1')")

	exec(t, a, "START TRANSACTION")
	if got := queryString(t, a, "SELECT COUNT(*) FROM wp_options"); got != "1" {
		t.Fatalf("count = %s", got)
	}
	exec(t, b, "INSERT INTO wp_options (option_name, option_value) VALUES ('y', '1')")
	exec(t, b, "UPDATE wp_options SET option_value = '2' WHERE option_name = 'x'")
	if got := queryString(t, a, "SELECT COUNT(*) FROM wp_options"); got != "1" {
		t.Errorf("count inside transaction changed to %s", got)
	}
	if got := queryString(t, a, "SELECT option_value FROM wp_options WHERE option_name = 'x'"); got != "1" {
		t.Errorf("value inside transaction changed to %s", got)
	}
	_, err := a.ExecContext(context.Background(), "UPDATE wp_options SET option_value = '3' WHERE option_name = 'x'")
	if mysqlErrNo(err) != 1213 {
		t.Fatalf("stale update: err = %v, want 1213", err)
	}
	exec(t, a, "ROLLBACK")
	if got := queryString(t, a, "SELECT option_value FROM wp_options WHERE option_name = 'x'"); got != "2" {
		t.Errorf("value after rollback = %s, want 2", got)
	}
}

// ロックを待つ時間が上限を超えるとエラー 1205。エラーで終わった自動コミットの文や、切断された接続は、
// ロックを残さない。
func TestLockTimeoutAndRelease(t *testing.T) {
	db, _, stop := concServer(t, func(st *store.Store) { st.LockTimeout = time.Second })
	defer stop()
	a, b := conn(t, db), conn(t, db)
	defer b.Close()

	exec(t, a, "START TRANSACTION")
	exec(t, a, "INSERT INTO wp_options (option_name, option_value) VALUES ('held', '1')")
	start := time.Now()
	_, err := b.ExecContext(context.Background(), "INSERT INTO wp_options (option_name, option_value) VALUES ('other', '1')")
	if mysqlErrNo(err) != 1205 {
		t.Fatalf("err = %v, want 1205", err)
	}
	if d := time.Since(start); d < 900*time.Millisecond || d > 5*time.Second {
		t.Errorf("waited %v, want about 1s", d)
	}
	// ロックを持ったまま切断すると、ロックは解放され、変更は捨てられる。
	db.SetMaxIdleConns(0) // 閉じた接続をプールに残さず、実際に切断させる
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = b.ExecContext(context.Background(), "INSERT INTO wp_options (option_name, option_value) VALUES ('other', '1')")
		if err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("insert after disconnect: %v", err)
	}
	if got := queryString(t, b, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'held'"); got != "0" {
		t.Errorf("uncommitted row of disconnected session is visible")
	}

	// 自動コミットの文がエラーになっても、ロックは残らない。
	c := conn(t, db)
	defer c.Close()
	if _, err := c.ExecContext(context.Background(), "INSERT INTO wp_options (option_name, option_value) VALUES ('other', 'dup')"); mysqlErrNo(err) != 1062 {
		t.Fatalf("duplicate insert: err = %v", err)
	}
	if _, err := b.ExecContext(context.Background(), "INSERT INTO wp_options (option_name, option_value) VALUES ('after_error', '1')"); err != nil {
		t.Fatalf("insert after failed autocommit statement: %v", err)
	}
}

// 多数の接続でランダムに読み書きしても、エラーにならず、インデックスと行が一致し、一意制約が保たれる。
func TestConcurrentRandomWorkload(t *testing.T) {
	db, dir, stop := concServer(t)
	keys := []string{"_thumb", "_lock", "colour", "Colour"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	unexpected := map[string]int{}
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 9))
			for i := range 120 {
				post, key := r.IntN(10), keys[r.IntN(len(keys))]
				var q string
				switch r.IntN(6) {
				case 0:
					q = fmt.Sprintf("INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (%d, '%s', 'v')", post, key)
				case 1:
					q = fmt.Sprintf("DELETE FROM wp_postmeta WHERE post_id = %d", post)
				case 2:
					q = fmt.Sprintf("UPDATE wp_postmeta SET meta_key = '%s' WHERE post_id = %d", key, post)
				case 3:
					q = fmt.Sprintf("INSERT INTO wp_options (option_name, option_value) VALUES ('opt%d', '%d') ON DUPLICATE KEY UPDATE option_value = VALUES(option_value)", post, i)
				default:
					q = fmt.Sprintf("SELECT meta_id FROM wp_postmeta WHERE meta_key = '%s'", key)
				}
				var err error
				if strings.HasPrefix(q, "SELECT") {
					var rows *sql.Rows
					if rows, err = db.Query(q); err == nil {
						for rows.Next() {
						}
						err = rows.Err()
						rows.Close()
					}
				} else {
					_, err = db.Exec(q)
				}
				if err != nil {
					mu.Lock()
					unexpected[err.Error()]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	for msg, n := range unexpected {
		t.Errorf("%d x %s", n, msg)
	}
	var dups int
	if err := db.QueryRow("SELECT COUNT(*) FROM (SELECT option_name FROM wp_options GROUP BY option_name HAVING COUNT(*) > 1) d").Scan(&dups); err != nil {
		t.Fatal(err)
	}
	if dups != 0 {
		t.Errorf("%d duplicated option names", dups)
	}
	stop()

	s := start(t, dir)
	defer s.stop(t)
	checkIndexConsistency(t, s, "wp_postmeta")
	checkIndexConsistency(t, s, "wp_options")
}
