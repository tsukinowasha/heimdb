package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 文が止められると（KILL QUERY、停止）、暗号化した退避の書きかけのファイルを残さない。起動時には、前の起動が残した
// 書きかけのファイルと暗号化する前の写しを消す（docs/plan.md の段階 87）。
func TestEncryptBackupCanceledAndCleanup(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "000001.sst"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file := filepath.Join(out, "b.tar.gz.age")
	if _, err := encryptBackup(sql.NewContext(ctx), src, file, []age.Recipient{id.Recipient()}); err == nil {
		t.Fatal("a canceled backup succeeded")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}

	for _, name := range []string{".b.ckpt", "b.tar.gz.age.partial", "keep", "keep.tar.gz.age"} {
		if err := os.MkdirAll(filepath.Join(out, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	(&Provider{BackupDir: out}).CleanBackupDir()
	entries, _ := os.ReadDir(out)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || names[0] != "keep" || names[1] != "keep.tar.gz.age" {
		t.Errorf("after cleanup: %v", names)
	}
}
