// Modified by Tsukinowa Inc.: 副問い合わせから持ち上げて NOT の下に置く、NULL になりうる外の列だけの条件を IsTrue で包む（上流の #3302。docs/engine.md）

package analyzer

import (
	"fmt"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
)

// hoistOutOfScopeFilters pulls filters upwards into the parent scope
// to decorrelate subqueries for further optimizations.
//
// select * from xy where exists (select * from uv where x = 1)
// =>
// select * from xy where x = 1 and exists (select * from uv)
func hoistOutOfScopeFilters(ctx *sql.Context, a *Analyzer, n sql.Node, scope *plan.Scope, sel RuleSelector, qFlags *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	if !qFlags.SubqueryIsSet() {
		return n, transform.SameTree, nil
	}

	switch n.(type) {
	case *plan.TriggerBeginEndBlock:
		return n, transform.SameTree, nil
	default:
	}

	inCorr := sql.ColSet{}
	if sq, ok := n.(*plan.SubqueryAlias); ok {
		inCorr = sq.Correlated
	}

	// todo: seems like inCorr/outCorr should match
	ret, same, filters, outCorr, err := recurseSubqueryForOuterFilters(n, a, inCorr, hoistScope{})
	if len(filters) != 0 {
		return n, transform.SameTree, fmt.Errorf("rule 'hoistOutOfScopeFilters' tried to hoist filters above root node")
	}

	if sq, ok := ret.(*plan.SubqueryAlias); ok {
		ret = sq.WithCorrelated(outCorr)
	}
	return ret, same, err
}

// recurseSubqueryForOuterFilters recursively hoists filters that belong
// to an outer scope (maybe higher than the parent). We do a DFS for hoisting
// subquery filters. We do a BFS to extract hoistable filters from subquery
// expressions before checking the normalized subquery and its hoisted
// filters for further hoisting.
func recurseSubqueryForOuterFilters(n sql.Node, a *Analyzer, corr sql.ColSet, outer hoistScope) (sql.Node, transform.TreeIdentity, []sql.Expression, sql.ColSet, error) {
	var hoistFilters []sql.Expression
	var newCorr sql.ColSet
	ret, same, err := transform.Node(n, func(n sql.Node) (sql.Node, transform.TreeIdentity, error) {
		sq, _ := n.(*plan.SubqueryAlias)
		if sq != nil {
			corrIn := corr.Union(sq.Correlated)
			newQ, same, hoisted, subCorr, err := recurseSubqueryForOuterFilters(sq.Child, a, corrIn, outer)
			if err != nil {
				return n, transform.SameTree, err
			}
			if same {
				return n, transform.SameTree, nil
			}
			if len(hoisted) > 0 {
				hoistFilters = append(hoistFilters, hoisted...)
			}
			newCorr = newCorr.Union(subCorr)
			return sq.WithChild(newQ).WithCorrelated(subCorr), transform.NewTree, nil
		}
		f, _ := n.(*plan.Filter)
		if f == nil {
			return n, transform.SameTree, nil
		}

		var keepFilters []sql.Expression
		allSame := transform.SameTree
		queue := expression.SplitConjunction(f.Expression)
		for len(queue) > 0 {
			e := queue[0]
			queue = queue[1:]

			var not bool
			if n, ok := e.(*expression.Not); ok {
				not = true
				e = n.Child
			}

			var sq *plan.Subquery
			switch e := e.(type) {
			case *plan.InSubquery:
				sq, _ = e.RightChild.(*plan.Subquery)
			case *plan.ExistsSubquery:
				sq = e.Query
			default:
			}

			// only try to pull filters from correlated subqueries
			if sq != nil && !sq.Correlated().Empty() {
				children := e.Children()
				corrIn := corr.Union(sq.Correlated())
				newQ, same, hoisted, subCorr, err := recurseSubqueryForOuterFilters(sq.Query, a, corrIn, outer.with(f.Child))
				if err != nil {
					return n, transform.SameTree, err
				}
				newCorr = newCorr.Union(subCorr)
				allSame = allSame && same
				newSq := sq.WithQuery(newQ)
				newSq = newSq.WithCorrelated(subCorr)
				children[len(children)-1] = newSq
				e, _ = e.WithChildren(children...)

				if len(hoisted) > 0 {
					if not {
						// hoisted are tied to parent NOT, more elegant simplification
						// required to expose individual expressions for further hoisting
						// HeimDB: 副問い合わせの中では NULL の条件は偽と同じだが、NOT(EXISTS ... AND 条件) では NULL に
						// なって行が落ちるので、NULL になりうる条件は IsTrue で包む（上流の #3302）。NULL になりうるかは、
						// この Filter の子のスキーマ（外部結合の NULL で埋められる側は NULL を許す）で決める
						hoisted = notNullHoisted(hoisted, outer.with(f.Child))
						e = expression.JoinAnd(e, expression.JoinAnd(hoisted...))
					} else {
						queue = append(queue, hoisted...)
					}
				}
			}

			if not {
				e = expression.NewNot(e)
			}

			inScope, outOfScope := partitionFilterByScope(e, corr)
			if !inScope.Empty() {
				// maintain reference to correlations that aren't hoisted
				newCorr = newCorr.Union(outOfScope)
				keepFilters = append(keepFilters, e)
			} else {
				// nothing tethers the subquery to this scope
				hoistFilters = append(hoistFilters, e)
			}
		}

		if len(hoistFilters) > 0 {
			allSame = transform.NewTree
		}
		if allSame {
			return n, transform.SameTree, nil
		}

		if corr.Empty() {
			// rootscope or equivalent, there is no benefit from hoisting
			// we should materialize filters
			newFilters := append(keepFilters, hoistFilters...)
			hoistFilters = hoistFilters[:0]
			return plan.NewFilter(expression.JoinAnd(newFilters...), f.Child), transform.NewTree, nil
		}

		if len(keepFilters) == 0 {
			return f.Child, transform.NewTree, nil
		}
		ret := plan.NewFilter(expression.JoinAnd(keepFilters...), f.Child)
		return ret, transform.NewTree, nil
	})
	return ret, same, hoistFilters, newCorr, err
}

