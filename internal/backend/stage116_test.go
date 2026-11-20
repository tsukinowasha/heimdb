package backend

import (
	"database/sql"
	"math"
	"strings"
	"testing"
)

// stage116Group は、段階 116 で直した値の違いと前からの違いの再現の文（docs/plan.md の段階 116）。直す前の HeimDB は
// 本物の MySQL 8.0 と違う結果かエラーを返した。setup で表を作り、both を既定の sql_mode と WordPress の sql_mode の
// 両方で流し（答えが sql_mode で変わる文があるので）、stmts をそのあとに流し（sql_mode は文の中で決める）、drop で
// 表を消す。
type stage116Group struct {
	name  string
	setup []string
	both  []string
	stmts []string
	drop  []string
}

var stage116Groups = []stage116Group{
	{
		name: "sum", // 1. 整数と DECIMAL の SUM は DECIMAL（桁 +22）、AVG は小数の桁 +4 の DECIMAL（前は DOUBLE で、桁と表示を失った）
		setup: []string{
			"CREATE TABLE s8 (id INT PRIMARY KEY, d DECIMAL(26,4), i INT, b BIGINT, bu BIGINT UNSIGNED, dec1 DECIMAL(10,2), f DOUBLE, ti TINYINT, y YEAR)",
			"INSERT INTO s8 VALUES (1, 99999.5000, 999999, 9007199254740993, 18446744073709551615, 10.50, 1.5, 1, 2024), " +
				"(2, 900000.2500, 1, 1, 1, 0.30, 2.5, 2, 2025), (3, 9000000.1234, 2147483647, 9007199254740993, 2, 1.00, 3.5, 3, 2026)",
			// WooCommerce の表（net_total は DOUBLE、num_items_sold は INT、価格の表は DECIMAL(19,4)）
			wpSQLMode,
			"CREATE TABLE wp_wc_order_stats (order_id BIGINT UNSIGNED NOT NULL PRIMARY KEY, parent_id BIGINT UNSIGNED NOT NULL DEFAULT 0, date_created DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', " +
				"num_items_sold INT NOT NULL DEFAULT 0, total_sales DOUBLE NOT NULL DEFAULT 0, net_total DOUBLE NOT NULL DEFAULT 0, returning_customer TINYINT(1) DEFAULT NULL, status VARCHAR(200) NOT NULL, " +
				"customer_id BIGINT UNSIGNED NOT NULL, KEY date_created (date_created), KEY status (status(191)))",
			"INSERT INTO wp_wc_order_stats VALUES (10, 0, '2024-01-01 10:00:00', 2, 30.1, 25.1, 0, 'wc-completed', 1), (11, 0, '2024-01-02 10:00:00', 1, 0.2, 0.1, 1, 'wc-completed', 1), " +
				"(12, 0, '2024-01-02 11:00:00', 3, 1234567.89, 1234567.79, 0, 'wc-processing', 2)",
			"CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL PRIMARY KEY, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, " +
				"total_sales BIGINT NULL DEFAULT 0, stock_quantity DOUBLE NULL DEFAULT NULL, KEY min_max_price (min_price, max_price))",
			"INSERT INTO wp_wc_product_meta_lookup VALUES (1, 10.0000, 12.5000, 3, 5), (2, 9.9950, 9.9950, 0, NULL), (3, 12345678901234.5678, 12345678901234.5678, 9007199254740993, 1)",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT SUM(d) FROM s8 WHERE id = 1",
			"SELECT SUM(d) FROM s8 WHERE id = 3",
			"SELECT SUM(d), SUM(i), SUM(b), SUM(bu), SUM(dec1), SUM(f), SUM(ti), SUM(y) FROM s8",
			"SELECT AVG(d), AVG(i), AVG(b), AVG(bu), AVG(dec1), AVG(f), AVG(ti) FROM s8",
			"SELECT id, SUM(d), SUM(i), AVG(dec1) FROM s8 GROUP BY id ORDER BY id",
			"SELECT id, SUM(d) FROM s8 GROUP BY id HAVING SUM(d) > 9000000 ORDER BY id",
			"SELECT ROUND(SUM(d), 2), ROUND(SUM(d), 0), SUM(d) * 1, SUM(d) + 0, CAST(SUM(d) AS DECIMAL(26,4)), SUM(d) = 9999999.8734, CAST(SUM(d) AS CHAR), CONCAT(SUM(dec1)), CONCAT(AVG(dec1)) FROM s8",
			"SELECT SUM(i) + 1, SUM(i) / 2, SUM(i) DIV 2, SUM(i) / COUNT(*), SUM(i) * 1.5, ROUND(AVG(i)), FLOOR(AVG(dec1)), FORMAT(SUM(d), 2) FROM s8",
			"SELECT MAX(d), MIN(d), SUM(DISTINCT d), AVG(DISTINCT i), SUM(DISTINCT b) FROM s8",
			"SELECT SUM(CAST(d AS CHAR)), SUM(id * 1.5), SUM(id) / 3, SUM(id > 1), AVG(id > 1), SUM(IF(id > 1, 1, 0)), SUM(CASE WHEN id > 1 THEN 1 ELSE 0 END) FROM s8",
			"SELECT SUM(NULL), AVG(NULL), SUM(i), AVG(d) FROM s8 WHERE id > 100",
			"SELECT SUM(x), AVG(x) FROM (SELECT 9223372036854775807 AS x UNION ALL SELECT 9223372036854775807 UNION ALL SELECT -5) q",
			"SELECT SUM(x) FROM (SELECT -9223372036854775807 AS x UNION ALL SELECT -9223372036854775807 UNION ALL SELECT -9) q",
			"SELECT SUM(x) FROM (SELECT d AS x FROM s8 UNION ALL SELECT dec1 FROM s8) q",
			"SELECT SUM(x) FROM (SELECT 1234567890.1000 AS x UNION ALL SELECT 10.5) q",
			"SELECT x FROM (SELECT dec1 AS x FROM s8 WHERE id = 1 UNION ALL SELECT d FROM s8 WHERE id = 1 UNION ALL SELECT i FROM s8 WHERE id = 2) q ORDER BY x",
			// ウィンドウ関数
			"SELECT id, SUM(d) OVER (), SUM(dec1) OVER (ORDER BY id), AVG(dec1) OVER (ORDER BY id), SUM(i) OVER (ORDER BY id), AVG(i) OVER (), SUM(bu) OVER (), SUM(f) OVER () FROM s8 ORDER BY id",
			"SELECT id, SUM(dec1) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), AVG(d) OVER (PARTITION BY id), SUM(b) OVER (ORDER BY id) FROM s8 ORDER BY id",
			// WooCommerce の集計の形
			"SELECT SUM(num_items_sold) AS items_sold, SUM(net_total) AS net_revenue, AVG(net_total) AS avg_order_value, COUNT(*) AS orders_count, SUM(total_sales) FROM wp_wc_order_stats WHERE status NOT IN ('wc-trash', 'wc-pending', 'wc-failed', 'wc-cancelled')",
			"SELECT DATE(date_created) AS d, SUM(num_items_sold), SUM(net_total), AVG(num_items_sold), SUM(returning_customer) FROM wp_wc_order_stats GROUP BY d ORDER BY d",
			"SELECT SUM(min_price), AVG(max_price), SUM(total_sales), AVG(total_sales), SUM(stock_quantity), MAX(min_price) FROM wp_wc_product_meta_lookup",
			"SELECT product_id, SUM(max_price) - SUM(min_price), AVG(min_price) FROM wp_wc_product_meta_lookup GROUP BY product_id ORDER BY product_id",
		},
		stmts: []string{
			// 結果の型（CREATE TABLE ... SELECT の列の型）
			"CREATE TABLE ty AS SELECT SUM(i) a, SUM(b) bb, SUM(d) c, AVG(d) dd, AVG(i) e, AVG(b) f, SUM(bu) g, SUM(dec1) h, AVG(dec1) k, SUM(f) l, AVG(f) m, SUM(ti) n, SUM(y) o, " +
				"SUM(CAST(id AS CHAR)) p, SUM(1.5) q, AVG(1.5) r FROM s8",
			"SHOW COLUMNS FROM ty",
			"DROP TABLE ty",
		},
		drop: []string{"DROP TABLE wp_wc_product_meta_lookup", "DROP TABLE wp_wc_order_stats", "DROP TABLE s8"},
	},
	{
		name: "round", // 2. ROUND の型（文字列は DOUBLE、DECIMAL は丸めた桁）、DOUBLE の丸め（rint）、TRUNCATE()
		setup: []string{
			"CREATE TABLE n4 (id INT PRIMARY KEY, i INT, u INT UNSIGNED, ti TINYINT, bu BIGINT UNSIGNED, dec1 DECIMAL(10,2), dec2 DECIMAL(26,4), f DOUBLE, s VARCHAR(20))",
			"INSERT INTO n4 VALUES (1, 1, 1, 1, 1, 10.50, 10.5000, 2.55, '2.55'), (2, 2, 2, 2, 2, 0.30, 1234567890.1000, 1.005, '1.005'), (3, 3, 3, 3, 3, 1.00, 2.0000, 2.45, 'x')",
			"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, " +
				"KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_price', '2.55'), (2, '_price', '10.005'), (3, '_price', '19.995'), (1, '_wc_average_rating', '4.45'), (2, '_wc_average_rating', '3.5')",
		},
		both: []string{
			"SELECT ROUND('2.55', 1), ROUND(2.55e0, 1), ROUND(2.55, 1), ROUND('2.45', 1), ROUND(2.45e0, 1), ROUND('1.005', 2), ROUND(1.005e0, 2), ROUND('2.5'), ROUND(2.5e0), ROUND(-2.5e0), ROUND('-2.55', 1), ROUND(0.285e0, 2), ROUND('0.285', 2)",
			"SELECT ROUND(f, 1), ROUND(s, 1), ROUND(dec1, 1), ROUND(f, 2), ROUND(s, 2), ROUND(f), ROUND(f * 10) FROM n4 ORDER BY id",
			"SHOW WARNINGS",
			"SELECT ROUND(2.675e0, 2), ROUND(1.15e0, 1), ROUND(1.25e0, 1), ROUND(1.35e0, 1), ROUND(5.015e0, 2), ROUND(1e15 + 0.5), ROUND(123.456e0, -1), ROUND('123.456', -2), ROUND(2.55e0 * 100) / 100",
			"SELECT ROUND(dec2, 2), ROUND(dec2, -2), ROUND(i, -1), ROUND(bu, 1), ROUND(dec1), ROUND(dec1, 5), ROUND(i, 2), ROUND(ti, -5) FROM n4 ORDER BY id",
			"SELECT ROUND(f, id), ROUND(dec1, id), ROUND(s, id), ROUND(i, id - 2), ROUND(dec2, id - 3) FROM n4 ORDER BY id",
			"SELECT ROUND(NULL), ROUND(1.5, NULL), ROUND('abc', 1), ROUND('', 1), ROUND(' 2.55 ', 1), ROUND('2.55x', 1), ROUND(9.995, 2), ROUND(-9.995, 2), ROUND(0.5), ROUND(-0.5), ROUND(1.5e0), ROUND(0.5e0)",
			"SHOW WARNINGS",
			"SELECT ROUND(2.55, 1) + 0, ROUND('2.55', 1) + 0, CONCAT(ROUND(2.5e0)), CONCAT(ROUND(f, 1)), CONCAT(ROUND(dec2, 1)), ROUND(1e308, 2), ROUND(1.5e300, -300), ROUND(123.45e0, 400) FROM n4 WHERE id = 1",
			"SELECT TRUNCATE(1.235, 2), TRUNCATE(-1.235, 2), TRUNCATE(123.456, -1), TRUNCATE(1.999, 0), TRUNCATE('1.999', 1), TRUNCATE(1.999e0, 1), TRUNCATE(10, 2), TRUNCATE(NULL, 1), TRUNCATE(1.5, NULL)",
			"SELECT TRUNCATE(dec1, 1), TRUNCATE(f, 1), TRUNCATE(s, 1), TRUNCATE(i, 0), TRUNCATE(i, -1), TRUNCATE(dec2, 2) FROM n4 ORDER BY id",
			"SELECT TRUNCATE(f, id), TRUNCATE(dec2, -2), TRUNCATE(bu, -1), TRUNCATE(-2.999e0, 2), TRUNCATE(1e20, -5), TRUNCATE(0.1 + 0.2, 20), TRUNCATE(123.4567e0, 3), TRUNCATE(-123.4567, -2), TRUNCATE(dec1, id) FROM n4 ORDER BY id",
			// WordPress と WooCommerce の形（メタの値は文字列なので DOUBLE で丸める）
			"SELECT post_id, ROUND(meta_value, 2), ROUND(meta_value, 1), ROUND(meta_value), TRUNCATE(meta_value, 1) FROM wp_postmeta WHERE meta_key = '_price' ORDER BY post_id",
			"SELECT ROUND(AVG(meta_value), 1), ROUND(AVG(CAST(meta_value AS DECIMAL(10,2))), 2), ROUND(SUM(meta_value), 2) FROM wp_postmeta WHERE meta_key = '_wc_average_rating'",
		},
		stmts: []string{
			wpSQLMode,
			"CREATE TABLE ty AS SELECT ROUND(s, 1) a, ROUND(dec1, 1) b, ROUND(dec2, -2) c, ROUND(f, 1) d, ROUND(i, -1) e, TRUNCATE(dec2, 2) f, TRUNCATE(s, 1) g, ROUND(dec1, id) h FROM n4",
			"SHOW COLUMNS FROM ty",
			"DROP TABLE ty",
		},
		drop: []string{"DROP TABLE wp_postmeta", "DROP TABLE n4"},
	},
	{
		name: "numstr", // 3. 数の列と文字列: BIGINT UNSIGNED と '' の比べ、lookup 結合の整数の列のキー（文字列と DOUBLE は DOUBLE で比べる）、2 つ以上の値の IN の日時でない文字列
		setup: []string{
			wpSQLMode,
			"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', dd DATE, PRIMARY KEY (ID), KEY pd (post_date), KEY dd (dd))",
			"CREATE TABLE pn (ID INT PRIMARY KEY, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', dd DATE)",
			"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255), meta_value LONGTEXT, PRIMARY KEY (meta_id), KEY post_id (post_id))",
			"CREATE TABLE u32 (id INT UNSIGNED NOT NULL PRIMARY KEY, i INT NOT NULL, ti TINYINT UNSIGNED NOT NULL, KEY i (i), KEY ti (ti))",
			"INSERT INTO p (post_date, dd) VALUES ('2024-01-01 10:00:00', '2024-01-01'), ('0000-00-00 00:00:00', '0000-00-00'), ('2024-06-01 00:00:00', '2024-06-01')",
			"INSERT INTO pn VALUES (1, '2024-01-01 10:00:00', '2024-01-01'), (2, '0000-00-00 00:00:00', '0000-00-00'), (3, '2024-06-01 00:00:00', '2024-06-01')",
			"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, 'a', ''), (2, 'b', '4294967295'), (3, 'c', '1.0000000000000000001'), (0, 'd', 'x'), (1, 'e', '1'), " +
				"(2, 'f', '-1'), (3, 'g', '1.5'), (1, 'h', '255'), (1, 'i', '256'), (1, 'j', ' 2 '), (1, 'k', '2abc'), (1, 'l', '1e0'), (1, 'm', '4294967296'), (1, 'n', '0.0')",
			"INSERT INTO u32 VALUES (0, 0, 0), (1, 1, 1), (2, 2, 2), (4294967295, -1, 255)",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			// BIGINT UNSIGNED と INT UNSIGNED の列と ''（MySQL は '' を 0 として比べる。前は 1366）
			"SELECT meta_id FROM pm WHERE post_id = '' ORDER BY meta_id",
			"SELECT meta_id FROM pm IGNORE INDEX (post_id) WHERE post_id = '' ORDER BY meta_id",
			"SELECT COUNT(*) FROM pm WHERE post_id IN ('', '1')",
			"SELECT COUNT(*) FROM pm WHERE post_id <> ''",
			"SELECT meta_id FROM pm WHERE post_id > '' ORDER BY meta_id",
			"SELECT meta_id FROM pm WHERE post_id = ' ' ORDER BY meta_id",
			"SELECT meta_id FROM pm WHERE post_id = 'abc' ORDER BY meta_id",
			"SELECT id FROM u32 WHERE id = '' ORDER BY id",
			"SELECT id FROM u32 WHERE ti = '' ORDER BY id",
			"SELECT id FROM u32 WHERE id IN ('', 'x', '2') ORDER BY id",
			// lookup 結合の整数の列のキー（ハッシュ結合と同じ結果）
			"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = pm.meta_value ORDER BY pm.meta_id",
			"SELECT /*+ HASH_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = pm.meta_value ORDER BY pm.meta_id",
			"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = pm.meta_value + 0.0 ORDER BY pm.meta_id",
			"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.i = pm.meta_value ORDER BY pm.meta_id",
			"SELECT /*+ HASH_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.i = pm.meta_value ORDER BY pm.meta_id",
			"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.ti = pm.meta_value ORDER BY pm.meta_id",
			"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = CAST(pm.meta_value AS DOUBLE) ORDER BY pm.meta_id",
			"SELECT /*+ HASH_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = CAST(pm.meta_value AS DOUBLE) ORDER BY pm.meta_id",
			"SELECT /*+ LOOKUP_JOIN(pm, p) */ pm.meta_id, p.ID FROM pm JOIN p ON p.ID = pm.meta_value ORDER BY pm.meta_id",
			"SELECT /*+ LOOKUP_JOIN(pm, p) */ pm.meta_id, p.ID FROM pm LEFT JOIN p ON p.ID = pm.meta_value ORDER BY pm.meta_id",
			"SELECT meta_id FROM pm WHERE meta_value IN (SELECT id FROM u32) ORDER BY meta_id",
			"SELECT id FROM u32 WHERE id IN (SELECT meta_value FROM pm) ORDER BY id",
			"SELECT meta_id FROM pm WHERE EXISTS (SELECT 1 FROM u32 WHERE u32.id = pm.meta_value) ORDER BY meta_id",
			// 2 つ以上の値の IN と NOT IN の日時として読めない文字列（MySQL はゼロの日時として比べ、警告 1292）
			"SELECT ID FROM p WHERE post_date IN ('garbage', '2024-01-01 10:00:00') ORDER BY ID",
			"SELECT ID FROM p IGNORE INDEX (pd) WHERE post_date IN ('garbage', '2024-01-01 10:00:00') ORDER BY ID",
			"SELECT ID FROM pn WHERE post_date IN ('garbage', '2024-01-01 10:00:00') ORDER BY ID",
			"SELECT ID FROM p WHERE post_date IN ('2024-01-01 10:00:00', '') ORDER BY ID",
			"SELECT ID FROM pn WHERE post_date IN ('', '2024-06-01') ORDER BY ID",
			"SELECT ID FROM p WHERE post_date NOT IN ('garbage', '2024-01-01 10:00:00') ORDER BY ID",
			"SELECT ID FROM pn WHERE post_date NOT IN ('garbage', '2024-01-01 10:00:00') ORDER BY ID",
			"SELECT ID FROM p WHERE dd IN ('garbage', '2024-01-01') ORDER BY ID",
			"SELECT ID FROM pn WHERE dd IN ('garbage', '2024-01-01') ORDER BY ID",
			"SELECT ID FROM p WHERE post_date IN ('garbage', 'junk') ORDER BY ID",
			"SELECT ID FROM p WHERE post_date IN ('2024-01-01 10:00:00x', '2024-06-01') ORDER BY ID",
			"SELECT ID, post_date IN ('garbage', '2024-01-01 10:00:00'), post_date NOT IN ('', 'x') FROM pn ORDER BY ID",
			"SELECT ID FROM pn WHERE post_date BETWEEN 'garbage' AND '2024-01-01 10:00:00' ORDER BY ID",
		},
		drop: []string{"DROP TABLE u32", "DROP TABLE pm", "DROP TABLE pn", "DROP TABLE p"},
	},
	{
		name: "datenum", // 4. 数と日時: 日時と数の比べ（日時として読める数は日時で、ほかは数で）、数の CAST AS DATE と DATETIME、小数の付いた YYYYMMDD、余計な文字の付いたゼロの日付
		setup: []string{
			wpSQLMode,
			"CREATE TABLE dt (id INT PRIMARY KEY, d DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', d6 DATETIME(6), dd DATE, ts TIMESTAMP NULL, KEY d (d), KEY d6x (d6), KEY dd (dd), KEY ts (ts))",
			"CREATE TABLE dn (id INT PRIMARY KEY, d DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', d6 DATETIME(6), dd DATE, ts TIMESTAMP NULL)",
			"INSERT INTO dt VALUES (1, '2024-01-01 10:00:00', '2024-01-01 10:00:00.500000', '2024-01-01', '2024-01-01 10:00:00'), (2, '0000-00-00 00:00:00', NULL, '0000-00-00', NULL), " +
				"(3, '2024-06-01 00:00:01', '2024-06-01 00:00:00.999999', '2024-06-01', '2024-06-01 00:00:01'), (4, '2024-01-01 10:00:01', '2024-01-01 10:00:01.000000', '2024-01-02', '2024-01-01 10:00:01')",
			"INSERT INTO dn SELECT * FROM dt",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT id, d > 0, d < 0, d = 0, d <> 0, d >= 0, d <= 0, d > 0.0, d > 0e0, d > -1, d > 1, d > 20240101100000, d > 19000101000000 FROM dt ORDER BY id",
			"SELECT id, dd > 0, dd = 0, dd > 20240101, dd >= 20240101, dd = 20240101.5, d = 20240101100000.6, d = 20240101100000.4, d6 = 20240101100000.5, d6 > 20240101100000.4999995, ts > 0, ts = 20240101100000 FROM dt ORDER BY id",
			"SELECT id FROM dt WHERE d > 0 ORDER BY id",
			"SELECT id FROM dn WHERE d > 0 ORDER BY id",
			"SELECT id FROM dt WHERE d = 0 ORDER BY id",
			"SELECT id FROM dt WHERE d <> 0 ORDER BY id",
			"SELECT id FROM dt WHERE d > 20240101 ORDER BY id",
			"SELECT id FROM dt WHERE d >= 20240101100000 ORDER BY id",
			"SELECT id FROM dt WHERE d = 20240101100000 ORDER BY id",
			"SELECT id FROM dn WHERE d = 20240101100000 ORDER BY id",
			"SELECT id FROM dt WHERE d < 1 ORDER BY id",
			"SELECT id FROM dn WHERE d < 1 ORDER BY id",
			"SELECT id FROM dt WHERE d > 20241301 ORDER BY id",
			"SELECT id FROM dt WHERE d < 20240100 ORDER BY id",
			"SELECT id FROM dt WHERE d > 99999999999999 ORDER BY id",
			"SELECT id FROM dt WHERE d > 991231 ORDER BY id",
			"SELECT id FROM dt WHERE d > 240101 ORDER BY id",
			"SELECT id FROM dt WHERE dd > 0 ORDER BY id",
			"SELECT id FROM dt WHERE dd = 20240101 ORDER BY id",
			"SELECT id FROM dt WHERE dd > 20240101 ORDER BY id",
			"SELECT id FROM dt WHERE dd = 20240101.5 ORDER BY id",
			"SELECT id FROM dn WHERE dd = 20240101.5 ORDER BY id",
			"SELECT id FROM dt WHERE d = 20240101100000.6 ORDER BY id",
			"SELECT id FROM dn WHERE d = 20240101100000.6 ORDER BY id",
			"SELECT id FROM dt WHERE d = 20240101100000.4 ORDER BY id",
			"SELECT id FROM dt WHERE d6 > 0 ORDER BY id",
			"SELECT id FROM dt WHERE ts > 0 ORDER BY id",
			"SELECT id FROM dt WHERE ts > 20240101100000 ORDER BY id",
			"SELECT id FROM dt WHERE d > 0 + 0 ORDER BY id",
			"SELECT id FROM dt WHERE d > 20240101100000 + 0 ORDER BY id",
			"SELECT id FROM dt WHERE d > (SELECT 0) ORDER BY id",
			"SELECT id FROM dt WHERE d > id - id ORDER BY id",
			"SELECT id FROM dt WHERE 0 < d ORDER BY id",
			"SELECT id FROM dt WHERE d > 0 AND id > 0 ORDER BY id",
			"SELECT id FROM dt WHERE IF(d > 0, 1, 0) = 1 ORDER BY id",
			"SELECT id FROM dt WHERE DATE(d) > 0 ORDER BY id",
			"SELECT id FROM dt WHERE d > 0 OR d IS NULL ORDER BY id",
			"SELECT id FROM dt WHERE d BETWEEN 20240101 AND 20240102 ORDER BY id",
			"SELECT id FROM dt WHERE d IN (20240101100000, 0) ORDER BY id",
			"SELECT id FROM dt WHERE dd IN (20240101, 20240601) ORDER BY id",
			"SELECT id FROM dt WHERE d > CAST(0 AS DATETIME) ORDER BY id",
			// 数の CAST と日時の関数
			"SELECT CAST(0 AS DATETIME), CAST(20240101 AS DATETIME), CAST(20240101100000 AS DATETIME), CAST(1 AS DATETIME), CAST(20240101.5 AS DATE), DATE(20240101), CAST(0 AS DATE)",
			"SELECT CAST(20240101 AS DATE), CAST(240101 AS DATE), CAST(991231 AS DATE), CAST(700101 AS DATE), CAST(691231 AS DATE), CAST(101 AS DATE), CAST(100 AS DATE), CAST(20241301 AS DATE), CAST(20240230 AS DATE), CAST(-1 AS DATE)",
			"SELECT CAST(20240101103000 AS DATETIME), CAST(240101103000 AS DATETIME), CAST(20240101103000.123456 AS DATETIME(6)), CAST(20240101103000.5 AS DATETIME), CAST(20240101.5e0 AS DATETIME(1)), CAST(1e20 AS DATETIME), CAST(10000101 AS DATE), CAST(9991231 AS DATE)",
			"SELECT DATE(20240101), DATE(20240101103000), DATE(20240101.5), DATE_FORMAT(20240101.5, '%Y-%m-%d'), DATEDIFF(20240102.5, 20240101), DATE_FORMAT(20240101103000, '%H:%i'), YEAR(20240101), MONTH(240315), DAYOFWEEK(20240101), UNIX_TIMESTAMP(20240101)",
		},
		stmts: []string{
			// 余計な文字の付いたゼロの日付（既定の sql_mode の NO_ZERO_DATE では MySQL は 1525 か NULL。WordPress の sql_mode で比べる）
			wpSQLMode,
			"SELECT id FROM dt WHERE d = '0000-00-00 00:00:00x' ORDER BY id",
			"SELECT id FROM dn WHERE d = '0000-00-00 00:00:00x' ORDER BY id",
			"SELECT id FROM dt WHERE d = '0000-00-00x' ORDER BY id",
			"SELECT id FROM dt WHERE dd = '0000-00-00x' ORDER BY id",
			"SELECT id FROM dn WHERE dd = '0000-00-00x' ORDER BY id",
			"SELECT id FROM dt WHERE d IN ('0000-00-00 00:00:00x', '2024-01-01 10:00:00') ORDER BY id",
			"SELECT id FROM dt WHERE d > '0000-00-00 00:00:00x' ORDER BY id",
			"SELECT id FROM dn WHERE d > '0000-00-00 00:00:00x' ORDER BY id",
			"SELECT DATE(0), CAST(0.0 AS DATE), CAST(0 AS DATETIME)",
			"SELECT CAST('0000-00-00 00:00:00x' AS DATETIME), CAST('0000-00-00x' AS DATE), DATE('0000-00-00 00:00:00x'), '0000-00-00 00:00:00x' = CAST('0000-00-00' AS DATETIME)",
			"SELECT '0000-00-00 00:00:00x' = d FROM dt WHERE id = 2",
			"SELECT CAST('0000-00-00 00:00:00 ' AS DATETIME), CAST(' 0000-00-00' AS DATE), CAST('0000-00-00 00:00' AS DATETIME), CAST('0000-00-00 10:00:00' AS DATETIME), CAST('0000-00-001' AS DATE)",
			// 数を日時の列に入れる（MySQL は数を日時として読む）
			"CREATE TABLE di (id INT PRIMARY KEY, d DATETIME, dd DATE)",
			"INSERT INTO di VALUES (1, 20240101103000, 20240101), (2, 240101, 991231), (3, 20240101103000.5, 20240101.5), (4, 0, 0)",
			"SELECT * FROM di ORDER BY id",
			"INSERT INTO di VALUES (5, 1, 100)",
			"SELECT * FROM di ORDER BY id",
			"SET SESSION sql_mode = DEFAULT",
			"INSERT INTO di VALUES (6, 20240102, 20240103)",
			"INSERT INTO di VALUES (7, 1, NULL)",
			"SELECT * FROM di ORDER BY id",
			"DROP TABLE di",
		},
		drop: []string{"DROP TABLE dn", "DROP TABLE dt"},
	},
	{
		name: "datestr", // 5. 日時を文字列に: ゼロの日時が '0000-01-01'、DATE の列に時刻が付く（CONCAT、CAST AS CHAR、GROUP_CONCAT、LIKE、LENGTH、UNION）、CONCAT(TIME(...)) の 1105
		setup: []string{
			wpSQLMode,
			"CREATE TABLE v3 (id INT PRIMARY KEY, d DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', dd DATE, d3 DATETIME(3), t TIME, e ENUM('a','b','c') NOT NULL DEFAULT 'a', s VARCHAR(10))",
			"INSERT INTO v3 VALUES (1, '2024-01-01 10:00:00', '2024-01-01', '2024-01-01 10:00:00.250', '13:04:05', 'b', 'x'), (2, '0000-00-00 00:00:00', '0000-00-00', '0000-00-00 00:00:00', '-01:00:00', 'c', NULL)",
			"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_date_gmt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
			"INSERT INTO wp_posts (post_date, post_date_gmt, post_status) VALUES ('2024-01-01 10:00:00', '2024-01-01 01:00:00', 'publish'), ('2024-02-01 10:00:00', '0000-00-00 00:00:00', 'draft')",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT CONCAT(d), CAST(d AS CHAR), CONCAT(dd), CAST(dd AS CHAR), CONCAT(d, ''), CONCAT(d3), CAST(d3 AS CHAR) FROM v3 ORDER BY id",
			"SELECT CONCAT('x', d), LENGTH(d), LENGTH(dd), UPPER(dd), LEFT(d, 10), LEFT(dd, 4), REPLACE(dd, '-', ''), CHAR_LENGTH(d3) FROM v3 ORDER BY id",
			"SELECT CONCAT(DATE(d)), CONCAT(TIME(d)), CONCAT(t), DATE_FORMAT(d, '%Y-%m-%d %H:%i:%s') FROM v3 ORDER BY id",
			"SELECT d LIKE '2024%', dd LIKE '2024-01-01', d LIKE '0000%', dd LIKE '%-00' FROM v3 ORDER BY id",
			"SELECT GROUP_CONCAT(dd ORDER BY id), GROUP_CONCAT(d ORDER BY id), GROUP_CONCAT(e ORDER BY id), GROUP_CONCAT(d3 ORDER BY id) FROM v3",
			"SELECT COALESCE(dd, 'x'), IFNULL(dd, 'x'), CASE WHEN id = 1 THEN dd ELSE 'x' END, CASE WHEN id = 2 THEN d ELSE 'x' END FROM v3 ORDER BY id",
			"SELECT dd FROM v3 UNION ALL SELECT 'x'",
			"SELECT d FROM v3 UNION ALL SELECT 'x'",
			"SELECT dd FROM v3 UNION SELECT d FROM v3",
			"SELECT CONCAT(t), CONCAT('x', t), CAST(t AS CHAR), LENGTH(t), CONCAT(TIME('13:04:05')), TIME('13:04:05'), TIME('-01:00:00'), TIME('2024-01-01 13:04:05') FROM v3 ORDER BY id",
			"SELECT CONCAT_WS(',', d, dd), REPLACE(d, ' ', 'T'), SUBSTRING(dd, 1, 7), RIGHT(d, 8), INSTR(dd, '-01'), LOCATE('00:00', d), HEX(dd), REVERSE(dd), LPAD(dd, 12, '*') FROM v3 ORDER BY id",
			"SELECT IFNULL(NULL, dd), NULLIF(dd, '2024-01-01'), CONCAT(IFNULL(dd, '')) FROM v3 ORDER BY id",
			// WordPress の形（下書きの post_date_gmt はゼロの日時）
			"SELECT ID, CONCAT(post_date_gmt), LEFT(post_date_gmt, 4), post_date_gmt LIKE '0000%' FROM wp_posts ORDER BY ID",
			"SELECT ID FROM wp_posts WHERE post_date_gmt LIKE '0000-00-00%' ORDER BY ID",
			"SELECT ID, CONCAT(post_status, ':', post_date_gmt) FROM wp_posts ORDER BY ID",
			"SELECT YEAR(post_date), GROUP_CONCAT(post_date_gmt ORDER BY ID) FROM wp_posts GROUP BY YEAR(post_date)",
		},
		drop: []string{"DROP TABLE wp_posts", "DROP TABLE v3"},
	},
	{
		name: "datenum2", // 6. 日時を数に: CAST AS SIGNED と DECIMAL（前は UNIX 時刻）、ABS、dd + 0、TIME の算術
		setup: []string{
			wpSQLMode,
			"CREATE TABLE v6 (id INT PRIMARY KEY, d DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', dd DATE, d3 DATETIME(3), t TIME)",
			"INSERT INTO v6 VALUES (1, '2024-01-01 10:00:00', '2024-01-01', '2024-01-01 10:00:00.250', '13:04:05'), (2, '0000-00-00 00:00:00', '0000-00-00', '0000-00-00 00:00:00', '-01:00:00')",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT ABS(DATE(d)), ABS(dd), ABS(t), ABS(d), -dd, -t FROM v6 WHERE id = 1",
			"SELECT CAST(d AS SIGNED), CAST(dd AS SIGNED), CAST(t AS SIGNED), CAST(d AS UNSIGNED), CAST(DATE(d) AS SIGNED), CAST(t AS DECIMAL(10,2)), CAST(d AS DECIMAL(20,2)), CAST(d3 AS DECIMAL(20,3)), CAST(dd AS DOUBLE) FROM v6 ORDER BY id",
			"SELECT d + 0, dd + 0, t + 0, t + 1, dd * 1, d / 1, t - 1, FLOOR(t), ROUND(t), t DIV 1, t MOD 7, d3 + 0 FROM v6 ORDER BY id",
			"SELECT CAST('13:04:05' AS SIGNED), CAST(TIME('13:04:05') AS SIGNED), ABS(TIME('13:04:05')), TIME('13:04:05') + 0, CAST(DATE('2024-01-01') AS SIGNED), ABS(DATE('2024-01-01'))",
			"SELECT -t, CAST(dd AS DECIMAL(10,2)), CAST(t AS DOUBLE), dd + 0.5, t * 2, d - 1, YEAR(d) + 0, CAST(NOW() AS SIGNED) > 20240101000000, d - INTERVAL 1 HOUR FROM v6 ORDER BY id",
			"SELECT id FROM v6 WHERE d + 0 > 20240101000000 ORDER BY id",
			"SELECT id FROM v6 WHERE CAST(dd AS SIGNED) = 20240101 ORDER BY id",
			"SELECT SUM(d + 0), SUM(dd + 0), MAX(d + 0) FROM v6",
		},
		drop: []string{"DROP TABLE v6"},
	},
	{
		name: "mixed", // 7. IF、CASE、COALESCE、IFNULL、GREATEST、LEAST の型（日時か ENUM と数は文字列、DECIMAL は桁の大きい方）
		setup: []string{
			wpSQLMode,
			"CREATE TABLE v7 (id INT PRIMARY KEY, d DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', dd DATE, t TIME, e ENUM('a','b','c') NOT NULL DEFAULT 'a', s VARCHAR(10), dec1 DECIMAL(10,2))",
			"INSERT INTO v7 VALUES (1, '2024-01-01 10:00:00', '2024-01-01', '13:04:05', 'b', 'x', 10.50), (2, '0000-00-00 00:00:00', '0000-00-00', '-01:00:00', 'c', NULL, 0.30)",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT IF(id = 1, d, 0), IF(id = 1, 0, d), IF(id = 1, dd, 1), IF(id = 1, e, 1), IF(id = 1, 1, e), IF(id = 1, e, 'z'), IF(id = 1, d, 'z'), IF(id = 1, t, 0) FROM v7 ORDER BY id",
			"SELECT IF(1, '2024-01-01 10:00:00', 0), IF(0, '2024-01-01 10:00:00', 0), IF(0, NOW(), 0), IF(1, CURDATE(), 0) = CURDATE(), IF(0, CURDATE(), 0)",
			"SELECT IF(id = 2, d, 0), IF(id = 2, dd, 0), IF(id = 1, d, NULL), IF(id = 1, d, dd) FROM v7 ORDER BY id",
			"SELECT CASE WHEN id = 1 THEN d ELSE 0 END, CASE WHEN id = 1 THEN e ELSE 1 END, CASE WHEN id = 1 THEN dd ELSE 'x' END, COALESCE(e, 1), COALESCE(d, 0), COALESCE(NULL, e, 1) FROM v7 ORDER BY id",
			"SELECT IFNULL(s, d), IFNULL(s, e), IFNULL(s, 1), IF(s IS NULL, e, s), NULLIF(e, 'b'), GREATEST(e, 1), LEAST(e, 'b'), GREATEST(e, 'b') FROM v7 ORDER BY id",
			"SELECT SUM(CASE WHEN id > 1 THEN dd ELSE 0 END), SUM(CASE WHEN id > 0 THEN 1.5 ELSE 0 END), CASE WHEN id = 1 THEN 1.50 ELSE 10 END, SUM(CASE WHEN id > 1 THEN dec1 ELSE 0 END), CASE WHEN id = 2 THEN dec1 ELSE 100 END FROM v7 GROUP BY id ORDER BY id",
			"SELECT SUM(CASE WHEN id > 0 THEN dec1 ELSE 0 END), SUM(IF(id > 0, dec1, 0)), SUM(COALESCE(dec1, 0)) FROM v7",
			"SELECT id FROM v7 WHERE IF(id = 1, e, 1) = 'b' ORDER BY id",
			"SELECT id FROM v7 WHERE COALESCE(e, 1) = 'c' ORDER BY id",
		},
		drop: []string{"DROP TABLE v7"},
	},
	{
		name: "strtodate", // 8. STR_TO_DATE の月の日数を超える日（MySQL は NULL）と書式と同じ文字列（すべて 0 の日付）、MICROSECOND と TIME_TO_SEC の時刻の文字列
		setup: []string{
			"CREATE TABLE v8 (id INT PRIMARY KEY, d DATETIME, t TIME, s VARCHAR(30))",
			"INSERT INTO v8 VALUES (1, '2024-01-01 13:04:05', '13:04:05', '2024-02-30'), (2, NULL, '-01:00:00', '30/04/2024')",
		},
		both: []string{
			"SELECT STR_TO_DATE('2024-01-01 10', '%Y-%m-%d %H'), STR_TO_DATE('10:04', '%H:%i'), STR_TO_DATE('2024-01-01x', '%Y-%m-%d'), STR_TO_DATE('2024-13-01', '%Y-%m-%d'), STR_TO_DATE('2024-02-30', '%Y-%m-%d'), STR_TO_DATE(NULL, '%Y'), STR_TO_DATE('2024', NULL)",
			"SELECT STR_TO_DATE('2024-02-30', '%Y-%m-%d'), STR_TO_DATE('2024-04-31', '%Y-%m-%d'), STR_TO_DATE('2023-02-29', '%Y-%m-%d'), STR_TO_DATE('2024-02-29', '%Y-%m-%d'), STR_TO_DATE('2024-02-30 10:00:00', '%Y-%m-%d %H:%i:%s'), STR_TO_DATE('2024-00-10', '%Y-%m-%d'), STR_TO_DATE('2024-02-00', '%Y-%m-%d')",
			"SELECT STR_TO_DATE('31/02/2024', '%d/%m/%Y'), STR_TO_DATE('30 Feb 2024', '%d %b %Y'), STR_TO_DATE('31 Dec 2024', '%d %b %Y'), DATE('2024-02-30'), CAST('2024-02-30' AS DATE)",
			"SELECT id, STR_TO_DATE(s, '%Y-%m-%d'), STR_TO_DATE(s, '%d/%m/%Y') FROM v8 ORDER BY id",
			"SELECT MICROSECOND('13:04:05.5'), MICROSECOND('13:04:05.123456'), MICROSECOND('2024-01-01 13:04:05.5'), MICROSECOND('13:04:05'), MICROSECOND(t), MICROSECOND(d), MICROSECOND('13:04:05.1234567'), MICROSECOND('x') FROM v8 ORDER BY id",
			"SELECT SECOND('13:04:05.5'), MINUTE('13:04:05.5'), HOUR('13:04:05.5'), TIME_FORMAT('13:04:05.5', '%f'), TIME_TO_SEC('13:04:05.5'), TIME_TO_SEC('-01:00:00'), TIME_TO_SEC(t) FROM v8 ORDER BY id",
		},
		stmts: []string{
			// 指定子のない書式と同じ文字列（MySQL は WordPress の sql_mode ですべて 0 の日付）
			wpSQLMode,
			"SELECT STR_TO_DATE('x', 'x'), STR_TO_DATE('x', '%Y'), STR_TO_DATE('2024', 'x'), STR_TO_DATE('', ''), STR_TO_DATE('abc', 'abc'), STR_TO_DATE('2024-01-01', '%Y-%m-%d'), STR_TO_DATE('01/02/2024', '%m/%d/%Y')",
			"SELECT STR_TO_DATE('x', 'x') IS NULL, DATE(STR_TO_DATE('x', 'x')), CONCAT(STR_TO_DATE('x', 'x'))",
		},
		drop: []string{"DROP TABLE v8"},
	},
	{
		name: "numrest", // 9. 数の残り: DECIMAL の列の値の後ろの 0、'nan' と 'inf'、DOUBLE の範囲の外の 1690、STRICT の UPDATE の 1264
		setup: []string{
			"CREATE TABLE n9 (id INT PRIMARY KEY, i INT, u INT UNSIGNED, ti TINYINT, bu BIGINT UNSIGNED, dec1 DECIMAL(10,2), dec2 DECIMAL(26,4), f DOUBLE, s VARCHAR(20), KEY dec1 (dec1))",
			"INSERT INTO n9 VALUES (1, 1, 1, 1, 1, 10.50, 10.5000, 2.55, '2.55'), (2, 2, 2, 2, 2, 0.30, 1234567890.1000, 1.005, '1.005'), (3, 3, 3, 3, 3, 1.00, 2.0000, 2.45, 'x')",
			wpSQLMode,
			"CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL PRIMARY KEY, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, KEY min_max_price (min_price, max_price))",
			"INSERT INTO wp_wc_product_meta_lookup VALUES (1, 10.0000, 12.5000), (2, 9.9950, 9.9950), (3, 0.0000, 100.1000)",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT CONCAT(dec1), CONCAT(dec2), CONCAT(dec1, '-', dec2), CAST(dec2 AS CHAR), dec2, LENGTH(dec2), FORMAT(dec2, 2), CONCAT(MAX(dec1)) FROM n9 GROUP BY id ORDER BY id",
			"SELECT dec1 FROM n9 WHERE id = 1 UNION ALL SELECT 'x'",
			"SELECT dec1 FROM n9 WHERE id = 1 UNION ALL SELECT dec2 FROM n9 WHERE id = 3",
			"SELECT dec1 * 2, dec1 + 1, dec1 / 2, -dec1, dec1 FROM n9 ORDER BY id",
			"SELECT id, dec1 FROM n9 WHERE dec1 >= 1 ORDER BY dec1",
			"SELECT product_id, CONCAT(min_price, '-', max_price), CONCAT('$', max_price) FROM wp_wc_product_meta_lookup ORDER BY product_id",
			"SELECT IF('nan', 1, 0), IF('inf', 1, 0), IF('-inf', 1, 0), IF('infinity', 1, 0), IF('NaN', 1, 0), 'nan' IS TRUE, 'inf' IS TRUE, NOT 'nan', 'nan' = 0, 'inf' + 0, 'nan' + 0, 'inf' AND 1, CASE WHEN 'nan' THEN 1 ELSE 0 END",
			"SELECT IF('1x', 1, 0), IF('x', 1, 0), IF('0.0', 1, 0), IF(' 1', 1, 0), IF('-0', 1, 0), IF('.', 1, 0), IF('e', 1, 0), IF('n', 1, 0), IF('i', 1, 0), '0x10' + 0, '1e3' + 0",
			"SELECT id FROM n9 WHERE 'nan' ORDER BY id",
			"SELECT id FROM n9 WHERE 'inf' ORDER BY id",
			"SELECT 1e308 * 10",
			"SELECT 1e308 + 1e308",
		},
		stmts: []string{
			// STRICT の UPDATE の範囲の外は 1264（MySQL は行を変えない）
			"UPDATE n9 SET i = 99999999999 WHERE id = 1",
			"UPDATE n9 SET ti = 300 WHERE id = 1",
			"UPDATE n9 SET u = -1 WHERE id = 1",
			"UPDATE n9 SET bu = -1 WHERE id = 1",
			"UPDATE n9 SET dec1 = 123456789.12 WHERE id = 1",
			"UPDATE n9 SET i = '99999999999' WHERE id = 1",
			"UPDATE n9 SET i = i + 2147483647 WHERE id = 1",
			"UPDATE n9 SET i = 1.0e20 WHERE id = 1",
			"UPDATE n9 SET i = 99999999999, ti = 1 WHERE id = 2",
			"SELECT id, i, u, ti, bu, dec1 FROM n9 ORDER BY id",
			"UPDATE IGNORE n9 SET i = 99999999999 WHERE id = 1",
			"SELECT id, i FROM n9 WHERE id = 1",
			// WordPress の sql_mode は端の値と警告 1264
			wpSQLMode,
			"UPDATE n9 SET ti = 300 WHERE id = 3",
			"UPDATE n9 SET dec1 = 123456789.12 WHERE id = 3",
			"SELECT id, ti, dec1 FROM n9 WHERE id = 3",
		},
		drop: []string{"DROP TABLE wp_wc_product_meta_lookup", "DROP TABLE n9"},
	},
	{
		name: "enum", // 10. WordPress の sql_mode の ENUM と SET への定義にない値（空の値と警告 1265）、ENUM と SET の COLLATE、ENUM と SET の UNION
		setup: []string{
			"CREATE TABLE e6 (id INT PRIMARY KEY, s VARCHAR(5), e ENUM('a','b','c') NOT NULL DEFAULT 'a', en ENUM('x','y') NULL, st SET('p','q')) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO e6 VALUES (1, 'abc', 'a', 'x', 'p'), (2, 'de', 'b', 'y', 'q'), (3, 'f', 'c', NULL, 'p,q')",
		},
		both: []string{
			"SELECT e COLLATE utf8mb4_bin, st COLLATE utf8mb4_bin FROM e6 ORDER BY id",
			"SELECT id FROM e6 WHERE e COLLATE utf8mb4_bin = 'a' ORDER BY id",
			"SELECT id FROM e6 ORDER BY e COLLATE utf8mb4_bin DESC, id",
			"SELECT id FROM e6 WHERE e = 'a' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT id FROM e6 WHERE e LIKE 'a%' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT e COLLATE utf8mb4_general_ci = 'A', CONCAT(e COLLATE utf8mb4_bin, '!') FROM e6 ORDER BY id",
			"SELECT e FROM e6 UNION ALL SELECT st FROM e6",
			"SELECT e FROM e6 UNION ALL SELECT en FROM e6",
			"SELECT e FROM e6 UNION ALL SELECT 'zz' FROM e6",
			"SELECT x, COUNT(*) FROM (SELECT e AS x FROM e6 UNION ALL SELECT st FROM e6) q GROUP BY x ORDER BY x",
		},
		stmts: []string{
			wpSQLMode,
			"UPDATE e6 SET e = 'z' WHERE id = 2",
			"UPDATE e6 SET e = '' WHERE id = 3",
			"UPDATE e6 SET st = 'r' WHERE id = 2",
			"UPDATE e6 SET en = 'z' WHERE id = 1",
			"SELECT id, s, e, en, st FROM e6 ORDER BY id",
			"INSERT INTO e6 (id, e) VALUES (4, 'z')",
			"INSERT INTO e6 (id, e) VALUES (5, '')",
			"INSERT INTO e6 (id, e) VALUES (6, 4)",
			"INSERT INTO e6 (id, en) VALUES (7, 'z')",
			"INSERT INTO e6 (id, st) VALUES (8, 'r')",
			"INSERT INTO e6 (id, e) VALUES (9, 'a'), (10, '')",
			"SELECT LAST_INSERT_ID(0)", // REPLACE と ON DUPLICATE KEY UPDATE の insert_id にセッションの値が出るので 0 にする
			"INSERT IGNORE INTO e6 (id, e) VALUES (11, '')",
			"REPLACE INTO e6 (id, e) VALUES (12, '')",
			"INSERT INTO e6 (id, e) VALUES (13, 'a') ON DUPLICATE KEY UPDATE e = ''",
			"INSERT INTO e6 (id, e) VALUES (13, 'a') ON DUPLICATE KEY UPDATE e = ''",
			"SELECT id, e, en, st, e + 0, e = '', e IS NULL, CONCAT(e), LENGTH(e) FROM e6 ORDER BY id",
			"SELECT id FROM e6 WHERE e = '' ORDER BY id",
			"SELECT e, COUNT(*) FROM e6 GROUP BY e ORDER BY e",
			"SELECT e FROM e6 WHERE id = 1 UNION ALL SELECT e FROM e6 WHERE id = 2",
		},
		drop: []string{"DROP TABLE e6"},
	},
	{
		name: "shape", // 11. SQL の形: HAVING の名前（GROUP BY の列、SELECT の項の順）、SELECT DISTINCT 式 ... ORDER BY 1、UPDATE と DELETE の 1093、文字列の列の BETWEEN の端の混在
		setup: []string{
			"CREATE TABLE t6 (id INT PRIMARY KEY, k INT, v INT, name VARCHAR(20), KEY k (k))",
			"CREATE TABLE t7 (id INT PRIMARY KEY, k INT, q INT, name VARCHAR(20), x INT)",
			"INSERT INTO t6 VALUES (1, 1, 10, 'a'), (2, 1, 20, 'b'), (3, 2, 30, '5'), (4, 2, 40, '10'), (5, 3, 50, 'c')",
			"INSERT INTO t7 VALUES (1, 1, 1, 'a', 7), (2, 2, 2, 'b', 8), (3, 3, 3, 'c', 9)",
		},
		both: []string{
			"SELECT k, SUM(v) AS v FROM t6 GROUP BY k HAVING v > 35 ORDER BY k",
			"SELECT k, SUM(v) AS v FROM t6 GROUP BY k HAVING v > 50 ORDER BY k",
			"SELECT k, COUNT(*) AS k FROM t6 GROUP BY k HAVING k > 1 ORDER BY 1",
			"SELECT k AS kk, SUM(v) AS v FROM t6 GROUP BY k HAVING v > 35 ORDER BY kk",
			"SELECT name, COUNT(*) AS name FROM t6 GROUP BY name HAVING name > 'b' ORDER BY 1",
			"SELECT k, MAX(v) AS v FROM t6 GROUP BY k HAVING v = 20 ORDER BY k",
			"SELECT k, v + 100 AS v FROM t6 GROUP BY k, v HAVING v > 135 ORDER BY k, v",
			"SELECT k, SUM(v) AS v FROM t6 GROUP BY k HAVING v > 35 AND k > 0 ORDER BY k",
			"SELECT k, SUM(v) AS v FROM t6 GROUP BY k HAVING v IN (50, 70) ORDER BY k",
			"SELECT k, SUM(v) AS v FROM t6 GROUP BY k HAVING SUM(v) > 35 ORDER BY k",
			"SELECT k, SUM(v) AS total FROM t6 GROUP BY k HAVING total > 35 ORDER BY k",
			"SELECT k, v FROM t6 GROUP BY k, v HAVING v > 35 ORDER BY k, v",
			"SELECT k, SUM(v) v FROM t6 WHERE v > 15 GROUP BY k HAVING v > 35 ORDER BY k",
			"SELECT DISTINCT k + 1 FROM t6 ORDER BY 1",
			"SELECT DISTINCT k + 1 FROM t6 ORDER BY 1 DESC",
			"SELECT DISTINCT k + 1 AS kk FROM t6 ORDER BY 1",
			"SELECT DISTINCT k + 1 FROM t6 ORDER BY k + 1",
			"SELECT DISTINCT CONCAT(name, 'x') FROM t6 ORDER BY 1",
			"SELECT DISTINCT k, v + 1 FROM t6 ORDER BY 2, 1",
			"SELECT DISTINCT UPPER(name) FROM t6 ORDER BY 1 LIMIT 2",
			"SELECT DISTINCT k * 2 FROM t6 GROUP BY k ORDER BY 1",
			"SELECT id, name FROM t6 WHERE name BETWEEN 1 AND 7 ORDER BY id",
			"SELECT id, name FROM t6 WHERE name BETWEEN '1' AND 7 ORDER BY id",
			"SELECT id, name FROM t6 WHERE name BETWEEN 1 AND '7' ORDER BY id",
			"SELECT id, name FROM t6 WHERE name BETWEEN '1' AND '7' ORDER BY id",
			"SELECT id, name FROM t6 WHERE name BETWEEN 'a' AND 7 ORDER BY id",
			"SELECT id, name FROM t6 WHERE name BETWEEN 0 AND 'b' ORDER BY id",
			"SELECT id, name FROM t6 WHERE name NOT BETWEEN '1' AND 7 ORDER BY id",
			"SELECT id, name FROM t6 WHERE name BETWEEN 1.5 AND '9' ORDER BY id",
			"SELECT name BETWEEN '1' AND 7, name BETWEEN 1 AND 7, name BETWEEN '1' AND '7' FROM t6 ORDER BY id",
		},
		stmts: []string{
			// UPDATE と DELETE の副問い合わせが対象の表を読めば 1093（派生テーブルの中は読める）
			"UPDATE t6 SET v = v + 1 WHERE id IN (SELECT id FROM t6 WHERE k = 1)",
			"UPDATE t6 SET v = v + 1 WHERE k = (SELECT MAX(k) FROM t6)",
			"UPDATE t6 SET v = (SELECT MAX(v) FROM t6) WHERE id = 1",
			"DELETE FROM t6 WHERE id IN (SELECT id FROM t6 WHERE k = 9)",
			"DELETE FROM t6 WHERE v < (SELECT AVG(v) FROM t6) AND k = 99",
			"DELETE FROM t6 WHERE id IN (SELECT t.id FROM t6 t WHERE t.k = 9)",
			"UPDATE t6 SET v = 1 WHERE EXISTS (SELECT 1 FROM t6 b WHERE b.id = t6.id AND b.k = 99)",
			"UPDATE t6 SET v = 1 WHERE id IN (SELECT t7.id FROM t7 JOIN t6 ON t6.k = t7.k WHERE t7.q = 99)",
			"UPDATE t6 SET v = v + 1 WHERE id IN (SELECT id FROM (SELECT id FROM t6 WHERE k = 1) q)",
			"UPDATE t6 SET v = v + 1 WHERE id IN (SELECT id FROM t7 WHERE k = 1)",
			"DELETE t6 FROM t6 JOIN (SELECT id FROM t6 WHERE k = 99) q ON q.id = t6.id",
			"SELECT id, v FROM t6 ORDER BY id",
		},
		drop: []string{"DROP TABLE t7", "DROP TABLE t6"},
	},
	{
		name: "errors", // 12. 誤りの番号: STRICT の 1406 と 1265（後ろの空白だけなら Note 1265 で切る）、1193、1367、知らない表の修飾子の 1054、行の値の = と IN の NULL
		setup: []string{
			"CREATE TABLE e5 (id INT PRIMARY KEY, s VARCHAR(5), c CHAR(3), e ENUM('a','b','c') NOT NULL DEFAULT 'a', en ENUM('x','y') NULL, st SET('p','q'), b VARBINARY(3)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO e5 (id) VALUES (1)",
			"CREATE TABLE A (id INT PRIMARY KEY, ts INT)",
			"INSERT INTO A VALUES (1, 1)",
			"CREATE TABLE t6 (id INT PRIMARY KEY, k INT, v INT)",
			"INSERT INTO t6 VALUES (1, 1, 10), (2, 1, 20), (3, 2, 30)",
		},
		both: []string{
			"SELECT A.version FROM A",
			"SELECT A.ts FROM nope A",
			"SELECT A.timestamp FROM A",
			"SELECT @@nonexistent_var",
			"SET @@nonexistent_var = 1",
			"SET SESSION nonexistent_var = 1",
			"SET GLOBAL nonexistent_var = 1",
			"SELECT 1e400",
			"SELECT -1e400",
			"SELECT 1e400 + 0",
			"SELECT k FROM t6 GROUP BY k HAVING v > 1",
			"SELECT k FROM t6 GROUP BY k HAVING nope > 1",
			"SELECT k FROM t6 GROUP BY k HAVING t7.v > 1",
			"SELECT k FROM t6 GROUP BY k HAVING MAX(nope) > 1",
			"SELECT (1, NULL) IN ((1, NULL)), (1, NULL) IN ((1, 2)), (1, NULL) = (1, NULL), (1, 2) = (1, 2), (1, 2) = (1, 3)",
			"SELECT (1, 2) IN ((1, NULL), (1, 2)), (1, 3) IN ((1, NULL), (1, 2)), (2, 3) IN ((1, NULL))",
			"SELECT id FROM t6 WHERE (k, v) IN ((1, 10), (2, NULL)) ORDER BY id",
			"SELECT id, (k, v) = (1, NULL), (k, v) IN ((2, NULL), (1, 20)) FROM t6 ORDER BY id",
		},
		stmts: []string{
			// STRICT（既定の sql_mode）
			"INSERT INTO e5 (id, s) VALUES (2, 'abcdef')",
			"INSERT INTO e5 (id, c) VALUES (3, 'abcd')",
			"INSERT INTO e5 (id, e) VALUES (4, 'z')",
			"INSERT INTO e5 (id, e) VALUES (5, '')",
			"INSERT INTO e5 (id, e) VALUES (6, 4)",
			"INSERT INTO e5 (id, en) VALUES (7, 'z')",
			"INSERT INTO e5 (id, st) VALUES (8, 'r')",
			"INSERT INTO e5 (id, b) VALUES (9, 'abcd')",
			"INSERT INTO e5 (id, s) VALUES (10, 'あいうえおか')",
			"INSERT INTO e5 (id, s) VALUES (11, 'abcde  ')",
			"INSERT INTO e5 (id, s) VALUES (12, 'abc  d')",
			"UPDATE e5 SET s = 'abcdef' WHERE id = 1",
			"UPDATE e5 SET e = 'z' WHERE id = 1",
			"UPDATE e5 SET e = '' WHERE id = 1",
			"UPDATE e5 SET st = 'r' WHERE id = 1",
			"UPDATE e5 SET s = CONCAT('ab', 'cdefg') WHERE id = 1",
			"SELECT id, s, c, e, en, st FROM e5 ORDER BY id",
		},
		drop: []string{"DROP TABLE t6", "DROP TABLE A", "DROP TABLE e5"},
	},
	{
		name: "distinct2", // 13. レビュー: SELECT DISTINCT 別名のない式 ... ORDER BY 番号は、射影の列で並べる（前は射影の行の上で式をもう一度評価した）
		setup: append([]string{
			"CREATE TABLE t8 (id INT PRIMARY KEY, i INT)",
			"INSERT INTO t8 VALUES (1, 1), (2, 2), (3, -3), (4, NULL), (5, 1)",
		}, stage116Posts...),
		both: []string{
			"SELECT DISTINCT -i FROM t8 ORDER BY 1",
			"SELECT DISTINCT -i FROM t8 ORDER BY 1 DESC",
			"SELECT DISTINCT -i AS x FROM t8 ORDER BY 1",
			"SELECT DISTINCT -i FROM t8 ORDER BY -i",
			"SELECT DISTINCT -i FROM t8 ORDER BY -i DESC",
			"SELECT DISTINCT id % 2, -i FROM t8 ORDER BY 2, 1",
			"SELECT DISTINCT i, -i FROM t8 ORDER BY 2",
			"SELECT DISTINCT -i, i FROM t8 ORDER BY 1 DESC, 2",
			"SELECT DISTINCT -i FROM t8 ORDER BY 1 LIMIT 2",
			"SELECT DISTINCT -i FROM t8 ORDER BY 1 LIMIT 1, 2",
			"SELECT DISTINCT -i FROM t8 WHERE i > 0 ORDER BY 1",
			"SELECT DISTINCT CONCAT('a', i) FROM t8 ORDER BY 1 DESC",
			"SELECT x FROM (SELECT DISTINCT -i AS x FROM t8 ORDER BY 1 LIMIT 3) q ORDER BY x",
			"SELECT * FROM (SELECT DISTINCT -i FROM t8 ORDER BY 1 LIMIT 2) q",
			"SELECT DISTINCT LENGTH(post_title) FROM wp_posts ORDER BY 1",
			"SELECT DISTINCT LENGTH(post_title) FROM wp_posts ORDER BY 1 DESC",
			"SELECT DISTINCT post_author, DATE(post_date) FROM wp_posts ORDER BY 2 DESC",
			"SELECT DISTINCT post_author, DATE(post_date) FROM wp_posts ORDER BY 2",
			"SELECT DISTINCT post_author, DATE(post_date) FROM wp_posts ORDER BY 1 DESC, 2 DESC",
			"SELECT DISTINCT YEAR(post_date), MONTH(post_date) FROM wp_posts ORDER BY 1 DESC, 2 DESC",
			"SELECT DISTINCT YEAR(post_date) AS year, MONTH(post_date) AS month FROM wp_posts ORDER BY 1 DESC, 2 DESC",
			"SELECT DISTINCT -post_author FROM wp_posts GROUP BY post_author ORDER BY 1",
			"SELECT DISTINCT COUNT(*) FROM wp_posts GROUP BY post_author ORDER BY 1",
			"SELECT DISTINCT -COUNT(*) FROM wp_posts GROUP BY post_author ORDER BY 1",
			"SELECT DISTINCT SUBSTRING(post_title, 2, 3) FROM wp_posts ORDER BY 1",
		},
		drop: []string{"DROP TABLE wp_posts", "DROP TABLE t8"},
	},
	{
		name:  "having2", // 14. レビュー: HAVING の名前。列の別名の GROUP BY の列は GROUP BY の列、SELECT の項は列でない項（集計の別名）が同じ名前の列より先
		setup: stage116Posts,
		both: []string{
			"SELECT post_author AS a, COUNT(*) AS post_author FROM wp_posts GROUP BY a HAVING post_author > 1 ORDER BY a",
			"SELECT post_author AS a, COUNT(*) AS post_author FROM wp_posts GROUP BY a HAVING post_author > 1 AND a < 3 ORDER BY a",
			"SELECT post_author AS a, COUNT(*) AS post_author FROM wp_posts GROUP BY 1 HAVING post_author > 1 ORDER BY a",
			"SELECT p.post_author AS a, COUNT(*) AS post_author FROM wp_posts p GROUP BY a HAVING post_author > 1 ORDER BY a",
			"SELECT post_author AS a, COUNT(*) AS post_author FROM wp_posts GROUP BY post_author HAVING post_author > 1 ORDER BY a",
			"SELECT post_author, COUNT(*) AS post_author FROM wp_posts GROUP BY post_author HAVING post_author > 1 ORDER BY 1",
			"SELECT post_author AS a, COUNT(*) AS n FROM wp_posts GROUP BY a HAVING post_author > 1 ORDER BY a",
			"SELECT post_author AS a, COUNT(*) AS n FROM wp_posts GROUP BY a HAVING a > 1 ORDER BY a",
			"SELECT post_author + 0 AS a, COUNT(*) AS post_author FROM wp_posts GROUP BY a HAVING post_author > 1 ORDER BY a",
			"SELECT post_author AS a, SUM(comment_count) AS comment_count FROM wp_posts GROUP BY a HAVING comment_count > 0 ORDER BY a",
			"SELECT p.ID, p.comment_count, COUNT(c.ID) AS comment_count FROM wp_posts p LEFT JOIN wp_posts c ON c.post_author = p.ID GROUP BY p.ID HAVING comment_count > 0 ORDER BY p.ID",
			"SELECT p.ID AS pid, p.comment_count, COUNT(c.ID) AS comment_count FROM wp_posts p LEFT JOIN wp_posts c ON c.post_author = p.ID GROUP BY pid HAVING comment_count > 0 ORDER BY pid",
			"SELECT p.ID, COUNT(c.ID) AS comment_count, p.comment_count FROM wp_posts p LEFT JOIN wp_posts c ON c.post_author = p.ID GROUP BY p.ID HAVING comment_count > 0 ORDER BY p.ID",
			"SELECT post_author, COUNT(*) AS cnt FROM wp_posts GROUP BY post_author HAVING cnt > 1 ORDER BY post_author",
		},
		stmts: []string{
			// WordPress の sql_mode（ONLY_FULL_GROUP_BY でない）: 式の別名が同じ名前の集計しない列より先
			wpSQLMode,
			"SELECT post_author, comment_count, comment_count * 10 AS comment_count FROM wp_posts GROUP BY post_author HAVING comment_count > 5 ORDER BY post_author",
			"SELECT post_author, comment_count * 10 AS comment_count, comment_count FROM wp_posts GROUP BY post_author HAVING comment_count > 5 ORDER BY post_author",
			"SELECT post_author, comment_count FROM wp_posts GROUP BY post_author HAVING comment_count > 0 ORDER BY post_author",
		},
		drop: []string{"DROP TABLE wp_posts"},
	},
	{
		name: "enum2", // 15. レビュー: ENUM と SET の後ろの空白、SET の定義にない要素（非 STRICT は定義にある要素だけ残す）、索引のある ENUM の列の ''、SET にならない文字列との比べ
		setup: []string{
			"CREATE TABLE e7 (id INT PRIMARY KEY, e ENUM('a','b','c') NOT NULL DEFAULT 'a', e2 ENUM('a','b','c') NULL, st SET('x','y') NULL, KEY e (e)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			// STRICT（既定の sql_mode）: 後ろの空白は除く（前の空白は 1265）
			"INSERT INTO e7 (id, e, e2) VALUES (1, 'a ', 'b ')",
			"INSERT INTO e7 (id, e) VALUES (2, 'b  ')",
			"INSERT INTO e7 (id, e) VALUES (3, ' a')",
			"INSERT INTO e7 (id, e) VALUES (4, 'A ')",
			"INSERT INTO e7 (id, st) VALUES (5, 'x ')",
			"INSERT INTO e7 (id, st) VALUES (6, 'x,y ')",
			"INSERT INTO e7 (id, st) VALUES (7, 'x,bad')",
			"UPDATE e7 SET e = 'c ' WHERE id = 1",
			"UPDATE e7 SET e2 = 'c  ' WHERE id = 2",
			"UPDATE e7 SET e = 'zz ' WHERE id = 2",
			"SELECT id, e, e2, st, e + 0, st + 0 FROM e7 ORDER BY id",
			// WordPress の sql_mode
			wpSQLMode,
			"INSERT INTO e7 (id, e, e2) VALUES (11, 'a ', 'c ')",
			"INSERT INTO e7 (id, e) VALUES (12, ' a')",
			"INSERT INTO e7 (id, st) VALUES (15, 'x ')",
			"INSERT INTO e7 (id, st) VALUES (17, 'x,bad')",
			"INSERT INTO e7 (id, st) VALUES (18, 'bad,y')",
			"INSERT INTO e7 (id, st) VALUES (19, 'bad')",
			"INSERT INTO e7 (id, e, e2) VALUES (20, 'zz', 'zz')",
			"INSERT INTO e7 (id, st) VALUES (21, 'y,bad,x')",
			"UPDATE e7 SET e = 'b ' WHERE id = 11",
			"UPDATE e7 SET st = 'y,bad' WHERE id = 15",
			"REPLACE INTO e7 (id, e, st) VALUES (22, 'c ', 'bad,x')",
			"INSERT INTO e7 (id, e) VALUES (22, 'a') ON DUPLICATE KEY UPDATE st = 'nope,y'",
			"SELECT id, e, e2, st, e + 0, st + 0 FROM e7 ORDER BY id",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			// 索引のある e と索引のない e2 の比べ（空の値は '' に合う。定義にない値の範囲は NULL でない行を読んで比べる）
			"SELECT id FROM e7 WHERE e = '' ORDER BY id",
			"SELECT id FROM e7 IGNORE INDEX (e) WHERE e = '' ORDER BY id",
			"SELECT id FROM e7 WHERE e2 = '' ORDER BY id",
			"SELECT id FROM e7 WHERE e = '  ' ORDER BY id",
			"SELECT id FROM e7 WHERE e2 = '  ' ORDER BY id",
			"SELECT id FROM e7 WHERE e > '' ORDER BY id",
			"SELECT id FROM e7 WHERE e >= '' ORDER BY id",
			"SELECT id FROM e7 WHERE e < 'b' ORDER BY id",
			"SELECT id FROM e7 WHERE e <= 'a' ORDER BY id",
			"SELECT id FROM e7 WHERE e BETWEEN '' AND 'a' ORDER BY id",
			"SELECT id FROM e7 WHERE e IN ('', 'a') ORDER BY id",
			"SELECT id FROM e7 IGNORE INDEX (e) WHERE e IN ('', 'a') ORDER BY id",
			"SELECT id FROM e7 WHERE e IN ('') ORDER BY id",
			"SELECT id FROM e7 WHERE e2 IN ('', 'b') ORDER BY id",
			"SELECT id FROM e7 WHERE e <> '' ORDER BY id",
			"SELECT id FROM e7 WHERE e NOT IN ('', 'b') ORDER BY id",
			"SELECT id FROM e7 WHERE e = 'a ' ORDER BY id",
			"SELECT id FROM e7 IGNORE INDEX (e) WHERE e = 'zz' ORDER BY id",
			"SELECT id FROM e7 WHERE e2 = 'zz' ORDER BY id",
			"SELECT id FROM e7 WHERE st = 'bad' ORDER BY id",
			"SELECT id FROM e7 WHERE st IN ('x', 'bad') ORDER BY id",
			"SELECT id FROM e7 WHERE st = '' ORDER BY id",
			"SELECT id FROM e7 WHERE st = 'x' ORDER BY id",
			"SELECT id FROM e7 WHERE st = 'x,bad' ORDER BY id",
			"SELECT id FROM e7 WHERE st <> 'bad' ORDER BY id",
			"SELECT id FROM e7 WHERE st > 'bad' ORDER BY id",
			"SELECT id, st = 'bad', st = 'x', st < 'bad', e = '', e > '', e2 = '' FROM e7 ORDER BY id",
		},
		drop: []string{"DROP TABLE e7"},
	},
	{
		name: "datebool", // 16. レビュー: 日時の真偽。ゼロの日付だけが偽（前はゼロの日付が真、1970-01-01 00:00:00 が偽）、TIME の値の真偽（前は 1105）
		setup: append([]string{
			wpSQLMode,
			"CREATE TABLE d1 (id INT PRIMARY KEY, dt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', dd DATE NULL, ts TIMESTAMP NULL, t TIME NULL, y YEAR NULL)",
			"INSERT INTO d1 VALUES (1, '0000-00-00 00:00:00', '0000-00-00', NULL, '00:00:00', 0), (2, '2024-01-01 00:00:00', '2024-01-01', '2024-01-01 00:00:00', '10:00:00', 2024), " +
				"(3, '1970-01-01 00:00:00', '1970-01-01', '1970-01-01 00:00:01', '-01:00:00', 1970), (4, '2000-01-01 00:00:00', NULL, NULL, NULL, NULL), (5, '2024-01-01 00:00:00.5', '0001-01-01', NULL, '00:00:00.5', 2000)",
			"SET SESSION sql_mode = DEFAULT",
		}, stage116Posts...),
		both: []string{
			"SELECT id, dt, IF(dt, 'y', 'n'), NOT dt, dt IS TRUE, dt IS FALSE, dt IS NOT TRUE, IF(dd, 'y', 'n'), NOT dd, dd IS TRUE, IF(ts, 'y', 'n'), IF(t, 'y', 'n'), NOT t, IF(y, 'y', 'n') FROM d1 ORDER BY id",
			"SELECT id, dt AND 1, dt OR 0, dt XOR 1, dd AND dt, t OR 0, CASE WHEN dt THEN 1 ELSE 0 END, CASE WHEN t THEN 1 ELSE 0 END FROM d1 ORDER BY id",
			"SELECT id FROM d1 WHERE dt ORDER BY id",
			"SELECT id FROM d1 WHERE NOT dt ORDER BY id",
			"SELECT id FROM d1 WHERE dd ORDER BY id",
			"SELECT id FROM d1 WHERE ts ORDER BY id",
			"SELECT id FROM d1 WHERE t ORDER BY id",
			"SELECT id FROM d1 WHERE dt AND dd ORDER BY id",
			"SELECT CASE WHEN dt THEN 1 ELSE 0 END, COUNT(*) FROM d1 GROUP BY 1 ORDER BY 1",
			"SELECT IF(CAST('0000-00-00' AS DATE), 1, 0), IF(FROM_UNIXTIME(0), 1, 0), IF(STR_TO_DATE('x', 'x'), 1, 0), IF(CAST('00:00:00' AS TIME), 1, 0), IF(CAST('00:00:01' AS TIME), 1, 0)",
			// WordPress の下書きの post_date_gmt はゼロの日時
			"SELECT ID FROM wp_posts WHERE post_date_gmt ORDER BY ID",
			"SELECT ID, IF(post_date_gmt, 'y', 'n'), NOT post_date_gmt, post_date_gmt IS TRUE FROM wp_posts ORDER BY ID",
			"SELECT ID FROM wp_posts WHERE NOT post_date_gmt ORDER BY ID",
			"SELECT COUNT(*) FROM wp_posts WHERE post_date_gmt AND post_author = 1",
		},
		drop: []string{"DROP TABLE wp_posts", "DROP TABLE d1"},
	},
	{
		name: "ifnull2", // 17. レビュー: IFNULL の型。日時、TIME、ENUM、SET と数か文字列、数と文字列は文字列（前は左の型で、IFNULL(d, '') が 1105）
		setup: []string{
			wpSQLMode,
			"CREATE TABLE d2 (id INT PRIMARY KEY, dt DATETIME NULL, dd DATE NULL, t TIME NULL, e ENUM('a','b') NULL, st SET('x','y') NULL, s VARCHAR(10) NULL, i INT NULL, " +
				"dz DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', price DECIMAL(10,2) NULL)",
			"INSERT INTO d2 VALUES (1, '2024-01-02 03:04:05', '2024-01-02', '10:00:00', 'b', 'x,y', 'str', 5, '0000-00-00 00:00:00', 10.50), (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, '2024-01-01 00:00:00', NULL)",
			"SET SESSION sql_mode = DEFAULT",
		},
		both: []string{
			"SELECT id, IFNULL(dt, ''), IFNULL(dd, 'none'), IFNULL(dd, 0), IFNULL(dt, 0), IFNULL(t, ''), IFNULL(t, 0), IFNULL(e, ''), IFNULL(e, 0), IFNULL(st, 'none'), IFNULL(dz, '') FROM d2 ORDER BY id",
			"SELECT id, IFNULL(dt, dd), IFNULL(dt, NULL), IFNULL(NULL, dd), IFNULL(s, ''), IFNULL(i, 0), IFNULL(i, ''), IFNULL(i, 'none'), IFNULL(s, 0), IFNULL(s, dd), IFNULL(dd, s), IFNULL(price, ''), IFNULL(price, 0) FROM d2 ORDER BY id",
			"SELECT id, CONCAT(IFNULL(dd, '')), LENGTH(IFNULL(dt, '')), IFNULL(dd, '') = '', IFNULL(dd, '-'), CONCAT('[', IFNULL(i, ''), ']'), IFNULL(e, 'none') = 'b' FROM d2 ORDER BY id",
			"SELECT id FROM d2 WHERE IFNULL(dd, '') = '' ORDER BY id",
			"SELECT id FROM d2 WHERE IFNULL(dt, '') <> '' ORDER BY id",
			"SELECT id, COALESCE(dd, ''), COALESCE(dd, 0), IFNULL(dd, 0) = COALESCE(dd, 0) FROM d2 ORDER BY id",
			"SELECT IFNULL(NULL, ''), IFNULL(NULL, 0), IFNULL(1, ''), IFNULL(CAST(NULL AS DATE), ''), IFNULL(CAST('2024-01-01' AS DATE), 'x')",
		},
		drop: []string{"DROP TABLE d2"},
	},
	{
		name: "sumstr", // 18. レビュー: 数にならない文字列の SUM と AVG は先頭の数の部分（前は 0。WordPress の _edit_lock の '1704103200:1'）
		setup: []string{
			"CREATE TABLE pm (meta_id INT PRIMARY KEY, post_id INT, meta_key VARCHAR(255), meta_value LONGTEXT, s VARCHAR(20))",
			"INSERT INTO pm VALUES (1, 1, '_edit_lock', '1704103200:1', '3x'), (2, 2, '_edit_lock', '1704106800:2', '1'), (3, 1, '_price', '12abc', NULL), (4, 2, '_price', '10.5', '2.5y'), " +
				"(5, 3, '_price', '', ''), (6, 3, '_x', '1e400', 'abc'), (7, 4, '_x', 'abc', NULL), (8, 4, '_x', NULL, NULL), (9, 5, '_x', ' 7 ', ' 7'), (10, 5, '_x', '-3.5e2x', '-1')",
		},
		both: []string{
			"SELECT SUM(meta_value), AVG(meta_value) FROM pm WHERE meta_key = '_edit_lock'",
			"SELECT SUM(meta_value), AVG(meta_value), COUNT(meta_value) FROM pm WHERE meta_key = '_price'",
			"SELECT SUM(meta_value) FROM pm WHERE meta_key = '_x'",
			"SELECT meta_key, SUM(meta_value), AVG(meta_value), SUM(s), AVG(s) FROM pm GROUP BY meta_key ORDER BY meta_key",
			"SELECT SUM(s), AVG(s), SUM(s) + 0 FROM pm",
			"SELECT SUM('12abc'), SUM('1e400'), AVG('12abc'), SUM('abc'), SUM(''), SUM(' 5'), SUM('0x10'), SUM('1704103200:1')",
			"SELECT SUM(DISTINCT meta_value) FROM pm WHERE meta_key = '_edit_lock'",
			"SELECT post_id, SUM(meta_value) OVER (PARTITION BY post_id), AVG(meta_value) OVER (PARTITION BY post_id) FROM pm WHERE meta_key = '_price' ORDER BY post_id",
			"SELECT SUM(meta_value + 0), SUM(meta_value) = SUM(meta_value + 0) FROM pm WHERE meta_key IN ('_price', '_edit_lock')",
			"SELECT post_id FROM pm GROUP BY post_id HAVING SUM(meta_value) > 1000 ORDER BY post_id",
		},
		drop: []string{"DROP TABLE pm"},
	},
	{
		name: "enum3", // 19. 2 度目のレビュー: ENUM と文字列の比べは列の照合順序の文字列の比べ（NO PAD では 'a ' は 'a' に合わない。BINARY はバイトで比べる）、定義にない文字列との範囲（前は e > 'zz' が空の値より小さい扱い）
		setup: []string{
			// 既定の照合順序 utf8mb4_0900_ai_ci（NO PAD）と、WordPress の utf8mb4_unicode_520_ci（PAD SPACE）
			"CREATE TABLE e8 (id INT PRIMARY KEY, k INT NOT NULL, e ENUM('a','b','c') NOT NULL, e2 ENUM('a','b','c') NOT NULL, KEY e (e), KEY ke (k, e))",
			"CREATE TABLE e9 (id INT PRIMARY KEY, e ENUM('a','b','c') NOT NULL, e2 ENUM('a','b','c') NOT NULL, KEY e (e)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			wpSQLMode,
			"INSERT INTO e8 VALUES (1, 1, 'a', 'a'), (2, 1, 'b', 'b'), (3, 1, 'zz', 'zz'), (4, 1, 'c', 'c'), (5, 2, 'a', 'a'), (6, 2, 'c', 'c')",
			"INSERT INTO e9 SELECT id, e, e2 FROM e8",
			"SET SESSION sql_mode = DEFAULT",
			// 入れるときは、NO PAD でも後ろの空白を除く
			"INSERT INTO e8 VALUES (7, 3, 'b ', 'c  ')",
			"UPDATE e8 SET e2 = 'a ' WHERE id = 7",
			"SELECT id, e, e2 FROM e8 WHERE id = 7",
			"DELETE FROM e8 WHERE id = 7",
		},
		both: []string{
			// NO PAD: 後ろの空白のある文字列は値に合わない（索引のある e と索引のない e2）
			"SELECT id FROM e8 WHERE e = 'a ' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 = 'a ' ORDER BY id",
			"SELECT id FROM e8 IGNORE INDEX (e, ke) WHERE e = 'A ' ORDER BY id",
			"SELECT id FROM e8 WHERE e IN ('a ', 'b') ORDER BY id",
			"SELECT id FROM e8 WHERE e2 IN ('A', 'B ') ORDER BY id",
			"SELECT id FROM e8 WHERE e NOT IN ('a ', 'b') ORDER BY id",
			"SELECT id FROM e8 WHERE e = CONCAT('a', ' ') ORDER BY id",
			"SELECT id FROM e8 WHERE e = BINARY 'a ' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 = BINARY 'a' ORDER BY id",
			"SELECT id FROM e8 WHERE e = BINARY 'A' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 = BINARY 'A' ORDER BY id",
			"SELECT id FROM e8 WHERE e <> 'a ' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 <> 'a ' ORDER BY id",
			"SELECT id FROM e8 WHERE e = '  ' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 = '  ' ORDER BY id",
			"SELECT id FROM e8 WHERE e IN ('  ', 'b') ORDER BY id",
			"SELECT id FROM e8 WHERE e > 'a ' ORDER BY id",
			"SELECT id FROM e8 WHERE e >= 'A ' ORDER BY id",
			"SELECT id, e = 'a ', e2 = 'a ', e = 'a', e = '', e2 = '  ' FROM e8 ORDER BY id",
			// 定義にない文字列との範囲は文字列として比べる（空の値は ''）
			"SELECT id FROM e8 WHERE e > 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e < 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e <= 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e >= 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e BETWEEN 'a' AND 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e NOT BETWEEN 'a' AND 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e > 'aa' ORDER BY id",
			"SELECT id FROM e8 WHERE e < 'bb' ORDER BY id",
			"SELECT id FROM e8 IGNORE INDEX (e, ke) WHERE e > 'zz' ORDER BY id",
			"SELECT id FROM e8 IGNORE INDEX (e, ke) WHERE e < 'bb' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 > 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 BETWEEN 'a' AND 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE e2 < 'bb' ORDER BY id",
			"SELECT id FROM e8 WHERE k = 1 AND e > 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE k = 1 AND e < 'bb' ORDER BY id",
			"SELECT id FROM e8 WHERE k = 1 AND e BETWEEN 'a' AND 'zz' ORDER BY id",
			"SELECT id FROM e8 WHERE k = 1 AND e = 'a ' ORDER BY id",
			"SELECT id, e > 'zz', e < 'zz', e > 'aa', e < 'bb', e2 >= 'zz' FROM e8 ORDER BY id",
			// PAD SPACE: 後ろの空白は除いて比べる
			"SELECT id FROM e9 WHERE e = 'a ' ORDER BY id",
			"SELECT id FROM e9 WHERE e2 = 'A ' ORDER BY id",
			"SELECT id FROM e9 WHERE e IN ('a ', 'b') ORDER BY id",
			"SELECT id FROM e9 WHERE e2 IN ('a ', 'b') ORDER BY id",
			"SELECT id FROM e9 WHERE e <> 'a ' ORDER BY id",
			"SELECT id FROM e9 WHERE e = '  ' ORDER BY id",
			"SELECT id FROM e9 WHERE e2 = '  ' ORDER BY id",
			"SELECT id FROM e9 WHERE e2 = BINARY 'a ' ORDER BY id",
			"SELECT id FROM e9 WHERE e2 = BINARY 'A' ORDER BY id",
			"SELECT id FROM e9 WHERE e2 = BINARY 'a' ORDER BY id",
			"SELECT id FROM e9 WHERE e > 'zz' ORDER BY id",
			"SELECT id FROM e9 WHERE e < 'bb' ORDER BY id",
			"SELECT id FROM e9 WHERE e2 BETWEEN 'a' AND 'zz' ORDER BY id",
		},
		drop: []string{"DROP TABLE e8", "DROP TABLE e9"},
	},
	{
		name: "ifnull3", // 20. 2 度目のレビュー: 文字列の IFNULL の TIME の値は時刻の文字列（前は TIME の値のままで、IFNULL(t, '') + 0 が 130405、IFNULL(t, 0) = 0 がすべての行で真）
		setup: []string{
			"CREATE TABLE t9 (id INT PRIMARY KEY, t TIME)",
			"INSERT INTO t9 VALUES (1, '13:04:05'), (2, '00:00:00'), (3, NULL), (4, '-838:59:59'), (5, '-01:00:00'), (6, '00:00:01')",
		},
		both: []string{
			"SELECT id, IFNULL(t, ''), IFNULL(t, 0), IFNULL(t, 'none'), IFNULL(t, 1.5) FROM t9 ORDER BY id",
			"SELECT id, IFNULL(t, '') + 0, IFNULL(t, '') = 0, IFNULL(t, 0) = 0, IFNULL(t, 0) + 0, IFNULL(t, 'x') + 0, LENGTH(IFNULL(t, '')), LENGTH(IFNULL(t, 0)) FROM t9 ORDER BY id",
			"SELECT id FROM t9 WHERE IFNULL(t, 0) = 0 ORDER BY id",
			"SELECT id FROM t9 WHERE IFNULL(t, '') = '13:04:05' ORDER BY id",
			"SELECT id FROM t9 WHERE IFNULL(t, '') = '' ORDER BY id",
			"SELECT id, IFNULL(NULL, t), IFNULL('', t), CONCAT(IFNULL(t, '')), IFNULL(t, '') > '00:00:00', IFNULL(t, 'none') LIKE '13%' FROM t9 ORDER BY id",
			"SELECT id, COALESCE(t, ''), COALESCE(t, '') + 0, IF(id > 0, t, '') + 0, CONCAT(t) FROM t9 ORDER BY id",
		},
		drop: []string{"DROP TABLE t9"},
	},
	{
		name: "sumovf", // 21. 2 度目のレビュー: DOUBLE の上限を超えた SUM と AVG は MySQL と同じく '0' と表示し（値は無限大のまま）、ウィンドウ関数では上限（1.7976931348623157e308）にする（前は '+Inf'）
		setup: []string{
			"CREATE TABLE ov (id INT PRIMARY KEY, g INT, vc VARCHAR(20), dbl DOUBLE)",
			"INSERT INTO ov VALUES (1, 1, '1e400', 1e308), (2, 1, '1e400', 1e308), (3, 1, '5', 5), (4, 2, '1e308', 1e308), (5, 2, '1e308', 1e308), (6, 2, '-1e308', -1e308), " +
				"(7, 3, '-1e400', -1e308), (8, 3, '-1e400', -1e308), (9, 4, '1e400', 1.5), (10, 4, NULL, NULL)",
		},
		both: []string{
			"SELECT SUM(vc), AVG(vc), SUM(dbl), AVG(dbl) FROM ov WHERE g = 1",
			"SELECT g, SUM(vc), AVG(vc), SUM(dbl), AVG(dbl), COUNT(vc) FROM ov GROUP BY g ORDER BY g",
			"SELECT SUM(vc), AVG(vc), SUM(dbl), AVG(dbl) FROM ov",
			"SELECT SUM(vc) + 1 FROM ov WHERE g = 1",
			"SELECT SUM(dbl) = 0, SUM(dbl) IS NULL, SUM(dbl) > 1e308, CONCAT(SUM(dbl)), CAST(SUM(dbl) AS CHAR), ROUND(SUM(dbl)) FROM ov WHERE g = 1",
			"SELECT -SUM(dbl) FROM ov WHERE g = 1",
			"SELECT g, SUM(dbl) FROM ov GROUP BY g HAVING SUM(dbl) = 0 ORDER BY g",
			"SELECT SUM(DISTINCT vc), SUM(DISTINCT dbl), AVG(DISTINCT dbl) FROM ov WHERE g = 1",
			"SELECT SUM(x) FROM (SELECT 1e308 AS x UNION ALL SELECT 1e308) u",
			"SELECT SUM(x) FROM (SELECT '1e308' AS x UNION ALL SELECT '1e308') u",
			// ウィンドウ関数
			"SELECT id, SUM(vc) OVER (ORDER BY id), SUM(dbl) OVER (ORDER BY id), AVG(vc) OVER (ORDER BY id), AVG(dbl) OVER (ORDER BY id) FROM ov ORDER BY id",
			"SELECT id, SUM(vc) OVER (PARTITION BY g ORDER BY id), SUM(dbl) OVER (PARTITION BY g), AVG(dbl) OVER (PARTITION BY g) FROM ov ORDER BY id",
			"SELECT id, SUM(dbl) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), SUM(vc) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW), " +
				"AVG(dbl) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM ov ORDER BY id",
			"SELECT id, SUM(dbl) OVER (ORDER BY id ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING), AVG(vc) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) FROM ov ORDER BY id",
			// 枠の値がすべて NULL か、枠が空なら NULL（前は SUM が 0、DOUBLE の AVG が NaN）
			"SELECT id, SUM(vc) OVER w, AVG(dbl) OVER w, SUM(IF(vc IS NULL, NULL, g)) OVER w, AVG(IF(vc IS NULL, NULL, g)) OVER w FROM ov WINDOW w AS (ORDER BY id ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING) ORDER BY id",
			"SELECT id, SUM(dbl) OVER w, AVG(dbl) OVER w, SUM(g) OVER w, AVG(g) OVER w FROM ov WINDOW w AS (ORDER BY id ROWS BETWEEN 2 PRECEDING AND 1 PRECEDING) ORDER BY id",
		},
		drop: []string{"DROP TABLE ov"},
	},
	{
		name: "enumjoin", // 22. 3 度目のレビュー: ENUM か SET と文字列の列、定義の違う ENUM どうしの結合と比べは値の文字列（前はマージ結合が番号の順で突き合わせ、'a' が '1' に合った）
		setup: []string{
			"CREATE TABLE ej (id INT PRIMARY KEY, e ENUM('a','b','c','Dd') NOT NULL DEFAULT 'a', KEY (e)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT INTO ej VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'b'), (5, 'Dd'), (7, 'A')",
			"INSERT IGNORE INTO ej VALUES (6, 'bad')",
			"CREATE TABLE ev (id INT PRIMARY KEY, v VARCHAR(20), KEY (v)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT INTO ev VALUES (1, 'a'), (2, 'a '), (3, 'A'), (4, 'b'), (5, 'zz'), (6, ''), (7, NULL), (8, 'dd'), (9, '1'), (10, '2')",
			"CREATE TABLE ejp (id INT PRIMARY KEY, e ENUM('a','b','c','Dd') NOT NULL DEFAULT 'a', KEY (e)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO ejp SELECT * FROM ej",
			"CREATE TABLE evp (id INT PRIMARY KEY, v VARCHAR(20), KEY (v)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO evp SELECT * FROM ev",
			"CREATE TABLE ee (id INT PRIMARY KEY, e1 ENUM('a','b','c') NOT NULL, e2 ENUM('c','b','a') NOT NULL, e3 ENUM('a','b','c','d') NULL, KEY (e1), KEY (e2)) " +
				"DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT INTO ee VALUES (1, 'a', 'a', 'a'), (2, 'a', 'b', 'b'), (3, 'b', 'a', 'd'), (4, 'c', 'c', NULL), (5, 'b', 'b', 'c')",
			"CREATE TABLE es (id INT PRIMARY KEY, s SET('a','b','c') NOT NULL, s2 SET('c','b','a'), KEY (s)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT INTO es VALUES (1, 'a', 'a'), (2, 'b', 'a,b'), (3, 'a,b', 'b,a'), (4, '', 'c'), (5, 'c', '')",
		},
		both: []string{
			"SELECT a.id, b.id FROM ej a JOIN ev b ON a.e = b.v ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ej a JOIN ev b ON b.v = a.e ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ej a LEFT JOIN ev b ON a.e = b.v ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ev b LEFT JOIN ej a ON a.e = b.v ORDER BY b.id, a.id",
			"SELECT a.id, b.id FROM ej a JOIN ev b ON a.e = b.v AND b.id > 2 ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ej a JOIN ev b ON a.e = CONCAT(b.v, '') ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ejp a JOIN evp b ON a.e = b.v ORDER BY a.id, b.id",
			"SELECT id FROM ev WHERE v IN (SELECT e FROM ej) ORDER BY id",
			"SELECT id FROM ev WHERE v NOT IN (SELECT e FROM ej) ORDER BY id",
			"SELECT id FROM ev WHERE v = (SELECT e FROM ej WHERE id = 1) ORDER BY id",
			"SELECT id FROM ej WHERE e IN (SELECT v FROM ev) ORDER BY id",
			"SELECT id FROM ej a WHERE EXISTS (SELECT 1 FROM ev b WHERE b.v = a.e) ORDER BY id",
			"SELECT id FROM ej a WHERE NOT EXISTS (SELECT 1 FROM ev b WHERE b.v = a.e) ORDER BY id",
			"SELECT a.id, b.id, a.e = b.v, b.v = a.e, a.e < b.v FROM ej a JOIN ev b ON a.id = 1 ORDER BY b.id",
			// 定義の違う ENUM どうし（同じ定義なら番号で比べてよい）
			"SELECT a.id, b.id FROM ee a JOIN ee b ON a.e1 = b.e2 ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ee a JOIN ee b ON a.e1 = b.e3 ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ee a LEFT JOIN ee b ON a.e2 = b.e3 ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ee a JOIN ee b ON a.e1 = b.e1 ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM ee a JOIN ee b ON a.e1 = b.id ORDER BY a.id, b.id",
			"SELECT id, e1 = e2, e2 = e1, e1 = e3, e1 <> e2 FROM ee ORDER BY id",
			"SELECT id FROM ee WHERE e1 = e2 ORDER BY id",
			"SELECT a.id, b.id FROM ej a JOIN ej b ON a.e = b.e ORDER BY a.id, b.id",
			// SET
			"SELECT a.id, b.id FROM es a JOIN ev b ON a.s = b.v ORDER BY a.id, b.id",
			"SELECT a.id, b.id FROM es a JOIN es b ON a.s = b.s2 ORDER BY a.id, b.id",
			"SELECT id, s = s2 FROM es ORDER BY id",
			"SELECT id FROM ev WHERE v IN (SELECT s FROM es) ORDER BY id",
		},
		drop: []string{"DROP TABLE ej", "DROP TABLE ev", "DROP TABLE ejp", "DROP TABLE evp", "DROP TABLE ee", "DROP TABLE es"},
	},
	{
		name: "enumfunc", // 23. 3 度目のレビュー: 文字列の関数の ENUM と SET の引数は値の文字列（前は FIELD、STRCMP、REGEXP などが番号の文字列 '1' で比べた）、CAST(... AS CHAR) の照合順序は接続の照合順序（前はバイナリで並べた）
		setup: []string{
			"CREATE TABLE ef (id INT PRIMARY KEY, e ENUM('a','b','c','Dd') NOT NULL DEFAULT 'a', en ENUM('a','b','c','Dd') NULL, s SET('a','b','c') NULL, KEY (e)) " +
				"DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT IGNORE INTO ef VALUES (1, 'a', 'a', 'a'), (2, 'b', 'b', 'a,b'), (3, 'c', 'c', ''), (4, 'b', NULL, NULL), (5, 'Dd', 'dd', 'c'), (6, 'bad', 'bad', 'b'), (7, 'A', 'C', 'a,c')",
			"CREATE TABLE eft (id INT PRIMARY KEY, st ENUM('publish','draft','pending','Private') NOT NULL, KEY (st)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT IGNORE INTO eft VALUES (1, 'publish'), (2, 'draft'), (3, 'pending'), (4, 'Private'), (5, 'publish'), (6, 'oops')",
			"CREATE TABLE efv (id INT PRIMARY KEY, v VARCHAR(20), t LONGTEXT) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
			"INSERT INTO efv VALUES (1, 'b', 'b'), (2, 'B', 'B'), (3, 'a', 'a'), (4, 'Dd', 'Dd'), (5, 'c', 'c'), (6, 'A', 'A'), (7, '10', '10'), (8, '9', '9')",
		},
		both: []string{
			"SELECT id, st, FIELD(st, 'publish', 'draft', 'pending', 'Private') FROM eft ORDER BY id",
			"SELECT id FROM eft ORDER BY FIELD(st, 'publish', 'draft', 'pending', 'Private'), id",
			"SELECT id FROM eft ORDER BY FIELD(st, 'pending', 'publish') DESC, id",
			"SELECT id, FIELD('draft', st, 'x'), FIELD(st, 'PUBLISH'), FIELD(st, ''), FIELD(st, 1), FIELD(st, 2, 3), FIELD(e, en), FIELD(en, 'c', 'a'), FIELD(s, 'a,b', 'b') FROM eft JOIN ef USING (id) ORDER BY id",
			"SELECT id, STRCMP(e, 'a'), STRCMP('b', e), STRCMP(e, en), STRCMP(s, 'a,b') FROM ef ORDER BY id",
			"SELECT id FROM ef WHERE STRCMP(e, 'a') = 0 ORDER BY id",
			"SELECT id FROM ef WHERE e REGEXP '^a$' ORDER BY id",
			"SELECT id FROM ef WHERE s REGEXP 'b' ORDER BY id",
			"SELECT id, 'Dd' REGEXP e, 'xAy' REGEXP e FROM ef WHERE id <> 6 ORDER BY id",
			"SELECT id, e REGEXP '^[ab]', REGEXP_LIKE(e, 'd'), e RLIKE 'D', REGEXP_INSTR(e, 'b'), REGEXP_SUBSTR(e, '[a-z]+'), REGEXP_REPLACE(e, 'a', 'x'), REGEXP_REPLACE('xay', 'a', e) FROM ef ORDER BY id",
			"SELECT id, LOCATE('d', e), INSTR(e, 'D'), POSITION('b' IN e), LOCATE(e, 'xxDd'), FIND_IN_SET(e, 'x,b,a'), FIND_IN_SET('b', e), FIND_IN_SET(e, s), ELT(2, e, en), ELT(1, s) FROM ef ORDER BY id",
			"SELECT id, REPEAT(e, 2), ASCII(e), ORD(e), MD5(e), SHA1(e), SHA2(e, 256), CRC32(e), TO_BASE64(e), BIT_LENGTH(e), QUOTE(e), MD5(s), QUOTE(s) FROM ef ORDER BY id",
			"SELECT id, SOUNDEX(e) FROM ef WHERE id <> 6 ORDER BY id",
			// CAST(... AS CHAR) は接続の照合順序
			"SELECT id FROM ef ORDER BY CAST(e AS CHAR), id",
			"SELECT DISTINCT CAST(e AS CHAR) FROM ef ORDER BY 1",
			"SELECT CAST(e AS CHAR) AS c, COUNT(*) FROM ef GROUP BY c ORDER BY c",
			"SELECT id FROM efv ORDER BY CAST(t AS CHAR), id",
			"SELECT id FROM efv ORDER BY CAST(v AS CHAR) DESC, id",
			"SELECT id, CAST(e AS CHAR) = 'A', CAST(e AS CHAR) < 'b', COLLATION(CAST(e AS CHAR)), COLLATION(CAST(e AS CHAR CHARACTER SET utf8mb4)) FROM ef ORDER BY id",
		},
		stmts: []string{
			// WordPress の接続（utf8mb4_unicode_520_ci）
			"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci",
			"SELECT id FROM ef ORDER BY CAST(e AS CHAR), id",
			"SELECT id FROM efv ORDER BY CAST(t AS CHAR), id",
			"SELECT DISTINCT CAST(t AS CHAR) FROM efv ORDER BY 1",
			"SELECT COLLATION(CAST(t AS CHAR)), COLLATION(CAST(e AS CHAR CHARACTER SET utf8mb4)) FROM ef JOIN efv USING (id) WHERE id = 1",
			"SET NAMES utf8mb4",
		},
		drop: []string{"DROP TABLE ef", "DROP TABLE eft", "DROP TABLE efv"},
	},
	{
		name: "enumcoll", // 24. 3 度目のレビュー: ENUM と SET と COLLATE を付けた文字列の比べは、その照合順序（前は列の照合順序で、NO PAD の列の e = 'a ' COLLATE utf8mb4_bin が 'a' に合わなかった）
		setup: []string{
			"CREATE TABLE ec (id INT PRIMARY KEY, e ENUM('a','b','c','Dd') NOT NULL DEFAULT 'a', en ENUM('a','b','c','Dd') NULL, s SET('a','b','c') NULL, KEY (e)) " +
				"DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT IGNORE INTO ec VALUES (1, 'a', 'a', 'a'), (2, 'b', 'b', 'a,b'), (3, 'c', 'c', ''), (4, 'b', NULL, NULL), (5, 'Dd', 'dd', 'c'), (6, 'bad', 'bad', 'b'), (7, 'A', 'C', 'a,c')",
			"CREATE TABLE ecx (id INT PRIMARY KEY, e ENUM('a','b','c','Dd') NOT NULL DEFAULT 'a', en ENUM('a','b','c','Dd') NULL, s SET('a','b','c') NULL) " +
				"DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
			"INSERT INTO ecx SELECT * FROM ec",
		},
		both: []string{
			"SELECT id FROM ec WHERE e = 'a ' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT id FROM ecx WHERE e = 'a ' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT id FROM ec WHERE e = _utf8mb4'a ' COLLATE utf8mb4_unicode_520_ci ORDER BY id",
			"SELECT id FROM ecx WHERE e = _utf8mb4'A ' COLLATE utf8mb4_unicode_520_ci ORDER BY id",
			"SELECT id FROM ec WHERE e = 'A ' COLLATE utf8mb4_unicode_520_ci ORDER BY id",
			"SELECT id FROM ec WHERE e = 'A' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT id FROM ecx WHERE e = 'A' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT id FROM ec WHERE e = 'dd' COLLATE utf8mb4_0900_as_cs ORDER BY id",
			"SELECT id FROM ec WHERE e = 'Dd' COLLATE utf8mb4_0900_as_cs ORDER BY id",
			"SELECT id FROM ec WHERE e <> 'a ' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT id FROM ec WHERE 'a ' COLLATE utf8mb4_bin = e ORDER BY id",
			"SELECT id FROM ec WHERE e = 'a ' ORDER BY id",
			"SELECT id FROM ec WHERE e IN ('a ' COLLATE utf8mb4_bin, 'c') ORDER BY id",
			"SELECT id FROM ecx WHERE e IN ('a ' COLLATE utf8mb4_bin, 'c', 'zz', 'b ') ORDER BY id",
			"SELECT id FROM ec WHERE e NOT IN ('a ' COLLATE utf8mb4_bin, 'c') ORDER BY id",
			"SELECT id FROM ec WHERE e IN ('A' COLLATE utf8mb4_bin, 'c') ORDER BY id",
			"SELECT id FROM ec WHERE e IN ('a ', 'c') ORDER BY id",
			"SELECT id, e = 'a ' COLLATE utf8mb4_bin, e = 'A' COLLATE utf8mb4_bin, en = 'C ' COLLATE utf8mb4_unicode_520_ci, s = 'a,b ' COLLATE utf8mb4_bin, " +
				"s = 'A,B' COLLATE utf8mb4_bin FROM ec ORDER BY id",
			"SELECT id FROM ec WHERE s = 'a,c ' COLLATE utf8mb4_bin ORDER BY id",
			"SELECT COERCIBILITY(_utf8mb4'a ' COLLATE utf8mb4_unicode_520_ci), COLLATION(_utf8mb4'a ' COLLATE utf8mb4_unicode_520_ci)",
		},
		drop: []string{"DROP TABLE ec", "DROP TABLE ecx"},
	},
	{
		name: "dateprec", // 25. 4 度目のレビュー: 日時の式の小数の桁は MySQL と同じ（DATE_ADD は入力の桁、FROM_UNIXTIME(0) と TIMESTAMP('...') は 0、NOW(3) は 3。前は DATETIME(6) で、文字列の関数が '2024-01-02 10:00:00.000000' を見た）
		setup: []string{
			"CREATE TABLE dp (id INT PRIMARY KEY, post_date DATETIME NOT NULL, d3 DATETIME(3) NULL, ts2 TIMESTAMP(2) NULL, dd DATE NULL, vc VARCHAR(30) NULL)",
			"INSERT INTO dp VALUES (1, '2024-01-01 10:00:00', '2024-01-01 10:00:00.120', '2024-01-01 10:00:00.10', '2024-01-01', '2024-01-01 10:00:00'), " +
				"(2, '2024-01-02 10:00:00', '2024-01-02 10:00:00.000', '2024-01-02 10:00:00.00', '2024-01-02', '2024-01-02 10:00:00.5'), (3, '2024-01-03 10:00:00', NULL, NULL, NULL, NULL)",
		},
		both: []string{
			"SELECT id, STRCMP(DATE_ADD(post_date, INTERVAL 1 DAY), '2024-01-02 10:00:00'), DATE_ADD(post_date, INTERVAL 1 DAY) REGEXP '10:00:00$', MD5(DATE_ADD(post_date, INTERVAL 1 DAY)), " +
				"FIELD(DATE_ADD(post_date, INTERVAL 1 DAY), '2024-01-02 10:00:00', '2024-01-03 10:00:00'), QUOTE(DATE_ADD(post_date, INTERVAL 1 DAY)), BIT_LENGTH(DATE_ADD(post_date, INTERVAL 1 DAY)), " +
				"FIND_IN_SET(DATE_ADD(post_date, INTERVAL 1 DAY), 'x,2024-01-02 10:00:00'), REPEAT(DATE_SUB(post_date, INTERVAL 1 HOUR), 2), TO_BASE64(post_date + INTERVAL 1 DAY), MD5(post_date - INTERVAL 1 DAY) FROM dp ORDER BY id",
			"SELECT id FROM dp WHERE DATE_ADD(post_date, INTERVAL 1 DAY) REGEXP '^2024-01-0[2-4] 10:00:00$' ORDER BY id",
			"SELECT id FROM dp WHERE STRCMP(DATE_ADD(post_date, INTERVAL 1 DAY), '2024-01-02 10:00:00') = 0 ORDER BY id",
			"SELECT id FROM dp WHERE QUOTE(DATE_SUB(post_date, INTERVAL 0 DAY)) = '''2024-01-01 10:00:00''' ORDER BY id",
			// CONCAT、CAST、LENGTH と、入力の小数の桁（DATETIME(3)、TIMESTAMP(2)）、MICROSECOND、DATE と時刻の間隔
			"SELECT id, CONCAT(DATE_ADD(post_date, INTERVAL 1 DAY)), CAST(DATE_ADD(post_date, INTERVAL 1 DAY) AS CHAR), LENGTH(DATE_ADD(post_date, INTERVAL 1 DAY)), CONCAT(DATE_ADD(d3, INTERVAL 1 DAY)), " +
				"CONCAT(DATE_SUB(ts2, INTERVAL 1 SECOND)), CONCAT(d3 - INTERVAL 1 DAY), CONCAT(INTERVAL 1 DAY + d3), CONCAT(DATE_ADD(post_date, INTERVAL 1 MICROSECOND)), CONCAT(DATE_ADD(post_date, INTERVAL 0 MICROSECOND)), " +
				"CONCAT(DATE_ADD(dd, INTERVAL 1 HOUR)), CONCAT(DATE_ADD(dd, INTERVAL 1 DAY)), CONCAT(dd + INTERVAL 1 DAY), CONCAT(DATE_ADD(vc, INTERVAL 1 DAY)), CONCAT(ADDDATE(d3, 1)) FROM dp ORDER BY id",
			"SELECT id, DATE_ADD(post_date, INTERVAL 1 DAY), DATE_ADD(d3, INTERVAL 1 DAY), DATE_SUB(ts2, INTERVAL 1 SECOND), d3 - INTERVAL 1 DAY, post_date + INTERVAL 1 HOUR, DATE_ADD(post_date, INTERVAL 1 MICROSECOND), " +
				"DATE_ADD(dd, INTERVAL 1 HOUR), DATE_ADD(dd, INTERVAL 1 DAY), dd + INTERVAL 1 DAY, DATE_ADD(vc, INTERVAL 1 DAY) FROM dp ORDER BY id",
			"SELECT STRCMP(FROM_UNIXTIME(0), '1970-01-01 00:00:00'), CONCAT(FROM_UNIXTIME(0)), BIT_LENGTH(FROM_UNIXTIME(0)), FROM_UNIXTIME(1.5), FROM_UNIXTIME(1.50), CONCAT(FROM_UNIXTIME(1.50)), FROM_UNIXTIME(1.1234567), " +
				"FROM_UNIXTIME('1.5'), FROM_UNIXTIME(1e0), FROM_UNIXTIME(1.9999999), FROM_UNIXTIME(-1), FROM_UNIXTIME(NULL), FROM_UNIXTIME(1700000000), MD5(FROM_UNIXTIME(1700000000)), FROM_UNIXTIME(1.5, '%s.%f')",
			// IF、CASE、COALESCE、IFNULL、GREATEST、LEAST は枝の中で最も多い小数の桁
			"SELECT id, IF(id = 1, post_date, NULL), CONCAT(IF(id = 1, post_date, NULL)), MD5(IF(id = 1, post_date, NULL)), QUOTE(IF(id = 1, post_date, NULL)), IF(id = 1, post_date, d3), CONCAT(IF(id = 1, post_date, d3)), " +
				"IF(id = 1, d3, NULL), CONCAT(GREATEST(post_date, '2024-01-01')), MD5(GREATEST(post_date, '2024-01-01')), GREATEST(post_date, d3), LEAST(d3, ts2), CONCAT(COALESCE(post_date, '')), MD5(COALESCE(post_date, '')), " +
				"COALESCE(d3, post_date), COALESCE(post_date, d3), IFNULL(d3, post_date), IFNULL(post_date, d3), CASE WHEN id = 1 THEN post_date ELSE d3 END, CONCAT(CASE WHEN id = 1 THEN dd ELSE d3 END), " +
				"IF(id = 1, dd, post_date), IF(id = 1, dd, NULL) FROM dp ORDER BY id",
			"SELECT id, TIMESTAMP('2024-01-01 10:00:00'), CONCAT(TIMESTAMP('2024-01-01 10:00:00')), BIT_LENGTH(TIMESTAMP('2024-01-01 10:00:00')), MD5(TIMESTAMP('2024-01-01 10:00:00')), TIMESTAMP('2024-01-01 10:00:00.50'), " +
				"CONCAT(TIMESTAMP('2024-01-01 10:00:00.50')), TIMESTAMP('2024-01-01'), TIMESTAMP(dd), CONCAT(TIMESTAMP(dd)), TIMESTAMP(d3), TIMESTAMP(post_date), TIMESTAMP(vc), CONCAT(TIMESTAMP(ts2)), TIMESTAMP(20240101) FROM dp ORDER BY id",
			// 今の時刻は長さだけ（NOW(3) は 3 桁、UTC_TIMESTAMP() は小数なし、CURTIME(2) は 2 桁）
			"SELECT BIT_LENGTH(NOW()), BIT_LENGTH(NOW(3)), BIT_LENGTH(NOW(6)), BIT_LENGTH(CURTIME()), BIT_LENGTH(CURTIME(2)), LENGTH(CURRENT_TIME(1)), LENGTH(CURTIME(6)), BIT_LENGTH(UTC_TIMESTAMP()), " +
				"BIT_LENGTH(UTC_TIMESTAMP(3)), LENGTH(SYSDATE(4)), LENGTH(CURRENT_TIMESTAMP(1)), LENGTH(LOCALTIME(2)), LENGTH(LOCALTIMESTAMP(5)), LENGTH(CONCAT(NOW(3))), LENGTH(CAST(NOW(2) AS CHAR)), " +
				"CONCAT(UTC_TIMESTAMP()) REGEXP '[.]', NOW() = UTC_TIMESTAMP()",
			// 列は前と同じく型の桁（DATETIME(3) の .000）
			"SELECT id, d3, CONCAT(d3), CAST(d3 AS CHAR), QUOTE(d3), LENGTH(d3), ts2, CONCAT(ts2), CONCAT(post_date), CAST(post_date AS CHAR), CONCAT(dd), CONCAT(CAST(d3 AS DATETIME(2))), " +
				"CONCAT(CAST(post_date AS DATETIME(2))), STRCMP(d3, '2024-01-01 10:00:00.120'), MD5(ts2) FROM dp ORDER BY id",
		},
		drop: []string{"DROP TABLE dp"},
	},
}

