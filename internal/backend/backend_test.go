package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

const testDB = "wordpress"

// wpSchema は WordPress コアの CREATE TABLE 文を返す。
func wpSchema() []string {
	b, err := os.ReadFile("../testdata/wp_schema.sql")
	if err != nil {
		panic(err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "--") {
			lines = append(lines, l)
		}
	}
	var stmts []string
	for _, s := range strings.Split(strings.Join(lines, "\n"), ";\n") {
		if s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";")); s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

// server はテスト用のエンジンと、それが使うストア。
type server struct {
	st     *store.Store
	engine *sqle.Engine
	ctx    *sql.Context
}

func start(t *testing.T, dir string) *server {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pro := NewProvider(st, testDB)
	engine := NewEngine(pro)
	ctx := newContext(NewSession(sql.NewBaseSession(), pro))
	return &server{st: st, engine: engine, ctx: ctx}
}

func newContext(sess sql.Session) *sql.Context {
	ctx := sql.NewContext(context.Background(), sql.WithSession(sess))
	ctx.SetCurrentDatabase(testDB)
	return ctx
}

func (s *server) stop(t *testing.T) {
	t.Helper()
	// Engine.Close はバックグラウンド処理を止めるだけで、止めた結果の context.Canceled を返す。
	if err := s.engine.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.st.Close(); err != nil {
		t.Fatal(err)
	}
}

func query(ctx *sql.Context, engine *sqle.Engine, q string) ([]sql.Row, error) {
	_, iter, _, err := engine.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return sql.RowIterToRows(ctx, iter)
}

func (s *server) mustQuery(t *testing.T, q string) []sql.Row {
	t.Helper()
	rows, err := query(s.ctx, s.engine, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return rows
}

// snapshot は SHOW TABLES、SHOW CREATE TABLE、SHOW INDEX FROM の結果をまとめた文字列を返す。
func snapshot(t *testing.T, ctx *sql.Context, engine *sqle.Engine) string {
	t.Helper()
	var b strings.Builder
	rows, err := query(ctx, engine, "SHOW TABLES")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "table %v\n", r[0])
	}
	for _, r := range rows {
		name := r[0].(string)
		for _, q := range []string{"SHOW CREATE TABLE `" + name + "`", "SHOW INDEX FROM `" + name + "`"} {
			res, err := query(ctx, engine, q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			for _, row := range res {
				fmt.Fprintf(&b, "%s: %v\n", q, row)
			}
		}
	}
	return b.String()
}

func withoutShowIndex(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "SHOW INDEX") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// sortedLines は、SHOW CREATE TABLE の中の改行も含めて行に分け、並べ替えてつなぐ。
func sortedLines(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, ",\n", "\n"), "\n")
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("first difference at line %d:\n- %s\n+ %s", i+1, x, y)
		}
	}
	return ""
}

// 段階 1 の完了条件: WordPress コアの CREATE TABLE がすべて通り、再起動しても
// SHOW TABLES、SHOW CREATE TABLE、SHOW INDEX FROM の結果が変わらない。
func TestWordPressSchemaSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := start(t, dir)
	stmts := wpSchema()
	for _, q := range stmts {
		s.mustQuery(t, q)
	}
	if got := len(s.mustQuery(t, "SHOW TABLES")); got != len(stmts) {
		t.Fatalf("SHOW TABLES returned %d tables, want %d", got, len(stmts))
	}
	before := snapshot(t, s.ctx, s.engine)
	s.stop(t)

	s = start(t, dir)
	after := snapshot(t, s.ctx, s.engine)
	if before != after {
		t.Fatalf("schema changed across restart\n%s", diffLines(before, after))
	}

	// テーブルの削除と名前変更、インデックスの追加と削除も再起動後に残る。
	s.mustQuery(t, "DROP TABLE wp_links")
	s.mustQuery(t, "RENAME TABLE wp_signups TO wp_signups_old")
	s.mustQuery(t, "ALTER TABLE wp_posts ADD INDEX post_title_idx (post_title(50))")
	s.mustQuery(t, "ALTER TABLE wp_posts DROP INDEX post_name")
	s.mustQuery(t, "ALTER TABLE wp_users RENAME INDEX user_email TO user_email_idx")
	before = snapshot(t, s.ctx, s.engine)
	for _, want := range []string{"post_title_idx", "user_email_idx", "table wp_signups_old"} {
		if !strings.Contains(before, want) {
			t.Errorf("snapshot after DDL does not contain %q", want)
		}
	}
	for _, unwanted := range []string{"table wp_links\n", "table wp_signups\n", "KEY `post_name`"} {
		if strings.Contains(before, unwanted) {
			t.Errorf("snapshot after DDL still contains %q", unwanted)
		}
	}
	s.stop(t)

	s = start(t, dir)
	defer s.stop(t)
	after = snapshot(t, s.ctx, s.engine)
	if before != after {
		t.Fatalf("schema changed across restart after DDL\n%s", diffLines(before, after))
	}
}

// HeimDB の SHOW CREATE TABLE が、go-mysql-server の memory 実装と一致することを確かめる。
// 保存と復元の途中で情報が落ちていないことの確認になる。
// memory はインデックスを名前順に並べ、HeimDB は作った順（MySQL に近い）に並べるので、行の順序は比べない。
func TestWordPressSchemaMatchesMemory(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)

	memDB := memory.NewDatabase(testDB)
	memDB.EnablePrimaryKeyIndexes()
	pro := memory.NewDBProvider(memDB)
	memEngine := sqle.NewDefault(pro)
	memCtx := newContext(memory.NewSession(sql.NewBaseSession(), pro))

	for _, q := range wpSchema() {
		s.mustQuery(t, q)
		if _, err := query(memCtx, memEngine, q); err != nil {
			t.Fatalf("memory: %s: %v", q, err)
		}
	}
	// SHOW INDEX は本物の MySQL と比べる（dml_test.go）。memory は Sub_part を返さないので、ここでは除く。
	got := sortedLines(withoutShowIndex(snapshot(t, s.ctx, s.engine)))
	want := sortedLines(withoutShowIndex(snapshot(t, memCtx, memEngine)))
	if got != want {
		t.Fatalf("heimdb differs from memory\n%s", diffLines(want, got))
	}
}

func TestUnsupportedTablesAreRejected(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)

	// 主キーのない表は段階 72 から作れる。AUTO_INCREMENT の列があり主キーがない表と、見えない列の名前は拒否する。
	for _, q := range []string{
		"CREATE TABLE nokey (a int AUTO_INCREMENT, UNIQUE KEY a (a))",
		"CREATE TABLE hid (__heimdb_row_id int NOT NULL PRIMARY KEY)",
	} {
		if _, err := query(s.ctx, s.engine, q); err == nil {
			t.Errorf("%s: expected an error", q)
		}
	}
	if rows := s.mustQuery(t, "SHOW TABLES"); len(rows) != 0 {
		t.Errorf("rejected tables must not be created, found %v", rows)
	}
	// FULLTEXT は定義だけを扱う（段階 10、fulltext_test.go）。

}
