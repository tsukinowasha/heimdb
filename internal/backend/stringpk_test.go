package backend

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// rowsOf は、1 列の結果を「,」でつなぐ。
func rowsOf(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return strings.Join(out, ",")
}

// 1 列の文字列の主キー（docs/plan.md の段階 38）。Yoast SEO の wp_yoast_expiring_store の形。
func TestStringPrimaryKey(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	// Yoast が出す文そのまま（DEFAULT CHARSET=utf8 は段階 37 で utf8mb4_general_ci になる）。
	mustExec(t, m, "CREATE TABLE `wp_yoast_expiring_store` ( `key_name` varchar(255) NOT NULL, `value` text NOT NULL, `exp` datetime NOT NULL, PRIMARY KEY (`key_name`)) DEFAULT CHARSET=utf8;")
	mustExec(t, m, "INSERT INTO wp_yoast_expiring_store VALUES ('wpseo_a', 'one', '2026-10-12 00:00:00'), ('wpseo_b', 'two', '2026-10-13 00:00:00')")
	if got := one(t, m, "SELECT value FROM wp_yoast_expiring_store WHERE key_name = 'WPSEO_A'"); got != "one" {
		t.Errorf("case-insensitive lookup: %q", got)
	}
	if _, err := m.Exec("INSERT INTO wp_yoast_expiring_store VALUES ('WPSEO_A', 'dup', '2026-10-12 00:00:00')"); err == nil || !strings.Contains(err.Error(), "1062") {
		t.Errorf("case-only duplicate: %v", err)
	}
	mustExec(t, m, "INSERT INTO wp_yoast_expiring_store VALUES ('WPSEO_A', 'upserted', '2026-10-14 00:00:00') ON DUPLICATE KEY UPDATE value = VALUES(value)")
	mustExec(t, m, "DELETE FROM wp_yoast_expiring_store WHERE exp < '2026-10-13 00:00:00' AND key_name = 'wpseo_b'")
	if got := rowsOf(t, m, "SELECT CONCAT(key_name, '=', value) FROM wp_yoast_expiring_store ORDER BY key_name"); got != "wpseo_a=upserted,wpseo_b=two" {
		t.Errorf("expiring store: %s", got)
	}

	// 既定の照合順序（utf8mb4_0900_ai_ci）の表。二次インデックスと一意インデックスを持つ。
	mustExec(t, m, "CREATE TABLE s (k varchar(64) NOT NULL PRIMARY KEY, v int NOT NULL, u varchar(20), KEY v (v), UNIQUE KEY u (u), KEY vk (v, k))")
	mustExec(t, m, "INSERT INTO s VALUES ('', 0, NULL), ('ab', 1, 'x1'), ('abc', 2, 'x2'), ('abd', 2, NULL), ('b', 3, 'x3'), ('bc', 3, NULL), ('あ', 4, NULL), ('日本', 4, 'x4')")
	for q, want := range map[string]string{
		"SELECT k FROM s WHERE k = 'AB'":                                       "ab",
		"SELECT k FROM s WHERE k = ''":                                         "",
		"SELECT k FROM s WHERE k IN ('abd', 'B', 'zz') ORDER BY k":             "abd,b",
		"SELECT k FROM s WHERE k > 'abc' AND k < 'bz' ORDER BY k":              "abd,b,bc",
		"SELECT k FROM s WHERE k BETWEEN 'ab' AND 'b' ORDER BY k":              "ab,abc,abd,b",
		"SELECT k FROM s WHERE k LIKE 'ab%' ORDER BY k":                        "ab,abc,abd",
		"SELECT k FROM s ORDER BY k DESC LIMIT 3":                              "日本,あ,bc",
		"SELECT k FROM s ORDER BY k LIMIT 3":                                   ",ab,abc",
		"SELECT k FROM s WHERE v = 2 ORDER BY k":                               "abc,abd",
		"SELECT k FROM s WHERE v = 3 ORDER BY k DESC":                          "bc,b",
		"SELECT k FROM s WHERE v = 4 AND k > 'z' ORDER BY k":                   "あ,日本",
		"SELECT k FROM s WHERE u = 'x2'":                                       "abc",
		"SELECT COUNT(*) FROM s":                                               "8",
		"SELECT b.k FROM s a JOIN s b ON b.k = a.u WHERE a.v = 0":              "",
		"SELECT a.k FROM s a JOIN s b ON a.k = b.k WHERE b.v = 3 ORDER BY a.k": "b,bc",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	// 一意インデックスの重複の検査は、エントリから主キーを取り出す（文字列の主キーは前から読む）。
	if _, err := m.Exec("INSERT INTO s VALUES ('c', 5, 'X1')"); err == nil || !strings.Contains(err.Error(), "1062") {
		t.Errorf("unique index duplicate: %v", err)
	}
	mustExec(t, m, "UPDATE s SET u = 'x1' WHERE k = 'ab'") // 自分と同じ値への更新は重複ではない

	t0 := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	time.Sleep(10 * time.Millisecond)
	// 大小だけを変える更新（主キーのバイト列は同じ）、主キーを変える更新、削除。
	mustExec(t, m, "UPDATE s SET k = 'AB' WHERE k = 'ab'")
	mustExec(t, m, "UPDATE s SET k = 'zz' WHERE k = 'abd'")
	mustExec(t, m, "DELETE FROM s WHERE k = 'bc'")
	if got := rowsOf(t, m, "SELECT k FROM s WHERE v IN (1, 2, 3) ORDER BY k"); got != "AB,abc,b,zz" {
		t.Errorf("after updates: %s", got)
	}
	if got := rowsOf(t, m, "SELECT k FROM s AS OF '"+t0+"' WHERE v IN (1, 2, 3) ORDER BY k"); got != "ab,abc,abd,b,bc" {
		t.Errorf("AS OF: %s", got)
	}
	mustExec(t, m, "ANALYZE TABLE s")

	// ブランチの変更は差分に出て、マージで本番に入る。
	mustExec(t, m, "CALL heimdb_branch_create('st')")
	b := openBranch(t, dsn, "st")
	defer b.Close()
	mustExec(t, b, "INSERT INTO s VALUES ('branch', 9, NULL)")
	mustExec(t, b, "UPDATE s SET v = 8 WHERE k = 'abc'")
	mustExec(t, b, "INSERT INTO wp_yoast_expiring_store VALUES ('wpseo_c', 'from branch', '2026-10-15 00:00:00')")
	diff, err := m.Query("CALL heimdb_branch_diff('st', 's')")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for diff.Next() {
		var table, id, bc, pc, action string
		var res, cols, detail, by, at, values sql.NullString
		if err := diff.Scan(&table, &id, &bc, &pc, &action, &res, &cols, &detail, &by, &at, &values); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id+":"+action)
	}
	diff.Close()
	if got := strings.Join(ids, ","); got != "k=abc:update,k=branch:insert" {
		t.Errorf("diff: %s", got)
	}
	mustExec(t, m, "CALL heimdb_branch_merge('st')")
	if got := rowsOf(t, m, "SELECT CONCAT(k, '=', v) FROM s WHERE k IN ('abc', 'branch') ORDER BY k"); got != "abc=8,branch=9" {
		t.Errorf("after merge: %s", got)
	}
	if got := one(t, m, "SELECT value FROM wp_yoast_expiring_store WHERE key_name = 'WPSEO_C'"); got != "from branch" {
		t.Errorf("merged expiring store: %q", got)
	}

	// 変更の一覧と復元。
	lst, err := m.Query("CALL heimdb_changes('main', '" + t0 + "', 's')")
	if err != nil {
		t.Fatal(err)
	}
	var changed []string
	for lst.Next() {
		var table, id, change string
		var cols, at, detail sql.NullString
		if err := lst.Scan(&table, &id, &change, &cols, &at, &detail); err != nil {
			t.Fatal(err)
		}
		changed = append(changed, id+":"+change)
	}
	lst.Close()
	if got := strings.Join(changed, ","); !strings.Contains(got, "k=AB:update") || !strings.Contains(got, "k=abd:delete") || !strings.Contains(got, "k=zz:insert") || !strings.Contains(got, "k=bc:delete") || !strings.Contains(got, "k=branch:insert") {
		t.Errorf("changes: %s", got)
	}
	mustExec(t, m, "CALL heimdb_restore('main', '"+t0+"', 's')")
	if got := rowsOf(t, m, "SELECT CONCAT(k, '=', v) FROM s ORDER BY k"); got != "=0,ab=1,abc=2,abd=2,b=3,bc=3,あ=4,日本=4" {
		t.Errorf("after restore: %s", got)
	}
	if got := rowsOf(t, m, "SELECT k FROM s WHERE v = 3 ORDER BY k"); got != "b,bc" {
		t.Errorf("index after restore: %s", got)
	}

	// 前方の長さを付けた文字列の主キーは拒否する（複数列の文字列の主キーは TestMixedPrimaryKey）。
	for _, q := range []string{
		"CREATE TABLE p1 (k varchar(64) NOT NULL, PRIMARY KEY (k(10)))",
	} {
		if _, err := m.Exec(q); err == nil {
			t.Errorf("%s: accepted", q)
		}
	}
}
