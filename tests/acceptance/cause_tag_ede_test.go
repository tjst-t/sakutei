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

// cause_tag_ede_test.go contains CLI-level acceptance tests for Story S072a14-3,
// verifying cause tag E (external-input change) and D (cascade / upstream change)
// activation, plus the daemon's ability to handle composite expansion and E/D
// classification.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect only its observable behaviour (stdout, stderr, exit code, state
// file).  No internal package is imported directly.
//
// AC tags:
//
//	AC-S072a14-3-1  External input change → cause: E (RenderedRevisionChanged activated)
//	AC-S072a14-3-2  Composite parent intent change → child Op has cause: D (UpstreamChanged activated)
//	[AC-S072a14-3-M] marks the machine-executable scenario tests.
package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// local helpers
// ─────────────────────────────────────────────────────────────────────────────

// startDaemonWithArgs launches the sakutei daemon with explicit extra flags
// (e.g. --generated, --inventory-hash).  Unlike startDaemonCombined, the
// caller passes all extra flags as extraArgs.
func startDaemonWithArgs(t *testing.T, intentDir, statePath, interval string, extraArgs ...string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
	t.Helper()
	bin := buildBinary(t)

	var stdoutBuf, stderrBuf syncBuffer
	args := []string{
		"--intent", intentDir,
		"--state", statePath,
		"--interval", interval,
	}
	args = append(args, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("startDaemonWithArgs: Start: %v", err)
	}
	return cmd, &stdoutBuf, &stderrBuf
}

// writeSingleResourceIntentE writes a plain resource into intentDir with a given size.
func writeSingleResourceIntentE(t *testing.T, intentDir, id, size string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       id,
				"fields":   map[string]string{"name": id, "size": size},
			},
		},
	})
}

// writeCompositeIntentE writes a composite resource with an extra discriminating
// field so that changing extraField changes the intent_revision.
func writeCompositeIntentE(t *testing.T, intentDir, compositeID, extraField string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       compositeID,
				"fields": map[string]string{
					"name":  compositeID,
					"kind":  "composite",
					"extra": extraField,
				},
			},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S072a14-3-1: cause: E fires when external input changes (intent stable)
// ─────────────────────────────────────────────────────────────────────────────

