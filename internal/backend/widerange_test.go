package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// 段階 77: ほぼ全件の範囲で、インデックスが文の使う列を持たないなら、表の全件を読む。ORDER BY、LIMIT、JOIN の文と、
// 範囲の狭い文、インデックスだけで読める文は今のまま。
func TestWideRangeReadsTable(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append([]string{wpSQLMode}, wpSchema()...) {
		mustExec(t, db, q)
	}
	// 承認済み 950 件、未承認 30 件、スパム 20 件。user_id = 1 は 10 件に 1 件（うち未承認 3 件、スパムなし）
	var vals []string
	for i := 1; i <= 1000; i++ {
		approved := "1"
		switch {
		case i%50 == 0:
			approved = "spam"
		case i%33 == 0:
			approved = "0"
		}
		vals = append(vals, fmt.Sprintf("(1, 'a', '2024-01-01 00:00:00' + INTERVAL %d MINUTE, '2024-01-01 00:00:00' + INTERVAL %d MINUTE, 'c', '%s', 'comment', %d)", i, i, approved, i%10/9))
	}
	mustExec(t, db, "INSERT INTO wp_comments (comment_post_ID, comment_author, comment_date, comment_date_gmt, comment_content, comment_approved, comment_type, user_id) VALUES "+strings.Join(vals, ", "))
	mustExec(t, db, "ANALYZE TABLE wp_comments")

	for _, tc := range []struct {
		q     string
		table bool   // 表の全件を読む
		want  string // 結果の 1 行目
	}{
		// ダッシュボードの自分のコメントの数と承認済みのコメントの数（範囲の割合 0.98 と 0.95）
		{"SELECT COUNT(*) FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') AND user_id = 1", true, "100"},
		{"SELECT COUNT(*) FROM wp_comments WHERE ( comment_approved = '1' ) AND comment_type NOT IN ('note')", true, "950"},
		{"SELECT comment_type, COUNT(*) FROM wp_comments WHERE comment_approved = '1' GROUP BY comment_type", true, "comment|950"},
		// 範囲の狭い文
		{"SELECT COUNT(*) FROM wp_comments WHERE comment_approved = 'spam' AND user_id = 1", false, "0"},
		// インデックスだけで読める文
		{"SELECT COUNT(*) FROM wp_comments WHERE comment_approved IN ('0', '1')", false, "980"},
		// ORDER BY と LIMIT（最近のコメント）、LIMIT だけ、JOIN
		{"SELECT wp_comments.comment_ID FROM wp_comments WHERE ( ( comment_approved = '0' OR comment_approved = '1' ) ) AND comment_type NOT IN ('note') ORDER BY wp_comments.comment_date_gmt DESC LIMIT 0,25", false, "999"},
		{"SELECT comment_ID FROM wp_comments WHERE comment_approved = '1' AND user_id = 1 LIMIT 1", false, ""},
		{"SELECT COUNT(*) FROM wp_comments JOIN wp_posts ON wp_posts.ID = wp_comments.comment_post_ID WHERE comment_approved = '1' AND user_id = 1", false, "0"},
	} {
		plan := explain(t, db, tc.q)
		if got := !strings.Contains(plan, "IndexedTableAccess(wp_comments)"); got != tc.table {
			t.Errorf("%s: reads the whole table %v, want %v:\n%s", tc.q, got, tc.table, plan)
		}
		if tc.want == "" {
			continue
		}
		rows := queryRows(t, db, tc.q, false)
		if len(rows) == 0 || rows[0] != tc.want {
			t.Errorf("%s: got %v, want %s", tc.q, rows, tc.want)
		}
	}
}
