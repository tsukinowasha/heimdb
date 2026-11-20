package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 大きさの上限を超えたブランチは、書き込みと DDL をエラー 1114 で拒否し、読み取りは通す。上限を外せば書ける
// （docs/plan.md の段階 45）。
func TestBranchSizeLimit(t *testing.T) {
	var st *store.Store
	mainDB, dsn, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	big := strings.Repeat("x", 1000)
	for i := 0; i < 50; i++ {
		mustExec(t, staging, insertPost+fmt.Sprintf("(NULL, 1, 'p%d', 'publish', 'post', '%s', '', '', '', '')", i, big))
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}

	if _, err := mainDB.Exec("CALL heimdb_branch_limit('main', '1G', '0')"); err == nil {
		t.Error("limit on main should fail")
	}
	for _, bad := range []string{"CALL heimdb_branch_limit('staging', '2X', '0')", "CALL heimdb_branch_limit('staging', '1G', '30')"} {
		if _, err := mainDB.Exec(bad); err == nil {
			t.Errorf("%s should fail", bad)
		}
	}
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_limit('staging', '1K', '30s')"), "\n"); !strings.HasSuffix(got, "|1024|30000") {
		t.Errorf("limit row: %s", got)
	}
	if got := diffRows(t, mainDB, "CALL heimdb_branch_list()"); !strings.HasSuffix(got[1], "|1024|30000") || !strings.HasSuffix(got[0], "||") {
		t.Errorf("branch list:\n%s", strings.Join(got, "\n"))
	}

	for _, q := range []string{
		insertPost + "(NULL, 1, 'more', 'publish', 'post', '', '', '', '', '')",
		"UPDATE wp_posts SET post_title = 'x' WHERE ID = 1",
		"DELETE FROM wp_posts WHERE ID = 1",
	} {
		if _, err := staging.Exec(q); mysqlErrNo(err) != 1114 || !strings.Contains(err.Error(), "The table 'wp_posts' is full") {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		"ALTER TABLE wp_posts ADD COLUMN extra int",
		"CREATE TABLE wp_new (id int PRIMARY KEY)",
		"CREATE INDEX t_idx ON wp_posts (post_password)",
	} {
		if _, err := staging.Exec(q); mysqlErrNo(err) != 1114 {
			t.Errorf("%s: %v", q, err)
		}
	}
	// 表を消す DDL は、満杯でも通す。
	mustExec(t, staging, "TRUNCATE TABLE wp_links")
	mustExec(t, staging, "DROP TABLE wp_links")
	if got := one(t, staging, "SELECT COUNT(*) FROM wp_posts"); got != "51" {
		t.Errorf("read on a full branch: %s", got)
	}
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'still writable' WHERE option_name = 'blogname'")

	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '0')")
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'x' WHERE ID = 1")
	mustExec(t, staging, "ALTER TABLE wp_posts ADD COLUMN extra int")
}

// 大きさの上限の付いたブランチに書くトランザクションは、上限の大きさまでで、超えた文はエラー 1197 で取り消され、
// それまでの書き込みは残る（docs/plan.md の段階 45。コミットまでメモリに持つので）。
func TestBranchTxnSizeLimit(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '64K', '0')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	staging.SetMaxOpenConns(1)
	big := strings.Repeat("x", 1000)
	tx, err := staging.Begin()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ; n < 1000; n++ {
		_, err = tx.Exec(insertPost + fmt.Sprintf("(NULL, 1, 'p%d', 'publish', 'post', '%s', '', '', '', '')", n, big))
		if err != nil {
			break
		}
	}
	if mysqlErrNo(err) != 1197 || n < 20 || n > 64 {
		t.Fatalf("after %d inserts: %v", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := one(t, staging, "SELECT COUNT(*) FROM wp_posts WHERE post_title LIKE 'p%'"); got != fmt.Sprint(n) {
		t.Errorf("committed rows: %s, want %d", got, n)
	}
	// 上限のないブランチは変わらない。
	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '0')")
	mustExec(t, staging, "UPDATE wp_posts SET post_content = CONCAT(post_content, post_content)")
}

