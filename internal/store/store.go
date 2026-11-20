// Package store は Pebble の上に、バージョン付きのテーブル定義と ID の採番、コミット時刻の発行を実装する。
package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
)

// Latest は「最新のバージョン」を読むときに渡す時刻。
const Latest uint64 = math.MaxUint64

// Store は HeimDB のデータを保持する Pebble データベース。
type Store struct {
	db        *pebble.DB
	cacheSize int64 // Pebble のブロックキャッシュの大きさ（Status に出す）
	// CommitCheck は、コミットで書き込みを並べる間に、ときどき呼ぶ（nil なら呼ばない）。エラーを返すとコミットをやめて
	// トランザクションを取り消す（backend のメモリの上限。docs/plan.md の段階 55）。開いた直後に一度だけ設定する。
	CommitCheck func() error
	// ReadCheck は、変わった行の一覧のように、読んだものを集める長い繰り返しで、ときどき呼ぶ（nil なら呼ばない）。エラーを
	// 返すと読むのをやめてそのエラーを返す（backend のメモリの上限。docs/plan.md の段階 58）。開いた直後に一度だけ設定する。
	ReadCheck func() error
	// AllocCheck は、大きなバッチを確保する前に、確保するバイト数 n を渡して呼ぶ（nil なら呼ばない）。エラーを返すと
	// 確保せずにそのエラーを返す（backend のメモリの上限。docs/plan.md の段階 59。一度に確保するので、確保した後の
	// 確かめでは間に合わず、プロセスが落ちた）。開いた直後に一度だけ設定する。
	AllocCheck func(n int) error
	// writeMu は書き込みを直列にする。時刻と ID の発行を単調にするため。
	writeMu sync.Mutex
	// stopWait は、停止が始まったら閉じる（StopLockWaits）。
	stopWait     chan struct{}
	stopWaitOnce sync.Once
	lastTS       uint64
	now          func() time.Time
	// committed は最後にコミットした時刻。トランザクションのスナップショットに使う。written は Pebble に書いた最後の
	// コミットの時刻で、同期のレプリカの知らせを待つ間だけ committed より先にある（段階 25）。
	committed atomic.Uint64
	written   atomic.Uint64
	// commitWait は、コミットを書いた後、見せる前に呼ぶ（同期のレプリカの知らせを待つ。SetCommitWait）。
	commitWait func(ts uint64)

	locksMu sync.Mutex
	locks   map[uint64]chan struct{}
	// holders と waiters は、ブランチの書き込みロックの持ち主と、待っている数（locksMu で守る。lock.go）。
	holders map[uint64]LockHolder
	waiters map[uint64]int

	// branches は m/branch の内容（ブランチ ID から情報）。作成と削除のときに更新する。
	branchesMu sync.RWMutex
	branches   map[uint64]BranchInfo
	// branchOpMu は、ブランチの作成と削除を直列にする（子のあるブランチを消さないため）。
	branchOpMu sync.Mutex
	// LockTimeout は、書き込みロックを待つ時間の上限（MySQL の innodb_lock_wait_timeout にあたる）。
	// go-mysql-server の innodb_lock_wait_timeout は 1 秒に固定されていて使えないので、サーバの設定で決める。
	LockTimeout time.Duration

	aiMu     sync.Mutex
	autoIncs map[autoIncKeyT]*AutoIncrement

	// snapMu は、開いているトランザクションのスナップショット（snapshots）と、GC の境界（gcBoundary）を守る。
	// スナップショットを決めることと GC が境界を決めることを排他にし、境界より古いスナップショットを作らない。
	snapMu     sync.Mutex
	snapshots  map[*Txn]uint64
	pins       map[*int]uint64
	gcBoundary uint64
	// lastGC は最後に動いた GC の記録（status.go）。
	lastGCMu sync.Mutex
	// usage は、ブランチの大きさの見積もりを少しの間覚えておく（limit.go）。
	usageMu sync.Mutex
	usage   map[uint64]usageEntry
	lastGC  *GCRun
	// gcMu は GC を 1 つずつ動かす。closed は Close の後（GC を動かさない）。closing は Close で閉じる。
	gcMu    sync.Mutex
	closed  bool
	closing chan struct{}
	// openTxns は、始めてまだ終わっていないトランザクション（docs/plan.md の段階 41）。Close はこれをロールバック
	// してから Pebble を閉じる（トランザクションが持つイテレータが残っていると、Pebble の Close が panic する）。
	openMu   sync.Mutex
	openTxns map[*Txn]struct{}

	// コミットの記録とレプリカ（replication.go）。replica は記録を適用するだけのレプリカとして開いたか。
	// logRetention は記録を残す期間（0 なら記録しない）。notify はコミットのたびに閉じて作り直す（WaitCommit）。
	replica      bool
	logRetention time.Duration
	notifyMu     sync.Mutex
	notify       chan struct{}
	applyMu      sync.Mutex
	onApply      []func(Applied)
}

