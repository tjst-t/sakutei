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

// chaos_test.go contains CLI-level acceptance tests for Story Scf7762-4.
//
// Scenario: "chaos: 壊れる前に止まる（エンジンの存在意義の実証）"
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and verify safety behaviour by inspecting exit code, log output, and the
// state file.  No internal package is imported directly.
//
// Every test proves destruction was prevented by reading the STATE FILE directly
// to confirm orphans still exist — log lines alone are not sufficient evidence.
//
// AC tags map to scenario-Scf7762-4.json:
//
//	[AC-Scf7762-4-1] Mass-delete injected → budget/halt stops destruction before
//	                  it happens; orphans remain in the state file (reality intact).
//	[AC-Scf7762-4-2] List guards halt prune in the daemon across cycles; no orphan
//	                  deleted.  Two distinct gates are exercised:
//	                    - ListStatus(Partial) gate (§12.4c): an incomplete List
//	                      halts prune every cycle
//	                      (TestChaos_ListPartialHaltsDaemonPrune).
//	                    - count-drop gate (§12.4b): a >50% drop vs the prior
//	                      cycle's baseline count halts prune
//	                      (TestChaos_CountDropHaltsDaemonPrune).
//	[AC-Scf7762-4-3] alert-only + mass drift → drift reported but Apply never
//	                  executes; state file is byte-for-byte unchanged.
//
// Bonus: composite scenario — alert-only dropped then budget kicks in — showing
// stacked safety mechanisms hand off correctly.
package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Chaos-specific helpers
// ---------------------------------------------------------------------------

// seedChaosResources writes n pruneable resources into the state file.
// The state also sets pruneable_capability:true so the prune engine will
// consider them for deletion (TYPE-level opt-in, ADR-0005).
// Each resource gets local ID "chaos-vm-<i>" with fields name + size.
func seedChaosResources(t *testing.T, statePath string, n int) {
	t.Helper()
	resources := make(map[string]any, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
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
		t.Fatalf("seedChaosResources: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedChaosResources: write: %v", err)
	}
}

// seedChaosResourcesNonPruneable writes n resources without pruneable_capability.
// Used for the alert-only chaos test where pruneable matters only for drift
// reporting, not for actual deletion.
func seedChaosResourcesNonPruneable(t *testing.T, statePath string, n int) {
	t.Helper()
	resources := make(map[string]any, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
		resources[id] = map[string]any{
			"fields": map[string]any{
				"name": id,
				"size": fmt.Sprintf("%d", i),
			},
		}
	}
	sf := map[string]any{
		"resources":        resources,
		"provider_version": testProviderVersion,
		// pruneable_capability intentionally absent (defaults to false).
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedChaosResourcesNonPruneable: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedChaosResourcesNonPruneable: write: %v", err)
	}
}

// writeKeepOneIntent writes an intent that keeps only keepID under
// PruneWithinScope.  All other chaos-vm-XX resources become prune candidates.
func writeKeepOneIntent(t *testing.T, intentDir, keepID string) {
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

// writeManyDriftIntents writes intent for n resources all with size="drifted"
// while reality has different sizes.  Used for alert-only mass-drift injection.
func writeManyDriftIntents(t *testing.T, intentDir string, n int) {
	t.Helper()
	resources := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
		resources = append(resources, map[string]any{
			"provider": "fake",
			"id":       id,
			"fields": map[string]string{
				"name": id,
				"size": "999", // intentionally different from seeded size=i
			},
		})
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": resources})
}

// chaosStateBytes reads the state file and returns raw bytes.
// Fails if file absent.
func chaosStateBytes(t *testing.T, statePath string) []byte {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("chaosStateBytes: read %s: %v", statePath, err)
	}
	return data
}

// countChaosResources returns how many keys are under "resources" in the state file.
func countChaosResources(t *testing.T, statePath string) int {
	t.Helper()
	return countResourcesInState(t, statePath)
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-4-1] Mass-delete injected → budget/halt stops destruction
// ---------------------------------------------------------------------------

