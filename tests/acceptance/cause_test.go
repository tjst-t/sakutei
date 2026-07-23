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

// cause_test.go contains CLI-level acceptance tests for Story Scb0ecd-2.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout/stderr, exit code, and side effects.  No internal
// package is imported directly; only the binary's observable behaviour is
// verified.
//
// AC tags map to scenario-Scb0ecd-2.json and ROADMAP.json:
//
//	AC-Scb0ecd-2-1  sakutei plan shows cause: A when intent_revision changes
//	AC-Scb0ecd-2-2  cause: B when reality drifts and intent/revision are stable
//	AC-Scb0ecd-2-3  cause: C when provider_version changes; ProviderUpgradeDiff event logged; reconcile does NOT apply
//	AC-Scb0ecd-2-4  D/E seam: rendered_revision == intent_revision in v1a (E never fires); UpstreamChanged false (D never fires)
//
// [AC-Scb0ecd-2-M] marks the machine-executable scenario tests required by the story.
package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// runPlanCombined runs "sakutei plan ..." and returns the combined
// stdout+stderr output, the separate stderr, and exit code.  Unlike
// runPlan, this captures stderr even when the command exits 0.
// Used in cause-tag tests where ProviderUpgradeDiff is written to stderr.
func runPlanCombined(t *testing.T, intentDir, statePath string, extraArgs ...string) (combined, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)
	args := []string{"plan", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)
	cmd := exec.Command(bin, args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		}
	}
	return stdoutBuf.String() + stderrBuf.String(), stderrBuf.String(), code
}

// runReconcileCombined runs "sakutei reconcile ..." and returns combined
// stdout+stderr output, the separate stderr, and exit code.
func runReconcileCombined(t *testing.T, intentDir, statePath string, extraArgs ...string) (combined, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)
	args := []string{"reconcile", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)
	cmd := exec.Command(bin, args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		}
	}
	return stdoutBuf.String() + stderrBuf.String(), stderrBuf.String(), code
}

// extractCauseTags parses the cause tag values from sakutei plan/reconcile
// stdout.  It looks for lines of the form:
//
//	  cause:  A
//
// and returns all found cause letter strings.
func extractCauseTags(output string) []string {
	var tags []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "cause:") {
			continue
		}
		parts := strings.Fields(trimmed)
		if len(parts) >= 2 {
			tags = append(tags, parts[1])
		}
	}
	return tags
}

// allTagsEqual returns true if all tags in the slice equal the given value.
func allTagsEqual(tags []string, want string) bool {
	if len(tags) == 0 {
		return false
	}
	for _, t := range tags {
		if t != want {
			return false
		}
	}
	return true
}