// BranchInfo は m/branch/<ブランチID> に保存するブランチの情報。
type BranchInfo struct {
	ID       uint64 `json:"id"`
	Name     string `json:"name"`
	Parent   uint64 `json:"parent,omitempty"`
	ForkedAt uint64 `json:"forked_at,omitempty"`
	// MaxBytes と MaxExecutionMS は、ブランチの大きさと 1 つの文の実行時間の上限（0 なら上限なし。limit.go）。
	MaxBytes       uint64 `json:"max_bytes,omitempty"`
	MaxExecutionMS uint64 `json:"max_execution_ms,omitempty"`
}

// DefaultCacheSize は、Pebble のブロックキャッシュの既定の大きさ（MySQL の innodb_buffer_pool_size の既定と同じ）。
// Pebble の既定（8 MB）では、記事 5,000 件ほどのデータでも行を読むたびにファイルを読み直していた（docs/bench.md）。
const DefaultCacheSize = 128 << 20

// Options は Open の設定。
type Options struct {
	// CacheSize は Pebble のブロックキャッシュの大きさ（バイト）。0 なら DefaultCacheSize。
	CacheSize int64
	// LogRetention はコミットの記録を残す期間。0 なら DefaultLogRetention。NoLog なら記録しない。
	LogRetention time.Duration
	NoLog        bool
	// Replica なら、本番の記録を適用するだけのレプリカとして開く（書き込みはすべて ErrReadOnlyReplica）。
	// データはバックアップから始めること（空のデータは開かない）。
	Replica bool
}

// Open は dir の Pebble データベースを既定の設定で開く。なければ作り、main ブランチを用意する。
func Open(dir string) (*Store, error) {
	return OpenWith(dir, Options{})
}

