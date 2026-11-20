<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// 文字列の列と数の比べ、meta_query の NUMERIC と DECIMAL（docs/plan.md の段階 85、88）。

class ComparisonTest extends Compat_TestCase {
	const VALUES = array( '1', '1abc', ' 1 ', '1.0', '0', 'abc', '', '12', '999.5', '1000', '1500', '12abc', '0x0C', '１' );

	public static function set_up_before_class() {
		parent::set_up_before_class();
		global $wpdb;
		// 同じ値を、索引のある列 v と索引のない列 w に入れる
		$wpdb->query( 'DROP TABLE IF EXISTS compat_text' );
		$wpdb->query( 'CREATE TABLE compat_text (id INT NOT NULL PRIMARY KEY, v VARCHAR(50) NOT NULL, w VARCHAR(50) NOT NULL, KEY v (v)) ' . $wpdb->get_charset_collate() );
	}

	public static function tear_down_after_class() {
		global $wpdb;
		$wpdb->query( 'DROP TABLE IF EXISTS compat_text' );
		parent::tear_down_after_class();
	}

	public function set_up() {
		parent::set_up();
		global $wpdb;
		// 記録するのは値そのもの（記事のタイトルは "[値]"。ID は流す順で変わる）
		foreach ( self::VALUES as $i => $v ) {
			$id = self::factory()->post->create( array( 'post_title' => "[$v]" ) );
			add_post_meta( $id, 'compat_n', $v );
			$wpdb->insert( 'compat_text', array( 'id' => $i + 1, 'v' => $v, 'w' => $v ) );
		}
	}

	private function titles( $where ) {
		global $wpdb;
		return Compat::column( "SELECT p.post_title FROM {$wpdb->posts} p JOIN {$wpdb->postmeta} m ON m.post_id = p.ID WHERE $where ORDER BY m.meta_id" );
	}

	public function test_text_column_and_number() {
		global $wpdb;
		Compat::observe( 'value_eq_1', $this->titles( "m.meta_key = 'compat_n' AND m.meta_value = 1" ) );
		Compat::observe( 'value_eq_0', $this->titles( "m.meta_key = 'compat_n' AND m.meta_value = 0" ) );
		Compat::observe( 'value_eq_12', $this->titles( "m.meta_key = 'compat_n' AND m.meta_value = 12" ) );
		Compat::observe( 'value_in', $this->titles( "m.meta_key = 'compat_n' AND m.meta_value IN (1, 12)" ) );
		Compat::observe( 'value_ne_1', $this->titles( "m.meta_key = 'compat_n' AND m.meta_value <> 1" ) );
		Compat::observe( 'value_eq_string', $this->titles( "m.meta_key = 'compat_n' AND m.meta_value = '1'" ) );
		// 索引のある列とない列で同じになる（索引の範囲を数で引かない）
		foreach ( array( 'eq_1' => '= 1', 'eq_0' => '= 0', 'eq_12' => '= 12', 'in' => 'IN (1, 12)', 'gt_100' => '> 100' ) as $label => $cond ) {
			Compat::observe( "indexed_$label", Compat::column( "SELECT id FROM compat_text WHERE v $cond ORDER BY id" ) );
			Compat::observe( "unindexed_$label", Compat::column( "SELECT id FROM compat_text WHERE w $cond ORDER BY id" ) );
		}
	}

	public function test_meta_query_numeric() {
		$q = new WP_Query(
			array(
				'post_type'      => 'post',
				'posts_per_page' => -1,
				'orderby'        => 'title',
				'order'          => 'ASC',
				'meta_query'     => array(
					array(
						'key'     => 'compat_n',
						'value'   => 12,
						'compare' => '>=',
						'type'    => 'NUMERIC',
					),
				),
			)
		);
		Compat::observe( 'ge_12', wp_list_pluck( $q->posts, 'post_title' ) );
	}

	public function test_meta_query_decimal() {
		$q = new WP_Query(
			array(
				'post_type'      => 'post',
				'posts_per_page' => -1,
				'orderby'        => 'title',
				'order'          => 'ASC',
				'meta_query'     => array(
					array(
						'key'     => 'compat_n',
						'value'   => 1000,
						'compare' => '>=',
						'type'    => 'DECIMAL',
					),
				),
			)
		);
		Compat::observe( 'ge_1000', wp_list_pluck( $q->posts, 'post_title' ) );
	}

	public function test_orderby_meta_value_num() {
		$q = new WP_Query(
			array(
				'post_type'      => 'post',
				'posts_per_page' => -1,
				'meta_key'       => 'compat_n',
				'orderby'        => array(
					'meta_value_num' => 'ASC',
					'title'          => 'ASC',
				),
			)
		);
		Compat::observe( 'order', wp_list_pluck( $q->posts, 'post_title' ) );
	}
}
