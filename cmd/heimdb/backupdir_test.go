package main

import (
	"os"
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
		{filepath.Join(data, "CURRENT"), false},
		{filepath.Join(data, "MANIFEST-000001"), false},
		{filepath.Join(data, "OPTIONS-000001", "x"), false},
	} {
		if err := checkBackupDir(data, filepath.Clean(c.dir)); (err == nil) != c.ok {
			t.Errorf("checkBackupDir(%q) = %v, want ok %v", c.dir, err, c.ok)
		}
	}
	// シンボリックリンクを通しても、-data そのものと、その中の名前を見分ける。
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}
	if err := checkBackupDir(link, data); err == nil {
		t.Error("-backup-dir equal to -data through a symlink was accepted")
	}
	if err := checkBackupDir(data, filepath.Join(link, "000001.sst")); err == nil {
		t.Error("a store file name inside -data through a symlink was accepted")
	}
	if err := checkBackupDir(link, filepath.Join(data, "backups")); err != nil {
		t.Errorf("backups inside -data through a symlink: %v", err)
	}
}
