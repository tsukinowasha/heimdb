# HeimDB で使えるプラグイン

WordPress のプラグインが HeimDB の上で動くかの一覧。HeimDB は WordPress 専用として、MySQL の一部の機能を明示的なエラーで拒否する（下の「判定の基準」）。プラグインがその機能を使うと、表が作れない、文が失敗するなどで、プラグインの一部か全部が動かない。

157 本を調べた（2026-10-16〜25 に HeimDB 0.2.9 から段階 74 で調べ、2026-11-06 に段階 107 で、DB を使わない `hello` と GitHub で配られる `s3-uploads` のほかを調べ直した）。wordpress.org の人気の上位 100 と、Tsukinowa Inc. が運用している WordPress のサイトで使われているもの。どう選び、どう調べたかと、調べる途中で見つかった HeimDB の差は [`plugins-research.md`](plugins-research.md)。

## 判定

| 判定 | 意味 |
|---|---|
| 使える | HeimDB が拒否する SQL を使わない（有効にして、トップページ、管理画面、予定の cron を流しても、MySQL と違うエラーが出ない） |
| 条件付き | ふだんは動くが、特定の機能を使うと HeimDB が拒否する（どの機能かを書く） |
| 使えない | 有効にできない、または主な機能が HeimDB で拒否される |

表の列:

- **人気**: wordpress.org の人気の順位（2026-10-20 の上位 100。`https://wordpress.org/plugins/browse/popular/`）。
- **バージョン**: 調べたバージョン。ふつうは調べた日の wordpress.org の最新バージョン。「入手したバージョン」は、有料版や wordpress.org にないもので、手に入ったいちばん新しいバージョン（最新バージョンではないことがある）。「公式サイト」は作者のサイトで配られているバージョン。
- **調べ方**: 静的（ソースの SQL の検査）、有効化（HeimDB の上の WordPress で有効にする）、比較（主な機能を HeimDB と本物の MySQL 8.0 で動かし、エラーになった文を比べる）、運用（HeimDB の上で動く WordPress のサイトで実際に使って確かめた）。詳しくは [`plugins-research.md`](plugins-research.md) の「調べ方」。
- 「MySQL でも同じ」としたものは、本物の MySQL 8.0 でも同じエラーになる（HeimDB の差ではない）。

## 使えないもの、一部が使えないもの

- **WooCommerce**（`woocommerce`、条件付き）: 注文の全文検索（設定で有効にしたときだけ）は、全文検索の索引を拒否するので使えない。注文、商品、属性の絞り込み、管理画面は MySQL と同じに動く。

0.2.21 以前の HeimDB は、表の手入れの文（`OPTIMIZE TABLE`、`CHECK TABLE`、`REPAIR TABLE`）が構文エラーになり、WP-Optimize の最適化、Advanced Database Cleaner の最適化と壊れた表の確認、WP-Sweep の表の最適化、WordPress の `wp-admin/maint/repair.php`、wp-cli の `wp db check` / `optimize` / `repair` が失敗した（段階 74 で直した。どれも何もせず OK を返す）。

0.2.19 以前の HeimDB は、主キーのない表を作れず、WP Cerber と Loginizer が使えなかった（段階 72 で直した。[`plugins-research.md`](plugins-research.md) の「判定が変わったもの」）。

0.2.18 以前の HeimDB は、文字列、バイト列、日付、`ENUM` を含む主キーの表を作れず、WPML とそのアドオン、Wordfence、WP Reviews for Google が使えず、WooCommerce（属性の絞り込み）と WPForms Lite（分析）の一部が使えなかった（0.2.19 で直した。[`plugins-research.md`](plugins-research.md) の「判定が変わったもの」）。

0.2.17 以前の HeimDB は、`SHOW INDEX ... WHERE` で索引の有無を確かめるプラグイン（下の表のメモにあるもの）に誤った答えを返していた。そのころ有効にしたプラグインは、足すべき索引がないことがある（`SHOW INDEX FROM 表` で確かめる。[`plugins-research.md`](plugins-research.md) の「見つかった HeimDB の差」）。

## 一覧

