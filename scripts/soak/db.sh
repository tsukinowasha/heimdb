#!/bin/bash
# 長時間の負荷試験の HeimDB（docs/bench.md の「段階 108」）。本番とレプリカを同じマシンで動かす。
#
#   SOAK_DIR=/srv/soak scripts/soak/db.sh start ./heimdb wpbench.sql   # データを入れ、レプリカを作って起動する
#   SOAK_DIR=/srv/soak scripts/soak/db.sh restart ./heimdb             # 同じデータで起動し直す
#   SOAK_DIR=/srv/soak scripts/soak/db.sh bounce-replica ./heimdb 300   # レプリカを 5 分止めてから起動し直す
#   SOAK_DIR=/srv/soak scripts/soak/db.sh stop
#
# GC が働く状態に早く入るように、保持期間を短くする（既定 2 時間）。レプリカのための記録も同じ長さにする
# （既定の 24 時間のままだと、記録の分だけディスクが伸び続ける）。レプリカにも同じ保持期間を付ける（GC は複製しない）。
set -euo pipefail
D=${SOAK_DIR:?SOAK_DIR にデータとログの置き場所を渡す}
RET=${SOAK_RETENTION:-2h}
GCI=${SOAK_GC_INTERVAL:-15m}
MEM=${SOAK_MEMORY_LIMIT:-2G}
CACHE=${SOAK_CACHE_SIZE:-268435456}
P1=${SOAK_PORT:-3356}
P2=${SOAK_REPLICA_PORT:-3357}
RP=${SOAK_REPLICATION_PORT:-18095}
M1=${SOAK_METRICS_PORT:-19104}
M2=${SOAK_REPLICA_METRICS_PORT:-19105}
BIND=${SOAK_BIND:-127.0.0.1}
export HEIMDB_REPLICATION_TOKEN=${HEIMDB_REPLICATION_TOKEN:-soak-token}
common=(-retention "$RET" -gc-interval "$GCI" -replication-log-retention "$RET" -memory-limit "$MEM" -cache-size "$CACHE" -slow-query 200ms)

my() { mysql -h127.0.0.1 -P"$1" -uroot -prootpw "${@:2}" 2>/dev/null; }
wait_up() { for _ in $(seq 120); do my "$1" -e 'SELECT 1' >/dev/null && return; sleep 1; done; echo "port $1 did not come up" >&2; exit 1; }

start_primary() {
  HEIMDB_PPROF_ADDR=127.0.0.1:${SOAK_PPROF_PORT:-19106} "$1" -addr "$BIND:$P1" -data "$D/primary" -db wpbench -root-password rootpw \
    -backup-dir "$D/backups" -replication-addr "127.0.0.1:$RP" -metrics-addr "127.0.0.1:$M1" "${common[@]}" >>"$D/primary.log" 2>&1 &
  echo $! >"$D/primary.pid"
  wait_up "$P1"
}

start_replica() {
  HEIMDB_PPROF_ADDR=127.0.0.1:${SOAK_REPLICA_PPROF_PORT:-19107} "$1" -addr "127.0.0.1:$P2" -data "$D/replica" -db wpbench \
    -replicate-from "127.0.0.1:$RP" -replica-name soak-replica -metrics-addr "127.0.0.1:$M2" "${common[@]}" >>"$D/replica.log" 2>&1 &
  echo $! >"$D/replica.pid"
  wait_up "$P2"
}

stop_one() {
  local f=$D/$1.pid
  [ -f "$f" ] || return 0
  local pid; pid=$(cat "$f")
  if kill -0 "$pid" 2>/dev/null && ps -p "$pid" -o command= | grep -q -- "-data $D/$1"; then
    kill "$pid"
    for _ in $(seq 60); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    if kill -0 "$pid" 2>/dev/null; then
      echo "$1 ($pid) did not stop in 60 s; killing it" >&2
      kill -KILL "$pid"
    fi
  fi
  rm -f "$f"
}

case ${1:-} in
start)
  bin=${2:?heimdb のバイナリ}; sql=${3:?wpbench.sql}
  [ ! -e "$D/primary" ] || { echo "$D/primary already exists" >&2; exit 1; }
  mkdir -p "$D/backups"
  start_primary "$bin"
  my "$P1" wpbench <"$sql"
  # 管理者のパスワード（WordPress は MD5 の古い形を受け付け、ログインのときに作り直す）
  my "$P1" wpbench -e "UPDATE wp_users SET user_pass = MD5('adminpw') WHERE user_login = 'admin'"
  my "$P1" -N -e "CALL heimdb_backup('replica-seed')" >/dev/null
  mv "$D/backups/replica-seed" "$D/replica"
  start_replica "$bin"
  echo "primary $(cat "$D/primary.pid") on $P1, replica $(cat "$D/replica.pid") on $P2"
  ;;
restart)
  bin=${2:?heimdb のバイナリ}
  stop_one replica; stop_one primary
  start_primary "$bin"; start_replica "$bin"
  echo "primary $(cat "$D/primary.pid") on $P1, replica $(cat "$D/replica.pid") on $P2"
  ;;
bounce-replica)
  # レプリカを止めて、間を置いて起動し直す（追いつけるかを見る）
  bin=${2:?heimdb のバイナリ}
  stop_one replica; sleep "${3:-300}"; start_replica "$bin"
  echo "replica $(cat "$D/replica.pid") on $P2"
  ;;
stop)
  stop_one replica; stop_one primary
  ;;
*)
  echo "usage: $0 start|restart|bounce-replica|stop" >&2; exit 2 ;;
esac
