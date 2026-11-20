package backend

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// stage84Data は、metaQueryData に、post_date が記事 10 と同じ記事 5 件（post__in の同じ順位）と、meta_value の境目の値
// （空、末尾の空白、大文字、NULL、全角、64 文字のプレフィックスより長い値）を足す。
func stage84Data() []string {
	long := strings.Repeat("z", 70)
	stmts := metaQueryData()
	// ID は決めて 1 行ずつ入れる（INSERT ... SELECT の後の AUTO_INCREMENT の飛び方と、ID を決めた複数行の INSERT が
	// 返す insert_id は MySQL と違う。段階 84 では記録だけ）
	for id := 601; id <= 605; id++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO wp_posts (ID, post_author, post_date, post_content, post_title, post_excerpt, post_status, post_name, to_ping, pinged, post_content_filtered, post_type) "+
			"VALUES (%d, 1, '2024-01-01 01:10:00', 'c', 'Same %d', '', 'publish', 'same-%d', '', '', '', 'news')", id, id, id))
	}
	for i, m := range [][2]any{
		{601, "('event_date', '')"}, {602, "('event_date', '20250101 ')"}, {603, "('event_date', NULL)"}, {604, "('event_date', '２０２５０２０１')"},
		{605, "('event_date', '20250331" + long + "')"}, {601, "('event_date', '20250201" + long + "')"}, {602, "('event_date', '20250331')"},
		{601, "('category', 'Cat3')"}, {602, "('category', 'cat3 ')"}, {603, "('category', 'cat3')"}, {604, "('featured', '1')"}, {603, "('featured', '1')"},
	} {
		kv := strings.TrimSuffix(strings.TrimPrefix(m[1].(string), "("), ")")
		stmts = append(stmts, fmt.Sprintf("INSERT INTO wp_postmeta (meta_id, post_id, meta_key, meta_value) VALUES (%d, %d, %s)", 20001+i, m[0], kv))
	}
	return append(stmts, "ANALYZE TABLE wp_posts, wp_postmeta")
}

