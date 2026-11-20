package backend

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
	"github.com/tsukinowasha/heimdb/internal/rowenc"
	"github.com/tsukinowasha/heimdb/internal/schema"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// editor は INSERT、UPDATE、DELETE、REPLACE と AUTO_INCREMENT の変更を、セッションのトランザクションに書く。
type editor struct {
	t *Table
	// tx は、セッションではなくこのトランザクションに書くとき（マージ、復元）に指定する。
	tx *store.Txn
	// defChecked は、テーブル定義が最新のものと同じことを確かめ済みかどうか（エディタは文ごとに作られる）。
	defChecked bool
	// sizeChecked は、ブランチの大きさの上限を確かめ済みかどうか（limit.go）。
	sizeChecked bool
	// writes は、この文で書いた回数（実行時間の上限で止められたかを確かめる。limit.go）。
	writes int
	// txnMax は、トランザクションの大きさの上限（大きさの上限の付いたブランチ。limit.go）。0 なら上限なし。
	txnMax int
	// noWatch は、変更の見張り（watch.go）に印を残さないかどうか（復元）。
	noWatch bool
	// watchChecked、watchKind、watchCols は、見張りのためのテーブルの種類と列の位置（最初の書き込みで調べる）。
	watchChecked bool
	watchKind    watchKind
	watchCols    map[string]int
	// updated は、行 ID で並べる表で、この文で変えた行の行 ID（locateRow）。
	updated map[any]bool
}

var (
	_ sql.TableEditor         = (*editor)(nil)
	_ sql.AutoIncrementSetter = (*editor)(nil)
)

// txn は、セッションのトランザクションを、書き込みロックを取ったうえで返す。主キーや一意制約の検査は
// 「最新の確定済みデータ + 自分の変更」で行うので、検査より前にロックを取る必要がある。
func (e *editor) txn(ctx *sql.Context) (*store.Txn, error) {
	if e.t.asOf != store.Latest {
		return nil, fmt.Errorf("HeimDB: table %s AS OF a past time is read-only", e.t.def.Name)
	}
	txn := e.tx
	if txn == nil {
		s, ok := ctx.Session.(*Session)
		if !ok || s.txn == nil {
			return nil, fmt.Errorf("HeimDB: write to %s outside of a transaction", e.t.def.Name)
		}
		txn = s.txn
	}
	if err := txn.LockForWrite(e.t.db.branch); err != nil {
		return nil, err
	}
	// マージと復元（tx を指定した呼び出し）は、上限を付けられない main や、戻す操作なので確かめない。
	if e.tx == nil {
		if !e.sizeChecked {
			if err := checkBranchSize(e.t.db.st, e.t.db.branch, e.t.def.Name); err != nil {
				return nil, err
			}
			e.txnMax = txnLimit(e.t.db.st, e.t.db.branch)
			e.sizeChecked = true
		}
	}
	// テーブル定義はスナップショットの時点で解決している。その後に他の接続が定義を変えていたら（列の変更、
	// インデックスの追加、TRUNCATE など）、古い定義で書くとインデックスや行の形が食い違うので、MySQL と同じく
	// エラー 1412 にする。書き込みロックを持っている間は定義は変わらないので、エディタごとに 1 回確かめれば足りる。
	// tx を指定した呼び出し（マージ、復元）は、ロックを取ってから定義を読んでいるので確かめない。復元は、確定済みの
	// 最新の定義とは違う定義（過去のバージョン）で書く。
	if !e.defChecked && e.tx == nil {
		latest, ok, err := e.t.db.st.GetTable(e.t.db.branch, e.t.def.Name, store.Latest)
		if err != nil {
			return nil, err
		}
		if !ok || latest.ID != e.t.def.ID || latest.Version != e.t.def.Version {
			return nil, errTableDefChanged
		}
		e.defChecked = true
	}
	return txn, nil
}

