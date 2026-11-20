package rowenc

import (
	"bytes"
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"testing/quick"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 段階 117: DECIMAL、FLOAT、DOUBLE、ENUM のインデックスのキー（docs/plan.md の「順序を保つ符号化」）。

func TestKeyableTypeLevels(t *testing.T) {
	text := types.MustCreateString(sqltypes.VarChar, 20, sql.Collation_utf8mb4_unicode_520_ci)
	enum := types.MustCreateEnumType([]string{"a", "b"}, sql.Collation_utf8mb4_0900_ai_ci)
	set := types.MustCreateSetType([]string{"a", "b"}, sql.Collation_utf8mb4_0900_ai_ci)
	dec := types.MustCreateDecimalType(19, 4)
	for _, typ := range []sql.Type{types.Int64, types.Uint64, types.DatetimeMaxPrecision, types.Date, text, types.MustCreateBinary(sqltypes.VarBinary, 20)} {
		if !KeyableType(typ, 0) || !KeyableType(typ, KeyLevel) {
			t.Errorf("%s should be keyable at both levels", typ)
		}
	}
	for _, typ := range []sql.Type{dec, types.Float32, types.Float64, enum} {
		if KeyableType(typ, 0) {
			t.Errorf("%s should not be keyable at level 0", typ)
		}
		if !KeyableType(typ, 1) {
			t.Errorf("%s should be keyable at level 1", typ)
		}
	}
	// SET と BIT は水準 2 から（段階 120。TestKeyableTypeLevel2）
	for _, typ := range []sql.Type{set, types.MustCreateBitType(8)} {
		if KeyableType(typ, 1) {
			t.Errorf("%s should not be keyable at level 1", typ)
		}
	}
	if KeyableType(types.JSON, KeyLevel) {
		t.Error("JSON should not be keyable")
	}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestIndexOrderDecimal(t *testing.T) {
	typ := types.MustCreateDecimalType(65, 30)
	vals := []any{nil}
	for _, s := range []string{
		"0", "-0", "1", "-1", "1.5", "1.50", "1.5000001", "1.4999999", "-1.5", "-1.50", "-1.5000001", "-1.4999999",
		"10", "9.9999", "10.0001", "100", "-100", "-99.99", "0.001", "0.01", "0.1", "-0.001", "-0.01", "-0.1",
		"999999999999999.9999", "-999999999999999.9999", "12345678901234567890123456789012345", "0.000000000000000000000000000001",
		"-0.000000000000000000000000000001", "1000000", "999999",
	} {
		vals = append(vals, dec(s))
	}
	checkOrder(t, typ, vals)

	// 桁が違っても同じ値は同じキー（1.5 と 1.50、0 と -0）
	col := IndexColumn{Type: typ}
	for _, pair := range [][2]string{{"1.5", "1.50"}, {"0", "-0.000"}, {"-2.10", "-2.1"}, {"100", "100.000"}} {
		a, _, _ := AppendIndexValue(nil, col, dec(pair[0]))
		b, _, _ := AppendIndexValue(nil, col, dec(pair[1]))
		if !bytes.Equal(a, b) {
			t.Errorf("%s and %s: keys %x and %x", pair[0], pair[1], a, b)
		}
	}
	if _, _, err := AppendIndexValue(nil, col, "1.5"); err == nil {
		t.Error("a string for a decimal column was not an error")
	}
}

// 順序のプロパティテスト: 乱数の DECIMAL の組で、キーの順が値の順と一致し、一方が他方の前方にならない。
func TestIndexOrderDecimalProperty(t *testing.T) {
	ctx := context.Background()
	typ := types.MustCreateDecimalType(65, 30)
	col := IndexColumn{Type: typ}
	f := func(a, b int64, ea, eb uint8) bool {
		da := decimal.New(a, -int32(ea%12))
		db := decimal.New(b, -int32(eb%12))
		ka, _, err1 := AppendIndexValue(nil, col, da)
		kb, _, err2 := AppendIndexValue(nil, col, db)
		if err1 != nil || err2 != nil {
			return false
		}
		want, _ := typ.Compare(ctx, da, db)
		if sign(bytes.Compare(ka, kb)) != sign(want) {
			t.Logf("%s vs %s: %x %x", da, db, ka, kb)
			return false
		}
		if want != 0 && (bytes.HasPrefix(ka, kb) || bytes.HasPrefix(kb, ka)) {
			t.Logf("%s vs %s: prefix %x %x", da, db, ka, kb)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 20000}); err != nil {
		t.Error(err)
	}
	// 近い値（同じ係数で指数だけ違う、1 だけ違う）も
	r := rand.New(rand.NewPCG(117, 1))
	var vals []any
	for range 300 {
		c := r.Int64N(2001) - 1000
		vals = append(vals, decimal.New(c, -int32(r.IntN(5))))
	}
	checkOrder(t, typ, vals)
}

func TestIndexOrderFloat(t *testing.T) {
	f64 := []any{nil, -math.MaxFloat64, -1e300, -1.5, -1.0, -math.SmallestNonzeroFloat64, 0.0, math.Copysign(0, -1),
		math.SmallestNonzeroFloat64, 0.1, 0.3, 1.0, 1.5, 1e300, math.MaxFloat64}
	checkOrder(t, types.Float64, f64)
	f32 := []any{nil, float32(-math.MaxFloat32), float32(-1.5), float32(-0.1), float32(0), float32(math.Copysign(0, -1)),
		float32(0.1), float32(1.5), float32(1e38), float32(math.MaxFloat32)}
	checkOrder(t, types.Float32, f32)

	// -0 は 0 と同じキー
	col := IndexColumn{Type: types.Float64}
	a, _, _ := AppendIndexValue(nil, col, 0.0)
	b, _, _ := AppendIndexValue(nil, col, math.Copysign(0, -1))
	if !bytes.Equal(a, b) {
		t.Errorf("0 and -0: %x %x", a, b)
	}
	// FLOAT の値は倍精度に広げる（float32 の 0.1 と float64 の 0.1 は違うキー）
	c32, _, _ := AppendIndexValue(nil, IndexColumn{Type: types.Float32}, float32(0.1))
	c64, _, _ := AppendIndexValue(nil, IndexColumn{Type: types.Float32}, float64(float32(0.1)))
	if !bytes.Equal(c32, c64) {
		t.Errorf("float32 0.1 widened: %x %x", c32, c64)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := AppendIndexValue(nil, col, v); err == nil {
			t.Errorf("%v was not an error", v)
		}
	}
	if _, _, err := AppendIndexValue(nil, col, "1"); err == nil {
		t.Error("a string for a double column was not an error")
	}
}

func TestIndexOrderFloatProperty(t *testing.T) {
	ctx := context.Background()
	col := IndexColumn{Type: types.Float64}
	f := func(a, b float64) bool {
		ka, _, err1 := AppendIndexValue(nil, col, a)
		kb, _, err2 := AppendIndexValue(nil, col, b)
		if err1 != nil || err2 != nil {
			return false
		}
		want, _ := types.Float64.Compare(ctx, a, b)
		return sign(bytes.Compare(ka, kb)) == sign(want)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 20000}); err != nil {
		t.Error(err)
	}
	// ビット列から作った任意の値（非正規化数を含む。NaN と無限大は除く）
	g := func(x, y uint64) bool {
		a, b := math.Float64frombits(x), math.Float64frombits(y)
		if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
			return true
		}
		return f(a, b)
	}
	if err := quick.Check(g, &quick.Config{MaxCount: 20000}); err != nil {
		t.Error(err)
	}
}

