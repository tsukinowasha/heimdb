<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// 書き込み: 複数の表の DELETE（delete_expired_transients。docs/plan.md の段階 86）、insert_id と rows_affected、
// ON DUPLICATE KEY UPDATE、主キーのない表（段階 72）、変換の警告。insert_id は、AUTO_INCREMENT がロールバックで
// 戻らない（前のテストが使った番号が飛ぶ）ので、同じテストの最初の行の ID との差で記録する。

class DmlTest extends Compat_TestCase {
	public static function set_up_before_class() {
		parent::set_up_before_class();
		global $wpdb;
		$collate = $wpdb->get_charset_collate();
		foreach ( array( 'compat_ai', 'compat_keyless', 'compat_log', 'compat_w' ) as $t ) {
			$wpdb->query( "DROP TABLE IF EXISTS $t" );
		}
		$wpdb->query( "CREATE TABLE compat_ai (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, k VARCHAR(64) NOT NULL, n INT NOT NULL DEFAULT 0, PRIMARY KEY (id), UNIQUE KEY k (k)) $collate" );
		// プラグインがよく作る形: 主キーがなく一意キーで並べる表（Loginizer など）と、キーのない記録の表
		$wpdb->query( "CREATE TABLE compat_keyless (ip VARCHAR(40) NOT NULL, hits INT NOT NULL DEFAULT 0, seen INT NOT NULL DEFAULT 0, UNIQUE KEY ip (ip)) $collate" );
		$wpdb->query( "CREATE TABLE compat_log (at INT NOT NULL, msg VARCHAR(100) NOT NULL) $collate" );
		$wpdb->query( "CREATE TABLE compat_w (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, v VARCHAR(5), c CHAR(3), u INT UNSIGNED, d DECIMAL(5,2)) $collate" );
	}

	public static function tear_down_after_class() {
		global $wpdb;
		foreach ( array( 'compat_ai', 'compat_keyless', 'compat_log', 'compat_w' ) as $t ) {
			$wpdb->query( "DROP TABLE IF EXISTS $t" );
		}
		parent::tear_down_after_class();
	}

	public function test_delete_expired_transients() {
		global $wpdb;
		// WordPress が入れた期限切れのものを先に消し、消した行の数がこのテストの 3 組だけになるようにする
		delete_expired_transients( true );
		$past   = time() - 3600;
		$future = time() + 3600;
		foreach ( array( 'a' => $past, 'b' => $past, 'c' => $future ) as $name => $timeout ) {
			add_option( "_transient_compat_$name", "v$name", '', false );
			add_option( "_transient_timeout_compat_$name", $timeout, '', false );
		}
		// WordPress の delete_expired_transients と同じ文（消した行の数を見るため、直に流す）
		$sql = $wpdb->prepare(
			"DELETE a, b FROM {$wpdb->options} a, {$wpdb->options} b
			WHERE a.option_name LIKE %s
			AND a.option_name NOT LIKE %s
			AND b.option_name = CONCAT( '_transient_timeout_', SUBSTRING( a.option_name, 12 ) )
			AND b.option_value < %d",
			$wpdb->esc_like( '_transient_' ) . '%',
			$wpdb->esc_like( '_transient_timeout_' ) . '%',
			time()
		);
		Compat::observe_sql( 'delete', $sql );
		Compat::observe( 'left', Compat::column( "SELECT option_name FROM {$wpdb->options} WHERE option_name LIKE '%compat%' ORDER BY option_name" ) );
	}

