package backend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/rowexec"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/schema"
)

// FULLTEXT インデックス（docs/plan.md の段階 10）。
//
// go-mysql-server の FULLTEXT は、主キーが文字列の補助テーブルで実装されていて、整数の主キーだけに対応する HeimDB では
// 使えない。HeimDB は FULLTEXT の定義だけを持ち、エントリは作らない。
//   - 作成: go-mysql-server は FULLTEXT の作成を補助テーブルの経路に回すので、解析の最後に、定義の印（制約）を
//     ふつうのインデックスに変え、コメントの先頭に fulltextMarker を付けて Table.CreateIndex に届ける。
//   - go-mysql-server には Index.IsFullText で false を返す。true を返すと、DROP TABLE や列の削除で
//     fulltext.Database への型アサーションが走り、panic する（sql/rowexec/dml.go、ddl_iters.go）。
//   - 表示: SHOW INDEX と information_schema は IndexType の "FULLTEXT" を使う。SHOW CREATE TABLE は
//     showCreateTableFulltext で直す。検索には使わせない（Index.CanSupport）。
//   - MATCH ... AGAINST は、go-mysql-server が FULLTEXT インデックスを見つけられずにエラーにする。

// fulltextMarker は、FULLTEXT の定義であることを Table.CreateIndex に伝えるコメントの先頭の印。
const fulltextMarker = "\x00heimdb:fulltext\x00"

// fulltextIndexesId は HeimDB が足す解析ルールの ID。
const fulltextIndexesId analyzer.RuleId = 10003

// errBadFulltextColumn は MySQL のエラー 1283（ER_BAD_FT_COLUMN）。
func errBadFulltextColumn(col string) error {
	return mysql.NewSQLError(1283, mysql.SSUnknownSQLState, "Column '%s' cannot be part of FULLTEXT index", col)
}

// errFulltextOneAtATime は MySQL のエラー 1795（ER_INNODB_FT_LIMIT）。
var errFulltextOneAtATime = mysql.NewSQLError(1795, mysql.SSUnknownSQLState, "InnoDB presently supports one FULLTEXT index creation at a time")

// errFulltextTemporary は MySQL のエラー 1796（ER_INNODB_NO_FT_TEMP_TABLE）。
var errFulltextTemporary = mysql.NewSQLError(1796, mysql.SSUnknownSQLState, "Cannot create FULLTEXT index on temporary InnoDB table")

// fulltextIndexes は、CREATE TABLE と ALTER TABLE / CREATE INDEX の FULLTEXT の定義を、印の付いたふつうの
// インデックスの定義に変える。一時テーブルや文字列でない列の FULLTEXT は、テーブルを作る前にエラーにする（MySQL と
// 同じく何も作らない）。
func fulltextIndexes(_ *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		switch node := node.(type) {
		case *plan.CreateTable:
			defs := node.Indexes()
			changed := false
			out := make(sql.IndexDefs, len(defs))
			for i, d := range defs {
				c := *d
				if c.IsFullText() {
					if err := checkFulltextColumns(node.Temporary(), node.PkSchema().Schema, c.Columns, false); err != nil {
						return nil, transform.SameTree, err
					}
					c.Constraint, c.Comment, changed = sql.IndexConstraint_None, fulltextMarker+c.Comment, true
				}
				out[i] = &c
			}
			if !changed {
				return node, transform.SameTree, nil
			}
			nc, err := node.WithIndexDefs(out)
			return nc, transform.NewTree, err
		case *plan.Block:
			// ALTER TABLE ... ADD COLUMN x, ADD FULLTEXT (x): 同じ文で足す列も含めて、実行の前に確かめる（途中で失敗して
			// 列だけが残らないように。MySQL は文全体を取り消す）。
			var added sql.Schema
			for _, c := range node.Children() {
				if ac, ok := c.(*plan.AddColumn); ok {
					added = append(added, ac.Column())
				}
			}
			created := 0
			for _, c := range node.Children() {
				ai, ok := c.(*plan.AlterIndex)
				if !ok || ai.Action != plan.IndexAction_Create || !strings.HasPrefix(ai.Comment, fulltextMarker) {
					continue
				}
				// MySQL（InnoDB）は 1 つの ALTER TABLE で FULLTEXT インデックスを 1 つしか作れない。
				if created++; created > 1 {
					return nil, transform.SameTree, errFulltextOneAtATime
				}
				if t := heimdbTable(ai.Table); t != nil {
					if err := checkFulltextColumns(t.IsTemporary(), append(slices.Clone(t.sch.Schema), added...), ai.Columns, true); err != nil {
						return nil, transform.SameTree, err
					}
				}
			}
			return node, transform.SameTree, nil
		case *plan.AlterIndex:
			if node.Action != plan.IndexAction_Create || node.Constraint != sql.IndexConstraint_Fulltext {
				return node, transform.SameTree, nil
			}
			if t := heimdbTable(node.Table); t != nil {
				// 同じ ALTER で足す列はまだテーブルにないので、ここでは確かめない（作るときに Table.CreateIndex が確かめる）。
				if err := checkFulltextColumns(t.IsTemporary(), t.sch.Schema, node.Columns, true); err != nil {
					return nil, transform.SameTree, err
				}
			}
			nc := *node
			nc.Constraint, nc.Comment = sql.IndexConstraint_None, fulltextMarker+node.Comment
			return &nc, transform.NewTree, nil
		}
		return node, transform.SameTree, nil
	})
}

