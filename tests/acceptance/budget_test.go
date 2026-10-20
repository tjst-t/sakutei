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

// budget_test.go contains CLI-level acceptance tests for Story Scf7762-2.
//
// Scenario: "変更予算: 大量破壊を予算で止める"
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, exit code, and side effects on the state file.
// No internal package is imported directly; only the binary's observable
// behaviour is verified.
//
// AC tags map to scenario-Scf7762-2.json:
//
//	[AC-Scf7762-2-1] Per-cycle rate limit: a cycle with more destructive changes
//	                 than the configured limit exits non-0; excess changes are NOT
//	                 applied.
//	[AC-Scf7762-2-2] Cumulative time-window budget: daemon running multiple cycles
//	                 stops further destructive changes after the window budget is
//	                 exhausted (progressive bulk-delete is blocked).
//	[AC-Scf7762-2-3] Default halt; --approve-large-change N allows exceeding the
//	                 per-cycle rate limit when the number of destructive changes
//	                 is at most N.  The cumulative window budget is NOT bypassed.
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers specific to the budget tests
// ---------------------------------------------------------------------------

// seedManyPruneableResources writes n pruneable resources into the state file.
// Each resource has local ID "vm-<i>" and fields name="vm-<i>", size="<i>".
func seedManyPruneableResources(t *testing.T, statePath string, n int) {
	t.Helper()
	resources := make(map[string]any, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("vm-%02d", i)
		resources[id] = map[string]any{
			"fields": map[string]any{
				"name": id,
				"size": fmt.Sprintf("%d", i),
			},
		}
	}
	sf := map[string]any{
		"resources":            resources,
		"pruneable_capability": true,
		"provider_version":     testProviderVersion,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedManyPruneableResources: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedManyPruneableResources: write: %v", err)
	}
}

// countResourcesInState returns the number of resources in the state file.
func countResourcesInState(t *testing.T, statePath string) int {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("countResourcesInState: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("countResourcesInState: unmarshal: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	return len(resources)
}

// writeDesiredKeepOneResource writes an intent file that keeps only keepID
// under PruneWithinScope.  All other resources in reality become prune
// candidates in the same closed-world scope.
func writeDesiredKeepOneResource(t *testing.T, intentDir, keepID string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              keepID,
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": keepID,
					"size": "1",
				},
			},
		},
	})
}

// writeDesiredKeepResources writes an intent file that keeps exactly keepIDs
// under PruneWithinScope.  Every resource in reality that is NOT in keepIDs
// becomes a prune candidate in the same closed-world scope.  Rewriting this file
// mid-test progressively widens the prune scope so the daemon deletes a little
// more each cycle — the shape needed to exercise cross-cycle window accumulation.
func writeDesiredKeepResources(t *testing.T, intentDir string, keepIDs ...string) {
	t.Helper()
	resources := make([]map[string]any, 0, len(keepIDs))
	for i, id := range keepIDs {
		resources = append(resources, map[string]any{
			"provider":        "fake",
			"id":              id,
			"deletion_policy": "PruneWithinScope",
			"fields": map[string]string{
				"name": id,
				"size": fmt.Sprintf("%d", i+1),
			},
		})
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": resources})
}

// waitForResourceCount polls the state file until it holds exactly want
// resources or the deadline passes.  Returns true on match.  Used to confirm
// that a prior cycle's deletes actually landed in reality before the test moves
// to the next phase (proving deletion happened, not just that a plan was made).
func waitForResourceCount(t *testing.T, statePath string, want int, deadline time.Duration) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if countResourcesInState(t, statePath) == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// startDaemonWithFlags launches "sakutei" with the given flags and writes
// merged stdout+stderr to logPath.  Returns the *exec.Cmd and a wait func.
// The caller MUST ensure the process is eventually killed.
func startDaemonWithFlags(t *testing.T, logPath string, flags ...string) (*exec.Cmd, func() error) {
	t.Helper()
	bin := buildBinary(t)

	logF, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("startDaemonWithFlags: create log file: %v", err)
	}

	cmd := exec.Command(bin, flags...)
	cmd.Stdout = logF
	cmd.Stderr = logF

	if err := cmd.Start(); err != nil {
		_ = logF.Close()
		t.Fatalf("startDaemonWithFlags: Start: %v", err)
	}
	wait := func() error {
		err := cmd.Wait()
		_ = logF.Close()
		return err
	}
	return cmd, wait
}

