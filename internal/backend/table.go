package backend

import (
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/dolthub/vitess/go/sqltypes"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// Table は 1 つのテーブル定義のバージョンを go-mysql-server のテーブルとして見せる。
// 行の読み書きは、セッションのトランザクション（store.Txn）を通す。
type Table struct {
	db  *Database
	def *schema.Table
	// sch は HeimDB の中で使うスキーマ（主キーのない表では、見えない列と保存のキーを含む）。esch はエンジンに見せる
	// スキーマ（schema.Table.EngineSchema。主キーのない表の見えない列を除く）。
	sch, esch sql.PrimaryKeySchema
	collation sql.CollationID
	// hideFulltext は、GetIndexes で FULLTEXT インデックスを返さないかどうか（fulltextBeforeAnalysis を参照）。
	hideFulltext bool
	// asOf は読む時点。store.Latest なら最新の状態にセッションの未コミットの変更を重ねて読み、
	// それ以外は AS OF で指定された過去の時点を読み取り専用で読む。
	asOf uint64
	// nullFill は、列を NOT NULL に変える書き直しの間だけ、行を読むときに NULL を置き換える列（alter.go）。
	nullFill *nullFill
	// rewriting は、この表を書き直している ALTER TABLE（RewriteInserter から Close まで）。全件の走査を少しずつ読む。
	rewriting *rewriter
	// proj は、文が使う列（go-mysql-server の pruneTables が WithProjections で渡す）。nil ならすべての列。
	proj *projection
	// keys は、セカンダリインデックスのうちキーを持つもの（newTable で作る。def と一緒に置き換わる）。
	keys []keyIndex
	// ridOrd と hashOrd は、行 ID で並べる表の見えない列の位置（docs/plan.md の段階 72）。ほかの表は -1。ridIndex は
	// hashOrd の見えないインデックス。
	ridOrd, hashOrd int
	ridIndex        keyIndex
	// strPK は、主キーが整数でない列を含むかどうか（newTable で 1 回だけ求める。docs/plan.md の段階 48。インデックスの
	// エントリごとに型を調べると、インデックスを読む文が 1 割ほど遅くなっていた）。
	strPK bool
	// decimals は、DECIMAL の列の位置と小数の桁（newTable で求める）。保存した値は後ろの 0 を持たない（rowenc は
	// decimal の String で書く）ので、読むときに列の桁で表す（段階 116。前は CONCAT(price) が 10.5 になった）。
	decimals []decimalColumn
}

// decimalColumn は、DECIMAL の列の位置と小数の桁。
type decimalColumn struct {
	ord   int
	scale int32
}

// projection は、ProjectedTable として返す列（docs/plan.md の段階 27）。
type projection struct {
	names []string
	ords  []int // 全体のスキーマでの位置（names と同じ並び）
	sch   sql.Schema
	want  []bool // 行を読むときに復号する列（ords と主キー。readColumns）
}

var (
	_ sql.Table               = (*Table)(nil)
	_ sql.CommentedTable      = (*Table)(nil)
	_ sql.PrimaryKeyTable     = (*Table)(nil)
	_ sql.Databaseable        = (*Table)(nil)
	_ sql.IndexAddressable    = (*Table)(nil)
	_ sql.IndexAlterableTable = (*Table)(nil)
	_ sql.InsertableTable     = (*Table)(nil)
	_ sql.UpdatableTable      = (*Table)(nil)
	_ sql.DeletableTable      = (*Table)(nil)
	_ sql.ReplaceableTable    = (*Table)(nil)
	_ sql.AutoIncrementTable  = (*Table)(nil)
	_ sql.IndexedTable        = (*IndexedTable)(nil)
	_ sql.TemporaryTable      = (*Table)(nil)
	_ sql.ProjectedTable      = (*Table)(nil)
)

// IsTemporary は sql.TemporaryTable の実装。
func (t *Table) IsTemporary() bool { return t.db.temporary }

func newTable(ctx *sql.Context, db *Database, def *schema.Table) (*Table, error) {
	sch, err := def.Schema(db.name)
	if err != nil {
		return nil, err
	}
	esch := sch
	if def.Keyless {
		if esch, err = def.EngineSchema(db.name); err != nil {
			return nil, err
		}
	}
	collation, err := def.CollationID()
	if err != nil {
		return nil, err
	}
	t := &Table{db: db, def: def, sch: sch, esch: esch, collation: collation, asOf: store.Latest}
	for _, ord := range def.PKOrdinals {
		if !types.IsInteger(sch.Schema[ord].Type) {
			t.strPK = true
		}
	}
	for i, c := range sch.Schema {
		if dt, ok := c.Type.(sql.DecimalType); ok && types.IsDecimal(c.Type) {
			t.decimals = append(t.decimals, decimalColumn{ord: i, scale: int32(dt.Scale())})
		}
	}
	for _, idx := range def.Indexes {
		if ki, ok := t.keyIndex(idx); ok {
			t.keys = append(t.keys, ki)
			if idx.Hidden {
				t.ridIndex = ki
			}
		}
	}
	t.ridOrd, t.hashOrd = def.RowIDOrdinal(), -1
	if t.ridOrd >= 0 {
		t.hashOrd = def.ColumnIndex(schema.RowHashColumn)
	}
	return t, nil
}

// Name は sql.Table の実装。
func (t *Table) Name() string { return t.def.Name }

// String は sql.Table の実装。
func (t *Table) String() string { return t.def.Name }

// Schema は sql.Table の実装。列を絞っていれば（WithProjections）、その列だけを返す。
func (t *Table) Schema() sql.Schema {
	if t.proj != nil {
		return t.proj.sch
	}
	return t.esch.Schema
}

// visibleRow は、HeimDB の中の行 row を、エンジンに見せる列だけにする（主キーのない表の見えない列を除く）。
func (t *Table) visibleRow(row sql.Row) sql.Row {
	if n := len(t.esch.Schema); row != nil && len(row) > n {
		return row[:n:n]
	}
	return row
}

// WithProjections は sql.ProjectedTable の実装。読み出しの中では全体のスキーマの行を作り、返すときに列を絞る
// （projectIter）。知らない列があれば絞らない。
func (t *Table) WithProjections(names []string) sql.Table {
	nt := *t
	nt.proj = nil
	if names == nil {
		return &nt
	}
	p := &projection{names: names}
	for _, name := range names {
		ord := t.def.ColumnIndex(name)
		if ord < 0 || ord >= len(t.esch.Schema) {
			return &nt
		}
		p.ords = append(p.ords, ord)
		p.sch = append(p.sch, t.esch.Schema[ord])
	}
	p.want = make([]bool, len(t.sch.Schema))
	for _, ord := range p.ords {
		p.want[ord] = true
	}
	for _, ord := range t.def.PKOrdinals {
		p.want[ord] = true
	}
	nt.proj = p
	return &nt
}

// Projections は sql.ProjectedTable の実装。
func (t *Table) Projections() []string {
	if t.proj == nil {
		return nil
	}
	return t.proj.names
}

// noRowReuse は、行の使い回し（rowReuser）をやめる（使い回す場合とやめた場合の結果を比べるテスト用）。
var noRowReuse atomic.Bool

// projectRows は、全体のスキーマの行を返す it を、列を絞った行を返すようにする。主キーのない表の見えない列は、絞って
// いなくても除く。
func (t *Table) projectRows(it sql.RowIter, err error) (sql.RowIter, error) {
	if err != nil {
		return it, err
	}
	if t.proj == nil {
		if len(t.esch.Schema) < len(t.sch.Schema) {
			return &visibleIter{child: it, t: t}, nil
		}
		return it, nil
	}
	// projectIter は子の行から値を写してすぐに捨てるので、子は行を使い回してよい（rowReuser。段階 125）
	if r, ok := it.(rowReuser); ok && t.nullFill == nil && !noRowReuse.Load() {
		r.reuseRows()
	}
	return &projectIter{child: it, ords: t.proj.ords}, nil
}

// visibleIter は、行から主キーのない表の見えない列を除く sql.RowIter。
type visibleIter struct {
	child sql.RowIter
	t     *Table
}

func (it *visibleIter) Next(ctx *sql.Context) (sql.Row, error) {
	row, err := it.child.Next(ctx)
	return it.t.visibleRow(row), err
}

func (it *visibleIter) Close(ctx *sql.Context) error { return it.child.Close(ctx) }

// projectIter は、行を ords の列だけにする sql.RowIter。
type projectIter struct {
	child sql.RowIter
	ords  []int
}

func (it *projectIter) Next(ctx *sql.Context) (sql.Row, error) {
	row, err := it.child.Next(ctx)
	if err != nil {
		return nil, err
	}
	out := make(sql.Row, len(it.ords))
	for i, ord := range it.ords {
		out[i] = row[ord]
	}
	return out, nil
}

func (it *projectIter) Close(ctx *sql.Context) error { return it.child.Close(ctx) }

// CountRest は sql.RowCounter の実装（列を選ぶだけなので、子に数えさせる。visibleIter も同じ）。
func (it *projectIter) CountRest(ctx *sql.Context) (int64, bool, error) {
	return countRest(ctx, it.child)
}

func (it *visibleIter) CountRest(ctx *sql.Context) (int64, bool, error) {
	return countRest(ctx, it.child)
}

// countRest は、it が sql.RowCounter なら残りの行を数えさせる。
func countRest(ctx *sql.Context, it sql.RowIter) (int64, bool, error) {
	if rc, ok := it.(sql.RowCounter); ok {
		return rc.CountRest(ctx)
	}
	return 0, false, nil
}

// Collation は sql.Table の実装。
func (t *Table) Collation() sql.CollationID { return t.collation }

// Comment は sql.CommentedTable の実装。
func (t *Table) Comment() string { return t.def.Comment }

// PrimaryKeySchema は sql.PrimaryKeyTable の実装。
func (t *Table) PrimaryKeySchema() sql.PrimaryKeySchema { return t.esch }

// Database は sql.Databaseable の実装。
func (t *Table) Database() string { return t.db.name }

// Partitions は sql.Table の実装。全行を 1 つのパーティションで返す。
func (t *Table) Partitions(*sql.Context) (sql.PartitionIter, error) {
	return sql.PartitionsToPartitionIter(partition{}), nil
}

// PartitionRows は sql.Table の実装。行は主キーの順に返す。
func (t *Table) PartitionRows(ctx *sql.Context, p sql.Partition) (sql.RowIter, error) {
	if part, ok := p.(partition); ok && part.lookup != nil {
		return t.projectRows(t.lookupRows(ctx, *part.lookup))
	}
	if r := t.rewriting; r != nil {
		// ALTER TABLE の書き直し: 全行をメモリに集めずに読む（docs/plan.md の段階 53）。
		it := t.newRewriteScan()
		if r.scan != nil {
			r.scan.close()
		}
		r.scan = it
		return t.projectRows(it, nil)
	}
	// 全行をメモリに集めずに、1 行ずつ読む（docs/plan.md の段階 55）。
	return t.projectRows(t.newScanRowIter(ctx, nil, false, t.readColumns(nil), nil))
}

// loadedRows は、読み込んだ行を返す sql.RowIter を作る（ALTER の途中なら NULL を置き換える）。
func (t *Table) loadedRows(ctx *sql.Context, rows []sql.Row) (sql.RowIter, error) {
	if t.nullFill != nil {
		if err := t.nullFill.apply(ctx, rows); err != nil {
			return nil, err
		}
	}
	return &rowIter{rows: rows}, nil
}

// Inserter は sql.InsertableTable の実装。
func (t *Table) Inserter(*sql.Context) sql.RowInserter { return &editor{t: t} }

// Updater は sql.UpdatableTable の実装。
func (t *Table) Updater(*sql.Context) sql.RowUpdater { return &editor{t: t} }

// Deleter は sql.DeletableTable の実装。
func (t *Table) Deleter(*sql.Context) sql.RowDeleter { return &editor{t: t} }

// Replacer は sql.ReplaceableTable の実装。
func (t *Table) Replacer(*sql.Context) sql.RowReplacer { return &editor{t: t} }

// AutoIncrementSetter は sql.AutoIncrementTable の実装。
func (t *Table) AutoIncrementSetter(*sql.Context) sql.AutoIncrementSetter { return &editor{t: t} }

// PeekNextAutoIncrementValue は sql.AutoIncrementGetter の実装。行 ID で並べる表は、AUTO_INCREMENT の列のない表と
// 同じに見せる（行 ID は見えない AUTO_INCREMENT の列で採番する）。
func (t *Table) PeekNextAutoIncrementValue(ctx *sql.Context) (uint64, error) {
	if t.def.RowIDOrdinal() >= 0 {
		return 1, nil
	}
	c, err := t.autoIncrement(ctx)
	if err != nil {
		return 0, err
	}
	return c.Peek(), nil
}

// GetNextAutoIncrementValue は sql.AutoIncrementTable の実装。insertVal が nil なら（INSERT で値を指定しなかったか、
// 0 を指定した）次の値を払い出す。値が指定されていれば、それ以下を払い出さないようカウンタを進める。
func (t *Table) GetNextAutoIncrementValue(ctx *sql.Context, insertVal interface{}) (uint64, error) {
	c, err := t.autoIncrement(ctx)
	if err != nil {
		return 0, err
	}
	if insertVal == nil {
		v, err := c.Next()
		if err != nil {
			_, hi := t.autoIncrementRange()
			return 0, fmt.Errorf("%w: table %s, values below %d on this branch", err, t.def.Name, hi)
		}
		return v, nil
	}
	v, inRange, err := types.Uint64.Convert(ctx, insertVal)
	if err == nil && inRange == sql.InRange {
		c.Observe(v.(uint64))
	}
	return c.Peek(), nil
}

func (t *Table) autoIncrementOrdinal() int {
	for i, c := range t.def.Columns {
		if c.AutoIncrement {
			return i
		}
	}
	return -1
}

// autoIncrement はテーブルの AUTO_INCREMENT のカウンタを返す。カウンタはブランチの番号帯の中で動き、初めて使うときは
// 帯の中にある既存の行の最大値 + 1 と、帯の始まりの大きい方から始める。
func (t *Table) autoIncrement(ctx *sql.Context) (*store.AutoIncrement, error) {
	lo, hi := t.autoIncrementRange()
	return t.db.st.AutoIncrement(t.db.branch, t.def.ID, lo, hi, func() (uint64, error) {
		m, err := t.maxAutoIncrementValue(ctx)
		return m + 1, err
	})
}

// autoIncrementBlockSize は、ブランチごとの AUTO_INCREMENT の番号帯の幅（docs/plan.md の段階 6）。
const autoIncrementBlockSize = 10_000_000_000

// 32 ビットの列の番号帯（docs/plan.md の段階 61）。main は [1, 10^9)、ほかのブランチは 10^9 から上の 10^7 の幅の
// 枠を 100 個使い回す（符号付きの上限 2,147,483,647 に収まる）。
const (
	intAutoIncrementBase      = 1_000_000_000
	intAutoIncrementBlockSize = 10_000_000
	intAutoIncrementSlots     = 100
)

// autoIncrementRange は、このテーブルの AUTO_INCREMENT の番号帯 [lo, hi) を返す。
func (t *Table) autoIncrementRange() (lo, hi uint64) {
	ord := t.autoIncrementOrdinal()
	if ord < 0 {
		return 1, math.MaxUint64
	}
	return autoIncrementBand(t.db.branch, t.sch.Schema[ord].Type.Type())
}

// autoIncrementBand は、ブランチ b の、型 typ の AUTO_INCREMENT の列の番号帯 [lo, hi) を返す。ブランチ ID n
// （main は 1）の 64 ビットの列は [(n - 1) × 10^10, n × 10^10) で、ブランチどうしで ID が衝突しない。main の帯は
// [1, 10^10) で、マージで入ったブランチの ID（10^10 以上）では main のカウンタは進まない。32 ビットの列は、main が
// [1, 10^9)、ブランチ n が 10^9 + ((n - 2) mod 100) × 10^7 からの 10^7 の幅。枠を使い回しても、カウンタは帯の中の
// 見えている行の最大値 + 1 から始めるので、前に同じ枠を使ったブランチのマージ済みの行とは重ならない。一時テーブルと
// 24 ビット以下の列は帯を分けない（幅が足りない）。
func autoIncrementBand(b uint64, typ querypb.Type) (lo, hi uint64) {
	if store.IsHiddenBranch(b) {
		return 1, math.MaxUint64
	}
	switch typ {
	case sqltypes.Int64, sqltypes.Uint64:
		lo = (b - 1) * autoIncrementBlockSize
		return max(lo, 1), lo + autoIncrementBlockSize
	case sqltypes.Int32, sqltypes.Uint32:
		if b == 1 {
			return 1, intAutoIncrementBase
		}
		lo = intAutoIncrementBase + (b-2)%intAutoIncrementSlots*intAutoIncrementBlockSize
		return lo, lo + intAutoIncrementBlockSize
	}
	return 1, math.MaxUint64
}

// maxAutoIncrementValue は、既存の行のうち番号帯の中にある AUTO_INCREMENT 列の最大値を返す。なければ 0。
func (t *Table) maxAutoIncrementValue(ctx *sql.Context) (uint64, error) {
	ord := t.autoIncrementOrdinal()
	if ord < 0 {
		return 0, nil
	}
	lo, hi := t.autoIncrementRange()
	if slices.Equal(t.def.PKOrdinals, []int{ord}) {
		return t.lastAutoIncrementValue(ctx, ord, lo, hi)
	}
	return t.scanAutoIncrementValue(ctx, ord, lo, hi)
}

// scanAutoIncrementValue は、すべての行を読んで、番号帯 [lo, hi) の中の AUTO_INCREMENT 列の最大値を返す。
func (t *Table) scanAutoIncrementValue(ctx *sql.Context, ord int, lo, hi uint64) (uint64, error) {
	// 行を集めずに、AUTO_INCREMENT の列だけを読む（大きな表で、起動後に初めて求めるときに全行をメモリに載せない。
	// docs/plan.md の段階 53）。
	want := make([]bool, len(t.sch.Schema))
	want[ord] = true
	var m uint64
	err := t.eachRow(ctx, nil, nil, false, want, func(row sql.Row) error {
		v, inRange, err := types.Uint64.Convert(ctx, row[ord])
		if err == nil && inRange == sql.InRange && v != nil && v.(uint64) >= lo && v.(uint64) < hi {
			m = max(m, v.(uint64))
		}
		return nil
	})
	return m, err
}

// lastAutoIncrementValue は、保存のキーが AUTO_INCREMENT の列だけの表（WordPress の表はすべてこの形）で、番号帯
// [lo, hi) の中の最後の行を逆に 1 行だけ読んで、その値を返す（段階 80）。全行を読むと、起動し直した後の表への最初の
// 書き込みが、311 万行の wp_postmeta で数秒止まっていた（ホスティングの本番）。削除した行と古いバージョンは走査が隠し、層のあるブランチは
// 重ねて読む。帯の上（マージで入った他のブランチの ID）は hi で除く。
func (t *Table) lastAutoIncrementValue(ctx *sql.Context, ord int, lo, hi uint64) (uint64, error) {
	typ := t.sch.Schema[ord].Type
	bound := func(v uint64) ([]byte, error) {
		cv, inRange, err := typ.Convert(ctx, v)
		if err != nil || inRange != sql.InRange {
			return nil, nil // 列の型に収まらない端は、端なしで読む
		}
		row := make(sql.Row, len(t.sch.Schema))
		row[ord] = cv
		return t.encodePK(row)
	}
	loKey, err := bound(lo)
	if err != nil {
		return 0, err
	}
	hiKey, err := bound(hi)
	if err != nil {
		return 0, err
	}
	want := make([]bool, len(t.sch.Schema))
	want[ord] = true
	var m uint64
	err = t.eachRow(ctx, loKey, hiKey, true, want, func(row sql.Row) error {
		v, inRange, err := types.Uint64.Convert(ctx, row[ord])
		if err == nil && inRange == sql.InRange && v != nil && v.(uint64) >= lo && v.(uint64) < hi {
			m = v.(uint64)
			return errFoundLast
		}
		return nil
	})
	if errors.Is(err, errFoundLast) {
		err = nil
	}
	return m, err
}

// errFoundLast は、lastAutoIncrementValue が最後の行を見つけて走査を止めるための印。
var errFoundLast = errors.New("found the last row")

func (t *Table) encodePK(row sql.Row) ([]byte, error) {
	pk, err := rowenc.EncodePK(nil, t.sch, row)
	if err != nil {
		return nil, fmt.Errorf("HeimDB: table %s: %w", t.def.Name, err)
	}
	return pk, nil
}

// getByPK は、最新の確定済みデータに自分の変更を重ねて主キー pk の行を返す。書き込みの検査に使う。
func (t *Table) getByPK(txn *store.Txn, pk []byte) (sql.Row, bool, error) {
	v, ok, err := txn.GetRowLatest(t.db.branch, t.def.ID, pk)
	if err != nil || !ok {
		return nil, false, err
	}
	row, err := t.decodeRow(v)
	return row, err == nil, err
}

// GetIndexes は sql.IndexAddressable の実装。主キーを PRIMARY として先頭に返す（主キーのない表は返さない。見えない
// インデックスも返さない）。
func (t *Table) GetIndexes(*sql.Context) ([]sql.Index, error) {
	var indexes []sql.Index
	if !t.def.Keyless {
		pk := make([]schema.IndexColumn, len(t.def.PKOrdinals))
		for i, ord := range t.def.PKOrdinals {
			pk[i] = schema.IndexColumn{Name: t.def.Columns[ord].Name}
		}
		indexes = append(indexes, t.newIndex("PRIMARY", pk, true, ""))
	}
	// MySQL（InnoDB）と同じく、主キー、一意インデックス、それ以外のインデックス、FULLTEXT インデックスの順に、
	// それぞれ作った順で並べる。
	kind := func(idx schema.Index) int {
		switch {
		case idx.Unique:
			return 0
		case idx.Fulltext:
			return 2
		}
		return 1
	}
	for k := 0; k < 3; k++ {
		for _, idx := range t.def.Indexes {
			if (idx.Fulltext && t.hideFulltext) || idx.Hidden {
				continue
			}
			if kind(idx) == k {
				ni := t.newIndex(idx.Name, idx.Columns, idx.Unique, idx.Comment)
				ni.fulltext = idx.Fulltext
				indexes = append(indexes, ni)
			}
		}
	}
	return indexes, nil
}

// IndexedAccess は sql.IndexAddressable の実装。
func (t *Table) IndexedAccess(_ *sql.Context, lookup sql.IndexLookup) sql.IndexedTable {
	return &IndexedTable{Table: t, lookup: lookup}
}

// PreciseMatch は sql.IndexAddressable の実装。false にして、エンジンに条件の評価を残させる。
func (t *Table) PreciseMatch() bool { return false }

// IndexedTable は、インデックスによる検索を指定したテーブル。
type IndexedTable struct {
	*Table
	lookup sql.IndexLookup
}

// WithProjections は sql.ProjectedTable の実装（検索の指定を残す）。
func (t *IndexedTable) WithProjections(names []string) sql.Table {
	return &IndexedTable{Table: t.Table.WithProjections(names).(*Table), lookup: t.lookup}
}

// LookupPartitions は sql.IndexedTable の実装。
func (t *IndexedTable) LookupPartitions(_ *sql.Context, lookup sql.IndexLookup) (sql.PartitionIter, error) {
	return sql.PartitionsToPartitionIter(partition{lookup: &lookup}), nil
}

// CreateIndex は sql.IndexAlterableTable の実装。インデックス定義を保存し、キーを持てるインデックスなら
// 既存の行からエントリを作る。一意インデックスで既存の行が重複していたらエラーにする。
//
// エントリは、行を集めずに 1 行ずつ作り、rewriteChunkBytes ごとにコミットする（docs/plan.md の段階 53）。新しい
// インデックス ID は、最後のコミットでテーブル定義に足すまでどこからも読まれず、作成の途中の印で GC から守る
// （buildIndex）。作成が失敗したらエントリと印を消す。プロセスが途中で落ちて残ったものは、本番の起動時に消える。
// 印より前のバージョンで残ったエントリは、同じ ID で作り直すときに消す。
func (t *Table) CreateIndex(ctx *sql.Context, def sql.IndexDef) error {
	if def.IsPrimary() {
		return fmt.Errorf("HeimDB: adding a primary key is not supported")
	}
	def, err := t.fulltextDefinition(def)
	if err != nil {
		return err
	}
	if err := checkBranchSize(t.db.st, t.db.branch, t.def.Name); err != nil {
		return err
	}
	unlock, err := t.db.lockDDL(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cur, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
	if err != nil {
		return err
	}
	if !ok || cur.ID != t.def.ID {
		return sql.ErrTableNotFound.New(t.def.Name)
	}
	next := cur.Clone()
	if err := next.AddIndex(def); err != nil {
		return err
	}
	// renew は、空の表の保存のキーを変えるので、TRUNCATE と同じく新しいテーブル ID にするか（古い ID の過去のバージョンと、形の
	// 違う行が混ざらないように）。
	renew := false
	if next.Keyless {
		// 主キーのない表を並べる一意インデックスが変わる（docs/plan.md の段階 72）。空の表（CREATE TABLE の途中）は定義
		// だけを変える。行のある表は、エンジンがこの後で呼ぶ書き直し（RewriteInserter）で変える。
		settled := next.Clone()
		settled.SettleKey()
		if !settled.SameKey(next) {
			empty, err := t.empty(ctx)
			if err != nil {
				return err
			}
			if empty {
				next, renew = settled, true
			}
		}
	}
	view, err := newTable(ctx, t.db, next)
	if err != nil {
		return err
	}
	added := next.Indexes[len(next.Indexes)-1]
	ki, keyed := view.keyIndex(added)
	started := false
	if keyed && !renew {
		if started, err = view.buildIndex(ctx, ki); err != nil {
			if started {
				view.dropIndexBuild(ki)
			}
			return err
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
		if renew {
			if next.ID, err = tx.NextTableID(); err != nil {
				return err
			}
		} else if started {
			if err := tx.UnmarkIndexBuild(t.db.branch, t.def.ID, added.ID); err != nil {
				return err
			}
		} else if err := tx.DropIndexData(t.db.branch, t.def.ID, added.ID); err != nil {
			// エントリを書かなかった（行がない、キーを持たない）。前に途中で終わった作成のエントリが残っていれば消す。
			return err
		}
		next.Version = tx.TS
		return tx.PutTable(t.db.branch, next)
	})
	if err != nil {
		if started {
			view.dropIndexBuild(ki)
		}
		return err
	}
	updated, err := newTable(ctx, t.db, next)
	if err != nil {
		return err
	}
	if renew {
		rowCounts.forgetTable(t.db.st, t.db.branch, t.def.ID)
	}
	*t = *updated
	return nil
}

// DropIndex は sql.IndexAlterableTable の実装。主キーのない表の保存のキーが変わる（並べる一意インデックスを消す）ときは、
// 行のある表を書き直す（docs/plan.md の段階 72）。
func (t *Table) DropIndex(ctx *sql.Context, name string) error {
	i := t.def.FindIndex(name)
	if i < 0 {
		return sql.ErrIndexNotFound.New(name)
	}
	settled := t.def.Clone()
	settled.Indexes = slices.Delete(settled.Indexes, i, i+1)
	settled.SettleKey()
	renew := !settled.SameKey(t.def)
	if renew {
		empty, err := t.empty(ctx)
		if err != nil {
			return err
		}
		if !empty {
			return t.rewriteWithout(ctx, name)
		}
	}
	// 空の表の保存のキーが変わるときは、CreateIndex と同じく新しいテーブル ID にする。
	old := t.def.ID
	err := t.alter(ctx, func(tx *store.WriteTx, next *schema.Table) error {
		i := next.FindIndex(name)
		if i < 0 {
			return sql.ErrIndexNotFound.New(name)
		}
		next.Indexes = append(next.Indexes[:i], next.Indexes[i+1:]...)
		next.SettleKey()
		if renew {
			var err error
			next.ID, err = tx.NextTableID()
			return err
		}
		return nil
	})
	if err == nil && renew {
		rowCounts.forgetTable(t.db.st, t.db.branch, old)
	}
	return err
}

// empty は、表に行がないか。一時テーブルの DDL はトランザクションをコミットしないので、セッションのトランザクションの
// 行も見る。
func (t *Table) empty(ctx *sql.Context) (bool, error) {
	txn := t.db.st.Begin()
	defer txn.Rollback()
	scan := txn
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil && t.db.temporary {
		scan = s.txn
	}
	empty := true
	err := scan.ScanRowsLatest(t.db.branch, t.def.ID, nil, nil, false, func(store.RowVersion) (bool, error) {
		empty = false
		return false, nil
	})
	return empty, err
}

// rewriteWithout は、インデックス name を消して表を書き直す（主キーのない表を並べる一意インデックスを消すとき）。
func (t *Table) rewriteWithout(ctx *sql.Context, name string) error {
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil {
		if err := s.beginDDL(t.db.branch); err != nil {
			return err
		}
	}
	r, err := t.startRewrite(ctx, t.esch, nil, nil, name)
	if err != nil {
		return err
	}
	t.rewriting = r
	r.scan = t.newRewriteScan()
	for {
		row, err := r.scan.Next(ctx)
		if err == io.EOF {
			break
		}
		if err == nil {
			err = r.Insert(ctx, t.visibleRow(row))
		}
		if err != nil {
			_ = r.DiscardChanges(ctx, err)
			return err
		}
	}
	return r.Close(ctx)
}

// RenameIndex は sql.IndexAlterableTable の実装。
func (t *Table) RenameIndex(ctx *sql.Context, from, to string) error {
	return t.alter(ctx, func(_ *store.WriteTx, next *schema.Table) error {
		i := next.FindIndex(from)
		if i < 0 {
			return sql.ErrIndexNotFound.New(from)
		}
		if j := next.FindIndex(to); j >= 0 && j != i {
			return sql.ErrDuplicateKey.New(to)
		}
		next.Indexes[i].Name = to
		return nil
	})
}

// alter はテーブル定義の新しいバージョンを書き、成功したらこのテーブルにも反映する。
// go-mysql-server は CREATE TABLE のインデックスを同じ Table に対して続けて作るため、反映が必要。
func (t *Table) alter(ctx *sql.Context, fn func(tx *store.WriteTx, next *schema.Table) error) error {
	var next *schema.Table
	err := t.db.ddl(ctx, t.def.Name, func(tx *store.WriteTx) error {
		cur, ok, err := t.db.st.GetTable(t.db.branch, t.def.Name, store.Latest)
		if err != nil {
			return err
		}
		if !ok || cur.ID != t.def.ID {
			return sql.ErrTableNotFound.New(t.def.Name)
		}
		if err := fn(tx, cur); err != nil {
			return err
		}
		next = cur
		next.Version = tx.TS
		return tx.PutTable(t.db.branch, next)
	})
	if err != nil {
		return err
	}
	updated, err := newTable(ctx, t.db, next)
	if err != nil {
		return err
	}
	*t = *updated
	return nil
}

// Index は HeimDB のインデックスを go-mysql-server に見せる。キーにできない型を含むインデックスは、
// 全件走査と並べ替えで模擬する（rows.go の lookupRows）。
type Index struct {
	db, table string
	name      string
	columns   []schema.IndexColumn
	types     []sql.Type
	unique    bool
	comment   string
	// fulltext は FULLTEXT インデックス（定義だけ）。go-mysql-server には IsFullText で false を返す（fulltext.go）。
	fulltext bool
}

var _ sql.OrderedIndex = (*Index)(nil)

func (t *Table) newIndex(name string, cols []schema.IndexColumn, unique bool, comment string) *Index {
	types := make([]sql.Type, len(cols))
	for i, c := range cols {
		types[i] = t.sch.Schema[t.def.ColumnIndex(c.Name)].Type
	}
	return &Index{db: t.db.name, table: t.def.Name, name: name, columns: cols, types: types, unique: unique, comment: comment}
}

// ID は sql.Index の実装。
func (i *Index) ID() string { return i.name }

// Database は sql.Index の実装。
func (i *Index) Database() string { return i.db }

// Table は sql.Index の実装。
func (i *Index) Table() string { return i.table }

// Expressions は sql.Index の実装。memory パッケージと同じく「テーブル名.列名」の形式で返す。
func (i *Index) Expressions() []string {
	exprs := make([]string, len(i.columns))
	for j, c := range i.columns {
		exprs[j] = i.table + "." + c.Name
	}
	return exprs
}

// IsUnique は sql.Index の実装。
func (i *Index) IsUnique() bool { return i.unique }

// IsSpatial は sql.Index の実装。
func (i *Index) IsSpatial() bool { return false }

// IsFullText は sql.Index の実装。
func (i *Index) IsFullText() bool { return false }

// IsVector は sql.Index の実装。
func (i *Index) IsVector() bool { return false }

// Comment は sql.Index の実装。
func (i *Index) Comment() string { return i.comment }

// IndexType は sql.Index の実装。
func (i *Index) IndexType() string {
	if i.fulltext {
		return "FULLTEXT"
	}
	return "BTREE"
}

// IsGenerated は sql.Index の実装。
func (i *Index) IsGenerated() bool { return false }

// ColumnExpressionTypes は sql.Index の実装。
func (i *Index) ColumnExpressionTypes() []sql.ColumnExpressionType {
	exprs := i.Expressions()
	cets := make([]sql.ColumnExpressionType, len(exprs))
	for j, e := range exprs {
		cets[j] = sql.ColumnExpressionType{Expression: e, Type: i.types[j]}
	}
	return cets
}

// CanSupport は sql.Index の実装。どの範囲にも対応する（変換できない条件は行を読んだ後で絞る）。FULLTEXT は
// エントリを持たないが、go-mysql-server の結合の計画は CanSupport を見ずにインデックスを使うので、ここでも false に
// しない。検索はキーを持たないインデックスと同じく全件の走査になる（Table.lookupRows）。
func (i *Index) CanSupport(*sql.Context, ...sql.Range) bool { return true }

// CanSupportOrderBy は sql.Index の実装。
func (i *Index) CanSupportOrderBy(sql.Expression) bool { return false }

// PrefixLengths は sql.Index の実装。どの列にもプレフィックス長がなければ空にする（go-mysql-server の
// information_schema.STATISTICS は、ここで返した 0 を SUB_PART にそのまま出すため。MySQL は NULL）。
func (i *Index) PrefixLengths() []uint16 {
	lengths := make([]uint16, len(i.columns))
	any := false
	for j, c := range i.columns {
		lengths[j] = uint16(c.Length)
		any = any || c.Length > 0
	}
	if !any {
		return nil
	}
	return lengths
}

// Order は sql.OrderedIndex の実装。インデックスによる検索は、インデックスの列と主キーの昇順に行を返す。
func (i *Index) Order() sql.IndexOrder { return sql.IndexOrderAsc }

// Reversible は sql.OrderedIndex の実装。
func (i *Index) Reversible() bool { return true }

func (i *Index) String() string {
	return fmt.Sprintf("%s(%s)", i.name, strings.Join(i.Expressions(), ", "))
}
