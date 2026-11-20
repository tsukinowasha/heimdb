package backend

import (
	"strconv"
	"strings"
	"testing"
)

// 段階 21（非 STRICT の sql_mode での値の扱い）のレビュー。期待値は本物の MySQL 8.0.46 で確かめた。

// ---- 1. INSERT ... ON DUPLICATE KEY UPDATE の更新式 ----

// ON DUPLICATE KEY UPDATE の右辺は nonStrictValue で包まれない。MySQL（非 STRICT）では、複数行の INSERT の
// ON DUPLICATE KEY UPDATE s = NULL は警告 1048 と ”、不正な日時は（単一行でも）警告 1265 とゼロの日時。
// 単一行の INSERT の ON DUPLICATE KEY UPDATE s = NULL は MySQL でもエラー 1048。
func TestReview21OnDuplicateKeyUpdateExprs(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	// 単一行 + 不正な日時: MySQL は警告 1265 とゼロの日時。
	q := "INSERT INTO v21 (id, d) VALUES (1, '2001-01-01 00:00:00') ON DUPLICATE KEY UPDATE d = '2012-01-01 25:00:00'"
	if _, err := db.Exec(q); err != nil {
		t.Errorf("%s: %v, want warning 1265 and zero datetime (MySQL)", q, err)
	} else if got := warnings(t, db); strings.Join(got, "\n") != "1265 Data truncated for column 'd' at row 1" {
		t.Errorf("ODKU bad datetime warnings: %q", got)
	}
	if got := one(t, db, "SELECT d FROM v21 WHERE id = 1"); got != "0000-00-00 00:00:00" {
		t.Errorf("ODKU bad datetime: d = %q, want zero datetime", got)
	}
	// 複数行 + NULL: MySQL は警告 1048 と ''（既存の 2 行とも）。
	q = "INSERT INTO v21 (id, s) VALUES (1, 'a'), (2, 'b') ON DUPLICATE KEY UPDATE s = NULL"
	if _, err := db.Exec(q); err != nil {
		t.Errorf("%s: %v, want warning 1048 and '' (MySQL)", q, err)
	} else if got := warnings(t, db); strings.Join(got, "\n") != "1048 Column 's' cannot be null" {
		t.Errorf("ODKU NULL warnings: %q", got)
	}
	if got := rowsText(t, db, "SELECT id, s FROM v21 ORDER BY id"); got != "1|,2|" {
		t.Errorf("ODKU NULL: %q, want \"1|,2|\"", got)
	}
	// NOT NULL の列に NULL が書き込まれていないこと（NULL が入っていれば MySQL と違い、後の読み出しで NULL が返る）。
	if got := one(t, db, "SELECT COUNT(*) FROM v21 WHERE s IS NULL"); got != "0" {
		t.Errorf("NULL was stored in a NOT NULL column: %s rows", got)
	}
	// ON DUPLICATE KEY UPDATE の VALUES(d) 経由の不正な日時は、挿入する行の元で直るので MySQL と同じ。
	execWarn(t, db, "INSERT INTO v21 (id, d) VALUES (1, '2012-13-01'), (2, '2012-13-01') ON DUPLICATE KEY UPDATE d = VALUES(d)",
		"1265 Data truncated for column 'd' at row 1", "1265 Data truncated for column 'd' at row 2")
	// 単一行 + NULL: MySQL でもエラー 1048。
	if _, err := db.Exec("INSERT INTO v21 (id, s) VALUES (1, 'a') ON DUPLICATE KEY UPDATE s = NULL"); mysqlErrNo(err) != 1048 {
		t.Errorf("single-row ODKU NULL: %v, want error 1048", err)
	}
	// STRICT でもエラー 1048（go-mysql-server は ON DUPLICATE KEY UPDATE の新しい行の NULL を確かめず、段階 21 より前から
	// NOT NULL の列に NULL を書き込む）。
	mustExec(t, db, "SET SESSION sql_mode = 'STRICT_TRANS_TABLES'")
	mustExec(t, db, "INSERT INTO v21 (id, n) VALUES (7, 1)")
	if _, err := db.Exec("INSERT INTO v21 (id, n) VALUES (7, 2) ON DUPLICATE KEY UPDATE n = NULL"); mysqlErrNo(err) != 1048 {
		t.Errorf("strict ODKU NULL: %v, want error 1048", err)
	}
	if got := one(t, db, "SELECT COUNT(*) FROM v21 WHERE n IS NULL"); got != "0" {
		t.Errorf("strict: NULL was stored in a NOT NULL column: %s rows", got)
	}
}

