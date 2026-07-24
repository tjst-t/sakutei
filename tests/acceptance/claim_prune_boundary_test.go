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

// claim_prune_boundary_test.go — FIX #6 regression guard.
//
// FIX #6 (BLOCKER — data loss): --claim narrows `desired` before Observe/Diff,
// but the prune path listed ALL reality and did set-difference against the
// claim-narrowed desired.  An OUT-OF-CLAIM resource declared PruneWithinScope
// (present in reality, correctly absent from the claim's desired set) looked
// like an orphan and was DELETED — silent data loss across the claim boundary.
//
// The fix scopes the prune boundary to the claim: reality IDs whose scope does
// not match the --claim selector are dropped BEFORE the set-difference, so an
// out-of-claim resource is never treated as an orphan (§17.1: "claim にマッチ
// しない scope は Observe すらしない"; 原則3: 確信なき減算はしない).
//
// AC tags:
//
//	[AC-Sf6a62f-3-4]  --claim excludes non-matching scopes from convergence AND
//	                  from the prune boundary (this file adds the prune-boundary
//	                  case; claim_selector_test.go covers the Observe/Apply case).
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedStateScopes writes a per-resource scope map into an existing state file so
// the fake provider's List returns properly-scoped IDs.  scopes maps local ID →
// label set.  This lets the --claim prune-boundary filter (FIX #6) recognise
// out-of-claim reality; without it List returns empty-scope IDs (the default,
// preserved for every other test).
func seedStateScopes(t *testing.T, statePath string, scopes map[string]map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("seedStateScopes: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("seedStateScopes: unmarshal: %v", err)
	}
	m := make(map[string]any, len(scopes))
	for local, labels := range scopes {
		lm := make(map[string]any, len(labels))
		for k, v := range labels {
			lm[k] = v
		}
		m[local] = lm
	}
	sf["scopes"] = m
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStateScopes: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateScopes: write: %v", err)
	}
}

