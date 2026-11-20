package backend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// インデックスのキーの水準を上げて作り直す手続き（docs/plan.md の「キー設計」と段階 117）。
//
//	CALL heimdb_rebuild_indexes();  -- 接続中のデータベース（ブランチ）の、水準の古いインデックスを作り直す
//
// 段階 117 より前に作った DECIMAL、FLOAT、DOUBLE、ENUM の列を含むインデックスは、定義のキーの水準が 0 で、段階 120 より
// 前に作った SET と BIT の列を含むインデックスは水準が 0 か 1 で、エントリを持たずに全件を読む。手続きは、そういうインデックスを、CREATE INDEX と同じ形（書き込みのロック、buildIndex で今の行から
// エントリを作り、最後のコミットで今の水準の定義を書く）で作り直す。前の時点（AS OF）と、作り直しより前に分かれた
// ブランチは、その時点の定義（水準 0）で読むので、エントリのない時点を読まない。作り直すものがなければ何もしない
// （2 度呼んでもよい）。

var rebuildIndexesSchema = sql.Schema{
	{Name: "table", Type: types.LongText},
	{Name: "index", Type: types.LongText},
}

func (p *Provider) rebuildIndexesProcedures() []sql.ExternalStoredProcedureDetails {
	return []sql.ExternalStoredProcedureDetails{
		{Name: "heimdb_rebuild_indexes", Schema: rebuildIndexesSchema, Function: func(ctx *sql.Context) (sql.RowIter, error) {
			return p.rebuildKeyLevels(ctx)
		}},
	}
}

// rebuildKeyLevels は、接続中のデータベースのブランチの表のうち、水準の古いインデックスを作り直し、作り直したものを
// 表と名前の行で返す。
func (p *Provider) rebuildKeyLevels(ctx *sql.Context) (sql.RowIter, error) {
	if p.st.IsReplica() {
		return nil, store.ErrReadOnlyReplica
	}
	db, ok := p.database(ctx.GetCurrentDatabase())
	if !ok {
		return nil, fmt.Errorf("HeimDB: heimdb_rebuild_indexes() works on the connection's database; USE %s or a branch's database first", p.base)
	}
	defs, err := p.st.ListTables(db.branch, store.Latest)
	if err != nil {
		return nil, err
	}
	var rows []sql.Row
	for _, def := range defs {
		stale, err := staleIndexes(def)
		if err != nil {
			return nil, err
		}
		if len(stale) == 0 {
			continue
		}
		t, err := newTable(ctx, db, def)
		if err != nil {
			return nil, err
		}
		// エラーは包まない（一意インデックスの重複のエラーは、包むと 1062 と表とインデックスの名前の文にならない）。前の表で
		// 作り直したものはそのまま残り、直してから呼び直せば続きを作る
		names, err := t.raiseKeyLevels(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			rows = append(rows, sql.Row{def.Name, n})
		}
	}
	return &rowIter{rows: rows}, nil
}

// staleIndexes は、定義 def のインデックスのうち、水準が今より古く、今の水準ならエントリを持つのに今は持たないものの
// 位置を返す（FULLTEXT は持たない）。今の水準でも古い水準でもエントリを持つもの（整数や文字列だけの列）は、作り直さ
// なくても同じなので含めない。
func staleIndexes(def *schema.Table) ([]int, error) {
	var out []int
	for i, idx := range def.Indexes {
		if idx.Fulltext || idx.KeyLevel >= rowenc.KeyLevel {
			continue
		}
		keyedNow, keyedBefore := true, true
		for _, c := range idx.Columns {
			ord := def.ColumnIndex(c.Name)
			if ord < 0 {
				return nil, fmt.Errorf("HeimDB: table %s: index %s has no column %s", def.Name, idx.Name, c.Name)
			}
			typ, err := def.Columns[ord].SQLType()
			if err != nil {
				return nil, err
			}
			keyedNow = keyedNow && rowenc.KeyableType(typ, rowenc.KeyLevel)
			keyedBefore = keyedBefore && rowenc.KeyableType(typ, idx.KeyLevel)
		}
		if keyedNow && !keyedBefore {
			out = append(out, i)
		}
	}
	return out, nil
}

