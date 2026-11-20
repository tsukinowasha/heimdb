// Package merge は、ブランチを親にマージするときの三方向比較と、WordPress のテーブルごとのマージ方針を提供する
// （docs/plan.md の段階 7 と「WordPress 特有の課題」）。データの読み書きはせず、渡された行の値だけで判断する。
package merge

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Row は列名（小文字）から値への対応。nil はその時点に行がないことを表す。
type Row map[string]any

// Change は、片側（ブランチまたは親）での行の変化。
type Change int

const (
	None Change = iota
	Insert
	Update
	Delete
)

func (c Change) String() string {
	return [...]string{"none", "insert", "update", "delete"}[c]
}

// Action は、マージでその行をどう扱うか。
type Action int

const (
	// NoOp は、反映するものがない（ブランチが変えていない、または親がすでに同じ値）。
	NoOp Action = iota
	// Apply は、ブランチの変更を親に反映する。
	Apply
	// Skip は、方針により反映しない。
	Skip
	// Conflict は、両側が同じものを違う値に変えたので、自動では決められない。
	Conflict
)

func (a Action) String() string {
	return [...]string{"none", "apply", "skip", "conflict"}[a]
}

// Decision は 1 行の判定。
type Decision struct {
	Branch, Parent Change
	Action         Action
	// Kind は、Apply のときに親に行う操作（Insert、Update、Delete）。
	Kind Change
	// Columns は、Apply（Update）なら書き換える列、Conflict なら食い違った列。
	Columns []string
	// Clean は、Conflict のとき、競合せずに反映できる列（競合を解決したときに使う）。
	Clean  []string
	Reason string
}

// change は、基準 base から今 now への片側の変化を、比べる列 cols で求める。
func change(base, now Row, cols []string) Change {
	switch {
	case base == nil && now == nil:
		return None
	case base == nil:
		return Insert
	case now == nil:
		return Delete
	}
	for _, c := range cols {
		if !Equal(base[c], now[c]) {
			return Update
		}
	}
	return None
}

// Decide は、ブランチの基準 baseB と今 nowB、親の基準 baseP と今 nowP から、列 cols について三方向で判定する。
// 片側ずつ、基準と今の値の差で変化を求める（マージで片側を優先した行は、両側の基準の値が一致しないため）。
func Decide(baseB, nowB, baseP, nowP Row, cols []string) Decision {
	d := Decision{Branch: change(baseB, nowB, cols), Parent: change(baseP, nowP, cols)}
	switch d.Branch {
	case None:
		d.Action = NoOp
	case Insert:
		switch {
		case nowP == nil && d.Parent == Delete:
			d.Action, d.Reason = Conflict, "added on the branch, deleted on the parent"
		case nowP == nil:
			d.Action, d.Kind = Apply, Insert
		case rowsEqual(nowB, nowP, cols):
			d.Action, d.Reason = NoOp, "the parent already has the same row"
		default:
			d.Action, d.Reason = Conflict, "added on both sides with different values"
			d.Columns = diffColumns(nowB, nowP, cols)
		}
	case Delete:
		switch {
		case nowP == nil:
			d.Action, d.Reason = NoOp, "already absent on the parent"
		case d.Parent != None:
			d.Action, d.Reason = Conflict, "deleted on the branch, changed on the parent"
		default:
			d.Action, d.Kind = Apply, Delete
		}
	case Update:
		if nowP == nil {
			d.Action, d.Reason = Conflict, "changed on the branch, absent on the parent"
			return d
		}
		var apply, conflict []string
		for _, c := range cols {
			if Equal(baseB[c], nowB[c]) {
				continue // ブランチはこの列を変えていない
			}
			parentChanged := baseP == nil || !Equal(baseP[c], nowP[c])
			switch {
			case Equal(nowP[c], nowB[c]):
				// 親がすでに同じ値
			case parentChanged:
				conflict = append(conflict, c)
			default:
				apply = append(apply, c)
			}
		}
		switch {
		case len(conflict) > 0:
			d.Action, d.Columns, d.Clean, d.Reason = Conflict, conflict, apply, "the same columns were changed on both sides"
		case len(apply) > 0:
			d.Action, d.Kind, d.Columns = Apply, Update, apply
		default:
			d.Action, d.Reason = NoOp, "the parent already has the same values"
		}
	}
	return d
}

