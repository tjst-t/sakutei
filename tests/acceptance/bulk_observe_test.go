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

// bulk_observe_test.go contains library-style acceptance tests for the
// BulkObserver interface seam, the transparent per-Read fallback, and the
// prune-non-reuse TYPE SEPARATION guard (Story Sf6a62f-3, §5.7, §16.3,
// ADR-0011).
//
// These tests import and exercise the exported symbols of pkg/provider,
// pkg/sdk, pkg/core, and internal/prune directly (no subprocess): the seam
// under test is a Go interface + a type-level guarantee, which is proven in
// the language, not through the CLI.
//
// AC tags:
//
//	[AC-Sf6a62f-3-1] provider.BulkObserver exists as a distinct optional
//	                 interface (Lister-style discovery); ValidateProvider
//	                 requires the interface when Capabilities.BulkObserve==true;
//	                 Observations carry ObservedAt (cycle-scoped).
//	[AC-Sf6a62f-3-2] BulkObservation cannot feed prune's set-difference
//	                 completeness input — the types are distinct and no path
//	                 converts one to the other (ADR-0011).
//	[AC-Sf6a62f-3-3] SDK ObserveScope falls back transparently to per-resource
//	                 Read for non-declaring providers; declaring and
//	                 non-declaring fakes yield the SAME Observation set.
package acceptance

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test fakes: two providers over the same reality, one implementing
// BulkObserver and one not.  Both expose identical Read behaviour so the
// fallback path and the bulk path can be proven equivalent.
// ─────────────────────────────────────────────────────────────────────────────

// bulkReality is the shared "reality" both fakes observe: local id → State.
type bulkReality map[string]core.State

var testObservedAt = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

func (r bulkReality) read(local string) core.Observation {
	st, ok := r[local]
	if !ok {
		// Absent from reality: reachable, no state.  (Availability Ok, empty State
		// is how the fakeprovider represents "not present".)  Kept simple for the
		// equivalence test — what matters is both fakes agree.
		return core.Observation{Availability: core.AvailabilityOk, ObservedAt: testObservedAt}
	}
	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        st,
		ObservedAt:   testObservedAt,
	}
}

// readOnlyFake implements provider.Provider WITHOUT BulkObserver.
type readOnlyFake struct{ reality bulkReality }

func (f *readOnlyFake) Schema() provider.Schema             { return provider.Schema{} }
func (f *readOnlyFake) Capabilities() provider.Capabilities { return provider.Capabilities{ProviderVersion: "v-test"} }
func (f *readOnlyFake) Read(_ context.Context, id string) (core.Observation, error) {
	return f.reality.read(id), nil
}
func (f *readOnlyFake) Apply(_ context.Context, _ core.Plan) error { return nil }

// bulkFake implements provider.Provider AND provider.BulkObserver.
type bulkFake struct{ reality bulkReality }

func (f *bulkFake) Schema() provider.Schema { return provider.Schema{} }
func (f *bulkFake) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v-test", BulkObserve: true}
}
func (f *bulkFake) Read(_ context.Context, id string) (core.Observation, error) {
	return f.reality.read(id), nil
}
func (f *bulkFake) Apply(_ context.Context, _ core.Plan) error { return nil }

