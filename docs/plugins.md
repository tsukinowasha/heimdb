# Plugins that work with HeimDB

This list shows whether WordPress plugins work on HeimDB. Because HeimDB is WordPress-only, it rejects some MySQL features with an explicit error (see "Criteria" below). When a plugin uses such a feature, part or all of the plugin does not work: a table cannot be created, a statement fails, and so on.

157 plugins were checked (checked on 2026-10-16 to 25, from HeimDB 0.2.9 to stage 74, and checked again on 2026-11-06 at stage 107, except `hello`, which does not use the database, and `s3-uploads`, which is distributed on GitHub). They are the top 100 popular plugins on wordpress.org and the plugins used on WordPress sites that Tsukinowa Inc. runs. How they were chosen and checked, and the HeimDB differences found along the way, are in [`plugins-research.md`](plugins-research.md).

## Verdicts

| Verdict | Meaning |
|---|---|
| Works | Does not use SQL that HeimDB rejects (after activating it and loading the front page and the admin screens and running the scheduled cron, no error appears that differs from MySQL) |
| Partly works | Usually works, but HeimDB rejects a specific feature (the feature is named) |
| Does not work | Cannot be activated, or HeimDB rejects its main features |

Table columns:

- **Popularity**: rank by popularity on wordpress.org (the top 100 on 2026-10-20, `https://wordpress.org/plugins/browse/popular/`).
- **Version**: the version checked. Usually the latest version on wordpress.org on the day it was checked. "Version obtained" means a paid plugin or one not on wordpress.org, where the newest version we could get was checked (it may not be the latest version). "Official site" means the version distributed on the author's site.
- **Method**: static (inspecting the SQL in the source), activation (activating it in WordPress on HeimDB), comparison (running the main features on HeimDB and on real MySQL 8.0 and comparing the statements that failed), in use (checked by actually using it on a WordPress site running on HeimDB). See "Method" in [`plugins-research.md`](plugins-research.md) for details.
- "Same on MySQL" means real MySQL 8.0 returns the same error (it is not a HeimDB difference).

## Plugins that do not work or partly work

- **WooCommerce** (`woocommerce`, partly works): full-text search of orders (only when enabled in the settings) does not work, because HeimDB rejects full-text indexes. Orders, products, attribute filtering and the admin screens work the same as on MySQL.

HeimDB 0.2.21 and earlier returned a syntax error for table maintenance statements (`OPTIMIZE TABLE`, `CHECK TABLE`, `REPAIR TABLE`). Optimization in WP-Optimize, optimization and the corrupted table check in Advanced Database Cleaner, table optimization in WP-Sweep, WordPress's `wp-admin/maint/repair.php`, and wp-cli's `wp db check` / `optimize` / `repair` failed (fixed in stage 74; all of them now do nothing and return OK).

HeimDB 0.2.19 and earlier could not create tables without a primary key, so WP Cerber and Loginizer did not work (fixed in stage 72; see "Verdicts that changed" in [`plugins-research.md`](plugins-research.md)).

HeimDB 0.2.18 and earlier could not create tables whose primary key contains a string, a byte string, a date or an `ENUM`. WPML and its add-ons, Wordfence and WP Reviews for Google did not work, and parts of WooCommerce (attribute filtering) and WPForms Lite (analytics) did not work (fixed in 0.2.19; see "Verdicts that changed" in [`plugins-research.md`](plugins-research.md)).

HeimDB 0.2.17 and earlier returned wrong answers to plugins that check whether an index exists with `SHOW INDEX ... WHERE` (those noted in the table below). A plugin activated in that period may be missing an index it should have added (check with `SHOW INDEX FROM table`; see "HeimDB differences found" in [`plugins-research.md`](plugins-research.md)).

## List