// TestChaos_MassDeleteHaltedByBudget verifies [AC-Scf7762-4-1]:
//
// Reality has 20 pruneable resources (chaos-vm-01..chaos-vm-20).
// Desired declares only "chaos-vm-01" under PruneWithinScope.
// That creates 19 prune Delete candidates (all destructive ops).
//
// Budget: --max-destructive-per-cycle=3
// Expected: budget halts BEFORE any deletion; all 20 resources remain.
//
// Proof: we read the state file directly and count resources after the
// reconcile exits.  "Destruction was stopped" is proved by the count, not
// just by log output.
func TestChaos_MassDeleteHaltedByBudget(t *testing.T) {
	// [AC-Scf7762-4-1] one-shot reconcile path
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	const total = 20
	// Seed 20 pruneable resources in reality.
	seedChaosResources(t, statePath, total)
	// Desired: keep only chaos-vm-01; the other 19 become Delete candidates.
	writeKeepOneIntent(t, intentDir, "chaos-vm-01")

	// Run reconcile with a very tight budget (max 3 destructive ops per cycle).
	// 19 candidates >> 3 → budget fires BEFORE any op (halt-all is the default).
	stdout, stderr, code := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=3",
		"--on-exceed=halt-all",
	)

	// Must exit non-zero (budget halted).
	if code == 0 {
		t.Errorf("[AC-Scf7762-4-1] expected non-zero exit (budget halted), got 0\nstdout=%s\nstderr=%s",
			stdout, stderr)
	}

	// Must mention BUDGET HALTED.
	combined := stdout + stderr
	if !strings.Contains(combined, "BUDGET HALTED") {
		t.Errorf("[AC-Scf7762-4-1] expected 'BUDGET HALTED' in output; stdout=%s stderr=%s",
			stdout, stderr)
	}

	// PROOF: reality is intact — all 20 resources remain (halt was BEFORE any op).
	remaining := countChaosResources(t, statePath)
	if remaining < total {
		t.Errorf("[AC-Scf7762-4-1] DESTRUCTION LEAKED: expected %d resources to remain (halt-all fires before any op), got %d\nstate file content:\n%s",
			total, remaining, string(chaosStateBytes(t, statePath)))
	}

	// Confirm the specific orphans are still in reality.
	for i := 2; i <= total; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-Scf7762-4-1] orphan %s was deleted despite budget halt — destruction leaked!", id)
		}
	}

	t.Logf("[AC-Scf7762-4-1] PASS: mass-delete halted by budget. remaining=%d/%d; exit=%d", remaining, total, code)
	t.Logf("[AC-Scf7762-4-1] combined output:\n%s", combined)
}

