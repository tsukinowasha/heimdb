package backend

import (
	"context"
	"strings"

	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 表の手入れの文（docs/plan.md の段階 74）。WordPress の wp-admin/maint/repair.php、WP-Optimize、Advanced Database
// Cleaner、WP-Sweep、wp-cli の wp db check / optimize / repair（mysqlcheck）が、OPTIMIZE TABLE、CHECK TABLE、
// REPAIR TABLE を流す。vitess の構文解析はこの 3 つを知らず、構文エラーになっていた。
//
// HeimDB は古いバージョンを GC が消し、ディスクは Pebble が詰めるので、作り直す必要がない。壊れの確かめは Pebble のブロックの
// チェックサムに任せる。どれも何もせず、MySQL と同じ 4 列（Table、Op、Msg_type、Msg_text）の結果を返す。
//
// 構文解析の前に、文を ANALYZE TABLE 表の並び にして（rewriteMaintenance。後ろの選択肢は除く）、表の名前、データベース、
// 権限（そのデータベースに入れるか）はエンジンの ANALYZE の処理で決める。文の種類はセッションに覚え、実行の直前
// （execBuilder）に ANALYZE の節を、結果を返す節（maintenanceNode）にする。ANALYZE TABLE そのものも同じ節で返す
// （エンジンの ANALYZE は表を全件読んで統計を作るが、HeimDB は自分の統計を使うので読まない）。
//
// MySQL との差: OPTIMIZE は InnoDB の note の行（doing recreate + analyze instead）を返さず status の OK だけを返す
// （repair.php は先頭の行の Msg_text が OK かで成否を出す）。ない表は、MySQL は Error と Operation failed の行を返すが、
// エンジンの ANALYZE の解析がエラー 1146 にする（WP Cerber の手入れの cron は単一サイトにない表にも流すが、その表の手入れが行われないだけ）。記録だけ（WordPress は
// 使わない）: バイナリプロトコルの準備（COM_STMT_PREPARE）は vitess が先に解析するので構文エラーのまま。PREPARE と
// EXECUTE では種類が落ち、analyze の結果になる。複数の文を送れる接続では、エンジンのログに残る文が ANALYZE になる。
// MySQL と違い、開いているトランザクションを暗黙にコミットしない。HeimDB の表でないもの（information_schema など）の
// Table の列は今のデータベースの名前になる。表の並びを /*! */ で包むと構文エラーになる。

// rewriteMaintenance は、文が OPTIMIZE、CHECK、REPAIR [NO_WRITE_TO_BINLOG | LOCAL] TABLE | TABLES 表の並び [選択肢]
// なら、ANALYZE TABLE 表の並び にした文と、種類（optimize、check、repair）を返す。表の並びの後ろは、; か終わりまでを
// 選択肢とみなして確かめずに捨てる（; はコメントの中も見る）。違えば query と "" を返す（構文エラーのまま）。複数の文の
// 残り（; から後）は変えない。
func rewriteMaintenance(query string, ansiQuotes bool) (string, string) {
	// 字句解析は文を丸ごと写すので、先頭の語で先に絞る（大きな INSERT で写さない）
	rest := skipSpaceAndComments(query)
	end := 0
	for end < len(rest) && isWordByte(rest[end]) {
		end++
	}
	if w := rest[:end]; !strings.EqualFold(w, "OPTIMIZE") && !strings.EqualFold(w, "CHECK") && !strings.EqualFold(w, "REPAIR") {
		return query, ""
	}
	tkn := ast.NewStringTokenizer(query)
	if ansiQuotes {
		tkn = ast.NewStringTokenizerForAnsiQuotes(query)
	}
	scan := func() int {
		for {
			if typ, _ := tkn.Scan(); typ != ast.COMMENT {
				return typ
			}
		}
	}
	var op string
	switch scan() {
	case ast.OPTIMIZE:
		op = "optimize"
	case ast.CHECK:
		op = "check"
	case ast.REPAIR:
		op = "repair"
	default:
		return query, ""
	}
	typ := scan()
	if op != "check" && (typ == ast.NO_WRITE_TO_BINLOG || typ == ast.LOCAL) {
		typ = scan()
	}
	if typ != ast.TABLE && typ != ast.TABLES {
		return query, ""
	}
	// 表の並び: 名前（db.表 も）を , で区切る。名前は識別子か、予約語でない語（data、status など。予約語は後の ANALYZE の
	// 解析が拒否する）。字句解析の位置は、次の文字の位置に 1 を足したもの（FOR の後は先読みのため OldPosition が字句の始めを
	// 指さないので、名前を読んだ直後の Position を使う）
	name := func() bool {
		typ := scan()
		return typ == ast.ID || ast.KeywordString(typ) != ""
	}
	start := tkn.Position - 1
	for {
		if !name() {
			return query, ""
		}
		end = tkn.Position - 1
		if typ = scan(); typ == '.' {
			if !name() {
				return query, ""
			}
			end = tkn.Position - 1
			typ = scan()
		}
		if typ != ',' {
			break
		}
	}
	// 選択肢（QUICK、FOR UPGRADE、USE_FRM など）は ; か終わりまで捨てる
	next := len(query)
	if i := strings.IndexByte(query[end:], ';'); i >= 0 {
		next = end + i
	}
	return "ANALYZE TABLE" + query[start:end] + query[next:], op
}

// setMaintenance は、構文解析した文の種類（rewriteMaintenance。手入れの文でなければ ""）をセッションに覚える。
func setMaintenance(ctx context.Context, op string) {
	if c, ok := ctx.(*sql.Context); ok {
		if s, ok := c.Session.(*Session); ok {
			s.maintenance = op
		}
	}
}

// maintenanceStatements は、木の中の ANALYZE TABLE の節を、セッションに覚えた種類の結果を返す節にする。
func maintenanceStatements(ctx *sql.Context, n sql.Node) (sql.Node, error) {
	n, _, err := transform.Node(n, func(node sql.Node) (sql.Node, transform.TreeIdentity, error) {
		a, ok := node.(*plan.AnalyzeTable)
		if !ok {
			return node, transform.SameTree, nil
		}
		op := "analyze"
		if s, ok := ctx.Session.(*Session); ok && s.maintenance != "" {
			op = s.maintenance
		}
		return &maintenanceNode{op: op, db: a.Db, tables: a.Tables}, transform.NewTree, nil
	})
	return n, err
}

// maintenanceNode は、表の手入れの文の結果を返す。データには触らない。
type maintenanceNode struct {
	op     string
	db     string // 今のデータベース（HeimDB の表でなければ Table の列に使う。一時テーブルは元のデータベースの名前を持つ）
	tables []sql.Table
}

var _ sql.ExecSourceRel = (*maintenanceNode)(nil)

var maintenanceSchema = sql.Schema{
	{Name: "Table", Type: types.LongText},
	{Name: "Op", Type: types.LongText},
	{Name: "Msg_type", Type: types.LongText},
	{Name: "Msg_text", Type: types.LongText},
}

func (n *maintenanceNode) Resolved() bool       { return true }
func (n *maintenanceNode) String() string       { return "heimdb.Maintenance(" + n.op + ")" }
func (n *maintenanceNode) Schema() sql.Schema   { return maintenanceSchema }
func (n *maintenanceNode) Children() []sql.Node { return nil }
func (n *maintenanceNode) IsReadOnly() bool     { return true }
func (n *maintenanceNode) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 0 {
		return nil, sql.ErrInvalidChildrenNumber.New(n, len(children), 0)
	}
	return n, nil
}

// RowIter は sql.ExecSourceRel の実装。Table の列は MySQL と同じく データベース.表（ブランチなら wordpress@staging.表）。
func (n *maintenanceNode) RowIter(ctx *sql.Context, r sql.Row) (sql.RowIter, error) {
	msgType, msgText := "status", "OK"
	if n.op == "repair" {
		// InnoDB と同じ
		msgType, msgText = "note", "The storage engine for the table doesn't support repair"
	}
	rows := make([]sql.Row, len(n.tables))
	for i, t := range n.tables {
		db := n.db
		if ht := unwrapTable(t); ht != nil {
			db = ht.db.name
		}
		rows[i] = sql.Row{db + "." + t.Name(), n.op, msgType, msgText}
	}
	return &rowIter{rows: rows}, nil
}
