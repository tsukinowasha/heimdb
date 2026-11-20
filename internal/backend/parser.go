package backend

import (
	"context"
	"fmt"
	"strings"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// likeEscapeParser は、構文解析の前に、文字列の中の \_ と \% の \ を 2 つにする（docs/plan.md の段階 11）。
//
// MySQL は文字列の中の \_ と \% の \ を残す（LIKE のパターンで _ と % をエスケープできるように）が、vitess の構文解析は
// 未知のエスケープの \ を落とす。wpdb::prepare を通らない、\ が 1 つの LIKE のパターン（'wp\_%' など）が MySQL と
// 違う結果になる。\\_ にしておけば、構文解析の後の値は \_ になる。何度通しても同じ結果になる。
type likeEscapeParser struct {
	sql.Parser
}

var _ sql.Parser = likeEscapeParser{}

// recoverParsePanic は、vitess（フォークしていない）の構文解析器の panic を構文のエラーにする（docs/plan.md の段階 105）。
// SELECT のすぐ後に空の文字列を書くと、選択の式の元の字を取る処理（walkSelectExpressions）が引用符を 1 文字だけ
// 切り出して panic する。文の panic の回復でも接続は保たれるが、internal error ではなく、その文を解析できないことを
// 返す。vitess の呼び出しだけを包む（HeimDB の書き換えの panic は、不具合として internal error のままにする）。
// defer で直接呼ぶ。
func recoverParsePanic(err *error) {
	if r := recover(); r != nil {
		*err = sql.ErrSyntaxError.New(fmt.Sprintf("HeimDB: cannot parse the statement (%v)", r))
	}
}

func (p likeEscapeParser) ParseSimple(query string) (ast.Statement, error) {
	q, do := rewriteQuery(query, false)
	var stmt ast.Statement
	err := func() (err error) {
		defer recoverParsePanic(&err)
		stmt, err = p.Parser.ParseSimple(q)
		return err
	}()
	if err == nil {
		stmt, err = checkStatement(stmt, query, do)
	}
	return stmt, err
}

func (p likeEscapeParser) Parse(ctx *sql.Context, query string, multi bool) (ast.Statement, string, string, error) {
	rq, op := rewriteMaintenance(query, sql.LoadSqlMode(ctx).AnsiQuotes())
	setMaintenance(ctx, op)
	rq, do := rewriteQuery(rq, sql.LoadSqlMode(ctx).AnsiQuotes())
	var stmt ast.Statement
	var q, rest string
	err := func() (err error) {
		defer recoverParsePanic(&err)
		stmt, q, rest, err = p.Parser.Parse(ctx, rq, multi)
		return err
	}()
	if err == nil {
		stmt, err = checkStatement(stmt, statementText(query, q), do)
	}
	return stmt, q, rest, err
}

func (p likeEscapeParser) ParseWithOptions(ctx context.Context, query string, delimiter rune, multi bool, options ast.ParserOptions) (ast.Statement, string, string, error) {
	rq, op := rewriteMaintenance(query, options.AnsiQuotes)
	setMaintenance(ctx, op)
	rq, do := rewriteQuery(rq, options.AnsiQuotes)
	var stmt ast.Statement
	var q, rest string
	err := func() (err error) {
		defer recoverParsePanic(&err)
		stmt, q, rest, err = p.Parser.ParseWithOptions(ctx, rq, delimiter, multi, options)
		return err
	}()
	if err == nil {
		stmt, err = checkStatement(stmt, statementText(query, q), do)
	}
	return stmt, q, rest, err
}

// ParseOneWithOptions は sql.Parser の実装。返す位置は書き換えた文の中の位置になるが、go-mysql-server はこの位置を
// 使わない（engine.go）。
func (p likeEscapeParser) ParseOneWithOptions(ctx context.Context, query string, options ast.ParserOptions) (ast.Statement, int, error) {
	q, op := rewriteMaintenance(query, options.AnsiQuotes)
	setMaintenance(ctx, op)
	q, do := rewriteQuery(q, options.AnsiQuotes)
	var stmt ast.Statement
	var n int
	err := func() (err error) {
		defer recoverParsePanic(&err)
		stmt, n, err = p.Parser.ParseOneWithOptions(ctx, q, options)
		return err
	}()
	if err == nil {
		stmt, err = checkStatement(stmt, q[:min(max(n, 0), len(q))], do)
	}
	return stmt, n, err
}

// checkStatement は、構文解析の直後に、HeimDB が受け付けない文を明示的なエラーにする。エラーは、複数の文を送れる
// 接続でも、エンジン（server/handler.go の doQuery）が見て、その文と残りの文を実行しない（docs/plan.md の段階 42。
// それまでは、エラーのときに文を返さないことで防いでいた）。text は文の字句（複数の文なら、その文だけ）。do は、
// 文が DO だったか（rewriteDo）。
func checkStatement(stmt ast.Statement, text string, do bool) (ast.Statement, error) {
	if do {
		return stmt, markDo(stmt)
	}
	markUnspecifiedAuth(stmt, text)
	return stmt, checkPrimaryKeyPrefix(stmt)
}

// checkPrimaryKeyPrefix は、CREATE TABLE の主キーに前方の長さ（PRIMARY KEY (k(10))）があれば明示的なエラーにする
// （docs/plan.md の段階 38）。go-mysql-server は、HeimDB が sql.IndexedTableCreator を実装しないので長さを捨てて渡し、
// 列全体の主キーになってしまう（MySQL は先頭の文字だけで一意を判定する）。主キーの変更は ALTER では受け付けない。
func checkPrimaryKeyPrefix(stmt ast.Statement) error {
	ddl, ok := stmt.(*ast.DDL)
	if !ok || ddl.TableSpec == nil {
		return nil
	}
	for _, idx := range ddl.TableSpec.Indexes {
		if idx.Info == nil || !idx.Info.Primary {
			continue
		}
		for _, c := range idx.Columns {
			if c.Length != nil {
				return fmt.Errorf("HeimDB: a prefix length on primary key column %s is not supported", c.Column.String())
			}
		}
	}
	return nil
}

// rewriteQuery は、構文解析の前に文を書き換える（rewriteDo、rewriteDescribe、keepLikeEscapes と unquoteEngine）。文が DO
// なら true を返す。
func rewriteQuery(query string, ansiQuotes bool) (string, bool) {
	query, do := rewriteDo(query)
	query = rewriteDescribe(query, ansiQuotes)
	return unquoteEngine(keepLikeEscapes(query, ansiQuotes), ansiQuotes), do
}

// unquoteEngine は、CREATE TABLE と ALTER TABLE の表のオプションの、引用符で囲んだエンジン名（ENGINE='InnoDB'、
// ENGINE "InnoDB"）の引用符をバッククォートにする（docs/plan.md の段階 57）。MySQL は受け付けるが、vitess の構文解析は
// エンジン名に識別子しか受け付けない（WP Mail SMTP が ENGINE='InnoDB' で表を作る）。書き換えるのは、括弧の外の ENGINE
// の後の英数字だけの名前で、引用符の 1 文字ずつを替えるので文の長さと位置は変わらない。SELECT の条件
// （WHERE engine = 'InnoDB'）、CREATE TABLE ... SELECT の SELECT から後、列の定義、文字列と識別子とコメントの中は
// 変えない。変えるものがなければ query をそのまま返す。
func unquoteEngine(query string, ansiQuotes bool) string {
	if !strings.Contains(strings.ToUpper(query), "ENGINE") {
		return query
	}
	var b []byte // 書き換えたときだけ写す
	n := len(query)
	words, depth := 0, 0    // 文の始めからの語の数と、括弧の深さ
	first, ddl := "", false // 文の最初の語と、表のオプションを書き換える文（CREATE / ALTER ... TABLE）か
	for i := 0; i < n; {
		c := query[i]
		switch {
		case c == '\'' || (c == '"' && !ansiQuotes):
			i = skipQuoted(query, i, true)
		case c == '`' || (c == '"' && ansiQuotes):
			i = skipQuoted(query, i, false)
		case c == '#' || (c == '-' && strings.HasPrefix(query[i:], "-- ")):
			if end := strings.IndexByte(query[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = n
			}
		case c == '/' && strings.HasPrefix(query[i:], "/*") && !strings.HasPrefix(query[i:], "/*!"):
			if end := strings.Index(query[i+2:], "*/"); end >= 0 {
				i += end + 4
			} else {
				i = n
			}
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
		case c == ';' && depth <= 0:
			words, depth, first, ddl = 0, 0, "", false
			i++
		case isWordByte(c) && (i == 0 || !isWordByte(query[i-1])):
			end := i
			for end < n && isWordByte(query[end]) {
				end++
			}
			word := strings.ToUpper(query[i:end])
			words++
			switch {
			case words == 1:
				first = word
			case words <= 3 && word == "TABLE" && (first == "CREATE" || first == "ALTER"):
				ddl = true
			case ddl && depth == 0 && word == "SELECT":
				ddl = false
			case ddl && depth == 0 && word == "ENGINE":
				if open, close := quotedEngineName(query, end, ansiQuotes); open > 0 {
					if b == nil {
						b = []byte(query)
					}
					b[open], b[close] = '`', '`'
					end = close + 1
				}
			}
			i = end
		default:
			i++
		}
	}
	if b == nil {
		return query
	}
	return string(b)
}

// quotedEngineName は、ENGINE の直後 query[i:] の「[=] '名前'」の引用符の位置を返す。名前が英数字と _ だけでなければ
// 0, 0。
func quotedEngineName(query string, i int, ansiQuotes bool) (open, close int) {
	skip := func() {
		for i < len(query) && (query[i] == ' ' || query[i] == '\t' || query[i] == '\n' || query[i] == '\r') {
			i++
		}
	}
	skip()
	if i < len(query) && query[i] == '=' {
		i++
		skip()
	}
	if i >= len(query) || !(query[i] == '\'' || (query[i] == '"' && !ansiQuotes)) {
		return 0, 0
	}
	q := query[i]
	j := i + 1
	for j < len(query) && isWordByte(query[j]) {
		j++
	}
	if j == i+1 || j >= len(query) || query[j] != q {
		return 0, 0
	}
	return i, j
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c >= 0x80
}

// skipQuoted は、query[i] で始まる文字列（literal なら \ のエスケープを読む）か識別子の、閉じた次の位置を返す。
// 引用符を 2 つ重ねたものは引用符そのもの。
func skipQuoted(query string, i int, literal bool) int {
	q := query[i]
	for i++; i < len(query); i++ {
		switch {
		case literal && query[i] == '\\':
			i++
		case query[i] == q:
			if i+1 < len(query) && query[i+1] == q {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(query)
}

// keepLikeEscapes は、文字列のリテラルの中の \_ と \% を \\_ と \\% にする。識別子（`...`、ANSI_QUOTES なら "..."）と
// コメントの中は変えない。変えるものがなければ query をそのまま返す。
func keepLikeEscapes(query string, ansiQuotes bool) string {
	if !strings.Contains(query, `\_`) && !strings.Contains(query, `\%`) {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := len(query)
	for i := 0; i < n; {
		c := query[i]
		switch {
		case c == '\'' || (c == '"' && !ansiQuotes):
			// 文字列のリテラル
			b.WriteByte(c)
			i++
			for i < n {
				d := query[i]
				if d == '\\' && i+1 < n {
					if next := query[i+1]; next == '_' || next == '%' {
						b.WriteString(`\\`)
						b.WriteByte(next)
					} else {
						b.WriteByte(d)
						b.WriteByte(next)
					}
					i += 2
					continue
				}
				b.WriteByte(d)
				i++
				if d == c {
					if i < n && query[i] == c { // '' は ' そのもの
						b.WriteByte(c)
						i++
						continue
					}
					break
				}
			}
		case c == '`' || (c == '"' && ansiQuotes):
			// 識別子
			end := i + 1
			for end < n {
				if query[end] == c {
					if end+1 < n && query[end+1] == c {
						end += 2
						continue
					}
					end++
					break
				}
				end++
			}
			b.WriteString(query[i:end])
			i = end
		case c == '#' || (c == '-' && strings.HasPrefix(query[i:], "-- ")):
			// 行末までのコメント
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = n - i
			}
			b.WriteString(query[i : i+end])
			i += end
		case c == '/' && strings.HasPrefix(query[i:], "/*") && !strings.HasPrefix(query[i:], "/*!"):
			// コメント（/*! ... */ は MySQL が中身を実行するので、ふつうの文として扱う）
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				end = n - i - 2
			} else {
				end += 2
			}
			b.WriteString(query[i : i+2+end])
			i += 2 + end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// statementText は、構文解析した文の字句を返す。複数の文を解析したときは、その文だけ（q）。
func statementText(query, q string) string {
	if q != "" {
		return q
	}
	return query
}

// markUnspecifiedAuth は、IDENTIFIED のない ALTER USER（WITH MAX_USER_CONNECTIONS だけなど）の認証を nil にする
// （docs/plan.md の段階 45）。vitess の構文木は、IDENTIFIED のないときも、空の文字列の IDENTIFIED BY（空のパスワードに
// する）のときも同じ空の Authentication になるので、文の字句で区別する。エンジンは nil ならパスワードを変えない。
func markUnspecifiedAuth(stmt ast.Statement, text string) {
	ddl, ok := stmt.(*ast.DDL)
	if !ok || ddl.Action != ast.AlterStr || ddl.User.IsEmpty() || ddl.Authentication == nil {
		return
	}
	tkn := ast.NewStringTokenizer(text)
	for {
		typ, _ := tkn.Scan()
		if typ == ast.IDENTIFIED {
			return
		}
		if typ == 0 || typ == ast.LEX_ERROR {
			break
		}
	}
	ddl.Authentication = nil
}
