// Copyright 2026 Tsukinowa Inc.

package expression

import (
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 照合順序の違う文字列の列と定数の比較（HeimDB の段階 96）。値は今までと同じで、正しい UTF-8 の文字列は変換しない
// （1 回の比較で 8 回のアロケーションが 1 回になる）。不正な UTF-8 は今までどおり変換で NULL になって一致せず、エラーにしない。
func TestCompareTextValues(t *testing.T) {
	ctx := sql.NewEmptyContext()
	col := types.MustCreateString(sqltypes.VarChar, 255, sql.Collation_utf8mb4_unicode_520_ci)
	field := NewGetField(0, col, "meta_key", true)
	cases := []struct {
		row   string
		lit   string
		equal interface{}
		less  interface{}
	}{
		{"abc", "abc", true, false},
		{"ABC", "abc", true, false}, // 列の照合順序（大小文字を区別しない）
		{"ａｂｃ", "abc", true, false},
		{"abc ", "abc", false, false}, // PAD SPACE を持たない（compat/known-differences.txt）
		{"abb", "abc", false, true},
		{"\xff", "abc", false, false}, // 不正な UTF-8（変換で NULL になり、一致しない）
	}
	for _, c := range cases {
		lit := NewLiteral(c.lit, types.LongText)
		for _, e := range []struct {
			expr sql.Expression
			want interface{}
		}{{NewEquals(field, lit), c.equal}, {NewLessThan(field, lit), c.less}} {
			got, err := e.expr.Eval(ctx, sql.Row{c.row})
			if err != nil || got != e.want {
				t.Errorf("%q %s %q: %v, %v; want %v", c.row, e.expr, c.lit, got, err, e.want)
			}
		}
	}
	// COLLATE で定数の照合順序を明示すると、そちらで比べる
	bin := NewCollatedExpression(NewLiteral("abc", types.LongText), sql.Collation_utf8mb4_bin)
	if got, err := NewEquals(field, bin).Eval(ctx, sql.Row{"ABC"}); err != nil || got != false {
		t.Errorf("COLLATE utf8mb4_bin: %v, %v", got, err)
	}

	eq := NewEquals(field, NewLiteral("abc", types.LongText))
	row := sql.Row{"abd"}
	if n := testing.AllocsPerRun(100, func() { eq.Eval(ctx, row) }); n > 1 {
		t.Errorf("%v allocations per comparison", n)
	}
}