| Plugin | Popularity | Version | Verdict | Method | Checked | Notes |
|---|---|---|---|---|---|---|
| `acfml` |  | 2.1.3 (version obtained) | Works | Static, activation | 2026-11-06 | Requires WPML (checked with WPML active) |
| `ad-inserter` |  | 2.8.20 | Works | In use, static, activation | 2026-11-06 |  |
| `addquicktag` |  | 2.6.2 | Works | In use, static, activation | 2026-11-06 |  |
| `adjust-admin-categories` |  | 2.2.6 | Works | In use, static, activation | 2026-11-06 |  |
| `admin-menu-editor` |  | 1.15.2 | Works | Static, activation | 2026-11-06 |  |
| `advanced-custom-fields` | 28 | 6.8.11 | Works | Static, activation | 2026-11-06 |  |
| `advanced-database-cleaner` |  | 4.2.0 | Works | Static, activation | 2026-11-06 | Table optimization (`ANALYZE TABLE` after `OPTIMIZE TABLE`), the corrupted table check (`CHECK TABLE` on 20 tables at a time) and repair failed with a syntax error in 0.2.21 and earlier (fixed in stage 74) |
| `advanced-custom-fields-pro` |  | 6.3.11 (version obtained) | Works | In use, static, activation | 2026-11-06 |  |
| `akismet` | 8 | 5.7.2 | Works | Static, activation | 2026-11-06 |  |
| `all-in-one-seo-pack` | 21 | 5.0.5 | Works | Static, activation | 2026-11-06 | All 13 tables (including Action Scheduler) have an integer primary key |
| `all-in-one-wp-migration` | 10 | 7.113 | Works | Static, activation | 2026-11-06 |  |
| `all-in-one-wp-security-and-firewall` | 59 | 5.5.1 | Works | Static, activation | 2026-11-06 |  |
| `antispam-bee` | 85 | 2.11.13 | Works | Static, activation | 2026-11-06 |  |
| `astra-sites` | 32 | 4.7.10 | Works | Static, activation | 2026-11-06 |  |
| `autoptimize` | 77 | 3.1.17 | Works | Static, activation | 2026-11-06 |  |
| `backuply` | 99 | 1.6.0 | Works | Static, activation | 2026-11-06 | Creating a backup was not checked (it runs over HTTP to the site itself) |
| `better-search-replace` | 40 | 1.4.11 | Works | Static, activation, comparison | 2026-11-06 | Replacing text in post content and in serialized options succeeded |
| `better-wp-security` | 87 | 10.0.6 | Works | Static, activation | 2026-11-06 | `itsec_webauthn_credentials`, which only the Pro version creates, has a latin1 column (the free version does not create it) |
| `bogo` |  | 3.9.3 | Works | In use, static, activation | 2026-11-06 |  |
| `breadcrumb-navxt` | 80 | 7.5.2 | Works | Static, activation | 2026-11-06 |  |
| `bulk-move` |  | 1.3.0 | Works | Static, activation | 2026-11-06 |  |
| `classic-editor` | 4 | 1.7.0 | Works | In use, static, activation | 2026-11-06 |  |
| `classic-widgets` | 25 | 0.3 | Works | In use, static, activation | 2026-11-06 |  |
| `click-to-chat-for-whatsapp` | 93 | 4.46 | Works | Static, activation | 2026-11-06 |  |
| `code-snippets` | 33 | 4.1.0 | Works | Static, activation | 2026-11-06 |  |
| `coming-soon` | 92 | 6.20.11 | Works | Static, activation | 2026-11-06 |  |
| `complianz-gdpr` | 44 | 7.5.7 | Works | Static, activation | 2026-11-06 |  |
| `contact-form-7` | 3 | 6.2.4.1 | Works | In use, static, activation | 2026-11-06 |  |
| `cookie-law-info` | 39 | 3.5.8 | Works | Static, activation | 2026-11-06 |  |
| `cookie-notice` | 75 | 3.1.18 | Works | Static, activation | 2026-11-06 |  |
| `creame-whatsapp-me` | 88 | 6.4.2 | Works | Static, activation | 2026-11-06 |  |
| `custom-css-js` | 100 | 3.55 | Works | Static, activation | 2026-11-06 |  |
| `custom-post-type-permalinks` |  | 3.5.6 | Works | In use, static, activation | 2026-11-06 |  |
| `custom-post-type-ui` | 50 | 1.19.3 | Works | In use, static, activation | 2026-11-06 |  |
| `disable-comments` | 38 | 2.9.1 | Works | Static, activation | 2026-11-06 |  |
| `disable-emojis` |  | 1.9.3 | Works | In use, static, activation | 2026-11-06 |  |
| `duplicate-page` | 16 | 4.5.9 | Works | In use, static, activation | 2026-11-06 |  |
| `duplicate-post` | 15 | 4.7 | Works | Static, activation | 2026-11-06 |  |
| `duplicator` | 41 | 5.1.1 | Works | Static, activation | 2026-11-06 | Creating a backup was not checked (it runs over HTTP to the site itself). Calls `wpdb::check_connection()` (`DO 1`) along the way. Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `duracelltomi-google-tag-manager` | 83 | 2.1.1 | Works | Static, activation | 2026-11-06 |  |
| `easy-svg` |  | 4.2.1 | Works | In use, static, activation | 2026-11-06 |  |
| `elementor` | 1 | 4.3.6 | Works | Static, activation | 2026-11-06 |  |
| `elementskit-lite` | 31 | 4.1.0 | Works | Static, activation (with Elementor) | 2026-11-06 |  |
| `enable-media-replace` | 94 | 4.2.3 | Works | Static, activation | 2026-11-06 |  |
| `essential-addons-for-elementor-lite` | 29 | 6.8.8 | Works | Static, activation (with Elementor) | 2026-11-06 |  |
| `ewww-image-optimizer` | 60 | 8.9.1 | Works | Static, activation | 2026-11-06 | The primary key of `ewwwio_images` is written in a variable (it becomes UNIQUE only on MariaDB 10.4) |
| `export-media-library` |  | 4.0.2 | Works | Static, activation | 2026-11-06 |  |
| `flamingo` | 79 | 2.6.4 | Works | Static, activation | 2026-11-06 |  |
| `fluent-smtp` | 89 | 2.4.5 | Works | Static, activation | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `fluentform` | 82 | 6.2.17 | Works | Static, activation | 2026-11-06 | Has code that builds `MATCH ... AGAINST`, but does not use it by default |
| `forminator` | 99 | 1.58.4 | Works | Static, activation, comparison | 2026-11-06 | The migration when upgrading compares an indexed `ENUM` column with `''` (`status = ''`). This was error 1105 in 0.2.36 and earlier (fixed in stage 107). The popularity rank is from 2026-11-06 |
| `gcs` |  | 0.1.6 | Works | Static, activation | 2026-11-06 |  |
| `google-analytics-for-wordpress` | 24 | 11.3.0 | Works | Static, activation | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers). Has a table with a `mediumint` AUTO_INCREMENT column (see "Tables that need care when merging branches" below) |
| `google-listings-and-ads` | 74 | 3.10.0 | Works | Static, activation (with WooCommerce) | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `google-site-kit` | 7 | 1.191.0 | Works | Static, activation | 2026-11-06 |  |
| `google-sitemap-generator` | 56 | 4.1.25 | Works | Static, activation | 2026-11-06 |  |
| `gtranslate` | 71 | 5.0.2 | Works | Static, activation | 2026-11-06 |  |
| `header-footer` |  | 3.3.6 | Works | Static, activation | 2026-11-06 |  |
| `header-footer-elementor` | 26 | 2.9.6 | Works | Static, activation (with Elementor) | 2026-11-06 |  |
| `hello` |  | — | Works | — | 2026-10-16 | Does not use the database (one file) |
| `hostinger` | 20 | 3.0.81 | Works | Static, activation | 2026-11-06 |  |
| `hostinger-reach` | 42 | 1.8.8 | Works | Static, activation | 2026-11-06 |  |
| `html-editor-syntax-highlighter` |  | 2.5.0 | Works | Static, activation | 2026-11-06 |  |
| `image-optimization` | 57 | 1.7.7 | Works | Static, activation | 2026-11-06 |  |
| `imagify` | 68 | 2.3.5 | Works | Static, activation | 2026-11-06 |  |
| `insert-headers-and-footers` | 19 | 2.4.1 | Works | Static, activation | 2026-11-06 |  |
| `instagram-feed` | 63 | 6.15.1 | Works | Static, activation | 2026-11-06 |  |
| `intuitive-custom-post-order` |  | 3.2.0 | Works | In use, static, activation, comparison | 2026-11-06 | When deactivated, it checks whether the column exists with ``DESCRIBE wp_terms `term_order` `` and then drops the column. In 0.2.36 and earlier, `DESCRIBE` with a column was a syntax error, and the `term_order` column was left behind (fixed in stage 107) |
| `jetpack` | 18 | 16.4 | Works | Static, activation | 2026-11-06 |  |
| `jwt-authentication-for-wp-rest-api` |  | 1.5.0 | Works | Static, activation | 2026-11-06 |  |
| `limit-login-attempts-reloaded` | 34 | 3.4.1 | Works | Static, activation | 2026-11-06 |  |
| `litespeed-cache` | 5 | 7.9.3 | Works | Static, activation | 2026-11-06 |  |
| `loco-translate` | 36 | 2.8.9 | Works | Static, activation | 2026-11-06 |  |
| `loginizer` | 46 | 2.1.3 | Works | Static, activation, comparison | 2026-11-06 | Counting failed logins and lockout work the same as on MySQL. After changing the log table in a branch and merging, counting continues after the merge. In 0.2.19 and earlier, the log table `loginizer_logs` (no primary key, only `UNIQUE KEY ip`) could not be created, and reads and writes for login limiting failed |
| `mailchimp-for-wp` | 35 | 4.15.0 | Works | Static, activation | 2026-11-06 |  |
| `maintenance` | 58 | 4.33 | Works | Static, activation | 2026-11-06 |  |
| `mainwp-child` | 86 | 6.4.1 | Works | Static, activation | 2026-11-06 |  |
| `mw-wp-form` |  | 5.1.8 | Works | In use, static, activation | 2026-11-06 |  |
| `no-category-base-wpml` |  | 1.5.8 | Works | Static, activation | 2026-11-06 | Works without WPML |
| `one-click-demo-import` | 61 | 3.4.1 | Works | Static, activation | 2026-11-06 |  |
| `optinmonster` | 52 | 2.17.1 | Works | Static, activation | 2026-11-06 |  |
| `otgs-installer-plugin` |  | 3.1.3 (version obtained) | Works | Static, activation | 2026-11-06 | For updating WPML |
| `page-links-to` |  | 3.4.1 | Works | Static, activation | 2026-11-06 |  |
| `polylang` | 73 | 3.9.1 | Works | Static, activation | 2026-11-06 |  |
| `popup-maker` | 91 | 1.25.0 | Works | Static, activation | 2026-11-06 |  |
| `post-lockdown` |  | 4.1.1 | Works | In use, static, activation | 2026-11-06 |  |
| `post-smtp` |  | 4.0.2 | Works | Static, activation | 2026-11-06 | `DESCRIBE IF;` fails on activation (dbDelta reads the `IF` of `CREATE TABLE IF NOT EXISTS` as the table name; same error on MySQL) |
| `post-types-order` | 98 | 2.5.9 | Works | Static, activation | 2026-11-06 |  |
| `premium-addons-for-elementor` | 96 | 4.11.117 | Works | Static, activation (with Elementor) | 2026-11-06 |  |
| `public-post-preview` |  | 3.1.2 | Works | Static, activation | 2026-11-06 |  |
| `pz-linkcard` |  | 2.6.4 | Works | Static, activation | 2026-11-06 |  |
| `really-simple-ssl` | 17 | 9.8.4 | Works | Static, activation | 2026-11-06 |  |
| `redirection` | 22 | 5.10.1 | Works | Static, activation | 2026-11-06 |  |
| `redux-framework` | 70 | 4.5.17 | Works | Static, activation | 2026-11-06 |  |
| `regenerate-thumbnails` | 64 | 3.1.6 | Works | Static, activation | 2026-11-06 |  |
| `rewrite-rules-inspector` |  | 1.6.0 | Works | Static, activation | 2026-11-06 |  |
| `s3-uploads` |  | — | Works | In use | 2026-10-16 | Distributed on GitHub |
| `safe-svg` | 53 | 2.5.2 | Works | Static, activation | 2026-11-06 |  |
| `scheduled-post-trigger` |  | 3.3 | Works | Static, activation | 2026-11-06 |  |
| `sendgrid-email-delivery-simplified` |  | 1.11.8 (version obtained) | Works | Static, activation | 2026-11-06 | Closed on wordpress.org on 2021-07-13 (at the author's request) |
| `seo-by-rank-math` | 13 | 1.0.281 | Works | Static, activation | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `sg-ai-studio` | 49 | 1.3.2 | Works | Static, activation | 2026-11-06 | `DESCRIBE IF;` fails on activation (same on MySQL) |
| `sg-cachepress` | 43 | 7.8.6 | Works | Static, activation | 2026-11-06 |  |
| `sg-security` | 51 | 1.7.0 | Works | Static, activation | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers). The `DESCRIBE IF;` failure is the same on MySQL |
| `shortcodes-ultimate` |  | 7.9.2 | Works | In use, static, activation | 2026-11-06 |  |
| `simple-ga-ranking` |  | 3.0.0 (version obtained) | Works | Static, activation | 2026-11-06 | Closed on wordpress.org on 2024-11-20 (at the author's request) |
| `sitepress-multilingual-cms` |  | 4.6.11 (version obtained) | Works | Static, activation | 2026-11-06 | WPML. In 0.2.18 and earlier, `icl_locale_map` (a primary key of two `VARCHAR` columns) could not be created, and activation stopped |
| `smart-slider-3` | 78 | 3.5.1.40 | Works | Static, activation | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `svg-support` | 48 | 2.6.3 | Works | Static, activation | 2026-11-06 |  |
| `tablepress` | 97 | 3.4.1 | Works | Static, activation | 2026-11-06 |  |
| `the-events-calendar` | 95 | 6.18.3.1 | Works | Static, activation | 2026-11-06 |  |
| `tinymce-advanced` | 30 | 5.10.1 | Works | In use, static, activation | 2026-11-06 |  |
| `tinymce-clear-buttons` |  | 1.3.2 | Works | Static, activation | 2026-11-06 |  |
| `toggle-wpautop` |  | 1.3.0 | Works | Static, activation | 2026-11-06 |  |
| `two-factor` |  | 0.17.0 | Works | Static, activation | 2026-11-06 |  |
| `ultimate-addons-for-gutenberg` | 62 | 2.20.6 | Works | Static, activation | 2026-11-06 |  |
| `updraftplus` | 14 | 1.26.14 | Works | Static, activation, comparison | 2026-11-06 | Database backup (exported by PHP) and restore work the same as on MySQL (restore was also checked on 2026-10-24; in 0.2.19 and earlier, the table without a primary key that it creates to check privileges before a restore could not be created, so the restore empties the tables and reloads them). Calls `wpdb::check_connection()` (`DO 1`) during a backup (a syntax error in HeimDB 0.2.17 and earlier, so it reconnected). mysqldump cannot be used with HeimDB without `-tls-cert`, because the newer MariaDB client requires TLS, so it falls back to PHP |
| `user-role-editor` | 90 | 4.67 | Works | Static, activation | 2026-11-06 |  |
| `w3-total-cache` | 72 | 2.11.0 | Works | Static, activation | 2026-11-06 | The table it creates when the CDN is enabled has a `CHARACTER SET ascii` column (accepted since 0.2.10) |
| `webp-converter-for-media` |  | 6.7.2 | Works | Static, activation | 2026-11-06 |  |
| `woocommerce` | 6 | 11.2.2 | Partly works | Static, activation, comparison | 2026-11-06 | Full-text search of orders (only when enabled in the settings) does not work. Orders (HPOS), products, attribute filtering (`wc_product_attributes_lookup`) and the admin screens work the same as on MySQL (in 0.2.18 and earlier, this table could not be created because its primary key contains a `varchar`). Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `woocommerce-gateway-stripe` | 84 | 11.1.1 | Works | Static, activation (with WooCommerce) | 2026-11-06 |  |
| `woocommerce-payments` | 76 | 11.2.1 | Works | Static, activation (with WooCommerce) | 2026-11-06 |  |
| `woocommerce-paypal-payments` | 81 | 4.2.0 | Works | Static, activation (with WooCommerce) | 2026-11-06 |  |
| `wordfence` | 11 | 9.0.2 | Works | Static, activation, comparison | 2026-11-06 | Activation and scans work the same as on MySQL. In 0.2.18 and earlier, tables whose primary key contains `BINARY(16)`, `VARBINARY` or `ENUM` (`wfls_role_counts`, `wfblockediplog`, `wftrafficrates` and others) could not be created, and activation stopped. Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `wordpress-importer` | 23 | 0.9.7 | Works | Static, activation | 2026-11-06 |  |
| `wordpress-popular-posts` |  | 7.4.4 | Works | In use, static, activation | 2026-11-06 |  |
| `wordpress-seo` | 2 | 28.8 | Works | In use, static, activation, comparison | 2026-11-06 | It builds its tables in code, so the static check cannot see them. Its index tables are best excluded from merges (see "Tables to exclude from merges" below) |
| `worker` | 55 | 4.9.39 | Works | Static, activation | 2026-11-06 |  |
| `wp-2fa` |  | 4.3.0 | Works | In use, static, activation | 2026-11-06 |  |
| `wp-associate-post-r2` |  | 5.0.2 | Works | Static, activation | 2026-11-06 |  |
| `wp-cerber` |  | 9.10 (official site) | Works | Static, activation, comparison | 2026-11-06 | Activation, login, logging and traffic logging work the same as on MySQL. After changing the ACL and the logs in a branch and merging, logging continues after the merge. The `OPTIMIZE TABLE` in the maintenance cron was a syntax error in 0.2.21 and earlier (only the table maintenance did not run; behavior did not change; fixed in stage 74). In 0.2.19 and earlier, tables without a primary key (`cerber_acl`, `cerber_log`, `cerber_traffic` and others) could not be created, and activation stopped. It was closed on wordpress.org on 2022-09-22 (a security issue) and is distributed on the official site. The maintenance cron also runs `OPTIMIZE TABLE` on a table that does not exist on a single site (`cerber_ms_lists`), and HeimDB returns error 1146 (MySQL returns the error in a result row; only the table maintenance does not run). |
| `wp-cors` |  | 0.2.2 | Works | Static, activation | 2026-11-06 |  |
| `wp-emmet` |  | 0.3.4 | Works | Static, activation | 2026-11-06 |  |
| `wp-fastest-cache` | 47 | 1.5.5 | Works | Static, activation | 2026-11-06 |  |
| `wp-file-manager` | 45 | 8.0.7 | Works | Static, activation | 2026-11-06 |  |
| `wp-graphql` |  | 2.26.2 | Works | Static, activation | 2026-11-06 |  |
| `wp-graphql-acf-master` |  | 0.5.3 (version obtained) | Works | Static, activation | 2026-11-06 | Distributed on GitHub |
| `wp-graphql-meta-query-develop` |  | 0.1.1 (version obtained) | Works | Static, activation | 2026-11-06 | Distributed on GitHub |
| `wp-graphql-offset-pagination-master` |  | 0.2.0 (version obtained) | Works | Static, activation | 2026-11-06 | Distributed on GitHub |
| `wp-mail-smtp` | 12 | 4.10.3 | Works | Static, activation | 2026-11-06 | Creates tables with `ENGINE='InnoDB'` (accepted since 0.2.10) |
| `wp-missed-schedule-posts` |  | 1.1 | Works | Static, activation | 2026-11-06 |  |
| `wp-multibyte-patch` | 66 | 2.9.4 | Works | In use, static, activation | 2026-11-06 |  |
| `wp-optimize` | 54 | 4.7.0 | Works | Static, activation | 2026-11-06 | Table optimization (`OPTIMIZE TABLE`) and the table check (`CHECK TABLE`) failed with a syntax error in 0.2.21 and earlier (fixed in stage 74). The repair screen does not check InnoDB tables, so on HeimDB it shows "no corrupted tables" |
| `wp-pagenavi` |  | 3.0.1 | Works | In use, static, activation | 2026-11-06 |  |
| `wp-reviews-plugin-for-google` | 67 | 14.3 | Works | Static, activation, comparison | 2026-11-06 | In 0.2.18 and earlier, the primary key of `trustindex_google_views` (a single `date` column) could not be created, and activation stopped |
| `wp-seo-multilingual` |  | 2.1.0 (version obtained) | Works | Static, activation | 2026-11-06 | Requires WPML (checked with WPML active) |
| `wp-smushit` | 65 | 4.3.6 | Works | Static, activation | 2026-11-06 | Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers). Has a table with a `mediumint` AUTO_INCREMENT column |
| `wp-stateless` |  | 4.4.3 | Works | Static, activation | 2026-11-06 | Reads `wp_sm_sync`, which does not exist yet, on activation (same error on MySQL) |
| `wp-super-cache` | 37 | 3.1.5 | Works | Static, activation | 2026-11-06 |  |
| `wp-sweep` |  | 2.0.1 | Works | Static, activation | 2026-11-06 | Table optimization (`OPTIMIZE TABLE` listing all tables) failed with a syntax error in 0.2.21 and earlier (fixed in stage 74) |
| `wp2html` |  | 1.0.3 | Works | Static, activation | 2026-11-06 | No HeimDB difference. Activation does not finish (same on MySQL; apart from the HeimDB verdict, check whether it works with current WordPress) |
| `wpforms-lite` | 9 | 2.0.4 | Works | Static, activation, comparison | 2026-11-06 | In 0.2.18 and earlier, the analytics table `wpforms_analytics_forms` (primary key `(form_id BIGINT, period_date DATE)`) could not be created. Checks whether an index exists with `SHOW INDEX ... WHERE` (HeimDB 0.2.17 and earlier returned wrong answers) |
| `wpml-media-translation` |  | 2.7.4 (version obtained) | Works | Static, activation | 2026-11-06 | Requires WPML (checked with WPML active) |
| `wpml-string-translation` |  | 3.2.12 (version obtained) | Works | Static, activation | 2026-11-06 | Requires WPML (checked with WPML active) |
| `wps-hide-login` | 27 | 1.9.19 | Works | Static, activation | 2026-11-06 |  |
| `wpvivid-backuprestore` | 69 | 0.9.139 | Works | Static, activation | 2026-11-06 | Creating a backup cannot be started from WP-CLI and was not checked (same on MySQL) |

