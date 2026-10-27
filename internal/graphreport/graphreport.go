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

// Package graphreport は依存の DAG（リソースと depends_on・ready_condition、今の
// ready かどうか）を 1 つの値にまとめる（#136）。"sakutei graph --json" と、
// デーモンの読み取り専用の口の GET /graph が同じ形で出す。形は docs/read-api.md。
//
// ready は観測から出す（§8.3）。観測していない上流の条件は満たしていない側に
// 数える（unknown ≠ ready）。収束ループの判定と同じ。
package graphreport

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// JSONSchemaVersion は graph の JSON の版。フィールドを足すだけなら上げない。
const JSONSchemaVersion = 1

// Report は依存の DAG 1 つ分。
type Report struct {
	SchemaVersion    int    `json:"schema_version"`
	GeneratedAt      string `json:"generated_at"`
	IntentRevision   string `json:"intent_revision"`
	RenderedRevision string `json:"rendered_revision"`
	// Error はグラフを作れなかったときの理由（終了コード 1）。
	Error string `json:"error,omitempty"`
	// Claim は --claim で絞ったときの scope-selector（§17.1）。
	Claim string `json:"claim,omitempty"`

	// Nodes はリソース（local_id の昇順）。宣言の外の上流（ready_condition だけで
	// 名指しされたもの、§7 の external owner）は external: true で並ぶ。
	Nodes []Node `json:"nodes"`
	// Edges は依存 1 本ずつ（to の昇順、その中は depends_on の順）。
	Edges []Edge `json:"edges"`
	// Waves は Apply の順（Kahn の wave。各 wave の中は local_id の昇順）。
	// 宣言の外の上流は入らない。
	Waves [][]string `json:"waves"`
	// Summary は件数。
	Summary Summary `json:"summary"`
}

// Node はリソース 1 つ。
type Node struct {
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	Scope    map[string]string `json:"scope"`
	LocalID  string            `json:"local_id"`
	// External は宣言の外の上流（ready_condition の相手として観測だけするもの）。
	External bool `json:"external,omitempty"`
	// DependsOn は上流の local_id（宣言の順）。
	DependsOn []string `json:"depends_on"`
	// Observed はこの観測で ready の条件が読めた（Availability ok）こと。
	Observed bool `json:"observed"`
	// Conditions は観測した ready の条件（名前 → 満たしているか）。Observed が
	// false なら出ない。
	Conditions map[string]bool `json:"conditions,omitempty"`
	// Ready は ready_condition 付きの上流がすべて条件を満たしていること（そう
	// でなければ、収束ループはこのリソースとその下流をこの周に進めない）。
	// ready_condition を持たなければ true。
	Ready bool `json:"ready"`
	// WaitingFor は満たしていない最初の条件（"<上流>:<条件>"）。Ready なら空。
	WaitingFor string `json:"waiting_for,omitempty"`
	// Wave は Waves の中の位置（0 から）。宣言の外の上流は -1。
	Wave int `json:"wave"`
}

// Edge は依存 1 本（from が上流、to が下流）。
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// ReadyCondition は from が満たすべき条件（§8.3）。無ければ順序だけの依存。
	ReadyCondition string `json:"ready_condition,omitempty"`
	// Ready は ready_condition を今満たしているか。ready_condition が無い辺では出ない。
	Ready *bool `json:"ready,omitempty"`
}

// Summary は件数。
type Summary struct {
	Nodes    int `json:"nodes"`
	External int `json:"external"`
	Edges    int `json:"edges"`
	Waves    int `json:"waves"`
	// NotReady は Ready が false のリソースの数。
	NotReady int `json:"not_ready"`
}

// New は空の Report を作る（配列は JSON で null にならないよう空にしておく）。
func New(now time.Time) *Report {
	return &Report{
		SchemaVersion: JSONSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Nodes:         []Node{},
		Edges:         []Edge{},
		Waves:         [][]string{},
	}
}

