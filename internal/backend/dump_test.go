package backend

import (
	"strings"
	"testing"
)

// mysqldump での出し入れのための振る舞い（docs/plan.md の段階 17）。セーブポイントは savepoint_test.go。

// SHOW CREATE TABLE は、ENUM と SET の既定値を値で書く（go-mysql-server は内部の番号で書くので、mysqldump の出力を
// MySQL に入れるとエラー 1067 になっていた）。
func TestShowCreateTableEnumSetDefaults(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	// 値にクォートを含む ENUM は、go-mysql-server が型の文字列でクォートをエスケープしないので作れない（記録だけ）。
	mustExec(t, m, "CREATE TABLE en (id int NOT NULL PRIMARY KEY, e enum('a','b','c') NOT NULL DEFAULT 'b', e2 enum('x','y') NULL DEFAULT NULL, st set('x','y') NOT NULL DEFAULT 'x,y', st2 set('p','q') NOT NULL DEFAULT '', q enum('publish','draft') NOT NULL DEFAULT 'draft')")
	var name, stmt string
	if err := m.QueryRow("SHOW CREATE TABLE en").Scan(&name, &stmt); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"`e` enum('a','b','c') NOT NULL DEFAULT 'b',",
		"`e2` enum('x','y') DEFAULT NULL,",
		"`st` set('x','y') NOT NULL DEFAULT 'x,y',",
		"`st2` set('p','q') NOT NULL DEFAULT '',",
		"`q` enum('publish','draft') NOT NULL DEFAULT 'draft',",
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("SHOW CREATE TABLE lacks %q:\n%s", want, stmt)
		}
	}
	// 書き出した文で作り直しても、既定値は同じ。
	mustExec(t, m, "DROP TABLE en")
	mustExec(t, m, stmt)
	mustExec(t, m, "INSERT INTO en (id) VALUES (1)")
	// CONCAT は ENUM と SET を内部の番号で連結する（go-mysql-server の以前からの差。記録だけ）ので、列ごとに読む。
	var e, st, st2, q string
	var e2 *string
	if err := m.QueryRow("SELECT e, e2, st, st2, q FROM en").Scan(&e, &e2, &st, &st2, &q); err != nil {
		t.Fatal(err)
	}
	if e != "b" || e2 != nil || st != "x,y" || st2 != "" || q != "draft" {
		t.Errorf("defaults after re-creating: %q %v %q %q %q", e, e2, st, st2, q)
	}
}

// utf8mb4 以外の文字コード（utf8mb3 は段階 37 から utf8mb4 として受け入れる）のテーブルは、テーブル名の入った明示的なエラーにする（mysqldump の
// 出力を取り込むとき、どのテーブルかが分かるように）。
func TestUnsupportedTablesNameTheTable(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE ok1 (id int NOT NULL PRIMARY KEY, b varchar(20), v varbinary(10), bl blob) DEFAULT CHARSET=utf8mb4")
	for q, want := range map[string]string{
		"CREATE TABLE l1 (id int NOT NULL PRIMARY KEY, b varchar(20)) DEFAULT CHARSET=latin1":                       "table l1 uses character set latin1",
		"CREATE TABLE c1 (id int NOT NULL PRIMARY KEY, b varchar(20) CHARACTER SET latin1) DEFAULT CHARSET=utf8mb4": "table c1 uses character set latin1",
		"ALTER TABLE ok1 CONVERT TO CHARACTER SET latin1":                                                           "table ok1 uses character set latin1",
		"ALTER TABLE ok1 ADD COLUMN c varchar(10) CHARACTER SET latin1":                                             "table ok1 uses character set latin1",
	} {
		if _, err := m.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", q, err, want)
		}
	}
	if got := one(t, m, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN ('l1', 'c1')"); got != "0" {
		t.Errorf("rejected tables were created: %s", got)
	}
}
