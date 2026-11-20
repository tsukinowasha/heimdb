package backend

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// stage85Values は、数にする文字列の境目の値（docs/plan.md の段階 85）。
var stage85Values = []string{"", "  ", "-", "+", ".5", "5.", "1e", "1e3", "1e3x", "-5x", " 12 ", "\n12", "0x1A", "12abc", "12.7abc", "3.5e1abc", "99999999999999999999", "-99999999999999999999", "１２", "12,5", "abc"}

// stage85Script は、文字列を数にする CAST、比べ、IN、算術と、WordPress の meta_query の NUMERIC の形、数の列を文字列で
// 引く形、非 STRICT と STRICT の INSERT と UPDATE。
func stage85Script() []string {
	q := func(v string) string { return "'" + strings.ReplaceAll(v, "\n", "\\n") + "'" }
	var stmts []string
	for _, v := range stage85Values {
		stmts = append(stmts,
			fmt.Sprintf("SELECT CAST(%[1]s AS SIGNED), CAST(%[1]s AS UNSIGNED), CAST(%[1]s AS DECIMAL(30,1)), CAST(%[1]s AS DOUBLE), %[1]s + 0", q(v)),
			"SHOW WARNINGS",
			fmt.Sprintf("SELECT %[1]s = 12, %[1]s > 11, %[1]s BETWEEN 1 AND 12, 12 IN (%[1]s, 5), %[1]s = 0", q(v)),
		)
	}
	stmts = append(stmts,
		"CREATE TABLE n85 (id int NOT NULL PRIMARY KEY, n int, u int unsigned, d decimal(6,2), KEY n (n), KEY u (u))",
		"INSERT INTO n85 VALUES (1, 1, 1, 1), (12, 12, 12, 12), (13, 13, 13, 13), (20, 0, 0, 0), (35, 35, 35, 35), (-5, -5, 5, -5)",
		// 数の列を文字列で引く（範囲と主キー）
		"SELECT id FROM n85 WHERE id = '12abc'",
		"SELECT id FROM n85 WHERE n = '12abc' ORDER BY id",
		"SELECT id FROM n85 WHERE n > '11abc' ORDER BY id",
		"SELECT id FROM n85 WHERE n > '12.5x' ORDER BY id",
		"SELECT id FROM n85 WHERE n >= '12.5x' ORDER BY id",
		"SELECT id FROM n85 WHERE n < '12.5x' ORDER BY id",
		"SELECT id FROM n85 WHERE n = '12.5x' ORDER BY id",
		"SELECT id FROM n85 WHERE n IN ('12abc', '13') ORDER BY id",
		"SELECT id FROM n85 WHERE n BETWEEN '1x' AND '12x' ORDER BY id",
		"SELECT id FROM n85 WHERE n = 'abc' ORDER BY id",
		"SELECT id FROM n85 WHERE u > '-1x' ORDER BY id",
		"SELECT id FROM n85 WHERE u = '12abc' ORDER BY id",
		"SELECT id FROM n85 WHERE d = '12abc' ORDER BY id",
		"SELECT id FROM n85 WHERE d > '12.5x' ORDER BY id",
		"SELECT id FROM n85 WHERE n = '3.5e1abc' ORDER BY id",
		// JOIN で数の列を文字列で引く
		"CREATE TABLE s85 (id int NOT NULL PRIMARY KEY, v varchar(20))",
		"INSERT INTO s85 VALUES (1, '12abc'), (2, 'abc'), (3, '12.5x'), (4, '13'), (5, '-5x')",
		"SELECT s85.id, n85.id FROM s85 JOIN n85 ON n85.n = s85.v ORDER BY s85.id, n85.id",
		"SELECT s85.id, n85.id FROM s85 JOIN n85 ON n85.id = s85.v ORDER BY s85.id, n85.id",
		// 非 STRICT（WordPress）の INSERT と UPDATE
		"SET SESSION sql_mode = 'NO_ZERO_IN_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'",
		"INSERT INTO n85 (id, n) VALUES (30, '12abc')",
		"SHOW WARNINGS",
		"INSERT INTO n85 (id, n) VALUES (31, 'abc')",
		"SHOW WARNINGS",
		"INSERT INTO n85 (id, n, d) VALUES (32, '12.5x', '7.25zz'), (33, '1e3', '1e2'), (34, '-7y', 'q')",
		"SHOW WARNINGS",
		"UPDATE n85 SET n = '44zz' WHERE id = 30",
		"SHOW WARNINGS",
		"SELECT id, n, u, d FROM n85 WHERE id >= 30 ORDER BY id",
		"SET SESSION sql_mode = 'STRICT_TRANS_TABLES'",
		"INSERT INTO n85 (id, n) VALUES (40, '12')",
		"SELECT id, n FROM n85 WHERE id = 40",
	)
	return stmts
}

