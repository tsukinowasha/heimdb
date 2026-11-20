package backend

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 27: インデックスだけで読む（エントリの値に列の元の値を持たせる）、文が使う列だけを復号する、主キーで 1 行を
// 引く近道。

// coveringPosts は、照合順序で等しい違う値（大文字と小文字）を含む記事。
func coveringPosts() string {
	var rows []string
	for i, ts := range [][2]string{
		{"post", "publish"}, {"post", "Publish"}, {"Post", "publish"}, {"post", "draft"}, {"page", "publish"},
		{"post", "PUBLISH"}, {"revision", "inherit"}, {"post", "publish"},
	} {
		id := i + 1
		rows = append(rows, fmt.Sprintf("(%d, '2024-01-%02d 10:00:00', 'c', 'T%d', '', '%s', 'p%d', '', '', '', '%s')", id%3, 10-id, id, ts[1], id, ts[0]))
	}
	// ID は 1 から順に振られる（主キーを明示した複数行の INSERT の insert_id は MySQL と違うので明示しない。段階 22）
	return "INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES " + strings.Join(rows, ", ")
}

func termCountQuery(tt int) string {
	return fmt.Sprintf("SELECT COUNT(*) FROM wp_term_relationships, wp_posts WHERE wp_posts.ID = wp_term_relationships.object_id AND post_status IN ('publish') AND post_type IN ('post') AND term_taxonomy_id = %d", tt)
}

