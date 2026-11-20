# MySQL 互換のテスト集

WordPress と同じ接続（wpdb と mysqli）で文を流し、返った値を、本物の MySQL 8.0 で取った記録と比べる（`docs/plan.md` の段階 91）。値は mysqli が返す字のまま、エラーの文と番号と SQLSTATE、`rows_affected`、`insert_id`、`SHOW WARNINGS` を記録する。テストは期待値を書かない。同じテストを本物の MySQL と調べる DB に流し、記録を並べる。

PHPUnit の全体の比べ（`docs/plan.md` の「検証方法」、30 分ほど）は、テストが落ちない違いを見ない。このテスト集は、MySQL との小さな違い（警告、エラーの字、double の字の形、照合順序）を、数秒で見る。HeimDB に限らず、WordPress に MySQL 互換の DB を使うときの確かめに使える。

## 流し方

```sh
# HeimDB（データベースの名前は -db で決める。テストが WordPress の表を消して入れ直すので、専用のデータで）
heimdb -addr 127.0.0.1:3338 -data /tmp/compat-data -root-password root -db heimdb_compat &
scripts/compat/run.sh 3338 /tmp/heimdb.json
go run ./cmd/compatdiff compat/baseline/mysql-8.0.json /tmp/heimdb.json
```

- `scripts/compat/run.sh <port> <out.json>` は、wordpress-develop（`WP_DEVELOP_DIR`、既定はこのリポジトリと並べた `../wordpress-develop`）を `/app` に、このディレクトリを `/compat` にマウントした PHPUnit のコンテナ（`scripts/wpphp` と同じイメージ）で、`vendor/bin/phpunit -c /compat/phpunit.xml` を流す。データベースは `heimdb_compat`（`WP_TESTS_DB_NAME`）、パスワードは `WP_TESTS_DB_PASSWORD`（既定は root）。本物の MySQL には、なければデータベースを作る。
- `cmd/compatdiff` は、違う値と、片方にしかない記録を一覧にし、あれば終了コード 1 を返す。意図した違いと、知っていてまだ直していない違いは、`known-differences.txt` に理由と一緒に書く。直って違わなくなったら知らせるので、その行を消す。

## 接続の確かめ

最初に、接続の文字セットと照合順序が WordPress と同じ（`utf8mb4`、`utf8mb4_unicode_520_ci`）か、`sql_mode` に wpdb が外すもの（strict など）が残っていないかを確かめ、違えば止める（終了コード 2）。クライアントの文字セットが既定のまま（latin1 など）だと、値が壊れて入り、違いを取り違える。`sql_mode` そのものは記録する（wpdb はサーバの `sql_mode` から外すだけなので、サーバの既定の違いが出る）。

## ベースラインを取り直す

```sh
docker run -d --name realmysql -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=wordpress -p 3307:3306 mysql:8.0
scripts/compat/run.sh 3307 /tmp/mysql-a.json
scripts/compat/run.sh 3307 /tmp/mysql-b.json
cmp /tmp/mysql-a.json /tmp/mysql-b.json && cp /tmp/mysql-a.json compat/baseline/mysql-8.0.json
```

2 回流して同じになることを確かめてから置き換える（同順位の並びなど、流すたびに変わる値を記録しない）。今の記録は MySQL 8.0.46。

## テストを足す

`tests/` に `Compat_TestCase` を継いだクラスを置く。`Compat::observe_sql(label, sql)` は文を流して結果、エラー、行の数、警告を記録し、`Compat::observe(label, value)` は任意の値を記録する。キーは「クラス::テスト::label」。

- WordPress のテストは、テストごとにトランザクションで包み、終わると戻す。`CREATE TABLE` は一時テーブルに書き換えられるので、表は `set_up_before_class` で作り、`tear_down_after_class` で消す。
- 流す順や時刻で変わる値（ID、`NOW()`）を記録しない。ID は差で、記事は日付を決めて作り、並びは同順位にならないように決める。

## ライセンス

WordPress のテストの仕組みの上に作るので、このディレクトリは GPL-2.0-or-later（[LICENSE](LICENSE)）。HeimDB の Go のモジュールにも Docker のイメージにも入らない。`cmd/compatdiff` は HeimDB と同じ Apache License 2.0。
