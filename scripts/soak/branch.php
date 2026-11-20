<?php
/**
 * 長時間の負荷試験のブランチの一巡の WordPress 側（docs/bench.md の「段階 108」）。cycle.sh が流す。
 *
 * SOAK_BRANCH_MODE=write（ブランチにつないで流す）: 本番の書き込みと同じ行を変えないように、カテゴリ soak-branch
 * だけを使う。記事を 1 件足してコメントを 2 件付け、前の一巡でマージした記事を 1 件直す。
 * SOAK_BRANCH_MODE=cleanup（本番で流す）: SOAK_TTL 秒より古い soak-branch の記事を消す。
 *
 *   wp eval-file /soak/branch.php
 */
$mode = getenv( 'SOAK_BRANCH_MODE' ) ?: 'write';
$ttl  = (int) ( getenv( 'SOAK_TTL' ) ?: 7200 );
$cat  = term_exists( 'soak-branch', 'category' );
if ( ! $cat ) {
	$cat = wp_insert_term( 'soak-branch', 'category' );
}
$cat = (int) $cat['term_id'];

if ( 'cleanup' === $mode ) {
	$before = gmdate( 'Y-m-d H:i:s', time() - $ttl );
	$old    = get_posts( array( 'category' => $cat, 'numberposts' => -1, 'fields' => 'ids', 'post_status' => 'any', 'date_query' => array( array( 'before' => $before, 'column' => 'post_date_gmt' ) ) ) );
	foreach ( $old as $pid ) {
		wp_delete_post( $pid, true );
	}
	printf( "cleanup deleted=%d\n", count( $old ) );
	return;
}

$prev = get_posts( array( 'category' => $cat, 'numberposts' => 1, 'fields' => 'ids', 'orderby' => 'ID', 'order' => 'DESC' ) );
$id   = wp_insert_post(
	array(
		'post_title'    => 'branch ' . gmdate( 'H:i' ),
		'post_content'  => str_repeat( 'written on a branch. ', 40 ),
		'post_status'   => 'publish',
		'post_category' => array( $cat ),
		'meta_input'    => array( '_soak' => 1, 'soak_note' => 'branch' ),
	),
	true
);
if ( is_wp_error( $id ) ) {
	WP_CLI::error( $id->get_error_message() );
}
for ( $i = 0; $i < 2; $i++ ) {
	wp_insert_comment(
		array(
			'comment_post_ID'  => $id,
			'comment_author'   => 'soak-branch',
			'comment_content'  => "branch comment $i",
			'comment_approved' => 1,
		)
	);
}
if ( $prev ) {
	wp_update_post( array( 'ID' => $prev[0], 'post_content' => str_repeat( 'edited on a branch. ', 40 ) ) );
}
printf( "write post=%d edited=%d\n", $id, $prev ? $prev[0] : 0 );
