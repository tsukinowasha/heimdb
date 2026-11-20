// Package replication は、コミットの記録を本番からレプリカに HTTP で送る（docs/plan.md の段階 24）。
//
// レプリカは GET /v1/log?after=<適用した時刻> で、その時刻より後の記録をまとめて受け取る（なければ本番は少し待つ）。
// 次の要求の after が、受け取ったことの知らせになる。本文は記録ごとに「時刻 8 バイト、長さ 4 バイト、中身」を並べる。
// 認証は合言葉（Authorization: Bearer）だけで、暗号化はしない（閉じたネットワークか SSH のトンネルで使う）。
//
// 同期（段階 25）: 本番はコミットを書いた後、レプリカの知らせ（記録で確かめた after）がそのコミットに届くまで、
// 待ち時間を上限に OK を返さない（Primary.WaitAck を Store.SetCommitWait に渡す）。
package replication

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/store"
)

const (
	// maxResponseBytes は、1 回の応答に入れる記録の大きさの目安。
	maxResponseBytes = 4 << 20
	// defaultWait は、新しい記録がないときに本番が待つ時間。
	defaultWait = 5 * time.Second
	maxWait     = 30 * time.Second
	// headerCommitted は、本番の最後のコミットの時刻を返すヘッダ（レプリカの遅れの計算に使う）。
	headerCommitted = "X-HeimDB-Committed"
	// headerSync は、本番の同期の状態（sync、async、off）を返すヘッダ。
	headerSync = "X-HeimDB-Sync"
	// headerFormat は、本番のデータ形式のバージョンを返すヘッダ（docs/plan.md の段階 36）。
	headerFormat = "X-HeimDB-Format"
)

// formatVersion は、このプロセスが運ぶ記録の中身の形のバージョン（store.RecordFormat。テストで変える）。記録はキーと値を
// そのまま運ぶので、本番とレプリカのバージョンが違えば、記録を適用せずに止める。データ形式のバージョン（store.FormatVersion）とは
// 分ける（バージョン 3 は記録の置き方だけを変えたので、0.2.10 以前のバージョン 2 とつながる。docs/plan.md の段階 59）。
var formatVersion = store.RecordFormat

