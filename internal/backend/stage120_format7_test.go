package backend

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	vmysql "github.com/dolthub/vitess/go/mysql"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// format7Queries は、バージョン 7 のデータ（internal/testdata/format7_data.tar.gz。a78d94e の HeimDB 0.2.40 で
// internal/testdata/format7_data.sql を流して作った）で流す文と、0.2.40 が返した結果（行を | でつなぎ、NULL は空）。
// 0.2.40 は FLOAT(10,7) などの (M,D) と UNSIGNED を落として float と double で保存した。
var format7Queries = []struct {
	q    string
	want []string
}{
	{"SELECT city, lat, lon FROM wfLocs ORDER BY lat", []string{"Osaka|34.6937|135.502", "Tokyo|35.6812|139.767"}},
	{"SELECT city FROM wfLocs WHERE lat > 35", []string{"Tokyo"}},
	{"SELECT id, d, e, du, p, q, n FROM fd ORDER BY id", []string{"1|1700000000.1234567|12.34567|5|1.5|0.1|1.50", "2|1700000001.5|-0.00004|6|0.1|2.5|-2.25", "3|0|99999.99999|7|-3.25|1e300|0.00", "4||||||"}},
	{"SELECT id FROM fd WHERE d > 1700000000 ORDER BY id", []string{"1", "2"}},
	{"SELECT id FROM fd WHERE e < 0 ORDER BY id", []string{"2"}},
	{"SELECT id FROM fd WHERE n = 1.5 ORDER BY id", []string{"1"}},
	{"SELECT id, s, b + 0 FROM sb ORDER BY id", []string{"1|a|5", "2|a,c|255", "3||0", "4|b,c|16", "5||", "6|a|6"}},
	{"SELECT id FROM sb WHERE s = 'a' ORDER BY id", []string{"1", "6"}},
	{"SELECT id FROM sb WHERE s IN ('a,c', 'b,c') ORDER BY id", []string{"2", "4"}},
	{"SELECT id FROM sb WHERE b > 5 ORDER BY id", []string{"2", "4", "6"}},
	{"SELECT id FROM sb WHERE b IN (5, 16) ORDER BY id", []string{"1", "4"}},
	{"SELECT id FROM sb WHERE b64 > 1 ORDER BY id", []string{"2", "3", "6"}},
	{"SELECT id FROM sb WHERE s = 'a,c' AND b = 255", []string{"2"}},
	{"SELECT id FROM sb ORDER BY b DESC, id", []string{"2", "4", "6", "1", "3", "5"}},
	{"SELECT id, s FROM sb ORDER BY s, id", []string{"5|", "3|", "1|a", "6|a", "2|a,c", "4|b,c"}},
	{"SELECT id FROM `wordpress@dev`.fd WHERE d < 2 ORDER BY id", []string{"3", "5"}},
	{"SELECT id, s FROM `wordpress@dev`.sb ORDER BY s, id", []string{"5|", "3|", "6|a", "1|c", "2|a,c", "4|b,c"}},
}

// checkFormat7Queries は、format7Queries を流して結果を確かめる。値は送られた文字列のまま比べる（rawRows。go-sql-driver は
// FLOAT と DOUBLE を数に読み直す）。
func checkFormat7Queries(t *testing.T, conn *vmysql.Conn, when string) {
	t.Helper()
	for _, c := range format7Queries {
		if got := rawRows(t, conn, c.q); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %s\ngot  %v\nwant %v", when, c.q, got, c.want)
		}
	}
}

