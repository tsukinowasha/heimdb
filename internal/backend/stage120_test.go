package backend

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"

	vmysql "github.com/dolthub/vitess/go/mysql"
	"github.com/go-sql-driver/mysql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// stage120Groups は、段階 120 で直した FLOAT(M,D) と DOUBLE(M,D) の文（docs/plan.md の段階 120）。直す前の HeimDB は
// (M,D) と UNSIGNED を落とし、表示、定義、式の桁が本物の MySQL 8.0 と違った。組の形は段階 119 の stage119Group と同じ。
var stage120Groups = []stage119Group{
	{
		name: "realtypes", // 1. 型が (M,D) と UNSIGNED を持つ（SHOW CREATE TABLE、DESCRIBE、表示、式の桁、M と D の誤り、ALTER の MODIFY）
		setup: []string{
			"CREATE TABLE fm (id INT PRIMARY KEY, f FLOAT(7,2), d DOUBLE(10,3), u FLOAT(5,1) UNSIGNED, p FLOAT, q DOUBLE, fu FLOAT UNSIGNED, du DOUBLE UNSIGNED, " +
				"f30 FLOAT(30,5), r REAL(8,3), dp DOUBLE PRECISION(6,2), f25 FLOAT(25), f24 FLOAT(24), f0 FLOAT(7,0), f25u FLOAT(25) UNSIGNED)",
			// 値は D 桁の値（丸めは 3 の組）
			"INSERT INTO fm VALUES (1, 1.55, 2.001, 1.2, 1.5, 1.5, 1, 1, 1234567.125, 1.234, 1.23, 1.5, 1.5, 2, 1), (2, 12345.68, 1234567.891, 0, 2, 2, 2, 2, 1, 1, 1, 1, 1, -3, 2), " +
				"(3, 99999.99, 0, 3.4, 3, 3, 3, 3, 3, 3, 3, 3, 3, 0, 3), (4, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL), (5, 0.1, -0.001, 99.9, 0.5, 0.25, 0, 0, -1.5, -2.5, 0.01, 0.1, 0.1, 7, 0)",
		},
		stmts: []string{
			"SHOW CREATE TABLE fm",
			"DESCRIBE fm",
			"SHOW FULL COLUMNS FROM fm",
			"SELECT COLUMN_NAME, COLUMN_TYPE, DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'fm' ORDER BY ORDINAL_POSITION",
			"SELECT * FROM fm ORDER BY id",
			// 式の桁: + - * % は引数の桁の最大、/ は 4 を足す、符号と ABS は D、桁のない FLOAT との式は桁が決まらない
			"SELECT id, f + 0, f - 1, f * 2, f % 3, f / 3, d / 3, -f, ABS(f), -u, u - 1, f + d, f + p, f + 1.5, f + id, f * 1.5, f / 2.5, 1.5 / f, d / f, f / p, f0 + 1, f0 / 2 FROM fm ORDER BY id",
			"SELECT id, IF(id > 1, f, d), COALESCE(f, d), IFNULL(f, 0), IFNULL(f, d), CASE WHEN id > 1 THEN f ELSE d END, CASE WHEN id > 1 THEN f ELSE 0 END, IF(id > 1, f, f), COALESCE(u, 0), f DIV 1 FROM fm ORDER BY id",
			"SELECT SUM(f), AVG(f), MAX(f), MIN(f), SUM(d), AVG(d), MAX(d), MIN(d), AVG(u), SUM(f0), AVG(f0), MAX(f30), SUM(p) FROM fm",
			"SELECT id, SUM(f) OVER (ORDER BY id), AVG(d) OVER (), MAX(u) OVER (), MIN(f) OVER (PARTITION BY id > 2) FROM fm ORDER BY id",
			"SELECT id DIV 2 AS g, SUM(f), AVG(f), MAX(d) FROM fm GROUP BY g ORDER BY g",
			// M と D の範囲（MySQL と同じ誤り。前は 1105 strconv.ParseInt）
			"CREATE TABLE x1 (f FLOAT(256,2))",
			"CREATE TABLE x2 (f FLOAT(40,31))",
			"CREATE TABLE x3 (f FLOAT(3,5))",
			"CREATE TABLE x4 (f DOUBLE(256,2))",
			"CREATE TABLE x5 (f DOUBLE(10,31))",
			"CREATE TABLE x6 (f FLOAT(0,0))",
			"CREATE TABLE x7 (f FLOAT(255,30), g DOUBLE(255,30), h FLOAT(53,2))",
			"SHOW CREATE TABLE x7",
			"DROP TABLE x7",
			"ALTER TABLE fm ADD COLUMN z DOUBLE(300,2)",
			// ALTER の MODIFY で桁を変える（前は同じ型と見て、桁が変わらなかった）
			"ALTER TABLE fm MODIFY f FLOAT(8,3)",
			"ALTER TABLE fm MODIFY p FLOAT(10,4) UNSIGNED, MODIFY q DOUBLE(12,4)",
			"ALTER TABLE fm MODIFY fu FLOAT",
			"SHOW CREATE TABLE fm",
			"SELECT id, f, p, q, fu FROM fm ORDER BY id",
			"ALTER TABLE fm MODIFY f FLOAT(3,5)",
			// プラグインの表（Wordfence の wfLocs と wfHits の一部、MainWP Child の changes_logs）。dbDelta は DESCRIBE の Type を宣言と比べる
			"CREATE TABLE wfLocs (IP BINARY(16) NOT NULL DEFAULT '', ctime INT UNSIGNED NOT NULL, failed TINYINT UNSIGNED NOT NULL, city VARCHAR(255) DEFAULT '', " +
				"region VARCHAR(255) DEFAULT '', countryName VARCHAR(255) DEFAULT '', countryCode CHAR(2) DEFAULT '', lat FLOAT(10,7) DEFAULT 0, lon FLOAT(10,7) DEFAULT 0, PRIMARY KEY (IP))",
			"INSERT INTO wfLocs (IP, ctime, failed, city, lat, lon) VALUES ('a', 1, 0, 'Tokyo', 35.6812363, 139.7671248), ('b', 2, 0, 'Sydney', -33.8688197, 151.2092955), ('c', 3, 1, '', 0, 0)",
			"SELECT city, lat, lon FROM wfLocs ORDER BY ctime",
			"DESCRIBE wfLocs",
			"CREATE TABLE wfHits (id INT UNSIGNED NOT NULL AUTO_INCREMENT, attackLogTime DOUBLE(17,6) UNSIGNED NOT NULL, ctime DOUBLE(17,6) UNSIGNED NOT NULL, statusCode INT NOT NULL DEFAULT 200, " +
				"PRIMARY KEY (id), KEY k1 (ctime), KEY k2 (attackLogTime))",
			"INSERT INTO wfHits (attackLogTime, ctime) VALUES (0, 1700000000.123456), (1700000001.5, 1700000001.000001)",
			"SELECT id, attackLogTime, ctime, ctime - attackLogTime, ROUND(ctime) FROM wfHits ORDER BY id",
			"SELECT id FROM wfHits WHERE ctime > 1700000000.5 ORDER BY id",
			"DESCRIBE wfHits",
			"CREATE TABLE changes_logs (log_id BIGINT NOT NULL AUTO_INCREMENT, duration FLOAT(11,4) NOT NULL DEFAULT 0, PRIMARY KEY (log_id))",
			"INSERT INTO changes_logs (duration) VALUES (0.0125), (12.5), (0)",
			"SELECT log_id, duration, duration * 1000 FROM changes_logs ORDER BY log_id",
			"SHOW COLUMNS FROM changes_logs",
			"DROP TABLE changes_logs",
			"DROP TABLE wfHits",
			"DROP TABLE wfLocs",
		},
		drop: []string{"DROP TABLE fm"},
	}, {
		name: "realwrite", // 2. 書き込みで D 桁に丸め、範囲（M,D と UNSIGNED）を確かめる（非 STRICT は端の値と警告 1264、STRICT は 1264 の誤り）。比べは D 桁の定数で MySQL と同じ
		setup: []string{
			"CREATE TABLE rw (id INT PRIMARY KEY, f FLOAT(7,2), d DOUBLE(10,3), u FLOAT(5,1) UNSIGNED, fu FLOAT UNSIGNED, du DOUBLE UNSIGNED, f107 FLOAT(10,7), KEY kf (f), UNIQUE KEY ud (d))",
		},
		both: []string{
			"INSERT INTO rw VALUES (1, 1.555, 2.0005, 1.25, 1, 1, 1.23456789)",
			"SHOW WARNINGS",
			"INSERT INTO rw VALUES (2, 12345.678, 1234567.8915, 0.05, 0, 0, 35.6812363)",
			"INSERT INTO rw VALUES (3, 99999.999, 99999999, 9999.95, 1e39, 1e300, 1234567.5)",
			"SHOW WARNINGS",
			"INSERT INTO rw VALUES (4, -1, -0.0004, -5, -1, -2, -0.00000005)",
			"SHOW WARNINGS",
			"INSERT INTO rw VALUES (5, '12.345x', ' 7.0005 ', '-0.04', '3', '-0', '1e-8')",
			"SHOW WARNINGS",
			"INSERT INTO rw VALUES (6, 0.125, 1.0005, 3.35, 2.5, 0.0005, 0.12345675)",
			"SELECT * FROM rw ORDER BY id",
			"UPDATE rw SET f = f + 0.005, d = d * 1.0004 WHERE id = 1",
			"SHOW WARNINGS",
			"UPDATE rw SET d = 99999999, u = -1 WHERE id = 2",
			"SHOW WARNINGS",
			"INSERT INTO rw SELECT id + 10, f * 1.001, d + 1000.0004, u, fu, du, f107 / 3 FROM rw WHERE id <= 2",
			"SHOW WARNINGS",
			"INSERT INTO rw (id, f, d) VALUES (1, 2.345, 5) ON DUPLICATE KEY UPDATE f = f + 1.004, u = 2.25",
			"SHOW WARNINGS",
			// 一意索引は丸めた値で重なる（2.0006 は 2.001）
			"INSERT INTO rw (id, d) VALUES (20, 2.0016)",
			"INSERT INTO rw (id, d) VALUES (21, 2.0024)",
			"SELECT * FROM rw ORDER BY id",
			// D 桁の定数の = と >（索引 kf と ud を使う。FORCE と IGNORE で同じ）
			"SELECT id FROM rw WHERE f = 1.56 ORDER BY id",
			"SELECT id FROM rw IGNORE INDEX (kf) WHERE f = 1.56 ORDER BY id",
			"SELECT id FROM rw WHERE f > 12345.68 ORDER BY id",
			"SELECT id FROM rw IGNORE INDEX (kf) WHERE f > 12345.68 ORDER BY id",
			"SELECT id FROM rw WHERE d = 2.002 ORDER BY id",
			"SELECT id FROM rw IGNORE INDEX (ud) WHERE d = 2.002 ORDER BY id",
			"SELECT id FROM rw WHERE d >= 7 ORDER BY id",
			"SELECT id FROM rw IGNORE INDEX (ud) WHERE d >= 7 ORDER BY id",
			"DELETE FROM rw",
		},
		stmts: []string{
			// 比べ: MySQL の FLOAT(7,2) の 1.55、1.56、0.1、100、1.555（1.55 になる）と DOUBLE(10,3)
			"CREATE TABLE c1 (id INT PRIMARY KEY, f FLOAT(7,2), d DOUBLE(10,3))",
			"CREATE TABLE c2 (id INT PRIMARY KEY, f FLOAT(7,2), d DOUBLE(10,3), KEY kf (f), KEY kd (d))",
			"INSERT INTO c1 VALUES (1, 1.55, 1.5), (2, 1.56, 1.25), (3, 0.1, 0.1), (4, 100, 100), (5, 1.555, 2.0005)",
			"INSERT INTO c2 SELECT * FROM c1",
			"SELECT * FROM c2 ORDER BY id",
			"SELECT id FROM c1 WHERE f = 1.55 ORDER BY id",
			"SELECT id FROM c2 WHERE f = 1.55 ORDER BY id",
			"SELECT id FROM c1 WHERE f = 0.1 ORDER BY id",
			"SELECT id FROM c2 WHERE f = 0.1 ORDER BY id",
			"SELECT id FROM c1 WHERE f > 1.55 ORDER BY id",
			"SELECT id FROM c2 WHERE f > 1.55 ORDER BY id",
			"SELECT id FROM c1 WHERE f >= 1.56 ORDER BY id",
			"SELECT id FROM c2 WHERE f >= 1.56 ORDER BY id",
			"SELECT id FROM c1 WHERE f < 1.56 ORDER BY id",
			"SELECT id FROM c2 WHERE f < 1.56 ORDER BY id",
			"SELECT id FROM c1 WHERE f = 100 ORDER BY id",
			"SELECT id FROM c2 WHERE f = 100 ORDER BY id",
			"SELECT id FROM c1 WHERE f <> 1.55 ORDER BY id",
			"SELECT id FROM c2 WHERE f <> 1.55 ORDER BY id",
			"SELECT id FROM c1 WHERE d = 1.5 ORDER BY id",
			"SELECT id FROM c2 WHERE d = 1.5 ORDER BY id",
			"SELECT id FROM c1 WHERE d = 0.1 ORDER BY id",
			"SELECT id FROM c2 WHERE d = 0.1 ORDER BY id",
			"SELECT id FROM c1 WHERE d > 1.25 ORDER BY id",
			"SELECT id FROM c2 WHERE d > 1.25 ORDER BY id",
			"SELECT id FROM c1 WHERE d = 2.001 ORDER BY id",
			"SELECT id FROM c2 WHERE d = 2.001 ORDER BY id",
			// 丸めで値の変わる端（1.2495 は 1.25 でなく 1.249）は丸めずに比べる（索引の区間の端にしない）
			"SELECT id FROM c1 WHERE d > 1.2495 ORDER BY id",
			"SELECT id FROM c2 WHERE d > 1.2495 ORDER BY id",
			"SELECT id FROM c1 WHERE d < 1.2505 ORDER BY id",
			"SELECT id FROM c2 WHERE d < 1.2505 ORDER BY id",
			"SELECT id FROM c1 WHERE d >= 1.2505 ORDER BY id",
			"SELECT id FROM c2 WHERE d >= 1.2505 ORDER BY id",
			"SELECT id FROM c1 WHERE d <= 1.2495 ORDER BY id",
			"SELECT id FROM c2 WHERE d <= 1.2495 ORDER BY id",
			"SELECT id FROM c1 WHERE d BETWEEN 1.2495 AND 1.2505 ORDER BY id",
			"SELECT id FROM c2 WHERE d BETWEEN 1.2495 AND 1.2505 ORDER BY id",
			"SELECT id FROM c1 WHERE f >= 1.555 ORDER BY id",
			"SELECT id FROM c2 WHERE f >= 1.555 ORDER BY id",
			"SELECT id FROM c1 WHERE f < 1.555 ORDER BY id",
			"SELECT id FROM c2 WHERE f < 1.555 ORDER BY id",
			"/* rows */ SELECT id, IF(f = 1.55, 'y', 'n'), IF(f = 0.1, 'y', 'n'), IF(f > 1.55, 'y', 'n'), IF(d = 2.001, 'y', 'n'), IF(f = 1.55e0, 'y', 'n') FROM c1 ORDER BY id",
			// ALTER の書き直しで丸め、範囲を確かめる（FLOAT から FLOAT(5,1) UNSIGNED は MySQL も表を写して書き直す）
			"CREATE TABLE ra (id INT PRIMARY KEY, y FLOAT)",
			"INSERT INTO ra VALUES (1, 1.23456), (2, 2.5), (3, -1)",
			"ALTER TABLE ra MODIFY y FLOAT(5,1) UNSIGNED",
			"SELECT * FROM ra ORDER BY id",
			wpSQLMode,
			"ALTER TABLE ra MODIFY y FLOAT(5,1) UNSIGNED",
			"SELECT * FROM ra ORDER BY id",
			"SET SESSION sql_mode = DEFAULT",
			"DROP TABLE ra",
			"DROP TABLE c2",
			"DROP TABLE c1",
		},
		drop: []string{"DROP TABLE rw"},
	}, {
		name: "setbit", // 3. SET と BIT の列の索引のエントリ（キーの水準 2）。範囲で引いた結果と並びが、MySQL と同じで索引の有無で変わらない
		setup: []string{
			"CREATE TABLE sb (id INT PRIMARY KEY, s SET('a','b','c'), b BIT(8), b64 BIT(64), KEY ks (s), KEY kb (b), KEY kb64 (b64), UNIQUE KEY us (s, b))",
			"INSERT INTO sb VALUES (1, 'a', 5, 1), (2, 'a,c', 255, 18446744073709551615), (3, '', 0, 9223372036854775808), (4, 'b,c', 16, 0), (5, NULL, NULL, NULL), (6, 'a', 6, 2), (7, 'c', 53, 9223372036854775807)",
			"CREATE TABLE sj (id INT PRIMARY KEY, v VARCHAR(10))",
			"INSERT INTO sj VALUES (1, 'c'), (2, '4'), (3, 'a,c'), (4, 'x')",
		},
		stmts: []string{
			"SELECT id FROM sb WHERE s = 'a' ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s = 'a' ORDER BY id",
			"SELECT id FROM sb WHERE s = 'bad' ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s = 'bad' ORDER BY id",
			"SELECT id FROM sb WHERE s IN ('a', 'bad') ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s IN ('a', 'bad') ORDER BY id",
			"SELECT id FROM sb WHERE s IN ('a,c', 'b,c') ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s IN ('a,c', 'b,c') ORDER BY id",
			"SELECT id FROM sb WHERE s = '' ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s = '' ORDER BY id",
			"SELECT id FROM sb WHERE s = 3 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s = 3 ORDER BY id",
			"SELECT id FROM sb WHERE s IS NULL ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s IS NULL ORDER BY id",
			"SELECT id FROM sb WHERE b = 5 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b = 5 ORDER BY id",
			"SELECT id FROM sb WHERE b > 5 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b > 5 ORDER BY id",
			"SELECT id FROM sb WHERE b >= 6 AND b < 255 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b >= 6 AND b < 255 ORDER BY id",
			"SELECT id FROM sb WHERE b IN (5, 16, 300) ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b IN (5, 16, 300) ORDER BY id",
			"SELECT id FROM sb WHERE b BETWEEN 6 AND 53 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b BETWEEN 6 AND 53 ORDER BY id",
			"SELECT id FROM sb WHERE b = 256 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b = 256 ORDER BY id",
			"SELECT id FROM sb WHERE b = -1 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b = -1 ORDER BY id",
			"SELECT id FROM sb WHERE b < 0 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b < 0 ORDER BY id",
			"SELECT id FROM sb WHERE b <> 5 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b <> 5 ORDER BY id",
			"SELECT id FROM sb WHERE b64 >= 9223372036854775808 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b64 >= 9223372036854775808 ORDER BY id",
			"SELECT id FROM sb WHERE b64 < 2 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b64 < 2 ORDER BY id",
			"SELECT id FROM sb WHERE b64 = 18446744073709551615 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE b64 = 18446744073709551615 ORDER BY id",
			"SELECT id FROM sb WHERE s = 'a' AND b > 5 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s = 'a' AND b > 5 ORDER BY id",
			"SELECT id FROM sb WHERE s = 'a,c' AND b = 255 ORDER BY id",
			"SELECT id FROM sb IGNORE INDEX (ks, kb, kb64, us) WHERE s = 'a,c' AND b = 255 ORDER BY id",
			// 索引の順に読む（BIT(64) の 2^63 以上も符号なしの数の順）
			"SELECT id FROM sb FORCE INDEX (kb) WHERE b > 0 ORDER BY b",
			"SELECT id FROM sb FORCE INDEX (kb) WHERE b >= 0 ORDER BY b DESC",
			"SELECT id FROM sb FORCE INDEX (kb64) WHERE b64 > 0 ORDER BY b64 DESC",
			"SELECT id FROM sb FORCE INDEX (ks) WHERE s IN ('a', 'c', 'a,c') ORDER BY s, id",
			// 書き込みの後もエントリで引く。一意索引の重なりは 1062
			"UPDATE sb SET s = 'b', b = 7 WHERE id = 6",
			"DELETE FROM sb WHERE id = 7",
			"INSERT INTO sb VALUES (8, 'a,b', 9, 3)",
			"INSERT INTO sb VALUES (9, 'a,c', 255, 4)",
			"INSERT INTO sb VALUES (10, 'c,a', 255, 4)",
			"SELECT id FROM sb WHERE s = 'b' ORDER BY id",
			"SELECT id FROM sb WHERE b IN (6, 7, 9) ORDER BY id",
			"SELECT id FROM sb WHERE b64 >= 3 ORDER BY id",
			// SET と VARCHAR の結合は値の文字列で比べる（lookup にならない）
			"SELECT sb.id, sj.id FROM sb JOIN sj ON sb.s = sj.v ORDER BY sb.id, sj.id",
			"SELECT sj.id, sb.id FROM sj JOIN sb ON sb.s = sj.v ORDER BY sj.id, sb.id",
			"DROP TABLE sj",
		},
		drop: []string{"DROP TABLE sb"},
	}, {
		name: "review1", // 4. 段階 120 の 1 回目のレビュー: FLOAT(M,D) の列と定数の比べ（単精度）、FLOAT と DOUBLE の UNION、FLOAT(M,D) の端を倍精度で確かめる
		setup: []string{
			"CREATE TABLE c2 (id INT PRIMARY KEY, h FLOAT(10,7), k FLOAT(11,4), d DOUBLE(10,3), KEY kh (h), KEY kk (k))",
			"INSERT INTO c2 VALUES (1, 35.6812363, 1234567.12345, 1.5), (2, 0.1, 2.5, 0.1), (3, 1.25, 3, 2.001)",
			"CREATE TABLE wfHits (id INT PRIMARY KEY, ctime DOUBLE(17,6) UNSIGNED, f FLOAT(7,2), u FLOAT UNSIGNED)",
			"INSERT INTO wfHits VALUES (1, 1700000000.123456, 1.55, 2.5), (2, 9.5, 100, 0.5)",
			"CREATE TABLE wc_order_stats (id INT PRIMARY KEY, net_total DOUBLE, i INT, fl FLOAT)",
			"INSERT INTO wc_order_stats VALUES (1, 10.25, 7, 1.5), (2, 1000, 20, 0.25)",
			"CREATE TABLE e (id INT PRIMARY KEY, h FLOAT(10,7), d DOUBLE(5,2))",
		},
		stmts: []string{
			// A. 定数を列の単精度の型にして比べる（前は DECIMAL か倍精度で比べ、h = 35.6812363 が 0 行）。IN は MySQL も倍精度
			"SELECT id FROM c2 WHERE h = 35.6812363 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h = 35.6812363 ORDER BY id",
			"SELECT id FROM c2 WHERE h >= 35.6812363 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h >= 35.6812363 ORDER BY id",
			"SELECT id FROM c2 WHERE h IN (35.6812363, 0.1) ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h IN (35.6812363, 0.1) ORDER BY id",
			"SELECT id FROM c2 WHERE h IN (35.6812363, 1.25) ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h IN (35.6812363, 1.25) ORDER BY id",
			"SELECT id FROM c2 WHERE h <> 0 AND h < 35.6812363 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h <> 0 AND h < 35.6812363 ORDER BY id",
			"SELECT id FROM c2 WHERE h > 1.25 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h > 1.25 ORDER BY id",
			"SELECT id FROM c2 WHERE h <> 35.6812363 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h <> 35.6812363 ORDER BY id",
			"SELECT id FROM c2 WHERE h BETWEEN 1.25 AND 35.6812363 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h BETWEEN 1.25 AND 35.6812363 ORDER BY id",
			"SELECT id FROM c2 WHERE k = 1234567.125 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE k = 1234567.125 ORDER BY id",
			"SELECT id FROM c2 WHERE k >= 1234567.1234 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE k >= 1234567.1234 ORDER BY id",
			"SELECT id FROM c2 WHERE k = 1234567.1234 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE k = 1234567.1234 ORDER BY id",
			"SELECT id FROM c2 WHERE h = 0.1 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h = 0.1 ORDER BY id",
			"SELECT id FROM c2 WHERE h = 35 ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h = 35 ORDER BY id",
			"SELECT id FROM c2 WHERE h = '35.6812363' ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h = '35.6812363' ORDER BY id",
			"SELECT id FROM c2 WHERE h = CAST(35.6812363 AS FLOAT) ORDER BY id",
			"SELECT id FROM c2 IGNORE INDEX (kh, kk) WHERE h = CAST(35.6812363 AS FLOAT) ORDER BY id",
			"UPDATE c2 SET d = 9 WHERE h = 35.6812363",
			"SELECT id, d FROM c2 ORDER BY id",
			// B. FLOAT と DOUBLE の UNION は DOUBLE か FLOAT（前は (M,D) か UNSIGNED の列と DOUBLE が文字列になり、文字列の順に並んだ）
			"SELECT ctime FROM wfHits UNION ALL SELECT net_total FROM wc_order_stats ORDER BY 1",
			"SELECT ctime FROM wfHits UNION ALL SELECT 1 ORDER BY 1",
			"SELECT f FROM wfHits UNION ALL SELECT i FROM wc_order_stats ORDER BY 1",
			"SELECT f FROM wfHits UNION ALL SELECT fl FROM wc_order_stats ORDER BY 1",
			"SELECT u FROM wfHits UNION SELECT net_total FROM wc_order_stats ORDER BY 1",
			"SELECT f FROM wfHits UNION ALL SELECT ctime FROM wfHits ORDER BY 1",
			"SELECT net_total FROM wc_order_stats UNION ALL SELECT fl FROM wc_order_stats ORDER BY 1",
			"SELECT MAX(x) FROM (SELECT ctime AS x FROM wfHits UNION ALL SELECT net_total FROM wc_order_stats) t",
			"CREATE TABLE x1 AS SELECT ctime FROM wfHits UNION ALL SELECT net_total FROM wc_order_stats",
			"CREATE TABLE x4 AS SELECT f FROM wfHits UNION ALL SELECT fl FROM wc_order_stats",
			"CREATE TABLE x5 AS SELECT u FROM wfHits UNION ALL SELECT net_total FROM wc_order_stats",
			"CREATE TABLE x7 AS SELECT net_total FROM wc_order_stats UNION ALL SELECT fl FROM wc_order_stats",
			"SELECT TABLE_NAME, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN ('x1', 'x4', 'x5', 'x7') ORDER BY TABLE_NAME",
			"DROP TABLE x1",
			"DROP TABLE x4",
			"DROP TABLE x5",
			"DROP TABLE x7",
			// C. 端は丸めた倍精度の値で確かめる（前は単精度で比べ、FLOAT(10,7) に 1000 が入った）
			"INSERT INTO e VALUES (1, 1000, 1)",
			"INSERT INTO e VALUES (2, -1000, 1)",
			"INSERT INTO e VALUES (3, 999.99999999, 1)",
			"INSERT INTO e VALUES (4, 999.9999999, 999.99)",
			"INSERT INTO e VALUES (5, 999.99999994, 999.994)",
			wpSQLMode,
			"INSERT INTO e VALUES (6, 1000, 999.995)",
			"SHOW WARNINGS",
			"SET SESSION sql_mode = DEFAULT",
			"SELECT * FROM e ORDER BY id",
			"UPDATE e SET h = h + 0.0000001 WHERE id = 4",
			"UPDATE e SET d = 1 WHERE id = 6",
			"INSERT INTO e SELECT id + 10, h, d FROM e",
			"INSERT INTO e SELECT 20, h + 0, d FROM e WHERE id = 6",
			"SELECT * FROM e ORDER BY id",
		},
		drop: []string{"DROP TABLE e", "DROP TABLE wc_order_stats", "DROP TABLE wfHits", "DROP TABLE c2"},
	}, {
		name: "review2", // 5. 段階 120 の 2 回目のレビュー: FLOAT と DOUBLE の列の結合の索引の lookup のキー、WHERE の定数を列の型にする範囲
		setup: []string{
			"CREATE TABLE fa (id INT PRIMARY KEY, h FLOAT(10,7), k FLOAT(11,4), f FLOAT, d DOUBLE, dm DOUBLE(10,3), dec1 DECIMAL(12,7), i INT)",
			"CREATE TABLE fi (id INT PRIMARY KEY, h FLOAT(10,7), k FLOAT(11,4), f FLOAT, d DOUBLE, dm DOUBLE(10,3), dec1 DECIMAL(12,7), i INT, KEY (h), KEY (k), KEY (f), KEY (d), KEY (dm), KEY (dec1), KEY (i))",
			"INSERT INTO fa VALUES (1, 35.6812363, 35.6812363, 35.6812363, 35.6812363, 35.6812363, 35.6812363, 35), (2, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0), (3, 1.55, 1.55, 1.55, 1.55, 1.55, 1.55, 2), " +
				"(4, -35.6812363, -35.6812363, -35.6812363, -35.6812363, -35.6812363, -35.6812363, -35), (5, NULL, NULL, NULL, NULL, NULL, NULL, NULL), (6, 100, 100, 100, 100, 100, 100, 100)",
			"INSERT INTO fi SELECT * FROM fa",
		},
		stmts: []string{
			// 1. 結合の索引の lookup で、列の型にすると値の変わるキーはどの行にも合わない（索引のない fa と同じ）
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.dec1 = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.dec1 = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.d = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.d = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.i = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.i = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.d = b.h ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.d = b.h ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.dec1 = b.f ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.dec1 = b.f ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.d = b.f ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.d = b.f ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.h = b.h ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.h = b.h ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.i = b.d ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.i = b.d ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.dec1 = b.d ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.dec1 = b.d ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.f = b.d ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.f = b.d ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.k = b.k ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.k = b.k ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fi b ON a.dm = b.dm ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM fa a JOIN fa b ON a.dm = b.dm ORDER BY a.id, b.id",
			"SELECT id FROM fa WHERE h BETWEEN 35.6812363 AND 36 ORDER BY id",
			"SELECT id FROM fi WHERE h BETWEEN 35.6812363 AND 36 ORDER BY id",
			"SELECT id FROM fa WHERE h NOT BETWEEN 35.6812363 AND 36 ORDER BY id",
			"SELECT id FROM fa WHERE h = 35.6812363e0 ORDER BY id",
			"SELECT id FROM fi WHERE h = 35.6812363e0 ORDER BY id",
			"SELECT id FROM fa WHERE h >= 35.6812363e0 ORDER BY id",
			"SELECT id FROM fi WHERE h >= 35.6812363e0 ORDER BY id",
			"SELECT id FROM fa WHERE h IN (35.6812363) ORDER BY id",
			"SELECT id FROM fi WHERE h IN (35.6812363) ORDER BY id",
			"SELECT id FROM fa WHERE h = 1.55e0 ORDER BY id",
			"SELECT id FROM fi WHERE h = 1.55e0 ORDER BY id",
			"SELECT id FROM fa WHERE h IN (35.6812363, 1.55) ORDER BY id",
			"SELECT id FROM fi WHERE h IN (35.6812363, 1.55) ORDER BY id",
			"SELECT id FROM fa WHERE h = 1.55 ORDER BY id",
			"SELECT id FROM fi WHERE h = 1.55 ORDER BY id",
			"SELECT id FROM fa WHERE h = CONCAT('35.6812363', '') ORDER BY id",
			"SELECT id FROM fi WHERE h = CONCAT('35.6812363', '') ORDER BY id",
			"SELECT id FROM fa WHERE h = '35.6812363' ORDER BY id",
			"SELECT id FROM fi WHERE h = '35.6812363' ORDER BY id",
			"SELECT id FROM fa WHERE NOT (h < 35.6812363) AND id < 5 ORDER BY id",
			"SELECT id FROM fi WHERE NOT (h < 35.6812363) AND id < 5 ORDER BY id",
			"SELECT id FROM fa WHERE h = 35.6812363 OR h = 0.1 ORDER BY id",
			"SELECT id FROM fi WHERE h = 35.6812363 OR h = 0.1 ORDER BY id",
			"SELECT id FROM fi WHERE dm IN (SELECT dec1 FROM fa) ORDER BY id",
			"SELECT id FROM fi WHERE f IN (SELECT d FROM fa) ORDER BY id",
			// 2、3. WHERE の直接の比べのリテラルだけを列の型にする（BETWEEN、CASE、IF、定数の式、2 つ以上の値の IN はそのまま）
			"/* rows */ SELECT id, CASE h WHEN 35.6812363 THEN 'a' ELSE 'b' END, IF(h = 1.55e0, 'y', 'n') FROM fa ORDER BY id",
			"UPDATE fa SET i = 7 WHERE h BETWEEN 35.6812363 AND 36",
			"UPDATE fa SET i = 8 WHERE h = 35.6812363e0",
			"DELETE FROM fi WHERE h IN (1.55)",
			"SELECT id, i FROM fa ORDER BY id",
			"SELECT id FROM fi ORDER BY id",
		},
		drop: []string{"DROP TABLE fi", "DROP TABLE fa"},
	},
}