// checkFulltextColumns は、FULLTEXT インデックスを作れるか（一時テーブルでなく、列がすべて文字列）を確かめる。
// skipMissing なら、sch にない列は確かめない。
func checkFulltextColumns(temporary bool, sch sql.Schema, cols []sql.IndexColumn, skipMissing bool) error {
	if temporary {
		return errFulltextTemporary
	}
	for _, c := range cols {
		i := sch.IndexOfColName(c.Name)
		if i < 0 && skipMissing {
			continue
		}
		if i < 0 {
			return sql.ErrKeyColumnDoesNotExist.New(c.Name)
		}
		if !types.IsTextOnly(sch[i].Type) {
			return errBadFulltextColumn(sch[i].Name)
		}
	}
	return nil
}

// heimdbTable は、テーブルのノードが HeimDB のテーブルなら、それを返す。
func heimdbTable(n sql.Node) *Table {
	rt, ok := n.(*plan.ResolvedTable)
	if !ok {
		return nil
	}
	t, _ := rt.UnderlyingTable().(*Table)
	return t
}

// fulltextDefinition は、CreateIndex に届いた定義が FULLTEXT の印を持てば、印を外して FULLTEXT の定義に戻す。
// 一時テーブルと文字列でない列には作れない（MySQL と同じエラー。ふつうは fulltextIndexes が先に確かめている）。
func (t *Table) fulltextDefinition(def sql.IndexDef) (sql.IndexDef, error) {
	comment, ok := strings.CutPrefix(def.Comment, fulltextMarker)
	if !ok {
		return def, nil
	}
	def.Comment, def.Constraint = comment, sql.IndexConstraint_Fulltext
	return def, checkFulltextColumns(t.IsTemporary(), t.sch.Schema, def.Columns, false)
}

// fulltextBeforeAnalysisId は HeimDB が足す解析ルールの ID。
const fulltextBeforeAnalysisId analyzer.RuleId = 10006

