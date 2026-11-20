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

// Modified by Tsukinowa Inc.: 数の列の範囲で、文字列の定数は先頭の数の部分を読む（docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM の列の = と <> で、定義にない文字列はエラーにしない（docs/engine.md）
// Modified by Tsukinowa Inc.: 日時の列の範囲の端に、後ろを切り詰めて読んだ値を使う（上流の #3276 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: DECIMAL の列で、列の小数の桁に丸めた範囲の端を含む側にする（上流の #4054 の取り込みで。docs/engine.md）
// Modified by Tsukinowa Inc.: lookup のキーが DECIMAL の列で丸められるかを返す DecimalKeyChanged を足した（段階 115 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: decimalKeyRounded の整数のキーを、文字列を経ずに decimal にする（段階 115 の 2 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: 整数と浮動小数点数の列の空の文字列のキーを 0 にし、2 つ以上の値の IN の日時として読めない文字列をゼロの日時にする（段階 116。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM の列の '' を空の値（番号 0）の点にし、範囲の端の定義にない値を NULL でない行の範囲にする（段階 116 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM の列のキーを比べるときの変換（ConvertToCompare）で変え、後ろの空白を PAD SPACE の照合順序でだけ除く（段階 116 の 2 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の列の範囲の端を、丸めない元の型で変換する（段階 120。docs/engine.md）
// Modified by Tsukinowa Inc.: SET と BIT の列の範囲で、定義の外の値、整数でない BIT の値と、SET の = と IN でない比べは NULL でない行を読んで比べる側で決める（段階 120。docs/engine.md）

package sql

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/shopspring/decimal"
	"gopkg.in/src-d/go-errors.v1"
)

var (
	ErrInvalidColExpr      = errors.NewKind("the expression `%s` could not be found from the index `%s`")
	ErrRangeSimplification = errors.NewKind("attempting to simplify ranges has removed all ranges")
	ErrInvalidRangeType    = errors.NewKind("encountered the RangeType_Invalid")
)

// MySQLIndexBuilder builds ranges based on the combination of calls made for the given index, and then relies on the Index
// to return an IndexLookup from the created ranges.
type MySQLIndexBuilder struct {
	idx          Index
	isInvalid    bool
	err          error
	colExprTypes map[string]Type
	ranges       map[string][]MySQLRangeColumnExpr
}

// NewMySQLIndexBuilder returns a new MySQLIndexBuilder. Used internally to construct a range that will later be passed to
// integrators through the Index function NewLookup.
func NewMySQLIndexBuilder(idx Index) *MySQLIndexBuilder {
	colExprTypes := make(map[string]Type)
	ranges := make(map[string][]MySQLRangeColumnExpr)
	for _, cet := range idx.ColumnExpressionTypes() {
		typ := cet.Type
		if _, ok := typ.(StringType); ok {
			typ = typ.Promote()
		}
		colExprTypes[strings.ToLower(cet.Expression)] = typ
		ranges[strings.ToLower(cet.Expression)] = []MySQLRangeColumnExpr{AllRangeColumnExpr(typ)}
	}
	return &MySQLIndexBuilder{
		idx:          idx,
		isInvalid:    false,
		err:          nil,
		colExprTypes: colExprTypes,
		ranges:       ranges,
	}
}

