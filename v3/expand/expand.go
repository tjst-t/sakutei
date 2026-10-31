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

// Package expand は intent から Resource・Range を作る純粋な変換の口（設計 §3.8）。
// コアは変換の中身を知らない。段 1 は何もしない Passthrough だけを持つ。
package expand

import (
	"context"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
)

// Input は Expand に渡すもの。外部入力・採番の結果・秘密の世代はそれぞれの段で足す。
type Input struct {
	Intent []intent.Document
}

// Output は Expand の結果。Diagnostics の Reject は読み込みで断る
// （E-B5・E-A7・N-B7）。
type Output struct {
	Resources   []core.Resource
	Ranges      []core.Range
	Policies    []core.Policy
	Cycles      []core.CycleSettings // intent の cycle:。2 つ以上は読み込みで断る
	Diagnostics []core.Diagnostic
}

// Expander は intent を Resource・Range にする。同じ入力に同じ出力を返す
// （N-P5・不変条件 26）。Go の Expand と外のコマンドの Expand（E-A11・N-A14）は
// この口で足す。
type Expander interface {
	Expand(ctx context.Context, in Input) (Output, error)
}

// Passthrough は intent に書いた resources:・ranges:・policies:・cycle: をそのまま返す。
type Passthrough struct{}

// Expand は文書の順に読み、診断も文書の順に返す。
func (Passthrough) Expand(_ context.Context, in Input) (Output, error) {
	var out Output
	for _, doc := range in.Intent {
		d, diags := intent.Decode(doc)
		out.Resources = append(out.Resources, d.Resources...)
		out.Ranges = append(out.Ranges, d.Ranges...)
		out.Policies = append(out.Policies, d.Policies...)
		if d.Cycle != nil {
			out.Cycles = append(out.Cycles, *d.Cycle)
		}
		out.Diagnostics = append(out.Diagnostics, diags...)
	}
	return out, nil
}
