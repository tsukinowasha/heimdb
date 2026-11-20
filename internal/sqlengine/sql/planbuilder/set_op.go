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

// Modified by Tsukinowa Inc.: UNION の列の型を、両側を変える型にしてスコープの列にも使う（上流の #3060。docs/engine.md）
// Modified by Tsukinowa Inc.: 入れ子の UNION の列の ID を左の子の列の順で取る（上流の #3141。docs/engine.md）
// Modified by Tsukinowa Inc.: UNION の ORDER BY の番号で定数の列を指すとき、結果の列で並べる（docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL と整数の UNION の列を、MySQL と同じ桁と小数の桁の DECIMAL にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: DATE と DATETIME の UNION の列を DATETIME にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の列と DOUBLE か整数の UNION の列を、MySQL と同じ FLOAT か DOUBLE にする（段階 120 のレビュー。docs/engine.md）

package planbuilder

import (
	"fmt"
	"reflect"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression/function/aggregation"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func hasRecursiveCte(node sql.Node) bool {
	hasRCTE := false
	transform.Inspect(node, func(n sql.Node) bool {
		if _, ok := n.(*plan.RecursiveTable); ok {
			hasRCTE = true
			return false
		}
		return true
	})
	return hasRCTE
}

func (b *Builder) buildSetOp(inScope *scope, u *ast.SetOp) (outScope *scope) {
	leftScope := b.buildSelectStmt(inScope, u.Left)
	rightScope := b.buildSelectStmt(inScope, u.Right)

	var setOpType int
	switch u.Type {
	case ast.UnionStr, ast.UnionAllStr, ast.UnionDistinctStr:
		setOpType = plan.UnionType
	case ast.IntersectStr, ast.IntersectAllStr, ast.IntersectDistinctStr:
		setOpType = plan.IntersectType
	case ast.ExceptStr, ast.ExceptAllStr, ast.ExceptDistinctStr:
		setOpType = plan.ExceptType
	default:
		b.handleErr(fmt.Errorf("unknown union type %s", u.Type))
	}

	if setOpType != plan.UnionType {
		if hasRecursiveCte(leftScope.node) {
			b.handleErr(sql.ErrRecursiveCTENotUnion.New())
		}
		if hasRecursiveCte(rightScope.node) {
			b.handleErr(sql.ErrRecursiveCTENotUnion.New())
		}
	}

	// all is not distinct
	distinct := true
	switch u.Type {
	case ast.UnionAllStr, ast.IntersectAllStr, ast.ExceptAllStr:
		distinct = false
	}

	// HeimDB: the scope columns (what ORDER BY and an outer query see) take the
	// type that mergeSetOpSchemas converts both sides to (upstream #3060 does this
	// with GeneralizeTypes from #3032, which this fork does not have).
	leftScope.cols = mergeSetOpScopeColumns(leftScope, rightScope)

	limit := b.buildLimit(inScope, u.Limit)
	offset := b.buildOffset(inScope, u.Limit)

	for _, o := range u.OrderBy {
		if expr, ok := o.Expr.(*ast.ColName); ok && len(expr.Qualifier.Name.String()) != 0 {
			b.handleErr(ErrQualifiedOrderBy.New(expr.Qualifier.Name.String()))
		}
	}

	// mysql errors for order by right projection
	orderByScope := b.analyzeOrderBy(leftScope, leftScope, u.OrderBy)

	var sortFields sql.SortFields
	for _, c := range orderByScope.cols {
		so := sql.Ascending
		if c.descending {
			so = sql.Descending
		}
		scalar := c.scalar
		if scalar == nil {
			scalar = c.scalarGf()
		}
		if _, ok := scalar.(*expression.Literal); ok && c.id != 0 {
			// HeimDB: ORDER BY 1 on a constant column ('j1' in SELECT 'j1', id
			// ... UNION ALL SELECT 'j2', id ...) sorts by the result column, not
			// by the left side's constant (it did not sort at all). The constant
			// has no column id in the left side's projection, so the reference
			// goes by the result column's name.
			sch := leftScope.node.Schema()
			for i, lc := range leftScope.cols {
				if lc.id == c.id && i < len(sch) {
					scalar = expression.NewGetField(i, c.typ, sch[i].Name, c.nullable).WithId(0).(sql.Expression)
					break
				}
			}
		}
		// Unions pass order bys to the top scope, where the original
		// order by get field may no longer be accessible. Here it is
		// safe to assume the alias has already been computed.
		scalar, _, _ = transform.Expr(scalar, func(e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
			switch e := e.(type) {
			case *expression.Alias:
				return expression.NewGetField(int(c.id), e.Type(), e.Name(), e.IsNullable()), transform.NewTree, nil
			default:
				return e, transform.SameTree, nil
			}
		})
		sf := sql.SortField{
			Column: scalar,
			Order:  so,
		}
		sortFields = append(sortFields, sf)
	}

	n, ok := leftScope.node.(*plan.SetOp)
	if ok {
		if len(n.SortFields) > 0 {
			if len(sortFields) > 0 {
				err := sql.ErrConflictingExternalQuery.New()
				b.handleErr(err)
			}
			sortFields = n.SortFields
		}
		if n.Limit != nil {
			if limit != nil {
				err := fmt.Errorf("conflicing external LIMIT")
				b.handleErr(err)
			}
			limit = n.Limit
		}
		if n.Offset != nil {
			if offset != nil {
				err := fmt.Errorf("conflicing external OFFSET")
				b.handleErr(err)
			}
			offset = n.Offset
		}
		leftScope.node = plan.NewSetOp(n.SetOpType, n.Left(), n.Right(), n.Distinct, nil, nil, nil).WithColumns(n.Columns()).WithId(n.Id())
	}

	var cols sql.ColSet
	for _, c := range leftScope.cols {
		cols.Add(sql.ColumnId(c.id))
	}
	b.tabId++
	tabId := b.tabId
	ret := plan.NewSetOp(setOpType, leftScope.node, rightScope.node, distinct, limit, offset, sortFields).WithId(tabId).WithColumns(cols)
	outScope = leftScope
	outScope.node = b.mergeSetOpSchemas(ret.(*plan.SetOp))
	return
}

