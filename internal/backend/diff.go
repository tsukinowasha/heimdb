package backend

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// ブランチと親の差分（docs/plan.md の段階 7a）。読み取りだけで、何も書かない。
//
//	CALL heimdb_branch_diff('staging');              -- テーブルごとの要約
//	CALL heimdb_branch_diff('staging', 'wp_posts');  -- そのテーブルの行ごとの判定
//	CALL heimdb_branch_diff('staging', '*');         -- すべてのテーブルの行ごとの判定

// defaultPrefix は、WordPress のテーブルが見つからないときに使う接頭辞。
const defaultPrefix = "wp_"

// detectPrefix は、親の今のテーブルから WordPress のテーブルの接頭辞（wp-config.php の $table_prefix）を見つける。
// option_name 列を持つ「<接頭辞>options」と「<接頭辞>posts」がそろう接頭辞のうち、最も短いもの（なければ wp_）。
// 差分とマージは、組が 2 つ以上あれば拒否する（differ。段階 60）。
func detectPrefix(st *store.Store, branch uint64) (string, error) {
	prefixes, err := detectPrefixes(st, branch)
	if err != nil || len(prefixes) == 0 {
		return defaultPrefix, err
	}
	return prefixes[0], nil
}

var diffSummarySchema = sql.Schema{
	{Name: "table_name", Type: types.LongText},
	{Name: "status", Type: types.LongText},
	{Name: "inserts", Type: types.Int64},
	{Name: "updates", Type: types.Int64},
	{Name: "deletes", Type: types.Int64},
	{Name: "skipped", Type: types.Int64},
	{Name: "conflicts", Type: types.Int64},
	{Name: "resolved", Type: types.Int64},
	{Name: "warnings", Type: types.Int64},
	{Name: "detail", Type: types.LongText, Nullable: true},
}

var diffRowSchema = sql.Schema{
	{Name: "table_name", Type: types.LongText},
	{Name: "id", Type: types.LongText},
	{Name: "branch_change", Type: types.LongText},
	{Name: "parent_change", Type: types.LongText},
	{Name: "action", Type: types.LongText},
	{Name: "resolution", Type: types.LongText, Nullable: true},
	{Name: "columns", Type: types.LongText, Nullable: true},
	{Name: "detail", Type: types.LongText, Nullable: true},
	// 段階 63: 解決を記録したユーザーと時刻、競合した列の両側の値（JSON）。
	{Name: "resolved_by", Type: types.LongText, Nullable: true},
	{Name: "resolved_at", Type: types.LongText, Nullable: true},
	{Name: "conflict_values", Type: types.LongText, Nullable: true},
}

// テーブル単位の状態。
const (
	statusChanged         = "changed"
	statusNewTable        = "new table"
	statusDroppedOnBranch = "dropped on branch"
	statusDroppedOnParent = "dropped on parent"
	statusSchemaDiffers   = "schema differs"
	// statusUnchanged は、マージで変えるものはないが、差分で全行を比べる（重さを知らせる）テーブル。
	statusUnchanged = "unchanged"
)

// tableDiff は 1 つのテーブルの差分。
type tableDiff struct {
	Name   string
	Status string
	Detail string
	// Resolution は、列の定義が違うテーブルの解決（"parent" ならそのテーブルを除いてマージする）。
	Resolution string
	// BranchDef と ParentDef は、今のブランチと親の定義（なければ nil）。
	BranchDef, ParentDef *schema.Table
	Rows                 []rowDiff
	// Columns と Indexes は、列とインデックスの違い（coldiff.go、indexdiff.go）。
	Columns  []columnChange
	Indexes  []indexChange
	Warnings []rowWarning
	// Merged は、列の変更を当てた後の親の定義（列が変わらなければ nil）。マージでは親をこの定義で書き直す。
	Merged *schema.Table
	// parentConv は、マージで親のテーブルを書き直した後、親の今の行を新しい形にする（書き直さなければ nil）。
	parentConv func(sql.Row) (sql.Row, error)
	// RewriteRows は、マージで書き直す親の行数（Merged が nil なら 0）。FullCompare は、テーブル ID が基準と違うので、
	// 変わった行だけでなく全行の主キーを比べた数（重さを差分の要約で知らせる。段階 44）。
	RewriteRows, FullCompare int
	// excludedCounts は、外した表のブランチの側の追加、変更、削除の数（反映しない）。notExcluded は、パターンに当たった
	// が本体の表なので外さなかったときの、そのパターン（段階 60）。
	excludedCounts [3]int64
	notExcluded    string
	// lazyRows は、行の値を持たない rowDiff（pk）があること。parentRead は、その親の行を読む定義（書き直す前の親の今の
	// 定義）。
	lazyRows   bool
	parentRead *schema.Table
}

// rowDiff は 1 行の判定。NoOp の行は含めない。
type rowDiff struct {
	ID       string
	Decision merge.Decision
	// BranchRow と ParentRow は今のブランチと親の行（なければ nil）。ParentRow は同一性の列で対応づけた行。
	BranchRow, ParentRow sql.Row
	// DupRow は、同じ自然なキーで親に追加された行（記事とタームの重複の競合）。
	DupRow sql.Row
	// Forbid は、ブランチの側を採る解決ができない理由。
	Forbid string
	// Resolution は競合の解決: "branch"、"parent"、"stale"（記録した後に行が変わった）、空（未解決）。
	Resolution string
	// Hash は、競合の判定に使った値のハッシュ（decisionHash）。行を指定した解決に記録し、変わったら stale にする。
	Hash string
	// pk は、BranchRow と ParentRow を持たずに、マージの適用のときに読み直す行の主キー（比べる行が多いとき。
	// docs/plan.md の段階 59）。hasParent は、読み直す前でも ParentRow があるかどうか。
	pk        []byte
	hasParent bool
	// resolvedBy と resolvedAt は、当てはめた解決の記録のユーザーと時刻。conflictValues は、競合した列の両側の値
	// （差分に出す JSON。段階 63）。
	resolvedBy     string
	resolvedAt     uint64
	conflictValues string
}

// parentExists は、親の行があるかどうか（読み直す行では、読み直す前でも分かる）。
func (r *rowDiff) parentExists() bool {
	return r.ParentRow != nil || r.hasParent
}

type rowWarning struct {
	ID, Detail string
}

// rowOp は、マージで親に行う操作。
type rowOp struct {
	Kind merge.Change
	// Columns は Update で書き換える列。Copy なら DupRow に、主キーなどを除くブランチの列をすべて写す。
	Columns []string
	Copy    bool
}

// forbidNote は、ブランチの側を採れない競合が解決されずに残っているとき、その理由を返す（ワイルドカードの 'branch' は
// 当たらないので、黙って残さない。docs/plan.md の段階 61）。
func (r *rowDiff) forbidNote() string {
	if r.Forbid == "" || r.Decision.Action != merge.Conflict || r.Resolution == "parent" {
		return ""
	}
	return "only 'parent' can resolve this: " + r.Forbid
}

// effect は、マージで親に行う操作を返す。解決していない競合なら ok が false。
func (r *rowDiff) effect() (op rowOp, ok bool) {
	dec := r.Decision
	switch dec.Action {
	case merge.Apply:
		return rowOp{Kind: dec.Kind, Columns: dec.Columns}, true
	case merge.Conflict:
		switch r.Resolution {
		case "parent":
			if dec.Branch == merge.Update && r.parentExists() && len(dec.Clean) > 0 {
				return rowOp{Kind: merge.Update, Columns: dec.Clean}, true
			}
			return rowOp{}, true
		case "branch":
			switch {
			case r.DupRow != nil:
				return rowOp{Kind: merge.Update, Copy: true}, true
			case !r.parentExists() && dec.Branch == merge.Delete:
				return rowOp{}, true
			case !r.parentExists():
				return rowOp{Kind: merge.Insert}, true
			case dec.Branch == merge.Delete:
				return rowOp{Kind: merge.Delete}, true
			default:
				return rowOp{Kind: merge.Update, Columns: append(slices.Clone(dec.Clean), dec.Columns...)}, true
			}
		}
		return rowOp{}, false
	}
	return rowOp{}, true
}

// summary は、テーブルの反映の件数を数える。解決した競合は、解決の結果の操作として数える。
func (t *tableDiff) summary() (ins, upd, del, skipped, conflicts, resolved int64) {
	if t.Status == statusExcluded {
		return t.excludedCounts[0], t.excludedCounts[1], t.excludedCounts[2], 0, 0, 0
	}
	for i := range t.Rows {
		r := &t.Rows[i]
		switch r.Decision.Action {
		case merge.Skip:
			skipped++
			continue
		case merge.Conflict:
			if _, ok := r.effect(); !ok {
				conflicts++
				continue
			}
			resolved++
		}
		op, _ := r.effect()
		switch op.Kind {
		case merge.Insert:
			ins++
		case merge.Update:
			upd++
		case merge.Delete:
			del++
		}
	}
	if t.Status == statusSchemaDiffers {
		if t.Resolution == "parent" {
			resolved++
		} else {
			conflicts++
		}
	}
	for _, c := range t.definitionChanges() {
		if c.r.Decision.Action == merge.Conflict {
			if c.ok {
				resolved++
			} else {
				conflicts++
			}
		}
	}
	return
}

