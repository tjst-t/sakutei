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

// inventory_test.go — Backlog #31 (v1b): inventory validation — a sudden
// drop or loss of render phase 1's inventory is incomplete, not empty (§9.2).
//
// Tags:
//   [#31-1] InventoryGuard: a drop below 50% (or below the expected
//           cardinality, or to zero) is incomplete; the baseline does not move
//           while halted; unreachable keeps the previous inventory; the
//           inventory hash does not depend on item order
//   [#31-2] chaos: dropping the inventory below half under the daemon runs
//           no prune —
//           the cycle halts with an InventoryIncomplete Event and reality is
//           intact; restoring the inventory resumes convergence
//   [#31-3] control: a gradual shrink above the threshold is accepted and
//           prunes, so [#31-2]'s survival is the guard's doing
//   [#47-1] boundary: a drop to exactly half is accepted by the inventory
//           guard and does not halt the prune List count-drop gate — both
//           follow §9.2's "50% 未満で halt"

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// stubInventory is an InventorySource whose content the test sets per call.
type stubInventory struct {
	items []diff.DesiredResource
	err   error
}

func (s *stubInventory) Fetch(context.Context) ([]diff.DesiredResource, error) {
	return s.items, s.err
}

func inventoryItems(n int) []diff.DesiredResource {
	out := make([]diff.DesiredResource, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("inv-vm-%02d", i)
		out = append(out, diff.DesiredResource{
			ID:             core.NewID("fake", core.NewScope(nil), id),
			Fields:         map[string]string{"name": id},
			DeletionPolicy: core.PruneWithinScope,
		})
	}
	return out
}

func TestInventoryGuard_IncompleteNotEmpty(t *testing.T) {
	ctx := context.Background()
	src := &stubInventory{items: inventoryItems(10)}
	g := render.NewInventoryGuard(src)

	first := g.Compose(ctx)
	if first.Halt || len(first.Resources) != 10 || first.Input == "" {
		t.Fatalf("[#31-1] first fetch must be accepted: %+v", first)
	}

	// Below half → incomplete.
	src.items = inventoryItems(4)
	low := g.Compose(ctx)
	if !low.Halt || !low.Incomplete || low.Resources != nil {
		t.Fatalf("[#31-1] a drop below half must halt as incomplete: %+v", low)
	}
	if !strings.Contains(low.Reason, "from 10 to 4") {
		t.Errorf("[#31-1] reason should name the drop: %q", low.Reason)
	}
	// The baseline does not move while halted: 4 again still halts.
	if r := g.Compose(ctx); !r.Halt {
		t.Fatalf("[#31-1] the baseline moved to the incomplete inventory: %+v", r)
	}
	// Empty after non-empty is incomplete, never "delete everything".
	src.items = nil
	if r := g.Compose(ctx); !r.Halt || !r.Incomplete {
		t.Fatalf("[#31-1] an empty inventory after 10 must halt: %+v", r)
	}

	// Recovery, then a shrink above the threshold is accepted (10 → 6).
	src.items = inventoryItems(10)
	if r := g.Compose(ctx); r.Halt {
		t.Fatalf("[#31-1] recovered inventory must be accepted: %+v", r)
	}
	src.items = inventoryItems(6)
	if r := g.Compose(ctx); r.Halt || len(r.Resources) != 6 {
		t.Fatalf("[#31-1] 10 → 6 is above the threshold and must be accepted: %+v", r)
	}

	// Unreachable → the previous (6) inventory, not a halt.
	src.err = errors.New("cmdb down")
	r := g.Compose(ctx)
	if r.Halt || !r.UnreachableFallback || len(r.Resources) != 6 {
		t.Fatalf("[#31-1] unreachable must keep the previous inventory: %+v", r)
	}
}

func TestInventoryGuard_FirstCallAndExpectedMin(t *testing.T) {
	ctx := context.Background()

	src := &stubInventory{err: errors.New("cmdb down")}
	if r := render.NewInventoryGuard(src).Compose(ctx); !r.Halt || r.Incomplete {
		t.Fatalf("[#31-1] unreachable on the first call must halt (not incomplete): %+v", r)
	}

	// Without a baseline only the expected cardinality can catch a short inventory.
	src = &stubInventory{items: inventoryItems(3)}
	if r := render.NewInventoryGuard(src, render.WithInventoryExpectedMin(8)).Compose(ctx); !r.Halt || !r.Incomplete {
		t.Fatalf("[#31-1] 3 < expected 8 must halt as incomplete: %+v", r)
	}
	src.items = inventoryItems(8)
	if r := render.NewInventoryGuard(src, render.WithInventoryExpectedMin(8)).Compose(ctx); r.Halt {
		t.Fatalf("[#31-1] 8 >= expected 8 must be accepted: %+v", r)
	}
}

