package backend

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression/function"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// 列の変更（docs/plan.md の段階 5.5）。
//
// 行の形（列の数、順序、型、NULL の可否）が変わる変更は、go-mysql-server の RewritableTable の仕組みで、
// 新しい形の行を新しいテーブル ID に書き直す。テーブル定義の新しいバージョンは新しい ID を指し、古い ID のデータは
// 残るので、AS OF で変更前の定義と行が読める。列名、DEFAULT、コメントだけの変更は、テーブル定義だけを更新する。

var (
	_ sql.AlterableTable           = (*Table)(nil)
	_ sql.PrimaryKeyAlterableTable = (*Table)(nil)
	_ sql.RewritableTable          = (*Table)(nil)
	_ sql.TruncateableTable        = (*Table)(nil)
	_ sql.CollationAlterableTable  = (*Table)(nil)
)

// ShouldRewriteTable は sql.RewritableTable の実装。行の形が変わるなら書き直す。
func (t *Table) ShouldRewriteTable(_ *sql.Context, oldSchema, newSchema sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column) bool {
	return !sameRowLayout(oldSchema, newSchema, oldColumn, newColumn)
}

// sameRowLayout は、old と new で行の形（列の数、順序、型、NULL の可否、AUTO_INCREMENT、主キー）が同じかを返す。
// 列名が違ってよいのは、名前を変える列（oldColumn から newColumn）だけ。
func sameRowLayout(old, new sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column) bool {
	if len(old.Schema) != len(new.Schema) || !slices.Equal(old.PkOrdinals, new.PkOrdinals) {
		return false
	}
	for i := range old.Schema {
		oc, nc := old.Schema[i], new.Schema[i]
		if !oc.Type.Equals(nc.Type) || oc.Nullable != nc.Nullable || oc.AutoIncrement != nc.AutoIncrement {
			return false
		}
		if !strings.EqualFold(oc.Name, nc.Name) {
			renamed := oldColumn != nil && newColumn != nil &&
				strings.EqualFold(oc.Name, oldColumn.Name) && strings.EqualFold(nc.Name, newColumn.Name)
			if !renamed {
				return false
			}
		}
	}
	return true
}

// AddColumn は sql.AlterableTable の実装。列を足すと行の形が変わるので、必ず書き直し（RewriteInserter）を通る。
func (t *Table) AddColumn(*sql.Context, *sql.Column, *sql.ColumnOrder) error {
	return fmt.Errorf("HeimDB: adding a column requires rewriting the table")
}

// DropColumn は sql.AlterableTable の実装。AddColumn と同じく、必ず書き直しを通る。
func (t *Table) DropColumn(*sql.Context, string) error {
	return fmt.Errorf("HeimDB: dropping a column requires rewriting the table")
}

// ModifyColumn は sql.AlterableTable の実装。行の形が変わらない変更（列名、DEFAULT、コメントなど）だけがここに来る。
func (t *Table) ModifyColumn(ctx *sql.Context, columnName string, column *sql.Column, order *sql.ColumnOrder) (err error) {
	if order != nil {
		return fmt.Errorf("HeimDB: moving a column requires rewriting the table")
	}
	col, err := schema.ColumnFromSQL(column)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			warnStoredAsUTF8MB4(ctx, t.def.Name, []*sql.Column{column}, sql.Collation_Unspecified)
		}
	}()
	return t.alter(ctx, func(_ *store.WriteTx, next *schema.Table) error {
		i := next.ColumnIndex(columnName)
		if i < 0 {
			return sql.ErrTableColumnNotFound.New(next.Name, columnName)
		}
		if col.Type != next.Columns[i].Type || col.Nullable != next.Columns[i].Nullable {
			return fmt.Errorf("HeimDB: changing the type of column %s requires rewriting the table", columnName)
		}
		col.PrimaryKey = next.Columns[i].PrimaryKey
		next.Columns[i] = col
		next.Indexes = renameIndexColumns(next.Indexes, columnName, col.Name)
		return nil
	})
}

// renameIndexColumns は、インデックスの列名 from を to に付け替える。
func renameIndexColumns(indexes []schema.Index, from, to string) []schema.Index {
	for i := range indexes {
		for j := range indexes[i].Columns {
			if strings.EqualFold(indexes[i].Columns[j].Name, from) {
				indexes[i].Columns[j].Name = to
			}
		}
	}
	return indexes
}

