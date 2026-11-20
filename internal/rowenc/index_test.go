package rowenc

import (
	"bytes"
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
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

// checkOrder は、値の組について「キーの順序 = 列の型による比較（NULL が先頭）」を確かめる。
func checkOrder(t *testing.T, typ sql.Type, vals []any) {
	t.Helper()
	ctx := context.Background()
	col := IndexColumn{Type: typ}
	for _, a := range vals {
		for _, b := range vals {
			ka, _, err := AppendIndexValue(nil, col, a)
			if err != nil {
				t.Fatal(err)
			}
			kb, _, err := AppendIndexValue(nil, col, b)
			if err != nil {
				t.Fatal(err)
			}
			var want int
			switch {
			case a == nil && b == nil:
				want = 0
			case a == nil:
				want = -1
			case b == nil:
				want = 1
			default:
				if want, err = typ.Compare(ctx, a, b); err != nil {
					t.Fatal(err)
				}
			}
			if got := sign(bytes.Compare(ka, kb)); got != sign(want) {
				t.Errorf("%s: order of %#v vs %#v: key %d, type %d", typ, a, b, got, want)
			}
		}
	}
}

func TestIndexOrderStrings(t *testing.T) {
	ci := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_unicode_520_ci)
	vals := []any{nil, "", "a", "A", "a ", "ab", "aB", "b", "B", "z", "Z", "あ", "ア", "日本", "日本語", "\x00", "é", "e", "E", "_", "-", "0", "9"}
	checkOrder(t, ci, vals)

	r := rand.New(rand.NewPCG(1, 2))
	alphabet := []rune{'a', 'A', 'b', 'B', ' ', 'é', 'あ', '日', '_'}
	var random []any
	for range 200 {
		n := r.IntN(5)
		rs := make([]rune, n)
		for i := range rs {
			rs[i] = alphabet[r.IntN(len(alphabet))]
		}
		random = append(random, string(rs))
	}
	checkOrder(t, ci, random)

	bin := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_bin)
	checkOrder(t, bin, []any{nil, "", "a", "A", "ab", "b", "B"})
}

func TestIndexOrderIntsAndTimes(t *testing.T) {
	checkOrder(t, types.Int64, []any{nil, int64(-5), int64(0), int64(7)})
	checkOrder(t, types.Uint64, []any{nil, uint64(0), uint64(1), uint64(1 << 63), uint64(1<<64 - 1)})
	dt := types.DatetimeMaxPrecision
	checkOrder(t, dt, []any{
		nil, time.Time{},
		time.Date(1969, 12, 31, 23, 59, 59, 999999000, time.UTC),
		time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 2, 10, 0, 0, 1000, time.UTC),
	})
}

func TestIndexOrderBinary(t *testing.T) {
	vb := types.MustCreateBinary(sqltypes.VarBinary, 255)
	checkOrder(t, vb, []any{nil, []byte{}, []byte{0}, []byte{0, 0}, []byte{1}, []byte("a")})
}

func TestIndexPrefix(t *testing.T) {
	ci := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_unicode_520_ci)
	col := IndexColumn{Type: ci, Prefix: 3}
	k1, tr1, _ := AppendIndexValue(nil, col, "日本語テキスト")
	k2, tr2, _ := AppendIndexValue(nil, col, "日本語")
	if !bytes.Equal(k1, k2) || !tr1 || tr2 {
		t.Errorf("prefix keys: equal=%v truncated=%v,%v", bytes.Equal(k1, k2), tr1, tr2)
	}
}

func TestKeyableType(t *testing.T) {
	keyable := []sql.Type{
		types.Int64, types.Uint64, types.Int8, types.DatetimeMaxPrecision, types.Date, types.Timestamp,
		types.MustCreateString(sqltypes.VarChar, 20, sql.Collation_utf8mb4_unicode_520_ci),
		types.MustCreateBinary(sqltypes.VarBinary, 20),
	}
	for _, typ := range keyable {
		if !KeyableType(typ, 0) {
			t.Errorf("%s should be keyable", typ)
		}
	}
	for _, typ := range []sql.Type{types.Float64, types.MustCreateDecimalType(10, 2), types.JSON} {
		if KeyableType(typ, 0) {
			t.Errorf("%s should not be keyable", typ)
		}
	}
}

