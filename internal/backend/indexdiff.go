package backend

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// インデックスの違いのマージ（docs/plan.md の段階 12）。
//
// インデックスは名前（大文字と小文字を区別しない）で対応づけ、定義（列、プレフィックス長、一意、FULLTEXT、コメント）を
// 1 つの値にして、行と同じ三方向の判定（merge.Decide）にかける。ブランチだけが足した、消した、変えたインデックスを
// 親に反映し、両側の食い違いは競合にする。

// indexChange は、1 つのインデックスの判定。rowDiff の ID は "INDEX <名前>"、BranchRow と ParentRow は使わない。
type indexChange struct {
	rowDiff
	// BranchIndex と ParentIndex は、今のブランチと親のインデックスの定義（なければ nil）。
	BranchIndex, ParentIndex *schema.Index
}

// indexDefColumn は、インデックスの定義を merge.Decide にかけるときの列名。
const indexDefColumn = "def"

// effect は、マージで親に行う操作（Insert は追加、Delete は削除、Update は作り直し）を返す。解決していない競合なら
// ok が false。
func (c *indexChange) effect() (kind merge.Change, ok bool) {
	return definitionEffect(&c.rowDiff, c.BranchIndex != nil, c.ParentIndex != nil)
}

// definitionEffect は、定義（インデックスや列）の判定 r について、マージで親に行う操作を返す。inBranch と inParent は、
// 今のブランチと親にその定義があるかどうか。解決していない競合なら ok が false。
func definitionEffect(r *rowDiff, inBranch, inParent bool) (kind merge.Change, ok bool) {
	dec := r.Decision
	switch dec.Action {
	case merge.Apply:
		return dec.Kind, true
	case merge.Conflict:
		switch r.Resolution {
		case "parent":
			return merge.None, true
		case "branch":
			switch {
			case !inBranch && !inParent:
				return merge.None, true
			case !inBranch:
				return merge.Delete, true
			case !inParent:
				return merge.Insert, true
			default:
				return merge.Update, true
			}
		}
		return merge.None, false
	}
	return merge.None, true
}

// action は、行ごとの表示の action 列の値。
func (c *indexChange) action() string {
	if c.Decision.Action != merge.Apply {
		return c.Decision.Action.String()
	}
	return map[merge.Change]string{merge.Insert: "add index", merge.Delete: "drop index", merge.Update: "recreate index"}[c.Decision.Kind]
}

// indexDefinition は、インデックスの定義を比べるための文字列にする（名前と ID は含めない）。
func indexDefinition(idx *schema.Index) merge.Row {
	if idx == nil {
		return nil
	}
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = strings.ToLower(c.Name)
		if c.Length > 0 {
			cols[i] += fmt.Sprintf("(%d)", c.Length)
		}
	}
	kind := "key"
	switch {
	case idx.Unique:
		kind = "unique"
	case idx.Fulltext:
		kind = "fulltext"
	}
	return merge.Row{indexDefColumn: fmt.Sprintf("%s(%s) comment=%q", kind, strings.Join(cols, ","), idx.Comment)}
}

// indexesByName は、テーブルのインデックスを小文字の名前で引けるようにする。def が nil なら空。
func indexesByName(def *schema.Table) map[string]*schema.Index {
	out := map[string]*schema.Index{}
	if def == nil {
		return out
	}
	for i := range def.Indexes {
		out[strings.ToLower(def.Indexes[i].Name)] = &def.Indexes[i]
	}
	return out
}

