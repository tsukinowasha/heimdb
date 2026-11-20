// Copyright 2023 Dolthub, Inc.
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
//
// Modified by Tsukinowa Inc.: サーバのファイルを読み書きする SQL を外した（docs/engine.md）

package planbuilder

import (
	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// buildLoad は、LOAD DATA を明示的なエラーにする（HeimDB: サーバのファイルを読み書きする SQL は外した。
// sql.ErrServerFiles）。LOCAL（クライアントのファイル）も同じ。
func (b *Builder) buildLoad(inScope *scope, d *ast.Load) (outScope *scope) {
	b.handleErr(sql.ErrServerFiles)
	return nil
}
