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

// mixed_provider_prune_test.go — S62dbba verify AF-1 correctness guard.
//
// AF-1: orphan prune Delete ops must route to the provider that actually owns
// the orphan, not to an arbitrary fallback provider.  The bug: the router
// resolved the target provider from localToProvider, which is built from the
// DESIRED resource list.  A prune target is an ORPHAN (present in reality,
// absent from desired), so its Local ID is not in that map and the router fell
// back to whichever provider happened to be first in map-iteration order.  In a
// mixed fake+dnszone intent this could dispatch a DNS-record deletion to
// fakeprovider.Apply (the wrong provider deleting the wrong thing).
//
// The fix routes each op by op.Target.Provider, which the prune engine fills in
// from the owning Lister's own IDs.
//
// Test discipline: launches the REAL "sakutei" binary as a subprocess with a
// mixed intent, seeds an orphan in BOTH backends, and asserts — by inspecting
// each backend's own file — that each orphan is removed from its OWN backend
// (the fake orphan from the fake state file; the DNS orphan from the zone file),
// with exit code measured directly.  If the delete were mis-routed, the wrong
// backend's file would be touched (or the orphan would survive), and the
// assertions below would fail.
//
// AC tags:
//
//	[AC-S62dbba-2-M]   machine-executable scenario tests (exit code + output).
//	AC-S62dbba-2-5     mixed fake+dnszone: orphan prune routes to the owning
//	                   provider (fake orphan → fake state file; DNS orphan →
//	                   zone file); no cross-provider mis-routing.
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMixedProviderPrune_OrphanRoutesToOwningProvider is the primary AF-1 guard.
//
// Setup:
//   - fake state file seeds two resources: "fake-keep" (stays in desired) and
//     "fake-orphan" (absent from desired → prune candidate).  The fake TYPE is
//     opted in to pruning (Capabilities().Pruneable=true).
//   - zone file seeds two A records: "keep.example.com." (stays in desired) and
//     "orphan.example.com." (absent from desired → prune candidate).
//   - intent declares BOTH desired resources, each with PruneWithinScope, one
//     per provider.
//
// Expected after one reconcile:
//   - fake-orphan is gone from the FAKE STATE FILE (routed to fakeprovider).
//   - orphan.example.com. is gone from the ZONE FILE (routed to dnszone).
//   - fake-keep survives in the state file; keep.example.com. survives in the
//     zone file.
//   - the zone file still parses (fakeprovider never wrote JSON into it) and the
//     state file is still valid JSON (dnszone never wrote a zone line into it).
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-5]
func TestMixedProviderPrune_OrphanRoutesToOwningProvider(t *testing.T) {
	// [AC-S62dbba-2-5]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "fake-state.json")
	zonePath := filepath.Join(tmp, "example.com.zone")

	// Seed the fake backend: one keeper + one orphan; opt the TYPE in to pruning.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"fake-keep":   {"name": "fake-keep", "size": "10"},
		"fake-orphan": {"name": "fake-orphan", "size": "5"},
	})
	seedStatePruneableCapability(t, statePath)

	// Seed the dnszone backend: one keeper + one orphan A record.
	initialZone := `; mixed-provider example zone
$ORIGIN example.com.
@	300 IN SOA ns1 admin 2024010101 3600 900 604800 300
@	300 IN NS ns1.example.com.
keep.example.com. 300 IN A 1.2.3.4
orphan.example.com. 300 IN A 9.9.9.9
`
	writeZoneFile(t, zonePath, initialZone)

	// Intent declares BOTH keepers, each with PruneWithinScope so its provider's
	// prune engine runs.  Neither orphan is declared → both are prune candidates.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "fake-keep",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "fake-keep",
					"size": "10",
				},
			},
			dnszoneResource(
				"keep.example.com./A", "keep.example.com.", "A", "1.2.3.4", "300",
				"PruneWithinScope"),
		},
	})

	// Reconcile once.  --state points the fake provider at its state file;
	// --zone points the dnszone provider at its own zone file (distinct paths).
	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--zone", zonePath)
	if code != 0 {
		t.Fatalf("[AC-S62dbba-2-5] reconcile exited %d; stderr=%s\nstdout=%s", code, stderr, stdout)
	}

	// ── Fake backend assertions (fake orphan must be pruned from the STATE FILE) ──
	if resourceExistsInState(t, statePath, "fake-orphan") {
		t.Errorf("[AC-S62dbba-2-5] fake-orphan still present in the fake state file; "+
			"the fake orphan's Delete was NOT applied by fakeprovider (routing bug?). stdout=%s", stdout)
	}
	if !resourceExistsInState(t, statePath, "fake-keep") {
		t.Errorf("[AC-S62dbba-2-5] fake-keep was removed from the state file; a delete was mis-routed "+
			"onto the fake provider or the wrong resource was pruned. stdout=%s", stdout)
	}

	// ── dnszone backend assertions (DNS orphan must be pruned from the ZONE FILE) ──
	zoneContent := readZoneFile(t, zonePath)
	if strings.Contains(zoneContent, "orphan.example.com.") {
		// This is the exact mis-routing symptom: if the DNS orphan's Delete were
		// dispatched to fakeprovider.Apply (the pre-fix behaviour), fakeprovider
		// would look for "orphan.example.com./A" in its JSON state (not there,
		// no-op), and the record would survive here in the zone file.
		t.Errorf("[AC-S62dbba-2-5] orphan.example.com. still present in the zone file; "+
			"the DNS orphan's Delete was NOT applied by dnszone (likely mis-routed to fakeprovider).\nzone:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, "keep.example.com.") {
		t.Errorf("[AC-S62dbba-2-5] keep.example.com. was removed from the zone file; a delete was "+
			"mis-routed onto dnszone or the wrong record was pruned.\nzone:\n%s", zoneContent)
	}

	// ── Cross-contamination guards: each provider only ever touched its own file ──
	// The fake orphan's Local ID must never appear in the zone file, and the DNS
	// orphan's Local ID must never appear in the state file.  Either would prove
	// a provider wrote into the wrong backend.
	if strings.Contains(zoneContent, "fake-orphan") || strings.Contains(zoneContent, "fake-keep") {
		t.Errorf("[AC-S62dbba-2-5] fake resource IDs leaked into the zone file (wrong provider wrote there):\n%s", zoneContent)
	}
	stateRaw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S62dbba-2-5] read state file: %v", err)
	}
	if strings.Contains(string(stateRaw), "orphan.example.com.") || strings.Contains(string(stateRaw), "keep.example.com.") {
		t.Errorf("[AC-S62dbba-2-5] dnszone record IDs leaked into the fake state file (wrong provider wrote there):\n%s", string(stateRaw))
	}

	// Both DELETE ops must have been reported (one per provider).
	deleteCount := strings.Count(stdout, "DELETE")
	if deleteCount < 2 {
		t.Errorf("[AC-S62dbba-2-5] expected at least 2 DELETE ops (one per provider) in stdout; got %d:\n%s", deleteCount, stdout)
	}

	// SOA/NS in the zone must be untouched (dnszone never manages them).
	if !strings.Contains(zoneContent, "SOA") || !strings.Contains(zoneContent, "NS") {
		t.Errorf("[AC-S62dbba-2-5] SOA/NS disappeared from the zone file:\n%s", zoneContent)
	}

	t.Logf("[AC-S62dbba-2-5] PASS: fake orphan pruned from state file, DNS orphan pruned from zone file, no cross-routing. stdout=%s stderr=%s", stdout, stderr)
}