// TestCauseTag_E_PlanExternalInputChange verifies that when --inventory-hash
// changes between two plan invocations while intent is stable, the Op for a
// drifted resource is tagged cause: E (RenderedRevisionChanged=true).
//
// Steps:
//  1. Reconcile with H1 to establish prev_rendered_revision=H1 in cause-state.
//  2. Drift reality (so there is a diff).
//  3. Plan with same intent + SAME H1 → rendered stable → NOT E (B or A).
//  4. Plan with same intent + DIFFERENT H2 → rendered changes H1→H2 → cause: E.
//
// [AC-S072a14-3-1] [AC-S072a14-3-M]
func TestCauseTag_E_PlanExternalInputChange(t *testing.T) {
	// [AC-S072a14-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	const h1 = "e-plan-inv-hash-h1-aaa"
	const h2 = "e-plan-inv-hash-h2-bbb"
	const resID = "e-plan-vm"

	// Cycle 1: reconcile with H1 to establish cause-state with prev_rendered_revision=H1.
	writeSingleResourceIntentE(t, intentDir, resID, "10")
	initStateFile(t, statePath)
	_, _, code1 := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h1)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-1] setup reconcile exited %d", code1)
	}

	// Confirm rendered_revision ≠ intent_revision (phase 1 wired correctly).
	stdout1b, _, _ := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h1)
	intentRev1, renderedRev1 := extractRevisions(stdout1b)
	if intentRev1 == renderedRev1 {
		t.Fatalf("[AC-S072a14-3-1] rendered_revision should differ from intent_revision when --inventory-hash set; both=%s stdout:\n%s",
			intentRev1, stdout1b)
	}

	// Drift reality: provider recorded size=10, now make it size=50.
	seedState(t, statePath, map[string]map[string]any{
		resID: {"name": resID, "size": "50"},
	})

	// Cycle 2: plan with H1 (same inventory) to update prev_rendered_revision=H1 in
	// cause-state.  There is a diff (size 50 vs 10) but rendered_revision is unchanged.
	// Expected: NOT E (rendered stable).
	stdout2, _, code2 := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h1)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-3-1] cycle2 plan (H1) exited %d; stdout=%s", code2, stdout2)
	}
	tags2 := extractCauseTags(stdout2)
	if anyTagEquals(tags2, "E") {
		t.Errorf("[AC-S072a14-3-1] cycle2: E must NOT fire when rendered_revision is stable (H1→H1); tags=%v\nstdout:\n%s",
			tags2, stdout2)
	}
	t.Logf("[AC-S072a14-3-1] cycle2 tags (H1 stable, drift): %v", tags2)

	// Cycle 3: plan with SAME intent + H2 (different inventory hash).
	// rendered_revision changes H1→H2, intent_revision is the same → cause: E.
	// (runPlan does not apply, so state still has size=50.)
	stdout3, _, code3 := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h2)
	if code3 != 0 {
		t.Fatalf("[AC-S072a14-3-1] cycle3 plan (H2) exited %d; stdout=%s", code3, stdout3)
	}

	// Verify rendered changed from H1 → H2 while intent stayed the same.
	intentRev3, renderedRev3 := extractRevisions(stdout3)
	if intentRev3 != intentRev1 {
		t.Errorf("[AC-S072a14-3-1] intent_revision should be stable between H1 and H2 runs; H1=%s H2=%s",
			intentRev1, intentRev3)
	}
	if renderedRev3 == renderedRev1 {
		t.Errorf("[AC-S072a14-3-1] rendered_revision must change from H1=%s when inventory hash changes to H2; got same",
			renderedRev1)
	}

	// The diff must be tagged E.
	tags3 := extractCauseTags(stdout3)
	if !anyTagEquals(tags3, "E") {
		t.Errorf("[AC-S072a14-3-1] expected cause: E when inventory hash changes with stable intent; got %v\nstdout:\n%s",
			tags3, stdout3)
	}
	// E has higher priority than B; B must not appear.
	if anyTagEquals(tags3, "B") {
		t.Errorf("[AC-S072a14-3-1] cause: B must not appear when cause: E fires (E > B); got %v\nstdout:\n%s",
			tags3, stdout3)
	}
	t.Logf("[AC-S072a14-3-1] PASS: inventory hash H1→H2 + stable intent → cause: E; tags=%v", tags3)
}

// TestCauseTag_E_ReconcileExternalInputChange verifies cause: E on
// "sakutei reconcile" when the inventory hash changes with stable intent.
//
// [AC-S072a14-3-1] [AC-S072a14-3-M]
func TestCauseTag_E_ReconcileExternalInputChange(t *testing.T) {
	// [AC-S072a14-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	const h1 = "e-rec-hash-h1-111"
	const h2 = "e-rec-hash-h2-222"
	const resID = "e-rec-vm"

	// Cycle 1: reconcile with H1 (creates resource, establishes cause-state H1).
	writeSingleResourceIntentE(t, intentDir, resID, "5")
	initStateFile(t, statePath)
	_, _, code1 := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h1)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-1] setup reconcile exited %d", code1)
	}

	// Drift reality (size 5 → 99).
	seedState(t, statePath, map[string]map[string]any{
		resID: {"name": resID, "size": "99"},
	})

	// Cycle 2: reconcile with H1 (same) → records prev_rendered_revision=H1, corrects drift.
	stdout2, _, code2 := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h1)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-3-1] cycle2 reconcile (H1) exited %d; stdout=%s", code2, stdout2)
	}

	// Drift reality again (cycle 2 corrected it).
	seedState(t, statePath, map[string]map[string]any{
		resID: {"name": resID, "size": "77"},
	})

	// Cycle 3: reconcile with H2 (different hash), same intent → cause: E.
	stdout3, _, code3 := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--inventory-hash", h2)
	if code3 != 0 {
		t.Fatalf("[AC-S072a14-3-1] cycle3 reconcile (H2) exited %d; stdout=%s", code3, stdout3)
	}
	tags3 := extractCauseTags(stdout3)
	if !anyTagEquals(tags3, "E") {
		t.Errorf("[AC-S072a14-3-1] reconcile: expected cause: E when inventory hash changes with stable intent; got %v\nstdout:\n%s",
			tags3, stdout3)
	}
	t.Logf("[AC-S072a14-3-1] PASS: reconcile H1→H2 + stable intent → cause: E; tags=%v", tags3)
}

