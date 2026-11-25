package backend

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 親の変更をブランチに取り込む（docs/plan.md の段階 94）。
//
//	CALL heimdb_branch_sync('staging', 'dry_run');  -- 書かずに、取り込む件数と競合を確かめる
//	CALL heimdb_branch_sync('staging');             -- 競合があれば何も書かずに拒否する
//	CALL heimdb_branch_sync('staging', 'branch');   -- 競合した列はブランチの値を残す
//	CALL heimdb_branch_sync('staging', 'parent');   -- 競合した列は親の値を採る
//
// 基準の中身（両側の変化を測る元）は、直近が取り込みなら前の取り込みが読んだ親の状態、直近がマージならそのマージが読んだ
// ブランチの状態、どちらもなければ分岐の時点。候補は、親が自分の層で前の取り込み（なければ分岐）の後に書いた行。
// 写した行は 1 つのコミットでブランチに書き、取り込みの記録（m/sync）を残す。後のマージは、直近が取り込みなら両側とも
// この親の状態を基準に比べるので、写した行はブランチの変更に数えない。
//
// コメントとユーザーの表（merge.KindParentOnly）は写さない。マージでも行を反映しないので、基準が進んでも影響しない（段階 132）。
// 定義はマージで列ごとに比べるので、違えばほかの表と同じく拒否する（拒否しないと、本番だけにある列を次のマージが消す）。ブランチに
// 表がなければ（WordPress のインストールより前に分岐した）、ほかの新しい表と同じく表ごと写す。

// syncRefused は、マージと共通の拒否（branch_not_found など）の文を、取り込みの文にする。
func syncRefused(err error) error {
	if e, ok := asRefusal(err); ok {
		return mysql.NewSQLError(e.Num, e.State, "%s", strings.Replace(e.Message, "HeimDB: merge refused", "HeimDB: sync refused", 1))
	}
	return err
}

// branchSync は heimdb_branch_sync の本体。mode は ""（競合があれば拒否）、"branch"、"parent"。
func (p *Provider) branchSync(ctx *sql.Context, branchName, mode string, dryRun bool) (sql.RowIter, error) {
	rows, err := p.syncBranch(ctx, branchName, mode, dryRun)
	if err != nil {
		return nil, syncRefused(err)
	}
	return rows, nil
}

func (p *Provider) syncBranch(ctx *sql.Context, branchName, mode string, dryRun bool) (sql.RowIter, error) {
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
	// ブランチの書き込みロックをこの接続のトランザクションが持っていると、取り込みは自分を待って止まる。
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil && s.txn.HoldsLock(b.ID) {
		return nil, refuseInTransaction.errorf("commit or roll back the current transaction before syncing")
	}
	defer p.lockBranchOp(b.ID)()
	tx := p.st.BeginInternal("sync")
	defer tx.Rollback()
	// ブランチのロックを取ってから読む。親はロックせず、スナップショット（d.snap）で読む（本番の書き込みを止めない）。
	if err := tx.LockForWrite(b.ID); err != nil {
		return nil, err
	}
	d, err := p.newDiffer(branchName)
	if err != nil {
		return nil, err
	}
	d.versionOnly = true
	if err := d.checkDBVersion(); err != nil {
		return nil, err
	}
	if d.versionNote != "" {
		return nil, refuseDBVersionDiffers.errorf("db_version differs (%s)", strings.Replace(d.versionNote, "before merging", "before syncing", 1))
	}
	db, ok := p.database(p.dbName(b))
	if !ok {
		return nil, fmt.Errorf("HeimDB: branch %q not found", branchName)
	}
	s := &syncer{p: p, ctx: ctx, tx: tx, d: d, db: db, mode: mode,
		notCopied: map[merge.Kind]map[string]bool{merge.KindPosts: {}, merge.KindTerms: {}, merge.KindTermTaxonomy: {}}}
	s.m = &merger{ctx: ctx, tx: tx, db: db, d: d, target: b.ID, insertedPosts: map[string]bool{}, touchedPosts: map[string]bool{},
		touchedTT: map[string]bool{}, defs: map[string]*schema.Table{}, wrote: map[string]bool{}}
	bases := d.bases
	s.merged = bases.Merge.ParentTS != b.ForkedAt // マージの記録がある（記録のキーはマージのコミット時刻で、分岐時刻より後）
	switch {
	case bases.SyncLatest():
		s.base = readPoint{branch: d.parent.ID, ts: bases.Sync.ParentTS}
	case s.merged:
		s.base = readPoint{branch: b.ID, ts: bases.Merge.BranchTS}
	default:
		s.base = readPoint{branch: d.parent.ID, ts: b.ForkedAt}
	}
	s.window = b.ForkedAt
	if bases.HasSync {
		s.window = bases.Sync.ParentTS
	}
	return s.run(dryRun)
}