// mergeSetOpScopeColumns returns the left scope's columns with the types and
// nullability of the set operation's result: where the two sides' types differ,
// the type both are converted to in mergeSetOpSchemas (an ENUM and a VARCHAR
// become a string, so an outer WHERE, MAX or ORDER BY no longer reads the
// right side's strings as ENUM values).
func mergeSetOpScopeColumns(left, right *scope) []scopeColumn {
	ls, rs := left.node.Schema(), right.node.Schema()
	if len(left.cols) != len(ls) || len(ls) != len(rs) {
		return left.cols
	}
	merged := make([]scopeColumn, len(left.cols))
	copy(merged, left.cols)
	for i := range merged {
		merged[i].nullable = merged[i].nullable || rs[i].Nullable
		if reflect.DeepEqual(ls[i].Type, rs[i].Type) {
			continue
		}
		if t, ok := expression.RealSetOpType(ls[i].Type, rs[i].Type); ok {
			// HeimDB: FLOAT と DOUBLE の列は MySQL と同じ FLOAT か DOUBLE（段階 120 のレビュー）
			merged[i].typ = t
			merged[i].scalar = nil
			continue
		}
		convertTo := setOpConvertToType(ls[i].Type, rs[i].Type)
		length, scale := setOpDecimalLengthAndScale(convertTo, ls[i].Type, rs[i].Type)
		gf := expression.NewGetField(0, ls[i].Type, ls[i].Name, ls[i].Nullable)
		merged[i].typ = expression.NewConvertWithLengthAndScale(gf, convertTo, length, scale).Type()
		// the left side's expression has the left side's type; ORDER BY reads the
		// result column (scalarGf) instead
		merged[i].scalar = nil
	}
	return merged
}

