package backend

import (
	"strings"
	"testing"
	"time"

	"github.com/dolthub/vitess/go/sqltypes"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 文字列、バイト列、日付を含む主キー（docs/plan.md の段階 70）。表はプラグインが作るものそのまま。
func TestMixedPrimaryKey(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	for _, q := range []string{
		// WooCommerce: 整数と文字列の複数列の主キー（文字列が最後）。セカンダリインデックスのエントリから、長さの
		// 決まらない主キーを前から読んで取り出す。
		"CREATE TABLE wp_wc_product_attributes_lookup (product_id bigint(20) NOT NULL, product_or_parent_id bigint(20) NOT NULL, taxonomy varchar(32) NOT NULL, term_id bigint(20) NOT NULL, is_variation_attribute tinyint(1) NOT NULL, in_stock tinyint(1) NOT NULL, INDEX is_variation_attribute_term_id (is_variation_attribute, term_id), PRIMARY KEY (product_or_parent_id, term_id, product_id, taxonomy)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci",
		// Wordfence: バイト列が先頭の主キー。
		"CREATE TABLE wp_wfls_role_counts (serialized_roles VARBINARY(255) NOT NULL, two_factor_inactive TINYINT(1) NOT NULL, passkey_inactive TINYINT(1) NOT NULL DEFAULT 0, user_count BIGINT UNSIGNED NOT NULL DEFAULT 0, PRIMARY KEY (serialized_roles, two_factor_inactive, passkey_inactive))",
		// Wordfence: BINARY(16) の IP（値は末尾を 0x00 で埋めて 16 バイト）。
		"CREATE TABLE wp_wfblockediplog (IP binary(16) NOT NULL DEFAULT '\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0', countryCode varchar(2) NOT NULL, blockCount int(10) unsigned NOT NULL DEFAULT '0', unixday int(10) unsigned NOT NULL, blockType varchar(50) NOT NULL DEFAULT 'generic', PRIMARY KEY (IP, unixday, blockType)) DEFAULT CHARSET=utf8mb4",
		// Wordfence: ENUM を含む主キー（値は番号で持ち、番号の順に並ぶ）。
		"CREATE TABLE wp_wftrafficrates (eMin int(10) unsigned NOT NULL, IP binary(16) NOT NULL DEFAULT '\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0\\0', hitType enum('hit','404') NOT NULL DEFAULT 'hit', hits int(10) unsigned NOT NULL, PRIMARY KEY (eMin, IP, hitType)) DEFAULT CHARSET=utf8",
		// WPForms Lite: 整数と日付（固定長の列だけなので、エントリの後ろから主キーを切る）。
		"CREATE TABLE wp_wpforms_analytics_forms (form_id bigint(20) unsigned NOT NULL, period_date date NOT NULL, views int(10) unsigned NOT NULL DEFAULT 0, entries int(10) unsigned NOT NULL DEFAULT 0, PRIMARY KEY (form_id, period_date), KEY views (views))",
		// WP Reviews for Google: 日付の 1 列。
		"CREATE TABLE wp_trustindex_google_views (date DATE NOT NULL, viewed INT(11) NOT NULL DEFAULT 0, PRIMARY KEY (date))",
		// WPML: 文字列の 2 列（inc/setup/sitepress-schema.php）。
		"CREATE TABLE IF NOT EXISTS `wp_icl_locale_map` ( `code` VARCHAR( 7 ) NOT NULL , `locale` VARCHAR( 35 ) NOT NULL , PRIMARY KEY (`code` ,`locale`) ) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci",
		// 日時の主キーと、セカンダリインデックス。
		"CREATE TABLE ev (at datetime(6) NOT NULL, ts timestamp NOT NULL, name varchar(20) NOT NULL, KEY name (name), PRIMARY KEY (at, ts))",
	} {
		mustExec(t, m, q)
	}
	for _, q := range []string{
		"INSERT INTO wp_wc_product_attributes_lookup VALUES (11, 10, 'pa_color', 5, 1, 1), (12, 10, 'pa_color', 6, 1, 0), (12, 10, 'pa_size', 5, 1, 1), (20, 20, 'PA_COLOR', 5, 0, 1), (20, 20, 'pa_colorx', 5, 0, 1)",
		"INSERT INTO wp_wfls_role_counts (serialized_roles, two_factor_inactive, user_count) VALUES ('a:1:{i:0;s:13:\"administrator\";}', 0, 1), ('a:1:{i:0;s:13:\"administrator\";}', 1, 2), (X'00FF', 0, 3), ('', 0, 4), (X'00', 0, 5), ('A', 0, 6), ('a', 0, 7)",
		"INSERT INTO wp_wfblockediplog (IP, countryCode, blockCount, unixday, blockType) VALUES (X'00000000000000000000FFFF7F000001', 'JP', 1, 20381, 'waf'), (X'00000000000000000000FFFF7F000001', 'JP', 2, 20382, 'waf'), (X'20010DB8000000000000000000000001', 'US', 3, 20381, 'brute'), ('ab', 'XX', 4, 1, 'x')",
		"INSERT INTO wp_wftrafficrates (eMin, IP, hitType, hits) VALUES (100, X'00000000000000000000FFFF7F000001', '404', 1), (100, X'00000000000000000000FFFF7F000001', 'hit', 2), (101, X'00000000000000000000FFFF7F000001', 'hit', 3)",
		"INSERT INTO wp_wpforms_analytics_forms (form_id, period_date, views) VALUES (1, '2026-10-19', 3), (1, '2026-10-20', 5), (2, '2026-10-20', 3), (1, '2025-12-31', 1)",
		"INSERT INTO wp_trustindex_google_views VALUES ('2026-10-20', 1), ('2026-10-21', 2), ('0000-00-00', 3)",
		"INSERT INTO wp_icl_locale_map VALUES ('ja', 'ja'), ('en', 'en_US'), ('en', 'en_GB'), ('pt-br', 'pt_BR')",
		"INSERT INTO ev VALUES ('2026-10-20 10:00:00.000001', '2026-10-20 10:00:00', 'b'), ('2026-10-20 10:00:00', '2026-10-20 11:00:00', 'a'), ('2026-10-20 10:00:00', '2026-10-20 10:00:00', 'a')",
	} {
		mustExec(t, m, q)
	}
	for q, want := range map[string]string{
		// 主キーの点と範囲、セカンダリインデックス
		"SELECT product_id FROM wp_wc_product_attributes_lookup WHERE product_or_parent_id = 10 AND term_id = 5 AND product_id = 12 AND taxonomy = 'PA_SIZE'":  "12",
		"SELECT CONCAT(product_id, taxonomy) FROM wp_wc_product_attributes_lookup WHERE product_or_parent_id = 10 ORDER BY term_id, product_id, taxonomy":      "11pa_color,12pa_size,12pa_color",
		"SELECT CONCAT(product_id, taxonomy) FROM wp_wc_product_attributes_lookup WHERE is_variation_attribute = 0 AND term_id = 5 ORDER BY taxonomy":          "20PA_COLOR,20pa_colorx",
		"SELECT DISTINCT product_or_parent_id FROM wp_wc_product_attributes_lookup WHERE term_id IN (5) AND taxonomy = 'pa_color' AND in_stock = 1 ORDER BY 1": "10,20",
		// 本体の行を引く（エントリから主キーを取り出す）
		"SELECT GROUP_CONCAT(in_stock ORDER BY taxonomy) FROM wp_wc_product_attributes_lookup WHERE is_variation_attribute = 0 AND term_id = 5":                        "1,1",
		"SELECT GROUP_CONCAT(entries) FROM wp_wpforms_analytics_forms WHERE views = 5":                                                                                 "0",
		"SELECT GROUP_CONCAT(ts ORDER BY ts) FROM ev WHERE name = 'a' AND ts > '2000-01-01'":                                                                           "2026-10-20 10:00:00,2026-10-20 11:00:00",
		"SELECT user_count FROM wp_wfls_role_counts WHERE serialized_roles = 'a:1:{i:0;s:13:\"administrator\";}' AND two_factor_inactive = 1 AND passkey_inactive = 0": "2",
		"SELECT user_count FROM wp_wfls_role_counts WHERE serialized_roles = X'00FF' AND two_factor_inactive = 0 AND passkey_inactive = 0":                             "3",
		"SELECT user_count FROM wp_wfls_role_counts WHERE serialized_roles = 'a' AND two_factor_inactive = 0 AND passkey_inactive = 0":                                 "7",
		"SELECT GROUP_CONCAT(user_count ORDER BY serialized_roles, two_factor_inactive) FROM wp_wfls_role_counts":                                                      "4,5,3,6,7,1,2",
		"SELECT user_count FROM wp_wfls_role_counts ORDER BY serialized_roles DESC, two_factor_inactive DESC, passkey_inactive DESC LIMIT 3":                           "2,1,7",
		"SELECT views FROM wp_wpforms_analytics_forms WHERE form_id = 1 AND period_date = '2026-10-20'":                                                                "5",
		"SELECT views FROM wp_wpforms_analytics_forms WHERE form_id = 1 AND period_date = '2026-10-20 00:00:00'":                                                       "5",
		"SELECT SUM(blockCount) FROM wp_wfblockediplog WHERE IP = X'00000000000000000000FFFF7F000001'":                                                                 "3",
		"SELECT blockCount FROM wp_wfblockediplog WHERE IP = X'00000000000000000000FFFF7F000001' AND unixday = 20382 AND blockType = 'waf'":                            "2",
		"SELECT GROUP_CONCAT(blockCount ORDER BY IP, unixday) FROM wp_wfblockediplog":                                                                                  "1,2,3,4",
		"SELECT HEX(IP) FROM wp_wfblockediplog WHERE countryCode = 'XX'":                                                                                               "61620000000000000000000000000000",
		"SELECT blockCount FROM wp_wfblockediplog WHERE IP = X'61620000000000000000000000000000' AND unixday = 1 AND blockType = 'x'":                                  "4",
		// BINARY(n) の短い値は、MySQL と同じく埋めずに比べる（'ab' の行は 'ab\0...\0' なので 'ab' より大きい）
		"SELECT COUNT(*) FROM wp_wfblockediplog WHERE IP = 'ab'":                                                               "0",
		"SELECT blockCount FROM wp_wfblockediplog WHERE IP > 'ab'":                                                             "4",
		"SELECT blockCount FROM wp_wfblockediplog WHERE IP > 'ab' AND unixday = 1":                                             "4",
		"SELECT COUNT(*) FROM wp_wfblockediplog WHERE IP >= 'a' AND IP < 'ab\x00\x01'":                                         "1",
		"SELECT GROUP_CONCAT(hits ORDER BY eMin, IP, hitType) FROM wp_wftrafficrates":                                          "2,1,3",
		"SELECT hitType FROM wp_wftrafficrates ORDER BY eMin, IP, hitType":                                                     "hit,404,hit",
		"SELECT hits FROM wp_wftrafficrates WHERE eMin = 100 AND IP = X'00000000000000000000FFFF7F000001' AND hitType = '404'": "1",
		"SELECT SUM(hits) FROM wp_wftrafficrates WHERE eMin >= 100 AND hitType = 'hit'":                                        "5",
		// 時刻の付いた値は DATE と等しくない（MySQL は DATETIME として比べる）
		"SELECT COUNT(*) FROM wp_wpforms_analytics_forms WHERE form_id = 1 AND period_date = '2026-10-20 10:00:00'":                                               "0",
		"SELECT GROUP_CONCAT(views ORDER BY period_date) FROM wp_wpforms_analytics_forms WHERE form_id = 1 AND period_date BETWEEN '2026-01-01' AND '2026-12-31'": "3,5",
		"SELECT period_date FROM wp_wpforms_analytics_forms WHERE views = 3 ORDER BY form_id":                                                                     "2026-10-19,2026-10-20",
		"SELECT SUM(views) FROM wp_wpforms_analytics_forms WHERE period_date >= '2026-10-01' AND form_id = 1":                                                     "8",
		"SELECT viewed FROM wp_trustindex_google_views WHERE date = '2026-10-21'":                                                                                 "2",
		"SELECT viewed FROM wp_trustindex_google_views WHERE date = CURDATE() - INTERVAL 0 DAY AND date = '2026-10-20'":                                           "",
		"SELECT GROUP_CONCAT(viewed ORDER BY date) FROM wp_trustindex_google_views":                                                                               "3,1,2",
		"SELECT GROUP_CONCAT(locale ORDER BY locale) FROM wp_icl_locale_map WHERE code = 'EN'":                                                                    "en_GB,en_US",
		"SELECT locale FROM wp_icl_locale_map WHERE code = 'pt-br' AND locale = 'PT_br'":                                                                          "pt_BR",
		"SELECT GROUP_CONCAT(name ORDER BY at, ts) FROM ev":                                                                                                       "a,a,b",
		"SELECT name FROM ev WHERE at = '2026-10-20 10:00:00.000001'":                                                                                             "b",
		"SELECT COUNT(*) FROM ev WHERE at = '2026-10-20 10:00:00' AND ts = '2026-10-20 11:00:00'":                                                                 "1",
		"SELECT GROUP_CONCAT(ts ORDER BY ts) FROM ev WHERE name = 'a'":                                                                                            "2026-10-20 10:00:00,2026-10-20 11:00:00",
		// 列の型に変換すると丸められる端（DATE の時刻、秒より細かい値）は区間に使わない（変換した値を端にすると行を落とす）
		"SELECT GROUP_CONCAT(viewed ORDER BY date) FROM wp_trustindex_google_views WHERE date < '2026-10-20 10:00:00'": "3,1",
		"SELECT COUNT(*) FROM ev WHERE at = '2026-10-20 10:00:00' AND ts > '2026-10-20 09:59:59.6'":                    "2",
		"SELECT COUNT(*) FROM ev WHERE at = '2026-10-20 10:00:00' AND ts < '2026-10-20 11:00:00.4'":                    "2",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	// 重複は 1062（照合順序で等しい文字列、同じバイト列、同じ日付）。
	for _, q := range []string{
		"INSERT INTO wp_wc_product_attributes_lookup VALUES (11, 10, 'PA_Color', 5, 1, 1)",
		"INSERT INTO wp_wfls_role_counts (serialized_roles, two_factor_inactive) VALUES (X'00FF', 0)",
		"INSERT INTO wp_wpforms_analytics_forms (form_id, period_date) VALUES (1, '2026-10-20')",
		"INSERT INTO wp_trustindex_google_views VALUES ('2026-10-20', 9)",
		"INSERT INTO wp_wfblockediplog (IP, countryCode, unixday, blockType) VALUES ('ab', 'YY', 1, 'x')",
		"INSERT INTO wp_wftrafficrates (eMin, IP, hitType, hits) VALUES (100, X'00000000000000000000FFFF7F000001', 2, 9)",
		"INSERT INTO ev VALUES ('2026-10-20 10:00:00', '2026-10-20 10:00:00', 'c')",
	} {
		if _, err := m.Exec(q); err == nil || !strings.Contains(err.Error(), "1062") || strings.Contains(q, "wftrafficrates") && !strings.Contains(err.Error(), `Duplicate entry '100-\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\xFF\xFF\x7F\x00\x00' for key 'wp_wftrafficrates.PRIMARY'`) {
			t.Errorf("%s: %v", q, err)
		}
	}
	// 重複のエラーの値は、MySQL と同じく列の値を - でつなぎ、バイナリの表示できないバイトは \xHH、DATE は日付だけ
	// （docs/plan.md の段階 90。本物の MySQL 8.0 で確かめた）。
	if _, err := m.Exec("INSERT INTO wp_wfls_role_counts (serialized_roles, two_factor_inactive) VALUES ('a', 0)"); err == nil || !strings.Contains(err.Error(), "Duplicate entry 'a-0-0' for key 'wp_wfls_role_counts.PRIMARY'") {
		t.Errorf("duplicate bytes: %v", err)
	}
	if _, err := m.Exec("INSERT INTO wp_wpforms_analytics_forms (form_id, period_date) VALUES (2, '2026-10-20')"); err == nil || !strings.Contains(err.Error(), "Duplicate entry '2-2026-10-20' for key 'wp_wpforms_analytics_forms.PRIMARY'") {
		t.Errorf("duplicate date: %v", err)
	}
	t0 := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	time.Sleep(10 * time.Millisecond)
	// WPForms と Wordfence が使う書き方。
	mustExec(t, m, "INSERT INTO wp_wpforms_analytics_forms (form_id, period_date, views) VALUES (1, '2026-10-20', 1) ON DUPLICATE KEY UPDATE views = views + VALUES(views)")
	mustExec(t, m, "INSERT INTO wp_wfls_role_counts (serialized_roles, two_factor_inactive, passkey_inactive, user_count) VALUES (X'00FF', 0, 0, 1) ON DUPLICATE KEY UPDATE user_count = user_count + VALUES(user_count)")
	mustExec(t, m, "INSERT INTO wp_wftrafficrates (eMin, IP, hitType, hits) VALUES (100, X'00000000000000000000FFFF7F000001', '404', 1) ON DUPLICATE KEY UPDATE hits = hits + 1")
	mustExec(t, m, "UPDATE wp_wc_product_attributes_lookup SET taxonomy = 'pa_shape' WHERE product_id = 12 AND taxonomy = 'pa_size'")
	mustExec(t, m, "DELETE FROM wp_wc_product_attributes_lookup WHERE product_id = 20 AND taxonomy = 'pa_color'")
	mustExec(t, m, "DELETE FROM wp_trustindex_google_views WHERE date < '2026-01-01'")
	mustExec(t, m, "UPDATE wp_trustindex_google_views SET date = '2026-10-22' WHERE date = '2026-10-21'")
	for q, want := range map[string]string{
		"SELECT views FROM wp_wpforms_analytics_forms WHERE form_id = 1 AND period_date = '2026-10-20'":                                                             "6",
		"SELECT user_count FROM wp_wfls_role_counts WHERE serialized_roles = X'00FF'":                                                                               "4",
		"SELECT GROUP_CONCAT(CONCAT(product_id, taxonomy) ORDER BY product_id, taxonomy) FROM wp_wc_product_attributes_lookup":                                      "11pa_color,12pa_color,12pa_shape,20pa_colorx",
		"SELECT GROUP_CONCAT(CONCAT(product_id, taxonomy) ORDER BY taxonomy) FROM wp_wc_product_attributes_lookup WHERE is_variation_attribute = 1 AND term_id = 5": "11pa_color,12pa_shape",
		"SELECT date FROM wp_trustindex_google_views ORDER BY date":                                                                                                 "2026-10-20,2026-10-22",
		"SELECT viewed FROM wp_trustindex_google_views ORDER BY date":                                                                                               "1,2",
		"SELECT date FROM wp_trustindex_google_views AS OF '" + t0 + "' ORDER BY date":                                                                              "0000-00-00,2026-10-20,2026-10-21",
		"SELECT GROUP_CONCAT(CONCAT(product_id, taxonomy) ORDER BY product_id, taxonomy) FROM wp_wc_product_attributes_lookup AS OF '" + t0 + "'":                   "11pa_color,12pa_color,12pa_size,20PA_COLOR,20pa_colorx",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}

	// ブランチの変更は差分に出て、マージで本番に入る。主キーの値は SELECT と同じ書き方（日付は日付だけ、バイト列は
	// そのまま）。
	mustExec(t, m, "CALL heimdb_branch_create('st')")
	b := openBranch(t, dsn, "st")
	defer b.Close()
	mustExec(t, b, "INSERT INTO wp_wpforms_analytics_forms (form_id, period_date, views) VALUES (3, '2026-10-21', 7)")
	mustExec(t, b, "UPDATE wp_wfls_role_counts SET user_count = 9 WHERE serialized_roles = 'a' AND two_factor_inactive = 0")
	mustExec(t, b, "DELETE FROM wp_wc_product_attributes_lookup WHERE product_id = 11")
	mustExec(t, b, "INSERT INTO ev VALUES ('2026-10-21 00:00:00', '2026-10-21 00:00:00', 'z')")
	mustExec(t, b, "UPDATE wp_wftrafficrates SET hits = 7 WHERE eMin = 101")
	mustExec(t, b, "UPDATE wp_wfblockediplog SET blockCount = 8 WHERE countryCode IN ('US', 'XX')")
	rows := strings.Join(diffRows(t, m, "CALL heimdb_branch_diff('st', '*')"), "\n")
	for _, want := range []string{
		"wp_wpforms_analytics_forms|form_id=3,period_date=2026-10-21|insert|none|insert|",
		"wp_wfls_role_counts|serialized_roles=a,two_factor_inactive=0,passkey_inactive=0|update|none|update|",
		"wp_wc_product_attributes_lookup|product_or_parent_id=10,term_id=5,product_id=11,taxonomy=pa_color|delete|none|delete|",
		"wp_wftrafficrates|eMin=101,IP=0x00000000000000000000FFFF7F000001,hitType=hit|update|none|update|",
		// UTF-8 として読めても、制御文字（0x00）を含むバイト列は 16 進
		"wp_wfblockediplog|IP=0x20010DB8000000000000000000000001,unixday=20381,blockType=brute|update|none|update|",
		"wp_wfblockediplog|IP=0x61620000000000000000000000000000,unixday=1,blockType=x|update|none|update|",
		"ev|at=2026-10-21 00:00:00,ts=2026-10-21 00:00:00|insert|none|insert|",
	} {
		if !strings.Contains(rows, want) {
			t.Errorf("diff lacks %q:\n%s", want, rows)
		}
	}
	mustExec(t, m, "CALL heimdb_branch_merge('st')")
	for q, want := range map[string]string{
		"SELECT views FROM wp_wpforms_analytics_forms WHERE form_id = 3 AND period_date = '2026-10-21'": "7",
		"SELECT user_count FROM wp_wfls_role_counts WHERE serialized_roles = 'a'":                       "9",
		"SELECT COUNT(*) FROM wp_wc_product_attributes_lookup WHERE product_id = 11":                    "0",
		"SELECT SUM(blockCount) FROM wp_wfblockediplog WHERE countryCode IN ('US', 'XX')":               "16",
		"SELECT name FROM ev WHERE name = 'z'":                                                          "z",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("after merge: %s: %q, want %q", q, got, want)
		}
	}
	if rows := diffRows(t, m, "CALL heimdb_branch_diff('st')"); len(rows) != 0 {
		t.Errorf("diff after merge:\n%s", strings.Join(rows, "\n"))
	}

	// 変更の一覧と復元。
	if got := strings.Join(diffRows(t, m, "CALL heimdb_changes('main', '"+t0+"', 'wp_trustindex_google_views')"), "\n"); !strings.Contains(got, "date=0000-00-00|delete") || !strings.Contains(got, "date=2026-10-22|insert") {
		t.Errorf("changes:\n%s", got)
	}
	mustExec(t, m, "CALL heimdb_restore('main', '"+t0+"', 'wp_trustindex_google_views')")
	if got := rowsOf(t, m, "SELECT date FROM wp_trustindex_google_views ORDER BY date") + " " + rowsOf(t, m, "SELECT viewed FROM wp_trustindex_google_views ORDER BY date"); got != "0000-00-00,2026-10-20,2026-10-21 3,1,2" {
		t.Errorf("after restore: %s", got)
	}
}

// ブランチで主キーの列のキーの形を変えると、テーブル単位の違いになる（docs/plan.md の段階 70）。
func TestPrimaryKeyTypeDiffers(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE wp_k (k varchar(20) NOT NULL PRIMARY KEY, v int NOT NULL)")
	mustExec(t, m, "INSERT INTO wp_k VALUES ('a', 1)")
	// キーの形が変わらない型の変更（整数を広げる、文字列を長くする）は、テーブル単位ではなく列の競合になる。
	mustExec(t, m, "CREATE TABLE wp_w (id int NOT NULL, k varchar(20) NOT NULL, v int NOT NULL, PRIMARY KEY (id, k))")
	mustExec(t, m, "INSERT INTO wp_w VALUES (1, 'a', 1)")
	mustExec(t, m, "CALL heimdb_branch_create('st')")
	b := openBranch(t, dsn, "st")
	defer b.Close()
	mustExec(t, b, "ALTER TABLE wp_k MODIFY k varbinary(20) NOT NULL")
	mustExec(t, b, "UPDATE wp_k SET v = 2")
	summary := strings.Join(diffRows(t, m, "CALL heimdb_branch_diff('st')"), "\n")
	if !strings.Contains(summary, "wp_k|schema differs|") || !strings.Contains(summary, "primary key differs") {
		t.Errorf("summary:\n%s", summary)
	}

	mustExec(t, m, "CALL heimdb_branch_create('st2')")
	b2 := openBranch(t, dsn, "st2")
	defer b2.Close()
	mustExec(t, b2, "ALTER TABLE wp_w MODIFY id bigint NOT NULL, MODIFY k varchar(30) NOT NULL")
	mustExec(t, b2, "INSERT INTO wp_w VALUES (7, 'b', 2)")
	if summary := strings.Join(diffRows(t, m, "CALL heimdb_branch_diff('st2')"), "\n"); strings.Contains(summary, "schema differs") || !strings.Contains(summary, "!id") {
		t.Errorf("summary:\n%s", summary)
	}
	// 主キーの列の変更は、段階 13 のとおり親の側にだけ解決できる。行の変更はマージできる。
	mustExec(t, m, "CALL heimdb_branch_resolve('st2', 'wp_w', 'COLUMN id', 'parent')")
	mustExec(t, m, "CALL heimdb_branch_resolve('st2', 'wp_w', 'COLUMN k', 'parent')")
	mustExec(t, m, "CALL heimdb_branch_merge('st2')")
	if got := rowsOf(t, m, "SELECT GROUP_CONCAT(id ORDER BY id) FROM wp_w WHERE k IN ('a', 'B')"); got != "1,7" {
		t.Errorf("after merge: %q", got)
	}
}

// COLLATE を書かない ALTER TABLE ... MODIFY で、文字列の主キーの列を変えられる（列はテーブルの照合順序になる）。
func TestModifyStringPKWithoutCollate(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE wp_p (a varchar(32) NOT NULL, b int NOT NULL, PRIMARY KEY (a)) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci")
	mustExec(t, m, "INSERT INTO wp_p VALUES ('Abc', 1), ('b', 2)")
	mustExec(t, m, "ALTER TABLE wp_p MODIFY a varchar(64) NOT NULL")
	for q, want := range map[string]string{
		"SELECT COLLATION_NAME FROM information_schema.columns WHERE table_name = 'wp_p' AND column_name = 'a'": "utf8mb4_unicode_520_ci",
		"SELECT b FROM wp_p WHERE a = 'ABC'": "1",
		"SELECT b FROM wp_p WHERE a > 'abc'": "2",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
}

// バイト列の主キーの値は、[]byte でも string でも、列にそのまま入れば主キーの点の読み出しに使う（docs/plan.md の
// 段階 70。エンジンの IsText はバイト列の型にも真になるので、文字列として扱うと []byte の値が外れる）。
func TestExactKeyBinary(t *testing.T) {
	ctx := gsql.NewEmptyContext()
	vb := types.MustCreateBinary(sqltypes.VarBinary, 4)
	for _, c := range []struct {
		v    any
		want bool
	}{
		{[]byte("ab"), true}, {"ab", true}, {[]byte{}, true}, {[]byte{0, 0xff}, true},
		{[]byte("abcde"), false}, {int64(1), false},
	} {
		if got := exactKey(ctx, vb, c.v); got != c.want {
			t.Errorf("exactKey(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
	// BINARY(n) は、ちょうど n バイトの値だけ（短い値の比べ方はエンジンと MySQL で違う）。
	bin := types.MustCreateBinary(sqltypes.Binary, 4)
	for _, c := range []struct {
		v    any
		want bool
	}{
		{[]byte("abcd"), true}, {"ab\x00\x00", true}, {[]byte("ab"), false}, {"ab", false},
	} {
		if got := exactKey(ctx, bin, c.v); got != c.want {
			t.Errorf("BINARY exactKey(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
	date := types.Date
	for _, c := range []struct {
		v    any
		want bool
	}{
		{"2026-10-20", true}, {"2026-10-20 00:00:00", true}, {"2026-10-20 10:00:00", false},
	} {
		if got := exactKey(ctx, date, c.v); got != c.want {
			t.Errorf("DATE exactKey(%#v) = %v, want %v", c.v, got, c.want)
		}
	}
}
