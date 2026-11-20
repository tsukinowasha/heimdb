package backend

import (
	"fmt"
	"strings"
	"testing"
)

// stage111Groups は、段階 111 で取り込んだ上流の go-mysql-server の修正の再現の文（docs/engine-upstream.md）。
// 直す前の HeimDB は本物の MySQL 8.0 と違う結果かエラーを返した。各組の終わりに作った表を消す。
var stage111Groups = []struct {
	pr    int
	stmts []string
}{
	{2995, []string{ // ENUM と SET の値の ' と \ を型の文字列でエスケープしなかった（表が作れず、残って消せない。\ は別の値に読まれた）
		"CREATE TABLE t (i INT PRIMARY KEY, e ENUM('it''s','b'), s SET('a''b','c'))",
		"SELECT COLUMN_NAME, COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 't' ORDER BY ORDINAL_POSITION",
		"INSERT INTO t VALUES (1, 'it''s', 'a''b'), (2, 'b', 'c,a''b')",
		"SELECT * FROM t ORDER BY i",
		"SELECT i FROM t WHERE e = 'it''s'",
		`CREATE TABLE q3 (i INT PRIMARY KEY, e ENUM('a\\b','c','d\'e'))`,
		"SELECT COLUMN_NAME, COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'q3' ORDER BY ORDINAL_POSITION",
		`INSERT INTO q3 VALUES (1, 'a\\b'), (2, 'd''e')`,
		"SELECT i, e FROM q3 ORDER BY i",
		`SELECT i FROM q3 WHERE e = 'a\\b'`,
		"ALTER TABLE q3 ADD COLUMN f ENUM('x''y','z') NOT NULL DEFAULT 'x''y'",
		"ALTER TABLE q3 MODIFY e ENUM('a\\\\b','c','d''e','f''g')",
		"SELECT COLUMN_NAME, COLUMN_TYPE FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'q3' ORDER BY ORDINAL_POSITION",
		"SELECT i, e, f FROM q3 ORDER BY i",
		"DROP TABLE q3",
		"DROP TABLE t",
	}},
	{3000, []string{ // 大文字を含む表名の UPDATE ... WHERE ... IN (SELECT ...) と UPDATE ... JOIN が何も更新しなかった（#3000、#3406）
		"CREATE TABLE MiXeDcAsE (i INT PRIMARY KEY, j INT)",
		"INSERT INTO MiXeDcAsE VALUES (1,1),(2,2)",
		"UPDATE MiXeDcAsE SET j = 999 WHERE i IN (SELECT 1)",
		"SELECT * FROM MiXeDcAsE ORDER BY i",
		"CREATE TABLE wp_Posts (ID INT PRIMARY KEY, j INT)",
		"INSERT INTO wp_Posts VALUES (1,1),(2,2)",
		"UPDATE wp_Posts SET j = 5 WHERE ID IN (SELECT 2)",
		"SELECT * FROM wp_Posts ORDER BY ID",
		"CREATE TABLE WPX_posts (ID INT PRIMARY KEY, post_status VARCHAR(20))",
		"CREATE TABLE WPX_postmeta (meta_id INT PRIMARY KEY, post_id INT, meta_value VARCHAR(20))",
		"INSERT INTO WPX_posts VALUES (1, 'draft')",
		"INSERT INTO WPX_postmeta VALUES (1, 1, 'publish')",
		"UPDATE WPX_posts p INNER JOIN WPX_postmeta pm ON pm.post_id = p.ID SET p.post_status = pm.meta_value",
		"SELECT * FROM WPX_posts",
		"UPDATE WPX_posts INNER JOIN WPX_postmeta ON WPX_postmeta.post_id = WPX_posts.ID SET WPX_posts.post_status = 'x'",
		"SELECT * FROM WPX_posts",
		"CREATE TABLE Items(ItemID CHAR(38) NOT NULL PRIMARY KEY, Version INT)",
		"INSERT INTO Items VALUES ('1234', 1)",
		"CREATE TABLE Items2(ItemID CHAR(38) NOT NULL PRIMARY KEY, Version INT)",
		"INSERT INTO Items2 VALUES ('1234', 2)",
		"UPDATE Items INNER JOIN Items2 ON (Items.ItemID = Items2.ItemID) SET Items.Version = Items2.Version WHERE Items.Version != Items2.Version",
		"SELECT * FROM Items",
		"UPDATE Items INNER JOIN Items2 ON (Items.ItemID = Items2.ItemID) SET Items.Version = 7, Items2.Version = Items2.Version",
		"SELECT * FROM Items",
		"SELECT * FROM Items2",
		"UPDATE Items INNER JOIN Items2 ON (Items.ItemID = Items2.ItemID) SET Items.Version = Items2.Version + 1, Items2.Version = 9",
		"SELECT i.Version, j.Version FROM Items i JOIN Items2 j ON i.ItemID = j.ItemID",
		"DROP TABLE Items2",
		"DROP TABLE Items",
		"DROP TABLE WPX_postmeta",
		"DROP TABLE WPX_posts",
		"DROP TABLE wp_Posts",
		"DROP TABLE MiXeDcAsE",
	}},
	{3103, []string{ // 符号なしの大きな整数を小さい符号なしの列に入れると、上の桁を捨てて受け付けた（4294967296 が 0）
		"CREATE TABLE src (v BIGINT UNSIGNED)",
		"INSERT INTO src VALUES (4294967296), (65536), (256), (18446744073709551615)",
		"CREATE TABLE dst (v INT UNSIGNED, s SMALLINT UNSIGNED, t TINYINT UNSIGNED)",
		"SET sql_mode = ''",
		"INSERT INTO dst SELECT v, v, v FROM src",
		"SHOW WARNINGS",
		"SELECT v, s, t FROM dst ORDER BY v, s, t",
		wpSQLMode,
		"INSERT INTO dst (v) SELECT v FROM src WHERE v = 4294967296",
		"SELECT COUNT(*), MAX(v) FROM dst",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE dst",
		"DROP TABLE src",
	}},
	{3527, []string{ // utf8mb4 の列に不正な UTF-8 を INSERT IGNORE と非 STRICT で入れると、空の値か 1105 だった（MySQL は切って警告 1366）
		"CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(255), tx TEXT) DEFAULT CHARSET=utf8mb4",
		"INSERT IGNORE INTO t VALUES (1, UNHEX('5353442031544220322E3585204E564D65'), 'a')",
		"SHOW WARNINGS",
		wpSQLMode,
		"INSERT INTO t VALUES (2, UNHEX('85'), UNHEX('61FF62')), (3, UNHEX('E3818285E38182'), UNHEX('E381'))",
		"SHOW WARNINGS",
		"UPDATE t SET name = UNHEX('61626364FE') WHERE id = 1",
		"SHOW WARNINGS",
		"INSERT INTO t VALUES (4, UNHEX('787985616263646566676869'), UNHEX('F09F98'))",
		"SHOW WARNINGS",
		"SELECT id, HEX(name), HEX(tx) FROM t ORDER BY id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE t",
	}},
	{3134, []string{ // ALTER で ENUM と SET の列を文字列の型にすると、値が番号（ENUM）とビット（SET）になった
		"CREATE TABLE t (i INT PRIMARY KEY, e ENUM('abc','def'), s SET('x','y'), e2 ENUM('p','q') NOT NULL)",
		"INSERT INTO t VALUES (1,'abc','x,y','q'),(2,'def','y','p'),(3,NULL,'','q'),(4,'abc',NULL,'p')",
		"ALTER TABLE t MODIFY e VARCHAR(10)",
		"ALTER TABLE t MODIFY s VARCHAR(10)",
		"ALTER TABLE t MODIFY e2 TEXT NOT NULL",
		"SELECT * FROM t ORDER BY i",
		"SELECT i FROM t WHERE e = 'def' AND s = 'y' AND e2 = 'p'",
		"DROP TABLE t",
	}},
	{3461, []string{ // ON DUPLICATE KEY UPDATE で ON UPDATE の列が進まなかった（#3461）。WITH 付きの UPDATE が ON UPDATE の列を取り違えた（#3643）
		"CREATE TABLE test (pk INT PRIMARY KEY, v1 INT, dt DATETIME NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO test (pk, v1) VALUES (1, 1), (2, 2)",
		"INSERT INTO test(pk, v1) VALUES (1, 2) ON DUPLICATE KEY UPDATE v1 = VALUES(v1)",
		"SELECT pk, v1, dt = '2000-01-01 00:00:00' FROM test ORDER BY pk",
		"INSERT INTO test(pk, v1) VALUES (2, 2) ON DUPLICATE KEY UPDATE v1 = VALUES(v1)",
		"SELECT pk, v1, dt = '2000-01-01 00:00:00' FROM test ORDER BY pk",
		"INSERT INTO test(pk, v1) VALUES (2, 5), (3, 3) ON DUPLICATE KEY UPDATE v1 = v1 + 1, dt = '2001-01-01 00:00:00'",
		"SELECT pk, v1, dt FROM test WHERE pk IN (2, 3) ORDER BY pk",
		"INSERT IGNORE INTO test(pk, v1) VALUES (3, 3) ON DUPLICATE KEY UPDATE v1 = 3",
		"SELECT pk, v1, dt = '2000-01-01 00:00:00' FROM test WHERE pk = 3",
		"CREATE TABLE t2 (id VARCHAR(64) PRIMARY KEY, val INT, updated_at DATETIME NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO t2 (id, val) VALUES ('a', 1), ('b', 1)",
		"WITH r(id) AS (SELECT 'a') UPDATE t2 SET val = 2 WHERE id IN (SELECT id FROM r)",
		"SELECT id, val, updated_at > '2000-01-01 00:00:00' FROM t2 ORDER BY id",
		"WITH r(id) AS (SELECT 'b') UPDATE t2 SET val = 1 WHERE id IN (SELECT id FROM r)",
		"SELECT id, val, updated_at > '2000-01-01 00:00:00' FROM t2 ORDER BY id",
		"DROP TABLE t2",
		"DROP TABLE test",
	}},
	{3468, []string{ // ON UPDATE の列のある表で、値の変わらない UPDATE が時刻を進め、影響を受けた行に数えた
		"CREATE TABLE t (id INT PRIMARY KEY, v INT, w VARCHAR(10), u DATETIME NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP, ts TIMESTAMP NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO t (id, v, w) VALUES (1, 1, 'a'), (2, 2, 'b'), (3, 3, 'c')",
		"UPDATE t SET v = 1 WHERE id = 1",
		"SELECT id, u = '2000-01-01 00:00:00', ts = '2000-01-01 00:00:00' FROM t ORDER BY id",
		"UPDATE t SET v = 2 WHERE id IN (1, 2)",
		"SELECT id, u = '2000-01-01 00:00:00', ts = '2000-01-01 00:00:00' FROM t ORDER BY id",
		"UPDATE t SET w = 'c', v = v WHERE id = 3",
		"UPDATE t SET u = '2001-01-01 00:00:00' WHERE id = 3",
		"SELECT id, u, ts = '2000-01-01 00:00:00' FROM t WHERE id = 3",
		"UPDATE t SET v = 3, u = u WHERE id = 3",
		"SELECT id, u, ts = '2000-01-01 00:00:00' FROM t WHERE id = 3",
		"DROP TABLE t",
	}},
	{3468, []string{ // 同じ形を WordPress の sql_mode（非 STRICT。HeimDB は SET の式を包み直す）で
		wpSQLMode,
		"CREATE TABLE t (id INT PRIMARY KEY, v INT, u DATETIME NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO t (id, v) VALUES (1, 1), (2, 2)",
		"UPDATE t SET v = 1 WHERE id = 1",
		"INSERT INTO t (id, v) VALUES (2, 2) ON DUPLICATE KEY UPDATE v = VALUES(v)",
		"SELECT id, u = '2000-01-01 00:00:00' FROM t ORDER BY id",
		"UPDATE t SET v = 5 WHERE id = 1",
		"INSERT INTO t (id, v) VALUES (2, 6) ON DUPLICATE KEY UPDATE v = VALUES(v)",
		"SELECT id, v, u = '2000-01-01 00:00:00' FROM t ORDER BY id",
		"DROP TABLE t",
		"SET SESSION sql_mode = DEFAULT",
	}},
	{3277, []string{ // LIMIT 0 の集計で、まだ埋めていない buffer を片付けて panic した（1105）
		"CREATE TABLE test_table (id INT PRIMARY KEY, value INT, category VARCHAR(50))",
		"INSERT INTO test_table VALUES (1, 100, 'A'), (2, 200, 'B'), (3, 300, 'A')",
		"SELECT SUM(value) FROM test_table LIMIT 0",
		"SELECT category, SUM(value) FROM test_table GROUP BY category LIMIT 0",
		"SELECT COUNT(*) FROM test_table LIMIT 0",
		"SELECT SUM(value) FROM test_table LIMIT 1",
		"DROP TABLE test_table",
	}},
}

