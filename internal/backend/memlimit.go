package backend

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// メモリの上限（docs/plan.md の段階 55）。
//
// コミットしていない書き込み（トランザクションのバッチ）と、エンジンの並べ替えや集計は、メモリに持つ。上限を超えて
// プロセスが落ちると同じ VM のほかのサイトも巻き込むので、上限に近づいたら文をエラー 1041 で止め、トランザクションを
// 取り消してメモリを手放す（プロセスは落ちない）。
//
// 調べるのは、行を読む、書く、コミットするところ（interrupted と同じ間隔）。メモリはプロセス全体で数えるので、止まる
// 文が多く使っている当の文とは限らない。

// memAbortRatio は、生きているヒープが上限のこの割合を超えたら文を止める割合。残りは GC が回るための余裕と、調べる
// 間に増える分。
const memAbortRatio = 0.75

// memForceGCInterval は、生きているヒープの値が古いかもしれないときに、確かめるために GC を回す間隔の下限。
const memForceGCInterval = 100 * time.Millisecond

var (
	memLimit   atomic.Int64 // 上限（バイト）。0 なら上限なし
	memGCMu    sync.Mutex
	memLastGC  time.Time
	memAborted atomic.Int64 // 止めた文の数（テストと状態の表示）
)

// SetMemoryLimit は、プロセスのメモリの上限を n バイトにする（0 なら上限なし）。Go のメモリの上限
// （debug.SetMemoryLimit）にも渡し、上限に近づいたら GC を早めに回させる。
func SetMemoryLimit(n int64) {
	if n < 0 {
		n = 0
	}
	memLimit.Store(n)
	if n > 0 {
		debug.SetMemoryLimit(n)
	} else {
		debug.SetMemoryLimit(math.MaxInt64) // 上限なし（Go の既定）
	}
}

// MemoryLimit は、今のメモリの上限（バイト。0 なら上限なし）を返す。
func MemoryLimit() int64 { return memLimit.Load() }

// MemoryAbortBytes は、文を止める境目（上限の memAbortRatio。上限がなければ 0）。
func MemoryAbortBytes() int64 { return int64(float64(memLimit.Load()) * memAbortRatio) }

// ブロックキャッシュと上限（docs/plan.md の段階 58）。cgo なしで建てると（本番のイメージ）、Pebble はブロックキャッシュを
// Go のヒープに取るので、キャッシュは生きているヒープに数えられる。キャッシュは読めば満ちて GC でも減らないので、
// 上限の境目が「起動直後の分 + キャッシュ」より小さいと、どの文も拒否され続ける。
const (
	// cacheShare は、-cache-size を指定しないときに、キャッシュを上限の何分の 1 にするか。
	cacheShare = 8
	// minAutoCacheSize は、上限から決めるキャッシュの大きさの下限。
	minAutoCacheSize = 16 << 20
)

// memStartupHeadroom は、起動時の確かめで、起動直後の分とキャッシュのほかに文のために残す分（LIKE の照合順序の表の
// ように、後から作るものを含む。テストで変える）。
var memStartupHeadroom int64 = 32 << 20

// AutoCacheSize は、-cache-size を指定しないときのブロックキャッシュの大きさ。上限の cacheShare 分の 1 を
// minAutoCacheSize と def の間に収める（上限がなければ def）。
func AutoCacheSize(limit, def int64) int64 {
	if limit <= 0 {
		return def
	}
	return max(minAutoCacheSize, min(limit/cacheShare, def))
}

// CheckStartupMemory は、データを開いた後に呼ぶ。GC を回した後の生きているヒープに、キャッシュが満ちた分と文の余裕を
// 足して、文を止める境目を超えるなら、要る上限を添えたエラーを返す（起動を拒否する）。autoCache なら、キャッシュの大きさは
// 上限から決めたもの（AutoCacheSize）で、要る上限もそれに合わせて求める。
func CheckStartupMemory(cacheSize int64, autoCache bool) error {
	limit := memLimit.Load()
	if limit <= 0 {
		return nil
	}
	runtime.GC()
	live := liveHeap()
	if live+cacheSize+memStartupHeadroom <= MemoryAbortBytes() {
		return nil
	}
	want := limit
	for c := cacheSize; float64(want)*memAbortRatio < float64(live+c+memStartupHeadroom); {
		want += 1 << 20
		if autoCache {
			c = AutoCacheSize(want, store.DefaultCacheSize)
		}
	}
	return fmt.Errorf("HeimDB: the memory limit (-memory-limit) of %d MiB is too small: the server uses %d MiB after opening the data and the block cache (-cache-size) grows to %d MiB, so statements would fail with error 1041 once the cache fills (they fail above %d MiB, %d%% of the limit); use a memory limit of at least %d MiB or a smaller -cache-size",
		limit>>20, live>>20, cacheSize>>20, MemoryAbortBytes()>>20, int(memAbortRatio*100), (want+1<<20-1)>>20)
}

