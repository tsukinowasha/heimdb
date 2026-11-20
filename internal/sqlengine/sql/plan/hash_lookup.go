// Copyright 2021 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: ハッシュ結合の文字列のキーを、照合順序の重みでハッシュする（docs/engine.md）
// Modified by Tsukinowa Inc.: 両辺の型から比べる型を求め、キーをその型に変える（上流の #3200。docs/engine.md）
// Modified by Tsukinowa Inc.: 日時と文字列のキーを日時にし、後ろを切り詰めた値と、読めない文字列をゼロの日時としてハッシュする（上流の #3276 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL のキーを、後ろの 0 を除いた文字列にしてハッシュする（上流の #3191。docs/engine.md）

package plan

import (
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// NewHashLookup returns a node that performs an indexed hash lookup
// of cached rows for fulfilling RowIter() calls. In particular, this
// node sits directly on top of a `CachedResults` node and has two
// expressions: a projection for hashing the Child row results and
// another projection for hashing the parent row values when
// performing a lookup. When RowIter is called, if cached results are
// available, it fulfills the RowIter call by performing a hash lookup
// on the projected results. If cached results are not available, it
// simply delegates to the child.
func NewHashLookup(n sql.Node, rightEntryKey sql.Expression, leftProbeKey sql.Expression, joinType JoinType) *HashLookup {
	return &HashLookup{
		UnaryNode:     UnaryNode{n},
		RightEntryKey: rightEntryKey,
		LeftProbeKey:  leftProbeKey,
		CompareType:   GetCompareType(leftProbeKey.Type(), rightEntryKey.Type()),
		Mutex:         new(sync.Mutex),
		JoinType:      joinType,
	}
}

type HashLookup struct {
	UnaryNode
	RightEntryKey sql.Expression
	LeftProbeKey  sql.Expression
	CompareType   sql.Type
	Mutex         *sync.Mutex
	Lookup        *map[interface{}][]sql.Row
	JoinType      JoinType
}

var _ sql.Node = (*HashLookup)(nil)
var _ sql.Expressioner = (*HashLookup)(nil)
var _ sql.CollationCoercible = (*HashLookup)(nil)

// GetCompareType は、型 left と right の値を比べるときの型（上流の #3200）。両辺のキーをこの型に変えてからハッシュ
// する。
//
// HeimDB: 上流は時刻、JSON、バイナリの組み合わせも扱い、それ以外を LONGTEXT にするが、ここでは数と、数か文字列の
// 組だけを変え、ほかは前と同じく左の型にする（文字列どうしは左の照合順序の重みでハッシュする。ENUM などの値を
// LONGTEXT に変えると番号の文字列になる）。MySQL は数と文字列を double で比べる（数にできない文字列は 0）ので、
// BOOLEAN の 0 は 'g' にも等しいが、左の VARCHAR に揃えると '0' にしか当たらなかった。double は値をまとめる向きに
// しか丸めない（等しい値は同じ double になる）ので、引いた後で結合の条件を評価し直せば結果は変わらない。
//
// 複数の列のキー（タプル）は、列ごとに比べる型を求める（上流は左のタプルの型のまま。NATURAL JOIN の BOOLEAN と
// VARCHAR の組で、左の BOOLEAN に揃えると STRICT の sql_mode で 1366 になった）。
func GetCompareType(left, right sql.Type) sql.Type {
	if left.Equals(right) {
		return left
	}
	if lt, ok := left.(types.TupleType); ok {
		rt, ok := right.(types.TupleType)
		if !ok || len(lt) != len(rt) {
			return left
		}
		typs := make([]sql.Type, len(lt))
		for i := range lt {
			typs[i] = GetCompareType(lt[i], rt[i])
		}
		return types.CreateTuple(typs...)
	}
	// HeimDB: 日時と文字列は、比べる側（comparison の castLeftAndRight）と同じく日時にしてハッシュする（段階 114。
	// 左の LONGTEXT に揃えると、'2024/1/3 3:4:5' や後ろを切り詰めて読む '2024-01-01 09:00:00 x' が当たらなかった）
	if (types.IsTime(left) && types.IsText(right)) || (types.IsText(left) && types.IsTime(right)) {
		return types.DatetimeMaxPrecision
	}
	lNum, rNum := types.IsNumber(left), types.IsNumber(right)
	if !(lNum && (rNum || types.IsText(right))) && !(rNum && types.IsText(left)) {
		return left
	}
	if lNum && rNum && types.IsInteger(left) && types.IsInteger(right) {
		if types.IsSigned(left) && types.IsSigned(right) {
			return types.Int64
		}
		if types.IsUnsigned(left) && types.IsUnsigned(right) {
			return types.Uint64
		}
	}
	return types.Float64
}

func (n *HashLookup) Expressions() []sql.Expression {
	return []sql.Expression{n.RightEntryKey, n.LeftProbeKey}
}

func (n *HashLookup) IsReadOnly() bool {
	return n.Child.IsReadOnly()
}

func (n *HashLookup) WithExpressions(exprs ...sql.Expression) (sql.Node, error) {
	if len(exprs) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(n, len(exprs), 2)
	}
	ret := *n
	ret.RightEntryKey = exprs[0]
	ret.LeftProbeKey = exprs[1]
	return &ret, nil
}

