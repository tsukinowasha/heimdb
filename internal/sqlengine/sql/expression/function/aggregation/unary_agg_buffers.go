// Modified by Tsukinowa Inc.: COUNT(DISTINCT) で、照合順序で等しい値を一つと数える（docs/engine.md）
// Modified by Tsukinowa Inc.: FIRST の値を最初の行の NULL の後の行で上書きしない（上流の #3030。docs/engine.md）
// Modified by Tsukinowa Inc.: 整数と DECIMAL の SUM と AVG を DECIMAL で丸めずに計算する（SumType、AvgType。docs/engine.md）
// Modified by Tsukinowa Inc.: 数にならない文字列の SUM と AVG は先頭の数の部分を足す（段階 116 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: TEXT の列の SUM と AVG で警告 1292 を出さない（段階 118。docs/engine.md）
// Modified by Tsukinowa Inc.: 数でない文字列の VARIANCE と STDDEV を先頭の数の部分で計算し、警告の文を直す（段階 119。docs/engine.md）
// Modified by Tsukinowa Inc.: 日付、日時、TIME の VARIANCE と STDDEV の型を DOUBLE にする（VarianceType。段階 119 のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の SUM と AVG の型を、MySQL と同じ D 桁と D+4 桁にする（段階 120。docs/engine.md）

package aggregation

import (
	"fmt"
	"math"
	"reflect"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"
	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

type anyValueBuffer struct {
	res  interface{}
	expr sql.Expression
}

func NewAnyValueBuffer(child sql.Expression) *anyValueBuffer {
	return &anyValueBuffer{nil, child}
}

// Update implements the AggregationBuffer interface.
func (a *anyValueBuffer) Update(ctx *sql.Context, row sql.Row) error {
	if a.res != nil {
		return nil
	}

	v, err := a.expr.Eval(ctx, row)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}

	a.res = v

	return nil
}

// Eval implements the AggregationBuffer interface.
func (a *anyValueBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return a.res, nil
}

// Dispose implements the Disposable interface.
func (a *anyValueBuffer) Dispose() {
	expression.Dispose(a.expr)
}

type sumBuffer struct {
	isnil bool
	sum   interface{} // sum is either decimal.Decimal or float64
	expr  sql.Expression

	// exact は、結果が DECIMAL の SUM と AVG（引数が整数か DECIMAL。SumType）。整数と DECIMAL を丸めずに足す
	// （HeimDB。前はどの整数も float64 で足し、2^53 を超える値と DECIMAL(26,4) の桁を失った）。
	exact bool
	isum  int64           // exact の整数の値の和（溢れたら dsum に移す）
	dsum  decimal.Decimal // exact のそれ以外の値の和
	scale int32           // exact の結果の小数の桁（表の DECIMAL の値は後ろの 0 を持たないので、型の桁で表す）
}

func NewSumBuffer(child sql.Expression) *sumBuffer {
	b := &sumBuffer{isnil: true, sum: float64(0), expr: child, dsum: decimal.Zero}
	if dt, ok := SumType(child.Type()).(sql.DecimalType); ok {
		b.exact, b.scale = true, int32(dt.Scale())
	}
	return b
}

// SumType は、MySQL の SUM の結果の型（HeimDB）。整数（YEAR、BIT も）と DECIMAL の引数は、桁を 22 足した
// DECIMAL（65 まで）で小数の桁は引数のもの。それ以外（浮動小数点数、文字列、日時）は DOUBLE。
// VarianceType は、VARIANCE、STDDEV とその仲間の結果の型（HeimDB: 段階 119 のレビュー）。日付、日時、TIME の引数は、
// MySQL と同じく数の形（DoubleArg）で計算した DOUBLE にする（引数の型のままだと、TIME の値にならない分散が 1105 で
// 文全体を誤りにする）。ほかの型は前と同じく引数の型（整数と DECIMAL も MySQL は DOUBLE。docs/differences.md）。
func VarianceType(t sql.Type) sql.Type {
	if types.IsTime(t) || types.IsTimespan(t) || types.IsYear(t) {
		return types.Float64
	}
	return t
}

