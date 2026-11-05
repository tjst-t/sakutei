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

package fake_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// state のファイルの偽の provider は、Apply・Delete でファイルを書き換え、
// Observe・List は毎回ファイルを読み直す（観測を持ち越さない）。
func TestModuleStateFile(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	p, err := fake.Module(map[string]any{
		"state": state,
		"kinds": map[string]any{"record": map[string]any{"fields": map[string]any{"value": "string"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := core.ID{Type: "dns/record", Name: "www-a"}
	b := core.ID{Type: "dns/record", Name: "mail"}
	got, err := p.Observe(ctx, []core.ID{a})
	if err != nil || got[a].Availability != core.Absent {
		t.Fatalf("%+v %v", got, err)
	}
	if err := p.Apply(ctx, core.WriteContext{}, a, core.Fields{"value": "x"}, got[a]); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, core.WriteContext{}, b, core.Fields{"value": "y"}, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(state)
	if !strings.Contains(string(data), `"dns/record/www-a"`) {
		t.Fatalf("%s", data)
	}
	l, err := p.List(ctx, core.Range{Provider: "dns", Args: map[string]string{"prefix": "www"}})
	if err != nil || !l.Complete || len(l.IDs) != 1 || l.IDs[0] != a {
		t.Fatalf("%+v %v", l, err)
	}
	if err := p.Delete(ctx, core.WriteContext{}, a, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	// ファイルを外から書き換えると、次の Observe がそれを読む。
	if err := os.WriteFile(state, []byte(`{"dns/record/mail": {"availability": "converging", "note": "作業中"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = p.Observe(ctx, []core.ID{a, b})
	if err != nil || got[a].Availability != core.Absent || got[b].Availability != core.Converging {
		t.Fatalf("%+v %v", got, err)
	}
	if c := p.(*fake.Provider).Calls(); c != (fake.Calls{Observe: 2, Apply: 2, Delete: 1, List: 1}) {
		t.Fatalf("%+v", c)
	}
	if err := os.WriteFile(state, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Observe(ctx, []core.ID{a}); err == nil {
		t.Fatal("壊れた state で失敗しなかった")
	}
}

func TestModuleRejectsSettings(t *testing.T) {
	for _, s := range []map[string]any{
		{"stat": "x"},
		{"state": 1},
		{"kinds": map[string]any{"r": map[string]any{"fields": map[string]any{"v": "decimal"}}}},
		{"kinds": map[string]any{"r": map[string]any{"fields": map[string]any{"v": map[string]any{"type": "string", "ident": true}}}}},
		{"kinds": map[string]any{"r": map[string]any{"observe": true}}},
	} {
		if _, err := fake.Module(s); err == nil {
			t.Errorf("%v: reject しなかった", s)
		}
	}
}

// Range の scope は現実のラベルがちょうどそれのものを選ぶ（v2 の scope の書き直し）。
// state のファイルのラベルは Apply・Delete のあとも残る。
func TestScopeRanges(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(state, []byte(`{
  "fake/resource/hpc-head": {"availability": "present", "fields": {"name": "hpc-head"}, "labels": {"pool": "hpc"}},
  "fake/resource/node-1": {"availability": "present", "fields": {"name": "node-1"}, "labels": {"pool": "hpc", "slot": "1"}},
  "fake/resource/other": {"availability": "present", "fields": {"name": "other"}}
}`), 0o644)
	p, err := fake.Module(map[string]any{"state": state, "kinds": map[string]any{"resource": map[string]any{"fields": map[string]any{"name": "string"}}}})
	if err != nil {
		t.Fatal(err)
	}
	list := func(scope string) []core.ID {
		l, err := p.List(context.Background(), core.Range{Provider: "fake", Args: map[string]string{"scope": scope}})
		if err != nil {
			t.Fatal(err)
		}
		return l.IDs
	}
	if ids := list("pool=hpc"); len(ids) != 1 || ids[0].Name != "hpc-head" {
		t.Fatalf("%v", ids)
	}
	if ids := list("pool=hpc,slot=1"); len(ids) != 1 || ids[0].Name != "node-1" {
		t.Fatalf("%v", ids)
	}
	head := core.ID{Type: "fake/resource", Name: "hpc-head"}
	if err := p.Apply(context.Background(), core.WriteContext{}, head, core.Fields{"name": "x"}, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if ids := list("pool=hpc"); len(ids) != 1 {
		t.Fatalf("Apply がラベルを落とした: %v", ids)
	}
	v := p.(core.Validator)
	ranges := []core.Range{{Provider: "fake", Args: map[string]string{"scope": "pool=hpc"}}, {Provider: "fake", Args: map[string]string{"scope": "pool=hpc"}}}
	diags := v.Validate([]core.Resource{{ID: head, Labels: map[string]string{"pool": "hpc", "slot": "9"}}}, ranges)
	msgs := ""
	for _, d := range diags {
		msgs += d.Message + "\n"
	}
	if !strings.Contains(msgs, "重なる") || !strings.Contains(msgs, "どの Range にも入らない") {
		t.Fatalf("%s", msgs)
	}
}
