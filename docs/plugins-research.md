# プラグインの調べた記録

[`plugins.md`](plugins.md) の一覧を、どう選び、どう調べたか。調べる途中で見つかった HeimDB の差と、判定が変わったもの。

## 調べたもの

| 調べた日 | HeimDB | 選び方 | 本数 |
|---|---|---|---|
| 2026-10-16 | 0.2.9 | Tsukinowa Inc. が HeimDB の上で運用している WordPress で使えるようにしているもの | 24 |
| 2026-10-16 | 0.2.9 | Tsukinowa Inc. が運用している WordPress のサイトで使われているもの（上の 24 本を除く） | 52 |
| 2026-10-20 | 0.2.17 | wordpress.org の人気の上位 100（`https://wordpress.org/plugins/browse/popular/` の順。`api.wordpress.org` の `browse=popular`）。上の 2 つで調べた 22 本を除く | 78 |
| 2026-11-06 | 段階 107（0.2.36 の後） | 上の 3 つを調べ直した（`hello` と `s3-uploads` のほか。wordpress.org にあるものは最新バージョン、入手したバージョンは手元にあるいちばん新しいバージョン）。人気の上位 100 に新しく入った `forminator` を足した | 155（新しく 1） |

一覧は 157 本。WP Mail SMTP と W3 Total Cache は、0.2.10 で直した後に調べ直した。`SHOW INDEX ... WHERE` を使うもの（`seo-by-rank-math`、`duplicator`、`fluent-smtp`、`google-analytics-for-wordpress`、`wp-smushit`、`wpforms-lite`、`sg-security`、`smart-slider-3`）は、0.2.18 で比較を流し直し、HeimDB でだけエラーになる文が `wpforms-lite` の分析の表（主キー）のほかにないことを確かめた。

wordpress.org にないプラグイン（有料版、GitHub で配られるもの）は、入手できたいちばん新しいバージョンを調べた（一覧の「入手したバージョン」）。最新バージョンではないことがある。作者が自分のサイトで配っているものは、そこから取った（「公式サイト」）。

## 調べ方

2026-11-06 の調べ直しは、静的な検査を新しいキャッシュ（`TMPDIR` を空のディレクトリに）で流し、有効化（`activate.sh`。プラグインごとに、HeimDB が拒否した文、有効になったか、文のエラーのほかの HeimDB の警告を出す）を 18 本ずつに分けて流し、拒否した文があったものだけを比較（`compare.sh` の `scenarios/activate.sh`）で本物の MySQL と比べた。WordPress は `wordpress:php8.3-apache` の 7.1.4。比較のシナリオ 8 つも流し直した。

- **静的**: `scripts/plugin-check/check.py <スラッグ か ディレクトリ>`。wordpress.org の最新バージョンを取り（ディレクトリを渡せばそのバージョンを読み）、ソースの `CREATE TABLE` の主キーと文字セット、全文検索、ストアドプロシージャなど、ファイルを読み書きする SQL を探す。ソースの文字列を読むだけなので、コードで組み立てる表（Yoast SEO のマイグレーションなど）は見えず、構文の違い（`ENGINE='InnoDB'`）も見えない。大文字の `CREATE TABLE` で列の一覧が読めないもの（列の一覧が変数、表の名前に引数のある関数呼び出し）は「表の定義が読めない」として出すので、目で確かめる。`ALTER TABLE` で後から足す列や主キーは調べない。
- **有効化**: `scripts/plugin-check/activate.sh <スラッグ か ディレクトリ>`。このリポジトリの HeimDB（ポート 3336）と WordPress（Docker）を立て、プラグインを 1 つずつ有効にし、トップページと管理画面を開いて予定の cron を流し、その間に HeimDB が拒否した文を出す。dbDelta が表の有無を確かめる文の `table not found` は、MySQL でも同じなので数えない。有効にしただけでは作らない表（設定の画面や機能を有効にしたときに作る表）は見えないので、静的な検査と合わせる。前提のプラグインが要るもの（WooCommerce や Elementor のアドオン）は、`REQUIRES=woocommerce scripts/plugin-check/activate.sh <スラッグ>` で前提を有効にしたまま確かめる。
- **比較**: 有効化だけでは主な機能の SQL が流れないもの（EC、バックアップ、検索と置換、セキュリティのスキャン）は、`scripts/plugin-check/compare.sh <シナリオ>` で、同じ手順（WP-CLI で商品と注文を作る、`wp eval` でバックアップを取る、など）を HeimDB と本物の MySQL 8.0（Docker）の上の WordPress で続けて流し、エラーになった文を比べる（MySQL は `performance_schema.events_statements_history_long` の `MYSQL_ERRNO`。HeimDB でだけエラーになった文があれば終了コード 1）。シナリオは `scripts/plugin-check/scenarios/` にある（有効化だけの `activate.sh` は `PLUGINS` にスラッグを渡す。ほかに WooCommerce、UpdraftPlus（バックアップと復元）、Better Search Replace、Wordfence、Loginizer、WP Cerber。`keyless-branch.sh` は HeimDB のブランチを使うので `DB=heimdb` で流す）。
- **運用**: HeimDB の上で動く WordPress のサイトに入れて、管理画面と主な機能を使って確かめた。
- 「MySQL でも同じ」としたものは、本物の MySQL 8.0 で同じ文、同じ有効化を試して確かめた。