// 認証が有効なら、上限はユーザーが権限を持つブランチで決まり、データベースを指定せずにつないでも外せない。一時テーブルも
// 作れない。全体の権限を持つユーザーには効かない（docs/plan.md の段階 45）。
func TestBranchLimitsByPrivilege(t *testing.T) {
	root, _, addr, stop := limitServer(t, nil)
	defer stop()
	stagingDB := testDB + "@staging"
	mustExec(t, root, "CALL heimdb_branch_limit('staging', '1G', '200ms')")

	for _, db := range []string{"", stagingDB} {
		stg, err := connectAs(t, addr, "stg", "pw", db)
		if err != nil {
			t.Fatal(err)
		}
		stg.SetMaxOpenConns(1)
		for _, q := range []string{"SELECT SLEEP(5)", "USE information_schema", "SELECT SLEEP(5)", "SELECT COUNT(*), SLEEP(5) FROM `" + stagingDB + "`.wp_posts"} {
			_, err := stg.Exec(q)
			if strings.HasPrefix(q, "USE") {
				if err != nil {
					t.Fatal(err)
				}
				continue
			}
			if mysqlErrNo(err) != 3024 {
				t.Errorf("db %q: %s: %v", db, q, err)
			}
		}
		if _, err := stg.Exec("CREATE TEMPORARY TABLE `" + stagingDB + "`.t (id int PRIMARY KEY)"); mysqlErrNo(err) != 1235 {
			t.Errorf("db %q: temporary table: %v", db, err)
		}
		stg.Close()
	}

	// SUPER のない全体の権限（GRANT SELECT ON *.*）では、上限は外れない（管理者の判定は手続きと同じ SUPER。段階 63）。
	mustExec(t, root, "CREATE USER 'reader'@'%' IDENTIFIED BY 'pw'")
	mustExec(t, root, "GRANT SELECT ON *.* TO 'reader'@'%'")
	mustExec(t, root, "GRANT ALL ON `"+stagingDB+"`.* TO 'reader'@'%'")
	reader, err := connectAs(t, addr, "reader", "pw", stagingDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Exec("SELECT SLEEP(5)"); mysqlErrNo(err) != 3024 {
		t.Errorf("global SELECT: %v", err)
	}
	reader.Close()

	// 全体の権限を持つユーザーは、一時テーブルを作れる。
	admin, err := connectAs(t, addr, "root", "rootpw", stagingDB)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	mustExec(t, admin, "CREATE TEMPORARY TABLE t (id int PRIMARY KEY)")
}

// limitServer は、認証を有効にしたサーバで WordPress のテーブルを作り（setup があれば続けて呼ぶ）、ブランチ staging と、
// そのブランチだけの権限のユーザー stg を作る。root（全体の権限）の接続と、stg が staging につないだ接続を返す。
func limitServer(t *testing.T, setup func(root *sql.DB)) (root, staging *sql.DB, addr string, stop func()) {
	t.Helper()
	addr, stopServer := startAuthServer(t, t.TempDir(), "rootpw")
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	seedWordPress(t, root)
	if setup != nil {
		setup(root)
	}
	mustExec(t, root, "CALL heimdb_branch_create('staging')")
	mustExec(t, root, "CREATE USER 'stg'@'%' IDENTIFIED BY 'pw'")
	mustExec(t, root, "GRANT ALL ON `"+testDB+"@staging`.* TO 'stg'@'%'")
	staging, err = connectAs(t, addr, "stg", "pw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	return root, staging, addr, func() {
		staging.Close()
		root.Close()
		stopServer()
	}
}

// 実行時間の上限を過ぎた文は、エラー 3024 で止まり、その文の書き込みは残らない。全体の権限を持つユーザーは止めない
// （docs/plan.md の段階 45）。
func TestBranchTimeLimit(t *testing.T) {
	const bigRows = 100000
	mainDB, staging, addr, stop := limitServer(t, func(root *sql.DB) {
		mustExec(t, root, "CREATE TABLE wp_big (id bigint unsigned NOT NULL PRIMARY KEY, v int NOT NULL)")
		for i := 0; i < bigRows; i += 20000 {
			var vals []string
			for j := i + 1; j <= i+20000; j++ {
				vals = append(vals, fmt.Sprintf("(%d, 0)", j))
			}
			mustExec(t, root, "INSERT INTO wp_big VALUES "+strings.Join(vals, ","))
		}
	})
	defer stop()
	staging.SetMaxOpenConns(1)

	// 明示的なトランザクションの中で止まった文は、その文だけを取り消す。
	tx, err := staging.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE wp_big SET v = 7 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	// 上限は 1ms にする。行を 1 行ずつ読むので、10 万行でも 30ms より早く終わりうる（段階 53、55）。
	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '1ms')")
	if _, err := tx.Exec("UPDATE wp_big SET v = v + 1"); mysqlErrNo(err) != 3024 {
		t.Errorf("update in a transaction: %v", err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '0')")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := one(t, staging, "SELECT SUM(v) FROM wp_big"); got != "7" {
		t.Errorf("after the interrupted statement in a transaction: sum %s, want 7", got)
	}

	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '1ms')")
	for _, q := range []string{
		"SELECT COUNT(*) FROM wp_big WHERE v + 1 = 100",
		"SELECT SLEEP(5)",
		"UPDATE wp_big SET v = v + 1",
		"DELETE FROM wp_big WHERE v >= 0",
		"ALTER TABLE wp_big ADD COLUMN w int",
	} {
		start := time.Now()
		_, err := staging.Exec(q)
		if mysqlErrNo(err) != 3024 || !strings.Contains(err.Error(), "maximum statement execution time exceeded") {
			t.Errorf("%s: %v", q, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s took %v", q, d)
		}
	}
	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '0')")
	if got := one(t, staging, "SELECT CONCAT(COUNT(*), ',', SUM(v)) FROM wp_big"); got != "100000,7" {
		t.Errorf("writes of interrupted statements remain: %s", got)
	}
	var name, def string
	if err := staging.QueryRow("SHOW CREATE TABLE wp_big").Scan(&name, &def); err != nil || strings.Contains(def, "`w`") {
		t.Errorf("interrupted ALTER changed the table: %s, %v", def, err)
	}
	// 全体の権限を持つユーザーは、上限の付いたブランチにつないでも止めない。認証のないときは、heimdb_* の手続きを
	// 止めない。
	mustExec(t, mainDB, "CALL heimdb_branch_limit('staging', '0', '1s')")
	admin, err := connectAs(t, addr, "root", "rootpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if got := one(t, admin, "SELECT SLEEP(1.2)"); got != "0" {
		t.Errorf("sleep as root: %s", got)
	}
	if !isHeimDBCall(" /* x */ call\nHeimDB_branch_list()") || isHeimDBCall("CALL other()") || isHeimDBCall("SELECT 'CALL heimdb_'") {
		t.Error("isHeimDBCall")
	}
	// 上限の内に終わる文は止めない。止めた後の同じ接続の文も止まらない。
	if _, err := staging.Exec("SELECT SLEEP(2)"); mysqlErrNo(err) != 3024 {
		t.Errorf("sleep on staging: %v", err)
	}
	for i := 0; i < 5; i++ {
		if got := one(t, staging, "SELECT COUNT(*) FROM wp_big WHERE id < 10"); got != "9" {
			t.Errorf("count: %s", got)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// MAX_USER_CONNECTIONS を超えた接続はエラー 1203 になり、切れば再びつなげる。ALTER USER で変えられ（パスワードは
// 変わらない）、再起動の後も効く（docs/plan.md の段階 45）。
func TestMaxUserConnections(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startAuthServer(t, dir, "rootpw")
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, root, "CREATE USER 'stg'@'%' IDENTIFIED BY 'pw' WITH MAX_USER_CONNECTIONS 2")
	mustExec(t, root, "GRANT ALL ON `"+testDB+"`.* TO 'stg'@'%'")
	if got := one(t, root, "SELECT max_user_connections FROM mysql.user WHERE user = 'stg'"); got != "2" {
		t.Errorf("mysql.user: %s", got)
	}

	check := func(addr string) {
		t.Helper()
		c1, err := connectAs(t, addr, "stg", "pw", testDB)
		if err != nil {
			t.Fatal(err)
		}
		c2, err := connectAs(t, addr, "stg", "pw", testDB)
		if err != nil {
			t.Fatal(err)
		}
		defer c2.Close()
		if c3, err := connectAs(t, addr, "stg", "pw", testDB); mysqlErrNo(err) != 1203 {
			t.Errorf("third connection: %v", err)
			if c3 != nil {
				c3.Close()
			}
		}
		// 他のアカウントは数えない。
		if c, err := connectAs(t, addr, "root", "rootpw", testDB); err != nil {
			t.Errorf("root: %v", err)
		} else {
			c.Close()
		}
		c1.Close()
		deadline := time.Now().Add(3 * time.Second)
		for {
			c3, err := connectAs(t, addr, "stg", "pw", testDB)
			if err == nil {
				c3.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("connection after closing one: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	check(addr)

	// 上限を外すと、いくつでもつなげる。IDENTIFIED のない ALTER USER はパスワードを変えない。
	mustExec(t, root, "ALTER USER 'stg'@'%' WITH MAX_USER_CONNECTIONS 0")
	var conns []interface{ Close() error }
	for i := 0; i < 4; i++ {
		c, err := connectAs(t, addr, "stg", "pw", testDB)
		if err != nil {
			t.Fatalf("connection %d without a limit: %v", i, err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Close()
	}
	if c, err := connectAs(t, addr, "stg", "", testDB); err == nil {
		c.Close()
		t.Error("ALTER USER without IDENTIFIED cleared the password")
	}
	// IDENTIFIED BY '' は、MySQL と同じく空のパスワードにする。
	mustExec(t, root, "ALTER USER 'stg'@'%' IDENTIFIED BY '' WITH MAX_USER_CONNECTIONS 2")
	if c, err := connectAs(t, addr, "stg", "", testDB); err != nil {
		t.Errorf("empty password after IDENTIFIED BY '': %v", err)
	} else {
		c.Close()
	}
	mustExec(t, root, "ALTER USER 'stg'@'%' IDENTIFIED BY 'pw'")

	root.Close()
	stop()

	// 再起動の後も効く。
	addr, stop = startAuthServer(t, dir, "")
	defer stop()
	check(addr)
}

// IDENTIFIED のない ALTER USER だけ、構文木の認証を nil にする（パスワードを変えない）。字句で見るので、名前や
// コメントの IDENTIFIED は数えない（docs/plan.md の段階 45）。
func TestMarkUnspecifiedAuth(t *testing.T) {
	p := likeEscapeParser{Parser: gsql.NewMysqlParser()}
	for q, unspecified := range map[string]bool{
		"ALTER USER 'stg'@'%' WITH MAX_USER_CONNECTIONS 1":                  true,
		"ALTER USER 'identified'@'%' WITH MAX_USER_CONNECTIONS 1":           true,
		"ALTER USER stg /* IDENTIFIED BY '' */ WITH MAX_USER_CONNECTIONS 1": true,
		"ALTER USER stg IDENTIFIED BY ''":                                   false,
		"alter user stg identified by 'pw' with max_user_connections 1":     false,
		"ALTER USER stg IDENTIFIED WITH caching_sha2_password":              false,
	} {
		stmt, err := p.ParseSimple(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got := stmt.(*ast.DDL).Authentication == nil; got != unspecified {
			t.Errorf("%s: unspecified %v", q, got)
		}
	}
}
