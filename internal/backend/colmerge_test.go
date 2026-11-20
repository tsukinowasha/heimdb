package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// columnShape は SHOW FULL COLUMNS の、名前、型、照合順序、NULL、キー、既定値、Extra を並びの順に返す。
func columnShape(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	return strings.Join(diffRows(t, db, "SHOW FULL COLUMNS FROM "+table), "\n")
}

func colFixture(t *testing.T) (mainDB, staging *sql.DB, stop func()) {
	t.Helper()
	m, dsn, stopServer := branchServer(t)
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')",
		"CREATE TABLE wp_plug (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a int NOT NULL, b varchar(50) NOT NULL, note varchar(20) NULL, old varchar(10) NOT NULL DEFAULT '', KEY ka (a), KEY kold (old)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		"INSERT INTO wp_plug (a, b, note, old) VALUES (1, 'one', NULL, 'x'), (2, 'two', 'n2', 'y')",
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

func TestColumnMerge(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	for _, q := range []string{
		"ALTER TABLE wp_plug ADD COLUMN c varchar(20) NOT NULL DEFAULT 'def' AFTER a",
		"UPDATE wp_plug SET c = 'set' WHERE id = 2", // プラグインの移行処理が値を入れた
		"ALTER TABLE wp_plug MODIFY b varchar(255) NOT NULL",
		"ALTER TABLE wp_plug MODIFY a bigint NOT NULL",
		"UPDATE wp_plug SET note = '' WHERE note IS NULL",
		"ALTER TABLE wp_plug MODIFY note varchar(20) NOT NULL DEFAULT ''",
		"ALTER TABLE wp_plug ADD COLUMN created datetime NOT NULL DEFAULT CURRENT_TIMESTAMP",
		"ALTER TABLE wp_plug DROP COLUMN old",
		"INSERT INTO wp_plug (a, c, b) VALUES (3, 'branch', 'three')",
	} {
		mustExec(t, staging, q)
	}
	// その間に本番でも書き込みがある（NULL の note を含む）。
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b, note) VALUES (7, 'prod', NULL)")
	mustExec(t, mainDB, "UPDATE wp_plug SET b = 'one (prod)' WHERE id = 1")

	summary := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"), "\n")
	if !strings.Contains(summary, "wp_plug|changed|") || !strings.Contains(summary, "columns: ~a, +c, +created, ~note, -old, ~b") && !strings.Contains(summary, "columns: ~a, ~b, +c, +created, ~note, -old") {
		t.Errorf("summary:\n%s", summary)
	}
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	for _, want := range []string{
		"wp_plug|COLUMN c|insert|none|add column|||",
		"wp_plug|COLUMN old|delete|none|drop column|||",
		"wp_plug|COLUMN b|update|none|modify column|||",
		"wp_plug|id=2|update|none|update||c|",
		"wp_plug|id=1|update|update|update||note|", // ブランチの UPDATE note = '' と、本番の b の変更
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("rows lack %q:\n%s", want, rows)
		}
	}
	// 型を変えた列（a）と、CURRENT_TIMESTAMP で足した列（created）は、行の変更に見えない。
	for _, bad := range []string{"|a|", ",a|", "|a,", "created"} {
		for _, line := range strings.Split(rows, "\n") {
			if strings.HasPrefix(line, "wp_plug|id=") && strings.Contains(line, bad) {
				t.Errorf("spurious column change %q: %s", bad, line)
			}
		}
	}

	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	got, want := columnShape(t, mainDB, "wp_plug"), columnShape(t, staging, "wp_plug")
	if got != want {
		t.Errorf("columns after merge:\n%s\nwant (staging):\n%s", got, want)
	}
	for q, want := range map[string]string{
		"SELECT CONCAT(id, ':', a, ':', c, ':', b, ':', note) FROM wp_plug WHERE id = 1":                          "1:1:def:one (prod):",
		"SELECT CONCAT(id, ':', a, ':', c, ':', b, ':', note) FROM wp_plug WHERE id = 2":                          "2:2:set:two:n2",
		"SELECT CONCAT(a, ':', c, ':', b, ':', note) FROM wp_plug WHERE b = 'prod'":                               "7:def:prod:",
		"SELECT CONCAT(a, ':', c, ':', b) FROM wp_plug WHERE b = 'three'":                                         "3:branch:three",
		"SELECT COUNT(*) FROM wp_plug WHERE created IS NULL OR created = '0000-00-00 00:00:00'":                   "0",
		"SELECT GROUP_CONCAT(b ORDER BY id) FROM wp_plug WHERE a = 7":                                             "prod",
		"SELECT COUNT(*) FROM information_schema.statistics WHERE table_name = 'wp_plug' AND index_name = 'kold'": "0",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
	// AUTO_INCREMENT は書き直しの前の値を引き継ぐ。
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b) VALUES (8, 'after')")
	if got := one(t, mainDB, "SELECT id FROM wp_plug WHERE b = 'after'"); got != "4" {
		t.Errorf("auto increment after the rewrite: %s", got)
	}
	if rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"); len(rows) != 0 {
		t.Errorf("diff after merge:\n%s", strings.Join(rows, "\n"))
	}
}

