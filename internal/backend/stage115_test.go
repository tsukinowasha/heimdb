package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// stage115WPTables は、段階 115 の NOT IN の形で使う WordPress の表（列と索引は WordPress のもの。NOT IN の列はどれも NOT NULL）。
var stage115WPTables = []string{
	"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0, post_type VARCHAR(20) NOT NULL DEFAULT 'post', " +
		"post_status VARCHAR(20) NOT NULL DEFAULT 'publish', post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', PRIMARY KEY (ID), KEY post_parent (post_parent), " +
		"KEY type_status_date (post_type, post_status, post_date, ID)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
	"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, " +
		"meta_value LONGTEXT, PRIMARY KEY (meta_id), KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
	"CREATE TABLE wp_term_relationships (object_id BIGINT UNSIGNED NOT NULL DEFAULT 0, term_taxonomy_id BIGINT UNSIGNED NOT NULL DEFAULT 0, " +
		"term_order INT NOT NULL DEFAULT 0, PRIMARY KEY (object_id, term_taxonomy_id), KEY term_taxonomy_id (term_taxonomy_id)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
	"INSERT INTO wp_posts (post_parent, post_type) VALUES (0, 'post'), (0, 'page'), (2, 'attachment'), (0, 'post'), (9, 'attachment')",
	"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '3'), (2, '_thumbnail_id', NULL), (4, 'x', '1'), (5, 'x', '5')",
	"INSERT INTO wp_term_relationships VALUES (1, 1, 0), (4, 2, 0)",
}

// stage115NotInShapes は、WordPress の NOT IN の 6 つの形（段階 80 の形と、WP_Query、プラグインの形）。NOT IN の左の列
// （ID、post_parent、post_id）は NOT NULL なので、#3960 の後も計画は変わらない（TestStage115NotInPlans）。
var stage115NotInShapes = []struct{ q, join string }{
	{"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT meta_value FROM wp_postmeta WHERE meta_key = '_thumbnail_id') ORDER BY ID", "LeftOuterHashJoinExcludingNulls"},
	{"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT post_id FROM wp_postmeta WHERE meta_key = 'x') ORDER BY ID", "LeftOuterHashJoinExcludingNulls"},
	{"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT object_id FROM wp_term_relationships WHERE term_taxonomy_id IN (1, 2)) ORDER BY ID", "LeftOuterMergeJoin"},
	{"SELECT ID FROM wp_posts WHERE post_parent NOT IN (SELECT ID FROM wp_posts WHERE post_type = 'page') ORDER BY ID", "LeftOuterHashJoinExcludingNulls"},
	{"SELECT post_id FROM wp_postmeta WHERE meta_key = 'x' AND post_id NOT IN (SELECT ID FROM wp_posts WHERE post_type = 'post') ORDER BY post_id", "LeftOuterLookupJoin"},
	{"SELECT meta_id FROM wp_postmeta WHERE meta_value NOT IN (SELECT ID FROM wp_posts WHERE post_type = 'attachment') ORDER BY meta_id", "LeftOuterHashJoinExcludingNulls"},
}

func stage115NotInStmts() []string {
	var out []string
	for _, s := range stage115NotInShapes {
		out = append(out, s.q)
	}
	return out
}

