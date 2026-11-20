package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// diffRows は heimdb_branch_diff の結果を「列|列|…」の行にする。行ごとの差分では、段階 63 で足した最後の 3 列
// （resolved_by、resolved_at、conflict_values）を除く（diffRowsAll）。
func diffRows(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	return queryRows(t, db, q, true)
}

// diffRowsAll は、diffRows と同じで、すべての列を返す。
func diffRowsAll(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	return queryRows(t, db, q, false)
}

func queryRows(t *testing.T, db *sql.DB, q string, trim bool) []string {
	t.Helper()
	var out []string
	for _, r := range queryCells(t, db, q, "") {
		if trim && len(r) == len(diffRowSchema) {
			r = r[:len(r)-3]
		}
		out = append(out, strings.Join(r, "|"))
	}
	return out
}

const insertPost = "INSERT INTO wp_posts (ID, post_author, post_title, post_status, post_type, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES "

func TestBranchDiff(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')",
		"INSERT INTO wp_users (ID, user_login, user_registered) VALUES (1, 'admin', '2026-01-01 00:00:00')",
		insertPost + "(2, 1, 'About', 'publish', 'page', 'c', '', '', '', ''), (3, 1, 'Contact', 'publish', 'page', 'c', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (2, '_edit_lock', '1:1')",
	} {
		mustExec(t, mainDB, q)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()

	for _, q := range []string{
		insertPost + "(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{}', '', '', '', '')",
		"UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE post_type = 'wp_global_styles'",
		"INSERT INTO wp_terms (name, slug) VALUES ('x', 'x')",
		"UPDATE wp_options SET option_value = 'Staging' WHERE option_name = 'blogname'",
		"UPDATE wp_options SET option_value = 'http://staging.example' WHERE option_name = 'siteurl'",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('plugin_x', 'branch'), ('_transient_foo', 'x')",
		"UPDATE wp_posts SET post_title = 'About v2' WHERE ID = 2",
		"UPDATE wp_posts SET post_title = 'Contact (staging)' WHERE ID = 3",
		"DELETE FROM wp_posts WHERE ID = 1",
		insertPost + "(NULL, 1, 'New page', 'publish', 'page', 'c', '', '', '', '')",
		insertPost + "(NULL, 1, 'Auto Draft', 'auto-draft', 'post', '', '', '', '', '')",
		insertPost + "(NULL, 7, 'Ghost author', 'publish', 'post', '', '', '', '', '')",
		"UPDATE wp_postmeta SET meta_value = '2:1' WHERE post_id = 2",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (3, 'color', 'red')",
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content) VALUES (2, 'a', 'from staging')",
		"CREATE TABLE wp_custom (id bigint unsigned NOT NULL PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_custom VALUES (1, 'a')",
	} {
		mustExec(t, staging, q)
	}
	for _, q := range []string{
		insertPost + "(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{\"x\":1}', '', '', '', '')", // 中身が違うので競合
		"UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE post_type = 'wp_global_styles'",
		"INSERT INTO wp_terms (name, slug) VALUES ('X', 'x')",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('plugin_x', 'parent')",
		"UPDATE wp_posts SET post_excerpt = 'prod excerpt', comment_count = 1 WHERE ID = 2",
		"UPDATE wp_posts SET post_title = 'Contact (prod)' WHERE ID = 3",
		"INSERT INTO wp_comments (comment_post_ID, comment_author, comment_content) VALUES (1, 'a', 'on deleted post')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (3, 'color', 'blue')",
	} {
		mustExec(t, mainDB, q)
	}

	summary := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"), "\n")
	wantSummary := strings.Join([]string{
		"wp_comments|changed|0|0|0|1|0|0|0|",
		"wp_custom|new table|1|0|0|0|0|0|0|",
		"wp_options|changed|0|1|0|2|1|0|0|",
		"wp_postmeta|changed|1|0|0|1|0|0|1|",
		"wp_posts|changed|1|1|1|1|3|0|1|",
		"wp_terms|changed|0|0|0|0|1|0|0|",
	}, "\n")
	if summary != wantSummary {
		t.Errorf("summary:\n%s\nwant:\n%s", summary, wantSummary)
	}

	posts := strings.Join(diffRows(t, staging, "CALL heimdb_branch_diff('staging', 'wp_posts')"), "\n")
	for _, want := range []string{
		"wp_posts|ID=1|delete|none|delete|||",
		"wp_posts|ID=2|update|update|update||post_title|",
		"wp_posts|ID=3|update|update|conflict||post_title|the same columns were changed on both sides",
		// ステージングで追加した行は、ブランチの番号帯（10^10 から）の ID になる。
		"wp_posts|ID=10000000000|insert|insert|conflict|||post_type=wp_global_styles,post_name=wp-global-styles-x was also added on the parent as ID=4",
		"wp_posts|ID=10000000001|insert|none|insert|||",
		"wp_posts|ID=10000000002|insert|none|skip|||auto-draft",
		"wp_posts|ID=10000000003|insert|none|conflict||post_author|author 7 does not exist on the parent",
		"wp_posts|ID=1|||warning|||1 comments on the parent belong to this deleted post",
	} {
		if !strings.Contains(posts, want) {
			t.Errorf("posts diff lacks %q:\n%s", want, posts)
		}
	}
	options := strings.Join(diffRows(t, staging, "CALL heimdb_branch_diff('staging', 'wp_options')"), "\n")
	for _, want := range []string{
		"wp_options|option_name=_transient_foo|insert|none|skip|||transient",
		"wp_options|option_name=blogname|update|none|update||option_value|",
		"wp_options|option_name=plugin_x|insert|insert|conflict||option_value|added on both sides with different values",
		"wp_options|option_name=siteurl|update|none|skip|||environment-specific option",
	} {
		if !strings.Contains(options, want) {
			t.Errorf("options diff lacks %q:\n%s", want, options)
		}
	}
	all := diffRows(t, staging, "CALL heimdb_branch_diff('staging', '*')")
	if len(all) != 18 {
		t.Errorf("all rows: %d\n%s", len(all), strings.Join(all, "\n"))
	}

	// db_version が違えば、差分の要約は最初の行で知らせ（段階 44）、マージはエラー。
	mustExec(t, staging, "UPDATE wp_options SET option_value = '60800' WHERE option_name = 'db_version'")
	versionRows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')")
	if want := "*|db_version differs|0|0|0|0|0|0|0|branch 60800, parent 60717; run the WordPress database upgrade on both sides before merging"; len(versionRows) < 2 || versionRows[0] != want {
		t.Errorf("db_version mismatch:\n%s", strings.Join(versionRows, "\n"))
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "db_version differs") {
		t.Errorf("merge with db_version mismatch: %v", err)
	}
	// 行ごとの差分は、知らせる行がないのでエラーのまま。
	if _, err := mainDB.Exec("CALL heimdb_branch_diff('staging', '*')"); err == nil || !strings.Contains(err.Error(), "db_version differs") {
		t.Errorf("row diff with db_version mismatch: %v", err)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging', 'dry_run')"); err == nil || !strings.Contains(err.Error(), "db_version differs") {
		t.Errorf("dry run with db_version mismatch: %v", err)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_diff('main')"); err == nil {
		t.Error("diff of main should fail")
	}
}