## Criteria (what HeimDB rejects)

| Rejected | HeimDB error | Example |
|---|---|---|
| Tables with an `AUTO_INCREMENT` column and no primary key (tables without a primary key can be created since stage 72) | `HeimDB: table X has an AUTO_INCREMENT column but no primary key; ...` | |
| Primary keys containing a column that is not an integer, `CHAR`, `VARCHAR`, `BINARY`, `VARBINARY`, `DATE`, `DATETIME`, `TIMESTAMP` or `ENUM` (`TEXT`, `SET`, `DECIMAL` and so on; in 0.2.18 and earlier, only integer columns or a single `CHAR` / `VARCHAR` column) | `HeimDB: table X: primary key column "c" has type T; primary key columns must be ...` | |
| Primary keys with a prefix length (`PRIMARY KEY (k(16))`) | `HeimDB: a prefix length on primary key column k is not supported` | |
| Generated columns (`GENERATED ALWAYS AS`, `AS (...) STORED`) | `HeimDB: generated column "c" is not supported` | |
| Tables and columns with a character set other than utf8mb4 (latin1 and so on; utf8, utf8mb3 and ascii are accepted as the utf8mb4 collation of the same name; ascii since 0.2.10) | `HeimDB: table X uses character set latin1; only utf8mb4 is supported ...` | |
| Full-text search (`MATCH ... AGAINST`) | Error | Search plugins |
| Stored procedures, functions, triggers, views and events | Error | |
| SQL that reads or writes files on the server (`LOAD DATA INFILE`, `INTO OUTFILE`, `LOAD_FILE()`) | Error | |
| Syntax that the HeimDB parser does not accept | `syntax error ...` | None found (a quoted engine name, `ENGINE='InnoDB'`, is accepted since 0.2.10) |