// CreatePrimaryKey は sql.PrimaryKeyAlterableTable の実装。エンジンは RewritableTable を実装した表を書き直しで変える
// （主キーのない表に主キーを足す。RewriteInserter）ので、ここには来ない。
func (t *Table) CreatePrimaryKey(*sql.Context, []sql.IndexColumn) error {
	return fmt.Errorf("HeimDB: adding a primary key requires rewriting the table")
}

// DropPrimaryKey は sql.PrimaryKeyAlterableTable の実装。CreatePrimaryKey と同じく、書き直しを通る。
func (t *Table) DropPrimaryKey(*sql.Context) error {
	return fmt.Errorf("HeimDB: dropping the primary key requires rewriting the table")
}

// errPrimaryKeyChange は、主キーを変える ALTER TABLE を拒否するエラー。
var errPrimaryKeyChange = fmt.Errorf("HeimDB: changing the primary key is not supported")

// RewriteInserter は sql.RewritableTable の実装。go-mysql-server は、古い形の行を新しい形に変換して、返した
// RowInserter に流し込み、最後に Close を呼ぶ。行は集めずに、新しいテーブル ID へ何回かに分けてコミットし、Close で
// テーブル定義をその ID に向ける（docs/plan.md の段階 53。rewriter を参照）。
func (t *Table) RewriteInserter(ctx *sql.Context, oldSchema, newSchema sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column, idxCols []sql.IndexColumn) (sql.RowInserter, error) {
	if idxCols != nil {
		// go-mysql-server は一意インデックスを作った後にも、インデックスを作るための書き直しを求める
		// （sql/rowexec/ddl_iters.go の rewriteTableForIndexCreate）。HeimDB は CreateIndex で重複の確認と
		// エントリの作成を済ませているので、何もしない。ただし、主キーのない表を並べる一意インデックスが変わるときは
		// 書き直す（docs/plan.md の段階 72。空の表は CreateIndex で定義だけを変えている）。主キーの変更は対応しない。
		addPK := len(oldSchema.PkOrdinals) == 0 && len(newSchema.PkOrdinals) > 0 // 主キーのない表への ADD PRIMARY KEY
		if !addPK && (!slices.Equal(oldSchema.PkOrdinals, newSchema.PkOrdinals) || len(oldSchema.Schema) != len(newSchema.Schema)) {
			return nil, fmt.Errorf("HeimDB: changing the primary key is not supported")
		}
		if !addPK && !t.keyChanges() {
			return discardInserter{}, nil
		}
	}
	if err := t.db.ValidateSchema(newSchema.Schema); err != nil {
		return nil, err
	}
	// 主キーの列の数が変わる書き直し（複数の列の主キーから列を消すなど）は対応しない（段階 53 のレビュー。WordPress は
	// しない）。エンジンは、消した主キーの列の位置を -1 にして渡す。
	// 主キーのない表への ADD PRIMARY KEY と DROP PRIMARY KEY（主キーをすべて外す）は書き直す（docs/plan.md の段階 72）。
	keyChange := len(oldSchema.PkOrdinals) == 0 || len(newSchema.PkOrdinals) == 0
	if !keyChange && len(newSchema.PkOrdinals) != len(oldSchema.PkOrdinals) || slices.Contains(newSchema.PkOrdinals, -1) {
		return nil, errPrimaryKeyChange
	}
	// 行を読む前に、実行中のトランザクションを暗黙にコミットして書き込みロックを取る。go-mysql-server はこの後で
	// 行を読むので、ロックの後の新しいスナップショットで読み、書き直すまでの間に他の変更が入らない。
	if err := checkBranchSize(t.db.st, t.db.branch, t.def.Name); err != nil {
		return nil, err
	}
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil {
		if err := s.beginDDL(t.db.branch); err != nil {
			return nil, err
		}
	}
	if oldColumn != nil && newColumn != nil && oldColumn.Nullable && !newColumn.Nullable {
		if ord := oldSchema.Schema.IndexOfColName(oldColumn.Name); ord >= 0 {
			t.nullFill = &nullFill{
				ord:    ord,
				name:   newColumn.Name,
				zero:   newColumn.Type.Zero(),
				warn:   true,
				strict: strictMode(ctx),
			}
			if newColumn.Type.Type() == sqltypes.Timestamp {
				// MySQL は TIMESTAMP の列の NULL を、型の既定値ではなく文の時刻（NOW()）にして、警告を出さない。
				// go-mysql-server の日時はセッションのタイムゾーンでの値なので、NOW() と同じ評価で求める。
				now, err := function.NewNow()
				if err != nil {
					return nil, err
				}
				if t.nullFill.zero, err = now.Eval(ctx, nil); err != nil {
					return nil, err
				}
				t.nullFill.warn = false
			}
		}
	}
	r, err := t.startRewrite(ctx, newSchema, oldColumn, newColumn, "")
	if err != nil {
		t.nullFill = nil
		return nil, err
	}
	t.rewriting = r
	if sess, ok := ctx.Session.(*Session); ok {
		// エンジンは、失敗したときに DiscardChanges も Close も呼ばずに返すことがある（ENUM を狭めて値が入らないときなど。
		// sql/rowexec/ddl_iters.go）。文の終わり（CommandEnd）に、終わっていない書き直しを取り消す（段階 53 のレビュー）。
		sess.rewriters = append(sess.rewriters, r)
		r.sess = sess
	}
	return r, nil
}

