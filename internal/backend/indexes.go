package backend

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// keyIndex は、キーを持つセカンダリインデックス（列がすべてキーにできる型のもの）。
type keyIndex struct {
	def  schema.Index
	ords []int
	cols []rowenc.IndexColumn
	// hasPrefix は、プレフィックス長のある列を含むか。含むとキーの順序が値の順序と一致しない。
	hasPrefix bool
	// cover は、エントリの値に元の値を持つ列（インデックスの列と、含まれていない主キーの列。段階 27）。
	// 文がこの列だけを使うなら、本体の行を引かずにエントリだけで読める。
	cover []int
}

// keyIndexes は、テーブルのセカンダリインデックスのうちキーを持つものを返す。
func (t *Table) keyIndexes() []keyIndex {
	return t.keys
}

// keyIndex は、インデックス def がキーを持てるなら、その情報を返す。持てるかは列の型と定義のキーの水準で決まる
// （docs/plan.md の「キー設計」と段階 117。水準 0 の DECIMAL などのインデックスはキーを持たず、全件を読む）。
func (t *Table) keyIndex(def schema.Index) (keyIndex, bool) {
	if def.Fulltext {
		return keyIndex{}, false // FULLTEXT は定義だけでエントリを持たない
	}
	ki := keyIndex{def: def}
	for _, c := range def.Columns {
		ord := t.def.ColumnIndex(c.Name)
		typ := t.sch.Schema[ord].Type
		if !rowenc.KeyableType(typ, def.KeyLevel) {
			return keyIndex{}, false
		}
		ki.ords = append(ki.ords, ord)
		ki.cols = append(ki.cols, rowenc.IndexColumn{Type: typ, Prefix: c.Length})
		ki.hasPrefix = ki.hasPrefix || c.Length > 0
	}
	ki.cover = append([]int(nil), ki.ords...)
	for _, ord := range t.def.PKOrdinals {
		if !slices.Contains(ki.cover, ord) {
			ki.cover = append(ki.cover, ord)
		}
	}
	return ki, true
}

// findKeyIndex は名前でキーを持つインデックスを探す。
// 表を作るときに作った t.keys から探す（lookup JOIN は左の行ごとに探す。docs/plan.md の段階 102）。
func (t *Table) findKeyIndex(name string) (keyIndex, bool) {
	for _, ki := range t.keys {
		if strings.EqualFold(ki.def.Name, name) {
			return ki, true
		}
	}
	return keyIndex{}, false
}

// entry は、行 row のインデックスのエントリのキーの部分（列の値と主キー）と、エントリの値（ki.cover の列の元の値）を
// 符号化する。キーの文字列は照合順序の重みで元の値に戻せないので、値に元の値を持たせる（docs/plan.md の段階 27）。
func (t *Table) entry(ki keyIndex, row sql.Row) (key, value []byte, err error) {
	for i, ord := range ki.ords {
		if key, _, err = rowenc.AppendIndexValue(key, ki.cols[i], row[ord]); err != nil {
			return nil, nil, fmt.Errorf("HeimDB: index %s: %w", ki.def.Name, err)
		}
	}
	if key, err = rowenc.EncodePK(key, t.sch, row); err != nil {
		return nil, nil, err
	}
	vals := make([]any, len(ki.cover))
	for i, ord := range ki.cover {
		vals[i] = row[ord]
	}
	if value, err = rowenc.EncodeValues(vals); err != nil {
		return nil, nil, fmt.Errorf("HeimDB: index %s: %w", ki.def.Name, err)
	}
	return key, value, nil
}

// pkOfEntry は、インデックス ki のエントリの末尾の主キーを返す。整数の主キーは 1 列 9 バイトの固定長なので後ろから
// 切る。整数でない列を含む主キー（docs/plan.md の段階 38、70）は、インデックスの列を前から読み飛ばした残りにする
// （文字列とバイト列は長さが決まらず、後ろからは前の列のバイトと区別できない。日時は固定長だが、分けても速さの
// 差をテストで捕まえられないので同じ扱いにする）。
// entriesDecoded は、主キーを取り出したインデックスのエントリの数（テスト用。docs/plan.md の段階 55）。
var entriesDecoded atomic.Int64

