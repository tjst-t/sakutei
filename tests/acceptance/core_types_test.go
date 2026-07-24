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

// Package acceptance contains library-style acceptance tests for Story
// Sead944-1.  As an external consumer of the public API, this package may
// only import and call exported symbols from pkg/core and pkg/provider.
//
// Each test function references the Acceptance Criterion it verifies in the
// form [AC-Sead944-1-N].
package acceptance

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// ---------------------------------------------------------------------------
// [AC-Sead944-1-1] Composite ID uniqueness and equality
// ---------------------------------------------------------------------------

// TestCompositeID_Equality verifies that two IDs are equal iff all three
// components (Provider, Scope, Local) match, and that the string
// representation contains all three components.
//
// Scenario step 1 from scenario-Sead944-1.json:
//
//	core.NewID("proxmox_vm", scope, "orders-db") compared against
//	IDs with a different provider or different scope.
func TestCompositeID_Equality(t *testing.T) {
	// [AC-Sead944-1-1] IDs with all three components equal must be equal.
	scopeProd := core.NewScope(map[string]string{"site": "tokyo", "cluster": "prod"})
	id1 := core.NewID("proxmox_vm", scopeProd, "orders-db")
	id2 := core.NewID("proxmox_vm", scopeProd, "orders-db")

	if !id1.Equal(id2) {
		t.Errorf("[AC-Sead944-1-1] identical IDs should be equal: %v vs %v", id1, id2)
	}

	// [AC-Sead944-1-1] IDs with a different provider must NOT be equal.
	idDiffProvider := core.NewID("netbox_vm", scopeProd, "orders-db")
	if id1.Equal(idDiffProvider) {
		t.Errorf("[AC-Sead944-1-1] IDs with different providers should not be equal")
	}

	// [AC-Sead944-1-1] IDs with a different scope must NOT be equal.
	scopeStaging := core.NewScope(map[string]string{"site": "tokyo", "cluster": "staging"})
	idDiffScope := core.NewID("proxmox_vm", scopeStaging, "orders-db")
	if id1.Equal(idDiffScope) {
		t.Errorf("[AC-Sead944-1-1] IDs with different scopes should not be equal")
	}

	// [AC-Sead944-1-1] IDs with a different local ID must NOT be equal.
	idDiffLocal := core.NewID("proxmox_vm", scopeProd, "payments-db")
	if id1.Equal(idDiffLocal) {
		t.Errorf("[AC-Sead944-1-1] IDs with different local IDs should not be equal")
	}
}

// TestCompositeID_StringContainsAllComponents verifies that ID.String()
// contains the provider name, scope representation, and local ID.
func TestCompositeID_StringContainsAllComponents(t *testing.T) {
	// [AC-Sead944-1-1] String() must include all three components.
	scope := core.NewScope(map[string]string{"cluster": "prod"})
	id := core.NewID("proxmox_vm", scope, "orders-db")
	s := id.String()

	for _, want := range []string{"proxmox_vm", "cluster=prod", "orders-db"} {
		if !strings.Contains(s, want) {
			t.Errorf("[AC-Sead944-1-1] ID.String() %q does not contain %q", s, want)
		}
	}
}

// TestCompositeID_EmptyScopeIsValid verifies that an empty-label Scope does
// not panic and still produces a consistent string.
func TestCompositeID_EmptyScopeIsValid(t *testing.T) {
	// [AC-Sead944-1-1] Empty scope is a valid edge case.
	emptyScope := core.NewScope(nil)
	id := core.NewID("dns", emptyScope, "example.com")
	s := id.String()
	if !strings.Contains(s, "dns") || !strings.Contains(s, "example.com") {
		t.Errorf("[AC-Sead944-1-1] ID.String() with empty scope should still include provider and local: %q", s)
	}
}

