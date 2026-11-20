package backend

import (
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// GC の後も、境界以降の AS OF、ブランチの差分、復元、マージが GC の前と同じ結果になる。境界より前の AS OF はエラー。
func TestGCWithWordPressActivity(t *testing.T) {
	var st *store.Store
	mainDB, dsn, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')")
	for i := 0; i < 5; i++ {
		mustExec(t, mainDB, "INSERT INTO wp_options (option_name, option_value) VALUES ('_transient_x', 'v') ON DUPLICATE KEY UPDATE option_value = CONCAT(option_value, 'v')")
	}
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()
	mustExec(t, staging, "UPDATE wp_options SET option_value = 'Staging' WHERE option_name = 'blogname'")
	mustExec(t, mainDB, "CREATE TABLE wp_old (id bigint unsigned NOT NULL PRIMARY KEY)")
	mustExec(t, mainDB, "INSERT INTO wp_old VALUES (1)")
	mustExec(t, mainDB, "DROP TABLE wp_old")
	mustExec(t, mainDB, "TRUNCATE wp_terms")
	mustExec(t, mainDB, "INSERT INTO wp_terms (term_id, name, slug) VALUES (1, 'a', 'a')")
	// 主キーのない表（段階 72）
	mustExec(t, mainDB, "CREATE TABLE lg (ip varchar(39) NOT NULL, code int NOT NULL)")
	mustExec(t, mainDB, "INSERT INTO lg VALUES ('a', 1), ('b', 2), ('a', 1)")
	mustExec(t, mainDB, "UPDATE lg SET code = 3 WHERE ip = 'b'")
	mustExec(t, mainDB, "DELETE FROM lg WHERE ip = 'a' LIMIT 1")

	tOld := markLocal(t)
	time.Sleep(30 * time.Millisecond)
	tKeep := markLocal(t)
	cut := time.Now()
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'Prod2' WHERE option_name = 'blogname'")
	mustExec(t, mainDB, "ALTER TABLE wp_links ADD COLUMN extra int NOT NULL DEFAULT 0")
	mustExec(t, mainDB, "INSERT INTO lg VALUES ('c', 4)")
	mustExec(t, mainDB, "DELETE FROM lg WHERE ip = 'a'")
	mustExec(t, mainDB, "ALTER TABLE lg ADD COLUMN extra int NOT NULL DEFAULT 0")

	tables := []string{"wp_options", "wp_terms", "wp_links", "wp_posts", "lg"}
	snapshot := func() map[string]string {
		m := map[string]string{}
		for _, tb := range tables {
			m[tb] = tableState(t, mainDB, tb, "")
			m[tb+"@keep"] = tableState(t, mainDB, tb, tKeep)
		}
		m["diff"] = strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")
		m["preview"] = strings.Join(diffRows(t, mainDB, "CALL heimdb_restore_preview('main', '"+tKeep+"')"), "\n")
		return m
	}
	before := snapshot()

	// 境界を tOld と tKeep の間に置く。
	stats, err := st.GC(time.Since(cut) + 15*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Versions == 0 || stats.Tables == 0 {
		t.Errorf("GC removed nothing: %+v", stats)
	}
	after := snapshot()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s after GC:\n%s\nwant:\n%s", k, after[k], v)
		}
	}
	for _, q := range []string{
		"SELECT * FROM wp_options AS OF '" + tOld + "'",
		"CALL heimdb_restore('main', '" + tOld + "')",
		"CALL heimdb_branch_create('old', 'main', '" + tOld + "')",
	} {
		if _, err := mainDB.Exec(q); err == nil || !strings.Contains(err.Error(), "retention boundary") {
			t.Errorf("%s: %v", q, err)
		}
	}
	// 境界以降への復元と、ブランチのマージは動く。
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tKeep+"')")
	if got := one(t, mainDB, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Prod" {
		t.Errorf("blogname after restore: %s", got)
	}
	diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := one(t, mainDB, "SELECT option_value FROM wp_options WHERE option_name = 'blogname'"); got != "Staging" {
		t.Errorf("blogname after merge: %s", got)
	}
	// heimdb_gc は保持期間 0 ならエラー（サーバの既定は -retention で決める）。
	if _, err := mainDB.Exec("CALL heimdb_gc()"); err == nil {
		t.Error("heimdb_gc with retention 0 should fail")
	}
}

// AS OF で読む時刻は文の間だけ GC の境界を止め、文が終われば解ける。
func TestGCAsOfPinIsReleased(t *testing.T) {
	var st *store.Store
	mainDB, _, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	tAt := markLocal(t)
	mustExec(t, mainDB, "UPDATE wp_options SET option_value = 'x' WHERE option_name = 'blogname'")
	if got := one(t, mainDB, "SELECT option_value FROM wp_options AS OF '"+tAt+"' WHERE option_name = 'blogname'"); got != "Prod" {
		t.Fatalf("AS OF: %s", got)
	}
	last := st.LastCommitted() // GC は境界を記録するコミットで LastCommitted を進めるので、先に取る
	stats, err := st.GC(time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Boundary != last {
		t.Errorf("boundary %d held back after the AS OF statement ended (last commit %d)", stats.Boundary, last)
	}
}
