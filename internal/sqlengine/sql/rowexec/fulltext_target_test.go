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

package rowexec

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// noFulltextDatabase は、FULLTEXT に対応しない（fulltext.Database でない）データベース。
type noFulltextDatabase struct{ sql.Database }

// FULLTEXT に対応しない表やデータベースでは、panic せずにエラーを返す。
func TestFulltextTarget(t *testing.T) {
	db := memory.NewDatabase("db")
	tbl := memory.NewTable(db, "t", sql.PrimaryKeySchema{}, nil)

	if _, _, err := fulltextTarget(tbl, db); err != nil {
		t.Errorf("memory table and database: %v", err)
	}
	if _, _, err := fulltextTarget(tbl, noFulltextDatabase{db}); err == nil {
		t.Errorf("a database without FULLTEXT support was accepted")
	}
	if _, _, err := fulltextTarget(struct{ sql.Table }{tbl}, db); err == nil {
		t.Errorf("a table that is not index addressable was accepted")
	}
}
