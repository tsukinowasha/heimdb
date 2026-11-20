package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// 段階 122: 全件を読む WHERE の比べで、行ごとに式の型を調べない（comparison.go の useEnumSetText、core.go の
// convertToBool、comparison.go の quietTextDoubles）。段階 121 の測り（docs/bench.md）で遅くなっていた
// ページの文を、WordPress と同じ表と接続の照合順序で流す。

// stage122Statements は、段階 121 の測りで長くなっていた文（ダッシュボードの自分のコメントの数、承認済みの
// コメントの数、投稿一覧の年と月、トップページの記事の一覧）。
var stage122Statements = []struct{ name, query string }{
	{"own_comments", "SELECT COUNT(*) FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') AND user_id = 1"},
	{"approved_comments", "SELECT COUNT(*) FROM wp_comments WHERE ( comment_approved = '1' ) AND comment_type NOT IN ('note')"},
	{"post_months", "SELECT DISTINCT YEAR( post_date ) AS year, MONTH( post_date ) AS month FROM wp_posts WHERE post_type = 'post' AND post_status != 'auto-draft' AND post_status != 'trash' ORDER BY post_date DESC"},
	{"front_page", "SELECT SQL_CALC_FOUND_ROWS  wp_posts.ID\n\t\t\t\t\t FROM wp_posts \n\t\t\t\t\t WHERE 1=1  AND ((wp_posts.post_type = 'post' AND (wp_posts.post_status = 'publish' \nOR wp_posts.post_status = 'private')))\n\t\t\t\t\t \n\t\t\t\t\t ORDER BY wp_posts.post_date DESC\n\t\t\t\t\t LIMIT 0, 10"},
}

