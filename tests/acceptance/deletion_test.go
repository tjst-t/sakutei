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

// deletion_test.go contains CLI-level acceptance tests for Story Scf7762-1.
//
// Scenario: "削除の非対称: 確信が持てない削除をしない"
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, exit code, and side effects on the state file.
// No internal package is imported directly; only the binary's observable
// behaviour is verified.
//
// AC tags map to scenario-Scf7762-1.json:
//
//	[AC-Scf7762-1-1] pruneable:false (default) — resource in reality but absent
//	                 from desired is NOT deleted.
//	[AC-Scf7762-1-2] List drop-rate halt — >50% drop in list count halts prune;
//	                 no resources are deleted.
//	[AC-Scf7762-1-3] unknown ≠ absent — Observe returns Unknown for a resource;
//	                 prune engine does not delete it.
//
// Positive case (not tagged as AC in scenario file but required by the story):
//
//	pruneable:true + PruneWithinScope — resource in reality but absent from
//	desired IS deleted (normal prune path).
package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// seedStateWithResources writes a state file containing the given resources.
// resources is a map[local_id]map[field_name]value.
func seedStateWithResources(t *testing.T, statePath string, resources map[string]map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(map[string]any{
		"resources": func() map[string]any {
			m := make(map[string]any, len(resources))
			for local, fields := range resources {
				m[local] = map[string]any{"fields": fields}
			}
			return m
		}(),
		"provider_version": testProviderVersion,
	}, "", "  ")
	if err != nil {
		t.Fatalf("seedStateWithResources: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateWithResources: write: %v", err)
	}
}

// seedStateListPartial adds list_partial:true to an existing state file,
// simulating an incomplete List call to trigger the prune halt guard.
func seedStateListPartial(t *testing.T, statePath string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("seedStateListPartial: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("seedStateListPartial: unmarshal: %v", err)
	}
	sf["list_partial"] = true
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStateListPartial: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateListPartial: write: %v", err)
	}
}

// seedStatePruneableCapability sets the pruneable_capability flag in an existing
// state file, opting the fake provider TYPE in to automatic deletion.
//
// Scf7762-1: pruneability is a TYPE-level provider capability
// (Capabilities().Pruneable), not a per-instance intent flag.  CLI tests that
// exercise the deletion path therefore opt the type in here rather than via a
// "pruneable":true field in the intent (which is now ignored).
func seedStatePruneableCapability(t *testing.T, statePath string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("seedStatePruneableCapability: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("seedStatePruneableCapability: unmarshal: %v", err)
	}
	sf["pruneable_capability"] = true
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStatePruneableCapability: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedStatePruneableCapability: write: %v", err)
	}
}

// seedStateObserveUnknown adds an observe_unknown entry for the given local ID,
// simulating an unreachable target (Availability=Unknown).
func seedStateObserveUnknown(t *testing.T, statePath, localID string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("seedStateObserveUnknown: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("seedStateObserveUnknown: unmarshal: %v", err)
	}
	ou, _ := sf["observe_unknown"].(map[string]any)
	if ou == nil {
		ou = make(map[string]any)
	}
	ou[localID] = "unknown"
	sf["observe_unknown"] = ou
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStateObserveUnknown: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateObserveUnknown: write: %v", err)
	}
}

