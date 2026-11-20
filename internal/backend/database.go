package backend

import (
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// Database は 1 つのブランチを go-mysql-server のデータベースとして見せる。
type Database struct {
	name   string
	st     *store.Store
	branch uint64
	// temporary は、セッションの一時テーブルを置く隠しブランチのデータベースかどうか。
	temporary bool
	// ddlOp と ddlWait は、接続のない裏の処理が DDL のロックを取るときの持ち主の名前と待つ時間（起動の後の古い
	// インデックスの作り直し。autorebuild.go。docs/plan.md の段階 121）。空なら "ddl" と LockTimeout。
	ddlOp   string
	ddlWait time.Duration
}

var (
	_ sql.Database              = (*Database)(nil)
	_ sql.TableCreator          = (*Database)(nil)
	_ sql.TableDropper          = (*Database)(nil)
	_ sql.TableRenamer          = (*Database)(nil)
	_ sql.SchemaValidator       = (*Database)(nil)
	_ sql.TemporaryTableCreator = (*Database)(nil)
)

// Name は sql.Database の実装。
func (d *Database) Name() string {
	return d.name
}

// GetTableInsensitive は sql.Database の実装。セッションに同名の一時テーブルがあれば、そちらを返す（MySQL と同じ）。
// テーブル定義はトランザクションのスナップショットの時点で解決する。他の接続が列を変えたり TRUNCATE したり
// しても（新しいテーブル ID になる）、開いているトランザクションは同じテーブルを読み続ける。
func (d *Database) GetTableInsensitive(ctx *sql.Context, name string) (sql.Table, bool, error) {
	if tmp := d.tempDB(ctx, false); tmp != nil {
		if t, ok, err := tmp.GetTableInsensitive(ctx, name); err != nil || ok {
			return t, ok, err
		}
	}
	def, ok, err := d.st.GetTable(d.branch, name, d.catalogTS(ctx))
	if err != nil || !ok {
		return nil, false, err
	}
	t, err := newTable(ctx, d, def)
	if err != nil {
		return nil, false, err
	}
	return t, true, nil
}

// GetTableNames は sql.Database の実装。
func (d *Database) GetTableNames(ctx *sql.Context) ([]string, error) {
	defs, err := d.st.ListTables(d.branch, d.catalogTS(ctx))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(defs))
	for i, def := range defs {
		names[i] = def.Name
	}
	return names, nil
}

// ValidateSchema は sql.SchemaValidator の実装。AUTO_INCREMENT の列があり主キーのないテーブル、見えない列の名前、
// キーにできない型の主キー（rowenc.PKType）、utf8mb4 以外の文字コードを拒否する。
func (d *Database) ValidateSchema(sch sql.Schema) error {
	if len(sch) == 0 || sch[0].Source == "" {
		// CREATE TABLE の構文解析で呼ばれるときは、列にテーブル名がない。エラーにテーブル名を出せるよう、createTable で
		// 名前を付けて確かめる。
		return nil
	}
	return validateSchema(sch[0].Source, sch)
}

// validateSchema は、テーブル table のスキーマが HeimDB で扱えるかを確かめる。
func validateSchema(table string, sch sql.Schema) error {
	hasPK, autoInc := false, false
	for _, c := range sch {
		if strings.HasPrefix(strings.ToLower(c.Name), schema.HiddenPrefix) {
			return fmt.Errorf("HeimDB: table %s: column names starting with %s are reserved", table, schema.HiddenPrefix)
		}
		autoInc = autoInc || c.AutoIncrement
		typ := c.Type
		if tc, ok := typ.(sql.TypeWithCollation); ok && !types.IsBinaryType(typ) {
			if err := checkCharset(table, tc.Collation()); err != nil {
				return err
			}
			// 主キーの型は、保存するときの utf8mb4 の照合順序で判定する（ascii の主キー。段階 57）。COLLATE を書かない
			// ALTER TABLE ... MODIFY の列は照合順序が「なし」で来る（あとでテーブルの照合順序になる）ので、既定の照合順序で
			// 判定する（段階 70）。
			if tc.Collation() == sql.Collation_Unspecified {
				if t, err := tc.WithNewCollation(sql.Collation_Default); err == nil {
					typ = t
				}
			} else if m, mapped, _ := schema.MapToUTF8MB4(tc.Collation()); mapped {
				if t, err := tc.WithNewCollation(m); err == nil {
					typ = t
				}
			}
		}
		if !c.PrimaryKey {
			continue
		}
		hasPK = true
		if !rowenc.PKType(typ) {
			return fmt.Errorf("HeimDB: table %s: primary key column %q has type %s; primary key columns must be integer, CHAR, VARCHAR, BINARY, VARBINARY, DATE, DATETIME, TIMESTAMP or ENUM", table, c.Name, c.Type)
		}
	}
	if !hasPK && autoInc {
		// 行 ID で並べる表は、行 ID を見えない AUTO_INCREMENT の列で採番する（docs/plan.md の段階 72）。MySQL でも
		// AUTO_INCREMENT の列はインデックスに含める必要があり、WordPress のプラグインは主キーにする。
		return fmt.Errorf("HeimDB: table %s has an AUTO_INCREMENT column but no primary key; make the column the primary key", table)
	}
	return nil
}

