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

// Modified by Tsukinowa Inc.: 止めるときに受け付けた接続が来ても panic しないことのテスト（上流の #3630。docs/engine.md）

package server

import (
	"net"
	"sync"
	"testing"

	"golang.org/x/sync/errgroup"
)

// lateListener は、Close の後に、待たせておいた接続を 1 つ返す net.Listener。
type lateListener struct {
	release chan struct{}
	once    sync.Once
	done    bool
}

func (l *lateListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, net.ErrClosed
	}
	<-l.release
	l.done = true
	c, _ := net.Pipe()
	return c, nil
}

func (l *lateListener) Close() error   { return nil }
func (l *lateListener) Addr() net.Addr { return &net.TCPAddr{} }

// 止める途中に受け付けた接続が来ても、閉じた conns に送って panic しない（前は Close が shutdown と conns の両方を
// 閉じ、受け付けの select が conns への送信を選ぶと send on closed channel で落ちた）。
func TestListenerCloseWithLateConn(t *testing.T) {
	for i := 0; i < 200; i++ {
		nl := &lateListener{release: make(chan struct{})}
		l := &Listener{netListener: nl, conns: make(chan connRes), eg: new(errgroup.Group), shutdown: make(chan struct{}), once: &sync.Once{}}
		l.eg.Go(func() error { return l.acceptLoop(nl) })
		accepted := make(chan error, 1)
		go func() {
			_, err := l.Accept()
			accepted <- err
		}()
		closed := make(chan error, 1)
		go func() { closed <- l.Close() }()
		<-l.shutdown // Close が shutdown を閉じ、受け付けの終わりを待っている
		close(nl.release)
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		<-accepted
	}
}
