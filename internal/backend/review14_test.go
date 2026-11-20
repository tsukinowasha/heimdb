package backend

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// 段階 14（認証）のレビューの再現テスト。

// review14Setup は、認証を有効にしたサーバに本番のデータとブランチ staging、other、ユーザー wp（本番だけ）と
// staging（wordpress@staging だけ）を作る。
func review14Setup(t *testing.T, dir string) (addr string, stop func()) {
	t.Helper()
	addr, stop = startAuthServer(t, dir, "rootpw")
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...),
		"INSERT INTO wp_options (option_name, option_value) VALUES ('blogname', 'Prod'), ('secret', 'prod-secret')",
		"CALL heimdb_branch_create('staging')",
		"CALL heimdb_branch_create('other')",
		"UPDATE wp_options SET option_value = 'prod-secret-2' WHERE option_name = 'secret'",
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'",
		"GRANT ALL ON wordpress.* TO 'wp'@'%'",
		"CREATE USER 'staging'@'%' IDENTIFIED BY 'stpw'",
		"GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%'",
	) {
		mustExec(t, root, q)
	}
	return addr, stop
}

// probe は q を実行し、成功なら "ok: <最初の行>"、失敗なら "ERR <エラー>" を返す。
func probe(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		return "ERR " + err.Error()
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var first []string
	if rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "SCANERR " + err.Error()
		}
		for _, v := range vals {
			first = append(first, v.String)
		}
	}
	return "ok: " + strings.Join(first, "|")
}

// 引数の数が合わない手続きの呼び出し（引数のない手続きに引数を渡す）で接続が切れていた。
//
// go-mysql-server の ExternalStoredProcedureRegistry.LookupByNameAndParamCount は、同じ名前で引数の数が合う登録が
// なければ「引数が最も多い登録」を返すが、引数 0 の登録しかないとゼロ値（Function が nil）を返し、planbuilder の
// resolveExternalStoredProcedure が reflect で panic する。サーバは panic を回復して接続を閉じる。認証とは無関係
// （どのユーザーでも起きる）だが、「接続を切らない」という方針に反する。段階 46 でエンジンの登録簿を直した。
func TestReview14CallWrongArity(t *testing.T) {
	mainDB, _, stop := branchServer(t)
	defer stop()
	for q, want := range map[string]string{
		"CALL heimdb_branch_list('x')": "expected `0` parameters but got `1`",
		"CALL heimdb_gc(1)":            "expected `0` parameters but got `1`",
		"CALL heimdb_branch_create()":  "parameters but got `0`",
	} {
		_, err := mainDB.Exec(q)
		if mysqlErrNo(err) != 1105 || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "internal error") {
			t.Errorf("%s: err = %v, want error 1105 (%s)", q, err, want)
		}
	}
}

// ステージングのユーザーに許されていた操作（段階 14 では go-mysql-server の限界として記録だけにし、段階 35 で直した）。
//   - information_schema.processlist は、他のユーザーの実行中のクエリ（本番の INSERT の値など）を見せていた。
//   - SET GLOBAL は MySQL では SUPER（SYSTEM_VARIABLES_ADMIN）が要るが、誰でもできた。
func TestReview14PrivilegeGaps(t *testing.T) {
	addr, stop := review14Setup(t, t.TempDir())
	defer stop()
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer stg.Close()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = root.Exec("SELECT SLEEP(1.5), 'secret-in-flight'")
	}()
	time.Sleep(300 * time.Millisecond)
	rows, err := stg.Query("SELECT user, info FROM information_schema.processlist")
	if err != nil {
		t.Fatalf("processlist: %v", err)
	}
	for rows.Next() {
		var user string
		var info sql.NullString
		if err := rows.Scan(&user, &info); err != nil {
			t.Fatal(err)
		}
		if user != "staging" || strings.Contains(info.String, "secret-in-flight") {
			t.Errorf("staging sees another user's connection in information_schema.processlist: %s %q", user, info.String)
		}
	}
	rows.Close()
	<-done

	if _, err := stg.Exec("SET GLOBAL max_connections = 10"); err == nil || !strings.Contains(err.Error(), "1227") {
		t.Errorf("SET GLOBAL by the staging user: %v", err)
	}
}

