package backend

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// checkLookupKeysId は、JOIN のインデックスの検索のキーを、列の型に入るか確かめる式で包む解析ルールの ID。
const checkLookupKeysId analyzer.RuleId = 10011

// checkLookupKeys は、JOIN のインデックスの検索（LookupJoin の右側）のキーのうち、整数の列に対するものを
// lookupKey で包む。
//
// go-mysql-server は、検索のキーを列の型に変換し、範囲の外の値を端の値に丸めて（-1 を int unsigned なら
// 4294967295 に）、2^63 以上の uint64 を狭い unsigned の型では下位のビットに切り詰めて（2^63+1 を 1 に）検索する
// （plan.LookupBuilder.GetLookup）。小数は四捨五入する。JOIN の条件は検索で済んだものとして評価し直さないので、
// 値の違う行がつながる。NULL を等しいとみなす比較（<=>）のキーは包まない（NULL にすると NULL の行に一致する。
// WordPress は使わない）。
func checkLookupKeys(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	return transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		access, ok := node.(*plan.IndexedTableAccess)
		if !ok || access.IsStatic() {
			return node, transform.SameTree, nil
		}
		idx, ok := access.Index().(*Index)
		if !ok {
			return node, transform.SameTree, nil
		}
		keys := access.Expressions()
		mask := access.NullMask()
		if len(keys) > len(idx.types) || len(mask) != len(keys) {
			return node, transform.SameTree, nil
		}
		wrapped := make([]sql.Expression, len(keys))
		changed := false
		for i, k := range keys {
			wrapped[i] = k
			if _, ok := k.(*lookupKey); ok || mask[i] || !types.IsInteger(idx.types[i]) {
				continue
			}
			wrapped[i] = &lookupKey{child: k, typ: idx.types[i]}
			changed = true
		}
		if !changed {
			return node, transform.SameTree, nil
		}
		newNode, err := access.WithExpressions(wrapped...)
		if err != nil {
			return nil, transform.SameTree, err
		}
		return newNode, transform.NewTree, nil
	})
}

// lookupKey は、JOIN の検索のキー child を評価し、整数の値が列の型 typ にそのまま入らなければ（範囲の外）NULL を
// 返す（NULL のキーは何にも一致しない）。整数でない値（文字列、小数）は、go-mysql-server が MySQL と同じく DOUBLE か
// DECIMAL で比べて引く（段階 116）。
type lookupKey struct {
	child sql.Expression
	typ   sql.Type
}

var _ sql.Expression = (*lookupKey)(nil)

func (e *lookupKey) Resolved() bool             { return e.child.Resolved() }
func (e *lookupKey) String() string             { return e.child.String() }
func (e *lookupKey) Type() sql.Type             { return e.child.Type() }
func (e *lookupKey) IsNullable() bool           { return true }
func (e *lookupKey) Children() []sql.Expression { return []sql.Expression{e.child} }

func (e *lookupKey) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 1)
	}
	return &lookupKey{child: children[0], typ: e.typ}, nil
}

func (e *lookupKey) Eval(ctx *sql.Context, row sql.Row) (any, error) {
	v, err := e.child.Eval(ctx, row)
	if err != nil || v == nil {
		return v, err
	}
	if _, isInt := intValue(v); !isInt {
		// 文字列、浮動小数点数、DECIMAL のキーは、go-mysql-server の plan.integerLookupKey が MySQL と同じく
		// DOUBLE か DECIMAL で比べた値で引く（段階 116。前はここで NULL にしたので、''、'1.0000000000000000001'、
		// 4294967295.0 が何にもつながらなかった）
		return v, nil
	}
	if keyChanged(ctx, e.typ, v) {
		return nil, nil
	}
	return v, nil
}
