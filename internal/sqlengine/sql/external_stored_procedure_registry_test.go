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

// 引数の数が合う登録がなければ、引数が最も多い登録を返す。引数のない登録しかなくても、ゼロ値ではなくその登録を返す。
func TestLookupByNameAndParamCountFallback(t *testing.T) {
	reg := NewExternalStoredProcedureRegistry()
	reg.Register(ExternalStoredProcedureDetails{Name: "noargs", Function: func(*Context) (RowIter, error) { return nil, nil }})
	reg.Register(ExternalStoredProcedureDetails{Name: "twoargs", Function: func(*Context, string, string) (RowIter, error) { return nil, nil }})
	reg.Register(ExternalStoredProcedureDetails{Name: "twoargs", Function: func(*Context) (RowIter, error) { return nil, nil }})

	for _, tc := range []struct {
		name   string
		params int
		want   int
	}{
		{"noargs", 0, 0},
		{"noargs", 1, 0},
		{"twoargs", 0, 0},
		{"twoargs", 1, 2},
		{"twoargs", 3, 2},
	} {
		proc, err := reg.LookupByNameAndParamCount(tc.name, tc.params)
		if err != nil {
			t.Fatalf("%s(%d): %v", tc.name, tc.params, err)
		}
		if proc == nil || proc.Function == nil {
			t.Errorf("%s(%d): got %+v, want the registration with %d params", tc.name, tc.params, proc, tc.want)
			continue
		}
		if got := reg.countNumberOfParams(*proc); got != tc.want {
			t.Errorf("%s(%d): got the registration with %d params, want %d", tc.name, tc.params, got, tc.want)
		}
	}
	if proc, err := reg.LookupByNameAndParamCount("missing", 0); proc != nil || err != nil {
		t.Errorf("missing: got %v, %v", proc, err)
	}
}
