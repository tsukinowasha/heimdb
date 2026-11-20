#!/bin/bash
# 負荷の測り（docs/bench.md の段階 126）の DB のインスタンスを整える。~/b に次を置いてから流す:
#   heimdb-0242（tag v0.2.42 を Dockerfile と同じ形で建てた linux/amd64 の実行ファイル）、wpbench.sql（ページのベンチのダンプ）、
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
chmod +x heimdb-0242 dbctl.sh sample.py
nproc; free -m; uname -r
