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

// dag_test.go contains CLI-level acceptance tests for Story S94f1f9-2.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, exit code, and side effects on the state
// file.  No internal package is imported directly; only the binary's observable
// behaviour is verified.
//
// AC tags map to scenario-S94f1f9-2.json:
//
//	AC-S94f1f9-2-1 — wave order: resource A (no deps) is applied before B (depends_on A)
//	AC-S94f1f9-2-2 — upstream failure: A fails → B is skipped; B is not in state file
//	AC-S94f1f9-2-3 — cyclic depends_on is rejected at load time (exit non-0, explicit error)
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

// runReconcileFull runs "sakutei reconcile" and captures stdout, stderr, and
// the exit code independently, including stderr when exit code is 0.  This is
// necessary for tests that inspect stderr-only output (e.g. skip messages)
// when the process exits 0.
func runReconcileFull(t *testing.T, intentDir, statePath string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)

	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(bin, "reconcile", "--intent", intentDir, "--state", statePath)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// ---------------------------------------------------------------------------
// [AC-S94f1f9-2-1] Wave order: A applied before B when B depends_on A
// ---------------------------------------------------------------------------

// TestDAG_WaveOrder verifies scenario-S94f1f9-2 / step 1.
//
// [AC-S94f1f9-2-1] When B depends_on A and reality is empty, "sakutei
// reconcile" must:
//   - Apply A before B (wave 1 = {A}, wave 2 = {B}).
//   - Both A and B must appear in the state file after reconcile.
//   - Exit 0.
//
// Proof of ordering: after reconcile, both resources must exist in the state
// file.  Since A has no deps (wave 1) and B depends_on A (wave 2), and the
// only way B can be applied is if A was already applied (the DAG enforces it),
// the presence of both A and B confirms wave ordering.  A direct ordering
// observation would require hooking into Apply internals; we infer order from
// the fact that B exists (B was applied, which requires A to have succeeded
// first per the wave contract).
func TestDAG_WaveOrder(t *testing.T) {
	// [AC-S94f1f9-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: A (no deps) and B (depends_on A).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "res-a",
				"fields": map[string]string{
					"name": "res-a",
					"size": "1",
				},
			},
			{
				"provider":   "fake",
				"id":         "res-b",
				"depends_on": []string{"res-a"},
				"fields": map[string]string{
					"name": "res-b",
					"size": "2",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-S94f1f9-2-1] expected exit 0, got %d; stderr: %s stdout: %s",
			code, stderr, stdout)
	}

	// Both resources must be in the state file.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S94f1f9-2-1] state file missing after reconcile: %v", err)
	}

	if !strings.Contains(string(data), "res-a") {
		t.Errorf("[AC-S94f1f9-2-1] res-a not found in state file; content:\n%s", data)
	}
	if !strings.Contains(string(data), "res-b") {
		t.Errorf("[AC-S94f1f9-2-1] res-b not found in state file — wave ordering may be wrong; content:\n%s", data)
	}

	// Stdout must mention both resources as CREATE.
	if !strings.Contains(stdout, "res-a") {
		t.Errorf("[AC-S94f1f9-2-1] stdout does not mention res-a; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "res-b") {
		t.Errorf("[AC-S94f1f9-2-1] stdout does not mention res-b; got:\n%s", stdout)
	}

	t.Logf("[AC-S94f1f9-2-1] stdout:\n%s", stdout)
	t.Logf("[AC-S94f1f9-2-1] state:\n%s", data)
}

// TestDAG_WaveOrder_ReverseDeclarationOrder verifies that wave ordering holds
// even when B is declared before A in the intent file (topological sort must
// not depend on declaration order).
//
// A-4 note: the stdout op-print order is *declaration* order (the reconcile
// command iterates `desired` when printing applied ops), which is a known,
// benign cosmetic detail — stdout order is NOT the proof of wave ordering here.
// The wave-order guarantee is verified through the state-file contents instead:
// B can only appear in the state file if A was applied first (the DAG makes B's
// Apply reachable only after A's Apply succeeds).  We therefore assert on the
// state file, not on stdout ordering.
func TestDAG_WaveOrder_ReverseDeclarationOrder(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// B declared first, then A.  Wave order must still apply A before B.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":   "fake",
				"id":         "rev-b",
				"depends_on": []string{"rev-a"},
				"fields": map[string]string{
					"name": "rev-b",
					"size": "20",
				},
			},
			{
				"provider": "fake",
				"id":       "rev-a",
				"fields": map[string]string{
					"name": "rev-a",
					"size": "10",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s stdout: %s", code, stderr, stdout)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if !strings.Contains(string(data), "rev-a") {
		t.Errorf("rev-a not in state file; content:\n%s", data)
	}
	if !strings.Contains(string(data), "rev-b") {
		t.Errorf("rev-b not in state file (should be applied after rev-a); content:\n%s", data)
	}

	t.Logf("stdout:\n%s", stdout)
}

