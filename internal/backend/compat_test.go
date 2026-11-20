package backend

import (
	"context"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// compatScript は、段階 11 で直した小さな差を本物の MySQL と比べる文の列。
var compatScript = []string{
	// A: SET NAMES ... COLLATE
	"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci",
	"SELECT @@character_set_client, @@character_set_connection, @@character_set_results, @@collation_connection",
	"SET NAMES 'utf8mb4' COLLATE 'utf8mb4_unicode_ci'",
	"SELECT @@collation_connection",
	"SET NAMES utf8mb4",
	"SELECT @@collation_connection",
	"SET NAMES utf8 COLLATE utf8_general_ci",
	"SELECT @@collation_connection",
	"SET NAMES utf8mb4 COLLATE latin1_swedish_ci",
	"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci, sql_mode = ''",
	"SELECT @@collation_connection, @@sql_mode",
	// 文字列やコメントの中の NAMES ... COLLATE には反応しない
	"SET @msg = 'NAMES utf8mb4 COLLATE nope'",
	"SET @q = 'SET NAMES latin1 COLLATE latin1_bin'",
	"SET /* names utf8mb4 collate xx */ @x = 1",
	"SELECT @msg, @q, @@collation_connection",
	"SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
	// B: SET autocommit=1 は未コミットのトランザクションをコミットする
	"SET autocommit = 0",
	"INSERT INTO wp_options (option_name, option_value) VALUES ('ac_1', 'v')",
	"SET autocommit = 1",
	"ROLLBACK",
	"SELECT COUNT(*) FROM wp_options WHERE option_name = 'ac_1'",
	// 既に autocommit=1 なら、SET autocommit=1 はトランザクションをコミットしない（START TRANSACTION の中）
	"START TRANSACTION",
	"INSERT INTO wp_options (option_name, option_value) VALUES ('ac_2', 'v')",
	"SET autocommit = 1",
	"ROLLBACK",
	"SELECT COUNT(*) FROM wp_options WHERE option_name = 'ac_2'",
	"SET autocommit = 0",
	"INSERT INTO wp_options (option_name, option_value) VALUES ('ac_3', 'v')",
	"SET @@autocommit = ON",
	"ROLLBACK",
	"SELECT COUNT(*) FROM wp_options WHERE option_name = 'ac_3'",
	// D: 照合順序を指定しないテーブルは utf8mb4_0900_ai_ci（大文字と小文字を区別しない）
	"CREATE TABLE plain_c (id int NOT NULL PRIMARY KEY, name varchar(50), body text, b varbinary(10))",
	"SHOW FULL COLUMNS FROM plain_c",
	"INSERT INTO plain_c VALUES (1, 'Hello', 'World', 'x')",
	"SELECT id FROM plain_c WHERE name = 'hello' AND body LIKE 'world'",
	"ALTER TABLE plain_c ADD COLUMN extra varchar(10)",
	"SHOW FULL COLUMNS FROM plain_c LIKE 'extra'",
	"CREATE TABLE plain_bin (id int NOT NULL PRIMARY KEY, name varchar(50) COLLATE utf8mb4_0900_bin)",
	"INSERT INTO plain_bin VALUES (1, 'Hello')",
	"SELECT COUNT(*) FROM plain_bin WHERE name = 'hello'",
	"SHOW FULL COLUMNS FROM plain_bin",
	// SHOW CREATE TABLE は比べない（go-mysql-server は NULL を許す列の DEFAULT NULL と、列の COLLATE の前の
	// CHARACTER SET を書かない。照合順序とは別の、表示の差）。
	"CREATE TEMPORARY TABLE tmp_c (id int NOT NULL PRIMARY KEY, name varchar(50))",
	"INSERT INTO tmp_c VALUES (1, 'Hello')",
	"SELECT id FROM tmp_c WHERE name = 'HELLO'",
	// J: 同順位の行の並び（type_status_date の逆方向の走査。別名を付けても同じ）
	"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (11, 1, '2026-10-01 10:00:00', '', 'a', '', 'publish', 'a', '', '', '', 'post')",
	"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (12, 1, '2026-10-01 10:00:00', '', 'b', '', 'publish', 'b', '', '', '', 'post')",
	"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (13, 1, '2026-10-02 10:00:00', '', 'c', '', 'publish', 'c', '', '', '', 'post')",
	"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (14, 1, '2026-10-01 10:00:00', '', 'd', '', 'publish', 'd', '', '', '', 'post')",
	"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.post_type = 'post' AND wp_posts.post_status = 'publish' ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
	"SELECT p.ID FROM wp_posts p WHERE p.post_type = 'post' AND p.post_status = 'publish' ORDER BY p.post_date DESC LIMIT 0, 10",
	"SELECT p.ID FROM wp_posts AS p WHERE p.post_type = 'post' AND p.post_status IN ('publish') ORDER BY p.post_date DESC",
	"SELECT p.ID FROM wp_posts AS p WHERE p.post_type = 'post' AND p.post_status = 'publish' ORDER BY p.post_date ASC",
	// E: 末尾が単独のエスケープ文字の LIKE は、その文字そのものに一致する
	`INSERT INTO wp_options (option_name, option_value) VALUES ('x\\', '1'), ('x|', '1'), ('x\\\\', '1')`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE 'x\\'`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE 'x\\\\\\'`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE 'x|' ESCAPE '|'`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE 'x\\\\' ORDER BY option_name`,
	// G: 1 つの ALTER で FULLTEXT を 2 つ作るとエラー 1795
	"CREATE TABLE ft2 (id int NOT NULL PRIMARY KEY, a text, b text)",
	"ALTER TABLE ft2 ADD FULLTEXT KEY fa (a), ADD FULLTEXT KEY fb (b)",
	"SHOW INDEX FROM ft2",
	"ALTER TABLE ft2 ADD FULLTEXT KEY fa (a)",
	"ALTER TABLE ft2 ADD FULLTEXT KEY fb (b), ADD KEY kb (b(10))",
	"SHOW INDEX FROM ft2",
	// H: プレフィックス長のないインデックスの SUB_PART は NULL（列ごとにプレフィックス長が違う複合インデックスは対象外）
	"SELECT TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME, SUB_PART FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN ('wp_postmeta', 'wp_term_relationships') ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX",
	// I: 文字列の中の \_ と \% は \ を残す（ほかの未知のエスケープは \ を落とす）
	`SELECT '\_', LENGTH('\_'), '\%', 'a\qb', '\\_', "\_", 'it''s \_'`,
	"INSERT INTO wp_options (option_name, option_value) VALUES ('x_y', '1'), ('xay', '1')",
	`SELECT option_name FROM wp_options WHERE option_name LIKE 'x\_y' ORDER BY option_name`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE 'x_y' ORDER BY option_name`,
	"SELECT `option_name` FROM wp_options WHERE option_name = 'x_y' /* '\\_' */",
	// F（段階 15）: 非 STRICT の sql_mode で、NULL を含む列を NOT NULL にする ALTER は、NULL を型の既定値（列の既定値
	// ではない）にして、行ごとに警告 1265 を出す。STRICT ではエラー 1138。
	"CREATE TABLE nn (id int NOT NULL PRIMARY KEY, s varchar(20) NULL, n int NULL, d datetime NULL, e enum('a','b') NULL, s2 varchar(10) NULL DEFAULT 'x', KEY ks (s)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
	"INSERT INTO nn VALUES (1, NULL, NULL, NULL, NULL, NULL), (2, 'v', 5, '2020-01-01', 'b', 'y'), (3, NULL, NULL, NULL, NULL, NULL)",
	"ALTER TABLE nn MODIFY s varchar(20) NOT NULL",
	"SHOW WARNINGS",
	"ALTER TABLE nn MODIFY n int NOT NULL",
	"SHOW WARNINGS",
	"ALTER TABLE nn MODIFY d datetime NOT NULL",
	"SHOW WARNINGS",
	"ALTER TABLE nn MODIFY e enum('a','b') NOT NULL",
	"SHOW WARNINGS",
	"ALTER TABLE nn CHANGE s2 s3 varchar(10) NOT NULL DEFAULT 'x'",
	"SHOW WARNINGS",
	"SELECT id, s, n, d, e, s3 FROM nn ORDER BY id",
	"SELECT id FROM nn WHERE s = '' ORDER BY id",
	"SHOW FULL COLUMNS FROM nn",
	"CREATE TABLE nn2 (id int NOT NULL PRIMARY KEY, s varchar(20) NULL)",
	"INSERT INTO nn2 VALUES (1, NULL), (2, 'v')",
	"SET SESSION sql_mode = 'STRICT_TRANS_TABLES'",
	"ALTER TABLE nn2 MODIFY s varchar(20) NOT NULL",
	"SELECT id, s FROM nn2 ORDER BY id",
	"SHOW FULL COLUMNS FROM nn2",
	// TRADITIONAL は STRICT を含む（go-mysql-server は展開しない）
	"SET SESSION sql_mode = 'TRADITIONAL'",
	"ALTER TABLE nn2 MODIFY s varchar(20) NOT NULL",
	"SELECT id, s FROM nn2 ORDER BY id",
	"SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
	// TIMESTAMP の列の NULL は文の時刻になり、警告は出ない（DATETIME は型の既定値と警告 1265）
	"CREATE TABLE ts1 (id int NOT NULL PRIMARY KEY, a timestamp NULL, b timestamp NULL, e datetime NULL)",
	"INSERT INTO ts1 (id) VALUES (1), (2)",
	"ALTER TABLE ts1 MODIFY a timestamp NOT NULL",
	"SHOW WARNINGS",
	"ALTER TABLE ts1 MODIFY b timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP",
	"SHOW WARNINGS",
	"ALTER TABLE ts1 MODIFY e datetime NOT NULL DEFAULT CURRENT_TIMESTAMP",
	"SHOW WARNINGS",
	"SELECT id, a BETWEEN NOW() - INTERVAL 1 MINUTE AND NOW(), b BETWEEN NOW() - INTERVAL 1 MINUTE AND NOW(), e FROM ts1 ORDER BY id",
}

func TestCompatMatchesMySQL(t *testing.T) {
	stmts := append(append([]string{wpSQLMode}, wpSchema()...), compatScript...)
	checkGolden(t, "../testdata/compat_golden.txt", "heimdb_compat_golden", stmts)
}

// C: 一時テーブルにだけ書くトランザクションは、ブランチの書き込みロックを取らない（他の接続の書き込みを止めない）。
// 一時テーブルと同じ名前を含む文や、ふつうのテーブルにも書く文は、これまでどおりロックを取る。
func TestCompatTemporaryWritesDoNotLockBranch(t *testing.T) {
	db, _, stop := concServer(t, func(s *store.Store) { s.LockTimeout = 300 * time.Millisecond })
	defer stop()
	c1, c2 := conn(t, db), conn(t, db)
	defer c1.Close()
	defer c2.Close()
	ctx := context.Background()
	exec(t, c1, "CREATE TEMPORARY TABLE tmp (id int NOT NULL PRIMARY KEY, v int)")
	exec(t, c1, "BEGIN")
	for _, q := range []string{"INSERT INTO tmp VALUES (1, 1)", "UPDATE tmp SET v = 2 WHERE id = 1", "DELETE FROM tmp WHERE id = 1", "REPLACE INTO tmp VALUES (2, 2)"} {
		exec(t, c1, q)
		if _, err := c2.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value) VALUES (CONCAT('t_', RAND()), 'v')"); err != nil {
			t.Errorf("another connection's write after %q: %v", q, err)
		}
	}
	// ふつうのテーブルに書けば、ブランチのロックを取る。
	exec(t, c1, "INSERT INTO wp_options (option_name, option_value) VALUES ('mine', 'v')")
	if _, err := c2.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value) VALUES ('blocked', 'v')"); mysqlErrNo(err) != 1205 {
		t.Errorf("write while c1 holds the branch lock: %v", err)
	}
	exec(t, c1, "COMMIT")
	if got := queryString(t, c1, "SELECT COUNT(*) FROM tmp"); got != "1" {
		t.Errorf("temporary rows after commit: %s", got)
	}
}

// 一時テーブルと同じ名前の別名で本物のテーブルを消す DELETE は、ブランチのロックを文の前に取る（取らないと、同時に
// 書く接続のコミットを待たずにスナップショットを決め、エラー 1213 になる）。
func TestCompatDeleteAliasNamedLikeTemporaryTable(t *testing.T) {
	db, _, stop := concServer(t, func(s *store.Store) { s.LockTimeout = 2 * time.Second })
	defer stop()
	exec(t, conn(t, db), "INSERT INTO wp_options (option_name, option_value) VALUES ('victim', '1')")
	c1, c2 := conn(t, db), conn(t, db)
	defer c1.Close()
	defer c2.Close()
	exec(t, c1, "CREATE TEMPORARY TABLE t (id int NOT NULL PRIMARY KEY)")
	exec(t, c2, "BEGIN")
	exec(t, c2, "UPDATE wp_options SET option_value = '2' WHERE option_name = 'victim'")
	done := make(chan error, 1)
	go func() {
		_, err := c1.ExecContext(context.Background(), "DELETE t FROM wp_options AS t WHERE option_name = 'victim'")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	exec(t, c2, "COMMIT")
	if err := <-done; err != nil {
		t.Errorf("DELETE through an alias named like a temporary table: %v", err)
	}
	if got := queryString(t, c1, "SELECT COUNT(*) FROM wp_options WHERE option_name = 'victim'"); got != "0" {
		t.Errorf("row not deleted: count = %s", got)
	}
}
