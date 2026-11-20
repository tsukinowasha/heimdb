package backend

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// HEIMDB_TEST_CURSOR_MIN_ROWS を指定すると、マージと復元が行を走査で読み、マージが行の値を持たずに読み直す
// しきい値（cursorMinRows）をその値にしてテストを流す（docs/plan.md の段階 59。全体を 1 で流して確かめる）。
func init() {
	if s := os.Getenv("HEIMDB_TEST_CURSOR_MIN_ROWS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			cursorMinRows = n
		}
	}
}

// 大きなマージと復元の経路（走査で読む、行の値を持たずに読み直す）を、しきい値を 1 にして、マージと復元の
// テストで確かめる。
func TestMergeAndRestoreWithCursors(t *testing.T) {
	defer func(n int) { cursorMinRows = n }(cursorMinRows)
	cursorMinRows = 1
	for name, fn := range map[string]func(*testing.T){
		"BranchMerge": TestBranchMerge, "BranchMergeConflicts": TestBranchMergeConflicts, "ColumnMerge": TestColumnMerge,
		"ColumnMergeConflicts": TestColumnMergeConflicts, "ColumnMergeParentOnlyColumn": TestColumnMergeParentOnlyColumn,
		"MergeUndo": TestMergeUndo, "MergeDryRun": TestMergeDryRun, "IndexMerge": TestIndexMerge,
		"Restore": TestRestore, "RestoreSelectedTablesAndAll": TestRestoreSelectedTablesAndAll, "RestoreBranch": TestRestoreBranch,
		"RestoreRecountsComments": TestRestoreRecountsComments, "Review8RestoreUniqueSwap": TestReview8RestoreUniqueSwap,
		"Review8RestoreBranchRebuildsIndex": TestReview8RestoreBranchRebuildsIndex, "Review8RestoreCommentsOnlyRecounts": TestReview8RestoreCommentsOnlyRecounts,
		"Review8RestoreParentThenMergeChild": TestReview8RestoreParentThenMergeChild, "MergeMemoryLimit": TestMergeMemoryLimit,
		"RestoreMemoryLimit": TestRestoreMemoryLimit,
	} {
		t.Run(name, fn)
	}
}

// ブランチを分かれる前の時点に戻すと、変わった行はブランチと親の両方の層にある。走査で読んでも、両方の層の行を戻す
// （変わった行の一覧は層をまたいで主キーの順に返る）。
func TestRestoreBranchAcrossLayersWithCursors(t *testing.T) {
	defer func(n int) { cursorMinRows = n }(cursorMinRows)
	cursorMinRows = 1
	mainDB, dsn, stop := branchServer(t)
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE r (id bigint NOT NULL PRIMARY KEY, v varchar(20) NOT NULL)")
	var vals []string
	for i := 1; i <= 20; i++ {
		vals = append(vals, fmt.Sprintf("(%d, 'v%d')", i, i))
	}
	mustExec(t, mainDB, "INSERT INTO r VALUES "+strings.Join(vals, ","))
	at := timeArg()
	mustExec(t, mainDB, "UPDATE r SET v = 'main' WHERE id % 2 = 0")
	mustExec(t, mainDB, "CALL heimdb_branch_create('layers')")
	b := openBranch(t, dsn, "layers")
	defer b.Close()
	mustExec(t, b, "UPDATE r SET v = 'branch' WHERE id % 2 = 1")
	mustExec(t, b, "DELETE FROM r WHERE id = 1")
	mustExec(t, b, "INSERT INTO r VALUES (0, 'new')")
	mustExec(t, b, "CALL heimdb_restore('layers', '"+at+"', 'r')")
	if got := one(t, b, "SELECT CONCAT(COUNT(*), '/', SUM(v = CONCAT('v', id))) FROM r"); got != "20/20" {
		t.Errorf("rows after restoring across layers: %s", got)
	}
}

// rowCursor は主キーの昇順でしか引けない。逆に引くと、読めない行をないものとして扱わずにエラーにする。
func TestRowCursorRejectsOutOfOrder(t *testing.T) {
	c, err := (&differ{}).openCursor(0, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pk := range []string{"a", "b", "b"} {
		if _, err := c.get([]byte(pk)); err != nil {
			t.Fatal(pk, err)
		}
	}
	if _, err := c.get([]byte("a")); err == nil || !strings.Contains(err.Error(), "out of primary key order") {
		t.Errorf("reading backwards: %v", err)
	}
}
