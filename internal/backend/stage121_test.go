package backend

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/tsukinowasha/heimdb/internal/metrics"
	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 段階 121: 起動の後に、main の水準の古いインデックスを裏で 1 つずつ作り直す（autorebuild.go）。

// waitIndexRebuild は、裏の作り直しが 1 回を終えて止まるまで待ち、その様子を返す。
func waitIndexRebuild(t *testing.T, pro *Provider) rebuildSnapshot {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		s := pro.rebuild.snapshot()
		if s.state == "done" && !s.running {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background rebuild did not finish: %+v", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shutdownAndClose は、裏の処理を止めてからサーバとストアを閉じる（serverConfig.rebuild のとき）。
func shutdownAndClose(t *testing.T, pro *Provider, st *store.Store, srv interface{ Close() error }) {
	t.Helper()
	if !pro.Shutdown(30 * time.Second) {
		t.Error("shutdown did not finish")
	}
	srv.Close()
	if err := st.Close(); err != nil {
		t.Error(err)
	}
}

// addOldUniqueWithDuplicates は、表 name に、行の重複する、水準 0 の一意インデックス index（列 col）を足す（0.2.38 以前の
// CREATE UNIQUE INDEX は重複を確かめなかった。stage117_branch_test.go の TestStage117RebuildFailure と同じ形）。
func addOldUniqueWithDuplicates(t *testing.T, st *store.Store, db *sql.DB, name, index, col, insert string) {
	t.Helper()
	mustExec(t, db, fmt.Sprintf("CREATE UNIQUE INDEX %s ON %s (%s)", index, name, col))
	def, _, _ := st.GetTable(store.MainBranchID, name, store.Latest)
	var idx = def.Indexes[len(def.Indexes)-1]
	idx.KeyLevel = 0
	mustExec(t, db, fmt.Sprintf("ALTER TABLE %s DROP INDEX %s", name, index))
	mustExec(t, db, insert)
	if err := st.Update(func(tx *store.WriteTx) error {
		cur, _, err := st.GetTable(store.MainBranchID, name, store.Latest)
		if err != nil {
			return err
		}
		next := cur.Clone()
		next.Indexes = append(next.Indexes, idx)
		next.Version = tx.TS
		return tx.PutTable(store.MainBranchID, next)
	}); err != nil {
		t.Fatal(err)
	}
}

// logged は、ログに s を含む行があればその行を返す。
func logged(hook *logtest.Hook, s string) string {
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, s) {
			return e.Message
		}
	}
	return ""
}

