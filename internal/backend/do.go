package backend

import (
	"strings"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// DO 文（docs/plan.md の段階 69）。WordPress の wpdb::check_connection() は、接続が生きているかを DO 1 で確かめる
// （mysqli_query(...) !== false）。vitess の構文解析は DO を知らず、構文エラーになると WordPress はつなぎ直していた。
//
// 構文解析の前に、文の先頭の DO を SELECT にして式を解析し（rewriteDo）、解析した木の式に目印の別名を付ける（markDo）。
// 実行の直前（execBuilder）に、目印の付いた射影を、射影を実行して読み切り、行を返さずに OK を返す節（doNode）にする
// （doStatements）。解析のルールで置き換えると、後のルールが中の射影に届かず、副問い合わせの列の位置が決まらない。
//
// 記録だけ（WordPress は使わない）: DO 1 FROM t のように MySQL では構文エラーになる書き方も、SELECT として通る。
// DO MAX(1) のように、エンジンが結果を最大 1 行として返す文はエラーになる（その経路が OK の結果を扱わない）。
// バイナリプロトコルの準備（COM_STMT_PREPARE）は vitess が HeimDB の構文解析より先に文を解析するので、DO は構文
// エラーのまま（文字列の PREPARE s FROM 'DO 1' は通る）。複数の文を送れる接続では、エンジンのログに残る文が
// 書き換えた後の SELECT になる。

// doAlias は、DO の式に付ける目印の別名。
const doAlias = "__heimdb_do__"

// rewriteDo は、文の先頭（空白とコメントの後）の語が DO なら、SELECT にした文と true を返す。
func rewriteDo(query string) (string, bool) {
	rest := skipSpaceAndComments(query)
	if len(rest) < 2 || !strings.EqualFold(rest[:2], "DO") || len(rest) > 2 && isWordByte(rest[2]) {
		return query, false
	}
	return query[:len(query)-len(rest)] + "SELECT" + rest[2:], true
}

// markDo は、SELECT として解析した DO の式に目印の別名を付ける。UNION のような SELECT にしかない形は、MySQL と同じく
// 構文エラーにする（DO * は、表のない SELECT * としてエラーになる）。
func markDo(stmt ast.Statement) error {
	sel, ok := stmt.(*ast.Select)
	if !ok {
		return sql.ErrSyntaxError.New("syntax error in DO")
	}
	for _, e := range sel.SelectExprs {
		if ae, ok := e.(*ast.AliasedExpr); ok {
			ae.As = ast.NewColIdent(doAlias)
		}
	}
	return nil
}

// doStatements は、木の中の、目印の別名だけの射影を doNode にする。b は射影を実行する組み立て。
func doStatements(n sql.Node, b sql.NodeExecBuilder) (sql.Node, error) {
	n, _, err := transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		// DO には式が 1 つ以上ある（列のない射影は、IN (SELECT ...) の解析が作る）
		p, ok := node.(*plan.Project)
		if !ok || len(p.Projections) == 0 {
			return node, transform.SameTree, nil
		}
		for _, e := range p.Projections {
			if a, ok := e.(*expression.Alias); !ok || a.Name() != doAlias {
				return node, transform.SameTree, nil
			}
		}
		return &doNode{project: p, b: b}, transform.NewTree, nil
	})
	return n, err
}

// doNode は DO 文。射影を実行して読み切り（式の副問い合わせも SELECT と同じに実行する）、行を返さずに OK を返す。
type doNode struct {
	project sql.Node
	b       sql.NodeExecBuilder
}

var _ sql.ExecSourceRel = (*doNode)(nil)

func (n *doNode) Resolved() bool       { return true }
func (n *doNode) String() string       { return "heimdb.Do" }
func (n *doNode) Schema() sql.Schema   { return types.OkResultSchema }
func (n *doNode) Children() []sql.Node { return nil }
func (n *doNode) IsReadOnly() bool     { return true }
func (n *doNode) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 0 {
		return nil, sql.ErrInvalidChildrenNumber.New(n, len(children), 0)
	}
	return n, nil
}

// RowIter は sql.ExecSourceRel の実装。
func (n *doNode) RowIter(ctx *sql.Context, r sql.Row) (sql.RowIter, error) {
	iter, err := n.b.Build(ctx, n.project, r)
	if err != nil {
		return nil, err
	}
	if _, err := sql.RowIterToRows(ctx, iter); err != nil {
		return nil, err
	}
	return &rowIter{rows: []sql.Row{{types.NewOkResult(0)}}}, nil
}