	public function test_insert_id_and_affected() {
		global $wpdb;
		$one  = Compat::run( "INSERT INTO compat_ai (k) VALUES ('one')" );
		$base = $wpdb->insert_id;
		Compat::observe( 'single', $one['affected'] );
		$multi = Compat::run( "INSERT INTO compat_ai (k) VALUES ('two'), ('three'), ('four')" );
		// 複数の行の INSERT の insert_id は、MySQL では最初の行の ID
		Compat::observe( 'multi', array( $multi['affected'], $wpdb->insert_id - $base ) );
		$upd = Compat::run( "INSERT INTO compat_ai (k, n) VALUES ('two', 5) ON DUPLICATE KEY UPDATE n = n + VALUES(n)" );
		Compat::observe( 'odku_update', array( $upd['affected'], $upd['warnings'] ) );
		$same = Compat::run( "INSERT INTO compat_ai (k, n) VALUES ('two', 5) ON DUPLICATE KEY UPDATE n = n" );
		Compat::observe( 'odku_unchanged', $same['affected'] );
		$new = Compat::run( "INSERT INTO compat_ai (k, n) VALUES ('five', 1) ON DUPLICATE KEY UPDATE n = n + 1" );
		Compat::observe( 'odku_insert', array( $new['affected'], $wpdb->insert_id - $base ) );
		Compat::observe_sql( 'update_matched', "UPDATE compat_ai SET n = 0 WHERE k IN ('one', 'two')" );
		Compat::observe_sql( 'replace', "REPLACE INTO compat_ai (id, k, n) VALUES ($base, 'one', 9)" );
		Compat::observe_sql( 'ignore', "INSERT IGNORE INTO compat_ai (k) VALUES ('one'), ('six')" );
		Compat::observe( 'rows', Compat::column( "SELECT CONCAT(k, '=', n) FROM compat_ai ORDER BY k" ) );
	}

	public function test_keyless_tables() {
		$upsert = "INSERT INTO compat_keyless (ip, hits, seen) VALUES ('10.0.0.1', 1, 100) ON DUPLICATE KEY UPDATE hits = hits + 1, seen = VALUES(seen)";
		Compat::observe( 'upsert', array( Compat::run( $upsert )['affected'], Compat::run( $upsert )['affected'] ) );
		Compat::observe_sql( 'duplicate', "INSERT INTO compat_keyless (ip) VALUES ('10.0.0.1')" );
		Compat::observe_sql( 'select', 'SELECT ip, hits, seen FROM compat_keyless' );
		Compat::observe_sql( 'log_insert', "INSERT INTO compat_log VALUES (1, 'a'), (1, 'a'), (2, 'b')" );
		Compat::observe_sql( 'log_delete', 'DELETE FROM compat_log WHERE at = 1 LIMIT 1' );
		Compat::observe_sql( 'log_select', 'SELECT at, msg FROM compat_log ORDER BY at, msg' );
	}

	public function test_write_warnings() {
		// WordPress の sql_mode（strict でない）での、入らない値の書き込み
		Compat::observe_sql( 'truncate_string', "INSERT INTO compat_ai (k, n) VALUES ('" . str_repeat( 'x', 70 ) . "', 1)" );
		Compat::observe_sql( 'bad_number', "INSERT INTO compat_ai (k, n) VALUES ('w1', '12abc')" );
		Compat::observe_sql( 'out_of_range', "INSERT INTO compat_ai (k, n) VALUES ('w2', 99999999999)" );
		Compat::observe_sql( 'rows', "SELECT k, n FROM compat_ai WHERE k LIKE 'w%' OR k LIKE 'xxx%' ORDER BY k" );
	}

	public function test_write_fit() {
		global $wpdb;
		// 列に収まらない値（docs/plan.md の段階 92）: 日本語の切り詰め、末尾の空白（Note）、CHAR の末尾の空白、UNSIGNED
		// の負の数、DECIMAL の端と小数の丸め。UPDATE と INSERT IGNORE も同じ
		Compat::observe_sql( 'insert', "INSERT INTO compat_w (v, c, u, d) VALUES ('あいうえおか', 'ab   ', -1, 12345), ('abcde   ', 'abcd', '-5', 1.005), ('abc', 'x', 5000000000, '99999abc')" );
		Compat::observe_sql( 'update', "UPDATE compat_w SET v = CONCAT(v, 'zzzzzz'), d = -99999 WHERE v = 'abc'" );
		Compat::observe_sql( 'ignore', "INSERT IGNORE INTO compat_w (v, u) VALUES ('かきくけこさ', -1)" );
		Compat::observe_sql( 'rows', 'SELECT v, c, u, d FROM compat_w ORDER BY id' );
		// wpdb::insert は列の長さを自分で確かめ、長すぎる値は DB に送らずに失敗する
		Compat::observe( 'wpdb_insert', array( $wpdb->insert( 'compat_w', array( 'v' => 'abcdefgh' ) ), $wpdb->last_error ) );
	}
}
