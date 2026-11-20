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

// Modified by Tsukinowa Inc.: 表名を小文字にして結合の行の表を引く（上流の #3000、#3406。docs/engine.md）
// Modified by Tsukinowa Inc.: 明示の式で行が変わらなければ ON UPDATE の列の式を当てない（上流の #3468 を小さく。docs/engine.md）
// Modified by Tsukinowa Inc.: UPDATE JOIN の ON UPDATE の列を、表ごとにその表の行が変わったかで進める（docs/engine.md）
// Modified by Tsukinowa Inc.: 副問い合わせのある UPDATE で、主キーのない表の同じ値の行をまとめない（上流の #3052 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: UPDATE JOIN の LEFT JOIN の向きの確かめで、反結合から作った LEFT JOIN（右の列を落とす形）では結合の条件を見ない（上流の #3455 の代わり。docs/engine.md）
// Modified by Tsukinowa Inc.: UPDATE JOIN の NULL で埋めた行の確かめを、結合の条件を見ずに、木のどこにある外部結合でも NULL で埋められる側の表の行がすべて NULL かで行う（docs/engine.md）
// Modified by Tsukinowa Inc.: STRICT の UPDATE の数の列の範囲の外を 1264 にする（段階 116。docs/engine.md）

package rowexec

import (
	"errors"
	"strings"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type updateIter struct {
	childIter    sql.RowIter
	schema       sql.Schema
	updater      sql.RowUpdater
	checks       sql.CheckConstraints
	closed       bool
	ignore       bool
	returnExprs  []sql.Expression
	returnSchema sql.Schema
}

func (u *updateIter) Next(ctx *sql.Context) (sql.Row, error) {
	oldAndNewRow, err := u.childIter.Next(ctx)
	if err != nil {
		return nil, err
	}

	oldRow, newRow := oldAndNewRow[:len(oldAndNewRow)/2], oldAndNewRow[len(oldAndNewRow)/2:]
	if equals, err := oldRow.Equals(ctx, newRow, u.schema); err == nil {
		if !equals {
			// apply check constraints
			for _, check := range u.checks {
				if !check.Enforced {
					continue
				}

				res, err := sql.EvaluateCondition(ctx, check.Expr, newRow)
				if err != nil {
					return nil, err
				}

				if sql.IsFalse(res) {
					return nil, u.ignoreOrError(ctx, newRow, sql.ErrCheckConstraintViolated.New(check.Name))
				}
			}

			err := u.validateNullability(ctx, newRow, u.schema)
			if err != nil {
				return nil, u.ignoreOrError(ctx, newRow, err)
			}

			err = u.updater.Update(ctx, oldRow, newRow)
			if err != nil {
				return nil, u.ignoreOrError(ctx, newRow, err)
			}
		}

		if len(u.returnExprs) > 0 {
			var retExprRow sql.Row
			for _, returnExpr := range u.returnExprs {
				result, err := returnExpr.Eval(ctx, newRow)
				if err != nil {
					return nil, err
				}
				retExprRow = append(retExprRow, result)
			}
			return retExprRow, nil
		}
	} else {
		return nil, err
	}

	return oldAndNewRow, nil
}

// Applies the update expressions given to the row given, returning the new resultant row. In the case that ignore is
// provided and there is a type conversion error, this function sets the value to the zero value as per the MySQL standard.
func applyUpdateExpressionsWithIgnore(ctx *sql.Context, updateExprs []sql.Expression, tableSchema sql.Schema, row sql.Row, ignore bool) (sql.Row, error) {
	var secondPass []int
	var derived []int
	oldRow := row

	for i, updateExpr := range updateExprs {
		if expression.IsDerivedSetField(updateExpr) {
			derived = append(derived, i)
			continue
		}
		defaultVal, isDefaultVal := defaultValFromSetExpression(updateExpr)
		// Any generated columns must be projected into place so that the caller gets their newest values as well. We
		// do this in a second pass as necessary.
		if isDefaultVal && !defaultVal.IsLiteral() {
			secondPass = append(secondPass, i)
			continue
		}

		val, err := updateExpr.Eval(ctx, row)
		if err != nil {
			var wtce sql.WrappedTypeConversionError
			isTypeConversionError := errors.As(err, &wtce)
			if !isTypeConversionError || !ignore {
				if isTypeConversionError && wtce.OffendingIdx < len(tableSchema) &&
					(sql.ErrValueOutOfRange.Is(wtce.Err) || types.ErrConvertToDecimalLimit.Is(wtce.Err)) {
					// HeimDB: 1264, as MySQL's strict UPDATE (it was the type's end value, or 1105; stage 116)
					return nil, mysql.NewSQLError(mysql.ERWarnDataOutOfRange, mysql.SSDataOutOfRange, "Out of range value for column '%s' at row %d", tableSchema[wtce.OffendingIdx].Name, 1)
				}
				return nil, err
			}

			cpy := row.Copy()
			cpy[wtce.OffendingIdx] = wtce.OffendingVal // Needed for strings
			val = convertDataAndWarn(ctx, tableSchema, cpy, wtce.OffendingIdx, wtce.Err)
		}
		var ok bool
		row, ok = val.(sql.Row)
		if !ok {
			return nil, plan.ErrUpdateUnexpectedSetResult.New(val)
		}
	}

	for _, index := range secondPass {
		val, err := updateExprs[index].Eval(ctx, row)
		if err != nil {
			return nil, err
		}

		var ok bool
		row, ok = val.(sql.Row)
		if !ok {
			return nil, plan.ErrUpdateUnexpectedSetResult.New(val)
		}
	}

	// MySQL は、明示の式で行が変わらなければ ON UPDATE の列を進めず、影響を受けた行にも数えない。UPDATE JOIN では表ごとに、
	// その表の列が明示の式で変わったかで決める（段階 115。前は結合の行のどこかが変われば、変えていない表の ON UPDATE の
	// 列も進め、その表の行も影響を受けた行に数えた）
	if len(derived) > 0 {
		explicit := row
		lastTable, lastChanged, haveLast := "", false, false
		for _, index := range derived {
			table := derivedTargetTable(updateExprs[index])
			if !haveLast || table != lastTable {
				var err error
				lastChanged, err = tableRowChangedFor(ctx, tableSchema, table, oldRow, explicit)
				if err != nil {
					return nil, err
				}
				lastTable, haveLast = table, true
			}
			if !lastChanged {
				continue
			}
			val, err := updateExprs[index].Eval(ctx, row)
			if err != nil {
				return nil, err
			}
			var isRow bool
			row, isRow = val.(sql.Row)
			if !isRow {
				return nil, plan.ErrUpdateUnexpectedSetResult.New(val)
			}
		}
	}
	return row, nil
}

// derivedTargetTable は、ON UPDATE の式（派生の SetField）が書く列の表の名前（小文字）。分からなければ ""（HeimDB）。
func derivedTargetTable(e sql.Expression) string {
	if sf, ok := e.(*expression.SetField); ok {
		if gf, ok := sf.LeftChild.(*expression.GetField); ok {
			return strings.ToLower(gf.Table())
		}
	}
	return ""
}

// tableRowChangedFor は、表 table の列（tableSchema の Source が table の列）の値が変わったか。table が "" か、
// tableSchema にその表の列がなければ、行のすべての列で決める（tableRowChanged。1 つの表の UPDATE）（HeimDB）。
func tableRowChangedFor(ctx *sql.Context, tableSchema sql.Schema, table string, oldRow, newRow sql.Row) (bool, error) {
	n := len(tableSchema)
	if table == "" || len(oldRow) < n || len(newRow) < n {
		return tableRowChanged(ctx, tableSchema, oldRow, newRow)
	}
	oldT, newT := oldRow[len(oldRow)-n:], newRow[len(newRow)-n:]
	found := false
	for i, c := range tableSchema {
		if !strings.EqualFold(c.Source, table) {
			continue
		}
		found = true
		cmp, err := c.Type.Compare(ctx, oldT[i], newT[i])
		if err != nil {
			return true, err
		}
		if cmp != 0 {
			return true, nil
		}
	}
	if !found {
		return tableRowChanged(ctx, tableSchema, oldRow, newRow)
	}
	return false, nil
}

// tableRowChanged は、表の列（行の後ろの len(tableSchema) 個。前には外の scope の値がありうる）の値が変わったか。
func tableRowChanged(ctx *sql.Context, tableSchema sql.Schema, oldRow, newRow sql.Row) (bool, error) {
	n := len(tableSchema)
	if len(oldRow) < n || len(newRow) < n {
		return true, nil
	}
	same, err := oldRow[len(oldRow)-n:].Equals(ctx, newRow[len(newRow)-n:], tableSchema)
	return !same, err
}

func (u *updateIter) validateNullability(ctx *sql.Context, row sql.Row, schema sql.Schema) error {
	for idx := 0; idx < len(row); idx++ {
		col := schema[idx]
		if !col.Nullable && row[idx] == nil {
			// In the case of an IGNORE we set the nil value to a default and add a warning
			if u.ignore {
				row[idx] = col.Type.Zero()
				_ = warnOnIgnorableError(ctx, row, sql.ErrInsertIntoNonNullableProvidedNull.New(col.Name)) // will always return nil
			} else {
				return sql.ErrInsertIntoNonNullableProvidedNull.New(col.Name)
			}

		}
	}
	return nil
}

func (u *updateIter) Close(ctx *sql.Context) error {
	if !u.closed {
		u.closed = true
		if err := u.updater.Close(ctx); err != nil {
			return err
		}
		return u.childIter.Close(ctx)
	}
	return nil
}

func (u *updateIter) ignoreOrError(ctx *sql.Context, row sql.Row, err error) error {
	if !u.ignore {
		return err
	}

	return warnOnIgnorableError(ctx, row, err)
}

func newUpdateIter(
	childIter sql.RowIter,
	schema sql.Schema,
	updater sql.RowUpdater,
	checks sql.CheckConstraints,
	ignore bool,
	returnExprs []sql.Expression,
	returnSchema sql.Schema,
) sql.RowIter {
	if ignore {
		return plan.NewCheckpointingTableEditorIter(&updateIter{
			childIter:    childIter,
			updater:      updater,
			schema:       schema,
			checks:       checks,
			ignore:       true,
			returnExprs:  returnExprs,
			returnSchema: returnSchema,
		}, updater)
	} else {
		return plan.NewTableEditorIter(&updateIter{
			childIter:    childIter,
			updater:      updater,
			schema:       schema,
			checks:       checks,
			returnExprs:  returnExprs,
			returnSchema: returnSchema,
		}, updater)
	}
}

// updateJoinIter wraps the child UpdateSource ProjectIter and returns join row in such a way that updates per table row are
// done once.
type updateJoinIter struct {
	updateSourceIter sql.RowIter
	joinSchema       sql.Schema
	updaters         map[string]sql.RowUpdater
	caches           map[string]sql.KeyValueCache
	disposals        map[string]sql.DisposeFunc
	joinNode         sql.Node
	accumulator      *updateJoinRowHandler
	// HeimDB: joinSchema の列ごとの、外部結合で NULL で埋められうるか（outerJoinColumns）
	outerCols []bool
	// HeimDB: 表ごとの、主キーを持たないか（keyless）
	keylessTables map[string]bool
}

// keyless は、表 tableName（小文字）が主キーを持たないか（HeimDB）。
func (u *updateJoinIter) keyless(tableName string) bool {
	if u.keylessTables == nil {
		u.keylessTables = keylessTablesByName(u.joinNode)
	}
	return u.keylessTables[tableName]
}

// keylessTablesByName は、node の中の表（別名があれば別名、小文字）ごとに、主キーを持たないかを返す（HeimDB）。
func keylessTablesByName(node sql.Node) map[string]bool {
	ret := map[string]bool{}
	transform.Inspect(node, func(n sql.Node) bool {
		switch n := n.(type) {
		case *plan.TableAlias:
			if rt := resolvedTableOf(n.Child); rt != nil {
				ret[strings.ToLower(n.Name())] = sql.IsKeyless(rt.Schema())
			}
			return false
		case *plan.ResolvedTable:
			ret[strings.ToLower(n.Name())] = sql.IsKeyless(n.Schema())
		case *plan.IndexedTableAccess:
			if rt := resolvedTableOf(n.TableNode); rt != nil {
				ret[strings.ToLower(rt.Name())] = sql.IsKeyless(rt.Schema())
			}
			return false
		}
		return true
	})
	return ret
}

// resolvedTableOf は、n が表（索引で読む表を含む）なら、その ResolvedTable を返す（HeimDB）。
func resolvedTableOf(n sql.Node) *plan.ResolvedTable {
	switch n := n.(type) {
	case *plan.ResolvedTable:
		return n
	case *plan.IndexedTableAccess:
		return resolvedTableOf(n.TableNode)
	}
	return nil
}

// outerTable は、表 tableName（小文字）の列が外部結合で NULL で埋められうるか（HeimDB）。
func (u *updateJoinIter) outerTable(tableName string) bool {
	for i, c := range u.joinSchema {
		if i < len(u.outerCols) && u.outerCols[i] && strings.EqualFold(c.Source, tableName) {
			return true
		}
	}
	return false
}

// outerJoinColumns は、node の出力の列ごとに、外部結合で NULL で埋められうるか（LEFT JOIN の右、RIGHT JOIN の左、
// FULL OUTER JOIN の両側）を返す（HeimDB）。外部結合は半結合、内部結合、Project、Filter、ハッシュ結合の作る側
// （HashLookup）などの下にあってもよい。
// Project の式の列は GetField（別名を含む）だけをたどる。結合の行に出ない副問い合わせの中は見ない。
func outerJoinColumns(node sql.Node) []bool {
	out := make([]bool, len(node.Schema()))
	switch n := node.(type) {
	case *plan.JoinNode:
		left, right := outerJoinColumns(n.Left()), outerJoinColumns(n.Right())
		jt := n.JoinType()
		for i := range right {
			right[i] = right[i] || jt.IsLeftOuter() || jt.IsFullOuter()
		}
		for i := range left {
			left[i] = left[i] || jt.IsRightOuter() || jt.IsFullOuter()
		}
		// 半結合と反結合の行は左の行だけ
		copy(out, append(left, right...))
	case *plan.Project:
		child := outerJoinColumns(n.Child)
		for i, e := range n.Projections {
			if a, ok := e.(*expression.Alias); ok {
				e = a.Child
			}
			if gf, ok := e.(*expression.GetField); ok && gf.Index() >= 0 && gf.Index() < len(child) && i < len(out) {
				out[i] = child[gf.Index()]
			}
		}
	case *plan.Filter, *plan.Sort, *plan.TopN, *plan.Limit, *plan.Offset, *plan.Distinct, *plan.OrderedDistinct, *plan.Having,
		*plan.HashLookup, *plan.CachedResults, *plan.StripRowNode:
		child := outerJoinColumns(n.Children()[0])
		if len(child) == len(out) {
			copy(out, child)
		}
	}
	return out
}

var _ sql.RowIter = (*updateJoinIter)(nil)

func (u *updateJoinIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		oldAndNewRow, err := u.updateSourceIter.Next(ctx)
		if err != nil {
			return nil, err
		}

		oldJoinRow, newJoinRow := oldAndNewRow[:len(oldAndNewRow)/2], oldAndNewRow[len(oldAndNewRow)/2:]

		tableToOldRowMap := plan.SplitRowIntoTableRowMap(oldJoinRow, u.joinSchema)
		tableToNewRowMap := plan.SplitRowIntoTableRowMap(newJoinRow, u.joinSchema)

		for tableName, _ := range u.updaters {
			tableName = strings.ToLower(tableName)
			oldTableRow := tableToOldRowMap[tableName]

			// Handle the case of row being ignored due to it not being valid in the join row.
			// HeimDB: 外部結合で NULL で埋められる側の表だけを、木のどこにある外部結合でも確かめる（一番上の結合しか
			// 見ないと、IN (SELECT ...) の半結合の下にある LEFT JOIN の NULL の行を更新しようとした）
			if u.outerTable(tableName) {
				if !u.shouldUpdateDirectionalJoin(oldTableRow) {
					// rewrite the newJoinRow to ensure an update does not happen
					tableToNewRowMap[tableName] = oldTableRow
					continue
				}
			}

			// HeimDB: a keyless table is updated here only from an UPDATE whose
			// subquery became a join (an explicit UPDATE JOIN of one is rejected,
			// upstream #3052), where each row comes once; identical rows are
			// different rows, so they are not deduplicated by value
			if u.keyless(tableName) {
				u.accumulator.handleRowMatched()
				continue
			}

			// Determine whether this row in the table has already been updated
			cache := u.getOrCreateCache(ctx, tableName)
			hash, err := sql.HashOf(ctx, oldTableRow)
			if err != nil {
				return nil, err
			}

			_, err = cache.Get(hash)
			if errors.Is(err, sql.ErrKeyNotFound) {
				cache.Put(hash, struct{}{})

				// updateJoin counts matched rows from join output
				u.accumulator.handleRowMatched()

				continue
			} else if err != nil {
				return nil, err
			}

			// If this row for the table has already been updated we rewrite the newJoinRow counterpart to ensure that this
			// returned row is not incorrectly counted by the update accumulator.
			tableToNewRowMap[tableName] = oldTableRow
		}

		newJoinRow = recreateRowFromMap(tableToNewRowMap, u.joinSchema)
		equals, err := oldJoinRow.Equals(ctx, newJoinRow, u.joinSchema)
		if err != nil {
			return nil, err
		}
		if !equals {
			return append(oldJoinRow, newJoinRow...), nil
		}
	}
}

