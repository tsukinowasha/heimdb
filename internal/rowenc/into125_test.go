package rowenc

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 段階 125: DecodeRowColumnsInto は、DecodeRowColumns と同じ行を、渡した行の上に作る。

// TestDecodeRowColumnsIntoProperty は、乱数の行と列の選び方で、DecodeRowColumnsInto の行が DecodeRowColumns と
// 同じこと（前の行の値が残らない）、容量の足りる dst の上に作ること、バイト列の値を前の行と共有しないことを
// 確かめる。
func TestDecodeRowColumnsIntoProperty(t *testing.T) {
	r := rand.New(rand.NewSource(125))
	value := func() any {
		switch r.Intn(9) {
		case 0:
			return nil
		case 1:
			return int64(r.Intn(1000) - 500)
		case 2:
			return uint32(r.Intn(1000))
		case 3:
			return fmt.Sprintf("s%d", r.Intn(100))
		case 4:
			return []byte{byte(r.Intn(256)), 1, 2}
		case 5:
			return decimal.New(int64(r.Intn(10000)), -2)
		case 6:
			return time.Date(2026, 1+time.Month(r.Intn(12)), 1+r.Intn(28), 0, 0, 0, 0, time.UTC)
		case 7:
			return types.JSONDocument{Val: map[string]any{"k": float64(r.Intn(9))}}
		}
		return r.Float64()
	}
	var dst sql.Row
	for i := range 2000 {
		n := 1 + r.Intn(12)
		row := make(sql.Row, n)
		for j := range row {
			row[j] = value()
		}
		b, err := EncodeRow(uint64(i), row)
		if err != nil {
			t.Fatal(err)
		}
		var want []bool
		if r.Intn(4) > 0 {
			want = make([]bool, n+r.Intn(2)-r.Intn(2)) // 列の数より短いことも長いこともある
			for j := range want {
				want[j] = r.Intn(2) == 0
			}
		}
		v1, exp, err := DecodeRowColumns(b, want)
		if err != nil {
			t.Fatal(err)
		}
		prev := append(sql.Row(nil), dst...)
		v2, got, err := DecodeRowColumnsInto(b, want, dst)
		if err != nil {
			t.Fatal(err)
		}
		if v1 != v2 || !reflect.DeepEqual(got, exp) {
			t.Fatalf("row %d: Into %v, want %v", i, got, exp)
		}
		if cap(dst) >= n && &got[0] != &dst[:1][0] {
			t.Fatalf("row %d: not built on dst (cap %d, %d columns)", i, cap(dst), n)
		}
		// 前の行のバイト列の値と、配列を共有しない
		for j, v := range prev {
			pb, ok1 := v.([]byte)
			if j >= len(got) {
				break
			}
			gb, ok2 := got[j].([]byte)
			if ok1 && ok2 && len(pb) > 0 && len(gb) > 0 && &pb[0] == &gb[0] {
				t.Fatalf("row %d: column %d shares bytes with the previous row", i, j)
			}
		}
		dst = got
	}
}

// TestDecodeRowColumnsIntoCorrupt は、壊れた値で DecodeRowColumns と同じく ErrCorrupt を返すことを確かめる。
func TestDecodeRowColumnsIntoCorrupt(t *testing.T) {
	b, err := EncodeRow(1, sql.Row{int64(1), "abc", nil})
	if err != nil {
		t.Fatal(err)
	}
	dst := make(sql.Row, 3)
	for _, c := range [][]byte{nil, b[:7], b[:len(b)-1], append(append([]byte{}, b...), 0)} {
		for _, want := range [][]bool{nil, {true, false, true}} {
			_, _, e1 := DecodeRowColumns(c, want)
			_, row, e2 := DecodeRowColumnsInto(c, want, dst)
			if e1 != ErrCorrupt || e2 != ErrCorrupt || row != nil {
				t.Errorf("%x %v: %v and %v (%v)", c, want, e1, e2, row)
			}
		}
	}
}
