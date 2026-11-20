// Copyright 2024 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modified by Tsukinowa Inc.: DISTINCT と集合演算で、照合順序で等しい値を一つにする（HashOfTypes。docs/engine.md）
// Modified by Tsukinowa Inc.: TopN の並べ替えのキーを 1 行に 1 回だけ計算し、LIMIT に入らない行はヒープに入れない（docs/engine.md）
// Modified by Tsukinowa Inc.: SQL_CALC_FOUND_ROWS の LIMIT は、子が RowCounter なら残りの行を作らずに数えさせる（docs/engine.md）
// Modified by Tsukinowa Inc.: DISTINCT のキャッシュを 2 回片付けない（docs/engine.md）
// Modified by Tsukinowa Inc.: TopN の足した列の位置を実際の行の長さから決め、Sort も並べ替えのキーを 1 行に 1 回だけ計算する（上流の #3626、#3733 の問題。docs/engine.md）

package iters

import (
	"container/heap"
	"fmt"
	"io"
	"sort"

	"github.com/dolthub/jsonpath"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type topRowsIter struct {
	fields        sql.SortFields   // ORDER BY の並べ替え（keyIdx の位置は最初の行で足した列の参照に替える）
	keys          []sql.Expression // 先に計算して行の後ろ（順番の列の後）に足す並べ替えのキー
	keyIdx        []int            // fields の中で keys に替える位置
	sortFields    sql.SortFields   // 最初の行の長さから作る、比べるときの並べ替え
	calcFoundRows bool
	childIter     sql.RowIter
	limit         int64
	topRows       []sql.Row
	numFoundRows  int64
	idx           int
}

func NewTopRowsIter(s sql.SortFields, limit int64, calcFoundRows bool, child sql.RowIter, childSchemaLen int) *topRowsIter {
	// 列の参照でない並べ替えの式（ORDER BY CAST(meta_value AS CHAR) など）は、比べるたびに評価し直さず、行ごとに 1 回
	// 計算して行の後ろに足す（HeimDB の段階 96）。足した列（順番の列とキー）の位置は、最初の行の長さから決める。
	// 副問い合わせの中の行は外の行を前に持ち（外の行 || 子の行）、memory の表は childSchemaLen に 0 を渡すので、
	// 子のスキーマの長さでは位置がずれ、キーと同順位の順番が効かなかった（上流の #3626 の問題）。
	fields := make(sql.SortFields, len(s))
	copy(fields, s)
	var keys []sql.Expression
	var keyIdx []int
	for j, f := range fields {
		switch f.Column.(type) {
		case *expression.GetField, *expression.Literal:
		default:
			keys = append(keys, f.Column)
			keyIdx = append(keyIdx, j)
		}
	}
	return &topRowsIter{
		fields:        fields,
		keys:          keys,
		keyIdx:        keyIdx,
		limit:         limit,
		calcFoundRows: calcFoundRows,
		childIter:     child,
		idx:           -1,
	}
}

// sortFieldsFor は、base 列の行の後ろに順番の列とキーを足した行を比べる並べ替えを返す。
func (i *topRowsIter) sortFieldsFor(base int) sql.SortFields {
	fields := make(sql.SortFields, len(i.fields), len(i.fields)+1)
	copy(fields, i.fields)
	for k, j := range i.keyIdx {
		f := fields[j]
		gf := expression.NewGetField(base+1+k, f.Column.Type(), f.Column.String(), f.Column.IsNullable())
		f.Column, f.Column2 = gf, gf
		fields[j] = f
	}
	return append(fields, sql.SortField{Column: expression.NewGetField(base, types.Int64, "order", false)})
}

func (i *topRowsIter) Next(ctx *sql.Context) (sql.Row, error) {
	if i.idx == -1 {
		err := i.computeTopRows(ctx)
		if err != nil {
			return nil, err
		}
		i.idx = 0
	}

	if i.idx >= len(i.topRows) {
		return nil, io.EOF
	}
	row := i.topRows[i.idx]
	i.idx++
	return row[:len(row)-1-len(i.keys)], nil
}

func (i *topRowsIter) Close(ctx *sql.Context) error {
	i.topRows = nil

	if i.calcFoundRows {
		ctx.SetLastQueryInfoInt(sql.FoundRows, i.numFoundRows)
	}

	return i.childIter.Close(ctx)
}

func (i *topRowsIter) computeTopRows(ctx *sql.Context) error {
	topRowsHeap := &expression.TopRowsHeap{
		expression.Sorter{
			SortFields: nil,
			Rows:       []sql.Row{},
			LastError:  nil,
			Ctx:        ctx,
		},
	}
	base := -1
	for {
		row, err := i.childIter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		i.numFoundRows++

		if base < 0 {
			base = len(row)
			i.sortFields = i.sortFieldsFor(base)
			topRowsHeap.Sorter.SortFields = i.sortFields
		}
		// 足す列で子の行の後ろの配列を書き換えないよう、写してから足す
		r := make(sql.Row, len(row), len(row)+1+len(i.keys))
		copy(r, row)
		row = append(r, i.numFoundRows)
		for _, k := range i.keys {
			// キーは足す前の子の行で評価する。足した列を含む行を渡すと、副問い合わせのキー（ORDER BY (SELECT ...)）
			// は外の行の長さを読み違え、外の列の位置がずれる（上流の #3626）
			v, err := k.Eval(ctx, row[:base])
			if err != nil {
				return sql.ErrUnableSort.Wrap(err)
			}
			row = append(row, v)
		}

		if i.limit > 0 && int64(topRowsHeap.Len()) == i.limit {
			// ヒープが満ちていれば、頂点（残す行のうち最後に並ぶ行）より前に並ぶ行だけを入れ替える。順番の列が最後の
			// キーなので同順位はなく、入れてから頂点を出すのと同じ結果になる（HeimDB の段階 96）。
			rows := append(topRowsHeap.Sorter.Rows, row)
			topRowsHeap.Sorter.Rows = rows
			before := topRowsHeap.Sorter.Less(len(rows)-1, 0)
			topRowsHeap.Sorter.Rows = rows[:len(rows)-1]
			if before {
				topRowsHeap.Sorter.Rows[0] = row
				heap.Fix(topRowsHeap, 0)
			}
		} else {
			heap.Push(topRowsHeap, row)
			if int64(topRowsHeap.Len()) > i.limit {
				heap.Pop(topRowsHeap)
			}
		}
		if topRowsHeap.LastError != nil {
			return topRowsHeap.LastError
		}
	}

	var err error
	i.topRows, err = topRowsHeap.Rows()
	return err
}

// GetInt64Value returns the int64 literal value in the expression given, or an error with the errStr given if it
// cannot.
func GetInt64Value(ctx *sql.Context, expr sql.Expression) (int64, error) {
	i, err := expr.Eval(ctx, nil)
	if err != nil {
		return 0, err
	}

	switch i := i.(type) {
	case int:
		return int64(i), nil
	case int8:
		return int64(i), nil
	case int16:
		return int64(i), nil
	case int32:
		return int64(i), nil
	case int64:
		return i, nil
	case uint:
		return int64(i), nil
	case uint8:
		return int64(i), nil
	case uint16:
		return int64(i), nil
	case uint32:
		return int64(i), nil
	case uint64:
		return int64(i), nil
	default:
		// analyzer should catch this already
		panic(fmt.Sprintf("Unsupported type for limit %T", i))
	}
}

type JsonTableColOpts struct {
	Name      string
	Typ       sql.Type
	ForOrd    bool
	Exists    bool
	DefErrVal interface{}
	DefEmpVal interface{}
	ErrOnErr  bool
	ErrOnEmp  bool
}

// JsonTableCol represents a column in a json table.
type JsonTableCol struct {
	Path string // if there are nested columns, this is a schema Path, otherwise it is a col Path
	Opts *JsonTableColOpts
	Cols []*JsonTableCol // nested columns

	data     []interface{}
	err      error
	pos      int
	finished bool // exhausted all rows in data
	currSib  int
}

// IsSibling returns if the jsonTableCol contains multiple columns
func (c *JsonTableCol) IsSibling() bool {
	return len(c.Cols) != 0
}

// NextSibling starts at the current sibling and moves to the next unfinished sibling
// if there are no more unfinished siblings, it sets c.currSib to the first sibling and returns true
// if the c.currSib is unfinished, nothing changes
func (c *JsonTableCol) NextSibling() bool {
	for i := c.currSib; i < len(c.Cols); i++ {
		if c.Cols[i].IsSibling() && !c.Cols[i].finished {
			c.currSib = i
			return false
		}
	}
	c.currSib = 0
	for i := 0; i < len(c.Cols); i++ {
		if c.Cols[i].IsSibling() {
			c.currSib = i
			break
		}
	}
	return true
}

// LoadData loads the data for this column from the given object and c.path
// LoadData will always wrap the data in a slice to ensure it is iterable
// Additionally, this function will set the c.currSib to the first sibling
func (c *JsonTableCol) LoadData(obj interface{}) {
	var data interface{}
	data, c.err = jsonpath.JsonPathLookup(obj, c.Path)
	if d, ok := data.([]interface{}); ok {
		c.data = d
	} else {
		c.data = []interface{}{data}
	}
	c.pos = 0

	c.NextSibling()
}

// Reset clears the column's data and error, and recursively resets all nested columns
func (c *JsonTableCol) Reset() {
	c.data, c.err = nil, nil
	c.finished = false
	for _, col := range c.Cols {
		col.Reset()
	}
}

// Next returns the next row for this column.
func (c *JsonTableCol) Next(ctx *sql.Context, obj interface{}, pass bool, ord int) (sql.Row, error) {
	// nested column should recurse
	if len(c.Cols) != 0 {
		if c.data == nil {
			c.LoadData(obj)
		}

		var innerObj interface{}
		if !c.finished {
			innerObj = c.data[c.pos]
		}

		var row sql.Row
		for i, col := range c.Cols {
			innerPass := len(col.Cols) != 0 && i != c.currSib
			rowPart, err := col.Next(ctx, innerObj, pass || innerPass, c.pos+1)
			if err != nil {
				return nil, err
			}
			row = append(row, rowPart...)
		}

		if pass {
			return row, nil
		}

		if c.NextSibling() {
			for _, col := range c.Cols {
				col.Reset()
			}
			c.pos++
		}

		if c.pos >= len(c.data) {
			c.finished = true
		}

		return row, nil
	}

	// this should only apply to nested columns, maybe...
	if pass {
		return sql.Row{nil}, nil
	}

	// FOR ORDINAL is a special case
	if c.Opts != nil && c.Opts.ForOrd {
		return sql.Row{ord}, nil
	}

	// TODO: cache this?
	val, err := jsonpath.JsonPathLookup(obj, c.Path)
	if c.Opts.Exists {
		if err != nil {
			return sql.Row{0}, nil
		} else {
			return sql.Row{1}, nil
		}
	}

	// key error means empty
	if err != nil {
		if c.Opts.ErrOnEmp {
			return nil, fmt.Errorf("missing value for JSON_TABLE column '%s'", c.Opts.Name)
		}
		val = c.Opts.DefEmpVal
	}

	val, _, err = c.Opts.Typ.Convert(ctx, val)
	if err != nil {
		if c.Opts.ErrOnErr {
			return nil, err
		}
		val, _, err = c.Opts.Typ.Convert(ctx, c.Opts.DefErrVal)
		if err != nil {
			return nil, err
		}
	}

	// Base columns are always finished
	c.finished = true
	return sql.Row{val}, nil
}

type JsonTableRowIter struct {
	Data    []interface{}
	pos     int
	Cols    []*JsonTableCol
	currSib int
}

// NextSibling starts at the current sibling and moves to the next unfinished sibling
// if there are no more unfinished siblings, it resets to the first sibling
func (j *JsonTableRowIter) NextSibling() bool {
	for i := j.currSib; i < len(j.Cols); i++ {
		if !j.Cols[i].finished && len(j.Cols[i].Cols) != 0 {
			j.currSib = i
			return false
		}
	}
	j.currSib = 0
	for i := 0; i < len(j.Cols); i++ {
		if len(j.Cols[i].Cols) != 0 {
			j.currSib = i
			break
		}
	}
	return true
}

func (j *JsonTableRowIter) ResetAll() {
	for _, col := range j.Cols {
		col.Reset()
	}
}

func (j *JsonTableRowIter) Next(ctx *sql.Context) (sql.Row, error) {
	if j.pos >= len(j.Data) {
		return nil, io.EOF
	}
	obj := j.Data[j.pos]

	var row sql.Row
	for i, col := range j.Cols {
		pass := len(col.Cols) != 0 && i != j.currSib
		rowPart, err := col.Next(ctx, obj, pass, j.pos+1)
		if err != nil {
			return nil, err
		}
		row = append(row, rowPart...)
	}

	if j.NextSibling() {
		j.ResetAll()
		j.pos++
	}

	return row, nil
}

func (j *JsonTableRowIter) Close(ctx *sql.Context) error {
	return nil
}

// orderedDistinctIter iterates the children iterator and skips all the
// repeated rows assuming the iterator has all rows sorted.
type orderedDistinctIter struct {
	childIter sql.RowIter
	schema    sql.Schema
	prevRow   sql.Row
}

func NewOrderedDistinctIter(child sql.RowIter, schema sql.Schema) *orderedDistinctIter {
	return &orderedDistinctIter{childIter: child, schema: schema}
}

func (di *orderedDistinctIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		row, err := di.childIter.Next(ctx)
		if err != nil {
			return nil, err
		}

		if di.prevRow != nil {
			ok, err := di.prevRow.Equals(ctx, row, di.schema)
			if err != nil {
				return nil, err
			}

			if ok {
				continue
			}
		}

		di.prevRow = row
		return row, nil
	}
}

