package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/batchrepr"
	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// uncommittedTS は、コミット前のバージョンのキーに入れる仮の時刻。反転すると 0 になり、同じ行のどのバージョンよりも前
// （新しい側）に並ぶので、自分の未コミットの変更が最新のバージョンとして見える。コミット時に本当の時刻に書き換える。
const uncommittedTS = Latest

// ErrMultipleBranches は、1 つのトランザクションで複数のブランチに書こうとしたことを表す。
var ErrMultipleBranches = errors.New("HeimDB: a transaction can write to only one branch")

// ErrTxnDone は、終わったトランザクションを使おうとしたことを表す。
var ErrTxnDone = errors.New("HeimDB: transaction already committed or rolled back")

// MaxTxnBytes は、コミットしていない書き込み（バッチ）の大きさの上限（docs/plan.md の段階 59）。コミットしていない
// 書き込みはメモリに持つ。接続のトランザクションは、メモリの上限（-memory-limit）より先にこれで止める。テストで変える。
var MaxTxnBytes = 1536 << 20

// MaxInternalTxnBytes は、マージと復元のトランザクション（BeginInternal）の上限。分けて書けないので、Pebble が
// 受け付ける限りまで使う。Pebble のバッチは 4 GiB で panic し、コミットの記録を残すときはコミットのバッチに同じ中身を
// 2 回入れるので、その半分より小さくする。テストで変える。
var MaxInternalTxnBytes = 1900 << 20

// errTxnTooLarge は、コミットしていない書き込みが上限を超えたことを表す（MySQL のエラー 1197、ER_TRANS_CACHE_FULL）。
// 書こうとした文は呼び出し側が取り消す。
func (tx *Txn) errTxnTooLarge() error {
	switch tx.op {
	case "":
		return mysql.NewSQLError(1197, mysql.SSUnknownSQLState, "HeimDB: the transaction writes more than %d MiB, which HeimDB keeps in memory until COMMIT; commit in smaller transactions (with mariadb-dump, use --skip-no-autocommit)", MaxTxnBytes>>20)
	case "restore":
		return mysql.NewSQLError(1197, mysql.SSUnknownSQLState, "HeimDB: the restore writes more than %d MiB, which HeimDB keeps in memory until it commits; restore fewer tables at a time", MaxInternalTxnBytes>>20)
	default:
		return mysql.NewSQLError(1197, mysql.SSUnknownSQLState, "HeimDB: the %s writes more than %d MiB, which HeimDB keeps in memory until it commits", tx.op, MaxInternalTxnBytes>>20)
	}
}

// maxBytes は、このトランザクションの書き込みの上限。
func (tx *Txn) maxBytes() int {
	if tx.op != "" {
		return MaxInternalTxnBytes
	}
	return MaxTxnBytes
}

// batchRecordOverhead は、バッチの 1 件の書き込みがキーと値のほかに使うバイト数の上限（種別と 2 つの長さの varint）。
const batchRecordOverhead = 1 + 2*binary.MaxVarintLen32

// IsTxnTooLarge は、err が MaxTxnBytes を超えた書き込みのエラーかどうか。
func IsTxnTooLarge(err error) bool {
	var sqlErr *mysql.SQLError
	return errors.As(err, &sqlErr) && sqlErr.Num == 1197 && len(sqlErr.Message) > 8 && sqlErr.Message[:8] == "HeimDB: "
}

// DefaultLockTimeout は、書き込みロックを待つ時間の既定値（MySQL の innodb_lock_wait_timeout の既定と同じ）。
const DefaultLockTimeout = 50 * time.Second

