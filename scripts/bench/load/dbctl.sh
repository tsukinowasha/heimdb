#!/bin/bash
# 負荷の測り（docs/bench.md の段階 126）の DB のインスタンスで、サーバーを入れ替え、データを入れ直し、CPU とメモリを見る。
# 負荷のインスタンスの run.sh が ssh で呼ぶ。サーバーは 1 つずつ動かす（測らない方は止める）。
#
#   dbctl.sh start-mysql [mysqld の引数...]   MySQL 8.0 を素の設定で立て直し（コンテナごと作り直す）、ダンプを入れる。0.0.0.0:3307
#   dbctl.sh start-heimdb [heimdb の引数...]  HeimDB を空のデータで立て、ダンプを入れる。0.0.0.0:3316、指標は 127.0.0.1:9104
#                                             （HEIMDB_GODEBUG=gctrace=1 を付けると、GC ごとの行をログに書く）
#                                             SEED=<name> copies ~/b/seed-<name> (make-seed) instead of loading the dump.
#                                             BK=same|other|enc adds -backup-dir on the data's filesystem (~/b/bk), on the
#                                             second disk (/mnt/bk/bk), or on the data's filesystem with -backup-key bench
#   dbctl.sh make-seed <name> [<MB>]          load the dump, grow the store to about <MB> MB with wp_ballast (rows that no
#                                             page reads), stop, and keep the data directory as ~/b/seed-<name>
#   dbctl.sh stop                             両方を止める
#   dbctl.sh pid                              動いているサーバーの PID（mysqld か heimdb）
#   dbctl.sh sample-start <名前>              ~/b/res/<名前>.cpu.csv に 1 秒ごとの CPU と RSS（HeimDB は指標も <名前>.metrics.txt に）
#   dbctl.sh sample-stop
#   dbctl.sh stats                            行の鍵の待ちと接続の数（MySQL は SHOW GLOBAL STATUS、HeimDB は指標）
#   dbctl.sh capture-writes                   MySQL の上の WordPress で書き込みの文を取る（~/b/res/writes-capture.json）
#
# パスワードは scripts/bench と同じ使い捨ての値（MySQL は root、HeimDB は rootpw）。
set -uo pipefail
B=~/b
mkdir -p $B/res $B/log
MYSQL_IMAGE=mysql:8.0.46

client() { sudo docker run --rm -i --network host $MYSQL_IMAGE mysql -h127.0.0.1 "$@"; }

stop_all() {
	sudo docker rm -f realmysql >/dev/null 2>&1
	if [ -f $B/heimdb.pid ]; then
		local p
		p=$(cat $B/heimdb.pid)
		kill "$p" 2>/dev/null
		for _ in $(seq 60); do kill -0 "$p" 2>/dev/null || break; sleep 1; done
		rm -f $B/heimdb.pid
	fi
	rm -f $B/server.pid
	sync
	echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null
}

load_dump() { # port user password
	local s e
	s=$(date +%s%N)
	client -P"$1" -u"$2" -p"$3" wpbench < $B/wpbench.sql 2> $B/log/load.err
	e=$(date +%s%N)
	echo "load $(( (e - s) / 1000000 )) ms"
	client -P"$1" -u"$2" -p"$3" -N -e "select count(*) from wpbench.wp_posts; select count(*) from wpbench.wp_comments" 2>/dev/null | tr '\n' ' '
	echo
}

case "$1" in
start-mysql)
	shift
	stop_all
	sudo docker run -d --name realmysql --network host -e MYSQL_ROOT_PASSWORD=root $MYSQL_IMAGE --port=3307 "$@" >/dev/null
	for _ in $(seq 120); do
		sudo docker exec realmysql mysql -uroot -proot -h127.0.0.1 -P3307 -e 'select 1' >/dev/null 2>&1 && break
		sleep 1
	done
	sudo docker exec realmysql mysql -uroot -proot -h127.0.0.1 -P3307 -e 'create database wpbench' 2>/dev/null
	load_dump 3307 root root
	sudo docker inspect -f '{{.State.Pid}}' realmysql > $B/server.pid
	echo "mysqld pid=$(cat $B/server.pid) $(sudo docker exec realmysql mysql -uroot -proot -N -e 'select version(), @@innodb_buffer_pool_size, @@max_connections' 2>/dev/null)"
	;;
