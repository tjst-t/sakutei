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

package capacity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func pool(used, total any) core.Observation {
	return core.Observation{
		Availability: core.AvailabilityOk,
		State: core.State{
			"used":  {Kind: core.FieldKindInt, Value: used},
			"total": {Kind: core.FieldKindInt, Value: total},
		},
	}
}

func TestParseGate(t *testing.T) {
	g, err := ParseGate("resource=cluster-a, used=u, total=t, max_utilization=80%, on_exceed=halt-destructive-only, provider=pve")
	if err != nil {
		t.Fatal(err)
	}
	want := Gate{Provider: "pve", Resource: "cluster-a", UsedField: "u", TotalField: "t",
		MaxUtilization: 80, OnExceed: budget.OnExceedHaltDestructiveOnly}
	if g != want {
		t.Errorf("got %+v, want %+v", g, want)
	}
	for _, bad := range []string{
		"resource=c,used=u",                              // total / max missing
		"resource=c,used=u,total=t",                      // max missing
		"resource=c,used=u,total=t,max_utilization=0",    // out of range
		"resource=c,used=u,total=t,max_utilization=120%", // out of range
		"resource=c,used=u,total=t,max_utilization=x",    // not numeric
		"resource=c,used=u,total=t,max_utilization=80,on_exceed=x",
		"resource=c,used=u,total=t,max_utilization=80,color=red",
		"resource",
	} {
		if _, err := ParseGate(bad); err == nil {
			t.Errorf("ParseGate(%q) must fail", bad)
		}
	}
}

func TestEvaluate(t *testing.T) {
	g := Gate{Resource: "c", UsedField: "used", TotalField: "total", MaxUtilization: 80}
	cases := []struct {
		name   string
		obs    core.Observation
		err    error
		halted bool
		inMsg  string
	}{
		{"under", pool(50, 100), nil, false, ""},
		{"at threshold is within", pool(80, 100), nil, false, ""},
		{"over", pool(81, 100), nil, true, "81.0%"},
		{"float values", pool(90.5, 100.0), nil, true, "90.5%"},
		{"string values", pool("9", "10"), nil, true, "90.0%"},
		{"read error", core.Observation{}, errors.New("boom"), true, "unknown"},
		{"unknown availability", core.Observation{Availability: core.AvailabilityUnknown}, nil, true, "unknown"},
		{"absent", core.Observation{Availability: core.AvailabilityOk}, nil, true, "unknown"},
		{"missing field", core.Observation{Availability: core.AvailabilityOk,
			State: core.State{"used": {Value: 1}}}, nil, true, "total"},
		{"non numeric", pool("lots", 100), nil, true, "not numeric"},
		{"zero total", pool(0, 0), nil, true, "not positive"},
	}
	for _, c := range cases {
		d := g.Evaluate(c.obs, c.err)
		if d.HaltAll != c.halted || d.HaltDestructive != c.halted {
			t.Errorf("%s: got %+v, want halted=%v", c.name, d, c.halted)
		}
		if c.inMsg != "" && !strings.Contains(d.Reason, c.inMsg) {
			t.Errorf("%s: reason %q lacks %q", c.name, d.Reason, c.inMsg)
		}
	}

	g.OnExceed = budget.OnExceedHaltDestructiveOnly
	if d := g.Evaluate(pool(99, 100), nil); d.HaltAll || !d.HaltDestructive {
		t.Errorf("halt-destructive-only: got %+v", d)
	}
}

func TestCheckAllMergesAndNilProvider(t *testing.T) {
	gates := []Gate{{Resource: "c", UsedField: "used", TotalField: "total", MaxUtilization: 80}}
	d := CheckAll(context.Background(), gates, func(Gate) provider.Provider { return nil })
	if !d.HaltAll || !strings.Contains(d.Reason, "not available") {
		t.Errorf("a gate whose provider is unavailable must halt; got %+v", d)
	}
	if d := CheckAll(context.Background(), nil, nil); d.HaltAll || d.HaltDestructive {
		t.Errorf("no gates must not halt; got %+v", d)
	}
}

func TestMerge(t *testing.T) {
	a := budget.CycleDecision{HaltDestructive: true, Reason: "a"}
	b := budget.CycleDecision{HaltAll: true, HaltDestructive: true, Reason: "b"}
	if m := budget.Merge(a, b); !m.HaltAll || !m.HaltDestructive || m.Reason != "a; b" {
		t.Errorf("Merge = %+v", m)
	}
	if m := budget.Merge(budget.CycleDecision{}, a); m.HaltAll || !m.HaltDestructive || m.Reason != "a" {
		t.Errorf("Merge = %+v", m)
	}
}
