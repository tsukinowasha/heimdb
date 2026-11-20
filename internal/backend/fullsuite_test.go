package backend

import (
	"database/sql"
	"testing"
)

// PHPUnit の全テストで見つかった差（docs/plan.md の段階 20）。期待値は本物の MySQL 8.0 で確かめた。

func column(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	return joinCells(queryCells(t, db, q, ""), "", ",")
}

// SQL_CALC_FOUND_ROWS と LIMIT の offset: FOUND_ROWS() は offset に関係なく全件の数（以前は offset の分が引かれ、
// WordPress の 2 ページ目以降で総数とページの数が合わなかった）。返る行は変わらない。
func TestFoundRowsWithOffset(t *testing.T) {
	db, _, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1) // FOUND_ROWS() は接続ごと
	mustExec(t, db, "CREATE TABLE fr (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, t varchar(20) NOT NULL, d datetime NOT NULL, KEY td (t, d, id))")
	mustExec(t, db, "INSERT INTO fr (t, d) VALUES ('p','2020-01-01'),('p','2020-01-02'),('p','2020-01-03'),('p','2020-01-04'),('p','2020-01-05'),('q','2020-01-06')")
	for _, c := range []struct{ q, rows, found string }{
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = 'p' ORDER BY d DESC LIMIT 0, 2", "5,4", "5"},
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = 'p' ORDER BY d DESC LIMIT 2, 2", "3,2", "5"},
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = 'p' ORDER BY d DESC LIMIT 4, 2", "1", "5"},
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = 'p' ORDER BY d DESC LIMIT 10, 2", "", "5"},
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr ORDER BY id LIMIT 3, 2", "4,5", "6"},
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = 'p' ORDER BY id LIMIT 3, 2", "4,5", "5"},
		{"SELECT SQL_CALC_FOUND_ROWS id FROM fr ORDER BY id LIMIT 4, 18446744073709551615", "5,6", "6"}, // offset から最後まで
	} {
		if got := column(t, db, c.q); got != c.rows {
			t.Errorf("%s: rows = %q, want %q", c.q, got, c.rows)
		}
		if got := one(t, db, "SELECT FOUND_ROWS()"); got != c.found {
			t.Errorf("%s: FOUND_ROWS() = %s, want %s", c.q, got, c.found)
		}
	}
	// プリペアドステートメントの引数の LIMIT と offset。
	rows, err := db.Query("SELECT SQL_CALC_FOUND_ROWS id FROM fr WHERE t = ? ORDER BY d DESC LIMIT ?, ?", "p", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if got := one(t, db, "SELECT FOUND_ROWS()"); n != 2 || got != "5" {
		t.Errorf("prepared: %d rows, FOUND_ROWS() = %s, want 2 rows and 5", n, got)
	}
}

