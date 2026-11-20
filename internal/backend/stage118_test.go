package backend

import (
	"database/sql"
	"math"
	"testing"
)

// stage118Group は、段階 118 で直したプラグインで当たる値の違いの文（docs/plan.md の段階 118）。直す前の HeimDB は
// 本物の MySQL 8.0 と違う結果か誤りを返した。setup で表を作り、both を既定の sql_mode と WordPress の sql_mode の
// 両方で流し、stmts をそのあとに流し、drop で表を消す。
type stage118Group struct {
	name  string
	setup []string
	both  []string
	stmts []string
	drop  []string
}

var stage118Groups = []stage118Group{
	{
		name: "fromunixtime", // 1. FROM_UNIXTIME の数でない文字列（定数と列）は先頭の数の部分を DECIMAL として読み、警告 1292（前は 1366 で文全体が誤り）
		setup: []string{
			"CREATE TABLE fu (id INT PRIMARY KEY, v LONGTEXT, vc VARCHAR(50), t TEXT)",
			"INSERT INTO fu VALUES (1, '1700000000', '1700000000', '1700000000.5'), (2, '1700000000:1', '1700000000:1', '1700000000:1'), (3, 'abc', 'abc', ''), " +
				"(4, '1e3', '-5', '1e5x'), (5, NULL, '', NULL)",
			"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, " +
				"KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_edit_lock', '1704103200:1'), (2, '_edit_lock', '1704189600:2'), (3, '_edit_lock', 'x'), (1, '_schedule', '1704103200'), (2, '_schedule', '')",
		},
		both: []string{
			"SELECT FROM_UNIXTIME('abc')",
			"SHOW WARNINGS",
			"SELECT FROM_UNIXTIME('1e5x'), FROM_UNIXTIME('1e3'), FROM_UNIXTIME(''), FROM_UNIXTIME('-5'), FROM_UNIXTIME('abc', '%Y'), FROM_UNIXTIME(' 12'), FROM_UNIXTIME('12.5abc'), FROM_UNIXTIME('.5')",
			"SHOW WARNINGS",
			"SELECT FROM_UNIXTIME('1e'), FROM_UNIXTIME('1e+2'), FROM_UNIXTIME('+3'), FROM_UNIXTIME('0x10'), FROM_UNIXTIME('-abc'), FROM_UNIXTIME('-0'), FROM_UNIXTIME('1.9999999'), FROM_UNIXTIME('  ')",
			"SHOW WARNINGS",
			"SELECT FROM_UNIXTIME('1700000000.123456789'), FROM_UNIXTIME('  1700000000  '), FROM_UNIXTIME('1700000000x'), FROM_UNIXTIME('1.5e3'), FROM_UNIXTIME('99999999999'), FROM_UNIXTIME('1e20'), FROM_UNIXTIME('1e400')",
			"SELECT FROM_UNIXTIME('1700000000x', '%Y-%m-%d %H:%i:%s'), FROM_UNIXTIME('abc', '%Y'), FROM_UNIXTIME('-1', '%Y')",
			"SHOW WARNINGS",
			// 列（MySQL は列の正しい値の行にも 1366 Incorrect DECIMAL value: '0' を出す。MySQL の不具合なので警告は見ない）
			"SELECT id, FROM_UNIXTIME(v), FROM_UNIXTIME(vc), FROM_UNIXTIME(t), FROM_UNIXTIME(v, '%Y-%m-%d') FROM fu ORDER BY id",
			"SELECT id FROM fu WHERE FROM_UNIXTIME(v) > '2000-01-01' ORDER BY id",
			"SELECT COUNT(*), MAX(FROM_UNIXTIME(v)), MIN(FROM_UNIXTIME(t)) FROM fu",
			// プラグインの形（_edit_lock の値は「時刻:利用者」）
			"SELECT post_id, FROM_UNIXTIME(meta_value), DATE(FROM_UNIXTIME(meta_value)) FROM wp_postmeta WHERE meta_key = '_edit_lock' ORDER BY post_id",
			"SELECT post_id, FROM_UNIXTIME(meta_value, '%Y-%m-%d') FROM wp_postmeta WHERE meta_key IN ('_edit_lock', '_schedule') ORDER BY meta_id",
		},
		drop: []string{"DROP TABLE wp_postmeta", "DROP TABLE fu"},
	},
	{
		name: "textwarn", // 3. TEXT の列の参照を数にするとき（SUM、AVG、+、-、*、/、符号、数との比べ、WHERE v）は警告 1292 を出さない（VARCHAR と定数と式は出す）
		setup: []string{
			"CREATE TABLE tw (id INT PRIMARY KEY, v LONGTEXT, t TEXT, tt TINYTEXT, mt MEDIUMTEXT, vc VARCHAR(50))",
			"INSERT INTO tw VALUES (1, '1700000000', '1700000000', '1700000000', '1700000000', '1700000000'), (2, '1700000000:1', '1700000000:1', '1700000000:1', '1700000000:1', '17'), " +
				"(3, 'abc', 'abc', 'abc', 'abc', 'abc'), (4, '12abc', '12abc', '12abc', '12abc', '12abc'), (5, '1.0e400', '1.0e400', '1.0e400', '1.0e400', '3'), (6, '', ' 2 ', NULL, '0', '')",
			"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, " +
				"KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_edit_lock', '1704103200:1'), (2, '_edit_lock', '1704189600:2'), (1, '_price', '10.50'), (2, '_price', ''), (3, '_price', '9.99'), " +
				"(1, '_stock', '5'), (2, '_stock', 'n/a'), (3, '_stock', '0')",
		},
		both: []string{
			"SELECT SUM(v), AVG(v), SUM(t), AVG(tt), SUM(mt), COUNT(v) FROM tw",
			"SHOW WARNINGS",
			"SELECT SUM(v) FROM tw WHERE id <> 5",
			"SHOW WARNINGS",
			"SELECT id, SUM(t), AVG(v) FROM tw GROUP BY id ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, v + 0, t - 1, tt * 2, mt / 2, 1 + v, -v, -t FROM tw WHERE id <> 5 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, v + 0, -v FROM tw WHERE id = 5",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE v > 100 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE v = 12 OR t = 0 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE 12 < tt ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE t >= 1.5 AND mt <> 1e0 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, v > 100, t = 12, v < 1.5 FROM tw ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE v ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE t ORDER BY id",
			"SHOW WARNINGS",
			// VARCHAR の列、定数、式は今のまま警告を出す（'12:1' の形の VARCHAR は MySQL が出さない。docs/differences.md）
			"SELECT SUM(vc), AVG(vc) FROM tw",
			"SHOW WARNINGS",
			"SELECT id, vc + 0 FROM tw ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tw WHERE vc > 10 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT 'abc' + 0, '12abc' * 1, -'x'",
			"SHOW WARNINGS",
			"SELECT id, LOWER(v) + 0, CONCAT(t) + 0 FROM tw WHERE id IN (3, 4) ORDER BY id",
			"SHOW WARNINGS",
			// プラグインの形
			"SELECT SUM(meta_value) FROM wp_postmeta WHERE meta_key = '_edit_lock'",
			"SHOW WARNINGS",
			"SELECT post_id, meta_value + 0, meta_value * 1.1 FROM wp_postmeta WHERE meta_key = '_price' ORDER BY post_id",
			"SHOW WARNINGS",
			"SELECT post_id FROM wp_postmeta WHERE meta_key = '_stock' AND meta_value > 0 ORDER BY post_id",
			"SHOW WARNINGS",
			"SELECT post_id FROM wp_postmeta WHERE meta_key = '_stock' AND meta_value = 0 ORDER BY post_id",
			"SHOW WARNINGS",
			"SELECT SUM(meta_value), AVG(meta_value), MAX(meta_value + 0) FROM wp_postmeta WHERE meta_key IN ('_price', '_stock')",
			"SHOW WARNINGS",
		},
		drop: []string{"DROP TABLE wp_postmeta", "DROP TABLE tw"},
	},
	{
		name: "zerodate", // 4. ゼロの日付の TO_DAYS、LAST_DAY、EXTRACT(WEEK)、CONVERT_TZ、WEEK(NULL) と YEARWEEK(NULL)、ゼロの日付の警告の文
		setup: []string{
			wpSQLMode,
			"CREATE TABLE zd (id INT PRIMARY KEY, d DATE, dt DATETIME, s VARCHAR(30))",
			"INSERT INTO zd VALUES (1, '0000-00-00', '0000-00-00 00:00:00', '0000-00-00'), (2, '2024-02-10', '2024-02-10 10:00:00', '2024-02-10 10:00:00'), (3, NULL, NULL, NULL), " +
				"(4, '0000-00-00', '0000-00-00 00:00:00', '0000-00-00 00:00:00')",
			"CREATE TABLE wp_posts (ID BIGINT UNSIGNED PRIMARY KEY, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_date_gmt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_status VARCHAR(20) NOT NULL DEFAULT 'publish', post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0)",
			"INSERT INTO wp_posts VALUES (1, '2024-01-03 10:00:00', '2024-01-03 01:00:00', 'publish', 0), (2, '2024-01-10 10:00:00', '0000-00-00 00:00:00', 'draft', 0), (3, '2024-02-10 10:00:00', '2024-02-10 01:00:00', 'publish', 1)",
			"SET SESSION sql_mode = DEFAULT",
		},
		// WordPress の sql_mode（NO_ZERO_DATE がない）だけで流す。NO_ZERO_DATE のある既定の sql_mode では、MySQL はゼロの日付の
		// 文字列を不正な日付として NULL と警告にする（EXTRACT(YEAR FROM '0000-00-00') も NULL）が、HeimDB は前からゼロの日付として
		// 扱う（docs/differences.md）
		stmts: []string{
			wpSQLMode,
			"SELECT TO_DAYS('0000-00-00'), TO_DAYS('0000-00-00 00:00:00'), TO_DAYS(0), LAST_DAY('0000-00-00'), LAST_DAY('0000-00-00 10:00:00'), LAST_DAY(0), EXTRACT(WEEK FROM '0000-00-00')",
			"SHOW WARNINGS",
			"SELECT id, TO_DAYS(d), TO_DAYS(dt), TO_DAYS(s) FROM zd ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, LAST_DAY(d), LAST_DAY(dt), LAST_DAY(s) FROM zd ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, EXTRACT(WEEK FROM d), EXTRACT(WEEK FROM dt), EXTRACT(WEEK FROM s) FROM zd WHERE id <> 2 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, CONVERT_TZ(dt, '+00:00', '+09:00'), CONVERT_TZ(d, '+00:00', '+09:00') FROM zd ORDER BY id",
			"SHOW WARNINGS",
			"SELECT CONVERT_TZ('0000-00-00 00:00:00', '+00:00', '+09:00'), CONVERT_TZ(0, '+00:00', '+09:00'), CONVERT_TZ('2024-02-10 10:00:00', '+00:00', '+09:00')",
			"SHOW WARNINGS",
			"SELECT id, CONVERT_TZ(s, '+00:00', '+09:00') FROM zd WHERE id <> 2 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT WEEK(NULL), YEARWEEK(NULL), WEEK(NULL, 3), YEARWEEK(NULL, 1), WEEK('0000-00-00'), YEARWEEK('0000-00-00'), WEEK(0)",
			"SHOW WARNINGS",
			"SELECT id, WEEK(d), WEEK(dt, 3), YEARWEEK(d), YEARWEEK(dt, 1) FROM zd ORDER BY id",
			"SHOW WARNINGS",
			"SELECT DAYOFWEEK('0000-00-00'), WEEKDAY('0000-00-00'), DAYOFYEAR('0000-00-00'), DAYNAME('0000-00-00'), MONTHNAME('0000-00-00'), WEEKOFYEAR('0000-00-00')",
			"SHOW WARNINGS",
			"SELECT id, DAYOFWEEK(d), WEEKDAY(dt), DAYOFYEAR(d), DAYNAME(dt), MONTHNAME(d), WEEKOFYEAR(dt), QUARTER(d), MONTH(d), YEAR(dt) FROM zd ORDER BY id",
			"SHOW WARNINGS",
			// WP_Date_Query の week は WEEK(post_date, mode) を出す。LEFT JOIN の NULL（前は 1105 で文全体が誤り）
			"SELECT p.ID, WEEK(p.post_date, 1), WEEK(c.post_date, 1), YEARWEEK(c.post_date) FROM wp_posts p LEFT JOIN wp_posts c ON c.post_parent = p.ID ORDER BY p.ID",
			"SHOW WARNINGS",
			"SELECT ID FROM wp_posts WHERE WEEK(post_date, 1) = 1 AND YEAR(post_date) = 2024 ORDER BY ID",
			"SELECT ID, TO_DAYS(post_date_gmt), LAST_DAY(post_date_gmt), WEEK(post_date_gmt, 1) FROM wp_posts ORDER BY ID",
			"SHOW WARNINGS",
		},
		drop: []string{"DROP TABLE wp_posts", "DROP TABLE zd"},
	},
}

