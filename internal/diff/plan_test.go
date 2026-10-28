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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// refiningProvider は RefinePlan を差し替えられる最小の provider。
type refiningProvider struct {
	refine func(ops []core.Op) ([]core.Op, error)
	calls  int
}

func (p *refiningProvider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		"name": {Kind: core.FieldKindString, Identity: true},
		"a":    {Kind: core.FieldKindString},
		"b":    {Kind: core.FieldKindString},
	}}
}
func (p *refiningProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (p *refiningProvider) Read(context.Context, string) (core.Observation, error) {
	return core.Observation{}, nil
}
func (p *refiningProvider) Apply(context.Context, core.Plan) error { return nil }
func (p *refiningProvider) RefinePlan(_ string, _ core.State, _ map[string]string, ops []core.Op) ([]core.Op, error) {
	p.calls++
	return p.refine(ops)
}

func planWith(t *testing.T, refine func([]core.Op) ([]core.Op, error)) (core.Plan, error, *refiningProvider) {
	t.Helper()
	p := &refiningProvider{refine: refine}
	obs := core.Observation{Availability: core.AvailabilityOk, ObservedAt: time.Now(), State: core.State{
		"name": {Value: "x"}, "a": {Value: "1"}, "b": {Value: "1"},
	}}
	desired := DesiredResource{ID: core.NewID("p", core.NewScope(nil), "x"), Fields: map[string]string{"name": "x", "a": "2", "b": "2"}}
	pl, err := ForProvider(p).Plan(obs, desired, cause.CauseContext{})
	return pl, err, p
}

func TestPlan_RefinementOnlyDropsFieldsAndRaisesDestructive(t *testing.T) {
	pl, err, _ := planWith(t, func(ops []core.Op) ([]core.Op, error) {
		op := ops[0]
		// Changed values and an extra field are ignored; Destructive and the
		// diagnostic are taken.
		op.Fields = []core.FieldDiff{{Field: "a", From: "9", To: "9"}, {Field: "evil", To: "x"}}
		op.Destructive = true
		op.Diagnostic = "why"
		// An op of another Kind matches no core op and is ignored.
		other := op
		other.Kind = core.OpKindDelete
		return []core.Op{other, op}, nil
	})
	if err != nil || len(pl.Ops) != 1 {
		t.Fatalf("Plan = %+v, %v", pl, err)
	}
	op := pl.Ops[0]
	if op.Kind != core.OpKindUpdate || !op.Destructive || op.Diagnostic != "why" ||
		len(op.Fields) != 1 || op.Fields[0] != (core.FieldDiff{Field: "a", From: "1", To: "2"}) {
		t.Fatalf("op = %+v; want the core's Update with only field a (core's values), destructive, the provider's diagnostic", op)
	}
}

func TestPlan_RefinementCannotLowerDestructiveAndEmptyOpIsDropped(t *testing.T) {
	pl, err, _ := planWith(t, func(ops []core.Op) ([]core.Op, error) {
		op := ops[0]
		op.Fields = nil
		return []core.Op{op}, nil
	})
	if err != nil || len(pl.Ops) != 0 {
		t.Fatalf("Plan = %+v, %v; want no op once every field is dropped", pl, err)
	}
	if !mergeRefined([]core.Op{{Kind: core.OpKindUpdate, Destructive: true, Fields: []core.FieldDiff{{Field: "a"}}}},
		[]core.Op{{Kind: core.OpKindUpdate, Destructive: false, Fields: []core.FieldDiff{{Field: "a"}}}})[0].Destructive {
		t.Fatal("the refinement lowered Destructive")
	}
}

func TestPlan_RefinementErrorGivesNoOps(t *testing.T) {
	pl, err, _ := planWith(t, func([]core.Op) ([]core.Op, error) { return nil, errors.New("not offered") })
	if err == nil || len(pl.Ops) != 0 {
		t.Fatalf("Plan = %+v, %v; want an error and no op", pl, err)
	}
}

func TestPlan_NoRefinementWithoutOkState(t *testing.T) {
	p := &refiningProvider{refine: func(ops []core.Op) ([]core.Op, error) { return nil, errors.New("must not be called") }}
	desired := DesiredResource{ID: core.NewID("p", core.NewScope(nil), "x"), Fields: map[string]string{"a": "2"}}
	for _, obs := range []core.Observation{{Availability: core.AvailabilityUnknown}, {Availability: core.AvailabilityConverging}} {
		if _, err := ForProvider(p).Plan(obs, desired, cause.CauseContext{}); err != nil {
			t.Fatalf("Plan(%v) = %v", obs.Availability, err)
		}
	}
	if p.calls != 0 {
		t.Fatalf("RefinePlan called %d time(s) without an Ok State", p.calls)
	}
}

// approvingProvider は ApprovalRequirer を持つ最小の provider（Backlog #179）。
type approvingProvider struct {
	refiningProvider
	seen []core.OpKind
}

func (p *approvingProvider) ApprovalRequired(_ string, observed core.State, _ map[string]string, op core.Op) string {
	p.seen = append(p.seen, op.Kind)
	if op.Kind == core.OpKindCreate && observed != nil {
		return "observed must be nil for a create"
	}
	return "needs approval"
}

func TestPlan_ApprovalRequirerMarksCreateAndUpdate(t *testing.T) {
	p := &approvingProvider{refiningProvider: refiningProvider{refine: func(ops []core.Op) ([]core.Op, error) { return ops, nil }}}
	desired := DesiredResource{ID: core.NewID("p", core.NewScope(nil), "x"), Fields: map[string]string{"name": "x", "a": "2"}}
	eng := ForProvider(p)
	for _, obs := range []core.Observation{
		{Availability: core.AvailabilityOk},
		{Availability: core.AvailabilityOk, State: core.State{"name": {Value: "x"}, "a": {Value: "1"}}},
	} {
		pl, err := eng.Plan(obs, desired, cause.CauseContext{})
		if err != nil || len(pl.Ops) != 1 {
			t.Fatalf("Plan = %+v, %v", pl, err)
		}
		op := pl.Ops[0]
		if !op.Destructive || !op.RequiresApproval || op.ApprovalReason != "needs approval" {
			t.Fatalf("%v: want destructive, requiring approval with the reason: %+v", op.Kind, op)
		}
	}
	// Unknown: no ops, not asked; an explicit absent is the core's alone.
	if _, err := eng.Plan(core.Observation{Availability: core.AvailabilityUnknown}, desired, cause.CauseContext{}); err != nil {
		t.Fatal(err)
	}
	absent := DesiredResource{ID: desired.ID, Absent: true}
	pl, _ := eng.Plan(core.Observation{Availability: core.AvailabilityOk, State: core.State{"name": {Value: "x"}}}, absent, cause.CauseContext{})
	if len(pl.Ops) != 1 || pl.Ops[0].ApprovalReason != "" {
		t.Fatalf("an absent's delete keeps the core's approval: %+v", pl)
	}
	if len(p.seen) != 2 || p.seen[0] != core.OpKindCreate || p.seen[1] != core.OpKindUpdate {
		t.Fatalf("asked for %v, want create and update", p.seen)
	}
}