// stage84Queries は、段階 84 の文（ヘッドレスの API の REST の文と、範囲の境目）。標準の索引と、API 用の索引を足した後の両方で流す。
var stage84Queries = []string{
	// post__in（WP_Query の既定の日付順）。LIMIT と SQL_CALC_FOUND_ROWS も
	"SELECT wp_posts.ID, wp_posts.post_date FROM wp_posts WHERE 1=1 AND wp_posts.ID IN (5,10,601,602,603,300,200,100,50,9999) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) ORDER BY wp_posts.post_date DESC",
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.ID IN (5,10,601,602,603,300,200,100,50) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) ORDER BY wp_posts.post_date DESC LIMIT 0, 5",
	"SELECT FOUND_ROWS()",
	"SELECT wp_posts.ID FROM wp_posts WHERE 1=1 AND wp_posts.ID IN (5,10,601,602,603) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish' OR wp_posts.post_status = 'private')) ORDER BY wp_posts.post_date DESC",
	// range（meta_value の BETWEEN）
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'event_date' AND wp_postmeta.meta_value BETWEEN '20250101' AND '20250331' ) ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(wp_postmeta.meta_value AS CHAR) ASC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
	// 範囲の境目
	"SELECT post_id, meta_value FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value BETWEEN '20250101' AND '20250331' AND post_id > 590 ORDER BY meta_id",
	"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value BETWEEN '20250101' AND '20250331'",
	"SELECT post_id, meta_value FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value >= '20250331' ORDER BY meta_id",
	"SELECT post_id FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value > '20250331" + strings.Repeat("z", 64) + "' ORDER BY meta_id",
	"SELECT post_id FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value >= '20250331" + strings.Repeat("z", 70) + "' ORDER BY meta_id",
	"SELECT post_id, meta_value FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value < '2' ORDER BY meta_id",
	"SELECT post_id FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value > '２' ORDER BY meta_id",
	"SELECT post_id, meta_value FROM wp_postmeta WHERE meta_key = 'category' AND meta_value >= 'CAT3' AND meta_value < 'cat4' ORDER BY meta_id LIMIT 10",
	"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'category' AND meta_value >= 'CAT3' AND meta_value < 'cat4'",
	// 末尾の空白（'cat3 '）は、MySQL の utf8mb4_unicode_520_ci（PAD SPACE）では 'cat3' と等しいが、HeimDB は等しくない
	// （インデックスのない列でも同じ。段階 84 では直さず記録）ので、境目の値は大文字だけを比べる
	"SELECT post_id, meta_value FROM wp_postmeta WHERE meta_key = 'category' AND meta_value >= 'CAT3' AND meta_value <= 'cat3' AND post_id IN (601, 603) ORDER BY meta_id",
	// 数との比較は数として比べる（'990' と 990 で違う）
	"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'priority' AND meta_value > 990",
	"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'priority' AND meta_value > '990'",
	// 数の後ろに文字の続く値（'20250331zzz'）を数にする変換は MySQL と違う（段階 84 では直さず記録。CAST も同じ）ので、
	// 数との比較は記事 600 件までのふつうの値で比べる
	"SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'event_date' AND meta_value BETWEEN 20250101 AND 20250331 AND post_id <= 600",
	// two_filters（ホスティング側が組み立て直したもの）
	"SELECT SQL_CALC_FOUND_ROWS wp_posts.ID FROM wp_posts INNER JOIN wp_postmeta ON ( wp_posts.ID = wp_postmeta.post_id ) INNER JOIN wp_postmeta AS mt1 ON ( wp_posts.ID = mt1.post_id ) INNER JOIN wp_postmeta AS mt2 ON ( wp_posts.ID = mt2.post_id ) WHERE 1=1 AND ( ( wp_postmeta.meta_key = 'category' AND wp_postmeta.meta_value = 'cat3' ) AND ( mt1.meta_key = 'featured' AND mt1.meta_value = '1' ) AND mt2.meta_key = 'event_date' ) AND wp_posts.post_type = 'news' AND ((wp_posts.post_status = 'publish')) GROUP BY wp_posts.ID ORDER BY CAST(mt2.meta_value AS CHAR) DESC, wp_posts.ID DESC LIMIT 0, 20",
	"SELECT FOUND_ROWS()",
}

// apiIndexes は、ヘッドレスの API が wp_postmeta に足した索引。
const apiIndexes = "ALTER TABLE wp_postmeta ADD INDEX key_value (meta_key(191), meta_value(64)), ADD INDEX post_key (post_id, meta_key(191))"

// 段階 84 の文の結果（並びと FOUND_ROWS() を含む）が、標準の索引でも API 用の索引でも、本物の MySQL と同じ。
func TestStage84MatchMySQL(t *testing.T) {
	stmts := append(append([]string{wpSQLMode}, wpSchema()...), stage84Data()...)
	stmts = append(stmts, stage84Queries...)
	stmts = append(stmts, apiIndexes, "ANALYZE TABLE wp_postmeta")
	stmts = append(stmts, stage84Queries...)
	checkGolden(t, "../testdata/stage84_golden.txt", "heimdb_golden84", stmts)
}

