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

package sdk

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// TB is the subset of testing.TB used by ContractTester.  It is satisfied by
// *testing.T and *testing.B from the stdlib testing package without requiring
// this file to import "testing".  Not importing "testing" keeps the production
// binary free of testing overhead even though ContractTester lives in a
// non-test file (Go's linker dead-strips unreachable symbols, but importing
// the testing package unconditionally pulls in its init() registration which
// this design avoids).
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// ContractTester runs the standard provider contract tests against any
// provider.Provider implementation (§28.6, ADR-0022).
//
// The contracts verified are:
//
//  1. Idempotency: Apply twice → Read result is identical both times.
//  2. Absent→Create→Read: After a Create Apply, Read returns Availability=Ok
//     and a State that matches the desired fields.  For a minimal-tier
//     provider (Capabilities.TrueObserve == false) only the Identity fields
//     must be present; other fields are compared only when returned.
//  3. List completeness: Every ID returned by List can be Read successfully
//     with Availability=Ok.
//
// A provider that implements provider.CheckReader (minimal tier observed by a
// dry run, §28.7) gets the check-mode contracts instead (runCheckContracts).
//
// These contracts are the minimum that any provider must satisfy for the
// convergence loop to work correctly.  A provider that violates them will
// cause repeated destructive operations, silent data loss, or incorrect
// prune behaviour.
//
// Usage in provider unit tests:
//
//	func TestMyProviderContracts(t *testing.T) {
//	    p := mypackage.New(t.TempDir())
//	    ct := sdk.ContractTester{
//	        Provider:      p,
//	        Lister:        p,                   // nil if provider does not list
//	        Scope:         core.NewScope(nil),
//	        SampleID:      "test-resource-1",
//	        SampleDesired: map[string]string{"name": "test-resource-1", "size": "10"},
//	    }
//	    ct.RunAll(t)
//	}
//
// The RunAll method accepts any value that implements TB, which includes
// *testing.T from the standard library testing package.
type ContractTester struct {
	// Provider is the provider implementation under test.  Required.
	Provider provider.Provider

	// Lister is the Lister implementation, if the provider supports List.
	// May be nil; when nil, List completeness tests are skipped.
	Lister provider.Lister

	// Scope is the scope used for List calls.
	Scope core.Scope

	// SampleID is the provider-local ID of the test resource that will be
	// created by the Apply contract tests.  Must not conflict with existing
	// resources in the provider (or set Cleanup to remove it before RunAll).
	SampleID string

	// SampleDesired is the desired field values for the SampleID resource.
	// These are used to build the Create Plan in the idempotency and
	// absent→Create→Read tests.
	SampleDesired map[string]string

	// Cleanup is called after all contract tests complete to remove the
	// test resource from the provider.  If nil, no cleanup is performed.
	Cleanup func() error

	// ── Generation from Schema (§28.3 / §28.6, Backlog #32) ──────────────────
	//
	// When SampleDesired is nil, RunAll generates it from Provider.Schema():
	// every field that is neither Computed nor Managed gets a value — the one
	// in Examples if given, otherwise a default derived from the field's Kind
	// (see DefaultValueForKind).  Provider authors only supply Examples for
	// fields whose values the target validates (an IP address, a rule syntax).

	// Examples maps a field name to the example value to use for it when
	// SampleDesired is generated.  Ignored when SampleDesired is set.
	Examples map[string]string

	// MakeID derives the provider-local ID from the generated desired fields
	// when SampleID is empty.  When nil, the default is the values of the
	// Identity fields sorted by field name and joined with "/" (a single
	// identity field gives its value as-is).
	MakeID func(fields map[string]string) string

	// MakeUnreachable, if set, makes the provider's target unreachable and
	// returns a function that restores it.  RunAll then checks three-valued
	// correctness (§4.5): with the target unreachable, Read of the sample must
	// return an error or Availability Unknown — never Ok with a nil State,
	// which would claim the resource is absent (unknown ≠ absent).
	MakeUnreachable func() (restore func(), err error)

	// Preexisting says the sample resource already exists in the target before
	// any Apply: the provider manages properties of something it never creates
	// or deletes (e.g. the power of a server behind a BMC, §29.1).  The
	// absent→Create→Read contract then starts from that existing resource:
	// the Read before Apply must be Ok with a State, and that State must NOT
	// already match SampleDesired (a provider that peeks desired would report
	// it, ADR-0002) — so the test author sets up the target to differ from the
	// sample.  The Create plan is still what Apply receives.
	Preexisting bool
}

