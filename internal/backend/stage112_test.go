package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// stage112Groups は、段階 112 で取り込んだ上流の go-mysql-server の修正の再現の文（docs/engine-upstream.md）。
// 直す前の HeimDB は本物の MySQL 8.0 と違う結果かエラーを返した。各組の終わりに作った表を消す。
var stage112Groups = []struct {
	pr    int
	stmts []string
}{
	{3135, []string{ // NOT EXISTS の右の相関の列に NULL があると、すべての行が落ちた（反結合が NULL の組を一致とみなした）
		"CREATE TABLE xy_hasnull (x INT PRIMARY KEY, y INT)",
		"CREATE TABLE ab_hasnull (a INT PRIMARY KEY, b INT)",
		"INSERT INTO xy_hasnull VALUES (1,0),(2,1),(0,2),(3,NULL)",
		"INSERT INTO ab_hasnull VALUES (0,2),(1,2),(2,NULL),(3,1)",
		"SELECT * FROM xy_hasnull x WHERE NOT EXISTS (SELECT 1 FROM ab_hasnull a WHERE a.b = x.y) ORDER BY x",
		"SELECT * FROM xy_hasnull x WHERE NOT EXISTS (SELECT 1 FROM ab_hasnull a WHERE a.b = x.y AND a.a > 0) ORDER BY x",
		"SELECT * FROM xy_hasnull x WHERE EXISTS (SELECT 1 FROM ab_hasnull a WHERE a.b = x.y) ORDER BY x",
		// NOT IN は前と同じく、右に NULL があればどの行にも当たらず、左の NULL の行も落ちる
		"SELECT x FROM xy_hasnull WHERE y NOT IN (SELECT b FROM ab_hasnull) ORDER BY x",
		"SELECT x FROM xy_hasnull WHERE y NOT IN (SELECT b FROM ab_hasnull WHERE b IS NOT NULL) ORDER BY x",
		"SELECT x FROM xy_hasnull WHERE y NOT IN (SELECT b FROM ab_hasnull WHERE a > 100) ORDER BY x",
		// WordPress の形: NULL を許す列（meta_value、meta_key）で相関する NOT EXISTS（プラグインのメタの参照）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0, post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO p (ID, post_parent) VALUES (1, 0), (2, 1), (3, 1), (4, 0)",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '3'), (2, '_thumbnail_id', NULL), (2, NULL, 'x'), (4, '_edit_lock', '1')",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.meta_key = '_thumbnail_id' AND pm.meta_value = p.ID) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID AND pm.meta_key = '_thumbnail_id') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID AND pm.meta_key = '_thumbnail_id' AND pm.meta_value IS NOT NULL) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.post_status = 'publish' AND NOT EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.post_parent) ORDER BY p.ID",
		// WP_Tax_Query の 'operator' => 'NOT EXISTS'（相関の列は NOT NULL。前から MySQL と同じ）
		"CREATE TABLE tr (object_id BIGINT UNSIGNED NOT NULL, term_taxonomy_id BIGINT UNSIGNED NOT NULL, PRIMARY KEY(object_id, term_taxonomy_id))",
		"CREATE TABLE tt (term_taxonomy_id BIGINT UNSIGNED NOT NULL PRIMARY KEY, taxonomy VARCHAR(32) NOT NULL)",
		"INSERT INTO tt VALUES (10,'category'),(20,'post_tag')",
		"INSERT INTO tr VALUES (1,10),(2,20)",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM tr INNER JOIN tt ON tt.term_taxonomy_id = tr.term_taxonomy_id WHERE tt.taxonomy = 'category' AND tr.object_id = p.ID) ORDER BY p.ID",
		"DROP TABLE tt",
		"DROP TABLE tr",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE ab_hasnull",
		"DROP TABLE xy_hasnull",
	}},
	{3211, []string{ // 条件が NULL になる NOT EXISTS（<>）が、条件が NULL の組を一致とみなした。右が空の EXISTS で iterator を終えた
		"CREATE TABLE t (c VARCHAR(500))",
		"INSERT INTO t VALUES ('a'),('a')",
		"SELECT NULL FROM t WHERE NOT EXISTS (SELECT 1 FROM (SELECT 'a' AS c FROM DUAL WHERE FALSE) v WHERE v.c <> t.c)",
		"WITH v AS (SELECT 'a' AS c FROM DUAL WHERE FALSE) SELECT NULL FROM t WHERE NOT EXISTS (SELECT 1 FROM v WHERE v.c <> t.c)",
		"SELECT COUNT(*) FROM t WHERE EXISTS (SELECT 1 FROM (SELECT 'a' AS c FROM DUAL WHERE FALSE) v WHERE v.c = t.c)",
		"SELECT COUNT(*) FROM t WHERE NOT EXISTS (SELECT 1 FROM (SELECT 'a' AS c FROM DUAL WHERE FALSE) v WHERE v.c <=> t.c)",
		"CREATE TABLE xy_hasnull (x INT PRIMARY KEY, y INT)",
		"CREATE TABLE ab_hasnull (a INT PRIMARY KEY, b INT)",
		"INSERT INTO xy_hasnull VALUES (1,0),(2,1),(0,2),(3,NULL)",
		"INSERT INTO ab_hasnull VALUES (0,2),(1,2),(2,NULL),(3,1)",
		"SELECT x FROM xy_hasnull WHERE NOT EXISTS (SELECT 1 FROM ab_hasnull WHERE ab_hasnull.b <> xy_hasnull.y) ORDER BY x",
		"SELECT x FROM xy_hasnull WHERE EXISTS (SELECT 1 FROM ab_hasnull WHERE ab_hasnull.b <> xy_hasnull.y) ORDER BY x",
		"SELECT x FROM xy_hasnull WHERE NOT EXISTS (SELECT 1 FROM ab_hasnull WHERE ab_hasnull.b > xy_hasnull.y) ORDER BY x",
		"SELECT x FROM xy_hasnull WHERE NOT EXISTS (SELECT 1 FROM ab_hasnull WHERE ab_hasnull.b = -1) ORDER BY x",
		"SELECT x FROM xy_hasnull WHERE EXISTS (SELECT 1 FROM ab_hasnull WHERE ab_hasnull.b = -1) ORDER BY x",
		"CREATE TABLE xy_hasnull_idx (x INT PRIMARY KEY, y INT, KEY y (y))",
		"CREATE TABLE rs (r INT PRIMARY KEY, s INT, KEY s (s))",
		"INSERT INTO xy_hasnull_idx VALUES (1,0),(2,1),(0,2),(3,NULL)",
		"INSERT INTO rs VALUES (0,0),(1,0),(2,1),(3,NULL)",
		"SELECT x FROM xy_hasnull_idx WHERE NOT EXISTS (SELECT 1 FROM rs WHERE rs.s = xy_hasnull_idx.y) ORDER BY x",
		// WordPress の形: 記事の状態と違う値のメタがない記事（<> の相関。meta_value が NULL の行は一致しない）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id))",
		"INSERT INTO p VALUES (1, 'publish'), (2, 'draft'), (3, 'publish')",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_status', 'publish'), (2, '_status', 'publish'), (3, '_status', NULL)",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID AND pm.meta_key = '_status' AND pm.meta_value <> p.post_status) ORDER BY p.ID",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE rs",
		"DROP TABLE xy_hasnull_idx",
		"DROP TABLE ab_hasnull",
		"DROP TABLE xy_hasnull",
		"DROP TABLE t",
	}},
	{3355, []string{ // OR の中の NULL を許す右の列を、反結合から作った LEFT JOIN の NULL の判定に使い、一致した行を残した
		"CREATE TABLE t0 (c0 BOOLEAN)",
		"CREATE TABLE t1 (c0 BOOLEAN)",
		"INSERT INTO t0 VALUES (NULL)",
		"INSERT INTO t1 VALUES (TRUE)",
		"SELECT * FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t0 WHERE (t0.c0) OR (t1.c0))",
		"SELECT * FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t0 WHERE t0.c0 OR NOT t1.c0)",
		"SELECT * FROM t1 WHERE NOT EXISTS (SELECT 1 FROM t0 WHERE t0.c0 = t1.c0)",
		// WordPress の形: NULL を許す meta_value と記事の列の OR で相関する NOT EXISTS
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_title TEXT NOT NULL, post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO p VALUES (1, 'a', 'publish'), (2, 'b', 'draft'), (3, 'c', 'publish')",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_t', 'a'), (2, '_t', NULL), (3, NULL, NULL)",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.meta_value = p.post_title OR p.post_status = 'draft') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.meta_key IS NULL OR p.post_status = 'draft') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID AND (pm.meta_value = p.post_title OR p.post_status = 'draft')) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT 1 FROM pm WHERE pm.meta_value = p.post_title) ORDER BY p.ID",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE t1",
		"DROP TABLE t0",
	}},
	{3503, []string{ // NOT IN の LEFT JOIN（ハッシュでも lookup でもない形）が NULL を落とす印をなくし、NOT IN (SELECT NULL) が行を返した
		"CREATE TABLE hasnullid (id INT, c1 INT)",
		"INSERT INTO hasnullid VALUES (1, 100), (2, 200), (3, 300), (NULL, NULL)",
		"SELECT id FROM hasnullid WHERE id NOT IN (SELECT NULL)",
		"SELECT id FROM hasnullid WHERE id NOT IN (SELECT 1) ORDER BY id",
		"SELECT id FROM hasnullid WHERE id NOT IN (SELECT NULL UNION SELECT 2) ORDER BY id",
		"CREATE TABLE r (v INT)",
		"INSERT INTO r VALUES (1), (NULL)",
		"SELECT id FROM hasnullid WHERE id NOT IN (SELECT v FROM r) ORDER BY id",
		"SELECT id FROM hasnullid WHERE id NOT IN (SELECT v FROM r WHERE v IS NOT NULL) ORDER BY id",
		// WordPress の形: meta_value に NULL があるメタを相手にする NOT IN（どの記事にも当たらない）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO p VALUES (1, 'post'), (2, 'post'), (3, 'attachment'), (4, 'attachment')",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '3'), (2, '_thumbnail_id', NULL), (2, '_edit_lock', '1')",
		"SELECT p.ID FROM p WHERE p.ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.post_type = 'attachment' AND p.ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id' AND meta_value IS NOT NULL) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID NOT IN (SELECT post_id FROM pm WHERE meta_key = '_thumbnail_id') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID NOT IN (SELECT meta_value FROM pm) ORDER BY p.ID",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE r",
		"DROP TABLE hasnullid",
	}},
	{3318, []string{ // 相関の NOT EXISTS と EXISTS の DELETE が 1105（attempted to evaluate uninitialized subquery）。
		// 主キーを書く表は AUTO_INCREMENT にしない（複数行の insert_id の違いは前からの記録だけ）
		"CREATE TABLE student (id BIGINT, name VARCHAR(50) NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE student_hobby (id BIGINT, student_id BIGINT NOT NULL, hobby VARCHAR(50) NOT NULL, PRIMARY KEY (id))",
		"INSERT INTO student VALUES (1,'test1'),(2,'test2')",
		"INSERT INTO student_hobby VALUES (1,1,'test1'),(2,2,'test2'),(3,100,'test3'),(4,100,'test3')",
		"DELETE FROM student_hobby WHERE NOT EXISTS (SELECT 1 FROM student WHERE student.id = student_hobby.student_id)",
		"SELECT * FROM student_hobby ORDER BY id",
		"DELETE FROM student_hobby WHERE EXISTS (SELECT 1 FROM student WHERE student.id = student_hobby.student_id AND student.name = 'test2')",
		"SELECT * FROM student_hobby ORDER BY id",
		// WordPress の形: 記事のなくなったメタを消す掃除（プラグインが出す）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"CREATE TABLE cm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, comment_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY comment_id (comment_id))",
		"CREATE TABLE c (comment_ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, comment_post_ID BIGINT UNSIGNED NOT NULL DEFAULT 0, KEY comment_post_ID (comment_post_ID))",
		"INSERT INTO p VALUES (1, 'post'), (2, 'post'), (3, 'revision')",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_edit_lock', '1'), (2, '_thumbnail_id', NULL), (5, '_edit_lock', '2'), (6, NULL, NULL), (3, '_edit_last', '1'), (7, '_wp_old_slug', 'x')",
		"INSERT INTO c VALUES (1, 1), (2, 9)",
		"INSERT INTO cm (comment_id, meta_key, meta_value) VALUES (1, 'k', 'v'), (2, 'k', 'v'), (3, 'k', 'v')",
		"DELETE FROM pm WHERE NOT EXISTS (SELECT 1 FROM p WHERE p.ID = pm.post_id)",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		"DELETE FROM cm WHERE NOT EXISTS (SELECT 1 FROM c WHERE c.comment_ID = cm.comment_id)",
		"SELECT meta_id, comment_id FROM cm ORDER BY meta_id",
		"DELETE FROM c WHERE NOT EXISTS (SELECT * FROM p WHERE p.ID = c.comment_post_ID)",
		"SELECT * FROM c ORDER BY comment_ID",
		"DELETE FROM pm WHERE EXISTS (SELECT 1 FROM p WHERE p.ID = pm.post_id AND p.post_type = 'revision')",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (8, 'a', '1'), (9, 'a', '1'), (10, 'a', '1')",
		"DELETE FROM pm WHERE NOT EXISTS (SELECT 1 FROM p WHERE p.ID = pm.post_id) ORDER BY meta_id LIMIT 2",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		"DELETE pm FROM pm WHERE NOT EXISTS (SELECT 1 FROM p WHERE p.ID = pm.post_id)",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (11, 'a', '1')",
		"DELETE FROM pm WHERE post_id NOT IN (SELECT ID FROM p)",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		// 相関の EXISTS の UPDATE も同じ 1105 だった（NOT EXISTS の UPDATE は #3455）
		"UPDATE p SET post_type = 'page' WHERE EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID AND pm.meta_key = '_edit_lock')",
		"SELECT * FROM p ORDER BY ID",
		"UPDATE student SET name = 'has hobby' WHERE EXISTS (SELECT 1 FROM student_hobby WHERE student_hobby.student_id = student.id)",
		"SELECT * FROM student ORDER BY id",
		// 相手の行が 2 つある EXISTS の UPDATE と DELETE は、行を 1 度だけ変える
		"CREATE TABLE p2 (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, n INT NOT NULL DEFAULT 0)",
		"CREATE TABLE pm2 (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, KEY post_id (post_id))",
		"INSERT INTO p2 (ID) VALUES (1), (2), (3)",
		"INSERT INTO pm2 (post_id, meta_key) VALUES (1, 'a'), (1, 'a'), (2, 'a'), (2, 'b')",
		"UPDATE p2 SET n = n + 1 WHERE EXISTS (SELECT 1 FROM pm2 WHERE pm2.post_id = p2.ID AND pm2.meta_key = 'a')",
		"SELECT * FROM p2 ORDER BY ID",
		"DELETE FROM pm2 WHERE EXISTS (SELECT 1 FROM p2 WHERE p2.ID = pm2.post_id AND p2.n > 0)",
		"SELECT meta_id FROM pm2 ORDER BY meta_id",
		"DROP TABLE pm2",
		"DROP TABLE p2",
		"DROP TABLE c",
		"DROP TABLE cm",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE student_hobby",
		"DROP TABLE student",
	}},
	{3455, []string{ // UPDATE の中の反結合（NOT IN と相関の NOT EXISTS）を LEFT JOIN にして、UPDATE JOIN の iterator が列を見失い 1105
		"CREATE TABLE p (id VARCHAR(32) PRIMARY KEY, status VARCHAR(16), name VARCHAR(32))",
		"INSERT INTO p VALUES ('a', 'open', 'x'), ('b', 'open', 'y'), ('c', 'open', 'z')",
		"CREATE TABLE k (id VARCHAR(32) PRIMARY KEY, status VARCHAR(16))",
		"INSERT INTO k VALUES ('a', 'keep'), ('b', 'drop')",
		"UPDATE p SET status = 'closed' WHERE id NOT IN (SELECT id FROM k WHERE status = 'keep')",
		"SELECT * FROM p ORDER BY id",
		"UPDATE p SET status = 'x' WHERE NOT EXISTS (SELECT 1 FROM k WHERE k.id = p.id AND k.status = 'drop')",
		"SELECT * FROM p ORDER BY id",
		"UPDATE p SET status = 'y' WHERE id NOT IN (SELECT id FROM k)",
		"SELECT * FROM p ORDER BY id",
		"INSERT INTO k VALUES ('n', NULL)",
		"UPDATE p SET name = 'n' WHERE status NOT IN (SELECT status FROM k)",
		"UPDATE p SET name = 'm' WHERE NOT EXISTS (SELECT 1 FROM k WHERE k.status = p.status)",
		"SELECT * FROM p ORDER BY id",
		// WordPress の形: メタのない記事の状態を変える（一括の変更のプラグイン）
		"CREATE TABLE wp (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_status VARCHAR(20) NOT NULL DEFAULT 'publish', post_type VARCHAR(20) NOT NULL DEFAULT 'post')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO wp (ID) VALUES (1), (2), (3), (4)",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_keep', '1'), (1, '_keep', '2'), (3, '_keep', NULL), (4, '_other', '1')",
		"UPDATE wp SET post_status = 'draft' WHERE ID NOT IN (SELECT post_id FROM pm WHERE meta_key = '_keep')",
		"SELECT * FROM wp ORDER BY ID",
		"UPDATE wp SET post_status = 'private' WHERE post_type = 'post' AND NOT EXISTS (SELECT 1 FROM pm WHERE pm.post_id = wp.ID AND pm.meta_key = '_keep' AND pm.meta_value IS NOT NULL)",
		"SELECT * FROM wp ORDER BY ID",
		"UPDATE wp SET post_type = 'page' WHERE ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_keep')",
		"UPDATE wp SET post_type = 'page' WHERE ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_other')",
		"SELECT * FROM wp ORDER BY ID",
		// 書いた LEFT JOIN の UPDATE は前と同じ（相手のない右の行は変えない）
		"UPDATE wp LEFT JOIN pm ON pm.post_id = wp.ID AND pm.meta_key = '_keep' SET pm.meta_value = 'L', wp.post_type = 'post'",
		"SELECT * FROM wp ORDER BY ID",
		"SELECT meta_id, post_id, meta_key, meta_value FROM pm ORDER BY meta_id",
		"UPDATE wp LEFT JOIN pm ON pm.post_id = wp.ID AND pm.meta_key = '_keep' SET wp.post_status = 'nometa' WHERE pm.meta_id IS NULL",
		"SELECT * FROM wp ORDER BY ID",
		// DELETE は前から LEFT JOIN の形で消せる
		"DELETE FROM pm WHERE post_id NOT IN (SELECT ID FROM wp WHERE post_status = 'publish')",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		"DROP TABLE pm",
		"DROP TABLE wp",
		"DROP TABLE k",
		"DROP TABLE p",
	}},
	{3212, []string{ // 半結合を内部結合にしたとき右の重複を除かず、IN (SELECT ...) と EXISTS が左の行を重ねて返した
		"CREATE TABLE u (c0 INT, c1 INT, PRIMARY KEY (c0, c1))",
		"INSERT INTO u VALUES (1,1),(2,2),(2,3)",
		"SELECT * FROM u WHERE EXISTS (SELECT 1 FROM u AS x WHERE x.c0 = u.c0) ORDER BY c0, c1",
		"SELECT * FROM u WHERE c0 IN (SELECT c0 FROM u AS x) ORDER BY c0, c1",
		"SELECT COUNT(*) FROM u WHERE c0 IN (SELECT c0 FROM u AS x WHERE x.c1 > 0)",
		"CREATE TABLE xy_hasnull_idx (x INT PRIMARY KEY, y INT, KEY y (y))",
		"CREATE TABLE rs (r INT PRIMARY KEY, s INT, KEY s (s))",
		"INSERT INTO xy_hasnull_idx VALUES (1,0),(2,1),(0,2),(3,NULL)",
		"INSERT INTO rs VALUES (0,0),(1,0),(2,1),(3,NULL)",
		"SELECT x FROM xy_hasnull_idx WHERE EXISTS (SELECT 1 FROM rs WHERE rs.s = xy_hasnull_idx.y) ORDER BY x",
		"SELECT x FROM xy_hasnull_idx WHERE y IN (SELECT s FROM rs) ORDER BY x",
		// WordPress の形: 同じ記事に当たるメタやタームが複数ある IN (SELECT ...) と EXISTS（プラグインの絞り込み）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_status VARCHAR(20) NOT NULL DEFAULT 'publish')",
		"CREATE TABLE tr (object_id BIGINT UNSIGNED NOT NULL, term_taxonomy_id BIGINT UNSIGNED NOT NULL, PRIMARY KEY(object_id, term_taxonomy_id), KEY term_taxonomy_id (term_taxonomy_id))",
		"CREATE TABLE tt (term_taxonomy_id BIGINT UNSIGNED NOT NULL PRIMARY KEY, taxonomy VARCHAR(32) NOT NULL)",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO p (ID, post_type) VALUES (1, 'post'), (2, 'post'), (3, 'page'), (4, 'post')",
		"INSERT INTO tt VALUES (10,'category'),(11,'category'),(20,'post_tag')",
		"INSERT INTO tr VALUES (1,10),(1,11),(1,20),(2,20),(3,10),(3,11)",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1,'_color','red'),(1,'_color','blue'),(2,'_color','red'),(2,'_size','L'),(3,'_size','M'),(3,'_size','M')",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT post_id FROM pm WHERE meta_value = 'red' OR meta_value = 'blue') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT post_id FROM pm WHERE meta_key = '_color') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT post_id FROM pm WHERE meta_key = '_size' AND meta_value = 'M') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID AND pm.meta_key = '_color') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT object_id FROM tr WHERE term_taxonomy_id IN (10, 11)) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.post_type = 'post' AND p.ID IN (SELECT object_id FROM tr WHERE term_taxonomy_id IN (10, 11, 20)) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT 1 FROM tr INNER JOIN tt ON tt.term_taxonomy_id = tr.term_taxonomy_id WHERE tt.taxonomy = 'category' AND tr.object_id = p.ID) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT tr.object_id FROM tr INNER JOIN tt ON tt.term_taxonomy_id = tr.term_taxonomy_id WHERE tt.taxonomy IN ('category', 'post_tag')) ORDER BY p.ID",
		"SELECT COUNT(*) FROM p WHERE p.ID IN (SELECT post_id FROM pm WHERE meta_key IN ('_color', '_size'))",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT post_id FROM pm WHERE meta_key = '_color') AND p.ID IN (SELECT object_id FROM tr WHERE term_taxonomy_id = 20) ORDER BY p.ID",
		"DROP TABLE pm",
		"DROP TABLE tt",
		"DROP TABLE tr",
		"DROP TABLE p",
		"DROP TABLE rs",
		"DROP TABLE xy_hasnull_idx",
		"DROP TABLE u",
	}},
	{3212, []string{ // 半結合の下の外部結合: 書いた LEFT JOIN の UPDATE と DELETE に IN (SELECT ...) や EXISTS を足すと、半結合が
		// 一番上の結合になり、LEFT JOIN が NULL で埋めた右の行を更新しようとして 1105（NULL in primary key）。消すときも同じ
		"CREATE TABLE uj1 (id INT PRIMARY KEY, a INT, x INT)",
		"CREATE TABLE uj2 (id INT PRIMARY KEY, a INT, y INT)",
		"CREATE TABLE uj3 (id INT PRIMARY KEY)",
		"CREATE TABLE uj4 (id INT PRIMARY KEY, z INT)",
		"INSERT INTO uj1 VALUES (1,1,0),(2,2,0),(3,3,0)",
		"INSERT INTO uj2 VALUES (10,1,0),(20,2,0)",
		"INSERT INTO uj3 VALUES (1),(3)",
		"INSERT INTO uj4 VALUES (10,0)",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj2.y = 1 WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj2.y = 2 WHERE uj1.id IN (SELECT id FROM uj3 WHERE id > 0)",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj2.y = 3, uj1.x = 3 WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj1 ORDER BY id",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj1.x = 4 WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj1 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj2.y = 5 WHERE EXISTS (SELECT 1 FROM uj3 WHERE uj3.id = uj1.id)",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj2.y = 6, uj1.x = 6 WHERE NOT EXISTS (SELECT 1 FROM uj3 WHERE uj3.id = uj1.id)",
		"SELECT * FROM uj1 ORDER BY id",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj2.y = 7 WHERE uj1.id NOT IN (SELECT id FROM uj3)",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj2 RIGHT JOIN uj1 ON uj1.a = uj2.a SET uj2.y = 8 WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj2 RIGHT JOIN uj1 ON uj1.a = uj2.a SET uj2.y = 9, uj1.x = 9 WHERE EXISTS (SELECT 1 FROM uj3 WHERE uj3.id = uj1.id)",
		"SELECT * FROM uj1 ORDER BY id",
		"SELECT * FROM uj2 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a LEFT JOIN uj4 ON uj4.id = uj2.id SET uj4.z = 1, uj2.y = 10 WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj2 ORDER BY id",
		"SELECT * FROM uj4 ORDER BY id",
		"UPDATE uj1 LEFT JOIN uj2 ON uj1.a = uj2.a SET uj1.x = 11 WHERE uj2.id IN (SELECT id FROM uj4) OR uj2.id IS NULL",
		"SELECT * FROM uj1 ORDER BY id",
		// 主キーのない表のすべて NULL の本物の行（LEFT JOIN の左）は消す（主キーのない表の UPDATE JOIN は HeimDB も上流も
		// 非対応のエラー）
		"CREATE TABLE ujn (a INT, b INT)",
		"INSERT INTO ujn VALUES (NULL, NULL), (1, NULL), (5, 0)",
		"DELETE ujn FROM ujn LEFT JOIN uj2 ON ujn.a = uj2.a WHERE ujn.b IS NULL AND ujn.a IN (SELECT id FROM uj3) OR ujn.a IS NULL",
		"SELECT * FROM ujn ORDER BY a, b",
		"DELETE ujn FROM ujn LEFT JOIN uj2 ON ujn.a = uj2.a WHERE ujn.a IN (SELECT id FROM uj3)",
		"SELECT * FROM ujn ORDER BY a, b",
		// DELETE: 右の表だけ、左の表だけ、両方
		"DELETE uj2 FROM uj1 LEFT JOIN uj2 ON uj1.a = uj2.a WHERE uj1.id IN (1, 3)",
		"SELECT * FROM uj2 ORDER BY id",
		"INSERT INTO uj2 VALUES (10,1,0)",
		"DELETE uj2 FROM uj1 LEFT JOIN uj2 ON uj1.a = uj2.a WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj2 ORDER BY id",
		"INSERT INTO uj2 VALUES (10,1,0)",
		"DELETE uj2 FROM uj1 LEFT JOIN uj2 ON uj1.a = uj2.a WHERE EXISTS (SELECT 1 FROM uj3 WHERE uj3.id = uj1.id)",
		"SELECT * FROM uj2 ORDER BY id",
		"INSERT INTO uj2 VALUES (10,1,0)",
		"DELETE uj2 FROM uj2 RIGHT JOIN uj1 ON uj1.a = uj2.a WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj2 ORDER BY id",
		"INSERT INTO uj2 VALUES (10,1,0)",
		"DELETE uj1 FROM uj1 LEFT JOIN uj2 ON uj1.a = uj2.a WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj1 ORDER BY id",
		"INSERT INTO uj1 VALUES (1,1,0),(3,3,0)",
		"DELETE uj1, uj2 FROM uj1 LEFT JOIN uj2 ON uj1.a = uj2.a WHERE uj1.id IN (SELECT id FROM uj3)",
		"SELECT * FROM uj1 ORDER BY id",
		"SELECT * FROM uj2 ORDER BY id",
		// WordPress の形: 記事とメタの LEFT JOIN（一括の変更や掃除のプラグイン）
		"CREATE TABLE wp (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post', menu_order INT NOT NULL DEFAULT 0)",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"CREATE TABLE tr (object_id BIGINT UNSIGNED NOT NULL, term_taxonomy_id BIGINT UNSIGNED NOT NULL, PRIMARY KEY(object_id, term_taxonomy_id))",
		"INSERT INTO wp (ID, post_type) VALUES (1, 'post'), (2, 'post'), (3, 'revision'), (4, 'revision'), (5, 'post')",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_k', 'a'), (1, '_j', 'b'), (3, '_k', 'c'), (5, '_k', 'd')",
		"INSERT INTO tr VALUES (1, 10), (2, 10), (3, 20), (4, 20)",
		"UPDATE wp LEFT JOIN pm ON pm.post_id = wp.ID AND pm.meta_key = '_k' SET pm.meta_value = 'x' WHERE wp.ID IN (SELECT object_id FROM tr WHERE term_taxonomy_id = 10)",
		"UPDATE wp LEFT JOIN pm ON pm.post_id = wp.ID AND pm.meta_key = '_k' SET pm.meta_value = 'y', wp.menu_order = 1 WHERE EXISTS (SELECT 1 FROM tr WHERE tr.object_id = wp.ID AND tr.term_taxonomy_id = 20)",
		"SELECT * FROM wp ORDER BY ID",
		"SELECT meta_id, post_id, meta_key, meta_value FROM pm ORDER BY meta_id",
		"DELETE pm FROM wp LEFT JOIN pm ON pm.post_id = wp.ID WHERE wp.ID IN (SELECT object_id FROM tr WHERE term_taxonomy_id = 20)",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		"DELETE a, b, c FROM wp a LEFT JOIN tr b ON b.object_id = a.ID LEFT JOIN pm c ON c.post_id = a.ID WHERE a.post_type = 'revision'",
		"SELECT * FROM wp ORDER BY ID",
		"SELECT * FROM tr ORDER BY object_id, term_taxonomy_id",
		"DELETE c FROM wp a LEFT JOIN pm c ON c.post_id = a.ID WHERE a.ID IN (SELECT object_id FROM tr WHERE term_taxonomy_id = 10)",
		"SELECT meta_id, post_id FROM pm ORDER BY meta_id",
		// 数と文字列の IN (SELECT ...) を選ぶ列に置く（結合にしない。相関する副問い合わせと、左が NULL の形）
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (2, '_m', ' 2'), (2, '_n', NULL), (5, '_m', '5.0'), (5, '_m', '7abc')",
		"SELECT ID, ID IN (SELECT meta_value FROM pm WHERE pm.post_id = wp.ID) FROM wp ORDER BY ID",
		"SELECT ID, ID + 2 IN (SELECT meta_value FROM pm WHERE pm.post_id = wp.ID AND meta_key = '_m') FROM wp ORDER BY ID",
		"SELECT ID, NULLIF(ID, ID) IN (SELECT meta_value FROM pm WHERE pm.post_id = wp.ID) FROM wp ORDER BY ID",
		"SELECT ID, NULLIF(ID, ID) NOT IN (SELECT meta_value FROM pm WHERE pm.post_id = wp.ID AND meta_value IS NOT NULL) FROM wp ORDER BY ID",
		"DROP TABLE tr",
		"DROP TABLE pm",
		"DROP TABLE wp",
		"DROP TABLE ujn",
		"DROP TABLE uj4",
		"DROP TABLE uj3",
		"DROP TABLE uj2",
		"DROP TABLE uj1",
	}},
	{3212, []string{ // ハッシュ結合の作る側の外部結合: 内部結合の相手の表が大きいと、書いた LEFT JOIN がハッシュ結合の作る側
		// （HashLookup）の下に入り、その下の外部結合を見落として、相手のない行を更新して消そうとし 1105 になった
		"CREATE TABLE hb1 (id INT PRIMARY KEY, a INT, x INT)",
		"CREATE TABLE hb2 (id INT PRIMARY KEY, a INT, b INT, y INT)",
		"CREATE TABLE hb3 (id INT PRIMARY KEY, b INT, z INT)",
		"INSERT INTO hb1 VALUES (1,10,0),(2,20,0),(3,30,0),(4,40,0)",
		"INSERT INTO hb2 VALUES (1,10,100,0),(2,20,200,0),(5,50,500,0)",
		"INSERT INTO hb3 WITH RECURSIVE n AS (SELECT 1 AS i UNION ALL SELECT i + 1 FROM n WHERE i < 50) SELECT a.i * 100 + b.i, a.i, 0 FROM n a, n b",
		"ANALYZE TABLE hb1, hb2, hb3",
		"UPDATE hb1 LEFT JOIN hb2 ON hb1.a = hb2.a JOIN hb3 ON hb3.z = hb1.x SET hb2.y = 1",
		"SELECT * FROM hb2 ORDER BY id",
		"UPDATE hb1 LEFT JOIN hb2 ON hb1.a = hb2.a, hb3 SET hb2.y = 2 WHERE hb3.z = hb1.x",
		"SELECT * FROM hb2 ORDER BY id",
		"UPDATE hb3 JOIN (hb1 LEFT JOIN hb2 ON hb1.a = hb2.a) ON hb3.z = hb1.x SET hb2.y = 3, hb1.x = 0",
		"SELECT * FROM hb1 ORDER BY id",
		"SELECT * FROM hb2 ORDER BY id",
		"DELETE hb2 FROM hb1 LEFT JOIN hb2 ON hb1.a = hb2.a, hb3 WHERE hb3.z = hb1.x",
		"SELECT * FROM hb2 ORDER BY id",
		// 結合から 1 つの表だけを消す DELETE は、何度も結び付く行も 1 度だけ数える（前は結び付いた数）
		"DELETE hb1 FROM hb1 JOIN hb3 ON hb3.z = hb1.x WHERE hb1.id <= 2",
		"SELECT * FROM hb1 ORDER BY id",
		"DROP TABLE hb3",
		"DROP TABLE hb2",
		"DROP TABLE hb1",
	}},
	{3263, []string{ // 派生表の中の、同じ表への IN (SELECT ...) の半結合の下の列を刈り、1105（index out of range）
		"CREATE TABLE t0 (id INT PRIMARY KEY, name LONGTEXT, description LONGTEXT, comment LONGTEXT, created_at TIMESTAMP(6), archived BOOL)",
		"INSERT INTO t0 VALUES (1, 'first', 'abc', 'def', '2025-10-14 10:00:00', 0)",
		"SELECT * FROM (SELECT id, name, description, archived FROM t0 AS test0 WHERE (id IN (SELECT id FROM t0))) AS dummy_alias ORDER BY dummy_alias.name ASC",
		"SELECT dummy_alias.name FROM (SELECT id, name, archived FROM t0 AS test0 WHERE id IN (SELECT id FROM t0 WHERE archived = 0)) AS dummy_alias ORDER BY dummy_alias.name",
		// WordPress の形: 派生表の中の、記事やメタへの IN (SELECT ...)（プラグインの集計）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_title TEXT NOT NULL, post_status VARCHAR(20) NOT NULL DEFAULT 'publish', post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0)",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO p VALUES (1, 'b', 'publish', 'post', 0), (2, 'a', 'draft', 'post', 0), (3, 'c', 'publish', 'page', 1), (4, 'd', 'publish', 'attachment', 1)",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '4'), (2, '_thumbnail_id', '4'), (3, '_edit_lock', '1')",
		"SELECT * FROM (SELECT ID, post_title, post_status FROM p AS p0 WHERE ID IN (SELECT ID FROM p WHERE post_type = 'post')) AS d ORDER BY d.post_title",
		"SELECT d.post_title FROM (SELECT ID, post_title, post_type FROM p AS p0 WHERE ID IN (SELECT post_parent FROM p)) AS d ORDER BY d.post_title DESC",
		"SELECT d.post_status, COUNT(*) FROM (SELECT ID, post_title, post_status FROM p WHERE ID IN (SELECT post_id FROM pm WHERE meta_key = '_thumbnail_id')) AS d GROUP BY d.post_status ORDER BY d.post_status",
		"SELECT * FROM (SELECT ID, post_title FROM p WHERE EXISTS (SELECT 1 FROM pm WHERE pm.post_id = p.ID)) AS d ORDER BY d.post_title",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE t0",
	}},
	{3780, []string{ // GROUP BY のない集計の EXISTS（いつも 1 行を返す）を結合にして、集計を外して相手のない行で偽にした
		"CREATE TABLE t (id INT PRIMARY KEY, k INT, f INT DEFAULT 0)",
		"INSERT INTO t (id, k) VALUES (1, 10), (2, 99)",
		"CREATE TABLE u (k INT PRIMARY KEY)",
		"INSERT INTO u VALUES (10)",
		"SELECT id FROM t WHERE EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k) ORDER BY id",
		"SELECT id FROM t WHERE NOT EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k) ORDER BY id",
		"SELECT id FROM t WHERE EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k HAVING COUNT(*) > 0) ORDER BY id",
		"SELECT id FROM t WHERE EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k LIMIT 0) ORDER BY id",
		"SELECT id FROM t WHERE EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k LIMIT 1 OFFSET 1) ORDER BY id",
		"SELECT id FROM t WHERE EXISTS(SELECT u.k FROM u WHERE u.k = t.k GROUP BY u.k) ORDER BY id",
		"DELETE FROM t WHERE NOT EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k)",
		"SELECT id FROM t ORDER BY id",
		"UPDATE t SET f = 9 WHERE NOT EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k)",
		"UPDATE t SET f = 8 WHERE EXISTS(SELECT MAX(u.k) FROM u WHERE u.k = t.k)",
		"SELECT id, f FROM t ORDER BY id",
		"CREATE TABLE r (id INT, k INT)",
		"INSERT INTO r SELECT id, k FROM t WHERE NOT EXISTS(SELECT COUNT(*) FROM u WHERE u.k = t.k)",
		"SELECT id FROM r ORDER BY id",
		// WordPress の形: メタの数や最大を数える副問い合わせを EXISTS に入れた文（プラグインの書き誤り）
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_type VARCHAR(20) NOT NULL DEFAULT 'post')",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"INSERT INTO p (ID) VALUES (1), (2), (3)",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_views', '5'), (1, '_views', '7'), (3, '_edit_lock', '1')",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT COUNT(*) FROM pm WHERE pm.post_id = p.ID AND pm.meta_key = '_views') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE NOT EXISTS (SELECT MAX(meta_value) FROM pm WHERE pm.post_id = p.ID) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT COUNT(*) FROM pm WHERE pm.post_id = p.ID HAVING COUNT(*) > 1) ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT pm.post_id FROM pm WHERE pm.post_id = p.ID GROUP BY pm.post_id) ORDER BY p.ID",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE r",
		"DROP TABLE u",
		"DROP TABLE t",
	}},
	{3343, []string{ // 文字列の主キーを数のキーで引く lookup 結合が、' 12' や '12abc'（MySQL は double で比べて 12 に等しい）を落とした
		"CREATE TABLE t0(c0 VARCHAR(500), c1 BOOLEAN, PRIMARY KEY(c0))",
		"CREATE TABLE t1(c0 BOOLEAN, c1 BOOLEAN)",
		"INSERT INTO t1(c0, c1) VALUES (false, true)",
		"INSERT INTO t0(c0, c1) VALUES ('i', true)",
		"SELECT t0.c1, t0.c0 FROM t0 NATURAL RIGHT JOIN t1",
		"CREATE TABLE s (k VARCHAR(20) PRIMARY KEY)",
		"INSERT INTO s VALUES ('12'), (' 12'), ('12abc'), ('13'), ('x')",
		"CREATE TABLE n (v INT)",
		"INSERT INTO n VALUES (12), (0)",
		"SELECT s.k FROM n JOIN s ON s.k = n.v ORDER BY s.k",
		"SELECT s.k, n.v FROM s JOIN n ON n.v = s.k ORDER BY s.k",
		"SELECT n.v, s.k FROM n LEFT JOIN s ON s.k = n.v ORDER BY n.v, s.k",
		"SELECT s.k FROM s WHERE s.k IN (SELECT v FROM n) ORDER BY s.k",
		"SELECT s.k FROM s WHERE EXISTS (SELECT 1 FROM n WHERE n.v = s.k) ORDER BY s.k",
		"SELECT s.k FROM s WHERE s.k = 'x' OR s.k IN (SELECT v FROM n) ORDER BY s.k",
		"SELECT n.v FROM n WHERE n.v = 5 OR n.v IN (SELECT k FROM s) ORDER BY n.v",
		"SELECT n.v FROM n WHERE n.v = 5 OR n.v NOT IN (SELECT k FROM s) ORDER BY n.v",
		"SELECT s.k FROM s WHERE s.k = 12 ORDER BY s.k",
		"SELECT s.k FROM s WHERE s.k IN (12, 13) ORDER BY s.k",
		// WordPress の形: アイキャッチの meta_value（LONGTEXT）と記事の ID（BIGINT）の結合。meta_value に索引を足す
		// プラグイン（Index WP MySQL For Speed の meta_value(32)）があると、記事の ID で meta_value の索引を引く
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_name VARCHAR(200) NOT NULL DEFAULT '', post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0, menu_order INT NOT NULL DEFAULT 0, KEY post_name (post_name(191)), KEY type_status (post_type))",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"ALTER TABLE pm ADD KEY meta_value (meta_value(32))",
		"INSERT INTO p (ID, post_name, post_type) VALUES (1, 'hello', 'post'), (2, '3', 'page'), (3, 'img', 'attachment'), (4, '03', 'attachment'), (5, '3-2', 'post'), (6, ' 3', 'page')",
		"INSERT INTO pm (post_id, meta_key, meta_value) VALUES (1, '_thumbnail_id', '3'), (2, '_thumbnail_id', ' 3'), (5, '_thumbnail_id', '3abc'), (6, '_thumbnail_id', '3.0'), (1, '_thumbnail_id', '3.5'), (2, '_thumbnail_id', '4e0'), (5, '_thumbnail_id', ''), (6, '_thumbnail_id', NULL), (1, '_k', 'abc'), (2, '_k', '04')",
		"SELECT p.ID, pm.meta_id FROM p JOIN pm ON pm.meta_value = p.ID ORDER BY p.ID, pm.meta_id",
		"SELECT p.ID, pm.meta_id FROM p JOIN pm ON pm.meta_value = p.ID WHERE p.post_type = 'attachment' ORDER BY p.ID, pm.meta_id",
		"SELECT pm.meta_id, p.ID FROM pm JOIN p ON p.ID = pm.meta_value WHERE pm.meta_key = '_thumbnail_id' ORDER BY pm.meta_id",
		"SELECT p.ID FROM p WHERE p.ID IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id') ORDER BY p.ID",
		"SELECT p.ID FROM p WHERE EXISTS (SELECT 1 FROM pm WHERE pm.meta_value = p.ID) ORDER BY p.ID",
		"SELECT meta_id FROM pm WHERE meta_value = 3 ORDER BY meta_id",
		"SELECT meta_id FROM pm WHERE meta_key = '_thumbnail_id' AND meta_value IN (3, 4) ORDER BY meta_id",
		"SELECT meta_id FROM pm WHERE meta_value > 3 ORDER BY meta_id",
		"SELECT ID FROM p WHERE post_name = 3 ORDER BY ID",
		"SELECT p.ID, p2.ID FROM p JOIN p p2 ON p2.post_name = p.ID ORDER BY p.ID, p2.ID",
		"SELECT ID FROM p WHERE ID = '3abc'",
		"SELECT ID FROM p WHERE ID = ' 3'",
		"SELECT ID FROM p WHERE ID = '3.5'",
		"SELECT ID FROM p WHERE ID IN ('3', '4.0', '5.5') ORDER BY ID",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE n",
		"DROP TABLE s",
		"DROP TABLE t1",
		"DROP TABLE t0",
	}},
	{3423, []string{ // 文字列と数の列のマージ結合と範囲のヒープ結合が、並びの順の違いで行を落とした
		"CREATE TABLE t0(c0 VARCHAR(500) PRIMARY KEY, c1 INT)",
		"CREATE TABLE t1(c0 INT PRIMARY KEY, c1 VARCHAR(500))",
		"INSERT INTO t0(c0) VALUES (5), (33), (223), ('123a')",
		"INSERT INTO t1(c0) VALUES (5), (33), (223), (123)",
		"INSERT INTO t1(c0) VALUES (-1)",
		"INSERT INTO t0(c0, c1) VALUES (false, -2)",
		"SELECT t0.c0, t1.c0 FROM t0 JOIN t1 ON t0.c0 = t1.c0 ORDER BY t1.c0",
		"SELECT * FROM t1 INNER JOIN t0 ON (t1.c0 BETWEEN t0.c1 AND t0.c0)",
		"SELECT t0.c0, t1.c0 FROM t1 LEFT JOIN t0 ON t0.c0 = t1.c0 ORDER BY t1.c0",
		"SELECT * FROM t1 INNER JOIN t0 ON t1.c0 >= t0.c1 AND t1.c0 <= t0.c0 ORDER BY t1.c0, t0.c0",
		// WordPress の形: meta_value の索引（Index WP MySQL For Speed）と ID の主キーを並べて突き合わせると、文字列の
		// 並び（'101' が '11' より前）で添付の行を落とした
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_name VARCHAR(200) NOT NULL DEFAULT '', post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0, menu_order INT NOT NULL DEFAULT 0, KEY post_name (post_name(191)), KEY type_status (post_type))",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"ALTER TABLE pm ADD KEY meta_value (meta_value(32))",
		"SET SESSION cte_max_recursion_depth = 1000",
		"INSERT INTO p (ID, post_name, post_type, post_parent, menu_order) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) SELECT i, CONCAT('p-', i), IF(i % 2 = 0, 'attachment', 'post'), IF(i % 2 = 0, i - 1, 0), i % 7 FROM n",
		"INSERT INTO pm (post_id, meta_key, meta_value) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300), k (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j < 5) SELECT i, IF(j = 0, '_thumbnail_id', CONCAT('_k', j)), IF(j = 0, ELT(1 + (i DIV 2) % 6, CAST(i + 1 AS CHAR), CONCAT(' ', i + 1), CONCAT(i + 1, 'abc'), CONCAT(i + 1, '.0'), CONCAT(i + 1, '.5'), CONCAT('0', i + 1)), CONCAT('v', i, '-', j)) FROM n, k WHERE j > 0 OR i % 2 = 1 ORDER BY i, j",
		"ANALYZE TABLE p, pm",
		"SELECT a.ID, pm.post_id FROM p a INNER JOIN pm ON pm.meta_value = a.ID WHERE a.post_type = 'attachment' AND a.ID < 40 ORDER BY a.ID, pm.post_id",
		"SELECT COUNT(*), SUM(a.ID), SUM(pm.post_id) FROM p a INNER JOIN pm ON pm.meta_value = a.ID WHERE a.post_type = 'attachment'",
		"SELECT COUNT(*), SUM(a.ID) FROM p a INNER JOIN pm ON a.ID = pm.meta_value",
		"SELECT COUNT(*) FROM pm LEFT JOIN p a ON a.ID = pm.meta_value WHERE a.ID IS NULL",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE t1",
		"DROP TABLE t0",
	}},
	{3200, []string{ // ハッシュ結合が両辺を左の型に揃えて引き、BOOLEAN と VARCHAR（MySQL は double で比べ、数にできない文字列は 0）の組を落とした
		"CREATE TABLE t0 (c1 BOOLEAN)",
		"CREATE TABLE t1 (c2 VARCHAR(500))",
		"INSERT INTO t0 VALUES (FALSE),(TRUE)",
		"INSERT INTO t1 VALUES ('z'),('3bxu'),('O5'),('g'),('Qo9')",
		"SELECT * FROM t0, t1 WHERE t1.c2 = t0.c1 ORDER BY c2",
		"SELECT * FROM t0 JOIN t1 ON t1.c2 = t0.c1 ORDER BY c2",
		"SELECT * FROM t1 JOIN t0 ON t0.c1 = t1.c2 ORDER BY c2",
		"INSERT INTO t1 VALUES ('1'), (' 1'), ('1.0'), ('1x'), ('1.5'), (NULL)",
		"SELECT t0.c1, t1.c2 FROM t0 JOIN t1 ON t1.c2 = t0.c1 ORDER BY t0.c1, t1.c2",
		"SELECT t0.c1, t1.c2 FROM t1 LEFT JOIN t0 ON t0.c1 = t1.c2 ORDER BY t1.c2, t0.c1",
		"CREATE TABLE d (x DECIMAL(10,2), f DOUBLE, u BIGINT UNSIGNED, i INT)",
		"INSERT INTO d VALUES (1.00, 1, 1, 1), (1.50, 1.5, 2, -1), (2.00, 2, 3, 3)",
		"SELECT a.x, b.i FROM d a JOIN d b ON a.x = b.i ORDER BY a.x, b.i",
		"SELECT a.f, b.x FROM d a JOIN d b ON a.f = b.x ORDER BY a.f, b.x",
		"SELECT a.u, b.i FROM d a JOIN d b ON a.u = b.i ORDER BY a.u, b.i",
		"SELECT a.i, t1.c2 FROM d a JOIN t1 ON t1.c2 = a.i ORDER BY a.i, t1.c2",
		"SELECT a.x, t1.c2 FROM d a JOIN t1 ON t1.c2 = a.x ORDER BY a.x, t1.c2",
		// 2 列のキー（列ごとに比べる型を求める）
		"CREATE TABLE m (s VARCHAR(10), k INT)",
		"CREATE TABLE q (i INT, k INT)",
		"INSERT INTO m VALUES (' 3', 1), ('3abc', 1), ('3', 2), ('x', 1), ('0', 1)",
		"INSERT INTO q VALUES (3, 1), (3, 2), (0, 1)",
		"SELECT m.s, m.k, q.i FROM m JOIN q ON m.s = q.i AND m.k = q.k ORDER BY m.s, m.k, q.i",
		"SELECT m.s, m.k, q.i FROM q JOIN m ON q.i = m.s AND q.k = m.k ORDER BY m.s, m.k, q.i",
		"SELECT q.i, q.k, m.s FROM q LEFT JOIN m ON m.s = q.i AND m.k = q.k ORDER BY q.i, q.k, m.s",
		"DROP TABLE q",
		"DROP TABLE m",
		// WordPress の形: 索引のない列どうし（meta_value と ID の NOT IN、NOT EXISTS、IN）。HeimDB は整数と文字列の組の
		// キーでもハッシュ結合にする
		"CREATE TABLE p (ID BIGINT UNSIGNED NOT NULL PRIMARY KEY, post_name VARCHAR(200) NOT NULL DEFAULT '', post_type VARCHAR(20) NOT NULL DEFAULT 'post', post_parent BIGINT UNSIGNED NOT NULL DEFAULT 0, menu_order INT NOT NULL DEFAULT 0, KEY post_name (post_name(191)), KEY type_status (post_type))",
		"CREATE TABLE pm (meta_id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, post_id BIGINT UNSIGNED NOT NULL DEFAULT 0, meta_key VARCHAR(255) DEFAULT NULL, meta_value LONGTEXT, KEY post_id (post_id), KEY meta_key (meta_key(191)))",
		"SET SESSION cte_max_recursion_depth = 1000",
		"INSERT INTO p (ID, post_name, post_type, post_parent, menu_order) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) SELECT i, CONCAT('p-', i), IF(i % 2 = 0, 'attachment', 'post'), IF(i % 2 = 0, i - 1, 0), i % 7 FROM n",
		"INSERT INTO pm (post_id, meta_key, meta_value) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300), k (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j < 5) SELECT i, IF(j = 0, '_thumbnail_id', CONCAT('_k', j)), IF(j = 0, ELT(1 + (i DIV 2) % 6, CAST(i + 1 AS CHAR), CONCAT(' ', i + 1), CONCAT(i + 1, 'abc'), CONCAT(i + 1, '.0'), CONCAT(i + 1, '.5'), CONCAT('0', i + 1)), CONCAT('v', i, '-', j)) FROM n, k WHERE j > 0 OR i % 2 = 1 ORDER BY i, j",
		"ANALYZE TABLE p, pm",
		"SELECT ID FROM p WHERE post_type = 'attachment' AND ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id' AND meta_value IS NOT NULL) ORDER BY ID LIMIT 10",
		"SELECT COUNT(*) FROM p WHERE post_type = 'attachment' AND ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id' AND meta_value IS NOT NULL)",
		"SELECT COUNT(*) FROM p WHERE post_type = 'attachment' AND NOT EXISTS (SELECT 1 FROM pm WHERE pm.meta_key = '_thumbnail_id' AND pm.meta_value = p.ID)",
		"SELECT COUNT(*), SUM(ID) FROM p WHERE post_type = 'attachment' AND ID IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id')",
		"SELECT COUNT(*), SUM(ID) FROM p WHERE EXISTS (SELECT 1 FROM pm WHERE pm.meta_key = '_thumbnail_id' AND pm.meta_value = p.ID)",
		"SELECT COUNT(*), SUM(a.ID) FROM p a INNER JOIN pm ON pm.meta_value = a.ID WHERE a.ID < 100",
		"SELECT COUNT(*), SUM(p.ID) FROM p INNER JOIN pm ON pm.meta_value = p.menu_order WHERE pm.meta_key = '_thumbnail_id'",
		"UPDATE p SET menu_order = 99 WHERE post_type = 'attachment' AND NOT EXISTS (SELECT 1 FROM pm WHERE pm.meta_key = '_thumbnail_id' AND pm.meta_value = p.ID)",
		"SELECT COUNT(*), SUM(ID) FROM p WHERE menu_order = 99",
		// 単表の DELETE と OR の中の IN (SELECT ...) は、結合にせず副問い合わせの値と比べる
		"SELECT COUNT(*), SUM(ID) FROM p WHERE post_type = 'post' OR ID IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id')",
		"SELECT COUNT(*), SUM(ID) FROM p WHERE post_type = 'post' OR ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id' AND meta_value IS NOT NULL)",
		"SELECT COUNT(*) FROM pm WHERE meta_key = '_k1' OR meta_value IN (SELECT ID FROM p WHERE post_type = 'attachment')",
		"UPDATE p SET menu_order = 98 WHERE ID IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id')",
		"SELECT COUNT(*), SUM(ID) FROM p WHERE menu_order = 98",
		"DELETE FROM p WHERE post_type = 'attachment' AND ID NOT IN (SELECT meta_value FROM pm WHERE meta_key = '_thumbnail_id' AND meta_value IS NOT NULL)",
		"SELECT COUNT(*), SUM(ID) FROM p",
		"DROP TABLE pm",
		"DROP TABLE p",
		"DROP TABLE d",
		"DROP TABLE t1",
		"DROP TABLE t0",
	}},
}