// TestIDKey_EqualIffKeysEqual verifies the collision-free invariant of
// ID.Key() in BOTH directions (FIX #8):
//
//	(a) Equal IDs  → equal Keys.
//	(b) distinct IDs → distinct Keys, even when a separator character
//	    ('/', '=', ',') appears inside a scope-label value or the local ID and
//	    would make String() alias them.
//
// The doc for IDKey claims "two IDs are Equal iff their Keys are equal"; before
// FIX #8, Key() was IDKey(id.String()), which aliased the pairs below (the
// reverse direction failed).  Key() now uses a length-delimited encoding so no
// separator ambiguity can exist.
//
// [AC-Sead944-1-1]
func TestIDKey_EqualIffKeysEqual(t *testing.T) {
	// (a) Equal IDs must yield equal Keys.
	scope := core.NewScope(map[string]string{"site": "tokyo", "cluster": "prod"})
	a := core.NewID("fake", scope, "orders-db")
	b := core.NewID("fake", core.NewScope(map[string]string{"cluster": "prod", "site": "tokyo"}), "orders-db")
	if !a.Equal(b) {
		t.Fatalf("[AC-Sead944-1-1] precondition: a and b should be Equal")
	}
	if a.Key() != b.Key() {
		t.Errorf("[AC-Sead944-1-1] Equal IDs must have equal Keys; got %q vs %q", a.Key(), b.Key())
	}

	// keysDistinct is a table of ID pairs that are NOT Equal but which, under the
	// old String()-based key, collided.  Each pair must now have DISTINCT keys.
	type pair struct {
		name string
		x, y core.ID
	}
	pairs := []pair{
		{
			// scope {site:"x/a"}+local "b"  vs  scope {site:"x"}+local "a/b".
			// String() → "fake/site=x/a/b" for BOTH → collision under old Key().
			name: "slash-in-scope-value-vs-local",
			x:    core.NewID("fake", core.NewScope(map[string]string{"site": "x/a"}), "b"),
			y:    core.NewID("fake", core.NewScope(map[string]string{"site": "x"}), "a/b"),
		},
		{
			// A label VALUE containing ',' and '=' must not alias a two-label scope.
			// {site:"x,cluster=y"} vs {site:"x", cluster:"y"} both render to the
			// same "cluster=y,site=x"-ish String() tail depending on ordering.
			name: "comma-and-equals-in-label-value",
			x:    core.NewID("fake", core.NewScope(map[string]string{"site": "x,cluster=y"}), "z"),
			y:    core.NewID("fake", core.NewScope(map[string]string{"site": "x", "cluster": "y"}), "z"),
		},
		{
			// A label KEY/value split ambiguity: value "b=c" under key "a" vs
			// value "c" under key "a=b" would collide via '='-join.
			name: "equals-in-value-vs-key",
			x:    core.NewID("fake", core.NewScope(map[string]string{"a": "b=c"}), "l"),
			y:    core.NewID("fake", core.NewScope(map[string]string{"a=b": "c"}), "l"),
		},
	}
	for _, p := range pairs {
		if p.x.Equal(p.y) {
			t.Fatalf("[AC-Sead944-1-1] %s: precondition failed — pair must NOT be Equal", p.name)
		}
		if p.x.Key() == p.y.Key() {
			t.Errorf("[AC-Sead944-1-1] %s: distinct IDs collided on Key()=%q (String() x=%q y=%q)",
				p.name, p.x.Key(), p.x.String(), p.y.String())
		}
	}
}

// TestScope_Path_Deterministic verifies that Path() is deterministic
// regardless of map iteration order.
func TestScope_Path_Deterministic(t *testing.T) {
	// [AC-Sead944-1-1] Path() must be stable.
	labels := map[string]string{"cluster": "prod", "site": "tokyo", "rack": "r03"}
	s1 := core.NewScope(labels)
	s2 := core.NewScope(labels)

	if s1.Path() != s2.Path() {
		t.Errorf("[AC-Sead944-1-1] Path() must be deterministic: %q vs %q", s1.Path(), s2.Path())
	}

	// Path must contain all keys.
	for _, k := range []string{"cluster", "site", "rack"} {
		if !strings.Contains(s1.Path(), k) {
			t.Errorf("[AC-Sead944-1-1] Path() missing key %q: %q", k, s1.Path())
		}
	}
}

