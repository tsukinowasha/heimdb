package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// 引用符で囲んだエンジン名は、CREATE TABLE と ALTER TABLE の表のオプションでだけバッククォートにする。長さは変えない
// （docs/plan.md の段階 57）。
func TestUnquoteEngine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"CREATE TABLE t (id int PRIMARY KEY) ENGINE='InnoDB'", "CREATE TABLE t (id int PRIMARY KEY) ENGINE=`InnoDB`"},
		{"CREATE TABLE t (id int PRIMARY KEY) ENGINE = \"InnoDB\" DEFAULT CHARSET=utf8mb4", "CREATE TABLE t (id int PRIMARY KEY) ENGINE = `InnoDB` DEFAULT CHARSET=utf8mb4"},
		{"create temporary table if not exists t (id int primary key)\n\tengine 'MyISAM'", "create temporary table if not exists t (id int primary key)\n\tengine `MyISAM`"},
		{"ALTER TABLE t ENGINE='InnoDB'", "ALTER TABLE t ENGINE=`InnoDB`"},
		{"/* x */ ALTER TABLE t ADD c int, ENGINE='InnoDB'", "/* x */ ALTER TABLE t ADD c int, ENGINE=`InnoDB`"},
		{"SELECT 1; CREATE TABLE t (id int PRIMARY KEY) ENGINE='InnoDB'", "SELECT 1; CREATE TABLE t (id int PRIMARY KEY) ENGINE=`InnoDB`"},
		// 変えないもの
		{"SELECT table_name FROM information_schema.tables WHERE engine = 'InnoDB'", ""},
		{"SELECT engine 'e' FROM information_schema.tables", ""},
		{"CREATE TABLE t (id int PRIMARY KEY, engine varchar(10) DEFAULT 'InnoDB', CHECK (engine = 'InnoDB'))", ""},
		{"ALTER TABLE t ADD COLUMN engine varchar(10) DEFAULT 'InnoDB'", ""},
		{"ALTER TABLE t ALTER COLUMN engine SET DEFAULT 'InnoDB'", ""},
		{"CREATE TABLE t (id int PRIMARY KEY) AS SELECT 1 AS id FROM x WHERE engine = 'InnoDB'", ""},
		{"CREATE TABLE t (id int PRIMARY KEY) COMMENT 'ENGINE=''InnoDB''' ENGINE=InnoDB", ""},
		{"CREATE TABLE t (id int PRIMARY KEY) /* ENGINE='InnoDB' */ ENGINE=InnoDB", ""},
		{"CREATE TABLE t (id int PRIMARY KEY) -- ENGINE='InnoDB'\n", ""},
		{"CREATE TABLE t (id int PRIMARY KEY) ENGINE='Inno DB'", ""},
		{"CREATE TABLE t (id int PRIMARY KEY) ENGINE=''", ""},
		{"INSERT INTO t VALUES ('CREATE TABLE x ENGINE=''InnoDB''')", ""},
		{"UPDATE t SET engine = 'InnoDB'; ALTER TABLE t COMMENT 'x'", ""},
	} {
		want := tc.want
		if want == "" {
			want = tc.in
		}
		got := unquoteEngine(tc.in, false)
		if got != want {
			t.Errorf("unquoteEngine(%q)\n got %q\nwant %q", tc.in, got, want)
		}
		if len(got) != len(tc.in) {
			t.Errorf("unquoteEngine(%q) changed the length", tc.in)
		}
	}
	// ANSI_QUOTES では "InnoDB" は識別子なので、そのままでよい
	if q := `CREATE TABLE t (id int PRIMARY KEY) ENGINE="InnoDB"`; unquoteEngine(q, true) != q {
		t.Errorf("ANSI_QUOTES: %q", unquoteEngine(q, true))
	}
}

