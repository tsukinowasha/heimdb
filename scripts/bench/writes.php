<?php
/**
 * 書き込みの時間を測る（docs/bench.md）。wp-cli の起動の時間を含めないよう、PHP の中で繰り返して測る。
 * 作った記事とコメントは最後に消すので、何度流してもデータは元に戻る。
 *
 *   docker compose -f scripts/bench/docker-compose.yml run --rm -T cli-mysql wp eval-file - < scripts/bench/writes.php
 */
$n = (int) ( getenv( 'BENCH_N' ) ?: 200 );

function bench_db_time() {
	global $wpdb;
	$t = 0.0;
	foreach ( (array) $wpdb->queries as $q ) {
		$t += $q[1];
	}
	return $t;
}

function bench( $label, $n, $fn ) {
	global $wpdb;
	$wpdb->queries = array();
	$start         = microtime( true );
	for ( $i = 0; $i < $n; $i++ ) {
		$fn( $i );
	}
	$total = microtime( true ) - $start;
	printf( "%s\t%.2f\t%.2f\t%.1f\n", $label, $total / $n * 1000, bench_db_time() / $n * 1000, count( $wpdb->queries ) / $n );
}

wp_suspend_cache_addition( false );
$author = 1;
$ids    = array();

echo "op\tms_per_op\tdb_ms_per_op\tqueries_per_op\n";
bench(
	'記事の作成',
	$n,
	function ( $i ) use ( &$ids, $author ) {
		$ids[] = wp_insert_post(
			array(
				'post_title'    => "Bench post $i",
				'post_content'  => str_repeat( 'Lorem ipsum dolor sit amet. ', 40 ),
				'post_status'   => 'publish',
				'post_author'   => $author,
				'post_category' => array( 1 ),
				'tags_input'    => array( 'bench-a', 'bench-b' ),
			)
		);
	}
);
bench(
	'コメントの追加',
	$n,
	function ( $i ) use ( &$ids ) {
		wp_insert_comment(
			array(
				'comment_post_ID'      => $ids[ $i % count( $ids ) ],
				'comment_author'       => "Bench $i",
				'comment_author_email' => "bench$i@example.com",
				'comment_content'      => 'Bench comment.',
				'comment_approved'     => 1,
			)
		);
	}
);
bench(
	'オプションの更新',
	$n,
	function ( $i ) {
		update_option( 'bench_option', "value $i" );
	}
);
bench(
	'記事の削除',
	$n,
	function ( $i ) use ( &$ids ) {
		wp_delete_post( $ids[ $i ], true );
	}
);
delete_option( 'bench_option' );
foreach ( array( 'bench-a', 'bench-b' ) as $slug ) {
	$term = get_term_by( 'slug', $slug, 'post_tag' );
	if ( $term ) {
		wp_delete_term( $term->term_id, 'post_tag' );
	}
}