// Txn は 1 つのセッションのトランザクション。書き込みはコミットまで Pebble の IndexedBatch に溜める。
//
// 分離（docs/plan.md の段階 5）:
//   - 読み取りは、最初に読んだ時点のコミット時刻（スナップショット）以前のバージョンと、自分の未コミットの変更を見る。
//   - 書き込むトランザクションは、ブランチの書き込みロックを COMMIT か ROLLBACK まで持つ。書き込みはブランチごとに
//     一度に 1 トランザクションだけなので、一意制約などの検査は「最新の確定済みデータ + 自分の変更」で行えば正しい。
//   - スナップショットより後に他のトランザクションが変更した行を書こうとしたら、ErrLockDeadlock（MySQL のエラー 1213）を返す。
type Txn struct {
	s     *Store
	batch *pebble.Batch
	// snapshot は読み取りの時点。0 ならまだ読んでいない。
	snapshot uint64
	// LockTimeout は書き込みロックを待つ時間の上限。Store.LockTimeout で始まる。
	LockTimeout time.Duration
	// Waits は、このトランザクションが待った時間を足す先（nil なら数えない。docs/plan.md の段階 81）。
	Waits *Waits
	// Owner は、書き込みロックの持ち主として記録する接続 ID（docs/plan.md の段階 82）。Cancel は、今の文が止められたら
	// 閉じるチャネルで、閉じたらロックを待つのをやめる（文ごとに入れ直す。nil なら待ち切る）。
	Owner  uint32
	Cancel <-chan struct{}
	// locks は、持っている書き込みロックを解放する関数（ブランチごと）。
	locks map[uint64]func()
	// wrote は、書き込んだブランチ。
	wrote map[uint64]bool
	// DDL は、実行中の DDL 文のために始めたトランザクションかどうか（backend.Session.beginDDL を参照）。
	DDL bool
	// 書き込みの位置（文の始め、Mark、印）は、バッチの件数（Count。Set と Delete の数）で数える。文の取り消しは、
	// バッチを先頭から読み直して作り直す（docs/plan.md の段階 59。それまでは同じ書き込みを別の記録にも持っていた）。
	// stmtStart は実行中の文が始まった時点の件数。stmtDepth は入れ子の深さ。
	stmtStart int
	stmtDepth int
	// retired は、文の取り消しで使わなくなったバッチ。開いたままのイテレータがありうるので、次のコマンドの始め
	// （ResetStatement）で走査が残っていなければ閉じる。残っていればトランザクションの終わりまで閉じない。
	retired []*pebble.Batch
	// op は、マージや復元のトランザクション（BeginInternal）の操作の名前。接続のトランザクションでは空。
	op   string
	done bool
	// readers は、主キーで 1 行を引くのに使い回す RowReader（PointReader を参照）。書き込むと捨てる。
	readers map[[2]uint64]*RowReader
	// scans は、開いている走査（RowScan、IndexScan）。トランザクションの終わりに閉じる。
	scans map[*Scan]struct{}
	// notes は、書き込みに添えた印（Note を参照）。
	notes []txnNote
	// closeMu は、トランザクションを終わらせる処理（close）を 1 回にする。トランザクションはほかの点では 1 つの
	// ゴルーチンから使うが、Store.Close のロールバック（docs/plan.md の段階 41）と、接続が切れたときのロールバックは
	// 別のゴルーチンから同時に来うる。
	closeMu sync.Mutex
}

type txnNote struct {
	pos int
	v   any
}

// Begin は新しいトランザクションを始める。
func (s *Store) Begin() *Txn {
	tx := &Txn{s: s, batch: s.db.NewIndexedBatch(), LockTimeout: s.LockTimeout, locks: map[uint64]func(){}, wrote: map[uint64]bool{}}
	s.openMu.Lock()
	s.openTxns[tx] = struct{}{}
	s.openMu.Unlock()
	return tx
}

// BeginInternal は、マージや復元が書くトランザクションを始める。書き込みの上限は MaxInternalTxnBytes で、超えたときの
// エラーは操作の名前 op で案内する。
func (s *Store) BeginInternal(op string) *Txn {
	tx := s.Begin()
	tx.op = op
	return tx
}

