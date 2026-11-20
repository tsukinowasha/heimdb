# WP Cerber（公式サイトのバージョン）を有効にし、ログインの失敗と成功、管理画面とログを開く（docs/plan.md の段階 72）。
# 主キーのない表（cerber_acl、cerber_log、cerber_traffic、cerber_qmem、cerber_lab、cerber_uss など）を作る。
fail() { curl -s -o /dev/null --data "log=admin&pwd=wrong&wp-submit=Log+In&testcookie=1" "http://127.0.0.1:$WP_PORT/wp-login.php"; }
step activate
wp plugin install https://downloads.wpcerber.com/plugin/wp-cerber.zip --activate 2>&1 | tail -1
login
get /wp-admin/admin.php?page=cerber-security
step traffic
get /
get /?p=1
get /no-such-page/
step failed-logins
for i in 1 2 3; do fail; done
step login
login
get /wp-admin/admin.php?page=cerber-security&tab=activity
get /wp-admin/admin.php?page=cerber-traffic
get /wp-admin/admin.php?page=cerber-security&tab=lockouts
step cron
wp cron event run --due-now >/dev/null 2>&1
get /
step rows
wp eval 'global $wpdb; foreach (array("cerber_log", "cerber_traffic", "cerber_qmem", "cerber_acl", "cerber_uss") as $t) { echo $t, " ", $wpdb->get_var("SELECT COUNT(*) FROM $t") > 0 ? "rows" : "empty", "\n"; }'
