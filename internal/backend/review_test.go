package backend

import (
	"context"
	"testing"
)

// 段階 5 と 5.5 のレビューで見つかった問題の再発防止テスト（TCP のサーバを通す）。

// 一意インデックスの追加が、重複のない表では成功し、重複があれば 1062 で失敗して残らない。
func TestReviewAddUniqueKey(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	c := conn(t, db)
	defer c.Close()
	// go-mysql-server の既定の照合順序は utf8mb4_0900_bin（MySQL 8.0 の既定は utf8mb4_0900_ai_ci）なので明示する。
	exec(t, c, "CREATE TABLE u (id int NOT NULL, v varchar(10), w varchar(10), PRIMARY KEY (id)) DEFAULT CHARSET=utf8mb4 COLLATE utf8mb4_unicode_520_ci")
	exec(t, c, "INSERT INTO u VALUES (1, 'a', 'x'), (2, 'b', 'x')")
	exec(t, c, "ALTER TABLE u ADD UNIQUE KEY uv (v)")
	if _, err := c.ExecContext(context.Background(), "INSERT INTO u VALUES (3, 'A', 'y')"); mysqlErrNo(err) != 1062 {
		t.Errorf("insert duplicate into new unique index: err = %v, want 1062", err)
	}
	if _, err := c.ExecContext(context.Background(), "ALTER TABLE u ADD UNIQUE KEY uw (w)"); mysqlErrNo(err) != 1062 {
		t.Errorf("unique index over duplicates: err = %v, want 1062", err)
	}
	if got := queryString(t, c, "SELECT COUNT(*) FROM information_schema.statistics WHERE table_name = 'u' AND index_name = 'uw'"); got != "0" {
		t.Errorf("failed unique index was persisted")
	}
}

// DDL の前に実行中のトランザクションが暗黙にコミットされ、CREATE INDEX が自分の行もインデックスに入れる。
func TestReviewImplicitCommitBeforeDDL(t *testing.T) {
	db, dir, stop := concServer(t)
	c := conn(t, db)
	exec(t, c, "CREATE TABLE x (id int NOT NULL, a varchar(10), PRIMARY KEY (id))")
	exec(t, c, "START TRANSACTION")
	exec(t, c, "INSERT INTO x VALUES (1, 'p')")
	exec(t, c, "ALTER TABLE x ADD KEY a (a)")
	exec(t, c, "ROLLBACK") // DDL で暗黙にコミット済みなので、何も戻らない
	if got := queryString(t, c, "SELECT id FROM x WHERE a = 'p'"); got != "1" {
		t.Errorf("row inserted before CREATE INDEX not found through the index: %q", got)
	}
	c.Close()
	stop()
	s := start(t, dir)
	defer s.stop(t)
	checkIndexConsistency(t, s, "x")
}

// 書き直し（ADD COLUMN）は、DDL を出したトランザクションの古いスナップショットではなく、最新の行を読む。
func TestReviewRewriteSeesLatestRows(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	a, b := conn(t, db), conn(t, db)
	defer a.Close()
	defer b.Close()
	exec(t, a, "CREATE TABLE x (id int NOT NULL, PRIMARY KEY (id))")
	exec(t, a, "INSERT INTO x VALUES (1)")
	exec(t, a, "START TRANSACTION")
	if got := queryString(t, a, "SELECT COUNT(*) FROM x"); got != "1" {
		t.Fatal(got)
	}
	exec(t, b, "INSERT INTO x VALUES (2)")
	exec(t, a, "ALTER TABLE x ADD COLUMN v int")
	if got := queryString(t, b, "SELECT COUNT(*) FROM x"); got != "2" {
		t.Errorf("rows after rewrite = %s, want 2 (a committed row was lost)", got)
	}
}

// 開いているトランザクションは、他の接続が列を変えても同じテーブルを読み続け、書こうとすると 1412 になる。
func TestReviewCatalogSnapshot(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	a, b := conn(t, db), conn(t, db)
	defer a.Close()
	defer b.Close()
	exec(t, a, "CREATE TABLE x (id int NOT NULL, v int, PRIMARY KEY (id))")
	exec(t, a, "INSERT INTO x VALUES (1, 1)")
	exec(t, a, "START TRANSACTION")
	if got := queryString(t, a, "SELECT COUNT(*) FROM x"); got != "1" {
		t.Fatal(got)
	}
	exec(t, b, "ALTER TABLE x ADD COLUMN w int")
	if got := queryString(t, a, "SELECT COUNT(*) FROM x"); got != "1" {
		t.Errorf("count inside transaction after another session's ALTER = %s, want 1", got)
	}
	if _, err := a.ExecContext(context.Background(), "UPDATE x SET v = 2 WHERE id = 1"); mysqlErrNo(err) != 1412 {
		t.Errorf("write with stale table definition: err = %v, want 1412", err)
	}
	exec(t, a, "ROLLBACK")
	exec(t, a, "UPDATE x SET v = 3 WHERE id = 1")
	if got := queryString(t, a, "SELECT v FROM x WHERE id = 1"); got != "3" {
		t.Errorf("v = %s, want 3", got)
	}
}

// スナップショットの後に他の接続が削除した主キーに、挿入できる（MySQL と同じ）。
func TestReviewInsertAfterConcurrentDelete(t *testing.T) {
	db, _, stop := concServer(t)
	defer stop()
	a, b := conn(t, db), conn(t, db)
	defer a.Close()
	defer b.Close()
	exec(t, a, "CREATE TABLE x (id int NOT NULL, PRIMARY KEY (id))")
	exec(t, a, "INSERT INTO x VALUES (5)")
	exec(t, a, "START TRANSACTION")
	if got := queryString(t, a, "SELECT COUNT(*) FROM x"); got != "1" {
		t.Fatal(got)
	}
	exec(t, b, "DELETE FROM x WHERE id = 5")
	exec(t, a, "INSERT INTO x VALUES (5)")
	exec(t, a, "COMMIT")
	if got := queryString(t, b, "SELECT COUNT(*) FROM x"); got != "1" {
		t.Errorf("count = %s, want 1", got)
	}
}