func (t *Table) pkOfEntry(ki keyIndex, entry []byte) ([]byte, error) {
	entriesDecoded.Add(1)
	if !t.strPK {
		n := 9 * len(t.def.PKOrdinals)
		if len(entry) < n {
			return nil, fmt.Errorf("HeimDB: index %s: corrupt entry", ki.def.Name)
		}
		return entry[len(entry)-n:], nil
	}
	rest := entry
	for _, col := range ki.cols {
		var err error
		if rest, err = rowenc.SkipIndexValue(col, rest); err != nil {
			return nil, fmt.Errorf("HeimDB: index %s: %w", ki.def.Name, err)
		}
	}
	return rest, nil
}

// valuePrefix は、インデックスの列の値だけを符号化する（主キーを含まない）。一意制約の検査に使う。
func (t *Table) valuePrefix(ki keyIndex, row sql.Row) ([]byte, error) {
	var b []byte
	for i, ord := range ki.ords {
		var err error
		if b, _, err = rowenc.AppendIndexValue(b, ki.cols[i], row[ord]); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// indexInterval は、インデックスの列に対する範囲 r を、エントリの区間 [lo, hi) に変換する。
// 先頭から等号の列が続く間は値をつなぎ、最初の等号でない列で区間を決める。変換できない端（NULL の側、
// 型の変換に失敗した値など）は広げる。プレフィックス長で切った値は等号でも区間の端でも含む側に広げる。
// 広げすぎた分は、呼び出し側が範囲の条件で行を絞って落とす。
func indexInterval(ctx *sql.Context, cols []*sql.Column, encode func(i int, v any) ([]byte, bool, error), r sql.MySQLRange) (lo, hi []byte) {
	cutKey := func(i int, cut sql.MySQLRangeCut) (key []byte, truncated, ok bool) {
		if !sql.MySQLRangeCutIsBinding(cut) {
			return nil, false, false
		}
		v, ok := convertKey(ctx, cols[i].Type, sql.GetMySQLRangeCutKey(cut))
		if !ok {
			return nil, false, false
		}
		k, tr, err := encode(i, v)
		if err != nil {
			return nil, false, false
		}
		return k, tr, true
	}
	var prefix []byte
	for i := range cols {
		if i >= len(r) {
			break
		}
		ce := r[i]
		lower, lowerTr, lowerOK := cutKey(i, ce.LowerBound)
		upper, upperTr, upperOK := cutKey(i, ce.UpperBound)
		_, lowerIncl := ce.LowerBound.(sql.Below)
		_, upperIncl := ce.UpperBound.(sql.Above)
		lowerIncl = lowerIncl || lowerTr
		upperIncl = upperIncl || upperTr
		if lowerOK && upperOK && lowerIncl && upperIncl && string(lower) == string(upper) {
			prefix = append(prefix, lower...)
			continue
		}
		lo, hi = append([]byte(nil), prefix...), keyenc.PrefixEnd(prefix)
		if lowerOK {
			if lowerIncl {
				lo = append(append([]byte(nil), prefix...), lower...)
			} else {
				lo = keyenc.PrefixEnd(append(append([]byte(nil), prefix...), lower...))
			}
		}
		if upperOK {
			if upperIncl {
				hi = keyenc.PrefixEnd(append(append([]byte(nil), prefix...), upper...))
			} else {
				hi = append(append([]byte(nil), prefix...), upper...)
			}
		}
		return lo, hi
	}
	return prefix, keyenc.PrefixEnd(prefix)
}

// exactIntervals は、列 cols のインデックスか主キーの区間（indexInterval）が範囲 ranges をちょうど表すか（広げた分が
// ないか）を返す。prefix は、インデックスに前方の長さの列があるか（あれば false）。ちょうどなら、区間で読んだ行を
// 範囲の条件で絞り直さなくてよい。各範囲が、先頭からの等号の列、範囲のある列
// 1 つ（なくてもよい）、残りはすべての値（NULL を含む）の形で、端の値が列の型にそのまま入る（丸めも切り詰めも
// ない）ときだけ true。端の値は、整数の列は整数、文字列の列は文字列、日時の列は丸めなしで入るものだけ見る。
func exactIntervals(ctx *sql.Context, prefix bool, cols []*sql.Column, ranges sql.MySQLRangeCollection) bool {
	if prefix {
		return false
	}
	exactCut := func(i int, cut sql.MySQLRangeCut) bool {
		if !sql.MySQLRangeCutIsBinding(cut) {
			return false
		}
		return exactKey(ctx, cols[i].Type, sql.GetMySQLRangeCutKey(cut))
	}
	for _, r := range ranges {
		if len(r) > len(cols) {
			return false
		}
		i := 0
		for ; i < len(r); i++ {
			ce := r[i]
			_, lowerIncl := ce.LowerBound.(sql.Below)
			_, upperIncl := ce.UpperBound.(sql.Above)
			if !lowerIncl || !upperIncl || !exactCut(i, ce.LowerBound) || !exactCut(i, ce.UpperBound) {
				break
			}
			lo, hi := sql.GetMySQLRangeCutKey(ce.LowerBound), sql.GetMySQLRangeCutKey(ce.UpperBound)
			if c, err := cols[i].Type.Compare(ctx, lo, hi); err != nil || c != 0 {
				break
			}
		}
		if i == len(r) {
			continue
		}
		ce := r[i]
		if _, ok := ce.LowerBound.(sql.BelowNull); !ok && !exactCut(i, ce.LowerBound) {
			return false
		}
		if _, ok := ce.UpperBound.(sql.AboveAll); !ok && !exactCut(i, ce.UpperBound) {
			return false
		}
		for _, rest := range r[i+1:] {
			_, lowerAll := rest.LowerBound.(sql.BelowNull)
			_, upperAll := rest.UpperBound.(sql.AboveAll)
			if !lowerAll || !upperAll {
				return false
			}
		}
	}
	return true
}

// exactKey は、範囲の端の値 v が、列の型 typ にそのまま（丸めも切り詰めもなく）入るか。
func exactKey(ctx *sql.Context, typ sql.Type, v any) bool {
	if !exactKeyKind(typ, v) {
		return false
	}
	_, ok := convertKey(ctx, typ, v)
	return ok
}

// exactKeyKind は、exactKey のうち、値 v の Go の型が列の型 typ の種類に合うか（変換は見ない）。
func exactKeyKind(typ sql.Type, v any) bool {
	switch {
	case types.IsSet(typ), types.IsBit(typ):
		// SET と BIT の列は、ENUM と同じくちょうどとしない（段階 120）。比べる側は値の出どころで比べ方が違う（MySQL は BIT の
		// b = 5.4 を 5 に丸めて当て、文字列の '5' は = で 0x35、> で 5 として比べる）ので、範囲の条件で絞り直し、索引の有無で
		// 結果を変えない
		return false
	case types.IsInteger(typ):
		switch v.(type) {
		case int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint:
		default:
			return false
		}
	case types.IsBinaryType(typ):
		// エンジンの IsText はバイト列の型にも真になるので、先に見る（段階 70）
		switch v.(type) {
		case []byte, string:
		default:
			return false
		}
	case types.IsText(typ):
		if _, ok := v.(string); !ok {
			return false
		}
	case types.IsTime(typ):
	case types.IsDecimal(typ):
		// DECIMAL の列は、丸めのない DECIMAL と整数の値（段階 117。丸めるものは convertKey が拒否する）。比べる側も DECIMAL で
		// 比べる
		switch v.(type) {
		case decimal.Decimal, int8, int16, int32, int64, int, uint8, uint16, uint32, uint64, uint:
		default:
			return false
		}
	case typ.Type() == sqltypes.Float64:
		// DOUBLE の列は倍精度の値だけ（段階 117）。FLOAT の列は、比べる側が単精度に丸めた値で比べるか倍精度で比べるかが
		// 値の出どころで違うので、ちょうどとせず範囲の条件で絞り直す（docs/differences.md）
		if _, ok := v.(float64); !ok {
			return false
		}
	default:
		return false
	}
	return true
}

// keyChanged は、値 v を列の型 typ に変換すると、範囲の外になるか値が変わる（丸められる、切り詰められる）か。
// 数値として読めない文字列は、MySQL と同じく先頭の数の部分（'12abc' なら 12、'abc' なら 0）で決める（段階 85。
// '12.5x' は 12.5 なので整数の列では true）。数値として読めても列の型に変換できない文字列（int unsigned に '-1'、
// '4294967296'）は true。
func keyChanged(ctx *sql.Context, typ sql.Type, v any) bool {
	if v == nil {
		return false
	}
	if _, _, err := typ.Convert(ctx, v); err != nil && !types.TruncatedDatetime(err) {
		d, _, derr := types.InternalDecimalType.Convert(ctx, v)
		if derr != nil && types.TruncatedString(v, derr) && d != nil {
			return keyChanged(ctx, typ, d)
		}
		return derr == nil
	}
	_, ok := convertKey(ctx, typ, v)
	return !ok
}

// byteString は、文字列かバイト列の値のバイト列を返す。
func byteString(v any) (string, bool) {
	switch v := v.(type) {
	case []byte:
		return string(v), true
	case string:
		return v, true
	}
	return "", false
}

// signedInt は、整数の値を符号と絶対値で表す。
type signedInt struct {
	neg bool
	abs uint64
}

// intValue は、Go の整数型の値 v を signedInt にする。整数型でなければ ok が false。
func intValue(v any) (signedInt, bool) {
	var i int64
	switch v := v.(type) {
	case int8:
		i = int64(v)
	case int16:
		i = int64(v)
	case int32:
		i = int64(v)
	case int64:
		i = v
	case int:
		i = int64(v)
	case uint8:
		return signedInt{abs: uint64(v)}, true
	case uint16:
		return signedInt{abs: uint64(v)}, true
	case uint32:
		return signedInt{abs: uint64(v)}, true
	case uint64:
		return signedInt{abs: v}, true
	case uint:
		return signedInt{abs: uint64(v)}, true
	default:
		return signedInt{}, false
	}
	if i < 0 {
		return signedInt{neg: true, abs: uint64(-(i + 1)) + 1}, true
	}
	return signedInt{abs: uint64(i)}, true
}

// convertKey は、値 v を列の型 typ に変換する。範囲の外の値と、整数の列で値が変わる（小数が丸められる）ものは
// ok が false。go-mysql-server の Convert は、2^63 以上の uint64 を狭い unsigned の型に変換すると、下位のビットに
// 切り詰めて範囲内と返す（int unsigned に 2^63+1 なら 1）。BINARY(n) の列に短い値も ok が false（Convert は末尾を
// 0x00 で埋めるが、MySQL は埋めずに比べる。'ab' より大きい範囲に 'ab\0\0' の行が入る。docs/plan.md の段階 70）。
// 日時の列で値が丸められるもの（DATE の列に時刻付き、DATETIME の列に秒より細かい値）も ok が false（Filter は
// 最大の精度で比べるので、丸めた値を区間の端にすると行を落とす）。DECIMAL の列で値が丸められるもの（段階 115）も
// ok が false。
func convertKey(ctx *sql.Context, typ sql.Type, v any) (any, bool) {
	cv, inRange, err := typ.Convert(ctx, v)
	// 日時の列の '2020-01-01 a' は、比べる側（CAST）と同じく切り詰めた値をキーにする（段階 114、上流の #3276）
	if (err != nil && !types.TruncatedDatetime(err)) || inRange != sql.InRange {
		return nil, false
	}
	if typ.Type() == sqltypes.Binary {
		raw, ok := byteString(v)
		padded, _ := byteString(cv)
		if !ok || len(raw) != len(padded) {
			return nil, false
		}
	}
	if types.IsTime(typ) && v != nil {
		full, _, err := types.DatetimeMaxPrecision.Convert(ctx, v)
		if err != nil && !types.TruncatedDatetime(err) {
			return nil, false
		}
		if c, err := types.DatetimeMaxPrecision.Compare(ctx, cv, full); err != nil || c != 0 {
			return nil, false
		}
	}
	if types.IsDecimal(typ) && v != nil {
		// DECIMAL の列で値が丸められるもの（DECIMAL(10,2) に 123.45000001、'123.451'）も ok が false（比べる側は丸めずに
		// DECIMAL か DOUBLE で比べるので、丸めた値を区間の端や決まった値にすると行を落とすか余計に返す。段階 115、上流の #4054）
		orig, _, err := types.InternalDecimalType.Convert(ctx, v)
		a, ok1 := orig.(decimal.Decimal)
		b, ok2 := cv.(decimal.Decimal)
		if err != nil || !ok1 || !ok2 || !a.Equal(b) {
			return nil, false
		}
	}
	if types.IsFloat(typ) && v != nil {
		if _, _, declared := types.RealPrecision(typ); declared || types.RealUnsigned(typ) {
			// FLOAT(M,D)、DOUBLE(M,D) と UNSIGNED の列で値が丸められるか範囲の端にされるもの（FLOAT(7,2) に 1.555）も ok が
			// false（比べる側は丸めないので、1.56 を区間の端にすると、f > 1.555 で 1.56 の行を落とす。段階 120）
			orig, _, err := types.Float64.Convert(ctx, v)
			a, ok1 := orig.(float64)
			var b float64
			switch c := cv.(type) {
			case float32:
				a, b = float64(float32(a)), float64(c)
			case float64:
				b = c
			default:
				return nil, false
			}
			if err != nil || !ok1 || a != b {
				return nil, false
			}
		}
	}
	if types.IsInteger(typ) && v != nil {
		if a, ok := intValue(v); ok {
			b, ok := intValue(cv)
			return cv, ok && a == b
		}
		orig, _, err1 := types.InternalDecimalType.Convert(ctx, v)
		conv, _, err2 := types.InternalDecimalType.Convert(ctx, cv)
		a, ok1 := orig.(decimal.Decimal)
		b, ok2 := conv.(decimal.Decimal)
		if err1 != nil || err2 != nil || !ok1 || !ok2 || !a.Equal(b) {
			return nil, false
		}
	}
	return cv, true
}

// buildIndex は、既存の行からインデックス ki のエントリを作り、rewriteChunkBytes ごとにコミットする（CREATE INDEX。
// docs/plan.md の段階 53）。書き込みロックを持った状態で、確定済みの最新の行を 1 行ずつ読む。DDL の前に実行中の
// トランザクションを暗黙にコミットしている（Session.beginDDL）ので、同じセッションの変更も確定済みになっている。
//
// 最初のコミットで、前に途中で終わった作成のエントリを消し、作成の途中の印を付ける（GC が消さない。store の
// MarkIndexBuild）。コミットしたら started を返す（呼び出し側が、定義を書くコミットで印を外すか、失敗したら消す）。
//
// 一意インデックスなら、行ごとに、コミット済みのエントリ（INSERT と同じ checkUniqueByIndex）と、まだコミットしていない
// 行（値の先頭部分で引く表）で重複を確かめ、重複があれば MySQL と同じく重複のエラー（1062）にする。NULL を含む行は対象外。
// testHookIndexChunk は、buildIndex が作りかけのエントリをコミットするたびに呼ぶ（テストで、作り直しの途中の停止と
// ブランチの作成を起こす。段階 121）。
var testHookIndexChunk func(table, index string)

func (t *Table) buildIndex(ctx *sql.Context, ki keyIndex) (started bool, err error) {
	st, branch := t.db.st, t.db.branch
	var pending []rewriteKV
	pendingBytes := 0
	var txn *store.Txn
	var byPrefix map[string][]sql.Row
	u := uniqueIndex{ords: ki.ords, name: ki.def.Name}
	if ki.def.Unique {
		txn = st.Begin()
		defer func() { _ = txn.Rollback() }()
		byPrefix = map[string][]sql.Row{}
		for _, c := range ki.def.Columns {
			u.prefixes = append(u.prefixes, c.Length)
		}
	}
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := st.Update(func(tx *store.WriteTx) error {
			if !started {
				if err := tx.DropIndexData(branch, t.def.ID, ki.def.ID); err != nil {
					return err
				}
				if err := tx.MarkIndexBuild(branch, t.def.ID, ki.def.ID); err != nil {
					return err
				}
			}
			for _, kv := range pending {
				if err := tx.PutIndexEntry(branch, t.def.ID, ki.def.ID, kv.key, kv.value); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			started = true
			if testHookIndexChunk != nil {
				testHookIndexChunk(t.def.Name, ki.def.Name)
			}
		}
		pending, pendingBytes = nil, 0
		if byPrefix != nil {
			byPrefix = map[string][]sql.Row{}
		}
		return err
	}
	e := &editor{t: t}
	n := 0
	err = st.ScanRowsAsOf(branch, t.def.ID, store.Latest-1, nil, nil, false, func(r store.RowVersion) (bool, error) {
		n++
		if err := interrupted(ctx, n); err != nil {
			return false, err
		}
		row, err := t.decodeRow(r.Value)
		if err != nil {
			return false, err
		}
		if byPrefix != nil {
			if err := t.checkNewUnique(ctx, e, txn, ki, u, byPrefix, row, r.PK); err != nil {
				return false, err
			}
		}
		entry, value, err := t.entry(ki, row)
		if err != nil {
			return false, err
		}
		pending = append(pending, rewriteKV{key: entry, value: value})
		pendingBytes += len(entry) + len(value)
		if pendingBytes >= rewriteChunkBytes {
			return true, flush()
		}
		return true, nil
	})
	if err != nil {
		return started, err
	}
	return started, flush()
}

// checkNewUnique は、作成中の一意インデックス ki で、row がコミット済みのエントリや、まだコミットしていない行
// （byPrefix）と重ならないかを確かめ、byPrefix に row を足す。
func (t *Table) checkNewUnique(ctx *sql.Context, e *editor, txn *store.Txn, ki keyIndex, u uniqueIndex, byPrefix map[string][]sql.Row, row sql.Row, pk []byte) error {
	for _, ord := range ki.ords {
		if row[ord] == nil {
			return nil // NULL を含む行は一意制約の対象外（MySQL と同じ）
		}
	}
	prefix, err := t.valuePrefix(ki, row)
	if err != nil {
		return err
	}
	for _, existing := range byPrefix[string(prefix)] {
		match, err := t.columnsMatch(ctx, u, row, existing)
		if err != nil {
			return err
		}
		if match {
			return t.uniqueDuplicateErr(row, u, existing)
		}
	}
	if err := e.checkUniqueByIndex(ctx, txn, ki, row, pk); err != nil {
		return err
	}
	byPrefix[string(prefix)] = append(byPrefix[string(prefix)], row)
	return nil
}

// dropIndexBuild は、作成に失敗したインデックス ki のエントリと作成の途中の印を消す。消せなくても、次の起動か、同じ
// ID で作り直すときに消える。
func (t *Table) dropIndexBuild(ki keyIndex) {
	if err := t.db.st.Update(func(tx *store.WriteTx) error {
		if err := tx.DropIndexData(t.db.branch, t.def.ID, ki.def.ID); err != nil {
			return err
		}
		return tx.UnmarkIndexBuild(t.db.branch, t.def.ID, ki.def.ID)
	}); err != nil {
		logrus.Errorf("HeimDB: drop the entries of a failed CREATE INDEX on %s: %v", t.def.Name, err)
	}
}

// checkUniqueRows は、rows が一意インデックス def の制約を満たすかを確かめる。
func (t *Table) checkUniqueRows(ctx *sql.Context, def schema.Index, rows []sql.Row) error {
	u := uniqueIndex{name: def.Name}
	for _, c := range def.Columns {
		u.ords = append(u.ords, t.def.ColumnIndex(c.Name))
		u.prefixes = append(u.prefixes, c.Length)
	}
	// 列の型（照合順序を含む）とプレフィックス長で並べ替え、隣どうしを比べる。
	var candidates []sql.Row
	for _, row := range rows {
		hasNull := false
		for _, ord := range u.ords {
			hasNull = hasNull || row[ord] == nil
		}
		if !hasNull {
			candidates = append(candidates, row)
		}
	}
	var cmpErr error
	sort.SliceStable(candidates, func(i, j int) bool {
		c, err := t.compareUnique(ctx, u, candidates[i], candidates[j])
		if err != nil && cmpErr == nil {
			cmpErr = err
		}
		return c < 0
	})
	if cmpErr != nil {
		return cmpErr
	}
	for i := 1; i < len(candidates); i++ {
		c, err := t.compareUnique(ctx, u, candidates[i-1], candidates[i])
		if err != nil {
			return err
		}
		if c == 0 {
			return t.uniqueDuplicateErr(candidates[i], u, candidates[i-1])
		}
	}
	return nil
}

// compareUnique は、一意インデックスの列について a と b を比べる（プレフィックス長は文字数で切る）。
func (t *Table) compareUnique(ctx *sql.Context, u uniqueIndex, a, b sql.Row) (int, error) {
	for i, ord := range u.ords {
		c, err := t.sch.Schema[ord].Type.Compare(ctx, prefix(a[ord], u.prefixes[i]), prefix(b[ord], u.prefixes[i]))
		if err != nil || c != 0 {
			return c, err
		}
	}
	return 0, nil
}