| プラグイン | 人気 | バージョン | 判定 | 調べ方 | 調べた日 | メモ |
|---|---|---|---|---|---|---|
| `acfml` |  | 2.1.3（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | WPML が要る（WPML を有効にして確かめた） |
| `ad-inserter` |  | 2.8.20 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `addquicktag` |  | 2.6.2 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `adjust-admin-categories` |  | 2.2.6 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `admin-menu-editor` |  | 1.15.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `advanced-custom-fields` | 28 | 6.8.11 | 使える | 静的、有効化 | 2026-11-06 |  |
| `advanced-database-cleaner` |  | 4.2.0 | 使える | 静的、有効化 | 2026-11-06 | 表の最適化（`OPTIMIZE TABLE` の後に `ANALYZE TABLE`）、壊れた表の確認（`CHECK TABLE` を 20 表ずつ）、修復は、0.2.21 以前は構文エラーで失敗した（段階 74 で直した） |
| `advanced-custom-fields-pro` |  | 6.3.11（入手したバージョン） | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `akismet` | 8 | 5.7.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `all-in-one-seo-pack` | 21 | 5.0.5 | 使える | 静的、有効化 | 2026-11-06 | 表 13 個（Action Scheduler を含む）はどれも整数の主キー |
| `all-in-one-wp-migration` | 10 | 7.113 | 使える | 静的、有効化 | 2026-11-06 |  |
| `all-in-one-wp-security-and-firewall` | 59 | 5.5.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `antispam-bee` | 85 | 2.11.13 | 使える | 静的、有効化 | 2026-11-06 |  |
| `astra-sites` | 32 | 4.7.10 | 使える | 静的、有効化 | 2026-11-06 |  |
| `autoptimize` | 77 | 3.1.17 | 使える | 静的、有効化 | 2026-11-06 |  |
| `backuply` | 99 | 1.6.0 | 使える | 静的、有効化 | 2026-11-06 | バックアップの作成は確かめていない（サイト自身への HTTP で動く） |
| `better-search-replace` | 40 | 1.4.11 | 使える | 静的、有効化、比較 | 2026-11-06 | 記事の本文とシリアライズしたオプションの置き換えが通った |
| `better-wp-security` | 87 | 10.0.6 | 使える | 静的、有効化 | 2026-11-06 | Pro バージョンだけが作る `itsec_webauthn_credentials` に latin1 の列がある（無料版は作らない） |
| `bogo` |  | 3.9.3 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `breadcrumb-navxt` | 80 | 7.5.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `bulk-move` |  | 1.3.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `classic-editor` | 4 | 1.7.0 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `classic-widgets` | 25 | 0.3 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `click-to-chat-for-whatsapp` | 93 | 4.46 | 使える | 静的、有効化 | 2026-11-06 |  |
| `code-snippets` | 33 | 4.1.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `coming-soon` | 92 | 6.20.11 | 使える | 静的、有効化 | 2026-11-06 |  |
| `complianz-gdpr` | 44 | 7.5.7 | 使える | 静的、有効化 | 2026-11-06 |  |
| `contact-form-7` | 3 | 6.2.4.1 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `cookie-law-info` | 39 | 3.5.8 | 使える | 静的、有効化 | 2026-11-06 |  |
| `cookie-notice` | 75 | 3.1.18 | 使える | 静的、有効化 | 2026-11-06 |  |
| `creame-whatsapp-me` | 88 | 6.4.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `custom-css-js` | 100 | 3.55 | 使える | 静的、有効化 | 2026-11-06 |  |
| `custom-post-type-permalinks` |  | 3.5.6 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `custom-post-type-ui` | 50 | 1.19.3 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `disable-comments` | 38 | 2.9.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `disable-emojis` |  | 1.9.3 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `duplicate-page` | 16 | 4.5.9 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `duplicate-post` | 15 | 4.7 | 使える | 静的、有効化 | 2026-11-06 |  |
| `duplicator` | 41 | 5.1.1 | 使える | 静的、有効化 | 2026-11-06 | バックアップの作成は確かめていない（サイト自身への HTTP で動く）。途中で `wpdb::check_connection()`（`DO 1`）を呼ぶ。`SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `duracelltomi-google-tag-manager` | 83 | 2.1.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `easy-svg` |  | 4.2.1 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `elementor` | 1 | 4.3.6 | 使える | 静的、有効化 | 2026-11-06 |  |
| `elementskit-lite` | 31 | 4.1.0 | 使える | 静的、有効化（Elementor と） | 2026-11-06 |  |
| `enable-media-replace` | 94 | 4.2.3 | 使える | 静的、有効化 | 2026-11-06 |  |
| `essential-addons-for-elementor-lite` | 29 | 6.8.8 | 使える | 静的、有効化（Elementor と） | 2026-11-06 |  |
| `ewww-image-optimizer` | 60 | 8.9.1 | 使える | 静的、有効化 | 2026-11-06 | `ewwwio_images` の主キーは変数で書かれている（MariaDB 10.4 のときだけ UNIQUE にする） |
| `export-media-library` |  | 4.0.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `flamingo` | 79 | 2.6.4 | 使える | 静的、有効化 | 2026-11-06 |  |
| `fluent-smtp` | 89 | 2.4.5 | 使える | 静的、有効化 | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `fluentform` | 82 | 6.2.17 | 使える | 静的、有効化 | 2026-11-06 | `MATCH ... AGAINST` を組み立てる部品を持つが、既定では使わない |
| `forminator` | 99 | 1.58.4 | 使える | 静的、有効化、比較 | 2026-11-06 | バージョンを上げるときの移し替えで、インデックスのある `ENUM` の列を `''` と比べる（`status = ''`）。0.2.36 以前はエラー 1105 になった（段階 107 で直した）。人気の順位は 2026-11-06 のもの |
| `gcs` |  | 0.1.6 | 使える | 静的、有効化 | 2026-11-06 |  |
| `google-analytics-for-wordpress` | 24 | 11.3.0 | 使える | 静的、有効化 | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した）。`mediumint` の AUTO_INCREMENT の表がある（下の「ブランチのマージで注意の要る表」） |
| `google-listings-and-ads` | 74 | 3.10.0 | 使える | 静的、有効化（WooCommerce と） | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `google-site-kit` | 7 | 1.191.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `google-sitemap-generator` | 56 | 4.1.25 | 使える | 静的、有効化 | 2026-11-06 |  |
| `gtranslate` | 71 | 5.0.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `header-footer` |  | 3.3.6 | 使える | 静的、有効化 | 2026-11-06 |  |
| `header-footer-elementor` | 26 | 2.9.6 | 使える | 静的、有効化（Elementor と） | 2026-11-06 |  |
| `hello` |  | — | 使える | — | 2026-10-16 | DB を使わない（1 ファイル） |
| `hostinger` | 20 | 3.0.81 | 使える | 静的、有効化 | 2026-11-06 |  |
| `hostinger-reach` | 42 | 1.8.8 | 使える | 静的、有効化 | 2026-11-06 |  |
| `html-editor-syntax-highlighter` |  | 2.5.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `image-optimization` | 57 | 1.7.7 | 使える | 静的、有効化 | 2026-11-06 |  |
| `imagify` | 68 | 2.3.5 | 使える | 静的、有効化 | 2026-11-06 |  |
| `insert-headers-and-footers` | 19 | 2.4.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `instagram-feed` | 63 | 6.15.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `intuitive-custom-post-order` |  | 3.2.0 | 使える | 運用、静的、有効化、比較 | 2026-11-06 | 無効にするときに ``DESCRIBE wp_terms `term_order` `` で列があるかを確かめてから列を消す。0.2.36 以前は列を付けた `DESCRIBE` が構文エラーになり、`term_order` の列が残った（段階 107 で直した） |
| `jetpack` | 18 | 16.4 | 使える | 静的、有効化 | 2026-11-06 |  |
| `jwt-authentication-for-wp-rest-api` |  | 1.5.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `limit-login-attempts-reloaded` | 34 | 3.4.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `litespeed-cache` | 5 | 7.9.3 | 使える | 静的、有効化 | 2026-11-06 |  |
| `loco-translate` | 36 | 2.8.9 | 使える | 静的、有効化 | 2026-11-06 |  |
| `loginizer` | 46 | 2.1.3 | 使える | 静的、有効化、比較 | 2026-11-06 | ログイン失敗の数え上げとロックアウトは MySQL と同じに動いた。ログの表をブランチで変えてマージしても、マージの後に数え上げが続く。0.2.19 以前は、ログの表 `loginizer_logs`（主キーがなく `UNIQUE KEY ip` だけ）が作れず、ログイン制限の読み書きが失敗した |
| `mailchimp-for-wp` | 35 | 4.15.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `maintenance` | 58 | 4.33 | 使える | 静的、有効化 | 2026-11-06 |  |
| `mainwp-child` | 86 | 6.4.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `mw-wp-form` |  | 5.1.8 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `no-category-base-wpml` |  | 1.5.8 | 使える | 静的、有効化 | 2026-11-06 | WPML なしでも動く |
| `one-click-demo-import` | 61 | 3.4.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `optinmonster` | 52 | 2.17.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `otgs-installer-plugin` |  | 3.1.3（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | WPML の更新用 |
| `page-links-to` |  | 3.4.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `polylang` | 73 | 3.9.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `popup-maker` | 91 | 1.25.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `post-lockdown` |  | 4.1.1 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `post-smtp` |  | 4.0.2 | 使える | 静的、有効化 | 2026-11-06 | 有効化で `DESCRIBE IF;` が失敗する（dbDelta が `CREATE TABLE IF NOT EXISTS` の `IF` を表の名前と読む。MySQL でも同じエラー） |
| `post-types-order` | 98 | 2.5.9 | 使える | 静的、有効化 | 2026-11-06 |  |
| `premium-addons-for-elementor` | 96 | 4.11.117 | 使える | 静的、有効化（Elementor と） | 2026-11-06 |  |
| `public-post-preview` |  | 3.1.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `pz-linkcard` |  | 2.6.4 | 使える | 静的、有効化 | 2026-11-06 |  |
| `really-simple-ssl` | 17 | 9.8.4 | 使える | 静的、有効化 | 2026-11-06 |  |
| `redirection` | 22 | 5.10.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `redux-framework` | 70 | 4.5.17 | 使える | 静的、有効化 | 2026-11-06 |  |
| `regenerate-thumbnails` | 64 | 3.1.6 | 使える | 静的、有効化 | 2026-11-06 |  |
| `rewrite-rules-inspector` |  | 1.6.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `s3-uploads` |  | — | 使える | 運用 | 2026-10-16 | GitHub で配られる |
| `safe-svg` | 53 | 2.5.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `scheduled-post-trigger` |  | 3.3 | 使える | 静的、有効化 | 2026-11-06 |  |
| `sendgrid-email-delivery-simplified` |  | 1.11.8（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | wordpress.org で 2021-07-13 に公開が止まっている（作者の申し出） |
| `seo-by-rank-math` | 13 | 1.0.281 | 使える | 静的、有効化 | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `sg-ai-studio` | 49 | 1.3.2 | 使える | 静的、有効化 | 2026-11-06 | 有効化で `DESCRIBE IF;` が失敗する（MySQL でも同じ） |
| `sg-cachepress` | 43 | 7.8.6 | 使える | 静的、有効化 | 2026-11-06 |  |
| `sg-security` | 51 | 1.7.0 | 使える | 静的、有効化 | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した）。`DESCRIBE IF;` の失敗は MySQL でも同じ |
| `shortcodes-ultimate` |  | 7.9.2 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `simple-ga-ranking` |  | 3.0.0（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | wordpress.org で 2024-11-20 に公開が止まっている（作者の申し出） |
| `sitepress-multilingual-cms` |  | 4.6.11（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | WPML。0.2.18 以前は `icl_locale_map`（2 列の `VARCHAR` の主キー）が作れず、有効化が止まった |
| `smart-slider-3` | 78 | 3.5.1.40 | 使える | 静的、有効化 | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `svg-support` | 48 | 2.6.3 | 使える | 静的、有効化 | 2026-11-06 |  |
| `tablepress` | 97 | 3.4.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `the-events-calendar` | 95 | 6.18.3.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `tinymce-advanced` | 30 | 5.10.1 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `tinymce-clear-buttons` |  | 1.3.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `toggle-wpautop` |  | 1.3.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `two-factor` |  | 0.17.0 | 使える | 静的、有効化 | 2026-11-06 |  |
| `ultimate-addons-for-gutenberg` | 62 | 2.20.6 | 使える | 静的、有効化 | 2026-11-06 |  |
| `updraftplus` | 14 | 1.26.14 | 使える | 静的、有効化、比較 | 2026-11-06 | データベースのバックアップ（PHP での書き出し）と復元は MySQL と同じに動いた（2026-10-24 に復元も確かめた。0.2.19 以前は、復元の前に権限を確かめる主キーのない表が作れず、表を空にして入れ直す復元になる）。バックアップの途中で `wpdb::check_connection()`（`DO 1`）を呼ぶ（0.2.17 以前の HeimDB では構文エラーになり、つなぎ直していた）。mysqldump は、MariaDB の新しいクライアントが TLS を求めるので、`-tls-cert` なしの HeimDB では使えず PHP に切り替わる |
| `user-role-editor` | 90 | 4.67 | 使える | 静的、有効化 | 2026-11-06 |  |
| `w3-total-cache` | 72 | 2.11.0 | 使える | 静的、有効化 | 2026-11-06 | CDN を有効にしたときに作る表に `CHARACTER SET ascii` の列がある（0.2.10 から受け付ける） |
| `webp-converter-for-media` |  | 6.7.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `woocommerce` | 6 | 11.2.2 | 条件付き | 静的、有効化、比較 | 2026-11-06 | 注文の全文検索（設定で有効にしたときだけ）は使えない。注文（HPOS）、商品、属性の絞り込み（`wc_product_attributes_lookup`）、管理画面は MySQL と同じに動いた（0.2.18 以前は、この表の主キーに `varchar` を含むので作れなかった）。`SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `woocommerce-gateway-stripe` | 84 | 11.1.1 | 使える | 静的、有効化（WooCommerce と） | 2026-11-06 |  |
| `woocommerce-payments` | 76 | 11.2.1 | 使える | 静的、有効化（WooCommerce と） | 2026-11-06 |  |
| `woocommerce-paypal-payments` | 81 | 4.2.0 | 使える | 静的、有効化（WooCommerce と） | 2026-11-06 |  |
| `wordfence` | 11 | 9.0.2 | 使える | 静的、有効化、比較 | 2026-11-06 | 有効化とスキャンは MySQL と同じに動いた。0.2.18 以前は、`BINARY(16)`、`VARBINARY`、`ENUM` を含む主キーの表（`wfls_role_counts`、`wfblockediplog`、`wftrafficrates` など）が作れず、有効化が止まった。`SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `wordpress-importer` | 23 | 0.9.7 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wordpress-popular-posts` |  | 7.4.4 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `wordpress-seo` | 2 | 28.8 | 使える | 運用、静的、有効化、比較 | 2026-11-06 | 表をコードで組み立てるので静的な検査には見えない。索引の表はマージから外すとよい（下の「マージから外す表」） |
| `worker` | 55 | 4.9.39 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-2fa` |  | 4.3.0 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `wp-associate-post-r2` |  | 5.0.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-cerber` |  | 9.10（公式サイト） | 使える | 静的、有効化、比較 | 2026-11-06 | 有効化、ログイン、ログと通信の記録は MySQL と同じに動いた。ACL とログをブランチで変えてマージしても、マージの後に記録が続く。手入れの cron の `OPTIMIZE TABLE` は、0.2.21 以前は構文エラーになった（表の手入れが行われないだけで、動きは変わらなかった。段階 74 で直した）。0.2.19 以前は、主キーのない表（`cerber_acl`、`cerber_log`、`cerber_traffic` など）が作れず、有効化が止まった。wordpress.org では 2022-09-22 に公開が止まり（セキュリティの問題）、公式サイトで配られている 手入れの cron は、単一サイトにはない表（`cerber_ms_lists`）にも `OPTIMIZE TABLE` を流し、HeimDB は 1146 のエラーにする（MySQL は結果の行でエラーを返す。表の手入れが行われないだけ）。 |
| `wp-cors` |  | 0.2.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-emmet` |  | 0.3.4 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-fastest-cache` | 47 | 1.5.5 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-file-manager` | 45 | 8.0.7 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-graphql` |  | 2.26.2 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-graphql-acf-master` |  | 0.5.3（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | GitHub で配られる |
| `wp-graphql-meta-query-develop` |  | 0.1.1（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | GitHub で配られる |
| `wp-graphql-offset-pagination-master` |  | 0.2.0（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | GitHub で配られる |
| `wp-mail-smtp` | 12 | 4.10.3 | 使える | 静的、有効化 | 2026-11-06 | `ENGINE='InnoDB'` で表を作る（0.2.10 から受け付ける） |
| `wp-missed-schedule-posts` |  | 1.1 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-multibyte-patch` | 66 | 2.9.4 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `wp-optimize` | 54 | 4.7.0 | 使える | 静的、有効化 | 2026-11-06 | 表の最適化（`OPTIMIZE TABLE`）と表の確認（`CHECK TABLE`）は、0.2.21 以前は構文エラーで失敗した（段階 74 で直した）。修復の画面は InnoDB の表を確かめないので、HeimDB では「壊れた表なし」になる |
| `wp-pagenavi` |  | 3.0.1 | 使える | 運用、静的、有効化 | 2026-11-06 |  |
| `wp-reviews-plugin-for-google` | 67 | 14.3 | 使える | 静的、有効化、比較 | 2026-11-06 | 0.2.18 以前は、`trustindex_google_views` の主キー（`date` の 1 列）が作れず、有効化が止まった |
| `wp-seo-multilingual` |  | 2.1.0（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | WPML が要る（WPML を有効にして確かめた） |
| `wp-smushit` | 65 | 4.3.6 | 使える | 静的、有効化 | 2026-11-06 | `SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した）。`mediumint` の AUTO_INCREMENT の表がある |
| `wp-stateless` |  | 4.4.3 | 使える | 静的、有効化 | 2026-11-06 | 有効化でまだない `wp_sm_sync` を読む（MySQL でも同じエラー） |
| `wp-super-cache` | 37 | 3.1.5 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wp-sweep` |  | 2.0.1 | 使える | 静的、有効化 | 2026-11-06 | 表の最適化（`OPTIMIZE TABLE` に全部の表を並べる）は、0.2.21 以前は構文エラーで失敗した（段階 74 で直した） |
| `wp2html` |  | 1.0.3 | 使える | 静的、有効化 | 2026-11-06 | HeimDB の差はない。有効化が完了しない（MySQL でも同じ。HeimDB の判定とは別に、今の WordPress で動くかを確かめる） |
| `wpforms-lite` | 9 | 2.0.4 | 使える | 静的、有効化、比較 | 2026-11-06 | 0.2.18 以前は、分析の表 `wpforms_analytics_forms`（主キー `(form_id BIGINT, period_date DATE)`）が作れなかった。`SHOW INDEX ... WHERE` で索引の有無を確かめる（0.2.17 以前の HeimDB は誤った答えを返した） |
| `wpml-media-translation` |  | 2.7.4（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | WPML が要る（WPML を有効にして確かめた） |
| `wpml-string-translation` |  | 3.2.12（入手したバージョン） | 使える | 静的、有効化 | 2026-11-06 | WPML が要る（WPML を有効にして確かめた） |
| `wps-hide-login` | 27 | 1.9.19 | 使える | 静的、有効化 | 2026-11-06 |  |
| `wpvivid-backuprestore` | 69 | 0.9.139 | 使える | 静的、有効化 | 2026-11-06 | バックアップの作成は WP-CLI から起動できず確かめていない（MySQL でも同じ） |

