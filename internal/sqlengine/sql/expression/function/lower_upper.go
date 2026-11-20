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
// Modified by Tsukinowa Inc.: ENUM と SET の LOWER、UPPER の結果を文字列にする（上流の #3068 の取り込みで。docs/engine.md）

package function

import (
	"fmt"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Lower is a function that returns the lowercase of the text provided.
type Lower struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Lower)(nil)
var _ sql.CollationCoercible = (*Lower)(nil)

// NewLower creates a new Lower expression.
func NewLower(e sql.Expression) sql.Expression {
	return &Lower{expression.UnaryExpression{Child: e}}
}

// FunctionName implements sql.FunctionExpression
func (l *Lower) FunctionName() string {
	return "lower"
}

// Description implements sql.FunctionExpression
func (l *Lower) Description() string {
	return "returns the string str with all characters in lower case."
}

// Eval implements the Expression interface.
func (l *Lower) Eval(
	ctx *sql.Context,
	row sql.Row,
) (interface{}, error) {
	v, err := l.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if v == nil {
		return nil, nil
	}

	vStr, collation, err := types.ConvertToCollatedString(ctx, v, l.Child.Type())
	if err != nil {
		return nil, err
	}
	return collation.CharacterSet().Encoder().Lowercase(vStr), nil
}

func (l *Lower) String() string {
	return fmt.Sprintf("%s(%s)", l.FunctionName(), l.Child)
}

// WithChildren implements the Expression interface.
func (l *Lower) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(l, len(children), 1)
	}
	return NewLower(children[0]), nil
}

// Type implements the Expression interface.
func (l *Lower) Type() sql.Type {
	// ENUM と SET の結果は文字列（値の型のままだと、大文字にした値を ENUM の値として書いてしまう。上流の #3068 の取り込みで）
	if t := l.Child.Type(); types.IsEnum(t) || types.IsSet(t) {
		return collatedText(types.LongText, l.Child)
	}
	return l.Child.Type()
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (l *Lower) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, l.Child)
}

// Upper is a function that returns the UPPERCASE of the text provided.
type Upper struct {
	expression.UnaryExpression
}

var _ sql.FunctionExpression = (*Upper)(nil)
var _ sql.CollationCoercible = (*Upper)(nil)

// NewUpper creates a new Lower expression.
func NewUpper(e sql.Expression) sql.Expression {
	return &Upper{expression.UnaryExpression{Child: e}}
}

// FunctionName implements sql.FunctionExpression
func (u *Upper) FunctionName() string {
	return "upper"
}

// Description implements sql.FunctionExpression
func (u *Upper) Description() string {
	return "converts string to uppercase."
}

// Eval implements the Expression interface.
func (u *Upper) Eval(
	ctx *sql.Context,
	row sql.Row,
) (interface{}, error) {
	v, err := u.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if v == nil {
		return nil, nil
	}

	vStr, collation, err := types.ConvertToCollatedString(ctx, v, u.Child.Type())
	if err != nil {
		return nil, err
	}
	return collation.CharacterSet().Encoder().Uppercase(vStr), nil
}

func (u *Upper) String() string {
	return fmt.Sprintf("%s(%s)", u.FunctionName(), u.Child)
}

// WithChildren implements the Expression interface.
func (u *Upper) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(u, len(children), 1)
	}
	return NewUpper(children[0]), nil
}

// Type implements the Expression interface.
func (u *Upper) Type() sql.Type {
	// ENUM と SET の結果は文字列（値の型のままだと、大文字にした値を ENUM の値として書いてしまう。上流の #3068 の取り込みで）
	if t := u.Child.Type(); types.IsEnum(t) || types.IsSet(t) {
		return collatedText(types.LongText, u.Child)
	}
	return u.Child.Type()
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (u *Upper) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, u.Child)
}
