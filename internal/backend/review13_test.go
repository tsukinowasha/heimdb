package backend

import (
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 13 のレビューの再現テスト。

// 同じ dbDelta を両側で流し、既定値が CURRENT_TIMESTAMP の列が両側に入った。列の定義は同じなので列の変更はないが、
// 基準の行にはその列がなく、今の行の値は両側で違う時刻になる。行の変更と見なしてはいけない。
func TestReview13TimestampColumnOnBothSides(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN created datetime NOT NULL DEFAULT CURRENT_TIMESTAMP")
	time.Sleep(1100 * time.Millisecond) // datetime は秒の精度
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN created datetime NOT NULL DEFAULT CURRENT_TIMESTAMP")
	mustExec(t, staging, "UPDATE wp_plug SET b = 'two (staging)' WHERE id = 2")

	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	if strings.Contains(rows, "conflict") || !strings.Contains(rows, "wp_plug|id=2|update|none|update||b|") || strings.Contains(rows, "id=1|") {
		t.Errorf("rows:\n%s", rows)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := one(t, mainDB, "SELECT b FROM wp_plug WHERE id = 2"); got != "two (staging)" {
		t.Errorf("b = %q", got)
	}
}

// 両側で同じ名前の列を違う既定値で足し、親の側に解決した。ほかに列の変更があるので target が立つ。親の行の c は親の
// 既定値のままでなければならない（ブランチの ALTER が入れた値は変更ではない）。
func TestReview13BothSidesDefaultResolvedParent(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN c varchar(10) NOT NULL DEFAULT 'b'")
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN d int NOT NULL DEFAULT 0")
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN c varchar(10) NOT NULL DEFAULT 'p'")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'COLUMN c', 'parent')")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	for _, line := range strings.Split(rows, "\n") {
		if strings.HasPrefix(line, "wp_plug|id=") {
			t.Errorf("spurious row change: %s", line)
		}
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(c ORDER BY id) FROM wp_plug"); got != "p,p" {
		t.Errorf("c after merge = %q, want p,p", got)
	}
}

// 同じく、ブランチの側に解決した。親の行は変わっていないので、ブランチの削除は競合にならない。
func TestReview13BothSidesDefaultResolvedBranch(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN c varchar(10) NOT NULL DEFAULT 'b'")
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN d int NOT NULL DEFAULT 0")
	mustExec(t, staging, "DELETE FROM wp_plug WHERE id = 1")
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN c varchar(10) NOT NULL DEFAULT 'p'")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_plug', 'COLUMN c', 'branch')")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	if !strings.Contains(rows, "wp_plug|id=1|delete|none|delete|") {
		t.Errorf("rows:\n%s", rows)
	}
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_plug WHERE id = 1"); got != "0" {
		t.Errorf("id=1 still on the parent")
	}
}

// ENUM の値の並びを変える MODIFY。行の値は添字（uint16）で保存されているので、添字で変換すると別の値になる。
func TestReview13EnumReorder(t *testing.T) {
	m, dsn, stopServer := branchServer(t)
	defer stopServer()
	mustExec(t, m, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, m, "CREATE TABLE wp_en (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, s enum('a','b','c') NOT NULL)")
	mustExec(t, m, "INSERT INTO wp_en (s) VALUES ('b')")
	mustExec(t, m, "CALL heimdb_branch_create('staging')")
	s := openBranch(t, dsn, "staging")
	defer s.Close()
	mustExec(t, s, "ALTER TABLE wp_en MODIFY s enum('x','a','b','c') NOT NULL")
	if got := one(t, s, "SELECT s FROM wp_en WHERE id = 1"); got != "b" {
		t.Logf("after the branch's own ALTER (go-mysql-server rewrite): s = %q", got)
	}
	mustExec(t, m, "INSERT INTO wp_en (s) VALUES ('c')") // 本番だけの行（差分に出ないので、書き直しの変換だけを通る）
	rows := strings.Join(diffRows(t, m, "CALL heimdb_branch_diff('staging', 'wp_en')"), "\n")
	if strings.Contains(rows, "id=1|") {
		t.Errorf("spurious row change (the branch did not change the row's value):\n%s", rows)
	}
	if _, err := m.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	for id, want := range map[string]string{"1": "b", "2": "c"} {
		if got := one(t, m, "SELECT s FROM wp_en WHERE id = "+id); got != want {
			t.Errorf("parent after merge: id=%s s = %q, want %q", id, got, want)
		}
	}
}

