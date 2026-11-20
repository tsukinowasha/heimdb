# 運用

起動のフラグ、Docker、レプリカ、通信の暗号化（TLS）、指標（Prometheus）の詳しい説明。基本の使い方は [README](../README.md)、`heimdb_*` の手続きは [`procedures.md`](procedures.md)。

## 起動のフラグ

| フラグ | 既定 | 意味 |
|---|---|---|
| `-addr` | `0.0.0.0:3306` | 待ち受けるアドレス |
| `-data` | `data` | データのディレクトリ |
| `-db` | `wordpress` | 本番（main ブランチ）のデータベース名 |
| `-root-password` | | 初回に作る `root@'%'` のパスワード |
| `-site-user` | `wordpress` | WordPress がつなぐユーザーの名前 |
| `-site-password` | | WordPress がつなぐユーザーのパスワード（または環境変数 `HEIMDB_SITE_PASSWORD`）。あれば、そのユーザーを `-db` のデータベースだけの権限で作る（まだいなければ。いれば変えない） |
| `-retention` | `720h`（30 日） | `AS OF` と復元のために古いバージョンを残す期間（0 なら消さない） |
| `-gc-interval` | `1h` | 古いバージョンを消す間隔 |
| `-backup-dir` | | `CALL heimdb_backup()` の書き出し先（空なら無効） |
| `-backup-key` | | `名前=パス`。`CALL heimdb_backup('<退避の名前>', '<名前>')` で、パスのファイルの age の受取人（`age1...`、1 行に 1 つ）に暗号化した退避を書き出す。ファイルは退避のたびに読む（繰り返して複数の鍵。`-backup-dir` が要る。下の「暗号化した退避」） |
| `-reset-passwords` | `false` | 起動時に、`root@'%'` のパスワードを `-root-password` に、いる `-site-user` のパスワードを `-site-password` にする（ほかのユーザーは残す。または環境変数 `HEIMDB_RESET_PASSWORDS=1`。退避から復元した store を開くとき。下の「暗号化した退避」） |
| `-reset-users` | `false` | 起動時に、`root@'%'` と、`-site-password` があればサイトのユーザー（権限は残してパスワードを上書きし、いなければ作る）のほかのユーザーをすべて消し、`root@'%'` のパスワードを上書きする（または環境変数 `HEIMDB_RESET_USERS=1`） |
| `-shutdown-timeout` | `30s` | SIGTERM で実行中の文を待つ上限（過ぎたらデータを閉じずに終える） |
| `-slow-query` | `0` | この時間を超えた文をログに出す（0 なら出さない）。起動時の値で、止めずに `SET GLOBAL long_query_time` と `slow_query_log` で変えられる（下の「遅い文のログ」） |
| `-slow-query-redact` | `false` | 遅い文のログの文の値を `?` に畳む |
| `-cache-size` | 上限の 8 分の 1（16〜128 MiB） | ブロックキャッシュの大きさ（`innodb_buffer_pool_size` に相当。バイト）。指定がなければ、メモリの上限の 8 分の 1 を 16 MiB から 128 MiB の間に収めたもの（上限がなければ 128 MiB） |
| `-memory-limit` | コンテナの上限の 8 割 | プロセスのメモリの上限（`4G`、`512M` など。または環境変数 `HEIMDB_MEMORY_LIMIT`）。近づくと、文がエラー 1041 で止まり、そのトランザクションが取り消される（プロセスは落ちない）。指定がなければ、コンテナのメモリの上限（cgroup）の 8 割。`0` なら上限なし（下の「メモリの上限」） |
| `-lock-wait-timeout` | | 書き込みのロックを待つ時間（`innodb_lock_wait_timeout` に相当） |
| `-idle-lock-timeout` | `30s` | トランザクションの中で何もしないまま、書き込みのロックをこれより長く持つ接続を切る（0 なら切らない。下の「書き込みが詰まったとき」） |
| `-tls-cert`、`-tls-key` | | TLS の証明書と鍵（PEM）。MySQL の接続で TLS を受け付け、レプリカへの送り出しを HTTPS にする |
| `-require-tls` | `false` | TLS でない MySQL の接続を拒否する（`-tls-cert` が要る） |
| `-replication-addr` | | レプリカに記録を送る HTTP（`-tls-cert` があれば HTTPS）のアドレス（空なら送らない） |
| `-replication-token` | | 本番とレプリカの合言葉（または環境変数 `HEIMDB_REPLICATION_TOKEN`） |
| `-replication-log-retention` | `24h` | レプリカのためにコミットの記録を残す期間（0 なら残さない） |
| `-replication-sync-timeout` | `10s` | コミットを返す前に、レプリカが適用したという知らせを待つ上限（過ぎたら非同期に切り替える。0 なら待たない） |
| `-replicate-from` | | この本番（`host:port`、`https://host:port`）の読み取り専用のレプリカとして動く |
| `-replication-ca` | | 本番の証明書を確かめる CA（PEM。指定すると `-replicate-from` は https になる。なければ OS が信頼するもの） |
| `-replica-name` | ホスト名 | 本番の `heimdb_status` に出すレプリカの名前 |
| `-metrics-addr` | | Prometheus の形式の指標を `http://<アドレス>/metrics` で返す（空なら開かない。認証がないので外に開けない。下の「指標」） |
| `-debug` | `false` | すべての文をログに出す |
| `-no-auth` | `false` | 認証なしで誰でも接続できる（開発用） |
| `-rebuild-indexes` | `true` | 起動の後に、本番（main）の、古い HeimDB（0.2.38 以前、`SET` と `BIT` は 0.2.40 以前）で作ったエントリのないインデックスを、裏で 1 つずつ作り直す（または環境変数 `HEIMDB_REBUILD_INDEXES`。フラグを書いたときは、フラグが優先する）。止めるのは `-rebuild-indexes=false`。レプリカでは動かない。下の「古いインデックスの作り直し」 |
| `-branch-procedures` | `false` | ブランチだけの権限のユーザーが、自分のブランチの差分、解決、変更の一覧、復元を呼べる（または環境変数 `HEIMDB_BRANCH_PROCEDURES=1`。フラグを書いたときは、フラグが優先する）。差分は親で変わった値も返すので、ブランチをテナントの分離に使うなら付けない（[`procedures.md`](procedures.md) の「ブランチのユーザーが呼べる手続き」） |

- 初回の起動で、`-root-password`（または環境変数 `HEIMDB_ROOT_PASSWORD`）のパスワードで `root@'%'`（すべての権限）を作る。ユーザーがいなくてどちらも指定しなければ起動しない。
- HeimDB は UTC で動く（RDS などの MySQL の既定と同じ）。
- 止めるときは SIGTERM。実行中の文が終わるのを `-shutdown-timeout` まで待ってから、データを閉じる。

## Docker

