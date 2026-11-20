#!/bin/bash
# プラグインを HeimDB の上の WordPress で有効にして、HeimDB が拒否した文を集める（docs/plugins.md、docs/plan.md の段階 56）。
#
#   scripts/plugin-check/activate.sh redirection wp-cerber                # wordpress.org の最新バージョン
#   scripts/plugin-check/activate.sh ~/sites/x/wp-content/plugins/acfml   # 手元のディレクトリか zip
#   REQUIRES=woocommerce scripts/plugin-check/activate.sh woocommerce-payments   # 前提のプラグインを先に有効にしておく
#   REQUIRES=~/sites/x/wp-content/plugins/sitepress-multilingual-cms scripts/plugin-check/activate.sh ~/sites/x/wp-content/plugins/acfml
#
# このリポジトリの HeimDB を建てて HEIMDB_PORT（既定 3336。WordPress のコンテナから届くアドレスだけ）で動かし、WordPress（127.0.0.1:WP_PORT、既定 18090）を
# 入れる。プラグインごとに、有効にして、トップページと管理画面を開き、予定の cron を流し、その間に HeimDB のログに
# 出た文のエラーを出してから、無効に戻す。終わったら、WordPress のコンテナとボリューム、HeimDB とそのデータを消す。
#
# 有効にしただけでは表を作らないプラグイン（設定の画面で初めて作る、機能を有効にしたときに作るもの）もあるので、
# check.py の静的な検査と合わせて判定する。REQUIRES（空白区切りのスラッグか手元のディレクトリ）のプラグインは最初に有効にし、最後まで
# 有効のままにする（その間のエラーは前提の分として先に出す）。
set -euo pipefail

: "${HEIMDB_PORT:=3336}"
: "${WP_PORT:=18090}"
: "${HEIMDB_ROOT_PASSWORD:=plugin-check}"
export HEIMDB_PORT WP_PORT HEIMDB_ROOT_PASSWORD
if [ "$HEIMDB_PORT" = 3306 ]; then
  echo "HEIMDB_PORT に 3306 は使わない" >&2
  exit 2
fi
if [ $# -eq 0 ]; then
  sed -n '2,11p' "$0" >&2
  exit 2
fi

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/heimdb-plugin-check.XXXXXX")"
export PLUGIN_DIR="$work/plugins"
mkdir -p "$PLUGIN_DIR"
compose() { docker compose -f "$here/docker-compose.yml" "$@"; }
wp() { compose run --rm -T cli wp "$@"; }

pid=""
cleanup() {
  compose --profile cli down -v >/dev/null 2>&1 || true
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    while kill -0 "$pid" 2>/dev/null; do sleep 0.2; done
  fi
  rm -rf "$work"
}
trap cleanup EXIT

if lsof -nP -iTCP:"$HEIMDB_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "ポート $HEIMDB_PORT はもう使われている（HEIMDB_PORT で変える）" >&2
  exit 1
fi
(cd "$root" && go build -o "$work/heimdb" ./cmd/heimdb)
# HeimDB は WordPress のコンテナから届くところだけで待ち受ける（LAN に開けない）。Docker Desktop（macOS）は
# host.docker.internal をホストの 127.0.0.1 に届けるので 127.0.0.1、Linux は host-gateway（既定のブリッジ）のアドレス。
case "$(uname)" in
Darwin) bind=127.0.0.1 ;;
*) bind="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true)" ;;
esac
if [ -z "$bind" ]; then
  echo "Docker の既定のブリッジのアドレスがわからない" >&2
  exit 1
fi
"$work/heimdb" -addr "$bind:$HEIMDB_PORT" -data "$work/data" -db wordpress -root-password "$HEIMDB_ROOT_PASSWORD" -debug >"$work/heimdb.log" 2>&1 &
pid=$!
for _ in $(seq 100); do
  grep -q "Server ready" "$work/heimdb.log" && break
  sleep 0.2
done

compose up -d wordpress >/dev/null 2>&1
for _ in $(seq 60); do
  curl -s -o /dev/null "http://127.0.0.1:$WP_PORT/" && break
  sleep 1