// raiseKeyLevels は、表の水準の古いインデックス（staleIndexes）を今の水準にして、今の行からエントリを作り、定義を
// 書く（CreateIndex と同じ順）。作り直したインデックスの名前を返す。一意インデックスで行が重複していれば重複の
// エラー（1062）にし、作りかけのエントリを消す。
//
// only が nil でなければ、その ID のインデックスだけを作り直す（起動の後の裏の作り直しが 1 つずつ作る。autorebuild.go。
// docs/plan.md の段階 121）。ロックを取ってから読んだ今の定義で、まだ古いものだけを作る（手続きと裏の処理が並んでも、
// 後の方は何もしない）。
func (t *Table) raiseKeyLevels(ctx *sql.Context, only []uint64) ([]string, error) {
	if err := checkBranchSize(t.db.st, t.db.branch, t.def.Name); err != nil {
		return nil, err
	}
	unlock, err := t.db.lockDDL(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cur, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
	if err != nil {
		return nil, err
	}
	if !ok || cur.ID != t.def.ID {
		return nil, sql.ErrTableNotFound.New(t.def.Name)
	}
	stale, err := staleIndexes(cur)
	if err != nil {
		return nil, err
	}
	if only != nil {
		stale = slices.DeleteFunc(stale, func(i int) bool { return !slices.Contains(only, cur.Indexes[i].ID) })
	}
	if len(stale) == 0 {
		return nil, nil
	}
	next := cur.Clone()
	for _, i := range stale {
		next.Indexes[i].KeyLevel = rowenc.KeyLevel
	}
	view, err := newTable(ctx, t.db, next)
	if err != nil {
		return nil, err
	}
	type build struct {
		ki      keyIndex
		started bool
	}
	var builds []build
	cleanup := func() {
		for _, b := range builds {
			if b.started {
				view.dropIndexBuild(b.ki)
			}
		}
	}
	for _, i := range stale {
		ki, keyed := view.keyIndex(next.Indexes[i])
		if !keyed {
			continue
		}
		started, err := view.buildIndex(ctx, ki)
		builds = append(builds, build{ki, started})
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	err = t.db.st.Update(func(tx *store.WriteTx) error {
		latest, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
		if err != nil {
			return err
		}
		if !ok || latest.ID != cur.ID || latest.Version != cur.Version {
			return sql.ErrTableNotFound.New(t.def.Name)
		}
		for _, b := range builds {
			if b.started {
				if err := tx.UnmarkIndexBuild(t.db.branch, t.def.ID, b.ki.def.ID); err != nil {
					return err
				}
			} else if err := tx.DropIndexData(t.db.branch, t.def.ID, b.ki.def.ID); err != nil {
				// 行がなくエントリを書かなかった。前に途中で終わった作成のエントリが残っていれば消す（CreateIndex と同じ）。
				return err
			}
		}
		next.Version = tx.TS
		return tx.PutTable(t.db.branch, next)
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	names := make([]string, len(builds))
	for i, b := range builds {
		names[i] = b.ki.def.Name
	}
	return names, nil
}

// logStaleIndexes は、どのブランチかの今の定義に水準の古いインデックス（staleIndexes）があれば、データベースごとに
// ログで知らせる（起動時。docs/plan.md の段階 117）。background は、本番が起動の後に main のものを裏で作り直すか
// （StartIndexRebuild。段階 121）。ブランチのものは利用者が明示する（heimdb_rebuild_indexes）。レプリカでは、本番が
// 作り直したエントリが記録で届く旨を書く。
func (p *Provider) logStaleIndexes(background bool) {
	total := 0
	mainStale, otherStale := false, false
	var parts []string
	for _, b := range p.st.Branches() {
		defs, err := p.st.ListTables(b.ID, store.Latest)
		if err != nil {
			logrus.Warnf("HeimDB: check the index key levels of branch %s: %v", b.Name, err)
			continue
		}
		var names []string
		for _, def := range defs {
			stale, err := staleIndexes(def)
			if err != nil {
				logrus.Warnf("HeimDB: check the index key levels of %s: %v", def.Name, err)
				continue
			}
			for _, i := range stale {
				names = append(names, def.Name+"."+def.Indexes[i].Name)
			}
		}
		if len(names) == 0 {
			continue
		}
		if b.ID == store.MainBranchID {
			mainStale = true
		} else {
			otherStale = true
		}
		total += len(names)
		shown := names
		if len(shown) > 10 {
			shown = append(shown[:10:10], fmt.Sprintf("and %d more", len(names)-10))
		}
		parts = append(parts, fmt.Sprintf("%s: %s", p.dbName(b), strings.Join(shown, ", ")))
	}
	if total == 0 {
		return
	}
	const manual = "run CALL heimdb_rebuild_indexes() on the primary while connected to each of these databases"
	todo := "to build them, " + manual
	switch {
	case mainStale && p.st.IsReplica():
		todo = "the primary rebuilds " + p.base + "'s in the background after it starts (unless -rebuild-indexes=false), and the entries reach this replica through its commit records"
	case mainStale && background:
		todo = p.base + "'s are rebuilt in the background now"
	}
	if mainStale && otherStale && (background || p.st.IsReplica()) {
		todo += "; to build the others, " + manual
	}
	logrus.Warnf("HeimDB: %d indexes on DECIMAL, FLOAT, DOUBLE, ENUM, SET or BIT columns were made by an older HeimDB (0.2.38 or earlier, or 0.2.40 or earlier for SET and BIT) and have no entries, so they read every row (%s); %s", total, strings.Join(parts, "; "), todo)
}
