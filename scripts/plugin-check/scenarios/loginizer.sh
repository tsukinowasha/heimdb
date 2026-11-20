# Loginizer を有効にし、ログインの失敗でロックアウトさせる（docs/plan.md の段階 72）。ログの表 loginizer_logs に
# 主キーがない（UNIQUE KEY ip だけ）。
fail() { curl -s -o /dev/null --data "log=admin&pwd=wrong&wp-submit=Log+In&testcookie=1" "http://127.0.0.1:$WP_PORT/wp-login.php"; }
step activate
wp plugin install loginizer --activate 2>&1 | tail -1
get /wp-admin/admin.php?page=loginizer
step failed-logins
for i in 1 2 3 4 5; do fail; done
get /wp-login.php
step login
login
get /wp-admin/admin.php?page=loginizer_brute_force
wp eval 'global $wpdb; echo $wpdb->get_var("SELECT CONCAT(count, \":\", lockout) FROM {$wpdb->prefix}loginizer_logs"), "\n";'
