package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// stage88Script は、DOUBLE と FLOAT の文字列の形、CAST(... AS DECIMAL) の桁と範囲の外、文字列の列と数の IN と等号
// （docs/plan.md の段階 88）。
func stage88Script() []string {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	return append(stmts,
		// DOUBLE の形（結果の行）
		"SELECT 20250331e0, 1234567e0, 12345678.9e0, 1e14, 1e15, 999999999999999.9e0, 1e15 + 0.5e0, 1.234567890123456e15, 1.2345678901234568e17",
		"SELECT 1e-14, 1e-15, 1e-16, 1.5e-16, 0.00000015e0, 0.00001e0, 1e100, -1e15, -0.00000015e0, -1*0e0, 0.1e0",
		// CONCAT と CAST(... AS CHAR) の形（22 文字まで）
		"SELECT CONCAT(1e15), CONCAT(20250331e0), CONCAT(1.2345678901234567e-5), CAST(1e100 AS CHAR), CAST(0.00000015e0 AS CHAR), CONCAT(-1*0e0)",
		// FLOAT の列（6 桁）と、集計
		"CREATE TABLE f88 (id int PRIMARY KEY, f float, d double)",
		"INSERT INTO f88 VALUES (1, 20250331, 20250331), (2, 12345678.9, 12345678.9), (3, 3.1415927, 3.1415927), (4, 33.333332, 0.00000015), (5, 1234567, 1e15)",
		"SELECT id, f, d FROM f88 ORDER BY id",
		"SELECT SUM(d), AVG(f), MAX(d) / 3 FROM f88",
		"SELECT CONCAT(f), CAST(f AS CHAR) FROM f88 ORDER BY id",
		// CAST(... AS DECIMAL): 桁の既定（10,0）、DECIMAL(M)、範囲の外（端の値と警告 1264。名前は別名か書いたとおりの式）
		"SELECT CAST(1.5 AS DECIMAL), CAST('12.345' AS DECIMAL), CAST(2.5 AS DECIMAL(5)), CAST(-2.5 AS DECIMAL(5))",
		"SELECT CAST('12345678901' AS DECIMAL), CAST(123456.78 AS DECIMAL(5,1)) a, CAST(-123456.78 AS DECIMAL(5,1)), CAST(9999.95 AS DECIMAL(5,1)), CAST(9999.94 AS DECIMAL(5,1)), CAST(-0.04 AS DECIMAL(5,1)), CAST(1e20 AS DECIMAL(10,2)), CAST('123456789.999' AS DECIMAL(10,2))",
		"SHOW WARNINGS",
		"SELECT 1 FROM dual WHERE CAST(123456 AS DECIMAL(3)) > 0",
		"SHOW WARNINGS",
		"SELECT CAST('12abc' AS DECIMAL(5,1))",
		"SHOW WARNINGS",
		// 数の後ろに文字の続く大きな値は 1292 と 1264 の両方、とても大きな値も端の値
		"SELECT CAST('123456789abc' AS DECIMAL(5,1)) a, CAST(1e40 AS DECIMAL(10,2)) b, CAST('-1000000000000000000000000000000000000000' AS DECIMAL(10,2)) c",
		"SHOW WARNINGS",
		// 範囲の外の桁はエラー
		"SELECT CAST(1 AS DECIMAL(300))",
		"SELECT CAST(1 AS DECIMAL(66))",
		"SELECT CAST(1 AS DECIMAL(10,31))",
		"SELECT CAST(1 AS DECIMAL(65,31))",
		"SELECT CAST(1 AS DECIMAL(65,30))",
		// 文字列の型の式の中の DOUBLE（前は符号が消え、ほかの値の文字が混ざった）
		"SELECT IF(1, -1.5e0, 'a') a, 'x' b, IF(1, 1e15, 'a') c, IF(1, -0.00000015e0, 'a') d",

		// 文字列の列（索引のあるものとないもの）と数の IN と等号
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '12abc', '12abc'), (1, '12', '12'), (1, '9', '9'), (1, ' 12 ', ' 12 '), (1, 'abc', 'abc'), (1, '9.0', '9.0'), (1, '', ''), (1, '0x0C', '0x0C'), (1, '  9abc', '  9abc')",
		"SELECT meta_id FROM wp_postmeta WHERE meta_key = 12 ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_key IN (12, 9) ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_key IN (12, '9') ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_key IN ('12', '9') ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_key IN (0) ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_value IN (12, 9) ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_value IN (12, 9, 'abc') ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_value IN (12.0, 9) ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_value IN (0) ORDER BY meta_id",
		"SELECT meta_id FROM wp_postmeta WHERE meta_value NOT IN (12, 9) ORDER BY meta_id",
		"SELECT 'a' IN (0), '1.0' IN (1), '1.0' IN ('1', 2), '1.0' IN (1, '2'), '0x0C' IN (12, 9)",
		"SHOW WARNINGS",
		"SELECT '12abc' IN (12)",
		"SHOW WARNINGS",
		"SELECT meta_id FROM wp_postmeta WHERE meta_id IN ('1', '2abc') ORDER BY meta_id",
		// WordPress の meta_query の DECIMAL と NUMERIC
		"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (1, 1, '2024-01-01 00:00:01', 'c', 'A', '', 'publish', 'a', '', '', '', 'post')",
		"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (2, 1, '2024-01-01 00:00:02', 'c', 'B', '', 'publish', 'b', '', '', '', 'post')",
		"INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES (3, 1, '2024-01-01 00:00:03', 'c', 'C', '', 'publish', 'c', '', '', '', 'post')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'price', '10.4'), (2, 'price', '10.5'), (3, 'price', '12345678901')",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL) >= '11' ) ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC LIMIT 0, 10",
		"SELECT FOUND_ROWS()",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,1)) BETWEEN '10.45' AND '20' ) ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC LIMIT 0, 10",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'price' ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(wp_postmeta.meta_value AS DECIMAL) DESC, wp_posts.ID ASC LIMIT 0, 10",
		"SELECT post_id, CAST(meta_value AS DECIMAL), meta_value + 0 FROM wp_postmeta WHERE meta_key = 'price' ORDER BY post_id",
		// 索引の先頭の列を数で決める並べ替え（post_type = 0 は、数で始まらないすべての種類に当たる）
		"SELECT ID FROM wp_posts WHERE post_type = 0 AND post_status = 'publish' ORDER BY post_date DESC LIMIT 5",
	)
}

