# HeimDB

[English](README.md) | 日本語

WordPress 専用の、ブランチとタイムトラベルのできるデータベース。MySQL のプロトコルで話し、WordPress、wp-cli、mysqldump からは MySQL に見えるので、接続先を替えるだけで使える。

- **タイムトラベル**: 過去の任意の時点のデータを読み、その時点に戻せる（戻したことも取り消せる）。
- **ブランチ**: 本番からコピーなしでステージングを切り、差分を見て本番にマージできる（その間に本番に入ったコメントは残る）。

> **開発中**。データ形式は 0.2.41 からバージョン 8（0.2.40 以前のバージョン 1 から 7 のデータは、起動時にそのまま移る。変えるときは、新しいバージョンが起動時に移す。上げた後のデータは古いバージョンでは開けない。そのように変えるときはマイナーバージョンを上げ（0.3.x から 0.4.0）、ほかはパッチのバージョンを上げる）。バックアップを取り、バージョンを決めて使うこと（手順は [`docs/operations.md`](docs/operations.md) の「HeimDB のバージョンを上げる」）。MySQL との違いは [`docs/differences.md`](docs/differences.md)、計画と経緯は [`docs/plan.md`](docs/plan.md)。

## 性能

同じ WordPress とデータ（記事 5,202 件、コメント 2 万件、オブジェクトキャッシュなし）での、本物の MySQL 8.0 との比較（2026-11-19、HeimDB 0.2.42）。GCE の n2-standard-4（東京、pd-balanced）の 1 台で、MySQL は Docker の既定の設定。測り方と全部の値は [`docs/bench.md`](docs/bench.md)。

| ページの表示（中央値） | HeimDB 全体 | MySQL 全体 | HeimDB の DB | MySQL の DB |
|---|---|---|---|---|
| トップページ | 114 ms | 97 ms | 30 ms | 15 ms |
| 個別の記事（コメント 60 件） | 196 ms | 175 ms | 37 ms | 19 ms |
| カテゴリの 2 ページ目（記事 5,001 件） | 130 ms | 103 ms | 46 ms | 21 ms |
| 検索 | 145 ms | 189 ms | 56 ms | 103 ms |
| REST API（記事 100 件） | 330 ms | 276 ms | 102 ms | 59 ms |
| 管理画面のダッシュボード | 126 ms | 100 ms | 68 ms | 44 ms |
| 投稿一覧 | 156 ms | 131 ms | 73 ms | 50 ms |

| 書き込み（1 回あたりの DB の時間）と同時アクセス | HeimDB | MySQL |
|---|---|---|
| 記事の作成 | 64 ms | 63 ms |
| コメントの追加 | 3.3 ms | 6.5 ms |
| オプションの更新 | 1.3 ms | 3.1 ms |
| 記事の削除 | 43 ms | 46 ms |
| トップページ（10 並列） | 22.4 件/秒 | 26.0 件/秒 |
| 個別の記事（10 並列） | 12.3 件/秒 | 13.7 件/秒 |

ページの DB の時間は MySQL の 1.5〜2.1 倍で、ページ全体では 1.1〜1.3 倍（検索は MySQL より短い）。同じインスタンスで測った 0.2.35 より、ページの DB の時間は 4〜11% 短い（うちトップページ、個別の記事、検索、カテゴリの 4〜8% は、ビルドの配置による揺れと同じくらい）。速度は目標にしていない（公開ページはページキャッシュで返す前提）。

負荷への強さ（DB と負荷を別の n2-standard-4 に置き、7 つのページの文を多くの接続から流した。HeimDB 0.2.42）: 4 vCPU を使い切ったときの速さの上限は MySQL の 88%（毎秒 57.5 ページと 65.5 ページ）で、同じ速さでのサーバーの CPU は MySQL の 1.4 倍。上限を超えても、500 接続でも、誤りや切断はなく、30 分流しても時間とメモリは増えない（RSS は約 260 MB で、MySQL の半分以下）。詳しくは [`docs/bench.md`](docs/bench.md) の「段階 126」。

