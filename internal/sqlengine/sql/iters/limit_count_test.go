// Copyright 2026 Tsukinowa Inc.

package iters

import (
	"io"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// counterIter は、n 行を返す RowIter。can なら、残りを RowCounter で数える。
type counterIter struct {
	n, pos, nexts, counts int
	can                   bool
}

func (i *counterIter) Next(*sql.Context) (sql.Row, error) {
	i.nexts++
	if i.pos >= i.n {
		return nil, io.EOF
	}
	i.pos++
	return sql.Row{i.pos}, nil
}

func (i *counterIter) Close(*sql.Context) error { return nil }

func (i *counterIter) CountRest(*sql.Context) (int64, bool, error) {
	i.counts++
	if !i.can {
		return 0, false, nil
	}
	n := i.n - i.pos
	i.pos = i.n
	return int64(n), true, nil
}

// SQL_CALC_FOUND_ROWS の LIMIT は、子が RowCounter なら残りを数えさせる（HeimDB の段階 97）。数えられないなら、今までどおり
// Next で数える。数えた後の Next は、子を読まずに io.EOF を返す。
func TestLimitCountsRest(t *testing.T) {
	for _, c := range []struct {
		rows, limit int64
		can         bool
		nexts       int // 子の Next を呼ぶ回数
	}{
		{100, 20, true, 20},
		{100, 0, true, 0},
		{10, 20, true, 11},
		{20, 20, true, 20},
		{100, 20, false, 101},
	} {
		ctx := sql.NewEmptyContext()
		child := &counterIter{n: int(c.rows), can: c.can}
		li := &LimitIter{CalcFoundRows: true, Limit: c.limit, ChildIter: child}
		var got int64
		for {
			_, err := li.Next(ctx)
			if err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			got++
		}
		if _, err := li.Next(ctx); err != io.EOF {
			t.Errorf("%+v: Next after the end: %v", c, err)
		}
		if err := li.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if want := min(c.rows, c.limit); got != want {
			t.Errorf("%+v: %d rows, want %d", c, got, want)
		}
		if found := ctx.GetLastQueryInfoInt(sql.FoundRows); found != c.rows {
			t.Errorf("%+v: FOUND_ROWS() = %d", c, found)
		}
		if child.nexts != c.nexts && c.rows > c.limit {
			t.Errorf("%+v: child Next called %d times, want %d", c, child.nexts, c.nexts)
		}
		if c.can && c.rows > c.limit && child.counts != 1 {
			t.Errorf("%+v: CountRest called %d times", c, child.counts)
		}
	}

	// CALC_FOUND_ROWS のない LIMIT は数えない
	child := &counterIter{n: 100, can: true}
	li := &LimitIter{Limit: 5, ChildIter: child}
	for {
		if _, err := li.Next(sql.NewEmptyContext()); err != nil {
			break
		}
	}
	if child.counts != 0 || child.nexts != 5 {
		t.Errorf("without SQL_CALC_FOUND_ROWS: %d counts, %d nexts", child.counts, child.nexts)
	}
}
