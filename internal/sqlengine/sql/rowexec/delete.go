// Copyright 2023 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: 複数の表から消す DELETE は、結び付いた行を先に全部読んでから消し、影響を受けた行の数を消した行の数にする（docs/engine.md）
// Modified by Tsukinowa Inc.: 結合から 1 つの表だけを消す DELETE でも、外部結合が NULL で埋めた行は消さない（docs/engine.md）
// Modified by Tsukinowa Inc.: 結合から 1 つの表だけを消す DELETE は、何度も結び付く行を 1 度だけ消して数える（docs/engine.md）
// Modified by Tsukinowa Inc.: 結合から主キーのない表の行を消すとき、同じ値の行をもう消していれば消せなくてもよい（docs/engine.md）

package rowexec

import (
	"fmt"
	"io"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
)

// schemaPositionDeleter contains a sql.RowDeleter and the start (inclusive) and end (exclusive) position
// within a schema that indicate the portion of the schema that is associated with this specific deleter.
type schemaPositionDeleter struct {
	deleter     sql.RowDeleter
	schemaStart int
	schemaEnd   int
	// HeimDB: 表の名前（小文字。別名ではなく元の表）と、主キーがあるか（複数の表から消すときに、同じ行を 1 度だけ
	// 消して数えるため）
	table string
	hasPK bool
	// HeimDB: 外部結合で NULL で埋められうる表か（outerJoinColumns）。消す表が 1 つでも、その表のすべて NULL の行は
	// 消さない（LEFT JOIN で相手のない側）
	outer bool
}

// findSourcePosition searches the specified |schema| for the first group of columns whose source is |name|,
// and returns the start position of that source in the schema (inclusive) and the end position (exclusive).
// If any problems were an encountered, such as not finding any columns from the specified source name,
// an error is returned.
func findSourcePosition(schema sql.Schema, name string) (uint, uint, error) {
	foundStart := false
	name = strings.ToLower(name)
	var start uint
	for i, col := range schema {
		if strings.ToLower(col.Source) == name {
			if !foundStart {
				start = uint(i)
				foundStart = true
			}
		} else {
			if foundStart {
				return start, uint(i), nil
			}
		}
	}
	if foundStart {
		return start, uint(len(schema)), nil
	}

	return 0, 0, fmt.Errorf("unable to find any columns in schema from source %q", name)
}

// deleteIter executes the DELETE FROM logic to delete rows from tables as they flow through the iterator. For every
// table the deleteIter needs to delete rows from, it needs a schemaPositionDeleter that provides the RowDeleter
// interface as well as start and end position for that table's full row in the row this iterator consumes from its
// child. For simple DELETE FROM statements deleting from a single table, this will likely be the full row contents,
// but in more complex scenarios when there are columns contributed by outer scopes and for DELETE FROM JOIN statements
// the child iterator will return a row that is composed of rows from multiple table sources.
type deleteIter struct {
	deleters  []schemaPositionDeleter
	schema    sql.Schema
	childIter sql.RowIter
	closed    bool
	// HeimDB: 複数の表から消すとき（DELETE a, b FROM ...）は、子の行（結び付いた行）を先に全部読み、消す表ごとの行
	// だけを pending に集めてから消す（drained）。読みながら消すと、消した行がその後の結び付きに現れず（同じ
	// トランザクションの書き込みが見える）、1 つの行が何度も結び付くとき 2 つ目からの組を消さなかった。MySQL は文の
	// 前の状態で消す行を決める。主キーのある表の行は、表ごとに 1 度だけ集める（同じ表を別名で 2 度消す自己結合も
	// 含む）。この iterator が返す行の数が影響を受けた行の数になるので、消した行の数の合計になる（MySQL と同じ）。
	pending []pendingDelete
	next    int
	drained bool
	// HeimDB: 結合から 1 つの表だけを消すとき、主キーのある表の消した行（HashOf）。1 つの行が何度も結び付いても
	// 1 度だけ消して数える（MySQL と同じ。前は結び付いた数だけ影響を受けた行に数えた）
	deleted map[uint64]struct{}
	// HeimDB: 結合から主キーのない表の行を消したとき、消した値（deleters の添字ごとの HashOf。alreadyDeleted）
	keylessDeleted map[int]map[uint64]struct{}
}

// pendingDelete は、複数の表から消すときに、消す行（deleters[deleter] の表の行）。
type pendingDelete struct {
	deleter int
	row     sql.Row
}

func (d *deleteIter) Next(ctx *sql.Context) (sql.Row, error) {
	if len(d.deleters) > 1 {
		return d.nextMulti(ctx)
	}
	for {
		row, err := d.childIter.Next(ctx)
		if err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		target := d.targetRow(0, row)
		if d.deleters[0].outer && allNull(target) {
			// HeimDB: 外部結合が NULL で埋めた行は消さない（前は主キーが NULL の行を消そうとしてエラーになった）
			continue
		}
		if d.deleters[0].hasPK && len(target) < len(row) {
			h, err := sql.HashOf(ctx, target)
			if err != nil {
				return nil, err
			}
			if _, ok := d.deleted[h]; ok {
				continue
			}
			if d.deleted == nil {
				d.deleted = map[uint64]struct{}{}
			}
			d.deleted[h] = struct{}{}
		}
		if err := d.deleteFrom(ctx, 0, target); err != nil {
			if d.alreadyDeleted(ctx, 0, target, err) {
				continue
			}
			return nil, err
		}
		if err := d.recordKeyless(ctx, 0, target, len(target) < len(row)); err != nil {
			return nil, err
		}
		return row, nil
	}
}

