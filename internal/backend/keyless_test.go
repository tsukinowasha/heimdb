package backend

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 主キーのない表（docs/plan.md の段階 72）。一意インデックスもない表は見えない行 ID で足した順に並べ、すべての列が
// NOT NULL の一意インデックスがあれば、その列で並べる（MySQL の InnoDB と同じ）。
func TestKeylessTable(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	for _, q := range []string{
		// WP Cerber のログ（一意インデックスがない）
		"CREATE TABLE wp_cerber_qmem (ip varchar(39) CHARACTER SET ascii NOT NULL, http_code int(10) UNSIGNED NOT NULL, stamp int(10) UNSIGNED NOT NULL, KEY ip_stamp (ip, stamp)) DEFAULT CHARSET=utf8",
		// Loginizer（すべての列が NOT NULL の一意インデックス）
		"CREATE TABLE wp_loginizer_logs (`username` varchar(255) NOT NULL DEFAULT '', `time` int(10) NOT NULL DEFAULT '0', `count` int(10) NOT NULL DEFAULT '0', `lockout` int(10) NOT NULL DEFAULT '0', `ip` varchar(255) NOT NULL DEFAULT '', `url` varchar(255) NOT NULL DEFAULT '', UNIQUE KEY `ip` (`ip`)) DEFAULT CHARSET=utf8",
	} {
		mustExec(t, m, q)
	}
	for _, q := range []string{
		"INSERT INTO wp_cerber_qmem VALUES ('10.0.0.3', 404, 30), ('10.0.0.1', 200, 10), ('10.0.0.2', 200, 20), ('10.0.0.1', 200, 10)",
		"INSERT INTO wp_loginizer_logs (username, time, count, ip, lockout, url) VALUES ('b', 2, 1, '10.0.0.2', 0, '/'), ('a', 1, 1, '10.0.0.1', 0, '/')",
	} {
		mustExec(t, m, q)
	}
	check := func(want map[string]string) {
		t.Helper()
		for q, w := range want {
			if got := rowsOf(t, m, q); got != w {
				t.Errorf("%s: %q, want %q", q, got, w)
			}
		}
	}
	check(map[string]string{
		// 行 ID の表は足した順、一意キーの表はその列の順（ORDER BY なし）
		"SELECT CONCAT(ip, ':', stamp) FROM wp_cerber_qmem":                        "10.0.0.3:30,10.0.0.1:10,10.0.0.2:20,10.0.0.1:10",
		"SELECT username FROM wp_loginizer_logs":                                   "a,b",
		"SELECT COUNT(*) FROM wp_cerber_qmem WHERE ip = '10.0.0.1'":                "2",
		"SELECT http_code FROM wp_cerber_qmem WHERE ip = '10.0.0.3' AND stamp > 5": "404",
	})
	// 同じ中身の行の片方だけを消す、変える
	mustExec(t, m, "DELETE FROM wp_cerber_qmem WHERE ip = '10.0.0.1' LIMIT 1")
	mustExec(t, m, "UPDATE wp_cerber_qmem SET stamp = 40 WHERE ip = '10.0.0.2'")
	check(map[string]string{
		"SELECT CONCAT(ip, ':', stamp) FROM wp_cerber_qmem": "10.0.0.3:30,10.0.0.2:40,10.0.0.1:10",
	})
	mustExec(t, m, "DELETE FROM wp_cerber_qmem WHERE stamp < 35")
	check(map[string]string{
		"SELECT CONCAT(ip, ':', stamp) FROM wp_cerber_qmem": "10.0.0.2:40",
	})
	// Loginizer の数え上げ（左の代入の結果を右の式で使う）
	upsert := "INSERT INTO wp_loginizer_logs (username, time, count, ip, lockout, url) VALUES ('c', 3, 1, '10.0.0.1', FLOOR(1 / 3), '/x') ON DUPLICATE KEY UPDATE username = VALUES(username), time = VALUES(time), count = count + 1, lockout = FLOOR(count / 3), url = VALUES(url)"
	mustExec(t, m, upsert)
	mustExec(t, m, upsert)
	check(map[string]string{
		"SELECT CONCAT(username, count, lockout, url) FROM wp_loginizer_logs WHERE ip = '10.0.0.1'": "c31/x",
		"SELECT COUNT(*) FROM wp_loginizer_logs":                                                    "2",
	})
	if _, err := m.Exec("UPDATE wp_loginizer_logs SET ip = '10.0.0.2' WHERE ip = '10.0.0.1'"); err == nil || !strings.Contains(err.Error(), "Duplicate entry '10.0.0.2' for key 'wp_loginizer_logs.ip'") {
		t.Errorf("duplicate by update: %v", err)
	}
	if _, err := m.Exec("INSERT INTO wp_loginizer_logs (ip) VALUES ('10.0.0.2')"); err == nil || !strings.Contains(err.Error(), "Error 1062 (23000): Duplicate entry '10.0.0.2' for key 'wp_loginizer_logs.ip'") {
		t.Errorf("duplicate: %v", err)
	}
	// 並べる一意インデックスは、NULL を許す列と前方の長さ付きの列を含まない最初のもの（本物の MySQL 8.0 で確かめた）
	mustExec(t, m, "CREATE TABLE kn (a int NULL, b varchar(10) NOT NULL, c int NOT NULL, UNIQUE KEY ua (a), UNIQUE KEY ub (b), UNIQUE KEY uc (c))")
	mustExec(t, m, "INSERT INTO kn VALUES (3, 'x', 1), (1, 'z', 3), (2, 'y', 2)")
	mustExec(t, m, "CREATE TABLE kp (t varchar(50) NOT NULL, c int NOT NULL, UNIQUE KEY ut (t(5)), UNIQUE KEY uc (c))")
	mustExec(t, m, "INSERT INTO kp VALUES ('bbbbbbb', 1), ('aaaaaaa', 2), ('ccccccc', 0)")
	// 主キーにできない型（DECIMAL）の一意インデックスでは並べない（行 ID で足した順）
	mustExec(t, m, "CREATE TABLE kd (d decimal(10,2) NOT NULL, UNIQUE KEY d (d))")
	mustExec(t, m, "INSERT INTO kd VALUES (2.5), (1.5)")
	mustExec(t, m, "INSERT INTO kd VALUES (1.5) ON DUPLICATE KEY UPDATE d = VALUES(d) + 10")
	check(map[string]string{
		"SELECT a FROM kn": "3,2,1",
		"SELECT c FROM kp": "0,1,2",
		"SELECT d FROM kd": "2.50,11.50",
	})
	// 行 ID で並べる表の、NULL を許す一意インデックスの重複（エンジンには見える列だけの行を返す）
	mustExec(t, m, "CREATE TABLE ku (k int NULL, n int NOT NULL, UNIQUE KEY k (k))")
	mustExec(t, m, "INSERT INTO ku VALUES (1, 1), (NULL, 1), (NULL, 1)")
	mustExec(t, m, "INSERT INTO ku VALUES (1, 4) ON DUPLICATE KEY UPDATE n = n + 10 * VALUES(n)")
	mustExec(t, m, "REPLACE INTO ku VALUES (NULL, 5)")
	mustExec(t, m, "INSERT IGNORE INTO ku VALUES (1, 7)")
	check(map[string]string{
		"SELECT CONCAT(COALESCE(k, '-'), n) FROM ku": "141,-1,-1,-5",
	})
	// SHOW CREATE TABLE は主キーなしのまま、見えない列もない
	if got := rowsOf(t, m, "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_NAME = 'wp_cerber_qmem' ORDER BY ORDINAL_POSITION"); got != "ip,http_code,stamp" {
		t.Errorf("columns: %s", got)
	}
	var name, create string
	if err := m.QueryRow("SHOW CREATE TABLE wp_loginizer_logs").Scan(&name, &create); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(create, "PRIMARY") || strings.Contains(create, "__heimdb") || !strings.Contains(create, "UNIQUE KEY `ip`") {
		t.Errorf("create: %s", create)
	}
	if err := m.QueryRow("SHOW CREATE TABLE wp_cerber_qmem").Scan(&name, &create); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(create, "PRIMARY") || strings.Contains(create, "__heimdb") || strings.Contains(create, "AUTO_INCREMENT") {
		t.Errorf("create: %s", create)
	}
	if got := rowsOf(t, m, "SELECT GROUP_CONCAT(INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_NAME = 'wp_cerber_qmem'"); got != "ip_stamp,ip_stamp" {
		t.Errorf("statistics: %s", got)
	}
}

