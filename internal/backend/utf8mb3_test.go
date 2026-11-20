package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// yoastCreates は、Yoast SEO（wordpress-seo）が新しいサイトで出す CREATE TABLE（ホスティング側の verify-plugin.sh で -debug の
// ログから取った。文字列の主キーの wp_yoast_expiring_store は段階 38）。DEFAULT CHARSET=utf8 を固定で出す。
var yoastCreates = []string{
	"CREATE TABLE `wp_yoast_indexable_hierarchy` ( `indexable_id` int(11) UNSIGNED, `ancestor_id` int(11) UNSIGNED, `depth` int(11) UNSIGNED, PRIMARY KEY (`indexable_id`,`ancestor_id`)) DEFAULT CHARSET=utf8;",
	"CREATE TABLE `wp_yoast_indexable` ( `id` int(11) UNSIGNED auto_increment NOT NULL, `permalink` mediumtext, `permalink_hash` varchar(191), `object_id` int(11) UNSIGNED, `object_type` varchar(32) NOT NULL, `object_sub_type` varchar(32), `author_id` int(11) UNSIGNED, `post_parent` int(11) UNSIGNED, `title` varchar(191), `description` text, `breadcrumb_title` varchar(191), `post_status` varchar(191), `is_public` tinyint(1), `is_protected` tinyint(1) DEFAULT '0', `has_public_posts` tinyint(1), `number_of_pages` int(11) UNSIGNED, `canonical` mediumtext, `primary_focus_keyword` varchar(191), `primary_focus_keyword_score` int(3), `readability_score` int(3), `is_cornerstone` tinyint(1) DEFAULT '0', `is_robots_noindex` tinyint(1) DEFAULT '0', `is_robots_nofollow` tinyint(1) DEFAULT '0', `is_robots_noarchive` tinyint(1) DEFAULT '0', `is_robots_noimageindex` tinyint(1) DEFAULT '0', `is_robots_nosnippet` tinyint(1) DEFAULT '0', `twitter_title` varchar(191), `twitter_image` mediumtext, `twitter_description` mediumtext, `twitter_image_id` varchar(191), `twitter_image_source` varchar(191), `open_graph_title` varchar(191), `open_graph_description` mediumtext, `open_graph_image` mediumtext, `open_graph_image_id` varchar(191), `open_graph_image_source` varchar(191), `open_graph_image_meta` text, `link_count` int(11), `incoming_link_count` int(11), `prominent_words_version` int(11) UNSIGNED, PRIMARY KEY (`id`)) DEFAULT CHARSET=utf8;",
	"CREATE TABLE `wp_yoast_migrations` ( `id` int(11) UNSIGNED auto_increment NOT NULL, `version` varchar(191), PRIMARY KEY (`id`)) DEFAULT CHARSET=utf8;",
	"CREATE TABLE `wp_yoast_primary_term` ( `id` int(11) UNSIGNED auto_increment NOT NULL, `post_id` int(11) UNSIGNED NOT NULL, `term_id` int(11) UNSIGNED NOT NULL, `taxonomy` varchar(32) NOT NULL, PRIMARY KEY (`id`)) DEFAULT CHARSET=utf8;",
	"CREATE TABLE `wp_yoast_seo_links` ( `id` bigint(20) UNSIGNED auto_increment, `url` varchar(255), `post_id` bigint(20) UNSIGNED, `target_post_id` bigint(20) UNSIGNED, `type` varchar(8), PRIMARY KEY (`id`)) DEFAULT CHARSET=utf8;",
}

