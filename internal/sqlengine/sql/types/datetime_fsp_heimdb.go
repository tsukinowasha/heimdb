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

// HeimDB: 日時の式の結果の小数の桁（docs/engine.md の段階 116 の 4 度目のレビュー）。

package types

import (
	"strings"

	"github.com/dolthub/vitess/go/sqltypes"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// DatetimeFsp は、typ が DATE、DATETIME か TIMESTAMP で、表示する小数の桁が決まっているなら、その桁を返す（DATE と
// DATETIME は 0、DATETIME(3) は 3）。DatetimeMaxPrecision のように値のとおりに表示する型（fsp が false で桁が 0 でない）
// は、桁が決まっていないので false。
func DatetimeFsp(typ sql.Type) (int, bool) {
	dt, ok := typ.(datetimeType)
	if !ok {
		return 0, false
	}
	if dt.baseType == sqltypes.Date || dt.precision == 0 {
		return 0, true
	}
	if !dt.fsp {
		return 0, false
	}
	return dt.precision, true
}

// DatetimeWithFsp は、小数の桁が fsp の DATETIME の型を返す（0 なら Datetime。MySQL の式の結果の DATETIME(fsp)）。
func DatetimeWithFsp(fsp int) sql.DatetimeType {
	if fsp <= 0 {
		return Datetime
	}
	if fsp > 6 {
		fsp = 6
	}
	return MustCreateDatetimeType(sqltypes.Datetime, fsp)
}

// TemporalBranchType は、IF、CASE、COALESCE、IFNULL、GREATEST、LEAST の枝の型 ts（NULL を除く）がすべて DATE、
// DATETIME か TIMESTAMP なら、MySQL と同じ結果の型を返す。すべて同じ型ならその型、すべて DATE なら DATE、ほかは
// 枝の中で最も多い小数の桁の DATETIME（IF(c, d0, d3) は DATETIME(3)）。桁が決まっていない枝があれば
// DatetimeMaxPrecision（値のとおりに表示する）。枝がほかの型なら false。
func TemporalBranchType(ts ...sql.Type) (sql.Type, bool) {
	var first sql.Type
	same, allDate, known := true, true, true
	fsp := 0
	for _, t := range ts {
		if t == nil || t == Null {
			continue
		}
		dt, ok := t.(datetimeType)
		if !ok {
			return nil, false
		}
		if first == nil {
			first = t
		} else if !first.Equals(t) {
			same = false
		}
		if dt.baseType != sqltypes.Date {
			allDate = false
		}
		if p, ok := DatetimeFsp(t); ok {
			fsp = max(fsp, p)
		} else {
			known = false
		}
	}
	switch {
	case first == nil:
		return nil, false
	case same:
		return first, true
	case allDate:
		return Date, true
	case !known:
		return DatetimeMaxPrecision, true
	}
	return DatetimeWithFsp(fsp), true
}

// DatetimeStringFsp は、日時の文字列 s の秒の小数の桁を返す（'2024-01-01 10:00:00.50' は 2。6 桁まで）。MySQL の
// TIMESTAMP('...') は、文字列の小数の桁の DATETIME になる。
func DatetimeStringFsp(s string) int {
	s = strings.TrimRight(s, dateSpaces)
	i := strings.LastIndexByte(s, '.')
	if i < 0 || !strings.Contains(s[:i], ":") {
		return 0
	}
	n := 0
	for _, c := range s[i+1:] {
		if c < '0' || c > '9' {
			break
		}
		n++
	}
	return min(n, 6)
}