// anyTagEquals returns true if any tag in the slice equals the given value.
func anyTagEquals(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// testProviderVersion is the default provider_version written by seedState and
// initStateFile for tests that are NOT testing C (ghost diff) detection.
// Scb0ecd-3: the fail-closed gate requires a non-empty provider_version; all
// tests that do not explicitly test version changes must supply one.
const testProviderVersion = "v-test-1.0.0"

// seedState writes a fake-provider state JSON with the given resource fields.
// This simulates "reality already has this resource" without going through Apply.
//
// Scb0ecd-3: seedState always writes testProviderVersion so that the
// fail-closed provider_version validation passes in tests that are NOT testing
// C (ghost-diff) detection.  Tests that DO exercise provider_version changes
// call setProviderVersion explicitly to override.
func seedState(t *testing.T, statePath string, resources map[string]map[string]any) {
	t.Helper()
	sf := map[string]any{
		"resources": func() map[string]any {
			m := make(map[string]any, len(resources))
			for id, fields := range resources {
				m[id] = map[string]any{"fields": fields}
			}
			return m
		}(),
		"provider_version": testProviderVersion,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedState: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("seedState: write: %v", err)
	}
}

// initStateFile creates a minimal state file with no resources but a valid
// provider_version.  Used by tests that start with an empty reality but still
// need the fail-closed gate to pass (Scb0ecd-3).
func initStateFile(t *testing.T, statePath string) {
	t.Helper()
	seedState(t, statePath, nil)
}

// seedStateNoVersion writes a fake-provider state JSON with the given resource
// fields but WITHOUT setting provider_version.  Used exclusively by fail-closed
// tests (TestProviderVersion_FailClosed_*) that need the gate to fire.
func seedStateNoVersion(t *testing.T, statePath string, resources map[string]map[string]any) {
	t.Helper()
	sf := map[string]any{
		"resources": func() map[string]any {
			m := make(map[string]any, len(resources))
			for id, fields := range resources {
				m[id] = map[string]any{"fields": fields}
			}
			return m
		}(),
		// provider_version intentionally absent — triggers fail-closed gate.
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStateNoVersion: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateNoVersion: write: %v", err)
	}
}

// setProviderVersion writes the provider_version field into an existing state
// file (or creates a minimal one).  Used to inject a version change for C tests.
func setProviderVersion(t *testing.T, statePath, version string) {
	t.Helper()
	// Read existing state (if any).
	var sf map[string]any
	data, err := os.ReadFile(statePath)
	if err == nil && len(data) > 0 {
		if jsonErr := json.Unmarshal(data, &sf); jsonErr != nil {
			t.Fatalf("setProviderVersion: unmarshal existing state: %v", jsonErr)
		}
	} else {
		sf = map[string]any{"resources": map[string]any{}}
	}
	sf["provider_version"] = version
	out, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("setProviderVersion: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, out, 0600); err != nil {
		t.Fatalf("setProviderVersion: write: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-1: cause: A when intent_revision changes
// ─────────────────────────────────────────────────────────────────────────────

// TestCauseTag_A_IntentChange verifies that when the intent file changes between
// two invocations, the diff Op shows cause: A.
//
// [AC-Scb0ecd-2-1] [AC-Scb0ecd-2-M]
func TestCauseTag_A_IntentChange(t *testing.T) {
	// [AC-Scb0ecd-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	// Cycle 1: initial intent.  Reality is empty → Create.  First-run →
	// prev intent revision is "" so intent always "changed" → tag A.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "vm-a-test",
				"fields":   map[string]string{"name": "vm-a-test", "size": "10"},
			},
		},
	})
	initStateFile(t, statePath)
	stdout1, stderr1, code1 := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code1 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-1] first plan exited %d; stderr: %s", code1, stderr1)
	}
	tags1 := extractCauseTags(stdout1)
	if !allTagsEqual(tags1, "A") {
		t.Errorf("[AC-Scb0ecd-2-1] first run (first-run → A): got cause tags %v; want all A\nstdout:\n%s",
			tags1, stdout1)
	}
	t.Logf("[AC-Scb0ecd-2-1] cycle 1 tags: %v (stdout truncated)", tags1)

	// Seed reality so that the resource now exists (matching cycle-1 desired).
	seedState(t, statePath, map[string]map[string]any{
		"vm-a-test": {"name": "vm-a-test", "size": "10"},
	})

	// Cycle 2: same intent → converged → no ops.  Confirm cause-state was updated.
	stdout2, _, code2 := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code2 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-1] second plan (converged) exited %d; stdout: %s", code2, stdout2)
	}
	if !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[AC-Scb0ecd-2-1] second plan should converge; got:\n%s", stdout2)
	}

	// Cycle 3: change a field.  intent_revision changes → tag A.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "vm-a-test",
				"fields":   map[string]string{"name": "vm-a-test", "size": "20"}, // changed
			},
		},
	})
	stdout3, stderr3, code3 := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code3 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-1] third plan exited %d; stderr: %s", code3, stderr3)
	}
	tags3 := extractCauseTags(stdout3)
	if !allTagsEqual(tags3, "A") {
		t.Errorf("[AC-Scb0ecd-2-1] intent changed → expected all cause: A; got %v\nstdout:\n%s",
			tags3, stdout3)
	}
	t.Logf("[AC-Scb0ecd-2-1] PASS: cycle 3 after intent change tags: %v", tags3)
}