// stage115Groups は、段階 115 で取り込んだ上流の go-mysql-server の修正と、前の段階から回した計画と SQL の形の違いの
// 再現の文（docs/engine-upstream.md）。直す前の HeimDB は本物の MySQL 8.0 と違う結果かエラーを返した。各組の終わりに
// 作った表を消し、sql_mode を既定に戻す。
var stage115Groups = []struct {
	pr    int
	stmts []string
}{
	{3960, append(append(append([]string{ // 索引のある NULL を持てる列の NOT IN (SELECT ...)。lookup 結合とマージ結合は NULL の比べを見ない
		"CREATE TABLE nullable_left (id INT PRIMARY KEY, k INT, INDEX nullable_left_k_idx (k))",
		"INSERT INTO nullable_left VALUES (1, 1), (2, 2), (3, 3), (4, NULL), (5, 5)",
		"CREATE TABLE null_key (id INT PRIMARY KEY, k INT, INDEX null_key_k_idx (k))",
		"INSERT INTO null_key VALUES (1, 1), (2, 2), (3, NULL)",
		"CREATE TABLE nullable_right (id INT PRIMARY KEY, k INT, INDEX nullable_right_k_idx (k))",
		"INSERT INTO nullable_right VALUES (1, 1), (2, 2)",
		"CREATE TABLE nonnull_left (id INT PRIMARY KEY, k INT NOT NULL, INDEX nonnull_left_k_idx (k))",
		"INSERT INTO nonnull_left VALUES (1, 1), (2, 2), (3, 3), (4, 5)",
		"SELECT k FROM nullable_left WHERE k NOT IN (SELECT k FROM null_key) ORDER BY k",
		"SELECT /*+ MERGE_JOIN(nullable_left, null_key) */ k FROM nullable_left WHERE k NOT IN (SELECT k FROM null_key) ORDER BY k",
		"SELECT /*+ LOOKUP_JOIN(nullable_left, null_key) */ k FROM nullable_left WHERE k NOT IN (SELECT k FROM null_key) ORDER BY k",
		"SELECT k FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right) ORDER BY k",
		"SELECT /*+ MERGE_JOIN(nullable_left, nullable_right) */ k FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right) ORDER BY k",
		"SELECT /*+ LOOKUP_JOIN(nullable_left, nullable_right) */ k FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right) ORDER BY k",
		"SELECT id FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right WHERE k IS NOT NULL) ORDER BY id",
		"SELECT id FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right) AND k IS NOT NULL ORDER BY id",
		"SELECT id FROM nullable_left WHERE NOT EXISTS (SELECT 1 FROM nullable_right r WHERE r.k = nullable_left.k) ORDER BY id",
		"SELECT k FROM nullable_left WHERE NOT EXISTS (SELECT 1 FROM null_key WHERE null_key.k = nullable_left.k) ORDER BY k IS NOT NULL, k",
		"SELECT k FROM nonnull_left WHERE k NOT IN (SELECT k FROM nullable_right) ORDER BY k",
		"SELECT /*+ MERGE_JOIN(nonnull_left, null_key) */ k FROM nonnull_left WHERE k NOT IN (SELECT k FROM null_key) ORDER BY k",
		"SELECT /*+ LOOKUP_JOIN(nonnull_left, null_key) */ k FROM nonnull_left WHERE k NOT IN (SELECT k FROM null_key) ORDER BY k",
		"UPDATE nullable_left SET id = id + 10 WHERE k NOT IN (SELECT k FROM nullable_right)",
		"SELECT id, k FROM nullable_left ORDER BY id",
		"DELETE FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right)",
		"SELECT id, k FROM nullable_left ORDER BY id",
		wpSQLMode,
	}, stage115WPTables...), stage115NotInStmts()...), []string{
		// プラグインの表の NULL を持てる親の列（索引付き）の NOT IN
		"CREATE TABLE wp_plugin_items (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_parent BIGINT UNSIGNED NULL, KEY post_parent (post_parent))",
		"INSERT INTO wp_plugin_items (post_parent) VALUES (1), (2), (NULL), (4), (7)",
		"SELECT id FROM wp_plugin_items WHERE post_parent NOT IN (SELECT ID FROM wp_posts WHERE post_type = 'post') ORDER BY id",
		"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT post_parent FROM wp_plugin_items) ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT post_parent FROM wp_plugin_items WHERE post_parent IS NOT NULL) ORDER BY ID",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_plugin_items",
		"DROP TABLE wp_term_relationships",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE nonnull_left",
		"DROP TABLE nullable_right",
		"DROP TABLE null_key",
		"DROP TABLE nullable_left",
	}...)},
	{3110, []string{ // UNION の派生テーブル: 外の WHERE で行が消える（#3110）、ENUM と文字列の UNION の型（#3060）
		"CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, words VARCHAR(100))",
		"INSERT INTO t (words) VALUES ('foo'), ('bar'), ('baz'), ('zap')",
		"CREATE TABLE t2 (id INT PRIMARY KEY, words VARCHAR(100))",
		"INSERT INTO t2 SELECT * FROM t",
		"UPDATE t2 SET words = 'boo' WHERE id = 1",
		"SELECT * FROM (SELECT 'parent' AS tbl, id, words FROM t UNION SELECT 'child' AS tbl, id, words FROM t2) AS combined WHERE combined.id = 1 ORDER BY tbl DESC",
		"SELECT * FROM (SELECT 'parent' AS tbl, id, words FROM t UNION ALL SELECT 'child' AS tbl, id, words FROM t2) AS combined WHERE combined.words = 'foo' ORDER BY tbl DESC",
		"SELECT * FROM (SELECT id, words FROM t UNION SELECT id, words FROM t2) AS combined WHERE combined.id = 1 ORDER BY words",
		"SELECT tbl, COUNT(*) FROM (SELECT 'parent' AS tbl, id FROM t UNION ALL SELECT 'child', id FROM t2) c WHERE c.id > 2 GROUP BY tbl ORDER BY tbl",
		"WITH a (x) AS (SELECT 1 UNION SELECT 2) SELECT * FROM a WHERE x > 1",
		"WITH a AS (SELECT 'p' AS k, id FROM t UNION SELECT 'c', id FROM t2) SELECT k, id FROM a WHERE id = 2 ORDER BY k",
		"WITH RECURSIVE a (x) AS (SELECT 1 UNION SELECT 2) SELECT * FROM a WHERE x > 1 UNION SELECT * FROM a WHERE x > 1",
		"CREATE TABLE enum_table (i INT PRIMARY KEY, e ENUM('a','b') NOT NULL)",
		"INSERT INTO enum_table VALUES (1, 'a'), (2, 'b')",
		"CREATE TABLE uv (u INT PRIMARY KEY, v VARCHAR(10))",
		"INSERT INTO uv VALUES (0, 'bug'), (1, 'ant'), (3, NULL)",
		"SELECT * FROM (SELECT e FROM enum_table UNION SELECT v FROM uv) sq ORDER BY 1",
		"WITH a AS (SELECT e FROM enum_table UNION SELECT v FROM uv) SELECT * FROM a ORDER BY e",
		"SELECT e FROM enum_table UNION SELECT v FROM uv ORDER BY 1",
		"SELECT e FROM enum_table UNION ALL SELECT v FROM uv ORDER BY 1",
		"SELECT e FROM enum_table UNION ALL SELECT v FROM uv ORDER BY e DESC",
		"SELECT v FROM uv UNION SELECT e FROM enum_table ORDER BY 1",
		"SELECT COUNT(*) FROM (SELECT e FROM enum_table UNION ALL SELECT v FROM uv) sq WHERE e IS NULL",
		"SELECT e, e = 'bug' FROM (SELECT e FROM enum_table UNION ALL SELECT v FROM uv) sq ORDER BY 1",
		"SELECT * FROM (SELECT e FROM enum_table UNION ALL SELECT v FROM uv) sq WHERE e = 'a'",
		"SELECT COUNT(*), MAX(e), MIN(e) FROM (SELECT e FROM enum_table UNION ALL SELECT v FROM uv) sq",
		"SELECT * FROM (SELECT i, e FROM enum_table UNION ALL SELECT u, 'b' FROM uv) sq ORDER BY e, i",
		wpSQLMode,
		// WordPress の形: 投稿とコメントをまとめた一覧（プラグインの活動のフィード）を外で絞る
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title TEXT NOT NULL, post_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', post_status VARCHAR(20) NOT NULL DEFAULT 'publish') DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_posts (post_title, post_date) VALUES ('a', '2024-01-01 09:00:00'), ('b', '2024-01-02 09:00:00')",
		"CREATE TABLE wp_comments (comment_ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, comment_post_ID BIGINT UNSIGNED NOT NULL DEFAULT 0, comment_content TEXT NOT NULL, comment_date DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', comment_approved VARCHAR(20) NOT NULL DEFAULT '1') DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_comments (comment_post_ID, comment_content, comment_date) VALUES (1, 'c1', '2024-01-01 10:00:00'), (2, 'c2', '2024-01-03 10:00:00')",
		"SELECT * FROM (SELECT 'post' AS kind, ID AS id, post_title AS body, post_date AS d FROM wp_posts WHERE post_status = 'publish' UNION ALL SELECT 'comment', comment_ID, comment_content, comment_date FROM wp_comments WHERE comment_approved = '1') feed WHERE feed.id = 1 ORDER BY d",
		"SELECT kind, COUNT(*) FROM (SELECT 'post' AS kind, ID AS id FROM wp_posts UNION SELECT 'comment', comment_ID FROM wp_comments) feed WHERE feed.id >= 2 GROUP BY kind ORDER BY kind",
		// 3 つ以上の UNION（(A UNION B) UNION C）の列の順（上流の #3141。#3110 と合わせて要る）
		"SELECT 1 AS r, ID, post_title, NULL AS c, post_date FROM wp_posts UNION SELECT 2, comment_ID, comment_content, comment_post_ID, comment_date FROM wp_comments UNION SELECT 3, ID, post_status, ID, NULL FROM wp_posts ORDER BY r, ID",
		"SELECT * FROM (SELECT 1 AS r, ID, post_title, NULL AS c FROM wp_posts UNION ALL SELECT 2, comment_ID, comment_content, comment_post_ID FROM wp_comments UNION ALL SELECT 3, ID, post_status, ID FROM wp_posts) x WHERE x.ID = 2 ORDER BY r",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_comments",
		"DROP TABLE wp_posts",
		"DROP TABLE uv",
		"DROP TABLE enum_table",
		"DROP TABLE t2",
		"DROP TABLE t",
	}},
	{3191, []string{ // DECIMAL: ハッシュ結合のキー（#3191）、定数を列の桁に丸めずに比べる（#4054。文字列と WHERE の double の定数も DECIMAL で比べる）、索引の範囲の端
		"CREATE TABLE t1 (c0 DECIMAL(6,3))",
		"CREATE TABLE t2 (c0 DECIMAL(5,2))",
		"INSERT INTO t1 VALUES (10.000), (20.505), (30.000)",
		"INSERT INTO t2 VALUES (20.5), (25.0), (30.0)",
		"SELECT * FROM t1 JOIN t2 ON t1.c0 = t2.c0",
		"SELECT /*+ HASH_JOIN(t1, t2) */ * FROM t1 JOIN t2 ON t1.c0 = t2.c0",
		"SELECT * FROM t1 WHERE c0 IN (SELECT c0 FROM t2)",
		"SELECT /*+ HASH_JOIN(t1, t2) */ * FROM t1 JOIN t2 ON t1.c0 + 0 = t2.c0 + 0",
		"SELECT /*+ HASH_JOIN(t1, t2) */ * FROM t1 JOIN t2 ON t1.c0 * 1 = t2.c0",
		"SELECT /*+ HASH_JOIN(t1, t2) */ * FROM t1 JOIN t2 ON ROUND(t1.c0, 1) = ROUND(t2.c0, 1)",
		"SELECT 1.0 IN (1.00), 1.0 IN (1.00, 2.000, 3, 4, 5, 6, 7, 8, 9, 10, 11), 30.000 IN (30.00, 20.50, 25.0, 1, 2, 3, 4, 5, 6, 7, 8)",
		"SELECT * FROM t1 WHERE c0 IN (30.00, 20.5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11) ORDER BY 1",
		"SELECT * FROM t1 WHERE c0 + 0 IN (30.00, 20.5, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11) ORDER BY 1",
		"CREATE TABLE u1 (c0 INT)",
		"CREATE TABLE u2 (c0 INT)",
		"INSERT INTO u1 VALUES (1), (2)",
		"INSERT INTO u2 VALUES (3), (4)",
		"SELECT /*+ HASH_JOIN(u1, u2) */ * FROM u1 JOIN u2 ON (u1.c0 % 2) = (u2.c0 % 2) ORDER BY 1",
		"SELECT * FROM u1 JOIN u2 ON (u1.c0 % 2) = (u2.c0 % 2) ORDER BY 1",
		"SELECT /*+ HASH_JOIN(u1, u2) */ * FROM u1 JOIN u2 ON (u1.c0 + 0.0) = (u2.c0 - 2.0) ORDER BY 1",
		"SELECT /*+ HASH_JOIN(u1, u2) */ * FROM u1 JOIN u2 ON (u1.c0 / 1) = (u2.c0 / 2) ORDER BY 1",
		"CREATE TABLE dec_tbl (d DECIMAL(10,2))",
		"INSERT INTO dec_tbl VALUES (123.45)",
		"SELECT d = 123.45, d = 123.45000001, d > 123.45000001, d < 123.45000001, d <=> 123.451, d >= 123.455, d <> 123.451 FROM dec_tbl",
		"SELECT d = '123.451', d = '123.45', d < '123.451', d = 123.451e0, d = 123.45e0, d > 123.449e0, d = 123, d > 123, d = TRUE FROM dec_tbl",
		// DECIMAL と、double では表せない桁の文字列は DECIMAL で比べる（WHERE の double の定数も）
		"CREATE TABLE dt2 (d DECIMAL(30,25))",
		"INSERT INTO dt2 VALUES (123.45), (0.1000000000000000055511151)",
		"SELECT d, d = '123.4500000000000000001', d < '123.4500000000000000001', d = '0.1', d IN ('123.4500000000000000001') FROM dt2",
		"SELECT COUNT(*) FROM dt2 WHERE d = '123.4500000000000000001'",
		"SELECT COUNT(*) FROM dt2 WHERE d = 0.1e0",
		"DROP TABLE dt2",
		"SELECT COUNT(*) FROM dec_tbl WHERE d = 123.451",
		"SELECT COUNT(*) FROM dec_tbl WHERE d < 123.451",
		"SELECT COUNT(*) FROM dec_tbl WHERE d = '123.451'",
		"SELECT COUNT(*) FROM dec_tbl WHERE d BETWEEN 123.451 AND 124 OR d BETWEEN 123.449 AND 123.4500001",
		"CREATE TABLE dec_idx (id INT PRIMARY KEY, d DECIMAL(10,2), KEY (d))",
		"INSERT INTO dec_idx VALUES (1, 123.45), (2, 123.46), (3, 100.00), (4, NULL), (5, -5.50)",
		"CREATE TABLE dn (id INT PRIMARY KEY, d DECIMAL(10,2))",
		"INSERT INTO dn SELECT * FROM dec_idx",
		"SELECT id FROM dec_idx WHERE d = 123.45000001",
		"SELECT id FROM dec_idx WHERE d < 123.45000001 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d > 123.45000001 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d >= 123.455 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d > 123.445 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d <= 123.455 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d <> 123.451 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d <> 123.45 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d BETWEEN 100.001 AND 123.455 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d >= -5.505 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d > -5.505 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d < -5.495 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d = '123.451'",
		"SELECT id FROM dec_idx WHERE d < '123.451' ORDER BY id",
		"SELECT id FROM dec_idx WHERE d < 123.45e0 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d = 123.45e0 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d = 123 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d < 99999999999 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d > -99999999999 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d < 1e20 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d > 99999999999 ORDER BY id",
		"SELECT id FROM dec_idx WHERE d = 99999999999 ORDER BY id",
		"SELECT id, d = 123.45000000000000000000000000001, d < 123.450000000000000000000000000001 FROM dec_idx ORDER BY id",
		"SELECT id FROM dec_idx WHERE d IN (123.451, 100.00) ORDER BY id",
		"SELECT id FROM dec_idx WHERE d IN ('123.451', '100') ORDER BY id",
		"SELECT id FROM dn WHERE d IN ('123.451', '100') ORDER BY id",
		"SELECT id FROM dn WHERE d IN (123.5, 1) ORDER BY id",
		"SELECT id FROM dec_idx WHERE d IN (123.5, 1) ORDER BY id",
		"SELECT id FROM dn WHERE d IN (123.451e0, 100e0) ORDER BY id",
		"SELECT id FROM dn WHERE d IN (123.45e0, 1) ORDER BY id",
		"SELECT id FROM dn WHERE d IN (123, 100, 99999999999) ORDER BY id",
		"SELECT id FROM dn WHERE d NOT IN ('123.451', '100') ORDER BY id",
		"SELECT id FROM dn WHERE d IN (123.450, 7) ORDER BY id",
		"SELECT id, d IN ('123.451', '100'), d IN (123.5, 1), d IN (123.45, 1.5, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11), d IN (123.4500, NULL) FROM dn ORDER BY id",
		wpSQLMode,
		// WordPress の meta_query（type が DECIMAL の CAST と文字列の値）と、WooCommerce の価格の絞り込み（wc_product_meta_lookup の索引）
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_status VARCHAR(20) NOT NULL DEFAULT 'publish', KEY type_status (post_type, post_status, ID)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_posts (post_type) VALUES ('post'), ('post'), ('post'), ('post'), ('product')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'price', '10.5'), (2, 'price', '10.49'), (3, 'price', '9.995'), (4, 'price', '11'), (5, 'price', '10.495')",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) >= '10.5' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) >= '10.495' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) > '10.495' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) = '9.995' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) = '10.5' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) BETWEEN '10.495' AND '10.505' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) NOT BETWEEN '10.495' AND '10.505' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) IN ('10.495','11') ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL) >= '10.5' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS SIGNED) >= '10.5' ) ) AND wp_posts.post_type = 'post' GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC",
		"SELECT wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE wp_postmeta.meta_key = 'price' AND wp_postmeta.meta_value+0 >= 10.495 ORDER BY wp_posts.ID ASC",
		"SELECT wp_postmeta.post_id, CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) AS p FROM wp_postmeta WHERE meta_key = 'price' ORDER BY p, post_id",
		"SELECT CAST('10.5' AS DECIMAL(10,2)) = 10.501, CAST('10.5' AS DECIMAL(10,2)) = '10.501', 10.50 = 10.501, 10.50 = '10.501'",
		"CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, onsale TINYINT(1) DEFAULT 0, stock_status VARCHAR(100) NULL DEFAULT 'instock', PRIMARY KEY (product_id), KEY min_max_price (min_price, max_price), KEY onsale (onsale), KEY stock_status (stock_status))",
		"INSERT INTO wp_wc_product_meta_lookup VALUES (1, 10.0000, 10.0000, 0, 'instock'), (2, 19.9999, 25.0000, 1, 'instock'), (3, 9.9950, 9.9950, 0, 'instock'), (4, NULL, NULL, 0, 'outofstock'), (5, 25.0000, 30.0000, 1, 'instock')",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 10.000000 AND max_price <= 25.000000 ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.99501 AND max_price <= 19.99985 ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.9950001 ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE max_price > 19.99985 AND stock_status = 'instock' ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE max_price >= 10 AND min_price <= 25 ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE max_price >= '9.99' AND min_price <= '25.00001' ORDER BY product_id",
		"SELECT product_id FROM wp_wc_product_meta_lookup WHERE NOT (max_price < 10.00001 OR min_price > 24.99999) ORDER BY product_id",
		"SELECT p.ID FROM wp_posts p JOIN wp_wc_product_meta_lookup l ON l.product_id = p.ID WHERE l.min_price >= 9.99501 AND l.max_price <= 30 ORDER BY p.ID",
		"SELECT MIN(min_price), MAX(max_price) FROM wp_wc_product_meta_lookup WHERE min_price > 9.995 AND onsale = 1",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_wc_product_meta_lookup",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE dn",
		"DROP TABLE dec_idx",
		"DROP TABLE dec_tbl",
		"DROP TABLE u2",
		"DROP TABLE u1",
		"DROP TABLE t2",
		"DROP TABLE t1",
	}},
	{0, []string{ // 前からの計画の違い: 並べ替えのある IN (SELECT ... ORDER BY ...) のマージ結合が行を落とす、IN (SELECT ... WHERE FALSE) の vertex not found
		"CREATE TABLE a (id INT PRIMARY KEY, v VARCHAR(10))",
		"CREATE TABLE b (id INT PRIMARY KEY, aid INT, x INT, KEY (aid), KEY (x))",
		"INSERT INTO a VALUES (1, 'a1'), (2, 'a2'), (3, 'a3'), (4, 'a4'), (5, 'a5')",
		"INSERT INTO b VALUES (1, 5, 1), (2, 1, 2), (3, 4, 3), (4, 2, 4), (5, 3, 5)",
		"SELECT id FROM a WHERE id IN (SELECT aid FROM b ORDER BY x) ORDER BY id",
		"SELECT id FROM a WHERE id IN (SELECT aid FROM b ORDER BY x DESC) ORDER BY id",
		"SELECT id FROM a WHERE id IN (SELECT DISTINCT aid FROM b ORDER BY aid DESC) ORDER BY id",
		"SELECT id FROM a WHERE id NOT IN (SELECT aid FROM b WHERE x > 2 ORDER BY x) ORDER BY id",
		"SELECT id FROM a WHERE EXISTS (SELECT 1 FROM b WHERE b.aid = a.id ORDER BY x) ORDER BY id",
		"SELECT id FROM a WHERE id = (SELECT aid FROM b ORDER BY x LIMIT 1)",
		"SELECT id FROM a WHERE id <> (SELECT aid FROM b ORDER BY x DESC LIMIT 1) ORDER BY id",
		"SELECT a.id FROM a JOIN (SELECT aid FROM b ORDER BY x DESC) s ON s.aid = a.id ORDER BY a.id",
		"CREATE TABLE ab (a INT PRIMARY KEY, b INT)",
		"CREATE TABLE xy (x INT PRIMARY KEY, y INT)",
		"INSERT INTO ab VALUES (1, 1), (2, 2)",
		"INSERT INTO xy VALUES (1, 1), (2, 3)",
		"SELECT * FROM xy WHERE y IN (SELECT b FROM ab WHERE FALSE)",
		"SELECT * FROM xy WHERE y NOT IN (SELECT b FROM ab WHERE FALSE) ORDER BY x",
		"SELECT * FROM xy WHERE y IN (SELECT b FROM ab WHERE 1 = 0)",
		"SELECT * FROM xy WHERE EXISTS (SELECT 1 FROM ab WHERE ab.b = xy.y AND FALSE)",
		"SELECT * FROM xy WHERE NOT EXISTS (SELECT 1 FROM ab WHERE ab.b = xy.y AND 1 = 0) ORDER BY x",
		"SELECT * FROM xy WHERE x IN (SELECT a FROM ab WHERE FALSE) OR y = 3",
		"UPDATE xy SET y = y + 10 WHERE y IN (SELECT b FROM ab WHERE FALSE)",
		"DELETE FROM xy WHERE y IN (SELECT b FROM ab WHERE 1 = 0)",
		"SELECT * FROM xy ORDER BY x",
		wpSQLMode,
		// WordPress の形: メタの値の順に並べた副問い合わせの IN と EXISTS（プラグインの並べ替え）、空になる副問い合わせ
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_status VARCHAR(20) NOT NULL DEFAULT 'publish', PRIMARY KEY (ID), KEY type_status (post_type, post_status, ID)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, PRIMARY KEY (meta_id), KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_posts (post_type) VALUES ('post'), ('post'), ('post'), ('post'), ('post')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (5, 'rank', '1'), (1, 'rank', '2'), (4, 'rank', '3'), (2, 'rank', '4'), (3, 'rank', '5')",
		"SELECT ID FROM wp_posts WHERE ID IN (SELECT post_id FROM wp_postmeta WHERE meta_key = 'rank' ORDER BY meta_value) ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE ID IN (SELECT post_id FROM wp_postmeta ORDER BY meta_id DESC) ORDER BY ID",
		"SELECT ID FROM wp_posts p WHERE EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = p.ID ORDER BY m.meta_value) ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT post_id FROM wp_postmeta WHERE meta_key = 'rank' AND meta_value > '2' ORDER BY meta_value) ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE post_type = 'post' AND ID IN (SELECT post_id FROM wp_postmeta WHERE FALSE) ORDER BY ID",
		"SELECT ID FROM wp_posts WHERE ID NOT IN (SELECT post_id FROM wp_postmeta WHERE 1 = 0) ORDER BY ID",
		"SELECT ID FROM wp_posts p WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta m WHERE m.post_id = p.ID AND FALSE) ORDER BY ID",
		"SELECT p.ID FROM wp_posts p JOIN (SELECT post_id FROM wp_postmeta ORDER BY meta_value DESC) m ON m.post_id = p.ID ORDER BY p.ID",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE xy",
		"DROP TABLE ab",
		"DROP TABLE b",
		"DROP TABLE a",
	}},
	{3073, []string{ // 名前の解決: GROUP BY と HAVING の SELECT の別名（#3073）、表で修飾したシステム変数の名前（#3260）、HAVING だけの GROUP_CONCAT
		"CREATE TABLE xy (x INT PRIMARY KEY, y INT, UNIQUE INDEX y_idx (y))",
		"INSERT INTO xy VALUES (1, 0), (2, 1), (0, 2), (3, 3)",
		"SELECT y AS z FROM xy GROUP BY (y) HAVING AVG(z) > 0 ORDER BY z",
		"SELECT y AS z FROM xy GROUP BY (z) HAVING AVG(z) > 0 ORDER BY z",
		"SELECT y + 1 AS z FROM xy GROUP BY (z) HAVING AVG(z) > 1 ORDER BY z",
		"SELECT y AS z FROM xy GROUP BY z HAVING z > 0 ORDER BY z",
		"SELECT y AS z, COUNT(*) FROM xy GROUP BY z HAVING SUM(z) > 0 ORDER BY z",
		"SELECT y AS x FROM xy GROUP BY (y) HAVING AVG(x) > 0 ORDER BY x",
		"SELECT x FROM xy WHERE x IN (SELECT y AS w FROM xy GROUP BY w HAVING AVG(w) > 0) ORDER BY x",
		"SELECT y AS w, (SELECT COUNT(*) FROM xy i GROUP BY i.y HAVING MAX(i.y) = w LIMIT 1) AS c FROM xy ORDER BY w",
		"CREATE TABLE A (id INT)",
		"CREATE TABLE B (id INT)",
		"INSERT INTO A VALUES (1)",
		"INSERT INTO B VALUES (2)",
		"SELECT A.timestamp FROM A",
		"SELECT A.version FROM A",
		"SELECT UPPER(A.sql_mode) FROM A",
		"SELECT UNIX_TIMESTAMP(A.timestamp) FROM A LIMIT 1",
		"SELECT A.max_connections FROM A",
		"SELECT a1.timestamp FROM A a1 JOIN B b1 ON a1.id = b1.id",
		"SELECT A.id, A.nosuchcolumn FROM A",
		"SELECT id FROM A WHERE nosuchcolumn = 1",
		"SELECT @@session.sql_mode = @@sql_mode, @@global.max_connections > 0, @@SESSION.autocommit, @@session.time_zone",
		wpSQLMode,
		// WordPress の形: メタのキーをまとめた結果で絞る（HAVING だけの GROUP_CONCAT）と、別名の HAVING
		"CREATE TABLE wp_posts (ID BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_title TEXT NOT NULL) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_posts (post_title) VALUES ('a'), ('b'), ('c')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'foo', '1'), (1, 'bar', '2'), (2, 'baz', '3'), (3, 'foo', '4')",
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY p.ID HAVING GROUP_CONCAT(m.meta_key) LIKE '%foo%' ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY p.ID HAVING GROUP_CONCAT(m.meta_key ORDER BY m.meta_key SEPARATOR '|') = 'bar|foo'",
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY p.ID HAVING GROUP_CONCAT(DISTINCT m.meta_key) NOT LIKE '%baz%' ORDER BY p.ID",
		"SELECT p.ID, GROUP_CONCAT(m.meta_key) FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY p.ID HAVING GROUP_CONCAT(m.meta_key) LIKE '%foo%' ORDER BY p.ID",
		"SELECT p.ID, GROUP_CONCAT(m.meta_key) AS keys_ FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY p.ID HAVING keys_ LIKE '%foo%' ORDER BY p.ID",
		"SELECT p.ID FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY p.ID HAVING COUNT(m.meta_id) > 1 ORDER BY p.ID",
		"SELECT p.ID AS pid, COUNT(m.meta_id) AS n FROM wp_posts p JOIN wp_postmeta m ON m.post_id = p.ID GROUP BY pid HAVING n >= 1 AND MAX(pid) > 1 ORDER BY pid",
		"SELECT m.meta_key AS k, COUNT(*) FROM wp_postmeta m GROUP BY k HAVING COUNT(k) > 1 ORDER BY k",
		"SELECT wp_posts.post_title FROM wp_posts",
		"SELECT wp_posts.timestamp FROM wp_posts",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wp_posts",
		"DROP TABLE B",
		"DROP TABLE A",
		"DROP TABLE xy",
	}},
	{3052, []string{ // UPDATE と DELETE: 主キーのない表の副問い合わせのある UPDATE（#3052）、UPDATE JOIN の表ごとの ON UPDATE、結合から主キーのない表を消す DELETE
		"CREATE TABLE t (i INT)",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET i = 10 WHERE i IN (SELECT 1)",
		"SELECT * FROM t",
		"UPDATE t SET i = 11 WHERE i IN (SELECT i FROM (SELECT 10 AS i) s)",
		"SELECT * FROM t",
		"CREATE TABLE tk (i INT, v INT)",
		"INSERT INTO tk VALUES (1, 1), (1, 1), (2, 2), (3, 3)",
		"CREATE TABLE s (k INT PRIMARY KEY)",
		"INSERT INTO s VALUES (1), (3)",
		"UPDATE tk SET v = v + 10 WHERE i IN (SELECT k FROM s)",
		"SELECT * FROM tk ORDER BY i, v",
		"UPDATE tk SET v = 0 WHERE EXISTS (SELECT 1 FROM s WHERE s.k = tk.i)",
		"SELECT * FROM tk ORDER BY i, v",
		"UPDATE tk SET v = 5 WHERE i NOT IN (SELECT k FROM s)",
		"SELECT * FROM tk ORDER BY i, v",
		"DELETE tk FROM tk JOIN s ON s.k = tk.i",
		"SELECT * FROM tk ORDER BY i, v",
		// UPDATE JOIN の ON UPDATE の列（同じ名前の列 u と、違う名前の列）
		"CREATE TABLE j1 (id INT PRIMARY KEY, v INT, u TIMESTAMP NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"CREATE TABLE j2 (id INT PRIMARY KEY, v INT, u TIMESTAMP NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO j1 VALUES (1, 1, '2000-01-01 00:00:00'), (2, 2, '2000-01-01 00:00:00')",
		"INSERT INTO j2 VALUES (1, 1, '2000-01-01 00:00:00'), (2, 2, '2000-01-01 00:00:00')",
		"UPDATE j1 JOIN j2 ON j1.id = j2.id SET j2.v = 2 WHERE j1.id = 1",
		"SELECT 'j1', id, v, u > '2001-01-01' FROM j1 UNION ALL SELECT 'j2', id, v, u > '2001-01-01' FROM j2 ORDER BY 1, 2",
		"UPDATE j1, j2 SET j2.v = 5 WHERE j1.id = j2.id AND j1.id = 1",
		"SELECT 'j1', id, v, u > '2001-01-01' FROM j1 UNION ALL SELECT 'j2', id, v, u > '2001-01-01' FROM j2 ORDER BY 1, 2",
		"UPDATE j2 JOIN j1 ON j1.id = j2.id SET j2.v = 6 WHERE j2.id = 1",
		"SELECT 'j1', id, v, u > '2001-01-01' FROM j1 UNION ALL SELECT 'j2', id, v, u > '2001-01-01' FROM j2 ORDER BY 1, 2",
		"UPDATE j1 JOIN j2 ON j1.id = j2.id SET j1.v = 1, j2.v = 3 WHERE j1.id = 2",
		"SELECT 'j1', id, v, u > '2001-01-01' FROM j1 UNION ALL SELECT 'j2', id, v, u > '2001-01-01' FROM j2 ORDER BY 1, 2",
		"UPDATE j1 JOIN j2 ON j1.id = j2.id SET j1.v = j1.v, j2.v = 30 WHERE j1.id = 2",
		"SELECT 'j1', id, v, u > '2001-01-01' FROM j1 UNION ALL SELECT 'j2', id, v, u > '2001-01-01' FROM j2 ORDER BY 1, 2",
		"UPDATE j1 SET v = 9 WHERE id IN (SELECT id FROM j2 WHERE v = 30)",
		"SELECT 'j1', id, v, u > '2001-01-01' FROM j1 UNION ALL SELECT 'j2', id, v, u > '2001-01-01' FROM j2 ORDER BY 1, 2",
		"CREATE TABLE k1 (id INT PRIMARY KEY, v INT, u1 TIMESTAMP NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"CREATE TABLE k2 (id INT PRIMARY KEY, w INT, u2 TIMESTAMP NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO k1 VALUES (1, 1, '2000-01-01 00:00:00')",
		"INSERT INTO k2 VALUES (1, 1, '2000-01-01 00:00:00')",
		"UPDATE k1 JOIN k2 ON k1.id = k2.id SET k2.w = 2",
		"SELECT 'k1', id, v, u1 > '2001-01-01' FROM k1 UNION ALL SELECT 'k2', id, w, u2 > '2001-01-01' FROM k2 ORDER BY 1",
		// UNION の ORDER BY の番号で定数の列を指す（前からの違い。上の SELECT の並び）
		"SELECT 'j1' AS k, id FROM j1 UNION SELECT 'j2', id FROM j2 ORDER BY 1 DESC, 2",
		"SELECT 'a', 'x', id FROM j1 UNION ALL SELECT 'b', 'y', id FROM j2 ORDER BY 2 DESC, 3",
		// 結合から主キーのない表を消す（同じ行が何度も結び付く、同じ値の行）
		"CREATE TABLE l (id INT PRIMARY KEY)",
		"INSERT INTO l VALUES (1), (2)",
		"CREATE TABLE r2 (lid INT, v INT)",
		"INSERT INTO r2 VALUES (1, 10), (NULL, NULL), (3, 30), (3, 30)",
		"DELETE r2 FROM l JOIN r2 ON TRUE",
		"SELECT * FROM r2",
		"INSERT INTO r2 VALUES (1, 10), (1, 10), (2, 20), (3, 30)",
		"DELETE r2 FROM l JOIN r2 ON l.id <= r2.lid",
		"SELECT * FROM r2 ORDER BY lid",
		"INSERT INTO r2 VALUES (1, 10), (2, 20), (2, 20)",
		"DELETE l, r2 FROM l JOIN r2 ON l.id = r2.lid WHERE r2.v = 20",
		"SELECT * FROM l ORDER BY id",
		"SELECT * FROM r2 ORDER BY lid",
		"CREATE TABLE r3 (id INT PRIMARY KEY, lid INT, v INT)",
		"INSERT INTO r3 VALUES (1, 1, 10), (2, NULL, NULL), (3, 3, 30)",
		"DELETE r3 FROM l JOIN r3 ON TRUE",
		"SELECT * FROM r3",
		"INSERT INTO r3 VALUES (1, 1, 10), (2, NULL, NULL), (3, 3, 30)",
		"DELETE r3 FROM l LEFT JOIN r3 ON l.id = r3.lid OR r3.lid IS NULL",
		"SELECT * FROM r3 ORDER BY id",
		wpSQLMode,
		// プラグインの表の形: updated_at の ON UPDATE を持つ 2 つの表の UPDATE JOIN と、主キーのない記録の表
		"CREATE TABLE wp_plugin_orders (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, status VARCHAR(20) NOT NULL DEFAULT 'new', updated_at DATETIME NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP)",
		"CREATE TABLE wp_plugin_items (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, order_id BIGINT UNSIGNED NOT NULL, qty INT NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT '2000-01-01 00:00:00' ON UPDATE CURRENT_TIMESTAMP, KEY order_id (order_id))",
		"CREATE TABLE wp_plugin_log (order_id BIGINT UNSIGNED NOT NULL, note VARCHAR(20))",
		"INSERT INTO wp_plugin_orders (status) VALUES ('new'), ('new')",
		"INSERT INTO wp_plugin_items (order_id, qty) VALUES (1, 1), (1, 2), (2, 1)",
		"INSERT INTO wp_plugin_log VALUES (1, 'a'), (1, 'a'), (2, 'b')",
		"UPDATE wp_plugin_orders o JOIN wp_plugin_items i ON i.order_id = o.id SET i.qty = i.qty + 1 WHERE o.id = 1",
		"SELECT 'o', id, updated_at > '2001-01-01' FROM wp_plugin_orders UNION ALL SELECT 'i', id, updated_at > '2001-01-01' FROM wp_plugin_items ORDER BY 1, 2",
		"UPDATE wp_plugin_orders o JOIN wp_plugin_items i ON i.order_id = o.id SET o.status = 'done' WHERE o.id = 2",
		"SELECT 'o', id, status, updated_at > '2001-01-01' FROM wp_plugin_orders UNION ALL SELECT 'i', id, qty, updated_at > '2001-01-01' FROM wp_plugin_items ORDER BY 1, 2",
		"UPDATE wp_plugin_log SET note = 'x' WHERE order_id IN (SELECT id FROM wp_plugin_orders WHERE status = 'new')",
		"SELECT * FROM wp_plugin_log ORDER BY order_id, note",
		"DELETE g FROM wp_plugin_log g JOIN wp_plugin_items i ON i.order_id = g.order_id WHERE i.order_id = 1",
		"SELECT * FROM wp_plugin_log ORDER BY order_id, note",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_plugin_log",
		"DROP TABLE wp_plugin_items",
		"DROP TABLE wp_plugin_orders",
		"DROP TABLE r3",
		"DROP TABLE r2",
		"DROP TABLE l",
		"DROP TABLE k2",
		"DROP TABLE k1",
		"DROP TABLE j2",
		"DROP TABLE j1",
		"DROP TABLE s",
		"DROP TABLE tk",
		"DROP TABLE t",
	}},
	{41151, []string{ // レビュー 1: lookup 結合と IN の副問い合わせで、DECIMAL の列の小数の桁に丸められるキーはどの行にも当たらない
		"CREATE TABLE lk (product_id INT PRIMARY KEY, min_price DECIMAL(19,4), KEY (min_price))",
		"INSERT INTO lk VALUES (1, 10.0000), (2, 9.9950), (3, 20.0000), (4, NULL)",
		"CREATE TABLE pm (meta_id INT PRIMARY KEY, post_id INT, meta_value LONGTEXT)",
		"INSERT INTO pm VALUES (1, 1, '10.00001'), (2, 2, '9.995'), (3, 3, '20'), (4, 4, '9.99501'), (5, 5, NULL), (6, 6, '10.0000000000000000001'), (7, 7, ' 20 ')",
		"SELECT lk.product_id, pm.post_id FROM pm JOIN lk ON lk.min_price = pm.meta_value ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(pm, lk) */ lk.product_id, pm.post_id FROM pm JOIN lk ON lk.min_price = pm.meta_value ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(pm, lk) */ pm.post_id, lk.product_id FROM pm LEFT JOIN lk ON lk.min_price = pm.meta_value ORDER BY 1, 2",
		"SELECT product_id FROM lk WHERE min_price IN (SELECT meta_value FROM pm) ORDER BY 1",
		"SELECT product_id FROM lk WHERE EXISTS (SELECT 1 FROM pm WHERE pm.meta_value = lk.min_price) ORDER BY 1",
		"CREATE TABLE dd (id INT PRIMARY KEY, d DECIMAL(10,2), KEY d_idx (d))",
		"INSERT INTO dd VALUES (1, 123.45), (2, -0.01), (3, NULL), (5, 0.00), (6, 123.44), (10, 10.50)",
		"CREATE TABLE d2 (id INT PRIMARY KEY, e DECIMAL(12,4), f DOUBLE, KEY e_idx (e))",
		"INSERT INTO d2 VALUES (1, 123.45, 123.45), (2, 123.4500, 123.4501), (3, 10.5, 10.5), (4, 123.4501, 123.45), (5, NULL, NULL), (6, 0.0001, 0)",
		"SELECT dd.id, d2.id FROM dd JOIN d2 ON dd.d = d2.e ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(d2, dd) */ dd.id, d2.id FROM d2 JOIN dd ON dd.d = d2.e ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(dd, d2) */ dd.id, d2.id FROM dd JOIN d2 ON dd.d = d2.e ORDER BY 1, 2",
		"SELECT /*+ HASH_JOIN(dd, d2) */ dd.id, d2.id FROM dd JOIN d2 ON dd.d = d2.e ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(d2, dd) */ dd.id, d2.id FROM d2 JOIN dd ON dd.d = d2.f ORDER BY 1, 2",
		"SELECT dd.id FROM dd WHERE d IN (SELECT e FROM d2) ORDER BY 1",
		"SELECT dd.id FROM dd WHERE d IN (SELECT f FROM d2) ORDER BY 1",
		"SELECT dd.id FROM dd WHERE EXISTS (SELECT 1 FROM d2 WHERE d2.e = dd.d) ORDER BY 1",
		"SELECT dd.id, d2.id FROM dd LEFT JOIN d2 ON dd.d = d2.e ORDER BY 1, 2",
		"SELECT d2.id, dd.id FROM d2 LEFT JOIN dd ON dd.d = d2.e ORDER BY 1, 2",
		"SELECT dd.id, d2.id FROM dd JOIN d2 ON dd.d <=> d2.e ORDER BY 1, 2",
		wpSQLMode,
		// WooCommerce の形: 商品の価格の表とメタの値の結合
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL PRIMARY KEY, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, KEY min_max_price (min_price, max_price))",
		"INSERT INTO wp_wc_product_meta_lookup VALUES (1, 10.0000, 10.0000), (2, 9.9950, 12.5000), (3, 20.0000, 20.0000)",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_price', '10.00001'), (2, '_price', '9.995'), (3, '_price', '20'), (4, '_price', '9.99501')",
		"SELECT l.product_id, m.post_id FROM wp_postmeta m JOIN wp_wc_product_meta_lookup l ON l.min_price = m.meta_value WHERE m.meta_key = '_price' ORDER BY 1, 2",
		"SELECT l.product_id FROM wp_wc_product_meta_lookup l WHERE l.min_price IN (SELECT meta_value FROM wp_postmeta WHERE meta_key = '_price') ORDER BY 1",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_wc_product_meta_lookup",
		"DROP TABLE wp_postmeta",
		"DROP TABLE d2",
		"DROP TABLE dd",
		"DROP TABLE pm",
		"DROP TABLE lk",
	}},
	{41152, []string{ // レビュー 2: DECIMAL と定数でない文字列（列、CONCAT）は DOUBLE で、定数の文字列は DECIMAL で比べる
		"CREATE TABLE dd (id INT PRIMARY KEY, d DECIMAL(10,2), n DECIMAL(10,2), s VARCHAR(32), KEY d_idx (d))",
		"INSERT INTO dd VALUES (1, 123.45, 123.45, '123.45'), (2, -0.01, -0.01, '-0.01'), (3, NULL, NULL, NULL), (4, 99999999.99, 99999999.99, '99999999.99'), (5, 0.00, 0.00, '0'), (6, 123.44, 123.44, '123.44'), (7, 10.00, 10.00, '10'), (8, 10.50, 10.50, '10.5'), (9, 1.50, 1.50, '1.50000000000000000001')",
		"SELECT id FROM dd WHERE n = CONCAT(s, '0000000000000001') ORDER BY id",
		"SELECT id FROM dd WHERE d = CONCAT(s, '0000000000000001') ORDER BY id",
		"SELECT id FROM dd WHERE n = s ORDER BY id",
		"SELECT id FROM dd WHERE d = s ORDER BY id",
		"SELECT id FROM dd WHERE n > s ORDER BY id",
		"SELECT id FROM dd WHERE n IN (s, '7') ORDER BY id",
		"SELECT id FROM dd WHERE n IN (CONCAT(s, '0000000000000001')) ORDER BY id",
		"SELECT id, n = s, n < s, n = CONCAT(s, '1'), n = '1.50000000000000000001', n = CONCAT('1.5', '0000000000000000001') FROM dd ORDER BY id",
		"SELECT id FROM dd WHERE n = '1.50000000000000000001' ORDER BY id",
		"SELECT id FROM dd WHERE n = CONCAT('1.5', '0000000000000000001') ORDER BY id",
		"SELECT id FROM dd WHERE CAST(s AS DECIMAL(10,2)) >= '10.495' ORDER BY id",
		"SELECT id FROM dd WHERE CAST(s AS DECIMAL(10,2)) = '10.5' ORDER BY id",
		"SELECT id FROM dd WHERE CAST(s AS DECIMAL(10,2)) = s ORDER BY id",
		"SELECT id FROM dd WHERE CAST(s AS DECIMAL(10,2)) IN ('10.495', '10.5') ORDER BY id",
		// DECIMAL の列と文字列の列の NOT IN（半結合の比べも DOUBLE）
		"CREATE TABLE lk (product_id INT PRIMARY KEY, min_price DECIMAL(19,4), KEY (min_price))",
		"INSERT INTO lk VALUES (1, 10.0000), (2, 9.9950), (3, 20.0000), (4, NULL)",
		"CREATE TABLE pm (meta_id INT PRIMARY KEY, post_id INT, meta_value LONGTEXT)",
		"INSERT INTO pm VALUES (1, 1, '10.0000000000000000001'), (2, 2, '9.99501')",
		"SELECT product_id FROM lk WHERE min_price NOT IN (SELECT meta_value FROM pm WHERE meta_value IS NOT NULL) ORDER BY 1",
		"SELECT product_id FROM lk WHERE min_price IN (SELECT meta_value FROM pm) ORDER BY 1",
		"SELECT lk.product_id, pm.post_id FROM pm JOIN lk ON lk.min_price = pm.meta_value ORDER BY 1, 2",
		"DROP TABLE pm",
		"DROP TABLE lk",
		wpSQLMode,
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'price', '9.995'), (2, 'price', '10'), (3, 'price', '10.495'), (4, 'price', '10.50'), (5, 'price', 'abc'), (6, 'price', '10.0000000000000000001')",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) >= '10.495' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) = '10' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) BETWEEN '10' AND '10.495' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) = meta_value ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,4)) = meta_value ORDER BY post_id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE dd",
	}},
	{41153, []string{ // レビュー 3: HAVING の集計と GROUP_CONCAT の引数の式の中の SELECT の別名、集計の中の集計（1111）
		"CREATE TABLE xy (x INT PRIMARY KEY, y INT, z INT)",
		"INSERT INTO xy VALUES (0, 0, 5), (1, 1, 6), (2, 1, 7), (3, 2, 8)",
		"SELECT y AS q FROM xy GROUP BY q HAVING AVG(q + 1) > 1 ORDER BY q",
		"SELECT y AS q FROM xy GROUP BY q HAVING SUM(IF(q > 0, 1, 0)) > 1 ORDER BY q",
		"SELECT y AS q FROM xy GROUP BY q HAVING GROUP_CONCAT(q) IS NOT NULL ORDER BY q",
		"SELECT y AS q FROM xy GROUP BY q HAVING GROUP_CONCAT(q, '-', x ORDER BY x) LIKE '1-%' ORDER BY q",
		"SELECT y AS q FROM xy GROUP BY y HAVING MAX(q * 10 + x) > 20 ORDER BY q",
		"SELECT y AS z FROM xy GROUP BY y HAVING SUM(z + 0) > 10 ORDER BY z",
		"SELECT COUNT(*) AS cnt FROM xy GROUP BY y HAVING COUNT(cnt) > 1",
		"SELECT y, SUM(z) AS s FROM xy GROUP BY y HAVING SUM(s) > 10 ORDER BY s",
		"SELECT y AS q, SUM(q * 2) FROM xy GROUP BY q ORDER BY q",
		"SELECT o.x FROM xy o WHERE o.y IN (SELECT y AS w FROM xy GROUP BY w HAVING SUM(w + z) > 10) ORDER BY o.x",
		wpSQLMode,
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'foo', 'a'), (1, 'bar', 'b'), (2, 'foo', 'c'), (3, 'baz', 'd'), (3, 'bar', 'e')",
		"SELECT post_id AS pid FROM wp_postmeta GROUP BY pid HAVING SUM(IF(pid > 1, 1, 0)) >= 2 ORDER BY pid",
		"SELECT meta_key AS k FROM wp_postmeta GROUP BY k HAVING GROUP_CONCAT(CONCAT(k, ':', meta_value) ORDER BY meta_value) LIKE '%:b%' ORDER BY k",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE xy",
	}},
	{41154, []string{ // 2 度目のレビュー 1: HAVING の副問い合わせの集計の中の外の別名、副問い合わせの SELECT の別名（1054）、あいまいな別名（1052）、GROUP_CONCAT の ORDER BY の別名
		"CREATE TABLE xy (x INT PRIMARY KEY, y INT, z INT)",
		"INSERT INTO xy VALUES (0, 0, 1), (1, 0, 2), (2, 1, 3), (3, 1, 4), (4, 2, 5)",
		"CREATE TABLE xy2 (x INT PRIMARY KEY, y INT, q INT)",
		"INSERT INTO xy2 VALUES (0, 0, 7), (1, 0, 8), (2, 1, 9)",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT MAX(q + 1) FROM xy z) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT SUM(q) FROM xy z) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT SUM(q) FROM xy z WHERE z.x < 2) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT COUNT(q) FROM xy z) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT SUM(q + z.x) FROM xy z) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT GROUP_CONCAT(q) FROM xy z) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT SUM(z.x) FROM xy z GROUP BY z.y HAVING SUM(q) > 0 ORDER BY z.y LIMIT 1) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING EXISTS (SELECT 1 FROM xy2 WHERE xy2.y = q GROUP BY xy2.y HAVING COUNT(*) > 1) ORDER BY 1",
		"SELECT y AS w FROM xy GROUP BY y HAVING (SELECT COUNT(w) FROM xy2) > 2 ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT MAX(s) FROM (SELECT q AS r, SUM(r) AS s FROM xy2 GROUP BY q) d) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT MAX(s) FROM (SELECT q AS r, GROUP_CONCAT(r) AS s FROM xy2 GROUP BY q) d) ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING SUM(x) > (SELECT y AS r, SUM(r) FROM xy2 GROUP BY y LIMIT 1) ORDER BY 1",
		"SELECT y AS q, z AS Q FROM xy GROUP BY y, z HAVING SUM(q) > 0 ORDER BY 1, 2",
		"SELECT y AS q, z AS q FROM xy GROUP BY y, z HAVING SUM(q) > 0 ORDER BY 1, 2",
		"SELECT y AS q, z AS Q FROM xy GROUP BY y, z HAVING q > 0 ORDER BY 1, 2",
		"SELECT y AS q, z AS Q FROM xy GROUP BY y, z HAVING GROUP_CONCAT(q) <> '' ORDER BY 1, 2",
		"SELECT y AS q, y AS q FROM xy GROUP BY y HAVING SUM(q) > 0 ORDER BY 1",
		"SELECT y AS q, z AS Q FROM xy GROUP BY y, z HAVING SUM(x) > 0 ORDER BY 1, 2",
		"SELECT y + 0 AS q, z + 0 AS Q FROM xy GROUP BY y, z HAVING q > 0 ORDER BY 1, 2",
		"SELECT y + 0 AS q, z + 0 AS Q FROM xy GROUP BY y, z HAVING SUM(q) > 0 ORDER BY 1, 2",
		"SELECT y AS q, GROUP_CONCAT(x ORDER BY x) AS g FROM xy GROUP BY y HAVING GROUP_CONCAT(x ORDER BY q) <> '' ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING GROUP_CONCAT(x ORDER BY q DESC, x DESC SEPARATOR '') <> '' ORDER BY 1",
		"SELECT y AS q, GROUP_CONCAT(x ORDER BY x DESC) AS g FROM xy GROUP BY y HAVING GROUP_CONCAT(x ORDER BY q, x DESC) = '1,0' ORDER BY 1",
		"SELECT y AS q FROM xy GROUP BY y HAVING GROUP_CONCAT(x ORDER BY q + x DESC) = '1,0' ORDER BY 1",
		"SELECT y AS q, COUNT(*) AS cnt FROM xy GROUP BY y HAVING GROUP_CONCAT(x ORDER BY cnt) <> '' ORDER BY 1",
		"SELECT y AS q, COUNT(*) AS cnt FROM xy GROUP BY y ORDER BY SUM(cnt)",
		"SELECT y AS q, SUM(q) FROM xy GROUP BY y",
		wpSQLMode,
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'foo', 'a'), (1, 'bar', 'b'), (2, 'foo', 'c'), (3, 'baz', 'd'), (3, 'bar', 'e')",
		"SELECT post_id AS pid FROM wp_postmeta GROUP BY pid HAVING COUNT(*) > (SELECT COUNT(pid) FROM wp_postmeta m2 WHERE m2.meta_key = 'foo') - 1 ORDER BY pid",
		"SELECT post_id AS pid FROM wp_postmeta GROUP BY pid HAVING GROUP_CONCAT(meta_key ORDER BY pid, meta_key) LIKE 'bar%' ORDER BY pid",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE xy2",
		"DROP TABLE xy",
	}},
	{41155, []string{ // 2 度目のレビュー 2: DECIMAL の列が持てないキー（'1e30'、99999999999）は lookup 結合で 0.00 の行に当たらない
		"CREATE TABLE dk (id INT PRIMARY KEY, d DECIMAL(10,2), i INT, u TINYINT UNSIGNED, KEY(d), KEY(i), KEY(u))",
		"INSERT INTO dk VALUES (1, 0.00, 0, 0), (2, 10.50, 10, 10), (3, 99999999.99, 2147483647, 255)",
		"CREATE TABLE ks (id INT PRIMARY KEY, s VARCHAR(40))",
		"INSERT INTO ks VALUES (1, '99999999999'), (2, '1e30'), (3, '10.5'), (4, '-1e30'), (5, '0'), (6, '99999999.99')",
		"CREATE TABLE kb (id INT PRIMARY KEY, b BIGINT)",
		"INSERT INTO kb VALUES (1, 99999999999), (2, 10), (3, -99999999999), (4, 0)",
		"CREATE TABLE ke (id INT PRIMARY KEY, e DECIMAL(12,4))",
		"INSERT INTO ke VALUES (1, 99999999.9999), (2, 10.5000), (3, -99999999.9999), (4, 0.0000)",
		"SELECT /*+ LOOKUP_JOIN(ks, dk) JOIN_ORDER(ks, dk) */ ks.id, dk.id FROM ks JOIN dk ON dk.d = ks.s ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(ks, dk) JOIN_ORDER(ks, dk) */ ks.id, dk.id FROM ks LEFT JOIN dk ON dk.d = ks.s ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(kb, dk) JOIN_ORDER(kb, dk) */ kb.id, dk.id FROM kb JOIN dk ON dk.d = kb.b ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(kb, dk) JOIN_ORDER(kb, dk) */ kb.id, dk.id FROM kb LEFT JOIN dk ON dk.d = kb.b ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(ke, dk) JOIN_ORDER(ke, dk) */ ke.id, dk.id FROM ke JOIN dk ON dk.d = ke.e ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(ke, dk) JOIN_ORDER(ke, dk) */ ke.id, dk.id FROM ke LEFT JOIN dk ON dk.d = ke.e ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(ke, dk) JOIN_ORDER(ke, dk) */ ke.id, dk.id FROM ke LEFT JOIN dk ON dk.d <=> ke.e ORDER BY 1, 2",
		"SELECT id FROM dk WHERE d IN (SELECT s FROM ks) ORDER BY id",
		"SELECT id FROM dk WHERE d IN (SELECT b FROM kb) ORDER BY id",
		"SELECT id FROM dk WHERE d IN (SELECT e FROM ke) ORDER BY id",
		"SELECT id FROM dk WHERE EXISTS (SELECT 1 FROM ke WHERE ke.e = dk.d) ORDER BY id",
		"SELECT id FROM ks WHERE s IN (SELECT d FROM dk) ORDER BY id",
		"SELECT id FROM kb WHERE b IN (SELECT d FROM dk) ORDER BY id",
		"SELECT id FROM ke WHERE e IN (SELECT d FROM dk) ORDER BY id",
		"SELECT /*+ LOOKUP_JOIN(kb, dk) JOIN_ORDER(kb, dk) */ kb.id, dk.id FROM kb JOIN dk ON dk.i = kb.b ORDER BY 1, 2",
		"SELECT /*+ LOOKUP_JOIN(ks, dk) JOIN_ORDER(ks, dk) */ ks.id, dk.id FROM ks JOIN dk ON dk.u = ks.s ORDER BY 1, 2",
		wpSQLMode,
		"CREATE TABLE wc_product_meta_lookup (product_id BIGINT NOT NULL, min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, PRIMARY KEY (product_id), KEY min_max_price (min_price, max_price))",
		"INSERT INTO wc_product_meta_lookup VALUES (1, 0.0000, 0.0000), (2, 10.0000, 12.0000)",
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_price', '1e30'), (2, '_price', '10'), (3, '_price', '99999999999999999'), (4, '_price', '0')",
		"SELECT /*+ LOOKUP_JOIN(pm, lk) JOIN_ORDER(pm, lk) */ pm.meta_id, lk.product_id FROM wp_postmeta pm LEFT JOIN wc_product_meta_lookup lk ON lk.min_price = pm.meta_value ORDER BY 1, 2",
		"SELECT product_id FROM wc_product_meta_lookup WHERE min_price IN (SELECT meta_value FROM wp_postmeta WHERE meta_key = '_price') ORDER BY product_id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE wc_product_meta_lookup",
		"DROP TABLE ke",
		"DROP TABLE kb",
		"DROP TABLE ks",
		"DROP TABLE dk",
	}},
	{41156, []string{ // 2 度目のレビュー 3: DECIMAL と文字列は、2 つ以上の値の IN と BETWEEN と、定数どうしでは DOUBLE で比べる
		"CREATE TABLE cn (id INT PRIMARY KEY, n DECIMAL(10,2), s VARCHAR(40))",
		"INSERT INTO cn VALUES (1, 10.50, 'a'), (2, 10.50, 'b'), (3, 10.50, 'c'), (4, 0.30, 'd'), (5, 1.01, 'e'), (6, NULL, 'f'), (7, 10.50, 'g')",
		"SELECT id FROM cn WHERE n = '10.5000000000000000001' ORDER BY id",
		"SELECT id FROM cn WHERE n <> '10.5000000000000000001' ORDER BY id",
		"SELECT id FROM cn WHERE n IN ('10.5000000000000000001') ORDER BY id",
		"SELECT id FROM cn WHERE n NOT IN ('10.5000000000000000001') ORDER BY id",
		"SELECT id FROM cn WHERE n IN ('10.5000000000000000001', '0.3') ORDER BY id",
		"SELECT id FROM cn WHERE n NOT IN ('10.5000000000000000001', '0.3') ORDER BY id",
		"SELECT id FROM cn WHERE n IN ('10.5000000000000000001', 0.3) ORDER BY id",
		"SELECT id FROM cn WHERE n IN (10.5000000000000000001, 'x') ORDER BY id",
		"SELECT id FROM cn WHERE n NOT IN (10.5000000000000000001, 'x') ORDER BY id",
		"SELECT id FROM cn WHERE n IN (10.5000000000000000001, '0.3') ORDER BY id",
		"SELECT id FROM cn WHERE n IN (10.5000000000000000001, '10.5', NULL) ORDER BY id",
		"SELECT 0.30 IN (0.3000000000000000001, 'x'), 0.30 IN (0.3000000000000000001, '0.3000000000000000001')",
		"SELECT id FROM cn WHERE n IN ('10.5000000000000000001', '10.5000000000000000001') ORDER BY id",
		"SELECT id FROM cn WHERE n IN ('0.30000000000000004', '7') ORDER BY id",
		"SELECT id FROM cn WHERE n BETWEEN '10.5000000000000000001' AND '11' ORDER BY id",
		"SELECT id FROM cn WHERE n NOT BETWEEN '10.5000000000000000001' AND '11' ORDER BY id",
		"SELECT id FROM cn WHERE NOT (n BETWEEN '10.5000000000000000001' AND '11') ORDER BY id",
		"SELECT id FROM cn WHERE n BETWEEN '10.4' AND '10.4999999999999999999' ORDER BY id",
		"SELECT id FROM cn WHERE n BETWEEN 10.4 AND '10.4999999999999999999' ORDER BY id",
		"SELECT id FROM cn WHERE n BETWEEN '10.5000000000000000001' AND 11 ORDER BY id",
		"SELECT id FROM cn WHERE '10.5000000000000000001' IN (n) ORDER BY id",
		"SELECT id FROM cn WHERE '10.5000000000000000001' IN (n, 0) ORDER BY id",
		"SELECT id FROM cn WHERE n + 0 = '10.5000000000000000001' ORDER BY id",
		"SELECT id FROM cn WHERE n * 1 IN ('10.5000000000000000001', '1') ORDER BY id",
		"SELECT id FROM cn WHERE n = CONCAT('10.5', '000000000000000001') ORDER BY id",
		"SELECT 10.50 = '10.5000000000000000001', '10.5000000000000000001' = 10.50, 10.50 <> '10.5000000000000000001', 10.50 < '10.5000000000000000001'",
		"SELECT 10.50 IN ('10.5000000000000000001'), 10.50 IN ('10.5000000000000000001', '1'), 10.50 BETWEEN '10.5000000000000000001' AND 11, 10.50 NOT BETWEEN '10.5000000000000000001' AND 11",
		"SELECT CAST('10.50' AS DECIMAL(10,2)) = '10.5000000000000000001', CAST('10.50' AS DECIMAL(10,2)) IN ('10.5000000000000000001')",
		"SELECT id, n BETWEEN '10.5000000000000000001' AND '11', n IN ('10.5000000000000000001', '1'), n NOT IN ('10.5000000000000000001', '1'), n = '10.5000000000000000001' FROM cn ORDER BY id",
		"CREATE TABLE cx (id INT PRIMARY KEY, n DECIMAL(10,2), s VARCHAR(40), KEY(n))",
		"INSERT INTO cx VALUES (1, 10.50, '10.5'), (2, 10.50, 'a'), (3, 10.49, 'b'), (4, 0.30, 'c'), (5, 1.01, 'd'), (6, NULL, 'e'), (7, 10.51, 'f'), (8, 11.00, 'g')",
		"SELECT id FROM cx WHERE n BETWEEN '10.5000000000000000001' AND '11' ORDER BY id",
		"SELECT id FROM cx WHERE n NOT BETWEEN '10.5000000000000000001' AND '11' ORDER BY id",
		"SELECT id FROM cx WHERE n BETWEEN '10.4' AND '10.4999999999999999999' ORDER BY id",
		"SELECT id FROM cx WHERE n IN ('10.5000000000000000001', '0.3') ORDER BY id",
		"SELECT id FROM cx WHERE n IN ('10.4999999999999999999', '11') ORDER BY id",
		"SELECT id FROM cx WHERE n NOT IN ('10.5000000000000000001', '0.3') ORDER BY id",
		"SELECT id FROM cx WHERE n IN ('10.5000000000000000001') ORDER BY id",
		"SELECT id FROM cx WHERE n > '10.4999999999999999999' ORDER BY id",
		"SELECT a.id, b.id FROM cx a JOIN cx b ON a.n BETWEEN '10.5000000000000000001' AND b.n WHERE b.id = 8 ORDER BY 1, 2",
		"SELECT a.id, b.id FROM cx a JOIN cx b ON a.n IN ('10.5000000000000000001', b.s) WHERE b.id IN (1, 4) ORDER BY 1, 2",
		"SELECT id, CASE WHEN n BETWEEN '10.5000000000000000001' AND '11' THEN 1 ELSE 0 END FROM cx ORDER BY id",
		wpSQLMode,
		"CREATE TABLE wp_postmeta (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'price', '10.50'), (2, 'price', '10.495'), (3, 'price', '10.5'), (4, 'price', 'abc'), (5, 'price', '10.5000000000000000001'), (6, 'price', '10'), (7, 'price', '9.995'), (8, 'price', '11')",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) >= '10.495' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) = '10.5' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) BETWEEN '10' AND '10.495' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) BETWEEN '9.995' AND '10.5000000000000000001' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) NOT BETWEEN '10' AND '10.495' ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) IN ('10.495', '11') ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) IN ('10.5000000000000000001', '11') ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) NOT IN ('10.5000000000000000001', '11') ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL(10,2)) IN ('10.5000000000000000001') ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND CAST(meta_value AS DECIMAL) BETWEEN '10' AND '11' ORDER BY post_id",
		"SET SESSION sql_mode = DEFAULT",
		"DROP TABLE wp_postmeta",
		"DROP TABLE cx",
		"DROP TABLE cn",
	}},
}

