package backend

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 15（非 STRICT の sql_mode での NOT NULL の ALTER）のレビューの再現テスト。期待値は MySQL 8.0.46 で確かめた振る舞い。

// r15Conn は、WordPress の sql_mode を設定した 1 本の接続を返す。
func r15Conn(t *testing.T, db *sql.DB) *sql.Conn {
	t.Helper()
	c := conn(t, db)
	exec(t, c, wpSQLMode)
	exec(t, c, "SET time_zone = '+00:00'")
	return c
}

// r15Rows は q の全行を「列|列」の行にして改行でつなぐ。
func r15Rows(t *testing.T, c *sql.Conn, q string) string {
	t.Helper()
	return joinCells(queryCells(t, c, q, "NULL"), "|", "\n")
}

// r15Warnings は SHOW WARNINGS を「Code|Message」の行にする。
func r15Warnings(t *testing.T, c *sql.Conn) string {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), "SHOW WARNINGS")
	if err != nil {
		t.Fatalf("SHOW WARNINGS: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var level, msg string
		var code int
		if err := rows.Scan(&level, &code, &msg); err != nil {
			t.Fatal(err)
		}
		out = append(out, level+"|"+strconv.Itoa(code)+"|"+msg)
	}
	return strings.Join(out, "\n")
}

func r15Err(c *sql.Conn, q string) error {
	_, err := c.ExecContext(context.Background(), q)
	return err
}

// r15Column は SHOW FULL COLUMNS の 1 列の Null の値を返す。
func r15Nullable(t *testing.T, c *sql.Conn, table, col string) string {
	t.Helper()
	for _, line := range strings.Split(r15Rows(t, c, "SHOW FULL COLUMNS FROM "+table), "\n") {
		parts := strings.Split(line, "|")
		if parts[0] == col {
			return parts[3]
		}
	}
	t.Fatalf("column %s not in %s", col, table)
	return ""
}

func r15Server(t *testing.T) (db *sql.DB, c *sql.Conn, stop func()) {
	t.Helper()
	db, _, stopServer := concServer(t, func(s *store.Store) { s.LockTimeout = 500 * time.Millisecond })
	c = r15Conn(t, db)
	return db, c, func() {
		c.Close()
		stopServer()
	}
}

// 型ごとの置き換える値は MySQL と同じ（文字列は ”、数値は 0、datetime/date は 0 の日付、decimal は 0.00、SET は ”、
// バイナリと BLOB と TEXT は ”）。警告は行ごとに 1265。
func TestReview15ZeroValuesPerType(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE t1 (id int NOT NULL PRIMARY KEY, n int NULL, s varchar(10) NULL, dt datetime NULL, d date NULL, dec1 decimal(10,2) NULL, bu bigint unsigned NULL, ti tinyint NULL, st set('a','b') NULL, vb varbinary(5) NULL, bl blob NULL, tx longtext NULL) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	exec(t, c, "INSERT INTO t1 (id) VALUES (1), (2)")
	exec(t, c, "INSERT INTO t1 VALUES (3, 5, '7', '2020-01-01', '2020-01-01', 1.5, 7, 1, 'a', 'v', 'b', 't')")
	for col, def := range map[string]string{
		"n": "varchar(10) NOT NULL", "s": "int NOT NULL", "dt": "datetime NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP",
		"d": "date NOT NULL", "dec1": "decimal(10,2) NOT NULL", "bu": "bigint unsigned NOT NULL", "ti": "tinyint NOT NULL",
		"st": "set('a','b') NOT NULL", "vb": "varbinary(5) NOT NULL", "bl": "blob NOT NULL", "tx": "longtext NOT NULL",
	} {
		exec(t, c, "ALTER TABLE t1 MODIFY "+col+" "+def)
		want := "Warning|1265|Data truncated for column '" + col + "' at row 1\nWarning|1265|Data truncated for column '" + col + "' at row 2"
		if got := r15Warnings(t, c); got != want {
			t.Errorf("warnings after MODIFY %s:\n%s\nwant:\n%s", col, got, want)
		}
	}
	got := r15Rows(t, c, "SELECT id, n, s, dt, d, dec1, bu, ti, st, HEX(vb), HEX(bl), tx FROM t1 ORDER BY id")
	want := "1||0|0000-00-00 00:00:00|0000-00-00|0.00|0|0||||\n2||0|0000-00-00 00:00:00|0000-00-00|0.00|0|0||||\n3|5|7|2020-01-01 00:00:00|2020-01-01|1.50|7|1|a|76|62|t"
	if got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
}

