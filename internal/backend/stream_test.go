package backend

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// streamTable は、主キー id、インデックス k の表 m に n 行を入れる。
func streamTable(t *testing.T, s *server, n int) {
	t.Helper()
	s.mustQuery(t, "CREATE TABLE m (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, k int NOT NULL, v varchar(100), KEY k (k))")
	for b := 0; b < n; b += 1000 {
		var vals []string
		for i := b; i < min(b+1000, n); i++ {
			vals = append(vals, fmt.Sprintf("(%d, %d, 'value-%d')", i+1, i%97, i))
		}
		s.mustQuery(t, "INSERT INTO m (id, k, v) VALUES "+strings.Join(vals, ","))
	}
}

func (s *server) intQuery(t *testing.T, q string) int64 {
	t.Helper()
	rows := s.mustQuery(t, q)
	var f float64 // SUM は小数で返る
	if _, err := fmt.Sscan(fmt.Sprint(rows[0][0]), &f); err != nil {
		t.Fatalf("%s: %v", q, rows)
	}
	return int64(f)
}

// 1 行ずつ読む走査は、同じ文が書いた行を読まない（docs/plan.md の段階 55。前は当たる行をすべて集めてから返していた）。
// 同じ表への INSERT ... SELECT、主キーを変える UPDATE（全行、主キーの範囲、インデックスの範囲）、条件のない DELETE。
func TestStreamingScanSkipsOwnWrites(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	const n = 3000
	streamTable(t, s, n)

	s.mustQuery(t, "INSERT INTO m (k, v) SELECT k, v FROM m")
	if got := s.intQuery(t, "SELECT COUNT(*) FROM m"); got != 2*n {
		t.Fatalf("INSERT ... SELECT from the same table: %d rows, want %d", got, 2*n)
	}
	s.mustQuery(t, "DELETE FROM m WHERE id > 3000")

	sum := s.intQuery(t, "SELECT SUM(id) FROM m")
	for _, q := range []string{
		"UPDATE m SET id = id + 100000",                // 全行
		"UPDATE m SET id = id + 100000 WHERE id > 0",   // 主キーの範囲
		"UPDATE m SET id = id + 100000 WHERE k >= 0",   // インデックスの範囲
		"UPDATE m SET id = id - 300000 WHERE id > 200", // 主キーが範囲に戻る
	} {
		s.mustQuery(t, q)
	}
	if got := s.intQuery(t, "SELECT SUM(id) FROM m"); got != sum {
		t.Fatalf("each row was not updated exactly once: sum of ids %d, want %d", got, sum)
	}
	ksum := s.intQuery(t, "SELECT SUM(k) FROM m")
	s.mustQuery(t, "UPDATE m SET k = k + 1000 WHERE k >= 0 AND k < 50") // インデックスの列そのものを変える
	if got, want := s.intQuery(t, "SELECT SUM(k) FROM m"), ksum+1000*s.intQuery(t, "SELECT COUNT(*) FROM m WHERE k >= 1000"); got != want {
		t.Fatalf("index range update: sum of k %d, want %d", got, want)
	}

	// 前の文の変更は見える（トランザクションの中）
	s.mustQuery(t, "START TRANSACTION")
	s.mustQuery(t, "INSERT INTO m (id, k, v) VALUES (999999, 5, 'tx')")
	if got := s.intQuery(t, "SELECT COUNT(*) FROM m WHERE v = 'tx'"); got != 1 {
		t.Errorf("full scan in the transaction: %d", got)
	}
	if got := s.intQuery(t, "SELECT COUNT(*) FROM m WHERE id > 999990"); got != 1 {
		t.Errorf("primary key range in the transaction: %d", got)
	}
	if got := s.intQuery(t, "SELECT COUNT(*) FROM m WHERE k = 5 AND v = 'tx'"); got != 1 {
		t.Errorf("index range in the transaction: %d", got)
	}
	s.mustQuery(t, "ROLLBACK")

	// AUTO_INCREMENT のある表の条件のない DELETE は、1 行ずつ消す（TRUNCATE にしない）
	s.mustQuery(t, "DELETE FROM m")
	if got := s.intQuery(t, "SELECT COUNT(*) FROM m"); got != 0 {
		t.Fatalf("DELETE without WHERE left %d rows", got)
	}
}