// checkCharset は、utf8mb4 以外の文字コードを明示的なエラーにする（docs/plan.md の段階 17）。go-mysql-server は
// latin1 や utf8mb3 の列に、その文字コードで表せない文字をそのまま入れてしまい、MySQL（? にして警告）と違う値になる。
// WordPress は 4.2 からテーブルを utf8mb4 に変換しているので、古いサイトは移す前に変換してもらう。utf8mb3 と ascii は、
// utf8mb4 の同じ名前の照合順序として保存する（段階 37、57。schema.MapToUTF8MB4）ので通す。
func checkCharset(table string, c sql.CollationID) error {
	if c == sql.Collation_Unspecified || c.CharacterSet() == sql.CharacterSet_utf8mb4 {
		return nil
	}
	if cs := c.CharacterSet(); cs == sql.CharacterSet_utf8mb3 || cs == sql.CharacterSet_ascii {
		if _, _, err := schema.MapToUTF8MB4(c); err != nil {
			return fmt.Errorf("HeimDB: table %s uses character set %s with collation %s, which has no utf8mb4 counterpart; use another collation (ALTER TABLE %s CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci)", table, cs.Name(), c.Name(), table)
		}
		return nil
	}
	return fmt.Errorf("HeimDB: table %s uses character set %s; only utf8mb4 is supported (convert it before moving the site: ALTER TABLE %s CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_520_ci)", table, c.CharacterSet().Name(), table)
}

// CreateTable は sql.TableCreator の実装。
//
// sql.IndexedTableCreator（主キーのプレフィックス長を受け取る）は実装しない。go-mysql-server は、それを実装した
// データベースには主キーのあるテーブルをいつも CreateIndexedTable で作らせ、テーブルのコメントを渡さない
// （sql/rowexec/ddl.go の buildCreateTable）。主キーは整数だけなので、プレフィックス長は要らない。
func (d *Database) CreateTable(ctx *sql.Context, name string, sch sql.PrimaryKeySchema, collation sql.CollationID, comment string) error {
	return d.createTable(ctx, name, sch, collation, comment)
}

func (d *Database) createTable(ctx *sql.Context, name string, sch sql.PrimaryKeySchema, collation sql.CollationID, comment string) error {
	if err := checkCharset(name, collation); err != nil {
		return err
	}
	if err := validateSchema(name, sch.Schema); err != nil {
		return err
	}
	err := d.ddl(ctx, name, func(tx *store.WriteTx) error {
		if _, ok, err := d.st.GetTable(d.branch, name, store.Latest); err != nil {
			return err
		} else if ok {
			return sql.ErrTableAlreadyExists.New(name)
		}
		id, err := tx.NextTableID()
		if err != nil {
			return err
		}
		def, err := schema.FromSQL(id, name, sch, collation, comment)
		if err != nil {
			return err
		}
		// 主キーのない表は、ここでは行 ID で並べる。一意インデックスはこの後で 1 つずつ作られ、空の表なので、作るときに
		// 決め直す（Table.CreateIndex）。
		def.SettleKey()
		return tx.PutTable(d.branch, def)
	})
	if err == nil {
		warnStoredAsUTF8MB4(ctx, name, sch.Schema, collation)
	}
	return err
}

