// Package schema は、テーブル定義を Pebble に保存するための形式と、go-mysql-server のスキーマとの相互変換を提供する。
//
// 型は go-mysql-server の文字列表現で保存し、読み込み時に構文解析し直す。
// DEFAULT 式と ON UPDATE 式は文字列のまま保存し、未解決の式として返す（解決はエンジンの解析器が行う）。
// どちらも memory パッケージ（memory/table.go の NewPartitionedTableWithCollation）と同じ扱い。
package schema

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/planbuilder"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
)

// Table はテーブル定義の保存形式。
type Table struct {
	// ID は行とインデックスのキーに使う。名前を変えても変わらない。
	ID   uint64 `json:"id"`
	Name string `json:"name"`
	// Columns は列の定義。順序は SELECT * の列順。
	Columns []Column `json:"columns"`
	// PKOrdinals は主キーを構成する列の Columns 内の位置（主キーでの順）。主キーのない表（Keyless）では、保存のキーの列
	// （SettleKey）。
	PKOrdinals []int `json:"pk_ordinals"`
	// Keyless は主キーのない表（docs/plan.md の段階 72）。エンジンには主キーのない表として見せ（EngineSchema）、保存の
	// キーは、一意キーで並べる表ならその一意インデックスの列、行 ID で並べる表なら見えない列 RowIDColumn にする（SettleKey）。
	Keyless bool    `json:"keyless,omitempty"`
	Indexes []Index `json:"indexes,omitempty"`
	// NextIndexID は次に払い出すインデックス ID。インデックス ID は再利用しない。
	NextIndexID uint64 `json:"next_index_id"`
	Collation   string `json:"collation"`
	Comment     string `json:"comment,omitempty"`

	// Version はこの定義のバージョン（書き込んだコミット時刻）。保存はせず、読み込み時にキーから入れる。
	Version uint64 `json:"-"`
}

// Column は列の定義。
type Column struct {
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Default       *string `json:"default,omitempty"`
	OnUpdate      *string `json:"on_update,omitempty"`
	Nullable      bool    `json:"nullable"`
	AutoIncrement bool    `json:"auto_increment,omitempty"`
	PrimaryKey    bool    `json:"primary_key,omitempty"`
	Comment       string  `json:"comment,omitempty"`
	Extra         string  `json:"extra,omitempty"`
	// Hidden は、主キーのない表の見えない列（RowIDColumn、RowHashColumn）。列の末尾に置く。
	Hidden bool `json:"hidden,omitempty"`
}

// Index はセカンダリインデックスの定義。主キーは含めない。
type Index struct {
	ID      uint64        `json:"id"`
	Name    string        `json:"name"`
	Columns []IndexColumn `json:"columns"`
	Unique  bool          `json:"unique,omitempty"`
	Comment string        `json:"comment,omitempty"`
	// Fulltext は FULLTEXT インデックス。定義だけを持ち、エントリは作らない（docs/plan.md の段階 10）。
	Fulltext bool `json:"fulltext,omitempty"`
	// Hidden は、主キーのない表の RowHashColumn の見えないインデックス。
	Hidden bool `json:"hidden,omitempty"`
	// KeyLevel は、エントリに入れられる型の範囲を示すキーの水準（rowenc.KeyLevel。docs/plan.md の「キー設計」と段階 117）。
	// 0（段階 117 より前に作った定義）は整数、日時、文字列、バイト列だけで、DECIMAL などの列を含めばエントリを持たない。
	// 新しく作るインデックスには今の水準を付ける（AddIndex）。定義の等しさ（復元、取り込み、マージ）には含めない。
	KeyLevel int `json:"key_level,omitempty"`
}

// IndexColumn はインデックスの列。Length はプレフィックス長で、0 なら指定なし。
type IndexColumn struct {
	Name   string `json:"name"`
	Length int64  `json:"length,omitempty"`
}

