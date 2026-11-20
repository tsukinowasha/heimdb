package backend

import (
	"testing"
)

// stage119Group は、段階 119 で直した TEXT の列の値を数にする残りの経路の文（docs/plan.md の段階 119）。直す前の
// HeimDB は本物の MySQL 8.0 と違う警告か値か誤りを返した。setup で表を作り、both を既定の sql_mode と WordPress の
// sql_mode の両方で流し、stmts をそのあとに流し、drop で表を消す（段階 118 の stage118Group と同じ）。
type stage119Group struct {
	name  string
	setup []string
	both  []string
	stmts []string
	drop  []string
}

var stage119Groups = []stage119Group{
	{
		name: "textwarn", // 1. TEXT の列の参照を数にする残りの経路（IFNULL と COALESCE、IF と CASE の枝、真偽、ROUND など、%、ビット演算、IN、ウィンドウの SUM と AVG）は警告 1292 を出さない
		setup: []string{
			"CREATE TABLE tx (id INT PRIMARY KEY, v LONGTEXT, t TEXT, tt TINYTEXT, mt MEDIUMTEXT, vc VARCHAR(50))",
			"INSERT INTO tx VALUES (1, '1700000000:1', '1700000000:1', '5', '1700000000:1', '17'), (2, 'abc', 'abc', 'abc', 'abc', 'abc'), (3, '12abc', '12abc', '12abc', '12abc', '12abc'), " +
				"(4, '', ' 2 ', '-3.5x', '0', '3'), (5, NULL, NULL, NULL, NULL, NULL)",
			"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, " +
				"KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_edit_lock', '1704103200:1'), (2, '_edit_lock', '1704189600:2'), (1, '_price', '10.505'), (2, '_price', ''), (3, '_price', '9.99'), " +
				"(1, '_stock', '5'), (2, '_stock', 'n/a'), (3, '_stock', '0'), (4, '_stock', '-2')",
		},
		both: []string{
			// IFNULL と COALESCE は、MySQL では引数によらず（VARCHAR の列でも）出さない
			"SELECT id, IFNULL(v, 0) + 0, COALESCE(v, 0) + 0, IFNULL(t, 0) * 2, COALESCE(mt, tt) - 1 FROM tx ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, IFNULL(vc, 0) + 0, COALESCE(vc, 0) * 1, ROUND(IFNULL(vc, 0)), IFNULL(vc, 0) > 5 FROM tx ORDER BY id",
			"SHOW WARNINGS",
			// IF、CASE、NULLIF は選んだ枝の値
			"SELECT id, IF(id > 0, v, 0) + 0, CASE WHEN id > 0 THEN t END + 0, NULLIF(v, 'q') + 0, IF(id > 0, 1, mt) + 0 FROM tx ORDER BY id",
			"SHOW WARNINGS",
			// 真偽
			"SELECT id, IF(v, 1, 0), NOT v, v AND id, v OR 0, v XOR 1, t IS TRUE, tt IS NOT FALSE, CASE WHEN mt THEN 'y' ELSE 'n' END FROM tx ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx WHERE NOT v ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx WHERE v OR id = 5 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx WHERE t AND id > 0 ORDER BY id",
			"SHOW WARNINGS",
			// 数の関数（CEIL と FLOOR も。SIGN は前から出さない）
			"SELECT id, ROUND(v), ROUND(t, 1), TRUNCATE(tt, 1), CEIL(tt), FLOOR(tt), ABS(tt), SIGN(v) FROM tx ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, CEIL(v), FLOOR(t) FROM tx WHERE id IN (2, 3) ORDER BY id",
			"SHOW WARNINGS",
			// %、MOD、ビット演算
			"SELECT id, v % 7, MOD(t, 7), tt % 2.5 FROM tx ORDER BY id",
			"SHOW WARNINGS",
			// ビット演算は 'abc' と '12abc' の行だけ（'1700000000:1' と '-3.5x' の値の違いは docs/differences.md）
			"SELECT id, v & 1, t | 0, mt << 1, tt >> 1, v ^ 1 FROM tx WHERE id IN (2, 3) ORDER BY id",
			"SHOW WARNINGS",
			// IN と BETWEEN
			"SELECT id, v IN (1, 2), v IN (12, 13), t NOT IN (12), tt IN (5, 7), 12 IN (v, 13), v BETWEEN 1 AND 20 FROM tx ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx WHERE v IN (12, 13) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx WHERE t NOT IN (12, 13) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx WHERE v BETWEEN 1 AND 20 ORDER BY id",
			"SHOW WARNINGS",
			// ウィンドウの SUM と AVG
			"SELECT id, SUM(v) OVER (), AVG(t) OVER (ORDER BY id), SUM(tt) OVER (PARTITION BY id) FROM tx ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id FROM tx ORDER BY v + 0, id",
			"SHOW WARNINGS",
			// VARCHAR の列、定数、式は今のまま警告を出す（MySQL と同じ）
			"SELECT id, IF(vc, 1, 0), NOT vc, ROUND(vc), ABS(vc), CEIL(vc), vc % 7 FROM tx WHERE id IN (2, 3) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, vc IN (1, 2), IF(id > 0, vc, 0) + 0, NULLIF(vc, 'q') + 0 FROM tx WHERE id IN (2, 3) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, SUM(vc) OVER () FROM tx WHERE id IN (2, 3) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, ROUND(LOWER(v)), NOT CONCAT(t), LOWER(v) IN (12), IF(UPPER(v), 1, 0) FROM tx WHERE id IN (2, 3) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT NOT 'abc', ROUND('12abc'), ABS('x'), 'abc' IN (1, 2), '7x' % 2",
			"SHOW WARNINGS",
			// プラグインの形
			"SELECT post_id, ROUND(meta_value, 2), ABS(meta_value), IFNULL(meta_value, 0) + 0 FROM wp_postmeta WHERE meta_key = '_price' ORDER BY post_id",
			"SHOW WARNINGS",
			"SELECT post_id FROM wp_postmeta WHERE meta_key = '_stock' AND meta_value IN (0, 5) ORDER BY post_id",
			"SHOW WARNINGS",
			"SELECT post_id, IF(meta_value, 'in', 'out'), meta_value > 0 AND meta_value FROM wp_postmeta WHERE meta_key = '_stock' ORDER BY post_id",
			"SHOW WARNINGS",
			"SELECT post_id, meta_value, SUM(meta_value) OVER (PARTITION BY meta_key) FROM wp_postmeta WHERE meta_key IN ('_price', '_stock') ORDER BY meta_id",
			"SHOW WARNINGS",
		},
		drop: []string{"DROP TABLE wp_postmeta", "DROP TABLE tx"},
	},
	{
		name: "mathtext", // 2. 数でない文字列（TEXT と VARCHAR の列、定数）の VARIANCE と STDDEV、POW、SQRT、対数、三角関数、EXP、SIGN、CEIL と FLOOR の値（前は 0 か 1366 か 1105）
		setup: []string{
			"CREATE TABLE tm (id INT PRIMARY KEY, v LONGTEXT, t TEXT, vc VARCHAR(50))",
			"INSERT INTO tm VALUES (1, 'abc', 'abc', 'abc'), (2, '12abc', '12abc', '12abc'), (3, '', '', ''), (4, '4', '4', '4'), (5, NULL, NULL, NULL), (6, '0.5x', '0.5x', '0.5x')",
			"CREATE TABLE tb (id INT PRIMARY KEY, v LONGTEXT)",
			"INSERT INTO tb VALUES (1, '3000000000x'), (2, '-3000000000.5'), (3, '1e10'), (4, '0.4'), (5, ' 5'), (6, '-0.0001x')",
			"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, " +
				"KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_price', '10'), (2, '_price', ''), (3, '_price', '14'), (4, '_price', 'n/a'), (1, '_rating', '4'), (2, '_rating', '5 stars'), (3, '_rating', '3')",
		},
		both: []string{
			// VARIANCE と STDDEV は先頭の数の部分で計算する（前は 0。警告の文は %!s(float64=0) だった）
			"SELECT VARIANCE(v), STDDEV(v), STD(v), VAR_POP(v), VAR_SAMP(v), STDDEV_POP(v), STDDEV_SAMP(v) FROM tm",
			"SHOW WARNINGS",
			"SELECT VARIANCE(t), STDDEV_SAMP(t), VARIANCE(vc), STDDEV(vc) FROM tm",
			"SHOW WARNINGS",
			"SELECT id, VARIANCE(v), STDDEV(vc) FROM tm GROUP BY id ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, STDDEV(v) OVER (ORDER BY id), VAR_POP(t) OVER (ORDER BY id) FROM tm WHERE id < 5 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, VAR_SAMP(vc) OVER () FROM tm WHERE id < 5 ORDER BY id",
			"SHOW WARNINGS",
			// 数学の関数は先頭の数の部分で計算する（前は 1366 か 1105 で文全体が誤り）。最後の桁が Go と libm で違う関数は ROUND で丸める
			"SELECT id, POW(v, 1), POWER(t, 2), POW(2, v), SQRT(v), EXP(t), DEGREES(v) FROM tm ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, LN(v), LOG(t), LOG10(v), LOG2(t), LOG(v, 8) FROM tm ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, ROUND(SIN(v), 12), ROUND(COS(t), 12), ROUND(TAN(v), 12), ROUND(ASIN(t), 12), ROUND(ACOS(v), 12), ROUND(ATAN(t), 12), ROUND(ATAN2(1, v), 12), ROUND(RADIANS(t), 12) FROM tm ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, ROUND(COT(v), 12) FROM tm WHERE id IN (2, 4, 6) ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, POW(vc, 1), SQRT(vc), LN(vc), DEGREES(vc), EXP(vc), SIGN(vc) FROM tm ORDER BY id",
			"SHOW WARNINGS",
			"SELECT POW('abc', 1), POW('12abc', 2), SQRT('12abc'), LN('x'), EXP(''), LOG('2', '8x'), DEGREES('1x'), ROUND(SIN('1x'), 12)",
			"SHOW WARNINGS",
			"SELECT LN(0), LOG(-1), LOG2(0), LOG10(0), LOG(1, 2)",
			"SHOW WARNINGS",
			// SIGN は DOUBLE で決める（前は SIGN(0.4) と SIGN(' 5') が 0）。CEIL と FLOOR の文字列は DOUBLE（前は INT で '3000000000x' が INT の端になった）
			"SELECT SIGN(0.4), SIGN(-0.3), SIGN(0.4e0), SIGN('0.3'), SIGN('-0.0001x'), SIGN(' 5'), SIGN('-0'), SIGN('x'), SIGN('1e-400'), SIGN(-0.0)",
			"SHOW WARNINGS",
			"SELECT id, SIGN(v), CEIL(v), FLOOR(v), CEIL(v) + 1 FROM tb ORDER BY id",
			"SHOW WARNINGS",
			"SELECT CEIL('3000000000x'), FLOOR('-3000000000.5'), CEIL('1e10'), FLOOR('12abc')",
			"SHOW WARNINGS",
			// DOUBLE を超える文字列は DOUBLE の端（前は CEIL と FLOOR が INT の端、SQRT と POW が 1366）
			"SELECT CEIL('1e400'), FLOOR('1e400'), SQRT('1e400'), POW('1e400', 1), SIGN('+.5')",
			"SHOW WARNINGS",
			// プラグインの形
			"SELECT meta_key, VARIANCE(meta_value), STDDEV(meta_value), AVG(meta_value) FROM wp_postmeta GROUP BY meta_key ORDER BY meta_key",
			"SHOW WARNINGS",
			"SELECT post_id, POW(meta_value, 2), SQRT(meta_value), SIGN(meta_value - 12) FROM wp_postmeta WHERE meta_key = '_price' ORDER BY post_id",
			"SHOW WARNINGS",
		},
		stmts: []string{
			// 段階 119 のレビュー: 日付、日時、TIME は数の形、JSON はその値（前は SIGN(t) が 1105、SIGN(j) が 1366。POW などは
			// 段階 119 の前から誤り）。JSON の警告 3156 は MySQL が「at row N」まで書くので見ない（docs/differences.md）
			"CREATE TABLE t6 (id INT PRIMARY KEY, t TIME, j JSON, d DATE, dt DATETIME)",
			"INSERT INTO t6 VALUES (1, '03:04:05', '12', '2024-01-02', '2024-01-02 03:04:05'), (2, '-10:00:00', '\"5x\"', '2024-01-02', '2024-01-02 03:04:05'), (3, '00:00:00', '{\"a\": 1}', NULL, NULL), (4, NULL, 'true', NULL, NULL)",
			"SELECT id, SIGN(t), SIGN(j) FROM t6 ORDER BY id",
			"SELECT id, POW(t, 1), SQRT(t), LN(t), ROUND(SIN(t), 12) FROM t6 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT id, POW(j, 1), SQRT(j), EXP(j), LN(j), ROUND(SIN(j), 12) FROM t6 ORDER BY id",
			"SELECT id, SIGN(d), SIGN(dt), POW(d, 1), POW(dt, 1), SQRT(d) FROM t6 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT VARIANCE(t), STDDEV(d), VAR_SAMP(dt), STDDEV_SAMP(t) FROM t6",
			"SHOW WARNINGS",
			"DROP TABLE t6",
		},
		drop: []string{"DROP TABLE wp_postmeta", "DROP TABLE tb", "DROP TABLE tm"},
	},
}

// stage119Statements は、組をつないだ文の列。
func stage119Statements() []string {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage119Groups {
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

func TestStage119Golden(t *testing.T) {
	checkGolden(t, "../testdata/stage119_golden.txt", "stage119_golden", stage119Statements())
}
