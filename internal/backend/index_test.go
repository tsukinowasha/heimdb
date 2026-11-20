package backend

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// checkIndexConsistency は、テーブルのキーを持つすべてのインデックスについて、エントリが行から計算したものと
// 一致することを確かめる。
func checkIndexConsistency(t *testing.T, s *server, name string) {
	t.Helper()
	db, err := s.engine.Analyzer.Catalog.Database(s.ctx, testDB)
	if err != nil {
		t.Fatal(err)
	}
	tbl, ok, err := db.GetTableInsensitive(s.ctx, name)
	if err != nil || !ok {
		t.Fatalf("table %s: %v", name, err)
	}
	table := tbl.(*Table)
	rows, err := table.scanAll(s.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ki := range table.keyIndexes() {
		var want []string
		for _, row := range rows {
			e, v, err := table.entry(ki, row)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, string(e)+"="+string(v))
		}
		slices.Sort(want)
		var got []string
		txn := s.st.Begin()
		err := txn.ScanIndex(store.MainBranchID, table.def.ID, ki.def.ID, nil, nil, false, func(entry, value []byte) (bool, error) {
			got = append(got, string(entry)+"="+string(value))
			return true, nil
		})
		txn.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s.%s: %d entries, want %d (rows %d)", name, ki.def.Name, len(got), len(want), len(rows))
		}
	}
}

func TestIndexMaintenanceRandom(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	for _, q := range wpSchema() {
		s.mustQuery(t, q)
	}
	r := rand.New(rand.NewPCG(7, 8))
	keys := []string{"_thumb", "_edit_lock", "colour", "Colour", "日本語キー", strings.Repeat("長", 200)}
	for i := 0; i < 400; i++ {
		postID := r.IntN(20)
		key := keys[r.IntN(len(keys))]
		switch r.IntN(6) {
		case 0, 1:
			s.mustQuery(t, fmt.Sprintf("INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (%d, '%s', 'v%d')", postID, key, i))
		case 2:
			s.mustQuery(t, fmt.Sprintf("UPDATE wp_postmeta SET meta_key = '%s' WHERE post_id = %d", key, postID))
		case 3:
			s.mustQuery(t, fmt.Sprintf("UPDATE wp_postmeta SET post_id = %d WHERE meta_key = '%s' AND post_id = %d", r.IntN(20), key, postID))
		case 4:
			s.mustQuery(t, fmt.Sprintf("DELETE FROM wp_postmeta WHERE post_id = %d", postID))
		case 5:
			// ロールバックした変更はエントリにも残らない。
			s.mustQuery(t, "START TRANSACTION")
			s.mustQuery(t, fmt.Sprintf("UPDATE wp_postmeta SET meta_key = 'rolled_back' WHERE post_id = %d", postID))
			s.mustQuery(t, fmt.Sprintf("INSERT INTO wp_postmeta (post_id, meta_key) VALUES (%d, 'rolled_back')", postID))
			s.mustQuery(t, "ROLLBACK")
		}
	}
	checkIndexConsistency(t, s, "wp_postmeta")
	if got := rowsString(t, s, "SELECT COUNT(*) FROM wp_postmeta WHERE meta_key = 'rolled_back'"); got != "[0]\n" {
		t.Errorf("rolled back rows visible: %s", got)
	}

	// インデックス経由の検索と全件走査の結果が一致する。
	for _, key := range keys {
		viaIndex := rowsString(t, s, "SELECT meta_id FROM wp_postmeta WHERE meta_key = '"+key+"' ORDER BY meta_id")
		full := rowsString(t, s, "SELECT meta_id FROM wp_postmeta IGNORE INDEX (meta_key) WHERE meta_key = '"+key+"' ORDER BY meta_id")
		if viaIndex != full {
			t.Errorf("meta_key = %q: index %q, full scan %q", key, viaIndex, full)
		}
	}
	for id := 0; id < 20; id++ {
		viaIndex := rowsString(t, s, fmt.Sprintf("SELECT meta_id FROM wp_postmeta WHERE post_id = %d ORDER BY meta_id DESC", id))
		full := rowsString(t, s, fmt.Sprintf("SELECT meta_id FROM wp_postmeta IGNORE INDEX (post_id) WHERE post_id = %d ORDER BY meta_id DESC", id))
		if viaIndex != full {
			t.Errorf("post_id = %d: index %q, full scan %q", id, viaIndex, full)
		}
	}
}

func TestIndexLookupsAndCreateIndex(t *testing.T) {
	s := start(t, t.TempDir())
	defer s.stop(t)
	for _, q := range wpSchema() {
		s.mustQuery(t, q)
	}
	s.mustQuery(t, "INSERT INTO wp_users (ID, user_login, user_pass, user_nicename, user_email, user_url, user_registered, user_activation_key, display_name) VALUES "+
		"(1, 'alice', 'x', 'alice', 'alice@example.com', '', '2026-10-01 00:00:00', '', 'Alice'),"+
		"(2, 'bob', 'x', 'bob', 'Bob@Example.com', '', '2026-10-02 00:00:00', '', 'Bob')")
	s.mustQuery(t, "SET time_zone = '+00:00'") // mark は UTC の時刻を返す
	before := mark(t)
	s.mustQuery(t, "UPDATE wp_users SET user_email = 'robert@example.com' WHERE ID = 2")

	check := func(q, want string) {
		t.Helper()
		if got := rowsString(t, s, q); got != want {
			t.Errorf("%s\n got: %q\nwant: %q", q, got, want)
		}
	}
	// 照合順序（大文字と小文字を区別しない）での検索
	check("SELECT ID FROM wp_users WHERE user_email = 'ROBERT@example.com'", "[2]\n")
	check("SELECT ID FROM wp_users WHERE user_email = 'bob@example.com'", "")
	check("SELECT ID FROM wp_users AS OF '"+before+"' WHERE user_email = 'bob@example.com'", "[2]\n")
	check("SELECT user_login FROM wp_users WHERE user_login >= 'B' ORDER BY user_login", "[bob]\n")

	// インデックスが実際に使われている
	plan := rowsString(t, s, "EXPLAIN PLAN SELECT ID FROM wp_users WHERE user_email = 'x'")
	if !strings.Contains(plan, "wp_users.user_email") {
		t.Errorf("user_email index not used:\n%s", plan)
	}

	// 既存の行があるテーブルにインデックスを足すと、その行もインデックスで引ける。
	s.mustQuery(t, "ALTER TABLE wp_users ADD INDEX display_name_idx (display_name(3))")
	checkIndexConsistency(t, s, "wp_users")
	check("SELECT ID FROM wp_users WHERE display_name = 'alice' ORDER BY ID", "[1]\n")
	check("SELECT display_name FROM wp_users WHERE display_name > 'A' ORDER BY display_name DESC", "[Bob]\n[Alice]\n")
	s.mustQuery(t, "ALTER TABLE wp_users DROP INDEX display_name_idx")
	check("SELECT ID FROM wp_users WHERE display_name = 'Bob'", "[2]\n")
}

// scanAll は、テーブルの全行を主キーの順に集めて返す（すべての列を復号する）。
func (t *Table) scanAll(ctx *sql.Context) ([]sql.Row, error) {
	var rows []sql.Row
	err := t.eachRow(ctx, nil, nil, false, nil, func(row sql.Row) error {
		rows = append(rows, row)
		return nil
	})
	return rows, err
}