// nullFill は、NULL を許す列を NOT NULL に変える書き直しで、既存の行の NULL を扱う（docs/plan.md の段階 15）。
// go-mysql-server は行を変換した直後に NULL を拒否する（sql/rowexec/ddl_iters.go の validateNullability）ので、
// 書き直しのために行を読むところ（Table.PartitionRows）で置き換える。MySQL と同じく、非 STRICT の sql_mode では
// 列の既定値ではなく型の既定値（” や 0）にして、行ごとに警告 1265 を出す。STRICT ではエラー 1138。
type nullFill struct {
	ord    int    // 古い行での列の位置
	name   string // 新しい列名（警告に出す）
	zero   any
	warn   bool
	strict bool
}

func (f *nullFill) apply(ctx *sql.Context, rows []sql.Row) error {
	for i, row := range rows {
		if err := f.applyRow(ctx, row, i+1); err != nil {
			return err
		}
	}
	return nil
}

// applyRow は、n 番目に読んだ行 row の NULL を置き換える。
func (f *nullFill) applyRow(ctx *sql.Context, row sql.Row, n int) error {
	if row[f.ord] != nil {
		return nil
	}
	if f.strict {
		return mysql.NewSQLError(1138, "22004", "Invalid use of NULL value")
	}
	row[f.ord] = f.zero
	if f.warn {
		ctx.Warn(1265, "Data truncated for column '%s' at row %d", f.name, n)
	}
	return nil
}

// discardInserter は、渡された行を捨てる sql.RowInserter。
type discardInserter struct{}

func (discardInserter) StatementBegin(*sql.Context)              {}
func (discardInserter) DiscardChanges(*sql.Context, error) error { return nil }
func (discardInserter) StatementComplete(*sql.Context) error     { return nil }
func (discardInserter) Insert(*sql.Context, sql.Row) error       { return nil }
func (discardInserter) Close(*sql.Context) error                 { return nil }

// rewriteChunkBytes は、書き直しで 1 回にコミットする行とインデックスのエントリの大きさの目安。
var rewriteChunkBytes = 4 << 20

// rewriter は、書き直した行を新しいテーブル ID に書く sql.RowInserter（docs/plan.md の段階 53）。
//
// 行とインデックスのエントリは、符号化して rewriteChunkBytes ごとにコミットする。新しい ID は、Close のコミットで
// テーブル定義が指すまでどこからも見えず、書きかけの印（store の MarkRewrite）で GC から守る。文が失敗したら、書いた
// データと印を消す。書き込みのロックは ALTER の間持っているので、古い表は書き直しの間変わらない。
//
// 主キーの重複は、主キーの列を変える書き直しでだけ確かめる（ほかの書き直しは主キーの値を変えない）。一意インデックスは、
// 変える列（消す列、名前を変える列を含む）を含むものだけ確かめる。どちらも、コミット済みの新しい ID のデータを引き、
// まだコミットしていない分はメモリの表で比べる。
type rewriter struct {
	t                    *Table
	oldColumn, newColumn *sql.Column
	next                 *schema.Table // 新しいテーブル定義（ID は書きかけ）
	view                 *Table        // next の Table
	keys                 []keyIndex

	pending      []rewriteKV
	pendingBytes int
	rows         int // 書いた行の数

	// 確かめ（checkPK、uniques のどれかがあるときだけ使う）
	checkPK     bool
	uniques     []schema.Index
	txn         *store.Txn // コミット済みの新しい ID のデータを読む
	pendingPKs  map[string]sql.Row
	pendingRows []sql.Row
	pendingVals map[int64]map[string][]sql.Row // インデックス ID → 値の先頭部分 → 行

	nextAutoInc uint64
	aiOrd       int

	scan      *rewriteScan // 古い行を読むイテレータ（Close で閉じる）
	unlock    func()       // 書き込みのロックを放す（lockDDL）
	sess      *Session     // 文の終わりに、終わっていなければ取り消す（Session.CommandEnd）
	finished  bool
	discarded bool
}

