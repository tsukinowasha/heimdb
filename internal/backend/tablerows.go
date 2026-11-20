package backend

import (
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/information_schema"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// tableRowsId は HeimDB が足す解析ルールの ID。
const tableRowsId analyzer.RuleId = 10010

// tableRowsOnlyNamedId は、tableRowsOnlyNamed の解析ルールの ID。
const tableRowsOnlyNamedId analyzer.RuleId = 10017

// tablesTableRowsOrdinal は、information_schema.TABLES の TABLE_ROWS の列の位置。
const tablesTableRowsOrdinal = 7

// tableRows は、information_schema.TABLES の TABLE_ROWS に HeimDB のテーブルの行数を入れる。
//
// go-mysql-server は sql.StatisticsTable の RowCount を使うが、HeimDB の RowCount は実行計画のための見積もり
// （rowcount.go）で、権限も見ない。そこで information_schema.TABLES を読むときは、その行に数えた行数を入れ、読めない
// テーブルは 0 にする。WordPress はサイトヘルスで、この値を
// 使ってオブジェクトキャッシュを勧めるかを決める（いつも 0 だと勧めなかった）。数えるので大きなテーブルでは遅い。
func tableRows(_ *sql.Context, a *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		rt, ok := node.(*plan.ResolvedTable)
		if !ok {
			return node, transform.SameTree, nil
		}
		ist, ok := rt.Table.(*information_schema.InformationSchemaTable)
		if !ok || ist.Name() != information_schema.TablesTableName {
			return node, transform.SameTree, nil
		}
		nt, err := rt.ReplaceTable(&tablesWithRowCount{InformationSchemaTable: ist, cat: a.Catalog})
		if err != nil {
			return nil, transform.SameTree, err
		}
		return nt.(*plan.ResolvedTable), transform.NewTree, nil
	})
}

// tableRowsOnlyNamed は、information_schema.TABLES のすぐ上の Filter が、TABLE_SCHEMA と TABLE_NAME を定数の等号か
// IN で決めていれば、行数を数える表をそれに絞る（段階 86）。WordPress のサイトヘルスの
// `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ... AND TABLE_NAME IN (...) AND ENGINE = 'MyISAM'`
// が、すべてのブランチのすべての表を数えて、本番で 1.8 秒かかっていた。絞った外の行は Filter で落ちるので、
// TABLE_ROWS を数えなくても結果は変わらない。tableRows の後に動かす。
func tableRowsOnlyNamed(_ *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		f, ok := node.(*plan.Filter)
		if !ok {
			return node, transform.SameTree, nil
		}
		child := f.Child
		alias := ""
		if ta, ok := child.(*plan.TableAlias); ok {
			alias = ta.Name()
			child = ta.Child
		}
		rt, ok := child.(*plan.ResolvedTable)
		if !ok {
			return node, transform.SameTree, nil
		}
		t, ok := rt.Table.(*tablesWithRowCount)
		if !ok || t.schemas != nil || t.names != nil {
			return node, transform.SameTree, nil
		}
		name := information_schema.TablesTableName
		if alias != "" {
			name = alias
		}
		conds := expression.SplitConjunction(f.Expression)
		schemas, names := namedValues(conds, name, "table_schema"), namedValues(conds, name, "table_name")
		if schemas == nil && names == nil {
			return node, transform.SameTree, nil
		}
		nt, err := rt.ReplaceTable(&tablesWithRowCount{InformationSchemaTable: t.InformationSchemaTable, cat: t.cat, schemas: schemas, names: names})
		if err != nil {
			return nil, transform.SameTree, err
		}
		var newChild sql.Node = nt
		if alias != "" {
			if newChild, err = f.Child.WithChildren(nt); err != nil {
				return nil, transform.SameTree, err
			}
		}
		newFilter, err := f.WithChildren(newChild)
		return newFilter, transform.NewTree, err
	})
}

