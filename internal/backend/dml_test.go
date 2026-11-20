package backend

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	gmsserver "github.com/tsukinowasha/heimdb/internal/sqlengine/server"
	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/sqlcompare"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 期待値は本物の MySQL 8.0 で作る。go-mysql-server の memory 実装は、照合順序を無視した一意制約や
// ロールバックで戻る AUTO_INCREMENT など MySQL と違う点があり、比較の相手にならない。
//
//	go test ./internal/backend -run 'MatchesMySQL' -update -mysql 'root:root@tcp(127.0.0.1:3307)/'
//
// で、MySQL に比較用のデータベースを作り直してスクリプトを流し、testdata の *_golden.txt を書き直す。
var (
	update   = flag.Bool("update", false, "regenerate golden files from a real MySQL server")
	mysqlDSN = flag.String("mysql", "", "DSN of the real MySQL server used with -update (without database name)")
)

// serverConfig は、テストのサーバの構成。
type serverConfig struct {
	auth         bool   // EnableAuth（本番と同じ）。偽なら root（パスワードなし、全権限）を足すだけ
	rootPassword string // auth のときの root のパスワード
	persist      bool   // auth なしでも、ユーザーの変更を保存する（EnableAuth と同じ persister）
	store        []func(*store.Store)
	provider     func(*Provider)
	engine       func(*sqle.Engine)
	rebuild      bool // 起動の後に main の古いインデックスを裏で作り直す（-rebuild-indexes。段階 121）。止める前に pro.Shutdown を呼ぶ
}

// serve は HeimDB を TCP のサーバとして起動する。停止は呼び出し側が行う（stopServer）。
func serve(t testing.TB, dir string, c serverConfig) (addr string, pro *Provider, st *store.Store, srv *gmsserver.Server) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	addr, pro, srv = serveOn(t, st, c)
	return addr, pro, st, srv
}