// rewriteKV は、まだコミットしていない行（index が 0）かインデックスのエントリ。
type rewriteKV struct {
	index      uint64
	key, value []byte
}

// startRewrite は、新しいテーブル ID を払い出して書きかけの印を付け（1 回目のコミット）、rewriter を作る。
// dropIndex は、書き直しで消すインデックスの名前（Table.rewriteWithout。なければ空）。
func (t *Table) startRewrite(ctx *sql.Context, newSchema sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column, dropIndex string) (*rewriter, error) {
	r := &rewriter{t: t, oldColumn: oldColumn, newColumn: newColumn, aiOrd: -1}
	unlock, err := t.db.lockDDL(ctx)
	if err != nil {
		return nil, err
	}
	r.unlock = unlock
	err = t.db.st.Update(func(tx *store.WriteTx) error {
		cur, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
		if err != nil {
			return err
		}
		if !ok || cur.ID != t.def.ID {
			return sql.ErrTableNotFound.New(t.def.Name)
		}
		newID, err := tx.NextTableID()
		if err != nil {
			return err
		}
		if err := tx.MarkRewrite(t.db.branch, newID); err != nil {
			return err
		}
		collation, err := cur.CollationID()
		if err != nil {
			return err
		}
		if r.next, err = schema.FromSQL(newID, cur.Name, newSchema, collation, cur.Comment); err != nil {
			return err
		}
		r.next.NextIndexID = cur.NextIndexID
		r.next.Indexes = remapIndexes(cur.Indexes, oldColumn, newColumn, r.next)
		// 書き直しは新しいテーブル ID にすべてのエントリを作るので、すべてのインデックスに今のキーの水準を付ける（段階 117。
		// 水準 0 の DECIMAL などのインデックスも、ここでエントリを持つようになる）
		for i := range r.next.Indexes {
			r.next.Indexes[i].KeyLevel = rowenc.KeyLevel
		}
		if i := r.next.FindIndex(dropIndex); dropIndex != "" && i >= 0 {
			r.next.Indexes = slices.Delete(r.next.Indexes, i, i+1)
		}
		// 主キーのない表の保存のキーを、新しい定義で決め直す（docs/plan.md の段階 72）。見えないインデックスは、見えない
		// 列が FromSQL の定義にないので remapIndexes で外れ、ここで新しい ID で足す。
		r.next.SettleKey()
		// 行の値の先頭に入れる定義のバージョン。定義は Close のコミットで書くが、バージョンはここで決める（行を書く前に要る）。定義を
		// 書く時刻とは違うが、行の値のバージョンは読むときに使わない（decodeRowColumns）。
		r.next.Version = tx.TS
		// 確かめが要る一意インデックス: 変える列を含んでいたもの。
		changed := func(name string) bool {
			return (oldColumn != nil && strings.EqualFold(name, oldColumn.Name)) ||
				(newColumn != nil && strings.EqualFold(name, newColumn.Name))
		}
		check := map[uint64]bool{}
		for _, idx := range cur.Indexes {
			for _, c := range idx.Columns {
				if idx.Unique && changed(c.Name) {
					check[idx.ID] = true
				}
			}
		}
		for _, idx := range r.next.Indexes {
			if check[idx.ID] {
				r.uniques = append(r.uniques, idx)
			}
		}
		for _, ord := range r.next.PKOrdinals {
			r.checkPK = r.checkPK || changed(r.next.Columns[ord].Name)
		}
		// 主キーのない表に主キーを足す（ADD PRIMARY KEY）ときは、新しい主キーの重複を確かめる。
		r.checkPK = r.checkPK || (cur.Keyless && !r.next.Keyless)
		return nil
	})
	if err != nil {
		r.release(ctx)
		return nil, err
	}
	if r.view, err = newTable(ctx, t.db, r.next); err != nil {
		r.drop(ctx)
		r.release(ctx)
		return nil, err
	}
	r.keys = r.view.keyIndexes()
	if r.aiOrd = r.view.autoIncrementOrdinal(); r.aiOrd >= 0 {
		r.nextAutoInc = 1
		if t.autoIncrementOrdinal() >= 0 {
			c, err := t.autoIncrement(ctx)
			if err != nil {
				r.drop(ctx)
				r.release(ctx)
				return nil, err
			}
			r.nextAutoInc = c.Peek()
		}
	}
	if r.checkPK || len(r.uniques) > 0 {
		r.txn = t.db.st.Begin()
		r.pendingPKs = map[string]sql.Row{}
		r.pendingVals = map[int64]map[string][]sql.Row{}
	}
	return r, nil
}