// peerFormat は、相手が示した形式のバージョン。示さない相手（0.1.5 以前）はバージョン 1（それまでの形式はすべてバージョン 1）。
func peerFormat(s string) (uint64, error) {
	if s == "" {
		return 1, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

// replicaLevel は、このプロセスがレプリカとして分かる記録の水準（docs/plan.md の段階 58）。本番が、ソフトの変更で
// 古いレプリカが正しく適用できない記録を書くようになったら上げ、minReplicaLevel もそれに合わせる。
//
//	1: 0.2.8 からの、何回かに分けてコミットする ALTER TABLE の書きかけの印（GC から守る）を知っている。
//	   水準を示さないレプリカ（0.2.10 以前）は 0 として扱う（0.2.7 以前と見分けられない）。
//	2: 0.2.19 からの、整数でない列を含む主キーの表（段階 70）を読める。0.2.18 以前のレプリカは、記録の形は同じでも
//	   その表を開くと主キーの型のエラーになる。
//	3: 段階 72 からの、主キーのない表を読める。0.2.19 以前のレプリカは、その表を開くと主キーのエラーになる。
//	4: 段階 94 からの、親の変更をブランチに取り込んだ記録（m/sync）を知っている。0.2.32 以前のレプリカは記録を受け取れても
//	   知らないので、本番に上げたときにマージの基準を読み違える。
//	5: 段階 117 からの、インデックスの定義のキーの水準を知っている。0.2.38 以前のレプリカは、水準 1 の DECIMAL、FLOAT、
//	   DOUBLE、ENUM のインデックスのエントリを記録で受け取れても、本番に上げた後の書き込みでエントリを保守しない。
//	6: 段階 120 からの、FLOAT(M,D) と DOUBLE(M,D) の型と、水準 2 の SET と BIT のインデックスを知っている。0.2.39 と
//	   0.2.40 のレプリカは、float(7,2) の定義を桁のない float と読み、本番に上げた後の書き込みで丸めず、SET と BIT の
//	   インデックスのエントリを保守しない。
//
// データ形式のバージョン（formatVersion）と違い、記録の形は同じでも、適用する側のソフトの振る舞いが要るもの。
var replicaLevel uint64 = 6

// minReplicaLevel は、本番が受け付けるレプリカの水準の下限（テストで変える）。
var minReplicaLevel uint64 = 6

func replicaTooOld(level uint64) error {
	return fmt.Errorf("HeimDB: the replica is too old for this primary (replica level %d, the primary needs %d; HeimDB 0.2.10 and earlier show level 0, 0.2.18 and earlier level 1, 0.2.19 level 2, 0.2.20 to 0.2.32 level 3, 0.2.33 to 0.2.38 level 4, 0.2.39 to 0.2.40 level 5); upgrade the replica to the primary's version first", level, minReplicaLevel)
}

func formatMismatch(primary, replica uint64) error {
	return fmt.Errorf("HeimDB: data format version differs (primary %d, replica %d); run the same HeimDB data format on both and rebuild the replica from a backup of the primary", primary, replica)
}

// 同期の状態。
const (
	syncOff   = "off"
	syncAsync = "async"
	syncOn    = "sync"
)

// StatusItem は heimdb_status の 1 行。
type StatusItem struct {
	Name  string
	Value any
}

// Primary は本番の側。http.Handler として、レプリカに記録を返す。
type Primary struct {
	st    *store.Store
	token string

	mu       sync.Mutex
	replicas map[string]*replicaSeen
	// closed は Close の後。inflight は応答の途中の要求（ストアを閉じる前に終わりを待つ）。
	closed   bool
	inflight sync.WaitGroup

	// 同期（段階 25）。timeout は知らせを待つ上限（0 なら同期しない）。acked は記録で確かめた知らせの位置の最大、
	// ackedBy はそれを知らせたレプリカ。ackCh は acked か mode が変わるたびに閉じて作り直す。
	timeout    time.Duration
	mode       string
	since      time.Time
	asyncCount int
	acked      uint64
	ackedBy    string
	ackCh      chan struct{}
	waits      int
	waitTotal  time.Duration
}

// Close は、新しい要求を拒否し、応答の途中の要求が終わるのを待つ。
func (p *Primary) Close() {
	p.mu.Lock()
	p.closed = true
	if p.mode != syncOff {
		p.mode = syncOff
		p.wake()
	}
	p.mu.Unlock()
	p.inflight.Wait()
}

func (p *Primary) enter() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.inflight.Add(1)
	return true
}

type replicaSeen struct {
	addr     string
	position uint64
	at       time.Time
	err      string
}

// NewPrimary は、st の記録を合言葉 token で返す Primary を作る。syncTimeout が 0 でなければ同期のレプリカとして、
// コミットを見せる前にレプリカの知らせを最大 syncTimeout 待つ（非同期から始め、レプリカが追いついたら同期にする）。
func NewPrimary(st *store.Store, token string, syncTimeout time.Duration) *Primary {
	p := &Primary{st: st, token: token, replicas: map[string]*replicaSeen{}, timeout: syncTimeout,
		mode: syncOff, since: time.Now(), ackCh: make(chan struct{})}
	if syncTimeout > 0 && st.IsReplica() {
		// レプリカが送り出す（連鎖する）ときは、自分ではコミットしないので同期しない
		p.timeout = 0
	} else if syncTimeout > 0 {
		p.mode = syncAsync
		st.SetCommitWait(p.WaitAck)
		logrus.Infof("replication: async until a replica catches up (sync timeout %s)", syncTimeout)
	}
	return p
}

// WaitAck は、コミット ts をレプリカが適用したと知らせるまで待つ（同期の間だけ）。待ち時間を過ぎたら非同期に切り替える。
// Store.Update が書き込みのロックの中で呼ぶ。
func (p *Primary) WaitAck(ts uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode != syncOn {
		return
	}
	start := time.Now()
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	for p.mode == syncOn && p.acked < ts {
		ch := p.ackCh
		p.mu.Unlock()
		timedOut := false
		select {
		case <-ch:
		case <-timer.C:
			timedOut = true
		}
		p.mu.Lock()
		if timedOut && p.mode == syncOn && p.acked < ts {
			p.setMode(syncAsync)
			logrus.Warnf("replication: no replica acknowledged commit %s within %s; switched to async (replica position %s)",
				formatTS(ts), p.timeout, formatTS(p.acked))
		}
	}
	p.waits++
	p.waitTotal += time.Since(start)
}

// setMode は同期の状態を変え、待っている WaitAck を起こす。p.mu を持って呼ぶ。
func (p *Primary) setMode(mode string) {
	p.mode = mode
	p.since = time.Now()
	if mode == syncAsync {
		p.asyncCount++
	}
	p.wake()
}

func (p *Primary) wake() {
	close(p.ackCh)
	p.ackCh = make(chan struct{})
}

// ack は、レプリカ name が位置 position まで適用したという知らせ（記録で確かめた after）を受け取る。
// 非同期の間にレプリカが書いた最後のコミットまで追いついたら、同期に戻す。
func (p *Primary) ack(name string, position uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if position > p.acked {
		p.acked, p.ackedBy = position, name
		p.wake()
	}
	// written は増えるだけなので、ここで読んだ値まで知らせが届いていれば、それまでに OK を返したコミットはすべて
	// レプリカにある。この後に書いたコミットは、同期の WaitAck で待つ。
	if p.mode == syncAsync && p.acked >= p.st.Written() {
		p.setMode(syncOn)
		logrus.Infof("replication: replica %s caught up at %s; switched to sync", name, formatTS(p.acked))
	}
}

// Finish は、止めるときに、書いた最後のコミットの知らせを待ち、その結果をログに出して、同期をやめる（この後の
// コミットは待たない）。知らせが同期の待ち時間のあいだ進まなければ、待つのをやめる（進んでいる間は、適用に時間の
// かかるレプリカを待ち続ける）。実行中の文が終わり、AUTO_INCREMENT を保存した後、送り出しを閉じる前に呼ぶ。
func (p *Primary) Finish() {
	last := p.st.Written()
	p.mu.Lock()
	defer p.mu.Unlock()
	// 非同期でも、つながっているレプリカが追いつくのを待つ（昇格させるときに、最後まで届いているように）。
	// 同期なら、最後のコミットはもう知らせを受けている。
	if p.timeout > 0 && p.acked < last && p.recentlySeen() {
		timer := time.NewTimer(p.timeout)
		defer timer.Stop()
	wait:
		for p.acked < last {
			ch, prev := p.ackCh, p.acked
			p.mu.Unlock()
			select {
			case <-ch:
				p.mu.Lock()
				if p.acked > prev {
					timer.Reset(p.timeout)
				}
			case <-timer.C:
				p.mu.Lock()
				break wait
			}
		}
	}
	switch {
	case p.acked >= last:
		logrus.Infof("replication: last commit %s was acknowledged by replica %s (%s)", formatTS(last), p.ackedBy, p.mode)
	case p.timeout == 0:
		// 同期しない構成では、レプリカが少し遅れているのが普通
		logrus.Infof("replication: last commit %s; replica position %s (sync is off)", formatTS(last), formatTS(p.acked))
	default:
		logrus.Warnf("replication: last commit %s was NOT acknowledged by any replica (replica position %s, %s)", formatTS(last), formatTS(p.acked), p.mode)
	}
	if p.mode != syncOff {
		p.mode = syncOff
		p.since = time.Now()
		p.wake()
	}
}

func (p *Primary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/log" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !p.enter() {
		http.Error(w, "HeimDB: shutting down", http.StatusServiceUnavailable)
		return
	}
	defer p.inflight.Done()
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.token)) != 1 {
		http.Error(w, "HeimDB: wrong replication token", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	w.Header().Set(headerFormat, strconv.FormatUint(formatVersion, 10))
	after, err := strconv.ParseUint(q.Get("after"), 10, 64)
	if err != nil {
		http.Error(w, "HeimDB: bad position", http.StatusBadRequest)
		return
	}
	wait := defaultWait
	if s := q.Get("wait"); s != "" {
		if wait, err = time.ParseDuration(s); err != nil || wait < 0 {
			http.Error(w, "HeimDB: bad wait", http.StatusBadRequest)
			return
		}
	}
	name := q.Get("name")
	if name == "" {
		name = r.RemoteAddr
	}
	// 形式のバージョンが違うレプリカには記録を送らない（キーと値をそのまま運ぶので、黙って食い違う）。
	if v, err := peerFormat(q.Get("format")); err != nil {
		http.Error(w, "HeimDB: bad format version", http.StatusBadRequest)
		return
	} else if v != formatVersion {
		err := formatMismatch(formatVersion, v)
		p.seen(name, r.RemoteAddr, after, err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// 水準の足りないレプリカにも送らない（記録は適用できても、本番と食い違うことがある）。
	level := uint64(0)
	if v := q.Get("level"); v != "" {
		if level, err = strconv.ParseUint(v, 10, 64); err != nil {
			http.Error(w, "HeimDB: bad replica level", http.StatusBadRequest)
			return
		}
	}
	if level < minReplicaLevel {
		err := replicaTooOld(level)
		p.seen(name, r.RemoteAddr, after, err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// 位置は受け取ったことの知らせなので、新しい記録を待つ前に覚える。同期の知らせにするのは、記録で確かめられた
	// 位置だけ（違う本番から作ったレプリカの位置を数えない）。
	p.seen(name, r.RemoteAddr, after, nil)
	recs, err := p.st.ReadRecords(after, maxResponseBytes)
	if err == nil {
		p.ack(name, after)
	}
	if err == nil && len(recs) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), min(wait, maxWait))
		if p.st.WaitCommit(ctx, after) {
			recs, err = p.st.ReadRecords(after, maxResponseBytes)
		}
		cancel()
	}
	if err != nil {
		p.seen(name, r.RemoteAddr, after, err)
	}
	switch {
	case errors.Is(err, store.ErrRecordGone):
		http.Error(w, err.Error()+" (rebuild the replica from a backup)", http.StatusGone)
		return
	case errors.Is(err, store.ErrNoLog):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set(headerCommitted, strconv.FormatUint(p.st.Written(), 10))
	p.mu.Lock()
	w.Header().Set(headerSync, p.mode)
	p.mu.Unlock()
	bw := bufio.NewWriter(w)
	for _, rec := range recs {
		var h [12]byte
		binary.BigEndian.PutUint64(h[:8], rec.TS)
		binary.BigEndian.PutUint32(h[8:], uint32(len(rec.Data)))
		bw.Write(h[:])
		bw.Write(rec.Data)
	}
	bw.Flush()
}

// recentlySeen は、最近（長く待つ要求の間隔のうちに）要求を出して、エラーになっていないレプリカがあるか。
// p.mu を持って呼ぶ。
func (p *Primary) recentlySeen() bool {
	for _, rs := range p.replicas {
		if rs.err == "" && time.Since(rs.at) < maxWait {
			return true
		}
	}
	return false
}

func (p *Primary) seen(name, addr string, position uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rs := &replicaSeen{addr: addr, position: position, at: time.Now()}
	if err != nil {
		rs.err = err.Error()
	}
	p.replicas[name] = rs
}

// Status は、本番から見たレプリカごとの状態（最後に知らせた位置、本番の最後のコミットとの差、最後に来た時刻）を返す。
func (p *Primary) Status() []StatusItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.replicas))
	for n := range p.replicas {
		names = append(names, n)
	}
	sort.Strings(names)
	committed := p.st.Written()
	out := []StatusItem{{"replication_sync", p.mode}, {"replication_sync_since", formatTS(uint64(p.since.UnixMicro()))}}
	if p.timeout > 0 {
		var avg any
		if p.waits > 0 {
			avg = (p.waitTotal / time.Duration(p.waits)).Round(time.Microsecond).String()
		}
		out = append(out, StatusItem{"replication_sync_timeout", p.timeout.String()},
			StatusItem{"replication_sync_async_switches", strconv.Itoa(p.asyncCount)},
			StatusItem{"replication_sync_ack_wait_avg", avg})
	}
	for _, n := range names {
		rs := p.replicas[n]
		v := fmt.Sprintf("addr %s, position %s, behind %s, last seen %s ago", rs.addr, formatTS(rs.position),
			behind(committed, rs.position), time.Since(rs.at).Round(time.Second))
		if rs.err != "" {
			v += ", error: " + rs.err
		}
		out = append(out, StatusItem{"replica " + n, v})
	}
	return out
}

