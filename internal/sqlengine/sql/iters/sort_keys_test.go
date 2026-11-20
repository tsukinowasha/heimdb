// Copyright 2026 Tsukinowa Inc.

package iters

import (
	"fmt"
	"io"
	"math/rand"
	"sort"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func drain(t *testing.T, it sql.RowIter) []sql.Row {
	t.Helper()
	ctx := sql.NewEmptyContext()
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
	return out
}

// 副問い合わせの中の行は外の行を前に持つ（外の行 || 子の行）。TopN の足したキーと順番の列の位置は、子のスキーマの長さ
// ではなく実際の行の長さから決める（上流の #3626 の問題）。memory の表の childSchemaLen の 0 も同じ。
func TestTopRowsWithOuterRowPrefix(t *testing.T) {
	// 行は (外の列, a, b)。キーは a+0（列の参照でないので先に計算する）、同順位は来た順
	var rows []sql.Row
	for i, a := range []int64{3, 1, 2, 1, 3, 0, 2} {
		rows = append(rows, sql.Row{"outer", a, int64(i)})
	}
	key := expression.NewArithmetic(expression.NewGetField(1, types.Int64, "a", false), expression.NewLiteral(int64(0), types.Int64), "+")
	for _, childSchemaLen := range []int{2, 0} {
		for _, desc := range []bool{false, true} {
			order := sql.Ascending
			if desc {
				order = sql.Descending
			}
			in := make([]sql.Row, len(rows))
			for i, r := range rows {
				in[i] = append(sql.Row{}, r...)
			}
			got := drain(t, NewTopRowsIter(sql.SortFields{{Column: key, Order: order}}, 4, false, sql.RowsToRowIter(in...), childSchemaLen))
			want := append([]sql.Row(nil), rows...)
			sort.SliceStable(want, func(i, j int) bool {
				return (want[i][1].(int64) < want[j][1].(int64)) != desc && want[i][1] != want[j][1]
			})
			want = want[:4]
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("childSchemaLen %d desc %v:\n got %v\nwant %v", childSchemaLen, desc, got, want)
			}
		}
	}
}

// TopN でない並べ替えも、列の参照でない式は 1 行に 1 回だけ評価し（比べるたびに評価すると ORDER BY RAND() が偏る。
// 上流の #3733 の問題）、足したキーは返す前に外す。結果は安定に並べたものと同じ。
func TestSortIterPrecomputedKeys(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	var rows []sql.Row
	for i := 0; i < 300; i++ {
		var a interface{} = int64(rnd.Intn(5))
		if rnd.Intn(7) == 0 {
			a = nil
		}
		rows = append(rows, sql.Row{"outer", a, fmt.Sprint(i)})
	}
	for _, desc := range []bool{false, true} {
		order := sql.Ascending
		if desc {
			order = sql.Descending
		}
		n := 0
		a := countingExpr{expression.NewGetField(1, types.Int64, "a", true), &n}
		in := make([]sql.Row, len(rows))
		for i, r := range rows {
			in[i] = append(sql.Row{}, r...)
		}
		got := drain(t, NewSortIter(sql.SortFields{{Column: a, Order: order, NullOrdering: sql.NullsFirst}}, sql.RowsToRowIter(in...)))
		want := append([]sql.Row(nil), rows...)
		sort.SliceStable(want, func(i, j int) bool {
			x, y := want[i][1], want[j][1]
			if x == y {
				return false
			}
			if x == nil || y == nil {
				return (x == nil) != desc
			}
			return (x.(int64) < y.(int64)) != desc
		})
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("desc %v:\n got %v\nwant %v", desc, got, want)
		}
		if n != len(rows) {
			t.Errorf("desc %v: evaluated the sort key %d times for %d rows", desc, n, len(rows))
		}
	}
}

// ORDER BY RAND() は、1 行に 1 回だけ乱数を引くので、先頭に来る行が偏らない。
func TestSortIterRandomKeyIsUniform(t *testing.T) {
	const rowsN, runs = 8, 4000
	counts := make([]int, rowsN)
	for r := 0; r < runs; r++ {
		var rows []sql.Row
		for i := 0; i < rowsN; i++ {
			rows = append(rows, sql.Row{int64(i)})
		}
		key := randExpr{rand.New(rand.NewSource(int64(r)))}
		got := drain(t, NewSortIter(sql.SortFields{{Column: key, Order: sql.Ascending}}, sql.RowsToRowIter(rows...)))
		if len(got) != rowsN || len(got[0]) != 1 {
			t.Fatalf("got %v", got)
		}
		counts[got[0][0].(int64)]++
	}
	for i, c := range counts {
		// 一様なら 1 行あたり 500 回。前の形（比べるたびに評価）では先頭の行が 1,000 回を超え、後ろの行は 100 回を下回った
		if c < 380 || c > 620 {
			t.Errorf("row %d came first %d times of %d: %v", i, c, runs, counts)
		}
	}
}

// randExpr は、評価するたびに乱数を返す式（RAND() の代わり）。
type randExpr struct{ r *rand.Rand }

func (e randExpr) Resolved() bool                                         { return true }
func (e randExpr) String() string                                         { return "rand()" }
func (e randExpr) Type() sql.Type                                         { return types.Float64 }
func (e randExpr) IsNullable() bool                                       { return false }
func (e randExpr) Children() []sql.Expression                             { return nil }
func (e randExpr) Eval(*sql.Context, sql.Row) (interface{}, error)        { return e.r.Float64(), nil }
func (e randExpr) WithChildren(...sql.Expression) (sql.Expression, error) { return e, nil }

// rowLenExpr は、評価した行の長さを覚える式（副問い合わせのキーは、行の長さで外の列の位置を決める）。
type rowLenExpr struct {
	randExpr
	lens map[int]bool
}

func (e rowLenExpr) Eval(_ *sql.Context, row sql.Row) (interface{}, error) {
	e.lens[len(row)] = true
	return row[1], nil
}

// 並べ替えのキーは、順番の列やほかのキーを足す前の子の行で評価する（上流の #3626。ORDER BY (SELECT ...) の副問い合わせ
// は、渡された行を外の行として読む）。
func TestSortKeysSeeChildRow(t *testing.T) {
	var rows []sql.Row
	for i, a := range []int64{3, 1, 2} {
		rows = append(rows, sql.Row{"outer", a, int64(i)})
	}
	for _, limit := range []int64{-1, 2} {
		e := rowLenExpr{lens: map[int]bool{}}
		var it sql.RowIter
		if limit < 0 {
			it = NewSortIter(sql.SortFields{{Column: e, Order: sql.Ascending}, {Column: e, Order: sql.Ascending}}, sql.RowsToRowIter(rows...))
		} else {
			it = NewTopRowsIter(sql.SortFields{{Column: e, Order: sql.Ascending}, {Column: e, Order: sql.Ascending}}, limit, false, sql.RowsToRowIter(rows...), 3)
		}
		got := drain(t, it)
		if len(got) == 0 || got[0][1] != int64(1) || len(got[0]) != 3 {
			t.Errorf("limit %d: got %v", limit, got)
		}
		if len(e.lens) != 1 || !e.lens[3] {
			t.Errorf("limit %d: evaluated the key on rows of lengths %v, want only 3", limit, e.lens)
		}
	}
}
