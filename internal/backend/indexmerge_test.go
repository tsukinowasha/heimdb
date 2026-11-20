package backend

import (
	"database/sql"
	"sort"
	"strings"
	"testing"
)

// indexShape は、SHOW INDEX のうち定義に関わる列（名前、順、列、一意、プレフィックス長、種類）を名前順に返す。
func indexShape(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var out []string
	for _, r := range diffRows(t, db, "SHOW INDEX FROM "+table) {
		f := strings.Split(r, "|")
		out = append(out, strings.Join([]string{f[2], f[3], f[4], f[1], f[7], f[10]}, " "))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// indexFixture は、main に索引を試すテーブルを作り、ステージングを分岐する。
func indexFixture(t *testing.T) (mainDB, staging *sql.DB, stop func()) {
	t.Helper()
	m, dsn, stopServer := branchServer(t)
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')",
		"CREATE TABLE wp_plug (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a int NOT NULL, b varchar(50) NOT NULL, body text, KEY b (b), KEY old_a (a))",
		"INSERT INTO wp_plug (a, b, body) VALUES (5, 'x', 'hello'), (6, 'y', 'world')",
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

func TestIndexMerge(t *testing.T) {
	mainDB, staging, stop := indexFixture(t)
	defer stop()
	for _, q := range []string{
		"ALTER TABLE wp_plug ADD INDEX ia (a)",                     // 追加
		"ALTER TABLE wp_plug DROP INDEX old_a",                     // 削除
		"ALTER TABLE wp_plug DROP INDEX b, ADD INDEX b (b(10), a)", // 定義の変更（作り直し）
		"ALTER TABLE wp_plug ADD FULLTEXT KEY ft_body (body)",      // FULLTEXT
		"ALTER TABLE wp_postmeta ADD INDEX meta_value (meta_value(32))",
		"INSERT INTO wp_plug (a, b, body) VALUES (5, 'z', 'from staging')",
	} {
		mustExec(t, staging, q)
	}
	// 分岐の後に本番に入った行も、新しいインデックスで引ける必要がある。
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b, body) VALUES (5, 'w', 'from prod')")

	summary := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"), "\n")
	for _, want := range []string{
		"wp_plug|changed|1|0|0|0|0|0|0|indexes: ~b, +ft_body, +ia, -old_a",
		"wp_postmeta|changed|0|0|0|0|0|0|0|indexes: +meta_value",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	for _, want := range []string{
		"wp_plug|INDEX ia|insert|none|add index|||",
		"wp_plug|INDEX old_a|delete|none|drop index|||",
		"wp_plug|INDEX b|update|none|recreate index|||",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("rows lack %q:\n%s", want, rows)
		}
	}

	merged := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')"), "\n")
	if !strings.Contains(merged, "wp_plug|merged|1|0|0|0|0|0|0|indexes: ~b, +ft_body, +ia, -old_a") {
		t.Errorf("merge result:\n%s", merged)
	}
	for _, table := range []string{"wp_plug", "wp_postmeta"} {
		if got, want := indexShape(t, mainDB, table), indexShape(t, staging, table); got != want {
			t.Errorf("%s indexes after merge:\n%s\nwant (staging):\n%s", table, got, want)
		}
	}
	// 新しいインデックスのエントリは、本番の行とマージした行の両方を指す。
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(b ORDER BY b) FROM wp_plug WHERE a = 5"); got != "w,x,z" {
		t.Errorf("lookup through the new index: %s", got)
	}
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(a ORDER BY a) FROM wp_plug WHERE b = 'w'"); got != "5" {
		t.Errorf("lookup through the recreated index: %s", got)
	}
	// 2 回目の差分は空。
	if rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"); len(rows) != 0 {
		t.Errorf("diff after merge:\n%s", strings.Join(rows, "\n"))
	}
}

