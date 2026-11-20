# プラグインを 1 つずつ有効にし、トップページと管理画面を開き、予定の cron を流して、無効に戻す（activate.sh と同じ）。
#   PLUGINS="smart-slider-3 sg-security" scripts/plugin-check/compare.sh scripts/plugin-check/scenarios/activate.sh
#   REQUIRES=woocommerce PLUGINS=woocommerce-payments scripts/plugin-check/compare.sh scripts/plugin-check/scenarios/activate.sh
# REQUIRES（空白区切りのスラッグ）は最初に有効にし、最後まで有効のままにする。有効にならなかったプラグインは
# 文を流さないので、エラーの比べでは見えない。NOT ACTIVE が出たら確かめる。
for p in ${REQUIRES:-}; do
  step "前提 $p"
  wp plugin install "$p" --activate 2>&1 | tail -1
done
for p in ${PLUGINS:?PLUGINS にスラッグを空白区切りで渡す}; do
  step "$p"
  wp plugin install "$p" --activate 2>&1 | tail -1
  if wp plugin is-active "$p" 2>/dev/null; then
    echo "  active $(wp plugin get "$p" --field=version 2>/dev/null)"
  else
    echo "  NOT ACTIVE"
  fi
  login
  for path in / /wp-admin/ /wp-admin/plugins.php /wp-admin/options-general.php; do
    get "$path" >/dev/null
  done
  wp cron event run --due-now >/dev/null 2>&1
  wp plugin deactivate "$p" >/dev/null 2>&1
done
