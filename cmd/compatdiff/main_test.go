package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 違い、片方にしかないキーは 1、知っている違いだけなら 0。知っている違いが違わなくなっても 0（知らせるだけ）。
// 値は空白の違いを無視して比べる。
func TestReport(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	base := map[string]json.RawMessage{"a": raw(`[1, "x"]`), "b": raw(`"y"`), "c": raw(`1`)}
	for _, c := range []struct {
		name   string
		target map[string]json.RawMessage
		known  map[string]string
		want   int
	}{
		{"same", map[string]json.RawMessage{"a": raw(`[1,"x"]`), "b": raw(`"y"`), "c": raw(`1`)}, nil, 0},
		{"different", map[string]json.RawMessage{"a": raw(`[1,"z"]`), "b": raw(`"y"`), "c": raw(`1`)}, nil, 1},
		{"known", map[string]json.RawMessage{"a": raw(`[1,"z"]`), "b": raw(`"y"`), "c": raw(`1`)}, map[string]string{"a": "why"}, 0},
		{"known but same", map[string]json.RawMessage{"a": raw(`[1,"x"]`), "b": raw(`"y"`), "c": raw(`1`)}, map[string]string{"a": "why"}, 0},
		{"missing", map[string]json.RawMessage{"a": raw(`[1,"x"]`), "b": raw(`"y"`)}, map[string]string{"c": "why"}, 1},
		{"extra", map[string]json.RawMessage{"a": raw(`[1,"x"]`), "b": raw(`"y"`), "c": raw(`1`), "d": raw(`2`)}, nil, 1},
	} {
		if got := report(base, c.target, c.known); got != c.want {
			t.Errorf("%s: report = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestLoadKnown(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok.txt")
	os.WriteFile(ok, []byte("# comment\n\nA::b\treason one\n  C::d\t reason two \n"), 0o644)
	known, err := loadKnown(ok)
	if err != nil || len(known) != 2 || known["A::b"] != "reason one" || known["C::d"] != "reason two" {
		t.Errorf("loadKnown = %v, %v", known, err)
	}
	bad := filepath.Join(dir, "bad.txt")
	os.WriteFile(bad, []byte("A::b without a tab\n"), 0o644)
	if _, err := loadKnown(bad); err == nil {
		t.Error("a line without a reason was accepted")
	}
	if known, err := loadKnown(filepath.Join(dir, "none.txt")); err != nil || len(known) != 0 {
		t.Errorf("missing file: %v, %v", known, err)
	}
}