// OpenWith は dir の Pebble データベースを opts の設定で開く。
func OpenWith(dir string, opts Options) (*Store, error) {
	if opts.CacheSize == 0 {
		opts.CacheSize = DefaultCacheSize
	}
	// レプリカは、ないディレクトリを作らない（バックアップの場所の書き間違いで空のデータを作らないように。Pebble の
	// ErrorIfNotExists は、エラーにする前にディレクトリを作る）。
	if opts.Replica {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("HeimDB: a replica must start from a backup of the primary: %w", err)
		}
	}
	db, err := pebble.Open(dir, &pebble.Options{Logger: pebbleLogger{}, CacheSize: opts.CacheSize, ErrorIfNotExists: opts.Replica})
	if err != nil {
		return nil, err
	}
	if err := checkFormat(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, now: time.Now, autoIncs: map[autoIncKeyT]*AutoIncrement{}, locks: map[uint64]chan struct{}{}, holders: map[uint64]LockHolder{}, waiters: map[uint64]int{}, LockTimeout: DefaultLockTimeout, snapshots: map[*Txn]uint64{}, pins: map[*int]uint64{}, stopWait: make(chan struct{}), closing: make(chan struct{}), openTxns: map[*Txn]struct{}{},
		replica: opts.Replica, logRetention: opts.LogRetention, notify: make(chan struct{}), cacheSize: opts.CacheSize}
	if s.logRetention == 0 {
		s.logRetention = DefaultLogRetention
	}
	if opts.NoLog {
		s.logRetention = 0
	}
	if s.lastTS, err = s.getUint64(clockKey()); err != nil {
		db.Close()
		return nil, err
	}
	if s.gcBoundary, err = s.getUint64(gcKey()); err != nil {
		db.Close()
		return nil, err
	}
	s.committed.Store(s.lastTS)
	s.written.Store(s.lastTS)
	if s.replica && s.lastTS == 0 {
		db.Close()
		return nil, errors.New("HeimDB: a replica must start from a backup of the primary (the data directory is empty)")
	}
	if !s.replica {
		if err := s.initMainBranch(); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.loadBranches(); err != nil {
		db.Close()
		return nil, err
	}
	// レプリカは書かない（一時テーブルのデータは、本番が片付けたときに記録で消える）。
	if !s.replica {
		if err := s.dropUnregisteredBranches(); err != nil {
			db.Close()
			return nil, err
		}
		if err := s.cleanupRewrites(); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// FormatVersion は、このバージョンの HeimDB が書くデータ形式のバージョン（docs/plan.md の段階 18 と「データ形式のバージョン」）。
// キーや値の符号化を変えたら上げ、古いバージョンを読む移行の処理を足す。
//   - 1: 0.1.0 から 0.1.5。
//   - 2: 1 列の文字列の主キー（段階 38）。バージョン 1 のデータはそのまま読める（移す処理はない）。0.1.5 以前は、文字列の
//     主キーの表のインデックスのエントリで主キーを 9 バイトと読み違えるので、バージョンを上げて開かせない。
//   - 3: 1 MiB を超えるコミットの記録を分けて書く（段階 59）。バージョン 2 の記録は分けていない記録として読める（移す処理は
//     ない）。古い HeimDB は分けた記録を別々の記録と読み違えるので、開かせない。
//   - 4: バイト列、日付を含む主キーと、文字列を含む複数列の主キー（段階 70）。バージョン 3 のデータはそのまま読める（既存の
//     キーの形は変えていない）。0.2.18 以前に新しい形の主キーの表を開かせない。
//   - 5: 主キーのない表（段階 72）。バージョン 4 のデータはそのまま読める（既存のキーの形は変えていない）。0.2.19 以前に主キーの
//     ない表を開かせない。
//   - 6: 親の変更をブランチに取り込んだ記録 m/sync（段階 94）。バージョン 5 のデータはそのまま読める（既存のキーの形は変えて
//     いない）。0.2.32 以前は取り込みの記録を知らず、取り込んだ後のマージで写した行をブランチの変更と読み違えるので、
//     開かせない。
//   - 7: DECIMAL、FLOAT、DOUBLE、ENUM の列を含むインデックスのエントリと、インデックスの定義のキーの水準（段階 117）。
//     バージョン 6 のデータはそのまま読める（水準のない定義は水準 0 で、前と同じくそのインデックスはエントリを持たない）。
//     0.2.38 以前は水準を知らず、水準 1 のインデックスのエントリを保守しない（行を変えてもエントリが古いまま残る）ので、
//     開かせない。
//   - 8: FLOAT(M,D)、DOUBLE(M,D) と FLOAT と DOUBLE の UNSIGNED の型の文字列と、SET と BIT の列を含むインデックスの
//     キーの水準 2（段階 120）。バージョン 7 のデータはそのまま読める（前からある表の FLOAT と DOUBLE は桁のない型の文字列で、
//     水準 0 と 1 のインデックスは前と同じく SET と BIT の列を含めばエントリを持たない）。0.2.39 と 0.2.40 は float(7,2)
//     を黙って桁のない float と読んで書き込みで丸めず、水準 2 のインデックスのエントリを保守しないので、開かせない。
const FormatVersion uint64 = 8

// RecordFormat は、レプリケーションで運ぶ記録の中身（キーと値）の形のバージョン（docs/plan.md の「データ形式のバージョン」）。本番と
// レプリカで確かめる。データ形式のバージョンのうち、記録の置き方だけを変えたバージョン（3）と、既存のキーの形を変えずに新しい形の
// 主キーや主キーのない表、取り込みの記録、新しい型のインデックスのエントリと型の桁を足したバージョン（4、5、6、7、8）では上げない。行やインデックスのキーと値を変えるときは FormatVersion と一緒に上げる。
const RecordFormat uint64 = 2

// checkFormat は、データ形式のバージョンを確かめる。バージョンがなければ（新しいデータ、またはバージョンを記録する前のデータ）今のバージョンを書く。
// 今より新しいバージョンのデータは開かない（黙って読み違えないように）。
func checkFormat(db *pebble.DB) error {
	v, closer, err := db.Get(formatKey())
	if errors.Is(err, pebble.ErrNotFound) {
		return db.Set(formatKey(), binary.BigEndian.AppendUint64(nil, FormatVersion), pebble.Sync)
	}
	if err != nil {
		return err
	}
	defer closer.Close()
	if len(v) != 8 {
		return fmt.Errorf("HeimDB: corrupt data format version")
	}
	got := binary.BigEndian.Uint64(v)
	if got > FormatVersion {
		return fmt.Errorf("HeimDB: the data directory has data format %d, written by a newer heimdb; this heimdb reads format %d or older", got, FormatVersion)
	}
	if got < FormatVersion {
		// 古いバージョンのデータを今のバージョンにする（バージョン 1 から 2、2 から 3 は、移す処理がない）。書き換えないと、古い HeimDB に戻したときに、
		// 今のバージョンで書いたデータを開けてしまう。
		logrus.Infof("upgrading the data format from %d to %d", got, FormatVersion)
		return db.Set(formatKey(), binary.BigEndian.AppendUint64(nil, FormatVersion), pebble.Sync)
	}
	return nil
}

// dropUnregisteredBranches は、m/branch に登録されていないブランチのデータを消す。一時テーブルの隠しブランチは
// 切断時に消すが、クラッシュするとデータが残るので、起動時にここで片付ける。
func (s *Store) dropUnregisteredBranches() error {
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{tagBranch}, UpperBound: []byte{tagBranch + 1}})
	if err != nil {
		return err
	}
	var stale []uint64
	for valid := iter.First(); valid; {
		branch, _, err := keyenc.DecodeUint64(iter.Key()[1:])
		if err != nil {
			iter.Close()
			return fmt.Errorf("HeimDB: corrupt branch key %x: %w", iter.Key(), err)
		}
		if _, ok, err := s.get(branchMetaKey(branch)); err != nil {
			iter.Close()
			return err
		} else if !ok {
			stale = append(stale, branch)
		}
		next := keyenc.PrefixEnd(branchPrefix(branch))
		if next == nil {
			break
		}
		valid = iter.SeekGE(next)
	}
	if err := iter.Close(); err != nil {
		return err
	}
	for _, b := range stale {
		logrus.Infof("HeimDB: dropping data of unregistered branch %d (leftover temporary tables)", b)
		if err := s.DropBranchData(b); err != nil {
			return err
		}
	}
	return nil
}