// definitionChange は、列かインデックスの判定と、解決の後に親に行う操作があるかどうか。
type definitionChange struct {
	r  *rowDiff
	ok bool // 解決済み（または競合でない）
}

// definitionChanges は、テーブルの列とインデックスの判定を返す。
func (t *tableDiff) definitionChanges() []definitionChange {
	var out []definitionChange
	for i := range t.Columns {
		_, ok := t.Columns[i].effect()
		out = append(out, definitionChange{&t.Columns[i].rowDiff, ok})
	}
	for i := range t.Indexes {
		_, ok := t.Indexes[i].effect()
		out = append(out, definitionChange{&t.Indexes[i].rowDiff, ok})
	}
	return out
}

// differ は、ブランチと親の、ある時点の差分を求める。
type differ struct {
	st             *store.Store
	policy         merge.Policy
	branch, parent store.BranchInfo
	base           store.MergeBase
	// bases は、マージの基準と最新の取り込みの記録。直近が取り込みなら、両側の基準の中身は取り込みが読んだ親の状態
	// （branchBase、parentBase。docs/plan.md の段階 94）。変わった行の候補は、今までどおりマージの基準のブランチの時刻から
	// 探す（取り込みの前にブランチが変えた行も候補にする）。
	bases store.Bases
	// candB は、diffTable の中で、変わった行の候補を探すときのブランチの基準の定義（マージの基準のブランチの時刻）。
	candB *schema.Table
	// target は、行を比べるときにそろえる列と型（列の変更を当てた後の親の定義。列が変わらなければ nil）。
	target *schema.Table
	// snap は両側の「今」として読む時刻。
	snap    uint64
	schemas map[*schema.Table]sql.PrimaryKeySchema
	// parentDefs は、onParent で読んだ親の今の定義（接頭辞の後の名前から。親にない表は nil）。
	parentDefs map[string]*schema.Table
	// defaults は、定義ごとの列のリテラルの既定値（defaultsOf）。convCtx は、行の値の型を変える context（使い回す）。
	defaults map[*schema.Table][]colDefault
	convCtx  *sql.Context
	// reads は読んだ行の数。マージと復元は変わる行を集めて計画を作るので、ときどきメモリの上限を確かめる（段階 58）。
	reads int
	// ctx は、手続きを呼んだ文の context。nil でなければ、読みながら文が止められた（実行時間の上限、KILL QUERY）かも
	// 確かめる（ブランチのユーザーが呼ぶ手続きにも上限を効かせる。段階 63）。マージでは nil。
	ctx *sql.Context

	// resolutions は競合の解決の記録。
	resolutions []store.Resolution
	// excludes は、マージから外す表のパターン（段階 60）。
	excludes []string
	// notCreated は、ブランチで追加したが親に作らない記事、ターム、term_taxonomy の ID（それにぶら下がる行も反映しない）。
	notCreated map[merge.Kind]map[string]bool
	// created は、このマージで親に作る記事、ターム、term_taxonomy の ID。
	created map[merge.Kind]map[string]bool
	// revived は、前のマージで反映しなかった（方針で飛ばした、親の側で解決した）が、今回は親に作る記事、ターム、
	// term_taxonomy の ID。それにぶら下がる行は、変わっていなくても候補にする。
	revived map[merge.Kind]map[string]bool
	// deletedPosts はブランチで削除して親に反映する記事の ID。
	deletedPosts []string
	// versionOnly は、db_version の違いをエラーにせず versionNote に記録する（差分の要約。段階 44）。
	versionOnly bool
	versionNote string
	// weigh は、マージで書き直す親の行数を数える（差分の要約。マージは親のロックを持っているので数えず、書き直した
	// ときの行数を使う）。
	weigh bool
}

func (p *Provider) newDiffer(branchName string) (*differ, error) {
	b, ok := p.st.BranchByName(branchName)
	if !ok {
		return nil, refuseBranchNotFound.errorf("branch %q not found", branchName)
	}
	if b.Parent == 0 {
		return nil, refuseNoParent.errorf("branch %q has no parent", branchName)
	}
	var parent store.BranchInfo
	for _, pb := range p.st.Branches() {
		if pb.ID == b.Parent {
			parent = pb
		}
	}
	bases, err := p.st.Bases(b.ID)
	if err != nil {
		return nil, err
	}
	base := bases.Merge
	res, err := p.st.Resolutions(b.ID)
	if err != nil {
		return nil, err
	}
	// 接頭辞が決まらなければ、マージの規則（記事とメタの関係、件数の数え直し）と本体の表の判定を取り違えうるので拒否する。
	prefixes, err := detectPrefixes(p.st, parent.ID)
	if err != nil {
		return nil, err
	}
	if len(prefixes) > 1 {
		return nil, refuseAmbiguousPrefix.errorf("the parent has WordPress tables with more than one prefix (%s); drop or rename the unused tables",
			strings.Join(prefixes, ", "))
	}
	prefix := defaultPrefix
	if len(prefixes) == 1 {
		prefix = prefixes[0]
	}
	excludes, err := p.st.MergeExcludes()
	if err != nil {
		return nil, err
	}
	return &differ{
		st: p.st, policy: merge.Policy{Prefix: prefix}, branch: b, parent: parent, base: base, bases: bases, excludes: excludes,
		snap: p.st.LastCommitted(), schemas: map[*schema.Table]sql.PrimaryKeySchema{}, resolutions: res,
		notCreated: map[merge.Kind]map[string]bool{merge.KindPosts: {}, merge.KindTerms: {}, merge.KindTermTaxonomy: {}},
		revived:    map[merge.Kind]map[string]bool{merge.KindPosts: {}, merge.KindTerms: {}, merge.KindTermTaxonomy: {}},
		created:    map[merge.Kind]map[string]bool{merge.KindPosts: {}, merge.KindTerms: {}, merge.KindTermTaxonomy: {}},
	}, nil
}