func (r *rewriter) StatementBegin(*sql.Context) {}

func (r *rewriter) DiscardChanges(ctx *sql.Context, _ error) error {
	if !r.discarded && !r.finished {
		r.discarded = true
		r.drop(ctx)
		r.release(ctx)
	}
	return nil
}

func (r *rewriter) StatementComplete(*sql.Context) error { return nil }

func (r *rewriter) Insert(ctx *sql.Context, row sql.Row) error {
	r.rows++
	if err := interrupted(ctx, r.rows); err != nil {
		return err
	}
	v := r.view
	if v.ridOrd >= 0 {
		var err error
		if row, err = r.withRowID(ctx, row); err != nil {
			return err
		}
	}
	pk, err := v.encodePK(row)
	if err != nil {
		return err
	}
	if err := r.check(ctx, row, pk); err != nil {
		return err
	}
	value, err := rowenc.EncodeRow(r.next.Version, row)
	if err != nil {
		return fmt.Errorf("HeimDB: table %s: %w", r.next.Name, err)
	}
	r.add(0, pk, value)
	for _, ki := range r.keys {
		entry, ev, err := v.entry(ki, row)
		if err != nil {
			return err
		}
		r.add(ki.def.ID, entry, ev)
	}
	if r.aiOrd >= 0 {
		lo, hi := v.autoIncrementRange()
		if n, inRange, err := types.Uint64.Convert(ctx, row[r.aiOrd]); err == nil && inRange == sql.InRange && n != nil && n.(uint64) >= lo && n.(uint64) < hi {
			r.nextAutoInc = max(r.nextAutoInc, n.(uint64)+1)
		}
	}
	if r.pendingBytes >= rewriteChunkBytes {
		return r.flush(ctx)
	}
	return nil
}

// keyChanges は、主キーのない表を今のインデックスで決め直すと、保存のキーが変わるか（docs/plan.md の段階 72）。
func (t *Table) keyChanges() bool {
	if !t.def.Keyless {
		return false
	}
	settled := t.def.Clone()
	settled.SettleKey()
	return !settled.SameKey(t.def)
}

// withRowID は、行 ID で並べる表に書き直す行 row（エンジンが変換した、見える列だけの行）に見えない列を入れる
// （docs/plan.md の段階 72）。古い表も行 ID で並べるなら、エンジンが今変換した行（書き直しの読み出しが最後に返した行）の
// 行 ID を引き継ぐ。そうでなければ、ブランチの番号帯で順に振る。
func (r *rewriter) withRowID(ctx *sql.Context, row sql.Row) (sql.Row, error) {
	v := r.view
	var rid uint64
	if r.t.ridOrd >= 0 {
		rid = r.scan.lastRID
	} else {
		lo, _ := v.autoIncrementRange()
		rid = max(r.nextAutoInc, lo)
	}
	full := append(row[:len(v.esch.Schema):len(v.esch.Schema)], rid, nil)
	return v.completeRow(ctx, full, nil)
}

func (r *rewriter) add(index uint64, key, value []byte) {
	r.pending = append(r.pending, rewriteKV{index: index, key: key, value: value})
	r.pendingBytes += len(key) + len(value)
}

