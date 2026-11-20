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

// Modified by Tsukinowa Inc.: ON UPDATE の列の式に「派生」の印を付ける（上流の #3468 を小さく。docs/engine.md）
// Modified by Tsukinowa Inc.: SET の数の列の範囲の外の値を誤りにする（STRICT の UPDATE の 1264。段階 116。docs/engine.md）

package expression

import (
	"fmt"

	"gopkg.in/src-d/go-errors.v1"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

var errCannotSetField = errors.NewKind("Expected GetField expression on left but got %T")

// SetField updates the value of a field or a system variable
type SetField struct {
	BinaryExpressionStub
	// derived は、文に書かれていない、ON UPDATE の列の式（NewDerivedSetField）。明示の式で行が変わらなければ当てない。
	derived bool
}

var _ sql.Expression = (*SetField)(nil)
var _ sql.CollationCoercible = (*SetField)(nil)

// NewSetField creates a new SetField expression.
func NewSetField(left, expr sql.Expression) sql.Expression {
	return &SetField{BinaryExpressionStub: BinaryExpressionStub{LeftChild: left, RightChild: expr}}
}

// NewDerivedSetField は、ON UPDATE の列の式の SetField（IsDerived）。
func NewDerivedSetField(left, expr sql.Expression) sql.Expression {
	return &SetField{BinaryExpressionStub: BinaryExpressionStub{LeftChild: left, RightChild: expr}, derived: true}
}

// IsDerived は、文に書かれていない ON UPDATE の列の式か。
func (s *SetField) IsDerived() bool {
	return s.derived
}

// IsDerivedSetField は、e が ON UPDATE の列の式の SetField か。
func IsDerivedSetField(e sql.Expression) bool {
	sf, ok := e.(*SetField)
	return ok && sf.derived
}

func (s *SetField) String() string {
	return fmt.Sprintf("SET %s = %s", s.LeftChild, s.RightChild)
}

func (s *SetField) DebugString() string {
	return fmt.Sprintf("SET %s = %s", sql.DebugString(s.LeftChild), sql.DebugString(s.RightChild))
}

// Type implements the Expression interface.
func (s *SetField) Type() sql.Type {
	return s.LeftChild.Type()
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (s *SetField) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, s.LeftChild)
}

// Eval implements the Expression interface.
// Returns a copy of the given row with an updated value.
func (s *SetField) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	getField, ok := s.LeftChild.(*GetField)
	if !ok {
		return nil, errCannotSetField.New(s.LeftChild)
	}

	if getField.fieldIndex < 0 || getField.fieldIndex >= len(row) {
		return nil, ErrIndexOutOfBounds.New(getField.fieldIndex, len(row))
	}
	val, err := s.RightChild.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	if val != nil {
		convertedVal, inRange, err := getField.fieldType.Convert(ctx, val)
		if err == nil && inRange == sql.OutOfRange && isNumberColumn(getField.fieldType) {
			// HeimDB: a value out of a number column's range is an error (1264 in UPDATE without IGNORE), not the
			// type's end value, as in MySQL's strict mode (a non-strict UPDATE clamps it before; stage 116)
			err = sql.ErrValueOutOfRange.New(val, getField.fieldType)
		}
		if err != nil {
			// Fill in error with information
			if types.ErrLengthBeyondLimit.Is(err) {
				return nil, sql.NewWrappedTypeConversionError(val, getField.fieldIndex, types.ErrLengthBeyondLimit.New(val, getField.Name()))
			}
			return nil, sql.NewWrappedTypeConversionError(val, getField.fieldIndex, err)
		}
		val = convertedVal
	}
	updatedRow := row.Copy()
	updatedRow[getField.fieldIndex] = val
	return updatedRow, nil
}

// WithChildren implements the Expression interface.
func (s *SetField) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(s, len(children), 2)
	}
	return &SetField{BinaryExpressionStub: BinaryExpressionStub{LeftChild: children[0], RightChild: children[1]}, derived: s.derived}, nil
}

// isNumberColumn reports whether t is an integer, floating point or DECIMAL type.
func isNumberColumn(t sql.Type) bool {
	_, ok := t.(sql.NumberType)
	return ok || types.IsDecimal(t)
}