// utf8mb3 の表と列は、utf8mb4 の同じ名前の照合順序として受け入れ、警告を出す（docs/plan.md の段階 37）。
func TestUTF8MB3StoredAsUTF8MB4(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	conn := func() *sql.Conn {
		c, err := m.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := conn()
	defer c.Close()
	// warned は、直前の文の警告に want が含まれるかを確かめる。
	warned := func(q, want string) {
		t.Helper()
		if _, err := c.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows, err := c.QueryContext(t.Context(), "SHOW WARNINGS")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var all []string
		for rows.Next() {
			var level, msg string
			var code int
			if err := rows.Scan(&level, &code, &msg); err != nil {
				t.Fatal(err)
			}
			all = append(all, msg)
		}
		if !strings.Contains(strings.Join(all, "\n"), want) {
			t.Errorf("%s: warnings %q, want %q", q, all, want)
		}
	}
	collations := func(table string) string {
		t.Helper()
		rows, err := m.Query("SELECT COLUMN_NAME, COLLATION_NAME FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND COLLATION_NAME IS NOT NULL ORDER BY ORDINAL_POSITION", table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var col, coll string
			if err := rows.Scan(&col, &coll); err != nil {
				t.Fatal(err)
			}
			out = append(out, col+"="+coll)
		}
		return one(t, m, "SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '"+table+"'") + " " + strings.Join(out, " ")
	}

	// Yoast の表は utf8mb4_general_ci になり、後で Yoast が出す CONVERT TO も通る。
	for _, q := range yoastCreates {
		warned(q, "character set utf8mb3 is stored as utf8mb4 with the same collation name")
	}
	if got := collations("wp_yoast_migrations"); got != "utf8mb4_general_ci version=utf8mb4_general_ci" {
		t.Errorf("wp_yoast_migrations: %s", got)
	}
	for _, table := range []string{"wp_yoast_indexable", "wp_yoast_indexable_hierarchy", "wp_yoast_migrations", "wp_yoast_primary_term"} {
		mustExec(t, m, "ALTER TABLE "+table+" CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci")
	}
	mustExec(t, m, "INSERT INTO wp_yoast_migrations (version) VALUES ('20171228151840')")
	if got := one(t, m, "SELECT version FROM wp_yoast_migrations"); got != "20171228151840" {
		t.Errorf("version = %s", got)
	}

	// 照合順序は名前どおり（文字コードの部分だけ utf8mb4 にする）。
	mustExec(t, m, "CREATE TABLE u1 (id int NOT NULL PRIMARY KEY, g varchar(20) COLLATE utf8mb3_general_ci, b varchar(20) CHARACTER SET utf8 COLLATE utf8_bin, u text CHARACTER SET utf8mb3 COLLATE utf8mb3_unicode_ci, n varchar(20)) DEFAULT CHARSET=utf8mb4")
	if got := collations("u1"); got != "utf8mb4_0900_ai_ci g=utf8mb4_general_ci b=utf8mb4_bin u=utf8mb4_unicode_ci n=utf8mb4_0900_ai_ci" {
		t.Errorf("u1: %s", got)
	}
	// 比較の意味は utf8mb3 のまま（general_ci は濁点を区別し、大小を区別しない）。
	mustExec(t, m, "INSERT INTO u1 (id, g, b) VALUES (1, 'は', 'A')")
	if got := one(t, m, "SELECT COUNT(*) FROM u1 WHERE g = 'ば'"); got != "0" {
		t.Errorf("general_ci treats は and ば as equal: %s", got)
	}
	if got := one(t, m, "SELECT COUNT(*) FROM u1 WHERE b = 'a'"); got != "0" {
		t.Errorf("bin ignores case: %s", got)
	}

	// 列の追加と変更、既定の照合順序の変更。既定を utf8 にした後に文字コードを書かずに足した列も utf8mb4 になる。
	warned("ALTER TABLE u1 ADD COLUMN a varchar(10) CHARACTER SET utf8", "column a")
	warned("ALTER TABLE u1 MODIFY n varchar(20) CHARACTER SET utf8", "column n")
	warned("ALTER TABLE u1 DEFAULT CHARACTER SET utf8", "the table default")
	if got := one(t, m, "SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'u1'"); got != "utf8mb4_general_ci" {
		t.Errorf("table default after ALTER: %s", got)
	}
	mustExec(t, m, "ALTER TABLE u1 ADD COLUMN later varchar(10)")
	if got := collations("u1"); got != "utf8mb4_general_ci g=utf8mb4_general_ci b=utf8mb4_bin u=utf8mb4_unicode_ci n=utf8mb4_general_ci a=utf8mb4_general_ci later=utf8mb4_general_ci" {
		t.Errorf("u1 after ALTER: %s", got)
	}
	if got := one(t, m, "SELECT g FROM u1 WHERE id = 1"); got != "は" {
		t.Errorf("value after rewriting: %s", got)
	}

	// MySQL の mysqldump の形（utf8mb3 と書く）をデータごと取り込める。
	mustExec(t, m, "CREATE TABLE dumped (id bigint unsigned NOT NULL AUTO_INCREMENT, name varchar(64) NOT NULL DEFAULT '', PRIMARY KEY (id), UNIQUE KEY name (name)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci")
	mustExec(t, m, "INSERT INTO dumped VALUES (1,'はな'),(2,'ばな'),(3,'Hana')")
	if _, err := m.Exec("INSERT INTO dumped VALUES (4,'hana')"); err == nil {
		t.Error("general_ci unique key accepted a case-only duplicate")
	}
	if got := one(t, m, "SELECT COUNT(*) FROM dumped"); got != "3" {
		t.Errorf("dumped rows: %s", got)
	}
	var name, stmt string
	if err := m.QueryRow("SHOW CREATE TABLE dumped").Scan(&name, &stmt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stmt, "utf8mb3") || !strings.Contains(stmt, "utf8mb4_general_ci") {
		t.Errorf("SHOW CREATE TABLE:\n%s", stmt)
	}

	// latin1、utf16、対応する utf8mb4 の照合順序がない utf8mb3 は、これまでどおり拒否する（ascii は段階 57 から受け入れる）。
	for q, want := range map[string]string{
		"CREATE TABLE l1 (id int NOT NULL PRIMARY KEY, b varchar(20)) DEFAULT CHARSET=latin1":                              "uses character set latin1",
		"CREATE TABLE a1 (id int NOT NULL PRIMARY KEY, b varchar(20) CHARACTER SET utf16)":                                 "uses character set utf16",
		"CREATE TABLE t1 (id int NOT NULL PRIMARY KEY, b varchar(20)) DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_tolower_ci":  "has no utf8mb4 counterpart",
		"CREATE TABLE t2 (id int NOT NULL PRIMARY KEY, b varchar(20) CHARACTER SET utf8 COLLATE utf8_general_mysql500_ci)": "has no utf8mb4 counterpart",
	} {
		if _, err := m.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
}
