package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
)

// restore-backup（docs/plan.md の段階 87）: 暗号化した退避（heimdb_backup の 2 つ目の引数で鍵を指定したもの）を、
// サーバを起動せずに、データのディレクトリに展開する。
//
//	heimdb restore-backup -in <名前>.tar.gz.age -identity <age の秘密鍵のファイル> [-data <ディレクトリ>]
//
// Docker のイメージでは ENTRYPOINT が -data /data を渡すので、docker run <image> restore-backup -in ... -identity ...
// で /data に展開する。データのディレクトリは空でなければ拒む。いったん中の仮のディレクトリに全部展開し、最後まで
// 復号できたこと（age は途中で切れたファイルを最後の塊で見分ける）と、store のファイル（MANIFEST）があることを確かめてから、
// データのディレクトリに移す。鍵が違う、ファイルが壊れている、途中で切れているときは、何も残さずにエラーで終わる
// （空の store ができて、検証が「差分なし」で通ってしまわないように）。

// restoreBackup は、restore-backup の引数 args を読み、展開する。dataDir は -data の値（restore-backup の -data で
// 上書きできる）。
func restoreBackup(args []string, dataDir string) error {
	fs := flag.NewFlagSet("restore-backup", flag.ContinueOnError)
	in := fs.String("in", "", "encrypted backup written by CALL heimdb_backup('<name>', '<key>') (<name>.tar.gz.age)")
	identity := fs.String("identity", "", "file of age identities (AGE-SECRET-KEY-1...) that can decrypt it")
	data := fs.String("data", dataDir, "empty data directory to restore into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *in == "" || *identity == "" {
		return errors.New("restore-backup needs -in and -identity")
	}
	idf, err := os.Open(*identity)
	if err != nil {
		return err
	}
	ids, err := age.ParseIdentities(idf)
	idf.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", *identity, err)
	}
	src, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer src.Close()

	// データのディレクトリがなければ作る。失敗したら、作ったディレクトリも消す（空のディレクトリが残ると、確かめずに
	// 起動したときに空の store になる）。
	created := false
	if _, err := os.Stat(*data); os.IsNotExist(err) {
		if err := os.MkdirAll(*data, 0o700); err != nil {
			return err
		}
		created = true
	}
	ok := false
	defer func() {
		if !ok && created {
			os.RemoveAll(*data)
		}
	}()
	entries, err := os.ReadDir(*data)
	if err != nil {
		return err
	}
	var others []string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), ".restore-"):
			// 前の restore-backup が途中で止まった（kill など）ときの残り。中身は展開しかけのものなので消す。
			if err := os.RemoveAll(filepath.Join(*data, e.Name())); err != nil {
				return err
			}
		case e.Name() == "lost+found":
			// ファイルシステムの最上位（GCE のディスクなど）にあるもの
		default:
			others = append(others, e.Name())
		}
	}
	if len(others) > 0 {
		if len(others) > 5 {
			others = append(others[:5], "...")
		}
		return fmt.Errorf("the data directory %s is not empty (%s)", *data, strings.Join(others, ", "))
	}
	tmp, err := os.MkdirTemp(*data, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extract(src, ids, tmp); err != nil {
		return fmt.Errorf("%s: %w", *in, err)
	}
	// 同じディレクトリの中なので、名前を変えるだけで移る。途中で失敗したら、移したものを消して空に戻す。
	moved, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(moved, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), "MANIFEST-") }) {
		return fmt.Errorf("%s is not a HeimDB backup (no MANIFEST file)", *in)
	}
	var done []string
	for _, e := range moved {
		dst := filepath.Join(*data, e.Name())
		if err := os.Rename(filepath.Join(tmp, e.Name()), dst); err != nil {
			for _, d := range done {
				os.RemoveAll(d)
			}
			return err
		}
		done = append(done, dst)
	}
	ok = true
	return nil
}

// extract は、src を ids で復号し、gzip の tar を dir に展開する。普通のファイルとディレクトリだけを、dir の中にだけ
// 書く。最後まで読んで、age の認証（途中で切れていないか）を確かめる。
func extract(src io.Reader, ids []age.Identity, dir string) error {
	plain, err := age.Decrypt(src, ids...)
	if err != nil {
		var nomatch *age.NoIdentityMatchError
		if errors.As(err, &nomatch) {
			return errors.New("the identity does not match the key of this backup")
		}
		return err
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unexpected path %q in the backup", hdr.Name)
		}
		path := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected entry %q in the backup", hdr.Name)
		}
	}
	// tar の終わりの後ろまで読み切る（age の最後の塊で、途中で切れていないことが確かめられる）。
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, plain)
	return err
}
