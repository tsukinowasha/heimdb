package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// fulltextScript は、FULLTEXT インデックスの定義を本物の MySQL と比べる文の列（docs/plan.md の段階 10）。
// HeimDB は定義だけを持ち、MATCH ... AGAINST は扱わないので、ここでは比べない（TestFulltextMatchIsRejected）。
var fulltextScript = []string{
	// WordPress の dbDelta のテストの準備と同じ形。照合順序は文字セットの既定の utf8mb4_0900_ai_ci を指定する（FULLTEXT と
	// 無関係な違いを SHOW CREATE TABLE に出さないため: 指定しないと go-mysql-server の既定は utf8mb4_0900_bin、既定以外
	// を指定すると MySQL は列ごとに COLLATE を書く）。
	`CREATE TABLE wptests_dbdelta_test (id bigint(20) NOT NULL AUTO_INCREMENT,
					column_1 varchar(255) NOT NULL,
					column_2 text,
					column_3 blob,
					PRIMARY KEY  (id),
					KEY key_1 (column_1(191)),
					KEY compound_key (id,column_1(191)),
					FULLTEXT KEY fulltext_key (column_1)) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci`,
	"SHOW INDEX FROM wptests_dbdelta_test",
	"SHOW CREATE TABLE wptests_dbdelta_test",
	"SHOW COLUMNS FROM wptests_dbdelta_test",
	// SUB_PART は比べない（go-mysql-server はプレフィックス長のないインデックスで NULL ではなく 0 を返す。FULLTEXT とは無関係）。
	"SELECT INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME, NON_UNIQUE, NULLABLE, INDEX_TYPE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'wptests_dbdelta_test' ORDER BY INDEX_NAME, SEQ_IN_INDEX",
	"INSERT INTO wptests_dbdelta_test (column_1, column_2) VALUES ('hello world', 'a'), ('second row', 'b')",
	"SELECT id FROM wptests_dbdelta_test WHERE column_1 = 'hello world'",
	// 作成、名前なしの作成、複数の列、名前の変更、削除
	"CREATE FULLTEXT INDEX ft_2 ON wptests_dbdelta_test (column_2)",
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT (column_1, column_2)",
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT INDEX key_5 (column_1)",
	"SHOW INDEX FROM wptests_dbdelta_test",
	"ALTER TABLE wptests_dbdelta_test RENAME INDEX ft_2 TO ft_2b",
	"DROP INDEX ft_2b ON wptests_dbdelta_test",
	"ALTER TABLE wptests_dbdelta_test DROP INDEX column_1",
	"SHOW INDEX FROM wptests_dbdelta_test",
	// 文字列でない列、BLOB には作れない
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT KEY bad (id)",
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT KEY bad (column_3)",
	// 列の変更の後も定義が残る
	"ALTER TABLE wptests_dbdelta_test MODIFY column_1 varchar(300) NOT NULL",
	"ALTER TABLE wptests_dbdelta_test CHANGE column_1 column_1b varchar(300) NOT NULL",
	"SHOW INDEX FROM wptests_dbdelta_test",
	"SHOW CREATE TABLE wptests_dbdelta_test",
	// 列を消すと、FULLTEXT インデックスからも外れる（最後の列なら消える）
	// （MySQL は 1 つの ALTER で FULLTEXT を 2 つ作れない: エラー 1795。HeimDB はそこまでは合わせない）
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT KEY ft_multi (column_1b, column_2)",
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT KEY ft_only2 (column_2)",
	"ALTER TABLE wptests_dbdelta_test DROP COLUMN column_2",
	"SHOW INDEX FROM wptests_dbdelta_test",
	// FULLTEXT インデックスの TEXT 列の型を変えられる（dbDelta は型が変わると CHANGE COLUMN を出す）
	"ALTER TABLE wptests_dbdelta_test ADD COLUMN body text",
	"ALTER TABLE wptests_dbdelta_test ADD FULLTEXT KEY ft_body (body)",
	"ALTER TABLE wptests_dbdelta_test MODIFY body longtext",
	"ALTER TABLE wptests_dbdelta_test CHANGE body body mediumtext",
	"ALTER TABLE wptests_dbdelta_test MODIFY body text COMMENT 'c'",
	"ALTER TABLE wptests_dbdelta_test ADD COLUMN c2 text, ADD FULLTEXT KEY ft_c2 (c2)",
	"ALTER TABLE wptests_dbdelta_test ADD COLUMN c3 int, ADD FULLTEXT KEY ft_c3 (c3)",
	"SHOW COLUMNS FROM wptests_dbdelta_test LIKE 'c%'",
	"ALTER TABLE wptests_dbdelta_test DROP COLUMN c2",
	"SHOW INDEX FROM wptests_dbdelta_test",
	// FULLTEXT インデックスの列での結合とサブクエリ
	"INSERT INTO wptests_dbdelta_test (column_1b, body) VALUES ('a', 'x'), ('b', 'y'), ('a', 'z')",
	"SELECT a.id FROM wptests_dbdelta_test a JOIN wptests_dbdelta_test b ON a.column_1b = b.column_1b WHERE b.body = 'y'",
	"SELECT id FROM wptests_dbdelta_test WHERE column_1b IN (SELECT column_1b FROM wptests_dbdelta_test WHERE body = 'z') ORDER BY id",
	"SELECT id FROM wptests_dbdelta_test WHERE column_1b = 'a' ORDER BY id",
	"SELECT id, column_1b FROM wptests_dbdelta_test ORDER BY column_1b, id",
	// CREATE TABLE ... LIKE は FULLTEXT とプレフィックス長を写す
	"CREATE TABLE wptests_like_copy LIKE wptests_dbdelta_test",
	"SHOW INDEX FROM wptests_like_copy",
	"DROP TABLE wptests_like_copy",
	"TRUNCATE TABLE wptests_dbdelta_test",
	"INSERT INTO wptests_dbdelta_test (column_1b, column_2) VALUES ('x', 'y')",
	"SELECT id, column_1b FROM wptests_dbdelta_test",
	"DROP TABLE wptests_dbdelta_test",
	// 一時テーブルには作れない
	"CREATE TEMPORARY TABLE tmp_ft (id int NOT NULL, body text, PRIMARY KEY (id), FULLTEXT KEY body (body))",
	"CREATE TEMPORARY TABLE tmp_ft (id int NOT NULL, body text, PRIMARY KEY (id))",
	// 作れない FULLTEXT を含む CREATE TABLE は、テーブルも作らない
	"CREATE TABLE bad_ft (id int NOT NULL PRIMARY KEY, FULLTEXT KEY f (id))",
	"SHOW TABLES LIKE 'bad\\_ft'",
}