// check は、row が、書いた行と主キーや一意インデックスで重ならないかを確かめる（確かめが要る書き直しだけ）。
func (r *rewriter) check(ctx *sql.Context, row sql.Row, pk []byte) error {
	if r.txn == nil {
		return nil
	}
	v := r.view
	if r.checkPK {
		existing, dup := r.pendingPKs[string(pk)]
		if !dup {
			var err error
			if existing, dup, err = v.getByPK(r.txn, pk); err != nil {
				return err
			}
		}
		if dup {
			return v.pkDuplicateErr(row, true, existing)
		}
		r.pendingPKs[string(pk)] = row.Copy()
	}
	e := &editor{t: v}
	unkeyed := false
	for _, idx := range r.uniques {
		ki, ok := v.keyIndex(idx)
		if !ok {
			// キーにできない型を含む一意インデックス: まだコミットしていない行と、コミット済みの行を走査して比べる。
			if err := r.checkPendingRows(ctx, idx, row); err != nil {
				return err
			}
			if err := e.checkUnique(ctx, r.txn, row, nil); err != nil {
				return err
			}
			unkeyed = true
			continue
		}
		hasNull := false
		for _, ord := range ki.ords {
			hasNull = hasNull || row[ord] == nil
		}
		if hasNull {
			continue // NULL を含む行は一意制約の対象外（MySQL と同じ）
		}
		prefix, err := v.valuePrefix(ki, row)
		if err != nil {
			return err
		}
		u := uniqueIndex{ords: ki.ords, name: ki.def.Name}
		for _, c := range ki.def.Columns {
			u.prefixes = append(u.prefixes, c.Length)
		}
		byPrefix := r.pendingVals[int64(idx.ID)]
		if byPrefix == nil {
			byPrefix = map[string][]sql.Row{}
			r.pendingVals[int64(idx.ID)] = byPrefix
		}
		for _, existing := range byPrefix[string(prefix)] {
			match, err := v.columnsMatch(ctx, u, row, existing)
			if err != nil {
				return err
			}
			if match {
				return r.view.uniqueDuplicateErr(row, u, existing)
			}
		}
		if err := e.checkUniqueByIndex(ctx, r.txn, ki, row, nil); err != nil {
			return err
		}
		byPrefix[string(prefix)] = append(byPrefix[string(prefix)], row.Copy())
	}
	if unkeyed {
		r.pendingRows = append(r.pendingRows, row.Copy())
	}
	return nil
}

// checkPendingRows は、キーにできない型を含む一意インデックス idx で、まだコミットしていない行と row が重ならないかを
// 確かめる。
func (r *rewriter) checkPendingRows(ctx *sql.Context, idx schema.Index, row sql.Row) error {
	u := uniqueIndex{name: idx.Name}
	for _, c := range idx.Columns {
		ord := r.next.ColumnIndex(c.Name)
		if row[ord] == nil {
			return nil
		}
		u.ords = append(u.ords, ord)
		u.prefixes = append(u.prefixes, c.Length)
	}
	for _, existing := range r.pendingRows {
		match, err := r.view.columnsMatch(ctx, u, row, existing)
		if err != nil {
			return err
		}
		if match {
			return r.view.uniqueDuplicateErr(row, u, existing)
		}
	}
	return nil
}

