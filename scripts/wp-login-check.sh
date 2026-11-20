#!/bin/sh
# WordPress の管理画面にログインできるかを確かめる。成功すれば終了コード 0。
#
#   scripts/wp-login-check.sh [URL] [USER] [PASSWORD]
set -eu
url="${1:-http://localhost:18080}"
user="${2:-admin}"
pass="${3:-admin}"
jar="$(mktemp)"
trap 'rm -f "$jar"' EXIT

# wp-login.php はテスト用の Cookie がないとログインを拒否するので、先に一度開く。
curl -fsS -c "$jar" -b "$jar" -o /dev/null "$url/wp-login.php"
curl -fsS -c "$jar" -b "$jar" -o /dev/null \
  --data-urlencode "log=$user" --data-urlencode "pwd=$pass" \
  --data-urlencode "wp-submit=Log In" --data-urlencode "redirect_to=$url/wp-admin/" \
  --data-urlencode "testcookie=1" "$url/wp-login.php"
body="$(curl -fsS -c "$jar" -b "$jar" "$url/wp-admin/")"
case "$body" in
*'id="wpadminbar"'*)
  echo "login OK: $user at $url/wp-admin/"
  ;;
*)
  echo "login FAILED: $user at $url/wp-admin/" >&2
  printf '%s' "$body" | grep -o '<title>[^<]*</title>' >&2 || true
  exit 1
  ;;
esac
