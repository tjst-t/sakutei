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

package plan_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// TestCompareMatch は provider が決めた比べ方（FieldSpec.Match）で比べる
// （E-I7・E-D1）。観測に無い項目も Match が等しいと言えば差分にしない（作る
// ときだけ使う項目）。
func TestCompareMatch(t *testing.T) {
	ks := core.KindSchema{Fields: map[string]core.FieldSpec{
		"memory":   {Type: core.String, Match: func(d, o any) bool { return strings.TrimSuffix(d.(string), "M") == o }},
		"template": {Type: core.String, Match: func(any, any) bool { return true }},
	}}
	r := core.Resource{ID: core.ID{Type: "pve/vm", Name: "a"}, Fields: core.Fields{"memory": "2048M", "template": "debian"}}
	if c := plan.Compare(r, ks, present(core.Fields{"memory": "2048"})); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	c := plan.Compare(r, ks, present(core.Fields{"memory": "4096"}))
	if c.Op != plan.Update || len(c.Fields) != 1 || c.Fields[0].Name != "memory" {
		t.Fatalf("%+v", c)
	}
	if c := plan.Compare(r, ks, core.Observation{Availability: core.Absent}); c.Op != plan.Create || len(c.Fields) != 2 {
		t.Fatalf("作るときは template も渡す: %+v", c)
	}
}

type classified struct {
	*fake.Provider
	hints []core.Hint
}

func (c classified) Classify(core.ID, core.Observation, core.Fields) []core.Hint { return c.hints }

// TestDecideClassifier は provider が状況で言った壊す変更を予算の壊す変更に数え、
// 承認が要る変更を承認が揃うまで当てない（§3.3、E-C2・E-B6・不変条件 9・21）。
func TestDecideClassifier(t *testing.T) {
	ks := core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{"vm": {Fields: map[string]core.FieldSpec{"disk": {Type: core.String}}}}}
	vm := core.ID{Type: "pve/vm", Name: "a"}
	newP := func(h ...core.Hint) classified {
		p := fake.New(ks)
		p.Set(vm, core.Observation{Availability: core.Present, Fields: core.Fields{"disk": "a"}})
		return classified{Provider: p, hints: h}
	}
	res := []core.Resource{{ID: vm, Fields: core.Fields{"disk": "b"}}}
	decide := func(p classified, pol ...core.Policy) plan.Change {
		out := plan.Decide(context.Background(), plan.Input{Resources: res, Policies: pol, Now: time.Unix(0, 0),
			Readers: map[string]core.Reader{"pve": p}})
		return out.Changes[0]
	}

	c := decide(newP(core.Hint{Field: "disk", Destructive: true, Reason: "規則を変える"}))
	if !c.Destructive || c.Action != plan.ActApply || len(c.Classified) != 1 {
		t.Fatalf("%+v", c)
	}
	budget := core.Policy{Name: "b", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: core.Unlimited,
		DestructivePerCycle: 0, DestructivePerWindow: core.Unlimited, MaxIneffectiveApplies: core.Unlimited}}
	if c := decide(newP(core.Hint{Destructive: true}), budget); c.Action != plan.ActHalt {
		t.Fatalf("壊す変更が予算に数えられない: %+v", c)
	}

	c = decide(newP(core.Hint{Field: "disk", NeedsApproval: true, Reason: "volume を付け替える"}))
	if c.Action != plan.ActPending || c.NewRequest == nil || len(c.NewRequest.Rules) != 1 || c.NewRequest.Rules[0].N != 1 {
		t.Fatalf("承認を求めない: %+v", c)
	}
	if c := decide(newP()); c.Action != plan.ActApply || c.NeedsApproval {
		t.Fatalf("%+v", c)
	}
}