// numberKey は、数の列 typ の範囲の端にする文字列の定数 key が数として読めなければ、MySQL と同じく先頭の数の部分
// （'12abc' なら 12、'12.5x' なら 12.5）の decimal にする。HeimDB: 前は列の型への変換が誤りになり、文がエラー 1366 に
// なった。数として読める文字列も decimal にする（大きな整数の桁を落とさず、'12.5' を丸めた 13 の点にしない）。空の
// 文字列（DECIMAL の列のみ。ほかの数の列は 0）と、文字列でない値はそのまま。
func numberKey(typ Type, key interface{}) interface{} {
	s, ok := key.(string)
	if _, isNumber := typ.(NumberType); !ok || !isNumber {
		return key
	}
	t := strings.Trim(s, NumericCutSet)
	_, isDecimal := typ.(DecimalType)
	if t == "" {
		if !isDecimal {
			// HeimDB: 整数と浮動小数点数の列では、空の文字列も MySQL と同じく 0（BIGINT UNSIGNED の列への post_id = ''
			// が、列の型への変換の誤りで 1366 になった。段階 116）
			return decimal.Zero
		}
		return key
	}
	if d, err := decimal.NewFromString(t); err == nil {
		return d
	}
	prefix, _ := TruncateStringToDouble(t, false)
	d, err := decimal.NewFromString(prefix)
	if err != nil {
		if !isDecimal {
			return decimal.Zero
		}
		return key
	}
	return d
}

func ceil(val interface{}) interface{} {
	switch v := val.(type) {
	case float32:
		return float32(math.Ceil(float64(v)))
	case float64:
		return math.Ceil(v)
	case decimal.Decimal:
		return v.Ceil()
	case string:
		dec, err := decimal.NewFromString(v)
		if err != nil {
			return v
		}
		return ceil(dec)
	case []byte:
		return ceil(string(v))
	default:
		return v
	}
}

func floor(val interface{}) interface{} {
	switch v := val.(type) {
	case float32:
		return float32(math.Floor(float64(v)))
	case float64:
		return math.Floor(v)
	case decimal.Decimal:
		return v.Floor()
	case string:
		dec, err := decimal.NewFromString(v)
		if err != nil {
			return v
		}
		return floor(dec)
	case []byte:
		return floor(string(v))
	default:
		return v
	}
}

// Equals represents colExpr = key. For IN expressions, pass all of them in the same Equals call.
func (b *MySQLIndexBuilder) Equals(ctx *Context, colExpr string, keys ...interface{}) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	for i := range keys {
		keys[i] = numberKey(typ, keys[i])
	}
	potentialRanges := make([]MySQLRangeColumnExpr, len(keys))
	for i, k := range keys {
		// if converting from float to int results in rounding, then it's empty range
		if t, ok := typ.(NumberType); ok && !t.IsFloat() {
			f, c := floor(k), ceil(k)
			switch k.(type) {
			case float32, float64:
				if f != c {
					potentialRanges[i] = EmptyRangeColumnExpr(typ)
					continue
				}
			case decimal.Decimal:
				if !f.(decimal.Decimal).Equals(c.(decimal.Decimal)) {
					potentialRanges[i] = EmptyRangeColumnExpr(typ)
					continue
				}
			}
		}

		var err error
		orig := k
		k, _, err = convertKey(ctx, typ, k)
		err = truncatedKey(err)
		if _, isDate := typ.(DatetimeType); len(keys) > 1 && err != nil && isDate {
			// HeimDB: in an IN list of two or more values, MySQL compares a date
			// column with a string that is not a date as the zero date, with
			// warning 1292 (one value or = is error 1525) (stage 116)
			ctx.Warn(1292, "Incorrect datetime value: '%v'", orig)
			k, _, err = typ.Convert(ctx, "0000-00-00")
		}
		if decimalKeyInvalid(typ, err) || timeNumberKeyInvalid(typ, orig, err) {
			// DECIMAL の列に入らない値（範囲の外、数として読めない文字列）は、NULL でない行を読んで比べる側で決める
			potentialRanges[i] = NotNullRangeColumnExpr(typ)
			continue
		}
		if setBitWholeRange(typ, orig, false, err) {
			// HeimDB: SET と BIT の定義の外と、整数でない BIT の値は、NULL でない行を読んで比べる側で決める（段階 120）
			potentialRanges[i] = NotNullRangeColumnExpr(typ)
			continue
		}
		if _, ok := typ.(EnumType); ok && err != nil {
			// ほかの定義にない値には、合う行はない（MySQL は索引を使うと、定義にない値を番号 0 にして空の値の行に合わせる。
			// 索引を使わないと合わない。HeimDB は索引を使わないときと同じ）
			potentialRanges[i] = EmptyRangeColumnExpr(typ)
			continue
		}
		if err != nil {
			b.isInvalid = true
			b.err = err
			return b
		}
		potentialRanges[i] = ClosedRangeColumnExpr(k, k, typ)
	}
	b.updateCol(ctx, colExpr, potentialRanges...)
	return b
}