func SumType(t sql.Type) sql.Type {
	if scale, ok := types.RealScale(t); ok {
		return types.RealResultType(scale) // HeimDB: FLOAT(M,D) と DOUBLE(M,D) は D 桁（MySQL と同じ。段階 120）
	}
	p, s, ok := DecimalPrecision(t)
	if !ok {
		return types.Float64
	}
	return types.MustCreateDecimalType(min(p+22, types.DecimalTypeMaxPrecision), s)
}

// AvgType は、MySQL の AVG の結果の型（HeimDB）。整数と DECIMAL の引数は、桁と小数の桁を 4 つ
// （div_precision_increment）足した DECIMAL（65 と 30 まで）。それ以外は DOUBLE。
func AvgType(t sql.Type) sql.Type {
	if scale, ok := types.RealScale(t); ok {
		return types.RealResultType(scale + 4) // HeimDB: FLOAT(M,D) と DOUBLE(M,D) は D+4 桁（MySQL と同じ。段階 120）
	}
	p, s, ok := DecimalPrecision(t)
	if !ok {
		return types.Float64
	}
	return types.MustCreateDecimalType(min(p+4, types.DecimalTypeMaxPrecision), min(s+4, types.DecimalTypeMaxScale))
}

// DecimalPrecision は、整数か DECIMAL の型の、MySQL の decimal_precision() と小数の桁を返す。
func DecimalPrecision(t sql.Type) (precision, scale uint8, ok bool) {
	if dt, isDec := t.(sql.DecimalType); isDec && types.IsDecimal(t) {
		return dt.Precision(), dt.Scale(), true
	}
	if bt, isBit := t.(types.BitType); isBit {
		return bt.NumberOfBits(), 0, true
	}
	if types.IsYear(t) {
		return 4, 0, true
	}
	if !types.IsInteger(t) {
		return 0, 0, false
	}
	switch t.Type() {
	case sqltypes.Int8, sqltypes.Uint8:
		return 3, 0, true
	case sqltypes.Int16, sqltypes.Uint16:
		return 5, 0, true
	case sqltypes.Int24, sqltypes.Uint24:
		return 8, 0, true
	case sqltypes.Int32, sqltypes.Uint32:
		return 10, 0, true
	case sqltypes.Uint64:
		return 20, 0, true
	default:
		return 19, 0, true
	}
}

// Update implements the AggregationBuffer interface.
func (m *sumBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := m.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		return nil
	}

	m.PerformSum(ctx, v)

	return nil
}

func (m *sumBuffer) PerformSum(ctx *sql.Context, v interface{}) {
	if m.exact {
		m.isnil = false
		m.addExact(ctx, v)
		return
	}
	// decimal.Decimal values are evaluated to string value even though the Literal expr type is Decimal type,
	// so convert it to appropriate Decimal type
	if s, isStr := v.(string); isStr && types.IsDecimal(m.expr.Type()) {
		val, _, err := m.expr.Type().Convert(ctx, s)
		if err == nil {
			v = val
		}
	}

	switch n := v.(type) {
	case decimal.Decimal:
		if m.isnil {
			m.sum = decimal.NewFromInt(0)
			m.isnil = false
		}
		if sum, ok := m.sum.(decimal.Decimal); ok {
			m.sum = sum.Add(n)
		} else {
			m.sum = decimal.NewFromFloat(m.sum.(float64)).Add(n)
		}
	default:
		val, _, err := types.Float64.Convert(ctx, n)
		if err != nil {
			if types.TruncatedString(n, err) && val != nil {
				// HeimDB: a string that is not a number adds its leading number, as
				// MySQL does ('1704103200:1' of _edit_lock is 1704103200, '1e400' the
				// largest DOUBLE; it was 0; stage 116 review). A TEXT column gives no
				// warning, as in MySQL (stage 118)
				if !expression.IsTextColumn(m.expr) {
					ctx.Warn(1292, "Truncated incorrect DOUBLE value: '%v'", n)
				}
			} else {
				val = float64(0)
			}
		}
		if m.isnil {
			m.sum = float64(0)
			m.isnil = false
		}
		sum, _, err := types.Float64.Convert(ctx, m.sum)
		if err != nil {
			sum = float64(0)
		}
		m.sum = sum.(float64) + val.(float64)
	}
}