// resourceExistsInState reports whether the given local resource ID exists in
// the state file (i.e. is still present in reality).
func resourceExistsInState(t *testing.T, statePath, localID string) bool {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatalf("resourceExistsInState: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("resourceExistsInState: unmarshal: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	_, exists := resources[localID]
	return exists
}

// ---------------------------------------------------------------------------
// AC-Scf7762-1-1: pruneable:false default — absent-from-desired not deleted
// ---------------------------------------------------------------------------

// TestDeletion_PruneableDefaultFalse verifies [AC-Scf7762-1-1].
//
// Setup: reality contains "orphan-vm" (not in desired).
//
//	desired declares "desired-vm" with DeletionPolicy=PruneWithinScope.
//	The fake provider TYPE is NOT opted in to pruning (no
//	pruneable_capability flag → Capabilities().Pruneable defaults to
//	false, ADR-0005).
//
// Expected: reconcile exits 0, "orphan-vm" remains in the state file.
func TestDeletion_PruneableDefaultFalse(t *testing.T) {
	// [AC-Scf7762-1-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality: "orphan-vm" exists but is not in desired.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"orphan-vm":  {"name": "orphan-vm", "size": "5"},
		"desired-vm": {"name": "desired-vm", "size": "10"},
	})

	// Desired: "desired-vm" with PruneWithinScope but pruneable:false (default).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "desired-vm",
				"deletion_policy": "PruneWithinScope",
				// Type is not opted in to pruning → Capabilities().Pruneable=false.
				"fields": map[string]string{
					"name": "desired-vm",
					"size": "10",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Scf7762-1-1] expected exit 0, got %d; stdout=%s stderr=%s", code, stdout, stderr)
	}

	// "orphan-vm" must still exist in reality (not deleted).
	if !resourceExistsInState(t, statePath, "orphan-vm") {
		t.Errorf("[AC-Scf7762-1-1] orphan-vm was deleted, but pruneable:false (default) must prevent deletion")
	}

	// No DELETE op should appear in stdout.
	if strings.Contains(stdout, "DELETE") {
		t.Errorf("[AC-Scf7762-1-1] DELETE op appeared in stdout; expected no deletion when pruneable:false; stdout=%s", stdout)
	}
	t.Logf("[AC-Scf7762-1-1] PASS: orphan-vm not deleted (pruneable:false default). stdout=%s stderr=%s", stdout, stderr)
}

// ---------------------------------------------------------------------------
// AC-Scf7762-1-2: List drop-rate halt — prune halted, no deletion
// ---------------------------------------------------------------------------

// TestDeletion_ListDropHalt verifies [AC-Scf7762-1-2].
//
// Setup: reality contains many resources; desired declares PruneWithinScope
//
//	with pruneable:true; but ListPartial=true simulates a >50% drop
//	in list count, which must trigger the halt safety guard.
//
// Expected: reconcile does NOT delete any resources; prune is halted.
//
// Implementation note: we use the list_partial flag (ListPartial status) as the
// mechanism to trigger the halt guard.  The prune engine also halts on >50%
// count drop.  We test the ListStatus gate here (§12.4c); the count-drop gate
// (§12.4b) is tested inline as well using the drop simulation.
func TestDeletion_ListDropHalt(t *testing.T) {
	// [AC-Scf7762-1-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with several resources.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"vm-01": {"name": "vm-01", "size": "1"},
		"vm-02": {"name": "vm-02", "size": "2"},
		"vm-03": {"name": "vm-03", "size": "3"},
		"vm-04": {"name": "vm-04", "size": "4"},
		"vm-05": {"name": "vm-05", "size": "5"},
	})

	// Mark List as partial (ListPartial status) — simulates an incomplete List.
	seedStateListPartial(t, statePath)
	// Opt the TYPE in to pruning so the List-status halt is the ONLY reason no
	// resource is deleted (isolates the guard under test).
	seedStatePruneableCapability(t, statePath)

	// Desired: only "vm-01" with PruneWithinScope.
	// Without the halt guard, "vm-02"..."vm-05" would be deletion candidates.
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
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Scf7762-1-2] expected exit 0, got %d; stdout=%s stderr=%s", code, stdout, stderr)
	}

	// None of the resources should have been deleted.
	for _, id := range []string{"vm-01", "vm-02", "vm-03", "vm-04", "vm-05"} {
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-Scf7762-1-2] %s was deleted, but prune should be halted (list_partial)", id)
		}
	}

	// No DELETE op should appear in stdout.
	if strings.Contains(stdout, "DELETE") {
		t.Errorf("[AC-Scf7762-1-2] DELETE op in stdout; prune should be halted; stdout=%s", stdout)
	}
	t.Logf("[AC-Scf7762-1-2] PASS: no resources deleted (List drop halt). stdout=%s stderr=%s", stdout, stderr)
}

