package rowenc

import (
	"bytes"
	"context"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/memory"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func roundTrip(t *testing.T, row sql.Row) {
	t.Helper()
	b, err := EncodeRow(42, row)
	if err != nil {
		t.Fatalf("EncodeRow(%v): %v", row, err)
	}
	ver, got, err := DecodeRow(b)
	if err != nil {
		t.Fatalf("DecodeRow: %v", err)
	}
	if ver != 42 {
		t.Errorf("schema version = %d, want 42", ver)
	}
	if len(got) != len(row) {
		t.Fatalf("got %d columns, want %d", len(got), len(row))
	}
	for i := range row {
		if !valueEqual(row[i], got[i]) {
			t.Errorf("column %d: got %#v (%T), want %#v (%T)", i, got[i], got[i], row[i], row[i])
		}
	}

	// 列を選んで復号する（選ばない列は NULL）。1 列だけと、1 列おきを、どの位置からも試す。
	var masks [][]bool
	for i := range row {
		one := make([]bool, len(row))
		one[i] = true
		alt := make([]bool, len(row))
		for j := i % 2; j < len(row); j += 2 {
			alt[j] = true
		}
		masks = append(masks, one, alt)
	}
	for _, want := range append(masks, make([]bool, len(row)), make([]bool, 1)) {
		ver, part, err := DecodeRowColumns(b, want)
		if err != nil || ver != 42 || len(part) != len(row) {
			t.Fatalf("DecodeRowColumns(%v): version %d, %d columns, %v", want, ver, len(part), err)
		}
		for i := range row {
			wantV := row[i]
			if i >= len(want) || !want[i] {
				wantV = nil
			}
			if !valueEqual(wantV, part[i]) {
				t.Errorf("DecodeRowColumns(%v) column %d: got %#v, want %#v", want, i, part[i], wantV)
			}
		}
	}

	// インデックスのエントリの値（バージョンなし）
	vb, err := EncodeValues(row)
	if err != nil {
		t.Fatalf("EncodeValues: %v", err)
	}
	vals, err := DecodeValues(vb)
	if err != nil || len(vals) != len(row) {
		t.Fatalf("DecodeValues: %d values, %v", len(vals), err)
	}
	for i := range row {
		if !valueEqual(row[i], vals[i]) {
			t.Errorf("DecodeValues value %d: got %#v, want %#v", i, vals[i], row[i])
		}
	}
}

func valueEqual(a, b any) bool {
	if ta, ok := a.(time.Time); ok {
		tb, ok := b.(time.Time)
		// 同じ瞬間であるだけでなく、場所（タイムゾーン）も保たれていること。
		return ok && ta.Equal(tb) && ta.Location().String() == tb.Location().String()
	}
	if da, ok := a.(decimal.Decimal); ok {
		db, ok := b.(decimal.Decimal)
		return ok && da.Equal(db) && da.String() == db.String()
	}
	return reflect.DeepEqual(a, b)
}

func TestRoundTripGoValues(t *testing.T) {
	roundTrip(t, sql.Row{
		nil, true, false,
		int8(-128), int16(-32768), int32(math.MinInt32), int64(math.MinInt64), int64(math.MaxInt64),
		uint8(255), uint16(65535), uint32(math.MaxUint32), uint64(math.MaxUint64),
		float32(1.5), math.Inf(-1), "", "日本語\x00テキスト", []byte{}, []byte{0, 1, 2},
		time.Time{}, time.Date(2026, 10, 2, 8, 30, 0, 123456000, time.UTC),
		decimal.RequireFromString("-123.4500"),
		types.JSONDocument{Val: map[string]any{"a": []any{1.0, "x"}}},
		types.Timespan(-3600000000),
	})
}

func TestRoundTripWordPressValues(t *testing.T) {
	// WordPress コアのテーブルに、go-mysql-server を通して値を入れ、実際に行に入る Go の値を往復させる。
	db := memory.NewDatabase("wordpress")
	pro := memory.NewDBProvider(db)
	engine := sqle.NewDefault(pro)
	ctx := sql.NewContext(context.Background(), sql.WithSession(memory.NewSession(sql.NewBaseSession(), pro)))
	ctx.SetCurrentDatabase("wordpress")
	exec := func(q string) {
		t.Helper()
		_, iter, _, err := engine.Query(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if _, err := sql.RowIterToRows(ctx, iter); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	schemaSQL, err := os.ReadFile("../testdata/wp_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(schemaSQL), "\n") {
		if !strings.HasPrefix(l, "--") {
			lines = append(lines, l)
		}
	}
	for _, q := range strings.Split(strings.Join(lines, "\n"), ";\n") {
		if q = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q), ";")); q != "" {
			exec(q)
		}
	}

	// 既定値（'0000-00-00 00:00:00' など）だけの行と、値を入れた行を各テーブルに入れる。
	names, err := db.GetTableNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		tbl, _, _ := db.GetTableInsensitive(ctx, name)
		var cols, vals []string
		for _, c := range tbl.Schema() {
			if c.AutoIncrement || c.Default != nil {
				continue
			}
			cols = append(cols, "`"+c.Name+"`")
			switch {
			case types.IsInteger(c.Type):
				vals = append(vals, "1")
			case types.IsTime(c.Type):
				vals = append(vals, "'2026-10-02 08:30:00'")
			default:
				vals = append(vals, "'x'")
			}
		}
		exec("INSERT INTO `" + name + "` (" + strings.Join(cols, ",") + ") VALUES (" + strings.Join(vals, ",") + ")")
	}
	exec("UPDATE wp_posts SET post_title = 'タイトル', post_date = '2026-10-02 08:30:00', post_date_gmt = '0000-00-00 00:00:00', menu_order = -5, comment_count = 3")
	exec("UPDATE wp_comments SET comment_karma = -1, comment_approved = 'spam'")

	count := 0
	for _, name := range names {
		tbl, _, _ := db.GetTableInsensitive(ctx, name)
		parts, err := tbl.Partitions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for {
			p, err := parts.Next(ctx)
			if err != nil {
				break
			}
			iter, err := tbl.PartitionRows(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := sql.RowIterToRows(ctx, iter)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				roundTrip(t, r)
				count++
			}
		}
	}
	if count != len(names) {
		t.Fatalf("round-tripped %d rows, want %d", count, len(names))
	}
}