// writeTxn は、行を書く前の txn。長い文が止められた（実行時間の上限、KILL QUERY）か、トランザクションが大きくなりすぎて
// いないか（エラー 1197。その文は取り消される）を確かめる（limit.go）。取り消しの DiscardChanges は txn を使うので、
// ここでだけ確かめる。
func (e *editor) writeTxn(ctx *sql.Context) (*store.Txn, error) {
	txn, err := e.txn(ctx)
	if err != nil {
		return nil, err
	}
	e.writes++
	if e.tx == nil {
		if err := interrupted(ctx, e.writes); err != nil {
			return nil, err
		}
		if e.txnMax > 0 && txn.Size() > e.txnMax {
			return nil, errTxnTooLarge
		}
	} else if err := interrupted(nil, e.writes); err != nil {
		// マージと復元は自分のトランザクション（e.tx）に書く。メモリの上限だけを確かめ、止まれば操作ごと取り消される
		// （docs/plan.md の段階 58）。
		return nil, err
	}
	return txn, nil
}

// errTableDefChanged は MySQL のエラー 1412（ER_TABLE_DEF_CHANGED）。
var errTableDefChanged = mysql.NewSQLError(1412, mysql.SSUnknownSQLState, "Table definition has changed, please retry transaction")

// StatementBegin は sql.EditOpenerCloser の実装。
func (e *editor) StatementBegin(ctx *sql.Context) {
	if txn, err := e.txn(ctx); err == nil {
		txn.StatementBegin()
	}
}

// DiscardChanges は sql.EditOpenerCloser の実装。INSERT IGNORE などの無視できるエラーでは取り消さない（memory と同じ）。
func (e *editor) DiscardChanges(ctx *sql.Context, errorEncountered error) error {
	txn, err := e.txn(ctx)
	if err != nil {
		return err
	}
	if _, ignorable := errorEncountered.(sql.IgnorableError); ignorable {
		txn.StatementComplete()
		return nil
	}
	if err := txn.DiscardStatement(); err != nil {
		// 取り消しがメモリの上限で止まると、トランザクションごと取り消してある。go-mysql-server はこのエラーを捨てる
		// ので、セッションに覚えて文のエラーとして返す（docs/plan.md の段階 59）。
		if s, ok := ctx.Session.(*Session); ok && isOutOfMemory(err) {
			s.discardErr = err
		}
		return err
	}
	return nil
}

// StatementComplete は sql.EditOpenerCloser の実装。
func (e *editor) StatementComplete(ctx *sql.Context) error {
	txn, err := e.txn(ctx)
	if err != nil {
		return err
	}
	txn.StatementComplete()
	return nil
}

// Close は sql.Closer の実装。書き込みはトランザクションのコミットで確定するので、ここでは何もしない。
func (e *editor) Close(*sql.Context) error { return nil }

// Insert は sql.RowInserter の実装。
func (e *editor) Insert(ctx *sql.Context, row sql.Row) error {
	txn, err := e.writeTxn(ctx)
	if err != nil {
		return err
	}
	if row, err = e.t.completeRow(ctx, row, nil); err != nil {
		return err
	}
	pk, err := e.t.encodePK(row)
	if err != nil {
		return err
	}
	if existing, ok, err := e.t.getByPK(txn, pk); err != nil {
		return err
	} else if ok {
		// 一意キーで並べる主キーのない表では、その一意インデックスの重複（docs/plan.md の段階 72）。
		return e.t.pkDuplicateErr(row, !e.t.def.Keyless, existing)
	}
	if err := e.checkUnique(ctx, txn, row, nil); err != nil {
		return err
	}
	mark := txn.Mark()
	if err := e.put(txn, pk, row, true); err != nil {
		return err
	}
	if err := e.updateIndexes(txn, nil, row); err != nil {
		return err
	}
	e.watch(txn, mark, pk, nil, row)
	rowCounts.changed(e.t)
	return e.observeAutoIncrement(ctx, row)
}