// ---------------------------------------------------------------------------
// [AC-Sead944-1-2] State typed values and Observation
// ---------------------------------------------------------------------------

// TestState_TypedValues verifies that State can hold typed values and that
// the Kind field is preserved.
//
// Scenario step 2 from scenario-Sead944-1.json:
//
//	core.State with cpu:Int and image:String values.
func TestState_TypedValues(t *testing.T) {
	// [AC-Sead944-1-2] State holds typed values keyed by field name.
	st := core.State{
		"cpu": core.TypedValue{
			Kind:  core.FieldKindInt,
			Value: int64(4),
		},
		"image": core.TypedValue{
			Kind:  core.FieldKindString,
			Value: "ubuntu-22.04",
		},
	}

	cpuTV, ok := st["cpu"]
	if !ok {
		t.Fatal("[AC-Sead944-1-2] State missing 'cpu' field")
	}
	if cpuTV.Kind != core.FieldKindInt {
		t.Errorf("[AC-Sead944-1-2] cpu Kind = %v, want FieldKindInt", cpuTV.Kind)
	}
	if cpuTV.Value.(int64) != 4 {
		t.Errorf("[AC-Sead944-1-2] cpu Value = %v, want 4", cpuTV.Value)
	}

	imgTV, ok := st["image"]
	if !ok {
		t.Fatal("[AC-Sead944-1-2] State missing 'image' field")
	}
	if imgTV.Kind != core.FieldKindString {
		t.Errorf("[AC-Sead944-1-2] image Kind = %v, want FieldKindString", imgTV.Kind)
	}
	if imgTV.Value.(string) != "ubuntu-22.04" {
		t.Errorf("[AC-Sead944-1-2] image Value = %v, want 'ubuntu-22.04'", imgTV.Value)
	}
}

// TestObservation_Availability verifies that Observation correctly carries
// Availability, State, and ObservedAt.
//
// Scenario step 2 from scenario-Sead944-1.json:
//
//	Observation{Availability: Unknown, State, ObservedAt}
func TestObservation_Availability(t *testing.T) {
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)

	st := core.State{
		"cpu": core.TypedValue{Kind: core.FieldKindInt, Value: int64(8)},
	}

	// [AC-Sead944-1-2] Availability == Unknown: state is present in the
	// struct but semantically invalid (provider could not reach the target).
	obs := core.Observation{
		Availability: core.AvailabilityUnknown,
		State:        st,
		ObservedAt:   now,
	}

	if obs.Availability != core.AvailabilityUnknown {
		t.Errorf("[AC-Sead944-1-2] Availability = %v, want Unknown", obs.Availability)
	}
	if !obs.ObservedAt.Equal(now) {
		t.Errorf("[AC-Sead944-1-2] ObservedAt = %v, want %v", obs.ObservedAt, now)
	}

	// [AC-Sead944-1-2] Availability == Ok: State is valid.
	obsOk := core.Observation{
		Availability: core.AvailabilityOk,
		State:        st,
		ObservedAt:   now,
	}
	if obsOk.Availability != core.AvailabilityOk {
		t.Errorf("[AC-Sead944-1-2] Availability = %v, want Ok", obsOk.Availability)
	}
	if obsOk.State["cpu"].Kind != core.FieldKindInt {
		t.Errorf("[AC-Sead944-1-2] State cpu Kind = %v, want FieldKindInt", obsOk.State["cpu"].Kind)
	}

	// [AC-Sead944-1-2] Availability == Converging: dedicated value.
	obsConverging := core.Observation{
		Availability: core.AvailabilityConverging,
		ObservedAt:   now,
	}
	if obsConverging.Availability != core.AvailabilityConverging {
		t.Errorf("[AC-Sead944-1-2] Availability = %v, want Converging", obsConverging.Availability)
	}
}

