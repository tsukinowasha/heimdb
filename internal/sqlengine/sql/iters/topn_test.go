// Copyright 2026 Tsukinowa Inc.

package iters

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// countingExpr は、評価した回数を数える式（列の参照ではないので、TopN は先に計算する）。
type countingExpr struct {
	sql.Expression
	n *int
}

func (e countingExpr) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	*e.n++
	return e.Expression.Eval(ctx, row)
}

type baseType = sql.Type

// countingType は、比べた回数を数える型。
type countingType struct {
	baseType
	n *int
}

func (t countingType) Compare(ctx context.Context, a, b interface{}) (int, error) {
	*t.n++
	return t.baseType.Compare(ctx, a, b)
}

func topN(t *testing.T, fields sql.SortFields, limit int64, rows []sql.Row) ([]sql.Row, int64) {
	t.Helper()
	ctx := sql.NewEmptyContext()
	in := make([]sql.Row, len(rows))
	for i, r := range rows {
		in[i] = append(sql.Row{}, r...)
	}
	it := NewTopRowsIter(fields, limit, true, sql.RowsToRowIter(in...), 2)
	var out []sql.Row
	for {
		r, err := it.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := it.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return out, it.numFoundRows
}

// TopN の結果は、すべての行を安定に並べた先頭と同じ（同順位は来た順。NULL、昇順と降順、2 つのキー）。列の参照でない
// 並べ替えの式は 1 行に 1 回だけ評価する（HeimDB の段階 96）。
func TestTopRowsPrecomputedKeys(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	var rows []sql.Row
	for i := 0; i < 500; i++ {
		var a interface{} = int64(rnd.Intn(5))
		if rnd.Intn(7) == 0 {
			a = nil
		}
		rows = append(rows, sql.Row{a, fmt.Sprint(rnd.Intn(3))})
	}
	for _, desc := range []bool{false, true} {
		for _, limit := range []int64{0, 1, 20, 40, 499, 500, 600} {
			n := 0
			order := sql.Ascending
			if desc {
				order = sql.Descending
			}
			a := countingExpr{expression.NewGetField(0, types.Int64, "a", true), &n}
			b := expression.NewGetField(1, types.LongText, "b", false)
			fields := sql.SortFields{{Column: a, Order: order, NullOrdering: sql.NullsFirst}, {Column: b, Column2: b, Order: sql.Descending}}
			got, found := topN(t, fields, limit, rows)

			want := append([]sql.Row(nil), rows...)
			sort.SliceStable(want, func(i, j int) bool {
				x, y := want[i], want[j]
				if x[0] != y[0] {
					if x[0] == nil || y[0] == nil {
						return (x[0] == nil) != desc
					}
					return (x[0].(int64) < y[0].(int64)) != desc
				}
				return x[1].(string) > y[1].(string)
			})
			if int64(len(want)) > limit {
				want = want[:limit]
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("desc %v limit %d:\n got %v\nwant %v", desc, limit, got, want)
			}
			if found != int64(len(rows)) {
				t.Errorf("found rows %d", found)
			}
			if n != len(rows) {
				t.Errorf("desc %v limit %d: evaluated the sort key %d times for %d rows", desc, limit, n, len(rows))
			}
		}
	}
}

// ヒープが LIMIT の数に達したら、新しい行を頂点と 1 回だけ比べ、残らない行は入れない（HeimDB の段階 96）。
func TestTopRowsRejectsWithOneComparison(t *testing.T) {
	var rows []sql.Row
	for i := 0; i < 1000; i++ {
		rows = append(rows, sql.Row{int64(i), "x"})
	}
	n := 0
	a := expression.NewGetField(0, countingType{types.Int64, &n}, "a", false)
	got, _ := topN(t, sql.SortFields{{Column: a, Column2: a, Order: sql.Ascending}}, 10, rows)
	if len(got) != 10 || got[0][0] != int64(0) || got[9][0] != int64(9) {
		t.Fatalf("got %v", got)
	}
	if n > 1100 {
		t.Errorf("compared %d times for 1000 rows", n)
	}
}

// 並べ替えの式のエラーは ErrUnableSort で返す。
func TestTopRowsKeyError(t *testing.T) {
	ctx := sql.NewEmptyContext()
	bad := expression.NewConvert(expression.NewGetField(0, types.LongText, "a", false), expression.ConvertToJSON)
	it := NewTopRowsIter(sql.SortFields{{Column: bad, Order: sql.Ascending}}, 1, false, sql.RowsToRowIter(sql.Row{"{", "x"}, sql.Row{"[", "y"}), 2)
	if _, err := it.Next(ctx); !sql.ErrUnableSort.Is(err) {
		t.Errorf("got %v", err)
	}
}