// TestDeletion_CountDropHalt_Daemon verifies [AC-Scf7762-1-2] via the
// cross-cycle List count-drop path, exercised against the long-lived DAEMON.
//
// The count-drop guard (§12.4b) is inherently cross-cycle: it compares this
// cycle's List count against the previous cycle's.  Only the daemon holds a
// single prune.Engine across cycles (the one-shot "sakutei reconcile" resets the
// baseline every process, so its count-drop guard can never fire — see
// cmd/sakutei/reconcile.go).  We therefore drive the daemon subprocess:
//
//	cycle 1: List returns N=6 (baseline recorded), nothing pruned (type not yet
//	         pruneable, so the 5 non-desired resources are merely deferred).
//	then:    reality is externally shrunk to 2 resources (a >50% drop) and the
//	         type is opted in to pruning, introducing a fresh pruneable orphan.
//	next cycle: List returns 2 → 67% drop > 50% threshold → prune HALTS; the
//	         orphan is NOT deleted this cycle.
//
// We assert on BOTH the log (the HALT line) AND reality (the orphan survives),
// and we measure the daemon by killing it directly — no pipe, no "| head", so
// the check is not confused by shell plumbing.  The process is always killed.
func TestDeletion_CountDropHalt_Daemon(t *testing.T) {
	// [AC-Scf7762-1-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Desired declares ONLY "keep-a" with PruneWithinScope.  Crucially, the
	// resources we later drop are NOT in desired, so the additive Apply phase
	// never re-creates them — keeping the post-drop List count stable at 2 and
	// making the halt deterministic.
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

	// Baseline reality: keep-a + 5 others (List count = 6).  The type is NOT yet
	// pruneable, so cycle 1 prunes nothing (the 5 non-desired resources are
	// deferred) and the baseline is recorded as 6.
	baseline := map[string]map[string]any{
		"keep-a": {"name": "keep-a", "size": "1"},
	}
	for _, c := range []string{"b", "c", "d", "e", "f"} {
		baseline["pad-"+c] = map[string]any{"name": "pad-" + c, "size": "1"}
	}
	seedStateWithResources(t, statePath, baseline)
	// pruneable_capability deliberately NOT set here → default false for cycle 1.

	// Launch the daemon with its merged stdout/stderr written to a FILE (not an
	// in-memory buffer), so the test can poll the log without racing the
	// subprocess's writer goroutine on a shared bytes.Buffer.
	logPath := filepath.Join(tmp, "daemon.log")
	cmd, waitCmd := startDaemonToFile(t, intentDir, statePath, "300ms", logPath)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()

	// Wait until cycle 1 has completed so the baseline (List=6) is recorded.
	// We key on the first cycle summary line the daemon prints each cycle.
	if !waitForLogFile(logPath, "cycle 1:", 5*time.Second) {
		t.Fatalf("[AC-Scf7762-1-2] daemon did not complete cycle 1 in time; log:\n%s", readFileString(logPath))
	}

	// Now externally shrink reality to 2 resources (keep-a + a fresh pruneable
	// orphan) AND opt the type in to pruning.  List count drops 6 → 2 (67%).
	// Without the count-drop guard, "orphan-target" would be deleted.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"keep-a":        {"name": "keep-a", "size": "1"},
		"orphan-target": {"name": "orphan-target", "size": "9"},
	})
	seedStatePruneableCapability(t, statePath)

	// Wait for the daemon to observe the drop and HALT prune.  We kill the daemon
	// as soon as we see the HALT so the assertion is made while prune is halted
	// (a later cycle would compare 2→2 = 0% drop and could then prune).
	if !waitForLogFile(logPath, "prune HALT", 5*time.Second) {
		t.Fatalf("[AC-Scf7762-1-2] expected a count-drop prune HALT in the daemon log; log:\n%s", readFileString(logPath))
	}
	// Stop the daemon directly and measure its state — no shell pipe involved,
	// exit is observed via cmd.Wait, not through any "| head" plumbing.
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true

	logStr := readFileString(logPath)

	// Reality check: the orphan must still exist (halt prevented its deletion).
	if !resourceExistsInState(t, statePath, "orphan-target") {
		t.Errorf("[AC-Scf7762-1-2] orphan-target was deleted despite a >50%% List count drop; count-drop halt must prevent this. log:\n%s", logStr)
	}
	// keep-a (desired) must also still exist.
	if !resourceExistsInState(t, statePath, "keep-a") {
		t.Errorf("[AC-Scf7762-1-2] keep-a was unexpectedly removed")
	}

	// The halt must be reported in the log with the drop ratio.
	if !strings.Contains(logStr, "dropped from 6 to 2") {
		t.Errorf("[AC-Scf7762-1-2] expected the halt log to report the 6→2 count drop; log:\n%s", logStr)
	}
	t.Logf("[AC-Scf7762-1-2] PASS: count-drop halt fired; orphan-target retained.")
}

