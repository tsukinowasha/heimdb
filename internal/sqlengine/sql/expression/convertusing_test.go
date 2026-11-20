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

package expression

import (
	"errors"
	"testing"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 変換器のない文字セットへの変換は、panic せずにエラー 1235 を返す。
func TestConvertUsingWithoutEncoder(t *testing.T) {
	ctx := sql.NewEmptyContext()
	for _, cs := range []sql.CharacterSetID{sql.CharacterSet_ujis, sql.CharacterSet_koi8r} {
		_, err := NewConvertUsing(NewLiteral("abc", types.LongText), cs).Eval(ctx, nil)
		var sqlErr *mysql.SQLError
		if !errors.As(err, &sqlErr) || sqlErr.Num != mysql.ERNotSupportedYet {
			t.Errorf("%s: err = %v, want error %d", cs.Name(), err, mysql.ERNotSupportedYet)
		}
	}
	got, err := NewConvertUsing(NewLiteral("abc", types.LongText), sql.CharacterSet_latin1).Eval(ctx, nil)
	if err != nil || got != "abc" {
		t.Errorf("latin1: got %v, %v", got, err)
	}
}
