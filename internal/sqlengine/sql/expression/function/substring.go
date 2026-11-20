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
// Modified by Tsukinowa Inc.: SUBSTRING、LEFT、RIGHT、SUBSTRING_INDEX で、数や日時を文字列にしてから扱う（docs/engine.md）
// Modified by Tsukinowa Inc.: INSTR を探す文字列の照合順序で比べる（上流の #3980。docs/engine.md）
// Modified by Tsukinowa Inc.: SUBSTRING、LEFT、RIGHT、SUBSTRING_INDEX の日付と日時を型の形の文字列にする（段階 116。docs/engine.md）

package function

import (
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Substring is a function to return a part of a string.
// This function behaves as the homonym MySQL function.
// Since Go strings are UTF8, this function does not return a direct sub
// string str[start:start+length], instead returns the substring of rune
// s. That is, "á"[0:1] does not return a partial unicode glyph, but "á"
// itself.
type Substring struct {
	Str   sql.Expression
	Start sql.Expression
	Len   sql.Expression
}

var _ sql.FunctionExpression = (*Substring)(nil)
var _ sql.CollationCoercible = (*Substring)(nil)

// NewSubstring creates a new substring UDF.
func NewSubstring(args ...sql.Expression) (sql.Expression, error) {
	var str, start, ln sql.Expression
	switch len(args) {
	case 2:
		str = args[0]
		start = args[1]
		ln = nil
	case 3:
		str = args[0]
		start = args[1]
		ln = args[2]
	default:
		return nil, sql.ErrInvalidArgumentNumber.New("SUBSTRING", "2 or 3", len(args))
	}
	return &Substring{str, start, ln}, nil
}

// FunctionName implements sql.FunctionExpression
func (s *Substring) FunctionName() string {
	return "substring"
}

// Description implements sql.FunctionExpression
func (s *Substring) Description() string {
	return "returns a substring from the provided string starting at pos with a length of len characters. If no len is provided, all characters from pos until the end will be taken."
}

// Children implements the Expression interface.
func (s *Substring) Children() []sql.Expression {
	if s.Len == nil {
		return []sql.Expression{s.Str, s.Start}
	}
	return []sql.Expression{s.Str, s.Start, s.Len}
}

// Eval implements the Expression interface.
func (s *Substring) Eval(
	ctx *sql.Context,
	row sql.Row,
) (interface{}, error) {
	str, err := s.Str.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	var text []rune
	switch str := str.(type) {
	case string:
		text = []rune(str)
	case []byte:
		text = []rune(string(str))
	case nil:
		return nil, nil
	default:
		// HeimDB: 数や日時は文字列にしてから扱う（MySQL と同じ。LEFT(123, 2) は '12'。もとはエラーにしていた）。
		// ENUM と SET は値の文字列（上流の #3068）
		if es, ok := types.ValueString(s.Str.Type(), str); ok {
			str = es
		}
		v, _, err := types.LongText.Convert(ctx, str)
		if err != nil {
			return nil, sql.ErrInvalidType.New(reflect.TypeOf(str).String())
		}
		sv, _, err := sql.Unwrap[string](ctx, v)
		if err != nil {
			return nil, err
		}
		text = []rune(sv)
	}

	start, err := s.Start.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if start == nil {
		return nil, nil
	}

	start, _, err = types.Int64.Convert(ctx, start)
	if err != nil {
		return nil, err
	}

	var length int64
	runeCount := int64(len(text))
	if s.Len != nil {
		len, err := s.Len.Eval(ctx, row)
		if err != nil {
			return nil, err
		}

		if len == nil {
			return nil, nil
		}

		len, _, err = types.Int64.Convert(ctx, len)
		if err != nil {
			return nil, err
		}

		length = len.(int64)
	} else {
		length = runeCount
	}

	var startIdx int64
	if start := start.(int64); start < 0 {
		startIdx = runeCount + start
	} else {
		startIdx = start - 1
	}

	if startIdx < 0 || startIdx >= runeCount || length <= 0 {
		return "", nil
	}

	if startIdx+length > runeCount {
		length = int64(runeCount) - startIdx
	}

	return string(text[startIdx : startIdx+length]), nil
}

// IsNullable implements the Expression interface.
func (s *Substring) IsNullable() bool {
	return s.Str.IsNullable() || s.Start.IsNullable() || (s.Len != nil && s.Len.IsNullable())
}

func (s *Substring) String() string {
	if s.Len == nil {
		return fmt.Sprintf("SUBSTRING(%s, %s)", s.Str, s.Start)
	}
	return fmt.Sprintf("SUBSTRING(%s, %s, %s)", s.Str, s.Start, s.Len)
}

// Resolved implements the Expression interface.
func (s *Substring) Resolved() bool {
	return s.Start.Resolved() && s.Str.Resolved() && (s.Len == nil || s.Len.Resolved())
}

// Type implements the Expression interface.
func (s *Substring) Type() sql.Type { return argStringType(s.Str) }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (s *Substring) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, s.Str)
}

