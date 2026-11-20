package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// stage114WPPosts は、段階 114 の文で使う wp_posts（WordPress の列と索引のうち、日時と種類と状態のもの）。
const stage114WPPosts = "CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', " +
	"post_date_gmt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_title TEXT NOT NULL, post_status VARCHAR(20) NOT NULL DEFAULT 'publish', " +
	"post_type VARCHAR(20) NOT NULL DEFAULT 'post', KEY type_status_date (post_type, post_status, post_date, ID)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci"

// stage114Groups は、段階 114 で取り込んだ上流の go-mysql-server の修正の再現の文（docs/engine-upstream.md）。
// 直す前の HeimDB は本物の MySQL 8.0 と違う結果かエラーを返した。各組の終わりに作った表を消し、sql_mode を既定に戻す。
var stage114Groups = []struct {
	pr    int
	stmts []string
}{
	{3276, []string{ // 後ろに余計な文字のある日時の文字列（CAST、比べる、INSERT）、前後の空白（#3675）、'2024/01/05 3:4:5' の形（#3723）
		"SELECT CAST('2020-01-01 a' AS DATETIME), CAST('2020-01-01 12:30asdf123' AS DATETIME), CAST('2020-01-01 12:34:56abc' AS DATETIME)",
		"SHOW WARNINGS",
		"SELECT CAST('2020-01-01 12:34:56 abc' AS DATETIME), CAST('13:04:05' AS DATE), CAST('24-01-02' AS DATE), CAST('99/12/31 23:59' AS DATETIME)",
		"SELECT CAST('2020-01-01 a' AS DATE), CAST('2020-01-01abc123' AS DATE), CAST('2020-01-01 10:00:00 x' AS DATETIME), CAST('20200101abc' AS DATE)",
		"SELECT CAST('2020-01-01 12:34:56-123456 abc' AS DATETIME), CAST('2020-01-01 12:34:56.5x' AS DATETIME(1)), CAST('2020-02-30 x' AS DATE), CAST('2020-01-01 25:00:00' AS DATETIME)",
		"SELECT CAST(' 2024-01-01 ' AS DATE), CAST(CONCAT(' ', '2024-01-01') AS DATE), DATE(' 2024-01-01 '), CAST(' 2024-01-01 10:20:30 ' AS DATETIME), CAST('\t2024-01-01\n' AS DATE)",
		"SELECT CAST('2024/01/05 3:4:5' AS DATETIME), CAST('2024-1-5 3:4:5' AS DATETIME), CAST('2024.01.05 03:04:05' AS DATETIME), CAST('2024/1/5' AS DATE), CAST('2024-01-05 12' AS DATETIME), CAST('2024-01-05T03:04:05' AS DATETIME)",
		"SELECT CAST('2012-01-01 103' AS DATETIME), CAST('20240-01-01' AS DATE), CAST('2024-011-01' AS DATE), CAST('2024-01-01 10:301' AS DATETIME), CAST('2024-01-01 10:30:001' AS DATETIME), CAST('2012-01-01T00:00:00+0100' AS DATETIME)",
		"SELECT CAST('2007-12-11 20:21:22+09:00' AS DATETIME), '2020-01-01 a' = DATE('2020-01-01'), CAST('2020-01-01 a' AS DATE) = '2020-01-01'",
		"SELECT DATE('2024-01-02 xyz'), YEAR('2024-01-02 xyz'), DATE(20010203), DATE(20240102030405), DATE(240102), YEAR(20010203), MONTH(991231), DAY(20010203.5), DATE(1), DATE(99991231), DATE(20010230)",
		"SELECT HOUR('13:04:05'), MINUTE('13:04:05'), SECOND('13:04:05'), HOUR('2024-01-02 13:04:05'), HOUR('2024-01-02'), MINUTE('2024-01-02'), HOUR(130405), SECOND('2007-12-11T20:21:22Z'), MINUTE('2007-12-11T20:21:22Z')",
		"SHOW WARNINGS",
		"SELECT HOUR('25:00:00'), HOUR('-13:04:05'), HOUR('838:59:59'), MINUTE('13:04'), SECOND('1:2:3'), HOUR(NULL), HOUR('abc'), SECOND('2007-12-11 20:21:22Z'), HOUR('20240102103000')",
		"CREATE TABLE m (id INT PRIMARY KEY, v LONGTEXT)",
		"INSERT INTO m VALUES (1, '2020-01-01 a'), (2, '2019-12-31'), (3, ' 2024-05-01'), (4, '2024/04/01 10:00:00'), (5, '13:04:05'), (6, '2024-01-01 09:00:00 x'), (7, '2024/1/3 3:4:5')",
		"SELECT id FROM m WHERE CAST(v AS DATE) >= '2020-01-01' ORDER BY id",
		"SELECT id, CAST(v AS DATE), CAST(v AS DATETIME), DATE(v), HOUR(v) FROM m ORDER BY id",
		// 既定の sql_mode（STRICT）の INSERT はエラー 1292
		"CREATE TABLE t (id INT PRIMARY KEY, d DATETIME, dd DATE)",
		"INSERT INTO t VALUES (1, '2020-01-01 a', NULL)",
		"INSERT INTO t VALUES (2, NULL, '2020-01-01 a')",
		"INSERT INTO t VALUES (3, ' 2020-01-02 10:00:00 ', ' 2020-01-02 ')",
		"INSERT INTO t VALUES (4, '2020/01/03 1:2:3', '2020/1/3')",
		"SELECT * FROM t ORDER BY id",
		wpSQLMode,
		"INSERT INTO t VALUES (5, '2020-01-01 a', '2020-01-01 a')",
		"SHOW WARNINGS",
		"INSERT INTO t VALUES (6, '2020-01-01 12:34:56x', '2020-01-01abc'), (7, 'abc', 'abc'), (8, '20200101abc', '2020-13-01 x')",
		"SHOW WARNINGS",
		"UPDATE t SET d = '2021-02-03 04:05:06extra' WHERE id = 3",
		"SHOW WARNINGS",
		"SELECT * FROM t ORDER BY id",
		"SELECT id FROM t WHERE d = '2020-01-01 a' ORDER BY id",
		"SELECT id FROM t WHERE dd IN ('2020-01-01 a', '2020/1/3') ORDER BY id",
		"SELECT DATE('0000-00-00'), DATE(0), CAST('0000-00-00' AS DATE), DATE(false), YEAR('0000-00-00'), DATE(' 0000-00-00 ')",
		// WordPress の形: 投稿の日時の比べと WP_Date_Query の HOUR、DATE（索引のある post_date の範囲と、切り詰めた値）
		stage114WPPosts,
		"INSERT INTO wp_posts (post_date, post_date_gmt, post_title, post_status) VALUES ('2024-01-01 09:00:00', '2024-01-01 00:00:00', 'a', 'publish'), (' 2024-01-02 13:04:05', '2024-01-02 04:04:05', 'b', 'publish'), ('2024/01/03 3:4:5', '2024-01-02 18:04:05', 'c', 'publish'), ('2024-01-04 10:00:00 JST', '0000-00-00 00:00:00', 'd', 'draft')",
		"SELECT ID, post_date, post_date_gmt FROM wp_posts ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date >= '2024-01-02 a' ORDER BY post_date DESC",
		"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_date < ' 2024-01-02 13:04:05 ' ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE post_type = 'post' AND post_date = '2024/01/03 03:04:05' ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE HOUR(wp_posts.post_date) = 13 AND DATE(wp_posts.post_date) = '2024-01-02' ORDER BY ID",
		"SELECT p.ID, m.id FROM wp_posts p JOIN m ON p.post_date = m.v ORDER BY p.ID, m.id",
		"SELECT p.ID, m.id FROM wp_posts p JOIN m ON DATE(p.post_date) = CAST(m.v AS DATE) ORDER BY p.ID, m.id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_posts",
		"DROP TABLE t",
		"DROP TABLE m",
	}},
	{3761, []string{ // STR_TO_DATE の NULL の引数（#3761）と %p（#3771）、日時の列の引数、月か日が 0 の日付、%f、時刻の付いたゼロの日付の DATE_FORMAT
		"SELECT STR_TO_DATE(NULL, '%Y-%m-%d'), STR_TO_DATE('2024-01-02', NULL), STR_TO_DATE(NULL, NULL)",
		"SELECT STR_TO_DATE('2024-03-05 09:30:17 PM', '%Y-%m-%d %h:%i:%s %p'), STR_TO_DATE('2024-03-05 12:05:00 AM', '%Y-%m-%d %h:%i:%s %p'), STR_TO_DATE('2024-03-05 12:05:00 PM', '%Y-%m-%d %h:%i:%s %p'), STR_TO_DATE('09:30:17 pm','%h:%i:%s %p'), STR_TO_DATE('01/02/99 05:14:12 PM', '%m/%e/%y %r')",
		"SELECT STR_TO_DATE('12', '%h'), STR_TO_DATE('12 AM', '%h %p'), STR_TO_DATE('13:00 PM', '%h:%i %p'), STR_TO_DATE('0:30 AM', '%h:%i %p'), STR_TO_DATE('10:23 PM', '%H:%i %p'), STR_TO_DATE('March 5, 2024 7:15 pm', '%M %e, %Y %l:%i %p')",
		"SHOW WARNINGS",
		"SELECT STR_TO_DATE('0000', '%Y'), STR_TO_DATE('2024', '%Y'), STR_TO_DATE('0', '%Y'), STR_TO_DATE('2024-05', '%Y-%m'), STR_TO_DATE('9', '%m'), STR_TO_DATE('2024-00-00', '%Y-%m-%d')",
		"SHOW WARNINGS",
		"SELECT STR_TO_DATE('2024-01-01 12:34:56.5', '%Y-%m-%d %H:%i:%s.%f'), STR_TO_DATE('01/02/99 314', '%m/%e/%y %f'), STR_TO_DATE(1.5, '%s.%f'), STR_TO_DATE(20240102, '%Y%m%d')",
		"SELECT DATE_FORMAT('0000-00-00 12:34:56', '%H'), DATE_FORMAT('0000-00-00 12:34:56', '%Y-%m-%d %H:%i:%s'), DATE_FORMAT('abc', '%Y'), DATE_FORMAT('2024-01-02 x', '%Y-%m-%d')",
		"CREATE TABLE s (id INT PRIMARY KEY, d DATETIME, dd DATE, n INT, v VARCHAR(30))",
		"INSERT INTO s VALUES (1, '2024-01-02 03:04:05', '2024-01-02', 20240102, '2024-01-02'), (2, NULL, NULL, NULL, NULL), (3, '2024-12-31 23:59:59', '2024-12-31', 20241231, '12/31/2024 11:59 PM')",
		"SELECT id, STR_TO_DATE(d, '%Y-%m-%d %H:%i:%s'), STR_TO_DATE(dd, '%Y-%m-%d'), STR_TO_DATE(n, '%Y%m%d'), STR_TO_DATE(v, '%Y-%m-%d'), STR_TO_DATE(v, '%m/%d/%Y %h:%i %p'), STR_TO_DATE(v, NULL) FROM s ORDER BY id",
		wpSQLMode,
		"SELECT STR_TO_DATE('0000', '%Y'), STR_TO_DATE('0', '%Y'), STR_TO_DATE('2024', '%Y'), DATE_FORMAT('0000-00-00 12:34:56', '%H'), DATE_FORMAT('0000-00-00 12:34:56', '%Y-%m-%d %H:%i:%s')",
		// WordPress の形: 投稿の日時を文字列にして読み戻す（プラグインの集計）と、メタの値の 12 時間の時刻（イベントのプラグイン）
		stage114WPPosts,
		"INSERT INTO wp_posts (post_date, post_date_gmt, post_title, post_status) VALUES ('2024-01-01 09:00:00', '2024-01-01 00:00:00', 'a', 'publish'), ('2024-01-02 21:30:00', '2024-01-02 12:30:00', 'b', 'publish'), ('2024-01-03 10:00:00', '0000-00-00 00:00:00', 'c', 'draft')",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_event_start', '01/05/2024 07:30 PM'), (2, '_event_start', '01/06/2024 12:15 AM'), (3, '_event_start', NULL), (3, '_event_start', '')",
		"SELECT ID, STR_TO_DATE(post_date, '%Y-%m-%d %H:%i:%s'), DATE(STR_TO_DATE(post_date_gmt, '%Y-%m-%d %H:%i:%s')) FROM wp_posts ORDER BY ID",
		"SELECT p.ID, STR_TO_DATE(m.meta_value, '%m/%d/%Y %h:%i %p') AS s FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_event_start' ORDER BY s, p.ID",
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID WHERE m.meta_key = '_event_start' AND STR_TO_DATE(m.meta_value, '%m/%d/%Y %h:%i %p') >= '2024-01-05 19:00:00' ORDER BY p.ID",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE s",
	}},
	{3409, []string{ // TIMESTAMPDIFF の YEAR（#3407）、MONTH、QUARTER（#3409）を暦の月と時刻で数える
		"SELECT TIMESTAMPDIFF(YEAR, DATE '2011-07-05', DATE '2026-07-04'), TIMESTAMPDIFF(YEAR, DATE '2026-07-04', DATE '2011-07-05'), TIMESTAMPDIFF(YEAR, '2024-02-29', '2025-02-28'), TIMESTAMPDIFF(YEAR, '2024-01-01 10:00:00', '2025-01-01 09:59:59'), TIMESTAMPDIFF(YEAR, '1990-06-15', '2024-06-15')",
		"SELECT TIMESTAMPDIFF(MONTH, '2026-01-31', '2026-03-01'), TIMESTAMPDIFF(MONTH, '2026-01-31', '2026-02-28'), TIMESTAMPDIFF(MONTH, '2026-03-01', '2026-01-31'), TIMESTAMPDIFF(QUARTER, '2025-01-01', '2025-12-31'), TIMESTAMPDIFF(QUARTER, '2025-01-01', '2026-01-01'), TIMESTAMPDIFF(MONTH, '2024-01-15 12:00:00', '2024-02-15 11:59:59.999999'), TIMESTAMPDIFF(MONTH, '2024-01-15 12:00:00.5', '2024-02-15 12:00:00.4')",
		"SELECT TIMESTAMPDIFF(MONTH, '2024-01-15', '2024-02-15 x'), TIMESTAMPDIFF(DAY, '2024-01-01', '2024-03-01'), TIMESTAMPDIFF(YEAR, '0000-00-00', '2024-01-01'), TIMESTAMPDIFF(MONTH, NULL, '2024-01-01'), timestampdiff(month, '2023-12-31', '2024-12-30')",
		wpSQLMode,
		// WordPress の形: 会員の年齢や在籍の月数（ユーザーの登録日、メタの誕生日）
		"CREATE TABLE wp_users (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, user_login VARCHAR(60) NOT NULL DEFAULT '', user_registered DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00') DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_users (user_login, user_registered) VALUES ('a', '2020-02-29 10:00:00'), ('b', '2024-01-31 23:59:59'), ('c', '2023-03-01 00:00:00'), ('d', '0000-00-00 00:00:00')",
		"SELECT ID, TIMESTAMPDIFF(YEAR, user_registered, '2025-02-28 10:00:00'), TIMESTAMPDIFF(MONTH, user_registered, '2024-02-29 23:59:59'), TIMESTAMPDIFF(QUARTER, user_registered, '2024-06-01') FROM wp_users ORDER BY ID",
		"SELECT ID FROM wp_users WHERE TIMESTAMPDIFF(MONTH, user_registered, '2024-03-01') >= 12 ORDER BY ID",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_users",
	}},
	{3025, []string{ // 数: IF の型（#3025）、文字列の真偽（#3215）、ABS（#3365）、GREATEST/LEAST の DECIMAL と日付（#3447）、指数の数字のない '1E'（#3920）、STRICT の数のエラー
		"SELECT IF(1=0, 0, 2.5), IF(1=1, 2.5, 0), IF(1=0, 1, 2.75), IF(1=0, TRUE, 5), IF(1=1, TRUE, 5), IF(1=1, 1, 'a'), IF(1=0, 1, 1.5e0), IF(1=1, NULL, 2.5)",
		"CREATE TABLE t0 (c0 INT)",
		"INSERT INTO t0 VALUES (1), (123), (2)",
		"SELECT IF(c0 = 123, TRUE, c0) AS ref0, MIN(c0) FROM t0 GROUP BY ref0 ORDER BY ref0",
		"SELECT COUNT(DISTINCT IF(c0 > 1, TRUE, c0)) FROM t0",
		"CREATE TABLE w (id INT PRIMARY KEY, r DECIMAL(19,4), q INT, u BIGINT UNSIGNED, f DOUBLE, s VARCHAR(10))",
		"INSERT INTO w VALUES (1, 12.3400, 2, 18446744073709551615, 1.5, '7'), (2, -1.5, 3, 1, 2.25, 'x')",
		"SELECT id, IF(id=2, 0, r), IF(id=1, r, 0), IF(id=1, q, r), IF(id=1, u, q), IF(id=1, f, q), IF(id=1, s, q), IF(id=1, q, NULL) FROM w ORDER BY id",
		"SELECT SUM(IF(id=1, f, 0)), SUM(IF(id=1, 1, 0)), MAX(IF(id=1, r, 0)), MIN(IF(id=2, r, 0)) FROM w",
		"SELECT '3bxu' OR FALSE, '3bxu' AND TRUE, ' 1' AND TRUE, 'a' OR FALSE, '' OR FALSE, '0.0a' OR FALSE, ' 0 ' OR FALSE, '1e' AND TRUE, NOT '3x', '-2abc' AND 1",
		"SHOW WARNINGS",
		"CREATE TABLE b (s VARCHAR(10))",
		"INSERT INTO b VALUES ('1a'), ('0a'), ('a'), (' 2'), ('0.5x'), ('')",
		"SELECT s FROM b WHERE s ORDER BY s",
		"SELECT s FROM b WHERE NOT s ORDER BY s",
		"SELECT ABS('hi'), ABS('12.3hi'), ABS('-342.12hi'), ABS(1 AND TRUE), ABS('-5'), ABS(' -7 '), ABS(NULL), ABS(-2.50), ABS(-1.5e0)",
		"SHOW WARNINGS",
		"SELECT s, ABS(s) FROM b ORDER BY ABS(s), s",
		"SELECT GREATEST(10.5, 9.75), LEAST(10.5, 9.75), GREATEST(1, 2.5), LEAST(3, 2.50), GREATEST(1.10, 1.1), GREATEST(0.1, 1e0)",
		"SELECT id, GREATEST(r, q), LEAST(r, q, 0), GREATEST(r, f), GREATEST(q, 2.5) FROM w ORDER BY id",
		"SELECT ROUND('1E'), '1E' + 0, CAST('1E' AS DECIMAL(10,2)), CAST('1E5' AS DECIMAL(10,2)), CAST('1E+' AS DECIMAL(10,2)), CAST('1.5E' AS DECIMAL(10,2))",
		"SHOW WARNINGS",
		"SELECT ROUND('1x'), ROUND(HEX(255)), ROUND('12.4e'), CEIL('2.1e+'), FLOOR('3.7E'), ROUND('-2.4x', 0)",
		"SHOW WARNINGS",
		// 既定の sql_mode（STRICT）の INSERT の数のエラー（MySQL は範囲の外が 1264、整数と DOUBLE の切り詰めが 1265）
		"CREATE TABLE dd (id INT, d DECIMAL(10,2), e DOUBLE, i INT, t TINYINT, ui INT UNSIGNED)",
		"INSERT INTO dd (id, d) VALUES (1, '1E')",
		"INSERT INTO dd (id, e) VALUES (2, '2.5e-')",
		"INSERT INTO dd (id, i) VALUES (3, '3E')",
		"INSERT INTO dd (id, i) VALUES (4, '12abc')",
		"INSERT INTO dd (id, e) VALUES (5, '12abc')",
		"INSERT INTO dd (id, d) VALUES (6, '12abc')",
		"INSERT INTO dd (id, i) VALUES (7, 'abc')",
		"INSERT INTO dd (id, i) VALUES (8, 99999999999)",
		"INSERT INTO dd (id, t) VALUES (9, 300)",
		"INSERT INTO dd (id, ui) VALUES (10, -1)",
		"INSERT INTO dd (id, d) VALUES (11, 123456789.5)",
		"INSERT INTO dd (id, i) VALUES (12, '99999999999')",
		"INSERT INTO dd (id, i) VALUES (13, 1), (14, 99999999999)",
		wpSQLMode,
		"INSERT INTO dd VALUES (15, '1E', '1E', '1E', '1e+', '2E')",
		"SHOW WARNINGS",
		"INSERT INTO dd VALUES (16, '1Ex', '2Ey', '3Ez', 1, 1)",
		"SHOW WARNINGS",
		"SELECT * FROM dd ORDER BY id",
		"SELECT ROUND('1E'), CAST('1E' AS DECIMAL(10,2)), IF(1=0, 0, 2.5)",
		// WooCommerce の形: 注文の集計（wc_order_stats の IF の合計）と、商品の価格（wc_product_meta_lookup の DECIMAL）の GREATEST/LEAST
		"CREATE TABLE wp_wc_order_stats (order_id BIGINT UNSIGNED NOT NULL PRIMARY KEY, parent_id BIGINT UNSIGNED NOT NULL DEFAULT 0, date_created DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', num_items_sold INT NOT NULL DEFAULT 0, total_sales DOUBLE NOT NULL DEFAULT 0, net_total DOUBLE NOT NULL DEFAULT 0, returning_customer TINYINT(1) DEFAULT NULL, status VARCHAR(200) NOT NULL, customer_id BIGINT UNSIGNED NOT NULL, KEY date_created (date_created), KEY status (status(191)))",
		"INSERT INTO wp_wc_order_stats VALUES (1, 0, '2024-01-05 10:00:00', 2, 120.5, 100.25, 0, 'wc-completed', 7), (2, 0, '2024-01-20 11:00:00', 1, 30, 25.5, 1, 'wc-processing', 8), (3, 0, '2024-02-02 12:00:00', 3, 99.99, 80, 1, 'wc-refunded', 7), (4, 1, '2024-02-03 13:00:00', 1, 10, 8, NULL, 'wc-completed', 9)",
		"SELECT DATE_FORMAT(date_created, '%Y-%m') AS m, SUM(IF(status = 'wc-completed', net_total, 0)) AS a, SUM(IF(status IN ('wc-completed','wc-processing'), num_items_sold, 0)) AS b, SUM(IF(returning_customer = 1, 1, 0)) AS c, COUNT(DISTINCT IF(status <> 'wc-refunded', customer_id, NULL)) AS d, SUM(IF(parent_id = 0, 1, FALSE)) AS e FROM wp_wc_order_stats GROUP BY m ORDER BY m",
		"SELECT IF(returning_customer = 1, TRUE, returning_customer) AS rc, COUNT(*) FROM wp_wc_order_stats GROUP BY rc ORDER BY rc",
		"CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL PRIMARY KEY, sku VARCHAR(100) DEFAULT '', `virtual` TINYINT(1) DEFAULT 0, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, onsale TINYINT(1) DEFAULT 0, stock_quantity DOUBLE NULL DEFAULT NULL, KEY min_max_price (min_price, max_price))",
		"INSERT INTO wp_wc_product_meta_lookup VALUES (10, 'a', 0, 9.5000, 12.0000, 1, 3), (11, 'b', 0, 20.0000, 20.0000, 0, NULL), (12, 'c', 1, NULL, NULL, 0, 0), (13, 'd', 0, 0.9900, 1.5000, 1, 7.5)",
		"SELECT product_id, GREATEST(min_price, 10), LEAST(max_price, 15.5), GREATEST(min_price, max_price), IF(onsale, min_price, max_price), IF(onsale = 1, min_price, 0), ABS(min_price - 10) FROM wp_wc_product_meta_lookup ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE GREATEST(min_price, 1) >= 9.5 AND IF(onsale, min_price, max_price) < 20 ORDER BY product_id",
		"SELECT MIN(min_price), MAX(max_price), MAX(GREATEST(min_price, max_price)) FROM wp_wc_product_meta_lookup",
		// 日付の列の GREATEST/LEAST（段階 113 で回した違い）
		"CREATE TABLE gd (id INT PRIMARY KEY, d DATE, dt DATETIME, s VARCHAR(20))",
		"INSERT INTO gd VALUES (1, '2024-01-05', '2024-01-03 10:00:00', '2024-01-04'), (2, '2023-12-31', '2024-02-01 00:00:00', 'abc')",
		"SELECT id, GREATEST(d, dt), LEAST(d, dt), GREATEST(d, '2024-01-04'), LEAST(dt, '2024-01-04'), GREATEST(d, s), GREATEST(d, d), LEAST(d, 'abc'), GREATEST(d, '2024-01-04 12:00:00'), LEAST(d, dt, '2024-01-04'), GREATEST(d, NULL) FROM gd ORDER BY id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE gd",
		"DROP TABLE wp_wc_product_meta_lookup",
		"DROP TABLE wp_wc_order_stats",
		"DROP TABLE dd",
		"DROP TABLE b",
		"DROP TABLE w",
		"DROP TABLE t0",
	}},
	{3068, []string{ // 文字列: ENUM と SET の列を関数で値の文字列として扱う（#3068、HEX）、INSTR の照合順序（#3980）、式の中の不正な UTF-8（#3078）
		"CREATE TABLE e (id INT PRIMARY KEY, e ENUM('abc','defg','hjikl') NOT NULL, s SET('a','b','c'))",
		"INSERT INTO e VALUES (1, 'abc', 'a,b'), (2, 'defg', 'c'), (3, 'hjikl', '')",
		"SELECT id, e LIKE 'a%', CONCAT(e, 'test'), LENGTH(e), CHAR_LENGTH(e), HEX(e), UPPER(e), SUBSTRING(e, 2), CONCAT_WS('-', e, s), REPLACE(e, 'b', 'B'), LOCATE('g', e), INSTR(e, 'g'), LEFT(e, 2), RIGHT(s, 1), HEX(s), LOWER(s) FROM e ORDER BY id",
		"SELECT GROUP_CONCAT(e ORDER BY id), GROUP_CONCAT(DISTINCT e ORDER BY e), GROUP_CONCAT(s ORDER BY id SEPARATOR '|'), GROUP_CONCAT(e, '-', id ORDER BY id) FROM e",
		"SELECT id FROM e WHERE e LIKE '%g%' ORDER BY id",
		"SELECT id FROM e WHERE CONCAT(e, s) LIKE 'abc%' ORDER BY id",
		"SELECT id, e + 0, e = 'defg', e = 2, s + 0 FROM e ORDER BY id",
		"SELECT SUBSTRING_INDEX(e, 'g', 1) FROM e ORDER BY id",
		"CREATE TABLE ci (id INT PRIMARY KEY, s VARCHAR(20)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO ci VALUES (1, 'xyza'), (2, 'XYZ'), (3, 'café'), (4, NULL), (5, '日本語テキスト')",
		"SELECT id, INSTR(s, 'A'), INSTR(s, 'z'), INSTR(s, 'É'), INSTR(s, ''), LOCATE('A', s), INSTR(s, 'e'), INSTR(s, 'テキ') FROM ci ORDER BY id",
		"SELECT INSTR('abc', NULL), INSTR(NULL, 'a'), INSTR('xyza' COLLATE utf8mb4_bin, 'A'), INSTR('xyza', 'A'), INSTR(_utf8mb4'ÀBC' COLLATE utf8mb4_unicode_520_ci, 'a')",
		"SELECT INSTR('xyza' COLLATE utf8mb4_0900_ai_ci, 'A'), INSTR('XYZA' COLLATE utf8mb4_0900_ai_ci, 'a'), INSTR('Hello World' COLLATE utf8mb4_0900_ai_ci, 'world'), INSTR('abc' COLLATE utf8mb4_0900_ai_ci, 'Z')",
		"SELECT INSTR('xyza' COLLATE utf8mb4_0900_as_cs, 'A'), INSTR('xyza' COLLATE utf8mb4_bin, 'A'), INSTR('a' COLLATE utf8mb4_0900_ai_ci, 'A' COLLATE utf8mb4_bin), INSTR('a' COLLATE utf8mb4_bin, 'A' COLLATE utf8mb4_0900_ai_ci)",
		"SELECT INSTR('你好a', 'a'), INSTR('áb', 'b'), INSTR('éÉ' COLLATE utf8mb4_0900_ai_ci, 'É'), INSTR('éÉ' COLLATE utf8mb4_bin, 'É'), INSTR('é', 'e'), INSTR('', ''), INSTR('', 'a'), INSTR(12345, 34), INSTR('a1b', 1), INSTR('ababa', 'ba')",
		"SELECT HEX(CONCAT('ab', UNHEX('FF'), 'cd')), LENGTH(CONCAT('ab', UNHEX('FF'), 'cd')), CONCAT('ab', UNHEX('FF'), 'cd') = 'ab', HEX(CONCAT('x', UNHEX('85'))), HEX(CONCAT_WS(',', 'a', UNHEX('FE'))), CONCAT('a', UNHEX('62'), 'c')",
		"CREATE TABLE u (id INT PRIMARY KEY, s VARCHAR(20), b VARBINARY(20)) DEFAULT CHARSET=utf8mb4",
		"INSERT INTO u VALUES (1, CONCAT('ab', UNHEX('FF'), 'cd'), NULL)",
		"SHOW WARNINGS",
		"INSERT INTO u VALUES (2, NULL, CONCAT('ab', UNHEX('FF'), 'cd'))",
		"INSERT INTO u VALUES (6, UNHEX('E38182FF'), NULL)",
		"SHOW WARNINGS",
		wpSQLMode,
		"INSERT INTO u VALUES (3, CONCAT('ab', UNHEX('FF'), 'cd'), NULL)",
		"SHOW WARNINGS",
		"INSERT INTO u VALUES (4, CONCAT('Product ', UNHEX('85'), ' NVMe'), NULL), (5, CONCAT_WS('-', 'a', UNHEX('FE'), 'b'), NULL)",
		"SHOW WARNINGS",
		"SELECT id, s, HEX(s), HEX(b) FROM u ORDER BY id",
		// WordPress の形: 投稿の状態を ENUM で持つプラグインの表（GROUP_CONCAT と LIKE と CONCAT）と、wp_posts の INSTR
		"CREATE TABLE wp_wpforms_entries (entry_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, form_id BIGINT UNSIGNED NOT NULL, status ENUM('','spam','partial','abandoned') NOT NULL DEFAULT '', type ENUM('','payment') NOT NULL DEFAULT '', KEY form_id (form_id)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_wpforms_entries (form_id, status, type) VALUES (1, '', ''), (1, 'spam', 'payment'), (2, 'partial', ''), (2, 'abandoned', 'payment')",
		"SELECT form_id, GROUP_CONCAT(status ORDER BY entry_id), GROUP_CONCAT(DISTINCT type ORDER BY type SEPARATOR '/') FROM wp_wpforms_entries GROUP BY form_id ORDER BY form_id",
		"SELECT entry_id, CONCAT(status, ':', type), LENGTH(status) FROM wp_wpforms_entries WHERE status LIKE 'pa%' OR type LIKE 'pay%' ORDER BY entry_id",
		stage114WPPosts,
		"INSERT INTO wp_posts (post_date, post_date_gmt, post_title) VALUES ('2024-01-01 00:00:00', '2024-01-01 00:00:00', 'Hello World'), ('2024-01-02 00:00:00', '2024-01-02 00:00:00', 'WordPress の HELLO'), ('2024-01-03 00:00:00', '2024-01-03 00:00:00', 'Café')",
		"SELECT ID, INSTR(post_title, 'hello'), INSTR(post_title, 'é'), LOCATE('hello', post_title) FROM wp_posts ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE INSTR(post_title, 'WORLD') > 0 ORDER BY ID",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_posts",
		"DROP TABLE wp_wpforms_entries",
		"DROP TABLE u",
		"DROP TABLE ci",
		"DROP TABLE e",
	}},
	{2985, []string{ // SHOW と変数: NOT NULL の ENUM の既定値（#2985）、コメントのエスケープと DEFAULT NULL（#2997）、SHOW VARIABLES の ON/OFF（#3174）、innodb_lock_wait_timeout（#3320）、EXTRA の on update（#3862）
		"CREATE TABLE en (id INT PRIMARY KEY, e ENUM('abc','def') NOT NULL, n ENUM('x','y'), s SET('a','b') NOT NULL DEFAULT 'a')",
		"INSERT INTO en (id) VALUES (1)",
		"INSERT INTO en (id, s) VALUES (2, 'b')",
		"CREATE TABLE en2 (id INT PRIMARY KEY, s SET('a','b') NOT NULL)",
		"INSERT INTO en2 (id) VALUES (1)",
		wpSQLMode,
		"INSERT INTO en (id) VALUES (3)",
		"SHOW WARNINGS",
		"INSERT INTO en (id, e) VALUES (4, NULL), (5, 'def')",
		"SHOW WARNINGS",
		"INSERT INTO en2 (id) VALUES (2)",
		"SHOW WARNINGS",
		"INSERT IGNORE INTO en (id) VALUES (6)",
		"SELECT * FROM en ORDER BY id",
		"SELECT * FROM en2 ORDER BY id",
		"SET SESSION sql_mode = DEFAULT",
		"CREATE TABLE cm (id INT PRIMARY KEY COMMENT 'it''s', a VARCHAR(10) COMMENT 'a\\\\b\\nc', b INT NULL, c TEXT, d INT NOT NULL, q VARCHAR(5) COMMENT 'say \"hi\"', j JSON, bl BLOB, dt DATETIME, e ENUM('x','y'), u TIMESTAMP NULL DEFAULT NULL, dd DECIMAL(10,2) COMMENT 'price', g DATETIME ON UPDATE CURRENT_TIMESTAMP COMMENT 'upd') COMMENT='x\\\\y'",
		"SHOW CREATE TABLE cm",
		"SHOW VARIABLES LIKE 'autocommit'",
		"SHOW VARIABLES LIKE 'foreign_key_checks'",
		"SHOW VARIABLES WHERE Variable_name IN ('sql_auto_is_null', 'unique_checks', 'big_tables', 'sql_safe_updates')",
		"SET SESSION autocommit = 0",
		"SHOW SESSION VARIABLES LIKE 'autocommit'",
		"SELECT @@autocommit, @@foreign_key_checks",
		"SET SESSION autocommit = 1",
		"SET SESSION innodb_lock_wait_timeout = 50",
		"SELECT @@innodb_lock_wait_timeout, @@session.innodb_lock_wait_timeout",
		"SET innodb_lock_wait_timeout = 20",
		"SELECT @@innodb_lock_wait_timeout",
		"SET @@session.innodb_lock_wait_timeout = 120",
		"SELECT @@session.innodb_lock_wait_timeout",
		"SET SESSION innodb_lock_wait_timeout = DEFAULT",
		"SELECT @@session.innodb_lock_wait_timeout",
		"CREATE TABLE ou (id INT PRIMARY KEY, u TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, d DATETIME ON UPDATE CURRENT_TIMESTAMP, g DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3), a INT AUTO_INCREMENT UNIQUE)",
		"SELECT column_name, extra FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'ou' ORDER BY ordinal_position",
		"SHOW COLUMNS FROM ou",
		"SHOW CREATE TABLE ou",
		"DROP TABLE ou",
		"DROP TABLE cm",
		"DROP TABLE en2",
		"DROP TABLE en",
	}},
	{3277, []string{ // 段階 114 の実装後のレビューで直した形: 索引のある日時の列と切り詰めた文字列、lookup join、DATE_FORMAT と TIMESTAMPDIFF の数、記号と符号の区切り、ABS、STR_TO_DATE の %p と短い文字列、IF の ENUM
		"CREATE TABLE t114 (id INT PRIMARY KEY, d DATETIME, KEY d (d))",
		"INSERT INTO t114 VALUES (1, '2024-01-01 09:00:00'), (2, '2024-01-01 00:00:00'), (3, '2024-06-01 00:00:00'), (4, NULL)",
		"SELECT id FROM t114 WHERE d = '2024-01-01 09:00:00 x' ORDER BY id",
		"SELECT id FROM t114 WHERE d >= '2024-01-01 a' ORDER BY id",
		"SELECT id FROM t114 WHERE d > '2024-01-01 a' ORDER BY id",
		"SELECT id FROM t114 WHERE d < '2024-06-01 a' ORDER BY id",
		"SELECT id FROM t114 WHERE d <= '2024-06-01 a' ORDER BY id",
		"SELECT id FROM t114 WHERE d <> '2024-01-01 a' ORDER BY id",
		"SELECT id FROM t114 WHERE d IN ('2024-01-01 a', '2024-06-01 b') ORDER BY id",
		"SELECT id FROM t114 WHERE d BETWEEN '2024-01-01 a' AND '2024-12-31 b' ORDER BY id",
		"SELECT id FROM t114 WHERE d = '2024-01-01 a' OR d > '2024-05-01 x' ORDER BY id",
		"CREATE TABLE tj (id INT PRIMARY KEY, k VARCHAR(30))",
		"INSERT INTO tj VALUES (1, '2024-01-01 09:00:00 x'), (2, 'garbage'), (3, '2024-06-01'), (4, '2024/1/1 9:0:0')",
		"SELECT t.id, j.id FROM t114 t JOIN tj j ON t.d = j.k ORDER BY t.id, j.id",
		"SELECT j.id, t.id FROM tj j LEFT JOIN t114 t ON t.d = j.k ORDER BY j.id",
		"SELECT j.id, t.id FROM tj j JOIN t114 t ON t.d = j.k ORDER BY j.id",
		"SELECT j.id FROM tj j WHERE EXISTS (SELECT 1 FROM t114 t WHERE t.d = j.k) ORDER BY j.id",
		// 索引のない列の結合と比べ（日時として読めない 'garbage' はゼロの日時。前は 1105）
		"CREATE TABLE tx (id INT PRIMARY KEY, d DATETIME)",
		"INSERT INTO tx VALUES (1, '2024-01-01 09:00:00'), (2, '2024-06-01'), (3, NULL)",
		"SELECT j.id, x.id FROM tj j LEFT JOIN tx x ON x.d = j.k ORDER BY j.id, x.id",
		"SELECT j.id, x.id, x.d = j.k, x.d < j.k, x.d > j.k, x.d <=> j.k, j.k = x.d FROM tj j, tx x ORDER BY j.id, x.id",
		"SELECT x.id FROM tx x WHERE x.d IN (SELECT k FROM tj) ORDER BY x.id",
		"DROP TABLE tx",
		// WordPress の形: wp_comments の comment_date_gmt（1 列の日時の索引）
		wpSQLMode,
		"CREATE TABLE wp_comments (comment_ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, comment_post_ID BIGINT UNSIGNED NOT NULL DEFAULT 0, comment_date_gmt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', comment_approved VARCHAR(20) NOT NULL DEFAULT '1', KEY comment_date_gmt (comment_date_gmt), KEY comment_post_ID (comment_post_ID)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_comments (comment_post_ID, comment_date_gmt) VALUES (1, '2024-01-01 09:00:00'), (1, '2024-02-01 00:00:00'), (2, '2024-03-01 12:00:00'), (2, '0000-00-00 00:00:00')",
		"SELECT comment_ID FROM wp_comments WHERE comment_date_gmt >= '2024-02-01 00:00:00 +0000' ORDER BY comment_date_gmt DESC",
		"SELECT comment_ID FROM wp_comments WHERE comment_date_gmt < '2024-02-01T00:00:00Z' AND comment_date_gmt > '0000-00-00 00:00:00' ORDER BY comment_ID",
		"SELECT comment_ID FROM wp_comments WHERE comment_date_gmt BETWEEN ' 2024-01-01 ' AND '2024-12-31 23:59:59 x' ORDER BY comment_ID",
		"SELECT COUNT(*) FROM wp_comments WHERE comment_date_gmt = '2024-03-01 12:00:00 UTC'",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_comments",
		"SELECT DATE_FORMAT(20240105, '%Y-%m-%d'), DATE_FORMAT(20240105103000, '%H:%i'), DATE_FORMAT(240105, '%Y'), DATE_FORMAT(1, '%Y'), TIMESTAMPDIFF(MONTH, 20240101, 20240301), TIMESTAMPDIFF(DAY, 20240101, '2024-01-31')",
		"SELECT CAST('2024-01-05+09:00' AS DATETIME), CAST('2024-01-05 -1' AS DATETIME), CAST('2024-01-05 10:00:00+09:00' AS DATETIME), CAST('2024-01-05 10:00:00+0900' AS DATETIME), CAST('2024-01-05 10:00+09:00' AS DATETIME), CAST('2024-01-05 10:00:00 +09:00' AS DATETIME), CAST('2024-01-05-10' AS DATETIME), CAST('2024-01-05+09' AS DATE)",
		"CREATE TABLE ab (id INT PRIMARY KEY, dt DATETIME, d DATE, d3 DATETIME(3))",
		"INSERT INTO ab VALUES (1, '2024-01-01 09:00:00', '2024-01-01', '2024-01-01 09:00:00.125')",
		"SELECT ABS(dt), ABS(d), ABS(d3), ABS('1e400'), ABS('-1e400'), '1e400' + 0 FROM ab",
		"CREATE TABLE fz (d DOUBLE, i INT, f FLOAT)",
		"INSERT INTO fz (d) VALUES ('1e400')",
		"INSERT INTO fz (i) VALUES ('1e400')",
		"INSERT INTO fz (d) VALUES ('-1e400')",
		"SELECT STR_TO_DATE('2024-01-02 PM 07', '%Y-%m-%d %p %h'), STR_TO_DATE('PM', '%p'), STR_TO_DATE('07 PM', '%h %p'), STR_TO_DATE('07:30 PM', '%H:%i %p')",
		"SELECT STR_TO_DATE('2024-01-02', '%Y-%m-%d %H:%i:%s'), STR_TO_DATE('', '%H:%i'), STR_TO_DATE('2024-01-02 10', '%Y-%m-%d %H:%i:%s'), STR_TO_DATE('2024', '%Y-%m-%d'), STR_TO_DATE('10', '%H:%i:%s'), STR_TO_DATE('2024-01-', '%Y-%m-%d')",
		"CREATE TABLE ie (id INT PRIMARY KEY, e ENUM('abc','def'), s SET('a','b'))",
		"INSERT INTO ie VALUES (1, 'abc', 'a,b'), (2, 'def', ''), (3, NULL, NULL)",
		"SELECT id, IF(id = 1, e, 'none'), IF(id > 1, 'none', s), IF(id = 2, e, NULL), CONCAT(IF(id < 3, e, 'x'), '!') FROM ie ORDER BY id",
		"CREATE TABLE gm (id INT PRIMARY KEY, g GEOMETRY, p POINT NOT NULL)",
		"SHOW CREATE TABLE gm",
		wpSQLMode,
		"INSERT INTO fz (d, i, f) VALUES ('1e400', '1e400', '1e40')",
		"SHOW WARNINGS",
		"SELECT * FROM fz",
		"SELECT STR_TO_DATE('2024-01-02', '%Y-%m-%d %H:%i:%s'), STR_TO_DATE('', '%H:%i'), STR_TO_DATE('2024', '%Y-%m-%d'), STR_TO_DATE('2024-01-', '%Y-%m-%d')",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE gm",
		"DROP TABLE ie",
		"DROP TABLE fz",
		"DROP TABLE ab",
		"DROP TABLE tj",
		"DROP TABLE t114",
	}},
	{3278, []string{ // 2 回目のレビューで直した形: 指定子のない書式の空の文字列、結合の道ごとの読めない文字列とゼロの日時、= の NULL の lookup、文字列の単項マイナス
		"SELECT STR_TO_DATE('', ''), STR_TO_DATE('', 'x'), STR_TO_DATE('  ', ' '), STR_TO_DATE('', '%H:%i')",
		"SELECT -'1e400', -'-1e400', ABS(-'1e400'), -'12abc', -' 3 ', -'abc', -'1.5'",
		wpSQLMode,
		"SELECT STR_TO_DATE('', ''), STR_TO_DATE('', 'x'), STR_TO_DATE('  ', ' ')",
		"CREATE TABLE t (id INT PRIMARY KEY, d DATETIME, n INT, s VARCHAR(30), KEY d (d), KEY n (n), KEY s (s))",
		"INSERT INTO t VALUES (1, '2024-01-01 09:00:00', 1, 'a'), (4, NULL, NULL, NULL), (5, '0000-00-00 00:00:00', 5, 'e')",
		"CREATE TABLE j (id INT PRIMARY KEY, k VARCHAR(30), m INT, v VARCHAR(30))",
		"INSERT INTO j VALUES (1, '2024-01-01 09:00:00 x', 1, 'a'), (2, 'garbage', NULL, NULL), (3, NULL, NULL, NULL), (5, '', 5, 'e')",
		"SELECT t.id, j.id FROM t LEFT JOIN j ON t.d = j.k ORDER BY t.id, j.id",
		"SELECT t.id, j.id FROM t LEFT JOIN j ON t.d = j.k AND j.id > 0 ORDER BY t.id, j.id",
		"SELECT /*+ HASH_JOIN(t, j) */ t.id, j.id FROM t JOIN j ON t.d = j.k ORDER BY t.id, j.id",
		"SELECT /*+ HASH_JOIN(t, j) */ t.id, j.id FROM t LEFT JOIN j ON t.d = j.k ORDER BY t.id, j.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.d = j.k ORDER BY t.id, j.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.d = j.k WHERE j.k IS NULL ORDER BY t.id",
		"SELECT t.id, j.id FROM t JOIN j ON t.d = j.k WHERE j.k IS NULL ORDER BY t.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.d <=> j.k WHERE j.k IS NULL ORDER BY t.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.n = j.m ORDER BY t.id, j.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.s = j.v ORDER BY t.id, j.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.n <=> j.m ORDER BY t.id, j.id",
		"SELECT /*+ LOOKUP_JOIN(j, t) */ j.id, t.id FROM j LEFT JOIN t ON t.s = j.v ORDER BY j.id",
		// WordPress の形: NULL の meta_value で投稿を引く LEFT JOIN（wp_postmeta の meta_value を数として wp_posts の ID に結ぶ）
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_parent BIGINT UNSIGNED NULL, post_title TEXT NOT NULL, KEY post_parent (post_parent))",
		"INSERT INTO wp_posts (post_parent, post_title) VALUES (NULL, 'a'), (1, 'b'), (NULL, 'c')",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id))",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '2'), (3, '_thumbnail_id', NULL), (2, '_parent', NULL)",
		"SELECT /*+ LOOKUP_JOIN(m, p) */ m.meta_id, p.ID FROM wp_postmeta m LEFT JOIN wp_posts p ON p.post_parent = m.meta_value ORDER BY m.meta_id, p.ID",
		"SELECT /*+ LOOKUP_JOIN(m, p) */ m.meta_id, p.ID FROM wp_postmeta m JOIN wp_posts p ON p.post_parent = m.meta_value ORDER BY m.meta_id, p.ID",
		"SELECT /*+ LOOKUP_JOIN(m, p) */ m.meta_id, p.ID FROM wp_postmeta m LEFT JOIN wp_posts p ON p.ID = m.meta_value ORDER BY m.meta_id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE j",
		"DROP TABLE t",
	}},
	{3476, []string{ // DATETIME(n) と TIMESTAMP(n) の小数の桁の表示（0 の桁とゼロの日時も）
		wpSQLMode,
		"CREATE TABLE test (d0 DATETIME(0), d1 DATETIME(1), d3 DATETIME(3), d6 DATETIME(6), t3 TIMESTAMP(3) NULL, t6 TIMESTAMP(6) NULL)",
		"INSERT INTO test VALUES ('0000-00-00 00:00:00','0000-00-00 00:00:00','0000-00-00 00:00:00','0000-00-00 00:00:00', '0000-00-00 00:00:00', '0000-00-00 00:00:00')",
		"INSERT INTO test VALUES ('1994-05-30 12:34:56.789012', '1994-05-30 12:34:56.100000', '1994-05-30 12:34:56.100000', '1994-05-30 12:34:56.000000', '1994-05-30 12:34:56', '1994-05-30 12:34:56.5')",
		"INSERT INTO test VALUES ('2024-01-01 00:00:00', '2024-01-01 00:00:00.05', '2024-01-01 00:00:00.9999', '2024-01-01 00:00:00.0000005', NULL, NULL)",
		"SELECT * FROM test ORDER BY d0",
		"SELECT d3, d6 FROM test WHERE d3 > '2000-01-01' ORDER BY d3",
		"SELECT MAX(d3), MIN(d6), COUNT(*) FROM test",
		"SELECT NOW(3) IS NOT NULL, CAST('2020-01-01 00:00:00' AS DATETIME(3)), CAST('2020-01-01 12:34:56.9999995' AS DATETIME(6)), CAST('2020-01-01 12:34:56.5' AS DATETIME(2))",
		"SELECT CAST('2020-01-01 12:00:00' AS DATETIME), CAST('2020-01-01' AS DATE), DATE_ADD('2020-01-01 12:00:00', INTERVAL 1 DAY), TIMESTAMP('2020-01-01')",
		// WordPress の形: 秒の端数のない DATETIME の列と、その式は前と同じく端数を付けない
		stage114WPPosts,
		"INSERT INTO wp_posts (post_date, post_date_gmt, post_title) VALUES ('2024-01-01 09:00:00', '2024-01-01 00:00:00', 'a')",
		"SELECT post_date, post_date_gmt, DATE_ADD(post_date, INTERVAL 1 DAY), GREATEST(post_date, post_date_gmt), COALESCE(post_date, post_date_gmt), MAX(post_date) FROM wp_posts GROUP BY ID",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_posts",
		"DROP TABLE test",
	}},
}

