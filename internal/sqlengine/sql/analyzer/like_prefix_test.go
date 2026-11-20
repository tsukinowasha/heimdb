package analyzer

import (
	"testing"
	"unicode/utf8"
)

// incrementLastRune は、最後の文字を次の文字に替える。U+10FFFF で終わる prefix には上限がなく、サロゲートを飛ばす
// （上流の #3581）。
func TestIncrementLastRune(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"ap", "aq", true},
		{"## ", "##!", true},
		{"山田", "山由", true}, // U+7530 の次は U+7531
		{"a߿", "aࠀ", true},
		{"a퟿", "a", true},
		{"a\U0010FFFF", "", false},
		{"a\xff", "", false},
	} {
		got, ok := incrementLastRune(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("incrementLastRune(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
		if ok && !utf8.ValidString(got) {
			t.Errorf("incrementLastRune(%q) = %q is not valid UTF-8", c.in, got)
		}
	}
}