// NotEquals represents colExpr <> key.
func (b *MySQLIndexBuilder) NotEquals(ctx *Context, colExpr string, key interface{}) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	key = numberKey(typ, key)
	// if converting from float to int results in rounding, then it's entire range (excluding nulls)
	f, c := floor(key), ceil(key)
	switch key.(type) {
	case float32, float64:
		if f != c {
			b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
			return b
		}
	case decimal.Decimal:
		if !f.(decimal.Decimal).Equals(c.(decimal.Decimal)) {
			b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
			return b
		}
	}

	orig := key
	key, _, err := convertKey(ctx, typ, key)
	err = truncatedKey(err)
	if setBitWholeRange(typ, orig, true, err) {
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ)) // HeimDB: 段階 120
		return b
	}
	if _, ok := typ.(EnumType); ok && err != nil || decimalKeyInvalid(typ, err) || timeNumberKeyInvalid(typ, orig, err) {
		// 定義にない値とは、NULL でないどの行も違う（DECIMAL の列に入らない値は、NULL でない行を読んで比べる側で決める）
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
		return b
	}
	if err != nil {
		b.isInvalid = true
		b.err = err
		return b
	}
	if decimalKeyRounded(typ, orig, key) {
		// d <> 123.451 on a DECIMAL(10,2) does not exclude 123.45 (HeimDB)
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
		return b
	}

	b.updateCol(ctx, colExpr, GreaterThanRangeColumnExpr(key, typ), LessThanRangeColumnExpr(key, typ))
	if !b.isInvalid {
		ranges, err := SimplifyRangeColumn(b.ranges[colExpr]...)
		if err != nil {
			b.isInvalid = true
			b.err = err
			return b
		}
		if len(ranges) == 0 {
			b.isInvalid = true
			return b
		}
		b.ranges[colExpr] = ranges
	}
	return b
}

// GreaterThan represents colExpr > key.
func (b *MySQLIndexBuilder) GreaterThan(ctx *Context, colExpr string, key interface{}) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	key = numberKey(typ, key)

	if t, ok := typ.(NumberType); ok && !t.IsFloat() {
		key = floor(key)
	}

	orig := key
	key, _, err := convertKey(ctx, typ, key)
	err = truncatedKey(err)
	if setBitWholeRange(typ, orig, true, err) {
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ)) // HeimDB: 段階 120
		return b
	}
	if enumKeyInvalid(typ, err) || decimalKeyInvalid(typ, err) || timeNumberKeyInvalid(typ, orig, err) {
		// HeimDB: an ENUM column's range with a string that is not a value (e > '')
		// reads the rows that are not NULL and the filter compares them (it failed
		// with 1265; stage 116 review)
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
		return b
	}
	if err != nil {
		b.isInvalid = true
		b.err = err
		return b
	}

	if decimalKeyRounded(typ, orig, key) {
		// d > 123.445 on a DECIMAL(10,2) includes 123.45 (HeimDB)
		b.updateCol(ctx, colExpr, GreaterOrEqualRangeColumnExpr(key, typ))
		return b
	}
	b.updateCol(ctx, colExpr, GreaterThanRangeColumnExpr(key, typ))
	return b
}

