package backend

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 段階 78: 主キーの区間が範囲をちょうど表すときは、読んだ行を範囲の条件で絞り直さない。ちょうど表さない値（型の
// 違う定数、範囲の外、小数）では絞り直し、どちらでも返す行は同じ。
func TestPKRangeFilterDropped(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE tr (object_id bigint unsigned NOT NULL, term_taxonomy_id bigint unsigned NOT NULL, term_order int NOT NULL, PRIMARY KEY (object_id, term_taxonomy_id))")
	s.mustQuery(t, "INSERT INTO tr VALUES (1, 1, 0), (1, 2, 0), (2, 1, 0), (3, 5, 0), (4, 1, 0)")
	s.mustQuery(t, "CREATE TABLE sk (k varchar(10) NOT NULL PRIMARY KEY, v int NOT NULL) COLLATE utf8mb4_unicode_520_ci")
	s.mustQuery(t, "INSERT INTO sk VALUES ('a', 1), ('B', 2), ('c', 3)")

	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string, ranges sql.MySQLRangeCollection) (filtered bool, got string) {
		t.Helper()
		tbl, ok, err := db.GetTableInsensitive(s.ctx, name)
		if err != nil || !ok {
			t.Fatalf("table %s: %v", name, err)
		}
		table := tbl.(*Table)
		idxs, err := table.GetIndexes(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
		iter, err := table.lookupRows(s.ctx, sql.IndexLookup{Index: idxs[0], Ranges: ranges})
		if err != nil {
			t.Fatal(err)
		}
		if it, ok := iter.(*scanRowIter); ok {
			filtered = it.filter != nil
		} else {
			t.Fatalf("%s: iterator %T, want *scanRowIter", name, iter)
		}
		var b strings.Builder
		for {
			row, err := iter.Next(s.ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(&b, row[:2])
		}
		return filtered, b.String()
	}
	u := types.Uint64
	point := func(v any) sql.MySQLRangeColumnExpr { return sql.ClosedRangeColumnExpr(v, v, u) }
	all := sql.AllRangeColumnExpr(u)
	str := types.MustCreateString(sqltypes.VarChar, 10, sql.Collation_utf8mb4_unicode_520_ci)
	for _, tc := range []struct {
		name     string
		table    string
		ranges   sql.MySQLRangeCollection
		filtered bool
		want     string
	}{
		// 記事の term の JOIN の形（object_id IN (...)、term_taxonomy_id はすべて）
		{"points", "tr", sql.MySQLRangeCollection{{point(uint64(1)), all}, {point(uint64(3)), all}}, false, "[1 1][1 2][3 5]"},
		{"full key", "tr", sql.MySQLRangeCollection{{point(uint64(1)), point(uint64(2))}, {point(uint64(2)), point(uint64(1))}}, false, "[1 2][2 1]"},
		{"range", "tr", sql.MySQLRangeCollection{{sql.ClosedRangeColumnExpr(uint64(2), uint64(3), u), all}}, false, "[2 1][3 5]"},
		{"open range", "tr", sql.MySQLRangeCollection{{sql.GreaterThanRangeColumnExpr(uint64(2), u), all}}, false, "[3 5][4 1]"},
		// 文字列の主キーは照合順序で比べる（'b' は 'B' の行）
		{"string", "sk", sql.MySQLRangeCollection{{sql.ClosedRangeColumnExpr("b", "b", str)}, {sql.ClosedRangeColumnExpr("c", "c", str)}}, false, "[B 2][c 3]"},
		// 列の型にそのまま入らない端は、区間を広げて読むので絞り直す
		{"float", "tr", sql.MySQLRangeCollection{{sql.ClosedRangeColumnExpr(1.5, 3.5, types.Float64), all}}, true, "[2 1][3 5]"},
		{"string on int", "tr", sql.MySQLRangeCollection{{sql.ClosedRangeColumnExpr("2", "2", types.LongText), all}, {point(uint64(4)), all}}, true, "[2 1][4 1]"},
		{"out of range", "tr", sql.MySQLRangeCollection{{sql.ClosedRangeColumnExpr(int64(-1), int64(1), types.Int64), all}}, true, "[1 1][1 2]"},
		{"partial second column", "tr", sql.MySQLRangeCollection{{all, point(uint64(1))}}, true, "[1 1][2 1][4 1]"},
	} {
		filtered, got := lookup(tc.table, tc.ranges)
		if filtered != tc.filtered || got != tc.want {
			t.Errorf("%s: filtered %v rows %s, want %v %s", tc.name, filtered, got, tc.filtered, tc.want)
		}
	}
}