// 文字列の主キー（docs/plan.md の段階 38）: キーのバイト順は照合順序の比較と一致し、違うキーの一方が他方の前方に
// ならない（行のキーは主キーの後に時刻が続くので、前方が一致すると別の行と取り違える）。
func TestStringPKOrder(t *testing.T) {
	ctx := context.Background()
	r := rand.New(rand.NewPCG(3, 4))
	alphabet := []rune{'a', 'A', 'b', 'B', ' ', 'é', 'e', 'あ', 'ア', 'は', 'ば', '日', '_', '0', 'ß', 's', '😀'}
	vals := []string{"", "a", "A", "a ", "ab", "b", "あ", "ア", "は", "ば", "ss", "ß"}
	for range 300 {
		rs := make([]rune, r.IntN(6))
		for i := range rs {
			rs[i] = alphabet[r.IntN(len(alphabet))]
		}
		vals = append(vals, string(rs))
	}
	for _, coll := range []sql.CollationID{sql.Collation_utf8mb4_0900_ai_ci, sql.Collation_utf8mb4_unicode_520_ci, sql.Collation_utf8mb4_general_ci, sql.Collation_utf8mb4_bin} {
		typ := types.MustCreateString(sqltypes.VarChar, 255, coll)
		if !StringPKType(typ) {
			t.Fatalf("%s is not a string primary key type", typ)
		}
		sch := sql.NewPrimaryKeySchema(sql.Schema{{Name: "k", Type: typ, PrimaryKey: true}}, 0)
		keys := make([][]byte, len(vals))
		for i, v := range vals {
			k, err := EncodePK(nil, sch, sql.Row{v})
			if err != nil {
				t.Fatal(err)
			}
			if k2, err := AppendPKValue(nil, typ, v); err != nil || !bytes.Equal(k, k2) {
				t.Fatalf("AppendPKValue differs from EncodePK for %q: %v", v, err)
			}
			keys[i] = k
		}
		for i, a := range vals {
			for j, b := range vals {
				want, err := typ.Compare(ctx, a, b)
				if err != nil {
					t.Fatal(err)
				}
				if got := sign(bytes.Compare(keys[i], keys[j])); got != sign(want) {
					t.Errorf("%s: order of %q vs %q: key %d, collation %d", coll, a, b, got, want)
				}
				if want != 0 && bytes.HasPrefix(keys[i], keys[j]) {
					t.Errorf("%s: key of %q starts with the key of %q", coll, a, b)
				}
			}
		}
	}
	// NULL と壊れた UTF-8 は、panic せずにエラー。
	typ := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_0900_ai_ci)
	if _, err := AppendPKValue(nil, typ, nil); err == nil {
		t.Error("NULL primary key encoded")
	}
	if _, err := AppendPKValue(nil, typ, "a\xffb"); err == nil {
		t.Error("malformed UTF-8 primary key encoded")
	}
	// 主キーに使えない型。
	for _, bad := range []sql.Type{
		types.Text, types.LongText, types.MustCreateBinary(sqltypes.VarBinary, 10),
		types.MustCreateString(sqltypes.VarChar, 10, sql.Collation_latin1_swedish_ci),
		types.MustCreateEnumType([]string{"a"}, sql.Collation_utf8mb4_0900_ai_ci),
	} {
		if StringPKType(bad) {
			t.Errorf("%s accepted as a string primary key type", bad)
		}
	}
}

// SkipIndexValue は AppendIndexValue が書いた値をちょうど読み飛ばす（エントリから文字列の主キーを取り出すため）。
func TestSkipIndexValue(t *testing.T) {
	text := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_0900_ai_ci)
	cols := []IndexColumn{
		{Type: types.Int64}, {Type: text}, {Type: text, Prefix: 2}, {Type: types.Datetime},
		{Type: types.MustCreateBinary(sqltypes.VarBinary, 20)}, {Type: types.Uint64},
	}
	vals := [][]any{
		{int64(-5), "あいう", "abcdef", time.Date(2026, 10, 12, 1, 2, 3, 4000, time.UTC), []byte{0, 1, 0, 0xff}, uint64(1) << 63},
		{nil, "", nil, nil, []byte{}, nil},
		{int64(0), "\x01\x00", "x", time.Unix(0, 0).UTC(), []byte{1}, uint64(7)},
	}
	pk, err := AppendPKValue(nil, text, "key\x00name")
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
	if _, err := SkipIndexValue(IndexColumn{Type: text}, []byte{0x01, 0x01, 0x02}); err == nil {
		t.Error("truncated entry was not an error")
	}
}

