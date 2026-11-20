package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mergeFixture は、本番（main）とステージングの WordPress の典型的な作業を再現する。
func mergeFixture(t *testing.T) (mainDB, staging *sql.DB, stop func()) {
	t.Helper()
	m, dsn, stopServer := branchServer(t)
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717'), ('rewrite_rules', 'a:1:{}')",
		"INSERT INTO wp_users (ID, user_login, user_registered) VALUES (1, 'admin', '2026-01-01 00:00:00')",
		insertPost + "(2, 1, 'About', 'publish', 'page', 'about v1', '', '', '', ''), (3, 1, 'Contact', 'publish', 'page', 'c', '', '', '', '')",
		"INSERT INTO wp_terms (term_id, name, slug) VALUES (1, 'Uncategorized', 'uncategorized')",
		"INSERT INTO wp_term_taxonomy (term_taxonomy_id, term_id, taxonomy, description, count) VALUES (1, 1, 'category', '', 1)",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (3, 1)",
	} {
		mustExec(t, m, q)
	}
	mustExec(t, m, "CALL heimdb_branch_create('staging')")
	s := openBranch(t, dsn, "staging")
	return m, s, func() {
		s.Close()
		stopServer()
	}
}

func TestBranchMerge(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	for _, q := range []string{
		"UPDATE wp_posts SET post_content = 'about v2' WHERE ID = 2",
		"UPDATE wp_options SET option_value = 'Staged' WHERE option_name = 'blogname'",
		insertPost + "(NULL, 1, 'Pricing', 'publish', 'page', 'p', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000000, 'hero', 'red')",
		insertPost + "(NULL, 1, 'Auto Draft', 'auto-draft', 'post', '', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000001, '_wp_page_template', 'x')",
		"INSERT INTO wp_terms (name, slug) VALUES ('News', 'news')",
		"INSERT INTO wp_term_taxonomy (term_id, taxonomy, description, count) VALUES (10000000000, 'category', '', 1)",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (10000000000, 10000000000), (10000000001, 1)",
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content) VALUES (2, 'a', 'from staging')",
		"CREATE TABLE wp_custom (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_custom (v) VALUES ('a')",
	} {
		mustExec(t, staging, q)
	}
	// その間に本番でコメントが付き、同じページの抜粋が変わった。
	for _, q := range []string{
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content, comment_approved) VALUES (2, 'reader', 'nice', '1')",
		"UPDATE wp_posts SET post_excerpt = 'prod excerpt', comment_count = 1 WHERE ID = 2",
	} {
		mustExec(t, mainDB, q)
	}

	merged := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	// 最後の行は戻すための文（TestMergeUndo）。書かなかったコメントは入らない。
	if undo := lastRow(merged); !strings.HasPrefix(undo, "*|undo|") || !strings.HasSuffix(undo, "', 'wp_custom,wp_options,wp_postmeta,wp_posts,wp_term_relationships,wp_term_taxonomy,wp_terms')") {
		t.Errorf("undo: %s", undo)
	}
	got := strings.Join(merged[:len(merged)-1], "\n")
	want := strings.Join([]string{
		"wp_comments|merged|0|0|0|1|0|0|0|",
		"wp_custom|merged|1|0|0|0|0|0|0|",
		"wp_options|merged|0|1|0|0|0|0|0|",
		"wp_postmeta|merged|1|0|0|1|0|0|0|",
		"wp_posts|merged|1|1|0|1|0|0|0|",
		"wp_term_relationships|merged|1|0|0|1|0|0|0|",
		"wp_term_taxonomy|merged|1|0|0|0|0|0|0|",
		"wp_terms|merged|1|0|0|0|0|0|0|",
	}, "\n")
	if got != want {
		t.Errorf("merge:\n%s\nwant:\n%s", got, want)
	}
	for q, want := range map[string]string{
		"SELECT CONCAT(post_content, '/', post_excerpt, '/', comment_count) FROM wp_posts WHERE ID = 2": "about v2/prod excerpt/1",
		"SELECT COUNT(*) FROM wp_comments":                                                                          "1",
		"SELECT CONCAT(post_title, '/', comment_count) FROM wp_posts WHERE ID = 10000000000":                        "Pricing/0",
		"SELECT COUNT(*) FROM wp_posts WHERE post_status = 'auto-draft'":                                            "0",
		"SELECT GROUP_CONCAT(meta_key) FROM wp_postmeta":                                                            "hero",
		"SELECT option_value FROM wp_options WHERE option_name = 'blogname'":                                        "Staged",
		"SELECT option_value FROM wp_options WHERE option_name = 'rewrite_rules'":                                   "",
		"SELECT GROUP_CONCAT(CONCAT(term_taxonomy_id, ':', count) ORDER BY term_taxonomy_id) FROM wp_term_taxonomy": "1:1,10000000000:1",
		"SELECT COUNT(*) FROM wp_term_relationships":                                                                "2",
		"SELECT v FROM wp_custom":                                                                                   "a",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	// 本番の AUTO_INCREMENT は本番の番号帯のまま。
	mustExec(t, mainDB, insertPost+"(NULL, 1, 'Prod post', 'publish', 'post', '', '', '', '', '')")
	if got := one(t, mainDB, "SELECT MAX(ID) FROM wp_posts WHERE ID < 10000000000"); got != "4" {
		t.Errorf("main auto increment: %s", got)
	}

	// マージの後は差分がなく、次のマージは新しい基準から比べる。
	if rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"); len(rows) != 0 {
		t.Errorf("diff after merge:\n%s", strings.Join(rows, "\n"))
	}
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'Pricing v2' WHERE ID = 10000000000")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_content = 'p (prod)' WHERE ID = 10000000000")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT CONCAT(post_title, '/', post_content) FROM wp_posts WHERE ID = 10000000000"); got != "Pricing v2/p (prod)" {
		t.Errorf("second merge: %s", got)
	}
}