// TestCauseTag_A_ReconcileIntentChange verifies cause: A on sakutei reconcile
// when intent_revision changes.
//
// [AC-Scb0ecd-2-1] [AC-Scb0ecd-2-M]
func TestCauseTag_A_ReconcileIntentChange(t *testing.T) {
	// [AC-Scb0ecd-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	// Initial reconcile: creates the resource (first-run → A).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "rec-a-vm",
				"fields":   map[string]string{"name": "rec-a-vm", "size": "5"},
			},
		},
	})
	initStateFile(t, statePath)
	stdout1, _, code1 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code1 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-1] first reconcile exited %d", code1)
	}
	tags1 := extractCauseTags(stdout1)
	if !allTagsEqual(tags1, "A") {
		t.Errorf("[AC-Scb0ecd-2-1] first reconcile: expected all cause: A; got %v\nstdout:\n%s",
			tags1, stdout1)
	}

	// Second reconcile with changed intent → cause: A.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "rec-a-vm",
				"fields":   map[string]string{"name": "rec-a-vm", "size": "15"}, // changed
			},
		},
	})
	stdout2, stderr2, code2 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code2 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-1] second reconcile (intent changed) exited %d; stderr: %s",
			code2, stderr2)
	}
	tags2 := extractCauseTags(stdout2)
	if !allTagsEqual(tags2, "A") {
		t.Errorf("[AC-Scb0ecd-2-1] reconcile with intent change: expected all cause: A; got %v\nstdout:\n%s",
			tags2, stdout2)
	}
	t.Logf("[AC-Scb0ecd-2-1] PASS: reconcile intent-change tags: %v", tags2)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-2: cause: B when reality drifts, intent/revisions stable
// ─────────────────────────────────────────────────────────────────────────────

// TestCauseTag_B_RealityDrift verifies that when the intent is unchanged but
// the fake provider's state (reality) is directly modified to drift from
// desired, the Op gets cause: B.
//
// [AC-Scb0ecd-2-2] [AC-Scb0ecd-2-M]
func TestCauseTag_B_RealityDrift(t *testing.T) {
	// [AC-Scb0ecd-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	// Cycle 1: reconcile to create the resource (establishes intent_revision in
	// cause-state).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "drift-vm",
				"fields":   map[string]string{"name": "drift-vm", "size": "10"},
			},
		},
	})
	initStateFile(t, statePath)
	stdout1, _, code1 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code1 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-2] setup reconcile exited %d; stdout: %s", code1, stdout1)
	}

	// Directly modify reality to introduce drift (size: 10 → 99).
	// intent_revision, rendered_revision, provider_version are all unchanged.
	seedState(t, statePath, map[string]map[string]any{
		"drift-vm": {"name": "drift-vm", "size": "99"}, // drifted
	})

	// Cycle 2: plan with SAME intent.  Diff exists (size 99 vs 10).
	// Classification: intent unchanged → NOT A; provider_version unchanged → NOT C;
	// rendered unchanged → NOT E; upstream unchanged → NOT D.
	// By elimination: B (drift).
	stdout2, stderr2, code2 := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code2 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-2] drift plan exited %d; stderr: %s", code2, stderr2)
	}
	if !strings.Contains(stdout2, "UPDATE") {
		t.Fatalf("[AC-Scb0ecd-2-2] expected UPDATE op in plan output; got:\n%s", stdout2)
	}
	tags := extractCauseTags(stdout2)
	if !allTagsEqual(tags, "B") {
		t.Errorf("[AC-Scb0ecd-2-2] drift with stable intent: expected all cause: B; got %v\nstdout:\n%s",
			tags, stdout2)
	}
	t.Logf("[AC-Scb0ecd-2-2] PASS: drift tags: %v", tags)
}