// 型も同時に変える ALTER: int NULL -> varchar NOT NULL は ”、varchar NULL -> datetime NOT NULL は 0 の日付（MySQL と同じ）。
func TestReview15TypeChangeWithNotNull(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE t2 (id int NOT NULL PRIMARY KEY, v varchar(20) NULL, n int NULL)")
	exec(t, c, "INSERT INTO t2 VALUES (1, NULL, NULL), (2, '2021-02-03 04:05:06', 9)")
	exec(t, c, "ALTER TABLE t2 MODIFY v datetime NOT NULL")
	if got, want := r15Warnings(t, c), "Warning|1265|Data truncated for column 'v' at row 1"; got != want {
		t.Errorf("warnings: %q, want %q", got, want)
	}
	exec(t, c, "ALTER TABLE t2 MODIFY n varchar(10) NOT NULL")
	if got, want := r15Rows(t, c, "SELECT * FROM t2 ORDER BY id"), "1|0000-00-00 00:00:00|\n2|2021-02-03 04:05:06|9"; got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
}

// TIMESTAMP の列は MySQL では NULL を今の時刻にし、警告を出さない（DEFAULT や ON UPDATE の有無によらない）。
// DATETIME は 0 の日付と警告 1265。
func TestReview15TimestampBecomesCurrentTime(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE ts1 (id int NOT NULL PRIMARY KEY, a timestamp NULL, b timestamp NULL, e datetime NULL)")
	exec(t, c, "INSERT INTO ts1 (id) VALUES (1)")
	exec(t, c, "ALTER TABLE ts1 MODIFY a timestamp NOT NULL")
	if got := r15Warnings(t, c); got != "" {
		t.Errorf("warnings after timestamp NOT NULL: %q, want none", got)
	}
	exec(t, c, "ALTER TABLE ts1 MODIFY b timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP")
	if got := r15Warnings(t, c); got != "" {
		t.Errorf("warnings after timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP: %q, want none", got)
	}
	exec(t, c, "ALTER TABLE ts1 MODIFY e datetime NOT NULL DEFAULT CURRENT_TIMESTAMP")
	if got, want := r15Warnings(t, c), "Warning|1265|Data truncated for column 'e' at row 1"; got != want {
		t.Errorf("warnings after datetime: %q, want %q", got, want)
	}
	year := time.Now().UTC().Format("2006")
	got := r15Rows(t, c, "SELECT a, b, e FROM ts1")
	parts := strings.Split(got, "|")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], year) || !strings.HasPrefix(parts[1], year) || parts[2] != "0000-00-00 00:00:00" {
		t.Errorf("rows: %q, want timestamps of %s and a zero datetime", got, year)
	}
}

// STRICT ではエラー 1138 で何も変えず、テーブルも接続も使え、ブランチのロックも残らない。TRADITIONAL は MySQL では
// STRICT_TRANS_TABLES と STRICT_ALL_TABLES を含む。
func TestReview15StrictModes(t *testing.T) {
	db, c, stop := r15Server(t)
	defer stop()
	other := r15Conn(t, db)
	defer other.Close()
	for _, mode := range []string{"STRICT_TRANS_TABLES", "STRICT_ALL_TABLES", "STRICT_TRANS_TABLES,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION", "TRADITIONAL"} {
		exec(t, c, wpSQLMode)
		exec(t, c, "DROP TABLE IF EXISTS nn2")
		exec(t, c, "CREATE TABLE nn2 (id int NOT NULL PRIMARY KEY, s varchar(20) NULL)")
		exec(t, c, "INSERT INTO nn2 VALUES (1, NULL), (2, 'v')")
		exec(t, c, "SET SESSION sql_mode = '"+mode+"'")
		if err := r15Err(c, "ALTER TABLE nn2 MODIFY s varchar(20) NOT NULL"); mysqlErrNo(err) != 1138 {
			t.Errorf("sql_mode %s: ALTER: %v, want error 1138", mode, err)
		}
		if got := r15Rows(t, c, "SELECT id, s FROM nn2 ORDER BY id"); got != "1|NULL\n2|v" {
			t.Errorf("sql_mode %s: rows after failed ALTER:\n%s", mode, got)
		}
		if got := r15Nullable(t, c, "nn2", "s"); got != "YES" {
			t.Errorf("sql_mode %s: Null = %s after failed ALTER", mode, got)
		}
		// 同じ接続も他の接続もすぐに書ける（DDL のロックが残っていない）。
		exec(t, c, "INSERT INTO nn2 VALUES (10, 'same')")
		exec(t, other, "INSERT INTO nn2 VALUES (11, 'other')")
		exec(t, c, "DELETE FROM nn2 WHERE id >= 10")
	}
	exec(t, c, wpSQLMode)
	exec(t, c, "ALTER TABLE nn2 MODIFY s varchar(20) NOT NULL")
	if got := r15Rows(t, c, "SELECT id, s FROM nn2 ORDER BY id"); got != "1|\n2|v" {
		t.Errorf("rows after non-strict ALTER:\n%s", got)
	}
}