// DefaultValueForKind is the value used for a field without an Example when
// SampleDesired is generated from the Schema.  The values are deliberately
// plain so that any provider that does not validate a field accepts them.
func DefaultValueForKind(field string, kind core.FieldKind) string {
	switch kind {
	case core.FieldKindInt:
		return "1"
	case core.FieldKindBool:
		return "true"
	case core.FieldKindFloat:
		return "1.5"
	case core.FieldKindSecretRef:
		return "secretref:file/sdk-contract/" + field + "#gen1"
	default: // String, StringList, Map and unknown kinds
		return "sdk-contract-" + field
	}
}

// GenerateSample builds the sample local ID and desired fields from the
// provider's Schema, Examples and MakeID (see the field docs).  It is what
// RunAll uses when SampleDesired / SampleID are not given, exported so a
// provider author can print or inspect the generated sample.
func (ct *ContractTester) GenerateSample() (id string, desired map[string]string, err error) {
	if ct.Provider == nil {
		return "", nil, fmt.Errorf("ContractTester: Provider is nil")
	}
	schema := ct.Provider.Schema()
	desired = ct.SampleDesired
	if desired == nil {
		desired = make(map[string]string)
		for name, spec := range schema.Fields {
			if spec.Computed || spec.Managed {
				continue
			}
			if v, ok := ct.Examples[name]; ok {
				desired[name] = v
				continue
			}
			kind := spec.Kind
			if spec.Secret {
				kind = core.FieldKindSecretRef
			}
			desired[name] = DefaultValueForKind(name, kind)
		}
		if len(desired) == 0 {
			return "", nil, fmt.Errorf("ContractTester: the Schema has no writable field; pass SampleDesired")
		}
	}

	id = ct.SampleID
	if id == "" {
		if ct.MakeID != nil {
			id = ct.MakeID(desired)
		} else {
			var idFields []string
			for name, spec := range schema.Fields {
				if spec.Identity {
					idFields = append(idFields, name)
				}
			}
			sort.Strings(idFields)
			parts := make([]string, 0, len(idFields))
			for _, f := range idFields {
				parts = append(parts, desired[f])
			}
			id = strings.Join(parts, "/")
		}
		if id == "" {
			return "", nil, fmt.Errorf("ContractTester: cannot derive the sample ID (no Identity field and no MakeID); pass SampleID or MakeID")
		}
	}
	return id, desired, nil
}

// RunAll runs all contract tests against the provider using t for reporting.
// t must be a *testing.T (or any value satisfying TB).
//
// Tests run sequentially because they share provider state.
func (ct *ContractTester) RunAll(t TB) {
	t.Helper()

	// Work on a copy so the caller's ContractTester is not modified when the
	// sample is generated from the Schema.
	c := *ct
	id, desired, err := c.GenerateSample()
	if err != nil {
		t.Fatalf("%v", err)
		return
	}
	c.SampleID, c.SampleDesired = id, desired
	if ct.SampleDesired == nil {
		t.Logf("ContractTester: sample generated from Schema: id=%q fields=%v", id, desired)
	}
	c.runAll(t)
}

func (ct *ContractTester) runAll(t TB) {
	t.Helper()

	// A minimal-tier provider observed by its dry run (provider.CheckReader,
	// §28.7) has no State to compare: it gets the check-mode contracts.
	if cr, ok := ct.Provider.(provider.CheckReader); ok {
		ct.runCheckContracts(t, cr)
		if ct.Cleanup != nil {
			if err := ct.Cleanup(); err != nil {
				t.Logf("ContractTester: cleanup returned error (non-fatal): %v", err)
			}
		}
		return
	}

	// AF-3 (S62dbba verify): runAbsentCreateRead MUST run first, while the
	// sample resource is still genuinely absent.  Its pre-Apply Read guard
	// (ADR-0002 "Read never peeks desired") is only meaningful against a truly
	// absent starting point; running runIdempotency first would create the
	// resource and turn that guard into a false positive.
	ct.runAbsentCreateRead(t)
	ct.runIdempotency(t)

	if ct.Lister != nil {
		ct.runListCompleteness(t)
	}

	if ct.MakeUnreachable != nil {
		ct.runThreeValued(t)
	}

	if ct.Cleanup != nil {
		if err := ct.Cleanup(); err != nil {
			t.Logf("ContractTester: cleanup returned error (non-fatal): %v", err)
		}
	}
}

