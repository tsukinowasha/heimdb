package backend

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 競合の解決とマージ（docs/plan.md の段階 7b）。
//
//	CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=4', 'branch');  -- 1 件の競合をブランチの側で解決
//	CALL heimdb_branch_resolve('staging', 'wp_posts', '*', 'parent');     -- テーブルの競合をまとめて親の側で解決
//	CALL heimdb_branch_resolve('staging', '*', '*', 'none');              -- 全体のワイルドカードの解決を取り消す
//	CALL heimdb_branch_merge('staging');                                  -- 解決していない競合があれば拒否
//
// 行を指定した解決は、ワイルドカードより優先する。行を指定した解決が stale になったら、ワイルドカードがあっても
// その行は未解決になる（決め直してもらうため）。
// branchResolve は、競合の解決を記録する。行を指定するときは、その行が今の差分で競合になっていなければならない。
func (p *Provider) branchResolve(ctx *sql.Context, branchName, table, id, choice string, c caller) error {
	switch choice {
	case "branch", "parent", "none":
	default:
		return fmt.Errorf("HeimDB: resolution must be 'branch', 'parent' or 'none', not %q", choice)
	}
	table = strings.ToLower(table)
	if table == "*" && id != "*" {
		return fmt.Errorf("HeimDB: a row id needs a table name")
	}
	// ブランチのユーザーは、行を指定した記録だけ（記録した値のハッシュを持ち、値が変われば使わない。段階 63）。
	if !c.admin && id == "*" {
		return fmt.Errorf("HeimDB: a branch user must name a row; only an administrator can resolve with '*'")
	}
	d, err := p.newDiffer(branchName)
	if err != nil {
		return err
	}
	d.ctx = ctx
	res := store.Resolution{Table: table, ID: id, Choice: choice, Base: d.bases.Key(), By: c.user, At: uint64(time.Now().UnixMicro())}
	put := func() error {
		if err := p.st.PutResolution(d.branch.ID, res); err != nil {
			return err
		}
		logrus.Infof("HeimDB: %s resolved %s %s on branch %s: %s", c.user, table, id, branchName, choice)
		return nil
	}
	if choice == "none" || id == "*" {
		return put()
	}
	tables, err := d.diff()
	if err != nil {
		return err
	}
	for _, t := range tables {
		if strings.ToLower(t.Name) != table {
			continue
		}
		rows := make([]*rowDiff, 0, len(t.Rows)+len(t.Indexes))
		for i := range t.Rows {
			rows = append(rows, &t.Rows[i])
		}
		for i := range t.Columns {
			rows = append(rows, &t.Columns[i].rowDiff)
		}
		for i := range t.Indexes {
			rows = append(rows, &t.Indexes[i].rowDiff)
		}
		for _, r := range rows {
			if r.ID != id {
				continue
			}
			if r.Decision.Action != merge.Conflict {
				return fmt.Errorf("HeimDB: %s %s is not a conflict (%s)", t.Name, id, r.Decision.Action)
			}
			if choice == "branch" && r.Forbid != "" {
				return fmt.Errorf("HeimDB: %s %s cannot take the branch's side: %s", t.Name, id, r.Forbid)
			}
			res.Hash = r.Hash
			return put()
		}
		return fmt.Errorf("HeimDB: %s %s is not in the diff", t.Name, id)
	}
	return fmt.Errorf("HeimDB: table %q is not in the diff", table)
}

// mergeSchema は heimdb_branch_merge の結果。
var mergeSchema = diffSummarySchema