// ---- 2. プリペアドステートメント ----

// sql_mode は EXECUTE の時点のものが効く（MySQL で確認）。同じ文を繰り返し実行しても「at row N」は 1 から数える。
func TestReview21PreparedStatements(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	stmt, err := db.Prepare("UPDATE v21 SET s = ?, d = ? WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i := 0; i < 2; i++ {
		if _, err := stmt.Exec(nil, "2012-01-01 25:00:00", 1); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		if got := warnings(t, db); strings.Join(got, "\n") != "1048 Column 's' cannot be null\n1265 Data truncated for column 'd' at row 1" {
			t.Errorf("exec %d warnings: %q", i, got)
		}
	}
	mustExec(t, db, "SET SESSION sql_mode = 'STRICT_TRANS_TABLES'")
	if _, err := stmt.Exec(nil, "2012-01-01 25:00:00", 1); err == nil {
		t.Error("strict mode after PREPARE: succeeded, want error")
	}
	mustExec(t, db, wordpressSQLMode)
	// 複数行の INSERT をプリペアドで。
	ins, err := db.Prepare("INSERT INTO v21 (id, s, d) VALUES (?, ?, ?), (?, ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()
	if _, err := ins.Exec(10, nil, "2012-01-01 0", 11, "q", "bad"); err != nil {
		t.Fatal(err)
	}
	if got := warnings(t, db); strings.Join(got, "\n") != "1048 Column 's' cannot be null\n1265 Data truncated for column 'd' at row 2" {
		t.Errorf("prepared INSERT warnings: %q", got)
	}
	if got := rowsText(t, db, "SELECT id, s, d FROM v21 WHERE id >= 10 ORDER BY id"); got != "10||2012-01-01 00:00:00,11|q|0000-00-00 00:00:00" {
		t.Errorf("prepared INSERT rows: %q", got)
	}
}

// ---- 3. UPDATE の行番号と ORDER BY / LIMIT ----

func TestReview21UpdateRowNumbers(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	mustExec(t, db, "INSERT INTO v21 (id) VALUES (3), (4)")
	execWarn(t, db, "UPDATE v21 SET d = '2012-01-01 25:00:00', s = NULL WHERE id IN (2, 3)",
		"1265 Data truncated for column 'd' at row 1", "1048 Column 's' cannot be null",
		"1265 Data truncated for column 'd' at row 2", "1048 Column 's' cannot be null")
	execWarn(t, db, "UPDATE v21 SET d = 'abc' ORDER BY id DESC LIMIT 2",
		"1265 Data truncated for column 'd' at row 1", "1265 Data truncated for column 'd' at row 2")
	if got := column(t, db, "SELECT id FROM v21 WHERE d = '0000-00-00 00:00:00' ORDER BY id"); got != "2,3,4" {
		t.Errorf("rows with zero datetime: %s", got)
	}
	// 値が変わらなくても警告は出て、ROW_COUNT は 0（MySQL と同じ）。
	mustExec(t, db, "UPDATE v21 SET s = NULL WHERE id = 2")
	if got := warnings(t, db); strings.Join(got, "\n") != "1048 Column 's' cannot be null" {
		t.Errorf("no-change UPDATE warnings: %q", got)
	}
	if got := one(t, db, "SELECT ROW_COUNT()"); got != "0" {
		t.Errorf("no-change UPDATE ROW_COUNT() = %s, want 0", got)
	}
	// 右辺が NULL になる式（列やサブクエリ）も直す。
	execWarn(t, db, "UPDATE v21 SET s = (SELECT NULL), d = NULL WHERE id = 1",
		"1048 Column 's' cannot be null", "1048 Column 'd' cannot be null")
	// SET ... = DEFAULT（既定値が式のものも）は今までどおり。
	execWarn(t, db, "UPDATE v21 SET s = DEFAULT, d = DEFAULT, ts = DEFAULT WHERE id = 1")
	if got := rowsText(t, db, "SELECT s, d, ts = '0000-00-00 00:00:00' FROM v21 WHERE id = 1"); got != "x|2000-01-01 00:00:00|0" {
		t.Errorf("SET DEFAULT: %q", got)
	}
	// WHERE ... IN (SELECT ...) の形（go-mysql-server は Update Join にする）でも NULL は直る（MySQL: 警告 1048）。
	execWarn(t, db, "UPDATE v21 SET s = NULL WHERE id IN (SELECT 4)", "1048 Column 's' cannot be null")
	if got := one(t, db, "SELECT COUNT(*) FROM v21 WHERE s IS NULL"); got != "0" {
		t.Errorf("NULL stored by UPDATE ... IN (SELECT): %s rows", got)
	}
}

// ---- 4. 日時の文字列の細かい形 ----

// go-mysql-server が受け付ける形と、段階 21 の緩い読み取りが受け付ける形（MySQL で確認した値）。
func TestReview21DatetimeAcceptedForms(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	for i, c := range []struct{ in, want string }{
		{"2012-01-01T00:00:00+01:00", "2011-12-31 23:00:00"}, // 時差つきはセッションの時刻（UTC）に変換
		{"20120101", "2012-01-01 00:00:00"},
		{"2012/01/01", "2012-01-01 00:00:00"},
		{"0000-00-00 00:00:00", "0000-00-00 00:00:00"},
		{"2012-01-01 1:2:3.1234567", "2012-01-01 01:02:03"},
		{"2012-1-1", "2012-01-01 00:00:00"},
		{"2012-01-01 10", "2012-01-01 10:00:00"},
		{"2012-01-01 00:00:00.5", "2012-01-01 00:00:01"}, // MySQL は小数秒を丸める
	} {
		id := 100 + i
		mustExec(t, db, "INSERT INTO v21 (id, d) VALUES ("+strconv.Itoa(id)+", '"+c.in+"')")
		if got := warnings(t, db); len(got) != 0 {
			t.Errorf("%q: warnings %q, want none", c.in, got)
		}
		if got := one(t, db, "SELECT d FROM v21 WHERE id = "+strconv.Itoa(id)); got != c.want {
			t.Errorf("%q: d = %q, want %q", c.in, got, c.want)
		}
	}
	// ゼロの日時と警告 1265 になるもの。
	for i, in := range []string{"2012-01-01 103", "2012-01-01 24:00:00", "2012-01-01 00:00:60", "2012-01-01T00:00:00+0100"} {
		id := 200 + i
		execWarn(t, db, "INSERT INTO v21 (id, d) VALUES ("+strconv.Itoa(id)+", '"+in+"')", "1265 Data truncated for column 'd' at row 1")
		if got := one(t, db, "SELECT d FROM v21 WHERE id = "+strconv.Itoa(id)); got != "0000-00-00 00:00:00" {
			t.Errorf("%q: d = %q, want zero datetime", in, got)
		}
	}
	// DATE の列。
	execWarn(t, db, "INSERT INTO v21 (id, dt) VALUES (300, '2012-01-01 25:00:00'), (301, '2012-01-01 0')", "1265 Data truncated for column 'dt' at row 1")
	if got := rowsText(t, db, "SELECT id, dt FROM v21 WHERE id >= 300 ORDER BY id"); got != "300|0000-00-00,301|2012-01-01" {
		t.Errorf("DATE rows: %q", got)
	}
}

// 範囲外の TIMESTAMP（'1900-01-01 00:00:00'）は、MySQL（非 STRICT）では警告 1264 とゼロの日時。
func TestReview21TimestampOutOfRange(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	if _, err := db.Exec("INSERT INTO v21 (id, ts) VALUES (3, '1900-01-01 00:00:00')"); err != nil {
		t.Fatalf("out-of-range TIMESTAMP in non-strict mode: %v, want warning 1264 and zero (MySQL)", err)
	}
	if got := warnings(t, db); strings.Join(got, "\n") != "1264 Out of range value for column 'ts' at row 1" {
		t.Errorf("warnings: %q", got)
	}
	if got := one(t, db, "SELECT ts FROM v21 WHERE id = 3"); got != "0000-00-00 00:00:00" {
		t.Errorf("ts = %q, want zero", got)
	}
	execWarn(t, db, "UPDATE v21 SET ts = '2012-01-01 25:00:00' WHERE id = 1", "1265 Data truncated for column 'ts' at row 1")
	if got := one(t, db, "SELECT ts FROM v21 WHERE id = 1"); got != "0000-00-00 00:00:00" {
		t.Errorf("ts after UPDATE = %q, want zero", got)
	}
}

// ---- 5. 列の型ごとの暗黙の既定値 ----

// 複数行の INSERT（VALUES、SELECT、REPLACE）では、MySQL は警告 1048 と 1364 を列ごとに文で 1 回だけ出す（行ごとではない。
// UPDATE は行ごと）。直した。同じ行の中の警告の順は、MySQL は INSERT の列の並びの順、HeimDB はテーブルの列の順
// （記録だけ。下の (id, d, s) は MySQL では 1265 d、1048 s の順）。
func TestReview21InsertWarningsOncePerColumn(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	execWarn(t, db, "INSERT INTO v21 (id, s) VALUES (3, NULL), (4, NULL)", "1048 Column 's' cannot be null")
	execWarn(t, db, "INSERT INTO v21 (id, s) SELECT id + 10, NULL FROM v21 WHERE id <= 2", "1048 Column 's' cannot be null")
	execWarn(t, db, "REPLACE INTO v21 (id, s) VALUES (5, NULL), (6, NULL)", "1048 Column 's' cannot be null")
	mustExec(t, db, "CREATE TABLE md (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a longtext NOT NULL, b int NOT NULL, c datetime NOT NULL)")
	execWarn(t, db, "INSERT INTO md (b) VALUES (3), (4)", "1364 Field 'a' doesn't have a default value", "1364 Field 'c' doesn't have a default value")
	execWarn(t, db, "INSERT INTO md (a, b) VALUES (NULL, 1), ('x', 2), (NULL, 3)", "1048 Column 'a' cannot be null", "1364 Field 'c' doesn't have a default value")
	// 行の中の順: 1265 は行ごとに、1048 は 1 回だけ。行の中は INSERT に書いた列の順（MySQL 8.0.46 で確かめた。段階 92
	// より前は表の列の順で、期待も誤っていた）。
	execWarn(t, db, "INSERT INTO v21 (id, d, s) VALUES (32, 'bad', NULL), (33, 'bad', 'q')",
		"1265 Data truncated for column 'd' at row 1", "1048 Column 's' cannot be null", "1265 Data truncated for column 'd' at row 2")
	// UPDATE は行ごと。
	execWarn(t, db, "UPDATE v21 SET s = NULL WHERE id IN (1, 2)", "1048 Column 's' cannot be null", "1048 Column 's' cannot be null")
}

// 他の型の暗黙の既定値（MySQL で確認: text/blob は ”、decimal は 0.00、year は 0000、time は 00:00:00、bit は 0）。
func TestReview21OtherTypesZero(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	mustExec(t, db, "CREATE TABLE ot (id int NOT NULL PRIMARY KEY, b blob NOT NULL, t longtext NOT NULL, dc decimal(5,2) NOT NULL, y year NOT NULL, tm time NOT NULL, bi bit(3) NOT NULL, u bigint unsigned NOT NULL)")
	execWarn(t, db, "INSERT INTO ot (id) VALUES (1)",
		"1364 Field 'b' doesn't have a default value", "1364 Field 't' doesn't have a default value",
		"1364 Field 'dc' doesn't have a default value", "1364 Field 'y' doesn't have a default value",
		"1364 Field 'tm' doesn't have a default value", "1364 Field 'bi' doesn't have a default value",
		"1364 Field 'u' doesn't have a default value")
	execWarn(t, db, "INSERT INTO ot VALUES (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL), (3, 'b', 't', 1, 2001, '01:02:03', 1, 1)",
		"1048 Column 'b' cannot be null", "1048 Column 't' cannot be null", "1048 Column 'dc' cannot be null",
		"1048 Column 'y' cannot be null", "1048 Column 'tm' cannot be null", "1048 Column 'bi' cannot be null",
		"1048 Column 'u' cannot be null")
	if got := rowsText(t, db, "SELECT id, b, t, dc, y, tm, bi+0, u FROM ot ORDER BY id"); got != "1|||0.00|0|00:00:00|0|0,2|||0.00|0|00:00:00|0|0,3|b|t|1.00|2001|01:02:03|1|1" {
		t.Errorf("rows: %q", got)
	}
	execWarn(t, db, "UPDATE ot SET b = NULL, dc = NULL, y = NULL, tm = NULL, bi = NULL, u = NULL WHERE id = 3",
		"1048 Column 'b' cannot be null", "1048 Column 'dc' cannot be null", "1048 Column 'y' cannot be null",
		"1048 Column 'tm' cannot be null", "1048 Column 'bi' cannot be null", "1048 Column 'u' cannot be null")
	if got := rowsText(t, db, "SELECT b, dc, y, tm, bi+0, u FROM ot WHERE id = 3"); got != "|0.00|0|00:00:00|0|0" {
		t.Errorf("after UPDATE: %q", got)
	}
}

// ---- 6. 他の機能との組み合わせ ----

func TestReview21Interactions(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	// トランザクションの ROLLBACK で、直した値の行も消える。
	mustExec(t, db, "START TRANSACTION")
	execWarn(t, db, "INSERT INTO v21 (id, s) VALUES (3, NULL), (4, NULL)", "1048 Column 's' cannot be null")
	mustExec(t, db, "ROLLBACK")
	if got := one(t, db, "SELECT COUNT(*) FROM v21"); got != "2" {
		t.Errorf("after ROLLBACK: %s rows", got)
	}
	// INSERT IGNORE / UPDATE IGNORE は go-mysql-server の扱いのまま（警告 1048 の文面は go-mysql-server のもの）。
	mustExec(t, db, "INSERT IGNORE INTO v21 (id, s) VALUES (5, NULL)")
	if got := warnings(t, db); len(got) != 1 || !strings.HasPrefix(got[0], "1048 ") {
		t.Errorf("INSERT IGNORE warnings: %q", got)
	}
	if got := one(t, db, "SELECT s FROM v21 WHERE id = 5"); got != "" {
		t.Errorf("INSERT IGNORE: s = %q", got)
	}
	mustExec(t, db, "UPDATE IGNORE v21 SET s = NULL WHERE id = 2")
	if got := warnings(t, db); len(got) != 1 || !strings.HasPrefix(got[0], "1048 ") {
		t.Errorf("UPDATE IGNORE warnings: %q", got)
	}
	// AUTO_INCREMENT: LAST_INSERT_ID と ROW_COUNT は、行の元を包んでも変わらない。
	mustExec(t, db, "CREATE TABLE ai (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(5) NOT NULL, w int NOT NULL)")
	for _, c := range []struct{ q, lastID string }{
		{"INSERT INTO ai (v) VALUES (NULL), ('b')", "1"},
		{"INSERT INTO ai (id, v, w) VALUES (NULL, NULL, 1), (NULL, 'd', 2)", "3"},
	} {
		res, err := db.Exec(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if n, _ := res.RowsAffected(); n != 2 {
			t.Errorf("%s: affected %d, want 2", c.q, n)
		}
		if got := one(t, db, "SELECT LAST_INSERT_ID()"); got != c.lastID {
			t.Errorf("%s: LAST_INSERT_ID() = %s, want %s", c.q, got, c.lastID)
		}
	}
	if got := rowsText(t, db, "SELECT id, v, w FROM ai ORDER BY id"); got != "1||0,2|b|0,3||1,4|d|2" {
		t.Errorf("ai rows: %q", got)
	}
	// 段階 20 の大文字と小文字だけの変更と、NULL の置き換えを同じ UPDATE で。
	mustExec(t, db, "CREATE TABLE ci (id int NOT NULL PRIMARY KEY, e varchar(100) NOT NULL DEFAULT '', n int NOT NULL DEFAULT 0) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	mustExec(t, db, "INSERT INTO ci VALUES (1, 'editor@example.com', 1)")
	execWarn(t, db, "UPDATE ci SET e = 'Editor@example.com', n = NULL WHERE id = 1", "1048 Column 'n' cannot be null")
	if got := rowsText(t, db, "SELECT e, n FROM ci"); got != "Editor@example.com|0" {
		t.Errorf("case change + NULL: %q", got)
	}
	// 一時テーブル。
	mustExec(t, db, "CREATE TEMPORARY TABLE tmp21 (id int NOT NULL PRIMARY KEY, s varchar(5) NOT NULL)")
	execWarn(t, db, "INSERT INTO tmp21 VALUES (1, NULL), (2, 'a')", "1048 Column 's' cannot be null")
	execWarn(t, db, "UPDATE tmp21 SET s = NULL WHERE id = 2", "1048 Column 's' cannot be null")
	if got := rowsText(t, db, "SELECT id, s FROM tmp21 ORDER BY id"); got != "1|,2|" {
		t.Errorf("temporary table: %q", got)
	}
	// 生成列は HeimDB が非対応（明示的なエラー）なので、SET の右辺を包んでも go-mysql-server の生成列の 2 回目の走査
	// （rel_iters.go の defaultValFromSetExpression）には影響しない。
	if _, err := db.Exec("CREATE TABLE gen (id int NOT NULL PRIMARY KEY, b int NOT NULL, c int AS (b + 1) STORED)"); err == nil {
		t.Error("generated column was accepted; UPDATE's second pass for generated columns needs checking with nonStrictValue")
	}
	// EXPLAIN は書き換えの対象外でも、エラーにも panic にもならない。
	for _, q := range []string{"EXPLAIN INSERT INTO v21 (id, s) VALUES (8, NULL), (9, NULL)", "EXPLAIN UPDATE v21 SET s = NULL WHERE id = 1"} {
		if rows, err := db.Query(q); err != nil {
			t.Errorf("%s: %v", q, err)
		} else {
			rows.Close()
		}
	}
	// INSERT ... SELECT（同じテーブルから、複数の列）。
	execWarn(t, db, "INSERT INTO v21 (id, s, d) SELECT id + 20, NULL, 'bad' FROM v21 WHERE id <= 2 ORDER BY id",
		"1048 Column 's' cannot be null", "1265 Data truncated for column 'd' at row 1",
		"1265 Data truncated for column 'd' at row 2")
	if got := rowsText(t, db, "SELECT id, s, d FROM v21 WHERE id >= 21 ORDER BY id"); got != "21||0000-00-00 00:00:00,22||0000-00-00 00:00:00" {
		t.Errorf("INSERT SELECT: %q", got)
	}
}

// ブランチと AS OF: ブランチでの非 STRICT の書き込みは親に影響せず、AS OF で前の値が読める。
func TestReview21Branches(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mainDB.SetMaxOpenConns(1)
	mustExec(t, mainDB, "CREATE TABLE b21 (id int NOT NULL PRIMARY KEY, s varchar(10) NOT NULL, d datetime NOT NULL)")
	mustExec(t, mainDB, "INSERT INTO b21 VALUES (1, 'a', '2020-01-01 00:00:00')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('st')")
	st := openBranch(t, dsn, "st")
	defer st.Close()
	st.SetMaxOpenConns(1)
	mustExec(t, st, wordpressSQLMode)
	execWarn(t, st, "UPDATE b21 SET s = NULL, d = 'x' WHERE id = 1", "1048 Column 's' cannot be null", "1265 Data truncated for column 'd' at row 1")
	execWarn(t, st, "INSERT INTO b21 (id) VALUES (2)", "1364 Field 's' doesn't have a default value", "1364 Field 'd' doesn't have a default value")
	if got := rowsText(t, st, "SELECT id, s, d FROM b21 ORDER BY id"); got != "1||0000-00-00 00:00:00,2||0000-00-00 00:00:00" {
		t.Errorf("branch rows: %q", got)
	}
	if got := rowsText(t, mainDB, "SELECT id, s, d FROM b21 ORDER BY id"); got != "1|a|2020-01-01 00:00:00" {
		t.Errorf("main rows: %q", got)
	}
}

// 単一行の INSERT で NULL と不正な日時が同時にあると、MySQL はエラー 1048（行は入らない）。
func TestReview21SingleRowNullAndBadDate(t *testing.T) {
	db, stop := nonStrictServer(t)
	defer stop()
	if _, err := db.Exec("INSERT INTO v21 (id, s, d) VALUES (3, NULL, 'bad')"); mysqlErrNo(err) != 1048 {
		t.Errorf("err = %v, want 1048", err)
	}
	if got := one(t, db, "SELECT COUNT(*) FROM v21 WHERE id = 3"); got != "0" {
		t.Errorf("row was inserted")
	}
	// INSERT ... SET: MySQL は INSERT ... SET s = NULL をエラー 1048。
	if _, err := db.Exec("INSERT INTO v21 SET id = 3, s = NULL"); mysqlErrNo(err) != 1048 {
		t.Errorf("INSERT SET NULL: %v, want 1048", err)
	}
}