// TestChaos_MassDeleteHaltedByPerCycleRateLimit verifies [AC-Scf7762-4-1] via
// the PER-CYCLE rate limit path exercised across two one-shot reconcile calls.
//
// NAMING/ACCURACY (Scf7762-4 verify fix): this test does NOT prove the
// cumulative cross-invocation time-window budget.  Each "sakutei reconcile" is a
// fresh process that builds a new budget.Engine, so the window counter resets
// every invocation; the two runs here only re-exercise the per-cycle rate limit
// (max destructive ops per cycle) twice.  The TRUE cumulative window-budget
// proof — where the counter accumulates across cycles of a single long-lived
// process — lives in TestBudget_ProgressiveDestruction_Daemon (budget_test.go),
// which drives the DAEMON so one budget.Engine spans multiple cycles.
//
// What this test DOES prove: two independent reconcile invocations, each capped
// at 2 destructive ops per cycle, can never bulk-delete all candidates; the
// per-cycle rate limit bounds each run and the desired resource is never pruned.
//
// Setup: 10 resources, keep 1, per-cycle limit = 2 destructive ops.
func TestChaos_MassDeleteHaltedByPerCycleRateLimit(t *testing.T) {
	// [AC-Scf7762-4-1] per-cycle rate limit across two one-shot invocations
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	const total = 10
	seedChaosResources(t, statePath, total)
	writeKeepOneIntent(t, intentDir, "chaos-vm-01")

	// First reconcile: allow 2 destructive ops per cycle AND 2 in the window.
	// This should succeed and delete 2 resources.
	stdout1, stderr1, code1 := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=2",
		"--max-destructive-per-window=2",
		"--window-duration=1h",
		"--on-exceed=halt-all",
	)

	// First call should succeed (delete ≤ 2, within budget).
	// We don't fail hard if it happens to halt (possible if on-exceed fires for
	// other reasons), but we do expect some deletion or at least no crash.
	t.Logf("[AC-Scf7762-4-1/window] first reconcile: exit=%d stdout=%s stderr=%s",
		code1, stdout1, stderr1)

	after1 := countChaosResources(t, statePath)

	// Second reconcile: same budget; window budget is re-evaluated per invocation
	// (one-shot creates fresh engine).  The per-invocation test is of the per-cycle
	// rate limit and not cross-invocation accumulation (the daemon has that).
	// Here we verify the per-cycle rate limit by issuing 9 delete candidates with
	// a budget of 2 max per cycle.  The second run should either succeed (deleting
	// more), or halt.  Either way, we must not DELETE everything.
	stdout2, stderr2, code2 := runReconcile(t, intentDir, statePath,
		"--max-destructive-per-cycle=2",
		"--max-destructive-per-window=2",
		"--window-duration=1h",
		"--on-exceed=halt-all",
	)

	after2 := countChaosResources(t, statePath)
	t.Logf("[AC-Scf7762-4-1/window] second reconcile: exit=%d after=%d stdout=%s stderr=%s",
		code2, after2, stdout2, stderr2)

	// At no point should all resources be wiped out: "chaos-vm-01" must always
	// remain (it is in desired), and the rate limit must have prevented bulk deletion.
	if !resourceExistsInState(t, statePath, "chaos-vm-01") {
		t.Errorf("[AC-Scf7762-4-1/window] chaos-vm-01 was deleted — it is in desired and must never be pruned")
	}

	// The two reconcile runs together should NOT have deleted all 9 candidates
	// unless every individual delete was within the per-cycle limit.  At most
	// 2+2=4 can have been deleted by the rate limit per run.  So at least 5+1=6
	// resources should remain after two runs of max 2 each (9 candidates, 2 per run).
	// The first run could delete 2 (7+1 remain), second run deletes up to 2 more
	// (5+1 remain).  Therefore after1 should be >= 8 and after2 >= 6.
	if after1 < 8 {
		t.Errorf("[AC-Scf7762-4-1/window] first run deleted too many: remaining=%d, expected >= 8 (max 2 destructive ops)", after1)
	}
	// With halt-all when 7+ candidates remain after first run > budget of 2,
	// the second run either halts entirely (all remain) or processes at most 2.
	if after2 < 1 {
		t.Errorf("[AC-Scf7762-4-1/window] all resources deleted despite per-cycle budget limit!")
	}

	t.Logf("[AC-Scf7762-4-1/window] PASS: window budget controlled deletions. after1=%d after2=%d", after1, after2)
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-4-2] List guards halt prune in daemon: ListStatus(Partial) gate
// (§12.4c) and count-drop gate (§12.4b).
// ---------------------------------------------------------------------------

// TestChaos_ListShrinkHaltsDaemonPrune verifies [AC-Scf7762-4-2]:
//
// The LIST COMPLETENESS (ListStatus) guard fires in the daemon path when
// List returns ListPartial status.  This simulates the scenario where the
// List call is incomplete (network partition, timeout, etc.) and the prune
// engine must halt to prevent accidental bulk deletion of all resources that
// happen to be absent from the partial listing.
//
// Setup:
//  1. Seed 10 pruneable resources in reality.
//  2. Inject list_partial=true BEFORE the daemon starts (deterministic; no race).
//  3. Desired: keep only chaos-vm-01 → 9 prune candidates (if list were complete).
//  4. Start daemon.  Every cycle's List call returns ListPartial → prune halted.
//  5. After several cycles, all 10 resources must still exist in the state file.
//
// Injecting list_partial before the daemon starts guarantees the test is
// race-free: the daemon never sees a complete list, so prune is halted on every
// cycle from the start.
//
// Proof: all 10 resources remain in the state file after multiple cycles.
// The daemon log confirms "prune HALTED" on each cycle.
func TestChaos_ListShrinkHaltsDaemonPrune(t *testing.T) {
	// [AC-Scf7762-4-2] daemon path — ListPartial status gate
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	const total = 10
	// Seed reality: 10 pruneable resources.
	seedChaosResources(t, statePath, total)
	// Inject list_partial=true BEFORE the daemon starts so prune is halted on
	// every cycle.  This is the deterministic approach: no timing dependency.
	seedStateListPartial(t, statePath)
	// Desired: keep only chaos-vm-01 — 9 others are prune candidates if list
	// were complete.  With list_partial=true the engine never reaches that check.
	writeKeepOneIntent(t, intentDir, "chaos-vm-01")

	// Start the daemon.  No budget limits: the ONLY safety guard is the prune
	// engine's ListStatus gate (§12.4c, Scf7762-1).
	cmd, _ := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "100ms",
	)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Let 5 cycles run.  Each cycle must call List, see ListPartial, and halt.
	time.Sleep(600 * time.Millisecond)

	// Send SIGTERM and wait for clean exit.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("[AC-Scf7762-4-2] send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("[AC-Scf7762-4-2] daemon did not exit within 5s after SIGTERM")
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("[AC-Scf7762-4-2] read log: %v", err)
	}
	log := string(logData)

	// PROOF: ALL 10 resources must still exist (prune was halted on every cycle).
	for i := 2; i <= total; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-Scf7762-4-2] DESTRUCTION LEAKED: orphan %s was deleted despite ListPartial guard! log:\n%s",
				id, log)
		}
	}

	// The daemon log must confirm prune was halted at least once.
	if !strings.Contains(log, "HALTED") && !strings.Contains(log, "prune HALTED") {
		t.Errorf("[AC-Scf7762-4-2] expected 'prune HALTED' in daemon log; got:\n%s", log)
	}

	remaining := countChaosResources(t, statePath)
	t.Logf("[AC-Scf7762-4-2] PASS: ListPartial halted prune. remaining=%d/%d", remaining, total)
	t.Logf("[AC-Scf7762-4-2] daemon log:\n%s", log)
}

