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

package sql

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

type decimalTypeStub struct{ DecimalType }

// decimalKeyRounded は、整数のキーを値のまま decimal にして比べる（段階 115 の 2 度目のレビュー）。
func TestDecimalKeyRoundedIntegers(t *testing.T) {
	var typ Type = decimalTypeStub{}
	for _, c := range []struct {
		key  any
		conv decimal.Decimal
		want bool
	}{
		{int(12), decimal.RequireFromString("12.00"), false},
		{int8(-5), decimal.RequireFromString("-5.00"), false},
		{int16(300), decimal.RequireFromString("300"), false},
		{int32(7), decimal.RequireFromString("7.5"), true},
		{int64(math.MinInt64), decimal.RequireFromString("-9223372036854775808"), false},
		{uint(3), decimal.RequireFromString("3.00"), false},
		{uint8(255), decimal.RequireFromString("255"), false},
		{uint16(1), decimal.RequireFromString("2"), true},
		{uint32(math.MaxUint32), decimal.RequireFromString("4294967295"), false},
		{uint64(math.MaxUint64), decimal.RequireFromString("18446744073709551615"), false},
		{uint64(math.MaxUint64), decimal.RequireFromString("18446744073709551614"), true},
	} {
		if got := decimalKeyRounded(typ, c.key, c.conv); got != c.want {
			t.Errorf("decimalKeyRounded(%T %v, %s) = %v, want %v", c.key, c.key, c.conv, got, c.want)
		}
	}
}