// TestCauseTag_B_ReconcileDrift verifies cause: B on sakutei reconcile when
// reality drifts with stable intent.
//
// [AC-Scb0ecd-2-2] [AC-Scb0ecd-2-M]
func TestCauseTag_B_ReconcileDrift(t *testing.T) {
	// [AC-Scb0ecd-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	intent := map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "rec-drift-vm",
				"fields":   map[string]string{"name": "rec-drift-vm", "size": "5"},
			},
		},
	}
	writeIntentFile(t, intentDir, intent)

	initStateFile(t, statePath)
	// Cycle 1: reconcile to establish cause-state.
	_, _, code1 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code1 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-2] setup reconcile failed")
	}

	// Drift reality directly.
	seedState(t, statePath, map[string]map[string]any{
		"rec-drift-vm": {"name": "rec-drift-vm", "size": "42"}, // drifted
	})

	// Cycle 2: reconcile with same intent.  Should show cause: B and apply.
	stdout2, _, code2 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code2 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-2] drift reconcile exited %d; stdout: %s", code2, stdout2)
	}
	tags := extractCauseTags(stdout2)
	if !allTagsEqual(tags, "B") {
		t.Errorf("[AC-Scb0ecd-2-2] drift reconcile: expected all cause: B; got %v\nstdout:\n%s",
			tags, stdout2)
	}
	t.Logf("[AC-Scb0ecd-2-2] PASS: reconcile drift tags: %v", tags)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-3: cause: C when provider_version changes (ghost diff)
// ─────────────────────────────────────────────────────────────────────────────