// namedValues は、条件 conds（AND）のうち、表 table の列 col を文字列の定数の等号か IN で決めるものの値の集合
// （小文字）。決めるものがなければ nil。
func namedValues(conds []sql.Expression, table, col string) map[string]bool {
	var out map[string]bool
	for _, c := range conds {
		var operands []sql.Expression
		switch c := c.(type) {
		case *expression.Equals:
			operands = []sql.Expression{c.Left(), c.Right()}
		case *expression.InTuple, *expression.HashInTuple:
			in := c.Children()
			t, ok := in[1].(expression.Tuple)
			if !ok {
				continue
			}
			operands = append([]sql.Expression{in[0]}, t...)
		default:
			continue
		}
		gf, ok := operands[0].(*expression.GetField)
		rest := operands[1:]
		if !ok && len(operands) == 2 {
			gf, ok = operands[1].(*expression.GetField)
			rest = operands[:1]
		}
		if !ok || !strings.EqualFold(gf.Table(), table) || !strings.EqualFold(gf.Name(), col) {
			continue
		}
		vals := map[string]bool{}
		for _, e := range rest {
			lit, ok := e.(*expression.Literal)
			if !ok {
				vals = nil
				break
			}
			s, ok := lit.Value().(string)
			if !ok {
				vals = nil
				break
			}
			vals[strings.ToLower(s)] = true
		}
		if vals == nil {
			continue
		}
		if out == nil {
			out = vals
			continue
		}
		// AND で 2 つあれば、両方に入る値だけ
		for v := range out {
			if !vals[v] {
				delete(out, v)
			}
		}
	}
	return out
}

// tablesWithRowCount は、行の TABLE_ROWS を数えた行数にする information_schema.TABLES。
type tablesWithRowCount struct {
	*information_schema.InformationSchemaTable
	cat sql.Catalog
	// schemas と names は、行数を数えるデータベースと表の名前（小文字。nil ならすべて）。tableRowsOnlyNamed。
	schemas, names map[string]bool
}

func (t *tablesWithRowCount) PartitionRows(ctx *sql.Context, p sql.Partition) (sql.RowIter, error) {
	iter, err := t.InformationSchemaTable.PartitionRows(ctx, p)
	if err != nil {
		return nil, err
	}
	return &tableRowsIter{RowIter: iter, cat: t.cat, schemas: t.schemas, names: t.names}, nil
}

// countedTables は、TABLE_ROWS のために行数を数えた表を知らせる（テスト用）。
var countedTables func(name string)

type tableRowsIter struct {
	sql.RowIter
	cat            sql.Catalog
	schemas, names map[string]bool
}

func (it *tableRowsIter) Next(ctx *sql.Context) (sql.Row, error) {
	row, err := it.RowIter.Next(ctx)
	if err != nil || len(row) <= tablesTableRowsOrdinal {
		return row, err
	}
	schemaName, ok1 := row[1].(string)
	tableName, ok2 := row[2].(string)
	if !ok1 || !ok2 {
		return row, nil
	}
	if it.schemas != nil && !it.schemas[strings.ToLower(schemaName)] || it.names != nil && !it.names[strings.ToLower(tableName)] {
		return row, nil // 上の Filter で落ちる行は数えない
	}
	var n uint64
	if t := lookupHeimdbTable(ctx, it.cat, schemaName, tableName); t != nil {
		if countedTables != nil {
			countedTables(tableName)
		}
		if n, err = t.rowCount(ctx); err != nil {
			return nil, err
		}
	} else if !isHeimdbDatabase(ctx, it.cat, schemaName) {
		return row, nil
	}
	// 読めないテーブル（権限がない）は 0 にする。go-mysql-server は権限を見ずに RowCount の見積もりを出す。
	row = append(sql.Row(nil), row...)
	row[tablesTableRowsOrdinal] = n
	return row, nil
}

// isHeimdbDatabase は、データベース db が HeimDB のデータベースかどうか。
func isHeimdbDatabase(ctx *sql.Context, cat sql.Catalog, db string) bool {
	d, err := cat.Database(ctx, db)
	if err != nil {
		return false
	}
	for {
		switch u := d.(type) {
		case *Database:
			return true
		case interface{ Unwrap() sql.Database }:
			d = u.Unwrap()
		default:
			return false
		}
	}
}

// lookupHeimdbTable は、データベース db のテーブル name が HeimDB のテーブルならそれを返す。
func lookupHeimdbTable(ctx *sql.Context, cat sql.Catalog, db, name string) *Table {
	d, err := cat.Database(ctx, db)
	if err != nil {
		return nil
	}
	tbl, ok, err := d.GetTableInsensitive(ctx, name)
	if err != nil || !ok {
		return nil
	}
	for {
		switch t := tbl.(type) {
		case *Table:
			return t
		case sql.TableWrapper:
			tbl = t.Underlying()
		default:
			return nil
		}
	}
}

// rowCount は、このテーブルの時点の行数を数える（行を復号しない）。
func (t *Table) rowCount(ctx *sql.Context) (uint64, error) {
	var n uint64
	txn, done := txnFor(ctx, t.db.st)
	defer done()
	err := txn.ScanRows(t.db.branch, t.def.ID, nil, nil, false, func(store.RowVersion) (bool, error) {
		n++
		return true, interrupted(ctx, int(n))
	})
	return n, err
}
