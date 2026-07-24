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

// composite_status_test.go contains CLI-level acceptance tests for Story
// S072a14-3 AC-S072a14-3-3: §9.7 worst-first composite status aggregation.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect only its observable behaviour (stdout, stderr, exit code, state
// file).  No internal package is imported directly.
//
// The §9.7 full order is:
//
//	unknown > stuck > oscillating > drifted > blocked > converging > converged
//
// AC tags:
//
//	AC-S072a14-3-3  Child unknown → composite status = unknown (§9.7 worst-first)
//	[AC-S072a14-3-M] marks the machine-executable scenario tests.
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedStateWithObserveUnknown writes a state file that:
//   - Puts resources under the "resources" key.
//   - Puts unknownIDs under "observe_unknown" so the fake provider returns
//     AvailabilityUnknown for those IDs.
//   - Sets provider_version to testProviderVersion (Scb0ecd-3 gate).
func seedStateWithObserveUnknown(t *testing.T, statePath string,
	resources map[string]map[string]any, unknownIDs []string) {
	t.Helper()
	resMap := make(map[string]any, len(resources))
	for id, fields := range resources {
		resMap[id] = map[string]any{"fields": fields}
	}
	observeUnknown := make(map[string]string, len(unknownIDs))
	for _, id := range unknownIDs {
		observeUnknown[id] = "unknown" // any non-"error" value → AvailabilityUnknown
	}
	sf := map[string]any{
		"resources":        resMap,
		"observe_unknown":  observeUnknown,
		"provider_version": testProviderVersion,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStateWithObserveUnknown: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateWithObserveUnknown: write: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S072a14-3-3: composite status aggregation (§9.7 worst-first)
// ─────────────────────────────────────────────────────────────────────────────

// TestCompositeStatus_ChildUnknown_AggregatesUnknown verifies that when one
// child of a composite is observed as AvailabilityUnknown, the composite's
// aggregated status is "unknown" (the worst value in the §9.7 full order).
//
// Steps:
//  1. Reconcile with composite to create children in reality.
//  2. Inject observe_unknown for childA in the state file (simulates unreachable).
//  3. Run sakutei plan: the composite summary must show "unknown".
//
// [AC-S072a14-3-3] [AC-S072a14-3-M]
func TestCompositeStatus_ChildUnknown_AggregatesUnknown(t *testing.T) {
	// [AC-S072a14-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	compositeID := "cs-composite"
	childA := compositeID + "-child-a"
	childB := compositeID + "-child-b"

	// Step 1: reconcile to create children in reality.
	writeCompositeIntentE(t, intentDir, compositeID, "v1")
	initStateFile(t, statePath)
	stdout1, stderr1, code1 := runReconcile(t, intentDir, statePath,
		"--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-3] setup reconcile exited %d; stderr=%s; stdout=%s",
			code1, stderr1, stdout1)
	}

	// Confirm children exist in state.
	stateData, _ := os.ReadFile(statePath)
	if !strings.Contains(string(stateData), childA) {
		t.Fatalf("[AC-S072a14-3-3] childA not in state after setup; state:\n%s", stateData)
	}
	if !strings.Contains(string(stateData), childB) {
		t.Fatalf("[AC-S072a14-3-3] childB not in state after setup; state:\n%s", stateData)
	}

	// Step 2: inject observe_unknown for childA.
	// This simulates the child being unreachable (e.g. host down, network partition).
	// §3 contract: unknown ≠ absent — the plan must NOT schedule a Delete.
	seedStateWithObserveUnknown(t, statePath,
		map[string]map[string]any{
			childA: {"name": childA, "managed_by": compositeID}, // exists in reality
			childB: {"name": childB, "managed_by": compositeID}, // converged
		},
		[]string{childA}, // childA → AvailabilityUnknown
	)

	// Step 3: run plan — composite status must be "unknown" (worst-first).
	stdout2, stderr2, code2 := runPlan(t, intentDir, statePath,
		"--generated", generatedDir)
	_ = code2 // plan exits 0 even when composites have non-converged status
	t.Logf("[AC-S072a14-3-3] plan exit=%d stderr=%s", code2, stderr2)
	t.Logf("[AC-S072a14-3-3] plan stdout:\n%s", stdout2)

	// The composite status section must appear.
	if !strings.Contains(stdout2, "composite "+compositeID) {
		t.Fatalf("[AC-S072a14-3-3] composite status section missing from plan output; stdout:\n%s", stdout2)
	}

	// The aggregated status must be "unknown" (childA is unknown → worst-first).
	if !strings.Contains(stdout2, "composite "+compositeID+": unknown") {
		t.Errorf("[AC-S072a14-3-3] expected composite %s: unknown but got:\n%s",
			compositeID, stdout2)
	}
	t.Logf("[AC-S072a14-3-3] PASS: child unknown → composite status = unknown (§9.7 worst-first)")
}

// TestCompositeStatus_AllConverged_AggregatesConverged verifies that when all
// children are converged, the composite status is "converged".
//
// [AC-S072a14-3-3] [AC-S072a14-3-M]
func TestCompositeStatus_AllConverged_AggregatesConverged(t *testing.T) {
	// [AC-S072a14-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	compositeID := "cs-all-converged"
	childA := compositeID + "-child-a"
	childB := compositeID + "-child-b"

	// Reconcile to create children.
	writeCompositeIntentE(t, intentDir, compositeID, "v1")
	initStateFile(t, statePath)
	_, _, code1 := runReconcile(t, intentDir, statePath, "--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-3] setup reconcile exited %d", code1)
	}

	// Ensure both children are in state and matching (no drift, no unknown).
	stateData, _ := os.ReadFile(statePath)
	if !strings.Contains(string(stateData), childA) || !strings.Contains(string(stateData), childB) {
		t.Fatalf("[AC-S072a14-3-3] children not found in state; state:\n%s", stateData)
	}

	// Run plan with converged children.
	stdout, _, code := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	t.Logf("[AC-S072a14-3-3] all-converged plan (exit=%d):\n%s", code, stdout)

	// With no drift and no unknowns, the composite status must be "converged".
	if strings.Contains(stdout, "composite "+compositeID+": unknown") {
		t.Errorf("[AC-S072a14-3-3] composite must not be unknown when all children are converged; stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "composite "+compositeID) {
		// If the composite summary is printed, it must be "converged".
		if !strings.Contains(stdout, "composite "+compositeID+": converged") {
			t.Errorf("[AC-S072a14-3-3] expected composite %s: converged; stdout:\n%s", compositeID, stdout)
		}
	}
	// Either "converged" appears in the composite line or the "Converged (no changes)"
	// message appears (the composite summary is only printed for the converged case too).
	if !strings.Contains(stdout, "Converged (no changes)") && !strings.Contains(stdout, "composite "+compositeID+": converged") {
		t.Errorf("[AC-S072a14-3-3] expected Converged (no changes) or converged composite status; stdout:\n%s", stdout)
	}
	t.Logf("[AC-S072a14-3-3] PASS: all children converged → composite status = converged (or no-change)")
}

// TestCompositeStatus_ChildDrifted_AggregatesDrifted verifies §9.7: when a
// child has a diff (StatusDrifted), the composite status is "drifted".
//
// [AC-S072a14-3-3] [AC-S072a14-3-M]
func TestCompositeStatus_ChildDrifted_AggregatesDrifted(t *testing.T) {
	// [AC-S072a14-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	compositeID := "cs-drifted"
	childA := compositeID + "-child-a"
	childB := compositeID + "-child-b"

	// Reconcile to create children.
	writeCompositeIntentE(t, intentDir, compositeID, "v1")
	initStateFile(t, statePath)
	_, _, code1 := runReconcile(t, intentDir, statePath, "--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-3] setup reconcile exited %d", code1)
	}

	// Drift childA: set its name to a wrong value so diffFields detects a diff.
	// (diffFields only checks fields in desired; size would be ignored since it's
	// not in desired.  Changing "name" — which IS in desired — creates a real diff.)
	seedState(t, statePath, map[string]map[string]any{
		childA: {"name": "drifted-child-name", "managed_by": compositeID}, // name drifted
		childB: {"name": childB, "managed_by": compositeID},
	})

	// Plan: childA has a diff (name mismatch) → StatusDrifted → composite = "drifted".
	stdout, _, _ := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	t.Logf("[AC-S072a14-3-3] drifted-child plan:\n%s", stdout)

	if !strings.Contains(stdout, "composite "+compositeID+": drifted") {
		t.Errorf("[AC-S072a14-3-3] expected composite %s: drifted when childA has diff; stdout:\n%s",
			compositeID, stdout)
	}
	t.Logf("[AC-S072a14-3-3] PASS: drifted child → composite status = drifted")
}