// MapToUTF8MB4 は、utf8mb3（別名 utf8）と ascii の照合順序 c を、名前の文字コードの部分だけを utf8mb4 にした照合順序
// にする（docs/plan.md の段階 37、57。utf8mb3_general_ci と ascii_general_ci は utf8mb4_general_ci、utf8mb3_bin と
// ascii_bin は utf8mb4_bin）。utf8mb4 は utf8mb3 と ascii を含み、その範囲の文字の重みは同じなので、比較の意味は
// MySQL の元の文字コードと変わらない（ascii は MySQL 8.0 で 128 文字の並びと等しさを確かめた）。どちらでもなければ c を
// そのまま返す（mapped が偽）。対応する utf8mb4 の照合順序がなければエラー。
func MapToUTF8MB4(c sql.CollationID) (mapped sql.CollationID, ok bool, err error) {
	if c == sql.Collation_Unspecified {
		return c, false, nil
	}
	cs := c.CharacterSet()
	if cs != sql.CharacterSet_utf8mb3 && cs != sql.CharacterSet_ascii {
		return c, false, nil
	}
	name := "utf8mb4" + strings.TrimPrefix(c.Name(), cs.Name())
	m, err := sql.ParseCollation("", name, false)
	if err != nil || m.CharacterSet() != sql.CharacterSet_utf8mb4 {
		return c, false, fmt.Errorf("HeimDB: collation %s has no utf8mb4 counterpart; only utf8mb4 is supported", c.Name())
	}
	return m, true, nil
}

// FromSQL は go-mysql-server のスキーマからテーブル定義を作る。インデックスは含まない。utf8mb3 と ascii の照合順序は
// utf8mb4 にする（MapToUTF8MB4）。
func FromSQL(id uint64, name string, sch sql.PrimaryKeySchema, collation sql.CollationID, comment string) (*Table, error) {
	collation, _, err := MapToUTF8MB4(collation)
	if err != nil {
		return nil, err
	}
	t := &Table{
		ID:          id,
		Name:        name,
		PKOrdinals:  append([]int{}, sch.PkOrdinals...),
		Keyless:     len(sch.PkOrdinals) == 0,
		NextIndexID: 1,
		Collation:   collation.Name(),
		Comment:     comment,
	}
	for _, c := range sch.Schema {
		col, err := ColumnFromSQL(c)
		if err != nil {
			return nil, err
		}
		t.Columns = append(t.Columns, col)
	}
	return t, nil
}

// ColumnFromSQL は go-mysql-server の列の定義を保存形式にする。utf8mb3 と ascii の照合順序は utf8mb4 にする
// （MapToUTF8MB4）。
func ColumnFromSQL(c *sql.Column) (Column, error) {
	if c.Generated != nil || c.Virtual {
		return Column{}, fmt.Errorf("HeimDB: generated column %q is not supported", c.Name)
	}
	typ := c.Type
	if tc, ok := typ.(sql.TypeWithCollation); ok {
		m, mapped, err := MapToUTF8MB4(tc.Collation())
		if err != nil {
			return Column{}, err
		}
		if mapped {
			if typ, err = tc.WithNewCollation(m); err != nil {
				return Column{}, err
			}
		}
	}
	// 保存した型の文字列は、テーブルを開くたびに読み直す。読み直せない定義や、読み直すと値の変わる ENUM と SET を
	// 保存すると、表を開けなくなる（DROP もできない）ので、ここで拒否する（段階 111）。
	ts := typ.String()
	rt, err := parseType(ts)
	if err != nil {
		return Column{}, fmt.Errorf("HeimDB: column %q: type %s cannot be stored: %w", c.Name, ts, err)
	}
	if !sameEnumValues(typ, rt) {
		return Column{}, fmt.Errorf("HeimDB: column %q: the values of type %s cannot be stored", c.Name, ts)
	}
	return Column{
		Name:          c.Name,
		Type:          ts,
		Default:       fromDefault(c.Default),
		OnUpdate:      fromDefault(c.OnUpdate),
		Nullable:      c.Nullable,
		AutoIncrement: c.AutoIncrement,
		PrimaryKey:    c.PrimaryKey,
		Comment:       c.Comment,
		Extra:         c.Extra,
	}, nil
}

