// Copyright 2026 tjst-t
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

package template_test

import (
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/expand/expandtest"
	"github.com/tjst-t/sakutei/v3/providers/expanders/template"
)

// TestGolden はテンプレートの展開を golden で確かめる（N-P5・E-A11・N-A14）。inline は
// intent の items と vars（値の型を保つ差し込み・採番の要求・無い field のエラー）、
// inventory は外部入力からの展開、unreachable は届かない入力で宣言を作らず
// Uncertain を出す（E-K3）。
func TestGolden(t *testing.T) {
	for _, dir := range expandtest.Cases(t, "testdata") {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			expandtest.Golden(t, dir, map[string]expand.Expander{"template": template.Expander{}}, nil)
		})
	}
}
