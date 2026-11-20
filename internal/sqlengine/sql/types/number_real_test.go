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

// HeimDB が足したテスト（段階 120。docs/engine.md）。

package types

import (
	"context"
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の列に入れる値の丸めと範囲（MySQL の Field_real::truncate）。期待の値は本物の
// MySQL 8.0 に INSERT して SELECT で確かめた表示。
func TestRealTruncate(t *testing.T) {
	f72 := CreateRealType(sqltypes.Float32, 7, 2, false)
	d103 := CreateRealType(sqltypes.Float64, 10, 3, false)
	u51 := CreateRealType(sqltypes.Float32, 5, 1, true)
	f107 := CreateRealType(sqltypes.Float32, 10, 7, false)
	fu := CreateRealType(sqltypes.Float32, 0, 0, true)
	du := CreateRealType(sqltypes.Float64, 0, 0, true)
	dec := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	tests := []struct {
		typ     sql.Type
		in      any
		want    string
		inRange bool
	}{
		{f72, dec("1.555"), "1.55", true},
		{f72, dec("12345.678"), "12345.68", true},
		{f72, dec("99999.999"), "99999.99", false},
		{f72, dec("0.125"), "0.12", true},
		{f72, dec("2.675"), "2.67", true},
		{f72, 2.675, "2.67", true},
		{f72, dec("-1"), "-1.00", true},
		{f72, "12.345x", "12.35", true},
		{d103, dec("2.0005"), "2.001", true},
		{d103, 2.0005, "2.001", true},
		{d103, dec("1.0005"), "1.000", true},
		{d103, dec("0.0005"), "0.000", true},
		{d103, dec("-0.0004"), "0.000", true},
		{d103, dec("1234567.8915"), "1234567.891", true},
		{d103, int64(99999999), "9999999.999", false},
		{d103, dec("-99999999"), "-9999999.999", false},
		{u51, dec("1.25"), "1.2", true},
		{u51, dec("3.35"), "3.4", true},
		{u51, dec("0.05"), "0.0", true},
		{u51, int64(-5), "0.0", false},
		{u51, dec("-0.04"), "0.0", false},
		{u51, dec("9999.95"), "9999.9", false},
		{f107, dec("1.23456789"), "1.2345679", true},
		{f107, dec("35.6812363"), "35.6812363", true},
		{f107, dec("1234567.5"), "1000.0000000", false},
		{f107, int64(1000), "1000.0000000", false},         // MySQL と同じく倍精度で端と比べる（段階 120 のレビュー）
		{f107, dec("999.99999999"), "1000.0000000", false}, // 丸めると 1000
		{f107, dec("-1000"), "-1000.0000000", false},
		{f107, dec("999.99999994"), "1000.0000000", true}, // 丸めると 999.9999999
		{f107, 1000.0000001, "1000.0000000", false},       // lat + 0.0000001
		{f107, float32(1000), "1000.0000000", true},       // FLOAT の列の値（保存した端）は写せる
		{fu, int64(-1), "0", false},
		{du, dec("-2"), "0", false},
		{du, dec("1e300"), "1e300", true},
	}
	ctx := sql.NewEmptyContext()
	for _, tt := range tests {
		v, inRange, err := tt.typ.Convert(context.Background(), tt.in)
		if err != nil && !truncatedString(tt.in, err) {
			t.Errorf("%s %v: %v", tt.typ, tt.in, err)
			continue
		}
		got, err := tt.typ.SQL(ctx, nil, v)
		if err != nil {
			t.Fatal(err)
		}
		if got.ToString() != tt.want || (inRange == sql.InRange) != tt.inRange {
			t.Errorf("%s %v: %s in range %v, want %s %v", tt.typ, tt.in, got.ToString(), inRange == sql.InRange, tt.want, tt.inRange)
		}
	}
	// 桁のない型と式の型は丸めない（式の型は表示だけ）
	for _, typ := range []sql.Type{Float64, Float32, RealResultType(2)} {
		v, _, err := typ.Convert(context.Background(), 1.23456)
		if f, _ := convertToFloat64(Float64.(NumberTypeImpl_), v); err != nil || f < 1.2345 {
			t.Errorf("%s rounded 1.23456 to %v (%v)", typ, v, err)
		}
	}
}

// 型の文字列は MySQL と同じで、表の定義に保存して読み直せる形（docs/plan.md の「キー設計」の表の定義。段階 120）。
func TestRealTypeString(t *testing.T) {
	for typ, want := range map[sql.Type]string{
		CreateRealType(sqltypes.Float32, 7, 2, false):  "float(7,2)",
		CreateRealType(sqltypes.Float64, 10, 3, true):  "double(10,3) unsigned",
		CreateRealType(sqltypes.Float32, 0, 0, true):   "float unsigned",
		CreateRealType(sqltypes.Float32, 30, 5, false): "float(30,5)",
		Float32: "float",
		Float64: "double",
	} {
		if got := typ.String(); got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
	if CreateRealType(sqltypes.Float32, 7, 2, false).Equals(CreateRealType(sqltypes.Float32, 8, 2, false)) || CreateRealType(sqltypes.Float32, 0, 0, true).Equals(Float32) {
		t.Error("types with other (M,D) or UNSIGNED are equal")
	}
	if !IsFloat(CreateRealType(sqltypes.Float64, 10, 3, true)) || !IsFloat(RealResultType(2)) || !IsFloat64(RealResultType(2)) {
		t.Error("IsFloat")
	}
}
