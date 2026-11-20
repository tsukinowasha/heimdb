package backend

import (
	"math"
	"slices"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/memo"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// discardedRowCost は、範囲を順に読んで Filter で捨てる 1 行のコスト（discardedRowsCost）。範囲を読む 1 行は、引いて読む
// 1 行（go-mysql-server の lookup JOIN の 1 行は 1 + 1.3）より安い。記事 API の形の文（記事 2,000 件と 2 万件。category の
// 値の割合が 10%、12.5%、20%）で、0.2〜0.8 なら API 用の索引でも標準の索引でも MySQL と同じ読み始めになる。その間の値にする。
const discardedRowCost = 0.4

// nestedLoopPenalty は、条件のある入れ子のループの JOIN のコストに掛ける倍率。
const nestedLoopPenalty = 1e4

// minCoveredHashRows は、インデックスだけで読む作る側のハッシュ JOIN を軽く見積もる、作る側の行数の見積もりの下限。
// 少ない行の見積もり（統計のない表や、表にない値）は外れることがあり、外れるとハッシュ JOIN は作る側をすべて読む。
// 少なければ lookup JOIN との差もない。
const minCoveredHashRows = 100

// coster は、go-mysql-server の既定のコスト計算に、条件のある入れ子のループの JOIN への割り増しを足し、作る側を
// インデックスだけで読むハッシュ JOIN を軽く見積もる（coveredPKHash）。
//
// go-mysql-server は統計のないインデックスの読み出しを 1 行と見積もるので、等号の JOIN でも、内側を外側の行ごとに
// 読み直す入れ子のループ（InnerJoin など）を選びやすい。HeimDB の読み出しは 1 回ごとに範囲の行をまとめて読むので、
// WordPress の term や meta の JOIN で数百倍遅くなった（docs/bench.md）。lookup かハッシュの JOIN にできる限り、
// そちらを選ばせる。条件のない JOIN（CROSS JOIN）と、ほかに方法のない JOIN はそのまま入れ子のループになる。
type coster struct{ memo.Coster }

func (c coster) EstimateCost(ctx *sql.Context, n memo.RelExpr, s sql.StatsProvider) (float64, error) {
	if lj, ok := n.(*memo.LookupJoin); ok && uniqueLookup(lj) {
		// 左で捨てる行も数える（meta_query の、絞ったメタから記事を主キーで引く形。段階 95）
		return injectiveLookupCost(math.Max(1, float64(lj.Left.RelProps.GetStats().RowCount()))) + discardedRowsCost(ctx, lj), nil
	}
	cost, err := c.Coster.EstimateCost(ctx, n, s)
	if err != nil {
		return cost, err
	}
	if lj, ok := n.(*memo.LookupJoin); ok {
		if !lookupRefsLeft(lj) {
			// 引くキーが左の列を含まない（定数だけ）lookup JOIN は、左の行ごとに同じ範囲を読み直す入れ子のループ（段階 80）。
			return (cost + 1) * nestedLoopPenalty, nil
		}
		if fromSubquery(lj) || pairedWithUniqueLookup(lj) {
			cost = math.Max(cost, subqueryLookupCost(ctx, lj))
		}
	}
	if mj, ok := n.(*memo.MergeJoin); ok {
		cost = math.Max(cost, mergeScanCost(ctx, mj, s))
	}
	if j, ok := n.(memo.JoinRel); ok {
		cost += discardedRowsCost(ctx, j)
		jp := j.JoinPrivate()
		if len(jp.Filter) > 0 && nestedLoop(jp.Op) {
			cost = (cost + 1) * nestedLoopPenalty
		}
		if h, ok := n.(*memo.HashJoin); ok && !integerKeys(h) {
			cost = (cost + 1) * nestedLoopPenalty
		} else if r := float64(jp.Right.RelProps.GetStats().RowCount()); ok && r >= minCoveredHashRows && !jp.Op.IsPartial() {
			switch coveredPKHash(ctx, h) {
			case pkByKeys:
				// 作る側をインデックスのエントリだけで読むなら、行は小さく、本体の行も引かない。go-mysql-server は作る側の
				// 1 行を、読むのに 1、ハッシュに入れるのに 2 と見積もるが、測るとエントリ 1 行は主キーで 1 行を引くより
				// 5 倍ほど速い（記事 5,000 件の term 数で、lookup JOIN 10 ms、ハッシュ JOIN 5 ms）。作る側の 1 行を 1.5 にする。
				cost -= r * 1.5
			case pkByKeysAndRange:
				// 主キーの一部が範囲で決まる形（カテゴリの記事の一覧）は、同じ左から主キーで引く lookup JOIN の
				// コストのすぐ下にする。その lookup JOIN が選ばれていた文でだけハッシュ JOIN に替わり、読み始める表と
				// 行の並びは変わらない（右から読み始める方が安い文で左右が入れ替わると、ORDER BY のない文の並びが
				// MySQL と変わる。段階 77）。
				l := math.Max(1, float64(jp.Left.RelProps.GetStats().RowCount()))
				cost = math.Min(cost, injectiveLookupCost(l)*0.99)
			}
		}
	}
	return cost, nil
}

// injectiveLookupCost は、左の行ごとに右を高々 1 行引く lookup JOIN（主キーや一意キーで引く）のコスト。
// go-mysql-server は、JOIN の出力の行数の見積もりを左の行数の代わりに使うが、その見積もりは条件を見ずに表全体の
// 統計から作ることがある（wp_posts と wp_postmeta の JOIN で、メタの条件で数行に絞れても 1 万行台）。そうすると、
// 両方の表を全部読むマージ JOIN の方が安く見えてしまう。引く回数は左の行数を超えないので、左の行数で見積もる
// （係数は go-mysql-server の coster.go と同じ。順に読む 1、ランダムに引く 1.3）。
func injectiveLookupCost(l float64) float64 {
	return l*1 + l*(1.3+1)
}

// mergeScanCost は、マージ JOIN が両方の表をインデックスの順に全部読むコスト。go-mysql-server は、子が Filter の
// とき（条件のある表）、表の行数の代わりに条件で絞った行数の見積もりを使うので、メタのキーで数行に絞れる JOIN
// でも、表を全部読むマージ JOIN が最も安く見えていた（wp_posts と wp_postmeta の JOIN）。
func mergeScanCost(ctx *sql.Context, mj *memo.MergeJoin, s sql.StatsProvider) float64 {
	var rows float64
	for _, scan := range []*memo.IndexScan{mj.InnerScan, mj.OuterScan} {
		if scan == nil || scan.Table == nil {
			return 0
		}
		n, err := s.RowCount(ctx, scan.Table.Database().Name(), scan.Table)
		if err != nil {
			return 0
		}
		rows += float64(n)
	}
	return rows * (1 + 0.01)
}

// uniqueLookup は、lookup JOIN が一意なインデックス（主キーを含む）のすべての列で引くか（左の行ごとに高々 1 行）。
// go-mysql-server の Injective は、列の NULL の扱いなどを関数従属で確かめるので、主キーで引く JOIN でも false の
// ことがある。等号で引くので、NULL のキーはどの行にも当たらない。
func uniqueLookup(lj *memo.LookupJoin) bool {
	if lj.Injective {
		return true
	}
	if lj.Lookup == nil || lj.Lookup.Index == nil || lj.Lookup.Table == nil {
		return false
	}
	idx := lj.Lookup.Index.SqlIdx()
	return idx.IsUnique() && len(lj.Lookup.Table.Expressions()) >= len(idx.Expressions())
}

// lookupRefsLeft は、lookup JOIN の引くキーが左の列を含むか。go-mysql-server は右の条件の定数（`meta_key = 'x'`）
// だけでもキーを作り（analyzer の keyForExpr）、JOIN の条件（`ID = post_id`）を引いた後の条件に回す。そうすると左の
// 行ごとに同じ範囲を読み直すが、コストは読み直す分を数えないので、ハッシュ JOIN や JOIN の条件で引く lookup JOIN
// より安く見え、`NOT IN (SELECT post_id FROM wp_postmeta WHERE meta_key = ...)` や meta_query の NOT EXISTS が、
// 記事の数とそのキーの行の数の積の時間になっていた（ホスティング側の計測。MySQL は内側を 1 回読んで引く）。
func lookupRefsLeft(lj *memo.LookupJoin) bool {
	if lj.Lookup == nil || lj.Lookup.Table == nil {
		return true
	}
	left := lj.Left.RelProps.OutputCols()
	for _, e := range lj.Lookup.Table.Expressions() {
		if transform.InspectExpr(e, func(e sql.Expression) bool {
			gf, ok := e.(*expression.GetField)
			return ok && left.Contains(gf.Id())
		}) {
			return true
		}
	}
	return false
}

// fromSubquery は、lookup JOIN が、サブクエリから作った半結合か反結合（`IN (SELECT ...)`、`NOT IN`、`EXISTS`、
// `NOT EXISTS`）か。反結合は LEFT JOIN の右が NULL の行を残す形に書き換わり（NOT IN は LeftOuterExcludeNulls、
// NOT EXISTS は段階 112 の上流の #3135 から普通の LeftOuter）、lookup にすると書いた LEFT JOIN と同じ種類になるので、
// 書き換えでエンジンが付ける印（memo の JoinBase の FromAnti）で見分ける。NOT EXISTS の反結合は、右の NULL を含める
// 種類（AntiLookupIncludeNulls）にもなる。
func fromSubquery(lj *memo.LookupJoin) bool {
	switch lj.Op {
	case plan.JoinTypeSemiLookup, plan.JoinTypeAntiLookup, plan.JoinTypeAntiLookupIncludeNulls:
		return true
	case plan.JoinTypeLeftOuterLookup:
		if lj.FromAnti {
			return true
		}
		for e := lj.Group().First; e != nil; e = e.Next() {
			if j, ok := e.(memo.JoinRel); ok && j.JoinPrivate().FromAnti {
				return true
			}
		}
	}
	return false
}

// pairedWithUniqueLookup は、同じグループ（同じ 2 つの表の JOIN）に、主キーか一意キーで引く lookup JOIN があるか
// （段階 83）。go-mysql-server は、非一意のインデックスで引く lookup JOIN（記事から post_id でメタを引く）のコストに、
// 引くたびに読んで捨てる行（記事あたり 16 行）を数えず、JOIN の出力の行数として、同じグループの主キーで引く形
// （meta_key の範囲から記事を引く）の左の行数を借りる。そのため 2 つの向きのコストの差が約 5% しかなく、統計の標本の
// 揺れで入れ替わり、記事 2 万件から 32 万行を読む向きを選んでいた（ヘッドレスの API の priority と range、起動し直した後の
// category）。対になる形があるときだけ読む行を数えるので、向きの選択にだけ効き、ハッシュ JOIN には切り替えない
// （LIMIT で途中で止まる今の一覧は、どれも主キーで引く形で、ここを通らない）。
func pairedWithUniqueLookup(lj *memo.LookupJoin) bool {
	for e := lj.Group().First; e != nil; e = e.Next() {
		if other, ok := e.(*memo.LookupJoin); ok && other != lj && uniqueLookup(other) {
			return true
		}
	}
	return false
}

// discardedRowsCost は、JOIN が読み切る子（lookup JOIN の左、ハッシュ JOIN とマージ JOIN の両方）が、表やインデックスの
// 範囲を読んで、範囲に入らない条件の Filter で捨てる行を読む手間（段階 95）。go-mysql-server は、そうした Filter の
// グループをコストを数える前に決めてしまい（memo の MemoizeStaticIndexAccess）、JOIN は子の出力の行数しか見ないので、
// `CAST(meta_value AS SIGNED) = '1'` で 1 割に絞る読み出しが、範囲で同じ行数に絞る読み出し（API 用の索引の
// (category, cat3) の点）と同じに見える。そのため meta_query の two_filters（WP_Query の NUMERIC の比べ）で、featured の
// 2 万行を全部読んで CAST で絞っていた（MySQL は category の点から読み、featured は記事ごとに (post_id, meta_key) で
// 1 行ずつ引く）。捨てる行は、出力の行数と、範囲に入らない条件の割合（filterParts の residual）から数える。
func discardedRowsCost(ctx *sql.Context, j memo.JoinRel) float64 {
	jp := j.JoinPrivate()
	cost := discardedSideCost(ctx, jp.Left)
	if jp.Op.IsHash() || jp.Op.IsMerge() {
		cost += discardedSideCost(ctx, jp.Right)
	}
	return cost
}

// discardedSideCost は、JOIN の子 g が範囲を読んで Filter で捨てる行を読む手間（discardedRowsCost）。
func discardedSideCost(ctx *sql.Context, g *memo.ExprGroup) float64 {
	f, ok := g.Best.(*memo.Filter)
	if !ok || f.Child == nil {
		return 0
	}
	if _, ok := f.Child.Best.(memo.SourceRel); !ok {
		return 0
	}
	_, residual := filterParts(ctx, f)
	if residual <= 0 || residual >= 1 {
		return 0
	}
	return float64(g.RelProps.GetStats().RowCount()) * (1/residual - 1) * discardedRowCost
}

// subqueryLookupCost は、サブクエリから作った lookup JOIN が、左の行ごとに引いて読む行（引いた後の条件で捨てる行を
// 含む）を数えたコスト（段階 80）。go-mysql-server の lookup JOIN のコストは JOIN の出力の行数で数えるので、
// `NOT IN (SELECT post_id FROM wp_postmeta WHERE meta_key = ...)` を post_id で引くと、記事 1 件ごとにメタを約 100 行
// 読んで捨てるのに、1 行と見積もる。読む行は、引くインデックスの先頭の列の 1 つの値あたりの行数（表の行数 ÷ 値の
// 種類の数。statistics.go の tableDistinct）で見積もる。そうするとサブクエリを 1 回だけ読むハッシュ JOIN（MySQL の
// 実体化に当たる）が選ばれる。普通の JOIN は変えない（LIMIT のある記事の一覧で、作る側を全部読むハッシュ JOIN に
// 替わると遅くなる）。
func subqueryLookupCost(ctx *sql.Context, lj *memo.LookupJoin) float64 {
	if lj.Lookup == nil || lj.Lookup.Table == nil || lj.Lookup.Index == nil {
		return 0
	}
	var t *Table
	switch u := lj.Lookup.Table.UnderlyingTable().(type) {
	case *IndexedTable:
		t = u.Table
	default:
		t = unwrapTable(u)
	}
	if t == nil {
		return 0
	}
	statistics, err := rowCounts.stats(ctx, t)
	if err != nil {
		return 0
	}
	for _, st := range statistics {
		if !strings.EqualFold(st.Qualifier().Index(), lj.Lookup.Index.SqlIdx().ID()) {
			continue
		}
		perKey := float64(st.RowCount()) / math.Max(1, float64(st.DistinctCount()))
		l := math.Max(1, float64(lj.Left.RelProps.GetStats().RowCount()))
		return l*1 + l*perKey*(1.3+1)
	}
	return 0
}

// integerKeys は、ハッシュ JOIN のキーがすべて整数の列どうしか、整数と文字列の列の組か。go-mysql-server のハッシュ
// JOIN は照合順序を見ずに値をそのまま比べる（'Red' と 'red' が別のキーになる）ので、文字列などのキーでは lookup か
// 入れ子のループの JOIN にさせる。WordPress の JOIN のキーは整数の ID。整数と文字列の組（プラグインの
// ID = meta_value）は、両辺を double にしてハッシュする（段階 112 の上流の #3200。数にできない文字列は 0）ので照合
// 順序に関わらず、MySQL と同じ組がつながる。
func integerKeys(h *memo.HashJoin) bool {
	if len(h.LeftAttrs) != len(h.RightAttrs) {
		return false
	}
	for i, l := range h.LeftAttrs {
		lt, rt := l.Type(), h.RightAttrs[i].Type()
		switch {
		case types.IsInteger(lt) && types.IsInteger(rt):
		case types.IsInteger(lt) && types.IsText(rt), types.IsText(lt) && types.IsInteger(rt):
		default:
			return false
		}
	}
	return true
}

// nestedLoop は、op が内側を外側の行ごとに読み直す JOIN か。
func nestedLoop(op plan.JoinType) bool {
	switch op {
	case plan.JoinTypeInner, plan.JoinTypeSemi, plan.JoinTypeAnti, plan.JoinTypeAntiIncludeNulls, plan.JoinTypeLeftOuter,
		plan.JoinTypeLeftOuterExcludeNulls:
		return true
	}
	return false
}

// coveredPKHash は、ハッシュ JOIN h の作る側（右）が、文が使う列をすべてエントリに持つセカンダリインデックスの読み出し
// （本体の行を引かない。docs/plan.md の段階 27）で、作る側の主キーのすべての列で引くか。主キーで引くので、左の 1 行に
// 当たる行は高々 1 行で、行の並びは同じ左から主キーで引く lookup JOIN と同じ（左の順）になる。読み出しの範囲で 1 つの
// 値に決まる列（カテゴリの記事の一覧の term_taxonomy_id = 1）も、引く列に数える（段階 77）。
// pkHashKind は、coveredPKHash の結果。
type pkHashKind int

const (
	pkNotCovered     pkHashKind = iota
	pkByKeys                    // 主キーのすべての列を JOIN のキーで引く
	pkByKeysAndRange            // 主キーの一部の列が、作る側の読み出しの範囲で 1 つの値に決まる
)

func coveredPKHash(ctx *sql.Context, h *memo.HashJoin) pkHashKind {
	rel := h.Right.Best
	if f, ok := rel.(*memo.Filter); ok && f.Child != nil {
		rel = f.Child.Best // 範囲に入らない条件は、読み出しの上の Filter になる
	}
	scan, ok := rel.(*memo.IndexScan)
	if !ok || scan.Table == nil || scan.Index == nil {
		return pkNotCovered
	}
	var t *Table
	switch u := scan.Table.UnderlyingTable().(type) {
	case *IndexedTable:
		t = u.Table
	default:
		t = unwrapTable(u)
	}
	if t == nil {
		return pkNotCovered
	}
	ki, ok := t.findKeyIndex(scan.Index.SqlIdx().ID())
	if !ok || !t.covers(ki) {
		return pkNotCovered
	}
	table := scan.Alias
	if table == "" {
		table = scan.Table.Name()
	}
	var fixed map[string]bool // 範囲は、JOIN のキーで足りない列があるときだけ見る（コストは計画のたびに何度も求める）
	for _, ord := range t.def.PKOrdinals {
		name := t.def.Columns[ord].Name
		if slices.ContainsFunc(h.RightAttrs, func(e sql.Expression) bool {
			gf, ok := e.(*expression.GetField)
			return ok && strings.EqualFold(gf.Name(), name) && strings.EqualFold(gf.Table(), table)
		}) {
			continue
		}
		if fixed == nil {
			fixed = pointColumns(ctx, scan.Table)
		}
		if !fixed[strings.ToLower(name)] {
			return pkNotCovered
		}
	}
	if fixed != nil {
		return pkByKeysAndRange
	}
	return pkByKeys
}

// pointColumns は、インデックスの読み出し access のどの範囲でも 1 つの値に決まる、インデックスの先頭からの列の名前
// （小文字）を返す。
func pointColumns(ctx *sql.Context, access *plan.IndexedTableAccess) map[string]bool {
	if !access.IsStatic() {
		return nil
	}
	lookup, err := access.GetLookup(ctx, nil)
	if err != nil {
		return nil
	}
	ranges, ok := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok || len(ranges) == 0 {
		return nil
	}
	fixed := map[string]bool{}
	for i, e := range lookup.Index.Expressions() {
		var point any
		for j, r := range ranges {
			if i >= len(r) {
				return fixed
			}
			ce := r[i]
			_, lowerIncl := ce.LowerBound.(sql.Below)
			_, upperIncl := ce.UpperBound.(sql.Above)
			if !lowerIncl || !upperIncl {
				return fixed
			}
			v := sql.GetMySQLRangeCutKey(ce.LowerBound)
			if c, err := ce.Typ.Compare(ctx, v, sql.GetMySQLRangeCutKey(ce.UpperBound)); err != nil || c != 0 || v == nil {
				return fixed
			}
			if j == 0 {
				point = v
			} else if c, err := ce.Typ.Compare(ctx, v, point); err != nil || c != 0 {
				return fixed
			}
		}
		fixed[strings.ToLower(e[strings.LastIndexByte(e, '.')+1:])] = true
	}
	return fixed
}