// バージョン 6 のデータ（0.2.38 の DECIMAL、FLOAT、DOUBLE、ENUM のインデックス）を開くと、main の古いインデックスを裏で作り
// 直し、同じ結果をエントリで引く。重複のある一意インデックスは飛ばして警告にし、同じ表のほかのインデックスは作る。
// ブランチ（dev）は作り直さない。状態と指標とログに出し、次の起動では飛ばしたものだけをやり直す。
func TestStage121Format6Background(t *testing.T) {
	dir := t.TempDir()
	extractFormat6(t, dir)
	// 重複のある一意インデックスを持つ表を足す（同じ表に、作れる古いインデックスもある）
	addr, _, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	mustExec(t, db, "CREATE TABLE du (id INT PRIMARY KEY, d DECIMAL(10,2), KEY kd (d))")
	downgradeKeyLevels(t, st, store.MainBranchID, "du")
	mustExec(t, db, "INSERT INTO du VALUES (1, 1.5), (2, 2.5)")
	addOldUniqueWithDuplicates(t, st, db, "du", "ud", "d", "INSERT INTO du VALUES (3, 1.5)")
	db.Close()
	stopServer(t, st, srv)()

	hook := logtest.NewGlobal()
	defer hook.Reset()
	addr, pro, st, srv := serve(t, dir, serverConfig{rebuild: true})
	s := waitIndexRebuild(t, pro)
	if s.rebuilt != 7 || s.skipped != 1 || s.err != "du.ud: Duplicate entry '1.50' for key 'du.ud'; fix the duplicate rows and run CALL heimdb_rebuild_indexes()" {
		t.Errorf("rebuild %+v", s)
	}
	if msg := logged(hook, "indexes on DECIMAL"); !strings.HasSuffix(msg, "wordpress@dev: ft.kf, ft.ke, ft.kfe, sp.ua, wp_wc_product_meta_lookup.stock_quantity, wp_wc_product_meta_lookup.min_max_price); "+
		"wordpress's are rebuilt in the background now; to build the others, run CALL heimdb_rebuild_indexes() on the primary while connected to each of these databases") {
		t.Errorf("startup log: %s", msg)
	}
	for _, want := range []string{
		"HeimDB: rebuilding 8 indexes on wordpress made by an older HeimDB in the background, one at a time",
		"HeimDB: rebuilt index wp_wc_product_meta_lookup.min_max_price on wordpress (about 6 rows) in ",
		"HeimDB: skipped rebuilding index du.ud on wordpress: Duplicate entry '1.50' for key 'du.ud'; fix the duplicate rows and run CALL heimdb_rebuild_indexes()",
		"HeimDB: rebuilt 7 indexes on wordpress in ",
	} {
		if logged(hook, want) == "" {
			t.Errorf("no log %q", want)
		}
	}
	// main に残る古いインデックスは、重複のある du.ud だけ（同じ表の du.kd は作った）
	if jobs, err := pro.staleMainIndexes(); err != nil || len(jobs) != 1 || jobs[0].table != "du" || jobs[0].index != "ud" {
		t.Errorf("stale on main after the rebuild: %+v %v", jobs, err)
	}
	if got := keyLevels(t, st, store.MainBranchID, "du"); got != fmt.Sprintf("kd=%d,ud=0", rowenc.KeyLevel) {
		t.Errorf("du levels %s", got)
	}
	if got := keyLevels(t, st, branchID(t, st, "dev"), "ft"); got != "kf=0,ke=0,kfe=0" {
		t.Errorf("dev levels %s", got)
	}
	db = openRoot(t, addr)
	db.SetMaxOpenConns(1)
	before := entriesDecoded.Load()
	checkFormat6Queries(t, db, "rebuilt in the background")
	if entriesDecoded.Load() == before {
		t.Error("no entries read after the background rebuild")
	}
	if plan := explain(t, db, "SELECT product_id FROM wp_wc_product_meta_lookup WHERE min_price >= 9.995 AND max_price <= 12.5"); !strings.Contains(plan, "filters: [{[9.995, ∞), [NULL, ∞)}]") {
		t.Errorf("plan after the background rebuild:\n%s", plan)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := statusMap(t, conn)
	conn.Close()
	if m["index_rebuild"] != "done" || m["index_rebuild_indexes"] != "7 rebuilt, 1 skipped" || m["index_rebuild_done_at"] == "NULL" || m["index_rebuild_duration"] == "NULL" ||
		!strings.HasPrefix(m["index_rebuild_error"], "du.ud: Duplicate entry") {
		t.Errorf("status %v", m)
	}
	var mw metrics.Writer
	pro.WriteMetrics(&mw)
	for _, want := range []string{"heimdb_index_rebuild_ready 1\n", "heimdb_index_rebuild_skipped 1\n", "heimdb_index_rebuild_seconds "} {
		if !strings.Contains(string(mw.Bytes()), want) {
			t.Errorf("metrics have no %q", want)
		}
	}
	db.Close()
	shutdownAndClose(t, pro, st, srv)

	// 次の起動は、飛ばした du.ud だけをやり直す（また飛ばす）。重複を直して手続きを呼べば作る
	hook.Reset()
	addr, pro, st, srv = serve(t, dir, serverConfig{rebuild: true})
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	if s := waitIndexRebuild(t, pro); s.rebuilt != 0 || s.skipped != 1 {
		t.Errorf("second start %+v", s)
	}
	if logged(hook, "rebuilding 1 indexes on wordpress") == "" {
		t.Error("the second start did not try du.ud again")
	}
	db = openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "DELETE FROM du WHERE id = 3")
	if got := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false); strings.Join(got, ",") != "du|ud" {
		t.Errorf("rebuilt by hand %v", got)
	}
}

