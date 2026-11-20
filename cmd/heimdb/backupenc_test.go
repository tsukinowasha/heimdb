package main

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// writeKey は、新しい age の鍵を作り、受取人（公開鍵）のファイルと秘密鍵のファイルを dir に書く。
func writeKey(t *testing.T, dir, name string) (recipients, identity string, id *age.X25519Identity) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	recipients = filepath.Join(dir, name+".pub")
	identity = filepath.Join(dir, name+".key")
	if err := os.WriteFile(recipients, []byte("# "+name+"\n"+id.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return recipients, identity, id
}

// runRestore は、heimdb restore-backup を動かし、終了コードが 0 かと出力を返す。args の前に -addr と -data を付けて、
// Docker の ENTRYPOINT と同じ形にする。
func runRestore(t *testing.T, bin, data string, args ...string) (bool, string) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"-addr", "127.0.0.1:0", "-data", data, "restore-backup"}, args...)...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func emptyDir(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries) == 0
}

// 鍵を指定した heimdb_backup は、WAL を含む store の全体を、受取人に暗号化した 1 つのファイルにする。平文の目印は
// ファイルに入らず、平文の写しも残らない。restore-backup は、正しい秘密鍵でだけ展開し、失敗したら何も残さない
// （docs/plan.md の段階 87）。
func TestEncryptedBackup(t *testing.T) {
	bin := heimdbBinary(t)
	dir, backups, keys := t.TempDir(), t.TempDir(), t.TempDir()
	verifyPub, verifyKey, _ := writeKey(t, keys, "verify")
	otherPub, otherKey, _ := writeKey(t, keys, "other")
	args := []string{"-data", dir, "-backup-dir", backups, "-root-password", "pw", "-backup-key", "verify=" + verifyPub}
	srv := startArgs(t, bin, args...)
	db := srv.db(t)
	mustExec(t, db, "CREATE TABLE secrets (id INT PRIMARY KEY, v TEXT)")
	mustExec(t, db, "INSERT INTO secrets VALUES (1, 'HEIMDB-CANARY-SST-7f3a9b2e')")
	db.Close()
	stopProc(srv)
	// 起動し直すと、前の書き込みは SSTable に入る。後の書き込みは WAL（ログ）だけにある。
	srv = startArgs(t, bin, args...)
	db = srv.db(t)
	defer db.Close()
	mustExec(t, db, "INSERT INTO secrets VALUES (2, 'HEIMDB-CANARY-WAL-1c2d3e4f')")
	canaries := [][]byte{[]byte("HEIMDB-CANARY-SST-7f3a9b2e"), []byte("HEIMDB-CANARY-WAL-1c2d3e4f")}

	// 平文の退避には目印が入る（このテストの確かめが空振りしていないことの確かめ）
	var path, at string
	var size int64
	if err := db.QueryRow("CALL heimdb_backup('plain')").Scan(&path, &at, &size); err != nil {
		t.Fatal(err)
	}
	found := 0
	filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			for _, c := range canaries {
				if bytes.Contains(b, c) {
					found++
				}
			}
		}
		return nil
	})
	if found < 2 {
		t.Fatalf("the plain backup does not contain both canaries (%d)", found)
	}

	if err := db.QueryRow("CALL heimdb_backup('20261030T082028Z', 'verify')").Scan(&path, &at, &size); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(backups, "20261030T082028Z.tar.gz.age"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != size {
		t.Errorf("mode %v size %d (reported %d)", info.Mode().Perm(), info.Size(), size)
	}
	enc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range canaries {
		if bytes.Contains(enc, c) {
			t.Errorf("the encrypted backup contains %s", c)
		}
	}
	entries, _ := os.ReadDir(backups)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "20261030T082028Z.tar.gz.age plain" {
		t.Errorf("backup dir has %v (no plaintext copy or partial file may remain)", names)
	}
	recipient, _ := os.ReadFile(verifyPub)
	pub := strings.TrimSpace(strings.Split(string(recipient), "\n")[1])
	if !strings.Contains(srv.log.String(), "encrypted with key verify to "+pub) {
		t.Errorf("the log does not show the recipient:\n%s", srv.log)
	}
	// 名前の重なり、知らない鍵の名前はエラー（接続は切れない）
	for _, q := range []string{"CALL heimdb_backup('20261030T082028Z', 'verify')", "CALL heimdb_backup('plain', 'verify')", "CALL heimdb_backup('x', 'nokey')", "CALL heimdb_backup('../x', 'verify')", "CALL heimdb_backup('x.partial', 'verify')"} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: no error", q)
		}
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	// 正しい秘密鍵で展開し、-reset-users で開く
	restored := filepath.Join(t.TempDir(), "data")
	if ok, out := runRestore(t, bin, restored, "-in", path, "-identity", verifyKey); !ok {
		t.Fatalf("restore-backup failed:\n%s", out)
	}
	rs := startArgs(t, bin, "-data", restored, "-root-password", "verify-pw", "-reset-users")
	rdb, err := sql.Open("mysql", "root:verify-pw@tcp("+rs.addr+")/wordpress")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var n int
	if err := rdb.QueryRow("SELECT COUNT(*) FROM secrets WHERE v LIKE 'HEIMDB-CANARY-%'").Scan(&n); err != nil || n != 2 {
		t.Errorf("restored rows = %d, %v", n, err)
	}

	// 違う鍵、途中で切れたファイル、空でないディレクトリは失敗し、何も残さない
	truncated := filepath.Join(t.TempDir(), "truncated.tar.gz.age")
	os.WriteFile(truncated, enc[:len(enc)-100], 0o600)
	for _, c := range []struct {
		name string
		args []string
	}{
		{"wrong identity", []string{"-in", path, "-identity", otherKey}},
		{"truncated", []string{"-in", truncated, "-identity", verifyKey}},
		{"no identity", []string{"-in", path}},
	} {
		target := filepath.Join(t.TempDir(), "data")
		if ok, out := runRestore(t, bin, target, c.args...); ok {
			t.Errorf("%s: restore-backup succeeded:\n%s", c.name, out)
		}
		if !emptyDir(t, target) {
			t.Errorf("%s: the data directory is not empty after the failure", c.name)
		}
	}
	if ok, _ := runRestore(t, bin, restored, "-in", path, "-identity", verifyKey); ok {
		t.Error("restore-backup into a non-empty directory succeeded")
	}

	// 受取人のファイルを差し替えると、起動し直さずに次の退避から新しい鍵になり、警告が出る
	other, _ := os.ReadFile(otherPub)
	if err := os.WriteFile(verifyPub, other, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("CALL heimdb_backup('rotated', 'verify')").Scan(&path, &at, &size); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.log.String(), "recipients of backup key verify changed") {
		t.Errorf("no warning about the changed recipients:\n%s", srv.log)
	}
	if ok, _ := runRestore(t, bin, filepath.Join(t.TempDir(), "d"), "-in", path, "-identity", verifyKey); ok {
		t.Error("the backup after the key replacement opened with the old identity")
	}
	if ok, out := runRestore(t, bin, filepath.Join(t.TempDir(), "d"), "-in", path, "-identity", otherKey); !ok {
		t.Errorf("the backup after the key replacement did not open with the new identity:\n%s", out)
	}
}

