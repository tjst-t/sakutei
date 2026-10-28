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

package diff

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// ensure: absent（#163）: 在れば破壊的で承認の要る Delete、無ければ何もしない。
func TestDiff_Absent(t *testing.T) {
	e := New(provider.Schema{Fields: map[string]provider.FieldSpec{"a": {Kind: core.FieldKindString}}})
	r := DesiredResource{ID: core.NewID("fake", core.NewScope(nil), "old"), Absent: true}

	plan := e.Diff(core.Observation{Availability: core.AvailabilityOk, State: core.State{"a": {Value: "1"}}}, r, cause.CauseContext{})
	if len(plan.Ops) != 1 {
		t.Fatalf("present: ops = %+v; want one Delete", plan.Ops)
	}
	op := plan.Ops[0]
	if op.Kind != core.OpKindDelete || !op.Destructive || !op.RequiresApproval || op.Cause != core.CauseTagA {
		t.Errorf("present: op = %+v; want a destructive Delete requiring approval, cause A on a cold start", op)
	}
	if plan := e.Diff(core.Observation{Availability: core.AvailabilityOk}, r, cause.CauseContext{}); len(plan.Ops) != 0 {
		t.Errorf("absent in reality: ops = %+v; want none", plan.Ops)
	}
	if plan := e.Diff(core.Observation{Availability: core.AvailabilityUnknown}, r, cause.CauseContext{}); len(plan.Ops) != 0 {
		t.Errorf("unknown: ops = %+v; want none (unknown ≠ absent)", plan.Ops)
	}
}

func TestValidateAbsent(t *testing.T) {
	id := func(l string) core.ID { return core.NewID("fake", core.NewScope(nil), l) }
	ok := []DesiredResource{{ID: id("old"), Absent: true}, {ID: id("a"), DependsOn: []string{"b"}}, {ID: id("b")}}
	if err := ValidateAbsent(ok); err != nil {
		t.Errorf("ValidateAbsent(ok) = %v", err)
	}
	for name, rs := range map[string][]DesiredResource{
		"fields":     {{ID: id("old"), Absent: true, Fields: map[string]string{"a": "1"}}},
		"depends_on": {{ID: id("old"), Absent: true, DependsOn: []string{"b"}}, {ID: id("b")}},
		"dependent":  {{ID: id("old"), Absent: true}, {ID: id("a"), DependsOn: []string{"old"}}},
	} {
		if err := ValidateAbsent(rs); err == nil || !strings.Contains(err.Error(), "absent") {
			t.Errorf("%s: ValidateAbsent = %v; want a refusal", name, err)
		}
	}
}