func TestInventoryGuard_HashIgnoresOrder(t *testing.T) {
	ctx := context.Background()
	items := inventoryItems(4)
	rev := []diff.DesiredResource{items[3], items[1], items[2], items[0]}
	a := render.NewInventoryGuard(&stubInventory{items: items}).Compose(ctx)
	b := render.NewInventoryGuard(&stubInventory{items: rev}).Compose(ctx)
	if a.Input != b.Input {
		t.Errorf("[#31-1] inventory hash depends on item order: %q vs %q", a.Input, b.Input)
	}
	items[0].Fields = map[string]string{"name": "changed"}
	c := render.NewInventoryGuard(&stubInventory{items: items}).Compose(ctx)
	if c.Input == a.Input {
		t.Errorf("[#31-1] inventory hash did not change with the content")
	}
}

// ---------------------------------------------------------------------------
// chaos (daemon, real binary)
// ---------------------------------------------------------------------------

// writeInventoryFile writes an intent-format inventory with n PruneWithinScope
// resources inv-vm-01..n, atomically (the daemon may read it at any time).
func writeInventoryFile(t *testing.T, path string, n int) {
	t.Helper()
	resources := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("inv-vm-%02d", i)
		resources = append(resources, map[string]any{
			"provider":        "fake",
			"id":              id,
			"deletion_policy": "PruneWithinScope",
			"fields":          map[string]string{"name": id},
		})
	}
	data, err := json.MarshalIndent(map[string]any{"resources": resources}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// setupInventoryChaos prepares an empty intent dir, an inventory of 10 and a
// reality that already matches it with the type opted in to pruning — so any
// inventory item that disappears is a live prune candidate.
func setupInventoryChaos(t *testing.T) (intentDir, statePath, invPath, logPath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	invPath = filepath.Join(tmp, "inventory.json")
	logPath = filepath.Join(tmp, "daemon.log")

	writeInventoryFile(t, invPath, 10)
	reality := make(map[string]map[string]any, 10)
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("inv-vm-%02d", i)
		reality[id] = map[string]any{"name": id}
	}
	seedStateWithResources(t, statePath, reality)
	seedStatePruneableCapability(t, statePath)
	return intentDir, statePath, invPath, logPath
}

func TestChaos_InventoryBelowHalf_HaltsWithoutPrune(t *testing.T) {
	intentDir, statePath, invPath, logPath := setupInventoryChaos(t)

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "200ms",
		"--inventory-file", invPath)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()

	// Cycle 1 accepts the inventory of 10 as the baseline (reality matches).
	if !waitForLogFile(logPath, "cycle 1: observed=10 converged", 5*time.Second) {
		t.Fatalf("[#31-2] cycle 1 did not converge on the inventory of 10; log:\n%s", readFileString(logPath))
	}

	// Chaos: the CMDB suddenly returns less than half (200 OK, shrunk content).
	writeInventoryFile(t, invPath, 4)
	if !waitForLogFile(logPath, "InventoryIncomplete: inventory dropped from 10 to 4", 5*time.Second) {
		t.Fatalf("[#31-2] no InventoryIncomplete Event after the inventory dropped below half; log:\n%s", readFileString(logPath))
	}
	// Let several more cycles run on the shrunk inventory: the halt must hold.
	start := strings.Count(readFileString(logPath), "INVENTORY HALTED")
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(readFileString(logPath), "INVENTORY HALTED") < start+3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// PROOF (reality): all 10 resources survive — nothing was pruned.
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("inv-vm-%02d", i)
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[#31-2] %s was deleted although the inventory was only incomplete; log:\n%s", id, readFileString(logPath))
		}
	}
	logStr := readFileString(logPath)
	if strings.Count(logStr, "INVENTORY HALTED") < start+3 {
		t.Errorf("[#31-2] the halt did not hold over several cycles; log:\n%s", logStr)
	}
	if strings.Contains(logStr, "pruned=") {
		t.Errorf("[#31-2] prune ran on the incomplete inventory; log:\n%s", logStr)
	}

	// Recovery: the full inventory comes back and the daemon converges again.
	writeInventoryFile(t, invPath, 10)
	deadline = time.Now().Add(5 * time.Second)
	for strings.Count(readFileString(logPath), "observed=10 converged") < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Count(readFileString(logPath), "observed=10 converged") < 2 {
		t.Errorf("[#31-2] the daemon did not resume after the inventory recovered; log:\n%s", readFileString(logPath))
	}

	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true
	if n := countResourcesInState(t, statePath); n != 10 {
		t.Errorf("[#31-2] reality has %d resources, want 10", n)
	}
}