// Build は desired の DAG を r に入れる。ready は観測した ready の条件（local_id →
// 条件名 → 値）で、観測できた（Availability ok の）リソースだけがキーに在る。
func (r *Report) Build(desired []diff.DesiredResource, ready map[string]map[string]bool) {
	byLocal := make(map[string]diff.DesiredResource, len(desired))
	for _, d := range desired {
		byLocal[d.ID.Local] = d
	}
	waveOf := map[string]int{}
	r.Waves = waves(desired)
	for i, w := range r.Waves {
		for _, local := range w {
			waveOf[local] = i
		}
	}

	nodes := map[string]*Node{}
	node := func(id core.ID, external bool) *Node {
		if n, ok := nodes[id.Local]; ok {
			return n
		}
		scope := id.Scope.Labels
		if scope == nil {
			scope = map[string]string{}
		}
		n := &Node{ID: id.String(), Provider: id.Provider, Scope: scope, LocalID: id.Local,
			External: external, DependsOn: []string{}, Ready: true, Wave: -1}
		if conds, ok := ready[id.Local]; ok {
			n.Observed = true
			n.Conditions = map[string]bool{}
			for k, v := range conds {
				n.Conditions[k] = v
			}
		}
		if !external {
			n.Wave = waveOf[id.Local]
		}
		nodes[id.Local] = n
		return n
	}
	sorted := append([]diff.DesiredResource(nil), desired...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID.Local < sorted[j].ID.Local })
	for _, d := range sorted {
		n := node(d.ID, false)
		n.DependsOn = append(n.DependsOn, d.DependsOn...)
		for _, dep := range d.DependsOn {
			e := Edge{From: dep, To: d.ID.Local}
			if cond, ok := d.ReadyConditions[dep]; ok {
				e.ReadyCondition = cond
				ok := ready[dep][cond]
				e.Ready = &ok
				if !ok && n.Ready {
					n.Ready, n.WaitingFor = false, dep+":"+cond
				}
			}
			if _, in := byLocal[dep]; !in {
				// 宣言の外の上流（§7）: 下流の provider で観測する（収束ループと同じ）。
				node(core.ID{Provider: d.ID.Provider, Scope: d.ID.Scope, Local: dep}, true)
			}
			r.Edges = append(r.Edges, e)
		}
	}
	locals := make([]string, 0, len(nodes))
	for l := range nodes {
		locals = append(locals, l)
	}
	sort.Strings(locals)
	for _, l := range locals {
		n := *nodes[l]
		r.Nodes = append(r.Nodes, n)
		if n.External {
			r.Summary.External++
		}
		if !n.Ready {
			r.Summary.NotReady++
		}
	}
	r.Summary.Nodes = len(r.Nodes)
	r.Summary.Edges = len(r.Edges)
	r.Summary.Waves = len(r.Waves)
}

// waves は desired を Kahn 法で wave に分ける（収束ループの Apply の順と同じ）。
// 宣言の外への依存は順序に関係しない。循環は loader が先に拒む。万一残れば、
// 残りを最後の wave にまとめる。
func waves(desired []diff.DesiredResource) [][]string {
	inDeg := map[string]int{}
	down := map[string][]string{}
	for _, d := range desired {
		inDeg[d.ID.Local] += 0
	}
	for _, d := range desired {
		for _, dep := range d.DependsOn {
			if _, in := inDeg[dep]; !in {
				continue
			}
			inDeg[d.ID.Local]++
			down[dep] = append(down[dep], d.ID.Local)
		}
	}
	out := [][]string{}
	done := map[string]bool{}
	for len(done) < len(inDeg) {
		var w []string
		for l, n := range inDeg {
			if !done[l] && n == 0 {
				w = append(w, l)
			}
		}
		if len(w) == 0 {
			for l := range inDeg {
				if !done[l] {
					w = append(w, l)
				}
			}
		}
		sort.Strings(w)
		for _, l := range w {
			done[l] = true
			for _, c := range down[l] {
				inDeg[c]--
			}
		}
		out = append(out, w)
	}
	return out
}

// JSON は字下げつきの JSON にする（末尾の改行は含まない）。
func (r *Report) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
