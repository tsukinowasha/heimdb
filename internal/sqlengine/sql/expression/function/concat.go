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
// Modified by Tsukinowa Inc.: バイナリの引数のある CONCAT をバイト列のまま組み立てる（上流の #3078 の問題。docs/engine.md）

// Modified by Tsukinowa Inc.: 文字列の関数の結果の型に、引数の照合順序を付ける（docs/engine.md）
// Modified by Tsukinowa Inc.: CONCAT の日付と日時を型の形の文字列にする（段階 116。docs/engine.md）

package function

import (
	"fmt"
	"github.com/dolthub/vitess/go/sqltypes"
	"strings"
	"sync/atomic"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Concat joins several strings together.
type Concat struct {
	args []sql.Expression
	// binary は、バイナリの引数があるか（0 はまだ見ていない、1 はない、2 はある）。子は変わらないので、Eval の
	// 最初に 1 回だけ見る（段階 114）
	binary atomic.Int32
}

var _ sql.FunctionExpression = (*Concat)(nil)
var _ sql.CollationCoercible = (*Concat)(nil)

// NewConcat creates a new Concat UDF.
func NewConcat(args ...sql.Expression) (sql.Expression, error) {
	if len(args) == 0 {
		return nil, sql.ErrInvalidArgumentNumber.New("CONCAT", "1 or more", 0)
	}

	return &Concat{args: args}, nil
}

// FunctionName implements sql.FunctionExpression
func (c *Concat) FunctionName() string {
	return "concat"
}

// Description implements sql.FunctionExpression
func (c *Concat) Description() string {
	return "concatenates any group of fields into a single string."
}

// Type implements the Expression interface.
func (c *Concat) Type() sql.Type {
	// バイナリの引数があれば、MySQL と同じく結果はバイナリの文字列（段階 114、上流の #3078 の問題）
	if hasBinaryArg(c.args) {
		return types.LongBlob
	}
	return collatedText(types.LongText, c.args...)
}

// hasBinaryArg は、args にバイナリの文字列の型（BLOB、VARBINARY、BINARY）の式があるか。
func hasBinaryArg(args []sql.Expression) bool {
	for _, arg := range args {
		if arg == nil || !arg.Resolved() {
			continue
		}
		// JSON と GEOMETRY は文字列として扱う（types.IsBinaryType はこれらも含む）
		switch arg.Type().Type() {
		case sqltypes.Binary, sqltypes.VarBinary, sqltypes.Blob:
			return true
		}
	}
	return false
}

// binaryArgBytes は、CONCAT のバイナリの結果に入れる引数の値 val のバイト列（バイト列はそのまま。前は utf8mb4 に
// 変えようとして、UNHEX('FF') などの不正な UTF-8 で 1105 になった）。
func binaryArgBytes(ctx *sql.Context, arg sql.Expression, val interface{}) ([]byte, error) {
	val, err := sql.UnwrapAny(ctx, val)
	if err != nil {
		return nil, err
	}
	switch v := val.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	}
	if s, ok := types.ValueString(arg.Type(), val); ok {
		return []byte(s), nil
	}
	s, _, err := types.LongText.Convert(ctx, val)
	if err != nil {
		return nil, err
	}
	str, _, err := sql.Unwrap[string](ctx, s)
	if err != nil {
		return nil, err
	}
	return []byte(str), nil
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (c *Concat) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
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
func (c *Concat) IsNullable() bool {
	for _, arg := range c.args {
		if arg.IsNullable() {
			return true
		}
	}
	return false
}

func (c *Concat) String() string {
	var args = make([]string, len(c.args))
	for i, arg := range c.args {
		args[i] = arg.String()
	}
	return fmt.Sprintf("%s(%s)", c.FunctionName(), strings.Join(args, ","))
}

func (c *Concat) DebugString() string {
	var args = make([]string, len(c.args))
	for i, arg := range c.args {
		args[i] = sql.DebugString(arg)
	}
	return fmt.Sprintf("%s(%s)", c.FunctionName(), strings.Join(args, ","))
}

// WithChildren implements the Expression interface.
func (*Concat) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewConcat(children...)
}

// Resolved implements the Expression interface.
func (c *Concat) Resolved() bool {
	for _, arg := range c.args {
		if !arg.Resolved() {
			return false
		}
	}
	return true
}

// Children implements the Expression interface.
func (c *Concat) Children() []sql.Expression { return c.args }

// Eval implements the Expression interface.
func (c *Concat) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	if c.hasBinary() {
		var out []byte
		for _, arg := range c.args {
			val, err := arg.Eval(ctx, row)
			if err != nil || val == nil {
				return nil, err
			}
			b, err := binaryArgBytes(ctx, arg, val)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
		if out == nil {
			out = []byte{}
		}
		return out, nil
	}
	var parts []string

	for _, arg := range c.args {
		val, err := arg.Eval(ctx, row)
		if err != nil {
			return nil, err
		}

		if val == nil {
			return nil, nil
		}

		// ENUM と SET は番号ではなく値の文字列（上流の #3068）
		if s, ok := types.ValueString(arg.Type(), val); ok {
			val = s
		}
		val, _, err = types.LongText.Convert(ctx, val)
		if err != nil {
			return nil, err
		}

		val, _, err = sql.Unwrap[string](ctx, val)
		if err != nil {
			return nil, err
		}

		parts = append(parts, val.(string))
	}

	return strings.Join(parts, ""), nil
}

// hasBinary は、hasBinaryArg を式ごとに 1 回だけ求めて覚える。
func (c *Concat) hasBinary() bool {
	switch c.binary.Load() {
	case 1:
		return false
	case 2:
		return true
	}
	b := hasBinaryArg(c.args)
	if b {
		c.binary.Store(2)
	} else {
		c.binary.Store(1)
	}
	return b
}
