package backend

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/planbuilder"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 列の変更のマージ（docs/plan.md の段階 13）。
//
// 列は名前（大文字と小文字を区別しない）で対応づけ、定義を 1 つの値にして、インデックスと同じ三方向の判定にかける。
// ブランチだけが足した、変えた、消した列を親に反映する。親のテーブルは、マージの後の定義（merged）で新しいテーブル ID に
// 書き直す。行の比較も merged の列と型にそろえて行う（型を変えた列で、どの行も変わったと見なさないように）。

// columnChange は、1 つの列の判定。rowDiff の ID は "COLUMN <名前>"。
type columnChange struct {
	rowDiff
	// BranchCol と ParentCol は、今のブランチと親の列の定義（なければ nil）。
	BranchCol, ParentCol *schema.Column
}

func (c *columnChange) effect() (merge.Change, bool) {
	return definitionEffect(&c.rowDiff, c.BranchCol != nil, c.ParentCol != nil)
}

func (c *columnChange) action() string {
	if c.Decision.Action != merge.Apply {
		return c.Decision.Action.String()
	}
	return map[merge.Change]string{merge.Insert: "add column", merge.Delete: "drop column", merge.Update: "modify column"}[c.Decision.Kind]
}

// columnDefinition は、列の定義を比べるための値にする（名前と位置は含めない）。
func columnDefinition(c *schema.Column) merge.Row {
	if c == nil {
		return nil
	}
	str := func(p *string) string {
		if p == nil {
			return "<none>"
		}
		return *p
	}
	return merge.Row{indexDefColumn: fmt.Sprintf("%s null=%v default=%s onupdate=%s comment=%q extra=%q ai=%v pk=%v",
		strings.ToLower(c.Type), c.Nullable, str(c.Default), str(c.OnUpdate), c.Comment, c.Extra, c.AutoIncrement, c.PrimaryKey)}
}

func columnsByName(def *schema.Table) map[string]*schema.Column {
	out := map[string]*schema.Column{}
	if def == nil {
		return out
	}
	for i := range def.Columns {
		out[strings.ToLower(def.Columns[i].Name)] = &def.Columns[i]
	}
	return out
}

// diffColumns は、列の違いを求めて t.Columns に入れる。主キーと AUTO_INCREMENT の列の変更は、ブランチの側を採れない
// 競合にする（親の側に解決すれば、その列は親のまま）。
func (d *differ) diffColumns(t *tableDiff, baseB, nowB, baseP, nowP *schema.Table) {
	bb, nb, bp, np := columnsByName(baseB), columnsByName(nowB), columnsByName(baseP), columnsByName(nowP)
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
		dec := merge.Decide(columnDefinition(bb[n]), columnDefinition(nb[n]), columnDefinition(bp[n]), columnDefinition(np[n]), cols)
		if dec.Branch == merge.None || dec.Action == merge.NoOp {
			continue
		}
		dec.Columns, dec.Clean = nil, nil
		display := n
		if c := nb[n]; c != nil {
			display = c.Name
		} else if c := bb[n]; c != nil {
			display = c.Name
		}
		c := columnChange{rowDiff: rowDiff{ID: "COLUMN " + display, Decision: dec}, BranchCol: nb[n], ParentCol: np[n]}
		for _, col := range []*schema.Column{bb[n], nb[n], bp[n], np[n]} {
			if col != nil && (col.PrimaryKey || col.AutoIncrement) {
				c.Decision.Action, c.Decision.Kind = merge.Conflict, merge.None
				c.Decision.Reason = "primary key and AUTO_INCREMENT columns are not merged"
				c.Forbid = c.Decision.Reason
				break
			}
		}
		c.Hash = decisionHash(c.Decision, cols, columnDefinition(nb[n]), columnDefinition(np[n]))
		d.resolve(t, &c.rowDiff)
		t.Columns = append(t.Columns, c)
	}
}

// columnSummary は、要約の detail に出す列の変更（+ 追加、- 削除、~ 変更、! 解決していない競合）。
func (t *tableDiff) columnSummary() string {
	var parts []string
	for i := range t.Columns {
		c := &t.Columns[i]
		name := strings.TrimPrefix(c.ID, "COLUMN ")
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
	return "columns: " + strings.Join(parts, ", ")
}

// definitionSummary は、列とインデックスの変更の要約。
func (t *tableDiff) definitionSummary() string {
	var parts []string
	for _, s := range []string{t.columnSummary(), t.indexSummary()} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "; ")
}

