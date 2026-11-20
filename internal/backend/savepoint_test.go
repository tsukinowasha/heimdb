package backend

import "testing"

// savepointScript は、セーブポイントの振る舞いを本物の MySQL と比べる文の列（docs/plan.md の段階 17）。
var savepointScript = []string{
	"CREATE TABLE sp (id int NOT NULL PRIMARY KEY, v varchar(10) NOT NULL)",
	"START TRANSACTION",
	"INSERT INTO sp VALUES (1, 'a')",
	"SAVEPOINT a",
	"INSERT INTO sp VALUES (2, 'b')",
	"SAVEPOINT b",
	"INSERT INTO sp VALUES (3, 'c')",
	"ROLLBACK TO SAVEPOINT b",
	"SELECT id, v FROM sp ORDER BY id",
	// 名前は大文字と小文字を区別しない。a まで戻すと、後の b は忘れる
	"ROLLBACK TO SAVEPOINT A",
	"SELECT id, v FROM sp ORDER BY id",
	"ROLLBACK TO SAVEPOINT b",
	// 同じ名前は置き換える
	"INSERT INTO sp VALUES (4, 'd')",
	"SAVEPOINT a",
	"UPDATE sp SET v = 'x' WHERE id = 4",
	"ROLLBACK TO a",
	"SELECT id, v FROM sp ORDER BY id",
	// RELEASE はそれと後のものを忘れる（書き込みは残る）
	"SAVEPOINT c",
	"DELETE FROM sp WHERE id = 1",
	"RELEASE SAVEPOINT a",
	"ROLLBACK TO SAVEPOINT c",
	"ROLLBACK TO SAVEPOINT a",
	"SELECT id, v FROM sp ORDER BY id",
	"COMMIT",
	"SELECT id, v FROM sp ORDER BY id",
	// 自動コミットでは、セーブポイントは文の終わりに消える
	"SAVEPOINT x",
	"ROLLBACK TO SAVEPOINT x",
	// トランザクション全体の ROLLBACK はセーブポイントより前の書き込みも捨てる
	"BEGIN",
	"INSERT INTO sp VALUES (5, 'e')",
	"SAVEPOINT s",
	"INSERT INTO sp VALUES (6, 'f')",
	"ROLLBACK",
	"SELECT COUNT(*) FROM sp WHERE id >= 5",
	// AUTO_INCREMENT の払い出しは戻らない（欠番になる）
	"CREATE TABLE spa (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10) NOT NULL)",
	"BEGIN",
	"INSERT INTO spa (v) VALUES ('one')",
	"SAVEPOINT s",
	"INSERT INTO spa (v) VALUES ('two')",
	"ROLLBACK TO SAVEPOINT s",
	"INSERT INTO spa (v) VALUES ('three')",
	"COMMIT",
	"SELECT id, v FROM spa ORDER BY id",
	// mysqldump --single-transaction と同じ一連の文
	"SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ",
	"START TRANSACTION /*!40100 WITH CONSISTENT SNAPSHOT */",
	"SAVEPOINT sp",
	"SELECT id, v FROM sp ORDER BY id",
	"ROLLBACK TO SAVEPOINT sp",
	"SELECT id, v FROM spa ORDER BY id",
	"ROLLBACK TO SAVEPOINT sp",
	"RELEASE SAVEPOINT sp",
	"COMMIT",
}

func TestSavepointsMatchMySQL(t *testing.T) {
	checkGolden(t, "../testdata/savepoint_golden.txt", "heimdb_savepoint_golden", append([]string{wpSQLMode}, savepointScript...))
}