// -reset-passwords は root とサイトのユーザーのパスワードを上書きし、ほかのユーザーを残す。-reset-users はほかの
// ユーザーを消す。どちらも 2 度目の起動では書き換えず、ユーザーのいない store でも動く。レプリカと -no-auth では
// 拒む。WordPress の表には触れない（docs/plan.md の段階 87）。
func TestResetUsers(t *testing.T) {
	bin := heimdbBinary(t)
	dir := t.TempDir()
	open := func(s *proc, user, pass string) *sql.DB {
		t.Helper()
		d, err := sql.Open("mysql", user+":"+pass+"@tcp("+s.addr+")/wordpress")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	users := func(db *sql.DB) string {
		t.Helper()
		rows, err := db.Query("SELECT user FROM mysql.user ORDER BY user")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var u string
			rows.Scan(&u)
			out = append(out, u)
		}
		return strings.Join(out, " ")
	}
	canLogin := func(s *proc, user, pass string) bool {
		d := open(s, user, pass)
		defer d.Close()
		return d.Ping() == nil
	}

	// 本番: root、サイトのユーザー shop、SQL で作ったユーザー other
	prod := startArgs(t, bin, "-data", dir, "-root-password", "prod-root", "-site-user", "shop", "-site-password", "prod-shop")
	db := open(prod, "root", "prod-root")
	mustExec(t, db, "CREATE TABLE wp_users (ID INT PRIMARY KEY, user_login VARCHAR(60))")
	mustExec(t, db, "INSERT INTO wp_users VALUES (1, 'admin'), (2, 'editor')")
	mustExec(t, db, "CREATE USER 'other'@'%' IDENTIFIED BY 'prod-other'")
	mustExec(t, db, "GRANT SELECT ON wordpress.* TO 'other'@'%'")
	mustExec(t, db, "CREATE USER 'root'@'10.%' IDENTIFIED BY 'prod-local'")
	mustExec(t, db, "CREATE ROLE 'reader'")
	mustExec(t, db, "GRANT 'reader' TO 'other'@'%'")
	db.Close()
	stopProc(prod)

	// 旗がなければ、root のパスワードは今までどおり無視する
	s := startArgs(t, bin, "-data", dir, "-root-password", "verify-root")
	if canLogin(s, "root", "verify-root") || !strings.Contains(s.log.String(), "root password was ignored") {
		t.Errorf("without a flag, the root password was not ignored:\n%s", s.log)
	}
	stopProc(s)

	// -reset-passwords（環境変数でも）
	cmdEnv := []string{"HEIMDB_RESET_PASSWORDS=1"}
	s = startArgsEnv(t, bin, cmdEnv, "-data", dir, "-root-password", "verify-root", "-site-user", "shop", "-site-password", "verify-shop")
	if !canLogin(s, "root", "verify-root") || canLogin(s, "root", "prod-root") || !canLogin(s, "shop", "verify-shop") || canLogin(s, "shop", "prod-shop") {
		t.Errorf("-reset-passwords did not set the passwords:\n%s", s.log)
	}
	db = open(s, "other", "prod-other")
	if err := db.QueryRow("SELECT COUNT(*) FROM wp_users").Scan(new(int)); err != nil {
		t.Errorf("-reset-passwords dropped another user or its grant: %v", err)
	}
	db.Close()
	if !strings.Contains(s.log.String(), "kept root@'10.%' with the passwords from before") {
		t.Errorf("no warning about root@'10.%%':\n%s", s.log)
	}
	db = open(s, "shop", "verify-shop")
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM wp_users").Scan(&n); err != nil || n != 2 {
		t.Errorf("the site user lost its grants or wp_users changed: %d, %v", n, err)
	}
	db.Close()
	if !strings.Contains(s.log.String(), "-reset-passwords: set the passwords of root@'%' and shop@'%'") {
		t.Errorf("no log line for the reset:\n%s", s.log)
	}
	stopProc(s)
	s = startArgsEnv(t, bin, cmdEnv, "-data", dir, "-root-password", "verify-root", "-site-user", "shop", "-site-password", "verify-shop")
	if !strings.Contains(s.log.String(), "-reset-passwords: nothing to change") {
		t.Errorf("the second start with the flag wrote the users again:\n%s", s.log)
	}
	stopProc(s)

	// -reset-users、サイトのパスワードなし: root だけが残る
	s = startArgs(t, bin, "-data", dir, "-root-password", "verify-root2", "-reset-users")
	db = open(s, "root", "verify-root2")
	if got := users(db); got != "root" {
		t.Errorf("users after -reset-users = %q, want root", got)
	}
	// 消したユーザーのロールの結び付きも消える（同じ名前で作り直したユーザーが、本番のロールを引き継がない）
	var edges int
	if err := db.QueryRow("SELECT COUNT(*) FROM mysql.role_edges").Scan(&edges); err != nil || edges != 0 {
		t.Errorf("role edges after -reset-users = %d, %v", edges, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM wp_users").Scan(&n); err != nil || n != 2 {
		t.Errorf("wp_users = %d, %v", n, err)
	}
	db.Close()
	stopProc(s)

	// ユーザーのいない store でも動く（作るだけ）
	fresh := startArgs(t, bin, "-data", t.TempDir(), "-root-password", "x", "-reset-users")
	if !canLogin(fresh, "root", "x") {
		t.Errorf("-reset-users on an empty store:\n%s", fresh.log)
	}
	stopProc(fresh)

	// 拒む組み合わせ
	for _, args := range [][]string{
		{"-reset-users", "-no-auth"},
		{"-reset-users", "-reset-passwords", "-root-password", "x"},
		{"-reset-passwords", "-root-password", "x", "-replicate-from", "127.0.0.1:1", "-replication-token", "t"},
	} {
		cmd := exec.Command(bin, append([]string{"-addr", "127.0.0.1:0", "-data", t.TempDir()}, args...)...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%v: started:\n%s", args, out)
		}
	}
}