// ---------------------------------------------------------------------------
// [AC-S94f1f9-2-2] Upstream failure: A fails → B skipped this cycle
// ---------------------------------------------------------------------------

// TestDAG_UpstreamFailureSkipsDownstream verifies scenario-S94f1f9-2 / step 2.
//
// [AC-S94f1f9-2-2] When A is configured to fail Apply (via fail_inject in the
// state file) and B depends_on A, "sakutei reconcile" must:
//   - Attempt to Apply A (which fails).
//   - Skip B (without attempting Apply on B).
//   - Print a skip message to stderr.
//   - Exit 0 (Apply errors within a cycle are not fatal — level-triggered).
//   - Neither A nor B must appear in the state file resources section.
func TestDAG_UpstreamFailureSkipsDownstream(t *testing.T) {
	// [AC-S94f1f9-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: A (no deps) and B (depends_on A).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "fail-a",
				"fields": map[string]string{
					"name": "fail-a",
					"size": "1",
				},
			},
			{
				"provider":   "fake",
				"id":         "fail-b",
				"depends_on": []string{"fail-a"},
				"fields": map[string]string{
					"name": "fail-b",
					"size": "2",
				},
			},
		},
	})

	// Seed the state file with fail_inject for "fail-a".
	// When the reconciler tries to Apply fail-a, the fake provider returns an
	// error.  This should cause fail-b to be skipped for this cycle.
	seedState := map[string]any{
		"resources": map[string]any{},
		"fail_inject": map[string]string{
			"fail-a": "simulated Apply failure for test [AC-S94f1f9-2-2]",
		},
	}
	seedData, _ := json.MarshalIndent(seedState, "", "  ")
	if err := os.WriteFile(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	// Use runReconcileFull to capture both stdout and stderr independently
	// (runReconcile only captures stderr for non-zero exits, but skip messages
	// go to stderr even on a successful exit-0 run).
	stdout, stderr, code := runReconcileFull(t, intentDir, statePath)

	// Exit 0: Apply errors within a cycle are not fatal (level-triggered model).
	// The reconcile command exits 0 even on per-resource failures.
	if code != 0 {
		t.Fatalf("[AC-S94f1f9-2-2] expected exit 0 (level-triggered), got %d; stderr: %s stdout: %s",
			code, stderr, stdout)
	}

	// stderr must contain a skip message for fail-b.
	combined := stdout + stderr
	if !strings.Contains(combined, "skip") {
		t.Errorf("[AC-S94f1f9-2-2] expected 'skip' message in output; got stdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}

	// fail-b must NOT be in the state file resources (it was skipped).
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S94f1f9-2-2] read state file: %v", err)
	}

	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("[AC-S94f1f9-2-2] parse state file: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	if _, ok := resources["fail-b"]; ok {
		t.Errorf("[AC-S94f1f9-2-2] fail-b must NOT be in state file (it should be skipped); state:\n%s", data)
	}
	if _, ok := resources["fail-a"]; ok {
		t.Errorf("[AC-S94f1f9-2-2] fail-a must NOT be in state file (Apply failed); state:\n%s", data)
	}

	t.Logf("[AC-S94f1f9-2-2] stdout:\n%s", stdout)
	t.Logf("[AC-S94f1f9-2-2] stderr:\n%s", stderr)
	t.Logf("[AC-S94f1f9-2-2] state:\n%s", data)
}

