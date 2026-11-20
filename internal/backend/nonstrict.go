package backend

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/rowexec"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 非 STRICT の sql_mode での値の扱い（docs/plan.md の段階 21）。
//
// WordPress は接続ごとに STRICT を外した sql_mode にする。そのとき MySQL は、次の値をエラーにせず、値を直して警告に
// する（go-mysql-server はエラーにする）。
//   - NOT NULL の列への NULL（UPDATE、複数行の INSERT、INSERT ... SELECT、REPLACE）: 型の既定値と警告 1048。
//     単一行の INSERT ... VALUES の NULL は MySQL でもエラー 1048 なので直さない。
//   - 省いた、既定値のない NOT NULL の列: 型の既定値と警告 1364（単一行でも）。ENUM は最初の値で、警告なし（段階 114）。
//   - 不正な日時（文字列と、日時として読めない数）: ゼロの日時と警告 1265。時刻が途中までの '2012-01-01 0' は正しい値として受け付ける。後ろに余計な
//     文字のある '2020-01-01 a' は前の部分と警告 1265（段階 114）。
//   - 数の列に、数として読めない文字列: 先頭の数の部分（'12abc' なら 12）と警告 1265。数で始まらなければ（'abc'）0 と
//     警告 1366（段階 85）。
//   - ENUM と SET の列に、定義にない値: 空の値と警告 1265（段階 116）。
//   - 数の列に、型の範囲の外の値: 型の端の値と警告 1264。文字列の列に、長さを超える値: 切った値と警告 1265（段階 92）。
//
// go-mysql-server が値を確かめる前に直すため、INSERT は挿入する行の元を、UPDATE と INSERT ... ON DUPLICATE KEY UPDATE
// は SET の右辺の式を包む。INSERT IGNORE と UPDATE IGNORE は、STRICT でも MySQL が非 STRICT の複数行の INSERT と
// 同じように直すので、同じように包む（go-mysql-server は端の値と切り方が MySQL と違い、警告を Note と別の字にする。
// 段階 92）。
//
// go-mysql-server は ON DUPLICATE KEY UPDATE の結果の行の NULL を確かめない（STRICT でも NOT NULL の列に NULL を
// 書き込んでいた）。そのため ON DUPLICATE KEY UPDATE の右辺は STRICT でも包み、NULL をエラー 1048 にする。

// nonStrictValues は、木の中の INSERT と UPDATE を、非 STRICT の値の扱いをするように書き換える。strict なら、
// ON DUPLICATE KEY UPDATE の NULL をエラーにすることだけをする。
func nonStrictValues(n sql.Node, strict bool) (sql.Node, error) {
	n, _, err := transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		switch node := node.(type) {
		case *plan.InsertInto:
			if _, done := node.Source.(*nonStrictRows); done {
				return node, transform.SameTree, nil
			}
			strict := strict && !node.Ignore
			multi := node.Ignore || !singleValuesRow(node.Source)
			nn := node
			// ON DUPLICATE KEY UPDATE の警告の行の番号は、INSERT の行の番号（MySQL と同じ）
			var rows *rowCounter
			if !strict {
				rows = &rowCounter{}
				nn = node.WithSource(&nonStrictRows{
					child:    node.Source,
					sch:      node.Destination.Schema(),
					deferred: node.DeferredDefaults,
					multi:    multi,
					rows:     rows,
					order:    columnOrder(node.Destination.Schema(), node.ColumnNames),
				})
			}
			if onDup, ok := wrapSetFields(node.OnDupExprs, strict || !multi, !strict, true, rows); ok {
				if nn == node {
					c := *node
					nn = &c
				}
				nn.OnDupExprs = onDup
			}
			if nn == node {
				return node, transform.SameTree, nil
			}
			return nn, transform.NewTree, nil
		case *plan.UpdateSource:
			if strict && !node.Ignore {
				return node, transform.SameTree, nil
			}
			exprs, ok := wrapSetFields(node.Expressions(), false, true, false, nil)
			if !ok {
				return node, transform.SameTree, nil
			}
			nn, err := node.WithExpressions(exprs...)
			if err != nil {
				return nil, transform.SameTree, err
			}
			return nn, transform.NewTree, nil
		}
		return node, transform.SameTree, nil
	})
	return n, err
}

