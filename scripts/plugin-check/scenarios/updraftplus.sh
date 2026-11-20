# UpdraftPlus でデータベースのバックアップを取る（docs/plan.md の段階 68）。mysqldump はつなげない（TLS）ので、
# PHP での書き出しになる。途中で wpdb::check_connection()（DO 1）を呼ぶ。
step content
wp post generate --count=30 >/dev/null 2>&1
step updraftplus
wp plugin install updraftplus --activate 2>&1 | tail -1
wp eval 'global $updraftplus; $updraftplus->boot_backup(0, 1, false, true);' >/dev/null 2>&1
for _ in 1 2 3; do wp cron event run --due-now >/dev/null 2>&1; done
# 表ごとの行数（ログの Rows added）
wp eval 'foreach (glob(WP_CONTENT_DIR . "/updraft/log.*.txt") as $f) foreach (file($f) as $l) if (preg_match("/Table (\S+): Rows added in this batch \(next record: [^)]*\): (\d+)/", $l, $m)) echo "  $m[1] $m[2]\n";' 2>&1
