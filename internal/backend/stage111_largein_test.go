package backend

import (
	"fmt"
	"strings"
	"testing"
	"time"

	gsql "github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

func init() {
	// エンジンの範囲の木の不具合（重なった範囲）を、HeimDB のテストでも見つける（本番は 128 個までだけ確かめる）
	gsql.SetFullRangeValidation(true)
}

// 大きな IN の DELETE の解析が、範囲の数の 2 乗の確かめで書き込みのロックを持ち続けない（段階 111、上流の #4106）。
// 前は 2 万個で SELECT が 6.0 秒、DELETE が 4.8 秒かかった。
func TestStage111LargeInList(t *testing.T) {
	gsql.SetFullRangeValidation(false)
	defer gsql.SetFullRangeValidation(true)
	addr, _, st, srv := serve(t, t.TempDir(), serverConfig{})
	defer stopServer(t, st, srv)()
	dsn := fmt.Sprintf("root@tcp(%s)/%s", addr, testDB)
	var b strings.Builder
	for i := 1; i <= 40000; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprint(&b, i)
	}
	in := b.String()
	runScript(t, dsn, []string{
		"CREATE TABLE t (a INT, b INT, c INT, PRIMARY KEY (a, b))",
		"INSERT INTO t VALUES (1,1,1),(2,2,2),(39999,1,1),(50000,1,1)",
	})
	// 前の 2 乗の確かめは 5 秒前後かかったので、2 秒で見分けられる。-race ではコードが数倍遅くなるので 10 秒にする
	// （CI の -race で 2 秒を超えることがあった。段階 114）
	limit := 2 * time.Second
	if raceEnabled {
		limit = 10 * time.Second
	}
	for _, q := range []string{"SELECT COUNT(*) FROM t WHERE a IN (" + in + ")", "DELETE FROM t WHERE a IN (" + in + ")"} {
		start := time.Now()
		got := runScript(t, dsn, []string{q})[0]
		if d := time.Since(start); d > limit {
			t.Errorf("%s...: %s", q[:40], d)
		}
		t.Logf("%s...: %s %q", q[:40], time.Since(start), got)
	}
	if got := runScript(t, dsn, []string{"SELECT a FROM t"})[0]; got != "50000\n" {
		t.Errorf("rows after DELETE: %q", got)
	}
}
