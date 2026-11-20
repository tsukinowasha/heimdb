package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ブランチの操作の耐久性の試験（docs/plan.md の段階 76）。主キーのない表と、文字列と日付の複合の主キーの表に、本番の
// 書き込みと、ブランチの作成、ブランチへの書き込み、復元、マージ、削除を流しながら kill -9 で落とし、起動し直して、
// 応答を返した書き込みとマージが残り、途中のマージと復元が全部か無かのどちらかで、行が重複しないことを確かめる。

const branchBatch = 200 // ブランチへの 1 回の書き込みの行数（マージに時間がかかるように多めにする）

// branchRun は、ブランチの係の 1 回分（作成から削除まで）の記録。
type branchRun struct {
	name                    string
	batch1, batch2          []string // 1 回目と 2 回目の書き込みの行のキー
	created, wrote1, wrote2 bool     // 応答を受け取った
	restored, merged, gone  bool
	triedWrite2, triedMerge bool // 試みた（応答を受け取ったかは問わない）
	triedRestore, triedGone bool
}

type branchLedger struct {
	mu        sync.Mutex
	acked     map[string]bool // 本番の書き込みで応答を受け取った行のキー
	attempted map[string]bool
	runs      []*branchRun
}

func setupBranchTables(t *testing.T, srv *proc) {
	t.Helper()
	db := srv.db(t)
	defer db.Close()
	for _, q := range []string{
		// 主キーのない表（段階 72）
		"CREATE TABLE logs (k varchar(64) NOT NULL, v int NOT NULL, KEY k (k))",
		// 文字列と日付の複合の主キーの表（段階 70）
		"CREATE TABLE kv (site varchar(64) NOT NULL, d date NOT NULL, v int NOT NULL, PRIMARY KEY (site, d))",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// insertRows は、キー keys の行を 2 つの表に 1 つのトランザクションで書く。
func insertRows(ctx context.Context, db *sql.DB, keys []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var logs, kv []string
	for _, k := range keys {
		logs = append(logs, fmt.Sprintf("('%s', 1)", k))
		kv = append(kv, fmt.Sprintf("('%s', '2026-10-25', 1)", k))
	}
	if _, err := tx.Exec("INSERT INTO logs (k, v) VALUES " + strings.Join(logs, ",")); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec("INSERT INTO kv (site, d, v) VALUES " + strings.Join(kv, ",")); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func batchKeys(prefix string, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return keys
}

// branchWorkload は、srv が止まるまで、本番の書き込みとブランチの係を続ける。
func branchWorkload(t *testing.T, srv *proc, cycle int, lg *branchLedger) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			db := srv.db(t)
			defer db.Close()
			db.SetMaxOpenConns(1)
			for n := 0; ctx.Err() == nil; n++ {
				key := fmt.Sprintf("m%d-%d-%d", cycle, w, n)
				lg.mu.Lock()
				lg.attempted[key] = true
				lg.mu.Unlock()
				if err := insertRows(ctx, db, []string{key}); err != nil {
					if isLockError(err) {
						continue
					}
					return
				}
				lg.mu.Lock()
				lg.acked[key] = true
				lg.mu.Unlock()
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		root := srv.db(t)
		defer root.Close()
		root.SetMaxOpenConns(1)
		for i := 0; ctx.Err() == nil; i++ {
			r := &branchRun{name: fmt.Sprintf("b%d_%d", cycle, i)}
			r.batch1 = batchKeys(r.name+"-1", branchBatch)
			r.batch2 = batchKeys(r.name+"-2", branchBatch)
			lg.mu.Lock()
			lg.runs = append(lg.runs, r)
			lg.mu.Unlock()
			if !runBranch(ctx, srv, root, r, lg) {
				return
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// runBranch は、ブランチの係の 1 回分を流す。途中で失敗したら（落とされたら）false。
func runBranch(ctx context.Context, srv *proc, root *sql.DB, r *branchRun, lg *branchLedger) bool {
	set := func(f *bool) {
		lg.mu.Lock()
		*f = true
		lg.mu.Unlock()
	}
	if _, err := root.ExecContext(ctx, "CALL heimdb_branch_create(?)", r.name); err != nil {
		return false
	}
	set(&r.created)
	bdb, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress@"+r.name)
	if err != nil {
		return false
	}
	defer bdb.Close()
	if insertRows(ctx, bdb, r.batch1) != nil {
		return false
	}
	set(&r.wrote1)
	time.Sleep(5 * time.Millisecond)
	at := time.Now().UTC().Format("2006-01-02 15:04:05.000000-07:00")
	time.Sleep(5 * time.Millisecond)
	set(&r.triedWrite2)
	if insertRows(ctx, bdb, r.batch2) != nil {
		return false
	}
	set(&r.wrote2)
	set(&r.triedRestore)
	if _, err := root.ExecContext(ctx, "CALL heimdb_restore(?, ?)", r.name, at); err != nil {
		return false
	}
	set(&r.restored)
	set(&r.triedMerge)
	if _, err := root.ExecContext(ctx, "CALL heimdb_branch_merge(?)", r.name); err != nil {
		return false
	}
	set(&r.merged)
	set(&r.triedGone)
	if _, err := root.ExecContext(ctx, "CALL heimdb_branch_delete(?)", r.name); err != nil {
		return false
	}
	set(&r.gone)
	return true
}

// keyCounts は、表 table（列 col）のキーごとの行数を返す。
func keyCounts(t *testing.T, db *sql.DB, table, col string) map[string]int {
	t.Helper()
	rows, err := db.Query("SELECT " + col + " FROM " + table)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	have := map[string]int{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		have[k]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return have
}

// countOf は、keys のうち have にあるものの数を返す。
func countOf(have map[string]int, keys []string) int {
	n := 0
	for _, k := range keys {
		if have[k] > 0 {
			n++
		}
	}
	return n
}

// verifyBranches は、起動し直した HeimDB で、本番と残ったブランチの中身を確かめ、残ったブランチを消す。
func verifyBranches(t *testing.T, srv *proc, lg *branchLedger) {
	t.Helper()
	db := srv.db(t)
	defer db.Close()
	lg.mu.Lock()
	defer lg.mu.Unlock()
	for _, tc := range []struct{ table, col string }{{"logs", "k"}, {"kv", "site"}} {
		have := keyCounts(t, db, tc.table, tc.col)
		known := map[string]bool{}
		for k := range lg.acked {
			if have[k] == 0 {
				t.Errorf("%s: acknowledged write %s is missing", tc.table, k)
			}
		}
		for k := range lg.attempted {
			known[k] = true
		}
		for _, r := range lg.runs {
			n1, n2 := countOf(have, r.batch1), countOf(have, r.batch2)
			// 途中のマージは全部か無か。復元で消した 2 回目の行は、本番に来ない。
			if n1 != 0 && n1 != len(r.batch1) {
				t.Errorf("%s: branch %s merged %d of %d rows", tc.table, r.name, n1, len(r.batch1))
			}
			if r.merged && n1 == 0 {
				t.Errorf("%s: acknowledged merge of %s is missing", tc.table, r.name)
			}
			if n1 > 0 && !r.triedMerge {
				t.Errorf("%s: rows of %s are in main without a merge", tc.table, r.name)
			}
			if n2 != 0 {
				t.Errorf("%s: %d restored-away rows of %s are in main", tc.table, n2, r.name)
			}
			for _, k := range r.batch1 {
				known[k] = true
			}
		}
		for k, n := range have {
			if !known[k] {
				t.Errorf("%s: has %s that was never written", tc.table, k)
			}
			if n != 1 {
				t.Errorf("%s: %s appears %d times", tc.table, k, n)
			}
		}
	}
	// 残ったブランチ: 書き込みと復元は全部か無か。確かめた後に消す。
	for _, r := range lg.runs {
		if !r.created || r.gone {
			continue
		}
		bdb, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress@"+r.name)
		if err != nil {
			t.Fatal(err)
		}
		exists := true
		for _, tc := range []struct{ table, col string }{{"logs", "k"}, {"kv", "site"}} {
			rows, err := bdb.Query("SELECT " + tc.col + " FROM " + tc.table)
			if err != nil {
				if r.triedGone && strings.Contains(err.Error(), "1049") {
					exists = false // 削除は応答の前に済んでいた
					break
				}
				t.Fatalf("branch %s: read %s: %v", r.name, tc.table, err)
			}
			have := map[string]int{}
			for rows.Next() {
				var k string
				rows.Scan(&k)
				have[k]++
			}
			rows.Close()
			n1, n2 := countOf(have, r.batch1), countOf(have, r.batch2)
			if n1 != 0 && n1 != len(r.batch1) || n2 != 0 && n2 != len(r.batch2) {
				t.Errorf("branch %s: %s has %d of the first and %d of the second write", r.name, tc.table, n1, n2)
			}
			if r.wrote1 && n1 == 0 {
				t.Errorf("branch %s: %s lost the acknowledged first write", r.name, tc.table)
			}
			if r.wrote2 && !r.triedRestore && n2 == 0 {
				t.Errorf("branch %s: %s lost the acknowledged second write", r.name, tc.table)
			}
			if (r.restored || !r.triedWrite2) && n2 != 0 {
				t.Errorf("branch %s: %s has %d rows of the second write after the restore", r.name, tc.table, n2)
			}
			for k, n := range have {
				if n != 1 {
					t.Errorf("branch %s: %s: %s appears %d times", r.name, tc.table, k, n)
				}
			}
		}
		bdb.Close()
		if !exists {
			r.gone = true
			continue
		}
		if _, err := db.Exec("CALL heimdb_branch_delete(?)", r.name); err != nil {
			t.Errorf("delete leftover branch %s: %v", r.name, err)
		}
		r.gone = true
	}
	// そのままブランチを作ってマージできる。
	r := &branchRun{name: fmt.Sprintf("after%d", len(lg.runs))}
	r.batch1 = batchKeys(r.name, 3)
	if _, err := db.Exec("CALL heimdb_branch_create(?)", r.name); err != nil {
		t.Fatalf("create after restart: %v", err)
	}
	bdb, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress@"+r.name)
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	if err := insertRows(context.Background(), bdb, r.batch1); err != nil {
		t.Fatalf("write a branch after restart: %v", err)
	}
	for _, q := range []string{"CALL heimdb_branch_merge(?)", "CALL heimdb_branch_delete(?)"} {
		if _, err := db.Exec(q, r.name); err != nil {
			t.Fatalf("%s after restart: %v", q, err)
		}
	}
	for _, k := range r.batch1 {
		lg.acked[k] = true
		lg.attempted[k] = true
	}
	if strings.Contains(srv.log.String(), "panic") {
		t.Errorf("heimdb log has a panic:\n%s", srv.log)
	}
}

// ブランチの操作の最中に kill -9 で何度も落とし、そのたびに起動し直して確かめる。
func TestCrashRecoveryBranches(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs heimdb")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	// 復元の時刻が保持期間の外にならないよう、保持期間は試験より長くする（GC は 200 ミリ秒ごとに流れる）
	args := []string{"-data", dir, "-root-password", "pw", "-retention", "10m", "-gc-interval", "200ms"}
	lg := &branchLedger{acked: map[string]bool{}, attempted: map[string]bool{}}
	srv := startArgs(t, bin, args...)
	setupBranchTables(t, srv)
	r := rand.New(rand.NewSource(3))
	for cycle := 0; cycle < 8; cycle++ {
		stop := branchWorkload(t, srv, cycle, lg)
		time.Sleep(time.Duration(300+r.Intn(1500)) * time.Millisecond)
		srv.cmd.Process.Signal(syscall.SIGKILL)
		<-srv.done
		stop()
		srv = startArgs(t, bin, args...)
		verifyBranches(t, srv, lg)
		if t.Failed() {
			break
		}
	}
	lg.mu.Lock()
	merged, interrupted := 0, 0
	for _, r := range lg.runs {
		if r.merged {
			merged++
		} else if r.triedMerge {
			interrupted++
		}
	}
	t.Logf("acknowledged writes on main: %d, branches: %d, merged: %d, merges cut by the kill: %d", len(lg.acked), len(lg.runs), merged, interrupted)
	lg.mu.Unlock()
	srv.cmd.Process.Signal(syscall.SIGKILL)
	<-srv.done
}
