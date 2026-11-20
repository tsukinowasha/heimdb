package backend

import (
	"strings"
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	gms "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// stage92Script は、列に収まらない値の書き込み（docs/plan.md の段階 92）。非 STRICT では、長すぎる文字列を切って
// 警告 1265（末尾の空白だけなら Note）、範囲の外の数を型の端の値にして警告 1264。INSERT IGNORE と UPDATE IGNORE は
// STRICT でも同じ。
func stage92Script() []string {
	const cs = " DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci"
	return []string{
		wpSQLMode,
		"CREATE TABLE s (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(5), c CHAR(3), tx TINYTEXT, b VARBINARY(4), tb TINYBLOB)" + cs,
		"INSERT INTO s (v) VALUES ('abcdefg')",
		"SHOW WARNINGS",
		"INSERT INTO s (v) VALUES ('あいうえおか'), ('abc  '), ('abcde   '), ('abcde\t'), ('abcde \n'), ('abcde　'), (12345678)",
		"SHOW WARNINGS",
		"INSERT INTO s (c) VALUES ('ab\t\t'), ('ab \t'), ('abc\n')",
		"SHOW WARNINGS",
		"INSERT INTO s (c, tx) VALUES ('abcd', CONCAT(REPEAT('あ', 100), '   ')), ('ab   ', REPEAT('x', 300))",
		"SHOW WARNINGS",
		"INSERT INTO s (tx) VALUES (CONCAT(REPEAT('x', 255), '  '))",
		"SHOW WARNINGS",
		"INSERT INTO s (b, tb) VALUES ('abcdef', REPEAT('y', 300)), ('ab    ', NULL), (X'0102030405', NULL)",
		"SHOW WARNINGS",
		"SELECT id, v, c, HEX(c), CHAR_LENGTH(tx), LENGTH(tx), HEX(b), LENGTH(tb) FROM s ORDER BY id",
		"CREATE TABLE n (id INT AUTO_INCREMENT PRIMARY KEY, i INT, u INT UNSIGNED, ti TINYINT, tu TINYINT UNSIGNED, mi MEDIUMINT, bi BIGINT, bu BIGINT UNSIGNED, d DECIMAL(5,2), f FLOAT, db DOUBLE)" + cs,
		"INSERT INTO n (i) VALUES (99999999999), (-99999999999), ('99999999999'), (1e20), ('1e20'), (2147483647.6), (2147483647.4), ('99999999999abc'), ('-99999999999 '), (-2147483648.5)",
		"SHOW WARNINGS",
		"INSERT INTO n (u, tu) VALUES (-1, 256), ('-5', '-1'), (5000000000, -0.4), (-0.6, 0)",
		"SHOW WARNINGS",
		"INSERT INTO n (ti, mi, bi, bu) VALUES (300, 8388608, 99999999999999999999, -1), (-300, -8388609, '-99999999999999999999', '99999999999999999999')",
		"SHOW WARNINGS",
		"INSERT INTO n (d) VALUES (12345.678), (-99999), ('1000'), (999.995), (999.994), ('99999abc'), ('abc'), ('-1e10')",
		"SHOW WARNINGS",
		"INSERT INTO n (f, db) VALUES (1e40, '1e400'), (-1e40, '-1e400')",
		"SHOW WARNINGS",
		"INSERT INTO n (i, u, d) VALUES (99999999999, -1, 12345)",
		"SHOW WARNINGS",
		// go-mysql-server の Convert が UNSIGNED への float と DECIMAL を誤って判定するもの、大きすぎる指数、DECIMAL の小数
		"INSERT INTO n (u, bu) VALUES (3e9, 0e0), (5000000000.0, 99999999999999999999), (4294967295.4, 1.5)",
		"SHOW WARNINGS",
		"INSERT INTO n (i, db, d) VALUES ('1e99999', '1e99999', '1.005'), ('-1e99999', '-1e99999', ' 1.50 '), ('1e-99999', '1e-99999', '1.2e1')",
		"SHOW WARNINGS",
		"SELECT id, i, u, ti, tu, mi, bi, bu, d, f, db FROM n ORDER BY id",
		// UPDATE、ON DUPLICATE KEY UPDATE、INSERT ... SELECT、REPLACE
		"UPDATE s SET v = CONCAT(v, 'zzzzzz') WHERE id <= 3",
		"SHOW WARNINGS",
		"UPDATE n SET i = i + 3000000000, d = d * 10 WHERE id IN (1, 7)",
		"SHOW WARNINGS",
		"INSERT INTO s (id, v) VALUES (100, 'a'), (101, 'b'), (1, 'c') ON DUPLICATE KEY UPDATE v = 'yyyyyyyy'",
		"SHOW WARNINGS",
		// add_option と同じ形（VALUES() は切った後の値）
		"INSERT INTO s (id, v) VALUES (100, 'abcdefgh') ON DUPLICATE KEY UPDATE v = VALUES(v)",
		"SHOW WARNINGS",
		"INSERT INTO s (id, v) VALUES (200, 'q'), (101, 'あいうえおかき') ON DUPLICATE KEY UPDATE v = VALUES(v)",
		"SHOW WARNINGS",
		"INSERT INTO s (v, c) SELECT CONCAT(v, 'qqqqqq'), 'xxxx' FROM s WHERE id IN (2, 3) ORDER BY id",
		"SHOW WARNINGS",
		"REPLACE INTO s (id, v) VALUES (2, 'rrrrrrrr')",
		"SHOW WARNINGS",
		"SELECT id, v, c FROM s ORDER BY id",
		// STRICT: エラーは今までどおり（字は MySQL と違う。記録だけ）。IGNORE は MySQL と同じに直す
		"SET SESSION sql_mode = 'STRICT_TRANS_TABLES'",
		"CREATE TABLE g (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(3), b VARBINARY(2), n INT, u INT UNSIGNED, d DECIMAL(5,2), dt DATETIME, k INT NOT NULL)" + cs,
		"INSERT IGNORE INTO g (v, b, n, u, d, k) VALUES ('あいうえ', X'010203', 99999999999, -1, 12345, 1), ('abcd  ', 'ab ', '12abc', '-5', '99999abc', 2)",
		"SHOW WARNINGS",
		"INSERT IGNORE INTO g (v, dt, k) VALUES ('x', '2012-01-01 25:00:00', 3), ('y', '2012-13-01', 4)",
		"SHOW WARNINGS",
		"INSERT IGNORE INTO g (v, k) VALUES ('z', NULL), ('w', 5)",
		"SHOW WARNINGS",
		"INSERT IGNORE INTO g (v, k) VALUES ('s', NULL)",
		"SHOW WARNINGS",
		"INSERT IGNORE INTO g (v) VALUES ('t')",
		"SHOW WARNINGS",
		"UPDATE IGNORE g SET v = 'abcdef', n = n + 3000000000 WHERE id = 1",
		"SHOW WARNINGS",
		"UPDATE IGNORE g SET k = NULL WHERE id = 2",
		"SHOW WARNINGS",
		"SELECT id, v, HEX(v), HEX(b), n, u, d, dt, k FROM g ORDER BY id",
	}
}

func TestStage92NonStrictWritesMatchMySQL(t *testing.T) {
	checkGoldenRun(t, "../testdata/stage92_golden.txt", "heimdb_golden92", stage92Script(), runWithoutValuesDeprecation)
}

// runWithoutValuesDeprecation は runScriptErrors と同じ。VALUES() の非推奨の警告 1287 の行は除く（HeimDB は出さない。
// compat/known-differences.txt に記録した違い）。
func runWithoutValuesDeprecation(t *testing.T, dsn string, stmts []string) []string {
	out := runScriptErrors(t, dsn, stmts)
	for i, r := range out {
		var keep []string
		for _, l := range strings.Split(r, "\n") {
			if !strings.HasPrefix(l, "Warning | 1287 |") {
				keep = append(keep, l)
			}
		}
		out[i] = strings.Join(keep, "\n")
	}
	return out
}

// numberBounds は、型ごとの端の値。
func TestNumberBounds(t *testing.T) {
	for _, c := range []struct {
		typ    gms.Type
		lo, hi string
	}{
		{types.Int8, "-128", "127"},
		{types.Uint8, "0", "255"},
		{types.Int24, "-8388608", "8388607"},
		{types.Uint24, "0", "16777215"},
		{types.Int32, "-2147483648", "2147483647"},
		{types.Uint32, "0", "4294967295"},
		{types.Int64, "-9223372036854775808", "9223372036854775807"},
		{types.Uint64, "0", "18446744073709551615"},
		{types.MustCreateDecimalType(5, 2), "-999.99", "999.99"},
		{types.MustCreateDecimalType(3, 0), "-999", "999"},
	} {
		lo, hi, _, ok := numberBounds(c.typ)
		if !ok || lo.String() != c.lo || hi.String() != c.hi {
			t.Errorf("%s: %v %s %s, want %s %s", c.typ, ok, lo, hi, c.lo, c.hi)
		}
	}
	if _, _, _, ok := numberBounds(types.Year); ok {
		t.Error("YEAR has bounds")
	}
	// 端の値は、型の Convert が範囲の中として受け付ける
	for _, typ := range []gms.Type{types.Int8, types.Uint32, types.Int64, types.Uint64, types.Float32, types.Float64, types.MustCreateDecimalType(5, 2)} {
		lo, hi, _, _ := numberBounds(typ)
		for _, d := range []decimal.Decimal{lo, hi} {
			if _, inRange, err := typ.Convert(gms.NewEmptyContext(), boundValue(typ, d)); err != nil || inRange != gms.InRange {
				t.Errorf("%s: bound %s: %v %v", typ, d, inRange, err)
			}
		}
	}
}

// fixString は、文字の境目で切る（TEXT はバイトの上限の手前の境目、VARCHAR は文字の数）。
func TestFixStringBoundary(t *testing.T) {
	ctx := gms.NewEmptyContext()
	tiny := &gms.Column{Name: "tx", Type: types.TinyText}
	// 'あ' は 3 バイト。255 バイトの上限に、x を 1 つ前に置くと境目が 254 バイトになる
	in := "x" + strings.Repeat("あ", 100)
	got, ok := fixString(ctx, tiny, in, 1)
	if !ok || len(got.(string)) != 253 || !strings.HasPrefix(in, got.(string)) {
		t.Errorf("TINYTEXT: %v %d", ok, len(got.(string)))
	}
	vc := &gms.Column{Name: "v", Type: types.MustCreateString(sqltypes.VarChar, 3, gms.Collation_utf8mb4_unicode_520_ci)}
	if got, ok := fixString(ctx, vc, "あいうえ", 1); !ok || got != "あいう" {
		t.Errorf("VARCHAR(3): %v %q", ok, got)
	}
	if _, ok := fixString(ctx, vc, "あいう", 1); ok {
		t.Error("VARCHAR(3): fixed a value that fits")
	}
	vb := &gms.Column{Name: "b", Type: types.MustCreateBinary(sqltypes.VarBinary, 2)}
	if got, ok := fixString(ctx, vb, []byte{1, 2, 3}, 1); !ok || string(got.([]byte)) != "\x01\x02" {
		t.Errorf("VARBINARY(2): %v %q", ok, got)
	}
}