// addExact は、整数を int64 のまま（溢れるときは decimal で）、それ以外を decimal で足す。
func (m *sumBuffer) addExact(ctx *sql.Context, v interface{}) {
	var i int64
	switch n := v.(type) {
	case decimal.Decimal:
		m.dsum = m.dsum.Add(n)
		return
	case int64:
		i = n
	case int32:
		i = int64(n)
	case int16:
		i = int64(n)
	case int8:
		i = int64(n)
	case int:
		i = int64(n)
	case uint32:
		i = int64(n)
	case uint16:
		i = int64(n)
	case uint8:
		i = int64(n)
	case uint64:
		if n > math.MaxInt64 {
			m.dsum = m.dsum.Add(decimal.NewFromUint64(n))
			return
		}
		i = int64(n)
	case bool:
		if n {
			i = 1
		}
	default:
		d, _, err := types.InternalDecimalType.Convert(ctx, v)
		if dec, ok := d.(decimal.Decimal); ok && err == nil {
			m.dsum = m.dsum.Add(dec)
		}
		return
	}
	if s := m.isum + i; (i > 0 && s < m.isum) || (i < 0 && s > m.isum) {
		m.dsum = m.dsum.Add(decimal.NewFromInt(m.isum)).Add(decimal.NewFromInt(i))
		m.isum = 0
	} else {
		m.isum = s
	}
}

// Eval implements the AggregationBuffer interface.
func (m *sumBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	if m.isnil {
		return nil, nil
	}
	if m.exact {
		return m.dsum.Add(decimal.NewFromInt(m.isum)).Round(m.scale), nil
	}
	return m.sum, nil
}

// Dispose implements the Disposable interface.
func (m *sumBuffer) Dispose() {
	expression.Dispose(m.expr)
}

type lastBuffer struct {
	val  interface{}
	expr sql.Expression
}

func NewLastBuffer(child sql.Expression) *lastBuffer {
	const (
		sum  = float64(0)
		rows = int64(0)
	)

	return &lastBuffer{nil, child}
}

// Update implements the AggregationBuffer interface.
func (l *lastBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := l.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		return nil
	}

	l.val = v

	return nil
}

// Eval implements the AggregationBuffer interface.
func (l *lastBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return l.val, nil
}

// Dispose implements the Disposable interface.
func (l *lastBuffer) Dispose() {
	expression.Dispose(l.expr)
}

type avgBuffer struct {
	sum   *sumBuffer // sum is either decimal.Decimal or float64
	rows  int64
	expr  sql.Expression
	scale int32 // 結果が DECIMAL のときの小数の桁（AvgType）
}

func NewAvgBuffer(child sql.Expression) *avgBuffer {
	const (
		rows = int64(0)
	)

	scale := int32(4)
	if dt, ok := AvgType(child.Type()).(sql.DecimalType); ok {
		scale = int32(dt.Scale())
	}
	return &avgBuffer{NewSumBuffer(child), rows, child, scale}
}

// Update implements the AggregationBuffer interface.
func (a *avgBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := a.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		return nil
	}

	a.sum.PerformSum(ctx, v)
	a.rows += 1

	return nil
}

// Eval implements the AggregationBuffer interface.
func (a *avgBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	sum, err := a.sum.Eval(ctx)
	if err != nil {
		return nil, err
	}
	// This case is triggered when no rows exist.
	switch s := sum.(type) {
	case float64:
		if s == 0 && a.rows == 0 {
			return nil, nil
		}

		if a.rows == 0 {
			return float64(0), nil
		}

		return s / float64(a.rows), nil
	case decimal.Decimal:
		if s.IsZero() && a.rows == 0 {
			return nil, nil
		}
		if a.rows == 0 {
			return decimal.NewFromInt(0), nil
		}
		if a.sum.exact {
			return s.DivRound(decimal.NewFromInt(a.rows), a.scale), nil
		}
		scale := (s.Exponent() * -1) + 4
		return s.DivRound(decimal.NewFromInt(a.rows), scale), nil
	}
	return nil, nil
}

// Dispose implements the Disposable interface.
func (a *avgBuffer) Dispose() {
	expression.Dispose(a.expr)
}

