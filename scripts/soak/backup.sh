#!/bin/bash
# 長時間の負荷試験のバックアップ（docs/bench.md の「段階 108」）。HeimDB と同じマシンで、決まった間隔で
# heimdb_backup を取り、新しい 2 つだけを残す（古い sstable をハードリンクで持ち続けないように）。
#
#   SOAK_DIR=/srv/soak scripts/soak/backup.sh 60 25 >> out/backup.log   # 60 分ごと、25 時間
set -uo pipefail
D=${SOAK_DIR:?SOAK_DIR}
every=${1:-60}; hours=${2:-25}
end=$(awk "BEGIN { print int($(date +%s) + $hours * 3600) }")
while [ "$(date +%s)" -lt "$end" ]; do
  sleep $(( every * 60 ))
  name=soak-$(date -u +%Y%m%dT%H%M%S)
  t0=$(date +%s.%N)
  res=$(mysql -h127.0.0.1 -P"${SOAK_PORT:-3356}" -uroot -prootpw -N -B -e "CALL heimdb_backup('$name')" 2>&1 | grep -v Warning)
  t1=$(date +%s.%N)
  ls -1d "$D"/backups/soak-* 2>/dev/null | sort -r | tail -n +3 | while read -r old; do rm -rf "$old"; done
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%T)" "$name" "$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%.2f", b - a }')" "$res"
done
