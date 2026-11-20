package backend

import (
	"bytes"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// showColumnsMySQLId は、SHOW COLUMNS（DESCRIBE）を MySQL と同じ形にする解析ルールの ID。
const showColumnsMySQLId analyzer.RuleId = 10002

// showColumnsMySQL は、HeimDB のテーブルに対する SHOW [FULL] COLUMNS と DESCRIBE を showColumns に置き換える。
//
// go-mysql-server の SHOW COLUMNS（sql/rowexec/show.go の buildShowColumns）は、Type に COLLATE を付け、Collation に
// 列の照合順序ではなく既定値を返し、Default を SQL の式の形（'yes'）で返す。WordPress の dbDelta は DESCRIBE の
// Type と Default で既存の列を比べるので、更新のたびにほぼすべての列を ALTER し直す。wpdb::get_col_charset は
// SHOW FULL COLUMNS の Collation で文字セットを判定する。
func showColumnsMySQL(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		sc, ok := node.(*plan.ShowColumns)
		if !ok {
			return node, transform.SameTree, nil
		}
		rt, ok := sc.Child.(*plan.ResolvedTable)
		if !ok {
			return node, transform.SameTree, nil
		}
		if _, ok := rt.UnderlyingTable().(*Table); !ok {
			return node, transform.SameTree, nil
		}
		return &showColumns{ShowColumns: sc}, transform.NewTree, nil
	})
}

// showColumns は、MySQL と同じ形の行を返す SHOW [FULL] COLUMNS。
type showColumns struct {
	*plan.ShowColumns
}

var _ sql.ExecSourceRel = (*showColumns)(nil)

func (n *showColumns) WithChildren(children ...sql.Node) (sql.Node, error) {
	sc, err := n.ShowColumns.WithChildren(children...)
	if err != nil {
		return nil, err
	}
	return &showColumns{ShowColumns: sc.(*plan.ShowColumns)}, nil
}

// WithExpressions と WithTargetSchema も包み直す。後の解析の段（列の DEFAULT の解決など）がこれらでノードを
// 作り直すので、包み直さないと go-mysql-server の ShowColumns に戻ってしまう。
func (n *showColumns) WithExpressions(exprs ...sql.Expression) (sql.Node, error) {
	sc, err := n.ShowColumns.WithExpressions(exprs...)
	if err != nil {
		return nil, err
	}
	return &showColumns{ShowColumns: sc.(*plan.ShowColumns)}, nil
}

func (n *showColumns) WithTargetSchema(schema sql.Schema) (sql.Node, error) {
	sc, err := n.ShowColumns.WithTargetSchema(schema)
	if err != nil {
		return nil, err
	}
	return &showColumns{ShowColumns: sc.(*plan.ShowColumns)}, nil
}

func (n *showColumns) String() string { return "heimdb." + n.ShowColumns.String() }

// mysqlPrivileges は、SHOW FULL COLUMNS の Privileges 列。HeimDB は権限を管理しないので、MySQL の root と同じ値を返す。
const mysqlPrivileges = "select,insert,update,references"

// RowIter は sql.ExecSourceRel の実装。
func (n *showColumns) RowIter(ctx *sql.Context, _ sql.Row) (sql.RowIter, error) {
	table := n.Child.(*plan.ResolvedTable).UnderlyingTable().(*Table)
	keys := columnKeys(table)
	var rows []sql.Row
	for _, col := range n.TargetSchema() {
		// MySQL は文字列（CHAR、VARCHAR、TEXT）、ENUM、SET の列に照合順序を出す。
		var collation any
		if ct, ok := col.Type.(interface{ Collation() sql.CollationID }); ok && (types.IsTextOnly(col.Type) || types.IsEnum(col.Type) || types.IsSet(col.Type)) {
			collation = ct.Collation().Name()
		}
		null := "NO"
		if col.Nullable {
			null = "YES"
		}
		def, generated, err := columnDefault(ctx, col)
		if err != nil {
			return nil, err
		}
		var extras []string
		if col.AutoIncrement {
			extras = append(extras, "auto_increment")
		}
		if generated {
			extras = append(extras, "DEFAULT_GENERATED")
		}
		if col.OnUpdate != nil {
			extras = append(extras, "on update "+unwrapParens(col.OnUpdate.String()))
		}
		typ := mysqlTypeString(col.Type)
		key := keys[strings.ToLower(col.Name)]
		if n.Full {
			rows = append(rows, sql.Row{col.Name, typ, collation, null, key, def, strings.Join(extras, " "), mysqlPrivileges, col.Comment})
		} else {
			rows = append(rows, sql.Row{col.Name, typ, null, key, def, strings.Join(extras, " ")})
		}
	}
	return &rowIter{rows: rows}, nil
}