## 使えないものの詳細

- **WooCommerce**（`woocommerce`、条件付き）: 注文の全文検索（設定で有効にしたときだけ）は、全文検索の索引を拒否するので使えない。注文（HPOS の表）、商品、属性の絞り込み、管理画面は MySQL と同じに動いた（0.2.19 の比較。HeimDB でだけエラーになった文は 0）。

## 判定が変わったもの

- **主キーのない表（段階 72 で使えるようになった）**: 0.2.19 以前の HeimDB は主キーのない表を拒否し、次のものが使えなかった。段階 72 で主キーのない表を作れるようにし（docs/plan.md の段階 72）、2026-10-24 に比較のシナリオ（`scripts/plugin-check/scenarios/loginizer.sh`、`wp-cerber.sh`）で調べ直した（どちらも HeimDB でだけエラーになった文は、WP Cerber の `OPTIMIZE TABLE` のほかに 0）。Fable のレビューの修正の後にも流し直し、同じだった。`keyless-branch.sh` で、両方の表をブランチで変えて（Loginizer の記録の追加、Cerber の ACL の追加とログの追加と削除）本番にマージし、本番でプラグインが書き続けた行と合わさること、マージの後もログイン失敗の数え上げとロックアウト、ログ、通信の記録、`UPDATE` と `DELETE` が動くことを確かめた。ソースから主キーのない表を探し直すと（154 本）、ほかに UpdraftPlus の復元が権限の確かめに作る表があった（下）。このとき wordpress.org に新しいバージョンが出ていた 23 本（`all-in-one-seo-pack`、`wpforms-lite`、`w3-total-cache` など）は、最新バージョンで静的な検査と有効化の比較を流し直し（WooCommerce と Elementor のアドオンは、それを有効にして）、HeimDB でだけエラーになった文は 0 だった（表のバージョンと調べた日を更新した）。
  - **WP Cerber**（`wp-cerber`）: 主キーのない表（`cerber_acl`、`cerber_lab`、`cerber_qmem`、`wp_cerber_uss`、ログの `cerber_log` と通信の記録の `cerber_traffic`）があり、有効化が止まった。ログと通信の記録の表はコードで組み立てる（`CRB_Schema_Manager`）ので、静的な検査には見えず、有効化で見つかる。wordpress.org では 2022-09-22 にセキュリティの問題で公開が止まり、公式サイト（`https://downloads.wpcerber.com/plugin/wp-cerber.zip`）で配られている。調べたのは公式サイトの 9.10。手入れの cron が流す `OPTIMIZE TABLE` は HeimDB が受け付けない文で、構文エラーになる（主キーのない表とは関係なく、どの表でも同じ）。
  - **Loginizer**（`loginizer`）: ログイン制限のログの表 `loginizer_logs` に主キーがなく（`UNIQUE KEY ip` だけ）、表が作れず、ログイン制限の読み書きがすべて失敗した。
  - **UpdraftPlus**（`updraftplus`）: データベースの復元の前に、権限を確かめる表 `CREATE TABLE updraft_tmp_... (test INT)`（主キーがない）を作る。0.2.19 以前の HeimDB ではこれが作れず、コードでは「表を作る権限がない」とみなして、表を空にして入れ直す復元になる（この経路は流していない）。段階 72 からは、一時的な接頭辞の表に入れてから名前を変える復元（atomic restore）になり、`scenarios/updraftplus-restore.sh` で、本物の MySQL 8.0 と同じく復元できた（消した記事が戻る）。HeimDB でだけエラーになった文は、ストアドファンクションを作れるかの確かめ（`CREATE FUNCTION`、`DROP FUNCTION`）だけ。
