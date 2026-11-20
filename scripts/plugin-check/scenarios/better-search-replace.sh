# Better Search Replace で、記事の本文とシリアライズしたオプションの値を置き換える（docs/plan.md の段階 68）。
wp post create --post_title='Serial test' --post_content='Visit https://old.example.com/page now' --post_status=publish >/dev/null 2>&1
wp option update bsr_test_opt '{"url":"https://old.example.com"}' --format=json >/dev/null 2>&1
step better-search-replace
wp plugin install better-search-replace --activate 2>&1 | tail -1
get /wp-admin/tools.php?page=better-search-replace
wp eval 'foreach (array("loader", "db", "utils", "compatibility") as $c) require_once WP_PLUGIN_DIR . "/better-search-replace/includes/class-bsr-$c.php";
$db = new BSR_DB();
foreach (array("wp_posts", "wp_options") as $t) {
  $r = $db->srdb($t, 0, array("select_tables" => array($t), "search_for" => "old.example.com", "replace_with" => "new.example.com", "case_insensitive" => "off", "replace_guids" => "off", "dry_run" => "off", "completed_pages" => 0));
  echo "  $t change=", $r["table_report"]["change"], " updates=", $r["table_report"]["updates"], "\n";
}' 2>&1 | grep -v Deprecated
wp option get bsr_test_opt --format=json 2>&1 | tail -1
