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

// Modified by Tsukinowa Inc.: DOUBLE と FLOAT の文字列の形のテスト（docs/engine.md）
// Modified by Tsukinowa Inc.: 範囲を超えた値の文字列のテストを足した（段階 116 の 2 度目のレビュー。docs/engine.md）

package types

import (
	"math"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 期待値は MySQL 8.0.46 で確かめたもの（結果の行、CONCAT）。
func TestAppendMySQLDouble(t *testing.T) {
	for _, c := range []struct {
		x     float64
		width int
		want  string
	}{
		{20250331, 0, "20250331"},
		{1234567, 0, "1234567"},
		{12345678.9, 0, "12345678.9"},
		{1e14, 0, "100000000000000"},
		{1e15, 0, "1e15"},
		{999999999999999.9, 0, "999999999999999.9"},
		{1e15 + 0.5, 0, "1000000000000000.5"},
		{1.234567890123456e15, 0, "1.234567890123456e15"},
		{1.2345678901234568e17, 0, "1.2345678901234568e17"},
		{1e-14, 0, "0.00000000000001"},
		{1e-15, 0, "0.000000000000001"},
		{1e-16, 0, "1e-16"},
		{1.5e-16, 0, "1.5e-16"},
		{0.00000015, 0, "0.00000015"},
		{0.00001, 0, "0.00001"},
		{1e100, 0, "1e100"},
		{math.MaxFloat64, 0, "1.7976931348623157e308"},
		{5e-324, 0, "5e-324"},
		{-1e15, 0, "-1e15"},
		{-0.00000015, 0, "-0.00000015"},
		{math.Copysign(0, -1), 0, "-0"},
		{0, 0, "0"},
		{0.1, 0, "0.1"},
		{1.2345678901234567e-5, 0, "0.000012345678901234568"},
		{1.2345678901234567e-5, DoubleStringWidth, "1.2345678901234568e-5"},
	} {
		if got := string(AppendMySQLDouble(nil, c.x, c.width)); got != c.want {
			t.Errorf("%v (width %d) = %s, want %s", c.x, c.width, got, c.want)
		}
	}
}

func TestAppendMySQLFloat(t *testing.T) {
	for _, c := range []struct {
		x     float32
		width int
		want  string
	}{
		{20250331, 0, "20250300"},
		{12345678.9, 0, "12345700"},
		{3.1415927, 0, "3.14159"},
		{33.333332, 0, "33.3333"},
		{1234567, 0, "1234570"},
		{1234565, 0, "1234560"},
		{0.1, 0, "0.1"},
		{1.4e-45, 0, "1.4013e-45"},
		{1e14, 0, "100000000000000"},
		{1e12, FloatStringWidth, "1e12"},
		{1e11, FloatStringWidth, "100000000000"},
	} {
		if got := string(AppendMySQLFloat(nil, c.x, c.width)); got != c.want {
			t.Errorf("%v (width %d) = %s, want %s", c.x, c.width, got, c.want)
		}
	}
}

// 結果の行の値（Type.SQL）も MySQL の形にする。
func TestFloatTypeSQLText(t *testing.T) {
	for _, c := range []struct {
		typ  sql.Type
		v    interface{}
		want string
	}{
		{Float64, 20250331.0, "20250331"},
		{Float64, 1e15, "1e15"},
		{Float64, 0.00000015, "0.00000015"},
		{Float32, float32(20250331), "20250300"},
		{Float32, float32(3.1415927), "3.14159"},
	} {
		v, err := c.typ.SQL(sql.NewEmptyContext(), nil, c.v)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.ToString(); got != c.want {
			t.Errorf("%v %v = %s, want %s", c.typ, c.v, got, c.want)
		}
	}
}

// TestAppendMySQLDoubleOutOfRange: a value beyond the DOUBLE range (SUM past the
// largest DOUBLE) is '0', as MySQL's my_gcvt shows DTOA_OVERFLOW (stage 116 second review).
func TestAppendMySQLDoubleOutOfRange(t *testing.T) {
	for _, x := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		if got := string(AppendMySQLDouble(nil, x, 0)); got != "0" {
			t.Errorf("%v = %s, want 0", x, got)
		}
		if got := string(AppendMySQLDouble(nil, x, DoubleStringWidth)); got != "0" {
			t.Errorf("%v (width) = %s, want 0", x, got)
		}
	}
}