// TestCompositeStatus_WorstFirst_UnknownBeatsAll verifies §9.7 ordering:
// when one child is drifted AND another is unknown, the composite status is
// "unknown" (unknown > drifted in the full order).
//
// [AC-S072a14-3-3] [AC-S072a14-3-M]
func TestCompositeStatus_WorstFirst_UnknownBeatsAll(t *testing.T) {
	// [AC-S072a14-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	compositeID := "cs-worst"
	childA := compositeID + "-child-a" // will be unknown
	childB := compositeID + "-child-b" // will be drifted

	// Reconcile to create children.
	writeCompositeIntentE(t, intentDir, compositeID, "v1")
	initStateFile(t, statePath)
	_, _, code1 := runReconcile(t, intentDir, statePath, "--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-3] setup reconcile exited %d", code1)
	}

	// childA → unknown (via observe_unknown), childB → drifted (name wrong).
	// The composite aggregates: unknown > drifted → composite = "unknown".
	seedStateWithObserveUnknown(t, statePath,
		map[string]map[string]any{
			childA: {"name": childA, "managed_by": compositeID},
			childB: {"name": "worst-drifted-name", "managed_by": compositeID}, // name drifted
		},
		[]string{childA}, // childA → AvailabilityUnknown
	)

	// Plan: childA=unknown, childB=drifted (name mismatch) → composite = unknown (unknown > drifted).
	stdout, _, _ := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	t.Logf("[AC-S072a14-3-3] worst-first plan:\n%s", stdout)

	if !strings.Contains(stdout, "composite "+compositeID+": unknown") {
		t.Errorf("[AC-S072a14-3-3] expected composite %s: unknown (unknown > drifted); stdout:\n%s",
			compositeID, stdout)
	}
	t.Logf("[AC-S072a14-3-3] PASS: unknown > drifted in §9.7 worst-first ordering → composite = unknown")
}

