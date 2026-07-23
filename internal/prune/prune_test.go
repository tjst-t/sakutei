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
type fakeObserver struct{}

func (f *fakeObserver) Schema() provider.Schema           { return provider.Schema{} }
func (f *fakeObserver) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (f *fakeObserver) Read(_ context.Context, _ string) (core.Observation, error) {
	return core.Observation{
		Availability: core.AvailabilityOk,
		State:        core.State{},
		ObservedAt:   time.Now(),
	}, nil
}
func (f *fakeObserver) Apply(_ context.Context, _ core.Plan) error { return nil }

// makeID constructs a simple ID for test use.
func makeID(local string) core.ID {
	return core.NewID("fake", core.NewScope(nil), local)
}

// pruneDesired builds a minimal DesiredResource slice for prune tests.
func pruneDesired(locals ...string) []diff.DesiredResource {
	res := make([]diff.DesiredResource, len(locals))
	for i, local := range locals {
		res[i] = diff.DesiredResource{
			ID:             makeID(local),
			DeletionPolicy: core.PruneWithinScope,
			Pruneable:      true,
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
	_, err := engine.ComputePrune(context.Background(), lister1, &fakeObserver{}, core.NewScope(nil), desired1, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("cycle 1 ComputePrune error: %v", err)
	}

	// Cycle 2: List returns only 4 IDs (60% drop).
	cycle2IDs := cycle1IDs[:4]
	lister2 := &fakeCompleteList{ids: cycle2IDs}
	desired2 := pruneDesired("vm-a")
	buf := &bytes.Buffer{}
	result2, err := engine.ComputePrune(context.Background(), lister2, &fakeObserver{}, core.NewScope(nil), desired2, nil, buf)
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
	_, err := engine.ComputePrune(context.Background(), lister1, &fakeObserver{}, core.NewScope(nil), desired1, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("cycle 1 error: %v", err)
	}

	// Cycle 2: 8 IDs (20% drop).
	cycle2IDs := cycle1IDs[:8]
	lister2 := &fakeCompleteList{ids: cycle2IDs}
	desired2 := pruneDesired("vm-a")
	result2, err := engine.ComputePrune(context.Background(), lister2, &fakeObserver{}, core.NewScope(nil), desired2, nil, &bytes.Buffer{})
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

// TestPruneEngine_PruneableDefaultFalse verifies that resources absent from
// desired with pruneable:false (the default) are deferred, not deleted
// ([AC-Scf7762-1-1]).
func TestPruneEngine_PruneableDefaultFalse(t *testing.T) {
	engine := prune.New()

	// Reality has orphan-vm.
	lister := &fakeCompleteList{ids: []core.ID{makeID("desired-vm"), makeID("orphan-vm")}}

	// Desired: desired-vm with PruneWithinScope but pruneable:false.
	desired := []diff.DesiredResource{
		{
			ID:             makeID("desired-vm"),
			DeletionPolicy: core.PruneWithinScope,
			Pruneable:      false, // explicit false (also the default)
		},
	}

	result, err := engine.ComputePrune(context.Background(), lister, &fakeObserver{}, core.NewScope(nil), desired, nil, &bytes.Buffer{})
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

	result, err := engine.ComputePrune(context.Background(), lister, &fakeObserver{}, core.NewScope(nil), desired, unobservable, &bytes.Buffer{})
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

	result, err := engine.ComputePrune(context.Background(), lister, &fakeObserver{}, core.NewScope(nil), desired, nil, &bytes.Buffer{})
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