// WithChildren implements the Expression interface.
func (*Substring) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewSubstring(children...)
}

// SubstringIndex returns the substring from string str before count occurrences of the delimiter delim.
// If count is positive, everything to the left of the final delimiter (counting from the left) is returned.
// If count is negative, everything to the right of the final delimiter (counting from the right) is returned.
// SUBSTRING_INDEX() performs a case-sensitive match when searching for delim.
type SubstringIndex struct {
	str   sql.Expression
	delim sql.Expression
	count sql.Expression
}

var _ sql.FunctionExpression = (*SubstringIndex)(nil)
var _ sql.CollationCoercible = (*SubstringIndex)(nil)

// NewSubstringIndex creates a new SubstringIndex UDF.
func NewSubstringIndex(str, delim, count sql.Expression) sql.Expression {
	return &SubstringIndex{str, delim, count}
}

// FunctionName implements sql.FunctionExpression
func (s *SubstringIndex) FunctionName() string {
	return "substring_index"
}

// Description implements sql.FunctionExpression
func (s *SubstringIndex) Description() string {
	return "returns a substring after count appearances of delim. If count is negative, counts from the right side of the string."
}

// Children implements the Expression interface.
func (s *SubstringIndex) Children() []sql.Expression {
	return []sql.Expression{s.str, s.delim, s.count}
}

// Eval implements the Expression interface.
func (s *SubstringIndex) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	ex, err := s.str.Eval(ctx, row)
	if ex == nil || err != nil {
		return nil, err
	}
	// ENUM と SET は値の文字列（上流の #3068）
	if es, ok := types.ValueString(s.str.Type(), ex); ok {
		ex = es
	}
	ex, _, err = types.LongText.Convert(ctx, ex)
	if err != nil {
		return nil, err
	}
	str, ok := ex.(string)
	if !ok {
		return nil, sql.ErrInvalidType.New(reflect.TypeOf(ex).String())
	}

	ex, err = s.delim.Eval(ctx, row)
	if ex == nil || err != nil {
		return nil, err
	}
	ex, _, err = types.LongText.Convert(ctx, ex)
	if err != nil {
		return nil, err
	}
	delim, ok := ex.(string)
	if !ok {
		return nil, sql.ErrInvalidType.New(reflect.TypeOf(ex).String())
	}

	ex, err = s.count.Eval(ctx, row)
	if ex == nil || err != nil {
		return nil, err
	}
	ex, _, err = types.Int64.Convert(ctx, ex)
	if err != nil {
		return nil, err
	}
	count, ok := ex.(int64)
	if !ok {
		return nil, sql.ErrInvalidType.New(reflect.TypeOf(ex).String())
	}

	// Implementation taken from pingcap/tidb
	// https://github.com/pingcap/tidb/blob/37c128b64f3ad2f08d52bc767b6e3320ecf429d8/expression/builtin_string.go#L1229
	strs := strings.Split(str, delim)
	start, end := int64(0), int64(len(strs))
	if count > 0 {
		// If count is positive, everything to the left of the final delimiter (counting from the left) is returned.
		if count < end {
			end = count
		}
	} else {
		// If count is negative, everything to the right of the final delimiter (counting from the right) is returned.
		count = -count
		if count < 0 {
			// -count overflows max int64, returns an empty string.
			return "", nil
		}

		if count < end {
			start = end - count
		}
	}

	return strings.Join(strs[start:end], delim), nil
}

// IsNullable implements the Expression interface.
func (s *SubstringIndex) IsNullable() bool {
	return s.str.IsNullable() || s.delim.IsNullable() || s.count.IsNullable()
}

func (s *SubstringIndex) String() string {
	return fmt.Sprintf("SUBSTRING_INDEX(%s, %s, %s)", s.str, s.delim, s.count)
}

