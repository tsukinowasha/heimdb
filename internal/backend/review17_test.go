package backend

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlcompare"
)

// 段階 17 のレビューで書いた再現テスト。

// review17MySQL は、比較用の本物の MySQL（127.0.0.1:3307、root/root）。届かなければ "" を返す。
func review17MySQL(t *testing.T) string {
	t.Helper()
	dsn := "root:root@tcp(127.0.0.1:3307)/?timeout=2s"
	if v := os.Getenv("HEIMDB_REVIEW_MYSQL"); v != "" {
		dsn = v
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return ""
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Logf("real MySQL not reachable (%v); comparing against expectations only", err)
		return ""
	}
	return dsn
}

// compareScriptWithMySQL は、同じ文の列を HeimDB と本物の MySQL に流し、結果を文ごとに比べる。MySQL に届かなければ、
// HeimDB の結果だけを返す。
func compareScriptWithMySQL(t *testing.T, name string, stmts []string) (heimdb []string, mysql []string) {
	t.Helper()
	return compareScript(t, name, stmts, true)
}

// compareScriptWithMySQLNoFail は compareScriptWithMySQL と同じだが、違いをテストの失敗にしない（限界の確認に使う）。
func compareScriptWithMySQLNoFail(t *testing.T, name string, stmts []string) (heimdb []string, mysql []string) {
	t.Helper()
	return compareScript(t, name, stmts, false)
}

func compareScript(t *testing.T, name string, stmts []string, fail bool) (heimdb []string, mysql []string) {
	t.Helper()
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	heimdb = runScript(t, dsn, stmts)
	mdsn := review17MySQL(t)
	if mdsn == "" {
		return heimdb, nil
	}
	base := strings.TrimSuffix(mdsn, "?timeout=2s")
	runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS " + name, "CREATE DATABASE " + name})
	defer runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS " + name})
	mysql = runScript(t, strings.Replace(base, "/", "/"+name, 1)+"?timeout=2s", stmts)
	for i, q := range stmts {
		if strings.HasPrefix(q, "/*!") && strings.HasPrefix(heimdb[i], "ok") && strings.HasPrefix(mysql[i], "ok") {
			continue // go-mysql-server は SET に影響行数 1 を返す（sqlcompare が SET で始まる文に対してしていることと同じ）
		}
		if strings.HasPrefix(q, "INSERT INTO `d1`") {
			// 複数行の明示的な AUTO_INCREMENT 値の insert_id は MySQL が最後の値、HeimDB が最初の値（段階 17 の外。記録だけ）
			heimdb[i], _, _ = strings.Cut(heimdb[i], " insert_id=")
			mysql[i], _, _ = strings.Cut(mysql[i], " insert_id=")
		}
		if fail && heimdb[i] != mysql[i] {
			t.Errorf("statement %d: %s\n  HeimDB: %q\n  mysql:  %q", i, q, heimdb[i], mysql[i])
		}
	}
	return heimdb, mysql
}

