package backend

import (
	"testing"

	gms "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
)

// 連番の主キーで、間引き（ハッシュの下位の bits ビットが 0 の行。step = 2^bits）が行の並びの周期と重ならない:
// 組のどの位置の行もほぼ同じ割合（step 行に 1 行）で残り、隣り合う 2 行が両方残る割合も、行ごとに独立に選んだ場合
// （step² 組に 1 組）に近い（記事ごとに続けて挿入されるメタの組の片方だけが残らない）。
func TestSampledNotPeriodic(t *testing.T) {
	sch := gms.NewPrimaryKeySchema(gms.Schema{{Name: "meta_id", Type: types.Uint64, PrimaryKey: true}}, 0)
	const n = 30000
	near := func(got int, want float64) bool { return float64(got) >= want*0.8 && float64(got) <= want*1.2 }
	for _, step := range []uint64{2, 4, 8} {
		keep := make([]bool, n+2)
		for id := 1; id <= n+1; id++ {
			pk, err := rowenc.EncodePK(nil, sch, gms.Row{uint64(id)})
			if err != nil {
				t.Fatal(err)
			}
			keep[id] = pkHash(pk)&(step-1) == 0
		}
		for _, period := range []int{2, 3, 5} {
			kept := make([]int, period)
			for id := 1; id <= n; id++ {
				if keep[id] {
					kept[id%period]++
				}
			}
			for pos, k := range kept {
				if want := float64(n) / float64(period) / float64(step); !near(k, want) {
					t.Errorf("step %d, period %d: position %d kept %d rows, want about %.0f", step, period, pos, k, want)
				}
			}
		}
		both := 0
		for id := 1; id <= n; id++ {
			if keep[id] && keep[id+1] {
				both++
			}
		}
		if want := float64(n) / float64(step*step); !near(both, want) {
			t.Errorf("step %d: %d adjacent pairs both kept, want about %.0f", step, both, want)
		}
	}
}