- **文字列、バイト列、日付、`ENUM` を含む主キー（0.2.19 で使えるようになった）**: 0.2.18 以前の HeimDB は、主キーに整数の列と 1 列の `CHAR` / `VARCHAR` しか使えず、次のものが使えなかった。0.2.19 で、`CHAR`、`VARCHAR`、`BINARY`、`VARBINARY`、`DATE`、`DATETIME`、`TIMESTAMP`、`ENUM` をどう組み合わせても使えるようにし（docs/plan.md の段階 70）、2026-10-21 に調べ直した（WooCommerce と Wordfence は比較のシナリオ、WPForms Lite と WP Reviews for Google は有効化の比較、WPML とアドオンは有効化。どれも HeimDB でだけエラーになった文は 0）。
  - **WPML**（`sitepress-multilingual-cms`）: 有効化で作る `icl_locale_map` の主キーが `(code, locale)`（2 列の `VARCHAR`）で、作れずに有効化が止まった。WPML の上で動く `acfml`、`wpml-media-translation`、`wpml-string-translation`、`wp-seo-multilingual` も、単独では有効になるが WPML なしでは役に立たない。調べたのは入手した 4.6.11（有料で最新バージョンを取れない）。
  - **Wordfence**（`wordfence`）: 有効化で作る `wfls_role_counts` の主キーが `(serialized_roles VARBINARY(255), two_factor_inactive TINYINT, passkey_inactive TINYINT)` で、作れずに有効化が止まった。これが通ると、`BINARY(16)` の IP を主キーに含む表（`wfblockediplog`、`wfcrawlers`、`wflocs`、`wfreversecache`、`wflivetraffichuman`、`wffilemods` の `filenameMD5`）と、`ENUM` を含む `wftrafficrates (eMin, IP, hitType)` も作れなかった。
  - **WP Reviews for Google**（`wp-reviews-plugin-for-google`）: `trustindex_google_views` の主キーが `DATE` の 1 列で、作れずに有効化が止まった。
  - **WooCommerce**（`woocommerce`）: 属性の絞り込みのための表 `wc_product_attributes_lookup` の主キーが `(product_or_parent_id, term_id, product_id, taxonomy VARCHAR(32))` で作れなかった。新しく入れた WooCommerce はこの表を使う設定（`woocommerce_attribute_lookup_enabled`）が有効なので、属性（色など）で絞り込んだ商品の一覧の文が失敗し、商品を保存するたびにこの表への書き込みがエラーになった。
  - **WPForms Lite**（`wpforms-lite`）: 分析の表 `wpforms_analytics_forms` の主キーが `(form_id BIGINT, period_date DATE)` で作れず、フォームの表示数と送信数の分析が使えなかった。フォームの作成と送信は動く。
- WP Mail SMTP（`ENGINE='InnoDB'` で作る、デバッグイベントの記録とメールの送信キューの表）と W3 Total Cache（CDN を有効にしたときの、`CHARACTER SET ascii` の列の表）は、0.2.9 では条件付きだったが、0.2.10 で使えるようになった。

## 見つかった HeimDB の差