func TestStage114UpstreamFixes(t *testing.T) {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage114Groups {
		stmts = append(stmts, g.stmts...)
	}
	checkGolden(t, "../testdata/stage114_golden.txt", "heimdb_golden114", stmts)
}

// SHOW TABLE STATUS の最後の列は、MySQL と同じく Comment で、テーブルのコメントを出す（上流の #3175。前は列の名前が
// Comments で、WHERE Comment = ... が 1105、値はいつも NULL）。ほかの列（行の数、作った時刻など）は MySQL と違うので、
// ゴールデンファイルではなくここで見る。
func TestStage114ShowTableStatusComment(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE ts (id INT PRIMARY KEY) COMMENT 'hello'")
	mustExec(t, db, "CREATE TABLE tn (id INT PRIMARY KEY)")
	for _, c := range []struct{ q, name, comment string }{
		{"SHOW TABLE STATUS WHERE Comment = 'hello'", "ts", "hello"},
		{"SHOW TABLE STATUS LIKE 'ts'", "ts", "hello"},
		{"SHOW TABLE STATUS LIKE 'tn'", "tn", ""},
	} {
		rows, err := db.Query(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		cols, _ := rows.Columns()
		if cols[len(cols)-1] != "Comment" {
			t.Errorf("%s: last column %q, want Comment", c.q, cols[len(cols)-1])
		}
		n := 0
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			n++
			if vals[0].String != c.name || vals[len(vals)-1].String != c.comment || !vals[len(vals)-1].Valid {
				t.Errorf("%s: got %q comment %v, want %q %q", c.q, vals[0].String, vals[len(vals)-1], c.name, c.comment)
			}
		}
		rows.Close()
		if n != 1 {
			t.Errorf("%s: %d rows, want 1", c.q, n)
		}
	}
}