// 書き直しが途中で失敗したら（一意キーの重複、ENUM の値の縮小）、行は NULL のままで、その後の読み取りも NULL を
// ” に変えない（印が残らない）。接続は切れない。
func TestReview15FailedRewriteLeavesNulls(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE t3 (id int NOT NULL PRIMARY KEY, u varchar(20) NULL, UNIQUE KEY ku (u))")
	exec(t, c, "INSERT INTO t3 VALUES (1, NULL), (2, NULL)")
	if err := r15Err(c, "ALTER TABLE t3 MODIFY u varchar(20) NOT NULL"); mysqlErrNo(err) != 1062 {
		t.Errorf("unique duplicates after fill: %v, want error 1062", err)
	}
	if got := r15Rows(t, c, "SELECT id, u FROM t3 ORDER BY id"); got != "1|NULL\n2|NULL" {
		t.Errorf("rows after failed ALTER:\n%s", got)
	}
	if got := r15Rows(t, c, "SELECT id FROM t3 WHERE u IS NULL ORDER BY id"); got != "1\n2" {
		t.Errorf("IS NULL after failed ALTER:\n%s", got)
	}
	if got := r15Nullable(t, c, "t3", "u"); got != "YES" {
		t.Errorf("Null = %s after failed ALTER", got)
	}

	// go-mysql-server は、新しい ENUM にない値があると DiscardChanges も Close も呼ばずにエラーを返す
	// （sql/rowexec/ddl_iters.go の rewriteTable）。MySQL（非 STRICT）は '' にして通すが、HeimDB はエラーでよい。
	// ただし印が残ってはいけない。
	exec(t, c, "CREATE TABLE en (id int NOT NULL PRIMARY KEY, e enum('a','b') NULL)")
	exec(t, c, "INSERT INTO en VALUES (1, NULL), (2, 'b')")
	err := r15Err(c, "ALTER TABLE en MODIFY e enum('a') NOT NULL")
	t.Logf("enum shrink: %v", err)
	if got := r15Rows(t, c, "SELECT id, e FROM en ORDER BY id"); err != nil && got != "1|NULL\n2|b" {
		t.Errorf("rows after failed enum ALTER:\n%s", got)
	}
	if got := r15Rows(t, c, "SELECT 1"); got != "1" {
		t.Errorf("connection unusable: %s", got)
	}
}

// 警告の行番号は主キーの順（主キーが先頭の列でなく、挿入の順と違っても）。
func TestReview15RowNumbersFollowPrimaryKey(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE t4 (a varchar(5) NULL, id int NOT NULL, PRIMARY KEY (id))")
	exec(t, c, "INSERT INTO t4 VALUES ('x', 5), (NULL, 3), (NULL, 9), ('y', 1), (NULL, 7)")
	exec(t, c, "ALTER TABLE t4 MODIFY a varchar(5) NOT NULL")
	want := "Warning|1265|Data truncated for column 'a' at row 2\nWarning|1265|Data truncated for column 'a' at row 4\nWarning|1265|Data truncated for column 'a' at row 5"
	if got := r15Warnings(t, c); got != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", got, want)
	}
}

// 実行中のトランザクションの未コミットの行も、暗黙のコミットの後に書き直される（MySQL と同じ）。
func TestReview15UncommittedRowsAreRewritten(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE tx (id int NOT NULL PRIMARY KEY, v varchar(10) NULL)")
	exec(t, c, "INSERT INTO tx VALUES (1, 'a'), (2, NULL)")
	exec(t, c, "BEGIN")
	exec(t, c, "INSERT INTO tx VALUES (3, NULL)")
	exec(t, c, "ALTER TABLE tx MODIFY v varchar(10) NOT NULL")
	want := "Warning|1265|Data truncated for column 'v' at row 2\nWarning|1265|Data truncated for column 'v' at row 3"
	if got := r15Warnings(t, c); got != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", got, want)
	}
	exec(t, c, "ROLLBACK")
	if got := r15Rows(t, c, "SELECT id, v FROM tx ORDER BY id"); got != "1|a\n2|\n3|" {
		t.Errorf("rows:\n%s", got)
	}
}

