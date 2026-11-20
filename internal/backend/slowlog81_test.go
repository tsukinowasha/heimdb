package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// 段階 81: 遅い文のログの閾値（SET GLOBAL）、値を畳む、読んだ行とインデックス、待った時間、接続ごとの
// max_execution_time。

// slowLogEntries は、hook に残った遅い文のログ（項目つき）を返す。
func slowLogEntries(hook *logtest.Hook) []*logrus.Entry {
	var out []*logrus.Entry
	for _, e := range hook.AllEntries() {
		if strings.HasPrefix(e.Message, "slow query:") {
			out = append(out, e)
		}
	}
	return out
}

// 閾値は SET GLOBAL long_query_time と slow_query_log で、止めずに変わる。接続ごとの long_query_time は読まない。
func TestSlowQueryLogGlobalVariables(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	if err := SetSlowQuery(0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = SetSlowQuery(0)
		_ = SetSlowQuery(10 * time.Second)
		_ = SetSlowQuery(0)
	}()
	hook := logtest.NewGlobal()
	defer hook.Reset()
	db := openRoot(t, addr)
	defer db.Close()
	status := func() string {
		var v string
		if err := db.QueryRow("SELECT value FROM (CALL heimdb_status()) s WHERE name = 'slow_query'").Scan(&v); err == nil {
			return v
		}
		rows, err := db.Query("CALL heimdb_status()")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var value sql.NullString
			if err := rows.Scan(&name, &value); err != nil {
				t.Fatal(err)
			}
			if name == "slow_query" {
				return value.String
			}
		}
		return ""
	}
	if got := status(); got != "off" {
		t.Errorf("slow_query before = %q", got)
	}
	steps := []struct {
		set    string
		query  string
		logged bool
	}{
		{"", "SELECT SLEEP(0.3) AS step0", false},
		{"SET GLOBAL slow_query_log = ON, GLOBAL long_query_time = 0.2", "SELECT SLEEP(0.3) AS step1", true},
		{"SET GLOBAL long_query_time = 1", "SELECT SLEEP(0.3) AS step2", false},
		{"SET SESSION long_query_time = 0", "SELECT SLEEP(0.3) AS step3", false},
		{"SET GLOBAL long_query_time = 0.2, GLOBAL slow_query_log = OFF", "SELECT SLEEP(0.3) AS step4", false},
	}
	for i, s := range steps {
		if s.set != "" {
			mustExec(t, db, s.set)
		}
		if i == 1 {
			if got := status(); got != "200ms" {
				t.Errorf("slow_query after SET GLOBAL = %q", got)
			}
		}
		hook.Reset()
		mustExec(t, db, s.query)
		if logged := len(slowLogEntries(hook)) > 0; logged != s.logged {
			t.Errorf("%s then %s: logged = %v", s.set, s.query, logged)
		}
	}
}

// -slow-query-redact は文の値を ? に畳む。解析できない文は先頭の 2 語だけ。
func TestSlowQueryLogRedact(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	pro.SlowQueryRedact = true
	if err := SetSlowQuery(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	defer SetSlowQuery(0)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	db := openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "SELECT 'visitor search words' AS s, 12345 AS n FROM dual WHERE 7 IN (5, 6, 7)")
	var found bool
	for _, e := range slowLogEntries(hook) {
		if strings.Contains(e.Message, " n where") {
			found = true
			if e.Message != "slow query: select ? s, ? n where ? in (?)" { // vitess は文を書き直す（小文字、AS と FROM dual を省く）
				t.Errorf("not redacted: %s", e.Message)
			}
		}
	}
	if !found {
		for _, e := range hook.AllEntries() {
			t.Logf("%s %v", e.Message, e.Data)
		}
		t.Error("the statement was not logged")
	}
	if got := redactQuery("SELEC 'secret' FROM x"); got != "SELEC 'secret' ... (text hidden)" {
		t.Errorf("unparsable statement: %q", got)
	}
}

// 遅い文のログに、読んだ行（本体の行とインデックスのエントリ）と使ったインデックスが出る。
func TestSlowQueryLogRowsAndIndexes(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	db := openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, k INT, v VARCHAR(10), KEY k (k))")
	mustExec(t, db, "INSERT INTO t WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) SELECT i, i % 10, 'x' FROM n")
	if err := SetSlowQuery(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	defer SetSlowQuery(0)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	for _, c := range []struct {
		q       string
		rows    int64
		indexes string
	}{
		{"SELECT v FROM t WHERE v = 'x'", 100, "t.PRIMARY"},
		{"SELECT v FROM t WHERE id = 5", 1, "t.PRIMARY"},
		{"SELECT v FROM t WHERE k = 3", 20, "t.k"},  // エントリ 10 と本体の行 10
		{"SELECT id FROM t WHERE k = 3", 10, "t.k"}, // エントリだけで読む
	} {
		hook.Reset()
		mustExec(t, db, c.q)
		var e *logrus.Entry
		for _, x := range slowLogEntries(hook) {
			if strings.Contains(x.Message, c.q) {
				e = x
			}
		}
		if e == nil {
			t.Errorf("%s: not logged", c.q)
			continue
		}
		if e.Data["rows_examined"] != c.rows || e.Data["indexes"] != c.indexes {
			t.Errorf("%s: rows_examined=%v indexes=%v, want %d %s", c.q, e.Data["rows_examined"], e.Data["indexes"], c.rows, c.indexes)
		}
	}
}