start-heimdb)
	shift
	stop_all
	rm -rf $B/hdata $B/bk /mnt/bk/bk
	if [ -n "${SEED:-}" ]; then
		cp -a $B/seed-$SEED $B/hdata || { echo "no seed $SEED"; exit 1; }
	else
		mkdir -p $B/hdata
	fi
	case ${BK:-} in
	same) set -- -backup-dir $B/bk "$@" ;;
	other) set -- -backup-dir /mnt/bk/bk "$@" ;;
	enc) set -- -backup-dir $B/bk -backup-key bench=$B/age.pub "$@" ;;
	esac
	nohup env ${HEIMDB_GODEBUG:+GODEBUG=$HEIMDB_GODEBUG} $B/heimdb -addr 0.0.0.0:3316 -data $B/hdata -db wpbench -root-password rootpw -metrics-addr 127.0.0.1:9104 "$@" > $B/log/heimdb-$(date +%H%M%S).log 2>&1 &
	HP=$!
	echo $HP > $B/heimdb.pid
	for _ in $(seq 60); do sudo ss -ltnp | grep -q ':3316 ' && break; sleep 1; done
	LP=$(sudo ss -ltnp | grep ':3316 ' | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2)
	[ "$HP" = "$LP" ] || { echo "listener mismatch $HP $LP"; kill $HP; exit 1; }
	if [ -n "${SEED:-}" ]; then
		echo "seed $SEED $(du -sm $B/hdata | cut -f1) MB"
	else
		load_dump 3316 root rootpw
	fi
	echo $HP > $B/server.pid
	echo "heimdb pid=$HP $(client -P3316 -uroot -prootpw -N -e "select version()" 2>/dev/null)"
	;;
make-seed)
	SEED= BK= $0 start-heimdb
	MB=${3:-0}
	if [ "$MB" -gt 0 ]; then
		client -P3316 -uroot -prootpw wpbench -e 'CREATE TABLE wp_ballast (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, body longtext NOT NULL)'
		while [ "$(du -sm $B/hdata | cut -f1)" -lt "$MB" ]; do
			for _ in $(seq 10); do
				client -P3316 -uroot -prootpw wpbench -e 'INSERT INTO wp_ballast (body) SELECT post_content FROM wp_posts' || exit 1
			done
		done
		client -P3316 -uroot -prootpw -N -e 'SELECT COUNT(*) FROM wpbench.wp_ballast'
	fi
	stop_all
	rm -rf $B/seed-$2
	cp -a $B/hdata $B/seed-$2
	echo "seed $2 $(du -sm $B/seed-$2 | cut -f1) MB"
	;;
stop)
	stop_all
	;;
pid)
	cat $B/server.pid
	;;
stats)
	if [ -f $B/heimdb.pid ]; then
		curl -s http://127.0.0.1:9104/metrics | grep -E '^(heimdb_lock_wait_seconds_total|heimdb_connections|heimdb_connections_total|heimdb_statement_errors_total|heimdb_statements_total|heimdb_memory_live_bytes|heimdb_idle_lock_disconnects_total|heimdb_memory_aborted_statements_total|go_goroutines)[ {]'
		grep -c 'level=error' $B/log/$(ls -t $B/log | grep '^heimdb-' | head -1) | sed 's/^/log_errors /'
		grep -ci 'panic' $B/log/$(ls -t $B/log | grep '^heimdb-' | head -1) | sed 's/^/log_panics /'
	else
		sudo docker exec realmysql mysql -uroot -proot -N -e "show global status where Variable_name in ('Innodb_row_lock_waits','Innodb_row_lock_time','Innodb_row_lock_time_max','Innodb_deadlocks','Threads_connected','Max_used_connections','Aborted_clients','Aborted_connects','Connection_errors_max_connections','Questions','Innodb_buffer_pool_reads','Innodb_buffer_pool_read_requests')" 2>/dev/null
	fi
	;;
sample-start)
	P=$(cat $B/server.pid)
	M=()
	[ -f $B/heimdb.pid ] && M=(--metrics http://127.0.0.1:9104/metrics --metrics-out $B/res/$2.metrics.txt --metrics-every 10)
	sudo nohup python3 $B/sample.py --pid "$P" --out $B/res/$2.cpu.csv "${M[@]}" > /dev/null 2>&1 &
	echo $! > $B/sample.pid
	;;
sample-stop)
	[ -f $B/sample.pid ] && sudo kill "$(cat $B/sample.pid)" 2>/dev/null
	sudo pkill -f "$B/sample.py" 2>/dev/null
	rm -f $B/sample.pid
	;;
capture-writes)
	$0 start-mysql
	cd $B
	sudo docker compose -f docker-compose.yml up -d wp-mysql
	sleep 10
	sudo docker compose -f docker-compose.yml run --rm -T cli-mysql wp core version
	sudo docker compose -f docker-compose.yml run --rm -T cli-mysql wp eval-file - < capture-writes.php > $B/res/writes-capture.json
	sudo docker compose -f docker-compose.yml down
	;;
*)
	echo "usage: $0 start-mysql|start-heimdb|make-seed NAME [MB]|stop|pid|stats|sample-start NAME|sample-stop|capture-writes" >&2
	exit 2
	;;
esac