// 主キーのない表のブランチの差分とマージ、AS OF、変更の一覧と復元（docs/plan.md の段階 72）。行 ID はブランチの番号帯
// から振るので、両側で足した行は両方残り、分岐の前からある行は行 ID で対応づく。一意キーで並べる表は、その列で対応づく。
func TestKeylessBranchMerge(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE lg (ip varchar(39) NOT NULL, code int NOT NULL, stamp int NOT NULL, KEY ip (ip))")
	mustExec(t, m, "CREATE TABLE lk (ip varchar(39) NOT NULL, n int NOT NULL, UNIQUE KEY ip (ip))")
	mustExec(t, m, "INSERT INTO lg VALUES ('a', 200, 1), ('b', 200, 2), ('c', 200, 3)")
	mustExec(t, m, "INSERT INTO lk VALUES ('x', 1), ('y', 1)")
	t0 := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	time.Sleep(10 * time.Millisecond)
	mustExec(t, m, "CALL heimdb_branch_create('st')")
	b := openBranch(t, dsn, "st")
	defer b.Close()
	// 両側でログを足し、分岐の前の行を片側ずつ変える
	mustExec(t, b, "INSERT INTO lg VALUES ('d', 404, 4)")
	mustExec(t, m, "INSERT INTO lg VALUES ('e', 500, 5)")
	mustExec(t, b, "UPDATE lg SET code = 301 WHERE ip = 'a'")
	mustExec(t, m, "UPDATE lg SET stamp = 20 WHERE ip = 'a'")
	mustExec(t, b, "DELETE FROM lg WHERE ip = 'b'")
	mustExec(t, b, "UPDATE lk SET n = 5 WHERE ip = 'x'")
	mustExec(t, b, "INSERT INTO lk VALUES ('z', 1)")
	mustExec(t, m, "INSERT INTO lk VALUES ('z', 2)")
	rows := strings.Join(diffRows(t, m, "CALL heimdb_branch_diff('st', '*')"), "\n")
	for _, want := range []string{"lk|ip=x|update", "lk|ip=z|insert|insert|conflict"} {
		if !strings.Contains(rows, want) {
			t.Errorf("diff lacks %q:\n%s", want, rows)
		}
	}
	if strings.Contains(rows, "__heimdb_row_hash") {
		t.Errorf("diff shows the row hash:\n%s", rows)
	}
	mustExec(t, m, "CALL heimdb_branch_resolve('st', 'lk', 'ip=z', 'parent')")
	mustExec(t, m, "CALL heimdb_branch_merge('st')")
	for q, want := range map[string]string{
		"SELECT CONCAT(ip, code, ':', stamp) FROM lg":        "a301:20,c200:3,e500:5,d404:4",
		"SELECT CONCAT(ip, n) FROM lk":                       "x5,y1,z2",
		"SELECT COUNT(*) FROM lg WHERE ip = 'a'":             "1",
		"SELECT CONCAT(ip, code) FROM lg AS OF '" + t0 + "'": "a200,b200,c200",
	} {
		if got := rowsOf(t, m, q); got != want {
			t.Errorf("after merge: %s: %q, want %q", q, got, want)
		}
	}
	if rows := diffRows(t, m, "CALL heimdb_branch_diff('st')"); len(rows) != 0 {
		t.Errorf("diff after merge:\n%s", strings.Join(rows, "\n"))
	}
	// ブランチは本番の後の変更を見ず、自分の変更を続けられる
	mustExec(t, b, "DELETE FROM lg WHERE code = 404")
	if got := rowsOf(t, b, "SELECT CONCAT(ip, code) FROM lg"); got != "a301,c200" {
		t.Errorf("branch: %s", got)
	}

	// 変更の一覧と復元
	if got := strings.Join(diffRows(t, m, "CALL heimdb_changes('main', '"+t0+"', 'lg')"), "\n"); !strings.Contains(got, "|delete") || !strings.Contains(got, "|insert") {
		t.Errorf("changes:\n%s", got)
	}
	mustExec(t, m, "CALL heimdb_restore('main', '"+t0+"', 'lg')")
	mustExec(t, m, "CALL heimdb_restore('main', '"+t0+"', 'lk')")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, code, ':', stamp) FROM lg") + " " + rowsOf(t, m, "SELECT CONCAT(ip, n) FROM lk"); got != "a200:1,b200:2,c200:3 x1,y1" {
		t.Errorf("after restore: %s", got)
	}
	// 復元の後も書ける（行 ID は払い出した値を使い直さない）
	mustExec(t, m, "INSERT INTO lg VALUES ('f', 200, 6)")
	mustExec(t, m, "DELETE FROM lg WHERE ip = 'a'")
	if got := rowsOf(t, m, "SELECT ip FROM lg"); got != "b,c,f" {
		t.Errorf("after restore and insert: %s", got)
	}
}