wordpress.org の人気の上位 100 を調べる途中で見つかった、MySQL と違う振る舞い。どれも本物の MySQL 8.0 と比べて確かめた。

- **列を付けた `DESCRIBE`（段階 107 で直した）**: `DESCRIBE 表 列`（`DESC`、`EXPLAIN` も。列は名前か `%` と `_` を使える文字列）が構文エラーになっていた（MySQL はその列の行を `SHOW COLUMNS` と同じ形で返す。ない列なら空）。`intuitive-custom-post-order` が、無効にするときに列があるかを確かめるのに使い、`wp_terms` の `term_order` の列が残っていた。MySQL は `SHOW COLUMNS` の `LIKE` と `WHERE` で列の名前の大文字と小文字を区別しないが、HeimDB は区別する（前からの差。プラグインは作ったときと同じ名前で確かめる）。
- **インデックスのある `ENUM` の列と、定義にない文字列の比較（段階 107 で直した）**: `status = ''` がエラー 1105（`value  is not valid for this Enum`）になっていた（インデックスのない列は前から比べられた）。`forminator` が、バージョンを上げるときの移し替えで流す。MySQL は定義にない値を空の値として保存し、`''` はその行に合う。HeimDB は定義にない値を保存しない（エラーにする）ので、合う行はない。定義にない値で引くときは、インデックスを全部読んで条件で落とす（結果は同じ。Forminator がバージョンを上げるときに 1 回流すだけ）。

