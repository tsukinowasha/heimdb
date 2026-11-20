package backend

import (
	"strings"
	"testing"
	"time"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// レビュー指摘 2: 復元で定義の NextIndexID が T の値に戻ると、後の CREATE INDEX が T の後に作った（復元で消えた）インデックスと同じ ID を
// 払い出し、キー空間に残った古いエントリと混ざる。GC は「定義の次のインデックス ID 以上」のエントリを消さないので、
// 古いエントリは残り続ける。
func TestReview9IndexIDNotReusedAfterRestore(t *testing.T) {
	var st *store.Store
	mainDB, _, stop := branchServer(t, func(s *store.Store) { st = s })
	defer stop()
	mustExec(t, mainDB, "CREATE TABLE wp_x (id bigint unsigned NOT NULL PRIMARY KEY, a int NOT NULL, b int NOT NULL)")
	mustExec(t, mainDB, "INSERT INTO wp_x VALUES (1, 1, 10), (2, 2, 20)")
	tAt := markLocal(t)
	mustExec(t, mainDB, "CREATE INDEX ia ON wp_x (a)")
	def, _, _ := st.GetTable(store.MainBranchID, "wp_x", store.Latest)
	iaID := def.Indexes[0].ID
	mustExec(t, mainDB, "INSERT INTO wp_x VALUES (3, 3, 30)")
	diffRows(t, mainDB, "CALL heimdb_restore('main', '"+tAt+"', 'wp_x')")
	mustExec(t, mainDB, "CREATE INDEX ib ON wp_x (b)")
	def, _, _ = st.GetTable(store.MainBranchID, "wp_x", store.Latest)
	if def.Indexes[0].ID == iaID {
		t.Errorf("index ib reuses the ID %d of the dropped index ia", iaID)
	}
	// GC の後でも、新しいインデックスでの検索が古いエントリ（復元で消えた行 3 を指す）に当たる。
	if _, err := st.GC(time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if _, err := mainDB.Query("SELECT id FROM wp_x WHERE b < 15"); err != nil && strings.Contains(err.Error(), "missing row") {
		t.Errorf("lookup through the new index: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
}
