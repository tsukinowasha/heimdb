package backend

import (
	"fmt"
	"strings"
	"testing"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/iters"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// ハッシュ結合の右側に GROUP BY の副問い合わせを置いた文を流しても、途中結果のキャッシュが MemoryManager に残らない
// （docs/plan.md の段階 108。ハッシュの索引を作る iterator が子を閉じず、文ごとに GROUP BY のキャッシュが残っていた）。
func TestHashLookupChildClosed(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE p (ID BIGINT PRIMARY KEY, comment_count BIGINT)")
	s.mustQuery(t, "CREATE TABLE c (id BIGINT PRIMARY KEY, post BIGINT, KEY post (post))")
	s.mustQuery(t, "INSERT INTO p VALUES (1, 2), (2, 0), (3, 5)")
	s.mustQuery(t, "INSERT INTO c VALUES (1, 1), (2, 1), (3, 3)")

	q := "SELECT p.ID FROM p LEFT JOIN (SELECT post, COUNT(*) AS n FROM c GROUP BY post) x ON x.post = p.ID " +
		"WHERE p.comment_count <> COALESCE(x.n, 0)"
	if plan := rowsString(t, s, "EXPLAIN PLAN "+q); !strings.Contains(plan, "HashLookup") {
		t.Fatalf("not a hash join:\n%s", plan)
	}
	for _, sfx := range []string{"", " LIMIT 1"} {
		for i := 0; i < 3; i++ {
			if got := rowsString(t, s, q+sfx); got != "[3]\n" {
				t.Fatalf("%s: got %q", q+sfx, got)
			}
		}
		if n := s.ctx.Memory.NumCaches(); n != 0 {
			t.Errorf("%q: %d caches left", sfx, n)
		}
	}
}

// TCP のサーバでも、文が返った後にエンジンの MemoryManager のキャッシュが 0 に戻る（docs/plan.md の段階 109）。
// 書き込みが途中で重複のエラーになる文も含める（サブクエリのある INSERT ... SELECT が重複で止まると、文の計画の
// 片付けが走らず、サブクエリのキャッシュが残っていた）。数は stopServer でも確かめる。
func TestEngineCachesAfterStatements(t *testing.T) {
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{})
	defer stopServer(t, st, srv)()
	dsn := fmt.Sprintf("root@tcp(%s)/%s", addr, testDB)
	runScriptErrors(t, dsn, []string{
		"CREATE TABLE p (ID BIGINT PRIMARY KEY, comment_count BIGINT)",
		"CREATE TABLE c (id BIGINT PRIMARY KEY, post BIGINT, KEY post (post))",
		"CREATE TABLE u (id BIGINT PRIMARY KEY)",
		"CREATE TABLE w (id BIGINT PRIMARY KEY)",
		"INSERT INTO p VALUES (1, 2), (2, 0), (3, 5)",
		"INSERT INTO c VALUES (1, 1), (2, 1), (3, 3)",
		"INSERT INTO u VALUES (1)",
		"CREATE TABLE n (id BIGINT PRIMARY KEY, v BIGINT)",
		"INSERT INTO n VALUES (1, NULL), (2, 2)",
	})
	for _, q := range []string{
		"SELECT p.ID FROM p LEFT JOIN (SELECT post, COUNT(*) AS n FROM c GROUP BY post) x ON x.post = p.ID WHERE p.comment_count <> COALESCE(x.n, 0)",
		"SELECT DISTINCT post FROM c ORDER BY post LIMIT 1",
		"SELECT post FROM c UNION SELECT ID FROM p",
		"SELECT COUNT(DISTINCT post) FROM c",
		"INSERT INTO u SELECT DISTINCT post FROM c",
		"INSERT INTO u SELECT p.ID FROM p LEFT JOIN (SELECT post, COUNT(*) AS n FROM c GROUP BY post) x ON x.post = p.ID",
		"UPDATE p JOIN (SELECT post, COUNT(*) AS n FROM c GROUP BY post) x ON x.post = p.ID SET p.ID = 3",
		"INSERT INTO w SELECT ID FROM p WHERE comment_count IN (SELECT COUNT(*) FROM c GROUP BY post)",
		"INSERT INTO u SELECT ID FROM p WHERE comment_count IN (SELECT COUNT(*) FROM c GROUP BY post)",
		"INSERT INTO u SELECT ID FROM p WHERE ID IN (SELECT post FROM c WHERE post > 0 UNION SELECT 1)",
		"UPDATE p SET ID = 3 WHERE comment_count > (SELECT MIN(n) FROM (SELECT COUNT(*) AS n FROM c GROUP BY post) x)",
		"UPDATE p SET ID = (SELECT MAX(post) FROM c) WHERE ID = 1",
		"INSERT INTO u VALUES ((SELECT MIN(post) FROM c))",
		// 窓関数の iterator は子（GROUP BY）を閉じる（段階 111、上流の #3810）
		"SELECT post, n, ROW_NUMBER() OVER (ORDER BY post) FROM (SELECT post, COUNT(*) AS n FROM c GROUP BY post) s",
		// 反結合は、条件が NULL の左の行でも右（GROUP BY）を閉じる（段階 111、上流の #3428）
		"SELECT ID FROM p WHERE ID NOT IN (SELECT v FROM n GROUP BY v)",
		"SELECT ID FROM p WHERE NOT EXISTS (SELECT 1 FROM (SELECT v FROM n GROUP BY v) g WHERE g.v = p.ID)",
	} {
		runScriptErrors(t, dsn, []string{q})
		if n := srv.Engine.MemoryManager.NumCaches(); n != 0 {
			t.Fatalf("%d engine caches left after: %s", n, q)
		}
	}
	// 重複で止まった文の行は入らない（片付けで、止まった文の書き込みをコミットしない）
	if got := runScript(t, dsn, []string{"SELECT id FROM u ORDER BY id"})[0]; got != "1\n" {
		t.Errorf("u after the failed statements: %q", got)
	}
}

// MemoryManager は、片付けが 2 回呼ばれても、後から作ったキャッシュを一覧から消さない（docs/plan.md の段階 109。
// 一覧が空になると番号を 0 に戻していたので、古い片付けが同じ番号の新しいキャッシュを消していた）。
func TestMemoryManagerStaleDispose(t *testing.T) {
	m := gsql.NewMemoryManager(nil)
	_, first := m.NewHistoryCache()
	first()
	_, second := m.NewHistoryCache()
	first()
	if n := m.NumCaches(); n != 1 {
		t.Errorf("%d caches after a stale dispose, want 1", n)
	}
	second()
	if n := m.NumCaches(); n != 0 {
		t.Errorf("%d caches left", n)
	}
}

// DISTINCT は、読み終えたときと閉じたときの 2 回、キャッシュを片付けない（docs/plan.md の段階 109）。
func TestDistinctDisposeOnce(t *testing.T) {
	ctx := gsql.NewEmptyContext()
	di := iters.NewDistinctIter(ctx, gsql.RowsToRowIter(gsql.Row{int64(1)}, gsql.Row{int64(1)}), []gsql.Type{types.Int64})
	inner, calls := di.DisposeFunc, 0
	di.DisposeFunc = func() { calls++; inner() }
	if _, err := gsql.RowIterToRows(ctx, di); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("disposed %d times, want 1", calls)
	}
}
