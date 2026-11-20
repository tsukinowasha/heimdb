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

// Modified by Tsukinowa Inc.: 行を文のゴルーチンで読んで送る経路と、5 秒の後に始める切れた接続の見張りのテスト（段階 124。docs/engine.md）

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/stretchr/testify/require"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/internal/sockstate"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// goroutineID は、呼んだゴルーチンの番号を返す。
func goroutineID() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	n, _ := strconv.ParseUint(string(b[:bytes.IndexByte(b, ' ')]), 10, 64)
	return n
}

// stage124Iter は、next が返す行を返す sql.RowIter。Next と Close を呼んだゴルーチンと順を覚える。
type stage124Iter struct {
	next        func(ctx *sql.Context, i int) (sql.Row, error)
	calls       int
	closes      atomic.Int32
	closePan    bool
	nextGIDs    map[uint64]bool
	closeGID    uint64
	events      *[]string
	closeErr    error
	closeCtxErr error // Close のときの文の context の Err
}

func (it *stage124Iter) Next(ctx *sql.Context) (sql.Row, error) {
	if it.nextGIDs == nil {
		it.nextGIDs = map[uint64]bool{}
	}
	it.nextGIDs[goroutineID()] = true
	i := it.calls
	it.calls++
	return it.next(ctx, i)
}

func (it *stage124Iter) Close(ctx *sql.Context) error {
	it.closes.Add(1)
	it.closeGID = goroutineID()
	it.closeCtxErr = ctx.Err()
	if it.events != nil {
		*it.events = append(*it.events, "close")
	}
	if it.closePan {
		panic("close panicked")
	}
	return it.closeErr
}

// rowsUpTo は、n 行（0 から n-1 の数）を返して終わる next。
func rowsUpTo(n int) func(*sql.Context, int) (sql.Row, error) {
	return func(_ *sql.Context, i int) (sql.Row, error) {
		if i >= n {
			return nil, io.EOF
		}
		return sql.NewRow(int64(i)), nil
	}
}

// blockUntilDone は、文の context が取り消されるまで止まる next（SLEEP と同じく context.Canceled を返す）。
func blockUntilDone(ctx *sql.Context, _ int) (sql.Row, error) {
	<-ctx.Done()
	return nil, context.Canceled
}

var stage124Schema = sql.Schema{{Name: "n", Type: types.Int64}}

type stage124Run struct {
	batches []int
	rows    int
	gids    map[uint64]bool
}

// runDefaultIter は、resultForDefaultIter を conn と ctx で呼ぶ。callback は送った行を数える（cb が nil でなければ
// それも呼ぶ）。
func runDefaultIter(t *testing.T, h *Handler, ctx *sql.Context, c *mysql.Conn, it sql.RowIter, cb func(*sqltypes.Result) error) (*sqltypes.Result, bool, *stage124Run, error) {
	t.Helper()
	run := &stage124Run{gids: map[uint64]bool{}}
	buf := sql.ByteBufPool.Get().(*sql.ByteBuffer)
	defer func() {
		buf.Reset()
		sql.ByteBufPool.Put(buf)
	}()
	r, processed, err := h.resultForDefaultIter(ctx, c, stage124Schema, it, func(r *sqltypes.Result, more bool) error {
		run.gids[goroutineID()] = true
		run.batches = append(run.batches, len(r.Rows))
		run.rows += len(r.Rows)
		if cb != nil {
			return cb(r)
		}
		return nil
	}, schemaToFields(ctx, stage124Schema), false, buf)
	return r, processed, run, err
}

func stage124Context(parent context.Context) *sql.Context {
	pro := memory.NewDBProvider(memory.NewDatabase("test"))
	ctx := sql.NewContext(parent, sql.WithSession(memory.NewSession(sql.NewBaseSession(), pro)))
	ctx.GetLogger() // doQuery makes the session's logger before the rows are read
	return ctx
}

// tcpPair は、つながった TCP の接続の組（サーバーの側と接続先の側）を返す。
func tcpPair(t *testing.T) (server, client *net.TCPConn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	cc, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	sc := <-accepted
	require.NotNil(t, sc)
	t.Cleanup(func() {
		sc.Close()
		cc.Close()
	})
	return sc.(*net.TCPConn), cc.(*net.TCPConn)
}

