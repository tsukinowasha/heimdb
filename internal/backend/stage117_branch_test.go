package backend

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 117: キーの水準の古いインデックス（段階 117 より前に作った DECIMAL などのインデックス）と heimdb_rebuild_indexes。
// 作り直しの前後の AS OF、作り直しの前と後に分かれたブランチ、両方向のマージ、取り込み、復元、GC、レプリカで、
// インデックスで引いた結果が、インデックスを使わない同じ条件の結果と同じで、エントリを読むのは水準 1 のときだけかを確かめる。

// downgradeKeyLevels は、表 name のインデックスのキーの水準を 0 にした定義を書く（段階 117 より前に作った表と同じ。
// 行を入れる前に呼ぶので、エントリはない）。
func downgradeKeyLevels(t *testing.T, st *store.Store, branch uint64, name string) {
	t.Helper()
	def, ok, err := st.GetTable(branch, name, store.Latest)
	if err != nil || !ok {
		t.Fatalf("%s: %v %v", name, ok, err)
	}
	next := def.Clone()
	for i := range next.Indexes {
		next.Indexes[i].KeyLevel = 0
	}
	if err := st.Update(func(tx *store.WriteTx) error {
		next.Version = tx.TS
		return tx.PutTable(branch, next)
	}); err != nil {
		t.Fatal(err)
	}
}

// keyLevels は、表 name のインデックスの名前と水準を「名前=水準」で並べる。
func keyLevels(t *testing.T, st *store.Store, branch uint64, name string) string {
	t.Helper()
	def, ok, err := st.GetTable(branch, name, store.Latest)
	if err != nil || !ok {
		t.Fatalf("%s: %v %v", name, ok, err)
	}
	var out []string
	for _, idx := range def.Indexes {
		out = append(out, fmt.Sprintf("%s=%d", idx.Name, idx.KeyLevel))
	}
	return strings.Join(out, ",")
}

// stage117Pairs は、インデックスで引く文と、同じ条件をインデックスを使わない形にした文（列に +0 などをかける）。
// %s は表の名前（AS OF を付けることがある）。
var stage117Pairs = [][2]string{
	{"SELECT id FROM %s WHERE d > 1.5 ORDER BY id", "SELECT id FROM %s WHERE d + 0 > 1.5 ORDER BY id"},
	{"SELECT id FROM %s WHERE d = 2.5 ORDER BY id", "SELECT id FROM %s WHERE d + 0 = 2.5 ORDER BY id"},
	{"SELECT id FROM %s WHERE d BETWEEN -1 AND 2 ORDER BY id", "SELECT id FROM %s WHERE d + 0 BETWEEN -1 AND 2 ORDER BY id"},
	{"SELECT id, d FROM %s WHERE d IS NOT NULL ORDER BY d, id", "SELECT id, d FROM %s WHERE d + 0 IS NOT NULL ORDER BY d + 0, id"},
	{"SELECT id FROM %s WHERE g BETWEEN -1 AND 1.5 ORDER BY id", "SELECT id FROM %s WHERE g + 0 BETWEEN -1 AND 1.5 ORDER BY id"},
	{"SELECT id FROM %s WHERE g = 0.5 ORDER BY id", "SELECT id FROM %s WHERE g + 0 = 0.5 ORDER BY id"},
	{"SELECT id FROM %s WHERE e = 'b' ORDER BY id", "SELECT id FROM %s WHERE CONCAT(e) = 'b' ORDER BY id"},
	{"SELECT id FROM %s WHERE s = 'x' AND d < 3 ORDER BY id", "SELECT id FROM %s WHERE CONCAT(s) = 'x' AND d + 0 < 3 ORDER BY id"},
}

