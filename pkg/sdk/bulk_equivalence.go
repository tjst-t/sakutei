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

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// BulkEquivalenceTester verifies the BulkObserve consistency contract of a
// provider (§5.7, §28.3, ADR-0011):
//
//	BulkObserve(scope) ≡ { Read(id) | id ∈ scope }
//
// Passing it is the condition for a provider to call itself "complete tier +
// bulk".  It catches the class of bug where the bulk path and the per-resource
// path disagree about reality — e.g. the bulk API returns a stale or partial
// view — which would otherwise make convergence depend on which path the
// executor happened to take.
//
// Checks, in order:
//
//  1. Declaration consistency: Capabilities().BulkObserve is true if and only
//     if the provider implements provider.BulkObserver.  Either mismatch is a
//     violation (a silent lie in one direction; an undeclared, untested bulk
//     path in the other).  If the provider is not a BulkObserver the tester
//     stops here.
//  2. Target set: IDs if given; otherwise, when Lister is set, the IDs returned
//     by Lister.List(Scope).  The List result MUST be ListComplete — an
//     incomplete List cannot define "every id in scope".
//  3. One BulkObserve(Scope) call, then one Read per target id.  For every id:
//     - it is present in the bulk result (complete-or-error: a bulk call that
//     succeeds must not silently drop resources);
//     - the bulk Observation has ObservedAt set;
//     - Availability equals Read's; when both are Ok, State is equal.
//  4. When the targets came from List: the bulk result contains no id outside
//     that set (bulk must not invent resources that List cannot see).
//
// Reality must not change while the tester runs (it compares two observations
// of the same reality); run it against a quiescent fake or test fixture.
//
// Usage:
//
//	bt := sdk.BulkEquivalenceTester{Provider: p, Lister: p, Scope: core.NewScope(nil)}
//	bt.Run(t)
type BulkEquivalenceTester struct {
	// Provider is the provider under test.  Required.
	Provider provider.Provider

	// Lister supplies the target ids when IDs is empty.  Optional.
	Lister provider.Lister

	// Scope is passed to BulkObserve (and to List when Lister is used).
	Scope core.Scope

	// IDs are the resources to compare.  When empty, Lister must be set.
	IDs []core.ID
}

// Run executes the checks, reporting every violation through t.  Violations
// are reported with Errorf so a single run lists all disagreements; setup
// failures (BulkObserve/List/Read returning an error, no targets) use Fatalf.
func (bt *BulkEquivalenceTester) Run(t TB) {
	t.Helper()
	ctx := context.Background()

	if bt.Provider == nil {
		t.Fatalf("BulkEquivalenceTester: Provider is nil")
		return
	}

	// 1. Declaration consistency.
	declared := bt.Provider.Capabilities().BulkObserve
	bo, implements := bt.Provider.(provider.BulkObserver)
	switch {
	case declared && !implements:
		t.Errorf("BulkEquivalence: Capabilities.BulkObserve=true but provider does not implement provider.BulkObserver (§5.7)")
		return
	case !declared && implements:
		t.Errorf("BulkEquivalence: provider implements provider.BulkObserver but does not declare Capabilities.BulkObserve=true (undeclared bulk path, §5.7)")
	case !declared && !implements:
		t.Logf("BulkEquivalence: provider does not support BulkObserve; nothing to compare")
		return
	}

	// 2. Target set.
	targets := bt.IDs
	fromList := false
	if len(targets) == 0 {
		if bt.Lister == nil {
			t.Fatalf("BulkEquivalence: neither IDs nor Lister given; cannot determine the resources in scope")
			return
		}
		lr, err := bt.Lister.List(ctx, bt.Scope)
		if err != nil {
			t.Fatalf("BulkEquivalence: List(%s) error: %v", bt.Scope, err)
			return
		}
		if lr.Status != core.ListComplete {
			t.Fatalf("BulkEquivalence: List(%s) returned status %v, want ListComplete (an incomplete List cannot define the scope)", bt.Scope, lr.Status)
			return
		}
		targets = lr.IDs
		fromList = true
	}
	if len(targets) == 0 {
		t.Fatalf("BulkEquivalence: no resources in scope %s; seed at least one resource so the comparison means something", bt.Scope)
		return
	}

	// 3. One bulk call, then per-resource Reads.
	bulk, err := bo.BulkObserve(ctx, bt.Scope)
	if err != nil {
		t.Fatalf("BulkEquivalence: BulkObserve(%s) error: %v", bt.Scope, err)
		return
	}

	want := make(map[core.IDKey]bool, len(targets))
	for _, id := range targets {
		want[id.Key()] = true

		got, present := bulk.Observations[id.Key()]
		if !present {
			t.Errorf("BulkEquivalence: %s missing from BulkObserve result (complete-or-error: a successful bulk call must not drop resources)", id)
			continue
		}
		if got.ObservedAt.IsZero() {
			t.Errorf("BulkEquivalence: %s: bulk Observation has zero ObservedAt (every Observation must be stamped, §4.5)", id)
		}

		ref, err := bt.Provider.Read(ctx, id.Local)
		if err != nil {
			t.Fatalf("BulkEquivalence: Read(%s) error: %v", id.Local, err)
			return
		}
		if got.Availability != ref.Availability {
			t.Errorf("BulkEquivalence: %s: Availability bulk=%v read=%v", id, got.Availability, ref.Availability)
			continue
		}
		if ref.Availability == core.AvailabilityOk && !statesEqual(got.State, ref.State) {
			t.Errorf("BulkEquivalence: %s: State differs\n  bulk=%v\n  read=%v", id, got.State, ref.State)
		}
	}

	// 4. No invented resources (only meaningful when List defined the scope).
	if fromList {
		for key := range bulk.Observations {
			if !want[key] {
				t.Errorf("BulkEquivalence: BulkObserve returned %q, which List(%s) did not report", key, bt.Scope)
			}
		}
	}
}