// diff は、すべてのテーブルの差分を求める。記事のメタやタームの関連の判定は記事とタームの判定に依存するので、
// 記事、ターム、term_taxonomy の順に先に処理する。
func (d *differ) diff() ([]*tableDiff, error) {
	if err := d.checkDBVersion(); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, side := range []struct {
		branch uint64
		ts     uint64
	}{{d.branch.ID, d.snap}, {d.parent.ID, d.snap}, {d.branch.ID, d.base.BranchTS}, d.branchBaseSide()} {
		defs, err := d.st.ListTables(side.branch, side.ts)
		if err != nil {
			return nil, err
		}
		for _, def := range defs {
			names[strings.ToLower(def.Name)] = true
		}
	}
	order := make([]string, 0, len(names))
	for n := range names {
		order = append(order, n)
	}
	rank := func(name string) int {
		switch d.policy.Kind(name) {
		case merge.KindPosts:
			return 0
		case merge.KindTerms:
			return 1
		case merge.KindTermTaxonomy:
			return 2
		}
		return 3
	}
	sort.Slice(order, func(i, j int) bool {
		if ri, rj := rank(order[i]), rank(order[j]); ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})
	var out []*tableDiff
	for _, name := range order {
		t, err := d.diffTable(name)
		if err != nil {
			return nil, fmt.Errorf("HeimDB: diff %s: %w", name, err)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// checkDBVersion は、両側の db_version が同じかを確かめる。
func (d *differ) checkDBVersion() error {
	name := d.policy.Prefix + "options"
	var versions [2]any
	for i, b := range []uint64{d.branch.ID, d.parent.ID} {
		def, ok, err := d.st.GetTable(b, name, d.snap)
		if err != nil || !ok {
			return err
		}
		rows, err := d.optionsByName(b, def, d.snap)
		if err != nil {
			return err
		}
		if r := rows["db_version"]; r != nil {
			versions[i] = r["option_value"]
		}
	}
	if !merge.Equal(versions[0], versions[1]) {
		note := fmt.Sprintf("branch %v, parent %v; run the WordPress database upgrade on both sides before merging", versions[0], versions[1])
		if d.versionOnly {
			d.versionNote = note
			return nil
		}
		return refuseDBVersionDiffers.errorf("db_version differs (%s)", note)
	}
	return nil
}

func (d *differ) diffTable(name string) (*tableDiff, error) {
	get := func(branch, ts uint64) (*schema.Table, error) {
		def, ok, err := d.st.GetTable(branch, name, ts)
		if !ok {
			def = nil
		}
		return def, err
	}
	nowB, err := get(d.branch.ID, d.snap)
	if err != nil {
		return nil, err
	}
	bb := d.branchBaseSide()
	baseB, err := get(bb.branch, bb.ts)
	if err != nil {
		return nil, err
	}
	candB := baseB
	if d.bases.SyncLatest() {
		if candB, err = get(d.branch.ID, d.base.BranchTS); err != nil {
			return nil, err
		}
	}
	d.candB = candB
	defer func() { d.candB = nil }()
	nowP, err := get(d.parent.ID, d.snap)
	if err != nil {
		return nil, err
	}
	pb := d.parentBaseSide()
	baseP, err := get(pb.branch, pb.ts)
	if err != nil {
		return nil, err
	}
	t := &tableDiff{Name: name, BranchDef: nowB, ParentDef: nowP, parentRead: nowP}
	if nowB != nil {
		t.Name = nowB.Name
	} else if nowP != nil {
		t.Name = nowP.Name
	}
	if pattern, core := exclusion(d.excludes, d.policy.Prefix, t.Name); pattern != "" && !core {
		if nowB == nil && candB == nil {
			return t, nil // ブランチの側にない表（親だけで作った）は、外さない表と同じく出さない
		}
		// 外した表は比べない（本番の定義とデータをそのまま使う。段階 60）。件数はマージの基準からのブランチの変更。
		return t, d.excludedDiff(t, pattern, candB, nowB)
	} else if pattern != "" && (nowB != nil || nowP != nil) {
		t.notExcluded = pattern
	}
	switch {
	case nowB == nil && nowP == nil:
		return t, nil
	case nowB == nil && baseB != nil:
		t.Status = statusDroppedOnBranch
		t.Detail = "dropping tables is not merged"
		return t, nil
	case nowB == nil:
		return t, nil // 親だけで作ったテーブル
	case nowP == nil && baseB != nil:
		t.Status, t.Detail = statusDroppedOnParent, "the branch's changes to this table are not merged"
		return t, nil
	case nowP == nil:
		t.Status = statusNewTable
	default:
		// 主キーの列（名前かキーの形）が違えば、テーブル単位の競合（親の側にしか解決できない）。キーの形は型で変わる
		// （段階 70）。それ以外の列の違い（キーの形の変わらない主キーの列の型の変更を含む）は列ごとにマージする。
		pkB, err := pkColumns(nowB)
		if err != nil {
			return nil, err
		}
		pkP, err := pkColumns(nowP)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(pkB, pkP) {
			t.Status = statusSchemaDiffers
			t.Detail = "primary key differs"
			if res, ok := d.findWildcard(name); ok && res.Choice == "parent" {
				t.Resolution = "parent"
			}
			return t, nil
		}
		d.diffColumns(t, baseB, nowB, baseP, nowP)
		if t.Merged = mergedDefinition(t, nowB, nowP); t.Merged != nil && d.weigh {
			if t.RewriteRows, err = d.countRows(d.parent.ID, nowP); err != nil {
				return nil, err
			}
		}
	}

	ref := nowP
	if ref == nil {
		ref = nowB
	}
	if t.Merged != nil {
		ref = t.Merged
	}
	d.target = t.Merged
	defer func() { d.target = nil }()
	excluded := map[string]bool{}
	for _, c := range d.policy.ExcludedColumns(name) {
		excluded[c] = true
	}
	var cols []string
	for _, c := range ref.Columns {
		// 主キーのない表の行のハッシュは比べない（見える列から求まる。書くときに求め直す。docs/plan.md の段階 72）。
		if n := strings.ToLower(c.Name); !excluded[n] && n != schema.RowHashColumn {
			cols = append(cols, n)
		}
	}

	if idCols := d.policy.IdentityColumns(name); idCols != nil {
		err = d.diffByColumn(t, idCols[0], cols, baseB, nowB, baseP, nowP)
	} else {
		err = d.diffByPK(t, cols, baseB, nowB, baseP, nowP)
	}
	if err != nil {
		return nil, err
	}
	if nowP != nil {
		d.diffIndexes(t, baseB, nowB, baseP, nowP)
		t.Detail = t.definitionSummary()
	}
	t.Detail = joinDetail(t.Detail, t.weightSummary())
	if t.Status == "" && (len(t.Rows) > 0 || len(t.Columns) > 0 || len(t.Indexes) > 0 || len(t.Warnings) > 0) {
		t.Status = statusChanged
	}
	if t.Status == "" && t.FullCompare > 0 {
		t.Status = statusUnchanged
	}
	return t, nil
}

// weightSummary は、マージの重さ（親を書き直す行数と、全行を比べる行数）の要約。
func (t *tableDiff) weightSummary() string {
	var parts []string
	if t.RewriteRows > 0 {
		parts = append(parts, fmt.Sprintf("rewrite %d rows", t.RewriteRows))
	}
	if t.FullCompare > 0 {
		parts = append(parts, fmt.Sprintf("full compare %d rows", t.FullCompare))
	}
	return strings.Join(parts, ", ")
}

// joinDetail は、空でない要約を "; " でつなぐ。
func joinDetail(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "; ")
}

// countRows は、時刻 snap の時点のテーブル def の行数を数える（値は読まない）。
func (d *differ) countRows(branch uint64, def *schema.Table) (int, error) {
	n := 0
	err := d.st.ScanRowsAsOf(branch, def.ID, d.snap, nil, nil, false, func(store.RowVersion) (bool, error) {
		n++
		return true, nil
	})
	return n, err
}

// fillUnknownBase は、基準の行にない列（後から足した列で、既定値が CURRENT_TIMESTAMP などの式）を今の行の値にする。
// 列を足したときに入った値は、その側の変更とは見なさない。
func fillUnknownBase(base, now merge.Row) {
	if base == nil || now == nil {
		return
	}
	for k, v := range now {
		if _, ok := base[k]; !ok {
			base[k] = v
		}
	}
}

// pkColumns は、主キーの列の、小文字にした名前とキーの形（rowenc.PKKeyKind）。
func pkColumns(t *schema.Table) ([]string, error) {
	out := make([]string, len(t.PKOrdinals))
	for i, ord := range t.PKOrdinals {
		c := t.Columns[ord]
		typ, err := c.SQLType()
		if err != nil {
			return nil, err
		}
		out[i] = strings.ToLower(c.Name) + " " + rowenc.PKKeyKind(typ)
	}
	return out, nil
}

func pkNames(t *schema.Table) []string {
	out := make([]string, len(t.PKOrdinals))
	for i, ord := range t.PKOrdinals {
		out[i] = strings.ToLower(t.Columns[ord].Name)
	}
	return out
}

// diffByPK は、主キーで行を対応づけて比べる。候補はブランチの自分のキー空間で基準より後に書かれた行で、
// ブランチでテーブルが書き直された（テーブル ID が変わった）ときは、基準と今のブランチの全行。
func (d *differ) diffByPK(t *tableDiff, cols []string, baseB, nowB, baseP, nowP *schema.Table) error {
	pks, err := d.changedPKs(t, d.branch.ID, nowB, d.candB, d.base.BranchTS)
	if err != nil {
		return err
	}
	kind := d.policy.Kind(t.Name)
	if pks, err = d.addRevivedDependents(kind, nowB, pks); err != nil {
		return err
	}
	var parentAdded map[string]sql.Row
	if nowP != nil && (kind == merge.KindPosts || kind == merge.KindTerms || kind == merge.KindPostmeta) {
		if parentAdded, err = d.parentAdded(t, nowB, nowP); err != nil {
			return err
		}
	}
	forkP, ok, err := d.st.GetTable(d.parent.ID, t.Name, d.branch.ForkedAt)
	if err != nil {
		return err
	}
	if !ok {
		forkP = nil
	}
	autoIncPK := false
	for _, ord := range nowB.PKOrdinals {
		autoIncPK = autoIncPK || nowB.Columns[ord].AutoIncrement
	}
	var hashCols []string
	for _, c := range cols {
		if !slices.Contains(d.policy.LatestColumns(t.Name), c) {
			hashCols = append(hashCols, c)
		}
	}
	tracked := kind == merge.KindPosts || kind == merge.KindTerms || kind == merge.KindTermTaxonomy
	bb, pb := d.branchBaseSide(), d.parentBaseSide()
	srcs, closeSrcs, err := d.rowSources(len(pks), []rowSide{{bb.branch, baseB, bb.ts}, {d.branch.ID, nowB, d.snap}, {pb.branch, baseP, pb.ts}, {d.parent.ID, nowP, d.snap}})
	if err != nil {
		return err
	}
	defer closeSrcs()
	vt := d.valueTypes(nowB, nowP)
	for _, pk := range pks {
		var rows [4]sql.Row
		for i, src := range srcs {
			if rows[i], err = src(pk); err != nil {
				return err
			}
		}
		// ブランチで作った行で、親には分岐の時点にも基準にも今にもない（前のマージで方針により飛ばした、または親の側で
		// 解決した）ものは、ブランチの基準のバージョンを無視して追加として比べる。例: 前のマージで飛ばした自動下書きを公開した。
		revived := false
		if rows[0] != nil && rows[2] == nil && rows[3] == nil {
			atFork, err := d.getRow(d.parent.ID, forkP, d.branch.ForkedAt, pk)
			if err != nil {
				return err
			}
			revived = atFork == nil
		}
		// 取り込みの後は基準が両側とも親の状態なので、上の条件は立たない。基準にも親の今にもないブランチの行で、前のマージの
		// 時点のブランチにあったもの（前のマージで方針により飛ばした）を、同じく追加として扱い、ぶら下がる行も候補にする
		// （段階 94）。
		if d.bases.SyncLatest() && tracked && rows[0] == nil && rows[1] != nil && rows[3] == nil {
			prev, err := d.getRow(d.branch.ID, d.candB, d.base.BranchTS, pk)
			if err != nil {
				return err
			}
			revived = prev != nil
		}
		var mrows [4]merge.Row
		mrows[0], mrows[1] = d.sideRows(baseB, nowB, rows[0], rows[1])
		mrows[2], mrows[3] = d.sideRows(baseP, nowP, rows[2], rows[3])
		if revived {
			mrows[0] = nil
		}
		dec := merge.Decide(mrows[0], mrows[1], mrows[2], mrows[3], cols)
		dec = merge.ResolveLatest(dec, mrows[1], mrows[3], d.policy.LatestColumns(t.Name))
		if dec.Branch == merge.None {
			continue
		}
		cur, curDef := mrows[1], nowB
		if cur == nil {
			cur, curDef = toMergeRow(baseB, rows[0]), baseB
		}
		r := rowDiff{ID: pkString(nowB, baseB, rows[1], rows[0]), Decision: dec, BranchRow: rows[1], ParentRow: rows[3]}
		if dec.Action == merge.Conflict && dec.Branch == merge.Insert && rows[3] != nil && autoIncPK {
			r.Forbid = "different rows have the same auto-increment ID"
		}
		if err := d.applyPolicy(t, kind, &r, cur, cols, parentAdded); err != nil {
			return err
		}
		var dup merge.Row
		if r.DupRow != nil {
			dup = toMergeRow(t.ParentDef, r.DupRow)
		}
		r.Hash = decisionHash(r.Decision, hashCols, mrows[1], mrows[3], dup)
		vrows := mrows
		if dup != nil {
			vrows[3] = dup // 同じ自然なキーの重複の競合の相手は、親の別の主キーの行
		}
		r.conflictValues = conflictValues(d.conversionContext(), r.Decision, cols, vrows, vt)
		d.resolve(t, &r)
		op, ok := r.effect()
		if tracked && rows[1] != nil {
			id := fmt.Sprint(rows[1][curDef.PKOrdinals[0]])
			switch {
			case ok && op.Kind == merge.Insert:
				d.created[kind][id] = true
				if revived {
					d.revived[kind][id] = true
				}
			case rows[3] == nil:
				d.notCreated[kind][id] = true
			}
		}
		if ok && op.Kind == merge.Delete && kind == merge.KindPosts {
			d.deletedPosts = append(d.deletedPosts, fmt.Sprint(cur["id"]))
		}
		if r.Resolution == "branch" && r.DupRow != nil {
			t.Warnings = append(t.Warnings, rowWarning{ID: r.ID, Detail: "copied onto the parent's row; meta and term relationships of the branch row are not merged"})
		}
		if r.Decision.Action != merge.NoOp {
			if len(pks) >= cursorMinRows && r.DupRow == nil {
				// 行の値を持たず、適用のときに主キーの順に読み直す（候補は主キーの順に並べてある）
				r.pk, r.hasParent = pk, r.ParentRow != nil
				r.BranchRow, r.ParentRow = nil, nil
				t.lazyRows = true
			}
			t.Rows = append(t.Rows, r)
		}
	}
	if kind == merge.KindPosts && len(d.deletedPosts) > 0 {
		if err := d.warnOrphanComments(t); err != nil {
			return err
		}
	}
	return nil
}

// dependency は、ほかの行にぶら下がる行の、親の行を指す列とその行の種類。
type dependency struct {
	col  string
	kind merge.Kind
}

// dependsOn は、記事、ターム、term_taxonomy にぶら下がる行。
var dependsOn = map[merge.Kind][]dependency{
	merge.KindPostmeta:          {{"post_id", merge.KindPosts}},
	merge.KindTermmeta:          {{"term_id", merge.KindTerms}},
	merge.KindTermTaxonomy:      {{"term_id", merge.KindTerms}},
	merge.KindTermRelationships: {{"object_id", merge.KindPosts}, {"term_taxonomy_id", merge.KindTermTaxonomy}},
}

// addRevivedDependents は、今回親に作る（前のマージで反映しなかった）記事などにぶら下がる、ブランチで作った行を候補 pks に
// 加える。ぶら下がる行は前のマージから変わっていないことがあるため。
func (d *differ) addRevivedDependents(kind merge.Kind, nowB *schema.Table, pks [][]byte) ([][]byte, error) {
	deps := dependsOn[kind]
	need := false
	for _, dep := range deps {
		need = need || len(d.revived[dep.kind]) > 0
	}
	if !need || nowB == nil {
		return pks, nil
	}
	seen := map[string]bool{}
	for _, pk := range pks {
		seen[string(pk)] = true
	}
	own, err := d.st.ChangedRows(d.branch.ID, nowB.ID, 0, d.snap)
	if err != nil {
		return nil, err
	}
	for _, pk := range own {
		if seen[string(pk)] {
			continue
		}
		row, err := d.getRow(d.branch.ID, nowB, d.snap, pk)
		if err != nil {
			return nil, err
		}
		m := toMergeRow(nowB, row)
		for _, dep := range deps {
			if m != nil && d.revived[dep.kind][fmt.Sprint(m[dep.col])] {
				pks = append(pks, pk)
				seen[string(pk)] = true
				break
			}
		}
	}
	sort.Slice(pks, func(i, j int) bool { return string(pks[i]) < string(pks[j]) })
	return pks, nil
}

// withDefaults は、基準の定義 baseDef の行 row を、今の定義 now の列で表す。今の定義にあって基準の定義にない列（両側で
// 同じ ALTER TABLE ... ADD COLUMN をした後など）は、その列の既定値とみなす（ALTER で入る値なので、変更とは数えない）。
func (d *differ) withDefaults(baseDef, now *schema.Table, row sql.Row) merge.Row {
	m := toMergeRow(baseDef, row)
	if m == nil || now == nil {
		return m
	}
	sch, err := d.schema(now)
	if err != nil {
		return m
	}
	defaults := d.defaultsOf(now, sch)
	for i, c := range now.Columns {
		name := strings.ToLower(c.Name)
		if _, ok := m[name]; ok {
			continue
		}
		if defaults[i].ok {
			m[name] = defaults[i].v
		}
	}
	return m
}

// colDefault は、列のリテラルの既定値（literalDefault の結果）。
type colDefault struct {
	v  any
	ok bool
}

// defaultsOf は、定義 def（スキーマ sch）の列ごとのリテラルの既定値を、定義ごとに 1 回だけ求める。行ごとに求めると、
// 値の変換のたびに context を作るので、50 万行の列を足したマージで 38 秒かかった（docs/plan.md の段階 59）。
func (d *differ) defaultsOf(def *schema.Table, sch sql.PrimaryKeySchema) []colDefault {
	if out, ok := d.defaults[def]; ok {
		return out
	}
	out := make([]colDefault, len(def.Columns))
	for i, c := range def.Columns {
		out[i].v, out[i].ok = literalDefault(sch.Schema[i], c.Default)
	}
	if d.defaults == nil {
		d.defaults = map[*schema.Table][]colDefault{}
	}
	d.defaults[def] = out
	return out
}

// conversionContext は、行の値の型を変える context（differ ごとに 1 つを使い回す）。
func (d *differ) conversionContext() *sql.Context {
	if d.convCtx == nil {
		d.convCtx = sql.NewEmptyContext()
	}
	return d.convCtx
}

// literalDefault は、列の既定値（リテラル）を値にする。式の既定値（CURRENT_TIMESTAMP など）は扱わない。
func literalDefault(col *sql.Column, def *string) (any, bool) {
	if def == nil {
		if col.Nullable {
			return nil, true
		}
		return col.Type.Zero(), true
	}
	lit := strings.TrimSpace(*def)
	for len(lit) >= 2 && lit[0] == '(' && lit[len(lit)-1] == ')' {
		lit = strings.TrimSpace(lit[1 : len(lit)-1])
	}
	switch {
	case strings.EqualFold(lit, "NULL"):
		return nil, true
	case len(lit) >= 2 && (lit[0] == '\'' || lit[0] == '"') && lit[len(lit)-1] == lit[0]:
		q := string(lit[0])
		lit = strings.ReplaceAll(lit[1:len(lit)-1], q+q, q)
	case strings.IndexFunc(lit, func(r rune) bool { return !strings.ContainsRune("0123456789.-+eE", r) }) >= 0:
		return nil, false
	}
	v, _, err := col.Type.Convert(sql.NewEmptyContext(), lit)
	return v, err == nil
}

// applyPolicy は、WordPress のテーブルの方針で判定を変える。cur はブランチの今の行（削除ならその前の行）、cols は
// 比べる列、parentAdded は親で基準より後に追加された行の、自然なキーから行への対応。
func (d *differ) applyPolicy(t *tableDiff, kind merge.Kind, r *rowDiff, cur merge.Row, cols []string, parentAdded map[string]sql.Row) error {
	dec := &r.Decision
	skip := func(reason string) {
		if dec.Action != merge.NoOp {
			dec.Action, dec.Kind, dec.Columns, dec.Clean, dec.Reason = merge.Skip, merge.None, nil, nil, reason
		}
	}
	conflict := func(cols []string, reason string) {
		dec.Action, dec.Kind, dec.Columns, dec.Reason = merge.Conflict, merge.None, cols, reason
	}
	// 両側で同じ自然なキーの行を追加した。記事のメタは両方あってよいので警告にとどめ、記事とタームは競合にする。
	var dupKey string
	var dupRow sql.Row
	if dec.Action == merge.Apply && dec.Kind == merge.Insert {
		if k := d.policy.NaturalKey(t.Name, cur); k != "" {
			if row, ok := parentAdded[k]; ok {
				dupKey, dupRow = k, row
			}
		}
	}
	for _, dep := range dependsOn[kind] {
		if d.notCreated[dep.kind][fmt.Sprint(cur[dep.col])] {
			skip(fmt.Sprintf("belongs to a %s that is not merged", depName[dep.kind]))
			break
		}
	}
	// ブランチで足した行が、親にもなく、このマージでも作らない記事などにぶら下がるなら飛ばす（前のマージで飛ばした
	// 記事に、後からメタを足した、など。親に孤児の行を入れない）。
	if dec.Action == merge.Apply && dec.Kind == merge.Insert {
		for _, dep := range dependsOn[kind] {
			if d.created[dep.kind][fmt.Sprint(cur[dep.col])] {
				continue
			}
			ok, err := d.onParent(dependsTable[dep.kind], cur[dep.col])
			if err != nil {
				return err
			}
			if !ok {
				skip(fmt.Sprintf("belongs to a %s that is not on the parent", depName[dep.kind]))
				break
			}
		}
	}
	switch kind {
	case merge.KindParentOnly:
		skip("branch changes to this table are not merged")
	case merge.KindPosts:
		if ok, reason := d.policy.SkipPost(cur); ok {
			skip(reason)
			return nil
		}
		if dupRow != nil {
			d.duplicate(t, r, cur, cols, dupKey, dupRow)
			return nil
		}
		// 親に書く列に著者が含まれるなら、親にそのユーザーがいるかを確かめる（競合の行で、競合しない列に著者がある場合も）。
		var check bool
		switch dec.Action {
		case merge.Apply:
			check = dec.Kind == merge.Insert || (dec.Kind == merge.Update && slices.Contains(dec.Columns, "post_author"))
		case merge.Conflict:
			check = slices.Contains(dec.Clean, "post_author")
		}
		if !check {
			return nil
		}
		author := cur["post_author"]
		ok, err := d.userExists(author)
		if err != nil {
			return err
		}
		if !ok {
			reason := fmt.Sprintf("author %v does not exist on the parent", author)
			if dec.Action == merge.Apply {
				clean := slices.DeleteFunc(slices.Clone(dec.Columns), func(c string) bool { return c == "post_author" })
				conflict([]string{"post_author"}, reason)
				dec.Clean = clean
			} else {
				dec.Clean = slices.DeleteFunc(dec.Clean, func(c string) bool { return c == "post_author" })
				dec.Columns = append(dec.Columns, "post_author")
				dec.Reason += "; " + reason
			}
			r.Forbid = "the author does not exist on the parent"
		}
	case merge.KindTerms:
		// タームは中身が同じでも競合にする。ブロックテーマの wp_theme のタームを飛ばすと、ブランチで編集したテンプレートの
		// term_taxonomy と term_relationships も飛び、本番でテンプレートが効かない。
		if dupRow != nil {
			r.DupRow = dupRow
			conflict(nil, fmt.Sprintf("%s was also added on the parent as %s", dupKey, pkString(t.ParentDef, nil, dupRow, nil)))
		}
	case merge.KindPostmeta:
		key, _ := cur["meta_key"].(string)
		switch {
		case d.policy.SkipPostmeta(key):
			skip("editor lock")
		case dec.Action == merge.Skip:
		case dupRow != nil:
			t.Warnings = append(t.Warnings, rowWarning{ID: r.ID, Detail: dupKey + " was also added on the parent; both rows will exist"})
		}
	}
	return nil
}

// duplicate は、ブランチで追加した記事と同じ自然なキーの行 dupRow が親にも追加されたときの判定。主キーと、
// 両側で必ず食い違う列（merge.Policy.DuplicateIgnoredColumns）のほかが同じなら、親の行を残して飛ばす（WordPress が
// 両側で自動で作るナビゲーションなど。段階 64）。食い違えば競合。
func (d *differ) duplicate(t *tableDiff, r *rowDiff, cur merge.Row, cols []string, dupKey string, dupRow sql.Row) {
	r.DupRow = dupRow
	dec := &r.Decision
	parentID := pkString(t.ParentDef, nil, dupRow, nil)
	dup := toMergeRow(t.ParentDef, dupRow)
	ignored := d.policy.DuplicateIgnoredColumns(t.Name)
	for _, ord := range t.ParentDef.PKOrdinals {
		ignored = append(ignored, strings.ToLower(t.ParentDef.Columns[ord].Name))
	}
	for _, c := range cols {
		if !slices.Contains(ignored, c) && !merge.Equal(cur[c], dup[c]) {
			dec.Action, dec.Kind, dec.Columns, dec.Reason = merge.Conflict, merge.None, nil, fmt.Sprintf("%s was also added on the parent as %s", dupKey, parentID)
			return
		}
	}
	dec.Action, dec.Kind, dec.Columns, dec.Clean, dec.Reason = merge.Skip, merge.None, nil, nil, "same as the parent's "+parentID
}

// resolve は、競合の行に記録した解決を当てはめる。行を指定した解決は、記録した時点から行が変わっていれば使わない。
func (d *differ) resolve(t *tableDiff, r *rowDiff) {
	if r.Decision.Action != merge.Conflict {
		return
	}
	table := strings.ToLower(t.Name)
	for _, res := range d.resolutions {
		if res.Table == table && res.ID == r.ID {
			if res.Hash != r.Hash {
				r.Resolution = "stale"
			} else if res.Choice == "parent" || r.Forbid == "" {
				r.Resolution = res.Choice
			}
			if r.Resolution != "" {
				r.resolvedBy, r.resolvedAt = res.By, res.At
			}
			return
		}
	}
	if res, ok := d.findWildcard(table); ok && (res.Choice == "parent" || r.Forbid == "") {
		r.Resolution, r.resolvedBy, r.resolvedAt = res.Choice, res.By, res.At
	}
}

// findWildcard は、テーブル table に効くワイルドカードの解決（テーブル単位、なければ全体）を返す。
func (d *differ) findWildcard(table string) (store.Resolution, bool) {
	for _, key := range [][2]string{{strings.ToLower(table), "*"}, {"*", "*"}} {
		for _, res := range d.resolutions {
			if res.Table == key[0] && res.ID == key[1] {
				return res, true
			}
		}
	}
	return store.Resolution{}, false
}

// decisionHash は、競合の判定に使った値のハッシュ。競合した列（なければ比べた列 cols）の両側の値と、両側の変化の種類から
// 求める。比べない列（件数）や、競合に関係しない列が変わっても、記録した解決は使える。
func decisionHash(dec merge.Decision, cols []string, rows ...merge.Row) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d/%d\x00", dec.Branch, dec.Parent)
	hc := dec.Columns
	if len(hc) == 0 {
		hc = cols
	}
	for _, row := range rows {
		if row == nil {
			h.Write([]byte{0})
			continue
		}
		vals := make(sql.Row, len(hc))
		for i, c := range hc {
			vals[i] = row[c]
		}
		b, err := rowenc.EncodeRow(0, vals)
		if err != nil {
			b = []byte(fmt.Sprint(vals))
		}
		h.Write([]byte{1})
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(b))))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// parentAdded は、親で分岐の後に追加された行の、自然なキーから行への対応を返す。ブランチにも同じ主キーの行がある
// もの（前のマージでブランチから入った行）は除く。前のマージで反映しなかった行を今回作るときにも、分岐の後に親で
// 追加された同じキーの行と重ならないかを確かめるため、基準ではなく分岐の時点から見る。
func (d *differ) parentAdded(t *tableDiff, nowB, nowP *schema.Table) (map[string]sql.Row, error) {
	table := t.Name
	forkP, ok, err := d.st.GetTable(d.parent.ID, table, d.branch.ForkedAt)
	if err != nil {
		return nil, err
	}
	if !ok {
		forkP = nil
	}
	pks, err := d.changedPKs(t, d.parent.ID, nowP, forkP, d.branch.ForkedAt)
	if err != nil {
		return nil, err
	}
	out := map[string]sql.Row{}
	for _, pk := range pks {
		var rows [3]sql.Row
		for i, s := range []struct {
			branch uint64
			def    *schema.Table
			ts     uint64
		}{{d.parent.ID, forkP, d.branch.ForkedAt}, {d.parent.ID, nowP, d.snap}, {d.branch.ID, nowB, d.snap}} {
			if rows[i], err = d.getRow(s.branch, s.def, s.ts, pk); err != nil {
				return nil, err
			}
		}
		if rows[0] == nil && rows[1] != nil && rows[2] == nil {
			if k := d.policy.NaturalKey(table, toMergeRow(nowP, rows[1])); k != "" {
				out[k] = rows[1]
			}
		}
	}
	return out, nil
}

