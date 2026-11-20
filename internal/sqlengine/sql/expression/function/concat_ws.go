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
// Modified by Tsukinowa Inc.: ENUM と SET の列の値を、文字列の文脈で値の文字列として扱う（上流の #3068。docs/engine.md）
// Modified by Tsukinowa Inc.: バイナリの引数のある CONCAT_WS をバイト列のまま組み立てる（上流の #3078 の問題。docs/engine.md）

// Modified by Tsukinowa Inc.: 文字列の関数の結果の型に、引数の照合順序を付ける（docs/engine.md）
// Modified by Tsukinowa Inc.: CONCAT_WS の日付と日時を型の形の文字列にする（段階 116。docs/engine.md）

package function

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// ConcatWithSeparator joins several strings together. The first argument is
// the separator for the rest of the arguments. The separator is added between
// the strings to be concatenated. The separator can be a string, as can the
// rest of the arguments. If the separator is NULL, the result is NULL.
type ConcatWithSeparator struct {
	args []sql.Expression
	// binary は、バイナリの引数があるか（0 はまだ見ていない、1 はない、2 はある）。子は変わらないので、Eval の
	// 最初に 1 回だけ見る（段階 114）
	binary atomic.Int32
}

var _ sql.FunctionExpression = (*ConcatWithSeparator)(nil)
var _ sql.CollationCoercible = (*ConcatWithSeparator)(nil)

// NewConcatWithSeparator creates a new NewConcatWithSeparator UDF.
func NewConcatWithSeparator(args ...sql.Expression) (sql.Expression, error) {
	if len(args) == 0 {
		return nil, sql.ErrInvalidArgumentNumber.New("CONCAT_WS", "1 or more", 0)
	}

	return &ConcatWithSeparator{args: args}, nil
}

// FunctionName implements sql.FunctionExpression
func (f *ConcatWithSeparator) FunctionName() string {
	return "concat_ws"
}

// Description implements sql.FunctionExpression
func (f *ConcatWithSeparator) Description() string {
	return "concatenates any group of fields into a single string. The first argument is the separator for the rest of the arguments. The separator is added between the strings to be concatenated. The separator can be a string, as can the rest of the arguments. If the separator is NULL, the result is NULL."
}

// Type implements the Expression interface.
func (f *ConcatWithSeparator) Type() sql.Type {
	// バイナリの引数があれば、MySQL と同じく結果はバイナリの文字列（段階 114）
	if hasBinaryArg(f.args) {
		return types.LongBlob
	}
	return collatedText(types.LongText, f.args...)
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (c *ConcatWithSeparator) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	if len(c.args) == 0 {
		return sql.Collation_binary, 6
	}
	collation, coercibility = sql.GetCoercibility(ctx, c.args[0])
	for i := 1; i < len(c.args); i++ {
		nextCollation, nextCoercibility := sql.GetCoercibility(ctx, c.args[i])
		collation, coercibility = sql.ResolveCoercibility(collation, coercibility, nextCollation, nextCoercibility)
	}
	return collation, coercibility
}

// IsNullable implements the Expression interface.
func (f *ConcatWithSeparator) IsNullable() bool {
	for _, arg := range f.args {
		if arg.IsNullable() {
			return true
		}
	}
	return false
}

func (f *ConcatWithSeparator) String() string {
	var args = make([]string, len(f.args))
	for i, arg := range f.args {
		args[i] = arg.String()
	}
	return fmt.Sprintf("%s(%s)", f.FunctionName(), strings.Join(args, ","))
}

// WithChildren implements the Expression interface.
func (*ConcatWithSeparator) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewConcatWithSeparator(children...)
}

// Resolved implements the Expression interface.
func (f *ConcatWithSeparator) Resolved() bool {
	for _, arg := range f.args {
		if !arg.Resolved() {
			return false
		}
	}
	return true
}

// Children implements the Expression interface.
func (f *ConcatWithSeparator) Children() []sql.Expression { return f.args }

// Eval implements the Expression interface.
func (f *ConcatWithSeparator) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	if f.hasBinary() {
		var parts [][]byte
		for i, arg := range f.args {
			val, err := arg.Eval(ctx, row)
			if err != nil {
				return nil, err
			}
			if val == nil {
				if i == 0 {
					return nil, nil
				}
				continue
			}
			b, err := binaryArgBytes(ctx, arg, val)
			if err != nil {
				return nil, err
			}
			parts = append(parts, b)
		}
		out := bytes.Join(parts[1:], parts[0])
		if out == nil {
			out = []byte{}
		}
		return out, nil
	}
	var parts []string

	for i, arg := range f.args {
		val, err := arg.Eval(ctx, row)
		if err != nil {
			return nil, err
		}

		if val == nil && i == 0 {
			return nil, nil
		}

		if val == nil {
			continue
		}

		// ENUM と SET は番号ではなく値の文字列（上流の #3068）
		if s, ok := types.ValueString(arg.Type(), val); ok {
			val = s
		}
		val, _, err = types.LongText.Convert(ctx, val)
		if err != nil {
			return nil, err
		}

		parts = append(parts, val.(string))
	}

	return strings.Join(parts[1:], parts[0]), nil
}

// hasBinary は、hasBinaryArg を式ごとに 1 回だけ求めて覚える。
func (f *ConcatWithSeparator) hasBinary() bool {
	switch f.binary.Load() {
	case 1:
		return false
	case 2:
		return true
	}
	b := hasBinaryArg(f.args)
	if b {
		f.binary.Store(2)
	} else {
		f.binary.Store(1)
	}
	return b
}
