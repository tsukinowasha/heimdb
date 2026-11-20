package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// stage117Group は、段階 117 の DECIMAL、FLOAT、DOUBLE、ENUM の列のインデックス（エントリを持つ。docs/plan.md の
// 段階 117）で、本物の MySQL 8.0 と同じ結果になるかを確かめる文。setup で表を作り、stmts を流し、drop で表を消す。
// FLOAT の列と小数の定数の比較（f = 0.1）は HeimDB と MySQL で違う（docs/differences.md）ので入れない。
type stage117Group struct {
	name  string
	setup []string
	stmts []string
	drop  []string
}

var stage117Groups = []stage117Group{
	{
		name: "decimal", // DECIMAL(19,4) のインデックス: 範囲、等号、IN、並び（逆向きを含む）、行の変更の後
		setup: []string{
			"CREATE TABLE dd (id INT PRIMARY KEY, d DECIMAL(19,4) NULL, KEY kd (d))",
			"INSERT INTO dd VALUES (1, 9.9999), (2, 10.0000), (3, 10.0001), (4, 10.5000), (5, -0.0001), (6, 0), (7, NULL), (8, 999999999999999.9999), (9, -999999999999999.9999), (10, -0.0000), (11, 10.0000)",
		},
		stmts: []string{
			"SELECT id, d FROM dd WHERE d > 10.00005 ORDER BY d, id",
			"SELECT id, d FROM dd WHERE d >= 10.00005 ORDER BY d, id",
			"SELECT id, d FROM dd WHERE d < 10.00005 ORDER BY d, id",
			"SELECT id, d FROM dd WHERE d <= 10 ORDER BY d, id",
			"SELECT id, d FROM dd WHERE d = 10.00005",
			"SELECT id, d FROM dd WHERE d = 10 ORDER BY id",
			"SELECT id, d FROM dd WHERE d = 10.0000 ORDER BY id",
			"SELECT id, d FROM dd WHERE d = '10.00000' ORDER BY id",
			"SELECT id, d FROM dd WHERE d = 0 ORDER BY id",
			"SELECT id, d FROM dd WHERE d = -0.0001",
			"SELECT id, d FROM dd WHERE d BETWEEN 9.99995 AND 10.00005 ORDER BY d, id",
			"SELECT id, d FROM dd WHERE d BETWEEN -1 AND 1 ORDER BY d, id",
			"SELECT id, d FROM dd WHERE d IN (10, 10.5, -0.0001, 3) ORDER BY id",
			"SELECT id, d FROM dd WHERE d NOT IN (10, 10.5) ORDER BY id",
			"SELECT id, d FROM dd WHERE d > 1e30",
			"SELECT id, d FROM dd WHERE d < -1e30",
			"SELECT id, d FROM dd WHERE d IS NULL",
			"SELECT id, d FROM dd WHERE d IS NOT NULL ORDER BY d DESC, id DESC",
			"SELECT id, d FROM dd ORDER BY d, id",
			"SELECT id, d FROM dd ORDER BY d DESC, id DESC",
			"SELECT id, d FROM dd ORDER BY d LIMIT 3",
			"SELECT id, d FROM dd WHERE d > 0 ORDER BY d DESC LIMIT 2",
			"SELECT MIN(d), MAX(d), COUNT(d) FROM dd",
			"SELECT d, COUNT(*) FROM dd GROUP BY d ORDER BY d",
			"SELECT COUNT(*) FROM dd WHERE d >= 10",
			"UPDATE dd SET d = 10.2500 WHERE id = 3",
			"DELETE FROM dd WHERE id = 1",
			"INSERT INTO dd VALUES (12, 10.12), (13, -5)",
			"UPDATE dd SET d = d + 1 WHERE d BETWEEN 10 AND 10.3",
			"SELECT id, d FROM dd WHERE d BETWEEN 9 AND 12 ORDER BY d, id",
			"SELECT id, d FROM dd ORDER BY d, id",
		},
		drop: []string{"DROP TABLE dd"},
	},
	{
		name: "mixed", // (VARCHAR, DECIMAL) と (DECIMAL, INT) の複数列のインデックス
		setup: []string{
			"CREATE TABLE mm (id INT PRIMARY KEY, s VARCHAR(10), d DECIMAL(10,2), n INT, KEY ksd (s, d), KEY kdn (d, n))",
			"INSERT INTO mm VALUES (1,'a',1.00,1),(2,'a',2.50,2),(3,'b',0.10,3),(4,'a',NULL,4),(5,'a',1.01,5),(6,'b',2.50,1),(7,'a',2.50,0)",
		},
		stmts: []string{
			"SELECT id FROM mm WHERE s = 'a' AND d > 1.005 ORDER BY id",
			"SELECT id FROM mm WHERE s = 'a' AND d = 2.5 ORDER BY id",
			"SELECT id FROM mm WHERE s = 'a' AND d < 2 ORDER BY d, id",
			"SELECT id, d, n FROM mm WHERE d = 2.50 AND n >= 1 ORDER BY id",
			"SELECT id, d, n FROM mm WHERE d = 2.50 ORDER BY d, n",
			"SELECT id, s, d FROM mm WHERE s IN ('a', 'b') AND d BETWEEN 0.1 AND 1.01 ORDER BY s, d",
			"SELECT id FROM mm ORDER BY d DESC, n DESC",
		},
		drop: []string{"DROP TABLE mm"},
	},
	{
		name: "float", // FLOAT と DOUBLE のインデックス（FLOAT は単精度で表せる値だけ比べる）
		setup: []string{
			"CREATE TABLE ff (id INT PRIMARY KEY, f FLOAT NULL, g DOUBLE NULL, KEY kf (f), KEY kg (g))",
			"INSERT INTO ff VALUES (1, 0.5, 0.1), (2, 0, 0), (3, -0.0, -0.0), (4, 1e38, 1e300), (5, -1e38, -1e300), (6, NULL, NULL), (7, 0.25, 0.3), (8, 1.5, 1.5), (9, -2.5, -2.5), (10, 1.5, 0.30000000000000004)",
		},
		stmts: []string{
			"SELECT id, g FROM ff WHERE g = 0.1",
			"SELECT id, g FROM ff WHERE g = 0.3",
			"SELECT id, g FROM ff WHERE g > 0.1 ORDER BY g, id",
			"SELECT id, g FROM ff WHERE g >= 0.1 ORDER BY g, id",
			"SELECT id, g FROM ff WHERE g < 0 ORDER BY g, id",
			"SELECT id, g FROM ff WHERE g = 0 ORDER BY id",
			"SELECT id, g FROM ff WHERE g = -0.0 ORDER BY id",
			"SELECT id, g FROM ff WHERE g BETWEEN -3 AND 0.3 ORDER BY g, id",
			"SELECT id, g FROM ff WHERE g IN (0.1, 1.5, 7) ORDER BY id",
			"SELECT id, g FROM ff WHERE g = '0.1'",
			"SELECT id, g FROM ff WHERE g > 1e299 ORDER BY id",
			"SELECT id, g FROM ff ORDER BY g, id",
			"SELECT id, g FROM ff ORDER BY g DESC, id DESC",
			"SELECT id, f FROM ff WHERE f = 0.5",
			"SELECT id, f FROM ff WHERE f = 1.5 ORDER BY id",
			"SELECT id, f FROM ff WHERE f > 0.5 ORDER BY f, id",
			"SELECT id, f FROM ff WHERE f >= 0.25 ORDER BY f, id",
			"SELECT id, f FROM ff WHERE f < 0 ORDER BY f, id",
			"SELECT id, f FROM ff WHERE f = 0 ORDER BY id",
			"SELECT id, f FROM ff WHERE f BETWEEN -3 AND 0.25 ORDER BY f, id",
			"SELECT id, f FROM ff WHERE f IN (0.5, 1.5) ORDER BY id",
			"SELECT id, f FROM ff ORDER BY f, id",
			"SELECT id, f FROM ff ORDER BY f DESC, id DESC",
			"SELECT MIN(f), MAX(f), MIN(g), MAX(g) FROM ff",
			"UPDATE ff SET g = 0.2, f = 0.75 WHERE id = 1",
			"DELETE FROM ff WHERE id = 7",
			"SELECT id, f, g FROM ff WHERE g BETWEEN 0 AND 1 ORDER BY g, id",
			"SELECT id, f FROM ff WHERE f > 0.5 ORDER BY f, id",
		},
		drop: []string{"DROP TABLE ff"},
	},
	{
		name: "enum", // ENUM のインデックス（番号の順）。文字列との大小の比較は HeimDB が番号で比べる（docs/differences.md）ので入れない
		setup: []string{
			"CREATE TABLE ee (id INT PRIMARY KEY, e ENUM('z','b','a') NULL, KEY ke (e))",
			"INSERT INTO ee VALUES (1,'z'),(2,'b'),(3,'a'),(4,NULL),(5,'z'),(6,'a')",
		},
		stmts: []string{
			"SELECT id, e FROM ee ORDER BY e, id",
			"SELECT id, e FROM ee ORDER BY e DESC, id DESC",
			"SELECT id, e FROM ee WHERE e = 'a' ORDER BY id",
			"SELECT id, e FROM ee WHERE e = 'A' ORDER BY id",
			"SELECT id, e FROM ee WHERE e = 2 ORDER BY id",
			"SELECT id, e FROM ee WHERE e > 1 ORDER BY id",
			"SELECT id, e FROM ee WHERE e IN ('a','z') ORDER BY id",
			"SELECT id, e FROM ee WHERE e <> 'z' ORDER BY id",
			"SELECT id, e FROM ee WHERE e = 'q'",
			"SELECT id, e FROM ee WHERE e IS NULL",
			"SELECT e, COUNT(*) FROM ee GROUP BY e ORDER BY e",
			"UPDATE ee SET e = 'b' WHERE id = 1",
			"DELETE FROM ee WHERE id = 3",
			"INSERT INTO ee VALUES (7, 'a')",
			"SELECT id, e FROM ee WHERE e = 'a' ORDER BY id",
			"SELECT id, e FROM ee WHERE e = 'b' ORDER BY id",
			"SELECT id, e FROM ee ORDER BY e, id",
		},
		drop: []string{"DROP TABLE ee"},
	},
	{
		name: "unique", // 一意インデックスの重複の判定（1.5、1.50、1.504 と、0 と -0.0）
		setup: []string{
			"CREATE TABLE uu (id INT PRIMARY KEY, d DECIMAL(10,2), UNIQUE KEY ud (d))",
			"CREATE TABLE uf (id INT PRIMARY KEY, f FLOAT, g DOUBLE, UNIQUE KEY uf (f), UNIQUE KEY ug (g))",
			"CREATE TABLE ue (id INT PRIMARY KEY, e ENUM('a','b'), UNIQUE KEY ue (e))",
			"CREATE TABLE un (id INT PRIMARY KEY, d DECIMAL(10,2), f DOUBLE)",
		},
		stmts: []string{
			"INSERT INTO uu VALUES (1, 1.5)",
			"INSERT INTO uu VALUES (2, 1.50)",
			"INSERT INTO uu VALUES (3, 1.504)",
			"INSERT INTO uu VALUES (4, 1.506)",
			"INSERT INTO uu VALUES (5, NULL), (6, NULL)",
			"UPDATE uu SET d = 1.5 WHERE id = 4",
			"UPDATE uu SET d = 1.49 WHERE id = 4",
			"INSERT INTO uu VALUES (7, 1.51)",
			"SELECT id, d FROM uu ORDER BY id",
			"INSERT INTO uf VALUES (1, 0, 1)",
			"INSERT INTO uf VALUES (2, -0.0, 2)",
			"INSERT INTO uf VALUES (3, 1, -0.0)",
			"INSERT INTO uf VALUES (4, 2, 0)",
			"INSERT INTO uf VALUES (5, 2.5, 1)",
			"SELECT id, f, g FROM uf ORDER BY id",
			"INSERT INTO ue VALUES (1, 'a')",
			"INSERT INTO ue VALUES (2, 'A')",
			"INSERT INTO ue VALUES (3, 1)",
			"INSERT INTO ue VALUES (4, 'b')",
			"SELECT id, e FROM ue ORDER BY id",
			// CREATE UNIQUE INDEX と、表を書き直す ALTER の重複の判定
			"INSERT INTO un VALUES (1, 1.50, 0), (2, 2.00, -0.0), (3, 1.5, 3)",
			"CREATE UNIQUE INDEX und ON un (d)",
			"CREATE UNIQUE INDEX unf ON un (f)",
			"UPDATE un SET d = 3 WHERE id = 3",
			"CREATE UNIQUE INDEX und ON un (d)",
			"INSERT INTO un VALUES (4, 3.00, 4)",
			"ALTER TABLE un ADD COLUMN x INT",
			"INSERT INTO un VALUES (5, 2.0, 5, 1)",
			"ALTER TABLE un MODIFY d DECIMAL(10,0)",
			"ALTER TABLE un MODIFY d DECIMAL(12,2)",
			"INSERT INTO un VALUES (6, 2.0, 6, 1)",
			"SELECT id, d, f, x FROM un ORDER BY id",
		},
		drop: []string{"DROP TABLE uu", "DROP TABLE uf", "DROP TABLE ue", "DROP TABLE un"},
	},
	{
		name: "strpk", // 文字列の主キーの表（エントリの後ろの主キーを読み飛ばして取り出す）
		setup: []string{
			"CREATE TABLE sp (code VARCHAR(20) PRIMARY KEY, amount DECIMAL(10,2) NOT NULL, rate DOUBLE, kind ENUM('x','y'), KEY ka (amount), KEY kr (rate), KEY kk (kind, amount))",
			"INSERT INTO sp VALUES ('b', 1.50, 0.5, 'x'), ('a', -2.25, 1.5, 'y'), ('c', 0, NULL, 'x'), ('d', 1.5, -0.5, NULL), ('ee', 100, 2, 'y')",
		},
		stmts: []string{
			"SELECT code, amount FROM sp WHERE amount >= 0 ORDER BY amount, code",
			"SELECT code FROM sp WHERE amount = 1.5 ORDER BY code",
			"SELECT code, rate FROM sp WHERE rate > 0 ORDER BY rate",
			"SELECT code FROM sp WHERE kind = 'x' AND amount > 0 ORDER BY code",
			"SELECT code FROM sp ORDER BY kind DESC, amount DESC, code DESC",
			"UPDATE sp SET amount = 7 WHERE code = 'c'",
			"SELECT code, amount FROM sp WHERE amount > 1 ORDER BY amount, code",
		},
		drop: []string{"DROP TABLE sp"},
	},
	{
		name: "woo", // WooCommerce の wc_product_meta_lookup の価格と在庫の絞り込み、DECIMAL の列での結合
		setup: []string{
			"CREATE TABLE wp_wc_product_meta_lookup (product_id BIGINT NOT NULL, sku VARCHAR(100) NULL DEFAULT '', min_price DECIMAL(19,4) NULL DEFAULT NULL, max_price DECIMAL(19,4) NULL DEFAULT NULL, " +
				"stock_quantity DOUBLE NULL DEFAULT NULL, stock_status VARCHAR(100) NULL DEFAULT 'instock', PRIMARY KEY (product_id), KEY stock_status (stock_status), KEY stock_quantity (stock_quantity), KEY min_max_price (min_price, max_price))",
			"INSERT INTO wp_wc_product_meta_lookup (product_id, min_price, max_price, stock_quantity) VALUES (1, 10.0000, 12.5000, 5), (2, 9.9950, 9.9950, NULL), (3, 12345678901234.5678, 12345678901234.5678, 1), " +
				"(4, 0, 0, 0), (5, 20, 30, -1), (6, 25.0000, 25.0000, 2.5), (7, NULL, NULL, 0), (8, 9.99, 19.99, 10)",
			"CREATE TABLE price_list (id INT PRIMARY KEY, price DECIMAL(19,4), KEY kp (price))",
			"INSERT INTO price_list VALUES (1, 10), (2, 25), (3, 9.995), (4, 7), (5, NULL), (6, 25.00001)",
		},
		stmts: []string{
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 10.000000 AND max_price <= 25.000000 ORDER BY product_id",
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.99501 AND max_price <= 19.99985 ORDER BY product_id",
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE max_price >= 0 AND min_price <= 10 ORDER BY product_id",
			"SELECT product_id, min_price FROM wp_wc_product_meta_lookup ORDER BY min_price ASC, product_id ASC",
			"SELECT product_id, max_price FROM wp_wc_product_meta_lookup ORDER BY max_price DESC, product_id DESC",
			"SELECT MIN(min_price) AS min_price, MAX(max_price) AS max_price FROM wp_wc_product_meta_lookup WHERE product_id IN (1, 2, 5, 8)",
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE stock_quantity <= 0 ORDER BY product_id",
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE stock_quantity > 2 ORDER BY product_id",
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE stock_quantity = 2.5",
			"SELECT product_id, stock_quantity FROM wp_wc_product_meta_lookup WHERE stock_quantity IS NOT NULL ORDER BY stock_quantity, product_id",
			"SELECT l.id, p.product_id FROM price_list l JOIN wp_wc_product_meta_lookup p ON p.min_price = l.price ORDER BY l.id, p.product_id",
			"SELECT /*+ LOOKUP_JOIN(p, l) */ p.product_id, l.id FROM wp_wc_product_meta_lookup p JOIN price_list l ON l.price = p.max_price ORDER BY p.product_id, l.id",
			"SELECT /*+ LOOKUP_JOIN(l, p) */ l.id, p.product_id FROM price_list l LEFT JOIN wp_wc_product_meta_lookup p ON p.stock_quantity = l.price ORDER BY l.id, p.product_id",
			"UPDATE wp_wc_product_meta_lookup SET min_price = 11, max_price = 11 WHERE product_id = 1",
			"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 10.000000 AND max_price <= 25.000000 ORDER BY product_id",
		},
		drop: []string{"DROP TABLE price_list", "DROP TABLE wp_wc_product_meta_lookup"},
	},
}

