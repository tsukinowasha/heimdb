package backend

import (
	"bytes"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/stats"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/store"
)

const (
	// statsSampleRows は、統計に使う行の数の目安。これより多い表は間引く（sampled）。
	statsSampleRows = 20000
	// statsBuckets は、インデックスごとのヒストグラムのバケットの数の目安（MySQL の histogram の既定と同じ 100）。
	// 同じ値の並びの手前でバケットを閉じるので（段階 98）、値の入ったバケットは最大でその約 2 倍になる。
	statsBuckets = 100
)

// statsProvider は、go-mysql-server の統計の置き場所（Analyzer.Catalog.StatsProvider）を包み、HeimDB の表には
// インデックスごとのヒストグラムを返す。
//
// 統計がないと、go-mysql-server はインデックスをどれも表の行数から一様に決めた同じ行数と見積もり、同じなら
// インデックスの名前の順で選ぶ（記事ごとのコメントの数を、記事の ID ではなく承認の状態のインデックスで 2 万行
// 読む）。JOIN の順も、範囲の行数の見積もり（ヒストグラム）がないと表の大きさだけで決まる（記事が 75 件の
// タグの term 数でも、記事 5,000 行から引く）。
type statsProvider struct{ sql.StatsProvider }

func (p statsProvider) GetTableStats(ctx *sql.Context, db string, table sql.Table) ([]sql.Statistic, error) {
	if t := unwrapTable(table); t != nil {
		return rowCounts.stats(ctx, t)
	}
	return p.StatsProvider.GetTableStats(ctx, db, table)
}

// unwrapTable は、table が HeimDB の表ならそれを返す。
func unwrapTable(table sql.Table) *Table {
	for {
		switch t := table.(type) {
		case *Table:
			return t
		case sql.TableWrapper:
			table = t.Underlying()
		default:
			return nil
		}
	}
}

