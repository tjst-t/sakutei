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

// Package expand は intent から Resource・Range などを作る純粋な変換の口（設計 §3.8）。
// コアは変換の中身を知らない。intent をそのまま読む Passthrough のあとに、intent の
// expand: に書いた順に Expand を呼び、出力を足していく（Run）。Expand は Go で
// 組み込むもの（cli.WithExpander）と、外のコマンド（Exec）がある。
package expand

import (
	"context"
	"fmt"
	"sort"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
)

// ExternalInput は外部入力 1 つ（§3.8、E-K3・E-K4・N-K3・N-S4）。Reachable が
// false なら、Expand はそれに依る宣言を作らず、Uncertain の診断で「宣言が欠けて
// いるかもしれない」と言う（空の desired で消さない）。
type ExternalInput struct {
	Data      any    `json:"data,omitempty"`
	Reachable bool   `json:"reachable"`
	Note      string `json:"note,omitempty"` // 届かない・欠けている理由
}

// Input は Expand に渡すもの。同じ Input に同じ出力を返す（N-P5・不変条件 26）。
// 秘密の値・世代と採番の値は渡さない（コアが Expand のあとで差し込む）。
type Input struct {
	Intent []intent.Document
	Inputs map[string]ExternalInput
	// Prior はそれより前（intent をそのまま読んだものと、前の Expand）の出力。
	// 読むだけ（互換の表の照合などが使う、E-A7）。
	Prior Output
}

// Output は Expand の結果。Run は前の出力にこれを足す。Diagnostics の Reject は
// 読み込みで断り（E-B5・E-A7・N-B7）、Warn は知らせ（E-B5）、Uncertain はその
// 宣言を Unknown にする（E-G4・E-K3）。
type Output struct {
	Resources   []core.Resource
	Ranges      []core.Range
	Policies    []core.Policy
	Procedures  []core.Procedure
	Cycles      []core.CycleSettings // intent の cycle:。2 つ以上は読み込みで断る
	Pools       []core.Pool          // 採番のプール（N-B13）
	Allocations []core.Allocation    // 採番のファイル（intent の allocations/、N-B13）
	// DependsOn は前の出力の宣言に足す依存（宣言の ID → 依存）。互換の表が途中の
	// 組み合わせを辺の Condition で止める（E-A7）。宣言に無い ID は断る。
	DependsOn   map[core.ID][]core.Dependency
	Diagnostics []core.Diagnostic
}

// add は o に x を足す。
func (o *Output) add(x Output) {
	o.Resources = append(o.Resources, x.Resources...)
	o.Ranges = append(o.Ranges, x.Ranges...)
	o.Policies = append(o.Policies, x.Policies...)
	o.Procedures = append(o.Procedures, x.Procedures...)
	o.Cycles = append(o.Cycles, x.Cycles...)
	o.Pools = append(o.Pools, x.Pools...)
	o.Allocations = append(o.Allocations, x.Allocations...)
	o.Diagnostics = append(o.Diagnostics, x.Diagnostics...)
	if len(x.DependsOn) == 0 {
		return
	}
	at := make(map[core.ID]int, len(o.Resources))
	for i, r := range o.Resources {
		at[r.ID] = i
	}
	ids := make([]core.ID, 0, len(x.DependsOn))
	for id := range x.DependsOn {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		i, ok := at[id]
		if !ok {
			o.Diagnostics = append(o.Diagnostics, reject(core.Origin{}, id.String(), "Expand が宣言に無い %s に依存を足そうとした", id))
			continue
		}
		r := o.Resources[i]
		r.DependsOn = append(append([]core.Dependency(nil), r.DependsOn...), x.DependsOn[id]...)
		o.Resources[i] = r
	}
}

// Expander は intent と外部入力から宣言を作る（E-A11・N-A14・N-E7・N-B15）。
// 同じ入力に同じ出力を返す（N-P5・不変条件 26）。時刻・乱数・外への問い合わせを
// 使わない。返すのは足す分だけ（Prior を返し直さない。前の宣言を変えるのは依存を
// 足す DependsOn だけ）。
type Expander interface {
	// Keys は intent の最上位で、この Expand が読む項目（network_policy: など）。
	// Passthrough はこれを読まずに断りもしない。
	Keys() []string
	Expand(ctx context.Context, in Input) (Output, error)
}