// インデックスの先頭の列に条件がなく、後ろの列に IN があると、範囲ごとにインデックス全体を走査して同じ行を
// 範囲の数だけ返していた（コメントの post_status IN ('publish', 'future')）。
func TestIndexRangesDoNotDuplicateRows(t *testing.T) {
	db, _, stop := branchServer(t)
	defer stop()
	mustExec(t, db, "CREATE TABLE dp (ID bigint unsigned NOT NULL AUTO_INCREMENT, post_status varchar(20) NOT NULL DEFAULT 'publish', post_type varchar(20) NOT NULL DEFAULT 'post', post_date datetime NOT NULL DEFAULT '2000-01-01 00:00:00', PRIMARY KEY (ID), KEY type_status_date (post_type, post_status, post_date, ID))")
	mustExec(t, db, "CREATE TABLE dc (comment_ID bigint unsigned NOT NULL AUTO_INCREMENT, comment_post_ID bigint unsigned NOT NULL DEFAULT 0, comment_approved varchar(20) NOT NULL DEFAULT '1', comment_type varchar(20) NOT NULL DEFAULT 'comment', comment_date_gmt datetime NOT NULL DEFAULT '2000-01-01 00:00:00', PRIMARY KEY (comment_ID), KEY comment_post_ID (comment_post_ID), KEY comment_approved_date_gmt (comment_approved, comment_date_gmt))")
	mustExec(t, db, "INSERT INTO dp (post_status) VALUES ('publish'), ('draft'), ('future')")
	mustExec(t, db, "INSERT INTO dc (comment_post_ID) VALUES (1), (2), (3)")
	for q, want := range map[string]string{
		"SELECT dc.comment_ID FROM dc JOIN dp ON dp.ID = dc.comment_post_ID WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') AND dp.post_status IN ('publish','future') ORDER BY dc.comment_ID": "1,3",
		"SELECT ID FROM dp WHERE post_status IN ('publish', 'future') ORDER BY ID":                                  "1,3",
		"SELECT ID FROM dp WHERE post_type IN ('post', 'page') AND post_status IN ('publish', 'draft') ORDER BY ID": "1,2",
		"SELECT ID FROM dp WHERE ID IN (1, 3, 1) ORDER BY ID":                                                       "1,3",
	} {
		if got := column(t, db, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
}

// 大文字と小文字を区別しない照合順序で、大文字と小文字だけを変える UPDATE も書き込む（以前は等しい行とみなして
// 書き込まず、WordPress でメールアドレスの大文字への変更が保存されなかった）。変わらない UPDATE は 0 行のまま。
func TestUpdateCaseOnlyChange(t *testing.T) {
	db, _, stop := branchServer(t)
	defer stop()
	db.SetMaxOpenConns(1)
	mustExec(t, db, "CREATE TABLE ci (id int NOT NULL PRIMARY KEY, e varchar(100) NOT NULL DEFAULT '', n int NOT NULL DEFAULT 0, KEY e (e)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	mustExec(t, db, "INSERT INTO ci VALUES (1, 'editor@example.com', 0)")
	for _, c := range []struct{ q, affected, value string }{
		{"UPDATE ci SET e = 'Editor@example.com' WHERE id = 1", "1", "Editor@example.com"},
		{"UPDATE ci SET e = 'Editor@example.com' WHERE id = 1", "0", "Editor@example.com"},
		{"UPDATE ci SET e = 'x@y', n = 1 WHERE e = 'EDITOR@EXAMPLE.COM'", "1", "x@y"},
	} {
		mustExec(t, db, c.q)
		if got := one(t, db, "SELECT ROW_COUNT()"); got != c.affected {
			t.Errorf("%s: ROW_COUNT() = %s, want %s", c.q, got, c.affected)
		}
		if got := one(t, db, "SELECT e FROM ci WHERE id = 1"); got != c.value {
			t.Errorf("%s: e = %q, want %q", c.q, got, c.value)
		}
	}
	// インデックスも新しい値になる。
	if got := one(t, db, "SELECT COUNT(*) FROM ci WHERE e = 'X@Y'"); got != "1" {
		t.Errorf("lookup by the index after the update: %s", got)
	}
}

// information_schema.TABLES の TABLE_ROWS は、テーブルの行数（以前はいつも 0 で、WordPress のサイトヘルスが
// オブジェクトキャッシュを勧めなかった）。ブランチごとの行数になる。
func TestInformationSchemaTableRows(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE tr (id int NOT NULL PRIMARY KEY)")
	mustExec(t, m, "CREATE TABLE empty_t (id int NOT NULL PRIMARY KEY)")
	mustExec(t, m, "INSERT INTO tr VALUES (1), (2), (3)")
	mustExec(t, m, "CALL heimdb_branch_create('staging')")
	mustExec(t, m, "INSERT INTO tr VALUES (4)")
	q := "SELECT CONCAT(TABLE_NAME, '=', TABLE_ROWS) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN ('tr', 'empty_t') ORDER BY TABLE_NAME"
	if got := column(t, m, q); got != "empty_t=0,tr=4" {
		t.Errorf("main: %s", got)
	}
	s := openBranch(t, dsn, "staging")
	defer s.Close()
	if got := column(t, s, q); got != "empty_t=0,tr=3" {
		t.Errorf("staging: %s", got)
	}
}