// fulltextBeforeAnalysis は、go-mysql-server の解析が FULLTEXT インデックスを（IsFullText が false なので）ふつうの
// インデックスとして扱って困る文を、解析の前に直す。
//   - ALTER TABLE ... MODIFY / CHANGE: 列を TEXT にするとき、その列を含むインデックスにプレフィックス長を求める検証
//     （ValidateModifyColumn）は FULLTEXT を除くが、HeimDB の FULLTEXT は除かれない。検証が見るインデックスから
//     FULLTEXT を隠したテーブルの写しに差し替える（実行は同じテーブル。インデックスの定義は HeimDB が持ち越す）。
//   - CREATE TABLE ... LIKE: go-mysql-server はコピー元のインデックスをプレフィックス長なしの、FULLTEXT でない定義に
//     する。コピー元が HeimDB のテーブルなら、保存している定義から作り直す。
func fulltextBeforeAnalysis(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		switch node := node.(type) {
		case *plan.ModifyColumn:
			rt, ok := node.Table.(*plan.ResolvedTable)
			if !ok {
				return node, transform.SameTree, nil
			}
			t, ok := rt.UnderlyingTable().(*Table)
			if !ok || t.hideFulltext || !slices.ContainsFunc(t.def.Indexes, func(i schema.Index) bool { return i.Fulltext }) {
				return node, transform.SameTree, nil
			}
			hidden := *t
			hidden.hideFulltext = true
			nrt, err := rt.WithTable(&hidden)
			if err != nil {
				return nil, transform.SameTree, err
			}
			nc := *node
			nc.Table = nrt.(sql.Node)
			return &nc, transform.NewTree, nil
		case *plan.CreateTable:
			src, err := createTableLikeSource(ctx, node)
			if err != nil || src == nil {
				return node, transform.SameTree, err
			}
			var defs sql.IndexDefs
			for _, d := range node.Indexes() {
				if d.IsPrimary() {
					defs = append(defs, d)
				}
			}
			for _, idx := range src.def.Indexes {
				d := &sql.IndexDef{Name: idx.Name, Comment: idx.Comment, Constraint: sql.IndexConstraint_None}
				switch {
				case idx.Fulltext:
					d.Constraint = sql.IndexConstraint_Fulltext
				case idx.Unique:
					d.Constraint = sql.IndexConstraint_Unique
				}
				for _, c := range idx.Columns {
					d.Columns = append(d.Columns, sql.IndexColumn{Name: c.Name, Length: c.Length})
				}
				defs = append(defs, d)
			}
			nc, err := node.WithIndexDefs(defs)
			return nc, transform.NewTree, err
		}
		return node, transform.SameTree, nil
	})
}

// createTableLikeSource は、CREATE TABLE ... LIKE のコピー元が HeimDB のテーブルなら、それを返す。go-mysql-server の
// CreateTable はコピー元を持たないので、文を構文解析し直して、コピー先と同じデータベースから探す。
func createTableLikeSource(ctx *sql.Context, node *plan.CreateTable) (*Table, error) {
	if node.Like() != nil {
		return heimdbTable(node.Like()), nil
	}
	if !strings.Contains(strings.ToUpper(ctx.Query()), "LIKE") {
		return nil, nil
	}
	stmt, err := sqlparser.Parse(ctx.Query())
	if err != nil {
		return nil, nil
	}
	ddl, ok := stmt.(*sqlparser.DDL)
	if !ok || ddl.OptLike == nil || len(ddl.OptLike.LikeTables) != 1 || !strings.EqualFold(ddl.Table.Name.String(), node.Name()) {
		return nil, nil
	}
	like := ddl.OptLike.LikeTables[0]
	db := node.Database()
	if q := like.DbQualifier.String(); q != "" && !strings.EqualFold(q, db.Name()) {
		return nil, nil // 別のデータベースのテーブル（go-mysql-server の写しのまま）
	}
	t, ok, err := db.GetTableInsensitive(ctx, like.Name.String())
	if err != nil || !ok {
		return nil, err
	}
	src, _ := t.(*Table)
	return src, nil
}

// showCreateTableFulltextId は HeimDB が足す解析ルールの ID。
const showCreateTableFulltextId analyzer.RuleId = 10004

// showCreateTableFulltext は、HeimDB のテーブルの SHOW CREATE TABLE で go-mysql-server の表示を直す必要があるとき、
// showCreateTable に置き換える。
//   - FULLTEXT インデックス（go-mysql-server は IsFullText が false のインデックスを KEY と書く）
//   - ENUM と SET の列の既定値（go-mysql-server は既定値を列の型に変換してから書くので、内部の番号になる。
//     mysqldump の出力を MySQL に入れるとエラー 1067 になる。docs/plan.md の段階 17）
//   - 既定値のない NULL を許す列の DEFAULT NULL（go-mysql-server は書かない。段階 114）
func showCreateTableFulltext(_ *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		sc, ok := node.(*plan.ShowCreateTable)
		if !ok || sc.IsView || (len(fulltextNames(sc.Indexes)) == 0 && len(enumSetDefaults(sc.TargetSchema())) == 0 && len(defaultNullColumns(sc.TargetSchema())) == 0) {
			return node, transform.SameTree, nil
		}
		return &showCreateTable{ShowCreateTable: sc}, transform.NewTree, nil
	})
}

