package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

func openPair(t *testing.T) (p, r *store.Store) {
	t.Helper()
	p, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	backup := filepath.Join(t.TempDir(), "b")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	r, err = store.OpenWith(backup, store.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return p, r
}

// 待ち受けの途中で本番に書くと、すぐに受け取って適用する。本番の状態に、レプリカの名前と位置が出る。
func TestReplicaReceivesWhileWaiting(t *testing.T) {
	p, r := openPair(t)
	prim := NewPrimary(p, "tok", 0)
	srv := httptest.NewServer(prim)
	defer srv.Close()
	defer prim.Close()
	rep, err := NewReplica(r, srv.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rep.Run(ctx) }()

	time.Sleep(200 * time.Millisecond) // 要求が待ち受けに入る
	start := time.Now()
	if _, err := p.CreateBranch("x", store.MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	for r.LastCommitted() != p.LastCommitted() {
		if time.Since(start) > 3*time.Second {
			t.Fatalf("not applied: %v", rep.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("applied after %s (the waiting request should return at once)", d)
	}
	if _, ok := r.BranchByName("x"); !ok {
		t.Error("the replica does not see the branch")
	}
	time.Sleep(100 * time.Millisecond)
	if v, _ := statusOf(prim.Status(), "replica r1").(string); !strings.Contains(v, "behind 0s") {
		t.Errorf("primary status: %+v", prim.Status())
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
}

// 合言葉が違う、または位置が本番の記録にないと、やり直さずに止まる。
func TestReplicaStops(t *testing.T) {
	p, r := openPair(t)
	srv := httptest.NewServer(NewPrimary(p, "tok", 0))
	defer srv.Close()

	rep, err := NewReplica(r, srv.URL, "wrong", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = rep.Run(context.Background())
	var stop errStop
	if !errors.As(err, &stop) || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: %v", err)
	}

	// 本番の記録を消して、レプリカの位置をなくす
	other, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	srv2 := httptest.NewServer(NewPrimary(other, "tok", 0))
	defer srv2.Close()
	rep, err = NewReplica(r, srv2.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = rep.Run(context.Background())
	if !errors.As(err, &stop) || !strings.Contains(err.Error(), "410") || !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("unknown position: %v", err)
	}
	if st := rep.Status(); st[1].Value != "stopped" {
		t.Errorf("status: %+v", st)
	}

	// 合言葉なしの要求、知らない道
	resp, err := http.Get(srv.URL + "/v1/log?after=1")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no token: %s", resp.Status)
		}
	}
	resp, err = http.Get(srv.URL + "/other")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("other path: %s", resp.Status)
		}
	}
}

func statusOf(items []StatusItem, name string) any {
	for _, it := range items {
		if it.Name == name {
			return it.Value
		}
	}
	return nil
}

func waitMode(t *testing.T, prim *Primary, mode string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for statusOf(prim.Status(), "replication_sync") != mode {
		if time.Now().After(deadline) {
			t.Fatalf("mode is not %s: %+v", mode, prim.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 同期の間は、コミットが返ったときにレプリカが適用している。レプリカが止まると待ち時間の後に非同期に切り替わって
// 書き込みが続き、レプリカが戻って追いつくと同期に戻る（docs/plan.md の段階 25）。
func TestSyncReplica(t *testing.T) {
	p, r := openPair(t)
	prim := NewPrimary(p, "tok", time.Second)
	srv := httptest.NewServer(prim)
	defer srv.Close()
	defer prim.Close()
	if m := statusOf(prim.Status(), "replication_sync"); m != "async" {
		t.Errorf("mode at start: %v", m)
	}
	rep, err := NewReplica(r, srv.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	run := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); rep.Run(ctx) }()
		return func() { cancel(); <-done }
	}
	stop := run()
	waitMode(t, prim, "sync")

	for i := 0; i < 5; i++ {
		if err := p.SetUsers([]byte(fmt.Sprintf("users-%d", i))); err != nil {
			t.Fatal(err)
		}
		if r.LastCommitted() < p.LastCommitted() {
			t.Fatalf("commit %d returned before the replica applied it", i)
		}
	}
	if st := rep.Status(); statusOf(st, "replication_primary_sync") != "sync" || statusOf(st, "replication_primary_last_commit") == nil {
		t.Errorf("replica status: %+v", st)
	}

	// レプリカが止まると、待ち時間の後に非同期で返す。次のコミットは待たない。
	stop()
	start := time.Now()
	if err := p.SetUsers([]byte("alone")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < time.Second {
		t.Errorf("the commit returned after %s without an acknowledgement", d)
	}
	if st := prim.Status(); statusOf(st, "replication_sync") != "async" || statusOf(st, "replication_sync_async_switches") != "1" {
		t.Errorf("after the timeout: %+v", st)
	}
	start = time.Now()
	if _, err := p.CreateBranch("async", store.MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= time.Second {
		t.Errorf("an async commit waited %s", d)
	}

	// レプリカが戻ると、追いついて同期に戻る
	stop = run()
	defer stop()
	waitMode(t, prim, "sync")
	if r.LastCommitted() != p.LastCommitted() {
		t.Errorf("sync with a replica that is behind: %d < %d", r.LastCommitted(), p.LastCommitted())
	}
	if err := p.SetUsers([]byte("again")); err != nil {
		t.Fatal(err)
	}
	if r.LastCommitted() < p.LastCommitted() {
		t.Error("the commit returned before the replica applied it")
	}

	// 止めるときは、最後のコミットの知らせを確かめて同期をやめる
	prim.Finish()
	if m := statusOf(prim.Status(), "replication_sync"); m != "off" {
		t.Errorf("mode after finish: %v", m)
	}
}

// 本番の記録にない位置（違う本番から作ったレプリカ）は、知らせとして数えない（同期にならない）。
func TestBogusPositionIsNotAck(t *testing.T) {
	p, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	prim := NewPrimary(p, "tok", time.Second)
	srv := httptest.NewServer(prim)
	defer srv.Close()
	defer prim.Close()
	if err := p.SetUsers([]byte("x")); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/log?after=%d&wait=0s&format=%d&level=%d", srv.URL, p.Written()+1000, formatVersion, replicaLevel), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("bogus position: %s", resp.Status)
	}
	if m := statusOf(prim.Status(), "replication_sync"); m != "async" {
		t.Errorf("a bogus position was taken as an acknowledgement: %v", m)
	}
}

// openReplica は、p のバックアップからもう 1 台のレプリカを開く。
func openReplica(t *testing.T, p *store.Store) *store.Store {
	t.Helper()
	backup := filepath.Join(t.TempDir(), "b")
	if _, err := p.Checkpoint(backup); err != nil {
		t.Fatal(err)
	}
	r, err := store.OpenWith(backup, store.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// runReplica は rep.Run を動かし、止める関数を返す。
func runReplica(rep *Replica) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); rep.Run(ctx) }()
	return func() { cancel(); <-done }
}

func (r *Replica) contact() (contact, send time.Time, sync string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastContact, r.lastSend, r.primarySync
}

// レプリカの最後の連絡の時刻は、本番が知らせを待ち始めた時刻より前になる（本番のマシンが壊れたときの判断。
// docs/plan.md の段階 25）。適用に待ち時間より長くかかると、レプリカには本番が同期と見えたまま本番は非同期に
// 切り替わる。そのとき連絡の時刻 + 待ち時間は、切り替えより前でなければならない。
func TestSyncContactBeforeWait(t *testing.T) {
	p, r := openPair(t)
	const timeout = 300 * time.Millisecond
	prim := NewPrimary(p, "tok", timeout)
	srv := httptest.NewServer(prim)
	defer srv.Close()
	defer prim.Close()
	rep, err := NewReplica(r, srv.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	var delay atomic.Int64
	rep.applyHook = func(n int) {
		if n > 0 {
			time.Sleep(time.Duration(delay.Load()))
		}
	}
	// commit は、知らせを待ってから非同期に切り替わるコミットをして、そのコミットを待ち始めた時刻（より少し前）を返す。
	commit := func(name string) (waitFrom time.Time) {
		t.Helper()
		waitFrom = time.Now()
		if err := p.SetUsers([]byte(name)); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(waitFrom); d < timeout {
			t.Fatalf("%s: returned after %s without switching to async", name, d)
		}
		return waitFrom
	}
	// check は、レプリカがそのコミットを適用し終えるのを待って、連絡の時刻を確かめる。
	check := func(name string, waitFrom, sendBefore time.Time) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, send, _ := rep.contact(); send != sendBefore {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: not applied: %+v", name, rep.Status())
			}
			time.Sleep(5 * time.Millisecond)
		}
		contact, _, sync := rep.contact()
		if sync != "sync" {
			t.Fatalf("%s: the replica sees the primary as %q (the scenario needs sync)", name, sync)
		}
		if !contact.Before(waitFrom) {
			t.Errorf("%s: last contact %s is not before the commit started waiting (%s)", name,
				contact.Format("15:04:05.000"), waitFrom.Format("15:04:05.000"))
		}
	}

	// 待ち受けの要求がコミットを受け取り、適用に時間がかかる
	stop := runReplica(rep)
	waitMode(t, prim, "sync")
	delay.Store(int64(3 * timeout))
	_, send0, _ := rep.contact()
	check("long poll", commit("a"), send0)
	stop()

	// 本番が知らせを待ち始めた後に、レプリカがつなぎ直してコミットを受け取る
	delay.Store(0)
	stop = runReplica(rep)
	waitMode(t, prim, "sync")
	stop()
	_, send0, _ = rep.contact()
	done := make(chan time.Time)
	go func() { done <- commit("b") }()
	time.Sleep(timeout / 3)
	delay.Store(int64(3 * timeout))
	stop = runReplica(rep)
	defer stop()
	check("reconnect", <-done, send0)
}

// 止めるときは、知らせが進んでいる間は待ち時間を過ぎても待つ。最近来ていない、またはエラーになったレプリカは待たない。
func TestFinishWaits(t *testing.T) {
	const timeout = 200 * time.Millisecond
	newPrim := func(t *testing.T) (*Primary, uint64) {
		p, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.Close() })
		prim := NewPrimary(p, "tok", timeout)
		for i := 0; i < 3; i++ { // 非同期なので待たない
			if err := p.SetUsers([]byte(fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
		}
		return prim, p.Written()
	}

	t.Run("progress", func(t *testing.T) {
		prim, last := newPrim(t)
		prim.seen("r1", "addr", 0, nil)
		go func() {
			for i := 3; i >= 0; i-- {
				time.Sleep(timeout * 2 / 3)
				prim.ack("r1", last-uint64(i))
			}
		}()
		start := time.Now()
		prim.Finish()
		if d := time.Since(start); d < 2*timeout {
			t.Errorf("finished after %s", d)
		}
		if prim.acked < last {
			t.Errorf("finished before the last acknowledgement")
		}
	})
	for _, c := range []struct {
		name string
		err  error
		at   time.Duration
	}{{"none", nil, -1}, {"error", errors.New("gone"), 0}, {"old", nil, maxWait + time.Second}} {
		t.Run(c.name, func(t *testing.T) {
			prim, _ := newPrim(t)
			if c.at >= 0 {
				prim.seen("r1", "addr", 0, c.err)
				prim.replicas["r1"].at = time.Now().Add(-c.at)
			}
			start := time.Now()
			prim.Finish()
			if d := time.Since(start); d >= timeout {
				t.Errorf("waited %s", d)
			}
		})
	}
}

// 知らせを待つ間に、低い位置の知らせが先に来ても、待ち続けて同期のまま返す。
func TestWaitAckLowerFirst(t *testing.T) {
	p, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	prim := NewPrimary(p, "tok", time.Second)
	prim.mu.Lock()
	prim.setMode(syncOn)
	prim.mu.Unlock()
	const ts = 1000
	go func() {
		time.Sleep(50 * time.Millisecond)
		prim.ack("r1", ts-1)
		time.Sleep(50 * time.Millisecond)
		prim.ack("r1", ts)
	}()
	start := time.Now()
	prim.WaitAck(ts)
	if d := time.Since(start); d < 100*time.Millisecond || d >= time.Second {
		t.Errorf("returned after %s", d)
	}
	if st := prim.Status(); statusOf(st, "replication_sync") != "sync" || statusOf(st, "replication_sync_async_switches") != "0" {
		t.Errorf("status: %+v", st)
	}
}

// レプリカが送り出しもする（連鎖する）ときは、同期にしない。
func TestChainedReplicaIsNotSync(t *testing.T) {
	_, r := openPair(t)
	prim := NewPrimary(r, "tok", time.Second)
	defer prim.Close()
	prim.ack("r2", r.Written())
	st := prim.Status()
	if statusOf(st, "replication_sync") != "off" || statusOf(st, "replication_sync_timeout") != nil {
		t.Errorf("status: %+v", st)
	}
}

// レプリカが 2 台なら、進んだ方の知らせで返す。遅れている方も本番を同期と見ているので、切り替えの判断には
// いちばん進んだレプリカを使う（README）。
func TestTwoReplicas(t *testing.T) {
	p, r1 := openPair(t)
	r2 := openReplica(t, p)
	prim := NewPrimary(p, "tok", time.Second)
	srv := httptest.NewServer(prim)
	defer srv.Close()
	defer prim.Close()
	rep1, err := NewReplica(r1, srv.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := NewReplica(r2, srv.URL, "tok", "r2", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop1 := runReplica(rep1)
	defer stop1()
	stop2 := runReplica(rep2)
	waitMode(t, prim, "sync")
	if err := p.SetUsers([]byte("both")); err != nil {
		t.Fatal(err)
	}
	// コミットは r1 の知らせだけで返ることがあるので、r2 が適用して本番を同期と見るまで待ってから止める
	deadline := time.Now().Add(5 * time.Second)
	for r2.LastCommitted() < p.LastCommitted() || statusOf(rep2.Status(), "replication_primary_sync") != "sync" {
		if time.Now().After(deadline) {
			t.Fatalf("r2 did not catch up: %+v", rep2.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop2()
	for i := 0; i < 3; i++ {
		start := time.Now()
		if err := p.SetUsers([]byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d >= time.Second || r1.LastCommitted() < p.LastCommitted() {
			t.Fatalf("commit %d: %s, r1 at %d of %d", i, d, r1.LastCommitted(), p.LastCommitted())
		}
	}
	if r2.LastCommitted() >= p.LastCommitted() {
		t.Fatal("r2 is not behind")
	}
	if s := statusOf(rep2.Status(), "replication_primary_sync"); s != "sync" {
		t.Errorf("r2 sees the primary as %v", s)
	}
	if st := prim.Status(); statusOf(st, "replication_sync") != "sync" || statusOf(st, "replica r1") == nil || statusOf(st, "replica r2") == nil {
		t.Errorf("primary status: %+v", st)
	}
}

// 本番の指定は、方式、ホスト、ポートだけを受け付ける（docs/plan.md の段階 26）。
func TestSource(t *testing.T) {
	for _, c := range []struct {
		source string
		withCA bool
		want   string // 空ならエラー
	}{
		{"10.0.0.1:3310", false, "http://10.0.0.1:3310"},
		{"10.0.0.1:3310", true, "https://10.0.0.1:3310"},
		{"http://db:3310", false, "http://db:3310"},
		{"https://db:3310/", false, "https://db:3310"},
		{"https://db:3310", true, "https://db:3310"},
		{"[::1]:3310", true, "https://[::1]:3310"},
		{"http://db:3310", true, ""},
		{"ftp://db:3310", false, ""},
		{"https://u:p@db:3310", false, ""},
		{"https://db:3310/x", false, ""},
		{"https://db:3310?a=1", false, ""},
		{"https://db:3310#f", false, ""},
		{"", false, ""},
	} {
		got, err := Source(c.source, c.withCA)
		if c.want == "" {
			if err == nil || !strings.HasPrefix(err.Error(), "HeimDB: ") {
				t.Errorf("%q (CA %v): %q, %v", c.source, c.withCA, got, err)
			}
		} else if got != c.want || err != nil {
			t.Errorf("%q (CA %v): %q, %v; want %q", c.source, c.withCA, got, err, c.want)
		}
	}
}

// formatWriter は、本番の応答の形式のバージョンのヘッダを書き換える（v が空なら消す。0.1.5 以前の本番）。
type formatWriter struct {
	http.ResponseWriter
	v     string
	wrote bool
}

func (f *formatWriter) WriteHeader(code int) {
	if !f.wrote {
		f.wrote = true
		if f.v == "" {
			f.Header().Del(headerFormat)
		} else {
			f.Header().Set(headerFormat, f.v)
		}
	}
	f.ResponseWriter.WriteHeader(code)
}

func (f *formatWriter) Write(b []byte) (int, error) {
	if !f.wrote {
		f.WriteHeader(http.StatusOK)
	}
	return f.ResponseWriter.Write(b)
}

func (f *formatWriter) Flush() {
	if fl, ok := f.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// 本番とレプリカのデータ形式のバージョンが違えば、どちらの向きでも記録を 1 件も適用せずに止まる。バージョンを示さない相手（0.1.5
// 以前）はバージョン 1 とみなすので、バージョン 2（段階 38）とはつながらない（docs/plan.md の段階 36）。
func TestFormatVersion(t *testing.T) {
	if formatVersion < 2 {
		t.Fatalf("formatVersion = %d", formatVersion)
	}
	cur, newer := formatVersion, formatVersion+1
	p, r := openPair(t)
	prim := NewPrimary(p, "tok", 0)
	defer prim.Close()
	if _, err := p.CreateBranch("x", store.MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	before := r.LastCommitted()
	serve := func(v string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			prim.ServeHTTP(&formatWriter{ResponseWriter: w, v: v}, req)
		}))
	}
	get := func(srv *httptest.Server, query string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/log?"+query, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		return resp, string(buf[:n])
	}

	// 新しい形式のレプリカと、バージョンを示さない古いレプリカは、本番が拒否する（409）。本番の状態に、そのレプリカのエラーが出る。
	srv := httptest.NewServer(prim)
	defer srv.Close()
	for name, q := range map[string]string{
		"newer": fmt.Sprintf("format=%d", newer),
		"old":   "",
	} {
		v := uint64(1)
		if name == "newer" {
			v = newer
		}
		resp, body := get(srv, fmt.Sprintf("after=%d&name=%s&wait=0s&%s", before, name, q))
		want := fmt.Sprintf("data format version differs (primary %d, replica %d)", cur, v)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, want) || resp.Header.Get(headerFormat) != fmt.Sprint(cur) {
			t.Errorf("%s replica: %s %q %q", name, resp.Status, body, resp.Header.Get(headerFormat))
		}
		if v, _ := statusOf(prim.Status(), "replica "+name).(string); !strings.Contains(v, want) {
			t.Errorf("primary status: %+v", prim.Status())
		}
	}
	// 同じバージョンなら受け付ける。
	if resp, _ := get(srv, fmt.Sprintf("after=%d&name=same&wait=0s&format=%d&level=%d", before, cur, replicaLevel)); resp.StatusCode != http.StatusOK {
		t.Errorf("same version: %s", resp.Status)
	}

	// 新しい形式の本番と、バージョンを示さない古い本番からは、記録を受け取っても適用せずに止まる。
	for _, v := range []string{fmt.Sprint(newer), ""} {
		other := serve(v)
		rep, err := NewReplica(r, other.URL, "tok", "r1", nil)
		if err != nil {
			t.Fatal(err)
		}
		pv := v
		if pv == "" {
			pv = "1"
		}
		err = rep.Run(context.Background())
		other.Close()
		var stop errStop
		if want := fmt.Sprintf("data format version differs (primary %s, replica %d)", pv, cur); !errors.As(err, &stop) || !strings.Contains(err.Error(), want) {
			t.Errorf("primary %q: %v", v, err)
		}
		if r.LastCommitted() != before {
			t.Fatal("the replica applied records from a primary with another data format")
		}
		if s, _ := statusOf(rep.Status(), "replication_last_error").(string); !strings.Contains(s, "data format version differs") {
			t.Errorf("replica status: %+v", rep.Status())
		}
	}

	// 同じバージョンの本番からは受け取る。
	same := serve(fmt.Sprint(cur))
	defer same.Close()
	rep, err := NewReplica(r, same.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopRun := runReplica(rep)
	defer stopRun()
	for start := time.Now(); r.LastCommitted() != p.LastCommitted(); time.Sleep(5 * time.Millisecond) {
		if time.Since(start) > 3*time.Second {
			t.Fatalf("not applied from a primary with the same version: %+v", rep.Status())
		}
	}
	if _, ok := r.BranchByName("x"); !ok {
		t.Error("the replica does not see the branch")
	}
}

// 本番は、記録の水準の足りないレプリカ（水準を示さない 0.2.10 以前を含む）を 409 で拒否し、本番の状態に理由を出す。
// 拒否されたレプリカは記録を適用せずに止まる（docs/plan.md の段階 58。ホスティング側の報告で、0.2.5 のレプリカが 0.2.8 の
// 本番に黙ってつながった）。
func TestReplicaLevel(t *testing.T) {
	defer func(level, min uint64) { replicaLevel, minReplicaLevel = level, min }(replicaLevel, minReplicaLevel)
	if replicaLevel < minReplicaLevel || minReplicaLevel < 6 {
		t.Fatalf("replicaLevel = %d, minReplicaLevel = %d", replicaLevel, minReplicaLevel)
	}
	p, r := openPair(t)
	prim := NewPrimary(p, "tok", 0)
	defer prim.Close()
	if _, err := p.CreateBranch("x", store.MainBranchID, 0); err != nil {
		t.Fatal(err)
	}
	before := r.LastCommitted()
	srv := httptest.NewServer(prim)
	defer srv.Close()
	get := func(name, level string) (*http.Response, string) {
		t.Helper()
		q := fmt.Sprintf("after=%d&name=%s&wait=0s&format=%d%s", before, name, formatVersion, level)
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/log?"+q, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	// 0.2.18 以前のレプリカ（水準 1）は整数でない列を含む主キーの表を、0.2.19 のレプリカ（水準 2）は主キーのない表を
	// 読めない（段階 70、72）。0.2.20 から 0.2.32（水準 3）は取り込みの記録を、0.2.33 から 0.2.38（水準 4）はインデックスの
	// キーの水準を知らない（段階 94、117）。0.2.39 と 0.2.40（水準 5）は FLOAT(M,D) と SET と BIT のインデックスを知らない（段階 120）。
	for name, level := range map[string]string{"none": "", "zero": "&level=0", "one": "&level=1", "two": "&level=2", "three": "&level=3", "four": "&level=4", "five": "&level=5"} {
		shown := map[string]string{"none": "0", "zero": "0", "one": "1", "two": "2", "three": "3", "four": "4", "five": "5"}[name]
		resp, body := get(name, level)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "the replica is too old for this primary (replica level "+shown) {
			t.Errorf("%s: %s %q", name, resp.Status, body)
		}
		if v, _ := statusOf(prim.Status(), "replica "+name).(string); !strings.Contains(v, "too old") {
			t.Errorf("primary status: %+v", prim.Status())
		}
	}
	if resp, body := get("bad", "&level=x"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad level: %s %q", resp.Status, body)
	}
	if resp, body := get("same", fmt.Sprintf("&level=%d", replicaLevel)); resp.StatusCode != http.StatusOK {
		t.Errorf("same level: %s %q", resp.Status, body)
	}

	// 本番が求める水準より低いレプリカは、記録を適用せずに止まる。
	minReplicaLevel = replicaLevel + 1
	rep, err := NewReplica(r, srv.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = rep.Run(context.Background())
	var stop errStop
	if !errors.As(err, &stop) || !strings.Contains(err.Error(), "too old") {
		t.Errorf("old replica: %v", err)
	}
	if r.LastCommitted() != before {
		t.Fatal("a replica below the primary's level applied records")
	}
	// 水準が足りれば受け取る。
	minReplicaLevel = replicaLevel
	rep, err = NewReplica(r, srv.URL, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopRun := runReplica(rep)
	defer stopRun()
	for start := time.Now(); r.LastCommitted() != p.LastCommitted(); time.Sleep(5 * time.Millisecond) {
		if time.Since(start) > 3*time.Second {
			t.Fatalf("not applied: %+v", rep.Status())
		}
	}
}