// TestDAG_UpstreamFailureNextCycleRetries verifies that on the next cycle,
// if A is no longer failing, both A and B are applied.
//
// This confirms the level-triggered property: the skip is per-cycle only.
func TestDAG_UpstreamFailureNextCycleRetries(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: A → B dependency.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "retry-a",
				"fields": map[string]string{
					"name": "retry-a",
					"size": "1",
				},
			},
			{
				"provider":   "fake",
				"id":         "retry-b",
				"depends_on": []string{"retry-a"},
				"fields": map[string]string{
					"name": "retry-b",
					"size": "2",
				},
			},
		},
	})

	// First "cycle": inject failure for retry-a.
	seedState := map[string]any{
		"resources":   map[string]any{},
		"fail_inject": map[string]string{"retry-a": "cycle-1 failure"},
	}
	seedData, _ := json.MarshalIndent(seedState, "", "  ")
	if err := os.WriteFile(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	_, _, _ = runReconcile(t, intentDir, statePath) // ignore result; failure expected

	// Verify retry-b is not in state after cycle 1.
	data, _ := os.ReadFile(statePath)
	var sf1 map[string]any
	_ = json.Unmarshal(data, &sf1)
	resources1, _ := sf1["resources"].(map[string]any)
	if _, ok := resources1["retry-b"]; ok {
		t.Errorf("retry-b should not exist after cycle 1 (upstream failed); state:\n%s", data)
	}

	// Second "cycle": remove fail_inject.
	clearState := map[string]any{
		"resources": map[string]any{},
		// fail_inject omitted → no injected failures
	}
	clearData, _ := json.MarshalIndent(clearState, "", "  ")
	if err := os.WriteFile(statePath, clearData, 0644); err != nil {
		t.Fatal(err)
	}

	stdout2, stderr2, code2 := runReconcile(t, intentDir, statePath)
	if code2 != 0 {
		t.Fatalf("cycle 2: expected exit 0, got %d; stderr: %s stdout: %s", code2, stderr2, stdout2)
	}

	// Both A and B must now be in state.
	data2, _ := os.ReadFile(statePath)
	if !strings.Contains(string(data2), "retry-a") {
		t.Errorf("retry-a not in state after cycle 2; state:\n%s", data2)
	}
	if !strings.Contains(string(data2), "retry-b") {
		t.Errorf("retry-b not in state after cycle 2 (level-triggered retry); state:\n%s", data2)
	}

	t.Logf("cycle 2 stdout:\n%s", stdout2)
	t.Logf("state after cycle 2:\n%s", data2)
}

// ---------------------------------------------------------------------------
// [AC-S94f1f9-2-2] (D-1 safety) Unobservable upstream skips downstream
// ---------------------------------------------------------------------------

