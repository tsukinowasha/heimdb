package backend

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlcompare"
	gms "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// stage90Script は、重なりのエラー（1062）の文と SQLSTATE、INSERT IGNORE の警告（docs/plan.md の段階 90）。
// 値は列の型の形で - でつなぎ、プレフィックスの索引はその文字数、64 バイトまで。ON DUPLICATE KEY UPDATE と REPLACE は
// 今までどおり。
func stage90Script() []string {
	// é は 2 バイトなので、64 バイトがちょうど文字の境目（3 バイトの文字は、MySQL が文字の途中で切るので
	// TestDuplicateEntryTruncation で確かめる）
	x250, j100 := strings.Repeat("x", 250), strings.Repeat("é", 40)
	return []string{
		wpSQLMode,
		"CREATE TABLE p (id INT PRIMARY KEY, a VARCHAR(50), b INT, c VARCHAR(300), d VARBINARY(10), UNIQUE KEY ab (a,b), UNIQUE KEY cpre (c(5)), UNIQUE KEY dk (d))",
		"INSERT INTO p VALUES (1,'x',1,'hello world',X'0031'),(2,NULL,NULL,'zzz',NULL)",
		"INSERT INTO p VALUES (1,'y',2,'q',NULL)",
		"INSERT INTO p VALUES (3,'x',1,'q2',NULL)",
		"INSERT INTO p VALUES (4,'w',1,'hello there',NULL)",
		"INSERT INTO p VALUES (5,'v',1,'q5',X'0031')",
		"UPDATE p SET id=1 WHERE id=2",
		"UPDATE p SET a='x', b=1 WHERE id=2",
		"INSERT IGNORE INTO p VALUES (1,'y',2,'q',NULL),(3,'x',1,'q2',NULL)",
		"SHOW WARNINGS",
		"INSERT INTO p VALUES (1,'z',3,'q9',NULL) ON DUPLICATE KEY UPDATE a='odku'",
		"REPLACE INTO p VALUES (2,'rep',4,'q10',NULL)",
		"SELECT id, a, b, c FROM p ORDER BY id",
		"CREATE TEMPORARY TABLE tt (id INT PRIMARY KEY)",
		"INSERT INTO tt VALUES (1),(1)",
		"CREATE TABLE `we-ird` (k VARCHAR(5), UNIQUE KEY `my key` (k))",
		"INSERT INTO `we-ird` VALUES ('a'),('A')",
		"CREATE TABLE f (id INT PRIMARY KEY, d DATETIME, x DECIMAL(5,2), UNIQUE KEY dx (d,x))",
		"INSERT INTO f VALUES (1,'2025-01-02 03:04:05',1.5),(2,'2025-01-02 03:04:05',1.5)",
		"CREATE TABLE l (id INT PRIMARY KEY, v VARCHAR(255), w VARCHAR(191), UNIQUE KEY lv (v), UNIQUE KEY lw (w))",
		"INSERT INTO l VALUES (1,'" + x250 + "','" + j100 + "')",
		"INSERT INTO l VALUES (2,'" + x250 + "','q')",
		"INSERT INTO l VALUES (3,'q','" + j100 + "')",
		// 主キーのない表を並べる一意インデックス、主キーのない表の後から作る一意インデックス
		"CREATE TABLE kl (u VARCHAR(10) NOT NULL, v INT, UNIQUE KEY uk (u))",
		"INSERT INTO kl VALUES ('a',1),('a',2)",
		"CREATE TABLE ad (id INT PRIMARY KEY, v INT)",
		"INSERT INTO ad VALUES (1,7),(2,7)",
		"ALTER TABLE ad ADD UNIQUE KEY av (v)",
		"CREATE UNIQUE INDEX av2 ON ad (v)",
		"CREATE TABLE tx (id INT PRIMARY KEY, t TEXT, UNIQUE KEY tk (t(3)))",
		"INSERT INTO tx VALUES (1,'abcdef'),(2,'abcxyz')",
	}
}

// runScriptErrors は runScript と同じ。エラーは、番号に加えて SQLSTATE と文を書く。
func runScriptErrors(t *testing.T, dsn string, stmts []string) []string {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var out []string
	for _, q := range stmts {
		out = append(out, execOrCompare(ctx, conn, q))
		if afterStatement != nil {
			afterStatement(q)
		}
	}
	return out
}

func execOrCompare(ctx context.Context, conn *sql.Conn, q string) string {
	if _, err := conn.ExecContext(ctx, q); err != nil && !strings.HasPrefix(q, "SELECT") && !strings.HasPrefix(q, "SHOW") {
		var me *mysql.MySQLError
		if errors.As(err, &me) {
			return fmt.Sprintf("error %d (%s): %s", me.Number, me.SQLState, me.Message)
		}
	}
	if strings.HasPrefix(q, "SELECT") || strings.HasPrefix(q, "SHOW") {
		return sqlcompare.Run(ctx, conn, q)
	}
	return "ok"
}

func TestStage90DuplicateEntryMatchesMySQL(t *testing.T) {
	checkGoldenRun(t, "../testdata/stage90_golden.txt", "heimdb_golden90", stage90Script(), runScriptErrors)
}

// handshakeCollation は、伝えられた照合順序を utf8mb4 のものにし、知らない id と utf8mb4 にできないものは既定にする。
func TestHandshakeCollationMapping(t *testing.T) {
	unknown := uint8(0)
	for id := 1; id < 256; id++ {
		if gms.CollationID(id).Name() == "" {
			unknown = uint8(id)
			break
		}
	}
	if unknown == 0 {
		t.Fatal("no unused collation id")
	}
	for _, c := range []struct {
		id   uint8
		want gms.CollationID
	}{
		{255, gms.Collation_utf8mb4_0900_ai_ci},
		{45, gms.Collation_utf8mb4_general_ci},
		{46, gms.Collation_utf8mb4_bin},
		{224, gms.Collation_utf8mb4_unicode_ci},
		{246, gms.Collation_utf8mb4_unicode_520_ci},
		{33, gms.Collation_utf8mb4_general_ci},  // utf8mb3_general_ci（HeimDB の挨拶が名乗るもの）
		{192, gms.Collation_utf8mb4_unicode_ci}, // utf8mb3_unicode_ci
		{11, gms.Collation_utf8mb4_general_ci},  // ascii_general_ci
		{8, gms.Collation_utf8mb4_0900_ai_ci},   // latin1_swedish_ci
		{63, gms.Collation_utf8mb4_0900_ai_ci},  // binary
		{0, gms.Collation_utf8mb4_0900_ai_ci},
		{unknown, gms.Collation_utf8mb4_0900_ai_ci},
	} {
		if got := handshakeCollation(c.id); got != c.want {
			t.Errorf("handshakeCollation(%d) = %s, want %s", c.id, got.Name(), c.want.Name())
		}
	}
}

// 重なりのエラーの値は 64 バイトまでで、HeimDB は文字の境目で切る（MySQL は文字の途中で切り、壊れた文字が残る）。
func TestDuplicateEntryTruncation(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	got := runScriptErrors(t, dsn, []string{
		"CREATE TABLE j (id INT PRIMARY KEY, w VARCHAR(191), UNIQUE KEY jw (w))",
		"INSERT INTO j VALUES (1, '" + strings.Repeat("あ", 30) + "')",
		"INSERT INTO j VALUES (2, '" + strings.Repeat("あ", 30) + "')",
	})
	want := "error 1062 (23000): Duplicate entry '" + strings.Repeat("あ", 21) + "' for key 'j.jw'"
	if got[2] != want {
		t.Errorf("got %s, want %s", got[2], want)
	}
}