func (b *Builder) mergeSetOpSchemas(u *plan.SetOp) sql.Node {
	ls, rs := u.Left().Schema(), u.Right().Schema()
	if len(ls) != len(rs) {
		err := ErrUnionSchemasDifferentLength.New(len(ls), len(rs))
		b.handleErr(err)
	}

	leftIds := colIdsForRel(u.Left())
	rightIds := colIdsForRel(u.Right())

	les, res := make([]sql.Expression, len(ls)), make([]sql.Expression, len(rs))
	hasdiff := false
	var err error
	for i := range ls {
		// todo: proj col ids should align with input column ids
		les[i] = expression.NewGetFieldWithTable(int(leftIds[i]), 0, ls[i].Type, ls[i].DatabaseSource, ls[i].Source, ls[i].Name, ls[i].Nullable)
		res[i] = expression.NewGetFieldWithTable(int(rightIds[i]), 0, rs[i].Type, rs[i].DatabaseSource, rs[i].Source, rs[i].Name, rs[i].Nullable)
		if reflect.DeepEqual(ls[i].Type, rs[i].Type) {
			continue
		}
		hasdiff = true

		if t, ok := expression.RealSetOpType(ls[i].Type, rs[i].Type); ok {
			// HeimDB: FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の列と DOUBLE か整数の UNION は、MySQL と同じく DOUBLE（前は
			// 型が違うと文字列に変換し、文字列の順に並べた。段階 120 のレビュー）
			les[i] = expression.NewAlias(ls[i].Name, expression.NewRealResult(les[i], t))
			res[i] = expression.NewAlias(rs[i].Name, expression.NewRealResult(res[i], t))
			continue
		}

		// try to get optimal type to convert both into
		convertTo := setOpConvertToType(ls[i].Type, rs[i].Type)

		// TODO: Principled type coercion...
		length, scale := setOpDecimalLengthAndScale(convertTo, ls[i].Type, rs[i].Type)
		les[i], err = b.f.buildConvert(les[i], convertTo, length, scale)
		res[i], err = b.f.buildConvert(res[i], convertTo, length, scale)

		// Preserve schema names across the conversion.
		les[i] = expression.NewAlias(ls[i].Name, les[i])
		res[i] = expression.NewAlias(rs[i].Name, res[i])
	}
	var ret sql.Node = u
	if hasdiff {
		ret, err = u.WithChildren(
			plan.NewProject(les, u.Left()),
			plan.NewProject(res, u.Right()),
		)
		if err != nil {
			b.handleErr(err)
		}
	}
	return ret
}

// setOpConvertToType is expression.GetConvertToType, but a DATE with a
// DATETIME or TIMESTAMP is a DATETIME, as in MySQL (it was text; HeimDB, stage 116).
func setOpConvertToType(l, r sql.Type) string {
	if types.IsTime(l) && types.IsTime(r) {
		return expression.ConvertToDatetime
	}
	return expression.GetConvertToType(l, r)
}

// setOpDecimalLengthAndScale returns the DECIMAL type's precision and scale of
// a UNION column whose sides are DECIMALs or integers, as MySQL's: the most
// integer digits and the most decimals of the two sides (DECIMAL(10,2) and
// DECIMAL(26,4) become DECIMAL(26,4), not DECIMAL(65,30), so a SUM over the
// UNION shows 4 decimals). HeimDB. 0, 0 for other types.
func setOpDecimalLengthAndScale(convertTo string, l, r sql.Type) (int, int) {
	if convertTo != expression.ConvertToDecimal {
		return 0, 0
	}
	lp, lsc, lok := aggregation.DecimalPrecision(l)
	rp, rsc, rok := aggregation.DecimalPrecision(r)
	if !lok || !rok {
		return 0, 0
	}
	scale := max(lsc, rsc)
	length := min(int(max(lp-lsc, rp-rsc))+int(scale), types.DecimalTypeMaxPrecision)
	return length, int(scale)
}

// colIdsForRel returns the padded column set returned by a node,
// with 0's filled in for non-aliasable columns
func colIdsForRel(n sql.Node) []sql.ColumnId {
	var ids []sql.ColumnId
	switch n := n.(type) {
	case *plan.Project:
		for _, p := range n.Projections {
			if ide, ok := p.(sql.IdExpression); ok {
				ids = append(ids, ide.Id())
			} else {
				ids = append(ids, 0)
			}
		}
		return ids
	case *plan.SetOp:
		// SetOp nodes need to preserve original schema order to avoid column
		// scrambling in nested UNIONs (upstream #3141; needed with #3110, which
		// gives a SetOp's exec indexes its left child's column order)
		return colIdsForRel(n.Left())
	case plan.TableIdNode:
		cols := n.Columns()
		if tn, ok := n.(sql.TableNode); ok {
			if pkt, ok := tn.UnderlyingTable().(sql.PrimaryKeyTable); ok && len(pkt.PrimaryKeySchema().Schema) != len(n.Schema()) {
				firstcol, _ := cols.Next(1)
				for _, c := range n.Schema() {
					ord := pkt.PrimaryKeySchema().IndexOfColName(c.Name)
					colId := firstcol + sql.ColumnId(ord)
					ids = append(ids, colId)
				}
				return ids
			}
		}
		cols.ForEach(func(col sql.ColumnId) {
			ids = append(ids, col)
		})
		return ids
	default:
		return colIdsForRel(n.Children()[0])
	}
	return nil
}
