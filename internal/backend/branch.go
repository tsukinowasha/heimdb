package backend

import (
	"fmt"
	"strings"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// ブランチを操作する SQL のプロシージャ（docs/plan.md の段階 6）。
//
//	CALL heimdb_branch_create('staging');                                  -- main の今の状態から分岐
//	CALL heimdb_branch_create('staging', 'main');                          -- 親を指定
//	CALL heimdb_branch_create('staging', 'main', '2026-10-03 10:00:00');   -- 過去の時点から分岐（セッションの time_zone）
//	CALL heimdb_branch_list();
//	CALL heimdb_branch_delete('staging');                                  -- 子ブランチがあれば拒否
//	CALL heimdb_branch_diff('staging'[, 'wp_posts' | '*'[, 開始, 件数]]);  -- 親との差分（diff.go）
//	CALL heimdb_branch_resolve('staging', 'wp_posts', 'ID=4', 'branch');   -- 競合の解決（merge.go）
//	CALL heimdb_branch_merge('staging');                                   -- 親へのマージ（merge.go）
//	CALL heimdb_branch_merge('staging', 'dry_run');                        -- コミットせずに確かめる
//	CALL heimdb_branch_sync('staging'[, 'branch' | 'parent'][, 'dry_run']); -- 親の変更を取り込む（sync.go）
//	CALL heimdb_branch_limit('staging', '2G', '30s');                       -- 大きさと文の実行時間の上限（limit.go）
//	CALL heimdb_restore('main', '2026-10-03 10:00:00');                    -- 過去の時点への復元（restore.go）
//	CALL heimdb_gc();                                                       -- 古いバージョンの削除（gc.go）

var branchSchema = sql.Schema{
	{Name: "branch", Type: types.LongText},
	{Name: "parent", Type: types.LongText, Nullable: true},
	{Name: "forked_at", Type: types.LongText, Nullable: true},
	{Name: "database", Type: types.LongText},
	{Name: "size_bytes", Type: types.Int64},                       // 推定（status.go、store.BranchDiskUsage）
	{Name: "max_size_bytes", Type: types.Int64, Nullable: true},   // 大きさの上限（limit.go）
	{Name: "max_execution_ms", Type: types.Int64, Nullable: true}, // 文の実行時間の上限（limit.go）
}

func (p *Provider) branchProcedures() []sql.ExternalStoredProcedureDetails {
	return []sql.ExternalStoredProcedureDetails{
		{Name: "heimdb_branch_create", Schema: branchSchema, Function: func(ctx *sql.Context, name string) (sql.RowIter, error) {
			return p.createBranch(ctx, name, "main", "")
		}},
		{Name: "heimdb_branch_create", Schema: branchSchema, Function: func(ctx *sql.Context, name, parent string) (sql.RowIter, error) {
			return p.createBranch(ctx, name, parent, "")
		}},
		{Name: "heimdb_branch_create", Schema: branchSchema, Function: func(ctx *sql.Context, name, parent, at string) (sql.RowIter, error) {
			return p.createBranch(ctx, name, parent, at)
		}},
		{Name: "heimdb_branch_list", Schema: branchSchema, Function: func(ctx *sql.Context) (sql.RowIter, error) {
			var rows []sql.Row
			for _, b := range p.st.Branches() {
				row, err := p.branchRow(ctx, b)
				if err != nil {
					return nil, err
				}
				rows = append(rows, row)
			}
			return &rowIter{rows: rows}, nil
		}},
		{Name: "heimdb_branch_delete", Schema: nil, Function: func(ctx *sql.Context, name string) (sql.RowIter, error) {
			b, ok := p.st.BranchByName(name)
			if err := p.st.DeleteBranch(name); err != nil {
				return nil, err
			}
			if ok {
				rowCounts.forget(p.st, b.ID)
			}
			return nil, nil
		}},
		{Name: "heimdb_branch_diff", Schema: diffSummarySchema, ReadOnly: true, Function: func(ctx *sql.Context, name string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_branch_diff", name, func(name string, _ caller) (sql.RowIter, error) {
				return p.branchDiff(ctx, name, "", 0, 0)
			})
		}},
		{Name: "heimdb_branch_diff", Schema: diffRowSchema, ReadOnly: true, Function: func(ctx *sql.Context, name, table string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_branch_diff", name, func(name string, _ caller) (sql.RowIter, error) {
				if table == "" {
					return nil, fmt.Errorf("HeimDB: name a table (or '*'); for the summary, call heimdb_branch_diff(name)")
				}
				return p.branchDiff(ctx, name, table, 0, 0)
			})
		}},
		{Name: "heimdb_branch_diff", Schema: diffRowSchema, ReadOnly: true, Function: func(ctx *sql.Context, name, table string, offset, limit int64) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_branch_diff", name, func(name string, _ caller) (sql.RowIter, error) {
				if table == "" {
					return nil, fmt.Errorf("HeimDB: name a table (or '*') to page through the rows of the diff")
				}
				if offset < 0 || limit <= 0 {
					return nil, fmt.Errorf("HeimDB: the offset must be 0 or more and the count must be 1 or more")
				}
				return p.branchDiff(ctx, name, table, offset, limit)
			})
		}},
		{Name: "heimdb_branch_resolve", Schema: nil, Function: func(ctx *sql.Context, name, table, id, choice string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_branch_resolve", name, func(name string, c caller) (sql.RowIter, error) {
				return nil, p.branchResolve(ctx, name, table, id, choice, c)
			})
		}},
		{Name: "heimdb_branch_merge", Schema: mergeSchema, Function: func(ctx *sql.Context, name string) (sql.RowIter, error) {
			return p.branchMerge(ctx, name, false)
		}},
		{Name: "heimdb_branch_merge", Schema: mergeSchema, Function: func(ctx *sql.Context, name, mode string) (sql.RowIter, error) {
			if !strings.EqualFold(mode, "dry_run") {
				return nil, fmt.Errorf("HeimDB: unknown merge mode %q (use 'dry_run')", mode)
			}
			return p.branchMerge(ctx, name, true)
		}},
		{Name: "heimdb_branch_sync", Schema: mergeSchema, Function: func(ctx *sql.Context, name string) (sql.RowIter, error) {
			return p.branchSync(ctx, name, "", false)
		}},
		{Name: "heimdb_branch_sync", Schema: mergeSchema, Function: func(ctx *sql.Context, name, mode string) (sql.RowIter, error) {
			switch m := strings.ToLower(mode); m {
			case "dry_run":
				return p.branchSync(ctx, name, "", true)
			case "branch", "parent":
				return p.branchSync(ctx, name, m, false)
			}
			return nil, fmt.Errorf("HeimDB: unknown sync mode %q (use 'dry_run', 'branch' or 'parent')", mode)
		}},
		{Name: "heimdb_branch_sync", Schema: mergeSchema, Function: func(ctx *sql.Context, name, side, mode string) (sql.RowIter, error) {
			s := strings.ToLower(side)
			if (s != "branch" && s != "parent") || !strings.EqualFold(mode, "dry_run") {
				return nil, fmt.Errorf("HeimDB: use heimdb_branch_sync(name, 'branch' or 'parent', 'dry_run')")
			}
			return p.branchSync(ctx, name, s, true)
		}},
		{Name: "heimdb_branch_limit", Schema: branchSchema, Function: func(ctx *sql.Context, name, size, timeout string) (sql.RowIter, error) {
			return p.branchLimit(ctx, name, size, timeout)
		}},
		{Name: "heimdb_gc", Schema: gcSchema, Function: func(ctx *sql.Context) (sql.RowIter, error) {
			return p.gc(ctx)
		}},
		{Name: "heimdb_restore_preview", Schema: restoreSchema, ReadOnly: true, Function: func(ctx *sql.Context, name, at string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_restore_preview", name, func(name string, c caller) (sql.RowIter, error) {
				if err := p.checkBranchTime(ctx, c, name, at); err != nil {
					return nil, err
				}
				return p.restorePreview(ctx, name, at, "")
			})
		}},
		{Name: "heimdb_restore_preview", Schema: restoreSchema, ReadOnly: true, Function: func(ctx *sql.Context, name, at, tables string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_restore_preview", name, func(name string, c caller) (sql.RowIter, error) {
				if err := p.checkBranchTime(ctx, c, name, at); err != nil {
					return nil, err
				}
				return p.restorePreview(ctx, name, at, tables)
			})
		}},
		{Name: "heimdb_restore", Schema: restoreSchema, Function: func(ctx *sql.Context, name, at string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_restore", name, func(name string, c caller) (sql.RowIter, error) {
				if err := p.checkBranchTime(ctx, c, name, at); err != nil {
					return nil, err
				}
				return p.restore(ctx, name, at, "")
			})
		}},
		{Name: "heimdb_restore", Schema: restoreSchema, Function: func(ctx *sql.Context, name, at, tables string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_restore", name, func(name string, c caller) (sql.RowIter, error) {
				if err := p.checkBranchTime(ctx, c, name, at); err != nil {
					return nil, err
				}
				return p.restore(ctx, name, at, tables)
			})
		}},
		{Name: "heimdb_changes", Schema: changesSchema, ReadOnly: true, Function: func(ctx *sql.Context, name, at string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_changes", name, func(name string, c caller) (sql.RowIter, error) {
				if err := p.checkBranchTime(ctx, c, name, at); err != nil {
					return nil, err
				}
				return p.changes(ctx, name, at, "")
			})
		}},
		{Name: "heimdb_changes", Schema: changesSchema, ReadOnly: true, Function: func(ctx *sql.Context, name, at, tables string) (sql.RowIter, error) {
			return p.onBranch(ctx, "heimdb_changes", name, func(name string, c caller) (sql.RowIter, error) {
				if err := p.checkBranchTime(ctx, c, name, at); err != nil {
					return nil, err
				}
				return p.changes(ctx, name, at, tables)
			})
		}},
		{Name: "heimdb_status", Schema: statusSchema, ReadOnly: true, Function: func(ctx *sql.Context) (sql.RowIter, error) {
			return p.status(ctx)
		}},
		{Name: "heimdb_backup", Schema: backupSchema, Function: func(ctx *sql.Context) (sql.RowIter, error) {
			return p.backup(ctx, "", "")
		}},
		{Name: "heimdb_backup", Schema: backupSchema, Function: func(ctx *sql.Context, name string) (sql.RowIter, error) {
			return p.backup(ctx, name, "")
		}},
		{Name: "heimdb_backup", Schema: backupSchema, Function: func(ctx *sql.Context, name, key string) (sql.RowIter, error) {
			return p.backup(ctx, name, key)
		}},
	}
}

