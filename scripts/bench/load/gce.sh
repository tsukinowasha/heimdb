#!/bin/bash
# 負荷の測り（docs/bench.md の段階 126）の GCE の 2 つのインスタンスを、手元から作り、整え、結果を取り、消す。
#
#   scripts/bench/load/gce.sh create              DB と負荷のインスタンス（n2-standard-4、Debian 12、pd-balanced）を作る
#   scripts/bench/load/gce.sh push <heimdb> <dump> DB に heimdb の実行ファイルとダンプとこの置き場の台本を、負荷の側に loadgen とページを置き、
#                                                  DB を整え（setup-db.sh）、負荷の側から DB に ssh できるようにする
#   scripts/bench/load/gce.sh run <段階の引数...>   負荷の側で run.sh を裏で流す（例: run A、run B 52.3）。ログは ~/l/run-<段階>.log
#   scripts/bench/load/gce.sh fetch <置き場>        両方の res を手元に取る（agg.py <置き場> でまとめる）
#   scripts/bench/load/gce.sh delete               両方を消し、残っていないことを確かめる
#
# 書き込みのページを取り直すときは、DB で ~/b/dbctl.sh capture-writes を流し、~/b/res/writes-capture.json を
# mkwrites.py に渡す（pages-write.json）。
#
# GCE のプロジェクトは GCE_PROJECT で渡す（ゾーンは GCE_ZONE。既定は asia-northeast1-b）。
set -euo pipefail
: "${GCE_PROJECT:?set GCE_PROJECT to the GCE project}"
P=(--project "$GCE_PROJECT" --zone "${GCE_ZONE:-asia-northeast1-b}")
DBI=heimdb-bench-db
LDI=heimdb-bench-load
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
# The instances carry the tag heimdb-bench; create adds firewall rules for that tag only (SSH from IAP, and SSH and
# HeimDB between the two), and delete removes them.
TAG=heimdb-bench
gssh() { gcloud compute ssh "$1" "${P[@]}" --tunnel-through-iap --command "$2"; }
gscp() { gcloud compute scp "${P[@]}" --tunnel-through-iap "$@"; }
G=(--project "$GCE_PROJECT")
dbip() { gcloud compute instances describe $DBI "${P[@]}" --format='get(networkInterfaces[0].networkIP)'; }

case "$1" in
create)
	gcloud compute firewall-rules create $TAG-iap-ssh "${G[@]}" --network default --direction INGRESS \
		--source-ranges 35.235.240.0/20 --allow tcp:22 --target-tags $TAG
	gcloud compute firewall-rules create $TAG-internal "${G[@]}" --network default --direction INGRESS \
		--source-tags $TAG --allow tcp:22,tcp:3307,tcp:3316 --target-tags $TAG
	# The database instance has a second disk for the backups that go to another filesystem (run.sh K).
	gcloud compute instances create $DBI "${P[@]}" --machine-type n2-standard-4 --image-family debian-12 \
		--image-project debian-cloud --boot-disk-size 50GB --boot-disk-type pd-balanced --tags $TAG \
		--create-disk=name=$DBI-bk,size=50GB,type=pd-balanced,device-name=bk,auto-delete=yes
	gcloud compute instances create $LDI "${P[@]}" --machine-type n2-standard-4 --image-family debian-12 \
		--image-project debian-cloud --boot-disk-size 50GB --boot-disk-type pd-balanced --tags $TAG
	;;
push)
	T=$(mktemp -d)
	mkdir -p "$T/b/mu-plugins" "$T/l"
	cp "$2" "$T/b/heimdb"
	cp "$3" "$T/b/wpbench.sql"
	(cd "$ROOT" && go run filippo.io/age/cmd/age-keygen 2>/dev/null) | (cd "$ROOT" && go run filippo.io/age/cmd/age-keygen -y) > "$T/b/age.pub"
	cp "$ROOT/scripts/bench/docker-compose.yml" "$T/b/"
	cp "$ROOT/scripts/bench/mu-plugins/"*.php "$T/b/mu-plugins/"
	cp "$HERE/dbctl.sh" "$HERE/sample.py" "$HERE/capture-writes.php" "$HERE/setup-db.sh" "$T/b/"
	(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$T/l/loadgen" ./cmd/loadgen)
	cp "$HERE/pages-read.json" "$HERE/pages-write.json" "$HERE/run.sh" "$HERE/sample.py" "$T/l/"
	(cd "$T" && COPYFILE_DISABLE=1 tar czf b.tgz b && COPYFILE_DISABLE=1 tar czf l.tgz l)
	gscp "$T/b.tgz" $DBI:~
	gscp "$T/l.tgz" $LDI:~
	gssh $DBI 'tar xzf b.tgz && cd b && bash setup-db.sh > setup.log 2>&1 && tail -3 setup.log'
	K=$(gssh $LDI 'tar xzf l.tgz && sudo apt-get install -y python3 >/dev/null 2>&1; test -f ~/.ssh/id_ed25519 || ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519 -q; cat ~/.ssh/id_ed25519.pub')
	gssh $DBI "echo '$K' >> ~/.ssh/authorized_keys"
	gssh $LDI "ssh -o StrictHostKeyChecking=no -o BatchMode=yes $(dbip) hostname"
	rm -rf "$T"
	;;
run)
	shift
	gssh $LDI "cd ~/l && mkdir -p res && (DB=$(dbip) nohup ./run.sh $* > run-$1.log 2>&1 &)"
	;;
fetch)
	mkdir -p "$2"
	gssh $DBI 'cd ~/b && tar czf res-db.tgz res log'
	gssh $LDI 'cd ~/l && tar czf res-load.tgz res run-*.log'
	gscp $DBI:~/b/res-db.tgz "$2/"
	gscp $LDI:~/l/res-load.tgz "$2/"
	(cd "$2" && tar xzf res-db.tgz && tar xzf res-load.tgz)
	;;
delete)
	gcloud compute instances delete $DBI $LDI "${P[@]}" --quiet
	gcloud compute firewall-rules delete $TAG-iap-ssh $TAG-internal "${G[@]}" --quiet
	gcloud compute firewall-rules list "${G[@]}" --filter "name~^$TAG"
	gcloud compute instances list --project "$GCE_PROJECT"
	gcloud compute disks list --project "$GCE_PROJECT"
	;;
*)
	echo "usage: $0 create|push HEIMDB DUMP|run STAGE...|fetch DIR|delete" >&2
	exit 2
	;;
esac
