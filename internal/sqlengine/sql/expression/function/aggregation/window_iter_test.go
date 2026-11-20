// Copyright 2022 Dolthub, Inc.
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
// Modified by Tsukinowa Inc.: 整数の SUM の期待値を DECIMAL にした（MySQL と同じ。docs/engine.md）

// Modified by Tsukinowa Inc.: TestWindowIterClose を足した（上流の #3810。docs/engine.md）

package aggregation

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type closeTrackingIter struct {
	sql.RowIter
	closeCount int
	closeErr   error
}

// Close records the call and returns the configured error.
func (i *closeTrackingIter) Close(*sql.Context) error {
	i.closeCount++
	return i.closeErr
}

var (
	partitionByX = []sql.Expression{
		expression.NewGetFieldWithTable(1, 0, types.Text, "mydb", "a", "x", false),
	}
	sortByW = sql.SortFields{{
		Column: expression.NewGetFieldWithTable(0, 0, types.Int64, "mydb", "a", "w", false),
	}}
	sortByWDesc = sql.SortFields{
		{
			Column: expression.NewGetFieldWithTable(0, 0, types.Int64, "mydb", "a", "w", false),
			Order:  sql.Descending,
		},
	}
	lastX  = NewLastAgg(expression.NewGetField(1, types.Text, "x", true))
	firstY = NewFirstAgg(expression.NewGetField(2, types.Text, "y", true))
	sumZ   = NewSumAgg(expression.NewGetField(3, types.Int64, "z", true))
)

func TestWindowIter(t *testing.T) {
	tests := []struct {
		Name           string
		PartitionIters []*WindowPartitionIter
		OutputOrdinals [][]int
		Expected       []sql.Row
	}{
		{
			Name: "unbounded preceding to current row",
			PartitionIters: []*WindowPartitionIter{
				NewWindowPartitionIter(
					&WindowPartition{
						PartitionBy: partitionByX,
						SortBy:      sortByW,
						Aggs: []*Aggregation{
							NewAggregation(lastX, NewUnboundedPrecedingToCurrentRowFramer()),
							NewAggregation(sumZ, NewUnboundedPrecedingToCurrentRowFramer()),
						},
					}),
				NewWindowPartitionIter(
					&WindowPartition{
						PartitionBy: partitionByX,
						SortBy:      sortByWDesc,
						Aggs: []*Aggregation{
							NewAggregation(firstY, NewUnboundedPrecedingToCurrentRowFramer()),
						},
					}),
			},
			OutputOrdinals: [][]int{{0, 1}, {2}},
			Expected: []sql.Row{
				{"forest", decimal.NewFromInt(4), "wildflower"},
				{"forest", decimal.NewFromInt(8), "wildflower"},
				{"forest", decimal.NewFromInt(14), "wildflower"},
				{"forest", decimal.NewFromInt(17), "wildflower"},
				{"forest", decimal.NewFromInt(27), "wildflower"},
				{"desert", decimal.NewFromInt(4), "mummy"},
				{"desert", decimal.NewFromInt(10), "mummy"},
				{"desert", decimal.NewFromInt(18), "mummy"},
				{"desert", decimal.NewFromInt(23), "mummy"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			db := memory.NewDatabase("test")
			pro := memory.NewDBProvider(db)
			ctx := sql.NewContext(context.Background(), sql.WithSession(memory.NewSession(sql.NewBaseSession(), pro)))

			i := NewWindowIter(tt.PartitionIters, tt.OutputOrdinals, mustNewRowIter(t, db, ctx))
			res, err := sql.RowIterToRows(ctx, i)
			require.NoError(t, err)
			require.Equal(t, tt.Expected, res)
		})
	}
}

// TestWindowIterClose verifies that closing a window iterator releases its child and partitions.
func TestWindowIterClose(t *testing.T) {
	ctx := sql.NewEmptyContext()
	closeErr := errors.New("close error")
	child := &closeTrackingIter{closeErr: closeErr}
	partition := NewWindowPartitionIter(&WindowPartition{})
	partition.input = sql.WindowBuffer{{1}}

	iter := NewWindowIter([]*WindowPartitionIter{partition}, [][]int{{0}}, child)
	require.ErrorIs(t, iter.Close(ctx), closeErr)
	require.Equal(t, 1, child.closeCount)
	require.Nil(t, partition.input)
}