// branchMerge は、ブランチの変更を親に反映する。
// dryRun なら、同じ経路を最後まで通ってコミットせずに捨てる（解決の記録とマージの起点も残す。段階 44）。
func (p *Provider) branchMerge(ctx *sql.Context, branchName string, dryRun bool) (sql.RowIter, error) {
	b, ok := p.st.BranchByName(branchName)
	if !ok {
		return nil, refuseBranchNotFound.errorf("branch %q not found", branchName)
	}
	if b.Parent == 0 {
		return nil, refuseNoParent.errorf("branch %q has no parent", branchName)
	}
	if p.st.IsReplica() {
		return nil, refuseReadOnlyReplica.errorf("this server is a read-only replica")
	}
	// 親の書き込みロックをこの接続のトランザクションが持っていると、マージは自分を待って止まる。
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil && s.txn.HoldsLock(b.Parent) {
		return nil, refuseInTransaction.errorf("commit or roll back the current transaction before merging")
	}
	defer p.lockBranchOp(b.ID)()
	tx := p.st.BeginInternal("merge")
	defer tx.Rollback()
	// 親のロックを取ってから差分を読む。ロックを持っている間、親の最新の状態は変わらない。
	if err := tx.LockForWrite(b.Parent); err != nil {
		return nil, err
	}
	d, err := p.newDiffer(branchName)
	if err != nil {
		return nil, err
	}
	tables, err := d.diff()
	if err != nil {
		return nil, err
	}
	if err := checkResolved(tables); err != nil {
		return nil, err
	}
	parentDB, ok := p.database(p.dbName(d.parent))
	if !ok {
		return nil, fmt.Errorf("HeimDB: parent branch of %q not found", branchName)
	}

	// ブランチで作ったテーブルとインデックスの変更も、行と同じトランザクションに書く。マージは 1 つのコミットで、
	// 失敗すれば何も残らない。新しいテーブル ID だけは先に払い出す（store.Txn は ID を払い出せない。使われなくても害はない）。
	rewriteIDs := map[*tableDiff]uint64{}
	// wrote は、マージが書く親のテーブルの名前（戻すための文に入れる）。
	wrote := map[string]bool{}
	for _, t := range tables {
		switch {
		case t.Status == statusNewTable:
			if t.ParentDef, err = newTableLike(p.st, d.parent.ID, t.BranchDef); err != nil {
				return nil, err
			}
		case t.Merged != nil && t.Status != statusSchemaDiffers:
			// 列を変えるテーブルは、親を新しいテーブル ID に書き直す（段階 13）。
			if rewriteIDs[t], err = allocateTableID(p.st); err != nil {
				return nil, err
			}
		}
	}
	tx.ReadTS() // スナップショットを、ID を払い出した後に決める
	for _, t := range tables {
		if t.Status == statusNewTable {
			if err := tx.PutTable(d.parent.ID, t.ParentDef); err != nil {
				return nil, err
			}
			wrote[t.ParentDef.Name] = true
			continue
		}
		if t.Status == statusSchemaDiffers || t.ParentDef == nil || t.BranchDef == nil {
			continue
		}
		if id, ok := rewriteIDs[t]; ok {
			if err := rewriteParentTable(ctx, tx, parentDB, t, id); err != nil {
				return nil, fmt.Errorf("HeimDB: merge columns of %s: %w", t.Name, err)
			}
			wrote[t.ParentDef.Name] = true
		}
		if t.indexSummary() == "" {
			continue
		}
		if err := applyIndexChanges(ctx, tx, parentDB, t); err != nil {
			return nil, fmt.Errorf("HeimDB: merge indexes of %s: %w", t.Name, err)
		}
		wrote[t.ParentDef.Name] = true
	}
	failed := func(err error) (sql.RowIter, error) { return nil, err }

	m := &merger{ctx: ctx, tx: tx, db: parentDB, d: d, target: d.parent.ID, insertedPosts: map[string]bool{}, touchedPosts: map[string]bool{}, touchedTT: map[string]bool{}, defs: map[string]*schema.Table{}, wrote: wrote}
	for _, t := range tables {
		if t.ParentDef != nil {
			m.defs[strings.ToLower(t.Name)] = t.ParentDef
		}
	}
	var out []sql.Row
	for _, t := range tables {
		if t.Status == "" || t.Status == statusSchemaDiffers || t.ParentDef == nil || len(t.Rows) == 0 {
			continue
		}
		if err := m.applyTable(t); err != nil {
			return failed(fmt.Errorf("HeimDB: merge %s: %w", t.Name, err))
		}
	}
	if m.applied > 0 {
		if err := m.recount(); err != nil {
			return failed(err)
		}
		if err := m.clearRewriteRules(); err != nil {
			return failed(err)
		}
	}
	if err := tx.SetMergeBase(b.ID, d.snap); err != nil {
		return failed(err)
	}
	if !dryRun {
		if err := p.commitWatched(ctx.Session, tx, "merge of branch "+branchName); err != nil {
			return failed(err)
		}
		rowCounts.forget(p.st, b.Parent)
		if err := p.st.ClearResolutions(b.ID); err != nil {
			return nil, err
		}
	}
	zero := int64(0)
	for _, t := range tables {
		if t.notExcluded != "" {
			out = append(out, notExcludedRow(t))
		}
		if t.Status == "" || t.Status == statusUnchanged {
			continue // 変えるものがないテーブル（全行を比べた重さは差分の要約で知らせる）
		}
		ins, upd, del, skipped, _, resolved := t.summary()
		status := "merged"
		switch t.Status {
		case statusSchemaDiffers, statusDroppedOnBranch, statusDroppedOnParent:
			status = t.Status + " (not merged)"
		case statusExcluded:
			// 件数は、反映しなかったブランチの側の変更（段階 60）。
			out = append(out, sql.Row{t.Name, statusExcluded, ins, upd, del, zero, zero, zero, zero, t.Detail})
			continue
		}
		var detail any
		if s := joinDetail(t.definitionSummary(), t.weightSummary()); s != "" && t.Status != statusSchemaDiffers {
			detail = s
		}
		out = append(out, sql.Row{t.Name, status, ins, upd, del, skipped, int64(0), resolved, int64(len(t.Warnings)), detail})
	}
	if dryRun {
		out = append(out, sql.Row{"*", "dry run", zero, zero, zero, zero, zero, zero, zero, "nothing was committed"})
		return &rowIter{rows: out}, nil
	}
	if len(wrote) > 0 {
		// 戻すための文。時刻は差分を読んだ時点で、親のロックを取った後なので、マージの直前の親の状態そのもの。
		names := make([]string, 0, len(wrote))
		for n := range wrote {
			names = append(names, n)
		}
		sort.Strings(names)
		undo := fmt.Sprintf("CALL heimdb_restore('%s', '%s', '%s')", d.parent.Name, formatRestoreTime(d.snap), strings.Join(names, ","))
		out = append(out, sql.Row{"*", "undo", zero, zero, zero, zero, zero, zero, zero, undo})
	}
	return &rowIter{rows: out}, nil
}

