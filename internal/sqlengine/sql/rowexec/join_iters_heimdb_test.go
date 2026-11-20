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

// Modified by Tsukinowa Inc.: existsIter を閉じるとき、左の失敗でも右を閉じることのテスト（段階 111。docs/engine.md）
// Modified by Tsukinowa Inc.: existsIter の右が空のときのテスト（段階 112、上流の #3211 と #3361。docs/engine.md）

package rowexec

import (
	"errors"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type closeCountIter struct {
	sql.RowIter
	closed int
	err    error
}

func (i *closeCountIter) Close(*sql.Context) error {
	i.closed++
	return i.err
}

// 左の iterator を閉じるのに失敗しても、右の iterator（GROUP BY などのキャッシュを持ちうる）を閉じる（上流の #3428 の
// 漏れの直しの続き）。
func TestExistsIterCloseRightOnLeftError(t *testing.T) {
	ctx := sql.NewEmptyContext()
	left := &closeCountIter{err: errors.New("left")}
	right := &closeCountIter{}
	it := &existsIter{primary: left, rIter: right}
	if err := it.Close(ctx); err == nil {
		t.Error("the left error was lost")
	}
	if right.closed != 1 || left.closed != 1 {
		t.Errorf("closed: left %d, right %d", left.closed, right.closed)
	}
	if err := it.Close(ctx); err == nil || right.closed != 1 {
		t.Errorf("second close: %v, right closed %d times", err, right.closed)
	}
}

// emptyRightBuilder は、右の行を作るたびに、空の印の iterator（plan.EmptyIter。結果のキャッシュが空で終わった
// CachedResults などが返す）を返す。
type emptyRightBuilder struct{}

func (emptyRightBuilder) Build(*sql.Context, sql.Node, sql.Row) (sql.RowIter, error) {
	return plan.EmptyIter, nil
}

// 右が空の印の iterator なら、半結合（EXISTS）はどの左の行も返さず、反結合（NOT IN と NOT EXISTS）はすべての左の行を
// 返す。左の列が NULL でも、条件が NULL を拒んでも同じ（MySQL の `NULL NOT IN (空)` は真）。前は反結合で iterator
// 全体を終え（左の行を 1 つも返さない）、上流の #3211 だけでは NULL を拒む条件の反結合が、右を NULL として比べて
// 左の行を落とした（#3361 で直った）。
func TestExistsIterEmptyRight(t *testing.T) {
	ctx := sql.NewEmptyContext()
	cond := expression.NewEquals(
		expression.NewGetField(0, types.Int64, "y", true),
		expression.NewGetField(1, types.Int64, "b", true),
	)
	for _, c := range []struct {
		typ  plan.JoinType
		want int
	}{
		{plan.JoinTypeSemi, 0},
		{plan.JoinTypeAnti, 3},
		{plan.JoinTypeAntiIncludeNulls, 3},
	} {
		it := &existsIter{
			typ:     c.typ,
			primary: sql.RowsToRowIter(sql.Row{int64(1)}, sql.Row{nil}, sql.Row{int64(2)}),
			cond:    cond,
			rowSize: 2,
			b:       emptyRightBuilder{},
		}
		rows, err := sql.RowIterToRows(ctx, it)
		if err != nil {
			t.Fatalf("%s: %v", c.typ, err)
		}
		if len(rows) != c.want {
			t.Errorf("%s: %d rows %v, want %d", c.typ, len(rows), rows, c.want)
		}
	}
}