// GreaterOrEqual represents colExpr >= key.
func (b *MySQLIndexBuilder) GreaterOrEqual(ctx *Context, colExpr string, key interface{}) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	key = numberKey(typ, key)

	var exclude bool
	if t, ok := typ.(NumberType); ok && !t.IsFloat() {
		newKey := floor(key)
		switch key.(type) {
		case float32, float64:
			exclude = key != newKey
		case decimal.Decimal:
			exclude = !key.(decimal.Decimal).Equals(newKey.(decimal.Decimal))
		}
		key = newKey
	}

	orig := key
	key, _, err := convertKey(ctx, typ, key)
	err = truncatedKey(err)
	if setBitWholeRange(typ, orig, true, err) {
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ)) // HeimDB: 段階 120
		return b
	}
	if enumKeyInvalid(typ, err) || decimalKeyInvalid(typ, err) || timeNumberKeyInvalid(typ, orig, err) {
		// HeimDB: an ENUM column's range with a string that is not a value (e > '')
		// reads the rows that are not NULL and the filter compares them (it failed
		// with 1265; stage 116 review)
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
		return b
	}
	if err != nil {
		b.isInvalid = true
		b.err = err
		return b
	}

	var rangeColExpr MySQLRangeColumnExpr
	if exclude {
		rangeColExpr = GreaterThanRangeColumnExpr(key, typ)
	} else {
		rangeColExpr = GreaterOrEqualRangeColumnExpr(key, typ)
	}
	b.updateCol(ctx, colExpr, rangeColExpr)

	return b
}

// LessThan represents colExpr < key.
func (b *MySQLIndexBuilder) LessThan(ctx *Context, colExpr string, key interface{}) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	key = numberKey(typ, key)

	if t, ok := typ.(NumberType); ok && !t.IsFloat() {
		key = ceil(key)
	}
	orig := key
	key, _, err := convertKey(ctx, typ, key)
	err = truncatedKey(err)
	if setBitWholeRange(typ, orig, true, err) {
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ)) // HeimDB: 段階 120
		return b
	}
	if enumKeyInvalid(typ, err) || decimalKeyInvalid(typ, err) || timeNumberKeyInvalid(typ, orig, err) {
		// HeimDB: an ENUM column's range with a string that is not a value (e > '')
		// reads the rows that are not NULL and the filter compares them (it failed
		// with 1265; stage 116 review)
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
		return b
	}
	if err != nil {
		b.isInvalid = true
		b.err = err
		return b
	}

	if decimalKeyRounded(typ, orig, key) {
		// d < 123.45000001 on a DECIMAL(10,2) includes 123.45 (HeimDB)
		b.updateCol(ctx, colExpr, LessOrEqualRangeColumnExpr(key, typ))
		return b
	}
	b.updateCol(ctx, colExpr, LessThanRangeColumnExpr(key, typ))
	return b
}

// LessOrEqual represents colExpr <= key.
func (b *MySQLIndexBuilder) LessOrEqual(ctx *Context, colExpr string, key interface{}) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	key = numberKey(typ, key)

	var exclude bool
	if t, ok := typ.(NumberType); ok && !t.IsFloat() {
		newKey := ceil(key)
		switch key.(type) {
		case float32, float64:
			exclude = key != newKey
		case decimal.Decimal:
			exclude = !key.(decimal.Decimal).Equals(newKey.(decimal.Decimal))
		}
		key = newKey
	}

	orig := key
	key, _, err := convertKey(ctx, typ, key)
	err = truncatedKey(err)
	if setBitWholeRange(typ, orig, true, err) {
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ)) // HeimDB: 段階 120
		return b
	}
	if enumKeyInvalid(typ, err) || decimalKeyInvalid(typ, err) || timeNumberKeyInvalid(typ, orig, err) {
		// HeimDB: an ENUM column's range with a string that is not a value (e > '')
		// reads the rows that are not NULL and the filter compares them (it failed
		// with 1265; stage 116 review)
		b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))
		return b
	}
	if err != nil {
		b.isInvalid = true
		b.err = err
		return b
	}

	var rangeColExpr MySQLRangeColumnExpr
	if exclude {
		rangeColExpr = LessThanRangeColumnExpr(key, typ)
	} else {
		rangeColExpr = LessOrEqualRangeColumnExpr(key, typ)
	}
	b.updateCol(ctx, colExpr, rangeColExpr)

	return b
}