// DECIMAL の列の範囲の端（#4054）: 丸めた端は含む側にし（索引の範囲が行を落とさない）、Filter が元の定数で比べる。
// WooCommerce の価格の絞り込みは min_max_price の索引を使う。
func TestStage115DecimalPlans(t *testing.T) {
	db, stop := stage115PlanDB(t, 3191)
	defer stop()
	for _, c := range []struct{ q, want string }{
		{"SELECT id FROM dec_idx WHERE d < 123.45000001", "filters: [{(NULL, 123.45]}]"},
		{"SELECT id FROM dec_idx WHERE d > 123.445", "filters: [{[123.45, ∞)}]"},
		{"SELECT id FROM dec_idx WHERE d >= 123.455", "filters: [{[123.46, ∞)}]"},
		{"SELECT id FROM dec_idx WHERE d <> 123.451", "filters: [{(NULL, ∞)}]"},
		{"SELECT id FROM dec_idx WHERE d < 99999999999", "filters: [{(NULL, ∞)}]"},
		{"SELECT id FROM dec_idx WHERE d = 123.451", "(dec_idx.d = 123.451)"},
		// 段階 117 から min_max_price はエントリと統計を持つので、エンジンは統計のある先頭の列だけを範囲に入れる（前は
		// 2 列目に (NULL, 19.9999] も付いた）。区間は先頭の等号でない列で決まるので、読むエントリは同じ
		{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.99501 AND max_price <= 19.99985",
			"filters: [{[9.995, ∞), [NULL, ∞)}]"},
		{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.99501 AND max_price <= 19.99985",
			"((wp_wc_product_meta_lookup.min_price >= 9.99501) AND (wp_wc_product_meta_lookup.max_price <= 19.99985))"},
		{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 10.000000 AND max_price <= 25.000000",
			"filters: [{[10, ∞), [NULL, ∞)}]"},
	} {
		if plan := explain(t, db, c.q); !strings.Contains(plan, c.want) {
			t.Errorf("%s\nplan has no %s:\n%s", c.q, c.want, plan)
		}
	}
}

