package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// 非 STRICT の sql_mode での値の扱い（docs/plan.md の段階 21）。期待値は本物の MySQL 8.0.46 で確かめた。

const wordpressSQLMode = "SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"

// warnings は、直前の文の警告を「番号 文面」の行にして返す。
func warnings(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SHOW WARNINGS")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var level, msg string
		var code int
		if err := rows.Scan(&level, &code, &msg); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s", code, msg))
	}
	return out
}

// execWarn は、q を実行し、警告が want と同じかを確かめる。
func execWarn(t *testing.T, db *sql.DB, q string, want ...string) {
	t.Helper()
	mustExec(t, db, q)
	if got := warnings(t, db); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: warnings\n%s\nwant\n%s", q, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// rowsText は、q の結果を、列を「|」、行を「,」でつないだ文字列にする（CONCAT はゼロの日時と ENUM を MySQL と違う
// 形にするので、列をそのまま読む）。
func rowsText(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	return joinCells(queryCells(t, db, q, ""), "|", ",")
}

// querier は、*sql.DB と *sql.Conn。
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryCells は、q の結果の各行の値を文字列で返す。NULL は null にする。
func queryCells(t *testing.T, db querier, q, null string) [][]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = v.String
			if !v.Valid {
				cells[i] = null
			}
		}
		out = append(out, cells)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

// joinCells は、行の中を sep、行の間を rowSep でつなぐ。
func joinCells(rows [][]string, sep, rowSep string) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r, sep)
	}
	return strings.Join(out, rowSep)
}

func nonStrictServer(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db, _, stop := branchServer(t)
	db.SetMaxOpenConns(1) // sql_mode と警告は接続ごと
	mustExec(t, db, wordpressSQLMode)
	mustExec(t, db, "CREATE TABLE v21 (id int NOT NULL PRIMARY KEY, s varchar(10) NOT NULL DEFAULT 'x', n int NOT NULL DEFAULT 5, d datetime NOT NULL DEFAULT '2000-01-01 00:00:00', dt date NOT NULL DEFAULT '2000-01-01', ts timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP, e enum('a','b') NOT NULL DEFAULT 'b', f double NOT NULL DEFAULT 1)")
	mustExec(t, db, "INSERT INTO v21 (id) VALUES (1), (2)")
	return db, stop
}

// NOT NULL の列への NULL: UPDATE、複数行の INSERT、INSERT ... SELECT、REPLACE は型の既定値と警告 1048。
// 単一行の INSERT ... VALUES は MySQL でもエラー 1048。
func TestNonStrictNullIntoNotNull(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	execWarn(t, db, "UPDATE v21 SET s = NULL, n = NULL, d = NULL, dt = NULL, ts = NULL, e = NULL, f = NULL WHERE id = 1",
		"1048 Column 's' cannot be null", "1048 Column 'n' cannot be null", "1048 Column 'd' cannot be null",
		"1048 Column 'dt' cannot be null", "1048 Column 'ts' cannot be null", "1048 Column 'e' cannot be null",
		"1048 Column 'f' cannot be null")
	if got := rowsText(t, db, "SELECT s, n, d, dt, ts, e, f FROM v21 WHERE id = 1"); got != "|0|0000-00-00 00:00:00|0000-00-00|0000-00-00 00:00:00||0" {
		t.Errorf("after UPDATE: %s", got)
	}
	execWarn(t, db, "INSERT INTO v21 (id, s) VALUES (3, NULL), (4, 'q')", "1048 Column 's' cannot be null")
	execWarn(t, db, "INSERT INTO v21 (id, s) SELECT 5, NULL", "1048 Column 's' cannot be null")
	execWarn(t, db, "REPLACE INTO v21 (id, s) VALUES (6, NULL), (7, 'q')", "1048 Column 's' cannot be null")
	if got := rowsText(t, db, "SELECT id, s FROM v21 WHERE id >= 3 ORDER BY id"); got != "3|,4|q,5|,6|,7|q" {
		t.Errorf("after INSERT: %s", got)
	}
	if _, err := db.Exec("INSERT INTO v21 (id, s) VALUES (8, NULL)"); mysqlErrNo(err) != 1048 {
		t.Errorf("single-row INSERT of NULL: %v, want error 1048", err)
	}
	// AUTO_INCREMENT の列の NULL は採番（直さない）。
	mustExec(t, db, "CREATE TABLE ai (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v int NOT NULL DEFAULT 0)")
	execWarn(t, db, "INSERT INTO ai (id, v) VALUES (NULL, 1), (NULL, 2)")
	if got := column(t, db, "SELECT id FROM ai ORDER BY id"); got != "1,2" {
		t.Errorf("auto increment: %s", got)
	}
}