// Update は sql.RowUpdater の実装。
func (e *editor) Update(ctx *sql.Context, oldRow, newRow sql.Row) error {
	txn, err := e.writeTxn(ctx)
	if err != nil {
		return err
	}
	if oldRow, err = e.locateRow(txn, oldRow); err != nil {
		return err
	}
	if newRow, err = e.t.completeRow(ctx, newRow, oldRow); err != nil {
		return err
	}
	if e.t.ridOrd >= 0 {
		if e.updated == nil {
			e.updated = map[any]bool{}
		}
		e.updated[newRow[e.t.ridOrd]] = true
	}
	oldPK, err := e.t.encodePK(oldRow)
	if err != nil {
		return err
	}
	newPK, err := e.t.encodePK(newRow)
	if err != nil {
		return err
	}
	if string(oldPK) != string(newPK) {
		if existing, ok, err := e.t.getByPK(txn, newPK); err != nil {
			return err
		} else if ok {
			return e.t.pkDuplicateErr(newRow, !e.t.def.Keyless, existing)
		}
	}
	if err := e.checkUnique(ctx, txn, newRow, oldPK); err != nil {
		return err
	}
	mark := txn.Mark()
	if string(oldPK) != string(newPK) {
		if err := txn.DeleteRow(e.t.db.branch, e.t.def.ID, oldPK); err != nil {
			return err
		}
	}
	// 主キーが変わるときは、新しい主キーの行を挿入するのと同じ。
	if err := e.put(txn, newPK, newRow, string(oldPK) != string(newPK)); err != nil {
		return err
	}
	if err := e.updateIndexes(txn, oldRow, newRow); err != nil {
		return err
	}
	e.watch(txn, mark, newPK, oldRow, newRow)
	return e.observeAutoIncrement(ctx, newRow)
}

// Delete は sql.RowDeleter の実装。
func (e *editor) Delete(ctx *sql.Context, row sql.Row) error {
	txn, err := e.writeTxn(ctx)
	if err != nil {
		return err
	}
	if row, err = e.locateRow(txn, row); err != nil {
		return err
	}
	pk, err := e.t.encodePK(row)
	if err != nil {
		return err
	}
	mark := txn.Mark()
	if err := txn.DeleteRow(e.t.db.branch, e.t.def.ID, pk); err != nil {
		return err
	}
	rowCounts.changed(e.t)
	if err := e.updateIndexes(txn, row, nil); err != nil {
		return err
	}
	e.watch(txn, mark, pk, row, nil)
	return nil
}

// completeRow は、行 ID で並べる表に書く行 row に、見えない列（行 ID とハッシュ）を入れる（docs/plan.md の段階 72）。
// エンジンからの行は見える列だけで、行 ID は、変える行なら変える前の行 old のもの、足す行なら新しく払い出す。HeimDB の
// 中からの行（マージ、復元）は見えない列を含み、行 ID はそのまま使う。ハッシュはいつも見える列から求め直す。ほかの表の
// 行はそのまま返す。
func (t *Table) completeRow(ctx *sql.Context, row, old sql.Row) (sql.Row, error) {
	if t.ridOrd < 0 {
		return row, nil
	}
	full := make(sql.Row, len(t.sch.Schema))
	vis := len(t.esch.Schema)
	copy(full, row[:vis])
	switch {
	case len(row) == len(full):
		full[t.ridOrd] = row[t.ridOrd]
	case old != nil:
		full[t.ridOrd] = old[t.ridOrd]
	default:
		v, err := t.GetNextAutoIncrementValue(ctx, nil)
		if err != nil {
			return nil, err
		}
		full[t.ridOrd] = v
	}
	h, err := schema.RowHash(full[:vis])
	if err != nil {
		return nil, fmt.Errorf("HeimDB: table %s: %w", t.def.Name, err)
	}
	full[t.hashOrd] = h
	return full, nil
}

// locateRow は、エンジンが消す行や変える前の行として渡した、見える列だけの行 row を、行 ID で並べる表の保存した行にする
// （見えないインデックスでハッシュの同じ行を引き、同じ中身が何行もあれば、行 ID のいちばん小さいもの）。この文で変えた
// 行は、ほかになければ選ぶ（UPDATE t SET x = x + 1 で、1 を 2 にした行を、元から 2 の行より先に選ばない）。ほかの表の
// 行と、見えない列を含む行はそのまま返す。
func (e *editor) locateRow(txn *store.Txn, row sql.Row) (sql.Row, error) {
	t := e.t
	if t.ridOrd < 0 || len(row) == len(t.sch.Schema) {
		return row, nil
	}
	h, err := schema.RowHash(row[:len(t.esch.Schema)])
	if err != nil {
		return nil, fmt.Errorf("HeimDB: table %s: %w", t.def.Name, err)
	}
	probe := make(sql.Row, len(t.sch.Schema))
	probe[t.hashOrd] = h
	prefix, err := t.valuePrefix(t.ridIndex, probe)
	if err != nil {
		return nil, err
	}
	var found, touched sql.Row
	err = txn.ScanIndexLatest(t.db.branch, t.def.ID, t.ridIndex.def.ID, prefix, keyenc.PrefixEnd(prefix), false, func(entry, _ []byte) (bool, error) {
		pk, err := t.pkOfEntry(t.ridIndex, entry)
		if err != nil {
			return false, err
		}
		existing, ok, err := t.getByPK(txn, pk)
		if err != nil || !ok {
			return err == nil, err
		}
		if e.updated[existing[t.ridOrd]] {
			if touched == nil {
				touched = existing
			}
			return true, nil
		}
		found = existing
		return false, nil
	})
	if found == nil {
		found = touched
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, sql.ErrRowToChangeNotFound.New(t.def.Name)
	}
	return found, nil
}

