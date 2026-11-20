package backend

import (
	"database/sql"
	"strings"
	"testing"
)

// dumpBranch は、ブランチのすべてのテーブルの定義と行を文字列にする（マージの前後を比べる）。
func dumpBranch(t *testing.T, db *sql.DB) string {
	t.Helper()
	var out []string
	for _, name := range diffRows(t, db, "SHOW TABLES") {
		out = append(out, diffRows(t, db, "SHOW CREATE TABLE "+name)...)
		out = append(out, diffRows(t, db, "SELECT * FROM "+name)...)
	}
	return strings.Join(out, "\n")
}

// lastRow は、結果の最後の行を返す。
func lastRow(rows []string) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[len(rows)-1]
}

// stageChanges は、ステージングで行、列、インデックス、新しいテーブルを変える。
func stageChanges(t *testing.T, staging *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"UPDATE wp_posts SET post_content = 'about v2' WHERE ID = 2",
		insertPost + "(NULL, 1, 'Pricing', 'publish', 'page', 'p', '', '', '', '')",
		"INSERT INTO wp_term_relationships (object_id, term_taxonomy_id) VALUES (10000000000, 1)",
		"UPDATE wp_options SET option_value = 'Staged' WHERE option_name = 'blogname'",
		"ALTER TABLE wp_links ADD COLUMN extra int",
		"ALTER TABLE wp_links ADD INDEX extra_idx (extra)",
		"CREATE TABLE wp_custom (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_custom (v) VALUES ('a')",
	} {
		mustExec(t, staging, q)
	}
}

// マージの結果の最後の行の文を実行すると、親はマージの前と同じに戻る（段階 44）。
func TestMergeUndo(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	stageChanges(t, staging)
	before := dumpBranch(t, mainDB)

	rows := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	undo := lastRow(rows)
	const prefix = "*|undo|0|0|0|0|0|0|0|CALL heimdb_restore('main', '"
	if !strings.HasPrefix(undo, prefix) {
		t.Fatalf("no undo row:\n%s", strings.Join(rows, "\n"))
	}
	stmt := strings.TrimPrefix(undo, "*|undo|0|0|0|0|0|0|0|")
	// マージが書いたテーブルだけ（記事、ターム、件数を数え直した term_taxonomy、rewrite_rules を空にした options、
	// 列を足した links、新しいテーブル）。ユーザーやコメントは入らない。
	if want := "', 'wp_custom,wp_links,wp_options,wp_posts,wp_term_relationships,wp_term_taxonomy')"; !strings.HasSuffix(stmt, want) {
		t.Errorf("undo tables: %s", stmt)
	}
	merged := dumpBranch(t, mainDB)
	if merged == before {
		t.Fatal("merge changed nothing")
	}
	diffRows(t, mainDB, stmt)
	if after := dumpBranch(t, mainDB); after != before {
		t.Errorf("undo did not restore the parent:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// 戻した後にもう一度マージすると、同じ変更が入る（マージの基準も戻る）。
	again := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if got := dumpBranch(t, mainDB); got != merged {
		t.Errorf("merge after undo differs:\n%s\nfirst merge:\n%s\nagain:\n%s", strings.Join(again, "\n"), merged, got)
	}
	// 何も書かないマージは、戻すための文を返さない。
	if rows := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')"); strings.Contains(strings.Join(rows, "\n"), "undo") {
		t.Errorf("merge without changes returned an undo row:\n%s", strings.Join(rows, "\n"))
	}
}

// 空打ちは実際のマージと同じ結果を返し、親、解決の記録、マージの起点を変えない（段階 44）。
func TestMergeDryRun(t *testing.T) {
	mainDB, staging, stop := mergeFixture(t)
	defer stop()
	stageChanges(t, staging)
	// 競合を 1 つ作って解決しておく。
	mustExec(t, staging, "UPDATE wp_posts SET post_title = 'Contact (staging)' WHERE ID = 3")
	mustExec(t, mainDB, "UPDATE wp_posts SET post_title = 'Contact (prod)' WHERE ID = 3")
	mustExec(t, mainDB, "CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=3', 'branch')")

	before := dumpBranch(t, mainDB)
	diffBefore := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n")

	dry := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging', 'dry_run')")
	if got := lastRow(dry); got != "*|dry run|0|0|0|0|0|0|0|nothing was committed" {
		t.Errorf("dry run rows:\n%s", strings.Join(dry, "\n"))
	}
	if dumpBranch(t, mainDB) != before {
		t.Error("dry run changed the parent")
	}
	if got := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging', '*')"), "\n"); got != diffBefore {
		t.Errorf("dry run changed the diff (merge base or resolutions):\nbefore:\n%s\nafter:\n%s", diffBefore, got)
	}

	// 実際のマージは、空打ちと同じ表の行（戻すための行を除く）を返す。
	real := diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')")
	if strings.Join(dry[:len(dry)-1], "\n") != strings.Join(real[:len(real)-1], "\n") {
		t.Errorf("dry run and merge differ:\ndry:\n%s\nmerge:\n%s", strings.Join(dry, "\n"), strings.Join(real, "\n"))
	}
	if got := one(t, mainDB, "SELECT post_title FROM wp_posts WHERE ID = 3"); got != "Contact (staging)" {
		t.Errorf("resolution lost by the dry run: %s", got)
	}

	if _, err := mainDB.Exec("CALL heimdb_branch_merge('staging', 'force')"); err == nil || !strings.Contains(err.Error(), "unknown merge mode") {
		t.Errorf("unknown mode: %v", err)
	}
}