// stage124Handler は、1010 行の表 test を持つメモリのデータベースの Handler を作る。
func stage124Handler(t *testing.T) *Handler {
	e, pro := setupMemDB(require.New(t))
	return &Handler{
		e: e,
		sm: NewSessionManager(sql.NewContext, testSessionBuilder(pro), sql.NoopTracer, pro.Database,
			sql.NewMemoryManager(nil), sqle.NewProcessList(), "foo"),
	}
}

// TestStage124ResultBatches は、行を 128 行ずつ送り、端数を最後に送ること、128 の倍数ならその後に空の結果を
// 送らないこと、0 行なら列の情報だけの結果を 1 つ送ることを確かめる（もとの経路と同じ）。
func TestStage124ResultBatches(t *testing.T) {
	h := stage124Handler(t)
	c := newConn(1)
	h.NewConnection(c)
	require.NoError(t, h.ComInitDB(c, "test"))
	for _, n := range []int{0, 1, 127, 128, 129, 256, 300, 1010} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			var batches []int
			var got []int64
			fields := 0
			err := h.ComQuery(context.Background(), c, fmt.Sprintf("SELECT c1 FROM test ORDER BY c1 LIMIT %d", n), func(r *sqltypes.Result, more bool) error {
				require.False(t, more)
				batches = append(batches, len(r.Rows))
				require.EqualValues(t, len(r.Rows), r.RowsAffected)
				if len(r.Fields) == 1 {
					fields++
				}
				for _, row := range r.Rows {
					v, err := strconv.ParseInt(row[0].ToString(), 10, 64)
					require.NoError(t, err)
					got = append(got, v)
				}
				return nil
			})
			require.NoError(t, err)
			var want []int
			for left := n; left > 0; left -= rowsBatch {
				want = append(want, min(left, rowsBatch))
			}
			if n == 0 {
				want = []int{0}
			}
			require.Equal(t, want, batches)
			require.Equal(t, len(batches), fields)
			require.Len(t, got, n)
			for i, v := range got {
				require.EqualValues(t, i, v)
			}
		})
	}
}

// TestStage124ResultOnHandlerGoroutine は、行を読む、送る、閉じるのを、文を受けたゴルーチンで行い、文ごとに
// ゴルーチンを起こさないことを確かめる（段階 124 の変更。もとは errgroup の 4 つのゴルーチンで行った）。
func TestStage124ResultOnHandlerGoroutine(t *testing.T) {
	h := &Handler{}
	ctx := stage124Context(context.Background())
	me := goroutineID()
	base := runtime.NumGoroutine()
	extra := -1
	it := &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		if i == 200 {
			extra = runtime.NumGoroutine() - base
		}
		return rowsUpTo(300)(ctx, i)
	}}
	r, processed, run, err := runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, []int{128, 128}, run.batches)
	require.Len(t, r.Rows, 44)
	require.Equal(t, map[uint64]bool{me: true}, it.nextGIDs)
	require.Equal(t, map[uint64]bool{me: true}, run.gids)
	require.Equal(t, me, it.closeGID)
	require.EqualValues(t, 1, it.closes.Load())
	require.Equal(t, 0, extra, "goroutines started by the statement")
}

// TestStage124OkResult は、行の iterator が返す OkResult を結果にし、行の後の OkResult を panic のエラーにすることを
// 確かめる。
func TestStage124OkResult(t *testing.T) {
	h := &Handler{}
	ctx := stage124Context(context.Background())
	it := &stage124Iter{next: func(_ *sql.Context, i int) (sql.Row, error) {
		if i == 0 {
			return sql.NewRow(types.OkResult{RowsAffected: 3, InsertID: 7}), nil
		}
		return nil, io.EOF
	}}
	r, processed, _, err := runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.NoError(t, err)
	require.False(t, processed)
	require.EqualValues(t, 3, r.RowsAffected)
	require.EqualValues(t, 7, r.InsertID)
	require.EqualValues(t, 1, it.closes.Load())

	mixed := &stage124Iter{next: func(_ *sql.Context, i int) (sql.Row, error) {
		switch i {
		case 0:
			return sql.NewRow(int64(1)), nil
		case 1:
			return sql.NewRow(types.NewOkResult(1)), nil
		}
		return nil, io.EOF
	}}
	r, _, _, err = runDefaultIter(t, h, ctx, newConn(1), mixed, nil)
	require.Nil(t, r)
	var pe *QueryPanicError
	require.True(t, errors.As(err, &pe), "%v", err)
	require.Equal(t, "Got OkResult mixed with RowResult", pe.Value)
	require.EqualValues(t, 1, mixed.closes.Load())
}