// updateIndexes は、行が oldRow から newRow に変わったときのインデックスのエントリを書く。
// 挿入では oldRow、削除では newRow が nil。エントリが変わらないインデックスには何も書かない。キーが同じでも、
// エントリの値（元の値）が変わる（照合順序で等しい 'Post' から 'post'）なら、値を書き直す。
func (e *editor) updateIndexes(txn *store.Txn, oldRow, newRow sql.Row) error {
	for _, ki := range e.t.keyIndexes() {
		var oldEntry, oldValue, newEntry, newValue []byte
		var err error
		if oldRow != nil {
			if oldEntry, oldValue, err = e.t.entry(ki, oldRow); err != nil {
				return err
			}
		}
		if newRow != nil {
			if newEntry, newValue, err = e.t.entry(ki, newRow); err != nil {
				return err
			}
		}
		sameKey := oldEntry != nil && newEntry != nil && string(oldEntry) == string(newEntry)
		if sameKey && string(oldValue) == string(newValue) {
			continue
		}
		if oldEntry != nil && !sameKey {
			if err := txn.DeleteIndexEntry(e.t.db.branch, e.t.def.ID, ki.def.ID, oldEntry); err != nil {
				return err
			}
		}
		if newEntry != nil {
			if err := txn.PutIndexEntry(e.t.db.branch, e.t.def.ID, ki.def.ID, newEntry, newValue); err != nil {
				return err
			}
		}
	}
	return nil
}

// put は行を書く。insert が true なら新しい主キーの行の挿入（store.Txn.InsertRow を参照）。
func (e *editor) put(txn *store.Txn, pk []byte, row sql.Row, insert bool) error {
	value, err := rowenc.EncodeRow(e.t.def.Version, row)
	if err != nil {
		return fmt.Errorf("HeimDB: table %s: %w", e.t.def.Name, err)
	}
	if insert {
		return txn.InsertRow(e.t.db.branch, e.t.def.ID, pk, value)
	}
	return txn.PutRow(e.t.db.branch, e.t.def.ID, pk, value)
}

