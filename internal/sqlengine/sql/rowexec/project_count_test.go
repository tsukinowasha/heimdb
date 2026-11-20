// Copyright 2026 Tsukinowa Inc.

package rowexec

import (
	"io"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression/function"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type countingRows struct{ n int }

func (i *countingRows) Next(*sql.Context) (sql.Row, error)          { return nil, io.EOF }
func (i *countingRows) Close(*sql.Context) error                    { return nil }
func (i *countingRows) CountRest(*sql.Context) (int64, bool, error) { return int64(i.n), true, nil }

// 列（と別名）を選ぶだけの Project は子の RowCounter を中継し、式があれば中継しない（HeimDB の段階 97）。
func TestProjectCountsRest(t *testing.T) {
	col := expression.NewGetField(0, types.Int64, "id", false)
	for _, c := range []struct {
		projs []sql.Expression
		ok    bool
	}{
		{[]sql.Expression{col}, true},
		{[]sql.Expression{expression.NewAlias("ID", col)}, true},
		{[]sql.Expression{col, function.NewLower(col)}, false},
		{[]sql.Expression{expression.NewAlias("x", function.NewLower(col))}, false},
	} {
		it := &ProjectIter{projs: c.projs, childIter: &countingRows{n: 7}}
		n, ok, err := it.CountRest(sql.NewEmptyContext())
		if err != nil || ok != c.ok || (ok && n != 7) {
			t.Errorf("%v: %d, %v, %v", c.projs, n, ok, err)
		}
	}
	it := &ProjectIter{projs: []sql.Expression{col}, childIter: sql.RowsToRowIter()}
	if _, ok, _ := it.CountRest(sql.NewEmptyContext()); ok {
		t.Error("a child without RowCounter was counted")
	}
}