// LockForWrite はブランチの書き込みロックを取る。持っていれば何もしない。
// 書き込む文は、行を読む前にこれを呼ぶ。そうすればスナップショットがロックの後に決まり、待っている間に
// 他のトランザクションがコミットした変更も見えるので、自動コミットの文が競合で失敗しない。
func (tx *Txn) LockForWrite(branch uint64) error {
	if tx.done {
		return ErrTxnDone
	}
	if _, ok := tx.locks[branch]; ok {
		return nil
	}
	// 1 つのトランザクションが書けるのは 1 つのブランチだけ（一時テーブルの隠しブランチは別）。複数のブランチの
	// ロックを順不同で取るとデッドロックしうるので。ただし、まだ何も書いていなければ、先に取ったロック（接続中の
	// データベースのブランチを文の前に取ったもの）を放して付け替える（他のブランチのテーブルへの書き込み）。
	if !IsHiddenBranch(branch) {
		for held, release := range tx.locks {
			if IsHiddenBranch(held) {
				continue
			}
			if tx.wrote[held] {
				return ErrMultipleBranches
			}
			release()
			delete(tx.locks, held)
		}
	}
	start := time.Now()
	release, err := tx.s.LockBranchFor(branch, tx.LockTimeout, LockOwner{Conn: tx.Owner, Op: tx.op}, tx.Cancel)
	if tx.Waits != nil {
		tx.Waits.Lock += time.Since(start)
	}
	if err != nil {
		return err
	}
	tx.locks[branch] = release
	return nil
}

// HoldsLock は、ブランチの書き込みロックを持っているかを返す。
func (tx *Txn) HoldsLock(branch uint64) bool {
	_, ok := tx.locks[branch]
	return ok
}

// ReadTS は読み取りの時点（スナップショット）を返す。まだ決まっていなければ今決める。
func (tx *Txn) ReadTS() uint64 {
	return tx.readTS()
}

// Done は、トランザクションが終わっている（コミットかロールバックをした）かどうかを返す。
func (tx *Txn) Done() bool {
	return tx.done
}

// RefreshSnapshot は、まだ何も書いていないトランザクションの読み取りの時点を捨て、次の読み取りで最新に決め直す。
// 1 つの ALTER TABLE に複数の操作があると、go-mysql-server は操作ごとにテーブルを書き直す。DDL は別のトランザクションで
// すぐにコミットするので、次の操作が行を読む前に、書き直した結果が見えるようにする（backend.Session.beginDDL）。
func (tx *Txn) RefreshSnapshot() {
	if tx.done || tx.batch.Count() > 0 || tx.snapshot == 0 {
		return
	}
	tx.s.unregisterSnapshot(tx)
	tx.snapshot = 0
	tx.dropReaders() // 読み手は前の読み取りの時点を持つ
}

// readTS は読み取りの時点を返す。最初の読み取りで、その時点の最後のコミット時刻に決める。
func (tx *Txn) readTS() uint64 {
	if tx.snapshot == 0 {
		if tx.done {
			return tx.s.LastCommitted() // 終わったトランザクションは登録しない（GC の境界を止めないため）
		}
		tx.snapshot = tx.s.registerSnapshot(tx)
	}
	return tx.snapshot
}

// InsertRow は新しい行を書く。主キーの重複は呼び出し側が最新のデータで確かめている。
// スナップショットの後に他のトランザクションがこの主キーの行を削除していても、競合にしない（MySQL と同じく挿入できる）。
func (tx *Txn) InsertRow(branch, tableID uint64, pk, value []byte) error {
	if err := tx.prepareRowWrite(branch, tableID, pk, true); err != nil {
		return err
	}
	return tx.set(rowKey(branch, tableID, pk, uncommittedTS), append([]byte{valData}, value...))
}

// PutRow は既存の行の新しいバージョンを書く（UPDATE）。pk は rowenc.EncodePK で符号化した主キー、value は rowenc.EncodeRow の結果。
func (tx *Txn) PutRow(branch, tableID uint64, pk, value []byte) error {
	if err := tx.prepareRowWrite(branch, tableID, pk, false); err != nil {
		return err
	}
	return tx.set(rowKey(branch, tableID, pk, uncommittedTS), append([]byte{valData}, value...))
}

// DeleteRow は行に削除の印を書く。
func (tx *Txn) DeleteRow(branch, tableID uint64, pk []byte) error {
	if err := tx.prepareRowWrite(branch, tableID, pk, false); err != nil {
		return err
	}
	return tx.set(rowKey(branch, tableID, pk, uncommittedTS), []byte{valTombstone})
}

