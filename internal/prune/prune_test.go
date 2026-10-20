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

// Package prune_test provides unit tests for the prune engine (§12, ADR-0005,
// Scf7762-1).
//
// These tests operate on the Engine directly (not the CLI binary) to validate
// internal safety guards that are difficult to exercise through CLI subprocess
// tests — specifically the List count-drop halt (§12.4b).
package prune_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// fakeCompleteList is a mock Lister that always returns ListComplete.
type fakeCompleteList struct {
	ids []core.ID
}

func (f *fakeCompleteList) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	return core.ListResult{IDs: f.ids, Status: core.ListComplete}, nil
}

// fakePartialList is a mock Lister that always returns ListPartial.
type fakePartialList struct {
	ids []core.ID
}

func (f *fakePartialList) List(_ context.Context, _ core.Scope) (core.ListResult, error) {
	return core.ListResult{IDs: f.ids, Status: core.ListPartial}, nil
}

// fakeObserver is a minimal provider.Provider that reports all resources as
// AvailabilityOk with an empty state (sufficient for prune candidate observation).
//
// pruneable controls the TYPE-level Capabilities().Pruneable the observer
// reports (Scf7762-1): the prune engine consults it to decide whether an orphan
// of this type may be deleted.  Default (zero value) false matches ADR-0005.
type fakeObserver struct {
	pruneable bool
}

func (f *fakeObserver) Schema() provider.Schema { return provider.Schema{} }
func (f *fakeObserver) Capabilities() provider.Capabilities {
	return provider.Capabilities{List: true, Pruneable: f.pruneable}
}
func (f *fakeObserver) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        core.State{},
		ObservedAt:   time.Now(),
	}, nil
}
func (f *fakeObserver) Apply(_ context.Context, _ core.Plan) error { return nil }

// pruneableObserver returns a fakeObserver whose TYPE is pruneable (opt-in).
func pruneableObserver() *fakeObserver { return &fakeObserver{pruneable: true} }

// makeID constructs a simple ID for test use.
func makeID(local string) core.ID {
	return core.NewID("fake", core.NewScope(nil), local)
}

