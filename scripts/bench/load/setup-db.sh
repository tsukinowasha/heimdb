#!/bin/bash
# 負荷の測り（docs/bench.md の段階 126）の DB のインスタンスを整える。~/b に次を置いてから流す:
#   heimdb (a linux/amd64 build made the same way as the Dockerfile), age.pub (an age recipient for -backup-key)、wpbench.sql（ページのベンチのダンプ）、
#   scripts/bench の docker-compose.yml と mu-plugins、この台本の置き場の dbctl.sh、sample.py、capture-writes.php
# Docker、mysql:8.0.46（サーバーと、ダンプを入れるクライアント）、書き込みの文を取るための WordPress の像を入れる。
set -euxo pipefail
cd ~/b
find . -name '._*' -delete
command -v docker || (curl -fsSL https://get.docker.com | sudo sh)
sudo usermod -aG docker "$USER" || true
sudo apt-get install -y python3 >/dev/null
sudo docker pull mysql:8.0.46
sudo docker pull wordpress:php8.3-apache
sudo docker pull wordpress:cli
chmod +x heimdb dbctl.sh sample.py
# The second disk (gce.sh create) holds the backups that go to another filesystem.
if [ -e /dev/disk/by-id/google-bk ] && ! mountpoint -q /mnt/bk; then
	sudo blkid /dev/disk/by-id/google-bk >/dev/null || sudo mkfs.ext4 -q -F /dev/disk/by-id/google-bk
	sudo mkdir -p /mnt/bk
	sudo mount /dev/disk/by-id/google-bk /mnt/bk
	sudo chown "$USER" /mnt/bk
fi
df -h ~ /mnt/bk || true
nproc; free -m; uname -r
