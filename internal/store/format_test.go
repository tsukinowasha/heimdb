package store

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
)

// データ形式のバージョン（docs/plan.md の段階 18）: 新しいデータには今のバージョンを書き、今より新しいバージョンのデータは開かない。
func TestFormatVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.getUint64(formatKey()); err != nil || got != FormatVersion {
		t.Fatalf("format = %d (%v), want %d", got, err, FormatVersion)
	}
	// バージョンを記録する前のデータ（バージョンなし）は、今のバージョンとして開き、バージョンを書く。
	if err := s.db.Delete(formatKey(), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir); err != nil {
		t.Fatalf("open data without a format version: %v", err)
	}
	if got, _ := s.getUint64(formatKey()); got != FormatVersion {
		t.Errorf("format after opening old data = %d", got)
	}
	// バージョン 1（0.1.0 から 0.1.5）、バージョン 2（0.1.6 から 0.2.10）、バージョン 3（0.2.11 から 0.2.18）、バージョン 4（0.2.19）、バージョン 5（0.2.20 から 0.2.32）、
	// バージョン 6（0.2.33 から 0.2.38）、バージョン 7（0.2.39 と 0.2.40）のデータは、そのまま読み、バージョンを今のバージョンに書き換える（docs/plan.md の段階 38、59、70、72、94、117、120。書き換えないと、古い HeimDB に戻した
	// ときに今のバージョンで書いたデータを開けてしまう）。
	if FormatVersion != 8 {
		t.Fatalf("FormatVersion = %d; update this test", FormatVersion)
	}
	for _, old := range []uint64{1, 2, 3, 4, 5, 6, 7} {
		if err := s.db.Set(formatKey(), binary.BigEndian.AppendUint64(nil, old), pebble.Sync); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if s, err = Open(dir); err != nil {
			t.Fatalf("open version %d data: %v", old, err)
		}
		if got, _ := s.getUint64(formatKey()); got != FormatVersion {
			t.Errorf("format after opening version %d data = %d", old, got)
		}
	}
	// 新しい HeimDB が書いたデータは開かない。
	if err := s.db.Set(formatKey(), binary.BigEndian.AppendUint64(nil, FormatVersion+1), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer heimdb") {
		t.Fatalf("open newer data: %v", err)
	}
}