// -rebuild-indexes=false では作り直さず、起動時の警告は前と同じ文で、状態は off。
func TestStage121Off(t *testing.T) {
	dir := t.TempDir()
	extractDataDir(t, "../testdata/format7_data.tar.gz", dir)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	if msg := logged(hook, "indexes on DECIMAL"); !strings.HasSuffix(msg, "; to build them, run CALL heimdb_rebuild_indexes() on the primary while connected to each of these databases") {
		t.Errorf("startup log: %s", msg)
	}
	time.Sleep(100 * time.Millisecond)
	if got := keyLevels(t, st, store.MainBranchID, "sb"); strings.Contains(got, "=2") {
		t.Errorf("levels with the rebuild off %s", got)
	}
	db := openRoot(t, addr)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if m := statusMap(t, conn); m["index_rebuild"] != "off" || m["index_rebuild_indexes"] != "NULL" || m["index_rebuild_error"] != "NULL" {
		t.Errorf("status %v", m)
	}
	var mw metrics.Writer
	pro.WriteMetrics(&mw)
	if body := string(mw.Bytes()); !strings.Contains(body, "heimdb_index_rebuild_ready 0\n") || strings.Contains(body, "heimdb_index_rebuild_seconds") {
		t.Errorf("metrics with the rebuild off:\n%s", body)
	}
}

// stage121Table は、水準 0 のインデックス kd（DECIMAL）と kg（DOUBLE）を持つ表 name に n 行を入れる。
func stage121Table(t *testing.T, st *store.Store, db *sql.DB, name string, n int) {
	t.Helper()
	mustExec(t, db, fmt.Sprintf("CREATE TABLE %s (id INT PRIMARY KEY, d DECIMAL(10,2), g DOUBLE, KEY kd (d), KEY kg (g))", name))
	downgradeKeyLevels(t, st, store.MainBranchID, name)
	for i := 0; i < n; i += 500 {
		var vals []string
		for j := i; j < min(i+500, n); j++ {
			vals = append(vals, fmt.Sprintf("(%d, %d.25, %d.5)", j, j%100, j%37))
		}
		mustExec(t, db, "INSERT INTO "+name+" VALUES "+strings.Join(vals, ","))
	}
}