// 省いた、既定値のない NOT NULL の列は、単一行でも型の既定値と警告 1364。
func TestNonStrictOmittedColumnWithoutDefault(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	mustExec(t, db, "CREATE TABLE md (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a longtext NOT NULL, b int NOT NULL, c datetime NOT NULL, t varchar(20) NOT NULL DEFAULT 'x')")
	execWarn(t, db, "INSERT INTO md (id) VALUES (1)",
		"1364 Field 'a' doesn't have a default value", "1364 Field 'b' doesn't have a default value", "1364 Field 'c' doesn't have a default value")
	// 複数行でも、警告は列ごとに文で 1 回。
	execWarn(t, db, "INSERT INTO md (b) VALUES (3), (4)",
		"1364 Field 'a' doesn't have a default value", "1364 Field 'c' doesn't have a default value")
	if got := rowsText(t, db, "SELECT id, a, b, c, t FROM md ORDER BY id"); got != "1||0|0000-00-00 00:00:00|x,2||3|0000-00-00 00:00:00|x,3||4|0000-00-00 00:00:00|x" {
		t.Errorf("rows: %s", got)
	}
}

// 不正な日時はゼロの日時と警告 1265。時刻が途中までの形は正しい値（警告なし）。
func TestNonStrictInvalidDatetime(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	execWarn(t, db, "INSERT INTO v21 (id, d, dt) VALUES (3, '2012-01-01 0', '2012-13-01'), (4, '2016-01-16T00:00:00+0100', 'abc')",
		"1265 Data truncated for column 'dt' at row 1", "1265 Data truncated for column 'd' at row 2", "1265 Data truncated for column 'dt' at row 2")
	execWarn(t, db, "UPDATE v21 SET d = '2020-12-41 14:15:27' WHERE id = 2", "1265 Data truncated for column 'd' at row 1")
	execWarn(t, db, "INSERT INTO v21 (id, d) VALUES (5, '')", "1265 Data truncated for column 'd' at row 1")
	execWarn(t, db, "INSERT INTO v21 (id, d) VALUES (6, '2012-01-01 25:00:00')", "1265 Data truncated for column 'd' at row 1")
	execWarn(t, db, "INSERT INTO v21 (id, d) VALUES (7, '2012-1-2T3:4'), (8, '2012-02-30 00:00:00'), (9, '2012-01-01 00:60:00')",
		"1265 Data truncated for column 'd' at row 2", "1265 Data truncated for column 'd' at row 3")
	if got := rowsText(t, db, "SELECT id, d, dt FROM v21 WHERE id >= 2 ORDER BY id"); got != strings.Join([]string{
		"2|0000-00-00 00:00:00|2000-01-01",
		"3|2012-01-01 00:00:00|0000-00-00",
		"4|0000-00-00 00:00:00|0000-00-00",
		"5|0000-00-00 00:00:00|2000-01-01",
		"6|0000-00-00 00:00:00|2000-01-01",
		"7|2012-01-02 03:04:00|2000-01-01",
		"8|0000-00-00 00:00:00|2000-01-01",
		"9|0000-00-00 00:00:00|2000-01-01",
	}, ",") {
		t.Errorf("rows: %s", got)
	}
}

// STRICT の sql_mode では、どれも今までどおりエラー（INSERT IGNORE と UPDATE IGNORE は、段階 92 から非 STRICT と同じに直す）。
func TestNonStrictKeepsStrictErrors(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	for _, mode := range []string{"STRICT_TRANS_TABLES", "STRICT_ALL_TABLES", "TRADITIONAL"} {
		mustExec(t, db, "SET SESSION sql_mode = '"+mode+"'")
		for _, q := range []string{
			"UPDATE v21 SET s = NULL WHERE id = 2",
			"INSERT INTO v21 (id, s) VALUES (12, NULL), (13, 'q')",
			"INSERT INTO v21 (id, d) VALUES (12, '2012-01-01 25:00:00'), (13, '2012-01-01')",
			"UPDATE v21 SET d = '2020-12-41 14:15:27' WHERE id = 2",
		} {
			if _, err := db.Exec(q); err == nil {
				t.Errorf("%s: %s succeeded", mode, q)
			}
		}
	}
	if got := one(t, db, "SELECT COUNT(*) FROM v21"); got != "2" {
		t.Errorf("rows were inserted in strict mode: %s", got)
	}
	mustExec(t, db, wordpressSQLMode)
	// 重複キーは非 STRICT でもエラー（INSERT IGNORE の扱いにしていない）。
	if _, err := db.Exec("INSERT INTO v21 (id, s) VALUES (1, NULL), (2, 'q')"); mysqlErrNo(err) != 1062 {
		t.Errorf("duplicate key: %v, want error 1062", err)
	}
}
