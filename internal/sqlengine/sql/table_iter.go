// Copyright 2020-2021 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modified by Tsukinowa Inc.: SQL_CALC_FOUND_ROWS の LIMIT の後ろの行を、行を作らずに数える RowCounter（docs/engine.md）

package sql

import (
	"io"
)

// TableRowIter is an iterator over the partitions in a table.
type TableRowIter struct {
	table      Table
	partitions PartitionIter
	partition  Partition
	rows       RowIter
}

var _ RowIter = (*TableRowIter)(nil)

// RowCounter は、まだ返していない行を、行を作らずに数えられる RowIter（HeimDB。SQL_CALC_FOUND_ROWS の LIMIT が、
// 返さない行を数えるのに使う）。数えられないときは ok を false にし、そのときは何も読み進めない。数えた後の Next は
// io.EOF を返す。
type RowCounter interface {
	CountRest(ctx *Context) (n int64, ok bool, err error)
}

var _ RowCounter = (*TableRowIter)(nil)

// CountRest は、今のパーティションの残りを数え（そのパーティションの行が RowCounter のときだけ）、次からのパーティションは
// Next で数える。まだ読み始めていなければ（LIMIT 0）、Next と同じく最初のパーティションを開く。
func (i *TableRowIter) CountRest(ctx *Context) (int64, bool, error) {
	if i.rows == nil {
		if i.partition == nil {
			partition, err := i.partitions.Next(ctx)
			if err == io.EOF {
				return 0, false, nil // パーティションがない。後始末は Next に任せる
			} else if err != nil {
				return 0, false, err
			}
			i.partition = partition
		}
		rows, err := i.table.PartitionRows(ctx, i.partition)
		if err != nil {
			return 0, false, err
		}
		i.rows = rows
	}
	rc, ok := i.rows.(RowCounter)
	if !ok {
		return 0, false, nil
	}
	n, ok, err := rc.CountRest(ctx)
	if err != nil || !ok {
		return n, ok, err
	}
	for {
		if _, err := i.Next(ctx); err == io.EOF {
			return n, true, nil
		} else if err != nil {
			return n, true, err
		}
		n++
	}
}

// NewTableRowIter returns a new iterator over the rows in the partitions of the table given.
func NewTableRowIter(ctx *Context, table Table, partitions PartitionIter) *TableRowIter {
	return &TableRowIter{table: table, partitions: partitions}
}

func (i *TableRowIter) Next(ctx *Context) (Row, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if i.partition == nil {
		partition, err := i.partitions.Next(ctx)
		if err != nil {
			if err == io.EOF {
				if e := i.partitions.Close(ctx); e != nil {
					return nil, e
				}
			}

			return nil, err
		}

		i.partition = partition
	}

	if i.rows == nil {
		rows, err := i.table.PartitionRows(ctx, i.partition)
		if err != nil {
			return nil, err
		}

		i.rows = rows
	}

	row, err := i.rows.Next(ctx)
	if err != nil && err == io.EOF {
		if err = i.rows.Close(ctx); err != nil {
			return nil, err
		}

		i.partition = nil
		i.rows = nil
		row, err = i.Next(ctx)
	}
	return row, err
}

func (i *TableRowIter) Close(ctx *Context) error {
	if i.rows != nil {
		if err := i.rows.Close(ctx); err != nil {
			_ = i.partitions.Close(ctx)
			return err
		}
	}
	return i.partitions.Close(ctx)
}