// 並べ替えのある副問い合わせの表は、索引の順を使うマージ結合にしない（LIMIT のある ORDER BY ... LIMIT 1 は前のまま）。
func TestStage115SortedSubqueryPlans(t *testing.T) {
	db, stop := stage115PlanDB(t, 0)
	defer stop()
	for _, c := range []struct{ q, no, want string }{
		{"SELECT id FROM a WHERE id IN (SELECT aid FROM b ORDER BY x)", "MergeJoin", "Join"},
		{"SELECT id FROM a WHERE id IN (SELECT aid FROM b ORDER BY x DESC)", "MergeJoin", "Join"},
		{"SELECT ID FROM wp_posts WHERE ID IN (SELECT post_id FROM wp_postmeta WHERE meta_key = 'rank' ORDER BY meta_value)", "MergeJoin", "Join"},
		{"SELECT id FROM a WHERE id IN (SELECT aid FROM b)", "", "MergeJoin"},
	} {
		plan := explain(t, db, c.q)
		if (c.no != "" && strings.Contains(plan, c.no)) || !strings.Contains(plan, c.want) {
			t.Errorf("%s\nplan has %s or no %s:\n%s", c.q, c.no, c.want, plan)
		}
	}
}

func TestStage115UpstreamFixes(t *testing.T) {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage115Groups {
		stmts = append(stmts, g.stmts...)
	}
	checkGolden(t, "../testdata/stage115_golden.txt", "heimdb_golden115", stmts)
}

