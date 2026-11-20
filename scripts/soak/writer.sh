#!/bin/bash
# 長時間の負荷試験の書き込み（docs/bench.md の「段階 108」）。毎分 writer.php と予定の cron を流す。
#
#   scripts/soak/writer.sh 24 >> out/writer.log   # 24 時間
set -uo pipefail
C=$(cd "$(dirname "$0")" && pwd)/compose.yml
hours=${1:-24}
# 動かなくなった wp を 10 分で止める（timeout のない macOS ではそのまま流す）
to() { if command -v timeout >/dev/null; then timeout "$@"; else shift; "$@"; fi; }
end=$(awk "BEGIN { print int($(date +%s) + $hours * 3600) }")
while [ "$(date +%s)" -lt "$end" ]; do
  t0=$(date +%s)
  to 600 docker compose -f "$C" run --rm -T -e SOAK_TTL -e SOAK_COMMENTS -e SOAK_TRANSIENTS cli wp eval-file /soak/writer.php 2>&1 | tail -3
  to 600 docker compose -f "$C" run --rm -T cli wp cron event run --due-now 2>&1 | grep -v "^Executed\|^Success" | tail -3
  sleep $(( 60 - ( $(date +%s) - t0 ) % 60 ))
done
