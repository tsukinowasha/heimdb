# Wordfence を有効にし、スキャンを 1 回流す（docs/plan.md の段階 68、70）。
step activate
wp plugin install wordfence --activate 2>&1 | tail -1
get /wp-admin/admin.php?page=Wordfence
step scan
wp eval 'wfScanEngine::startScan(false, wfScanner::SCAN_TYPE_QUICK);' >/dev/null 2>&1
wp cron event run --due-now >/dev/null 2>&1
get /
get /wp-admin/admin.php?page=WordfenceScan
