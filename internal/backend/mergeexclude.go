package backend

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/merge"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// マージから外す表と、マージを拒否する理由（docs/plan.md の段階 60）。
//
//	CALL heimdb_merge_exclude('wp_yoast_%');   -- マージから外す表のパターンを足す（管理者だけ）
//	CALL heimdb_merge_include('wp_yoast_%');   -- パターンを消す（管理者だけ）
//	CALL heimdb_merge_excludes();              -- 一覧（サイトのユーザーも読める）
//
// 外した表には、マージで何もしない（行、列の定義、インデックスを比べず、書かない）。WordPress 本体の表は外せない。

// statusExcluded と statusNotExcluded は、差分の要約とマージの結果の、外した表と、パターンに当たったが外さなかった
// 本体の表の状態。
const (
	statusExcluded    = "excluded"
	statusNotExcluded = "not excluded"
)

// coreTables は、WordPress 本体の表の名前（接頭辞を除く）。マルチサイトの表（wp_2_posts など）は、もともとマージの規則の外。
var coreTables = []string{
	"posts", "postmeta", "options", "comments", "commentmeta", "terms", "term_taxonomy", "term_relationships",
	"termmeta", "links", "users", "usermeta",
}

// isCoreTable は、name が接頭辞 prefix の WordPress 本体の表かどうか（大小を区別しない完全な一致。名前の終わりだけでは
// 見ないので、wp_foo_posts は本体ではない）。
func isCoreTable(prefix, name string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(name), strings.ToLower(prefix))
	return ok && slices.Contains(coreTables, rest)
}

// likeMatch は、name がパターン pattern に当たるかどうか。% は 0 文字以上、_ は 1 文字（LIKE と同じ。エスケープはない）。
// 大小を区別しない。
func likeMatch(pattern, name string) bool {
	p, s := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(name))
	// 最後の % の位置と、そのとき s のどこまで読んだかを覚えて戻る（貪欲でない一致）。
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '%':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi] == '_' || p[pi] == s[si]):
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

// exclusion は、表 name に当たったパターン（なければ空）と、それが本体の表なので外さないかどうか。
func exclusion(patterns []string, prefix, name string) (pattern string, core bool) {
	for _, pat := range patterns {
		if likeMatch(pat, name) {
			return pat, isCoreTable(prefix, name)
		}
	}
	return "", false
}

var mergeExcludesSchema = sql.Schema{
	{Name: "pattern", Type: types.LongText},
}

// mergeExcludeProcedures は、マージから外す表の手続き。一覧（heimdb_merge_excludes）だけは管理者でなくても呼べる
// （publicProcedures）。
func (p *Provider) mergeExcludeProcedures() []sql.ExternalStoredProcedureDetails {
	return []sql.ExternalStoredProcedureDetails{
		{Name: "heimdb_merge_exclude", Schema: mergeExcludesSchema, Function: func(ctx *sql.Context, pattern string) (sql.RowIter, error) {
			return p.mergeExclude(pattern, true)
		}},
		{Name: "heimdb_merge_include", Schema: mergeExcludesSchema, Function: func(ctx *sql.Context, pattern string) (sql.RowIter, error) {
			return p.mergeExclude(pattern, false)
		}},
		{Name: "heimdb_merge_excludes", Schema: mergeExcludesSchema, ReadOnly: true, Function: func(ctx *sql.Context) (sql.RowIter, error) {
			return p.mergeExcludesRows()
		}},
	}
}

// publicProcedures は、管理者でなくても、そのデータベースの EXECUTE 権限（サイトのユーザーとブランチだけの権限の
// ユーザーは持つ）で呼べる手続き。読むだけのもの。
var publicProcedures = map[string]bool{"heimdb_merge_excludes": true}

