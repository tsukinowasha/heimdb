// Copyright 2020-2021 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 文字列の関数の結果の型に、引数の照合順序を付ける（docs/engine.md）
// Modified by Tsukinowa Inc.: 数の IF の型を両方の枝から決め、値をその型に変え、文字列の IF の ENUM と SET の枝を値の文字列にする（上流の #3025。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時か ENUM と数の IF を文字列にし、日時の枝を型の形の文字列にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時の枝の結果の型を、MySQL と同じく枝の中で最も多い小数の桁の DATETIME にする（types.TemporalBranchType。段階 116 の 4 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列の IF(v, ...) で警告 1292 を出さない（ConvertToBoolFor。段階 119。docs/engine.md）

package function

import (
	"fmt"
	"sync/atomic"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// If function returns the second value if the first is true, the third value otherwise.
type If struct {
	expr    sql.Expression
	ifTrue  sql.Expression
	ifFalse sql.Expression
	// conv は、値を変える型（数の結果で、枝の型が結果の型と違うとき）を、最初の Eval で求めて覚える（子は変わらない
	// ので、式ごとに 1 回。上流の #3025 の変換を行ごとに求めない）。
	conv atomic.Pointer[ifConversion]
}

// ifConversion は、If の真と偽の枝の値を変える型（変えなければ nil）。
type ifConversion struct {
	ifTrue, ifFalse sql.Type
	// trueEnum と falseEnum は、文字列の結果の ENUM と SET の枝の型（値は番号なので値の文字列にする。段階 114）
	trueEnum, falseEnum sql.Type
	// text は、結果が文字列か（日付と日時の枝を型の形の文字列にする。段階 116）
	text bool
}

// conversion は、f の枝の値を変える型を返す。
func (f *If) conversion() *ifConversion {
	if c := f.conv.Load(); c != nil {
		return c
	}
	c := &ifConversion{}
	if t := f.Type(); types.IsNumber(t) {
		branchConv := func(bt sql.Type) sql.Type {
			if bt.Equals(t) {
				return nil
			}
			// DECIMAL の枝は、その型の小数の桁のまま（MySQL と同じく IF(id = 2, 0, r) の r は 12.3400、0 は 0）
			if types.IsDecimal(bt) && types.IsDecimal(t) {
				return bt
			}
			return t
		}
		c.ifTrue = branchConv(f.ifTrue.Type())
		c.ifFalse = branchConv(f.ifFalse.Type())
	} else if types.IsText(t) {
		c.text = true
		enumType := func(bt sql.Type) sql.Type {
			if types.IsEnum(bt) || types.IsSet(bt) {
				return bt
			}
			return nil
		}
		c.trueEnum = enumType(f.ifTrue.Type())
		c.falseEnum = enumType(f.ifFalse.Type())
	}
	f.conv.Store(c)
	return c
}

var _ sql.FunctionExpression = (*If)(nil)
var _ sql.CollationCoercible = (*If)(nil)

// FunctionName implements sql.FunctionExpression
func (f *If) FunctionName() string {
	return "if"
}

// Description implements sql.FunctionExpression
func (f *If) Description() string {
	return "if expr evaluates to true, returns ifTrue. Otherwise returns ifFalse."
}

func (f *If) Resolved() bool {
	return f.expr.Resolved() && f.ifTrue.Resolved() && f.ifFalse.Resolved()
}

func (f *If) Children() []sql.Expression {
	return []sql.Expression{
		f.expr, f.ifTrue, f.ifFalse,
	}
}

// NewIf returns a new IF UDF
func NewIf(expr, ifTrue, ifFalse sql.Expression) sql.Expression {
	return &If{
		expr:    expr,
		ifTrue:  ifTrue,
		ifFalse: ifFalse,
	}
}

// Eval implements the Expression interface.
func (f *If) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	e, err := f.expr.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	var asBool bool
	if e == nil {
		asBool = false
	} else {
		asBool, err = sql.ConvertToBoolFor(ctx, f.expr, e)
		if err != nil {
			return nil, err
		}
	}

	c := f.conversion()
	branch, conv, enum := f.ifFalse, c.ifFalse, c.falseEnum
	if asBool {
		branch, conv, enum = f.ifTrue, c.ifTrue, c.trueEnum
	}
	v, err := branch.Eval(ctx, row)
	if err != nil || v == nil {
		return v, err
	}
	// IF(c, enum_col, 'none') は、MySQL と同じく ENUM の値の文字列（段階 114）
	if enum != nil {
		if str, ok := types.EnumOrSetString(enum, v); ok {
			return str, nil
		}
	}
	// 結果が文字列なら、日付と日時も型の形の文字列（IF(c, d, 0) の d。段階 116）
	if c.text {
		if str, ok := types.TemporalString(branch.Type(), v); ok {
			return str, nil
		}
	}
	// 数の結果は IF の型に変える（TRUE と 1 を同じ値にし、IF(c, 0, 2.5) の 2.5 を整数にしない。上流の #3025）。
	// 枝の型が結果の型と同じなら変えない（文字列の結果は前と同じく値のまま）
	if conv != nil {
		if cv, _, err := conv.Convert(ctx, v); err == nil {
			// HeimDB: a DECIMAL branch shows its type's decimals (stage 116)
			return expression.BranchDecimal(branch.Type(), cv), nil
		}
	}
	return expression.BranchDecimal(branch.Type(), v), nil
}

// Type implements the Expression interface.
func (f *If) Type() sql.Type {
	// if either type is string type, this should be a string type, regardless need to promote
	typ1 := f.ifTrue.Type()
	typ2 := f.ifFalse.Type()
	if types.IsText(typ1) || types.IsText(typ2) || expression.MixedBranchTypesAreText(typ1, typ2) {
		return collatedText(types.Text, f.ifTrue, f.ifFalse)
	}

	// HeimDB: dates and datetimes are DATETIME of the largest fractional digits of the
	// branches, as in MySQL (IF(c, d0, NULL) was DATETIME(6); stage 116, fourth review)
	if t, ok := types.TemporalBranchType(typ1, typ2); ok {
		return t
	}
	if typ1 == types.Null {
		return typ2.Promote()
	}
	// 数と数は、CASE と同じく両方の値を表せる型（整数と DECIMAL は DECIMAL、DOUBLE があれば DOUBLE。上流の #3025 の
	// 問題。前は ifTrue の型で、IF(c, 0, 2.5) が 3、IF(id = 2, 0, net_revenue) が整数になった）
	if typ2 != types.Null && types.IsNumber(typ1) && types.IsNumber(typ2) {
		return expression.CombinedBranchType(typ1, typ2)
	}
	return typ1.Promote()
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (f *If) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	// We would need to evaluate the condition to return the correct result here, so we'll copy
	// Type and just return the true result
	return sql.GetCoercibility(ctx, f.ifTrue)
}

// IsNullable implements the Expression interface.
func (f *If) IsNullable() bool {
	return f.ifTrue.IsNullable()
}

func (f *If) String() string {
	return fmt.Sprintf("%s(%s,%s,%s)", f.FunctionName(), f.expr, f.ifTrue, f.ifFalse)
}

// WithChildren implements the Expression interface.
func (f *If) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 3 {
		return nil, sql.ErrInvalidChildrenNumber.New(f, len(children), 3)
	}
	return NewIf(children[0], children[1], children[2]), nil
}