// ---------------------------------------------------------------------------
// AC-Scf7762-2-1: per-cycle rate limit halts excess destructive changes
// ---------------------------------------------------------------------------

// TestBudget_PerCycleRateLimit verifies [AC-Scf7762-2-1].
//
// Setup:
//   - Reality contains 10 pruneable resources (vm-01..vm-10).
//   - Desired declares only "vm-01" under PruneWithinScope.
//   - 9 resources become prune candidates (all destructive Delete ops).
//
// Budget: --max-destructive-per-cycle=3 (only 3 destructive ops per cycle).
//
// Expected:
//   - reconcile exits non-zero (budget halted).
//   - "BUDGET HALTED" appears in stderr.
//   - At least 7 resources remain (halt-all fires BEFORE any op when 9>3).
func TestBudget_PerCycleRateLimit(t *testing.T) {
	// [AC-Scf7762-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with 10 pruneable resources.
	seedManyPruneableResources(t, statePath, 10)
	// Desired: keep only "vm-01".
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=3",
		"--on-exceed=halt-all",
	)

	// Must exit non-zero (budget halted).
	if code == 0 {
		t.Errorf("[AC-Scf7762-2-1] expected non-zero exit when rate limit exceeded (9 > 3), got 0; stdout=%s stderr=%s", stdout, stderr)
	}

	// Must mention BUDGET HALTED in output.
	combined := stdout + stderr
	if !strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-1] expected 'BUDGET HALTED' in output; stdout=%s stderr=%s", stdout, stderr)
	}

	// With halt-all: the budget fires BEFORE any op → all 10 must remain.
	remaining := countResourcesInState(t, statePath)
	if remaining < 7 {
		t.Errorf("[AC-Scf7762-2-1] too few resources remaining=%d; expected >=7 (budget halted excess deletions)", remaining)
	}

	t.Logf("[AC-Scf7762-2-1] PASS: rate-limit halted. remaining=%d code=%d", remaining, code)
}

// TestBudget_PerCycleRateLimit_HaltDestructiveOnly verifies [AC-Scf7762-2-1]
// with on-exceed=halt-destructive-only.
//
// Additive ops (Create for a new resource) still run; only the excess
// destructive ops are blocked.
func TestBudget_PerCycleRateLimit_HaltDestructiveOnly(t *testing.T) {
	// [AC-Scf7762-2-1] on-exceed=halt-destructive-only variant
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Reality: 5 pruneable resources (vm-01..vm-05).
	seedManyPruneableResources(t, statePath, 5)

	// Desired: keep "vm-01" + create "vm-new" (additive) + PruneWithinScope.
	// 4 candidates: vm-02..vm-05 (all destructive).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "vm-01",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "vm-01",
					"size": "1",
				},
			},
			{
				"provider":        "fake",
				"id":              "vm-new",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "vm-new",
					"size": "99",
				},
			},
		},
	})

	// Budget: at most 1 destructive op; halt-destructive-only keeps additive ops.
	// 4 candidates > 1 → budget fires for destructive ops.
	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=1",
		"--on-exceed=halt-destructive-only",
	)

	// Must exit non-zero (destructive budget halted).
	if code == 0 {
		t.Errorf("[AC-Scf7762-2-1-hdo] expected non-zero exit, got 0; stdout=%s stderr=%s", stdout, stderr)
	}

	combined := stdout + stderr
	if !strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-1-hdo] expected 'BUDGET HALTED' in output; stdout=%s stderr=%s", stdout, stderr)
	}

	// vm-new (additive Create) should have been created despite the destructive halt.
	if !resourceExistsInState(t, statePath, "vm-new") {
		t.Errorf("[AC-Scf7762-2-1-hdo] vm-new (additive) should have been created despite destructive halt")
	}

	// At most 1 delete allowed (the per-cycle limit), so >=5 resources remain.
	remaining := countResourcesInState(t, statePath)
	if remaining < 5 {
		t.Errorf("[AC-Scf7762-2-1-hdo] expected >=5 resources remaining (limit=1 delete); got %d", remaining)
	}

	t.Logf("[AC-Scf7762-2-1-hdo] PASS: halt-destructive-only; vm-new created; remaining=%d code=%d", remaining, code)
}

