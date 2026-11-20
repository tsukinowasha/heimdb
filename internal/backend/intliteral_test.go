package backend

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 段階 77: 整数の列と整数の定数の比較は、定数が列の型にそのまま入るなら、定数の型を列の型にする（行ごとに変換しない）。

var intLiteralScript = []string{
	"CREATE TABLE ints (id int NOT NULL PRIMARY KEY, ti tinyint NOT NULL, tu tinyint unsigned NOT NULL, i int, bu bigint unsigned NOT NULL, bs bigint NOT NULL)",
	"INSERT INTO ints VALUES (1, -128, 0, NULL, 0, -9223372036854775808), (2, 127, 255, 1, 1, 9223372036854775807), " +
		"(3, 1, 1, -1, 18446744073709551615, 1), (4, 0, 128, 2, 9223372036854775808, 0)",
	// 列の型に入る定数（左右、<=>、値が 1 つの IN と NOT IN）
	"SELECT id FROM ints WHERE bu = 1 ORDER BY id",
	"SELECT id FROM ints WHERE 1 = bu ORDER BY id",
	"SELECT id FROM ints WHERE bu <> 0 ORDER BY id",
	"SELECT id FROM ints WHERE bu > 1 ORDER BY id",
	"SELECT id FROM ints WHERE bu >= 9223372036854775808 ORDER BY id",
	"SELECT id FROM ints WHERE bu = 18446744073709551615 ORDER BY id",
	"SELECT id FROM ints WHERE ti < 1 ORDER BY id",
	"SELECT id FROM ints WHERE tu <= 128 ORDER BY id",
	"SELECT id FROM ints WHERE i <=> 1 ORDER BY id",
	"SELECT id FROM ints WHERE i IN (2) ORDER BY id",
	"SELECT id FROM ints WHERE i NOT IN (2) ORDER BY id",
	"SELECT id FROM ints WHERE bs = -9223372036854775808 ORDER BY id",
	// 列の型に入らない定数（範囲の外、2^63 以上、負の数と unsigned）。そのまま比べる。符号付きの BIGINT の列と 2^63 は、
	// エンジンが DOUBLE で比べて 2^63-1 と等しいとみなす（MySQL と違う。段階 77 より前から。記録だけ）ので入れない
	"SELECT id FROM ints WHERE ti = 128 ORDER BY id",
	"SELECT id FROM ints WHERE ti < 128 ORDER BY id",
	"SELECT id FROM ints WHERE tu > -1 ORDER BY id",
	"SELECT id FROM ints WHERE bu = -1 ORDER BY id",
	"SELECT id FROM ints WHERE bu <> -1 ORDER BY id",
	"SELECT id FROM ints WHERE bu = 18446744073709551616 ORDER BY id",
	// 整数でない定数（小数、文字列、NULL）。そのまま比べる
	"SELECT id FROM ints WHERE bu = 1.0 ORDER BY id",
	"SELECT id FROM ints WHERE ti = 0.5 ORDER BY id",
	"SELECT id FROM ints WHERE bu = '1' ORDER BY id",
	"SELECT id FROM ints WHERE i <=> NULL ORDER BY id",
	"SELECT id FROM ints WHERE i = NULL ORDER BY id",
	// WordPress の形
	"SELECT COUNT(*) FROM ints WHERE ( ( ti = 0 OR ti = 1 ) ) AND tu NOT IN (128) AND bu = 1",
	"UPDATE ints SET i = 5 WHERE bu = 1",
	"DELETE FROM ints WHERE tu = 128",
	"SELECT id, i FROM ints ORDER BY id",
}

// 整数の定数との比較の結果が、本物の MySQL と同じ。
func TestIntLiteralQueriesMatchMySQL(t *testing.T) {
	stmts := append([]string{wpSQLMode}, intLiteralScript...)
	checkGolden(t, "../testdata/intliteral_golden.txt", "heimdb_golden77", stmts)
}

// 列の型に入る整数の定数だけを、列の型にする。
func TestIntLiteralTyped(t *testing.T) {
	ctx := sql.NewEmptyContext()
	bu := expression.NewGetFieldWithTable(0, 0, types.Uint64, "", "t", "bu", false)
	ti := expression.NewGetFieldWithTable(1, 0, types.Int8, "", "t", "ti", false)
	for _, tc := range []struct {
		name  string
		col   *expression.GetField
		lit   sql.Expression
		typed bool
	}{
		{"unsigned = 1", bu, expression.NewLiteral(int8(1), types.Int8), true},
		{"unsigned = 2^64-1", bu, expression.NewLiteral(uint64(18446744073709551615), types.Uint64), true},
		{"unsigned = -1", bu, expression.NewLiteral(int8(-1), types.Int8), false},
		{"tinyint = 127", ti, expression.NewLiteral(int8(127), types.Int8), true},
		{"tinyint = 128", ti, expression.NewLiteral(int16(128), types.Int16), false},
		{"unsigned = 1.0", bu, expression.NewLiteral(1.0, types.Float64), false},
		{"unsigned = '1'", bu, expression.NewLiteral("1", types.LongText), false},
		{"unsigned = NULL", bu, expression.NewLiteral(nil, types.Null), false},
	} {
		for _, flip := range []bool{false, true} {
			l, r := sql.Expression(tc.col), tc.lit
			if flip {
				l, r = r, l
			}
			n, _, err := columnTypedLiterals(ctx, nil, plan.NewFilter(expression.NewEquals(l, r), plan.NewResolvedDualTable()), nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			eq := n.(*plan.Filter).Expression.(*expression.Equals)
			lit := eq.Right()
			if flip {
				lit = eq.Left()
			}
			typed := types.TypesEqual(lit.Type(), tc.col.Type())
			if typed != tc.typed {
				t.Errorf("%s (flip %v): literal type %s, typed %v, want %v", tc.name, flip, lit.Type(), typed, tc.typed)
			}
		}
	}
}
