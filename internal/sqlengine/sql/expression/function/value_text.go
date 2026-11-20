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

// Modified by Tsukinowa Inc.: 文字列の関数の引数の ENUM、SET、日時の値を値の文字列にする valueText を足した（段階 116 の 3 度目のレビュー。docs/engine.md）

package function

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// valueText は、引数 e の値 v が ENUM か SET の値（番号かビット）、日付、日時、TIME なら、MySQL と同じく文字列の文脈の
// 値の文字列（types.ValueString。CONCAT と同じ）にし、ほかの値はそのまま返す。前は FIELD、STRCMP、REGEXP などが
// ENUM の値を番号の文字列（'1'）にした（段階 116 の 3 度目のレビュー）。
func valueText(e sql.Expression, v interface{}) interface{} {
	if e == nil {
		return v
	}
	if s, ok := types.ValueString(e.Type(), v); ok {
		return s
	}
	return v
}