// セーブポイントと文の失敗の取り消し、一時テーブル、暗黙のコミットの組み合わせを MySQL と比べる。
func TestReview17SavepointEdgeCasesMatchMySQL(t *testing.T) {
	compareScriptWithMySQL(t, "heimdb_review17_sp", []string{
		wpSQLMode,
		"CREATE TABLE sp (id int NOT NULL PRIMARY KEY, v varchar(10) NOT NULL, UNIQUE KEY v (v))",
		"INSERT INTO sp VALUES (1, 'a')",
		// 文の失敗（重複）の後の ROLLBACK TO
		"START TRANSACTION",
		"SAVEPOINT a",
		"INSERT INTO sp VALUES (2, 'b')",
		"INSERT INTO sp VALUES (3, 'a')",
		"INSERT INTO sp VALUES (3, 'c'), (4, 'b')",
		"SELECT id, v FROM sp ORDER BY id",
		"ROLLBACK TO SAVEPOINT a",
		"SELECT id, v FROM sp ORDER BY id",
		"INSERT INTO sp VALUES (5, 'e')",
		"SAVEPOINT b",
		"UPDATE sp SET v = 'a' WHERE id = 5",
		"ROLLBACK TO SAVEPOINT b",
		"INSERT INTO sp VALUES (6, 'f')",
		"COMMIT",
		"SELECT id, v FROM sp ORDER BY id",
		// 一時テーブルの書き込みも戻る
		"CREATE TEMPORARY TABLE tt (id int NOT NULL PRIMARY KEY, v varchar(10))",
		"START TRANSACTION",
		"INSERT INTO tt VALUES (1, 'x')",
		"SAVEPOINT t",
		"INSERT INTO tt VALUES (2, 'y')",
		"UPDATE sp SET v = 'z' WHERE id = 1",
		"ROLLBACK TO SAVEPOINT t",
		"SELECT id, v FROM tt ORDER BY id",
		"SELECT id, v FROM sp WHERE id = 1",
		"COMMIT",
		// DDL の暗黙のコミットでセーブポイントは消える
		"START TRANSACTION",
		"INSERT INTO sp VALUES (7, 'g')",
		"SAVEPOINT d",
		"CREATE TABLE sp2 (id int NOT NULL PRIMARY KEY)",
		"ROLLBACK TO SAVEPOINT d",
		"ROLLBACK",
		"SELECT id, v FROM sp WHERE id = 7",
		// SET autocommit = 1 でコミットされる
		"SET autocommit = 0",
		"INSERT INTO sp VALUES (8, 'h')",
		"SAVEPOINT e",
		"SET autocommit = 1",
		"ROLLBACK TO SAVEPOINT e",
		"SELECT id, v FROM sp WHERE id = 8",
		// 読み取り専用のトランザクション
		"START TRANSACTION READ ONLY",
		"SAVEPOINT r",
		"SELECT COUNT(*) FROM sp",
		"ROLLBACK TO SAVEPOINT r",
		"RELEASE SAVEPOINT r",
		"RELEASE SAVEPOINT r",
		"COMMIT",
		// 同じ名前のセーブポイントを置き換えたとき、古い位置は忘れる
		"START TRANSACTION",
		"SAVEPOINT s",
		"INSERT INTO sp VALUES (9, 'i')",
		"SAVEPOINT s",
		"INSERT INTO sp VALUES (10, 'j')",
		"ROLLBACK TO SAVEPOINT s",
		"SELECT id, v FROM sp WHERE id >= 9 ORDER BY id",
		"ROLLBACK",
		// 空の ROLLBACK TO の後の COMMIT と、全体の ROLLBACK
		"START TRANSACTION",
		"SAVEPOINT z",
		"ROLLBACK TO SAVEPOINT z",
		"INSERT INTO sp VALUES (11, 'k')",
		"ROLLBACK TO SAVEPOINT z",
		"COMMIT",
		"SELECT COUNT(*) FROM sp WHERE id = 11",
	})
}

// セーブポイントに戻した後、トランザクションが行のロックを持ち続けること（MySQL と同じ）と、戻した行への
// 別の接続の更新が、コミット後に待たずに通ること（戻した書き込みが競合として残らないこと）。
func TestReview17SavepointRollbackLeavesNoStaleConflict(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE acc (id int NOT NULL PRIMARY KEY, n int NOT NULL)")
	mustExec(t, m, "INSERT INTO acc VALUES (1, 10), (2, 20)")

	a, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	ca, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	exec(t, ca, "START TRANSACTION")
	exec(t, ca, "SELECT n FROM acc WHERE id = 1") // スナップショットを決める
	exec(t, ca, "SAVEPOINT s")
	exec(t, ca, "UPDATE acc SET n = n + 1 WHERE id = 1")
	exec(t, ca, "ROLLBACK TO SAVEPOINT s")
	// 戻した後も同じトランザクションで書ける（スナップショットは変わらないので競合しない）
	exec(t, ca, "UPDATE acc SET n = n + 5 WHERE id = 2")
	exec(t, ca, "COMMIT")
	if got := one(t, m, "SELECT CONCAT(id, ':', n) FROM acc WHERE id = 1"); got != "1:10" {
		t.Errorf("rolled back update leaked: %s", got)
	}
	if got := one(t, m, "SELECT CONCAT(id, ':', n) FROM acc WHERE id = 2"); got != "2:25" {
		t.Errorf("update after ROLLBACK TO lost: %s", got)
	}
	// 別の接続がすぐに書ける（ロックが残っていない）
	mustExec(t, m, "UPDATE acc SET n = n + 1 WHERE id = 1")
	if got := one(t, m, "SELECT n FROM acc WHERE id = 1"); got != "11" {
		t.Errorf("after other update: %s", got)
	}
}

