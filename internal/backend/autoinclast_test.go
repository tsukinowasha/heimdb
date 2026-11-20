package backend

import (
	"slices"
	"testing"
)

// 保存のキーが AUTO_INCREMENT の列だけの表は、最大値を最後の 1 行だけ読んで求め、全行を読んだ値と同じになる
// （docs/plan.md の段階 80）。最後の行を消した表、帯の上の行（マージで入る他のブランチの ID と、符号付きの 64 ビットを
// 超える値）、負の値、空の表、
// 層のあるブランチを含む。複合の主キーの表は全行を読む。
func TestLastAutoIncrementValue(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	for _, q := range []string{
		"CREATE TABLE big (id bigint(20) unsigned NOT NULL AUTO_INCREMENT, v varchar(10), PRIMARY KEY (id))",
		"INSERT INTO big (v) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50) SELECT 'x' FROM n",
		"DELETE FROM big WHERE id >= 49",
		"INSERT INTO big (id, v) VALUES (20000000005, 'above'), (9223372036854775813, 'huge')",
		"CREATE TABLE small (id int(11) NOT NULL AUTO_INCREMENT, v varchar(10), PRIMARY KEY (id))",
		"INSERT INTO small (id, v) VALUES (-5, 'neg'), (1, 'a'), (2, 'b'), (3, 'c')",
		"CREATE TABLE gone (id bigint(20) NOT NULL AUTO_INCREMENT, PRIMARY KEY (id))",
		"INSERT INTO gone () VALUES (), (), ()",
		"DELETE FROM gone",
		"CREATE TABLE blank (id bigint(20) NOT NULL AUTO_INCREMENT, PRIMARY KEY (id))",
		"CREATE TABLE pair (id bigint(20) NOT NULL AUTO_INCREMENT, k int NOT NULL, PRIMARY KEY (k, id), KEY id (id))",
		"INSERT INTO pair (k) VALUES (2), (1), (3)",
		"CALL heimdb_branch_create('staging')",
	} {
		s.mustQuery(t, q)
	}
	staging := testDB + "@staging"
	s.ctx.SetCurrentDatabase(staging)
	for _, q := range []string{
		"INSERT INTO big (v) VALUES ('b1'), ('b2')",
		"DELETE FROM big WHERE v = 'b2'",
		"INSERT INTO small (v) VALUES ('b')",
		"DELETE FROM big WHERE id = 48",
	} {
		s.mustQuery(t, q)
	}
	s.ctx.SetCurrentDatabase(testDB)

	for _, c := range []struct {
		db, table string
		fast      bool
		want      uint64
	}{
		{testDB, "big", true, 48},
		{testDB, "small", true, 3},
		{testDB, "gone", true, 0},
		{testDB, "blank", true, 0},
		{testDB, "pair", false, 3},
		{staging, "big", true, 10_000_000_000},
		{staging, "small", true, 1_000_000_000},
	} {
		db, err := s.engine.Analyzer.Catalog.Database(s.ctx, c.db)
		if err != nil {
			t.Fatal(err)
		}
		tbl, ok, err := db.GetTableInsensitive(s.ctx, c.table)
		if err != nil || !ok {
			t.Fatalf("%s.%s: %v", c.db, c.table, err)
		}
		table := tbl.(*Table)
		ord := table.autoIncrementOrdinal()
		if fast := slices.Equal(table.def.PKOrdinals, []int{ord}); fast != c.fast {
			t.Errorf("%s.%s: reads the last row only = %v", c.db, c.table, fast)
		}
		before := rowsDecoded.Load()
		got, err := table.maxAutoIncrementValue(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if decoded := rowsDecoded.Load() - before; c.fast && decoded > 1 {
			t.Errorf("%s.%s: decoded %d rows to find the last one", c.db, c.table, decoded)
		}
		lo, hi := table.autoIncrementRange()
		scanned, err := table.scanAutoIncrementValue(s.ctx, ord, lo, hi)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want || scanned != c.want {
			t.Errorf("%s.%s: last row %d, all rows %d, want %d", c.db, c.table, got, scanned, c.want)
		}
	}
}
