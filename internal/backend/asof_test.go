package backend

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// mark は、直前までのコミットを含み、この後のコミットを含まない時刻を返す（UTC、マイクロ秒まで）。
func mark(t *testing.T) string {
	t.Helper()
	time.Sleep(2 * time.Millisecond)
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000000")
	time.Sleep(2 * time.Millisecond)
	return ts
}

func TestAsOf(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	for _, q := range wpSchema() {
		s.mustQuery(t, q)
	}
	s.mustQuery(t, "SET time_zone = '+00:00'")
	insertPost := func(id int, title, name string) {
		s.mustQuery(t, "INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES ("+
			strconv.Itoa(id)+", 1, '2026-10-01 10:00:00', 'c', '"+title+"', '', 'publish', '"+name+"', '', '', '', 'post')")
	}

	tEmpty := mark(t)
	insertPost(1, "A", "a")
	insertPost(2, "B", "b")
	s.mustQuery(t, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (2, '_thumb', 'x')")
	t0 := mark(t)
	s.mustQuery(t, "UPDATE wp_posts SET post_title = 'A2' WHERE ID = 1")
	t1 := mark(t)
	s.mustQuery(t, "DELETE FROM wp_posts WHERE ID = 2")
	s.mustQuery(t, "DELETE FROM wp_postmeta WHERE post_id = 2")
	t2 := mark(t)

	check := func(q, want string) {
		t.Helper()
		if got := rowsString(t, s, q); got != want {
			t.Errorf("%s\n got: %q\nwant: %q", q, got, want)
		}
	}
	titles := "SELECT ID, post_title FROM wp_posts AS OF '%s' ORDER BY ID"
	check(strings.Replace(titles, "%s", tEmpty, 1), "")
	check(strings.Replace(titles, "%s", t0, 1), "[1 A]\n[2 B]\n")
	check(strings.Replace(titles, "%s", t1, 1), "[1 A2]\n[2 B]\n")
	check(strings.Replace(titles, "%s", t2, 1), "[1 A2]\n")
	check("SELECT ID, post_title FROM wp_posts ORDER BY ID", "[1 A2]\n")
	check("SELECT ID FROM wp_posts AS OF '"+t0+"' ORDER BY ID DESC", "[2]\n[1]\n")

	// 主キーの検索とインデックス（模擬）経由の検索
	check("SELECT post_title FROM wp_posts AS OF '"+t1+"' WHERE ID = 2", "[B]\n")
	check("SELECT ID FROM wp_posts AS OF '"+t1+"' WHERE post_name = 'b'", "[2]\n")
	check("SELECT meta_value FROM wp_postmeta AS OF '"+t1+"' WHERE post_id = 2", "[x]\n")

	// 今と過去の結合: 削除された記事を探す
	check("SELECT p.ID, p.post_title FROM wp_posts AS OF '"+t1+"' p LEFT JOIN wp_posts n ON n.ID = p.ID WHERE n.ID IS NULL", "[2 B]\n")

	// 同じ瞬間を別の書き方で指定しても結果は同じ
	t1Time, err := time.Parse("2006-01-02 15:04:05.000000", t1)
	if err != nil {
		t.Fatal(err)
	}
	check("SELECT ID FROM wp_posts AS OF TIMESTAMP('"+t1+"') ORDER BY ID", "[1]\n[2]\n")
	s.mustQuery(t, "SET time_zone = '+09:00'")
	check("SELECT ID FROM wp_posts AS OF '"+t1Time.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04:05.000000")+"' ORDER BY ID", "[1]\n[2]\n")
	// 同じ文字列でも +09:00 では 9 時間前の瞬間になり、テーブルもまだない。
	if _, err := query(s.ctx, s.engine, "SELECT ID FROM wp_posts AS OF '"+t1+"'"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("AS OF 9 hours earlier: err = %v, want table not found", err)
	}
	s.mustQuery(t, "SET time_zone = '+00:00'")

	// 日付だけの指定（その日の 0 時）はテーブルがまだない時点なので見つからない。不正な値はエラー。
	for _, q := range []string{
		"SELECT COUNT(*) FROM wp_posts AS OF '2000-01-01'",
		"SELECT * FROM wp_posts AS OF 'yesterday'",
		"SELECT * FROM wp_posts AS OF '2026-13-01 00:00:00'",
	} {
		if _, err := query(s.ctx, s.engine, q); err == nil {
			t.Errorf("%s: expected an error", q)
		}
	}

	// 過去の行を使って復元する
	s.mustQuery(t, "INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '"+t1+"' WHERE ID = 2")
	check("SELECT ID, post_title FROM wp_posts ORDER BY ID", "[1 A2]\n[2 B]\n")

	// 削除したテーブルも、削除前の時点なら読める
	s.mustQuery(t, "DROP TABLE wp_links")
	tDropped := mark(t)
	check("SHOW TABLES AS OF '"+t2+"' LIKE 'wp_links'", "[wp_links]\n")
	check("SHOW TABLES AS OF '"+tDropped+"' LIKE 'wp_links'", "")
	check("SELECT COUNT(*) FROM wp_links AS OF '"+t2+"'", "[0]\n")
	if _, err := query(s.ctx, s.engine, "SELECT * FROM wp_links AS OF '"+tDropped+"'"); err == nil {
		t.Error("dropped table should not be found after the drop")
	}
	s.stop(t)

	// 再起動後も過去が読める
	s = start(t, dir)
	defer s.stop(t)
	s.mustQuery(t, "SET time_zone = '+00:00'")
	check(strings.Replace(titles, "%s", t0, 1), "[1 A]\n[2 B]\n")
}

func TestParseAsOfRejectsPlaceholderAndOtherTypes(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	for _, v := range []any{nil, 42, []byte("2026-10-02")} {
		if _, err := parseAsOf(s.ctx, v); err == nil {
			t.Errorf("parseAsOf(%#v) should fail", v)
		}
	}
}
