package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 段階 78: Clone は、すべてのフィールドを写し（Version は写さない）、写しを書き換えても元が変わらない。フィールドを
// 足して Clone で写し忘れると落ちる。
func TestCloneDeep(t *testing.T) {
	var orig Table
	fill(reflect.ValueOf(&orig).Elem(), 1)
	c := orig.Clone()

	want := orig
	want.Version = 0
	if !reflect.DeepEqual(*c, want) {
		t.Fatalf("clone differs:\n%+v\nwant\n%+v", *c, want)
	}
	// JSON で写していたときと同じ値
	b, err := json.Marshal(&orig)
	if err != nil {
		t.Fatal(err)
	}
	var j Table
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*c, j) {
		t.Fatalf("clone differs from the JSON copy:\n%+v\nwant\n%+v", *c, j)
	}

	before, _ := json.Marshal(&orig)
	fill(reflect.ValueOf(c).Elem(), 7)
	if after, _ := json.Marshal(&orig); string(after) != string(before) {
		t.Fatalf("changing the clone changed the original:\n%s\nwas\n%s", after, before)
	}
}

// fill は、v のすべてのフィールドを seed から作った 0 でない値にする。スライスは 2 要素にし、要素とポインタの先も
// 書き換える（すでに値があれば、新しく作らずにその場で書き換える）。
func fill(v reflect.Value, seed int) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fill(v.Field(i), seed+i)
		}
	case reflect.Slice:
		if v.Len() == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		}
		for i := 0; i < v.Len(); i++ {
			fill(v.Index(i), seed+i)
		}
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(v.Elem(), seed)
	case reflect.String:
		v.SetString(v.String() + string(rune('a'+seed%26)))
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + int64(seed))
	case reflect.Uint64:
		v.SetUint(v.Uint() + uint64(seed))
	default:
		panic("fill: unhandled kind " + v.Kind().String())
	}
}
