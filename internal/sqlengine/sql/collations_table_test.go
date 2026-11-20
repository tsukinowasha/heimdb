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

// HeimDB が足したファイル（docs/engine.md）。

package sql

import "testing"

// 表で包んだ重みの関数は、すべての照合順序で、U+FFFF までのすべての文字と、それより後のいくつかの文字について、元の
// 関数と同じ重みを返す（インデックスのキーが変わらない）。ASCII の表（ASCIIWeights）も同じ重み。
func TestTableSorterMatchesBase(t *testing.T) {
	samples := []rune{-1, bmpRunes, 0x1F600, 0x20000, 0x10FFFF}
	n := 0
	for i := range collationArray {
		base := sorterBase[i]
		if base == nil {
			continue
		}
		n++
		got := collationArray[i].Sorter
		for r := rune(0); r < bmpRunes; r++ {
			if g, w := got(r), base(r); g != w {
				t.Fatalf("%s: weight of %U = %d, want %d", collationArray[i].Name, r, g, w)
			}
		}
		for _, r := range samples {
			if g, w := got(r), base(r); g != w {
				t.Fatalf("%s: weight of %U = %d, want %d", collationArray[i].Name, r, g, w)
			}
		}
		ascii := CollationID(i).ASCIIWeights()
		for r := rune(0); r < 128; r++ {
			if g, w := ascii[r], base(r); g != w {
				t.Fatalf("%s: ASCII weight of %q = %d, want %d", collationArray[i].Name, r, g, w)
			}
		}
	}
	if n < 50 {
		t.Errorf("only %d collations have a sorter", n)
	}
	// WordPress の既定の照合順序と、照合順序を指定しないとき。
	for _, c := range []CollationID{Collation_utf8mb4_unicode_520_ci, Collation_utf8mb4_unicode_ci, Collation_Unspecified, Collation_Default} {
		if c.Sorter() == nil || c.ASCIIWeights() == nil {
			t.Errorf("%s has no sorter", c.Name())
		}
		if c.Sorter()('A') != c.Sorter()('a') && !collationArray[c].IsCaseSensitive {
			t.Errorf("%s: 'A' and 'a' differ", c.Name())
		}
	}
}

// U+FFFF までの文字は、最初に使ったときに作った表で引き、元の関数を 1 文字ごとに呼ばない（日本語の LIKE と比較）。
// それより後の文字は元の関数を呼ぶ。
func TestTableSorterCachesBMP(t *testing.T) {
	calls := 0
	f := func(r rune) int32 {
		calls++
		return int32(r) + 1
	}
	sorter, ascii := tableSorter(f)
	if calls != 128 || ascii['a'] != 'a'+1 {
		t.Fatalf("ASCII table: %d calls, a=%d", calls, ascii['a'])
	}
	if w := sorter('あ'); w != 'あ'+1 {
		t.Fatalf("weight of あ = %d", w)
	}
	built := calls
	for _, r := range "吾輩は猫である。ＡＢＣé" {
		if w := sorter(r); w != r+1 {
			t.Fatalf("weight of %q = %d", r, w)
		}
	}
	if calls != built {
		t.Errorf("BMP runes called the base function %d more times", calls-built)
	}
	if w := sorter(0x1F600); w != 0x1F601 || calls != built+1 {
		t.Errorf("weight of U+1F600 = %d after %d calls", w, calls-built)
	}
}