// STRICT の sql_mode では、符号なしの大きな整数を小さい符号なしの列に入れる文を拒否する（段階 111、上流の #3103。前は上の桁を
// 捨てて受け付けた）。範囲の外のエラーの番号は、MySQL の 1264 でなく 1105（前からの違い。段階 113）なので、ゴールデンでなく
// ここで確かめる。
func TestStage111StrictUnsignedOutOfRange(t *testing.T) {
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{})
	defer stopServer(t, st, srv)()
	got := runScript(t, fmt.Sprintf("root@tcp(%s)/%s", addr, testDB), []string{
		"CREATE TABLE src (v BIGINT UNSIGNED)",
		"INSERT INTO src VALUES (4294967296), (256)",
		"CREATE TABLE dst (v INT UNSIGNED, t TINYINT UNSIGNED)",
		"SET sql_mode = 'STRICT_TRANS_TABLES'",
		"INSERT INTO dst (v) SELECT v FROM src WHERE v = 4294967296",
		"INSERT INTO dst (t) SELECT v FROM src WHERE v = 256",
		"SELECT COUNT(*) FROM dst",
	})
	for i, q := range []int{4, 5} {
		if !strings.HasPrefix(got[q], "error ") {
			t.Errorf("statement %d: %q, want an error", i, got[q])
		}
	}
	if got[6] != "0\n" {
		t.Errorf("rows in dst: %q", got[6])
	}
}

// 段階 111 で取り込んだ上流の修正の後、再現の文が MySQL と同じ結果になる。
func TestStage111UpstreamFixes(t *testing.T) {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage111Groups {
		stmts = append(stmts, g.stmts...)
	}
	checkGolden(t, "../testdata/stage111_golden.txt", "heimdb_golden111", stmts)
}