## 判定の基準（HeimDB が拒否するもの）

| 拒否するもの | HeimDB のエラー | 例 |
|---|---|---|
| `AUTO_INCREMENT` の列があり主キーのない表（主キーのない表は段階 72 から作れる） | `HeimDB: table X has an AUTO_INCREMENT column but no primary key; ...` | |
| 整数、`CHAR`、`VARCHAR`、`BINARY`、`VARBINARY`、`DATE`、`DATETIME`、`TIMESTAMP`、`ENUM` のどれでもない列を含む主キー（`TEXT`、`SET`、`DECIMAL` など。0.2.18 以前は、整数の列か 1 列の `CHAR` / `VARCHAR` だけ） | `HeimDB: table X: primary key column "c" has type T; primary key columns must be ...` | |
| 前方の長さのある主キー（`PRIMARY KEY (k(16))`） | `HeimDB: a prefix length on primary key column k is not supported` | |
| 生成列（`GENERATED ALWAYS AS`、`AS (...) STORED`） | `HeimDB: generated column "c" is not supported` | |
| utf8mb4 以外の文字セットの表と列（latin1 など。utf8、utf8mb3、ascii は utf8mb4 の同じ名前の照合順序として通す。ascii は 0.2.10 から） | `HeimDB: table X uses character set latin1; only utf8mb4 is supported ...` | |
| 全文検索（`MATCH ... AGAINST`） | エラー | 検索のプラグイン |
| ストアドプロシージャ、関数、トリガー、ビュー、イベント | エラー | |
| サーバのファイルを読み書きする SQL（`LOAD DATA INFILE`、`INTO OUTFILE`、`LOAD_FILE()`） | エラー | |
| HeimDB の構文解析が受け付けない書き方 | `syntax error ...` | 見つかっているものはない（引用符で囲んだエンジン名 `ENGINE='InnoDB'` は 0.2.10 から受け付ける） |