func TestFulltextMatchesMySQL(t *testing.T) {
	stmts := append([]string{wpSQLMode}, fulltextScript...)
	checkGolden(t, "../testdata/fulltext_golden.txt", "heimdb_fulltext_golden", stmts)
}

// MATCH ... AGAINST は、接続を切らずにエラーになる（HeimDB は FULLTEXT の定義だけを持つ）。
func TestFulltextMatchIsRejected(t *testing.T) {
	mainDB, _, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE wp_ft (id bigint unsigned NOT NULL PRIMARY KEY, body text, FULLTEXT KEY body (body))")
	mustExec(t, mainDB, "INSERT INTO wp_ft VALUES (1, 'hello world')")
	for _, q := range []string{
		"SELECT id FROM wp_ft WHERE MATCH (body) AGAINST ('hello')",
		"SELECT id FROM wp_ft WHERE MATCH (body) AGAINST ('+hello' IN BOOLEAN MODE)",
	} {
		_, err := mainDB.Exec(q)
		t.Logf("%s: %v", q, err)
		if err == nil {
			t.Errorf("%s should fail", q)
		}
	}
	if got := one(t, mainDB, "SELECT body FROM wp_ft WHERE id = 1"); got != "hello world" {
		t.Errorf("connection after MATCH: %q", got)
	}
}

// FULLTEXT の定義は、再起動、ブランチで作ったテーブルのマージ、復元で保たれる。
func TestFulltextDefinitionsPersist(t *testing.T) {
	dir := t.TempDir()
	dsn, stop := startServer(t, dir)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		mustExec(t, db, q)
	}
	mustExec(t, db, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, db, "CREATE TABLE wp_ft (id bigint unsigned NOT NULL PRIMARY KEY, body text, FULLTEXT KEY body (body))")
	indexType := func(db *sql.DB, table, index string) string {
		t.Helper()
		for _, r := range diffRows(t, db, "SHOW INDEX FROM "+table) {
			f := strings.Split(r, "|")
			if f[2] == index {
				return f[10]
			}
		}
		return ""
	}
	db.Close()
	stop()

	dsn, stop = startServer(t, dir)
	defer stop()
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := indexType(db, "wp_ft", "body"); got != "FULLTEXT" {
		t.Errorf("after restart: %q", got)
	}

	mustExec(t, db, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "CREATE TABLE wp_plugin_ft (id bigint unsigned NOT NULL PRIMARY KEY, title varchar(100) NOT NULL, FULLTEXT KEY title (title))")
	diffRows(t, db, "CALL heimdb_branch_merge('staging')")
	if got := indexType(db, "wp_plugin_ft", "title"); got != "FULLTEXT" {
		t.Errorf("table created on the branch, after merge: %q", got)
	}

	tAt := markLocal(t)
	mustExec(t, db, "ALTER TABLE wp_ft DROP INDEX body")
	diffRows(t, db, "CALL heimdb_restore('main', '"+tAt+"', 'wp_ft')")
	if got := indexType(db, "wp_ft", "body"); got != "FULLTEXT" {
		t.Errorf("after restore: %q", got)
	}
}
