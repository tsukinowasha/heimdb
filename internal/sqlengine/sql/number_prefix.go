// Copyright 2022 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 上流の go-mysql-server（main）の sql/types/number.go から、文字列の先頭の数の部分を切り出す関数を取り込んだ（docs/engine.md）

package sql

import (
	"strings"
	"unicode"
)

// IntCutSet と NumericCutSet は、文字列を整数と数にするときに前後から除く字（types の intCutSet、numericCutSet と同じ）。
const (
	IntCutSet     = " \t"
	NumericCutSet = " \t\n\r\v\f"
)

// TruncateStringToInt trims any whitespace from s, then truncates the string to the left most characters that make
// up a valid integer. Empty strings are converted "0". Additionally, returns a flag indicating if truncation occurred.
// HeimDB: 上流の go-mysql-server（main、sql/types/number.go）から取り込んだ。
func TruncateStringToInt(s string) (string, bool) {
	var seenDigit bool
	s = strings.Trim(s, IntCutSet)
	i, n := 0, len(s)
	for ; i < n; i++ {
		c := rune(s[i])
		if unicode.IsDigit(c) {
			seenDigit = true
			continue
		}
		if i == 0 && (c == '-' || c == '+') {
			continue
		}
		break
	}
	if !seenDigit {
		return "0", i != n
	}
	return s[:i], i != n
}

// TruncateStringToDouble returns the longest prefix of |s| that is a
// valid double, after trimming surrounding whitespace, and reports
// whether any characters after that prefix were truncated.
//
// The prefix is "0" if |s| does not begin with a number.
//
// An exponent marker and optional sign with no digits after them
// are dropped from the prefix, and count as truncation only if
// |truncExp| is true.
// HeimDB: 上流の go-mysql-server（main、sql/types/number.go）から取り込んだ。
func TruncateStringToDouble(s string, truncExp bool) (prefix string, truncated bool) {
	// expIndex is the exponent marker's position. It follows a digit,
	// so 0 means no exponent has been seen.
	var signIndex, expIndex int
	var seenDigit, seenDot bool
	s = strings.Trim(s, NumericCutSet)
	i, n := 0, len(s)
	for ; i < n; i++ {
		char := rune(s[i])
		if unicode.IsDigit(char) {
			seenDigit = true
			continue
		}
		// A decimal point is only valid before the exponent.
		if char == '.' && !seenDot && expIndex == 0 {
			seenDot = true
			continue
		}
		if (char == 'e' || char == 'E') && expIndex == 0 && seenDigit {
			expIndex = i
			signIndex = i + 1 // allow a sign following exponent
			continue
		}
		if i == signIndex && (char == '-' || char == '+') {
			continue
		}
		break
	}
	if !seenDigit {
		return "0", i != n
	}
	// The scan ended on the exponent marker or its sign, so the
	// exponent has no digits.
	if expIndex != 0 && !unicode.IsDigit(rune(s[i-1])) {
		return s[:expIndex], i != n || truncExp
	}
	return s[:i], i != n
}
