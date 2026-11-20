package backend

import (
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"

	"github.com/tsukinowasha/heimdb/internal/schema"
)

// coveringIndexForSortId は、並べ替える読み出しのインデックスを、インデックスだけで読めるものに替える解析ルールの ID。
const coveringIndexForSortId analyzer.RuleId = 10013

// coveringIndexForSort は、Sort（または TopN）の下で、go-mysql-server が選んだインデックスでは本体の行を引く読み出しを、
// 先頭の等号の列が同じで、文が使う列をすべてエントリに持つインデックスの読み出しに替える（docs/plan.md の段階 28）。
//
// 例: 投稿一覧の post_type = 'post' AND post_status <> 'trash' ... ORDER BY post_date DESC LIMIT 20 は、統計では
// type_status_author と type_status_date が同じに見え、type_status_author を選んで記事の本体を 5,000 行引いていた。
// MySQL は type_status_date を選び、インデックスだけで読んで並べ替える（Using index; Using filesort）。
//
// 上に Sort があるので、読む順が変わっても結果の並びは変わらない（同順位の行の並びだけが変わる）。範囲は、先頭の
// 等号の列（どの範囲でも 1 つの値）をそのまま使い、残りの列はすべての値にする（後ろの列は narrowIndexRanges が
// 条件で狭める）。
func coveringIndexForSort(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		switch node.(type) {
		case *plan.Sort, *plan.TopN:
		default:
			return node, transform.SameTree, nil
		}
		child, same, err := swapCoveringIndex(ctx, node.Children()[0])
		if err != nil || same {
			return node, transform.SameTree, err
		}
		newNode, err := node.WithChildren(child)
		return newNode, transform.NewTree, err
	})
}

// swapCoveringIndex は、Project、Filter、GroupBy、JOIN の下の IndexedTableAccess を、インデックスだけで読める読み出しに
// 替えた木を返す。GroupBy と JOIN の下で読む順が変わっても、結果の行の集まりは変わらず、並びは上の Sort が決める
// （docs/plan.md の段階 50。カテゴリの記事の一覧は、記事を LEFT JOIN で引いて GroupBy で束ね、post_date で並べる）。
// 替えるのは静的な読み出しだけで、JOIN で外側の行ごとに引く側（静的でない）は替えない。マージ JOIN の下は替えない。
func swapCoveringIndex(ctx *sql.Context, n sql.Node) (sql.Node, transform.TreeIdentity, error) {
	switch n := n.(type) {
	case *plan.Project, *plan.Filter, *plan.GroupBy:
		child, same, err := swapCoveringIndex(ctx, n.Children()[0])
		if err != nil || same {
			return n, transform.SameTree, err
		}
		newNode, err := n.WithChildren(child)
		return newNode, transform.NewTree, err
	case *plan.JoinNode:
		// マージ JOIN は両側をキーの順に読む前提なので、読む順を変えない。替えるのは、外側の行ごとに引く JOIN と
		// ハッシュ JOIN（読む順が結果に響かない）の下だけ。
		if op := n.JoinType(); !op.IsLookup() && !op.IsHash() {
			return n, transform.SameTree, nil
		}
		children := n.Children()
		changed := false
		for i, c := range children {
			nc, same, err := swapCoveringIndex(ctx, c)
			if err != nil {
				return n, transform.SameTree, err
			}
			if !same {
				children[i], changed = nc, true
			}
		}
		if !changed {
			return n, transform.SameTree, nil
		}
		newNode, err := n.WithChildren(children...)
		return newNode, transform.NewTree, err
	case *plan.IndexedTableAccess:
		access, err := coveringAccess(ctx, n)
		if err != nil || access == nil {
			return n, transform.SameTree, err
		}
		return access, transform.NewTree, nil
	}
	return n, transform.SameTree, nil
}

// coveringAccess は、access を替える読み出しを返す（替えられなければ nil）。
func coveringAccess(ctx *sql.Context, access *plan.IndexedTableAccess) (*plan.IndexedTableAccess, error) {
	if !access.IsStatic() {
		return nil, nil
	}
	table, ok := access.UnderlyingTable().(*Table)
	if !ok {
		if it, ok2 := access.UnderlyingTable().(*IndexedTable); ok2 {
			table, ok = it.Table, true
		}
	}
	if !ok || table.proj == nil {
		return nil, nil
	}
	lookup, err := access.GetLookup(ctx, nil)
	if err != nil {
		return nil, nil
	}
	idx, ok := lookup.Index.(*Index)
	ranges, ok2 := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok || !ok2 || idx.fulltext || lookup.IsEmptyRange || lookup.IsReverse || len(ranges) == 0 {
		return nil, nil
	}
	cur, ok := table.findKeyIndex(idx.name)
	if !ok || table.covers(cur) {
		return nil, nil
	}
	// どの範囲でも 1 つの値の、先頭の列の数
	k := len(idx.columns)
	for _, r := range ranges {
		m := 0
		for m < len(r) && m < k && isPoint(ctx, r[m]) {
			m++
		}
		for _, ce := range r[m:] {
			_, lowerAll := ce.LowerBound.(sql.BelowNull)
			_, upperAll := ce.UpperBound.(sql.AboveAll)
			if !lowerAll || !upperAll {
				return nil, nil // 等号の列の後ろに範囲がある（替えると読む行が増える）
			}
		}
		k = m
	}
	if k == 0 {
		return nil, nil
	}
	for _, ki := range table.keyIndexes() {
		if ki.hasPrefix || len(ki.def.Columns) <= k || !table.covers(ki) || !samePrefix(idx.columns[:k], ki) {
			continue
		}
		newRanges := make(sql.MySQLRangeCollection, len(ranges))
		for i, r := range ranges {
			nr := append(sql.MySQLRange(nil), r[:k]...)
			for _, ord := range ki.ords[k:] {
				nr = append(nr, sql.AllRangeColumnExpr(table.sch.Schema[ord].Type))
			}
			newRanges[i] = nr
		}
		var newIdx *Index
		indexes, err := table.GetIndexes(ctx)
		if err != nil {
			return nil, err
		}
		for _, si := range indexes {
			if si.ID() == ki.def.Name {
				newIdx = si.(*Index)
			}
		}
		if newIdx == nil {
			return nil, nil
		}
		lookup.Index = newIdx
		lookup.Ranges = newRanges
		lookup.IsPointLookup = false
		return plan.NewStaticIndexedAccessForTableNode(ctx, access.TableNode, lookup)
	}
	return nil, nil
}

// samePrefix は、インデックス ki の先頭の列が cols と同じ（プレフィックス長なし）か。
func samePrefix(cols []schema.IndexColumn, ki keyIndex) bool {
	for i, c := range cols {
		if c.Length > 0 || !strings.EqualFold(c.Name, ki.def.Columns[i].Name) {
			return false
		}
	}
	return true
}
