package backend

import (
	gosql "database/sql"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 段階 125: 全件の走査とエントリだけで作る行で、復号した行を使い回す（docs/plan.md の段階 125）。使い回すのは
// projectIter が値を写す行だけで、行を持ち続ける側（並べ替え、GROUP BY、DISTINCT、結合、副問い合わせ、UNION、
// INSERT ... SELECT、UPDATE と DELETE、窓関数、PREPARE）の結果が、使い回さない場合と同じことを確かめる。

// stage125Table は、テストの表を返す。
func stage125Table(t *testing.T, s *server, name string) *Table {
	t.Helper()
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, ok, err := db.GetTableInsensitive(s.ctx, name)
	if err != nil || !ok {
		t.Fatalf("table %s: %v", name, err)
	}
	return tbl.(*Table)
}

// stage125Next は、it の行を最後まで読み、行と、行の配列の先頭の番地（使い回すと同じになる）を返す。値は読んだ
// その場で文字列にする（使い回す行は次の Next で書き換わる）。
func stage125Next(t *testing.T, ctx *sql.Context, it sql.RowIter) (rows []string, firsts []*any) {
	t.Helper()
	defer it.Close(ctx)
	for {
		row, err := it.Next(ctx)
		if err == io.EOF {
			return rows, firsts
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, fmt.Sprint(row))
		if len(row) > 0 {
			firsts = append(firsts, &row[0])
		}
	}
}

// stage125Shared は、子の行の配列が 1 つだけ（使い回している）なら真。
func stage125Shared(firsts []*any) bool {
	for _, p := range firsts[1:] {
		if p != firsts[0] {
			return false
		}
	}
	return len(firsts) > 1
}

// TestStage125ReuseOnlyUnderProjection は、projectIter の子にした全件の走査とエントリだけの読み出しだけが行を使い
// 回し、列を絞らない読み出し（行をそのまま返す）、ALTER の途中（NULL を埋める）、テスト用のスイッチでは使い回さない
// ことと、返す行（projectIter の行）は行ごとに別のことを確かめる。
func TestStage125ReuseOnlyUnderProjection(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE r (id INT PRIMARY KEY, a INT, b VARCHAR(20), c TEXT, KEY b_a (b, a))")
	s.mustQuery(t, "INSERT INTO r VALUES (1, 10, 'x', 'p'), (2, 20, 'y', 'q'), (3, 30, 'z', 'r')")
	tbl := stage125Table(t, s, "r")

	// 全件の走査を projectIter の子にすると、子の行は使い回し、projectIter の行は別々。
	pt := tbl.WithProjections([]string{"a", "b"}).(*Table)
	it, err := pt.PartitionRows(s.ctx, partition{})
	if err != nil {
		t.Fatal(err)
	}
	pi, ok := it.(*projectIter)
	if !ok {
		t.Fatalf("projected scan is %T, want *projectIter", it)
	}
	if sc, ok := pi.child.(*scanRowIter); !ok || !sc.reuse {
		t.Fatalf("child %T does not reuse rows", pi.child)
	}
	rows, firsts := stage125Next(t, s.ctx, pi.child)
	if got := strings.Join(rows, " "); got != "[1 10 x <nil>] [2 20 y <nil>] [3 30 z <nil>]" {
		t.Errorf("child rows %s", got)
	}
	if !stage125Shared(firsts) {
		t.Errorf("child rows are not reused")
	}
	it, err = pt.PartitionRows(s.ctx, partition{})
	if err != nil {
		t.Fatal(err)
	}
	rows, firsts = stage125Next(t, s.ctx, it)
	if got := strings.Join(rows, " "); got != "[10 x] [20 y] [30 z]" {
		t.Errorf("projected rows %s", got)
	}
	if firsts[0] == firsts[1] || firsts[1] == firsts[2] {
		t.Errorf("projected rows share an array")
	}

	// エントリだけで作る行（covered）も同じ。
	ki, ok := pt.findKeyIndex("b_a")
	if !ok || !pt.covers(ki) {
		t.Fatalf("b_a does not cover (a, b)")
	}
	pk, err := pt.newPKRowIter(s.ctx, ki, []interval{{}}, false, nil, pt.readColumns(nil))
	if err != nil {
		t.Fatal(err)
	}
	it, err = pt.projectRows(pk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := pk.(*pkRowIter); !ok || !p.reuse {
		t.Fatalf("covered reader %T does not reuse rows", pk)
	}
	if rows, _ = stage125Next(t, s.ctx, it); strings.Join(rows, " ") != "[10 x] [20 y] [30 z]" {
		t.Errorf("covered rows %s", rows)
	}
	pk, err = pt.newPKRowIter(s.ctx, ki, []interval{{}}, false, nil, pt.readColumns(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pt.projectRows(pk, nil); err != nil {
		t.Fatal(err)
	}
	if _, firsts = stage125Next(t, s.ctx, pk); !stage125Shared(firsts) {
		t.Errorf("covered rows are not reused")
	}

	// 列を絞らない読み出しは、行をそのまま返すので使い回さない。
	it, err = tbl.WithProjections(nil).(*Table).PartitionRows(s.ctx, partition{})
	if err != nil {
		t.Fatal(err)
	}
	if sc, ok := it.(*scanRowIter); !ok || sc.reuse {
		t.Fatalf("unprojected scan %T reuses rows", it)
	}
	if rows, firsts = stage125Next(t, s.ctx, it); stage125Shared(firsts) || firsts[0] == firsts[1] {
		t.Errorf("unprojected rows share an array: %s", rows)
	}

	// ALTER の途中（NULL を埋める）とテスト用のスイッチでは使い回さない。
	nf := *pt
	nf.nullFill = &nullFill{}
	sc, err := nf.newScanRowIter(s.ctx, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = nf.projectRows(sc, nil); err != nil || sc.reuse {
		t.Errorf("reuses rows while filling NULLs (%v)", err)
	}
	sc.Close(s.ctx)
	noRowReuse.Store(true)
	it, err = pt.PartitionRows(s.ctx, partition{})
	noRowReuse.Store(false)
	if err != nil {
		t.Fatal(err)
	}
	if sc := it.(*projectIter).child.(*scanRowIter); sc.reuse {
		t.Errorf("reuses rows with noRowReuse")
	}
	it.Close(s.ctx)

	// 集めて返す読み出し（rowIter）は使い回しの対象ではない。
	if _, ok := any(&rowIter{}).(rowReuser); ok {
		t.Errorf("rowIter is a rowReuser")
	}
}

// stage125Schema は、性質のテストの表。主キーのない表（見えない列）、プレフィックス長の列のインデックス（読み込んで
// 並べ替える）、FULLTEXT（全件を読んで集める）、エントリだけで読めるインデックス、照合順序の違う列、DECIMAL、日時、
// BLOB、JSON を含む。
var stage125Schema = []string{
	`CREATE TABLE t1 (id INT PRIMARY KEY, a INT, b VARCHAR(20) COLLATE utf8mb4_unicode_520_ci, c TEXT, d DECIMAL(10,2),
		e DATETIME, f BLOB, g JSON, KEY a (a), KEY b_a (b, a), KEY c5 (c(3)), FULLTEXT KEY ft (b))`,
	`CREATE TABLE t2 (id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT, a INT, b VARCHAR(20) COLLATE utf8mb4_bin, d DECIMAL(8,3),
		KEY a (a))`,
	`CREATE TABLE t3 (a INT, b VARCHAR(20), c TEXT)`,
	`CREATE TABLE t4 (id INT PRIMARY KEY, a INT, b VARCHAR(20) COLLATE utf8mb4_unicode_520_ci, d DECIMAL(10,2))`,
}

// stage125Data は、乱数 r で表の行を入れる文を作る。
func stage125Data(r *rand.Rand) []string {
	str := func() string {
		if r.Intn(8) == 0 {
			return "NULL"
		}
		words := []string{"a", "A", "a ", "b", "B", "ab", "Ab", "ba", "é", "e", "x y", "", "zz", "z"}
		return "'" + words[r.Intn(len(words))] + words[r.Intn(len(words))] + "'"
	}
	num := func(n int) string {
		if r.Intn(8) == 0 {
			return "NULL"
		}
		return fmt.Sprint(r.Intn(n))
	}
	var out []string
	var v []string
	for i := 1; i <= 150+r.Intn(150); i++ {
		v = append(v, fmt.Sprintf("(%d, %s, %s, %s, %s, '2026-0%d-1%d 0%d:00:00', %s, '{\"k\": %d}')", i, num(12), str(), str(),
			fmt.Sprintf("%d.%02d", r.Intn(100), r.Intn(100)), 1+r.Intn(9), r.Intn(10), r.Intn(10), str(), r.Intn(5)))
	}
	out = append(out, "INSERT INTO t1 VALUES "+strings.Join(v, ", "))
	v = v[:0]
	for i := 1; i <= 100+r.Intn(100); i++ {
		v = append(v, fmt.Sprintf("(%s, %s, %d.%03d)", num(12), str(), r.Intn(50), r.Intn(1000)))
	}
	out = append(out, "INSERT INTO t2 (a, b, d) VALUES "+strings.Join(v, ", "))
	v = v[:0]
	for i := 1; i <= 80+r.Intn(80); i++ {
		v = append(v, fmt.Sprintf("(%s, %s, %s)", num(12), str(), str()))
	}
	out = append(out, "INSERT INTO t3 VALUES "+strings.Join(v, ", "))
	return out
}

// stage125Queries は、行を持ち続ける側の文の形。%[1]d は乱数の整数、%[2]s は乱数の文字列。
var stage125Queries = []string{
	// 並べ替え、GROUP BY、DISTINCT、集計
	"SELECT a, b FROM t1 ORDER BY b, a, id",
	"SELECT b, c, d FROM t1 ORDER BY d DESC, id LIMIT %[1]d",
	"SELECT b, COUNT(*), SUM(d), MAX(e), MIN(c) FROM t1 GROUP BY b ORDER BY b",
	"SELECT a, GROUP_CONCAT(b ORDER BY id SEPARATOR '|') FROM t1 GROUP BY a ORDER BY a",
	"SELECT DISTINCT b FROM t1 ORDER BY b",
	"SELECT DISTINCT a, b FROM t2 ORDER BY a, b",
	"SELECT COUNT(DISTINCT b), COUNT(*) FROM t1 WHERE a < %[1]d",
	"SELECT YEAR(e), MONTH(e), COUNT(*) FROM t1 GROUP BY YEAR(e), MONTH(e) ORDER BY 1 DESC, 2 DESC",
	"SELECT b, d FROM t1 WHERE b = %[2]s ORDER BY id",
	"SELECT a, b FROM t1 WHERE b > %[2]s ORDER BY b, a",
	"SELECT id, c FROM t1 WHERE c LIKE 'a%%' ORDER BY c, id",
	"SELECT id, c FROM t1 WHERE c >= %[2]s ORDER BY c, id",
	"SELECT f, g FROM t1 ORDER BY id DESC LIMIT 20",
	"SELECT * FROM t1 ORDER BY b, id",
	"SELECT * FROM t3 ORDER BY c, a, b",
	"SELECT a, c FROM t3 ORDER BY c, a",
	"SELECT b, COUNT(*) FROM t3 GROUP BY b ORDER BY b",
	// 結合（ハッシュ、lookup、マージ、入れ子）
	"SELECT t1.id, t2.id, t2.b FROM t1 JOIN t2 ON t1.a = t2.a WHERE t1.id < %[1]d ORDER BY t1.id, t2.id",
	"SELECT /*+ HASH_JOIN(t1, t2) */ t1.b, t2.b FROM t1 JOIN t2 ON t1.b = t2.b ORDER BY t1.b, t2.b, t1.id, t2.id",
	"SELECT /*+ MERGE_JOIN(t1, t2) */ t1.id, t2.d FROM t1 JOIN t2 ON t1.id = t2.id ORDER BY t1.id",
	"SELECT /*+ LOOKUP_JOIN(t2, t1) */ t2.id, t1.c FROM t2 JOIN t1 ON t1.id = t2.a ORDER BY t2.id",
	"SELECT t1.id, t3.b FROM t1 LEFT JOIN t3 ON t3.a = t1.a AND t3.b = t1.b ORDER BY t1.id, t3.b",
	"SELECT x.b, y.b FROM t3 x JOIN t3 y ON x.a = y.a AND x.c < y.c ORDER BY x.b, y.b, x.c, y.c",
	"SELECT t1.a, t2.a FROM t1, t2 WHERE t1.id < 5 AND t2.id < 5 ORDER BY 1, 2",
	// 副問い合わせ（IN、EXISTS、スカラー、キャッシュされるもの、派生表）
	"SELECT id, b FROM t1 WHERE a IN (SELECT a FROM t2 WHERE d > %[1]d) ORDER BY id",
	"SELECT id FROM t1 WHERE b NOT IN (SELECT b FROM t2 WHERE b IS NOT NULL) ORDER BY id",
	"SELECT id FROM t1 WHERE EXISTS (SELECT 1 FROM t3 WHERE t3.a = t1.a AND t3.b = t1.b) ORDER BY id",
	"SELECT id, (SELECT MAX(d) FROM t2 WHERE t2.a = t1.a) FROM t1 ORDER BY id",
	"SELECT id, (SELECT b FROM t2 ORDER BY id LIMIT 1) FROM t1 WHERE id < %[1]d ORDER BY id",
	"SELECT s.b, s.n FROM (SELECT b, COUNT(*) n FROM t1 GROUP BY b) s ORDER BY s.n DESC, s.b",
	"SELECT * FROM (SELECT a, b FROM t1 ORDER BY b LIMIT 30) s ORDER BY a, b",
	"WITH w AS (SELECT a, b FROM t2) SELECT w.a, COUNT(*) FROM w JOIN t1 ON t1.a = w.a GROUP BY w.a ORDER BY w.a",
	"SELECT id FROM t1 WHERE a = (SELECT MIN(a) FROM t3) ORDER BY id",
	// UNION
	"SELECT b FROM t1 UNION SELECT b FROM t2 UNION SELECT b FROM t3 ORDER BY 1",
	"SELECT a, b FROM t1 UNION ALL SELECT a, b FROM t2 ORDER BY 1, 2",
	"(SELECT b FROM t1 ORDER BY id LIMIT 10) UNION (SELECT c FROM t3 ORDER BY c LIMIT 10) ORDER BY 1",
	// 窓関数
	"SELECT id, a, ROW_NUMBER() OVER (PARTITION BY a ORDER BY id), SUM(d) OVER (PARTITION BY a) FROM t1 ORDER BY id",
	"SELECT b, RANK() OVER (ORDER BY d DESC) FROM t2 ORDER BY id",
	"SELECT id, LAG(b) OVER (ORDER BY id), LEAD(c) OVER (ORDER BY id) FROM t1 ORDER BY id",
}

// stage125Writes は、行を書く文（同じ表を読みながら書くもの）。
var stage125Writes = []string{
	"INSERT IGNORE INTO t4 SELECT id, a, b, d FROM t1 WHERE a < %[1]d",
	"INSERT INTO t3 SELECT a, b, c FROM t3 WHERE a = %[1]d",
	"INSERT INTO t2 (a, b, d) SELECT a, b, d FROM t2 WHERE a > %[1]d ORDER BY id",
	"UPDATE t1 SET a = a + 1, b = CONCAT(b, 'u') WHERE c > %[2]s",
	"UPDATE t1 SET d = (SELECT MAX(d) FROM t2 WHERE t2.a = t1.a) WHERE id %% 7 = %[1]d %% 7",
	"UPDATE t3 SET c = CONCAT(c, b) WHERE a < %[1]d",
	"DELETE FROM t2 WHERE b = %[2]s",
	"DELETE FROM t3 WHERE a = %[1]d",
	"DELETE FROM t4 WHERE id IN (SELECT id FROM t1 WHERE a = %[1]d)",
	"UPDATE t4 JOIN t1 ON t1.id = t4.id SET t4.b = t1.c WHERE t1.a > %[1]d",
	"REPLACE INTO t4 SELECT id, a + 100, c, d FROM t1 WHERE id < %[1]d",
}

// stage125Script は、乱数 r で文の列を作る（読む文、書く文、PREPARE と EXECUTE、各表の全体の中身）。
func stage125Script(r *rand.Rand) []string {
	strs := []string{"'a'", "'A'", "'ab'", "'b'", "'é'", "'e'", "'z'", "''", "'x y'", "'ab '"}
	arg := func(f string) string {
		return strings.NewReplacer("%[1]d", fmt.Sprint(r.Intn(12)), "%[2]s", strs[r.Intn(len(strs))], "%%", "%").Replace(f)
	}
	out := stage125Data(r)
	all := []string{"SELECT * FROM t1 ORDER BY id", "SELECT * FROM t2 ORDER BY id", "SELECT * FROM t3 ORDER BY a, b, c", "SELECT * FROM t4 ORDER BY id"}
	for range 4 {
		for range 30 {
			out = append(out, arg(stage125Queries[r.Intn(len(stage125Queries))]))
		}
		for range 3 {
			out = append(out, arg(stage125Writes[r.Intn(len(stage125Writes))]))
		}
		out = append(out, all...)
	}
	for _, q := range stage125Queries {
		out = append(out, arg(q))
	}
	out = append(out,
		"PREPARE p1 FROM 'SELECT a, b FROM t1 WHERE a < ? ORDER BY b, id'", "SET @x = 6", "EXECUTE p1 USING @x", "SET @x = 3", "EXECUTE p1 USING @x",
		"PREPARE p2 FROM 'SELECT b, COUNT(*) FROM t2 WHERE a > ? GROUP BY b ORDER BY b'", "EXECUTE p2 USING @x", "EXECUTE p2 USING @x",
		"DEALLOCATE PREPARE p1", "DEALLOCATE PREPARE p2")
	return out
}

// stage125Run は、新しい店で文の列を流し、文ごとの結果（エラーを含む）を返す。
func stage125Run(t *testing.T, script []string, reuse bool) []string {
	t.Helper()
	noRowReuse.Store(!reuse)
	defer noRowReuse.Store(false)
	s := start(t, t.TempDir())
	defer s.stop(t)
	for _, q := range stage125Schema {
		s.mustQuery(t, q)
	}
	out := make([]string, len(script))
	for i, q := range script {
		rows, err := query(s.ctx, s.engine, q)
		if err != nil {
			out[i] = "error: " + err.Error()
			continue
		}
		out[i] = fmt.Sprint(rows)
	}
	return out
}

// TestStage125ReuseProperty は、乱数で作った表と文で、行を使い回す場合と使い回さない場合の結果が同じことを確かめる。
func TestStage125ReuseProperty(t *testing.T) {
	seeds := []int64{1, 2, 3, 4, 5, 6}
	if testing.Short() {
		seeds = seeds[:2]
	}
	for _, seed := range seeds {
		script := stage125Script(rand.New(rand.NewSource(seed)))
		off := stage125Run(t, script, false)
		on := stage125Run(t, script, true)
		errs, diffs := 0, 0
		for i := range script {
			if strings.HasPrefix(off[i], "error: ") {
				errs++
				if errs <= 12 {
					t.Logf("seed %d: %s: %.300s", seed, script[i], off[i])
				}
			}
			if on[i] != off[i] {
				diffs++
				if diffs <= 5 {
					t.Errorf("seed %d: %s\n reuse: %.600s\n   off: %.600s", seed, script[i], on[i], off[i])
				}
			}
		}
		if diffs > 0 {
			t.Errorf("seed %d: %d of %d statements differ", seed, diffs, len(script))
		}
		// 文の大半が通ること（形の誤りで、何も比べていないテストにならないように）
		if errs > len(script)/10 {
			t.Errorf("seed %d: %d of %d statements failed", seed, errs, len(script))
		}
	}
}

// TestStage125ConcurrentScans は、8 つの接続が全件を読んで並べ替え、まとめ、結合する間に、ほかの接続が同じ行を
// 書き換えても、読んだ行の列がほかの行の値と混ざらないことを確かめる（-race で、使い回す行を共有しないことも見る）。
// 書く文は a と b を同じ値から作るので、どの時点の行でも b = CONCAT('x', a)。a は行ごとに違う。
func TestStage125ConcurrentScans(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	db := openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "CREATE TABLE c (id INT PRIMARY KEY, a INT, b VARCHAR(20), pad TEXT, KEY ab (a, b))")
	mustExec(t, db, `INSERT INTO c WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 400)
		SELECT i, i, CONCAT('x', i), REPEAT('p', i % 50) FROM n`)
	const n = 400
	pool, err := gosql.Open("mysql", fmt.Sprintf("root@tcp(%s)/%s", addr, testDB))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(12)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	fail := func(f string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(f, args...))
		}
	}
	for w := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := 1 + r.Intn(n)
				if _, err := pool.Exec("UPDATE c SET b = CONCAT('x', a + 1000), a = a + 1000 WHERE id = ?", id); err != nil {
					fail("update: %v", err)
					return
				}
			}
		}()
	}
	queries := []string{
		"SELECT id, a, b FROM c ORDER BY b DESC, id",
		"SELECT a, b FROM c GROUP BY a, b ORDER BY a",
		"SELECT DISTINCT a, b FROM c",
		"SELECT x.a, x.b FROM c x JOIN c y ON y.id = x.id ORDER BY x.id",
		"SELECT a, b FROM c WHERE id IN (SELECT id FROM c WHERE pad <> 'zz') ORDER BY a",
		"SELECT a, b FROM c WHERE a > ? ORDER BY b",
	}
	var rwg sync.WaitGroup
	for k := range 8 {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for i := range 25 {
				q := queries[(k+i)%len(queries)]
				var rows *gosql.Rows
				var err error
				if strings.Contains(q, "?") {
					rows, err = pool.Query(q, i) // 準備した文（バイナリのプロトコル）
				} else {
					rows, err = pool.Query(q)
				}
				if err != nil {
					fail("%s: %v", q, err)
					return
				}
				got := 0
				seen := map[int64]bool{}
				for rows.Next() {
					var a gosql.NullInt64
					var b gosql.NullString
					var dst []any
					if cols, _ := rows.Columns(); len(cols) == 3 {
						var id int
						dst = []any{&id, &a, &b}
					} else {
						dst = []any{&a, &b}
					}
					if err := rows.Scan(dst...); err != nil {
						fail("%s: %v", q, err)
						break
					}
					if b.String != fmt.Sprintf("x%d", a.Int64) {
						fail("%s: a %d and b %s from different rows", q, a.Int64, b.String)
					}
					got++
					seen[a.Int64] = true
				}
				if err := rows.Err(); err != nil {
					fail("%s: %v", q, err)
				}
				rows.Close()
				// a は行ごとに違う（id から 1000 ずつ進める）ので、同じ行が並べば数が減る
				if !strings.Contains(q, "WHERE") && (got != n || len(seen) != n) {
					fail("%s: %d rows, %d distinct", q, got, len(seen))
				}
			}
		}()
	}
	rwg.Wait()
	close(stop)
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
}
