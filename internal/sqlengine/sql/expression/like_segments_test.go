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

package expression

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 大小、アクセント、全角と半角、日本語、合字、トルコ語の i、結合文字、無視される文字、NUL、BMP の外の文字（表を
// 引かずに元の関数を呼ぶ）、U+FFFD、壊れた UTF-8。
var likeSegmentPieces = []string{
	"a", "A", "á", "Á", "b", "B", "e", "é", "s", "ß", "ss", "ǆ", "ǅ", "ı", "İ", "i", "\u0301",
	"あ", "ア", "ｱ", "ａ", "Ａ", "猫", " ", "\u00ad", "\u200b", "\x00", "😀", "\U00020000",
	"\ufffd", "\xff", "\xe3\x81", "%", "_", "\\", "|",
}

// likeSegmentCollations は、重みの関数を持つ utf8mb4 のすべての照合順序。
func likeSegmentCollations() []sql.CollationID {
	var cs []sql.CollationID
	for it := sql.NewCollationsIterator(); ; {
		c, ok := it.Next()
		if !ok {
			return cs
		}
		if c.CharacterSet == sql.CharacterSet_utf8mb4 && c.Sorter != nil {
			cs = append(cs, c.ID)
		}
	}
}

// usesWildcardRune は、パターンに `_`（どの 1 文字にも一致するノード）があるか（likeSegments を使わない）。
func usesWildcardRune(m LikeMatcher) bool {
	for _, node := range m.nodes {
		if r, ok := node.(likeMatcherRune); ok && r.sortOrder < 0 {
			return true
		}
	}
	return false
}

// 区間ごとの照合（likeSegments）は、ランダムな文字列とパターンとエスケープの文字で、元の後戻りの照合（matchNodes）
// と同じ結果を返す。
func TestLikeSegmentsMatchNodes(t *testing.T) {
	rnd := rand.New(rand.NewSource(54))
	str := func(max int) string {
		var b strings.Builder
		for n := rnd.Intn(max + 1); n > 0; n-- {
			b.WriteString(likeSegmentPieces[rnd.Intn(len(likeSegmentPieces))])
		}
		return b.String()
	}
	pattern := func(escape string) string {
		var b strings.Builder
		for n := rnd.Intn(8); n > 0; n-- {
			switch rnd.Intn(4) {
			case 0:
				b.WriteString("%")
			case 1:
				// エスケープした文字（`\%`、`\_`、エスケープの文字そのもの、普通の文字）
				b.WriteString(escape)
				b.WriteString([]string{"%", "_", escape, "a", "あ", "😀"}[rnd.Intn(6)])
			default:
				p := likeSegmentPieces[rnd.Intn(len(likeSegmentPieces))]
				if strings.Contains(p, escape) || (p == "_" && rnd.Intn(4) != 0) {
					p = "a"
				}
				b.WriteString(p)
			}
		}
		return b.String()
	}
	// matching は、パターンのノードから、一致することの多い文字列を作る（`%` にはランダムな文字列を入れる）。
	matching := func(m LikeMatcher) string {
		var b strings.Builder
		for _, node := range m.nodes {
			switch node := node.(type) {
			case likeMatcherRune:
				b.WriteRune(node.original)
			case likeMatcherAny:
				b.WriteString(str(3))
			}
		}
		return b.String()
	}
	collations := likeSegmentCollations()
	if len(collations) < 50 {
		t.Fatalf("only %d utf8mb4 collations have a sorter", len(collations))
	}
	fast, matched := 0, 0
	for _, c := range collations {
		for _, escape := range []rune{'\\', '|', 'あ'} {
			for i := 0; i < 300; i++ {
				p := pattern(string(escape))
				m, err := ConstructLikeMatcher(c, p, escape)
				if err != nil {
					continue
				}
				if (m.fast == nil) != usesWildcardRune(m) {
					t.Fatalf("%s: %q ESCAPE %q: likeSegments %v", c.Name(), p, escape, m.fast != nil)
				}
				if m.fast == nil {
					continue
				}
				fast++
				for j := 0; j < 20; j++ {
					s := str(8)
					if rnd.Intn(3) == 0 {
						s = matching(m)
					}
					got, want := m.Match(s), m.matchNodes(s)
					if got != want {
						t.Fatalf("%s: %q LIKE %q ESCAPE %q = %v, want %v", c.Name(), s, p, escape, got, want)
					}
					if want {
						matched++
					}
				}
			}
		}
	}
	if fast < 30000 || matched < 50000 {
		t.Errorf("too few cases: %d patterns, %d matches", fast, matched)
	}
}

