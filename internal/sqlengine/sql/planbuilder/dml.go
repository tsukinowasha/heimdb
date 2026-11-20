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
// Modified by Tsukinowa Inc.: サーバのファイルを読み書きする SQL を外した（docs/engine.md）
// Modified by Tsukinowa Inc.: INSERT ... ON DUPLICATE KEY UPDATE に UPDATE の権限を求める（上流の #3940。docs/engine.md）
// Modified by Tsukinowa Inc.: UPDATE と ON DUPLICATE KEY UPDATE の ON UPDATE の列の式に派生の印を付け、列を名前から引く（上流の #3468 を小さく、#3461、#3643。docs/engine.md）
// Modified by Tsukinowa Inc.: UPDATE JOIN の ON UPDATE と生成の列の式を、SET で書く表の列だけに付ける（docs/engine.md）
// Modified by Tsukinowa Inc.: 1 つの表の DELETE の消す表を、WHERE の前の表で持つ（上流の #3318。docs/engine.md）
// Modified by Tsukinowa Inc.: UPDATE と DELETE の副問い合わせが対象の表を読めば 1093 にする（段階 116。docs/engine.md）

package planbuilder

import (
	"fmt"
	"strings"

	"github.com/dolthub/vitess/go/mysql"
	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
)

const OnDupValuesPrefix = "__new_ins"