// stage116Posts は、レビューの組の wp_posts（WordPress の sql_mode で作る）。
var stage116Posts = []string{
	wpSQLMode,
	"CREATE TABLE wp_posts (ID BIGINT UNSIGNED PRIMARY KEY, post_author BIGINT UNSIGNED NOT NULL DEFAULT 0, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', " +
		"post_date_gmt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_title TEXT, comment_count BIGINT NOT NULL DEFAULT 0)",
	"INSERT INTO wp_posts VALUES (1, 1, '2024-01-03 10:00:00', '2024-01-03 01:00:00', 'Hello world', 2), (2, 1, '2024-01-01 10:00:00', '0000-00-00 00:00:00', 'A much longer title', 0), " +
		"(3, 2, '2024-01-02 10:00:00', '2024-01-02 01:00:00', 'Mid', 1), (4, 2, '2024-01-02 12:00:00', '0000-00-00 00:00:00', 'Draft post', 0), (5, 3, '2023-12-31 10:00:00', '2023-12-31 01:00:00', 'x', 0)",
	"SET SESSION sql_mode = DEFAULT",
}

// stage116Statements は、組をつないだ文の列。
func stage116Statements() []string {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage116Groups {
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

func TestStage116Fixes(t *testing.T) {
	checkGolden(t, "../testdata/stage116_golden.txt", "heimdb_golden116", stage116Statements())
}

// stage116PlanDB は、組 name の setup を流したサーバを返す（計画を見るテスト用）。
func stage116PlanDB(t *testing.T, name string) (*sql.DB, func()) {
	t.Helper()
	dsn, stop := startServer(t, t.TempDir())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, g := range stage116Groups {
		if g.name == name {
			for _, q := range g.setup {
				mustExec(t, db, q)
			}
		}
	}
	return db, func() { db.Close(); stop() }
}

// 段階 116 の 3 の文が、lookup 結合と索引の範囲の道を通ることを確かめる（道が変わると、ゴールデンの文がその道を確かめなくなる）。
func TestStage116NumberStringPlans(t *testing.T) {
	db, stop := stage116PlanDB(t, "numstr")
	defer stop()
	mustExec(t, db, wpSQLMode)
	for _, c := range []struct{ q, want string }{
		{"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = pm.meta_value ORDER BY pm.meta_id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = pm.meta_value + 0.0 ORDER BY pm.meta_id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.i = pm.meta_value ORDER BY pm.meta_id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.ti = pm.meta_value ORDER BY pm.meta_id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(pm, u32) */ pm.meta_id, u32.id FROM pm JOIN u32 ON u32.id = CAST(pm.meta_value AS DOUBLE) ORDER BY pm.meta_id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(pm, p) */ pm.meta_id, p.ID FROM pm LEFT JOIN p ON p.ID = pm.meta_value ORDER BY pm.meta_id", "LeftOuterLookupJoin"},
		// 日時でない文字列はゼロの日時の点
		{"SELECT ID FROM p WHERE post_date IN ('garbage', '2024-01-01 10:00:00') ORDER BY ID", "filters: [{[0000-01-01 00:00:00 +0000 UTC, 0000-01-01 00:00:00 +0000 UTC]}, {[2024-01-01 10:00:00 +0000 UTC, 2024-01-01 10:00:00 +0000 UTC]}]"},
	} {
		if plan := explain(t, db, c.q); !strings.Contains(plan, c.want) {
			t.Errorf("%s\nplan has no %s:\n%s", c.q, c.want, plan)
		}
	}
}

// 段階 116 の 3 度目のレビュー: バイナリプロトコル（プリペアドステートメント。go-sql-driver は引数のある Query と
// Prepare で使う）では、DOUBLE の上限を超えた SUM と AVG を MySQL と同じく IEEE の無限大のまま送る（前は 55bcf90 の
// テキストの '0' を vitess が読み直して 0 になった）。テキストプロトコルは MySQL と同じく '0'。期待の値は本物の MySQL
// 8.0 で PHP の mysqli の get_result（バイナリ）と query（テキスト）で確かめた。
func TestStage116BinaryProtocolDouble(t *testing.T) {
	db, stop := stage116PlanDB(t, "sumovf")
	defer stop()
	const q = "SELECT SUM(dbl), AVG(dbl), -SUM(dbl), SUM(vc), MAX(dbl), SUM(dbl) > 1e308 FROM ov WHERE g = 1"
	inf, ninf := math.Inf(1), math.Inf(-1)
	wantBinary := []float64{inf, inf, ninf, inf, 1e308, 1}
	// 引数のある Query はバイナリプロトコル（interpolateParams は既定で false）
	for _, args := range [][]any{nil, {1}} {
		query := q
		if args != nil {
			query += " AND id >= ?"
		}
		stmt, err := db.Prepare(query)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]float64, len(wantBinary))
		ptrs := make([]any, len(got))
		for i := range got {
			ptrs[i] = &got[i]
		}
		if err := stmt.QueryRow(args...).Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		stmt.Close()
		for i := range got {
			if got[i] != wantBinary[i] {
				t.Errorf("%s: column %d over the binary protocol = %v, want %v", query, i, got[i], wantBinary[i])
			}
		}
	}
	// ウィンドウ関数は上限、範囲の中の値はそのまま
	var w, d float64
	stmt, err := db.Prepare("SELECT SUM(dbl) OVER (ORDER BY id), dbl FROM ov WHERE id = ? OR id = 2 ORDER BY id DESC LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if err := stmt.QueryRow(1).Scan(&w, &d); err != nil {
		t.Fatal(err)
	}
	if w != math.MaxFloat64 || d != 1e308 {
		t.Errorf("window SUM and dbl over the binary protocol = %v, %v, want %v, 1e308", w, d, math.MaxFloat64)
	}
	// テキストプロトコルは '0'
	var s1, s2 string
	if err := db.QueryRow("SELECT SUM(dbl), -SUM(dbl) FROM ov WHERE g = 1").Scan(&s1, &s2); err != nil {
		t.Fatal(err)
	}
	if s1 != "0" || s2 != "0" {
		t.Errorf("SUM(dbl), -SUM(dbl) over the text protocol = %q, %q, want \"0\", \"0\"", s1, s2)
	}
}
