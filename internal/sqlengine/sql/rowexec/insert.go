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

// Modified by Tsukinowa Inc.: INSERT IGNORE の重なりの警告を MySQL と同じ Warning と文にする（docs/engine.md）

// Modified by Tsukinowa Inc.: ON DUPLICATE KEY UPDATE で、明示の式で行が変わらなければ ON UPDATE の列の式を当てない（上流の #3461、#3468。docs/engine.md）
// Modified by Tsukinowa Inc.: STRICT の長すぎる値の後ろが空白だけなら切って Note 1265 にし、IGNORE の長すぎる値の警告を 1265 にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: STRICT の日時の列への日時として読めない値を 1292 にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: STRICT の数の列の範囲の外を 1264、数で始まる文字列の切り詰めを 1265、文字列の列への不正な UTF-8 を 1366 にする（上流の #3920、#3078 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: 省いた NOT NULL の ENUM の列に最初の値を入れる（上流の #2985。docs/engine.md）

package rowexec

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/vt/proto/query"
	"gopkg.in/src-d/go-errors.v1"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression/function"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type insertIter struct {
	schema       sql.Schema
	inserter     sql.RowInserter
	replacer     sql.RowReplacer
	updater      sql.RowUpdater
	rowSource    sql.RowIter
	unlocker     func()
	ctx          *sql.Context
	insertExprs  []sql.Expression
	updateExprs  []sql.Expression
	checks       sql.CheckConstraints
	tableNode    sql.Node
	closed       bool
	ignore       bool
	returnExprs  []sql.Expression
	returnSchema sql.Schema

	firstGeneratedAutoIncRowIdx int

	// rowNumber は、読んだ行の番号（1 から。MySQL の誤りの文の at row に使う）。
	rowNumber int

	deferredDefaults sql.FastIntSet
}

func getInsertExpressions(values sql.Node) []sql.Expression {
	var exprs []sql.Expression
	transform.Inspect(values, func(node sql.Node) bool {
		switch node := node.(type) {
		case *plan.Project:
			exprs = node.Projections
			return false
		}
		return true
	})
	return exprs
}