// TestStage124ErrorMidIteration は、途中の行のエラーを返し、それまでの組を送り、送った後に一度だけ閉じることを
// 確かめる。送るときのエラーと閉じるときのエラーも返す。
func TestStage124ErrorMidIteration(t *testing.T) {
	h := &Handler{}
	ctx := stage124Context(context.Background())
	boom := errors.New("boom")
	var events []string
	it := &stage124Iter{events: &events, next: func(_ *sql.Context, i int) (sql.Row, error) {
		if i == 200 {
			return nil, boom
		}
		return sql.NewRow(int64(i)), nil
	}}
	r, processed, run, err := runDefaultIter(t, h, ctx, newConn(1), it, func(*sqltypes.Result) error {
		events = append(events, "send")
		return nil
	})
	require.ErrorIs(t, err, boom)
	require.Nil(t, r)
	require.False(t, processed)
	require.Equal(t, []int{128}, run.batches)
	require.Equal(t, []string{"send", "close"}, events)

	sendErr := errors.New("client went away")
	it = &stage124Iter{next: rowsUpTo(1000)}
	_, _, run, err = runDefaultIter(t, h, ctx, newConn(1), it, func(*sqltypes.Result) error { return sendErr })
	require.ErrorIs(t, err, sendErr)
	require.Equal(t, []int{128}, run.batches)
	require.Equal(t, 128, it.calls) // the first batch failed to send before the 129th row was read
	require.EqualValues(t, 1, it.closes.Load())

	closeErr := errors.New("close failed")
	it = &stage124Iter{next: rowsUpTo(3), closeErr: closeErr}
	r, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.ErrorIs(t, err, closeErr)
	require.Nil(t, r)

	// 行のエラーは、閉じるときのエラーより先に返す。
	it = &stage124Iter{next: func(*sql.Context, int) (sql.Row, error) { return nil, boom }, closeErr: closeErr}
	_, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.ErrorIs(t, err, boom)
}

// TestStage124Panics は、行を読む、送る、閉じるときの panic を QueryPanicError にし、閉じることを確かめる。
func TestStage124Panics(t *testing.T) {
	h := &Handler{}
	ctx := stage124Context(context.Background())
	var pe *QueryPanicError

	it := &stage124Iter{next: func(_ *sql.Context, i int) (sql.Row, error) {
		if i == 5 {
			panic("next panicked")
		}
		return sql.NewRow(int64(i)), nil
	}}
	r, processed, _, err := runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.True(t, errors.As(err, &pe), "%v", err)
	require.Equal(t, "next panicked", pe.Value)
	require.Nil(t, r)
	require.False(t, processed)
	require.EqualValues(t, 1, it.closes.Load())

	it = &stage124Iter{next: rowsUpTo(300)}
	_, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, func(*sqltypes.Result) error { panic("send panicked") })
	require.True(t, errors.As(err, &pe), "%v", err)
	require.Equal(t, "send panicked", pe.Value)
	require.EqualValues(t, 1, it.closes.Load())

	it = &stage124Iter{next: rowsUpTo(3), closePan: true}
	r, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.True(t, errors.As(err, &pe), "%v", err)
	require.Equal(t, "close panicked", pe.Value)
	require.Nil(t, r)
}