// 親で消した列にブランチがインデックスを足した。マージは panic せずに明示的なエラーになり、何も書かない。
func TestReview13IndexOnDroppedColumn(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD KEY kold2 (old)")
	mustExec(t, staging, "UPDATE wp_plug SET b = 'two (staging)' WHERE id = 2")
	mustExec(t, mainDB, "ALTER TABLE wp_plug DROP COLUMN old")
	rows := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_plug')"), "\n")
	t.Logf("diff:\n%s", rows)
	_, err := mainDB.Exec("CALL heimdb_branch_merge('staging')")
	t.Logf("merge: %v", err)
	if got := one(t, mainDB, "SELECT b FROM wp_plug WHERE id = 2"); err != nil && got != "two" {
		t.Errorf("a failed merge wrote rows: b = %s", got)
	}
}

// 既定値が CURRENT_TIMESTAMP の列を足した後、移行処理がその列を明示的に書き換えた。基準の行にない列は今の値で
// 埋めるので、この書き換えはマージされない（設計上の制限。docs/plan.md の段階 13）。
func TestReview13MigrationAfterTimestampColumn(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN created datetime NOT NULL DEFAULT CURRENT_TIMESTAMP")
	mustExec(t, staging, "UPDATE wp_plug SET created = '2020-01-01 00:00:00'")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT COUNT(*) FROM wp_plug WHERE created = '2020-01-01 00:00:00'"); got != "0" {
		t.Errorf("the limitation changed (update docs/plan.md): %s rows carry the migrated value", got)
	}
}

// 1 つの ALTER で列の追加と削除をすると、既存の行の足した列に既定値が入るか（マージではなく ALTER の確認）。
// 操作ごとの書き直しが前の書き直しの結果を読めず、行がすべて消えていた。
func TestReview13MultiActionAlter(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN e1 int NOT NULL DEFAULT 1, ADD COLUMN e2 int NOT NULL DEFAULT 2")
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(CONCAT_WS(':', id, b, e1, e2) ORDER BY id) FROM wp_plug"); got != "1:one:1:2,2:two:1:2" {
		t.Errorf("main after two ADDs in one ALTER: %q", got)
	}
	mustExec(t, mainDB, "BEGIN")
	mustExec(t, mainDB, "INSERT INTO wp_plug (a, b) VALUES (3, 'three')")
	mustExec(t, mainDB, "ALTER TABLE wp_plug DROP COLUMN e1, DROP COLUMN e2") // 実行中のトランザクションを暗黙にコミットする
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(b ORDER BY id) FROM wp_plug"); got != "one,two,three" {
		t.Errorf("main after two DROPs in one ALTER: %q", got)
	}
	mustExec(t, mainDB, "DELETE FROM wp_plug WHERE id = 3")
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN c varchar(20) NOT NULL DEFAULT 'def', DROP COLUMN old")
	if got := one(t, staging, "SELECT GROUP_CONCAT(CONCAT(id, ':', IFNULL(c, 'NULL')) ORDER BY id) FROM wp_plug"); got != "1:def,2:def" {
		t.Errorf("staging after ADD + DROP in one ALTER: %q (count %s)\n%s", got, one(t, staging, "SELECT COUNT(*) FROM wp_plug"), columnShape(t, staging, "wp_plug"))
	}
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN c varchar(20) NOT NULL DEFAULT 'def', DROP COLUMN old")
	if got := one(t, mainDB, "SELECT GROUP_CONCAT(CONCAT(id, ':', IFNULL(c, 'NULL')) ORDER BY id) FROM wp_plug"); got != "1:def,2:def" {
		t.Errorf("main after ADD + DROP in one ALTER: %q (count %s)", got, one(t, mainDB, "SELECT COUNT(*) FROM wp_plug"))
	}
}

