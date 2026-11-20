<!--
Docker Hub の tsukinowa/heimdb の説明文の日本語版（英語版は dockerhub.md）。Docker Hub に載せられる説明文は 1 つ
なので、使うときはどちらかを貼る。
- Short description（100 文字まで）: 下の 1 行
- Repository overview: 「---」より下の全部
英語版を変えたら、こちらも合わせる。compose.yaml の内容を変えたときも同じ。
Docker Hub の説明は 25,000 バイトまで。タグの節は最新のバージョンだけを詳しく書き、古いバージョンは上げるときに気を付けることだけを残す。
-->

Short description:

```
WordPress 専用の、ブランチとタイムトラベルのできる MySQL 互換 DB（開発中）
```

---

# HeimDB

WordPress 専用の、ブランチとタイムトラベルのできるデータベース。MySQL のプロトコルで話し、WordPress、wp-cli、mysqldump からは MySQL 8.0 に見えるので、接続先を替えるだけで使える。

- **タイムトラベル**: 過去の任意の時点のデータを読み、サイトをその時点に戻せる（戻したことも取り消せる）。
- **ブランチ**: 本番からコピーなしで一瞬でステージングを作り、差分を見て本番にマージできる。その間に本番に入ったコメントは残る。

> **開発中**。データ形式は 0.2.41 からバージョン 8。新しいバージョンは古いデータを起動時に移し、古いバージョンは新しいデータを開かない。データ形式を変えて古いバージョンでは開けなくなるときは、マイナーバージョンを上げる（0.3.x から 0.4.0）。バージョンを決めて使い、バックアップを取ること。

HeimDB は WordPress 専用。ビュー、トリガー、ストアドプロシージャ、全文検索には対応せず、明示的なエラーを返す。サーバのファイルを読み書きする SQL（`LOAD_FILE()`、`SELECT ... INTO OUTFILE`、`LOAD DATA`）も同じ。

## タグ