詳しい差は [`differences.md`](differences.md#wordpress-専用として割り切っている違い) の「WordPress 専用として割り切っている違い」。

## マージから外す表

ブランチ（ステージング）からマージしたくない表は、`heimdb_merge_exclude` で外せる（`docs/procedures.md` の「マージから外す表」。0.2.12 から）。外した表は本番の定義とデータのまま残り、マージの結果に `excluded` の行が出る。プラグインは、その行を見て、マージの後に本番で作り直す。

| プラグイン | 外す表 | 理由 | マージの後にすること |
|---|---|---|---|
| Yoast SEO（`wordpress-seo`） | `<接頭辞>yoast_%`（`yoast_indexable`、`yoast_indexable_hierarchy`、`yoast_primary_term`、`yoast_seo_links`、`yoast_migrations` など） | 索引は記事の保存やページの表示のたびに両側で増え、別の表がその ID を指す（ステージングのものをマージしたい場面はない）。ブランチが Yoast の更新をまたぐと、表の定義も片側だけ変わる | 下の手順 |

Yoast SEO を外したときの、マージの後の手順:

1. ブランチで Yoast を更新していたら（Yoast のバージョンが本番と違えば）、本番の `wp_options` の `yoast_migrations_free` を消す。Yoast はマイグレーションをどこまで済ませたかをこのオプションに持ち、このオプションは `wp_options` の行としてマージで本番に入るので、そのままだと、本番の古い定義の表のまま「済んだ」と判断する。消すと、Yoast が `yoast_migrations` の表（外したので本番の記録）を見て、足りないマイグレーションを本番の表に流す。
2. 索引を作り直す（`wp yoast index` など）。時間がかかるので、マージのフックの中で待たずに、予約イベントやジョブに回す。
3. マージを戻した（`undo`）ときも、索引を作り直す（外した表は戻さないので、索引がマージの後の内容のまま残る）。

## ブランチのマージで注意の要る表

ブランチで足した行の AUTO_INCREMENT は、`bigint` と `int`（符号の有無とも）の列なら本番と重ならない番号帯から採るので（`docs/procedures.md` の `heimdb_branch_create`。`int` は 0.2.13 から）、両側で行を足してもマージは競合にならず、表どうしの参照（Redirection の `redirection_items.group_id` など）もそのまま正しい行を指す。次の場合は、本番とブランチが同じ番号を振るので、両側で同じ表に行を足すと `added on both sides` の競合でマージが止まる。この競合はブランチの側を採れず（`'parent'` で解決するとブランチで足した行は捨てる）、ワイルドカードの `'branch'` を記録しても残る（差分の `detail` とマージのエラーに `only 'parent' can resolve this` と出る）。

- `tinyint`、`smallint`、`mediumint` の AUTO_INCREMENT の表（番号帯を分けるには幅が足りない）。`scripts/plugin-check/check.py` が「注意」として出す。
- 0.2.12 以前の HeimDB で作ったブランチの、それまでに足した `int` の行（作り直したブランチや、次に足す行は番号帯から採る）。
- ブランチ ID が 100 違う 2 つのブランチ（`int` の番号帯の枠を使い回す）が同時にあり、両方が同じ表に足したとき。同じ枠の前のブランチのマージより前の時点からブランチを作ったときと、マージの後に本番で消した行の番号を 100 本後のブランチが振り直したとき（消した行を指していた参照が、新しい行を指す）も重なる。

作り直せる表なら、マージから外す。

同じデータベースに別の接頭辞で複製を作るプラグイン（WP Staging の `wpstg0_` など）の表が残っていると、WordPress の表の接頭辞が 2 つ見つかり、マージが `ambiguous_prefix`（51006）で止まる。使わない複製の表は消す（マルチサイトのサブサイトの `wp_2_` などは数えない）。