// errOutOfMemoryFor は MySQL のエラー 1041（ER_OUT_OF_RESOURCES）。extra は、確保しようとして止めた大きさ（なければ 0）。
func errOutOfMemoryFor(live, extra, limit int64) error {
	if extra > 0 {
		return mysql.NewSQLError(1041, "HY000", "HeimDB: out of memory: the statement needs %d MiB more and the server uses %d MiB of its %d MiB memory limit (-memory-limit); the statement and its transaction were rolled back", extra>>20, live>>20, limit>>20)
	}
	return mysql.NewSQLError(1041, "HY000", "HeimDB: out of memory: the server uses %d MiB of its %d MiB memory limit (-memory-limit); the statement and its transaction were rolled back", live>>20, limit>>20)
}

// isOutOfMemory は、err がメモリの上限で止めた文のエラーかどうか（INSERT のエラーの包みも外して見る）。
func isOutOfMemory(err error) bool {
	var sqlErr *mysql.SQLError
	return errors.As(sql.UnwrapError(err), &sqlErr) && sqlErr.Num == 1041 && strings.HasPrefix(sqlErr.Message, "HeimDB: out of memory")
}

// liveHeap は、直前の GC で生きていたヒープの大きさを返す。
func liveHeap() int64 {
	s := [1]metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s[:])
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return int64(s[0].Value.Uint64())
}

// checkMemory は、生きているヒープが上限の memAbortRatio を超えていればエラー 1041 を返す。値は直前の GC の時点の
// ものなので、超えていたら GC を回して確かめ直す（ゴミが多いだけなら止めない）。
func checkMemory() error {
	return checkMemoryGC(true, 0)
}

// checkCommitMemory は、コミットの途中の確認（Store.CommitCheck）。コミットは書き込みの鍵（Store.writeMu）を持って
// いるので、GC を回して全ブランチのコミットを待たせず、直前の GC の値だけで判断する。
func checkCommitMemory() error {
	return checkMemoryGC(false, 0)
}

// checkAlloc は、大きなバッチを確保する前の確認（Store.AllocCheck）。確保する n バイトを足しても境目を超えないか
// 確かめる。書き込みの鍵を持たずに呼ばれるので、超えていれば GC を回して確かめ直す（docs/plan.md の段階 59）。
func checkAlloc(n int) error {
	return checkMemoryGC(true, int64(n))
}

// checkMemoryGC は、生きているヒープに extra バイトを足した値が境目を超えていればエラー 1041 を返す。
func checkMemoryGC(forceGC bool, extra int64) error {
	limit := memLimit.Load()
	if limit <= 0 {
		return nil
	}
	threshold := int64(float64(limit) * memAbortRatio)
	live := liveHeap()
	if live+extra <= threshold {
		return nil
	}
	if !forceGC {
		memAborted.Add(1)
		return errOutOfMemoryFor(live, extra, limit)
	}
	memGCMu.Lock()
	if time.Since(memLastGC) >= memForceGCInterval {
		runtime.GC()
		memLastGC = time.Now()
		live = liveHeap()
	}
	memGCMu.Unlock()
	if live+extra <= threshold {
		return nil
	}
	memAborted.Add(1)
	return errOutOfMemoryFor(live, extra, limit)
}

// memoryReleased は、止めた文のトランザクションを取り消した後に呼ぶ。止めたときの GC はまだトランザクションが生きている
// 間に回ったので、直後に来た文がその古い値で止まらないよう、次の checkMemory で GC を回させる。
func memoryReleased() {
	memGCMu.Lock()
	memLastGC = time.Time{}
	memGCMu.Unlock()
}

// cgroupShare は、上限を指定しないときに使う、コンテナのメモリの上限（cgroup）の割合。プロセスの使うメモリ（RSS）は、
// ヒープの外の分（実行ファイル、スタック、Go の管理の領域）と、バッチが一度に大きく伸びる分だけ、上限を 1〜2 割
// 超えることがある（手元の 80 万行の UPDATE で、上限 1 GiB に対して最大 1,174 MiB）。超えるとコンテナごと止まるので、
// 余裕をとる。
const cgroupShare = 0.8

// ParseMemoryLimit は、-memory-limit の値（"4G"、"512M"、"1073741824" など。K、M、G、T は 1024 の累乗。"0" は
// 上限なし）をバイトにする。空か "auto" なら、コンテナのメモリの上限（cgroup）の cgroupShare（上限がなければ 0）。
func ParseMemoryLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "auto") {
		return int64(float64(cgroupMemoryLimit()) * cgroupShare), nil
	}
	orig := s
	mult := int64(1)
	switch strings.ToUpper(s[len(s)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("HeimDB: invalid memory limit %q (use bytes or a number with K, M, G or T)", orig)
	}
	return n * mult, nil
}

// cgroupFiles は、コンテナのメモリの上限を読むファイル（cgroup v2、v1 の順）。テストで差し替える。
var cgroupFiles = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}

// cgroupMemoryLimit は、cgroup のメモリの上限を返す（上限がなければ 0）。
func cgroupMemoryLimit() int64 {
	for _, f := range cgroupFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "max" {
			return 0
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 { // v1 は上限がないとき非常に大きな値になる
			return 0
		}
		return n
	}
	return 0
}
