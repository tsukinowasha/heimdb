package backend

import (
	"strings"
	"testing"
)

// alterScript は、列の変更、TRUNCATE、一時テーブルを本物の MySQL と比べる文の列。
// go-mysql-server の構文解析器は、AFTER name や CHANGE name の name を予約語として扱って構文エラーにする
// （MySQL は受け付ける）ので、列名に name を使わない。
var alterScript = []string{
	"CREATE TABLE t (id bigint unsigned NOT NULL AUTO_INCREMENT, label varchar(50) NOT NULL DEFAULT '', score int, PRIMARY KEY (id), KEY label (label), UNIQUE KEY u_score (score)) DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_unicode_520_ci",
	"INSERT INTO t (label, score) VALUES ('A', 10), ('B', NULL), ('B', 30), ('C', NULL)",
	// 列の追加（DEFAULT、NOT NULL、位置の指定）
	"ALTER TABLE t ADD COLUMN extra varchar(20) NOT NULL DEFAULT 'x' AFTER label",
	"SELECT * FROM t ORDER BY id",
	"ALTER TABLE t ADD COLUMN c2 int",
	"ALTER TABLE t ADD COLUMN first_col int NOT NULL DEFAULT 7 FIRST",
	"SELECT * FROM t ORDER BY id",
	// 型の変更、名前の変更
	"ALTER TABLE t MODIFY score bigint",
	"ALTER TABLE t CHANGE label title varchar(100) NOT NULL DEFAULT ''",
	"SELECT id FROM t WHERE title = 'b' ORDER BY id",
	"ALTER TABLE t CHANGE title title2 varchar(100) NOT NULL DEFAULT ''",
	"SELECT id, title2 FROM t WHERE title2 = 'C'",
	"ALTER TABLE t ALTER COLUMN extra SET DEFAULT 'y'",
	"INSERT INTO t (title2, score) VALUES ('D', 40)",
	"SELECT id, extra FROM t WHERE title2 = 'D'",
	"ALTER TABLE t RENAME COLUMN c2 TO c3",
	"SELECT c3 FROM t WHERE id = 1",
	"SHOW INDEX FROM t",
	// 一意制約: 既存の行が重複する一意インデックスは作れない
	"ALTER TABLE t ADD UNIQUE KEY u_title (title2)",
	"INSERT INTO t (title2, score) VALUES ('E', 40)",
	// 列の削除（その列のインデックスも消える）
	"ALTER TABLE t DROP COLUMN extra",
	"ALTER TABLE t DROP COLUMN score",
	"SHOW INDEX FROM t",
	"SELECT * FROM t ORDER BY id",
	"INSERT INTO t (title2) VALUES ('F')",
	"SELECT id, title2 FROM t ORDER BY id DESC LIMIT 2",
	// WordPress の dbDelta と同じ形
	"ALTER TABLE wp_posts CHANGE COLUMN `post_title` post_title text NOT NULL",
	"ALTER TABLE wp_options ADD COLUMN `option_flags` int(11) NOT NULL DEFAULT 0",
	"INSERT INTO wp_options (option_name, option_value) VALUES ('a', '1')",
	"SELECT option_name, option_flags FROM wp_options",
	"SHOW INDEX FROM wp_options",
	// 条件のない DELETE はトランザクションの一部（go-mysql-server が Truncate に変換しても戻せる）
	"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (1, 1), (2, 1)",
	"START TRANSACTION",
	"DELETE FROM wp_term_relationships",
	"SELECT COUNT(*) FROM wp_term_relationships",
	"ROLLBACK",
	"SELECT COUNT(*) FROM wp_term_relationships",
	// TRUNCATE は AUTO_INCREMENT を 1 に戻す
	"TRUNCATE TABLE t",
	"SELECT COUNT(*) FROM t",
	"INSERT INTO t (title2) VALUES ('G')",
	"SELECT id, title2 FROM t",
	// 一時テーブル
	"CREATE TEMPORARY TABLE tmp (id int NOT NULL, v varchar(10), PRIMARY KEY (id))",
	"INSERT INTO tmp VALUES (1, 'a'), (2, 'b')",
	"SELECT * FROM tmp ORDER BY id",
	"SHOW TABLES LIKE 'tmp'",
	"CREATE TEMPORARY TABLE wp_links (id int NOT NULL, PRIMARY KEY (id))",
	"SELECT COUNT(*) FROM wp_links",
	"DROP TEMPORARY TABLE wp_links",
	"SHOW COLUMNS FROM wp_links LIKE 'link_id'",
	"DROP TEMPORARY TABLE tmp",
	"SELECT * FROM tmp",
}

func TestAlterMatchesMySQL(t *testing.T) {
	stmts := append(append([]string{wpSQLMode}, wpSchema()...), alterScript...)
	checkGolden(t, "../testdata/alter_golden.txt", "heimdb_alter_golden", stmts)
}

// 列の変更と TRUNCATE の前の時点も、AS OF でそのときの定義と行が読める。
func TestAsOfAcrossAlter(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	run := func(q string) {
		t.Helper()
		if _, err := query(s.ctx.WithQuery(q), s.engine, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run("SET time_zone = '+00:00'")
	run("CREATE TABLE x (id int NOT NULL, a varchar(10), PRIMARY KEY (id), KEY a (a))")
	run("INSERT INTO x VALUES (1, 'p'), (2, 'q')")
	t0 := mark(t)
	run("ALTER TABLE x ADD COLUMN b int DEFAULT 5")
	run("UPDATE x SET b = 6 WHERE id = 1")
	t1 := mark(t)
	run("ALTER TABLE x DROP COLUMN a")
	t2 := mark(t)
	run("TRUNCATE TABLE x")
	t3 := mark(t)

	check := func(q, want string) {
		t.Helper()
		if got := rowsString(t, s, q); got != want {
			t.Errorf("%s\n got: %q\nwant: %q", q, got, want)
		}
	}
	check("SELECT * FROM x AS OF '"+t0+"' ORDER BY id", "[1 p]\n[2 q]\n")
	check("SELECT * FROM x AS OF '"+t1+"' ORDER BY id", "[1 p 6]\n[2 q 5]\n")
	check("SELECT * FROM x AS OF '"+t2+"' ORDER BY id", "[1 6]\n[2 5]\n")
	check("SELECT * FROM x AS OF '"+t3+"' ORDER BY id", "")
	check("SELECT id FROM x AS OF '"+t1+"' WHERE a = 'q'", "[2]\n")
	check("SELECT * FROM x", "")
	run("INSERT INTO x (id) VALUES (3)")
	check("SELECT * FROM x", "[3 5]\n")
}

// テーブルのコメントが保存される（go-mysql-server の CreateIndexedTable の経路では失われていた）。
func TestTableComment(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE c (id int NOT NULL, PRIMARY KEY (id)) COMMENT='hello heimdb'")
	got := rowsString(t, s, "SHOW CREATE TABLE c")
	if !strings.Contains(got, "COMMENT='hello heimdb'") {
		t.Errorf("table comment lost: %s", got)
	}
}