// TestStage124Cancellation は、文の context の取り消し（KILL QUERY、実行時間の上限）を、止まった Next の中でも
// 行の間でも、取り消しの理由のエラーで返すことを確かめる。
func TestStage124Cancellation(t *testing.T) {
	h := &Handler{}
	cause := errors.New("query killed")

	parent, cancel := context.WithCancelCause(context.Background())
	ctx := stage124Context(parent)
	it := &stage124Iter{next: blockUntilDone}
	time.AfterFunc(50*time.Millisecond, func() { cancel(cause) })
	_, _, _, err := runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.ErrorIs(t, err, cause)
	require.EqualValues(t, 1, it.closes.Load())

	// 行が途切れずに来ても、行ごとに取り消しを見る（取り消した後の Next は呼ばない）。TCP の接続でも同じ。
	for _, tcp := range []bool{false, true} {
		parent, cancel = context.WithCancelCause(context.Background())
		ctx = stage124Context(parent)
		it = &stage124Iter{next: func(_ *sql.Context, i int) (sql.Row, error) {
			if i == 1000 {
				cancel(cause)
			}
			return sql.NewRow(int64(i)), nil
		}}
		c := newConn(1)
		if tcp {
			sc, _ := tcpPair(t)
			c = &mysql.Conn{ConnectionID: 1, Conn: sc}
		}
		_, _, run, err := runDefaultIter(t, h, ctx, c, it, nil)
		require.ErrorIs(t, err, cause)
		require.Equal(t, 1001, it.calls)
		require.Equal(t, 7, len(run.batches))
		require.EqualValues(t, 1, it.closes.Load())
	}

	// 取り消されていない文のエラーは、そのまま返す。
	ctx = stage124Context(context.Background())
	it = &stage124Iter{next: func(*sql.Context, int) (sql.Row, error) { return nil, context.DeadlineExceeded }}
	_, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestStage124ReadTimeout は、readTimeout を、次の行を待つ時間に掛けることを確かめる（止まった Next は戻す。
// 行が間に合えば、文全体が長くても止めない。接続先へ送る時間は数えない）。
func TestStage124ReadTimeout(t *testing.T) {
	h := &Handler{readTimeout: 100 * time.Millisecond}
	ctx := stage124Context(context.Background())

	start := time.Now()
	it := &stage124Iter{next: blockUntilDone}
	_, _, _, err := runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.True(t, ErrRowTimeout.Is(err), "%v", err)
	require.Less(t, time.Since(start), 2*time.Second)
	require.EqualValues(t, 1, it.closes.Load())

	// 遅れて来る最初の行も同じ。
	it = &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			return sql.NewRow(int64(i)), nil
		case <-ctx.Done():
			return nil, context.Canceled
		}
	}}
	_, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.True(t, ErrRowTimeout.Is(err), "%v", err)

	it = &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		time.Sleep(30 * time.Millisecond)
		return rowsUpTo(10)(ctx, i)
	}}
	r, _, _, err := runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.NoError(t, err)
	require.Len(t, r.Rows, 10)

	it = &stage124Iter{next: rowsUpTo(300)}
	r, _, run, err := runDefaultIter(t, h, ctx, newConn(1), it, func(*sqltypes.Result) error {
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []int{128, 128}, run.batches)
	require.Len(t, r.Rows, 44)

	h = &Handler{}
	it = &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		time.Sleep(150 * time.Millisecond)
		return rowsUpTo(2)(ctx, i)
	}}
	r, _, _, err = runDefaultIter(t, h, ctx, newConn(1), it, nil)
	require.NoError(t, err)
	require.Len(t, r.Rows, 2)
}

// stage124SockHooks は、切れた接続の見張りの /proc の読み出しを置き換えて、呼んだ回数を数える。
type stage124SockHooks struct {
	inodes, states atomic.Int32
	state          atomic.Int32
}

func setStage124SockHooks(t *testing.T, sleep time.Duration) *stage124SockHooks {
	hk := &stage124SockHooks{}
	hk.state.Store(int32(sockstate.Other))
	oldInode, oldState, oldSleep := getConnInode, getInodeSockState, tcpCheckerSleepDuration
	getConnInode = func(*net.TCPConn) (uint64, error) {
		hk.inodes.Add(1)
		return 42, nil
	}
	getInodeSockState = func(int, uint64) (sockstate.SockState, error) {
		hk.states.Add(1)
		return sockstate.SockState(hk.state.Load()), nil
	}
	tcpCheckerSleepDuration = sleep
	t.Cleanup(func() {
		getConnInode, getInodeSockState, tcpCheckerSleepDuration = oldInode, oldState, oldSleep
	})
	return hk
}

