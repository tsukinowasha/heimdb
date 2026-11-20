package backend

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

// legacyStatement は、テストのダンプの 1 つの文と、拒否されるはずならそのエラーの文の一部。
type legacyStatement struct {
	line    int
	query   string
	wantErr string
}

// readLegacyDump は、internal/testdata/legacy_dump.sql を文に分ける。行末の「;」で文が終わり、「-- expect error: 」の行は
// 次の文が拒否されるはずであることを示す。ほかの「--」の行は読み飛ばす。
func readLegacyDump(t *testing.T) []legacyStatement {
	t.Helper()
	b, err := os.ReadFile("../testdata/legacy_dump.sql")
	if err != nil {
		t.Fatal(err)
	}
	var out []legacyStatement
	var cur []string
	start, wantErr := 0, ""
	for i, l := range strings.Split(string(b), "\n") {
		if len(cur) == 0 {
			if w, ok := strings.CutPrefix(l, "-- expect error: "); ok {
				wantErr = w
				continue
			}
			if strings.HasPrefix(l, "--") || strings.TrimSpace(l) == "" {
				continue
			}
			start = i + 1
		}
		cur = append(cur, l)
		if strings.HasSuffix(strings.TrimSpace(l), ";") {
			out = append(out, legacyStatement{line: start, query: strings.Join(cur, "\n"), wantErr: wantErr})
			cur, wantErr = nil, ""
		}
	}
	if len(cur) > 0 {
		t.Fatalf("unterminated statement at line %d", start)
	}
	return out
}

// 履歴のあるサイトの mysqldump の形を取り込む（docs/plan.md の段階 39）。新しく入れた WordPress にはない形（古い
// プラグインの表、utf8mb3、MyISAM、前方の長さ付きのインデックス、主キーのない表（段階 72 から受け入れる）、latin1）を、
// 受け入れるものと拒否するものに分けて確かめる。
func TestLegacyDump(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	db := openRoot(t, addr)
	defer db.Close()
	ctx := context.Background()
	c, err := db.Conn(ctx) // mysql クライアントと同じく、1 つの接続で順に流す（LOCK TABLES、SET の変数）
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	stmts := readLegacyDump(t)
	rejected := 0
	for _, s := range stmts {
		_, err := c.ExecContext(ctx, s.query)
		switch {
		case s.wantErr == "" && err != nil:
			t.Errorf("line %d: %v\n%s", s.line, err, s.query)
		case s.wantErr != "" && err == nil:
			t.Errorf("line %d: accepted, want an error containing %q\n%s", s.line, s.wantErr, s.query)
		case s.wantErr != "" && !strings.Contains(err.Error(), s.wantErr):
			t.Errorf("line %d: %v, want %q", s.line, err, s.wantErr)
		case s.wantErr != "":
			rejected++
		}
		if err != nil && (strings.Contains(err.Error(), "invalid connection") || strings.Contains(err.Error(), "EOF")) {
			t.Fatalf("line %d: the connection was dropped: %v", s.line, err)
		}
	}
	if rejected != 1 {
		t.Errorf("rejected %d statements, want 1", rejected)
	}

	q := func(query string) string {
		t.Helper()
		var v sql.NullString
		if err := c.QueryRowContext(ctx, query).Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v.String
	}
	for query, want := range map[string]string{
		"SELECT COUNT(*) FROM wp_options":                                              "5",
		"SELECT option_value FROM wp_options WHERE option_name = 'BLOGNAME'":           "株式会社サンプル",
		"SELECT option_name FROM wp_options WHERE option_name = 'blogdescription'":     "Blogdescription",
		"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = '_yoast_wpseo_title'":       "1",
		"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key IS NULL":                      "1",
		"SELECT post_date FROM wp_posts WHERE ID = 11":                                 "0000-00-00 00:00:00",
		"SELECT ID FROM wp_posts WHERE post_type = 'page' AND post_status = 'publish'": "10",
		"SELECT COUNT(*) FROM wp_yoast_seo_links WHERE post_id = 11":                   "2",
		"SELECT value FROM wp_yoast_expiring_store WHERE key_name = 'WPSEO_SITE_B'":    "b",
		// utf8mb3 の表は utf8mb4 の同じ名前の照合順序になる（段階 37）
		"SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'wp_options'":                           "utf8mb4_general_ci",
		"SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'wp_yoast_seo_links'":                   "utf8mb4_unicode_ci",
		"SELECT SUB_PART FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'wp_postmeta' AND index_name = 'meta_key'": "191",
		"SELECT `AUTO_INCREMENT` FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'wp_options'":                          "1203",
		// 主キーのない表（段階 72）
		"SELECT internal_link_count FROM wp_yoast_seo_meta WHERE object_id = 11": "1",
		"SELECT COUNT(*) FROM wp_xsg_sitemap_meta":                               "0",
		// 拒否した表はできていない
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'wp_old_counter'": "0",
	} {
		if got := q(query); got != want {
			t.Errorf("%s: %q, want %q", query, got, want)
		}
	}
}