// 段階 88 の値と警告が、本物の MySQL と同じ。
func TestStage88MatchMySQL(t *testing.T) {
	checkGolden(t, "../testdata/stage88_golden.txt", "heimdb_golden88", stage88Script())
}

// 文字列の列と数の等号と IN は、索引の範囲にしない（数として比べるので、文字列にした 1 つの値に決まらない。MySQL も
// 全件を読む）。文字列どうしは今までどおり索引で読む。
func TestStage88TextColumnNumberPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		mustExec(t, db, q)
	}
	for _, c := range []struct {
		where    string
		indexRng bool
	}{
		{"meta_key = 12", false},
		{"meta_key IN (12, 9)", false},
		{"meta_key IN (12, '9')", false},
		{"meta_key = '12'", true},
		{"meta_key IN ('12', '9')", true},
	} {
		plan := explain(t, db, "SELECT meta_id FROM wp_postmeta WHERE "+c.where)
		if got := strings.Contains(plan, "index: [wp_postmeta.meta_key]"); got != c.indexRng {
			t.Errorf("%s: index range = %v, want %v:\n%s", c.where, got, c.indexRng, plan)
		}
	}
}

// CONCAT と CAST(... AS CHAR) で DOUBLE と FLOAT を文字列にするとき、MySQL と同じ形にする（期待値は MySQL 8.0.46）。
// 結果の行の DOUBLE と FLOAT の形は、Go の MySQL のドライバが数として読み直すので、ここでは見えない（エンジンの
// float_format_test.go と number_test.go で確かめる）。
func TestStage88DoubleText(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	texts := func(q string) string {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, " ")
	}
	for q, want := range map[string]string{
		"SELECT CONCAT_WS(' ', CONCAT(1e15), CONCAT(1.2345678901234567e-5), CAST(1e100 AS CHAR), CONCAT(20250331e0), CONCAT(0.00000015e0), CONCAT(-1*0e0))": "1e15 1.2345678901234568e-5 1e100 20250331 0.00000015 -0",
	} {
		if got := texts(q); got != want {
			t.Errorf("%s\n got: %s\nwant: %s", q, got, want)
		}
	}
	mustExec(t, db, "CREATE TABLE f88 (id int PRIMARY KEY, f float)")
	mustExec(t, db, "INSERT INTO f88 VALUES (1, 20250331), (2, 12345678.9), (3, 3.1415927), (4, 1234567)")
	if got, want := texts("SELECT CONCAT(f) FROM f88 ORDER BY id"), "20250300 12345700 3.14159 1234570"; got != want {
		t.Errorf("CONCAT(float) = %s, want %s", got, want)
	}
}