// TestCompositeStatus_NoComposites_NoStatusSection verifies that without any
// composite resources, the "Composite status" section is NOT printed.
// Regression guard for plain-resource-only plans.
//
// [AC-S072a14-3-M]
func TestCompositeStatus_NoComposites_NoStatusSection(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	// Plain resource (no composite).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "plain-vm-cs",
				"fields":   map[string]string{"name": "plain-vm-cs", "size": "1"},
			},
		},
	})
	initStateFile(t, statePath)

	stdout, _, code := runPlan(t, intentDir, statePath, "--generated", generatedDir)
	if code != 0 {
		t.Fatalf("plain plan exited %d; stdout=%s", code, stdout)
	}

	if strings.Contains(stdout, "Composite status") {
		t.Errorf("plain resource: 'Composite status' section must not appear; stdout:\n%s", stdout)
	}
	t.Logf("PASS: no composites → no composite status section")
}

// TestCompositeStatus_DeterministicOrder verifies F-3: when several composites
// are present, the "Composite status" lines are emitted in a deterministic
// (sorted) order — not the randomised order of Go map iteration.  It runs the
// same plan several times and asserts the composite-line order is identical and
// ascending every time.
//
// [AC-S072a14-3-M]
func TestCompositeStatus_DeterministicOrder(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	// Three composites whose IDs are deliberately NOT in insertion order, so a
	// map-iteration bug would surface as a non-sorted / unstable order.
	compositeIDs := []string{"cs-zebra", "cs-alpha", "cs-mike"}
	writeIntentFile(t, intentDir, map[string]any{
		"resources": func() []map[string]any {
			out := make([]map[string]any, 0, len(compositeIDs))
			for _, id := range compositeIDs {
				out = append(out, map[string]any{
					"provider": "fake",
					"id":       id,
					"fields": map[string]string{
						"name":  id,
						"kind":  "composite",
						"extra": "v1",
					},
				})
			}
			return out
		}(),
	})
	initStateFile(t, statePath)

	// Expected sorted order of composite lines.
	wantOrder := []string{"cs-alpha", "cs-mike", "cs-zebra"}

	extractOrder := func(stdout string) []string {
		var got []string
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			const prefix = "composite "
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			rest := strings.TrimPrefix(line, prefix)
			// rest is "<id>: <status>"
			if idx := strings.IndexByte(rest, ':'); idx >= 0 {
				got = append(got, rest[:idx])
			}
		}
		return got
	}

	var firstOrder []string
	for i := 0; i < 5; i++ {
		stdout, stderr, code := runPlan(t, intentDir, statePath, "--generated", generatedDir)
		if code != 0 {
			t.Fatalf("F-3: plan run %d exited %d; stderr=%s stdout=%s", i, code, stderr, stdout)
		}
		got := extractOrder(stdout)
		if len(got) != len(wantOrder) {
			t.Fatalf("F-3: run %d expected %d composite lines, got %d; stdout:\n%s",
				i, len(wantOrder), len(got), stdout)
		}
		for j := range wantOrder {
			if got[j] != wantOrder[j] {
				t.Fatalf("F-3: run %d composite order not sorted: got %v want %v; stdout:\n%s",
					i, got, wantOrder, stdout)
			}
		}
		if firstOrder == nil {
			firstOrder = got
		} else {
			for j := range firstOrder {
				if got[j] != firstOrder[j] {
					t.Fatalf("F-3: composite order not stable across runs: run0=%v run%d=%v",
						firstOrder, i, got)
				}
			}
		}
	}
	t.Logf("F-3: PASS composite status printed in deterministic sorted order %v across 5 runs", firstOrder)
}
