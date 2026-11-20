package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// AUTO_INCREMENT の番号帯（docs/plan.md の段階 6 と段階 61）。
func TestAutoIncrementBand(t *testing.T) {
	const unbounded = ^uint64(0)
	for _, c := range []struct {
		branch uint64
		typ    querypb.Type
		lo, hi uint64
	}{
		{1, sqltypes.Int64, 1, 10_000_000_000},
		{2, sqltypes.Uint64, 10_000_000_000, 20_000_000_000},
		{1, sqltypes.Int32, 1, 1_000_000_000},
		{1, sqltypes.Uint32, 1, 1_000_000_000},
		{2, sqltypes.Int32, 1_000_000_000, 1_010_000_000},
		{3, sqltypes.Uint32, 1_010_000_000, 1_020_000_000},
		{101, sqltypes.Int32, 1_990_000_000, 2_000_000_000},
		{102, sqltypes.Int32, 1_000_000_000, 1_010_000_000}, // 枠を使い回す
		{2, sqltypes.Int24, 1, unbounded},
		{2, sqltypes.Uint24, 1, unbounded},
		{2, sqltypes.Int16, 1, unbounded},
		{2, sqltypes.Int8, 1, unbounded},
		{1<<62 + 5, sqltypes.Int32, 1, unbounded}, // 一時テーブル
		{1<<62 + 5, sqltypes.Int64, 1, unbounded},
	} {
		lo, hi := autoIncrementBand(c.branch, c.typ)
		if lo != c.lo || hi != c.hi {
			t.Errorf("autoIncrementBand(%d, %v) = [%d, %d), want [%d, %d)", c.branch, c.typ, lo, hi, c.lo, c.hi)
		}
	}
	// 帯は符号付きの int に収まる。
	if _, hi := autoIncrementBand(101, sqltypes.Int32); hi-1 > 2147483647 {
		t.Errorf("the last block ends at %d", hi-1)
	}
}

// 本番とブランチが別の番号を振るのは、64 ビットと 32 ビットの列だけ。24 ビット以下の列と一時テーブルは分けない。
// 明示した値と ALTER TABLE の書き直しの後も、カウンタは自分の帯の中で続く。
func TestIntAutoIncrementBands(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	tables := map[string]string{"t_int": "int(11)", "t_uint": "int(11) unsigned", "t_big": "bigint(20) unsigned", "t_medium": "mediumint(9)"}
	for name, typ := range tables {
		mustExec(t, mainDB, "CREATE TABLE "+name+" (id "+typ+" NOT NULL AUTO_INCREMENT, v varchar(20), PRIMARY KEY (id))")
		mustExec(t, mainDB, "INSERT INTO "+name+" (v) VALUES ('m1'), ('m2')")
	}
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	insert := func(db *sql.DB, table, v string) string {
		t.Helper()
		mustExec(t, db, "INSERT INTO "+table+" (v) VALUES ('"+v+"')")
		return one(t, db, "SELECT id FROM "+table+" WHERE v = '"+v+"'")
	}
	for table, want := range map[string]string{"t_int": "1000000000", "t_uint": "1000000000", "t_big": "10000000000", "t_medium": "3"} {
		if got := insert(staging, table, "s1"); got != want {
			t.Errorf("%s on the branch: %s, want %s", table, got, want)
		}
		if got := insert(mainDB, table, "m3"); got != "3" {
			t.Errorf("%s on main: %s, want 3", table, got)
		}
	}
	// 明示した値は、帯の中なら続きをそこからにし、帯の外なら（本番の番号）カウンタを動かさない。
	mustExec(t, staging, "INSERT INTO t_int (id, v) VALUES (1000000010, 'x1'), (50, 'x2')")
	if got := insert(staging, "t_int", "s2"); got != "1000000011" {
		t.Errorf("after explicit values: %s", got)
	}
	// 本番に帯の上の値があっても（マージで入ったブランチの行）、本番は 10^9 未満で続く。
	mustExec(t, mainDB, "INSERT INTO t_int (id, v) VALUES (1000000005, 'merged')")
	if got := insert(mainDB, "t_int", "m4"); got != "4" {
		t.Errorf("main after a branch ID: %s", got)
	}
	// 書き直す ALTER TABLE の後も、帯の中の続きから。
	mustExec(t, staging, "ALTER TABLE t_uint ADD COLUMN extra int NOT NULL DEFAULT 0")
	if got := insert(staging, "t_uint", "s2"); got != "1000000001" {
		t.Errorf("after ALTER TABLE on the branch: %s", got)
	}
	mustExec(t, mainDB, "ALTER TABLE t_uint ADD COLUMN extra int NOT NULL DEFAULT 0")
	if got := insert(mainDB, "t_uint", "m4"); got != "4" {
		t.Errorf("after ALTER TABLE on main: %s", got)
	}
	// 一時テーブルは分けない。
	conn, err := staging.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		"CREATE TEMPORARY TABLE tmp2 (id int NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO tmp2 (v) VALUES ('a')",
	} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	var id string
	if err := conn.QueryRowContext(t.Context(), "SELECT id FROM tmp2").Scan(&id); err != nil || id != "1" {
		t.Errorf("temporary table: %s %v", id, err)
	}
}