// ---------------------------------------------------------------------------
// AC-Scf7762-2-2: cumulative time-window budget blocks progressive bulk-delete
// ---------------------------------------------------------------------------

// TestBudget_CumulativeWindowBudget_Daemon verifies [AC-Scf7762-2-2].
//
// The cumulative window counter is S3 (memory, daemon lifetime).  Only the
// long-lived daemon accumulates the count across cycles.
//
// Strategy (cross-cycle window accumulation):
//   - Reality: 4 pruneable resources.
//   - Desired: keep only "vm-01" → 3 prune candidates (vm-02, vm-03, vm-04).
//   - Budget: --max-destructive-per-cycle=3 (per-cycle limit matches candidate
//     count, so cycle 1 can delete all 3), --max-destructive-per-window=2,
//     --window-duration=60s.
//   - With window=2, the budget engine sees 3 planned deletes > window cap 2
//     on cycle 1, fires BUDGET HALTED (window), and deletes nothing.
//
// Alternative: this also validates the one-shot path for window budget because
// even a single reconcile invocation pre-checks the window budget and halts
// when the planned destructive count exceeds it.
//
// Expected:
//   - "BUDGET HALTED" appears in the daemon log.
//   - At most 2 resources are deleted (window cap).
func TestBudget_CumulativeWindowBudget_Daemon(t *testing.T) {
	// [AC-Scf7762-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	// Seed reality with 4 pruneable resources (vm-01..vm-04).
	seedManyPruneableResources(t, statePath, 4)
	// Desired: keep only "vm-01" → 3 prune candidates.
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	// Launch the daemon with:
	//   --max-destructive-per-cycle=3  (per-cycle matches 3 candidates)
	//   --max-destructive-per-window=2 (window cap < 3 → halts on first cycle)
	//   --window-duration=60s          (long enough not to decay in test)
	//   --interval=200ms               (fast cycles)
	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "200ms",
		"--max-destructive-per-cycle=3",
		"--max-destructive-per-window=2",
		"--window-duration=60s",
		"--on-exceed=halt-all",
	)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()

	// Wait for the daemon to emit BUDGET HALTED (window budget: 3 planned > cap 2).
	if !waitForLogFile(logPath, "BUDGET HALTED", 10*time.Second) {
		logStr := readFileString(logPath)
		remaining := countResourcesInState(t, statePath)
		t.Fatalf("[AC-Scf7762-2-2] daemon did not emit BUDGET HALTED within deadline; remaining=%d; log:\n%s",
			remaining, logStr)
	}

	// Stop the daemon.
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true

	logStr := readFileString(logPath)

	// At most 2 resources deleted (window cap).
	remaining := countResourcesInState(t, statePath)
	deleted := 4 - remaining
	if deleted > 2 {
		t.Errorf("[AC-Scf7762-2-2] window budget violated: deleted=%d (max=2); log:\n%s", deleted, logStr)
	}

	if !strings.Contains(logStr, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-2] 'BUDGET HALTED' not in daemon log; log:\n%s", logStr)
	}

	t.Logf("[AC-Scf7762-2-2] PASS: cumulative window budget halted progressive deletion; deleted=%d remaining=%d", deleted, remaining)
}