// 主キーのない表の書き直し（docs/plan.md の段階 72）。列を足しても行 ID と足した順は変わらない。NOT NULL の一意
// インデックスを足すとその列で並べ、消すと行 ID で並べる表に戻る。空の表は定義だけを変える。
func TestKeylessAlter(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE acl (ip varchar(39) NOT NULL, tag char(1) NOT NULL)")
	mustExec(t, m, "INSERT INTO acl VALUES ('c', 'W'), ('a', 'B'), ('b', 'W'), ('a', 'B')")
	mustExec(t, m, "ALTER TABLE acl ADD v int NOT NULL DEFAULT 0 AFTER ip, ADD INDEX tag (tag)")
	mustExec(t, m, "ALTER TABLE acl MODIFY COLUMN ip varchar(81) NOT NULL")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, v, tag) FROM acl"); got != "c0W,a0B,b0W,a0B" {
		t.Errorf("after add column: %s", got)
	}
	mustExec(t, m, "DELETE FROM acl WHERE ip = 'a' LIMIT 1")
	if _, err := m.Exec("ALTER TABLE acl ADD UNIQUE KEY ipu (ip)"); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(t, m, "SELECT CONCAT(ip, tag) FROM acl"); got != "aB,bW,cW" {
		t.Errorf("after add unique: %s", got)
	}
	if _, err := m.Exec("INSERT INTO acl VALUES ('a', 1, 'X')"); err == nil {
		t.Error("duplicate after add unique: no error")
	}
	mustExec(t, m, "INSERT INTO acl VALUES ('0', 1, 'X')")
	mustExec(t, m, "ALTER TABLE acl RENAME INDEX ipu TO ipx")
	mustExec(t, m, "ALTER TABLE acl ADD INDEX v (v)")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, tag) FROM acl"); got != "0X,aB,bW,cW" {
		t.Errorf("after rename index: %s", got)
	}
	mustExec(t, m, "ALTER TABLE acl DROP INDEX ipx")
	mustExec(t, m, "INSERT INTO acl VALUES ('a', 2, 'Y')")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, v, tag) FROM acl"); got != "01X,a0B,b0W,c0W,a2Y" {
		t.Errorf("after drop unique: %s", got)
	}
	mustExec(t, m, "UPDATE acl SET v = 3 WHERE ip = 'a' AND v = 2")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, v) FROM acl WHERE tag = 'Y'"); got != "a3" {
		t.Errorf("after update: %s", got)
	}
	// ADD PRIMARY KEY は重複を拒否し、通れば主キーのある表になる。DROP PRIMARY KEY は主キーの順に行 ID を振る。
	if _, err := m.Exec("ALTER TABLE acl ADD PRIMARY KEY (ip)"); err == nil || !strings.Contains(err.Error(), "1062") {
		t.Errorf("add primary key with duplicates: %v", err)
	}
	mustExec(t, m, "DELETE FROM acl WHERE tag = 'Y'")
	mustExec(t, m, "ALTER TABLE acl ADD PRIMARY KEY (ip)")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, tag) FROM acl WHERE ip = 'b'") + " " + rowsOf(t, m, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_NAME = 'acl' AND INDEX_NAME = 'PRIMARY'"); got != "bW 1" {
		t.Errorf("after add primary key: %s", got)
	}
	mustExec(t, m, "UPDATE acl SET ip = '9' WHERE ip = '0'")
	mustExec(t, m, "ALTER TABLE acl DROP PRIMARY KEY")
	mustExec(t, m, "INSERT INTO acl VALUES ('0', 0, 'Z')")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, tag) FROM acl"); got != "9X,aB,bW,cW,0Z" {
		t.Errorf("after drop primary key: %s", got)
	}
	// CREATE UNIQUE INDEX の文でも、行のある表を並べ直す
	mustExec(t, m, "CREATE UNIQUE INDEX iu ON acl (ip)")
	if got := rowsOf(t, m, "SELECT ip FROM acl"); got != "0,9,a,b,c" {
		t.Errorf("after create unique index: %s", got)
	}
	mustExec(t, m, "DROP INDEX iu ON acl")
	// 同じ文で変えた行を、同じ中身の別の行より先に引かない（1 を 2 にした行を、元から 2 の行として 3 にしない）
	mustExec(t, m, "CREATE TABLE inc (x int NOT NULL)")
	mustExec(t, m, "INSERT INTO inc VALUES (1), (2)")
	mustExec(t, m, "UPDATE inc SET x = x + 1")
	// 同じ文で同じ行を 2 回変える
	mustExec(t, m, "CREATE TABLE twice (a int NULL, b int NOT NULL, UNIQUE KEY a (a))")
	mustExec(t, m, "INSERT INTO twice VALUES (1, 0)")
	mustExec(t, m, "INSERT INTO twice VALUES (1, 0), (1, 0) ON DUPLICATE KEY UPDATE b = b + 1")
	if got := rowsOf(t, m, "SELECT x FROM inc") + " " + rowsOf(t, m, "SELECT b FROM twice"); got != "2,3 2" {
		t.Errorf("update in one statement: %s", got)
	}
	mustExec(t, m, "TRUNCATE TABLE acl")
	mustExec(t, m, "INSERT INTO acl VALUES ('z', 0, 'W'), ('y', 0, 'W')")
	if got := rowsOf(t, m, "SELECT ip FROM acl WHERE tag = 'W'"); got != "z,y" {
		t.Errorf("after truncate: %s", got)
	}
}