// partitionFilterByScope returns two colsets that include the in and
// out-of-scope columns referenced in this expression.
func partitionFilterByScope(e sql.Expression, corr sql.ColSet) (inScope, outOfScope sql.ColSet) {
	transform.InspectExpr(e, func(e sql.Expression) bool {
		switch e := e.(type) {
		case *expression.GetField:
			// we're searching for anything in-scope
			// return true if not correlated from outerscope
			id := e.Id()
			if corr.Contains(id) {
				outOfScope.Add(id)
			} else {
				inScope.Add(id)
			}
		case *plan.Subquery:
			// TODO cache in-scope on subqueries?
			transform.Inspect(e.Query, func(n sql.Node) bool {
				if ne, ok := n.(sql.Expressioner); ok {
					for _, e := range ne.Expressions() {
						in, out := partitionFilterByScope(e, corr)
						inScope = inScope.Union(in)
						outOfScope = outOfScope.Union(out)
					}
				}
				return true
			})
		default:
		}
		return false
	})
	return
}

// notNullHoisted は、NOT の下に置く持ち上げた条件のうち、NULL になりうるものを IsTrue で包む。このフォークでは
// WEEKDAY、LOG、SQRT、CASE などの IsNullable が偽を返すので（上流の #3308、#3325 は取り込んでいない）、列と定数の単純な
// 比較（isPlainComparison）のほかはいつも包む。単純な比較は、列が schema で NULL を許さず、定数が NULL でなければ包まない
// （NOT NULL の列の比較の計画を変えない）。列の IsNullable は列の定義を見るので、外部結合の NULL で埋められる側の
// NOT NULL の列も偽になる。そのため、この Filter と外のスコープの Filter の子のスキーマ（LEFT JOIN の右と RIGHT JOIN の
// 左は NULL を許す）で決め、スキーマにない列は NULL になりうるとみなす。派生表の列は、その中の外部結合の NULL を許す
// 印がスキーマに伝わらないので、表の列（tables）のほかは NULL になりうるとみなす。
func notNullHoisted(hoisted []sql.Expression, sc hoistScope) []sql.Expression {
	ret := make([]sql.Expression, len(hoisted))
	for i, h := range hoisted {
		if _, ok := h.(*expression.IsTrue); ok || (isPlainComparison(h) && !nullableIn(h, sc)) {
			ret[i] = h
			continue
		}
		ret[i] = expression.NewIsTrue(h)
	}
	return ret
}