// pebbleLogger は Pebble のログを logrus に流す。Pebble の info ログは多いので Debug レベルにする。
type pebbleLogger struct{}

func (pebbleLogger) Infof(format string, args ...interface{}) {
	logrus.Debugf("pebble: "+format, args...)
}
func (pebbleLogger) Errorf(format string, args ...interface{}) {
	logrus.Errorf("pebble: "+format, args...)
}
func (pebbleLogger) Fatalf(format string, args ...interface{}) {
	logrus.Fatalf("pebble: "+format, args...)
}

// Close は、残っているトランザクションをロールバックし、進んだ AUTO_INCREMENT のカウンタを保存してからデータベースを
// 閉じる。呼ぶ側は、ほかのゴルーチンがトランザクションを使っていないことを保証する（HeimDB では、止めるときの
// 手順が新しい文と切断の後片付けを拒否し、実行中の文を待ってから呼ぶ）。
func (s *Store) Close() error {
	// 動いている GC が終わるのを待ち、その後は動かさない（閉じた Pebble を触ると panic するため）。
	s.gcMu.Lock()
	if !s.closed {
		close(s.closing)
	}
	s.closed = true
	s.gcMu.Unlock()
	// 文と文の間で開いたままのトランザクションは、主キーで 1 行を引くイテレータ（PointReader）を持ちうる。閉じる前に
	// ロールバックして手放す（MySQL を止めたときと同じく、コミットしていない変更は残らない。docs/plan.md の段階 41）。
	s.openMu.Lock()
	open := make([]*Txn, 0, len(s.openTxns))
	for tx := range s.openTxns {
		open = append(open, tx)
	}
	s.openMu.Unlock()
	for _, tx := range open {
		_ = tx.Rollback()
	}
	if err := s.flushAutoIncrements(); err != nil {
		s.db.Close()
		return err
	}
	return s.db.Close()
}

