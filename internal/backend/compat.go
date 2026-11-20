package backend

import (
	"regexp"
	"strings"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 本物の MySQL との小さな差を直すルール（docs/plan.md の段階 11）。

// setNamesCollateId は HeimDB が足す解析ルールの ID。
const setNamesCollateId analyzer.RuleId = 10007

// setNamesCollatePattern は、SET NAMES <文字セット> COLLATE <照合順序>（SET の変数の並びの中の NAMES も含む）の
// 文字セットと照合順序。maskLiteralsAndComments で文字列の中身とコメントを潰した文に当てる。
var setNamesCollatePattern = regexp.MustCompile("(?is)(?:\\bSET\\s+|,\\s*)NAMES\\s+['\"`]?(\\w+)['\"`]?\\s+COLLATE\\s+['\"`]?(\\w+)['\"`]?")

// setNamesCollate は、SET NAMES <文字セット> COLLATE <照合順序> の照合順序を collation_connection に設定する。
// go-mysql-server（と vitess の構文解析）は COLLATE を捨て、collation_connection を文字セットの既定の照合順序にする
// （sql/planbuilder/set.go の TODO）。WordPress は接続のたびに SET NAMES utf8mb4 COLLATE utf8mb4_unicode_520_ci を送る。
// 文を読み直して照合順序を取り出し、文字セットの設定の後に collation_connection の設定を足す。
func setNamesCollate(ctx *sql.Context, _ *analyzer.Analyzer, n sql.Node, _ *plan.Scope, _ analyzer.RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	set, ok := n.(*plan.Set)
	if !ok || !setsCharacterSetClient(set) {
		return n, transform.SameTree, nil
	}
	// 文字列とコメントの中の NAMES ... COLLATE には反応しない。値は元の文から同じ位置で取り出す（引用符で囲んだ
	// 文字セットと照合順序も、潰した文では同じ長さの x になる）。
	q := ctx.Query()
	idx := setNamesCollatePattern.FindStringSubmatchIndex(maskLiteralsAndComments(q))
	if idx == nil {
		return n, transform.SameTree, nil
	}
	m := []string{q[idx[0]:idx[1]], q[idx[2]:idx[3]], q[idx[4]:idx[5]]}
	// 文字セットに合わない照合順序は、MySQL と同じくエラー（1253）にする。名前は MySQL と同じ正式な名前にする
	// （utf8_general_ci は utf8mb3_general_ci）。
	collation, err := sql.ParseCollation(m[1], m[2], false)
	if err != nil {
		return nil, transform.SameTree, mysql.NewSQLError(1253, "42000", "COLLATION '%s' is not valid for CHARACTER SET '%s'", m[2], m[1])
	}
	exprs := append(set.Exprs[:len(set.Exprs):len(set.Exprs)],
		expression.NewSetField(
			expression.NewSystemVar("collation_connection", sql.GetMysqlScope(sql.SystemVariableScope_Session), ""),
			expression.NewLiteral(collation.Name(), types.LongText)))
	return plan.NewSet(exprs), transform.NewTree, nil
}

// setsCharacterSetClient は、SET 文が character_set_client を設定するか（go-mysql-server は SET NAMES を
// character_set_client、character_set_connection、character_set_results の設定に展開する）を返す。
func setsCharacterSetClient(set *plan.Set) bool {
	for _, e := range set.Exprs {
		sf, ok := e.(*expression.SetField)
		if !ok {
			continue
		}
		if v, ok := sf.LeftChild.(*expression.SystemVar); ok && strings.EqualFold(v.Name, "character_set_client") {
			return true
		}
	}
	return false
}

// maskLiteralsAndComments は、文字列のリテラルの中身を x に、コメントを空白にした、同じ長さの文を返す。
func maskLiteralsAndComments(q string) string {
	b := []byte(q)
	n := len(b)
	for i := 0; i < n; {
		switch c := b[i]; {
		case c == '\'' || c == '"':
			i++
			for i < n {
				if b[i] == '\\' && i+1 < n {
					b[i], b[i+1] = 'x', 'x'
					i += 2
					continue
				}
				if b[i] == c {
					if i+1 < n && b[i+1] == c {
						b[i], b[i+1] = 'x', 'x'
						i += 2
						continue
					}
					i++
					break
				}
				b[i] = 'x'
				i++
			}
		case c == '`':
			for i++; i < n && b[i] != '`'; i++ {
			}
			i++
		case c == '#' || (c == '-' && i+2 < n && b[i+1] == '-' && b[i+2] == ' '):
			for ; i < n && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case c == '/' && i+1 < n && b[i+1] == '*':
			for ; i < n; i++ {
				if b[i] == '*' && i+1 < n && b[i+1] == '/' {
					b[i], b[i+1] = ' ', ' '
					i += 2
					break
				}
				b[i] = ' '
			}
		default:
			i++
		}
	}
	return string(b)
}
