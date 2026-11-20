package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
)

// changeLines は、手続きの結果の各行を「|」でつないだ文字列にする（NULL は NULL、changed_at の列は時刻の代わりに T）。
func changeLines(t *testing.T, c *sql.Conn, q string) []string {
	t.Helper()
	rows, err := c.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			switch {
			case !v.Valid:
				parts[i] = "NULL"
			case cols[i] == "changed_at":
				parts[i] = "T"
			default:
				parts[i] = v.String
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func timeArg() string {
	time.Sleep(5 * time.Millisecond)
	s := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	time.Sleep(5 * time.Millisecond)
	return s
}

// 変更の一覧と、復元のときの警告（docs/plan.md の段階 34）。
func TestChanges(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	hook := logtest.NewGlobal()
	defer hook.Reset()
	db := openRoot(t, addr)
	defer db.Close()
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exec := func(qs ...string) {
		t.Helper()
		for _, q := range qs {
			if _, err := c.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	check := func(what, q string, want ...string) {
		t.Helper()
		got := changeLines(t, c, q)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	for _, q := range wpSchema() {
		exec(q)
	}
	const admin = `a:1:{s:13:"administrator";b:1;}`
	exec(`INSERT INTO wp_options (option_name, option_value) VALUES ('blogname', 'Site'), ('default_role', 'subscriber'), ('_transient_a', '1'), ('cron', 'a:0:{}')`,
		"INSERT INTO wp_users (ID, user_login, user_pass, user_email) VALUES (1, 'admin', 'h1', 'a@example.com')",
		"INSERT INTO wp_usermeta (umeta_id, user_id, meta_key, meta_value) VALUES (1, 1, 'wp_capabilities', '"+admin+"')",
		"INSERT INTO wp_posts (ID, post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES (1, 'One', 'a', '', '', '', ''), (2, 'Two', 'b', '', '', '', '')")
	t0 := timeArg()

	// 乗っ取りの再現と、普段の書き込み。
	exec("UPDATE wp_options SET option_value = 'administrator' WHERE option_name = 'default_role'")
	time.Sleep(2 * time.Millisecond)
	exec("INSERT INTO wp_users (ID, user_login, user_pass, user_email) VALUES (7, 'evil', 'h7', 'e@example.net')",
		"INSERT INTO wp_usermeta (umeta_id, user_id, meta_key, meta_value) VALUES (2, 7, 'wp_capabilities', '"+admin+"')")
	time.Sleep(2 * time.Millisecond)
	exec("UPDATE wp_posts SET post_content = 'changed', post_title = 'One!' WHERE ID = 1",
		"DELETE FROM wp_posts WHERE ID = 2",
		"UPDATE wp_options SET option_value = '2' WHERE option_name = '_transient_a'",
		"UPDATE wp_options SET option_value = 'a:1:{}' WHERE option_name = 'cron'",
		"INSERT INTO wp_postmeta (meta_id, post_id, meta_key, meta_value) VALUES (1, 1, '_edit_lock', '1:1'), (2, 1, 'color', 'red')",
		// 変えて戻した行は出ない。
		"UPDATE wp_options SET option_value = 'Other' WHERE option_name = 'blogname'",
		"UPDATE wp_options SET option_value = 'Site' WHERE option_name = 'blogname'")

	all := []string{
		`wp_options|option_name=default_role|update|option_value|T|option default_role: "subscriber" -> "administrator"`,
		`wp_users|ID=7|insert|NULL|T|user added: "evil" (ID 7)`,
		`wp_usermeta|umeta_id=2|insert|NULL|T|administrator role given: user ID 7 (wp_capabilities)`,
		`wp_posts|ID=1|update|post_content,post_title|T|NULL`,
		`wp_posts|ID=2|delete|NULL|T|NULL`,
		`wp_postmeta|meta_id=2|insert|NULL|T|NULL`,
		`*|*|not shown|NULL|NULL|3 rows that change routinely (transients, cron, edit locks)`,
	}
	check("all", "CALL heimdb_changes('main', '"+t0+"')", all...)
	check("tables", "CALL heimdb_changes('main', '"+t0+"', 'wp_users, wp_usermeta')", all[1], all[2])

	// 既定の復元は、ユーザーを戻さないことを、見張りの対象の変化とともに知らせる（ログにも出す）。
	notRestored := []string{
		"wp_usermeta|not restored|1|0|0|administrator role given: user ID 7 (wp_capabilities); to restore users too: CALL heimdb_restore('main', '",
		`wp_users|not restored|1|0|0|user added: "evil" (ID 7); to restore users too: CALL heimdb_restore('main', '`,
	}
	hasNotRestored := func(what string, lines []string, want bool) {
		t.Helper()
		found := 0
		for _, l := range lines {
			for _, w := range notRestored {
				if strings.HasPrefix(l, w) {
					found++
				}
			}
			if strings.Contains(l, "not restored") && !strings.HasSuffix(l, "', '*')") {
				t.Errorf("%s: %s", what, l)
			}
		}
		if (found == len(notRestored)) != want || (!want && found > 0) {
			t.Errorf("%s: not restored rows found %d, want %v:\n%s", what, found, want, strings.Join(lines, "\n"))
		}
	}
	hasNotRestored("preview", changeLines(t, c, "CALL heimdb_restore_preview('main', '"+t0+"')"), true)
	hasNotRestored("preview *", changeLines(t, c, "CALL heimdb_restore_preview('main', '"+t0+"', '*')"), false)
	hasNotRestored("preview tables", changeLines(t, c, "CALL heimdb_restore_preview('main', '"+t0+"', 'wp_options')"), false)

	out := changeLines(t, c, "CALL heimdb_restore('main', '"+t0+"')")
	hasNotRestored("restore", out, true)
	if last := out[len(out)-1]; !strings.HasPrefix(last, "*|undo|") {
		t.Errorf("no undo row: %s", last)
	}
	logged := 0
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "HeimDB: restore of main to ") && strings.Contains(e.Message, `user added: "evil" (ID 7)`) {
			logged++
		}
	}
	if logged != 1 {
		t.Errorf("restore warnings in the log: %d", logged)
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_users WHERE ID = 7").Scan(&n); err != nil || n != 1 {
		t.Errorf("the default restore removed the user: %d %v", n, err)
	}
	var role string
	if err := c.QueryRowContext(ctx, "SELECT option_value FROM wp_options WHERE option_name = 'default_role'").Scan(&role); err != nil || role != "subscriber" {
		t.Errorf("default_role = %q %v", role, err)
	}
	// すべて戻せば、ユーザーも戻り、警告は出ない。
	hasNotRestored("restore *", changeLines(t, c, "CALL heimdb_restore('main', '"+t0+"', '*')"), false)
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_users WHERE ID = 7").Scan(&n); err != nil || n != 0 {
		t.Errorf("the user is still there: %d %v", n, err)
	}

	// テーブルの作成と書き直しは 1 行。ブランチでは、分岐の後のブランチの変更だけが見える。
	t1 := timeArg()
	exec("CREATE TABLE plugin_t (id INT PRIMARY KEY)",
		"ALTER TABLE wp_links ADD COLUMN extra INT",
		"CALL heimdb_branch_create('b')",
		"UPDATE `wordpress@b`.wp_posts SET post_title = 'Branch' WHERE ID = 1",
		"UPDATE wp_posts SET post_content = 'main only' WHERE ID = 1")
	check("tables created and changed", "CALL heimdb_changes('main', '"+t1+"', 'plugin_t,wp_links,wp_posts')",
		"plugin_t|*|table created|NULL|T|NULL",
		"wp_links|*|table changed|NULL|T|NULL",
		"wp_posts|ID=1|update|post_content|T|NULL")
	check("branch", "CALL heimdb_changes('b', '"+t1+"', 'wp_posts')",
		"wp_posts|ID=1|update|post_title|T|NULL")
	// 時刻の前からあって消したテーブルは 1 行。時刻の後に作って消したテーブルは、時刻にも今にもないので指定できない。
	exec("DROP TABLE plugin_t", "DROP TABLE wp_links")
	check("dropped", "CALL heimdb_changes('main', '"+t1+"', 'wp_links')",
		"wp_links|*|table dropped|NULL|NULL|NULL")

	// 誤り: ブランチがない、今より先、ないテーブル、保持期間の外。
	for _, q := range []string{
		"CALL heimdb_changes('nope', '" + t1 + "')",
		"CALL heimdb_changes('main', '2999-01-01 00:00:00')",
		"CALL heimdb_changes('main', '" + t1 + "', 'nope')",
		"CALL heimdb_changes('main', '" + t1 + "', 'plugin_t')",
	} {
		if _, err := c.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: no error", q)
		}
	}
	pro.Retention = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	exec("CALL heimdb_gc()")
	if _, err := c.ExecContext(ctx, "CALL heimdb_changes('main', '"+t0+"')"); err == nil {
		t.Error("changes before the GC boundary: no error")
	}
}

// 時刻の後に定義だけが変わった表（インデックスの追加。テーブル ID は同じ）でも、行ごとの変化と、既定の復元が戻さない
// ユーザーの変化を出す（docs/plan.md の段階 43。プラグインの更新の dbDelta がインデックスを足すと、攻撃者が足した
// 管理者が復元の結果に出なくなっていた）。
func TestChangesAfterIndexChange(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	db := openRoot(t, addr)
	defer db.Close()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exec := func(qs ...string) {
		t.Helper()
		for _, q := range qs {
			if _, err := c.ExecContext(context.Background(), q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	for _, q := range wpSchema() {
		exec(q)
	}
	exec("INSERT INTO wp_users (ID, user_login, user_pass, user_email) VALUES (1, 'admin', 'h1', 'a@example.com')")
	t0 := timeArg()
	exec("INSERT INTO wp_users (ID, user_login, user_pass, user_email) VALUES (7, 'evil', 'h7', 'e@example.net')",
		`INSERT INTO wp_usermeta (umeta_id, user_id, meta_key, meta_value) VALUES (2, 7, 'wp_capabilities', 'a:1:{s:13:"administrator";b:1;}')`,
		"ALTER TABLE wp_usermeta ADD INDEX heimdb_test_idx (meta_value(10))")
	got := strings.Join(changeLines(t, c, "CALL heimdb_changes('main', '"+t0+"', 'wp_users,wp_usermeta')"), "\n")
	for _, want := range []string{
		`wp_users|ID=7|insert|NULL|T|user added: "evil" (ID 7)`,
		`wp_usermeta|umeta_id=2|insert|NULL|T|administrator role given: user ID 7 (wp_capabilities)`,
		"wp_usermeta|*|definition changed|NULL|T|NULL",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("changes: missing %q in\n%s", want, got)
		}
	}
	preview := strings.Join(changeLines(t, c, "CALL heimdb_restore_preview('main', '"+t0+"')"), "\n")
	for _, want := range []string{"wp_users|not restored|1|0|0|user added: \"evil\" (ID 7)", "wp_usermeta|not restored|1|0|0|administrator role given: user ID 7"} {
		if !strings.Contains(preview, want) {
			t.Errorf("restore preview: missing %q in\n%s", want, preview)
		}
	}
}