// prepareRowWrite は、行を書く前に書き込みロックを取り、スナップショットより後に他のトランザクションが
// その行を変更していないかを確かめる。変更されていたら、古い値をもとにした更新になるので ErrLockDeadlock を返す。
// insert が true なら、最新のバージョンが削除の印のとき（他のトランザクションが削除した主キーに挿入するとき）は許す。
func (tx *Txn) prepareRowWrite(branch, tableID uint64, pk []byte, insert bool) error {
	if err := tx.LockForWrite(branch); err != nil {
		return err
	}
	tx.wrote[branch] = true
	if tx.snapshot == 0 || tx.s.LastCommitted() <= tx.snapshot {
		return nil
	}
	ts, deleted, ok, err := tx.s.lastCommittedVersion(branch, tableID, pk)
	if err != nil {
		return err
	}
	if ok && ts > tx.snapshot && !(insert && deleted) {
		return sql.ErrLockDeadlock.New("a row was changed by another transaction after this transaction read it")
	}
	return nil
}

// lastCommittedVersion は、行の最新の確定済みのバージョンの時刻と、それが削除の印かどうかを返す。
func (s *Store) lastCommittedVersion(branch, tableID uint64, pk []byte) (ts uint64, deleted, ok bool, err error) {
	prefix := append(RowPrefix(branch, tableID), pk...)
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return 0, false, false, err
	}
	defer iter.Close()
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) != len(prefix)+8 {
			continue // 主キーが pk で始まる別の行（固定長の主キーでは起きない）
		}
		ts, _, err := keyenc.DecodeInvertedTime(iter.Key()[len(prefix):])
		v := iter.Value()
		return ts, len(v) > 0 && v[0] == valTombstone, err == nil, err
	}
	return 0, false, false, iter.Error()
}

// PutIndexEntry はインデックスのエントリを書く。entry は列の値と主キーを符号化したもの、value はエントリの値
// （列の元の値。docs/plan.md の段階 27）。対応する行の PutRow か DeleteRow と一緒に呼ぶので、ロックと競合の確認は
// そちらで済んでいる。
func (tx *Txn) PutIndexEntry(branch, tableID, indexID uint64, entry, value []byte) error {
	return tx.set(indexKey(branch, tableID, indexID, entry, uncommittedTS), append([]byte{valData}, value...))
}

// DeleteIndexEntry はインデックスのエントリに削除の印を書く。
func (tx *Txn) DeleteIndexEntry(branch, tableID, indexID uint64, entry []byte) error {
	return tx.set(indexKey(branch, tableID, indexID, entry, uncommittedTS), []byte{valTombstone})
}

func (tx *Txn) set(key, value []byte) error {
	if tx.done {
		return ErrTxnDone
	}
	n := tx.batch.Len() + batchRecordOverhead + len(key) + len(value)
	if n > tx.maxBytes() {
		return tx.errTxnTooLarge()
	}
	if check := tx.s.AllocCheck; check != nil {
		// バッチが伸びるときは、倍の大きさを新しく確保して写す。大きなバッチでは、その確保の前に確かめる
		// （docs/plan.md の段階 59）。
		if c := cap(tx.batch.Repr()); n > c && c >= 1<<20 {
			if err := check(2 * c); err != nil {
				return err
			}
		}
	}
	tx.dropReaders()
	return tx.batch.Set(key, value, nil)
}

// deleteCommitted は、コミットした記録のキー key をそのまま消す（時刻を付け直さない。DropMergeBasesAfter）。
func (tx *Txn) deleteCommitted(key []byte) error {
	if tx.done {
		return ErrTxnDone
	}
	return tx.batch.Delete(key, nil)
}

// count は、ここまでの書き込みの数（位置）。
func (tx *Txn) count() int {
	return int(tx.batch.Count())
}

// PointReader は、ブランチ branch のテーブル tableID の行を主キーで引く RowReader を、トランザクションの中で使い
// 回す（JOIN で左の行ごとに 1 行を引くとき、引くたびに層の数だけイテレータを作らない。docs/plan.md の段階 27）。
// 呼び出し側は Close しない。トランザクションが書き込むか終わると閉じる（RowReader は作ったときの変更までしか
// 見ないため）。同じトランザクションを複数のゴルーチンから同時に使ってはいけない（ほかの読み出しと同じ）。
func (tx *Txn) PointReader(branch, tableID uint64) (*RowReader, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	k := [2]uint64{branch, tableID}
	if r, ok := tx.readers[k]; ok {
		return r, nil
	}
	r, err := tx.RowReader(branch, tableID)
	if err != nil {
		return nil, err
	}
	if tx.readers == nil {
		tx.readers = map[[2]uint64]*RowReader{}
	}
	tx.readers[k] = r
	return r, nil
}