// checkResolved は、解決していない競合が残っていればエラーを返す。
func checkResolved(tables []*tableDiff) error {
	var pending []string
	for _, t := range tables {
		if t.Status == statusSchemaDiffers && t.Resolution != "parent" {
			pending = append(pending, t.Name+" (schema)")
		}
		for i := range t.Rows {
			r := &t.Rows[i]
			if _, ok := r.effect(); !ok {
				s := t.Name + " " + r.ID
				if r.Resolution == "stale" {
					s += " (stale resolution)"
				} else if n := r.forbidNote(); n != "" {
					s += " (" + n + ")"
				}
				pending = append(pending, s)
			}
		}
		for _, c := range t.definitionChanges() {
			if !c.ok {
				s := t.Name + " " + c.r.ID
				if c.r.Resolution == "stale" {
					s += " (stale resolution)"
				}
				pending = append(pending, s)
			}
		}
	}
	if len(pending) == 0 {
		return nil
	}
	more := ""
	if len(pending) > 10 {
		more = fmt.Sprintf(" and %d more", len(pending)-10)
		pending = pending[:10]
	}
	return refuseUnresolvedConflicts.errorf("unresolved conflicts: %s%s; resolve them with heimdb_branch_resolve", strings.Join(pending, ", "), more)
}

// allocateTableID は、新しいテーブル ID を払い出す（マージのトランザクションで使う。使われなくても害はない）。
func allocateTableID(st *store.Store) (uint64, error) {
	var id uint64
	err := st.Update(func(w *store.WriteTx) error {
		var err error
		id, err = w.NextTableID()
		return err
	})
	return id, err
}

