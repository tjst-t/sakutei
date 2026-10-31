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

// Package expandtest は Expand の golden のテスト（設計 §3.8、N-P5）。Expand は
// 同じ入力に同じ出力を返す純粋な変換なので、intent と外部入力を置いたディレクトリの
// 出力を golden のファイルと比べて確かめられる（不変条件 26）。Go の Expand も外の
// コマンドの Expand も、作者が自分のテストで使う。
//
//	testdata/<場合>/
//	├── intent/*.yaml     # intent（expand: で Expand を選ぶ）
//	├── inputs.json       # 任意。外部入力 {"<名前>": {"reachable": true, "data": …}}
//	└── golden.json       # 期待する出力（go test -update で書き直す）
package expandtest

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/intent"
)

var update = flag.Bool("update", false, "golden のファイルを書き直す")

// Golden は dir の intent と外部入力で Expand を 2 回呼び、出力が同じこと（純粋さ）と
// golden.json と同じことを確かめる。gos は Go の Expand、execs は外のコマンドの
// Expand（名前 → パス）。
func Golden(t *testing.T, dir string, gos map[string]expand.Expander, execs map[string]string) {
	t.Helper()
	docs, err := intent.LoadDir(filepath.Join(dir, "intent"))
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]expand.ExternalInput{}
	if data, err := os.ReadFile(filepath.Join(dir, "inputs.json")); err == nil {
		if err := json.Unmarshal(data, &inputs); err != nil {
			t.Fatalf("inputs.json: %v", err)
		}
	}
	run := func() []byte {
		out, err := expand.Run(context.Background(), docs, inputs, gos, execs)
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		b, err := json.MarshalIndent(Render(out), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	got := run()
	if again := run(); string(again) != string(got) {
		t.Fatalf("同じ入力に違う出力を返した（純粋でない、N-P5）:\n%s\n---\n%s", got, again)
	}
	path := filepath.Join(dir, "golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v（go test -update で書く）", err)
	}
	if string(want) != string(got) {
		t.Errorf("%s と違う（意図した変更なら go test -update）:\n--- got\n%s", path, got)
	}
}

// Output は golden に書く形（宣言の順・診断の順を保つ）。
type Output struct {
	Resources   []Resource   `json:"resources,omitempty"`
	Ranges      []any        `json:"ranges,omitempty"`
	Policies    []string     `json:"policies,omitempty"`
	Procedures  []string     `json:"procedures,omitempty"`
	Pools       []string     `json:"pools,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// Resource は宣言 1 件。
type Resource struct {
	ID        string            `json:"id"`
	Ensure    string            `json:"ensure,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Fields    core.Fields       `json:"fields,omitempty"`
	DependsOn []string          `json:"depends_on,omitempty"`
	Apply     any               `json:"apply,omitempty"`
	Reason    string            `json:"reason,omitempty"`
}

// Diagnostic は診断 1 件。
type Diagnostic struct {
	Severity string `json:"severity"`
	Target   string `json:"target,omitempty"`
	Message  string `json:"message"`
}

// Render は Expand の出力を golden に書く形にする。
func Render(out expand.Output) Output {
	var g Output
	for _, r := range out.Resources {
		x := Resource{ID: r.ID.String(), Labels: r.Labels, Fields: r.Fields, Reason: r.Reason}
		if r.Ensure == core.EnsureAbsent {
			x.Ensure = "absent"
		}
		for _, d := range r.DependsOn {
			s := ""
			if d.HasOn() {
				s = d.On.String()
			}
			if d.When != nil {
				if s != "" {
					s += " "
				}
				s += "when " + d.When.String()
			}
			x.DependsOn = append(x.DependsOn, s)
		}
		if t := r.Txn(); t != nil {
			var confirm []string
			for _, c := range t.Confirm {
				confirm = append(confirm, c.Phase.String()+": "+c.When.String())
			}
			a := map[string]any{"timeout": t.Timeout.String()}
			if t.Group != "" {
				a["group"] = t.Group
			}
			if len(confirm) > 0 {
				a["confirm"] = confirm
			}
			x.Apply = a
		}
		g.Resources = append(g.Resources, x)
	}
	for _, rg := range out.Ranges {
		g.Ranges = append(g.Ranges, map[string]any{"provider": rg.Provider, "args": rg.Args, "prune": rg.Prune})
	}
	for _, p := range out.Policies {
		g.Policies = append(g.Policies, p.Name)
	}
	for _, p := range out.Procedures {
		g.Procedures = append(g.Procedures, p.Name)
	}
	for _, p := range out.Pools {
		g.Pools = append(g.Pools, p.Name)
	}
	for _, d := range out.Diagnostics {
		g.Diagnostics = append(g.Diagnostics, Diagnostic{Severity: d.Severity.String(), Target: d.Target, Message: d.Message})
	}
	return g
}

// Cases はディレクトリの下の場合（golden.json か intent/ を持つもの）を名前順に返す。
func Cases(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "intent")); err == nil {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}