// serveOn は、開いた Store st の HeimDB を TCP のサーバとして起動する（レプリカの Store にも使う）。
func serveOn(t testing.TB, st *store.Store, c serverConfig) (addr string, pro *Provider, srv *gmsserver.Server) {
	t.Helper()
	for _, opt := range c.store {
		opt(st)
	}
	pro = NewProvider(st, testDB)
	if c.provider != nil {
		c.provider(pro)
	}
	engine := NewEngine(pro)
	if c.engine != nil {
		c.engine(engine)
	}
	if c.auth {
		if _, err := EnableAuth(engine, st, c.rootPassword); err != nil {
			st.Close()
			t.Fatal(err)
		}
	} else {
		// go-mysql-server はデータベースを mysql_db.PrivilegedDatabase で包む（docs/plan.md の段階 14）。
		mdb := engine.Analyzer.Catalog.MySQLDb
		if c.persist {
			mdb.SetPersister(usersPersister{st})
		}
		ed := mdb.Editor()
		mdb.AddSuperUser(ed, "root", "%", "")
		ed.Close()
	}
	srv, err := gmsserver.NewServer(gmsserver.Config{Protocol: "tcp", Address: "127.0.0.1:0"}, engine, gsql.NewContext, NewSessionBuilder(pro), nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	pro.StartIndexRebuild(c.rebuild)
	return srv.Listener.Addr().(*net.TCPAddr).String(), pro, srv
}

// stopServer は、サーバを止めて Store を閉じる関数を返す。
func stopServer(t testing.TB, st *store.Store, srv *gmsserver.Server) func() {
	return func() {
		srv.Close()
		checkNoCaches(t, srv)
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}
}

// checkNoCaches は、エンジンの MemoryManager に文の途中結果のキャッシュが残っていないかを確かめる（docs/plan.md の
// 段階 109）。KILL や切れた接続の文は、サーバを閉じた後に片付けが終わることがあるので、2 秒まで待つ。
func checkNoCaches(t testing.TB, srv *gmsserver.Server) {
	t.Helper()
	n := 0
	for i := 0; i < 200; i++ {
		if n = srv.Engine.MemoryManager.NumCaches(); n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d engine caches left after the statements", n)
}

// afterStatement は、ゴールデンの文を 1 つ流すたびに呼ぶ（checkGoldenRun が置く）。
var afterStatement func(q string)

// startServer は、root（パスワードなし）で接続する DSN を返す。
func startServer(t *testing.T, dir string, opts ...func(*store.Store)) (dsn string, stop func()) {
	t.Helper()
	addr, _, st, srv := serve(t, dir, serverConfig{store: opts})
	return fmt.Sprintf("root@tcp(%s)/%s", addr, testDB), stopServer(t, st, srv)
}

// runScript は、1 本の接続で文を順に実行し、各文の結果を文字列にして返す。
func runScript(t *testing.T, dsn string, stmts []string) []string {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var out []string
	for _, q := range stmts {
		out = append(out, sqlcompare.Run(ctx, conn, q))
		if afterStatement != nil {
			afterStatement(q)
		}
	}
	return out
}

// wpSQLMode は、WordPress が接続時に設定する sql_mode（MySQL 8.0 の既定から wpdb::$incompatible_modes を除いたもの）。
const wpSQLMode = "SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"

func dmlStatements(t *testing.T) []string {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	// 作ったすべてのテーブルの SHOW INDEX FROM を MySQL と比べる。
	for _, q := range wpSchema() {
		name := strings.Fields(strings.TrimPrefix(q, "CREATE TABLE "))[0]
		stmts = append(stmts, "SHOW INDEX FROM "+name, "SHOW FULL COLUMNS FROM "+name)
	}
	stmts = append(stmts, "DESCRIBE wp_posts", "SHOW COLUMNS FROM wp_options",
		"CREATE TABLE cols (id int NOT NULL AUTO_INCREMENT, created datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated timestamp NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, "+
			"price decimal(10,2) NOT NULL DEFAULT '1.50', kind enum('a','b') NOT NULL DEFAULT 'a', note varchar(10) DEFAULT NULL, flag tinyint(1) NOT NULL DEFAULT 0, "+
			"a int, b int, PRIMARY KEY (id), UNIQUE KEY ab (a, b), KEY b (b)) DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_unicode_520_ci",
		"SHOW FULL COLUMNS FROM cols",
		"CREATE TABLE coltypes (id int NOT NULL, b1 bit(1) NOT NULL DEFAULT b'0', b8 bit(8) DEFAULT b'101', bn binary(4) NOT NULL DEFAULT 'ab', vb varbinary(10) DEFAULT 'xyz', "+
			"f float NOT NULL DEFAULT 1.5, d double DEFAULT 0.1, dz decimal(5,2) DEFAULT 0, dt date DEFAULT '2026-10-03', tm time DEFAULT '10:00:00', "+
			"ts timestamp(3) NULL DEFAULT CURRENT_TIMESTAMP(3), ui int unsigned DEFAULT 7, neg int DEFAULT -1, st set('a','b') DEFAULT 'a,b', PRIMARY KEY (id)) DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_unicode_520_ci",
		"SHOW FULL COLUMNS FROM coltypes")
	return append(stmts, dmlScript...)
}

func TestDMLMatchesMySQL(t *testing.T) {
	checkGolden(t, "../testdata/dml_golden.txt", "heimdb_golden", dmlStatements(t))
}

// checkGolden は、stmts を HeimDB に流した結果をゴールデンファイルと比べる。-update では、本物の MySQL の
// データベース mysqlDB を作り直して stmts を流し、ゴールデンファイルを書き直す。
func checkGolden(t *testing.T, goldenFile, mysqlDB string, stmts []string) {
	t.Helper()
	checkGoldenRun(t, goldenFile, mysqlDB, stmts, runScript)
}

// checkGoldenRun は checkGolden と同じ。文を流して結果を文字列にするのを run にする。
func checkGoldenRun(t *testing.T, goldenFile, mysqlDB string, stmts []string, run func(*testing.T, string, []string) []string) {
	t.Helper()
	if *update {
		if *mysqlDSN == "" {
			t.Fatal("-update requires -mysql")
		}
		setup := runScript(t, *mysqlDSN, []string{"DROP DATABASE IF EXISTS " + mysqlDB, "CREATE DATABASE " + mysqlDB})
		t.Logf("setup: %v", setup)
		got := run(t, *mysqlDSN+mysqlDB, stmts)
		var b strings.Builder
		for i, q := range stmts {
			fmt.Fprintf(&b, "-- %s\n%s\n", q, got[i])
		}
		if err := os.WriteFile(goldenFile, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	golden, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{})
	defer stopServer(t, st, srv)()
	// 文ごとに、エンジンのキャッシュが増えたままになっていないかを見る（1 本の接続で順に流すので、文が返った後は 0）
	left := 0
	afterStatement = func(q string) {
		if n := srv.Engine.MemoryManager.NumCaches(); n > left {
			t.Errorf("%d engine caches left after: %s", n-left, q)
			left = n
		}
	}
	defer func() { afterStatement = nil }()
	got := run(t, fmt.Sprintf("root@tcp(%s)/%s", addr, testDB), stmts)
	var b strings.Builder
	for i, q := range stmts {
		fmt.Fprintf(&b, "-- %s\n%s\n", q, got[i])
	}
	// HEIMDB_GOLDEN_GOT にパスがあれば、HeimDB の結果をそこに書く（ゴールデンとの diff を見るため）
	if p := os.Getenv("HEIMDB_GOLDEN_GOT"); p != "" {
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Error(err)
		}
	}
	if b.String() != string(golden) {
		wantBlocks := strings.Split(string(golden), "\n-- ")
		gotBlocks := strings.Split(b.String(), "\n-- ")
		for i := 0; i < len(wantBlocks) && i < len(gotBlocks); i++ {
			if wantBlocks[i] != gotBlocks[i] {
				t.Errorf("mismatch:\n got: %s\nwant: %s", gotBlocks[i], wantBlocks[i])
			}
		}
		if len(wantBlocks) != len(gotBlocks) {
			t.Errorf("got %d results, want %d", len(gotBlocks), len(wantBlocks))
		}
	}
}

// dmlScript は、HeimDB と memory の両方で実行して結果を比べる文の列。
var dmlScript = []string{
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('siteurl', 'http://example.com', 'yes')",
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('home', 'http://example.com', 'yes'), ('blogname', 'Test', 'yes')",
	"SELECT * FROM wp_options ORDER BY option_id",
	"SELECT LAST_INSERT_ID()",
	// 一意制約（照合順序は大文字と小文字を区別しない）
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('HOME', 'x', 'yes')",
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('home', 'http://new.example.com', 'yes') ON DUPLICATE KEY UPDATE option_value = VALUES(option_value)",
	"SELECT option_id, option_name, option_value FROM wp_options WHERE option_name = 'home'",
	"INSERT IGNORE INTO wp_options (option_name, option_value, autoload) VALUES ('home', 'ignored', 'yes')",
	"REPLACE INTO wp_options (option_id, option_name, option_value, autoload) VALUES (2, 'home', 'replaced', 'no')",
	"SELECT * FROM wp_options ORDER BY option_id",
	"INSERT INTO wp_options (option_id, option_name, option_value, autoload) VALUES (100, 'explicit', 'v', 'yes')",
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('after_explicit', 'v', 'yes')",
	"SELECT option_id, option_name FROM wp_options ORDER BY option_id DESC",
	"SELECT option_id FROM wp_options WHERE option_id > 2 AND option_id <= 100 ORDER BY option_id",
	"SELECT option_id FROM wp_options WHERE option_id IN (1, 3, 101) ORDER BY option_id DESC",
	"UPDATE wp_options SET option_value = 'updated' WHERE option_name LIKE 'blog%'",
	"UPDATE wp_options SET option_name = 'siteurl' WHERE option_name = 'blogname'",
	"UPDATE wp_options SET option_id = 50 WHERE option_name = 'explicit'",
	"DELETE FROM wp_options WHERE option_name = 'after_explicit'",
	"SELECT * FROM wp_options ORDER BY option_id",
	// 失敗した文は、その文の変更だけが取り消される
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('ok1', 'v', 'yes'), ('siteurl', 'dup', 'yes')",
	"SELECT option_name FROM wp_options ORDER BY option_id",
	// トランザクション
	"START TRANSACTION",
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('in_tx', 'v', 'yes')",
	"SELECT option_name FROM wp_options WHERE option_name = 'in_tx'",
	"ROLLBACK",
	"SELECT option_name FROM wp_options WHERE option_name = 'in_tx'",
	"SET autocommit = 0",
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('ac0', 'v', 'yes')",
	"DELETE FROM wp_options WHERE option_name = 'home'",
	"SELECT option_name FROM wp_options ORDER BY option_name",
	"ROLLBACK",
	"SELECT option_name FROM wp_options ORDER BY option_name",
	"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('committed', 'v', 'yes')",
	"COMMIT",
	"SET autocommit = 1",
	"SELECT option_name FROM wp_options ORDER BY option_name",
	// セカンダリインデックスを使う検索、結合、並び順
	"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES " +
		"(1, '2026-10-01 10:00:00', 'c', 'A', '', 'publish', 'a', '', '', '', 'post')," +
		"(1, '2026-10-01 10:00:00', 'c', 'B', '', 'publish', 'b', '', '', '', 'post')," +
		"(2, '2026-10-02 10:00:00', 'c', 'C', '', 'draft', 'c', '', '', '', 'post')," +
		"(2, '2026-09-30 10:00:00', 'c', 'D', '', 'publish', 'd', '', '', '', 'page')",
	"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_thumb', 'x'), (2, '_thumb', 'y'), (2, 'other', 'z'), (4, '_thumb', 'w')",
	"SELECT ID, post_title FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC, ID DESC",
	"SELECT ID FROM wp_posts WHERE post_name = 'c'",
	"SELECT post_author, COUNT(*) FROM wp_posts GROUP BY post_author ORDER BY post_author",
	"SELECT p.ID, m.meta_value FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID WHERE m.meta_key = '_thumb' ORDER BY p.ID",
	"SELECT p.post_title FROM wp_posts p WHERE p.ID IN (SELECT post_id FROM wp_postmeta WHERE meta_key = 'other')",
	"SELECT meta_id FROM wp_postmeta WHERE post_id = 2 ORDER BY meta_id DESC",
	// 同順位の並び順（MySQL は type_status_date を等号で引いて走査し、同じ日付の行は ID の順になる）
	"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES " +
		"(3, '2026-10-01 10:00:00', 'c', 'E', '', 'publish', 'e', '', '', '', 'post')," +
		"(1, '2026-10-01 10:00:00', 'c', 'F', '', 'private', 'f', '', '', '', 'post')," +
		"(2, '2026-10-01 10:00:00', 'c', 'G', '', 'publish', 'g', '', '', '', 'post')",
	"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC",
	"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date ASC",
	"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC LIMIT 2",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish'))) ORDER BY wp_posts.post_date DESC LIMIT 0, 3",
	"SELECT FOUND_ROWS()",
	"SELECT wp_posts.* FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC LIMIT 3",
	"SELECT ID FROM wp_posts WHERE post_parent = 0 AND post_type = 'post' ORDER BY ID DESC",
	"SELECT ID FROM wp_posts ORDER BY post_name DESC LIMIT 2",
	"DELETE FROM wp_postmeta WHERE post_id = 2",
	"SELECT meta_id, post_id FROM wp_postmeta ORDER BY meta_id",
	"SELECT COUNT(*) FROM wp_posts",
	// 複数列の主キー
	"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (1, 1), (1, 2), (2, 1)",
	"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (1, 2)",
	"SELECT * FROM wp_term_relationships WHERE object_id = 1 ORDER BY term_taxonomy_id DESC",
	"SELECT object_id FROM wp_term_relationships WHERE term_taxonomy_id = 1 ORDER BY object_id",
}

// 確定した行と AUTO_INCREMENT のカウンタは、再起動後も残る。
func TestRowsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	for _, q := range wpSchema() {
		s.mustQuery(t, q)
	}
	s.mustQuery(t, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('a', '1', 'yes'), ('b', '2', 'yes')")
	s.mustQuery(t, "DELETE FROM wp_options WHERE option_name = 'b'")
	s.mustQuery(t, "SET autocommit = 0")
	s.mustQuery(t, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('uncommitted', '3', 'yes')")
	s.stop(t)

	s = start(t, dir)
	defer s.stop(t)
	got := rowsString(t, s, "SELECT option_id, option_name FROM wp_options ORDER BY option_id")
	if want := "[1 a]\n"; got != want {
		t.Errorf("rows after restart = %q, want %q", got, want)
	}
	// 2（削除した行）と 3（コミットしなかった行）の番号は再利用しない。
	s.mustQuery(t, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('c', '4', 'yes')")
	got = rowsString(t, s, "SELECT option_id FROM wp_options WHERE option_name = 'c'")
	if got != "[3]\n" && got != "[4]\n" {
		t.Errorf("auto increment after restart = %q", got)
	}
}

// 主キーは整数の列と、1 列の CHAR か VARCHAR だけ（docs/plan.md の段階 38）。ほかの形は明示的なエラー。
func TestNonIntegerPrimaryKeyRejected(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	for _, q := range []string{
		"CREATE TABLE s1 (k text NOT NULL, PRIMARY KEY (k(10)))",
		"CREATE TABLE s5 (k decimal(10,2) NOT NULL PRIMARY KEY)",
		"CREATE TABLE s10 (k set('a','b') NOT NULL PRIMARY KEY)",
		"CREATE TABLE s9 (a int NOT NULL, k time NOT NULL, PRIMARY KEY (a, k))",
	} {
		if _, err := query(s.ctx, s.engine, q); err == nil || !strings.Contains(err.Error(), "primary key columns must be integer, CHAR, VARCHAR, BINARY, VARBINARY, DATE, DATETIME, TIMESTAMP or ENUM") && !strings.Contains(err.Error(), "prefix length on primary key") {
			t.Errorf("%s: err = %v", q, err)
		}
	}
	// 文字列、バイト列、日時を含む主キーは作れる（docs/plan.md の段階 70）。
	for _, q := range []string{
		"CREATE TABLE s2 (a varchar(10) NOT NULL, b varchar(10) NOT NULL, PRIMARY KEY (a, b))",
		"CREATE TABLE s3 (a int NOT NULL, b varchar(10) NOT NULL, PRIMARY KEY (a, b))",
		"CREATE TABLE s6 (k varbinary(10) NOT NULL PRIMARY KEY)",
		"CREATE TABLE s7 (k datetime NOT NULL PRIMARY KEY)",
		"CREATE TABLE s8 (k binary(16) NOT NULL PRIMARY KEY)",
		"CREATE TABLE s4 (k enum('a','b') NOT NULL PRIMARY KEY)",
	} {
		if _, err := query(s.ctx, s.engine, q); err != nil {
			t.Errorf("%s: err = %v", q, err)
		}
	}
}

func rowsString(t *testing.T, s *server, q string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range s.mustQuery(t, q) {
		fmt.Fprintf(&b, "%v\n", r)
	}
	return b.String()
}