// newTableLike は、親 branch に作るテーブルの定義（def と同じで、テーブル ID だけを新しく払い出したもの）を返す。
// 定義はマージのトランザクションで書く。親に同じ名前のテーブルがあればエラー。
func newTableLike(st *store.Store, branch uint64, def *schema.Table) (*schema.Table, error) {
	if _, ok, err := st.GetTable(branch, def.Name, store.Latest); err != nil {
		return nil, err
	} else if ok {
		return nil, sql.ErrTableAlreadyExists.New(def.Name)
	}
	var created *schema.Table
	err := st.Update(func(w *store.WriteTx) error {
		id, err := w.NextTableID()
		if err != nil {
			return err
		}
		created = def.Clone()
		created.ID = id
		return nil
	})
	return created, err
}

// merger は、差分を親のトランザクションに書く。
type merger struct {
	ctx *sql.Context
	tx  *store.Txn
	db  *Database
	d   *differ
	// target は書く先のブランチ（マージなら親、取り込みならブランチ。db のブランチ）。
	target uint64

	applied int
	// insertedPosts はブランチから入った記事、touchedPosts は状態が変わりうる記事、touchedTT は件数が変わりうるタームの
	// term_taxonomy_id。
	insertedPosts, touchedPosts, touchedTT map[string]bool
	// defs は、マージで書く親のテーブルの定義（新しく作ったテーブルと、書き直したテーブルはまだ確定していない）。
	defs map[string]*schema.Table
	// wrote は、マージが書いた親のテーブルの名前。
	wrote map[string]bool
}