// pruneDesired builds a minimal DesiredResource slice for prune tests.
//
// Scf7762-1: pruneability is now a TYPE-level provider capability, not a
// per-resource intent flag, so DesiredResource no longer carries a Pruneable
// field.  Tests opt the type in via pruneableObserver().
func pruneDesired(locals ...string) []diff.DesiredResource {
	res := make([]diff.DesiredResource, len(locals))
	for i, local := range locals {
		res[i] = diff.DesiredResource{
			ID:             makeID(local),
			DeletionPolicy: core.PruneWithinScope,
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// §12.4c: ListStatus gate
// ---------------------------------------------------------------------------

// TestPruneEngine_ListPartial_Halts verifies that a ListPartial result causes
// the engine to halt prune (§12.4c, [AC-Scf7762-1-2]).
func TestPruneEngine_ListPartial_Halts(t *testing.T) {
	engine := prune.New()
	lister := &fakePartialList{
		ids: []core.ID{makeID("orphan-a"), makeID("orphan-b")},
	}

	result, err := engine.ComputePrune(
		context.Background(),
		lister,
		&fakeObserver{},
		core.NewScope(nil),
		core.NewScope(nil),
		pruneDesired("desired-only"),
		nil,
		&bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("ComputePrune: unexpected error: %v", err)
	}
	if !result.Halted {
		t.Errorf("expected Halted=true when ListPartial; got Halted=false (DeleteOps=%v)", result.DeleteOps)
	}
	if len(result.DeleteOps) != 0 {
		t.Errorf("expected no DeleteOps when halted; got %d ops", len(result.DeleteOps))
	}
}

// ---------------------------------------------------------------------------
// §12.4b: List count-drop halt
// ---------------------------------------------------------------------------

// TestPruneEngine_CountDrop_Halts verifies that a >50% drop in List count
// between cycles halts prune (§12.4b).
//
// Cycle 1: List returns 10 IDs → baseline = 10.
// Cycle 2: List returns 4 IDs (60% drop > 50% threshold) → halt.
func TestPruneEngine_CountDrop_Halts(t *testing.T) {
	engine := prune.New() // default threshold = 50%

	// Cycle 1: establish baseline.
	cycle1IDs := make([]core.ID, 10)
	for i := range cycle1IDs {
		cycle1IDs[i] = makeID("vm-" + string(rune('a'+i)))
	}
	lister1 := &fakeCompleteList{ids: cycle1IDs}

	desired1 := pruneDesired("vm-a") // keep vm-a; rest are orphans
	_, err := engine.ComputePrune(context.Background(), lister1, &fakeObserver{}, core.NewScope(nil), core.NewScope(nil), desired1, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("cycle 1 ComputePrune error: %v", err)
	}

	// Cycle 2: List returns only 4 IDs (60% drop).
	cycle2IDs := cycle1IDs[:4]
	lister2 := &fakeCompleteList{ids: cycle2IDs}
	desired2 := pruneDesired("vm-a")
	buf := &bytes.Buffer{}
	result2, err := engine.ComputePrune(context.Background(), lister2, &fakeObserver{}, core.NewScope(nil), core.NewScope(nil), desired2, nil, buf)
	if err != nil {
		t.Fatalf("cycle 2 ComputePrune error: %v", err)
	}
	if !result2.Halted {
		t.Errorf("expected Halted=true on >50%% count drop (10→4); got Halted=false, DeleteOps=%v", result2.DeleteOps)
	}
	if len(result2.DeleteOps) != 0 {
		t.Errorf("expected no DeleteOps when halted; got %d ops", len(result2.DeleteOps))
	}
	t.Logf("halt log: %s", buf.String())
}

// TestPruneEngine_CountDrop_BelowThreshold_Proceeds verifies that a count drop
// below the threshold does NOT halt prune.
//
// Cycle 1: 10 IDs.
// Cycle 2: 8 IDs (20% drop < 50% threshold) → prune proceeds.
func TestPruneEngine_CountDrop_BelowThreshold_Proceeds(t *testing.T) {
	engine := prune.New()

	// Cycle 1.
	cycle1IDs := make([]core.ID, 10)
	for i := range cycle1IDs {
		cycle1IDs[i] = makeID("vm-" + string(rune('a'+i)))
	}
	lister1 := &fakeCompleteList{ids: cycle1IDs}
	desired1 := pruneDesired("vm-a")
	_, err := engine.ComputePrune(context.Background(), lister1, &fakeObserver{}, core.NewScope(nil), core.NewScope(nil), desired1, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("cycle 1 error: %v", err)
	}

	// Cycle 2: 8 IDs (20% drop).
	cycle2IDs := cycle1IDs[:8]
	lister2 := &fakeCompleteList{ids: cycle2IDs}
	desired2 := pruneDesired("vm-a")
	result2, err := engine.ComputePrune(context.Background(), lister2, &fakeObserver{}, core.NewScope(nil), core.NewScope(nil), desired2, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("cycle 2 error: %v", err)
	}
	if result2.Halted {
		t.Errorf("expected prune to proceed (20%% drop < 50%% threshold); got Halted=true")
	}
}

// ---------------------------------------------------------------------------
// §12.3: pruneable:false default
// ---------------------------------------------------------------------------

// TestPruneEngine_PruneableDefaultFalse verifies that orphans of a
// non-pruneable TYPE (the default) are deferred, not deleted ([AC-Scf7762-1-1]).
//
// Scf7762-1: pruneability is a TYPE-level provider capability.  Here the
// observer reports Capabilities().Pruneable == false (default), so the orphan
// is deferred even though the scope is PruneWithinScope.
func TestPruneEngine_PruneableDefaultFalse(t *testing.T) {
	engine := prune.New()

	// Reality has orphan-vm.
	lister := &fakeCompleteList{ids: []core.ID{makeID("desired-vm"), makeID("orphan-vm")}}

	// Desired: desired-vm with PruneWithinScope.  The provider TYPE is NOT
	// pruneable (default false observer), so nothing is deleted.
	desired := []diff.DesiredResource{
		{
			ID:             makeID("desired-vm"),
			DeletionPolicy: core.PruneWithinScope,
		},
	}

	result, err := engine.ComputePrune(context.Background(), lister, &fakeObserver{}, core.NewScope(nil), core.NewScope(nil), desired, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ComputePrune error: %v", err)
	}
	if result.Halted {
		t.Errorf("expected Halted=false; got Halted=true: %s", result.HaltReason)
	}
	if len(result.DeleteOps) != 0 {
		t.Errorf("expected no DeleteOps (pruneable:false); got %d: %v", len(result.DeleteOps), result.DeleteOps)
	}
	if len(result.Deferred) == 0 {
		t.Errorf("expected orphan-vm to appear in Deferred (pruneable:false); got empty Deferred")
	}
}

// ---------------------------------------------------------------------------
// unknown ≠ absent (§4, ADR-0002)
// ---------------------------------------------------------------------------

// TestPruneEngine_UnknownNotAbsent verifies that resources whose Observe
// returned non-Ok are excluded from the deletion candidate set ([AC-Scf7762-1-3]).
func TestPruneEngine_UnknownNotAbsent(t *testing.T) {
	engine := prune.New()

	// Reality: desired-vm + unreachable-vm.
	lister := &fakeCompleteList{ids: []core.ID{makeID("desired-vm"), makeID("unreachable-vm")}}

	// "unreachable-vm" is unobservable this cycle.
	unobservable := map[string]bool{"unreachable-vm": true}

	// Desired: only desired-vm with PruneWithinScope + pruneable:true.
	desired := pruneDesired("desired-vm")

	result, err := engine.ComputePrune(context.Background(), lister, pruneableObserver(), core.NewScope(nil), core.NewScope(nil), desired, unobservable, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ComputePrune error: %v", err)
	}
	if result.Halted {
		t.Errorf("unexpected halt: %s", result.HaltReason)
	}
	for _, op := range result.DeleteOps {
		if op.Target.Local == "unreachable-vm" {
			t.Errorf("unreachable-vm must not be in DeleteOps (unknown ≠ absent); ops=%v", result.DeleteOps)
		}
	}
	// unreachable-vm should appear in Deferred.
	found := false
	for _, id := range result.Deferred {
		if id.Local == "unreachable-vm" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("unreachable-vm not found in Deferred; expected it to be deferred (unknown ≠ absent)")
	}
}

// ---------------------------------------------------------------------------
// Normal prune path
// ---------------------------------------------------------------------------

// TestPruneEngine_NormalDelete verifies that a resource absent from desired
// with PruneWithinScope + pruneable:true is approved for deletion.
func TestPruneEngine_NormalDelete(t *testing.T) {
	engine := prune.New()

	lister := &fakeCompleteList{ids: []core.ID{makeID("desired-vm"), makeID("orphan-vm")}}
	desired := pruneDesired("desired-vm") // orphan-vm absent from desired

	// Type is pruneable (opt-in), so orphan-vm is approved for deletion.
	result, err := engine.ComputePrune(context.Background(), lister, pruneableObserver(), core.NewScope(nil), core.NewScope(nil), desired, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ComputePrune error: %v", err)
	}
	if result.Halted {
		t.Errorf("unexpected halt: %s", result.HaltReason)
	}
	if len(result.DeleteOps) != 1 {
		t.Errorf("expected 1 DeleteOp for orphan-vm; got %d: %v", len(result.DeleteOps), result.DeleteOps)
	}
	if result.DeleteOps[0].Target.Local != "orphan-vm" {
		t.Errorf("expected DeleteOp for orphan-vm; got %s", result.DeleteOps[0].Target.Local)
	}
	if result.DeleteOps[0].Kind != core.OpKindDelete {
		t.Errorf("expected OpKindDelete; got %v", result.DeleteOps[0].Kind)
	}
	if !result.DeleteOps[0].Destructive {
		t.Errorf("expected Destructive=true for Delete op")
	}
}

// ---------------------------------------------------------------------------
// Mixed-type safety (DD-1): a non-pruneable TYPE's orphan is never deleted,
// even when a pruneable TYPE shares the scope.
// ---------------------------------------------------------------------------

// TestPruneEngine_MixedType_NonPruneableTypeNotDeleted verifies the type-level
// safety guarantee introduced by Scf7762-1 (DD-1).
//
// The OLD (buggy) engine used a scope-level "any pruneable:true resource in the
// scope permits pruning" gate, so an orphan of a pruneable:false type could be
// deleted merely because another pruneable:true resource shared the scope.  The
// new engine gates on the orphan's own provider TYPE (Capabilities().Pruneable),
// so a non-pruneable type is protected regardless of what else is in the scope.
//
// We model "two types share the scope" by running two prune passes over the
// SAME reality (same List) with two providers: one pruneable, one not.  The
// non-pruneable provider's pass must delete nothing (all orphans deferred); the
// pruneable provider's pass may delete.  The critical assertion is the first:
// the non-pruneable pass never emits a Delete even though pruneable orphans
// coexist in the very same reality set.
func TestPruneEngine_MixedType_NonPruneableTypeNotDeleted(t *testing.T) {
	// Reality contains orphans of both a "vm" type and a "record" type, none of
	// which are in desired.  We represent both in a single List (same scope).
	realityIDs := []core.ID{
		makeID("desired-vm"),
		makeID("vm-orphan"),     // would belong to the non-pruneable type
		makeID("record-orphan"), // would belong to the pruneable type
	}

	desired := pruneDesired("desired-vm") // PruneWithinScope; both orphans absent

	// Pass A: the NON-pruneable provider type.  Its Capabilities().Pruneable is
	// false, so NO orphan may be deleted — not vm-orphan, and crucially not
	// record-orphan either, even though a pruneable type also has orphans here.
	engineA := prune.New()
	listerA := &fakeCompleteList{ids: realityIDs}
	resultA, err := engineA.ComputePrune(
		context.Background(), listerA, &fakeObserver{ /* pruneable:false */ },
		core.NewScope(nil), core.NewScope(nil), desired, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("pass A ComputePrune error: %v", err)
	}
	if resultA.Halted {
		t.Fatalf("pass A unexpectedly halted: %s", resultA.HaltReason)
	}
	if len(resultA.DeleteOps) != 0 {
		t.Errorf("mixed-type safety violated: non-pruneable TYPE must delete nothing; got %d DeleteOps: %v",
			len(resultA.DeleteOps), resultA.DeleteOps)
	}
	// Both orphans must be reported as deferred (reported, not deleted).
	if len(resultA.Deferred) != 2 {
		t.Errorf("expected both orphans deferred under non-pruneable type; got Deferred=%v", resultA.Deferred)
	}

	// Pass B: the pruneable provider type over the SAME reality.  This confirms
	// the scope genuinely contains deletable orphans, so pass A's zero-deletion
	// result is due to the type gate, not an empty candidate set.
	engineB := prune.New()
	listerB := &fakeCompleteList{ids: realityIDs}
	resultB, err := engineB.ComputePrune(
		context.Background(), listerB, pruneableObserver(),
		core.NewScope(nil), core.NewScope(nil), desired, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("pass B ComputePrune error: %v", err)
	}
	if len(resultB.DeleteOps) != 2 {
		t.Errorf("pass B (pruneable type) should delete both orphans; got %d: %v",
			len(resultB.DeleteOps), resultB.DeleteOps)
	}
}

// TestPruneEngine_OnlyWithinDeclaredScope: reality in a scope where nothing
// declares PruneWithinScope is open-world and never a candidate (#38, §12.2).
func TestPruneEngine_OnlyWithinDeclaredScope(t *testing.T) {
	tokyo := core.NewScope(map[string]string{"site": "tokyo"})
	legacy := core.NewScope(map[string]string{"site": "legacy"})
	lister := &fakeCompleteList{ids: []core.ID{
		core.NewID("fake", tokyo, "web-1"),
		core.NewID("fake", tokyo, "orphan-t"),
		core.NewID("fake", legacy, "old-1"),
	}}
	desired := []diff.DesiredResource{{
		ID:             core.NewID("fake", tokyo, "web-1"),
		DeletionPolicy: core.PruneWithinScope,
	}}

	result, err := prune.New().ComputePrune(context.Background(), lister, pruneableObserver(),
		core.NewScope(nil), core.NewScope(nil), desired, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DeleteOps) != 1 || result.DeleteOps[0].Target.Local != "orphan-t" {
		t.Fatalf("want only orphan-t deleted, got %v", result.DeleteOps)
	}
}

// TestPruneEngine_CountDrop_PlannedRemovalNotCounted verifies §13.3: removals
// noted as planned_change lane deletes are not a sudden drop, while an extra,
// undeclared shrink on top of them still halts.
func TestPruneEngine_CountDrop_PlannedRemovalNotCounted(t *testing.T) {
	ids := make([]core.ID, 10)
	for i := range ids {
		ids[i] = makeID("vm-" + string(rune('a'+i)))
	}
	run := func(engine *prune.Engine, list []core.ID) prune.PruneResult {
		t.Helper()
		res, err := engine.ComputePrune(context.Background(), &fakeCompleteList{ids: list}, &fakeObserver{},
			core.NewScope(nil), core.NewScope(nil), pruneDesired("vm-a"), nil, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// 10 → 3 with the 7 vanished IDs declared: not halted.
	engine := prune.New()
	run(engine, ids)
	for _, id := range ids[3:] {
		engine.NotePlannedRemoval(id)
	}
	if res := run(engine, ids[:3]); res.Halted {
		t.Fatalf("declared removals must not trip the drop gate: %s", res.HaltReason)
	}

	// 10 → 2 with only 4 declared: 6 → 2 is still a 67% drop → halt.
	engine = prune.New()
	run(engine, ids)
	for _, id := range ids[6:] {
		engine.NotePlannedRemoval(id)
	}
	if res := run(engine, ids[:2]); !res.Halted {
		t.Fatalf("an undeclared shrink on top of the declared one must still halt")
	}
}