// stage117Server は、水準 0 のインデックスの表 p（整数の主キー）と q（文字列の主キー）を作ったサーバ。
func stage117Server(t *testing.T) (main *sql.DB, addr string, st *store.Store, stop func()) {
	t.Helper()
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{})
	main = openRoot(t, addr)
	mustExec(t, main, "CREATE TABLE p (id INT PRIMARY KEY, d DECIMAL(10,2), g DOUBLE, e ENUM('a','b','c'), s VARCHAR(10), "+
		"KEY kd (d), KEY kg (g), KEY ke (e), KEY ksd (s, d), KEY kid (id), UNIQUE KEY ud (d, s))")
	mustExec(t, main, "CREATE TABLE q (code VARCHAR(10) PRIMARY KEY, d DECIMAL(10,2), KEY kd (d))")
	downgradeKeyLevels(t, st, store.MainBranchID, "p")
	downgradeKeyLevels(t, st, store.MainBranchID, "q")
	mustExec(t, main, "INSERT INTO p VALUES (1, 1.50, 0.5, 'a', 'x'), (2, 2.50, -0.5, 'b', 'x'), (3, NULL, NULL, NULL, 'y'), (4, -1, 1.5, 'b', 'x'), "+
		"(5, 2.5, 0.5, 'c', 'y'), (6, 0, 0, 'a', NULL)")
	mustExec(t, main, "INSERT INTO q VALUES ('k1', 1.5), ('k2', -2), ('k3', NULL), ('k4', 3)")
	return main, addr, st, func() { main.Close(); stopServer(t, st, srv)() }
}

// checkStage117 は、db の表 p（table に AS OF を付けてもよい）で stage117Pairs の結果が同じかと、q の結果を確かめ、
// インデックスで引いた文がエントリを読んだかが keyed と一致するかを確かめる。
func checkStage117(t *testing.T, db *sql.DB, label, table string, keyed bool) {
	t.Helper()
	read := int64(0)
	for _, pair := range stage117Pairs {
		before := entriesDecoded.Load()
		got := queryRows(t, db, fmt.Sprintf(pair[0], table), false)
		read += entriesDecoded.Load() - before
		want := queryRows(t, db, fmt.Sprintf(pair[1], table), false)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %s\ngot  %v\nwant %v", label, fmt.Sprintf(pair[0], table), got, want)
		}
	}
	qt := strings.Replace(table, "p", "q", 1)
	before := entriesDecoded.Load()
	got := queryRows(t, db, "SELECT code FROM "+qt+" WHERE d >= 0 ORDER BY code", false)
	read += entriesDecoded.Load() - before
	if want := queryRows(t, db, "SELECT code FROM "+qt+" WHERE d + 0 >= 0 ORDER BY code", false); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: q: got %v, want %v", label, got, want)
	}
	if (read > 0) != keyed {
		t.Errorf("%s: %d entries read, want keyed = %v", label, read, keyed)
	}
}