// startDaemonToFile launches "sakutei --intent ... --state ... --interval ..."
// with merged stdout+stderr written to logPath.  It returns the *exec.Cmd and a
// wait closure.  Writing to a file (rather than an in-memory buffer) lets the
// test poll the log via the filesystem without racing the subprocess writer.
//
// The caller MUST ensure the process is eventually killed and waited on.
func startDaemonToFile(t *testing.T, intentDir, statePath, interval, logPath string) (*exec.Cmd, func() error) {
	t.Helper()
	bin := buildBinary(t)

	logF, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("startDaemonToFile: create log file: %v", err)
	}

	cmd := exec.Command(bin,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", interval,
	)
	cmd.Stdout = logF
	cmd.Stderr = logF

	if err := cmd.Start(); err != nil {
		_ = logF.Close()
		t.Fatalf("startDaemonToFile: Start: %v", err)
	}
	// wait closes the log file after the process exits so all output is flushed.
	wait := func() error {
		err := cmd.Wait()
		_ = logF.Close()
		return err
	}
	return cmd, wait
}

// waitForLogFile polls logPath until its content contains substr or the deadline
// passes.  Returns true on match.
func waitForLogFile(logPath, substr string, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), substr) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// readFileString reads logPath and returns its content as a string (empty on
// error).  Used only for diagnostic messages.
func readFileString(logPath string) string {
	data, _ := os.ReadFile(logPath)
	return string(data)
}

// ---------------------------------------------------------------------------
// AC-Scf7762-1-3: unknown ≠ absent — Observe Unknown → not deleted
// ---------------------------------------------------------------------------

// TestDeletion_UnknownNotAbsent verifies [AC-Scf7762-1-3].
//
// Setup: reality contains "unreachable-vm" which is absent from desired AND
//
//	whose Observe returns AvailabilityUnknown (via observe_unknown inject).
//
// Expected: reconcile does NOT delete "unreachable-vm" (unknown ≠ absent).
func TestDeletion_UnknownNotAbsent(t *testing.T) {
	// [AC-Scf7762-1-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality: "unreachable-vm" exists but is not in desired.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"desired-vm":     {"name": "desired-vm", "size": "10"},
		"unreachable-vm": {"name": "unreachable-vm", "size": "99"},
	})

	// Mark "unreachable-vm" as observe-unknown (simulates target unreachable).
	seedStateObserveUnknown(t, statePath, "unreachable-vm")
	// Opt the TYPE in to pruning so the unknown≠absent gate is the ONLY reason
	// "unreachable-vm" survives (isolates the guard under test).
	seedStatePruneableCapability(t, statePath)

	// Desired: only "desired-vm" with PruneWithinScope.
	// "unreachable-vm" is absent from desired; without the unknown≠absent guard
	// it would be a deletion candidate.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "desired-vm",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "desired-vm",
					"size": "10",
				},
			},
		},
	})

	// reconcile exits 1 because "unreachable-vm" is unobservable (D-1 exit policy).
	// That is expected: the important check is that no DELETE was applied.
	stdout, stderr, _ := runReconcile(t, intentDir, statePath)

	// "unreachable-vm" must NOT have been deleted.
	if !resourceExistsInState(t, statePath, "unreachable-vm") {
		t.Errorf("[AC-Scf7762-1-3] unreachable-vm was deleted, but unknown ≠ absent must prevent deletion")
	}

	// No DELETE op targeting unreachable-vm should appear in stdout.
	if strings.Contains(stdout, "DELETE") && strings.Contains(stdout, "unreachable-vm") {
		t.Errorf("[AC-Scf7762-1-3] DELETE for unreachable-vm in stdout; unknown ≠ absent must prevent this; stdout=%s", stdout)
	}
	t.Logf("[AC-Scf7762-1-3] PASS: unreachable-vm not deleted (unknown ≠ absent). stdout=%s stderr=%s", stdout, stderr)
}