func TestIndexMergeConflicts(t *testing.T) {
	mainDB, staging, stop := indexFixture(t)
	defer stop()
	for _, q := range []string{
		"ALTER TABLE wp_plug ADD INDEX dual_idx (a)",
		"ALTER TABLE wp_plug DROP INDEX b",
		"ALTER TABLE wp_plug ADD INDEX same (a, b)",
	} {
		mustExec(t, staging, q)
	}
	for _, q := range []string{
		"ALTER TABLE wp_plug ADD INDEX dual_idx (b)",           // 同じ名前、違う定義
		"ALTER TABLE wp_plug DROP INDEX b, ADD INDEX b (b(5))", // ブランチが消し、親が変えた
		"ALTER TABLE wp_plug ADD INDEX same (a, b)",            // 同じ定義なら競合しない
	} {
		mustExec(t, mainDB, q)
	}
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	for _, want := range []string{
		"wp_plug|INDEX dual_idx|insert|insert|conflict|||added on both sides with different values",
		"wp_plug|INDEX b|delete|update|conflict|||deleted on the branch, changed on the parent",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("rows lack %q:\n%s", want, rows)
		}
	}
	if strings.Contains(rows, "INDEX same") {
		t.Errorf("an index added with the same definition on both sides is not a change:\n%s", rows)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "wp_plug INDEX b, wp_plug INDEX dual_idx") {
		t.Fatalf("merge with index conflicts: %v", err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'INDEX dual_idx', 'branch')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'INDEX b', 'parent')")
	// 解決した後にブランチで定義を変えたら、その解決は使わない。
	mustExec(t, staging, "ALTER TABLE wp_plug DROP INDEX dual_idx, ADD INDEX dual_idx (a, id)")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "INDEX dual_idx (stale resolution)") {
		t.Fatalf("stale index resolution: %v", err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'INDEX dual_idx', 'branch')")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	shape := indexShape(t, mainDB, "wp_plug")
	for _, want := range []string{"dual_idx 1 a 1", "dual_idx 2 id 1", "b 1 b 1 5"} {
		if !strings.Contains(shape, want) {
			t.Errorf("indexes after resolving lack %q:\n%s", want, shape)
		}
	}
}

// 一意インデックスを足すとき、親の行が重複していればマージ全体がエラーになり、何も書かない。
func TestIndexMergeUniqueViolation(t *testing.T) {
	mainDB, staging, stop := indexFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD UNIQUE KEY ua (a)")
	mustExec(t, staging, "UPDATE wp_options SET option_value = 'Staged' WHERE option_name = 'blogname'")
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b) VALUES (5, 'dup')")
	before := indexShape(t, mainDB, "wp_plug")
	_, err := mainDB.Exec("CALL heimdb_branch_merge('staging')")
	if err == nil || !strings.Contains(err.Error(), "unique index ua") {
		t.Fatalf("merge adding a violated unique index: %v", err)
	}
	if got := indexShape(t, mainDB, "wp_plug"); got != before {
		t.Errorf("indexes changed by a failed merge:\n%s", got)
	}
	if got := one(t, mainDB, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod" {
		t.Errorf("rows merged by a failed merge: %s", got)
	}
}

// マージは 1 つのコミット: ブランチの行が、ブランチで足した一意インデックスで本番の行と重複して失敗したら、
// インデックスもブランチで作ったテーブルも本番に残らない。
func TestIndexMergeIsAtomic(t *testing.T) {
	mainDB, staging, stop := indexFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD UNIQUE KEY ub (b)")
	mustExec(t, staging, "INSERT INTO wp_plug (a, b) VALUES (9, 'same')")
	mustExec(t, staging, "CREATE TABLE wp_newplug (id bigint unsigned NOT NULL PRIMARY KEY)")
	mustExec(t, staging, "INSERT INTO wp_newplug VALUES (1)")
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b) VALUES (8, 'same')")
	before := indexShape(t, mainDB, "wp_plug")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err == nil || !strings.Contains(err.Error(), "duplicate unique key") {
		t.Fatalf("merge: %v", err)
	}
	if got := indexShape(t, mainDB, "wp_plug"); got != before {
		t.Errorf("index left on the parent by a failed merge:\n%s", got)
	}
	if rows := diffRows(t, mainDB, "SHOW TABLES LIKE 'wp\\\\_newplug'"); len(rows) != 0 {
		t.Errorf("table left on the parent by a failed merge: %v", rows)
	}
	// 本番のデータを直せば、マージできる。
	mustExec(t, mainDB, "DELETE FROM wp_plug WHERE b = 'same'")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_newplug"); got != "1" {
		t.Errorf("new table after the retried merge: %s", got)
	}
	if !strings.Contains(indexShape(t, mainDB, "wp_plug"), "ub 1 b 0") {
		t.Errorf("unique index after the retried merge:\n%s", indexShape(t, mainDB, "wp_plug"))
	}
}
