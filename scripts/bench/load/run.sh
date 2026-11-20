#!/bin/bash
# 負荷の測り（docs/bench.md の段階 126）を、負荷のインスタンスの ~/l で流す。DB のインスタンスの dbctl.sh を ssh で呼んで
# サーバーを入れ替え、ここの loadgen で負荷をかける。結果は ~/l/res/<名前>.json（loadgen）、<名前>.load.csv（このホストの CPU）、
# DB の側の ~/b/res/<名前>.cpu.csv（サーバーのプロセスの CPU と RSS）と <名前>.stats-{before,after}.txt。
#
#   DB=10.146.0.40 ./run.sh A              同時の接続の数を 1〜128 に変える（読みだけ、閉じた形、60 秒）。MySQL と HeimDB を 2 回ずつ交互に
#   DB=... ./run.sh B <MySQL の最大>       その 25、50、75% の決まった速さ（ページ毎秒）。2 回ずつ交互に
#   DB=... ./run.sh C                      読み:書き込み 95:5 と 80:20、32 接続。2 回ずつ交互に
#   DB=... ./run.sh D <MySQL の最大> <HeimDB の最大>  それぞれの最大の 150% の速さと、500 接続
#   DB=... ./run.sh E <MySQL の最大>       その 50% の速さで 30 分
#   DB=... ./run.sh R                      参考: MySQL の innodb_buffer_pool_size=2G で A を 1 回
#   DB=... ./run.sh all                    A の後、A の最大（2 回の平均の、接続の数ごとの最大）で B、C、D、E、R を続けて流す（約 3 時間半）
#
# 測りのたびにデータを入れ直し（読みだけの A と B は、サーバーを立てるたび）、8 接続で 30 秒流して温め、loadgen も 10 秒温めてから測る。
set -uo pipefail
cd ~/l
mkdir -p res
: "${DB:?set DB to the address of the database instance}"
DUR=${DUR:-60s}

dbc() { ssh -o StrictHostKeyChecking=no -o BatchMode=yes "$DB" "$@"; }
dsn() {
	case $1 in
	mysql) echo "root:root@tcp($DB:3307)/wpbench" ;;
	heimdb) echo "root:rootpw@tcp($DB:3316)/wpbench" ;;
	esac
}

start() { # システム [サーバーの引数...]
	local sys=$1
	shift
	dbc "~/b/dbctl.sh start-$sys $*" | tee -a res/servers.log
	./loadgen -dsn "$(dsn "$sys")" -pages pages-read.json -conns 8 -duration 30s -warmup 0s -interval 0 -label warm -out /dev/null 2>> res/warm.log
}

one() { # 名前 システム loadgen の引数...
	local name=$1 sys=$2
	shift 2
	dbc "~/b/dbctl.sh stats" > res/$name.stats-before.txt 2>&1
	dbc "~/b/dbctl.sh sample-start $name"
	python3 sample.py --pid 0 --out res/$name.load.csv &
	local sp=$!
	./loadgen -dsn "$(dsn "$sys")" -label "$name" -out res/$name.json "$@" 2>&1 | tee -a res/summary.log
	kill $sp 2>/dev/null
	wait $sp 2>/dev/null
	dbc "~/b/dbctl.sh sample-stop"
	dbc "~/b/dbctl.sh stats" > res/$name.stats-after.txt 2>&1
}

sweep() { # 接頭辞 システム
	for c in 1 2 4 8 16 32 64 128; do
		one "$1-c$c" "$2" -pages pages-read.json -conns $c -duration $DUR -warmup 10s
	done
}

maxof() { # システム: A の接続の数ごとの 2 回の平均のうち、最大のページ毎秒
	python3 - "$1" <<'PY'
import glob, json, statistics, sys
best = 0
for c in (1, 2, 4, 8, 16, 32, 64, 128):
    v = [json.load(open(f))["pages_per_s"] for f in glob.glob(f"res/A-{sys.argv[1]}-r*-c{c}.json")]
    if v:
        best = max(best, statistics.mean(v))
print(round(best, 2))
PY
}

rate() { python3 -c "import sys; print(round(float(sys.argv[1]) * float(sys.argv[2]), 2))" "$1" "$2"; }

case "$1" in
A)
	for r in 1 2; do
		for s in mysql heimdb; do
			start $s
			sweep "A-$s-r$r" $s
		done
	done
	;;
B)
	MAX=$2
	for r in 1 2; do
		for s in mysql heimdb; do
			start $s
			for p in 25 50 75; do
				one "B-$s-r$r-p$p" $s -pages pages-read.json -conns 128 -rate "$(rate "$MAX" 0.$p)" -duration $DUR -warmup 10s
			done
		done
	done
	;;
C)
	for r in 1 2; do
		for s in mysql heimdb; do
			for w in 5 20; do
				start $s
				one "C-$s-r$r-w$w" $s -pages pages-read.json,pages-write.json -write "$(rate $w 0.01)" -conns 32 -duration $DUR -warmup 10s
			done
		done
	done
	;;
D)
	start mysql
	one "D-mysql-rate150" mysql -pages pages-read.json -conns 128 -rate "$(rate "$2" 1.5)" -duration $DUR -warmup 10s
	start heimdb
	one "D-heimdb-rate150" heimdb -pages pages-read.json -conns 128 -rate "$(rate "$3" 1.5)" -duration $DUR -warmup 10s
	start mysql
	one "D-mysql-c500" mysql -pages pages-read.json -conns 500 -duration $DUR -warmup 10s
	start mysql --max-connections=1000
	one "D-mysqlmc1000-c500" mysql -pages pages-read.json -conns 500 -duration $DUR -warmup 10s
	start heimdb
	one "D-heimdb-c500" heimdb -pages pages-read.json -conns 500 -duration $DUR -warmup 10s
	;;
E)
	R=$(rate "$2" 0.5)
	start mysql
	one "E-mysql" mysql -pages pages-read.json -conns 64 -rate "$R" -duration 30m -warmup 10s -interval 60s
	dbc "HEIMDB_GODEBUG=gctrace=1 ~/b/dbctl.sh start-heimdb" | tee -a res/servers.log
	./loadgen -dsn "$(dsn heimdb)" -pages pages-read.json -conns 8 -duration 30s -warmup 0s -interval 0 -label warm -out /dev/null 2>> res/warm.log
	one "E-heimdb" heimdb -pages pages-read.json -conns 64 -rate "$R" -duration 30m -warmup 10s -interval 60s
	;;
R)
	start mysql --innodb-buffer-pool-size=2G
	sweep "R-mysqlbp2g" mysql
	;;
all)
	"$0" A > run-A.log 2>&1
	MM=$(maxof mysql)
	HM=$(maxof heimdb)
	echo "mysql_max=$MM heimdb_max=$HM" | tee res/maxima.txt
	"$0" B "$MM" > run-B.log 2>&1
	"$0" C > run-C.log 2>&1
	"$0" D "$MM" "$HM" > run-D.log 2>&1
	"$0" E "$MM" > run-E.log 2>&1
	"$0" R > run-R.log 2>&1
	;;
*)
	echo "usage: $0 A|B max|C|D mysql-max heimdb-max|E max|R|all" >&2
	exit 2
	;;
esac
dbc "~/b/dbctl.sh stop"
echo "done $1"
