package backend

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// smallRewrites は、書き直しのコミットと読み出しの単位を小さくする（行ごとにコミットし、2 行ずつ読む）。
func smallRewrites(t *testing.T) {
	chunk, page := rewriteChunkBytes, rewriteScanPage
	rewriteChunkBytes, rewriteScanPage = 1, 2
	t.Cleanup(func() { rewriteChunkBytes, rewriteScanPage = chunk, page })
}

// 書き直しを行ごとにコミットし、2 行ずつ読んでも、列の変更の結果は本物の MySQL と同じ（docs/plan.md の段階 53）。
func TestAlterMatchesMySQLInSmallChunks(t *testing.T) {
	smallRewrites(t)
	TestAlterMatchesMySQL(t)
}

// 何回にも分けてコミットした書き直しで、行とインデックスが書き直しの前と同じに読め、書きかけの印が残らない。
func TestChunkedRewriteKeepsRowsAndIndexes(t *testing.T) {
	for _, small := range []bool{false, true} {
		t.Run(fmt.Sprintf("small=%v", small), func(t *testing.T) {
			if small {
				smallRewrites(t)
				rewriteChunkBytes = 500
				rewriteScanPage = 7
			}
			s := start(t, t.TempDir())
			defer s.stop(t)
			s.mustQuery(t, "CREATE TABLE r (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, a varchar(20) NOT NULL, b int, u int, KEY a (a), UNIQUE KEY u (u))")
			var vals []string
			for i := 1; i <= 300; i++ {
				vals = append(vals, fmt.Sprintf("('k%02d', %d, %d)", i%37, i%11, i))
			}
			s.mustQuery(t, "INSERT INTO r (a, b, u) VALUES "+strings.Join(vals, ", "))
			read := func() string {
				var b strings.Builder
				for _, q := range []string{
					"SELECT id, a, b, u FROM r ORDER BY id",
					"SELECT a, COUNT(*) FROM r WHERE a >= 'k' GROUP BY a ORDER BY a",
					"SELECT id FROM r WHERE u BETWEEN 100 AND 110 ORDER BY u",
				} {
					fmt.Fprintf(&b, "%v\n", s.mustQuery(t, q))
				}
				return b.String()
			}
			before := read()
			for _, q := range []string{
				"ALTER TABLE r ADD COLUMN c tinyint NULL",
				"ALTER TABLE r MODIFY b bigint",
				"ALTER TABLE r DROP COLUMN c",
			} {
				s.mustQuery(t, q)
				if got := read(); got != before {
					t.Fatalf("after %s:\n%s\nwant:\n%s", q, got, before)
				}
			}
			// AUTO_INCREMENT は書き直しの後も続く。
			s.mustQuery(t, "INSERT INTO r (a, b, u) VALUES ('new', 1, 1000)")
			if got := s.mustQuery(t, "SELECT id FROM r WHERE u = 1000"); fmt.Sprint(got) != "[[301]]" {
				t.Errorf("id after the rewrites: %v", got)
			}
			if n, err := s.st.PendingRewrites(); err != nil || n != 0 {
				t.Errorf("pending rewrites: %d, %v", n, err)
			}
		})
	}
}

