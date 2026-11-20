package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// stage113Groups は、段階 113 で取り込んだ上流の go-mysql-server の修正の再現の文（docs/engine-upstream.md）。
// 直す前の HeimDB は本物の MySQL 8.0 と違う結果かエラーを返した。各組の終わりに作った表を消す。
var stage113Groups = []struct {
	pr    int
	stmts []string
}{
	{3030, []string{ // GROUP BY の集計しない列（FIRST）が、グループの最初の行の NULL の後の行の値で上書きされた
		"CREATE TABLE t0 (c0 INT, c1 INT)",
		"INSERT INTO t0 VALUES (NULL,1),(1,NULL)",
		"SELECT t0.c0 = t0.c1 AS ref0, SUM(1) AS ref1 FROM t0 GROUP BY ref0",
		"CREATE TABLE t1 (id INT PRIMARY KEY, c0 INT, c1 INT)",
		"INSERT INTO t1 VALUES (1,NULL,NULL),(2,1,1),(3,1,NULL),(4,2,1),(5,NULL,1)",
		"SELECT t1.c0 = t1.c1 AS ref0, SUM(1) AS ref1 FROM t1 GROUP BY ref0 ORDER BY ref0",
		wpSQLMode,
		"CREATE TABLE g (a INT, b INT, k INT)",
		"INSERT INTO g VALUES (NULL,1,1),(5,2,1)",
		"SELECT k, a FROM g GROUP BY k",
		"SELECT k, a, b FROM g GROUP BY k",
		// WordPress の形: ONLY_FULL_GROUP_BY を外した sql_mode で、記事ごとにまとめたメタの集計しない列（最初の行が NULL）
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_price', NULL), (1, '_price', '10'), (2, '_price', '20'), (2, '_price', NULL)",
		"SELECT post_id, meta_value, COUNT(*) FROM pm WHERE meta_key = '_price' GROUP BY post_id ORDER BY post_id",
		"SELECT post_id, meta_value FROM pm GROUP BY post_id ORDER BY post_id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE pm",
		"DROP TABLE g",
		"DROP TABLE t1",
		"DROP TABLE t0",
	}},
	{3556, []string{ // U+FFFD（置き換えの文字）を含む値を壊れた UTF-8 とみなし、CHAR_LENGTH が 1105、LIKE が一致しなかった
		"CREATE TABLE t (id INT PRIMARY KEY, s VARCHAR(100)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO t VALUES (1, CONCAT('ab', UNHEX('EFBFBD'), 'cd')), (2, 'abcd'), (3, UNHEX('EFBFBD'))",
		"SELECT id, CHAR_LENGTH(s), LENGTH(s) FROM t ORDER BY id",
		"SELECT id FROM t WHERE s LIKE '%ab%' ORDER BY id",
		"SELECT id FROM t WHERE s LIKE 'ab_cd' ORDER BY id",
		"SELECT id FROM t WHERE s LIKE CONCAT('%', UNHEX('EFBFBD'), '%') ORDER BY id",
		"SELECT id FROM t WHERE s LIKE '_' ORDER BY id",
		"SELECT id FROM t WHERE s NOT LIKE '%cd' ORDER BY id",
		// WordPress の形: 文字化けした本文とタイトルの検索（WP_Query の s）と、esc_like のパターン
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title TEXT NOT NULL, post_content LONGTEXT NOT NULL, post_excerpt TEXT NOT NULL, post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_status VARCHAR(20) NOT NULL DEFAULT 'publish') DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_posts (post_title, post_content, post_excerpt) VALUES (CONCAT('caf', UNHEX('EFBFBD'), ' menu'), CONCAT('今日の', UNHEX('EFBFBD'), 'ランチ'), ''), ('plain', 'ランチ 100%', ''), ('x_y', 'body', '')",
		"SELECT ID FROM wp_posts WHERE 1=1 AND (((wp_posts.post_title LIKE '%ランチ%') OR (wp_posts.post_excerpt LIKE '%ランチ%') OR (wp_posts.post_content LIKE '%ランチ%'))) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) ORDER BY wp_posts.ID",
		"SELECT ID FROM wp_posts WHERE post_title LIKE '%menu%' ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE post_content LIKE '%100\\%%' ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE post_title LIKE 'x\\_y' ORDER BY ID",
		"SELECT ID, CHAR_LENGTH(post_title), CHAR_LENGTH(post_content) FROM wp_posts ORDER BY ID",
		"DROP TABLE wp_posts",
		"DROP TABLE t",
	}},
	{3581, []string{ // LIKE 'prefix%' の範囲の上限を byte(255)（U+FFFD の重み）にして、'z' や CJK が続く値を落とした
		"CREATE TABLE z (id INT PRIMARY KEY, txt VARCHAR(100)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO z VALUES (1, '## z'), (2, '## y'), (3, '## za'), (4, '## x'), (5, '## 中 body'), (6, 'plain'), (7, '##nospaces')",
		"SELECT id FROM z WHERE txt LIKE '## %' ORDER BY id",
		"SELECT id FROM z WHERE txt NOT LIKE '## %' ORDER BY id",
		"SELECT id FROM z WHERE txt LIKE 'ab%' OR txt LIKE '## z%' ORDER BY id",
		"SELECT SUM(txt LIKE '## %') FROM z",
		"SELECT id FROM z WHERE id IN (SELECT id FROM z WHERE txt LIKE '## %') ORDER BY id",
		"SELECT d.id FROM (SELECT id FROM z WHERE txt LIKE '## %') d ORDER BY d.id",
		"CREATE TABLE u (ID INT PRIMARY KEY, display_name VARCHAR(250)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO u VALUES (1, '山田太郎'), (2, 'yamada'), (3, 'yamaz'), (4, 'Yamazaki'), (5, '山本')",
		"SELECT ID FROM u WHERE display_name LIKE '山田%' ORDER BY ID",
		"SELECT ID FROM u WHERE display_name LIKE '山%' ORDER BY ID",
		"SELECT ID FROM u WHERE display_name LIKE 'yama%' ORDER BY ID",
		// 索引のある列（WordPress の wp_users の user_login、display_name の検索。WP_User_Query の 'yama*' は LIKE 'yama%'）
		"CREATE TABLE wp_users (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, user_login VARCHAR(60) NOT NULL DEFAULT '', user_nicename VARCHAR(50) NOT NULL DEFAULT '', user_email VARCHAR(100) NOT NULL DEFAULT '', user_url VARCHAR(100) NOT NULL DEFAULT '', display_name VARCHAR(250) NOT NULL DEFAULT '', KEY user_login_key (user_login), KEY user_nicename (user_nicename), KEY user_email (user_email)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_users (user_login, user_nicename, user_email, display_name) VALUES ('yamada', 'yamada', 'yamada@example.com', '山田太郎'), ('yamaz', 'yamaz', 'z@example.com', 'Yamaz'), ('yamazaki', 'yamazaki', 'zaki@example.com', '山崎'), ('suzuki', 'suzuki', 's@example.com', '鈴木'), ('yama', 'yama', 'y@example.com', 'やま'), ('yamaＺa', 'yamaz-2', 'z2@example.com', 'Ｚ')",
		"SELECT ID FROM wp_users WHERE 1=1 AND (user_login LIKE 'yama%' OR user_url LIKE 'yama%' OR user_email LIKE 'yama%' OR user_nicename LIKE 'yama%' OR display_name LIKE 'yama%') ORDER BY user_login ASC",
		"SELECT ID FROM wp_users WHERE 1=1 AND (user_login LIKE 'yama%' OR user_email LIKE 'yama%' OR user_nicename LIKE 'yama%' OR display_name LIKE 'yama%') ORDER BY user_login ASC",
		"SELECT ID, user_login FROM wp_users WHERE user_login LIKE 'yama%' ORDER BY user_login ASC, ID",
		"SELECT ID FROM wp_users WHERE user_login NOT LIKE 'yama%' ORDER BY ID",
		"SELECT ID FROM wp_users WHERE display_name LIKE '山%' ORDER BY ID",
		"SELECT COUNT(*) FROM wp_users WHERE user_email LIKE 'z%'",
		// wp_options の option_name（一意の索引）の前方一致。esc_like のパターン（ESCAPE を付けて置き換えない）も同じ
		"CREATE TABLE wp_options (option_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, option_name VARCHAR(191) NOT NULL DEFAULT '', option_value LONGTEXT NOT NULL, autoload VARCHAR(20) NOT NULL DEFAULT 'yes', UNIQUE KEY option_name (option_name)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('widget_zoo', 'a'), ('widget_block', 'b'), ('widget_日本', 'c'), ('widgets', 'd'), ('widgez', 'e'), ('_transient_zz', 'f'), ('_transient_aa', 'g')",
		"SELECT option_name FROM wp_options WHERE option_name LIKE 'widget%' ORDER BY option_name",
		"SELECT option_name FROM wp_options WHERE option_name LIKE 'widget\\_%' ORDER BY option_name",
		"SELECT option_name FROM wp_options WHERE option_name LIKE '\\_transient\\_%' ORDER BY option_name",
		"SELECT COUNT(*) FROM wp_options WHERE option_name LIKE 'widget_日%'",
		"DELETE FROM wp_options WHERE option_name LIKE 'widget%'",
		"SELECT option_name FROM wp_options ORDER BY option_name",
		// ほかの照合順序（_bin は最後の文字を進めた上限、ほかは下限と LIKE だけ）
		"CREATE TABLE b (id INT PRIMARY KEY, txt VARCHAR(50) COLLATE utf8mb4_bin, KEY txt (txt))",
		"INSERT INTO b VALUES (1, 'apple'), (2, 'apricot'), (3, 'banana'), (4, 'apZ'), (5, 'aq'), (6, 'Apple'), (7, 'ap中')",
		"SELECT id FROM b WHERE txt LIKE 'ap%' ORDER BY id",
		"SELECT id FROM b WHERE txt NOT LIKE 'ap%' ORDER BY id",
		"CREATE TABLE g (id INT PRIMARY KEY, txt VARCHAR(50) COLLATE utf8mb4_general_ci, KEY txt (txt))",
		"INSERT INTO g VALUES (1, 'apple'), (2, 'APRICOT'), (3, 'banana'), (4, 'ap中'), (5, 'apz')",
		"SELECT id FROM g WHERE txt LIKE 'ap%' ORDER BY id",
		"CREATE TABLE a9 (id INT PRIMARY KEY, txt VARCHAR(50) COLLATE utf8mb4_0900_ai_ci, KEY txt (txt))",
		"INSERT INTO a9 VALUES (1, '## z'), (2, '## y'), (3, '## 中'), (4, '#'), (5, '## é')",
		"SELECT id FROM a9 WHERE txt LIKE '## %' ORDER BY id",
		// 列より長いパターン（範囲の上限が列の長さを超える）
		"CREATE TABLE s5 (id INT PRIMARY KEY, txt VARCHAR(5), KEY txt (txt)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO s5 VALUES (1, 'abcde'), (2, 'abcdz'), (3, 'abcd')",
		"SELECT id FROM s5 WHERE txt LIKE 'abcde%' ORDER BY id",
		"SELECT id FROM s5 WHERE txt LIKE 'abcd%' ORDER BY id",
		"DROP TABLE s5",
		"DROP TABLE a9",
		"DROP TABLE g",
		"DROP TABLE b",
		"DROP TABLE wp_options",
		"DROP TABLE wp_users",
		"DROP TABLE u",
		"DROP TABLE z",
	}},
	{4021, []string{ // 主キーの MAX/MIN を ORDER BY ... LIMIT 1 に置き換えるとき GroupBy を外し、合う行がないと 0 行を返した（MySQL は NULL の 1 行）
		"CREATE TABLE emptytable (i INT PRIMARY KEY, v INT)",
		"SELECT MAX(i) FROM emptytable",
		"SELECT MIN(i) FROM emptytable",
		"SELECT COALESCE(MAX(i), 0) + 1 FROM emptytable",
		"SELECT MAX(i) + 1, MAX(i) IS NULL FROM emptytable",
		"SELECT MAX(v) FROM emptytable",
		"SELECT MAX(i) AS m FROM emptytable",
		"INSERT INTO emptytable VALUES (1, 1), (2, 2)",
		"SELECT MAX(i) FROM emptytable WHERE i > 5",
		"SELECT MIN(i) FROM emptytable WHERE i > 5",
		"SELECT MAX(i) FROM emptytable",
		"SELECT MIN(i) FROM emptytable",
		"SELECT MAX(i) FROM emptytable WHERE i < 2",
		"SELECT MIN(i) FROM emptytable WHERE i > 1",
		"SELECT MAX(i) FROM emptytable GROUP BY v ORDER BY 1",
		"SELECT MAX(i) FROM emptytable WHERE i > 5 GROUP BY v",
		"CREATE TABLE three_pk (pk1 INT, pk2 INT, pk3 INT, PRIMARY KEY (pk1, pk2, pk3))",
		"INSERT INTO three_pk VALUES (1, 0, 10), (1, 0, 5)",
		"SELECT MAX(pk3) FROM three_pk WHERE pk1=1 AND pk2=0",
		"SELECT MAX(pk3) FROM three_pk WHERE pk1=2 AND pk2=0",
		"SELECT MIN(pk3) FROM three_pk WHERE pk1=2 AND pk2=0",
		"SELECT MAX(pk1) FROM three_pk WHERE pk1=2",
		"SELECT MAX(pk1) FROM three_pk",
		"SELECT MAX(pk3) FROM three_pk WHERE pk1=2 AND pk2=0 GROUP BY pk1, pk2",
		// 副問い合わせの中の MAX（スカラー副問い合わせは 0 行でも NULL だが、IN や派生表では行の数が効く）
		"SELECT (SELECT MAX(i) FROM emptytable WHERE i > 5) IS NULL",
		"SELECT COUNT(*) FROM (SELECT MAX(i) AS m FROM emptytable WHERE i > 5) d",
		"SELECT pk1 FROM three_pk WHERE pk3 > (SELECT COALESCE(MAX(i), 0) FROM emptytable WHERE i > 5) ORDER BY pk3",
		// WordPress の形: 空の表で次の番号を採る（プラグインの独自の表）、記事の最大の ID
		"CREATE TABLE wp_custom_log (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, msg TEXT NOT NULL)",
		"SELECT COALESCE(MAX(id), 0) + 1 FROM wp_custom_log",
		"SELECT IFNULL(MAX(id), 0) FROM wp_custom_log",
		"SELECT MAX(id) FROM wp_custom_log",
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"SELECT MAX(ID) FROM wp_posts",
		"INSERT INTO wp_posts (post_type) VALUES ('post'), ('page'), ('post')",
		"SELECT MAX(ID) FROM wp_posts",
		"SELECT MIN(ID) FROM wp_posts",
		"SELECT MAX(ID) FROM wp_posts WHERE ID > 100",
		"SELECT MAX(ID) FROM wp_posts WHERE post_type = 'attachment'",
		"DROP TABLE wp_posts",
		"DROP TABLE wp_custom_log",
		"DROP TABLE three_pk",
		"DROP TABLE emptytable",
	}},
	{3739, []string{ // HAVING の集計の列を名前だけで探し、COUNT(p2.id) を p1.id に結び付けて 1146（table not found: p2）
		"CREATE TABLE customers (id BIGINT PRIMARY KEY)",
		"CREATE TABLE orders (id BIGINT PRIMARY KEY, customer_id BIGINT, status VARCHAR(16), total BIGINT)",
		"INSERT INTO customers VALUES (1), (2)",
		"INSERT INTO orders VALUES (1, 1, 'paid', 10), (2, 1, 'paid', 20), (3, 2, 'paid', 5)",
		"SELECT c.id, COUNT(p1.total) AS total_count, COUNT(p2.id) AS peer_orders FROM customers c JOIN orders p1 ON p1.customer_id = c.id LEFT JOIN orders p2 ON p2.customer_id = c.id AND p2.id <> p1.id WHERE p1.status = 'paid' AND p2.status = 'paid' GROUP BY c.id HAVING COUNT(p2.id) > 0",
		"SELECT c.id, COUNT(p2.id) AS n FROM customers c JOIN orders p1 ON p1.customer_id = c.id LEFT JOIN orders p2 ON p2.customer_id = c.id AND p2.id <> p1.id GROUP BY c.id HAVING COUNT(p2.id) > 0",
		"SELECT c.id, COUNT(p1.id), COUNT(p2.id) FROM customers c JOIN orders p1 ON p1.customer_id = c.id LEFT JOIN orders p2 ON p2.customer_id = c.id AND p2.id <> p1.id GROUP BY c.id HAVING COUNT(p1.id) > 1 ORDER BY c.id",
		"SELECT c.id, COUNT(p1.id), COUNT(p2.id) FROM customers c JOIN orders p1 ON p1.customer_id = c.id LEFT JOIN orders p2 ON p2.customer_id = c.id AND p2.id <> p1.id GROUP BY c.id HAVING COUNT(p2.id) = 0 ORDER BY c.id",
		"SELECT p1.customer_id, SUM(p2.total) FROM orders p1 JOIN orders p2 ON p2.customer_id = p1.customer_id AND p2.id > p1.id GROUP BY p1.customer_id HAVING SUM(p2.total) > 5",
		// WordPress の形: 記事とメタの自己結合（同じ列名 meta_id、post_id）を HAVING COUNT で絞る
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post')",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id))",
		"INSERT INTO wp_posts VALUES (1, 'post'), (2, 'post'), (3, 'page')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'color', 'red'), (1, 'size', 'L'), (1, 'size', 'M'), (2, 'color', 'blue'), (3, 'size', 'S')",
		"SELECT p.ID, COUNT(m1.meta_id), COUNT(m2.meta_id) FROM wp_posts p JOIN wp_postmeta m1 ON m1.post_id = p.ID AND m1.meta_key = 'color' LEFT JOIN wp_postmeta m2 ON m2.post_id = p.ID AND m2.meta_key = 'size' GROUP BY p.ID HAVING COUNT(m2.meta_id) > 1 ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m1 ON m1.post_id = p.ID LEFT JOIN wp_postmeta m2 ON m2.post_id = p.ID AND m2.meta_id <> m1.meta_id GROUP BY p.ID HAVING COUNT(m2.meta_id) > 0 AND COUNT(m1.meta_id) > 1 ORDER BY p.ID",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE orders",
		"DROP TABLE customers",
	}},
	{3760, []string{ // スカラー副問い合わせの中の集計を、外の同じ集計の値で使い回した（MAX(d) が外の値、MIN(id) は 1105）
		"CREATE TABLE s (id INT PRIMARY KEY, d DATETIME, tag VARCHAR(10))",
		"INSERT INTO s VALUES (1, '2020-01-01 00:00:00', 'keep'), (2, '2021-01-01 00:00:00', 'skip'), (3, '2030-01-01 00:00:00', 'skip')",
		"SELECT MAX(d), (SELECT MAX(d) FROM s WHERE tag = 'keep') FROM s",
		"SELECT COUNT(id), (SELECT COUNT(id) FROM s WHERE tag = 'keep') FROM s",
		"SELECT SUM(id), (SELECT SUM(id) FROM s WHERE tag = 'keep') FROM s",
		"SELECT MIN(id), (SELECT MIN(id) FROM s WHERE tag = 'skip') FROM s",
		"SELECT MIN(d) AS min_d, (SELECT MIN(d) AS min_d FROM s WHERE tag = 'skip') FROM s",
		"SELECT COUNT(*), (SELECT COUNT(*) FROM s WHERE tag = 'keep') FROM s",
		"SELECT tag, MAX(d), (SELECT MAX(d) FROM s s2 WHERE s2.tag = 'keep') FROM s GROUP BY tag ORDER BY tag",
		"SELECT tag, COUNT(id) FROM s GROUP BY tag HAVING COUNT(id) > (SELECT COUNT(id) FROM s WHERE tag = 'keep') ORDER BY tag",
		// 相関のスカラー副問い合わせ（外の行ごとの集計）は外の集計と別
		"SELECT s.tag, MAX(s.id), (SELECT MAX(s3.id) FROM s s3 WHERE s3.tag = s.tag AND s3.id < 3) FROM s GROUP BY s.tag ORDER BY s.tag",
		// WordPress の形: 統計のプラグインが、外と副問い合わせで同じ集計を書く
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_date DATETIME NOT NULL, post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"INSERT INTO wp_posts VALUES (1, '2024-01-01 00:00:00', 'post', 'publish'), (2, '2025-01-01 00:00:00', 'post', 'draft'), (3, '2026-01-01 00:00:00', 'page', 'publish')",
		"SELECT COUNT(ID), (SELECT COUNT(ID) FROM wp_posts WHERE post_status = 'publish') FROM wp_posts",
		"SELECT MAX(post_date), (SELECT MAX(post_date) FROM wp_posts WHERE post_type = 'post') FROM wp_posts",
		"SELECT post_type, COUNT(ID), (SELECT COUNT(ID) FROM wp_posts WHERE post_status = 'draft') FROM wp_posts GROUP BY post_type ORDER BY post_type",
		"DROP TABLE wp_posts",
		"DROP TABLE s",
	}},
	{3305, []string{ // ゼロの日付（'0000-00-00'、0、false）の日時の関数が 0000-01-01 の値（月 1、日 1、土曜）を返した
		wpSQLMode,
		"SELECT YEAR('0000-00-00'), MONTH('0000-00-00'), DAY('0000-00-00'), DAYOFMONTH('0000-00-00'), QUARTER('0000-00-00')",
		"SELECT DAYOFWEEK('0000-00-00'), WEEKDAY('0000-00-00'), DAYOFYEAR('0000-00-00'), DAYNAME('0000-00-00'), MONTHNAME('0000-00-00')",
		"SELECT WEEK('0000-00-00'), YEARWEEK('0000-00-00'), WEEKOFYEAR('0000-00-00'), TO_DAYS('0000-00-00'), DATE('0000-00-00')",
		"SELECT DATEDIFF('0000-00-00', '2020-10-10'), DATEDIFF('2020-10-10', '0000-00-00')",
		"SELECT DATE_ADD(0, INTERVAL 1 DAY), DATE_ADD('0000-00-00', INTERVAL 1 DAY), DATE_SUB('0000-00-00', INTERVAL 1 DAY), TIMESTAMPADD(DAY, 1, '0000-00-00')",
		"SELECT DATE_ADD(false, INTERVAL 1 DAY), DAY(0), MONTH(false), DAYNAME(0)",
		"SELECT DATE_FORMAT('0000-00-00', '%Y-%m-%d'), DATE_FORMAT('0000-00-00 00:00:00', '%Y-%m-%d %H:%i:%s'), DATE_FORMAT('0000-00-00', '%y/%c/%e')",
		"SELECT DATE_FORMAT('0000-00-00', '%W'), DATE_FORMAT('0000-00-00', '%M %Y')",
		"SELECT EXTRACT(MONTH FROM '0000-00-00'), EXTRACT(YEAR_MONTH FROM '0000-00-00'), EXTRACT(DAY FROM '0000-00-00'), EXTRACT(YEAR FROM '0000-00-00')",
		"SELECT '0000-00-00' + INTERVAL 1 DAY, '0000-00-00 00:00:00' - INTERVAL 1 DAY",
		// ゼロでない日付は前と同じ
		"SELECT MONTH('2024-03-05'), DAY('2024-03-05'), DAYOFWEEK('2024-03-05'), QUARTER('2024-03-05'), DATEDIFF('2024-03-05', '2024-01-01'), DATE_ADD('2024-03-05', INTERVAL 1 DAY), DATE_FORMAT('2024-03-05', '%Y-%m-%d %W')",
		// WordPress の形: 下書きの post_date_gmt は '0000-00-00 00:00:00'（WordPress の sql_mode で入る）
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_date_gmt DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"INSERT INTO wp_posts VALUES (1, '2024-03-05 10:00:00', '2024-03-05 01:00:00', 'publish'), (2, '2024-04-01 09:00:00', '0000-00-00 00:00:00', 'draft'), (3, '2024-04-02 09:00:00', '0000-00-00 00:00:00', 'auto-draft')",
		"SELECT ID, YEAR(post_date_gmt), MONTH(post_date_gmt), DAY(post_date_gmt), DAYOFWEEK(post_date_gmt), DATEDIFF(post_date_gmt, '2020-01-01'), DATE_FORMAT(post_date_gmt, '%Y-%m-%d') FROM wp_posts ORDER BY ID",
		"SELECT ID, DATE(post_date_gmt), DAYNAME(post_date_gmt), WEEK(post_date_gmt), DATE_ADD(post_date_gmt, INTERVAL 1 DAY), post_date_gmt + INTERVAL 9 HOUR FROM wp_posts ORDER BY ID",
		"SELECT YEAR(post_date_gmt) AS y, MONTH(post_date_gmt) AS m, COUNT(*) FROM wp_posts GROUP BY y, m ORDER BY y, m",
		"SELECT ID FROM wp_posts WHERE MONTH(post_date_gmt) = 0 ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE DATEDIFF(NOW(), post_date_gmt) > 30 ORDER BY ID",
		"SELECT ID, post_date_gmt = '0000-00-00 00:00:00' FROM wp_posts ORDER BY ID",
		// DATE_FORMAT のすべての指定子（ゼロの日付の定数と列。MySQL は月の名前と曜日が NULL、週は意味のない値を返す）
		"SELECT DATE_FORMAT('0000-00-00', '%a'), DATE_FORMAT('0000-00-00', '%A'), DATE_FORMAT('0000-00-00', '%b'), DATE_FORMAT('0000-00-00', '%c'), DATE_FORMAT('0000-00-00', '%D'), DATE_FORMAT('0000-00-00', '%d'), DATE_FORMAT('0000-00-00', '%e'), DATE_FORMAT('0000-00-00', '%f'), DATE_FORMAT('0000-00-00', '%H'), DATE_FORMAT('0000-00-00', '%h'), DATE_FORMAT('0000-00-00', '%I')",
		"SELECT DATE_FORMAT(post_date_gmt, '%a'), DATE_FORMAT(post_date_gmt, '%A'), DATE_FORMAT(post_date_gmt, '%b'), DATE_FORMAT(post_date_gmt, '%c'), DATE_FORMAT(post_date_gmt, '%D'), DATE_FORMAT(post_date_gmt, '%d'), DATE_FORMAT(post_date_gmt, '%e'), DATE_FORMAT(post_date_gmt, '%f'), DATE_FORMAT(post_date_gmt, '%H'), DATE_FORMAT(post_date_gmt, '%h'), DATE_FORMAT(post_date_gmt, '%I') FROM wp_posts WHERE ID = 2",
		"SELECT DATE_FORMAT('0000-00-00', '%i'), DATE_FORMAT('0000-00-00', '%j'), DATE_FORMAT('0000-00-00', '%k'), DATE_FORMAT('0000-00-00', '%l'), DATE_FORMAT('0000-00-00', '%M'), DATE_FORMAT('0000-00-00', '%m'), DATE_FORMAT('0000-00-00', '%p'), DATE_FORMAT('0000-00-00', '%r'), DATE_FORMAT('0000-00-00', '%S'), DATE_FORMAT('0000-00-00', '%s'), DATE_FORMAT('0000-00-00', '%T')",
		"SELECT DATE_FORMAT(post_date_gmt, '%i'), DATE_FORMAT(post_date_gmt, '%j'), DATE_FORMAT(post_date_gmt, '%k'), DATE_FORMAT(post_date_gmt, '%l'), DATE_FORMAT(post_date_gmt, '%M'), DATE_FORMAT(post_date_gmt, '%m'), DATE_FORMAT(post_date_gmt, '%p'), DATE_FORMAT(post_date_gmt, '%r'), DATE_FORMAT(post_date_gmt, '%S'), DATE_FORMAT(post_date_gmt, '%s'), DATE_FORMAT(post_date_gmt, '%T') FROM wp_posts WHERE ID = 2",
		"SELECT DATE_FORMAT('0000-00-00', '%U'), DATE_FORMAT('0000-00-00', '%u'), DATE_FORMAT('0000-00-00', '%V'), DATE_FORMAT('0000-00-00', '%v'), DATE_FORMAT('0000-00-00', '%W'), DATE_FORMAT('0000-00-00', '%w'), DATE_FORMAT('0000-00-00', '%X'), DATE_FORMAT('0000-00-00', '%x'), DATE_FORMAT('0000-00-00', '%Y'), DATE_FORMAT('0000-00-00', '%y'), DATE_FORMAT('0000-00-00', '%%')",
		"SELECT DATE_FORMAT(post_date_gmt, '%U'), DATE_FORMAT(post_date_gmt, '%u'), DATE_FORMAT(post_date_gmt, '%V'), DATE_FORMAT(post_date_gmt, '%v'), DATE_FORMAT(post_date_gmt, '%W'), DATE_FORMAT(post_date_gmt, '%w'), DATE_FORMAT(post_date_gmt, '%X'), DATE_FORMAT(post_date_gmt, '%x'), DATE_FORMAT(post_date_gmt, '%Y'), DATE_FORMAT(post_date_gmt, '%y'), DATE_FORMAT(post_date_gmt, '%%') FROM wp_posts WHERE ID = 2",
		// DATE_FORMAT の値を STR_TO_DATE で読み戻す（NO_ZERO_DATE のない sql_mode ではゼロの日付。一部だけ 0 の日付は NULL）
		"SELECT ID, STR_TO_DATE(DATE_FORMAT(post_date_gmt, '%Y-%m-%d'), '%Y-%m-%d'), STR_TO_DATE(DATE_FORMAT(post_date_gmt, '%Y-%m-%d %H:%i:%s'), '%Y-%m-%d %H:%i:%s') FROM wp_posts ORDER BY ID",
		"SELECT STR_TO_DATE('0000-00-00', '%Y-%m-%d'), STR_TO_DATE('00/00/0000', '%m/%d/%Y'), STR_TO_DATE('2024-00-00', '%Y-%m-%d'), STR_TO_DATE('0', '%Y')",
		// %y と 2 桁以下の %Y は 2 桁の年（00 は 2000 年）なのでゼロの日付にならない。書式の順に読み、文字列が先に尽きても読める
		"SELECT STR_TO_DATE('000000', '%y%m%d'), STR_TO_DATE('00-00-00', '%y-%m-%d'), STR_TO_DATE('0000-00-00', '%y-%m-%d'), STR_TO_DATE('0-0-0', '%Y-%m-%d'), STR_TO_DATE('00-0-0', '%Y-%m-%d')",
		"SELECT STR_TO_DATE('000-00-00', '%Y-%m-%d'), STR_TO_DATE('00000-00-00', '%Y-%m-%d'), STR_TO_DATE('0000-0-0', '%Y-%m-%d'), STR_TO_DATE('0000/00/00', '%Y/%m/%d'), STR_TO_DATE('0000-00-00', '%Y-%c-%e'), STR_TO_DATE('00000000', '%Y%m%d')",
		"SELECT STR_TO_DATE('0000-00-00', '%Y-%m-%d %H:%i:%s'), STR_TO_DATE('0000-00-00abc', '%Y-%m-%d'), STR_TO_DATE('0000-00-00', '%d-%m-%Y'), STR_TO_DATE('00.00.0000', '%d.%m.%Y'), STR_TO_DATE('00', '%m'), STR_TO_DATE('0000-00', '%Y-%m'), STR_TO_DATE('00:00:00', '%H:%i:%s')",
		"SELECT STR_TO_DATE('2024-01-00', '%Y-%m-%d'), STR_TO_DATE('0000-01-00', '%Y-%m-%d'), STR_TO_DATE('0000-00-01', '%Y-%m-%d')",
		"SET SESSION sql_mode = DEFAULT",
		"SELECT STR_TO_DATE('0000-00-00', '%Y-%m-%d'), STR_TO_DATE('0000-00-00 00:00:00', '%Y-%m-%d %H:%i:%s')",
		"DROP TABLE wp_posts",
	}},
	{3626, []string{ // 副問い合わせの中の行（外の行 || 子の行）で、段階 96 の TopN の足したキーと順番の列の位置がずれ、キーが効かなかった
		"CREATE TABLE foo (id VARCHAR(36) PRIMARY KEY)",
		"CREATE TABLE bar (id VARCHAR(36) PRIMARY KEY, foo_id VARCHAR(36) NOT NULL, approved_on DATETIME(3))",
		"INSERT INTO foo VALUES ('foo-1'),('foo-2'),('foo-3'),('foo-4'),('foo-5'),('foo-6'),('foo-7'),('foo-8')",
		"INSERT INTO bar VALUES ('bar-1','foo-1','2026-07-14 07:18:04.000'),('bar-2','foo-2','2026-07-14 07:18:03.000'),('bar-3','foo-3','2026-07-14 07:18:05.000'),('bar-4','foo-4','2026-07-14 07:18:01.000'),('bar-5','foo-5','2026-07-14 07:18:08.000'),('bar-6','foo-6','2026-07-14 07:18:02.000'),('bar-7','foo-7','2026-07-14 07:18:07.000'),('bar-8','foo-8','2026-07-14 07:18:00.000')",
		"SELECT * FROM foo ORDER BY (SELECT MAX(r.approved_on) FROM bar r WHERE r.foo_id = foo.id) ASC LIMIT 1",
		"SELECT * FROM foo ORDER BY (SELECT MAX(r.approved_on) FROM bar r WHERE r.foo_id = foo.id) DESC LIMIT 2",
		"SELECT * FROM foo ORDER BY (SELECT MAX(r.approved_on) FROM bar r WHERE r.foo_id = foo.id) ASC LIMIT 4",
		"SELECT * FROM foo ORDER BY (SELECT MAX(r.approved_on) FROM bar r WHERE r.foo_id = foo.id) DESC",
		// 相関の副問い合わせの中の ORDER BY ... LIMIT（行は外の行を前に持つ）
		"SELECT f.id, (SELECT r.id FROM bar r WHERE r.foo_id <> f.id ORDER BY CONCAT(r.approved_on, '') DESC, r.id LIMIT 1) FROM foo f ORDER BY f.id",
		"SELECT f.id, (SELECT r.id FROM bar r WHERE r.foo_id >= f.id ORDER BY LOWER(r.id) LIMIT 1) FROM foo f ORDER BY f.id",
		// 相関の副問い合わせの中の式の並べ替え（同順位は id で決める）
		"CREATE TABLE t (id INT PRIMARY KEY, k INT)",
		"INSERT INTO t VALUES (1, 2), (2, 1), (3, 2), (4, 1), (5, 2)",
		"SELECT t1.id, (SELECT t2.id FROM t t2 WHERE t2.id <> t1.id ORDER BY t2.k + 0, t2.id LIMIT 1) FROM t t1 ORDER BY t1.id",
		// ORDER BY RAND() は 1 行に 1 回だけ評価する（全部の行を返す。順番は乱数なので数だけを見る）
		"SELECT COUNT(*) FROM (SELECT id FROM t ORDER BY RAND()) d",
		"SELECT SUM(id) FROM (SELECT id FROM t ORDER BY RAND()) d",
		"SELECT COUNT(DISTINCT id) FROM (SELECT id FROM t ORDER BY RAND() LIMIT 3) d",
		// WordPress の形: 最新のコメントの日時で記事を並べる（相関の副問い合わせの ORDER BY ... LIMIT）
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_title TEXT NOT NULL)",
		"CREATE TABLE wp_comments (comment_ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, comment_post_ID BIGINT UNSIGNED NOT NULL DEFAULT 0, comment_date DATETIME NOT NULL, KEY comment_post_ID (comment_post_ID))",
		"INSERT INTO wp_posts VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd')",
		"INSERT INTO wp_comments VALUES (1, 1, '2024-01-03 00:00:00'), (2, 2, '2024-01-05 00:00:00'), (3, 3, '2024-01-01 00:00:00'), (4, 1, '2024-01-02 00:00:00'), (5, 4, '2024-01-04 00:00:00')",
		"SELECT p.ID FROM wp_posts p ORDER BY (SELECT MAX(c.comment_date) FROM wp_comments c WHERE c.comment_post_ID = p.ID) DESC LIMIT 2",
		"SELECT p.ID FROM wp_posts p ORDER BY (SELECT MAX(c.comment_date) FROM wp_comments c WHERE c.comment_post_ID = p.ID) ASC LIMIT 3",
		"SELECT p.ID, (SELECT c.comment_ID FROM wp_comments c WHERE c.comment_post_ID = p.ID ORDER BY DATE(c.comment_date) DESC LIMIT 1) FROM wp_posts p ORDER BY p.ID",
		"DROP TABLE wp_comments",
		"DROP TABLE wp_posts",
		"DROP TABLE t",
		"DROP TABLE bar",
		"DROP TABLE foo",
	}},
	{3302, []string{ // 副問い合わせから持ち上げた外の列だけの条件が NULL になると、NOT EXISTS の NOT(EXISTS ... AND 条件) が NULL になり行が落ちた
		"CREATE TABLE t0 (c0 INT, c1 INT)",
		"INSERT INTO t0 VALUES (1, -2)",
		"SELECT * FROM t0 WHERE NOT EXISTS (SELECT 1 FROM (SELECT 1) alias0 WHERE (CASE -1 WHEN t0.c1 THEN false END))",
		"SELECT * FROM t0 WHERE EXISTS (SELECT 1 FROM (SELECT 1) alias0 WHERE (CASE -1 WHEN t0.c1 THEN false END))",
		"CREATE TABLE t1 (c0 INT PRIMARY KEY)",
		"INSERT INTO t1 VALUES (1)",
		"SELECT * FROM t1 WHERE NOT EXISTS (SELECT 1 FROM (SELECT 1) AS subQuery WHERE WEEKDAY(t1.c0))",
		"CREATE TABLE t2 (c0 DOUBLE)",
		"INSERT INTO t2 VALUES (-1)",
		"SELECT * FROM t2 WHERE NOT EXISTS (SELECT 1 FROM (SELECT 1) AS sq WHERE LOG(t2.c0))",
		"SELECT * FROM t2 WHERE NOT EXISTS (SELECT 1 FROM (SELECT 1) AS sq WHERE SQRT(t2.c0))",
		"CREATE TABLE u (a INT PRIMARY KEY, b INT)",
		"INSERT INTO u VALUES (1, 10), (2, NULL), (3, 30)",
		"SELECT a FROM u WHERE NOT EXISTS (SELECT 1 FROM t1 WHERE u.b > 15) ORDER BY a",
		"SELECT a FROM u WHERE EXISTS (SELECT 1 FROM t1 WHERE u.b > 15) ORDER BY a",
		"SELECT a FROM u WHERE NOT EXISTS (SELECT 1 FROM t1 WHERE t1.c0 = 1 AND u.b + 0 > 15) ORDER BY a",
		"SELECT a FROM u WHERE a NOT IN (SELECT c0 FROM t1 WHERE u.b > 15) ORDER BY a",
		"SELECT a FROM u WHERE NOT EXISTS (SELECT 1 FROM t1 WHERE u.a > 1) ORDER BY a",
		// WordPress の形: 外の記事の列だけの条件を NOT EXISTS の中に書くプラグイン（NOT NULL の列は前と同じ）
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0, post_type VARCHAR(20) NOT NULL DEFAULT 'post', menu_order INT DEFAULT NULL)",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id))",
		"INSERT INTO wp_posts VALUES (1, 0, 'post', 1), (2, 1, 'attachment', NULL), (3, 0, 'page', 5)",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '2'), (3, '_edit_lock', '1')",
		"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = p.ID AND p.post_type = 'post') ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.meta_key = '_edit_lock' AND p.menu_order > 2) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.meta_key = '_edit_lock' AND CASE p.post_type WHEN 'page' THEN 1 END) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p WHERE EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.meta_key = '_thumbnail_id' AND p.post_parent = 0) ORDER BY p.ID",
		// 外部結合の NULL で埋められる側の NOT NULL の列（LEFT JOIN の右、RIGHT JOIN の左）は NULL になりうる。列の定義だけを
		// 見ると包まず、NOT(EXISTS ... AND NULL) で行が落ちた（段階 113 のレビュー）
		"CREATE TABLE wp_term_relationships (object_id BIGINT UNSIGNED NOT NULL DEFAULT 0, term_taxonomy_id BIGINT UNSIGNED NOT NULL DEFAULT 0, term_order INT NOT NULL DEFAULT 0, PRIMARY KEY (object_id, term_taxonomy_id))",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (1, 10), (2, 10), (3, 11)",
		"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.meta_id > 0) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.post_id = 1) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.post_id = p.ID) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.meta_id IN (1, 3)) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE p.ID NOT IN (SELECT tr.object_id FROM wp_term_relationships tr WHERE m.meta_id > 0) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.meta_id > 0) ORDER BY p.ID",
		"SELECT p.ID FROM wp_postmeta m RIGHT JOIN wp_posts p ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.meta_id > 0) ORDER BY p.ID",
		"SELECT d.ID FROM (SELECT p.ID, m.meta_id FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id') d WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = d.ID AND d.meta_id > 0) ORDER BY d.ID",
		"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = tr.object_id AND p.post_type = 'post')) ORDER BY p.ID",
		// 外と内で同じ別名を使う（外の m は内部結合、中の m は LEFT JOIN の NULL で埋められる側）。名前は内側のスコープから探す
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr LEFT JOIN wp_postmeta m ON m.post_id = tr.object_id AND m.meta_key = '_thumbnail_id' WHERE tr.object_id = p.ID AND NOT EXISTS (SELECT 1 FROM wp_posts q WHERE q.ID = tr.object_id AND m.meta_id > 0)) ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr LEFT JOIN wp_postmeta m ON m.post_id = tr.object_id AND m.meta_key = '_thumbnail_id' WHERE tr.object_id = p.ID AND NOT EXISTS (SELECT 1 FROM wp_posts q WHERE q.ID = tr.object_id AND m.meta_id > 0)) ORDER BY p.ID",
		"DROP TABLE wp_term_relationships",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE u",
		"DROP TABLE t2",
		"DROP TABLE t1",
		"DROP TABLE t0",
	}},
}