// 行 ID で並べる表は、エンジンが渡す値（保存した行を復号したもの）のハッシュで行を引く。どの型でも、書いた値と復号した
// 値のハッシュが同じになり、消す・変える行が見つかる（docs/plan.md の段階 72）。
func TestKeylessAllTypes(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE ty (n int NOT NULL, ti tinyint, bu bigint unsigned, de decimal(14,4), dd double, fl float, vc varchar(20), ch char(3), tx text, lt longtext, bl blob, bi binary(8), vb varbinary(8), d date, dt datetime, dt6 datetime(6), ts timestamp NULL, tm time, yr year, en enum('a','b'), st set('x','y'), bt bit(3), js json) DEFAULT CHARSET=utf8mb4")
	mustExec(t, m, "INSERT INTO ty VALUES (1, -1, 18446744073709551615, 1234567890.1234, 1.5e300, 0.1, 'Ab', 'x', 'テキスト', REPEAT('z', 3000), X'00FF', X'0102', X'', '2026-10-24', '2026-10-24 12:34:56', '2026-10-24 12:34:56.123456', '2026-10-24 01:02:03', '-12:34:56.5', 2026, 'b', 'x,y', b'101', '{\"b\": [1, 2.50, \"x\"], \"a\": null, \"n\": 1e2}')")
	mustExec(t, m, "INSERT INTO ty (n) VALUES (2), (2)")
	mustExec(t, m, "INSERT INTO ty (n, js, de, dd) VALUES (3, '[1.0, 10000000000000000000000, -0.0]', -0.0001, -0.0)")
	mustExec(t, m, "INSERT INTO ty SELECT n + 10, ti, bu, de, dd, fl, vc, ch, tx, lt, bl, bi, vb, d, dt, dt6, ts, tm, yr, en, st, bt, js FROM ty WHERE n IN (1, 3)")
	mustExec(t, m, "UPDATE ty SET ti = 5 WHERE n IN (1, 13)")
	mustExec(t, m, "UPDATE ty SET ti = 6 WHERE n = 2 LIMIT 1")
	mustExec(t, m, "DELETE FROM ty WHERE n IN (3, 11)")
	if got := rowsOf(t, m, "SELECT CONCAT(n, ':', COALESCE(ti, '-')) FROM ty"); got != "1:5,2:6,2:-,13:5" {
		t.Errorf("rows: %s", got)
	}
	mustExec(t, m, "DELETE FROM ty")
	if got := rowsOf(t, m, "SELECT COUNT(*) FROM ty"); got != "0" {
		t.Errorf("after delete: %s", got)
	}
}

