# Loginizer と WP Cerber の主キーのない表を、ブランチで変えて本番にマージし、マージの後もプラグインが書けることを
# 確かめる（docs/plan.md の段階 72）。ブランチは HeimDB だけなので、DB=heimdb で流す。ホストの mysql クライアントを使う。
q() { mysql -h127.0.0.1 -P"$HEIMDB_PORT" -uroot -p"$HEIMDB_ROOT_PASSWORD" -N -e "$2" "$1" 2>&1 | grep -v "Using a password"; }
fail() { curl -s -o /dev/null --data "log=admin&pwd=wrong&wp-submit=Log+In&testcookie=1" "http://127.0.0.1:$WP_PORT/wp-login.php"; }
counts() { q "$1" "SELECT CONCAT('loginizer=', (SELECT GROUP_CONCAT(CONCAT(ip, ':', count, '/', lockout) ORDER BY ip) FROM wp_loginizer_logs), ' cerber_log=', (SELECT COUNT(*) FROM cerber_log), ' traffic=', (SELECT COUNT(*) FROM cerber_traffic), ' acl=', (SELECT COUNT(*) FROM cerber_acl))"; }
step activate
wp plugin install loginizer --activate 2>&1 | tail -1
wp plugin install https://downloads.wpcerber.com/plugin/wp-cerber.zip --activate 2>&1 | tail -1
q wordpress "SHOW CREATE TABLE wp_loginizer_logs" | head -3
fail
get /
counts wordpress
step branch
q wordpress "CALL heimdb_branch_create('st')"
# ブランチ: Loginizer の別の IP の記録を足し、Cerber の ACL とログを足し、ログを 1 行消す
q wordpress@st "INSERT INTO wp_loginizer_logs (username, time, count, lockout, ip, url) VALUES ('x', UNIX_TIMESTAMP(), 1, 0, '192.0.2.9', '/')"
q wordpress@st "INSERT INTO cerber_acl (ip, tag, comments, acl_slice) VALUES ('198.51.100.7', 'B', 'from branch', 0)"
q wordpress@st "INSERT INTO cerber_log SELECT * FROM cerber_log LIMIT 1"
q wordpress@st "DELETE FROM cerber_log ORDER BY stamp LIMIT 1"
counts wordpress@st
# 本番: プラグインが書き続ける
fail
get /?p=1
get /no-such-page/
counts wordpress
step diff
q wordpress "CALL heimdb_branch_diff('st', '*')" | cut -c1-160
step merge
q wordpress "CALL heimdb_branch_merge('st')"
counts wordpress
step after-merge
# マージの後もプラグインが読み書きできる（ログイン失敗の数え上げ、ログ、通信の記録）
fail
get /
q wordpress "UPDATE cerber_acl SET comments = 'edited' WHERE ip = '198.51.100.7'"
q wordpress "DELETE FROM cerber_log ORDER BY stamp DESC LIMIT 1"
counts wordpress
q wordpress "SELECT CONCAT(ip, ' ', tag, ' ', comments) FROM cerber_acl"
q wordpress "CALL heimdb_branch_diff('st')" | head -5
