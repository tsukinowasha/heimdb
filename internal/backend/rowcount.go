package backend

import (
	"sync"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

var _ sql.StatisticsTable = (*Table)(nil)

// RowCount は sql.StatisticsTable の実装。go-mysql-server が JOIN の方法と順序、インデックスの選び方の見積もりに
// 使う（coster.go）。数え直しは重いので、前に数えた値に、その後の挿入と削除の数が十分に少ないうちはそれを返す。
// 正確でない（exact が false）ので、go-mysql-server は COUNT(*) の置き換えには使わない。
func (t *Table) RowCount(ctx *sql.Context) (uint64, bool, error) {
	n, err := rowCounts.get(ctx, t)
	return n, false, err
}

// DataLength は sql.StatisticsTable の実装。見積もりには使わないので 0 を返す。
func (t *Table) DataLength(*sql.Context) (uint64, error) { return 0, nil }

// rowCountKey は、行数を覚えるテーブル（ストア、ブランチ、テーブルの ID）。
type rowCountKey struct {
	st     *store.Store
	branch uint64
	table  uint64
}

type rowCountEntry struct {
	count   uint64
	changes uint64 // 数えた後の挿入と削除の数（取り消されたものも数える）
	// stats は、インデックスごとの統計（statistics.go）。数え直すと捨て、次に要るときに作る。statsVersion は、作ったときの
	// テーブルの定義のバージョン（インデックスを足したり消したりすると統計を作り直す。一部のインデックスにだけ統計があると、
	// go-mysql-server はその表の統計をすべて使わない）。
	stats        []sql.Statistic
	prefixes     *prefixSample
	statsBuilt   bool
	statsVersion uint64
}

// rowCountCache は、見積もりに使うテーブルの行数を覚える。数え直すのは、数えた後の挿入と削除が
// 行数の 4 分の 1 と 64 の和を超えたとき（数える手間は書き込み 1 回あたりで一定になる）。時刻では数え直さない
// （同じ文の並びなら同じ実行計画になる）。
type rowCountCache struct {
	mu      sync.Mutex
	entries map[rowCountKey]*rowCountEntry
	// building は、表ごとに、統計を作る間だけ持つロック。同じ表を同時に読み始めた文と裏での作成（WarmStats）が、
	// 大きな表を何度も読まないようにする（段階 80）。
	building map[rowCountKey]*sync.Mutex
}

var rowCounts = &rowCountCache{entries: map[rowCountKey]*rowCountEntry{}, building: map[rowCountKey]*sync.Mutex{}}

func (c *rowCountCache) get(ctx *sql.Context, t *Table) (uint64, error) {
	key := rowCountKey{t.db.st, t.db.branch, t.def.ID}
	c.mu.Lock()
	e, ok := c.entries[key]
	if ok && e.fresh() {
		n := e.count
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	// 数え直すときは統計も作る（同じ 1 回の走査で数える。続く見積もりで統計も要るので、表を 2 度読まない。段階 80）。
	n, _, err := c.build(ctx, t)
	return n, err
}

// stats は、テーブル t のインデックスごとの統計を返す。行数と同じく、数えた後の挿入と削除が多くなったら作り直す。
// テーブルの定義が変わったときも作り直す。
func (c *rowCountCache) stats(ctx *sql.Context, t *Table) ([]sql.Statistic, error) {
	key := rowCountKey{t.db.st, t.db.branch, t.def.ID}
	c.mu.Lock()
	e, ok := c.entries[key]
	if ok && e.fresh() && e.statsBuilt && e.statsVersion == t.def.Version {
		s := e.stats
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()
	_, s, err := c.build(ctx, t)
	return s, err
}

// prefixes は、テーブル t の統計の標本の、インデックスの先頭の列の組のハッシュを返す（statistics.go の prefixSample）。
// 統計をまだ作っていなければ作らずに nil を返す（見積もりの途中で表を読まない。統計は、インデックスの選び方の
// 見積もりで先に作られている）。
func (c *rowCountCache) prefixes(t *Table) *prefixSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[rowCountKey{t.db.st, t.db.branch, t.def.ID}]
	if ok && e.fresh() && e.statsBuilt && e.statsVersion == t.def.Version {
		return e.prefixes
	}
	return nil
}

// build は、テーブル t の行数と統計を作って覚える。同じ表を作っている間は待ち、待った後に新しいものがあればそれを返す。
func (c *rowCountCache) build(ctx *sql.Context, t *Table) (uint64, []sql.Statistic, error) {
	key := rowCountKey{t.db.st, t.db.branch, t.def.ID}
	if s, ok := ctx.Session.(*Session); ok {
		// 文が統計を作るか、作り終えるのを待った時間（遅い文のログと指標。段階 81）
		start := time.Now()
		defer func() { s.cmdStatsWait += time.Since(start) }()
	}
	c.mu.Lock()
	m, ok := c.building[key]
	if !ok {
		m = &sync.Mutex{}
		c.building[key] = m
	}
	c.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && e.fresh() && e.statsBuilt && e.statsVersion == t.def.Version {
		c.mu.Unlock()
		return e.count, e.stats, nil
	}
	c.mu.Unlock()
	n, s, p, err := buildStats(ctx, t)
	if err != nil {
		return 0, nil, err
	}
	c.mu.Lock()
	c.entries[key] = &rowCountEntry{count: n, stats: s, prefixes: p, statsBuilt: true, statsVersion: t.def.Version}
	c.mu.Unlock()
	return n, s, nil
}

// fresh は、数えた後の挿入と削除が少なく、覚えた値をそのまま使えるか。
func (e *rowCountEntry) fresh() bool { return e.changes <= e.count/4+64 }

// changed は、テーブル t に行を 1 つ挿入したか削除したことを記録する。
func (c *rowCountCache) changed(t *Table) {
	c.mu.Lock()
	if e, ok := c.entries[rowCountKey{t.db.st, t.db.branch, t.def.ID}]; ok {
		e.changes++
	}
	c.mu.Unlock()
}

// changedBy は、ストア st のブランチ branch のテーブル ID table に、行のバージョンを n 個書いたことを記録する（レプリカが
// 本番の記録を適用したとき）。
func (c *rowCountCache) changedBy(st *store.Store, branch, table uint64, n int) {
	c.mu.Lock()
	if e, ok := c.entries[rowCountKey{st, branch, table}]; ok {
		e.changes += uint64(n)
	}
	c.mu.Unlock()
}

// forgetTable は、ストア st のブランチ branch のテーブル ID table の行数を忘れる（テーブルの削除や、ID を変える
// 書き直しのとき）。
func (c *rowCountCache) forgetTable(st *store.Store, branch, table uint64) {
	c.mu.Lock()
	delete(c.entries, rowCountKey{st, branch, table})
	delete(c.building, rowCountKey{st, branch, table})
	c.mu.Unlock()
}

// forget は、ストア st のブランチ branch の行数を忘れる（マージや復元のように、行の編集を通らずに書くときと、
// ブランチを消すとき）。
func (c *rowCountCache) forget(st *store.Store, branch uint64) {
	c.mu.Lock()
	for k := range c.entries {
		if k.st == st && k.branch == branch {
			delete(c.entries, k)
		}
	}
	for k := range c.building {
		if k.st == st && k.branch == branch {
			delete(c.building, k)
		}
	}
	c.mu.Unlock()
}
