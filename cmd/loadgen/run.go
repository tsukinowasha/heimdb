package main

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Conn は、1 本の接続。テストは偽の接続を渡す。
type Conn interface {
	// Run は文を流す。query の文は行を読み切り、最初の行の列を前から nsave 個返す。そうでない文は LAST_INSERT_ID を返す。
	Run(ctx context.Context, sql string, query bool, nsave int) (lastID int64, cols []string, err error)
	Close() error
}

// Dialer は、接続を作る。
type Dialer func(ctx context.Context) (Conn, error)

// Config は、測りの設定。
type Config struct {
	Conns       int
	Rate        float64 // ページ毎秒。0 は速さを決めない（前の応答を待って次を流す、閉じた形）
	Warmup      time.Duration
	Duration    time.Duration
	WriteFrac   float64
	Seed        uint64
	UniqueBase  int64
	Interval    time.Duration
	StmtTimeout time.Duration
	DialTimeout time.Duration
	RetryDelay  time.Duration
}

// Result は、測りの結果（JSON で書く）。
type Result struct {
	Label        string          `json:"label,omitempty"`
	Mode         string          `json:"mode"`
	Conns        int             `json:"conns"`
	TargetRate   float64         `json:"target_pages_per_s,omitempty"`
	WriteFrac    float64         `json:"write_frac"`
	WarmupS      float64         `json:"warmup_s"`
	WindowS      float64         `json:"window_s"`
	StartUnix    float64         `json:"start_unix"` // 測りの始めと終わり（温めの後。サーバーの側の CPU の標本を切り出す）
	EndUnix      float64         `json:"end_unix"`
	Pages        int             `json:"pages"`
	FailedPages  int             `json:"failed_pages"`
	PagesPerS    float64         `json:"pages_per_s"`
	Statements   int             `json:"statements"`
	StmtsPerS    float64         `json:"statements_per_s"`
	ReadPages    int             `json:"read_pages"`
	WritePages   int             `json:"write_pages"`
	Latency      map[string]Dist `json:"latency_ms"`
	Service      map[string]Dist `json:"service_ms,omitempty"`
	Lateness     *Dist           `json:"lateness_ms,omitempty"`
	LatePages    int             `json:"late_pages,omitempty"`
	Scheduled    int             `json:"scheduled_pages,omitempty"`
	NotStarted   int             `json:"not_started_pages,omitempty"`
	Errors       map[string]int  `json:"errors"`
	ConnErrors   map[string]int  `json:"connection_errors"`
	ConnectFails map[string]int  `json:"connect_failures"`
	Reconnects   int             `json:"reconnects"`
	OpenAtStart  int             `json:"connections_open_at_start"`
	NeverOpened  int             `json:"connections_never_opened"`
	Series       []Interval      `json:"series,omitempty"`
	Client       *ClientCPU      `json:"client,omitempty"`
}

// lateThreshold より遅れて始めたページを、遅れたページとして数える。
const lateThreshold = 10 * time.Millisecond

type worker struct {
	id      int
	mix     *Mixer
	conn    Conn
	vars    map[string]string
	samples []sample
	errs    map[string]int
	cerrs   map[string]int
	dfails  map[string]int
	reconn  int
	opened  bool
}

// classify は、接続の誤りの種類を返す（MySQL の誤りの番号のないもの）。
func classify(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	case errors.Is(err, mysql.ErrInvalidConn):
		return "invalid_conn"
	case errors.Is(err, driver.ErrBadConn):
		return "bad_conn"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "net_timeout"
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:60]
	}
	return "other: " + s
}

