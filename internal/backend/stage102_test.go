package backend

import (
	"bytes"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
)

// pointLookup（lookup JOIN が引く形の範囲を、点の値を 1 回だけ変換して区間にする）の結果が、今までの経路
// （mergeIntervals と indexInterval、exactIntervals、shortPrefixPoints）と同じ（docs/plan.md の段階 102）。インデックスの
// 列の型（符号なしの整数、整数、照合順序のある文字列とそのプレフィックス長、日時、バイナリとそのプレフィックス長、
// 固定長の文字列）ごとに、列の型に合う値と合わない値（文字列の数、範囲外の数、小数、数の列への文字列、不正な日時、
// ちょうどプレフィックス長の文字列（ASCII と多バイト）、不正な UTF-8）をランダムに点に置く。点の後はすべての値の列。点でない列、すべての値の後の点、
// 範囲が 2 つの形は pointLookup が受けない。
func TestStage102PointLookup(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE t102 (id bigint unsigned NOT NULL AUTO_INCREMENT, u bigint unsigned, i int, s varchar(255), d datetime, b varbinary(100), c char(10), PRIMARY KEY (id), "+
		"KEY us (u, s(10)), KEY id2 (i, d), KEY bs (b(4), i), KEY sc (s(191), c), KEY s1 (s), KEY uic (u, i, c)) DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_520_ci")
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, ok, err := db.GetTableInsensitive(s.ctx, "t102")
	if err != nil || !ok {
		t.Fatal(err)
	}
	table := tbl.(*Table)
	strs := []any{"", "a", "abc", "Abc", "abcdefghij", "abcdefghijk", "日本語の文字列です。", "日本語の文字列です", "日本語の文字列です。とても長い", strings.Repeat("x", 191), strings.Repeat("x", 190), "\xff\xfe", int64(5), []byte("abc")}
	values := map[string][]any{
		"u": {int64(1), uint64(7), int8(3), int64(-1), uint64(1 << 63), "5", "abc", 1.5, int32(9)},
		"i": {int64(1), int32(-5), int64(1 << 40), "12", "x", 2.0, uint8(4)},
		"s": strs,
		"c": strs,
		"d": {time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), time.Date(2024, 1, 2, 3, 4, 5, 500, time.UTC), "2024-01-02 03:04:05", "2024-13-01", int64(5)},
		"b": {[]byte("ab"), []byte("abcd"), []byte("abcdefg"), "ab", "abcdef", int64(3)},
	}
	rng := rand.New(rand.NewPCG(102, 1))
	taken := 0
	seen := map[[2]bool]int{}
	for _, ki := range table.keyIndexes() {
		cols := make([]*gsql.Column, len(ki.ords))
		for i, ord := range ki.ords {
			cols[i] = table.sch.Schema[ord]
		}
		for n := 0; n < 1000; n++ {
			r := make(gsql.MySQLRange, len(cols))
			points := rng.IntN(len(cols) + 1)
			for i, c := range cols {
				if i < points {
					vs := values[strings.ToLower(c.Name)]
					v := vs[rng.IntN(len(vs))]
					r[i] = gsql.MySQLRangeColumnExpr{LowerBound: gsql.Below{Key: v}, UpperBound: gsql.Above{Key: v}, Typ: c.Type}
				} else {
					r[i] = gsql.MySQLRangeColumnExpr{LowerBound: gsql.BelowNull{}, UpperBound: gsql.AboveAll{}, Typ: c.Type}
				}
			}
			ranges := gsql.MySQLRangeCollection{r}
			pl, ok := pointLookup(s.ctx, ki, cols, ranges)
			// 受けないのは、点がないか、比べずに同じと分からない（小数など）か、列の型に変換できない点の値があるときだけ
			converts := true
			for i := range points {
				v := gsql.GetMySQLRangeCutKey(r[i].LowerBound)
				cv, ok := convertKey(s.ctx, cols[i].Type, v)
				if !ok || !sameKey(v, v) {
					converts = false
				} else if _, _, err := rowenc.AppendIndexValue(nil, ki.cols[i], cv); err != nil {
					converts = false
				}
			}
			if ok != (points > 0 && converts) {
				t.Fatalf("%s %v: pointLookup ok %v", ki.def.Name, r, ok)
			}
			if !ok {
				continue
			}
			taken++
			seen[[2]bool{pl.exact && (!ki.hasPrefix || pl.shortRanged), !ki.hasPrefix || pl.shortAll}]++
			old := mergeIntervals(ranges, func(r gsql.MySQLRange) (lo, hi []byte) {
				return indexInterval(s.ctx, cols, func(i int, v any) ([]byte, bool, error) {
					return rowenc.AppendIndexValue(nil, ki.cols[i], v)
				}, r)
			})
			oldExact := exactIntervals(s.ctx, ki.hasPrefix && !shortPrefixPoints(s.ctx, ki, ranges, true), cols, ranges)
			oldSorted := !ki.hasPrefix || shortPrefixPoints(s.ctx, ki, ranges, false)
			exact := pl.exact && (!ki.hasPrefix || pl.shortRanged)
			sorted := !ki.hasPrefix || pl.shortAll
			if len(old) != 1 || !bytes.Equal(old[0].lo, pl.iv.lo) || !bytes.Equal(old[0].hi, pl.iv.hi) || oldExact != exact || oldSorted != sorted {
				t.Errorf("%s %v:\n got %x..%x exact %v sorted %v\nwant %v exact %v sorted %v", ki.def.Name, r, pl.iv.lo, pl.iv.hi, exact, sorted, old, oldExact, oldSorted)
			}
		}
	}
	if taken < 2000 || len(seen) != 4 {
		t.Errorf("pointLookup took only %d ranges (exact, sorted: %v)", taken, seen)
	}
	// 受けない形（変換できない値のほか）: 点でない列、すべての値の後の点、両端の違う点、型の違う両端、照合順序で同じ重みの違う両端、NULL の点、範囲が 2 つ、範囲が列より短い
	ki, _ := table.findKeyIndex("us")
	cols := []*gsql.Column{table.sch.Schema[ki.ords[0]], table.sch.Schema[ki.ords[1]]}
	point := func(v any) gsql.MySQLRangeColumnExpr {
		return gsql.MySQLRangeColumnExpr{LowerBound: gsql.Below{Key: v}, UpperBound: gsql.Above{Key: v}, Typ: cols[0].Type}
	}
	all := gsql.MySQLRangeColumnExpr{LowerBound: gsql.BelowNull{}, UpperBound: gsql.AboveAll{}, Typ: cols[1].Type}
	for name, ranges := range map[string]gsql.MySQLRangeCollection{
		"range":           {{{LowerBound: gsql.Below{Key: int64(1)}, UpperBound: gsql.Above{Key: int64(5)}, Typ: cols[0].Type}, all}},
		"point after all": {{all, point("a")}},
		"different types": {{{LowerBound: gsql.Below{Key: int64(1)}, UpperBound: gsql.Above{Key: uint64(1)}, Typ: cols[0].Type}, all}},
		"same weights":    {{point(int64(1)), {LowerBound: gsql.Below{Key: "abc"}, UpperBound: gsql.Above{Key: "Abc"}, Typ: cols[1].Type}}},
		"null":            {{{LowerBound: gsql.BelowNull{}, UpperBound: gsql.AboveNull{}, Typ: cols[0].Type}, all}},
		"empty":           {{{LowerBound: gsql.AboveAll{}, UpperBound: gsql.AboveAll{}, Typ: cols[0].Type}, all}},
		"two ranges":      {{point(int64(1)), all}, {point(int64(2)), all}},
		"short":           {{point(int64(1))}},
	} {
		if _, ok := pointLookup(s.ctx, ki, cols[:len(ranges[0])], ranges); ok {
			t.Errorf("%s: pointLookup took %v", name, ranges)
		}
	}
	ki, _ = table.findKeyIndex("uic")
	cols = []*gsql.Column{table.sch.Schema[ki.ords[0]], table.sch.Schema[ki.ords[1]], table.sch.Schema[ki.ords[2]]}
	r := gsql.MySQLRange{point(int64(1)), {LowerBound: gsql.BelowNull{}, UpperBound: gsql.AboveAll{}, Typ: cols[1].Type}, {LowerBound: gsql.Below{Key: "a"}, UpperBound: gsql.Above{Key: "a"}, Typ: cols[2].Type}}
	if _, ok := pointLookup(s.ctx, ki, cols, gsql.MySQLRangeCollection{r}); ok {
		t.Errorf("point after all: pointLookup took %v", r)
	}
}

// meta_query の two_filters_t は、メタを (post_id, meta_key) と post_id で引くたびに pointLookup を通る（最初の lookup も、
// 2 回目からも。go-mysql-server は最初だけ左の値をそのまま、後は変換した値を範囲に置く）。
func TestStage102JoinTakesPointLookup(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), metaQueryDataN(2000)...) {
		mustExec(t, db, q)
	}
	for _, name := range []string{"standard indexes", "API indexes"} {
		if name == "API indexes" {
			mustExec(t, db, apiIndexes)
			mustExec(t, db, "ANALYZE TABLE wp_postmeta")
		}
		before := pointLookups.Load()
		found := one(t, db, fmt.Sprintf("SELECT COUNT(*) FROM (%s) x", strings.Replace(strings.Split(stage101Queries[0], " LIMIT ")[0], "SQL_CALC_FOUND_ROWS ", "", 1)))
		// category の 1 割（200 件）の記事ごとに featured を、そのうち 1/9 の記事ごとに event_date を引く
		if n := pointLookups.Load() - before; n < 200 {
			t.Errorf("%s: %d point lookups (%s rows)", name, n, found)
		}
	}
}