func rowsEqual(a, b Row, cols []string) bool {
	return len(diffColumns(a, b, cols)) == 0
}

func diffColumns(a, b Row, cols []string) []string {
	var out []string
	for _, c := range cols {
		if !Equal(a[c], b[c]) {
			out = append(out, c)
		}
	}
	return out
}

// Equal は 2 つの列の値が同じかを、照合順序などを考えずに厳密に比べる（大文字と小文字の違いも変更とみなす）。
func Equal(a, b any) bool {
	switch av := a.(type) {
	case time.Time:
		bv, ok := b.(time.Time)
		return ok && av.Equal(bv)
	case []byte:
		bv, ok := b.([]byte)
		return ok && bytes.Equal(av, bv)
	case decimal.Decimal:
		bv, ok := b.(decimal.Decimal)
		return ok && av.Equal(bv)
	}
	return reflect.DeepEqual(a, b)
}

// Kind は、WordPress のテーブルの種類（マージ方針を決める）。
type Kind int

const (
	KindDefault Kind = iota
	KindOptions
	KindPosts
	KindPostmeta
	KindTermTaxonomy
	KindTerms
	KindTermmeta
	KindTermRelationships
	// KindParentOnly は、ブランチ側の変更を反映しないテーブル（コメントとユーザー）。
	KindParentOnly
)

// Policy は、WordPress のテーブルのマージ方針。Prefix はテーブルの接頭辞（既定は wp_）。
type Policy struct {
	Prefix string
}

// Kind はテーブル名の種類を返す。
func (p Policy) Kind(table string) Kind {
	name, ok := strings.CutPrefix(strings.ToLower(table), strings.ToLower(p.Prefix))
	if !ok {
		return KindDefault
	}
	switch name {
	case "options":
		return KindOptions
	case "posts":
		return KindPosts
	case "postmeta":
		return KindPostmeta
	case "term_taxonomy":
		return KindTermTaxonomy
	case "terms":
		return KindTerms
	case "termmeta":
		return KindTermmeta
	case "term_relationships":
		return KindTermRelationships
	case "comments", "commentmeta", "users", "usermeta":
		return KindParentOnly
	}
	return KindDefault
}

// IdentityColumns は、行を対応づける列を返す。nil なら主キー。wp_options は option_name（両側で add_option した
// 同名のオプションは option_id が違うため）。
func (p Policy) IdentityColumns(table string) []string {
	if p.Kind(table) == KindOptions {
		return []string{"option_name"}
	}
	return nil
}

// NaturalKey は、両側で別々に追加すると重複になる行の自然なキーを返す。空ならキーを持たない。
// 記事は種類とスラッグ（エディタを開くと両側でグローバルスタイルの記事ができる、など）、タームはスラッグ、
// 記事のメタは記事 ID とキー。
func (p Policy) NaturalKey(table string, r Row) string {
	switch p.Kind(table) {
	case KindPosts:
		if name := fmt.Sprint(r["post_name"]); r["post_name"] != nil && name != "" {
			return fmt.Sprintf("post_type=%v,post_name=%s", r["post_type"], name)
		}
	case KindTerms:
		return fmt.Sprintf("slug=%v", r["slug"])
	case KindPostmeta:
		return fmt.Sprintf("post_id=%v,meta_key=%v", r["post_id"], r["meta_key"])
	}
	return ""
}

// ExcludedColumns は、比べない列を返す。件数の列は両側の変更で必ず食い違うので、マージの後に親の側で数え直す。
func (p Policy) ExcludedColumns(table string) []string {
	switch p.Kind(table) {
	case KindPosts:
		return []string{"comment_count"}
	case KindTermTaxonomy:
		return []string{"count"}
	}
	return nil
}