func TestDecodeRowCorrupt(t *testing.T) {
	good, err := EncodeRow(1, sql.Row{"abc", int64(5)})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(good); i++ {
		if _, _, err := DecodeRow(good[:i]); err == nil {
			t.Errorf("DecodeRow of truncated input (%d bytes) should fail", i)
		}
	}
	if _, _, err := DecodeRow(append(good, 0)); err == nil {
		t.Error("DecodeRow with trailing bytes should fail")
	}
}

// 壊れた値と、途中で切れた値は ErrCorrupt（列を読み飛ばす場合も）。
func TestDecodeValuesCorrupt(t *testing.T) {
	good, err := EncodeValues([]any{"abc", int64(5), nil})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{nil, good[:len(good)-1], append(append([]byte(nil), good...), 0), {0xff}, {200}} {
		if _, err := DecodeValues(b); err != ErrCorrupt {
			t.Errorf("DecodeValues(%x) = %v, want ErrCorrupt", b, err)
		}
	}
	row, err := EncodeRow(1, sql.Row{"abc", int64(5), []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(row); n++ {
		if _, _, err := DecodeRowColumns(row[:n], []bool{false, false, false}); err != ErrCorrupt {
			t.Errorf("DecodeRowColumns(%d bytes) = %v, want ErrCorrupt", n, err)
		}
	}
}

func TestEncodeRowRejectsUnknownType(t *testing.T) {
	if _, err := EncodeRow(1, sql.Row{struct{}{}}); err == nil {
		t.Error("expected error for unsupported type")
	}
}

func TestPKOrder(t *testing.T) {
	sch := sql.NewPrimaryKeySchema(sql.Schema{{Name: "a", Type: types.Int64, PrimaryKey: true}, {Name: "b", Type: types.Uint64, PrimaryKey: true}})
	rows := []sql.Row{
		{int64(math.MinInt64), uint64(0)},
		{int64(-1), uint64(math.MaxUint64)},
		{int64(0), uint64(0)},
		{int64(0), uint64(1)},
		{int64(0), uint64(math.MaxInt64)},
		{int64(0), uint64(math.MaxInt64) + 1},
		{int64(0), uint64(math.MaxUint64)},
		{int64(1), uint64(0)},
		{int64(math.MaxInt64), uint64(0)},
	}
	var prev []byte
	for i, r := range rows {
		k, err := EncodePK(nil, sch, r)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && bytes.Compare(prev, k) >= 0 {
			t.Errorf("key for %v does not sort after %v", r, rows[i-1])
		}
		prev = k
	}
	if _, err := EncodePK(nil, sch, sql.Row{"x", uint64(1)}); err == nil {
		t.Error("string primary key should be rejected")
	}
}

// 段階 78: DecodeValuesTo は、値を ords の位置に置き、want が false の位置は読み飛ばす。値の数が違うか壊れていれば
// ErrCorrupt。
func TestDecodeValuesTo(t *testing.T) {
	vals := []any{"abc", int64(5), nil, []byte{1, 2}, 1.5}
	b, err := EncodeValues(vals)
	if err != nil {
		t.Fatal(err)
	}
	ords := []int{4, 0, 2, 5, 1}
	for _, want := range [][]bool{nil, {true, true, true, true, true, true}, {false, true, true, false, false, true}, make([]bool, 6)} {
		row := make(sql.Row, 6)
		if err := DecodeValuesTo(b, row, ords, want); err != nil {
			t.Fatal(err)
		}
		exp := make(sql.Row, 6)
		for i, ord := range ords {
			if want == nil || want[ord] {
				exp[ord] = vals[i]
			}
		}
		if !reflect.DeepEqual(row, exp) {
			t.Errorf("want %v: got %v, want %v", want, row, exp)
		}
	}
	for _, c := range []struct {
		b    []byte
		ords []int
	}{
		{b, ords[:4]}, {b, append(ords, 3)}, {b[:len(b)-1], ords}, {append(append([]byte(nil), b...), 0), ords}, {nil, nil},
	} {
		if err := DecodeValuesTo(c.b, make(sql.Row, 6), c.ords, make([]bool, 6)); err != ErrCorrupt {
			t.Errorf("DecodeValuesTo(%x, %v) = %v, want ErrCorrupt", c.b, c.ords, err)
		}
	}
}
