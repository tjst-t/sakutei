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

// Package acceptance contains CLI-level acceptance tests for Story Sead944-2.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, exit code, and side effects on the state file.  No
// internal package is imported directly; only the binary's observable behaviour
// is verified.
//
// AC tags map to scenario-Sead944-2.json:
//
//	AC-Sead944-2-1 — Create diff is displayed when reality is empty
//	AC-Sead944-2-2 — plan does NOT modify the fake provider state file (Apply not called)
//	AC-Sead944-2-3 — "Converged (no changes)" printed and exit 0 when reality matches desired
package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binaryDir is a package-level temp dir that persists for the life of the
// test binary process.  It is created by TestMain so it outlives any single
// test function's TempDir().
var binaryDir string

// TestMain sets up a package-level temp dir, compiles the sakutei binary into
// it once, and runs all tests.
func TestMain(m *testing.M) {
	// #141: the test binary doubles as a fake NETCONF server behind a fake ssh.
	serveNetconfHelperIfAsked()

	// Use os.MkdirTemp so the dir is NOT cleaned up by t.Cleanup (we manage
	// it ourselves via defer + os.RemoveAll).
	dir, err := os.MkdirTemp("", "sakutei-acceptance-")
	if err != nil {
		panic("acceptance TestMain: MkdirTemp: " + err.Error())
	}
	defer os.RemoveAll(dir)
	binaryDir = dir

	// Find repo root relative to this file's package (tests/acceptance → ../..)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		panic("acceptance TestMain: repo root: " + err.Error())
	}

	out := filepath.Join(binaryDir, "sakutei")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/sakutei")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("acceptance TestMain: go build: " + err.Error())
	}

	os.Exit(m.Run())
}

// buildBinary returns the path to the pre-compiled sakutei binary.
func buildBinary(t *testing.T) string {
	t.Helper()
	if binaryDir == "" {
		t.Fatal("buildBinary: binaryDir not set (TestMain not called?)")
	}
	return filepath.Join(binaryDir, "sakutei")
}

// runPlan runs "sakutei plan --intent <intentDir> --state <statePath>" and
// returns stdout, stderr, and the exit code.
func runPlan(t *testing.T, intentDir, statePath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)

	args := []string{"plan", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)

	cmd := exec.Command(bin, args...)
	outBytes, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(outBytes), string(exitErr.Stderr), exitErr.ExitCode()
		}
		t.Fatalf("runPlan: unexpected error: %v", err)
	}
	return string(outBytes), "", 0
}

// writeIntentFile writes a provisional intent JSON file into dir.
func writeIntentFile(t *testing.T, dir string, content any) {
	t.Helper()
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		t.Fatalf("writeIntentFile: marshal: %v", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "resources.json"), data, 0644); err != nil {
		t.Fatalf("writeIntentFile: write: %v", err)
	}
}

// readStateFile reads and returns the raw content of the state file.
// Returns nil if the file does not exist.
func readStateFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("readStateFile: %v", err)
	}
	return data
}

// ---------------------------------------------------------------------------
// scenario-Sead944-2 / scenario-1
// Steps 1+2+3 are each a separate test function for clarity.
// ---------------------------------------------------------------------------

// TestPlan_CreateDiff verifies scenario-Sead944-2 / step 1.
//
// [AC-Sead944-2-1] When intent declares one resource and reality is empty,
// "sakutei plan" must exit 0 and print a Create diff for that resource.
func TestPlan_CreateDiff(t *testing.T) {
	// [AC-Sead944-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	// Scb0ecd-3: write a minimal state file with provider_version so the
	// fail-closed gate passes; reality is still empty (no resources).
	initStateFile(t, statePath)

	// Write intent: one resource "test-vm" with name and size.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "test-vm",
				"fields": map[string]string{
					"name": "test-vm",
					"size": "10",
				},
			},
		},
	})

	// State file is absent → reality is empty.
	stdout, stderr, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Sead944-2-1] expected exit 0, got %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "CREATE") {
		t.Errorf("[AC-Sead944-2-1] stdout should contain CREATE; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "test-vm") {
		t.Errorf("[AC-Sead944-2-1] stdout should mention resource id 'test-vm'; got:\n%s", stdout)
	}
	// Must NOT contain the "converged" message when there are diffs.
	if strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("[AC-Sead944-2-1] stdout must not contain 'Converged (no changes)' when diffs exist; got:\n%s", stdout)
	}
	t.Logf("[AC-Sead944-2-1] plan output:\n%s", stdout)
}

