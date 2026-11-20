package main

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestPercentile(t *testing.T) {
	var ds []time.Duration
	for i := 100; i >= 1; i-- {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	d := distOf(ds)
	if d.Count != 100 || d.P50 != 50 || d.P95 != 95 || d.P99 != 99 || d.Max != 100 || d.Mean != 50.5 {
		t.Fatalf("dist %+v", d)
	}
	if got := percentile([]time.Duration{7}, 99); got != 7 {
		t.Fatalf("single %v", got)
	}
	if got := distOf(nil); got.Count != 0 {
		t.Fatalf("empty %+v", got)
	}
	// 最も近い順位: 10 個の p95 は 10 番目、p50 は 5 番目。
	ds = ds[:0]
	for i := 1; i <= 10; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	if d := distOf(ds); d.P50 != 5 || d.P95 != 10 || d.P90 != 9 {
		t.Fatalf("ten %+v", d)
	}
}

func TestSeries(t *testing.T) {
	var ss []sample
	for i := 0; i < 30; i++ {
		ss = append(ss, sample{at: time.Duration(i) * 100 * time.Millisecond, latency: time.Duration(i+1) * time.Millisecond, failed: i == 25})
	}
	got := series(ss, 3*time.Second, time.Second)
	if len(got) != 3 || got[0].Pages != 10 || got[2].Pages != 10 || got[0].P50 != 5 || got[2].Max != 30 || got[2].FailedPage != 1 || got[1].PagesPerS != 10 {
		t.Fatalf("series %+v", got)
	}
}

const testWorkload = `{
  "session": ["SET NAMES 'utf8mb4'"],
  "setup": [{"check": "SELECT id FROM tags WHERE slug = 'a'", "steps": [{"sql": "INSERT INTO tags (slug) VALUES ('a')"}]}],
  "pages": [
    {"name": "r1", "kind": "read", "steps": [{"sql": "SELECT 1"}, {"sql": "SELECT 2"}]},
    {"name": "r2", "kind": "read", "steps": [{"sql": "SELECT 3"}]},
    {"name": "create", "kind": "write", "steps": [
      {"sql": "SELECT id FROM tags WHERE slug = 'a'", "save": ["tag"]},
      {"sql": "INSERT INTO posts (title, at) VALUES ('Bench {{u}}', '{{now}}')", "save_id": "post"},
      {"sql": "INSERT INTO rel VALUES ({{post}}, {{tag}})"}]},
    {"name": "delete", "kind": "write", "steps": [{"sql": "DELETE FROM posts WHERE id = {{ post }} AND x = {{missing}}"}]}
  ]
}`

func TestParseWorkload(t *testing.T) {
	w, err := ParseWorkload([]byte(testWorkload))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.reads) != 2 || len(w.writes) != 2 {
		t.Fatalf("reads %d writes %d", len(w.reads), len(w.writes))
	}
	del := w.writes[1].Steps[0]
	if got := del.render(map[string]string{"post": "42"}); got != "DELETE FROM posts WHERE id = 42 AND x = 0" {
		t.Fatalf("render %q", got)
	}
	if del.query || !w.writes[0].Steps[0].query || w.writes[0].Steps[1].query {
		t.Fatal("query detection")
	}
	l, err := ParseWorkload([]byte(`["SELECT 1", "SELECT {{x}}"]`))
	if err != nil || len(l.reads) != 1 || len(l.reads[0].Steps) != 2 {
		t.Fatalf("list form %v %+v", err, l)
	}
	for _, bad := range []string{`{"pages":[{"name":"x","kind":"other","steps":[{"sql":"SELECT 1"}]}]}`,
		`{"pages":[{"name":"x","kind":"read","steps":[]}]}`,
		`{"pages":[{"name":"x","kind":"read","steps":[{"sql":"SELECT {{x"}]}]}`} {
		if _, err := ParseWorkload([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestMixer(t *testing.T) {
	w, err := ParseWorkload([]byte(testWorkload))
	if err != nil {
		t.Fatal(err)
	}
	m := NewMixer(w, 0.2, 7, 3)
	n, writes := 20000, 0
	var lastWrite string
	reads := map[string]int{}
	for i := 0; i < n; i++ {
		p := m.Next()
		if p.Kind == "write" {
			// 書き込みはファイルの順に回る。
			if lastWrite == p.Name {
				t.Fatalf("write order: %s twice", p.Name)
			}
			lastWrite = p.Name
			writes++
		} else {
			reads[p.Name]++
		}
	}
	if writes < 3800 || writes > 4200 {
		t.Fatalf("writes %d of %d", writes, n)
	}
	if d := reads["r1"] - reads["r2"]; d < -1 || d > 1 {
		t.Fatalf("reads not rotated: %v", reads)
	}
	// 読みだけのときは書き込みを選ばない。同じ種と接続は同じ並び。
	a, b := NewMixer(w, 0.5, 9, 1), NewMixer(w, 0.5, 9, 1)
	for i := 0; i < 100; i++ {
		if a.Next() != b.Next() {
			t.Fatal("not deterministic")
		}
	}
	r := NewMixer(w, 0, 1, 0)
	for i := 0; i < 100; i++ {
		if r.Next().Kind != "read" {
			t.Fatal("write with frac 0")
		}
	}
}

// fakeConn は、文ごとに delay だけ待ち、流した文を残す。
type fakeConn struct {
	f      *fakeServer
	id     int
	closed bool
}

type fakeServer struct {
	mu       sync.Mutex
	delay    time.Duration
	nextID   int64
	stmts    map[int][]string
	dials    atomic.Int64
	failOn   string // この文字を含む文は 1205 を返す
	breakOn  string // この文字を含む文は接続を切る
	inflight atomic.Int64
	maxIn    atomic.Int64
	tagExist bool
}

func newFake(delay time.Duration) *fakeServer {
	return &fakeServer{delay: delay, nextID: 100, stmts: map[int][]string{}}
}

func (f *fakeServer) dial(ctx context.Context) (Conn, error) {
	return &fakeConn{f: f, id: int(f.dials.Add(1))}, nil
}

func (c *fakeConn) Run(ctx context.Context, q string, query bool, nsave int) (int64, []string, error) {
	if c.closed {
		return 0, nil, mysql.ErrInvalidConn
	}
	n := c.f.inflight.Add(1)
	defer c.f.inflight.Add(-1)
	for {
		m := c.f.maxIn.Load()
		if n <= m || c.f.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	if c.f.delay > 0 {
		select {
		case <-time.After(c.f.delay):
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.stmts[c.id] = append(c.f.stmts[c.id], q)
	if c.f.failOn != "" && strings.Contains(q, c.f.failOn) {
		return 0, nil, &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	}
	if c.f.breakOn != "" && strings.Contains(q, c.f.breakOn) {
		return 0, nil, io.ErrUnexpectedEOF
	}
	if strings.HasPrefix(q, "INSERT INTO tags") {
		c.f.tagExist = true
	}
	if query {
		if strings.Contains(q, "FROM tags") {
			if !c.f.tagExist {
				return 0, nil, nil
			}
			return 0, []string{"7"}[:min(1, nsave)], nil
		}
		return 0, []string{}, nil
	}
	c.f.nextID++
	return c.f.nextID, nil, nil
}

func (c *fakeConn) Close() error { c.closed = true; return nil }

func TestClosedLoop(t *testing.T) {
	w, _ := ParseWorkload([]byte(testWorkload))
	f := newFake(time.Millisecond)
	cfg := Config{Conns: 4, Warmup: 50 * time.Millisecond, Duration: 400 * time.Millisecond, WriteFrac: 0.3, Seed: 1,
		UniqueBase: 5000, Interval: 100 * time.Millisecond, DialTimeout: time.Second}
	res, err := Run(context.Background(), w, f.dial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "closed" || res.OpenAtStart != 4 || res.NeverOpened != 0 || res.Pages == 0 || res.FailedPages != 0 {
		t.Fatalf("result %+v", res)
	}
	// 4 本の接続が、1 文 1 ms のページ（1〜3 文）を流し続ける。上限は 4 本 × 400 ms / 1 ms。
	if res.Statements > 1600 || res.Statements < 200 {
		t.Fatalf("statements %d", res.Statements)
	}
	if res.ReadPages+res.WritePages != res.Pages || res.Latency["all"].Count != res.Pages || res.Latency["create"].Count == 0 {
		t.Fatalf("counts %+v", res)
	}
	if f.maxIn.Load() > 4 {
		t.Fatalf("more than 4 statements at once: %d", f.maxIn.Load())
	}
	if len(res.Series) != 4 {
		t.Fatalf("series %d", len(res.Series))
	}
	// setup がタグを作り、作成のページは取っておいた ID を次の文に使う。各接続は最初に session の文を流す。
	if !f.tagExist {
		t.Fatal("setup did not run")
	}
	checked := 0
	for id, ss := range f.stmts {
		if id == 1 { // setup の接続
			continue
		}
		if len(ss) == 0 || ss[0] != "SET NAMES 'utf8mb4'" {
			t.Fatalf("conn %d did not start with the session: %v", id, ss[:min(1, len(ss))])
		}
		for i, s := range ss {
			if strings.HasPrefix(s, "INSERT INTO posts") && i+1 < len(ss) && strings.HasPrefix(ss[i+1], "INSERT INTO rel") {
				if !strings.HasSuffix(ss[i+1], ", 7)") || strings.Contains(ss[i+1], "(0,") || !strings.Contains(s, "'Bench 5") {
					t.Fatalf("variables not carried: %q then %q", s, ss[i+1])
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no create page checked")
	}
}

func TestOpenLoop(t *testing.T) {
	w, _ := ParseWorkload([]byte(`["SELECT 1"]`))
	// 間に合う速さ: 予定どおりの数を流し、遅れは小さい。
	f := newFake(2 * time.Millisecond)
	cfg := Config{Conns: 4, Rate: 200, Warmup: 100 * time.Millisecond, Duration: time.Second, Seed: 1, DialTimeout: time.Second}
	res, err := Run(context.Background(), w, f.dial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "open" || res.Scheduled != 200 || res.Pages < 190 || res.Pages > 200 || res.NotStarted > 10 {
		t.Fatalf("on time: pages %d scheduled %d not started %d", res.Pages, res.Scheduled, res.NotStarted)
	}
	if res.Lateness.P50 > 5 {
		t.Fatalf("lateness %+v", res.Lateness)
	}
	// 1 本の接続で 1 ページ 10 ms なら毎秒 100 ページまで。毎秒 200 の予定は遅れていき、時間は予定からの遅れを含む。
	f = newFake(10 * time.Millisecond)
	cfg = Config{Conns: 1, Rate: 200, Warmup: 0, Duration: time.Second, Seed: 1, DialTimeout: time.Second}
	res, err = Run(context.Background(), w, f.dial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages > 105 || res.Pages < 50 || res.NotStarted < 90 {
		t.Fatalf("saturated: pages %d not started %d", res.Pages, res.NotStarted)
	}
	if res.Latency["all"].Max < 300 || res.Service["all"].P99 > 50 || res.LatePages < res.Pages/2 {
		t.Fatalf("saturated latency %+v service %+v late %d", res.Latency["all"], res.Service["all"], res.LatePages)
	}
}

func TestErrors(t *testing.T) {
	w, _ := ParseWorkload([]byte(`{"pages":[{"name":"p","kind":"read","steps":[{"sql":"SELECT a"},{"sql":"SELECT lockme"},{"sql":"SELECT b"}]},
		{"name":"q","kind":"read","steps":[{"sql":"SELECT breakme"},{"sql":"SELECT c"}]}]}`))
	f := newFake(0)
	f.failOn, f.breakOn = "lockme", "breakme"
	cfg := Config{Conns: 1, Duration: 100 * time.Millisecond, Seed: 1, DialTimeout: time.Second}
	res, err := Run(context.Background(), w, f.dial, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// p は 1205 の後も続け、q は接続が切れて残りを流さず、次のページの前に作り直す。
	if res.Errors["1205"] == 0 || res.ConnErrors["eof"] == 0 || res.Reconnects == 0 || res.FailedPages != res.Pages {
		t.Fatalf("errors %+v conn %+v reconnects %d failed %d pages %d", res.Errors, res.ConnErrors, res.Reconnects, res.FailedPages, res.Pages)
	}
	for _, ss := range f.stmts {
		for i, s := range ss {
			if s == "SELECT breakme" && i+1 < len(ss) && ss[i+1] == "SELECT c" {
				t.Fatal("ran a statement after the connection broke")
			}
		}
	}
	if res.Errors["1205"] > res.Pages/2+1 {
		t.Fatalf("1205 counted %d for %d pages", res.Errors["1205"], res.Pages)
	}
}

// TestAgainstServer は、LOADGEN_TEST_DSN のサーバー（127.0.0.1:3360 の HeimDB など、wpbench のデータ）に、
// scripts/bench/load の読みと書き込みのページを短く流す。
func TestAgainstServer(t *testing.T) {
	dsn := os.Getenv("LOADGEN_TEST_DSN")
	if dsn == "" {
		t.Skip("LOADGEN_TEST_DSN is not set")
	}
	w, err := LoadWorkload("../../scripts/bench/load/pages-read.json")
	if err != nil {
		t.Fatal(err)
	}
	wr, err := LoadWorkload("../../scripts/bench/load/pages-write.json")
	if err != nil {
		t.Fatal(err)
	}
	w.Merge(wr)
	dial, err := MySQLDialer(dsn)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), w, dial, Config{Conns: 4, Duration: 5 * time.Second, WriteFrac: 0.3, Seed: 1,
		UniqueBase: time.Now().Unix() * 100000, DialTimeout: 5 * time.Second, StmtTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pages %d (%.1f/s) stmts/s %.0f errors %v conn %v", res.Pages, res.PagesPerS, res.StmtsPerS, res.Errors, res.ConnErrors)
	if res.Pages == 0 || res.FailedPages != 0 || res.WritePages == 0 {
		t.Fatalf("result %+v", res)
	}
}
