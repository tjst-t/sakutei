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

// claim_selector_test.go contains CLI-level acceptance tests for the --claim
// scope-selector seam (Story Sf6a62f-3, §17.1).
//
// Test discipline: these tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, exit code, and the state-file side effects.
//
// AC tags:
//
//	[AC-Sf6a62f-3-4] --claim <scope-selector> is accepted by cmd/sakutei, parses a
//	                 core.Scope matcher (e.g. {site: tokyo}), and EXCLUDES scopes
//	                 that do not match from convergence (Observe/plan) — the
//	                 out-of-claim resource is never applied.  (Real sparse checkout
//	                 / git distribution is v1b, §17.4.)
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoSiteIntent writes an intent with one "fake" resource in scope
// {site: tokyo} and one in {site: osaka}.
func writeTwoSiteIntent(t *testing.T, intentDir string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "tokyo-vm",
				"scope":    map[string]string{"site": "tokyo"},
				"fields": map[string]string{
					"name": "tokyo-vm",
					"size": "1",
				},
			},
			{
				"provider": "fake",
				"id":       "osaka-vm",
				"scope":    map[string]string{"site": "osaka"},
				"fields": map[string]string{
					"name": "osaka-vm",
					"size": "2",
				},
			},
		},
	})
}

// stateResourceIDs returns the set of resource local IDs present in the fake
// provider's state file.
func stateResourceIDs(t *testing.T, statePath string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var sf struct {
		Resources map[string]json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("unmarshal state file: %v", err)
	}
	ids := make(map[string]bool, len(sf.Resources))
	for id := range sf.Resources {
		ids[id] = true
	}
	return ids
}

// TestClaimSelector_ExcludesOutOfScope verifies that --claim site=tokyo applies
// only the in-claim resource and never touches the out-of-claim one.
//
// [AC-Sf6a62f-3-4]
func TestClaimSelector_ExcludesOutOfScope(t *testing.T) {
	// [AC-Sf6a62f-3-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeTwoSiteIntent(t, intentDir)

	// Reconcile with a claim that matches only the tokyo scope.
	stdout, stderr, code := runReconcile(t, intentDir, statePath, "--claim", "site=tokyo")
	if code != 0 {
		t.Fatalf("[AC-Sf6a62f-3-4] reconcile exited %d; stderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}

	// The in-claim resource must have been created.
	ids := stateResourceIDs(t, statePath)
	if !ids["tokyo-vm"] {
		t.Errorf("[AC-Sf6a62f-3-4] expected in-claim resource 'tokyo-vm' to be applied; state ids=%v", ids)
	}
	// The out-of-claim resource must NOT have been observed or applied.
	// (This is the load-bearing assertion: an excluded scope leaves NO side
	// effect in reality — it was never Observed/planned/Applied, §17.1.)
	if ids["osaka-vm"] {
		t.Errorf("[AC-Sf6a62f-3-4] out-of-claim resource 'osaka-vm' was applied but should have been excluded; state ids=%v", ids)
	}
	// The stdout summary should reflect a single change (only the in-claim vm).
	if !strings.Contains(stdout, "1 change(s) applied") {
		t.Errorf("[AC-Sf6a62f-3-4] expected exactly 1 change applied under --claim; got stdout:\n%s", stdout)
	}
}

// TestClaimSelector_EmptyMatchesWholeWorld verifies that with no --claim flag
// every scope is converged (the default single-shard behaviour).
//
// [AC-Sf6a62f-3-4]
func TestClaimSelector_EmptyMatchesWholeWorld(t *testing.T) {
	// [AC-Sf6a62f-3-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeTwoSiteIntent(t, intentDir)

	// No --claim: both scopes are in the claim (whole world).
	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Sf6a62f-3-4] reconcile exited %d; stderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	ids := stateResourceIDs(t, statePath)
	if !ids["tokyo-vm"] || !ids["osaka-vm"] {
		t.Errorf("[AC-Sf6a62f-3-4] expected BOTH resources applied without --claim; state ids=%v", ids)
	}
}

// TestClaimSelector_InvalidRejected verifies that a malformed selector is
// rejected with a non-zero exit (fail-closed on operator input error).
//
// [AC-Sf6a62f-3-4]
func TestClaimSelector_InvalidRejected(t *testing.T) {
	// [AC-Sf6a62f-3-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeTwoSiteIntent(t, intentDir)

	// "site" has no '=value' → not a key=value term.
	_, stderr, code := runReconcile(t, intentDir, statePath, "--claim", "site")
	if code == 0 {
		t.Fatalf("[AC-Sf6a62f-3-4] expected non-zero exit for invalid --claim; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "invalid --claim") {
		t.Errorf("[AC-Sf6a62f-3-4] expected 'invalid --claim' diagnostic; got:\n%s", stderr)
	}
}
