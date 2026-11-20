package merge

import (
	"slices"
	"testing"
	"time"
)

var cols = []string{"title", "content", "status"}

func row(title, content, status string) Row {
	return Row{"title": title, "content": content, "status": status}
}

func TestDecide(t *testing.T) {
	base := row("T", "C", "publish")
	cases := []struct {
		name                     string
		baseB, nowB, baseP, nowP Row
		action                   Action
		kind                     Change
		columns                  []string
	}{
		{"untouched on the branch", base, base, base, row("T", "C2", "publish"), NoOp, None, nil},
		{"branch only change", base, row("T2", "C", "publish"), base, base, Apply, Update, []string{"title"}},
		// 本番でコメントが付いて別の列（ここでは status）が変わっても、別の列なら両方を採用する。
		{"different columns on each side", base, row("T2", "C", "publish"), base, row("T", "C", "private"), Apply, Update, []string{"title"}},
		{"same column, different values", base, row("T2", "C", "publish"), base, row("T3", "C", "publish"), Conflict, None, []string{"title"}},
		{"same column, same value", base, row("T2", "C", "publish"), base, row("T2", "C", "publish"), NoOp, None, nil},
		{"changed and reverted", base, base, base, base, NoOp, None, nil},
		{"insert", nil, base, nil, nil, Apply, Insert, nil},
		{"insert on both, same values", nil, base, nil, base, NoOp, None, nil},
		{"insert on both, different values", nil, base, nil, row("X", "C", "publish"), Conflict, None, []string{"title"}},
		{"delete", base, nil, base, base, Apply, Delete, nil},
		{"delete on both", base, nil, base, nil, NoOp, None, nil},
		{"delete vs parent change", base, nil, base, row("T", "C2", "publish"), Conflict, None, nil},
		{"update vs parent delete", base, row("T2", "C", "publish"), base, nil, Conflict, None, nil},
		// 前回のマージで本番を優先した行: 両側の基準の値が違っても、片側ずつ比べる。
		{"divergent bases after a merge", row("B", "C", "publish"), row("B2", "C", "publish"), row("P", "C", "publish"), row("P", "C", "publish"), Apply, Update, []string{"title"}},
	}
	for _, c := range cases {
		d := Decide(c.baseB, c.nowB, c.baseP, c.nowP, cols)
		if d.Action != c.action || d.Kind != c.kind || !slices.Equal(d.Columns, c.columns) {
			t.Errorf("%s: got action=%s kind=%s columns=%v (%s), want %s %s %v", c.name, d.Action, d.Kind, d.Columns, d.Reason, c.action, c.kind, c.columns)
		}
	}
}

func TestEqual(t *testing.T) {
	t1 := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	if !Equal(t1, t1.In(time.FixedZone("JST", 9*3600))) {
		t.Error("same instant in different zones should be equal")
	}
	if Equal("A", "a") {
		t.Error("case differences are changes")
	}
	if !Equal([]byte("x"), []byte("x")) || Equal(nil, "") || !Equal(nil, nil) {
		t.Error("bytes/nil comparison")
	}
	if Equal(int64(1), uint64(1)) {
		t.Error("different Go types are not equal (columns keep one type)")
	}
}

func TestPolicy(t *testing.T) {
	p := Policy{Prefix: "wp_"}
	kinds := map[string]Kind{
		"wp_options": KindOptions, "WP_POSTS": KindPosts, "wp_postmeta": KindPostmeta, "wp_term_taxonomy": KindTermTaxonomy,
		"wp_comments": KindParentOnly, "wp_usermeta": KindParentOnly, "wp_terms": KindTerms, "wp_woocommerce_orders": KindDefault,
		"wptests_options": KindDefault,
	}
	for table, want := range kinds {
		if got := p.Kind(table); got != want {
			t.Errorf("Kind(%s) = %d, want %d", table, got, want)
		}
	}
	if got := (Policy{Prefix: "wptests_"}).Kind("wptests_options"); got != KindOptions {
		t.Errorf("custom prefix: %d", got)
	}
	for _, name := range []string{"siteurl", "home", "blog_public", "cron", "rewrite_rules", "db_version", "_transient_x", "_site_transient_update_core", "core_updater.lock", "auto_updater.lock", "https_detection_errors"} {
		if skip, _ := p.SkipOption(name); !skip {
			t.Errorf("option %s should be skipped", name)
		}
	}
	for _, name := range []string{"blogname", "active_plugins", "page_on_front", "sidebars_widgets", "theme_mods_twentytwentyfive", "widget_block"} {
		if skip, _ := p.SkipOption(name); skip {
			t.Errorf("option %s should be merged", name)
		}
	}
	if !p.SkipPostmeta("_edit_lock") || p.SkipPostmeta("_thumbnail_id") {
		t.Error("postmeta skip")
	}
	if skip, _ := p.SkipPost(Row{"post_status": "auto-draft", "post_type": "post"}); !skip {
		t.Error("auto-draft should be skipped")
	}
	if skip, _ := p.SkipPost(Row{"post_status": "inherit", "post_type": "revision"}); !skip {
		t.Error("revision should be skipped")
	}
	if skip, _ := p.SkipPost(Row{"post_status": "publish", "post_type": "page"}); skip {
		t.Error("page should be merged")
	}
	if got := p.ExcludedColumns("wp_posts"); !slices.Equal(got, []string{"comment_count"}) {
		t.Errorf("excluded columns of posts = %v", got)
	}
	if got := p.IdentityColumns("wp_options"); !slices.Equal(got, []string{"option_name"}) {
		t.Errorf("identity of options = %v", got)
	}
}

func TestResolveLatest(t *testing.T) {
	t1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t2, t3 := t1.Add(time.Hour), t1.Add(2*time.Hour)
	cols := []string{"post_title", "post_content", "post_modified"}
	base := Row{"post_title": "T", "post_content": "C", "post_modified": t1}
	latest := Policy{Prefix: "wp_"}.LatestColumns("wp_posts")
	cases := []struct {
		name       string
		nowB, nowP Row
		action     Action
		columns    []string
	}{
		// ブランチはタイトル、親は本文を変え、どちらも更新日時が変わった。ブランチの方が新しい。
		{"branch later", Row{"post_title": "T2", "post_content": "C", "post_modified": t3}, Row{"post_title": "T", "post_content": "C2", "post_modified": t2}, Apply, []string{"post_title", "post_modified"}},
		{"parent later", Row{"post_title": "T2", "post_content": "C", "post_modified": t2}, Row{"post_title": "T", "post_content": "C2", "post_modified": t3}, Apply, []string{"post_title"}},
		{"real conflict stays", Row{"post_title": "T2", "post_content": "C", "post_modified": t3}, Row{"post_title": "T3", "post_content": "C", "post_modified": t2}, Conflict, []string{"post_title"}},
		{"only the date differs", Row{"post_title": "T", "post_content": "C", "post_modified": t2}, Row{"post_title": "T", "post_content": "C", "post_modified": t3}, NoOp, nil},
	}
	for _, c := range cases {
		d := ResolveLatest(Decide(base, c.nowB, base, c.nowP, cols), c.nowB, c.nowP, latest)
		if d.Action != c.action || !slices.Equal(d.Columns, c.columns) {
			t.Errorf("%s: got %s %v (%s), want %s %v", c.name, d.Action, d.Columns, d.Reason, c.action, c.columns)
		}
	}
}