// LIMIT のある文は、表の全行を読まずにやめる（集めずに 1 行ずつ読む。段階 55）。
func TestStreamingScanStopsAtLimit(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	const n = 20000
	streamTable(t, s, n)
	s.mustQuery(t, "SELECT * FROM m WHERE id > 0 AND k > 0 LIMIT 1") // 実行計画の統計を先に作る（全行を間引いて読む）
	for _, q := range []string{
		"SELECT * FROM m LIMIT 3",
		"SELECT * FROM m WHERE id > 0 LIMIT 3",
		"SELECT * FROM m WHERE id > 0 ORDER BY id DESC LIMIT 3",
		"SELECT * FROM m WHERE v LIKE 'value-1%' LIMIT 3",
	} {
		before := rowsDecoded.Load()
		if rows := s.mustQuery(t, q); len(rows) != 3 {
			t.Fatalf("%s: %d rows", q, len(rows))
		}
		if read := rowsDecoded.Load() - before; read > 100 {
			t.Errorf("%s read %d rows", q, read)
		}
	}
	for _, q := range []string{
		"SELECT id, k FROM m WHERE k >= 0 LIMIT 3",  // エントリだけで読む
		"SELECT * FROM m WHERE k >= 0 LIMIT 3",      // 本体の行を引く
		"SELECT * FROM m WHERE k IN (1, 2) LIMIT 3", // 区間が 2 つ
	} {
		before := entriesDecoded.Load()
		if rows := s.mustQuery(t, q); len(rows) != 3 {
			t.Fatalf("%s: %d rows", q, len(rows))
		}
		if read := entriesDecoded.Load() - before; read > 64 { // 最初の fill（firstFill）で足りる
			t.Errorf("%s read %d index entries", q, read)
		}
	}
	// 区間を移して読む（IN と逆順）。結果は MySQL と同じ並び
	if got := fmt.Sprint(s.mustQuery(t, "SELECT id FROM m WHERE id IN (5, 19000, 3) ORDER BY id DESC")); got != "[[19000] [5] [3]]" {
		t.Errorf("IN on the primary key, descending: %s", got)
	}
	if got := fmt.Sprint(s.mustQuery(t, "SELECT k, id FROM m WHERE k IN (3, 1) ORDER BY k DESC, id DESC LIMIT 4")); got != "[[3 19986] [3 19889] [3 19792] [3 19695]]" {
		t.Errorf("IN on an index, descending: %s", got)
	}
}