func TestIndexOrderEnum(t *testing.T) {
	enum := types.MustCreateEnumType([]string{"z", "b", "a"}, sql.Collation_utf8mb4_0900_ai_ci)
	// エンジンは番号（uint16）を行に入れ、番号で比べる。0 は空の値（非 strict で入る）
	checkOrder(t, enum, []any{nil, uint16(0), uint16(1), uint16(2), uint16(3)})
}

// 新しい型を含むエントリの後ろの主キーを取り出せる（整数でない主キーの表。pkOfEntry）。
func TestSkipIndexValue117(t *testing.T) {
	enum := types.MustCreateEnumType([]string{"a", "b"}, sql.Collation_utf8mb4_0900_ai_ci)
	cols := []IndexColumn{{Type: types.MustCreateDecimalType(19, 4)}, {Type: types.Float64}, {Type: types.Float32}, {Type: enum}, {Type: types.MustCreateDecimalType(10, 2)}}
	vals := [][]any{
		{dec("12.3400"), 1.5, float32(-0.1), uint16(2), dec("-0.01")},
		{dec("0"), 0.0, float32(0), uint16(0), dec("-100")},
		{nil, nil, nil, nil, nil},
		{dec("-999999999999999.9999"), -1e300, float32(1e38), uint16(1), dec("0.5")},
	}
	text := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_0900_ai_ci)
	pk, err := AppendPKValue(nil, text, "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range vals {
		var entry []byte
		for i, c := range cols {
			if entry, _, err = AppendIndexValue(entry, c, row[i]); err != nil {
				t.Fatal(err)
			}
		}
		entry = append(entry, pk...)
		rest := entry
		for _, c := range cols {
			if rest, err = SkipIndexValue(c, rest); err != nil {
				t.Fatalf("%v: %v", row, err)
			}
		}
		if !bytes.Equal(rest, pk) {
			t.Errorf("%v: rest %x, want %x", row, rest, pk)
		}
	}
	for _, bad := range [][]byte{{0x01, 0x03}, {0x01, 0x03, 0x01, 0x02}, {0x01, 0x01, 0xFE, 0xFE}, {0x01, 0x04}, {0x01}} {
		if _, err := SkipIndexValue(cols[0], bad); err == nil {
			t.Errorf("corrupt decimal %x was not an error", bad)
		}
	}
	if _, err := SkipIndexValue(cols[1], []byte{0x01, 1, 2, 3}); err == nil {
		t.Error("truncated double was not an error")
	}
}

// 複数列のキー: (DECIMAL, 整数) の組でキーの順が列ごとの比較を前から並べた順と一致する（可変長の DECIMAL の
// 後ろに列が続いても崩れない）。
func TestIndexOrderDecimalComposite(t *testing.T) {
	ctx := context.Background()
	typ := types.MustCreateDecimalType(19, 4)
	cols := []IndexColumn{{Type: typ}, {Type: types.Int64}}
	r := rand.New(rand.NewPCG(117, 2))
	type pair struct {
		d decimal.Decimal
		i int64
	}
	var ps []pair
	for range 200 {
		ps = append(ps, pair{decimal.New(r.Int64N(401)-200, -int32(r.IntN(4))), r.Int64N(5) - 2})
	}
	key := func(p pair) []byte {
		b, _, _ := AppendIndexValue(nil, cols[0], p.d)
		b, _, _ = AppendIndexValue(b, cols[1], p.i)
		return b
	}
	for _, a := range ps {
		for _, b := range ps {
			want, _ := typ.Compare(ctx, a.d, b.d)
			if want == 0 {
				want = sign(int(a.i - b.i))
			}
			if got := sign(bytes.Compare(key(a), key(b))); got != sign(want) {
				t.Fatalf("(%s,%d) vs (%s,%d): key %d, want %d", a.d, a.i, b.d, b.i, got, want)
			}
		}
	}
}
