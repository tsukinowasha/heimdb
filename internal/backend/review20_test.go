package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 段階 20 のレビュー。期待値のうち「MySQL で確認」とあるものは、本物の MySQL 8.0（127.0.0.1:3307）で確かめた。

func review20Rows(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	return joinCells(queryCells(t, db, q, ""), " ", ",")
}

// ---- 1. FOUND_ROWS() と offset ----

// SQL_CALC_FOUND_ROWS のいろいろな形で、組み替えが結果と FOUND_ROWS() を壊さない（期待値は MySQL で確認）。
func TestReview20FoundRowsVariants(t *testing.T) {
	db, _, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE fr (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, t varchar(20) NOT NULL, d datetime NOT NULL, KEY td (t, d, id))")
	mustExec(t, db, "INSERT INTO fr (t, d) VALUES ('p','2020-01-01'),('p','2020-01-02'),('p','2020-01-03'),('p','2020-01-04'),('p','2020-01-05'),('q','2020-01-06')")
	for _, c := range []struct{ q, rows, found string }{
		// 派生テーブルの中の LIMIT/offset は外の総数に関係ない。
		{"SELECT SQL_CALC_FOUND_ROWS * FROM (SELECT id FROM fr ORDER BY id LIMIT 1, 3) x LIMIT 1, 1", "3", "3"},
		// GROUP BY（WordPress の WP_Query は GROUP BY を付けることがある）。
		{"SELECT SQL_CALC_FOUND_ROWS t FROM fr GROUP BY t ORDER BY t LIMIT 1, 2", "q", "2"},
		// IN サブクエリ。
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE id IN (SELECT id FROM fr WHERE t='p') ORDER BY id LIMIT 1,2", "2,3", "5"},
		// DISTINCT。
		{"SELECT SQL_CALC_FOUND_ROWS DISTINCT t FROM fr LIMIT 1,1", "q", "2"},
		// ソートがインデックスにならず TopN になる形（go-mysql-server が TopN で数える）。
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr ORDER BY d DESC LIMIT 2,2", "4,3", "6"},
		// 固定された先頭列と ORDER BY をインデックスに置き換える HeimDB のルール（TopN → Limit）と offset。
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t='p' ORDER BY d DESC, id ASC LIMIT 1,10", "4,3,2,1", "5"},
		// offset なし。
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr ORDER BY id LIMIT 2", "1,2", "6"},
	} {
		if got := review20Rows(t, db, c.q); got != c.rows {
			t.Errorf("%s: rows = %q, want %q", c.q, got, c.rows)
		}
		if got := one(t, db, "SELECT FOUND_ROWS()"); got != c.found {
			t.Errorf("%s: FOUND_ROWS() = %s, want %s", c.q, got, c.found)
		}
	}
	// EXPLAIN は組み替えた式（limitPlusOffset）を文字列にする。
	// EXPLAIN は組み替えた式（limitPlusOffset）を文字列にする。組み替えのない形と同じ結果（エラーの有無）になること。
	explain := func(q string) error {
		rows, err := db.Query(q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	}
	errPlain := explain("EXPLAIN SELECT id FROM fr WHERE t = 'p' ORDER BY d DESC LIMIT 2, 2")
	errCalc := explain("EXPLAIN SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = 'p' ORDER BY d DESC LIMIT 2, 2")
	t.Logf("EXPLAIN: plain err = %v, SQL_CALC_FOUND_ROWS err = %v", errPlain, errCalc)
	if (errPlain == nil) != (errCalc == nil) {
		t.Errorf("EXPLAIN differs: plain %v, calc %v", errPlain, errCalc)
	}
	if got := one(t, db, "SELECT 1"); got != "1" {
		t.Errorf("connection after EXPLAIN: %q", got)
	}
	// LIMIT だけが引数のプリペアドステートメント。
	rows, err := db.Query("SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = ? ORDER BY id LIMIT ?", "p", 2)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if got := one(t, db, "SELECT FOUND_ROWS()"); n != 2 || got != "5" {
		t.Errorf("prepared LIMIT ?: %d rows, FOUND_ROWS() = %s, want 2 rows and 5", n, got)
	}
}

// 「offset から最後まで」の慣用句 LIMIT k, 18446744073709551615（MySQL のマニュアルの書き方）に SQL_CALC_FOUND_ROWS を付けると、
// 組み替えた Limit の値 n + k が int64 を超えて負になり、行が 1 つも返らない（FOUND_ROWS() も 0）。組み替えのない形は
// 行を返す。MySQL は 5,6 と 6（MySQL で確認）。limitPlusOffset.Eval で、和が溢れるときは math.MaxInt64 に丸めて直した。
func TestReview20FoundRowsOffsetToEnd(t *testing.T) {
	db, _, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE fr (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, t varchar(20) NOT NULL)")
	mustExec(t, db, "INSERT INTO fr (t) VALUES ('p'),('p'),('p'),('p'),('p'),('q')")
	if got := review20Rows(t, db, "SELECT id FROM fr ORDER BY id LIMIT 4, 18446744073709551615"); got != "5,6" {
		t.Errorf("without SQL_CALC_FOUND_ROWS: rows = %q, want 5,6", got)
	}
	for _, q := range []string{
		"SELECT SQL_CALC_FOUND_ROWS id FROM fr ORDER BY id LIMIT 4, 18446744073709551615",
		"SELECT SQL_CALC_FOUND_ROWS id FROM fr ORDER BY id LIMIT 4, 9223372036854775807",
	} {
		if got := review20Rows(t, db, q); got != "5,6" {
			t.Errorf("%s: rows = %q, want 5,6 (MySQL)", q, got)
		}
		if got := one(t, db, "SELECT FOUND_ROWS()"); got != "6" {
			t.Errorf("%s: FOUND_ROWS() = %s, want 6 (MySQL)", q, got)
		}
	}
}

// ---- 2. mergeIntervals ----

func TestReview20MergeIntervalsUnit(t *testing.T) {
	b := func(s string) []byte { return []byte(s) }
	cases := []struct {
		name string
		in   []interval
		want []interval
	}{
		{"disjoint", []interval{{b("c"), b("d")}, {b("a"), b("b")}}, []interval{{b("a"), b("b")}, {b("c"), b("d")}}},
		{"adjacent", []interval{{b("a"), b("b")}, {b("b"), b("c")}}, []interval{{b("a"), b("c")}}},
		{"overlap", []interval{{b("a"), b("c")}, {b("b"), b("d")}}, []interval{{b("a"), b("d")}}},
		{"contained", []interval{{b("a"), b("z")}, {b("b"), b("c")}}, []interval{{b("a"), b("z")}}},
		{"same", []interval{{b("a"), b("b")}, {b("a"), b("b")}, {b("a"), b("b")}}, []interval{{b("a"), b("b")}}},
		{"unbounded hi first", []interval{{b("a"), nil}, {b("b"), b("c")}}, []interval{{b("a"), nil}}},
		{"unbounded hi later", []interval{{b("a"), b("b")}, {b("b"), nil}, {b("c"), b("d")}}, []interval{{b("a"), nil}}},
		{"unbounded hi disjoint", []interval{{b("a"), b("b")}, {b("c"), nil}}, []interval{{b("a"), b("b")}, {b("c"), nil}}},
		{"whole key space", []interval{{nil, nil}, {b("a"), b("b")}}, []interval{{nil, nil}}},
		{"empty lo", []interval{{nil, b("b")}, {b("a"), b("c")}}, []interval{{nil, b("c")}}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		i := 0
		ranges := make(gsql.MySQLRangeCollection, len(c.in))
		got := mergeIntervals(ranges, func(gsql.MySQLRange) ([]byte, []byte) {
			iv := c.in[i]
			i++
			return iv.lo, iv.hi
		})
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %q, want %q", c.name, fmt.Sprint(got), fmt.Sprint(c.want))
		}
	}
}

// 範囲をまとめた後も、逆方向の走査、上限のない範囲、プレフィックス長のインデックス、AS OF で結果と並びが正しい。
func TestReview20IndexRangesMergedResults(t *testing.T) {
	db, dsn, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "SET time_zone = '+00:00'")
	mustExec(t, db, "CREATE TABLE ir (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, a varchar(10) NOT NULL, b varchar(10) NOT NULL, n varchar(20) NOT NULL, KEY ab (a, b), KEY np (n(3)))")
	mustExec(t, db, "INSERT INTO ir (a, b, n) VALUES ('x','1','abcd'),('x','2','abce'),('y','1','abcf'),('y','2','xyz'),('z','1','abzz'),('z','3','q')")
	before := mark(t)
	mustExec(t, db, "DELETE FROM ir WHERE id = 3")
	for q, want := range map[string]string{
		// 先頭列の条件なしの IN: どの範囲もインデックス全体になる。重複なし、並びは ORDER BY のとおり。
		"SELECT id FROM ir WHERE b IN ('1','2') ORDER BY id":                                "1,2,4,5",
		"SELECT id FROM ir WHERE b IN ('1','2') ORDER BY id DESC":                           "5,4,2,1",
		"SELECT id FROM ir WHERE a IN ('x','z') AND b IN ('1','3') ORDER BY a, b":           "1,5,6",
		"SELECT id FROM ir WHERE a IN ('x','z') AND b IN ('1','3') ORDER BY a DESC, b DESC": "6,5,1",
		// 上限のない範囲と点の範囲（主キー）。
		"SELECT id FROM ir WHERE id > 4 OR id IN (1, 5) ORDER BY id":      "1,5,6",
		"SELECT id FROM ir WHERE id > 4 OR id IN (1, 5) ORDER BY id DESC": "6,5,1",
		"SELECT id FROM ir WHERE id < 2 OR id >= 5 ORDER BY id DESC":      "6,5,1",
		// セカンダリインデックスで上限なし。
		"SELECT id FROM ir WHERE a >= 'y' OR a = 'x' ORDER BY id": "1,2,4,5,6",
		// プレフィックス長（3 文字）: 'abcd' と 'abce' は同じキー、'abzz' は別。
		"SELECT id FROM ir WHERE n IN ('abcd','abce','abzz') ORDER BY id": "1,2,5",
		"SELECT id FROM ir WHERE n IN ('abcd','abce') ORDER BY id DESC":   "2,1",
		"SELECT id FROM ir WHERE n = 'abcf'":                              "",
		// AS OF（削除した行が見える時点）。
		"SELECT id FROM ir AS OF '" + before + "' WHERE b IN ('1','2') ORDER BY id":       "1,2,3,4,5",
		"SELECT id FROM ir AS OF '" + before + "' WHERE n IN ('abcf','abcd') ORDER BY id": "1,3",
		// IN に同じ値。
		"SELECT COUNT(*) FROM ir WHERE a IN ('x','x','x')": "2",
	} {
		if got := review20Rows(t, db, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	// ブランチでも同じ。
	mustExec(t, db, "CALL heimdb_branch_create('st')")
	s := openBranch(t, dsn, "st")
	defer s.Close()
	mustExec(t, s, "DELETE FROM ir WHERE id = 1")
	if got := review20Rows(t, s, "SELECT id FROM ir WHERE b IN ('1','2') ORDER BY id"); got != "2,4,5" {
		t.Errorf("branch: %q", got)
	}
	if got := review20Rows(t, db, "SELECT id FROM ir WHERE b IN ('1','2') ORDER BY id"); got != "1,2,4,5" {
		t.Errorf("main after branch delete: %q", got)
	}
}

// ---- 3. 大文字と小文字だけを変える UPDATE ----

func TestReview20UpdateCaseVariants(t *testing.T) {
	db, dsn, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE ci (id int NOT NULL PRIMARY KEY, e varchar(100) NOT NULL DEFAULT '', c longtext, n int NOT NULL DEFAULT 0, UNIQUE KEY e (e)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	mustExec(t, db, "INSERT INTO ci VALUES (1, 'a@x', 'hello', 0), (2, 'b@x', 'world', 0)")
	mustExec(t, db, "CREATE TABLE j0 (id int NOT NULL PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO j0 VALUES (1), (2)")
	check := func(q, affected, want string) {
		t.Helper()
		mustExec(t, db, q)
		if got := one(t, db, "SELECT ROW_COUNT()"); got != affected {
			t.Errorf("%s: ROW_COUNT() = %s, want %s", q, got, affected)
		}
		if got := review20Rows(t, db, "SELECT e, c FROM ci ORDER BY id"); got != want {
			t.Errorf("%s: rows = %q, want %q", q, got, want)
		}
	}
	// longtext（WordPress の post_content）の大文字と小文字だけの変更。
	check("UPDATE ci SET c = 'Hello' WHERE id = 1", "1", "a@x Hello,b@x world")
	// UPDATE IGNORE（MySQL で確認: 1 行）。
	check("UPDATE IGNORE ci SET e = 'A@x' WHERE id = 1", "1", "A@x Hello,b@x world")
	// 一意キーの列の大文字と小文字だけの変更は、自分自身と重複しない。
	check("UPDATE ci SET e = 'B@X' WHERE e = 'b@x'", "1", "A@x Hello,B@X world")
	// 全行を関数で（2 行とも変わる）。
	check("UPDATE ci SET e = LOWER(e)", "2", "a@x Hello,b@x world")
	// 変わらなければ 0 行のまま。
	check("UPDATE ci SET e = LOWER(e), n = n", "0", "a@x Hello,b@x world")
	// ORDER BY と LIMIT。
	check("UPDATE ci SET c = UPPER(c) ORDER BY id DESC LIMIT 1", "1", "a@x Hello,b@x WORLD")
	// SET のサブクエリ。
	check("UPDATE ci SET e = (SELECT UPPER('a@x')) WHERE id = 1", "1", "A@X Hello,b@x WORLD")
	// 複数の条件（主キーの検索とインデックスの検索の両方）。
	check("UPDATE ci SET e = 'a@x' WHERE id = 1 AND e = 'A@X'", "1", "a@x Hello,b@x WORLD")
	// 別名、データベース名つき、IN サブクエリの条件。
	check("UPDATE ci AS c SET c.e = 'A@x' WHERE c.id = 1", "1", "A@x Hello,b@x WORLD")
	check("UPDATE wordpress.ci SET e = 'a@X' WHERE id = 1", "1", "a@X Hello,b@x WORLD")
	// 相関のあるスカラーサブクエリの条件（Filter のまま。包みが届く）。
	check("UPDATE ci SET e = 'a@x' WHERE id = 1 AND 1 = (SELECT COUNT(*) FROM j0 WHERE j0.id = ci.id)", "1", "a@x Hello,b@x WORLD")
	// INSERT ... ON DUPLICATE KEY UPDATE（WordPress の add_option の形）でも大文字と小文字だけの変更が書き込まれる
	// （go-mysql-server は等しさを比べずに Update を呼ぶ。ROW_COUNT は MySQL では 2 だが、以前からの差なので見ない）。
	mustExec(t, db, "INSERT INTO ci (id, e, c, n) VALUES (1, 'A@X', 'Hello', 0) ON DUPLICATE KEY UPDATE e = VALUES(e), c = VALUES(c)")
	if got := review20Rows(t, db, "SELECT e, c FROM ci ORDER BY id"); got != "A@X Hello,b@x WORLD" {
		t.Errorf("ON DUPLICATE KEY UPDATE: %q", got)
	}
	check("UPDATE ci SET e = 'a@x' WHERE id = 1", "1", "a@x Hello,b@x WORLD")

	// プリペアドステートメント。
	if res, err := db.Exec("UPDATE ci SET e = ? WHERE id = ?", "A@x", 1); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("prepared: affected %d, want 1", n)
	}
	if got := one(t, db, "SELECT e FROM ci WHERE id = 1"); got != "A@x" {
		t.Errorf("prepared: %q", got)
	}

	// トランザクションの中で変えて ROLLBACK すると元に戻る。
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE ci SET e = 'a@X' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := tx.QueryRow("SELECT e FROM ci WHERE id = 1").Scan(&v); err != nil || v != "a@X" {
		t.Errorf("in tx: %q %v", v, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := one(t, db, "SELECT e FROM ci WHERE id = 1"); got != "A@x" {
		t.Errorf("after rollback: %q", got)
	}

	// 文字列の主キーは HeimDB が非対応（整数の主キーだけ）なので、主キーの列の大文字と小文字の変更は確かめない。

	// 一時テーブル。
	mustExec(t, db, "CREATE TEMPORARY TABLE tci (id int NOT NULL PRIMARY KEY, e varchar(20) NOT NULL) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	mustExec(t, db, "INSERT INTO tci VALUES (1, 'abc')")
	mustExec(t, db, "UPDATE tci SET e = 'Abc' WHERE id = 1")
	if got := one(t, db, "SELECT CONCAT(e, '/', ROW_COUNT()) FROM tci"); got != "Abc/1" {
		t.Errorf("temporary: %q", got)
	}

	// ブランチの中の変更は main に出ない。
	mustExec(t, db, "CALL heimdb_branch_create('st')")
	s := openBranch(t, dsn, "st")
	defer s.Close()
	mustExec(t, s, "UPDATE ci SET e = 'B@X' WHERE id = 2")
	if got := one(t, s, "SELECT e FROM ci WHERE id = 2"); got != "B@X" {
		t.Errorf("branch: %q", got)
	}
	if got := one(t, db, "SELECT e FROM ci WHERE id = 2"); got != "b@x" {
		t.Errorf("main after branch update: %q", got)
	}

	// 非対応のものは明示的なエラー（切断や panic にならない）。
	for _, q := range []string{
		"CREATE TRIGGER tr BEFORE UPDATE ON ci FOR EACH ROW SET NEW.n = 1",
		"UPDATE ci AS OF '2020-01-01' SET e = 'x' WHERE id = 1",
		// EXISTS の条件の UPDATE は go-mysql-server が "attempted to evaluate uninitialized subquery" を返す
		// （包みを外しても同じ。段階 20 以前からの差で、WordPress は使わない）。
		"UPDATE ci SET e = 'x' WHERE EXISTS (SELECT 1 FROM j0 WHERE j0.id = ci.id)",
	} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%s: no error", q)
		}
	}
	if got := one(t, db, "SELECT 1"); got != "1" {
		t.Errorf("connection after errors: %q", got)
	}
}

// ---- 4. information_schema.TABLES の TABLE_ROWS ----

func TestReview20TableRowsVariants(t *testing.T) {
	db, _, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE tr (id int NOT NULL PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO tr VALUES (1), (2), (3)")
	q := "SELECT TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tr'"
	if got := one(t, db, q); got != "3" {
		t.Errorf("plain: %s", got)
	}
	// 削除した行は数えない。
	mustExec(t, db, "DELETE FROM tr WHERE id = 3")
	if got := one(t, db, q); got != "2" {
		t.Errorf("after delete: %s", got)
	}
	// トランザクションの中では、自分の未コミットの行を数える（MySQL の値は推定なので、エラーにならなければよい）。
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO tr VALUES (10)"); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := tx.QueryRow(q).Scan(&v); err != nil {
		t.Errorf("in tx: %v", err)
	}
	tx.Rollback()
	// 一時テーブルは MySQL では information_schema.TABLES に出ない。HeimDB でもエラーにならないこと。
	mustExec(t, db, "CREATE TEMPORARY TABLE tmpx (id int NOT NULL PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO tmpx VALUES (1)")
	if got := review20Rows(t, db, "SELECT TABLE_SCHEMA, TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_NAME = 'tmpx'"); got != "" {
		t.Logf("temporary table is listed in information_schema.TABLES (MySQL does not list it): %q", got)
	}
	// 他の列（SHOW TABLE STATUS の Rows、派生テーブル経由）。MySQL はどちらも行数を返す。
	if got := review20Rows(t, db, "SELECT * FROM (SELECT TABLE_NAME, TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE()) x WHERE TABLE_NAME = 'tr'"); got != "tr 2" {
		t.Errorf("derived table: %q, want \"tr 2\"", got)
	}
	rows, err := db.Query("SHOW TABLE STATUS LIKE 'tr'")
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	for i, c := range cols {
		if c == "Rows" && vals[i].String != "2" {
			t.Logf("SHOW TABLE STATUS Rows = %q (MySQL: 2); recorded only", vals[i].String)
		}
	}
	// 接続は生きている。
	if got := one(t, db, "SELECT 1"); got != "1" {
		t.Errorf("connection: %q", got)
	}
}

// 権限を絞ったユーザー: MySQL は権限のあるテーブルだけを information_schema.TABLES に出す（MySQL で確認）。
// go-mysql-server はもともと全テーブルを出すので、HeimDB の行数の追加で何が見えるかを確かめる。
func TestReview20TableRowsRestrictedUser(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startAuthServer(t, dir, "rootpw")
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...),
		"INSERT INTO wp_options (option_name, option_value) VALUES ('a', '1'), ('b', '2')",
		"INSERT INTO wp_posts (post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES ('p', 'c', '', '', '', '')",
		"CREATE USER 'ro'@'%' IDENTIFIED BY 'ro'",
		"GRANT SELECT ON wordpress.wp_options TO 'ro'@'%'",
	) {
		mustExec(t, root, q)
	}
	root.Close()
	ro, err := connectAs(t, addr, "ro", "ro", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got := review20Rows(t, ro, "SELECT TABLE_NAME, TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'wordpress' AND TABLE_NAME IN ('wp_options', 'wp_posts') ORDER BY TABLE_NAME")
	t.Logf("restricted user sees: %q (MySQL: \"wp_options 2\")", got)
	if !strings.Contains(got, "wp_options 2") {
		t.Errorf("restricted user should see its own table's row count: %q", got)
	}
	if strings.Contains(got, "wp_posts 1") {
		t.Errorf("restricted user sees the row count of a table it cannot read: %q", got)
	}
}