// エンジンが Close を呼ばずに文を終えても、走査は文の終わりに閉じる（AS OF の走査はトランザクションに属さないので、
// 閉じないと Store.Close が開いたイテレータで失敗する。トランザクションの外の走査は GC の境界を止める）。
func TestStreamingScanClosedAtCommandEnd(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	sess := s.ctx.Session.(*Session)
	streamTable(t, s, 3000)
	sess.CommandEnd()
	s.mustQuery(t, "SET time_zone = '+00:00'")
	ts := mark(t)
	for _, q := range []string{
		"SELECT * FROM m AS OF '" + ts + "'",
		"SELECT * FROM m AS OF '" + ts + "' WHERE id > 10",
		"SELECT * FROM m AS OF '" + ts + "' WHERE k > 10",
		"SELECT * FROM m",
		"SELECT * FROM m WHERE k > 10",
	} {
		_, iter, _, err := s.engine.Query(s.ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if _, err := iter.Next(s.ctx); err != nil {
			t.Fatal(err)
		}
		if len(sess.closers) == 0 {
			t.Fatalf("%s: no scan registered", q)
		}
		sess.CommandEnd() // Close を呼ばずに終える
		if len(sess.closers) != 0 {
			t.Fatalf("%s: %d scans left open after the statement", q, len(sess.closers))
		}
	}
	s.mustQuery(t, "INSERT INTO m (k, v) VALUES (1, 'after')")
	sess.CommandEnd()
	time.Sleep(10 * time.Millisecond)
	stats, err := s.st.GC(time.Microsecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Boundary < s.st.LastCommitted() {
		t.Errorf("GC boundary held back by %v", time.Duration(s.st.LastCommitted()-stats.Boundary)*time.Microsecond)
	}
}

// メモリの上限に近づいたら、文はエラー 1041 で止まり、トランザクションごと取り消される（プロセスは落ちない）。
func TestMemoryLimitStopsStatement(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	defer SetMemoryLimit(0)
	sess := s.ctx.Session.(*Session)
	s.mustQuery(t, "CREATE TABLE big (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, v longtext)")
	s.mustQuery(t, "INSERT INTO big (v) VALUES "+strings.TrimSuffix(strings.Repeat("(REPEAT('x', 200)),", 2000), ","))
	sess.CommandEnd()

	// 生きているヒープで止める経路を見る。バッチが伸びる前の確かめ（AllocCheck。TestCheckAlloc）は、伸びる前に止めて
	// しまうので外す。
	s.st.AllocCheck = nil
	defer func() { s.st.AllocCheck = checkAlloc }()
	runtime.GC()
	// 今の生きているヒープより 40 MB 多いところで止める上限
	SetMemoryLimit(int64(float64(liveHeap()+40<<20) / memAbortRatio))
	s.mustQuery(t, "START TRANSACTION")
	s.mustQuery(t, "INSERT INTO big (id, v) VALUES (1000000, 'kept until the error')")
	var err error
	for i := 0; i < 12 && err == nil; i++ { // 行の数を倍にしていく
		_, err = query(s.ctx, s.engine, "INSERT INTO big (v) SELECT v FROM big")
	}
	if err == nil || !isOutOfMemory(err) {
		t.Fatalf("growing the transaction: %v", err)
	}
	if !strings.Contains(err.Error(), "out of memory") {
		t.Errorf("error: %v", err)
	}
	// 止めたときと同じく、トランザクションが生きている間に GC を回したばかりにする
	memGCMu.Lock()
	runtime.GC()
	memLastGC = time.Now()
	memGCMu.Unlock()
	if live := liveHeap(); live <= int64(float64(MemoryLimit())*memAbortRatio) {
		t.Fatalf("live heap %d MiB is already under the limit before the rollback", live>>20)
	}
	if got := sess.TranslateQueryError(err); !isOutOfMemory(got) {
		t.Errorf("translated error: %v", got)
	}
	// 上限はそのままで、すぐ後の文も止まらない（止めたときの GC の古い値で止めない）
	if err := checkMemory(); err != nil {
		t.Errorf("check right after the rollback: %v", err)
	}
	sess.CommandEnd()

	// トランザクションは取り消され、続く文は新しいトランザクションで動く
	if sess.txn != nil {
		t.Errorf("transaction left open after the out of memory error")
	}
	if got := s.intQuery(t, "SELECT COUNT(*) FROM big"); got != 2000 {
		t.Errorf("rows after the error: %d, want 2000", got)
	}
	sess.CommandEnd()
	SetMemoryLimit(0)
	s.mustQuery(t, "COMMIT")
	if got := s.intQuery(t, "SELECT COUNT(*) FROM big WHERE id = 1000000"); got != 0 {
		t.Errorf("a COMMIT after the error committed the earlier statement")
	}
}

// COMMIT がメモリの上限（コミットの途中の確認）で失敗すると、トランザクションは取り消されて終わり、続く文は自動
// コミットされる（Fable のレビュー。CommitTransaction は先にトランザクションを外すので、取り消しが明示的な
// トランザクションの状態を終えなかった）。
func TestMemoryLimitStopsCommit(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	sess := s.ctx.Session.(*Session)
	s.mustQuery(t, "CREATE TABLE c (id bigint NOT NULL PRIMARY KEY, v varchar(10))")
	sess.CommandEnd()
	s.mustQuery(t, "START TRANSACTION")
	sess.CommandEnd()
	vals := make([]string, 1100) // コミットの確認は 1,024 件ごと
	for i := range vals {
		vals[i] = fmt.Sprintf("(%d, 'v')", i+1)
	}
	s.mustQuery(t, "INSERT INTO c VALUES "+strings.Join(vals, ","))
	sess.CommandEnd()
	s.st.CommitCheck = func() error { return errOutOfMemoryFor(3<<30, 0, 4<<30) }
	_, err := query(s.ctx, s.engine, "COMMIT")
	s.st.CommitCheck = nil
	if !isOutOfMemory(err) {
		t.Fatalf("COMMIT: %v", err)
	}
	if got := sess.TranslateQueryError(err); !isOutOfMemory(got) {
		t.Errorf("translated error: %v", got)
	}
	sess.CommandEnd()
	if sess.txn != nil || sess.GetIgnoreAutoCommit() {
		t.Errorf("explicit transaction left open after the failed COMMIT (txn %v, ignore autocommit %v)", sess.txn != nil, sess.GetIgnoreAutoCommit())
	}
	// 続く文は自動コミットされ、ROLLBACK で消えない
	s.mustQuery(t, "INSERT INTO c VALUES (5000, 'after')")
	sess.CommandEnd()
	s.mustQuery(t, "ROLLBACK")
	sess.CommandEnd()
	if got := s.intQuery(t, "SELECT COUNT(*) FROM c"); got != 1 {
		t.Errorf("rows after the failed COMMIT and an autocommitted INSERT: %d, want 1", got)
	}
}

// コミットの途中の確認は、書き込みの鍵を持っているので GC を回さず、直前の GC の値で判断する（Fable のレビュー）。
func TestCommitMemoryCheckSkipsGC(t *testing.T) {
	defer SetMemoryLimit(0)
	runtime.GC()
	SetMemoryLimit(int64(float64(liveHeap()) / memAbortRatio / 2)) // 生きているヒープが上限の 75% を超える
	defer func() {
		memGCMu.Lock()
		memLastGC = time.Time{}
		memGCMu.Unlock()
	}()
	memoryReleased()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := checkCommitMemory()
	runtime.ReadMemStats(&after)
	if !isOutOfMemory(err) {
		t.Errorf("checkCommitMemory over the limit: %v", err)
	}
	if after.NumForcedGC != before.NumForcedGC {
		t.Errorf("checkCommitMemory forced %d GCs", after.NumForcedGC-before.NumForcedGC)
	}
	// 文の確認は GC を回して確かめる
	runtime.ReadMemStats(&before)
	_ = checkMemory()
	runtime.ReadMemStats(&after)
	if after.NumForcedGC == before.NumForcedGC {
		t.Errorf("checkMemory did not force a GC")
	}
}

func TestParseMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	old := cgroupFiles
	defer func() { cgroupFiles = old }()
	write := func(name, v string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		in     string
		cgroup string
		want   int64
	}{
		{"4G", "", 4 << 30},
		{"512M", "", 512 << 20},
		{"1048576", "", 1 << 20},
		{"64k", "", 64 << 10},
		{"0", "1073741824\n", 0},
		{"", "1073741824\n", 1 << 30 * 8 / 10},
		{"auto", "max\n", 0},
		{"", "9223372036854771712\n", 0}, // cgroup v1 の上限なし
		{"", "", 0},
	} {
		cgroupFiles = []string{dir + "/none"}
		if tc.cgroup != "" {
			cgroupFiles = []string{write("memory.max", tc.cgroup)}
		}
		got, err := ParseMemoryLimit(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseMemoryLimit(%q) with cgroup %q = %d, %v; want %d", tc.in, tc.cgroup, got, err, tc.want)
		}
	}
	for _, bad := range []string{"G", "-1", "4X", "1.5G", "99999999999T"} {
		if _, err := ParseMemoryLimit(bad); err == nil {
			t.Errorf("ParseMemoryLimit(%q) succeeded", bad)
		}
	}
}

// -cache-size を指定しないときのブロックキャッシュは、上限の 8 分の 1 を 16 MiB から既定の間に収めたもの
// （docs/plan.md の段階 58）。
func TestAutoCacheSize(t *testing.T) {
	const def = 128 << 20
	for _, c := range []struct{ limit, want int64 }{
		{0, def},
		{64 << 20, 16 << 20},
		{256 << 20, 32 << 20},
		{1638 << 20, def},
		{16 << 30, def},
	} {
		if got := AutoCacheSize(c.limit, def); got != c.want {
			t.Errorf("AutoCacheSize(%d MiB) = %d MiB, want %d MiB", c.limit>>20, got>>20, c.want>>20)
		}
	}
}

// 起動直後の分、キャッシュ、文の余裕が上限の境目を超えるなら、起動を拒否するエラーに足りる上限が出る。その上限なら通る
// （ホスティング側の報告で、256M と 64M の上限では、キャッシュが満ちた後にどの文も 1041 になった）。
func TestCheckStartupMemory(t *testing.T) {
	defer SetMemoryLimit(0)
	SetMemoryLimit(0)
	if err := CheckStartupMemory(128<<20, false); err != nil {
		t.Errorf("no limit: %v", err)
	}
	SetMemoryLimit(16 << 30)
	if err := CheckStartupMemory(128<<20, false); err != nil {
		t.Errorf("16 GiB: %v", err)
	}
	// キャッシュを数える（16 MiB のキャッシュに 40 MiB の余りがあり、200 MiB のキャッシュには足りない上限）
	runtime.GC()
	SetMemoryLimit(int64(float64(liveHeap()+16<<20+memStartupHeadroom+40<<20) / memAbortRatio))
	if err := CheckStartupMemory(16<<20, false); err != nil {
		t.Errorf("16 MiB cache: %v", err)
	}
	if err := CheckStartupMemory(200<<20, false); err == nil {
		t.Errorf("200 MiB cache passed")
	}
	// 余裕を大きくして、足りる上限でキャッシュが 16 MiB より大きくなるようにする
	defer func(h int64) { memStartupHeadroom = h }(memStartupHeadroom)
	memStartupHeadroom = 200 << 20
	for _, auto := range []bool{false, true} {
		limit := int64(64 << 20)
		cache := int64(64 << 20)
		if auto {
			cache = AutoCacheSize(limit, 128<<20)
		}
		SetMemoryLimit(limit)
		err := CheckStartupMemory(cache, auto)
		if err == nil || !strings.HasPrefix(err.Error(), "HeimDB: the memory limit (-memory-limit) of 64 MiB is too small") {
			t.Fatalf("auto %v: %v", auto, err)
		}
		var want int64
		if _, e := fmt.Sscanf(err.Error()[strings.Index(err.Error(), "at least "):], "at least %d MiB", &want); e != nil {
			t.Fatalf("no limit in %q", err)
		}
		// 足りる上限は、起動直後の分 + キャッシュ + 余裕を境目に収める（キャッシュが上限で変わるときはそれも含める）
		runtime.GC()
		live := liveHeap()
		if auto {
			cache = AutoCacheSize(want<<20, 128<<20)
		}
		if float64(want<<20)*memAbortRatio < float64(live+cache+memStartupHeadroom-1<<20) {
			t.Errorf("auto %v: %d MiB is not enough for %d MiB live, %d MiB cache", auto, want, live>>20, cache>>20)
		}
		SetMemoryLimit((want + 8) << 20) // 生きているヒープの揺れの分
		if err := CheckStartupMemory(cache, auto); err != nil {
			t.Errorf("auto %v: the suggested limit %d MiB fails: %v", auto, want, err)
		}
	}
}

// heimdb_status に、メモリの上限、文を止める境目、生きているヒープ、止めた文の数、キャッシュの大きさと使っている分が
// 出る。止めると数が増える（docs/plan.md の段階 58）。
func TestStatusMemory(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	defer SetMemoryLimit(0)
	status := func() map[string]int64 {
		t.Helper()
		out := map[string]int64{}
		for _, r := range s.mustQuery(t, "CALL heimdb_status()") {
			name := r[0].(string)
			if strings.HasPrefix(name, "memory_") || strings.HasPrefix(name, "cache_") {
				n, err := strconv.ParseInt(r[1].(string), 10, 64)
				if err != nil {
					t.Fatalf("%s = %v", name, r[1])
				}
				out[name] = n
			}
		}
		return out
	}
	SetMemoryLimit(0)
	st := status()
	if len(st) != 6 || st["memory_limit_bytes"] != 0 || st["memory_abort_bytes"] != 0 || st["memory_live_bytes"] <= 0 || st["cache_size_bytes"] != 128<<20 {
		t.Errorf("without a limit: %v", st)
	}
	s.mustQuery(t, "CREATE TABLE c (id bigint NOT NULL PRIMARY KEY, v longtext)")
	s.mustQuery(t, "INSERT INTO c VALUES (1, REPEAT('x', 100000))")
	if err := s.st.Flush(); err != nil { // キャッシュに載るのはファイルから読んだ分
		t.Fatal(err)
	}
	s.intQuery(t, "SELECT LENGTH(v) FROM c")
	if st := status(); st["cache_used_bytes"] <= 0 || st["cache_used_bytes"] > st["cache_size_bytes"] {
		t.Errorf("cache after a read: %v", st)
	}

	SetMemoryLimit(1 << 40)
	before := status()
	if before["memory_limit_bytes"] != 1<<40 || before["memory_abort_bytes"] != int64(float64(1<<40)*memAbortRatio) {
		t.Errorf("with a limit: %v", before)
	}
	SetMemoryLimit(1 << 20)
	if err := checkMemory(); !isOutOfMemory(err) {
		t.Fatalf("1 MiB limit: %v", err)
	}
	SetMemoryLimit(1 << 40)
	if after := status(); after["memory_aborted_statements"] != before["memory_aborted_statements"]+1 {
		t.Errorf("aborted statements: %d, then %d", before["memory_aborted_statements"], after["memory_aborted_statements"])
	}
}