func (s *Store) initMainBranch() error {
	_, ok, err := s.get(branchMetaKey(MainBranchID))
	if err != nil || ok {
		return err
	}
	return s.Update(func(tx *WriteTx) error {
		if err := tx.setJSON(branchMetaKey(MainBranchID), BranchInfo{ID: MainBranchID, Name: "main"}); err != nil {
			return err
		}
		tx.setUint64(seqKey(seqBranch), MainBranchID)
		return nil
	})
}

// WriteTx は 1 回のアトミックな書き込み。すべての書き込みは同じコミット時刻 TS を持つ。
type WriteTx struct {
	s     *Store
	batch *pebble.Batch
	// TS はこの書き込みのコミット時刻（マイクロ秒単位の UNIX 時刻）。
	TS   uint64
	seqs map[string]uint64
}

// testHookBeforeCommit は、コミットのバッチ b を書く直前に呼ぶ（テストでコミットの最中のメモリと、b が先に確保した
// 大きさ presized（0 なら確保していない）に収まったかを見る）。
var testHookBeforeCommit func(b *pebble.Batch, presized int)

// Update は fn の書き込みをアトミックにコミットする。fn がエラーを返したら何も書かない。
// コミットの記録を残すなら、バッチの中身を同じバッチで l/<コミット時刻> に書く（replication.go）。
func (s *Store) Update(fn func(tx *WriteTx) error) error {
	return s.update(0, fn)
}

// largeCommitFlush は、コミットの後にファイルへの書き出しを促す大きさ。Pebble は大きなバッチ（memtable の半分、既定で
// 32 MiB 以上）を中身ごと書き出し待ちの列に入れ、すぐに書き出す。ところが書き出した後、列は先頭を切り詰めるだけで
// （pebble v2.1.7 の flush1 の queue[n:]）、読み取りの状態も同じ配列を共有するので、書き出したバッチは配列が作り
// 直されるまで GC されない。そのため大きなコミットの後に、コミットのバッチ（記録を残すなら中身の 2 倍）を、次に
// 書き込みがたまって列が回るまで持ち続けた（docs/plan.md の段階 59）。小さなコミットで促すと小さなファイルが増えるので、
// 大きなものだけにする（これより小さく、書き出し待ちの列に入るバッチは、次に列が回るまで残る。多くて 64 MiB 程度）。
const largeCommitFlush = 64 << 20

// flushLargeCommit は、n バイトのバッチをコミットした後に呼び、大きければ書き出しを 2 回促す（1 回目でバッチを書き出し、
// 2 回目で列の配列を作り直させる。ほかの memtable が並んでいると 2 回では足りないことがあるが、そのときは書き込みが
// 続いているので、列はすぐに回る）。終わるのは待たない。
func (s *Store) flushLargeCommit(n int) {
	if n < largeCommitFlush {
		return
	}
	go func() {
		for i := 0; i < 2; i++ {
			// 閉じた Pebble を触ると panic するので、Close と同じ鍵の中で頼む
			s.gcMu.Lock()
			var done <-chan struct{}
			var err error
			if !s.closed {
				done, err = s.db.AsyncFlush()
			}
			s.gcMu.Unlock()
			if err != nil {
				logrus.Warnf("HeimDB: flushing after a large commit: %v", err)
			}
			if done == nil || err != nil {
				return
			}
			select {
			case <-done:
			case <-s.closing:
				return
			}
		}
	}()
}

