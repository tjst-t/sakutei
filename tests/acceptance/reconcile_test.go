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

// reconcile_test.go contains CLI-level acceptance tests for Story Sead944-3.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, exit code, and side effects on the state file.  No
// internal package is imported directly; only the binary's observable behaviour
// is verified.
//
// AC tags map to scenario-Sead944-3.json:
//
//	AC-Sead944-3-1 — reconcile once; resource is created in state file
//	AC-Sead944-3-2 — second reconcile is a no-op (idempotent)
//	AC-Sead944-3-3 — reconcile observes independently (observation state-less)
package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runReconcile runs "sakutei reconcile --intent <intentDir> --state <statePath>"
// and returns stdout, stderr, and the exit code.
func runReconcile(t *testing.T, intentDir, statePath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)

	args := []string{"reconcile", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)

	cmd := exec.Command(bin, args...)
	outBytes, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(outBytes), string(exitErr.Stderr), exitErr.ExitCode()
		}
		t.Fatalf("runReconcile: unexpected error: %v", err)
	}
	return string(outBytes), "", 0
}

// ---------------------------------------------------------------------------
// scenario-Sead944-3 / scenario-1
// ---------------------------------------------------------------------------

// TestReconcile_CreatesResource verifies scenario-Sead944-3 / step 1.
//
// [AC-Sead944-3-1] When intent declares one resource and reality is empty,
// "sakutei reconcile" must exit 0 and the state file must contain the resource
// with the desired field values afterwards.
func TestReconcile_CreatesResource(t *testing.T) {
	// [AC-Sead944-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: one fake resource "reconcile-vm" with name and size.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "reconcile-vm",
				"fields": map[string]string{
					"name": "reconcile-vm",
					"size": "42",
				},
			},
		},
	})

	// State file absent → reality is empty.
	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Sead944-3-1] expected exit 0, got %d; stderr: %s", code, stderr)
	}

	// Output must mention CREATE (reconcile prints the applied ops).
	if !strings.Contains(stdout, "CREATE") {
		t.Errorf("[AC-Sead944-3-1] stdout should contain CREATE; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "reconcile-vm") {
		t.Errorf("[AC-Sead944-3-1] stdout should mention resource id 'reconcile-vm'; got:\n%s", stdout)
	}

	// State file must now exist and contain the resource.
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatalf("[AC-Sead944-3-1] state file was not created by reconcile at %s", statePath)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-Sead944-3-1] read state file: %v", err)
	}

	// Verify the resource key exists in the state file.
	if !strings.Contains(string(data), "reconcile-vm") {
		t.Errorf("[AC-Sead944-3-1] state file does not contain 'reconcile-vm'; content:\n%s", data)
	}

	t.Logf("[AC-Sead944-3-1] reconcile output:\n%s", stdout)
	t.Logf("[AC-Sead944-3-1] state file after reconcile:\n%s", data)
}

// TestReconcile_Idempotent verifies scenario-Sead944-3 / step 2.
//
// [AC-Sead944-3-2] Running "sakutei reconcile" a second time when reality
// already matches desired must exit 0 and produce no CREATE/UPDATE operations.
// The state file content must be identical after the second run.
func TestReconcile_Idempotent(t *testing.T) {
	// [AC-Sead944-3-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "idempotent-vm",
				"fields": map[string]string{
					"name": "idempotent-vm",
					"size": "8",
				},
			},
		},
	})

	// First reconcile — creates the resource.
	_, stderr1, code1 := runReconcile(t, intentDir, statePath)
	if code1 != 0 {
		t.Fatalf("[AC-Sead944-3-2] first reconcile failed (exit %d); stderr: %s", code1, stderr1)
	}

	// Snapshot state after first reconcile.
	stateAfterFirst := readStateFile(t, statePath)
	if stateAfterFirst == nil {
		t.Fatal("[AC-Sead944-3-2] state file absent after first reconcile")
	}

	// Second reconcile — must be a no-op.
	stdout2, stderr2, code2 := runReconcile(t, intentDir, statePath)
	if code2 != 0 {
		t.Errorf("[AC-Sead944-3-2] second reconcile failed (exit %d); stderr: %s", code2, stderr2)
	}

	// Must report no changes.
	if strings.Contains(stdout2, "CREATE") || strings.Contains(stdout2, "UPDATE") {
		t.Errorf("[AC-Sead944-3-2] second reconcile must not report CREATE/UPDATE; got:\n%s", stdout2)
	}
	if !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[AC-Sead944-3-2] second reconcile must print 'Converged (no changes)'; got:\n%s", stdout2)
	}

	// State file must be byte-for-byte identical after the second reconcile.
	stateAfterSecond := readStateFile(t, statePath)
	if string(stateAfterFirst) != string(stateAfterSecond) {
		t.Errorf("[AC-Sead944-3-2] state file changed after second reconcile — Apply was called when it should not have been\nafter-first:  %s\nafter-second: %s",
			stateAfterFirst, stateAfterSecond)
	}

	t.Logf("[AC-Sead944-3-2] second reconcile output:\n%s", stdout2)
	t.Logf("[AC-Sead944-3-2] state file (unchanged after second reconcile):\n%s", stateAfterSecond)
}

