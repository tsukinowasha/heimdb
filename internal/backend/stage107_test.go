package backend

import (
	"fmt"
	"testing"
)

// インデックスのある ENUM の列を、定義にない文字列（Forminator の移し替えの `status = ”`）と比べても、エラーにせず
// MySQL と同じ行を返す（docs/plan.md の段階 107）。HeimDB は定義にない値を保存しないので、= に合う行はない。
func TestStage107EnumNonMember(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE e (id bigint NOT NULL AUTO_INCREMENT, status ENUM('active','spam','draft') NOT NULL DEFAULT 'active', s2 ENUM('a','b'), PRIMARY KEY (id), KEY st (status), KEY fs (id, status), KEY s2 (s2))")
	s.mustQuery(t, "INSERT INTO e (status, s2) VALUES ('active','a'),('spam',NULL),('draft','b')")
	for _, c := range []struct{ q, want string }{
		{"SELECT id FROM e WHERE status = '' ORDER BY id", "[]"},
		{"SELECT id FROM e WHERE status = 'nope' ORDER BY id", "[]"},
		{"SELECT id FROM e WHERE (status IS NULL OR status = '') ORDER BY id", "[]"},
		{"SELECT id FROM e WHERE status IN ('', 'spam') ORDER BY id", "[[2]]"},
		{"SELECT id FROM e WHERE status <> '' ORDER BY id", "[[1] [2] [3]]"},
		{"SELECT id FROM e WHERE status = 'spam' ORDER BY id", "[[2]]"},
		{"SELECT id FROM e WHERE id = 2 AND status = '' ORDER BY id", "[]"},
		{"SELECT id FROM e IGNORE INDEX (st) WHERE status = '' ORDER BY id", "[]"},
		{"SELECT id FROM e WHERE s2 <> 'x' ORDER BY id", "[[1] [3]]"},
		{"SELECT id FROM e IGNORE INDEX (s2) WHERE s2 <> 'x' ORDER BY id", "[[1] [3]]"},
		{"SELECT id FROM e WHERE s2 = 'x' ORDER BY id", "[]"},
	} {
		if got := fmt.Sprint(s.mustQuery(t, c.q)); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
	}
	s.mustQuery(t, "UPDATE e SET `status` = 'active' WHERE (`status` IS NULL OR `status` = '')")
	// 内部では ENUM を番号で持つ（1 が active）
	if got := fmt.Sprint(s.mustQuery(t, "SELECT id, status FROM e ORDER BY id")); got != "[[1 1] [2 2] [3 3]]" {
		t.Errorf("after the update: %s", got)
	}
}

// 列を付けた DESCRIBE は、その列の行を SHOW COLUMNS と同じ形で返す（Intuitive Custom Post Order）。ない列なら空。
func TestStage107DescribeColumn(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	s.mustQuery(t, "CREATE TABLE t (id int PRIMARY KEY, term_order int, other_col int)")
	for _, c := range []struct{ q, want string }{
		{"DESCRIBE  t `term_order`", "[[term_order int YES  <nil> ]]"},
		{"DESCRIBE t term_order", "[[term_order int YES  <nil> ]]"},
		{"desc " + testDB + ".t 'term%';", "[[term_order int YES  <nil> ]]"},
		{"DESC `t` \"%_col\"", "[[other_col int YES  <nil> ]]"},
		{"DESCRIBE t `nope`", "[]"},
		{"/* x */ DESCRIBE t id", "[[id int NO PRI <nil> ]]"},
		{"DESCRIBE t", "[[id int NO PRI <nil> ] [term_order int YES  <nil> ] [other_col int YES  <nil> ]]"},
	} {
		if got := fmt.Sprint(s.mustQuery(t, c.q)); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"DESCRIBE t `a'b`", "SHOW COLUMNS FROM t LIKE 'a''b'"},
		{"DESCRIBE t `a``b\\c`", "SHOW COLUMNS FROM t LIKE 'a`b\\\\c'"},
		{"DESC t \"a'b\"", "DESC t \"a'b\""},
		{"DESC t \"a%\"", "SHOW COLUMNS FROM t LIKE 'a%'"},
		{"EXPLAIN FORMAT=JSON SELECT 1", "EXPLAIN FORMAT=JSON SELECT 1"},
		{"EXPLAIN ANALYZE SELECT 1", "EXPLAIN ANALYZE SELECT 1"},
		{"DESCRIBE t c -- x", "DESCRIBE t c -- x"},
		{"DESCRIBE IF;", "DESCRIBE IF;"},
		{"DESCRIBE t c; DESCRIBE t d", "DESCRIBE t c; DESCRIBE t d"},
		{"DESCRIBE SELECT 1", "DESCRIBE SELECT 1"},
		{"DESC PLAN SELECT", "DESC PLAN SELECT"},
		{"EXPLAIN t id", "SHOW COLUMNS FROM t LIKE 'id'"},
		{"EXPLAIN SELECT id FROM t", "EXPLAIN SELECT id FROM t"},
		{"EXPLAIN PLAN SELECT 1", "EXPLAIN PLAN SELECT 1"},
	} {
		if got := rewriteDescribe(c.in, false); got != c.want {
			t.Errorf("rewriteDescribe(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