// Passthrough は intent に書いた最上位の項目をそのまま読む。claimed は Expand の
// モジュールが読む項目。
type Passthrough struct {
	Claimed []string
}

// Expand は文書の順に読み、診断も文書の順に返す。
func (p Passthrough) Expand(_ context.Context, in Input) (Output, error) {
	var out Output
	for _, doc := range in.Intent {
		d, diags := intent.Decode(doc, p.Claimed...)
		out.Resources = append(out.Resources, d.Resources...)
		out.Ranges = append(out.Ranges, d.Ranges...)
		out.Policies = append(out.Policies, d.Policies...)
		out.Procedures = append(out.Procedures, d.Procedures...)
		if d.Cycle != nil {
			out.Cycles = append(out.Cycles, *d.Cycle)
		}
		out.Pools = append(out.Pools, d.Pools...)
		out.Allocations = append(out.Allocations, d.Allocations...)
		out.Diagnostics = append(out.Diagnostics, diags...)
	}
	return out, nil
}

// Keys は Passthrough が読む項目。
func (Passthrough) Keys() []string { return intent.TopKeys }

// Run は Passthrough と、intent の expand: に書いた順の Expand を呼び、出力を
// 足していく（§2.1 の段 2）。gos は Go で組み込んだ Expand、execs はつなぎ方の
// 設定の expanders（名前 → コマンドのパス）。名前の無い Expand・同じ項目を 2 つの Expand が
// 読む宣言は Reject の診断にして、Expand を呼ばずに返す。
func Run(ctx context.Context, docs []intent.Document, inputs map[string]ExternalInput, gos map[string]Expander, execs map[string]string) (Output, error) {
	steps := intent.ScanExpand(docs)
	var chain []Expander
	var claimed []string
	owner := map[string]string{}
	for _, k := range intent.TopKeys {
		owner[k] = "intent"
	}
	var diags []core.Diagnostic
	for _, s := range steps {
		kind, name := s.Module()
		var ex Expander
		var keys []string
		switch kind {
		case "go":
			ex = gos[name]
			if ex == nil {
				diags = append(diags, reject(s.Origin, "expand", "Go の Expand %q が組み込まれていない（組み込まれているもの: %s）", name, names(gos)))
				continue
			}
			keys = ex.Keys()
		case "exec":
			path, ok := execs[name]
			if !ok {
				diags = append(diags, reject(s.Origin, "expand", "外の Expand %q がつなぎ方の設定の expanders に無い", name))
				continue
			}
			ex = &Exec{Name: name, Path: path, Claimed: s.Keys}
			keys = s.Keys
		}
		for _, k := range keys {
			if o, dup := owner[k]; dup {
				diags = append(diags, reject(s.Origin, "expand", "intent の項目 %q を %s と %s の 2 つが読む", k, o, s.Kind))
				continue
			}
			owner[k] = s.Kind
			claimed = append(claimed, k)
		}
		chain = append(chain, ex)
	}
	out, err := Passthrough{Claimed: claimed}.Expand(ctx, Input{Intent: docs, Inputs: inputs})
	if err != nil {
		return Output{}, err
	}
	if len(diags) > 0 {
		out.Diagnostics = append(out.Diagnostics, diags...)
		return out, nil
	}
	for i, ex := range chain {
		got, err := ex.Expand(ctx, Input{Intent: docs, Inputs: inputs, Prior: out})
		if err != nil {
			return Output{}, fmt.Errorf("%s: %w", steps[i].Kind, err)
		}
		out.add(got)
	}
	return out, nil
}

// Sections は文書の最上位の項目 key の値を、文書の順に集める（Expand のモジュールが
// 自分の項目を読む）。
func Sections(docs []intent.Document, key string) []Section {
	var out []Section
	for _, d := range docs {
		if d.Root == nil || d.Root.Map[key] == nil {
			continue
		}
		n := d.Root.Map[key]
		out = append(out, Section{Doc: d.Path, Line: n.Line, Value: n.Value()})
	}
	return out
}

// Section は文書 1 つの中の Expand の項目。
type Section struct {
	Doc   string
	Line  int
	Value any
}

// Origin は Section の場所。
func (s Section) Origin() core.Origin { return core.Origin{File: s.Doc, Line: s.Line} }

func reject(o core.Origin, target, format string, a ...any) core.Diagnostic {
	return core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)}
}

func names(m map[string]Expander) string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "無い"
	}
	return fmt.Sprint(out)
}
