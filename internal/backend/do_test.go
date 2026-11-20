package backend

import (
	"database/sql"
	"strings"
	"testing"
)

func TestRewriteDo(t *testing.T) {
	cases := []struct {
		in, want string
		do       bool
	}{
		{"DO 1", "SELECT 1", true},
		{"do 1, 2", "SELECT 1, 2", true},
		{"DO(1)", "SELECT(1)", true},
		{" /* c */ DO 1", " /* c */ SELECT 1", true},
		{"-- c\nDO 1", "-- c\nSELECT 1", true},
		{"DO", "SELECT", true}, // 式がなければ構文エラー（解析が返す）
		{"DO 1; SELECT 2", "SELECT 1; SELECT 2", true},
		{"SELECT 'DO 1'", "SELECT 'DO 1'", false},
		{"SELECT 1; DO 1", "SELECT 1; DO 1", false}, // 2 つ目の文は、次の構文解析で書き換える
		{"do_x()", "do_x()", false},
		{"DOUBLE", "DOUBLE", false},
		{"EXPLAIN DO 1", "EXPLAIN DO 1", false},
		{"D", "D", false},
	}
	for _, c := range cases {
		got, do := rewriteDo(c.in)
		if got != c.want || do != c.do {
			t.Errorf("rewriteDo(%q) = %q, %v, want %q, %v", c.in, got, do, c.want, c.do)
		}
	}
}

// DO は、式を評価して、行を返さずに OK を返す（docs/plan.md の段階 69。WordPress の wpdb::check_connection() が
// DO 1 を送る）。
func TestDoStatement(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id bigint NOT NULL PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"DO 1", "do 1, 2", "/* c */ DO (1)", "DO SLEEP(0)", "DO (SELECT COUNT(*) FROM t)", "DO 1/0"} {
		res, err := db.Exec(q)
		if err != nil {
			t.Errorf("%q: %v", q, err)
			continue
		}
		if n, err := res.RowsAffected(); err != nil || n != 0 {
			t.Errorf("%q: rows affected %d, %v", q, n, err)
		}
		// OK を返し、列を返さない（SELECT の結果にしない）
		rows, err := db.Query(q)
		if err != nil {
			t.Errorf("%q: %v", q, err)
			continue
		}
		if cols, err := rows.Columns(); err != nil || len(cols) != 0 {
			t.Errorf("%q: columns %v, %v", q, cols, err)
		}
		rows.Close()
	}

	// 式のエラー（実行して初めてわかる、2 行を返す副問い合わせを含む）はそのまま返す。SELECT にしかない形と、先頭が DO でない文は構文エラー（MySQL と同じ）。
	for _, q := range []string{"DO (SELECT COUNT(*) FROM nosuch)", "DO (SELECT 1 UNION SELECT 2)", "DO *", "DO", "DO 1 UNION SELECT 2", "EXPLAIN DO 1"} {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("%q: no error", q)
		}
	}

	// 文字列の PREPARE と EXECUTE
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{"PREPARE s FROM 'DO 1'", "EXECUTE s"} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}

	// 複数の文を送れる接続: DO の後の文と、DO の前の文の後の DO
	multi, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	defer multi.Close()
	var two int
	if err := multi.QueryRow("DO 1; SELECT 2").Scan(&two); err != nil || two != 2 {
		t.Errorf("DO 1; SELECT 2: %d, %v", two, err)
	}
	if _, err := multi.Exec("SELECT 1; DO 1"); err != nil {
		t.Errorf("SELECT 1; DO 1: %v", err)
	}

	// DO を含む文字列は変えない
	var s string
	if err := db.QueryRow("SELECT 'DO 1'").Scan(&s); err != nil || s != "DO 1" {
		t.Errorf("SELECT 'DO 1': %q, %v", s, err)
	}
}

// SHOW INDEX（SHOW KEYS）の WHERE で絞り込む（docs/plan.md の段階 69。エンジンが WHERE を捨て、すべての索引を
// 返していた）。HeimDB が Sub_part を返す置き換え（showIndexesWithSubPart）は、WHERE の下でも効く。
func TestShowIndexWhere(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id int NOT NULL AUTO_INCREMENT PRIMARY KEY, application varchar(20) NOT NULL, isSystem int NOT NULL DEFAULT 0, meta_key varchar(255), KEY application (application), INDEX (isSystem), KEY meta_key (meta_key(191)))"); err != nil {
		t.Fatal(err)
	}
	// 各行の Key_name|Column_name|Sub_part
	keys := func(q string) string {
		t.Helper()
		var out []string
		for _, r := range queryCells(t, db, q, "NULL") {
			out = append(out, r[2]+"|"+r[4]+"|"+r[7])
		}
		return strings.Join(out, ",")
	}
	cases := []struct{ q, want string }{
		{"SHOW INDEX FROM t", "PRIMARY|id|NULL,application|application|NULL,isSystem|isSystem|NULL,meta_key|meta_key|191"},
		{"SHOW INDEX FROM t WHERE Key_name = 'system'", ""}, // Smart Slider 3 は、ない索引を消そうとしていた
		{"SHOW INDEX FROM t WHERE key_name = 'PRIMARY'", "PRIMARY|id|NULL"},
		{"SHOW KEYS FROM t WHERE Column_name = 'isSystem'", "isSystem|isSystem|NULL"},
		{"SHOW INDEX FROM t WHERE Non_unique = 0", "PRIMARY|id|NULL"},
		{"SHOW INDEX FROM t WHERE Key_name = 'meta_key' AND Seq_in_index = 1", "meta_key|meta_key|191"},
		{"SHOW KEYS FROM t WHERE Key_name = 'PRIMARY' AND Column_name = 'id'", "PRIMARY|id|NULL"}, // WooCommerce
	}
	for _, c := range cases {
		if got := keys(c.q); got != c.want {
			t.Errorf("%q = %q, want %q", c.q, got, c.want)
		}
	}
	if _, err := db.Exec("SHOW INDEX FROM t WHERE foo = 1"); err == nil {
		t.Error("unknown column: no error")
	}
}