func (di *orderedDistinctIter) Close(ctx *sql.Context) error {
	return di.childIter.Close(ctx)
}

// TODO a queue is probably more optimal
type RecursiveTableIter struct {
	pos int
	Buf []sql.Row
}

var _ sql.RowIter = (*RecursiveTableIter)(nil)

func (r *RecursiveTableIter) Next(ctx *sql.Context) (sql.Row, error) {
	if r.Buf == nil || r.pos >= len(r.Buf) {
		return nil, io.EOF
	}
	r.pos++
	return r.Buf[r.pos-1], nil
}

func (r *RecursiveTableIter) Close(ctx *sql.Context) error {
	r.Buf = nil
	return nil
}

type LimitIter struct {
	CalcFoundRows bool
	currentPos    int64
	ChildIter     sql.RowIter
	Limit         int64
	counted       bool // 残りの行を数え終えた（HeimDB。この後の Next は子を読まない）
}

func (li *LimitIter) Next(ctx *sql.Context) (sql.Row, error) {
	if li.currentPos >= li.Limit {
		// If we were asked to calc all found rows, then when we are past the limit we iterate over the rest of the
		// result set to count it
		if li.CalcFoundRows {
			// HeimDB: 子が数えられるなら、行を作らずに数えさせる（記事の一覧で、返さない行の本体を引いて復号しない）。
			// 子は実行時に差し替わる（サーバーが deferred projection の Project を外す）ので、ここで判定する。
			if li.counted {
				return nil, io.EOF
			}
			if rc, ok := li.ChildIter.(sql.RowCounter); ok {
				n, ok, err := rc.CountRest(ctx)
				if err != nil {
					li.counted = true
					return nil, err
				}
				if ok {
					li.currentPos += n
					li.counted = true
					return nil, io.EOF
				}
			}
			for {
				_, err := li.ChildIter.Next(ctx)
				if err != nil {
					li.counted = err == io.EOF
					return nil, err
				}
				li.currentPos++
			}
		}

		return nil, io.EOF
	}

	childRow, err := li.ChildIter.Next(ctx)
	if err != nil {
		return nil, err
	}
	li.currentPos++

	return childRow, nil
}