// stage115PlanDB は、組 pr の CREATE と INSERT と SET を流したサーバを返す（計画を見るテスト用）。
func stage115PlanDB(t *testing.T, pr int) (*sql.DB, func()) {
	t.Helper()
	dsn, stop := startServer(t, t.TempDir())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, g := range stage115Groups {
		if g.pr != pr {
			continue
		}
		for _, q := range g.stmts {
			if strings.HasPrefix(q, "CREATE") || strings.HasPrefix(q, "INSERT") || strings.HasPrefix(q, "SET") {
				mustExec(t, db, q)
			}
		}
	}
	return db, func() { db.Close(); stop() }
}

// WordPress の NOT IN の形（左の列が NOT NULL）は、#3960 の後も lookup 結合、マージ結合、ハッシュ結合のまま。
// NULL を持てる列の NOT IN はハッシュ結合になる（lookup とマージは NULL の比べを見ないので）。
func TestStage115NotInPlans(t *testing.T) {
	db, stop := stage115PlanDB(t, 3960)
	defer stop()
	mustExec(t, db, wpSQLMode)
	cases := append([]struct{ q, join string }{}, stage115NotInShapes...)
	cases = append(cases,
		struct{ q, join string }{"SELECT /*+ MERGE_JOIN(nonnull_left, nullable_right) */ k FROM nonnull_left WHERE k NOT IN (SELECT k FROM nullable_right) ORDER BY k", "LeftOuterHashJoinExcludingNulls"},
		struct{ q, join string }{"SELECT /*+ LOOKUP_JOIN(nullable_left, nullable_right) */ k FROM nullable_left WHERE k NOT IN (SELECT k FROM nullable_right) ORDER BY k", "LeftOuterHashJoinExcludingNulls"},
		struct{ q, join string }{"SELECT id FROM wp_plugin_items WHERE post_parent NOT IN (SELECT ID FROM wp_posts WHERE post_type = 'post') ORDER BY id", "LeftOuterHashJoinExcludingNulls"},
	)
	for _, c := range cases {
		if plan := explain(t, db, c.q); !strings.Contains(plan, c.join) {
			t.Errorf("%s\nplan has no %s:\n%s", c.q, c.join, plan)
		}
	}
}

