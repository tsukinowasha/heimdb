#!/bin/bash
# 同じ手順（シナリオ）を、HeimDB と本物の MySQL 8.0 の上の WordPress で流し、エラーになった文を比べる
# （docs/plugins-research.md の「調べ方」の比較、docs/plan.md の段階 68、69）。
#
#   scripts/plugin-check/compare.sh scripts/plugin-check/scenarios/updraftplus.sh
#   PLUGINS="smart-slider-3 sg-security" scripts/plugin-check/compare.sh scripts/plugin-check/scenarios/activate.sh
#   DB=heimdb scripts/plugin-check/compare.sh ...   # 片方だけ（heimdb か mysql）
#
# シナリオは bash の断片で、次の関数を使える: wp（wp-cli）、get（ログインしたまま管理画面などを開き、状態の番号を出す）、
# login、step（見出しを出す）。WordPress は空のサイトを入れたところから始まる（パーマリンクは /%postname%/）。
#
# HeimDB はこのリポジトリから建てて 127.0.0.1:HEIMDB_PORT（既定 3346）で動かし、MySQL は Docker のコンテナで動かす。
# WordPress は 127.0.0.1:WP_PORT（既定 18091）。両方のエラーの文を「回数、エラー、文」で出し、HeimDB でだけエラーに
# なった文があれば終了コード 1（文の字句で比べるので、両方でエラーになり理由が違う文は出ない）。表の有無を確かめる
# 文（DESCRIBE、SHOW など）の「表がない」は、どちらでも出さない。本物の MySQL の記録は 20 万文までなので、長い
# シナリオでは古い文のエラーが落ちる。終わったら、コンテナとボリューム、HeimDB とそのデータを消す。
set -euo pipefail

: "${HEIMDB_PORT:=3346}"
: "${WP_PORT:=18091}"
: "${HEIMDB_ROOT_PASSWORD:=plugin-check}"
export HEIMDB_PORT WP_PORT HEIMDB_ROOT_PASSWORD
if [ "$HEIMDB_PORT" = 3306 ]; then
  echo "HEIMDB_PORT に 3306 は使わない" >&2
  exit 2
fi
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
  sed -n '2,10p' "$0" >&2
  exit 2
fi
scenario="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/heimdb-compare.XXXXXX")"
export PLUGIN_DIR="$work/plugins"
mkdir -p "$PLUGIN_DIR"

db="" pid=""
compose() {
  if [ "$db" = mysql ]; then
    docker compose -p "heimdb-compare-mysql" -f "$here/docker-compose.yml" -f "$here/docker-compose.mysql.yml" "$@"
  else
    docker compose -p "heimdb-compare-heimdb" -f "$here/docker-compose.yml" "$@"
  fi
}
wp() { compose run --rm -T cli wp "$@"; }
mysql_db() { compose exec -T db mysql -uroot -p"$HEIMDB_ROOT_PASSWORD" "$@" 2>/dev/null; }
jar="$work/cookies"
login() {
  curl -s -c "$jar" -b "$jar" -o /dev/null "http://127.0.0.1:$WP_PORT/wp-login.php"
  curl -s -c "$jar" -b "$jar" -o /dev/null --data "log=admin&pwd=admin&wp-submit=Log+In&testcookie=1" \
    "http://127.0.0.1:$WP_PORT/wp-login.php"
}
get() { curl -s -b "$jar" -o /dev/null -w "%{http_code} $1\n" "http://127.0.0.1:$WP_PORT$1" || true; }
step() { echo "--- $*"; }

stop() {
  if [ -n "$db" ]; then
    compose --profile cli down -v >/dev/null 2>&1 || true
  fi
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    while kill -0 "$pid" 2>/dev/null; do sleep 0.2; done
    pid=""
  fi
  rm -f "$jar"
}
trap 'stop; rm -rf "$work"' EXIT

