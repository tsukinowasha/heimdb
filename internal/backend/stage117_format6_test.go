package backend

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// extractFormat6 は、internal/testdata/format6_data.tar.gz（3d02beb の HeimDB、データ形式のバージョン 6 で
// internal/testdata/format6_data.sql を流して作ったデータディレクトリ）を dir に展開する。
func extractFormat6(t *testing.T, dir string) {
	t.Helper()
	extractDataDir(t, "../testdata/format6_data.tar.gz", dir)
}

// extractDataDir は、データディレクトリの中身の tar.gz の path を dir に展開する（段階 117、120）。
func extractDataDir(t *testing.T, path, dir string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.Create(filepath.Join(dir, filepath.Base(h.Name)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
		out.Close()
	}
}

// format6Queries は、バージョン 6 のデータで流す文と、3d02beb の HeimDB が返した結果（行を | でつなぐ）。
var format6Queries = []struct {
	q    string
	want []string
}{
	{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.995 AND max_price <= 12.5 ORDER BY product_id", []string{"1", "2", "6"}},
	{"SELECT product_id, min_price FROM wp_wc_product_meta_lookup WHERE min_price > 0 ORDER BY min_price, product_id", []string{"2|9.9950", "1|10.0000", "6|10.0000"}},
	{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE stock_quantity > 0 ORDER BY product_id", []string{"1", "5", "6"}},
	{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE stock_quantity = 5 ORDER BY product_id", []string{"1", "6"}},
	{"SELECT product_id, min_price FROM wp_wc_product_meta_lookup ORDER BY min_price DESC, product_id DESC", []string{"6|10.0000", "1|10.0000", "2|9.9950", "3|0.0000", "4|-1.5000", "5|"}},
	{"SELECT id FROM ft WHERE f > 0 ORDER BY id", []string{"4"}},
	{"SELECT id FROM ft WHERE f = 0 ORDER BY id", []string{"2", "3"}},
	{"SELECT id, e FROM ft WHERE e = 'a' ORDER BY id", []string{"3|a", "6|a"}},
	{"SELECT id, e FROM ft ORDER BY e, id", []string{"4|", "5|z", "2|b", "3|a", "6|a"}},
	{"SELECT code FROM sp WHERE amount BETWEEN -3 AND 1.5 ORDER BY code", []string{"x", "y", "z"}},
	{"SELECT code, amount FROM sp ORDER BY amount", []string{"y|-2.25", "z|0.00", "x|1.50"}},
	{"SELECT id FROM `wordpress@dev`.ft WHERE f > 0 ORDER BY id", []string{"1", "4", "7"}},
	{"SELECT product_id FROM `wordpress@dev`.wp_wc_product_meta_lookup WHERE min_price < 8 ORDER BY product_id", []string{"3", "4", "6"}},
}

// checkFormat6Queries は、format6Queries を流して結果を確かめる。
func checkFormat6Queries(t *testing.T, db *sql.DB, when string) {
	t.Helper()
	for _, c := range format6Queries {
		if got := queryRows(t, db, c.q, false); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %s\ngot  %v\nwant %v", when, c.q, got, c.want)
		}
	}
}

// 段階 117: データ形式のバージョン 6 のデータディレクトリ（DECIMAL、FLOAT、DOUBLE、ENUM の列のインデックスがエントリを持たない）を
// 開くと、バージョン 7 に書き換え、水準のないインデックスは前と同じくエントリなしで、3d02beb と同じ結果を返す。行を変えても
// 結果は同じ。
func TestStage117Format6Data(t *testing.T) {
	dir := t.TempDir()
	extractFormat6(t, dir)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	addr, _, st, srv := serve(t, dir, serverConfig{})
	defer func() { stopServer(t, st, srv)() }()
	upgraded, stale := false, ""
	for _, e := range hook.AllEntries() {
		upgraded = upgraded || e.Message == "upgrading the data format from 6 to 8"
		if strings.Contains(e.Message, "indexes on DECIMAL") {
			stale = e.Message
		}
	}
	if !upgraded {
		t.Error("no log of the upgrade from data format 6")
	}
	// 起動時に、作り直していないインデックスをデータベースごとに知らせる（整数と文字列だけの virtual、sku などは含めない）
	if want := "HeimDB: 12 indexes on DECIMAL, FLOAT, DOUBLE, ENUM, SET or BIT columns were made by an older HeimDB (0.2.38 or earlier, or 0.2.40 or earlier for SET and BIT) and have no entries, so they read every row (" +
		"wordpress: ft.kf, ft.ke, ft.kfe, sp.ua, wp_wc_product_meta_lookup.stock_quantity, wp_wc_product_meta_lookup.min_max_price; " +
		"wordpress@dev: ft.kf, ft.ke, ft.kfe, sp.ua, wp_wc_product_meta_lookup.stock_quantity, wp_wc_product_meta_lookup.min_max_price); " +
		"to build them, run CALL heimdb_rebuild_indexes() on the primary while connected to each of these databases"; stale != want {
		t.Errorf("startup log:\n%s\nwant\n%s", stale, want)
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	checkFormat6Queries(t, db, "opened")
	// バージョン 6 の定義には水準がなく（0）、DECIMAL などのインデックスは前と同じく全件を読む（範囲の区間にしない）
	for _, name := range []string{"wp_wc_product_meta_lookup", "ft", "sp"} {
		def, ok, err := st.GetTable(store.MainBranchID, name, store.Latest)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
		for _, idx := range def.Indexes {
			if idx.KeyLevel != 0 {
				t.Errorf("%s.%s: level %d", name, idx.Name, idx.KeyLevel)
			}
		}
	}
	before := entriesDecoded.Load()
	checkFormat6Queries(t, db, "entries")
	if n := entriesDecoded.Load() - before; n != 0 {
		t.Errorf("%d index entries decoded from indexes without entries", n)
	}
	// 書いても前と同じく読める（エントリのないインデックスは保守しない）
	mustExec(t, db, "UPDATE ft SET f = 0.5 WHERE id = 4")
	mustExec(t, db, "UPDATE ft SET f = 1.5 WHERE id = 4")
	mustExec(t, db, "UPDATE sp SET amount = 1.50 WHERE code = 'x'")
	checkFormat6Queries(t, db, "after writes")
	// 一意インデックスの重複は、エントリがなくても前と同じく拒否する
	if _, err := db.Exec("INSERT INTO sp VALUES ('w', 1.5)"); err == nil || !strings.Contains(err.Error(), "1062") {
		t.Errorf("duplicate on an index without entries: %v", err)
	}

	// main で作り直すと、同じ結果をエントリで引く。dev（作り直す前に分かれた）は水準 0 のまま
	if got := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false); len(got) != 6 {
		t.Errorf("rebuilt %v", got)
	}
	before = entriesDecoded.Load()
	checkFormat6Queries(t, db, "rebuilt")
	if entriesDecoded.Load() == before {
		t.Error("no entries read after the rebuild")
	}
	if plan := explain(t, db, "SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.995 AND max_price <= 12.5"); !strings.Contains(plan, "filters: [{[9.995, ∞), [NULL, ∞)}]") {
		t.Errorf("plan after the rebuild:\n%s", plan)
	}
	mustExec(t, db, "USE `wordpress@dev`")
	if got := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false); len(got) != 6 {
		t.Errorf("rebuilt on dev %v", got)
	}
	mustExec(t, db, "USE wordpress")
	checkFormat6Queries(t, db, "rebuilt on dev")
	hook.Reset()
	stopServer(t, st, srv)()
	addr, _, st, srv = serve(t, dir, serverConfig{})
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "indexes on DECIMAL") || strings.Contains(e.Message, "upgrading") {
			t.Errorf("log after the rebuild: %s", e.Message)
		}
	}
}