// 2 回目のレビューの結合の形が、ゴールデンファイルと同じ表でそれぞれの道（ハッシュ結合、lookup、入れ子の繰り返し）を
// 通ることを確かめる（道が変わると、ゴールデンファイルの文がその道を確かめなくなる）。
func TestStage114JoinPathPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, g := range stage114Groups {
		if g.pr != 3278 {
			continue
		}
		for _, q := range g.stmts {
			if strings.HasPrefix(q, "SELECT") {
				break
			}
			mustExec(t, db, q)
		}
		for _, q := range g.stmts {
			if strings.HasPrefix(q, "CREATE") || strings.HasPrefix(q, "INSERT") || strings.HasPrefix(q, "SET") {
				mustExec(t, db, q)
			}
		}
	}
	for _, c := range []struct{ q, want string }{
		{"SELECT /*+ HASH_JOIN(t, j) */ t.id, j.id FROM t JOIN j ON t.d = j.k ORDER BY t.id, j.id", "HashJoin"},
		{"SELECT /*+ HASH_JOIN(t, j) */ t.id, j.id FROM t LEFT JOIN j ON t.d = j.k ORDER BY t.id, j.id", "LeftOuterHashJoin"},
		{"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.d = j.k ORDER BY t.id, j.id", "LookupJoin"},
		{"SELECT t.id, j.id FROM t LEFT JOIN j ON t.d = j.k AND j.id > 0 ORDER BY t.id, j.id", "LeftOuterJoin"},
		{"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.n = j.m ORDER BY t.id, j.id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.s = j.v ORDER BY t.id, j.id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(j, t) */ t.id, j.id FROM j JOIN t ON t.n <=> j.m ORDER BY t.id, j.id", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(j, t) */ j.id, t.id FROM j LEFT JOIN t ON t.s = j.v ORDER BY j.id", "LeftOuterLookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(m, p) */ m.meta_id, p.ID FROM wp_postmeta m LEFT JOIN wp_posts p ON p.post_parent = m.meta_value ORDER BY m.meta_id, p.ID", "LeftOuterLookupJoin"},
	} {
		if plan := explain(t, db, c.q); !strings.Contains(plan, c.want) {
			t.Errorf("%s\nplan has no %s:\n%s", c.q, c.want, plan)
		}
	}
}