// sameEnumValues は、ENUM と SET の型 a と、その文字列を読み直した b の値が同じか（ほかの型は真）。
func sameEnumValues(a, b sql.Type) bool {
	switch at := a.(type) {
	case sql.EnumType:
		bt, ok := b.(sql.EnumType)
		return ok && slices.Equal(at.Values(), bt.Values())
	case sql.SetType:
		bt, ok := b.(sql.SetType)
		return ok && slices.Equal(at.Values(), bt.Values())
	}
	return true
}

func fromDefault(d *sql.ColumnDefaultValue) *string {
	if d == nil {
		return nil
	}
	// 式の中の列参照からテーブル名を取り除く。名前を変えたテーブルでも同じ式として読めるように。
	stripped, _, _ := transform.Expr(d, func(e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
		if gf, ok := e.(*expression.GetField); ok {
			ne := expression.NewGetField(gf.Index(), gf.Type(), gf.Name(), gf.IsNullable())
			return ne.WithQuotedNames(sql.GlobalSchemaFormatter, gf.IsQuotedIdentifier()), transform.NewTree, nil
		}
		return e, transform.SameTree, nil
	})
	s := stripped.String()
	return &s
}

// 主キーのない表の見えない列（docs/plan.md の段階 72）。
const (
	// RowIDColumn は、行 ID で並べる表の保存のキー。AUTO_INCREMENT の列として採番する。
	RowIDColumn = "__heimdb_row_id"
	// RowHashColumn は、見える列の値のハッシュ（RowHash）。同じ名前の見えないインデックスで、値から行 ID を引く。
	RowHashColumn = "__heimdb_row_hash"
)

// HiddenPrefix は、見えない列の名前の先頭。この名前で始まる列は作れない。
const HiddenPrefix = "__heimdb_row_"

// Visible は見える列の数（見えない列は末尾にある）。
func (t *Table) Visible() int {
	n := len(t.Columns)
	for n > 0 && t.Columns[n-1].Hidden {
		n--
	}
	return n
}

// RowIDOrdinal は RowIDColumn の位置。行 ID で並べる表でなければ -1。
func (t *Table) RowIDOrdinal() int {
	if !t.Keyless {
		return -1
	}
	return t.ColumnIndex(RowIDColumn)
}

// SameKey は、t と u の保存のキーが同じか（主キーのない表のキーを決め直したときに、表を書き直す必要があるか）。
func (t *Table) SameKey(u *Table) bool {
	return slices.Equal(t.PKOrdinals, u.PKOrdinals)
}

// SettleKey は、主キーのない表の保存のキーを、今のインデックスで決め直す（docs/plan.md の段階 72）。MySQL の InnoDB と
// 同じく、すべての列が NOT NULL で前方の長さ付きの列を含まない一意インデックスのうち最初のもので並べる（その列が
// 主キーにできる型のときだけ）。なければ見えない列とインデックスを足して、行 ID で並べる。主キーのある表は何もしない。
func (t *Table) SettleKey() {
	if !t.Keyless {
		return
	}
	for _, idx := range t.Indexes {
		if keyCandidate(t, idx) {
			t.PKOrdinals = t.PKOrdinals[:0]
			for _, c := range idx.Columns {
				t.PKOrdinals = append(t.PKOrdinals, t.ColumnIndex(c.Name))
			}
			t.Columns = t.Columns[:t.Visible()]
			t.Indexes = slices.DeleteFunc(t.Indexes, func(idx Index) bool { return idx.Hidden })
			return
		}
	}
	if t.ColumnIndex(RowIDColumn) < 0 {
		t.Columns = append(t.Columns,
			Column{Name: RowIDColumn, Type: "bigint unsigned", AutoIncrement: true, Hidden: true},
			Column{Name: RowHashColumn, Type: "varbinary(16)", Hidden: true})
	}
	if t.FindIndex(RowHashColumn) < 0 {
		t.Indexes = append(t.Indexes, Index{ID: t.NextIndexID, Name: RowHashColumn, Columns: []IndexColumn{{Name: RowHashColumn}}, Hidden: true, KeyLevel: rowenc.KeyLevel})
		t.NextIndexID++
	}
	t.PKOrdinals = []int{t.ColumnIndex(RowIDColumn)}
}