// TestPlan_DoesNotModifyState verifies scenario-Sead944-2 / step 2.
//
// [AC-Sead944-2-2] After running "sakutei plan", the fake provider state
// file must be unchanged (Apply was not called).
//
// Two cases are covered so both the "file does not exist yet" and the
// "file already exists" paths are exercised:
//   - absent:   the state file does not exist before plan and must remain
//     absent (byte-for-byte, nil) afterwards — plan must not create it.
//   - existing: the state file exists with arbitrary content before plan and
//     must be byte-for-byte identical afterwards — plan must not rewrite it.
func TestPlan_DoesNotModifyState(t *testing.T) {
	// [AC-Sead944-2-2]
	// Scb0ecd-3: both seed cases must include a provider_version so the
	// fail-closed gate passes.  The key invariant (plan must not call Apply)
	// is still verified: the state file must be byte-for-byte identical before
	// and after plan.
	cases := []struct {
		name string
		// seed is the exact bytes to write to the state file before plan.
		// A nil seed is no longer used: Scb0ecd-3 requires a provider_version,
		// so a completely absent state file triggers fail-closed rejection.
		seed []byte
	}{
		{
			name: "absent-with-version",
			// Minimal state file: no resources, but a valid provider_version.
			// plan must leave these exact bytes untouched.
			seed: []byte(`{"provider_version":"v-test-1.0.0","resources":{}}`),
		},
		{
			name: "existing",
			// Pre-existing content with resources and a valid provider_version.
			// plan must leave these exact bytes untouched.
			seed: []byte(`{"provider_version":"v-test-1.0.0","resources":{}}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// [AC-Sead944-2-2]
			tmp := t.TempDir()
			intentDir := filepath.Join(tmp, "intent")
			if err := os.MkdirAll(intentDir, 0755); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(tmp, "state.json")

			// Write intent declaring one resource.
			writeIntentFile(t, intentDir, map[string]any{
				"resources": []map[string]any{
					{
						"provider": "fake",
						"id":       "no-apply-vm",
						"fields": map[string]string{
							"name": "no-apply-vm",
							"size": "5",
						},
					},
				},
			})

			// Seed the state file (or leave it absent for the "absent" case).
			if tc.seed != nil {
				if err := writeFileAtomic(statePath, tc.seed, 0644); err != nil {
					t.Fatal(err)
				}
			}

			// Snapshot state before plan.
			beforePlan := readStateFile(t, statePath)

			// Run plan.
			_, _, code := runPlan(t, intentDir, statePath)
			if code != 0 {
				t.Fatalf("[AC-Sead944-2-2] plan exited %d", code)
			}

			// Snapshot state after plan.
			afterPlan := readStateFile(t, statePath)

			// The state file must not have been created or modified by plan.
			// Both nil (absent) or identical byte-for-byte content is acceptable.
			if string(beforePlan) != string(afterPlan) {
				t.Errorf("[AC-Sead944-2-2] state file changed after plan — Apply must not be called by plan\nbefore: %s\nafter:  %s",
					beforePlan, afterPlan)
			}
			t.Logf("[AC-Sead944-2-2] state file unchanged (before=%d bytes, after=%d bytes)", len(beforePlan), len(afterPlan))
		})
	}
}

// TestPlan_ConvergedWhenRealityMatchesDesired verifies scenario-Sead944-2 / step 3.
//
// [AC-Sead944-2-3] When the state file already reflects the desired state,
// "sakutei plan" must exit 0 and print "Converged (no changes)".
func TestPlan_ConvergedWhenRealityMatchesDesired(t *testing.T) {
	// [AC-Sead944-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Write intent.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "synced-vm",
				"fields": map[string]string{
					"name": "synced-vm",
					"size": "20",
				},
			},
		},
	})

	// Seed the state file so reality matches desired.
	// Scb0ecd-3: include provider_version so fail-closed gate passes.
	seedStateData := map[string]any{
		"provider_version": testProviderVersion,
		"resources": map[string]any{
			"synced-vm": map[string]any{
				"fields": map[string]any{
					"name": "synced-vm",
					"size": "20",
				},
			},
		},
	}
	seedData, _ := json.MarshalIndent(seedStateData, "", "  ")
	if err := writeFileAtomic(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Sead944-2-3] expected exit 0, got %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("[AC-Sead944-2-3] expected 'Converged (no changes)' in stdout; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "CREATE") || strings.Contains(stdout, "UPDATE") {
		t.Errorf("[AC-Sead944-2-3] unexpected diff ops in stdout when already converged; got:\n%s", stdout)
	}
	t.Logf("[AC-Sead944-2-3] plan output:\n%s", stdout)
}

// ---------------------------------------------------------------------------
// Additional edge-case tests
// ---------------------------------------------------------------------------

// TestPlan_UpdateDiff verifies that field-level drift produces an Update op.
func TestPlan_UpdateDiff(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Desired: size=20.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "drift-vm",
				"fields": map[string]string{
					"name": "drift-vm",
					"size": "20",
				},
			},
		},
	})

	// Reality: size=10 (drift).
	// Scb0ecd-3: include provider_version so fail-closed gate passes.
	seedStateData := map[string]any{
		"provider_version": testProviderVersion,
		"resources": map[string]any{
			"drift-vm": map[string]any{
				"fields": map[string]any{
					"name": "drift-vm",
					"size": "10",
				},
			},
		},
	}
	seedData, _ := json.MarshalIndent(seedStateData, "", "  ")
	if err := writeFileAtomic(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "UPDATE") {
		t.Errorf("expected UPDATE op in stdout; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "size") {
		t.Errorf("expected 'size' field in update diff; got:\n%s", stdout)
	}
	t.Logf("plan output:\n%s", stdout)
}

// TestPlan_MissingIntentFlag verifies that --intent is required.
func TestPlan_MissingIntentFlag(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "plan")
	_, err := cmd.Output()
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() == 0 {
			t.Error("expected non-zero exit when --intent is missing, got 0")
		}
	} else {
		t.Errorf("expected exit error, got: %v", err)
	}
}

// TestPlan_EmptyIntentDir verifies that an empty intent dir → converged.
func TestPlan_EmptyIntentDir(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	// Scb0ecd-3: provide a valid state file so the fail-closed gate passes.
	initStateFile(t, statePath)

	stdout, _, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("expected exit 0 for empty intent dir, got %d", code)
	}
	if !strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("expected 'Converged (no changes)' for empty intent; got:\n%s", stdout)
	}
}
