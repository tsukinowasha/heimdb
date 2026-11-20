// Copyright 2026 Tsukinowa Inc.

package memo

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/stats"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// JOIN の出力の行数は、子に値の種類の数がなければ「小さい方の子の行数 ÷ 0.8」で、JoinKeyDistinct がそれより多い値の
// 種類の数を返せば、それで割る（1 行未満にはしない）。少ない数は使わない（既定の 0 では元のまま）。子が値の種類の数を
// 持つときも、多い方を使う。
func TestJoinKeyDistinct(t *testing.T) {
	m := NewMemo(nil, nil, nil, 0, nil, nil)
	scan := func(id sql.TableId, rows uint64) *ExprGroup {
		g := newExprGroup(m, GroupId(id), &TableScan{
			sourceBase: &sourceBase{relBase: &relBase{}},
			Table: plan.NewResolvedTable(&dummyTable{schema: sql.NewPrimaryKeySchema(sql.Schema{
				{Name: "x", Source: "t", Type: types.Int64},
			})}, nil, nil).WithId(id).WithColumns(sql.NewColSet(sql.ColumnId(id))),
		})
		g.RelProps.SetStats(&stats.Statistic{RowCnt: rows})
		return g
	}
	join := &InnerJoin{JoinBase: &JoinBase{relBase: &relBase{}, Op: plan.JoinTypeInner, Left: scan(1, 200), Right: scan(2, 2000)}}
	orig := JoinKeyDistinct
	defer func() { JoinKeyDistinct = orig }()
	for _, tt := range []struct {
		distinct float64
		want     uint64
	}{{0, 250}, {10, 250}, {20000, 20}, {1e7, 1}} {
		var got JoinRel
		JoinKeyDistinct = func(ctx *sql.Context, rel JoinRel) float64 { got = rel; return tt.distinct }
		require.Equal(t, tt.want, statsForRel(sql.NewEmptyContext(), join).RowCount(), "distinct %v", tt.distinct)
		require.Equal(t, JoinRel(join), got)
	}

	// 子が値の種類の数を持つ（ヒストグラムのある IndexScan など）: 元は 200 × 2000 ÷ 500
	join.Right.RelProps.SetStats(&stats.Statistic{RowCnt: 2000, DistinctCnt: 500})
	JoinKeyDistinct = func(ctx *sql.Context, rel JoinRel) float64 { return 0 }
	require.Equal(t, uint64(800), statsForRel(sql.NewEmptyContext(), join).RowCount())
	JoinKeyDistinct = func(ctx *sql.Context, rel JoinRel) float64 { return 4000 }
	require.Equal(t, uint64(100), statsForRel(sql.NewEmptyContext(), join).RowCount())

	// 子が空なら 0 行のまま
	join.Left.RelProps.SetStats(&stats.Statistic{RowCnt: 0})
	require.Equal(t, uint64(0), statsForRel(sql.NewEmptyContext(), join).RowCount())
}