// BulkObserve returns one Observation per known resource in a single call.
func (f *bulkFake) BulkObserve(_ context.Context, scope core.Scope) (provider.BulkObservation, error) {
	obs := make(map[core.IDKey]core.Observation, len(f.reality))
	for local := range f.reality {
		id := core.NewID("fake", scope, local)
		obs[id.Key()] = f.reality.read(local)
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// Compile-time assertions: bulkFake is a BulkObserver, readOnlyFake is not.
var _ provider.BulkObserver = (*bulkFake)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sf6a62f-3-1] BulkObserver is a distinct optional interface + validation.
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkObserver_InterfaceAndValidation(t *testing.T) {
	// [AC-Sf6a62f-3-1]

	// The interface is discovered by type assertion, exactly like Lister.
	var p provider.Provider = &bulkFake{reality: bulkReality{}}
	if _, ok := p.(provider.BulkObserver); !ok {
		t.Fatalf("[AC-Sf6a62f-3-1] bulkFake must satisfy provider.BulkObserver")
	}
	var ro provider.Provider = &readOnlyFake{reality: bulkReality{}}
	if _, ok := ro.(provider.BulkObserver); ok {
		t.Fatalf("[AC-Sf6a62f-3-1] readOnlyFake must NOT satisfy provider.BulkObserver")
	}

	// ValidateProvider accepts a provider that declares BulkObserve and
	// implements the interface.
	if err := provider.ValidateProvider(&bulkFake{reality: bulkReality{}}); err != nil {
		t.Fatalf("[AC-Sf6a62f-3-1] ValidateProvider rejected a valid BulkObserver: %v", err)
	}

	// A provider that declares BulkObserve==true but does NOT implement the
	// interface must be rejected (fail-closed: the flag would be a silent lie).
	if err := provider.ValidateProvider(&liarBulkProvider{}); err == nil {
		t.Fatalf("[AC-Sf6a62f-3-1] ValidateProvider must reject BulkObserve=true without BulkObserver")
	}

	// Observations are cycle-scoped: each Observation carries ObservedAt.
	bo := &bulkFake{reality: bulkReality{"a": core.State{}}}
	res, err := bo.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-Sf6a62f-3-1] BulkObserve error: %v", err)
	}
	for k, obs := range res.Observations {
		if obs.ObservedAt.IsZero() {
			t.Errorf("[AC-Sf6a62f-3-1] Observation for %v has zero ObservedAt (must be cycle-scoped)", k)
		}
	}
}

// liarBulkProvider declares BulkObserve==true but does NOT implement
// provider.BulkObserver.
type liarBulkProvider struct{}

func (l *liarBulkProvider) Schema() provider.Schema { return provider.Schema{} }
func (l *liarBulkProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ProviderVersion: "v-test", BulkObserve: true}
}
func (l *liarBulkProvider) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{}, nil
}
func (l *liarBulkProvider) Apply(_ context.Context, _ core.Plan) error { return nil }

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sf6a62f-3-3] Transparent fallback: bulk and per-Read paths are equivalent.
// ─────────────────────────────────────────────────────────────────────────────