// 本番の int の表に帯の上の AUTO_INCREMENT（ブランチの番号を含むダンプ）を指定しても、カウンタは止まらない。本番が
// 帯を使い切ったら、表の名前の付いたエラーになる。型を int と bigint の間で変えると、新しい型の帯で続く。
func TestIntAutoIncrementAlter(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE t_int (id int(11) unsigned NOT NULL AUTO_INCREMENT, v varchar(20), PRIMARY KEY (id))")
	mustExec(t, mainDB, "CREATE TABLE t_mod (id int(11) unsigned NOT NULL AUTO_INCREMENT, v varchar(20), PRIMARY KEY (id))")
	mustExec(t, mainDB, "INSERT INTO t_mod (v) VALUES ('m1')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	insert := func(db *sql.DB, table, v string) string {
		t.Helper()
		mustExec(t, db, "INSERT INTO "+table+" (v) VALUES ('"+v+"')")
		return one(t, db, "SELECT id FROM "+table+" WHERE v = '"+v+"'")
	}

	mustExec(t, mainDB, "ALTER TABLE t_int AUTO_INCREMENT = 1000000005")
	if got := insert(mainDB, "t_int", "a"); got != "1" {
		t.Errorf("after AUTO_INCREMENT above the band: %s, want 1", got)
	}
	mustExec(t, mainDB, "ALTER TABLE t_int AUTO_INCREMENT = 999999999")
	if got := insert(mainDB, "t_int", "b"); got != "999999999" {
		t.Errorf("last value of the band: %s", got)
	}
	_, err := mainDB.Exec("INSERT INTO t_int (v) VALUES ('c')")
	if err == nil || !strings.Contains(err.Error(), "AUTO_INCREMENT range is exhausted: table t_int, values below 1000000000") {
		t.Errorf("past the band: %v", err)
	}

	// int から bigint に変えると、ブランチは bigint の帯（10^10 から）、本番はそのまま続く。
	if got := insert(staging, "t_mod", "s1"); got != "1000000000" {
		t.Errorf("int on the branch: %s", got)
	}
	mustExec(t, staging, "ALTER TABLE t_mod MODIFY id bigint(20) unsigned NOT NULL AUTO_INCREMENT")
	if got := insert(staging, "t_mod", "s2"); got != "10000000000" {
		t.Errorf("bigint on the branch: %s", got)
	}
	mustExec(t, mainDB, "ALTER TABLE t_mod MODIFY id bigint(20) unsigned NOT NULL AUTO_INCREMENT")
	if got := insert(mainDB, "t_mod", "m2"); got != "2" {
		t.Errorf("bigint on main: %s", got)
	}
	// bigint から int に戻すと、本番の行だけの表なら本番は続き、ブランチは int の帯から振る。
	mustExec(t, mainDB, "CALL heimdb_branch_create('later')")
	later := openBranch(t, dsn, "later")
	defer later.Close()
	mustExec(t, later, "ALTER TABLE t_mod MODIFY id int(11) unsigned NOT NULL AUTO_INCREMENT")
	if got := insert(later, "t_mod", "l1"); got != "1010000000" {
		t.Errorf("int again on the branch: %s", got)
	}
	mustExec(t, mainDB, "ALTER TABLE t_mod MODIFY id int(11) unsigned NOT NULL AUTO_INCREMENT")
	if got := insert(mainDB, "t_mod", "m3"); got != "3" {
		t.Errorf("int again on main: %s", got)
	}
}

// 番号帯の枠を使い回すブランチは、前に同じ枠を使ったブランチのマージ済みの行の続きから振る。
func TestIntAutoIncrementBlockReuse(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	mustExec(t, mainDB, "CREATE TABLE t_int (id int(11) NOT NULL AUTO_INCREMENT, v varchar(20), PRIMARY KEY (id))")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')") // ID 2: 枠 0
	staging := openBranch(t, dsn, "staging")
	mustExec(t, staging, "INSERT INTO t_int (v) VALUES ('a'), ('b')")
	staging.Close()
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")
	for i := 3; i <= 101; i++ {
		mustExec(t, mainDB, fmt.Sprintf("CALL heimdb_branch_create('b%d')", i))
	}
	mustExec(t, mainDB, "CALL heimdb_branch_create('again')") // ID 102: 枠 0
	again := openBranch(t, dsn, "again")
	defer again.Close()
	mustExec(t, again, "INSERT INTO t_int (v) VALUES ('c')")
	if got := one(t, again, "SELECT id FROM t_int WHERE v = 'c'"); got != "1000000002" {
		t.Errorf("reused block: %s, want 1000000002", got)
	}
	b101 := openBranch(t, dsn, "b101")
	defer b101.Close()
	mustExec(t, b101, "INSERT INTO t_int (v) VALUES ('d')")
	if got := one(t, b101, "SELECT id FROM t_int WHERE v = 'd'"); got != "1990000000" {
		t.Errorf("last block: %s, want 1990000000", got)
	}
}

