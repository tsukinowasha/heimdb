package rowenc

import (
	"bytes"
	"math"
	"testing"
	"testing/quick"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 段階 120: SET と BIT のインデックスのキー（キーの水準 2。docs/plan.md の「順序を保つ符号化」）。

func TestKeyableTypeLevel2(t *testing.T) {
	if KeyLevel != 2 {
		t.Fatalf("KeyLevel = %d; update this test", KeyLevel)
	}
	set := types.MustCreateSetType([]string{"a", "b", "c"}, sql.Collation_utf8mb4_0900_ai_ci)
	for _, typ := range []sql.Type{set, types.MustCreateBitType(1), types.MustCreateBitType(8), types.MustCreateBitType(64)} {
		if KeyableType(typ, 0) || KeyableType(typ, 1) {
			t.Errorf("%s should not be keyable before level 2", typ)
		}
		if !KeyableType(typ, 2) {
			t.Errorf("%s should be keyable at level 2", typ)
		}
	}
	if KeyableType(types.JSON, 2) {
		t.Error("JSON should not be keyable")
	}
}

// SET の番号の組と BIT の値（uint64）は、符号なしの数の順に並ぶ（MySQL の SET と BIT のインデックスの順）。2^63 以上も。
func TestIndexOrderSetBit(t *testing.T) {
	set := types.MustCreateSetType([]string{"a", "b", "c"}, sql.Collation_utf8mb4_0900_ai_ci)
	bit64 := types.MustCreateBitType(64)
	vals := []uint64{0, 1, 2, 3, 4, 5, 6, 7, 255, 256, math.MaxInt64 - 1, math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64 - 1, math.MaxUint64}
	for _, typ := range []sql.Type{set, bit64} {
		col := IndexColumn{Type: typ}
		null, _, err := AppendIndexValue(nil, col, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range vals {
			ka, _, err := AppendIndexValue(nil, col, a)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Compare(null, ka) >= 0 {
				t.Errorf("%s: NULL is not before %d", typ, a)
			}
			for _, b := range vals {
				kb, _, _ := AppendIndexValue(nil, col, b)
				want := 0
				if a < b {
					want = -1
				} else if a > b {
					want = 1
				}
				if got := sign(bytes.Compare(ka, kb)); got != want {
					t.Errorf("%s: order of %d vs %d: key %d", typ, a, b, got)
				}
			}
		}
	}
}

func TestIndexOrderSetBitProperty(t *testing.T) {
	for _, typ := range []sql.Type{types.MustCreateBitType(64), types.MustCreateSetType([]string{"a", "b"}, sql.Collation_utf8mb4_0900_ai_ci)} {
		col := IndexColumn{Type: typ}
		f := func(a, b uint64) bool {
			ka, _, err1 := AppendIndexValue(nil, col, a)
			kb, _, err2 := AppendIndexValue(nil, col, b)
			if err1 != nil || err2 != nil {
				return false
			}
			want := 0
			if a < b {
				want = -1
			} else if a > b {
				want = 1
			}
			return sign(bytes.Compare(ka, kb)) == want
		}
		if err := quick.Check(f, &quick.Config{MaxCount: 20000}); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

// SkipIndexValue は SET と BIT の値（9 バイト）と NULL を読み飛ばし、後ろの主キーを返す。
func TestSkipIndexValueSetBit(t *testing.T) {
	for _, typ := range []sql.Type{types.MustCreateBitType(8), types.MustCreateSetType([]string{"a", "b"}, sql.Collation_utf8mb4_0900_ai_ci)} {
		col := IndexColumn{Type: typ}
		for _, v := range []any{nil, uint64(0), uint64(3), uint64(math.MaxUint64)} {
			k, _, err := AppendIndexValue(nil, col, v)
			if err != nil {
				t.Fatal(err)
			}
			rest, err := SkipIndexValue(col, append(k, 0xAB, 0xCD))
			if err != nil || !bytes.Equal(rest, []byte{0xAB, 0xCD}) {
				t.Errorf("%s %v: %x %v", typ, v, rest, err)
			}
		}
		if _, err := SkipIndexValue(col, []byte{0x01, 0x00}); err == nil {
			t.Errorf("%s: a short value was skipped", typ)
		}
	}
}
