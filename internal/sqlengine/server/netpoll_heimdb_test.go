// Copyright 2026 Tsukinowa Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modified by Tsukinowa Inc.: 文を始めるときに眠った Go のスレッドを起こすことのテスト（段階 128。docs/engine.md）

package server

import (
	"context"
	gosql "database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/sqltypes"
	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// countWakes は、wakeIdleThread を数えるものに替え、数を返す関数を返す。
func countWakes(t *testing.T) func() int64 {
	var n atomic.Int64
	old := wakeIdleThread
	wakeIdleThread = func() { n.Add(1) }
	t.Cleanup(func() { wakeIdleThread = old })
	return n.Load
}

// TestStage128WakeWhenOthersWait は、文を始めるときに、ほかに開いた接続があり、文の途中の接続がほかにないときだけ
// スレッドを起こすことを確かめる（1 本だけ、ほかが文の途中、閉じた後は起こさない）。
func TestStage128WakeWhenOthersWait(t *testing.T) {
	wakes := countWakes(t)
	h := stage124Handler(t)
	c1, c2 := newConn(1), newConn(2)
	h.NewConnection(c1)
	require.NoError(t, h.ComInitDB(c1, "test"))
	query := func(c *mysql.Conn, cb func()) {
		require.NoError(t, h.ComQuery(context.Background(), c, "SELECT 1", func(*sqltypes.Result, bool) error {
			if cb != nil {
				cb()
			}
			return nil
		}))
	}

	query(c1, nil)
	require.EqualValues(t, 0, wakes(), "one connection")

	h.NewConnection(c2)
	require.NoError(t, h.ComInitDB(c2, "test"))
	query(c1, nil)
	require.EqualValues(t, 1, wakes(), "c2 waits for its next command")
	query(c2, nil)
	require.EqualValues(t, 2, wakes(), "c1 waits for its next command")

	// c1 の文の途中で c2 の文を始める: c2 を始めるときは、どちらも文を処理しているので起こさない。
	query(c1, func() { query(c2, nil) })
	require.EqualValues(t, 3, wakes(), "only c1's statement wakes")
	require.EqualValues(t, 0, h.busyConns.Load())

	// エラーの文も数えを戻す。
	require.Error(t, h.ComQuery(context.Background(), c1, "SELECT * FROM no_such_table", dummyCb))
	require.EqualValues(t, 4, wakes())
	require.EqualValues(t, 0, h.busyConns.Load())

	// 3 本で、c1 の文の途中に c2 の文を始める: c3 は待っているが、c1 が文の途中なので起こさない。
	c3 := newConn(3)
	h.NewConnection(c3)
	require.NoError(t, h.ComInitDB(c3, "test"))
	query(c1, func() { query(c2, nil) })
	require.EqualValues(t, 5, wakes(), "c1's statement wakes, c2's does not")
	h.ConnectionClosed(c3)

	h.ConnectionClosed(c2)
	require.EqualValues(t, 1, h.openConns.Load())
	query(c1, nil)
	require.EqualValues(t, 5, wakes(), "c2 is closed")
}

// stage128Spin は、Go のスレッドを手放さずに stage128SpinFor の間だけ回る式（網を見るスレッドがいない状態を作る）。
type stage128Spin struct{}

const stage128SpinFor = 100 * time.Millisecond

func (stage128Spin) Resolved() bool             { return true }
func (stage128Spin) String() string             { return "heimdb_test_spin()" }
func (stage128Spin) Type() sql.Type             { return types.Int64 }
func (stage128Spin) IsNullable() bool           { return false }
func (stage128Spin) Children() []sql.Expression { return nil }
func (s stage128Spin) WithChildren(c ...sql.Expression) (sql.Expression, error) {
	if len(c) != 0 {
		return nil, sql.ErrInvalidChildrenNumber.New(s, len(c), 0)
	}
	return s, nil
}
func (stage128Spin) Eval(*sql.Context, sql.Row) (interface{}, error) {
	end := time.Now().Add(stage128SpinFor)
	x := uint64(1)
	for time.Now().Before(end) {
		for i := 0; i < 1000; i++ {
			x = x*6364136223846793005 + 1442695040888963407
		}
	}
	return int64(x & 1), nil
}

// stage128Listener は、受けた接続を *net.TCPConn でない型に包む。TCP の接続では、結果の行を送る文が切れた接続の
// 見張りのタイマー（time.AfterFunc）を置き、タイマーを置くときにランタイムがスレッドを起こす（wakeNetPoller）ので、
// それのない経路（OK、1 行、UNIX ソケットの文）と同じにする。
type stage128Listener struct{ net.Listener }

type stage128Conn struct{ net.Conn }

func (l stage128Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return stage128Conn{c}, nil
}

const stage128ClientEnv = "HEIMDB_STAGE128_CLIENT"

// TestStage128IdleConnectionReadDuringStatement は、別のプロセスの 2 本の接続で、片方の長い文（スレッドを手放さない）
// の途中にもう片方が流した SELECT 1 が、長い文の終わりや sysmon（10 ms ごと）を待たずに返ることを確かめる。
// 接続の相手が同じプロセスにいると、相手のゴルーチンが待つたびにスレッドが空いて netpoll に入るので、別の
// プロセスにする（このテストのバイナリを、stage128ClientEnv を付けて TestStage128Client だけで起こす）。
func TestStage128IdleConnectionReadDuringStatement(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a client process")
	}
	if os.Getenv(stage128ClientEnv) != "" {
		t.Skip("client process")
	}
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 (the spinning statement holds the only P)")
	}
	e, pro := setupMemDB(require.New(t))
	ctx := sql.NewContext(context.Background(), sql.WithSession(memory.NewSession(sql.NewBaseSession(), pro)))
	e.Analyzer.Catalog.RegisterFunction(ctx, sql.NewFunction0("heimdb_test_spin", func() sql.Expression { return stage128Spin{} }))
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l := stage128Listener{tl}
	s, err := NewServer(Config{Listener: l}, e, sql.NewContext, testSessionBuilder(pro), nil)
	require.NoError(t, err)
	go s.Start()
	t.Cleanup(func() { s.Close() })

	cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, os.Args[0], "-test.run=^TestStage128Client$", "-test.v")
	cmd.Env = append(os.Environ(), stage128ClientEnv+"="+l.Addr().String())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	var lat []time.Duration
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "stage128 latency_us "); ok {
			us, err := strconv.Atoi(v)
			require.NoError(t, err)
			lat = append(lat, time.Duration(us)*time.Microsecond)
		}
	}
	require.Len(t, lat, 9, string(out))
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("SELECT 1 during the spinning statement: %v", lat)
	// 直さないと、届いた文は sysmon の 10 ms ごとの netpoll か長い文の終わりまで読まれない（中央値で数 ms）。
	require.Less(t, lat[len(lat)/2], 2*time.Millisecond, "median")
}