// nullableIn は、e の列か定数のどれかが NULL になりうるか（列は schema の表と名前で探す）。
func nullableIn(e sql.Expression, sc hoistScope) bool {
	nullable := false
	transform.InspectExpr(e, func(e sql.Expression) bool {
		switch e := e.(type) {
		case *expression.GetField:
			if e.IsNullable() || sc.nullable(e.Table(), e.Name()) {
				nullable = true
			}
		case *expression.Literal:
			if e.Value() == nil {
				nullable = true
			}
		}
		return nullable
	})
	return nullable
}

// hoistScope は、今の Filter とその外のスコープの、行のスキーマと表の名前（持ち上げた条件が NULL になりうるかを決める）。
type hoistScope struct {
	levels []hoistLevel // 外のスコープから順に
}

// hoistLevel は 1 つのスコープ（Filter の子）の行のスキーマと表の名前。
type hoistLevel struct {
	schema sql.Schema
	tables map[string]bool
}

// with は、外のスコープの内側に n の行のスコープを足したスコープを返す。
func (s hoistScope) with(n sql.Node) hoistScope {
	levels := append(append([]hoistLevel{}, s.levels...), hoistLevel{schema: n.Schema(), tables: baseTableNames(n)})
	return hoistScope{levels: levels}
}

// nullable は、表 table の列 col が NULL になりうるか。名前は SQL と同じく内側のスコープから探す（外と内で同じ別名を
// 使うと、外の列の NULL を許すかを使ってしまう）。見つからない列と、表でない（派生表などの）列は NULL になりうる。
func (s hoistScope) nullable(table, col string) bool {
	for i := len(s.levels) - 1; i >= 0; i-- {
		l := s.levels[i]
		for _, c := range l.schema {
			if strings.EqualFold(c.Name, col) && strings.EqualFold(c.Source, table) {
				return c.Nullable || !l.tables[strings.ToLower(table)]
			}
		}
	}
	return true
}

// baseTableNames は、n の下の表（別名を含む）の名前を小文字で返す。派生表、WITH、副問い合わせの中には入らない。
func baseTableNames(n sql.Node) map[string]bool {
	names := map[string]bool{}
	transform.Inspect(n, func(n sql.Node) bool {
		switch n := n.(type) {
		case *plan.SubqueryAlias:
			return false
		case *plan.TableAlias:
			switch n.Child.(type) {
			case *plan.ResolvedTable, *plan.IndexedTableAccess:
				names[strings.ToLower(n.Name())] = true
			}
			return false
		case *plan.ResolvedTable:
			names[strings.ToLower(n.Name())] = true
		case *plan.IndexedTableAccess:
			names[strings.ToLower(n.Name())] = true
		}
		return true
	})
	return names
}

// isPlainComparison は、e が列と定数だけの比較（= < > <= >= <=> と、列の IN (定数, ...)、列の IS NULL）と、その NOT、
// AND、OR か。そのような式は、列と定数が NULL にならなければ NULL にならない（IsNullable がそのまま使える）。
func isPlainComparison(e sql.Expression) bool {
	plain := func(e sql.Expression) bool {
		switch e.(type) {
		case *expression.GetField, *expression.Literal:
			return true
		}
		return false
	}
	switch e := e.(type) {
	case *expression.Not:
		return isPlainComparison(e.Child)
	case *expression.And:
		return isPlainComparison(e.LeftChild) && isPlainComparison(e.RightChild)
	case *expression.Or:
		return isPlainComparison(e.LeftChild) && isPlainComparison(e.RightChild)
	case *expression.IsNull:
		return plain(e.Child)
	case *expression.InTuple:
		t, ok := e.Right().(expression.Tuple)
		if !ok || !plain(e.Left()) {
			return false
		}
		for _, v := range t {
			if !plain(v) {
				return false
			}
		}
		return true
	case expression.Comparer:
		return plain(e.Left()) && plain(e.Right())
	}
	return false
}