// keyCandidate は、idx が主キーのない表を並べる一意インデックスになれるか。
func keyCandidate(t *Table, idx Index) bool {
	if !idx.Unique || idx.Hidden || idx.Fulltext {
		return false
	}
	for _, c := range idx.Columns {
		ord := t.ColumnIndex(c.Name)
		if ord < 0 || c.Length > 0 || t.Columns[ord].Nullable {
			return false
		}
		typ, err := t.Columns[ord].SQLType()
		if err != nil || !rowenc.PKType(typ) {
			return false
		}
	}
	return true
}

// RowHash は、行 ID で並べる表の行の見える列の値 vals のハッシュ（RowHashColumn の値）。値の符号化の SHA-256 の先頭
// 16 バイト。エンジンが行を消すときに渡す値は保存した行を復号したもので、書くときの値（列の型に変換したもの）と同じ
// 符号になる（TestKeylessAllTypes）。
func RowHash(vals []any) ([]byte, error) {
	b, err := rowenc.EncodeValues(vals)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return h[:16], nil
}

// EngineSchema は、エンジンに見せるスキーマ。主キーのない表では見えない列を除き、主キーをなしにする。
func (t *Table) EngineSchema(dbName string) (sql.PrimaryKeySchema, error) {
	full, err := t.Schema(dbName)
	if err != nil || !t.Keyless {
		return full, err
	}
	return sql.NewPrimaryKeySchema(full.Schema[:t.Visible()]), nil
}

// Schema はテーブル定義から go-mysql-server のスキーマを組み立てる。主キーのない表では、見えない列と保存のキーを含む
// （HeimDB の中で使う。エンジンには EngineSchema）。
// 列の Source にはテーブル名、DatabaseSource にはデータベース名を入れる。
func (t *Table) Schema(dbName string) (sql.PrimaryKeySchema, error) {
	cols := make(sql.Schema, len(t.Columns))
	for i, c := range t.Columns {
		typ, err := parseType(c.Type)
		if err != nil {
			return sql.PrimaryKeySchema{}, fmt.Errorf("HeimDB: column %s.%s: parse type %q: %w", t.Name, c.Name, c.Type, err)
		}
		cols[i] = &sql.Column{
			Name:           c.Name,
			Type:           typ,
			Default:        toDefault(c.Default),
			OnUpdate:       toDefault(c.OnUpdate),
			Nullable:       c.Nullable,
			AutoIncrement:  c.AutoIncrement,
			PrimaryKey:     c.PrimaryKey,
			Comment:        c.Comment,
			Extra:          c.Extra,
			Source:         t.Name,
			DatabaseSource: dbName,
		}
	}
	return sql.NewPrimaryKeySchema(cols, t.PKOrdinals...), nil
}

func toDefault(s *string) *sql.ColumnDefaultValue {
	if s == nil {
		return nil
	}
	return sql.NewUnresolvedColumnDefaultValue(*s)
}

// CollationID は保存した照合順序を go-mysql-server の ID に戻す。
func (t *Table) CollationID() (sql.CollationID, error) {
	if t.Collation == "" {
		return sql.Collation_Unspecified, nil
	}
	return sql.ParseCollation("", t.Collation, false)
}

