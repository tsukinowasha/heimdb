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

// Modified by Tsukinowa Inc.: 結果の行をバイナリプロトコルで送る文の印を足した（段階 116 の 3 度目のレビュー。docs/engine.md）

package sql

import "context"

type binaryProtocolKey struct{}

// WithBinaryProtocol は、結果の行をバイナリプロトコル（COM_STMT_EXECUTE）で送る文の context を返す（HeimDB）。
// vitess はバイナリの行を、ハンドラが作るテキストの値から作り直す（DOUBLE は strconv.ParseFloat で読む）。
func WithBinaryProtocol(ctx context.Context) context.Context {
	return context.WithValue(ctx, binaryProtocolKey{}, true)
}

// IsBinaryProtocol は、ctx の文が結果の行をバイナリプロトコルで送るか（HeimDB）。
func IsBinaryProtocol(ctx *Context) bool {
	if ctx == nil || ctx.Context == nil {
		return false
	}
	v, _ := ctx.Value(binaryProtocolKey{}).(bool)
	return v
}