func (i *insertIter) Next(ctx *sql.Context) (returnRow sql.Row, returnErr error) {
	row, err := i.rowSource.Next(ctx)
	if err == io.EOF {
		return nil, err
	}

	if err != nil {
		return nil, i.ignoreOrClose(ctx, row, err)
	}
	i.rowNumber++

	// Prune the row down to the size of the schema. It can be larger in the case of running with an outer scope, in which
	// case the additional scope variables are prepended to the row.
	if len(row) > len(i.schema) {
		row = row[len(row)-len(i.schema):]
	}

	err = i.validateNullability(ctx, i.schema, row)
	if err != nil {
		return nil, i.ignoreOrClose(ctx, row, err)
	}

	err = i.evaluateChecks(ctx, row)
	if err != nil {
		return nil, i.ignoreOrClose(ctx, row, err)
	}

	origRow := make(sql.Row, len(row))
	copy(origRow, row)

	// Do any necessary type conversions to the target schema
	for idx, col := range i.schema {
		if row[idx] != nil {
			converted, inRange, cErr := col.Type.Convert(ctx, row[idx])
			if cErr == nil && !inRange {
				cErr = sql.ErrValueOutOfRange.New(row[idx], col.Type)
			}
			if cErr != nil {
				// Ignore individual column errors when INSERT IGNORE, UPDATE IGNORE, etc. is specified.
				// For JSON column types, always throw an error. MySQL throws the following error even when
				// IGNORE is specified:
				// ERROR 3140 (22032): Invalid JSON text: "Invalid value." at position 0 in value for column
				// 'table.column'.
				if i.ignore && col.Type.Type() != query.Type_JSON {
					if _, ok := col.Type.(sql.NumberType); ok {
						if converted == nil {
							converted = i.schema[idx].Type.Zero()
						}
						row[idx] = converted
						// Add a warning instead
						ctx.Session.Warn(&sql.Warning{
							Level:   "Note",
							Code:    sql.CastSQLError(cErr).Num,
							Message: cErr.Error(),
						})
					} else {
						row = convertDataAndWarn(ctx, i.schema, row, idx, cErr)
					}
					continue
				} else {
					if str, ok := row[idx].(string); ok && types.ErrLengthBeyondLimit.Is(cErr) {
						if cut, ok := trailingSpacesFit(col.Type, str); ok {
							// HeimDB: past the length only by trailing spaces is cut with Note 1265, as in MySQL (stage 116)
							ctx.Session.Warn(&sql.Warning{Level: "Note", Code: mysql.ERWarnDataTruncated, Message: fmt.Sprintf("Data truncated for column '%s' at row %d", col.Name, i.rowNumber)})
							row[idx] = cut
							continue
						}
					}
					// Fill in error with information
					if types.ErrLengthBeyondLimit.Is(cErr) {
						cErr = types.ErrLengthBeyondLimit.New(row[idx], col.Name)
					} else if types.ErrBadCharsetString.Is(cErr) {
						// STRICT の文字列の列への不正な UTF-8 は、MySQL と同じく 1366（段階 111 で回した 1105。上流の #3078）
						cErr = mysql.NewSQLError(mysql.ERTruncatedWrongValueForField, mysql.SSUnknownSQLState, "Incorrect string value: '%s' for column '%s' at row %d", invalidStringBytes(row[idx]), col.Name, i.rowNumber)
					} else if _, isNumber := col.Type.(sql.NumberType); (isNumber || types.IsDecimal(col.Type)) &&
						(sql.ErrValueOutOfRange.Is(cErr) || types.ErrConvertToDecimalLimit.Is(cErr)) {
						// STRICT の数の列の範囲の外は、MySQL と同じく 1264（段階 111 で回した 1105）
						cErr = mysql.NewSQLError(mysql.ERWarnDataOutOfRange, mysql.SSDataOutOfRange, "Out of range value for column '%s' at row %d", col.Name, i.rowNumber)
					} else if _, isNumber := col.Type.(sql.NumberType); isNumber && !types.IsDecimal(col.Type) && types.TruncatedString(row[idx], cErr) && types.StringOverflowsDouble(row[idx].(string)) {
						// '1e400' は、MySQL と同じく範囲の外の 1264（段階 114）
						cErr = mysql.NewSQLError(mysql.ERWarnDataOutOfRange, mysql.SSDataOutOfRange, "Out of range value for column '%s' at row %d", col.Name, i.rowNumber)
					} else if _, isNumber := col.Type.(sql.NumberType); isNumber && !types.IsDecimal(col.Type) && types.TruncatedString(row[idx], cErr) && startsWithNumber(row[idx]) {
						// 整数と DOUBLE の列への数で始まる文字列（'12abc'）は、MySQL と同じく 1265（DECIMAL と、数で
						// 始まらない 'abc' は 1366 のまま）
						cErr = mysql.NewSQLError(mysql.ERWarnDataTruncated, "01000", "Data truncated for column '%s' at row %d", col.Name, i.rowNumber)
					} else if types.IsTime(col.Type) && types.ErrConvertingToTime.Is(cErr) {
						// STRICT の日時の列への日時として読めない値は、MySQL と同じく 1292（段階 116。前は 1105）
						cErr = mysql.NewSQLError(mysql.ERTruncatedWrongValue, "22007", "Incorrect datetime value: '%v' for column '%s' at row %d", row[idx], col.Name, i.rowNumber)
					} else if sql.ErrNotMatchingSRID.Is(cErr) {
						cErr = sql.ErrNotMatchingSRIDWithColName.New(col.Name, cErr)
					}
					return nil, sql.NewWrappedInsertError(origRow, cErr)
				}
			}
			row[idx] = converted
		}
	}

	if i.replacer != nil {
		toReturn := make(sql.Row, len(row)*2)
		for i := 0; i < len(row); i++ {
			toReturn[i+len(row)] = row[i]
		}
		// May have multiple duplicate pk & unique errors due to multiple indexes
		//TODO: how does this interact with triggers?
		for {
			if err := i.replacer.Insert(ctx, row); err != nil {
				if !sql.ErrPrimaryKeyViolation.Is(err) && !sql.ErrUniqueKeyViolation.Is(err) {
					i.rowSource.Close(ctx)
					i.rowSource = nil
					return nil, sql.NewWrappedInsertError(row, err)
				}

				ue := err.(*errors.Error).Cause().(sql.UniqueKeyError)
				if err = i.replacer.Delete(ctx, ue.Existing); err != nil {
					i.rowSource.Close(ctx)
					i.rowSource = nil
					return nil, sql.NewWrappedInsertError(row, err)
				}
				// the row had to be deleted, write the values into the toReturn row
				copy(toReturn, ue.Existing)
			} else {
				break
			}
		}
		return toReturn, nil
	} else {
		if err := i.inserter.Insert(ctx, row); err != nil {
			if (!sql.ErrPrimaryKeyViolation.Is(err) && !sql.ErrUniqueKeyViolation.Is(err) && !sql.ErrDuplicateEntry.Is(err)) || len(i.updateExprs) == 0 {
				return nil, i.ignoreOrClose(ctx, row, err)
			}

			ue := err.(*errors.Error).Cause().(sql.UniqueKeyError)
			return i.handleOnDuplicateKeyUpdate(ctx, ue.Existing, row)
		}
	}

	i.updateLastInsertId(ctx, row)

	if len(i.returnExprs) > 0 {
		var retExprRow sql.Row
		for _, returnExpr := range i.returnExprs {
			result, err := returnExpr.Eval(ctx, row)
			if err != nil {
				return nil, err
			}
			retExprRow = append(retExprRow, result)
		}
		return retExprRow, nil
	}

	return row, nil
}