func (b *Builder) buildInsert(inScope *scope, i *ast.Insert) (outScope *scope) {
	// TODO: this shouldn't be called during ComPrepare or `PREPARE ... FROM ...` statements, but currently it is.
	//   The end result is that the ComDelete counter is incremented during prepare statements, which is incorrect.
	sql.IncrementStatusVariable(b.ctx, "Com_insert", 1)
	b.qFlags.Set(sql.QFlagInsert)

	if err := b.cat.AuthorizationHandler().HandleAuth(b.ctx, b.authQueryState, i.Auth); err != nil && b.authEnabled {
		b.handleErr(err)
	}
	// An upsert requires UPDATE even when none of its candidate rows conflict.
	if len(i.OnDup) > 0 {
		updateAuth := i.Auth
		updateAuth.AuthType = ast.AuthType_UPDATE
		if err := b.cat.AuthorizationHandler().HandleAuth(b.ctx, b.authQueryState, updateAuth); err != nil && b.authEnabled {
			b.handleErr(err)
		}
	}
	if i.With != nil {
		inScope = b.buildWith(inScope, i.With)
	}

	destScope, ok := b.buildResolvedTableForTablename(inScope, i.Table, nil)
	if !ok {
		b.handleErr(sql.ErrTableNotFound.New(i.Table.Name.String()))
	}
	var db sql.Database
	var rt *plan.ResolvedTable
	switch n := destScope.node.(type) {
	case *plan.ResolvedTable:
		rt = n
		db = rt.SqlDatabase
	case *plan.UnresolvedTable:
		db = n.Database()
	default:
		b.handleErr(fmt.Errorf("expected insert destination to be resolved or unresolved table"))
	}
	if rt == nil {
		if b.TriggerCtx().Active && !b.TriggerCtx().Call {
			b.TriggerCtx().UnresolvedTables = append(b.TriggerCtx().UnresolvedTables, i.Table.Name.String())
		} else {
			err := fmt.Errorf("expected resolved table: %s", i.Table.Name.String())
			b.handleErr(err)
		}
	}
	isReplace := i.Action == ast.ReplaceStr

	var columns []string
	{
		columns = columnsToStrings(i.Columns)
		// If no column names were specified in the query, go ahead and fill
		// them all in now that the destination is resolved.
		// TODO: setting the plan field directly is not great
		if len(columns) == 0 && len(destScope.cols) > 0 && rt != nil {
			schema := rt.Schema()
			columns = make([]string, len(schema))
			for i, col := range schema {
				// Tables with any generated column must always supply a column list, so this is always an error
				if col.Generated != nil {
					b.handleErr(sql.ErrGeneratedColumnValue.New(col.Name, rt.Name()))
				}
				columns[i] = col.Name
			}
		}
	}
	sch := destScope.node.Schema()
	if rt != nil {
		sch = b.resolveSchemaDefaults(destScope, rt.Schema())
	}

	insertRows := i.Rows

	// We need to give a table name to the scope of the values being inserted. We use the row alias if provided, otherwise go with a default.
	// If the row alias also provided column names, we create a map from the destination names to the aliases. This will allow us to
	// rewrite VALUES() function expressions to use the new column names.
	inScope.insertTableAlias = OnDupValuesPrefix
	if aliasedValues, ok := insertRows.(*ast.AliasedValues); ok {
		valueTableName := aliasedValues.As.String()
		if valueTableName != "" {
			inScope.insertTableAlias = valueTableName
		}
		if len(aliasedValues.Columns) > 0 {
			inScope.insertColumnAliases = make(map[string]string)
			for i, destColumn := range columns {
				sourceColumn := aliasedValues.Columns[i].Lowered()
				inScope.insertColumnAliases[destColumn] = sourceColumn
			}
		}
	}

	srcScope, srcLiteralOnly := b.insertRowsToNode(inScope, insertRows, columns, i.Table.Name.String(), sch)

	var onDupExprs []sql.Expression
	if len(i.OnDup) > 0 {
		// TODO: on duplicate expressions need to reference both VALUES and
		//  derived columns equally in ON DUPLICATE UPDATE expressions.
		combinedScope := inScope.replace()
		combinedScope.insertTableAlias = inScope.insertTableAlias
		combinedScope.insertColumnAliases = inScope.insertColumnAliases
		for i, c := range destScope.cols {
			combinedScope.newColumn(c)
			// if the srcScope is empty, it is a values statement
			if len(srcScope.cols) == 0 {
				// The to-be-inserted values can be referenced via the provided alias.
				c.table = combinedScope.insertTableAlias
				if len(combinedScope.insertColumnAliases) > 0 {
					aliasColumnName := combinedScope.insertColumnAliases[c.col]
					c.col = aliasColumnName
					c.originalCol = aliasColumnName
				}
				combinedScope.newColumn(c)
				continue
			}
			if i < len(srcScope.cols) {
				combinedScope.newColumn(srcScope.cols[i])
			}
		}
		onDupExprs = b.buildOnDupUpdateExprs(combinedScope, destScope, sch, ast.AssignmentExprs(i.OnDup))
	}

	ignore := false
	// TODO: make this a bool in vitess
	if strings.Contains(strings.ToLower(i.Ignore), "ignore") {
		ignore = true
	}

	dest := destScope.node

	ins := plan.NewInsertInto(db, plan.NewInsertDestination(sch, dest), srcScope.node, isReplace, columns, onDupExprs, ignore)
	ins.LiteralValueSource = srcLiteralOnly

	if i.Returning != nil {
		returningExprs := make([]sql.Expression, len(i.Returning))
		for i, selectExpr := range i.Returning {
			returningExprs[i] = b.selectExprToExpression(destScope, selectExpr)
		}
		ins.Returning = returningExprs
	}

	b.validateInsert(ins)

	outScope = destScope
	outScope.node = ins
	if rt != nil {
		checks := b.loadChecksFromTable(destScope, rt.Table)
		outScope.node = ins.WithChecks(checks)
	}

	return
}

func (b *Builder) insertRowsToNode(inScope *scope, ir ast.InsertRows, columnNames []string, tableName string, destSchema sql.Schema) (outScope *scope, literalOnly bool) {
	switch v := ir.(type) {
	case ast.SelectStatement:
		outScope = b.buildSelectStmt(inScope, v)
	case *ast.AliasedValues:
		outScope, literalOnly = b.buildInsertValues(inScope, v, columnNames, tableName, destSchema)
	default:
		err := sql.ErrUnsupportedSyntax.New(ast.String(ir))
		b.handleErr(err)
	}
	return
}