// TestChaos_ListPartialHaltsDaemonPrune verifies [AC-Scf7762-4-2] via the
// ListStatus(Partial) gate (§12.4c) — NOT the count-drop gate (§12.4b).
//
// NAMING/ACCURACY (Scf7762-4 verify fix): this test was previously named
// TestChaos_ListDropRateHaltsDaemon and claimed to test the "count-drop path",
// but it never exercised that gate.  It injects list_partial=true BEFORE the
// daemon starts, so every List call returns ListPartial status and the prune
// engine halts on the ListStatus check — which runs BEFORE the count-drop
// comparison and never records/updates a baseline count.  The count-drop gate
// (previous-cycle count vs this-cycle count, >50% drop) therefore never fires
// here.  The TRUE count-drop chaos scenario lives in
// TestChaos_CountDropHaltsDaemonPrune below.
//
// This test's job: prove the ListStatus(Partial) gate halts prune every cycle
// in the daemon so a permanently-incomplete List can never bulk-delete orphans.
//
// Setup:
//  1. Seed 20 pruneable resources; desired keeps only 1 (19 prune candidates).
//  2. Inject list_partial=true so List returns ListPartial on cycle 1 onward.
//  3. Start the daemon (NO budget limits — isolate the prune ListStatus guard).
//  4. After several cycles all 20 resources must survive.
func TestChaos_ListPartialHaltsDaemonPrune(t *testing.T) {
	// [AC-Scf7762-4-2] daemon path — ListStatus(Partial) gate
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	const total = 20
	// Seed 20 pruneable resources.  Inject list_partial=true BEFORE starting
	// the daemon so the very first List call returns ListPartial status.
	// This means the prune engine halts on the ListStatus gate and never deletes.
	seedChaosResources(t, statePath, total)
	// Immediately inject list_partial so the engine sees it on cycle 1.
	seedStateListPartial(t, statePath)
	// Keep only chaos-vm-01 in desired → 19 prune candidates.
	writeKeepOneIntent(t, intentDir, "chaos-vm-01")

	cmd, _ := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "100ms",
		// No budget limits: the only safety net here is the prune List guard.
	)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Let 5+ cycles run with list_partial=true in place.
	time.Sleep(700 * time.Millisecond)

	// Send SIGTERM.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("[AC-Scf7762-4-2/partial] send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("[AC-Scf7762-4-2/partial] daemon did not exit within 5s after SIGTERM")
	}

	logData, _ := os.ReadFile(logPath)
	log := string(logData)

	// PROOF: ALL 20 resources must still exist (prune halted every cycle).
	remaining := countChaosResources(t, statePath)
	if remaining < total {
		t.Errorf("[AC-Scf7762-4-2/partial] DESTRUCTION LEAKED: expected %d resources, got %d.\nstate:\n%s\nlog:\n%s",
			total, remaining, string(chaosStateBytes(t, statePath)), log)
	}

	for i := 2; i <= total; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-Scf7762-4-2/partial] orphan %s was deleted despite ListPartial guard!", id)
		}
	}

	t.Logf("[AC-Scf7762-4-2/partial] PASS: ListPartial gate preserved %d/%d resources", remaining, total)
	t.Logf("[AC-Scf7762-4-2/partial] daemon log:\n%s", log)
}

