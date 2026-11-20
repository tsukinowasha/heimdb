package backend

import "testing"

func TestKeepLikeEscapes(t *testing.T) {
	cases := []struct {
		in, want string
		ansi     bool
	}{
		{`SELECT 1`, `SELECT 1`, false},
		{`LIKE 'wp\_%'`, `LIKE 'wp\\_%'`, false},
		{`LIKE 'wp\\_%'`, `LIKE 'wp\\_%'`, false}, // 既に \\ なら変えない
		{`LIKE '100\%' OR "a\_b"`, `LIKE '100\\%' OR "a\\_b"`, false},
		{`SELECT 'a\'\_'`, `SELECT 'a\'\\_'`, false},              // \' の後も文字列の中
		{`SELECT 'it''s \_'`, `SELECT 'it''s \\_'`, false},        // '' は ' そのもの
		{"SELECT `c\\_x` FROM t", "SELECT `c\\_x` FROM t", false}, // 識別子は変えない
		{`SELECT "c\_x"`, `SELECT "c\_x"`, true},                  // ANSI_QUOTES では " は識別子
		{"SELECT 1 /* '\\_' */, '\\_'", "SELECT 1 /* '\\_' */, '\\\\_'", false},
		{"SELECT 1 -- '\\_\nFROM t WHERE a = '\\_'", "SELECT 1 -- '\\_\nFROM t WHERE a = '\\\\_'", false},
		{"SELECT 1 # '\\_\n, '\\%'", "SELECT 1 # '\\_\n, '\\\\%'", false},
		{"/*!40101 SET x = '\\_' */", "/*!40101 SET x = '\\\\_' */", false},
		{`SELECT 'unterminated \_`, `SELECT 'unterminated \\_`, false},
	}
	for _, c := range cases {
		got := keepLikeEscapes(c.in, c.ansi)
		if got != c.want {
			t.Errorf("keepLikeEscapes(%q, %v) = %q, want %q", c.in, c.ansi, got, c.want)
		}
		if again := keepLikeEscapes(got, c.ansi); again != got {
			t.Errorf("not idempotent: %q -> %q", got, again)
		}
	}
}
