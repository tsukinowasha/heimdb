package main

import (
	"database/sql"
	"testing"
)

// 接続の照合順序は、接続のときにクライアントが伝えたものになり、サーバの既定は MySQL 8.0 と同じ utf8mb4_0900_ai_ci
// （docs/plan.md の段階 90）。期待する値は、本物の MySQL 8.0.46 に同じドライバでつないで確かめたもの（utf8mb3 も
// MySQL は utf8mb4_general_ci にする）。latin1 だけは、MySQL は latin1 になるが、HeimDB は文字セットを utf8mb4 の
// ままにし、既定の照合順序にする。
func TestHandshakeCollation(t *testing.T) {
	bin := heimdbBinary(t)
	srv := startArgs(t, bin, "-data", t.TempDir(), "-root-password", "pw")
	const literals = "SELECT @@collation_connection, @@character_set_connection, @@collation_server, 'a'='A', 'a'='ａ', 'ア'='あ', '1'='１'"
	type result struct {
		coll, cs, server     string
		aA, aFull, kana, one int
	}
	for _, c := range []struct {
		collation string // go-sql-driver が接続のときに伝える照合順序
		want      result
	}{
		{"utf8mb4_0900_ai_ci", result{"utf8mb4_0900_ai_ci", "utf8mb4", "utf8mb4_0900_ai_ci", 1, 1, 1, 1}},
		{"utf8mb4_general_ci", result{"utf8mb4_general_ci", "utf8mb4", "utf8mb4_0900_ai_ci", 1, 0, 0, 0}},
		{"utf8mb4_unicode_ci", result{"utf8mb4_unicode_ci", "utf8mb4", "utf8mb4_0900_ai_ci", 1, 1, 1, 1}},
		{"utf8mb4_unicode_520_ci", result{"utf8mb4_unicode_520_ci", "utf8mb4", "utf8mb4_0900_ai_ci", 1, 1, 1, 1}},
		{"utf8mb4_bin", result{"utf8mb4_bin", "utf8mb4", "utf8mb4_0900_ai_ci", 0, 0, 0, 0}},
		{"utf8mb3_general_ci", result{"utf8mb4_general_ci", "utf8mb4", "utf8mb4_0900_ai_ci", 1, 0, 0, 0}},
		{"latin1_swedish_ci", result{"utf8mb4_0900_ai_ci", "utf8mb4", "utf8mb4_0900_ai_ci", 1, 1, 1, 1}},
	} {
		t.Run(c.collation, func(t *testing.T) {
			db, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress?collation="+c.collation)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var got result
			if err := db.QueryRow(literals).Scan(&got.coll, &got.cs, &got.server, &got.aA, &got.aFull, &got.kana, &got.one); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}

	// データベースを選ばない接続の collation_database も、MySQL と同じくサーバの既定
	nodb, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/")
	if err != nil {
		t.Fatal(err)
	}
	defer nodb.Close()
	var dbColl string
	if err := nodb.QueryRow("SELECT @@collation_database").Scan(&dbColl); err != nil || dbColl != "utf8mb4_0900_ai_ci" {
		t.Errorf("@@collation_database without a database = %q, %v", dbColl, err)
	}

	// SET NAMES の後は、今までどおりその照合順序（WordPress が接続のたびに送る形）
	db, err := sql.Open("mysql", "root:pw@tcp("+srv.addr+")/wordpress?collation=utf8mb4_general_ci")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci"); err != nil {
		t.Fatal(err)
	}
	var coll string
	var kana int
	if err := db.QueryRow("SELECT @@collation_connection, 'ア'='あ'").Scan(&coll, &kana); err != nil {
		t.Fatal(err)
	}
	if coll != "utf8mb4_unicode_520_ci" || kana != 1 {
		t.Errorf("after SET NAMES: %s, 'ア'='あ' = %d", coll, kana)
	}
}
