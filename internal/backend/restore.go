package backend

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 過去の時点への復元（docs/plan.md の段階 8）。
//
//	CALL heimdb_restore_preview('main', '2026-10-03 10:00:00');             -- 何が戻るかを表示するだけ
//	CALL heimdb_restore('main', '2026-10-03 10:00:00');                     -- コメントとユーザー以外を戻す
//	CALL heimdb_restore('main', '2026-10-03 10:00:00', 'wp_options,wp_posts');
//	CALL heimdb_restore('main', '2026-10-03 10:00:00', '*');                -- すべてのテーブルを戻す
//
// 復元は、過去のバージョンを新しいバージョンとして書く 1 つのコミットで、履歴は消えない。結果の最後の行に、復元を取り消す（復元の
// 直前の状態に戻す）呼び出しを出す。

var restoreSchema = sql.Schema{
	{Name: "table_name", Type: types.LongText},
	{Name: "action", Type: types.LongText},
	{Name: "inserts", Type: types.Int64},
	{Name: "updates", Type: types.Int64},
	{Name: "deletes", Type: types.Int64},
	{Name: "detail", Type: types.LongText, Nullable: true},
}

// 復元でのテーブルの扱い。
const (
	restoreRows     = "restore rows"
	restoreDrop     = "drop"
	restoreRedefine = "restore definition"
	// restoreNotRestored は、既定の復元で戻さないユーザーのテーブルに、時刻の後の見張りの対象の変化があること
	// （docs/plan.md の段階 34）。何も書かない。
	restoreNotRestored = "not restored"
)

// restoreTable は、1 つのテーブルの復元の内容。
type restoreTable struct {
	Name   string
	Action string
	// At は時刻 T の定義、Now は今の定義（なければ nil）。
	At, Now *schema.Table
	// 行の変更の数。行そのものは持たず、適用するときに変わった行（pks）をもう一度読む（docs/plan.md の段階 59）。
	Deletes, Updates, Inserts int
	// pks は、時刻 T の後に変わった行の主キー（行の数が cursorMinRows 以上なら主キーの順）。
	pks [][]byte
	// deletedIDs と writtenIDs は、記事のテーブルで、復元が消す記事と、書く（変える、足す）記事の ID。
	deletedIDs, writtenIDs map[string]bool
	Warnings               []string
}

// restorePlan は、ブランチを時刻 ts の状態に戻す内容。
type restorePlan struct {
	branch store.BranchInfo
	// ts は戻す時刻、now は今として読んだ時刻（復元の直前の状態）。
	ts, now uint64
	tables  []*restoreTable
	// ctx は、手続きを呼んだ文の context（読みながら、文が止められたかを確かめる。differ.ctx）。
	ctx *sql.Context
	// release は、時刻 ts の登録（GC がその時刻より先に境界を進めない）を解く。使い終わったら呼ぶ。
	release func()
	// recountComments は、記事とコメントの片方だけを戻すとき、コメント数を数え直すかどうか。
	recountComments bool
	prefix          string
}