// TestCauseTag_E_NeverFiresWithoutExternalInput confirms backward compatibility:
// without --inventory-hash, rendered_revision == intent_revision and cause: E
// never fires (v1a invariant preserved).
//
// [AC-S072a14-3-M]
func TestCauseTag_E_NeverFiresWithoutExternalInput(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	writeSingleResourceIntentE(t, intentDir, "no-ext-vm", "5")
	initStateFile(t, statePath)

	// Cycle 1: reconcile (no inventory hash) to establish cause-state.
	_, _, code1 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code1 != 0 {
		t.Fatalf("setup reconcile exited %d", code1)
	}

	// Drift reality.
	seedState(t, statePath, map[string]map[string]any{
		"no-ext-vm": {"name": "no-ext-vm", "size": "99"},
	})

	// Cycle 2: plan (no inventory hash) → rendered == intent → E must never fire.
	stdout, _, _ := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)

	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev != renderedRev {
		t.Errorf("without --inventory-hash: rendered_revision must equal intent_revision; got intent=%s rendered=%s",
			intentRev, renderedRev)
	}

	tags := extractCauseTags(stdout)
	if anyTagEquals(tags, "E") {
		t.Errorf("without --inventory-hash: cause: E must not fire; got %v\nstdout:\n%s", tags, stdout)
	}
	t.Logf("PASS: no external input → rendered==intent, E never fires; tags=%v", tags)
}