// ユーザーと権限の変更（CREATE USER、GRANT、REVOKE、ALTER USER、DROP USER、自分のパスワードの変更）がすべて m/users に
// 保存され、再起動しても残る。
func TestReview14UsersPersist(t *testing.T) {
	dir := t.TempDir()
	addr, stop := review14Setup(t, dir)
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE USER 'gone'@'%' IDENTIFIED BY 'x'",
		"DROP USER 'gone'@'%'",
		"CREATE USER 'ro'@'%' IDENTIFIED BY 'ropw'",
		"GRANT SELECT, INSERT ON wordpress.* TO 'ro'@'%'",
		"REVOKE INSERT ON wordpress.* FROM 'ro'@'%'",
		"ALTER USER 'wp'@'%' IDENTIFIED BY 'wppw2'",
	} {
		mustExec(t, root, q)
	}
	root.Close()
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, stg, "ALTER USER 'staging'@'%' IDENTIFIED BY 'stpw2'")
	stg.Close()
	stop()

	addr, stop = startAuthServer(t, dir, "")
	defer stop()
	if _, err := connectAs(t, addr, "gone", "x", testDB); err == nil {
		t.Errorf("dropped user still connects after restart")
	}
	if _, err := connectAs(t, addr, "wp", "wppw", testDB); mysqlErrNo(err) != 1045 {
		t.Errorf("wp with the old password: err = %v, want 1045", err)
	}
	wp, err := connectAs(t, addr, "wp", "wppw2", testDB)
	if err != nil {
		t.Errorf("wp with the new password: %v", err)
	} else {
		wp.Close()
	}
	stg, err = connectAs(t, addr, "staging", "stpw2", testDB+"@staging")
	if err != nil {
		t.Errorf("staging with its self-changed password: %v", err)
	} else {
		stg.Close()
	}
	ro, err := connectAs(t, addr, "ro", "ropw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got := one(t, ro, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod" {
		t.Errorf("ro: blogname = %q", got)
	}
	if _, err := ro.Exec("INSERT INTO wp_options (option_name, option_value) VALUES ('x', 'y')"); err == nil {
		t.Errorf("REVOKE was not persisted: ro could INSERT after restart")
	}
}