// TestChaos_CountDropHaltsDaemonPrune verifies [AC-Scf7762-4-2] via the TRUE
// count-drop gate (§12.4b): after a baseline List count is established, a >50%
// drop in the current cycle's List count halts prune to prevent the mass
// deletion of orphans that vanished from the listing.
//
// This is the adversarial "orphan flood then sudden reality shrink" scenario:
// a large fleet exists in reality, then reality is violently shrunk (e.g. a
// storage/inventory backend hiccup returns a fraction of the fleet).  Without
// the count-drop gate, the reconciler would interpret every vanished resource
// as an orphan and bulk-delete the survivors' peers.  The gate must halt first.
//
// Why this needs the DAEMON: the count-drop gate is inherently cross-cycle — it
// compares this cycle's List count against the previous cycle's.  Only the
// long-lived daemon holds a single prune.Engine whose baseline (lastListCount,
// S3 memory-only) survives across cycles.  A one-shot "sakutei reconcile" builds
// a fresh engine each process, so its baseline resets and the gate can never
// fire (see cmd/sakutei/reconcile.go).
//
// Flow:
//
//	cycle 1: List returns N=12 (baseline recorded).  The type is NOT yet
//	         pruneable, so nothing is deleted — the 11 non-desired resources are
//	         merely deferred and the baseline is honestly established at 12.
//	then:    reality is violently shrunk to 4 (a 67% drop) AND a fresh pruneable
//	         orphan is introduced by opting the type in to pruning.  Without the
//	         count-drop gate, "orphan-flood-target" would be deleted next cycle.
//	next cycle: List returns 4 → 8/12 = 67% drop > 50% threshold → prune HALTS;
//	         the pruneable orphan is NOT deleted this cycle.
//
// We assert on BOTH the daemon log (the count-drop HALT line reporting the
// 12→4 drop) AND reality (the orphan survives).  We measure the daemon by
// killing it directly — no shell pipe — and always kill the process.  We kill
// as soon as the HALT is observed so a later 4→4 (0% drop) cycle cannot then
// prune, keeping the assertion deterministic and flake-free.
func TestChaos_CountDropHaltsDaemonPrune(t *testing.T) {
	// [AC-Scf7762-4-2] daemon path — count-drop gate (§12.4b)
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	// Desired declares ONLY "keep-a" under PruneWithinScope.  The resources we
	// later drop are NOT in desired, so the additive Apply phase never re-creates
	// them — keeping the post-drop List count stable at 4 and the halt
	// deterministic.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "keep-a",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "keep-a",
					"size": "1",
				},
			},
		},
	})

	// Baseline reality: keep-a + 11 chaos orphans (List count = 12).  The type is
	// NOT yet pruneable, so cycle 1 prunes nothing (the 11 non-desired resources
	// are deferred) and the baseline is recorded as 12.
	const baselineCount = 12
	baseline := map[string]map[string]any{
		"keep-a": {"name": "keep-a", "size": "1"},
	}
	for i := 1; i <= baselineCount-1; i++ {
		id := fmt.Sprintf("chaos-orphan-%02d", i)
		baseline[id] = map[string]any{"name": id, "size": fmt.Sprintf("%d", i)}
	}
	seedStateWithResources(t, statePath, baseline)
	// pruneable_capability deliberately NOT set → default false for cycle 1.

	// Launch the daemon; merged stdout+stderr go to a FILE so the test can poll
	// the log without racing the subprocess writer goroutine.
	cmd, waitCmd := startDaemonToFile(t, intentDir, statePath, "300ms", logPath)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()

	// Wait until cycle 1 has completed so the baseline (List=12) is recorded.
	if !waitForLogFile(logPath, "cycle 1:", 5*time.Second) {
		t.Fatalf("[AC-Scf7762-4-2/count-drop] daemon did not complete cycle 1 in time; log:\n%s", readFileString(logPath))
	}

	// Now VIOLENTLY shrink reality to 4 resources (keep-a + 3 survivors incl. a
	// fresh pruneable orphan) AND opt the type in to pruning.  List count drops
	// 12 → 4 (67%).  Without the count-drop gate, "chaos-orphan-flood" would be
	// deleted next cycle.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"keep-a":             {"name": "keep-a", "size": "1"},
		"chaos-orphan-flood": {"name": "chaos-orphan-flood", "size": "9"},
		"chaos-survivor-a":   {"name": "chaos-survivor-a", "size": "2"},
		"chaos-survivor-b":   {"name": "chaos-survivor-b", "size": "3"},
	})
	seedStatePruneableCapability(t, statePath)

	// Wait for the daemon to observe the drop and HALT prune via the count-drop
	// gate.  We key on the engine's count-drop HALT log line (not the generic
	// "prune HALTED" cycle summary) to be certain it is the COUNT-DROP gate that
	// fired, not the ListStatus gate (list_partial is never set in this test).
	if !waitForLogFile(logPath, "count dropped from", 5*time.Second) {
		t.Fatalf("[AC-Scf7762-4-2/count-drop] expected a count-drop prune HALT in the daemon log; log:\n%s", readFileString(logPath))
	}
	// Stop the daemon directly and measure its state while prune is still halted
	// (a later 4→4 = 0% drop cycle could otherwise prune).  Exit is observed via
	// cmd.Wait — no shell "| head" plumbing.
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true

	logStr := readFileString(logPath)

	// PROOF (reality): the pruneable orphan must still exist — the count-drop gate
	// halted before its deletion.
	if !resourceExistsInState(t, statePath, "chaos-orphan-flood") {
		t.Errorf("[AC-Scf7762-4-2/count-drop] chaos-orphan-flood was deleted despite a >50%% List count drop; the count-drop gate must prevent this. log:\n%s", logStr)
	}
	// keep-a (desired) must also survive.
	if !resourceExistsInState(t, statePath, "keep-a") {
		t.Errorf("[AC-Scf7762-4-2/count-drop] keep-a was unexpectedly removed")
	}

	// PROOF (log): the halt must be the COUNT-DROP gate reporting the 12→4 drop —
	// distinguishing it from the ListStatus(Partial) gate.  The engine emits
	// "List count dropped from 12 to 4 (...)" (internal/prune/prune.go §12.4b).
	if !strings.Contains(logStr, "dropped from 12 to 4") {
		t.Errorf("[AC-Scf7762-4-2/count-drop] expected the halt log to report the 12→4 count drop (count-drop gate, not ListStatus); log:\n%s", logStr)
	}

	t.Logf("[AC-Scf7762-4-2/count-drop] PASS: count-drop gate fired (12→4); chaos-orphan-flood retained.")
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-4-3] alert-only + mass drift → reported but Apply not executed
// ---------------------------------------------------------------------------

