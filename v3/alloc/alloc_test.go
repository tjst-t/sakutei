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

package alloc_test

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/alloc"
	"github.com/tjst-t/sakutei/v3/core"
)

func req(name string) core.AllocRequest {
	return core.AllocRequest{Resource: core.ID{Type: "pve/vm", Name: name}, Field: "vmid", Pool: "vmid", Block: "staff"}
}

// TestResolve は block の中の空きの先頭を選び、reuse: false では last 以下を払わず、
// ファイルが写しより戻っていれば新しい値を出さず、ファイルが写しより進んでいる
// （人が書き足した）のは受け入れることを確かめる（不変条件 11、N-B13）。
func TestResolve(t *testing.T) {
	pool := core.Pool{Name: "vmid", Lo: 100, Hi: 199, Blocks: []core.Block{{Name: "staff", Lo: 100, Hi: 102}}, Reuse: false}
	file := core.Allocation{Pool: "vmid", Values: map[string]int64{"pve/vm/a#vmid": 100}, Last: map[string]int64{"staff": 101}}
	p, diags := alloc.Resolve(alloc.State{Pools: []core.Pool{pool}, Files: []core.Allocation{file}}, []core.AllocRequest{req("a"), req("b"), req("c")}, nil)
	if len(diags) > 0 {
		t.Fatal(diags)
	}
	if p.Values["pve/vm/a#vmid"] != 100 || p.Values["pve/vm/b#vmid"] != 102 {
		t.Fatalf("値 = %v（101 は last 以下なので払わない）", p.Values)
	}
	if !strings.Contains(p.Blocked["pve/vm/c#vmid"], "空きが無い") {
		t.Fatalf("尽きた block を理由つきで止めていない: %v", p.Blocked)
	}
	if w := p.Writes["vmid"]; !w.WriteF || w.Next.Last["staff"] != 102 {
		t.Fatalf("書く内容 = %+v", w)
	}

	// 写しが先に進んでいる（ファイルを巻き戻した）。
	cp := core.Allocation{Pool: "vmid", Values: map[string]int64{"pve/vm/a#vmid": 100, "pve/vm/b#vmid": 102}, Last: map[string]int64{"staff": 102}}
	p, _ = alloc.Resolve(alloc.State{Pools: []core.Pool{pool}, Files: []core.Allocation{file}, Copies: map[string]*core.Allocation{"vmid": &cp}},
		[]core.AllocRequest{req("a"), req("c")}, nil)
	if p.Values["pve/vm/a#vmid"] != 100 || !strings.Contains(p.Blocked["pve/vm/c#vmid"], "写しより戻っている") || len(p.Writes) != 0 {
		t.Fatalf("巻き戻し: %+v", p)
	}

	// 人がファイルに書き足した（写しを含む）：受け入れて写しを書き直す。
	grown := core.Allocation{Pool: "vmid", Values: map[string]int64{"pve/vm/a#vmid": 100, "pve/vm/z#vmid": 150}, Last: map[string]int64{"staff": 101}}
	small := core.Allocation{Pool: "vmid", Values: map[string]int64{"pve/vm/a#vmid": 100}, Last: map[string]int64{"staff": 101}}
	p, _ = alloc.Resolve(alloc.State{Pools: []core.Pool{pool}, Files: []core.Allocation{grown}, Copies: map[string]*core.Allocation{"vmid": &small}}, nil, nil)
	if w, ok := p.Writes["vmid"]; !ok || !w.Copy || w.WriteF {
		t.Fatalf("書き足しを受け入れていない: %+v", p.Writes)
	}

	// 同じ値を 2 つに払っているファイル・範囲の外は reject する。
	bad := core.Allocation{Pool: "vmid", Values: map[string]int64{"pve/vm/a#vmid": 100, "pve/vm/b#vmid": 100, "pve/vm/c#vmid": 500}}
	if _, diags := alloc.Resolve(alloc.State{Pools: []core.Pool{pool}, Files: []core.Allocation{bad}}, nil, nil); len(diags) != 2 {
		t.Fatalf("diags = %v", diags)
	}
}

// TestFormatRoundTrip は書いた採番のファイルを intent として読み戻せる。
func TestFormatRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := core.Allocation{Pool: "vmid", Values: map[string]int64{"pve/vm/app#01#vmid": 100}, Last: map[string]int64{"": 100, "staff": 7}}
	if err := writeFile(dir+"/allocations/vmid.yaml", alloc.Format(a)); err != nil {
		t.Fatal(err)
	}
	got, err := alloc.ReadFile(dir, "allocations/vmid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !alloc.Same(got, a) {
		t.Fatalf("読み戻し = %+v", got)
	}
}
