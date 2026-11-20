package store

import (
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// 書き込みはバッチだけに持ち、文の取り消しとセーブポイントはバッチの先頭から読み直して作る（docs/plan.md の段階 59）。

// 同じキーを 2 回書いた間の位置に戻すと、最初の値が見える（作り直しは、キーごとの最新ではなく、すべての書き込みを
// 順に写す）。
func TestRollbackToBetweenWritesOfSameKey(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	defer tx.Rollback()
	mustPut(t, tx, 1, "a")
	m := tx.Mark()
	mustPut(t, tx, 1, "b")
	mustPut(t, tx, 2, "c")
	if err := tx.RollbackTo(m); err != nil {
		t.Fatal(err)
	}
	expectRows(t, tx, "1=a")
	mustPut(t, tx, 3, "d")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = s.Begin()
	defer tx.Rollback()
	expectRows(t, tx, "1=a", "3=d")
}

// マージの記録の削除（DropMergeBasesAfter）は、取り消した位置より前のものだけがコミットで消える。
func TestDropMergeBasesAfterRollbackTo(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	dev, err := s.CreateBranch("dev", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateBranch("other", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := s.LastCommitted()
	for _, b := range []uint64{dev.ID, other.ID} {
		writeOn(t, s, MainBranchID, func(tx *Txn) {
			if err := tx.SetMergeBase(b, s.LastCommitted()); err != nil {
				t.Fatal(err)
			}
		})
	}
	devBase, _ := s.MergeBase(dev.ID)
	tx := s.Begin()
	mustPut(t, tx, 1, "x")
	m := tx.Mark()
	if err := tx.DropMergeBasesAfter(MainBranchID, before); err != nil { // dev と other の記録を消す
		t.Fatal(err)
	}
	mustPut(t, tx, 2, "y")
	if err := tx.RollbackTo(m); err != nil { // 消したことを取り消す
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.MergeBase(dev.ID); got.ParentTS != devBase.ParentTS {
		t.Errorf("a merge record dropped after the savepoint survived the rollback: %d, want %d", got.ParentTS, devBase.ParentTS)
	}

	tx = s.Begin()
	if err := tx.DropMergeBasesAfter(MainBranchID, before); err != nil {
		t.Fatal(err)
	}
	m = tx.Mark()
	mustPut(t, tx, 3, "z")
	if err := tx.RollbackTo(m); err != nil { // 消したことは残る
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.MergeBase(dev.ID); got.ParentTS != dev.ForkedAt {
		t.Errorf("merge record kept: %d, want the fork %d", got.ParentTS, dev.ForkedAt)
	}
	tx = s.Begin()
	defer tx.Rollback()
	expectRows(t, tx, "1=x")
}

// 文の取り消しの前に開いた走査は、取り消しの後も読み続けられる（古いバッチをトランザクションの終わりまで閉じない）。
func TestScanSurvivesDiscardStatement(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	defer tx.Rollback()
	for i := int64(1); i <= 200; i++ {
		mustPut(t, tx, i, strings.Repeat("v", 100))
	}
	tx.StatementBegin()
	mustPut(t, tx, 500, "gone")
	sc, err := tx.RowScan(MainBranchID, tbl, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := sc.Next(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := tx.DiscardStatement(); err != nil {
		t.Fatal(err)
	}
	// 取り消した後の書き込みで、使わなくなったバッチの領域を使い回させる
	for i := int64(1000); i < 1200; i++ {
		mustPut(t, tx, i, strings.Repeat("w", 100))
	}
	n := 1
	for {
		entry, value, ok, err := sc.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if v := string(value); v != strings.Repeat("v", 100) && v != "gone" {
			t.Fatalf("row %x read %q after the discard", entry, v)
		}
		n++
	}
	if n != 201 {
		t.Errorf("the scan read %d rows, want 201 (as of when it opened)", n)
	}
	sc.Close()
	if got := len(rows(t, tx, false)); got != 400 {
		t.Errorf("rows after the discard: %d", got)
	}
}

// 書き込み、文の始めと取り消し、セーブポイントとそこへの取り消しをランダムに並べ、map のモデルと突き合わせる
// （件数で数える位置の 1 つのずれも見つける）。
func TestTxnBatchMatchesModel(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	rng := rand.New(rand.NewSource(59))
	for round := 0; round < 30; round++ {
		tx := s.Begin()
		// 確定済みの行から始める
		committed := map[int64]string{}
		_ = tx.ScanRows(MainBranchID, tbl, nil, nil, false, func(r RowVersion) (bool, error) {
			n, _, _ := keyenc.DecodeInt64(r.PK)
			committed[n] = string(r.Value)
			return true, nil
		})
		model := maps(committed)
		var stmt map[int64]string
		stmtMarks := 0 // 文の始めのセーブポイントの数（取り消した文の中のセーブポイントは無効になる）
		type mark struct {
			pos   int
			state map[int64]string
		}
		var marks []mark
		for step := 0; step < 200; step++ {
			switch op := rng.Intn(10); {
			case op < 5:
				n, v := int64(rng.Intn(20)), fmt.Sprintf("r%d-%d", round, step)
				mustPut(t, tx, n, v)
				model[n] = v
			case op < 6:
				n := int64(rng.Intn(20))
				if err := tx.DeleteRow(MainBranchID, tbl, pk(n)); err != nil {
					t.Fatal(err)
				}
				delete(model, n)
			case op < 7 && stmt == nil:
				tx.StatementBegin()
				stmt = maps(model)
				stmtMarks = len(marks)
			case op < 8 && stmt != nil:
				if rng.Intn(2) == 0 {
					if err := tx.DiscardStatement(); err != nil {
						t.Fatal(err)
					}
					model = stmt
					marks = marks[:stmtMarks]
				} else {
					tx.StatementComplete()
				}
				stmt = nil
			case op < 9:
				marks = append(marks, mark{tx.Mark(), maps(model)})
			default:
				if len(marks) == 0 || stmt != nil {
					continue
				}
				i := rng.Intn(len(marks))
				if err := tx.RollbackTo(marks[i].pos); err != nil {
					t.Fatal(err)
				}
				model = maps(marks[i].state)
				marks = marks[:i+1]
			}
			if got, want := rows(t, tx, false), modelRows(model); !slices.Equal(got, want) {
				t.Fatalf("round %d step %d: rows %v, want %v", round, step, got, want)
			}
		}
		if stmt != nil {
			tx.StatementComplete()
		}
		if rng.Intn(3) == 0 {
			tx.Rollback()
			model = committed
		} else if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx = s.Begin()
		if got, want := rows(t, tx, false), modelRows(model); !slices.Equal(got, want) {
			t.Fatalf("round %d after commit: rows %v, want %v", round, got, want)
		}
		tx.Rollback()
	}
}

func maps(m map[int64]string) map[int64]string {
	out := make(map[int64]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func modelRows(m map[int64]string) []string {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%d=%s", k, m[k]))
	}
	return out
}

// liveHeap は生きているヒープを返す。GC を 2 回して、Pebble が使い回すために sync.Pool に置いたバッチ（前のテストで
// 閉じたもの）を数えないようにする（1 回目の GC では Pool の控えに残る）。
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64())
}

// コミットしていない書き込みのメモリは、ほぼバッチの大きさ（Pebble のバッチの本体と並びの索引）で、コミットの最中の
// 山もバッチの数倍に収まる（ホスティングの取り込みと同じ形: 行と 2 つのインデックスのエントリ）。直す前は 1 行 974 バイト、
// 山はバッチの 5.6 倍だった。山の残りは、倍々に伸びたバッチの余りと、コミットのバッチ（記録を残すので中身を 2 回持つ）。
func TestTxnMemoryPerRow(t *testing.T) {
	defer func(n int) { LogChunkBytes = n }(LogChunkBytes)
	LogChunkBytes = 1 << 20 // 小さく分けると、部分ごとのキーの分だけコミットのバッチが大きくなる
	s, _ := openTemp(t)
	defer s.Close()
	const rows = 50000
	base := liveHeap()
	tx := s.Begin()
	value := []byte(strings.Repeat("x", 74))
	for i := int64(1); i <= rows; i++ {
		if err := tx.InsertRow(MainBranchID, tbl, pk(i), value); err != nil {
			t.Fatal(err)
		}
		for idx := uint64(1); idx <= 2; idx++ {
			entry := append(keyenc.AppendInt64(nil, i%5000), pk(i)...)
			if err := tx.PutIndexEntry(MainBranchID, tbl, idx, entry, pk(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	size := int64(tx.Size())
	held := liveHeap() - base
	if per := held / rows; per > 700 {
		t.Errorf("uncommitted writes hold %d bytes per row (batch %d bytes per row)", per, size/rows)
	}
	var peak int64
	s.CommitCheck = func() error {
		peak = max(peak, liveHeap()-base)
		return nil
	}
	testHookBeforeCommit = func(*pebble.Batch, int) { peak = max(peak, liveHeap()-base) }
	defer func() { s.CommitCheck, testHookBeforeCommit = nil, nil }()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if peak == 0 || float64(peak) > 4.5*float64(size) {
		t.Errorf("commit peak %d MiB for a %d MiB batch", peak>>20, size>>20)
	}
}

// コミットしていない書き込みが MaxTxnBytes を超えると 1197 を返し、それまでの書き込みは残る（文の取り消しは呼び出し側）。
func TestTxnTooLarge(t *testing.T) {
	defer func(n int) { MaxTxnBytes = n }(MaxTxnBytes)
	MaxTxnBytes = 64 << 10
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	defer tx.Rollback()
	tx.StatementBegin()
	var err error
	n := int64(0)
	for err == nil && n < 10000 {
		n++
		err = tx.PutRow(MainBranchID, tbl, pk(n), []byte(strings.Repeat("x", 100)))
	}
	if !IsTxnTooLarge(err) || !strings.Contains(err.Error(), "--skip-no-autocommit") {
		t.Fatalf("after %d rows: %v", n, err)
	}
	if tx.Size() > MaxTxnBytes {
		t.Errorf("batch %d bytes over the limit %d", tx.Size(), MaxTxnBytes)
	}
	if err := tx.DiscardStatement(); err != nil {
		t.Fatal(err)
	}
	expectRows(t, tx)
	mustPut(t, tx, 1, "ok")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// 上限で止まった文を同じトランザクションで繰り返しても、取り消したバッチは次のコマンドの始め（ResetStatement）で
// 手放し、メモリは溜まらない。取り消しの写し先は要る大きさで確保する（倍々に伸ばさない）。
func TestTxnTooLargeRepeated(t *testing.T) {
	defer func(n int) { MaxTxnBytes = n }(MaxTxnBytes)
	MaxTxnBytes = 4700 << 10 // 写す分（その 9 割）が 4 MiB を少し超え、倍々に伸ばすと 8 MiB になる
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	defer tx.Rollback()
	value := []byte(strings.Repeat("x", 100))
	n := int64(0)
	fill := func() error {
		for {
			n++
			if err := tx.PutRow(MainBranchID, tbl, pk(n), value); err != nil {
				return err
			}
		}
	}
	// 上限の 9 割まで書いてから、止まる文を繰り返す
	for tx.Size() < MaxTxnBytes*9/10 {
		mustPut(t, tx, n+1, string(value))
		n++
	}
	size := int64(tx.Size())
	base := liveHeap()
	for round := 0; round < 5; round++ {
		tx.ResetStatement()
		tx.StatementBegin()
		if err := fill(); !IsTxnTooLarge(err) {
			t.Fatal(err)
		}
		before := liveHeap()
		if err := tx.DiscardStatement(); err != nil {
			t.Fatal(err)
		}
		// 写し先は写した分（size）とその索引だけ増える。倍々に伸ばすと 8 MiB（size の 1.9 倍）を確保する
		if grew := liveHeap() - before; float64(grew) > 1.7*float64(size) {
			t.Errorf("round %d: discarding grew the heap by %d KiB for a %d KiB batch", round, grew>>10, size>>10)
		}
		if tx.Size() != int(size) {
			t.Fatalf("round %d: batch %d bytes after the discard, want %d", round, tx.Size(), size)
		}
	}
	tx.ResetStatement()
	if len(tx.retired) != 0 {
		t.Errorf("%d discarded batches kept", len(tx.retired))
	}
	if held := liveHeap() - base; float64(held) > 1.5*float64(size) {
		t.Errorf("the heap grew by %d KiB over 5 refused statements (batch %d KiB)", held>>10, size>>10)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// コミットのバッチは、トランザクションの書き込みと記録に加えて、保存していない AUTO_INCREMENT のカウンタ（すべての
// テーブルの分）も入れる。その分も先に確保し、バッチが倍々に伸びない（多くのテーブルに取り込んだ後の大きなコミットで、
// 2 倍を確保しない）。
func TestCommitBatchFitsAutoIncrements(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	for i := uint64(0); i < 3000; i++ {
		c, err := s.AutoIncrement(MainBranchID, 1000+i, 1, math.MaxUint64, func() (uint64, error) { return 1, nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Next(); err != nil {
			t.Fatal(err)
		}
	}
	tx := s.Begin()
	for i := int64(1); i <= 1000; i++ {
		mustPut(t, tx, i, strings.Repeat("x", 100))
	}
	checked := false
	testHookBeforeCommit = func(b *pebble.Batch, presized int) {
		checked = true
		if got := cap(b.Repr()); got != presized {
			t.Errorf("the commit batch grew to %d bytes from %d", got, presized)
		}
	}
	defer func() { testHookBeforeCommit = nil }()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Error("the hook was not called")
	}
}

// コミットのバッチを先に確保する前と、トランザクションのバッチが倍に伸びる前に、確保する大きさで AllocCheck を呼び、
// エラーなら確保せずに返す（docs/plan.md の段階 59。GCE の 310 万行の復元で、一度に確保したバッチでメモリの上限を
// 確かめる前に cgroup に落とされた）。
func TestAllocCheck(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	refuse := fmt.Errorf("refused")
	var asked []int
	limit := 1 << 62
	s.AllocCheck = func(n int) error {
		asked = append(asked, n)
		if n > limit {
			return refuse
		}
		return nil
	}
	tx := s.Begin()
	defer tx.Rollback()
	value := []byte(strings.Repeat("x", 100))
	n := int64(0)
	for tx.Size() < 3<<20 {
		n++
		mustPut(t, tx, n, string(value))
	}
	// 1 MiB 以上のバッチが伸びるたびに、倍の大きさを尋ねる
	if len(asked) == 0 || asked[0] < 2<<20 {
		t.Fatalf("asked %v while the batch grew to %d bytes", asked, tx.Size())
	}
	// 次に伸びるところで拒否すると、拒否した書き込みは入らない
	limit = asked[len(asked)-1]
	var err error
	for err == nil {
		n++
		err = tx.PutRow(MainBranchID, tbl, pk(n), value)
	}
	if err != refuse {
		t.Fatal(err)
	}
	if got := len(rows(t, tx, false)); got != int(n-1) {
		t.Fatalf("rows %d after the refused write of row %d", got, n)
	}
	// コミットのバッチ（中身の 2 倍。記録を残すので）を確保する前に拒否すると、何も書かない
	limit = tx.Size()
	if err := tx.Commit(); err != refuse {
		t.Fatalf("commit: %v", err)
	}
	if last := asked[len(asked)-1]; last < 2*limit {
		t.Errorf("the commit asked for %d bytes for a %d byte batch", last, limit)
	}
	s.AllocCheck = nil
	tx = s.Begin()
	defer tx.Rollback()
	expectRows(t, tx)
}

// 大きなコミットの後、Pebble が持つコミットのバッチ（書き出すまで中身ごと持つ）は、次の書き込みを待たずに手放す
// （docs/plan.md の段階 59。手放さないと、100 万行の復元の後に 1 GB 余りを持ち続けた）。
func TestLargeCommitReleased(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	base := liveHeap()
	tx := s.Begin()
	value := []byte(strings.Repeat("x", 200))
	for i := int64(1); i <= 400000; i++ {
		if err := tx.PutRow(MainBranchID, tbl, pk(i), value); err != nil {
			t.Fatal(err)
		}
	}
	size := int64(tx.Size())
	if size < largeCommitFlush {
		t.Fatalf("batch %d bytes is too small", size)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 書き出しは非同期。CI（2 コア、-race、ほかのパッケージと並ぶ）では 10 秒を超えたことがある
	held := liveHeap() - base
	for deadline := time.Now().Add(30 * time.Second); held > size/4 && time.Now().Before(deadline); held = liveHeap() - base {
		time.Sleep(100 * time.Millisecond)
	}
	if held > size/4 {
		t.Errorf("%d MiB still held after committing a %d MiB batch", held>>20, size>>20)
	}
}

// 文の取り消しで写す分の確保も AllocCheck で確かめ、拒否されたら写さずにトランザクションごと取り消す（docs/plan.md の
// 段階 59。1041 で止めた文の取り消しは、この後トランザクションごと捨てるので、写すと無駄に確保する）。
func TestTruncateAllocCheck(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	tx := s.Begin()
	defer tx.Rollback()
	value := strings.Repeat("x", 100)
	n := int64(0)
	for tx.Size() < 2<<20 {
		n++
		mustPut(t, tx, n, value)
	}
	tx.StatementBegin()
	for i := 0; i < 10; i++ {
		n++
		mustPut(t, tx, n, value)
	}
	refuse := fmt.Errorf("refused")
	var asked int
	s.AllocCheck = func(size int) error {
		asked = size
		return refuse
	}
	if err := tx.DiscardStatement(); err != refuse {
		t.Fatalf("discard: %v", err)
	}
	if asked < 2<<20 || asked > 3<<20 { // 写す分（文の前までの 2 MiB 余り）
		t.Errorf("asked for %d bytes", asked)
	}
	if err := tx.PutRow(MainBranchID, tbl, pk(n+1), []byte(value)); err != ErrTxnDone {
		t.Errorf("write after the refused discard: %v", err)
	}
	s.AllocCheck = nil
	tx = s.Begin()
	defer tx.Rollback()
	expectRows(t, tx)
}
