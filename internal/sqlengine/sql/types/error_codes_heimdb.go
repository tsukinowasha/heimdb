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

// HeimDB: MySQL の誤りの番号（docs/engine.md の段階 116）。

package types

import (
	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

func init() {
	sql.ErrorCodeHooks = append(sql.ErrorCodeHooks, func(err error) (int, string, bool) {
		switch {
		case ErrLengthBeyondLimit.Is(err):
			// a strict INSERT or UPDATE of a value past a string column's length (it was 1105)
			return mysql.ERDataTooLong, "22001", true
		case ErrConvertingToEnum.Is(err), sql.ErrInvalidSetValue.Is(err):
			// a strict INSERT or UPDATE of a value that an ENUM or SET does not define (it was 1105)
			return mysql.ERWarnDataTruncated, "01000", true
		}
		return 0, "", false
	})
}