// mergedDefinition は、親の今の定義 nowP に、列の変更（の解決の結果）を当てた定義を返す。列が変わらなければ nil。
// 足した列は、ブランチの定義でその前にある列のうち、親にもある最後の列の後ろに置く。消した列はインデックスからも外す。
// テーブル ID は nowP のまま（書き直すときに新しい ID にする）。
func mergedDefinition(t *tableDiff, nowB, nowP *schema.Table) *schema.Table {
	changed := false
	for i := range t.Columns {
		if k, ok := t.Columns[i].effect(); ok && k != merge.None {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	next := nowP.Clone()
	next.Version = nowP.Version
	pk := pkNames(nowP)
	for i := range t.Columns {
		c := &t.Columns[i]
		kind, _ := c.effect()
		switch kind {
		case merge.Delete:
			if j := next.ColumnIndex(c.ParentCol.Name); j >= 0 {
				removed := &sql.Column{Name: next.Columns[j].Name}
				next.Columns = append(next.Columns[:j], next.Columns[j+1:]...)
				next.Indexes = remapIndexes(next.Indexes, removed, nil, next)
			}
		case merge.Update:
			if j := next.ColumnIndex(c.ParentCol.Name); j >= 0 {
				next.Columns[j] = *c.BranchCol
			}
		}
	}
	// 追加はブランチの列の順に処理する（前の列が先に入るように）。
	for _, bc := range nowB.Columns {
		var c *columnChange
		for i := range t.Columns {
			if t.Columns[i].BranchCol != nil && strings.EqualFold(t.Columns[i].BranchCol.Name, bc.Name) {
				c = &t.Columns[i]
			}
		}
		if c == nil {
			continue
		}
		if kind, _ := c.effect(); kind != merge.Insert {
			continue
		}
		at := 0
		for _, prev := range nowB.Columns {
			if strings.EqualFold(prev.Name, bc.Name) {
				break
			}
			if j := next.ColumnIndex(prev.Name); j >= 0 {
				at = j + 1
			}
		}
		next.Columns = slices.Insert(next.Columns, at, *c.BranchCol)
	}
	next.PKOrdinals = next.PKOrdinals[:0]
	for _, name := range pk {
		next.PKOrdinals = append(next.PKOrdinals, next.ColumnIndex(name))
	}
	return next
}

// sideRows は、一方の側（ブランチか親）の基準の行 base（定義 baseDef）と今の行 now（定義 nowDef）を、比べられる
// merge.Row にする。基準の行にない列（その側が後から足した列）は、その側の今の定義の既定値で埋め、式の既定値
// （CURRENT_TIMESTAMP など）は今の行の値で埋める（列を足したときに入った値は、その側の変更とは見なさない）。列の変更が
// あれば、マージの後の定義 d.target の列と型にそろえる。
func (d *differ) sideRows(baseDef, nowDef *schema.Table, base, now sql.Row) (merge.Row, merge.Row) {
	b, n := d.withDefaults(baseDef, nowDef, base), toMergeRow(nowDef, now)
	fillUnknownBase(b, n)
	if d.target != nil {
		b, n = d.conformRow(b, baseDef, nowDef, d.target), d.conformRow(n, nowDef, nowDef, d.target)
	}
	return b, n
}

// conformRow は、行 m（列の型は定義 def の型、def にない列は定義 now の型）を、マージの後の定義 target の列と型に
// そろえる。target にない列は落とし、m にない列は target の既定値にし、型の違う列は target の型に変換する（変換
// できなければ元の値のまま）。
func (d *differ) conformRow(m merge.Row, def, now, target *schema.Table) merge.Row {
	if m == nil {
		return nil
	}
	sch, err := d.schema(target)
	if err != nil {
		return m
	}
	defaults := d.defaultsOf(target, sch)
	out := make(merge.Row, len(target.Columns))
	for i, c := range target.Columns {
		name := strings.ToLower(c.Name)
		v, ok := m[name]
		if !ok {
			if defaults[i].ok {
				out[name] = defaults[i].v
			}
			continue
		}
		from := def
		if def == nil || def.ColumnIndex(c.Name) < 0 {
			from = now
		}
		if j := from.ColumnIndex(c.Name); j >= 0 && from.Columns[j].Type != c.Type && v != nil {
			if fs, err := d.schema(from); err == nil {
				if cv, inRange, err := convertValue(d.conversionContext(), fs.Schema[j].Type, sch.Schema[i].Type, v); err == nil && inRange == sql.InRange {
					v = cv
				}
			}
		}
		out[name] = v
	}
	return out
}

// convertValue は、型 from の値 v を型 to に変換する。ENUM と SET は内部では番号で持っているので、文字列にしてから
// 変換する（番号のまま変換すると、値の並びを変えたときに別の値になる）。
func convertValue(ctx *sql.Context, from, to sql.Type, v any) (any, sql.ConvertInRange, error) {
	switch ft := from.(type) {
	case sql.EnumType:
		if n, _, err := types.Int64.Convert(ctx, v); err == nil {
			if s, ok := ft.At(int(n.(int64))); ok {
				v = s
			}
		}
	case sql.SetType:
		if n, _, err := types.Uint64.Convert(ctx, v); err == nil {
			if s, err := ft.BitsToString(n.(uint64)); err == nil {
				v = s
			}
		}
	}
	return to.Convert(ctx, v)
}

// rowConverter は、親の今の定義 from の行を、マージの後の定義 to の行にする（親のテーブルの書き直しと、行の反映で
// 親の行を新しい形で扱うため）。
type rowConverter struct {
	from, to   *schema.Table
	fromSch    sql.PrimaryKeySchema
	toSch      sql.PrimaryKeySchema
	defaults   []any  // to の列ごとの既定値（from にない列に使う）
	hasDefault []bool // defaults を求めたか
}

func newRowConverter(ctx *sql.Context, from, to *schema.Table) (*rowConverter, error) {
	fs, err := from.Schema("")
	if err != nil {
		return nil, err
	}
	ts, err := to.Schema("")
	if err != nil {
		return nil, err
	}
	rc := &rowConverter{from: from, to: to, fromSch: fs, toSch: ts, defaults: make([]any, len(to.Columns)), hasDefault: make([]bool, len(to.Columns))}
	for i, c := range to.Columns {
		if from.ColumnIndex(c.Name) >= 0 {
			continue
		}
		v, err := evalColumnDefault(ctx, ts.Schema[i], c.Default)
		if err != nil {
			return nil, fmt.Errorf("default of column %s: %w", c.Name, err)
		}
		rc.defaults[i], rc.hasDefault[i] = v, true
	}
	return rc, nil
}

// convert は 1 行を変換する。NOT NULL の列の NULL は型の既定値（” や 0）にする（WordPress の非 STRICT の sql_mode で
// ALTER したときと同じ）。新しい型に収まらない値はエラー（黙って切り詰めない）。
func (rc *rowConverter) convert(ctx *sql.Context, row sql.Row) (sql.Row, error) {
	if row == nil {
		return nil, nil
	}
	out := make(sql.Row, len(rc.to.Columns))
	for i, c := range rc.to.Columns {
		col := rc.toSch.Schema[i]
		j := rc.from.ColumnIndex(c.Name)
		var v any
		if j < 0 {
			v = rc.defaults[i]
		} else {
			v = row[j]
			if v != nil && rc.from.Columns[j].Type != c.Type {
				cv, inRange, err := convertValue(ctx, rc.fromSch.Schema[j].Type, col.Type, v)
				if err != nil || inRange != sql.InRange {
					if err == nil {
						err = sql.ErrValueOutOfRange.New(v, col.Type)
					}
					return nil, fmt.Errorf("row %s: value %v of column %s does not fit %s: %w", formatCols(row, rc.from.PKOrdinals), v, c.Name, c.Type, err)
				}
				v = cv
			}
		}
		if v == nil && !c.Nullable {
			v = col.Type.Zero()
		}
		out[i] = v
	}
	return out, nil
}

// evalColumnDefault は列の既定値を求める（式の既定値、例えば CURRENT_TIMESTAMP も評価する）。既定値がなければ NULL。
func evalColumnDefault(ctx *sql.Context, col *sql.Column, def *string) (any, error) {
	if v, ok := literalDefault(col, def); ok {
		return v, nil
	}
	// planbuilder.StringToColumnDefaultValue は式を文字列に戻すところで壊れる（v0.20.0）ので、SELECT として評価する。
	// 関数（CURRENT_TIMESTAMP など）を引くためにカタログが要る（nil だと panic する）。
	s, ok := ctx.Session.(*Session)
	if !ok || s.pro == nil {
		return nil, fmt.Errorf("unsupported default expression %q", *def)
	}
	node, _, err := planbuilder.Parse(ctx, analyzer.NewCatalog(s.pro), "SELECT "+*def)
	if err != nil {
		return nil, err
	}
	proj, ok := node.(*plan.Project)
	if !ok || len(proj.Projections) != 1 {
		return nil, fmt.Errorf("unsupported default expression %q", *def)
	}
	v, err := proj.Projections[0].Eval(ctx, nil)
	if err != nil {
		return nil, err
	}
	cv, _, err := col.Type.Convert(ctx, v)
	return cv, err
}

// rewriteParentTable は、親のテーブル t.ParentDef をマージの後の定義 t.Merged で新しいテーブル ID newID に書き直す
// （マージのトランザクション tx の中）。親の今の行を変換して書き、すべてのインデックスのエントリを作る。AUTO_INCREMENT の
// カウンタは引き継ぐ。書いた後の定義を t.ParentDef にし、親の行を新しい形にする変換を t.parentConv に置く。
func rewriteParentTable(ctx *sql.Context, tx *store.Txn, db *Database, t *tableDiff, newID uint64) error {
	old := t.ParentDef
	next := t.Merged.Clone()
	next.ID, next.Version = newID, old.Version
	rc, err := newRowConverter(ctx, old, next)
	if err != nil {
		return err
	}
	view, err := newTable(ctx, db, next)
	if err != nil {
		return err
	}
	oldView, err := newTable(ctx, db, old)
	if err != nil {
		return err
	}
	keyIndexes := view.keyIndexes()
	write := func(row sql.Row) error {
		// 行 ID で並べる表は、列が変わるのでハッシュを求め直す（docs/plan.md の段階 72）。
		row, err := view.completeRow(ctx, row, nil)
		if err != nil {
			return err
		}
		pk, err := view.encodePK(row)
		if err != nil {
			return err
		}
		value, err := rowenc.EncodeRow(next.Version, row)
		if err != nil {
			return err
		}
		if err := tx.InsertRow(db.branch, next.ID, pk, value); err != nil {
			return err
		}
		for _, ki := range keyIndexes {
			entry, value, err := view.entry(ki, row)
			if err != nil {
				return err
			}
			if err := tx.PutIndexEntry(db.branch, next.ID, ki.def.ID, entry, value); err != nil {
				return err
			}
		}
		return nil
	}
	// 一意インデックスがあれば、全行を並べて確かめてから書く（WordPress では wp_options のような小さいテーブル）。
	// なければ、読みながら書いて行を持たない（docs/plan.md の段階 59。310 万行の列を足したマージで全行を持っていた）。
	unique := false
	for _, idx := range next.Indexes {
		unique = unique || idx.Unique
	}
	var rows []sql.Row
	n := 0
	if err := tx.ScanRowsLatest(db.branch, old.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
		row, err := oldView.decodeRow(r.Value)
		if err != nil {
			return false, err
		}
		conv, err := rc.convert(ctx, row)
		if err != nil {
			return false, err
		}
		n++
		if unique {
			rows = append(rows, conv)
		} else if err := write(conv); err != nil {
			return false, err
		}
		return true, interrupted(ctx, n)
	}); err != nil {
		return err
	}
	for _, idx := range next.Indexes {
		if idx.Unique {
			if err := view.checkUniqueRows(ctx, idx, rows); err != nil {
				return fmt.Errorf("unique index %s: %w", idx.Name, err)
			}
		}
	}
	for i, row := range rows {
		if err := interrupted(ctx, i+1); err != nil {
			return err
		}
		if err := write(row); err != nil {
			return err
		}
	}
	if err := carryAutoIncrement(ctx, db, &restoreTable{Now: old, At: next}, view); err != nil {
		return err
	}
	if err := tx.PutTable(db.branch, next); err != nil {
		return err
	}
	t.ParentDef = next
	t.RewriteRows = n
	t.parentConv = func(row sql.Row) (sql.Row, error) { return rc.convert(ctx, row) }
	return nil
}
