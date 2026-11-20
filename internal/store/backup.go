package store

import "github.com/cockroachdb/pebble/v2"

// Checkpoint は、データベースの整合したスナップショットを dir に作り、それに入った最後のコミットの時刻を返す
// （docs/plan.md の段階 16）。dir はまだないこと。作ったディレクトリは、そのまま Open で開ける。
//
// コミットはすべて Update の書き込みのロックの中で 1 つのバッチとして書くので、ロックの中で取れば、その時刻までの
// コミットがすべて入り、それより後は 1 つも入らない。同じファイルシステムならハードリンクで作るので速い。
//
// バックアップはレプリカの始まりにもなる（段階 24）。最後のコミットの記録がなければ（記録を残す前のデータで起動して、
// まだ書いていない）、先に何も変えないコミットをして、その記録を作る（レプリカの位置は本番の記録にあること）。
func (s *Store) Checkpoint(dir string) (uint64, error) {
	if s.Logging() && !s.replica {
		if _, ok, err := s.get(logKey(s.LastCommitted())); err != nil {
			return 0, err
		} else if !ok {
			if err := s.Update(func(*WriteTx) error { return nil }); err != nil {
				return 0, err
			}
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.db.Checkpoint(dir, pebble.WithFlushedWAL()); err != nil {
		return 0, err
	}
	return s.lastTS, nil
}