type bitAndBuffer struct {
	res  uint64
	rows uint64
	expr sql.Expression
}

func NewBitAndBuffer(child sql.Expression) *bitAndBuffer {
	const (
		res  = ^uint64(0) // bitwise not xor, so 0xffff...
		rows = uint64(0)
	)

	return &bitAndBuffer{res, rows, child}
}

// Update implements the AggregationBuffer interface.
func (b *bitAndBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := b.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		return nil
	}

	v, _, err = types.Uint64.Convert(ctx, v)
	if err != nil {
		v = uint64(0)
	}

	b.res &= v.(uint64)
	b.rows += 1

	return nil
}

// Eval implements the AggregationBuffer interface.
func (b *bitAndBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return b.res, nil
}

// Dispose implements the Disposable interface.
func (b *bitAndBuffer) Dispose() {
	expression.Dispose(b.expr)
}

type bitOrBuffer struct {
	res  uint64
	rows uint64
	expr sql.Expression
}

func NewBitOrBuffer(child sql.Expression) *bitOrBuffer {
	const (
		res  = uint64(0)
		rows = uint64(0)
	)

	return &bitOrBuffer{res, rows, child}
}

// Update implements the AggregationBuffer interface.
func (b *bitOrBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := b.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		return nil
	}

	v, _, err = types.Uint64.Convert(ctx, v)
	if err != nil {
		v = uint64(0)
	}

	b.res |= v.(uint64)
	b.rows += 1

	return nil
}

// Eval implements the AggregationBuffer interface.
func (b *bitOrBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return b.res, nil
}

// Dispose implements the Disposable interface.
func (b *bitOrBuffer) Dispose() {
	expression.Dispose(b.expr)
}

type bitXorBuffer struct {
	res  uint64
	rows uint64
	expr sql.Expression
}

func NewBitXorBuffer(child sql.Expression) *bitXorBuffer {
	const (
		res  = uint64(0)
		rows = uint64(0)
	)

	return &bitXorBuffer{res, rows, child}
}

// Update implements the AggregationBuffer interface.
func (b *bitXorBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := b.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		return nil
	}

	v, _, err = types.Uint64.Convert(ctx, v)
	if err != nil {
		v = uint64(0)
	}

	b.res ^= v.(uint64)
	b.rows += 1

	return nil
}

// Eval implements the AggregationBuffer interface.
func (b *bitXorBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	// This case is triggered when no rows exist.
	if b.res == 0 && b.rows == 0 {
		return uint64(0), nil
	}

	if b.rows == 0 {
		return uint64(0), nil
	}

	return b.res, nil
}

// Dispose implements the Disposable interface.
func (b *bitXorBuffer) Dispose() {
	expression.Dispose(b.expr)
}

type countDistinctBuffer struct {
	seen  map[uint64]struct{}
	exprs []sql.Expression
}

func NewCountDistinctBuffer(children []sql.Expression) *countDistinctBuffer {
	return &countDistinctBuffer{make(map[uint64]struct{}), children}
}

// Update implements the AggregationBuffer interface.
func (c *countDistinctBuffer) Update(ctx *sql.Context, row sql.Row) error {
	var value interface{}
	if len(c.exprs) == 0 {
		return fmt.Errorf("no expressions")
	}
	_, star := c.exprs[0].(*expression.Star)
	if star {
		value = row
	} else {
		val := make(sql.Row, len(c.exprs))
		for i, expr := range c.exprs {
			v, err := expr.Eval(ctx, row)
			if err != nil {
				return err
			}
			// skip nil values
			if v == nil {
				return nil
			}
			val[i] = v
		}
		value = val
	}

	// HeimDB: 文字列の式の値は、式の照合順序の重みでハッシュする（MySQL と同じく、'a' と 'A' を一つと数える）。
	// 値の区切りは NUL にする（もとの「,」では ('a,b', 'c') と ('a', 'b,c') が同じになった）。
	hash := xxhash.New()
	for i, val := range value.(sql.Row) {
		// skip nil values
		if val == nil {
			return nil
		}
		if i > 0 {
			hash.Write([]byte{0})
		}
		if !star && i < len(c.exprs) {
			if st, ok := c.exprs[i].Type().(sql.StringType); ok && st.Collation() != sql.Collation_binary {
				if vs, err := types.ConvertToString(ctx, val, st, nil); err == nil && utf8.ValidString(vs) {
					hash.Write([]byte{1})
					if err := st.Collation().WriteWeightString(hash, vs); err != nil {
						return err
					}
					continue
				}
			}
		}
		v, _, err := types.Text.Convert(ctx, val)
		if err != nil {
			return err
		}
		vv, ok := v.(string)
		if !ok {
			return fmt.Errorf("count distinct unable to hash value: %s", err)
		}
		hash.WriteString(vv)
	}
	h := hash.Sum64()
	c.seen[h] = struct{}{}

	return nil
}

