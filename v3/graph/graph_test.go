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

package graph_test

import (
	"reflect"
	"testing"

	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/graph"
)

func id(n string) core.ID { return core.ID{Type: "p/r", Name: n} }

func TestGraph(t *testing.T) {
	ext := core.ID{Type: "m/pool", Name: "x"}
	res := []core.Resource{
		{ID: id("a"), Labels: map[string]string{"role": "node"}},
		{ID: id("b"), Labels: map[string]string{"role": "node"}, DependsOn: []core.Dependency{{On: id("a")}}},
		{ID: id("c"), DependsOn: []core.Dependency{{On: id("b"), When: condition.MustParse(`observe(p/r/b).v == 1 and present(m/pool/x)`)}}},
		{ID: id("d"), DependsOn: []core.Dependency{{When: condition.MustParse(`count(p/r[role=node], it.v == 1) > 1`)}}},
	}
	g := graph.New(res)
	if got := g.Downstream(id("a")); !reflect.DeepEqual(got, []core.ID{id("b"), id("c")}) {
		t.Errorf("Downstream(a) = %v", got)
	}
	if got := g.Upstream(id("c")); !reflect.DeepEqual(got, []core.ID{id("b"), ext}) {
		t.Errorf("Upstream(c) = %v", got)
	}
	// 量化の選んだものは観測するが辺にしない。
	if got := g.Upstream(id("d")); len(got) != 0 {
		t.Errorf("Upstream(d) = %v", got)
	}
	if got := g.Refs(id("d")); !reflect.DeepEqual(got, []core.ID{id("a"), id("b")}) {
		t.Errorf("Refs(d) = %v", got)
	}
	if got := g.Select(core.Selection{Type: "p/r", Labels: map[string]string{"role": "node"}}); !reflect.DeepEqual(got, []core.ID{id("a"), id("b")}) {
		t.Errorf("Select = %v", got)
	}
	if c := g.Cycles(); len(c) != 0 {
		t.Errorf("Cycles = %v", c)
	}
	edges := g.Edges()
	if len(edges) != 3 || edges[2].Kind != "when" || edges[2].On != "m/pool/x" {
		t.Errorf("Edges = %+v", edges)
	}

	res[0].DependsOn = []core.Dependency{{When: condition.MustParse(`present(p/r/c)`)}}
	if c := graph.New(res).Cycles(); len(c) != 1 || graph.PathString(c[0]) != "p/r/a → p/r/c → p/r/b → p/r/a" {
		t.Errorf("Cycles = %v", c)
	}
}
