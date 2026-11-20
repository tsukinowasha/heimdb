package store

// Users は、保存したユーザーと権限（go-mysql-server の mysql_db の直列化）を返す。まだなければ nil
// （docs/plan.md の段階 14）。
func (s *Store) Users() ([]byte, error) {
	v, _, err := s.get(usersKey())
	return v, err
}

// SetUsers は、ユーザーと権限を保存する。バージョンを持たない（ブランチにもタイムトラベルにも属さない）。レプリカに送るため、
// コミット（Update）を通して書く。
func (s *Store) SetUsers(data []byte) error {
	return s.Update(func(tx *WriteTx) error {
		return tx.batch.Set(usersKey(), data, nil)
	})
}