// mysqlNumber は、MySQL の誤りの番号を返す（MySQL の誤りでなければ 0）。
func mysqlNumber(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// Run は、測りを流して結果を返す。
func Run(ctx context.Context, w *Workload, dial Dialer, cfg Config) (*Result, error) {
	if cfg.Conns < 1 {
		return nil, fmt.Errorf("conns must be at least 1")
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = time.Second
	}
	if err := runSetup(ctx, w, dial, cfg); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	workers := make([]*worker, cfg.Conns)
	for i := range workers {
		workers[i] = &worker{id: i, mix: NewMixer(w, cfg.WriteFrac, cfg.Seed, i), vars: map[string]string{},
			errs: map[string]int{}, cerrs: map[string]int{}, dfails: map[string]int{}}
	}
	// 測りの前に全部の接続を作る（作れなかった接続は、測りの中で作り直しを続ける）。
	var wg sync.WaitGroup
	for _, wk := range workers {
		wg.Add(1)
		go func() { defer wg.Done(); wk.connect(ctx, w, dial, cfg) }()
	}
	wg.Wait()
	open := 0
	for _, wk := range workers {
		if wk.conn != nil {
			open++
		}
	}

	cpu0 := readClientCPU()
	t0 := time.Now()
	mStart := t0.Add(cfg.Warmup)
	tEnd := mStart.Add(cfg.Duration)
	var ticket, counter atomic.Int64
	for _, wk := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wk.loop(ctx, w, dial, cfg, t0, mStart, tEnd, &ticket, &counter)
		}()
	}
	wg.Wait()
	cpu1 := readClientCPU()

	res := &Result{Mode: "closed", Conns: cfg.Conns, WriteFrac: cfg.WriteFrac, WarmupS: cfg.Warmup.Seconds(),
		WindowS: cfg.Duration.Seconds(), Errors: map[string]int{}, ConnErrors: map[string]int{},
		ConnectFails: map[string]int{}, OpenAtStart: open,
		StartUnix: float64(mStart.UnixMilli()) / 1000, EndUnix: float64(tEnd.UnixMilli()) / 1000}
	if cfg.Rate > 0 {
		res.Mode = "open"
		res.TargetRate = cfg.Rate
	}
	var all []sample
	for _, wk := range workers {
		all = append(all, wk.samples...)
		for k, v := range wk.errs {
			res.Errors[k] += v
		}
		for k, v := range wk.cerrs {
			res.ConnErrors[k] += v
		}
		for k, v := range wk.dfails {
			res.ConnectFails[k] += v
		}
		res.Reconnects += wk.reconn
		if !wk.opened {
			res.NeverOpened++
		}
	}
	summarize(res, all, cfg)
	res.Client = cpu0.until(cpu1)
	return res, nil
}

