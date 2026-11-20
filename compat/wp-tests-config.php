<?php
// SPDX-License-Identifier: GPL-2.0-or-later
//
// MySQL 互換のテスト集の WordPress のテストの設定（compat/README.md）。wordpress-develop を /app に、このディレクトリを
// /compat にマウントしたコンテナで読む（scripts/compat/run.sh）。接続先とデータベースは環境変数で決める。
// DB_CHARSET と DB_COLLATE は WordPress の既定（wpdb が SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci を送る）。
// COMPAT_DB_CHARSET は、接続の確かめ（bootstrap.php）が止まることを試すときだけ使う。

define( 'ABSPATH', '/app/src/' );

define( 'DB_NAME', getenv( 'WP_TESTS_DB_NAME' ) ?: 'heimdb_compat' );
define( 'DB_USER', getenv( 'WP_TESTS_DB_USER' ) ?: 'root' );
define( 'DB_PASSWORD', getenv( 'WP_TESTS_DB_PASSWORD' ) ?: '' );
define( 'DB_HOST', getenv( 'WP_TESTS_DB_HOST' ) ?: 'host.docker.internal:3306' );
define( 'DB_CHARSET', getenv( 'COMPAT_DB_CHARSET' ) ?: 'utf8mb4' );
define( 'DB_COLLATE', '' );

define( 'AUTH_KEY', 'compat' );
define( 'SECURE_AUTH_KEY', 'compat' );
define( 'LOGGED_IN_KEY', 'compat' );
define( 'NONCE_KEY', 'compat' );
define( 'AUTH_SALT', 'compat' );
define( 'SECURE_AUTH_SALT', 'compat' );
define( 'LOGGED_IN_SALT', 'compat' );
define( 'NONCE_SALT', 'compat' );

$table_prefix = 'wptests_';

define( 'WP_TESTS_DOMAIN', 'example.org' );
define( 'WP_TESTS_EMAIL', 'admin@example.org' );
define( 'WP_TESTS_TITLE', 'Test Blog' );
define( 'WP_PHP_BINARY', 'php' );
define( 'WPLANG', '' );
