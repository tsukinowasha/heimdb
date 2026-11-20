package backend

import "testing"

// likeScript は、エスケープを含む LIKE（WordPress の $wpdb->esc_like の結果）を本物の MySQL と比べる文の列。
// SQL の文字列の中の \\ は、値では \ になる（wpdb::prepare は esc_like の \ をさらにエスケープして送る）。
var likeScript = []string{
	"INSERT INTO wp_options (option_name, option_value) VALUES ('_transient_x', '1'), ('_transient_timeout_x', '1'), ('_site_transient_y', '1'), ('atransientbx', '1'), ('100%', '1'), ('100x', '1')",
	`SELECT option_name FROM wp_options WHERE option_name LIKE '\\_transient\\_%' ORDER BY option_name`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE '\\_transient\\_x'`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE '%\\_transient\\_%' ORDER BY option_name`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE '100\\%'`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE '_transient_x' ORDER BY option_name`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE '|_transient|_%' ESCAPE '|' ORDER BY option_name`,
	`DELETE FROM wp_options WHERE option_name LIKE '\\_transient\\_timeout\\_%'`,
	`SELECT COUNT(*) FROM wp_options WHERE option_name LIKE '\\_transient%'`,
	// サブクエリ、派生テーブル、WITH、UNION、定数の式のパターン
	"INSERT INTO wp_options (option_name, option_value) VALUES ('_transient_timeout_z', '1')",
	`SELECT option_name FROM (SELECT option_name FROM wp_options WHERE option_name LIKE '\\_transient\\_%') d ORDER BY option_name`,
	`WITH c AS (SELECT option_name FROM wp_options WHERE option_name LIKE '\\_transient\\_%') SELECT option_name FROM c ORDER BY option_name`,
	`SELECT option_name FROM wp_options WHERE option_id IN (SELECT option_id FROM wp_options WHERE option_name LIKE '\\_transient\\_%') ORDER BY option_name`,
	`SELECT COUNT(*) FROM wp_options o WHERE EXISTS (SELECT 1 FROM wp_options p WHERE p.option_id = o.option_id AND p.option_name LIKE '\\_transient\\_%')`,
	`SELECT (SELECT COUNT(*) FROM wp_options WHERE option_name LIKE '\\_transient\\_%')`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE '\\_transient\\_x' UNION SELECT option_name FROM wp_options WHERE option_name LIKE '100\\%' ORDER BY 1`,
	`SELECT option_name FROM wp_options WHERE option_name LIKE CONCAT('\\_transient', '\\_%') ORDER BY option_name`,
	`SELECT option_name FROM wp_options WHERE option_name NOT LIKE '\\_%' AND option_name LIKE '%transient%' ORDER BY option_name`,
	// 照合順序を指定せずに作ったテーブル
	"CREATE TABLE plain_t (id int NOT NULL PRIMARY KEY, name varchar(50), body text)",
	"INSERT INTO plain_t VALUES (1, '_transient_a', 'hello world'), (2, 'other', 'bye')",
	`SELECT id FROM plain_t WHERE name LIKE '\\_transient\\_%'`,
	`SELECT id FROM plain_t WHERE name LIKE '%trans%'`,
	`SELECT id FROM plain_t WHERE body LIKE '%world'`,
	"SELECT id FROM plain_t WHERE name = '_transient_a'",
	`SHOW TABLES LIKE 'wp\\_options'`,
	`SHOW TABLES LIKE 'wp\\_term%'`,
}

func TestLikeEscapesMatchMySQL(t *testing.T) {
	stmts := append(append([]string{wpSQLMode}, wpSchema()...), likeScript...)
	checkGolden(t, "../testdata/like_golden.txt", "heimdb_like_golden", stmts)
}