// TestObservation_ThreeValuesDistinct verifies that the three Availability
// constants are distinct (Ok ≠ Unknown ≠ Converging).
func TestObservation_ThreeValuesDistinct(t *testing.T) {
	// [AC-Sead944-1-2] Three-value distinctness is the core non-conflation
	// contract (§4.5, unknown ≠ absent).
	if core.AvailabilityOk == core.AvailabilityUnknown {
		t.Error("[AC-Sead944-1-2] Ok and Unknown must be distinct")
	}
	if core.AvailabilityOk == core.AvailabilityConverging {
		t.Error("[AC-Sead944-1-2] Ok and Converging must be distinct")
	}
	if core.AvailabilityUnknown == core.AvailabilityConverging {
		t.Error("[AC-Sead944-1-2] Unknown and Converging must be distinct")
	}
}

// ---------------------------------------------------------------------------
// [AC-Sead944-1-3] Plan Op list, Destructive flag, TargetID
// ---------------------------------------------------------------------------

// TestPlan_OpsAndDestructive verifies that Plan carries an Op list with
// OpKind, Destructive flag, and Target ID.
//
// Scenario step 3 from scenario-Sead944-1.json:
//
//	Plan with Op(Create/Update/Delete), destructive flag, target ID.
func TestPlan_OpsAndDestructive(t *testing.T) {
	scope := core.NewScope(map[string]string{"cluster": "prod"})
	idVM := core.NewID("proxmox_vm", scope, "orders-db")

	// [AC-Sead944-1-3] Plan holds a list of Ops.
	plan := core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindCreate,
				Target:      idVM,
				Destructive: false,
				Cause:       core.CauseTagA,
				Diagnostic:  "resource does not exist; will create",
			},
		},
	}

	if len(plan.Ops) != 1 {
		t.Fatalf("[AC-Sead944-1-3] expected 1 Op, got %d", len(plan.Ops))
	}
	op := plan.Ops[0]

	if op.Kind != core.OpKindCreate {
		t.Errorf("[AC-Sead944-1-3] Op.Kind = %v, want Create", op.Kind)
	}
	if !op.Target.Equal(idVM) {
		t.Errorf("[AC-Sead944-1-3] Op.Target = %v, want %v", op.Target, idVM)
	}
	if op.Destructive {
		t.Error("[AC-Sead944-1-3] Create op should not be Destructive")
	}
}

// TestPlan_DeleteIsDestructive verifies that a Delete Op carries Destructive.
func TestPlan_DeleteIsDestructive(t *testing.T) {
	// [AC-Sead944-1-3] Delete Ops should be marked Destructive.
	scope := core.NewScope(map[string]string{"cluster": "prod"})
	idVM := core.NewID("proxmox_vm", scope, "zombie-vm")

	plan := core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindDelete,
				Target:      idVM,
				Destructive: true,
				Cause:       core.CauseTagB,
				Diagnostic:  "resource present in reality but absent from desired; pruning",
			},
		},
	}

	op := plan.Ops[0]
	if op.Kind != core.OpKindDelete {
		t.Errorf("[AC-Sead944-1-3] Op.Kind = %v, want Delete", op.Kind)
	}
	if !op.Destructive {
		t.Error("[AC-Sead944-1-3] Delete op must be Destructive")
	}
}