// wrapSetFields は、SET の式の右辺を nonStrictValue で包んだ複製を返す（ok は包んだものがあるか）。nullError なら
// NOT NULL の列への NULL をエラー 1048 に、そうでなければ型の既定値と警告 1048 にする（warnOnce なら警告は文で 1 回。
// MySQL は ON DUPLICATE KEY UPDATE では 1 回、UPDATE では行ごとに出す）。fixDates なら値を直す（fixValue）。
// rows があれば、警告の行の番号をそこから取る（ON DUPLICATE KEY UPDATE）。
func wrapSetFields(exprs []sql.Expression, nullError, fixDates, warnOnce bool, rows *rowCounter) ([]sql.Expression, bool) {
	var out []sql.Expression
	for i, e := range exprs {
		sf, ok := e.(*expression.SetField)
		if !ok {
			continue
		}
		gf, ok := sf.LeftChild.(*expression.GetField)
		if !ok {
			continue
		}
		if _, done := sf.RightChild.(*nonStrictValue); done {
			continue
		}
		if out == nil {
			out = append([]sql.Expression(nil), exprs...)
		}
		col := &sql.Column{Name: gf.Name(), Type: gf.Type(), Nullable: gf.IsNullable()}
		// WithChildren で作り直し、ON UPDATE の列の式の派生の印（expression.IsDerivedSetField）を残す（段階 111）
		ns, err := sf.WithChildren(gf, &nonStrictValue{Expression: sf.RightChild, col: col, nullError: nullError, fixDates: fixDates, warnOnce: warnOnce, rows: rows})
		if err != nil {
			continue
		}
		out[i] = ns
	}
	return out, out != nil
}

// singleValuesRow は、INSERT の行の元が 1 行だけの VALUES か。
func singleValuesRow(src sql.Node) bool {
	single := false
	transform.Inspect(src, func(n sql.Node) bool {
		if v, ok := n.(*plan.Values); ok {
			single = len(v.ExpressionTuples) == 1
			return false
		}
		return true
	})
	return single
}

// nonStrictRows は、INSERT の行の元を包み、行の値を非 STRICT の扱いで直す。子は go-mysql-server がそのまま見られる
// ように Children で返す（rowexec の getInsertExpressions が中の Project を探す）。
type nonStrictRows struct {
	child    sql.Node
	sch      sql.Schema // 挿入先のテーブルの列（行の値はこの順）
	deferred sql.FastIntSet
	multi    bool
	rows     *rowCounter // 読んだ行の番号を ON DUPLICATE KEY UPDATE の右辺に知らせる
	order    []int       // 値を直す列の順（警告の順）。INSERT に書いた列の順、その後に書かなかった列（MySQL と同じ）
}

// columnOrder は、sch の列の番号を、names（INSERT に書いた列）の順、その後に残りを表の順で並べる。
func columnOrder(sch sql.Schema, names []string) []int {
	order := make([]int, 0, len(sch))
	var seen sql.FastIntSet
	for _, name := range names {
		if i := sch.IndexOfColName(name); i >= 0 && !seen.Contains(i) {
			order = append(order, i)
			seen.Add(i)
		}
	}
	for i := range sch {
		if !seen.Contains(i) {
			order = append(order, i)
		}
	}
	return order
}

// rowCounter は、INSERT の行の元から読んだ行の番号。
type rowCounter struct{ n int }

var _ sql.ExecSourceRel = (*nonStrictRows)(nil)

func (n *nonStrictRows) Resolved() bool       { return n.child.Resolved() }
func (n *nonStrictRows) IsReadOnly() bool     { return n.child.IsReadOnly() }
func (n *nonStrictRows) Schema() sql.Schema   { return n.child.Schema() }
func (n *nonStrictRows) Children() []sql.Node { return []sql.Node{n.child} }
func (n *nonStrictRows) String() string       { return "heimdb.NonStrictRows(" + n.child.String() + ")" }

