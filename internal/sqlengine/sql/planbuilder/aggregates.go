// Copyright 2023 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Modified by Tsukinowa Inc.: HAVING の集計の列を表の修飾子で探し、スカラー副問い合わせの中の集計を外の集計で使い回さない（上流の #3739、#3760。docs/engine.md）
// Modified by Tsukinowa Inc.: GROUP BY と HAVING の集計の引数で SELECT の別名を使えるようにし（上流の #3073）、HAVING だけの GROUP_CONCAT の列を集計の前に記録する（docs/engine.md）
// Modified by Tsukinowa Inc.: HAVING の集計と GROUP_CONCAT の引数の式の中の別名を解き、集計の中の集計を 1111 にする（段階 115 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: HAVING の名前を SELECT の項から先に探す（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: HAVING の名前で、列の別名の GROUP BY の列を GROUP BY の列とし、SELECT の項は列でない項を同じ名前の列より先にする（段階 116 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: SUM と AVG の列の型を集計の型にする（整数と DECIMAL は DECIMAL。段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: HAVING の別名をその問い合わせの集計でだけ解き（副問い合わせの外の別名は外の値）、別名を列の番号で決め、あいまいな別名を 1052 にし、GROUP_CONCAT の ORDER BY でも別名を解く（段階 115 の 2 度目のレビュー。docs/engine.md）

package planbuilder