// IsNull represents colExpr = nil
func (b *MySQLIndexBuilder) IsNull(ctx *Context, colExpr string) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	b.updateCol(ctx, colExpr, NullRangeColumnExpr(typ))

	return b
}

// IsNotNull represents colExpr != nil
func (b *MySQLIndexBuilder) IsNotNull(ctx *Context, colExpr string) *MySQLIndexBuilder {
	if b.isInvalid {
		return b
	}
	typ, ok := b.colExprTypes[colExpr]
	if !ok {
		b.isInvalid = true
		b.err = ErrInvalidColExpr.New(colExpr, b.idx.ID())
		return b
	}
	b.updateCol(ctx, colExpr, NotNullRangeColumnExpr(typ))

	return b
}

// Ranges returns all ranges for this index builder. If the builder is in an error state then this returns nil.
func (b *MySQLIndexBuilder) Ranges(ctx *Context) MySQLRangeCollection {
	if b.err != nil {
		return nil
	}
	// An invalid builder that did not error got into a state where no columns will ever match, so we return an empty range
	if b.isInvalid {
		cets := b.idx.ColumnExpressionTypes()
		emptyRange := make(MySQLRange, len(cets))
		for i, cet := range cets {
			typ := cet.Type
			if _, ok := typ.(StringType); ok {
				typ = typ.Promote()
			}
			emptyRange[i] = EmptyRangeColumnExpr(typ)
		}
		return MySQLRangeCollection{emptyRange}
	}
	var allColumns [][]MySQLRangeColumnExpr
	for _, colExpr := range b.idx.Expressions() {
		ranges, ok := b.ranges[strings.ToLower(colExpr)]
		if !ok {
			// An index builder is guaranteed to cover the first n expressions, so if we hit an expression that we do
			// not have an entry for then we've hit all the ranges.
			break
		}
		allColumns = append(allColumns, ranges)
	}

	// In the builder ranges map we store multiple column expressions per column, however we want all permutations to
	// be their own range, so here we're creating a new range for every permutation.
	colCounts := make([]int, len(allColumns))
	permutation := make([]int, len(allColumns))
	for i, rangeColumn := range allColumns {
		colCounts[i] = len(rangeColumn)
	}
	var ranges MySQLRangeCollection
	exit := false
	for !exit {
		exit = true
		currentRange := make(MySQLRange, len(allColumns))
		for colIdx, exprCount := range colCounts {
			permutation[colIdx] = (permutation[colIdx] + 1) % exprCount
			if permutation[colIdx] != 0 {
				exit = false
				break
			}
		}
		for colIdx, exprIdx := range permutation {
			currentRange[colIdx] = allColumns[colIdx][exprIdx]
		}
		isempty, err := currentRange.IsEmpty()
		if err != nil {
			b.err = err
			return nil
		}
		if !isempty {
			ranges = append(ranges, currentRange)
		}
	}
	if len(ranges) == 0 {
		cets := b.idx.ColumnExpressionTypes()
		emptyRange := make(MySQLRange, len(cets))
		for i, cet := range cets {
			emptyRange[i] = EmptyRangeColumnExpr(cet.Type.Promote())
		}
		return MySQLRangeCollection{emptyRange}
	}
	return ranges
}

// Build constructs a new IndexLookup based on the ranges that have been built internally by this builder.
func (b *MySQLIndexBuilder) Build(ctx *Context) (IndexLookup, error) {
	if b.err != nil {
		return emptyLookup, b.err
	} else {
		ranges := b.Ranges(ctx)
		if len(ranges) == 0 {
			return emptyLookup, nil
		}
		return IndexLookup{Index: b.idx, Ranges: ranges}, nil
	}
}