// runIdempotency verifies that applying the same Plan twice leaves the
// resource in the same final state both times.
//
// Contract: Apply(plan) → Read → Apply(plan) → Read: both Reads return equal State.
func (ct *ContractTester) runIdempotency(t TB) {
	t.Helper()
	ctx := context.Background()

	plan := ct.buildCreatePlan()

	// First Apply.
	if err := ct.Provider.Apply(ctx, plan); err != nil {
		t.Fatalf("ContractTester/Idempotency: first Apply failed: %v", err)
		return
	}

	// First Read.
	obs1, err := ct.Provider.Read(ctx, ct.SampleID)
	if err != nil {
		t.Fatalf("ContractTester/Idempotency: first Read failed: %v", err)
		return
	}
	if obs1.Availability != core.AvailabilityOk {
		t.Fatalf("ContractTester/Idempotency: first Read availability = %v, want Ok", obs1.Availability)
		return
	}

	// Second Apply (same Plan).
	if err := ct.Provider.Apply(ctx, plan); err != nil {
		t.Fatalf("ContractTester/Idempotency: second Apply failed: %v", err)
		return
	}

	// Second Read.
	obs2, err := ct.Provider.Read(ctx, ct.SampleID)
	if err != nil {
		t.Fatalf("ContractTester/Idempotency: second Read failed: %v", err)
		return
	}
	if obs2.Availability != core.AvailabilityOk {
		t.Fatalf("ContractTester/Idempotency: second Read availability = %v, want Ok", obs2.Availability)
		return
	}

	// Both reads must produce the same State.
	if !statesEqual(obs1.State, obs2.State) {
		t.Errorf("ContractTester/Idempotency: State after first Apply+Read != State after second Apply+Read\nfirst:  %v\nsecond: %v",
			obs1.State, obs2.State)
	}
	t.Logf("ContractTester/Idempotency: PASS")
}

// runAbsentCreateRead verifies the absent → Create → Read sequence.
//
// Contract: After Apply(CreatePlan), Read must return Availability=Ok with
// a State that contains the desired fields at the expected values.
func (ct *ContractTester) runAbsentCreateRead(t TB) {
	t.Helper()
	ctx := context.Background()

	// AF-3 (S62dbba verify): regression guard for ADR-0002 "Read never looks at
	// desired".  Before any Apply, a Read of the sample ID must report the
	// resource as absent (nil State) — a provider that returns a populated State
	// here is peeking at desired (or at the Create plan) instead of observing
	// reality, which silently defeats the observation-state-less invariant.
	//
	// Best-effort: an error from this early Read is not itself a contract
	// violation (the target may be legitimately unreachable), so it is tolerated;
	// only a non-nil State on a successful Read is flagged.
	if ct.Preexisting {
		obs0, err0 := ct.Provider.Read(ctx, ct.SampleID)
		switch {
		case err0 != nil:
			t.Fatalf("ContractTester/AbsentCreateRead: Read of the preexisting sample before Apply failed: %v", err0)
			return
		case obs0.Availability != core.AvailabilityOk || obs0.State == nil:
			t.Fatalf("ContractTester/AbsentCreateRead: Read of the preexisting sample before Apply: availability=%v state=%v, want Ok with a State",
				obs0.Availability, obs0.State)
			return
		case ct.stateMatchesDesired(obs0.State):
			t.Errorf("ContractTester/AbsentCreateRead: Read before Apply already reports SampleDesired (provider may be peeking desired, ADR-0002; or the target was not set up to differ from the sample)")
		}
	} else if obs0, err0 := ct.Provider.Read(ctx, ct.SampleID); err0 == nil && obs0.State != nil {
		t.Errorf("ContractTester/AbsentCreateRead: Read before Apply returned non-nil State (provider may be peeking desired; ADR-0002)")
	}

	plan := ct.buildCreatePlan()

	// Apply Create.  This runs before runIdempotency (AF-3 ordering), so the
	// resource is created here for the first time.
	if err := ct.Provider.Apply(ctx, plan); err != nil {
		t.Fatalf("ContractTester/AbsentCreateRead: Apply failed: %v", err)
		return
	}

	// Read after Create.
	obs, err := ct.Provider.Read(ctx, ct.SampleID)
	if err != nil {
		t.Fatalf("ContractTester/AbsentCreateRead: Read after Create failed: %v", err)
		return
	}
	if obs.Availability != core.AvailabilityOk {
		t.Fatalf("ContractTester/AbsentCreateRead: Read availability = %v after Create, want Ok", obs.Availability)
		return
	}
	if obs.State == nil {
		t.Fatalf("ContractTester/AbsentCreateRead: Read returned nil State after Create")
		return
	}

	// Verify that all desired fields are present in the observed State.
	//
	// A minimal-tier provider (TrueObserve:false, §5.3) cannot return every
	// field — it may report little more than existence.  For it only the
	// Identity fields must be present; every other field is checked only when
	// the provider chose to return it.
	trueObserve := ct.Provider.Capabilities().TrueObserve
	schema := ct.Provider.Schema()
	pass := true
	for field, desiredVal := range ct.SampleDesired {
		tv, ok := obs.State[field]
		if !ok && !trueObserve && !schema.Fields[field].Identity {
			continue
		}
		if !ok {
			t.Errorf("ContractTester/AbsentCreateRead: field %q absent from observed State after Create", field)
			pass = false
			continue
		}
		observedStr := fmt.Sprintf("%v", tv.Value)
		if observedStr != desiredVal {
			t.Errorf("ContractTester/AbsentCreateRead: field %q: observed %q, desired %q", field, observedStr, desiredVal)
			pass = false
		}
	}
	if pass {
		t.Logf("ContractTester/AbsentCreateRead: PASS")
	}
}

