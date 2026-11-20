// Copyright 2026 Tsukinowa Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 段階 125: StringType.Compare は、同じバイト列の文字列を照合順序の重みを引かずに等しいとする。前と同じ答えに
// なることを、前の比べ方（stage125WeightCompare）と比べて確かめる。

// stage125WeightCompare は、段階 125 より前の Compare の、両辺が文字列のときの比べ方（文字ごとの重み）。
func stage125WeightCompare(t StringType, as, bs string) (int, error) {
	encoder := t.collation.CharacterSet().Encoder()
	getRuneWeight := t.collation.Sorter()
	for len(as) > 0 && len(bs) > 0 {
		ar, aRead := encoder.NextRune(as)
		br, bRead := encoder.NextRune(bs)
		if aRead == 0 || bRead == 0 || aRead == utf8.RuneError || bRead == utf8.RuneError {
			return 0, fmt.Errorf("malformed string encountered while comparing")
		}
		aWeight, bWeight := getRuneWeight(ar), getRuneWeight(br)
		if aWeight < bWeight {
			return -1, nil
		} else if aWeight > bWeight {
			return 1, nil
		}
		as, bs = as[aRead:], bs[bRead:]
	}
	if len(as) < len(bs) {
		return -1, nil
	} else if len(as) > len(bs) {
		return 1, nil
	}
	return 0, nil
}

func stage125Types() map[string]StringType {
	out := map[string]StringType{}
	for _, c := range []sql.CollationID{
		sql.Collation_utf8mb4_unicode_520_ci, sql.Collation_utf8mb4_general_ci, sql.Collation_utf8mb4_0900_ai_ci,
		sql.Collation_utf8mb4_0900_bin, sql.Collation_utf8mb4_bin, sql.Collation_utf8mb3_general_ci,
		sql.Collation_latin1_swedish_ci, sql.Collation_latin1_bin,
	} {
		out[c.Name()] = MustCreateString(sqltypes.VarChar, 255, c).(StringType)
		out[c.Name()+" text"] = MustCreateString(sqltypes.Text, TextBlobMax, c).(StringType)
	}
	out["binary"] = MustCreateBinary(sqltypes.VarBinary, 255).(StringType)
	out["blob"] = MustCreateBinary(sqltypes.Blob, TextBlobMax).(StringType)
	return out
}

// TestStage125StringCompare は、照合順序ごとに、等しいバイト列、大文字と小文字、アクセント、末尾の空白、長さの
// 違い、空の文字列、正しくない UTF-8 で、Compare が前の比べ方と同じ答えを返すことを確かめる。
func TestStage125StringCompare(t *testing.T) {
	ctx := context.Background()
	words := []string{"", "1", "0", "a", "A", "a ", "a  ", " a", "ab", "aB", "b", "é", "e", "É", "ß", "ss", "spam", "trash",
		"comment", "note", "Ä", "ä", "日本", "日本語", "🍣", "\x00", "a\x00", "\xff", "\xc3", "a\xffb", "\xe6\x97"}
	for name, typ := range stage125Types() {
		for _, a := range words {
			for _, b := range words {
				got, err := typ.Compare(ctx, a, b)
				want, werr := stage125WeightCompare(typ, a, b)
				if (err != nil) != (werr != nil) || got != want {
					t.Errorf("%s: Compare(%q, %q) = %d, %v; want %d, %v", name, a, b, got, err, want, werr)
				}
				if a == b && (got != 0 || err != nil) {
					t.Errorf("%s: Compare(%q, %q) = %d, %v; want 0", name, a, b, got, err)
				}
			}
		}
	}
}

// TestStage125StringCompareProperty は、乱数の文字列（同じものを多く含む）で、Compare が前の比べ方と同じ答えを
// 返すことを確かめる。
func TestStage125StringCompareProperty(t *testing.T) {
	ctx := context.Background()
	r := rand.New(rand.NewSource(125))
	alphabet := []string{"a", "A", "b", "é", "E", " ", "ß", "s", "日", "\x00", "\xff", "1", "0"}
	gen := func() string {
		s := ""
		for range r.Intn(6) {
			s += alphabet[r.Intn(len(alphabet))]
		}
		return s
	}
	for name, typ := range stage125Types() {
		for range 3000 {
			a := gen()
			b := a
			if r.Intn(2) == 0 {
				b = gen()
			}
			got, err := typ.Compare(ctx, a, b)
			want, werr := stage125WeightCompare(typ, a, b)
			if (err != nil) != (werr != nil) || got != want {
				t.Fatalf("%s: Compare(%q, %q) = %d, %v; want %d, %v", name, a, b, got, err, want, werr)
			}
		}
	}
}

// TestStage125StringCompareConverted は、文字列でない値（数、バイト列、NULL）を変換してから比べるときも、前と
// 同じ答えになることを確かめる（同じバイト列になった後だけ早道を通る）。
func TestStage125StringCompareConverted(t *testing.T) {
	ctx := context.Background()
	vc := MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_unicode_520_ci).(StringType)
	bin := MustCreateBinary(sqltypes.VarBinary, 255).(StringType)
	for _, c := range []struct {
		typ  StringType
		a, b any
		want int
	}{
		{vc, int64(1), "1", 0},
		{vc, "1", int64(1), 0},
		{vc, int64(1), "1 ", -1},
		{vc, "a", []byte("a"), 0},
		{vc, []byte("A"), "a", 0},
		{vc, nil, "a", 1}, // CompareNulls の決まり（NULL を後ろにする）
		{vc, "a", nil, -1},
		{vc, nil, nil, 0},
		{bin, []byte("a"), []byte("a"), 0},
		{bin, []byte("a"), []byte("A"), 1},
		{bin, "ab", []byte("ab"), 0},
		{bin, []byte("a "), []byte("a"), 1},
		{bin, int64(7), "7", 0},
	} {
		got, err := c.typ.Compare(ctx, c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("%s: Compare(%#v, %#v) = %d, %v; want %d", c.typ, c.a, c.b, got, err, c.want)
		}
	}
}