// 差分の要約とマージの結果の detail に、親を書き直す行数と全行を比べる行数が出る（段階 44）。
func TestMergeWeight(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('db_version', '60717')",
		"INSERT INTO wp_postmeta (meta_id, post_id, meta_key, meta_value) VALUES (101, 1, 'a', '1'), (102, 1, 'b', '2'), (103, 1, 'c', '3')",
		"CREATE TABLE wp_plug (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_plug (v) VALUES ('a'), ('b')",
		"CREATE TABLE wp_plug2 (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v varchar(10))",
		"INSERT INTO wp_plug2 (v) VALUES ('a')",
	} {
		mustExec(t, mainDB, q)
	}
	mustExec(t, mainDB, "CALL heimdb_branch_create('staging')")
	staging := openBranch(t, dsn, "staging")
	defer staging.Close()

	// ブランチで列を足す（ブランチの側でも書き直したので全行を比べ、マージで親の 2 行を書き直す）。
	mustExec(t, staging, "ALTER TABLE wp_plug ADD COLUMN w int")
	// 本番で記事のメタとプラグインの表の列を変える。記事のメタは、親で追加された行を分岐の時点から探すので全行を比べる。
	// プラグインの表は、ブランチで変えた行だけを見るので重くならない。
	mustExec(t, mainDB, "ALTER TABLE wp_postmeta ADD COLUMN prod_only int")
	mustExec(t, mainDB, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'd', '4')")
	mustExec(t, mainDB, "ALTER TABLE wp_plug2 ADD COLUMN prod_only int")
	mustExec(t, staging, "UPDATE wp_plug2 SET v = 'b'")

	summary := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_diff('staging')"), "\n")
	for _, want := range []string{
		"wp_plug|changed|0|0|0|0|0|0|0|columns: +w; rewrite 2 rows, full compare 2 rows",
		// 分岐の時点の 3 行と、今の 4 行の主キーの和。マージで変えるものはない。
		"wp_postmeta|unchanged|0|0|0|0|0|0|0|full compare 4 rows",
		"wp_plug2|changed|0|1|0|0|0|0|0|",
	} {
		if !strings.Contains(summary+"\n", want+"\n") {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	// マージの結果は、書き直した行数を出し、変えるものがないテーブルは出さない。
	merged := strings.Join(diffRows(t, mainDB, "CALL heimdb_branch_merge('staging')"), "\n")
	if !strings.Contains(merged, "wp_plug|merged|0|0|0|0|0|0|0|columns: +w; rewrite 2 rows, full compare 2 rows\n") {
		t.Errorf("merge lacks the rewrite of wp_plug:\n%s", merged)
	}
	if strings.Contains(merged, "wp_postmeta") {
		t.Errorf("merge lists an unchanged table:\n%s", merged)
	}
}
