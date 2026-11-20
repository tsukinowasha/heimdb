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

// Modified by Tsukinowa Inc.: 覚える警告の上限のテスト（docs/engine.md）

package sql

import "testing"

// 覚える警告は 1024 個まで（MySQL の max_error_count の既定）。数は全部を数える。
func TestWarningsCapped(t *testing.T) {
	s := NewBaseSession()
	for i := 0; i < 1100; i++ {
		s.Warn(&Warning{Level: "Warning", Code: 1292, Message: "x"})
	}
	if n := len(s.Warnings()); n != maxStoredWarnings {
		t.Errorf("stored %d warnings, want %d", n, maxStoredWarnings)
	}
	if n := s.WarningCount(); n != 1100 {
		t.Errorf("warning count %d, want 1100", n)
	}
}
