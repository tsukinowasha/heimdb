<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// WP_Query の並び（post__in、同じ日付の記事。docs/plan.md の段階 84）、サイトヘルスの information_schema.TABLES
// （段階 86）。同順位の並びは仕様では決まらないが、MySQL の実際の並びに合わせる（CLAUDE.md）。

class QueryTest extends Compat_TestCase {
	/** @var array<string, int> */
	private $posts = array();

	public function set_up() {
		parent::set_up();
		$dates = array(
			'p1' => '2025-01-03 10:00:00',
			'p2' => '2025-01-01 10:00:00',
			'p3' => '2025-01-02 10:00:00',
			't1' => '2025-02-01 10:00:00',
			't2' => '2025-02-01 10:00:00',
			't3' => '2025-02-01 10:00:00',
		);
		foreach ( $dates as $title => $date ) {
			$this->posts[ $title ] = self::factory()->post->create(
				array(
					'post_title' => $title,
					'post_date'  => $date,
				)
			);
		}
	}

	private function titles( $args ) {
		$q = new WP_Query( $args + array( 'posts_per_page' => -1, 'post_type' => 'post' ) );
		return wp_list_pluck( $q->posts, 'post_title' );
	}

	public function test_post_in() {
		$in = array( $this->posts['p3'], $this->posts['p1'], $this->posts['p2'] );
		Compat::observe( 'default_order', $this->titles( array( 'post__in' => $in ) ) );
		Compat::observe( 'orderby_post_in', $this->titles( array( 'post__in' => $in, 'orderby' => 'post__in' ) ) );
	}

	public function test_same_date() {
		// 同じ日付の 3 件（ID は作った順）。MySQL が同順位をどう並べるかを、ID の大小で記録する
		$t = $this->titles( array( 'date_query' => array( array( 'year' => 2025, 'month' => 2 ) ) ) );
		Compat::observe( 'desc', $t );
		$t = $this->titles( array( 'date_query' => array( array( 'year' => 2025, 'month' => 2 ) ), 'order' => 'ASC' ) );
		Compat::observe( 'asc', $t );
		Compat::observe( 'paged', $this->titles( array( 'date_query' => array( array( 'year' => 2025, 'month' => 2 ) ), 'posts_per_page' => 2, 'paged' => 2 ) ) );
	}

	public function test_found_rows() {
		// このテストの記事だけ（WordPress が入れる記事は、ほかのクラスの後片付けで消えるかどうかが流す順で変わる）
		$q = new WP_Query( array( 'post_type' => 'post', 'posts_per_page' => 2, 'post__in' => array_values( $this->posts ) ) );
		Compat::observe( 'found', array( $q->found_posts, $q->max_num_pages ) );
	}

	public function test_site_health_tables() {
		global $wpdb;
		$names = "'{$wpdb->posts}', '{$wpdb->options}'";
		// class-wp-site-health.php と同じ形。行の数と大きさは推定で、MySQL と同じにはならないので、名前と型だけを見る
		$rows = $wpdb->get_results(
			$wpdb->prepare(
				"SELECT TABLE_NAME AS 'table', TABLE_ROWS AS 'rows', SUM(data_length + index_length) as 'bytes' FROM information_schema.TABLES WHERE TABLE_SCHEMA = %s AND TABLE_NAME IN ($names) GROUP BY TABLE_NAME ORDER BY TABLE_NAME",
				DB_NAME
			),
			ARRAY_A
		);
		Compat::observe(
			'tables',
			array_map(
				function ( $r ) {
					return array( $r['table'], is_numeric( $r['rows'] ), is_numeric( $r['bytes'] ) );
				},
				$rows
			)
		);
		Compat::observe( 'error', $wpdb->last_error );
	}
}
