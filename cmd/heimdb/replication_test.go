package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// レプリカの試験（docs/plan.md の段階 24）。本番に書き込み、ブランチの作成と削除、ユーザーの作成、DDL を続けている
// 最中にバックアップからレプリカを足し、途中で止めて再開する。書き込みを止めて追いついた後、すべてのブランチの
// すべてのテーブル、AS OF の過去の時点、ユーザーと権限が本番と同じになることを確かめる。最後に本番を止めて
// レプリカを昇格させ、書き込めることを確かめる。

const replToken = "secret-token"

func TestReplicaCatchesUpAndPromotes(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	replAddr := freeAddr(t)
	backups := t.TempDir()
	primary := startArgs(t, bin, "-data", t.TempDir(), "-backup-dir", backups, "-root-password", "pw",
		"-replication-addr", replAddr, "-replication-token", replToken, "-shutdown-timeout", "10s")
	defer stopProc(primary)

	pdb := primary.db(t)
	defer pdb.Close()
	mustExec(t, pdb, "CREATE TABLE posts (ID bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, title varchar(100) NOT NULL, d datetime NOT NULL, KEY d (d))")
	mustExec(t, pdb, "CREATE TABLE meta (meta_id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id bigint unsigned NOT NULL, k varchar(50) NOT NULL, v longtext, KEY post_id (post_id))")
	for i := 0; i < 20; i++ {
		mustExec(t, pdb, "INSERT INTO posts (title, d) VALUES (?, NOW())", fmt.Sprintf("seed %d", i))
	}
	// 主キーのない表（段階 72。行 ID で並べる表と、一意キーで並べる表）
	mustExec(t, pdb, "CREATE TABLE lg (ip varchar(39) NOT NULL, stamp int NOT NULL, KEY ip (ip))")
	mustExec(t, pdb, "CREATE TABLE lk (ip varchar(39) NOT NULL, n int NOT NULL, UNIQUE KEY ip (ip))")
	mustExec(t, pdb, "INSERT INTO lg VALUES ('a', 1), ('b', 2), ('a', 1)")
	mustExec(t, pdb, "INSERT INTO lk VALUES ('a', 1), ('b', 2)")

	stop := replicaWorkload(t, primary)
	time.Sleep(time.Second)
	var backupDir, backedUpAt string
	var size int64
	if err := pdb.QueryRow("CALL heimdb_backup('replica')").Scan(&backupDir, &backedUpAt, &size); err != nil {
		t.Fatal(err)
	}
	replicaArgs := []string{"-data", backupDir, "-replicate-from", replAddr, "-replication-token", replToken,
		"-replica-name", "r1", "-shutdown-timeout", "10s"}
	replica := startArgs(t, bin, replicaArgs...)
	time.Sleep(time.Second)
	var asOf string
	if err := pdb.QueryRow("SELECT DATE_FORMAT(NOW(6), '%Y-%m-%d %H:%i:%s.%f')").Scan(&asOf); err != nil {
		t.Fatal(err)
	}
	// レプリカを止めて、書き込みが続く間に再開する
	stopProc(replica)
	time.Sleep(500 * time.Millisecond)
	replica = startArgs(t, bin, replicaArgs...)
	defer stopProc(replica)
	time.Sleep(time.Second)
	stop()
	mustExec(t, pdb, "INSERT INTO lg VALUES ('c', 3), ('a', 1)")
	mustExec(t, pdb, "DELETE FROM lg WHERE ip = 'a' LIMIT 1")
	mustExec(t, pdb, "UPDATE lk SET n = 5 WHERE ip = 'a'")
	// 明示した AUTO_INCREMENT の値（昇格の後は、その続きから払い出す）
	mustExec(t, pdb, "INSERT INTO meta (meta_id, post_id, k) VALUES (90000, 1, 'explicit')")
	var maxPost uint64
	if err := pdb.QueryRow("SELECT MAX(ID) FROM posts").Scan(&maxPost); err != nil {
		t.Fatal(err)
	}

	rdb := replica.db(t)
	defer rdb.Close()
	waitCaughtUp(t, pdb, rdb, replica)

	if got, want := dumpAll(t, rdb, ""), dumpAll(t, pdb, ""); got != want || !strings.Contains(want, "== wordpress@b") || !strings.Contains(want, "\t7") {
		t.Errorf("replica differs from primary:\n--- replica\n%s\n--- primary\n%s", got, want)
	}
	if got, want := dumpAll(t, rdb, asOf), dumpAll(t, pdb, asOf); got != want {
		t.Errorf("replica differs from primary AS OF %s:\n--- replica\n%s\n--- primary\n%s", asOf, got, want)
	}
	userQuery := "SELECT user, host, authentication_string, select_priv, insert_priv FROM mysql.user ORDER BY user, host"
	if got, want := queryAll(t, rdb, userQuery), queryAll(t, pdb, userQuery); got != want || !strings.Contains(want, "u_0") {
		t.Errorf("users differ:\n--- replica\n%s\n--- primary\n%s", got, want)
	}
	// 本番で作ったユーザーで、レプリカに接続できる
	udb, err := sql.Open("mysql", "u_0:pw@tcp("+replica.addr+")/wordpress")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := udb.QueryRow("SELECT COUNT(*) FROM posts").Scan(&n); err != nil || n == 0 {
		t.Errorf("user created on the primary, on the replica: %d rows, %v", n, err)
	}
	udb.Close()

	// レプリカは書けない
	if _, err := rdb.Exec("INSERT INTO posts (title, d) VALUES ('x', NOW())"); err == nil || !strings.Contains(err.Error(), "read-only replica") {
		t.Errorf("write on the replica: %v", err)
	}
	if _, err := rdb.Exec("CALL heimdb_branch_create('nope')"); err == nil || !strings.Contains(err.Error(), "read-only replica") {
		t.Errorf("branch on the replica: %v", err)
	}
	// 表の手入れの文は何も書かないので、レプリカでも通る（docs/plan.md の段階 74）
	for _, q := range []string{"OPTIMIZE TABLE posts", "CHECK TABLE posts", "REPAIR TABLE posts", "ANALYZE TABLE posts"} {
		if _, err := rdb.Exec(q); err != nil {
			t.Errorf("%s on the replica: %v", q, err)
		}
	}
	status := queryAll(t, pdb, "CALL heimdb_status()")
	if !strings.Contains(status, "replica r1") || !strings.Contains(status, "behind 0s") {
		t.Errorf("primary status:\n%s", status)
	}

	// 昇格: 本番を止め、レプリカを -replicate-from なしで起動し直す
	stopProc(primary)
	stopProc(replica)
	promoted := startArgs(t, bin, "-data", backupDir, "-shutdown-timeout", "10s")
	defer stopProc(promoted)
	ndb := promoted.db(t)
	defer ndb.Close()
	// AUTO_INCREMENT は本番の続きから払い出す
	if res, err := ndb.Exec("INSERT INTO posts (title, d) VALUES ('after promotion', NOW())"); err != nil {
		t.Errorf("write after promotion: %v\n%s", err, promoted.log)
	} else if id, _ := res.LastInsertId(); uint64(id) != maxPost+1 {
		t.Errorf("posts ID after promotion: %d, primary max %d", id, maxPost)
	}
	if res, err := ndb.Exec("INSERT INTO meta (post_id, k) VALUES (1, 'after promotion')"); err != nil {
		t.Errorf("write after promotion: %v", err)
	} else if id, _ := res.LastInsertId(); id != 90001 {
		t.Errorf("meta_id after promotion: %d, want 90001", id)
	}
	mustExec(t, ndb, "INSERT INTO lg VALUES ('d', 4)")
	mustExec(t, ndb, "DELETE FROM lg WHERE ip = 'a' LIMIT 1")
	if got := queryAll(t, ndb, "SELECT CONCAT(ip, stamp) FROM lg"); got != "b2\nc3\na1\nd4" {
		t.Errorf("keyless table after promotion: %q", got)
	}
	if _, err := ndb.Exec("CALL heimdb_branch_create('after')"); err != nil {
		t.Errorf("branch after promotion: %v", err)
	}
	for _, p := range []*proc{primary, replica, promoted} {
		if strings.Contains(p.log.String(), "panic") {
			t.Errorf("heimdb log has a panic:\n%s", p.log)
		}
	}
}

