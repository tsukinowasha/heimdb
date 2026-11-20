package schema

import (
	"testing"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/planbuilder"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// WordPress の DDL の数の型は、保存した型の文字列から読み直しても、表示幅が 0（TINYINT(1) と BOOL だけ 1）になる。
// go-mysql-server の IsSigned と IsUnsigned は、表示幅がこれだけであることを前提に、元の型の種類だけで調べる
// （docs/plan.md の段階 52、docs/engine.md）。
func TestNumberTypeDisplayWidths(t *testing.T) {
	for _, tc := range []struct {
		ddl   string
		width int
	}{
		{"bigint(20) unsigned", 0}, {"bigint(20)", 0}, {"int(11)", 0}, {"int(10) unsigned", 0}, {"int unsigned zerofill", 0},
		{"mediumint(9)", 0}, {"smallint(6)", 0}, {"smallint(5) unsigned", 0}, {"tinyint(4)", 0}, {"tinyint(3) unsigned", 0},
		{"tinyint(1)", 1}, {"bool", 1}, {"tinyint(1) unsigned", 0},
	} {
		typ, err := planbuilder.ParseColumnTypeString(tc.ddl)
		if err != nil {
			t.Fatalf("%s: %v", tc.ddl, err)
		}
		stored, err := parseType(typ.String())
		if err != nil {
			t.Fatalf("%s (%s): %v", tc.ddl, typ.String(), err)
		}
		nt, ok := stored.(sql.NumberType)
		if !ok {
			t.Fatalf("%s: %T is not a number type", tc.ddl, stored)
		}
		if got := nt.DisplayWidth(); got != tc.width {
			t.Errorf("%s: display width %d, want %d", tc.ddl, got, tc.width)
		}
	}
}

// ENUM と SET の値の ' と \ は、型の文字列でエスケープし、読み直すと同じ値になる（段階 111、上流の #2995）。読み直して値の
// 変わる型は保存しない（前は ' を含む ENUM の表を保存してから開けなくなり、DROP もできなかった）。
func TestEnumSetTypeStringRoundTrip(t *testing.T) {
	values := []string{"it's", `a\b`, `d\'e`, "''", "plain"}
	for _, typ := range []sql.Type{
		types.MustCreateEnumType(values, sql.Collation_utf8mb4_unicode_520_ci),
		types.MustCreateSetType(values, sql.Collation_utf8mb4_unicode_520_ci),
	} {
		c, err := ColumnFromSQL(&sql.Column{Name: "e", Type: typ, Nullable: true})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		rt, err := c.SQLType()
		if err != nil {
			t.Fatalf("%s: read back: %v", c.Type, err)
		}
		if !sameEnumValues(typ, rt) {
			t.Errorf("%s: read back as %s", typ, rt)
		}
	}
	if sameEnumValues(types.MustCreateEnumType([]string{`a\b`}, sql.Collation_Default), types.MustCreateEnumType([]string{"a\b"}, sql.Collation_Default)) {
		t.Error("sameEnumValues ignored a changed value")
	}
}