func (li *LimitIter) Close(ctx *sql.Context) error {
	err := li.ChildIter.Close(ctx)
	if err != nil {
		return err
	}

	if li.CalcFoundRows {
		ctx.SetLastQueryInfoInt(sql.FoundRows, li.currentPos)
	}
	return nil
}

type sortIter struct {
	sortFields sql.SortFields
	keys       []sql.Expression // 先に計算して行の後ろに足す並べ替えのキー
	keyIdx     []int            // sortFields の中で keys に替える位置
	childIter  sql.RowIter
	sortedRows []sql.Row
	idx        int
	base       int // キーを足す前の行の長さ（キーがなければ -1）
}

var _ sql.RowIter = (*sortIter)(nil)

func NewSortIter(s sql.SortFields, child sql.RowIter) *sortIter {
	// 列の参照でない並べ替えの式は、比べるたびに評価せず、行ごとに 1 回計算して行の後ろに足し、返す前に外す。比べる
	// たびに評価すると、ORDER BY RAND() が比べるごとに違う値になって偏り、重い式（CAST、副問い合わせ）は行の数の
	// log 倍だけ評価された（上流の #3733 の問題。上流は sorters パッケージに作り直すが、ここでは段階 96 の TopN と同じ形）。
	var keys []sql.Expression
	var keyIdx []int
	for j, f := range s {
		switch f.Column.(type) {
		case *expression.GetField, *expression.Literal:
		default:
			keys = append(keys, f.Column)
			keyIdx = append(keyIdx, j)
		}
	}
	return &sortIter{
		sortFields: s,
		keys:       keys,
		keyIdx:     keyIdx,
		childIter:  child,
		idx:        -1,
		base:       -1,
	}
}