// replicaWorkload は、止めるまで本番に書き込み続ける。行の挿入、更新、削除と、ブランチの作成と書き込みと削除、
// ユーザーの作成と権限、列の追加と削除。
func replicaWorkload(t *testing.T, srv *proc) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	run := func(f func(db *sql.DB, i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := srv.db(t)
			defer db.Close()
			for i := 0; ctx.Err() == nil; i++ {
				if err := f(db, i); err != nil && !isLockError(err) {
					t.Errorf("workload: %v", err)
					return
				}
			}
		}()
	}
	run(func(db *sql.DB, i int) error {
		res, err := db.Exec("INSERT INTO posts (title, d) VALUES (?, NOW())", fmt.Sprintf("post %d", i))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if _, err := db.Exec("INSERT INTO meta (post_id, k, v) VALUES (?, 'views', ?)", id, i); err != nil {
			return err
		}
		if _, err := db.Exec("UPDATE posts SET title = CONCAT(title, '!') WHERE ID = ?", id/2); err != nil {
			return err
		}
		if i%5 == 0 {
			_, err = db.Exec("DELETE FROM posts WHERE ID = ?", id/3)
		}
		return err
	})
	run(func(db *sql.DB, i int) error {
		name := fmt.Sprintf("b%d", i)
		if _, err := db.Exec("CALL heimdb_branch_create(?)", name); err != nil {
			return err
		}
		if _, err := db.Exec("INSERT INTO `wordpress@" + name + "`.posts (title, d) VALUES ('on branch', NOW())"); err != nil {
			return err
		}
		if i%3 == 2 {
			_, err := db.Exec("CALL heimdb_branch_delete(?)", name)
			return err
		}
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	run(func(db *sql.DB, i int) error {
		u := fmt.Sprintf("u_%d", i)
		if _, err := db.Exec("CREATE USER '" + u + "'@'%' IDENTIFIED BY 'pw'"); err != nil {
			return err
		}
		if _, err := db.Exec("GRANT SELECT ON wordpress.* TO '" + u + "'@'%'"); err != nil {
			return err
		}
		col := fmt.Sprintf("c%d", i)
		if _, err := db.Exec("ALTER TABLE meta ADD COLUMN " + col + " int NOT NULL DEFAULT 7"); err != nil {
			return err
		}
		if i%2 == 0 {
			if _, err := db.Exec("ALTER TABLE meta DROP COLUMN " + col); err != nil {
				return err
			}
		}
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

// waitCaughtUp は、レプリカの最後のコミットが本番と同じになるまで待つ。
func waitCaughtUp(t *testing.T, pdb, rdb *sql.DB, replica *proc) {
	t.Helper()
	want := statusValue(t, pdb, "last_commit")
	deadline := time.Now().Add(20 * time.Second)
	for statusValue(t, rdb, "last_commit") != want {
		if time.Now().After(deadline) {
			t.Fatalf("the replica did not catch up to %s:\n%s\n%s", want, queryAll(t, rdb, "CALL heimdb_status()"), replica.log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func statusValue(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	rows, err := db.Query("CALL heimdb_status()")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		if k == name {
			return v.String
		}
	}
	return ""
}

// dumpAll は、すべてのブランチのすべてのテーブルの行を、決まった順の文字列にする。asOf が空でなければその時点を読む
// （その時点にないテーブルはエラーの文を入れる）。
func dumpAll(t *testing.T, db *sql.DB, asOf string) string {
	t.Helper()
	var dbs []string
	for _, line := range strings.Split(queryAll(t, db, "SHOW DATABASES"), "\n") {
		if strings.HasPrefix(line, "wordpress") {
			dbs = append(dbs, line)
		}
	}
	sort.Strings(dbs)
	var b strings.Builder
	for _, d := range dbs {
		for _, table := range strings.Split(queryAll(t, db, "SHOW TABLES FROM `"+d+"`"), "\n") {
			if table == "" {
				continue
			}
			q := fmt.Sprintf("SELECT * FROM `%s`.`%s`", d, table)
			if asOf != "" {
				q += " AS OF '" + asOf + "'"
			}
			fmt.Fprintf(&b, "== %s.%s\n%s\n", d, table, queryOrError(db, q+" ORDER BY 1, 2"))
		}
	}
	return b.String()
}

func queryAll(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	s, err := query(db, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

func queryOrError(db *sql.DB, q string) string {
	s, err := query(db, q)
	if err != nil {
		return "error: " + err.Error()
	}
	return s
}

func query(db *sql.DB, q string) (string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var lines []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		var parts []string
		for _, v := range vals {
			if v.Valid {
				parts = append(parts, v.String)
			} else {
				parts = append(parts, "NULL")
			}
		}
		lines = append(lines, strings.Join(parts, "\t"))
	}
	return strings.Join(lines, "\n"), rows.Err()
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// stopProc は、SIGTERM で止めて終わるのを待つ（もう止まっていれば何もしない。何度呼んでもよい）。終わったかは exited で
// 見る（done は、テストが結果を受け取った後には空なので、待つと止まる。テストが done を受け取った直後、exited を
// 閉じる前に後片付けが走ると、受け取る人のいない done を待ち続けていた。docs/plan.md の段階 41）。
func stopProc(p *proc) {
	select {
	case <-p.exited:
		return
	default:
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(15 * time.Second):
		p.cmd.Process.Kill()
		<-p.exited
	}
}