func (b *Builder) buildInsertValues(inScope *scope, v *ast.AliasedValues, columnNames []string, tableName string, destSchema sql.Schema) (outScope *scope, literalOnly bool) {
	columnDefaultValues := make([]*sql.ColumnDefaultValue, len(columnNames))

	for i, columnName := range columnNames {
		index := destSchema.IndexOfColName(columnName)
		if index == -1 {
			if !b.TriggerCtx().Call && len(b.TriggerCtx().UnresolvedTables) > 0 {
				continue
			}
			err := sql.ErrUnknownColumn.New(columnName, tableName)
			b.handleErr(err)
		}

		columnDefaultValues[i] = destSchema[index].Default
		if columnDefaultValues[i] == nil && destSchema[index].Generated != nil {
			columnDefaultValues[i] = destSchema[index].Generated
		}
	}

	if !v.As.IsEmpty() {
		if len(v.Columns) != 0 {
			for _, tuple := range v.Values {
				if len(v.Columns) != len(tuple) {
					err := sql.ErrColumnCountMismatch.New()
					b.handleErr(err)
				}
			}
		}
	}

	literalOnly = true
	exprTuples := make([][]sql.Expression, len(v.Values))
	for i, vt := range v.Values {
		// noExprs is an edge case where we fill VALUES with nil expressions
		noExprs := len(vt) == 0
		// triggerUnknownTable is an edge case where we ignored an unresolved
		// table error and do not have a schema for resolving defaults
		triggerUnknownTable := (len(columnNames) == 0 && len(vt) > 0) && (len(b.TriggerCtx().UnresolvedTables) > 0)

		if len(vt) != len(columnNames) && !noExprs && !triggerUnknownTable {
			err := sql.ErrInsertIntoMismatchValueCount.New()
			b.handleErr(err)
		}
		exprs := make([]sql.Expression, len(columnNames))
		exprTuples[i] = exprs
		for j := range columnNames {
			if noExprs || triggerUnknownTable {
				exprs[j] = expression.WrapExpression(columnDefaultValues[j])
				continue
			}
			e := vt[j]
			switch e := e.(type) {
			case *ast.Default:
				exprs[j] = expression.WrapExpression(columnDefaultValues[j])
				// explicit DEFAULT values need their column indexes assigned early, since we analyze the insert values in
				// isolation (no access to the destination schema)
				exprs[j] = assignColumnIndexes(exprs[j], reorderSchema(columnNames, destSchema))
			case *ast.SQLVal:
				// In the case of an unknown bindvar, give it a target type of the column it's targeting.
				// We only do this for simple bindvars in tuples, not expressions that contain bindvars.
				if b.shouldAssignBindvarType(e) {
					name := strings.TrimPrefix(string(e.Val), ":")
					bindVar := expression.NewBindVar(name)
					bindVar.Typ = reorderSchema(columnNames, destSchema)[j].Type
					exprs[j] = bindVar
				} else {
					exprs[j] = b.buildScalar(inScope, e)
				}
			default:
				literalOnly = false
				exprs[j] = b.buildScalar(inScope, e)
			}
		}
	}

	outScope = inScope.push()
	outScope.node = plan.NewValuesWithAlias(v.As.String(), inScope.insertColumnAliases, exprTuples)
	return
}

func (b *Builder) shouldAssignBindvarType(e *ast.SQLVal) bool {
	return e.Type == ast.ValArg && (b.bindCtx == nil || b.bindCtx.resolveOnly)
}

// reorderSchema returns the schemas columns in the order specified by names
func reorderSchema(names []string, schema sql.Schema) sql.Schema {
	newSch := make(sql.Schema, len(names))
	for i, name := range names {
		newSch[i] = schema[schema.IndexOfColName(name)]
	}
	return newSch
}