// recordKeyless は、結合から主キーのない表の行を消したとき、その値を覚える（HeimDB）。
func (d *deleteIter) recordKeyless(ctx *sql.Context, i int, target sql.Row, joined bool) error {
	if d.deleters[i].hasPK || !joined {
		return nil
	}
	h, err := sql.HashOf(ctx, target)
	if err != nil {
		return err
	}
	if d.keylessDeleted == nil {
		d.keylessDeleted = map[int]map[uint64]struct{}{}
	}
	if d.keylessDeleted[i] == nil {
		d.keylessDeleted[i] = map[uint64]struct{}{}
	}
	d.keylessDeleted[i][h] = struct{}{}
	return nil
}

// alreadyDeleted は、主キーのない表の行を消せなかった誤り err が、この文で同じ値の行をもう消したためか（HeimDB）。
// 主キーのない表は同じ値の行を見分けられないので、結合で同じ行が何度も結び付くと同じ値を何度も消そうとする。同じ値の
// 行はどれも同じ相手と結び付くので、同じ値の行がなくなるまで消せば、MySQL と同じくすべて消える（前は 2 度目で
// 1105 の the row to change was not found だった。DELETE r FROM l JOIN r ON TRUE）。
func (d *deleteIter) alreadyDeleted(ctx *sql.Context, i int, target sql.Row, err error) bool {
	if d.deleters[i].hasPK || !sql.ErrRowToChangeNotFound.Is(err) {
		return false
	}
	h, herr := sql.HashOf(ctx, target)
	if herr != nil {
		return false
	}
	_, ok := d.keylessDeleted[i][h]
	return ok
}

// targetRow は、子の行 row のうち、deleters[i] の表の行（外の問い合わせの列を前に足した行にも対応する）。
func (d *deleteIter) targetRow(i int, row sql.Row) sql.Row {
	// For each target table from which we are deleting rows, reduce the row from our child iterator to just
	// the columns that are part of that target table. This means looking at the position in the schema for
	// the target table and also removing any prepended columns contributed by outer scopes.
	deleter := d.deleters[i]
	fullSchemaLength := len(d.schema)
	rowLength := len(row)
	schemaLength := deleter.schemaEnd - deleter.schemaStart
	if schemaLength < rowLength {
		return row[(rowLength - fullSchemaLength + deleter.schemaStart):(rowLength - fullSchemaLength + deleter.schemaEnd)]
	}
	return row
}

func (d *deleteIter) deleteFrom(ctx *sql.Context, i int, row sql.Row) error {
	return d.deleters[i].deleter.Delete(ctx, row)
}

// nextMulti は、複数の表から消すときの Next。初めに子の行を全部読んで消す行を集め、1 行ずつ消して返す。
func (d *deleteIter) nextMulti(ctx *sql.Context) (sql.Row, error) {
	if !d.drained {
		if err := d.collect(ctx); err != nil {
			return nil, err
		}
		d.drained = true
	}
	if d.next >= len(d.pending) {
		return nil, io.EOF
	}
	for d.next < len(d.pending) {
		p := d.pending[d.next]
		d.pending[d.next] = pendingDelete{}
		d.next++
		if err := d.deleteFrom(ctx, p.deleter, p.row); err != nil {
			if d.alreadyDeleted(ctx, p.deleter, p.row, err) {
				continue
			}
			return nil, err
		}
		if err := d.recordKeyless(ctx, p.deleter, p.row, true); err != nil {
			return nil, err
		}
		return p.row, nil
	}
	return nil, io.EOF
}

// collect は、子の行を全部読み、消す表ごとの行を pending に集める。LEFT JOIN で相手のない側（NULL で埋めた行）は
// 消さない（MySQL と同じ。前は主キーが NULL の行を消そうとしてエラーになった）。主キーのある表の行は、表ごとに
// 1 度だけ集める。主キーのない表は、同じ値の行を見分けられないので、結び付いたたびに集める（前と同じ）。
func (d *deleteIter) collect(ctx *sql.Context) error {
	seen := map[string]map[uint64]struct{}{}
	for {
		row, err := d.childIter.Next(ctx)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		for i, deleter := range d.deleters {
			target := d.targetRow(i, row)
			if allNull(target) {
				continue
			}
			target = append(sql.Row(nil), target...)
			if deleter.hasPK {
				h, err := sql.HashOf(ctx, target)
				if err != nil {
					return err
				}
				if seen[deleter.table] == nil {
					seen[deleter.table] = map[uint64]struct{}{}
				}
				if _, ok := seen[deleter.table][h]; ok {
					continue
				}
				seen[deleter.table][h] = struct{}{}
			}
			d.pending = append(d.pending, pendingDelete{deleter: i, row: target})
		}
	}
}

func (d *deleteIter) Close(ctx *sql.Context) error {
	if !d.closed {
		d.closed = true
		var firstErr error
		// Make sure we close all the deleters and the childIter, and track the first
		// error seen so we can return it after safely closing all resources.
		for _, deleter := range d.deleters {
			err := deleter.deleter.Close(ctx)
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		err := d.childIter.Close(ctx)

		if firstErr != nil {
			return firstErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func newDeleteIter(childIter sql.RowIter, schema sql.Schema, deleters ...schemaPositionDeleter) sql.RowIter {
	openerClosers := make([]sql.EditOpenerCloser, len(deleters))
	for i, ds := range deleters {
		openerClosers[i] = ds.deleter
	}
	return plan.NewTableEditorIter(&deleteIter{
		deleters:  deleters,
		childIter: childIter,
		schema:    schema,
	}, openerClosers...)
}

// allNull は、行の値がすべて NULL か（HeimDB）。
func allNull(row sql.Row) bool {
	for _, v := range row {
		if v != nil {
			return false
		}
	}
	return true
}
