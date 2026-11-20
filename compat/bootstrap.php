<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// MySQL 互換のテスト集の起動（compat/README.md）。WordPress のテストの仕組みを読み、接続の条件を確かめ、テストが
// 記録した値（Compat::observe）を、終わるときに COMPAT_OUT の JSON に書く。

require '/app/tests/phpunit/includes/bootstrap.php';

/**
 * テストが観察した値を集める。値は mysqli が返したままの字（NULL は null）で、期待値は書かない。同じテストを本物の
 * MySQL と調べる DB に流し、JSON を cmd/compatdiff で並べる。
 */
class Compat {
	/** @var array<string, mixed> */
	public static $observed = array();

	/** @var string 今のテストの名前（Compat_TestCase::set_up が入れる） */
	public static $test = '';

	/**
	 * 値を記録する。キーは「テストのクラス::テストの名前::label」。
	 */
	public static function observe( $label, $value ) {
		$key = self::$test . '::' . $label;
		if ( array_key_exists( $key, self::$observed ) ) {
			throw new LogicException( "observed twice: $key" );
		}
		self::$observed[ $key ] = $value;
	}

	/**
	 * 文を wpdb で流し、結果、エラー（文、番号、SQLSTATE）、行の数、警告を返す。
	 *
	 * @return array<string, mixed>
	 */
	public static function run( $sql ) {
		global $wpdb;
		$wpdb->suppress_errors( true );
		$ret = $wpdb->query( $sql );
		$dbh = $wpdb->dbh;
		$out = array(
			'error'    => $wpdb->last_error,
			'errno'    => $dbh->errno,
			'sqlstate' => $dbh->sqlstate,
		);
		if ( preg_match( '/^\s*(insert|delete|update|replace)\s/i', $sql ) ) {
			$out['affected'] = false === $ret ? null : $wpdb->rows_affected;
		} elseif ( false !== $ret && is_array( $wpdb->last_result ) ) {
			// wpdb は行を列の名前のオブジェクトで返すので、同じ名前の列は 1 つになる。数が合わなければ記録しない
			$names       = (array) $wpdb->get_col_info( 'name' );
			$out['rows'] = array_map(
				function ( $row ) use ( $names, $sql ) {
					$values = array_values( get_object_vars( $row ) );
					if ( count( $values ) !== count( $names ) ) {
						throw new LogicException( "columns with the same name in: $sql" );
					}
					return $values;
				},
				$wpdb->last_result
			);
		}
		// 警告は次の文で読む（SHOW WARNINGS も文なので、ほかの文を挟まない）
		$out['warnings'] = array_map(
			function ( $row ) {
				return array_values( get_object_vars( $row ) );
			},
			(array) $wpdb->get_results( 'SHOW WARNINGS' )
		);
		return $out;
	}

	/**
	 * 文を流した結果を label で記録する。
	 */
	public static function observe_sql( $label, $sql ) {
		self::observe( $label, self::run( $sql ) );
	}

	/**
	 * SELECT の 1 列目を並べて返す（記録の値を短くする）。
	 *
	 * @return array<int, string|null>
	 */
	public static function column( $sql ) {
		global $wpdb;
		return $wpdb->get_col( $sql );
	}
}

/**
 * テスト集の基底。テストごとの名前を Compat に入れ、エラーを画面に出さない（last_error には残る）。
 * WP_UnitTestCase はテストごとにトランザクションで包み、CREATE TABLE を一時テーブルに書き換えるので、表は
 * set_up_before_class で作る。
 */
abstract class Compat_TestCase extends WP_UnitTestCase {
	public function set_up() {
		parent::set_up();
		global $wpdb;
		$wpdb->suppress_errors( true );
		Compat::$test = static::class . '::' . $this->getName();
	}
}

// 接続の条件を確かめる。WordPress と同じ（文字セットは utf8mb4、照合順序は wpdb が選ぶ utf8mb4_unicode_520_ci、
// sql_mode に wpdb が外すものがない）でなければ、比べても意味がないので止める。sql_mode そのものは記録する
// （wpdb はサーバの sql_mode から外すだけなので、サーバの既定の違いはここに出る）。
global $wpdb;
$compat_conn = $wpdb->get_row( 'SELECT @@character_set_client, @@character_set_connection, @@character_set_results, @@collation_connection, @@SESSION.sql_mode', ARRAY_N );
$compat_want = array( 'utf8mb4', 'utf8mb4', 'utf8mb4', 'utf8mb4_unicode_520_ci' );
if ( array_slice( (array) $compat_conn, 0, 4 ) !== $compat_want ) {
	fwrite( STDERR, 'compat: the connection is not set up as WordPress sets it: ' . wp_json_encode( $compat_conn ) . "\n" );
	exit( 2 );
}
foreach ( array( 'STRICT_TRANS_TABLES', 'STRICT_ALL_TABLES', 'TRADITIONAL', 'ONLY_FULL_GROUP_BY', 'NO_ZERO_DATE', 'ANSI' ) as $compat_mode ) {
	if ( in_array( $compat_mode, explode( ',', $compat_conn[4] ), true ) ) {
		fwrite( STDERR, "compat: sql_mode has $compat_mode, which WordPress removes: {$compat_conn[4]}\n" );
		exit( 2 );
	}
}
Compat::$test = 'connection';
Compat::observe( 'charset', array_slice( $compat_conn, 0, 4 ) );
Compat::observe( 'sql_mode', $compat_conn[4] );

register_shutdown_function(
	function () {
		$path = getenv( 'COMPAT_OUT' );
		if ( ! $path ) {
			return;
		}
		ksort( Compat::$observed );
		file_put_contents( $path, json_encode( Compat::$observed, JSON_PRETTY_PRINT | JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES ) . "\n" );
	}
);