// syncer は、親の変更をブランチのトランザクションに書く。d の branch がブランチ（書く先）、parent が親（出どころ）。
type syncer struct {
	p    *Provider
	ctx  *sql.Context
	tx   *store.Txn
	d    *differ
	db   *Database
	m    *merger
	mode string
	// base は基準の中身を読む時点。window は、親の変わった行を探し始める時刻（前の取り込みが読んだ親の時刻か分岐時刻）。
	base   readPoint
	window uint64
	// merged は、マージの記録があること。基準がマージの時点のブランチ（直近がマージ）なら、ID の衝突を見分けるのに
	// 親のマージの時点も読む。
	merged bool
	// notCopied は、写さなかった親の記事、ターム、term_taxonomy の ID（それにぶら下がる行も写さない）。
	notCopied map[merge.Kind]map[string]bool
	// conflicts は、解決していない競合（mode が空のとき）。
	conflicts []string
}

// syncTable は 1 つの表の取り込みの件数。
type syncTable struct {
	name                       string
	status, detail             string
	ins, upd, del, skip, confl int64
	resolved                   int64
	warnings                   []string
}

func (s *syncer) run(dryRun bool) (sql.RowIter, error) {
	d := s.d
	names, err := s.tableNames()
	if err != nil {
		return nil, err
	}
	get := func(branch uint64, name string, ts uint64) (*schema.Table, error) {
		def, ok, err := d.st.GetTable(branch, name, ts)
		if !ok {
			def = nil
		}
		return def, err
	}
	type plan struct {
		t                   *syncTable
		pNow, cNow, baseDef *schema.Table
		create              *schema.Table
	}
	var plans []plan
	var differs []string
	// 1 回目: 表の定義を比べる。新しい表の ID は先に払い出す（store.Txn は ID を払い出せない。使われなくても害はない）。
	for _, name := range names {
		pNow, err := get(d.parent.ID, name, d.snap)
		if err != nil {
			return nil, err
		}
		cNow, err := get(d.branch.ID, name, d.snap)
		if err != nil {
			return nil, err
		}
		baseDef, err := get(s.base.branch, name, s.base.ts)
		if err != nil {
			return nil, err
		}
		t := &syncTable{name: name}
		for _, def := range []*schema.Table{pNow, cNow} {
			if def != nil {
				t.name = def.Name
			}
		}
		if pattern, core := exclusion(d.excludes, d.policy.Prefix, t.name); pattern != "" && !core {
			// 外した表は写さない（ブランチの表をそのまま使う。段階 60）。マージと同じく excluded の行を出す（件数は 0）。
			if pNow != nil && cNow != nil {
				t.status, t.detail = statusExcluded, fmt.Sprintf("matches the pattern '%s'; the branch's table is kept as it is", pattern)
				plans = append(plans, plan{t: t})
			}
			continue
		}
		pl := plan{t: t, pNow: pNow, cNow: cNow, baseDef: baseDef}
		switch {
		case pNow == nil:
			if cNow != nil && baseDef != nil {
				t.status, t.detail = statusDroppedOnParent+" (not synced)", "dropping tables is not synced"
				plans = append(plans, pl)
			}
			continue
		case cNow == nil && baseDef != nil:
			t.status, t.detail = statusDroppedOnBranch+" (not synced)", "the parent's changes to this table are not synced"
			plans = append(plans, pl)
			continue
		case cNow == nil:
			if pl.create, err = newTableLike(d.st, d.branch.ID, pNow); err != nil {
				return nil, err
			}
			t.status = statusNewTable
		case !sameShape(pNow, cNow):
			differs = append(differs, t.name)
			continue
		case d.policy.Kind(name) == merge.KindParentOnly:
			changed, err := d.st.ChangedRows(d.parent.ID, pNow.ID, s.window, d.snap)
			if err != nil {
				return nil, err
			}
			if len(changed) == 0 {
				continue
			}
			t.status, t.detail = "not synced", "comments and users are not synced; recreate the branch to get them"
		}
		plans = append(plans, pl)
	}
	if len(differs) > 0 {
		return nil, refuseSchemaDiffers.errorf("the definitions of %s differ between the branch and the parent; run the same plugin update or ALTER TABLE on the branch first",
			strings.Join(differs, ", "))
	}
	s.tx.ReadTS() // スナップショットを、ID を払い出した後に決める

	// 2 回目: 行を写す。記事、ターム、term_taxonomy を先に（ぶら下がる行の判定に使う）。
	var tables []*syncTable
	for _, pl := range plans {
		tables = append(tables, pl.t)
		if pl.t.status != "" && pl.create == nil {
			continue
		}
		if pl.create != nil {
			if err := s.tx.PutTable(d.branch.ID, pl.create); err != nil {
				return nil, err
			}
			s.m.wrote[pl.create.Name] = true
			s.m.defs[strings.ToLower(pl.create.Name)] = pl.create
			if err := s.copyTable(pl.t, pl.pNow, pl.create); err != nil {
				return nil, fmt.Errorf("HeimDB: sync %s: %w", pl.t.name, err)
			}
			continue
		}
		if err := s.syncRows(pl.t, pl.pNow, pl.cNow, pl.baseDef); err != nil {
			return nil, fmt.Errorf("HeimDB: sync %s: %w", pl.t.name, err)
		}
	}
	if len(s.conflicts) > 0 {
		more := ""
		pending := s.conflicts
		if len(pending) > 10 {
			more = fmt.Sprintf(" and %d more", len(pending)-10)
			pending = pending[:10]
		}
		return nil, refuseUnresolvedConflicts.errorf("conflicts: %s%s; take a side for all of them with heimdb_branch_sync(name, 'branch' or 'parent'), or make the rows on the branch equal to the parent's",
			strings.Join(pending, ", "), more)
	}
	if s.m.applied > 0 {
		if err := s.m.recount(); err != nil {
			return nil, err
		}
		if err := s.m.clearRewriteRules(); err != nil {
			return nil, err
		}
	}
	if err := s.tx.RecordSync(d.branch.ID, d.snap); err != nil {
		return nil, err
	}
	if !dryRun {
		if err := s.p.commitWatched(s.ctx.Session, s.tx, "sync of branch "+d.branch.Name+" from "+d.parent.Name); err != nil {
			return nil, err
		}
		rowCounts.forget(d.st, d.branch.ID)
		if err := d.st.ClearResolutions(d.branch.ID); err != nil {
			return nil, err
		}
	}
	zero := int64(0)
	var out []sql.Row
	sort.Slice(tables, func(i, j int) bool { return tables[i].name < tables[j].name })
	for _, t := range tables {
		if t.status == "" {
			if t.ins+t.upd+t.del+t.skip+t.resolved == 0 && len(t.warnings) == 0 {
				continue
			}
			t.status = "synced"
		}
		var detail any
		if s := joinDetail(t.detail, summarizeWarnings(t.warnings)); s != "" {
			detail = s
		}
		out = append(out, sql.Row{t.name, t.status, t.ins, t.upd, t.del, t.skip, zero, t.resolved, int64(len(t.warnings)), detail})
	}
	if dryRun {
		out = append(out, sql.Row{"*", "dry run", zero, zero, zero, zero, zero, zero, zero, "nothing was committed"})
		return &rowIter{rows: out}, nil
	}
	if len(s.m.wrote) > 0 {
		// 戻すための文。時刻は差分を読んだ時点で、ブランチのロックを取った後なので、取り込みの直前のブランチの状態そのもの。
		// 戻すと取り込みの記録も消える（DropSyncsAfter）。
		names := make([]string, 0, len(s.m.wrote))
		for n := range s.m.wrote {
			names = append(names, n)
		}
		sort.Strings(names)
		undo := fmt.Sprintf("CALL heimdb_restore('%s', '%s', '%s')", d.branch.Name, formatRestoreTime(d.snap), strings.Join(names, ","))
		out = append(out, sql.Row{"*", "undo", zero, zero, zero, zero, zero, zero, zero, undo})
	}
	return &rowIter{rows: out}, nil
}