// nowMicro は、サーバの今の時刻（セッションの time_zone）。続けて呼んでも同じにならないよう少し待つ。
func nowMicro(t *testing.T, db *sql.DB) string {
	t.Helper()
	time.Sleep(2 * time.Millisecond)
	var s string
	if err := db.QueryRow("SELECT NOW(6)").Scan(&s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	return s
}

func TestStage117RebuildIndexes(t *testing.T) {
	main, addr, st, stop := stage117Server(t)
	defer stop()
	dsn := fmt.Sprintf("root@tcp(%s)/%s", addr, testDB)
	checkStage117(t, main, "level 0", "p", false)
	t0 := nowMicro(t, main)
	mustExec(t, main, "CALL heimdb_branch_create('before')")
	mustExec(t, main, "UPDATE p SET d = 2.25 WHERE id = 1")
	mustExec(t, main, "UPDATE q SET d = 5 WHERE code = 'k2'")

	// 作り直す。整数だけの kid は水準 0 でもエントリを持つので作り直さない（水準も 0 のまま）。
	got := queryRows(t, main, "CALL heimdb_rebuild_indexes()", false)
	if want := "p|kd,p|kg,p|ke,p|ksd,p|ud,q|kd"; strings.Join(got, ",") != want {
		t.Errorf("rebuilt %v, want %s", got, want)
	}
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=2,kg=2,ke=2,ksd=2,kid=0,ud=2" {
		t.Errorf("levels %s", got)
	}
	if got := queryRows(t, main, "CALL heimdb_rebuild_indexes()", false); len(got) != 0 {
		t.Errorf("second call rebuilt %v", got)
	}
	checkStage117(t, main, "rebuilt", "p", true)
	t1 := nowMicro(t, main)
	// 作り直しの前の時点は水準 0 の定義で読み（エントリを読まない）、後の時点は水準 1
	checkStage117(t, main, "as of before", fmt.Sprintf("p AS OF '%s'", t0), false)
	checkStage117(t, main, "as of after", fmt.Sprintf("p AS OF '%s'", t1), true)
	if got := queryRows(t, main, fmt.Sprintf("SELECT id FROM p AS OF '%s' WHERE d = 2.5 ORDER BY id", t0), false); strings.Join(got, ",") != "2,5" {
		t.Errorf("as of before: %v", got)
	}

	// 作り直した後の書き込みはエントリを保守する
	mustExec(t, main, "INSERT INTO p VALUES (7, 1.75, 0.5, 'b', 'x'), (8, 9, -3, 'c', 'z')")
	mustExec(t, main, "UPDATE p SET d = 0.25, g = 1.25, e = 'a' WHERE id = 2")
	mustExec(t, main, "DELETE FROM p WHERE id = 4")
	mustExec(t, main, "INSERT INTO q VALUES ('k5', 0)")
	checkStage117(t, main, "writes after rebuild", "p", true)
	if _, err := main.Exec("INSERT INTO p VALUES (9, 1.75, 0, 'a', 'x')"); err == nil || !strings.Contains(err.Error(), "1062") {
		t.Errorf("duplicate after rebuild: %v", err)
	}

	// 作り直しの前に分かれたブランチは水準 0 のまま（エントリなし）。ブランチで作り直すと、親から見える行も含めて作る。
	before := openBranch(t, dsn, "before")
	defer before.Close()
	before.SetMaxOpenConns(1)
	bID := branchID(t, st, "before")
	checkStage117(t, before, "branch before", "p", false)
	mustExec(t, before, "UPDATE p SET d = 7 WHERE id = 6")
	mustExec(t, before, "INSERT INTO p VALUES (20, 2.75, 0.5, 'b', 'x')")
	checkStage117(t, before, "branch before, writes", "p", false)
	if got := queryRows(t, before, "CALL heimdb_rebuild_indexes()", false); len(got) != 6 {
		t.Errorf("rebuild on the branch: %v", got)
	}
	if got := keyLevels(t, st, bID, "p"); got != "kd=2,kg=2,ke=2,ksd=2,kid=0,ud=2" {
		t.Errorf("branch levels %s", got)
	}
	checkStage117(t, before, "branch before, rebuilt", "p", true)
	mustExec(t, before, "UPDATE p SET g = 0.5 WHERE id = 3")
	checkStage117(t, before, "branch before, rebuilt, writes", "p", true)

	// 作り直しの後に分かれたブランチは水準 1 と親のエントリを引き継ぐ
	mustExec(t, main, "CALL heimdb_branch_create('after')")
	after := openBranch(t, dsn, "after")
	defer after.Close()
	after.SetMaxOpenConns(1)
	if got := keyLevels(t, st, branchID(t, st, "after"), "p"); got != "kd=2,kg=2,ke=2,ksd=2,kid=0,ud=2" {
		t.Errorf("branch after levels %s", got)
	}
	checkStage117(t, after, "branch after", "p", true)
	mustExec(t, after, "UPDATE p SET d = -4 WHERE id = 1")
	mustExec(t, after, "INSERT INTO p VALUES (30, 3, 2, 'c', 'x')")
	mustExec(t, after, "DELETE FROM p WHERE id = 7")
	checkStage117(t, after, "branch after, writes", "p", true)

	// マージ（ブランチも親も水準 1）
	mustExec(t, main, "CALL heimdb_branch_merge('after')")
	checkStage117(t, main, "merged after", "p", true)
	mustExec(t, main, "CALL heimdb_branch_merge('before')")
	checkStage117(t, main, "merged before", "p", true)

	// 作り直しの前の時点への復元は、定義を書き直さず（水準 1 のまま）、行をエントリごと戻す
	mustExec(t, main, fmt.Sprintf("CALL heimdb_restore('main', '%s', 'p')", t0))
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=2,kg=2,ke=2,ksd=2,kid=0,ud=2" {
		t.Errorf("levels after restore %s", got)
	}
	checkStage117(t, main, "restored", "p", true)
	if got, want := queryRows(t, main, "SELECT id, d FROM p ORDER BY id", false), queryRows(t, main, fmt.Sprintf("SELECT id, d FROM p AS OF '%s' ORDER BY id", t0), false); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("restored rows %v, want %v", got, want)
	}
	// 定義が違う時点への復元（インデックスを足した後）も、同じ ID のインデックスの水準を下げない
	mustExec(t, main, "CREATE INDEX kgd ON p (g, d)")
	t2 := nowMicro(t, main)
	mustExec(t, main, "DROP INDEX kgd ON p")
	mustExec(t, main, "UPDATE p SET d = 8 WHERE id = 2")
	mustExec(t, main, fmt.Sprintf("CALL heimdb_restore('main', '%s', 'p')", t0))
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=2,kg=2,ke=2,ksd=2,kid=0,ud=2" {
		t.Errorf("levels after redefining restore %s", got)
	}
	checkStage117(t, main, "restored with definition", "p", true)
	mustExec(t, main, fmt.Sprintf("CALL heimdb_restore('main', '%s', 'p')", t2))
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=2,kg=2,ke=2,ksd=2,kid=0,ud=2,kgd=2" {
		t.Errorf("levels after restoring kgd %s", got)
	}
	checkStage117(t, main, "restored kgd", "p", true)
	if got, want := queryRows(t, main, "SELECT id FROM p WHERE g = 0.5 AND d > 1 ORDER BY id", false), queryRows(t, main, "SELECT id FROM p WHERE g + 0 = 0.5 AND d + 0 > 1 ORDER BY id", false); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("kgd after restore %v, want %v", got, want)
	}

	// GC の後も同じ
	if _, err := st.GC(time.Microsecond); err != nil {
		t.Fatal(err)
	}
	checkStage117(t, main, "after gc", "p", true)
	checkStage117(t, before, "branch before, after gc", "p", true)
}