// update は Update と同じ。size は fn が書く大きさの見込み（バイト。0 なら分からない）で、コミットのバッチをその大きさ
// （記録を残すなら、記録の分も足して）で確保する。
func (s *Store) update(size int, fn func(tx *WriteTx) error) error {
	return s.updateWaits(size, nil, fn)
}

// Waits は、トランザクションが待った時間（docs/plan.md の段階 81）。Lock は書き込みロック、Ack は同期のレプリカの知らせ。
type Waits struct {
	Lock, Ack time.Duration
}

// updateWaits は update と同じ。waits があれば、同期のレプリカの知らせを待った時間を足す。
func (s *Store) updateWaits(size int, waits *Waits, fn func(tx *WriteTx) error) error {
	if s.replica {
		return ErrReadOnlyReplica
	}
	presized := 0
	if size > 0 {
		// fn の後に足す、保存していない AUTO_INCREMENT のカウンタ（すべてのテーブルの分。1 件 64 バイト未満）の分も
		// 確保する。足りないとバッチは倍々に伸び、上限に近い大きさでは 2 倍を確保する。
		s.aiMu.Lock()
		size += len(s.autoIncs) * 64
		s.aiMu.Unlock()
		if s.Logging() {
			size = 2*size + (size/LogChunkBytes+1)*32 // 記録は分けて書くので、部分ごとのキーの分も
		}
		presized = size + 64<<10
		// 書き込みの鍵を取る前に確かめる（確かめで GC を回しても、ほかのコミットを待たせない）
		if check := s.AllocCheck; check != nil {
			if err := check(presized); err != nil {
				return err
			}
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ts := max(uint64(s.now().UnixMicro()), s.lastTS+1)
	var batch *pebble.Batch
	if presized > 0 {
		batch = s.db.NewBatchWithSize(presized)
	} else {
		batch = s.db.NewBatch()
	}
	tx := &WriteTx{s: s, batch: batch, TS: ts, seqs: map[string]uint64{}}
	defer tx.batch.Close()
	if err := fn(tx); err != nil {
		return err
	}
	saved := s.writeDirtyAutoIncrements(tx)
	tx.setUint64(clockKey(), ts)
	if s.Logging() {
		// 記録の値は、ここまでのバッチの中身（Repr。件数は記録の分を含まない）。写す先を確保してから写すので、Repr の
		// 別の写しは作らない（確保でバッチが伸びても、元の Repr は古い領域を指したまま読める）。
		if err := putLog(tx.batch, ts, tx.batch.Repr()); err != nil {
			return err
		}
	}
	if testHookBeforeCommit != nil {
		testHookBeforeCommit(tx.batch, presized)
	}
	n := tx.batch.Len()
	if err := tx.batch.Commit(pebble.Sync); err != nil {
		return err
	}
	s.flushLargeCommit(n)
	s.lastTS = ts
	s.written.Store(ts)
	markAutoIncrementsSaved(saved)
	s.notifyCommit() // 記録を送り出す
	if s.commitWait != nil {
		start := time.Now()
		s.commitWait(ts) // 書き込みのロックを持ったまま待つ（知らせを待つコミットは常に 1 つ）
		if waits != nil {
			waits.Ack += time.Since(start)
		}
	}
	s.committed.Store(ts)
	return nil
}

// NewHiddenBranch は、一時テーブルのための隠しブランチの ID を払い出す。ブランチの一覧には載せず、ID はユーザーの
// ブランチと別の範囲（hiddenBranchBase 以上）にする。
func (s *Store) NewHiddenBranch() (uint64, error) {
	var id uint64
	err := s.Update(func(tx *WriteTx) error {
		n, err := tx.NextID(seqHidden)
		id = hiddenBranchBase + n
		return err
	})
	return id, err
}

// DropBranchData は、ブランチのデータ（テーブル定義、行、インデックス、AUTO_INCREMENT）をすべて消す。
// 一時テーブルの隠しブランチを片付けるのに使う。バージョンを残さずに消すので、通常のブランチには使わない。
func (s *Store) DropBranchData(branch uint64) error {
	s.aiMu.Lock()
	for k := range s.autoIncs {
		if k.branch == branch {
			delete(s.autoIncs, k)
		}
	}
	s.aiMu.Unlock()
	s.locksMu.Lock()
	delete(s.locks, branch)
	s.locksMu.Unlock()
	prefix := branchPrefix(branch)
	return s.Update(func(tx *WriteTx) error {
		return tx.batch.DeleteRange(prefix, keyenc.PrefixEnd(prefix), nil)
	})
}

// LastCommitted は、最後にコミットした時刻を返す。
func (s *Store) LastCommitted() uint64 {
	return s.committed.Load()
}

// NextID は採番器 name から新しい ID を払い出す。
func (tx *WriteTx) NextID(name string) (uint64, error) {
	cur, ok := tx.seqs[name]
	if !ok {
		var err error
		if cur, err = tx.s.getUint64(seqKey(name)); err != nil {
			return 0, err
		}
	}
	cur++
	tx.seqs[name] = cur
	tx.setUint64(seqKey(name), cur)
	return cur, nil
}

// NextTableID は新しいテーブル ID を払い出す。
func (tx *WriteTx) NextTableID() (uint64, error) {
	return tx.NextID(seqTable)
}

// PutTable はテーブル定義の新しいバージョンを、t.Name の名前で書く。
func (tx *WriteTx) PutTable(branch uint64, t *schema.Table) error {
	b, err := t.Marshal()
	if err != nil {
		return err
	}
	return tx.batch.Set(catalogKey(branch, t.Name, tx.TS), append([]byte{valData}, b...), nil)
}

// PutIndexEntry はインデックスのエントリを書く（CREATE INDEX で既存の行からインデックスを作るときに使う）。
func (tx *WriteTx) PutIndexEntry(branch, tableID, indexID uint64, entry, value []byte) error {
	return tx.batch.Set(indexKey(branch, tableID, indexID, entry, tx.TS), append([]byte{valData}, value...), nil)
}

// PutRow は行のバージョンを書く（ALTER TABLE で行を新しいテーブル ID に書き直すときに使う）。
func (tx *WriteTx) PutRow(branch, tableID uint64, pk, value []byte) error {
	return tx.batch.Set(rowKey(branch, tableID, pk, tx.TS), append([]byte{valData}, value...), nil)
}

// SetAutoIncrement は、テーブルの AUTO_INCREMENT の次の値を保存する（ALTER TABLE で新しいテーブル ID に引き継ぐときに使う）。
func (tx *WriteTx) SetAutoIncrement(branch, tableID, next uint64) {
	tx.setUint64(autoIncKey(branch, tableID), next)
}

// DeleteTable はテーブル名 name に削除の印を書く。
func (tx *WriteTx) DeleteTable(branch uint64, name string) error {
	return tx.batch.Set(catalogKey(branch, name, tx.TS), []byte{valTombstone}, nil)
}

func (tx *WriteTx) setUint64(key []byte, v uint64) {
	// Batch.Set はメモリ上の追記で、失敗するのは Batch を閉じた後だけ。
	if err := tx.batch.Set(key, binary.BigEndian.AppendUint64(nil, v), nil); err != nil {
		panic(err)
	}
}

func (tx *WriteTx) setJSON(key []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.batch.Set(key, b, nil)
}

// GetTable は、ブランチ branch の時刻 asOf の時点でのテーブル name の定義を返す。なければ ok が false。
// ブランチでは、親の分岐時刻以前の定義を重ねて見る。
func (s *Store) GetTable(branch uint64, name string, asOf uint64) (t *schema.Table, ok bool, err error) {
	want := keyenc.AppendString(nil, strings.ToLower(name))
	err = s.scanCatalog(branch, asOf, want, keyenc.PrefixEnd(want), func(entry []byte, ts uint64, value []byte) (bool, error) {
		if bytes.Equal(entry, want) {
			t, ok, err = decodeTableValue(value, ts)
		}
		return false, err
	})
	return t, ok, err
}

// ListTables は、ブランチ branch で時刻 asOf の時点に存在するテーブルの定義を名前順に返す。
func (s *Store) ListTables(branch uint64, asOf uint64) ([]*schema.Table, error) {
	var tables []*schema.Table
	err := s.scanCatalog(branch, asOf, nil, nil, func(_ []byte, ts uint64, value []byte) (bool, error) {
		t, ok, err := decodeTableValue(value, ts)
		if err != nil {
			return false, err
		}
		if ok {
			tables = append(tables, t)
		}
		return true, nil
	})
	return tables, err
}

// scanCatalog は、ブランチのテーブル定義を層を重ねて読み、名前ごとに時刻 asOf 以前の最新のバージョンを fn に渡す。
// 削除の印のバージョンも渡す（value の先頭の印はそのまま）。バージョンの時刻は値から分からないので、各バージョンのキーから取り出して渡す。
func (s *Store) scanCatalog(branch, asOf uint64, lo, hi []byte, fn func(entry []byte, ts uint64, value []byte) (bool, error)) error {
	ls, err := s.layers(branch)
	if err != nil {
		return err
	}
	var cursors []*versionCursor
	for _, l := range ls {
		maxTS := min(l.maxTS, asOf)
		c, err := newVersionCursor(s.db.NewIter, catalogPrefix(l.branch), func(ts uint64) bool { return ts <= maxTS }, lo, hi, false)
		if err != nil {
			for _, c := range cursors {
				c.close()
			}
			return err
		}
		cursors = append(cursors, c)
	}
	defer func() {
		for _, c := range cursors {
			c.close()
		}
	}()
	for {
		best := -1
		for i, c := range cursors {
			if c.err != nil {
				return c.err
			}
			if c.valid && (best < 0 || bytes.Compare(c.entry, cursors[best].entry) < 0) {
				best = i
			}
		}
		if best < 0 {
			return nil
		}
		entry := append([]byte(nil), cursors[best].entry...)
		value := append([]byte(nil), cursors[best].value...)
		ts := cursors[best].ts
		for _, c := range cursors {
			if c.valid && bytes.Equal(c.entry, entry) {
				c.advance()
			}
		}
		if cont, err := fn(entry, ts, value); err != nil || !cont {
			return err
		}
	}
}

func decodeTableValue(v []byte, ts uint64) (*schema.Table, bool, error) {
	if len(v) == 0 {
		return nil, false, errors.New("HeimDB: empty catalog value")
	}
	if v[0] == valTombstone {
		return nil, false, nil
	}
	tableDefs.mu.Lock()
	cached, ok := tableDefs.m[string(v)]
	tableDefs.mu.Unlock()
	if !ok {
		var err error
		if cached, err = schema.Unmarshal(v[1:]); err != nil {
			return nil, false, err
		}
		tableDefs.mu.Lock()
		if len(tableDefs.m) >= maxTableDefs {
			clear(tableDefs.m)
		}
		tableDefs.m[string(v)] = cached
		tableDefs.mu.Unlock()
	}
	t := cached.Clone()
	t.Version = ts
	return t, true, nil
}

// tableDefs は、保存したテーブルの定義のバイト列から、復号した定義へのキャッシュ。文ごとに表の定義を読み、JSON の
// 復号が主キーで 1 行を読む文の CPU の 15% ほどになっていた（docs/plan.md の段階 78）。定義はバイト列だけで決まるので、
// ストアやブランチ、時刻をまたいで共有してよい。呼び出し側は定義を書き換えることがあるので、写しを返す。
var tableDefs = struct {
	mu sync.Mutex
	m  map[string]*schema.Table
}{m: map[string]*schema.Table{}}

// maxTableDefs は tableDefs の数の上限。超えたら空にする（ALTER のたびにバージョンが増えるが、WordPress の表は数十）。
const maxTableDefs = 4096

func (s *Store) get(key []byte) ([]byte, bool, error) {
	v, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer closer.Close()
	return append([]byte(nil), v...), true, nil
}

func (s *Store) getUint64(key []byte) (uint64, error) {
	v, ok, err := s.get(key)
	if err != nil || !ok {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("HeimDB: corrupt uint64 value at %x", key)
	}
	return binary.BigEndian.Uint64(v), nil
}
