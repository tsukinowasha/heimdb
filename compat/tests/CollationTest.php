<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// 照合順序（日本語の全角と半角、ひらがなとカタカナ）、リテラル同士の比べ（docs/plan.md の段階 90）。表の照合順序は
// WordPress の既定（utf8mb4_unicode_520_ci）。

class CollationTest extends Compat_TestCase {
	public static function set_up_before_class() {
		parent::set_up_before_class();
		global $wpdb;
		$wpdb->query( 'DROP TABLE IF EXISTS compat_unique' );
		$wpdb->query( 'CREATE TABLE compat_unique (id INT NOT NULL PRIMARY KEY, v VARCHAR(191) NOT NULL, UNIQUE KEY v (v)) ' . $wpdb->get_charset_collate() );
	}

	public static function tear_down_after_class() {
		global $wpdb;
		$wpdb->query( 'DROP TABLE IF EXISTS compat_unique' );
		parent::tear_down_after_class();
	}

	public function test_literals() {
		Compat::observe_sql( 'compare', "SELECT 'a' = 'A', 'a' = 'ａ', 'ア' = 'あ', 'ア' = 'ｱ', '1' = '１', 'é' = 'e', 'a ' = 'a', 'ß' = 'ss'" );
		Compat::observe_sql( 'collation', "SELECT COLLATION('a'), CHARSET('a')" );
	}

	public function test_terms() {
		$a = wp_insert_term( 'あ', 'post_tag' );
		Compat::observe( 'insert_hiragana', is_wp_error( $a ) ? $a->get_error_code() : 'ok' );
		Compat::observe( 'exists_katakana', (bool) term_exists( 'ア', 'post_tag' ) );
		Compat::observe( 'exists_halfwidth', (bool) term_exists( 'ｱ', 'post_tag' ) );
		$t = get_term_by( 'name', 'ア', 'post_tag' );
		Compat::observe( 'get_by_name_katakana', $t ? $t->name : false );
		$k = wp_insert_term( 'ア', 'post_tag' );
		Compat::observe( 'insert_katakana', is_wp_error( $k ) ? $k->get_error_code() : 'ok' );
		$w = wp_insert_term( 'Ａ', 'post_tag' );
		$l = wp_insert_term( 'a', 'post_tag' );
		Compat::observe( 'insert_fullwidth_then_ascii', array( is_wp_error( $w ) ? $w->get_error_code() : 'ok', is_wp_error( $l ) ? $l->get_error_code() : 'ok' ) );
	}

	public function test_order_by() {
		global $wpdb;
		$names = array( 'b', 'B', 'ａ', 'A', 'a', 'あ', 'ア', 'ｱ', 'い', '1', '１', 'é', 'e', 'Z' );
		foreach ( $names as $i => $n ) {
			$wpdb->insert( 'compat_unique', array( 'id' => $i + 1, 'v' => $n . '-' . $i ) );
		}
		Compat::observe( 'order', Compat::column( 'SELECT v FROM compat_unique ORDER BY v, id' ) );
		// 文字列の式の DISTINCT と GROUP BY（照合順序で等しい値を一つにする）。式の照合順序も記録する。等しい値のうち
		// どれが残るかは読む順で変わる（MySQL は v の一意インデックスを読む）ので、数を記録する
		Compat::observe( 'distinct', Compat::column( "SELECT COUNT(*) FROM (SELECT DISTINCT SUBSTRING_INDEX(v, '-', 1) AS k FROM compat_unique) t" ) );
		Compat::observe( 'group_by', Compat::column( "SELECT COUNT(*) FROM compat_unique GROUP BY LEFT(v, 1) ORDER BY MIN(id)" ) );
		Compat::observe_sql( 'expression_collation', "SELECT COLLATION(SUBSTRING_INDEX(v, '-', 1)), COLLATION(LEFT(v, 1)) FROM compat_unique LIMIT 1" );
	}

	public function test_unique() {
		Compat::observe_sql( 'first', "INSERT INTO compat_unique (id, v) VALUES (1, 'あ')" );
		Compat::observe_sql( 'katakana', "INSERT INTO compat_unique (id, v) VALUES (2, 'ア')" );
		Compat::observe_sql( 'ignore', "INSERT IGNORE INTO compat_unique (id, v) VALUES (3, 'ｱ'), (4, 'い')" );
		Compat::observe_sql( 'primary', "INSERT INTO compat_unique (id, v) VALUES (1, 'う')" );
	}
}
