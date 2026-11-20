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

package function

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// collatedText は、文字列の引数の照合順序を持つ型（fallback の長さのまま）。MySQL と同じく、文字列の関数の結果は
// 引数の照合順序になる（LEFT(col, 1) は列の照合順序）。もとの型は照合順序が既定（utf8mb4_0900_bin）で、式の
// ORDER BY、GROUP BY、DISTINCT が 'a' と 'A' を別にしていた。照合順序は、Type() に ctx がないので、引数の型から
// CollationCoercibility と同じ規則で決める（列は 2、リテラルは 4）。文字列の引数がないか、バイナリになるときは
// fallback のまま。
func collatedText(fallback sql.StringType, args ...sql.Expression) sql.Type {
	collation, coercibility := sql.Collation_binary, byte(6)
	found := false
	for _, arg := range args {
		if arg == nil {
			continue
		}
		st, ok := arg.Type().(sql.StringType)
		if !ok {
			continue
		}
		if st.Collation() == sql.Collation_binary {
			// バイナリの引数があれば、結果もバイナリ（MySQL と同じ。ResolveCoercibility は Unicode を選ぶ）
			return fallback
		}
		c := byte(2)
		if _, ok := arg.(*expression.Literal); ok {
			c = 4
		}
		if !found {
			collation, coercibility, found = st.Collation(), c, true
			continue
		}
		collation, coercibility = sql.ResolveCoercibility(collation, coercibility, st.Collation(), c)
	}
	if !found || collation == sql.Collation_binary || collation == fallback.Collation() {
		return fallback
	}
	return types.MustCreateString(fallback.Type(), fallback.MaxCharacterLength(), collation)
}

// argStringType は、文字列の引数の型（LEFT のように、結果が引数より長くならない関数の結果の型）。引数が文字列で
// なければ LONGTEXT。
func argStringType(arg sql.Expression) sql.Type {
	if t, ok := arg.Type().(sql.StringType); ok {
		return t
	}
	return types.LongText
}