// 書き直した後の親で、マージの前の時点の AS OF、復元、GC が動く。ブランチも読める。
func TestReview13RewriteThenAsOfRestoreGC(t *testing.T) {
	var st *store.Store
	m, dsn, stopServer := branchServer(t, func(s *store.Store) { st = s })
	defer stopServer()
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')",
		"CREATE TABLE wp_plug (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a int NOT NULL, old varchar(10) NOT NULL DEFAULT '', KEY ka (a))",
		"INSERT INTO wp_plug (a, old) VALUES (1, 'x'), (2, 'y')",
	} {
		mustExec(t, m, q)
	}
	mustExec(t, m, "CALL heimdb_branch_create('staging')")
	s := openBranch(t, dsn, "staging")
	defer s.Close()
	cut := time.Now()
	time.Sleep(30 * time.Millisecond)
	t0 := markLocal(t)
	mustExec(t, s, "ALTER TABLE wp_plug ADD COLUMN c varchar(20) NOT NULL DEFAULT 'def'")
	mustExec(t, s, "ALTER TABLE wp_plug DROP COLUMN old")
	mustExec(t, s, "INSERT INTO wp_plug (a, c) VALUES (3, 'branch')")
	mustExec(t, m, "INSERT INTO wp_plug (a, old) VALUES (7, 'prod')")
	diffRows(t, m, "CALL heimdb_branch_merge('staging')")

	if got := one(t, m, "SELECT GROUP_CONCAT(CONCAT(a, ':', c) ORDER BY a) FROM wp_plug"); got != "1:def,2:def,3:branch,7:def" {
		t.Errorf("after merge: %s", got)
	}
	if got := one(t, m, "SELECT GROUP_CONCAT(CONCAT(a, ':', old) ORDER BY a) FROM wp_plug AS OF '"+t0+"'"); got != "1:x,2:y" {
		t.Errorf("AS OF before the merge: %s", got)
	}
	if got := one(t, s, "SELECT GROUP_CONCAT(CONCAT(a, ':', c) ORDER BY a) FROM wp_plug"); got != "1:def,2:def,3:branch" {
		t.Errorf("staging after merge: %s", got)
	}
	stats, err := st.GC(time.Since(cut))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gc: %+v", stats)
	if got := one(t, s, "SELECT GROUP_CONCAT(CONCAT(a, ':', c) ORDER BY a) FROM wp_plug"); got != "1:def,2:def,3:branch" {
		t.Errorf("staging after GC: %s", got)
	}
	if got := one(t, m, "SELECT GROUP_CONCAT(CONCAT(a, ':', c) ORDER BY a) FROM wp_plug"); got != "1:def,2:def,3:branch,7:def" {
		t.Errorf("main after GC: %s", got)
	}
	preview := strings.Join(diffRows(t, m, "CALL heimdb_restore_preview('main', '"+t0+"')"), "\n")
	t.Logf("restore preview:\n%s", preview)
	diffRows(t, m, "CALL heimdb_restore('main', '"+t0+"')")
	if got := one(t, m, "SELECT GROUP_CONCAT(CONCAT(a, ':', old) ORDER BY a) FROM wp_plug"); got != "1:x,2:y" {
		t.Errorf("after restore: %s", got)
	}
	mustExec(t, m, "INSERT INTO wp_plug (a, old) VALUES (8, 'z')")
	if got := one(t, m, "SELECT id FROM wp_plug WHERE a = 8"); got != "4" {
		t.Errorf("auto increment after restore: %s", got)
	}
}

// 親だけで足した列があるとき、同じ自然なキーの記事をブランチの側に解決して親の行に写すと、親だけの列の値が
// 既定値で上書きされてはいけない。
func TestReview13CopyKeepsParentOnlyColumn(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	for _, q := range []string{
		insertPost + "(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{\"staging\":1}', '', '', '', '')",
		"UPDATE wp_posts SET post_name = 'wp-global-styles-x' WHERE post_type = 'wp_global_styles'",
	} {
		mustExec(t, staging, q)
	}
	for _, q := range []string{
		"ALTER TABLE wp_posts ADD COLUMN prod_only varchar(10) NOT NULL DEFAULT 'p'",
		insertPost + "(NULL, 1, 'Custom Styles', 'publish', 'wp_global_styles', '{\"prod\":1}', '', '', '', '')",
		"UPDATE wp_posts SET post_name = 'wp-global-styles-x', prod_only = 'keep' WHERE post_type = 'wp_global_styles'",
	} {
		mustExec(t, mainDB, q)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=10000000000', 'branch')")
	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging')"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := one(t, mainDB, "SELECT CONCAT(post_content, '/', prod_only) FROM wp_posts WHERE post_type = 'wp_global_styles'"); got != "{\"staging\":1}/keep" {
		t.Errorf("after copy: %q", got)
	}
}

// ALTER で足した列と、マージで足したり型を変えたりした列は、テーブルの照合順序になる（MySQL と同じ）。
func TestReview13AddedColumnCollation(t *testing.T) {
	mainDB, staging, stop := colFixture(t)
	defer stop()
	mustExec(t, mainDB, "ALTER TABLE wp_plug ADD COLUMN x varchar(10) NOT NULL DEFAULT ''")
	mustExec(t, staging, "ALTER TABLE wp_plug MODIFY note varchar(40) NULL")
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN tx longtext")
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	for _, c := range []string{"x", "note", "tx"} {
		if got := one(t, mainDB, "SELECT COLLATION_NAME FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'wp_plug' AND column_name = '"+c+"'"); got != "utf8mb4_unicode_520_ci" {
			t.Errorf("collation of %s = %s", c, got)
		}
	}
}