var typeSuffix = regexp.MustCompile(` (CHARACTER SET|COLLATE) \S+`)

// mysqlTypeString は、列の型を MySQL の SHOW COLUMNS と同じ形（文字セットと照合順序を付けない）にする。
// go-mysql-server の TIME 型は精度を持たず、常に time(6) と名乗るので time にする（TIME(6) の列も time と表示される）。
func mysqlTypeString(t sql.Type) string {
	s := typeSuffix.ReplaceAllString(t.String(), "")
	if s == "time(6)" {
		return "time"
	}
	return s
}

// unwrapParens は、式全体を囲む括弧を 1 組だけ外す（(CURRENT_TIMESTAMP) → CURRENT_TIMESTAMP）。
func unwrapParens(s string) string {
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		return s[1 : len(s)-1]
	}
	return s
}

// columnDefault は、列の DEFAULT を MySQL の SHOW COLUMNS と同じ形にする。定数なら値そのもの（文字列は引用符なし）、
// 式（CURRENT_TIMESTAMP など）なら式の文字列を返し、generated を true にする。DEFAULT がないか NULL なら nil。
func columnDefault(ctx *sql.Context, col *sql.Column) (value any, generated bool, err error) {
	if col.Default == nil {
		return nil, false, nil
	}
	if !col.Default.IsLiteral() {
		return unwrapParens(col.Default.String()), true, nil
	}
	v, err := col.Default.Eval(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	if v == nil {
		return nil, false, nil
	}
	// MySQL は BIT を b'101'、BINARY と VARBINARY を 0x6162 の形で表示する（BINARY の末尾の 0 埋めは含めない）。
	if types.IsBit(col.Type) {
		if n, ok := v.(uint64); ok {
			return "b'" + strconv.FormatUint(n, 2) + "'", false, nil
		}
	}
	if b, ok := v.([]byte); ok && types.IsBinaryType(col.Type) {
		if col.Type.Type() == sqltypes.Binary {
			b = bytes.TrimRight(b, "\x00")
		}
		return "0x" + strings.ToUpper(hex.EncodeToString(b)), false, nil
	}
	sqlVal, err := col.Type.SQL(ctx, nil, v)
	if err != nil {
		return nil, false, err
	}
	return sqlVal.ToString(), false, nil
}

// columnKeys は、MySQL の SHOW COLUMNS の Key 列を求める。主キーの列は PRI、1 列だけの一意インデックスの列は UNI、
// それ以外のインデックスの先頭の列は MUL（PRI、UNI、MUL の順に優先）。
func columnKeys(t *Table) map[string]string {
	keys := map[string]string{}
	set := func(col, key string) {
		col = strings.ToLower(col)
		rank := map[string]int{"": 0, "MUL": 1, "UNI": 2, "PRI": 3}
		if rank[key] > rank[keys[col]] {
			keys[col] = key
		}
	}
	for _, ord := range t.def.PKOrdinals {
		set(t.def.Columns[ord].Name, "PRI")
	}
	for _, idx := range t.def.Indexes {
		if idx.Unique && len(idx.Columns) == 1 {
			set(idx.Columns[0].Name, "UNI")
		} else {
			set(idx.Columns[0].Name, "MUL")
		}
	}
	return keys
}