// 決まった例。WordPress の検索と `esc_like` のパターン、空のパターン、末尾と先頭の区間の重なり。
func TestLikeSegmentsExamples(t *testing.T) {
	cases := []struct {
		s, p string
		want bool
	}{
		{"Lorem heimdbneedle ipsum", "%HEIMDBNEEDLE%", true},
		{"Lorem ipsum", "%heimdbneedle%", false},
		{"吾輩は猫である", "%猫%", true},
		{"ｱｲｳ", "%ア%", true}, // unicode_520_ci は全角と半角のカナを同じ重みにする
		{"アイウ", "%カ%", false},
		{"_transient_foo", "\\_transient\\_%", true},
		{"xtransient_foo", "\\_transient\\_%", false},
		{"", "", true},
		{"a", "", false},
		{"", "%", true},
		{"aba", "ab%ba", false},
		{"abba", "ab%ba", true},
		{"aXbXc", "a%b%c", true},
		{"aXcXb", "a%b%c", false},
		{"abc", "%abc", true},
		{"abc", "abc%", true},
		{"ab", "abc%", false},
		{"Ab�", "%b%", true}, // U+FFFD は正しい文字（上流の #3556）。壊れたバイト列だけが一致しない
		{"a�b", "%�%", true},
		{"Ab\xff", "%b%", false},
	}
	for _, tc := range cases {
		m, err := ConstructLikeMatcher(sql.Collation_utf8mb4_unicode_520_ci, tc.p, '\\')
		if err != nil {
			t.Fatal(err)
		}
		if m.fast == nil {
			t.Fatalf("%q did not use likeSegments", tc.p)
		}
		if got := m.Match(tc.s); got != tc.want || m.matchNodes(tc.s) != tc.want {
			t.Errorf("%q LIKE %q = %v (nodes %v), want %v", tc.s, tc.p, got, m.matchNodes(tc.s), tc.want)
		}
	}
}

// `_` を含むパターンと utf8mb4 以外の文字セットは、元の照合を使う。
func TestLikeSegmentsOnlyForPercent(t *testing.T) {
	for _, tc := range []struct {
		c sql.CollationID
		p string
	}{
		{sql.Collation_utf8mb4_unicode_520_ci, "a_c%"},
		{sql.Collation_utf8mb3_general_ci, "%abc%"},
		{sql.Collation_latin1_swedish_ci, "%abc%"},
	} {
		m, err := ConstructLikeMatcher(tc.c, tc.p, '\\')
		if err != nil {
			t.Fatal(err)
		}
		if m.fast != nil {
			t.Errorf("%s %q used likeSegments", tc.c.Name(), tc.p)
		}
	}
}

var likeBenchText = strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. ", 15)
var likeBenchJa = strings.Repeat("吾輩は猫である。名前はまだ無い。どこで生れたかとんと見当がつかぬ。何でも薄暗いじめじめした所でニャーニャー泣いていた事だけは記憶している。", 15)

func benchmarkLike(b *testing.B, text string) {
	m, err := ConstructLikeMatcher(sql.Collation_utf8mb4_unicode_520_ci, "%heimdbneedle%", '\\')
	if err != nil {
		b.Fatal(err)
	}
	m.Match(text)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if m.Match(text) {
			b.Fatal("matched")
		}
	}
}

// ベンチマークの記事の本文（約 1.9 KB）と、日本語の本文への検索の LIKE。
func BenchmarkLikeSearch(b *testing.B)         { benchmarkLike(b, likeBenchText) }
func BenchmarkLikeSearchJapanese(b *testing.B) { benchmarkLike(b, likeBenchJa) }
