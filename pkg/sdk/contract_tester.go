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
//     and a State that matches the desired fields.
//  3. List completeness: Every ID returned by List can be Read successfully
//     with Availability=Ok.
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
}

// RunAll runs all contract tests against the provider using t for reporting.
// t must be a *testing.T (or any value satisfying TB).
//
// Tests run sequentially because they share provider state.
func (ct *ContractTester) RunAll(t TB) {
	t.Helper()

	ct.runIdempotency(t)
	ct.runAbsentCreateRead(t)

	if ct.Lister != nil {
		ct.runListCompleteness(t)
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

	plan := ct.buildCreatePlan()

	// Apply Create (may be a no-op if resource already exists from idempotency test).
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
	pass := true
	for field, desiredVal := range ct.SampleDesired {
		tv, ok := obs.State[field]
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