func (b *Builder) assignmentExprsToExpressions(inScope *scope, e ast.AssignmentExprs) []sql.Expression {
	updateExprs := make([]sql.Expression, len(e))
	var startAggCnt int
	if inScope.groupBy != nil {
		startAggCnt = len(inScope.groupBy.aggs)
	}
	var startWinCnt int
	if inScope.windowFuncs != nil {
		startWinCnt = len(inScope.windowFuncs)
	}

	tableSch := b.resolveSchemaDefaults(inScope, inScope.node.Schema())

	for i, updateExpr := range e {
		colName := b.buildScalar(inScope, updateExpr.Name)

		innerExpr := b.buildScalar(inScope, updateExpr.Expr)
		if gf, ok := colName.(*expression.GetField); ok {
			colIdx := tableSch.IndexOfColName(gf.Name())
			// TODO: during trigger parsing the table in the node is unresolved, so we need this additional bounds check
			//  This means that trigger execution will be able to update generated columns
			// Prevent update of generated columns
			if colIdx >= 0 && tableSch[colIdx].Generated != nil {
				err := sql.ErrGeneratedColumnValue.New(tableSch[colIdx].Name, inScope.node.(sql.NameableNode).Name())
				b.handleErr(err)
			}

			// Replace default with column default from resolved schema
			if _, ok := updateExpr.Expr.(*ast.Default); ok {
				if colIdx >= 0 {
					innerExpr = expression.WrapExpression(tableSch[colIdx].Default)
				}
			}
		}

		// In the case of an unknown bindvar, give it a target type of the column it's targeting.
		// We only do this for simple bindvars in tuples, not expressions that contain bindvars.
		if innerSqlVal, ok := updateExpr.Expr.(*ast.SQLVal); ok && b.shouldAssignBindvarType(innerSqlVal) {
			if typ, ok := hasColumnType(colName); ok {
				rightBindVar := innerExpr.(*expression.BindVar)
				rightBindVar.Typ = typ
				innerExpr = rightBindVar
			}
		}

		updateExprs[i] = expression.NewSetField(colName, innerExpr)
		if inScope.groupBy != nil {
			if len(inScope.groupBy.aggs) > startAggCnt {
				err := sql.ErrAggregationUnsupported.New(updateExprs[i])
				b.handleErr(err)
			}
		}
		if inScope.windowFuncs != nil {
			if len(inScope.windowFuncs) > startWinCnt {
				err := sql.ErrWindowUnsupported.New(updateExprs[i])
				b.handleErr(err)
			}
		}
	}

	return b.addDependentUpdateExprs(inScope, tableSch, updateExprs)
}

// addDependentUpdateExprs adds update expressions for any generated columns and ON UPDATE expressions since their
// values still need to be updated despite not being part of an explicit update expression. ON UPDATE expressions are
// derived (expression.NewDerivedSetField): they apply only when the explicit expressions change the row. The column is
// built from its name, so its index follows the scope (a WITH clause or the inserted row can come before it).
func (b *Builder) addDependentUpdateExprs(inScope *scope, schema sql.Schema, updateExprs []sql.Expression) []sql.Expression {
	// HeimDB: in an UPDATE JOIN (or an UPDATE whose subquery became a join) only
	// the tables that the SET clause names get their generated and ON UPDATE
	// columns; the other tables' rows are not updated (stage 115)
	targets := explicitUpdateTables(updateExprs)
	for _, col := range schema {
		if len(targets) > 0 && col.Source != "" && !targets[strings.ToLower(col.Source)] {
			continue
		}
		if col.Generated != nil {
			generated := b.resolveColumnDefaultExpression(inScope, col, col.Generated)
			colExpr := b.buildColumnExpr(inScope, col.Name, col.Source, col.DatabaseSource)
			updateExprs = append(updateExprs, expression.NewSetField(colExpr, assignColumnIndexes(generated, schema)))
		} else if col.OnUpdate != nil {
			// don't add if column is already being updated
			if !isColumnUpdated(col, updateExprs) {
				onUpdate := b.resolveColumnDefaultExpression(inScope, col, col.OnUpdate)
				colExpr := b.buildColumnExpr(inScope, col.Name, col.Source, col.DatabaseSource)
				updateExprs = append(updateExprs, expression.NewDerivedSetField(colExpr, assignColumnIndexes(onUpdate, schema)))
			}
		}
	}
	return updateExprs
}

func isColumnUpdated(col *sql.Column, updateExprs []sql.Expression) bool {
	for _, expr := range updateExprs {
		sf, ok := expr.(*expression.SetField)
		if !ok {
			continue
		}
		gf, ok := sf.LeftChild.(*expression.GetField)
		if !ok {
			continue
		}
		// HeimDB: the column of the same table (j1.u and j2.u are different
		// columns; j2.u lost its ON UPDATE once j1.u had one)
		if strings.EqualFold(gf.Name(), col.Name) && (gf.Table() == "" || col.Source == "" || strings.EqualFold(gf.Table(), col.Source)) {
			return true
		}
	}
	return false
}

// explicitUpdateTables returns the tables (lower case) whose columns the SET
// clause updates (HeimDB).
func explicitUpdateTables(updateExprs []sql.Expression) map[string]bool {
	ret := map[string]bool{}
	for _, expr := range updateExprs {
		sf, ok := expr.(*expression.SetField)
		if !ok {
			continue
		}
		if gf, ok := sf.LeftChild.(*expression.GetField); ok && gf.Table() != "" {
			ret[strings.ToLower(gf.Table())] = true
		}
	}
	return ret
}