// convertKey は、DECIMAL の列で値が丸められる定数を ok が false にする（決まった値として索引と並べ替えを選ぶのに使わない。
// 段階 115）。列の型にそのまま入る値は前と同じく受ける。
func TestStage115ConvertKeyDecimal(t *testing.T) {
	ctx := gsql.NewEmptyContext()
	typ := types.MustCreateDecimalType(10, 2)
	for _, c := range []struct {
		v  any
		ok bool
	}{
		{decimal.RequireFromString("123.45"), true},
		{decimal.RequireFromString("123.4500"), true},
		{decimal.RequireFromString("123.45000001"), false},
		{decimal.RequireFromString("123.455"), false},
		{"123.45", true},
		{"123.451", false},
		{"123.45x", false},
		{int64(123), true},
		{123.45, true},
		{123.451, false},
		{int64(99999999999), false},
	} {
		if _, ok := convertKey(ctx, typ, c.v); ok != c.ok {
			t.Errorf("convertKey(DECIMAL(10,2), %v) ok = %v, want %v", c.v, ok, c.ok)
		}
	}
}

// レビュー 1 の文が lookup 結合と半結合の道を通ることを確かめる（道が変わると、ゴールデンの文がその道を確かめなくなる）。
func TestStage115DecimalLookupPlans(t *testing.T) {
	db, stop := stage115PlanDB(t, 41151)
	defer stop()
	for _, c := range []struct{ q, want string }{
		{"SELECT /*+ LOOKUP_JOIN(pm, lk) */ lk.product_id, pm.post_id FROM pm JOIN lk ON lk.min_price = pm.meta_value ORDER BY 1, 2", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(d2, dd) */ dd.id, d2.id FROM d2 JOIN dd ON dd.d = d2.e ORDER BY 1, 2", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(d2, dd) */ dd.id, d2.id FROM d2 JOIN dd ON dd.d = d2.f ORDER BY 1, 2", "LookupJoin"},
		{"SELECT /*+ LOOKUP_JOIN(pm, lk) */ pm.post_id, lk.product_id FROM pm LEFT JOIN lk ON lk.min_price = pm.meta_value ORDER BY 1, 2", "LeftOuterLookupJoin"},
	} {
		if plan := explain(t, db, c.q); !strings.Contains(plan, c.want) {
			t.Errorf("%s\nplan has no %s:\n%s", c.q, c.want, plan)
		}
	}
}