func (tx *Txn) dropReaders() {
	for k, r := range tx.readers {
		_ = r.Close()
		delete(tx.readers, k)
	}
}

// StatementBegin は文の開始を記録する。go-mysql-server は 1 つの文で複数のテーブルエディタを開くことがあるので、
// 入れ子を数え、最も外側の開始時点を取り消しの戻り先にする。
func (tx *Txn) StatementBegin() {
	if tx.stmtDepth == 0 {
		tx.stmtStart = tx.count()
	}
	tx.stmtDepth++
}

// ResetStatement は文の入れ子の数を 0 に戻す。コマンドの始まりに呼び、前のコマンドで対にならなかった
// StatementBegin の影響を残さない。
func (tx *Txn) ResetStatement() {
	tx.stmtDepth = 0
	// 前のコマンドで取り消した文のバッチを手放す。走査が残っていなければ、古いバッチを読むものはない（コールバックで
	// 読む走査はコマンドの中で終わる）。手放さないと、1197 で止まった文を同じトランザクションで繰り返すたびに、
	// 上限に近い大きさのバッチが溜まる（docs/plan.md の段階 59）。
	if len(tx.scans) == 0 {
		for _, b := range tx.retired {
			b.Close()
		}
		tx.retired = nil
	}
}

// StatementComplete は文の成功を記録する。
func (tx *Txn) StatementComplete() {
	if tx.stmtDepth > 0 {
		tx.stmtDepth--
	}
}

// DiscardStatement は、実行中の文の書き込みをすべて取り消す。
func (tx *Txn) DiscardStatement() error {
	if tx.done {
		return ErrTxnDone
	}
	if tx.stmtDepth > 0 {
		tx.stmtDepth--
	}
	return tx.truncate(tx.stmtStart)
}

// Size は、トランザクションの書き込みの大きさ（メモリに持つバッチのバイト数）を返す。
func (tx *Txn) Size() int {
	if tx.done || tx.batch == nil {
		return 0
	}
	return tx.batch.Len()
}

// Mark は、ここまでの書き込みの位置を返す（セーブポイント。docs/plan.md の段階 17）。
func (tx *Txn) Mark() int {
	return tx.count()
}

// RollbackTo は、位置 mark（Mark が返した値）より後の書き込みを取り消す。MySQL の ROLLBACK TO SAVEPOINT と同じく、
// 書き込みのロックと AUTO_INCREMENT の払い出しは戻さない。
func (tx *Txn) RollbackTo(mark int) error {
	if tx.done {
		return ErrTxnDone
	}
	return tx.truncate(mark)
}

// Note は、位置 pos（書き込む前に Mark で得た値）からの書き込みに印 v を添える。文の取り消しやセーブポイントへの
// ロールバックでその書き込みを取り消すと、印も消える。変更の見張り（docs/plan.md の段階 33）が使う。
func (tx *Txn) Note(pos int, v any) {
	tx.notes = append(tx.notes, txnNote{pos, v})
}

// Notes は、残っている印を添えた順に返す。Commit の前に呼ぶ。
func (tx *Txn) Notes() []any {
	out := make([]any, len(tx.notes))
	for i, n := range tx.notes {
		out[i] = n.v
	}
	return out
}