# run は、シナリオを db（heimdb か mysql）の上で流し、エラーになった文を $work/<db>.tsv に書く。
run() {
  db="$1"
  echo "=== $db"
  if [ "$db" = heimdb ]; then
    if lsof -nP -iTCP:"$HEIMDB_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
      echo "ポート $HEIMDB_PORT はもう使われている（HEIMDB_PORT で変える）" >&2
      exit 1
    fi
    (cd "$root" && go build -o "$work/heimdb" ./cmd/heimdb)
    # activate.sh と同じく、WordPress のコンテナから届くところだけで待ち受ける
    case "$(uname)" in
    Darwin) bind=127.0.0.1 ;;
    *) bind="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')" ;;
    esac
    rm -rf "$work/data"
    "$work/heimdb" -addr "$bind:$HEIMDB_PORT" -data "$work/data" -db wordpress -root-password "$HEIMDB_ROOT_PASSWORD" -debug >"$work/heimdb.log" 2>&1 &
    pid=$!
    for _ in $(seq 100); do
      grep -q "Server ready" "$work/heimdb.log" && break
      sleep 0.2
    done
  else
    compose up -d db >/dev/null 2>&1
    for _ in $(seq 120); do
      mysql_db -e 'SELECT 1' >/dev/null && break
      sleep 1
    done
  fi
  compose up -d wordpress >/dev/null 2>&1
  for _ in $(seq 60); do
    curl -s -o /dev/null "http://127.0.0.1:$WP_PORT/" && break
    sleep 1
  done
  wp core install --url="http://127.0.0.1:$WP_PORT" --title=compare --admin_user=admin \
    --admin_password=admin --admin_email=admin@example.com --skip-email >/dev/null
  wp rewrite structure '/%postname%/' >/dev/null
  # 準備の後のエラーだけを数える
  local start=0
  if [ "$db" = heimdb ]; then
    start=$(wc -l <"$work/heimdb.log")
  else
    mysql_db -e "TRUNCATE performance_schema.events_statements_history_long"
  fi
  login
  # シナリオの中の失敗（プラグインのエラーなど）では止めない。比べるのはエラーになった文
  set +e
  # shellcheck disable=SC1090
  source "$scenario"
  set -e
  if [ "$db" = heimdb ]; then
    tail -n +"$((start + 1))" "$work/heimdb.log" | python3 "$here/errors.py" --tsv >"$work/heimdb.tsv"
    if grep -qE '^panic:|recovered from a panic' "$work/heimdb.log"; then
      echo "HeimDB のログに panic がある" >&2
      exit 1
    fi
    # 文のエラーのほかの警告（書き込みのロックの待ちの時間切れ、何もしない接続を切った、統計を作れない、など）
    tail -n +"$((start + 1))" "$work/heimdb.log" | grep -E 'level=(warning|error)' | grep -v 'error running query' | sed 's/^/HeimDB の警告: /' | head -20 || true
  else
    # -r: 文のバックスラッシュをエスケープせずに出す（HeimDB 側と字句で比べる）
    mysql_db -N -r -e "SELECT COUNT(*), CONCAT(MYSQL_ERRNO, ' ', MESSAGE_TEXT), LEFT(REGEXP_REPLACE(TRIM(SQL_TEXT), '[[:space:]]+', ' '), 300)
      FROM performance_schema.events_statements_history_long
      WHERE MYSQL_ERRNO <> 0 AND CURRENT_SCHEMA = 'wordpress'
        AND NOT (MYSQL_ERRNO = 1146 AND REGEXP_LIKE(TRIM(SQL_TEXT), '^(DESCRIBE|DESC|SHOW|SELECT 1 FROM|SELECT COUNT)', 'i'))
      GROUP BY 2, 3 ORDER BY 1 DESC" >"$work/mysql.tsv"
    # 記録は 20 万文まで（docker-compose.mysql.yml）。あふれると古い文のエラーが黙って落ちる
    n=$(mysql_db -N -e "SELECT COUNT(*) FROM performance_schema.events_statements_history_long")
    if [ "$n" -ge 200000 ]; then
      echo "MySQL の文の記録があふれた（$n 文）。シナリオを分ける" >&2
    fi
  fi
  stop
}

case "${DB:-both}" in
both) run heimdb; run mysql ;;
heimdb | mysql) run "$DB" ;;
*)
  echo "DB は heimdb か mysql" >&2
  exit 2
  ;;
esac

# 両方のエラーを出し、HeimDB でだけエラーになった文（文の字句で比べる。末尾の ; は除く）を出す
python3 - "$work" <<'EOF'
import os, sys
work = sys.argv[1]
def load(db):
    p = os.path.join(work, db + ".tsv")
    if not os.path.exists(p):
        return None
    rows = []
    for line in open(p, encoding="utf-8", errors="replace"):
        n, err, query = (line.rstrip("\n").split("\t") + ["", "", ""])[:3]
        rows.append((n, err, query.rstrip("; ")))
    return rows
result = {db: load(db) for db in ("heimdb", "mysql")}
for db, rows in result.items():
    if rows is None:
        continue
    print(f"=== エラー（{db}）: {len(rows)} 種類")
    for n, err, query in rows:
        print(f"  {n} 回: {err}\n      文: {query}")
if result["heimdb"] is not None and result["mysql"] is not None:
    both = {q for _, _, q in result["mysql"]}
    only = [r for r in result["heimdb"] if r[2] not in both]
    print(f"=== HeimDB でだけエラー: {len(only)} 種類")
    for n, err, query in only:
        print(f"  {n} 回: {err}\n      文: {query}")
    sys.exit(1 if only else 0)
sys.exit(1 if result["heimdb"] else 0)
EOF