func (b *Builder) buildOnDupUpdateExprs(combinedScope, destScope *scope, schema sql.Schema, e ast.AssignmentExprs) []sql.Expression {
	b.insertActive = true
	defer func() {
		b.insertActive = false
	}()
	res := make([]sql.Expression, len(e))
	// todo(max): prevent aggregations in separate semantic walk step
	var startAggCnt int
	if combinedScope.groupBy != nil {
		startAggCnt = len(combinedScope.groupBy.aggs)
	}
	var startWinCnt int
	if combinedScope.windowFuncs != nil {
		startWinCnt = len(combinedScope.windowFuncs)
	}
	for i, updateExpr := range e {
		colName := b.buildOnDupLeft(destScope, updateExpr.Name)
		innerExpr := b.buildScalar(combinedScope, updateExpr.Expr)

		res[i] = expression.NewSetField(colName, innerExpr)
		if combinedScope.groupBy != nil {
			if len(combinedScope.groupBy.aggs) > startAggCnt {
				err := sql.ErrAggregationUnsupported.New(res[i])
				b.handleErr(err)
			}
		}
		if combinedScope.windowFuncs != nil {
			if len(combinedScope.windowFuncs) > startWinCnt {
				err := sql.ErrWindowUnsupported.New(res[i])
				b.handleErr(err)
			}
		}
	}
	return b.addDependentUpdateExprs(destScope, schema, res)
}

func (b *Builder) buildOnDupLeft(inScope *scope, e ast.Expr) sql.Expression {
	// expect col reference only
	switch e := e.(type) {
	case *ast.ColName:
		dbName := strings.ToLower(e.Qualifier.DbQualifier.String())
		tblName := strings.ToLower(e.Qualifier.Name.String())
		colName := strings.ToLower(e.Name.String())
		c, ok := inScope.resolveColumn(dbName, tblName, colName, true, false)
		if !ok {
			if tblName != "" && !inScope.hasTable(tblName) {
				b.handleErr(sql.ErrTableNotFound.New(tblName))
			} else if tblName != "" {
				b.handleErr(sql.ErrTableColumnNotFound.New(tblName, colName))
			}
			b.handleErr(sql.ErrColumnNotFound.New(e))
		}
		return c.scalarGf()
	default:
		err := fmt.Errorf("invalid update target; expected column reference, found: %T", e)
		b.handleErr(err)
	}
	return nil
}

func (b *Builder) buildDelete(inScope *scope, d *ast.Delete) (outScope *scope) {
	// TODO: this shouldn't be called during ComPrepare or `PREPARE ... FROM ...` statements, but currently it is.
	//   The end result is that the ComDelete counter is incremented during prepare statements, which is incorrect.
	sql.IncrementStatusVariable(b.ctx, "Com_delete", 1)
	b.qFlags.Set(sql.QFlagDelete)

	if len(d.Targets) == 0 {
		b.checkTargetInSubquery(d.TableExprs, d.Where)
	}
	outScope = b.buildFrom(inScope, d.TableExprs)

	// Capture the table node for simple DELETEs before buildWhere wraps it
	var targets []sql.Node
	var hasExplicitTargets bool
	if len(d.Targets) == 0 {
		targets = []sql.Node{outScope.node}
	}

	b.buildWhere(outScope, d.Where)
	orderByScope := b.analyzeOrderBy(outScope, outScope, d.OrderBy)
	b.buildOrderBy(outScope, orderByScope)
	offset := b.buildOffset(outScope, d.Limit)
	if offset != nil {
		outScope.node = plan.NewOffset(offset, outScope.node)
	}
	limit := b.buildLimit(outScope, d.Limit)
	if limit != nil {
		outScope.node = plan.NewLimit(limit, outScope.node)
	}

	if len(d.Targets) > 0 {
		hasExplicitTargets = true
		targets = make([]sql.Node, len(d.Targets))
		for i, tableName := range d.Targets {
			tabName := tableName.Name.String()
			var target sql.Node
			if _, ok := outScope.tables[tabName]; ok {
				transform.InspectUp(outScope.node, func(n sql.Node) bool {
					switch n := n.(type) {
					case sql.NameableNode:
						if strings.EqualFold(n.Name(), tabName) {
							target = n
							return true
						}
					default:
					}
					return false
				})
			} else {
				tableScope, ok := b.buildResolvedTableForTablename(inScope, tableName, nil)
				if !ok {
					b.handleErr(sql.ErrTableNotFound.New(tabName))
				}
				target = tableScope.node
			}
			targets[i] = target
		}
	}

	del := plan.NewDeleteFrom(outScope.node, targets, hasExplicitTargets)
	del.RefsSingleRel = !outScope.refsSubquery
	del.IsProcNested = b.ProcCtx().DbName != ""
	outScope.node = del
	return
}

