package backend

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 段階 16 のレビュー。

// バックアップは、別のセッションがブランチの書き込みロックを持っていても（BEGIN の中で UPDATE したまま）待たずに取れ、
// 未コミットの変更を含まない。自分のセッションの未コミットの変更も同じ。その後のコミットは成功する。
func TestReview16BackupDoesNotWaitForOpenTransactions(t *testing.T) {
	backupDir := t.TempDir()
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mustExec(t, root, "CREATE TABLE t (id int NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(20))")
	mustExec(t, root, "INSERT INTO t (v) VALUES ('committed')")

	other, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO t (v) VALUES ('uncommitted')"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var path, at string
	go func() {
		defer close(done)
		path, at, _ = backupRow(t, root, "CALL heimdb_backup('open-txn')")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("heimdb_backup waited for a transaction holding the branch write lock")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after backup: %v", err)
	}
	if got := one(t, root, "SELECT COUNT(*) FROM t AS OF '"+at+"'"); got != "1" {
		t.Errorf("source AS OF backup time: %s rows", got)
	}

	baddr, bstop := startBackupServer(t, path, "")
	defer bstop()
	b, err := connectAs(t, baddr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := one(t, b, "SELECT GROUP_CONCAT(v) FROM t"); got != "committed" {
		t.Errorf("backup rows = %q", got)
	}
	// 復元先の AUTO_INCREMENT は、払い出し済みで未コミットだった 2 を再利用してもよい（元では 2 がコミットされた）。
	mustExec(t, b, "INSERT INTO t (v) VALUES ('after')")
	if got := one(t, b, "SELECT id FROM t WHERE v = 'after'"); got != "2" {
		t.Errorf("restored AUTO_INCREMENT gave %s", got)
	}
}

// 一時テーブル（隠しブランチ）があるときのバックアップは開け、一時テーブルの残りは起動時に片付く。
func TestReview16BackupWithTemporaryTable(t *testing.T) {
	backupDir := t.TempDir()
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.SetMaxOpenConns(1)
	mustExec(t, root, "CREATE TABLE t (id int NOT NULL PRIMARY KEY)")
	mustExec(t, root, "CREATE TEMPORARY TABLE tmp (id int NOT NULL PRIMARY KEY)")
	mustExec(t, root, "INSERT INTO tmp VALUES (1), (2)")
	path, _, _ := backupRow(t, root, "CALL heimdb_backup('tmp')")

	baddr, bstop := startBackupServer(t, path, "")
	defer bstop()
	b, err := connectAs(t, baddr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := one(t, b, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'wordpress' AND table_name = 'tmp'"); got != "0" {
		t.Errorf("temporary table survived in the backup: %s", got)
	}
	if _, err := b.Query("SELECT * FROM tmp"); err == nil {
		t.Error("temporary table of the source is visible in the backup")
	}
}

// ブランチの作成と削除（削除は 2 回のコミット: 登録の削除、データの削除）が続いている最中に取ったバックアップは
// 開け、ブランチは「ある（読める）」か「ない」のどちらか。
func TestReview16BackupDuringBranchDelete(t *testing.T) {
	backupDir := t.TempDir()
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mustExec(t, root, "CREATE TABLE t (id int NOT NULL PRIMARY KEY, v int)")
	for i := 0; i < 50; i++ {
		mustExec(t, root, fmt.Sprintf("INSERT INTO t VALUES (%d, 0)", i))
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		db, err := connectAs(t, addr, "root", "rootpw", testDB)
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			if _, err := db.Exec("CALL heimdb_branch_create('s')"); err != nil {
				t.Error(err)
				return
			}
			if _, err := db.Exec("UPDATE `wordpress@s`.t SET v = v + 1"); err != nil {
				t.Error(err)
				return
			}
			if _, err := db.Exec("CALL heimdb_branch_delete('s')"); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var paths []string
	for i := 0; i < 8; i++ {
		time.Sleep(10 * time.Millisecond)
		p, _, _ := backupRow(t, root, fmt.Sprintf("CALL heimdb_backup('d%d')", i))
		paths = append(paths, p)
	}
	close(stopCh)
	wg.Wait()

	for _, p := range paths {
		baddr, bstop := startBackupServer(t, p, "")
		b, err := connectAs(t, baddr, "root", "rootpw", testDB)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		rows, err := b.Query("CALL heimdb_branch_list()")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			var parent, forkedAt, database sql.NullString
			var size int64
			var maxSize, maxExec sql.NullInt64
			if err := rows.Scan(&name, &parent, &forkedAt, &database, &size, &maxSize, &maxExec); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		rows.Close()
		var n sql.NullString
		err = b.QueryRow("SELECT COUNT(*) FROM `wordpress@s`.t").Scan(&n)
		switch {
		case strings.Contains(","+strings.Join(names, ",")+",", ",s,"):
			if err != nil || n.String != "50" {
				t.Errorf("%s: branch s: %v rows, %v", p, n.String, err)
			}
		case err == nil:
			t.Errorf("%s: deleted branch is readable (%s rows)", p, n.String)
		}
		t.Logf("%s: branches %v", p, names)
		b.Close()
		bstop()
	}
}

// 名前を省略した 2 回のバックアップが同じ秒に入っても、どちらも成功する（以前は名前が秒の粒度で、2 回目が
// 「already exists」で失敗していた。名前はマイクロ秒まで含める）。
func TestReview16DefaultNameCollidesWithinASecond(t *testing.T) {
	addr, stop := startBackupServer(t, t.TempDir(), t.TempDir())
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mustExec(t, root, "CREATE TABLE t (id int NOT NULL PRIMARY KEY)")
	// 秒の境界をまたがないよう、秒の初めまで待つ。
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now))
	var errs []error
	for i := 0; i < 2; i++ {
		_, err := root.Exec("CALL heimdb_backup()")
		errs = append(errs, err)
	}
	if errs[0] != nil {
		t.Fatal(errs[0])
	}
	if errs[1] != nil {
		t.Errorf("second heimdb_backup() in the same second: %v", errs[1])
	}
}

// 作られるディレクトリとファイルのパーミッション（バックアップには m/users のパスワードハッシュが入る）。
func TestReview16BackupPermissions(t *testing.T) {
	backupDir := filepath.Join(t.TempDir(), "backups")
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path, _, _ := backupRow(t, root, "CALL heimdb_backup('p')")
	for _, d := range []string{backupDir, path} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %v", d, info.Mode())
	}
	// HeimDB が作った置き場所は、HeimDB のユーザーだけが入れる（中の Pebble のファイルの権限にかかわらず読めない）。
	if info, err := os.Stat(backupDir); err == nil && info.Mode().Perm() != 0o700 {
		t.Errorf("backup dir mode = %v, want 0700", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		info, _ := e.Info()
		t.Logf("%s: %v", e.Name(), info.Mode())
	}
	// LOCK は写さない（元のサーバが動いたまま開ける）。
	if _, err := os.Stat(filepath.Join(path, "LOCK")); err == nil {
		t.Error("LOCK file was copied into the backup")
	}
}

// 接続が切れず、panic しないこと: 不正な名前、存在する名前、存在しない backup-dir の親。
func TestReview16BackupErrorsKeepConnection(t *testing.T) {
	backupDir := t.TempDir()
	addr, stop := startBackupServer(t, t.TempDir(), backupDir)
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.SetMaxOpenConns(1)
	// 名前と同じ名前のシンボリックリンク（壊れたもの）が置き場所にある。
	if err := os.Symlink(filepath.Join(backupDir, "nowhere"), filepath.Join(backupDir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec("CALL heimdb_backup('link')"); err == nil {
		t.Error("backup through a dangling symlink succeeded")
	} else {
		t.Logf("dangling symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "nowhere")); err == nil {
		t.Error("the backup was written through the symlink")
	}
	// 置き場所がファイル。
	f := filepath.Join(backupDir, "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Exec("CALL heimdb_backup('file')"); err == nil {
		t.Error("backup over an existing file succeeded")
	}
	if got := one(t, root, "SELECT 1"); got != "1" {
		t.Error("connection lost")
	}
	var n sql.NullString
	if err := root.QueryRow("SELECT COUNT(*) FROM information_schema.tables").Scan(&n); err != nil {
		t.Error(err)
	}
}
