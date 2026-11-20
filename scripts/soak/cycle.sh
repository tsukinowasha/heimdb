#!/bin/bash
# 長時間の負荷試験のブランチの一巡（docs/bench.md の「段階 108」）。決まった間隔で、本番からブランチを作り、
# WordPress（wp-cli）でブランチに書き、差分を見てマージし、ブランチを消す。マージの後に本番の数え直しの検査
# （check.sql）を流し、試験の前の結果（base）と違えば CHECK_DIFF を出す（検査の文が失敗したら CHECK_ERROR）。
# 5 秒おいた 2 回の読みの両方にある行だけを比べるので、2 回の間に数が変わり続ける違いは見落としうる。
#
#   SOAK_MYSQL=127.0.0.1:3356 scripts/soak/cycle.sh 30 24 out/check-base.txt >> out/cycle.log   # 30 分ごと、24 時間
set -uo pipefail
export LC_ALL=C
here=$(cd "$(dirname "$0")" && pwd)
C=$here/compose.yml
every=${1:-30}; hours=${2:-24}; base=${3:?試験の前の check.sql の結果}
hp=${SOAK_MYSQL:-127.0.0.1:3356}
my() {
  local o s
  o=$(mysql -h"${hp%:*}" -P"${hp#*:}" -uroot -prootpw -N -B "$@" 2>&1); s=$?
  printf '%s\n' "$o" | grep -v 'Using a password' || true
  return $s
}
# 動かなくなった wp を 10 分で止める（timeout のない macOS ではそのまま流す）
to() { if command -v timeout >/dev/null; then timeout "$@"; else shift; "$@"; fi; }
wpc() { local db=$1; shift; to 600 docker compose -f "$C" run --rm -T -e WORDPRESS_DB_NAME="$db" -e SOAK_TTL -e SOAK_BRANCH_MODE cli wp "$@" 2>&1 | tail -3; }
log() { printf '%s\t%s\n' "$(date -u +%T)" "$*"; }

# 書き込みの途中を見て違うと出さないように、5 秒おいて 2 回とも違う行だけを残す
check() {
  local a b
  a=$(my wpbench <"$here/check.sql" | sort) || { echo "  $a"; return 2; }
  sleep 5
  b=$(my wpbench <"$here/check.sql" | sort) || { echo "  $b"; return 2; }
  comm -12 <(echo "$a") <(echo "$b") | diff <(sort "$base") - >/dev/null && return 0
  comm -12 <(echo "$a") <(echo "$b") | diff <(sort "$base") - | sed 's/^/  /'
  return 1
}

end=$(awk "BEGIN { print int($(date +%s) + $hours * 3600) }")
while [ "$(date +%s)" -lt "$end" ]; do
  sleep $(( every * 60 ))
  name=b$(date -u +%m%d%H%M%S)
  t0=$(date +%s)
  out=$(my -e "CALL heimdb_branch_create('$name')") || { log "$name CREATE_FAILED $out"; continue; }
  log "$name created"
  log "$name $(SOAK_BRANCH_MODE=write wpc "wpbench@$name" eval-file /soak/branch.php)"
  log "$name diff $(my -e "CALL heimdb_branch_diff('$name')" | awk '{printf "%s ", $0}')"
  if out=$(my -e "CALL heimdb_branch_merge('$name')"); then
    log "$name merged $(echo "$out" | awk '{printf "%s ", $0}')"
  else
    log "$name MERGE_FAILED $out"
  fi
  if out=$(my -e "CALL heimdb_branch_delete('$name')"); then log "$name deleted"; else log "$name DELETE_FAILED $out"; fi
  log "$name $(SOAK_BRANCH_MODE=cleanup wpc wpbench eval-file /soak/branch.php)"
  c=$(check); rc=$?
  case $rc in
  0) log "$name check ok (cycle $(( $(date +%s) - t0 ))s)" ;;
  2) log "$name CHECK_ERROR"; echo "$c" ;;
  *) log "$name CHECK_DIFF"; echo "$c" ;;
  esac
done