func (b *Builder) buildUpdate(inScope *scope, u *ast.Update) (outScope *scope) {
	// TODO: this shouldn't be called during ComPrepare or `PREPARE ... FROM ...` statements, but currently it is.
	//   The end result is that the ComDelete counter is incremented during prepare statements, which is incorrect.
	sql.IncrementStatusVariable(b.ctx, "Com_update", 1)
	b.qFlags.Set(sql.QFlagUpdate)

	{
		var nodes []ast.SQLNode
		for _, e := range u.Exprs {
			nodes = append(nodes, e.Expr)
		}
		b.checkTargetInSubquery(u.TableExprs, u.Where, nodes...)
	}
	outScope = b.buildFrom(inScope, u.TableExprs)

	_, foundJoin := outScope.node.(*plan.JoinNode)

	// default expressions only resolve to target table
	updateExprs := b.assignmentExprsToExpressions(outScope, u.Exprs)

	b.buildWhere(outScope, u.Where)

	orderByScope := b.analyzeOrderBy(outScope, b.newScope(), u.OrderBy)

	b.buildOrderBy(outScope, orderByScope)
	offset := b.buildOffset(outScope, u.Limit)
	if offset != nil {
		outScope.node = plan.NewOffset(offset, outScope.node)
	}

	limit := b.buildLimit(outScope, u.Limit)
	if limit != nil {
		outScope.node = plan.NewLimit(limit, outScope.node)
	}

	// TODO comments
	// If the top level node can store comments and one was provided, store it.
	//if cn, ok := node.(sql.CommentedNode); ok && len(u.Comments) > 0 {
	//	node = cn.WithComment(string(u.Comments[0]))
	//}

	ignore := u.Ignore != ""
	update := plan.NewUpdate(outScope.node, ignore, updateExprs)

	update.IsJoin = foundJoin
	update.HasSingleRel = !outScope.refsSubquery
	update.IsProcNested = b.ProcCtx().DbName != ""

	var checks []*sql.CheckConstraint
	if join, ok := outScope.node.(*plan.JoinNode); ok {
		// TODO this doesn't work, a lot of the time the top node
		// is a filter. This would have to go before we build the
		// filter/accessory nodes. But that errors for a lot of queries.
		source := plan.NewUpdateSource(
			join,
			ignore,
			updateExprs,
		)
		updaters, err := rowUpdatersByTable(b.ctx, source, join)
		if err != nil {
			b.handleErr(err)
		}
		updateJoin := plan.NewUpdateJoin(updaters, source)
		update.Child = updateJoin
		transform.Inspect(update, func(n sql.Node) bool {
			// todo maybe this should be later stage
			switch n := n.(type) {
			case sql.NameableNode:
				if _, ok := updaters[n.Name()]; ok {
					rt := getResolvedTable(n)
					tableScope := inScope.push()
					for _, c := range rt.Schema() {
						tableScope.addColumn(scopeColumn{
							db:       rt.SqlDatabase.Name(),
							table:    strings.ToLower(n.Name()),
							tableId:  tableScope.tables[strings.ToLower(n.Name())],
							col:      strings.ToLower(c.Name),
							typ:      c.Type,
							nullable: c.Nullable,
						})
					}
					checks = append(checks, b.loadChecksFromTable(tableScope, rt.Table)...)
				}
			default:
			}
			return true
		})
	} else {
		transform.Inspect(update, func(n sql.Node) bool {
			// todo maybe this should be later stage
			if rt, ok := n.(*plan.ResolvedTable); ok {
				checks = append(checks, b.loadChecksFromTable(outScope, rt.Table)...)
			}
			return true
		})
	}

	if len(u.Returning) > 0 {
		returningExprs := make([]sql.Expression, len(u.Returning))
		for i, selectExpr := range u.Returning {
			returningExprs[i] = b.selectExprToExpression(outScope, selectExpr)
		}
		update.Returning = returningExprs
	}

	outScope.node = update.WithChecks(checks)
	return
}