// TestClaimPruneBoundary_OutOfClaimOrphanSurvives is the FIX #6 regression test.
//
// Setup:
//   - reality (fake state file) holds TWO resources, BOTH already present:
//     "tokyo-vm"  scope {site: tokyo}  (in-claim)
//     "osaka-vm"  scope {site: osaka}  (out-of-claim)
//     The fake TYPE is opted in to pruning (Capabilities().Pruneable=true).
//   - intent declares ONLY "tokyo-vm", with DeletionPolicy=PruneWithinScope so
//     the prune engine runs for the tokyo scope.  "osaka-vm" is NOT declared.
//   - reconcile runs with --claim site=tokyo, which narrows `desired` to the
//     tokyo resource only.
//
// Before FIX #6: the prune path listed BOTH reality resources and did
// set-difference against the tokyo-only desired set, so "osaka-vm" (out of
// claim, absent from desired) was treated as an orphan and DELETED — data loss.
//
// After FIX #6: "osaka-vm"'s listed scope {site: osaka} does not match the claim
// {site: tokyo}, so it is dropped before the set-difference — it is never an
// orphan candidate and SURVIVES.  It must not be planned for deletion either
// (no DELETE line for it in stdout).
//
// [AC-Sf6a62f-3-4]
func TestClaimPruneBoundary_OutOfClaimOrphanSurvives(t *testing.T) {
	// [AC-Sf6a62f-3-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Reality holds both resources; opt the fake TYPE in to pruning; attach the
	// scopes so List reports each resource with its real scope.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"tokyo-vm": {"name": "tokyo-vm", "size": "1"},
		"osaka-vm": {"name": "osaka-vm", "size": "2"},
	})
	seedStatePruneableCapability(t, statePath)
	seedStateScopes(t, statePath, map[string]map[string]string{
		"tokyo-vm": {"site": "tokyo"},
		"osaka-vm": {"site": "osaka"},
	})

	// Intent declares ONLY the in-claim resource, with PruneWithinScope so the
	// prune engine runs.  The out-of-claim osaka-vm is deliberately absent.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "tokyo-vm",
				"scope":           map[string]string{"site": "tokyo"},
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "tokyo-vm",
					"size": "1",
				},
			},
		},
	})

	// Reconcile scoped to the tokyo claim.
	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--claim", "site=tokyo")
	if code != 0 {
		t.Fatalf("[AC-Sf6a62f-3-4] reconcile exited %d; stderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}

	// The out-of-claim resource MUST survive — it is outside the claim boundary
	// and must never be treated as an orphan (this is the data-loss guard).
	if !resourceExistsInState(t, statePath, "osaka-vm") {
		t.Errorf("[AC-Sf6a62f-3-4] DATA LOSS: out-of-claim 'osaka-vm' was deleted by the prune path; "+
			"it must survive because it is outside --claim site=tokyo.\nstdout:\n%s", stdout)
	}
	// It must not even be planned for deletion.
	if strings.Contains(stdout, "osaka-vm") {
		t.Errorf("[AC-Sf6a62f-3-4] out-of-claim 'osaka-vm' appeared in reconcile output (planned for a change) "+
			"but must be excluded from the claim entirely:\n%s", stdout)
	}
	// The in-claim resource must still be present (it was already in reality and
	// converged; it must not have been collateral-pruned).
	if !resourceExistsInState(t, statePath, "tokyo-vm") {
		t.Errorf("[AC-Sf6a62f-3-4] in-claim 'tokyo-vm' is missing after reconcile; it must be kept.\nstdout:\n%s", stdout)
	}
}

// TestClaimPruneBoundary_InClaimOrphanStillPruned is the companion "fix does not
// over-suppress" test: within the claim, a genuine orphan is STILL pruned.  This
// proves the FIX #6 filter narrows the boundary to the claim rather than
// disabling prune wholesale.
//
// Setup mirrors the survival test, but reality additionally holds an IN-CLAIM
// orphan "tokyo-orphan" {site: tokyo} that is absent from desired.  Under
// --claim site=tokyo it is inside the boundary, so it must be deleted; the
// out-of-claim "osaka-vm" must still survive.
//
// [AC-Sf6a62f-3-4]
func TestClaimPruneBoundary_InClaimOrphanStillPruned(t *testing.T) {
	// [AC-Sf6a62f-3-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	seedStateWithResources(t, statePath, map[string]map[string]any{
		"tokyo-vm":     {"name": "tokyo-vm", "size": "1"},
		"tokyo-orphan": {"name": "tokyo-orphan", "size": "9"},
		"osaka-vm":     {"name": "osaka-vm", "size": "2"},
	})
	seedStatePruneableCapability(t, statePath)
	seedStateScopes(t, statePath, map[string]map[string]string{
		"tokyo-vm":     {"site": "tokyo"},
		"tokyo-orphan": {"site": "tokyo"},
		"osaka-vm":     {"site": "osaka"},
	})

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "tokyo-vm",
				"scope":           map[string]string{"site": "tokyo"},
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "tokyo-vm",
					"size": "1",
				},
			},
		},
	})

	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--claim", "site=tokyo")
	if code != 0 {
		t.Fatalf("[AC-Sf6a62f-3-4] reconcile exited %d; stderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}

	// In-claim orphan is pruned.
	if resourceExistsInState(t, statePath, "tokyo-orphan") {
		t.Errorf("[AC-Sf6a62f-3-4] in-claim orphan 'tokyo-orphan' survived; it must be pruned within the claim.\nstdout:\n%s", stdout)
	}
	// Out-of-claim resource still survives (FIX #6 boundary holds even with a
	// real in-claim prune happening).
	if !resourceExistsInState(t, statePath, "osaka-vm") {
		t.Errorf("[AC-Sf6a62f-3-4] out-of-claim 'osaka-vm' was pruned; the claim boundary must protect it.\nstdout:\n%s", stdout)
	}
	// In-claim keeper survives.
	if !resourceExistsInState(t, statePath, "tokyo-vm") {
		t.Errorf("[AC-Sf6a62f-3-4] in-claim 'tokyo-vm' was removed; it must be kept.\nstdout:\n%s", stdout)
	}
}