// buildStats は、表の行数と、インデックスごとの統計を作る。行は表の頭から 1 回だけ読み、行数も同じ走査で数える。
// 多ければ主キーのハッシュで間引く（同じデータなら同じ統計になり、同じ実行計画になる）。インデックスの値の順は、インデックスのキーの符号化
// （文字列は照合順序の重み）で決める。キーにできない型の列や FULLTEXT のインデックスがある表は、統計を作らない
// （一部のインデックスにだけ統計があると、go-mysql-server はその表の統計をすべて使わない）。
func buildStats(ctx *sql.Context, t *Table) (uint64, []sql.Statistic, *prefixSample, error) {
	indexes, err := t.GetIndexes(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	type statIndex struct {
		idx  sql.Index
		ki   keyIndex
		ords []int
		pk   bool
	}
	var sis []statIndex
	for _, idx := range indexes {
		if idx.ID() == "PRIMARY" {
			sis = append(sis, statIndex{idx: idx, ords: t.def.PKOrdinals, pk: true})
			continue
		}
		ki, ok := t.findKeyIndex(idx.ID())
		if !ok {
			n, err := t.rowCount(ctx)
			return n, nil, nil, err
		}
		sis = append(sis, statIndex{idx: idx, ki: ki, ords: ki.ords})
	}

	// 行は読みながら、インデックスごとの先頭の列の値と、その符号化だけを残す（記事の本文のような大きな列を、
	// 間引いた行の数だけ持たない）。
	//
	// 行数は読み終えるまでわからないので、残す行は主キーのハッシュの下位の bits ビットが 0 の行とし、残した行が
	// statsSampleRows を超えたら bits を 1 つ増やして捨て直す（段階 80。前は行数を先に数えるために表をもう 1 度
	// 読んでいた）。bits は「その bits で残る行が statsSampleRows 以下」になる最小の値になり、読む順によらない。
	type kept struct {
		hash uint64
		vals []any
		keys [][]byte
		// prefix は、インデックスごとの、先頭の 1 列、2 列、... の値の符号化のハッシュ（prefixSample。段階 84）。
		prefix [][]uint64
	}
	var sample []kept
	var n uint64
	bits := uint(0)
	txn, done := txnFor(ctx, t.db.st)
	defer done()
	err = txn.ScanRows(t.db.branch, t.def.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
		n++
		if err := interrupted(ctx, int(n)); err != nil {
			return false, err
		}
		h := pkHash(r.PK)
		if h&(1<<bits-1) != 0 {
			return true, nil
		}
		row, err := t.decodeRow(r.Value)
		if err != nil {
			return false, err
		}
		k := kept{hash: h, vals: make([]any, len(sis)), keys: make([][]byte, len(sis)), prefix: make([][]uint64, len(sis))}
		for j, si := range sis {
			if !si.pk {
				if k.prefix[j], err = t.prefixHashes(si.ki, row); err != nil {
					return false, err
				}
			}
			// ヒストグラムはインデックスの先頭の列だけで作る。go-mysql-server の stats.Union（OR の条件の見積もり）は、
			// 上端が複数の列のバケットで、先頭の列が違うと次の列も比べて読み進め、範囲の外を読んで panic していた
			// （PHPUnit の検索のテスト。段階 46 でエンジンを直した）。複数の列にすると実行計画が変わるので、先頭の
			// 列だけのままにする（インデックスの選び方と JOIN の順には足りる）。
			var key []byte
			if si.pk {
				col := t.sch.Schema[si.ords[0]]
				key, err = rowenc.EncodePK(nil, sql.NewPrimaryKeySchema(sql.Schema{col}, 0), sql.Row{row[si.ords[0]]})
			} else {
				key, _, err = rowenc.AppendIndexValue(nil, si.ki.cols[0], row[si.ords[0]])
			}
			if err != nil {
				return false, err
			}
			k.vals[j], k.keys[j] = row[si.ords[0]], key
		}
		sample = narrowSample(append(sample, k), func(k kept) uint64 { return k.hash }, &bits, statsSampleRows)
		return true, nil
	})
	if err != nil {
		return 0, nil, nil, err
	}
	ps := &prefixSample{scale: 1, hashes: map[string][][]uint64{}}
	if len(sample) > 0 {
		ps.scale = float64(n) / float64(len(sample))
	}
	for j, si := range sis {
		if si.pk {
			continue
		}
		byK := make([][]uint64, len(si.ords))
		for _, k := range sample {
			for c, h := range k.prefix[j] {
				byK[c] = append(byK[c], h)
			}
		}
		for _, hs := range byK {
			slices.Sort(hs)
		}
		ps.hashes[strings.ToLower(si.idx.ID())] = byK
	}
	vals := make([][]any, len(sis))
	keys := make([][][]byte, len(sis))
	for _, k := range sample {
		for j := range sis {
			vals[j] = append(vals[j], k.vals[j])
			keys[j] = append(keys[j], k.keys[j])
		}
	}

	var out []sql.Statistic
	for j, si := range sis {
		s, err := indexStatistic(t, si.idx, si.ords, vals[j], keys[j], n)
		if err != nil {
			return 0, nil, nil, err
		}
		out = append(out, s)
	}
	return n, out, ps, nil
}

// prefixSample は、統計の標本の行の、インデックスの先頭の k 列の値の組のハッシュ（段階 84）。ヒストグラムは先頭の
// 列だけなので（indexStatistic）、meta_key = 'category' AND meta_value = 'cat3' の行数を meta_key = 'category' と
// 同じに見積もり、絞り込みの強い表から読み始めなかった（ヘッドレスの API の two_filters）。MySQL は定数の等号ではインデックスを
// 実際に読んで数える（index dive）。それに近く、標本で同じ組の行を数え、表の行数に合わせて増やす。
type prefixSample struct {
	scale float64 // 表の行数 / 標本の行数
	// hashes は、インデックスの ID（小文字）ごとに、[k-1] が先頭の k 列の組のハッシュを並べたもの。
	hashes map[string][][]uint64
}