// stage120Statements は、組をつないだ文の列。
func stage120Statements() []string {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage120Groups {
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

// 段階 120 のゴールデンは、値を送られた文字列のまま比べる（runScriptRaw）。go-sql-driver はテキストプロトコルの FLOAT と
// DOUBLE を数に読み直すので、D 桁の表示（1.50、35.6812363）を比べられない。
func TestStage120Golden(t *testing.T) {
	checkGoldenRun(t, "../testdata/stage120_golden.txt", "stage120_golden", stage120Statements(), runScriptRaw)
}

// runScriptRaw は、dsn（go-sql-driver の形）の 1 本の接続で stmts を流し、結果を、送られた値の文字列のまま、行と列の
// 型と小数の桁（MySQL の列の情報の Decimals）にする（vitess の MySQL クライアント。段階 120）。エラーは MySQL の
// エラー番号、行を返さない文は影響行数。
func runScriptRaw(t *testing.T, dsn string, stmts []string) []string {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	conn, err := vmysql.Connect(context.Background(), &vmysql.ConnParams{Host: host, Port: p, Uname: cfg.User, Pass: cfg.Passwd, DbName: cfg.DBName})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var out []string
	for _, q := range stmts {
		out = append(out, rawResult(conn, q))
		if afterStatement != nil {
			afterStatement(q)
		}
	}
	return out
}

func rawResult(conn *vmysql.Conn, q string) string {
	// 「/* rows */」で始まる文は、列の型と小数の桁を比べない（この段階で変えていない型の違い。docs/differences.md）
	rowsOnly := strings.HasPrefix(q, "/* rows */ ")
	upper := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(q, "/* rows */ ")))
	r, err := conn.ExecuteFetch(q, 10000, true)
	if err != nil {
		if se, ok := err.(*vmysql.SQLError); ok {
			return fmt.Sprintf("error %d", se.Number())
		}
		return "error " + err.Error()
	}
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "SHOW") && !strings.HasPrefix(upper, "DESCRIBE") {
		if strings.HasPrefix(upper, "ALTER ") || strings.HasPrefix(upper, "SET ") {
			return "ok" // sqlcompare.Run と同じく、影響行数は比べない
		}
		return fmt.Sprintf("ok affected=%d", r.RowsAffected)
	}
	var b strings.Builder
	if strings.HasPrefix(upper, "SELECT") && !strings.Contains(upper, "INFORMATION_SCHEMA") && !rowsOnly {
		cols := make([]string, len(r.Fields))
		for i, f := range r.Fields {
			cols[i] = fmt.Sprintf("%s/%d", f.Type, f.Decimals)
		}
		b.WriteString("# " + strings.Join(cols, " ") + "\n")
	}
	for _, row := range r.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			if v.IsNull() {
				parts[i] = "NULL"
			} else {
				parts[i] = v.ToString()
			}
		}
		b.WriteString(strings.Join(parts, " | ") + "\n")
	}
	return b.String()
}

