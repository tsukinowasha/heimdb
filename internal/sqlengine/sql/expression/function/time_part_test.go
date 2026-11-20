// Copyright 2026 Tsukinowa Inc.

package function

import (
	"math/rand"
	"regexp"
	"testing"
)

// isTimeOnly と isDatetimeWithTime は、もとの正規表現と同じ文字列を受け付ける（段階 114 で正規表現を手で書いた）。
func TestTimePartMatchersAgreeWithRegexps(t *testing.T) {
	timeOnly := regexp.MustCompile(`^-?\d+(:\d{1,2}){0,2}(\.\d*)?$`)
	datetimeWithTime := regexp.MustCompile(`^\d{2,4}[[:punct:]]\d{1,2}[[:punct:]]\d{1,2} +\d`)
	fixed := []string{"13:04:05", "-838:59:59", "1:2", "130405", "13:04:05.5", "13:", "1:234", "", "-", "1.", "2024-01-02 13:04:05",
		"24/1/2  1", "2024-01-02", "2007-12-11T20:21:22Z", "20240-01-02 1", "2024-01-02 x", "abc", "12:34:56:78"}
	alphabet := []byte("0123456789:-./ Tx+")
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		b := make([]byte, r.Intn(14))
		for j := range b {
			b[j] = alphabet[r.Intn(len(alphabet))]
		}
		fixed = append(fixed, string(b))
	}
	for _, s := range fixed {
		if got, want := isTimeOnly(s), timeOnly.MatchString(s); got != want {
			t.Errorf("isTimeOnly(%q) = %v, want %v", s, got, want)
		}
		if got, want := isDatetimeWithTime(s), datetimeWithTime.MatchString(s); got != want {
			t.Errorf("isDatetimeWithTime(%q) = %v, want %v", s, got, want)
		}
	}
}
