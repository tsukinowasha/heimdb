package backend

import (
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startBackupServer は、データディレクトリ dir、バックアップの置き場所 backupDir で、認証つきのサーバを起動する。
func startBackupServer(t *testing.T, dir, backupDir string) (addr string, stop func()) {
	t.Helper()
	addr, _, st, srv := serve(t, dir, serverConfig{auth: true, rootPassword: "rootpw", provider: func(p *Provider) { p.BackupDir = backupDir }})
	return addr, stopServer(t, st, srv)
}

func backupRow(t *testing.T, db *sql.DB, q string) (path, at string, size int64) {
	t.Helper()
	if err := db.QueryRow(q).Scan(&path, &at, &size); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return path, at, size
}

// 書き込みが続いている最中に取ったバックアップは、ある時点のコミットまでをすべて含み、それより後を含まない。
// 残高の移動（合計が変わらない）を並行して続け、各バックアップを開いて、合計と、元のサーバの AS OF（バックアップの
// 時刻）と同じ内容かを確かめる。
func TestBackupWhileWriting(t *testing.T) {
	backupDir := t.TempDir()
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mustExec(t, root, "CREATE TABLE acct (id int NOT NULL PRIMARY KEY, bal int NOT NULL)")
	for i := 0; i < 10; i++ {
		mustExec(t, root, fmt.Sprintf("INSERT INTO acct VALUES (%d, 100)", i))
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			db, err := connectAs(t, addr, "root", "rootpw", testDB)
			if err != nil {
				t.Error(err)
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			r := rand.New(rand.NewSource(seed))
			for {
				select {
				case <-done:
					return
				default:
				}
				from, to, amt := r.Intn(10), r.Intn(10), r.Intn(5)+1
				tx, err := db.Begin()
				if err != nil {
					t.Error(err)
					return
				}
				_, err1 := tx.Exec("UPDATE acct SET bal = bal - ? WHERE id = ?", amt, from)
				_, err2 := tx.Exec("UPDATE acct SET bal = bal + ? WHERE id = ?", amt, to)
				if err1 != nil || err2 != nil {
					tx.Rollback() // ロックの待ちの打ち切りなどは、やり直さずに次へ
					continue
				}
				tx.Commit()
			}
		}(int64(w))
	}
	type taken struct{ path, at string }
	var backups []taken
	for i := 0; i < 5; i++ {
		time.Sleep(50 * time.Millisecond)
		path, at, _ := backupRow(t, root, fmt.Sprintf("CALL heimdb_backup('b%d')", i))
		backups = append(backups, taken{path, at})
	}
	close(done)
	wg.Wait()

	states := map[string]bool{}
	for _, b := range backups {
		want := one(t, root, "SELECT GROUP_CONCAT(CONCAT(id, ':', bal) ORDER BY id) FROM acct AS OF '"+b.at+"'")
		states[want] = true
		baddr, bstop := startBackupServer(t, b.path, "")
		db, err := connectAs(t, baddr, "root", "rootpw", testDB)
		if err != nil {
			t.Fatal(err)
		}
		if got := one(t, db, "SELECT SUM(bal) FROM acct"); got != "1000" {
			t.Errorf("%s: sum = %s", b.path, got)
		}
		if got := one(t, db, "SELECT GROUP_CONCAT(CONCAT(id, ':', bal) ORDER BY id) FROM acct"); got != want {
			t.Errorf("%s: rows = %s, want the state AS OF %s: %s", b.path, got, b.at, want)
		}
		db.Close()
		bstop()
	}
	if len(states) < 2 {
		t.Errorf("all backups have the same state; the writers did not run between them")
	}
}

// バックアップから起動すると、取った時点の状態（ブランチ、ユーザーと権限、AS OF の履歴）に戻る。
func TestBackupRestore(t *testing.T) {
	backupDir := t.TempDir()
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...),
		"INSERT INTO wp_options (option_name, option_value) VALUES ('blogname', 'v1')",
	) {
		mustExec(t, root, q)
	}
	time.Sleep(20 * time.Millisecond)
	v1 := one(t, root, "SELECT NOW(6)")
	time.Sleep(20 * time.Millisecond)
	for _, q := range []string{
		"UPDATE wp_options SET option_value = 'v2' WHERE option_name = 'blogname'",
		"CALL heimdb_branch_create('staging')",
		"CREATE USER 'staging'@'%' IDENTIFIED BY 'stpw'",
		"GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%'",
	} {
		mustExec(t, root, q)
	}
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, stg, "UPDATE wp_options SET option_value = 'staging' WHERE option_name = 'blogname'")
	stg.Close()

	path, _, size := backupRow(t, root, "CALL heimdb_backup()")
	if filepath.Dir(path) != backupDir || size <= 0 {
		t.Errorf("backup path %s (dir %s), size %d", path, backupDir, size)
	}
	// バックアップの後の変更は戻した後に残らない。
	for _, q := range []string{
		"UPDATE wp_options SET option_value = 'v3' WHERE option_name = 'blogname'",
		"CALL heimdb_branch_delete('staging')",
		"DROP USER 'staging'@'%'",
	} {
		mustExec(t, root, q)
	}

	baddr, bstop := startBackupServer(t, path, "")
	defer bstop()
	r, err := connectAs(t, baddr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := one(t, r, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "v2" {
		t.Errorf("restored blogname = %s, want v2", got)
	}
	if got := one(t, r, "SELECT option_value FROM wp_options AS OF '"+v1+"' WHERE option_name = 'blogname'"); got != "v1" {
		t.Errorf("restored AS OF = %s, want v1", got)
	}
	s, err := connectAs(t, baddr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatalf("staging user after restore: %v", err)
	}
	defer s.Close()
	if got := one(t, s, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "staging" {
		t.Errorf("restored staging blogname = %s", got)
	}
	if _, err := connectAs(t, baddr, "staging", "stpw", testDB); !dbAccessDenied(err) {
		t.Errorf("staging on main after restore: %v", err)
	}
	// 戻した先にも書ける（AUTO_INCREMENT も続きから）。
	mustExec(t, r, "INSERT INTO wp_options (option_name, option_value) VALUES ('after_restore', 'x')")
}

// 名前と置き場所の確かめ。管理者だけが実行できる。
func TestBackupNamesAndPermissions(t *testing.T) {
	addr, stop := startBackupServer(t, t.TempDir(), t.TempDir())
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"../escape", "a/b", ".hidden", "x y"} { // 空の名前は省略と同じ（時刻の名前）
		if _, err := root.Exec("CALL heimdb_backup(?)", name); err == nil {
			t.Errorf("backup name %q accepted", name)
		}
	}
	mustExec(t, root, "CALL heimdb_backup('daily-1')")
	if _, err := root.Exec("CALL heimdb_backup('daily-1')"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate backup name: %v", err)
	}
	mustExec(t, root, "CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'")
	mustExec(t, root, "GRANT ALL ON wordpress.* TO 'wp'@'%'")
	wp, err := connectAs(t, addr, "wp", "wppw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer wp.Close()
	if _, err := wp.Exec("CALL heimdb_backup('by-wp')"); err == nil || !strings.Contains(err.Error(), "command denied") {
		t.Errorf("wp: heimdb_backup: %v", err)
	}

	// -backup-dir がなければエラー。
	addr2, stop2 := startBackupServer(t, t.TempDir(), "")
	defer stop2()
	r2, err := connectAs(t, addr2, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.Exec("CALL heimdb_backup()"); err == nil || !strings.Contains(err.Error(), "-backup-dir") {
		t.Errorf("backup without -backup-dir: %v", err)
	}
}