// rowUpdatersByTable maps a set of tables to their RowUpdater objects.
func rowUpdatersByTable(ctx *sql.Context, node sql.Node, ij sql.Node) (map[string]sql.RowUpdater, error) {
	namesOfTableToBeUpdated := getTablesToBeUpdated(node)
	resolvedTables := getTablesByName(ij)

	rowUpdatersByTable := make(map[string]sql.RowUpdater)
	for tableToBeUpdated, _ := range namesOfTableToBeUpdated {
		resolvedTable, ok := resolvedTables[strings.ToLower(tableToBeUpdated)]
		if !ok {
			return nil, plan.ErrUpdateForTableNotSupported.New(tableToBeUpdated)
		}

		var table = resolvedTable.UnderlyingTable()

		// If there is no UpdatableTable for a table being updated, error out
		updatable, ok := table.(sql.UpdatableTable)
		if !ok && updatable == nil {
			return nil, plan.ErrUpdateForTableNotSupported.New(tableToBeUpdated)
		}

		keyless := sql.IsKeyless(updatable.Schema())
		if keyless {
			return nil, sql.ErrUnsupportedFeature.New("error: keyless tables unsupported for UPDATE JOIN")
		}

		rowUpdatersByTable[tableToBeUpdated] = updatable.Updater(ctx)
	}

	return rowUpdatersByTable, nil
}

// getTablesByName takes a node and returns all found resolved tables in a map.
func getTablesByName(node sql.Node) map[string]*plan.ResolvedTable {
	ret := make(map[string]*plan.ResolvedTable)

	transform.Inspect(node, func(node sql.Node) bool {
		switch n := node.(type) {
		case *plan.ResolvedTable:
			ret[strings.ToLower(n.Table.Name())] = n
		case *plan.IndexedTableAccess:
			rt, ok := n.TableNode.(*plan.ResolvedTable)
			if ok {
				ret[strings.ToLower(rt.Name())] = rt
			}
		case *plan.TableAlias:
			rt := getResolvedTable(n)
			if rt != nil {
				ret[strings.ToLower(n.Name())] = rt
			}
		default:
		}
		return true
	})

	return ret
}

// Finds first TableNode node that is a descendant of the node given
func getResolvedTable(node sql.Node) *plan.ResolvedTable {
	var table *plan.ResolvedTable
	transform.Inspect(node, func(node sql.Node) bool {
		// plan.Inspect will get called on all children of a node even if one of the children's calls returns false. We
		// only want the first TableNode match.
		if table != nil {
			return false
		}

		switch n := node.(type) {
		case *plan.ResolvedTable:
			if !plan.IsDualTable(n) {
				table = n
				return false
			}
		case *plan.IndexedTableAccess:
			rt, ok := n.TableNode.(*plan.ResolvedTable)
			if ok {
				table = rt
				return false
			}
		}
		return true
	})
	return table
}

// getTablesToBeUpdated takes a node and looks for the tables to modified by a SetField.
func getTablesToBeUpdated(node sql.Node) map[string]struct{} {
	ret := make(map[string]struct{})

	transform.InspectExpressions(node, func(e sql.Expression) bool {
		switch e := e.(type) {
		case *expression.SetField:
			gf := e.LeftChild.(*expression.GetField)
			ret[gf.Table()] = struct{}{}
			return false
		}

		return true
	})

	return ret
}

func (b *Builder) buildInto(inScope *scope, into *ast.Into) {
	// HeimDB: ファイルへの INTO（OUTFILE、DUMPFILE）は外した（sql.ErrServerFiles）。INTO @変数 は使える。
	if into.Dumpfile != "" || into.Outfile != "" {
		b.handleErr(sql.ErrServerFiles)
	}

	vars := make([]sql.Expression, len(into.Variables))
	for i, val := range into.Variables {
		if strings.HasPrefix(val.String(), "@") {
			vars[i] = expression.NewUserVar(strings.TrimPrefix(val.String(), "@"))
		} else {
			if inScope.proc == nil {
				err := sql.ErrExternalProcedureMissingContextParam.New(val.String())
				b.handleErr(err)
			}
			col, ok := inScope.proc.GetVar(val.String())
			if !ok {
				err := sql.ErrExternalProcedureMissingContextParam.New(val.String())
				b.handleErr(err)
			}
			vars[i] = col.scalarGf()
		}
	}
	inScope.node = plan.NewInto(inScope.node, vars, "", "")
}