// flush は、まだコミットしていない行とエントリを新しいテーブル ID にコミットする。
func (r *rewriter) flush(ctx *sql.Context) error {
	if len(r.pending) == 0 {
		return nil
	}
	t := r.t
	err := t.db.st.Update(func(tx *store.WriteTx) error {
		for _, kv := range r.pending {
			var err error
			if kv.index == 0 {
				err = tx.PutRow(t.db.branch, r.next.ID, kv.key, kv.value)
			} else {
				err = tx.PutIndexEntry(t.db.branch, r.next.ID, kv.index, kv.key, kv.value)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.pending, r.pendingBytes = nil, 0
	if r.pendingPKs != nil {
		r.pendingPKs = map[string]sql.Row{}
		r.pendingVals = map[int64]map[string][]sql.Row{}
		r.pendingRows = nil
	}
	return nil
}

// Close は、残りの行を書き、テーブル定義を新しいテーブル ID に向ける（書きかけの印を外す）。
func (r *rewriter) Close(ctx *sql.Context) error {
	if r.discarded || r.finished {
		return nil
	}
	r.finished = true
	defer r.release(ctx)
	t := r.t
	if err := r.flush(ctx); err != nil {
		r.drop(ctx)
		return err
	}
	err := t.db.st.Update(func(tx *store.WriteTx) error {
		cur, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
		if err != nil {
			return err
		}
		if !ok || cur.ID != t.def.ID {
			return sql.ErrTableNotFound.New(t.def.Name)
		}
		if r.aiOrd >= 0 {
			tx.SetAutoIncrement(t.db.branch, r.next.ID, r.nextAutoInc)
		}
		if err := tx.UnmarkRewrite(t.db.branch, r.next.ID); err != nil {
			return err
		}
		return tx.PutTable(t.db.branch, r.next)
	})
	if err != nil {
		r.drop(ctx)
		return err
	}
	updated, err := newTable(ctx, t.db, r.next)
	if err != nil {
		return err
	}
	rowCounts.forgetTable(t.db.st, t.db.branch, t.def.ID)
	*t = *updated
	if r.newColumn != nil {
		warnStoredAsUTF8MB4(ctx, t.def.Name, []*sql.Column{r.newColumn}, sql.Collation_Unspecified)
	}
	return nil
}

// release は、書き直しのために開いたものを閉じる。
func (r *rewriter) release(*sql.Context) {
	r.t.nullFill = nil
	if r.t.rewriting == r {
		r.t.rewriting = nil
	}
	if r.scan != nil {
		r.scan.close()
		r.scan = nil
	}
	if r.txn != nil {
		_ = r.txn.Rollback()
		r.txn = nil
	}
	r.pending, r.pendingPKs, r.pendingVals, r.pendingRows = nil, nil, nil, nil
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
	if r.sess != nil {
		r.sess.rewriters = slices.DeleteFunc(r.sess.rewriters, func(x *rewriter) bool { return x == r })
		r.sess = nil
	}
}

// drop は、書きかけの新しいテーブル ID のデータと印を消す（ロックを放す前に呼ぶ）。消せなくても、次の起動で消える
// （store の cleanupRewrites）。
func (r *rewriter) drop(ctx *sql.Context) {
	if r.next == nil {
		return
	}
	if err := r.t.db.st.Update(func(tx *store.WriteTx) error {
		return tx.DropRewrite(r.t.db.branch, r.next.ID)
	}); err != nil {
		logrus.Errorf("HeimDB: drop the data of a failed ALTER TABLE on %s: %v", r.t.def.Name, err)
	}
}

// remapIndexes は、列の変更に合わせてインデックスの定義を作り直す。名前を変えた列は付け替え、消した列は外す
// （MySQL と同じく、列がすべて消えたインデックスは削除する）。インデックス ID は新しいテーブル ID の下で使い続ける。
func remapIndexes(indexes []schema.Index, oldColumn, newColumn *sql.Column, next *schema.Table) []schema.Index {
	var out []schema.Index
	for _, idx := range indexes {
		var cols []schema.IndexColumn
		for _, c := range idx.Columns {
			name := c.Name
			if oldColumn != nil && strings.EqualFold(name, oldColumn.Name) {
				if newColumn == nil {
					continue // 消した列
				}
				name = newColumn.Name
			}
			if next.ColumnIndex(name) < 0 {
				continue
			}
			cols = append(cols, schema.IndexColumn{Name: name, Length: c.Length})
		}
		if len(cols) == 0 {
			continue
		}
		idx.Columns = cols
		out = append(out, idx)
	}
	return out
}

// rewriteScanPage は、書き直しのために古い行を読む 1 回の量。
var rewriteScanPage = 1000

// rewriteScan は、書き直しのために古いテーブルの行を、1 つのスナップショットのまま rewriteScanPage 行ずつ読む
// sql.RowIter（全行をメモリに集めない）。ALTER の間は書き込みのロックを持つので、読む間に行は変わらない。
type rewriteScan struct {
	t    *Table
	txn  *store.Txn
	want []bool
	rows []sql.Row
	i    int
	last []byte // 最後に読んだ行の主キー（次のページはここから）
	done bool
	n    int // 返した行の数（警告の行番号）
	// lastRID は、最後に返した行の行 ID（行 ID で並べる表。rewriter.withRowID）。
	lastRID uint64
}

func (t *Table) newRewriteScan() *rewriteScan {
	return &rewriteScan{t: t, txn: t.db.st.Begin(), want: t.readColumns(nil)}
}

func (it *rewriteScan) Next(ctx *sql.Context) (sql.Row, error) {
	for it.i >= len(it.rows) {
		if it.done {
			return nil, io.EOF
		}
		if err := it.fill(ctx); err != nil {
			return nil, err
		}
	}
	row := it.rows[it.i]
	it.rows[it.i] = nil
	it.i++
	it.n++
	if it.t.ridOrd >= 0 {
		it.lastRID = row[it.t.ridOrd].(uint64)
	}
	if it.t.nullFill != nil {
		if err := it.t.nullFill.applyRow(ctx, row, it.n); err != nil {
			return nil, err
		}
	}
	return row, nil
}

// fill は次のページを読む。主キーは前のページの最後の行から読み始め、その行は飛ばす（主キーの符号は、ある値が別の
// 値の先頭部分になりうるので、範囲の端を作らずに同じ値かで比べる）。
func (it *rewriteScan) fill(ctx *sql.Context) error {
	t := it.t
	it.rows, it.i = make([]sql.Row, 0, rewriteScanPage), 0
	last := it.last
	err := it.txn.ScanRows(t.db.branch, t.def.ID, last, nil, false, func(r store.RowVersion) (bool, error) {
		if last != nil && bytes.Equal(r.PK, last) {
			return true, nil
		}
		if err := interrupted(ctx, it.n+len(it.rows)+1); err != nil {
			return false, err
		}
		row, err := t.decodeRowColumns(r.Value, it.want)
		if err != nil {
			return false, err
		}
		it.rows = append(it.rows, row)
		it.last = r.PK
		return len(it.rows) < rewriteScanPage, nil
	})
	if err != nil {
		return err
	}
	it.done = len(it.rows) < rewriteScanPage
	return nil
}

func (it *rewriteScan) Close(*sql.Context) error {
	it.close()
	return nil
}

func (it *rewriteScan) close() {
	if it.txn != nil {
		_ = it.txn.Rollback()
		it.txn = nil
	}
}

// Truncate は sql.TruncateableTable の実装。
//
// TRUNCATE 文は DDL として、テーブルを新しいテーブル ID に向け直す（古い行は AS OF で読める。大きさの上限を超えた
// ブランチでも通す）。AUTO_INCREMENT は MySQL と同じく 1 に戻る。go-mysql-server は、AUTO_INCREMENT 列のないテーブルへの
// 条件のない DELETE も Truncate に変換する（sql/analyzer/process_truncate.go）。DELETE はトランザクションの一部なので、
// そのときは全行を 1 行ずつ削除する。
func (t *Table) Truncate(ctx *sql.Context) (int, error) {
	if !isTruncateQuery(ctx.Query()) {
		// 全行を集めずに 1 行ずつ消す。走査は開いた時点の行だけを見る（docs/plan.md の段階 55）。
		it, err := t.newScanRowIter(ctx, nil, false, nil, nil)
		if err != nil {
			return 0, err
		}
		defer it.Close(ctx)
		e := &editor{t: t}
		n := 0
		for {
			row, err := it.Next(ctx)
			if err == io.EOF {
				return n, nil
			} else if err != nil {
				return 0, err
			}
			if err := e.Delete(ctx, row); err != nil {
				return 0, err
			}
			n++
		}
	}
	var next *schema.Table
	err := t.db.dropDDL(ctx, func(tx *store.WriteTx) error {
		cur, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
		if err != nil {
			return err
		}
		if !ok || cur.ID != t.def.ID {
			return sql.ErrTableNotFound.New(t.def.Name)
		}
		if cur.ID, err = tx.NextTableID(); err != nil {
			return err
		}
		next = cur
		next.Version = tx.TS
		return tx.PutTable(t.db.branch, next)
	})
	if err != nil {
		return 0, err
	}
	updated, err := newTable(ctx, t.db, next)
	if err != nil {
		return 0, err
	}
	rowCounts.forgetTable(t.db.st, t.db.branch, t.def.ID)
	*t = *updated
	// MySQL の TRUNCATE は影響行数 0 を返す。
	return 0, nil
}

// isTruncateQuery は、クエリが TRUNCATE 文かどうかを先頭の語で判定する。
func isTruncateQuery(q string) bool {
	q = skipSpaceAndComments(q)
	return len(q) >= 8 && strings.EqualFold(q[:8], "TRUNCATE")
}

// ModifyDefaultCollation は sql.CollationAlterableTable の実装（ALTER TABLE ... COLLATE）。既定の照合順序だけを変える。
// utf8mb3 と ascii は utf8mb4 の同じ名前の照合順序にする（docs/plan.md の段階 37、57）。
func (t *Table) ModifyDefaultCollation(ctx *sql.Context, collation sql.CollationID) error {
	if err := checkCharset(t.def.Name, collation); err != nil {
		return err
	}
	stored, _, err := schema.MapToUTF8MB4(collation)
	if err != nil {
		return err
	}
	err = t.alter(ctx, func(_ *store.WriteTx, next *schema.Table) error {
		next.Collation = stored.Name()
		return nil
	})
	if err == nil {
		warnStoredAsUTF8MB4(ctx, t.def.Name, nil, collation)
	}
	return err
}

// ModifyStoredCollation は sql.CollationAlterableTable の実装（ALTER TABLE ... CONVERT TO CHARACTER SET）。
// 文字列の列の照合順序がすでにすべて collation なら、既定の照合順序だけを変える。列の照合順序を変える変換は
// go-mysql-server の memory と同じく対応しない。
func (t *Table) ModifyStoredCollation(ctx *sql.Context, collation sql.CollationID) error {
	for _, c := range t.sch.Schema {
		if st, ok := c.Type.(sql.StringType); ok && types.IsText(c.Type) && st.Collation() != collation {
			return fmt.Errorf("HeimDB: converting the collation of column %s is not supported", c.Name)
		}
	}
	return t.ModifyDefaultCollation(ctx, collation)
}
