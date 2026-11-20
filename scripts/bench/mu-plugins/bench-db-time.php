<?php
/**
 * ベンチマーク用: 1 回のリクエストで DB にかかった時間とクエリの数を、レスポンスヘッダ X-DB-Time（秒）と
 * X-DB-Queries に出す。SAVEQUERIES が要る。ヘッダを最後に足すため、出力をすべて溜める。
 */
if ( PHP_SAPI === 'cli' ) {
	return;
}
ob_start();
add_action(
	'shutdown',
	function () {
		global $wpdb;
		$time = 0.0;
		foreach ( (array) $wpdb->queries as $q ) {
			$time += $q[1];
		}
		if ( ! headers_sent() ) {
			header( sprintf( 'X-DB-Time: %.6f', $time ) );
			header( 'X-DB-Queries: ' . count( (array) $wpdb->queries ) );
		}
	},
	0
);