// Eval implements the AggregationBuffer interface.
func (c *countDistinctBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return int64(len(c.seen)), nil
}

func (c *countDistinctBuffer) Dispose() {
	for _, e := range c.exprs {
		expression.Dispose(e)
	}
}

type countBuffer struct {
	cnt  int64
	expr sql.Expression
}

func NewCountBuffer(child sql.Expression) *countBuffer {
	return &countBuffer{0, child}
}

// Update implements the AggregationBuffer interface.
func (c *countBuffer) Update(ctx *sql.Context, row sql.Row) error {
	var inc bool
	if _, ok := c.expr.(*expression.Star); ok {
		inc = true
	} else {
		v, err := c.expr.Eval(ctx, row)
		if v != nil {
			inc = true
		}

		if err != nil {
			return err
		}
	}

	if inc {
		c.cnt += 1
	}

	return nil
}

// Eval implements the AggregationBuffer interface.
func (c *countBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return c.cnt, nil
}

// Dispose implements the Disposable interface.
func (c *countBuffer) Dispose() {
	expression.Dispose(c.expr)
}

type firstBuffer struct {
	val interface{}
	// writtenNil は、最初の行の値が NULL で、後の行の値で上書きしないことを表す
	writtenNil bool
	expr       sql.Expression
}

func NewFirstBuffer(child sql.Expression) *firstBuffer {
	return &firstBuffer{nil, false, child}
}

// Update implements the AggregationBuffer interface.
func (f *firstBuffer) Update(ctx *sql.Context, row sql.Row) error {
	if f.val != nil || f.writtenNil {
		return nil
	}

	v, err := f.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if v == nil {
		f.writtenNil = true
		return nil
	}

	f.val = v

	return nil
}

// Eval implements the AggregationBuffer interface.
func (f *firstBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return f.val, nil
}

// Dispose implements the Disposable interface.
func (f *firstBuffer) Dispose() {
	expression.Dispose(f.expr)
}

type maxBuffer struct {
	val  interface{}
	expr sql.Expression
}

func NewMaxBuffer(child sql.Expression) *maxBuffer {
	return &maxBuffer{nil, child}
}

// Update implements the AggregationBuffer interface.
func (m *maxBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := m.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if reflect.TypeOf(v) == nil {
		return nil
	}

	if m.val == nil {
		m.val = v
		return nil
	}

	cmp, err := m.expr.Type().Compare(ctx, v, m.val)
	if err != nil {
		return err
	}
	if cmp == 1 {
		m.val = v
	}

	return nil
}

// Eval implements the AggregationBuffer interface.
func (m *maxBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return m.val, nil
}

// Dispose implements the Disposable interface.
func (m *maxBuffer) Dispose() {
	expression.Dispose(m.expr)
}

type minBuffer struct {
	val  interface{}
	expr sql.Expression
}

func NewMinBuffer(child sql.Expression) *minBuffer {
	return &minBuffer{nil, child}
}

// Update implements the AggregationBuffer interface.
func (m *minBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := m.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	if reflect.TypeOf(v) == nil {
		return nil
	}

	if m.val == nil {
		m.val = v
		return nil
	}

	cmp, err := m.expr.Type().Compare(ctx, v, m.val)
	if err != nil {
		return err
	}
	if cmp == -1 {
		m.val = v
	}

	return nil
}

// Eval implements the AggregationBuffer interface.
func (m *minBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return m.val, nil
}