// enumSetDefaults は、既定値（リテラル）を持つ ENUM と SET の列を返す。
func enumSetDefaults(sch sql.Schema) []*sql.Column {
	var out []*sql.Column
	for _, col := range sch {
		if col.Default == nil || !col.Default.IsLiteral() {
			continue
		}
		if types.IsEnum(col.Type) || types.IsSet(col.Type) {
			out = append(out, col)
		}
	}
	return out
}

// fixEnumSetDefaults は、CREATE TABLE 文の ENUM と SET の列の既定値を、内部の番号から値の文字列に直す。
func fixEnumSetDefaults(ctx *sql.Context, stmt string, sch sql.Schema) string {
	for _, col := range enumSetDefaults(sch) {
		v, err := col.Default.Eval(ctx, nil)
		if err != nil || v == nil {
			continue
		}
		str, _, err := convertValue(ctx, col.Type, types.LongText, v)
		if err != nil {
			continue
		}
		prefix := "\n  " + sql.QuoteIdentifier(col.Name) + " "
		i := strings.Index(stmt, prefix)
		if i < 0 {
			continue
		}
		end := strings.Index(stmt[i+1:], "\n")
		if end < 0 {
			end = len(stmt)
		} else {
			end += i + 1
		}
		line := strings.Replace(stmt[i:end], fmt.Sprintf(" DEFAULT '%v'", v), " DEFAULT '"+strings.ReplaceAll(fmt.Sprint(str), "'", "''")+"'", 1)
		stmt = stmt[:i] + line + stmt[end:]
	}
	return stmt
}

// defaultNullColumns は、MySQL の SHOW CREATE TABLE が DEFAULT NULL を書く列（既定値のない NULL を許す列。TEXT、BLOB と、
// 生成列、AUTO_INCREMENT の列は書かない。GEOMETRY と JSON は書く）と、NULL を許す TIMESTAMP の列（MySQL は型の後に NULL を書く）を
// 返す。
func defaultNullColumns(sch sql.Schema) []*sql.Column {
	var out []*sql.Column
	for _, col := range sch {
		if col.Nullable && col.Type.Type() == sqltypes.Timestamp {
			out = append(out, col)
			continue
		}
		if col.Default != nil || col.Generated != nil || !col.Nullable || col.AutoIncrement {
			continue
		}
		switch col.Type.Type() {
		case sqltypes.Text, sqltypes.Blob:
			continue
		}
		out = append(out, col)
	}
	return out
}

// fixDefaultNull は、CREATE TABLE 文の defaultNullColumns の列に、MySQL と同じ位置（ON UPDATE と COMMENT の前）で
// DEFAULT NULL を書く（段階 111 で回した違い。上流の #3045 の行）。
func fixDefaultNull(stmt string, sch sql.Schema) string {
	for _, col := range defaultNullColumns(sch) {
		prefix := "\n  " + sql.QuoteIdentifier(col.Name) + " "
		i := strings.Index(stmt, prefix)
		if i < 0 {
			continue
		}
		end := strings.Index(stmt[i+1:], "\n")
		if end < 0 {
			end = len(stmt)
		} else {
			end += i + 1
		}
		line := stmt[i:end]
		if col.Type.Type() == sqltypes.Timestamp {
			// NULL を許す TIMESTAMP は、MySQL と同じく型の後に NULL を書く（`u` timestamp NULL DEFAULT ...）
			if rest := line[len(prefix):]; strings.HasPrefix(rest, "timestamp") {
				t := len(prefix) + strings.IndexAny(rest+" ", " ,")
				line = line[:t] + " NULL" + line[t:]
			}
			if col.Default != nil || col.Generated != nil {
				stmt = stmt[:i] + line + stmt[end:]
				continue
			}
		}
		at := len(line)
		if strings.HasSuffix(line, ",") {
			at--
		}
		if col.Comment != "" {
			if j := strings.LastIndex(line, " COMMENT '"); j >= 0 {
				at = j
			}
		}
		if col.OnUpdate != nil {
			if j := strings.Index(line, " ON UPDATE "); j >= 0 && j < at {
				at = j
			}
		}
		line = line[:at] + " DEFAULT NULL" + line[at:]
		stmt = stmt[:i] + line + stmt[end:]
	}
	return stmt
}