func TestChaos_InventoryGradualShrink_Prunes(t *testing.T) {
	intentDir, statePath, invPath, logPath := setupInventoryChaos(t)

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "200ms",
		"--inventory-file", invPath)
	defer func() {
		_ = cmd.Process.Kill()
		_ = waitCmd()
	}()

	if !waitForLogFile(logPath, "cycle 1: observed=10 converged", 5*time.Second) {
		t.Fatalf("[#31-3] cycle 1 did not converge; log:\n%s", readFileString(logPath))
	}
	// 10 → 8 is a legitimate decommission (above 50%): accepted and pruned.
	writeInventoryFile(t, invPath, 8)
	if !waitForLogFile(logPath, "pruned=2", 5*time.Second) {
		t.Fatalf("[#31-3] a shrink above the threshold must prune the 2 removed items; log:\n%s", readFileString(logPath))
	}
	for _, id := range []string{"inv-vm-09", "inv-vm-10"} {
		if resourceExistsInState(t, statePath, id) {
			t.Errorf("[#31-3] %s should have been pruned", id)
		}
	}
	if strings.Contains(readFileString(logPath), "InventoryIncomplete") {
		t.Errorf("[#31-3] a shrink above the threshold must not be reported as incomplete")
	}
}

// boundaryLister is a provider.Lister whose complete List the test sets per call.
type boundaryLister struct{ ids []core.ID }

func (l *boundaryLister) List(context.Context, core.Scope) (core.ListResult, error) {
	return core.ListResult{IDs: l.ids, Status: core.ListComplete}, nil
}

// boundaryObserver reports every resource as observable (ok, empty state).
type boundaryObserver struct{}

func (boundaryObserver) Schema() provider.Schema { return provider.Schema{} }
func (boundaryObserver) Capabilities() provider.Capabilities {
	return provider.Capabilities{List: true}
}
func (boundaryObserver) Read(context.Context, string) (core.Observation, error) {
	return core.Observation{Availability: core.AvailabilityOk, State: core.State{}, ObservedAt: time.Now()}, nil
}
func (boundaryObserver) Apply(context.Context, core.Plan) error { return nil }

// TestInventoryAndPrune_DropBoundaryAgrees pins the §9.2 boundary: a drop to
// exactly half is not "50% 未満", so neither the inventory guard nor the prune
// List count-drop gate (§12.4b) halts on it; one item fewer halts both.
func TestInventoryAndPrune_DropBoundaryAgrees(t *testing.T) {
	ctx := context.Background()
	ids := func(items []diff.DesiredResource) []core.ID {
		out := make([]core.ID, len(items))
		for i, r := range items {
			out[i] = r.ID
		}
		return out
	}

	for _, tc := range []struct {
		next     int
		wantHalt bool
	}{
		{next: 5, wantHalt: false}, // exactly half
		{next: 4, wantHalt: true},  // below half
	} {
		src := &stubInventory{items: inventoryItems(10)}
		g := render.NewInventoryGuard(src)
		if r := g.Compose(ctx); r.Halt {
			t.Fatalf("[#47-1] baseline of 10 must be accepted: %+v", r)
		}
		src.items = inventoryItems(tc.next)
		inv := g.Compose(ctx)

		engine := prune.New()
		lister := &boundaryLister{ids: ids(inventoryItems(10))}
		desired := inventoryItems(10)
		if _, err := engine.ComputePrune(ctx, lister, boundaryObserver{}, core.NewScope(nil), core.NewScope(nil), desired, nil, &bytes.Buffer{}); err != nil {
			t.Fatalf("[#47-1] prune baseline: %v", err)
		}
		lister.ids = ids(inventoryItems(tc.next))
		pr, err := engine.ComputePrune(ctx, lister, boundaryObserver{}, core.NewScope(nil), core.NewScope(nil), inventoryItems(tc.next), nil, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("[#47-1] prune: %v", err)
		}

		if inv.Halt != tc.wantHalt || inv.Incomplete != tc.wantHalt {
			t.Errorf("[#47-1] inventory 10 → %d: Halt=%v Incomplete=%v, want %v (%s)",
				tc.next, inv.Halt, inv.Incomplete, tc.wantHalt, inv.Reason)
		}
		if pr.Halted != tc.wantHalt {
			t.Errorf("[#47-1] prune List 10 → %d: Halted=%v, want %v (%s)",
				tc.next, pr.Halted, tc.wantHalt, pr.HaltReason)
		}
		if inv.Halt != pr.Halted {
			t.Errorf("[#47-1] inventory and prune disagree on 10 → %d: inventory halt=%v, prune halt=%v",
				tc.next, inv.Halt, pr.Halted)
		}
	}
}