// stage118Statements は、組をつないだ文の列。
func stage118Statements() []string {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage118Groups {
		stmts = append(stmts, g.setup...)
		if len(g.both) > 0 {
			stmts = append(stmts, "SET SESSION sql_mode = DEFAULT")
			stmts = append(stmts, g.both...)
			stmts = append(stmts, wpSQLMode)
			stmts = append(stmts, g.both...)
			stmts = append(stmts, "SET SESSION sql_mode = DEFAULT")
		}
		stmts = append(stmts, g.stmts...)
		stmts = append(stmts, "SET SESSION sql_mode = DEFAULT")
		stmts = append(stmts, g.drop...)
	}
	return stmts
}

func TestStage118Golden(t *testing.T) {
	checkGolden(t, "../testdata/stage118_golden.txt", "stage118_golden", stage118Statements())
}

// 段階 118 の 2: FLOAT と DOUBLE の列と式の列の情報の Decimals は、MySQL と同じく 31（NOT_FIXED_DEC。go-sql-driver の
// DecimalSize は小数の桁を math.MaxInt64 と読む）。前は 0 で、mysqlnd はバイナリプロトコルの FLOAT の 1.5 を 2 と読んだ
// （PHP の mysqli_stmt とエミュレーションなしの PDO。本物の MySQL 8.0 と比べて確かめた）。期待の値は本物の MySQL 8.0 の
// mysql --column-type-info で確かめた（FLOAT(7,2) の 2 と PI() の 6 は HeimDB では 31 で、docs/differences.md に記録）。
func TestStage118FloatDecimals(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE fl (id INT PRIMARY KEY, f FLOAT, d DOUBLE, r REAL, n DECIMAL(10,2))")
	mustExec(t, db, "INSERT INTO fl VALUES (1, 1.5, 1.5, 1.5, 1.5)")
	const q = "SELECT f, d, r, f + 0, ROUND(f, 2), SUM(d), AVG(f), CAST(f AS DOUBLE), IF(id = 1, f, d), COALESCE(f, d), 1.5e0, f * 2, STDDEV(f), AVG(id), n FROM fl WHERE id >= ? GROUP BY id"
	notFixed := int64(math.MaxInt64)
	want := []int64{notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, notFixed, 4, 2}
	for _, binary := range []bool{false, true} {
		var rows *sql.Rows
		if binary {
			rows, err = db.Query(q, 1) // 引数のある Query はバイナリプロトコル
		} else {
			rows, err = db.Query(q[:len(q)-len("WHERE id >= ? GROUP BY id")] + "GROUP BY id")
		}
		if err != nil {
			t.Fatal(err)
		}
		cts, err := rows.ColumnTypes()
		if err != nil {
			t.Fatal(err)
		}
		for i, ct := range cts {
			_, scale, ok := ct.DecimalSize()
			if !ok || scale != want[i] {
				t.Errorf("binary=%v: %s: decimals %d (%v), want %d", binary, ct.Name(), scale, ok, want[i])
			}
		}
		rows.Close()
	}
}