// userExists は、親の今のユーザーの表に ID id がいるかを返す。0 と NULL（著者なし）はいるとみなす。
func (d *differ) userExists(id any) (bool, error) {
	if id == nil || fmt.Sprint(id) == "0" {
		return true, nil
	}
	return d.onParent("users", id)
}

// depName は、ぶら下がる先の種類の、理由に書く名前。
var depName = map[merge.Kind]string{merge.KindPosts: "post", merge.KindTerms: "term", merge.KindTermTaxonomy: "term"}

// dependsTable は、ぶら下がる先の種類のテーブル（接頭辞の後）。
var dependsTable = map[merge.Kind]string{merge.KindPosts: "posts", merge.KindTerms: "terms", merge.KindTermTaxonomy: "term_taxonomy"}

// onParent は、親のテーブル table（接頭辞の後）に、主キーが id の行が今あるかを返す。
func (d *differ) onParent(table string, id any) (bool, error) {
	def, ok := d.parentDefs[table]
	if !ok {
		var err error
		if def, ok, err = d.st.GetTable(d.parent.ID, d.policy.Prefix+table, d.snap); err != nil {
			return false, err
		}
		if !ok {
			def = nil
		}
		if d.parentDefs == nil {
			d.parentDefs = map[string]*schema.Table{}
		}
		d.parentDefs[table] = def // ぶら下がる行ごとに呼ぶので、定義を読み直さない（スナップショットは differ ごとに固定）
	}
	if def == nil {
		return false, nil
	}
	sch, err := d.schema(def)
	if err != nil {
		return false, err
	}
	if len(def.PKOrdinals) != 1 {
		return false, fmt.Errorf("HeimDB: unexpected primary key of %s", def.Name)
	}
	row := make(sql.Row, len(def.Columns))
	row[def.PKOrdinals[0]] = id
	pk, err := rowenc.EncodePK(nil, sch, row)
	if err != nil {
		return false, err
	}
	r, err := d.getRow(d.parent.ID, def, d.snap, pk)
	return r != nil, err
}