func TestColumnMergeConflicts(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug MODIFY b varchar(100) NOT NULL")
	mustExec(t, mainDB, "ALTER TABLE wp_plug MODIFY b varchar(200) NOT NULL")
	mustExec(t, staging, "ALTER TABLE wp_plug MODIFY id bigint unsigned NOT NULL AUTO_INCREMENT COMMENT 'pk'")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	for _, want := range []string{
		"wp_plug|COLUMN b|update|update|conflict|||the same columns were changed on both sides",
		"wp_plug|COLUMN id|update|none|conflict|||primary key and AUTO_INCREMENT columns are not merged",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("rows lack %q:\n%s", want, rows)
		}
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_resolve('staging', 'wp_plug', 'COLUMN id', 'branch')"); err == nil {
		t.Error("a primary key column cannot take the branch's side")
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'COLUMN id', 'parent')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'COLUMN b', 'branch')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT COLUMN_TYPE FROM information_schema.columns WHERE table_schema = 'wordpress' AND table_name = 'wp_plug' AND column_name = 'b'"); got != "varchar(100)" {
		t.Errorf("b after resolving to the branch: %s", got)
	}
	if got := one(t, mainDB, "SELECT COLUMN_COMMENT FROM information_schema.columns WHERE table_schema = 'wordpress' AND table_name = 'wp_plug' AND column_name = 'id'"); got != "" {
		t.Errorf("id after resolving to the parent: %q", got)
	}
}

// 型を狭める変更で、本番の値が収まらなければ、マージを拒否して何も書かない。
func TestColumnMergeNarrowingRefused(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug MODIFY b varchar(5) NOT NULL")
	mustExec(t, staging, "UPDATE wp_options SET option_value = 'Staged' WHERE option_name = 'blogname'")
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b) VALUES (9, 'much too long')")
	before := columnShape(t, mainDB, "wp_plug")
	_, err := mainDB.Exec("CALL heimdb_branch_merge('staging')")
	if err == nil || !strings.Contains(err.Error(), "much too long") || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("narrowing merge: %v", err)
	}
	if got := columnShape(t, mainDB, "wp_plug"); got != before {
		t.Errorf("columns changed by a failed merge:\n%s", got)
	}
	if got := one(t, mainDB, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod" {
		t.Errorf("rows merged by a failed merge: %s", got)
	}
}

// 本番だけで足した列があっても、ブランチで足した行はその列を既定値にしてマージする。
func TestColumnMergeParentOnlyColumn(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN prod_only varchar(10) NOT NULL DEFAULT 'p'")
	mustExec(t, staging, "INSERT INTO wp_plug (a, b) VALUES (5, 'from staging')")
	mustExec(t, staging, "UPDATE wp_plug SET b = 'two (staging)' WHERE id = 2")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	for q, want := range map[string]string{
		"SELECT prod_only FROM wp_plug WHERE b = 'from staging'": "p",
		"SELECT prod_only FROM wp_plug WHERE id = 2":             "p",
		"SELECT b FROM wp_plug WHERE id = 2":                     "two (staging)",
	} {
		if got := one(t, mainDB, q); got != want {
			t.Errorf("%s = %q, want %q", q, got, want)
		}
	}
}