// 親（水準 0）から分かれたブランチで作り直してマージする。取り込み（heimdb_branch_sync）は、親で作り直した後も拒否されない。
func TestStage117RebuildMergeAndSync(t *testing.T) {
	main, addr, st, stop := stage117Server(t)
	defer stop()
	dsn := fmt.Sprintf("root@tcp(%s)/%s", addr, testDB)
	mustExec(t, main, "CALL heimdb_branch_create('b1')")
	mustExec(t, main, "CALL heimdb_branch_create('s1')")
	b1 := openBranch(t, dsn, "b1")
	defer b1.Close()
	b1.SetMaxOpenConns(1)
	s1 := openBranch(t, dsn, "s1")
	defer s1.Close()
	s1.SetMaxOpenConns(1)

	// ブランチ（水準 1）から親（水準 0）へ
	mustExec(t, b1, "CALL heimdb_rebuild_indexes()")
	mustExec(t, b1, "UPDATE p SET d = 0.5 WHERE id = 5")
	mustExec(t, b1, "INSERT INTO p VALUES (40, 1.25, 1, 'c', 'x')")
	checkStage117(t, b1, "b1", "p", true)
	mustExec(t, main, "CALL heimdb_branch_merge('b1')")
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=0,kg=0,ke=0,ksd=0,kid=0,ud=0" {
		t.Errorf("parent levels after merge %s", got)
	}
	checkStage117(t, main, "merged into level 0", "p", false)

	// 親で作り直した後、水準 0 のブランチへ取り込む
	mustExec(t, s1, "UPDATE p SET e = 'c' WHERE id = 1")
	mustExec(t, main, "CALL heimdb_rebuild_indexes()")
	mustExec(t, main, "UPDATE p SET d = -0.5, s = 'x' WHERE id = 6")
	checkStage117(t, main, "parent rebuilt", "p", true)
	rows := queryRows(t, main, "CALL heimdb_branch_sync('s1')", false)
	for _, r := range rows {
		if strings.Contains(r, "refused") || strings.Contains(r, "conflict") {
			t.Errorf("sync: %v", rows)
		}
	}
	if got := queryRows(t, s1, "SELECT d FROM p WHERE id = 6", false); strings.Join(got, ",") != "-0.50" {
		t.Errorf("sync did not bring the parent's row: %v (%v)", got, rows)
	}
	checkStage117(t, s1, "synced", "p", false)
	mustExec(t, main, "CALL heimdb_branch_merge('s1')")
	checkStage117(t, main, "merged s1", "p", true)
	if got := queryRows(t, main, "SELECT e FROM p WHERE id = 1", false); strings.Join(got, ",") != "c" {
		t.Errorf("merged s1: %v", got)
	}
}