func (i *sortIter) Next(ctx *sql.Context) (sql.Row, error) {
	if i.idx == -1 {
		err := i.computeSortedRows(ctx)
		if err != nil {
			return nil, err
		}
		i.idx = 0
	}

	if i.idx >= len(i.sortedRows) {
		return nil, io.EOF
	}
	row := i.sortedRows[i.idx]
	i.idx++
	if i.base >= 0 {
		return row[:i.base], nil
	}
	return row, nil
}

func (i *sortIter) Close(ctx *sql.Context) error {
	i.sortedRows = nil
	return i.childIter.Close(ctx)
}

func (i *sortIter) computeSortedRows(ctx *sql.Context) error {
	cache, dispose := ctx.Memory.NewRowsCache()
	defer dispose()

	sortFields := i.sortFields
	for {
		row, err := i.childIter.Next(ctx)

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if len(i.keys) > 0 {
			if i.base < 0 {
				// 足したキーの位置は、最初の行の長さから決める（副問い合わせの中の行は外の行を前に持つ）
				i.base = len(row)
				sortFields = make(sql.SortFields, len(i.sortFields))
				copy(sortFields, i.sortFields)
				for k, j := range i.keyIdx {
					f := sortFields[j]
					gf := expression.NewGetField(i.base+k, f.Column.Type(), f.Column.String(), f.Column.IsNullable())
					f.Column, f.Column2 = gf, gf
					sortFields[j] = f
				}
			}
			r := make(sql.Row, len(row), len(row)+len(i.keys))
			copy(r, row)
			for _, k := range i.keys {
				v, err := k.Eval(ctx, row)
				if err != nil {
					return sql.ErrUnableSort.Wrap(err)
				}
				r = append(r, v)
			}
			row = r
		}

		if err := cache.Add(row); err != nil {
			return err
		}
	}

	rows := cache.Get()
	sorter := &expression.Sorter{
		SortFields: sortFields,
		Rows:       rows,
		LastError:  nil,
		Ctx:        ctx,
	}
	sort.Stable(sorter)
	if sorter.LastError != nil {
		return sorter.LastError
	}
	i.sortedRows = rows
	return nil
}

