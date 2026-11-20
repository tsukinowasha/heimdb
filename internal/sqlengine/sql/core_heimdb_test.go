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

// Modified by Tsukinowa Inc.: 真偽にするときに TEXT の列かを調べるのは警告を出すときだけか（段階 122。docs/engine.md）

package sql

import "testing"

// TestStage122ConvertToBoolTextColumnOnlyToWarn は、EvaluateCondition と ConvertToBoolFor が、式が TEXT の列の
// 参照か（IsTextColumnRef）を、数として読めない文字列で警告を出すかを決めるときだけ調べるかを確かめる。真偽や
// 数の値（比べの結果など）では調べない（段階 122。前は行ごとに調べ、全件を読む WHERE が遅くなった）。
func TestStage122ConvertToBoolTextColumnOnlyToWarn(t *testing.T) {
	saved := IsTextColumnRef
	defer func() { IsTextColumnRef = saved }()
	calls := 0
	quiet := false
	IsTextColumnRef = func(Expression) bool { calls++; return quiet }

	// 式の値は使わない（EvaluateCondition は Eval を呼ぶので、値を返す式を渡す）
	cases := []struct {
		v         interface{}
		want      bool
		calls     int // IsTextColumnRef を呼ぶ回数
		warnLoud  int // TEXT の列でないときの警告の数
		warnQuiet int // TEXT の列のときの警告の数
	}{
		{true, true, 0, 0, 0},
		{false, false, 0, 0, 0},
		{int64(0), false, 0, 0, 0},
		{int8(1), true, 0, 0, 0},
		{float64(0.5), true, 0, 0, 0},
		{"", false, 0, 0, 0},
		{"1", true, 0, 0, 0},
		{" 2 ", true, 0, 0, 0},
		{"3bxu", true, 1, 1, 0},
		{"abc", false, 1, 1, 0},
		{[]byte("x1"), false, 1, 1, 0},
	}
	for _, c := range cases {
		for _, q := range []bool{false, true} {
			quiet = q
			wantWarn := c.warnLoud
			if q {
				wantWarn = c.warnQuiet
			}

			calls = 0
			ctx := NewEmptyContext()
			got, err := EvaluateCondition(ctx, valueExpr{c.v}, nil)
			if err != nil || got != c.want {
				t.Errorf("EvaluateCondition(%#v) = %v, %v; want %v", c.v, got, err, c.want)
			}
			if calls != c.calls {
				t.Errorf("EvaluateCondition(%#v): IsTextColumnRef called %d times, want %d", c.v, calls, c.calls)
			}
			if n := int(ctx.WarningCount()); n != wantWarn {
				t.Errorf("EvaluateCondition(%#v) quiet=%v: %d warnings, want %d", c.v, q, n, wantWarn)
			}

			calls = 0
			ctx = NewEmptyContext()
			b, err := ConvertToBoolFor(ctx, valueExpr{c.v}, c.v)
			if err != nil || b != c.want {
				t.Errorf("ConvertToBoolFor(%#v) = %v, %v; want %v", c.v, b, err, c.want)
			}
			if calls != c.calls {
				t.Errorf("ConvertToBoolFor(%#v): IsTextColumnRef called %d times, want %d", c.v, calls, c.calls)
			}
			if n := int(ctx.WarningCount()); n != wantWarn {
				t.Errorf("ConvertToBoolFor(%#v) quiet=%v: %d warnings, want %d", c.v, q, n, wantWarn)
			}

			// ConvertToBool は TEXT の列を調べず、いつも警告を出す
			calls = 0
			ctx = NewEmptyContext()
			if b, err := ConvertToBool(ctx, c.v); err != nil || b != c.want || calls != 0 || int(ctx.WarningCount()) != c.warnLoud {
				t.Errorf("ConvertToBool(%#v) = %v, %v with %d calls and %d warnings", c.v, b, err, calls, ctx.WarningCount())
			}
		}
	}
}

// valueExpr は、いつも v を返す式（テスト用）。
type valueExpr struct{ v interface{} }

func (e valueExpr) Resolved() bool                                 { return true }
func (e valueExpr) String() string                                 { return "value" }
func (e valueExpr) Type() Type                                     { return nil }
func (e valueExpr) IsNullable() bool                               { return false }
func (e valueExpr) Eval(*Context, Row) (interface{}, error)        { return e.v, nil }
func (e valueExpr) Children() []Expression                         { return nil }
func (e valueExpr) WithChildren(...Expression) (Expression, error) { return e, nil }
