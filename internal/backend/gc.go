package backend

import (
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// 古いバージョンの削除（docs/plan.md の段階 9）。ふだんはサーバがバックグラウンドで動かす（cmd/heimdb の -gc-interval）。
//
//	CALL heimdb_gc();  -- 今すぐ動かし、境界と消した数を返す

var gcSchema = sql.Schema{
	{Name: "boundary", Type: types.LongText},
	{Name: "versions", Type: types.Int64},
	{Name: "tables", Type: types.Int64},
	{Name: "indexes", Type: types.Int64},
	{Name: "merge_records", Type: types.Int64},
}

func (p *Provider) gc(ctx *sql.Context) (sql.RowIter, error) {
	stats, err := p.st.GC(p.Retention)
	if err != nil {
		return nil, err
	}
	// 境界は UTC のオフセット付きで出す（AS OF と復元にそのまま渡せる）。
	b := time.UnixMicro(int64(stats.Boundary)).UTC().Format("2006-01-02 15:04:05.000000-07:00")
	return &rowIter{rows: []sql.Row{{b, int64(stats.Versions), int64(stats.Tables), int64(stats.Indexes), int64(stats.MergeRecords)}}}, nil
}