// distinctIter keeps track of the hashes of all rows that have been emitted.
// It does not emit any rows whose hashes have been seen already.
// TODO: come up with a way to use less memory than keeping all hashes in memory.
// Even though they are just 64-bit integers, this could be a problem in large
// result sets.
type distinctIter struct {
	childIter   sql.RowIter
	seen        sql.KeyValueCache
	DisposeFunc sql.DisposeFunc
	typs        []sql.Type
}

// NewDistinctIter は、typs（行の列の型）の照合順序で等しい行を一つにする。HeimDB: もとは型を見ず、'a' と 'A' を
// 別の行にしていた。
func NewDistinctIter(ctx *sql.Context, child sql.RowIter, typs []sql.Type) *distinctIter {
	cache, dispose := ctx.Memory.NewHistoryCache()
	return &distinctIter{
		childIter:   child,
		seen:        cache,
		DisposeFunc: dispose,
		typs:        typs,
	}
}

func (di *distinctIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		row, err := di.childIter.Next(ctx)
		if err != nil {
			if err == io.EOF {
				di.Dispose()
			}
			return nil, err
		}

		hash, err := sql.HashOfTypes(ctx, row, di.typs)
		if err != nil {
			return nil, err
		}

		if _, err := di.seen.Get(hash); err == nil {
			continue
		}

		if err := di.seen.Put(hash, struct{}{}); err != nil {
			return nil, err
		}

		return row, nil
	}
}