// 主キーや一意インデックスの列を変える書き直しで重複が出たら、重複のエラー（MySQL と同じ 1062）にし、表を変えず、書きかけを
// 消す。重複する行が、同じコミットの単位にある場合と、前にコミットした単位にある場合の両方。
func TestChunkedRewriteDuplicates(t *testing.T) {
	for _, small := range []bool{false, true} {
		t.Run(fmt.Sprintf("small=%v", small), func(t *testing.T) {
			if small {
				smallRewrites(t)
			}
			s := start(t, t.TempDir())
			defer s.stop(t)
			for _, tc := range []struct {
				create, insert, alter, want string
			}{
				// 主キーの照合順序を、大文字と小文字を区別しないものにする
				{"CREATE TABLE p (id varchar(10) COLLATE utf8mb4_bin NOT NULL PRIMARY KEY, v int)",
					"INSERT INTO p VALUES ('A', 1), ('b', 2), ('c', 3), ('a', 4)",
					"ALTER TABLE p MODIFY id varchar(10) COLLATE utf8mb4_general_ci NOT NULL", "duplicate"},
				// 一意インデックスの列の照合順序
				{"CREATE TABLE q (id int NOT NULL PRIMARY KEY, k varchar(10) COLLATE utf8mb4_bin, UNIQUE KEY k (k))",
					"INSERT INTO q VALUES (1, 'x'), (2, NULL), (3, 'y'), (4, NULL), (5, 'X')",
					"ALTER TABLE q MODIFY k varchar(10) COLLATE utf8mb4_general_ci", "duplicate"},
				// 複数の列の一意インデックスから列を消す
				{"CREATE TABLE w (id int NOT NULL PRIMARY KEY, a varchar(10), b int, UNIQUE KEY ab (a, b))",
					"INSERT INTO w VALUES (1, 'p', 1), (2, 'q', 1), (3, 'p', 2)",
					"ALTER TABLE w DROP COLUMN b", "duplicate"},
			} {
				s.mustQuery(t, tc.create)
				s.mustQuery(t, tc.insert)
				name := strings.Fields(tc.create)[2]
				before := fmt.Sprint(s.mustQuery(t, "SHOW CREATE TABLE "+name), s.mustQuery(t, "SELECT * FROM "+name+" ORDER BY id"))
				if _, err := query(s.ctx, s.engine, tc.alter); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: %v, want %q", tc.alter, err, tc.want)
				}
				if after := fmt.Sprint(s.mustQuery(t, "SHOW CREATE TABLE "+name), s.mustQuery(t, "SELECT * FROM "+name+" ORDER BY id")); after != before {
					t.Errorf("%s changed the table:\n%s\nwant:\n%s", tc.alter, after, before)
				}
			}
			if n, err := s.st.PendingRewrites(); err != nil || n != 0 {
				t.Errorf("pending rewrites after the failed statements: %d, %v", n, err)
			}
			// 重複のない書き直しは通る。
			s.mustQuery(t, "DELETE FROM p WHERE id = 'a'")
			s.mustQuery(t, "ALTER TABLE p MODIFY id varchar(10) COLLATE utf8mb4_general_ci NOT NULL")
			if got := fmt.Sprint(s.mustQuery(t, "SELECT v FROM p WHERE id = 'B'")); got != "[[2]]" {
				t.Errorf("after the rewrite: %s", got)
			}
		})
	}
}