// 段階 120: FLOAT(M,D) と DOUBLE(M,D) の列と式の列の情報の Decimals は、MySQL と同じく D（式は MySQL の桁）。前は 31
// （段階 118 で直した NOT_FIXED_DEC）で、mysqlnd などは桁の決まらない値として読んだ。期待の値は本物の MySQL 8.0 で
// go-sql-driver の DecimalSize を見て確かめた。
func TestStage120RealDecimals(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE fd (id INT PRIMARY KEY, f FLOAT(7,2), d DOUBLE(10,3), u FLOAT(5,1) UNSIGNED, p FLOAT)")
	mustExec(t, db, "INSERT INTO fd VALUES (1, 1.5, 1.5, 1.5, 1.5)")
	const q = "SELECT f, d, u, f + 0, -u, f / 3, SUM(f), AVG(f), IF(id, f, d), MAX(f), COALESCE(f, d), ABS(f), f * d, f % 2, f + p FROM fd WHERE id >= ? GROUP BY id, f, d, u, p"
	notFixed := int64(math.MaxInt64)
	want := []int64{2, 3, 1, 2, 1, 6, 2, 6, 3, 2, 3, 2, 3, 2, notFixed}
	for _, binary := range []bool{false, true} {
		var rows *sql.Rows
		if binary {
			rows, err = db.Query(q, 1) // 引数のある Query はバイナリプロトコル
		} else {
			rows, err = db.Query(q[:len(q)-len("WHERE id >= ? GROUP BY id, f, d, u, p")] + "GROUP BY id, f, d, u, p")
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

// 段階 120: 新しい表の SET と BIT の索引はキーの水準 2 でエントリを持ち、= と IN（SET）と範囲（BIT）をエントリで引く。
// SET の大小の比べは MySQL と同じく範囲にしない（全件を読んで比べる側で決める）。
func TestStage120SetBitEntries(t *testing.T) {
	dir := t.TempDir()
	addr, _, st, srv := serve(t, dir, serverConfig{})
	defer stopServer(t, st, srv)()
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE sb (id INT PRIMARY KEY, s SET('a','b','c'), b BIT(8), b64 BIT(64), KEY ks (s), KEY kb (b), KEY kb64 (b64))")
	mustExec(t, db, "INSERT INTO sb VALUES (1, 'a', 5, 1), (2, 'a,c', 255, 18446744073709551615), (3, '', 0, 9223372036854775808), (4, 'b,c', 16, 0), (5, NULL, NULL, NULL), (6, 'a', 6, 2)")
	def, _, err := st.GetTable(store.MainBranchID, "sb", store.Latest)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range def.Indexes {
		if idx.KeyLevel != 2 {
			t.Errorf("%s: level %d", idx.Name, idx.KeyLevel)
		}
	}
	for _, c := range []struct{ q, want, plan string }{
		{"SELECT id FROM sb WHERE s = 'a' ORDER BY id", "1,6", "[1, 1]"},
		{"SELECT id FROM sb WHERE s IN ('a,c', 'b,c') ORDER BY id", "2,4", "[5, 5]"},
		{"SELECT id FROM sb WHERE b > 5 ORDER BY id", "2,4,6", "(5, ∞)"},
		{"SELECT id FROM sb WHERE b IN (5, 16) ORDER BY id", "1,4", "[16, 16]"},
		{"SELECT id FROM sb WHERE b64 >= 9223372036854775808 ORDER BY id", "2,3", "[9223372036854775808, ∞)"},
	} {
		before := entriesDecoded.Load()
		if got := strings.Join(queryRows(t, db, c.q, false), ","); got != c.want {
			t.Errorf("%s: %s, want %s", c.q, got, c.want)
		}
		if n := entriesDecoded.Load() - before; n == 0 || n > 3 {
			t.Errorf("%s: %d entries read", c.q, n)
		}
		if plan := explain(t, db, c.q); !strings.Contains(plan, c.plan) {
			t.Errorf("%s: plan\n%s", c.q, plan)
		}
	}
	// SET の大小は範囲にしない（NULL でない行をすべて読む）
	if plan := explain(t, db, "SELECT id FROM sb WHERE s > 'a'"); !strings.Contains(plan, "(NULL, ∞)") {
		t.Errorf("s > 'a': plan\n%s", plan)
	}
}