// utf8mb4 以外の文字コードが、ValidateSchema を通らない道で入らないこと（utf8mb3 は段階 37、ascii は段階 57 から utf8mb4 として入る。
// 対応する utf8mb4 の照合順序がない utf8mb3_tolower_ci は拒否する）。
func TestReview17CharsetRejectedOnAllPaths(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE ok1 (id int NOT NULL PRIMARY KEY, b varchar(20), c varchar(20)) DEFAULT CHARSET=utf8mb4")
	for _, q := range []string{
		"ALTER TABLE ok1 ADD COLUMN l1 varchar(10) CHARACTER SET latin1 FIRST",
		"ALTER TABLE ok1 ADD COLUMN l1 varchar(10) CHARACTER SET latin1 AFTER id",
		"ALTER TABLE ok1 ADD COLUMN l1 enum('a','b') CHARACTER SET latin1",
		"ALTER TABLE ok1 ADD COLUMN l1 varchar(10) COLLATE latin1_swedish_ci",
		"ALTER TABLE ok1 CHANGE b b varchar(20) CHARACTER SET latin1 FIRST",
		"ALTER TABLE ok1 CHANGE b b2 varchar(20) CHARACTER SET latin1",
		"ALTER TABLE ok1 MODIFY b varchar(20) COLLATE latin1_bin",
		"ALTER TABLE ok1 MODIFY b varchar(20) CHARACTER SET utf16",
		"ALTER TABLE ok1 DEFAULT CHARACTER SET latin1",
		"ALTER TABLE ok1 CHARACTER SET = latin1",
		"ALTER TABLE ok1 COLLATE latin1_bin",
		"ALTER TABLE ok1 CONVERT TO CHARACTER SET utf8mb3 COLLATE utf8mb3_tolower_ci",
		"ALTER TABLE ok1 ADD COLUMN d int, ADD COLUMN l1 varchar(10) CHARACTER SET latin1",
		"CREATE TEMPORARY TABLE tmp1 (id int NOT NULL PRIMARY KEY, b varchar(10)) DEFAULT CHARSET=latin1",
		"CREATE TEMPORARY TABLE tmp1 (id int NOT NULL PRIMARY KEY, b varchar(10) CHARACTER SET latin1)",
		"CREATE TABLE sel1 AS SELECT 1 AS id, CONVERT('x' USING latin1) AS b",
	} {
		_, err := m.Exec(q)
		if err == nil || !strings.Contains(err.Error(), "character set") {
			t.Errorf("%s: err = %v, want a character set error", q, err)
		}
	}
	var name, stmt string
	if err := m.QueryRow("SHOW CREATE TABLE ok1").Scan(&name, &stmt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stmt, "latin1") || strings.Contains(stmt, "utf8mb3") || strings.Contains(stmt, "ascii") {
		t.Errorf("non-utf8mb4 crept into ok1:\n%s", stmt)
	}
	if got := one(t, m, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN ('sel1')"); got != "0" {
		t.Errorf("rejected tables were created: %s", got)
	}
	// 一時テーブルは information_schema に出ないので、使えないことで確かめる
	if _, err := m.Exec("SELECT * FROM tmp1"); err == nil {
		t.Errorf("rejected temporary table tmp1 exists")
	}
	// utf8mb4 の指定はどれも通る
	for _, q := range []string{
		"ALTER TABLE ok1 ADD COLUMN u1 varchar(10) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin FIRST",
		"ALTER TABLE ok1 ADD COLUMN u2 varbinary(10), ADD COLUMN u3 blob, ADD COLUMN u4 json, ADD COLUMN u5 char(4) CHARACTER SET binary",
		"ALTER TABLE ok1 CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci",
		"ALTER TABLE ok1 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci",
		"CREATE TEMPORARY TABLE tmp2 (id int NOT NULL PRIMARY KEY, b varchar(10)) DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE like1 LIKE ok1",
	} {
		mustExec(t, m, q)
	}
	_ = dsn
}