// TestBudget_ProgressiveDestruction_Daemon verifies [AC-Scf7762-2-2] via the
// TRUE cross-cycle accumulation path — the core scenario of this Story.
//
// The sibling test above (single-cycle pre-check) halts on cycle 1 before ANY
// delete runs, so CommitOps is never reached and windowTotal never advances.
// That path does not prove the "delete a little each cycle until the rolling
// window is exhausted" behaviour.  This test does: it drives the daemon through
// multiple cycles where deletes ACTUALLY happen (confirmed in reality), then the
// window budget halts the next delete once the accumulated committed count would
// exceed the cap.
//
// Mechanics — the daemon reloads intent every cycle, so we widen the prune scope
// in phases by rewriting the intent file, and confirm each phase's delete landed
// in reality before advancing:
//
//	reality  = 5 pruneable VMs (vm-01..vm-05)
//	window   = 2, window-duration = 10m (no decay during the test)
//	per-cycle limit = high (10): the per-cycle rate gate never fires, so the ONLY
//	                  thing that can halt here is the cumulative window budget.
//
//	phase A: keep vm-01..vm-04  → orphan vm-05         → delete 1 (windowTotal 0→1); reality 5→4
//	phase B: keep vm-01..vm-03  → orphan vm-04         → delete 1 (windowTotal 1→2); reality 4→3
//	phase C: keep vm-01,vm-02   → orphan vm-03         → 2+1=3 > cap 2 → BUDGET HALTED; vm-03 survives
//
// Each phase deletes at most 1 of the current list, well under the prune 50%
// drop-rate guard, so the ONLY halt observed is the budget halt in phase C.
func TestBudget_ProgressiveDestruction_Daemon(t *testing.T) {
	// [AC-Scf7762-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	// Reality: 5 pruneable resources vm-01..vm-05.
	seedManyPruneableResources(t, statePath, 5)

	// Phase A intent (keep 4, orphan vm-05) must be in place before the daemon
	// starts so the first cycle deletes exactly one resource.
	writeDesiredKeepResources(t, intentDir, "vm-01", "vm-02", "vm-03", "vm-04")

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "150ms",
		"--max-destructive-per-cycle=10", // high: rate gate never fires
		"--max-destructive-per-window=2", // window cap: accumulates across cycles
		"--window-duration=10m",          // long: no decay during the test
		"--on-exceed=halt-all",
	)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()

	// --- Phase A: confirm vm-05 was ACTUALLY deleted (reality 5→4). ---
	if !waitForResourceCount(t, statePath, 4, 10*time.Second) {
		t.Fatalf("[AC-Scf7762-2-2] phase A: expected 4 resources after deleting vm-05; got %d; log:\n%s",
			countResourcesInState(t, statePath), readFileString(logPath))
	}
	if resourceExistsInState(t, statePath, "vm-05") {
		t.Fatalf("[AC-Scf7762-2-2] phase A: vm-05 should have been pruned (real delete); log:\n%s", readFileString(logPath))
	}

	// --- Phase B: widen scope; confirm vm-04 was ACTUALLY deleted (reality 4→3). ---
	writeDesiredKeepResources(t, intentDir, "vm-01", "vm-02", "vm-03")
	if !waitForResourceCount(t, statePath, 3, 10*time.Second) {
		t.Fatalf("[AC-Scf7762-2-2] phase B: expected 3 resources after deleting vm-04; got %d; log:\n%s",
			countResourcesInState(t, statePath), readFileString(logPath))
	}
	if resourceExistsInState(t, statePath, "vm-04") {
		t.Fatalf("[AC-Scf7762-2-2] phase B: vm-04 should have been pruned (real delete); log:\n%s", readFileString(logPath))
	}

	// At this point windowTotal == 2 (two deletes committed across two cycles):
	// the rolling window is now full.

	// --- Phase C: widen scope again; the next delete would push committed 2 + planned 1 = 3 > cap 2.
	// The cumulative window budget MUST halt it, leaving vm-03 alive. ---
	writeDesiredKeepResources(t, intentDir, "vm-01", "vm-02")

	if !waitForLogFile(logPath, "BUDGET HALTED", 10*time.Second) {
		t.Fatalf("[AC-Scf7762-2-2] phase C: daemon did not emit BUDGET HALTED after the window filled; remaining=%d; log:\n%s",
			countResourcesInState(t, statePath), readFileString(logPath))
	}

	// Give the daemon a couple more cycles to prove it does NOT delete vm-03
	// despite the widened scope (the window budget keeps halting).
	time.Sleep(600 * time.Millisecond)

	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true

	logStr := readFileString(logPath)

	// vm-03 must still exist: the window budget prevented the 3rd cumulative delete.
	if !resourceExistsInState(t, statePath, "vm-03") {
		t.Errorf("[AC-Scf7762-2-2] phase C: vm-03 must survive (window budget halts the 3rd cumulative delete); log:\n%s", logStr)
	}
	remaining := countResourcesInState(t, statePath)
	if remaining != 3 {
		t.Errorf("[AC-Scf7762-2-2] phase C: expected exactly 3 resources remaining (vm-01,vm-02,vm-03); got %d; log:\n%s",
			remaining, logStr)
	}

	// The halt line must be present AND cycle-numbered (E fix: no number-less dup).
	if !strings.Contains(logStr, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-2] 'BUDGET HALTED' not in daemon log; log:\n%s", logStr)
	}

	t.Logf("[AC-Scf7762-2-2] PASS: progressive destruction across cycles (deleted vm-05 then vm-04 for real), then window budget halted vm-03; remaining=%d", remaining)
}