import (
	"fmt"
	"sort"
	"strings"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression/function/aggregation"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

var _ ast.Expr = (*aggregateInfo)(nil)

type groupBy struct {
	inCols   []scopeColumn
	outScope *scope
	aggs     map[string]scopeColumn
	grouping map[string]bool
}

func (g *groupBy) addInCol(c scopeColumn) {
	g.inCols = append(g.inCols, c)
}

func (g *groupBy) addOutCol(c scopeColumn) columnId {
	return g.outScope.newColumn(c)
}

func (g *groupBy) hasAggs() bool {
	return len(g.aggs) > 0
}

func (g *groupBy) aggregations() []scopeColumn {
	aggregations := make([]scopeColumn, 0, len(g.aggs))
	for _, agg := range g.aggs {
		aggregations = append(aggregations, agg)
	}
	sort.Slice(aggregations, func(i, j int) bool {
		return aggregations[i].scalar.String() < aggregations[j].scalar.String()
	})
	return aggregations
}

func (g *groupBy) addAggStr(c scopeColumn) {
	if g.aggs == nil {
		g.aggs = make(map[string]scopeColumn)
	}
	g.aggs[strings.ToLower(c.scalar.String())] = c
}

func (g *groupBy) getAggRef(name string) sql.Expression {
	if g.aggs == nil {
		return nil
	}
	ret, _ := g.aggs[name]
	if ret.empty() {
		return nil
	}
	return ret.scalarGf()
}

type aggregateInfo struct {
	ast.Expr
}

func (b *Builder) needsAggregation(fromScope *scope, sel *ast.Select) bool {
	return len(sel.GroupBy) > 0 ||
		(fromScope.groupBy != nil && fromScope.groupBy.hasAggs())
}

func (b *Builder) buildGroupingCols(fromScope, projScope *scope, groupby ast.GroupBy, selects ast.SelectExprs) []sql.Expression {
	// grouping col will either be:
	// 1) alias into targets
	// 2) a column reference
	// 3) an index into selects
	// 4) a simple non-aggregate expression
	groupings := make([]sql.Expression, 0)
	if fromScope.groupBy == nil {
		fromScope.initGroupBy()
	}
	g := fromScope.groupBy
	for _, e := range groupby {
		var col scopeColumn
		switch e := e.(type) {
		case *ast.ColName:
			var ok bool
			// GROUP BY binds to column references before projections.
			dbName := strings.ToLower(e.Qualifier.DbQualifier.String())
			tblName := strings.ToLower(e.Qualifier.Name.String())
			colName := strings.ToLower(e.Name.String())
			col, ok = fromScope.resolveColumn(dbName, tblName, colName, true, false)
			if !ok {
				col, ok = projScope.resolveColumn(dbName, tblName, colName, true, true)
			}

			if !ok {
				b.handleErr(sql.ErrColumnNotFound.New(e.Name.String()))
			}
		case *ast.SQLVal:
			// literal -> index into targets
			v, ok := b.normalizeIntVal(e)
			if !ok {
				b.handleErr(fmt.Errorf("expected integer order by literal"))
			}
			idx, _, err := types.Int64.Convert(b.ctx, v)
			if err != nil {
				b.handleErr(err)
			}
			intIdx, ok := idx.(int64)
			if !ok {
				b.handleErr(fmt.Errorf("expected integer order by literal"))
			}
			if intIdx < 1 {
				// TODO: this actually works in MySQL
				b.handleErr(fmt.Errorf("expected positive integer order by literal"))
			}
			if int(intIdx) > len(selects) {
				b.handleErr(fmt.Errorf("column ordinal out of range: %d", intIdx))
			}
			col = projScope.cols[intIdx-1]
		default:
			expr := b.buildScalar(fromScope, e)
			col = scopeColumn{
				col:      expr.String(),
				typ:      nil,
				scalar:   expr,
				nullable: expr.IsNullable(),
			}
		}
		if col.scalar == nil {
			gf := expression.NewGetFieldWithTable(int(col.id), int(col.tableId), col.typ, col.db, col.table, col.col, col.nullable)
			id, ok := fromScope.getExpr(gf.String(), true)
			if !ok {
				err := sql.ErrColumnNotFound.New(gf.String())
				b.handleErr(err)
			}
			col.scalar = gf.WithIndex(int(id))
		}
		g.addInCol(col)
		if g.grouping == nil {
			g.grouping = make(map[string]bool)
		}
		g.grouping[strings.ToLower(col.col)] = true // HeimDB: HAVING's names (stage 116)
		groupings = append(groupings, col.scalar)
	}

	return groupings
}

func (b *Builder) buildNameConst(fromScope *scope, f *ast.FuncExpr) sql.Expression {
	if len(f.Exprs) != 2 {
		b.handleErr(fmt.Errorf("incorrect parameter count in the call to native function NAME_CONST"))
	}
	alias := b.selectExprToExpression(fromScope, f.Exprs[0])
	aLit, ok := alias.(*expression.Literal)
	if !ok {
		b.handleErr(fmt.Errorf("incorrect arguments to: NAME_CONST"))
	}
	value := b.selectExprToExpression(fromScope, f.Exprs[1])
	vLit, ok := value.(*expression.Literal)
	if !ok {
		b.handleErr(fmt.Errorf("incorrect arguments to: NAME_CONST"))
	}
	var aliasStr string
	if types.IsText(aLit.Type()) {
		aliasStr = strings.Trim(aLit.String(), "'")
	} else {
		aliasStr = aLit.String()
	}
	return expression.NewAlias(aliasStr, vLit)
}

func (b *Builder) buildAggregation(fromScope, projScope *scope, groupingCols []sql.Expression) *scope {
	b.qFlags.Set(sql.QFlagAggregation)

	// GROUP_BY consists of:
	// - input arguments projection
	// - grouping cols projection
	// - aggregate expressions
	// - output projection
	if fromScope.groupBy == nil {
		fromScope.initGroupBy()
	}

	group := fromScope.groupBy
	outScope := group.outScope
	// select columns:
	//  - aggs
	//  - extra columns needed by having, order by, select
	var selectExprs []sql.Expression
	var selectGfs []sql.Expression
	selectStr := make(map[string]bool)
	for _, e := range group.aggregations() {
		if !selectStr[strings.ToLower(e.String())] {
			selectExprs = append(selectExprs, e.scalar)
			selectGfs = append(selectGfs, e.scalarGf())
			selectStr[strings.ToLower(e.String())] = true
		}
	}
	var aliases []sql.Expression
	for _, col := range projScope.cols {
		// eval aliases in project scope
		switch e := col.scalar.(type) {
		case *expression.Alias:
			if !e.Unreferencable() {
				aliases = append(aliases, e.WithId(sql.ColumnId(col.id)).(*expression.Alias))
			}
		default:
		}

		// projection dependencies -> table cols needed above
		transform.InspectExpr(col.scalar, func(e sql.Expression) bool {
			switch e := e.(type) {
			case *expression.GetField:
				colName := strings.ToLower(e.String())
				if !selectStr[colName] {
					selectExprs = append(selectExprs, e)
					selectGfs = append(selectGfs, e)
					selectStr[colName] = true
				}
			default:
			}
			return false
		})
	}
	for _, e := range fromScope.extraCols {
		// accessory cols used by ORDER_BY, HAVING
		if !selectStr[e.String()] {
			selectExprs = append(selectExprs, e.scalarGf())
			selectGfs = append(selectGfs, e.scalarGf())

			selectStr[e.String()] = true
		}
	}
	gb := plan.NewGroupBy(selectExprs, groupingCols, fromScope.node)
	outScope.node = gb

	if len(aliases) > 0 {
		outScope.node = plan.NewProject(append(selectGfs, aliases...), outScope.node)
	}
	return outScope
}

func isAggregateFunc(name string) bool {
	switch name {
	case "avg", "bit_and", "bit_or", "bit_xor", "count",
		"group_concat", "json_arrayagg", "json_objectagg",
		"max", "min", "std", "stddev_pop", "stddev_samp",
		"stddev", "sum", "var_pop", "var_samp", "variance",
		"first", "last", "any_value":
		return true
	default:
		return false
	}
}

// buildAggregateFunc tags aggregate functions in the correct scope
// and makes the aggregate available for reference by other clauses.
func (b *Builder) buildAggregateFunc(inScope *scope, name string, e *ast.FuncExpr) sql.Expression {
	if len(inScope.windowFuncs) > 0 {
		err := sql.ErrNonAggregatedColumnWithoutGroupBy.New()
		b.handleErr(err)
	}

	if inScope.groupBy == nil {
		inScope.initGroupBy()
	}
	gb := inScope.groupBy

	if strings.EqualFold(name, "count") {
		if _, ok := e.Exprs[0].(*ast.StarExpr); ok {
			var agg sql.Aggregation
			if e.Distinct {
				agg = aggregation.NewCountDistinct(expression.NewLiteral(1, types.Int64))
			} else {
				agg = aggregation.NewCount(expression.NewLiteral(1, types.Int64))
			}
			b.qFlags.Set(sql.QFlagCountStar)
			aggName := strings.ToLower(agg.String())
			gf := gb.getAggRef(aggName)
			if gf != nil {
				// if we've already computed use reference here
				return gf
			}

			col := scopeColumn{col: strings.ToLower(agg.String()), scalar: agg, typ: agg.Type(), nullable: agg.IsNullable()}
			id := gb.outScope.newColumn(col)
			col.id = id

			agg = agg.WithId(sql.ColumnId(id)).(sql.Aggregation)
			gb.outScope.cols[len(gb.outScope.cols)-1].scalar = agg
			col.scalar = agg

			gb.addAggStr(col)
			return col.scalarGf()
		}
	}

	if strings.EqualFold(name, "jsonarray") {
		// TODO we don't have any tests for this
		if _, ok := e.Exprs[0].(*ast.StarExpr); ok {
			var agg sql.Aggregation
			agg = aggregation.NewJsonArray(expression.NewLiteral(expression.NewStar(), types.Int64))
			b.qFlags.Set(sql.QFlagStar)

			//if e.Distinct {
			//	agg = plan.NewDistinct(expression.NewLiteral(1, types.Int64))
			//}
			aggName := strings.ToLower(agg.String())
			gf := gb.getAggRef(aggName)
			if gf != nil {
				// if we've already computed use reference here
				return gf
			}

			col := scopeColumn{col: strings.ToLower(agg.String()), scalar: agg, typ: agg.Type(), nullable: agg.IsNullable()}
			id := gb.outScope.newColumn(col)

			agg = agg.WithId(sql.ColumnId(id)).(*aggregation.JsonArray)
			gb.outScope.cols[len(gb.outScope.cols)-1].scalar = agg
			col.scalar = agg

			col.id = id
			gb.addAggStr(col)
			return col.scalarGf()
		}
	}

	if strings.EqualFold(name, "any_value") {
		b.qFlags.Set(sql.QFlagAnyAgg)
	}

	var args []sql.Expression
	for _, arg := range e.Exprs {
		e := b.aggregateArgument(inScope, arg)
		switch e := e.(type) {
		case *expression.GetField:
			if e.TableId() == 0 {
				// HeimDB: an outer query's select alias, as SUM(q) in a subquery of
				// the outer HAVING: a value of the outer row, aggregated here as
				// MAX(q + 1) already was (aggregateArgument has rejected this
				// query's own aliases)
				args = append(args, e)
				gb.addInCol(scopeColumn{col: e.String(), scalar: e, typ: e.Type()})
				break
			}
			args = append(args, e)
			col := scopeColumn{tableId: e.TableID(), db: e.Database(), table: e.Table(), col: e.Name(), scalar: e, typ: e.Type(), nullable: e.IsNullable()}
			gb.addInCol(col)
		case *expression.Star:
			err := sql.ErrStarUnsupported.New()
			b.handleErr(err)
		case *plan.Subquery:
			args = append(args, e)
			col := scopeColumn{col: e.QueryString, scalar: e, typ: e.Type()}
			gb.addInCol(col)
		default:
			args = append(args, e)
			col := scopeColumn{col: e.String(), scalar: e, typ: e.Type()}
			gb.addInCol(col)
		}
	}

	var agg sql.Aggregation
	if e.Distinct && name == "count" {
		agg = aggregation.NewCountDistinct(args...)
	} else {

		// NOTE: Not all aggregate functions support DISTINCT. Fortunately, the vitess parser will throw
		// errors for when DISTINCT is used on aggregate functions that don't support DISTINCT.
		if e.Distinct {
			if len(e.Exprs) != 1 {
				err := sql.ErrUnsupportedSyntax.New("more than one expression with distinct")
				b.handleErr(err)
			}

			args[0] = expression.NewDistinctExpression(args[0])
		}

		f, ok := b.cat.Function(b.ctx, name)
		if !ok {
			// todo(max): similar names in registry?
			err := sql.ErrFunctionNotFound.New(name)
			b.handleErr(err)
		}

		newInst, err := f.NewInstance(args)
		if err != nil {
			b.handleErr(err)
		}

		agg, ok = newInst.(sql.Aggregation)
		if !ok {
			err := fmt.Errorf("expected function to be aggregation: %s", f.FunctionName())
			b.handleErr(err)
		}
	}

	if name == "count" {
		b.qFlags.Set(sql.QFlagCount)
	}

	// SUM と AVG の型は、集計の型（整数と DECIMAL の引数では DECIMAL。前はいつも DOUBLE にしたので、DECIMAL の和が
	// DOUBLE の表示と桁になった。HeimDB）
	aggType := agg.Type()

	aggName := strings.ToLower(plan.AliasSubqueryString(agg))
	// 同じ集計をもう計算していれば、その参照を使う。今の集計のスコープ（gb）だけを見る。前は外のスコープまで名前で
	// 探したので、スカラー副問い合わせの中の MAX(d) が外の MAX(d) を使い回した（上流の #3760）
	if gf := gb.getAggRef(aggName); gf != nil {
		return gf
	}

	col := scopeColumn{col: aggName, scalar: agg, typ: aggType, nullable: agg.IsNullable()}
	id := gb.outScope.newColumn(col)

	agg = agg.WithId(sql.ColumnId(id)).(sql.Aggregation)
	gb.outScope.cols[len(gb.outScope.cols)-1].scalar = agg
	col.scalar = agg

	col.id = id
	gb.addAggStr(col)
	return col.scalarGf()
}

func (b *Builder) buildGroupConcat(inScope *scope, e *ast.GroupConcatExpr) sql.Expression {
	if inScope.groupBy == nil {
		inScope.initGroupBy()
	}
	gb := inScope.groupBy

	args := make([]sql.Expression, len(e.Exprs))
	for i, a := range e.Exprs {
		args[i] = b.aggregateArgument(inScope, a)
	}

	separatorS := ","
	if !e.Separator.DefaultSeparator {
		separatorS = e.Separator.SeparatorString
	}

	var sortFields sql.SortFields
	addSortFields := func(order ast.OrderBy) {
		orderByScope := b.analyzeOrderBy(inScope, inScope, order)
		for _, c := range orderByScope.cols {
			so := sql.Ascending
			if c.descending {
				so = sql.Descending
			}
			scalar := c.scalar
			if scalar == nil {
				scalar = c.scalarGf()
			}
			sf := sql.SortField{
				Column: scalar,
				Order:  so,
			}
			sortFields = append(sortFields, sf)
		}
	}
	if !b.orderByNamesHavingAlias(e.OrderBy) {
		addSortFields(e.OrderBy)
	} else {
		// HeimDB: in a HAVING, GROUP_CONCAT(x ORDER BY q) sorts by the select
		// alias's expression, as MySQL does (analyzeOrderBy looked for q among
		// the FROM's columns and sent 1054)
		for _, o := range e.OrderBy {
			if !b.orderByNamesHavingAlias(ast.OrderBy{o}) {
				addSortFields(ast.OrderBy{o})
				continue
			}
			so := sql.Ascending
			if strings.EqualFold(o.Direction, ast.DescScr) {
				so = sql.Descending
			}
			sortFields = append(sortFields, sql.SortField{Column: b.aggregateArgument(inScope, &ast.AliasedExpr{Expr: o.Expr}), Order: so})
		}
	}

	//TODO: this should be acquired at runtime, not at parse time, so fix this
	gcml, err := b.ctx.GetSessionVariable(b.ctx, "group_concat_max_len")
	if err != nil {
		b.handleErr(err)
	}
	groupConcatMaxLen := gcml.(uint64)

	// todo store ref to aggregate
	agg := aggregation.NewGroupConcat(e.Distinct, sortFields, separatorS, args, int(groupConcatMaxLen))
	aggName := strings.ToLower(plan.AliasSubqueryString(agg))
	if gf := gb.getAggRef(strings.ToLower(agg.String())); gf != nil {
		// HeimDB: reuse the GROUP_CONCAT that analyzeHaving recorded, as
		// buildAggregateFunc does for the other aggregates
		return gf
	}
	col := scopeColumn{col: aggName, scalar: agg, typ: agg.Type(), nullable: agg.IsNullable()}

	id := gb.outScope.newColumn(col)

	agg = agg.WithId(sql.ColumnId(id)).(*aggregation.GroupConcat)
	gb.outScope.cols[len(gb.outScope.cols)-1].scalar = agg
	col.scalar = agg

	// HeimDB: record the column with its id, or getAggRef takes it for empty
	col.id = id
	gb.addAggStr(col)
	return col.scalarGf()
}

// aggregateArgument builds an aggregate function's argument. In a HAVING, a
// reference to the query's select alias (a GetField without a table whose
// column is one of the query's aliases, anywhere in the argument, as in
// AVG(q + 1) or SUM(IF(q > 0, 1, 0))) is replaced with the alias's expression
// (upstream #3073 did this only for a bare alias, by building the argument
// again in the outer scope, which loses the columns of an expression like
// q * 10 + x). The alias is found by its column id, so of two aliases the one
// that the name resolved to is taken (analyzeHaving sends 1052 when they
// differ). A column of the query's aggregate left after that (COUNT(cnt) for
// COUNT(*) AS cnt, also in ORDER BY) is 1111, as in MySQL. Outside the HAVING
// (the select list) MySQL does not see the query's aliases in an aggregate and
// sends 1054. A column of an outer query's HAVING alias, as SUM(q) in a
// subquery of the outer HAVING, is the outer row's value and is left alone;
// b.havingProj belongs to the query being built, since buildSelect moves it to
// b.outerHavingProjs for a subquery. Any other column without a table is 1054
// as before (an outer select-list alias, which the subquery cannot read)
// (HeimDB, stage 115's reviews).
func (b *Builder) aggregateArgument(inScope *scope, arg ast.SelectExpr) sql.Expression {
	argScope := inScope
	if b.havingFrom != nil && inScope.groupBy == b.havingFrom.groupBy && inScope != b.havingFrom && namesInScope(b.havingFrom, arg) {
		// HeimDB: inside a HAVING aggregate a name is the FROM's column before a
		// select alias (HAVING SUM(v) with SUM(v) AS v is the column's sum), as
		// the HAVING's own names now take the select list first (stage 116)
		argScope = b.havingFrom
	}
	e := b.selectExprToExpression(argScope, arg)
	if !hasAliasRef(e) {
		return e
	}
	if b.havingProj != nil {
		e, _, _ = transform.Expr(e, func(x sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
			gf, ok := x.(*expression.GetField)
			if !ok || gf.TableId() != 0 {
				return x, transform.SameTree, nil
			}
			for _, c := range b.havingProj.cols {
				if a, ok := c.scalar.(*expression.Alias); ok && sql.ColumnId(c.id) == gf.Id() {
					// the alias itself, as buildScalar resolves the name in analyzeHaving,
					// so both builds of the aggregate have the same name and share it
					return a, transform.NewTree, nil
				}
			}
			return x, transform.SameTree, nil
		})
	}
	transform.InspectExpr(e, func(x sql.Expression) bool {
		gf, ok := x.(*expression.GetField)
		if !ok || gf.TableId() != 0 {
			return false
		}
		if inScope.groupBy != nil {
			for _, c := range inScope.groupBy.aggs {
				if sql.ColumnId(c.id) == gf.Id() {
					b.handleErr(sql.ErrInvalidGroupFuncUse.New())
				}
			}
		}
		for _, proj := range b.outerHavingProjs {
			for _, c := range proj.cols {
				if _, ok := c.scalar.(*expression.Alias); ok && sql.ColumnId(c.id) == gf.Id() {
					// an alias of an outer query's HAVING: the outer row's value
					return false
				}
			}
		}
		// this query's alias outside its HAVING, or an outer query's alias that
		// the subquery cannot read (one in the outer select list)
		b.handleErr(sql.ErrColumnNotFound.New(gf.Name()))
		return false
	})
	return e
}

// namesInScope reports whether every column name in arg is a column of s
// itself (not of an outer query) (HeimDB).
func namesInScope(s *scope, arg ast.SelectExpr) bool {
	ok := true
	ast.Walk(func(node ast.SQLNode) (bool, error) {
		switch n := node.(type) {
		case *ast.Subquery:
			return false, nil
		case *ast.ColName:
			if _, found := s.resolveColumn(strings.ToLower(n.Qualifier.DbQualifier.String()), strings.ToLower(n.Qualifier.Name.String()), strings.ToLower(n.Name.String()), false, true); !found {
				ok = false
			}
		}
		return true, nil
	}, arg)
	return ok
}

// orderByNamesHavingAlias reports whether a GROUP_CONCAT's ORDER BY in a HAVING
// names a select alias of the query that no column of its FROM has (HeimDB).
func (b *Builder) orderByNamesHavingAlias(order ast.OrderBy) bool {
	if b.havingProj == nil || len(order) == 0 {
		return false
	}
	found := false
	for _, o := range order {
		ast.Walk(func(node ast.SQLNode) (bool, error) {
			switch n := node.(type) {
			case *ast.Subquery:
				return false, nil
			case *ast.ColName:
				if !n.Qualifier.IsEmpty() {
					return false, nil
				}
				name := strings.ToLower(n.Name.String())
				if _, ok := b.havingFrom.resolveColumn("", "", name, true, true); ok {
					return false, nil
				}
				for _, c := range b.havingProj.cols {
					if _, ok := c.scalar.(*expression.Alias); ok && strings.EqualFold(c.col, name) {
						found = true
					}
				}
			}
			return true, nil
		}, o.Expr)
	}
	return found
}

// checkHavingAliasAmbiguity sends 1052, as MySQL does, when a HAVING name that
// no column of the FROM has names two select aliases of different table
// columns (SELECT y AS q, z AS Q ... HAVING q > 0); before, the map of aliases
// picked one at random. MySQL reports this only for aliases of plain columns:
// with an alias of an expression or constant among them (y AS q, z + 0 AS Q,
// or 1 AS col2, 2 AS col2) it takes one of them, which HeimDB does not follow
// (recorded in docs/engine.md) (HeimDB, stage 115's second review).
func (b *Builder) checkHavingAliasAmbiguity(projScope *scope, name string) {
	var first *expression.GetField
	for _, c := range projScope.cols {
		a, ok := c.scalar.(*expression.Alias)
		if !ok || !strings.EqualFold(c.col, name) {
			continue
		}
		gf, ok := a.Child.(*expression.GetField)
		if !ok || gf.TableId() == 0 {
			continue
		}
		if first == nil {
			first = gf
		} else if gf.Id() != first.Id() {
			b.handleErr(sql.ErrAmbiguousColumnOrAliasName.New(name))
		}
	}
}

// hasAliasRef reports whether e reads a column without a table (a select
// alias) (HeimDB).
func hasAliasRef(e sql.Expression) bool {
	return transform.InspectExpr(e, func(e sql.Expression) bool {
		gf, ok := e.(*expression.GetField)
		return ok && gf.TableId() == 0
	})
}

func isWindowFunc(name string) bool {
	switch name {
	case "first", "last", "count", "sum", "any_value",
		"avg", "max", "min", "count_distinct", "json_arrayagg",
		"row_number", "percent_rank", "lead", "lag",
		"first_value", "last_value",
		"rank", "dense_rank",
		"ntile",
		"std", "stddev", "stddev_pop", "stddev_samp",
		"variance", "var_pop", "var_samp":
		return true
	default:
		return false
	}
}

func (b *Builder) buildWindowFunc(inScope *scope, name string, e *ast.FuncExpr, over *ast.WindowDef) sql.Expression {
	if inScope.groupBy != nil {
		err := sql.ErrNonAggregatedColumnWithoutGroupBy.New()
		b.handleErr(err)
	}

	// internal expressions can be complex, but window can't be more than alias
	var args []sql.Expression
	for _, arg := range e.Exprs {
		e := b.selectExprToExpression(inScope, arg)
		args = append(args, e)
	}

	var win sql.WindowAdaptableExpression
	if name == "count" {
		if _, ok := e.Exprs[0].(*ast.StarExpr); ok {
			win = aggregation.NewCount(expression.NewLiteral(1, types.Int64))
			b.qFlags.Set(sql.QFlagCountStar)
		}
	}
	if win == nil {
		f, ok := b.cat.Function(b.ctx, name)
		if !ok {
			// todo(max): similar names in registry?
			err := sql.ErrFunctionNotFound.New(name)
			b.handleErr(err)
		}

		newInst, err := f.NewInstance(args)

		win, ok = newInst.(sql.WindowAdaptableExpression)
		if !ok {
			err := fmt.Errorf("function is not a window adaptable exprssion: %s", f.FunctionName())
			b.handleErr(err)
		}
		if err != nil {
			b.handleErr(err)
		}
	}

	def := b.buildWindowDef(inScope, over)
	switch w := win.(type) {
	case sql.WindowAdaptableExpression:
		win = w.WithWindow(def)
	}

	col := scopeColumn{col: strings.ToLower(win.String()), scalar: win, typ: win.Type(), nullable: win.IsNullable()}
	id := inScope.newColumn(col)
	col.id = id
	win = win.WithId(sql.ColumnId(id)).(sql.WindowAdaptableExpression)
	inScope.cols[len(inScope.cols)-1].scalar = win
	col.scalar = win
	inScope.windowFuncs = append(inScope.windowFuncs, col)
	return col.scalarGf()
}

func (b *Builder) buildWindow(fromScope, projScope *scope) *scope {
	if len(fromScope.windowFuncs) == 0 {
		return fromScope
	}
	// passthrough dependency cols plus window funcs
	var selectExprs []sql.Expression
	var selectGfs []sql.Expression
	selectStr := make(map[string]bool)
	for _, col := range fromScope.windowFuncs {
		e := col.scalar
		if !selectStr[strings.ToLower(e.String())] {
			switch e.(type) {
			case sql.WindowAdaptableExpression:
				selectStr[strings.ToLower(e.String())] = true
				selectExprs = append(selectExprs, e)
				selectGfs = append(selectGfs, col.scalarGf())
			default:
				err := fmt.Errorf("expected window function to be sql.WindowAggregation")
				b.handleErr(err)
			}
		}
	}
	var aliases []sql.Expression
	for _, col := range projScope.cols {
		// eval aliases in project scope
		switch e := col.scalar.(type) {
		case *expression.Alias:
			if !e.Unreferencable() {
				aliases = append(aliases, e.WithId(sql.ColumnId(col.id)).(*expression.Alias))
			}
		default:
		}

		// projection dependencies -> table cols needed above
		transform.InspectExpr(col.scalar, func(e sql.Expression) bool {
			switch e := e.(type) {
			case *expression.GetField:
				colName := strings.ToLower(e.String())
				if !selectStr[colName] {
					selectExprs = append(selectExprs, e)
					selectGfs = append(selectGfs, e)
					selectStr[colName] = true
				}
			default:
			}
			return false
		})
	}
	for _, e := range fromScope.extraCols {
		// accessory cols used by ORDER_BY, HAVING
		if !selectStr[e.String()] {
			selectExprs = append(selectExprs, e.scalarGf())
			selectGfs = append(selectGfs, e.scalarGf())
			selectStr[e.String()] = true
		}
	}

	outScope := fromScope
	window := plan.NewWindow(selectExprs, fromScope.node)
	fromScope.node = window

	if len(aliases) > 0 {
		outScope.node = plan.NewProject(append(selectGfs, aliases...), outScope.node)
	}

	return outScope
}

func (b *Builder) buildNamedWindows(fromScope *scope, window ast.Window) {
	// topo sort first
	adj := make(map[string]*ast.WindowDef)
	for _, w := range window {
		adj[w.Name.Lowered()] = w
	}

	var topo []*ast.WindowDef
	var seen map[string]bool
	var dfs func(string)
	dfs = func(name string) {
		if ok, _ := seen[name]; ok {
			b.handleErr(sql.ErrCircularWindowInheritance.New())
		}
		seen[name] = true
		cur := adj[name]
		if ref := cur.NameRef.Lowered(); ref != "" {
			dfs(ref)
		}
		topo = append(topo, cur)
	}
	for _, w := range adj {
		seen = make(map[string]bool)
		dfs(w.Name.Lowered())
	}

	fromScope.windowDefs = make(map[string]*sql.WindowDefinition)
	for _, w := range topo {
		fromScope.windowDefs[w.Name.Lowered()] = b.buildWindowDef(fromScope, w)
	}
	return
}

func (b *Builder) buildWindowDef(fromScope *scope, def *ast.WindowDef) *sql.WindowDefinition {
	if def == nil {
		return nil
	}

	var sortFields sql.SortFields
	for _, c := range def.OrderBy {
		// resolve col in fromScope
		e := b.buildScalar(fromScope, c.Expr)
		so := sql.Ascending
		if c.Direction == ast.DescScr {
			so = sql.Descending
		}
		sf := sql.SortField{
			Column: e,
			Order:  so,
		}
		sortFields = append(sortFields, sf)
	}

	partitions := make([]sql.Expression, len(def.PartitionBy))
	for i, expr := range def.PartitionBy {
		partitions[i] = b.buildScalar(fromScope, expr)
	}

	frame := b.NewFrame(fromScope, def.Frame)

	// According to MySQL documentation at https://dev.mysql.com/doc/refman/8.0/en/window-functions-usage.html
	// "If OVER() is empty, the window consists of all query rows and the window function computes a result using all rows."
	if def.OrderBy == nil && frame == nil {
		frame = plan.NewRowsUnboundedPrecedingToUnboundedFollowingFrame()
	}

	windowDef := sql.NewWindowDefinition(partitions, sortFields, frame, def.NameRef.Lowered(), def.Name.Lowered())
	if ref, ok := fromScope.windowDefs[def.NameRef.Lowered()]; ok {
		// this is only safe if windows are built in topo order
		windowDef = b.mergeWindowDefs(windowDef, ref)
		// collapse dependencies if any reference this window
		fromScope.windowDefs[windowDef.Name] = windowDef
	}
	return windowDef
}

// mergeWindowDefs combines the attributes of two window definitions or returns
// an error if the two are incompatible. [def] should have a reference to
// [ref] through [def.Ref], and the return value drops the reference to indicate
// the two were properly combined.
func (b *Builder) mergeWindowDefs(def, ref *sql.WindowDefinition) *sql.WindowDefinition {
	if ref.Ref != "" {
		panic("unreachable; cannot merge unresolved window definition")
	}

	var orderBy sql.SortFields
	switch {
	case len(def.OrderBy) > 0 && len(ref.OrderBy) > 0:
		err := sql.ErrInvalidWindowInheritance.New("", "", "both contain order by clause")
		b.handleErr(err)
	case len(def.OrderBy) > 0:
		orderBy = def.OrderBy
	case len(ref.OrderBy) > 0:
		orderBy = ref.OrderBy
	default:
	}

	var partitionBy []sql.Expression
	switch {
	case len(def.PartitionBy) > 0 && len(ref.PartitionBy) > 0:
		err := sql.ErrInvalidWindowInheritance.New("", "", "both contain partition by clause")
		b.handleErr(err)
	case len(def.PartitionBy) > 0:
		partitionBy = def.PartitionBy
	case len(ref.PartitionBy) > 0:
		partitionBy = ref.PartitionBy
	default:
		partitionBy = []sql.Expression{}
	}

	var frame sql.WindowFrame
	switch {
	case def.Frame != nil && ref.Frame != nil:
		_, isDefDefaultFrame := def.Frame.(*plan.RowsUnboundedPrecedingToUnboundedFollowingFrame)
		_, isRefDefaultFrame := ref.Frame.(*plan.RowsUnboundedPrecedingToUnboundedFollowingFrame)

		// if both frames are set and one is RowsUnboundedPrecedingToUnboundedFollowingFrame (default),
		// we should use the other frame
		if isDefDefaultFrame {
			frame = ref.Frame
		} else if isRefDefaultFrame {
			frame = def.Frame
		} else {
			// if both frames have identical string representations, use either one
			df := def.Frame.String()
			rf := ref.Frame.String()
			if df != rf {
				err := sql.ErrInvalidWindowInheritance.New("", "", "both contain different frame clauses")
				b.handleErr(err)
			}
			frame = def.Frame
		}
	case def.Frame != nil:
		frame = def.Frame
	case ref.Frame != nil:
		frame = ref.Frame
	default:
	}

	return sql.NewWindowDefinition(partitionBy, orderBy, frame, "", def.Name)
}

func (b *Builder) analyzeHaving(fromScope, projScope *scope, having *ast.Where) {
	// build having filter expr
	// aggregates added to fromScope.groupBy
	// can see projScope outputs
	if having == nil {
		return
	}
	savedFrom, savedProj := b.havingFrom, b.havingProj
	b.havingFrom, b.havingProj = fromScope, projScope
	defer func() { b.havingFrom, b.havingProj = savedFrom, savedProj }()

	// HeimDB: a name that no column of the FROM has and that names two
	// different select aliases is ambiguous (1052), as in MySQL
	checkName := func(n *ast.ColName) {
		if !n.Qualifier.IsEmpty() {
			return
		}
		colName := strings.ToLower(n.Name.String())
		if _, ok := fromScope.resolveColumn("", "", colName, true, true); !ok {
			b.checkHavingAliasAmbiguity(projScope, colName)
		}
	}

	ast.Walk(func(node ast.SQLNode) (bool, error) {
		switch n := node.(type) {
		case *ast.Subquery:
			return false, nil
		case *ast.FuncExpr:
			name := n.Name.Lowered()
			if isAggregateFunc(name) {
				// record aggregate
				// TODO: this should get projScope as well
				_ = b.buildAggregateFunc(fromScope, name, n)
			} else if isWindowFunc(name) {
				_ = b.buildWindowFunc(fromScope, name, n, (*ast.WindowDef)(n.Over))
			}
		case *ast.GroupConcatExpr:
			// HeimDB: record GROUP_CONCAT like the other aggregates, so a HAVING
			// that alone uses GROUP_CONCAT(m.meta_key) sees m.meta_key (it failed
			// with 1146, table not found: m)
			ast.Walk(func(node ast.SQLNode) (bool, error) {
				switch c := node.(type) {
				case *ast.Subquery:
					return false, nil
				case *ast.ColName:
					checkName(c)
				}
				return true, nil
			}, n)
			_ = b.buildGroupConcat(fromScope, n)
			return false, nil
		case *ast.ColName:
			checkName(n)
			// add to extra cols
			dbName := strings.ToLower(n.Qualifier.DbQualifier.String())
			tblName := strings.ToLower(n.Qualifier.Name.String())
			colName := strings.ToLower(n.Name.String())
			c, ok := fromScope.resolveColumn(dbName, tblName, colName, true, false)
			if ok {
				c.scalar = expression.NewGetFieldWithTable(int(c.id), 0, c.typ, c.db, c.table, c.col, c.nullable)
				fromScope.addExtraColumn(c)
				break
			}
			c, ok = projScope.resolveColumn(dbName, tblName, colName, false, true)
			if ok {
				// references projection alias
				break
			}
			err := sql.ErrColumnNotFound.New(n.Name)
			b.handleErr(err)
		}
		return true, nil
	}, having.Expr)
}

func (b *Builder) buildInnerProj(fromScope, projScope *scope) *scope {
	outScope := fromScope
	var proj []sql.Expression

	// eval aliases in project scope
	for _, col := range projScope.cols {
		switch e := col.scalar.(type) {
		case *expression.Alias:
			if !e.Unreferencable() {
				proj = append(proj, e.WithId(sql.ColumnId(col.id)).(*expression.Alias))
			}
		}
	}

	aliasCnt := len(proj)

	if len(proj) == 0 && !(len(fromScope.cols) == 1 && fromScope.cols[0].id == 0) {
		// remove redundant projection unless it is the single dual table column
		return outScope
	}

	for _, c := range fromScope.cols {
		proj = append(proj, c.scalarGf())
	}

	// todo: fulltext indexes depend on match alias first
	proj = append(proj[aliasCnt:], proj[:aliasCnt]...)

	if len(proj) > 0 {
		outScope.node = plan.NewProject(proj, outScope.node)
	}

	return outScope
}

// isFieldItem は、SELECT の項が表の列そのもの（別名の付いた列も）か（HeimDB、段階 116 のレビュー）。集計の別名は、
// 集計の出力を読む表のない GetField を包む。
func isFieldItem(e sql.Expression) bool {
	if a, ok := e.(*expression.Alias); ok {
		e = a.Child
	}
	gf, ok := e.(*expression.GetField)
	return ok && gf.Table() != ""
}

func (b *Builder) buildHaving(fromScope, projScope, outScope *scope, having *ast.Where) {
	// expressions in having can be from aggOut or projScop
	if having == nil {
		return
	}
	if fromScope.groupBy == nil {
		fromScope.initGroupBy()
	}

	havingScope := b.newScope()
	if fromScope.parent != nil {
		havingScope.parent = fromScope.parent
		// HeimDB: lend the select aliases to the parent scope only while the
		// HAVING is built (upstream #3073 leaves them there, where an outer
		// query's names could later resolve to this query's aliases)
		saved := havingScope.parent.selectAliases
		havingScope.parent.selectAliases = fromScope.selectAliases
		defer func() { havingScope.parent.selectAliases = saved }()
	}

	// HeimDB: a GROUP BY column's name comes first, then the select list's, so a
	// HAVING name that is both a select alias and a column that is not grouped
	// (SUM(v) AS v ... GROUP BY k HAVING v > 35) is the alias, as in MySQL
	// (stage 116; it was the column)
	for _, c := range fromScope.groupBy.inCols {
		if fromScope.groupBy.grouping[strings.ToLower(c.col)] && !havingScope.colset.Contains(sql.ColumnId(c.id)) {
			havingScope.addColumn(c)
		}
		// HeimDB: GROUP BY a, where a is a select alias of a column, groups by the
		// column, so a HAVING name of the column is the column, as in MySQL (SELECT
		// post_author AS a, COUNT(*) AS post_author ... GROUP BY a HAVING
		// post_author > 1; stage 116 review)
		if a, ok := c.scalar.(*expression.Alias); ok && fromScope.groupBy.grouping[strings.ToLower(c.col)] {
			if gf, ok := a.Child.(*expression.GetField); ok && gf.Table() != "" {
				col, found := fromScope.resolveColumn(gf.Database(), gf.Table(), gf.Name(), false, false)
				if found && !havingScope.colset.Contains(sql.ColumnId(col.id)) {
					havingScope.addColumn(col)
				}
			}
		}
	}
	// HeimDB: in the select list, an item that is not a column comes before a
	// column of the same name, as MySQL's find_item_in_list stops at it (SELECT
	// p.comment_count, COUNT(c.ID) AS comment_count ... HAVING comment_count > 0
	// is the COUNT; stage 116 review)
	for _, fields := range []bool{false, true} {
		for _, c := range projScope.cols {
			if isFieldItem(c.scalar) == fields && !havingScope.colset.Contains(sql.ColumnId(c.id)) {
				havingScope.addColumn(c)
			}
		}
	}

	// add columns from fromScope referenced in the groupBy
	for _, c := range fromScope.groupBy.inCols {
		if !havingScope.colset.Contains(sql.ColumnId(c.id)) {
			havingScope.addColumn(c)
		}
	}

	// add columns from fromScope referenced in any aggregate expressions
	for _, c := range fromScope.groupBy.aggregations() {
		transform.InspectExpr(c.scalar, func(e sql.Expression) bool {
			switch e := e.(type) {
			case *expression.GetField:
				// 列は名前だけでなく表の修飾子でも探す（p1.id と p2.id を取り違えない。上流の #3739）
				col, found := fromScope.resolveColumn(e.Database(), e.Table(), e.Name(), false, false)
				if found && !havingScope.colset.Contains(sql.ColumnId(col.id)) {
					havingScope.addColumn(col)
				}
			}
			return false
		})
	}

	// Add columns from projScope referenced in any aggregate expressions, that are not already in the havingScope
	// This prevents aliases with the same name from overriding columns in the fromScope
	// Additionally, the original name from plain aliases (not expressions) are added to havingScope
	for _, c := range projScope.cols {
		if !havingScope.colset.Contains(sql.ColumnId(c.id)) {
			havingScope.addColumn(c)
		}
		// The unaliased column is allowed in having clauses regardless if it is just an aliased getfield and not an expression
		alias, isAlias := c.scalar.(*expression.Alias)
		if !isAlias {
			continue
		}
		gf, isGetField := alias.Child.(*expression.GetField)
		if !isGetField {
			continue
		}
		col, found := fromScope.resolveColumn(gf.Database(), gf.Table(), gf.Name(), false, false)
		if found && !havingScope.colset.Contains(sql.ColumnId(col.id)) {
			havingScope.addColumn(col)
		}
	}

	havingScope.groupBy = fromScope.groupBy
	savedFrom, savedProj := b.havingFrom, b.havingProj
	b.havingFrom, b.havingProj = fromScope, projScope
	defer func() { b.havingFrom, b.havingProj = savedFrom, savedProj }()
	h := b.buildScalar(havingScope, having.Expr)
	outScope.node = plan.NewHaving(h, outScope.node)
	return
}