func (i *insertIter) handleOnDuplicateKeyUpdate(ctx *sql.Context, oldRow, newRow sql.Row) (returnRow sql.Row, returnErr error) {
	var err error
	updateAcc := append(oldRow, newRow...)
	var evalRow sql.Row
	apply := func(updateExpr sql.Expression) error {
		// this SET <val> indexes into LHS, but the <expr> can
		// reference the new row on RHS
		val, err := updateExpr.Eval(i.ctx, updateAcc)
		if err != nil {
			if i.ignore {
				idx, ok := getFieldIndexFromUpdateExpr(updateExpr)
				if !ok {
					return err
				}

				val = convertDataAndWarn(ctx, i.schema, newRow, idx, err)
			} else {
				return err
			}
		}

		updateAcc = val.(sql.Row)
		return nil
	}
	var derived []sql.Expression
	for _, updateExpr := range i.updateExprs {
		if expression.IsDerivedSetField(updateExpr) {
			derived = append(derived, updateExpr)
			continue
		}
		if err := apply(updateExpr); err != nil {
			return nil, err
		}
	}
	// MySQL は、明示の式で行が変わらなければ ON UPDATE の列を進めない（影響を受けた行も 0）
	if len(derived) > 0 {
		changed, err := tableRowChanged(ctx, i.schema, oldRow, updateAcc[:len(oldRow)])
		if err != nil {
			return nil, err
		}
		if changed {
			for _, updateExpr := range derived {
				if err := apply(updateExpr); err != nil {
					return nil, err
				}
			}
		}
	}
	// project LHS only
	evalRow = updateAcc[:len(oldRow)]

	// Should revaluate the check conditions.
	err = i.evaluateChecks(ctx, evalRow)
	if err != nil {
		return nil, i.ignoreOrClose(ctx, newRow, err)
	}

	err = i.updater.Update(ctx, oldRow, evalRow)
	if err != nil {
		return nil, i.ignoreOrClose(ctx, newRow, err)
	}

	// In the case that we attempted an update, return a concatenated [old,new] row just like update.
	return oldRow.Append(evalRow), nil
}

func getFieldIndexFromUpdateExpr(updateExpr sql.Expression) (int, bool) {
	setField, ok := updateExpr.(*expression.SetField)
	if !ok {
		return 0, false
	}

	getField, ok := setField.LeftChild.(*expression.GetField)
	if !ok {
		return 0, false
	}

	return getField.Index(), true
}

