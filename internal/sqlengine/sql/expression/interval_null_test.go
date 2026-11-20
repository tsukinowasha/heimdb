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

package expression

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 日時と NULL の INTERVAL の足し算と引き算は NULL（MySQL と同じ）。型付きの nil で plus と minus が panic していた
// （docs/plan.md の段階 105）。
func TestArithmeticNullInterval(t *testing.T) {
	ctx := sql.NewEmptyContext()
	date := NewLiteral("2024-01-01", types.LongText)
	null := func() *Interval { return NewInterval(NewLiteral(nil, types.Null), "DAY") }
	for name, e := range map[string]*Arithmetic{
		"date + interval": NewPlus(date, null()),
		"interval + date": NewPlus(null(), date),
		"date - interval": NewMinus(date, null()),
	} {
		v, err := e.Eval(ctx, nil)
		if err != nil || v != nil {
			t.Errorf("%s: %v %v", name, v, err)
		}
	}
	v, err := NewPlus(date, NewInterval(NewLiteral(int64(1), types.Int64), "DAY")).Eval(ctx, nil)
	if err != nil || v == nil {
		t.Errorf("date + 1 day: %v %v", v, err)
	}
}
