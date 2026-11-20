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

// Modified by Tsukinowa Inc.: ENUM と SET を値の文字列で比べるかを式ごとに一度だけ決めるかのテスト（段階 122。docs/engine.md）
// Modified by Tsukinowa Inc.: 両辺の型が同じかと、= の両辺が行の値かを式ごとに一度だけ決めるかのテスト（段階 123。docs/engine.md）

package expression

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// countingField は、Children を呼んだ回数を数える列の参照（enumSetAsText の constantExpr が呼ぶ）。
type countingField struct {
	*GetField
	n *int64
}

func (f *countingField) Children() []sql.Expression {
	atomic.AddInt64(f.n, 1)
	return nil
}

func (f *countingField) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return f, nil
}

// TestStage122EnumSetDecisionOnce は、ENUM か SET を値の文字列で比べるか（enumSetAsText）を、比べの式ごとに最初の
// Compare で一度だけ決め、WithChildren で作った新しい式では決め直すかを確かめる（段階 122。前は行ごとに両辺の
// 型を調べた）。結果は段階 116 と同じ（ENUM の列と文字列の列は値の文字列で、定数の文字列は ENUM の番号でも比べる）。
func TestStage122EnumSetDecisionOnce(t *testing.T) {
	ctx := sql.NewEmptyContext()
	enum := types.MustCreateEnumType([]string{"a", "b", "c"}, sql.Collation_Default)
	e := NewGetField(0, enum, "e", false)
	var n int64
	v := &countingField{GetField: NewGetField(1, types.LongText, "v", false), n: &n}

	ctors := []struct {
		name string
		new  func(l, r sql.Expression) sql.Expression
		// e = 'a'（番号 1）と v の 'a'、'b'、'1' の結果
		want [3]bool
	}{
		{"=", func(l, r sql.Expression) sql.Expression { return NewEquals(l, r) }, [3]bool{true, false, false}},
		{"<", func(l, r sql.Expression) sql.Expression { return NewLessThan(l, r) }, [3]bool{false, true, false}},
		{">", func(l, r sql.Expression) sql.Expression { return NewGreaterThan(l, r) }, [3]bool{false, false, true}},
		{"<=", func(l, r sql.Expression) sql.Expression { return NewLessThanOrEqual(l, r) }, [3]bool{true, true, false}},
		{">=", func(l, r sql.Expression) sql.Expression { return NewGreaterThanOrEqual(l, r) }, [3]bool{true, false, true}},
	}
	rows := []sql.Row{{uint16(1), "a"}, {uint16(1), "b"}, {uint16(1), "1"}}
	for _, c := range ctors {
		atomic.StoreInt64(&n, 0)
		cmp := c.new(e, v)
		for i := 0; i < 100; i++ {
			for j, row := range rows {
				got, err := cmp.Eval(ctx, row)
				if err != nil || got != c.want[j] {
					t.Fatalf("%s row %v: got %v, %v; want %v", c.name, row, got, err, c.want[j])
				}
			}
		}
		if got := atomic.LoadInt64(&n); got != 1 {
			t.Errorf("%s: the ENUM text decision looked at the string side %d times over 300 rows, want 1", c.name, got)
		}

		// WithChildren の新しい式は決め直す: 同じ子なら同じ結果で、もう一度だけ調べる
		nc, err := cmp.WithChildren(e, v)
		if err != nil {
			t.Fatal(err)
		}
		for j, row := range rows {
			if got, err := nc.Eval(ctx, row); err != nil || got != c.want[j] {
				t.Errorf("%s after WithChildren row %v: got %v, %v; want %v", c.name, row, got, err, c.want[j])
			}
		}
		if got := atomic.LoadInt64(&n); got != 2 {
			t.Errorf("%s after WithChildren: looked %d times in all, want 2", c.name, got)
		}
	}

	// 決めた後で子を定数の文字列に替えた式は、値の文字列でなく ENUM の比べになる（e = '1' は番号 1 の 'a' に合う。
	// 前の式の答えを写すと、値の文字列で比べて合わない）
	eq := NewEquals(e, v)
	if got, _ := eq.Eval(ctx, sql.Row{uint16(1), "1"}); got != false {
		t.Fatalf("e = v with 'a' and '1': got %v, want false", got)
	}
	lit, err := eq.WithChildren(e, NewLiteral("1", types.LongText))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := lit.Eval(ctx, sql.Row{uint16(1), "x"}); err != nil || got != true {
		t.Errorf("e = '1' with 'a': got %v, %v; want true", got, err)
	}
	// 逆に、文字列の列どうしの式に ENUM の列を入れると、値の文字列で比べる
	vv := NewEquals(NewGetField(1, types.LongText, "v", false), NewGetField(2, types.LongText, "w", false))
	if got, _ := vv.Eval(ctx, sql.Row{nil, "1", "1"}); got != true {
		t.Fatalf("v = w: got %v, want true", got)
	}
	ev, err := vv.WithChildren(e, NewGetField(1, types.LongText, "v", false))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ev.Eval(ctx, sql.Row{uint16(1), "1"}); err != nil || got != false {
		t.Errorf("e = v with 'a' and '1' after WithChildren: got %v, %v; want false", got, err)
	}

	// 作るときには型を見ない（解決する前の列の Type は panic する）
	_ = NewEquals(NewUnresolvedColumn("e"), NewUnresolvedColumn("v"))

	// 並行して評価しても、答えを一度決めれば同じ結果になる（go test -race で確かめる）
	atomic.StoreInt64(&n, 0)
	shared := NewEquals(e, v)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := sql.NewEmptyContext()
			for i := 0; i < 200; i++ {
				for j, row := range rows {
					if got, err := shared.Eval(ctx, row); err != nil || got != ctors[0].want[j] {
						t.Errorf("concurrent = row %v: got %v, %v", row, got, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&n); got < 1 || got > 8 {
		t.Errorf("concurrent: looked %d times, want 1 to 8", got)
	}
}

// typeCountingField は、Type を呼んだ回数を数える列の参照。
type typeCountingField struct {
	*GetField
	n *int64
}

func (f *typeCountingField) Type() sql.Type {
	atomic.AddInt64(f.n, 1)
	return f.GetField.Type()
}

func (f *typeCountingField) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return f, nil
}

// TestStage123TypeDecisionOnce は、比べの両辺の型が同じか（TypesEqual）と、= の両辺が行の値か（IsTuple）を、
// 式ごとに一度だけ決め、WithChildren で作った新しい式では決め直すかを確かめる（段階 123。前は行ごとに両辺の型を
// 調べ、段階 120 で項目の増えた NumberTypeImpl_.Equals が全件を読む WHERE の user_id = 1 を遅くした）。
func TestStage123TypeDecisionOnce(t *testing.T) {
	ctx := sql.NewEmptyContext()
	var n int64
	a := &typeCountingField{GetField: NewGetField(0, types.Uint64, "user_id", false), n: &n}

	ctors := []struct {
		name string
		new  func(l, r sql.Expression) sql.Expression
		want [3]bool // user_id が 0、1、2 で、右が 1
	}{
		{"=", func(l, r sql.Expression) sql.Expression { return NewEquals(l, r) }, [3]bool{false, true, false}},
		{"<", func(l, r sql.Expression) sql.Expression { return NewLessThan(l, r) }, [3]bool{true, false, false}},
		{">", func(l, r sql.Expression) sql.Expression { return NewGreaterThan(l, r) }, [3]bool{false, false, true}},
		{"<=", func(l, r sql.Expression) sql.Expression { return NewLessThanOrEqual(l, r) }, [3]bool{true, true, false}},
		{">=", func(l, r sql.Expression) sql.Expression { return NewGreaterThanOrEqual(l, r) }, [3]bool{false, true, true}},
	}
	rows := []sql.Row{{uint64(0)}, {uint64(1)}, {uint64(2)}}
	one := NewLiteral(uint64(1), types.Uint64)
	for _, c := range ctors {
		atomic.StoreInt64(&n, 0)
		cmp := c.new(a, one)
		for i := 0; i < 100; i++ {
			for j, row := range rows {
				got, err := cmp.Eval(ctx, row)
				if err != nil || got != c.want[j] {
					t.Fatalf("%s row %v: got %v, %v; want %v", c.name, row, got, err, c.want[j])
				}
			}
		}
		// 行ごとに残るのは、同じ型の Compare を呼ぶときの 1 回だけ（前は TypesEqual と IsTuple で 2〜3 回多かった）
		if got := atomic.LoadInt64(&n); got > 300+5 {
			t.Errorf("%s: the column's Type was called %d times over 300 rows, want at most 305", c.name, got)
		}
	}

	// WithChildren の新しい式は決め直す: 同じ型の数どうしで決めた後で、右を数として読む文字列にすると、Uint64 の
	// Compare（'1.4' を 1 にする）でなく数として比べる（1 = '1.4' は偽、1 < '1.4' は真）
	for _, c := range ctors[:2] {
		cmp := c.new(a, one)
		if got, err := cmp.Eval(ctx, sql.Row{uint64(1)}); err != nil || got != c.want[1] {
			t.Fatalf("%s 1 with 1: got %v, %v", c.name, got, err)
		}
		nc, err := cmp.WithChildren(a, NewLiteral("1.4", types.LongText))
		if err != nil {
			t.Fatal(err)
		}
		want := c.name == "<"
		if got, err := nc.Eval(ctx, sql.Row{uint64(1)}); err != nil || got != want {
			t.Errorf("%s 1 with '1.4' after WithChildren: got %v, %v; want %v", c.name, got, err, want)
		}
	}

	// 行の値の = は MySQL と同じく NULL を返し（段階 116 の TupleEquals）、行の値でない子に替えると普通の比べになる
	tl := NewTuple(NewLiteral(int64(1), types.Int64), NewLiteral(nil, types.Null))
	tr := NewTuple(NewLiteral(int64(1), types.Int64), NewLiteral(nil, types.Null))
	teq := NewEquals(tl, tr)
	for i := 0; i < 3; i++ {
		if got, err := teq.Eval(ctx, nil); err != nil || got != nil {
			t.Fatalf("(1, NULL) = (1, NULL): got %v, %v; want NULL", got, err)
		}
	}
	plain, err := teq.WithChildren(a, one)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plain.Eval(ctx, sql.Row{uint64(1)}); err != nil || got != true {
		t.Errorf("user_id = 1 after WithChildren of a row comparison: got %v, %v; want true", got, err)
	}
	back, err := plain.WithChildren(tl, tr)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := back.Eval(ctx, nil); err != nil || got != nil {
		t.Errorf("(1, NULL) = (1, NULL) after WithChildren of user_id = 1: got %v, %v; want NULL", got, err)
	}

	// 文字列の列と定数は、型が違う（照合順序）ので前と同じく照合順序で比べる（'A' = 'a' は _ci で真）
	s := NewGetField(0, types.MustCreateString(sqltypes.VarChar, 20, sql.Collation_utf8mb4_unicode_520_ci), "comment_approved", false)
	seq := NewEquals(s, NewLiteral("a", types.LongText))
	for _, v := range []string{"A", "a", "b"} {
		got, err := seq.Eval(ctx, sql.Row{v})
		if err != nil || got != (v != "b") {
			t.Errorf("%q = 'a': got %v, %v", v, got, err)
		}
	}

	// 並行して評価しても、答えを一度決めれば同じ結果になる（go test -race で確かめる）
	shared := NewEquals(a, one)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := sql.NewEmptyContext()
			for i := 0; i < 200; i++ {
				for j, row := range rows {
					if got, err := shared.Eval(ctx, row); err != nil || got != ctors[0].want[j] {
						t.Errorf("concurrent = row %v: got %v, %v", row, got, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