// ブランチでテーブルを書き直した（TRUNCATE、列を変える ALTER）ときの差分。
func TestBranchDiffRewrittenTables(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_terms (term_id, name, slug) VALUES (1, 'a', 'a'), (2, 'b', 'b')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "TRUNCATE wp_terms")
	mustExec(t, staging, "INSERT INTO wp_terms (term_id, name, slug) VALUES (2, 'b', 'b'), (3, 'c', 'c')")
	mustExec(t, staging, "ALTER TABLE wp_links ADD COLUMN extra int")
	mustExec(t, staging, "DROP TABLE wp_termmeta")

	got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"), "\n")
	want := strings.Join([]string{
		"wp_links|changed|0|0|0|0|0|0|0|columns: +extra", // 列の追加はマージできる（段階 13）
		"wp_termmeta|dropped on branch|0|0|0|0|0|0|0|dropping tables is not merged",
		"wp_terms|changed|1|0|1|0|0|0|0|full compare 3 rows", // TRUNCATE で書き直したので、前後の全行（1、2、3）を比べる
	}, "\n")
	if got != want {
		t.Errorf("summary:\n%s\nwant:\n%s", got, want)
	}
	terms := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_terms')"), "\n")
	if terms != "wp_terms|term_id=1|delete|none|delete|||\nwp_terms|term_id=3|insert|none|insert|||" {
		t.Errorf("terms:\n%s", terms)
	}
}