// TestCauseTag_C_ProviderVersionChange verifies that when provider_version
// changes between two invocations, the diff Op gets cause: C, a
// ProviderUpgradeDiff event is emitted to stderr, and reconcile does NOT apply
// the change (ghost-diff gate, §10.4, task Scb0ecd-2-2).
//
// [AC-Scb0ecd-2-3] [AC-Scb0ecd-2-M]
func TestCauseTag_C_ProviderVersionChange(t *testing.T) {
	// [AC-Scb0ecd-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	// Seed reality: resource exists and matches desired.
	seedState(t, statePath, map[string]map[string]any{
		"ghost-vm": {"name": "ghost-vm", "size": "10"},
	})
	// Set initial provider version in state file.
	setProviderVersion(t, statePath, "v1.0.0")

	intent := map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "ghost-vm",
				"fields":   map[string]string{"name": "ghost-vm", "size": "10"},
			},
		},
	}
	writeIntentFile(t, intentDir, intent)

	// Cycle 1: reconcile.  Resource is already converged (reality matches desired).
	// provider_version = v1.0.0 is saved to cause-state.
	combined1, _, code1 := runReconcileCombined(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code1 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-3] setup reconcile exited %d; output: %s", code1, combined1)
	}
	// Should converge cleanly (no diff yet).
	if !strings.Contains(combined1, "Converged (no changes)") {
		// May have applied; that's fine as long as cause-state was written.
		t.Logf("[AC-Scb0ecd-2-3] setup cycle did not converge immediately: %s", combined1)
	}

	// Simulate provider upgrade: change provider_version in state file.
	// intent and reality are both unchanged (size=10 in both).
	// This models a provider that now normalises "10" differently but produces
	// the same external effect.  The diff engine will see a diff because the
	// provider's interpretation changed.
	setProviderVersion(t, statePath, "v2.0.0") // upgraded

	// To create an actual diff for the ghost diff test, we need the reality to
	// differ from desired slightly.  In the fake provider, provider_version does
	// not actually change diff computation — the fake is intentionally dumb.
	// So we inject a field-level drift to create a diff, then the cause-tag
	// classification kicks in and tags it C because provider_version changed.
	seedState(t, statePath, map[string]map[string]any{
		"ghost-vm": {"name": "ghost-vm", "size": "11"}, // drift to force diff
	})
	// Keep the updated provider_version in state.
	setProviderVersion(t, statePath, "v2.0.0")

	// Cycle 2: plan (combined output to capture stderr even on exit 0).
	// Diff exists (size 11 vs 10).
	// Classification: provider_version changed (v1.0.0 → v2.0.0) → tag C.
	combined2, stderr2, code2 := runPlanCombined(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code2 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-3] ghost-diff plan exited %d; output: %s", code2, combined2)
	}
	tags2 := extractCauseTags(combined2)
	if !anyTagEquals(tags2, "C") {
		t.Errorf("[AC-Scb0ecd-2-3] provider_version changed: expected cause: C; got %v\noutput:\n%s",
			tags2, combined2)
	}
	// ProviderUpgradeDiff event must appear in stderr.
	if !strings.Contains(stderr2, "ProviderUpgradeDiff") {
		t.Errorf("[AC-Scb0ecd-2-3] expected ProviderUpgradeDiff in stderr; got:\n%s", stderr2)
	}
	t.Logf("[AC-Scb0ecd-2-3] plan cause tags: %v, ProviderUpgradeDiff in stderr: %v",
		tags2, strings.Contains(stderr2, "ProviderUpgradeDiff"))

	// Reset cause-state from cycle 1 again to test reconcile path independently.
	// (Re-establish prev_provider_version = v1.0.0 so cycle compares v1→v2.)
	// We do this by writing to cause-state directly.
	prevCauseState := `{"prev_intent_revision":"","prev_rendered_revision":"","prev_provider_version":"v1.0.0"}`
	if err := os.WriteFile(causeStatePath, []byte(prevCauseState), 0600); err != nil {
		t.Fatal(err)
	}

	// Re-seed reality with drift so diff is produced.
	seedState(t, statePath, map[string]map[string]any{
		"ghost-vm": {"name": "ghost-vm", "size": "11"},
	})
	setProviderVersion(t, statePath, "v2.0.0")

	// Cycle 2 (reconcile path): cause: C → should NOT apply (ghost diff gate).
	stateBeforeBytes, _ := os.ReadFile(statePath)
	combined3, stderr3, code3 := runReconcileCombined(t, intentDir, statePath, "--cause-state", causeStatePath)
	// reconcile should exit 0 (ghost diff is not an error; it's a warning)
	// or exit non-zero if it treats C-only ops as "incomplete".
	// Per design (Scb0ecd-2-2): C ops are filtered out → no Apply for ghost
	// diffs → reconcile has nothing to apply → "Converged (no changes)" or
	// no-apply exit.
	t.Logf("[AC-Scb0ecd-2-3] reconcile code=%d output=%s", code3, combined3)

	// Key assertions:
	// 1. ProviderUpgradeDiff event in stderr.
	if !strings.Contains(stderr3, "ProviderUpgradeDiff") {
		t.Errorf("[AC-Scb0ecd-2-3] reconcile: expected ProviderUpgradeDiff in stderr; got:\n%s", stderr3)
	}
	// 2. State file NOT changed (ghost diff was not applied).
	stateAfterBytes, _ := os.ReadFile(statePath)
	// The state file may have had provider_version and other fields; what must
	// NOT have changed is the resource's "size" field (still 11, not corrected
	// to 10).  Full byte equality may differ due to apply_count etc., so we
	// check the resource field value instead.
	if string(stateBeforeBytes) == "" {
		t.Fatalf("[AC-Scb0ecd-2-3] state file disappeared")
	}
	// The resource must still have size=11 (ghost diff was not applied → size
	// was not corrected to 10).
	if !strings.Contains(string(stateAfterBytes), `"11"`) {
		t.Errorf("[AC-Scb0ecd-2-3] cause: C op should NOT have been applied; state:\n%s\nbefore:\n%s",
			stateAfterBytes, stateBeforeBytes)
	}
	t.Logf("[AC-Scb0ecd-2-3] PASS: ghost diff not applied; ProviderUpgradeDiff in stderr")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-4: D/E seam correctness (v1a: never fires)
// ─────────────────────────────────────────────────────────────────────────────