// ブランチで主キーのない表を書き直しても（列の追加）、行 ID は変わらず、マージで行が対応づく（docs/plan.md の段階 72）。
func TestKeylessBranchAlter(t *testing.T) {
	m, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE lg (ip varchar(39) NOT NULL, code int NOT NULL)")
	mustExec(t, m, "INSERT INTO lg VALUES ('a', 1), ('b', 2), ('c', 3)")
	mustExec(t, m, "CALL heimdb_branch_create('st')")
	b := openBranch(t, dsn, "st")
	defer b.Close()
	mustExec(t, b, "ALTER TABLE lg ADD note varchar(10) NOT NULL DEFAULT ''")
	mustExec(t, b, "UPDATE lg SET note = 'x' WHERE ip = 'b'")
	mustExec(t, b, "INSERT INTO lg VALUES ('d', 4, 'y')")
	mustExec(t, m, "UPDATE lg SET code = 30 WHERE ip = 'c'")
	mustExec(t, m, "DELETE FROM lg WHERE ip = 'a'")
	mustExec(t, m, "CALL heimdb_branch_merge('st')")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, code, note) FROM lg"); got != "b2x,c30,d4y" {
		t.Errorf("after merge: %s", got)
	}
	// 書き直した行のハッシュは新しい列を含む（変える行、消す行が見つかる）
	mustExec(t, m, "UPDATE lg SET code = 40 WHERE ip = 'c'")
	mustExec(t, m, "DELETE FROM lg WHERE ip = 'b'")
	if got := rowsOf(t, m, "SELECT CONCAT(ip, code, note) FROM lg"); got != "c40,d4y" {
		t.Errorf("after update and delete: %s", got)
	}
}