// 段階 112 で取り込んだ上流の修正の後、再現の文が MySQL と同じ結果になる。
func TestStage112UpstreamFixes(t *testing.T) {
	stmts := []string{"SET SESSION sql_mode = DEFAULT", "SET time_zone = '+00:00'"}
	for _, g := range stage112Groups {
		stmts = append(stmts, g.stmts...)
	}
	checkGolden(t, "../testdata/stage112_golden.txt", "heimdb_golden112", stmts)
}

// NOT EXISTS の反結合が、右の NULL を含める種類（上流の #3135 の AntiJoinIncludingNulls と、そこから作る普通の
// LEFT JOIN）になっても、段階 80 のとおりサブクエリを 1 回読むハッシュ結合のまま（HeimDB の coster が新しい種類と、
// 反結合から作った LEFT JOIN の印を見る）。見ないと、記事ごとに post_id でメタを引く lookup JOIN か、右を記事ごとに
// 読み直す入れ子のループの反結合になる（結果は同じで、遅くなるだけ）。
func TestStage112NotExistsPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), subqueryData()...) {
		mustExec(t, db, q)
	}
	for _, c := range []struct {
		q          string
		want, deny []string
	}{
		{ // 記事ごとに post_id で引くと、記事 1 件あたりメタを 20 行読んで捨てる
			"select ID from wp_posts where post_type = 'post' and not exists (select 1 from wp_postmeta where post_id = wp_posts.ID and meta_key = 'bzb_include_rss')",
			[]string{"LeftOuterHashJoin\n", "filters: [{[bzb_include_rss, bzb_include_rss]}]"},
			[]string{"LookupJoin", "keys: wp_posts.ID"},
		},
		{ // 索引のない列の相関は、右を記事ごとに読み直さない
			"select ID from wp_posts where not exists (select 1 from wp_postmeta where wp_postmeta.meta_value = wp_posts.post_title)",
			[]string{"HashJoin"},
			[]string{"AntiJoinIncludingNulls"},
		},
		{ // NOT IN は前と同じく NULL を落とす種類のまま
			subqueryScript[1],
			[]string{"LeftOuterHashJoinExcludingNulls"},
			nil,
		},
		{subqueryScript[3], []string{"LeftOuterHashJoin\n"}, []string{"keys: wp_posts.ID"}},
		// UPDATE の中の NOT IN と NOT EXISTS も、反結合から作った LEFT JOIN のハッシュ結合（上流の #3455 のように UPDATE の
		// 中で書き換えをやめると、記事ごとに右を読み直す入れ子のループの反結合になる。記事 1,500 件で 4.7 秒と 19 ms）
		{
			"UPDATE wp_posts SET menu_order = menu_order + 1 WHERE ID NOT IN (SELECT post_id FROM wp_postmeta WHERE meta_key = 'bzb_include_rss' AND meta_value = 'none')",
			[]string{"LeftOuterHashJoinExcludingNulls"},
			[]string{"AntiJoin"},
		},
		{
			"UPDATE wp_posts SET menu_order = menu_order + 1 WHERE NOT EXISTS (SELECT 1 FROM wp_postmeta WHERE wp_postmeta.post_id = wp_posts.ID AND meta_key = 'bzb_include_rss' AND meta_value = 'none')",
			[]string{"LeftOuterHashJoin\n"},
			[]string{"AntiJoin", "keys: wp_posts.ID"},
		},
		{ // 記事のなくなったメタを消す掃除は、post_id と ID の索引のマージ結合（段階 112 の上流の #3318 で 1105 から直った）
			"DELETE FROM wp_postmeta WHERE NOT EXISTS (SELECT 1 FROM wp_posts WHERE wp_posts.ID = wp_postmeta.post_id)",
			[]string{"LeftOuterMergeJoin"},
			[]string{"AntiJoin", "NOT(EXISTS"},
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

// stage112TermData は、subqueryData の記事 300 件に、ターム 30 個（category 10、post_tag 20）と、記事ごとに 3 つの
// タームを付ける。
func stage112TermData() []string {
	return []string{
		"INSERT INTO wp_terms (term_id, name, slug) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30) SELECT i, CONCAT('t', i), CONCAT('t', i) FROM n",
		"INSERT INTO wp_term_taxonomy (term_taxonomy_id, term_id, taxonomy, description) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30) SELECT i, i, IF(i <= 10, 'category', 'post_tag'), '' FROM n",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300), k (j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM k WHERE j < 2) " +
			"SELECT i, 1 + (i * 7 + j * 11) % 30 FROM n, k",
		"ANALYZE TABLE wp_posts, wp_postmeta, wp_term_relationships, wp_term_taxonomy, wp_terms",
	}
}

// 半結合を内部結合にするとき右を Distinct で包んでも（上流の #3212）、WordPress の形の IN (SELECT ...) と EXISTS は、
// サブクエリを 1 回読んで記事を主キーで引くか、ハッシュ JOIN のまま（記事ごとにメタやタームを引き直さない）。
func TestStage112SemiJoinPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append(append([]string{wpSQLMode}, wpSchema()...), subqueryData()...), stage112TermData()...) {
		mustExec(t, db, q)
	}
	for _, c := range []struct {
		q          string
		want, deny []string
	}{
		{ // meta のキーと値で絞ったメタを 1 回読み、記事を主キーで引く
			subqueryScript[2],
			[]string{"LookupJoin\n", "Distinct", "keys: wp_postmeta.post_id"},
			[]string{"keys: wp_posts.ID", "keys: 'bzb_include_rss'"},
		},
		{subqueryScript[4], []string{"LookupJoin\n", "Distinct", "keys: wp_postmeta.post_id"}, []string{"keys: wp_posts.ID", "keys: 'bzb_include_rss'"}},
		{ // 同じ記事に 2 行当たる OR（上流の #3212 の再現の形）は、重複を除いてハッシュ JOIN
			"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.ID IN (SELECT post_id FROM wp_postmeta WHERE meta_value = 'none' OR meta_value = 'v1-1')",
			[]string{"HashJoin\n", "Distinct"},
			[]string{"keys: wp_posts.ID"},
		},
		{
			"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.post_type = 'post' AND wp_posts.post_status = 'publish' AND wp_posts.ID IN (SELECT post_id FROM wp_postmeta WHERE meta_key = '_k3') ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
			[]string{"HashJoin\n", "Distinct", "TopN"},
			[]string{"keys: wp_posts.ID", "keys: '_k3'"},
		},
		{ // タームの IN（WP_Tax_Query の形）は、タームの行を 1 回読み、記事を主キーで引く
			"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.ID IN (SELECT object_id FROM wp_term_relationships WHERE term_taxonomy_id IN (3, 4, 5)) AND wp_posts.post_type = 'post' AND wp_posts.post_status = 'publish' ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
			[]string{"LookupJoin\n", "Distinct", "keys: wp_term_relationships.object_id"},
			[]string{"keys: wp_posts.ID"},
		},
		{ // WP_Tax_Query の 'operator' => 'EXISTS'
			"SELECT wp_posts.ID FROM wp_posts WHERE 1=1 AND ( EXISTS ( SELECT 1 FROM wp_term_relationships INNER JOIN wp_term_taxonomy ON wp_term_taxonomy.term_taxonomy_id = wp_term_relationships.term_taxonomy_id WHERE wp_term_taxonomy.taxonomy = 'category' AND wp_term_relationships.object_id = wp_posts.ID ) ) AND wp_posts.post_type = 'post' AND wp_posts.post_status = 'publish' ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
			[]string{"HashJoin\n", "Distinct"},
			[]string{"keys: wp_posts.ID", "SemiJoin"},
		},
		{
			"SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.ID IN (SELECT tr.object_id FROM wp_term_relationships tr INNER JOIN wp_term_taxonomy tt ON tt.term_taxonomy_id = tr.term_taxonomy_id WHERE tt.taxonomy = 'post_tag' AND tt.term_id IN (21, 22))",
			[]string{"HashJoin\n", "Distinct"},
			[]string{"keys: wp_posts.ID", "SemiJoin"},
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

// 数と文字列の列の結合（プラグインの ID = meta_value。上流の #3343、#3423、#3200）は、行を落とさず、記事ごとにメタを
// 読み直さない。整数と文字列のキーのハッシュ結合は両辺を double にして引くので、HeimDB の coster は整数どうしと同じく
// ハッシュ結合を選べる（前は入れ子のループの反結合で、記事 3,000 件で 3.7 秒）。
func TestStage112MixedKeyPlans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	setup := append(append([]string{wpSQLMode}, wpSchema()...), subqueryData()...)
	setup = append(setup,
		// 記事ごとのアイキャッチ（_thumbnail_id）。値はほかの記事の ID の文字列
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) "+
			"SELECT i, '_thumbnail_id', CAST(1 + (i * 7) % 300 AS CHAR) FROM n",
		"ANALYZE TABLE wp_posts, wp_postmeta")
	for _, q := range setup {
		mustExec(t, db, q)
	}
	type planCase struct {
		q          string
		want, deny []string
	}
	check := func(cases []planCase) {
		t.Helper()
		for _, c := range cases {
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
	check([]planCase{
		{ // アイキャッチに使われていない添付
			"SELECT ID FROM wp_posts WHERE post_type = 'attachment' AND ID NOT IN (SELECT meta_value FROM wp_postmeta WHERE meta_key = '_thumbnail_id' AND meta_value IS NOT NULL)",
			[]string{"LeftOuterHashJoinExcludingNulls"},
			[]string{"LookupJoin", "AntiJoin"},
		},
		{
			"SELECT ID FROM wp_posts p WHERE post_type = 'attachment' AND NOT EXISTS (SELECT 1 FROM wp_postmeta pm WHERE pm.meta_key = '_thumbnail_id' AND pm.meta_value = p.ID)",
			[]string{"LeftOuterHashJoin\n"},
			[]string{"LookupJoin", "AntiJoin"},
		},
		{ // IN と EXISTS は、メタを 1 回読んで記事を主キーかハッシュ結合で引き、記事の行の重複を除く
			"SELECT ID FROM wp_posts WHERE post_type = 'attachment' AND ID IN (SELECT meta_value FROM wp_postmeta WHERE meta_key = '_thumbnail_id')",
			[]string{"Distinct\n     └─ Project"},
			[]string{"SemiLookupJoin", "keys: '_thumbnail_id'"},
		},
		{
			"SELECT ID FROM wp_posts p WHERE EXISTS (SELECT 1 FROM wp_postmeta pm WHERE pm.meta_key = '_thumbnail_id' AND pm.meta_value = p.ID)",
			[]string{"Distinct\n     └─ Project"},
			[]string{"SemiLookupJoin", "keys: '_thumbnail_id'"},
		},
		{ // アイキャッチの添付を主キーで引く形は前と同じ
			"SELECT p.ID, a.guid FROM wp_posts p INNER JOIN wp_postmeta pm ON pm.post_id = p.ID AND pm.meta_key = '_thumbnail_id' INNER JOIN wp_posts a ON a.ID = pm.meta_value WHERE p.post_type = 'post' AND p.post_status = 'publish' ORDER BY p.post_date DESC LIMIT 10",
			[]string{"keys: pm.meta_value"},
			[]string{"MergeJoin"},
		},
	})
	// meta_value の索引（Index WP MySQL For Speed の meta_value(32)）があっても、数の ID で文字列の索引を引かず、
	// ID の主キーと並べて突き合わせない
	mustExec(t, db, "ALTER TABLE wp_postmeta ADD KEY meta_value (meta_value(32))")
	mustExec(t, db, "ANALYZE TABLE wp_postmeta")
	check([]planCase{
		{
			"SELECT a.ID, pm.post_id FROM wp_posts a INNER JOIN wp_postmeta pm ON pm.meta_value = a.ID WHERE a.post_type = 'attachment' AND a.ID < 100",
			[]string{"HashJoin"},
			[]string{"MergeJoin", "keys: a.ID"},
		},
		{
			"SELECT a.ID FROM wp_posts a INNER JOIN wp_postmeta pm ON a.ID = pm.meta_value WHERE pm.meta_key = '_thumbnail_id'",
			[]string{"keys: pm.meta_value"},
			[]string{"MergeJoin", "keys: a.ID"},
		},
	})
}