// LatestColumns は、両側が変えても競合にせず、新しい方の値を採る列を返す（記事の更新日時は、別の列を
// 変えただけでも両側で変わるため）。
func (p Policy) LatestColumns(table string) []string {
	if p.Kind(table) == KindPosts {
		return []string{"post_modified", "post_modified_gmt"}
	}
	return nil
}

// DuplicateIgnoredColumns は、同じ自然なキーの重複（NaturalKey）で、中身が同じかを比べるときに見ない列を返す（主キーの
// ほか）。記事の guid はサイトの URL と ID を、日時は作った時刻を持つので、同じ中身でも両側で食い違う。
func (p Policy) DuplicateIgnoredColumns(table string) []string {
	if p.Kind(table) == KindPosts {
		return []string{"guid", "post_date", "post_date_gmt", "post_modified", "post_modified_gmt"}
	}
	return nil
}

// ResolveLatest は、判定 d の競合の列のうち latest に含まれるものを、新しい方の値を採るよう解決する。
// ブランチの値が新しければその列を反映し、親の値が新しければ親の値を残す。ほかの列の競合が残れば競合のまま。
// 競合がなくなり、反映する列もなければ NoOp にする。
func ResolveLatest(d Decision, nowB, nowP Row, latest []string) Decision {
	if d.Action != Conflict || d.Branch != Update || d.Parent != Update || len(latest) == 0 {
		return d
	}
	var rest []string
	apply := d.Clean
	for _, c := range d.Columns {
		if !slices.Contains(latest, c) {
			rest = append(rest, c)
		} else if later(nowB[c], nowP[c]) {
			apply = append(apply, c)
		}
	}
	if len(rest) > 0 {
		d.Columns = rest
		return d
	}
	d.Reason, d.Clean = "", nil
	if len(apply) == 0 {
		d.Action, d.Kind, d.Columns = NoOp, None, nil
		return d
	}
	d.Action, d.Kind, d.Columns = Apply, Update, apply
	return d
}

// later は a が b より新しい日時かを返す。日時でなければ文字列として比べる。
func later(a, b any) bool {
	at, aok := a.(time.Time)
	bt, bok := b.(time.Time)
	if aok && bok {
		return at.After(bt)
	}
	return fmt.Sprint(a) > fmt.Sprint(b)
}

// skippedOptions は、マージしないオプション（環境ごとに違って当然の値、作り直させる値、DB のバージョン、一時的な値）。
var skippedOptions = map[string]bool{
	"siteurl": true, "home": true, "blog_public": true, "cron": true, "rewrite_rules": true,
	"db_version": true, "initial_db_version": true, "db_upgraded": true,
	"recently_activated": true, "fresh_site": true, "auto_core_update_notified": true,
}

// SkipOption は、オプション name をマージしないかどうかと、その理由を返す。
func (p Policy) SkipOption(name string) (bool, string) {
	switch {
	case skippedOptions[name]:
		return true, "environment-specific option"
	case strings.HasPrefix(name, "_transient_"), strings.HasPrefix(name, "_site_transient_"):
		return true, "transient"
	case strings.HasSuffix(name, "_lock"), strings.HasSuffix(name, ".lock"):
		return true, "lock"
	case strings.HasPrefix(name, "https_"):
		return true, "environment-specific option"
	}
	return false, ""
}

// SkipPostmeta は、記事のメタ meta_key をマージしないかどうかを返す（編集中のロックは、ページを開いただけで変わる）。
func (p Policy) SkipPostmeta(metaKey string) bool {
	return metaKey == "_edit_lock" || metaKey == "_edit_last"
}

// SkipPost は、記事の行をマージしないかどうかと、その理由を返す（自動下書きとリビジョン）。
func (p Policy) SkipPost(row Row) (bool, string) {
	if row == nil {
		return false, ""
	}
	if s, _ := row["post_status"].(string); s == "auto-draft" {
		return true, "auto-draft"
	}
	if t, _ := row["post_type"].(string); t == "revision" {
		return true, "revision"
	}
	return false, ""
}
