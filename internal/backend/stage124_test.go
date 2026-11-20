package backend

import (
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 段階 124: 結果の行を、文を受けたゴルーチンで読んで送る（docs/stages/121-130.md）。HeimDB の上で、組（128 行）を
// またぐ結果、途中のエラー、KILL QUERY、実行時間の上限、同時の接続が前と同じに動くことを確かめる。

// stage124Rows は、q の結果の行を「列,列」で返す。途中のエラーは、それまでの行と一緒に返す。
func stage124Rows(db *sql.DB, q string) ([]string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return out, err
		}
		s := ""
		for i, v := range vals {
			if i > 0 {
				s += ","
			}
			s += v.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func stage124Setup(t *testing.T) (addr string, db *sql.DB, stop func()) {
	t.Helper()
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	db = openRoot(t, addr)
	mustExec(t, db, "CREATE TABLE r (i INT PRIMARY KEY, v VARCHAR(40))")
	mustExec(t, db, `INSERT INTO r WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
		SELECT i, CONCAT('{"a": ', i, '}') FROM n`)
	mustExec(t, db, `UPDATE r SET v = 'not json' WHERE i = 500`)
	return addr, db, func() {
		db.Close()
		srv.Close()
		st.Close()
	}
}

// TestStage124ResultRows は、組の境目の前後の行の数（0、1、127、128、129、256、1000）で、行の値と順と列が前と同じ
// こと、途中の行のエラーで止まり、接続が使えたままなことを確かめる。
func TestStage124ResultRows(t *testing.T) {
	_, db, stop := stage124Setup(t)
	defer stop()
	for _, n := range []int{0, 1, 127, 128, 129, 256, 1000} {
		got, err := stage124Rows(db, fmt.Sprintf("SELECT i, v FROM r WHERE i <> 500 ORDER BY i LIMIT %d", n))
		if err != nil {
			t.Fatalf("LIMIT %d: %v", n, err)
		}
		want := min(n, 999)
		if len(got) != want {
			t.Fatalf("LIMIT %d: %d rows", n, len(got))
		}
		for k, row := range got {
			i := k + 1
			if i >= 500 {
				i++
			}
			if row != fmt.Sprintf(`%d,{"a": %d}`, i, i) {
				t.Fatalf("LIMIT %d: row %d is %q", n, k, row)
			}
		}
	}
	rows, err := db.Query("SELECT i, v FROM r WHERE i < 0")
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	if rows.Next() || len(cols) != 2 {
		t.Errorf("empty result: columns %v", cols)
	}
	rows.Close()

	// 500 行目の値は JSON でない（エラー 3141）。最初の組（128 行）を送る前のエラーは 3141 で返り、接続はそのまま。
	var id1, id2 int
	if err := db.QueryRow("SELECT CONNECTION_ID()").Scan(&id1); err != nil {
		t.Fatal(err)
	}
	got, err := stage124Rows(db, "SELECT i, JSON_EXTRACT(v, '$.a') FROM r WHERE i > 400 ORDER BY i")
	if mysqlErrNo(err) != 3141 || len(got) != 0 {
		t.Errorf("JSON_EXTRACT in the first batch: %d rows, %v", len(got), err)
	}
	if err := db.QueryRow("SELECT CONNECTION_ID()").Scan(&id2); err != nil || id2 != id1 {
		t.Errorf("after the error: connection %d, was %d: %v", id2, id1, err)
	}
	// 組を送った後のエラーは、送った組の後で止まる。vitess は接続を閉じる（前からの違い。docs/differences.md の
	// 「そのほか」）。
	got, err = stage124Rows(db, "SELECT i, JSON_EXTRACT(v, '$.a') FROM r ORDER BY i")
	if err == nil || len(got) != 3*128 {
		t.Errorf("JSON_EXTRACT after 3 batches: %d rows, %v", len(got), err)
	}
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Errorf("after the error: %v", err)
	}
}

// TestStage124KillAndTimeoutWhileSending は、行を送っている途中の文を、KILL QUERY（1317）と実行時間の上限（3024）で
// 止められることを確かめる。
func TestStage124KillAndTimeoutWhileSending(t *testing.T) {
	addr, db, stop := stage124Setup(t)
	defer stop()
	const slow = "SELECT i, SLEEP(0.01) FROM r ORDER BY i" // 1000 行で 10 秒
	var id int
	if err := db.QueryRow("SELECT CONNECTION_ID()").Scan(&id); err != nil {
		t.Fatal(err)
	}
	k := openRoot(t, addr)
	defer k.Close()
	time.AfterFunc(500*time.Millisecond, func() { k.Exec(fmt.Sprintf("KILL QUERY %d", id)) })
	start := time.Now()
	got, err := stage124Rows(db, slow)
	// 選ぶ列の SLEEP の取り消しは、1105 context canceled になる（前からの違い。docs/differences.md の「誤りの番号と
	// 警告」）。
	if mysqlErrNo(err) != 1105 || time.Since(start) > 5*time.Second || len(got) >= 1000 {
		t.Errorf("KILL QUERY: %d rows, %v after %s", len(got), err, time.Since(start))
	}
	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Errorf("after KILL QUERY: %v", err)
	}

	mustExec(t, db, "SET SESSION max_execution_time = 300")
	start = time.Now()
	got, err = stage124Rows(db, slow)
	if mysqlErrNo(err) != 3024 || time.Since(start) > 5*time.Second || len(got) >= 1000 {
		t.Errorf("max_execution_time: %d rows, %v after %s", len(got), err, time.Since(start))
	}
	mustExec(t, db, "SET SESSION max_execution_time = 0")
	if err := db.QueryRow("SELECT COUNT(*) FROM r").Scan(&one); err != nil || one != 1000 {
		t.Errorf("after the timeout: %d, %v", one, err)
	}
}

// TestStage124ConcurrentResults は、8 つの接続が同時に組をまたぐ結果を読んでも、行の数が正しいことを確かめる。
func TestStage124ConcurrentResults(t *testing.T) {
	addr, db, stop := stage124Setup(t)
	defer stop()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			c := openRoot(t, addr)
			defer c.Close()
			for i := 0; i < 20; i++ {
				n := 100 + (g*37+i*53)%900
				got, err := stage124Rows(c, fmt.Sprintf("SELECT i, v FROM r WHERE i <= %d", n))
				if err != nil || len(got) != n {
					errs <- fmt.Errorf("%d rows of %d: %v", len(got), n, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	_ = db
}