// tableNames は、両側の今の表の名前（小文字）を、記事、ターム、term_taxonomy、ほかの順に返す。
func (s *syncer) tableNames() ([]string, error) {
	d := s.d
	seen := map[string]bool{}
	for _, b := range []uint64{d.branch.ID, d.parent.ID} {
		defs, err := d.st.ListTables(b, d.snap)
		if err != nil {
			return nil, err
		}
		for _, def := range defs {
			seen[strings.ToLower(def.Name)] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
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
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	return names, nil
}

// sameShape は、2 つの表の定義が、テーブル ID、インデックス ID、バージョンを除いて同じかを返す。
func sameShape(a, b *schema.Table) bool {
	ac, bc := a.Clone(), b.Clone()
	for _, t := range []*schema.Table{ac, bc} {
		t.ID, t.Version, t.NextIndexID = 0, 0, 0
		for i := range t.Indexes {
			t.Indexes[i].ID = 0
		}
	}
	return sameDefinition(ac, bc)
}

// copyTable は、親で作られた表の全行を、ブランチに作った表 def に写す。
func (s *syncer) copyTable(t *syncTable, pNow, def *schema.Table) error {
	tbl, err := newTable(s.ctx, s.db, def)
	if err != nil {
		return err
	}
	ed := &editor{t: tbl, tx: s.tx}
	kind := s.d.policy.Kind(t.name)
	return s.d.scanRows(s.d.parent.ID, pNow, s.d.snap, func(_ []byte, row sql.Row) error {
		if err := ed.Insert(s.ctx, row); err != nil {
			return err
		}
		t.ins++
		s.m.applied++
		s.m.track(kind, def, rowOp{Kind: merge.Insert}, nil, row)
		return nil
	})
}

// syncRows は、両側にある表の、親の変わった行をブランチに写す。
func (s *syncer) syncRows(t *syncTable, pNow, cNow, baseDef *schema.Table) error {
	d := s.d
	kind := d.policy.Kind(t.name)
	excluded := d.policy.ExcludedColumns(t.name)
	var cols []string
	for _, c := range pNow.Columns {
		if n := strings.ToLower(c.Name); !slices.Contains(excluded, n) && n != schema.RowHashColumn {
			cols = append(cols, n)
		}
	}
	tbl, err := newTable(s.ctx, s.db, cNow)
	if err != nil {
		return err
	}
	ed := &editor{t: tbl, tx: s.tx}
	conv, err := newRowConverter(s.ctx, pNow, cNow)
	if err != nil {
		return err
	}
	apply := func(id string, dec merge.Decision, pRow, cRow sql.Row, collide bool) error {
		op := rowOp{Kind: dec.Kind, Columns: dec.Columns}
		if dec.Action == merge.Conflict {
			// 両側が同じ ID で別の行を足した競合は、ブランチの行を残せない。残すと、取り込みの後の基準（親の行）から見て
			// ブランチが行を変えたことになり、次のマージで本番の行を黙って上書きする（マージでも 'parent' だけで解決する）。
			bothAdded := collide || (dec.Branch == merge.Insert && dec.Parent == merge.Insert)
			if s.mode == "" || (s.mode == "branch" && bothAdded) {
				note := ""
				if s.mode == "branch" {
					note = " (only 'parent' can resolve this: different rows have the same ID)"
				}
				s.conflicts = append(s.conflicts, t.name+" "+id+note)
				return nil
			}
			// マージの向きの言い方で、出どころ（親）を "branch"、書く先（ブランチ）を "parent" として操作を求める。
			r := rowDiff{Decision: dec, BranchRow: pRow, ParentRow: cRow, Resolution: "branch"}
			if s.mode == "branch" {
				r.Resolution = "parent"
			}
			op, _ = r.effect()
			t.resolved++
			if collide && s.mode == "parent" {
				t.warnings = append(t.warnings, id+": the branch's row with the same ID was replaced by the parent's")
			}
		} else if dec.Action != merge.Apply {
			return nil
		}
		// 親の記事、ターム、term_taxonomy をブランチに入れなかった（競合でブランチの側を採った）なら、ぶら下がる行も写さない。
		if op.Kind == merge.None && pRow != nil && cRow == nil {
			if ids, ok := s.notCopied[kind]; ok {
				ids[fmt.Sprint(toMergeRow(pNow, pRow)[strings.ToLower(pNow.Columns[pNow.PKOrdinals[0]].Name)])] = true
			}
		}
		var before, after sql.Row
		switch op.Kind {
		case merge.None:
			return nil
		case merge.Insert:
			if after, err = conv.convert(s.ctx, pRow); err == nil {
				err = ed.Insert(s.ctx, after)
			}
			t.ins++
		case merge.Delete:
			before = cRow
			err = ed.Delete(s.ctx, before)
			t.del++
		case merge.Update:
			before = cRow
			var src sql.Row
			if src, err = conv.convert(s.ctx, pRow); err == nil {
				after = copyColumns(cNow, cNow, src, cRow, func(c string) bool { return slices.Contains(op.Columns, c) })
				err = ed.Update(s.ctx, before, after)
			}
			t.upd++
		}
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		s.m.applied++
		s.m.wrote[cNow.Name] = true
		s.m.track(kind, cNow, op, before, after)
		return nil
	}
	if idCols := d.policy.IdentityColumns(t.name); idCols != nil {
		return s.syncByColumn(t, idCols[0], cols, pNow, cNow, baseDef, apply)
	}

	pWin, ok, err := d.st.GetTable(d.parent.ID, t.name, s.window)
	if err != nil {
		return err
	}
	if !ok {
		pWin = nil
	}
	pks, err := d.changedPKs(&tableDiff{}, d.parent.ID, pNow, pWin, s.window)
	if err != nil {
		return err
	}
	sides := []rowSide{{s.base.branch, baseDef, s.base.ts}, {d.branch.ID, cNow, d.snap}, {d.parent.ID, pNow, d.snap}}
	// 基準がマージの時点のブランチなら、親の窓の始まりとマージの時点の行も読む（ID の衝突を見分ける）。
	collisionCheck := s.merged && !d.bases.SyncLatest()
	if collisionCheck {
		pMerge, ok, err := d.st.GetTable(d.parent.ID, t.name, d.bases.Merge.ParentTS)
		if err != nil {
			return err
		}
		if !ok {
			pMerge = nil
		}
		sides = append(sides, rowSide{d.parent.ID, pWin, s.window}, rowSide{d.parent.ID, pMerge, d.bases.Merge.ParentTS})
	}
	srcs, closeSrcs, err := d.rowSources(len(pks), sides)
	if err != nil {
		return err
	}
	defer closeSrcs()
	var added map[string]string // ブランチが基準の後に足した行の自然なキー → ID（記事とターム）
	for _, pk := range pks {
		rows := make([]sql.Row, len(srcs))
		for i, src := range srcs {
			if rows[i], err = src(pk); err != nil {
				return err
			}
		}
		baseRow, cRow, pRow := rows[0], rows[1], rows[2]
		mbP, mP := d.sideRows(baseDef, pNow, baseRow, pRow)
		mbC, mC := d.sideRows(baseDef, cNow, baseRow, cRow)
		// 基準のブランチ（マージの時点）にある行を、親がマージの後に足した（マージの時点の親になく、窓の始まりにもない）なら、
		// 両側で同じ ID の別の行を足した。両側の追加として比べる（黙って置き換えない）。マージの時点の親にあれば、前のマージで
		// ブランチから入った行として普通に比べる（親の値が違えば親が変えたとみなす）。
		collide := collisionCheck && baseRow != nil && pRow != nil && rows[3] == nil && rows[4] == nil
		if collide {
			mbP, mbC = nil, nil
		}
		dec := merge.Decide(mbP, mP, mbC, mC, cols)
		dec = merge.ResolveLatest(dec, mP, mC, d.policy.LatestColumns(t.name))
		if dec.Branch == merge.None || dec.Action == merge.NoOp {
			continue
		}
		id := pkString(pNow, baseDef, pRow, baseRow)
		cur := mP
		if cur == nil {
			cur = mbP
		}
		if s.dependsOnNotCopied(kind, cur) {
			t.skip++
			continue
		}
		if dec.Action == merge.Apply && dec.Kind == merge.Insert && (kind == merge.KindPosts || kind == merge.KindTerms) {
			if k := d.policy.NaturalKey(t.name, cur); k != "" {
				if added == nil {
					if added, err = s.branchAdded(t.name, cNow, baseDef); err != nil {
						return err
					}
				}
				if other, ok := added[k]; ok {
					s.notCopied[kind][fmt.Sprint(cur[strings.ToLower(pNow.Columns[pNow.PKOrdinals[0]].Name)])] = true
					t.skip++
					t.warnings = append(t.warnings, fmt.Sprintf("%s: %s was also added on the branch as %s; not synced", id, k, other))
					continue
				}
			}
		}
		if err := apply(id, dec, pRow, cRow, collide); err != nil {
			return err
		}
	}
	return nil
}

// dependsOnNotCopied は、親の行 cur が、写さなかった記事などにぶら下がる（ので写さない）かどうか。
func (s *syncer) dependsOnNotCopied(kind merge.Kind, cur merge.Row) bool {
	for _, dep := range dependsOn[kind] {
		if s.notCopied[dep.kind][fmt.Sprint(cur[dep.col])] {
			return true
		}
	}
	return false
}

// branchAdded は、ブランチが基準の後に足した行（今あって基準にない）の、自然なキーから ID への対応を返す。候補は、
// ブランチが自分の層で前のマージ（なければ分岐）の後に書いた行。
func (s *syncer) branchAdded(name string, cNow, baseDef *schema.Table) (map[string]string, error) {
	d := s.d
	out := map[string]string{}
	pks, err := d.st.ChangedRows(d.branch.ID, cNow.ID, d.base.BranchTS, d.snap)
	if err != nil {
		return nil, err
	}
	for _, pk := range pks {
		row, err := d.getRow(d.branch.ID, cNow, d.snap, pk)
		if err != nil || row == nil {
			if err != nil {
				return nil, err
			}
			continue
		}
		base, err := d.getRow(s.base.branch, baseDef, s.base.ts, pk)
		if err != nil {
			return nil, err
		}
		if base != nil {
			continue
		}
		if k := d.policy.NaturalKey(name, toMergeRow(cNow, row)); k != "" {
			out[k] = pkString(cNow, nil, row, nil)
		}
	}
	return out, nil
}

// syncByColumn は、列 idCol の値で行を対応づけて写す（wp_options の option_name）。行数が少ない表なので、全行を読む。
func (s *syncer) syncByColumn(t *syncTable, idCol string, cols []string, pNow, cNow, baseDef *schema.Table,
	apply func(id string, dec merge.Decision, pRow, cRow sql.Row, collide bool) error) error {
	d := s.d
	var raw [3]map[string]sql.Row
	for i, side := range []rowSide{{s.base.branch, baseDef, s.base.ts}, {d.branch.ID, cNow, d.snap}, {d.parent.ID, pNow, d.snap}} {
		raw[i] = map[string]sql.Row{}
		err := d.scanRows(side.branch, side.def, side.ts, func(_ []byte, row sql.Row) error {
			raw[i][fmt.Sprint(toMergeRow(side.def, row)[idCol])] = row
			return nil
		})
		if err != nil {
			return err
		}
	}
	keys := map[string]bool{}
	for _, m := range []map[string]sql.Row{raw[0], raw[2]} {
		for k := range m {
			keys[k] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	// 主キー（option_id）は両側で違ってよいので比べない。
	pk := pkNames(pNow)
	cmp := slices.DeleteFunc(slices.Clone(cols), func(c string) bool { return slices.Contains(pk, c) })
	for _, k := range sorted {
		mbP, mP := d.sideRows(baseDef, pNow, raw[0][k], raw[2][k])
		mbC, mC := d.sideRows(baseDef, cNow, raw[0][k], raw[1][k])
		dec := merge.Decide(mbP, mP, mbC, mC, cmp)
		if dec.Branch == merge.None || dec.Action == merge.NoOp {
			continue
		}
		if skip, _ := d.policy.SkipOption(k); skip {
			t.skip++
			continue
		}
		if err := apply(idCol+"="+k, dec, raw[2][k], raw[1][k], false); err != nil {
			return err
		}
	}
	return nil
}

// summarizeWarnings は、警告を 3 件まで並べる。
func summarizeWarnings(ws []string) string {
	if len(ws) <= 3 {
		return strings.Join(ws, "; ")
	}
	return strings.Join(ws[:3], "; ") + fmt.Sprintf("; and %d more", len(ws)-3)
}