// ---------------------------------------------------------------------------
// Positive case: pruneable:true + PruneWithinScope → resource deleted
// ---------------------------------------------------------------------------

// TestDeletion_PruneWithinScope_NormalPath verifies the normal prune path:
// a resource present in reality but absent from desired, with DeletionPolicy=
// PruneWithinScope and pruneable:true, IS deleted.
func TestDeletion_PruneWithinScope_NormalPath(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality: "to-delete-vm" exists but is not in desired.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"desired-vm":   {"name": "desired-vm", "size": "10"},
		"to-delete-vm": {"name": "to-delete-vm", "size": "5"},
	})
	// Opt the fake TYPE in to pruning (Capabilities().Pruneable=true).
	seedStatePruneableCapability(t, statePath)

	// Desired: only "desired-vm" with PruneWithinScope.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "desired-vm",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "desired-vm",
					"size": "10",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("PruneWithinScope normal path: expected exit 0, got %d; stderr=%s", code, stderr)
	}

	// "to-delete-vm" must have been deleted from reality.
	if resourceExistsInState(t, statePath, "to-delete-vm") {
		t.Errorf("PruneWithinScope normal path: to-delete-vm was NOT deleted; expected deletion with pruneable:true")
	}

	// "desired-vm" must still exist.
	if !resourceExistsInState(t, statePath, "desired-vm") {
		t.Errorf("PruneWithinScope normal path: desired-vm was unexpectedly deleted")
	}

	// DELETE op should appear in stdout.
	if !strings.Contains(stdout, "DELETE") {
		t.Errorf("PruneWithinScope normal path: expected DELETE in stdout; got: %s", stdout)
	}
	t.Logf("PruneWithinScope normal path PASS: to-delete-vm deleted. stdout=%s stderr=%s", stdout, stderr)
}

// ---------------------------------------------------------------------------
// Extra: ExplicitAbsentOnly (open-world default) never prunes
// ---------------------------------------------------------------------------

// TestDeletion_ExplicitAbsentOnly_NoPrune verifies that with the default
// DeletionPolicy (ExplicitAbsentOnly), resources absent from desired are
// never deleted even when the provider supports List.
func TestDeletion_ExplicitAbsentOnly_NoPrune(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed: "extra-vm" is in reality but not in desired.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"my-vm":    {"name": "my-vm", "size": "10"},
		"extra-vm": {"name": "extra-vm", "size": "5"},
	})

	// Desired: only "my-vm" with NO deletion_policy (defaults to ExplicitAbsentOnly).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "my-vm",
				"fields": map[string]string{
					"name": "my-vm",
					"size": "10",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("ExplicitAbsentOnly: expected exit 0, got %d; stderr=%s", code, stderr)
	}

	// "extra-vm" must NOT be deleted (open-world: intent doesn't manage it).
	if !resourceExistsInState(t, statePath, "extra-vm") {
		t.Errorf("ExplicitAbsentOnly: extra-vm was deleted; open-world (ExplicitAbsentOnly) must never auto-prune")
	}

	if strings.Contains(stdout, "DELETE") {
		t.Errorf("ExplicitAbsentOnly: DELETE in stdout; expected none; stdout=%s", stdout)
	}
	t.Logf("ExplicitAbsentOnly PASS: extra-vm not deleted. stdout=%s stderr=%s", stdout, stderr)
}