// shouldUpdateDirectionalJoin は、外部結合で NULL で埋められうる表の行を更新するか（HeimDB）。すべて NULL の行は、
// 外部結合が NULL で埋めた行なので更新しない。UPDATE JOIN は主キーのない表を非対応にしていて（keyless tables
// unsupported for UPDATE JOIN）、主キーの列は NULL にならないので、本物の行がすべて NULL になることはない。前は一番上の
// 結合だけを見て、その結合の条件を行に当てていたが、IN (SELECT ...) の半結合の下の LEFT JOIN を見落とし、反結合から作った
// LEFT JOIN（上の Project が右の列を落とす）では行が短く条件の列の位置が合わず、ON t2.a IS NULL のような条件では NULL で
// 埋めた行も条件を満たした。
func (u *updateJoinIter) shouldUpdateDirectionalJoin(tableRow sql.Row) bool {
	for _, v := range tableRow {
		if v != nil {
			return true
		}
	}
	return false
}

func (u *updateJoinIter) Close(context *sql.Context) error {
	for _, disposeF := range u.disposals {
		disposeF()
	}

	return u.updateSourceIter.Close(context)
}

func (u *updateJoinIter) getOrCreateCache(ctx *sql.Context, tableName string) sql.KeyValueCache {
	potential, exists := u.caches[tableName]
	if exists {
		return potential
	}

	cache, disposal := ctx.Memory.NewHistoryCache()
	u.caches[tableName] = cache
	u.disposals[tableName] = disposal

	return cache
}

// recreateRowFromMap takes a join schema and row map and recreates the original join row.
func recreateRowFromMap(rowMap map[string]sql.Row, joinSchema sql.Schema) sql.Row {
	var ret sql.Row

	if len(joinSchema) == 0 {
		return ret
	}

	currentTable := strings.ToLower(joinSchema[0].Source)
	ret = append(ret, rowMap[currentTable]...)

	for i := 1; i < len(joinSchema); i++ {
		newTable := strings.ToLower(joinSchema[i].Source)
		if !strings.EqualFold(newTable, currentTable) {
			ret = append(ret, rowMap[newTable]...)
			currentTable = newTable
		}
	}

	return ret
}