// truncate は、書き込みを最初の n 件に切り詰める。古いバッチの先頭の n 件を新しいバッチに写して入れ替える。古い
// バッチは、開いたままのイテレータ（文の途中の走査）が読むことがあるので、すぐには閉じず、次のコマンドの始め
// （ResetStatement）で走査が残っていなければ閉じる（閉じるとメモリがほかのバッチに使い回される）。
func (tx *Txn) truncate(n int) error {
	for len(tx.notes) > 0 && tx.notes[len(tx.notes)-1].pos >= n {
		tx.notes = tx.notes[:len(tx.notes)-1]
	}
	if n >= tx.count() {
		return nil
	}
	tx.dropReaders()
	old := tx.batch
	// 写す n 件の大きさを先に数え、新しいバッチを要る大きさで確保する（倍々に伸ばすと、上限に近いバッチの取り消しで
	// 古いバッチの 2 倍近くを確保する）。
	size := batchrepr.HeaderLen
	r := old.Reader()
	for i := 0; i < n; i++ {
		_, key, value, ok, err := r.Next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("HeimDB: the transaction has %d writes, fewer than %d", i, n)
		}
		size += batchRecordOverhead + len(key) + len(value)
	}
	// 写す分の確保もメモリの上限で確かめる。収まらなければ、写さずにトランザクションごと取り消す（1041 で止めた文の
	// 取り消しは、この後トランザクションごと捨てるので、写しても無駄。docs/plan.md の段階 59）。
	if check := tx.s.AllocCheck; check != nil && size >= 1<<20 {
		if err := check(size); err != nil {
			tx.closeOnce()
			return err
		}
	}
	tx.retired = append(tx.retired, old)
	tx.batch = tx.s.db.NewIndexedBatchWithSize(size)
	r = old.Reader()
	for i := 0; i < n; i++ {
		kind, key, value, ok, err := r.Next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("HeimDB: the transaction has %d writes, fewer than %d", i, n)
		}
		switch kind {
		case pebble.InternalKeyKindSet:
			err = tx.batch.Set(key, value, nil)
		case pebble.InternalKeyKindDelete:
			err = tx.batch.Delete(key, nil)
		default:
			err = fmt.Errorf("HeimDB: unexpected write kind %v in a transaction", kind)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Commit は書き込みを確定する。コミット時刻を発行し、仮の時刻を入れたキーを書き換えて 1 つのバッチで書く。
func (tx *Txn) Commit() error {
	// 終わったかの確かめと書き込みを、Store.Close のロールバックと同時に行わない（docs/plan.md の段階 43）。
	tx.closeMu.Lock()
	defer tx.closeMu.Unlock()
	if tx.done {
		return ErrTxnDone
	}
	defer tx.close()
	if tx.count() == 0 {
		return tx.s.tryFlushAutoIncrements() // 読みだけのコミットは、書き込みのロックを待たない
	}
	// 書き込みを順に読み、仮の時刻のキーをコミット時刻に付け直してコミットのバッチに写す。コミットのバッチは要る大きさで
	// 確保し、キーと値は直接書き込む（倍々に伸ばしたり、キーを 1 件ずつ作ったりしない。docs/plan.md の段階 59）。
	return tx.s.updateWaits(tx.batch.Len(), tx.Waits, func(w *WriteTx) error {
		r := tx.batch.Reader()
		for i := 0; ; i++ {
			kind, key, value, ok, err := r.Next()
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if check := tx.s.CommitCheck; check != nil && i%1024 == 1023 {
				if err := check(); err != nil {
					return err
				}
			}
			switch kind {
			case pebble.InternalKeyKindDelete:
				err = w.batch.Delete(key, nil)
			case pebble.InternalKeyKindSet:
				if len(key) < 8 {
					return fmt.Errorf("HeimDB: a transaction wrote a key without a timestamp: %x", key)
				}
				op := w.batch.SetDeferred(len(key), len(value))
				n := copy(op.Key, key[:len(key)-8])
				keyenc.AppendInvertedTime(op.Key[:n], w.TS) // op.Key の残りの 8 バイトに書く
				copy(op.Value, value)
				err = op.Finish()
			default:
				err = fmt.Errorf("HeimDB: unexpected write kind %v in a transaction", kind)
			}
			if err != nil {
				return err
			}
		}
	})
}

// Rollback は書き込みをすべて捨てる。終わったトランザクションでは何もしない。
func (tx *Txn) Rollback() error {
	tx.closeOnce()
	return nil
}

// closeOnce は、まだ終わっていなければトランザクションを終わらせる。
func (tx *Txn) closeOnce() {
	tx.closeMu.Lock()
	defer tx.closeMu.Unlock()
	if !tx.done {
		tx.close()
	}
}

func (tx *Txn) close() {
	tx.done = true
	tx.s.openMu.Lock()
	delete(tx.s.openTxns, tx)
	tx.s.openMu.Unlock()
	tx.dropReaders()
	for sc := range tx.scans {
		_ = sc.Close()
	}
	if tx.snapshot != 0 {
		tx.s.unregisterSnapshot(tx)
	}
	for branch, release := range tx.locks {
		release()
		delete(tx.locks, branch)
	}
	tx.batch.Close()
	for _, b := range tx.retired {
		b.Close()
	}
	tx.retired = nil
	tx.notes = nil
}

// RowVersion は、走査で見つかった行の最新のバージョン。
type RowVersion struct {
	// PK は rowenc.EncodePK で符号化した主キー。
	PK []byte
	// Value は rowenc.EncodeRow の結果。
	Value []byte
}

// ScanRows は、テーブルの行のうち主キーが [lo, hi) にあるものを、主キーの順に fn へ渡す。
// lo と hi は符号化した主キー（またはその先頭部分）で、nil は上限・下限なしを表す。
// reverse が true なら主キーの降順に渡す。削除の印が最新の行は渡さない。fn が false を返したら止める。
// スナップショット以前の確定済みのデータに、このトランザクションの未コミットの変更を重ねて見る。
func (tx *Txn) ScanRows(branch, tableID uint64, lo, hi []byte, reverse bool, fn func(RowVersion) (bool, error)) error {
	if tx.done {
		return ErrTxnDone
	}
	return tx.s.scanBranch(branch, tx.batch.NewIter, rowPrefixFor(tableID), tx.snapshotVisible(), lo, hi, reverse, rowCallback(fn))
}

// RowScan は ScanRows と同じ行を 1 行ずつ返す走査を開く（エントリは主キー）。作った時点の自分の変更までを見て、
// 後から書いた行は見えない（Scan を参照）。使い終わったら Close する。閉じなければトランザクションの終わりに閉じる。
func (tx *Txn) RowScan(branch, tableID uint64, lo, hi []byte, reverse bool) (*Scan, error) {
	return tx.openScan(rowPrefixFor(tableID), lo, hi, reverse, branch)
}

// IndexScan は ScanIndex と同じエントリを 1 件ずつ返す走査を開く（RowScan を参照）。
func (tx *Txn) IndexScan(branch, tableID, indexID uint64, lo, hi []byte, reverse bool) (*Scan, error) {
	return tx.openScan(indexPrefixFor(tableID, indexID), lo, hi, reverse, branch)
}

func (tx *Txn) openScan(prefixOf func(uint64) []byte, lo, hi []byte, reverse bool, branch uint64) (*Scan, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	sc, err := tx.s.openBranchScan(branch, tx.batch.NewIter, prefixOf, tx.snapshotVisible(), lo, hi, reverse)
	if err != nil {
		return nil, err
	}
	if tx.scans == nil {
		tx.scans = map[*Scan]struct{}{}
	}
	tx.scans[sc] = struct{}{}
	sc.onClose = func(sc *Scan) { delete(tx.scans, sc) }
	return sc, nil
}

// ScanRowsLatest は ScanRows と同じだが、スナップショットではなく最新の確定済みデータに自分の変更を重ねて見る。
// 書き込みロックを持った状態で、一意制約などの検査に使う。
func (tx *Txn) ScanRowsLatest(branch, tableID uint64, lo, hi []byte, reverse bool, fn func(RowVersion) (bool, error)) error {
	if tx.done {
		return ErrTxnDone
	}
	return tx.s.scanBranch(branch, tx.batch.NewIter, rowPrefixFor(tableID), allVisible, lo, hi, reverse, rowCallback(fn))
}

// ScanRowsAsOf は ScanRows と同じだが、時刻 asOf の時点で確定していたバージョンを読む。未コミットの変更は含めない。
func (s *Store) ScanRowsAsOf(branch, tableID, asOf uint64, lo, hi []byte, reverse bool, fn func(RowVersion) (bool, error)) error {
	return s.scanBranch(branch, s.db.NewIter, rowPrefixFor(tableID), asOfVisible(asOf), lo, hi, reverse, rowCallback(fn))
}

// RowScanAsOf は ScanRowsAsOf と同じ行を 1 行ずつ返す走査を開く。使い終わったら Close する。
func (s *Store) RowScanAsOf(branch, tableID, asOf uint64, lo, hi []byte, reverse bool) (*Scan, error) {
	return s.openBranchScan(branch, s.db.NewIter, rowPrefixFor(tableID), asOfVisible(asOf), lo, hi, reverse)
}

// IndexScanAsOf は、時刻 asOf の時点で確定していたインデックスのエントリを 1 件ずつ返す走査を開く。
func (s *Store) IndexScanAsOf(branch, tableID, indexID, asOf uint64, lo, hi []byte, reverse bool) (*Scan, error) {
	return s.openBranchScan(branch, s.db.NewIter, indexPrefixFor(tableID, indexID), asOfVisible(asOf), lo, hi, reverse)
}

// snapshotVisible は、スナップショット以前のバージョンと自分の未コミットのバージョンを見せる。
func (tx *Txn) snapshotVisible() func(uint64) bool {
	snap := tx.readTS()
	return func(ts uint64) bool { return ts <= snap || ts == uncommittedTS }
}

func asOfVisible(asOf uint64) func(uint64) bool {
	return func(ts uint64) bool { return ts <= asOf }
}

func allVisible(uint64) bool { return true }

func rowCallback(fn func(RowVersion) (bool, error)) func(entry, value []byte) (bool, error) {
	return func(entry, value []byte) (bool, error) {
		return fn(RowVersion{PK: append([]byte(nil), entry...), Value: append([]byte(nil), value...)})
	}
}

// ScanIndex は、インデックスのエントリのうち [lo, hi) にあるものを、エントリの値と一緒に順に fn へ渡す。範囲と
// 順序の扱いは ScanRows と同じ。
func (tx *Txn) ScanIndex(branch, tableID, indexID uint64, lo, hi []byte, reverse bool, fn func(entry, value []byte) (bool, error)) error {
	if tx.done {
		return ErrTxnDone
	}
	return tx.s.scanBranch(branch, tx.batch.NewIter, indexPrefixFor(tableID, indexID), tx.snapshotVisible(), lo, hi, reverse, fn)
}

// ScanIndexLatest は ScanIndex と同じだが、最新の確定済みデータに自分の変更を重ねて見る（ScanRowsLatest を参照）。
func (tx *Txn) ScanIndexLatest(branch, tableID, indexID uint64, lo, hi []byte, reverse bool, fn func(entry, value []byte) (bool, error)) error {
	if tx.done {
		return ErrTxnDone
	}
	return tx.s.scanBranch(branch, tx.batch.NewIter, indexPrefixFor(tableID, indexID), allVisible, lo, hi, reverse, fn)
}

// ScanIndexAsOf は ScanIndex と同じだが、時刻 asOf の時点で確定していたエントリを読む。
func (s *Store) ScanIndexAsOf(branch, tableID, indexID, asOf uint64, lo, hi []byte, reverse bool, fn func(entry, value []byte) (bool, error)) error {
	return s.scanBranch(branch, s.db.NewIter, indexPrefixFor(tableID, indexID), asOfVisible(asOf), lo, hi, reverse, fn)
}

// GetRowLatest は、最新の確定済みデータに自分の変更を重ねて、主キー pk の行を返す。なければ ok が false。
// 書き込みロックを持った状態で、主キーの重複の検査などに使う。
func (tx *Txn) GetRowLatest(branch, tableID uint64, pk []byte) (value []byte, ok bool, err error) {
	hi := keyenc.PrefixEnd(pk)
	err = tx.ScanRowsLatest(branch, tableID, pk, hi, false, func(r RowVersion) (bool, error) {
		if string(r.PK) == string(pk) {
			value, ok = r.Value, true
		}
		return false, nil
	})
	return value, ok, err
}

func rowPrefixFor(tableID uint64) func(branch uint64) []byte {
	return func(branch uint64) []byte { return RowPrefix(branch, tableID) }
}

func indexPrefixFor(tableID, indexID uint64) func(branch uint64) []byte {
	return func(branch uint64) []byte { return IndexPrefix(branch, tableID, indexID) }
}
