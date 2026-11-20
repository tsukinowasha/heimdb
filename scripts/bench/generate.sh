#!/bin/sh
# ベンチマークのデータを本物の MySQL（127.0.0.1:3307、root/root）のデータベース wpbench に作り、heimdb
# （127.0.0.1:3316、root/rootpw、-db wpbench で起動しておく）に取り込む（docs/bench.md）。
#
#   記事 5,000 件、固定ページ 200 件、カテゴリ 100、タグ 200、コメント 20,000 件、投稿メタ 15,000 件
set -eu
cd "$(dirname "$0")"
dc() { docker compose -f docker-compose.yml "$@"; }
my() { mysql -h127.0.0.1 -P3307 -uroot -proot "$@" 2>/dev/null; }

my -e "DROP DATABASE IF EXISTS wpbench; CREATE DATABASE wpbench"
dc up -d wp-mysql wp-heimdb
dc run --rm -T cli-mysql wp core install --url=http://localhost:18181 --title=Bench \
  --admin_user=admin --admin_password=adminpw --admin_email=admin@example.com --skip-email

content=$(python3 -c "print(' '.join(['Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua.']*15))")
echo "$content" | dc run --rm -T cli-mysql wp post generate --count=5000 --post_content
echo "$content" | dc run --rm -T cli-mysql wp post generate --post_type=page --count=200 --post_content
dc run --rm -T cli-mysql wp term generate category --count=100
dc run --rm -T cli-mysql wp term generate post_tag --count=200
my wpbench < data.sql
dc run --rm -T cli-mysql wp term recount category post_tag

mysqldump --single-transaction --set-gtid-purged=OFF -h127.0.0.1 -P3307 -uroot -proot wpbench > wpbench.sql 2>/dev/null
MYSQL_PWD=rootpw mysql -h127.0.0.1 -P3316 -uroot wpbench < wpbench.sql
rm wpbench.sql