// mergeExclude は、パターンを足す（add）か消す。同じパターンを足しても、ないパターンを消しても何もしない（払い出しの
// 手順を何度流してもよいように）。足すときは、本番の今の表で接頭辞が決まれば、本体の表に当たるパターンをエラーにする
// （WordPress のインストールの前なら受け付け、マージのときに本体の表を外さない）。
func (p *Provider) mergeExclude(pattern string, add bool) (sql.RowIter, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, fmt.Errorf("HeimDB: the pattern is empty")
	}
	if add {
		prefixes, err := detectPrefixes(p.st, store.MainBranchID)
		if err != nil {
			return nil, err
		}
		if len(prefixes) == 1 {
			for _, c := range coreTables {
				if name := prefixes[0] + c; likeMatch(pattern, name) {
					return nil, refuseCoreTable.errorf("the pattern %q matches the WordPress core table %s, which cannot be excluded from merges", pattern, name)
				}
			}
		}
	}
	err := p.st.UpdateMergeExcludes(func(cur []string) ([]string, error) {
		i := slices.IndexFunc(cur, func(s string) bool { return strings.EqualFold(s, pattern) })
		switch {
		case add && i < 0:
			return append(cur, pattern), nil
		case !add && i >= 0:
			return slices.Delete(cur, i, i+1), nil
		}
		return cur, nil
	})
	if err != nil {
		return nil, err
	}
	return p.mergeExcludesRows()
}

func (p *Provider) mergeExcludesRows() (sql.RowIter, error) {
	patterns, err := p.st.MergeExcludes()
	if err != nil {
		return nil, err
	}
	rows := make([]sql.Row, len(patterns))
	for i, pat := range patterns {
		rows[i] = sql.Row{pat}
	}
	return &rowIter{rows: rows}, nil
}

// refusal は、マージを拒否する理由。語（reason）と番号（num）は、プラグインが分岐に使うので、翻訳せず、足すだけで変えも
// 消しもしない（docs/procedures.md の「マージを拒否する理由」）。番号は MySQL が第三者に空けている 50000〜51999 から取る。
type refusal struct {
	reason string
	num    int
}

var (
	refuseBranchNotFound      = refusal{"branch_not_found", 51001}
	refuseNoParent            = refusal{"no_parent", 51002}
	refuseInTransaction       = refusal{"in_transaction", 51003}
	refuseUnresolvedConflicts = refusal{"unresolved_conflicts", 51004}
	refuseDBVersionDiffers    = refusal{"db_version_differs", 51005}
	refuseAmbiguousPrefix     = refusal{"ambiguous_prefix", 51006}
	refuseParentChanged       = refusal{"parent_changed", 51007}
	refuseReadOnlyReplica     = refusal{"read_only_replica", 51008}
	// refuseCoreTable は、本体の表に当たるパターンを足そうとした（heimdb_merge_exclude）。
	refuseCoreTable = refusal{"core_table", 51009}
	// refuseSchemaDiffers は、両側で定義の違う表があるので取り込まない（heimdb_branch_sync。段階 94）。
	refuseSchemaDiffers = refusal{"schema_differs", 51010}
)

// errorf は、理由の語と番号の付いたエラーを返す（HeimDB: merge refused (<語>): <説明>）。
func (r refusal) errorf(format string, args ...any) error {
	return mysql.NewSQLError(r.num, mysql.SSUnknownSQLState, "HeimDB: merge refused (%s): %s", r.reason, fmt.Sprintf(format, args...))
}

// asRefusal は、err がマージを拒否するエラーなら、包みを外して返す（マージは差分のエラーを包むので、そのままだと 1105 になる）。
func asRefusal(err error) (*mysql.SQLError, bool) {
	var sqlErr *mysql.SQLError
	if !errors.As(sql.UnwrapError(err), &sqlErr) || sqlErr.Num < 51001 || sqlErr.Num > 51999 {
		return nil, false
	}
	return sqlErr, true
}

