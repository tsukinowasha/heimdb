package backend

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// wrapUpdateTargets は、木の中の UPDATE の対象のテーブルを binaryCompareTable で包む（execBuilder が実行の直前に呼ぶ）。
//
// go-mysql-server の UPDATE は、古い行と新しい行を対象のテーブルの Schema() の型で比べ、等しければ書き込まない。
// 文字列の比較は列の照合順序に従うので、大文字と小文字を区別しない照合順序（WordPress の既定）では、
// 'editor@example.com' を 'Editor@example.com' に変える UPDATE が書き込まれなかった（MySQL は書き込む）。
// 包んだテーブルの Schema() は、文字列の列をバイト単位で比べる照合順序（utf8mb4_bin）にする。
func wrapUpdateTargets(n sql.Node) (sql.Node, error) {
	n, _, err := transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		u, ok := node.(*plan.Update)
		if !ok {
			return node, transform.SameTree, nil
		}
		child, ok := wrapUpdateTarget(u.Child)
		if !ok {
			return node, transform.SameTree, nil
		}
		nu, err := u.WithChildren(child)
		if err != nil {
			return nil, transform.SameTree, err
		}
		return nu, transform.NewTree, nil
	})
	return n, err
}

// wrapUpdateTarget は、plan.GetUpdatable と同じ道筋（子が 1 つの間）で対象のテーブルを探し、包んだ木を返す。
func wrapUpdateTarget(n sql.Node) (sql.Node, bool) {
	switch n := n.(type) {
	case *plan.ResolvedTable:
		if _, done := n.Table.(*binaryCompareTable); done {
			return n, false
		}
		upd, ok := updatableTable(n.Table)
		if !ok {
			return n, false
		}
		nt, err := n.ReplaceTable(&binaryCompareTable{Table: n.Table, upd: upd, sch: binaryCompareSchema(upd.Schema())})
		if err != nil {
			return n, false
		}
		return nt.(*plan.ResolvedTable), true
	case *plan.IndexedTableAccess:
		tn, ok := wrapUpdateTarget(n.TableNode)
		if !ok {
			return n, false
		}
		ita := *n
		ita.TableNode = tn.(sql.TableNode)
		return &ita, true
	}
	children := n.Children()
	if len(children) != 1 {
		return n, false
	}
	child, ok := wrapUpdateTarget(children[0])
	if !ok {
		return n, false
	}
	nn, err := n.WithChildren(child)
	if err != nil {
		return n, false
	}
	return nn, true
}

// updatableTable は、t か、t が包むテーブルのうち、最初の sql.UpdatableTable を返す（plan.GetUpdatable と同じ）。
func updatableTable(t sql.Table) (sql.UpdatableTable, bool) {
	for {
		switch tt := t.(type) {
		case sql.UpdatableTable:
			return tt, true
		case sql.TableWrapper:
			t = tt.Underlying()
		default:
			return nil, false
		}
	}
}

// binaryCompareSchema は、文字列の列の照合順序を utf8mb4_bin にした sch の複製を返す。
func binaryCompareSchema(sch sql.Schema) sql.Schema {
	out := make(sql.Schema, len(sch))
	for i, c := range sch {
		cc := *c
		if st, ok := c.Type.(sql.StringType); ok && st.Collation() != sql.Collation_binary {
			if bt, err := types.CreateString(st.Type(), st.MaxCharacterLength(), sql.Collation_utf8mb4_bin); err == nil {
				cc.Type = bt
			}
		}
		out[i] = &cc
	}
	return out
}

// binaryCompareTable は、Schema() だけを差し替えた UPDATE の対象のテーブル。行の読み出しは元のテーブル（処理の
// 追跡のための包みを含む）に、Updater は包まれた sql.UpdatableTable に任せる。
type binaryCompareTable struct {
	sql.Table
	upd sql.UpdatableTable
	sch sql.Schema
}

var (
	_ sql.UpdatableTable = (*binaryCompareTable)(nil)
	_ sql.TableWrapper   = (*binaryCompareTable)(nil)
)

func (t *binaryCompareTable) Schema() sql.Schema                      { return t.sch }
func (t *binaryCompareTable) Underlying() sql.Table                   { return t.Table }
func (t *binaryCompareTable) Updater(ctx *sql.Context) sql.RowUpdater { return t.upd.Updater(ctx) }