// rows は、インデックス index の先頭の k 列が、ハッシュ h の組になる行の数の見積もり。標本に 1 行もなければ ok が false。
func (p *prefixSample) rows(index string, k int, h uint64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	byK, ok := p.hashes[strings.ToLower(index)]
	if !ok || k < 1 || k > len(byK) {
		return 0, false
	}
	hs := byK[k-1]
	// 同じハッシュの続きの終わりも二分探索で探す（JOIN の順の候補ごとに呼ばれ、meta_key = 'category' の続きは標本の
	// 1,000 件を超える。段階 104）。h+1 で探すと math.MaxUint64 で桁あふれする。
	lo, _ := slices.BinarySearch(hs, h)
	hi := lo + sort.Search(len(hs)-lo, func(i int) bool { return hs[lo+i] > h })
	if hi == lo {
		return 0, false
	}
	return float64(hi-lo) * p.scale, true
}

// samplePrefixRows は、インデックス idx の先頭の len(vals) 列が vals（列の型に変換したもの）になる行の数を、統計の標本で
// 見積もる（prefixSample）。統計がまだないか、標本に 1 行もなければ ok が false。
func samplePrefixRows(t *Table, idx sql.Index, vals []any) (float64, bool) {
	p := rowCounts.prefixes(t)
	ki, ok := t.findKeyIndex(idx.ID())
	if p == nil || !ok || len(vals) == 0 || len(vals) > len(ki.cols) {
		return 0, false
	}
	hs, err := prefixHashesOf(ki, vals)
	if err != nil {
		return 0, false
	}
	return p.rows(idx.ID(), len(vals), hs[len(hs)-1])
}

// prefixHashes は、行 row のインデックス ki の先頭の 1 列、2 列、... の値の符号化のハッシュ。
func (t *Table) prefixHashes(ki keyIndex, row sql.Row) ([]uint64, error) {
	vals := make([]any, len(ki.ords))
	for i, ord := range ki.ords {
		vals[i] = row[ord]
	}
	return prefixHashesOf(ki, vals)
}

// prefixHashesOf は、インデックス ki の先頭の列から順の値 vals（列の型に変換したもの）の、先頭の 1 列、2 列、... の
// 符号化のハッシュ。
func prefixHashesOf(ki keyIndex, vals []any) ([]uint64, error) {
	var key []byte
	out := make([]uint64, len(vals))
	for i, v := range vals {
		var err error
		if key, _, err = rowenc.AppendIndexValue(key, ki.cols[i], v); err != nil {
			return nil, err
		}
		out[i] = pkHash(key)
	}
	return out, nil
}

// narrowSample は、標本 sample が limit を超えている間、残す行のハッシュの下位のビットを 1 つずつ増やし（*bits）、
// そのビットが 0 でない行を捨てる。行を 1 つ足すたびに呼べば、最後の *bits は「そのビット数で残る行が limit 以下」に
// なる最小の値で、残る行は読む順によらない。
func narrowSample[T any](sample []T, hash func(T) uint64, bits *uint, limit int) []T {
	for len(sample) > limit {
		*bits++
		mask := uint64(1)<<*bits - 1
		sample = slices.DeleteFunc(sample, func(k T) bool { return hash(k)&mask != 0 })
	}
	return sample
}

// pkHash は、符号化した主キー pk のハッシュ。統計に使う行を、下位のビットが 0 の行として選ぶ。等間隔（主キーの順に
// step 行ごと）に間引くと、記事ごとに続けて挿入されるメタの組（_pingme と _encloseme）の片方だけが残り、残らなかった
// キーを 0 行と見積もる（docs/plan.md の段階 29）。主キーのハッシュで決めれば行の並びの周期と重ならず、同じデータなら
// 同じ行が残る。
func pkHash(pk []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, b := range pk {
		h ^= uint64(b)
		h *= 1099511628211
	}
	// FNV-1a の下位のビットは各バイトの下位のビットだけで決まる（連番の主キーでは h % 2 がまた交互になる）ので、
	// 全体を混ぜてから割る（splitmix64 の最後の段）。
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}