// diffIndexes は、列が同じテーブルのインデックスの違いを求めて t.Indexes に入れる。候補はブランチの基準か今にある名前
// （親だけの変更は反映しないので比べなくてよい）。
func (d *differ) diffIndexes(t *tableDiff, baseB, nowB, baseP, nowP *schema.Table) {
	bb, nb, bp, np := indexesByName(baseB), indexesByName(nowB), indexesByName(baseP), indexesByName(nowP)
	names := map[string]bool{}
	for n := range bb {
		names[n] = true
	}
	for n := range nb {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	cols := []string{indexDefColumn}
	for _, n := range sorted {
		dec := merge.Decide(indexDefinition(bb[n]), indexDefinition(nb[n]), indexDefinition(bp[n]), indexDefinition(np[n]), cols)
		if dec.Branch == merge.None || dec.Action == merge.NoOp {
			continue
		}
		dec.Columns, dec.Clean = nil, nil
		display := n
		if idx := nb[n]; idx != nil {
			display = idx.Name
		} else if idx := bb[n]; idx != nil {
			display = idx.Name
		}
		c := indexChange{rowDiff: rowDiff{ID: "INDEX " + display, Decision: dec}, BranchIndex: nb[n], ParentIndex: np[n]}
		c.Hash = decisionHash(dec, cols, indexDefinition(nb[n]), indexDefinition(np[n]))
		d.resolve(t, &c.rowDiff)
		t.Indexes = append(t.Indexes, c)
	}
}

// indexSummary は、要約の detail に出すインデックスの変更（+ 追加、- 削除、~ 作り直し、! 解決していない競合）。
func (t *tableDiff) indexSummary() string {
	var parts []string
	for i := range t.Indexes {
		c := &t.Indexes[i]
		name := strings.TrimPrefix(c.ID, "INDEX ")
		kind, ok := c.effect()
		switch {
		case !ok:
			parts = append(parts, "!"+name)
		case kind == merge.Insert:
			parts = append(parts, "+"+name)
		case kind == merge.Delete:
			parts = append(parts, "-"+name)
		case kind == merge.Update:
			parts = append(parts, "~"+name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "indexes: " + strings.Join(parts, ", ")
}

// applyIndexChanges は、テーブル t のインデックスの変更をマージのトランザクション tx に書く（行の反映の前）。親の書き込み
// ロックは tx が持っている。追加するインデックスのエントリは親の今の行から作る。一意インデックスで親の行が重複して
// いればエラー（マージ全体を取り消す。マージする行との重複は、行を書くときにエディタが確かめる）。書いた後の定義を
// t.ParentDef にする（行の反映はこの定義で書くので、新しいインデックスのエントリもエディタが書く）。
func applyIndexChanges(ctx *sql.Context, tx *store.Txn, db *Database, t *tableDiff) error {
	next := t.ParentDef.Clone()
	next.Version = t.ParentDef.Version
	var added []string
	for i := range t.Indexes {
		c := &t.Indexes[i]
		kind, _ := c.effect()
		if (kind == merge.Delete || kind == merge.Update) && c.ParentIndex != nil {
			if j := next.FindIndex(c.ParentIndex.Name); j >= 0 {
				next.Indexes = append(next.Indexes[:j], next.Indexes[j+1:]...)
			}
		}
		if (kind == merge.Insert || kind == merge.Update) && c.BranchIndex != nil {
			if err := next.AddIndex(indexDefOf(c.BranchIndex)); err != nil {
				return fmt.Errorf("index %s: %w", c.BranchIndex.Name, err)
			}
			added = append(added, c.BranchIndex.Name)
		}
	}
	view, err := newTable(ctx, db, next)
	if err != nil {
		return err
	}
	var rows []sql.Row
	if err := tx.ScanRowsLatest(db.branch, next.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
		row, err := view.decodeRow(r.Value)
		rows = append(rows, row)
		return err == nil, err
	}); err != nil {
		return err
	}
	for _, name := range added {
		idx := next.Indexes[next.FindIndex(name)]
		if idx.Unique {
			if err := view.checkUniqueRows(ctx, idx, rows); err != nil {
				return fmt.Errorf("unique index %s: %w", idx.Name, err)
			}
		}
		ki, ok := view.keyIndex(idx)
		if !ok {
			continue
		}
		for _, row := range rows {
			entry, value, err := view.entry(ki, row)
			if err != nil {
				return err
			}
			if err := tx.PutIndexEntry(db.branch, next.ID, ki.def.ID, entry, value); err != nil {
				return err
			}
		}
	}
	if err := tx.PutTable(db.branch, next); err != nil {
		return err
	}
	t.ParentDef = next
	return nil
}

// indexDefOf は、保存しているインデックスの定義を go-mysql-server の定義にする。
func indexDefOf(idx *schema.Index) sql.IndexDef {
	def := sql.IndexDef{Name: idx.Name, Comment: idx.Comment, Constraint: sql.IndexConstraint_None}
	switch {
	case idx.Unique:
		def.Constraint = sql.IndexConstraint_Unique
	case idx.Fulltext:
		def.Constraint = sql.IndexConstraint_Fulltext
	}
	for _, c := range idx.Columns {
		def.Columns = append(def.Columns, sql.IndexColumn{Name: c.Name, Length: c.Length})
	}
	return def
}