// wpMailSMTPCreates は、WP Mail SMTP 4.10.2 が出す CREATE TABLE（段階 56 の scripts/plugin-check で -debug のログから
// 取った。送信キューは $wpdb->collate を COLLATE='...' で付ける）。
var wpMailSMTPCreates = []string{
	"CREATE TABLE IF NOT EXISTS `wp_wpmailsmtp_debug_events` ( `id` INT UNSIGNED NOT NULL AUTO_INCREMENT, `content` TEXT DEFAULT NULL, `initiator` TEXT DEFAULT NULL, `event_type` TINYINT UNSIGNED NOT NULL DEFAULT '0', `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (id) ) ENGINE='InnoDB' DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci;",
	"\n\t\tCREATE TABLE `wp_wpmailsmtp_emails_queue` (\n\t\t    `id` INT UNSIGNED NOT NULL AUTO_INCREMENT,\n\t\t    `data` LONGTEXT NULL,\n\t\t    `status` TINYINT UNSIGNED NOT NULL DEFAULT '0',\n\t\t    `date_enqueued` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,\n\t\t    `date_processed` TIMESTAMP NULL,\n\t\t    PRIMARY KEY (id),\n\t\t    INDEX status (status),\n\t\t    INDEX date_processed (date_processed)\n\t\t)\n\t\tENGINE='InnoDB'\n\t\tCOLLATE='utf8mb4_unicode_520_ci';",
}

// w3tcCDNCreate は、W3 Total Cache 2.10.7 が CDN を有効にしたときに作る表（Cdn_Environment.php。$charset_collate は
// WordPress の既定）。
const w3tcCDNCreate = `CREATE TABLE IF NOT EXISTS ` + "`wp_w3tc_cdn_pathmap`" + ` (
			-- Relative file path.
			-- For reference, not actually used for finding files.
			path TEXT NOT NULL,
			-- MD5 hash of remote path, used for finding files.
			path_hash VARCHAR(32) CHARACTER SET ascii NOT NULL,
			type tinyint(1) NOT NULL DEFAULT '0',
			-- Google Drive: document identifier
			remote_id VARCHAR(200) CHARACTER SET ascii,
			PRIMARY KEY (path_hash),
			KEY ` + "`remote_id`" + ` (` + "`remote_id`" + `)
			) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci`

// MySQL が受け付ける、引用符で囲んだエンジン名を受け付ける（段階 57）。
func TestQuotedEngine(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	for _, q := range wpMailSMTPCreates {
		mustExec(t, m, q)
	}
	mustExec(t, m, "INSERT INTO wp_wpmailsmtp_emails_queue (data) VALUES ('mail')")
	if got := one(t, m, "SELECT data FROM wp_wpmailsmtp_emails_queue WHERE status = 0"); got != "mail" {
		t.Errorf("queue row: %s", got)
	}
	if got := one(t, m, "SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'wp_wpmailsmtp_emails_queue'"); got != "utf8mb4_unicode_520_ci" {
		t.Errorf("queue collation: %s", got)
	}
	for _, q := range []string{
		`CREATE TABLE e1 (id int NOT NULL PRIMARY KEY) ENGINE="InnoDB"`,
		"CREATE TABLE e2 (id int NOT NULL PRIMARY KEY) ENGINE 'MyISAM'",
		"ALTER TABLE e1 ENGINE='InnoDB'",
	} {
		mustExec(t, m, q)
	}
	// 条件の文字列は書き換えない（書き換えると InnoDB という列を探してエラーになる）
	if got := one(t, m, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND engine = 'InnoDB' AND table_name LIKE 'e%'"); got != "2" {
		t.Errorf("tables with engine = 'InnoDB': %s", got)
	}
}