See "Differences accepted because HeimDB is WordPress-only" in [`differences.md`](differences.md#differences-accepted-because-heimdb-is-wordpress-only) for the details.

## Tables to exclude from merges

Tables that you do not want to merge from a branch (staging) can be excluded with `heimdb_merge_exclude` (see "Tables excluded from merges" in `docs/procedures.md`; since 0.2.12). An excluded table keeps its production definition and data, and the merge result has an `excluded` row. The plugin reads that row and rebuilds the table in production after the merge.

| Plugin | Tables to exclude | Reason | What to do after the merge |
|---|---|---|---|
| Yoast SEO (`wordpress-seo`) | `<prefix>yoast_%` (`yoast_indexable`, `yoast_indexable_hierarchy`, `yoast_primary_term`, `yoast_seo_links`, `yoast_migrations` and others) | The index grows on both sides each time a post is saved or a page is shown, and other tables point to its IDs (there is no case where you want to merge the staging one). If the branch spans a Yoast update, the table definitions also change on one side only | The steps below |

Steps after a merge with Yoast SEO excluded:

1. If Yoast was updated in the branch (if the Yoast version differs from production), delete `yoast_migrations_free` from `wp_options` in production. Yoast keeps in this option how far its migrations have run. The option is a row in `wp_options`, so the merge brings it into production, and as is, Yoast would decide the migrations are "done" while the production tables still have the old definitions. Once it is deleted, Yoast reads the `yoast_migrations` table (the production record, since it was excluded) and runs the missing migrations on the production tables.
2. Rebuild the index (`wp yoast index` and so on). It takes time, so do not wait for it inside the merge hook; hand it to a scheduled event or a job.
3. When the merge is undone (`undo`), rebuild the index too (excluded tables are not reverted, so the index stays as it was after the merge).

## Tables that need care when merging branches

For `bigint` and `int` columns (signed or unsigned), AUTO_INCREMENT values for rows added in a branch are taken from a number range that does not overlap with production (see `heimdb_branch_create` in `docs/procedures.md`; `int` since 0.2.13). So adding rows on both sides does not cause a merge conflict, and references between tables (such as Redirection's `redirection_items.group_id`) still point to the right rows. In the cases below, production and the branch assign the same numbers, so adding rows to the same table on both sides stops the merge with an `added on both sides` conflict. This conflict cannot be resolved in favor of the branch (resolving with `'parent'` discards the rows added in the branch), and it remains even if a wildcard `'branch'` is recorded (the diff's `detail` and the merge error say `only 'parent' can resolve this`).

- Tables with a `tinyint`, `smallint` or `mediumint` AUTO_INCREMENT column (too narrow to split into number ranges). `scripts/plugin-check/check.py` reports them as a "caution".
- Rows of type `int` added up to then in a branch created with HeimDB 0.2.12 or earlier (a recreated branch, and rows added from now on, take numbers from the range).
- Two branches whose branch IDs differ by 100 (they reuse the same `int` number range slot) exist at the same time, and both add rows to the same table. Numbers also overlap when a branch is created from a point before the merge of the previous branch in the same slot, and when a branch 100 later reassigns the number of a row deleted in production after a merge (references that pointed to the deleted row now point to the new row).

If the table can be rebuilt, exclude it from merges.

If tables are left over from a plugin that makes a copy in the same database with another prefix (such as WP Staging's `wpstg0_`), two WordPress table prefixes are found, and the merge stops with `ambiguous_prefix` (51006). Delete copy tables you do not use (subsites of a multisite, such as `wp_2_`, are not counted).