- `0.3.0`、`latest`: linux/amd64、linux/arm64。ソースを公開した最初のバージョン（[github.com/tsukinowasha/heimdb](https://github.com/tsukinowasha/heimdb)）。コードは 0.2.45 と同じで、データ形式（バージョン 8）もレプリカとやり取りする記録の形も同じなので、0.2.41 以降からはそのまま上げられる。0.3.0 から、データ形式を変えて古いバージョンでは開けなくなるときにマイナーバージョンを上げ（0.3.x から 0.4.0）、ほかはパッチのバージョンを上げる。0.2.45: 2 本の接続が 1 つのスレッドで交互に走らず、並んで文を処理するようになった（4 vCPU で、2 接続のページの数が 1 接続の 1.58 倍から 1.79 倍。4 接続以上は変わらない）。0.2.44: バイナリに記号表を残した（約 5 MB 大きい）ので、イメージに `govulncheck -mode binary` をかけると HeimDB が呼ぶものだけが出る。0.2.43: GO-2026-6859（net/http。HTTP/1 のサーバが落ちうる。HeimDB が HTTP を使うのは、既定で閉じている指標の口だけ）のため Go 1.27.4 で作った。0.2.42: 本番が起動の後に、古いバージョン（0.2.38 以前。SET と BIT は 0.2.40 以前）で作った main のインデックスを裏で 1 つずつ作り直すので、`CALL heimdb_rebuild_indexes()` を呼ばなくても全件を読まなくなる（止めるのは `-rebuild-indexes=false` か `HEIMDB_REBUILD_INDEXES=false`）。ブランチの作成は親の書き込み中のトランザクションを待つ。結果の行を文ごとのゴルーチンなしで送り、全件の走査で復号した行を使い回すので、ページの DB の時間が 0.2.35 より 4〜11% 短い。データ形式はバージョン 8、レプリカとやり取りする記録の形も 0.2.41 と同じ。

古いタグも pull できる。上げるときに気を付けるバージョンは次のとおり（機能は「使い方」と「設定」を参照）。

- `0.2.41`: **データ形式がバージョン 8**（上げた後は古いバージョンで開けない）。0.2.41 以降の本番は 0.2.40 以前のレプリカを拒否する（昇格した後に FLOAT(M,D) を桁のない FLOAT と読み、新しいインデックスのエントリを保守しない）。レプリカを先に上げる。上げた後に本番で `CALL heimdb_rebuild_indexes()` を呼ぶと、前からある SET と BIT のインデックスにエントリができる。前からある列は桁のない `float` と `double` のまま。上げた後、Wordfence と MainWP Child が宣言の型にする ALTER を 1 回出し、その後は、その前に作ったブランチへの `heimdb_branch_sync` が表の定義の違いで拒否される。
- `0.2.39`: **データ形式がバージョン 7**（上げた後は古いバージョンで開けない）。0.2.39 以降の本番は 0.2.38 以前のレプリカを拒否する（昇格した後に新しいインデックスのエントリを保守しない）。レプリカを先に上げる。上げた後に本番で `CALL heimdb_rebuild_indexes()` を呼ぶと、前からある DECIMAL などのインデックスにエントリができる。
- `0.2.33`: **データ形式がバージョン 6**（上げた後は古いバージョンで開けない）。0.2.33 以降の本番は 0.2.32 以前のレプリカを拒否する（取り込みの記録を知らず、本番に昇格するとマージの基準を読み違える）。レプリカを先に上げる。
- `0.2.20`: **データ形式がバージョン 5**（上げた後は古いバージョンで開けない）。0.2.20 以降の本番は 0.2.19 以前のレプリカを拒否する。
- `0.2.19`: **データ形式がバージョン 4**（上げた後は古いバージョンで開けない）。0.2.19 以降の本番は 0.2.18 以前のレプリカを拒否する。
- `0.2.18`: 0.2.17 以前で有効にしたプラグインは、足すべき索引がないことがある（`SHOW INDEX ... WHERE` が誤った答えを返していた）。`SHOW INDEX FROM 表` で確かめる。
- `0.2.16`: 同じ中身の記事を両側で足した重複では、行ごとの差分の `parent_change` が `insert` になった。`heimdb_branch_diff(name, '')` はエラーになった（要約は `heimdb_branch_diff(name)`）。
- `0.2.15`: 行ごとの差分の最後に `resolved_by`、`resolved_at`、`conflict_values` の 3 列を足した。列を順で読むものは、3 列を足して読む。
- `0.2.12`: マージを拒否するエラーの番号が 51001〜51009 になった。
- `0.2.11`: **データ形式がバージョン 3**（上げた後は古いバージョンで開けない）。0.2.11 以降の本番は 0.2.10 以前のレプリカを拒否する。
- `0.2.8`: 0.2.8 以降の本番に 0.2.7 以前のレプリカをつながない。
- `0.2.5`: 0.2.4 以前は `IDENTIFIED` のない `ALTER USER` でパスワードが消えた。そうして変えたアカウントのパスワードを設定し直す。
- `0.1.6`: **データ形式がバージョン 2**（上げた後は 0.1.5 以前で開けない）。本番を先に上げ、レプリカは本番の新しいバックアップから作り直す。
- `0.1.0`: 使わないこと。SQL でサーバのファイルを読み書きできた。0.1.1 で直した。

`latest` ではなく、`0.3.0` のようにバージョンを決めて使う。バージョンを上げる前にバックアップ（`CALL heimdb_backup(...)` か mysqldump）を取る。レプリカがあれば、ふつうはレプリカを先に、次に本番を上げる（0.1.6 から 0.2.11、0.2.19、0.2.20、0.2.33、0.2.39、0.2.41 も。0.2.11 はデータ形式がバージョン 3、0.2.19 はバージョン 4、0.2.20 はバージョン 5、0.2.33 はバージョン 6、0.2.39 はバージョン 7、0.2.41 はバージョン 8 になるが、レプリカとやり取りする記録の形は変わらない）。記録の形が変わるとき（0.1.5 以前から 0.1.6 以降）だけは、本番を先に、次にレプリカを上げる（逆にすると、上げたレプリカは古い本番とつながらず、戻すこともできない）。本番とレプリカは記録の形のバージョンを確かめ合い、違えば、レプリカは何も適用せずに止まる。

## すぐに試す

```sh
docker run -d --name heimdb -p 3306:3306 -v heimdb-data:/data \
  -e HEIMDB_ROOT_PASSWORD='secret' -e HEIMDB_SITE_PASSWORD='site-secret' tsukinowa/heimdb:0.3.0

mysql -h 127.0.0.1 -P 3306 -u root -p wordpress
```

HeimDB は root でないユーザー（UID 65532）で動き、公式の MySQL のイメージと違って、起動時に所有者を変えない。名前付きのボリュームはそのまま使える。ホストのディレクトリをマウントするときは、先に `chown -R 65532:65532 <ディレクトリ>` しておく（しないと `open store: open /data/LOCK: permission denied` ですぐ終わる）。

初回の起動で、`HEIMDB_ROOT_PASSWORD` のパスワードで管理者の `root@'%'`（すべての権限）を作る。`HEIMDB_SITE_PASSWORD` があれば、本番のデータベース `wordpress` だけの権限のユーザー `wordpress@'%'` を作る（まだいなければ）。WordPress は root ではなく `wordpress` でつなぐ。root でつなぐと、プラグインの穴から HeimDB の手続き（復元やブランチの削除）、ユーザーの管理、ほかのブランチまで届く。

## WordPress とステージングを一緒に動かす（Docker Compose）

次の内容を `compose.yaml` として保存する。

```yaml
name: heimdb

x-db-host: &db-host
  WORDPRESS_DB_HOST: db:3306
  WORDPRESS_DB_NAME: wordpress

services:
  db:
    image: tsukinowa/heimdb:${HEIMDB_VERSION:-0.3.0}
    environment:
      HEIMDB_ROOT_PASSWORD: ${HEIMDB_ROOT_PASSWORD:?set HEIMDB_ROOT_PASSWORD}  # 初回の起動で root@'%' を作る
      HEIMDB_SITE_PASSWORD: ${HEIMDB_SITE_PASSWORD:?set HEIMDB_SITE_PASSWORD}  # いなければ wordpress@'%' を作る
    volumes:
      - heimdb-data:/data
    # 止めるときに実行中の文を待つ（HeimDB の -shutdown-timeout の既定 30 秒より長く）
    stop_grace_period: 1m

  wordpress:
    image: wordpress:php8.3-apache
    depends_on: [db]
    ports:
      - "${WP_PORT:-8080}:80"
    environment:
      <<: *db-host
      WORDPRESS_DB_USER: wordpress
      WORDPRESS_DB_PASSWORD: ${HEIMDB_SITE_PASSWORD:?set HEIMDB_SITE_PASSWORD}
    volumes:
      - wordpress:/var/www/html

  # ステージングを最初に起動するときに、本番のファイル（言語、プラグイン、テーマ、アップロード）を写す。DB のブランチと
  # 同じく、ファイルも分岐の時点の写しから始める（写さないと、ステージングの管理画面でファイルのないプラグインが無効に
  # なり、テーマが標準のものに替わり、それが本番にマージされてしまう）。2 回目からは写さない（ステージングで変えた
  # ファイルを残す）。
  staging-files:
    image: wordpress:php8.3-apache
    profiles: ["staging"]
    environment:
      STAGING_DB_PASSWORD: ${STAGING_DB_PASSWORD:-}
    # ステージングのパスワードは、ステージングを使うときだけ要る（${...:?} はプロファイルに関係なく効くので、ここで
    # 確かめる）。空かどうかではなく本体があるかで見る（Docker がイメージの空の wp-content を先にボリュームへ写すため）
    entrypoint:
      - sh
      - -c
      - |
        [ -n "$$STAGING_DB_PASSWORD" ] || { echo "set STAGING_DB_PASSWORD" >&2; exit 1; }
        [ -e /to/wp-includes/version.php ] || cp -a /from/. /to/
    volumes:
      - wordpress:/from:ro
      - wordpress-staging:/to
    restart: "no"

  staging:
    image: wordpress:php8.3-apache
    profiles: ["staging"]
    depends_on:
      db:
        condition: service_started
      staging-files:
        condition: service_completed_successfully
    ports:
      - "${STAGING_PORT:-8081}:80"
    environment:
      <<: *db-host
      WORDPRESS_DB_NAME: wordpress@${STAGING_BRANCH:-staging}
      WORDPRESS_DB_USER: staging
      WORDPRESS_DB_PASSWORD: ${STAGING_DB_PASSWORD:-}
      # 本番と同じデータを読むので、URL をこのサイトのものに替える
      WORDPRESS_CONFIG_EXTRA: |
        define('WP_HOME', 'http://localhost:${STAGING_PORT:-8081}');
        define('WP_SITEURL', 'http://localhost:${STAGING_PORT:-8081}');
    # ファイル（プラグイン、テーマ、アップロード）は DB の外にあるので、本番とは別のボリュームに置く（ステージングで
    # プラグインを更新しても本番のファイルは変わらない）。ファイルはマージされない
    volumes:
      - wordpress-staging:/var/www/html

  # wp-cli。管理者の道具なので root でつなぐ（CALL heimdb_* とユーザーの作成のため）
  cli:
    image: wordpress:cli
    profiles: ["cli"]
    depends_on: [db]
    user: "33:33"
    environment:
      <<: *db-host
      WORDPRESS_DB_USER: root
      WORDPRESS_DB_PASSWORD: ${HEIMDB_ROOT_PASSWORD:?set HEIMDB_ROOT_PASSWORD}
    volumes:
      - wordpress:/var/www/html

volumes:
  heimdb-data:
  wordpress:
  wordpress-staging:
```

本番を起動して、WordPress をインストールする（http://localhost:8080）。

```sh
export HEIMDB_ROOT_PASSWORD='secret' HEIMDB_SITE_PASSWORD='site-secret'
docker compose up -d
docker compose run --rm cli wp core install --url=http://localhost:8080 --title=Site \
  --admin_user=admin --admin_password='change-me' --admin_email=you@example.com
```

WordPress は `wordpress` でつなぐ。root は wp-cli（`cli`）だけが使う。

ステージング（http://localhost:8081）は、そのブランチだけの権限のユーザー `staging` でつなぐ（ステージングが乗っ取られても本番に届かない）。ブランチとユーザーを作ってから起動する。`docker compose up` だけではステージングは起動しない（`profiles` で分けてある）。`STAGING_DB_PASSWORD` がないと、ステージングは起動しない。

```sh
export STAGING_DB_PASSWORD='staging-secret'
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose run --rm cli wp db query "CREATE USER 'staging'@'%' IDENTIFIED BY '$STAGING_DB_PASSWORD'; GRANT ALL ON \`wordpress@staging\`.* TO 'staging'@'%'"
docker compose --profile staging up -d staging
```

ステージングで編集した後、差分を見て本番にマージする。

```sh
docker compose run --rm cli wp db query "CALL heimdb_branch_diff('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_merge('staging')"
```

ファイル（言語、プラグイン、テーマ、アップロード）はマージされない。ステージングは本番のファイルの写しから始まる。ステージングでプラグイン、テーマ、言語を入れたり更新したりしたら、マージの前に本番にも同じものを入れる。写さないと、ステージングの管理画面は英語になり、管理画面を開くとファイルのないプラグインが無効に、テーマが標準のものに替わって、それが本番にマージされてしまう。

ステージングを、本番の今の DB とファイルから作り直す（ユーザー `staging` はそのまま使える）。

```sh
docker compose --profile staging rm -sf staging staging-files
docker volume rm heimdb_wordpress-staging
docker compose run --rm cli wp db query "CALL heimdb_branch_delete('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose --profile staging up -d staging
```

## 使い方

HeimDB の操作は、管理者（すべての権限を持つユーザー）が SQL の `CALL` で行う。

### ブランチ

```sql
CALL heimdb_branch_create('staging');                               -- 今の本番から
CALL heimdb_branch_create('test', 'main', '2026-10-08 09:00:00');   -- 過去の時点から
CALL heimdb_branch_list();
CALL heimdb_branch_delete('test');
```

ブランチは `wordpress@ブランチ名` という名前のデータベースに見える。ブランチだけの権限は普通の `GRANT` で与える。

```sql
CREATE USER 'staging'@'%' IDENTIFIED BY 'pw' WITH MAX_USER_CONNECTIONS 20;   -- 同時接続数の上限
GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%';
CALL heimdb_branch_limit('staging', '2G', '30s');   -- ブランチの大きさと、1 つの文の実行時間の上限
```

### 差分とマージ

```sql
CALL heimdb_branch_diff('staging');               -- テーブルごとの要約
CALL heimdb_branch_diff('staging', 'wp_posts');   -- 行ごと（'*' ならすべてのテーブル）
CALL heimdb_branch_diff('staging', 'wp_posts', 0, 100);   -- 行ごとの差分を 100 行ずつ
CALL heimdb_branch_merge('staging', 'dry_run');   -- 書かずに、マージできるかと件数を確かめる
CALL heimdb_branch_merge('staging');              -- 親に 1 つのコミットで反映する
```

行は主キーで対応づけ（`wp_options` は `option_name`）、分岐の時点と比べて列ごとに判定する。ブランチだけが変えた列は反映し、本番だけが変えた列は残す。環境ごとのオプション（`siteurl`、`home`、transient、cron など）、自動下書き、リビジョン、コメント、ユーザーはマージしない。プラグインがしたテーブルとインデックスの変更（dbDelta）はマージする。マージが何かを書けば、結果の最後の行に、マージを戻す `heimdb_restore` の文が出る。

両側が同じ列を変えた行は競合で、どちらを採るかを決めるまでマージは拒否される。

```sql
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=42', 'branch');  -- ステージングの値を採る
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=42', 'parent');  -- 本番の値を残す
CALL heimdb_branch_resolve('staging', 'wp_posts', '*', 'parent');      -- テーブルの競合をすべて
```

行は、差分の `id` の列の形のまま書く（`'ID=42'`）。

マージの後にプラグインが作り直す表（Yoast SEO の索引など）は、マージから外せる。本番の表はそのまま残る。

```sql
CALL heimdb_merge_exclude('wp_yoast_%');
```

### 本番の変更をブランチに取り込む

`staging` の検証中にほかのブランチが本番にマージされたら、本番の最新を `staging` に取り込んで確かめ直せる。

```sql
CALL heimdb_branch_sync('staging', 'dry_run');  -- 書かずに、件数と競合を確かめる
CALL heimdb_branch_sync('staging');             -- 両側で同じ列を変えていれば拒否（51004）
CALL heimdb_branch_sync('staging', 'branch');   -- 競合した列はステージングの値を残す
CALL heimdb_branch_sync('staging', 'parent');   -- 競合した列は本番の値を採る
```

本番だけが変えた列を 1 回のコミットでブランチに書き、ステージングの変更は残す。コメントとユーザーも写し、環境ごとのオプションとマージから外した表は写さない。本番はロックしない。両側で定義の違う表があれば拒否する（51010）。結果の最後に、取り込みを戻す `heimdb_restore` の文を出す。

ブロックテーマは、トップページを最初に表示したときにナビゲーション（`wp_navigation` の投稿）を作る。ブランチを作った後に本番とステージングの両方で初めて表示すると、2 つのナビゲーションが競合する。`'parent'` で本番の方を残す。

### タイムトラベルと復元

```sql
SET time_zone = '+09:00';   -- 時刻はセッションの time_zone で読む

-- 過去を読む
SELECT ID, post_title FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
-- 消した記事を 1 件だけ戻す
INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;

-- ある時刻から後に変わった行を、変わった順に見る
CALL heimdb_changes('main', '2026-10-08 09:00:00');
CALL heimdb_changes('main', '2026-10-08 09:00:00', 'wp_users,wp_usermeta');  -- このテーブルだけ

-- サイト全体を戻す（コメントとユーザーは今のまま残す）
CALL heimdb_restore_preview('main', '2026-10-08 09:00:00');
CALL heimdb_restore('main', '2026-10-08 09:00:00');
CALL heimdb_restore('main', '2026-10-08 09:00:00', 'wp_options,wp_posts');  -- このテーブルだけ
```

復元は古いバージョンを新しいコミットとして書くので、履歴は消えない。結果の最後の行に、復元を取り消す `CALL` が出る。古いバージョンは既定で 30 日残る（`-retention`）。

`heimdb_changes` は、変わった行ごとに、行の名前、変化（`insert`、`update`、`delete`）、変わった列、最後に変わった時刻、見張りの対象ならその説明を返す。毎分のように変わる行（一時データ、cron、編集ロック）は出さず、その数を最後の行に出す。

テーブルを省いた復元はユーザーを戻さない。復元の時刻の後に、ユーザーの追加、パスワードやメールアドレスの変更、administrator の役割の付与があれば、結果（とプレビュー）に `not restored` の行が出て、ログにも警告が出る（攻撃者が足した管理者は、そのままでは残るため）。ユーザーも戻すなら `'*'` で復元する。

### 状態、バックアップ、GC

```sql
CALL heimdb_status();                  -- ディスク使用量、GC、接続数、開いたトランザクション、レプリケーション、変更の見張り
CALL heimdb_backup('before-upgrade');  -- 動いたまま、整合したバックアップを -backup-dir に作る
CALL heimdb_gc();                      -- 保持期間より古いバージョンを今すぐ消す
```

### 危ない変更の見張り

WordPress が乗っ取られるときに変わるものがコミットで変わると、HeimDB はログに警告を出す。止めはしない。

```
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: option default_role: \"subscriber\" -> \"administrator\""
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: administrator role given: user ID 7 (wp_capabilities)"
```

- 見張るもの: オプションの `siteurl`、`home`、`admin_email`、`users_can_register`、`default_role`、`template`、`stylesheet`、`active_plugins` と役割の定義。ユーザーの追加と削除、パスワードとメールアドレスの変更（パスワードのハッシュはログに出さない）。ユーザーに administrator の役割が付いたこと。マルチサイトの `site_admins` と、ネットワークでのプラグインの有効化。
- 各行には、変えたユーザーと接続元のアドレス、データベース（ブランチなら `wordpress@staging`）が出る。ブランチからマージで入った変更には `(merge of branch staging)` が付く。コミットした変更だけを出す。復元とレプリカでは出さない。
- `CALL heimdb_status()` で、起動からの件数（`watch_changes`）と最後の 1 件（`watch_last_change`、`watch_last_change_at`）も見られる。
- 普段の運用でも出る: WordPress のインストール、ユーザーの登録、WordPress 6.8 以降に上げた後の各ユーザーの最初のログイン（パスワードのハッシュの作り直し）、`wp search-replace` や `wp db import`。
- 心当たりのない行があれば、その少し前からの変化を `heimdb_changes` で見て、`heimdb_restore_preview` で確かめてから `heimdb_restore` で戻す（タイムトラベルの節）。データベースの外のファイル（プラグイン、アップロード）は戻らない。

## 設定

| | |
| --- | --- |
| ポート | 3306 |
| データ | `/data`（ボリュームを使う） |
| ユーザー | nonroot（UID 65532） |
| `HEIMDB_ROOT_PASSWORD` | `root@'%'` のパスワード。初回の起動のときだけ使う（`-reset-passwords`、`-reset-users` を付ければ上書きする） |
| `HEIMDB_SITE_PASSWORD` | WordPress がつなぐユーザー（`-site-user`、既定 `wordpress`）のパスワード。まだいなければ、`-db` のデータベースだけの権限で作る |
| `HEIMDB_RESET_PASSWORDS`、`HEIMDB_RESET_USERS` | `1` で `-reset-passwords`、`-reset-users`（下） |

イメージ名の後ろに足した引数は、そのまま HeimDB に渡る。

```sh
docker run -d -p 3306:3306 -v heimdb-data:/data -v heimdb-backups:/backups \
  -e HEIMDB_ROOT_PASSWORD='secret' tsukinowa/heimdb:0.3.0 \
  -backup-dir /backups -slow-query 1s
```

| フラグ | 既定 | 意味 |
| --- | --- | --- |
| `-db` | `wordpress` | 本番（`main` ブランチ）のデータベース名 |
| `-site-user` | `wordpress` | `HEIMDB_SITE_PASSWORD` で作るユーザーの名前 |
| `-retention` | `720h` | `AS OF` と復元のために古いバージョンを残す期間（0 なら消さない） |
| `-gc-interval` | `1h` | 古いバージョンを消す間隔 |
| `-backup-dir` | | `CALL heimdb_backup()` の書き出し先（空なら無効） |
| `-backup-key` | | `名前=パス`。`CALL heimdb_backup('<退避の名前>', '<名前>')` で、パスのファイルの [age](https://age-encryption.org/) の受取人（`age1...`、1 行に 1 つ）に暗号化した 1 つのファイル `<退避の名前>.tar.gz.age` を書き出す。ファイルは退避のたびに読み、受取人をログに出す（ファイルが変わっていれば警告）。繰り返して複数の鍵。0.2.28 から |
| `-reset-passwords` | `false` | 起動時に、`root@'%'` と、いる `-site-user` のパスワードを `HEIMDB_ROOT_PASSWORD` と `HEIMDB_SITE_PASSWORD` にする（ほかのユーザーは残す。退避から復元した store を開くとき）。0.2.28 から |
| `-reset-users` | `false` | 起動時に、`root@'%'`（`HEIMDB_SITE_PASSWORD` があれば `-site-user` も。権限は残す）のほかのユーザーをすべて消し、そのパスワードを上書きする（いなければ作る）。0.2.28 から |
| `-slow-query` | | この時間を超えた文をログに出す（起動時の値。管理者が `SET GLOBAL long_query_time` と `slow_query_log` で止めずに変えられる） |
| `-slow-query-redact` | `false` | 遅い文のログの文の値を `?` に畳む |
| `-idle-lock-timeout` | `30s` | トランザクションの中で何もしないまま、書き込みのロックをこれより長く持つ接続を切る（0 なら切らない） |
| `-shutdown-timeout` | `30s` | 止めるときに実行中の文を待つ上限 |
| `-branch-procedures` | `false` | ブランチだけの権限のユーザーが、`GRANT EXECUTE` で接続中のブランチの差分、解決、変更の一覧、復元を呼べる（ブランチには `''` を渡す。`HEIMDB_BRANCH_PROCEDURES=1` でも指定できる） |
| `-memory-limit` | コンテナの上限の 8 割 | メモリの上限（`4G` など。`HEIMDB_MEMORY_LIMIT` でも指定できる。`0` なら上限なし）。近づくと、文がエラー 1041 で止まり、そのトランザクションが取り消される（プロセスは落ちない） |
| `-tls-cert`、`-tls-key` | | このファイルで TLS を有効にする（UID 65532 が読めるようにマウントする） |
| `-metrics-addr` | | Prometheus の形式の指標を、このアドレスの `/metrics` で返す（`0.0.0.0:9104` など。認証がないので、ポートを公開せず、同じ Docker のネットワークから読む）。0.2.21 から |

読み取り専用のレプリカと、残りのフラグの説明はソースのリポジトリにある（まだ公開していない）。

- 止めるときは `docker stop`（SIGTERM）。`-shutdown-timeout` より長い時間を与える（`docker stop -t 60`、Compose なら `stop_grace_period`）。
- バックアップから戻すときは、バックアップのディレクトリをデータのディレクトリにして HeimDB を起動する。
- 暗号化した退避は、空のデータのボリュームに展開してから、いつもどおり起動する（本番のパスワードを知らない環境では `-reset-passwords` か `-reset-users` を付ける）。鍵が違うときやファイルが壊れているときは、何も残さずに失敗する。

```sh
docker run --rm -v verify-data:/data -v /path/to/backups:/backups:ro -v /path/to/verify.key:/run/secrets/verify.key:ro \
  tsukinowa/heimdb:<バージョン> restore-backup -in /backups/<退避の名前>.tar.gz.age -identity /run/secrets/verify.key
```

## ライセンス

ソース: [github.com/tsukinowasha/heimdb](https://github.com/tsukinowasha/heimdb)。不具合と MySQL との違いはそこに報告する（脆弱性はリポジトリの SECURITY.md の手順で非公開で報告する）。

Apache License 2.0。Copyright 2026 Tsukinowa Inc. 依存のライセンスの表示は、イメージの `/licenses` にある。
