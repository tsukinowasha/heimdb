package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// sealTar は、entries（名前と中身。中身が nil ならディレクトリ）と、型を決めた hdrs を tar.gz にして id の受取人に
// 暗号化する。
func sealTar(t *testing.T, id *age.X25519Identity, entries [][2]string, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o600, Size: int64(len(e[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e[1]))
	}
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	enc.Close()
	return buf.Bytes()
}

// 退避の中の危ないもの（外へ出るパス、絶対パス、シンボリックリンク、ハードリンク）は展開せずにエラーにし、何も
// 残さない。最後の塊がない（ちょうど塊の境目で切れた）ファイルもエラーにする（docs/plan.md の段階 87）。
func TestRestoreBackupRejects(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "k")
	os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600)
	good := [][2]string{{"MANIFEST-000001", "m"}, {"000002.log", "w"}}
	cases := map[string][]byte{
		"parent":   sealTar(t, id, append(good, [2]string{"../escape", "x"})),
		"absolute": sealTar(t, id, append(good, [2]string{"/abs", "x"})),
		"symlink":  sealTar(t, id, good, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"hardlink": sealTar(t, id, good, &tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "MANIFEST-000001"}),
		"no store": sealTar(t, id, [][2]string{{"x", "y"}}),
	}
	// ちょうど最後の塊（64 KiB の平文）の分を落とす: tar は読み終えられても、age が最後の塊を見つけられない
	random := make([]byte, 3*64*1024) // 圧縮で縮まない中身
	rand.Read(random)
	big := sealTar(t, id, append(good, [2]string{"000003.sst", string(random)}))
	cases["last chunk"] = big[:len(big)-(64*1024+16)]
	for name, data := range cases {
		in := filepath.Join(t.TempDir(), "b.tar.gz.age")
		os.WriteFile(in, data, 0o600)
		dataDir := filepath.Join(t.TempDir(), "data")
		if err := restoreBackup([]string{"-in", in, "-identity", keyFile}, dataDir); err == nil {
			t.Errorf("%s: restored", name)
		}
		if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
			t.Errorf("%s: the data directory was left behind", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dataDir), "escape")); err == nil {
			t.Errorf("%s: wrote outside the data directory", name)
		}
	}

	// 前の restore-backup の残り（.restore-*）と lost+found は、空でないとみなさない
	in := filepath.Join(t.TempDir(), "b.tar.gz.age")
	os.WriteFile(in, sealTar(t, id, good), 0o600)
	dataDir := t.TempDir()
	os.MkdirAll(filepath.Join(dataDir, ".restore-123", "half"), 0o700)
	os.MkdirAll(filepath.Join(dataDir, "lost+found"), 0o700)
	if err := restoreBackup([]string{"-in", in, "-identity", keyFile}, dataDir); err != nil {
		t.Fatalf("restore over leftovers: %v", err)
	}
	entries, _ := os.ReadDir(dataDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "000002.log MANIFEST-000001 lost+found" {
		t.Errorf("data directory = %v", names)
	}
	// 空でないディレクトリは、中身の名前をエラーに出す
	if err := restoreBackup([]string{"-in", in, "-identity", keyFile}, dataDir); err == nil || !strings.Contains(err.Error(), "MANIFEST-000001") {
		t.Errorf("restore into a non-empty directory: %v", err)
	}
}