- **`OPTIMIZE TABLE`、`CHECK TABLE`、`REPAIR TABLE`（段階 74 で直した）**: 0.2.21 以前の HeimDB は構文エラーにしていた（MySQL は表を作り直し、結果の表を返す）。WP Cerber の手入れの cron が流す（段階 72 の比較で見つかった）。ほかに WP-Optimize、Advanced Database Cleaner、WP-Sweep、WordPress の `wp-admin/maint/repair.php`、wp-cli の `wp db check` / `optimize` / `repair`（mysqlcheck）が使う。段階 74 で、何もせずに MySQL と同じ形の結果を返すようにした（docs/plan.md の段階 74）。
- **`SHOW INDEX`（`SHOW KEYS`）の `WHERE` を無視していた（0.2.18 で直した）**: `SHOW INDEX FROM t WHERE Key_name = 'x'` が、絞り込まずにすべての索引を返していた（エンジンの `buildShowIndex` が `WHERE` を読まなかった）。プラグインは「行が返れば索引がある」と判断するので、索引を足す処理は足さず、消す処理はない索引を `DROP INDEX` してエラーになっていた（`smart-slider-3`、`sg-security`、`google-listings-and-ads` の有効化）。0.2.17 以前の HeimDB で有効にしたプラグインは、足すべき索引がないことがある（プラグインのバージョンを上げたときの索引の作り直しなど。`SHOW INDEX FROM 表` で確かめる）。MySQL は `Key_name` と `Column_name` の値を大小文字を区別せずに比べるが、HeimDB は区別する（プラグインは作ったときと同じ名前で確かめるので、困らない）。上位 100 では `woocommerce`（`wp_comments` に足す索引、注文の全文検索の索引の有無、税の表の主キーの確かめなど 6 ファイル）、`seo-by-rank-math`、`wordfence`、`wpforms-lite`、`duplicator`、`fluent-smtp`、`google-analytics-for-wordpress`、`google-listings-and-ads`、`sg-security`、`smart-slider-3`、`wp-smushit` が使う。ほかの `SHOW`（`COLUMNS`、`TABLES`、`TABLE STATUS`、`VARIABLES`）の `WHERE` と `LIKE` は正しい。
- **`DO 1` が構文エラーになっていた（0.2.18 で直した）**: WordPress 本体の `wpdb::check_connection()` が、接続が生きているかを `DO 1` で確かめる。HeimDB は `DO` を受け付けず、WordPress は切れたと判断してつなぎ直していた（つなぎ直しは成功するので、止まりはしない）。本体は接続が切れたとき（2006）だけ呼ぶが、`updraftplus` と `duplicator` はバックアップの途中で表ごとに呼ぶ。バイナリプロトコルの準備（COM_STMT_PREPARE）の `DO` は構文エラーのまま（WordPress は使わない。`internal/backend/do.go`）。
- **文字列、日付、バイト列を含む主キー（0.2.19 で直した）**: 上位 100 の `woocommerce`、`wordfence`、`wpforms-lite`、`wp-reviews-plugin-for-google` がこれで表を作れなかった（上の「判定が変わったもの」）。
- **`FLOAT(M,D)`、`DOUBLE(M,D)` の (M,D) と UNSIGNED を落とす（記録だけ。段階 119 で数えた）**: HeimDB は表の定義に (M,D) と UNSIGNED を残さず、保存するときに D 桁に丸めず、範囲も確かめず、素の FLOAT と DOUBLE として表す（直すにはデータ形式のバージョンを上げる。[`differences.md`](differences.md) の「FLOAT(M,D) と DOUBLE(M,D) の (M,D) を落とす」）。2026-11-16 に、[`plugins.md`](plugins.md) の 157 本のうち wordpress.org と GitHub から取れた 151 本（`hello` は `hello-dolly` として。有料の `advanced-custom-fields-pro`、`acfml`、`otgs-installer-plugin`、`wp-seo-multilingual`、`wpml-media-translation`、`wpml-string-translation` は取れない）の最新バージョンのソースを、`FLOAT`、`DOUBLE`（`DOUBLE PRECISION`）、`REAL` の `(M,D)` と、`FLOAT`、`DOUBLE`、`REAL` の UNSIGNED の列の定義で探した。使うのは 2 本で、どちらも有効化と主な機能は前から使える（判定は変わらない）。
  - `wordfence` 9.1.1: `lib/wfSchema.php` の `wfAuditEvents` と `wfSecurityEvents` の `event_time`（DOUBLE(14,4)）、`wfHits` の `attackLogTime` と `ctime`、`wfLogins` と `wfStatus` の `ctime`（DOUBLE(17,6) UNSIGNED）、`wfLocs` の `lat` と `lon`（FLOAT(10,7)）。`wordfenceClass.php` のバージョンアップも `attackLogTime` を DOUBLE(17,6) UNSIGNED で足す。表は `CREATE TABLE IF NOT EXISTS` で作る（dbDelta は使わない）。時刻（`microtime(true)`）は 6 桁より細かい桁が残るだけ。`wfLocs` の緯度と経度は PHP に 6 桁の有効数字で返る（本物の MySQL で 35.6812363 の値が 35.6812）。
  - `mainwp-child` 6.4.2: `modules/changes-logs` の `changes_logs` の `duration`（FLOAT(11,4)）。dbDelta で作るので、DESCRIBE の型（`float`）と定義が違い、表を作る処理を流すたびに `ALTER TABLE ... CHANGE COLUMN` を出す（表のバージョンが変わったときだけ流す）。7 桁以上の値は 6 桁の有効数字になる（1234567.12345 が MySQL は 1234567.1250、HeimDB は 1234570）。
- TLS: MariaDB の新しいクライアント（WordPress の公式イメージの `mysqldump` など）は、既定で TLS を求める。`-tls-cert` を付けない HeimDB では接続を拒否される（`updraftplus` は PHP での書き出しに切り替えた）。本物の MySQL 8.0 は、何も設定しなくても TLS で話す。

## 調べ直す

プラグインの新しいバージョンを使うときや、HeimDB のバージョンを上げたときは、静的な検査と有効化を流し直す。

```sh
scripts/plugin-check/check.py --quiet redirection wp-mail-smtp ~/sites/x/wp-content/plugins/acfml
scripts/plugin-check/activate.sh redirection wp-mail-smtp ~/sites/x/wp-content/plugins/acfml
```

`activate.sh` は Docker と Go が要る。HeimDB はポート 3336（`HEIMDB_PORT`）、WordPress は 127.0.0.1:18090（`WP_PORT`）で動かし、終わったら消す。