done
wp core install --url="http://127.0.0.1:$WP_PORT" --title=plugin-check --admin_user=admin \
  --admin_password=admin --admin_email=admin@example.com --skip-email >/dev/null

# login は管理画面を開くための Cookie を jar に入れる。
jar="$work/cookies"
login() {
  curl -s -c "$jar" -b "$jar" -o /dev/null "http://127.0.0.1:$WP_PORT/wp-login.php"
  curl -s -c "$jar" -b "$jar" -o /dev/null --data "log=admin&pwd=admin&wp-submit=Log+In&testcookie=1" \
    "http://127.0.0.1:$WP_PORT/wp-login.php"
}

# install_source は、引数（スラッグ、手元のディレクトリか zip）を wp plugin install に渡す形にし、name に名前を入れる。
install_source() {
  local arg="$1"
  if [ -e "$arg" ]; then
    name="$(basename "${arg%.zip}")"
    if [ -d "$arg" ]; then
      (cd "$(dirname "$arg")" && zip -qr "$PLUGIN_DIR/$name.zip" "$(basename "$arg")")
    else
      cp "$arg" "$PLUGIN_DIR/$name.zip"
    fi
    source="/plugins/$name.zip"
  else
    name="$arg"
    source="$arg"
  fi
}

failed=0
if [ -n "${REQUIRES:-}" ]; then
  start=$(wc -l <"$work/heimdb.log")
  for arg in $REQUIRES; do
    install_source "$arg"
    wp plugin install "$source" --activate >/dev/null 2>&1 || true
  done
  login
  for path in / /wp-admin/ /wp-admin/plugins.php; do
    curl -s -b "$jar" -o /dev/null "http://127.0.0.1:$WP_PORT$path" || true
  done
  wp cron event run --due-now >/dev/null 2>&1 || true
  echo "前提（${REQUIRES}）: 有効なもの $(wp plugin list --status=active --field=name 2>/dev/null | tr '\n' ' ')"
  tail -n +"$((start + 1))" "$work/heimdb.log" | python3 "$here/errors.py" || failed=1
fi
for arg in "$@"; do
  install_source "$arg"
  start=$(wc -l <"$work/heimdb.log")
  out="$(wp plugin install "$source" --activate 2>&1 || true)"
  login
  for path in / /wp-admin/ /wp-admin/plugins.php /wp-admin/options-general.php; do
    curl -s -b "$jar" -o /dev/null "http://127.0.0.1:$WP_PORT$path" || true
  done
  wp cron event run --due-now >/dev/null 2>&1 || true
  active="$(wp plugin list --status=active --field=name 2>/dev/null | tr '\n' ' ')"
  refused=0
  errors="$(tail -n +"$((start + 1))" "$work/heimdb.log" | python3 "$here/errors.py")" || refused=1
  version="$(wp plugin get "$name" --field=version 2>/dev/null || echo '?')"
  status="有効"
  case " $active " in *" $name "*) ;; *) status="有効にならなかった" ;; esac
  if [ "$refused" = 1 ]; then
    echo "$name ${version}: ${status}、HeimDB が拒否した文あり"
    printf '%s\n' "$errors"
    failed=1
  else
    echo "$name ${version}: ${status}、HeimDB が拒否した文なし"
    [ -n "$errors" ] && printf '%s\n' "$errors"
  fi
  if [ "$status" != "有効" ]; then
    printf '%s\n' "$out" | tail -3 | sed 's/^/  wp: /'
  fi
  # 文のエラーのほかの警告（書き込みのロックの待ちの時間切れ、何もしない接続を切った、統計を作れない、など）
  tail -n +"$((start + 1))" "$work/heimdb.log" | grep -E 'level=(warning|error)' | grep -v 'error running query' | sed 's/^/  HeimDB の警告: /' | head -10 || true
  # 次のプラグインの確かめに混ざらないよう、無効に戻す（作った表は残る）
  wp plugin deactivate "$name" >/dev/null 2>&1 || true
done
# Go の panic（行頭の panic:）と、文の panic を回復したときのログだけを見る（文の中の panic という語は数えない）
grep -qE '^panic:|recovered from a panic' "$work/heimdb.log" && { echo "HeimDB のログに panic がある" >&2; failed=1; }
exit "$failed"