func (p *Provider) createBranch(ctx *sql.Context, name, parentName, at string) (sql.RowIter, error) {
	parent, ok := p.st.BranchByName(parentName)
	if !ok {
		return nil, fmt.Errorf("HeimDB: parent branch %q not found", parentName)
	}
	var ts uint64
	if at != "" {
		var err error
		if ts, err = parseAsOf(ctx, at); err != nil {
			return nil, err
		}
	}
	// 親の書き込みロックを取ってから分ける（docs/plan.md の段階 121）。何回かに分けてコミットするインデックスの作成
	// （裏の作り直し、heimdb_rebuild_indexes、CREATE INDEX、表を書き直す ALTER）の途中に分岐点が入ると、ブランチは
	// 前の定義のまま親の作りかけのエントリを見て、後でブランチで作り直すと、ブランチで消した行の親のエントリが残る。
	// この接続のトランザクションが親のロックを持っていれば、それで足りる（自分を待たない）。
	if s, ok := ctx.Session.(*Session); !ok || s.txn == nil || !s.txn.HoldsLock(parent.ID) {
		var conn uint32
		if ok {
			conn = s.ID()
		}
		release, err := p.st.LockBranchFor(parent.ID, p.st.LockTimeout, store.LockOwner{Conn: conn, Op: "create branch"}, ctx.Done())
		if err != nil {
			return nil, err
		}
		defer release()
	}
	b, err := p.st.CreateBranch(name, parent.ID, ts)
	if err != nil {
		return nil, err
	}
	row, err := p.branchRow(ctx, b)
	if err != nil {
		return nil, err
	}
	return &rowIter{rows: []sql.Row{row}}, nil
}

// branchRow は、ブランチの情報を 1 行にする。分岐時刻はセッションの time_zone で表す。
func (p *Provider) branchRow(ctx *sql.Context, b store.BranchInfo) (sql.Row, error) {
	var parent, forkedAt any
	if b.Parent != 0 {
		for _, pb := range p.st.Branches() {
			if pb.ID == b.Parent {
				parent = pb.Name
			}
		}
		loc, err := sessionLocation(ctx)
		if err != nil {
			return nil, err
		}
		forkedAt = time.UnixMicro(int64(b.ForkedAt)).In(loc).Format("2006-01-02 15:04:05.000000")
	}
	size, err := p.st.BranchDiskUsage(b.ID)
	if err != nil {
		return nil, err
	}
	var maxSize, maxExec any
	if b.MaxBytes > 0 {
		maxSize = int64(b.MaxBytes)
	}
	if b.MaxExecutionMS > 0 {
		maxExec = int64(b.MaxExecutionMS)
	}
	return sql.Row{b.Name, parent, forkedAt, p.dbName(b), int64(size), maxSize, maxExec}, nil
}