// indexStatistic は、間引いた行のインデックスの先頭の列の値 sample と、その符号化 keys から、インデックス idx
// （列は ords）の統計を作る。
// バケットは行数がほぼ等しくなるように切り、同じ値は 1 つのバケットに収める。数は表の行数 total に合わせて増やす。
// 切れ目が同じ値の並びの途中に来たら、並びがバケットの途中から始まるならその手前で閉じ、並びは次のバケットにする
// （段階 98）。並びの最後まで延ばすと、9 割を超える値（wp_options.autoload の off）の手前の少ない値（auto と no）が
// その値と同じバケットに入り、等号の見積もりが表の行数近くになって、5 行を返す文でも表を全部読んでいた。
func indexStatistic(t *Table, idx sql.Index, ords []int, sample []any, keys [][]byte, total uint64) (sql.Statistic, error) {
	order := make([]int, len(sample))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return bytes.Compare(keys[order[a]], keys[order[b]]) < 0 })

	scale := 1.0
	if len(sample) > 0 {
		scale = float64(total) / float64(len(sample))
	}
	scaled := func(c int) uint64 { return uint64(float64(c)*scale + 0.5) }

	var buckets []sql.HistogramBucket
	var distinct, nulls int
	sorted := make([][]byte, len(order))
	for j, o := range order {
		sorted[j] = keys[o]
	}
	start := 0
	for _, end := range bucketEnds(sorted, (len(sample)+statsBuckets-1)/statsBuckets) {
		var bDistinct, bNulls, boundCnt int
		for j := start; j < end; j++ {
			if j == start || !bytes.Equal(keys[order[j]], keys[order[j-1]]) {
				bDistinct++
			}
			if sample[order[j]] == nil {
				bNulls++
			}
			if bytes.Equal(keys[order[j]], keys[order[end-1]]) {
				boundCnt++
			}
		}
		// バケットの前に、バケットの最小の値を上端にした空のバケットを置く。go-mysql-server は、等号の値を含む
		// バケット（値より上端が大きい最初のバケット）の行数をそのまま見積もりにするので、値の種類が少ないと、
		// 表にない値（メニューの項目のメタのキー）でも何千行と見積もる。空のバケットがあれば、前のバケットの
		// 上端と最小の値の間にある値は 0 行と見積もる。
		lowest := sql.Row{sample[order[start]]}
		upper := sql.Row{sample[order[end-1]]}
		buckets = append(buckets,
			stats.NewHistogramBucket(0, 0, 0, 0, lowest, nil, nil),
			stats.NewHistogramBucket(scaled(end-start), scaled(bDistinct), scaled(bNulls), scaled(boundCnt), upper, nil, nil))
		distinct += bDistinct
		nulls += bNulls
		start = end
	}

	// 最後に、先頭の列の型の最大値を上端にした空のバケットを置く。表のどの値より大きい値（作ったばかりの term の
	// ID）では、go-mysql-server はバケットが 1 つもない見積もりになり、表の半分の行数と見積もる。
	if len(buckets) > 0 {
		if hi, ok := typeMax(sample[order[len(order)-1]], t.sch.Schema[ords[0]].Type); ok {
			buckets = append(buckets, stats.NewHistogramBucket(0, 0, 0, 0, sql.Row{hi}, nil, nil))
		}
	}

	first := idx.ColumnExpressionTypes()[0]
	cols := []string{strings.TrimPrefix(strings.ToLower(first.Expression), strings.ToLower(t.def.Name)+".")}
	types := []sql.Type{first.Type}
	qual := sql.NewStatQualifier(t.db.name, "", t.def.Name, idx.ID())
	s := stats.NewStatistic(total, tableDistinct(idx.IsUnique() && len(ords) == 1, keys, order, total, scale), scaled(nulls), 0, time.Time{}, qual, cols, types, buckets, sql.IndexClassDefault, nil)
	fds, colSet := indexFDs(t, idx, ords)
	return s.WithFuncDeps(fds).WithColSet(colSet), nil
}