// 段階 113 で取り込んだ上流の修正の後、再現の文が MySQL と同じ結果になる。
func TestStage113UpstreamFixes(t *testing.T) {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage113Groups {
		stmts = append(stmts, g.stmts...)
	}
	checkGolden(t, "../testdata/stage113_golden.txt", "heimdb_golden113", stmts)
}

// LIKE 'prefix%' は、utf8mb4_unicode_520_ci の列では下限と U+10FFFF を足した上限の索引の範囲になり、LIKE が残りの条件と
// して残る（上流の #3581。前は上限が byte(255) で、'z' や CJK の値が範囲から落ちた）。WordPress の esc_like のパターンは
// HeimDB が ESCAPE を明示するので前と同じく置き換えない。
func TestStage113LikePrefixPlans(t *testing.T) {
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
		q          string
		want, deny []string
	}{
		{ // WP_User_Query の search 'yama*'
			"SELECT ID FROM wp_users WHERE user_login LIKE 'yama%'",
			[]string{"index: [wp_users.user_login]", "(wp_users.user_login <= 'yama\U0010FFFF')", "wp_users.user_login LIKE 'yama%'"},
			[]string{"ÿ"},
		},
		{
			"SELECT option_name, option_value FROM wp_options WHERE option_name LIKE 'widget%'",
			[]string{"index: [wp_options.option_name]", "(wp_options.option_name >= 'widget')", "wp_options.option_name LIKE 'widget%'"},
			nil,
		},
		{ // NOT LIKE も LIKE を残す
			"SELECT ID FROM wp_users WHERE user_login NOT LIKE 'yama%'",
			[]string{"NOT(wp_users.user_login LIKE 'yama%')"},
			nil,
		},
		{ // esc_like のパターン（ESCAPE を明示した LIKE）は置き換えない
			`SELECT option_name FROM wp_options WHERE option_name LIKE '\_transient\_%'`,
			[]string{"wp_options.option_name LIKE"},
			[]string{">=", "<="},
		},
		{ // 派生表の中でも範囲を重ねない
			"SELECT d.ID FROM (SELECT ID FROM wp_users WHERE user_login LIKE 'yama%') d",
			[]string{"(wp_users.user_login >= 'yama')"},
			[]string{"AND (wp_users.user_login >= 'yama')"},
		},
	} {
		plan := explain(t, db, c.q)
		for _, w := range c.want {
			if !strings.Contains(plan, w) {
				t.Errorf("%s\nplan has no %q:\n%s", c.q, w, plan)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(plan, d) {
				t.Errorf("%s\nplan has %q:\n%s", c.q, d, plan)
			}
		}
	}
}