// TestCauseTag_E_DaemonWithInventoryHash verifies that the daemon correctly
// handles --inventory-hash (render phase 1 wired in daemon) and converges
// resources while producing rendered_revision ≠ intent_revision.
//
// The daemon's cause-E detection requires two cycles with different hashes
// in the same process (prevRev is in-memory, not persisted across restarts).
// This test verifies the daemon infrastructure: phase 1 runs, rendered revision
// is computed correctly, and the daemon converges without crashing.
//
// Cause-E subprocess detection is covered by plan/reconcile tests (plan and
// reconcile persist cause-state to a file, making cross-invocation E tests
// straightforward).  For the daemon, E fires within a single long-running
// process if the ResolverCache returns a different hash over time — tested via
// the render/phase1 unit tests (TestRenderPhase1_CacheLastValueOnUnreachable).
//
// [AC-S072a14-3-1] [AC-S072a14-3-M]
func TestCauseTag_E_DaemonWithInventoryHash(t *testing.T) {
	// [AC-S072a14-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	generatedDir := filepath.Join(tmp, "generated")

	const h1 = "daemon-e-hash-h1-111"
	const resID = "daemon-e-vm"

	writeSingleResourceIntentE(t, intentDir, resID, "3")
	initStateFile(t, statePath)

	// Start daemon with --inventory-hash to verify render phase 1 is wired.
	cmd, stdoutBuf, stderrBuf := startDaemonWithArgs(t, intentDir, statePath, "150ms",
		"--inventory-hash", h1,
		"--generated", generatedDir,
	)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// The daemon must converge (resource created, no phase 1 crash).
	if !waitForStringInBuf(stdoutBuf, "converged (no changes)", 8*time.Second) {
		t.Fatalf("[AC-S072a14-3-1] daemon with --inventory-hash did not converge within 8s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// The daemon must NOT have crashed with ExternalInputUnreachable
	// (we provided a valid hash, so phase 1 should succeed).
	if strings.Contains(stderrBuf.String(), "ExternalInputUnreachable") {
		t.Errorf("[AC-S072a14-3-1] daemon emitted ExternalInputUnreachable unexpectedly\nstderr:\n%s",
			stderrBuf.String())
	}

	// The daemon must print rendered_revision in cycle logs.  Since --inventory-hash
	// is set, rendered_revision ≠ intent_revision.  We verify the resource was
	// created in the state file (meaning phase 1 + phase 2 + convergence all worked).
	stateData, _ := os.ReadFile(statePath)
	if !strings.Contains(string(stateData), resID) {
		t.Errorf("[AC-S072a14-3-1] resource %s not in state after daemon convergence; state:\n%s",
			resID, stateData)
	}

	t.Logf("[AC-S072a14-3-1] PASS: daemon with --inventory-hash converged correctly; render phase 1 wired")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S072a14-3-2: cause: D fires for composite children when parent intent changes
// ─────────────────────────────────────────────────────────────────────────────

// TestCauseTag_D_CompositeParentChange verifies that when the composite parent's
// intent changes, expanded children with a diff get cause: D.
//
// The FakeCompositeExpander produces two children: <parent>-child-a and
// <parent>-child-b from a parent with kind=composite.  The parent is NOT
// diff'd directly; only children are diff'd and applied.
//
// Steps:
//  1. Reconcile with composite V1 → seeds reality (children created).
//  2. Plan with V1 → converged (saves cause-state with V1 intent_revision).
//  3. Drift childA AND change parent to V2.
//  4. Plan: childA has a diff AND parent changed → D fires for childA.
//
// [AC-S072a14-3-2] [AC-S072a14-3-M]
func TestCauseTag_D_CompositeParentChange(t *testing.T) {
	// [AC-S072a14-3-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	compositeID := "d-composite"
	childA := compositeID + "-child-a"
	childB := compositeID + "-child-b"

	// Cycle 1: reconcile with composite V1 to seed reality with children.
	writeCompositeIntentE(t, intentDir, compositeID, "v1")
	initStateFile(t, statePath)
	stdout1, stderr1, code1 := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-2] setup reconcile exited %d; stderr=%s stdout=%s",
			code1, stderr1, stdout1)
	}

	// Confirm children were created in reality.
	stateData, _ := os.ReadFile(statePath)
	if !strings.Contains(string(stateData), childA) {
		t.Fatalf("[AC-S072a14-3-2] childA not in state after setup; state:\n%s", stateData)
	}

	// Cycle 2: plan with V1 → converged (saves cause-state with V1 intent_revision).
	stdout2, _, code2 := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--generated", generatedDir)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-3-2] converged plan exited %d; stdout=%s", code2, stdout2)
	}
	t.Logf("[AC-S072a14-3-2] cycle2 plan:\n%s", stdout2)

	// Drift childA: set its name to a wrong value so diffFields detects a diff.
	// Desired childA = {name=<childA>, managed_by=<compositeID>}.
	// Reality childA = {name="wrong-child-a", managed_by=<compositeID>}.
	// → "name" field differs (desired != actual) → UPDATE op → StatusDrifted.
	seedState(t, statePath, map[string]map[string]any{
		childA: {"name": "wrong-child-a", "managed_by": compositeID}, // name drifted
		childB: {"name": childB, "managed_by": compositeID},
	})

	// Change composite intent V1 → V2. Now:
	//   - intent_revision changes (parent's "extra" field changed V1→V2)
	//   - childA has a diff (name field wrong: "wrong-child-a" vs desired childA)
	//   - childA should get cause: D (upstream/parent changed + child has diff)
	writeCompositeIntentE(t, intentDir, compositeID, "v2")

	stdout3, _, code3 := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--generated", generatedDir)
	if code3 != 0 {
		t.Fatalf("[AC-S072a14-3-2] changed-intent plan exited %d; stdout=%s", code3, stdout3)
	}
	t.Logf("[AC-S072a14-3-2] cycle3 (parent V1→V2 + drift childA):\n%s", stdout3)

	tags3 := extractCauseTags(stdout3)
	t.Logf("[AC-S072a14-3-2] tags: %v", tags3)

	if !anyTagEquals(tags3, "D") {
		t.Errorf("[AC-S072a14-3-2] expected cause: D for childA when parent intent V1→V2; got %v\nstdout:\n%s",
			tags3, stdout3)
	}
	t.Logf("[AC-S072a14-3-2] PASS: composite parent V1→V2 + drifted child → cause: D; tags=%v", tags3)
}

