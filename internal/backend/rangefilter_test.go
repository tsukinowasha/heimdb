package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// インデックスの範囲がそのまま表す条件は、Filter から外す（dropRangeFilters。docs/plan.md の段階 50）。件数は、
// 入れた行から表の照合順序で数えたものと同じ。条件を残すべき形では Filter が残る。
func TestDropRangeFilters(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE p (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, type varchar(20) NOT NULL,
		status varchar(20) NOT NULL, d datetime NOT NULL, tt bigint unsigned NOT NULL, name varchar(200) NOT NULL,
		KEY ts (type, status, d, id), KEY tt (tt), KEY name (name(3)))`)
	type row struct {
		typ, status, name string
		tt                int
	}
	var rows []row
	var vals []string
	types := []string{"post", "page", "POST", "attachment"}
	statuses := []string{"publish", "draft", "Publish", "publish ", "private"}
	for i := 0; i < 200; i++ {
		r := row{types[i%len(types)], statuses[i%len(statuses)], fmt.Sprintf("name%03d", i), i % 7}
		rows = append(rows, r)
		vals = append(vals, fmt.Sprintf("('%s', '%s', '2024-01-01 00:00:%02d', %d, '%s')", r.typ, r.status, i%60, r.tt, r.name))
	}
	exec("INSERT INTO p (type, status, d, tt, name) VALUES " + strings.Join(vals, ", "))
	exec("ANALYZE TABLE p")

	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	// 表の照合順序（utf8mb4_0900_ai_ci）は、大文字と小文字を区別せず、末尾の空白も比べる（NO PAD）。
	eq := strings.EqualFold
	expect := func(f func(r row) bool) int {
		n := 0
		for _, r := range rows {
			if f(r) {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		where   string
		want    func(r row) bool
		dropped bool // Filter がなくなるか
	}{
		// カテゴリの term 数と同じ形。大文字と小文字だけ違う値も、照合順序で等しいので数える。
		{"type IN ('post') AND status IN ('publish')", func(r row) bool { return eq(r.typ, "post") && eq(r.status, "publish") }, true},
		{"type = 'post' AND status = 'publish'", func(r row) bool { return eq(r.typ, "post") && eq(r.status, "publish") }, true},
		{"type IN ('post', 'page') AND status = 'draft'", func(r row) bool { return (eq(r.typ, "post") || eq(r.typ, "page")) && eq(r.status, "draft") }, true},
		{"tt = 3", func(r row) bool { return r.tt == 3 }, true},
		{"tt IN (1, 5)", func(r row) bool { return r.tt == 1 || r.tt == 5 }, true},
		// 範囲が点でない列の条件、数との比較、照合順序の付いた定数、プレフィックス長の列は残す。
		{"type = 'post' AND status > 'p'", func(r row) bool { return eq(r.typ, "post") && strings.ToLower(r.status) > "p" }, false},
		{"tt = '3'", func(r row) bool { return r.tt == 3 }, false},
		{"type = 'post' AND status = 'publish' COLLATE utf8mb4_bin", func(r row) bool { return eq(r.typ, "post") && r.status == "publish" }, false},
		{"name = 'name010'", func(r row) bool { return r.name == "name010" }, true}, // プレフィックス長のある列も外す（段階 83。lookupRows が範囲の条件を本体の値で掛け直す）
	} {
		q := "SELECT COUNT(*) FROM p WHERE " + tc.where
		if got, want := count(q), expect(tc.want); got != want || want == 0 {
			t.Errorf("%s: %d rows, want %d (non-zero)", tc.where, got, want)
		}
		plan := explain(t, db, q)
		if !strings.Contains(plan, "IndexedTableAccess") {
			t.Errorf("%s: no index access:\n%s", tc.where, plan)
			continue
		}
		if hasFilter := strings.Contains(plan, "Filter"); hasFilter == tc.dropped {
			t.Errorf("%s: Filter in the plan = %v, want %v:\n%s", tc.where, hasFilter, !tc.dropped, plan)
		}
	}
	// JOIN の中（カテゴリの term 数）。
	exec("CREATE TABLE r (object_id bigint unsigned NOT NULL, term_taxonomy_id bigint unsigned NOT NULL, PRIMARY KEY (object_id, term_taxonomy_id), KEY term_taxonomy_id (term_taxonomy_id))")
	exec("INSERT INTO r SELECT id, tt FROM p")
	q := "SELECT COUNT(*) FROM r, p WHERE p.id = r.object_id AND status IN ('publish') AND type IN ('post') AND term_taxonomy_id = 3"
	want := expect(func(r row) bool { return eq(r.typ, "post") && eq(r.status, "publish") && r.tt == 3 })
	if got := count(q); got != want || want == 0 {
		t.Errorf("join: %d rows, want %d (non-zero)", got, want)
	}
}

// 範囲の点が項の定数のどれとも違うとき、項より前の列が点でないときは、項を外さない（今の計画では起きないが、
// 範囲の作り方が変わっても結果を変えないための確かめ）。
func TestCoveredByRangesGuards(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE g (id bigint NOT NULL PRIMARY KEY, type varchar(20) NOT NULL, status varchar(20) NOT NULL, KEY ts (type, status))")
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, ok, err := db.GetTableInsensitive(s.ctx, "g")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	table := tbl.(*Table)
	var idx *Index
	idxs, _ := table.GetIndexes(s.ctx)
	for _, i := range idxs {
		if i.ID() == "ts" {
			idx = i.(*Index)
		}
	}
	vt := table.sch.Schema[1].Type
	point := func(v string) gsql.MySQLRangeColumnExpr { return gsql.ClosedRangeColumnExpr(v, v, vt) }
	cond := func(col, v string) gsql.Expression {
		return expression.NewEquals(expression.NewGetFieldWithTable(0, 0, vt, "", "g", col, false), expression.NewLiteral(v, types.LongText))
	}
	for _, tc := range []struct {
		name   string
		ranges gsql.MySQLRangeCollection
		c      gsql.Expression
		want   bool
	}{
		{"same point", gsql.MySQLRangeCollection{{point("post"), point("publish")}}, cond("status", "publish"), true},
		{"other point", gsql.MySQLRangeCollection{{point("post"), point("draft")}}, cond("status", "publish"), false},
		{"range before", gsql.MySQLRangeCollection{{gsql.ClosedRangeColumnExpr("a", "q", vt), point("publish")}}, cond("status", "publish"), false},
		{"first column", gsql.MySQLRangeCollection{{point("post"), gsql.AllRangeColumnExpr(vt)}}, cond("type", "post"), true},
	} {
		if got := coveredByRanges(s.ctx, table, idx, "g", tc.ranges, tc.c); got != tc.want {
			t.Errorf("%s: covered = %v, want %v", tc.name, got, tc.want)
		}
	}
}