// updateCol updates the internal columns with the given ranges by intersecting each given range with each existing
// range. That means that each given range is treated as an OR with respect to the other given ranges. If multiple
// ranges are to be intersected with respect to one another, multiple calls to updateCol should be made.
func (b *MySQLIndexBuilder) updateCol(ctx *Context, colExpr string, potentialRanges ...MySQLRangeColumnExpr) {
	if len(potentialRanges) == 0 {
		return
	}

	currentRanges, ok := b.ranges[colExpr]
	if !ok {
		b.ranges[colExpr] = potentialRanges
		return
	}

	var newRanges []MySQLRangeColumnExpr
	for _, currentRange := range currentRanges {
		for _, potentialRange := range potentialRanges {

			newRange, ok, err := currentRange.TryIntersect(potentialRange)
			if err != nil {
				b.isInvalid = true
				if !ErrInvalidValue.Is(err) {
					b.err = err
				}
				return
			}
			if ok {
				isempty, err := newRange.IsEmpty()
				if err != nil {
					b.isInvalid = true
					b.err = err
					return
				}
				if !isempty {
					newRanges = append(newRanges, newRange)
				}
			}
		}
	}

	// If we end up with zero ranges then we had an impossible combination, such as (x < 1 AND x > 1)
	if len(newRanges) == 0 {
		b.isInvalid = true
		return
	}
	b.ranges[colExpr] = newRanges
}

// SpatialIndexBuilder is like the MySQLIndexBuilder, but spatial
type SpatialIndexBuilder struct {
	idx Index
	typ Type
	rng MySQLRangeColumnExpr
}

func NewSpatialIndexBuilder(idx Index) *SpatialIndexBuilder {
	return &SpatialIndexBuilder{idx: idx, typ: idx.ColumnExpressionTypes()[0].Type}
}

func (b *SpatialIndexBuilder) AddRange(lower, upper interface{}) *SpatialIndexBuilder {
	b.rng = MySQLRangeColumnExpr{
		LowerBound: Below{Key: lower},
		UpperBound: Above{Key: upper},
		Typ:        b.typ,
	}
	return b
}

func (b *SpatialIndexBuilder) Build() (IndexLookup, error) {
	return IndexLookup{
		Index:           b.idx,
		Ranges:          MySQLRangeCollection{{b.rng}},
		IsSpatialLookup: true,
	}, nil
}

// EqualityIndexBuilder is a range builder builds equality expressions
// more quickly than the default builder
type EqualityIndexBuilder struct {
	idx   Index
	rng   MySQLRange
	empty bool
}

func NewEqualityIndexBuilder(idx Index) *EqualityIndexBuilder {
	return &EqualityIndexBuilder{idx: idx, rng: make(MySQLRange, len(idx.Expressions()))}
}

// AddEquality represents colExpr = key. For IN expressions, pass all of them in the same AddEquality call.
func (b *EqualityIndexBuilder) AddEquality(ctx *Context, colIdx int, k interface{}) error {
	if b.empty {
		return nil
	}
	if colIdx >= len(b.rng) {
		return fmt.Errorf("invalid index for building index lookup")
	}
	if b.rng[colIdx].UpperBound != nil {
		return fmt.Errorf("redundant restriction on index column")
	}

	typ := b.idx.ColumnExpressionTypes()[colIdx].Type
	// if converting from float to int results in rounding, then it's empty range
	if t, ok := typ.(NumberType); ok && !t.IsFloat() {
		f, c := floor(k), ceil(k)
		switch k.(type) {
		case float32, float64:
			if f != c {
				b.empty = true
				return nil
			}
		case decimal.Decimal:
			if !f.(decimal.Decimal).Equals(c.(decimal.Decimal)) {
				b.empty = true
				return nil
			}
		}
	}

	var err error
	k, _, err = convertKey(ctx, typ, k)
	err = truncatedKey(err)
	if enumKeyInvalid(typ, err) {
		// HeimDB: no ENUM value equals a string that is not a value (stage 116 second review)
		b.empty = true
		return nil
	}
	if _, isSet := typ.(SetType); (isSet || typ.Type() == query.Type_BIT) && err != nil {
		// HeimDB: no SET or BIT value equals a value outside the column's definition or range (stage 120)
		b.empty = true
		return nil
	}
	if err != nil {
		return err
	}
	b.rng[colIdx] = ClosedRangeColumnExpr(k, k, typ)

	return nil
}

