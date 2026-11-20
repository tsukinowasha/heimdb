<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// 数と文字列の変換、double の字、DECIMAL（docs/plan.md の段階 85、88）。

class NumbersTest extends Compat_TestCase {
	public function test_string_to_number() {
		Compat::observe_sql( 'cast', "SELECT CAST('12abc' AS SIGNED), CAST(' 12 ' AS SIGNED), CAST('abc' AS UNSIGNED), '12abc' + 0, '1e3x' + 0" );
		Compat::observe_sql( 'compare', "SELECT '12abc' = 12, ' 12 ' = 12, 'abc' = 0, '0x0C' = 12" );
	}

	public function test_double_text() {
		Compat::observe_sql( 'literals', 'SELECT 20250331e0, 0.00000015e0, 1e15, 1e14, -1e15, 1.5e-16, 0.1e0 + 0.2e0' );
		Compat::observe_sql( 'concat', 'SELECT CONCAT(1e15), CONCAT(20250331e0), CAST(0.00000015e0 AS CHAR)' );

		$post = self::factory()->post->create( array( 'post_title' => 'views' ) );
		add_post_meta( $post, 'views', '1000000' );
		add_post_meta( $post, 'views', '135.5' );
		add_post_meta( $post, 'rating', '4.25' );
		global $wpdb;
		Compat::observe_sql( 'meta_plus_zero', "SELECT meta_value + 0 FROM {$wpdb->postmeta} WHERE post_id = $post AND meta_key = 'views' ORDER BY meta_id" );
		Compat::observe_sql( 'sum', "SELECT SUM(meta_value + 0), AVG(meta_value + 0), MAX(meta_value + 0) / 3 FROM {$wpdb->postmeta} WHERE post_id = $post AND meta_key = 'views'" );
	}

	public function test_decimal() {
		Compat::observe_sql( 'cast', "SELECT CAST('1.5' AS DECIMAL), CAST('2.5' AS DECIMAL), CAST('1.555' AS DECIMAL(10,2)), CAST('12abc' AS DECIMAL(5,1))" );
		Compat::observe_sql( 'out_of_range', 'SELECT CAST(123456.78 AS DECIMAL(5,1)), CAST(-123456.78 AS DECIMAL(5,1))' );
		Compat::observe_sql( 'bad_precision', 'SELECT CAST(1 AS DECIMAL(66))' );
	}

	public function test_division_and_zero_dates() {
		// wpdb が残す sql_mode（ERROR_FOR_DIVISION_BY_ZERO、NO_ZERO_IN_DATE）に関わるもの
		Compat::observe_sql( 'division', 'SELECT 1 / 0, 1 DIV 0, MOD(1, 0)' );
		Compat::observe_sql( 'zero_in_date', "SELECT CAST('2025-00-10' AS DATE), CAST('2025-01-00' AS DATE)" );
	}
}