// checkUnique は、row が一意インデックスの制約に反しないかを確かめる。exceptPK の行（更新前の自分）は比べない。
// キーを持つインデックスは、同じ値のエントリを引いて確かめる。キーにできない型を含むインデックスは全件を走査する。
func (e *editor) checkUnique(ctx *sql.Context, txn *store.Txn, row sql.Row, exceptPK []byte) error {
	var uniques []uniqueIndex
	for _, idx := range e.t.def.Indexes {
		if !idx.Unique {
			continue
		}
		if ki, ok := e.t.keyIndex(idx); ok {
			if err := e.checkUniqueByIndex(ctx, txn, ki, row, exceptPK); err != nil {
				return err
			}
			continue
		}
		u := uniqueIndex{name: idx.Name}
		hasNull := false
		for _, c := range idx.Columns {
			ord := e.t.def.ColumnIndex(c.Name)
			if row[ord] == nil {
				hasNull = true // NULL を含む行は一意制約の対象外（MySQL と同じ）
			}
			u.ords = append(u.ords, ord)
			u.prefixes = append(u.prefixes, c.Length)
		}
		if !hasNull {
			uniques = append(uniques, u)
		}
	}
	if len(uniques) == 0 {
		return nil
	}
	var conflict error
	err := txn.ScanRowsLatest(e.t.db.branch, e.t.def.ID, nil, nil, false, func(r store.RowVersion) (bool, error) {
		if exceptPK != nil && string(r.PK) == string(exceptPK) {
			return true, nil
		}
		existing, err := e.t.decodeRow(r.Value)
		if err != nil {
			return false, err
		}
		for _, u := range uniques {
			match, err := e.t.columnsMatch(ctx, u, row, existing)
			if err != nil {
				return false, err
			}
			if match {
				conflict = e.t.uniqueDuplicateErr(row, u, e.t.visibleRow(existing))
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return conflict
}

// checkUniqueByIndex は、キーを持つ一意インデックス ki で、row と同じ値の行がないかを確かめる。
// キーが等しくても値が等しいとは限らない（プレフィックス長で切った場合）ので、行を引いて値で比べる。
func (e *editor) checkUniqueByIndex(ctx *sql.Context, txn *store.Txn, ki keyIndex, row sql.Row, exceptPK []byte) error {
	u := uniqueIndex{ords: ki.ords, name: ki.def.Name}
	for _, c := range ki.def.Columns {
		u.prefixes = append(u.prefixes, c.Length)
	}
	for _, ord := range ki.ords {
		if row[ord] == nil {
			return nil // NULL を含む行は一意制約の対象外（MySQL と同じ）
		}
	}
	prefix, err := e.t.valuePrefix(ki, row)
	if err != nil {
		return err
	}
	var conflict error
	err = txn.ScanIndexLatest(e.t.db.branch, e.t.def.ID, ki.def.ID, prefix, keyenc.PrefixEnd(prefix), false, func(entry, _ []byte) (bool, error) {
		pk, err := e.t.pkOfEntry(ki, entry)
		if err != nil {
			return false, err
		}
		if exceptPK != nil && string(pk) == string(exceptPK) {
			return true, nil
		}
		existing, ok, err := e.t.getByPK(txn, pk)
		if err != nil || !ok {
			return err == nil, err
		}
		match, err := e.t.columnsMatch(ctx, u, row, existing)
		if err != nil {
			return false, err
		}
		if match {
			conflict = e.t.uniqueDuplicateErr(row, u, e.t.visibleRow(existing))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return conflict
}

type uniqueIndex struct {
	ords     []int
	prefixes []int64
	// name はインデックスの名前（重なりのエラーの文に書く）。
	name string
}

// columnsMatch は、一意インデックスの列について a と b が等しいかを、列の型（照合順序を含む）で比べる。
// プレフィックス長は MySQL と同じく文字数で数える。
func (t *Table) columnsMatch(ctx *sql.Context, u uniqueIndex, a, b sql.Row) (bool, error) {
	for i, ord := range u.ords {
		va, vb := prefix(a[ord], u.prefixes[i]), prefix(b[ord], u.prefixes[i])
		if vb == nil {
			return false, nil
		}
		c, err := t.sch.Schema[ord].Type.Compare(ctx, va, vb)
		if err != nil {
			return false, err
		}
		if c != 0 {
			return false, nil
		}
	}
	return true, nil
}

func prefix(v any, n int64) any {
	if n <= 0 {
		return v
	}
	switch s := v.(type) {
	case string:
		if int64(utf8.RuneCountInString(s)) > n {
			return string([]rune(s)[:n])
		}
	case []byte:
		if int64(len(s)) > n {
			return s[:n]
		}
	}
	return v
}

// formatCols は、重複のエラーに出す列の値を書く。バイト列はそのまま（読めなければ 0x の 16 進。bytesString）、
// 日時は日付と時刻にする（段階 70。列の型を見ないので、DATE にも 00:00:00 が付く）。
func formatCols(row sql.Row, ords []int) string {
	parts := make([]string, len(ords))
	for i, ord := range ords {
		switch v := row[ord].(type) {
		case []byte:
			parts[i] = bytesString(v)
		case time.Time:
			parts[i] = v.Format("2006-01-02 15:04:05.999999")
		default:
			parts[i] = fmt.Sprintf("%v", v)
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// pkDuplicateErr は、row の主キー（主キーのない表では、行を並べる一意インデックス）が既存の行 existing と重なる
// エラー。isPK は sql.NewUniqueKeyErr と同じ。
func (t *Table) pkDuplicateErr(row sql.Row, isPK bool, existing sql.Row) error {
	name := "PRIMARY"
	if t.def.Keyless {
		for _, idx := range t.def.Indexes {
			if idx.Unique && len(idx.Columns) == len(t.def.PKOrdinals) && slices.EqualFunc(idx.Columns, t.def.PKOrdinals,
				func(c schema.IndexColumn, ord int) bool { return t.def.ColumnIndex(c.Name) == ord }) {
				name = idx.Name
				break
			}
		}
	}
	return t.duplicateErr(row, t.def.PKOrdinals, nil, isPK, existing, name)
}

// uniqueDuplicateErr は、row が一意インデックス u で既存の行 existing と重なるエラー。
func (t *Table) uniqueDuplicateErr(row sql.Row, u uniqueIndex, existing sql.Row) error {
	return t.duplicateErr(row, u.ords, u.prefixes, false, existing, u.name)
}

// duplicateErr は、row の列 ords（プレフィックス長 prefixes）がインデックス index で既存の行と重なるエラー。文に書く値は
// 文を返すときにだけ作るので、キーの列の値だけを写しておく。
func (t *Table) duplicateErr(row sql.Row, ords []int, prefixes []int64, isPK bool, existing sql.Row, index string) error {
	key := make(sql.Row, len(row))
	for _, ord := range ords {
		key[ord] = row[ord]
	}
	return sql.NewDuplicateEntryErr(formatCols(row, ords), isPK, existing,
		func() string { return t.duplicateEntry(key, ords, prefixes) }, t.def.Name+"."+index)
}

// duplicateEntryBytes は、MySQL が重なりのエラーの文に書く値の長さの上限（ER_DUP_ENTRY_WITH_KEY_NAME の %-.64s）。
const duplicateEntryBytes = 64

// duplicateEntry は、重なりのエラーの文に書く値を MySQL と同じ形にする（docs/plan.md の段階 90）。列の値を列の型の
// 文字列（日時は列の桁、DECIMAL は列の小数の桁）にして - でつなぎ、プレフィックスの索引の列はその文字数だけにする。
// バイナリの列の表示できないバイトは \xHH にする。64 バイトで切る（MySQL は文字の途中でも切るが、HeimDB は文字の
// 境目で切る）。
func (t *Table) duplicateEntry(row sql.Row, ords []int, prefixes []int64) string {
	ctx := sql.NewEmptyContext()
	parts := make([]string, len(ords))
	for i, ord := range ords {
		v := row[ord]
		if prefixes != nil {
			v = prefix(v, prefixes[i])
		}
		typ := t.sch.Schema[ord].Type
		s := fmt.Sprintf("%v", v)
		if sv, err := typ.SQL(ctx, nil, v); err == nil {
			s = sv.ToString()
		}
		if types.IsBinaryType(typ) {
			var b strings.Builder
			for i := 0; i < len(s); i++ {
				if c := s[i]; c < 0x20 || c >= 0x7f {
					fmt.Fprintf(&b, "\\x%02X", c)
				} else {
					b.WriteByte(c)
				}
			}
			s = b.String()
		}
		parts[i] = s
	}
	s := strings.Join(parts, "-")
	if len(s) > duplicateEntryBytes {
		n := duplicateEntryBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}

// observeAutoIncrement は、行に入った AUTO_INCREMENT 列の値でカウンタを進める。
func (e *editor) observeAutoIncrement(ctx *sql.Context, row sql.Row) error {
	ord := e.t.autoIncrementOrdinal()
	if ord < 0 || row[ord] == nil {
		return nil
	}
	c, err := e.t.autoIncrement(ctx)
	if err != nil {
		return err
	}
	v, inRange, err := types.Uint64.Convert(ctx, row[ord])
	if err != nil || inRange != sql.InRange {
		return nil // 負の値などはカウンタに影響しない
	}
	c.Observe(v.(uint64))
	return nil
}

// SetAutoIncrementValue は sql.AutoIncrementSetter の実装（ALTER TABLE ... AUTO_INCREMENT = v）。
func (e *editor) SetAutoIncrementValue(ctx *sql.Context, v uint64) error {
	c, err := e.t.autoIncrement(ctx)
	if err != nil {
		return err
	}
	floor, err := e.t.maxAutoIncrementValue(ctx)
	if err != nil {
		return err
	}
	c.Set(v, floor+1)
	return nil
}

// AcquireAutoIncrementLock は sql.AutoIncrementSetter の実装。カウンタ自体が排他制御をしているので何もしない。
func (e *editor) AcquireAutoIncrementLock(*sql.Context) (func(), error) {
	return func() {}, nil
}
