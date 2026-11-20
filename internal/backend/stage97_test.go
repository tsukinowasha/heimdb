package backend

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// SQL_CALC_FOUND_ROWS の LIMIT が返さない行は、インデックスのエントリで数え、本体の行を引かない（docs/plan.md の段階 97）。
// FOUND_ROWS() は同じ条件の COUNT(*) と同じ。ブランチの層（親の行、ブランチで消した行と変えた行）、トランザクションの
// 中の変更、AS OF でも同じ。
func TestStage97FoundRows(t *testing.T) {
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	var vals []string
	for i := 1; i <= 700; i++ {
		typ, status := "post", "publish"
		if i%7 == 0 {
			typ = "page"
		}
		if i%5 == 0 {
			status = "draft"
		}
		vals = append(vals, fmt.Sprintf("(%d, 't%d', '%s', '', '', '', '', '%s', '%s', '2025-01-01' + INTERVAL %d MINUTE)",
			i+1, i, strings.Repeat("本文", 200), typ, status, i%300))
	}
	mustExec(t, mainDB, "INSERT INTO wp_posts (ID, post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered, post_type, post_status, post_date) VALUES "+strings.Join(vals, ", "))
	mustExec(t, mainDB, "ANALYZE TABLE wp_posts")
	at := markLocal(t)
	mustExec(t, mainDB, "CALL heimdb_branch_create('stg97')")
	mustExec(t, mainDB, "DELETE FROM wp_posts WHERE ID % 11 = 0")
	branch := openBranch(t, dsn, "stg97")
	defer branch.Close()
	mustExec(t, branch, "DELETE FROM wp_posts WHERE ID % 13 = 0")
	mustExec(t, branch, "UPDATE wp_posts SET post_status = 'draft' WHERE ID % 17 = 0")
	mustExec(t, branch, "UPDATE wp_posts SET post_status = 'publish' WHERE ID % 19 = 0")

	const where = "post_type = 'post' AND post_status = 'publish'"
	const ranged = "post_type = 'post' AND post_status >= 'private' AND post_date >= '2025-01-01 02:00:00'"
	list := "SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts%s WHERE 1=1 AND " + where + " ORDER BY wp_posts.post_date DESC, wp_posts.ID DESC LIMIT %s"
	cases := []struct {
		q     string
		read  int    // 引いた本体の行の数（-1 なら、数えるのに本体を引く）
		count string // FOUND_ROWS() と比べる COUNT(*) の条件（空なら where）
	}{
		{fmt.Sprintf(list, "", "0, 20"), 20, ""},
		{fmt.Sprintf(list, "", "20, 20"), 40, ""},
		{fmt.Sprintf(list, "", "300, 20"), -3, ""}, // 先読み（256 行から。まとめて復号する）の後で数える
		{fmt.Sprintf(list, "", "0"), 0, ""},
		{fmt.Sprintf(list, "", "5000, 20"), -2, ""}, // 全部を返す前に終わる（数えるところに来ない）
		{"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE " + where + " ORDER BY post_date ASC LIMIT 10", 0, ""}, // covered
		{"SELECT SQL_CALC_FOUND_ROWS ID, post_title FROM wp_posts WHERE " + where + " ORDER BY post_date DESC LIMIT 10", 10, ""},
		// 範囲で絞れない条件があると、今までどおり行を読んで数える
		{"SELECT SQL_CALC_FOUND_ROWS ID FROM wp_posts WHERE " + where + " AND post_title LIKE 't1%' ORDER BY post_date DESC LIMIT 5", -1, where + " AND post_title LIKE 't1%'"},
		// 区間がちょうど表さない範囲（後ろの列の条件）は、エンジンが Filter を残すので、今までどおり行を読んで数える
		{"SELECT SQL_CALC_FOUND_ROWS wp_posts.* FROM wp_posts WHERE " + ranged + " ORDER BY post_status, post_date LIMIT 5", -1, ranged},
		{"SELECT SQL_CALC_FOUND_ROWS CONCAT(ID, post_title) FROM wp_posts WHERE " + where + " ORDER BY post_date DESC LIMIT 5", 5, ""},
	}
	ctx := context.Background()
	check := func(db *sql.DB, name string, asOf string) {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		from := ""
		if asOf != "" {
			from = " AS OF '" + asOf + "'"
		}
		for _, c := range cases {
			q := strings.Replace(c.q, "FROM wp_posts ", "FROM wp_posts"+from+" ", 1)
			q = strings.Replace(q, "FROM wp_posts%s", "FROM wp_posts"+from, 1)
			if name == "main" && asOf == "" {
				if plan := explain(t, db, q); !strings.Contains(plan, "IndexedTableAccess(wp_posts)") || !strings.Contains(plan, "wp_posts.post_type,wp_posts.post_status,wp_posts.post_date") || strings.Contains(plan, "TopN") {
					t.Fatalf("%s does not read type_status_date in order:\n%s", q, plan)
				}
			}
			read, decoded := pkRowsRead.Load(), rowsDecoded.Load()
			rows, err := conn.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			read, decoded = pkRowsRead.Load()-read, rowsDecoded.Load()-decoded
			var found int
			if err := conn.QueryRowContext(ctx, "SELECT FOUND_ROWS()").Scan(&found); err != nil {
				t.Fatal(err)
			}
			cond := c.count
			if cond == "" {
				cond = where
			}
			var want int
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_posts"+from+" WHERE "+cond).Scan(&want); err != nil {
				t.Fatal(err)
			}
			if found != want {
				t.Errorf("%s %s: FOUND_ROWS() = %d, want %d", name, q, found, want)
			}
			switch {
			case c.read == -1:
				if read < int64(want) {
					t.Errorf("%s %s: read %d rows, want at least %d", name, q, read, want)
				}
			case c.read == -3:
				if read != 320 {
					t.Errorf("%s %s: read %d rows, want 320", name, q, read)
				}
			case c.read == -2:
				if n != 0 {
					t.Errorf("%s %s: %d rows", name, q, n)
				}
			case read != int64(c.read) || decoded != int64(c.read):
				t.Errorf("%s %s: read %d rows and decoded %d, want %d", name, q, read, decoded, c.read)
			}
		}
	}
	check(mainDB, "main", "")
	check(branch, "branch", "")
	check(mainDB, "as of", at)

	// トランザクションの中で、インデックスの列を変えた行と消した行
	conn, err := branch.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{"BEGIN", "UPDATE wp_posts SET post_status = 'draft' WHERE ID % 23 = 0", "DELETE FROM wp_posts WHERE ID % 29 = 0", "UPDATE wp_posts SET post_status = 'publish' WHERE ID % 31 = 0"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var count, found int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_posts WHERE "+where).Scan(&count); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(list, "", "0, 20"))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()
	if err := conn.QueryRowContext(ctx, "SELECT FOUND_ROWS()").Scan(&found); err != nil {
		t.Fatal(err)
	}
	if found != count {
		t.Errorf("in a transaction: FOUND_ROWS() = %d, want %d", found, count)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	// 射影に相関サブクエリのある文（式の射影なので数えさせず、今までどおり Next で数える）。サブクエリは同じインデックスを
	// 引くので、走査を使い回す。
	q := "SELECT SQL_CALC_FOUND_ROWS p.ID, (SELECT COUNT(*) FROM wp_posts q WHERE q.post_type = 'post' AND q.post_status = 'publish' AND q.post_date < p.post_date) FROM wp_posts p WHERE p.post_type = 'post' AND p.post_status = 'publish' ORDER BY p.post_date DESC LIMIT 3"
	conn2, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	var want int
	if err := conn2.QueryRowContext(ctx, "SELECT COUNT(*) FROM wp_posts WHERE "+where).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if rows, err = conn2.QueryContext(ctx, q); err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()
	if err := conn2.QueryRowContext(ctx, "SELECT FOUND_ROWS()").Scan(&found); err != nil || found != want {
		t.Errorf("%s: FOUND_ROWS() = %d, %v, want %d", q, found, err, want)
	}
}

// 使い回す走査を、読み終えて置き場所に戻した後にもう読まない（docs/plan.md の段階 97。段階 83 からの不具合）。同じ
// インデックスを引く semi join で、外側の走査が尽きて置き場所に戻ると、内側の検索がそれを取って区間を移し、外側の次の
// Next が内側の区間のエントリを自分の行として読んで、終わらなかった。
func TestStage97PooledScanAfterEnd(t *testing.T) {
	mainDB, _, stop := branchServer(t)
	defer stop()
	var vals []string
	for i := 1; i <= 100; i++ {
		vals = append(vals, fmt.Sprintf("(%d, 't', '', '', '', '', '', 'post', 'publish', '2025-01-01' + INTERVAL %d MINUTE)", i+1, i))
	}
	mustExec(t, mainDB, "INSERT INTO wp_posts (ID, post_title, post_content, post_excerpt, to_ping, pinged, post_content_filtered, post_type, post_status, post_date) VALUES "+strings.Join(vals, ", "))
	mustExec(t, mainDB, "ANALYZE TABLE wp_posts")
	q := "SELECT p.ID FROM wp_posts p WHERE p.post_type = 'post' AND p.post_status = 'publish' AND EXISTS (SELECT 1 FROM wp_posts q WHERE q.post_type = 'post' AND q.post_status = 'publish' AND q.post_date < p.post_date) ORDER BY p.post_date"
	if plan := explain(t, mainDB, q); !strings.Contains(plan, "SemiLookupJoin") {
		t.Fatalf("not a semi lookup join:\n%s", plan)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := mainDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("row %d twice", id)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 100 || seen[1] { // 最初の記事（seedWordPress。日時が 0）より前の記事はない
		t.Errorf("%d rows, want 100 without the first post", len(seen))
	}
}

// 閉じた走査を 2 回閉じても、置き場所に重ねて戻さない（ほかの検索が取った後なら、2 つの検索が同じ走査を読む）。
func TestStage97IntervalScanClosesOnce(t *testing.T) {
	pool := &scanPool{idle: map[scanKey][]*scanHandle{}}
	h := &scanHandle{sc: &store.Scan{}, pool: pool}
	s := intervalScan{h: h}
	s.close()
	if len(pool.idle[h.key]) != 1 {
		t.Fatalf("%d idle scans after closing", len(pool.idle[h.key]))
	}
	pool.idle[h.key] = nil // ほかの検索が取った
	s.close()
	if _, _, ok, err := s.next(); ok || err != nil {
		t.Errorf("read a closed scan: %v, %v", ok, err)
	}
	if len(pool.idle[h.key]) != 0 {
		t.Errorf("a scan taken by another lookup went back to the pool")
	}
}
