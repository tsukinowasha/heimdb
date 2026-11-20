# WooCommerce で、属性のある商品と注文を作り、ショップ（属性の絞り込みを含む）と注文の管理画面を開く（docs/plan.md の
# 段階 68）。
step activate
wp plugin install woocommerce --activate 2>&1 | tail -1
wp option update woocommerce_coming_soon no >/dev/null 2>&1
step products
wp wc product_attribute create --name=Color --slug=color --user=admin >/dev/null 2>&1
attr=$(wp wc product_attribute list --user=admin --field=id 2>/dev/null | head -1)
for c in Red Blue; do wp wc product_attribute_term create "$attr" --name=$c --user=admin >/dev/null 2>&1; done
p1=$(wp wc product create --name=Shirt --type=simple --regular_price=10 --attributes="[{\"id\":$attr,\"options\":[\"Red\"],\"visible\":true}]" --user=admin --porcelain 2>/dev/null | tail -1)
p2=$(wp wc product create --name=Hoodie --type=variable --attributes="[{\"id\":$attr,\"options\":[\"Red\",\"Blue\"],\"visible\":true,\"variation\":true}]" --user=admin --porcelain 2>/dev/null | tail -1)
wp wc product_variation create "$p2" --regular_price=20 --attributes="[{\"id\":$attr,\"option\":\"Blue\"}]" --user=admin >/dev/null 2>&1
wp action-scheduler run >/dev/null 2>&1
step order
o=$(wp wc shop_order create --customer_id=1 --line_items="[{\"product_id\":$p1,\"quantity\":2}]" --set_paid=true --status=processing --user=admin --porcelain 2>/dev/null | tail -1)
wp wc shop_order update "$o" --status=completed --user=admin >/dev/null 2>&1
step pages
get /
get "/?post_type=product"
get "/?post_type=product&filter_color=blue"
get "/?p=$p1"
get /wp-admin/admin.php?page=wc-orders
get "/wp-admin/admin.php?page=wc-orders&s=Shirt"
get /wp-admin/edit.php?post_type=product
get /wp-admin/admin.php?page=wc-status
wp action-scheduler run >/dev/null 2>&1
wp cron event run --due-now >/dev/null 2>&1