func TestObserveScope_BulkAndFallbackEquivalent(t *testing.T) {
	// [AC-Sf6a62f-3-3]
	reality := bulkReality{
		"vm-1": core.State{"size": core.TypedValue{Value: "1"}},
		"vm-2": core.State{"size": core.TypedValue{Value: "2"}},
	}
	scope := core.NewScope(map[string]string{"site": "tokyo"})
	ids := []core.ID{
		core.NewID("fake", scope, "vm-1"),
		core.NewID("fake", scope, "vm-2"),
	}

	ctx := context.Background()

	// Bulk path (provider implements BulkObserver).
	bulkResult, err := sdk.ObserveScope(ctx, &bulkFake{reality: reality}, scope, ids)
	if err != nil {
		t.Fatalf("[AC-Sf6a62f-3-3] ObserveScope (bulk) error: %v", err)
	}

	// Fallback path (provider does NOT implement BulkObserver → per-Read).
	fallbackResult, err := sdk.ObserveScope(ctx, &readOnlyFake{reality: reality}, scope, ids)
	if err != nil {
		t.Fatalf("[AC-Sf6a62f-3-3] ObserveScope (fallback) error: %v", err)
	}

	if !reflect.DeepEqual(bulkResult, fallbackResult) {
		t.Fatalf("[AC-Sf6a62f-3-3] bulk and fallback paths must be behaviour-equivalent\n bulk=%#v\n fallback=%#v",
			bulkResult, fallbackResult)
	}
	if len(bulkResult) != len(ids) {
		t.Errorf("[AC-Sf6a62f-3-3] expected %d observations, got %d", len(ids), len(bulkResult))
	}
	for _, id := range ids {
		if _, ok := bulkResult[id.Key()]; !ok {
			t.Errorf("[AC-Sf6a62f-3-3] result missing observation for %v", id)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// [AC-Sf6a62f-3-2] Type separation: BulkObservation is NOT prune's completeness
// input (ADR-0011).
// ─────────────────────────────────────────────────────────────────────────────

func TestBulkObservation_NotPruneCompletenessInput(t *testing.T) {
	// [AC-Sf6a62f-3-2]
	//
	// Prune's set-difference completeness signal is core.ListResult, which is
	// produced ONLY by provider.Lister.List and carries an explicit ListStatus.
	// prune.Engine.ComputePrune's signature takes a provider.Lister (→ ListResult),
	// never a BulkObservation.  A degraded/partial bulk read must therefore be
	// structurally incapable of being read as "these resources are absent from
	// reality" — otherwise a partial bulk read would cause false orphan deletions.
	//
	// The following (correctly) DOES NOT COMPILE, proving the guard at the type
	// level.  Uncomment to confirm the compiler rejects it:
	//
	//	var bo provider.BulkObservation
	//	var _ core.ListResult = bo                       // cannot use bo (BulkObservation) as core.ListResult
	//	var _ core.ListResult = core.ListResult(bo)      // cannot convert bo to core.ListResult
	//	_ = prune.ComputePrune(ctx, bo, ...)             // BulkObservation is not a Lister
	//
	// We assert the same fact reflectively so the guarantee is machine-checked
	// and travels with the test suite.

	boType := reflect.TypeOf(provider.BulkObservation{})
	listResultType := reflect.TypeOf(core.ListResult{})

	// 1. The two types are distinct (not an alias / not identical).
	if boType == listResultType {
		t.Fatalf("[AC-Sf6a62f-3-2] BulkObservation and ListResult must be DISTINCT types (ADR-0011)")
	}

	// 2. BulkObservation is NOT convertible to ListResult (no lurking structural
	//    identity that would let a cast smuggle a bulk read into prune).
	if boType.ConvertibleTo(listResultType) {
		t.Errorf("[AC-Sf6a62f-3-2] BulkObservation must NOT be convertible to ListResult (ADR-0011 type separation)")
	}

	// 3. BulkObservation.Observations field type is NOT core.ListResult and NOT
	//    []core.ID — it carries no completeness assertion at all.
	obsField, ok := boType.FieldByName("Observations")
	if !ok {
		t.Fatalf("[AC-Sf6a62f-3-2] BulkObservation.Observations field missing")
	}
	if obsField.Type == listResultType {
		t.Errorf("[AC-Sf6a62f-3-2] Observations must not be a ListResult")
	}
	if obsField.Type == reflect.TypeOf([]core.ID(nil)) {
		t.Errorf("[AC-Sf6a62f-3-2] Observations must not be a []core.ID (the Lister completeness set)")
	}

	// 4. prune.ComputePrune's completeness input is a provider.Lister (→
	//    ListResult), and BulkObservation does NOT implement provider.Lister, so
	//    there is no way to pass a bulk observation where prune expects
	//    completeness.  Verify BulkObservation is not assignable to Lister.
	listerType := reflect.TypeOf((*provider.Lister)(nil)).Elem()
	if reflect.PtrTo(boType).Implements(listerType) || boType.Implements(listerType) {
		t.Errorf("[AC-Sf6a62f-3-2] BulkObservation must NOT implement provider.Lister (it cannot be prune's completeness source)")
	}

	// Reference prune so the dependency (and thus the ComputePrune signature this
	// guard reasons about) is real and compiled into the test binary.
	_ = prune.New()
}