// TestCauseTag_E_SeamNeverFiresInV1a verifies that in v1a, where
// rendered_revision == intent_revision (no external inputs), cause: E is never
// produced.  This validates the seam contract: E is structurally present (the
// code path exists) but dormant until S072a14 wires real external inputs.
//
// [AC-Scb0ecd-2-4] [AC-Scb0ecd-2-M]
func TestCauseTag_E_SeamNeverFiresInV1a(t *testing.T) {
	// [AC-Scb0ecd-2-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	// Setup: reconcile to establish cause-state.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "seam-vm",
				"fields":   map[string]string{"name": "seam-vm", "size": "3"},
			},
		},
	})
	initStateFile(t, statePath)
	_, _, code0 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code0 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-4] setup reconcile failed")
	}

	// Introduce drift (to force a diff) without changing intent.
	seedState(t, statePath, map[string]map[string]any{
		"seam-vm": {"name": "seam-vm", "size": "99"},
	})

	// The revision header must show rendered_revision == intent_revision (v1a).
	stdout, _, code := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code != 0 {
		t.Fatalf("[AC-Scb0ecd-2-4] plan exited %d", code)
	}

	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev != renderedRev {
		t.Errorf("[AC-Scb0ecd-2-4] v1a: rendered_revision must equal intent_revision; got intent=%s rendered=%s",
			intentRev, renderedRev)
	}

	// With rendered == intent, even a drift diff must NOT produce cause: E.
	tags := extractCauseTags(stdout)
	if anyTagEquals(tags, "E") {
		t.Errorf("[AC-Scb0ecd-2-4] v1a: cause: E must not fire (rendered == intent); got %v\nstdout:\n%s",
			tags, stdout)
	}
	// The drift should produce cause: B (not E, not A).
	if !anyTagEquals(tags, "B") {
		t.Errorf("[AC-Scb0ecd-2-4] expected cause: B for drift (not E) in v1a; got %v\nstdout:\n%s",
			tags, stdout)
	}
	t.Logf("[AC-Scb0ecd-2-4] PASS: v1a seam — rendered==intent, E never fires, got tags: %v", tags)
}

// TestCauseTag_D_SeamNeverFiresInV1a verifies that in v1a, cause: D (cascade)
// is never produced because UpstreamChanged is always false.
//
// [AC-Scb0ecd-2-4] [AC-Scb0ecd-2-M]
func TestCauseTag_D_SeamNeverFiresInV1a(t *testing.T) {
	// [AC-Scb0ecd-2-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	causeStatePath := filepath.Join(tmp, "cause-state.json")

	// Two resources with a dependency chain.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "d-upstream",
				"fields":   map[string]string{"name": "d-upstream", "size": "1"},
			},
			{
				"provider":   "fake",
				"id":         "d-downstream",
				"depends_on": []string{"d-upstream"},
				"fields":     map[string]string{"name": "d-downstream", "size": "2"},
			},
		},
	})

	initStateFile(t, statePath)
	// Cycle 1: reconcile to establish cause-state.
	_, _, code0 := runReconcile(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code0 != 0 {
		t.Fatalf("[AC-Scb0ecd-2-4] setup reconcile failed (code=%d)", code0)
	}

	// Drift downstream only (upstream is converged).
	seedState(t, statePath, map[string]map[string]any{
		"d-upstream":   {"name": "d-upstream", "size": "1"},
		"d-downstream": {"name": "d-downstream", "size": "99"}, // drifted
	})

	// Cycle 2: plan with same intent.  Downstream diff should be cause: B
	// (drift), NOT D (cascade seam is false in v1a).
	stdout, _, code := runPlan(t, intentDir, statePath, "--cause-state", causeStatePath)
	if code != 0 {
		t.Fatalf("[AC-Scb0ecd-2-4] drift plan exited %d", code)
	}
	tags := extractCauseTags(stdout)
	if anyTagEquals(tags, "D") {
		t.Errorf("[AC-Scb0ecd-2-4] v1a: cause: D must not fire (UpstreamChanged seam is false); got %v\nstdout:\n%s",
			tags, stdout)
	}
	t.Logf("[AC-Scb0ecd-2-4] PASS: D seam never fires in v1a; tags: %v", tags)
}