func summarize(res *Result, all []sample, cfg Config) {
	lat := map[string][]time.Duration{}
	svc := map[string][]time.Duration{}
	var late []time.Duration
	for _, s := range all {
		res.Pages++
		res.Statements += s.stmts
		if s.failed {
			res.FailedPages++
		}
		kind := "read"
		if s.write {
			kind = "write"
			res.WritePages++
		} else {
			res.ReadPages++
		}
		for _, k := range []string{"all", kind, s.page} {
			lat[k] = append(lat[k], s.latency)
			svc[k] = append(svc[k], s.service)
		}
		if cfg.Rate > 0 {
			late = append(late, s.late)
			if s.late > lateThreshold {
				res.LatePages++
			}
		}
	}
	win := cfg.Duration.Seconds()
	res.PagesPerS = math.Round(float64(res.Pages)/win*100) / 100
	res.StmtsPerS = math.Round(float64(res.Statements)/win*10) / 10
	res.Latency = map[string]Dist{}
	for k, v := range lat {
		res.Latency[k] = distOf(v)
	}
	if cfg.Rate > 0 {
		res.Service = map[string]Dist{}
		for k, v := range svc {
			res.Service[k] = distOf(v)
		}
		d := distOf(late)
		res.Lateness = &d
		res.Scheduled = int(math.Floor(cfg.Duration.Seconds() * cfg.Rate))
		// 予定の時刻が測りの中にあり、終わりまでに終えなかったページ（始めなかったものと、終わりに流していたもの）。
		res.NotStarted = res.Scheduled - res.Pages
		if res.NotStarted < 0 {
			res.NotStarted = 0
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	res.Series = series(all, cfg.Duration, cfg.Interval)
}

func runSetup(ctx context.Context, w *Workload, dial Dialer, cfg Config) error {
	if len(w.Setup) == 0 {
		return nil
	}
	c, err := dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	for _, s := range w.Session {
		if _, _, err := c.Run(ctx, s, false, 0); err != nil {
			return err
		}
	}
	vars := map[string]string{"u": strconv.FormatInt(cfg.UniqueBase, 10), "now": time.Now().UTC().Format(time.DateTime)}
	for _, su := range w.Setup {
		_, cols, err := c.Run(ctx, su.Check, true, 1)
		if err != nil {
			return err
		}
		if cols != nil {
			continue
		}
		for _, st := range su.Steps {
			if err := runStep(ctx, c, st, vars); err != nil {
				return err
			}
		}
	}
	return nil
}

func runStep(ctx context.Context, c Conn, st *Step, vars map[string]string) error {
	id, cols, err := c.Run(ctx, st.render(vars), st.query, len(st.Save))
	if err != nil {
		return err
	}
	if st.SaveID != "" {
		vars[st.SaveID] = strconv.FormatInt(id, 10)
	}
	for i, name := range st.Save {
		if i < len(cols) {
			vars[name] = cols[i]
		} else {
			delete(vars, name)
		}
	}
	return nil
}

func (wk *worker) connect(ctx context.Context, w *Workload, dial Dialer, cfg Config) {
	dctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	c, err := dial(dctx)
	if err != nil {
		wk.dfails[errKey(err)]++
		return
	}
	for _, s := range w.Session {
		if _, _, err := c.Run(dctx, s, false, 0); err != nil {
			wk.dfails[errKey(err)]++
			c.Close()
			return
		}
	}
	if wk.opened {
		wk.reconn++
	}
	wk.opened = true
	wk.conn = c
}

func errKey(err error) string {
	if n := mysqlNumber(err); n != 0 {
		return strconv.Itoa(int(n))
	}
	return classify(err)
}

// sleepUntil は t まで待つ。ctx が終われば false。
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (wk *worker) loop(ctx context.Context, w *Workload, dial Dialer, cfg Config, t0, mStart, tEnd time.Time, ticket, counter *atomic.Int64) {
	defer func() {
		if wk.conn != nil {
			wk.conn.Close()
		}
	}()
	for ctx.Err() == nil {
		if wk.conn == nil {
			if time.Now().After(tEnd) {
				return
			}
			wk.connect(ctx, w, dial, cfg)
			if wk.conn == nil {
				if !sleepUntil(ctx, minTime(time.Now().Add(cfg.RetryDelay), tEnd)) {
					return
				}
				continue
			}
		}
		var intended time.Time
		if cfg.Rate > 0 {
			k := ticket.Add(1) - 1
			intended = t0.Add(time.Duration(float64(k) / cfg.Rate * float64(time.Second)))
			if !intended.Before(tEnd) {
				return
			}
			if !sleepUntil(ctx, intended) {
				return
			}
		}
		start := time.Now()
		if !start.Before(tEnd) {
			return
		}
		if cfg.Rate == 0 {
			intended = start
		}
		measured := !intended.Before(mStart)
		p := wk.mix.Next()
		wk.vars["u"] = strconv.FormatInt(cfg.UniqueBase+counter.Add(1), 10)
		wk.vars["now"] = start.UTC().Format(time.DateTime)
		n, failed := wk.runPage(ctx, p, cfg, measured)
		end := time.Now()
		if measured && !end.After(tEnd) {
			wk.samples = append(wk.samples, sample{page: p.Name, write: p.Kind == "write", at: end.Sub(mStart),
				latency: end.Sub(intended), service: end.Sub(start), late: start.Sub(intended), stmts: n, failed: failed})
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// runPage は、ページの文を順に流し、流した文の数と、誤りがあったかを返す。
func (wk *worker) runPage(ctx context.Context, p *Page, cfg Config, measured bool) (int, bool) {
	failed := false
	n := 0
	for _, st := range p.Steps {
		sctx := ctx
		var cancel context.CancelFunc
		if cfg.StmtTimeout > 0 {
			sctx, cancel = context.WithTimeout(ctx, cfg.StmtTimeout)
		}
		err := runStep(sctx, wk.conn, st, wk.vars)
		if cancel != nil {
			cancel()
		}
		n++
		if err == nil {
			continue
		}
		failed = true
		if num := mysqlNumber(err); num != 0 {
			if measured {
				wk.errs[strconv.Itoa(int(num))]++
			}
			if st.SaveID != "" || len(st.Save) > 0 {
				break
			}
			continue
		}
		// 接続の誤り: 接続を捨て、次のページの前に作り直す。
		if measured {
			wk.cerrs[classify(err)]++
		}
		wk.conn.Close()
		wk.conn = nil
		break
	}
	return n, failed
}

// mysqlConn は、go-sql-driver の接続をそのまま使う（database/sql の接続の溜めを通さない）。
type mysqlConn struct {
	c driver.Conn
}

// MySQLDialer は、dsn の MySQL に接続する Dialer を返す。
func MySQLDialer(dsn string) (Dialer, error) {
	mc, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (Conn, error) {
		c, err := conn.Connect(ctx)
		if err != nil {
			return nil, err
		}
		return &mysqlConn{c: c}, nil
	}, nil
}

func (m *mysqlConn) Run(ctx context.Context, q string, query bool, nsave int) (int64, []string, error) {
	if !query {
		r, err := m.c.(driver.ExecerContext).ExecContext(ctx, q, nil)
		if err != nil {
			return 0, nil, err
		}
		id, _ := r.LastInsertId()
		return id, nil, nil
	}
	rows, err := m.c.(driver.QueryerContext).QueryContext(ctx, q, nil)
	if err != nil {
		return 0, nil, err
	}
	dest := make([]driver.Value, len(rows.Columns()))
	var cols []string
	first := true
	for {
		if err := rows.Next(dest); err != nil {
			if err == io.EOF {
				break
			}
			rows.Close()
			return 0, nil, err
		}
		if first {
			first = false
			cols = make([]string, 0, nsave)
			for i := 0; i < nsave && i < len(dest); i++ {
				cols = append(cols, valueString(dest[i]))
			}
		}
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	return 0, cols, nil
}

func valueString(v driver.Value) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case time.Time:
		return x.Format(time.DateTime)
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func (m *mysqlConn) Close() error { return m.c.Close() }
