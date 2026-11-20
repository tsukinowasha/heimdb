#!/bin/sh
# MySQL 互換のテスト集（compat/README.md）を、ホストの <port> で待ち受ける DB に流し、観察した値を <out.json> に書く。
#
#   scripts/compat/run.sh 3307 /tmp/mysql.json       # 本物の MySQL（realmysql）
#   scripts/compat/run.sh 3338 /tmp/heimdb.json      # HeimDB（heimdb -db heimdb_compat -addr 127.0.0.1:3338 ...）
#   go run ./cmd/compatdiff compat/baseline/mysql-8.0.json /tmp/heimdb.json
#
# テストは、データベース heimdb_compat（WP_TESTS_DB_NAME で変えられる）の WordPress の表を消して入れ直す。大事な
# データのあるデータベースに向けない。HeimDB は -db heimdb_compat で起動する（HeimDB は CREATE DATABASE を
# しない）。本物の MySQL には、なければ作る。パスワードは WP_TESTS_DB_PASSWORD（既定は root）。
# PHPUnit のコンテナのイメージは scripts/wpphp と同じ（WPPHP_IMAGE、既定は wp-phpunit:latest）。
set -eu
if [ $# -ne 2 ]; then
	echo "usage: $0 <port> <out.json>" >&2
	exit 2
fi
port=$1
out=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
repo=$(cd "$(dirname "$0")/../.." && pwd)
dir="${WP_DEVELOP_DIR:-$(cd "$(dirname "$0")/../.." && pwd)/../wordpress-develop}"
image="${WPPHP_IMAGE:-wp-phpunit:latest}"
db="${WP_TESTS_DB_NAME:-heimdb_compat}"
pw="${WP_TESTS_DB_PASSWORD:-root}"

# 本物の MySQL ならデータベースを作る（HeimDB では失敗するので、結果は見ない）
if command -v mysql >/dev/null; then
	mysql -h 127.0.0.1 -P "$port" -uroot -p"$pw" -e "CREATE DATABASE IF NOT EXISTS \`$db\`" >/dev/null 2>&1 || true
else
	echo "$0: no mysql client; create the database $db yourself if the server is MySQL" >&2
fi

rm -f "$out"
docker run --rm \
	--add-host host.docker.internal:host-gateway \
	-v "$dir":/app -v "$repo/compat":/compat:ro -v "$(dirname "$out")":/out \
	-w /app \
	-e WP_TESTS_DB_HOST="host.docker.internal:$port" -e WP_TESTS_DB_PASSWORD="$pw" -e WP_TESTS_DB_NAME="$db" \
	-e COMPAT_OUT="/out/$(basename "$out")" ${COMPAT_DB_CHARSET:+-e COMPAT_DB_CHARSET="$COMPAT_DB_CHARSET"} \
	-e COMPAT_UID="$(id -u):$(id -g)" \
	"$image" sh -c 'vendor/bin/phpunit -c /compat/phpunit.xml; s=$?; [ -f "$COMPAT_OUT" ] && chown "$COMPAT_UID" "$COMPAT_OUT"; exit $s'
test -s "$out"
