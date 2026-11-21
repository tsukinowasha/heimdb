package main

import (
	"path/filepath"
	"testing"
)

// -backup-dir は -data の外か、-data の中の Pebble のファイルの名前になりえないディレクトリ（docs/stages/131.md）。
func TestCheckBackupDir(t *testing.T) {
	data := t.TempDir()
	for _, c := range []struct {
		dir string
		ok  bool
	}{
		{filepath.Join(filepath.Dir(data), "backups"), true},
		{filepath.Join(data, "backups"), true},
		{filepath.Join(data, "backups", "daily"), true},
		{filepath.Join(data, "..backups"), false},
		{data, false},
		{data + string(filepath.Separator), false},
		{filepath.Join(data, "000001.sst"), false},
		{filepath.Join(data, "backups.d"), false},
		{filepath.Join(data, "LOCK"), false},
		{filepath.Join(data, "MANIFEST-000001"), false},
		{filepath.Join(data, "OPTIONS-000001", "x"), false},
	} {
		if err := checkBackupDir(data, filepath.Clean(c.dir)); (err == nil) != c.ok {
			t.Errorf("checkBackupDir(%q) = %v, want ok %v", c.dir, err, c.ok)
		}
	}
}