// SHOW CREATE TABLE の ENUM と SET の既定値の書き換えが、紛らわしい列名や値でも正しいこと。本物の MySQL があれば、
// HeimDB の出力した CREATE TABLE 文を MySQL に流し、MySQL の SHOW CREATE TABLE と列の行が一致することを確かめる。
func TestReview17EnumSetDefaultSurgery(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	create := "CREATE TABLE `en2` (" +
		"`id` int NOT NULL, " +
		"`e` enum('a','b','c') NOT NULL DEFAULT 'b', " +
		"`e2` enum('x','y') NOT NULL DEFAULT 'y', " +
		"`e e` enum('p','q') NOT NULL DEFAULT 'q' COMMENT ' DEFAULT 2', " +
		"`e``q` enum('m','n') NOT NULL DEFAULT 'N', " +
		"`num` enum('3','2','1') NOT NULL DEFAULT '1', " +
		"`n2` enum('a','b') NOT NULL DEFAULT 2, " +
		"`s` set('x','y','z') NOT NULL DEFAULT 'z,x', " +
		"`s2` set('1','2') NOT NULL DEFAULT '2', " +
		"`t` varchar(10) NOT NULL DEFAULT '2', " +
		"PRIMARY KEY (`id`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci"
	mustExec(t, m, create)
	var name, stmt string
	if err := m.QueryRow("SHOW CREATE TABLE en2").Scan(&name, &stmt); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"`e` enum('a','b','c') NOT NULL DEFAULT 'b',",
		"`e2` enum('x','y') NOT NULL DEFAULT 'y',",
		"`e e` enum('p','q') NOT NULL DEFAULT 'q' COMMENT ' DEFAULT 2',",
		"`e``q` enum('m','n') NOT NULL DEFAULT 'n',",
		"`num` enum('3','2','1') NOT NULL DEFAULT '1',",
		"`n2` enum('a','b') NOT NULL DEFAULT 'b',",
		"`s` set('x','y','z') NOT NULL DEFAULT 'x,z',",
		"`s2` set('1','2') NOT NULL DEFAULT '2',",
		"`t` varchar(10) NOT NULL DEFAULT '2',",
	}
	for _, w := range want {
		if !strings.Contains(stmt, w) {
			t.Errorf("SHOW CREATE TABLE lacks %q", w)
		}
	}
	if t.Failed() {
		t.Logf("HeimDB:\n%s", stmt)
	}
	// ブランチと AS OF でも同じ
	mustExec(t, m, "CALL heimdb_branch_create('staging')")
	br := openBranch(t, dsn, "staging")
	defer br.Close()
	var bstmt string
	if err := br.QueryRow("SHOW CREATE TABLE en2").Scan(&name, &bstmt); err != nil {
		t.Fatal(err)
	}
	if bstmt != stmt {
		t.Errorf("branch SHOW CREATE TABLE differs:\n%s", bstmt)
	}
	var astmt string
	time.Sleep(1100 * time.Millisecond) // NOW() は秒の精度
	if err := m.QueryRow("SHOW CREATE TABLE en2 AS OF NOW()").Scan(&name, &astmt); err != nil {
		t.Logf("SHOW CREATE TABLE AS OF: %v", err)
	} else if astmt != stmt {
		t.Errorf("AS OF SHOW CREATE TABLE differs:\n%s", astmt)
	}
	// 出力した文で作り直せる
	mustExec(t, m, "DROP TABLE en2")
	mustExec(t, m, stmt)

	mdsn := review17MySQL(t)
	if mdsn == "" {
		return
	}
	runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS heimdb_review17_en", "CREATE DATABASE heimdb_review17_en"})
	defer runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS heimdb_review17_en"})
	out := runScript(t, strings.Replace(strings.TrimSuffix(mdsn, "?timeout=2s"), "/", "/heimdb_review17_en", 1), []string{stmt, "SHOW CREATE TABLE en2"})
	if strings.HasPrefix(out[0], "error") {
		t.Fatalf("MySQL rejected heimdb's SHOW CREATE TABLE output: %s\n%s", out[0], stmt)
	}
	// MySQL は、テーブルの照合順序が文字コードの既定でないとき、列ごとに COLLATE を書く
	normalized := strings.ReplaceAll(out[1], " COLLATE utf8mb4_unicode_520_ci NOT NULL", " NOT NULL")
	for _, w := range want {
		if !strings.Contains(normalized, w) {
			t.Errorf("MySQL's SHOW CREATE TABLE lacks %q", w)
		}
	}
	if t.Failed() {
		t.Logf("MySQL:\n%s", out[1])
	}
}