// 複数の操作がある ALTER: 列が見つからなければ全体が失敗して何も変わらない（MySQL は文全体を先に検証する）。
// 2 つの列を NOT NULL にすると、警告は出た順で、LIMIT は先頭から。
func TestReview15MultiClause(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "CREATE TABLE t6 (id int NOT NULL PRIMARY KEY, v varchar(20) NULL, w varchar(20) NULL)")
	exec(t, c, "INSERT INTO t6 VALUES (1, NULL, NULL)")
	// go-mysql-server は列がないことを実行前に見つける（番号は 1054 ではなく 1105。段階 15 の範囲外）。
	if err := r15Err(c, "ALTER TABLE t6 MODIFY v varchar(20) NOT NULL, MODIFY nosuch int"); err == nil {
		t.Errorf("unknown column in second clause: no error")
	}
	if got := r15Rows(t, c, "SELECT * FROM t6"); got != "1|NULL|NULL" {
		t.Errorf("rows after failed multi-clause ALTER: %s", got)
	}
	if got := r15Nullable(t, c, "t6", "v"); got != "YES" {
		t.Errorf("Null = %s after failed multi-clause ALTER", got)
	}
	exec(t, c, "ALTER TABLE t6 MODIFY v varchar(20) NOT NULL, MODIFY w varchar(20) NOT NULL")
	// go-mysql-server は @@warning_count に対応しない（段階 15 の範囲外）。
	want := "Warning|1265|Data truncated for column 'v' at row 1\nWarning|1265|Data truncated for column 'w' at row 1"
	if got := r15Warnings(t, c); got != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", got, want)
	}
	if got := r15Rows(t, c, "SHOW WARNINGS LIMIT 1"); got != "Warning|1265|Data truncated for column 'v' at row 1" {
		t.Errorf("SHOW WARNINGS LIMIT 1: %s", got)
	}
	if got := r15Rows(t, c, "SHOW WARNINGS LIMIT 1, 1"); got != "Warning|1265|Data truncated for column 'w' at row 1" {
		t.Errorf("SHOW WARNINGS LIMIT 1, 1: %s", got)
	}
	if got := r15Rows(t, c, "SELECT * FROM t6"); got != "1||" {
		t.Errorf("rows: %s", got)
	}
}

// 構文解析の段階で出る警告（DROP TABLE IF EXISTS の Note 1051 など）は、MySQL と同じく出た順に並ぶ。段階 15 から
// 段階 92 までは逆の順だった（engine.go が警告を新しい順の前提で写して入れ直していた。段階 93 で直した）。
func TestReview15PlanWarningsOrder(t *testing.T) {
	_, c, stop := r15Server(t)
	defer stop()
	exec(t, c, "DROP TABLE IF EXISTS zz1, zz2")
	if got, want := r15Warnings(t, c), "Note|1051|Unknown table 'zz1'\nNote|1051|Unknown table 'zz2'"; got != want {
		t.Errorf("warnings:\n%s\nwant:\n%s", got, want)
	}
}

// ブランチで dbDelta が列を NOT NULL にする: ブランチの行（親から引き継いだもの）は ” になり、親は NULL のまま、
// AS OF は NULL を見せる。マージすると親の NULL も ”（段階 13 の変換）。
func TestReview15BranchAlterAndMerge(t *testing.T) {
	mainDB, stagingDB, stop := colFixture(t)
	defer stop()
	m, s := r15Conn(t, mainDB), r15Conn(t, stagingDB)
	defer m.Close()
	defer s.Close()
	before := mark(t)
	exec(t, s, "ALTER TABLE wp_plug MODIFY note varchar(20) NOT NULL DEFAULT ''")
	if got, want := r15Warnings(t, s), "Warning|1265|Data truncated for column 'note' at row 1"; got != want {
		t.Errorf("warnings on branch: %q, want %q", got, want)
	}
	if got := r15Rows(t, s, "SELECT id, note FROM wp_plug ORDER BY id"); got != "1|\n2|n2" {
		t.Errorf("branch rows:\n%s", got)
	}
	if got := r15Rows(t, s, "SELECT id FROM wp_plug WHERE note = '' ORDER BY id"); got != "1" {
		t.Errorf("branch lookup by '': %s", got)
	}
	if got := r15Rows(t, s, "SELECT id, note FROM wp_plug AS OF '"+before+"' ORDER BY id"); got != "1|NULL\n2|n2" {
		t.Errorf("AS OF before ALTER:\n%s", got)
	}
	if got := r15Rows(t, m, "SELECT id, note FROM wp_plug ORDER BY id"); got != "1|NULL\n2|n2" {
		t.Errorf("main rows:\n%s", got)
	}
	exec(t, m, "INSERT INTO wp_plug (a, b, note) VALUES (7, 'prod', NULL)")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := r15Nullable(t, m, "wp_plug", "note"); got != "NO" {
		t.Errorf("main Null = %s after merge", got)
	}
	if got := r15Rows(t, m, "SELECT id, note FROM wp_plug ORDER BY id"); got != "1|\n2|n2\n3|" {
		t.Errorf("main rows after merge:\n%s", got)
	}
	if got := r15Rows(t, m, "SELECT id FROM wp_plug WHERE note IS NULL"); got != "" {
		t.Errorf("NULL rows after merge: %s", got)
	}
}