// Replica はレプリカの側。本番から記録を受け取って適用し続ける。
type Replica struct {
	st     *store.Store
	source string // http://host:port
	token  string
	name   string
	client *http.Client

	mu          sync.Mutex
	primaryLast uint64
	primarySync string
	// lastContact は、本番がこの時刻より後に待ち始めたコミットしか知らせを待っていないと言える時刻（本番のマシンが
	// 壊れたときの判断に使う）。最後の応答が空ならその要求を送った時刻、記録を受け取ったならその前の要求を送った
	// 時刻（受け取った記録の知らせを本番が待ち始めたのは、前の要求の読み出しより後）。lastSend は最後に成功した要求を
	// 送った時刻。
	lastContact time.Time
	lastSend    time.Time
	// applyHook はテスト用（適用の前に、受け取った記録の数で呼ぶ）。
	applyHook func(n int)
	lastErr   string
	stopped   bool
}

// NewReplica は、source（host:port、http://host:port、https://host:port）の本番から、合言葉 token で記録を受け取る
// Replica を作る。name は本番の heimdb_status に出す名前。rootCAs は HTTPS で本番の証明書を確かめる CA（nil なら OS が
// 信頼するもの）。rootCAs があれば、方式のない source は https にする（docs/plan.md の段階 26）。
func NewReplica(st *store.Store, source, token, name string, rootCAs *x509.CertPool) (*Replica, error) {
	if !st.IsReplica() {
		return nil, errors.New("HeimDB: the store is not opened as a replica")
	}
	src, err := Source(source, rootCAs != nil)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12}
	return &Replica{st: st, source: src, token: token, name: name,
		client: &http.Client{Timeout: maxWait + 30*time.Second, Transport: tr}}, nil
}