// meta_query の NUMERIC（CAST(meta_value AS SIGNED)）と、引用符のない数と meta_value の比べ。数の後ろに文字の続く値を
// 含む。
func stage85MetaScript() []string {
	stmts := append([]string{wpSQLMode}, wpSchema()...)
	stmts = append(stmts,
		"INSERT INTO wp_posts (post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) VALUES "+
			"(1, '2024-01-01 00:00:01', 'c', 'A', '', 'publish', 'a', '', '', '', 'post'), (1, '2024-01-01 00:00:02', 'c', 'B', '', 'publish', 'b', '', '', '', 'post'), "+
			"(1, '2024-01-01 00:00:03', 'c', 'C', '', 'publish', 'c', '', '', '', 'post'), (1, '2024-01-01 00:00:04', 'c', 'D', '', 'publish', 'd', '', '', '', 'post'), "+
			"(1, '2024-01-01 00:00:05', 'c', 'E', '', 'publish', 'e', '', '', '', 'post'), (1, '2024-01-01 00:00:06', 'c', 'F', '', 'publish', 'f', '', '', '', 'post')",
		"INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, 'price', '12abc'), (2, 'price', '9'), (3, 'price', '100 yen'), "+
			"(4, 'price', 'free'), (5, 'price', '15.5'), (6, 'price', '-3x'), (1, 'event_date', '20250331zzz'), (2, 'event_date', '20250101')",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS SIGNED) >= '10' ) ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.post_date DESC LIMIT 0, 10",
		"SELECT FOUND_ROWS()",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'price' ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(wp_postmeta.meta_value AS SIGNED) DESC, wp_posts.ID ASC LIMIT 0, 10",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS SIGNED) BETWEEN '5' AND '20' ) ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC LIMIT 0, 10",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( wp_postmeta.meta_key = 'price' ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_postmeta.meta_value+0 DESC, wp_posts.ID ASC LIMIT 0, 10",
		"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'price' AND CAST(wp_postmeta.meta_value AS DECIMAL(10,2)) > '12.4' ) ) AND wp_posts.post_type = 'post' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY wp_posts.ID ASC LIMIT 0, 10",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND meta_value > 10 ORDER BY post_id",
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value >= 20250101 ORDER BY post_id",
		// 文字列の列と数の IN は、MySQL は数として比べるが HeimDB は文字列として比べる（段階 85 では直さず記録）ので、
		// WordPress と同じく値を引用符で囲む
		"SELECT post_id FROM wp_postmeta WHERE meta_key = 'price' AND meta_value IN ('12', '9') ORDER BY post_id",
	)
	return stmts
}

// 文字列を数にする変換（値と警告）が、本物の MySQL と同じ（段階 85）。
func TestStage85MatchMySQL(t *testing.T) {
	checkGolden(t, "../testdata/stage85_golden.txt", "heimdb_golden85", stage85Script())
}

// meta_query の NUMERIC の文の結果が、本物の MySQL と同じ（段階 85）。
func TestStage85MetaQueryMatchMySQL(t *testing.T) {
	checkGolden(t, "../testdata/stage85_meta_golden.txt", "heimdb_golden85m", stage85MetaScript())
}

// 数の列を文字列の定数で引くとき、索引の範囲は MySQL の比べ方（先頭の数の部分を double で比べる）より狭くならず、
// 条件は Filter に残る。JOIN は先頭の数の部分で引く。
func TestStage85Plans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range stage85Script() {
		if strings.HasPrefix(q, "CREATE TABLE") || strings.HasPrefix(q, "INSERT INTO n85 VALUES") || strings.HasPrefix(q, "INSERT INTO s85") {
			mustExec(t, db, q)
		}
	}
	for _, c := range []struct {
		where string
		want  string
	}{
		{"n = '12abc'", "[{[12, 12]}]"},
		{"n BETWEEN '1x' AND '12x'", "[{[1, 12]}]"},
	} {
		plan := explain(t, db, "SELECT id FROM n85 WHERE "+c.where)
		if !strings.Contains(plan, c.want) || !strings.Contains(plan, "Filter") {
			t.Errorf("%s: plan has no range %s with the Filter kept:\n%s", c.where, c.want, plan)
		}
	}
}