// runListCompleteness verifies that every ID returned by List can be Read
// successfully with Availability=Ok.
//
// Contract: ∀ id ∈ List(scope).IDs → Read(id).Availability == Ok.
func (ct *ContractTester) runListCompleteness(t TB) {
	t.Helper()
	ctx := context.Background()

	result, err := ct.Lister.List(ctx, ct.Scope)
	if err != nil {
		t.Fatalf("ContractTester/ListCompleteness: List returned error: %v", err)
		return
	}
	if result.Status != core.ListComplete {
		t.Logf("ContractTester/ListCompleteness: List returned status %v (not ListComplete); skipping completeness check", result.Status)
		return
	}

	pass := true
	for _, id := range result.IDs {
		obs, readErr := ct.Provider.Read(ctx, id.Local)
		if readErr != nil {
			t.Errorf("ContractTester/ListCompleteness: Read(%q) returned error: %v", id.Local, readErr)
			pass = false
			continue
		}
		if obs.Availability != core.AvailabilityOk {
			t.Errorf("ContractTester/ListCompleteness: Read(%q) availability = %v, want Ok (List returned this ID)", id.Local, obs.Availability)
			pass = false
		}
	}
	if pass {
		t.Logf("ContractTester/ListCompleteness: PASS (%d IDs verified)", len(result.IDs))
	}
}

// runThreeValued verifies unknown ≠ absent (§4.5): with the target made
// unreachable, Read of the (existing) sample must not report it as absent.
//
// Contract: unreachable → Read returns an error or Availability Unknown; it
// never returns Availability Ok with a nil State.
func (ct *ContractTester) runThreeValued(t TB) {
	t.Helper()
	restore, err := ct.MakeUnreachable()
	if err != nil {
		t.Fatalf("ContractTester/ThreeValued: MakeUnreachable failed: %v", err)
		return
	}
	obs, readErr := ct.Provider.Read(context.Background(), ct.SampleID)
	if restore != nil {
		restore()
	}
	switch {
	case readErr != nil:
		t.Logf("ContractTester/ThreeValued: PASS (Read returned an error while unreachable)")
	case obs.Availability == core.AvailabilityUnknown:
		t.Logf("ContractTester/ThreeValued: PASS (Read returned Unknown while unreachable)")
	case obs.Availability == core.AvailabilityOk && obs.State == nil:
		t.Errorf("ContractTester/ThreeValued: unreachable target reported as absent (Availability Ok, nil State); unknown must not be conflated with absent (§4.5)")
	default:
		t.Errorf("ContractTester/ThreeValued: unreachable target reported Availability %v with State %v; want an error or Unknown", obs.Availability, obs.State)
	}
}

