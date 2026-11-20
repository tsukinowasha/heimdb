package backend

import "testing"

// INSERT ... ON DUPLICATE KEY UPDATE は、重なる行がなくても UPDATE の権限を求める（段階 111、上流の #3940）。前は
// SELECT と INSERT の権限だけのユーザーが、既存の行を書き換えられた。MySQL と同じく 1142 で拒否する。
func TestStage111UpsertNeedsUpdatePrivilege(t *testing.T) {
	addr, stop := startAuthServer(t, t.TempDir(), "rootpw")
	defer stop()
	root, err := connectAs(t, addr, "root", "rootpw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, q := range []string{
		"CREATE TABLE test (pk INT PRIMARY KEY, v1 INT)",
		"INSERT INTO test VALUES (0, 0), (1, 1)",
		"CREATE USER 'ins'@'%' IDENTIFIED BY 'inspw'",
		"GRANT SELECT, INSERT ON " + testDB + ".test TO 'ins'@'%'",
	} {
		if _, err := root.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ins, err := connectAs(t, addr, "ins", "inspw", testDB)
	if err != nil {
		t.Fatal(err)
	}
	defer ins.Close()
	for _, q := range []string{
		"INSERT INTO test VALUES (0, 9) ON DUPLICATE KEY UPDATE v1 = 9",
		"INSERT INTO test VALUES (99, 9) ON DUPLICATE KEY UPDATE v1 = 9",
	} {
		if _, err := ins.Exec(q); mysqlErrNo(err) != 1142 {
			t.Errorf("%s: err = %v, want 1142", q, err)
		}
	}
	if _, err := ins.Exec("INSERT INTO test VALUES (2, 2)"); err != nil {
		t.Errorf("plain INSERT: %v", err)
	}
	var v int
	if err := root.QueryRow("SELECT v1 FROM test WHERE pk = 0").Scan(&v); err != nil || v != 0 {
		t.Errorf("row 0: v1 = %d, %v (want 0)", v, err)
	}
	if err := root.QueryRow("SELECT COUNT(*) FROM test").Scan(&v); err != nil || v != 3 {
		t.Errorf("rows: %d, %v (want 3)", v, err)
	}
}