// Source は、本番の指定 source を http://host:port か https://host:port にする。withCA（本番の証明書を確かめる CA
// がある）なら、方式のない source は https にし、http:// はエラーにする。
func Source(source string, withCA bool) (string, error) {
	scheme := "http"
	if withCA {
		scheme = "https"
	}
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		u, err = url.Parse(scheme + "://" + source)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("HeimDB: bad replication source %q", source)
		}
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("HeimDB: bad replication source %q (use http:// or https://)", source)
	case u.Scheme == "http" && withCA:
		return "", fmt.Errorf("HeimDB: the replication CA needs an https:// source, not %q", source)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/"):
		return "", fmt.Errorf("HeimDB: bad replication source %q (give only the scheme, host and port)", source)
	}
	return u.Scheme + "://" + u.Host, nil
}

// errStop は、続けても直らない失敗（位置が本番の記録にない、合言葉が違う）。
type errStop struct{ error }

// Run は、ctx が終わるまで記録を受け取って適用する。つながらなければ間をおいてやり直す。続けても直らない失敗
// （位置が本番の記録にない、合言葉が違う）なら止めて、そのエラーを返す（レプリカは読み取りを続ける）。
func (r *Replica) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		sent := time.Now()
		n, err := r.fetch(ctx)
		r.mu.Lock()
		if err != nil {
			r.lastErr = err.Error()
		} else {
			r.lastErr = ""
			if n == 0 {
				r.lastContact = sent
			} else {
				r.lastContact = r.lastSend
			}
			r.lastSend = sent
		}
		r.mu.Unlock()
		var stop errStop
		switch {
		case err == nil:
			backoff = time.Second
			continue
		case ctx.Err() != nil:
			return nil
		case errors.As(err, &stop):
			r.mu.Lock()
			r.stopped = true
			r.mu.Unlock()
			logrus.Errorf("replication stopped: %v", err)
			return err
		}
		logrus.Warnf("replication: %v (retrying in %s)", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil
}

