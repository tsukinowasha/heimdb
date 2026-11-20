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

package planbuilder

import (
	"testing"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"
)

// 名前の空のシステム変数は、vitess の VarScope が panic する前にエラーにする（docs/plan.md の段階 105）。
func TestSysVarScopeEmptyName(t *testing.T) {
	for _, parts := range [][]string{{"@@"}, {"@@session."}, {"@@global."}, {"@@session", ""}, {"@@.x"}} {
		if _, _, _, err := sysVarScope(parts...); err == nil {
			t.Errorf("%q: no error", parts)
		}
	}
	for _, c := range []struct {
		parts []string
		name  string
		scope ast.SetScope
	}{
		{[]string{"@@sql_mode"}, "sql_mode", ast.SetScope_Session},
		{[]string{"@@global.sql_mode"}, "sql_mode", ast.SetScope_Global},
		{[]string{"@@session", "time_zone"}, "time_zone", ast.SetScope_Session},
		{[]string{"@x"}, "x", ast.SetScope_User},
		{[]string{"@"}, "", ast.SetScope_User},
		{[]string{"col"}, "col", ast.SetScope_None},
	} {
		name, scope, _, err := sysVarScope(c.parts...)
		if err != nil || name != c.name || scope != c.scope {
			t.Errorf("%q: %q %q %v", c.parts, name, scope, err)
		}
	}
}
