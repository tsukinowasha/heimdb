// Copyright 2020-2022 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 止めるときに conns を閉じず、Accept が shutdown を見る（上流の #3630）。受け付けのループを acceptLoop に切り出した（テストのため。docs/engine.md）

package server

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/sync/errgroup"
)

var UnixSocketInUseError = errors.New("bind address at given unix socket path is already in use")

// connRes represents a connection made to a listener and an error result
type connRes struct {
	conn net.Conn
	err  error
}

// Listener implements a single listener with two net.Listener,
// one for TCP socket and another for unix socket connections.
type Listener struct {
	// netListener is a tcp socket listener
	netListener net.Listener
	// unixListener is a unix socket listener
	unixListener net.Listener
	eg           *errgroup.Group
	// channel to receive connections on either listener
	conns chan connRes
	// channel to close both listener
	shutdown chan struct{}
	once     *sync.Once
}

// NewListener creates a new Listener.
// 'protocol' takes "tcp" and 'address' takes "host:port" information for TCP socket connection.
// For unix socket connection, 'unixSocketPath' takes a path for the unix socket file.
// If 'unixSocketPath' is empty, no need to create the second listener.
func NewListener(protocol, address string, unixSocketPath string) (*Listener, error) {
	netl, err := newNetListener(protocol, address)
	if err != nil {
		return nil, err
	}

	var unixl net.Listener
	var unixSocketInUse error
	if unixSocketPath != "" {
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("unable to create unix socket listener on Windows")
		}
		unixListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: unixSocketPath, Net: "unix"})
		if err == nil {
			unixl = unixListener
		} else if errors.Is(err, syscall.EADDRINUSE) {
			// we continue if error is unix socket bind address is already in use
			// we return UnixSocketInUseError error to track the error back to where server gets started and add warning
			unixSocketInUse = UnixSocketInUseError
		} else {
			return nil, err
		}
	}

	l := &Listener{
		netListener:  netl,
		unixListener: unixl,
		conns:        make(chan connRes),
		eg:           new(errgroup.Group),
		shutdown:     make(chan struct{}),
		once:         &sync.Once{},
	}
	l.eg.Go(func() error { return l.acceptLoop(l.netListener) })
	if l.unixListener != nil {
		l.eg.Go(func() error { return l.acceptLoop(l.unixListener) })
	}

	return l, unixSocketInUse
}

// acceptLoop passes the connections accepted by nl to Accept until the listener shuts down.
func (l *Listener) acceptLoop(nl net.Listener) error {
	for {
		conn, err := nl.Accept()
		// connection can be closed already from the other goroutine
		if errors.Is(err, net.ErrClosed) {
			return nil
		}

		select {
		case <-l.shutdown:
			conn.Close()
			return nil
		case l.conns <- connRes{conn, err}:
		}
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case cr := <-l.conns:
		return cr.conn, cr.err
	case <-l.shutdown:
		return nil, net.ErrClosed
	}
}

func (l *Listener) Close() error {
	err := l.netListener.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if l.unixListener != nil {
		err = l.unixListener.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	l.once.Do(func() {
		close(l.shutdown)
	})
	return l.eg.Wait()
}

func (l *Listener) Addr() net.Addr {
	return l.netListener.Addr()
}
