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
//
// Modified by Tsukinowa Inc.: 文字列と真偽値の ABS を MySQL と同じく double にし、日時は数の形にして返す（上流の #3365。docs/engine.md）
// Modified by Tsukinowa Inc.: ABS の TIME と DATE() の値を数の形にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列の ABS で警告 1292 を出さない（段階 119。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の ABS の型を、MySQL と同じ D 桁の DOUBLE にする（段階 120。docs/engine.md）

package function

import (
	"fmt"
	"math"
	"time"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// AbsVal is a function that takes the absolute value of a number
type AbsVal struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*AbsVal)(nil)
var _ sql.CollationCoercible = (*AbsVal)(nil)

// NewAbsVal creates a new AbsVal expression.
func NewAbsVal(e sql.Expression) sql.Expression {
	return &AbsVal{expression.UnaryExpression{Child: e}}
}

// FunctionName implements sql.FunctionExpression
func (t *AbsVal) FunctionName() string {
	return "abs"
}

// Description implements sql.FunctionExpression
func (t *AbsVal) Description() string {
	return "returns the absolute value of an expression."
}

// Eval implements the Expression interface.
func (t *AbsVal) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	val, err := t.Child.Eval(ctx, row)

	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil
	}

	// HeimDB: a TIME, or a date string of a date type (DATE()), is its number
	// (ABS(t) of 13:04:05 is 130405, ABS(DATE(d)) is 20240101; stage 116)
	if _, isTime := val.(time.Time); !isTime && val != nil {
		if n, ok := types.TemporalNumber(ctx, t.Child.Type(), val); ok {
			if types.IsTime(t.Child.Type()) {
				f, _ := n.Float64()
				return math.Abs(f), nil
			}
			return n.Abs(), nil
		}
	}

	// Fucking Golang
	switch x := val.(type) {
	case uint, uint64, uint32, uint16, uint8:
		return x, nil
	case int:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case int64:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case int32:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case int16:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case int8:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case float64:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case float32:
		if x < 0 {
			return -x, nil
		} else {
			return x, nil
		}
	case decimal.Decimal:
		return x.Abs(), nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case time.Time:
		// 日時は、MySQL と同じく数の形（DATETIME は YYYYMMDDhhmmss、DATE は YYYYMMDD）の絶対値（段階 114）
		return temporalNumber(t.Child.Type(), x), nil
	}

	// 文字列などは、MySQL と同じく double にして（数にできない文字列は先頭の数の部分で、警告 1292）絶対値を返す
	// （上流の #3365。前は NULL）。double にできない値は前と同じく NULL
	v, _, err := types.Float64.Convert(ctx, val)
	if err != nil {
		if !types.TruncatedString(val, err) || v == nil {
			return nil, nil
		}
		if !expression.IsTextColumn(t.Child) { // HeimDB: not for a TEXT column, as in MySQL (stage 119)
			ctx.Warn(mysql.ERTruncatedWrongValue, "Truncated incorrect DOUBLE value: '%v'", val)
		}
	}
	f, ok := v.(float64)
	if !ok {
		return nil, nil
	}
	return math.Abs(f), nil
}

// String implements the fmt.Stringer interface.
func (t *AbsVal) String() string {
	return fmt.Sprintf("%s(%s)", t.FunctionName(), t.Child.String())
}

func (t *AbsVal) DebugString() string {
	return fmt.Sprintf("%s(%s)", t.FunctionName(), sql.DebugString(t.Child))
}

// IsNullable implements the Expression interface.
func (t *AbsVal) IsNullable() bool {
	return t.Child.IsNullable()
}

// WithChildren implements the Expression interface.
func (t *AbsVal) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(t, len(children), 1)
	}
	return NewAbsVal(children[0]), nil
}

// Type implements the Expression interface.
func (t *AbsVal) Type() sql.Type {
	// 数でない引数（日時も。MySQL は DOUBLE）の結果は DOUBLE（上流の #3365）
	if typ := t.Child.Type(); types.IsNumber(typ) {
		if scale, ok := types.RealScale(typ); ok {
			return types.RealResultType(scale) // HeimDB: FLOAT(M,D) は MySQL と同じく D 桁の DOUBLE（段階 120）
		}
		return typ
	}
	return types.Float64
}

// temporalNumber は、日時 v の MySQL の数の形（DATE は YYYYMMDD、ほかは YYYYMMDDhhmmss と秒の端数）の DOUBLE。ゼロの日付は 0。
func temporalNumber(typ sql.Type, v time.Time) float64 {
	if v.Equal(types.ZeroTime) {
		return 0
	}
	y, mo, d := v.Date()
	n := float64(y)*10000 + float64(mo)*100 + float64(d)
	if types.IsDateType(typ) {
		return n
	}
	h, mi, sec := v.Clock()
	n = n*1000000 + float64(h)*10000 + float64(mi)*100 + float64(sec)
	return n + float64(v.Nanosecond()/1000)/1e6
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (*AbsVal) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}