// mysqldump / mariadb-dump が出す文の列を HeimDB に流しても、接続が切れたり誤った値になったりしないこと。
func TestReview17DumpStreamStatements(t *testing.T) {
	compareScriptWithMySQL(t, "heimdb_review17_dump", []string{
		"/*M!999999\\- enable the sandbox mode */",
		"/* plain comment only */",
		"-- comment line",
		"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */",
		"/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */",
		"/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */",
		"/*!50503 SET NAMES utf8mb4 */",
		"/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */",
		"/*!40103 SET TIME_ZONE='+00:00' */",
		"/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */",
		"/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */",
		"/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */",
		"/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */",
		"DROP TABLE IF EXISTS `d1`",
		"/*!40101 SET @saved_cs_client     = @@character_set_client */",
		"/*!50503 SET character_set_client = utf8mb4 */",
		"CREATE TABLE `d1` (`id` bigint unsigned NOT NULL AUTO_INCREMENT, `v` varchar(20) COLLATE utf8mb4_unicode_520_ci DEFAULT NULL, `b` varbinary(8) DEFAULT NULL, `ts` timestamp NULL DEFAULT NULL, PRIMARY KEY (`id`)) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"/*!40101 SET character_set_client = @saved_cs_client */",
		"LOCK TABLES `d1` WRITE",
		"/*!40000 ALTER TABLE `d1` DISABLE KEYS */",
		// NO_AUTO_VALUE_ON_ZERO: id 0 の行はそのまま入る。--hex-blob の 0x リテラル。
		// （複数行の明示的な AUTO_INCREMENT 値の insert_id は、MySQL が最後の値、HeimDB が最初の値を返す。段階 17 の外なので
		// 記録だけにし、ここでは 1 行ずつ入れる）
		"INSERT INTO `d1` (`id`, `v`, `b`, `ts`) VALUES (0,'zero',0x00FF,'2026-01-02 03:04:05')",
		"INSERT INTO `d1` (`id`, `v`, `b`, `ts`) VALUES (2,'two','',NULL),(4,'four',NULL,'0000-00-00 00:00:00')",
		"/*!40000 ALTER TABLE `d1` ENABLE KEYS */",
		"UNLOCK TABLES",
		"/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */",
		"/*!40101 SET SQL_MODE=@OLD_SQL_MODE */",
		"/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */",
		"/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */",
		"/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */",
		"/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */",
		"/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */",
		"/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */",
		"SELECT id, v, HEX(b), ts FROM d1 ORDER BY id",
		"INSERT INTO d1 (v) VALUES ('next')",
		"SELECT id FROM d1 ORDER BY id DESC LIMIT 1",
		"SELECT @@sql_mode = @OLD_SQL_MODE, @@time_zone = @OLD_TIME_ZONE",
	})
}

// TIMESTAMP の値は、MySQL ではセッションの time_zone で読み書きされるが、HeimDB（go-mysql-server）は time_zone を
// 無視して値をそのまま返す（記録だけ。docs/plan.md の段階 17）。HeimDB は UTC で動かし（cmd/heimdb）、WordPress は
// time_zone を変えないので、mysqldump の往復はずれない（TestReview17TimestampDumpRoundTrip）。接続が time_zone を
// UTC 以外にして TIMESTAMP を使うときだけ MySQL と違う。限界が変わったら（go-mysql-server の更新など）、記録を直す。
func TestReview17TimestampSessionTimeZoneMatchesMySQL(t *testing.T) {
	heimdb, mysql := compareScriptWithMySQLNoFail(t, "heimdb_review17_tz", []string{
		wpSQLMode,
		"SET time_zone = '+09:00'",
		"CREATE TABLE tz (id int NOT NULL PRIMARY KEY, ts timestamp NULL DEFAULT NULL, dt datetime DEFAULT NULL)",
		"INSERT INTO tz VALUES (1, '2026-01-02 12:00:00', '2026-01-02 12:00:00')",
		"SELECT id, ts, dt FROM tz",
		// mysqldump --tz-utc が読む形
		"SET time_zone = '+00:00'",
		"SELECT id, ts, dt FROM tz",
		"INSERT INTO tz VALUES (2, '2026-01-02 03:00:00', '2026-01-02 03:00:00')",
		"SET time_zone = '+09:00'",
		"SELECT id, ts, dt FROM tz ORDER BY id",
	})
	if mysql != nil && strings.Join(heimdb, "\n") == strings.Join(mysql, "\n") {
		t.Errorf("TIMESTAMP now follows time_zone like MySQL (update docs/plan.md)")
	}
}

