package rowenc

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/planbuilder"
)

// 主キーの列の型の変更のうち、キーのバイト列の比べ方が変わらないものだけ、キーの形が同じ（docs/plan.md の段階 70）。
func TestPKKeyKind(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"int", "bigint unsigned", true},
		{"varchar(20) COLLATE utf8mb4_unicode_520_ci", "varchar(30) COLLATE utf8mb4_unicode_520_ci", true},
		{"varchar(20) COLLATE utf8mb4_unicode_520_ci", "varchar(20) COLLATE utf8mb4_bin", false},
		{"varchar(20) COLLATE utf8mb4_unicode_520_ci", "char(20) COLLATE utf8mb4_unicode_520_ci", false},
		{"varchar(20) COLLATE utf8mb4_unicode_520_ci", "varbinary(20)", false},
		{"varbinary(20)", "varbinary(255)", true},
		{"binary(16)", "binary(20)", false},
		{"binary(16)", "varbinary(16)", false},
		{"datetime", "datetime(6)", true},
		{"date", "datetime", false},
		{"datetime", "timestamp", false},
		{"enum('a','b')", "enum('a','b')", true},
		{"enum('a','b')", "enum('b','a')", false},
		{"enum('a','b')", "enum('a','b','c')", false},
		{"int", "enum('a')", false},
	} {
		ta, err := planbuilder.ParseColumnTypeString(c.a)
		if err != nil {
			t.Fatal(err)
		}
		tb, err := planbuilder.ParseColumnTypeString(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := PKKeyKind(ta) == PKKeyKind(tb); got != c.same {
			t.Errorf("%s (%s) vs %s (%s): same = %v, want %v", c.a, PKKeyKind(ta), c.b, PKKeyKind(tb), got, c.same)
		}
	}
}
