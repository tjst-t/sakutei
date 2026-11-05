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

// Package graph は depends_on と Condition から宣言の順序を作る（設計 §3.5）。
//
// 辺は「下流 → 上流」で、上流は depends_on の id と、when が名指しで参照する対象
// （observe・present・absent）。quantifier（all・any・count）が選んだものは観測するが
// 辺にしない（集合の条件で自分を含んでも循環にしない）。
package graph

import (
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

// Graph は宣言の依存。
type Graph struct {
	res  []core.Resource
	byID map[core.ID]int
	up   map[core.ID][]Edge
	down map[core.ID][]core.ID // 宣言した上流 → 宣言した下流
}

// Edge は From が On に依る（On が上流）。Kind は "depends_on" か "when"。
type Edge struct {
	From string `json:"from"`
	On   string `json:"on"`
	Kind string `json:"kind"`

	from, on core.ID
}

// New は宣言から依存を作る。
func New(res []core.Resource) *Graph {
	g := &Graph{res: res, byID: make(map[core.ID]int, len(res)), up: map[core.ID][]Edge{}, down: map[core.ID][]core.ID{}}
	for i, r := range res {
		g.byID[r.ID] = i
	}
	for _, r := range res {
		seen := map[core.ID]bool{}
		add := func(on core.ID, kind string) {
			if seen[on] {
				return
			}
			seen[on] = true
			g.up[r.ID] = append(g.up[r.ID], Edge{From: r.ID.String(), On: on.String(), Kind: kind, from: r.ID, on: on})
			if _, ok := g.byID[on]; ok {
				g.down[on] = append(g.down[on], r.ID)
			}
		}
		for _, d := range r.DependsOn {
			if d.HasOn() {
				add(d.On, "depends_on")
			}
		}
		for _, d := range r.DependsOn {
			if d.When != nil {
				for _, id := range d.When.Direct() {
					add(id, "when")
				}
			}
		}
	}
	return g
}

// Resource は宣言 id（無ければ nil）。
func (g *Graph) Resource(id core.ID) *core.Resource {
	i, ok := g.byID[id]
	if !ok {
		return nil
	}
	return &g.res[i]
}

// Select は quantifier の選び方に合う宣言の ID を名前順で返す。
func (g *Graph) Select(s core.Selection) []core.ID {
	var out []core.ID
	for _, r := range g.res {
		if r.ID.Type != s.Type {
			continue
		}
		hit := true
		for k, v := range s.Labels {
			if got, ok := r.Labels[k]; !ok || got != v {
				hit = false
				break
			}
		}
		if hit {
			out = append(out, r.ID)
		}
	}
	sortIDs(out)
	return out
}

// Upstream は id の直接の上流（宣言に無いものを含む）。
func (g *Graph) Upstream(id core.ID) []core.ID {
	out := make([]core.ID, 0, len(g.up[id]))
	for _, e := range g.up[id] {
		out = append(out, e.on)
	}
	return out
}

// Dependents は id に直接依る宣言。
func (g *Graph) Dependents(id core.ID) []core.ID { return g.down[id] }

// Downstream は id に（間接にも）依る宣言を名前順で返す（N-J11・E-H5：変わる
// Resource の下流を見せる）。
func (g *Graph) Downstream(id core.ID) []core.ID {
	seen := map[core.ID]bool{id: true}
	var out []core.ID
	stack := append([]core.ID(nil), g.down[id]...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
		stack = append(stack, g.down[n]...)
	}
	sortIDs(out)
	return out
}

// Refs は id の待ちを判定するために観測する対象（上流と、when の参照の全部）。
func (g *Graph) Refs(id core.ID) []core.ID {
	r := g.Resource(id)
	if r == nil {
		return nil
	}
	var out []core.ID
	for _, d := range r.DependsOn {
		if d.HasOn() {
			out = append(out, d.On)
		}
		if d.When != nil {
			out = append(out, d.When.Refs(g.Select)...)
		}
	}
	return out
}

// Edges は全部の辺（宣言の順）。
func (g *Graph) Edges() []Edge {
	out := []Edge{}
	for _, r := range g.res {
		out = append(out, g.up[r.ID]...)
	}
	return out
}

// Cycles は宣言どうしの循環を 1 つずつ返す（E-J7・E-M3：読み込み時にエラーにする（reject））。
// 宣言に無い上流は辺にしない。
func (g *Graph) Cycles() [][]core.ID {
	const (
		white = iota
		grey
		black
	)
	color := map[core.ID]int{}
	var out [][]core.ID
	var stack []core.ID
	var visit func(id core.ID)
	visit = func(id core.ID) {
		color[id] = grey
		stack = append(stack, id)
		for _, e := range g.up[id] {
			if _, declared := g.byID[e.on]; !declared {
				continue
			}
			switch color[e.on] {
			case white:
				visit(e.on)
			case grey:
				var path []core.ID
				for i := len(stack) - 1; i >= 0; i-- {
					path = append([]core.ID{stack[i]}, path...)
					if stack[i] == e.on {
						break
					}
				}
				out = append(out, append(path, e.on))
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	for _, r := range g.res {
		if color[r.ID] == white {
			visit(r.ID)
		}
	}
	return out
}

// PathString は循環の道を "a → b → a" にする。
func PathString(path []core.ID) string {
	parts := make([]string, len(path))
	for i, id := range path {
		parts[i] = id.String()
	}
	return strings.Join(parts, " → ")
}

func sortIDs(ids []core.ID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
}
