<?php
/**
 * 負荷の測り（docs/bench.md の段階 126）の書き込みのページの文を取る。scripts/bench/writes.php と同じ 4 つの書き込み
 * （記事の作成、コメントの追加、オプションの更新、記事の削除）を、記事ごとにこの順で BENCH_N 回（既定 6）繰り返し、
 * 回ごとに $wpdb->queries の文と、その回に作った ID と日時を JSON で書く。SAVEQUERIES が要る。
 * 最初の回はタグを作るなどの初めの文を含むので、mkwrites.py は後ろの回を型にする。最後に作ったものを消す。
 *
 *   docker compose -f scripts/bench/docker-compose.yml run --rm -T cli-mysql wp eval-file - < scripts/bench/load/capture-writes.php > writes-capture.json
 */
$n = (int) ( getenv( 'BENCH_N' ) ?: 6 );

function cap_take() {
	global $wpdb;
	$q             = array();
	foreach ( (array) $wpdb->queries as $x ) {
		$q[] = $x[0];
	}
	$wpdb->queries = array();
	return $q;
}

wp_suspend_cache_addition( false );
global $wpdb;
$out = array();
for ( $i = 0; $i < $n; $i++ ) {
	$it            = array( 'i' => $i );
	$wpdb->queries = array();
	$title         = "Bench post 9000000$i";
	$id            = wp_insert_post(
		array(
			'post_title'    => $title,
			'post_content'  => str_repeat( 'Lorem ipsum dolor sit amet. ', 40 ),
			'post_status'   => 'publish',
			'post_author'   => 1,
			'post_category' => array( 1 ),
			'tags_input'    => array( 'bench-a', 'bench-b' ),
		)
	);
	$it['post_create'] = cap_take();
	$p                 = get_post( $id );
	$it['post']        = $id;
	$it['post_date']   = $p->post_date;
	$it['post_date_gmt'] = $p->post_date_gmt;
	$wpdb->queries     = array();
	$cid               = wp_insert_comment(
		array(
			'comment_post_ID'      => $id,
			'comment_author'       => "Bench 9000000$i",
			'comment_author_email' => "bench9000000$i@example.com",
			'comment_content'      => 'Bench comment.',
			'comment_approved'     => 1,
		)
	);
	$it['comment_add'] = cap_take();
	$c                 = get_comment( $cid );
	$it['comment']     = $cid;
	$it['comment_date'] = $c->comment_date;
	$it['comment_date_gmt'] = $c->comment_date_gmt;
	$wpdb->queries     = array();
	update_option( 'bench_option', "value 9000000$i" );
	$it['option_update'] = cap_take();
	$wpdb->queries       = array();
	wp_delete_post( $id, true );
	$it['post_delete'] = cap_take();
	$it['tags']        = array();
	foreach ( array( 'bench-a', 'bench-b' ) as $slug ) {
		$t = get_term_by( 'slug', $slug, 'post_tag' );
		$it['tags'][ $slug ] = $t ? array( (int) $t->term_id, (int) $t->term_taxonomy_id ) : null;
	}
	$out[] = $it;
}
$wpdb->queries = array();
delete_option( 'bench_option' );
foreach ( array( 'bench-a', 'bench-b' ) as $slug ) {
	$term = get_term_by( 'slug', $slug, 'post_tag' );
	if ( $term ) {
		wp_delete_term( $term->term_id, 'post_tag' );
	}
}
echo json_encode( $out, JSON_PRETTY_PRINT | JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES );
