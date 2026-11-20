package backend

import (
	"regexp"
	"strings"
)

// 列を付けた DESCRIBE（docs/plan.md の段階 107）。MySQL は DESCRIBE 表 列（列は名前か、% と _ を使える文字列）で、
// その列の行だけを SHOW COLUMNS と同じ形で返す（ない列なら空）。Intuitive Custom Post Order は、無効にするときに
// DESCRIBE wp_terms `term_order` で列があるかを確かめてから列を消す。vitess の構文解析は列を知らず、構文エラーになると
// 列が残っていた。
//
// 構文解析の前に、1 つの文の DESCRIBE（DESC、EXPLAIN）表 列を SHOW COLUMNS FROM 表 LIKE '列' にする。記録だけ
// （WordPress は使わない）: 複数の文を送ったときと、エスケープのある "文字列" は書き換えない（構文エラーのまま）。
// MySQL は列の名前の大文字と小文字を区別しないが、HeimDB の SHOW COLUMNS の LIKE は区別する（プラグインは作ったときと
// 同じ名前で確かめる）。

// describeColumn は、DESCRIBE 表 列（DESC と EXPLAIN も同じ）。表は名前か `名前`（データベース名付きも）、列は名前、`名前`、'文字列'、"文字列"。
var describeColumn = regexp.MustCompile("(?is)^(\\s*)(?:DESCRIBE|DESC|EXPLAIN)\\s+((?:`(?:[^`]|``)+`|\\w+)(?:\\s*\\.\\s*(?:`(?:[^`]|``)+`|\\w+))?)\\s+(`(?:[^`]|``)+`|\\w+|'(?:[^'\\\\]|\\\\.|'')*'|\"(?:[^\"\\\\]|\\\\.|\"\")*\")\\s*;?\\s*$")

// explainWords は、DESCRIBE の後に来ると文の実行計画になる語（DESCRIBE SELECT 1、DESCRIBE FORMAT=JSON ...）。
var explainWords = map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"WITH": true, "TABLE": true, "FORMAT": true, "ANALYZE": true, "PLAN": true, "FOR": true, "EXTENDED": true, "PARTITIONS": true}

// rewriteDescribe は、文が DESCRIBE 表 列なら、SHOW COLUMNS FROM 表 LIKE '列' にした文を返す。違えば query をそのまま返す。
func rewriteDescribe(query string, ansiQuotes bool) string {
	rest := skipSpaceAndComments(query)
	if len(rest) < 7 || !strings.EqualFold(rest[:4], "DESC") && !strings.EqualFold(rest[:7], "EXPLAIN") {
		return query
	}
	m := describeColumn.FindStringSubmatch(rest)
	if m == nil || explainWords[strings.ToUpper(m[2])] {
		return query
	}
	col := m[3]
	switch {
	case col[0] == '\'':
	case col[0] == '"' && !ansiQuotes:
		// 文字列。エスケープのあるものは書き換えない（WordPress は使わない）
		inner := col[1 : len(col)-1]
		if strings.ContainsAny(inner, `\'`) || strings.Contains(inner, `""`) {
			return query
		}
		col = "'" + inner + "'"
	case col[0] == '`' || col[0] == '"':
		// 名前（ANSI_QUOTES の "名前" も）。その字をそのまま LIKE の型にする（MySQL と同じ）
		q := col[:1]
		col = "'" + strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(col[1:len(col)-1], q+q, q), `\`, `\\`), "'", "''") + "'"
	default:
		col = "'" + col + "'"
	}
	return query[:len(query)-len(rest)] + m[1] + "SHOW COLUMNS FROM " + m[2] + " LIKE " + col
}