// runCheckContracts verifies a CheckReader (minimal tier, §5.6, §28.7):
//
//  1. Read without the desired is never Ok (it has nothing to ask the target).
//  2. Before Apply, the dry run of SampleDesired reports a change (the target
//     is set up to differ from the sample; with Preexisting as well).
//  3. Apply(CreatePlan) → the dry run reports no change (converged), and an
//     observation with Check never also carries a State.
//  4. Idempotency: a second Apply of the same plan → still no change.
//  5. With MakeUnreachable: the dry run returns an error or Unknown — never
//     Ok (a target that cannot answer is not "unchanged").
func (ct *ContractTester) runCheckContracts(t TB, cr provider.CheckReader) {
	t.Helper()
	ctx := context.Background()

	if obs, err := ct.Provider.Read(ctx, ct.SampleID); err == nil && obs.Availability == core.AvailabilityOk {
		t.Errorf("ContractTester/Check: Read without the desired returned Ok (State %v); a CheckReader can only answer against the desired", obs.State)
	}

	check := func(step string) *core.CheckResult {
		t.Helper()
		obs, err := cr.ReadCheck(ctx, ct.SampleID, ct.SampleDesired)
		if err != nil {
			t.Fatalf("ContractTester/Check: %s: ReadCheck failed: %v", step, err)
			return nil
		}
		if obs.Availability != core.AvailabilityOk || obs.Check == nil {
			t.Fatalf("ContractTester/Check: %s: ReadCheck availability=%v check=%v, want Ok with a check result", step, obs.Availability, obs.Check)
			return nil
		}
		if obs.State != nil {
			t.Errorf("ContractTester/Check: %s: ReadCheck returned a State as well as the check result", step)
		}
		if obs.ObservedAt.IsZero() {
			t.Errorf("ContractTester/Check: %s: ReadCheck returned a zero ObservedAt", step)
		}
		return obs.Check
	}

	if c := check("before Apply"); c == nil {
		return
	} else if !c.Changed {
		t.Errorf("ContractTester/Check: before Apply the dry run reports no change; set the target up to differ from the sample")
	}
	plan := ct.buildCreatePlan()
	for i, step := range []string{"after the first Apply", "after the second Apply (idempotency)"} {
		if err := ct.Provider.Apply(ctx, plan); err != nil {
			t.Fatalf("ContractTester/Check: Apply %d failed: %v", i+1, err)
			return
		}
		if c := check(step); c == nil {
			return
		} else if c.Changed {
			t.Errorf("ContractTester/Check: %s the dry run still reports a change", step)
		}
	}

	if ct.MakeUnreachable != nil {
		restore, err := ct.MakeUnreachable()
		if err != nil {
			t.Fatalf("ContractTester/Check: MakeUnreachable failed: %v", err)
			return
		}
		obs, readErr := cr.ReadCheck(ctx, ct.SampleID, ct.SampleDesired)
		if restore != nil {
			restore()
		}
		if readErr == nil && obs.Availability == core.AvailabilityOk {
			t.Errorf("ContractTester/Check: unreachable target reported Ok (check %v); want an error or Unknown (§4.5)", obs.Check)
		}
	}
	t.Logf("ContractTester/Check: PASS")
}

// buildCreatePlan constructs a Create Plan for SampleID using SampleDesired.
func (ct *ContractTester) buildCreatePlan() core.Plan {
	scope := ct.Scope
	// The provider name in the ID is informational for tests; the provider
	// local key (SampleID) is what matters for Apply dispatch.
	id := core.NewID("sdk-test", scope, ct.SampleID)

	fields := make([]core.FieldDiff, 0, len(ct.SampleDesired))
	for name, val := range ct.SampleDesired {
		fields = append(fields, core.FieldDiff{
			Field: name,
			From:  "",
			To:    val,
		})
	}

	return core.Plan{
		Ops: []core.Op{
			{
				Kind:        core.OpKindCreate,
				Target:      id,
				Destructive: false,
				Cause:       core.CauseTagA,
				Diagnostic:  "ContractTester: create test resource",
				Fields:      fields,
			},
		},
	}
}

// stateMatchesDesired reports whether every SampleDesired field is present in
// state with the same value.
func (ct *ContractTester) stateMatchesDesired(state core.State) bool {
	for field, want := range ct.SampleDesired {
		tv, ok := state[field]
		if !ok || fmt.Sprintf("%v", tv.Value) != want {
			return false
		}
	}
	return true
}

// statesEqual returns true if two State maps contain the same field keys and
// values (compared via their string representation).
func statesEqual(a, b core.State) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if fmt.Sprintf("%v", va.Value) != fmt.Sprintf("%v", vb.Value) {
			return false
		}
	}
	return true
}