イメージは Docker Hub の [`tsukinowa/heimdb`](https://hub.docker.com/r/tsukinowa/heimdb)（linux/amd64、linux/arm64）。データは `/data`、ポートは 3306 で、`docker run` の後ろに足したフラグはそのまま HeimDB に渡る。依存のライセンスの表示はイメージの `/licenses` にある。

```sh
docker run -d --name heimdb -p 3306:3306 -v heimdb-data:/data \
  -e HEIMDB_ROOT_PASSWORD='secret' -e HEIMDB_SITE_PASSWORD='site-secret' tsukinowa/heimdb:0.2.14 -slow-query 1s
```

HeimDB は nonroot（UID 65532）で動き、起動時に所有者を変えない（公式の MySQL のイメージと違う）。名前付きのボリュームはそのまま使えるが、ホストのディレクトリをマウントするときは、先に `chown -R 65532:65532 <ディレクトリ>` しておく（しないと `open store: open /data/LOCK: permission denied` ですぐ終わる）。`-backup-dir` も同じ。

### WordPress と一緒に動かす

リポジトリの直下の [`compose.yaml`](../compose.yaml) で、HeimDB と WordPress をまとめて起動できる。HeimDB のポートはホストに開けない（WordPress と wp-cli のコンテナからだけ使う）。

```sh
export HEIMDB_ROOT_PASSWORD='secret' HEIMDB_SITE_PASSWORD='site-secret'  # または .env に書く。ないと起動しない
docker compose up -d                       # WordPress は http://localhost:8080（WP_PORT で変えられる）
docker compose run --rm cli wp core install --url=http://localhost:8080 --title=Site \
  --admin_user=admin --admin_password='...' --admin_email=you@example.com
```

WordPress は、本番のデータベースだけの権限のユーザー `wordpress` でつなぐ（HeimDB が `HEIMDB_SITE_PASSWORD` で作る）。root は wp-cli（`cli`）だけが使う（`CALL heimdb_*` と、ユーザーの作成のため）。

ステージング（ブランチ `wordpress@staging` につなぐ 2 つ目の WordPress。http://localhost:8081、`STAGING_PORT` で変えられる）は、そのブランチだけの権限のユーザー `staging` でつなぐ（ステージングが乗っ取られても本番に届かない）。ブランチとユーザーを作ってから起動する。`STAGING_DB_PASSWORD` がないと、ステージングは起動しない。

```sh
export STAGING_DB_PASSWORD='staging-secret'
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose run --rm cli wp db query "CREATE USER 'staging'@'%' IDENTIFIED BY '$STAGING_DB_PASSWORD'; GRANT ALL ON \`wordpress@staging\`.* TO 'staging'@'%'"
docker compose --profile staging up -d staging
# ステージングで編集した後、差分を見て本番にマージする
docker compose run --rm cli wp db query "CALL heimdb_branch_diff('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_merge('staging')"
```

- ステージングの URL は `WP_HOME` と `WP_SITEURL` で切り替えている。
- ファイル（言語、プラグイン、テーマ、アップロード）は DB の外にあるので、ステージングは本番と別のボリュームを使う（ステージングでプラグインを更新しても本番のファイルは変わらない）。ステージングを最初に起動するとき、`staging-files` が本番のファイルをそのボリュームに写す。DB のブランチと同じく、ファイルも分岐の時点の写しから始まる（写さないと、ステージングは英語になり、管理画面を開くとファイルのないプラグインが無効に、テーマが標準のものに替わって、それが本番にマージされる）。
- **ファイルはマージされない**。ステージングで入れたり更新したりしたプラグイン、テーマ、言語は、マージの前に本番にも同じものを入れる（`wp plugin install` など）。ステージングを作った後に本番で入れたものは、ステージングには入らない（作り直す）。

ステージングを作り直す（本番の今の DB とファイルから始め直す。ユーザー `staging` はそのまま使える）:

```sh
docker compose --profile staging rm -sf staging staging-files
docker volume rm heimdb_wordpress-staging
docker compose run --rm cli wp db query "CALL heimdb_branch_delete('staging')"
docker compose run --rm cli wp db query "CALL heimdb_branch_create('staging')"
docker compose --profile staging up -d staging
```

- ブロックテーマは、トップページを最初に表示したときにナビゲーション（`wp_navigation` の投稿）を作る。ブランチを切った後に本番とステージングの両方で初めて表示すると、両側に同じ名前のナビゲーションができ、マージが競合で止まる。`heimdb_branch_diff('staging', 'wp_posts')` で競合の行を見て、`CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=…', 'parent')`（本番の方を残す）で解決してからマージする（解決の指定は [`procedures.md`](procedures.md) の「heimdb_branch_resolve」）。

### 運用の注意

- **バージョンを決めて使う**。`latest` ではなく `0.2.14` のようなバージョンを指定し、上げるときは下の「HeimDB のバージョンを上げる」の手順による。
- 止めるときは `docker stop`（SIGTERM）。実行中の文を `-shutdown-timeout`（既定 30 秒）まで待つので、`docker stop -t` や `stop_grace_period` はそれより長くする（短いと強制終了になる）。
- `CALL heimdb_backup()` を使うなら、書き出し先をボリュームにして `-backup-dir` で渡す（例: `-v heimdb-backups:/backups` と `-backup-dir /backups`）。
- `heimdb_backup` のバックアップは、データベースを丸ごと写すので、取った時点で GC が消していない古いバージョン（既定の `-retention` なら約 30 日分）、ブランチ、ユーザーと権限を含む。そのディレクトリで起動すれば、`AS OF`、`heimdb_restore`、過去の時点からのブランチの作成が、取った時と同じ範囲で使える（mysqldump のバックアップは今の行だけで、履歴を持たない）。ただし GC は今の時刻から `-retention` を数えるので、`-retention` より古いバックアップを起動すると、最初の GC（`-gc-interval`、既定 1 時間）で最後の状態より前のバージョンが消える（最後の状態は残る）。古いバックアップの履歴を見るなら、`-retention 0`（消さない）で起動する。
- 退避を別の場所（ほかのプロジェクト、検証の環境）へ写すなら、鍵を指定して暗号化する（下の「暗号化した退避」）。平文の退避は、表の行も WAL（ログ）の直近の書き込みも、そのまま読める。
- **表の手入れは要らない**（0.2.22 から受け付ける）。`OPTIMIZE TABLE`、`CHECK TABLE`、`REPAIR TABLE`、`ANALYZE TABLE` は、何もせずに `OK`（`REPAIR` は InnoDB と同じ「対応しない」の `note`）を返す。古いバージョンは GC が消し、ディスクは Pebble が自分で詰め、壊れは Pebble が読むときにチェックサムで確かめる。WP-Optimize や Advanced Database Cleaner の最適化、`wp-admin/maint/repair.php`、`wp db optimize` はエラーにならないが、何も変わらない。wp-cli の `wp db check` / `optimize` / `repair` は MariaDB の `mariadb-check` を使い、既定で TLS を求めるので、HeimDB を `-tls-cert` なしで動かしているなら `--skip-ssl` を付ける（`wp db check --skip-ssl`）。
- ログに `HeimDB: recovered from a panic in a statement`（スタック付き）が出たら、HeimDB の不具合。その文はエラー 1105（`HeimDB: internal error: ...`）で失敗し、その文の書き込みは取り消され、サーバはそのまま動く。接続も続くが、結果の行を送り始めた後（多くの行を返す `SELECT` の途中）なら、ほかのエラーと同じく接続が切れる。取り消せないもの: 複数の操作の `ALTER TABLE` で先に終わった操作、自動コミットの文がコミットを終えた後の panic。スタックと、そのころの文（スロークエリのログなど）を添えて知らせる。
- **メモリの目安**（GCE の n2-standard-4、ホスティングの `wp_postmeta` と同じ形の 310 万行、0.2.11。`docs/bench.md` の「段階 58」と「段階 59」）。常駐（開いて全件を読んだ後の RSS）は約 500 MiB で、うち生きているヒープが約 200 MiB（ブロックキャッシュ 128 MiB と、照合順序の表など約 40 MiB を含む）。常駐は表の行数にほぼよらない。行数とともに増えるのは次のもの（最大の RSS）。上限があれば、超える前にエラー 1041 で止まる。

  | 文 | 310 万行、上限なし | 上限 3,276 MiB |
  |---|---|---|
  | `ALTER TABLE ... ADD COLUMN`（0.2.8 から行数によらない） | 621 MiB | 通る |
  | インデックスを使えない `ORDER BY ... LIMIT` | 976 MiB | 通る |
  | 値がすべて違う列の `GROUP BY` | 1,755 MiB | 通る |
  | 表全体の `UPDATE` | 2,625 MiB | 通る |
  | 表全体の `DELETE` | 4,640 MiB | 1041 |
  | 1 つのトランザクションでの 310 万行の取り込み（`mariadb-dump` の既定） | 5,643 MiB | 1041（`COMMIT` で） |
  | 全行を変えたブランチのマージ | 4,439 MiB（55 秒） | 1041 |
  | 列を足したブランチのマージ | 5,477 MiB（81 秒） | 1041 |
  | 全行を変えた後の復元 | 10,540 MiB（154 秒） | 1041 |

  0.2.10 までは、`UPDATE` が 4,042 MiB、`DELETE` が 9,344 MiB、取り込みが 11,192 MiB、マージが 7,871〜11,869 MiB（列を足したものは 881 秒）、復元が 12 GiB を超えた。0.2.7 までは `ALTER TABLE` と `CREATE INDEX` も行数に比例し、311 万行の `ADD COLUMN` で 7.8 GiB を使った。0.2.8 までは読み出し（インデックスを使えない `SELECT`、`COUNT(*)`、範囲の検索）も比例した。

  書き込みの多い文が増えるのは、コミットしていない書き込みと、コミットの間の写しのため。コミットしていない書き込みは、`wp_postmeta` の形で 1 行あたり約 0.5 KB（行とインデックスのエントリ。生きているヒープ）。コミットの間は、その上にコミットのバッチ（レプリカに送る記録を含めて中身の 2 倍）と、Pebble の書き出しの記録（WAL）の作業の分を一時的に使い、合わせてコミットしていない書き込みの約 4 倍になる。上限 3,276 MiB では、`wp_postmeta` の形で 1 つのトランザクションに入るのは約 150 万行まで（GCE で、150 万行は入り、200 万行は `COMMIT` で 1041 になった）。マージと復元は自分のトランザクションに書くので同じく増え、上限 L の中で 1 回に変えられる量は、書き込みの大きさで L の 3 分の 1 程度（上限 3,276 MiB なら、310 万行の全行を戻す復元は 1041 になる。表や時刻を分けて戻す）。止まれば何も変えない。表を指定した復元で組の表の片方だけを戻すときは、戻さない表の変わった行の主キーも読む（`docs/procedures.md` の `heimdb_restore`）。
- **上限を付けて動かす**。上限がない（`-memory-limit 0`）と、Go の GC は生きているヒープの 2 倍までメモリを使ってから回収するので、大きな文の後に RSS が生きているヒープの 2 倍近くまで伸びる（310 万行の復元で、生きているヒープ 6.7 GiB の後に cgroup の 12 GiB を超えて落ちた）。上限があれば、GC は上限に近づく前に回り、文は 1041 で止まる。
- **メモリの上限**（0.2.9 から）。上の残りでメモリを使い切ってプロセスが落ちないよう、`-memory-limit` に近づいたら、そのとき行を読み書きしている文をエラー 1041（`HeimDB: out of memory: ...`）で止め、そのトランザクションを取り消す（明示的なトランザクションも、文だけでなくトランザクションごと取り消す）。止めるのは、生きているヒープが上限の 75% を超えたとき。メモリはプロセス全体で数えるので、止まる文が多く使っている当の文とは限らない（同時に動いている別の文が止まることもある。大きな変更を持ったまま何もしていないトランザクションがあると、それが終わるまでほかの文が止まり続ける）。プロセスの使うメモリ（RSS）は、ヒープの外の分とバッチが一度に伸びる分だけ、上限を 1〜2 割超えることがある。上限を指定しなければコンテナの上限の 8 割を使うのはこのため。コンテナの上限は自分の cgroup の `memory.max` だけを読む（入れ子で親だけに上限があるときは上限なしになるので、`-memory-limit` を指定する）。コンテナの外で動かすなら、`-memory-limit` を指定する（1 つの VM に複数のサイトを置くなら、合計が VM のメモリに収まるように）。
- **上限に数える常駐の分**（0.2.11 から）。生きているヒープには、起動直後の分（約 40 MiB。大半は照合順序の表）と、ブロックキャッシュ（公式のイメージは cgo なしで建てるので、Pebble はキャッシュを Go のヒープに取る）が入る。キャッシュは読めば満ち、GC でも減らない。0.2.10 までは、上限の 75% がこの 2 つより小さいと、キャッシュが満ちた後はどの文も 1041 になり、起動し直しても直らなかった（既定のキャッシュ 128 MiB で、上限 256 MiB 以下）。0.2.11 から、`-cache-size` を指定しなければキャッシュを上限の 8 分の 1 にし、起動直後の分 + キャッシュ + 文の余裕（32 MiB）が上限の 75% を超える設定では、要る上限をエラーに出して起動しない。表の行数には、常駐はほぼよらない。監視は `CALL heimdb_status()` の `memory_live_bytes` と `memory_abort_bytes` を比べる（`heimdb_status()` は行を読まないので、文が止まる状態でも通る）。
- **ダンプの取り込み**。HeimDB はコミットしていない書き込みをメモリに持つ（`wp_postmeta` の形で 1 行あたり約 0.5 KB、コミットの間はその約 4 倍）ので、大きな表を 1 つのトランザクションで入れると、メモリが行数に比例する。`mariadb-dump` は既定で表ごとに `SET autocommit=0` と `COMMIT` で囲む（`--no-autocommit` が既定で有効）ので、`--skip-no-autocommit` を付けて取る（ホスティング側の報告: 311 万行の `wp_postmeta` が、0.2.9 の既定では 7.7 GiB で落ち、付けると 193 秒で入った。0.2.11 では既定のままで 5.6 GiB、上限 3,276 MiB なら `COMMIT` で 1041）。MySQL の `mysqldump` は既定では囲まない（`--no-autocommit` を付けない）。上限があれば、落ちずにエラー 1041 で止まる。
- **1 つのトランザクションの大きさの上限**（0.2.11 から）。コミットしていない書き込みが 1,536 MiB を超えると、その文をエラー 1197（`HeimDB: the transaction writes more than 1536 MiB ...`）で取り消す（トランザクションのそれまでの書き込みは残り、`COMMIT` か `ROLLBACK` を選べる）。Pebble は 4 GiB を超えるバッチを書けず、コミットのバッチは中身の 2 倍になるため。マージと復元の上限は 1,900 MiB で、超えると 1197 で何も変えずに止まる（復元は表を分けて戻す）。メモリの上限が小さければ、この大きさより前に 1041 で止まる。
- TLS の証明書や鍵は、ファイルをマウントして `-tls-cert`、`-tls-key` で渡す。プロセスは nonroot（UID 65532）で動くので、そのユーザーが読める権限にする。
- イメージを作り直すときは、リポジトリの直下で `docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=0.2.14 -t tsukinowa/heimdb:0.2.14 --push .`。

## HeimDB のバージョンを上げる

バージョンの付け方: ソースを公開した 0.3.0（コードは 0.2.45 と同じ）から、データ形式を変えて古いバージョンでは開けなくなるとき（後方互換をなくすとき）にマイナーバージョンを上げ（0.3.x から 0.4.0）、ほかはパッチのバージョンを上げる。データ形式を変えたバージョンも、古いデータは起動時に移す。

データ形式は 0.2.41 からバージョン 8（FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の型と、SET と BIT の列のインデックスのエントリのため）、0.2.39 と 0.2.40 はバージョン 7（DECIMAL、FLOAT、DOUBLE、ENUM の列のインデックスのエントリのため）、0.2.33 から 0.2.38 はバージョン 6（親の変更をブランチに取り込んだ記録のため）、0.2.20 から 0.2.32 はバージョン 5（主キーのない表のため）、0.2.19 はバージョン 4（文字列、バイト列、日付、ENUM を含む主キーのため）、0.2.11 から 0.2.18 はバージョン 3（大きなコミットの記録を 1 MiB ずつに分けて書くため）、0.1.6 から 0.2.10 はバージョン 2（1 列の文字列の主キーのため）、0.1.0 から 0.1.5 はバージョン 1。新しいバージョンは古いバージョンのデータをそのまま読み、起動時にバージョンの番号だけを書き換える（変換はないので、すぐ終わる）。古い HeimDB は、新しいバージョンのデータの起動を拒否する（読み違えない）。形式を変えるときは、新しい HeimDB が起動時に古いバージョンのデータを移し、リリースの説明にそう書く。

レプリカがないとき:

1. `CALL heimdb_backup('before-upgrade')` を取る（`-backup-dir` の下）。
2. HeimDB を止め（SIGTERM。`docker stop`）、新しいバージョンで同じ `-data` を指定して起動する。
3. 戻すときは、古いバージョンで、手順 1 のバックアップのディレクトリを `-data` にして起動する（データ形式が変わるバージョンでは、上げた後のデータを古いバージョンは開けない）。

同期のレプリカがあるとき:

- データ形式が同じバージョンどうし（普通の更新）: レプリカを止めて新しいバージョンで起動し、追いつく（`replication_behind` が 0s、本番の `replication_sync` が `sync`）のを待ってから、本番を同じように上げる。本番が止まっている間、WordPress は書き込めない（本番を上げずにレプリカへ切り替えるなら、「レプリカ」の昇格の手順による）。 0.2.7 以前のレプリカに 0.2.8 以降の本番をつながない（0.2.8 から、大きな表の `ALTER TABLE` と `CREATE INDEX` を何回かに分けてコミットし、途中のデータを GC から守る印を付ける。古いレプリカは印を知らず、途中で自分の GC が書きかけを消すと、本番と食い違う）。
- 0.2.11 はデータ形式のバージョン 3 だが、レプリカに運ぶ記録の形（本番とレプリカが確かめ合うバージョン）はバージョン 2 のまま変わらない。0.2.10 以前とつながるので、普通の更新と同じく、レプリカを先に上げる。上げた後のレプリカのデータはバージョン 3 なので、0.2.10 以前に戻すときは本番のバックアップから作り直す。
- 0.2.19 はデータ形式のバージョン 4 だが、0.2.11 と同じく、レプリカに運ぶ記録の形はバージョン 2 のまま変わらない。レプリカを先に上げる。上げた後のレプリカのデータはバージョン 4 なので、0.2.18 以前に戻すときは本番のバックアップから作り直す。0.2.19 の本番は、0.2.18 以前のレプリカを記録の水準（下の項目）で拒否する（0.2.18 以前のレプリカは、0.2.19 から作れるようになった主キーの表（WooCommerce の `wc_product_attributes_lookup`、Wordfence の `wfls_role_counts` など）を開けない）。
- 0.2.20 はデータ形式のバージョン 5 だが、0.2.19 と同じく、レプリカに運ぶ記録の形はバージョン 2 のまま変わらない。レプリカを先に上げる。上げた後のレプリカのデータはバージョン 5 なので、0.2.19 以前に戻すときは本番のバックアップから作り直す。0.2.20 の本番は、0.2.19 以前のレプリカを記録の水準（下の項目）で拒否する（0.2.19 以前のレプリカは、0.2.20 から作れるようになった主キーのない表（WP Cerber、Loginizer のログの表など）を開けない）。
- 0.2.33 はデータ形式のバージョン 6 だが、0.2.20 と同じく、レプリカに運ぶ記録の形はバージョン 2 のまま変わらない。レプリカを先に上げる。上げた後のレプリカのデータはバージョン 6 なので、0.2.32 以前に戻すときは本番のバックアップから作り直す。本番は、0.2.32 以前のレプリカを記録の水準で拒否する（0.2.32 以前のレプリカは、親の変更をブランチに取り込んだ記録（`heimdb_branch_sync`）を知らず、本番に昇格するとマージの基準を読み違える）。
- 0.2.39 はデータ形式のバージョン 7 だが、0.2.33 と同じく、レプリカに運ぶ記録の形はバージョン 2 のまま変わらない。レプリカを先に上げる。上げた後のレプリカのデータはバージョン 7 なので、0.2.38 以前に戻すときは本番のバックアップから作り直す。本番は、0.2.38 以前のレプリカを記録の水準で拒否する（0.2.38 以前のレプリカは、DECIMAL、FLOAT、DOUBLE、ENUM の列を含むインデックスのキーの水準を知らず、本番に昇格した後の書き込みでそのエントリを保守しない）。上げた後も、前からある DECIMAL などのインデックスは前と同じく全件を読む。0.2.42 からは、本番が起動の後に main のものを裏で作り直す（下の「古いインデックスの作り直し」）。0.2.41 以前のままエントリを作るには、本番で `CALL heimdb_rebuild_indexes()` を呼ぶ（docs/procedures.md）。
- 0.2.41 はデータ形式のバージョン 8 だが、0.2.39 と同じく、レプリカに運ぶ記録の形はバージョン 2 のまま変わらない。レプリカを先に上げる。上げた後のレプリカのデータはバージョン 8 なので、0.2.40 以前に戻すときは本番のバックアップから作り直す。本番は、0.2.40 以前のレプリカを記録の水準で拒否する（0.2.39 と 0.2.40 のレプリカは、FLOAT(M,D) と DOUBLE(M,D) の型を桁のない型と読み、本番に昇格した後の書き込みで丸めず、SET と BIT の列を含むインデックスのエントリを保守しない）。上げた後も、前からある表の FLOAT と DOUBLE の列は桁のない型のまま（前に `FLOAT(10,7)` と作った列も `float`）で、前からある SET と BIT の列のインデックスは前と同じく全件を読む。0.2.42 からは、本番が起動の後に main のものを裏で作り直す（下の「古いインデックスの作り直し」）。0.2.41 のままエントリを作るには、本番で `CALL heimdb_rebuild_indexes()` を呼ぶ（docs/procedures.md）。
- 0.2.11 から、本番は、記録の水準を示さないレプリカ（0.2.10 以前。0.2.7 以前と見分けられない）と水準の足りないレプリカを拒否する（`HeimDB: the replica is too old for this primary` を、本番の `replica <名前>` の行と、レプリカのログと `replication_last_error` に出し、レプリカは記録を適用せずに止まる）。上の「レプリカを先に」の順なら困らない。本番を先に上げたら、レプリカも上げれば続きから追いつく。
- 記録の中身の形（レプリカに運ぶ記録の形のバージョン）の変わるバージョン: **本番を先に**上げる（レプリカを先に上げると、新しいバージョンのレプリカは自分のデータを新しい形式にしてしまい、古い本番とつながらず、古いバージョンにも戻せない）。本番とレプリカは形式のバージョンを確かめ合い、違えば、レプリカは記録を 1 件も適用せずに止まる（`HeimDB: data format version differs (primary N, replica M)` を、ログと `heimdb_status` の `replication_last_error` に出す。本番の `replica <名前>` の行にも出る）ので、本番を上げてからレプリカを上げるまでの間、レプリカは追いつかない。0.1.5 以前の HeimDB はバージョンを示さず、バージョン 1 として扱う。
  - 0.1.5 以前から 0.1.6 以降（バージョン 1 から 2）: 形式の中身の変換はないので、本番を上げた後、レプリカも止めて新しいバージョンで起動すれば、そのまま続きから追いつく（作り直さなくてよい）。
  - 中身を変換するバージョン（将来）: 本番を上げた後、レプリカを本番の新しいバックアップから作り直す（リリースの説明に書く）。

## 脆弱性の確かめ方

`.github/workflows/vuln.yml`（段階 49 と 127）が、main への push、PR、毎週月曜に、既知の脆弱性を調べる。手動でも流せる（`gh workflow run vuln`）。

- `govulncheck`: Go の最新の安定バージョンで、HeimDB のコードが実際に呼ぶ関数の脆弱性を調べる。
- `toolchain`: go.mod の `toolchain` のバージョン（イメージを作る Go と同じ）で調べる。標準ライブラリの脆弱性は使う Go のバージョンで決まるので、Go の修正バージョンが出たらここで落ちる。go.mod の `toolchain` と `Dockerfile` の `golang:` のバージョンを上げてリリースする（Dependabot は go.mod の `toolchain` を上げない）。
- `image`（毎週と手動だけ）: 公開している `tsukinowa/heimdb:latest` の amd64 のバイナリを取り出し、`govulncheck -mode binary` で調べる。出したイメージが古い Go や古い依存で作られていれば落ちる。新しいバージョンを出して直す。`Dockerfile` は記号表を残してビルドする（`-s` を付けると、govulncheck が呼ばない関数の脆弱性まで出す）。
- `advisories`: `internal/sqlengine` にフォークした go-mysql-server と vitess は、govulncheck にも Dependabot にも見えない（モジュールのパスが HeimDB のもの）。GitHub の Go の advisory を 2 つのモジュールで引き、`.github/known-advisories.txt` にないものがあれば落ちる。落ちたら、advisory の直した PR を読み、フォークの同じ経路に当たるかを確かめる。当たるなら、上流の修正を取り込む段階として直し（`docs/engine.md` の変更の一覧に記録）、リリースする。当たらない（v0.20.0 にない経路、WordPress が通らず HeimDB が拒否する経路など）なら、理由を書いて `known-advisories.txt` に足す。

手元では `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...` で同じものを調べられる（手元の Go のバージョンで）。

## 危ない変更の見張り

WordPress が乗っ取られるときに変わる行が変わると、HeimDB はログに警告を出す（止めはしない）。

```
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: option default_role: \"subscriber\" -> \"administrator\""
level=warning msg="HeimDB: watch: wordpress@172.18.0.4 on wordpress: administrator role given: user ID 7 (wp_capabilities)"
```

| 見張るもの | ログに出す中身 |
| --- | --- |
| オプションの `siteurl`、`home`、`admin_email`、`users_can_register`、`default_role`、`template`、`stylesheet` | 前と後の値（長ければ切る） |
| オプションの `active_plugins` | 有効にしたプラグインと、無効にしたプラグイン |
| オプションの `<接頭辞>user_roles`（役割の定義） | 変わったことだけ |
| ユーザーの追加と削除 | ログイン名と ID |
| パスワードの変更 | ログイン名と ID（ハッシュは出さない） |
| メールアドレスの変更 | 前と後のアドレス |
| ユーザーに administrator の役割が付いた（`<接頭辞>capabilities`） | ユーザー ID |
| マルチサイトの特権管理者（`site_admins`） | 前と後の一覧 |
| マルチサイトのネットワークでのプラグインの有効化（`active_sitewide_plugins`） | 有効にしたプラグインと、無効にしたプラグイン |

- 行頭の `wordpress@172.18.0.4` は変えたユーザー、`on` の後はデータベース（ブランチなら `wordpress@staging`）。マージで入った変更には `(merge of branch staging)` が付く。
- テーブルは名前の終わりと列で見分けるので、接頭辞が `wp_` でなくても、マルチサイトの `wp_2_options` でも効く。
- コミットした変更だけを出す（ロールバックや失敗した文の変更は出ない）。WordPress のインストール、管理画面での設定の変更、wp-cli での変更も出る。
- `CALL heimdb_status()` の `watch_changes`（起動からの件数）、`watch_last_change`、`watch_last_change_at` でも見られる（起動し直すと 0 に戻る）。
- 復元（`heimdb_restore`）で戻した行と、レプリカでは出さない。`siteurl`、`home`、ユーザーはマージの規則で本番に入らないので、マージでも出ない。テーブルごとの操作（`TRUNCATE`、`DROP TABLE`、`RENAME TABLE`）も見張らない。
- 普段の運用でも出るもの: WordPress のインストール、ユーザーの登録を開いたサイトやネットショップでの客の登録（`user added`）、WordPress 6.8 以降に上げた後の各ユーザーの最初のログイン（パスワードのハッシュを作り直すので `password changed`）、`wp search-replace` や `wp db import`（まとめて出る）。

### 心当たりのない行があったら

1. その少し前の時刻から後に変わった行を見る: `CALL heimdb_changes('main', '2026-10-12 03:00:00')`。`detail` に見張りの説明が付いた行と、同じ頃に変わった行（記事、プラグインのテーブル）を確かめる。
2. 戻す内容を見る: `CALL heimdb_restore_preview('main', '2026-10-12 03:00:00')`。既定ではユーザーを戻さないので、後から足された管理者などは `not restored` の行に出る。
3. 戻す: `CALL heimdb_restore('main', '2026-10-12 03:00:00')`（ユーザーも戻すなら `'*'`。その時刻の後に登録したユーザーやコメントも消える）。結果の最後の行の呼び出しで、復元を取り消せる。表を指定して戻すときは、記事と `wp_postmeta`、`wp_term_relationships` のように、行とメタの表を一緒に指定する（片方だけなら、結果の `detail` に両方を戻す呼び出しが出る。`docs/procedures.md` の `heimdb_restore`）。
4. WordPress の側でも、管理者のパスワードの変更、知らないプラグインのファイルの削除などをする（データベースの外のファイルは戻らない）。

## 暗号化した退避

退避を、本番とは別の権限の環境（検証の環境など）で開くための仕組み（0.2.28 から）。退避には、すべてのブランチと履歴のほか、DB のユーザーとパスワードのハッシュ（`mysql_native_password`。総当たりに弱い）も入る。

**鍵**: [age](https://age-encryption.org/) の鍵の組を、退避を開く側で作る（`age-keygen -o verify.key`）。秘密鍵のファイルは開く側だけが持ち、公開鍵（`age1...` の 1 行）だけを本番に渡す。本番の HeimDB に置くのは公開鍵だけなので、本番のマシンが乗っ取られても、過去の退避は解けない。1 つのファイルに公開鍵を複数行書けば、どの秘密鍵でも解ける退避になる。

```sh
./heimdb -data ./data -backup-dir ./backups -backup-key verify=/run/secrets/verify-recipients
```

```sql
CALL heimdb_backup('', 'verify');   -- 名前は時刻（20261030T082028.000000Z.tar.gz.age）
```

- 退避は、store の全体（WAL を含む）を tar.gz にして暗号化した 1 つのファイル `<名前>.tar.gz.age`（0600）。書き出し中は `<名前>.tar.gz.age.partial` に書き、終えてから名前を変える。暗号化する前の写し（`.<名前>.ckpt`）は、失敗や `KILL QUERY` でも消す。起動時には、前の起動が残した書きかけのものを消す。
- 公開鍵のファイルは退避のたびに読むので、差し替えに起動し直しは要らない。退避のたびに、鍵の名前と受取人をログに出す（`HeimDB: heimdb_backup <名前>: encrypted with key verify to age1...`）。前に読んだときと中身が変わっていれば、変わる前と後を並べて警告する（`HeimDB: recipients of backup key verify changed since ...`）。差し替えの予定がないのにこれが出たら、ファイルが書き換えられた合図。
- SQL の文に出るのは鍵の名前だけ。受け付ける受取人は age の X25519 の公開鍵だけ（パスフレーズと SSH の鍵は受け付けない。サーバに秘密を置かないため）。
- 鍵を指定しない `heimdb_backup` は、今までどおり、そのまま開けるディレクトリを作る。

**開く**: サーバを起動せずに、空のデータのディレクトリに展開してから起動する。

```sh
heimdb -data ./restored restore-backup -in 20261030T082028.000000Z.tar.gz.age -identity verify.key
# Docker: ENTRYPOINT が -data /data を渡すので、その後ろに続ける
docker run --rm -v verify-data:/data -v /path/to/backups:/backups:ro -v /path/to/verify.key:/run/secrets/verify.key:ro \
  tsukinowa/heimdb:<バージョン> restore-backup -in /backups/20261030T082028.000000Z.tar.gz.age -identity /run/secrets/verify.key
```

- いったんデータのディレクトリの中の仮のディレクトリに展開し、最後まで復号できたことと store のファイルがあることを確かめてから移す。鍵が違う、鍵がない、ファイルが壊れているか途中で切れている、データのディレクトリが空でないときは、何も残さずに 0 以外の終了コードで終わる。
- 形式は標準の age なので、`age -d -i verify.key <ファイル> | tar -xz -C <ディレクトリ>` でも開ける（失敗したときに途中まで展開したものが残るので、`restore-backup` のほうが確実）。
- 展開した store には本番のユーザーがいるので、起動時に `-reset-passwords`（`root@'%'` と `-site-user` のパスワードを、`-root-password` と `-site-password` で上書きする。ほかのユーザーと権限は残す）か `-reset-users`（`root@'%'` と、`-site-password` があればサイトのユーザーのほかは、すべてのユーザーとロールを消す。`root@'%'` とサイトのユーザーのパスワードは上書きし、サイトのユーザーの権限は残す。いなければ作る）を付ける。`-reset-passwords` は `'%'` でないホストの root やサイトのユーザー（`root@'localhost'` など）を本番のパスワードのまま残すので、あればログに警告が出る（接続元によってはそちらに当たる。消すか `-reset-users` を使う）。サイトのパスワードを指定しなければ、サイトのユーザーには何もしない。どちらも、ユーザーのいない store では作るだけで、値がすでに同じなら書き込まない（付けたままでも、起動のたびに書き換えない）。ログには起動のたびに 1 行出る。レプリカと `-no-auth` では使えない。本番で使うと、`ALTER USER` と同じくレプリカにも写る。WordPress のユーザー（`wp_users`）には触れない。
- 動いている store 自体（データのディレクトリ）は暗号化しない。置き場所のディスクの暗号化で守る。

## レプリカ

本番のコミットを順に受け取って適用する、読み取り専用のレプリカを置ける（ブランチ、`AS OF` の履歴、ユーザーと権限も同じになる）。マシンが壊れたときは、レプリカを昇格させて続ける。切り替えは手動。

```sh
# 本番: レプリカに記録を送る（HTTP。暗号化するなら下の「通信の暗号化（TLS）」）
export HEIMDB_REPLICATION_TOKEN='long-random-secret'
./heimdb -data ./data -backup-dir ./backups -replication-addr 10.0.0.1:3310
# 本番でバックアップを取り（CALL heimdb_backup('replica')）、そのディレクトリをレプリカのマシンに写す
# （暗号化した退避なら、レプリカのマシンで restore-backup で展開する。上の「暗号化した退避」）
./heimdb -data ./backups/replica -replicate-from 10.0.0.1:3310    # レプリカ
```

- レプリカは、バックアップの時点から本番の記録で追いつく。止めて起動し直すと、続きから受け取る。書き込みはエラーになる。
- 本番は記録を `-replication-log-retention`（既定 24 時間）だけ残す。バックアップからレプリカを起動するまでの間や、レプリカが止まっていた間がそれより長いと追いつけないので、バックアップから作り直す（レプリカはそのエラーをログと `heimdb_status` に出して止まる）。
- 古いバージョンを消す GC は複製しない。レプリカも自分の `-retention` と自分の時計で GC する（本番の GC が、レプリカで読んでいる最中のバージョンを消さないように）。レプリカの `AS OF` と、昇格した後に復元できる範囲は、レプリカの `-retention` で決まるので、レプリカにも本番と同じ値（またはそれより長く）を付ける。`-replication-log-retention` はレプリカが追いつくための記録を残す期間で、この範囲とは関係ない。
- どこまで追いついているかは、レプリカの `CALL heimdb_status()` の `replication_position` と `replication_behind`（本番の最後のコミットとの差）、本番の `replica <名前>` の行で見る。
- 同期（MySQL の準同期レプリケーションと同じ）: 本番はコミットをディスクに書いた後、レプリカがそれを適用してディスクに確定させたという知らせを受けてから OK を返す（それまでほかの接続からも見えない）。`-replication-sync-timeout`（既定 10 秒）を過ぎても知らせがなければ、OK を返して非同期に切り替え、レプリカが追いつくと同期に戻る（どちらもログに出る）。起動したときは非同期で、レプリカが追いつくと同期になる。今の状態は本番の `heimdb_status` の `replication_sync`（`sync`、`async`、`off`）で見る。書き込みのたびにレプリカとの往復とディスクの確定を待つので、コミットは遅くなる（同じマシンのレプリカで 1 回あたり約 5 ms、コメントの追加が 1.7 倍ほど。[`bench.md`](bench.md)）。同期が要らなければ `-replication-sync-timeout 0` にする。レプリカの適用がいつも待ち時間より遅いと、追いついて同期に戻るたびに、次の書き込みが待ち時間だけ止まって非同期に切り替わる（MySQL と同じ）。そのときはレプリカのマシンを見直すか、同期をやめる。

昇格（本番を切り替えるとき、本番が壊れたとき）:

1. 本番を止める（壊れて止まっていればそのまま）。
2. OK を返したコミットがすべてレプリカにあるかを確かめる。
   - 本番を止められたとき: 本番のログの最後の `replication:` の行が `last commit ... was acknowledged by replica <名前>` なら、最後のコミットまでそのレプリカにある。止めるときは、レプリカの知らせが進んでいる間は待ち時間を過ぎても待つ。`was NOT acknowledged` なら、本番を起動し直してレプリカを追いつかせ（`replication_behind` が 0s）、もう一度止める。ログに `last commit` の行がない（実行中の文が終わらずに `statements still running` で終えた）ときは、次の「マシンが壊れたとき」で判断する。
   - 本番のマシンが壊れたとき: レプリカの `heimdb_status` の `replication_primary_sync`（最後に受け取った応答のときの本番の同期の状態）が `sync` で、本番が止まった時刻が `replication_last_contact` の時刻から `-replication-sync-timeout` よりも十分前（マシンの時計のずれの分、数秒の余裕を見る）なら、OK を返したコミットはすべてレプリカにある（本番が非同期に切り替わるには、知らせのない時間が待ち時間だけ続く必要がある。`replication_last_contact` は、本番がそれより後に待ち始めたコミットしか知らせを待っていないと言える時刻で、受け取った時刻より少し前になる）。`async` なら、非同期の間のコミットは届いていないことがある。
   - レプリカが複数台なら、`replication_position` がいちばん進んだレプリカを昇格させ、上の判断もそのレプリカで行う（本番はどれか 1 台の知らせで OK を返すので、遅れたレプリカも本番を `sync` と見ていることがある）。
3. レプリカを止め、`-replicate-from` を外して同じデータで起動し直す。これが新しい本番になる（自分も記録を持つので、そのまま次のレプリカの元になる）。履歴（`AS OF` と復元の範囲）はそのまま残る。古い HeimDB で作ったエントリのないインデックスが残っていれば、本番として起動した後に裏で作り直す（下の「古いインデックスの作り直し」）。`-retention` は外さない（付け忘れると既定の 30 日になる）。昇格の後のコミット時刻は、それまでの最後のコミットより必ず後になる（新しいマシンの時計が遅れていても逆戻りしない）。
4. WordPress の `DB_HOST` を新しい本番に向ける。古い本番は、レプリカとしてつなぎ直さず、新しい本番のバックアップから作り直す（知らせを待っていたコミットを自分だけが持っていることがある。つなぐと、位置が新しい本番の記録にないので止まる）。

壊れた本番を、切り替えずに本番のまま起動し直してもよい。知らせを待っていたコミット（OK は返していない）は、起動し直すと見えるようになり、レプリカにも送られる（MySQL と同じ）。

## 通信の暗号化（TLS）

`-tls-cert` と `-tls-key` を指定すると、MySQL の接続で TLS を受け付け（クライアントが望んだとき）、レプリカへの送り出し（`-replication-addr`）を HTTPS にする。`-require-tls` を足すと、TLS でない接続をパスワードを確かめる前に拒否する（エラー 3159）。TLS 1.2 以上。証明書を替えるときは起動し直す。

`-tls-cert` なしで動かすときは、クライアントに TLS を求めさせない。TLS を必須にしたクライアント（Node.js の mysql2 の `ssl`、mysql クライアントの `--ssl-mode=REQUIRED` など）はつながらない（mysql2 は `Server does not support secure connection`、mysql クライアントは `SSL connection error: SSL is required but the server doesn't support it`）。

自前の CA で証明書を作る例（ECDSA P-256。RSA より接続の握手が速い）。`subjectAltName` には、クライアントとレプリカが接続に使う名前と IP をすべて入れる。

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
  -subj "/CN=HeimDB CA" -keyout ca-key.pem -out ca.pem
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=heimdb" \
  -keyout server-key.pem -out server.csr
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -days 825 -out server.pem \
  -extfile <(printf "subjectAltName=DNS:db.example.internal,IP:10.0.0.1\nextendedKeyUsage=serverAuth")

./heimdb -data ./data -tls-cert server.pem -tls-key server-key.pem -require-tls
```

- WordPress: `wp-config.php` に `define( 'MYSQL_CLIENT_FLAGS', MYSQLI_CLIENT_SSL );` を足す（`-require-tls` でこれがないと「データベース接続確立エラー」）。WordPress は CA を渡す手段がないので、**暗号化はするが HeimDB の証明書を確かめない**（PHP の mysqlnd は CA を渡さないと確かめない）。盗聴は防げるが、なりすましの中継は防げないので、WordPress と HeimDB はプライベートなネットワークか VPN の中に置く。
- mysql コマンドと mysqldump（MySQL 8）: 既定（`--ssl-mode=PREFERRED`）で暗号化する。証明書を確かめるなら `--ssl-mode=VERIFY_IDENTITY --ssl-ca=ca.pem`。
- レプリカ: `-replicate-from https://10.0.0.1:3310 -replication-ca ca.pem`（`-replication-ca` があれば `https://` は省ける）。証明書を確かめられないときは、エラーをログと `heimdb_status` の `replication_last_error` に出してやり直し続ける。合言葉も暗号化される。
- 状態: `heimdb_status` の `tls`（`off`、`on`、`required`）、`tls_cert_not_after`（証明書の期限）、`connection_tls`（その接続が TLS か）。期限の前に証明書を作り直して起動し直す。
- 接続 1 回の時間は約 1 ms から約 2.5 ms になるが、ページの表示の時間は揺れの範囲で変わらない（[`bench.md`](bench.md)）。

## 書き込みが詰まったとき

書き込むトランザクションはブランチごとに 1 つで、書き込みのロックを `COMMIT` か `ROLLBACK` まで持つ。ほかの書き込みは `-lock-wait-timeout`（既定 50 秒）待ってエラー 1205 になる。ロックを持ったまま止まった接続があると、書き込みが詰まり続ける（トランザクションの中で何もしなくなった接続、トランザクションの中で終わらない文）。

- **HeimDB が自分で外す**: トランザクションの中で何もしないまま、ロックを `-idle-lock-timeout`（既定 30 秒）より長く持つ接続は切る。切った接続のトランザクションはロールバックされ、ロックが外れ、待っていた書き込みが続く。ログに `HeimDB: closing a connection idle in a transaction with the write lock ...` と、接続 ID、ユーザー、持っていた時間、最後の文が出る。管理者の接続も切る（対話的に `BEGIN` したまま放置しない）。文を実行中の接続は切らない（`main` の実行時間の上限で止める。`heimdb_branch_limit`）。
- **持ち主を見る**: 指標（DB の権限が要らない）の `heimdb_branch_write_lock_holder{branch}`（持ち主の接続 ID。マージや復元など内部の操作なら 0）、`heimdb_branch_write_lock_held_seconds`（持っている時間）、`heimdb_branch_write_lock_idle_seconds`（持ち主が何もしていない時間。実行中なら 0）、`heimdb_branch_write_lock_waiters`（待っている文の数）。書き込みが 1205 になると、ログに `HeimDB: lock wait timeout; the write lock of branch main is held by connection 8613 (wordpress) for 5m3s, running a statement, last statement: ...` が出る（10 秒に 1 回まで）。管理者は `CALL heimdb_status()` の `write_lock <ブランチ>` でも見られる。
- **自分で切る**: サイトのユーザーのままで、`SHOW PROCESSLIST`（または `SELECT * FROM information_schema.processlist`）に自分のユーザーの接続が出る（PROCESS 権限は要らない。MySQL と同じ）。持ち主の ID を `KILL <ID>` で切ると、その接続のトランザクションはロールバックされ、ロックが外れる。文だけを止めるなら `KILL QUERY <ID>`（明示的なトランザクションの中の文なら、ロックは残る）。ほかのユーザーの接続は切れない（1095）。ロックを待っている文も `KILL QUERY` で止まる。
- **起動し直すのは最後**: 起動し直すと、持ち主の手がかり（接続、最後の文）が消える。

## 遅い文のログ

`-slow-query 1s` のように指定すると、それ以上かかった文を警告のログに出す。

```
level=warning msg="slow query: SELECT ..." connectionID=12 database=wordpress duration=1.234s indexes=wp_postmeta.meta_key,wp_posts.PRIMARY lock_wait=0s rows_examined=20412 user=wordpress
```

- `duration` は文を受け取ってから結果を書き終わるまで。時間は 1 µs 刻みで、`312µs`、`1.234ms`、`1.5s` のように出す（`µ` は U+00B5。0.2.26 までは 1 ms 刻みだった）。`rows_examined` は文が読んだ本体の行とインデックスのエントリの数（インデックスから本体の行を引くと両方を数える）。`indexes` は読んだインデックス（`表.インデックス`。主キーは `PRIMARY`）。待った時間は、待ったときだけ出す: `lock_wait`（書き込みのロック）、`replica_wait`（同期のレプリカの知らせ）、`stats_wait`（表の統計を作るのを待った時間。起動し直した後の表ごとの最初の文）。
- 閾値は、管理者が止めずに変えられる: `SET GLOBAL long_query_time = 0.5`（秒、小数可）、`SET GLOBAL slow_query_log = OFF`（`ON` で戻す）。`-slow-query` は起動したときの値で、起動し直すと戻る。接続ごとの `SET SESSION long_query_time` は読まない（サイトの PHP が自分の接続でログをあふれさせないように）。今の値は `CALL heimdb_status()` の `slow_query`。
- ログには訪問者が入れた字（検索語、フォームの値）が入りうる。`-slow-query-redact` を付けると、文の値を `?` に畳んで出す（文は小文字で書き直す。解析できない文は先頭の 2 語だけ）。ユーザーの管理の文（`CREATE USER` など）は、付けなくても本文を隠す。
- 読まない変数: `wait_timeout`、`interactive_timeout`（何もしない接続は切らない。上の `-idle-lock-timeout`）、`innodb_lock_wait_timeout`、`lock_wait_timeout`（`-lock-wait-timeout`）、`net_read_timeout`、`net_write_timeout`、接続ごとの `long_query_time` は、`SET` を受け付けるが読まない。接続ごとに `SET` すると、変数ごとに 1 回、ログに `HeimDB: SET <変数> is accepted but has no effect` が出る。
- 文の実行時間の上限: 接続ごとの `SET SESSION max_execution_time = 5000`（ミリ秒）か、管理者の `SET GLOBAL max_execution_time`（その後につないだ接続の既定）で、MySQL と同じく `SELECT` の文（`WITH ... SELECT`、かっこで始まる `UNION` を含む）を止め、エラー 3024 を返す。`INSERT ... SELECT`、`UPDATE`、`SET`、`DO` の中の `SELECT` には効かない。ブランチの上限（`heimdb_branch_limit`）があれば短い方に従い、`SET` で外すことはできない。クライアントが切れたときの文は、Linux では TCP の接続が閉じれば数秒で止まる（PHP が接続を持ったまま待っていると止まらない）。

## 起動し直した直後

HeimDB は文の計画に使う統計（表の行数と、インデックスごとの値の分布）をメモリにだけ持つ。起動すると、接続を受け付けながら、main のすべての表の統計を裏で作る。作り終える前にその表を使う文は、作り終えるのを待つ（表を全部読む分。記事 20 万件、`wp_postmeta` 320 万行で、起動から約 1 秒、文は最大 0.4 秒ほど遅れる。[`bench.md`](bench.md) の段階 106）。待った時間は `heimdb_stats_wait_seconds_total` と遅い文のログの `stats_wait` に出る。

外からの要求を流す前に待つなら、次のどちらかを見る。
- 指標の `heimdb_stats_ready` が 1 になるまで（DB の権限が要らない）。
- `CALL heimdb_status()` の `stats` が `ready` になるまで。`stats_ready_at`（作り終えた時刻）、`stats_build_duration`（かかった時間）、`stats_tables`（`18 built, 0 failed` のように、作った表と失敗した表の数）も出る。失敗した表があっても `ready` になる（その表は、使う最初の文が統計を作る）。表の一覧を読めなかったときは `failed` になり、`stats_error` に理由が出る（そのときは各表の最初の文が統計を作る）。

`ready` は起動したときの 1 回が終わったことを表す。その後の書き込みや `ALTER TABLE` で統計を作り直しても `building` には戻らない。ブランチの表は温めない（ブランチごとに、表を使う最初の文が作る）。レプリカでも作るが、追いついたかとは別なので、レプリカは `replication_behind` と合わせて見る。

## 古いインデックスの作り直し

0.2.38 以前の HeimDB で作った `DECIMAL`、`FLOAT`、`DOUBLE`、`ENUM` の列を含むインデックスと、0.2.40 以前で作った `SET` と `BIT` の列を含むインデックスは、エントリを持たず、引くたびに表の全件を読む（WooCommerce の `wc_product_meta_lookup` の `min_max_price` と `stock_quantity` など）。0.2.42 から、本番は起動して接続を受け付け始めた後に、main のそういうインデックスを裏で作り直す（`CALL heimdb_rebuild_indexes()` と同じ書き込み。`-rebuild-indexes=false` で止める）。

- 1 つずつ作り直す。1 つの間は main への書き込みを待たせ（読むのは止めない）、インデックスとインデックスの間でロックを放す。10 万行の表で 0.1〜0.4 秒、100 万行で、一意でないインデックスが 1〜2 秒、一意のインデックスが 4〜5 秒ほど（macOS で測った値。段階 121）。その間の書き込みは、作り終えるまで待つ（100 万行の一意のインデックスの間で最大 3 秒）。待たされた書き込みは `-lock-wait-timeout`（既定 50 秒）まで待つ。
- 書き込みのロックは 5 秒だけ待つ。長いトランザクションがロックを持っていて取れなければ、そのインデックスを飛ばし、次の起動でやり直す。
- 見積もった行の数が、一意でないインデックスで 500 万行、一意のインデックスで 100 万行を超えるものは、書き込みを長く待たせるので飛ばす。空いている時間に `CALL heimdb_rebuild_indexes()` で作り直す。
- 一意のインデックスで行の値が重複していれば（0.2.38 以前の `CREATE UNIQUE INDEX` は重複を確かめなかった）、そのインデックスを飛ばし、残りを続ける。重複を直してから `CALL heimdb_rebuild_indexes()` を呼ぶ。
- 飛ばしたものは、表とインデックスの名前と理由を警告でログに出す。始めに作り直す数、インデックスごとに名前と行の見積もりとかかった時間、終わりにまとめを出す。
- 書き込みのロックの持ち主は `rebuild indexes` と出る（`heimdb_status` の `write_lock main`、指標の `heimdb_branch_write_lock_holder` は 0）。
- 様子は `CALL heimdb_status()` の `index_rebuild`（`off`、`running`、`done`）、`index_rebuild_done_at`、`index_rebuild_duration`、`index_rebuild_indexes`（`6 rebuilt, 1 skipped` のように、最後の 1 回で作り直した数と飛ばした数）、`index_rebuild_error`（最後に飛ばした理由）と、指標の `heimdb_index_rebuild_ready`、`heimdb_index_rebuild_seconds`、`heimdb_index_rebuild_skipped` で見る。
- 止めると（SIGTERM）、作りかけのインデックスはそこでやめ、作りかけのエントリは消える（消し切れなかった分も、次の起動で消える）。次の起動で残りを作り直す。
- `CALL heimdb_restore()` で main を戻した後にも、もう一度動く（戻した表の定義に古いインデックスがあれば作り直す）。
- 作り直しの途中に取ったバックアップを戻すと、そこまでのインデックスは古いまま戻り、次の起動で作り直す。
- ブランチは作り直さない（ホスティングのブランチは短く生きるので）。ブランチのデータベースにつないで `CALL heimdb_rebuild_indexes()` を呼ぶ。起動時の警告にはブランチの名前も出る。
- レプリカでは動かない。本番が作ったエントリは記録でレプリカに届く。レプリカを昇格すると、本番として起動したときに作り直す。

## 指標（Prometheus）

`-metrics-addr 127.0.0.1:9104` のように指定すると、`GET /metrics` で Prometheus のテキスト形式の指標を返す。Prometheus、Grafana Cloud（Alloy）、Google Cloud Managed Service for Prometheus（Ops Agent）、New Relic（インフラのエージェントの nri-prometheus か OpenTelemetry Collector）、Datadog など、Prometheus の形式を読めるものなら取り込める。本番でもレプリカでも開ける。

- **認証がない。** 取り込み役は同じホストか同じネットワークの中から読む。`127.0.0.1` か内側のアドレスで開き、ファイアウォールで外に開けない。Docker では `-metrics-addr 0.0.0.0:9104` にして、ポートをホストに公開せず（`ports` に書かない）、同じネットワークの取り込み役から `db:9104` で読む。
- 出すのは数と大きさだけで、文の中身、ユーザー名、データベース名、表の名前は出さない。ブランチ名（`branch`）とレプリカの名前（`replica`、`-replica-name`）は出す。
- 読むたびに今の値を集める（重い処理はない）。30 秒か 1 分ごとに読めば足りる。

| 指標 | 型 | 意味 |
|---|---|---|
| `heimdb_build_info{version}` | gauge | HeimDB のバージョン（値は 1） |
| `heimdb_start_time_seconds` | gauge | 起動した時刻（Unix 時刻） |
| `heimdb_replica` | gauge | レプリカなら 1、本番なら 0 |
| `heimdb_connections`、`heimdb_connections_total` | gauge、counter | 開いている接続の数、起動からの接続の数 |
| `heimdb_running_statements` | gauge | 実行中の文の数 |
| `heimdb_statements_total{type}` | counter | 終わった文の数。`type` は文の先頭の語で `select`、`insert`、`update`、`delete`、`replace`、`call`、`ddl`、`transaction`、`set`、`show`、`other`（構文のエラーの文も `other`） |
| `heimdb_statement_errors_total{code}` | counter | エラーで終わった文の数。`code` は接続先に返した MySQL のエラー番号（1205 はロックの待ち、1041 はメモリ、1146 はない表、1213 はデッドロック、3024 は実行時間の上限。構文のエラーは 1105）。1041、1205、1213、3024 は起きる前から 0 で出す。準備（`COM_STMT_PREPARE`）は数えず実行だけを数えるので、準備の段階のエラーも数えない（WordPress はサーバ側の準備を使わない） |
| `heimdb_statement_duration_seconds` | histogram | 文の実行と結果の送信にかかった時間 |
| `heimdb_slow_statements_total` | counter | 遅い文のログに出した文の数（下の「遅い文のログ」。出していなければ 0） |
| `heimdb_branch_write_lock_holder{branch}`、`heimdb_branch_write_lock_held_seconds{branch}`、`heimdb_branch_write_lock_idle_seconds{branch}` | gauge | 書き込みのロックの持ち主の接続 ID（内部の操作なら 0）、持っている時間、持ち主が何もしていない時間（実行中なら 0）。持ち主のいるブランチだけ出る（上の「書き込みが詰まったとき」） |
| `heimdb_connection_write_lock_held_seconds{branch}` | gauge | 接続がブランチの書き込みのロックを持っている時間（マージや復元など内部の操作は含まない）。すべてのブランチについて、接続が持っていなければ 0 で、いつも出る（一時テーブルのロックは、その接続しか待たないので出さない）。2 つの指標を突き合わせられない監視（Cloud Monitoring など）でも、「接続が長く持っている」の警報に使える。検証環境のブランチを同じプロセスに持つなら、`{branch="main"}` で本番だけを見られる（0.2.25 ではブランチのラベルがなく、全ブランチのいちばん長い時間だった） |
| `heimdb_branch_write_lock_waiters{branch}` | gauge | 書き込みのロックを待っている文の数（待っているブランチだけ） |
| `heimdb_longest_statement_seconds` | gauge | いちばん長く走っている文の時間 |
| `heimdb_idle_lock_disconnects_total` | counter | `-idle-lock-timeout` で切った接続の数 |
| `heimdb_lock_wait_seconds_total`、`heimdb_replica_wait_seconds_total`、`heimdb_stats_wait_seconds_total` | counter | 文が待った時間の合計。書き込みのロック、同期のレプリカの知らせ、表の統計を作るのを待った時間（起動し直した後の表ごとの最初の文） |
| `heimdb_stats_ready`、`heimdb_stats_build_seconds` | gauge | 起動した後に main の表の統計を作り終えたら 1（それまでは 0。失敗した表があっても 1）、作るのにかかった秒数（作り終えるまで出ない）。上の「起動し直した直後」 |
| `heimdb_index_rebuild_ready`、`heimdb_index_rebuild_seconds`、`heimdb_index_rebuild_skipped` | gauge | 起動の後に main の古いインデックスを作り直し終えたら 1（作っている間と、`-rebuild-indexes=false` とレプリカでは 0）、かかった秒数（終えるまで出ない）、飛ばした数。上の「古いインデックスの作り直し」 |
| `heimdb_memory_limit_bytes`、`heimdb_memory_abort_bytes`、`heimdb_memory_live_bytes` | gauge | メモリの上限、文を止める境目、生きているヒープ（上の「運用の注意」のメモリの上限。上限がなければ 0） |
| `heimdb_memory_aborted_statements_total` | counter | メモリの上限で止めた文の数 |
| `heimdb_engine_caches` | gauge | SQL のエンジンが持っている文の途中結果（GROUP BY、DISTINCT、並べ替え、サブクエリ）のキャッシュの数。文が動いていなければ 0。文がないときにも 0 に戻らず増え続けるなら、片付けの漏れ（段階 108、109） |
| `heimdb_cache_size_bytes`、`heimdb_cache_used_bytes` | gauge | ブロックキャッシュの大きさと使っている分 |
| `go_goroutines` | gauge | goroutine の数 |
| `heimdb_disk_bytes` | gauge | データのディスク使用量 |
| `heimdb_branches`、`heimdb_branch_size_bytes{branch}` | gauge | ブランチの数（main を含む）、ブランチごとの大きさ（推定。`heimdb_branch_list` の `size_bytes`） |
| `heimdb_last_commit_timestamp_seconds` | gauge | 最後のコミットの時刻 |
| `heimdb_open_transactions`、`heimdb_oldest_snapshot_age_seconds` | gauge | 開いたトランザクションの数、最も古いスナップショットの古さ（GC の境界を止めている。なければ 0）。読むだけのトランザクションも含む（`mysqldump --single-transaction` の論理ダンプは、その間ずっと増える）。書き込みが詰まっているかは、下の書き込みのロックの指標で見る |
| `heimdb_pinned_times` | gauge | 使用中の時刻（`AS OF`、復元、過去の時点からのブランチ作成）の数 |
| `heimdb_gc_boundary_age_seconds` | gauge | GC の境界の古さ（まだ消していなければ出ない） |
| `heimdb_gc_last_run_timestamp_seconds`、`heimdb_gc_last_duration_seconds`、`heimdb_gc_last_failed` | gauge | 最後の GC の時刻、かかった時間、失敗したら 1（起動してから GC が動いていなければ出ない） |
| `heimdb_replication_sync`、`heimdb_replication_async_switches_total` | gauge、counter | 本番: 同期なら 1、非同期に切り替わった回数 |
| `heimdb_replica_behind_seconds{replica}`、`heimdb_replica_last_seen_seconds{replica}` | gauge | 本番: レプリカごとの遅れと最後の連絡からの時間 |
| `heimdb_replication_behind_seconds`、`heimdb_replication_last_contact_seconds`、`heimdb_replication_failing` | gauge | レプリカ: 本番からの遅れ、最後の連絡からの時間、最後の要求が失敗したか止まったら 1 |
| `heimdb_tls_cert_expiry_timestamp_seconds` | gauge | TLS の証明書の期限（`-tls-cert` のとき） |
| `heimdb_watch_changes_total` | counter | 危ない変更の見張りが見つけた変更の数 |

設定の例は [`scripts/metrics`](../scripts/metrics) にある。

- **Prometheus:** [`prometheus.yml`](../scripts/metrics/prometheus.yml)（`db:9104` を読む）と、知らせる規則の例 [`rules.yml`](../scripts/metrics/rules.yml)。規則は、指標が読めない、メモリの境目に近い・文が止められた、ロックの待ちの失敗、古いスナップショット、GC の失敗、レプリカの遅れ・連絡がない・受け取れない、証明書の期限、危ない変更。本番はつないできたレプリカを起動し直すまで覚えているので、レプリカを減らすと、その名前の `HeimDBReplicaGone` は本番を起動し直すまで続く（Alertmanager で黙らせる）。Alertmanager や Grafana から知らせる。監視は HeimDB と別の VM に置く（同じ VM だと、VM ごと止まったときに知らせも止まる）。
- **手元で見る（Prometheus と Grafana）:** [`compose.yml`](../scripts/metrics/compose.yml)。`-metrics-addr 127.0.0.1:9104` で動かした手元の HeimDB を読み（Docker Desktop の `host.docker.internal` 経由）、`rules.yml` の規則と、ダッシュボードの例 [`grafana-dashboard.json`](../scripts/metrics/grafana-dashboard.json)（クエリの数と時間、エラー、接続、メモリ、ディスク、スナップショット、知らせ）を読み込む。`docker compose -f scripts/metrics/compose.yml up -d` で、Grafana は http://127.0.0.1:3000 （ログインなし）、Prometheus は http://127.0.0.1:9090 。ポートは 127.0.0.1 だけで開く。試すためのもので、Grafana はログインなしで管理者になる。
- **New Relic（OpenTelemetry Collector）:** [`otel-collector.yml`](../scripts/metrics/otel-collector.yml)。Collector（`otel/opentelemetry-collector-contrib`）が HeimDB を読み、OTLP で New Relic に送る。ライセンスキーは環境変数 `NEW_RELIC_LICENSE_KEY` で渡す。
- **New Relic（インフラのエージェント）:** VM にエージェントを入れているなら、Prometheus の連携（nri-prometheus）の対象に `http://127.0.0.1:9104/metrics` を足す（設定の書き方は New Relic の文書を見る）。