// どの行にも当たらない lookup のキー（整数の列の '0.01'、DECIMAL の列の '1e30'）は、表もインデックスも読まない
// （2 度目のレビュー 4）。前は空の範囲が端のない区間になり、左の行ごとに右の全体を読んで捨てた（行数の 2 乗の時間）。
func TestStage115EmptyLookupRanges(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	const n = 500
	mustExec(t, db, "CREATE TABLE ek (id INT PRIMARY KEY, i INT, d DECIMAL(10,2), KEY(i), KEY(d))")
	mustExec(t, db, "CREATE TABLE ed (id INT PRIMARY KEY, s VARCHAR(20), big VARCHAR(20))")
	var ek, ed []string
	for i := 1; i <= n; i++ {
		ek = append(ek, fmt.Sprintf("(%d, %d, %d.00)", i, i, i))
		ed = append(ed, fmt.Sprintf("(%d, '%d.%02d', '1e3%d')", i, i/100, i%100, i))
	}
	mustExec(t, db, "INSERT INTO ek VALUES "+strings.Join(ek, ", "))
	mustExec(t, db, "INSERT INTO ed VALUES "+strings.Join(ed, ", "))
	for _, c := range []struct{ q, want string }{
		// '1.00' から '5.00' だけが整数の列に入る
		{"SELECT /*+ LOOKUP_JOIN(ed, ek) JOIN_ORDER(ed, ek) */ COUNT(*) FROM ed JOIN ek ON ek.i = ed.s", "5"},
		{"SELECT /*+ LOOKUP_JOIN(ed, ek) JOIN_ORDER(ed, ek) */ COUNT(*) FROM ed JOIN ek ON ek.id = ed.s", "5"},
		// '1e30' 以上は DECIMAL(10,2) に入らない
		{"SELECT /*+ LOOKUP_JOIN(ed, ek) JOIN_ORDER(ed, ek) */ COUNT(*) FROM ed JOIN ek ON ek.d = ed.big", "0"},
	} {
		if plan := explain(t, db, c.q); !strings.Contains(plan, "LookupJoin") {
			t.Fatalf("%s: no LookupJoin:\n%s", c.q, plan)
		}
		decoded, evals := rowsDecoded.Load(), rangeFilterEvals.Load()
		if got := one(t, db, c.q); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
		// 左の表の n 行と、当たった行だけを読む（前は左の行ごとに右の n 行を読んだ）
		if read := rowsDecoded.Load() - decoded + rangeFilterEvals.Load() - evals; read > 3*n {
			t.Errorf("%s read %d rows, want at most %d", c.q, read, 3*n)
		}
	}
}