// 書き込みのロックを待った時間と、統計を作るのを待った時間が、遅い文のログと指標に出る。
func TestSlowQueryLogWaits(t *testing.T) {
	addr, pro, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	a := openRoot(t, addr)
	defer a.Close()
	mustExec(t, a, "CREATE TABLE w (id INT PRIMARY KEY, k INT, KEY k (k))")
	mustExec(t, a, "INSERT INTO w VALUES (1, 1), (2, 2)")
	b := openRoot(t, addr)
	defer b.Close()
	if err := SetSlowQuery(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	defer SetSlowQuery(0)
	hook := logtest.NewGlobal()
	defer hook.Reset()

	// 表を初めて読む文は統計を作り、2 度目は作らない
	for i := range 2 {
		hook.Reset()
		mustExec(t, b, fmt.Sprintf("SELECT id FROM w WHERE k = %d", i+1))
		var waited bool
		for _, e := range slowLogEntries(hook) {
			_, waited = e.Data["stats_wait"]
		}
		if waited != (i == 0) {
			t.Errorf("statement %d: stats_wait logged = %v", i, waited)
		}
	}

	ctx := context.Background()
	ca, err := a.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	if _, err := ca.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.ExecContext(ctx, "UPDATE w SET k = 10 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = ca.ExecContext(ctx, "COMMIT")
	}()
	hook.Reset()
	mustExec(t, b, "UPDATE w SET k = 20 WHERE id = 2")
	var lock time.Duration
	for _, e := range slowLogEntries(hook) {
		if strings.Contains(e.Message, "k = 20") {
			if s, ok := e.Data["lock_wait"].(string); ok {
				lock, _ = time.ParseDuration(s)
			}
		}
	}
	if lock < 250*time.Millisecond {
		t.Errorf("lock_wait = %v", lock)
	}
	if got := time.Duration(pro.stats.lockWait.Load()); got < 250*time.Millisecond {
		t.Errorf("lock wait metric = %v", got)
	}
	if pro.stats.statsWait.Load() == 0 {
		t.Error("stats wait metric is 0")
	}
}

// 接続ごとの max_execution_time は、MySQL と同じく SELECT にだけ効き、エラー 3024 で止める。ブランチの上限より
// 長い値では外せない。
func TestSessionMaxExecutionTime(t *testing.T) {
	addr, _, st, srv := startShutdownServer(t, t.TempDir())
	defer func() {
		srv.Close()
		st.Close()
	}()
	db := openRoot(t, addr)
	defer db.Close()
	mustExec(t, db, "CREATE TABLE a (i INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO a WITH RECURSIVE n (i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100) SELECT i FROM n")
	mustExec(t, db, "CREATE TABLE r (c BIGINT)")
	mustExec(t, db, "SET SESSION max_execution_time = 100")
	const heavy = "SELECT COUNT(*) FROM a x, a y, a z WHERE x.i + y.i + z.i > 0"
	for _, c := range []struct {
		q       string
		stopped bool
	}{
		{heavy, true},
		{"WITH t AS (" + heavy + ") SELECT * FROM t", true},
		{"(" + heavy + ") UNION ALL (SELECT 1)", true},
		{"INSERT INTO r " + heavy, false},
		{"SET @v = (" + heavy + ")", false},
	} {
		_, err := db.Exec(c.q)
		var me *mysql.MySQLError
		stopped := err != nil && asMySQLError(err, &me) && me.Number == 3024
		if stopped != c.stopped || (!c.stopped && err != nil) {
			t.Errorf("%s: %v", c.q, err)
		}
	}
	mustExec(t, db, "SET SESSION max_execution_time = 0")
	if _, err := db.Exec(heavy); err != nil {
		t.Errorf("without the limit: %v", err)
	}
}

func asMySQLError(err error, target **mysql.MySQLError) bool {
	me, ok := err.(*mysql.MySQLError)
	if ok {
		*target = me
	}
	return ok
}

// 遅い文のログの時間は 1 µs 刻み（段階 84）。
func TestSlowQueryLogDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		312456 * time.Nanosecond:  "312µs",
		1234567 * time.Nanosecond: "1.235ms",
		1500 * time.Millisecond:   "1.5s",
		400 * time.Nanosecond:     "0s",
	} {
		if got := logDuration(d); got != want {
			t.Errorf("logDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