// TestStage124ClosedConnectionPoll は、切れた接続の見張りを tcpCheckerSleepDuration の後にだけ始め（短い文では
// /proc を読まない）、切れていれば文を ErrConnectionWasClosed で止め、文の終わりに止めることを確かめる。
func TestStage124ClosedConnectionPoll(t *testing.T) {
	hk := setStage124SockHooks(t, 40*time.Millisecond)
	h := &Handler{}
	sc, _ := tcpPair(t)
	c := &mysql.Conn{ConnectionID: 1, Conn: sc}

	// 短い文は見張らない。
	for i := 0; i < 20; i++ {
		_, _, _, err := runDefaultIter(t, h, stage124Context(context.Background()), c, &stage124Iter{next: rowsUpTo(300)}, nil)
		require.NoError(t, err)
	}
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 0, hk.inodes.Load())
	require.EqualValues(t, 0, hk.states.Load())

	// 長い文で接続が切れていれば止める。
	hk.state.Store(int32(sockstate.Broken))
	start := time.Now()
	it := &stage124Iter{next: blockUntilDone}
	_, _, _, err := runDefaultIter(t, h, stage124Context(context.Background()), c, it, nil)
	require.True(t, ErrConnectionWasClosed.Is(err), "%v", err)
	require.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)
	require.EqualValues(t, 1, hk.inodes.Load())
	require.EqualValues(t, 1, hk.states.Load())
	require.EqualValues(t, 1, it.closes.Load())

	// 長い文で接続がつながっていれば、tcpCheckerSleepDuration ごとに見て、文の終わりに止める。
	hk.state.Store(int32(sockstate.Other))
	hk.inodes.Store(0)
	hk.states.Store(0)
	it = &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		if i == 0 {
			time.Sleep(200 * time.Millisecond)
		}
		return rowsUpTo(3)(ctx, i)
	}}
	r, _, _, err := runDefaultIter(t, h, stage124Context(context.Background()), c, it, nil)
	require.NoError(t, err)
	require.Len(t, r.Rows, 3)
	require.EqualValues(t, 1, hk.inodes.Load())
	n := hk.states.Load()
	require.GreaterOrEqual(t, n, int32(2))
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, n, hk.states.Load(), "the poller kept running after the statement")
	require.NoError(t, it.closeCtxErr, "the iterator was closed with a cancelled context")
}

// TestStage124PollPanic は、見張りの panic を回復して、文を QueryPanicError で止めることを確かめる（プロセスを
// 落とさない）。
func TestStage124PollPanic(t *testing.T) {
	setStage124SockHooks(t, 20*time.Millisecond)
	getConnInode = func(*net.TCPConn) (uint64, error) { panic("inode lookup failed") }
	sc, _ := tcpPair(t)
	it := &stage124Iter{next: blockUntilDone}
	_, _, _, err := runDefaultIter(t, &Handler{}, stage124Context(context.Background()), &mysql.Conn{ConnectionID: 1, Conn: sc}, it, nil)
	var pe *QueryPanicError
	require.True(t, errors.As(err, &pe), "%v", err)
	require.EqualValues(t, 1, it.closes.Load())
}

// TestStage124PollNotTCP は、TCP でない接続では見張りを始めないことを確かめる。
func TestStage124PollNotTCP(t *testing.T) {
	hk := setStage124SockHooks(t, 10*time.Millisecond)
	it := &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		if i == 0 {
			time.Sleep(60 * time.Millisecond)
		}
		return rowsUpTo(1)(ctx, i)
	}}
	_, _, _, err := runDefaultIter(t, &Handler{}, stage124Context(context.Background()), newConn(1), it, nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, hk.inodes.Load())
}