var coveringScript = []string{
	coveringPosts(),
	"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (1, 1), (2, 1), (3, 1), (4, 1), (5, 1), (6, 2), (1, 2), (7, 1), (8, 1)",
	// meta_key はプレフィックス長 191 のインデックス。191 文字を超える値と NULL
	"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_Thumb', 'a'), (2, '_thumb', 'b'), (3, NULL, 'c'), " +
		"(4, REPEAT('k', 200), 'd'), (5, CONCAT(REPEAT('k', 191), 'x'), 'e'), (6, CONCAT(REPEAT('k', 191), 'X'), 'f'), (7, CONCAT(REPEAT('k', 191), 'a'), 'g')",
	// term 数（インデックスだけで読む側と、主キーで 1 行を引く側）
	termCountQuery(1),
	termCountQuery(2),
	termCountQuery(99),
	// インデックスの列だけを使う文は、エントリの値（元の値）を返す
	"SELECT ID, post_type, post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY ID",
	"SELECT post_status, ID FROM wp_posts WHERE post_type = 'POST' ORDER BY post_status, ID",
	"SELECT COUNT(*) FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish'",
	"SELECT ID, post_author FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND post_author > 0 ORDER BY ID",
	"SELECT ID, post_date FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY post_date DESC",
	"SELECT object_id FROM wp_term_relationships WHERE term_taxonomy_id = 1 ORDER BY object_id",
	// プレフィックス長のあるインデックス
	"SELECT meta_id, meta_key FROM wp_postmeta WHERE meta_key = '_thumb' ORDER BY meta_id",
	"SELECT meta_id, meta_key FROM wp_postmeta WHERE meta_key IS NULL",
	"SELECT meta_id, LENGTH(meta_key), RIGHT(meta_key, 1) FROM wp_postmeta WHERE meta_key LIKE 'kkk%' ORDER BY meta_key, meta_id",
	"SELECT meta_id, RIGHT(meta_key, 1) FROM wp_postmeta WHERE meta_key = CONCAT(REPEAT('k', 191), 'x') ORDER BY meta_id",
	"SELECT meta_id FROM wp_postmeta WHERE meta_key > CONCAT(REPEAT('k', 191), 'a') ORDER BY meta_id",
	// 照合順序で等しい値への更新（キーは同じでエントリの値だけが変わる）
	"UPDATE wp_posts SET post_status = 'PUBLISH' WHERE ID = 1",
	"UPDATE wp_postmeta SET meta_key = '_THUMB' WHERE meta_id = 2",
	"SELECT ID, post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY ID",
	"SELECT meta_id, meta_key FROM wp_postmeta WHERE meta_key = '_thumb' ORDER BY meta_id",
	// 列を絞らない文（UPDATE、サブクエリのある SELECT）は、インデックスで見つけた行の本体を読む
	"UPDATE wp_posts SET post_title = CONCAT(post_title, 'x') WHERE post_type = 'post' AND post_status = 'draft'",
	"SELECT ID, post_author, post_date, post_title, post_status, post_name, post_type FROM wp_posts WHERE ID = 4",
	"SELECT post_title, post_name FROM wp_posts WHERE post_type = 'post' AND post_status = 'draft' AND ID IN (SELECT 4)",
	"DELETE FROM wp_postmeta WHERE meta_key = '_thumb' AND meta_value = 'zzz'",
	"INSERT INTO wp_posts (post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) SELECT 'c', CONCAT(post_title, '-copy'), '', 'draft', post_name, '', '', '', 'post' FROM wp_posts WHERE post_type = 'post' AND post_status = 'draft'",
	"SELECT ID, post_title, post_name, post_status FROM wp_posts WHERE post_title LIKE '%-copy' ORDER BY ID",
	"DELETE FROM wp_posts WHERE post_title LIKE '%-copy'",
	// 主キーで 1 行を引く（列の型にそのまま入らない値は近道を使わない）
	"SELECT post_title FROM wp_posts WHERE ID = 3",
	"SELECT post_title FROM wp_posts WHERE ID = '3'",
	"SELECT post_title FROM wp_posts WHERE ID = 3.0",
	"SELECT post_title FROM wp_posts WHERE ID = 3.5",
	"SELECT post_title FROM wp_posts WHERE ID = -1",
	"SELECT post_title FROM wp_posts WHERE ID = 18446744073709551616",
	"SELECT post_title FROM wp_posts WHERE ID = 999",
	"SELECT term_order FROM wp_term_relationships WHERE object_id = 1 AND term_taxonomy_id = 2",
	"SELECT p.post_title, m.meta_value FROM wp_postmeta m JOIN wp_posts p ON p.ID = m.post_id ORDER BY m.meta_id",
	"SELECT m.meta_id, p.post_status FROM wp_postmeta m LEFT JOIN wp_posts p ON p.ID = m.post_id + 3 ORDER BY m.meta_id",
	// トランザクションの中で、引いた後に書いた行も見える（使い回す読み手を捨てる）。失敗した文は取り消す
	"BEGIN",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	termCountQuery(1),
	"INSERT INTO wp_posts (ID, post_content, post_title, post_excerpt, post_status, to_ping, pinged, post_content_filtered, post_type) VALUES (20, 'c', 'T20', '', 'publish', '', '', '', 'post')",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (20, 1)",
	termCountQuery(1),
	"UPDATE wp_posts SET post_title = 'T20b' WHERE ID = 20",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	"INSERT INTO wp_posts (ID, post_content, post_title, post_excerpt, to_ping, pinged, post_content_filtered) VALUES (21, 'c', 'T21', '', '', '', ''), (20, 'c', 'dup', '', '', '', '')",
	"SELECT post_title FROM wp_posts WHERE ID = 21",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	// 未コミットの、照合順序で等しい値への更新を、インデックスだけで読む
	"UPDATE wp_posts SET post_status = 'Publish' WHERE ID = 20",
	"SELECT ID, post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND ID >= 8 ORDER BY ID",
	// セーブポイントまで戻すと、使い回す読み手も戻した後の行を読む
	"SAVEPOINT s1",
	"UPDATE wp_posts SET post_title = 'T20c' WHERE ID = 20",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	"ROLLBACK TO SAVEPOINT s1",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	"SELECT ID, post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND ID >= 8 ORDER BY ID",
	"ROLLBACK",
	"SELECT post_title FROM wp_posts WHERE ID = 20",
	termCountQuery(1),
	// 列を足す ALTER（書き直し）と、既存の行からのインデックスの作成の後
	"ALTER TABLE wp_posts ADD COLUMN extra int NOT NULL DEFAULT 7",
	"SELECT ID, post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY ID",
	termCountQuery(1),
	"CREATE INDEX type_extra ON wp_posts (post_type, extra)",
	"SELECT ID, extra, post_type FROM wp_posts WHERE post_type = 'post' AND extra = 7 ORDER BY ID",
	"UPDATE wp_posts SET extra = 8 WHERE ID = 2",
	"SELECT ID, extra FROM wp_posts WHERE post_type = 'POST' AND extra > 6 ORDER BY ID",
}

// 段階 27 の文の結果が、本物の MySQL と同じ。
func TestCoveringQueriesMatchMySQL(t *testing.T) {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	stmts = append(stmts, coveringScript...)
	checkGolden(t, "../testdata/covering_golden.txt", "heimdb_golden27", stmts)
}

// coveringServer は、WordPress のテーブルに coveringPosts などを入れた HeimDB（プロセスの中）を返す。
func coveringServer(t *testing.T) *server {
	t.Helper()
	s := start(t, t.TempDir())
	t.Cleanup(func() { s.st.Close() })
	for _, q := range append(wpSchema(), coveringScript[:3]...) {
		s.mustQuery(t, q)
	}
	return s
}

