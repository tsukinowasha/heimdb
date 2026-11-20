package backend

import (
	"strings"
	"testing"
)

// 主キーの等号の文（1 行しか返さない）を含む UNION、EXCEPT が、すべての行を返す（docs/plan.md の段階 71。
// go-mysql-server は等号の文の「最大 1 行」の印を文全体に使い、2 行目で max1Row のエラーにしていた）。
func TestSetOpWithPointLookup(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE q (id int NOT NULL PRIMARY KEY, b varchar(10) NOT NULL)")
	mustExec(t, m, "INSERT INTO q VALUES (1, 'x'), (2, 'y'), (3, 'z')")
	for q, want := range map[string]string{
		"SELECT b FROM q WHERE id = 1 UNION ALL SELECT b FROM q WHERE id > 1":                                              "x,y,z",
		"SELECT b FROM q WHERE id > 1 UNION ALL SELECT b FROM q WHERE id = 1":                                              "y,z,x",
		"SELECT b FROM q WHERE id = 1 UNION SELECT b FROM q WHERE id = 2":                                                  "x,y",
		"SELECT b FROM q WHERE id = 1 UNION ALL SELECT 'w'":                                                                "x,w",
		"SELECT b FROM q EXCEPT SELECT b FROM q WHERE id = 1 ORDER BY b":                                                   "y,z",
		"(SELECT b FROM q WHERE id = 1) UNION ALL (SELECT b FROM q WHERE id = 2) UNION ALL (SELECT b FROM q WHERE id = 3)": "x,y,z",
		"SELECT b FROM q WHERE id = 1 UNION ALL SELECT b FROM q WHERE id = 2 ORDER BY b DESC LIMIT 5":                      "y,x",
		"SELECT b FROM q WHERE id = 1":                                                                                     "x",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	// 準備した文（引数のある文。ドライバはサーバ側で準備する）でも同じ。
	rows, err := m.Query("SELECT b FROM q WHERE id = ? UNION ALL SELECT b FROM q WHERE id = ?", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	if err := rows.Err(); err != nil || strings.Join(got, ",") != "x,y" {
		t.Errorf("prepared: %v %v", got, err)
	}
}