// post__in は主キーで引いて並べ替え、API 用の索引では meta_value の範囲を読み、記事を主キーで引く。two_filters は
// event_date（全記事）から読まず、記事をハッシュ JOIN で全部読まない（docs/plan.md の段階 84）。
func TestStage84Plans(t *testing.T) {
	dsn, stop := startServer(t, t.TempDir())
	defer stop()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{wpSQLMode}, wpSchema()...), stage84Data()...) {
		mustExec(t, db, q)
	}
	postIn := func() {
		t.Helper()
		for _, q := range stage84Queries[:2] {
			if plan := explain(t, db, q); !strings.Contains(plan, "index: [wp_posts.ID]") || strings.Contains(plan, "wp_posts.post_date,wp_posts.ID]") {
				t.Errorf("%s\npost__in does not look up the primary key:\n%s", q, plan)
			}
		}
	}
	postIn()
	// 点の数が並べ替えのインデックスで読む行と変わらなければ、並べ替えのインデックスを読む
	var ids []string
	for i := 1; i <= 600; i++ {
		ids = append(ids, strconv.Itoa(i))
	}
	many := "SELECT wp_posts.ID FROM wp_posts WHERE wp_posts.ID IN (" + strings.Join(ids, ",") + ") AND wp_posts.post_type = 'news' AND wp_posts.post_status = 'publish' ORDER BY wp_posts.post_date DESC LIMIT 5"
	if plan := explain(t, db, many); !strings.Contains(plan, "wp_posts.post_type,wp_posts.post_status,wp_posts.post_date,wp_posts.ID]") {
		t.Errorf("post__in with every post does not read type_status_date:\n%s", plan)
	}

	mustExec(t, db, apiIndexes)
	mustExec(t, db, "ANALYZE TABLE wp_postmeta")
	postIn()
	rng := explain(t, db, stage84Queries[4])
	if !strings.Contains(rng, "[{[event_date, event_date], [20250101, 20250331]}]") || !strings.Contains(rng, "keys: wp_postmeta.post_id") || strings.Contains(rng, "post_author") {
		t.Errorf("range does not read the meta_value range and look up posts by the primary key:\n%s", rng)
	}
	// 数との比較は範囲に入れない
	if plan := explain(t, db, stage84Queries[16]); strings.Contains(plan, "[990") {
		t.Errorf("a numeric comparison is in the range:\n%s", plan)
	}
	// featured = '0'（記事の 9 割）は絞り込みが弱いので、MySQL と同じく category から読み、featured は記事ごとに引く
	// （標本の見積もり。featured = '1' と同じ 0.1 にすると featured から読む）
	weak := explain(t, db, strings.Replace(stage84Queries[19], "mt1.meta_value = '1'", "mt1.meta_value = '0'", 1))
	if !strings.Contains(weak, "keys: wp_postmeta.post_id, 'featured'") || strings.Contains(weak, "[featured, featured], [0, 0]") {
		t.Errorf("two_filters with featured = '0' does not start from category:\n%s", weak)
	}
	two := explain(t, db, stage84Queries[19])
	if strings.Contains(two, "[{[event_date, event_date], [NULL, ∞)}]") || strings.Contains(two, "post_author") {
		t.Errorf("two_filters reads the event_date range or all posts:\n%s", two)
	}
}

// 標本の見積もり（prefixSample）は、インデックスの先頭の列の組の行の数を、表の照合順序で数える。標本にない組は ok が
// false（見積もりはヒストグラムと既定の割合に戻る）。
func TestSamplePrefixRows(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE m (id bigint NOT NULL PRIMARY KEY, k varchar(255), v longtext, KEY kv (k(191), v(4))) DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_520_ci")
	var vals []string
	for i := 1; i <= 300; i++ {
		vals = append(vals, fmt.Sprintf("(%d, 'k%d', '%s')", i, i%3, []string{"cat3", "Cat3", "cat4", "category-long"}[i%4]))
	}
	s.mustQuery(t, "INSERT INTO m VALUES "+strings.Join(vals, ", "))
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, _, err := db.GetTableInsensitive(s.ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	table := tbl.(*Table)
	if _, err := rowCounts.stats(s.ctx, table); err != nil {
		t.Fatal(err)
	}
	idxs, _ := table.GetIndexes(s.ctx)
	var idx *Index
	for _, i := range idxs {
		if i.ID() == "kv" {
			idx = i.(*Index)
		}
	}
	for _, tc := range []struct {
		vals []any
		want float64
		ok   bool
	}{
		{[]any{"k1"}, 100, true},
		{[]any{"k1", "cat3"}, 50, true},     // 'cat3' と 'Cat3'（照合順序で等しい）
		{[]any{"K1", "CAT3"}, 50, true},     // 定数の大文字と小文字も同じ
		{[]any{"k1", "category"}, 25, true}, // プレフィックス（4 文字）で切った値は、同じ先頭の値の行を数える（多めの見積もり）
		{[]any{"k1", "dog"}, 0, false},
		{[]any{"k9"}, 0, false},
	} {
		got, ok := samplePrefixRows(table, idx, tc.vals)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%v: rows %v, %v; want %v, %v", tc.vals, got, ok, tc.want, tc.ok)
		}
	}
}
