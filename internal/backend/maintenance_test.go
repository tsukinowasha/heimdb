package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// maintRows は、q の結果を行ごとに | でつないで返す。
func maintRows(t *testing.T, db interface {
	Query(string, ...any) (*sql.Rows, error)
}, q string) ([]string, error) {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		s := make([]string, len(cols))
		for i, v := range vals {
			s[i] = v.String
		}
		out = append(out, strings.Join(s, "|"))
	}
	return out, rows.Err()
}

// OPTIMIZE、CHECK、REPAIR、ANALYZE TABLE は、何もせずに MySQL と同じ形の結果を返す（docs/plan.md の段階 74）。
// 結果は本物の MySQL 8.0 で確かめた（OPTIMIZE の InnoDB の note の行は返さない）。
func TestMaintenanceStatements(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE t (id INT PRIMARY KEY, v TEXT)", "CREATE TABLE `u;x` (id INT PRIMARY KEY)", "INSERT INTO t VALUES (1, 'a')",
		"CREATE TABLE `status` (id INT PRIMARY KEY)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ok := func(op, table string) string { return testDB + "." + table + "|" + op + "|status|OK" }
	repair := testDB + ".t|repair|note|The storage engine for the table doesn't support repair"
	for q, want := range map[string][]string{
		"OPTIMIZE TABLE t": {ok("optimize", "t")},
		"optimize no_write_to_binlog tables t, `u;x`":       {ok("optimize", "t"), ok("optimize", "u;x")},
		"OPTIMIZE LOCAL TABLE " + testDB + ".t":             {ok("optimize", "t")},
		"/* c */ CHECK TABLE `t`, `u;x` QUICK":              {ok("check", "t"), ok("check", "u;x")},
		"CHECK TABLE `t`,`u;x`;":                            {ok("check", "t"), ok("check", "u;x")},
		"check table t for upgrade":                         {ok("check", "t")},
		"CHECK TABLE t FAST MEDIUM EXTENDED CHANGED":        {ok("check", "t")},
		"REPAIR TABLE t":                                    {repair},
		"REPAIR NO_WRITE_TO_BINLOG TABLE t QUICK USE_FRM":   {repair},
		"ANALYZE TABLE t":                                   {ok("analyze", "t")},
		"CHECK TABLE status, " + testDB + ".status":         {ok("check", "status"), ok("check", "status")},
		"CHECK TABLE `" + testDB + "`.`t` /* x */ EXTENDED": {ok("check", "t")},
	} {
		got, err := maintRows(t, db, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", q, got, want)
		}
	}
	// ない表はエラー 1146（MySQL は Error と Operation failed の行）。CHECKSUM TABLE は対象外のまま
	for _, q := range []string{"OPTIMIZE TABLE nope", "CHECK TABLE t, nope", "CHECKSUM TABLE t", "CHECK t", "OPTIMIZE TABLE 'x'"} {
		if _, err := maintRows(t, db, q); err == nil {
			t.Errorf("%s: no error", q)
		}
	}
	// データは変わらず、接続はそのまま使える
	if got, err := maintRows(t, db, "SELECT v FROM t"); err != nil || len(got) != 1 || got[0] != "a" {
		t.Errorf("SELECT v FROM t: %v, %v", got, err)
	}
}