// Resolved implements the Expression interface.
func (s *SubstringIndex) Resolved() bool {
	return s.str.Resolved() && s.delim.Resolved() && s.count.Resolved()
}

// Type implements the Expression interface.
func (s *SubstringIndex) Type() sql.Type { return argStringType(s.str) }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (s *SubstringIndex) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, s.str)
}

// WithChildren implements the Expression interface.
func (s *SubstringIndex) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 3 {
		return nil, sql.ErrInvalidChildrenNumber.New(s, len(children), 3)
	}
	return NewSubstringIndex(children[0], children[1], children[2]), nil
}

// Left is a function that returns the first N characters of a string expression.
type Left struct {
	str sql.Expression
	len sql.Expression
}

var _ sql.FunctionExpression = Left{}
var _ sql.CollationCoercible = Left{}

// NewLeft creates a new LEFT function.
func NewLeft(str, len sql.Expression) sql.Expression {
	return Left{str, len}
}

// FunctionName implements sql.FunctionExpression
func (l Left) FunctionName() string {
	return "left"
}

// Description implements sql.FunctionExpression
func (l Left) Description() string {
	return "returns the first N characters in the string given."
}

// Children implements the Expression interface.
func (l Left) Children() []sql.Expression {
	return []sql.Expression{l.str, l.len}
}

// Eval implements the Expression interface.
func (l Left) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	str, err := l.str.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	var text []rune
	switch str := str.(type) {
	case string:
		text = []rune(str)
	case []byte:
		text = []rune(string(str))
	case nil:
		return nil, nil
	default:
		// HeimDB: 数や日時は文字列にしてから扱う（MySQL と同じ。LEFT(123, 2) は '12'。もとはエラーにしていた）。
		// ENUM と SET は値の文字列（上流の #3068）
		if es, ok := types.ValueString(l.str.Type(), str); ok {
			str = es
		}
		v, _, err := types.LongText.Convert(ctx, str)
		if err != nil {
			return nil, sql.ErrInvalidType.New(reflect.TypeOf(str).String())
		}
		sv, _, err := sql.Unwrap[string](ctx, v)
		if err != nil {
			return nil, err
		}
		text = []rune(sv)
	}

	var length int64
	runeCount := int64(len(text))
	len, err := l.len.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if len == nil {
		return nil, nil
	}

	len, _, err = types.Int64.Convert(ctx, len)
	if err != nil {
		return nil, err
	}

	length = len.(int64)

	if length > runeCount {
		length = runeCount
	}
	if length <= 0 {
		return "", nil
	}

	return string(text[:length]), nil
}

// IsNullable implements the Expression interface.
func (l Left) IsNullable() bool {
	return l.str.IsNullable() || l.len.IsNullable()
}

func (l Left) String() string {
	return fmt.Sprintf("LEFT(%s, %s)", l.str, l.len)
}

// Resolved implements the Expression interface.
func (l Left) Resolved() bool {
	return l.str.Resolved() && l.len.Resolved()
}

// Type implements the Expression interface.
func (l Left) Type() sql.Type { return argStringType(l.str) }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (l Left) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, l.str)
}

// WithChildren implements the Expression interface.
func (l Left) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(l, len(children), 2)
	}
	return NewLeft(children[0], children[1]), nil
}

// Right is a function that returns the last N characters of a string expression.
type Right struct {
	str sql.Expression
	len sql.Expression
}

var _ sql.FunctionExpression = Right{}
var _ sql.CollationCoercible = Right{}

// NewRight creates a new RIGHT function.
func NewRight(str, len sql.Expression) sql.Expression {
	return Right{str, len}
}

// FunctionName implements sql.FunctionExpression
func (r Right) FunctionName() string {
	return "right"
}

// Description implements sql.FunctionExpression
func (r Right) Description() string {
	return "returns the specified rightmost number of characters."
}

// Children implements the Expression interface.
func (r Right) Children() []sql.Expression {
	return []sql.Expression{r.str, r.len}
}