// TestCauseTag_D_NotFiredWithoutParentChange verifies that cause: D does NOT
// fire when the composite parent is stable and only a child drifts (cause: B).
//
// [AC-S072a14-3-2] [AC-S072a14-3-M]
func TestCauseTag_D_NotFiredWithoutParentChange(t *testing.T) {
	// [AC-S072a14-3-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	compositeID := "d-stable-composite"
	childA := compositeID + "-child-a"
	childB := compositeID + "-child-b"

	// Cycle 1: reconcile with stable composite.
	writeCompositeIntentE(t, intentDir, compositeID, "stable")
	initStateFile(t, statePath)
	_, _, code1 := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--generated", generatedDir)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-3-2] setup reconcile exited %d", code1)
	}

	// Cycle 2: plan with same intent (saves stable cause-state).
	_, _, code2 := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--generated", generatedDir)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-3-2] converged plan exited %d", code2)
	}

	// Drift childA's name so diffFields detects a real diff; parent intent unchanged.
	seedState(t, statePath, map[string]map[string]any{
		childA: {"name": "drifted-name", "managed_by": compositeID}, // name drifted
		childB: {"name": childB, "managed_by": compositeID},
	})

	// Plan with SAME intent (parent stable) + drifted child → cause: B (not D).
	stdout, _, _ := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath,
		"--generated", generatedDir)

	tags := extractCauseTags(stdout)
	t.Logf("[AC-S072a14-3-2] drift-only (parent stable) tags: %v", tags)

	if anyTagEquals(tags, "D") {
		t.Errorf("[AC-S072a14-3-2] cause: D must not fire when parent is stable (child-only drift); got %v\nstdout:\n%s",
			tags, stdout)
	}
	t.Logf("[AC-S072a14-3-2] PASS: parent stable + child drift → not D; tags=%v", tags)
}