// stage122Fixture は、WordPress の wp_comments と wp_posts（主な列と索引）に comments 件と posts 件を入れ、
// WordPress と同じ SET NAMES の接続を返す。
func stage122Fixture(tb testing.TB, comments, posts int) (*sql.Conn, func()) {
	tb.Helper()
	addr, _, st, srv := serve(tb, tb.TempDir(), serverConfig{})
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", addr, testDB))
	if err != nil {
		tb.Fatal(err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	exec := func(q string) {
		tb.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			tb.Fatalf("%s: %v", q, err)
		}
	}
	exec("SET NAMES 'utf8mb4' COLLATE 'utf8mb4_unicode_520_ci'")
	exec("SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'")
	exec("CREATE TABLE wp_comments (comment_ID bigint(20) unsigned NOT NULL auto_increment, comment_post_ID bigint(20) unsigned NOT NULL default '0', comment_author tinytext NOT NULL, comment_author_email varchar(100) NOT NULL default '', comment_date datetime NOT NULL default '0000-00-00 00:00:00', comment_date_gmt datetime NOT NULL default '0000-00-00 00:00:00', comment_content text NOT NULL, comment_approved varchar(20) NOT NULL default '1', comment_type varchar(20) NOT NULL default 'comment', comment_parent bigint(20) unsigned NOT NULL default '0', user_id bigint(20) unsigned NOT NULL default '0', PRIMARY KEY (comment_ID), KEY comment_post_ID (comment_post_ID), KEY comment_approved_date_gmt (comment_approved,comment_date_gmt), KEY comment_date_gmt (comment_date_gmt), KEY comment_parent (comment_parent), KEY comment_author_email (comment_author_email(10))) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	exec("CREATE TABLE wp_posts (ID bigint(20) unsigned NOT NULL auto_increment, post_author bigint(20) unsigned NOT NULL default '0', post_date datetime NOT NULL default '0000-00-00 00:00:00', post_content longtext NOT NULL, post_title text NOT NULL, post_status varchar(20) NOT NULL default 'publish', post_name varchar(200) NOT NULL default '', post_parent bigint(20) unsigned NOT NULL default '0', post_type varchar(20) NOT NULL default 'post', PRIMARY KEY (ID), KEY post_name (post_name(191)), KEY type_status_date (post_type,post_status,post_date,ID), KEY post_parent (post_parent), KEY post_author (post_author)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	approved := []string{"1", "1", "1", "0", "spam", "trash"}
	types := []string{"comment", "comment", "comment", "note", "pingback"}
	for i := 0; i < comments; i += 500 {
		var b strings.Builder
		b.WriteString("INSERT INTO wp_comments (comment_post_ID, comment_author, comment_author_email, comment_date, comment_date_gmt, comment_content, comment_approved, comment_type, user_id) VALUES ")
		for j := i; j < i+500 && j < comments; j++ {
			if j > i {
				b.WriteString(",")
			}
			d := fmt.Sprintf("20%02d-%02d-%02d 10:00:00", 15+j%10, 1+j%12, 1+j%28)
			fmt.Fprintf(&b, "(%d, 'author %d', 'a%d@example.com', '%s', '%s', 'comment text %d', '%s', '%s', %d)", 1+j%posts, j, j, d, d, j, approved[j%len(approved)], types[j%len(types)], j%3)
		}
		exec(b.String())
	}
	statuses := []string{"publish", "publish", "publish", "draft", "private", "trash", "auto-draft"}
	ptypes := []string{"post", "post", "post", "page"}
	for i := 0; i < posts; i += 500 {
		var b strings.Builder
		b.WriteString("INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_status, post_name, post_type) VALUES ")
		for j := i; j < i+500 && j < posts; j++ {
			if j > i {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "(1, '20%02d-%02d-%02d %02d:00:00', 'content %d', 'title %d', '%s', 'post-%d', '%s')", 15+j%10, 1+j%12, 1+j%28, j%24, j, j, statuses[j%len(statuses)], j, ptypes[j%len(ptypes)])
		}
		exec(b.String())
	}
	exec("ANALYZE TABLE wp_comments, wp_posts")
	return conn, func() {
		conn.Close()
		db.Close()
		stopServer(tb, st, srv)()
	}
}

// stage122Rows は、文の結果を 1 行ずつ「,」でつないだ文字列にする。
func stage122Rows(tb testing.TB, conn *sql.Conn, q string) []string {
	tb.Helper()
	rows, err := conn.QueryContext(context.Background(), q)
	if err != nil {
		tb.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		tb.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			tb.Fatal(err)
		}
		s := make([]string, len(vals))
		for i, v := range vals {
			s[i] = v.String
		}
		out = append(out, strings.Join(s, ","))
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return out
}

// TestStage122FullScanFilters は、段階 121 の測りで長くなっていた文の結果が、表に入れた値から数えたものと同じか
// を確かめる（比べ方を式ごとに一度だけ決めても、結果は変わらない）。
func TestStage122FullScanFilters(t *testing.T) {
	const comments, posts = 3000, 700
	conn, done := stage122Fixture(t, comments, posts)
	defer done()

	approved := []string{"1", "1", "1", "0", "spam", "trash"}
	types := []string{"comment", "comment", "comment", "note", "pingback"}
	own, ok := 0, 0
	for j := 0; j < comments; j++ {
		a, ty := approved[j%len(approved)], types[j%len(types)]
		if (a == "0" || a == "1") && ty != "note" && j%3 == 1 {
			own++
		}
		if a == "1" && ty != "note" {
			ok++
		}
	}
	statuses := []string{"publish", "publish", "publish", "draft", "private", "trash", "auto-draft"}
	ptypes := []string{"post", "post", "post", "page"}
	months := map[string]bool{}
	var front []int
	for j := 0; j < posts; j++ {
		s, ty := statuses[j%len(statuses)], ptypes[j%len(ptypes)]
		if ty == "post" && s != "auto-draft" && s != "trash" {
			months[fmt.Sprintf("20%02d,%d", 15+j%10, 1+j%12)] = true
		}
		if ty == "post" && (s == "publish" || s == "private") {
			front = append(front, j)
		}
	}
	want := map[string]string{
		"own_comments":      fmt.Sprint(own),
		"approved_comments": fmt.Sprint(ok),
	}
	for _, s := range stage122Statements {
		got := stage122Rows(t, conn, s.query)
		switch s.name {
		case "own_comments", "approved_comments":
			if len(got) != 1 || got[0] != want[s.name] {
				t.Errorf("%s: got %v, want %s", s.name, got, want[s.name])
			}
		case "post_months":
			// 記事のある年と月が 1 回ずつ出る
			seen := map[string]bool{}
			for _, r := range got {
				if !months[r] || seen[r] {
					t.Errorf("post_months: unexpected or repeated row %q", r)
				}
				seen[r] = true
			}
			if len(seen) != len(months) {
				t.Errorf("post_months: got %d rows, want %d", len(seen), len(months))
			}
		case "front_page":
			if len(got) != 10 {
				t.Errorf("front_page: got %d rows, want 10", len(got))
			}
			found := stage122Rows(t, conn, "SELECT FOUND_ROWS()")
			if len(found) != 1 || found[0] != fmt.Sprint(len(front)) {
				t.Errorf("front_page: FOUND_ROWS() = %v, want %d", found, len(front))
			}
		}
	}

	// 比べの左右を入れ替えた形や、ENUM の列を足した表でも、値の文字列で比べるかは式ごとに決まる（段階 116 の
	// 意味は TestStage116Fixes が確かめる）
	for _, q := range []string{
		"SELECT COUNT(*) FROM wp_comments WHERE '1' = comment_approved AND 1 = user_id",
		"SELECT COUNT(*) FROM wp_comments WHERE comment_approved = '1' AND user_id = 1",
	} {
		got := stage122Rows(t, conn, q)
		n := 0
		for j := 0; j < comments; j++ {
			if approved[j%len(approved)] == "1" && j%3 == 1 {
				n++
			}
		}
		if len(got) != 1 || got[0] != fmt.Sprint(n) {
			t.Errorf("%s: got %v, want %d", q, got, n)
		}
	}
}

// BenchmarkStage122FullScanFilters は、段階 121 の測りで長くなっていた文を、WordPress の接続の照合順序で流す
// （コメント 20,000 件、記事 5,000 件。docs/bench.md の段階 121 と同じ数）。
func BenchmarkStage122FullScanFilters(b *testing.B) {
	conn, done := stage122Fixture(b, 20000, 5000)
	defer done()
	for _, s := range stage122Statements {
		b.Run(s.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				stage122Rows(b, conn, s.query)
			}
		})
	}
}