func (n *HashLookup) String() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("HashLookup")
	children := make([]string, 3)
	children[0] = fmt.Sprintf("left-key: %s", n.LeftProbeKey)
	children[1] = fmt.Sprintf("right-key: %s", n.RightEntryKey)
	children[2] = n.Child.String()
	_ = pr.WriteChildren(children...)
	return pr.String()
}

func (n *HashLookup) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("HashLookup")
	children := make([]string, 3)
	children[0] = fmt.Sprintf("left-key: %s", sql.DebugString(n.LeftProbeKey))
	children[1] = fmt.Sprintf("right-key: %s", sql.DebugString(n.RightEntryKey))
	children[2] = sql.DebugString(n.Child)
	_ = pr.WriteChildren(children...)
	return pr.String()
}

func (n *HashLookup) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(n, len(children), 1)
	}
	nn := *n
	nn.UnaryNode.Child = children[0]
	return &nn, nil
}

// CollationCoercibility implements the interface sql.CollationCoercible.
func (n *HashLookup) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, n.Child)
}

// Convert a tuple expression returning []interface{} into something comparable.
// Fast paths a few smaller slices into fixed size arrays, puts everything else
// through string serialization and a hash for now. It is OK to hash lossy here
// as the join condition is still evaluated after the matching rows are returned.
func (n *HashLookup) GetHashKey(ctx *sql.Context, e sql.Expression, row sql.Row) (interface{}, error) {
	key, err := e.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	typ := n.CompareType
	if typ == nil {
		typ = n.LeftProbeKey.Type()
	}
	if s, ok := key.([]interface{}); ok {
		// HeimDB: 複数の列のキーは列ごとに比べる型に変え、文字列は照合順序の重みでハッシュする（1 列のキーと同じ）。
		typs, ok := typ.(types.TupleType)
		if !ok || len(typs) != len(s) {
			return nil, sql.ErrInvalidColumnNumber.New(len(typs), len(s))
		}
		conv := make([]interface{}, len(s))
		for i, v := range s {
			if conv[i], err = convertHashKey(ctx, typs[i], v); err != nil {
				return nil, err
			}
		}
		return sql.HashOfTypes(ctx, conv, typs)
	}
	key, err = convertHashKey(ctx, typ, key)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, nil
	}
	// byte slices are not hashable
	if k, ok := key.([]byte); ok {
		key = string(k)
	}
	// decimal.Decimal is a struct holding a big.Int and an exponent, so equal
	// values with different scales (30.000 and 30.00 from arithmetic) are
	// different map keys; String() drops the trailing zeros (upstream #3191).
	if d, ok := key.(decimal.Decimal); ok {
		key = d.String()
	}
	// HeimDB: 文字列は、照合順序の重みでハッシュする（MySQL と同じく、ON a.s = b.s で 'a' と 'A' を結ぶ。もとは字
	// そのものをキーにしていた）。両側を左のキーの型に変えてから引く。
	if s, ok := key.(string); ok {
		if st, ok := typ.(sql.StringType); ok && st.Collation() != sql.Collation_binary && utf8.ValidString(s) {
			return st.Collation().HashToUint(s)
		}
	}
	return key, nil
}

// convertHashKey は、ハッシュ結合のキーの値 v を比べる型 typ に変える。
func convertHashKey(ctx *sql.Context, typ sql.Type, v interface{}) (interface{}, error) {
	k, _, err := typ.Convert(ctx, v)
	if types.ErrValueNotNil.Is(err) {
		// The LHS expression was NullType. This is allowed.
		return nil, nil
	}
	// HeimDB: 数にできない文字列（'g'、'12abc'）は、先頭の数の部分を読んだ値（0、12）でハッシュする（上流の #3200 の
	// ErrTruncatedIncorrect の扱い。フォークでは段階 85 の types.TruncatedString で見分ける）。
	// 日時の文字列の後ろの余計な文字も、比べる側と同じく切り詰めた値でハッシュする（上流の #3276）
	if err != nil && !types.TruncatedString(v, err) && !types.TruncatedDatetime(err) {
		if types.IsTime(typ) && types.IsText(types.ApproximateTypeFromValue(v)) && types.ErrConvertingToTime.Is(err) {
			// 日時として読めない文字列（'garbage'、''）は、比べる側（comparison の datetimeOrZero）と同じくゼロの日時で
			// ハッシュし、警告 1292 を出す（MySQL と同じくゼロの日時の行に当たる。段階 114 のレビュー）
			ctx.Warn(1292, "Incorrect datetime value: '%v'", v)
			return types.ZeroTime, nil
		}
		return nil, err
	}
	return k, nil
}

func (n *HashLookup) Dispose() {
	n.Lookup = nil
}