// stage117Statements は、組をつないだ文の列。
func stage117Statements() []string {
	stmts := []string{"SET SESSION sql_mode = DEFAULT"}
	for _, g := range stage117Groups {
		stmts = append(stmts, g.setup...)
		stmts = append(stmts, g.stmts...)
		stmts = append(stmts, g.drop...)
	}
	return stmts
}

func TestStage117Golden(t *testing.T) {
	checkGolden(t, "../testdata/stage117_golden.txt", "heimdb_golden117", stage117Statements())
}

// stage117PlanDB は、組 name の setup を流したサーバを返す（計画を見るテスト用）。
func stage117PlanDB(t *testing.T, names ...string) (*sql.DB, func()) {
	t.Helper()
	dsn, stop := startServer(t, t.TempDir())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, g := range stage117Groups {
		for _, n := range names {
			if g.name == n {
				for _, q := range g.setup {
					mustExec(t, db, q)
				}
			}
		}
	}
	return db, func() { db.Close(); stop() }
}

// 新しい型のインデックスを範囲で引く（全件を読まない）。読んだエントリの数で確かめる。
func TestStage117Plans(t *testing.T) {
	db, stop := stage117PlanDB(t, "decimal", "float", "enum", "woo")
	defer stop()
	for _, c := range []struct {
		q    string
		want []string
		// entries は、文が読むエントリの数（範囲で引けば、当てはまる行の数ほど）
		entries int64
	}{
		{"SELECT id FROM dd WHERE d > 10.00005", []string{"IndexedTableAccess(dd)", "filters: [{[10.0001, ∞)}]"}, 4},
		{"SELECT id FROM dd WHERE d = 10", []string{"filters: [{[10, 10]}]"}, 2},
		// 端は列の桁に丸めて含む側にする（段階 115）ので、10.0001 の行も読み、Filter で落とす
		{"SELECT id FROM dd WHERE d BETWEEN 9.99995 AND 10.00005", []string{"filters: [{[10, 10.0001]}]"}, 3},
		{"SELECT id FROM dd WHERE d = '10.0000'", []string{"filters: [{[10, 10]}]"}, 2},
		// NULL の行のエントリも区間の先頭に入る
		{"SELECT id FROM dd WHERE d < 0", []string{"filters: [{(NULL, 0)}]"}, 3},
		{"SELECT id FROM ff WHERE g > 0.1", []string{"IndexedTableAccess(ff)", "filters: [{(0.1, ∞)}]"}, 4},
		{"SELECT id FROM ff WHERE g = 0.3", []string{"filters: [{[0.3, 0.3]}]"}, 1},
		{"SELECT id FROM ff WHERE f BETWEEN -3 AND 0.25", []string{"IndexedTableAccess(ff)"}, 5},
		{"SELECT id FROM ff WHERE f > 0.5", []string{"filters: [{(0.5, ∞)}]"}, 4},
		{"SELECT id FROM ee WHERE e = 'a'", []string{"IndexedTableAccess(ee)", "filters: [{[3, 3]}]"}, 2},
		{"SELECT id FROM ee WHERE e IN ('a','z')", []string{"filters: [{[1, 1]}, {[3, 3]}]"}, 4},
		{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 10.000000 AND max_price <= 25.000000", []string{"index: [wp_wc_product_meta_lookup.min_price,wp_wc_product_meta_lookup.max_price]"}, 4},
		{"SELECT product_id FROM wp_wc_product_meta_lookup WHERE stock_quantity > 2", []string{"index: [wp_wc_product_meta_lookup.stock_quantity]"}, 3},
	} {
		plan := explain(t, db, c.q)
		for _, w := range c.want {
			if !strings.Contains(plan, w) {
				t.Errorf("%s\nplan has no %s:\n%s", c.q, w, plan)
			}
		}
		before := entriesDecoded.Load()
		rows, err := db.Query(c.q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		rows.Close()
		// 0 なら、エントリのないインデックスとして全件を読んだ
		if n := entriesDecoded.Load() - before; n == 0 || n > c.entries {
			t.Errorf("%s: %d entries read, want 1 to %d", c.q, n, c.entries)
		}
	}
}

// DECIMAL と DOUBLE の列の lookup JOIN は、点の区間を 1 回で作る経路（pointLookup）で引き、範囲の条件で絞り直さない。
// FLOAT は絞り直す。
func TestStage117LookupJoin(t *testing.T) {
	db, stop := stage117PlanDB(t, "woo", "float")
	defer stop()
	for _, c := range []struct {
		q     string
		exact bool
	}{
		{"SELECT /*+ LOOKUP_JOIN(l, p) */ l.id, p.product_id FROM price_list l JOIN wp_wc_product_meta_lookup p ON p.min_price = l.price", true},
		{"SELECT /*+ LOOKUP_JOIN(p, l) */ p.product_id, l.id FROM wp_wc_product_meta_lookup p JOIN price_list l ON l.price = p.max_price", true},
		{"SELECT /*+ LOOKUP_JOIN(a, b) */ a.id, b.id FROM ff a JOIN ff b ON b.g = a.g", true},
		{"SELECT /*+ LOOKUP_JOIN(a, b) */ a.id, b.id FROM ff a JOIN ff b ON b.f = a.f", false},
	} {
		if plan := explain(t, db, c.q); !strings.Contains(plan, "LookupJoin") {
			t.Errorf("%s\nnot a lookup join:\n%s", c.q, plan)
			continue
		}
		points, evals := pointLookups.Load(), rangeFilterEvals.Load()
		rows, err := db.Query(c.q)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		if pointLookups.Load() == points {
			t.Errorf("%s: no point lookups", c.q)
		}
		if got := rangeFilterEvals.Load() > evals; got == c.exact {
			t.Errorf("%s: range filter evaluated = %v, want %v (%d rows)", c.q, got, !c.exact, n)
		}
	}
}