// warnOrphanComments は、ブランチで削除した記事のコメントが親に残るなら警告する。
func (d *differ) warnOrphanComments(t *tableDiff) error {
	def, ok, err := d.st.GetTable(d.parent.ID, d.policy.Prefix+"comments", d.snap)
	if err != nil || !ok {
		return err
	}
	deleted := map[string]int{}
	for _, id := range d.deletedPosts {
		deleted[id] = 0
	}
	err = d.scanRows(d.parent.ID, def, d.snap, func(_ []byte, row sql.Row) error {
		id := fmt.Sprint(toMergeRow(def, row)["comment_post_id"])
		if n, ok := deleted[id]; ok {
			deleted[id] = n + 1
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range d.deletedPosts {
		if n := deleted[id]; n > 0 {
			t.Warnings = append(t.Warnings, rowWarning{ID: "ID=" + id, Detail: fmt.Sprintf("%d comments on the parent belong to this deleted post", n)})
		}
	}
	return nil
}

// diffByColumn は、列 idCol の値で行を対応づけて比べる（wp_options の option_name）。行数が少ないテーブルに使うので、
// 4 つの時点の全行を読む。
func (d *differ) diffByColumn(t *tableDiff, idCol string, cols []string, baseB, nowB, baseP, nowP *schema.Table) error {
	var raw [4]map[string]sql.Row
	for i, s := range []struct {
		branch uint64
		def    *schema.Table
		ts     uint64
	}{{d.branchBaseSide().branch, baseB, d.branchBaseSide().ts}, {d.branch.ID, nowB, d.snap}, {d.parentBaseSide().branch, baseP, d.parentBaseSide().ts}, {d.parent.ID, nowP, d.snap}} {
		raw[i] = map[string]sql.Row{}
		err := d.scanRows(s.branch, s.def, s.ts, func(_ []byte, row sql.Row) error {
			raw[i][fmt.Sprint(toMergeRow(s.def, row)[idCol])] = row
			return nil
		})
		if err != nil {
			return err
		}
	}
	keys := map[string]bool{}
	for k := range raw[0] {
		keys[k] = true
	}
	for k := range raw[1] {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	// 主キー（option_id）は両側で違ってよいので比べない。
	var cmp []string
	pkNames := map[string]bool{}
	for _, ord := range (nowB).PKOrdinals {
		pkNames[strings.ToLower(nowB.Columns[ord].Name)] = true
	}
	for _, c := range cols {
		if !pkNames[c] {
			cmp = append(cmp, c)
		}
	}
	vt := d.valueTypes(nowB, nowP)
	for _, k := range sorted {
		var m [4]merge.Row
		m[0], m[1] = d.sideRows(baseB, nowB, raw[0][k], raw[1][k])
		m[2], m[3] = d.sideRows(baseP, nowP, raw[2][k], raw[3][k])
		dec := merge.Decide(m[0], m[1], m[2], m[3], cmp)
		if dec.Branch == merge.None {
			continue
		}
		if skip, reason := d.policy.SkipOption(k); skip && dec.Action != merge.NoOp {
			dec.Action, dec.Kind, dec.Columns, dec.Reason = merge.Skip, merge.None, nil, reason
		}
		if dec.Action != merge.NoOp {
			r := rowDiff{ID: idCol + "=" + k, Decision: dec, BranchRow: raw[1][k], ParentRow: raw[3][k]}
			r.Hash = decisionHash(dec, cmp, m[1], m[3])
			r.conflictValues = conflictValues(d.conversionContext(), dec, cmp, m, vt)
			d.resolve(t, &r)
			t.Rows = append(t.Rows, r)
		}
	}
	return nil
}

// changedPKs は、ブランチ branch のテーブル（今の定義 now、基準の定義 base）で、基準の時刻 after より後に書かれた行の主キーを返す。
// テーブル ID が変わった（書き直した、作った）ときは、基準と今の全行の主キー。書き直したときは、その数を t.FullCompare に足す。
func (d *differ) changedPKs(t *tableDiff, branch uint64, now, base *schema.Table, after uint64) ([][]byte, error) {
	if now != nil && base != nil && now.ID == base.ID {
		return d.st.ChangedRows(branch, now.ID, after, d.snap)
	}
	seen := map[string]bool{}
	var out [][]byte
	for _, s := range []struct {
		def *schema.Table
		ts  uint64
	}{{base, after}, {now, d.snap}} {
		err := d.scanRows(branch, s.def, s.ts, func(pk []byte, _ sql.Row) error {
			if !seen[string(pk)] {
				seen[string(pk)] = true
				out = append(out, pk)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i]) < string(out[j]) })
	if now != nil && base != nil {
		// テーブルを書き直した（列を変えた）ので、全行を比べる。重さとして数える。
		t.FullCompare += len(out)
	}
	return out, nil
}

// cursorMinRows は、比べる行がこれ以上なら、行ごとに点で読まず、時点ごとの走査を主キーの順に進めて読む
// （rowCursor）。点の読み出しは層ごとにイテレータを作るので、310 万行の列を足したマージで 1,240 万回になり、881 秒
// かかった（docs/plan.md の段階 59）。少ない行は点で読むほうが速い（走査はテーブルの全行を読む）。
var cursorMinRows = 50000

// rowCursor は、時刻 ts の時点のテーブル def の行を、主キーの昇順に呼ばれる get で引く。走査を前に進めるだけなので、
// 主キーは昇順（同じものの繰り返しは可）で渡す。変わった行の一覧（ChangedRows、ChangedRowsSince）と
// addRevivedDependents は主キーの順に返す。
type rowCursor struct {
	d     *differ
	def   *schema.Table
	sc    *store.Scan
	pk    []byte
	value []byte
	done  bool
	// last は、前に引いた主キー。昇順でなければエラーにする（読めない行を、ないものとして扱わない）。
	last []byte
}

func (d *differ) openCursor(branch uint64, def *schema.Table, ts uint64) (*rowCursor, error) {
	c := &rowCursor{d: d, def: def, done: def == nil}
	if def != nil {
		sc, err := d.st.RowScanAsOf(branch, def.ID, ts, nil, nil, false)
		if err != nil {
			return nil, err
		}
		c.sc = sc
	}
	return c, nil
}

func (c *rowCursor) get(pk []byte) (sql.Row, error) {
	if c.last != nil && bytes.Compare(pk, c.last) < 0 {
		return nil, fmt.Errorf("HeimDB: rows read out of primary key order (%x after %x)", pk, c.last)
	}
	c.last = pk
	for !c.done && (c.pk == nil || bytes.Compare(c.pk, pk) < 0) {
		if err := c.d.readCheck(); err != nil {
			return nil, err
		}
		entry, value, ok, err := c.sc.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			c.done = true
			break
		}
		c.pk, c.value = entry, value
	}
	if c.done || !bytes.Equal(c.pk, pk) {
		return nil, nil
	}
	return decodeRowFor(c.def, c.value)
}

func (c *rowCursor) close() {
	if c.sc != nil {
		c.sc.Close()
	}
}

// rowSource は、主キーで行を引く方法（点の読み出しか rowCursor）。
type rowSource func(pk []byte) (sql.Row, error)

// rowSources は、sides の時点ごとに行を引く方法を返す。n 行（主キーの昇順に引く）以上なら rowCursor を使う。
// close は使い終わったら呼ぶ。
func (d *differ) rowSources(n int, sides []rowSide) (srcs []rowSource, close func(), err error) {
	var cursors []*rowCursor
	close = func() {
		for _, c := range cursors {
			c.close()
		}
	}
	for _, s := range sides {
		if n < cursorMinRows {
			s := s
			srcs = append(srcs, func(pk []byte) (sql.Row, error) { return d.getRow(s.branch, s.def, s.ts, pk) })
			continue
		}
		c, err := d.openCursor(s.branch, s.def, s.ts)
		if err != nil {
			close()
			return nil, nil, err
		}
		cursors = append(cursors, c)
		srcs = append(srcs, c.get)
	}
	return srcs, close, nil
}

// readPoint は、ブランチと時刻（読む時点）。
type readPoint = struct{ branch, ts uint64 }

// branchBaseSide は、ブランチの側の基準の行を読む時点（ブランチと時刻）。直近が取り込みなら、取り込みが読んだ親の状態
// （段階 94）。
func (d *differ) branchBaseSide() readPoint {
	if d.bases.SyncLatest() {
		return readPoint{d.parent.ID, d.bases.Sync.ParentTS}
	}
	return readPoint{d.branch.ID, d.base.BranchTS}
}

// parentBaseSide は、親の側の基準の行を読む時点。直近が取り込みなら、取り込みが読んだ親の状態。
func (d *differ) parentBaseSide() readPoint {
	if d.bases.SyncLatest() {
		return readPoint{d.parent.ID, d.bases.Sync.ParentTS}
	}
	return readPoint{d.parent.ID, d.base.ParentTS}
}

// rowSide は、行を読む時点（ブランチ、テーブルの定義、時刻）。
type rowSide struct {
	branch uint64
	def    *schema.Table
	ts     uint64
}

// getRow は、時刻 ts の時点のテーブル def の主キー pk の行を返す。なければ nil。
func (d *differ) getRow(branch uint64, def *schema.Table, ts uint64, pk []byte) (sql.Row, error) {
	if def == nil {
		return nil, nil
	}
	if err := d.readCheck(); err != nil {
		return nil, err
	}
	var row sql.Row
	err := d.st.ScanRowsAsOf(branch, def.ID, ts, pk, keyenc.PrefixEnd(pk), false, func(r store.RowVersion) (bool, error) {
		if string(r.PK) == string(pk) {
			var err error
			row, err = decodeRowFor(def, r.Value)
			return false, err
		}
		return false, nil
	})
	return row, err
}

// readCheck は、行を 1 つ読むたびに呼び、interruptEvery 行に 1 回、メモリの上限と文が止められたかを確かめる。
func (d *differ) readCheck() error {
	d.reads++
	return interrupted(d.ctx, d.reads)
}

// scanRows は、時刻 ts の時点のテーブル def の全行を主キーの順に fn へ渡す。def が nil なら何もしない。
func (d *differ) scanRows(branch uint64, def *schema.Table, ts uint64, fn func(pk []byte, row sql.Row) error) error {
	if def == nil {
		return nil
	}
	return d.st.ScanRowsAsOf(branch, def.ID, ts, nil, nil, false, func(r store.RowVersion) (bool, error) {
		if err := d.readCheck(); err != nil {
			return false, err
		}
		row, err := decodeRowFor(def, r.Value)
		if err != nil {
			return false, err
		}
		return true, fn(r.PK, row)
	})
}

func (d *differ) optionsByName(branch uint64, def *schema.Table, ts uint64) (map[string]merge.Row, error) {
	out := map[string]merge.Row{}
	err := d.scanRows(branch, def, ts, func(_ []byte, row sql.Row) error {
		m := toMergeRow(def, row)
		out[fmt.Sprint(m["option_name"])] = m
		return nil
	})
	return out, err
}

func (d *differ) schema(def *schema.Table) (sql.PrimaryKeySchema, error) {
	if s, ok := d.schemas[def]; ok {
		return s, nil
	}
	s, err := def.Schema("")
	if err == nil {
		d.schemas[def] = s
	}
	return s, err
}

func decodeRowFor(def *schema.Table, value []byte) (sql.Row, error) {
	_, row, err := rowenc.DecodeRow(value)
	if err != nil {
		return nil, err
	}
	if len(row) != len(def.Columns) {
		return nil, fmt.Errorf("HeimDB: table %s: row has %d columns, schema has %d", def.Name, len(row), len(def.Columns))
	}
	return row, nil
}

func toMergeRow(def *schema.Table, row sql.Row) merge.Row {
	if row == nil {
		return nil
	}
	m := make(merge.Row, len(row))
	for i, c := range def.Columns {
		m[strings.ToLower(c.Name)] = row[i]
	}
	return m
}

// pkString は、行の主キーを「列=値」の形で表す。記録した競合の解決は、この文字列で行と対応づける。
func pkString(nowDef, baseDef *schema.Table, now, base sql.Row) string {
	def, row := nowDef, now
	if row == nil {
		def, row = baseDef, base
	}
	parts := make([]string, len(def.PKOrdinals))
	for i, ord := range def.PKOrdinals {
		parts[i] = def.Columns[ord].Name + "=" + pkValueString(&def.Columns[ord], row[ord])
	}
	return strings.Join(parts, ",")
}

// pkValueString は、主キーの列 c の値 v を書く。日時、バイト列、ENUM は SELECT の結果と同じ書き方（jsonValue）に
// する（段階 70。ENUM の値は番号）。整数と文字列は fmt の書き方のまま（記録した競合の解決の行の名前を変えない）。
// UTF-8 として読めないか制御文字を含むバイト列（BINARY の IP など）は 0x の 16 進にする（結果の列は utf8mb4 の
// 文字列。bytesString）。
func pkValueString(c *schema.Column, v any) string {
	if b, ok := v.([]byte); ok {
		return bytesString(b)
	}
	if typ, err := c.SQLType(); err == nil && (types.IsTime(typ) || types.IsEnum(typ)) {
		// DATETIME(n) と TIMESTAMP(n) は、段階 114 で SELECT が小数の桁を固定して書くようになった（上流の #3476）が、
		// 記録した競合の解決の行の名前を変えないよう、前と同じく値のとおりの桁で書く
		if dt, ok := typ.(sql.DatetimeType); ok && dt.Precision() > 0 {
			switch typ.Type() {
			case sqltypes.Datetime:
				typ = types.DatetimeMaxPrecision
			case sqltypes.Timestamp:
				typ = types.TimestampMaxPrecision
			}
		}
		return fmt.Sprint(jsonValue(sql.NewEmptyContext(), typ, v))
	}
	return fmt.Sprint(v)
}

// bytesString は、バイト列 b を書く。UTF-8 として読めないか、制御文字（BINARY の末尾を埋めた 0x00 など）を含めば
// 0x の 16 進にする。
func bytesString(b []byte) string {
	if utf8.Valid(b) && !slices.ContainsFunc(b, func(c byte) bool { return c < 0x20 || c == 0x7f }) {
		return string(b)
	}
	return "0x" + strings.ToUpper(hex.EncodeToString(b))
}

// branchDiff は heimdb_branch_diff の本体。table が空なら要約、"*" ならすべてのテーブルの行、そうでなければそのテーブルの行。
// limit が 0 より大きければ、offset 行を飛ばして limit 行までを返し、続きがあれば最後に action が more の行を足す。
func (p *Provider) branchDiff(ctx *sql.Context, branchName, table string, offset, limit int64) (sql.RowIter, error) {
	d, err := p.newDiffer(branchName)
	if err != nil {
		return nil, err
	}
	d.ctx = ctx
	// db_version の違いは、マージではエラーだが、差分の要約では状態の行として返す（管理画面が扱えるように。段階 44）。
	// 行ごとの差分は、知らせる行がないのでエラーのまま。
	d.versionOnly = table == ""
	d.weigh = true
	tables, err := d.diff()
	if err != nil {
		return nil, err
	}
	var rows []sql.Row
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	resolvedAt := func(ts uint64) any {
		if ts == 0 {
			return nil
		}
		return formatRestoreTime(ts)
	}
	if table == "" {
		if d.versionNote != "" {
			rows = append(rows, sql.Row{"*", "db_version differs", int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), d.versionNote})
		}
		for _, t := range tables {
			if t.notExcluded != "" {
				rows = append(rows, notExcludedRow(t))
			}
			if t.Status == "" {
				continue
			}
			ins, upd, del, skipped, conflicts, resolved := t.summary()
			rows = append(rows, sql.Row{t.Name, t.Status, ins, upd, del, skipped, conflicts, resolved, int64(len(t.Warnings)), nullable(t.Detail)})
		}
		return &rowIter{rows: rows}, nil
	}
	found := false
	for _, t := range tables {
		if table != "*" && !strings.EqualFold(t.Name, table) {
			continue
		}
		found = true
		for _, r := range t.Rows {
			action := r.Decision.Action.String()
			if r.Decision.Action == merge.Apply {
				action = r.Decision.Kind.String()
			}
			parent := r.Decision.Parent.String()
			if r.DupRow != nil {
				parent = merge.Insert.String() // 親には、同じ自然なキーの別の主キーの行が足された
			}
			rows = append(rows, sql.Row{t.Name, r.ID, r.Decision.Branch.String(), parent, action,
				nullable(r.Resolution), nullable(strings.Join(r.Decision.Columns, ",")), nullable(joinDetail(r.Decision.Reason, r.forbidNote())),
				nullable(r.resolvedBy), resolvedAt(r.resolvedAt), nullable(r.conflictValues)})
		}
		if t.Status == statusSchemaDiffers {
			rows = append(rows, sql.Row{t.Name, "*", "", "", "conflict", nullable(t.Resolution), nil, t.Detail, nil, nil, nil})
		}
		if t.Status == statusExcluded {
			rows = append(rows, sql.Row{t.Name, "*", "", "", statusExcluded, nil, nil, t.Detail, nil, nil, nil})
		}
		if t.notExcluded != "" {
			rows = append(rows, sql.Row{t.Name, "*", "", "", statusNotExcluded, nil, nil, notExcludedRow(t)[9], nil, nil, nil})
		}
		for _, c := range t.Columns {
			rows = append(rows, sql.Row{t.Name, c.ID, c.Decision.Branch.String(), c.Decision.Parent.String(), c.action(),
				nullable(c.Resolution), nil, nullable(c.Decision.Reason), nullable(c.resolvedBy), resolvedAt(c.resolvedAt), nil})
		}
		for _, c := range t.Indexes {
			rows = append(rows, sql.Row{t.Name, c.ID, c.Decision.Branch.String(), c.Decision.Parent.String(), c.action(),
				nullable(c.Resolution), nil, nullable(c.Decision.Reason), nullable(c.resolvedBy), resolvedAt(c.resolvedAt), nil})
		}
		for _, w := range t.Warnings {
			rows = append(rows, sql.Row{t.Name, w.ID, "", "", "warning", nil, nil, w.Detail, nil, nil, nil})
		}
	}
	if !found && table != "*" {
		return nil, fmt.Errorf("HeimDB: table %q not found on either side", table)
	}
	if limit > 0 {
		rows = pageRows(rows, branchName, table, offset, limit)
	}
	return &rowIter{rows: rows}, nil
}

// pageRows は、行ごとの差分の rows から、offset 行を飛ばして limit 行を返す。続きがあれば、残りの数と次の呼び出しを
// 書いた more の行を最後に足す（段階 63。CALL は LIMIT で包めないので）。
func pageRows(rows []sql.Row, branchName, table string, offset, limit int64) []sql.Row {
	n := int64(len(rows))
	start := min(offset, n)
	end := start + min(limit, n-start) // start+limit は、件数に大きな数を渡すとあふれる
	out := rows[start:end:end]
	if end < n {
		next := fmt.Sprintf("%d more rows; next: CALL heimdb_branch_diff('%s', '%s', %d, %d)", n-end, branchName, table, end, limit)
		out = append(out, sql.Row{"*", "*", "", "", "more", nil, nil, next, nil, nil, nil})
	}
	return out
}

// conflictValues は、競合した行の、競合した列の両側の今の値を JSON（{"列":{"branch":値,"parent":値}}）で返す（段階 63。
// ブランチのユーザーは親を読めないので）。競合した列がなければ、両側に行があれば食い違った列（同じ自然なキーの重複）、
// なければ変更した側が変えた列（削除と変更の競合など）。行のない側の値は null。競合でなければ、または列がなければ空。
// m は、ブランチの基準と今、親の基準と今（重複の競合では相手の行）の行。typ は、ブランチと親の今の行の列の型。
func conflictValues(ctx *sql.Context, dec merge.Decision, cols []string, m [4]merge.Row, typ [2]map[string]sql.Type) string {
	if dec.Action != merge.Conflict {
		return ""
	}
	names := dec.Columns
	if len(names) == 0 && m[1] != nil && m[3] != nil {
		for _, c := range cols {
			if !merge.Equal(m[1][c], m[3][c]) {
				names = append(names, c)
			}
		}
	}
	if len(names) == 0 {
		for _, c := range cols {
			if (dec.Branch == merge.Update && !merge.Equal(m[0][c], m[1][c])) || (dec.Parent == merge.Update && !merge.Equal(m[2][c], m[3][c])) {
				names = append(names, c)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	type sides struct {
		Branch any `json:"branch"`
		Parent any `json:"parent"`
	}
	vals := make(map[string]sides, len(names))
	for _, c := range names {
		vals[c] = sides{jsonValue(ctx, typ[0][c], m[1][c]), jsonValue(ctx, typ[1][c], m[3][c])}
	}
	b, _ := json.Marshal(vals) // 文字列と nil だけなので失敗しない
	return string(b)
}

// valueTypes は、ブランチと親の今の定義の列の型（列の名前の小文字から）を返す。テーブルのない側は nil。
func (d *differ) valueTypes(nowB, nowP *schema.Table) [2]map[string]sql.Type {
	var out [2]map[string]sql.Type
	for i, def := range []*schema.Table{nowB, nowP} {
		if def == nil {
			continue
		}
		sch, _ := d.schema(def) // 読めない定義の表は、差分の前の読み出しで失敗している
		out[i] = make(map[string]sql.Type, len(sch.Schema))
		for _, c := range sch.Schema {
			out[i][strings.ToLower(c.Name)] = c.Type
		}
	}
	return out
}

// jsonValue は、行の値を JSON に書く値にする。SELECT の結果と同じく、列の型でテキストにする（DATE は日付だけ、
// DECIMAL は桁をそろえ、ゼロの日時は 0000-00-00 00:00:00）。NULL は nil。型がわからなければ fmt の書き方。
func jsonValue(ctx *sql.Context, typ sql.Type, v any) any {
	if v == nil {
		return nil
	}
	if typ != nil {
		if val, err := typ.SQL(ctx, nil, v); err == nil {
			return val.ToString()
		}
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}
