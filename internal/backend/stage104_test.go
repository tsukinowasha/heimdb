package backend

import (
	"math"
	"math/rand"
	"slices"
	"testing"
)

// prefixSample.rows は、同じハッシュの続きの終わりを二分探索で探す（docs/plan.md の段階 104）。重複の多いランダムな
// 並びで、1 件ずつ数える前の数え方と同じ件数を返す（並びの先頭と末尾、ない値、1 件だけの並び、空の並び、すべて同じ
// 値の並び、0 と math.MaxUint64）。
func TestStage104PrefixSampleRows(t *testing.T) {
	// 段階 103 までの数え方
	oldRows := func(hs []uint64, h uint64) (float64, bool) {
		lo, _ := slices.BinarySearch(hs, h)
		hi := lo
		for hi < len(hs) && hs[hi] == h {
			hi++
		}
		if hi == lo {
			return 0, false
		}
		return float64(hi - lo), true
	}
	check := func(hs []uint64, h uint64) {
		t.Helper()
		p := &prefixSample{scale: 1, hashes: map[string][][]uint64{"i": {hs}}}
		got, gotOK := p.rows("I", 1, h)
		want, wantOK := oldRows(hs, h)
		if got != want || gotOK != wantOK {
			t.Fatalf("rows(%v, %d) = %v, %v; want %v, %v", hs, h, got, gotOK, want, wantOK)
		}
	}
	edges := [][]uint64{
		nil,
		{},
		{5},
		{7, 7, 7, 7},
		{0, 0, 1, math.MaxUint64},
		{0, math.MaxUint64, math.MaxUint64, math.MaxUint64},
	}
	for _, hs := range edges {
		for _, h := range []uint64{0, 1, 5, 6, 7, 8, math.MaxUint64 - 1, math.MaxUint64} {
			check(hs, h)
		}
	}
	r := rand.New(rand.NewSource(104))
	for n := 0; n < 2000; n++ {
		// 値の種類を少なくして、長い続きを作る
		kinds := 1 + r.Intn(8)
		vals := make([]uint64, kinds)
		for i := range vals {
			switch r.Intn(4) {
			case 0:
				vals[i] = 0
			case 1:
				vals[i] = math.MaxUint64
			default:
				vals[i] = r.Uint64()
			}
		}
		hs := make([]uint64, r.Intn(300))
		for i := range hs {
			hs[i] = vals[r.Intn(kinds)]
		}
		slices.Sort(hs)
		for _, h := range vals {
			check(hs, h)
			check(hs, h+1)
			check(hs, h-1)
		}
	}
}
