// Copyright 2026 Tsukinowa Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// HeimDB: TEXT の列の値を数にするときの警告（docs/engine.md の段階 118 と 119）。

package expression

import (
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func init() {
	sql.IsTextColumnRef = IsTextColumn
}

// IsTextColumn は、e が TEXT 型（TINYTEXT から LONGTEXT。BLOB は含めない）の列の参照か。MySQL は TEXT の列の値を
// 数にするとき（SUM、AVG、+、-、*、/、%、符号、数との比べ、IN、WHERE v、NOT、AND、IF(v, ...)、ROUND、ABS など）、
// 数として読めない値でも警告 1292 を出さない（VARCHAR の列、定数、LOWER(v) などの式では出す）。
//
// 段階 119: 値をそのまま返す式も同じに扱う。IFNULL と COALESCE は、MySQL では引数によらず（VARCHAR の列でも）
// 出さない（文字列の結果を警告なしで数にする）。IF、CASE、NULLIF は選んだ枝の値を数にするので、結果になりうる
// 枝のうち文字列の型のものがすべて警告を出さない式のときだけ出さない（IF(c, v, 0) + 0 は出さない。IF(c, v, 'x') は
// 'x' の枝を選んだときだけ出すが、ここでは枝を選ぶ前に決めるので、v の枝でも出す。docs/differences.md）。
func IsTextColumn(e sql.Expression) bool {
	switch e := e.(type) {
	case *GetField:
		t := e.Type()
		return types.IsTextBlob(t) && types.IsTextOnly(t)
	case *Case:
		for _, b := range e.Branches {
			if !quietBranch(b.Value) {
				return false
			}
		}
		return e.Else == nil || quietBranch(e.Else)
	case sql.FunctionExpression:
		children := e.Children()
		switch e.FunctionName() {
		case "ifnull", "coalesce":
			return true
		case "if":
			return len(children) == 3 && quietBranch(children[1]) && quietBranch(children[2])
		case "nullif":
			return len(children) == 2 && quietBranch(children[0])
		}
	}
	return false
}

// quietBranch は、IF、CASE、NULLIF の結果になる式 e の値を数にするとき、警告 1292 を出さないか（文字列の型でないか、
// IsTextColumn）。
func quietBranch(e sql.Expression) bool {
	return !types.IsText(e.Type()) || IsTextColumn(e)
}

// quietDouble は、TEXT の列の値 v（文字列）を、警告を出さずに DOUBLE にする（先頭の数の部分。読めなければ 0）。
// 文字列でない値はそのまま返す。
func quietDouble(ctx *sql.Context, v interface{}) interface{} {
	s, ok := v.(string)
	if !ok {
		return v
	}
	d, _, err := types.Float64.Convert(ctx, s)
	if err != nil && (!types.TruncatedString(s, err) || d == nil) {
		return float64(0)
	}
	return d
}

// DoubleArg は、式 e の値 v を、MySQL の val_real と同じく DOUBLE にする（types.Float64.Convert と同じ形で返す。
// HeimDB: 段階 119）。数として読めない文字列は先頭の数の部分（なければ 0）にし、警告 1292 を出す（TEXT の列。
// IsTextColumn では出さない）。前は POW、SQRT、SIN などが 1366 で文全体を誤りにした。文字列でない値で変換が誤った
// ときは、前と同じく誤りを返す。
//
// 日付、日時、TIME は MySQL と同じく数の形（YYYYMMDD、YYYYMMDDhhmmss、hhmmss。types.TemporalNumber）にし、JSON は
// その値（数はそのまま、真偽は 1 と 0、文字列は先頭の数の部分）にする。数でない JSON の値は 0 と警告 3156
// （段階 119 のレビュー。前は TIME が 1105、JSON の '"5x"' が 1366 で文全体を誤りにした）。
func DoubleArg(ctx *sql.Context, e sql.Expression, v interface{}) (interface{}, sql.ConvertInRange, error) {
	if v == nil {
		return nil, sql.InRange, nil
	}
	if n, ok := types.TemporalNumber(ctx, e.Type(), v); ok {
		f, _ := n.Float64()
		return f, sql.InRange, nil
	}
	if js, ok := v.(sql.JSONWrapper); ok {
		return jsonDouble(ctx, e, js), sql.InRange, nil
	}
	f, inRange, err := types.Float64.Convert(ctx, v)
	if err != nil && types.TruncatedString(v, err) && f != nil {
		if !IsTextColumn(e) {
			ctx.Warn(1292, "Truncated incorrect DOUBLE value: '%v'", v)
		}
		return f, sql.InRange, nil
	}
	return f, inRange, err
}

// jsonDouble は、JSON の値 js を DOUBLE にする（MySQL の CAST(j AS DOUBLE) と同じ。DoubleArg）。数でない値は
// 先頭の数の部分（なければ 0）と警告 3156。MySQL の文は「at row N」まで書くが、HeimDB は行の番号を持たないので書かない
// （docs/differences.md）。
func jsonDouble(ctx *sql.Context, e sql.Expression, js sql.JSONWrapper) float64 {
	val, err := js.ToInterface()
	if err == nil {
		switch x := val.(type) {
		case bool:
			if x {
				return 1
			}
			return 0
		case string:
			f, _, cerr := types.Float64.Convert(ctx, x)
			if cerr == nil {
				return f.(float64)
			}
			if d, ok := f.(float64); ok && types.TruncatedString(x, cerr) {
				jsonDoubleWarning(ctx, e)
				return d
			}
		default:
			if f, _, cerr := types.Float64.Convert(ctx, x); cerr == nil {
				if d, ok := f.(float64); ok {
					return d
				}
			}
		}
	}
	jsonDoubleWarning(ctx, e)
	return 0
}

func jsonDoubleWarning(ctx *sql.Context, e sql.Expression) {
	name := e.String()
	if gf, ok := e.(*GetField); ok {
		name = gf.Name()
	}
	ctx.Warn(3156, "Invalid JSON value for CAST to DOUBLE from column %s", name)
}
