package backend

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// 暗号化した退避（docs/plan.md の段階 87）。
//
// 鍵は age（https://age-encryption.org/）の X25519 の受取人（公開鍵、age1...）で、起動時に -backup-key 名前=パスで
// 名前とファイルのパスを決める。HeimDB が持つのは公開鍵だけなので、退避を解く秘密鍵は、それを開く側（検証の環境など）
// だけが持つ。ファイルの中身は heimdb_backup のたびに読む（差し替えに起動し直しが要らない）。中身が前に読んだときと
// 変わっていれば警告を出す（書き換えに気づけるように）。
//
// 退避は、store の写し（store.Checkpoint。WAL を含む）を tar にして gzip で固め、受取人に暗号化した 1 つのファイル
// <名前>.tar.gz.age。書き出し中は <名前>.tar.gz.age.partial に書き、書き終えてから名前を変える。暗号化する前の写し
// （.<名前>.ckpt）は、終わったら（失敗しても）消す。解くのは heimdb restore-backup（cmd/heimdb/restore.go）か age の
// コマンド。

const (
	encryptedBackupExt = ".tar.gz.age"
	partialExt         = ".partial"
	checkpointExt      = ".ckpt"
)

// backupKeyNamePattern は、-backup-key の鍵の名前に使える文字。
var backupKeyNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// backupKeys は、鍵の名前ごとの受取人のファイルのパスと、前に読んだ中身。
type backupKeys struct {
	mu    sync.Mutex
	paths map[string]string
	last  map[string]readRecipients
}

// readRecipients は、受取人のファイルを前に読んだときの受取人（正規化した文字列）と時刻。
type readRecipients struct {
	lines []string
	at    time.Time
}

// AddBackupKey は、鍵の名前 name の受取人のファイル path を登録する（-backup-key）。起動時に一度読み、読めなければ
// エラーにする（パスの書き間違いにすぐ気づけるように）。
func (p *Provider) AddBackupKey(name, path string) error {
	if !backupKeyNamePattern.MatchString(name) {
		return fmt.Errorf("invalid backup key name %q (use letters, digits, '_' and '-')", name)
	}
	if p.backupKeys == nil {
		p.backupKeys = &backupKeys{paths: map[string]string{}, last: map[string]readRecipients{}}
	}
	if _, ok := p.backupKeys.paths[name]; ok {
		return fmt.Errorf("backup key %q is given twice", name)
	}
	p.backupKeys.paths[name] = path
	recipients, err := p.backupRecipients(name)
	if err != nil {
		return err
	}
	logrus.Infof("HeimDB: backup key %s: %s", name, recipientList(recipients))
	return nil
}

// backupRecipients は、鍵の名前 name の受取人のファイルを読む。前に読んだときと中身が変わっていれば警告を出す。
func (p *Provider) backupRecipients(name string) ([]age.Recipient, error) {
	if p.backupKeys == nil {
		return nil, fmt.Errorf("HeimDB: unknown backup key %q (start heimdb with -backup-key %s=<file of age recipients>)", name, name)
	}
	k := p.backupKeys
	k.mu.Lock()
	defer k.mu.Unlock()
	path, ok := k.paths[name]
	if !ok {
		return nil, fmt.Errorf("HeimDB: unknown backup key %q (start heimdb with -backup-key %s=<file of age recipients>)", name, name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("HeimDB: backup key %s: %w", name, err)
	}
	recipients, lines, err := parseRecipients(data)
	if err != nil {
		return nil, fmt.Errorf("HeimDB: backup key %s (%s): %w", name, path, err)
	}
	if last, ok := k.last[name]; ok && !slices.Equal(last.lines, lines) {
		logrus.Warnf("HeimDB: recipients of backup key %s changed since %s: %s -> %s", name,
			last.at.UTC().Format(time.RFC3339), strings.Join(last.lines, ", "), strings.Join(lines, ", "))
	}
	k.last[name] = readRecipients{lines: lines, at: time.Now()}
	return recipients, nil
}

// parseRecipients は、受取人のファイルの中身（1 行に 1 つの age1... の受取人。空の行と # で始まる行は読まない）を
// 読む。パスフレーズ（scrypt）や SSH の鍵は受け付けない（サーバに秘密を置かないため、X25519 の公開鍵だけ）。
func parseRecipients(data []byte) ([]age.Recipient, []string, error) {
	var recipients []age.Recipient
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := age.ParseX25519Recipient(line)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: not an age X25519 recipient (age1...): %w", n, err)
		}
		recipients = append(recipients, r)
		lines = append(lines, r.String())
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if len(recipients) == 0 {
		return nil, nil, fmt.Errorf("no recipients")
	}
	return recipients, lines, nil
}

// recipientList は、受取人を , でつないだ文字列（公開鍵なので、短くせずにそのまま出す）。
func recipientList(recipients []age.Recipient) string {
	var s []string
	for _, r := range recipients {
		if x, ok := r.(*age.X25519Recipient); ok {
			s = append(s, x.String())
		}
	}
	return strings.Join(s, ", ")
}

// encryptBackup は、ディレクトリ dir を tar にして gzip で固め、受取人 recipients に暗号化してファイル file に書く。
// 書き出し中は file.partial に書き、終えてから名前を変える。失敗したとき（KILL QUERY を含む）は file.partial を消す。
// 書いたファイルの大きさを返す。
func encryptBackup(ctx *sql.Context, dir, file string, recipients []age.Recipient) (size int64, err error) {
	partial := file + partialExt
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() {
		if f != nil {
			f.Close()
		}
		if err != nil {
			os.Remove(partial)
		}
	}()
	enc, err := age.Encrypt(f, recipients...)
	if err != nil {
		return 0, err
	}
	// Pebble の SSTable はすでに圧縮されているので、速さを優先する。
	gz, err := gzip.NewWriterLevel(enc, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unexpected file %s in the checkpoint", rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		if info.IsDir() {
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, ctxReader{ctx: ctx, r: src})
		return err
	})
	if err != nil {
		return 0, err
	}
	for _, c := range []io.Closer{tw, gz, enc} {
		if err := c.Close(); err != nil {
			return 0, err
		}
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		f = nil
		return 0, err
	}
	f = nil
	if err := os.Rename(partial, file); err != nil {
		return 0, err
	}
	if d, err := os.Open(filepath.Dir(file)); err == nil {
		d.Sync()
		d.Close()
	}
	return info.Size(), nil
}

// ctxReader は、読むたびに文が止められていないかを確かめる（KILL QUERY と停止で、大きな退避を途中で止める）。
type ctxReader struct {
	ctx *sql.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

// CleanBackupDir は、退避の置き場所に残った、暗号化する前の写し（.<名前>.ckpt）と書きかけのファイル（*.partial）を
// 消す（前の起動が退避の途中で止まったとき）。起動時に呼ぶ。
func (p *Provider) CleanBackupDir() {
	if p.BackupDir == "" {
		return
	}
	entries, err := os.ReadDir(p.BackupDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if (strings.HasPrefix(name, ".") && strings.HasSuffix(name, checkpointExt)) || strings.HasSuffix(name, encryptedBackupExt+partialExt) {
			if err := os.RemoveAll(filepath.Join(p.BackupDir, name)); err != nil {
				logrus.Warnf("HeimDB: remove the unfinished backup %s: %v", name, err)
			} else {
				logrus.Warnf("HeimDB: removed the unfinished backup %s", name)
			}
		}
	}
}