func (n *nonStrictRows) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(n, len(children), 1)
	}
	nn := *n
	nn.child = children[0]
	return &nn, nil
}

func (n *nonStrictRows) RowIter(ctx *sql.Context, row sql.Row) (sql.RowIter, error) {
	iter, err := rowexec.DefaultBuilder.Build(ctx, n.child, row)
	if err != nil {
		return nil, err
	}
	return &nonStrictRowIter{RowIter: iter, n: n}, nil
}

type nonStrictRowIter struct {
	sql.RowIter
	n      *nonStrictRows
	rowNum int
	// warned は、警告 1048 と 1364 を出した列（MySQL は複数行の INSERT でも、列ごとに文で 1 回だけ出す）。
	warned sql.FastIntSet
}

func (it *nonStrictRowIter) Next(ctx *sql.Context) (sql.Row, error) {
	row, err := it.RowIter.Next(ctx)
	if err != nil {
		return nil, err
	}
	it.rowNum++
	if it.n.rows != nil {
		it.n.rows.n = it.rowNum
	}
	// 外側のスコープの値が前に付くことがある（rowexec の insertIter と同じく、後ろの列を挿入先の列に当てる）。
	off := len(row) - len(it.n.sch)
	if off < 0 {
		return row, nil
	}
	var out sql.Row
	for _, i := range it.n.order {
		col := it.n.sch[i]
		v := row[off+i]
		var nv any
		var ok bool
		switch {
		case v == nil && !col.Nullable && !col.AutoIncrement && it.n.deferred.Contains(i) && types.IsEnum(col.Type):
			// 省いた NOT NULL の ENUM の列は、MySQL と同じく警告なしで最初の値（段階 114、上流の #2985）
			nv, ok = uint16(1), true
		case v == nil && !col.Nullable && !col.AutoIncrement && it.n.deferred.Contains(i):
			if !it.warned.Contains(i) {
				ctx.Warn(1364, "Field '%s' doesn't have a default value", col.Name)
				it.warned.Add(i)
			}
			nv, ok = col.Type.Zero(), true
		case v == nil && !col.Nullable && !col.AutoIncrement && it.n.multi:
			if !it.warned.Contains(i) {
				ctx.Warn(1048, "Column '%s' cannot be null", col.Name)
				it.warned.Add(i)
			}
			nv, ok = col.Type.Zero(), true
		default:
			nv, ok = fixValue(ctx, col, v, it.rowNum)
		}
		if !ok {
			continue
		}
		if out == nil {
			out = append(sql.Row(nil), row...)
		}
		out[off+i] = nv
	}
	if out == nil {
		return row, nil
	}
	return out, nil
}

// nonStrictValue は、UPDATE と ON DUPLICATE KEY UPDATE の SET の右辺を包み、値を非 STRICT の扱いで直す式。
type nonStrictValue struct {
	sql.Expression
	col       *sql.Column
	nullError bool // NOT NULL の列への NULL をエラー 1048 にする（STRICT か、単一行の INSERT の ON DUPLICATE KEY UPDATE）
	fixDates  bool
	warnOnce  bool        // 警告 1048 を文で 1 回だけ出す
	rows      *rowCounter // あれば、警告の行の番号はこれ（ON DUPLICATE KEY UPDATE）
	warned    bool
	rowNum    int // この式を評価した回数（SET の右辺は行ごとに 1 回評価されるので、行の番号になる）
}

func (e *nonStrictValue) Eval(ctx *sql.Context, row sql.Row) (any, error) {
	v, err := e.Expression.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	e.rowNum++
	if v == nil && !e.col.Nullable {
		if e.nullError {
			return nil, sql.ErrInsertIntoNonNullableProvidedNull.New(e.col.Name)
		}
		if !e.warnOnce || !e.warned {
			ctx.Warn(1048, "Column '%s' cannot be null", e.col.Name)
			e.warned = true
		}
		return e.col.Type.Zero(), nil
	}
	if !e.fixDates {
		return v, nil
	}
	rowNum := e.rowNum
	if e.rows != nil {
		rowNum = e.rows.n
	}
	if nv, ok := fixValue(ctx, e.col, v, rowNum); ok {
		return nv, nil
	}
	return v, nil
}