// 一意インデックスの行が重複していれば作り直しは 1062 で失敗し、作りかけのエントリを消して水準 0 のまま。作り直しの
// 途中で落ちたときの作りかけ（作成の途中の印）は、起動時に消える。
func TestStage117RebuildFailure(t *testing.T) {
	dir := t.TempDir()
	addr, _, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	mustExec(t, db, "CREATE TABLE u (id INT PRIMARY KEY, d DECIMAL(10,2), f FLOAT, KEY kf (f), UNIQUE KEY ud (d))")
	downgradeKeyLevels(t, st, store.MainBranchID, "u")
	def, _, _ := st.GetTable(store.MainBranchID, "u", store.Latest)
	// 水準 0 の一意インデックスは、MySQL と同じく行どうしを比べて重複を拒否する
	mustExec(t, db, "INSERT INTO u VALUES (1, 1.5, 0.5), (2, 2.5, 1)")
	if _, err := db.Exec("INSERT INTO u VALUES (3, 1.50, 2)"); err == nil || !strings.Contains(err.Error(), "1062") {
		t.Fatalf("duplicate on level 0: %v", err)
	}
	// 段階 117 より前のデータには重複がありうる（ここではエントリを経ずに定義のバージョンだけ変えて作る）
	mustExec(t, db, "ALTER TABLE u DROP INDEX ud")
	mustExec(t, db, "INSERT INTO u VALUES (3, 1.50, 2)")
	if err := st.Update(func(tx *store.WriteTx) error {
		cur, _, err := st.GetTable(store.MainBranchID, "u", store.Latest)
		if err != nil {
			return err
		}
		next := cur.Clone()
		next.Indexes = append(next.Indexes, def.Indexes[1])
		next.Version = tx.TS
		return tx.PutTable(store.MainBranchID, next)
	}); err != nil {
		t.Fatal(err)
	}
	old := rewriteChunkBytes
	rewriteChunkBytes = 1 // 1 行ごとにコミットする（重複の前に作りかけをコミットする）
	_, err := db.Exec("CALL heimdb_rebuild_indexes()")
	rewriteChunkBytes = old
	if err == nil || !strings.Contains(err.Error(), "1062") {
		t.Errorf("rebuild with duplicates: %v", err)
	}
	if got := keyLevels(t, st, store.MainBranchID, "u"); got != "kf=0,ud=0" {
		t.Errorf("levels after the failed rebuild %s", got)
	}
	if n := countIndexKeys(t, st, def.ID, def.Indexes[0].ID) + countIndexKeys(t, st, def.ID, def.Indexes[1].ID); n != 0 {
		t.Errorf("%d entries left after the failed rebuild", n)
	}

	// 作りかけの印とエントリを残して落ちる
	mustExec(t, db, "DELETE FROM u WHERE id = 3")
	if err := st.Update(func(tx *store.WriteTx) error {
		if err := tx.MarkIndexBuild(store.MainBranchID, def.ID, def.Indexes[0].ID); err != nil {
			return err
		}
		return tx.PutIndexEntry(store.MainBranchID, def.ID, def.Indexes[0].ID, []byte{0x01, 0x02}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	stopServer(t, st, srv)()
	addr, _, st, srv = serve(t, dir, serverConfig{})
	defer stopServer(t, st, srv)()
	if n := countIndexKeys(t, st, def.ID, def.Indexes[0].ID); n != 0 {
		t.Errorf("%d entries of the unfinished rebuild after restart", n)
	}
	db = openRoot(t, addr)
	defer db.Close()
	if got := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false); strings.Join(got, ",") != "u|kf,u|ud" {
		t.Errorf("rebuild after restart: %v", got)
	}
	before := entriesDecoded.Load()
	if got := queryRows(t, db, "SELECT id FROM u WHERE f >= 0.5 AND d > 0 ORDER BY id", false); strings.Join(got, ",") != "1,2" {
		t.Errorf("after rebuild: %v", got)
	}
	if entriesDecoded.Load() == before {
		t.Error("no entries read after rebuild")
	}
}

// countIndexKeys は、main のインデックスのキー（バージョンを含む）の数。
func countIndexKeys(t *testing.T, st *store.Store, tableID, indexID uint64) int {
	t.Helper()
	n := 0
	if err := st.ScanIndexAsOf(store.MainBranchID, tableID, indexID, store.Latest, nil, nil, false, func(_, _ []byte) (bool, error) {
		n++
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// branchID は、ブランチ name の ID。
func branchID(t *testing.T, st *store.Store, name string) uint64 {
	t.Helper()
	b, ok := st.BranchByName(name)
	if !ok {
		t.Fatalf("no branch %s", name)
	}
	return b.ID
}

// レプリカは作り直したエントリを記録で受け取って使い、手続きは拒否する。
func TestStage117RebuildOnReplica(t *testing.T) {
	p, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	addr, _, srv := serveOn(t, p, serverConfig{})
	db := openRoot(t, addr)
	mustExec(t, db, "CREATE TABLE r (id INT PRIMARY KEY, d DECIMAL(10,2), KEY kd (d))")
	downgradeKeyLevels(t, p, store.MainBranchID, "r")
	mustExec(t, db, "INSERT INTO r VALUES (1, 1.5), (2, 2.5), (3, NULL)")
	mustExec(t, db, "CALL heimdb_rebuild_indexes()")
	db.Close()
	srv.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	r, err := store.OpenWith(backup, store.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := keyLevels(t, r, store.MainBranchID, "r"); got != fmt.Sprintf("kd=%d", rowenc.KeyLevel) {
		t.Errorf("replica levels %s", got)
	}
	if _, err := NewProvider(r, testDB).rebuildKeyLevels(gsql.NewEmptyContext()); err == nil || !strings.Contains(err.Error(), "read-only replica") {
		t.Errorf("rebuild on a replica: %v", err)
	}
	raddr, _, rsrv := serveOn(t, r, serverConfig{})
	defer rsrv.Close()
	rdb := openRoot(t, raddr)
	defer rdb.Close()
	before := entriesDecoded.Load()
	if got := queryRows(t, rdb, "SELECT id FROM r WHERE d > 2 ORDER BY id", false); strings.Join(got, ",") != "2" {
		t.Errorf("replica: %v", got)
	}
	if entriesDecoded.Load() == before {
		t.Error("the replica did not read the entries")
	}
}

// 表を書き直す ALTER TABLE は、すべてのインデックスに今の水準を付けてエントリを作る。書き直さない ALTER（FORCE、
// インデックスの名前の変更）は水準を変えない（docs/differences.md）。
func TestStage117AlterRewrite(t *testing.T) {
	main, _, st, stop := stage117Server(t)
	defer stop()
	mustExec(t, main, "ALTER TABLE p FORCE")
	mustExec(t, main, "ALTER TABLE p RENAME INDEX kg TO kg2")
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=0,kg2=0,ke=0,ksd=0,kid=0,ud=0" {
		t.Errorf("levels after FORCE %s", got)
	}
	t0 := nowMicro(t, main)
	mustExec(t, main, "ALTER TABLE p ADD COLUMN z INT")
	mustExec(t, main, "ALTER TABLE q MODIFY d DECIMAL(12,3)")
	if got := keyLevels(t, st, store.MainBranchID, "p"); got != "kd=2,kg2=2,ke=2,ksd=2,kid=2,ud=2" {
		t.Errorf("levels after the rewrite %s", got)
	}
	checkStage117(t, main, "rewritten", "p", true)
	checkStage117(t, main, "as of before the rewrite", fmt.Sprintf("p AS OF '%s'", t0), false)
	if got := queryRows(t, main, "CALL heimdb_rebuild_indexes()", false); len(got) != 0 {
		t.Errorf("rebuild after the rewrite: %v", got)
	}
	mustExec(t, main, "UPDATE p SET d = 5 WHERE id = 3")
	checkStage117(t, main, "rewritten, writes", "p", true)
}