func fulltextNames(indexes []sql.Index) []string {
	var out []string
	for _, i := range indexes {
		if idx, ok := i.(*Index); ok && idx.fulltext {
			out = append(out, idx.name)
		}
	}
	return out
}

// showCreateTable は、FULLTEXT インデックスを FULLTEXT KEY と書き、ENUM と SET の既定値を値で書く SHOW CREATE TABLE。
type showCreateTable struct {
	*plan.ShowCreateTable
}

var _ sql.ExecSourceRel = (*showCreateTable)(nil)

// rewrap は、go-mysql-server の ShowCreateTable を返す With* の結果を包み直す（後の解析の段で作り直されても戻らないように）。
func rewrap(node sql.Node, err error) (sql.Node, error) {
	if err != nil {
		return nil, err
	}
	if sc, ok := node.(*plan.ShowCreateTable); ok {
		return &showCreateTable{ShowCreateTable: sc}, nil
	}
	return node, nil
}

func (n *showCreateTable) WithChildren(children ...sql.Node) (sql.Node, error) {
	return rewrap(n.ShowCreateTable.WithChildren(children...))
}

func (n *showCreateTable) WithExpressions(exprs ...sql.Expression) (sql.Node, error) {
	return rewrap(n.ShowCreateTable.WithExpressions(exprs...))
}

func (n *showCreateTable) WithTargetSchema(schema sql.Schema) (sql.Node, error) {
	return rewrap(n.ShowCreateTable.WithTargetSchema(schema))
}

func (n *showCreateTable) WithPrimaryKeySchema(schema sql.PrimaryKeySchema) (sql.Node, error) {
	return rewrap(n.ShowCreateTable.WithPrimaryKeySchema(schema))
}

func (n *showCreateTable) WithChecks(checks sql.CheckConstraints) sql.Node {
	node, _ := rewrap(n.ShowCreateTable.WithChecks(checks), nil)
	return node
}

func (n *showCreateTable) WithAsOf(asOf sql.Expression) (sql.Node, error) {
	return rewrap(n.ShowCreateTable.WithAsOf(asOf))
}

func (n *showCreateTable) String() string { return "heimdb." + n.ShowCreateTable.String() }

// RowIter は sql.ExecSourceRel の実装。go-mysql-server の結果の CREATE TABLE 文で、FULLTEXT インデックスの行の
// KEY を FULLTEXT KEY にし、ENUM と SET の既定値を値の文字列にする。
func (n *showCreateTable) RowIter(ctx *sql.Context, row sql.Row) (sql.RowIter, error) {
	iter, err := rowexec.DefaultBuilder.Build(ctx, n.ShowCreateTable, row)
	if err != nil {
		return nil, err
	}
	rows, err := sql.RowIterToRows(ctx, iter)
	if err != nil {
		return nil, err
	}
	names := fulltextNames(n.Indexes)
	for _, r := range rows {
		stmt, ok := r[1].(string)
		if !ok {
			continue
		}
		stmt = fixEnumSetDefaults(ctx, stmt, n.TargetSchema())
		stmt = fixDefaultNull(stmt, n.TargetSchema())
		for _, name := range names {
			key := "\n  KEY " + sql.QuoteIdentifier(name) + " ("
			stmt = strings.Replace(stmt, key, "\n  FULLTEXT KEY "+sql.QuoteIdentifier(name)+" (", 1)
		}
		r[1] = stmt
	}
	return &rowIter{rows: rows}, nil
}