func (m *merger) applyTable(t *tableDiff) error {
	tbl, err := newTable(m.ctx, m.db, t.ParentDef)
	if err != nil {
		return err
	}
	ed := &editor{t: tbl, tx: m.tx}
	kind := m.d.policy.Kind(t.Name)
	excluded := m.d.policy.ExcludedColumns(t.Name)
	// ブランチの行は、親の定義の列と型にそろえてから書く（親だけにある列は既定値。段階 13）。
	branchConv, err := newRowConverter(m.ctx, t.BranchDef, t.ParentDef)
	if err != nil {
		return err
	}
	// 親の行は、親のテーブルを書き直したなら新しい形にする。
	parentRow := func(row sql.Row) (sql.Row, error) {
		if t.parentConv == nil || row == nil {
			return row, nil
		}
		return t.parentConv(row)
	}
	// 行の値を持たない rowDiff は、差分を読んだのと同じ時点の行を、主キーの順に走査で読み直す。
	var fetch func(r *rowDiff) error
	if t.lazyRows {
		srcs, closeSrcs, err := m.d.rowSources(cursorMinRows, []rowSide{{m.d.branch.ID, t.BranchDef, m.d.snap}, {m.d.parent.ID, t.parentRead, m.d.snap}})
		if err != nil {
			return err
		}
		defer closeSrcs()
		fetch = func(r *rowDiff) error {
			var err error
			if r.BranchRow, err = srcs[0](r.pk); err != nil {
				return err
			}
			if r.hasParent {
				if r.ParentRow, err = srcs[1](r.pk); err != nil {
					return err
				}
				if r.ParentRow == nil {
					return refuseParentChanged.errorf("the parent's row changed while merging")
				}
			}
			return nil
		}
	}
	for i := range t.Rows {
		r := &t.Rows[i]
		op, _ := r.effect()
		if op.Kind == merge.None {
			continue
		}
		if r.pk != nil {
			if err := fetch(r); err != nil {
				return fmt.Errorf("%s: %w", r.ID, err)
			}
		}
		var before, after, branch sql.Row
		if branch, err = branchConv.convert(m.ctx, r.BranchRow); err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
		switch {
		case op.Kind == merge.Insert:
			after = branch
			err = ed.Insert(m.ctx, after)
		case op.Kind == merge.Delete:
			if before, err = parentRow(r.ParentRow); err == nil {
				err = ed.Delete(m.ctx, before)
			}
		case op.Kind == merge.Update && op.Copy:
			// 親の同じ自然なキーの行に、主キー、guid、件数の列以外のブランチの値を写す。親だけにある列は親の値のまま。
			skip := append(pkNames(t.ParentDef), "guid")
			skip = append(skip, excluded...)
			if before, err = parentRow(r.DupRow); err == nil {
				after = copyColumns(t.ParentDef, t.ParentDef, branch, before, func(c string) bool {
					return !slices.Contains(skip, c) && t.BranchDef.ColumnIndex(c) >= 0
				})
				err = ed.Update(m.ctx, before, after)
			}
		case op.Kind == merge.Update:
			if before, err = parentRow(r.ParentRow); err == nil {
				after = copyColumns(t.ParentDef, t.ParentDef, branch, before, func(c string) bool { return slices.Contains(op.Columns, c) })
				err = ed.Update(m.ctx, before, after)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
		m.applied++
		m.wrote[t.ParentDef.Name] = true
		m.track(kind, t.ParentDef, op, before, after)
		if r.pk != nil {
			r.BranchRow, r.ParentRow = nil, nil // 読み直した値は持ち続けない
		}
	}
	return nil
}

// track は、マージの後に数え直す記事とタームを記録する。
func (m *merger) track(kind merge.Kind, def *schema.Table, op rowOp, before, after sql.Row) {
	for _, row := range []sql.Row{before, after} {
		if row == nil {
			continue
		}
		mr := toMergeRow(def, row)
		switch kind {
		case merge.KindPosts:
			id := fmt.Sprint(mr["id"])
			m.touchedPosts[id] = true
			if op.Kind == merge.Insert {
				m.insertedPosts[id] = true
			}
		case merge.KindTermRelationships, merge.KindTermTaxonomy:
			m.touchedTT[fmt.Sprint(mr["term_taxonomy_id"])] = true
		}
	}
}

// copyColumns は、親の行 base を複製し、want が真の列にブランチの行 src の値を入れる。
func copyColumns(from, to *schema.Table, src, base sql.Row, want func(col string) bool) sql.Row {
	out := slices.Clone(base)
	for i, c := range to.Columns {
		if !want(strings.ToLower(c.Name)) {
			continue
		}
		if j := from.ColumnIndex(c.Name); j >= 0 {
			out[i] = src[j]
		}
	}
	return out
}

// parentTable は、親の今のテーブル name を、マージのトランザクションに書けるエディタとともに返す。なければ nil。
func (m *merger) parentTable(name string) (*Table, *editor, error) {
	def, ok := m.defs[strings.ToLower(m.d.policy.Prefix+name)]
	if !ok {
		var err error
		if def, ok, err = m.d.st.GetTable(m.target, m.d.policy.Prefix+name, store.Latest); err != nil || !ok {
			return nil, nil, err
		}
	}
	tbl, err := newTable(m.ctx, m.db, def)
	if err != nil {
		return nil, nil, err
	}
	return tbl, &editor{t: tbl, tx: m.tx}, nil
}

// scanLatest は、親のテーブルの全行を、マージのトランザクションの変更を重ねて読む。
func (m *merger) scanLatest(t *Table, fn func(row sql.Row) error) error {
	return m.tx.ScanRowsLatest(m.target, t.def.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
		row, err := t.decodeRow(r.Value)
		if err != nil {
			return false, err
		}
		return true, fn(row)
	})
}

// recount は、ブランチから入った記事のコメント数と、影響したタームの件数を親の側で数え直す。
func (m *merger) recount() error {
	posts, postsEd, err := m.parentTable("posts")
	if err != nil || posts == nil {
		return err
	}
	// 記事の今の状態（ID → 行）。
	byID := map[string]sql.Row{}
	if err := m.scanLatest(posts, func(row sql.Row) error {
		byID[fmt.Sprint(toMergeRow(posts.def, row)["id"])] = row
		return nil
	}); err != nil {
		return err
	}

	// コメント数: 承認済みのコメントの数（wp_update_comment_count と同じ）。
	if len(m.insertedPosts) > 0 {
		comments, _, err := m.parentTable("comments")
		if err != nil {
			return err
		}
		counts := map[string]int64{}
		if comments != nil {
			if err := m.scanLatest(comments, func(row sql.Row) error {
				c := toMergeRow(comments.def, row)
				if fmt.Sprint(c["comment_approved"]) == "1" {
					counts[fmt.Sprint(c["comment_post_id"])]++
				}
				return nil
			}); err != nil {
				return err
			}
		}
		for id := range m.insertedPosts {
			if err := m.setColumn(postsEd, byID[id], "comment_count", counts[id]); err != nil {
				return err
			}
		}
	}

	rels, _, err := m.parentTable("term_relationships")
	if err != nil {
		return err
	}
	tt, ttEd, err := m.parentTable("term_taxonomy")
	if err != nil || rels == nil || tt == nil {
		return err
	}
	objects := map[string][]string{}
	if err := m.scanLatest(rels, func(row sql.Row) error {
		r := toMergeRow(rels.def, row)
		ttID, obj := fmt.Sprint(r["term_taxonomy_id"]), fmt.Sprint(r["object_id"])
		objects[ttID] = append(objects[ttID], obj)
		if m.touchedPosts[obj] {
			m.touchedTT[ttID] = true
		}
		return nil
	}); err != nil {
		return err
	}
	return m.scanLatestCollect(tt, func(row sql.Row) error {
		r := toMergeRow(tt.def, row)
		id := fmt.Sprint(r["term_taxonomy_id"])
		if !m.touchedTT[id] {
			return nil
		}
		return m.setColumn(ttEd, row, "count", termCount(fmt.Sprint(r["taxonomy"]), objects[id], byID, posts.def))
	})
}

// scanLatestCollect は scanLatest と同じだが、全行を読んでから fn を呼ぶ（fn が同じテーブルに書くため）。
func (m *merger) scanLatestCollect(t *Table, fn func(row sql.Row) error) error {
	var rows []sql.Row
	if err := m.scanLatest(t, func(row sql.Row) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		return err
	}
	for _, row := range rows {
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

// termCount は、タームの件数を数える。WordPress はタクソノミーごとに登録された関数で数えるが、DB からは登録が
// 見えないので近似する: リンクのカテゴリは関連の数、それ以外は公開済みの記事（添付ファイルは親が公開済みのもの）の数。
func termCount(taxonomy string, objects []string, posts map[string]sql.Row, def *schema.Table) int64 {
	if taxonomy == "link_category" {
		return int64(len(objects))
	}
	var n int64
	for _, id := range objects {
		row, ok := posts[id]
		if !ok {
			continue
		}
		p := toMergeRow(def, row)
		switch status := fmt.Sprint(p["post_status"]); {
		case status == "publish":
			n++
		case status == "inherit" && fmt.Sprint(p["post_type"]) == "attachment":
			if parent, ok := posts[fmt.Sprint(p["post_parent"])]; ok && fmt.Sprint(toMergeRow(def, parent)["post_status"]) == "publish" {
				n++
			}
		}
	}
	return n
}

// setColumn は setColumn と同じで、書いたらそのテーブルを記録する。
func (m *merger) setColumn(ed *editor, row sql.Row, col string, v int64) error {
	changed, err := setColumn(m.ctx, ed, row, col, v)
	if changed {
		m.wrote[ed.t.def.Name] = true
	}
	return err
}

// setColumn は、行 row の列 col を v にする。値が同じなら何もしない。書いたら changed が真。
func setColumn(ctx *sql.Context, ed *editor, row sql.Row, col string, v int64) (changed bool, err error) {
	if row == nil {
		return false, nil
	}
	i := ed.t.def.ColumnIndex(col)
	if i < 0 {
		return false, nil
	}
	conv, _, err := ed.t.sch.Schema[i].Type.Convert(ctx, v)
	if err != nil {
		return false, err
	}
	if merge.Equal(row[i], conv) {
		return false, nil
	}
	next := slices.Clone(row)
	next[i] = conv
	return true, ed.Update(ctx, row, next)
}

// clearRewriteRules は、親の rewrite_rules を空にする（WordPress が次のリクエストで作り直す）。
func (m *merger) clearRewriteRules() error {
	options, ed, err := m.parentTable("options")
	if err != nil || options == nil {
		return err
	}
	var target sql.Row
	if err := m.scanLatest(options, func(row sql.Row) error {
		if r := toMergeRow(options.def, row); r["option_name"] == "rewrite_rules" && r["option_value"] != "" {
			target = row
		}
		return nil
	}); err != nil || target == nil {
		return err
	}
	next := slices.Clone(target)
	next[options.def.ColumnIndex("option_value")] = ""
	m.wrote[options.def.Name] = true
	return ed.Update(m.ctx, target, next)
}