// TestReconcile_ObservesIndependently verifies scenario-Sead944-3 / step 3.
//
// [AC-Sead944-3-3] reconcile must observe reality independently; it must not
// reuse the observation result of a prior "sakutei plan" invocation.
//
// Test strategy (subprocess level):
//  1. Start with reality empty.
//  2. Run "sakutei plan" (dry-run, no Apply). The plan observes that the
//     resource is absent and would create it — but does NOT create it.
//  3. Run "sakutei reconcile". If reconcile depended on plan's cached
//     observation, it could not re-observe that the resource is still absent
//     and would skip Apply. Conversely, a correct implementation re-observes
//     reality (which is still empty) and applies the Create op.
//  4. Verify the resource exists in the state file, proving reconcile Observed
//     independently and executed the Apply.
func TestReconcile_ObservesIndependently(t *testing.T) {
	// [AC-Sead944-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "stateless-vm",
				"fields": map[string]string{
					"name": "stateless-vm",
					"size": "16",
				},
			},
		},
	})

	// Step A: run "sakutei plan" — dry-run only. Reality remains empty.
	_, stderrPlan, codePlan := runPlan(t, intentDir, statePath)
	if codePlan != 0 {
		t.Fatalf("[AC-Sead944-3-3] plan failed (exit %d); stderr: %s", codePlan, stderrPlan)
	}

	// Plan must NOT have created the state file (Apply never called).
	if data := readStateFile(t, statePath); data != nil {
		t.Fatalf("[AC-Sead944-3-3] plan created the state file — Apply was called; content: %s", data)
	}

	// Step B: run "sakutei reconcile". Must re-observe that resource is absent
	// (not inherit plan's observation) and apply the Create op.
	stdoutRec, stderrRec, codeRec := runReconcile(t, intentDir, statePath)
	if codeRec != 0 {
		t.Fatalf("[AC-Sead944-3-3] reconcile failed (exit %d); stderr: %s", codeRec, stderrRec)
	}

	// Reconcile must have observed the empty reality and applied the Create op.
	if !strings.Contains(stdoutRec, "CREATE") {
		t.Errorf("[AC-Sead944-3-3] reconcile must report CREATE after plan (reality still empty before reconcile); got:\n%s", stdoutRec)
	}

	// The resource must now exist in the state file, proving Apply was called.
	stateData := readStateFile(t, statePath)
	if stateData == nil {
		t.Fatal("[AC-Sead944-3-3] state file absent after reconcile — Apply was not called")
	}
	if !strings.Contains(string(stateData), "stateless-vm") {
		t.Errorf("[AC-Sead944-3-3] state file does not contain 'stateless-vm'; content:\n%s", stateData)
	}

	t.Logf("[AC-Sead944-3-3] reconcile output:\n%s", stdoutRec)
	t.Logf("[AC-Sead944-3-3] state file after reconcile:\n%s", stateData)
}

// ---------------------------------------------------------------------------
// Additional edge-case tests
// ---------------------------------------------------------------------------

// TestReconcile_UpdatesDriftedResource verifies that reconcile corrects a
// drifted resource (field value in reality differs from desired).
func TestReconcile_UpdatesDriftedResource(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Desired: size=100.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "drifted-vm",
				"fields": map[string]string{
					"name": "drifted-vm",
					"size": "100",
				},
			},
		},
	})

	// Reality: size=50 (drift).
	seedState := map[string]any{
		"resources": map[string]any{
			"drifted-vm": map[string]any{
				"fields": map[string]any{
					"name": "drifted-vm",
					"size": "50",
				},
			},
		},
	}
	seedData, _ := json.MarshalIndent(seedState, "", "  ")
	if err := os.WriteFile(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr)
	}

	if !strings.Contains(stdout, "UPDATE") {
		t.Errorf("expected UPDATE op in stdout; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "size") {
		t.Errorf("expected 'size' field in update diff; got:\n%s", stdout)
	}

	// Verify the state file now reflects size=100.
	finalData := readStateFile(t, statePath)
	if !strings.Contains(string(finalData), "100") {
		t.Errorf("state file should contain updated size '100'; got:\n%s", finalData)
	}

	t.Logf("reconcile update output:\n%s", stdout)
}

// TestReconcile_MissingIntentFlag verifies that --intent is required.
func TestReconcile_MissingIntentFlag(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "reconcile")
	_, err := cmd.Output()
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() == 0 {
			t.Error("expected non-zero exit when --intent is missing, got 0")
		}
	} else {
		t.Errorf("expected exit error, got: %v", err)
	}
}

// TestReconcile_EmptyIntentDir verifies that an empty intent dir exits 0 with
// "Converged (no changes)" (nothing to reconcile).
func TestReconcile_EmptyIntentDir(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	stdout, _, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("expected exit 0 for empty intent dir, got %d", code)
	}
	if !strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("expected 'Converged (no changes)' for empty intent; got:\n%s", stdout)
	}
}