func (b *EqualityIndexBuilder) Build(_ *Context) (IndexLookup, error) {
	if b.empty {
		for i, cet := range b.idx.ColumnExpressionTypes() {
			b.rng[i] = EmptyRangeColumnExpr(cet.Type)
		}
	}
	return IndexLookup{
		Index:        b.idx,
		Ranges:       MySQLRangeCollection{b.rng},
		IsEmptyRange: b.empty,
	}, nil
}

// truncatedKey は、日時の文字列の後ろの余計な文字を切り詰めて読んだ誤り（ErrTruncatedIncorrect）なら nil を返す。範囲の
// 端は、比べる側（Filter）と同じく切り詰めた値にする（段階 114、上流の #3276。前は 1292 で文がエラーになった）。
// decimalKeyRounded reports whether converting the range key orig to the
// DECIMAL column type typ changed its value (the column's scale rounds
// 123.45000001 to 123.45). An exclusive bound on the rounded value would drop
// rows the comparison keeps, so the callers make it inclusive; the filter above
// the index still compares with the original value (HeimDB, stage 115, with
// upstream #4054). A key that cannot be read as a decimal counts as rounded.
// decimalKeyInvalid reports whether a range key could not be converted to the
// DECIMAL column type typ (out of the column's range, like 99999999999 for a
// DECIMAL(10,2), or a string that is not a number, like '123.45x', which MySQL
// compares as a double with a warning). Such a bound reads every non-NULL row
// and leaves the decision to the filter (HeimDB, stage 115; the bounds failed
// the statement with 1105 or 1366).
// timeNumberKeyInvalid reports whether a number key of a date column does not
// read as a date (1, 20241301): MySQL then compares the column as a number, so
// the range is every row that is not NULL and the filter decides (HeimDB, stage 116).
func timeNumberKeyInvalid(typ Type, key interface{}, err error) bool {
	if _, isDate := typ.(DatetimeType); !isDate || err == nil {
		return false
	}
	switch key.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, decimal.Decimal:
		return true
	}
	return false
}

// enumKeyInvalid は、ENUM の列の範囲の端が定義にない値で変えられなかったか（HeimDB、段階 116 のレビュー）。
func enumKeyInvalid(typ Type, err error) bool {
	_, ok := typ.(EnumType)
	return ok && err != nil
}

// enumKeyConverter is types.EnumType's conversion for a comparison (HeimDB).
type enumKeyConverter interface {
	ConvertToCompare(ctx context.Context, v interface{}) (interface{}, error)
}

// setBitWholeRange reports whether a range of the SET or BIT column type typ with
// the key key must read every row that is not NULL and let the filter compare
// (HeimDB, stage 120, when SET and BIT indexes got entries). A SET range other than =
// and IN (inequality is true), as MySQL uses its index only for those and compares
// the others as strings, and a BIT key that is not an integer (b = 5.4, b > '5',
// whose filter decides), or that the column cannot hold (err: b = 256 on a BIT(8),
// b = -1, s = 'bad'; it failed the statement with 1105 or 1265 with an index only).
func setBitWholeRange(typ Type, key interface{}, inequality bool, err error) bool {
	if _, isSet := typ.(SetType); isSet {
		return inequality || err != nil
	}
	if typ.Type() != query.Type_BIT {
		return false
	}
	if err != nil {
		return true
	}
	switch key.(type) {
	case int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint:
		return false
	}
	return true
}

