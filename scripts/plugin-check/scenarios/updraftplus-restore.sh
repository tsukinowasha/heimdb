# UpdraftPlus でデータベースのバックアップを取り、記事を消してから、管理画面と同じ手順（prepare_restore）で復元する
# （docs/plan.md の段階 72）。復元の前に権限を確かめる表 `CREATE TABLE updraft_tmp_... (test INT)` は主キーがない。
# これが作れると、一時的な接頭辞の表に入れてから名前を変える復元（atomic restore）になる。
step content
wp post generate --count=30 >/dev/null 2>&1
wp post list --post_type=post --format=count
step backup
wp plugin install updraftplus --activate 2>&1 | tail -1
wp eval 'global $updraftplus; $updraftplus->boot_backup(0, 1, false, true);' >/dev/null 2>&1
for _ in 1 2 3; do wp cron event run --due-now >/dev/null 2>&1; done
ts=$(wp eval 'foreach (UpdraftPlus_Backup_History::get_history() as $t => $b) { if (!empty($b["db"])) { echo $t; break; } }' 2>/dev/null)
echo "backup=$ts"
step change
wp post delete $(wp post list --post_type=post --format=ids) --force >/dev/null 2>&1
wp post list --post_type=post --format=count
step restore
wp --user=admin eval '
global $updraftplus;
if (!class_exists("UpdraftPlus_Admin")) require_once UPDRAFTPLUS_DIR."/admin.php";
$a = new UpdraftPlus_Admin();
$_REQUEST = $_POST = array("action" => "updraft_restore", "backup_timestamp" => "'"$ts"'", "updraft_restore" => array("db"), "updraftplus_ajax_restore" => "start_ajax_restore");
ob_start(); $a->prepare_restore(); ob_end_clean();
$job = $updraftplus->nonce;
echo "job=$job\n";
$_REQUEST = $_POST = array("action" => "updraft_ajaxrestore", "job_id" => $job, "updraftplus_ajax_restore" => "do_ajax_restore");
ob_start(); $a->prepare_restore(); $out = ob_get_clean();
echo substr(strip_tags($out), -1500), "\n";
' 2>&1 | tail -40
step after
wp post list --post_type=post --format=count
wp db query "SHOW TABLES" 2>&1 | tr '\n' ' '; echo
wp eval 'foreach (glob(WP_CONTENT_DIR . "/updraft/log.*.txt") as $f) foreach (file($f) as $l) if (preg_match("/permission|atomic|rename|RENAME|lock|Error|error|Finished|Restore successful|tables/i", $l)) echo "  ", substr(trim($l), 0, 220), "\n";' 2>&1 | tail -60
