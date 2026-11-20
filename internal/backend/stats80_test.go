package backend

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// 統計の標本は、行を読む順によらず同じになり、ビットの数は「残る行が上限以下」になる最小の値（docs/plan.md の段階 80）。
func TestNarrowSampleOrderIndependent(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	hashes := make([]uint64, 5000)
	for i := range hashes {
		hashes[i] = r.Uint64()
	}
	const limit = 700 // 残る行が上限以下になるビットの数は 3（奇数）
	run := func(order []uint64) ([]uint64, uint) {
		var sample []uint64
		var bits uint
		for _, h := range order {
			if h&(uint64(1)<<bits-1) != 0 {
				continue
			}
			sample = narrowSample(append(sample, h), func(h uint64) uint64 { return h }, &bits, limit)
		}
		slices.Sort(sample)
		return sample, bits
	}
	want, bits := run(hashes)
	count := func(bits uint) int {
		n := 0
		for _, h := range hashes {
			if h&(uint64(1)<<bits-1) == 0 {
				n++
			}
		}
		return n
	}
	if count(bits) > limit || count(bits-1) <= limit || len(want) != count(bits) {
		t.Errorf("bits %d keeps %d rows (one fewer bit keeps %d), sample %d", bits, count(bits), count(bits-1), len(want))
	}
	reversed := slices.Clone(hashes)
	slices.Reverse(reversed)
	shuffled := slices.Clone(hashes)
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for name, order := range map[string][]uint64{"reversed": reversed, "shuffled": shuffled} {
		if got, b := run(order); b != bits || !slices.Equal(got, want) {
			t.Errorf("%s: bits %d and %d rows, want bits %d and %d rows", name, b, len(got), bits, len(want))
		}
	}
}

// 値の種類の数は、間引いた標本から Chao1 で見積もる。間引かなければ数えた値、1 列の一意なインデックスは行数。
func TestTableDistinct(t *testing.T) {
	var keys [][]byte
	for i := range 100 { // 1 度だけ現れる値 100 種類
		keys = append(keys, fmt.Appendf(nil, "a%03d", i))
	}
	for i := range 10 { // 2 度現れる値 10 種類
		keys = append(keys, fmt.Appendf(nil, "b%03d", i), fmt.Appendf(nil, "b%03d", i))
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	for _, c := range []struct {
		unique bool
		total  uint64
		scale  float64
		want   uint64
	}{
		{false, 120, 1, 110},
		{false, 1200, 10, 110 + 100*99/(2*11)},
		{false, 300, 10, 300}, // 行数を超えない
		{true, 1200, 10, 1200},
	} {
		if got := tableDistinct(c.unique, keys, order, c.total, c.scale); got != c.want {
			t.Errorf("unique %v, total %d, scale %v: %d, want %d", c.unique, c.total, c.scale, got, c.want)
		}
	}
}

// 起動した後、文がなくても main の表の統計を裏で作る。停止を始めたらやめる。
func TestWarmStats(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE w (id bigint NOT NULL AUTO_INCREMENT, k int, PRIMARY KEY (id), KEY k (k))")
	s.mustQuery(t, "INSERT INTO w (k) WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) SELECT i % 7 FROM n")
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, _, err := db.GetTableInsensitive(s.ctx, "w")
	if err != nil {
		t.Fatal(err)
	}
	table := tbl.(*Table)
	key := rowCountKey{s.st, table.db.branch, table.def.ID}
	built := func() bool {
		rowCounts.mu.Lock()
		defer rowCounts.mu.Unlock()
		e, ok := rowCounts.entries[key]
		return ok && e.statsBuilt && e.count == 100
	}
	if built() {
		t.Fatal("statistics were built before warming")
	}
	pro := NewProvider(s.st, testDB)
	pro.WarmStats()
	for deadline := time.Now().Add(10 * time.Second); !built(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("statistics were not built")
		}
	}
	if n := pro.connections.Load(); n != 0 {
		t.Errorf("warming counts as %d connections", n)
	}
	if !pro.Shutdown(time.Second) {
		t.Error("shutdown waited for warming")
	}
}