func (di *distinctIter) Close(ctx *sql.Context) error {
	di.Dispose()
	return di.childIter.Close(ctx)
}

func (di *distinctIter) Dispose() {
	if di.DisposeFunc != nil {
		di.DisposeFunc()
		di.DisposeFunc = nil
	}
}

type UnionIter struct {
	Cur      sql.RowIter
	NextIter func(ctx *sql.Context) (sql.RowIter, error)
}

func (ui *UnionIter) Next(ctx *sql.Context) (sql.Row, error) {
	res, err := ui.Cur.Next(ctx)
	if err == io.EOF {
		if ui.NextIter == nil {
			return nil, io.EOF
		}
		err = ui.Cur.Close(ctx)
		if err != nil {
			return nil, err
		}
		ui.Cur, err = ui.NextIter(ctx)
		ui.NextIter = nil
		if err != nil {
			return nil, err
		}
		return ui.Cur.Next(ctx)
	}
	return res, err
}

func (ui *UnionIter) Close(ctx *sql.Context) error {
	if ui.Cur != nil {
		return ui.Cur.Close(ctx)
	} else {
		return nil
	}
}

type IntersectIter struct {
	LIter, RIter sql.RowIter
	cached       bool
	cache        map[uint64]int
	Types        []sql.Type // HeimDB: 行の列の型（照合順序で等しい値を同じにする）
}

func (ii *IntersectIter) Next(ctx *sql.Context) (sql.Row, error) {
	if !ii.cached {
		ii.cache = make(map[uint64]int)
		for {
			res, err := ii.RIter.Next(ctx)
			if err != nil && err != io.EOF {
				return nil, err
			}

			hash, herr := sql.HashOfTypes(ctx, res, ii.Types)
			if herr != nil {
				return nil, herr
			}
			if _, ok := ii.cache[hash]; !ok {
				ii.cache[hash] = 0
			}
			ii.cache[hash]++

			if err == io.EOF {
				break
			}
		}
		ii.cached = true
	}

	for {
		res, err := ii.LIter.Next(ctx)
		if err != nil {
			return nil, err
		}

		hash, herr := sql.HashOfTypes(ctx, res, ii.Types)
		if herr != nil {
			return nil, herr
		}
		if _, ok := ii.cache[hash]; !ok {
			continue
		}
		if ii.cache[hash] <= 0 {
			continue
		}
		ii.cache[hash]--

		return res, nil
	}
}

func (ii *IntersectIter) Close(ctx *sql.Context) error {
	if ii.LIter != nil {
		if err := ii.LIter.Close(ctx); err != nil {
			return err
		}
	}
	if ii.RIter != nil {
		if err := ii.RIter.Close(ctx); err != nil {
			return err
		}
	}
	return nil
}

type ExceptIter struct {
	LIter, RIter sql.RowIter
	cached       bool
	cache        map[uint64]int
	Types        []sql.Type // HeimDB: 行の列の型（照合順序で等しい値を同じにする）
}

func (ei *ExceptIter) Next(ctx *sql.Context) (sql.Row, error) {
	if !ei.cached {
		ei.cache = make(map[uint64]int)
		for {
			res, err := ei.RIter.Next(ctx)
			if err != nil && err != io.EOF {
				return nil, err
			}

			hash, herr := sql.HashOfTypes(ctx, res, ei.Types)
			if herr != nil {
				return nil, herr
			}
			if _, ok := ei.cache[hash]; !ok {
				ei.cache[hash] = 0
			}
			ei.cache[hash]++

			if err == io.EOF {
				break
			}
		}
		ei.cached = true
	}

	for {
		res, err := ei.LIter.Next(ctx)
		if err != nil {
			return nil, err
		}

		hash, herr := sql.HashOfTypes(ctx, res, ei.Types)
		if herr != nil {
			return nil, herr
		}
		if _, ok := ei.cache[hash]; !ok {
			return res, nil
		}
		if ei.cache[hash] <= 0 {
			return res, nil
		}
		ei.cache[hash]--
	}
}

func (ei *ExceptIter) Close(ctx *sql.Context) error {
	if ei.LIter != nil {
		if err := ei.LIter.Close(ctx); err != nil {
			return err
		}
	}
	if ei.RIter != nil {
		if err := ei.RIter.Close(ctx); err != nil {
			return err
		}
	}
	return nil
}