func (e *nonStrictValue) Children() []sql.Expression { return []sql.Expression{e.Expression} }

func (e *nonStrictValue) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 1)
	}
	return &nonStrictValue{Expression: children[0], col: e.col, nullError: e.nullError, fixDates: e.fixDates, warnOnce: e.warnOnce, rows: e.rows}, nil
}

func (e *nonStrictValue) String() string { return e.Expression.String() }

// fixValue は、列 col に入れる値 v を、非 STRICT の扱いで直す（fixDatetime、fixNumber、fixString）。直さなければ ok
// が false。
func fixValue(ctx *sql.Context, col *sql.Column, v any, rowNum int) (any, bool) {
	if nv, ok := fixDatetime(ctx, col, v, rowNum); ok {
		return nv, true
	}
	if nv, ok := fixNumber(ctx, col, v, rowNum); ok {
		return nv, true
	}
	if nv, ok := fixEnumSet(ctx, col, v, rowNum); ok {
		return nv, true
	}
	return fixString(ctx, col, v, rowNum)
}

// fixEnumSet は、ENUM と SET の列に入れる定義にない値 v を、MySQL の非 STRICT と同じく空の値（ENUM の番号 0、SET は
// 定義にある要素だけの集合）と警告 1265 にする（段階 116。前は 1105 で文が止まった。WordPress の sql_mode で NOT NULL
// の ENUM への空の文字列）。
func fixEnumSet(ctx *sql.Context, col *sql.Column, v any, rowNum int) (any, bool) {
	if v == nil || !(types.IsEnum(col.Type) || types.IsSet(col.Type)) {
		return nil, false
	}
	if _, _, err := col.Type.Convert(ctx, v); err == nil {
		return nil, false
	}
	ctx.Warn(1265, "Data truncated for column '%s' at row %d", col.Name, rowNum)
	if types.IsEnum(col.Type) {
		return uint16(0), true
	}
	// SET は、MySQL と同じく定義にある要素だけを残す（'x,bad' は 'x'。段階 116 のレビュー）
	var bits uint64
	if s, ok := v.(string); ok {
		for _, m := range strings.Split(s, ",") {
			if b, _, err := col.Type.Convert(ctx, m); err == nil {
				if u, ok := b.(uint64); ok {
					bits |= u
				}
			}
		}
	}
	return bits, true
}