// 外部の mysqldump と本物の MySQL で、HeimDB の TIMESTAMP を往復する。HeimDB は UTC で動き（cmd/heimdb）、接続は
// time_zone を変えない（WordPress と同じ）。HeimDB の値は UTC の値として MySQL に移り、UTC で読めば同じ。
func TestReview17TimestampDumpRoundTrip(t *testing.T) {
	mysqldump, err := osexec.LookPath("mysqldump")
	if err != nil {
		t.Skip("mysqldump not on PATH")
	}
	mysqlCLI, err := osexec.LookPath("mysql")
	if err != nil {
		t.Skip("mysql not on PATH")
	}
	mdsn := review17MySQL(t)
	if mdsn == "" {
		t.Skip("real MySQL not reachable")
	}
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	h, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	hc, err := h.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	exec(t, hc, "CREATE TABLE tz (id int NOT NULL PRIMARY KEY, ts timestamp NULL DEFAULT NULL, dt datetime DEFAULT NULL)")
	exec(t, hc, "INSERT INTO tz VALUES (1, '2026-01-02 12:00:00', '2026-01-02 12:00:00')")

	_, port, _ := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(dsn, "root@tcp("), ")/"+testDB))
	dump, err := osexec.Command(mysqldump, "-h127.0.0.1", "-P"+port, "-uroot", "--ssl-mode=DISABLED", testDB, "tz").CombinedOutput()
	if err != nil {
		t.Fatalf("mysqldump heimdb: %v\n%s", err, dump)
	}
	if !bytes.Contains(dump, []byte("SET TIME_ZONE='+00:00'")) {
		t.Fatalf("dump does not set TIME_ZONE:\n%s", dump)
	}
	t.Logf("heimdb dump INSERT: %s", firstLineWith(dump, "INSERT INTO"))
	path := filepath.Join(t.TempDir(), "tz.sql")
	if err := os.WriteFile(path, dump, 0o644); err != nil {
		t.Fatal(err)
	}
	runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS heimdb_review17_tzrt", "CREATE DATABASE heimdb_review17_tzrt"})
	defer runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS heimdb_review17_tzrt"})
	load := osexec.Command(mysqlCLI, "-h127.0.0.1", "-P3307", "-uroot", "-proot", "heimdb_review17_tzrt")
	f, _ := os.Open(path)
	load.Stdin = f
	if out, err := load.CombinedOutput(); err != nil {
		t.Fatalf("mysql load: %v\n%s", err, out)
	}
	f.Close()
	got := runScript(t, strings.Replace(strings.TrimSuffix(mdsn, "?timeout=2s"), "/", "/heimdb_review17_tzrt", 1), []string{
		"SET time_zone = '+00:00'",
		"SELECT id, ts, dt FROM tz",
	})
	if want := "1 | 2026-01-02 12:00:00 | 2026-01-02 12:00:00\n"; got[1] != want {
		t.Errorf("TIMESTAMP after heimdb -> mysqldump -> MySQL, read in UTC:\n got %q\nwant %q", got[1], want)
	}
}