// warnStoredAsUTF8MB4 は、テーブル table の定義（列 cols と既定の照合順序 collation）に utf8mb3 か ascii があれば、
// utf8mb4 として保存したことを、文字コードごとに警告とログに出す（docs/plan.md の段階 37、57）。黙って変えると気づけない
// ため。
func warnStoredAsUTF8MB4(ctx *sql.Context, table string, cols []*sql.Column, collation sql.CollationID) {
	var charsets []sql.CharacterSetID
	what := map[sql.CharacterSetID][]string{}
	add := func(c sql.CollationID, name string) {
		if _, mapped, _ := schema.MapToUTF8MB4(c); mapped {
			cs := c.CharacterSet()
			if _, ok := what[cs]; !ok {
				charsets = append(charsets, cs)
			}
			what[cs] = append(what[cs], name)
		}
	}
	add(collation, "the table default")
	for _, c := range cols {
		if tc, ok := c.Type.(sql.TypeWithCollation); ok {
			add(tc.Collation(), "column "+c.Name)
		}
	}
	for _, cs := range charsets {
		msg := fmt.Sprintf("HeimDB: table %s: character set %s is stored as utf8mb4 with the same collation name (%s)", table, cs.Name(), strings.Join(what[cs], ", "))
		ctx.Warn(3719, "%s", msg)
		logrus.Info(msg)
	}
}

// DropTable は sql.TableDropper の実装。行は消さず、テーブル定義に削除の印を書く。大きさの上限を超えたブランチでも通す。
// セッションに同名の一時テーブルがあれば、そちらを消す（MySQL と同じ）。
func (d *Database) DropTable(ctx *sql.Context, name string) error {
	if tmp := d.tempDB(ctx, false); tmp != nil {
		if _, ok, err := d.st.GetTable(tmp.branch, name, store.Latest); err != nil {
			return err
		} else if ok {
			return tmp.DropTable(ctx, name)
		}
	}
	return d.dropDDL(ctx, func(tx *store.WriteTx) error {
		def, ok, err := d.st.GetTable(d.branch, name, store.Latest)
		if err != nil {
			return err
		} else if !ok {
			return sql.ErrTableNotFound.New(name)
		}
		rowCounts.forgetTable(d.st, d.branch, def.ID)
		return tx.DeleteTable(d.branch, name)
	})
}

// RenameTable は sql.TableRenamer の実装。テーブル ID は変わらないので、行は書き直さない。
func (d *Database) RenameTable(ctx *sql.Context, oldName, newName string) error {
	return d.ddl(ctx, oldName, func(tx *store.WriteTx) error {
		def, ok, err := d.st.GetTable(d.branch, oldName, store.Latest)
		if err != nil {
			return err
		}
		if !ok {
			return sql.ErrTableNotFound.New(oldName)
		}
		if _, exists, err := d.st.GetTable(d.branch, newName, store.Latest); err != nil {
			return err
		} else if exists {
			return sql.ErrTableAlreadyExists.New(newName)
		}
		if err := tx.DeleteTable(d.branch, oldName); err != nil {
			return err
		}
		def.Name = newName
		return tx.PutTable(d.branch, def)
	})
}

// catalogTS は、テーブル定義を解決する時点を返す。トランザクションの中ではスナップショット、DDL の文の中
// （定義を変えた直後に読み直すことがある）とトランザクションの外では最新。
func (d *Database) catalogTS(ctx *sql.Context) uint64 {
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil && !s.txn.DDL && !d.temporary {
		return s.txn.ReadTS()
	}
	return store.Latest
}

// ddl は、ブランチの書き込みロックを持った状態で fn をコミットする。テーブル定義の変更や CREATE INDEX が、
// 書き込み中のトランザクションと重ならないようにする。セッションの中では、MySQL と同じく実行中のトランザクションを
// 先に暗黙にコミットする（Session.beginDDL）。
//
// 大きさの上限を超えたブランチでは、エラー 1114 で拒否する（limit.go。table はエラーに出す名前）。表を消す DDL
// （DROP TABLE、TRUNCATE）は、上限を確かめない dropDDL を使う。
func (d *Database) ddl(ctx *sql.Context, table string, fn func(tx *store.WriteTx) error) error {
	if err := checkBranchSize(d.st, d.branch, table); err != nil {
		return err
	}
	return d.dropDDL(ctx, fn)
}

