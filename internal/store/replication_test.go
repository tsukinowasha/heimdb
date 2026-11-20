package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"
)

// dumpKeys は、そのマシンだけのキー（m/gc、m/format、m/logtrim）を除いたすべてのキーと値を返す。
func dumpKeys(t *testing.T, s *Store) map[string]string {
	t.Helper()
	iter, err := s.db.NewIter(&pebble.IterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()
	out := map[string]string{}
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if bytes.Equal(k, gcKey()) || bytes.Equal(k, formatKey()) || bytes.Equal(k, logTrimKey()) {
			continue
		}
		out[string(k)] = string(iter.Value())
	}
	return out
}

func writeRows(t *testing.T, s *Store, branch uint64, from, to int) {
	t.Helper()
	tx := s.Begin()
	if err := tx.LockForWrite(branch); err != nil {
		t.Fatal(err)
	}
	for i := from; i < to; i++ {
		if err := tx.PutRow(branch, 10, []byte(fmt.Sprintf("pk%06d", i)), []byte{valData, byte(i)}); err != nil {
			t.Fatal(err)
		}
		if err := tx.PutIndexEntry(branch, 10, 1, []byte(fmt.Sprintf("ix%06d", i)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if i := from; i > 0 {
		if err := tx.DeleteRow(branch, 10, []byte(fmt.Sprintf("pk%06d", i-1))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// catchUp は、レプリカ r を本番 p の記録で追いつかせる。
func catchUp(t *testing.T, p, r *Store) Applied {
	t.Helper()
	var last Applied
	for r.LastCommitted() < p.LastCommitted() {
		recs, err := p.ReadRecords(r.LastCommitted(), 100)
		if err != nil {
			t.Fatal(err)
		}
		r.OnApply(func(a Applied) { last = a })
		if err := r.ApplyRecords(recs); err != nil {
			t.Fatal(err)
		}
	}
	return last
}

// バックアップから始めたレプリカは、記録を適用すると、本番とすべてのキーが同じになる（ブランチ、行、インデックス、
// ユーザー、記録そのものを含む）。止めて開き直しても、途中から追いつく（docs/plan.md の段階 24）。
func TestReplicaFollowsPrimary(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	writeRows(t, p, MainBranchID, 0, 50)
	if err := p.SetUsers([]byte("users-1")); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	at, err := p.Checkpoint(backup)
	if err != nil {
		t.Fatal(err)
	}
	// バックアップの後の書き込み
	child, err := p.CreateBranch("child", MainBranchID, 0)
	if err != nil {
		t.Fatal(err)
	}
	writeRows(t, p, child.ID, 50, 80)
	writeRows(t, p, MainBranchID, 80, 90)
	if err := p.SetUsers([]byte("users-2")); err != nil {
		t.Fatal(err)
	}

	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.LastCommitted() != at {
		t.Fatalf("replica position %d, backup %d", r.LastCommitted(), at)
	}
	a := catchUp(t, p, r)
	if !a.Users || a.Through != p.LastCommitted() {
		t.Errorf("applied: %+v", a)
	}
	if _, ok := r.BranchByName("child"); !ok {
		t.Error("the replica does not see the new branch")
	}
	if got, want := dumpKeys(t, r), dumpKeys(t, p); !mapsEqual(got, want) {
		t.Errorf("replica has %d keys, primary %d", len(got), len(want))
	}

	// 止めて、さらに書いてから開き直す
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	writeRows(t, p, child.ID, 90, 120)
	if err := p.DeleteBranch("child"); err != nil {
		t.Fatal(err)
	}
	r, err = OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	catchUp(t, p, r)
	if _, ok := r.BranchByName("child"); ok {
		t.Error("the replica still sees the deleted branch")
	}
	if got, want := dumpKeys(t, r), dumpKeys(t, p); !mapsEqual(got, want) {
		t.Errorf("after restart: replica has %d keys, primary %d", len(got), len(want))
	}

	// レプリカは書けない
	if err := r.SetUsers([]byte("x")); !errors.Is(err, ErrReadOnlyReplica) {
		t.Errorf("write on a replica: %v", err)
	}
	if _, err := r.CreateBranch("x", MainBranchID, 0); !errors.Is(err, ErrReadOnlyReplica) {
		t.Errorf("branch on a replica: %v", err)
	}
	// 同じ記録を 2 度は適用しない
	recs, err := p.ReadRecords(at, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyRecords(recs); err == nil {
		t.Error("an old record was applied again")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// 記録にない位置（違うデータ、消えた記録）は ErrRecordGone。空のデータはレプリカとして開かない。
func TestReplicaPositionChecks(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	writeRows(t, p, MainBranchID, 0, 3)
	if _, err := p.ReadRecords(p.LastCommitted()-1, 100); !errors.Is(err, ErrRecordGone) {
		t.Errorf("unknown position: %v", err)
	}
	if recs, err := p.ReadRecords(p.LastCommitted(), 100); err != nil || len(recs) != 0 {
		t.Errorf("at the newest record: %d records, %v", len(recs), err)
	}
	if _, err := OpenWith(t.TempDir(), Options{Replica: true}); err == nil {
		t.Error("an empty replica was opened")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenWith(missing, Options{Replica: true}); err == nil {
		t.Error("a replica was opened on a missing directory")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the replica created its missing directory: %v", err)
	}
	n, err := OpenWith(t.TempDir(), Options{NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	writeRows(t, n, MainBranchID, 0, 3)
	if _, err := n.ReadRecords(0, 100); !errors.Is(err, ErrNoLog) {
		t.Errorf("without a log: %v", err)
	}
	if lo, hi, err := n.LogRange(); err != nil || lo != 0 || hi != 0 {
		t.Errorf("log range without a log: %d %d %v", lo, hi, err)
	}
}

// 保持期間を過ぎた記録は消えるが、最新の記録は残る。
func TestTrimLog(t *testing.T) {
	p, err := OpenWith(t.TempDir(), Options{LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 5; i++ {
		writeRows(t, p, MainBranchID, i*10, i*10+10)
	}
	oldest, newest, err := p.LogRange()
	if err != nil || oldest == 0 || newest != p.LastCommitted() {
		t.Fatalf("log range %d %d %v", oldest, newest, err)
	}
	if n, err := p.TrimLog(); err != nil || n != 0 {
		t.Errorf("trim within retention: %d %v", n, err)
	}
	p.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	n, err := p.TrimLog()
	if err != nil || n < 5 {
		t.Errorf("trim: %d %v", n, err)
	}
	if lo, hi, err := p.LogRange(); err != nil || lo != newest || hi != newest {
		t.Errorf("after trim: %d %d %v (newest %d)", lo, hi, err, newest)
	}
	if _, err := p.ReadRecords(newest, 100); err != nil {
		t.Errorf("the newest position: %v", err)
	}
	if _, err := p.ReadRecords(oldest, 100); !errors.Is(err, ErrRecordGone) {
		t.Errorf("a trimmed position: %v", err)
	}
}

// 記録を残す前のデータで起動して、まだ書いていなくても、バックアップの位置の記録が本番にある。
func TestCheckpointWithoutRecord(t *testing.T) {
	dir := t.TempDir()
	old, err := OpenWith(dir, Options{NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	writeRows(t, old, MainBranchID, 0, 3)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	at, err := p.Checkpoint(backup)
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	writeRows(t, p, MainBranchID, 3, 6)
	if r.LastCommitted() != at {
		t.Fatalf("replica position %d, backup %d", r.LastCommitted(), at)
	}
	catchUp(t, p, r)
	if got, want := dumpKeys(t, r), dumpKeys(t, p); !mapsEqual(got, want) {
		t.Errorf("replica has %d keys, primary %d", len(got), len(want))
	}
}

// レプリカも自分で GC する。GC の境界はそのマシンだけのもので、記録に入らず（本番の GC はコミットにならない）、
// レプリカの境界は本番の境界に上書きされない（開き直しても）。
func TestReplicaGC(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	writeRows(t, p, MainBranchID, 0, 50)
	backup := filepath.Join(t.TempDir(), "backup")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	writeRows(t, p, MainBranchID, 0, 50) // 同じ行を書き直す（古いバージョンができる）
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	catchUp(t, p, r)

	before := p.LastCommitted()
	if _, err := p.GC(time.Hour); err != nil {
		t.Fatal(err)
	}
	if p.LastCommitted() != before {
		t.Error("GC on the primary made a commit")
	}
	if recs, err := p.ReadRecords(before, 1<<20); err != nil || len(recs) != 0 {
		t.Errorf("records after GC on the primary: %d, %v", len(recs), err)
	}

	r.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	stats, err := r.GC(time.Hour)
	if err != nil {
		t.Fatalf("GC on the replica: %v", err)
	}
	if stats.Versions == 0 {
		t.Errorf("GC on the replica removed nothing: %+v", stats)
	}
	rb := r.GCBoundary()
	if rb != r.LastCommitted() {
		t.Errorf("replica boundary %d, last commit %d", rb, r.LastCommitted())
	}

	// 本番で書いて、本番の境界をもっと先にしても、レプリカの境界は変わらない
	writeRows(t, p, MainBranchID, 50, 60)
	p.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := p.GC(time.Hour); err != nil {
		t.Fatal(err)
	}
	if p.GCBoundary() <= rb {
		t.Fatalf("primary boundary %d, replica %d", p.GCBoundary(), rb)
	}
	catchUp(t, p, r)
	if r.GCBoundary() != rb {
		t.Errorf("replica boundary after applying: %d, want %d", r.GCBoundary(), rb)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.GCBoundary() != rb {
		t.Errorf("replica boundary after reopening: %d, want %d", r.GCBoundary(), rb)
	}
}

// 長く書き込みがなかった後の 1 回の書き込みで、追いついていたレプリカの位置の記録が消えても、その位置から続けられる。
func TestTrimLogKeepsCaughtUpPosition(t *testing.T) {
	p, err := OpenWith(t.TempDir(), Options{LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			p.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		writeRows(t, p, MainBranchID, i*10, i*10+10)
	}
	oldest, pos, err := p.LogRange()
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	writeRows(t, p, MainBranchID, 30, 40)
	if n, err := p.TrimLog(); err != nil || n < 3 {
		t.Fatalf("trim: %d %v", n, err)
	}
	if lo, _, _ := p.LogRange(); lo != p.LastCommitted() {
		t.Fatalf("the record at %d was not trimmed (oldest %d)", pos, lo)
	}
	recs, err := p.ReadRecords(pos, 1<<20)
	if err != nil || len(recs) != 1 || recs[0].TS != p.LastCommitted() {
		t.Errorf("from the last trimmed position: %d records, %v", len(recs), err)
	}
	if _, err := p.ReadRecords(oldest, 1<<20); !errors.Is(err, ErrRecordGone) {
		t.Errorf("an older trimmed position: %v", err)
	}
	if _, err := p.ReadRecords(pos-1, 1<<20); !errors.Is(err, ErrRecordGone) {
		t.Errorf("a position that was never a record: %v", err)
	}
	// 閉じた後の削除はエラー（panic しない）
	closed = true
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TrimLog(); err == nil {
		t.Error("trim after close")
	}
}

// 記録を読む間に古い記録を消しても、記録を飛ばして返さない（続きを返すか、ErrRecordGone）。
func TestReadRecordsDuringTrim(t *testing.T) {
	p, err := OpenWith(t.TempDir(), Options{LogRetention: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const commits = 300
	var mu sync.Mutex
	var order []uint64 // コミットした順の時刻
	type read struct{ after, first uint64 }
	var reads []read
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := p.TrimLog(); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			mu.Lock()
			n := len(order)
			after := uint64(0)
			if n > 0 {
				after = order[n-1-min(n-1, 2)]
			}
			mu.Unlock()
			recs, err := p.ReadRecords(after, 1<<20)
			if err == nil && len(recs) > 0 {
				mu.Lock()
				reads = append(reads, read{after, recs[0].TS})
				mu.Unlock()
			} else if err != nil && !errors.Is(err, ErrRecordGone) {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < commits; i++ {
		writeRows(t, p, MainBranchID, i, i+1)
		mu.Lock()
		order = append(order, p.LastCommitted())
		mu.Unlock()
	}
	close(done)
	wg.Wait()
	next := map[uint64]uint64{}
	for i := 1; i < len(order); i++ {
		next[order[i-1]] = order[i]
	}
	for _, r := range reads {
		if want, ok := next[r.after]; ok && r.first != want {
			t.Fatalf("after %d: first record %d, want %d (records were skipped)", r.after, r.first, want)
		}
	}
	t.Logf("%d reads", len(reads))
}

// 適用が途中で失敗すると、それまでの記録は適用されたまま残り（開き直しても）、その位置から続きを適用できる。
func TestApplyRecordsResumesAfterFailure(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	writeRows(t, p, MainBranchID, 0, 10)
	backup := filepath.Join(t.TempDir(), "backup")
	at, err := p.Checkpoint(backup)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		writeRows(t, p, MainBranchID, i*10, i*10+10)
	}
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := p.ReadRecords(at, 1<<20)
	if err != nil || len(recs) != 3 {
		t.Fatalf("records: %d %v", len(recs), err)
	}
	bad := append([]Record(nil), recs...)
	bad[1].TS++ // 記録の中のコミット時刻と合わない
	if err := r.ApplyRecords(bad); err == nil {
		t.Fatal("a corrupt record was applied")
	}
	if r.LastCommitted() != recs[0].TS {
		t.Fatalf("position after the failure: %d, want %d", r.LastCommitted(), recs[0].TS)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.LastCommitted() != recs[0].TS {
		t.Fatalf("position after reopening: %d, want %d", r.LastCommitted(), recs[0].TS)
	}
	catchUp(t, p, r)
	if got, want := dumpKeys(t, r), dumpKeys(t, p); !mapsEqual(got, want) {
		t.Errorf("replica has %d keys, primary %d", len(got), len(want))
	}
}

// レプリカの位置は、記録の適用でメモリの内容（ブランチの一覧など）を作り直し、適用の通知を終えてから進む。位置が
// 本番に追いついたのに、本番で作ったブランチが見えない間がない（docs/plan.md の段階 30）。
func TestReplicaPositionAfterRefresh(t *testing.T) {
	p, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	at, err := p.Checkpoint(backup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateBranch("x", MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	r, err := OpenWith(backup, Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recs, err := p.ReadRecords(at, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	inApply, release := make(chan Applied), make(chan struct{})
	r.OnApply(func(a Applied) {
		inApply <- a
		<-release
	})
	done := make(chan error, 1)
	go func() { done <- r.ApplyRecords(recs) }()
	a := <-inApply
	if _, ok := r.BranchByName("x"); !ok {
		t.Error("the branch is not loaded when the apply is notified")
	}
	if a.Through != p.LastCommitted() {
		t.Errorf("notified through %d, want %d", a.Through, p.LastCommitted())
	}
	if got := r.LastCommitted(); got != at {
		t.Errorf("position %d before the apply is notified, want %d (unchanged)", got, at)
	}
	// 書いた位置（記録の配信と WaitCommit が使う）は、適用したときに進めておく
	if got := r.Written(); got != p.LastCommitted() {
		t.Errorf("written %d when the apply is notified, want %d", got, p.LastCommitted())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := r.LastCommitted(); got != p.LastCommitted() {
		t.Errorf("position %d after the apply, want %d", got, p.LastCommitted())
	}
}

// 大きな記録は LogChunkBytes ずつに分けて書き（どの値も LogChunkBytes 以下）、読むときにつなぐ。レプリカも同じように
// 分けて書くので、すべてのキーが本番と同じになる。記録の区切りでだけ止め、位置の記録の続きの部分は飛ばし、消した数は
// 記録の数で数える（docs/plan.md の段階 59）。
func TestChunkedRecords(t *testing.T) {
	defer func(n int) { LogChunkBytes = n }(LogChunkBytes)
	LogChunkBytes = 100
	p, err := OpenWith(t.TempDir(), Options{LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	at, err := p.Checkpoint(backup)
	if err != nil {
		t.Fatal(err)
	}
	writeRows(t, p, MainBranchID, 0, 50)
	big := p.LastCommitted()
	writeRows(t, p, MainBranchID, 50, 51) // 小さな記録（1 つの部分）
	writeRows(t, p, MainBranchID, 51, 80)

	parts, maxValue := 0, 0
	iter, err := p.db.NewIter(&pebble.IterOptions{LowerBound: logKey(big), UpperBound: logKey(big + 1)})
	if err != nil {
		t.Fatal(err)
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		parts++
		maxValue = max(maxValue, len(iter.Value()))
	}
	iter.Close()
	if parts < 10 || maxValue > LogChunkBytes {
		t.Fatalf("the big record has %d parts of up to %d bytes", parts, maxValue)
	}
	recs, err := p.ReadRecords(at, 1)
	if err != nil || len(recs) != 1 || recs[0].TS != big {
		t.Fatalf("records after the backup: %d %v", len(recs), err)
	}
	if n := len(recs[0].Data); n <= (parts-1)*LogChunkBytes {
		t.Errorf("the big record read back as %d bytes from %d parts", n, parts)
	}
	b := p.db.NewBatch()
	if err := b.SetRepr(bytes.Clone(recs[0].Data)); err != nil { // バッチは渡した領域を閉じるときに書き換える
		t.Fatalf("the joined record is not a batch: %v", err)
	}
	b.Close()
	// 位置が大きな記録なら、その続きの部分を飛ばして次の記録から返す
	if recs, err := p.ReadRecords(big, 1<<20); err != nil || len(recs) != 2 || recs[0].TS <= big {
		t.Fatalf("records after the big one: %d %v", len(recs), err)
	}

	r, err := OpenWith(backup, Options{Replica: true, LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	catchUp(t, p, r)
	if got, want := dumpKeys(t, r), dumpKeys(t, p); !mapsEqual(got, want) {
		t.Errorf("replica has %d keys, primary %d", len(got), len(want))
	}
	if rr, err := r.ReadRecords(at, 1<<20); err != nil || len(rr) != 3 || !bytes.Equal(rr[0].Data, recs[0].Data) {
		t.Errorf("the replica's records: %d %v", len(rr), err)
	}

	p.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if n, err := p.TrimLog(); err != nil || n != 3 { // 記録を残す前のデータの位置の記録と、4 つのうち最新を除く 3 つ
		_, newest, _ := p.LogRange()
		t.Errorf("trimmed %d records (%v), newest %d", n, err, newest)
	}
}

// 分けずに書いた大きな記録（バージョン 2 のデータ）も、そのまま 1 つの記録として読む。
func TestUnchunkedRecord(t *testing.T) {
	defer func(n int) { LogChunkBytes = n }(LogChunkBytes)
	p, err := OpenWith(t.TempDir(), Options{LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	before := p.LastCommitted()
	writeRows(t, p, MainBranchID, 0, 50)
	old, err := p.ReadRecords(before, 1<<20)
	if err != nil || len(old) == 0 {
		t.Fatal(len(old), err)
	}
	LogChunkBytes = 100
	writeRows(t, p, MainBranchID, 50, 80)
	recs, err := p.ReadRecords(before, 1<<20)
	if err != nil || len(recs) != len(old)+1 || !bytes.Equal(recs[len(old)-1].Data, old[len(old)-1].Data) {
		t.Fatalf("records: %d (%v), before chunking %d", len(recs), err, len(old))
	}
}

// レプリカは記録を写さずに適用し、書くバッチ（中身と、分けて書く記録）は先に確保した大きさに収まる（docs/plan.md の
// 段階 59。倍々に伸ばすと、本番が通した大きな記録でレプリカが記録の 8 倍近くを持った）。
func TestApplyRecordsPresized(t *testing.T) {
	defer func(n int) { LogChunkBytes = n }(LogChunkBytes)
	LogChunkBytes = 4 << 10
	p, err := OpenWith(t.TempDir(), Options{LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	at, err := p.Checkpoint(backup)
	if err != nil {
		t.Fatal(err)
	}
	writeRows(t, p, MainBranchID, 0, 3000)
	r, err := OpenWith(backup, Options{Replica: true, LogRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	recs, err := p.ReadRecords(at, 1<<30)
	if err != nil || len(recs) != 1 || len(recs[0].Data) < 64<<10 {
		t.Fatalf("records: %d %v", len(recs), err)
	}
	checked := false
	testHookBeforeCommit = func(b *pebble.Batch, presized int) {
		checked = true
		if got := cap(b.Repr()); got != presized {
			t.Errorf("the replica's batch grew to %d bytes from %d", got, presized)
		}
	}
	defer func() { testHookBeforeCommit = nil }()
	if err := r.ApplyRecords(recs); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Error("the hook was not called")
	}
	if got, want := dumpKeys(t, r), dumpKeys(t, p); !mapsEqual(got, want) {
		t.Errorf("replica has %d keys, primary %d", len(got), len(want))
	}
}