// 文字列、バイト列、日時、整数を組み合わせた主キー（docs/plan.md の段階 70）: キーのバイト順は列ごとの型の比較を
// 前から並べた順と一致し、違うキーの一方が他方の前方にならない。
func TestMixedPKOrder(t *testing.T) {
	ctx := context.Background()
	text := types.MustCreateString(sqltypes.VarChar, 64, sql.Collation_utf8mb4_unicode_520_ci)
	vb := types.MustCreateBinary(sqltypes.VarBinary, 64)
	bin := types.MustCreateBinary(sqltypes.Binary, 3)
	enum := types.MustCreateEnumType([]string{"hit", "404", "a"}, sql.Collation_utf8mb4_0900_ai_ci)
	r := rand.New(rand.NewPCG(7, 8))
	gens := map[string]func() any{
		types.Int64.String(): func() any { return int64(r.IntN(5) - 2) },
		types.Uint64.String(): func() any {
			return []uint64{0, 1, 1 << 63, 1<<64 - 1}[r.IntN(4)]
		},
		text.String(): func() any {
			return []string{"", "a", "A", "a ", "ab", "aB", "b", "あ", "ア", "\x00", "é", "e"}[r.IntN(12)]
		},
		vb.String(): func() any {
			b := []byte{}
			for range r.IntN(4) {
				b = append(b, []byte{0x00, 0x01, 0xff, 'a', 'A'}[r.IntN(5)])
			}
			return b
		},
		// BINARY(n) の値は、エンジンが末尾を 0x00 で埋めて n バイトにしたもの
		bin.String(): func() any {
			b := make([]byte, 3)
			for i := range r.IntN(4) {
				b[i] = []byte{0x00, 0x01, 0xff, 'a'}[r.IntN(4)]
			}
			return b
		},
		// ENUM の値は番号（1 から）
		enum.String(): func() any { return uint16(1 + r.IntN(3)) },
		types.Date.String(): func() any {
			return []time.Time{{}, time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}[r.IntN(3)]
		},
		types.DatetimeMaxPrecision.String(): func() any {
			return []time.Time{{}, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 0, 0, 0, 1000, time.UTC), time.Date(2026, 10, 20, 0, 0, 1, 0, time.UTC)}[r.IntN(4)]
		},
		types.Timestamp.String(): func() any {
			return []time.Time{time.Date(1970, 1, 1, 0, 0, 1, 0, time.UTC), time.Date(2038, 1, 19, 3, 14, 7, 0, time.UTC)}[r.IntN(2)]
		},
	}
	all := []sql.Type{types.Int64, types.Uint64, text, vb, bin, enum, types.Date, types.DatetimeMaxPrecision, types.Timestamp}
	for _, typ := range all {
		if !PKType(typ) {
			t.Fatalf("%s is not a primary key type", typ)
		}
	}
	for range 200 {
		n := 1 + r.IntN(3)
		sch := make(sql.Schema, n)
		for i := range sch {
			sch[i] = &sql.Column{Name: string(rune('a' + i)), Type: all[r.IntN(len(all))], PrimaryKey: true}
		}
		pks := sql.NewPrimaryKeySchema(sch)
		rows := make([]sql.Row, 30)
		keys := make([][]byte, len(rows))
		for i := range rows {
			rows[i] = make(sql.Row, n)
			for j, c := range sch {
				rows[i][j] = gens[c.Type.String()]()
			}
			k, err := EncodePK(nil, pks, rows[i])
			if err != nil {
				t.Fatal(err)
			}
			keys[i] = k
		}
		for i, a := range rows {
			for j, b := range rows {
				want := 0
				for c, col := range sch {
					w, err := col.Type.Compare(ctx, a[c], b[c])
					if err != nil {
						t.Fatal(err)
					}
					if want = sign(w); want != 0 {
						break
					}
				}
				if got := sign(bytes.Compare(keys[i], keys[j])); got != want {
					t.Fatalf("%v: order of %#v vs %#v: key %d, values %d", sch, a, b, got, want)
				}
				if want != 0 && bytes.HasPrefix(keys[i], keys[j]) {
					t.Fatalf("%v: key of %#v starts with the key of %#v", sch, a, b)
				}
			}
		}
	}
	// エンジンはバイト列の列に string を入れることもある。同じキーになる。
	k1, err1 := AppendPKValue(nil, vb, []byte("a\x00b"))
	k2, err2 := AppendPKValue(nil, vb, "a\x00b")
	if err1 != nil || err2 != nil || !bytes.Equal(k1, k2) {
		t.Errorf("bytes and string keys differ: %x %x %v %v", k1, k2, err1, err2)
	}
	for _, typ := range all {
		if _, err := AppendPKValue(nil, typ, nil); err == nil {
			t.Errorf("%s: NULL primary key encoded", typ)
		}
	}
	// 主キーに使えない型。
	for _, bad := range []sql.Type{
		types.Blob, types.Text,
		types.MustCreateString(sqltypes.VarChar, 10, sql.Collation_latin1_swedish_ci),
		types.MustCreateSetType([]string{"a"}, sql.Collation_utf8mb4_0900_ai_ci),
		types.MustCreateDecimalType(10, 2), types.Float64, types.Time, types.JSON, types.Year,
	} {
		if PKType(bad) {
			t.Errorf("%s accepted as a primary key type", bad)
		}
	}
}