// DDL の暗黙のコミットは、MySQL では明示的なトランザクションを終わらせる。その後の文は（@@autocommit = 1 なら）文ごとに
// コミットされ、他の接続からすぐ見える。セーブポイントも消える。2 本の接続で HeimDB と MySQL を比べる。
func TestReview17ImplicitCommitEndsTransaction(t *testing.T) {
	run := func(dsn string) []string {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx := context.Background()
		a, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		b, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		var out []string
		for _, s := range []struct {
			c *sql.Conn
			q string
		}{
			{a, "CREATE TABLE ic (id int NOT NULL PRIMARY KEY)"},
			{a, "START TRANSACTION"},
			{a, "INSERT INTO ic VALUES (1)"},
			{a, "SAVEPOINT s"},
			{a, "CREATE TABLE ic2 (id int NOT NULL PRIMARY KEY)"},
			{b, "SELECT COUNT(*) FROM ic"},
			{a, "INSERT INTO ic VALUES (2)"},
			{b, "SELECT COUNT(*) FROM ic"},
			{a, "ROLLBACK TO SAVEPOINT s"},
			{a, "ROLLBACK"},
			{b, "SELECT COUNT(*) FROM ic"},
			{a, "DROP TABLE ic, ic2"},
		} {
			out = append(out, s.q+" => "+strings.TrimSpace(sqlcompare.Run(ctx, s.c, s.q)))
		}
		return out
	}
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	got := run(dsn)
	want := []string{
		"CREATE TABLE ic (id int NOT NULL PRIMARY KEY) => ok affected=0 insert_id=0",
		"START TRANSACTION => ok affected=0 insert_id=0",
		"INSERT INTO ic VALUES (1) => ok affected=1 insert_id=0",
		"SAVEPOINT s => ok affected=0 insert_id=0",
		"CREATE TABLE ic2 (id int NOT NULL PRIMARY KEY) => ok affected=0 insert_id=0",
		"SELECT COUNT(*) FROM ic => 1",
		"INSERT INTO ic VALUES (2) => ok affected=1 insert_id=0",
		"SELECT COUNT(*) FROM ic => 2",
		"ROLLBACK TO SAVEPOINT s => error 1305",
		"ROLLBACK => ok affected=0 insert_id=0",
		"SELECT COUNT(*) FROM ic => 2",
		"DROP TABLE ic, ic2 => ok affected=0 insert_id=0",
	}
	if mdsn := review17MySQL(t); mdsn != "" {
		runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS heimdb_review17_ic", "CREATE DATABASE heimdb_review17_ic"})
		defer runScript(t, mdsn, []string{"DROP DATABASE IF EXISTS heimdb_review17_ic"})
		want = run(strings.Replace(strings.TrimSuffix(mdsn, "?timeout=2s"), "/", "/heimdb_review17_ic", 1) + "?timeout=2s")
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("step %d:\n  HeimDB: %s\n  mysql:  %s", i, got[i], want[i])
		}
	}
}

// 列のコメントにクォートがあると、SHOW CREATE TABLE がエスケープせずに書いていた（go-mysql-server の sql/parser.go。
// docs/plan.md の段階 17 で記録だけにした）。段階 114 で上流の #2997 を取り込み、MySQL と同じくエスケープする。
func TestReview17ColumnCommentQuoting(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE cm (id int NOT NULL PRIMARY KEY, v varchar(10) COMMENT 'it''s') COMMENT='tab''le'")
	var name, stmt string
	if err := m.QueryRow("SHOW CREATE TABLE cm").Scan(&name, &stmt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stmt, "COMMENT='tab''le'") {
		t.Errorf("table comment not escaped:\n%s", stmt)
	}
	if !strings.Contains(stmt, "COMMENT 'it''s'") {
		t.Errorf("column comment not escaped:\n%s", stmt)
	}
}

func firstLineWith(b []byte, sub string) string {
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

// TestReview17ConnectionSurvives は、上のテストで使った異常な文の後も接続が生きていることのまとめ（panic や切断の検出）。
func TestReview17ConnectionSurvives(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := m.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{
		"ROLLBACK TO SAVEPOINT nosuch",
		"RELEASE SAVEPOINT nosuch",
		"SAVEPOINT `weird name`",
		"ROLLBACK TO SAVEPOINT `weird name`",
		"START TRANSACTION",
		"SAVEPOINT a",
		"SAVEPOINT a",
		"RELEASE SAVEPOINT a",
		"ROLLBACK TO SAVEPOINT a",
		"LOCK TABLES wp_options WRITE",
		"SAVEPOINT b",
		"ROLLBACK TO SAVEPOINT b",
		"UNLOCK TABLES",
		"ROLLBACK",
	} {
		_, _ = c.ExecContext(ctx, q)
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_options").Scan(&n); err != nil {
		t.Fatalf("connection unusable: %v", err)
	}
	_ = fmt.Sprint(n)
}