func (b *Builder) loadChecksFromTable(inScope *scope, table sql.Table) []*sql.CheckConstraint {
	var loadedChecks []*sql.CheckConstraint
	if checkTable, ok := table.(sql.CheckTable); ok {
		checks, err := checkTable.GetChecks(b.ctx)
		if err != nil {
			b.handleErr(err)
		}
		for _, ch := range checks {
			constraint := b.buildCheckConstraint(inScope, &ch)
			loadedChecks = append(loadedChecks, constraint)
		}
	}
	return loadedChecks
}

func (b *Builder) buildCheckConstraint(inScope *scope, check *sql.CheckDefinition) *sql.CheckConstraint {
	parsed, err := b.parser.ParseSimple(fmt.Sprintf("select %s", check.CheckExpression))
	if err != nil {
		b.handleErr(err)
	}

	selectStmt, ok := parsed.(*ast.Select)
	if !ok || len(selectStmt.SelectExprs) != 1 {
		err := sql.ErrInvalidCheckConstraint.New(check.CheckExpression)
		b.handleErr(err)
	}

	expr := selectStmt.SelectExprs[0]
	ae, ok := expr.(*ast.AliasedExpr)
	if !ok {
		err := sql.ErrInvalidCheckConstraint.New(check.CheckExpression)
		b.handleErr(err)
	}

	c := b.buildScalar(inScope, ae.Expr)

	return &sql.CheckConstraint{
		Name:     check.Name,
		Expr:     c,
		Enforced: check.Enforced,
	}
}

// checkTargetInSubquery fails with MySQL's 1093 when the single target table of
// an UPDATE or DELETE is read by a subquery in its WHERE or SET (not inside a
// derived table, which MySQL materializes): UPDATE t SET ... WHERE id IN
// (SELECT id FROM t ...) changed rows that MySQL refuses to (HeimDB, stage 116).
func (b *Builder) checkTargetInSubquery(tables ast.TableExprs, where *ast.Where, others ...ast.SQLNode) {
	if len(tables) != 1 {
		return
	}
	ate, ok := tables[0].(*ast.AliasedTableExpr)
	if !ok {
		return
	}
	tn, ok := ate.Expr.(ast.TableName)
	if !ok {
		return
	}
	target := strings.ToLower(tn.Name.String())
	targetDb := strings.ToLower(tn.DbQualifier.String())
	if targetDb == "" {
		targetDb = strings.ToLower(b.ctx.GetCurrentDatabase())
	}
	var fromReads func(te ast.TableExpr) bool
	fromReads = func(te ast.TableExpr) bool {
		switch t := te.(type) {
		case *ast.AliasedTableExpr:
			if n, ok := t.Expr.(ast.TableName); ok {
				db := strings.ToLower(n.DbQualifier.String())
				if db == "" {
					db = strings.ToLower(b.ctx.GetCurrentDatabase())
				}
				return strings.ToLower(n.Name.String()) == target && db == targetDb
			}
		case *ast.JoinTableExpr:
			return fromReads(t.LeftExpr) || fromReads(t.RightExpr)
		case *ast.ParenTableExpr:
			for _, e := range t.Exprs {
				if fromReads(e) {
					return true
				}
			}
		}
		return false
	}
	found := false
	walk := func(node ast.SQLNode) (bool, error) {
		if found {
			return false, nil
		}
		if sel, ok := node.(*ast.Select); ok {
			for _, te := range sel.From {
				if fromReads(te) {
					found = true
					return false, nil
				}
			}
		}
		if ate, ok := node.(*ast.AliasedTableExpr); ok {
			if _, isSub := ate.Expr.(*ast.Subquery); isSub {
				// a derived table is materialized
				return false, nil
			}
		}
		return true, nil
	}
	if where != nil {
		_ = ast.Walk(walk, where.Expr)
	}
	for _, n := range others {
		_ = ast.Walk(walk, n)
	}
	if found {
		b.handleErr(mysql.NewSQLError(1093, "HY000", "You can't specify target table '%s' for update in FROM clause", tn.Name.String()))
	}
}