// fixNumber は、数の列に入れる値 v を、MySQL の非 STRICT と同じく直す。
//   - 型の範囲の外（整数は丸めてから、DECIMAL は小数の桁に丸めてから比べる）: 入れようとした値の符号で、型の下の端か
//     上の端と警告 1264（段階 92）。go-mysql-server の Convert は UNSIGNED の負の数を回り込ませ、DECIMAL では値を
//     返さないので、端の値は自分で決める。文字列の後ろが数でない DECIMAL（'99999abc'）は、MySQL と同じく Note 1265
//     の後に 1264 を出す（整数は 1264 だけ）。
//   - 数として読めない文字列: 先頭の数の部分（go-mysql-server の Convert が誤りと一緒に返す値）と警告 1265。数で
//     始まらなければ（'abc'）0 と警告 1366（段階 85。前はエラー 1366 だった）。
func fixNumber(ctx *sql.Context, col *sql.Column, v any, rowNum int) (any, bool) {
	if v == nil || !types.IsNumber(col.Type) {
		return nil, false
	}
	cv, inRange, err := col.Type.Convert(ctx, v)
	// go-mysql-server の Convert は、UNSIGNED の整数の列への DECIMAL の値を、上限を超えても範囲の中とする（float の
	// 0 や 2^31 以上を範囲の外とする誤りもある）。この 2 つは下で端と比べる
	_, isDec := v.(decimal.Decimal)
	_, isFloat := v.(float64)
	if err == nil && inRange == sql.InRange && !(types.IsUnsigned(col.Type) && (isDec || isFloat)) {
		// DECIMAL の小数の桁より細かい値は、丸めて Note 1265（MySQL と同じ。整数の列は警告なしに丸める）
		if dt, ok := col.Type.(sql.DecimalType); ok && mayHaveMoreFraction(v, int32(dt.Scale())) {
			if d, ok := numberValue(v); ok && !d.Equal(d.Round(int32(dt.Scale()))) {
				ctx.Session.Warn(&sql.Warning{Level: "Note", Code: 1265, Message: fmt.Sprintf("Data truncated for column '%s' at row %d", col.Name, rowNum)})
				return cv, true
			}
		}
		return nil, false
	}
	s, isStr := v.(string)
	if _, _, declared := types.RealPrecision(col.Type); (declared || types.RealUnsigned(col.Type)) && cv != nil && inRange != sql.InRange &&
		(err == nil || (isStr && types.TruncatedString(s, err))) {
		// FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の範囲の外は、Convert が MySQL と同じく丸めて決めた端の値（UNSIGNED の負は 0）と
		// 警告 1264（段階 120）
		ctx.Warn(1264, "Out of range value for column '%s' at row %d", col.Name, rowNum)
		return cv, true
	}
	if lo, hi, places, ok := numberBounds(col.Type); ok {
		if d, ok := numberValue(v); ok {
			// UNSIGNED には、丸めると 0 になる負の数（-0.4）も範囲の外（MySQL と同じ）
			if types.IsUnsigned(col.Type) && d.IsNegative() {
				d = lo.Sub(decimal.New(1, 0))
			}
			if !types.IsFloat(col.Type) {
				d = d.Round(places)
			}
			if d.LessThan(lo) || d.GreaterThan(hi) {
				if isStr && types.IsDecimal(col.Type) {
					if _, truncated := sql.TruncateStringToDouble(s, false); truncated {
						ctx.Session.Warn(&sql.Warning{Level: "Note", Code: 1265, Message: fmt.Sprintf("Data truncated for column '%s' at row %d", col.Name, rowNum)})
					}
				}
				ctx.Warn(1264, "Out of range value for column '%s' at row %d", col.Name, rowNum)
				if d.LessThan(lo) {
					return boundValue(col.Type, lo), true
				}
				return boundValue(col.Type, hi), true
			}
			if err == nil && inRange != sql.InRange {
				// 端の中なのに Convert が範囲の外とした（UNSIGNED への float の 0 や 3e9）。丸めた値を入れる
				return boundValue(col.Type, d), true
			}
		}
	}
	if !isStr || err == nil || !types.TruncatedString(s, err) || cv == nil || inRange != sql.InRange {
		return nil, false
	}
	if prefix, _ := sql.TruncateStringToDouble(s, false); prefix == "0" && !startsWithDigit(s) {
		kind := "integer"
		switch {
		case types.IsFloat(col.Type):
			kind = "double"
		case types.IsDecimal(col.Type):
			kind = "decimal"
		}
		ctx.Warn(1366, "Incorrect %s value: '%s' for column '%s' at row %d", kind, s, col.Name, rowNum)
	} else if types.IsDecimal(col.Type) {
		// MySQL は DECIMAL の列への切り詰めを、警告ではなく Note にする
		ctx.Session.Warn(&sql.Warning{Level: "Note", Code: 1265, Message: fmt.Sprintf("Data truncated for column '%s' at row %d", col.Name, rowNum)})
	} else {
		ctx.Warn(1265, "Data truncated for column '%s' at row %d", col.Name, rowNum)
	}
	return cv, true
}