// planRestore は、ブランチ branchName を時刻 at の状態に戻す内容を求める。tablesArg は空（コメントとユーザー以外）、
// "*"（すべて）、カンマ区切りのテーブル名のどれか。
func (p *Provider) planRestore(ctx *sql.Context, branchName, at, tablesArg string, now uint64) (*restorePlan, error) {
	b, ok := p.st.BranchByName(branchName)
	if !ok {
		return nil, fmt.Errorf("HeimDB: branch %q not found", branchName)
	}
	ts, err := parseAsOf(ctx, at)
	if err != nil {
		return nil, err
	}
	// 最後のコミットより後の時刻なら、戻すものはない（何もしない）。今より先の時刻はエラー。
	if ts > uint64(time.Now().UnixMicro()) {
		return nil, fmt.Errorf("HeimDB: restore time %s is in the future", at)
	}
	// T のバージョンと定義を読み、T の定義（古いテーブル ID）を書き終えるまで、GC に T を消させない。
	release, err := p.st.Pin(ts)
	if err != nil {
		return nil, err
	}
	plan := &restorePlan{branch: b, ts: ts, now: now, release: release, ctx: ctx}
	planned := false
	defer func() {
		if !planned {
			release()
		}
	}()

	defsAt, err := p.st.ListTables(b.ID, ts)
	if err != nil {
		return nil, err
	}
	defsNow, err := p.st.ListTables(b.ID, now)
	if err != nil {
		return nil, err
	}
	byName := func(defs []*schema.Table) map[string]*schema.Table {
		m := map[string]*schema.Table{}
		for _, d := range defs {
			m[strings.ToLower(d.Name)] = d
		}
		return m
	}
	atByName, nowByName := byName(defsAt), byName(defsNow)
	names := map[string]bool{}
	for n := range atByName {
		names[n] = true
	}
	for n := range nowByName {
		names[n] = true
	}

	selected, err := p.restoreSelection(b.ID, tablesArg, names)
	if err != nil {
		return nil, err
	}
	sorted := make([]string, 0, len(selected))
	for n := range selected {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	if err := checkRenamed(sorted, selected, atByName, nowByName); err != nil {
		return nil, err
	}

	if plan.prefix, err = detectPrefix(p.st, b.ID); err != nil {
		return nil, err
	}
	for _, name := range sorted {
		at, cur := atByName[name], nowByName[name]
		t := &restoreTable{Name: name, At: at, Now: cur}
		switch {
		case at == nil && cur == nil:
			continue
		case at == nil:
			t.Name, t.Action = cur.Name, restoreDrop
		default:
			t.Name, t.Action = at.Name, restoreRows
			if cur == nil || !sameDefinition(at, cur) {
				t.Action = restoreRedefine
			}
			if err := p.planRows(plan, t); err != nil {
				return nil, fmt.Errorf("HeimDB: restore %s: %w", name, err)
			}
		}
		plan.tables = append(plan.tables, t)
	}
	plan.recountComments = selected[plan.prefix+"posts"] != selected[plan.prefix+"comments"]
	if err := p.warnRestoreComments(plan, selected); err != nil {
		return nil, err
	}
	if err := p.warnRestorePairs(plan, branchName, selected, atByName, nowByName); err != nil {
		return nil, err
	}
	if strings.TrimSpace(tablesArg) == "" {
		if err := p.warnRestoreUsers(plan, branchName, names, selected, atByName, nowByName); err != nil {
			return nil, err
		}
	}
	planned = true
	return plan, nil
}

// checkRenamed は、選んだテーブルと同じテーブル ID を、選んでいない別の名前が T の時点か今に持っていればエラーにする
// （T の後に RENAME したテーブルの片方の名前だけを戻すと、2 つの名前が同じテーブルを指してしまうため）。
func checkRenamed(sorted []string, selected map[string]bool, atByName, nowByName map[string]*schema.Table) error {
	owners := map[uint64][]string{}
	for _, m := range []map[string]*schema.Table{atByName, nowByName} {
		for n, d := range m {
			if !slices.Contains(owners[d.ID], n) {
				owners[d.ID] = append(owners[d.ID], n)
			}
		}
	}
	for _, n := range sorted {
		for _, d := range []*schema.Table{atByName[n], nowByName[n]} {
			if d == nil {
				continue
			}
			for _, other := range owners[d.ID] {
				if other != n && !selected[other] {
					return fmt.Errorf("HeimDB: table %s was renamed (the same table is also named %s now or at the restore time); include both in the restore", n, other)
				}
			}
		}
	}
	return nil
}

// restoreSelection は、復元するテーブル名（小文字）の集合を返す。
func (p *Provider) restoreSelection(branch uint64, tablesArg string, names map[string]bool) (map[string]bool, error) {
	out := map[string]bool{}
	switch strings.TrimSpace(tablesArg) {
	case "*":
		return names, nil
	case "":
		prefix, err := detectPrefix(p.st, branch)
		if err != nil {
			return nil, err
		}
		policy := merge.Policy{Prefix: prefix}
		for n := range names {
			if policy.Kind(n) != merge.KindParentOnly {
				out[n] = true
			}
		}
		return out, nil
	}
	for _, n := range strings.Split(tablesArg, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if !names[n] {
			return nil, fmt.Errorf("HeimDB: table %q does not exist now or at that time", n)
		}
		out[n] = true
	}
	return out, nil
}

// sameDefinition は、2 つの定義がバージョンとインデックスのキーの水準を除いて同じかを返す。水準は、heimdb_rebuild_indexes で
// 上げても行と定義の意味は変わらない（エントリの有無だけ）ので含めない（docs/plan.md の段階 117。含めると、作り直しの
// 前の時点への復元が定義の書き直しになって水準を下げ、作り直しの後の取り込みが表の形の違いで拒否される）。
func sameDefinition(a, b *schema.Table) bool {
	ac, bc := a.Clone(), b.Clone()
	ac.Version, bc.Version = 0, 0
	for _, t := range []*schema.Table{ac, bc} {
		for i := range t.Indexes {
			t.Indexes[i].KeyLevel = 0
		}
	}
	return reflect.DeepEqual(ac, bc)
}

// planRows は、時刻 T の定義のテーブル ID の行のうち、T の後に変わったものを、T の値に戻す変更として数える。
// 行の形はテーブル ID ごとに同じ（列を変えるとテーブル ID が変わる）なので、今の行も T の定義で読める。
func (p *Provider) planRows(plan *restorePlan, t *restoreTable) error {
	pks, err := p.st.ChangedRowsSince(plan.branch.ID, t.At.ID, plan.ts, plan.now)
	if err != nil {
		return err
	}
	t.pks = pks
	posts := strings.ToLower(t.Name) == plan.prefix+"posts"
	if posts {
		t.deletedIDs, t.writtenIDs = map[string]bool{}, map[string]bool{}
	}
	return p.eachRestoreChange(plan, t, func(before, after sql.Row) error {
		switch {
		case after == nil:
			t.Deletes++
		case before == nil:
			t.Inserts++
		default:
			t.Updates++
		}
		if posts {
			if after == nil {
				t.deletedIDs[fmt.Sprint(toMergeRow(t.At, before)["id"])] = true
			} else {
				t.writtenIDs[fmt.Sprint(toMergeRow(t.At, after)["id"])] = true
			}
		}
		return nil
	})
}

// eachRestoreChange は、テーブル t の変わった行ごとに、今の行 before と時刻 T の行 after（なければ nil）を fn に渡す。
// 同じ行は飛ばす。計画と適用で同じ行を同じ順に渡す（どちらも確定した時刻の読み出し）。
func (p *Provider) eachRestoreChange(plan *restorePlan, t *restoreTable, fn func(before, after sql.Row) error) error {
	d := &differ{st: p.st, ctx: plan.ctx}
	srcs, closeSrcs, err := d.rowSources(len(t.pks), []rowSide{{plan.branch.ID, t.At, plan.now}, {plan.branch.ID, t.At, plan.ts}})
	if err != nil {
		return err
	}
	defer closeSrcs()
	for _, pk := range t.pks {
		before, err := srcs[0](pk)
		if err != nil {
			return err
		}
		after, err := srcs[1](pk)
		if err != nil {
			return err
		}
		if (before == nil && after == nil) || (before != nil && after != nil && rowsIdentical(before, after)) {
			continue
		}
		if err := fn(before, after); err != nil {
			return err
		}
	}
	return nil
}

func rowsIdentical(a, b sql.Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !merge.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// warnRestoreComments は、コメントを戻さないときに、復元で消える（または T の時点になかった）記事にコメントが残るなら警告する。
func (p *Provider) warnRestoreComments(plan *restorePlan, selected map[string]bool) error {
	prefix, err := detectPrefix(p.st, plan.branch.ID)
	if err != nil {
		return err
	}
	postsName, commentsName := prefix+"posts", prefix+"comments"
	if !selected[postsName] || selected[commentsName] {
		return nil
	}
	var posts *restoreTable
	for _, t := range plan.tables {
		if strings.ToLower(t.Name) == postsName {
			posts = t
		}
	}
	if posts == nil {
		return nil
	}
	if posts.Action == restoreDrop {
		return nil
	}
	gone := posts.deletedIDs
	if len(gone) == 0 {
		return nil
	}
	def, ok, err := p.st.GetTable(plan.branch.ID, commentsName, plan.now)
	if err != nil || !ok {
		return err
	}
	counts := map[string]int{}
	d := &differ{st: p.st, ctx: plan.ctx}
	if err := d.scanRows(plan.branch.ID, def, plan.now, func(_ []byte, row sql.Row) error {
		id := fmt.Sprint(toMergeRow(def, row)["comment_post_id"])
		if gone[id] {
			counts[id]++
		}
		return nil
	}); err != nil {
		return err
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		posts.Warnings = append(posts.Warnings, fmt.Sprintf("%d comments remain on post %s, which the restore deletes", counts[id], id))
	}
	return nil
}

// restorePairs は、親の表と、その行を ID で指す子の表の組（docs/plan.md の段階 62）。列の名前は小文字。
var restorePairs = []struct{ parent, parentCol, child, childCol, noun string }{
	{"posts", "id", "postmeta", "post_id", "post"},
	{"posts", "id", "term_relationships", "object_id", "post"},
	{"comments", "comment_id", "commentmeta", "comment_id", "comment"},
	{"users", "id", "usermeta", "user_id", "user"},
}

// warnRestorePairs は、組の表の片方だけを戻すとき、T の後に親の行と子の行の両方が変わった ID があれば警告する
// （記事だけを戻すと、消した記事がメタと分類のない姿で戻り、後から作った記事のメタが残り、中身をメタに持つ記事は
// 題名だけが戻る）。片側だけが変わった ID は、戻しても食い違わないので数えない。
func (p *Provider) warnRestorePairs(plan *restorePlan, branchName string, selected map[string]bool, atByName, nowByName map[string]*schema.Table) error {
	var warned []*restoreTable
	var missing []string // 警告した組の、戻さない表
	warn := func(t *restoreTable, other, msg string) {
		t.Warnings = append(t.Warnings, msg)
		if !slices.Contains(warned, t) {
			warned = append(warned, t)
		}
		if !slices.Contains(missing, other) {
			missing = append(missing, other)
		}
	}
	for _, pr := range restorePairs {
		parent, child := plan.prefix+pr.parent, plan.prefix+pr.child
		from, other, fromCol, otherCol := parent, child, pr.parentCol, pr.childCol
		switch {
		case selected[parent] && !selected[child]:
		case selected[child] && !selected[parent]:
			from, other, fromCol, otherCol = child, parent, pr.childCol, pr.parentCol
		default:
			continue
		}
		var t *restoreTable // 選んだ表は、T か今にあるので計画に入っている
		for _, rt := range plan.tables {
			if strings.ToLower(rt.Name) == from {
				t = rt
			}
		}
		otherDef := nowByName[other]
		if otherDef == nil {
			otherDef = atByName[other]
		}
		if otherDef == nil {
			continue // 組の片方の表がないサイト
		}
		// T と今でテーブル ID が違う（作った、消した、書き直した）表は、変わった行を拾えないので比べない。
		rewritten := ""
		for _, n := range []string{from, other} {
			if a, c := atByName[n], nowByName[n]; a == nil || c == nil || a.ID != c.ID {
				rewritten = n
				break
			}
		}
		if rewritten != "" {
			what := otherDef.Name + " is not restored and was"
			if rewritten != other {
				what = otherDef.Name + " is not restored, and " + t.Name + " was"
			}
			warn(t, otherDef.Name, what+" rewritten, created or dropped after the restore time, so rows were not compared")
			continue
		}
		// 戻す表は、計画で拾った変わった行（t.pks）から。戻さない表は、ID の集合を作らずに重なりだけを数える。
		mine := map[string]bool{}
		if err := p.eachChangedLinkID(plan, t, fromCol, func(id string) { mine[id] = true }); err != nil {
			return err
		}
		if len(mine) == 0 {
			continue
		}
		pks, err := p.st.ChangedRowsSince(plan.branch.ID, otherDef.ID, plan.ts, plan.now)
		if err != nil {
			return err
		}
		hit := map[string]bool{}
		if err := p.eachChangedLinkID(plan, &restoreTable{At: otherDef, pks: pks}, otherCol, func(id string) {
			if mine[id] {
				hit[id] = true
			}
		}); err != nil {
			return err
		}
		if len(hit) == 0 {
			continue
		}
		both := make([]string, 0, len(hit))
		for id := range hit {
			both = append(both, id)
		}
		slices.SortFunc(both, func(a, b string) int {
			if len(a) != len(b) {
				return len(a) - len(b)
			}
			return strings.Compare(a, b)
		})
		const shown = 5
		ids := pr.noun + " " + both[0]
		if len(both) > 1 {
			ids = pr.noun + "s " + strings.Join(both[:min(len(both), shown)], ", ")
		}
		if len(both) > shown {
			ids += fmt.Sprintf(" (and %d more)", len(both)-shown)
		}
		warn(t, otherDef.Name, fmt.Sprintf("%s also changed in %s after the restore time, which is not restored", ids, otherDef.Name))
	}
	if len(warned) == 0 {
		return nil
	}
	// 当たった組の表をすべて含む呼び出しを 1 つだけ出す（組ごとに出すと、片方を流しても揃わない）。
	names := make([]string, 0, len(selected)+len(missing))
	for n := range selected {
		names = append(names, n)
	}
	sort.Strings(names)
	call := fmt.Sprintf("to restore them together: CALL heimdb_restore('%s', '%s', '%s')", branchName, formatRestoreTime(plan.ts), strings.Join(append(names, missing...), ","))
	for _, t := range warned {
		t.Warnings = append(t.Warnings, call)
	}
	return nil
}

// eachChangedLinkID は、表 t の変わった行（t.pks。T と今で同じテーブル ID）の、今と T の行の列 col の値を fn に渡す。
// 記事のメタのうち、記事の編集や保存のたびに WordPress が足し引きするもの（restoreNoiseMeta）は渡さない。
func (p *Provider) eachChangedLinkID(plan *restorePlan, t *restoreTable, col string, fn func(id string)) error {
	meta := strings.HasSuffix(strings.ToLower(t.At.Name), "postmeta")
	return p.eachRestoreChange(plan, t, func(before, after sql.Row) error {
		for _, r := range []sql.Row{before, after} {
			if r == nil {
				continue
			}
			m := toMergeRow(t.At, r)
			if k, _ := m["meta_key"].(string); meta && restoreNoiseMeta(k) {
				continue
			}
			fn(fmt.Sprint(m[col]))
		}
		return nil
	})
}

// restoreNoiseMeta は、組の確かめで数えない記事のメタか。マージで飛ばす編集のロック（merge.Policy.SkipPostmeta）と、
// 公開済みの記事を保存するたびに足され、cron の ping が消すもの（_encloseme、_pingme、_trackbackme）。
func restoreNoiseMeta(key string) bool {
	return merge.Policy{}.SkipPostmeta(key) || key == "_encloseme" || key == "_pingme" || key == "_trackbackme"
}

// warnRestoreUsers は、既定の復元で戻さないユーザーのテーブル（wp_users、wp_usermeta）に、時刻の後の見張りの対象の
// 変化（ユーザーの追加、パスワードとメールアドレスの変更、administrator の付与）があれば、not restored の行を足す。
// 乗っ取られた後に復元しても、後から足された管理者は残るため。
func (p *Provider) warnRestoreUsers(plan *restorePlan, branchName string, names, selected map[string]bool, atByName, nowByName map[string]*schema.Table) error {
	sorted := make([]string, 0, len(names))
	for n := range names {
		if !selected[n] {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)
	d := &differ{st: p.st, ctx: plan.ctx}
	for _, name := range sorted {
		def0, def1 := atByName[name], nowByName[name]
		// テーブル ID が同じなら、定義が変わっていても（インデックスの追加など）行の形は同じなので比べる（段階 43）。
		if def0 == nil || def1 == nil || def0.ID != def1.ID {
			continue
		}
		if kind, _ := watchDef(def1); kind != watchUsers && kind != watchUsermeta {
			continue
		}
		pks, err := p.st.ChangedRowsSince(plan.branch.ID, def1.ID, plan.ts, plan.now)
		if err != nil {
			return err
		}
		t := &restoreTable{Name: def1.Name, Action: restoreNotRestored, At: def0, Now: def1}
		for _, pk := range pks {
			old, err := d.getRow(plan.branch.ID, def1, plan.ts, pk)
			if err != nil {
				return err
			}
			cur, err := d.getRow(plan.branch.ID, def1, plan.now, pk)
			if err != nil {
				return err
			}
			w := watchDescribe(def1, old, cur)
			if len(w) == 0 {
				continue
			}
			t.Warnings = append(t.Warnings, w...)
			switch {
			case old == nil:
				t.Inserts++
			case cur == nil:
				t.Deletes++
			default:
				t.Updates++
			}
		}
		if len(t.Warnings) > 0 {
			t.Warnings = append(t.Warnings, fmt.Sprintf("to restore users too: CALL heimdb_restore('%s', '%s', '*')", branchName, formatRestoreTime(plan.ts)))
			plan.tables = append(plan.tables, t)
		}
	}
	return nil
}

// rows は、復元の内容を結果の行にする。
func (plan *restorePlan) rows() []sql.Row {
	var out []sql.Row
	for _, t := range plan.tables {
		if t.Action == restoreRows && t.Deletes+t.Updates+t.Inserts == 0 {
			continue
		}
		var detail any
		if len(t.Warnings) > 0 {
			detail = strings.Join(t.Warnings, "; ")
		}
		out = append(out, sql.Row{t.Name, t.Action, int64(t.Inserts), int64(t.Updates), int64(t.Deletes), detail})
	}
	return out
}

// restorePreview は heimdb_restore_preview の本体。何も書かない。
func (p *Provider) restorePreview(ctx *sql.Context, branchName, at, tablesArg string) (sql.RowIter, error) {
	plan, err := p.planRestore(ctx, branchName, at, tablesArg, p.st.LastCommitted())
	if err != nil {
		return nil, err
	}
	defer plan.release()
	return &rowIter{rows: plan.rows()}, nil
}

// restore は heimdb_restore の本体。ブランチの書き込みロックを取ってから内容を求め、1 つのトランザクションで書く。
func (p *Provider) restore(ctx *sql.Context, branchName, at, tablesArg string) (sql.RowIter, error) {
	b, ok := p.st.BranchByName(branchName)
	if !ok {
		return nil, fmt.Errorf("HeimDB: branch %q not found", branchName)
	}
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil && s.txn.HoldsLock(b.ID) {
		return nil, fmt.Errorf("HeimDB: commit or roll back the current transaction before restoring")
	}
	defer p.lockBranchOp(b.ID)()
	tx := p.st.BeginInternal("restore")
	defer tx.Rollback()
	if err := tx.LockForWrite(b.ID); err != nil {
		return nil, err
	}
	plan, err := p.planRestore(ctx, branchName, at, tablesArg, p.st.LastCommitted())
	if err != nil {
		return nil, err
	}
	defer plan.release()
	tx.ReadTS()
	db, ok := p.database(p.dbName(b))
	if !ok {
		return nil, fmt.Errorf("HeimDB: branch %q not found", branchName)
	}
	for _, t := range plan.tables {
		if t.Action == restoreNotRestored {
			continue
		}
		if err := p.applyRestore(ctx, tx, db, plan, t); err != nil {
			return nil, fmt.Errorf("HeimDB: restore %s: %w", t.Name, err)
		}
	}
	if err := p.recountRestoredComments(ctx, tx, db, plan); err != nil {
		return nil, err
	}
	// 子のブランチのマージの基準を、戻した時刻より前のものにする（マージを戻した後に、もう一度マージできるように。
	// すでに入った変更をもう一度マージしても、両側が同じ値なので何もしない）。
	if err := tx.DropMergeBasesAfter(b.ID, plan.ts); err != nil {
		return nil, err
	}
	// 戻した時刻より後の、このブランチへの取り込みの記録を消す。残すと、戻した行（取り込む前の値）がブランチの変更に見え、
	// マージで親の変更を消してしまう（docs/plan.md の段階 94）。
	if err := tx.DropSyncsAfter(b.ID, plan.ts); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rowCounts.forget(p.st, b.ID)
	out := plan.rows()
	wrote := false
	for _, r := range out {
		if r[1] == restoreNotRestored {
			logrus.Warnf("HeimDB: restore of %s to %s left %s as it is: %s", branchName, formatRestoreTime(plan.ts), r[0], r[5])
			continue
		}
		wrote = true
		if r[5] != nil {
			logrus.Warnf("HeimDB: restore of %s to %s: %s: %s", branchName, formatRestoreTime(plan.ts), r[0], r[5])
		}
	}
	if !wrote {
		return &rowIter{rows: out}, nil // 何も書いていないので、取り消すものもない
	}
	if b.ID == store.MainBranchID {
		// 戻した表の定義に、水準の古いインデックスがありうる（T の後に消した表など）。起動の後と同じく裏で作り直す
		// （docs/plan.md の段階 121。動かしていなければ何もしない）。
		p.kickIndexRebuild()
	}
	// 取り消しの時刻は、夏時間の切り替わりやセッションの time_zone に左右されないよう、UTC のオフセット付きで出す。
	undo := fmt.Sprintf("CALL heimdb_restore('%s', '%s'", branchName, formatRestoreTime(plan.now))
	if tablesArg != "" {
		undo += fmt.Sprintf(", '%s'", tablesArg)
	}
	out = append(out, sql.Row{"*", "undo", int64(0), int64(0), int64(0), undo + ")"})
	return &rowIter{rows: out}, nil
}

// formatRestoreTime は、内部の時刻を、parseAsOf がどの time_zone でも同じ時刻として読む形（UTC のオフセット付き）にする。
func formatRestoreTime(ts uint64) string {
	return time.UnixMicro(int64(ts)).UTC().Format("2006-01-02 15:04:05.000000-07:00")
}

// applyRestore は、1 つのテーブルの復元をトランザクションに書く。
func (p *Provider) applyRestore(ctx *sql.Context, tx *store.Txn, db *Database, plan *restorePlan, t *restoreTable) error {
	branch := db.branch
	switch t.Action {
	case restoreDrop:
		return tx.DeleteTable(branch, t.Now.Name)
	case restoreRedefine:
		// インデックス ID を再利用しないよう、次のインデックス ID は戻さない（T の後に作ったインデックスのエントリが
		// キー空間に残っているため。GC が後で消す）。名前が変わった（T の後に RENAME した）テーブルの今の名前は、別の
		// 名前として削除される。
		at := t.At.Clone()
		// 今の定義で同じ ID のインデックスのキーの水準は下げない（段階 117。下の rebuildIndexes が今の行からエントリを作る）
		if t.Now != nil && t.Now.ID == at.ID {
			for i := range at.Indexes {
				for _, now := range t.Now.Indexes {
					if now.ID == at.Indexes[i].ID {
						at.Indexes[i].KeyLevel = max(at.Indexes[i].KeyLevel, now.KeyLevel)
					}
				}
			}
		}
		used, err := p.st.UsedIndexIDs(branch, at.ID)
		if err != nil {
			return err
		}
		for _, id := range used {
			at.NextIndexID = max(at.NextIndexID, id+1)
		}
		if t.Now != nil && t.Now.ID == at.ID {
			at.NextIndexID = max(at.NextIndexID, t.Now.NextIndexID)
		}
		t.At = at
		if err := tx.PutTable(branch, at); err != nil {
			return err
		}
	}
	def := t.At
	if t.Action == restoreRows && t.Now != nil {
		// 定義はバージョンとキーの水準のほかは同じ（sameDefinition）。行は今の定義のエディタで書き、今の水準のインデックスの
		// エントリも保守する（段階 117。T の定義の水準が低いと、作り直したエントリが古いまま残る）
		def = t.Now
	}
	tbl, err := newTable(ctx, db, def)
	if err != nil {
		return err
	}
	if t.Action == restoreRedefine {
		// T の後に消したインデックスは、その後の行の変更を反映していない。T の定義のインデックスを、今の行から作り直す。
		if err := rebuildIndexes(tx, tbl); err != nil {
			return err
		}
		if err := carryAutoIncrement(ctx, db, t, tbl); err != nil {
			return err
		}
	}
	ed := &editor{t: tbl, tx: tx, noWatch: true}
	// 一意な列の値を行どうしで入れ替えた後でも途中で一意制約に当たらないよう、変更する行もいったん消してから、
	// T の値の行を入れる（主キーは同じなので、削除の印の上への挿入になる）。変わった行を 2 回読む（消す回と入れる回）。
	// 計画と数が違えば（読み出しは確定した時刻なので起きないはず）、何も書かずに止める。
	var deleted, inserted int
	if err := p.eachRestoreChange(plan, t, func(before, after sql.Row) error {
		if before == nil {
			return nil
		}
		deleted++
		return ed.Delete(ctx, before)
	}); err != nil {
		return err
	}
	if err := p.eachRestoreChange(plan, t, func(before, after sql.Row) error {
		if after == nil {
			return nil
		}
		inserted++
		return ed.Insert(ctx, after)
	}); err != nil {
		return err
	}
	if deleted != t.Deletes+t.Updates || inserted != t.Updates+t.Inserts {
		return fmt.Errorf("HeimDB: the rows to restore changed while restoring (planned %d deletes and %d inserts, found %d and %d)", t.Deletes+t.Updates, t.Updates+t.Inserts, deleted, inserted)
	}
	return nil
}

// rebuildIndexes は、テーブルのキーを持つインデックスのエントリを、トランザクションから見える今の行で作り直す。
func rebuildIndexes(tx *store.Txn, t *Table) error {
	branch, id := t.db.branch, t.def.ID
	// 行とエントリを集めずに、走査しながら消して書く（docs/plan.md の段階 59）。トランザクションの走査は、作った時点の
	// バッチの変更だけを見るので、走査の途中で書いたものは見えない。
	for _, ki := range t.keyIndexes() {
		n := 0
		if err := tx.ScanIndexLatest(branch, id, ki.def.ID, nil, nil, false, func(entry, _ []byte) (bool, error) {
			n++
			if err := tx.DeleteIndexEntry(branch, id, ki.def.ID, bytes.Clone(entry)); err != nil {
				return false, err
			}
			return true, interrupted(nil, n)
		}); err != nil {
			return err
		}
		if err := tx.ScanRowsLatest(branch, id, nil, nil, false, func(r store.RowVersion) (bool, error) {
			n++
			row, err := t.decodeRow(r.Value)
			if err != nil {
				return false, err
			}
			entry, value, err := t.entry(ki, row)
			if err != nil {
				return false, err
			}
			if err := tx.PutIndexEntry(branch, id, ki.def.ID, entry, value); err != nil {
				return false, err
			}
			return true, interrupted(nil, n)
		}); err != nil {
			return err
		}
	}
	return nil
}

// carryAutoIncrement は、テーブルを T の時点のテーブル ID に戻すとき、今のテーブルで払い出した値を再利用しないよう、
// T の時点のテーブル ID のカウンタを今のカウンタまで進める。
func carryAutoIncrement(ctx *sql.Context, db *Database, t *restoreTable, at *Table) error {
	if t.Now == nil || t.Now.ID == t.At.ID || at.autoIncrementOrdinal() < 0 {
		return nil
	}
	cur, err := newTable(ctx, db, t.Now)
	if err != nil {
		return err
	}
	if cur.autoIncrementOrdinal() < 0 {
		return nil
	}
	cn, err := cur.autoIncrement(ctx)
	if err != nil {
		return err
	}
	ca, err := at.autoIncrement(ctx)
	if err != nil {
		return err
	}
	if next := cn.Peek(); next > 1 {
		ca.Observe(next - 1)
	}
	return nil
}

// recountRestoredComments は、記事とコメントの片方だけを戻したとき、記事のコメント数を、トランザクションから見える
// 承認済みのコメントで数え直す。記事だけを戻すと戻した記事のコメント数が T の時点の値になり、コメントだけを戻すと
// 記事のコメント数が今のままになるため。記事だけなら戻した記事（変更、追加）、コメントだけならすべての記事を数え直す。
func (p *Provider) recountRestoredComments(ctx *sql.Context, tx *store.Txn, db *Database, plan *restorePlan) error {
	if !plan.recountComments {
		return nil
	}
	var posts, comments *restoreTable
	for _, t := range plan.tables {
		switch strings.ToLower(t.Name) {
		case plan.prefix + "posts":
			posts = t
		case plan.prefix + "comments":
			comments = t
		}
	}
	// 戻した後のコメントの表と記事の表の定義。
	commentsDef, ok, err := p.st.GetTable(db.branch, plan.prefix+"comments", plan.now)
	if err != nil {
		return err
	}
	if !ok {
		commentsDef = nil
	}
	if comments != nil {
		commentsDef = comments.At
		if comments.Action == restoreDrop {
			commentsDef = nil
		}
	}
	postsDef, ok, err := p.st.GetTable(db.branch, plan.prefix+"posts", plan.now)
	if err != nil || !ok {
		return err
	}
	var ids map[string]bool // nil ならすべての記事
	if posts != nil {
		if posts.Action == restoreDrop || posts.Updates+posts.Inserts == 0 {
			return nil
		}
		postsDef = posts.At
		ids = posts.writtenIDs
	} else if comments == nil {
		return nil
	}
	counts := map[string]int64{}
	if commentsDef != nil {
		if err := tx.ScanRowsLatest(db.branch, commentsDef.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
			row, err := decodeRowFor(commentsDef, r.Value)
			if err != nil {
				return false, err
			}
			c := toMergeRow(commentsDef, row)
			if fmt.Sprint(c["comment_approved"]) == "1" {
				counts[fmt.Sprint(c["comment_post_id"])]++
			}
			return true, nil
		}); err != nil {
			return err
		}
	}
	tbl, err := newTable(ctx, db, postsDef)
	if err != nil {
		return err
	}
	var rows []sql.Row
	if err := tx.ScanRowsLatest(db.branch, postsDef.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
		row, err := decodeRowFor(postsDef, r.Value)
		if err == nil && (ids == nil || ids[fmt.Sprint(toMergeRow(postsDef, row)["id"])]) {
			rows = append(rows, row)
		}
		return err == nil, err
	}); err != nil {
		return err
	}
	ed := &editor{t: tbl, tx: tx, noWatch: true}
	for _, row := range rows {
		if _, err := setColumn(ctx, ed, row, "comment_count", counts[fmt.Sprint(toMergeRow(postsDef, row)["id"])]); err != nil {
			return err
		}
	}
	return nil
}