func TestBranchMergeConflicts(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	for _, q := range []string{
		"UPDATE wp_posts SET post_title = 'Contact (staging)', post_content = 'staging body' WHERE ID = 3",
		insertPost + "(NULL, 9, 'Ghost', 'publish', 'post', '', '', '', '', '')",
		insertPost + "(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{\"staging\":1}', '', '', '', '')",
		"UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE post_type = 'wp_global_styles'",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000001, 'm', 'v')",
		"ALTER TABLE wp_links ADD COLUMN extra int",
	} {
		mustExec(t, staging, q)
	}
	for _, q := range []string{
		"UPDATE wp_posts SET post_title = 'Contact (prod)' WHERE ID = 3",
		insertPost + "(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{\"prod\":1}', '', '', '', '')",
		"UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE post_type = 'wp_global_styles'",
	} {
		mustExec(t, mainDB, q)
	}
	before := one(t, mainDB, "SELECT COUNT(*) FROM wp_posts")
	_, err := mainDB.Exec("CALL heimdb_branch_merge('staging')")
	if err == nil || !strings.Contains(err.Error(), "unresolved conflicts: wp_posts ID=3, wp_posts ID=10000000000 (only 'parent' can resolve this: the author does not exist on the parent), wp_posts ID=10000000001;") {
		t.Fatalf("merge with conflicts: %v", err)
	}
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_posts"); got != before {
		t.Fatalf("a refused merge wrote rows")
	}

	// 著者がいない記事はブランチの側を採れない。
	if _, err := mainDB.Exec("CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=10000000000', 'branch')"); err == nil || !strings.Contains(err.Error(), "cannot take the branch's side") {
		t.Errorf("forbidden resolution: %v", err)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=2', 'branch')"); err == nil {
		t.Error("resolving a row that is not a conflict should fail")
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=3', 'branch')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=10000000001', 'branch')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', '*', 'parent')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_links', '*', 'parent')")

	// 解決した後に行が変わったら、その解決は使わない。
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'Contact (staging 2)' WHERE ID = 3")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_posts')"), "\n")
	for _, want := range []string{
		"wp_posts|ID=3|update|update|conflict|stale|post_title|",
		"wp_posts|ID=10000000000|insert|none|conflict|parent|post_author|",
		"wp_posts|ID=10000000001|insert|insert|conflict|branch||",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("diff lacks %q:\n%s", want, rows)
		}
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "ID=3 (stale resolution)") {
		t.Fatalf("merge with a stale resolution: %v", err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=3', 'branch')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")

	for q, want := range map[string]string{
		"SELECT CONCAT(post_title, '/', post_content) FROM wp_posts WHERE ID = 3":                               "Contact (staging 2)/staging body",
		"SELECT COUNT(*) FROM wp_posts WHERE post_author = 9":                                                   "0",
		"SELECT GROUP_CONCAT(CONCAT(ID, ':', post_content)) FROM wp_posts WHERE post_type = 'wp_global_styles'": "4:{\"staging\":1}",
		"SELECT COUNT(*) FROM wp_postmeta":                                                                      "0",
		// ブランチで足した列は、競合でなければマージされる（段階 13。wp_links の '*' の解決は競合にだけ効く）。
		"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'wp_links' AND column_name = 'extra' AND table_schema = 'wordpress'": "1",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	// 解決の記録はマージで消える。
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'again' WHERE ID = 3")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'again (prod)' WHERE ID = 3")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "unresolved conflicts: wp_posts ID=3") {
		t.Errorf("resolutions should not survive a merge: %v", err)
	}
}

// 親の書き込みロックを持ったトランザクションの中からはマージできない（自分を待って止まるため）。
func TestBranchMergeInsideTransaction(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'x' WHERE ID = 2")
	c, err := mainDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	for _, q := range []string{"BEGIN", "UPDATE wp_posts SET post_content = 'y' WHERE ID = 3"} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ExecContext(ctx, "CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "commit or roll back") {
		t.Errorf("merge inside a transaction: %v", err)
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "CALL heimdb_branch_merge('staging')"); err != nil {
		t.Errorf("merge after commit: %v", err)
	}
}

// マージの間も、本番とステージングへの書き込みは（本番はロックを待って）続けられる。
func TestBranchMergeWhileWriting(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 2)
	writer := func(db *sql.DB, q func(i int) string) {
		for i := 0; ; i++ {
			if ctx.Err() != nil {
				errs <- nil
				return
			}
			if _, err := db.Exec(q(i)); err != nil {
				errs <- fmt.Errorf("%s: %w", q(i), err)
				return
			}
		}
	}
	go writer(mainDB, func(i int) string {
		return fmt.Sprintf("INSERT INTO wp_options (option_name, option_value) VALUES ('prod_%d', 'v')", i)
	})
	go writer(staging, func(i int) string {
		return fmt.Sprintf("UPDATE wp_posts SET post_content = 'staging %d' WHERE ID = 2", i)
	})
	for i := 0; i < 5; i++ {
		if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
			t.Errorf("merge %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if a, b := one(t, mainDB, "SELECT post_content FROM wp_posts WHERE ID = 2"), one(t, staging, "SELECT post_content FROM wp_posts WHERE ID = 2"); a != b {
		t.Errorf("after the last merge: main %q, staging %q", a, b)
	}
}