// numberBounds は、数の型 t の下の端と上の端、比べる前に丸める小数の桁（FLOAT と DOUBLE は丸めない。ok は端を
// 知っている型か。BIT と YEAR は知らない）。
func numberBounds(t sql.Type) (lo, hi decimal.Decimal, places int32, ok bool) {
	if dt, isDec := t.(sql.DecimalType); isDec {
		scale := int32(dt.Scale())
		hi = dt.ExclusiveUpperBound().Sub(decimal.New(1, -scale))
		return hi.Neg(), hi, scale, true
	}
	signed := func(bits uint) (decimal.Decimal, decimal.Decimal, int32, bool) {
		return decimal.NewFromInt(-1 << (bits - 1)), decimal.NewFromInt(1<<(bits-1) - 1), 0, true
	}
	unsigned := func(max uint64) (decimal.Decimal, decimal.Decimal, int32, bool) {
		return decimal.Zero, decimal.NewFromUint64(max), 0, true
	}
	switch t.Type() {
	case sqltypes.Int8:
		return signed(8)
	case sqltypes.Int16:
		return signed(16)
	case sqltypes.Int24:
		return signed(24)
	case sqltypes.Int32:
		return signed(32)
	case sqltypes.Int64:
		return signed(64)
	case sqltypes.Uint8:
		return unsigned(math.MaxUint8)
	case sqltypes.Uint16:
		return unsigned(math.MaxUint16)
	case sqltypes.Uint24:
		return unsigned(1<<24 - 1)
	case sqltypes.Uint32:
		return unsigned(math.MaxUint32)
	case sqltypes.Uint64:
		return unsigned(math.MaxUint64)
	case sqltypes.Float32:
		hi = decimal.NewFromFloat(math.MaxFloat32) // float64 の値（NewFromFloat32 の短い字は float32 の端より少し大きい）
		return hi.Neg(), hi, 0, true
	case sqltypes.Float64:
		hi = decimal.NewFromFloat(math.MaxFloat64)
		return hi.Neg(), hi, 0, true
	}
	return lo, hi, 0, false
}

// numberValue は、数の列に入れる値 v の数としての値（文字列は先頭の数の部分）。
func numberValue(v any) (decimal.Decimal, bool) {
	switch v := v.(type) {
	case string:
		prefix, _ := sql.TruncateStringToDouble(v, false)
		if d, err := decimal.NewFromString(prefix); err == nil {
			return d, true
		}
		// 指数が大きすぎる（shopspring/decimal は 10000 まで）。double の端より外か、0 に近い値
		f, err := strconv.ParseFloat(prefix, 64)
		switch {
		case math.IsInf(f, 1):
			return decimal.New(1, 309), true
		case math.IsInf(f, -1):
			return decimal.New(-1, 309), true
		case err != nil && !errors.Is(err, strconv.ErrRange):
			return decimal.Decimal{}, false
		}
		return decimal.NewFromFloat(f), true
	case int8:
		return decimal.NewFromInt(int64(v)), true
	case int16:
		return decimal.NewFromInt(int64(v)), true
	case int32:
		return decimal.NewFromInt(int64(v)), true
	case int64:
		return decimal.NewFromInt(v), true
	case int:
		return decimal.NewFromInt(int64(v)), true
	case uint8:
		return decimal.NewFromUint64(uint64(v)), true
	case uint16:
		return decimal.NewFromUint64(uint64(v)), true
	case uint32:
		return decimal.NewFromUint64(uint64(v)), true
	case uint64:
		return decimal.NewFromUint64(v), true
	case uint:
		return decimal.NewFromUint64(uint64(v)), true
	case float32:
		return numberValue(float64(v))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return decimal.Decimal{}, false
		}
		return decimal.NewFromFloat(v), true
	case decimal.Decimal:
		return v, true
	case decimal.NullDecimal:
		return v.Decimal, v.Valid
	}
	return decimal.Decimal{}, false
}

// mayHaveMoreFraction は、値 v が小数の桁 scale より細かい桁を持ちうるか（DECIMAL の列への値ごとに丸めて比べる
// 前の、安い確かめ）。
func mayHaveMoreFraction(v any, scale int32) bool {
	switch v := v.(type) {
	case decimal.Decimal:
		return -v.Exponent() > scale
	case string:
		i := strings.IndexByte(v, '.')
		if i < 0 {
			return strings.ContainsAny(v, "eE")
		}
		frac := strings.TrimRight(strings.TrimRight(v[i+1:], sql.NumericCutSet), "0")
		return int32(len(frac)) > scale || strings.ContainsAny(frac, "eE")
	case float32, float64:
		return true
	}
	return false
}