// detectPrefixes は、親の今のテーブルから、WordPress のテーブルの接頭辞（wp-config.php の $table_prefix）の候補を返す。
// option_name 列を持つ「<接頭辞>options」と「<接頭辞>posts」がそろう接頭辞（短い順）。マルチサイトのサブサイトの組
// （マルチサイトのネットワークの表 <p>blogs を持つ候補 p に対する p + 数字 + "_"。wp_2_options など）は候補にしない。
// 数字でない wp_old_ や、<p>blogs のない wp_ と wp_2_（数字だけの接頭辞のサイトに古いインストールが残った）は候補に残す。
func detectPrefixes(st *store.Store, branch uint64) ([]string, error) {
	defs, err := st.ListTables(branch, store.Latest)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, def := range defs {
		names[strings.ToLower(def.Name)] = true
	}
	var out []string
	for _, def := range defs {
		prefix, ok := strings.CutSuffix(strings.ToLower(def.Name), "options")
		if ok && def.ColumnIndex("option_name") >= 0 && names[prefix+"posts"] {
			out = append(out, prefix)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(a) - len(b) })
	cands := slices.Clone(out) // DeleteFunc は out を書き換えながら呼ぶので、写しを見る
	return slices.DeleteFunc(out, func(q string) bool {
		return slices.ContainsFunc(cands, func(p string) bool { return names[p+"blogs"] && isSubsitePrefix(p, q) })
	}), nil
}

// isSubsitePrefix は、q がマルチサイトの接頭辞 p のサブサイトの接頭辞（p + 数字 + "_"）かどうか。
func isSubsitePrefix(p, q string) bool {
	rest, ok := strings.CutPrefix(q, p)
	digits, ok2 := strings.CutSuffix(rest, "_")
	return ok && ok2 && digits != "" && strings.Trim(digits, "0123456789") == ""
}

// excludedDiff は、外した表の差分の要約（ブランチの側で変わった行の数。反映しない）。
func (d *differ) excludedDiff(t *tableDiff, pattern string, baseB, nowB *schema.Table) error {
	t.Status = statusExcluded
	t.Detail = fmt.Sprintf("excluded by the pattern '%s'", pattern)
	pks, err := d.changedPKs(t, d.branch.ID, nowB, baseB, d.base.BranchTS)
	if err != nil {
		return err
	}
	t.FullCompare = 0 // 外した表は比べないので、重さとして知らせない
	// 行が多ければ、行ごとに点で読まず、時点ごとの走査で読む（段階 59 の rowSources。changedPKs は主キーの順に返す）。
	srcs, closeSrcs, err := d.rowSources(len(pks), []rowSide{{d.branch.ID, baseB, d.base.BranchTS}, {d.branch.ID, nowB, d.snap}})
	if err != nil {
		return err
	}
	defer closeSrcs()
	for _, pk := range pks {
		before, err := srcs[0](pk)
		if err != nil {
			return err
		}
		after, err := srcs[1](pk)
		if err != nil {
			return err
		}
		switch {
		case before == nil && after != nil:
			t.excludedCounts[0]++
		case before != nil && after == nil:
			t.excludedCounts[2]++
		case before != nil && !sameMergeRow(toMergeRow(baseB, before), toMergeRow(nowB, after)):
			t.excludedCounts[1]++
		}
	}
	return nil
}

// sameMergeRow は、2 つの行の列と値が同じかどうか。
func sameMergeRow(a, b merge.Row) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !merge.Equal(v, w) {
			return false
		}
	}
	return true
}

// notExcludedRow は、パターンに当たったが WordPress 本体の表なので外さなかった表の行（差分の要約とマージの結果）。
// 本体の表はその表の行（merged など）でふつうに出る。
func notExcludedRow(t *tableDiff) sql.Row {
	zero := int64(0)
	return sql.Row{t.Name, statusNotExcluded, zero, zero, zero, zero, zero, zero, zero,
		fmt.Sprintf("matches the pattern '%s', but WordPress core tables are always merged", t.notExcluded)}
}
