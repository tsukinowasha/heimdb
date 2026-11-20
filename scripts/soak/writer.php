<?php
/**
 * 長時間の負荷試験の書き込みの 1 回分（docs/bench.md の「段階 108」）。writer.sh が毎分流す。
 *
 * 記事を 1 件足し、試験の記事を 1 件直し、コメントを SOAK_COMMENTS 件足し、SOAK_TTL 秒より古い試験の記事と
 * コメントを消す（データの量を一定にして、ディスクの伸びを GC の働きだけで見られるようにする）。
 * 期限付きの transient（autoload は off）を SOAK_TRANSIENTS 件足し、15 分に 1 回、期限の切れたものを消す
 * （ホスティングの wp_options にたまった形）。
 *
 *   wp eval-file /soak/writer.php
 */
$comments   = (int) ( getenv( 'SOAK_COMMENTS' ) ?: 6 );
$transients = (int) ( getenv( 'SOAK_TRANSIENTS' ) ?: 30 );
$ttl        = (int) ( getenv( 'SOAK_TTL' ) ?: 7200 );
$minute     = (int) floor( time() / 60 );

$cat = term_exists( 'soak-main', 'category' );
if ( ! $cat ) {
	$cat = wp_insert_term( 'soak-main', 'category' );
}
$cat = (int) $cat['term_id'];

// 記事を足して、試験の記事を 1 件直す（バージョンが残る）
$id = wp_insert_post(
	array(
		'post_title'    => "soak $minute",
		'post_content'  => str_repeat( "soak body $minute. ", 40 ),
		'post_status'   => 'publish',
		'post_category' => array( $cat ),
		'meta_input'    => array( '_soak' => 1, 'soak_views' => 0, 'soak_note' => "minute $minute" ),
	),
	true
);
if ( is_wp_error( $id ) ) {
	WP_CLI::error( $id->get_error_message() );
}
$soak_posts = get_posts(
	array( 'category' => $cat, 'numberposts' => -1, 'fields' => 'ids', 'post_status' => 'publish', 'orderby' => 'ID', 'order' => 'ASC' )
);
$edit       = $soak_posts[ array_rand( $soak_posts ) ];
wp_update_post( array( 'ID' => $edit, 'post_content' => str_repeat( "edited $minute. ", 40 ) ) );
update_post_meta( $edit, 'soak_views', (int) get_post_meta( $edit, 'soak_views', true ) + 1 );

// コメントは、元からある記事と試験の記事に付ける
global $wpdb;
$targets = $wpdb->get_col( "SELECT ID FROM $wpdb->posts WHERE post_type = 'post' AND post_status = 'publish' ORDER BY RAND() LIMIT $comments" );
foreach ( $targets as $i => $post_id ) {
	$cid = wp_insert_comment(
		array(
			'comment_post_ID'      => (int) $post_id,
			'comment_author'       => 'soak',
			'comment_author_email' => 'soak@example.com',
			'comment_content'      => "soak comment $minute-$i",
			'comment_approved'     => 1,
		)
	);
	add_comment_meta( $cid, '_soak', 1 );
}

// 古い試験のコメントと記事を消す（数を数え直す処理も WordPress が流す）
$before  = gmdate( 'Y-m-d H:i:s', time() - $ttl );
$old_cid = get_comments( array( 'meta_key' => '_soak', 'fields' => 'ids', 'number' => 0, 'date_query' => array( array( 'before' => $before, 'column' => 'comment_date_gmt' ) ) ) );
foreach ( $old_cid as $cid ) {
	wp_delete_comment( $cid, true );
}
$old_posts = get_posts( array( 'category' => $cat, 'numberposts' => -1, 'fields' => 'ids', 'post_status' => 'any', 'date_query' => array( array( 'before' => $before, 'column' => 'post_date_gmt' ) ) ) );
foreach ( $old_posts as $pid ) {
	wp_delete_post( $pid, true );
}

// 期限付きの transient（セッションの形）
for ( $i = 0; $i < $transients; $i++ ) {
	set_transient( 'soak_sess_' . wp_generate_password( 16, false ), str_repeat( 'x', 400 ), 600 );
}
$expired = 'no';
if ( $minute - (int) get_option( 'soak_last_expired', 0 ) >= 15 ) {
	delete_expired_transients( true );
	update_option( 'soak_last_expired', $minute, false );
	$expired = 'yes';
}

$options = (int) $wpdb->get_var( "SELECT COUNT(*) FROM $wpdb->options" );
printf(
	"%s\tpost=%d\tedit=%d\tcomments=%d\tdel_comments=%d\tdel_posts=%d\tsoak_posts=%d\texpired=%s\toptions=%d\n",
	gmdate( 'H:i:s' ),
	$id,
	$edit,
	count( $targets ),
	count( $old_cid ),
	count( $old_posts ),
	count( $soak_posts ),
	$expired,
	$options
);