// keyTyper is a type whose range keys convert with another type (types.NumberTypeImpl_'s KeyType: FLOAT(M,D)
// and DOUBLE(M,D) keys are not rounded to the column's decimals. HeimDB, stage 120).
type keyTyper interface {
	KeyType() Type
}

// convertKey converts a range key to the column type typ. An ENUM column's key is
// converted as for a comparison: trailing spaces are dropped only under a PAD SPACE
// collation (e = 'a ' matches no row under utf8mb4_0900_ai_ci, as in MySQL), a
// BINARY string matches a value's bytes, and an empty string is the empty value, index 0, which a
// non-strict INSERT stores (HeimDB, stage 116 second review).
func convertKey(ctx *Context, typ Type, key interface{}) (interface{}, ConvertInRange, error) {
	if kt, ok := typ.(keyTyper); ok {
		// HeimDB: FLOAT(M,D) と DOUBLE(M,D) の端は丸めない（段階 120。比べる側も丸めない）
		typ = kt.KeyType()
	}
	if ec, ok := typ.(enumKeyConverter); ok {
		if _, isEnum := typ.(EnumType); isEnum {
			k, err := ec.ConvertToCompare(ctx, key)
			return k, InRange, err
		}
	}
	return typ.Convert(ctx, key)
}

func decimalKeyInvalid(typ Type, err error) bool {
	_, ok := typ.(DecimalType)
	return ok && err != nil
}

// DecimalKeyChanged reports whether converting a lookup key orig to the
// DECIMAL type typ changed its value (conv is the converted value). A key that
// cannot be read as a decimal counts as unchanged, so the caller keeps its
// own handling (HeimDB, stage 115).
// A string or floating-point key is compared as a double, as MySQL compares a
// DECIMAL column with a string column or a DOUBLE column ('10.0000000000000000001'
// matches 10.0000).
func DecimalKeyChanged(typ Type, orig, conv interface{}) bool {
	if _, ok := typ.(DecimalType); !ok || orig == nil {
		return false
	}
	c, ok := conv.(decimal.Decimal)
	if !ok {
		return false
	}
	switch v := orig.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return false
		}
		return f != c.InexactFloat64()
	case float64:
		return v != c.InexactFloat64()
	case float32:
		return float64(v) != c.InexactFloat64()
	}
	return decimalKeyRounded(typ, orig, conv)
}

func decimalKeyRounded(typ Type, orig, conv interface{}) bool {
	if _, ok := typ.(DecimalType); !ok || orig == nil {
		return false
	}
	c, ok := conv.(decimal.Decimal)
	if !ok {
		return false
	}
	var o decimal.Decimal
	switch v := orig.(type) {
	case decimal.Decimal:
		o = v
	case decimal.NullDecimal:
		if !v.Valid {
			return false
		}
		o = v.Decimal
	case float64:
		o = decimal.NewFromFloat(v)
	case float32:
		o = decimal.NewFromFloat32(v)
	case int:
		// HeimDB: integers convert directly, without formatting and parsing
		// the value for every lookup key (stage 115's second review)
		o = decimal.NewFromInt(int64(v))
	case int8:
		o = decimal.NewFromInt(int64(v))
	case int16:
		o = decimal.NewFromInt(int64(v))
	case int32:
		o = decimal.NewFromInt(int64(v))
	case int64:
		o = decimal.NewFromInt(v)
	case uint:
		o = decimal.NewFromUint64(uint64(v))
	case uint8:
		o = decimal.NewFromInt(int64(v))
	case uint16:
		o = decimal.NewFromInt(int64(v))
	case uint32:
		o = decimal.NewFromInt(int64(v))
	case uint64:
		o = decimal.NewFromUint64(v)
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(v))
		if err != nil {
			return true
		}
		o = d
	default:
		return false
	}
	return !o.Equal(c)
}

func truncatedKey(err error) error {
	if err != nil && ErrTruncatedIncorrect.Is(err) {
		return nil
	}
	return err
}
