package backend

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// バックアップ（docs/plan.md の段階 16）。
//
// CALL heimdb_backup([名前]) で、起動時の -backup-dir の下に、整合したスナップショット（store.Checkpoint）を作る。
// そのディレクトリは、そのまま HeimDB のデータディレクトリとして開ける。SQL から任意のパスに書けないよう、置き場所は
// 起動時に決めた場所に限り、名前だけを受け付ける。

var backupSchema = sql.Schema{
	{Name: "path", Type: types.LongText},
	{Name: "backed_up_at", Type: types.LongText},
	{Name: "bytes", Type: types.Int64},
}

// backupNamePattern は、バックアップの名前に使える文字（パスの区切りや .. を含まない）。
var backupNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// backupMu は、heimdb_backup を 1 つずつにする（同じ名前の確かめと作成の間に、別の退避が入らないように）。
var backupMu sync.Mutex

// backup は、名前 name の退避を取る。key が空なら、そのまま開けるディレクトリにする。key があれば、その名前の鍵
// （-backup-key。backupkey.go）の受取人に暗号化した 1 つのファイル <name>.tar.gz.age にする（docs/plan.md の段階 87）。
func (p *Provider) backup(ctx *sql.Context, name, key string) (sql.RowIter, error) {
	if p.BackupDir == "" {
		return nil, fmt.Errorf("HeimDB: backups are disabled (start heimdb with -backup-dir)")
	}
	if name == "" {
		name = time.Now().UTC().Format("20060102T150405.000000Z")
	}
	if !backupNamePattern.MatchString(name) || strings.HasSuffix(name, ".partial") {
		return nil, fmt.Errorf("HeimDB: invalid backup name %q (use letters, digits, '.', '_' and '-')", name)
	}
	var recipients []age.Recipient
	if key != "" {
		var err error
		if recipients, err = p.backupRecipients(key); err != nil {
			return nil, err
		}
	}
	backupMu.Lock()
	defer backupMu.Unlock()
	// バックアップには m/users のパスワードのハッシュが入るので、置き場所は HeimDB のユーザーだけが読めるようにする。
	if err := os.MkdirAll(p.BackupDir, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(p.BackupDir, name)
	file := dir + encryptedBackupExt
	for _, path := range []string{dir, file} {
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("HeimDB: backup %q already exists", name)
		}
	}
	ckpt := dir
	if key != "" {
		// 暗号化する前の写しは、退避と見分けられる名前（. で始まる）に作り、終わったら（失敗しても）消す。
		ckpt = filepath.Join(p.BackupDir, "."+name+checkpointExt)
		if err := os.RemoveAll(ckpt); err != nil {
			return nil, err
		}
		defer os.RemoveAll(ckpt)
	}
	start := time.Now()
	ts, err := p.st.Checkpoint(ckpt)
	if err != nil {
		return nil, fmt.Errorf("HeimDB: backup: %w", err)
	}
	if d := time.Since(start); d > time.Second {
		// 取る間はすべての書き込みが止まる。別のファイルシステムだとハードリンクできず、全ファイルを写す。
		logrus.Warnf("heimdb_backup took %s and paused all writes meanwhile; put -backup-dir on the same filesystem as -data", d.Round(time.Millisecond))
	}
	if key == "" {
		size, err := dirSize(dir)
		if err != nil {
			return nil, err
		}
		return &rowIter{rows: []sql.Row{{dir, formatRestoreTime(ts), size}}}, nil
	}
	size, err := encryptBackup(ctx, ckpt, file, recipients)
	if err != nil {
		return nil, fmt.Errorf("HeimDB: backup: %w", err)
	}
	logrus.Infof("HeimDB: heimdb_backup %s: encrypted with key %s to %s", name, key, recipientList(recipients))
	return &rowIter{rows: []sql.Row{{file, formatRestoreTime(ts), size}}}, nil
}

// dirSize は、ディレクトリの中のファイルの大きさの合計を返す（ハードリンクで元のデータと共有しているものも数える）。
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