// インデックスの列と主キーだけを使う文は本体の行を引かない。ほかの列を使う文は引く。
func TestCoveringReadsSkipRows(t *testing.T) {
	s := coveringServer(t)
	for _, tc := range []struct {
		q       string
		covered bool
	}{
		{"SELECT ID, post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish'", true},
		{"SELECT COUNT(*) FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish'", true},
		{"SELECT meta_id, meta_key FROM wp_postmeta WHERE meta_key = '_thumb'", true},
		{"SELECT object_id FROM wp_term_relationships WHERE term_taxonomy_id = 1", true},
		{"SELECT post_title FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish'", false},
		{"SELECT * FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish'", false},
		{"SELECT post_id FROM wp_postmeta WHERE meta_key = '_thumb'", false},
	} {
		before := pkRowsRead.Load()
		s.mustQuery(t, tc.q)
		read := pkRowsRead.Load() - before
		if tc.covered && read != 0 {
			t.Errorf("%s: read %d rows, want 0 (index only)", tc.q, read)
		}
		if !tc.covered && read == 0 {
			t.Errorf("%s: read no rows, want the rows of the table", tc.q)
		}
	}
}

// 段階 27 より前に書いたエントリ（値を持たない）は、本体の行を引いて読む。
func TestCoveringOldEntries(t *testing.T) {
	s := coveringServer(t)
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, _, err := db.GetTableInsensitive(s.ctx, "wp_posts")
	if err != nil {
		t.Fatal(err)
	}
	table := tbl.(*Table)
	rows, err := table.scanAll(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// どのインデックスも、半分のエントリだけ値を消す
	if err := s.st.Update(func(tx *store.WriteTx) error {
		for _, ki := range table.keyIndexes() {
			for i, row := range rows {
				if i%2 == 0 {
					continue
				}
				entry, _, err := table.entry(ki, row)
				if err != nil {
					return err
				}
				if err := tx.PutIndexEntry(store.MainBranchID, table.def.ID, ki.def.ID, entry, nil); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := pkRowsRead.Load()
	q := "SELECT ID, post_type, post_status FROM wp_posts WHERE post_type = 'post' ORDER BY ID"
	if got, want := rowsString(t, s, q), "[1 post publish]\n[2 post Publish]\n[3 Post publish]\n[4 post draft]\n[6 post PUBLISH]\n[8 post publish]\n"; got != want {
		t.Errorf("%s\n got: %q\nwant: %q", q, got, want)
	}
	if pkRowsRead.Load() == before {
		t.Error("entries without values were read without the rows")
	}
}

// AS OF とブランチでも、エントリの値はその時点、そのブランチの元の値。マージの後も同じ。
func TestCoveringAsOfAndBranches(t *testing.T) {
	s := coveringServer(t)
	s.mustQuery(t, "SET time_zone = '+00:00'")
	t0 := mark(t)
	s.mustQuery(t, "UPDATE wp_posts SET post_status = 'PUBLISH' WHERE ID = 1")
	checkIndexConsistency(t, s, "wp_posts")
	q := "SELECT ID, post_status FROM wp_posts%s WHERE post_type = 'post' AND post_status = 'publish' AND ID < 3 ORDER BY ID"
	for _, tc := range []struct{ asOf, want string }{
		{"", "[1 PUBLISH]\n[2 Publish]\n"},
		{" AS OF '" + t0 + "'", "[1 publish]\n[2 Publish]\n"},
	} {
		before := pkRowsRead.Load()
		if got := rowsString(t, s, fmt.Sprintf(q, tc.asOf)); got != tc.want {
			t.Errorf("AS OF %q: got %q, want %q", tc.asOf, got, tc.want)
		}
		if read := pkRowsRead.Load() - before; read != 0 {
			t.Errorf("AS OF %q: read %d rows, want 0", tc.asOf, read)
		}
	}

	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_posts (ID, post_content, post_title, post_excerpt, post_status, to_ping, pinged, post_content_filtered, post_type) VALUES (10, 'c', 'T10', '', 'publish', '', '', '', 'post')")
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE wp_posts SET post_status = 'Publish' WHERE ID = 10")
	q = "SELECT post_status FROM wp_posts WHERE post_type = 'post' AND post_status = 'publish' AND ID = 10"
	if got := one(t, staging, q); got != "Publish" {
		t.Errorf("staging: got %q, want Publish", got)
	}
	if got := one(t, mainDB, q); got != "publish" {
		t.Errorf("main: got %q, want publish", got)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, q); got != "Publish" {
		t.Errorf("main after merge: got %q, want Publish", got)
	}
}