// 権限を絞ったユーザーが本番と他のブランチに届かないこと（レビューで確かめた拒否の一覧。回帰の見張り）。
func TestReview14Denied(t *testing.T) {
	addr, stop := review14Setup(t, t.TempDir())
	defer stop()
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer stg.Close()
	for _, q := range []string{
		"SELECT option_value FROM wordpress.wp_options WHERE option_name = 'secret'",
		"SELECT option_value FROM `wordpress@other`.wp_options WHERE option_name = 'secret'",
		"SELECT option_value FROM wordpress.wp_options AS OF NOW() WHERE option_name = 'secret'",
		"SELECT o.option_value FROM wp_options o JOIN wordpress.wp_options p ON p.option_name = o.option_name",
		"SELECT (SELECT option_value FROM wordpress.wp_options WHERE option_name = 'secret')",
		"SELECT option_value FROM wp_options UNION SELECT option_value FROM wordpress.wp_options",
		"WITH p AS (SELECT option_value FROM wordpress.wp_options) SELECT * FROM p",
		"INSERT INTO wp_options (option_name, option_value) SELECT 'leak', option_value FROM wordpress.wp_options",
		"CREATE TEMPORARY TABLE leak AS SELECT * FROM wordpress.wp_options",
		"CREATE TABLE leak2 AS SELECT * FROM wordpress.wp_options",
		"UPDATE wordpress.wp_options SET option_value = 'x'",
		"UPDATE wp_options o, wordpress.wp_options p SET p.option_value = 'x' WHERE p.option_name = o.option_name",
		"DELETE FROM wordpress.wp_options",
		"REPLACE INTO wordpress.wp_options (option_name, option_value) VALUES ('secret', 'x')",
		"INSERT INTO wordpress.wp_options (option_name, option_value) VALUES ('injected', 'x')",
		"DROP TABLE wordpress.wp_options",
		"ALTER TABLE wordpress.wp_options ADD COLUMN zz int",
		"RENAME TABLE wordpress.wp_options TO wordpress.wp_x",
		"TRUNCATE TABLE wordpress.wp_options",
		"CREATE TABLE wordpress.injected (id int PRIMARY KEY)",
		"CREATE INDEX zz ON wordpress.wp_options (autoload)",
		"SHOW CREATE TABLE wordpress.wp_options",
		"SHOW COLUMNS FROM wordpress.wp_options",
		"SHOW INDEX FROM wordpress.wp_options",
		"SHOW TABLES FROM wordpress",
		"DESCRIBE wordpress.wp_options",
		"USE wordpress",
		"SELECT user, host, authentication_string FROM mysql.user",
		"SHOW GRANTS FOR 'root'@'%'",
		"GRANT ALL ON wordpress.* TO 'staging'@'%'",
		"GRANT ALL ON *.* TO 'staging'@'%'",
		"GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%' WITH GRANT OPTION",
		"GRANT EXECUTE ON PROCEDURE `wordpress@staging`.heimdb_branch_merge TO 'staging'@'%'",
		"CREATE USER 'evil'@'%' IDENTIFIED BY 'x'",
		"DROP USER 'root'@'%'",
		"ALTER USER 'root'@'%' IDENTIFIED BY 'x'",
		"CALL heimdb_branch_list()",
		"CALL `wordpress@staging`.heimdb_branch_list()",
		"CALL heimdb_branch_diff('staging')",
		"CALL heimdb_branch_diff('other', 'wp_options')",
		"CALL heimdb_restore_preview('wordpress@staging', NOW())",
		"CALL heimdb_restore('wordpress', NOW())",
		"CALL heimdb_branch_create('x')",
		"CALL heimdb_branch_delete('other')",
		"CALL heimdb_branch_merge('staging')",
		"CALL heimdb_branch_resolve('staging', 'wp_options', '1', 'branch')",
		"CALL heimdb_gc()",
		"LOCK TABLES wordpress.wp_options READ",
		"KILL 99999",
	} {
		if r := probe(t, stg, q); !strings.HasPrefix(r, "ERR ") {
			t.Errorf("staging: %s -> %s, want an error", q, r)
		}
	}
	// SHOW PROCESSLIST は、MySQL と同じく自分のユーザーの接続だけを見せる（段階 82。ほかのユーザーの文は見えない）
	if got := one(t, stg, "SELECT GROUP_CONCAT(DISTINCT user) FROM information_schema.processlist"); got != "staging" {
		t.Errorf("information_schema.processlist shows %s", got)
	}
	if r := probe(t, stg, "SHOW PROCESSLIST"); strings.HasPrefix(r, "ERR ") || strings.Contains(r, "root") {
		t.Errorf("staging: SHOW PROCESSLIST -> %s, want only its own connections", r)
	}
	if st, err := stg.Prepare("SELECT option_value FROM wordpress.wp_options WHERE option_name = ?"); err == nil {
		st.Close()
		t.Errorf("staging prepared a query on main")
	}
	if got := one(t, stg, "SELECT GROUP_CONCAT(DISTINCT table_schema) FROM information_schema.tables WHERE table_schema NOT IN ('wordpress@staging', 'information_schema', 'mysql')"); got != "" {
		t.Errorf("information_schema.tables shows other databases: %s", got)
	}
	if got := one(t, stg, "SELECT GROUP_CONCAT(schema_name) FROM information_schema.schemata"); strings.Contains(got, "wordpress@other") || strings.Contains(strings.Replace(got, "wordpress@staging", "", 1), "wordpress") {
		t.Errorf("information_schema.schemata shows other databases: %s", got)
	}
	if err := stg.Ping(); err != nil {
		t.Errorf("staging connection dropped: %v", err)
	}

	wp, err := connectAs(t, addr, "wp", "wppw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer wp.Close()
	for _, q := range []string{
		"SELECT option_value FROM `wordpress@staging`.wp_options WHERE option_name = 'secret'",
		"INSERT INTO `wordpress@staging`.wp_options (option_name, option_value) VALUES ('x', 'y')",
		"USE `wordpress@staging`",
		"CALL heimdb_branch_list()",
	} {
		if r := probe(t, wp, q); !strings.HasPrefix(r, "ERR ") {
			t.Errorf("wp: %s -> %s, want an error", q, r)
		}
	}
}

// 管理者側の回帰: routines.go の空の実装で、ビュー、トリガー、手続き、イベントの参照系が動き、IF EXISTS の削除が
// 成功し、ブランチの AS OF が権限の包みを通る。
func TestReview14RoutinesRegression(t *testing.T) {
	addr, stop := review14Setup(t, t.TempDir())
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, q := range []string{
		"SHOW FULL TABLES",
		"SHOW TRIGGERS",
		"SHOW PROCEDURE STATUS",
		"SHOW EVENTS",
		"SELECT COUNT(*) FROM information_schema.views",
		"SELECT COUNT(*) FROM information_schema.routines",
		"SELECT COUNT(*) FROM information_schema.triggers",
		"SELECT COUNT(*) FROM information_schema.events",
		"DROP VIEW IF EXISTS v",
		"DROP TRIGGER IF EXISTS tr",
		"DROP PROCEDURE IF EXISTS p",
		"DROP EVENT IF EXISTS e",
		"CALL heimdb_branch_list()",
	} {
		if r := probe(t, root, q); !strings.HasPrefix(r, "ok") {
			t.Errorf("root: %s -> %s", q, r)
		}
	}
	for _, q := range []string{
		"CREATE VIEW v AS SELECT 1",
		"CREATE TRIGGER tr BEFORE INSERT ON wp_options FOR EACH ROW SET NEW.autoload = 'yes'",
		"CREATE PROCEDURE p() SELECT 1",
		"CREATE EVENT e ON SCHEDULE EVERY 1 HOUR DO SELECT 1",
		"SHOW CREATE VIEW v",
		"DROP VIEW v",
	} {
		if r := probe(t, root, q); !strings.HasPrefix(r, "ERR ") {
			t.Errorf("root: %s -> %s, want an explicit error", q, r)
		}
	}
	if err := root.Ping(); err != nil {
		t.Errorf("root connection dropped: %v", err)
	}
	// ブランチの AS OF（秒の精度なので 1 秒待つ）。分岐後に本番で変えた値は見えない。
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer stg.Close()
	time.Sleep(1100 * time.Millisecond)
	if got := one(t, stg, "SELECT option_value FROM wp_options AS OF NOW() WHERE option_name = 'secret'"); got != "prod-secret" {
		t.Errorf("staging AS OF NOW(): secret = %q", got)
	}
}
