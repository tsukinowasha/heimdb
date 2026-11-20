package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// branchServer は、WordPress のテーブルと数行のデータを入れた HeimDB を起動し、main への接続を返す。
func branchServer(t *testing.T, opts ...func(*store.Store)) (mainDB *sql.DB, dsn string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	dsn, stopServer := startServer(t, dir, opts...)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	seedWordPress(t, db)
	return db, dsn, func() {
		db.Close()
		stopServer()
	}
}

// seedWordPress は、db に WordPress のテーブルと、設定と記事を 1 つずつ作る。
func seedWordPress(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('blogname', 'Prod'), ('siteurl', 'http://prod.example')",
		"INSERT INTO wp_posts (post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES ('first', 'c', '', '', '', '')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// openBranch は、データベース名 wordpress@<name> に接続する。
func openBranch(t *testing.T, dsn, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", strings.Replace(dsn, "/"+testDB, "/"+testDB+"@"+name, 1))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func one(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	var v sql.NullString
	if err := db.QueryRow(q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v.String
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestBranchIsolation(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()

	if got := one(t, staging, "SELECT DATABASE()"); got != "wordpress@staging" {
		t.Errorf("DATABASE() = %q", got)
	}
	if got := one(t, staging, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod" {
		t.Errorf("branch does not see parent data: %q", got)
	}

	// ブランチでの変更は main に見えず、main の変更もブランチに見えない。
	mustExec(t, staging, "UPDATE wp_options SET option_value = 'Staging' WHERE option_name = 'blogname'")
	mustExec(t, staging, "DELETE FROM wp_posts WHERE post_title = 'first'")
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('main_only', '1')")
	checks := []struct {
		db      *sql.DB
		q, want string
	}{
		{mainDB, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'", "Prod"},
		{staging, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'", "Staging"},
		{mainDB, "SELECT COUNT(*) FROM wp_posts", "1"},
		{staging, "SELECT COUNT(*) FROM wp_posts", "0"},
		{mainDB, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'main_only'", "1"},
		{staging, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'main_only'", "0"},
		// インデックス経由の検索も重ね合わせを見る
		{staging, "SELECT option_value FROM wp_options WHERE option_name = 'siteurl'", "http://prod.example"},
	}
	for _, c := range checks {
		if got := one(t, c.db, c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}

	// AUTO_INCREMENT: ブランチは自分の番号帯から採番し、main は今の番号の続き。
	mustExec(t, staging, "INSERT INTO wp_posts (post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES ('staging post', 'c', '', '', '', '')")
	mustExec(t, mainDB, "INSERT INTO wp_posts (post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES ('main post', 'c', '', '', '', '')")
	if got := one(t, staging, "SELECT ID FROM wp_posts WHERE post_title = 'staging post'"); got != "10000000000" {
		t.Errorf("staging post ID = %s, want 10000000000 (branch 2 block)", got)
	}
	if got := one(t, mainDB, "SELECT ID FROM wp_posts WHERE post_title = 'main post'"); got != "2" {
		t.Errorf("main post ID = %s, want 2", got)
	}

	// テーブル定義の変更もブランチの中だけ。
	mustExec(t, staging, "ALTER TABLE wp_options ADD COLUMN staging_only int")
	mustExec(t, staging, "DROP TABLE wp_links")
	mustExec(t, staging, "TRUNCATE TABLE wp_comments")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'wordpress' AND table_name = 'wp_options' AND column_name = 'staging_only'"); got != "0" {
		t.Error("ALTER on branch changed main")
	}
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_links"); got != "0" {
		t.Errorf("main lost wp_links: %s", got)
	}
	if got := one(t, staging, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Staging" {
		t.Errorf("branch rows after rewrite = %q", got)
	}
	if _, err := staging.Exec("SELECT COUNT(*) FROM wp_links"); err == nil {
		t.Error("dropped table still visible on branch")
	}

	// 一覧と information_schema にブランチが出る。
	if got := one(t, mainDB, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = 'wordpress@staging'"); got != "1" {
		t.Errorf("branch database not listed in information_schema")
	}
	rows, err := mainDB.Query("CALL heimdb_branch_list()")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name, db string
		var parent, forked sql.NullString
		var size int64
		var maxSize, maxExec sql.NullInt64
		if err := rows.Scan(&name, &parent, &forked, &db, &size, &maxSize, &maxExec); err != nil {
			t.Fatal(err)
		}
		names = append(names, name+":"+parent.String+":"+db)
	}
	rows.Close()
	if got := strings.Join(names, ","); got != "main::wordpress,staging:main:wordpress@staging" {
		t.Errorf("branch list = %s", got)
	}
}

func TestBranchWritesDoNotBlockEachOther(t *testing.T) {
	mainDB, dsn, stop := branchServer(t, func(st *store.Store) { st.LockTimeout = time.Second })
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()

	ctx := context.Background()
	m, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.ExecContext(ctx, "START TRANSACTION"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecContext(ctx, "UPDATE wp_options SET option_value = 'x' WHERE option_name = 'blogname'"); err != nil {
		t.Fatal(err)
	}
	// main の書き込みロックを持ったトランザクションがあっても、ブランチには書ける。
	mustExec(t, staging, "UPDATE wp_options SET option_value = 'y' WHERE option_name = 'blogname'")
	// 1 つのトランザクションで 2 つのブランチには書けない。
	if _, err := m.ExecContext(ctx, "UPDATE `wordpress@staging`.wp_options SET option_value = 'z' WHERE option_name = 'blogname'"); err == nil || !strings.Contains(err.Error(), "only one branch") {
		t.Errorf("write to a second branch: err = %v", err)
	}
	if _, err := m.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestBranchFromPastAndDelete(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "SET time_zone = '+00:00'")
	time.Sleep(2 * time.Millisecond)
	before := time.Now().UTC().Format("2006-01-02 15:04:05.000000")
	time.Sleep(2 * time.Millisecond)
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'Broken' WHERE option_name = 'blogname'")

	// 壊れる前の時点からブランチを作る（タイムトラベルとの組み合わせ）。
	db, err := mainDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "SET time_zone = '+00:00'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "CALL heimdb_branch_create('rescue', 'main', '"+before+"')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	rescue := openBranch(t, dsn, "rescue")
	if got := one(t, rescue, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod" {
		t.Errorf("branch from the past sees %q, want Prod", got)
	}
	// 孫ブランチ
	mustExec(t, rescue, "CALL heimdb_branch_create('rescue2', 'rescue')")
	if _, err := mainDB.Exec("CALL heimdb_branch_delete('rescue')"); err == nil {
		t.Error("deleting a branch with children should fail")
	}
	mustExec(t, mainDB, "CALL heimdb_branch_delete('rescue2')")
	rescue.Close()
	mustExec(t, mainDB, "CALL heimdb_branch_delete('rescue')")
	gone := openBranch(t, dsn, "rescue")
	defer gone.Close()
	if err := gone.Ping(); err == nil {
		t.Error("connecting to a deleted branch should fail")
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_delete('main')"); err == nil {
		t.Error("deleting main should fail")
	}
}

// 段階 6 のレビューで見つかった問題の再発防止テスト。

// 一時テーブルの作成は、実行中のトランザクションをコミットせず、その後のスナップショットも保つ。
func TestReview6TemporaryTableKeepsTransaction(t *testing.T) {
	mainDB, _, stop := branchServer(t)
	defer stop()
	ctx := context.Background()
	a, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	run := func(q string) {
		t.Helper()
		if _, err := a.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func() string {
		t.Helper()
		var n string
		if err := a.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_options").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// 一時テーブルの作成は、書き込み中のトランザクションをコミットしない。
	run("START TRANSACTION")
	run("INSERT INTO wp_options (option_name, option_value) VALUES ('pending', '1')")
	run("CREATE TEMPORARY TABLE tmp1 (id int NOT NULL, PRIMARY KEY (id))")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'pending'"); got != "0" {
		t.Error("CREATE TEMPORARY TABLE committed the running transaction")
	}
	run("ROLLBACK")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'pending'"); got != "0" {
		t.Error("rolled back row is visible")
	}

	// 一時テーブルを作った後も、トランザクションはスナップショットで読み続ける（他の接続の ALTER の後に空に見えない）。
	// a は main に書いていないので、他の接続の ALTER は待たされない。
	run("START TRANSACTION")
	run("CREATE TEMPORARY TABLE tmp2 (id int NOT NULL, PRIMARY KEY (id))")
	before := count()
	mustExec(t, mainDB, "ALTER TABLE wp_options ADD COLUMN extra int")
	if got := count(); got != before {
		t.Errorf("count inside transaction changed from %s to %s after another session's ALTER", before, got)
	}
	run("COMMIT")
}

// 接続中でないブランチのテーブルにも、自動コミットで書ける（ブランチ間のコピーを含む）。
func TestReview6WriteToOtherBranch(t *testing.T) {
	mainDB, _, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	mustExec(t, mainDB, "INSERT INTO `wordpress@staging`.wp_options (option_name, option_value) VALUES ('from_main', '1')")
	mustExec(t, mainDB, "INSERT INTO wordpress.wp_options (option_name, option_value) VALUES ('copy_me', 'x')")
	mustExec(t, mainDB, "INSERT INTO `wordpress@staging`.wp_options (option_name, option_value) SELECT 'copied', option_value FROM wordpress.wp_options WHERE option_name = 'copy_me'")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM `wordpress@staging`.wp_options WHERE option_name IN ('from_main', 'copied')"); got != "2" {
		t.Errorf("rows written to the other branch = %s, want 2", got)
	}
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_options WHERE option_name IN ('from_main', 'copied')"); got != "0" {
		t.Errorf("rows leaked into main: %s", got)
	}
}

// AUTO_INCREMENT はブランチの番号帯の中で動き、帯の外の ID では進まない。
func TestReview6AutoIncrementStaysInBlock(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	insert := func(db *sql.DB, title string) string {
		t.Helper()
		mustExec(t, db, "INSERT INTO wp_posts (post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES ('"+title+"', 'c', '', '', '', '')")
		return one(t, db, "SELECT ID FROM wp_posts WHERE post_title = '"+title+"'")
	}
	mustExec(t, staging, "ALTER TABLE wp_posts AUTO_INCREMENT = 5")
	if got := insert(staging, "s1"); got != "10000000000" {
		t.Errorf("after AUTO_INCREMENT = 5 on a branch, ID = %s, want 10000000000", got)
	}
	mustExec(t, staging, "INSERT INTO wp_posts (ID, post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES (20000000000, 'far', 'c', '', '', '', '')")
	if got := insert(staging, "s2"); got != "10000000001" {
		t.Errorf("after an ID in another block, ID = %s, want 10000000001", got)
	}
	// ブランチの ID を持つ行が main に入っても（マージを想定）、main のカウンタは main の帯で続く。
	mustExec(t, mainDB, "INSERT INTO wp_posts (ID, post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES (10000000000, 'merged', 'c', '', '', '', '')")
	if got := insert(mainDB, "m1"); got != "2" {
		t.Errorf("main ID after a merged branch row = %s, want 2", got)
	}
}

// 書き込み中のブランチは、待ち続けずに「使用中」で消せない。
func TestReview6DeleteBranchInUse(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('busy')")
	busy := openBranch(t, dsn, "busy")
	defer busy.Close()
	ctx := context.Background()
	c, err := busy.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "START TRANSACTION"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value) VALUES ('w', '1')"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = mainDB.Exec("CALL heimdb_branch_delete('busy')")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("delete busy branch: err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("delete waited %v", d)
	}
	if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_delete('busy')")
}
