package backend

import (
	"strings"
	"testing"

	"github.com/dolthub/vitess/go/sqltypes"

	gms "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// stage93Script は、失敗した文の後の SHOW WARNINGS、照合順序で等しい値の扱い、sql_mode の既定（docs/plan.md の
// 段階 93）。
func stage93Script() []string {
	const cs = " DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci"
	return []string{
		// sql_mode の既定と並び
		"SELECT @@GLOBAL.sql_mode, @@SESSION.sql_mode",
		"SET SESSION sql_mode = 'NO_ENGINE_SUBSTITUTION,ERROR_FOR_DIVISION_BY_ZERO,NO_ZERO_IN_DATE'",
		"SELECT @@SESSION.sql_mode",
		"SET SESSION sql_mode = 'TIME_TRUNCATE_FRACTIONAL,PAD_CHAR_TO_FULL_LENGTH,NO_ENGINE_SUBSTITUTION,HIGH_NOT_PRECEDENCE,ERROR_FOR_DIVISION_BY_ZERO,ALLOW_INVALID_DATES,NO_ZERO_DATE,NO_ZERO_IN_DATE,STRICT_ALL_TABLES,STRICT_TRANS_TABLES,NO_BACKSLASH_ESCAPES,NO_AUTO_VALUE_ON_ZERO,NO_DIR_IN_CREATE,NO_UNSIGNED_SUBTRACTION,ONLY_FULL_GROUP_BY,IGNORE_SPACE,PIPES_AS_CONCAT,REAL_AS_FLOAT'",
		"SELECT @@SESSION.sql_mode",
		"SET SESSION sql_mode = DEFAULT",
		"SELECT @@SESSION.sql_mode",
		"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci",
		wpSQLMode,
		"SELECT @@SESSION.sql_mode",
		// 失敗した文の後の SHOW WARNINGS（エラーの行。失敗する前の警告は残る）
		"CREATE TABLE w (id INT PRIMARY KEY, v VARCHAR(3))" + cs,
		"INSERT INTO w VALUES (1, 'a')",
		"INSERT INTO w VALUES (1, 'b')",
		"SHOW WARNINGS",
		"INSERT INTO w VALUES (2, 'abcdef')",
		"SHOW WARNINGS",
		"INSERT INTO w VALUES (3, 'abcdef'), (1, 'x')",
		"SHOW WARNINGS",
		"SHOW WARNINGS",
		"INSERT INTO w VALUES (4, 'abcdef')",
		"SELECT CAST(1 AS DECIMAL(66, 0))",
		"SHOW WARNINGS",
		"INSERT INTO w VALUES (5, 'abcdef'), (6, 'abcdef'), (1, 'x')",
		"SHOW WARNINGS LIMIT 1",
		"SHOW WARNINGS LIMIT 1, 2",
		"SELECT 1",
		"SHOW WARNINGS",
		"INSERT IGNORE INTO w VALUES (1, 'y')",
		"SHOW WARNINGS",
		"UPDATE w SET id = 2 WHERE id = 1",
		"SHOW WARNINGS",
		"SELECT id, v FROM w ORDER BY id",
		// 照合順序で等しい値（a = A = ａ、あ = ア = ｱ、e = é）
		"CREATE TABLE c (id INT PRIMARY KEY, w VARCHAR(20), b VARBINARY(20))" + cs,
		"INSERT INTO c VALUES (1, 'a', 'a'), (2, 'A', 'A'), (3, 'ａ', 'a'), (4, 'b', 'b'), (5, 'B', 'B'), (6, 'あ', 'x'), (7, 'ア', 'y'), (8, 'ｱ', 'z'), (9, 'い', 'i'), (10, 'é', 'e'), (11, 'e', 'e'), (12, NULL, NULL)",
		"SELECT DISTINCT w FROM c ORDER BY w",
		"SELECT DISTINCT w FROM c",
		"SELECT COUNT(*) FROM (SELECT DISTINCT w FROM c) t",
		"SELECT DISTINCT w, b FROM c ORDER BY w, b",
		"SELECT DISTINCT b FROM c ORDER BY b",
		"SELECT COUNT(DISTINCT w), COUNT(DISTINCT b), COUNT(DISTINCT w, b), COUNT(DISTINCT LEFT(w, 1)) FROM c",
		"SELECT GROUP_CONCAT(DISTINCT w ORDER BY w), GROUP_CONCAT(DISTINCT b ORDER BY b) FROM c",
		"SELECT w, COUNT(*) FROM c GROUP BY w ORDER BY MIN(id)",
		"SELECT COUNT(*) FROM (SELECT w FROM c WHERE id <= 5 UNION SELECT w FROM c WHERE id > 5) t",
		"SELECT w FROM c WHERE id <= 3 UNION SELECT w FROM c WHERE id BETWEEN 4 AND 8 ORDER BY w",
		"SELECT COUNT(*) FROM (SELECT w FROM c WHERE id IN (1, 4, 6) INTERSECT SELECT w FROM c WHERE id IN (2, 8, 9)) t",
		"SELECT COUNT(*) FROM (SELECT w FROM c WHERE id IN (1, 4, 6, 9) EXCEPT SELECT w FROM c WHERE id IN (2, 8)) t",
		"CREATE TABLE c2 (id INT PRIMARY KEY, w VARCHAR(20))" + cs,
		"INSERT INTO c2 VALUES (1, 'A'), (2, 'b'), (3, 'ア'), (4, 'E')",
		"SELECT c.id, c2.id FROM c JOIN c2 ON c.w = c2.w ORDER BY c.id, c2.id",
		"SELECT c.id, c2.id FROM c LEFT JOIN c2 ON c.w = c2.w ORDER BY c.id, c2.id",
		"SELECT id FROM c WHERE w IN (SELECT w FROM c2) ORDER BY id",
		"SELECT id FROM c WHERE w NOT IN (SELECT w FROM c2) ORDER BY id",
		"SELECT id FROM c WHERE EXISTS (SELECT 1 FROM c2 WHERE c2.w = c.w) ORDER BY id",
		"SELECT id, w IN (SELECT w FROM c2) FROM c ORDER BY id",
		"CREATE TABLE d (id INT PRIMARY KEY, w VARCHAR(20), n INT)" + cs,
		"INSERT INTO d VALUES (1, '1', 1), (2, '2', 1), (3, 'A', 2), (4, 'a', 2), (5, 'ａ', 3)",
		"SELECT a.id, b.id FROM d a JOIN d b ON a.w = b.w AND a.n = b.n ORDER BY a.id, b.id",
		"SELECT DISTINCT IF(id = 1, w, n) AS k FROM d WHERE id <= 2 ORDER BY k",
		"SELECT COUNT(DISTINCT IF(id = 1, w, n)) FROM d WHERE id <= 2",
		"SELECT LPAD('abc', -1, 'x'), RPAD('abc', -1, 'x'), LPAD('abc', 0, 'x'), LPAD('ab', 4, ''), LPAD('あいう', 2, 'x'), RPAD('あ', 3, 'いう')",
		"SELECT x FROM (WITH RECURSIVE r (x) AS (SELECT w FROM c WHERE id = 1 UNION SELECT UPPER(x) FROM r) SELECT x FROM r) t",
		// 文字列の式（結果の照合順序は引数の照合順序）
		"SELECT DISTINCT LEFT(w, 1) AS k FROM c ORDER BY k",
		"SELECT COUNT(*) FROM c GROUP BY LEFT(w, 1) ORDER BY MIN(id)",
		"SELECT id, LEFT(w, 1) AS k FROM c ORDER BY k, id",
		"SELECT DISTINCT SUBSTRING_INDEX(w, '-', 1) AS k FROM c ORDER BY k",
		"SELECT DISTINCT RIGHT(w, 1) AS k FROM c ORDER BY k",
		"SELECT DISTINCT CONCAT(w, '-') AS k FROM c ORDER BY k",
		"SELECT DISTINCT CONCAT_WS('-', w, 'x') AS k FROM c ORDER BY k",
		"SELECT DISTINCT REPLACE(w, 'z', 'y') AS k FROM c ORDER BY k",
		"SELECT DISTINCT LPAD(w, 2, '-') AS k FROM c ORDER BY k",
		"SELECT DISTINCT RPAD(w, 2, '-') AS k FROM c ORDER BY k",
		"SELECT DISTINCT REPEAT(w, 2) AS k FROM c ORDER BY k",
		"SELECT DISTINCT REVERSE(w) AS k FROM c ORDER BY k",
		"SELECT DISTINCT TRIM(w) AS k FROM c ORDER BY k",
		"SELECT DISTINCT LOWER(w) AS k FROM c ORDER BY k",
		"SELECT DISTINCT SUBSTRING(w, 1, 1) AS k FROM c ORDER BY k",
		"SELECT DISTINCT IFNULL(w, '-') AS k FROM c ORDER BY k",
		"SELECT DISTINCT COALESCE(w, '-') AS k FROM c ORDER BY k",
		"SELECT DISTINCT IF(id > 0, w, '-') AS k FROM c ORDER BY k",
		"SELECT DISTINCT CASE WHEN id > 0 THEN w END AS k FROM c ORDER BY k",
		"SELECT COUNT(*) FROM c GROUP BY CONCAT(w, '') ORDER BY MIN(id)",
		"SELECT COLLATION(LEFT(w, 1)), COLLATION(RIGHT(w, 1)), COLLATION(SUBSTRING_INDEX(w, '-', 1)), COLLATION(CONCAT(w, '-')), COLLATION(CONCAT_WS('-', w)), COLLATION(REPLACE(w, 'a', 'b')), COLLATION(LPAD(w, 2, '-')), COLLATION(RPAD(w, 2, '-')), COLLATION(REPEAT(w, 2)) FROM c WHERE id = 1",
		// COLLATION() は、文字列とバイナリや数を混ぜた式で MySQL と違う（CONCAT(w, b) と LEFT(id, 1)。記録だけ）
		"SELECT COLLATION(LEFT(b, 1)), COLLATION(CONCAT(w, 1)), COLLATION(CONCAT('a', 'b')) FROM c WHERE id = 1",
		"SELECT DISTINCT LEFT(b, 1) AS k FROM c ORDER BY k",
		"SELECT DISTINCT LEFT(id, 1) AS k FROM c ORDER BY k",
		"SELECT DISTINCT CONCAT(w, b) AS k FROM c WHERE id <= 5 ORDER BY k",
		"SELECT id, ROW_NUMBER() OVER (PARTITION BY w ORDER BY id) FROM c ORDER BY id",
		"SELECT id, ROW_NUMBER() OVER (PARTITION BY LEFT(w, 1) ORDER BY id) FROM c ORDER BY id",
	}
}

func TestStage93MatchesMySQL(t *testing.T) {
	checkGoldenRun(t, "../testdata/stage93_golden.txt", "heimdb_golden93", stage93Script(), runScriptErrors)
}

// 構文の誤りや列名の誤りで失敗した文の後の SHOW WARNINGS は、前の文の警告を消し、エラーの行だけを出す（エラーの番号と
// 文は MySQL と違うのでゴールデンに入れない）。エラーの行は、覚える警告の上限（1024）を超えても出す。
func TestStage93ErrorRowAfterEarlyFailure(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	const cs = " DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci"
	out := runScriptErrors(t, dsn, []string{
		wpSQLMode,
		"CREATE TABLE w (id INT PRIMARY KEY, v VARCHAR(1))" + cs,
		"INSERT INTO w VALUES (1, 'abc')",
		"SELEC 1",
		"SHOW WARNINGS",
		"INSERT INTO w VALUES (2, 'abc')",
		"SELECT nosuch FROM w",
		"SHOW WARNINGS",
		"INSERT INTO w SELECT id + 10, 'abc' FROM (WITH RECURSIVE n (id) AS (SELECT 1 UNION ALL SELECT id + 1 FROM n WHERE id < 1100) SELECT id FROM n) t UNION ALL SELECT 1, 'x'",
		"SHOW WARNINGS LIMIT 1023, 5",
	})
	if !strings.HasPrefix(out[3], "error 1105 (HY000): syntax error") {
		t.Fatalf("SELEC: %s", out[3])
	}
	if got, want := out[4], "Error | 1105 | syntax error at position 6 near 'SELEC'\n"; got != want {
		t.Errorf("after a syntax error: %q, want %q", got, want)
	}
	if !strings.HasPrefix(out[7], "Error | ") || strings.Count(out[7], "\n") != 1 {
		t.Errorf("after an unknown column: %q", out[7])
	}
	if !strings.HasPrefix(out[8], "error 1062") {
		t.Fatalf("insert: %s", out[8])
	}
	if got, want := out[9], "Warning | 1265 | Data truncated for column 'v' at row 1024\nError | 1062 | Duplicate entry '1' for key 'w.PRIMARY'\n"; got != want {
		t.Errorf("error row over the limit: %q, want %q", got, want)
	}
}

// HashOfTypes は、文字列の型の値を照合順序の重みでハッシュする（バイナリと文字列でない値は HashOf と同じ）。
func TestStage93HashOfTypes(t *testing.T) {
	ctx := gms.NewEmptyContext()
	ci := []gms.Type{types.MustCreateString(sqltypes.VarChar, 20, gms.Collation_utf8mb4_unicode_520_ci), types.Int64}
	bin := []gms.Type{types.MustCreateString(sqltypes.VarChar, 20, gms.Collation_utf8mb4_bin), types.Int64}
	h := func(typs []gms.Type, r gms.Row) uint64 {
		v, err := gms.HashOfTypes(ctx, r, typs)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if h(ci, gms.Row{"a", 1}) != h(ci, gms.Row{"Ａ", 1}) {
		t.Error("unicode_520_ci: 'a' and 'Ａ' differ")
	}
	if h(ci, gms.Row{"a", 1}) == h(ci, gms.Row{"a", 2}) || h(ci, gms.Row{"a", 1}) == h(ci, gms.Row{"b", 1}) {
		t.Error("unicode_520_ci: different rows are equal")
	}
	if h(bin, gms.Row{"a", 1}) == h(bin, gms.Row{"A", 1}) {
		t.Error("utf8mb4_bin: 'a' and 'A' are equal")
	}
	vb := []gms.Type{types.MustCreateBinary(sqltypes.VarBinary, 20)}
	want, _ := gms.HashOf(ctx, gms.Row{[]byte("a")})
	if h(vb, gms.Row{[]byte("a")}) != want {
		t.Error("VARBINARY: differs from HashOf")
	}
	if h(ci, gms.Row{nil, nil}) != h(nil, gms.Row{nil, nil}) {
		t.Error("NULL: differs from HashOf")
	}
}