// Eval implements the Expression interface.
func (r Right) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	str, err := r.str.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	var text []rune
	switch str := str.(type) {
	case string:
		text = []rune(str)
	case sql.StringWrapper:
		s, err := str.Unwrap(ctx)
		if err != nil {
			return nil, err
		}
		text = []rune(s)
	case []byte:
		text = []rune(string(str))
	case sql.BytesWrapper:
		b, err := str.Unwrap(ctx)
		if err != nil {
			return nil, err
		}
		text = []rune(string(b))
	case nil:
		return nil, nil
	default:
		// HeimDB: 数や日時は文字列にしてから扱う（MySQL と同じ。LEFT(123, 2) は '12'。もとはエラーにしていた）。
		// ENUM と SET は値の文字列（上流の #3068）
		if es, ok := types.ValueString(r.str.Type(), str); ok {
			str = es
		}
		v, _, err := types.LongText.Convert(ctx, str)
		if err != nil {
			return nil, sql.ErrInvalidType.New(reflect.TypeOf(str).String())
		}
		sv, _, err := sql.Unwrap[string](ctx, v)
		if err != nil {
			return nil, err
		}
		text = []rune(sv)
	}

	var length int64
	runeCount := int64(len(text))
	len, err := r.len.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if len == nil {
		return nil, nil
	}

	len, _, err = types.Int64.Convert(ctx, len)
	if err != nil {
		return nil, err
	}

	length = len.(int64)

	if length > runeCount {
		length = runeCount
	}
	if length <= 0 {
		return "", nil
	}

	return string(text[runeCount-length:]), nil
}

// IsNullable implements the Expression interface.
func (r Right) IsNullable() bool {
	return r.str.IsNullable() || r.len.IsNullable()
}

func (r Right) String() string {
	return fmt.Sprintf("RIGHT(%s, %s)", r.str, r.len)
}

func (r Right) DebugString() string {
	pr := sql.NewTreePrinter()
	_ = pr.WriteNode("RIGHT")
	children := []string{
		fmt.Sprintf("str: %s", sql.DebugString(r.str)),
		fmt.Sprintf("len: %s", sql.DebugString(r.len)),
	}
	_ = pr.WriteChildren(children...)
	return pr.String()
}

// Resolved implements the Expression interface.
func (r Right) Resolved() bool {
	return r.str.Resolved() && r.len.Resolved()
}

// Type implements the Expression interface.
func (r Right) Type() sql.Type { return argStringType(r.str) }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (r Right) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.GetCoercibility(ctx, r.str)
}

// WithChildren implements the Expression interface.
func (r Right) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(r, len(children), 2)
	}
	return NewRight(children[0], children[1]), nil
}

type Instr struct {
	str    sql.Expression
	substr sql.Expression
}

var _ sql.FunctionExpression = Instr{}
var _ sql.CollationCoercible = Instr{}

// NewInstr creates a new instr UDF.
func NewInstr(str, substr sql.Expression) sql.Expression {
	return Instr{str, substr}
}

// FunctionName implements sql.FunctionExpression
func (i Instr) FunctionName() string {
	return "instr"
}

// Description implements sql.FunctionExpression
func (i Instr) Description() string {
	return "returns the 1-based index of the first occurence of str2 in str1, or 0 if it does not occur."
}

// Children implements the Expression interface.
func (i Instr) Children() []sql.Expression {
	return []sql.Expression{i.str, i.substr}
}

// Eval implements the Expression interface.
func (i Instr) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	str, err := i.str.Eval(ctx, row)
	if err != nil || str == nil {
		return nil, err
	}

	substr, err := i.substr.Eval(ctx, row)
	if err != nil || substr == nil {
		return nil, err
	}

	// 上流の #3980: 文字列でない引数（数、ENUM）は文字列にし、探す文字列の照合順序で比べる（前は文字のまま比べ、
	// utf8mb4_unicode_520_ci の列で INSTR(s, 'A') が 0 だった）
	text, _, err := types.ConvertToCollatedString(ctx, str, i.str.Type())
	if err != nil {
		return nil, err
	}
	subtext, _, err := types.ConvertToCollatedString(ctx, substr, i.substr.Type())
	if err != nil {
		return nil, err
	}

	// MySQL は、substr に COLLATE があっても探す文字列（1 つ目の引数）の照合順序で比べる。バイナリなら、位置は
	// 文字ではなくバイトの位置
	collation, _ := sql.GetCoercibility(ctx, i.str)
	if collation == sql.Collation_binary {
		return int64(strings.Index(text, subtext) + 1), nil
	}

	return instrCollated(text, subtext, collation)
}

