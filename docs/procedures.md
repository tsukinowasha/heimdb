# HeimDB の手続き

HeimDB の操作は、すべて SQL の `CALL heimdb_*(...)` で行う。mysql クライアント、wp-cli の `wp db query`、phpMyAdmin など、MySQL につながるものならどれからでも呼べる。

- 呼べるのは管理者（SUPER 権限。`root` か `GRANT ALL ON *.*` のユーザー）だけ。ステージングのサイトのユーザーからは呼べない。例外は 2 つ。
  - 読むだけの [`heimdb_merge_excludes()`](#heimdb_merge_excludes) は、サイトのユーザーとブランチだけの権限のユーザーも呼べる（そのデータベースの EXECUTE 権限。`GRANT ALL ON <データベース>.*` に含まれる）。
  - 起動フラグ `-branch-procedures` を付けると、ブランチだけの権限のユーザーが、自分のブランチの差分、解決、変更の一覧、復元を呼べる（[ブランチのユーザーが呼べる手続き](#ブランチのユーザーが呼べる手続き)）。
- `heimdb_branch_diff`、`heimdb_branch_resolve`、`heimdb_changes`、`heimdb_restore_preview`、`heimdb_restore` は、ブランチに `''` を渡すと、接続中のデータベースのブランチを対象にする（`wordpress@staging` につないでいれば `staging`、`wordpress` なら `main`）。
- 時刻は `'YYYY-MM-DD HH:MM:SS'`（小数の秒も可）で、セッションの `time_zone` の時刻として読む。`'2026-10-10 12:31:10.396594+00:00'` のようにオフセットを付ければ、その時刻そのもの（HeimDB が結果に出す時刻はこの形）。
- ブランチの名前は、英小文字、数字、`_`、`-`。`main` が本番。
- レプリカでは、データを変える手続き（ブランチの作成、削除、解決、マージ、復元）は `HeimDB: this server is a read-only replica` になる。
- 引数の数が違うと、エラーになる（接続は切れない）。

## 一覧

| 手続き | すること | データを変える | ブランチのユーザー |
| --- | --- | --- | --- |
| [`heimdb_branch_create(名前[, 親[, 時刻]])`](#heimdb_branch_create) | ブランチを作る | ○ | |
| [`heimdb_branch_list()`](#heimdb_branch_list) | ブランチの一覧 | | |
| [`heimdb_branch_delete(名前)`](#heimdb_branch_delete) | ブランチを消す | ○ | |
| [`heimdb_branch_limit(名前, 大きさ, 時間)`](#heimdb_branch_limit) | ブランチの大きさと文の実行時間に上限を付ける | ○ | |
| [`heimdb_branch_diff(名前[, テーブル[, 開始, 件数]])`](#heimdb_branch_diff) | 親との差分を見る | | フラグで可 |
| [`heimdb_branch_resolve(名前, テーブル, 行, 側)`](#heimdb_branch_resolve) | 競合をどちらで解決するか記録する | ○ | フラグで可（行の指定だけ） |
| [`heimdb_branch_merge(名前[, 'dry_run'])`](#heimdb_branch_merge) | 親にマージする（`'dry_run'` なら確かめるだけ） | ○ | |
| [`heimdb_branch_sync(名前[, 側][, 'dry_run'])`](#heimdb_branch_sync) | 親の変更をブランチに取り込む | ○ | |
| [`heimdb_merge_exclude(パターン)`](#heimdb_merge_exclude) | マージから外す表を足す | ○ | |
| [`heimdb_merge_include(パターン)`](#heimdb_merge_include) | マージから外した表を戻す | ○ | |
| [`heimdb_merge_excludes()`](#heimdb_merge_excludes) | マージから外す表の一覧（サイトのユーザーも呼べる） | | 可 |
| [`heimdb_changes(ブランチ, 時刻[, テーブル])`](#heimdb_changes) | ある時刻から後に変わった行を見る | | フラグで可 |
| [`heimdb_restore_preview(ブランチ, 時刻[, テーブル])`](#heimdb_restore_preview) | 復元で何が変わるかを見る | | フラグで可 |
| [`heimdb_restore(ブランチ, 時刻[, テーブル])`](#heimdb_restore) | 過去の時点に戻す | ○ | フラグで可 |
| [`heimdb_status()`](#heimdb_status) | 運用のための状態 | | |
| [`heimdb_backup([名前[, 鍵]])`](#heimdb_backup) | 動いたままバックアップを取る（鍵を指定すれば暗号化する） | | |
| [`heimdb_gc()`](#heimdb_gc) | 古いバージョンを今すぐ消す | | |
| [`heimdb_rebuild_indexes()`](#heimdb_rebuild_indexes) | 前からある DECIMAL、SET、BIT などの列のインデックスにエントリを作る | ○ | |

「ブランチのユーザー」は、[ブランチだけの権限](#ブランチだけの権限)のユーザーが呼べるか。「フラグで可」は、起動フラグ `-branch-procedures` を付けたときだけ、接続中のブランチに限って呼べる。

手続きではないが、過去の時点のデータは `AS OF` で読める（[タイムトラベル](#as-of)）。

## ブランチ

ブランチは、親のある時点の状態から分かれた、書き込みのできる DB の写し。作るのは一瞬で、データは複製しない（変えた行だけを持つ）。ブランチは `wordpress@ブランチ名` という名前のデータベースに見える。WordPress の `DB_NAME` をこの名前にすれば、そのブランチで動く。

### heimdb_branch_create

```sql
CALL heimdb_branch_create('staging');                                -- 今の main から
CALL heimdb_branch_create('child', 'staging');                       -- 親を指定する
CALL heimdb_branch_create('test', 'main', '2026-10-08 09:00:00');    -- 過去の時点から
```

作ったブランチを 1 行で返す（列は [`heimdb_branch_list`](#heimdb_branch_list) と同じ）。過去の時点から作れるのは、古いバージョンを残す期間（`-retention`、既定 30 日）の中だけ。

0.2.42 から、作る間は親の書き込みのロックを取る（段階 121）。親に書き込み中のトランザクションや、インデックスの作成（`CREATE INDEX`、表を書き直す `ALTER TABLE`、`heimdb_rebuild_indexes`、起動の後の作り直し）があれば、それが終わるのを `-lock-wait-timeout` まで待つ（過ぎたら `1205`）。呼んだ接続のトランザクションが親のロックを持っていれば待たない。それまでは、何回かに分けてコミットするインデックスの作成の途中に分岐点が入ることがあり、そのブランチでインデックスを作り直すと、ブランチで消した行がインデックスで引けた。

ブランチで追加した行の AUTO_INCREMENT は、本番と重ならない番号帯から採る。マージした行は、その番号のまま本番に入り、本番はその後も本番の帯の中で続きを振る。

| 列の型 | 本番（main） | ブランチ ID n（2 以上） |
|---|---|---|
| `bigint`（符号の有無とも） | 1 から 10^10 未満 | (n - 1) × 10^10 から 10^10 の幅（最初のブランチは `10000000000`） |
| `int`（符号の有無とも） | 1 から 10^9 未満 | 10^9 + ((n - 2) mod 100) × 10^7 から 10^7 の幅（最初のブランチは `1000000000`） |
| `mediumint` 以下、一時テーブル | 分けない | 分けない |

`int` の帯は 100 個の枠を使い回す。枠の中の見えている行（前に同じ枠を使ったブランチのマージ済みの行を含む）の最大値 + 1 から振るので、ふつうは番号が重ならない。重なるのは、ブランチ ID が 100 違う 2 つのブランチが同時に同じ表に足したとき、同じ枠の前のブランチのマージより前の時点からブランチを作ったとき、マージの後に本番で消した行の番号（100 本後のブランチが振り直すことがある）。重なった行と、帯を分けない型で両側が足した行は、マージの競合になる（`docs/plugins.md` の「ブランチのマージで注意の要る表」）。

`ALTER TABLE ... AUTO_INCREMENT = v` は帯の中で効く。帯の下限より小さい v は下限に上げ、帯の上の v（ほかのブランチの番号を含むダンプを流したときなど）は無視する（MySQL はその値から振る）。帯を使い切ると、INSERT が `HeimDB: AUTO_INCREMENT range is exhausted: table <表>, values below <上限> on this branch` で失敗する（本番の `int` の表なら 10^9 未満を使い切ったとき）。

### heimdb_branch_list

```sql
CALL heimdb_branch_list();
```

```
+---------+--------+----------------------------+-------------------+------------+----------------+------------------+
| branch  | parent | forked_at                  | database          | size_bytes | max_size_bytes | max_execution_ms |
+---------+--------+----------------------------+-------------------+------------+----------------+------------------+
| main    | NULL   | NULL                       | wordpress         |          0 |           NULL |             NULL |
| staging | main   | 2026-10-10 21:30:46.566968 | wordpress@staging |          0 |     2147483648 |            30000 |
+---------+--------+----------------------------+-------------------+------------+----------------+------------------+
```

- `forked_at`: 分岐した時刻（セッションの `time_zone`）。
- `size_bytes`: そのブランチが持つデータの大きさの推定。ディスクに書き出す前の分は数えないので、作った直後は 0 のことがある。
- `max_size_bytes`、`max_execution_ms`: [`heimdb_branch_limit`](#heimdb_branch_limit) で付けた上限（なければ `NULL`）。

### heimdb_branch_delete

```sql
CALL heimdb_branch_delete('test');
```

子のブランチがあれば拒否する（`HeimDB: branch has child branches`）。`main` は消せない。解決の記録も一緒に消える。

### heimdb_branch_limit

```sql
CALL heimdb_branch_limit('staging', '2G', '30s');   -- 大きさの上限と、1 つの文の実行時間の上限
CALL heimdb_branch_limit('staging', '0', '0');      -- 外す（0 は上限なし）
```

検証環境のブランチが、本番と同じ HeimDB のディスクと時間を食い尽くさないようにする。設定したブランチを 1 行で返す（列は [`heimdb_branch_list`](#heimdb_branch_list) と同じ）。大きさは `K`、`M`、`G`、`T`（1024 倍）を付けた数か、バイト数。時間は `ms`、`s`、`m` を付ける。`main` には実行時間の上限だけを付けられる（大きさの上限は、誤って本番を止めないため付けられない。`CALL heimdb_branch_limit('main', '0', '30s')`）。`main` の実行時間の上限は、管理者でない接続の `SELECT` と書き込みの文（`INSERT`、`UPDATE`、`DELETE`、`REPLACE`）にだけ効き、DDL と `CALL` には効かない（プラグインの更新の大きな `ALTER TABLE` を止めない）。WordPress の cron の重い書き込み（期限切れのトランジェントの削除など）が上限を超えると、毎回止まってやり直し続けるので、上限は画面の処理より十分長くする。子のブランチは上限を引き継がない。上限はレプリカにも届く。

- **大きさの上限**: 書き込みの始まり（文が最初に行を書く前と、DDL の前）に、`size_bytes` と同じ推定（1 秒ほど覚える）が上限を超えていれば、エラー 1114（`The table 'wp_posts' is full`。MySQL でディスクや表領域が尽きたときと同じ）で拒否し、ログに警告（`HeimDB: branch staging is over its size limit`。ブランチごとに 1 分に 1 回）を出す。読み取りと、表を消す `DROP TABLE` と `TRUNCATE` は通る。始まった文は止めないので、ディスクに書き出す前の分（数十 MB）と、1 つの文が書く分（大きな表に列を足す `ALTER` は表を丸ごと書き直す）だけ超えうる。実行時間の上限と組み合わせて使う。上限に当たったら、上限を上げるか、ブランチを作り直す（行を消しても大きさは減らない。消した印も書き込みで、古いバージョンは GC まで残る）。マージ（子からこのブランチへ）と復元は止めない。
- **トランザクションの大きさ**: コミットしていない書き込みはメモリに持つので、大きさの上限を付けたブランチに書くトランザクションは、上限の大きさ（最大 256MB）までにする。超えた文はエラー 1197（MySQL で `max_binlog_cache_size` を超えたときと同じ）で取り消す。それまでの書き込みは残る。
- **実行時間の上限**: このブランチの権限を持つユーザー（[ブランチだけの権限](#ブランチだけの権限)）のすべての文（読み取り、書き込み、DDL）に効く。接続中のデータベースによらない（データベースを指定せずにつないでも、別のデータベースに `USE` しても効く）。権限を持つブランチが複数あれば、短い方の上限に従う。管理者（SUPER 権限。`root` か `GRANT ALL ON *.*` のユーザー）には効かない（`GRANT SELECT ON *.*` のような、SUPER のない全体の権限では外れない）。認証を有効にしていないとき（`-no-auth`）は、接続中のデータベースがこのブランチなら、`heimdb_*` の手続きを除くすべての文に効く。過ぎたら文を止め、エラー 3024（`Query execution was interrupted, maximum statement execution time exceeded`。MySQL の `max_execution_time` と同じ）を返す。止めた文の書き込みは残らない（明示的なトランザクションの中なら、その文だけを取り消す）。ただし、自動コミットの文がコミットを終えた直後に止まると、書き込みが残ったままエラー 3024 になることがある（MySQL も同じ）。サイトの PHP が `SET` で外せない（接続ごとの `max_execution_time` は `SELECT` の文にだけ効き、この上限より長くはできない。[`operations.md`](operations.md) の「遅い文のログ」）。
- **一時テーブル**: 一時テーブルは上限の外の場所に置くので、上限を付けたブランチの権限を持つユーザー（認証のないときは、そのブランチにつないだ接続）は作れない（エラー 1235）。WordPress は使わない。

接続の数は、ユーザーに `MAX_USER_CONNECTIONS` を付けて抑える（[ブランチだけの権限](#ブランチだけの権限)）。

### ブランチだけの権限

ステージングのサイトには、そのブランチだけの権限のユーザーを使う（サイトが乗っ取られても本番に届かない）。

```sql
CREATE USER 'staging'@'%' IDENTIFIED BY 'pw' WITH MAX_USER_CONNECTIONS 20;
GRANT ALL ON `wordpress@staging`.* TO 'staging'@'%';
```

`MAX_USER_CONNECTIONS` は、そのアカウントの同時接続数の上限（0 なら上限なし）。超えた接続は、MySQL と同じエラー 1203 で拒否する。後から `ALTER USER 'staging'@'%' WITH MAX_USER_CONNECTIONS 10` で変えられる（`IDENTIFIED` を書かなければパスワードは変わらない）。全体の `@@max_user_connections` は効かない（本番のユーザーにも効いてしまうため）。

### ブランチのユーザーが呼べる手続き

起動フラグ `-branch-procedures`（または環境変数 `HEIMDB_BRANCH_PROCEDURES=1`。フラグを書けば、`-branch-procedures=false` を含めてフラグが優先する）を付けると、ブランチだけの権限のユーザーが、次の 5 つを呼べる。WordPress の管理画面（プラグイン）から、`$wpdb` のまま差分を見て、競合の解決を記録できるようにするため。既定はオフで、オフなら今までどおり管理者だけ。

| 手続き | ブランチのユーザーができること |
| --- | --- |
| [`heimdb_branch_diff`](#heimdb_branch_diff) | 自分のブランチの差分を見る |
| [`heimdb_branch_resolve`](#heimdb_branch_resolve) | 自分のブランチの競合の解決を、行を指定して記録する（`'*'` は管理者だけ） |
| [`heimdb_changes`](#heimdb_changes)、[`heimdb_restore_preview`](#heimdb_restore_preview) | 自分のブランチの変更と、復元の中身を見る |
| [`heimdb_restore`](#heimdb_restore) | 自分のブランチを過去の時点に戻す |

マージ、ブランチの作成と削除、上限の変更、マージから外す表の変更は、管理者だけのまま（上限は、ブランチのユーザーが自分で外せては意味がない）。

- **対象は接続から決まる。** 対象にできるのは、接続中のデータベースのブランチだけ。ブランチに `''` を渡すのが勧める呼び方（`CALL heimdb_branch_diff('')`）。名前を書いてもよいが、接続中のブランチと違えばエラーになる（`HeimDB: a branch user can call heimdb_branch_diff only on the branch it is connected to (connected to "wordpress@staging")`）。`CALL` をデータベース名で修飾しても、別のブランチは指せない。
- **main は対象にできない。** 本番のサイトのユーザー（`wordpress` の権限）がフラグで呼べるようになるわけではない（`HeimDB: only an administrator can call heimdb_restore on the main branch`）。
- **ブランチを作る前の時刻は使えない。** `heimdb_changes`、`heimdb_restore_preview`、`heimdb_restore` に、ブランチを作った時刻より前の時刻を渡すとエラーになる（`HeimDB: a branch user cannot use a time before the branch was created (...)`）。その時刻のブランチは親の状態で、親の履歴を見せることになるため。ただし、`SELECT ... AS OF` はこれまでどおりブランチを作る前の時刻も読める（ブランチを作る前の親の状態。作った後に親で変わった値は読めない）。
- **要る権限は、接続中のデータベースの EXECUTE。** `GRANT ALL ON <データベース>.*` に含まれる。権限を列挙して付けているなら、足す。

  ```sql
  GRANT EXECUTE ON `wordpress@staging`.* TO 'staging'@'%';
  ```

- **ブランチの上限が効く。** ブランチのユーザーが呼んだ手続きにも、[`heimdb_branch_limit`](#heimdb_branch_limit) の実行時間の上限が効く（差分、解決、変更の一覧、復元は、読みながら上限を確かめ、過ぎたらエラー 3024 で止まる）。同時接続数は `MAX_USER_CONNECTIONS` で抑える。管理者（`root` など）には、今までどおり効かない。
- **差分は親の変更を教える。** 差分の要約と行ごとの差分は、分岐の後に親（本番）で変わった行と、競合した列の親の値（`conflict_values`）を返す。ブランチのユーザーは親に権限を持たないので、権限の上では親の情報が流れる向き。同じサイトの本番とその検証環境なら問題にならないが、ブランチをテナントの分離に使うなら、フラグを付けない。

競合の解決を分担するときは、ブランチのユーザー（管理画面）が行ごとに解決を記録し、管理者が差分の `resolved_by` と `resolution` を見てからマージする（[heimdb_branch_resolve](#heimdb_branch_resolve)）。

## 差分とマージ

ふつうの流れは、差分を見る → 競合があれば解決を記録する → マージする。同じブランチで作業を続けて、何度でもマージできる（2 回目からは前のマージとの差分になる）。

```sql
CALL heimdb_branch_diff('staging');                                 -- 要約
CALL heimdb_branch_diff('staging', 'wp_posts');                     -- 行ごと
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=1', 'branch');
CALL heimdb_branch_merge('staging');
```

### マージの規則

- 行は主キーで対応づける（`wp_options` だけは `option_name`）。
- 列ごとに、分岐の時点（2 回目からは前のマージ）、今のブランチ、今の親の三方向で比べる。ブランチだけが変えた列は反映し、親だけが変えた列は残す。両側が同じ列を違う値に変えたら競合。`post_modified` などの更新日時は競合にせず、新しい方を採る。
- 両側で、同じ自然なキーの記事（`post_type` と `post_name`）やターム（`slug`）を別々に追加したら競合。ただし記事は、中身が同じなら競合にせず、親の行を残してブランチの行を飛ばす（`skip`、`same as the parent's ID=4`）。比べないのは主キーと、記事の `guid`、`post_date`、`post_date_gmt`、`post_modified`、`post_modified_gmt`（URL と作った時刻なので、同じ中身でも両側で違う）。ブロックテーマのサイトでは、ブランチを作った後に両側のページを開くだけで、WordPress が既定のナビゲーション（`wp_navigation`）を両側に 1 件ずつ作るため。飛ばした行に付いたブランチのメタなどは、「マージしない記事に属する行」として一緒に飛ばす。マージの後にブランチでその行を編集すると、重複の競合に戻る。タームは中身が同じでも競合のまま（ブロックテーマの `wp_theme` のタームを飛ばすと、ブランチで編集したテンプレートとテーマの結び付きも飛び、本番でテンプレートが効かないため）。記事のメタの重複は警告だけ。（0.2.16 から。それまでは中身が同じ記事も競合）
- ブランチで足した記事のメタ、タームの関係、term_taxonomy、タームのメタが、親になく、このマージでも作らない記事・タームを指すなら飛ばす（`belongs to a post that is not on the parent`）。前のマージで飛ばした（または `'parent'` で解決した）記事に、後からメタを足した場合など。親に孤児の行を入れない（0.2.16 から）。
- 次のものはマージしない（`skip`）。
  - 環境ごとのオプション: `siteurl`、`home`、`blog_public`、`cron`、`rewrite_rules`、`db_version` など、`_transient_*`、`*_lock`、`https_*`
  - 自動下書き、リビジョン、記事の編集ロック（`_edit_lock`、`_edit_last`）
  - コメントとユーザー（`wp_comments`、`wp_commentmeta`、`wp_users`、`wp_usermeta`）。本番の側が正しい
- テーブルの追加、列とインデックスの変更（プラグインの dbDelta）もマージする。列は列ごとに比べ、ブランチだけが変えた列は反映する。親（本番）だけが変えた列はそのままで、ブランチから入る行の、親だけにある列は既定値になる。主キーの列が両側で違うテーブルだけは、テーブルごとの競合（`schema differs`）になる。テーブルの削除はマージしない。
- マージは 1 つのコミット。失敗すれば何も書かない。マージの後に、記事のコメント数とタームの件数を本番の側で数え直し、`rewrite_rules` を空にする（WordPress が次のリクエストで作り直す）。
- 両側の `db_version` が違えば、マージは拒否する（差分は要約の最初の行で知らせる）。両側で WordPress の DB の更新を済ませてからマージする。
- [マージから外した表](#マージから外す表)には、何もしない（行、列の定義、インデックスを比べず、書かない）。
- WordPress の表の接頭辞（`wp-config.php` の `$table_prefix`）は、親の表から見つける（[本体の表の見分け方](#本体の表の見分け方)）。接頭辞が 2 つ以上見つかれば、差分とマージを拒否する（`ambiguous_prefix`）。

### heimdb_branch_diff

読むだけで、何も書かない。テーブルを省くと、テーブルごとの要約を返す。

```sql
CALL heimdb_branch_diff('staging');
```

```
+------------+---------+---------+---------+---------+---------+-----------+----------+----------+--------+
| table_name | status  | inserts | updates | deletes | skipped | conflicts | resolved | warnings | detail |
+------------+---------+---------+---------+---------+---------+-----------+----------+----------+--------+
| wp_options | changed |       0 |       1 |       0 |       1 |         0 |        0 |        0 | NULL   |
| wp_posts   | changed |       1 |       1 |       0 |       0 |         1 |        0 |        0 | NULL   |
+------------+---------+---------+---------+---------+---------+-----------+----------+----------+--------+
```

- `status`: `changed`、`new table`（ブランチで作った）、`dropped on branch`、`dropped on parent`、`schema differs`（主キーの列が両側で違う）、`unchanged`（マージで変えるものはないが、全行を比べる。下の `detail`。重さの知らせなので、マージするものがあるかを見るときは除く）、`excluded`（[マージから外した表](#マージから外す表)。マージでは何もしない）、`not excluded`（パターンに当たったが、本体の表なので外さない。その表の要約は別の行に出る）。
- `inserts`、`updates`、`deletes`: マージで親に行う操作の数。`excluded` の行では、ブランチの側で追加、変更、削除された行の数（反映しない。列を足すと、全行の形が変わって変更に数える）。`skipped` はマージしない行、`conflicts` は解決していない競合、`resolved` は解決を記録した競合。
- `detail`: 列とインデックスの変更（`columns: +w`）と、マージの重さ。
  - `rewrite N rows`: マージで親のテーブルを書き直す（ブランチで列を変えた）。N は親の行数。
  - `full compare N rows`: テーブルを書き直した（列の変更や `TRUNCATE`）側があるので、変わった行だけでなく全行を比べる。N は比べた行数。親の側では、記事、ターム、記事のメタ（親で足された行と重ならないかを分岐の時点から見るもの）がこうなり、ブランチを作り直すまで毎回かかる。
  - どちらも、マージの間に親への書き込みが待たされる時間に響く。差分は親のロックを取らないので、重いマージかどうかを先に見られる。

両側の `db_version` が違えば、最初に次の行を返し、続けてテーブルごとの要約を返す（マージは拒否する。テーブルを指定した行ごとの差分はエラー）。

```
| * | db_version differs | 0 | 0 | 0 | 0 | 0 | 0 | 0 | branch 60717, parent 60421; run the WordPress database upgrade on both sides before merging |
```

テーブルを指定すると（`'*'` ならすべてのテーブル）、行ごとの判定を返す。

```sql
CALL heimdb_branch_diff('staging', '*');
```

```
+------------+----------------------+---------------+---------------+----------+------------+--------------+---------------------------------------------+
| table_name | id                   | branch_change | parent_change | action   | resolution | columns      | detail                                      |
+------------+----------------------+---------------+---------------+----------+------------+--------------+---------------------------------------------+
| wp_options | option_name=blogname | update        | none          | update   | NULL       | option_value | NULL                                        |
| wp_options | option_name=siteurl  | update        | none          | skip     | NULL       | NULL         | environment-specific option                 |
| wp_posts   | ID=1                 | update        | update        | conflict | NULL       | post_title   | the same columns were changed on both sides |
| wp_posts   | ID=2                 | update        | none          | update   | NULL       | post_content | NULL                                        |
| wp_posts   | ID=10000000000       | insert        | none          | insert   | NULL       | NULL         | NULL                                        |
+------------+----------------------+---------------+---------------+----------+------------+--------------+---------------------------------------------+
```

- `id`: 行の名前。[`heimdb_branch_resolve`](#heimdb_branch_resolve) にはこの形のまま渡す。列とインデックスの変更は `COLUMN 名前`、`INDEX 名前`、テーブルの定義の食い違いは `*`。
- `branch_change`、`parent_change`: それぞれの側の変化（`insert`、`update`、`delete`、`none`）。同じ主キーの行の変化だが、同じ自然なキーの記事・タームの重複では、親に別の主キーで足された相手の行を指して `parent_change` が `insert`（0.2.16 から。それまでは `none`）。
- `action`: マージで行うこと。`insert`、`update`、`delete`、`skip`、`conflict`、`warning`。外した表は、行の判定の代わりに `id` が `*` で `excluded`（または `not excluded`）の 1 行。
- `resolution`: 記録した解決（`branch`、`parent`）。記録した後に行が変わっていれば `stale` で、決め直す。
- `columns`: 反映する列（`update`）か、食い違った列（`conflict`）。
- `detail`: 理由。
- `resolved_by`、`resolved_at`: 解決（`resolution`）を記録したユーザー（`'staging'@'%'` の形）と時刻（UTC）。ワイルドカードの解決が当たった行では、そのワイルドカードを記録したユーザーと時刻。0.2.15 より前に記録した解決と、主キーの列が違うテーブルの行（`id` が `*`）では NULL。
- `conflict_values`: 競合した行（`action` が `conflict`）の、競合した列の両側の今の値（JSON）。ほかの行は NULL。ブランチのユーザーは、ブランチを作った後に親で変わった値を読めないので、解決を選ぶ画面に両側の値を出すため。値は、SELECT の結果と同じ書き方の文字列（`DATE` は `2026-03-04`、`DECIMAL(19,4)` は `12.5000`、ゼロの日時は `0000-00-00 00:00:00`）で、`NULL` と行のない側（削除した側）は `null`。キーは列の名前の小文字（`ID` は `id`）。削除と変更の競合では、変更した側が変えた列を出す。同じ自然なキーの記事・タームの重複では、親の相手の行（`detail` の主キーの行）と食い違ったすべての列（主キーを含む）を出す。

  ```
  {"post_title":{"branch":"Hello from staging","parent":"Hello (edited on main)"}}
  ```

`resolved_by`、`resolved_at`、`conflict_values` の 3 列は 0.2.15 で最後に足した。列を名前で読むもの（`$wpdb->get_results` など）は変えなくてよいが、列の数と順で読むもの（Go の `rows.Scan` など）は、3 列を足して読む。

行ごとの差分は、開始と件数を渡すと、その範囲だけを返す（`CALL` は `LIMIT` で包めないので）。

```sql
CALL heimdb_branch_diff('', 'wp_postmeta', 0, 500);     -- 最初の 500 行
CALL heimdb_branch_diff('', 'wp_postmeta', 500, 500);   -- 続き
```

続きがあれば、最後に `table_name` と `id` が `*` で `action` が `more` の行を 1 つ足す。`detail` に、残りの行の数と次の呼び出しが入る（`3120 more rows; next: CALL heimdb_branch_diff('staging', 'wp_postmeta', 500, 500)`）。並びは開始を省いたときと同じ（テーブルの名前の順、テーブルの中は主キーの順）。呼ぶたびに差分を計算し直すので、軽くなるのは受け取る側（PHP）だけで、HeimDB の計算とメモリは変わらない（重さは表の大きさではなく、変わった行の数による）。間にブランチや親が変われば、ページの境目がずれる。テーブルは省けない（`'*'` は使える）。

### heimdb_branch_resolve

```sql
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=1', 'branch');   -- この行はブランチの側を採る
CALL heimdb_branch_resolve('staging', 'wp_posts', '*', 'parent');      -- このテーブルの競合はすべて親の側
CALL heimdb_branch_resolve('staging', '*', '*', 'parent');             -- すべての競合を親の側
CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=1', 'none');     -- 記録を取り消す
```

引数は、ブランチ、テーブル（`'*'` ならすべて）、行（差分の `id` の形。`'*'` ならそのテーブルのすべての競合）、側。

最後の引数は、競合した行でどちらの側の値を採るか。上の差分の例では、記事 ID=1 の `post_title` を、本番は `Hello (edited on main)` に、ステージングは `Hello from staging` に変えていた。

| 側 | 意味 | この例のマージ後のタイトル |
| --- | --- | --- |
| `'branch'` | ブランチ（ステージング）の値を採る | `Hello from staging` |
| `'parent'` | 親（本番）の値を残す | `Hello (edited on main)` |
| `'none'` | 記録した解決を取り消し、未解決に戻す | （マージできない） |

決めるのは競合した列だけ。ステージングだけが変えた列（例えば `post_content`）は、`'parent'` でもステージングの値が入る。

- `branch`: 競合した列もブランチの値にする。削除と変更の競合なら、ブランチの側（削除、または残す）に従う。同じ自然なキーの記事・タームの重複なら、本番の既存の行にブランチの行の内容を写す。主キーが同じ別の行、著者のいない記事、主キーの列が違うテーブル（`schema differs`）では拒否する。ワイルドカードの `'branch'` はこれらの行に当たらず、行は競合のまま残る。そのとき差分の `detail` とマージのエラーに `only 'parent' can resolve this: <理由>` と出る。
- `parent`: 競合した列は親のままにする（競合していない列は反映する）。主キーの列が違うテーブル（`schema differs`）なら、そのテーブルを除いてマージする。列の変更は列ごとにマージするので、プラグインの更新で列が変わっただけのテーブルは除かれない。
- `none`: 記録を取り消す。

行を指定した解決は、ワイルドカードより優先する。行を指定するときは、その行が今の差分で競合になっていなければならない（`'42'` のように数だけでは見つからない。`'ID=42'`）。記録はマージの成功とブランチの削除で消える。

記録には、記録したユーザーと時刻が残り、差分の `resolved_by` と `resolved_at` に出る。解決を記録するたびに、ログにも 1 行出す（`HeimDB: 'staging'@'%' resolved wp_posts ID=1 on branch staging: branch`）。

[ブランチのユーザー](#ブランチのユーザーが呼べる手続き)は、行を指定した記録だけができる。テーブルや行に `'*'` を使う呼び出し（`'none'` で消すときも）は、`HeimDB: a branch user must name a row; only an administrator can resolve with '*'` になる。行を指定した記録は、記録した時点の競合した列の両側の値を覚えていて、その後にどちらかの側で値が変わると使われない（`resolution` が `stale` になり、マージは拒否される）。ブランチのユーザーが見た値と違う値で、管理者のマージが通ることはない。

### heimdb_branch_merge

```sql
CALL heimdb_branch_merge('staging');
```

```
+------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| table_name | status | inserts | updates | deletes | skipped | conflicts | resolved | warnings | detail |
+------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| wp_options | merged |       0 |       1 |       0 |       1 |         0 |        0 |        0 | NULL   |
| wp_posts   | merged |       1 |       2 |       0 |       0 |         0 |        1 |        0 | NULL   |
+------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
```

- `status`: `merged`、`dropped on branch (not merged)` など反映しなかったもの、`excluded`（外した表。件数は反映しなかったブランチの側の変更）、`not excluded`（パターンに当たったが本体の表なので外さなかった。`detail` に当たったパターン）。変えるもののない表は出さない（`excluded` と `not excluded` の行は、変更がなくても出す）。
- 結果の読み方は [マージの結果をプラグインで読む](#マージの結果をプラグインで読む)。

解決していない競合が残っていれば、何も書かずに拒否する（[マージを拒否する理由](#マージを拒否する理由)）。

```
ERROR 51004 (HY000): HeimDB: merge refused (unresolved_conflicts): unresolved conflicts: wp_posts ID=1; resolve them with heimdb_branch_resolve
```

トランザクションの途中では呼べない（`COMMIT` か `ROLLBACK` をしてから）。マージの間、親への書き込みは待たされる。

マージが何かを書いたら、最後の行に、マージを戻す文を返す。

```
| * | undo | 0 | 0 | 0 | 0 | 0 | 0 | 0 | CALL heimdb_restore('main', '2026-10-14 01:23:45.678901+00:00', 'wp_options,wp_posts') |
```

- 時刻は、マージの直前の親の状態そのもの（マージは親のロックを取ってから差分を読むので、その時点の後に親は変わっていない）。どの `time_zone` でも同じ時刻として読める形。
- テーブルは、マージが書いたものだけ（行、列、インデックス、作ったテーブル、数え直した件数、空にした `rewrite_rules`）。
- [`heimdb_restore`](#heimdb_restore) なので、戻すとマージの後に本番がそのテーブルに書いたものも消える。古いバージョンを消した後（[`heimdb_gc`](#heimdb_gc)）は戻せない。
- 記事を戻してコメントは戻さないので、記事のコメント数は今のコメントの数に数え直す（マージの後に本番に付いたコメントは残る）。
- ブランチのマージの起点もマージの前に戻るので、ブランチを直してもう一度マージできる。解決の記録はマージで消えたままなので、競合はもう一度解決する。

結果には、マージで変えたテーブルだけを出す（差分の要約の `unchanged` の行は出さない）。

空打ち: 第 2 引数に `'dry_run'` を渡すと、実際のマージと同じ経路を最後まで通り（解決していない競合や `db_version` の違いは同じエラー）、コミットせずに捨てる。解決の記録とマージの起点も残る。結果は実際のマージと同じテーブルの行に、最後の `* | dry run` の行を足したもの。親のロックを取る時間は実際のマージと同じなので、本番の書き込みはその間待たされる。

```sql
CALL heimdb_branch_merge('staging', 'dry_run');
```

### heimdb_branch_sync

親（本番）の変更を、ブランチに取り込む。ブランチ A と B を本番にマージした後、検証に時間のかかるブランチ C に本番の最新を入れてから確かめる、という使い方をする。ブランチの検証中の変更（まだマージしていない変更）は残る。

```sql
CALL heimdb_branch_sync('c', 'dry_run');   -- 書かずに、取り込む件数と競合を確かめる
CALL heimdb_branch_sync('c');              -- 競合があれば何も書かずに拒否する
CALL heimdb_branch_sync('c', 'branch');    -- 競合した列はブランチの値を残す
CALL heimdb_branch_sync('c', 'parent');    -- 競合した列は親の値を採る
CALL heimdb_branch_sync('c', 'parent', 'dry_run');
```

```
+-------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| table_name  | status | inserts | updates | deletes | skipped | conflicts | resolved | warnings | detail |
+-------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
| wp_comments | synced |       1 |       0 |       0 |       0 |         0 |        0 |        0 | NULL   |
| wp_options  | synced |       0 |       1 |       0 |       1 |         0 |        0 |        0 | NULL   |
| wp_posts    | synced |       1 |       1 |       0 |       0 |         0 |        0 |        0 | NULL   |
| *           | undo   |       0 |       0 |       0 |       0 |         0 |        0 |        0 | CALL heimdb_restore('c', '2026-11-01 08:00:00.000000+00:00', 'wp_comments,wp_options,wp_posts') |
+-------------+--------+---------+---------+---------+---------+-----------+----------+----------+--------+
```

- 比べ方はマージと同じ三方向で、向きが逆になる。親だけが変えた列はブランチに書き、ブランチだけが変えた列は残す。基準は、前の取り込み（なければ前のマージ、どちらもなければ分岐）の時点。
- 取り込むもの: 行はすべて（記事、メタ、ターム、コメント、ユーザー、自動下書き、リビジョンも）。親で作られた表は、ブランチに作って行を写す。コメントを写した記事のコメント数とタームの件数は、ブランチの側で数え直し、`rewrite_rules` を空にする。
- 取り込まないもの: 環境ごとのオプション（`siteurl`、`home`、transient、`cron` など。マージと同じ）、[マージから外した表](#マージから外す表)、ブランチが足した記事やタームと同じ自然なキー（記事は種類とスラッグ、タームはスラッグ）の親の新しい行（ブランチの行を残し、`warnings` に出す。その行にぶら下がるメタなども写さない）、競合でブランチの側を採ったために入れなかった親の記事やタームにぶら下がる行（メタ、コメント、タームの関連）、親で消した表。マージから外した表は `excluded` の行に出す（件数は 0）。
- 競合（両側が同じ列を違う値に変えた行、片側が消して片側が変えた行、両側が同じ ID で違う行を足した行）があれば、何も書かずに 51004 で拒否し、競合の行を出す。第 2 引数の `'branch'` か `'parent'` で、すべての競合をまとめてどちらかに決める。`'branch'` で残したブランチの値は、次のマージで本番に入る。両側が同じ ID で別の行を足した競合は `'parent'` でしか決められない（`'branch'` でも 51004 で拒否する。ブランチの行を残すと、次のマージで本番の行を上書きするため）。行ごとに決めたいときは、ブランチでその行を親と同じ値に直してから取り込む。[`heimdb_branch_resolve`](#heimdb_branch_resolve) の記録は使わない（取り込むと、その前の記録は使われなくなる）。
- 両側で定義の違う表（列、インデックス、主キー）があれば、何も書かずに 51010 で拒否する。ブランチでも同じプラグインの更新（dbDelta）か `ALTER TABLE` をしてから取り込む。
- `db_version` が違えば拒否する（51005）。
- ブランチの書き込みは取り込みの間待たされるが、親（本番）はロックしない（スナップショットで読む）。同じブランチのマージ、取り込み、復元は同時に流れない。トランザクションの途中では呼べない。
- 最後の行は取り込みを戻す文。時刻は取り込みの直前のブランチの状態で、戻すと取り込みの記録も消え、基準も取り込みの前に戻る（戻した後のマージは、取り込まなかったときと同じ）。
- ファイル（プラグイン、テーマ、アップロード）は取り込まない。
- 前のマージより前にブランチで消した表は、親にあれば取り込みで作り直す（前のマージの時点のブランチを基準に比べるため。プラグインを消すときは本番でも消してから取り込む）。
- 拒否の文は `HeimDB: sync refused (<語>): <説明>` で、語と番号は [マージを拒否する理由](#マージを拒否する理由) と同じ。

取り込んだ後の [`heimdb_branch_diff`](#heimdb_branch_diff) と [`heimdb_branch_merge`](#heimdb_branch_merge) は、取り込みが読んだ親の状態を基準に比べるので、写した行はブランチの変更に数えない。取り込みの後に本番を取り込みの前に戻しても、取り込みの記録は残る（本番を戻したことは本番の変更として扱い、写した行をもう一度本番に入れない）。

### マージから外す表

マージに含めたくない表（作り直せる索引など）は、表の名前のパターンで外す。外した表には、マージで何もしない。本番（親）の定義とデータをそのまま使い、空にも置き換えもしない。行、列の定義、インデックスを比べないので、ブランチがプラグインの更新をまたいで表の定義が片側だけ変わっても、マージは止まらない。ブランチで作った表も親に作らず、ブランチで消した表も消さない。マージの `undo` の表の一覧にも入らない（戻すときにも触らない）。

```sql
CALL heimdb_merge_exclude('wp_yoast_%');   -- 足す
CALL heimdb_merge_include('wp_yoast_%');   -- 戻す
CALL heimdb_merge_excludes();              -- 一覧
```

- パターンは表の名前で、`LIKE` と同じく `%` が 0 文字以上、`_` が任意の 1 文字に当たる（エスケープはない。`wp_yoast_%` は `wp_yoast_indexable` に当たり、`wp_posts` は `wpxposts` にも当たる）。大小を区別しない。
- パターンに当たっても、ブランチの側に一度もなかった表（本番だけで作った表）は、結果に行を出さない。
- 設定はサーバ（1 つのサイト）に 1 つで、すべてのブランチのマージに効く。バージョンを持たない設定で、コミットを通して書くので、レプリカとバックアップに付いていく。新しく作ったサイトでは空。
- Yoast SEO を外すときの手順は [`plugins.md`](plugins.md) の「マージから外す表」。

#### heimdb_merge_exclude

パターンを足し、足した後の一覧を返す。同じパターン（大小の違いを含む）がすでにあれば何もしない（何度流してもよい）。管理者だけが呼べる。

本番の表で接頭辞が決まっていれば、[本体の表](#本体の表の見分け方)に当たるパターンはエラーにする。WordPress のインストールの前（接頭辞が決まらない）は受け付け、マージのときに本体の表を外さない（結果に `not excluded` の行を出す）。

```
ERROR 51009 (HY000): HeimDB: merge refused (core_table): the pattern "wp_%" matches the WordPress core table wp_posts, which cannot be excluded from merges
```

#### heimdb_merge_include

パターンを消し、消した後の一覧を返す。なければ何もしない。管理者だけが呼べる。大小を区別せずに一致するものを消す。

#### heimdb_merge_excludes

一覧（`pattern` の列。足した順）を返す。管理者のほか、サイトのユーザーとブランチだけの権限のユーザーも呼べる（プラグインが状態を表示するため。変えることはできない）。

### 本体の表の見分け方

WordPress 本体の表はマージから外せない（記事とメタの関係や件数の数え直しなど、マージの規則が崩れるため）。本体の表は、次の接頭辞に、`posts`、`postmeta`、`options`、`comments`、`commentmeta`、`terms`、`term_taxonomy`、`term_relationships`、`termmeta`、`links`、`users`、`usermeta` を付けた名前と、大小を区別せずに完全に一致する表。名前の終わりだけでは見ないので、`wp_foo_posts` は本体ではない（外せる）。

接頭辞は、親（本番）の表のうち、`option_name` 列を持つ `<接頭辞>options` と `<接頭辞>posts` がそろうものから求める。組が 1 つならその接頭辞、なければ `wp_`。マルチサイトのサブサイトの組（ネットワークの表 `<接頭辞>blogs` を持つ接頭辞 `wp_` に対する `wp_2_options` と `wp_2_posts` のように、ほかの組の接頭辞 + 数字 + `_`）は数えない。サブサイトの表は、記事とメタの関係などの規則を当てずに、ふつうの表としてマージし、本体の表の扱いでもないので、パターンに当たれば外せる。`wp_blogs` がなければ、`wp_2_` も 2 つ目の組として数える（数字だけの接頭辞のサイトに古いインストールの表が残った形）。組が 2 つ以上（古いインストールの表が残った `wp_` と `wp_old_`、同じデータベースに複製を作るプラグインの残り（WP Staging の `wpstg0_` など）など）なら、接頭辞が決まらないので、差分とマージと解決の記録を `ambiguous_prefix` で拒否する（使っていない方の表を消すか、名前を変える）。

組は、次の文で一覧にできる（HeimDB でも MySQL でも動く）。マルチサイトのサブサイトの組（`<接頭辞>blogs` のある行の接頭辞 + 数字 + `_`）も出るので、それを除いて 2 つ以上なら拒否される。

```sql
SELECT LEFT(c.table_name, CHAR_LENGTH(c.table_name) - 7) AS prefix
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema
 AND t.table_name = CONCAT(LEFT(c.table_name, CHAR_LENGTH(c.table_name) - 7), 'posts')
WHERE c.table_schema = DATABASE() AND c.column_name = 'option_name' AND c.table_name LIKE '%options';
```

### マージを拒否する理由

マージ（`dry_run` を含む）、取り込み、差分、解決の記録、除外の追加を拒否するときは、理由の語と、語ごとのエラー番号を付ける。形は `HeimDB: merge refused (<語>): <説明>`（差分と解決の記録でも同じ形。取り込みは `HeimDB: sync refused (<語>): <説明>`）。SQLSTATE は `HY000`。番号は MySQL が第三者に空けている範囲（50000〜51999）から取る。PHP の mysqli なら `errno`（`mysqli_errno()`、`mysqli_sql_exception::getCode()`）で分岐できる。

**語と番号は翻訳せず、足すだけで、変えも消しもしない**（プラグインが分岐に使う）。説明の文は変わることがある。

| 番号 | 語 | 意味 |
| --- | --- | --- |
| 51001 | `branch_not_found` | ブランチがない |
| 51002 | `no_parent` | 親のないブランチ（`main`）をマージしようとした |
| 51003 | `in_transaction` | この接続のトランザクションが親に書いている（`COMMIT` か `ROLLBACK` をしてから） |
| 51004 | `unresolved_conflicts` | 解決していない競合がある（[`heimdb_branch_diff`](#heimdb_branch_diff) で見て [`heimdb_branch_resolve`](#heimdb_branch_resolve) で解決する） |
| 51005 | `db_version_differs` | 両側の `db_version` が違う（両側で WordPress の DB の更新を済ませる） |
| 51006 | `ambiguous_prefix` | WordPress の表の接頭辞が 2 つ以上ある（[本体の表の見分け方](#本体の表の見分け方)） |
| 51007 | `parent_changed` | マージの間に親の行が変わった（ふつうは起きない。もう一度マージする） |
| 51008 | `read_only_replica` | レプリカでマージしようとした |
| 51009 | `core_table` | [`heimdb_merge_exclude`](#heimdb_merge_exclude) のパターンが本体の表に当たる |
| 51010 | `schema_differs` | 両側で定義の違う表があるので取り込まない（[`heimdb_branch_sync`](#heimdb_branch_sync)） |

このほかに、マージがメモリの上限で止まれば 1041、マージの書き込みが大きすぎれば 1197（`docs/operations.md`）を返す。どちらも何も変えない。それ以外のエラー（引数の誤りなど）は 1105 で、語を持たない。

### マージの結果をプラグインで読む

マージは DB サーバの中の操作なので、PHP のフック（`heimdb_after_merge` のようなもの）は HeimDB からは呼べない。プラグインがマージを呼び、結果を読んでフックを出す。

- 成功: [`heimdb_branch_merge`](#heimdb_branch_merge) の結果の行。`table_name` が `*` の行は表ではない（`undo`、`dry run`）。
  - 変えた表: `status` が `merged` の行（キャッシュのパージの範囲）。
  - 外した表: `status` が `excluded` の行。件数が 0 でなければ、ブランチでその表が変わっていた（作り直しが要るかの判断に使う）。
  - 外せなかったパターン: `status` が `not excluded` の行。
  - 解決した競合の数: `resolved` の列。
  - 戻すための文: `status` が `undo` の行の `detail`（[`heimdb_restore`](#heimdb_restore) の呼び出し）。
- 失敗: エラー。番号と語は [マージを拒否する理由](#マージを拒否する理由)。何も書いていない。
- 戻したとき（`undo`）: [`heimdb_restore`](#heimdb_restore) の結果の、`action` が `restore rows` の行が戻した表（列は `table_name`、`action`、`inserts`、`updates`、`deletes`、`detail`）。マージから外した表は戻さないので、マージの後に作り直した索引は、戻した内容に合わせて作り直す。表の一覧を省いた（または `'*'` の）`heimdb_restore` は、外した表も戻すので、`undo` の文の表の一覧はそのまま使う。
- 時間のかかる処理（索引の作り直しなど）は、フックの中で待たずに、WordPress の予約イベントやジョブに回す。

## タイムトラベルと復元

HeimDB は行を上書きせず、変更のたびにバージョンを足す。古いバージョンは `-retention`（既定 30 日）の間残り、その間の任意の時点を読める。

### AS OF

```sql
SET time_zone = '+09:00';
-- 過去の時点のデータを読む
SELECT ID, post_title FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
-- 消した記事を 1 件だけ戻す
INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '2026-10-08 09:00:00' WHERE ID = 123;
```

`AS OF` を付けたテーブルは読むだけ（書くとエラー）。

### heimdb_changes

ある時刻から今までに変わった行を、行が最後に変わった時刻の順に返す。何も書かない。見張りのログ（[`operations.md`](operations.md) の「危ない変更の見張り」）で気づいたときや、乗っ取られた疑いがあるときに、何が変わったかを調べる。

```sql
CALL heimdb_changes('main', '2026-10-12 03:00:00');                          -- すべてのテーブル
CALL heimdb_changes('main', '2026-10-12 03:00:00', 'wp_users,wp_usermeta');   -- 指定したテーブル
```

```
+-------------+--------------------------+--------+-------------------------+----------------------------------+------------------------------------------------------+
| table_name  | id                       | change | columns                 | changed_at                       | detail                                               |
+-------------+--------------------------+--------+-------------------------+----------------------------------+------------------------------------------------------+
| wp_options  | option_name=default_role | update | option_value            | 2026-10-12 03:10:05.123456+00:00 | option default_role: "subscriber" -> "administrator" |
| wp_users    | ID=7                     | insert | NULL                    | 2026-10-12 03:10:09.551200+00:00 | user added: "evil" (ID 7)                            |
| wp_usermeta | umeta_id=88              | insert | NULL                    | 2026-10-12 03:10:09.551200+00:00 | administrator role given: user ID 7 (wp_capabilities) |
| wp_posts    | ID=12                    | update | post_content,post_title | 2026-10-12 03:11:40.000100+00:00 | NULL                                                 |
| *           | *                        | not shown | NULL                 | NULL                             | 41 rows that change routinely (transients, cron, edit locks) |
+-------------+--------------------------+--------+-------------------------+----------------------------------+------------------------------------------------------+
```

- `id`: 行の名前（[`heimdb_branch_diff`](#heimdb_branch_diff) と同じ書き方。`wp_options` は `option_name`）。
- `change`: 時刻の時点の行と今の行を比べた結果（`insert`、`update`、`delete`）。変えて元に戻した行は出ない。`columns` は `update` で変わった列。
- `changed_at`: 行が最後に変わった時刻（UTC）。
- `detail`: 見張りの対象の変化なら、見張りのログと同じ説明。
- 時刻の後に作ったテーブル、消したテーブル、列の変更や `TRUNCATE` で書き直したテーブルは、行ではなくテーブルの 1 行（`table created`、`table dropped`、`table changed`）。
- 毎分のように変わる行（`_transient_*`、`_site_transient_*`、`cron`、記事の `_edit_lock`）は出さず、その数を最後の行に出す。
- 値の前と後は出さない。`AS OF` で読む（`SELECT option_value FROM wp_options AS OF '2026-10-12 03:00:00' WHERE option_name = 'default_role'`）。
- 時刻は、古いバージョンを残す期間（`-retention`）の中だけ。どのブランチでも使える（ブランチ名を渡す）。

### heimdb_restore_preview

[`heimdb_restore`](#heimdb_restore) と同じ引数で、何が戻るかを返す。何も書かない。

```sql
CALL heimdb_restore_preview('main', '2026-10-10 21:31:09');
```

```
+------------+--------------+---------+---------+---------+--------+
| table_name | action       | inserts | updates | deletes | detail |
+------------+--------------+---------+---------+---------+--------+
| wp_options | restore rows |       0 |       1 |       0 | NULL   |
| wp_posts   | restore rows |       1 |       0 |       0 | NULL   |
+------------+--------------+---------+---------+---------+--------+
```

- `action`: `restore rows`（行を戻す）、`restore definition`（テーブルの定義ごと戻す）、`drop`（その時点になかったテーブルを消す）。
- `inserts`、`updates`、`deletes`: 戻すために行う操作の数。
- `detail`: 警告（コメントを戻さないときに、戻すと消える記事にコメントが残る、など）。

### heimdb_restore

```sql
CALL heimdb_restore('main', '2026-10-08 09:00:00');                         -- コメントとユーザー以外
CALL heimdb_restore('main', '2026-10-08 09:00:00', 'wp_options,wp_posts');  -- 指定したテーブル（カンマ区切り）
CALL heimdb_restore('main', '2026-10-08 09:00:00', '*');                    -- すべて
```

ブランチを過去の時点の状態に戻す。テーブルを省くと、コメントとユーザー（`wp_comments`、`wp_commentmeta`、`wp_users`、`wp_usermeta`）は今のまま残す（戻した後に付いたコメントや、作ったユーザーを失わないため）。記事とコメントの片方だけを戻したときは、記事のコメント数を数え直す。

テーブルを省いたとき、戻さないユーザーのテーブルに、時刻の後のユーザーの追加、パスワードやメールアドレスの変更、administrator の役割の付与があれば、結果に `action` が `not restored` の行を出す（`heimdb_restore_preview` も同じ。復元したときはログにも警告で出す）。乗っ取られた後に復元しても、後から足された管理者は残るため。件数は、その変化の行の数（そのテーブルで変わったすべての行ではない）。ユーザーも戻すなら `'*'` で復元するか、そのユーザーを WordPress で消す。

```
| wp_users    | not restored | 1 | 0 | 0 | user added: "evil" (ID 7); to restore users too: CALL heimdb_restore('main', '2026-10-12 03:00:00.000000+00:00', '*') |
```

表を指定するときは、次の組を一緒に指定する。片方だけを戻すと、行とメタ（分類）が食い違う。消した記事はアイキャッチ、カスタムフィールド、カテゴリのない姿で戻り、後から作った記事のメタは残る。中身をメタに持つ記事（Elementor、ACF など）は、題名と状態だけが戻り、中身は今のままになる。

| 行の表 | 一緒に戻す表 |
|---|---|
| `wp_posts` | `wp_postmeta`、`wp_term_relationships` |
| `wp_comments` | `wp_commentmeta` |
| `wp_users` | `wp_usermeta` |

組の片方だけを指定し、時刻の後に同じ ID の行とメタの両方が変わっていれば、戻す表の行の `detail` に、その ID（最初の 5 件と残りの数）を組ごとに出し、最後に、当たった組の表をすべて加えた呼び出しを 1 つ出す（`heimdb_restore_preview` も同じ。復元したときはログにも警告で出す）。復元は止めない。片方だけが変わった ID（記事の題名だけを直した、など）は、戻しても食い違わないので出さない。記事の編集や保存のたびに WordPress が足し引きするメタ（`_edit_lock`、`_edit_last`、公開済みの記事の保存で足され、cron の ping が消す `_encloseme`、`_pingme`、`_trackbackme`）は数えない。時刻の後に組のどちらかの表を書き直した（`ALTER TABLE`、`TRUNCATE`）、作った、消したときは、行を比べず、そのことだけを出す。

```
| wp_posts | restore rows | 1 | 0 | 1 | posts 7, 8 also changed in wp_postmeta after the restore time, which is not restored; posts 7, 8 also changed in wp_term_relationships after the restore time, which is not restored; to restore them together: CALL heimdb_restore('main', '2026-10-18 06:33:19.158171+00:00', 'wp_posts,wp_postmeta,wp_term_relationships') |
```

分類の件数（`wp_term_taxonomy` の `count`）は数え直さない。プラグインの表どうしの組（WooCommerce の注文と注文のメタなど）と、マルチサイトのサブサイトの表（`wp_2_posts` など）は見ない。表を省いた既定の復元では組が割れない。確かめは、戻さない表の変わった行の主キーをメモリに持つので、メモリの上限（`-memory-limit`）が小さく、その表の大半が変わった後だと、記事だけの軽い復元でもエラー 1041 で止まることがある（何も変えない。組の表も一緒に戻すか、上限を上げる）。

復元は過去のバージョンを新しいバージョンとして書く 1 つのコミットで、履歴は消えない。結果の最後の行（`undo`）に、復元を取り消す（復元の直前に戻す）呼び出しが出る。

```
+------------+--------------+---------+---------+---------+-----------------------------------------------------------------------------+
| table_name | action       | inserts | updates | deletes | detail                                                                      |
+------------+--------------+---------+---------+---------+-----------------------------------------------------------------------------+
| wp_posts   | restore rows |       1 |       0 |       0 | NULL                                                                        |
| *          | undo         |       0 |       0 |       0 | CALL heimdb_restore('main', '2026-10-10 12:31:10.396594+00:00', 'wp_posts') |
+------------+--------------+---------+---------+---------+-----------------------------------------------------------------------------+
```

今より先の時刻はエラー。最後のコミットより後の時刻なら、何もしない。トランザクションの途中では呼べない。

戻したブランチを親とするブランチのマージの起点は、戻した時刻より前のもの（なければ分岐の時点）に戻る。マージを戻した後に、同じブランチをもう一度マージできる（すでに入っている変更は、両側が同じ値なので何もしない）。

## 運用

### heimdb_status

```sql
CALL heimdb_status();
```

項目（`name`）と値（`value`）の行を返す。値のない項目は `NULL`。時刻は UTC のオフセット付き。

| 項目 | 内容 |
| --- | --- |
| `disk_bytes` | データのディスク使用量 |
| `branches` | ブランチの数 |
| `connections`、`running_statements` | 接続の数、実行中の文の数（この呼び出しを含む） |
| `open_transactions`、`oldest_snapshot_age_seconds` | 開いたトランザクションの数と、最も古いものの経過秒数。長く開いたままのトランザクションは GC を止める。読むだけのトランザクション（論理ダンプなど）も含む |
| `pinned_times`、`oldest_pinned_time_age_seconds` | 復元などのために GC から守っている時刻の数と、最も古いもの |
| `gc_retention`、`gc_boundary`、`gc_boundary_age_seconds` | 古いバージョンを残す期間と、GC の境界（それより前の時点は読めない） |
| `gc_last_run_at`、`gc_last_duration`、`gc_last_removed`、`gc_last_error` | 最後の GC |
| `memory_limit_bytes`、`memory_abort_bytes` | メモリの上限（`-memory-limit`）と、文がエラー 1041 で止まる境目（上限の 75%）。上限がなければどちらも 0 |
| `memory_live_bytes` | 直前の GC で生きていたヒープ。`memory_abort_bytes` を超えると文が止まり始める。監視はこの 2 つを比べる（[`operations.md`](operations.md) の「メモリの上限」） |
| `memory_aborted_statements` | 起動から、メモリの上限で止めた文の数 |
| `cache_size_bytes`、`cache_used_bytes` | ブロックキャッシュの大きさと、今使っている分。公式のイメージ（cgo なし）では `memory_live_bytes` に含まれる |
| `idle_lock_timeout`、`idle_lock_disconnects` | 何もしないまま書き込みのロックを持つ接続を切る時間（`-idle-lock-timeout`）と、起動から切った数 |
| `write_lock <ブランチ>`、`write_lock_waiters <ブランチ>` | 書き込みのロックの持ち主（接続 ID とユーザー、または内部の操作の名前）、持っている時間、状態（実行中か、何もしていない時間）、最後の文。待っている文の数（[`operations.md`](operations.md) の「書き込みが詰まったとき」） |
| `stats`、`stats_ready_at`、`stats_build_duration`、`stats_tables`、`stats_error` | 起動した後に裏で作る main の表の統計。`stats` は `building`（作っている）、`ready`（起動したときの 1 回が終わった。失敗した表があっても `ready` で、`stats_tables` に `18 built, 0 failed` のように数が出る）、`failed`（表の一覧を読めなかった。理由は `stats_error`）。作り終えた時刻とかかった時間は `ready` のときだけ出る（[`operations.md`](operations.md) の「起動し直した直後」） |
| `slow_query`、`slow_query_redact` | 遅い文のログの閾値（`off` か、`1s` のような時間。`SET GLOBAL long_query_time` と `slow_query_log` で変わる）と、値を畳むか（`-slow-query-redact`） |
| `tls`、`tls_cert_not_after`、`connection_tls` | 通信の暗号化（`off`、`on`、`required`）、証明書の期限、この接続が TLS か |
| `watch_changes`、`watch_last_change`、`watch_last_change_at` | 危ない変更の見張りで見つけた変更の、起動からの件数と最後の 1 件（[`operations.md`](operations.md) の「危ない変更の見張り」） |
| `role`、`last_commit` | `primary` か `replica`、最後のコミットの時刻 |
| `pending_commit` | 同期のレプリカの知らせを待っている（まだ見せていない）コミット（本番だけ。待っているときだけ出る） |
| `replication_*`、`replica <名前>` | レプリケーションの状態（[`operations.md`](operations.md) の「読み取り専用のレプリカ」） |

### heimdb_backup

```sql
CALL heimdb_backup('before-upgrade');   -- 名前を付ける
CALL heimdb_backup();                   -- 名前は時刻（20261010T123114.438826Z など）
CALL heimdb_backup('', 'verify');       -- -backup-key verify=... の受取人に暗号化した 1 つのファイル
```

```
+---------------------------------+----------------------------------+-------+
| path                            | backed_up_at                     | bytes |
+---------------------------------+----------------------------------+-------+
| /backups/before-upgrade         | 2026-10-10 12:31:10.414812+00:00 | 13156 |
+---------------------------------+----------------------------------+-------+
```

動いたまま、整合したバックアップを、起動時の `-backup-dir` の下に作る（なければ `HeimDB: backups are disabled`）。すべてのブランチ、保持期間内の履歴、ユーザーと権限を含む。名前は英数字、`.`、`_`、`-` だけ（任意のパスには書けない）。同じ名前があれば拒否する。

そのディレクトリを `-data` に指定して起動すると、`backed_up_at` の時点に戻る。パスワードのハッシュを含むので、HeimDB を動かすユーザーだけが読めるようにしてある。

2 つ目の引数に `-backup-key` の鍵の名前を指定すると、store の全体（WAL を含む）を、その鍵の age の受取人に暗号化した 1 つのファイル `<名前>.tar.gz.age` にする（`path` はそのファイル、`bytes` はその大きさ）。知らない鍵の名前はエラー。開くには `heimdb restore-backup` で展開する（[`operations.md`](operations.md) の「暗号化した退避」）。

### heimdb_gc

```sql
CALL heimdb_gc();
```

```
+----------------------------------+----------+--------+---------+---------------+
| boundary                         | versions | tables | indexes | merge_records |
+----------------------------------+----------+--------+---------+---------------+
| 2026-09-10 12:31:14.400206+00:00 |        0 |      0 |       0 |             0 |
+----------------------------------+----------+--------+---------+---------------+
```

保持期間（`-retention`）より古いバージョンを今すぐ消し、境界と消した数を返す。ふだんはサーバが `-gc-interval`（既定 1 時間）ごとに動かすので、呼ぶ必要はない。境界より前の時点は、`AS OF`、復元、ブランチの作成に使えなくなる。開いたままのトランザクションや復元の途中の時点があれば、境界はそこで止まる。`-retention 0` なら古いバージョンを消さないので、エラーになる。

### heimdb_rebuild_indexes

```sql
CALL heimdb_rebuild_indexes();
```

```
+---------------------------+----------------+
| table                     | index          |
+---------------------------+----------------+
| wp_wc_product_meta_lookup | stock_quantity |
| wp_wc_product_meta_lookup | min_max_price  |
+---------------------------+----------------+
```

接続中のデータベース（`wordpress` なら main、`wordpress@staging` ならそのブランチ）の表のうち、段階 117 より前の HeimDB（0.2.38 以前）で作った、`DECIMAL`、`FLOAT`、`DOUBLE`、`ENUM` の列を含むインデックスと、段階 120 より前の HeimDB（0.2.40 以前）で作った、`SET` と `BIT` の列を含むインデックスを作り直し、作り直したものを返す。そういうインデックスは、エントリを持たず、引くたびに表の全件を読む（WooCommerce の `wc_product_meta_lookup` の `min_max_price` と `stock_quantity` など）。`SET` の列のインデックスは、MySQL と同じく `=` と `IN` だけを範囲で引く（大小の比べは全件を読む）。作り直すと、新しく作ったインデックスと同じく範囲で引く。新しく作った表とインデックスと、表を書き直す `ALTER TABLE`（列の追加や型の変更など）の後のインデックスは、初めからエントリを持つ（`ALTER TABLE ... FORCE` は HeimDB では何もしないので、作り直しにならない）。

- `CREATE INDEX` と同じく、作り直す間はそのブランチへの書き込みを待たせる（読むのは止めない）。大きな表は何回かに分けてコミットし、途中で止まっても、作りかけは次の起動で消える。
- 一意インデックスで、行の値が重複していれば `1062` のエラーで止まる（それより前の表で作り直したものは残る）。重複を直してから呼び直せば、残りを作り直す。作り直すものがなければ何もしない（何度呼んでもよい）。
- 作り直す前の時点（`AS OF`、復元）と、作り直す前に作ったブランチは、前と同じく全件を読む。ブランチは、ブランチのデータベースにつないで呼べば、そのブランチで作り直す。
- レプリカでは呼べない（本番で呼べば、作ったエントリがレプリカにも届く）。
- 0.2.42 から、本番は起動の後に main のものを裏で 1 つずつ作り直す（`-rebuild-indexes`。既定で動く。[`operations.md`](operations.md) の「古いインデックスの作り直し」）。手続きを呼ぶのは、ブランチ、裏の処理が飛ばしたもの（一意のインデックスの重複、行の多いもの、ロックを取れなかったもの）、`-rebuild-indexes=false` のときになる。手続きと裏の処理が並んでも、後から始めた方は、もう作り直したものを飛ばす。手続きは前と同じく `1062` で止まる（裏の処理は飛ばして続ける）。
- 起動時に、どれかのブランチに作り直していないインデックスがあれば、データベースごとに名前を並べて警告をログに出す（`HeimDB: N indexes on DECIMAL, FLOAT, DOUBLE, ENUM, SET or BIT columns were made by an older HeimDB (0.2.38 or earlier, or 0.2.40 or earlier for SET and BIT) and have no entries, ...`）。文の終わりは、裏の処理が動くときは main のものを裏で作り直す旨（`main's are rebuilt in the background now`）、`-rebuild-indexes=false` のときは前と同じく手続きを呼ぶ旨、レプリカでは本番が作り直してエントリが記録で届く旨になる。