// Dispose implements the Disposable interface.
func (m *minBuffer) Dispose() {
	expression.Dispose(m.expr)
}

type jsonArrayBuffer struct {
	vals []interface{}
	expr sql.Expression
}

func NewJsonArrayBuffer(child sql.Expression) *jsonArrayBuffer {
	return &jsonArrayBuffer{nil, child}
}

// Update implements the AggregationBuffer interface.
func (j *jsonArrayBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := j.expr.Eval(ctx, row)
	if err != nil {
		return err
	}

	// unwrap JSON values
	if js, ok := v.(sql.JSONWrapper); ok {
		v, err = js.ToInterface()
		if err != nil {
			return err
		}
	}

	j.vals = append(j.vals, v)

	return nil
}

// Eval implements the AggregationBuffer interface.
func (j *jsonArrayBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	return types.JSONDocument{Val: j.vals}, nil
}

// Dispose implements the Disposable interface.
func (j *jsonArrayBuffer) Dispose() {
}

type varBaseBuffer struct {
	vals []interface{}
	expr sql.Expression

	count uint64
	mean  float64
	std2  float64
}

// Update implements the AggregationBuffer interface.
func (vb *varBaseBuffer) Update(ctx *sql.Context, row sql.Row) error {
	v, err := vb.expr.Eval(ctx, row)
	if err != nil {
		return err
	}
	// HeimDB: a string that is not a number is its leading number with warning
	// 1292 (none for a TEXT column), as in MySQL and SUM (stage 119; it was 0, and
	// the warning was written as %!s(float64=0))
	orig := v
	v, _, err = expression.DoubleArg(ctx, vb.expr, v)
	if err != nil {
		v = 0.0
		ctx.Warn(1292, "Truncated incorrect DOUBLE value: '%v'", orig)
	}
	if v == nil {
		return nil
	}
	val := v.(float64)

	vb.count += 1
	if vb.count == 1 {
		vb.mean = val
		return nil
	}

	newMean := vb.mean + (val-vb.mean)/float64(vb.count)
	vb.std2 = vb.std2 + (val-vb.mean)*(val-newMean)
	vb.mean = newMean

	return nil
}

// Dispose implements the Disposable interface.
func (vb *varBaseBuffer) Dispose() {}

type stdDevPopBuffer struct {
	varBaseBuffer
}

func NewStdDevPopBuffer(child sql.Expression) *stdDevPopBuffer {
	return &stdDevPopBuffer{
		varBaseBuffer: varBaseBuffer{
			expr: child,
		},
	}
}

// Eval implements the AggregationBuffer interface.
func (s *stdDevPopBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	if s.count == 0 {
		return nil, nil
	}
	return math.Sqrt(s.std2 / float64(s.count)), nil
}

type stdDevSampBuffer struct {
	varBaseBuffer
}

func NewStdDevSampBuffer(child sql.Expression) *stdDevSampBuffer {
	return &stdDevSampBuffer{
		varBaseBuffer: varBaseBuffer{
			expr: child,
		},
	}
}

// Eval implements the AggregationBuffer interface.
func (s *stdDevSampBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	if s.count <= 1 {
		return nil, nil
	}
	return math.Sqrt(s.std2 / float64(s.count-1)), nil
}

type varPopBuffer struct {
	varBaseBuffer
}

func NewVarPopBuffer(child sql.Expression) *varPopBuffer {
	return &varPopBuffer{
		varBaseBuffer: varBaseBuffer{
			expr: child,
		},
	}
}

// Eval implements the AggregationBuffer interface.
func (vp *varPopBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	if vp.count == 0 {
		return nil, nil
	}
	return vp.std2 / float64(vp.count), nil
}

type varSampBuffer struct {
	varBaseBuffer
}

func NewVarSampBuffer(child sql.Expression) *varSampBuffer {
	return &varSampBuffer{
		varBaseBuffer: varBaseBuffer{
			expr: child,
		},
	}
}

// Eval implements the AggregationBuffer interface.
func (vp *varSampBuffer) Eval(ctx *sql.Context) (interface{}, error) {
	if vp.count <= 1 {
		return nil, nil
	}
	return vp.std2 / float64(vp.count-1), nil
}
