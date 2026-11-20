#!/bin/sh
# 最初のデモ: WordPress で記事を書き、消し、AS OF で消す前の記事を読む。
#
# 前提: heimdb がホストの 3306 番で動いていて、scripts/wordpress の WordPress がインストール済み。
# heimdb のユーザーとパスワードは HEIMDB_USER（既定 root）と HEIMDB_PASSWORD で渡す。
#   HEIMDB_PASSWORD=... scripts/demo-asof.sh
set -eu
cd "$(dirname "$0")/.."
compose="docker compose -f scripts/wordpress/docker-compose.yml"
db() { MYSQL_PWD="${HEIMDB_PASSWORD:-}" mysql -h127.0.0.1 -P"${HEIMDB_PORT:-3306}" -u"${HEIMDB_USER:-root}" wordpress "$@" 2>/dev/null; }

title="タイムトラベルのデモ $(date +%H%M%S)"
id="$($compose run --rm cli wp post create --post_title="$title" --post_content='消しても AS OF で読める' --post_status=publish --porcelain 2>/dev/null | tr -d '\r')"
echo "1. 記事を作った: ID=${id}「${title}」"

sleep 1
before="$(db -N -e 'SELECT NOW(6)')"
echo "2. 削除前の時刻を記録した: ${before}（セッションの time_zone での時刻）"
sleep 1

$compose run --rm cli wp post delete "$id" --force >/dev/null 2>&1
echo "3. 記事を完全に削除した（wp post delete --force）"

echo
echo "-- 今の wp_posts:"
now="$(db -N -e "SELECT ID, post_title FROM wp_posts WHERE ID = $id")"
echo "${now:-（なし）}"

echo "-- wp_posts AS OF '${before}':"
past="$(db -N -e "SELECT ID, post_title, post_status FROM wp_posts AS OF '${before}' WHERE ID = $id")"
echo "${past:-（なし）}"

if [ -z "$now" ] && [ -n "$past" ]; then
  echo
  echo "OK: 削除した記事を AS OF で読めた"
  echo "復元するには: INSERT INTO wp_posts SELECT * FROM wp_posts AS OF '${before}' WHERE ID = ${id};"
else
  echo "NG: 期待した結果にならなかった" >&2
  exit 1
fi
