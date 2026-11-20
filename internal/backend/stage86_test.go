package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// deleteExpiredTransients は、WordPress 本体の delete_expired_transients が流す文（wp-includes/option.php）。now は
// 期限の時刻。
func deleteExpiredTransients(now int) []string {
	return []string{
		fmt.Sprintf("DELETE a, b FROM wp_options a, wp_options b WHERE a.option_name LIKE '\\_transient\\_%%' AND a.option_name NOT LIKE '\\_transient\\_timeout\\_%%' AND b.option_name = CONCAT( '_transient_timeout_', SUBSTRING( a.option_name, 12 ) ) AND b.option_value < %d", now),
		fmt.Sprintf("DELETE a, b FROM wp_options a, wp_options b WHERE a.option_name LIKE '\\_site\\_transient\\_%%' AND a.option_name NOT LIKE '\\_site\\_transient\\_timeout\\_%%' AND b.option_name = CONCAT( '_site_transient_timeout_', SUBSTRING( a.option_name, 17 ) ) AND b.option_value < %d", now),
	}
}

// stage86Script は、期限切れの transient を消す文と、同じ条件の SELECT（docs/plan.md の段階 86）。期限切れの組、
// 期限の来ていない組、期限の行のない値の行、値の行のない期限の行、SUBSTRING の位置が効く名前を入れる。
func stage86Script() []string {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	var rows []string
	add := func(name, value string) {
		rows = append(rows, fmt.Sprintf("('%s', '%s', 'off')", name, value))
	}
	for i := 0; i < 20; i++ {
		exp := 1000 + i*100 // now = 2000 で、i < 10 が期限切れ
		add(fmt.Sprintf("_transient_k%d", i), "v")
		add(fmt.Sprintf("_transient_timeout_k%d", i), fmt.Sprint(exp))
		add(fmt.Sprintf("_site_transient_s%d", i), "v")
		add(fmt.Sprintf("_site_transient_timeout_s%d", i), fmt.Sprint(exp))
	}
	add("_transient_orphan", "v")                 // 期限の行がない
	add("_transient_timeout_lonely", "1")         // 値の行がない
	add("_transient_timeout_", "1")               // 名前が空の transient の期限
	add("_transient_", "v")                       // 名前が空の transient
	add("_transientx_k1", "v")                    // LIKE の \_ の確かめ
	add("_site_transient_timeout_k1", "1")        // _transient_ の期限と取り違えない
	add("siteurl", "http://example.com")          // ふつうの設定
	add("_transient_timeout_k0x", "not-a-number") // 数でない期限
	add("_transient_k0x", "v")
	stmts = append(stmts, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES "+strings.Join(rows, ", "))
	stmts = append(stmts,
		"SELECT a.option_name, b.option_name FROM wp_options a, wp_options b WHERE a.option_name LIKE '\\_transient\\_%' AND a.option_name NOT LIKE '\\_transient\\_timeout\\_%' AND b.option_name = CONCAT( '_transient_timeout_', SUBSTRING( a.option_name, 12 ) ) AND b.option_value < 2000 ORDER BY a.option_name",
		"SELECT a.option_name, b.option_name FROM wp_options a, wp_options b WHERE a.option_name LIKE '\\_site\\_transient\\_%' AND a.option_name NOT LIKE '\\_site\\_transient\\_timeout\\_%' AND b.option_name = CONCAT( '_site_transient_timeout_', SUBSTRING( a.option_name, 17 ) ) AND b.option_value < 2000 ORDER BY a.option_name",
	)
	for _, q := range deleteExpiredTransients(2000) {
		stmts = append(stmts, q, "SELECT option_name FROM wp_options ORDER BY option_name")
	}
	// 期限切れがもうないとき
	stmts = append(stmts, deleteExpiredTransients(2000)...)
	// 1 つの行が何度も結び付くとき、影響を受けた行の数は消した行の数（siteurl は 1 回だけ数える）
	stmts = append(stmts,
		"INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('m_a', 'x', 'off'), ('m_b', 'x', 'off'), ('m_c', 'x', 'off')",
		"DELETE a, b FROM wp_options a, wp_options b WHERE a.option_name = 'siteurl' AND b.option_name LIKE 'm\\_%'",
		"SELECT option_name FROM wp_options WHERE option_name = 'siteurl' OR option_name LIKE 'm\\_%'",
	)
	// LEFT JOIN で相手のない側は消さない（前は主キーが NULL の行を消そうとしてエラーになった）
	stmts = append(stmts,
		"CREATE TABLE l1 (id int PRIMARY KEY, v int)",
		"CREATE TABLE l2 (id int PRIMARY KEY, v int)",
		"INSERT INTO l1 VALUES (1, 1), (2, 2), (3, 3)",
		"INSERT INTO l2 VALUES (1, 1), (5, 5)",
		"DELETE l1, l2 FROM l1 LEFT JOIN l2 ON l1.id = l2.id WHERE l1.v < 3",
		"SELECT id FROM l1 ORDER BY id",
		"SELECT id FROM l2 ORDER BY id",
	)
	// 同じ表を 2 つの別名で消し、1 つの行が両方の別名で消える自己結合は、その行を 1 度だけ数える
	stmts = append(stmts,
		"CREATE TABLE o (id int PRIMARY KEY, name varchar(50), val varchar(50))",
		"INSERT INTO o VALUES (1, 'x', 'v'), (2, 'timeout_x', 'v'), (3, 'timeout_timeout_x', 'v')",
		"DELETE a, b FROM o a, o b WHERE b.name = CONCAT('timeout_', a.name)",
		"SELECT id FROM o ORDER BY id",
	)
	// 複数の表の UPDATE で、列と式の等号
	stmts = append(stmts,
		"UPDATE wp_options a, wp_options b SET a.option_value = 'renewed' WHERE a.option_name LIKE '\\_transient\\_%' AND a.option_name NOT LIKE '\\_transient\\_timeout\\_%' AND b.option_name = CONCAT( '_transient_timeout_', SUBSTRING( a.option_name, 12 ) )",
		"SELECT option_name, option_value FROM wp_options WHERE option_value = 'renewed' ORDER BY option_name",
	)
	return stmts
}

// 期限切れの transient を消す文と、同じ条件の SELECT の結果（影響を受けた行の数を含む）が、本物の MySQL と同じ。
func TestStage86MatchMySQL(t *testing.T) {
	checkGolden(t, "../testdata/stage86_golden.txt", "heimdb_golden86", stage86Script())
}

// 期限切れの transient を消す文は、b を option_name の索引で引き、全部の組み合わせを作らない。3 万 4 千行の
// wp_options でも数秒で終わる（前は 20 分を超えた）。
func TestStage86DeleteExpiredTransients(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		mustExec(t, db, q)
	}
	const pairs = 17000
	for start := 0; start < pairs; start += 1000 {
		var rows []string
		for i := start; i < start+1000; i++ {
			rows = append(rows, fmt.Sprintf("('_transient_k%d', 'v', 'off'), ('_transient_timeout_k%d', '1000', 'off')", i, i))
		}
		mustExec(t, db, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES "+strings.Join(rows, ", "))
	}
	mustExec(t, db, "ANALYZE TABLE wp_options")
	for _, q := range deleteExpiredTransients(2000) {
		checkPlan(t, db, "delete_expired_transients", q, []string{"LookupJoin", "index: [wp_options.option_name]", "keys: concat("}, []string{"CrossJoin"})
	}
	start := time.Now()
	res, err := db.Exec(deleteExpiredTransients(2000)[0])
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2*pairs {
		t.Errorf("rows affected = %d, want %d", n, 2*pairs)
	}
	// 組を全部比べていたころは、同じ 3 万 4 千行で 20 分を超えた。引く形でも、GitHub Actions の -race では 15 秒ほど
	// かかる。
	if d := time.Since(start); d > 60*time.Second {
		t.Errorf("deleting %d expired transients took %v", pairs, d)
	}
	var left int
	if err := db.QueryRow("SELECT COUNT(*) FROM wp_options WHERE option_name LIKE '\\_transient\\_%'").Scan(&left); err != nil || left != 0 {
		t.Errorf("transients left = %d, %v", left, err)
	}
}

// サイトヘルスの information_schema.TABLES の問い合わせは、WHERE で名前を決めた表の行だけを数える。数えた行数は
// 正確なまま。
func TestStage86InformationSchemaTablesOnlyNamed(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE named (id int PRIMARY KEY)")
	s.mustQuery(t, "CREATE TABLE other (id int PRIMARY KEY)")
	s.mustQuery(t, "INSERT INTO named VALUES (1), (2), (3)")
	s.mustQuery(t, "INSERT INTO other VALUES (1), (2)")
	counted := 0
	countedTables = func(name string) { counted++ }
	defer func() { countedTables = nil }()
	rows := s.mustQuery(t, fmt.Sprintf("SELECT TABLE_NAME, TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_SCHEMA = '%s' AND TABLE_NAME IN ('named', 'missing')", testDB))
	if len(rows) != 1 || rows[0][0] != "named" || fmt.Sprint(rows[0][1]) != "3" {
		t.Errorf("rows = %v", rows)
	}
	if counted != 1 {
		t.Errorf("counted %d tables, want 1", counted)
	}
	counted = 0
	rows = s.mustQuery(t, fmt.Sprintf("SELECT t.TABLE_NAME, t.TABLE_ROWS FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = '%s' ORDER BY t.TABLE_NAME", testDB))
	if len(rows) != 2 || counted != 2 {
		t.Errorf("schema only: rows = %v, counted %d", rows, counted)
	}
}