// ascii の表と列は、utf8mb4 の同じ名前の照合順序として受け入れ、警告を出す（段階 57）。MySQL 8.0 で、ASCII の 128 文字
// について ascii_general_ci と utf8mb4_general_ci、ascii_bin と utf8mb4_bin の並びと等しい組が同じことを確かめた。
func TestASCIIStoredAsUTF8MB4(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	c, err := m.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	warned := func(q, want string) {
		t.Helper()
		if _, err := c.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		var all []string
		rows, err := c.QueryContext(t.Context(), "SHOW WARNINGS")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
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
		return columnCollations(t, m, table)
	}

	warned(w3tcCDNCreate, "character set ascii is stored as utf8mb4 with the same collation name (column path_hash, column remote_id)")
	if got := collations("wp_w3tc_cdn_pathmap"); got != "utf8mb4_unicode_520_ci path=utf8mb4_unicode_520_ci path_hash=utf8mb4_general_ci remote_id=utf8mb4_general_ci" {
		t.Errorf("wp_w3tc_cdn_pathmap: %s", got)
	}
	mustExec(t, m, "INSERT INTO wp_w3tc_cdn_pathmap (path, path_hash, remote_id) VALUES ('a.png', '0cc175b9c0f1b6a831c399e269772661', 'X1')")
	if got := one(t, m, "SELECT path FROM wp_w3tc_cdn_pathmap WHERE remote_id = 'x1'"); got != "a.png" {
		t.Errorf("ascii_general_ci lookup: %s", got)
	}
	// 主キー（ascii_general_ci の VARCHAR）は、大小だけ違う値を同じとみなす
	if got := one(t, m, "SELECT path FROM wp_w3tc_cdn_pathmap WHERE path_hash = '0CC175B9C0F1B6A831C399E269772661'"); got != "a.png" {
		t.Errorf("primary key lookup: %s", got)
	}
	if _, err := m.Exec("INSERT INTO wp_w3tc_cdn_pathmap (path, path_hash) VALUES ('b.png', '0CC175B9C0F1B6A831C399E269772661')"); err == nil {
		t.Error("ascii_general_ci primary key accepted a case-only duplicate")
	}

	// 表の既定、ascii_bin、一意のキー、ALTER。utf8mb3 と ascii が混ざれば、文字コードごとに警告する。
	warned("CREATE TABLE a1 (id int NOT NULL PRIMARY KEY, g char(3) NOT NULL, b varchar(20) CHARACTER SET ascii COLLATE ascii_bin, u varchar(20) CHARACTER SET utf8, UNIQUE KEY g (g)) DEFAULT CHARSET=ascii",
		"character set ascii is stored as utf8mb4 with the same collation name (the table default, column g, column b)")
	warned("CREATE TABLE a2 (id int NOT NULL PRIMARY KEY, u varchar(20) CHARACTER SET utf8) DEFAULT CHARSET=ascii", "character set utf8mb3 is stored as utf8mb4 with the same collation name (column u)")
	if got := collations("a1"); got != "utf8mb4_general_ci g=utf8mb4_general_ci b=utf8mb4_bin u=utf8mb4_general_ci" {
		t.Errorf("a1: %s", got)
	}
	mustExec(t, m, "INSERT INTO a1 (id, g, b) VALUES (1, 'jpn', 'Key')")
	if _, err := m.Exec("INSERT INTO a1 (id, g) VALUES (2, 'JPN')"); err == nil {
		t.Error("ascii_general_ci unique key accepted a case-only duplicate")
	}
	if got := one(t, m, "SELECT COUNT(*) FROM a1 WHERE b = 'key'"); got != "0" {
		t.Errorf("ascii_bin ignores case: %s", got)
	}
	warned("ALTER TABLE a1 ADD COLUMN h char(32) CHARACTER SET ascii NOT NULL DEFAULT ''", "column h")
	warned("ALTER TABLE a1 MODIFY u varchar(20) CHARACTER SET ascii COLLATE ascii_bin", "column u")
	mustExec(t, m, "CREATE TABLE a3 (id int NOT NULL PRIMARY KEY, s varchar(20)) DEFAULT CHARSET=utf8mb4")
	warned("ALTER TABLE a3 DEFAULT CHARACTER SET ascii", "the table default")
	mustExec(t, m, "CREATE TABLE a4 (id int NOT NULL PRIMARY KEY, s varchar(20) CHARACTER SET ascii)")
	mustExec(t, m, "ALTER TABLE a4 CONVERT TO CHARACTER SET ascii")
	if got := collations("a4"); got != "utf8mb4_general_ci s=utf8mb4_general_ci" {
		t.Errorf("a4: %s", got)
	}
	var name, stmt string
	if err := m.QueryRow("SHOW CREATE TABLE a1").Scan(&name, &stmt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stmt, "ascii") || !strings.Contains(stmt, "utf8mb4_bin") {
		t.Errorf("SHOW CREATE TABLE:\n%s", stmt)
	}
}

// columnCollations は、表の既定の照合順序と、照合順序のある列の「列=照合順序」を並べて返す。
func columnCollations(t *testing.T, m *sql.DB, table string) string {
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
