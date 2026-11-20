#!/bin/bash
# 長時間の負荷試験の終わりの確かめ（docs/bench.md の「段階 108」）。負荷、書き込み、ブランチの一巡を止めた後に、
# HeimDB と同じマシンで流す。結果は OUT に置く。
#
#   1. レプリカが追いつくのを待ち、本番とレプリカの mysqldump、ブランチの一覧、ユーザー、AS OF の読みを比べる
#   2. 本番で数え直しの検査（check.sql）を流し、試験の前の結果と比べる
#   3. 本番とレプリカを止めて起動し、つながるまでと統計がそろうまでの時間、前後のディスクの大きさを測る
#      （db.sh start と同じ SOAK_RETENTION などを付けて流す。付けないと既定の値で起動する）
#   4. いちばん新しいバックアップの写しを別のポートで開き、数え直しの検査を流す
#
#   SOAK_DIR=/srv/soak OUT=out scripts/soak/final.sh ./heimdb out/check-base.txt
#
# AS OF で読むのは SOAK_ASOF_MINUTES 分前（既定 30。保持期間より短くする）。
export LC_ALL=C
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
D=${SOAK_DIR:?SOAK_DIR}; O=${OUT:?OUT}
bin=${1:?heimdb のバイナリ}; base=${2:?試験の前の check.sql の結果}
my() { mysql -h127.0.0.1 -P"$1" -uroot -prootpw -N -B "${@:2}" 2>&1 | grep -v 'Using a password'; }
status() { my "$1" -e 'CALL heimdb_status()' | awk -F'\t' -v k="$2" '$1 == k { print $2 }'; }
log() { echo "$(date -u +%T) $*" | tee -a "$O/final.txt"; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }

log "1. replica"
for _ in $(seq 300); do [ "$(status 3357 replication_behind)" = 0s ] && [ "$(status 3356 open_transactions)" = 0 ] && break; sleep 1; done
sleep 5
log "behind=$(status 3357 replication_behind) sync=$(status 3356 replication_sync) open=$(status 3356 open_transactions)"
for p in 3356 3357; do
  mysqldump -h127.0.0.1 -P$p -uroot -prootpw --single-transaction --skip-dump-date --order-by-primary --skip-extended-insert wpbench \
    >"$O/dump-$p.sql" 2>"$O/dump-$p.err"
  my $p -e 'CALL heimdb_branch_list()' | cut -f1,2 >"$O/branches-$p.txt"
  my $p -e 'SELECT user, host FROM mysql.user ORDER BY user, host' >"$O/users-$p.txt"
done
at=$(my 3356 -e "SELECT DATE_FORMAT(UTC_TIMESTAMP() - INTERVAL ${SOAK_ASOF_MINUTES:-30} MINUTE, '%Y-%m-%d %H:%i:%s+00:00')")
for p in 3356 3357; do
  my $p wpbench -e "SELECT COUNT(*), MAX(comment_ID), SUM(comment_post_ID) FROM wp_comments AS OF '$at'; SELECT COUNT(*), MAX(ID) FROM wp_posts AS OF '$at'" >"$O/asof-$p.txt"
done
log "dump lines $(wc -l <"$O/dump-3356.sql") / $(wc -l <"$O/dump-3357.sql"), differing lines $(diff "$O/dump-3356.sql" "$O/dump-3357.sql" | grep -c '^[<>]')"
log "branches $(cmp -s "$O/branches-3356.txt" "$O/branches-3357.txt" && echo same || echo DIFFERENT), users $(cmp -s "$O/users-3356.txt" "$O/users-3357.txt" && echo same || echo DIFFERENT), as of $at $(cmp -s "$O/asof-3356.txt" "$O/asof-3357.txt" && echo same || echo DIFFERENT)"

log "2. check"
my 3356 wpbench <"$here/check.sql" | sort >"$O/check-end.txt"
log "check $(diff -q <(sort "$base") "$O/check-end.txt" >/dev/null && echo same as before || echo DIFFERENT)"

log "3. restart"
before=$(du -sk "$D/primary" | cut -f1)
SOAK_DIR=$D "$here/db.sh" stop
t0=$(date +%s.%N)
if ! SOAK_DIR=$D "$here/db.sh" restart "$bin" >>"$O/final.txt" 2>&1; then
  log "RESTART_FAILED"; exit 1
fi
t1=$(date +%s.%N)
for _ in $(seq 600); do [ "$(status 3356 stats)" = ready ] && break; sleep 0.2; done
t2=$(date +%s.%N)
log "accepting after $(secs "$t0" "$t1") s (both, one after the other), stats ready after $(secs "$t0" "$t2") s; primary data ${before}K -> $(du -sk "$D/primary" | cut -f1)K"
log "stats_build_duration=$(status 3356 stats_build_duration) stats_tables=$(status 3356 stats_tables)"

log "4. backup"
last=$(ls -1d "$D"/backups/soak-* 2>/dev/null | sort | tail -1)
if [ -n "$last" ]; then
  rm -rf "$D/backup-open"; cp -a "$last" "$D/backup-open"
  "$bin" -addr 127.0.0.1:3358 -data "$D/backup-open" -db wpbench >"$O/backup-open.log" 2>&1 &
  bp=$!
  for _ in $(seq 120); do my 3358 -e 'SELECT 1' >/dev/null && break; sleep 1; done
  my 3358 wpbench <"$here/check.sql" | sort >"$O/check-backup.txt"
  log "backup $(basename "$last") opened, check $(diff -q <(sort "$base") "$O/check-backup.txt" >/dev/null && echo same as before || echo DIFFERENT)"
  kill $bp; wait $bp 2>/dev/null
  rm -rf "$D/backup-open"
fi
log done
