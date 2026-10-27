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

// inventory_oneshot_test.go — Backlog #115 (#109 の 3): the one-shot status,
// plan and reconcile compose render phase 1's inventory (--inventory-file)
// the same way the daemon does (§9.1, §9.2, §26.6).
//
// Tags:
//   [AC-114-1-1] status: an inventory resource is reported as managed (not
//                UNMANAGED), and a missing one as drifted
//   [AC-114-1-2] plan: an inventory resource that is not in reality is planned
//   [AC-114-1-3] reconcile: the inventory resources converge (status then
//                reports no drift)
//   [AC-114-1-4] below --inventory-expected-min, or unreadable, all three
//                exit 1 and touch nothing (incomplete≠empty); a one-shot run
//                has no previous count, so --inventory-min-ratio has no effect

package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupInventoryOneShot prepares an intent with managed-vm, an inventory of 3
// (inv-vm-01..03) and a reality holding managed-vm and inv-vm-01..02.
func setupInventoryOneShot(t *testing.T) (intentDir, statePath, invPath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	invPath = filepath.Join(tmp, "inventory.json")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "managed-vm", "fields": map[string]string{"name": "managed-vm"}},
		},
	})
	writeInventoryFile(t, invPath, 3)
	seedState(t, statePath, map[string]map[string]any{
		"managed-vm": {"name": "managed-vm"},
		"inv-vm-01":  {"name": "inv-vm-01"},
		"inv-vm-02":  {"name": "inv-vm-02"},
	})
	return intentDir, statePath, invPath
}

func TestInventoryOneShot_Status(t *testing.T) {
	intentDir, statePath, invPath := setupInventoryOneShot(t)

	// Control: without --inventory-file the inventory's reality is UNMANAGED.
	stdout, stderr, code := runStatusCmd(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-114-1-1] status exited %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "UNMANAGED fake//inv-vm-01") {
		t.Fatalf("[AC-114-1-1] control: without --inventory-file inv-vm-01 should be unmanaged; got:\n%s", stdout)
	}

	stdout, stderr, code = runStatusCmd(t, intentDir, statePath, "--inventory-file", invPath)
	if code != 0 {
		t.Fatalf("[AC-114-1-1] status --inventory-file exited %d; stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "UNMANAGED") {
		t.Errorf("[AC-114-1-1] inventory resources must be managed, not UNMANAGED; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "DRIFTED fake//inv-vm-03") {
		t.Errorf("[AC-114-1-1] the missing inventory resource inv-vm-03 must be drifted; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "drifted=1 converged=3 unobservable=0 unmanaged=0") {
		t.Errorf("[AC-114-1-1] summary should count the inventory as managed; got:\n%s", stdout)
	}
}

func TestInventoryOneShot_Plan(t *testing.T) {
	intentDir, statePath, invPath := setupInventoryOneShot(t)

	stdout, stderr, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-114-1-2] plan exited %d; stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "inv-vm-03") {
		t.Fatalf("[AC-114-1-2] control: without --inventory-file inv-vm-03 is not desired; got:\n%s", stdout)
	}

	stdout, stderr, code = runPlan(t, intentDir, statePath, "--inventory-file", invPath)
	if code != 0 {
		t.Fatalf("[AC-114-1-2] plan --inventory-file exited %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "CREATE") || !strings.Contains(stdout, "inv-vm-03") {
		t.Errorf("[AC-114-1-2] plan must create the inventory resource inv-vm-03; got:\n%s", stdout)
	}
	for _, id := range []string{"inv-vm-01", "inv-vm-02", "managed-vm"} {
		if strings.Contains(stdout, id) {
			t.Errorf("[AC-114-1-2] %s already matches reality and must not be planned; got:\n%s", id, stdout)
		}
	}
	// The inventory is an external input: rendered_revision moves off intent_revision.
	statusOut, _, _ := runStatusCmd(t, intentDir, statePath, "--inventory-file", invPath)
	if rev := revisionLine(stdout); rev == "" || !strings.Contains(statusOut, rev) {
		t.Errorf("[AC-114-1-2] plan and status must render the same revision %q; status:\n%s", rev, statusOut)
	}
}

// revisionLine returns the "intent_revision=… rendered_revision=…" line.
func revisionLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "intent_revision=") {
			return l
		}
	}
	return ""
}

func TestInventoryOneShot_ReconcileConverges(t *testing.T) {
	intentDir, statePath, invPath := setupInventoryOneShot(t)

	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--inventory-file", invPath)
	if code != 0 {
		t.Fatalf("[AC-114-1-3] reconcile --inventory-file exited %d; stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
	for _, id := range []string{"managed-vm", "inv-vm-01", "inv-vm-02", "inv-vm-03"} {
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-114-1-3] %s missing from reality after reconcile; stdout:\n%s", id, stdout)
		}
	}
	statusOut, stderr, code := runStatusCmd(t, intentDir, statePath, "--inventory-file", invPath)
	if code != 0 {
		t.Fatalf("[AC-114-1-3] status exited %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(statusOut, "drifted=0 converged=4 unobservable=0 unmanaged=0") {
		t.Errorf("[AC-114-1-3] after reconcile everything should be converged; got:\n%s", statusOut)
	}
}

func TestInventoryOneShot_IncompleteExits1(t *testing.T) {
	intentDir, statePath, invPath := setupInventoryOneShot(t)
	bin := buildBinary(t)
	before := readStateFile(t, statePath)

	run := func(args ...string) (string, int) {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return string(out), ee.ExitCode()
			}
			t.Fatal(err)
		}
		return string(out), 0
	}

	for _, verb := range []string{"status", "plan", "reconcile"} {
		base := []string{verb, "--intent", intentDir, "--state", statePath}

		// 3 items < expected 5: incomplete, not empty.
		out, code := run(append(base, "--inventory-file", invPath, "--inventory-expected-min", "5")...)
		if code != 1 {
			t.Errorf("[AC-114-1-4] %s below --inventory-expected-min exited %d, want 1; output:\n%s", verb, code, out)
		}
		if !strings.Contains(out, "InventoryIncomplete: inventory has 3 item(s), fewer than the expected 5") {
			t.Errorf("[AC-114-1-4] %s must report InventoryIncomplete; output:\n%s", verb, out)
		}

		// An unreadable inventory: nothing to fall back to in a one-shot run.
		out, code = run(append(base, "--inventory-file", filepath.Join(filepath.Dir(invPath), "missing.json"))...)
		if code != 1 || !strings.Contains(out, "ExternalInputUnreachable") {
			t.Errorf("[AC-114-1-4] %s with an unreadable inventory exited %d, want 1 with ExternalInputUnreachable; output:\n%s", verb, code, out)
		}

		// No previous count, so the ratio cannot halt a one-shot run.
		if verb != "reconcile" {
			out, code = run(append(base, "--inventory-file", invPath, "--inventory-min-ratio", "0.99")...)
			if code != 0 {
				t.Errorf("[AC-114-1-4] %s: --inventory-min-ratio must have no effect without a previous count; exited %d:\n%s", verb, code, out)
			}
		}

		// The help says so.
		out, _ = run(verb, "-h")
		if !strings.Contains(out, "-inventory-file") || !strings.Contains(out, "the only drop check in a one-shot run") {
			t.Errorf("[AC-114-1-4] %s -h must document the one-shot inventory drop check; output:\n%s", verb, out)
		}
	}

	if after := readStateFile(t, statePath); string(after) != string(before) {
		t.Errorf("[AC-114-1-4] reality changed although every run halted:\nbefore: %s\nafter: %s", before, after)
	}
}