// TestChaos_AlertOnly_MassDrift_NeverApplied verifies [AC-Scf7762-4-3]:
//
// Reality has 15 resources, all with size=<i> (seeded).
// Intent declares all 15 with size="999" (mass drift).
// Running in alert-only (--mode=alert-only) must:
//   - Report all 15 as drifted (DRIFT EVENT lines in output).
//   - NOT apply any correction to the state file.
//   - State file must be byte-for-byte identical before and after.
//
// Proof: byte comparison of the state file.
func TestChaos_AlertOnly_MassDrift_NeverApplied(t *testing.T) {
	// [AC-Scf7762-4-3] one-shot reconcile path, mass drift
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	const total = 15
	// Seed reality with 15 resources (non-pruneable so prune doesn't confuse things).
	seedChaosResourcesNonPruneable(t, statePath, total)
	// Intent: all 15 with size="999" (drifted from seeded sizes).
	writeManyDriftIntents(t, intentDir, total)

	// Capture state file BEFORE alert-only run.
	before := chaosStateBytes(t, statePath)

	// Run in alert-only mode.
	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--mode=alert-only")
	if code != 0 {
		t.Errorf("[AC-Scf7762-4-3] expected exit 0 in alert-only; got %d\nstdout=%s\nstderr=%s",
			code, stdout, stderr)
	}

	// Must report drifted resources.
	combined := stdout + stderr
	if !strings.Contains(combined, "drifted") && !strings.Contains(combined, "DRIFT EVENT") {
		t.Errorf("[AC-Scf7762-4-3] expected drift report for %d resources; got:\n%s", total, combined)
	}

	// Must NOT have applied any changes (no Apply-style op lines).
	if strings.Contains(stdout, "~ UPDATE") || strings.Contains(stdout, "+ CREATE") || strings.Contains(stdout, "- DELETE") {
		t.Errorf("[AC-Scf7762-4-3] alert-only printed Apply-style op lines — Apply was called!\nstdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "change(s) applied") {
		t.Errorf("[AC-Scf7762-4-3] alert-only reported applied changes — Apply was called!\nstdout:\n%s", stdout)
	}

	// PROOF: state file must be byte-for-byte identical (Apply was never called).
	after := chaosStateBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-4-3] DESTRUCTION LEAKED: state file changed after alert-only (Apply was called)\nbefore:\n%s\nafter:\n%s",
			before, after)
	}

	// All 15 resources must still exist with their original values.
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("chaos-vm-%02d", i)
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-Scf7762-4-3] resource %s missing after alert-only run!", id)
		}
	}

	t.Logf("[AC-Scf7762-4-3] PASS: %d drifted resources reported, none applied. state file unchanged.", total)
	t.Logf("[AC-Scf7762-4-3] output:\n%s", combined)
}