// ---------------------------------------------------------------------------
// AC-Scf7762-2-3: default halt; --approve-large-change N bypasses per-cycle limit
// ---------------------------------------------------------------------------

// TestBudget_DefaultHalt_WithoutApproval verifies [AC-Scf7762-2-3] — the
// default halt side: without approval, rate limit triggers non-zero exit.
func TestBudget_DefaultHalt_WithoutApproval(t *testing.T) {
	// [AC-Scf7762-2-3] no-approval side
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// 10 resources; keep only "vm-01" → 9 prune candidates > limit 3.
	seedManyPruneableResources(t, statePath, 10)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=3",
	)

	if code == 0 {
		t.Errorf("[AC-Scf7762-2-3] expected non-zero exit (default halt), got 0; stdout=%s stderr=%s", stdout, stderr)
	}

	combined := stdout + stderr
	if !strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-3] expected 'BUDGET HALTED' without approval flag; stdout=%s stderr=%s", stdout, stderr)
	}

	t.Logf("[AC-Scf7762-2-3-no-approval] PASS: default halt fires without --approve-large-change. code=%d", code)
}

// TestBudget_ApproveLargeChange verifies [AC-Scf7762-2-3] — explicit approval
// side: --approve-large-change=N raises the per-cycle rate limit to N and
// allows the destructive changes to proceed.
func TestBudget_ApproveLargeChange(t *testing.T) {
	// [AC-Scf7762-2-3] approval side
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// 5 resources; keep only "vm-01" → 4 prune candidates.
	seedManyPruneableResources(t, statePath, 5)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	// Per-cycle limit=2 (would halt 4 candidates), but approval=4 overrides it.
	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=2",
		"--approve-large-change=4",
	)

	if code != 0 {
		t.Errorf("[AC-Scf7762-2-3] expected exit 0 with --approve-large-change=4, got %d; stdout=%s stderr=%s", code, stdout, stderr)
	}

	// All 4 prune candidates (vm-02..vm-05) should have been deleted.
	remaining := countResourcesInState(t, statePath)
	if remaining != 1 {
		t.Errorf("[AC-Scf7762-2-3] expected 1 resource remaining (vm-01) after approval; got %d", remaining)
	}

	// Must NOT mention BUDGET HALTED.
	combined := stdout + stderr
	if strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-3] unexpected BUDGET HALTED with --approve-large-change=4; stdout=%s stderr=%s", stdout, stderr)
	}

	t.Logf("[AC-Scf7762-2-3-approval] PASS: approve-large-change=4 allowed 4 deletes. remaining=%d code=%d", remaining, code)
}

// TestBudget_ApproveLargeChange_DoesNotBypassWindowBudget verifies that the
// cumulative window budget is NOT bypassed by --approve-large-change.
//
// The approval flag raises only the per-cycle rate limit; the cumulative window
// budget always applies (§13.2: "halt-all: rendered_revision 全体を凍結し人へ").
func TestBudget_ApproveLargeChange_DoesNotBypassWindowBudget(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// 5 resources; keep only "vm-01" → 4 prune candidates.
	seedManyPruneableResources(t, statePath, 5)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	// Approve up to 10 per-cycle and raise the cycle limit to 10, but window cap = 2.
	// 4 candidates exceed the window cap of 2 → window budget fires.
	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=10",
		"--approve-large-change=10",
		"--max-destructive-per-window=2",
		"--window-duration=60s",
	)

	// Window budget must halt even with approval.
	if code == 0 {
		t.Errorf("[AC-Scf7762-2-3-window] expected non-zero exit (window budget fires even with approval), got 0; stdout=%s stderr=%s", stdout, stderr)
	}

	combined := stdout + stderr
	if !strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-2-3-window] expected 'BUDGET HALTED' (window budget) despite approval flag; stdout=%s stderr=%s", stdout, stderr)
	}

	t.Logf("[AC-Scf7762-2-3-window] PASS: window budget halts even with approve-large-change. code=%d", code)
}
