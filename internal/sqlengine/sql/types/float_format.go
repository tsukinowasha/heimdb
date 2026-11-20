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

// Modified by Tsukinowa Inc.: DOUBLE と FLOAT の値を MySQL と同じ形の文字列にする（docs/engine.md）
// Modified by Tsukinowa Inc.: 範囲を超えた値（無限大と NaN）を MySQL と同じく '0' にする（段階 116 の 2 度目のレビュー。docs/engine.md）

package types

import (
	"math"
	"strconv"
	"strings"
)

// MySQL の DOUBLE と FLOAT の文字列の形（mysql-server 8.0 の strings/dtoa.cc の my_gcvt）。
//
// 数字は、DOUBLE は元に戻せるいちばん短い桁（Go の精度 -1 と同じ）、FLOAT は float32 の値を 6 桁（FLT_DIG）に
// 丸めて末尾の 0 を除いたもの。小数点の位置を decpt（値 = 0.d1d2... × 10^decpt）、桁の数を n とすると、
// decpt >= -14 で、decpt <= 15 か n > decpt のとき、指数を使わない形（20250331、0.00000015、1000000000000000.5）。
// それ以外は指数の形（1e15、1.234567890123456e15、1e-16）で、指数に + と先頭の 0 を付けない。width は文字列の
// 上限（結果の行では制限なし。CONCAT と CAST(... AS CHAR) では DOUBLE 22、FLOAT 12）で、指数を使わない形が
// 長すぎれば指数の形にする。

const (
	// DoubleStringWidth と FloatStringWidth は、CONCAT や CAST(... AS CHAR) で文字列にするときの上限
	// （MySQL の MAX_DOUBLE_STR_LENGTH、MAX_FLOAT_STR_LENGTH）。
	DoubleStringWidth = 22
	FloatStringWidth  = 12
	// noWidth は、結果の行の値（上限なし）。
	noWidth = 1 << 20
	// maxDecptForF は、指数を使わない形にする小数点の位置の上限（MySQL の MAX_DECPT_FOR_F_FORMAT = DBL_DIG）。
	maxDecptForF = 15
)

// AppendMySQLDouble は、DOUBLE の値 x を MySQL と同じ形で dst に足す。width は上限（0 なら上限なし）。
func AppendMySQLDouble(dst []byte, x float64, width int) []byte {
	return appendMySQLFloat(dst, x, width, false)
}

// AppendMySQLFloat は、FLOAT の値 x を MySQL と同じ形で dst に足す。width は上限（0 なら上限なし）。
func AppendMySQLFloat(dst []byte, x float32, width int) []byte {
	return appendMySQLFloat(dst, float64(x), width, true)
}

func appendMySQLFloat(dst []byte, x float64, width int, isFloat bool) []byte {
	if width <= 0 {
		width = noWidth
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		// 範囲を超えた値（SUM(dbl) が DOUBLE の上限を超えたとき）は、MySQL の my_gcvt（DTOA_OVERFLOW）と同じく '0'
		// （段階 116 の 2 度目のレビュー。前は Go の形の '+Inf'）
		return append(dst, '0')
	}
	sign := 0
	if math.Signbit(x) {
		dst = append(dst, '-')
		x = -x
		sign = 1
	}
	if x == 0 {
		return append(dst, '0')
	}
	// 数字（先頭の 0 と末尾の 0 を除いたもの）と指数を得る。'e' の形は d.ddde±XX。
	var e []byte
	if isFloat {
		e = strconv.AppendFloat(nil, x, 'e', 5, 64)
	} else {
		e = strconv.AppendFloat(nil, x, 'e', -1, 64)
	}
	mant, exp, _ := strings.Cut(string(e), "e")
	digits := strings.TrimRight(strings.Replace(mant, ".", "", 1), "0")
	if digits == "" {
		digits = "0"
	}
	n, _ := strconv.Atoi(exp)
	decpt := n + 1

	if decpt >= -(maxDecptForF-1) && (decpt <= maxDecptForF || len(digits) > decpt) {
		f := fixedForm(digits, decpt)
		if sign+len(f) <= width {
			return append(dst, f...)
		}
	}
	return append(dst, expForm(digits, decpt)...)
}

// fixedForm は、指数を使わない形（digits と小数点の位置 decpt から）。
func fixedForm(digits string, decpt int) string {
	switch {
	case decpt <= 0:
		return "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		return digits + strings.Repeat("0", decpt-len(digits))
	default:
		return digits[:decpt] + "." + digits[decpt:]
	}
}

// expForm は、指数の形（d[.ddd]eN。指数に + と先頭の 0 を付けない）。
func expForm(digits string, decpt int) string {
	var b strings.Builder
	b.WriteByte(digits[0])
	if len(digits) > 1 {
		b.WriteByte('.')
		b.WriteString(digits[1:])
	}
	b.WriteByte('e')
	b.WriteString(strconv.Itoa(decpt - 1))
	return b.String()
}