// rawRows は、q の結果の行を、値を | でつなぎ（NULL は空）、送られた文字列のまま返す。
func rawRows(t *testing.T, conn *vmysql.Conn, q string) []string {
	t.Helper()
	r, err := conn.ExecuteFetch(q, 10000, false)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	var out []string
	for _, row := range r.Rows {
		parts := make([]string, len(row))
		for i, v := range row {
			if !v.IsNull() {
				parts[i] = v.ToString()
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

// vitessConn は、addr の HeimDB の db に vitess の MySQL クライアントでつなぐ。
func vitessConn(t *testing.T, addr, db string) *vmysql.Conn {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(port)
	conn, err := vmysql.Connect(context.Background(), &vmysql.ConnParams{Host: host, Port: p, Uname: "root", DbName: db})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// 段階 120: データ形式のバージョン 7 のデータディレクトリ（0.2.40 が FLOAT(M,D) を float と保存し、SET と BIT の列のインデックスは
// 水準 1 でエントリを持たない）を開くと、バージョン 8 に書き換え、前からある列は桁のない型のまま、0.2.40 と同じ結果を返す。
func TestStage120Format7Data(t *testing.T) {
	dir := t.TempDir()
	extractDataDir(t, "../testdata/format7_data.tar.gz", dir)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	addr, _, st, srv := serve(t, dir, serverConfig{})
	defer func() { stopServer(t, st, srv)() }()
	upgraded, stale := false, ""
	for _, e := range hook.AllEntries() {
		upgraded = upgraded || e.Message == "upgrading the data format from 7 to 8"
		if strings.Contains(e.Message, "indexes on DECIMAL") {
			stale = e.Message
		}
	}
	if !upgraded {
		t.Error("no log of the upgrade from data format 7")
	}
	// 起動時に、SET と BIT の列の作り直していないインデックスを知らせる（水準 1 の FLOAT、DOUBLE、DECIMAL の索引は含めない）
	if want := "HeimDB: 8 indexes on DECIMAL, FLOAT, DOUBLE, ENUM, SET or BIT columns were made by an older HeimDB (0.2.38 or earlier, or 0.2.40 or earlier for SET and BIT) and have no entries, so they read every row (" +
		"wordpress: sb.ks, sb.kb, sb.kb64, sb.us; wordpress@dev: sb.ks, sb.kb, sb.kb64, sb.us); " +
		"to build them, run CALL heimdb_rebuild_indexes() on the primary while connected to each of these databases"; stale != want {
		t.Errorf("startup log:\n%s\nwant\n%s", stale, want)
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn := vitessConn(t, addr, testDB)
	defer conn.Close()
	checkFormat7Queries(t, conn, "opened")
	// 前からある表の定義の型は float と double のまま（0.2.40 が保存した形）
	for name, want := range map[string]string{"wfLocs": "`lat` float DEFAULT '0'", "fd": "`d` double DEFAULT NULL"} {
		if got := queryRows(t, db, "SHOW CREATE TABLE "+name, false); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("SHOW CREATE TABLE %s: %v", name, got)
		}
	}
	for _, name := range []string{"wfLocs", "fd", "sb"} {
		def, ok, err := st.GetTable(store.MainBranchID, name, store.Latest)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
		for _, idx := range def.Indexes {
			if idx.KeyLevel != 1 {
				t.Errorf("%s.%s: level %d", name, idx.Name, idx.KeyLevel)
			}
		}
	}
	// 前からある桁のない列は、書いても丸めない（0.2.40 と同じ）
	mustExec(t, db, "UPDATE fd SET e = 12.34567 WHERE id = 1")
	mustExec(t, db, "UPDATE sb SET b = 5 WHERE id = 1")
	checkFormat7Queries(t, conn, "after writes")

	// main で作り直すと、SET と BIT の索引は水準 2 になり、同じ結果をエントリで引く。dev（作り直す前に分かれた）は水準 1 のまま
	if got := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false); strings.Join(got, ",") != "sb|ks,sb|kb,sb|kb64,sb|us" {
		t.Errorf("rebuilt %v", got)
	}
	before := entriesDecoded.Load()
	checkFormat7Queries(t, conn, "rebuilt")
	if entriesDecoded.Load() == before {
		t.Error("no entries read after the rebuild")
	}
	if def, _, err := st.GetTable(store.MainBranchID, "sb", store.Latest); err != nil || def.Indexes[0].KeyLevel != 2 {
		t.Errorf("sb levels after the rebuild: %v %v", def, err)
	}
	if plan := explain(t, db, "SELECT id FROM sb WHERE b > 5"); !strings.Contains(plan, "(5, ∞)") {
		t.Errorf("plan after the rebuild:\n%s", plan)
	}
	mustExec(t, db, "UPDATE sb SET s = 'b' WHERE id = 6")
	if got := rawRows(t, conn, "SELECT id FROM sb WHERE s = 'b'"); strings.Join(got, ",") != "6" {
		t.Errorf("after an update: %v", got)
	}
	mustExec(t, db, "UPDATE sb SET s = 'a' WHERE id = 6")

	// 利用者が宣言の形に直すと（Wordfence のバージョンアップの dbDelta が出す ALTER）、MySQL と同じ表示になる
	mustExec(t, db, "ALTER TABLE wfLocs MODIFY lat FLOAT(10,7) DEFAULT 0, MODIFY lon FLOAT(10,7) DEFAULT 0")
	if got := rawRows(t, conn, "SELECT city, lat, lon FROM wfLocs ORDER BY lat"); strings.Join(got, ",") != "Osaka|34.6937370|135.5021667,Tokyo|35.6812363|139.7671204" {
		t.Errorf("after ALTER: %v", got)
	}
	if got := queryRows(t, db, "SELECT city FROM wfLocs WHERE lat > 35", false); strings.Join(got, ",") != "Tokyo" {
		t.Errorf("after ALTER: %v", got)
	}
}
