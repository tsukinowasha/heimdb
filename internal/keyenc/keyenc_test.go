package keyenc

import (
	"bytes"
	"cmp"
	"math"
	"math/rand/v2"
	"testing"
	"testing/quick"
)

func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	}
	return 0
}

func TestUint64Order(t *testing.T) {
	f := func(a, b uint64) bool {
		return sign(bytes.Compare(AppendUint64(nil, a), AppendUint64(nil, b))) == cmp.Compare(a, b)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

func TestInt64Order(t *testing.T) {
	f := func(a, b int64) bool {
		return sign(bytes.Compare(AppendInt64(nil, a), AppendInt64(nil, b))) == cmp.Compare(a, b)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
	edges := []int64{math.MinInt64, -1, 0, 1, math.MaxInt64}
	for i := 0; i+1 < len(edges); i++ {
		if bytes.Compare(AppendInt64(nil, edges[i]), AppendInt64(nil, edges[i+1])) >= 0 {
			t.Errorf("%d should sort before %d", edges[i], edges[i+1])
		}
	}
}

func TestInvertedTimeOrder(t *testing.T) {
	f := func(a, b uint64) bool {
		return sign(bytes.Compare(AppendInvertedTime(nil, a), AppendInvertedTime(nil, b))) == -cmp.Compare(a, b)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

// 0x00 と 0x01、0xFF を多く含むバイト列を作る。エスケープと終端の境界を突くため。
func edgyBytes(r *rand.Rand) []byte {
	alphabet := []byte{0x00, 0x01, 0x02, 0xFE, 0xFF, 'a', 'b'}
	n := r.IntN(6)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return b
}

func TestBytesOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 100000 {
		a, b := edgyBytes(r), edgyBytes(r)
		got := sign(bytes.Compare(AppendBytes(nil, a), AppendBytes(nil, b)))
		if want := bytes.Compare(a, b); got != want {
			t.Fatalf("order mismatch for %x vs %x: got %d want %d", a, b, got, want)
		}
	}
	f := func(a, b []byte) bool {
		return sign(bytes.Compare(AppendBytes(nil, a), AppendBytes(nil, b))) == bytes.Compare(a, b)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
}

// 列の値: nil は NULL を表す。
type col = []byte

func appendCol(b []byte, v col) []byte {
	if v == nil {
		return AppendNull(b)
	}
	return AppendBytes(AppendNotNull(b), v)
}

// MySQL の昇順: NULL が先頭、その後は値の順。
func compareCol(a, b col) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return bytes.Compare(a, b)
}

func randCol(r *rand.Rand) col {
	if r.IntN(5) == 0 {
		return nil
	}
	b := edgyBytes(r)
	if b == nil {
		b = []byte{}
	}
	return b
}

// 複数列のキー（文字列、文字列、整数）が辞書式の順序になることを確かめる。
func TestCompositeKeyOrder(t *testing.T) {
	type key struct {
		a, b col
		n    int64
	}
	encode := func(k key) []byte {
		buf := appendCol(nil, k.a)
		buf = appendCol(buf, k.b)
		return AppendInt64(AppendNotNull(buf), k.n)
	}
	compare := func(x, y key) int {
		if c := compareCol(x.a, y.a); c != 0 {
			return c
		}
		if c := compareCol(x.b, y.b); c != 0 {
			return c
		}
		return cmp.Compare(x.n, y.n)
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 100000 {
		x := key{randCol(r), randCol(r), r.Int64N(5) - 2}
		y := key{randCol(r), randCol(r), r.Int64N(5) - 2}
		if got, want := sign(bytes.Compare(encode(x), encode(y))), compare(x, y); got != want {
			t.Fatalf("order mismatch for %+v vs %+v: got %d want %d", x, y, got, want)
		}
	}
}

func TestNullSortsBeforeEmptyString(t *testing.T) {
	if bytes.Compare(appendCol(nil, nil), appendCol(nil, []byte{})) >= 0 {
		t.Fatal("NULL must sort before empty string")
	}
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for range 10000 {
		s := edgyBytes(r)
		u := r.Uint64()
		i := int64(r.Uint64())
		buf := AppendBytes(nil, s)
		buf = AppendUint64(buf, u)
		buf = AppendInt64(buf, i)
		buf = AppendInvertedTime(buf, u)
		buf = AppendNull(buf)

		gs, rest, err := DecodeBytes(buf)
		if err != nil || !bytes.Equal(gs, s) {
			t.Fatalf("bytes: got %x, %v want %x", gs, err, s)
		}
		gu, rest, err := DecodeUint64(rest)
		if err != nil || gu != u {
			t.Fatalf("uint64: got %d, %v want %d", gu, err, u)
		}
		gi, rest, err := DecodeInt64(rest)
		if err != nil || gi != i {
			t.Fatalf("int64: got %d, %v want %d", gi, err, i)
		}
		gt, rest, err := DecodeInvertedTime(rest)
		if err != nil || gt != u {
			t.Fatalf("time: got %d, %v want %d", gt, err, u)
		}
		isNull, rest, err := DecodeNullTag(rest)
		if err != nil || !isNull || len(rest) != 0 {
			t.Fatalf("null: got %v, %v, rest %x", isNull, err, rest)
		}
	}
}

func TestDecodeCorrupt(t *testing.T) {
	for _, b := range [][]byte{{}, {'a'}, {0x00}, {0x00, 0x02}, {'a', 0x00}} {
		if _, _, err := DecodeBytes(b); err == nil {
			t.Errorf("DecodeBytes(%x) should fail", b)
		}
	}
	if _, _, err := DecodeUint64([]byte{1, 2, 3}); err == nil {
		t.Error("DecodeUint64 on short input should fail")
	}
	if _, _, err := DecodeNullTag([]byte{0x05}); err == nil {
		t.Error("DecodeNullTag on unknown tag should fail")
	}
}

func TestPrefixEnd(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{0x01, 0x02}, []byte{0x01, 0x03}},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
		{[]byte{0xFF, 0xFF}, nil},
	}
	for _, c := range cases {
		if got := PrefixEnd(c.in); !bytes.Equal(got, c.want) {
			t.Errorf("PrefixEnd(%x) = %x, want %x", c.in, got, c.want)
		}
	}
}