// indexEntries は、表 name のインデックス ID indexID のエントリの数（確定済みの最新）。
func indexEntries(t *testing.T, s *server, name string, indexID uint64) int {
	t.Helper()
	def, ok, err := s.st.GetTable(store.MainBranchID, name, store.Latest)
	if err != nil || !ok {
		t.Fatalf("table %s: %v, %v", name, ok, err)
	}
	n := 0
	if err := s.st.ScanIndexAsOf(store.MainBranchID, def.ID, indexID, store.Latest-1, nil, nil, false, func(_, _ []byte) (bool, error) {
		n++
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// CREATE INDEX を何回にも分けてコミットしても、インデックスで同じ行が読める。NULL の重なる一意インデックスは作れる。
// 前に途中で終わった作成のエントリは、作り直すときに消す（docs/plan.md の段階 53）。
func TestChunkedCreateIndex(t *testing.T) {
	for _, small := range []bool{false, true} {
		t.Run(fmt.Sprintf("small=%v", small), func(t *testing.T) {
			if small {
				smallRewrites(t)
			}
			s := start(t, t.TempDir())
			defer s.stop(t)
			s.mustQuery(t, "CREATE TABLE c (id int NOT NULL PRIMARY KEY, a varchar(20) COLLATE utf8mb4_bin, b int)")
			var vals []string
			for i := 1; i <= 200; i++ {
				a := fmt.Sprintf("'v%03d'", i)
				if i%10 == 0 {
					a = "NULL"
				}
				vals = append(vals, fmt.Sprintf("(%d, %s, %d)", i, a, i%7))
			}
			// 大文字と小文字だけ違う値（utf8mb4_bin では重複しない）
			vals = append(vals, "(201, 'V005', 0)")
			s.mustQuery(t, "INSERT INTO c VALUES "+strings.Join(vals, ", "))
			def, _, err := s.st.GetTable(store.MainBranchID, "c", store.Latest)
			if err != nil {
				t.Fatal(err)
			}
			next := def.NextIndexID
			// 前に途中で終わった作成のエントリ（作り直しで消える）
			if err := s.st.Update(func(tx *store.WriteTx) error {
				return tx.PutIndexEntry(store.MainBranchID, def.ID, next, []byte("stale"), nil)
			}); err != nil {
				t.Fatal(err)
			}
			s.mustQuery(t, "CREATE INDEX b ON c (b)")
			if n := indexEntries(t, s, "c", next); n != 201 {
				t.Errorf("entries of the new index: %d, want 201", n)
			}
			if got := fmt.Sprint(s.mustQuery(t, "SELECT COUNT(*) FROM c WHERE b = 3")); got != "[[29]]" {
				t.Errorf("rows by the new index: %s", got)
			}
			// 一意インデックス: 重複がなければ作れる（NULL は対象外）
			s.mustQuery(t, "CREATE UNIQUE INDEX a ON c (a)")
			if got := fmt.Sprint(s.mustQuery(t, "SELECT id FROM c WHERE a = 'v005'")); got != "[[5]]" {
				t.Errorf("row by the unique index: %s", got)
			}
			if n, err := s.st.PendingRewrites(); err != nil || n != 0 {
				t.Errorf("marks left by CREATE INDEX: %d, %v", n, err)
			}
		})
	}
}

// 照合順序で重複する値のある列に一意インデックスを作ると、重複のエラーになり、表は変わらず、作りかけのエントリが
// 消える。重複する行は、同じコミットの単位にある場合と、前にコミットした単位にある場合がある。
func TestChunkedCreateUniqueIndexDuplicate(t *testing.T) {
	for _, small := range []bool{false, true} {
		t.Run(fmt.Sprintf("small=%v", small), func(t *testing.T) {
			if small {
				smallRewrites(t)
			}
			s := start(t, t.TempDir())
			defer s.stop(t)
			s.mustQuery(t, "CREATE TABLE d (id int NOT NULL PRIMARY KEY, a varchar(20) COLLATE utf8mb4_general_ci)")
			var vals []string
			for i := 1; i <= 100; i++ {
				vals = append(vals, fmt.Sprintf("(%d, 'w%03d')", i, i))
			}
			vals = append(vals, "(101, 'W050')")
			s.mustQuery(t, "INSERT INTO d VALUES "+strings.Join(vals, ", "))
			def, _, err := s.st.GetTable(store.MainBranchID, "d", store.Latest)
			if err != nil {
				t.Fatal(err)
			}
			before := fmt.Sprint(s.mustQuery(t, "SHOW INDEX FROM d"))
			if _, err := query(s.ctx, s.engine, "CREATE UNIQUE INDEX a ON d (a)"); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("unique index over duplicates: %v", err)
			}
			if after := fmt.Sprint(s.mustQuery(t, "SHOW INDEX FROM d")); after != before {
				t.Errorf("failed CREATE INDEX changed the table:\n%s\nwant:\n%s", after, before)
			}
			if n := indexEntries(t, s, "d", def.NextIndexID); n != 0 {
				t.Errorf("entries left by the failed CREATE INDEX: %d", n)
			}
			if n, err := s.st.PendingRewrites(); err != nil || n != 0 {
				t.Errorf("marks left by the failed CREATE INDEX: %d, %v", n, err)
			}
		})
	}
}

// エンジンが取り消しも Close も呼ばずに返した書き直し（ENUM を狭めて値が入らない）は、文の終わり（CommandEnd）に
// 取り消す。書きかけと印が残らず、読み出しのスナップショットが閉じて GC の境界が止まらない（段階 53 のレビュー）。
func TestRewriteDiscardedAtCommandEnd(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	sess := s.ctx.Session.(*Session)
	s.mustQuery(t, "CREATE TABLE e (id int NOT NULL PRIMARY KEY, c enum('a','b'))")
	s.mustQuery(t, "INSERT INTO e VALUES (1, 'a'), (2, 'b')")
	if _, err := query(s.ctx, s.engine, "ALTER TABLE e MODIFY c enum('a')"); err == nil {
		t.Fatal("narrowing the enum succeeded")
	}
	sess.CommandEnd()
	if n, err := s.st.PendingRewrites(); err != nil || n != 0 {
		t.Errorf("pending rewrites after the failed ALTER: %d, %v", n, err)
	}
	if len(sess.rewriters) != 0 {
		t.Errorf("session still has %d rewriters", len(sess.rewriters))
	}
	s.mustQuery(t, "INSERT INTO e VALUES (3, 'a')")
	sess.CommandEnd()
	time.Sleep(10 * time.Millisecond)
	stats, err := s.st.GC(time.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Boundary < s.st.LastCommitted() {
		t.Errorf("GC boundary held back by %v after the failed ALTER", time.Duration(s.st.LastCommitted()-stats.Boundary)*time.Microsecond)
	}
	if got := fmt.Sprint(s.mustQuery(t, "SELECT id, c FROM e ORDER BY id")); got != "[[1 1] [2 2] [3 1]]" { // ENUM は番号で返る
		t.Errorf("rows: %s", got)
	}
}

// 主キーの列の数が変わる書き直し（複数の列の主キーから列を消す）は、panic せずに明示的なエラーで拒否する（段階 53 の
// レビュー。WordPress はしない）。
func TestRewriteRejectsPrimaryKeyChange(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE pk2 (a int NOT NULL, b int NOT NULL, v int, PRIMARY KEY (a, b))")
	s.mustQuery(t, "INSERT INTO pk2 VALUES (1, 1, 1), (1, 2, 2), (2, 1, 3)")
	if _, err := query(s.ctx, s.engine, "ALTER TABLE pk2 DROP COLUMN b"); err == nil || !strings.Contains(err.Error(), "changing the primary key is not supported") {
		t.Errorf("dropping a primary key column: %v", err)
	}
	if got := fmt.Sprint(s.mustQuery(t, "SELECT * FROM pk2 ORDER BY a, b")); got != "[[1 1 1] [1 2 2] [2 1 3]]" {
		t.Errorf("rows: %s", got)
	}
	if n, err := s.st.PendingRewrites(); err != nil || n != 0 {
		t.Errorf("pending rewrites: %d, %v", n, err)
	}
}

// 前に途中で終わった作成のエントリは、次に作るインデックスがキーを持たない（FULLTEXT）ときも消す（同じ ID を取る
// ので、残すと定義から参照され、GC が消さなくなる。段階 53 のレビュー）。
func TestStaleIndexEntriesDroppedForFulltext(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE f (id int NOT NULL PRIMARY KEY, a varchar(20), b text)")
	s.mustQuery(t, "INSERT INTO f VALUES (1, 'x', 'y')")
	def, _, err := s.st.GetTable(store.MainBranchID, "f", store.Latest)
	if err != nil {
		t.Fatal(err)
	}
	next := def.NextIndexID
	if err := s.st.Update(func(tx *store.WriteTx) error {
		return tx.PutIndexEntry(store.MainBranchID, def.ID, next, []byte("stale"), nil)
	}); err != nil {
		t.Fatal(err)
	}
	s.mustQuery(t, "CREATE FULLTEXT INDEX ft ON f (b)")
	if n := indexEntries(t, s, "f", next); n != 0 {
		t.Errorf("stale entries under the FULLTEXT index id: %d", n)
	}
}
