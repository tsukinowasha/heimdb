package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// lookup JOIN が右のセカンダリインデックスを左の行ごとに引くとき、書かない文では行の読み手を開かず、トランザクションの
// PointReader を使う（docs/plan.md の段階 99）。結果は、読み手を引くたびに開く文（WITH で始まる文）と同じ。
// ブランチ、AS OF、トランザクションの中で書いた後、同じ文で主キーの点と一緒に引く形も同じ。
func TestStage99SharedReader(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	var posts, metas []string
	for i := 2; i <= 301; i++ {
		posts = append(posts, fmt.Sprintf("(%d, 't%d', '', '', '', '', '')", i, i))
		for j := range 3 {
			metas = append(metas, fmt.Sprintf("(%d, 'k', 'v%d-%d')", i, i, j))
		}
		metas = append(metas, fmt.Sprintf("(%d, 'other%d', 'x')", i, i%7))
	}
	mustExec(t, mainDB, "INSERT INTO wp_posts (ID, post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered) VALUES "+strings.Join(posts, ", "))
	mustExec(t, mainDB, "INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES "+strings.Join(metas, ", "))
	mustExec(t, mainDB, "ANALYZE TABLE wp_posts, wp_postmeta")
	at := markLocal(t)
	mustExec(t, mainDB, "CALL heimdb_branch_create('stg99')")
	mustExec(t, mainDB, "UPDATE wp_postmeta SET meta_value = 'main' WHERE post_id % 11 = 0 AND meta_key = 'k'")
	branch := openBranch(t, dsn, "stg99")
	defer branch.Close()
	mustExec(t, branch, "UPDATE wp_postmeta SET meta_value = 'branch' WHERE post_id % 13 = 0 AND meta_key = 'k'")
	mustExec(t, branch, "DELETE FROM wp_postmeta WHERE post_id % 17 = 0")

	// 記事を主キーの範囲で読み、記事ごとにメタを post_id で引く。2 つ目は記事の親（主キーの点）も同じ文で引く。
	joins := []string{
		"SELECT CONCAT(p.ID, ':', m.meta_value) FROM wp_posts%s p INNER JOIN wp_postmeta%s m ON m.post_id = p.ID WHERE p.ID BETWEEN 10 AND 60 AND m.meta_value LIKE 'v%' ORDER BY p.ID, m.meta_id",
		"SELECT CONCAT(p.ID, ':', m.meta_value, ':', q.ID) FROM wp_posts%s p INNER JOIN wp_postmeta%s m ON m.post_id = p.ID INNER JOIN wp_posts%s q ON q.ID = p.ID WHERE p.ID BETWEEN 10 AND 60 AND m.meta_value LIKE 'v%' ORDER BY p.ID, m.meta_id",
	}
	query := func(q, asOf string) string {
		from := ""
		if asOf != "" {
			from = " AS OF '" + asOf + "'"
		}
		return strings.ReplaceAll(q, "%s", from)
	}
	check := func(db *sql.DB, name, asOf string, shared bool) {
		t.Helper()
		for _, j := range joins {
			q := query(j, asOf)
			if plan := explain(t, db, q); !strings.Contains(plan, "index: [wp_postmeta.post_id]") || !strings.Contains(plan, "keys: p.ID") {
				t.Fatalf("%s: %s does not look up meta by post_id:\n%s", name, q, plan)
			}
			before := readersOpened.Load()
			got := rowsOf(t, db, q)
			opened := readersOpened.Load() - before
			before = readersOpened.Load()
			want := rowsOf(t, db, "WITH c AS (SELECT 1) "+q)
			if readersOpened.Load() == before {
				t.Fatalf("%s: the WITH form shared the reader too, so it does not check the shared one", name)
			}
			if got != want || got == "" {
				t.Errorf("%s: %s\n got %.200s\nwant %.200s", name, q, got, want)
			}
			if shared && opened != 0 || !shared && opened == 0 {
				t.Errorf("%s: %s opened %d readers (shared %v)", name, q, opened, shared)
			}
		}
	}
	check(mainDB, "main", "", true)
	check(branch, "branch", "", true)
	check(mainDB, "as of", at, false)

	// トランザクションの中で、書いた後の SELECT は書いた値を読む（書くと PointReader を閉じ、次に引くときに作り直す）
	ctx := context.Background()
	conn, err := branch.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	q := query(joins[0], "")
	scan := func() string {
		var out []string
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}
	for _, s := range []string{"BEGIN", "SELECT 1"} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	first := scan()
	if _, err := conn.ExecContext(ctx, "UPDATE wp_postmeta SET meta_value = 'vtxn' WHERE post_id = 50 AND meta_key = 'k'"); err != nil {
		t.Fatal(err)
	}
	second := scan()
	if strings.Contains(first, "50:vtxn") || !strings.Contains(second, "50:vtxn") || strings.Contains(second, "50:v50") {
		t.Errorf("transaction: before %q, after %q", first, second)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	// 書く文の JOIN は、引くたびに読み手を開く（文の途中の書き込みで PointReader が閉じるため）。ブランチで 17 と 34 の
	// メタを消したので、31 件のうち 29 件。
	before := readersOpened.Load()
	res, err := branch.Exec("UPDATE wp_posts p INNER JOIN wp_postmeta m ON m.post_id = p.ID SET p.post_excerpt = m.meta_value WHERE p.ID BETWEEN 10 AND 40 AND m.meta_key = 'k'")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 29 {
		t.Errorf("UPDATE JOIN changed %d rows, want 29", n)
	}
	if got := one(t, branch, "SELECT COUNT(*) FROM wp_posts WHERE post_excerpt LIKE 'v%' OR post_excerpt IN ('branch', 'main')"); got != "29" {
		t.Errorf("UPDATE JOIN wrote %s excerpts, want 29", got)
	}
	if readersOpened.Load() == before {
		t.Error("UPDATE JOIN shared the transaction's reader")
	}
}

// 走査と行の読み手を文の中で使い回すのは、SELECT とかっこで始まる文だけ。WITH の後ろには UPDATE や DELETE が続きうる。
func TestReadOnlyQuery(t *testing.T) {
	for q, want := range map[string]bool{
		"SELECT 1":                              true,
		" /* c */ select * FROM wp_posts":       true,
		"(SELECT 1) UNION (SELECT 2)":           true,
		"WITH c AS (SELECT 1) SELECT * FROM c":  false,
		"WITH c AS (SELECT 1) UPDATE t SET a=1": false,
		"UPDATE wp_posts SET post_title = 'x'":  false,
		"INSERT INTO t SELECT 1":                false,
		"SELECTED":                              false,
		"EXPLAIN SELECT 1":                      false,
		"CALL heimdb_status()":                  false,
	} {
		if got := readOnlyQuery(q); got != want {
			t.Errorf("readOnlyQuery(%q) = %v, want %v", q, got, want)
		}
	}
}