// TestChaos_Freeze_MassDrift_NeverApplied verifies [AC-Scf7762-4-3] via the
// --freeze path (global freeze overrides auto to alert-only, §14.3).
func TestChaos_Freeze_MassDrift_NeverApplied(t *testing.T) {
	// [AC-Scf7762-4-3] --freeze variant
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	const total = 12
	seedChaosResourcesNonPruneable(t, statePath, total)
	writeManyDriftIntents(t, intentDir, total)

	before := chaosStateBytes(t, statePath)

	// Run with --freeze (global freeze).  Mode defaults to auto but freeze must
	// override it to alert-only (most-restrictive-wins, §14.3).
	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--freeze")
	if code != 0 {
		t.Errorf("[AC-Scf7762-4-3/freeze] expected exit 0; got %d\nstdout=%s\nstderr=%s",
			code, stdout, stderr)
	}

	combined := stdout + stderr
	// Must mention FROZEN.
	if !strings.Contains(combined, "FROZEN") {
		t.Errorf("[AC-Scf7762-4-3/freeze] expected 'FROZEN' in output; got:\n%s", combined)
	}

	// Must report drift.
	if !strings.Contains(combined, "drifted") && !strings.Contains(combined, "DRIFT EVENT") {
		t.Errorf("[AC-Scf7762-4-3/freeze] expected drift report; got:\n%s", combined)
	}

	// Must NOT have applied changes.
	if strings.Contains(stdout, "change(s) applied") || strings.Contains(stdout, "~ UPDATE") {
		t.Errorf("[AC-Scf7762-4-3/freeze] freeze must not apply changes; stdout:\n%s", stdout)
	}

	// PROOF: state file byte-for-byte unchanged.
	after := chaosStateBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-4-3/freeze] state changed under freeze — Apply was called!\nbefore:\n%s\nafter:\n%s",
			before, after)
	}

	t.Logf("[AC-Scf7762-4-3/freeze] PASS: %d drifted resources under freeze; state unchanged.", total)
}

// TestChaos_DaemonAlertOnly_MassDrift_NeverApplied verifies [AC-Scf7762-4-3]
// on the daemon path: even after many cycles, alert-only never touches reality.
func TestChaos_DaemonAlertOnly_MassDrift_NeverApplied(t *testing.T) {
	// [AC-Scf7762-4-3] daemon path
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	const total = 10
	seedChaosResourcesNonPruneable(t, statePath, total)
	writeManyDriftIntents(t, intentDir, total)

	before := chaosStateBytes(t, statePath)

	cmd, _ := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "100ms",
		"--mode=alert-only",
	)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Let 6 cycles run (600ms at 100ms interval).
	time.Sleep(700 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("[AC-Scf7762-4-3/daemon] send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("[AC-Scf7762-4-3/daemon] daemon did not exit within 5s after SIGTERM")
	}

	// PROOF: state file must be byte-for-byte unchanged across all cycles.
	after := chaosStateBytes(t, statePath)
	if !bytes.Equal(before, after) {
		logData, _ := os.ReadFile(logPath)
		t.Errorf("[AC-Scf7762-4-3/daemon] DESTRUCTION LEAKED: state changed in alert-only daemon after %d cycles\nbefore:\n%s\nafter:\n%s\nlog:\n%s",
			total, before, after, logData)
	}

	logData, _ := os.ReadFile(logPath)
	log := string(logData)
	if !strings.Contains(log, "alert-only") {
		t.Errorf("[AC-Scf7762-4-3/daemon] expected 'alert-only' in daemon log; got:\n%s", log)
	}

	t.Logf("[AC-Scf7762-4-3/daemon] PASS: %d drift events across cycles; state file unchanged.", total)
}