// Redirection の形（int のグループと、それを group_id で指す項目）。両側が足しても競合にならず、マージの後に項目が
// ブランチで足したグループを指し、本番は 10^9 未満で続く（docs/plan.md の段階 61）。
func TestMergeRedirectionShape(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	for _, q := range []string{
		"CREATE TABLE wp_redirection_groups (id int(11) unsigned NOT NULL AUTO_INCREMENT, name varchar(50) NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE wp_redirection_items (id int(11) unsigned NOT NULL AUTO_INCREMENT, url mediumtext NOT NULL, group_id int(11) unsigned NOT NULL DEFAULT 0, PRIMARY KEY (id), KEY group_id (group_id))",
		"INSERT INTO wp_redirection_groups (name) VALUES ('Redirections'), ('Modified Posts')",
		"INSERT INTO wp_redirection_items (url, group_id) VALUES ('/old', 1)",
	} {
		mustExec(t, mainDB, q)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')") // ID 3: 枠 1
	mustExec(t, staging, "INSERT INTO wp_redirection_groups (name) VALUES ('Campaign')")
	mustExec(t, staging, "INSERT INTO wp_redirection_items (url, group_id) SELECT '/campaign', id FROM wp_redirection_groups WHERE name = 'Campaign'")
	mustExec(t, staging, "INSERT INTO wp_redirection_items (url, group_id) VALUES ('/staged', 1)")
	mustExec(t, mainDB, "INSERT INTO wp_redirection_groups (name) VALUES ('Prod')")
	mustExec(t, mainDB, "INSERT INTO wp_redirection_items (url, group_id) VALUES ('/prod', 3)")

	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
	got := diffRows(t, mainDB, "SELECT i.id, i.url, g.name FROM wp_redirection_items i JOIN wp_redirection_groups g ON g.id = i.group_id ORDER BY i.id")
	want := []string{"1|/old|Redirections", "2|/prod|Prod", "1010000000|/campaign|Campaign", "1010000001|/staged|Redirections"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("items after the merge:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	mustExec(t, mainDB, "INSERT INTO wp_redirection_groups (name) VALUES ('Later')")
	if got := one(t, mainDB, "SELECT id FROM wp_redirection_groups WHERE name = 'Later'"); got != "4" {
		t.Errorf("main after the merge: %s, want 4", got)
	}
}

// ブランチの側を採れない競合（同じ ID の別の行）は、ワイルドカードの 'branch' で黙って残らず、差分とマージのエラーに
// 理由が出る（docs/plan.md の段階 61）。
func TestForbiddenConflictExplained(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE wp_small (id mediumint(9) NOT NULL AUTO_INCREMENT, v varchar(20), PRIMARY KEY (id))")
	mustExec(t, mainDB, "CALL heimdb_branch_delete('staging')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	mustExec(t, staging, "INSERT INTO wp_small (v) VALUES ('branch')")
	mustExec(t, mainDB, "INSERT INTO wp_small (v) VALUES ('parent')")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', '*', '*', 'branch')")
	const note = "only 'parent' can resolve this: different rows have the same auto-increment ID"
	rows := diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_small')")
	if len(rows) != 1 || !strings.HasSuffix(rows[0], "|conflict||v|added on both sides with different values; "+note) {
		t.Errorf("diff: %v", rows)
	}
	_, err := mainDB.Exec("CALL heimdb_branch_merge('staging')")
	if err == nil || !strings.Contains(err.Error(), "wp_small id=1 ("+note+")") {
		t.Errorf("merge: %v", err)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_small', '*', 'parent')")
	rows = diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', 'wp_small')")
	if len(rows) != 1 || strings.Contains(rows[0], note) {
		t.Errorf("diff after resolving: %v", rows)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
}

// バージョンを上げる前に作ったブランチは、保存したカウンタ（帯の下限より小さい）を使わず、自分の帯から振る。main の保存した
// カウンタはそのまま使う。
func TestSavedCounterBelowBand(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	zero := func() (uint64, error) { return 1, nil }
	for _, b := range []uint64{1, 2} {
		c, err := s.AutoIncrement(b, 7, 1, ^uint64(0), zero)
		if err != nil {
			t.Fatal(err)
		}
		for range 5 {
			if _, err := c.Next(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.FlushAutoIncrements(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for b, want := range map[uint64]uint64{1: 6, 2: 1_000_000_000} {
		lo, hi := autoIncrementBand(b, sqltypes.Int32)
		c, err := s.AutoIncrement(b, 7, lo, hi, zero)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Peek(); got != want {
			t.Errorf("branch %d: next = %d, want %d", b, got, want)
		}
	}
}