// boundValue は、端の値 d を、型 t の Convert がそのまま受け付ける値にする。
func boundValue(t sql.Type, d decimal.Decimal) any {
	switch {
	case types.IsDecimal(t):
		return d
	case types.IsFloat(t):
		f, _ := d.Float64()
		return f
	case types.IsUnsigned(t):
		return d.BigInt().Uint64()
	}
	return d.IntPart()
}

// fixString は、文字列の列に入れる値 v が列の長さを超えれば、MySQL の非 STRICT と同じく切って警告 1265 にする
// （段階 92）。長さの数え方は go-mysql-server の ConvertToBytes と同じ: TEXT は文字の境目でバイトの上限まで、1 文字が
// 1 バイトの文字セット（binary など）はバイトの数、それ以外は文字の数。切ったのが末尾の空白類（改行なども含む）だけ
// なら、MySQL と同じく Note にする（binary の文字セットでは Warning、CHAR では警告なし）。文字列でない値は、go-mysql-server と同じ字にしてから切る。
func fixString(ctx *sql.Context, col *sql.Column, v any, rowNum int) (any, bool) {
	st, ok := col.Type.(sql.StringType)
	if !ok || v == nil || !types.IsText(col.Type) {
		return nil, false
	}
	var s string
	isBytes := false
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s, isBytes = string(x), true
	default:
		var err error
		if s, err = types.ConvertToString(ctx, v, types.LongText, nil); err != nil {
			return nil, false
		}
	}
	// utf8mb4 の列に不正な UTF-8 が来たら、MySQL と同じく最初の不正なバイトの手前で切って警告 1366 にする（段階 111、
	// 上流の #3527。前は INSERT IGNORE で空の値、WordPress の sql_mode で 1105 のエラーだった）
	cut := false
	if cs := st.CharacterSet(); (cs == sql.CharacterSet_utf8mb4 || cs == sql.CharacterSet_utf8mb3) && !utf8.ValidString(s) {
		i := firstInvalidUTF8(s)
		ctx.Warn(1366, "Incorrect string value: '%s' for column '%s' at row %d", printableBytes(s[i:]), col.Name, rowNum)
		s, cut = s[:i], true
	}
	var n int
	switch {
	case col.Type.Type() == sqltypes.Text:
		if int64(len(s)) <= st.MaxByteLength() {
			return truncatedString(s, isBytes, cut)
		}
		n = int(st.MaxByteLength())
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
	case st.CharacterSet().MaxLength() == 1:
		if int64(len(s)) <= st.MaxCharacterLength() {
			return truncatedString(s, isBytes, cut)
		}
		n = int(st.MaxCharacterLength())
	default:
		max := st.MaxCharacterLength()
		if int64(len(s)) <= max || int64(utf8.RuneCountInString(s)) <= max {
			return truncatedString(s, isBytes, cut)
		}
		for i := range s {
			if max == 0 {
				n = i
				break
			}
			max--
		}
	}
	t := s[:n]
	if col.Type.Type() == sqltypes.Char {
		// MySQL の CHAR は末尾の空白を持たない（読むときに除く）。切ったのが空白類だけなら警告も出さない
		t = strings.TrimRight(t, " ")
	}
	blank := st.CharacterSet() != sql.CharacterSet_binary && strings.Trim(s[n:], " \t\n\v\f\r") == ""
	switch {
	case blank && col.Type.Type() == sqltypes.Char:
	case blank:
		ctx.Session.Warn(&sql.Warning{Level: "Note", Code: 1265, Message: fmt.Sprintf("Data truncated for column '%s' at row %d", col.Name, rowNum)})
	default:
		ctx.Warn(1265, "Data truncated for column '%s' at row %d", col.Name, rowNum)
	}
	if isBytes {
		return []byte(t), true
	}
	return t, true
}

// truncatedString は、不正な UTF-8 を切った文字列 s（cut）を、元の値の型で返す。切っていなければ直さない。
func truncatedString(s string, isBytes, cut bool) (any, bool) {
	if !cut {
		return nil, false
	}
	if isBytes {
		return []byte(s), true
	}
	return s, true
}