// bucketEnds は、並べたキー sorted を per 個ずつのバケットに切り、各バケットの終わり（次のバケットの先頭）の
// 位置を返す。同じ値はバケットの境目をまたがせない。切れ目が同じ値の並びの途中に来たら、並びがバケットの
// 途中から始まるならその手前で閉じ、バケットの先頭から始まるなら並びの最後まで延ばす（段階 98）。手前で閉じた
// バケットと次のバケットを合わせると per を超えるので、バケットの数は最大で約 2 × len(sorted) / per。
func bucketEnds(sorted [][]byte, per int) []int {
	// runStart[j] は、j 番目の値と同じ値の並びの先頭の位置
	runStart := make([]int, len(sorted))
	for j := 1; j < len(sorted); j++ {
		runStart[j] = j
		if bytes.Equal(sorted[j], sorted[j-1]) {
			runStart[j] = runStart[j-1]
		}
	}
	var ends []int
	for start := 0; start < len(sorted); {
		end := min(start+max(per, 1), len(sorted))
		if end < len(sorted) && runStart[end] < end {
			if runStart[end] > start {
				end = runStart[end]
			} else {
				for end < len(sorted) && runStart[end] < end {
					end++
				}
			}
		}
		ends = append(ends, end)
		start = end
	}
	return ends
}

// tableDistinct は、インデックスの先頭の列の値の種類の数を、表全体で見積もる（段階 80）。間引かなければ数えた値。
// 1 列の一意なインデックス（unique）は行数。間引いたときは、標本の値の種類を掛け算で増やすと、1 つの値の行数が間引きの幅より
// 少ない列（記事ごとに約 100 行の wp_postmeta.post_id）がどの値も標本に 1 度しか現れず、値の種類を行数近くまで
// 多く見積もる（JOIN の右を引くたびに読む行の数を 1 行と見てしまう。coster.go）。標本に 1 度と 2 度現れた値の数
// （f1、f2）から Chao1 で見積もる（表の行数を超えない）。バケットの値の種類は今のまま（段階 80 では変えない）。
func tableDistinct(unique bool, keys [][]byte, order []int, total uint64, scale float64) uint64 {
	if unique {
		return total
	}
	var d, f1, f2 float64
	for i := 0; i < len(order); {
		j := i + 1
		for j < len(order) && bytes.Equal(keys[order[j]], keys[order[i]]) {
			j++
		}
		d++
		switch j - i {
		case 1:
			f1++
		case 2:
			f2++
		}
		i = j
	}
	if scale <= 1 {
		return uint64(d)
	}
	return uint64(math.Min(d+f1*(f1-1)/(2*(f2+1)), float64(total)) + 0.5)
}

// indexFDs は、インデックスの列の集合と、一意なら鍵の関数従属を返す（go-mysql-server の analyzer.indexFds と同じ）。
func indexFDs(t *Table, idx sql.Index, ords []int) (*sql.FuncDepSet, sql.ColSet) {
	var idxCols, all, notNull sql.ColSet
	for _, ord := range ords {
		idxCols.Add(sql.ColumnId(ord + 1))
	}
	for i, col := range t.esch.Schema {
		all.Add(sql.ColumnId(i + 1))
		if !col.Nullable {
			notNull.Add(sql.ColumnId(i + 1))
		}
	}
	var strict, lax []sql.ColSet
	if idx.IsUnique() {
		if idxCols.SubsetOf(notNull) {
			strict = append(strict, idxCols)
		} else {
			lax = append(lax, idxCols)
		}
	}
	return sql.NewTablescanFDs(all, strict, lax, notNull), idxCols
}

// typeMax は、整数と日時の列の型 typ の最大値を、列の値 v と同じ Go の型で返す。
func typeMax(v any, typ sql.Type) (any, bool) {
	switch v.(type) {
	case int8:
		return int8(math.MaxInt8), true
	case int16:
		return int16(math.MaxInt16), true
	case int32:
		return int32(math.MaxInt32), true
	case int64:
		return int64(math.MaxInt64), true
	case uint8:
		return uint8(math.MaxUint8), true
	case uint16:
		return uint16(math.MaxUint16), true
	case uint32:
		return uint32(math.MaxUint32), true
	case uint64:
		return uint64(math.MaxUint64), true
	case time.Time:
		if types.IsTime(typ) {
			return time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), true
		}
	}
	return nil, false
}