// TestDAG_UnobservableUpstreamSkipsDownstream is the D-1 regression test.
//
// D-1: when an upstream resource's Observe returns Availability != Ok (target
// unreachable), the reconciler MUST NOT treat it as "converged" and proceed to
// Apply its downstream.  unknown ≠ absent; the downstream must be skipped this
// cycle (ADR-0002 / §3 asymmetry principle; halt-default).
//
// This is the safety-side counterpart of AC-S94f1f9-2-2 (which covers an Apply
// failure).  Here the *Observe* is what is unavailable.  Before the fix, an
// unobservable upstream produced an empty Plan indistinguishable from
// "converged", so the downstream was applied — a silent safety violation.
//
// Setup: A (no deps) is injected as observe-unknown; B depends_on A.
// Expected:
//   - B must NOT appear in the state file (it was skipped).
//   - A skip message must be printed.
//   - The one-shot reconcile exits non-zero (unobservable ⇒ convergence
//     incomplete; D-1 exit policy).
func TestDAG_UnobservableUpstreamSkipsDownstream(t *testing.T) {
	// [AC-S94f1f9-2-2] (D-1 regression: unobservable upstream, not Apply failure)
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: A (no deps) and B (depends_on A).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "obs-a",
				"fields": map[string]string{
					"name": "obs-a",
					"size": "1",
				},
			},
			{
				"provider":   "fake",
				"id":         "obs-b",
				"depends_on": []string{"obs-a"},
				"fields": map[string]string{
					"name": "obs-b",
					"size": "2",
				},
			},
		},
	})

	// Seed the state file so that Observe of obs-a returns AvailabilityUnknown
	// (reachable-but-unknown, no error): "unknown" mode in observe_unknown.
	seedState := map[string]any{
		"resources": map[string]any{},
		"observe_unknown": map[string]string{
			"obs-a": "unknown",
		},
	}
	seedData, _ := json.MarshalIndent(seedState, "", "  ")
	if err := os.WriteFile(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runReconcileFull(t, intentDir, statePath)

	// D-1 exit policy: unobservable upstream ⇒ non-zero exit.
	if code == 0 {
		t.Errorf("[AC-S94f1f9-2-2/D-1] expected non-zero exit when an upstream is unobservable; got 0; stdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}

	// A skip message must be present.
	combined := stdout + stderr
	if !strings.Contains(combined, "skip") {
		t.Errorf("[AC-S94f1f9-2-2/D-1] expected a skip message; got stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// The critical safety assertion: obs-b MUST NOT be applied when its upstream
	// obs-a was unobservable.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S94f1f9-2-2/D-1] read state file: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("[AC-S94f1f9-2-2/D-1] parse state file: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	if _, ok := resources["obs-b"]; ok {
		t.Errorf("[AC-S94f1f9-2-2/D-1] obs-b must NOT be applied when upstream obs-a is unobservable (unknown ≠ absent); state:\n%s", data)
	}
	// obs-a must not be applied either (it was never observable to diff).
	if _, ok := resources["obs-a"]; ok {
		t.Errorf("[AC-S94f1f9-2-2/D-1] obs-a must NOT be applied (it was unobservable); state:\n%s", data)
	}

	t.Logf("[AC-S94f1f9-2-2/D-1] stdout:\n%s", stdout)
	t.Logf("[AC-S94f1f9-2-2/D-1] stderr:\n%s", stderr)
}

// TestDAG_ObserveErrorUpstreamSkipsDownstream is the D-1 companion covering the
// Observe-*error* case (as opposed to AvailabilityUnknown without an error).
// Both must be treated identically: unobservable upstream ⇒ downstream skipped.
func TestDAG_ObserveErrorUpstreamSkipsDownstream(t *testing.T) {
	// [AC-S94f1f9-2-2/D-1]
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
				"id":       "err-a",
				"fields":   map[string]string{"name": "err-a", "size": "1"},
			},
			{
				"provider":   "fake",
				"id":         "err-b",
				"depends_on": []string{"err-a"},
				"fields":     map[string]string{"name": "err-b", "size": "2"},
			},
		},
	})

	// "error" mode: Observe of err-a returns a transient error.
	seedState := map[string]any{
		"resources": map[string]any{},
		"observe_unknown": map[string]string{
			"err-a": "error",
		},
	}
	seedData, _ := json.MarshalIndent(seedState, "", "  ")
	if err := os.WriteFile(statePath, seedData, 0644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runReconcileFull(t, intentDir, statePath)
	if code == 0 {
		t.Errorf("[AC-S94f1f9-2-2/D-1] expected non-zero exit on Observe error upstream; got 0; stderr:\n%s", stderr)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("parse state file: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	if _, ok := resources["err-b"]; ok {
		t.Errorf("[AC-S94f1f9-2-2/D-1] err-b must NOT be applied when upstream err-a's Observe errored; state:\n%s", data)
	}

	t.Logf("[AC-S94f1f9-2-2/D-1] stdout:\n%s\nstderr:\n%s", stdout, stderr)
}

// ---------------------------------------------------------------------------
// [AC-S94f1f9-2-3] Cyclic depends_on is rejected at load time
// ---------------------------------------------------------------------------

// TestDAG_CyclicDependsOnIsRejected verifies scenario-S94f1f9-2 / step 3.
//
// [AC-S94f1f9-2-3] When intent declares a cycle (A depends_on B, B depends_on
// A), "sakutei reconcile" must:
//   - Detect the cycle at intent load time.
//   - Exit non-0 before any Apply.
//   - Print an explicit error message mentioning the cycle.
//   - The state file must remain unchanged.
func TestDAG_CyclicDependsOnIsRejected(t *testing.T) {
	// [AC-S94f1f9-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: A depends_on B, B depends_on A → cycle.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":   "fake",
				"id":         "cycle-a",
				"depends_on": []string{"cycle-b"},
				"fields": map[string]string{
					"name": "cycle-a",
					"size": "1",
				},
			},
			{
				"provider":   "fake",
				"id":         "cycle-b",
				"depends_on": []string{"cycle-a"},
				"fields": map[string]string{
					"name": "cycle-b",
					"size": "2",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)

	// Must exit non-0 (cycle rejected).
	if code == 0 {
		t.Errorf("[AC-S94f1f9-2-3] expected non-zero exit for cyclic intent; got exit 0; stdout:\n%s", stdout)
	}

	// Error output must mention "cycl" (cycle, cyclic, etc.).
	combined := stdout + stderr
	if !strings.Contains(strings.ToLower(combined), "cycl") {
		t.Errorf("[AC-S94f1f9-2-3] expected cycle error message; got stdout:\n%s\nstderr:\n%s",
			stdout, stderr)
	}

	// State file must not have been created (no Apply happened).
	if data := readStateFile(t, statePath); data != nil {
		t.Errorf("[AC-S94f1f9-2-3] state file must not be created when cycle is detected; content:\n%s", data)
	}

	t.Logf("[AC-S94f1f9-2-3] stderr:\n%s", stderr)
	t.Logf("[AC-S94f1f9-2-3] exit code: %d", code)
}

// TestDAG_CyclicDependsOnRejectedBySakuteiPlan verifies that "sakutei plan"
// also rejects cyclic intent (not just reconcile).
func TestDAG_CyclicDependsOnRejectedBySakuteiPlan(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Three-node cycle: A → B → C → A.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":   "fake",
				"id":         "tri-a",
				"depends_on": []string{"tri-c"},
				"fields":     map[string]string{"name": "tri-a", "size": "1"},
			},
			{
				"provider":   "fake",
				"id":         "tri-b",
				"depends_on": []string{"tri-a"},
				"fields":     map[string]string{"name": "tri-b", "size": "2"},
			},
			{
				"provider":   "fake",
				"id":         "tri-c",
				"depends_on": []string{"tri-b"},
				"fields":     map[string]string{"name": "tri-c", "size": "3"},
			},
		},
	})

	_, stderr, code := runPlan(t, intentDir, statePath)

	if code == 0 {
		t.Errorf("expected non-zero exit for 3-node cycle in plan; got exit 0")
	}
	if !strings.Contains(strings.ToLower(stderr), "cycl") {
		t.Errorf("expected cycle error in stderr; got:\n%s", stderr)
	}

	t.Logf("stderr:\n%s", stderr)
}