// TestMixedProviderPrune_IdempotentSecondCycle verifies that after the orphans
// are pruned, a second reconcile of the mixed intent converges with no changes
// — confirming that both providers agree reality now matches desired and that
// no phantom cross-provider drift is reported.
//
// [AC-S62dbba-2-M] [AC-S62dbba-2-5]
func TestMixedProviderPrune_IdempotentSecondCycle(t *testing.T) {
	// [AC-S62dbba-2-5]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "fake-state.json")
	zonePath := filepath.Join(tmp, "example.com.zone")

	seedStateWithResources(t, statePath, map[string]map[string]any{
		"fake-keep":   {"name": "fake-keep", "size": "10"},
		"fake-orphan": {"name": "fake-orphan", "size": "5"},
	})
	seedStatePruneableCapability(t, statePath)

	writeZoneFile(t, zonePath, `; mixed zone
keep.example.com. 300 IN A 1.2.3.4
orphan.example.com. 300 IN A 9.9.9.9
`)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider":        "fake",
				"id":              "fake-keep",
				"deletion_policy": "PruneWithinScope",
				"fields": map[string]string{
					"name": "fake-keep",
					"size": "10",
				},
			},
			dnszoneResource(
				"keep.example.com./A", "keep.example.com.", "A", "1.2.3.4", "300",
				"PruneWithinScope"),
		},
	})

	// First cycle prunes both orphans.
	stdout1, stderr1, code1 := runReconcile(t, intentDir, statePath, "--zone", zonePath)
	if code1 != 0 {
		t.Fatalf("[AC-S62dbba-2-5] 1st reconcile exited %d; stderr=%s stdout=%s", code1, stderr1, stdout1)
	}

	// Second cycle: both keepers already converged, both orphans already gone →
	// no changes.  This also confirms the fake keeper was not accidentally deleted
	// on cycle 1 (else cycle 2 would re-CREATE it and not converge cleanly).
	stdout2, stderr2, code2 := runReconcile(t, intentDir, statePath, "--zone", zonePath)
	if code2 != 0 {
		t.Fatalf("[AC-S62dbba-2-5] 2nd reconcile exited %d; stderr=%s stdout=%s", code2, stderr2, stdout2)
	}
	if !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[AC-S62dbba-2-5] expected 'Converged (no changes)' on 2nd reconcile; got:\n%s", stdout2)
	}

	// Both keepers must still be present.
	if !resourceExistsInState(t, statePath, "fake-keep") {
		t.Errorf("[AC-S62dbba-2-5] fake-keep missing after two reconciles")
	}
	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "keep.example.com.") {
		t.Errorf("[AC-S62dbba-2-5] keep.example.com. missing after two reconciles:\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-2-5] PASS: mixed prune is idempotent; 2nd cycle converged with no changes")
}
