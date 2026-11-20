package backend

import (
	"database/sql"
	"testing"
)

// 1 行しか返さない文（集計、主キーの等号）の EXPLAIN PLAN と EXPLAIN が、計画の複数の行を返せる（段階 48。
// go-mysql-server は中の文の「最大 1 行」の印をそのまま使い、max1Row のエラーにしていた）。
func TestExplainOfSingleRowQuery(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE c (id bigint NOT NULL PRIMARY KEY, approved varchar(20) NOT NULL, type varchar(20) NOT NULL, user_id bigint NOT NULL, KEY approved (approved))",
		"INSERT INTO c VALUES (1, '1', 'comment', 1), (2, '0', 'comment', 1), (3, '1', 'note', 2)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM c WHERE ( ( approved = '0' OR approved = '1' ) ) AND type NOT IN ('note') AND user_id = 1",
		"SELECT COUNT(*) FROM c WHERE approved = '1'",
		"SELECT * FROM c WHERE id = 1",
	} {
		for _, prefix := range []string{"EXPLAIN PLAN ", "EXPLAIN "} {
			rows, err := db.Query(prefix + q)
			n := 0
			if err == nil {
				for rows.Next() {
					n++
				}
				err = rows.Err()
				rows.Close()
			}
			if err != nil || n == 0 {
				t.Errorf("%s%s: %d rows, %v", prefix, q, n, err)
			}
		}
		// 文そのものは今までどおり 1 行。
		rows, err := db.Query(q)
		n := 0
		if err == nil {
			for rows.Next() {
				n++
			}
			err = rows.Err()
			rows.Close()
		}
		if err != nil || n != 1 {
			t.Errorf("%s: %d rows, %v", q, n, err)
		}
	}
}