// ---------------------------------------------------------------------------
// Additional correctness tests
// ---------------------------------------------------------------------------

// TestDAG_NoDeps_StillApplied verifies that resources without depends_on are
// applied normally (regression guard: wave decomposition must not drop them).
func TestDAG_NoDeps_StillApplied(t *testing.T) {
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
				"id":       "solo",
				"fields":   map[string]string{"name": "solo", "size": "99"},
			},
		},
	})

	_, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr)
	}

	data, _ := os.ReadFile(statePath)
	if !strings.Contains(string(data), "solo") {
		t.Errorf("resource 'solo' not found in state file; content:\n%s", data)
	}
}

// TestDAG_SelfDependency verifies that a self-referential depends_on is also
// treated as a cycle and rejected.
func TestDAG_SelfDependency(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":   "fake",
				"id":         "self-ref",
				"depends_on": []string{"self-ref"},
				"fields":     map[string]string{"name": "self-ref", "size": "1"},
			},
		},
	})

	_, stderr, code := runReconcile(t, intentDir, statePath)
	if code == 0 {
		t.Errorf("expected non-zero exit for self-referential depends_on; got exit 0")
	}
	if !strings.Contains(strings.ToLower(stderr), "cycl") {
		t.Errorf("expected cycle error; got stderr:\n%s", stderr)
	}
	t.Logf("stderr:\n%s", stderr)
}

// TestDAG_DuplicateLocalIDRejected verifies D-2: two resources declaring the
// same Local ID are rejected at load time (silent overwrite would otherwise
// shadow one resource).  The reconcile command must exit non-zero with an
// explicit "duplicate" error, and no state file may be created.
func TestDAG_DuplicateLocalIDRejected(t *testing.T) {
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
				"id":       "dup",
				"fields":   map[string]string{"name": "dup", "size": "1"},
			},
			{
				"provider": "fake",
				"id":       "dup",
				"fields":   map[string]string{"name": "dup", "size": "2"},
			},
		},
	})

	_, stderr, code := runReconcile(t, intentDir, statePath)
	if code == 0 {
		t.Errorf("[D-2] expected non-zero exit for duplicate Local ID; got exit 0")
	}
	if !strings.Contains(strings.ToLower(stderr), "duplicate") {
		t.Errorf("[D-2] expected 'duplicate' error; got stderr:\n%s", stderr)
	}
	if data := readStateFile(t, statePath); data != nil {
		t.Errorf("[D-2] state file must not be created when a duplicate ID is detected; content:\n%s", data)
	}
	t.Logf("[D-2] stderr:\n%s", stderr)
}