// isASCII は、s が ASCII の字だけか。
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// instrASCII は instrCollated の速い経路で、subtext が ASCII の字だけで、文字セットが UTF-8 のときに使う。ASCII の n
// 文字と同じバイト数の部分は ASCII の n 文字だけなので、text の文字の境目から ASCII の字が続く所だけを、字ごとの重み
// で比べる（行ごとに文字の配列と符号化を作らない。段階 114）。
func instrASCII(text, subtext string, sorter sql.CollationSorter) int64 {
	n := len(subtext)
	first := sorter(rune(subtext[0]))
	pos := int64(0)
	for i := 0; i < len(text); {
		pos++
		c := text[i]
		if c < 0x80 && i+n <= len(text) && sorter(rune(c)) == first {
			matched := true
			for j := 1; j < n; j++ {
				b := text[i+j]
				if b >= 0x80 || sorter(rune(b)) != sorter(rune(subtext[j])) {
					matched = false
					break
				}
			}
			if matched {
				return pos
			}
		}
		if c < 0x80 {
			i++
		} else {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
		}
	}
	return 0
}

// instrCollated は、text の中で subtext が最初に現れる文字の位置（1 から。なければ 0）を、照合順序 collation で比べて
// 返す。空の subtext は 1（上流の #3980）。
func instrCollated(text, subtext string, collation sql.CollationID) (int64, error) {
	if len(subtext) == 0 {
		return 1, nil
	}

	sorter := collation.Sorter()
	if sorter == nil {
		return 0, sql.ErrCollationNotYetImplementedTemp.New(collation.Name())
	}
	if cs := collation.CharacterSet(); (cs == sql.CharacterSet_utf8mb4 || cs == sql.CharacterSet_utf8mb3) && isASCII(subtext) {
		return instrASCII(text, subtext, sorter), nil
	}

	// MySQL は、文字の境目から始まり、照合順序の文字セットで subtext と同じバイト数の部分だけを一致とみなす（バイト数の
	// 違う文字を等しいとする照合順序で効く。utf8mb4_unicode_520_ci で INSTR('café', 'e') は 0）
	encoder := collation.CharacterSet().Encoder()
	encodedSubtext, ok := encoder.Encode([]byte(subtext))
	if !ok {
		return 0, sql.ErrCollationMalformedString.New("converting INSTR substring")
	}

	// 文字は 1 つずつ照合順序の重みで比べるので、1 文字と複数の文字は一致しない（utf8mb4_0900_ai_ci の 'ß' と 'ss'。
	// MySQL は INSTR('straße', 'ss') を 5 にするが、これは 0）
	subtextRunes := []rune(subtext)
	textRunes := []rune(text)
	if len(subtextRunes) > len(textRunes) {
		return 0, nil
	}

	// byteOffsets[j] は、照合順序の文字セットでの textRunes[j] のバイトの位置
	byteOffsets := make([]int, len(textRunes)+1)
	var runeBytes []byte
	for j, r := range textRunes {
		runeBytes = utf8.AppendRune(runeBytes[:0], r)
		encoded, ok := encoder.Encode(runeBytes)
		if !ok {
			return 0, sql.ErrCollationMalformedString.New("converting INSTR string")
		}
		byteOffsets[j+1] = byteOffsets[j] + len(encoded)
	}

	for start := 0; start+len(subtextRunes) <= len(textRunes); start++ {
		if byteOffsets[start+len(subtextRunes)]-byteOffsets[start] != len(encodedSubtext) {
			continue
		}
		matched := true
		for j, r := range subtextRunes {
			if sorter(textRunes[start+j]) != sorter(r) {
				matched = false
				break
			}
		}
		if matched {
			return int64(start + 1), nil
		}
	}
	return 0, nil
}

// IsNullable implements the Expression interface.
func (i Instr) IsNullable() bool {
	return i.str.IsNullable() || i.substr.IsNullable()
}

func (i Instr) String() string {
	return fmt.Sprintf("INSTR(%s, %s)", i.str, i.substr)
}

// Resolved implements the Expression interface.
func (i Instr) Resolved() bool {
	return i.str.Resolved() && i.substr.Resolved()
}

// Type implements the Expression interface.
func (Instr) Type() sql.Type { return types.Int64 }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (Instr) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	return sql.Collation_binary, 5
}

// WithChildren implements the Expression interface.
func (i Instr) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(i, len(children), 2)
	}
	return NewInstr(children[0], children[1]), nil
}