// TestDuplicateSameAsParent は、両側で同じ自然なキーの記事を追加し、中身が同じなら競合にせずに飛ばすことを確かめる
// （段階 64。ブロックテーマのサイトで、両側を開くと WordPress がナビゲーションを 1 件ずつ作る）。タームは中身が同じでも
// 競合のまま。前のマージで親に作らなかった記事やタームに、後から足した行は親に入れない。
func TestDuplicateSameAsParent(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, mainDB, "INSERT INTO wp_users (ID, user_login, user_registered) VALUES (1, 'admin', '2026-01-01 00:00:00')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()

	const nav = "INSERT INTO wp_posts (ID, post_author, post_title, post_status, post_type, post_name, post_content, guid, post_date, post_date_gmt, post_modified, post_modified_gmt, post_excerpt, to_ping, pinged, post_content_filtered) VALUES "
	for _, q := range []string{
		nav + "(NULL, 1, 'Navigation', 'publish', 'wp_navigation', 'navigation', '<!-- wp:page-list /-->', 'http://stg/?p=1', '2026-10-19 02:33:43', '2026-10-19 02:33:43', '2026-10-19 02:33:43', '2026-10-19 02:33:43', '', '', '', '')",
		nav + "(NULL, 1, 'Menu', 'publish', 'wp_navigation', 'menu', '<!-- wp:navigation-link {\"label\":\"About\"} /-->', 'http://stg/?p=2', '2026-10-19 02:33:43', '2026-10-19 02:33:43', '2026-10-19 02:33:43', '2026-10-19 02:33:43', '', '', '', '')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000000, 'note', 'stg')",
		"INSERT INTO wp_terms (name, slug) VALUES ('News', 'news')",
	} {
		mustExec(t, staging, q)
	}
	for _, q := range []string{
		nav + "(50, 1, 'Navigation', 'publish', 'wp_navigation', 'navigation', '<!-- wp:page-list /-->', 'http://prod/?p=50', '2026-10-19 02:33:27', '2026-10-19 02:33:27', '2026-10-19 02:33:27', '2026-10-19 02:33:27', '', '', '', '')",
		nav + "(51, 1, 'Menu', 'publish', 'wp_navigation', 'menu', '<!-- wp:page-list /-->', 'http://prod/?p=51', '2026-10-19 02:33:27', '2026-10-19 02:33:27', '2026-10-19 02:33:27', '2026-10-19 02:33:27', '', '', '', '')",
		"INSERT INTO wp_terms (name, slug) VALUES ('News', 'news')",
	} {
		mustExec(t, mainDB, q)
	}

	got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
	for _, want := range []string{
		"wp_posts|ID=10000000000|insert|insert|skip|||same as the parent's ID=50",
		"wp_posts|ID=10000000001|insert|insert|conflict|||post_type=wp_navigation,post_name=menu was also added on the parent as ID=51",
		"|insert|none|skip|||belongs to a post that is not merged",
		"wp_terms|term_id=10000000000|insert|insert|conflict|||slug=news was also added on the parent as term_id=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}

	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=10000000001', 'parent')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_terms', 'term_id=10000000000', 'parent')")
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
	for q, want := range map[string]string{
		"SELECT GROUP_CONCAT(ID ORDER BY ID) FROM wp_posts WHERE post_type = 'wp_navigation'": "50,51",
		"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'note'":                            "0",
		"SELECT COUNT(*) FROM wp_terms WHERE slug = 'news'":                                   "1",
	} {
		var v string
		if err := mainDB.QueryRow(q).Scan(&v); err != nil || v != want {
			t.Errorf("%s = %q, %v; want %q", q, v, err, want)
		}
	}
	// マージの後に、親に作らなかった記事とタームへ行を足しても、親には入れない（孤児になる）。
	mustExec(t, staging, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (10000000000, 'later', 'x')")
	mustExec(t, staging, "INSERT INTO wp_term_taxonomy (term_id, taxonomy, description) VALUES (10000000000, 'category', '')")
	got = strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
	for _, want := range []string{
		"wp_postmeta|meta_id=10000000001|insert|none|skip|||belongs to a post that is not on the parent",
		"wp_term_taxonomy|term_taxonomy_id=10000000000|insert|none|skip|||belongs to a term that is not on the parent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff after the merge lacks %q:\n%s", want, got)
		}
	}
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
	for q, want := range map[string]string{
		"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'later'":         "0",
		"SELECT COUNT(*) FROM wp_term_taxonomy WHERE term_id = 10000000000": "0",
	} {
		var v string
		if err := mainDB.QueryRow(q).Scan(&v); err != nil || v != want {
			t.Errorf("%s = %q, %v; want %q", q, v, err, want)
		}
	}
	// マージの後にブランチで編集すると、重複の競合に戻る（親の行と見比べる）。
	mustExec(t, staging, "UPDATE wp_posts SET post_content = '<!-- wp:navigation-link /-->' WHERE ID = 10000000000")
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_posts')"), "\n"); !strings.Contains(got, "wp_posts|ID=10000000000|insert|insert|conflict|||post_type=wp_navigation,post_name=navigation was also added on the parent as ID=50") {
		t.Errorf("edited after the merge:\n%s", got)
	}
}

// TestBranchDiffEmptyTable は、2 つの引数の heimdb_branch_diff で、表の名前に空の文字列を渡すと、内部エラーではなく明示的な
// エラーを返すことを確かめる（要約は 1 つの引数で呼ぶ）。
func TestBranchDiffEmptyTable(t *testing.T) {
	mainDB, _, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	if _, err := mainDB.Exec("CALL heimdb_branch_diff('staging', '')"); err == nil || !strings.Contains(err.Error(), "HeimDB: name a table (or '*'); for the summary, call heimdb_branch_diff(name)") {
		t.Errorf("got %v", err)
	}
}