// 空の主キーのない表の保存のキーを変えると、新しいテーブル ID にする。前のバージョンの行と形が混ざらず、その前に戻せる
// （docs/plan.md の段階 72）。
func TestKeylessEmptyKeyChange(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE ea (x int NOT NULL)")
	mustExec(t, m, "CREATE TABLE eb (x int NOT NULL, UNIQUE KEY x (x))")
	mustExec(t, m, "INSERT INTO ea VALUES (1)")
	mustExec(t, m, "INSERT INTO eb VALUES (1)")
	mustExec(t, m, "DELETE FROM ea")
	mustExec(t, m, "DELETE FROM eb")
	time.Sleep(10 * time.Millisecond)
	t0 := time.Now().UTC().Format("2006-01-02 15:04:05.000000+00:00")
	time.Sleep(10 * time.Millisecond)
	mustExec(t, m, "ALTER TABLE ea ADD UNIQUE KEY x (x)")
	mustExec(t, m, "ALTER TABLE eb DROP INDEX x")
	mustExec(t, m, "INSERT INTO ea VALUES (2), (3)")
	mustExec(t, m, "INSERT INTO eb VALUES (2), (3)")
	for _, table := range []string{"ea", "eb"} {
		mustExec(t, m, "CALL heimdb_restore('main', '"+t0+"', '"+table+"')")
		if got := rowsOf(t, m, "SELECT COUNT(*) FROM "+table); got != "0" {
			t.Errorf("%s after restore: %s", table, got)
		}
	}
	// 一時テーブルの DDL はコミットしないので、トランザクションの中の行があれば空の表ではない
	c, err := m.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{
		"CREATE TEMPORARY TABLE tt (x int NOT NULL)",
		"CREATE TEMPORARY TABLE tu (x int NOT NULL, UNIQUE KEY x (x))",
		"BEGIN",
		"INSERT INTO tt VALUES (2), (1)",
		"INSERT INTO tu VALUES (2), (1)",
		"ALTER TABLE tt ADD UNIQUE KEY x (x)",
		"ALTER TABLE tu DROP INDEX x",
		"INSERT INTO tu VALUES (0)",
	} {
		if _, err := c.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var got []string
	for _, table := range []string{"tt", "tu"} {
		var v string
		if err := c.QueryRowContext(t.Context(), "SELECT GROUP_CONCAT(x) FROM "+table).Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if strings.Join(got, " ") != "1,2 1,2,0" {
		t.Errorf("temporary tables: %v", got)
	}
}

// 主キーのない表の SHOW INDEX は PRIMARY を出さず、見えないインデックスは見せず、消せない（docs/plan.md の段階 72）。
func TestKeylessIndexes(t *testing.T) {
	m, _, stop := branchServer(t)
	defer stop()
	mustExec(t, m, "CREATE TABLE lg (ip varchar(39) NOT NULL, n int NOT NULL, KEY ip (ip))")
	mustExec(t, m, "CREATE TABLE lk (ip varchar(39) NOT NULL, n int NOT NULL, UNIQUE KEY ip (ip))")
	mustExec(t, m, "INSERT INTO lg VALUES ('a', 1), ('a', 1)")
	for table, want := range map[string]string{"lg": "ip:1", "lk": "ip:0"} {
		rows, err := m.Query("SHOW INDEX FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		var got []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprintf("%s:%v", vals[2], vals[1]))
		}
		rows.Close()
		if strings.Join(got, ",") != want {
			t.Errorf("SHOW INDEX FROM %s: %v, want %s", table, got, want)
		}
	}
	if _, err := m.Exec("ALTER TABLE lg DROP INDEX __heimdb_row_hash"); err == nil {
		t.Error("dropping the hidden index: no error")
	}
	mustExec(t, m, "DELETE FROM lg LIMIT 1")
	if got := rowsOf(t, m, "SELECT COUNT(*) FROM lg"); got != "1" {
		t.Errorf("rows: %s", got)
	}
}