// 複数の文を送る接続でも、文ごとに種類が決まり、残りの文はそのまま実行する。
func TestMaintenanceMultiStatements(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn+"?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("OPTIMIZE TABLE t; INSERT INTO t VALUES (1); CHECK TABLE t QUICK; INSERT INTO t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if got, err := maintRows(t, db, "SELECT COUNT(*) FROM t"); err != nil || got[0] != "2" {
		t.Errorf("rows: %v, %v", got, err)
	}
	rows, err := db.Query("SELECT 1; REPAIR TABLE t; ANALYZE TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ops []string
	for {
		for rows.Next() {
			cols, _ := rows.Columns()
			if len(cols) == 4 {
				var tbl, op, mt, mx string
				if err := rows.Scan(&tbl, &op, &mt, &mx); err != nil {
					t.Fatal(err)
				}
				ops = append(ops, op+"|"+mt)
			}
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if strings.Join(ops, ",") != "repair|note,analyze|status" {
		t.Errorf("ops %v", ops)
	}
}

// サイト用のユーザーとブランチのユーザーは自分のデータベースの表に使え、Table の列はブランチのデータベース名になる。
// データベースに入れないユーザーは拒否される。何もコミットしない。
func TestMaintenancePrivilegesAndBranches(t *testing.T) {
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{auth: true, rootPassword: "rootpw"})
	defer stopServer(t, st, srv)()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.SetMaxOpenConns(1) // 一時テーブルは接続ごと
	for _, q := range []string{
		"CREATE TEMPORARY TABLE tmp (id INT PRIMARY KEY)", "CREATE TABLE wp_t (id INT PRIMARY KEY)", "INSERT INTO wp_t VALUES (1)", "CALL heimdb_branch_create('staging')",
		"CREATE USER 'wp'@'%' IDENTIFIED BY 'wppw'", "GRANT ALL ON wordpress.* TO 'wp'@'%'",
		"CREATE USER 'staging'@'%' IDENTIFIED BY 'stpw'", "GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%'",
	} {
		mustExec(t, root, q)
	}
	wp, err := connectAs(t, addr, "wp", "wppw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer wp.Close()
	stg, err := connectAs(t, addr, "staging", "stpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer stg.Close()
	rootStg, err := connectAs(t, addr, "root", "rootpw", testDB+"@staging")
	if err != nil {
		t.Fatal(err)
	}
	defer rootStg.Close()
	committed := st.LastCommitted()
	for _, c := range []struct {
		db   *sql.DB
		q    string
		want string
	}{
		{wp, "OPTIMIZE TABLE wp_t", "wordpress.wp_t|optimize|status|OK"},
		{wp, "CHECK TABLE `wp_t`", "wordpress.wp_t|check|status|OK"},
		{stg, "OPTIMIZE TABLE `wp_t`", "wordpress@staging.wp_t|optimize|status|OK"},
		{stg, "CHECK TABLE `wordpress@staging`.`wp_t` QUICK", "wordpress@staging.wp_t|check|status|OK"},
		{root, "CHECK TABLE tmp", "wordpress.tmp|check|status|OK"},
		{rootStg, "CHECK TABLE wordpress.wp_t, wp_t", "wordpress.wp_t|check|status|OK\nwordpress@staging.wp_t|check|status|OK"},
	} {
		got, err := maintRows(t, c.db, c.q)
		if err != nil || strings.Join(got, "\n") != c.want {
			t.Errorf("%s: %q, %v; want %q", c.q, got, err, c.want)
		}
	}
	// 本番のユーザーはブランチの表に、ブランチのユーザーは本番の表に使えない
	for _, c := range []struct {
		db *sql.DB
		q  string
	}{{wp, "CHECK TABLE `wordpress@staging`.wp_t"}, {stg, "OPTIMIZE TABLE wordpress.wp_t"}} {
		if _, err := maintRows(t, c.db, c.q); !dbAccessDenied(err) {
			t.Errorf("%s: %v, want access denied", c.q, err)
		}
	}
	if got := st.LastCommitted(); got != committed {
		t.Errorf("committed %d, want %d", got, committed)
	}
}

// 準備した文（バイナリプロトコル）の実行は構文解析を通らないので、前のコマンドの文の種類を使わない。
func TestMaintenancePreparedStatement(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	stmt, err := db.Prepare("ANALYZE TABLE t")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if _, err := maintRows(t, db, "OPTIMIZE TABLE t"); err != nil {
		t.Fatal(err)
	}
	var tbl, op, mt, mx string
	if err := stmt.QueryRow().Scan(&tbl, &op, &mt, &mx); err != nil || op != "analyze" {
		t.Errorf("prepared ANALYZE: %q, %v", op, err)
	}
}