メモリは、常駐が表の行数にほぼよらず約 500 MiB（`wp_postmeta` の形の 310 万行で）。大きな書き込み、マージ、復元は行数に比例して使うが、`-memory-limit` に近づくとエラー 1041 で止まる。経路ごとの目安は [`docs/operations.md`](docs/operations.md) の「運用の注意」の「メモリの目安」。

## ビルドと起動

Go 1.27 以上が必要。

```sh
go build -o heimdb ./cmd/heimdb
./heimdb -addr 0.0.0.0:3306 -data ./data -root-password 'secret' -site-password 'site-secret'
```

初回の起動で、`-root-password` のパスワードで `root@'%'`（管理者）を作る。`-site-password` があれば、WordPress がつなぐユーザー `wordpress@'%'` を、本番のデータベースだけの権限で作る（まだいなければ）。本番のデータは `-db` の名前（既定 `wordpress`）のデータベースに見える。Docker のイメージもある（[`tsukinowa/heimdb`](https://hub.docker.com/r/tsukinowa/heimdb)）。WordPress と一緒に起動するなら、リポジトリの直下の [`compose.yaml`](compose.yaml) を使う。

```sh
export HEIMDB_ROOT_PASSWORD='secret' HEIMDB_SITE_PASSWORD='site-secret'
docker compose up -d                                   # WordPress は http://localhost:8080
docker compose run --rm cli wp core install --url=http://localhost:8080 --title=Site \
  --admin_user=admin --admin_password='...' --admin_email=you@example.com
```

メモリの上限（`-memory-limit`）は、コンテナの中ならコンテナの上限の 8 割になる。コンテナの外で動かすなら指定する。上限に近づくと、文がエラー 1041 で止まってトランザクションが取り消され、プロセスは落ちない。

フラグの一覧、Docker（ステージングの起動とマージ）、メモリの目安、レプリカ、通信の暗号化（TLS）は [`docs/operations.md`](docs/operations.md)。

## WordPress から使う

`wp-config.php` の接続先を HeimDB にするだけ。

```php
define( 'DB_NAME', 'wordpress' );          // ステージングなら 'wordpress@staging'
define( 'DB_USER', 'wordpress' );          // root ではなく、そのデータベースだけの権限のユーザー
define( 'DB_PASSWORD', 'site-secret' );
define( 'DB_HOST', 'heimdb-host:3306' );
```

WordPress は root でつながない。root でつなぐと、プラグインの穴から HeimDB の手続き（復元やブランチの削除）、ユーザーの作成、ほかのブランチまで届く。root は管理者が `CALL heimdb_*` や wp-cli に使う。

ステージングの URL は、そのサイトの `wp-config.php` の `WP_HOME` と `WP_SITEURL` で切り替える。

MySQL で動いているサイトは、mysqldump で移せる（HeimDB から MySQL に戻すこともできる）。

```sh
mysqldump --single-transaction --set-gtid-purged=OFF -h mysql-host -u root -p wordpress > site.sql
mysql -h heimdb-host -u root -p wordpress < site.sql
```

HeimDB はコミットしていない書き込みをメモリに持つ。`mariadb-dump` は既定で表ごとに 1 つのトランザクションで囲むので、大きな表があるなら `--skip-no-autocommit` を付けて取る。

使えるプラグインと一部が使えないプラグイン（WooCommerce は注文の全文検索が使えない）の一覧は [`docs/plugins.md`](docs/plugins.md)。HeimDB は、ビュー、トリガー、全文検索、ストアドプロシージャ、utf8mb4 以外の文字セットなどを明示的なエラーで拒否する。

## 使い方

HeimDB の操作は、管理者が SQL の `CALL` で行う。手続きの一覧、引数、結果の見方は [`docs/procedures.md`](docs/procedures.md)。

### タイムトラベル

```sql
-- 過去の時点のデータを読む（時刻はセッションの time_zone で解釈する）
SELECT ID, post_title FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;

-- 消した記事を 1 件だけ戻す
INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
```

### 復元

```sql
-- その時点から後に変わった行を見る
CALL heimdb_changes('main', '2026-10-08 09:00:00');
-- 戻すと何件が変わるかを見る
CALL heimdb_restore_preview('main', '2026-10-08 09:00:00');
-- その時点に戻す（コメントとユーザーは戻さない。すべて戻すなら '*'）
CALL heimdb_restore('main', '2026-10-08 09:00:00');
CALL heimdb_restore('main', '2026-10-08 09:00:00', 'wp_options,wp_posts');  -- テーブルはカンマ区切り
```

復元は過去のバージョンを新しいバージョンとして書くだけなので、履歴は消えない。結果に出る「復元の直前の時刻」に復元し直せば元に戻る。

### ブランチ

```sql
CALL heimdb_branch_create('staging');                 -- 今の main から切る
CALL heimdb_branch_create('test', 'main', '2026-10-08 09:00:00');  -- 過去の時点から切る
CALL heimdb_branch_list();
CALL heimdb_branch_delete('test');
```

ブランチは `wordpress@staging` という名前のデータベースに見える。ステージングの WordPress は、そのブランチだけの権限のユーザーでつなぐ（普通の `GRANT` で与える。ステージングのサイトが乗っ取られても本番に届かない）。

```sql
CREATE USER 'staging'@'%' IDENTIFIED BY 'pw';
GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%';
```

### 差分とマージ

```sql
CALL heimdb_branch_diff('staging');                -- テーブルごとの要約
CALL heimdb_branch_diff('staging', 'wp_posts');    -- 行ごとの判定（'*' ならすべてのテーブル）
-- 両側が同じ列を違う値に変えた行（競合）は、どちらを採るかを記録する
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=42', 'branch');   -- 行は差分の id の列の形。'branch' | 'parent' | 'none'
CALL heimdb_branch_merge('staging', 'dry_run');    -- 書かずに、マージできるかと件数を確かめる
CALL heimdb_branch_merge('staging');               -- 親（main）に 1 つのコミットで反映する
```

行は主キーで対応づけ（`wp_options` は `option_name`）、列ごとに三方向で比べる。ブランチだけが変えた列は反映し、本番だけが変えた列は残す。解決していない競合が残っていれば、マージは何も書かずに拒否する。インデックスと列の定義の変更（プラグインの dbDelta）もマージする。マージが何かを書けば、結果の最後の行に、マージを戻す `heimdb_restore` の文が出る。作り直せる索引など、マージしたくない表は `CALL heimdb_merge_exclude('wp_yoast_%')` で外せる（本番の表をそのまま残す）。マージを拒否するときは、理由の語と番号（51001〜）をエラーに付ける。

ほかのブランチを本番にマージした後、検証中のブランチに本番の最新を取り込める（`CALL heimdb_branch_sync('staging')`）。親だけが変えた列をブランチに書き、ブランチの検証中の変更は残す。競合があれば拒否し、`'branch'` か `'parent'` でまとめて決める。取り込んだ後のマージは、写した行をブランチの変更に数えない。詳しくは [`docs/procedures.md`](docs/procedures.md) の「heimdb_branch_sync」。

### バックアップと状態

```sql
CALL heimdb_backup('before-upgrade');   -- 動いたまま、整合したバックアップを -backup-dir に作る
CALL heimdb_status();                   -- ディスク使用量、GC、接続数、開いたトランザクション、危ない変更の見張りなど
CALL heimdb_gc();                       -- 古いバージョンの削除をすぐに動かす
CALL heimdb_rebuild_indexes();          -- 前のバージョンで作った DECIMAL、SET、BIT などの列のインデックスにエントリを作る（main は起動の後に裏で作る）
```

バックアップは、すべてのブランチ、保持期間内の履歴、ユーザーと権限を含む。バックアップのディレクトリを `-data` に指定して起動すると、その時点に戻る。

`-metrics-addr 127.0.0.1:9104` を付けると、同じ値と文の数・時間を Prometheus の形式で `/metrics` に出す（New Relic、Grafana などに取り込める。認証がないので外に開けない）。指標の一覧と設定の例は [`docs/operations.md`](docs/operations.md) の「指標」。

読み取り専用のレプリカと、本番が壊れたときの昇格の手順は [`docs/operations.md`](docs/operations.md)。

## 開発

```sh
go test $(go list ./... | grep -v internal/sqlengine)        # 単体テストと、本物の MySQL の結果（internal/testdata）との比較
go test -race -timeout 30m $(go list ./... | grep -v internal/sqlengine)
go test ./internal/sqlengine/...                              # SQL エンジン（go-mysql-server のフォーク）の上流のテスト
go test ./internal/backend -run '^$' -fuzz '^FuzzQuery$' -fuzztime 1h -parallel 4   # SQL のファズ（生のパケットは FuzzPackets。internal/backend/fuzz_test.go）
```

- [`docs/differences.md`](docs/differences.md): MySQL との違い（互換性の確かめの結果、WordPress 専用として割り切っている違い、記録だけにした細かな違い）
- [`docs/procedures.md`](docs/procedures.md): `heimdb_*` の手続きの一覧と使い方
- [`docs/operations.md`](docs/operations.md): 起動のフラグ、Docker、メモリ、バージョンの上げ方、レプリカ、通信の暗号化（TLS）
- [`docs/plugins.md`](docs/plugins.md): プラグインが HeimDB で使えるかの一覧と判定の基準（調べた記録は [`docs/plugins-research.md`](docs/plugins-research.md)）
- [`docs/plan.md`](docs/plan.md): 計画、キーの設計、各段階の実装と結果、検証方法（PHPUnit をベースラインと比べる手順）
- [`docs/bench.md`](docs/bench.md): 本物の MySQL との性能とメモリの比較
- [`docs/engine.md`](docs/engine.md): SQL エンジン（`internal/sqlengine`、go-mysql-server v0.20.0 のフォーク）の取り込み元と変更の一覧
- [`compat/README.md`](compat/README.md): MySQL との互換を WordPress の接続（wpdb）で確かめるテスト集（数秒で、本物の MySQL の記録と比べる）
- `Dockerfile`: Docker のイメージ。`compose.yaml`: HeimDB と WordPress をまとめて起動する。`cmd/heimdb`: サーバ。`cmd/junitdiff`: PHPUnit の結果の比較。`cmd/compatdiff`: 互換のテスト集の記録の比較。`cmd/replay`: ログのクエリの再実行
- [`CONTRIBUTING.md`](CONTRIBUTING.md): 報告と PR の出し方（DCO の署名）。脆弱性の報告は [`SECURITY.md`](SECURITY.md)
- `scripts/`: WordPress（`scripts/wordpress`）、PHPUnit（`scripts/wpphp`）、ベンチマーク（`scripts/bench`）、プラグインの SQL の検査（`scripts/plugin-check`）、タイムトラベルのデモ（`scripts/demo-asof.sh`）

## ライセンス

Copyright 2026 Tsukinowa Inc.

[Apache License 2.0](LICENSE)（[NOTICE](NOTICE)）。SQL エンジン（`internal/sqlengine`）は go-mysql-server v0.20.0（DoltHub, Inc.、Apache License 2.0）から派生したもので、Tsukinowa Inc. が変更している（[`docs/engine.md`](docs/engine.md)）。Pebble は BSD 3-Clause、そのほかの依存も Apache 2.0、BSD、MIT のどれかで、hashicorp/golang-lru だけが Mozilla Public License 2.0（ファイル単位のコピーレフトで、手を加えずに使っている）。バイナリを配るときは、依存のライセンスの表示も同梱すること。

MySQL 互換のテスト集（`compat/`）は、WordPress のテストの仕組みの上に作るので、[GPL-2.0-or-later](compat/LICENSE) とする。Go のモジュールにも Docker のイメージにも入らない。
