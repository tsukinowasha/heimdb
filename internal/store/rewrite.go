package store

import (
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/keyenc"
)

// 表の書き直し（ALTER TABLE）とインデックスの作成（CREATE INDEX）の途中のデータ（docs/plan.md の段階 53）。
//
// 書き直しは、新しい形の行を新しいテーブル ID に何回かに分けてコミットし、最後のコミットでテーブル定義をその ID に
// 向ける。それまでの新しい ID はどのテーブル定義からも指されないので、GC は使われていない表として消してしまう。
// そこで、書きかけの ID に印 m/rewrite/<ブランチID>/<テーブルID> を付け、GC は印の付いた ID を消さない。印はコミットに
// 含めて書くので、自分で GC を回すレプリカにも写る。文が失敗したら書いたデータと印を消し（DropRewrite）、プロセスが
// 途中で落ちたら、本番の起動時に消す（cleanupRewrites）。
//
// インデックスの作成も同じく、エントリを何回かに分けてコミットし、最後のコミットでテーブル定義にインデックスを足す。
// 作成中のインデックス ID は、そのブランチの定義の次のインデックス ID 以上だが、同じテーブル ID を共有するほかの
// ブランチの定義では次のインデックス ID より小さいことがあり、GC が使われていないインデックスとして消してしまう。
// そこで印 m/rewrite/<ブランチID>/<テーブルID>/<インデックスID> を付ける。

// rewriteID は、書き直しの途中のテーブル ID（index が 0）か、作成の途中のインデックス ID。
type rewriteID struct{ branch, table, index uint64 }

// MarkRewrite は、ブランチ branch のテーブル ID tableID を書き直しの途中として印を付ける。
func (tx *WriteTx) MarkRewrite(branch, tableID uint64) error {
	return tx.batch.Set(rewriteKey(branch, tableID), nil, nil)
}

// UnmarkRewrite は、書き直しの途中の印を外す（テーブル定義が ID を指すコミットで使う）。
func (tx *WriteTx) UnmarkRewrite(branch, tableID uint64) error {
	return tx.batch.Delete(rewriteKey(branch, tableID), nil)
}

// DropRewrite は、書き直しの途中のテーブル ID のデータと印を消す（書き直しの文が失敗したとき）。
func (tx *WriteTx) DropRewrite(branch, tableID uint64) error {
	p := tablePrefix(branch, tableID)
	if err := tx.batch.DeleteRange(p, keyenc.PrefixEnd(p), nil); err != nil {
		return err
	}
	return tx.UnmarkRewrite(branch, tableID)
}

// DropIndexData は、インデックス indexID のエントリをすべて消す。作成の途中のインデックス（まだテーブル定義になく、
// 次のインデックス ID 以上なので GC が消さない）の作り直しや、作成が失敗したときに使う。
func (tx *WriteTx) DropIndexData(branch, tableID, indexID uint64) error {
	p := IndexPrefix(branch, tableID, indexID)
	return tx.batch.DeleteRange(p, keyenc.PrefixEnd(p), nil)
}

// MarkIndexBuild は、テーブル tableID のインデックス ID indexID を作成の途中として印を付ける。
func (tx *WriteTx) MarkIndexBuild(branch, tableID, indexID uint64) error {
	return tx.batch.Set(indexBuildKey(branch, tableID, indexID), nil, nil)
}

// UnmarkIndexBuild は、作成の途中の印を外す（テーブル定義にインデックスを足すコミットで使う）。
func (tx *WriteTx) UnmarkIndexBuild(branch, tableID, indexID uint64) error {
	return tx.batch.Delete(indexBuildKey(branch, tableID, indexID), nil)
}

// rewrites は、書き直しの途中の印の付いたテーブル ID と、作成の途中の印の付いたインデックス ID を返す。
func (s *Store) rewrites() (map[rewriteID]bool, error) {
	prefix := []byte{tagMeta, tagRewrite}
	iter, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyenc.PrefixEnd(prefix)})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	out := map[rewriteID]bool{}
	for valid := iter.First(); valid; valid = iter.Next() {
		rest := iter.Key()[len(prefix):]
		branch, rest, err := keyenc.DecodeUint64(rest)
		if err != nil {
			return nil, fmt.Errorf("HeimDB: corrupt rewrite key %x: %w", iter.Key(), err)
		}
		table, rest, err := keyenc.DecodeUint64(rest)
		if err != nil {
			return nil, fmt.Errorf("HeimDB: corrupt rewrite key %x: %w", iter.Key(), err)
		}
		var index uint64
		if len(rest) > 0 {
			if index, _, err = keyenc.DecodeUint64(rest); err != nil {
				return nil, fmt.Errorf("HeimDB: corrupt rewrite key %x: %w", iter.Key(), err)
			}
		}
		out[rewriteID{branch, table, index}] = true
	}
	return out, iter.Error()
}

// cleanupRewrites は、前のプロセスが書き直しの途中で終わったときに残ったデータと印を消す（本番の起動時。記録に
// 含めるので、レプリカからも消える）。
func (s *Store) cleanupRewrites() error {
	left, err := s.rewrites()
	if err != nil || len(left) == 0 {
		return err
	}
	return s.Update(func(tx *WriteTx) error {
		for id := range left {
			if id.index != 0 {
				logrus.Infof("HeimDB: dropping the entries of an unfinished CREATE INDEX (branch %d, table %d, index %d)", id.branch, id.table, id.index)
				if err := tx.DropIndexData(id.branch, id.table, id.index); err != nil {
					return err
				}
				if err := tx.UnmarkIndexBuild(id.branch, id.table, id.index); err != nil {
					return err
				}
				continue
			}
			logrus.Infof("HeimDB: dropping the data of an unfinished ALTER TABLE (branch %d, table %d)", id.branch, id.table)
			if err := tx.DropRewrite(id.branch, id.table); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingRewrites は、書き直しの途中の印の数を返す。
func (s *Store) PendingRewrites() (int, error) {
	m, err := s.rewrites()
	return len(m), err
}
