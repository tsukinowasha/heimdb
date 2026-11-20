package backend

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// execBuilder は、実行の直前に木を書き換えてから実行する sql.NodeExecBuilder。
//
// 単一のテーブルの UPDATE と、値だけの INSERT は、HeimDB が足した解析ルールを通らない（go-mysql-server の
// node_batches.go）ので、ここで手を入れる。go-mysql-server は、最上位の木だけをこの Build で作り、その下は
// 自分の中で作る（engine.go）。
type execBuilder struct {
	sql.NodeExecBuilder
}

func (b execBuilder) Build(ctx *sql.Context, n sql.Node, r sql.Row) (sql.RowIter, error) {
	n, err := wrapUpdateTargets(n)
	if err != nil {
		return nil, err
	}
	if n, err = nonStrictValues(n, strictMode(ctx)); err != nil {
		return nil, err
	}
	if n, err = doStatements(n, b.NodeExecBuilder); err != nil {
		return nil, err
	}
	if n, err = maintenanceStatements(ctx, n); err != nil {
		return nil, err
	}
	return b.NodeExecBuilder.Build(ctx, n, r)
}

// strictMode は、セッションの sql_mode が STRICT か（go-mysql-server は TRADITIONAL を展開しない。MySQL では
// STRICT_TRANS_TABLES と STRICT_ALL_TABLES を含む）。
func strictMode(ctx *sql.Context) bool {
	m := sql.LoadSqlMode(ctx)
	return m.ModeEnabled("STRICT_TRANS_TABLES") || m.ModeEnabled("STRICT_ALL_TABLES") || m.ModeEnabled("TRADITIONAL")
}