// dropDDL は、大きさの上限を確かめない ddl。
//
// 一時テーブル（隠しブランチ）の作成と削除は、MySQL と同じくトランザクションをコミットしない。go-mysql-server も
// CREATE/DROP TEMPORARY TABLE の後にはコミットしない（sql/planbuilder/ddl.go の buildDDL）。隠しブランチのロックだけを取る。
func (d *Database) dropDDL(ctx *sql.Context, fn func(tx *store.WriteTx) error) error {
	release, err := d.lockDDL(ctx)
	if err != nil {
		return err
	}
	defer release()
	return d.st.Update(fn)
}

// lockDDL は、DDL のための書き込みのロックを取る。セッションのトランザクションがあれば、それにロックを持たせる（実行中の
// トランザクションを暗黙にコミットし、ロックはトランザクションの終わりまで持つ。release は何もしない）。なければロックを
// 取り、release で放す。何回かに分けてコミットする DDL（表の書き直し、インデックスの作成。段階 53）は、最初に取って
// 最後まで持つ。
func (d *Database) lockDDL(ctx *sql.Context) (release func(), err error) {
	if s, ok := ctx.Session.(*Session); ok && s.txn != nil {
		if d.temporary {
			err = s.txn.LockForWrite(d.branch)
		} else {
			err = s.beginDDL(d.branch)
		}
		return func() {}, err
	}
	if d.ddlOp != "" {
		return d.st.LockBranchFor(d.branch, d.ddlWait, store.LockOwner{Op: d.ddlOp}, ctx.Done())
	}
	var conn uint32
	if s, ok := ctx.Session.(*Session); ok {
		conn = s.ID()
	}
	return d.st.LockBranchFor(d.branch, d.st.LockTimeout, store.LockOwner{Conn: conn, Op: "ddl"}, ctx.Done())
}

// CreateTemporaryTable は sql.TemporaryTableCreator の実装。一時テーブルは、セッション専用の隠しブランチに普通のテーブル
// として作り、切断時にブランチごと消す（Session.SessionEnd）。トランザクションの扱いは普通のテーブルと同じ。
//
// 上限の付いたブランチを使う接続では拒否する（一時テーブルの隠しブランチには上限がなく、大きさの上限を外せるので。limit.go）。
func (d *Database) CreateTemporaryTable(ctx *sql.Context, name string, sch sql.PrimaryKeySchema, collation sql.CollationID) error {
	if s, ok := ctx.Session.(*Session); ok && s.limited {
		return errLimitedTemporaryTable
	}
	tmp := d.tempDB(ctx, true)
	if tmp == nil {
		return sql.ErrTemporaryTableNotSupported.New()
	}
	if _, ok, err := d.st.GetTable(tmp.branch, name, store.Latest); err != nil {
		return err
	} else if ok {
		return sql.ErrTableAlreadyExists.New(name)
	}
	return tmp.createTable(ctx, name, sch, collation, "")
}

// tempDB は、セッションの一時テーブルのデータベースを返す。create が false で、まだ一時テーブルがなければ nil。
func (d *Database) tempDB(ctx *sql.Context, create bool) *Database {
	if d.temporary {
		return nil
	}
	s, ok := ctx.Session.(*Session)
	if !ok {
		return nil
	}
	if s.tempBranch == 0 {
		if !create {
			return nil
		}
		id, err := d.st.NewHiddenBranch()
		if err != nil {
			return nil
		}
		s.tempBranch = id
	}
	return &Database{name: d.name, st: d.st, branch: s.tempBranch, temporary: true}
}

var _ sql.CollatedDatabase = (*Database)(nil)

// defaultCollation は、照合順序を指定せずに作るテーブルの照合順序。MySQL 8.0 の既定（collation_server）と同じ
// （docs/plan.md の段階 11）。go-mysql-server の既定は utf8mb4_0900_bin で、大文字と小文字を区別してしまう。
const defaultCollation = sql.Collation_utf8mb4_0900_ai_ci

// GetCollation は sql.CollatedDatabase の実装。go-mysql-server は、照合順序を指定しない CREATE TABLE にこれを使う
// （plan.GetDatabaseCollation）。
func (d *Database) GetCollation(*sql.Context) sql.CollationID { return defaultCollation }

// SetCollation は sql.CollatedDatabase の実装。データベースの照合順序の変更（ALTER DATABASE ... COLLATE）は扱わない。
func (d *Database) SetCollation(*sql.Context, sql.CollationID) error {
	return fmt.Errorf("HeimDB: changing the database collation is not supported")
}