// 副問い合わせから持ち上げた外の列だけの条件は、NULL になりうるときだけ IsTrue で包む（上流の #3302）。WordPress の
// NOT NULL の列と定数の比較は前と同じ計画（包まず、半結合と反結合と索引の範囲のまま）。
func TestStage113HoistedFilterPlans(t *testing.T) {
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
		q          string
		want, deny []string
	}{
		{
			"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = p.ID AND p.post_type = 'post')",
			[]string{"(p.post_type = 'post')"},
			[]string{"IS TRUE"},
		},
		{
			"SELECT p.ID FROM wp_posts p WHERE EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = p.ID AND p.post_type = 'post' AND p.post_status IN ('publish', 'draft'))",
			[]string{"((p.post_type = 'post') AND (p.post_status HASH IN ('publish', 'draft')))", "MergeJoin"},
			[]string{"IS TRUE"},
		},
		{ // 外部結合の NULL で埋められる側の NOT NULL の列は包む（列の定義ではなく結合のスキーマで決める）
			"SELECT p.ID FROM wp_posts p LEFT JOIN wp_postmeta m ON m.post_id = p.ID AND m.meta_key = '_thumbnail_id' WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND m.meta_id > 0)",
			[]string{"(m.meta_id > 0) IS TRUE"},
			nil,
		},
		{ // 2 段の NOT EXISTS で外の NOT NULL の列の比較は包まない
			"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_term_relationships tr WHERE tr.object_id = p.ID AND NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = tr.object_id AND p.post_type = 'post'))",
			[]string{"(p.post_type = 'post')"},
			[]string{"IS TRUE"},
		},
		{ // NULL になりうる式（CASE）は包む
			"SELECT p.ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.meta_key = '_edit_lock' AND CASE p.post_type WHEN 'page' THEN 1 END)",
			[]string{"IS TRUE"},
			nil,
		},
	} {
		plan := explain(t, db, c.q)
		for _, w := range c.want {
			if !strings.Contains(plan, w) {
				t.Errorf("%s\nplan has no %q:\n%s", c.q, w, plan)
			}
		}
		for _, d := range c.deny {
			if strings.Contains(plan, d) {
				t.Errorf("%s\nplan has %q:\n%s", c.q, d, plan)
			}
		}
	}
}