// レプリカでは動かさない（状態は off、警告は本番が作り直す旨）。レプリカのデータを本番として開く（昇格）と作り直す。
// 本番が裏で作ったエントリは、記録を適用したレプリカにも入っている（本番の記録をそのままバックアップに写した形で
// 確かめる。プロセスでの記録の受け渡しは cmd/heimdb の TestRebuildIndexesReachReplica）。
func TestStage121ReplicaAndPromotion(t *testing.T) {
	p, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addr, pro, srv := serveOn(t, p, serverConfig{})
	db := openRoot(t, addr)
	stage121Table(t, p, db, "r", 10)
	db.Close()
	srv.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	pro.Shutdown(time.Second)
	p.Close()

	r, err := store.OpenWith(backup, store.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	hook := logtest.NewGlobal()
	defer hook.Reset()
	raddr, rpro, rsrv := serveOn(t, r, serverConfig{rebuild: true})
	if msg := logged(hook, "indexes on DECIMAL"); !strings.HasSuffix(msg, "; the primary rebuilds wordpress's in the background after it starts (unless -rebuild-indexes=false), and the entries reach this replica through its commit records") {
		t.Errorf("replica startup log: %s", msg)
	}
	time.Sleep(100 * time.Millisecond)
	if s := rpro.rebuild.snapshot(); s.state != "" || s.running {
		t.Errorf("replica rebuild %+v", s)
	}
	if got := keyLevels(t, r, store.MainBranchID, "r"); got != "kd=0,kg=0" {
		t.Errorf("replica levels %s", got)
	}
	rdb := openRoot(t, raddr)
	conn, err := rdb.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m := statusMap(t, conn); m["index_rebuild"] != "off" {
		t.Errorf("replica status %s", m["index_rebuild"])
	}
	conn.Close()
	rdb.Close()
	shutdownAndClose(t, rpro, r, rsrv)

	// 昇格: -replicate-from を外して同じデータで起動し直す
	q, err := store.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	qaddr, qpro, qsrv := serveOn(t, q, serverConfig{rebuild: true})
	if s := waitIndexRebuild(t, qpro); s.rebuilt != 2 {
		t.Errorf("promoted rebuild %+v", s)
	}
	if got := keyLevels(t, q, store.MainBranchID, "r"); got != fmt.Sprintf("kd=%d,kg=%d", rowenc.KeyLevel, rowenc.KeyLevel) {
		t.Errorf("promoted levels %s", got)
	}
	qdb := openRoot(t, qaddr)
	if got := queryRows(t, qdb, "SELECT id FROM r WHERE d > 5 ORDER BY id", false); strings.Join(got, ",") != "5,6,7,8,9" {
		t.Errorf("promoted: %v", got)
	}
	qdb.Close()
	qsrv.Close()
	backup2 := filepath.Join(t.TempDir(), "backup2")
	if _, err := q.Checkpoint(backup2); err != nil {
		t.Fatal(err)
	}
	shutdownAndClose(t, qpro, q, nopCloser{})
	r2, err := store.OpenWith(backup2, store.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if got := keyLevels(t, r2, store.MainBranchID, "r"); got != fmt.Sprintf("kd=%d,kg=%d", rowenc.KeyLevel, rowenc.KeyLevel) {
		t.Errorf("replica of the rebuilt primary: levels %s", got)
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// 作り直しの間の書き込みはロックを待って通り、失われない。作り直しの間、書き込みのロックの持ち主は rebuild indexes。
// 作った後のインデックスは、使わない同じ条件と同じ行を返す。
func TestStage121WritesDuringRebuild(t *testing.T) {
	dir := t.TempDir()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	stage121Table(t, st, db, "w", 5000)
	pro.Shutdown(time.Second)
	db.Close()
	stopServer(t, st, srv)()

	oldChunk := rewriteChunkBytes
	rewriteChunkBytes = 2048
	defer func() { rewriteChunkBytes = oldChunk }()
	var owners sync.Map
	var stp *store.Store
	var stMu sync.Mutex
	testHookIndexChunk = func(table, index string) {
		stMu.Lock()
		s := stp
		stMu.Unlock()
		holders, _ := s.LockHolders()
		for _, h := range holders {
			owners.Store(h.Owner.Op, true)
		}
		time.Sleep(time.Millisecond) // 書き込みが待つ間を作る
	}
	defer func() { testHookIndexChunk = nil }()
	stMu.Lock()
	st, err := store.Open(dir)
	stp = st
	stMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	addr, pro, srv = serveOn(t, st, serverConfig{rebuild: true})
	db = openRoot(t, addr)
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id := 10000 + w*1000 + i
				if _, err := db.Exec(fmt.Sprintf("INSERT INTO w VALUES (%d, %d.75, %d.125)", id, i, i)); err != nil {
					errs <- err
					return
				}
				if _, err := db.Exec(fmt.Sprintf("UPDATE w SET d = d + 1 WHERE id = %d", w*100+i)); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("write during the rebuild: %v", err)
	}
	if s := waitIndexRebuild(t, pro); s.rebuilt != 2 || s.skipped != 0 {
		t.Errorf("rebuild %+v", s)
	}
	if _, ok := owners.Load(rebuildOp); !ok {
		t.Error("the write lock holder was not rebuild indexes")
	}
	if got := queryRows(t, db, "SELECT COUNT(*) FROM w", false); strings.Join(got, ",") != "5200" {
		t.Errorf("rows %v", got)
	}
	for _, pair := range [][2]string{
		{"SELECT id FROM w WHERE d > 98 ORDER BY id", "SELECT id FROM w WHERE d + 0 > 98 ORDER BY id"},
		{"SELECT id FROM w WHERE d = 3.75 ORDER BY id", "SELECT id FROM w WHERE d + 0 = 3.75 ORDER BY id"},
		{"SELECT id FROM w WHERE g BETWEEN 10 AND 11 ORDER BY id", "SELECT id FROM w WHERE g + 0 BETWEEN 10 AND 11 ORDER BY id"},
		{"SELECT COUNT(*) FROM w WHERE d >= 0", "SELECT COUNT(*) FROM w WHERE d + 0 >= 0"},
	} {
		before := entriesDecoded.Load()
		got := queryRows(t, db, pair[0], false)
		if entriesDecoded.Load() == before {
			t.Errorf("%s read no entries", pair[0])
		}
		if want := queryRows(t, db, pair[1], false); strings.Join(got, ",") != strings.Join(want, ",") || len(got) == 0 {
			t.Errorf("%s\ngot  %v\nwant %v", pair[0], got, want)
		}
	}
	db.Close()
	shutdownAndClose(t, pro, st, srv)
}

// 作り直しの途中で止めると、作りかけのエントリを消して水準 0 のまま残し、panic しない。次の起動で続きを作る。
func TestStage121StopDuringRebuild(t *testing.T) {
	dir := t.TempDir()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	stage121Table(t, st, db, "c", 3000)
	def, _, _ := st.GetTable(store.MainBranchID, "c", store.Latest)
	pro.Shutdown(time.Second)
	db.Close()
	stopServer(t, st, srv)()

	oldChunk := rewriteChunkBytes
	rewriteChunkBytes = 512
	defer func() { rewriteChunkBytes = oldChunk }()
	var stop sync.Once
	var stopped = make(chan struct{})
	var pp *Provider
	var mu sync.Mutex
	testHookIndexChunk = func(table, index string) {
		stop.Do(func() {
			mu.Lock()
			p := pp
			mu.Unlock()
			// 止め始める（Shutdown は、裏の処理の取り消しの後、作りかけを消して戻るのを待つ）
			go func() { p.Shutdown(30 * time.Second); close(stopped) }()
			time.Sleep(50 * time.Millisecond)
		})
	}
	defer func() { testHookIndexChunk = nil }()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	addr, pro, srv = serveOn(t, st, serverConfig{rebuild: true})
	pp = pro
	mu.Unlock()
	_ = addr
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if s := pro.rebuild.snapshot(); s.state != "running" {
		t.Errorf("state after the stop %+v", s)
	}
	if got := keyLevels(t, st, store.MainBranchID, "c"); got != "kd=0,kg=0" {
		t.Errorf("levels after the stop %s", got)
	}
	if n := countIndexKeys(t, st, def.ID, def.Indexes[0].ID) + countIndexKeys(t, st, def.ID, def.Indexes[1].ID); n != 0 {
		t.Errorf("%d entries left after the stop", n)
	}
	srv.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	testHookIndexChunk = nil

	addr, pro, st, srv = serve(t, dir, serverConfig{rebuild: true})
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	if s := waitIndexRebuild(t, pro); s.rebuilt != 2 {
		t.Errorf("rebuild after the restart %+v", s)
	}
	db = openRoot(t, addr)
	defer db.Close()
	if got, want := queryRows(t, db, "SELECT COUNT(*) FROM c WHERE d > 50", false), queryRows(t, db, "SELECT COUNT(*) FROM c WHERE d + 0 > 50", false); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after the restart %v, want %v", got, want)
	}
}

// 長いトランザクションが書き込みのロックを持っていれば、短く待って飛ばす（1205 にせず、警告にする）。見積もった行の数が
// 上限を超えるインデックスも飛ばす。どちらも、手続きを呼べば作れる。
func TestStage121SkipBusyAndLarge(t *testing.T) {
	oldWait, oldRows, oldUnique := rebuildLockWait, rebuildMaxRows, rebuildMaxUniqueRows
	rebuildLockWait = 200 * time.Millisecond
	defer func() { rebuildLockWait, rebuildMaxRows, rebuildMaxUniqueRows = oldWait, oldRows, oldUnique }()
	dir := t.TempDir()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	stage121Table(t, st, db, "b", 10)
	pro.Shutdown(time.Second)
	db.Close()
	stopServer(t, st, srv)()

	// ロックの待ち
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	addr, pro, srv = serveOn(t, st, serverConfig{})
	db = openRoot(t, addr)
	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"BEGIN", "INSERT INTO b VALUES (100, 1, 1)"} {
		if _, err := held.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	hook := logtest.NewGlobal()
	defer hook.Reset()
	pro.StartIndexRebuild(true)
	if s := waitIndexRebuild(t, pro); s.rebuilt != 0 || s.skipped != 2 || !strings.Contains(s.err, "the write lock was not free within 200ms") {
		t.Errorf("rebuild with a held lock %+v", s)
	}
	if msg := logged(hook, "skipped rebuilding index b.kd"); !strings.Contains(msg, "it is tried again at the next start, or run CALL heimdb_rebuild_indexes()") {
		t.Errorf("log %q", msg)
	}
	if _, err := held.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	held.Close()
	if got := keyLevels(t, st, store.MainBranchID, "b"); got != "kd=0,kg=0" {
		t.Errorf("levels %s", got)
	}
	db.Close()
	shutdownAndClose(t, pro, st, srv)

	// 上限（一意でないものは 5 行、一意のものは 3 行にする。b は 11 行）
	rebuildMaxRows, rebuildMaxUniqueRows = 5, 3
	hook.Reset()
	addr, pro, st, srv = serve(t, dir, serverConfig{rebuild: true})
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	if s := waitIndexRebuild(t, pro); s.rebuilt != 0 || s.skipped != 2 || !strings.Contains(s.err, "about 11 rows, more than 5 for a background rebuild") {
		t.Errorf("rebuild over the cap %+v", s)
	}
	if msg := logged(hook, "skipped rebuilding index b.kd"); !strings.Contains(msg, "about 11 rows, more than 5 for a background rebuild, which would hold the write lock too long; run CALL heimdb_rebuild_indexes() at a quiet time") {
		t.Errorf("log %q", msg)
	}
	db = openRoot(t, addr)
	defer db.Close()
	if got := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false); strings.Join(got, ",") != "b|kd,b|kg" {
		t.Errorf("rebuilt by hand %v", got)
	}
}

// 手続きと裏の処理が並んでも、どちらも失敗せず、それぞれのインデックスを 1 回だけ作る（後の方は何もしない）。
func TestStage121ProcedureAndBackground(t *testing.T) {
	dir := t.TempDir()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	for _, name := range []string{"x1", "x2", "x3"} {
		stage121Table(t, st, db, name, 2000)
	}
	pro.Shutdown(time.Second)
	db.Close()
	stopServer(t, st, srv)()

	addr, pro, st, srv = serve(t, dir, serverConfig{rebuild: true})
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	db = openRoot(t, addr)
	defer db.Close()
	byHand := queryRows(t, db, "CALL heimdb_rebuild_indexes()", false)
	s := waitIndexRebuild(t, pro)
	if len(byHand)+s.rebuilt != 6 || s.skipped != 0 {
		t.Errorf("by hand %v, background %+v", byHand, s)
	}
	for _, name := range []string{"x1", "x2", "x3"} {
		if got := keyLevels(t, st, store.MainBranchID, name); got != fmt.Sprintf("kd=%d,kg=%d", rowenc.KeyLevel, rowenc.KeyLevel) {
			t.Errorf("%s levels %s", name, got)
		}
		if got, want := queryRows(t, db, "SELECT COUNT(*) FROM "+name+" WHERE g < 3", false), queryRows(t, db, "SELECT COUNT(*) FROM "+name+" WHERE g + 0 < 3", false); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// main を、古いインデックスのある時点に戻すと（その後に消した表が戻る）、裏の処理がもう 1 回動いて作り直す。
func TestStage121RestoreMain(t *testing.T) {
	dir := t.TempDir()
	addr, pro, st, srv := serve(t, dir, serverConfig{})
	db := openRoot(t, addr)
	stage121Table(t, st, db, "rs", 20)
	t0 := nowMicro(t, db)
	pro.Shutdown(time.Second)
	db.Close()
	stopServer(t, st, srv)()

	addr, pro, st, srv = serve(t, dir, serverConfig{rebuild: true})
	defer func() { shutdownAndClose(t, pro, st, srv) }()
	if s := waitIndexRebuild(t, pro); s.rebuilt != 2 {
		t.Errorf("rebuild %+v", s)
	}
	db = openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "DROP TABLE rs")
	mustExec(t, db, fmt.Sprintf("CALL heimdb_restore('main', '%s')", t0))
	deadline := time.Now().Add(30 * time.Second)
	for keyLevels(t, st, store.MainBranchID, "rs") != fmt.Sprintf("kd=%d,kg=%d", rowenc.KeyLevel, rowenc.KeyLevel) {
		if time.Now().After(deadline) {
			t.Fatalf("levels after the restore %s", keyLevels(t, st, store.MainBranchID, "rs"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s := waitIndexRebuild(t, pro); s.rebuilt != 2 {
		t.Errorf("rebuild after the restore %+v", s)
	}
	if got, want := queryRows(t, db, "SELECT id FROM rs WHERE d >= 10 ORDER BY id", false), queryRows(t, db, "SELECT id FROM rs WHERE d + 0 >= 10 ORDER BY id", false); strings.Join(got, ",") != strings.Join(want, ",") || len(got) == 0 {
		t.Errorf("after the restore %v, want %v", got, want)
	}
}