// fetch は 1 回の要求で記録を受け取って適用し、適用した記録の数を返す。
func (r *Replica) fetch(ctx context.Context) (int, error) {
	q := url.Values{"after": {strconv.FormatUint(r.st.LastCommitted(), 10)}, "name": {r.name}, "format": {strconv.FormatUint(formatVersion, 10)},
		"level": {strconv.FormatUint(replicaLevel, 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.source+"/v1/log?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("primary %s: %s: %s", r.source, resp.Status, trimNL(msg))
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusConflict {
			return 0, errStop{err}
		}
		return 0, err
	}
	// 本番の形式のバージョンが違えば、記録を読まずに止める（本番が確かめない古いバージョンでも、ここで止まる）。
	if v, err := peerFormat(resp.Header.Get(headerFormat)); err != nil || v != formatVersion {
		if err != nil {
			return 0, errStop{fmt.Errorf("primary %s: bad format version header %q", r.source, resp.Header.Get(headerFormat))}
		}
		return 0, errStop{fmt.Errorf("primary %s: %w", r.source, formatMismatch(v, formatVersion))}
	}
	var recs []store.Record
	br := bufio.NewReader(resp.Body)
	for {
		var h [12]byte
		if _, err := io.ReadFull(br, h[:]); err == io.EOF {
			break
		} else if err != nil {
			return 0, fmt.Errorf("primary %s: %w", r.source, err)
		}
		data := make([]byte, binary.BigEndian.Uint32(h[8:]))
		if _, err := io.ReadFull(br, data); err != nil {
			return 0, fmt.Errorf("primary %s: %w", r.source, err)
		}
		recs = append(recs, store.Record{TS: binary.BigEndian.Uint64(h[:8]), Data: data})
	}
	if r.applyHook != nil {
		r.applyHook(len(recs))
	}
	if err := r.st.ApplyRecords(recs); err != nil {
		return 0, errStop{err}
	}
	// 本番の状態は、記録を受け取りきった応答のものだけを使う
	if c, err := strconv.ParseUint(resp.Header.Get(headerCommitted), 10, 64); err == nil {
		r.mu.Lock()
		r.primaryLast = c
		r.primarySync = resp.Header.Get(headerSync)
		r.mu.Unlock()
	}
	return len(recs), nil
}

// Status は、レプリカの状態（本番、適用した位置、本番の最後のコミットとの差、最後のエラー）を返す。
func (r *Replica) Status() []StatusItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	pos := r.st.LastCommitted()
	state := "running"
	if r.stopped {
		state = "stopped"
	}
	var contact, errText any
	if !r.lastContact.IsZero() {
		contact = time.Since(r.lastContact).Round(time.Second).String() + " ago"
	}
	if r.lastErr != "" {
		errText = r.lastErr
	}
	var lag, primaryLast, primarySync any
	if r.primaryLast != 0 {
		lag = behind(r.primaryLast, pos)
		primaryLast = formatTS(r.primaryLast)
	}
	if r.primarySync != "" {
		primarySync = r.primarySync
	}
	return []StatusItem{
		{"replication_source", r.source},
		{"replication_state", state},
		{"replication_position", formatTS(pos)},
		{"replication_behind", lag},
		// 最後の応答のときの本番の状態（本番のマシンが壊れたときの切り替えの判断に使う。段階 25）
		{"replication_primary_sync", primarySync},
		{"replication_primary_last_commit", primaryLast},
		{"replication_last_contact", contact},
		{"replication_last_error", errText},
	}
}

// behind は、本番の最後のコミット committed と、位置 position の時刻の差を返す。
func behind(committed, position uint64) string {
	if position >= committed {
		return "0s"
	}
	return (time.Duration(committed-position) * time.Microsecond).Round(time.Millisecond).String()
}

func formatTS(ts uint64) string {
	return time.UnixMicro(int64(ts)).UTC().Format("2006-01-02 15:04:05.000000-07:00")
}

func trimNL(b []byte) string {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return string(b)
}
