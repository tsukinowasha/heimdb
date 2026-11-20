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

// Modified by Tsukinowa Inc.: 表ごとの列の一覧を小文字の表名で引くことのテスト（段階 111。docs/engine.md）

package plan

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 表ごとの列の一覧は、更新する表の一覧（小文字の表名）と同じ名前で引ける。前は書いたとおりの名前で、大文字を含む表の
// 列が見つからず、変わっていない表の行も書き直していた。
func TestRecreateTableSchemaLowerCase(t *testing.T) {
	sch := sql.Schema{
		{Name: "ItemID", Source: "Items", Type: types.Int64},
		{Name: "Version", Source: "Items", Type: types.Int64},
		{Name: "ItemID", Source: "Items2", Type: types.Int64},
	}
	m := RecreateTableSchemaFromJoinSchema(sch)
	if len(m["items"]) != 2 || len(m["items2"]) != 1 {
		t.Errorf("schemas: %v", m)
	}
	rows := SplitRowIntoTableRowMap(sql.Row{1, 2, 1}, sch)
	if len(rows["items"]) != 2 || len(rows["items2"]) != 1 {
		t.Errorf("rows: %v", rows)
	}
}