// ColumnIndex は列名（大文字と小文字を区別しない）から Columns 内の位置を返す。なければ -1。
func (t *Table) ColumnIndex(name string) int {
	for i, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// FindIndex はインデックス名（大文字と小文字を区別しない）から Indexes 内の位置を返す。なければ -1。
func (t *Table) FindIndex(name string) int {
	for i, idx := range t.Indexes {
		if strings.EqualFold(idx.Name, name) {
			return i
		}
	}
	return -1
}

// AddIndex は go-mysql-server のインデックス定義を追加し、新しいインデックス ID を払い出す。
// 主キー、SPATIAL、VECTOR は扱わない。FULLTEXT は定義だけを持つ（プレフィックス長は持たない）。
// 今のキーの水準（rowenc.KeyLevel）を付ける（段階 117）ので、呼び出し側は今の行からエントリを作る。
func (t *Table) AddIndex(def sql.IndexDef) error {
	switch def.Constraint {
	case sql.IndexConstraint_None, sql.IndexConstraint_Unique, sql.IndexConstraint_Fulltext:
	default:
		return fmt.Errorf("HeimDB: index %q: unsupported index type", def.Name)
	}
	if t.FindIndex(def.Name) >= 0 {
		return sql.ErrDuplicateKey.New(def.Name)
	}
	idx := Index{ID: t.NextIndexID, Name: def.Name, Unique: def.IsUnique(), Comment: def.Comment, Fulltext: def.IsFullText(), KeyLevel: rowenc.KeyLevel}
	for _, c := range def.Columns {
		if t.ColumnIndex(c.Name) < 0 {
			return sql.ErrKeyColumnDoesNotExist.New(c.Name)
		}
		length := c.Length
		if idx.Fulltext {
			length = 0
		}
		idx.Columns = append(idx.Columns, IndexColumn{Name: c.Name, Length: length})
	}
	t.NextIndexID++
	t.Indexes = append(t.Indexes, idx)
	return nil
}

// Clone は深いコピーを返す。文ごとに読む定義のキャッシュ（store の decodeTableValue）からも呼ぶので、JSON を通さずに
// 写す（docs/plan.md の段階 78）。フィールドを足したら、ここで写す（TestCloneDeep が確かめる）。
func (t *Table) Clone() *Table {
	c := *t
	c.Version = 0 // 保存しない値なので、JSON で写していたときと同じく写さない
	c.Columns = slices.Clone(t.Columns)
	for i := range c.Columns {
		c.Columns[i].Default = clonePtr(c.Columns[i].Default)
		c.Columns[i].OnUpdate = clonePtr(c.Columns[i].OnUpdate)
	}
	c.PKOrdinals = slices.Clone(t.PKOrdinals)
	c.Indexes = slices.Clone(t.Indexes)
	for i := range c.Indexes {
		c.Indexes[i].Columns = slices.Clone(c.Indexes[i].Columns)
	}
	return &c
}

func clonePtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Marshal は保存用のバイト列を返す。
func (t *Table) Marshal() ([]byte, error) {
	return json.Marshal(t)
}

// Unmarshal は Marshal で作ったバイト列を読む。
func Unmarshal(b []byte) (*Table, error) {
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("HeimDB: corrupt table definition: %w", err)
	}
	return &t, nil
}

// typeCache は、型の文字列から go-mysql-server の型への対応のキャッシュ。型の構文解析はクエリのたびに全列で
// 走るので重い（主キーで 1 行を読む SELECT の 3 分の 1 ほど）。go-mysql-server の型の値は変更されないので共有してよい。
// 列（sql.Column）はエンジンが書き換えることがあるので、キャッシュせず毎回作る。
var typeCache sync.Map // map[string]sql.Type

// SQLType は、列の型の文字列を型にする。
func (c *Column) SQLType() (sql.Type, error) {
	return parseType(c.Type)
}

func parseType(s string) (sql.Type, error) {
	if t, ok := typeCache.Load(s); ok {
		return t.(sql.Type), nil
	}
	t, err := planbuilder.ParseColumnTypeString(s)
	if err != nil {
		return nil, err
	}
	// go-mysql-server は、照合順序が既定（utf8mb4_0900_bin）の文字列型の文字列に COLLATE を書かない。読み直すと照合順序が
	// 「なし」の型になり、LIKE が常に偽になる（照合順序を指定せずに作ったテーブル）ので、既定の照合順序に戻す。
	if tc, ok := t.(sql.TypeWithCollation); ok && tc.Collation() == sql.Collation_Unspecified {
		if t, err = tc.WithNewCollation(sql.Collation_Default); err != nil {
			return nil, err
		}
	}
	typeCache.Store(s, t)
	return t, nil
}