// resolveValues resolves all VALUES functions.
func (i *insertIter) resolveValues(ctx *sql.Context, insertRow sql.Row) error {
	for _, updateExpr := range i.updateExprs {
		var err error
		sql.Inspect(updateExpr, func(expr sql.Expression) bool {
			valuesExpr, ok := expr.(*function.Values)
			if !ok {
				return true
			}
			getField, ok := valuesExpr.Child.(*expression.GetField)
			if !ok {
				err = fmt.Errorf("VALUES functions may only contain column names")
				return false
			}
			valuesExpr.Value = insertRow[getField.Index()]
			return false
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *insertIter) Close(ctx *sql.Context) error {
	if !i.closed {
		i.closed = true
		if i.unlocker != nil {
			i.unlocker()
		}
		var rsErr, iErr, rErr, uErr error
		if i.rowSource != nil {
			rsErr = i.rowSource.Close(ctx)
		}
		if i.inserter != nil {
			iErr = i.inserter.Close(ctx)
		}
		if i.replacer != nil {
			rErr = i.replacer.Close(ctx)
		}
		if i.updater != nil {
			uErr = i.updater.Close(ctx)
		}
		if rsErr != nil {
			return rsErr
		}
		if iErr != nil {
			return iErr
		}
		if rErr != nil {
			return rErr
		}
		if uErr != nil {
			return uErr
		}
	}
	return nil
}

func (i *insertIter) updateLastInsertId(ctx *sql.Context, row sql.Row) {
	if i.firstGeneratedAutoIncRowIdx < 0 {
		return
	}
	if i.firstGeneratedAutoIncRowIdx == 0 {
		autoIncVal := i.getAutoIncVal(row)
		ctx.SetLastQueryInfoInt(sql.LastInsertId, autoIncVal)
	}
	i.firstGeneratedAutoIncRowIdx--
}

func (i *insertIter) getAutoIncVal(row sql.Row) int64 {
	for i, expr := range i.insertExprs {
		if _, ok := expr.(*expression.AutoIncrement); ok {
			return toInt64(row[i])
		}
	}
	return 0
}

func (i *insertIter) ignoreOrClose(ctx *sql.Context, row sql.Row, err error) error {
	if !i.ignore {
		return sql.NewWrappedInsertError(row, err)
	}

	return warnOnIgnorableError(ctx, row, err)
}

// convertDataAndWarn modifies a row with data conversion issues in INSERT/UPDATE IGNORE calls
// Per MySQL docs "Rows set to values that would cause data conversion errors are set to the closest valid values instead"
// cc. https://dev.mysql.com/doc/refman/8.0/en/sql-mode.html#sql-mode-strict
func convertDataAndWarn(ctx *sql.Context, tableSchema sql.Schema, row sql.Row, columnIdx int, err error) sql.Row {
	if types.ErrLengthBeyondLimit.Is(err) {
		maxLength := tableSchema[columnIdx].Type.(sql.StringType).MaxCharacterLength()
		row[columnIdx] = row[columnIdx].(string)[:maxLength] // truncate string
	} else {
		row[columnIdx] = tableSchema[columnIdx].Type.Zero()
	}

	sqlerr := sql.CastSQLError(err)
	code := sqlerr.Num
	if types.ErrLengthBeyondLimit.Is(err) {
		// HeimDB: the truncated string is 1265 (Data truncated), as in MySQL; the strict error is 1406 (stage 116)
		code = mysql.ERWarnDataTruncated
	}

	// Add a warning instead
	if ctx != nil && ctx.Session != nil {
		ctx.Session.Warn(&sql.Warning{
			Level:   "Note",
			Code:    code,
			Message: err.Error(),
		})
	}

	return row
}

func warnOnIgnorableError(ctx *sql.Context, row sql.Row, err error) error {
	// Check that this error is a part of the list of Ignorable Errors and create the relevant warning
	for _, ie := range plan.IgnorableErrors {
		if ie.Is(err) {
			sqlerr := sql.CastSQLError(err)

			// Add a warning instead
			if ctx != nil && ctx.Session != nil {
				level, msg := "Note", err.Error()
				// 重なり（1062）は、MySQL と同じく Warning で、文も MySQL と同じにする
				if m, ok := sql.DuplicateEntryMessage(err); ok {
					level, msg = "Warning", m
				}
				ctx.Session.Warn(&sql.Warning{
					Level:   level,
					Code:    sqlerr.Num,
					Message: msg,
				})
			}

			// In this case the default value gets updated so return nil
			if sql.ErrInsertIntoNonNullableDefaultNullColumn.Is(err) {
				return nil
			}

			// Return the InsertIgnore err to ensure our accumulator doesn't count this row.
			return sql.NewIgnorableError(row)
		}
	}

	return err
}

func (i *insertIter) evaluateChecks(ctx *sql.Context, row sql.Row) error {
	for _, check := range i.checks {
		if !check.Enforced {
			continue
		}

		res, err := sql.EvaluateCondition(ctx, check.Expr, row)

		if err != nil {
			return err
		}

		if sql.IsFalse(res) {
			return sql.ErrCheckConstraintViolated.New(check.Name)
		}
	}

	return nil
}

func (i *insertIter) validateNullability(ctx *sql.Context, dstSchema sql.Schema, row sql.Row) error {
	for count, col := range dstSchema {
		// 省いた、既定値のない NOT NULL の ENUM の列は、MySQL と同じく最初の値（上流の #2985。NULL を許す ENUM は
		// NULL のまま。上流の #3043）
		if !col.Nullable && row[count] == nil && types.IsEnum(col.Type) && i.deferredDefaults.Contains(count) {
			row[count] = uint16(1)
			continue
		}
		if !col.Nullable && row[count] == nil {
			// In the case of an IGNORE we set the nil value to a default and add a warning
			if !i.ignore {
				if i.deferredDefaults.Contains(count) {
					return sql.ErrInsertIntoNonNullableDefaultNullColumn.New(col.Name)
				}
				return sql.ErrInsertIntoNonNullableProvidedNull.New(col.Name)
			}
			row[count] = col.Type.Zero()
			_ = warnOnIgnorableError(ctx, row, sql.ErrInsertIntoNonNullableProvidedNull.New(col.Name)) // will always return nil
		}
	}
	return nil
}

func toInt64(x interface{}) int64 {
	switch x := x.(type) {
	case int:
		return int64(x)
	case uint:
		return int64(x)
	case int8:
		return int64(x)
	case uint8:
		return int64(x)
	case int16:
		return int64(x)
	case uint16:
		return int64(x)
	case int32:
		return int64(x)
	case uint32:
		return int64(x)
	case int64:
		return x
	case uint64:
		return int64(x)
	case float32:
		return int64(x)
	case float64:
		return int64(x)
	default:
		panic(fmt.Sprintf("Expected a numeric auto increment value, but got %T", x))
	}
}

// startsWithNumber は、v が前後の空白を除いて符号と数字で始まる文字列か。
func startsWithNumber(v interface{}) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	s = strings.TrimLeft(strings.Trim(s, sql.NumericCutSet), "+-")
	return s != "" && (s[0] >= '0' && s[0] <= '9' || s[0] == '.' && len(s) > 1 && s[1] >= '0' && s[1] <= '9')
}

// invalidStringBytes は、v の最初の不正な UTF-8 のバイトから 6 バイトまでを、MySQL の誤りの文と同じ形（\xFF）で書く。
func invalidStringBytes(v interface{}) string {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		return fmt.Sprint(v)
	}
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n <= 1 {
			s = s[i:]
			break
		}
		i += n
	}
	var b strings.Builder
	for i := 0; i < len(s) && i < 6; i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "\\x%02X", c)
		}
	}
	if len(s) > 6 {
		b.WriteString("...")
	}
	return b.String()
}

// trailingSpacesFit returns str cut to the string column type t's length when only spaces are past it (HeimDB).
func trailingSpacesFit(t sql.Type, str string) (string, bool) {
	st, ok := t.(sql.StringType)
	if !ok {
		return "", false
	}
	max := st.MaxCharacterLength()
	r := []rune(str)
	if int64(len(r)) <= max || max <= 0 {
		return "", false
	}
	for _, c := range r[max:] {
		if c != ' ' {
			return "", false
		}
	}
	return string(r[:max]), true
}
