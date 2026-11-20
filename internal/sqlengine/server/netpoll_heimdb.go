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

// Modified by Tsukinowa Inc.: 文を始めるときに、ほかの接続が次の文を待ち、文の途中の接続がほかになければ眠った Go のスレッドを起こす（段階 128。docs/engine.md）

package server

// HeimDB（段階 128）: Go のランタイムでは、網の読みを待つゴルーチンを起こすのは、仕事のないスレッドが入る
// 止まる netpoll か、ゴルーチンを止めたスレッドが次を探すときの止まらない netpoll か、sysmon（10 ms ごと）
// だけになる。止まらない netpoll で見つけたゴルーチンをそのまま走らせると、ほかのスレッドは起こさないので、
// 網を見ているスレッドがいなくなる。2 本の接続では、片方の文の間に届いたもう片方の次の文が、片方が止まって
// 同じスレッドが netpoll するまで読まれず、2 つの接続が 1 つのスレッドで交互に走った（GCE で 2 接続が
// 1 接続の 1.57 倍）。MySQL は接続ごとのスレッドが読みで止まるので起きない。
//
// 文を始めるときに、開いている接続のうち文を処理していないものがあれば、空のゴルーチンを起こす。go 文は、
// 空いた P があり探しているスレッドがいなければ、眠ったスレッドを 1 つ起こす（runtime の wakep）。起きた
// スレッドは空のゴルーチンを走らせた後に仕事がなく、止まる netpoll に入るので、文の間に届いたほかの接続の
// 文をすぐに読める。P が全部ふさがっていれば何もしない。接続が 1 本だけのときは起こさない（ゴルーチンを
// 止めたスレッドがそのまま止まる netpoll に入るので要らず、スレッドの切り替えの手間だけが増える）。
//
// 起こすのは、ほかに文を処理している接続がないとき（文の途中の接続がこれだけのとき）に限る。2 本以上が文の
// 途中なら、どれかが止まるときのスレッドの止まる netpoll か injectglist が網を見る。文のたびに起こすと、
// 起きたスレッドがさらにもう 1 つ起こし、4 接続と決まった速さの負荷で 1,000 文あたりの CPU が 9〜10% 増えた
// （GCE。この条件では増えず、2 接続の伸びは同じ）。

// wakeIdleThread は、眠った Go のスレッドを起こす（テストで替える）。
var wakeIdleThread = func() { go func() {}() }

// commandStarted は、文を処理し始めるときに呼ぶ。commandDone と組にする。
func (h *Handler) commandStarted() {
	if h.busyConns.Add(1) == 1 && h.openConns.Load() > 1 {
		wakeIdleThread()
	}
}

// commandDone は、文の処理を終えたときに呼ぶ。
func (h *Handler) commandDone() {
	h.busyConns.Add(-1)
}
