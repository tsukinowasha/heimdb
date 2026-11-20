package store

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/pebble/v2"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// RowReader は、1 つのテーブルの行を主キーで 1 つずつ引く。層ごとのイテレータを作っておき、引くたびに位置を
// 移すだけにする（ScanRows で 1 行ずつ引くと、そのたびに層の数だけイテレータを作る）。インデックスで見つけた
// 行を読むのに使う。使い終わったら Close を呼ぶ。
//
// 見えるバージョンは、作ったときのトランザクションのスナップショット（AS OF なら時点）で決まる。作った後に同じ
// トランザクションで書いた変更は見えない。
type RowReader struct {
	layers []readerLayer
	buf    []byte
}

type readerLayer struct {
	iter    *pebble.Iterator
	prefix  []byte
	visible func(uint64) bool
}

// RowReader は、トランザクション tx から見たブランチ branch のテーブル tableID の行を引く RowReader を作る。
func (tx *Txn) RowReader(branch, tableID uint64) (*RowReader, error) {
	if tx.done {
		return nil, ErrTxnDone
	}
	return tx.s.newRowReader(branch, tableID, tx.batch.NewIter, tx.snapshotVisible())
}

// RowReaderAsOf は、時刻 asOf の時点で確定していた行を引く RowReader を作る。
func (s *Store) RowReaderAsOf(branch, tableID, asOf uint64) (*RowReader, error) {
	return s.newRowReader(branch, tableID, s.db.NewIter, asOfVisible(asOf))
}

func (s *Store) newRowReader(branch, tableID uint64, ownIter func(*pebble.IterOptions) (*pebble.Iterator, error), visible func(uint64) bool) (*RowReader, error) {
	ls, err := s.layers(branch)
	if err != nil {
		return nil, err
	}
	r := &RowReader{}
	for i, l := range ls {
		newIter := s.db.NewIter
		vis := visible
		if i == 0 {
			newIter = ownIter
		} else {
			maxTS := l.maxTS
			vis = func(ts uint64) bool { return ts <= maxTS && visible(ts) }
		}
		prefix := RowPrefix(l.branch, tableID)
		iter, err := newIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
		if err != nil {
			r.Close()
			return nil, err
		}
		r.layers = append(r.layers, readerLayer{iter: iter, prefix: prefix, visible: vis})
	}
	return r, nil
}

// Get は主キー pk の行を返す。なければ ok が false。pk は rowenc.EncodePK の結果（列の値はどの型も自分で
// 終わりが分かる形なので、あるエントリが別のエントリの先頭部分になることはない。docs/plan.md の段階 70）。
func (r *RowReader) Get(pk []byte) (value []byte, ok bool, err error) {
	v, ok, err := r.Peek(pk)
	if !ok || err != nil {
		return nil, ok, err
	}
	return append([]byte(nil), v...), true, nil
}

// Peek は Get と同じだが、値を写さずに返す。値は、次に Get か Peek を呼ぶか Close するまでしか使えない。
func (r *RowReader) Peek(pk []byte) (value []byte, ok bool, err error) {
	for _, l := range r.layers {
		r.buf = append(append(r.buf[:0], l.prefix...), pk...)
		// 同じエントリのバージョンは新しい順に並ぶので、見える最初のバージョンがこの層での値。
		for valid := l.iter.SeekGE(r.buf); valid; valid = l.iter.Next() {
			key := l.iter.Key()
			if !bytes.HasPrefix(key, r.buf) {
				break
			}
			if len(key) != len(r.buf)+8 {
				return nil, false, fmt.Errorf("HeimDB: corrupt versioned key %x", key)
			}
			ts, _, err := keyenc.DecodeInvertedTime(key[len(r.buf):])
			if err != nil {
				return nil, false, err
			}
			if !l.visible(ts) {
				continue
			}
			v := l.iter.Value()
			if len(v) == 0 {
				return nil, false, fmt.Errorf("HeimDB: empty value for entry %x", pk)
			}
			if v[0] == valTombstone {
				return nil, false, nil // 上の層の削除の印は下の層を隠す
			}
			return v[1:], true, nil
		}
		if err := l.iter.Error(); err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// Closed は、Close した後か（閉じた RowReader の Peek は、エラーにせずに行がないと答える）。
func (r *RowReader) Closed() bool {
	return r.layers == nil
}

// Close はイテレータを閉じる。
func (r *RowReader) Close() error {
	var first error
	for _, l := range r.layers {
		if err := l.iter.Close(); err != nil && first == nil {
			first = err
		}
	}
	r.layers = nil
	return first
}