// TestPlan_UpdateOp verifies the Update Op with FieldDiff.
func TestPlan_UpdateOp(t *testing.T) {
	// [AC-Sead944-1-3] Update Op carries field-level diffs.
	scope := core.NewScope(map[string]string{"cluster": "prod"})
	idVM := core.NewID("proxmox_vm", scope, "orders-db")

	plan := core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindUpdate,
				Target:      idVM,
				Destructive: false,
				Cause:       core.CauseTagA,
				Diagnostic:  "cpu count increased",
				Fields: []core.FieldDiff{
					{Field: "cpu", From: "4", To: "8"},
				},
			},
		},
	}

	op := plan.Ops[0]
	if op.Kind != core.OpKindUpdate {
		t.Errorf("[AC-Sead944-1-3] Op.Kind = %v, want Update", op.Kind)
	}
	if len(op.Fields) != 1 || op.Fields[0].Field != "cpu" {
		t.Errorf("[AC-Sead944-1-3] expected Fields=[{cpu 4 8}], got %v", op.Fields)
	}
}

// TestPlan_CauseTags verifies that all five CauseTag constants are defined
// and distinct.
func TestPlan_CauseTags(t *testing.T) {
	// [AC-Sead944-1-3] All A–E cause tags must be defined and distinct.
	tags := []core.CauseTag{
		core.CauseTagA,
		core.CauseTagB,
		core.CauseTagC,
		core.CauseTagD,
		core.CauseTagE,
	}
	seen := map[core.CauseTag]bool{}
	for _, tag := range tags {
		if seen[tag] {
			t.Errorf("[AC-Sead944-1-3] duplicate CauseTag value: %q", tag)
		}
		seen[tag] = true
	}
	if len(seen) != 5 {
		t.Errorf("[AC-Sead944-1-3] expected 5 distinct CauseTags, got %d", len(seen))
	}
}

// TestPlan_OpKinds verifies that Create/Update/Delete constants are distinct.
func TestPlan_OpKinds(t *testing.T) {
	// [AC-Sead944-1-3] Three OpKinds must be distinct.
	if core.OpKindCreate == core.OpKindUpdate {
		t.Error("[AC-Sead944-1-3] Create and Update must be distinct")
	}
	if core.OpKindCreate == core.OpKindDelete {
		t.Error("[AC-Sead944-1-3] Create and Delete must be distinct")
	}
	if core.OpKindUpdate == core.OpKindDelete {
		t.Error("[AC-Sead944-1-3] Update and Delete must be distinct")
	}
}

// ---------------------------------------------------------------------------
// Provider interface smoke tests
// ---------------------------------------------------------------------------

// TestProviderCapabilities_ZeroValue verifies that the zero-value Capabilities
// struct is valid and has sensible (safe) defaults.
func TestProviderCapabilities_ZeroValue(t *testing.T) {
	// [AC-Sead944-1-3] Zero-value Capabilities must compile and be "safe":
	// TrueObserve=false → destructive side; List=false → no prune.
	var caps provider.Capabilities
	if caps.TrueObserve {
		t.Error("zero-value TrueObserve should be false (safe default)")
	}
	if caps.List {
		t.Error("zero-value List should be false (safe default)")
	}
}

// TestProviderSchema_ZeroValue verifies that a zero-value Schema is constructible.
func TestProviderSchema_ZeroValue(t *testing.T) {
	// [AC-Sead944-1-3] Zero-value Schema must not panic.
	var s provider.Schema
	_ = s // nil Fields is valid; Schema-driven diff handles it.
}

// TestTransientAndPermanentError verifies the error type helpers.
func TestTransientAndPermanentError(t *testing.T) {
	// [AC-Sead944-1-3] Error classification types must be usable.
	base := errors.New("connection refused")
	te := provider.NewTransientError(base)
	pe := provider.NewPermanentError(base)

	if te.Error() == "" {
		t.Error("TransientError.Error() must not be empty")
	}
	if pe.Error() == "" {
		t.Error("PermanentError.Error() must not be empty")
	}
	// Unwrap must return the original error.
	if te.Unwrap() != base {
		t.Error("TransientError.Unwrap() must return the cause")
	}
	if pe.Unwrap() != base {
		t.Error("PermanentError.Unwrap() must return the cause")
	}
}