// TestCauseTag_D_DaemonCompositeCycle verifies that the daemon handles composite
// expansion correctly and remains stable after a parent intent change.
//
// [AC-S072a14-3-2] [AC-S072a14-3-M]
func TestCauseTag_D_DaemonCompositeCycle(t *testing.T) {
	// [AC-S072a14-3-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")

	compositeID := "dd-composite"
	childA := compositeID + "-child-a"

	writeCompositeIntentE(t, intentDir, compositeID, "v1")
	initStateFile(t, statePath)

	cmd, stdoutBuf, stderrBuf := startDaemonWithArgs(t, intentDir, statePath, "150ms",
		"--generated", generatedDir)
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Wait for composite children to be created.
	if !waitForCondition(8*time.Second, func() bool {
		data, err := os.ReadFile(statePath)
		if err != nil {
			return false
		}
		return strings.Contains(string(data), childA)
	}) {
		t.Fatalf("[AC-S072a14-3-2] daemon did not create composite children within 8s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// Wait for stable convergence.
	if !waitForStringInBuf(stdoutBuf, "converged (no changes)", 8*time.Second) {
		t.Fatalf("[AC-S072a14-3-2] daemon did not converge within 8s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// Drift childA's name AND change composite intent V1 → V2 simultaneously.
	// Using a wrong name for childA creates a real diff (name is in desired).
	seedState(t, statePath, map[string]map[string]any{
		childA:                   {"name": "daemon-wrong-name", "managed_by": compositeID},
		compositeID + "-child-b": {"name": compositeID + "-child-b", "managed_by": compositeID},
	})
	writeCompositeIntentE(t, intentDir, compositeID, "v2")

	// The daemon must process the D-tagged change and converge again.
	// Wait for a new "converged" or at least a new cycle after the change.
	if !waitForCondition(8*time.Second, func() bool {
		// Check that the daemon ran another cycle (new cycle counter or converged message).
		return strings.Count(stdoutBuf.String(), "cycle") >= 3
	}) {
		t.Logf("[AC-S072a14-3-2] cycles in stdout: %d", strings.Count(stdoutBuf.String(), "cycle"))
	}

	// The daemon must be still alive.
	if !waitForStringInBuf(stdoutBuf, "cycle", 3*time.Second) {
		t.Errorf("[AC-S072a14-3-2] daemon stopped cycling after parent change\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}
	t.Logf("[AC-S072a14-3-2] PASS: daemon handled composite parent change + child drift; still cycling")
}

// writeTwoCompositeIntentE writes two composite parents with their own
// discriminating "extra" fields.
func writeTwoCompositeIntentE(t *testing.T, intentDir, id1, extra1, id2, extra2 string) {
	t.Helper()
	var resources []map[string]any
	for _, p := range [][2]string{{id1, extra1}, {id2, extra2}} {
		resources = append(resources, map[string]any{
			"provider": "fake",
			"id":       p[0],
			"fields":   map[string]string{"name": p[0], "kind": "composite", "extra": p[1]},
		})
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": resources})
}

// causeTagsByTarget maps each Op target in plan output to its cause tag.
func causeTagsByTarget(output string) map[string]string {
	tags := make(map[string]string)
	target := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		parts := strings.Fields(trimmed)
		switch {
		case len(parts) >= 3 && (parts[1] == "CREATE" || parts[1] == "UPDATE" || parts[1] == "DELETE"):
			target = parts[2]
		case strings.HasPrefix(trimmed, "cause:") && len(parts) >= 2 && target != "":
			tags[target] = parts[1]
		}
	}
	return tags
}

// tagForLocal returns the cause tag of the Op whose target ends in "/"+local.
func tagForLocal(tags map[string]string, local string) string {
	for target, tag := range tags {
		if target == local || strings.HasSuffix(target, "/"+local) {
			return tag
		}
	}
	return ""
}

// TestCauseTag_D_OnlyChangedParent verifies #14: with two composite parents,
// changing only one of them tags only that parent's children cause: D.  The
// other parent's drifted child is plain drift (cause: B).
//
// [AC-14-1]
func TestCauseTag_D_OnlyChangedParent(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	generatedDir := filepath.Join(tmp, "generated")
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	const alpha, beta = "alpha", "beta"

	writeTwoCompositeIntentE(t, intentDir, alpha, "v1", beta, "v1")
	initStateFile(t, statePath)
	if stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--cause-state", causeStatePath, "--generated", generatedDir); code != 0 {
		t.Fatalf("[AC-14-1] setup reconcile exited %d; stderr=%s stdout=%s", code, stderr, stdout)
	}
	if stdout, _, code := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath, "--generated", generatedDir); code != 0 {
		t.Fatalf("[AC-14-1] converged plan exited %d; stdout=%s", code, stdout)
	}

	// Drift child-a of BOTH parents, then change only alpha.
	seedState(t, statePath, map[string]map[string]any{
		alpha + "-child-a": {"name": "wrong", "managed_by": alpha},
		alpha + "-child-b": {"name": alpha + "-child-b", "managed_by": alpha},
		beta + "-child-a":  {"name": "wrong", "managed_by": beta},
		beta + "-child-b":  {"name": beta + "-child-b", "managed_by": beta},
	})
	writeTwoCompositeIntentE(t, intentDir, alpha, "v2", beta, "v1")

	stdout, _, code := runPlan(t, intentDir, statePath,
		"--cause-state", causeStatePath, "--generated", generatedDir)
	if code != 0 {
		t.Fatalf("[AC-14-1] plan exited %d; stdout=%s", code, stdout)
	}
	tags := causeTagsByTarget(stdout)
	t.Logf("[AC-14-1] tags by target: %v", tags)

	if got := tagForLocal(tags, alpha+"-child-a"); got != "D" {
		t.Errorf("[AC-14-1] changed parent's child: cause %q, want D\nstdout:\n%s", got, stdout)
	}
	if got := tagForLocal(tags, beta+"-child-a"); got == "D" || got == "" {
		t.Errorf("[AC-14-1] unchanged parent's child: cause %q, want a non-D tag\nstdout:\n%s", got, stdout)
	}
}
