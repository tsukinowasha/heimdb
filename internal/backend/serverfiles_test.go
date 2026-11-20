package backend

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// サーバのファイルを読み書きする文は、どこに書いても、どの経路（テキスト、PREPARE、バイナリプロトコルのプリペア）でも
// 明示的なエラーになり、ファイルを読めず、作れない（docs/plan.md の段階 31）。go-mysql-server はそのままでは
// FILE 権限を確かめず、secure_file_priv が空ならどのファイルでも読み書きする。
func TestServerFilesRejected(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	files := t.TempDir()
	secret := filepath.Join(files, "secret.txt")
	if err := os.WriteFile(secret, []byte("1\tsecret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(files, "out.txt")
	// 行があると、ALTER TABLE ... ADD COLUMN がその場で既定値を計算する。
	if _, err := db.Exec("CREATE TABLE f (id INT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO f VALUES (1, 'x')"); err != nil {
		t.Fatal(err)
	}

	stmts := []string{
		"SELECT LOAD_FILE('" + secret + "')",
		"SELECT load_file('" + secret + "') AS x",
		"SELECT id FROM f WHERE v = (SELECT LOAD_FILE('" + secret + "'))",
		"SELECT * FROM (SELECT LOAD_FILE('" + secret + "') AS x) s",
		"INSERT INTO f VALUES (2, LOAD_FILE('" + secret + "'))",
		"UPDATE f SET v = LOAD_FILE('" + secret + "') WHERE id = 1",
		"SELECT 1 INTO OUTFILE '" + out + "'",
		"SELECT id, v FROM f INTO OUTFILE '" + out + "'",
		"SELECT 1 INTO DUMPFILE '" + out + "'",
		"(SELECT 1) UNION (SELECT 2) INTO DUMPFILE '" + out + "'",
		"LOAD DATA INFILE '" + secret + "' INTO TABLE f",
		"LOAD DATA LOCAL INFILE '" + secret + "' INTO TABLE f",
		"SELECT `LOAD_FILE`('" + secret + "')",
		"SELECT mysql.load_file('" + secret + "')",
		// vitess の ast.Walk が降りない木（DDL の列定義と既定値、EXPLAIN の中の文）
		"ALTER TABLE f ADD COLUMN w TEXT DEFAULT (LOAD_FILE('" + secret + "'))",
		"ALTER TABLE f ALTER COLUMN v SET DEFAULT (LOAD_FILE('" + secret + "'))",
		"ALTER TABLE f MODIFY COLUMN v TEXT DEFAULT (LOAD_FILE('" + secret + "'))",
		"CREATE TABLE g (id INT PRIMARY KEY, v TEXT DEFAULT (LOAD_FILE('" + secret + "')))",
		"EXPLAIN SELECT LOAD_FILE('" + secret + "')",
		"EXPLAIN ANALYZE SELECT LOAD_FILE('" + secret + "')",
		"DESCRIBE SELECT 1 INTO OUTFILE '" + out + "'",
	}
	check := func(how, q string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s %q: no error", how, q)
		} else if !strings.Contains(err.Error(), "HeimDB: reading or writing server files is not supported") {
			t.Errorf("%s %q: %v, want reading or writing server files", how, q, err)
		}
	}
	for _, q := range stmts {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.ExecContext(ctx, q)
		check("text", q, err)
		// PREPARE ... FROM（文字列と変数）
		_, err = conn.ExecContext(ctx, "PREPARE s FROM '"+strings.ReplaceAll(q, "'", "''")+"'")
		check("PREPARE", q, err)
		if _, err := conn.ExecContext(ctx, "SET @q = '"+strings.ReplaceAll(q, "'", "''")+"'"); err != nil {
			t.Fatal(err)
		}
		_, err = conn.ExecContext(ctx, "PREPARE s FROM @q")
		check("PREPARE FROM @q", q, err)
		// バイナリプロトコルのプリペア。LOAD DATA は、vitess が HeimDB の確認より前に、プレースホルダを数えるために文の木を
		// たどって panic し（IGNORE n LINES がないと nil の *SQLVal を読む）、接続が切れる。ファイルは読まれない
		// （docs/plan.md の段階 31 の「記録だけ」）。
		if !strings.HasPrefix(q, "LOAD DATA") {
			_, err = conn.PrepareContext(ctx, q)
			check("binary prepare", q, err)
		}
		// 同じ接続が生きている。
		var one int
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Errorf("%q: the connection was dropped: %v", q, err)
		}
		conn.Close()
	}

	// 複数の文を送れる接続（mysql クライアントなど）。エンジンの handler は、もとはこの接続で構文解析のエラーを見ずに
	// 文を実行していた（段階 42 でエンジンを直した）。実行のエラーの後は、vitess が残りの文を実行する（MySQL は止める。
	// docs/plan.md の段階 31 の「記録だけ」）ので、後ろは表を変えない文にする。構文解析のエラー（CONVERT と前方の長さ付きの
	// 主キー。HeimDB の構文解析器が返す）は、エンジンが見て止める。
	multi, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	defer multi.Close()
	for _, q := range append(stmts, "SELECT CONVERT('abc' USING ujis)", "CREATE TABLE pkp (k varchar(20) NOT NULL, PRIMARY KEY (k(5)))") {
		_, err := multi.Exec(q + "; SELECT 1")
		if err == nil || (!strings.Contains(err.Error(), "server files") && !strings.Contains(err.Error(), "CONVERT") && !strings.Contains(err.Error(), "prefix length")) {
			t.Errorf("multi-statement %q: %v", q, err)
		}
	}
	// 末尾がコメントや空白だけの文は、エラーにしない（MySQL と同じ。構文解析器は空の文のエラーを返すが、エンジンは
	// 何もしない文として扱う。docs/plan.md の段階 43。段階 42 でエラーになっていた）。
	for _, q := range []string{"SELECT 1; -- comment", "SELECT 1; /* c */", "SELECT 1;   ", "SELECT 1; # c"} {
		rows, err := multi.Query(q)
		if err != nil {
			t.Errorf("multi-statement %q: %v", q, err)
			continue
		}
		for rows.Next() {
		}
		for rows.NextResultSet() {
			for rows.Next() {
			}
		}
		if err := rows.Err(); err != nil {
			t.Errorf("multi-statement %q: %v", q, err)
		}
		rows.Close()
	}

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("%s was created (stat: %v)", out, err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM f").Scan(&n); err != nil || n != 1 {
		t.Errorf("rows in f: %d, %v; want 1", n, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'pkp'").Scan(&n); err != nil || n != 0 {
		t.Errorf("a table rejected by the parser was created through a multi-statement connection: %d %v", n, err)
	}
	var cols, tables int
	if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'f'").Scan(&cols); err != nil || cols != 2 {
		t.Errorf("columns of f: %d, %v; want 2", cols, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'g'").Scan(&tables); err != nil || tables != 0 {
		t.Errorf("table g: %d, %v; want none", tables, err)
	}
	var def sql.NullString
	if err := db.QueryRow("SELECT column_default FROM information_schema.columns WHERE table_name = 'f' AND column_name = 'v'").Scan(&def); err != nil || def.Valid {
		t.Errorf("default of f.v: %v, %v; want NULL", def, err)
	}

	// INTO @変数 は使える。
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT 42 INTO @v"); err != nil {
		t.Fatalf("SELECT INTO @v: %v", err)
	}
	var v int
	if err := conn.QueryRowContext(ctx, "SELECT @v").Scan(&v); err != nil || v != 42 {
		t.Errorf("@v = %d, %v; want 42", v, err)
	}
	// 列や文字列に load_file という名前があっても拒否しない。
	if _, err := conn.ExecContext(ctx, "SELECT 'LOAD_FILE(x)', 'INTO OUTFILE' AS load_file"); err != nil {
		t.Errorf("literal: %v", err)
	}
}