// TestStage124ClosedConnectionLinux は、Linux で本物の /proc を読み、接続先が接続を閉じた長い文を止めることを
// 確かめる（ほかの OS では見張りが動かないので飛ばす）。
func TestStage124ClosedConnectionLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("socket checks are implemented only on Linux")
	}
	old := tcpCheckerSleepDuration
	tcpCheckerSleepDuration = 100 * time.Millisecond
	t.Cleanup(func() { tcpCheckerSleepDuration = old })
	sc, cc := tcpPair(t)
	if inode, err := sockstate.GetConnInode(sc); err != nil || inode == 0 {
		t.Skipf("socket checks are not available here: %v", err)
	}
	c := &mysql.Conn{ConnectionID: 1, Conn: sc}
	time.AfterFunc(300*time.Millisecond, func() { cc.Close() })
	start := time.Now()
	it := &stage124Iter{next: blockUntilDone}
	_, _, _, err := runDefaultIter(t, &Handler{}, stage124Context(context.Background()), c, it, nil)
	require.True(t, ErrConnectionWasClosed.Is(err), "%v", err)
	require.Less(t, time.Since(start), 5*time.Second)

	// つながったままの接続では、見張りが動いても止めない。
	sc2, _ := tcpPair(t)
	it = &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
		if i == 0 {
			time.Sleep(450 * time.Millisecond)
		}
		return rowsUpTo(2)(ctx, i)
	}}
	r, _, _, err := runDefaultIter(t, &Handler{}, stage124Context(context.Background()), &mysql.Conn{ConnectionID: 2, Conn: sc2}, it, nil)
	require.NoError(t, err)
	require.Len(t, r.Rows, 2)
}

// TestStage124Concurrent は、多くの接続が同時に文を流しても結果が正しいことを確かめる（-race で走らせる）。
func TestStage124Concurrent(t *testing.T) {
	h := stage124Handler(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			c := newConn(uint32(10 + g))
			h.NewConnection(c)
			defer h.ConnectionClosed(c)
			if err := h.ComInitDB(c, "test"); err != nil {
				errs <- err
				return
			}
			for i := 0; i < 30; i++ {
				n := (g*31 + i*17) % 400
				rows := 0
				err := h.ComQuery(context.Background(), c, fmt.Sprintf("SELECT c1 FROM test WHERE c1 < %d", n), func(r *sqltypes.Result, _ bool) error {
					rows += len(r.Rows)
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
				if rows != n {
					errs <- fmt.Errorf("got %d rows, want %d", rows, n)
					return
				}
				if err := h.ComQuery(context.Background(), c, "SELECT nosuchcolumn FROM test", dummyCb); err == nil {
					errs <- errors.New("no error for an unknown column")
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestStage124NoGoroutineLeak は、行を送る経路が、成功、エラー、panic、取り消し、readTimeout、見張りを始めた文の
// どれでもゴルーチンを残さないことを確かめる。
func TestStage124NoGoroutineLeak(t *testing.T) {
	hk := setStage124SockHooks(t, 5*time.Millisecond)
	sc, _ := tcpPair(t)
	tcp := &mysql.Conn{ConnectionID: 1, Conn: sc}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()
	boom := errors.New("boom")
	for i := 0; i < 30; i++ {
		for _, c := range []*mysql.Conn{newConn(1), tcp} {
			ctx := stage124Context(context.Background())
			runDefaultIter(t, &Handler{}, ctx, c, &stage124Iter{next: rowsUpTo(300)}, nil)
			runDefaultIter(t, &Handler{}, ctx, c, &stage124Iter{next: func(*sql.Context, int) (sql.Row, error) { return nil, boom }}, nil)
			runDefaultIter(t, &Handler{}, ctx, c, &stage124Iter{next: func(*sql.Context, int) (sql.Row, error) { panic("x") }}, nil)
			runDefaultIter(t, &Handler{readTimeout: 5 * time.Millisecond}, ctx, c, &stage124Iter{next: blockUntilDone}, nil)
			runDefaultIter(t, &Handler{}, ctx, c, &stage124Iter{next: func(ctx *sql.Context, i int) (sql.Row, error) {
				if i == 0 {
					time.Sleep(15 * time.Millisecond)
				}
				return rowsUpTo(1)(ctx, i)
			}}, nil)
			parent, cancel := context.WithCancel(context.Background())
			time.AfterFunc(2*time.Millisecond, cancel)
			runDefaultIter(t, &Handler{}, stage124Context(parent), c, &stage124Iter{next: blockUntilDone}, nil)
		}
	}
	require.Greater(t, hk.inodes.Load(), int32(0), "the poller never started")
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), base)
}
