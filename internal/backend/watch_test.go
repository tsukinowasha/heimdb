package backend

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
)

// 危ない変更の見張り（docs/plan.md の段階 33）。WordPress が乗っ取られるときに変わる行の変更がログと heimdb_status に
// 出て、取り消した変更と見張らない行では出ない。
func TestWatch(t *testing.T) {
	addr, _, st, srv := startReview19Server(t, t.TempDir())
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
	seen := 0
	// logged は、前に呼んでからの見張りのログを返す。
	logged := func() []string {
		var out []string
		n := 0
		for _, e := range hook.AllEntries() {
			if m, ok := strings.CutPrefix(e.Message, "HeimDB: watch: "); ok {
				if n++; n > seen {
					out = append(out, m)
				}
			}
		}
		seen = n
		return out
	}
	expect := func(what string, want ...string) {
		t.Helper()
		got := logged()
		if len(got) != len(want) {
			t.Errorf("%s: got %d lines, want %d:\n%s", what, len(got), len(want), strings.Join(got, "\n"))
			return
		}
		for i := range want {
			// 「on ...: 」を省いた期待値は、本番のデータベースの変更。
			w := want[i]
			if !strings.HasPrefix(w, "on ") {
				w = "on wordpress: " + w
			}
			if got[i] != "root@127.0.0.1 "+w {
				t.Errorf("%s: line %d = %q, want ...%q", what, i, got[i], want[i])
			}
		}
	}

	for _, q := range wpSchema() {
		exec(q)
	}
	const hash = "$P$Bsecrethash0123456789"
	exec(`INSERT INTO wp_options (option_name, option_value) VALUES
		('siteurl', 'http://example.com'), ('blogname', 'Site'), ('default_role', 'subscriber'),
		('active_plugins', 'a:1:{i:0;s:19:"akismet/akismet.php";}'), ('wp_user_roles', 'a:0:{}')`)
	expect("install",
		`on wordpress: option siteurl: (none) -> "http://example.com"`,
		`on wordpress: option default_role: (none) -> "subscriber"`,
		`on wordpress: plugins activated: akismet/akismet.php`,
		`on wordpress: option wp_user_roles (role definitions) added`)

	// 見張らない行と、値の変わらない更新は出ない。
	exec("UPDATE wp_options SET option_value = 'Other' WHERE option_name = 'blogname'",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('_transient_x', '1')",
		"UPDATE wp_options SET option_value = 'http://example.com' WHERE option_name = 'siteurl'",
		`UPDATE wp_options SET option_value = 'a:1:{i:0;s:19:"akismet/akismet.php";}' WHERE option_name = 'active_plugins'`)
	expect("unwatched")

	exec("UPDATE wp_options SET option_value = 'http://evil.example' WHERE option_name = 'siteurl'")
	expect("siteurl", `on wordpress: option siteurl: "http://example.com" -> "http://evil.example"`)

	// WordPress の add_option の形（INSERT ... ON DUPLICATE KEY UPDATE）。
	exec("INSERT INTO wp_options (option_name, option_value) VALUES ('users_can_register', '1') ON DUPLICATE KEY UPDATE option_value = VALUES(option_value)",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('users_can_register', '0') ON DUPLICATE KEY UPDATE option_value = VALUES(option_value)")
	expect("upsert",
		`option users_can_register: (none) -> "1"`,
		`option users_can_register: "1" -> "0"`)

	// 1 つのトランザクションの 2 つの変更は、コミットの後に出る。
	exec("START TRANSACTION",
		"UPDATE wp_options SET option_value = 'administrator' WHERE option_name = 'default_role'",
		"UPDATE wp_options SET option_value = '1' WHERE option_name = 'users_can_register'")
	expect("before commit")
	exec("COMMIT")
	expect("commit",
		`option default_role: "subscriber" -> "administrator"`,
		`option users_can_register: "0" -> "1"`)

	// ロールバック、セーブポイントへのロールバック、失敗した文で取り消した変更は出ない。
	exec("START TRANSACTION",
		"UPDATE wp_options SET option_value = 'http://rolled.back' WHERE option_name = 'siteurl'",
		"ROLLBACK")
	exec("START TRANSACTION",
		"SAVEPOINT a",
		"UPDATE wp_options SET option_value = 'editor' WHERE option_name = 'default_role'",
		"ROLLBACK TO SAVEPOINT a",
		"UPDATE wp_options SET option_value = 'http://kept.example' WHERE option_name = 'siteurl'")
	if _, err := c.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value) VALUES ('home', 'http://failed'), ('blogname', 'dup')"); err == nil {
		t.Fatal("duplicate option inserted")
	}
	exec("COMMIT")
	if _, err := c.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value) VALUES ('admin_email', 'a@failed'), ('blogname', 'dup')"); err == nil {
		t.Fatal("duplicate option inserted")
	}
	expect("discarded", `option siteurl: "http://evil.example" -> "http://kept.example"`)

	// 同じ行を何度も書いても、最初と最後で比べる。
	exec("START TRANSACTION",
		"UPDATE wp_options SET option_value = 'http://tmp' WHERE option_name = 'siteurl'",
		"UPDATE wp_options SET option_value = 'http://kept.example' WHERE option_name = 'siteurl'",
		"UPDATE wp_options SET option_value = 'http://a' WHERE option_name = 'home'",
		"DELETE FROM wp_options WHERE option_name = 'home'",
		"COMMIT")
	expect("collapsed")

	exec(`UPDATE wp_options SET option_value = 'a:2:{i:0;s:9:"hello.php";i:1;s:15:"evil/shell.php";}' WHERE option_name = 'active_plugins'`,
		"UPDATE wp_options SET option_value = 'a:1:{s:5:\"admin\";a:0:{}}' WHERE option_name = 'wp_user_roles'",
		"DELETE FROM wp_options WHERE option_name = 'default_role'")
	expect("plugins and roles",
		`plugins activated: hello.php, evil/shell.php; deactivated: akismet/akismet.php`,
		`option wp_user_roles (role definitions) changed`,
		`option default_role: "administrator" -> (none)`)

	// ユーザー。パスワードのハッシュはログに出さない。
	exec("INSERT INTO wp_users (ID, user_login, user_pass, user_email) VALUES (1, 'admin', '"+hash+"', 'a@example.com')",
		"UPDATE wp_users SET user_pass = '$P$Bnewhash' WHERE ID = 1",
		"UPDATE wp_users SET user_email = 'attacker@example.net', user_pass = '$P$Bthird' WHERE ID = 1",
		"UPDATE wp_users SET user_status = 0, display_name = 'Admin' WHERE ID = 1",
		"INSERT INTO wp_users (ID, user_login, user_email) VALUES (2, 'backdoor', 'x@example.net')",
		"DELETE FROM wp_users WHERE ID = 2")
	expect("users",
		`user added: "admin" (ID 1)`,
		`password changed: "admin" (ID 1)`,
		`password changed: "admin" (ID 1)`,
		`email changed: "admin" (ID 1): "a@example.com" -> "attacker@example.net"`,
		`user added: "backdoor" (ID 2)`,
		`user deleted: "backdoor" (ID 2)`)

	// 管理者の役割が新しく付いたときだけ出る。
	const admin, subscriber = `a:1:{s:13:"administrator";b:1;}`, `a:1:{s:10:"subscriber";b:1;}`
	exec("INSERT INTO wp_usermeta (user_id, meta_key, meta_value) VALUES (1, 'wp_capabilities', '"+admin+"')",
		"INSERT INTO wp_usermeta (user_id, meta_key, meta_value) VALUES (3, 'wp_capabilities', '"+subscriber+"'), (3, 'nickname', 'administrator')",
		"UPDATE wp_usermeta SET meta_value = '"+admin+"' WHERE user_id = 3 AND meta_key = 'wp_capabilities'",
		"UPDATE wp_usermeta SET meta_value = '"+subscriber+"' WHERE user_id = 1 AND meta_key = 'wp_capabilities'",
		"INSERT INTO wp_usermeta (user_id, meta_key, meta_value) VALUES (3, 'wp_2_capabilities', '"+admin+"')")
	expect("administrator",
		`administrator role given: user ID 1 (wp_capabilities)`,
		`administrator role given: user ID 3 (wp_capabilities)`,
		`administrator role given: user ID 3 (wp_2_capabilities)`)

	exec(`INSERT INTO wp_sitemeta (site_id, meta_key, meta_value) VALUES (1, 'site_admins', 'a:1:{i:0;s:5:"admin";}'), (1, 'site_name', 'N')`,
		`UPDATE wp_sitemeta SET meta_value = 'a:2:{i:0;s:5:"admin";i:1;s:8:"backdoor";}' WHERE meta_key = 'site_admins'`)
	expect("site admins",
		`network option site_admins (super admins): (none) -> "admin"`,
		`network option site_admins (super admins): "admin" -> "admin, backdoor"`)

	// 接頭辞が違っても、マルチサイトのサイトのテーブルでも見張る。名前だけ似た別のテーブルは見張らない。
	exec("CREATE TABLE shop_2_options LIKE wp_options",
		"INSERT INTO shop_2_options (option_name, option_value) VALUES ('home', 'http://shop')",
		"CREATE TABLE my_options (id INT PRIMARY KEY, option_value TEXT)",
		"INSERT INTO my_options VALUES (1, 'siteurl')")
	expect("prefix", `on wordpress: option home: (none) -> "http://shop"`)

	// heimdb_status に件数と最後の 1 件。
	s := statusMap(t, c)
	if s["watch_changes"] != "25" || !strings.HasSuffix(s["watch_last_change"], `option home: (none) -> "http://shop"`) || s["watch_last_change_at"] == "NULL" {
		t.Errorf("status: %q %q %q", s["watch_changes"], s["watch_last_change"], s["watch_last_change_at"])
	}

	// ブランチでの変更はブランチの名前で出て、マージで親に入るときも出る。
	exec("CALL heimdb_branch_create('staging')",
		"USE `wordpress@staging`",
		"UPDATE wp_options SET option_value = 'http://staging' WHERE option_name = 'siteurl'",
		"INSERT INTO wp_options (option_name, option_value) VALUES ('admin_email', 'attacker@example.net')",
		"USE wordpress")
	expect("branch",
		`on wordpress@staging: option siteurl: "http://kept.example" -> "http://staging"`,
		`on wordpress@staging: option admin_email: (none) -> "attacker@example.net"`)
	// siteurl はマージの規則で親に入らない。
	exec("CALL heimdb_branch_merge('staging')")
	expect("merge", `on wordpress: option admin_email: (none) -> "attacker@example.net" (merge of branch staging)`)

	// 同じトランザクションで足して消したユーザーは出ない。
	exec("START TRANSACTION",
		"INSERT INTO wp_users (ID, user_login, user_email) VALUES (9, 'temp', 't@example.net')",
		"DELETE FROM wp_users WHERE ID = 9",
		"COMMIT")
	expect("added and deleted")

	// DDL の前の暗黙のコミットで確定した変更は出て、後の ROLLBACK では消えない。
	exec("START TRANSACTION",
		"UPDATE wp_options SET option_value = 'http://ddl.example' WHERE option_name = 'siteurl'",
		"CREATE TABLE ddl_t (id INT PRIMARY KEY)",
		"ROLLBACK")
	expect("implicit commit", `option siteurl: "http://kept.example" -> "http://ddl.example"`)

	// マルチサイトのネットワークでの有効化。
	exec(`INSERT INTO wp_sitemeta (site_id, meta_key, meta_value) VALUES (1, 'active_sitewide_plugins', 'a:1:{s:9:"hello.php";i:1700000000;}')`)
	expect("network plugins", `network plugins activated: hello.php`)

	// 名前に空白や改行を含むものは、引用符で囲んでエスケープする（ログの行を偽れない）。
	exec("INSERT INTO wp_options (option_name, option_value) VALUES ('x\\nlevel=error user_roles', '1')",
		`UPDATE wp_options SET option_value = 'a:1:{i:0;s:7:"a b.php";}' WHERE option_name = 'active_plugins'`)
	expect("escaped",
		`option "x\nlevel=error user_roles" (role definitions) added`,
		`plugins activated: "a b.php"; deactivated: hello.php, evil/shell.php`)

	// ブランチで足したユーザーは出る。ユーザーはマージの規則で親に入らないので、マージでは出ない。
	exec("CALL heimdb_branch_create('s2')",
		"INSERT INTO `wordpress@s2`.wp_users (ID, user_login, user_email) VALUES (10, 'branchuser', 'm@example.net')")
	expect("user on branch", `on wordpress@s2: user added: "branchuser" (ID 10)`)
	exec("CALL heimdb_branch_merge('s2')")
	expect("user merge")

	// 復元で戻した行は出さない。
	before := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	time.Sleep(10 * time.Millisecond)
	exec("UPDATE wp_options SET option_value = 'http://later' WHERE option_name = 'siteurl'")
	expect("before restore", `option siteurl: "http://ddl.example" -> "http://later"`)
	exec("CALL heimdb_restore('main', '" + before + "', 'wp_options')")
	expect("restore")
	var v string
	if err := c.QueryRowContext(ctx, "SELECT option_value FROM wp_options WHERE option_name = 'siteurl'").Scan(&v); err != nil || v != "http://ddl.example" {
		t.Errorf("restored siteurl = %q, %v", v, err)
	}

	// サイト用のユーザーの変更は、そのユーザーと接続元のアドレスで出る。
	exec("CREATE USER 'wordpress'@'%' IDENTIFIED BY 'pw'", "GRANT ALL ON wordpress.* TO 'wordpress'@'%'")
	site, err := sql.Open("mysql", "wordpress:pw@tcp("+addr+")/wordpress")
	if err != nil {
		t.Fatal(err)
	}
	defer site.Close()
	if _, err := site.Exec("UPDATE wp_options SET option_value = 'site@example.com' WHERE option_name = 'admin_email'"); err != nil {
		t.Fatal(err)
	}
	if got := logged(); len(got) != 1 || !strings.HasPrefix(got[0], "wordpress@127.0.0.1 on wordpress: option admin_email: ") {
		t.Errorf("site user: %q", got)
	}

	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "secrethash") || strings.Contains(e.Message, "newhash") || strings.Contains(e.Message, "Bthird") {
			t.Errorf("password hash in the log: %s", e.Message)
		}
	}
}