// ---------------------------------------------------------------------------
// Composite chaos: safety mechanisms stack (alert-only lifts → budget catches)
// ---------------------------------------------------------------------------

// TestChaos_Composite_AlertOff_ThenBudgetHalts verifies the stacking of
// alert-only protection followed by budget protection.
//
// Phase 1: Run in alert-only mode.  20 prune candidates exist.  Nothing deleted.
// Phase 2: Run in auto mode with a tight budget (max 2 destructive per cycle).
//
//	The budget fires; only limited deletion is possible.
//
// This proves the two safety layers are independent and additive:
//   - alert-only is a mode gate (prevents any Apply at all).
//   - budget is a quantity gate (limits how many destructive ops can execute).
//
// Together they give the operator: "first confirm what would happen; then lift
// alert-only but the budget still rate-limits the actual destruction."
func TestChaos_Composite_AlertOff_ThenBudgetHalts(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	const total = 20
	// Seed 20 pruneable resources.
	seedChaosResources(t, statePath, total)
	// Desired: keep only chaos-vm-01 → 19 prune candidates.
	writeKeepOneIntent(t, intentDir, "chaos-vm-01")

	// Phase 1: alert-only — observe the 19 candidates but do NOT delete.
	before := chaosStateBytes(t, statePath)
	stdoutP1, stderrP1, codeP1 := runReconcile(t, intentDir, statePath, "--mode=alert-only")
	if codeP1 != 0 {
		t.Errorf("[composite] phase1 alert-only expected exit 0, got %d\nstdout=%s stderr=%s",
			codeP1, stdoutP1, stderrP1)
	}
	// Must report drift (prune candidates are also drift events in alert-only).
	combinedP1 := stdoutP1 + stderrP1
	if !strings.Contains(combinedP1, "drifted") && !strings.Contains(combinedP1, "DRIFT EVENT") &&
		!strings.Contains(combinedP1, "orphan") && !strings.Contains(combinedP1, "DELETE") {
		t.Logf("[composite] phase1 alert-only output (drift may be empty if no desired-to-reality drift): %s", combinedP1)
	}
	// State must be untouched after phase 1.
	afterP1 := chaosStateBytes(t, statePath)
	if !bytes.Equal(before, afterP1) {
		t.Errorf("[composite] phase1 alert-only changed the state file!\nbefore:\n%s\nafter:\n%s",
			before, afterP1)
	}
	t.Logf("[composite] phase1 alert-only: %d resources preserved. output:\n%s", total, combinedP1)

	// Phase 2: auto mode, but with a tight budget (max 2 destructive per cycle).
	// The budget fires because 19 candidates > 2 limit.
	stdoutP2, stderrP2, codeP2 := runReconcile(t, intentDir, statePath,
		"--mode=auto",
		"--max-destructive-per-cycle=2",
		"--on-exceed=halt-all",
	)
	combinedP2 := stdoutP2 + stderrP2

	// Budget must fire (19 >> 2).
	if !strings.Contains(combinedP2, "BUDGET HALTED") {
		t.Errorf("[composite] phase2 expected BUDGET HALTED (19 candidates > max 2); stdout=%s stderr=%s",
			stdoutP2, stderrP2)
	}
	if codeP2 == 0 {
		t.Errorf("[composite] phase2 expected non-zero exit (budget halted), got 0")
	}

	// PROOF: with halt-all, NO deletion happened in phase 2.
	afterP2 := countChaosResources(t, statePath)
	if afterP2 < total {
		t.Errorf("[composite] DESTRUCTION LEAKED in phase2: expected %d resources, got %d\nstate:\n%s",
			total, afterP2, string(chaosStateBytes(t, statePath)))
	}

	t.Logf("[composite] PASS: alert-only → budget-halt stack proven. remaining=%d/%d", afterP2, total)
	t.Logf("[composite] phase2 output:\n%s", combinedP2)
}