// TestStage128Client は、TestStage128IdleConnectionReadDuringStatement が起こす接続の相手。
func TestStage128Client(t *testing.T) {
	addr := os.Getenv(stage128ClientEnv)
	if addr == "" {
		t.Skip("run by TestStage128IdleConnectionReadDuringStatement")
	}
	db, err := gosql.Open("mysql", "root@tcp("+addr+")/test")
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	a, err := db.Conn(ctx)
	require.NoError(t, err)
	b, err := db.Conn(ctx)
	require.NoError(t, err)
	var one int
	for _, c := range []*gosql.Conn{a, b} {
		require.NoError(t, c.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	}
	for i := 0; i < 9; i++ {
		done := make(chan error, 1)
		go func() {
			var v int64
			done <- a.QueryRowContext(ctx, "SELECT heimdb_test_spin()").Scan(&v)
		}()
		time.Sleep(3*time.Millisecond + time.Duration(i)*300*time.Microsecond)
		start := time.Now()
		require.NoError(t, b.QueryRowContext(ctx, "SELECT 1").Scan(&one))
		d := time.Since(start)
		select {
		case err := <-done:
			t.Fatalf("the spinning statement ended before SELECT 1 returned (%v)", err)
		default:
		}
		fmt.Printf("stage128 latency_us %d\n", d.Microseconds())
		require.NoError(t, <-done)
	}
}