// firstInvalidUTF8 は、s の最初の不正な UTF-8 のバイトの位置。
func firstInvalidUTF8(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return len(s)
}

// printableBytes は、警告 1366 に載せる値の形（MySQL の convert_to_printable と同じ）。6 バイトまでを、印字できる
// ASCII はそのまま、ほかは \xHH で書き、残りがあれば ... を付ける。
func printableBytes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && i < 6; i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "\\x%02X", c)
		}
	}
	if len(s) > 6 {
		b.WriteString("...")
	}
	return b.String()
}

// startsWithDigit は、前後の空白を除いた s が、符号と数字で始まるか（'0abc' は数で始まる）。
func startsWithDigit(s string) bool {
	s = strings.TrimLeft(strings.Trim(s, sql.NumericCutSet), "+-")
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// lenientDatetime は、MySQL が受け付ける、時刻が途中までの日時（'2012-01-01 0'、'2012-01-01T10:5' など）。
var lenientDatetime = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})(?:[ T](\d{1,2})(?::(\d{1,2})(?::(\d{1,2})(\.\d{1,6})?)?)?)?$`)

// fixDatetime は、日時の列に入れる文字列 v が go-mysql-server で変換できなければ、MySQL の非 STRICT と同じく直した
// 値を返す（ok が true）。時刻が途中までの形は正しい値に、型の範囲の外はゼロの日時と警告 1264 に、それ以外は
// ゼロの日時と警告 1265 にする。
func fixDatetime(ctx *sql.Context, col *sql.Column, v any, rowNum int) (any, bool) {
	if !types.IsTime(col.Type) {
		return nil, false
	}
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		if types.IsNumberValue(v) {
			// 日時として読めない数（1、20241301）は、MySQL の非 STRICT と同じくゼロの日時と警告 1265（段階 116。
			// 20240101 のような数は go-mysql-server がそのまま読む）
			if _, _, err := col.Type.Convert(ctx, v); err != nil && types.ErrConvertingToTime.Is(err) {
				ctx.Warn(1265, "Data truncated for column '%s' at row %d", col.Name, rowNum)
				return col.Type.Zero(), true
			}
		}
		return nil, false
	}
	cv, _, err := col.Type.Convert(ctx, s)
	if err == nil {
		return nil, false
	}
	if types.TruncatedDatetime(err) && cv != nil {
		// 後ろに余計な文字のある '2020-01-01 a' は、MySQL と同じく前の部分（2020-01-01）と警告 1265（上流の #3276）
		ctx.Warn(1265, "Data truncated for column '%s' at row %d", col.Name, rowNum)
		return cv, true
	}
	if t, ok := parseLenientDatetime(s); ok {
		if _, _, err := col.Type.Convert(ctx, t); err == nil {
			return t, true
		}
		// 読めるが型の範囲の外（TIMESTAMP の 1970 年より前など）。
		ctx.Warn(1264, "Out of range value for column '%s' at row %d", col.Name, rowNum)
		return col.Type.Zero(), true
	}
	ctx.Warn(1265, "Data truncated for column '%s' at row %d", col.Name, rowNum)
	return col.Type.Zero(), true
}

// parseLenientDatetime は、lenientDatetime の形の文字列を、範囲を確かめて "YYYY-MM-DD HH:MM:SS[.ffffff]" にする。
func parseLenientDatetime(s string) (string, bool) {
	m := lenientDatetime.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	num := func(i int) int {
		if m[i] == "" {
			return 0
		}
		n, _ := strconv.Atoi(m[i])
		return n
	}
	y, mo, d, h, mi, sec := num(1), num(2), num(3), num(4), num(5), num(6)
	if mo < 1 || mo > 12 || d < 1 || h > 23 || mi > 59 || sec > 59 {
		return "", false
	}
	if t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC); t.Day() != d {
		return "", false // 2 月 30 日など
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d%s", y, mo, d, h, mi, sec, m[7]), true
}
