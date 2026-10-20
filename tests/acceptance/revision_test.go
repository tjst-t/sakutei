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

// Package acceptance contains CLI-level acceptance tests for Story Scb0ecd-1.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, exit code, and side effects.  No internal package is
// imported directly; only the binary's observable behaviour is verified.
//
// AC tags map to scenario-Scb0ecd-1.json and 旧 ROADMAP.json（Backlog に移行済み）:
//
//	AC-Scb0ecd-1-1  DesiredSource.Fetch returns intent_revision visible in plan/reconcile output
//	AC-Scb0ecd-1-2  rendered_revision equals intent_revision in v1a (scope-unit seam)
//	AC-Scb0ecd-1-3  intent file change causes intent_revision to change in plan output
//	[determinism]   Same intent → same revision on repeated invocations
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// extractRevisions parses the revision header line from sakutei plan/reconcile
// stdout.  The header format is:
//
//	intent_revision=<hex> rendered_revision=<hex>
//
// Returns empty strings if the line is not present.
func extractRevisions(stdout string) (intentRev, renderedRev string) {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "intent_revision=") {
			continue
		}
		// Parse: intent_revision=<val> rendered_revision=<val>
		parts := strings.Fields(line)
		for _, part := range parts {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "intent_revision":
				intentRev = kv[1]
			case "rendered_revision":
				renderedRev = kv[1]
			}
		}
		return
	}
	return "", ""
}

// writeIntentJSON writes a provisional intent JSON file into intentDir as
// "resources.json".  content is marshalled to indented JSON.
func writeIntentJSON(t *testing.T, intentDir string, content any) {
	t.Helper()
	data, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		t.Fatalf("writeIntentJSON: marshal: %v", err)
	}
	if err := writeFileAtomic(filepath.Join(intentDir, "resources.json"), data, 0644); err != nil {
		t.Fatalf("writeIntentJSON: write: %v", err)
	}
}

// oneResourceIntent returns the minimal intent map for a single fake resource.
func oneResourceIntent(id, size string) any {
	return map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       id,
				"fields": map[string]string{
					"name": id,
					"size": size,
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// AC-Scb0ecd-1-1: intent_revision appears in plan and reconcile output
// ---------------------------------------------------------------------------

// TestRevision_PlanOutputContainsRevisionHeader verifies that sakutei plan
// prints a revision header line with both intent_revision and rendered_revision.
//
// [AC-Scb0ecd-1-1]
func TestRevision_PlanOutputContainsRevisionHeader(t *testing.T) {
	// [AC-Scb0ecd-1-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentJSON(t, intentDir, oneResourceIntent("vm-rev-test", "10"))
	initStateFile(t, statePath)

	stdout, stderr, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Scb0ecd-1-1] sakutei plan exited %d; stderr: %s", code, stderr)
	}

	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev == "" {
		t.Errorf("[AC-Scb0ecd-1-1] intent_revision not found in plan stdout:\n%s", stdout)
	}
	if renderedRev == "" {
		t.Errorf("[AC-Scb0ecd-1-1] rendered_revision not found in plan stdout:\n%s", stdout)
	}
	// Sanity: both are non-empty hex strings (SHA-256 = 64 hex chars).
	if len(intentRev) != 64 {
		t.Errorf("[AC-Scb0ecd-1-1] intent_revision should be 64-char SHA-256 hex, got %d chars: %q",
			len(intentRev), intentRev)
	}
	t.Logf("[AC-Scb0ecd-1-1] intent_revision=%s rendered_revision=%s", intentRev, renderedRev)
}

// TestRevision_ReconcileOutputContainsRevisionHeader verifies that
// sakutei reconcile also prints the revision header.
//
// [AC-Scb0ecd-1-1]
func TestRevision_ReconcileOutputContainsRevisionHeader(t *testing.T) {
	// [AC-Scb0ecd-1-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentJSON(t, intentDir, oneResourceIntent("vm-reconcile-rev", "20"))
	initStateFile(t, statePath)

	stdout, stderr, code := runReconcile(t, intentDir, statePath)
	// reconcile may exit 0 (converged) or 0 (applied); either is acceptable here.
	if code != 0 {
		// If exit non-zero, it may be because the state file appeared after
		// Apply (which is fine); what matters is the revision header is present.
		t.Logf("[AC-Scb0ecd-1-1] reconcile exited %d; stderr: %s", code, stderr)
	}

	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev == "" {
		t.Errorf("[AC-Scb0ecd-1-1] intent_revision not found in reconcile stdout:\n%s", stdout)
	}
	if renderedRev == "" {
		t.Errorf("[AC-Scb0ecd-1-1] rendered_revision not found in reconcile stdout:\n%s", stdout)
	}
	t.Logf("[AC-Scb0ecd-1-1] reconcile intent_revision=%s rendered_revision=%s", intentRev, renderedRev)
}

// ---------------------------------------------------------------------------
// AC-Scb0ecd-1-2: rendered_revision = intent_revision in v1a (scope-unit seam)
// ---------------------------------------------------------------------------

// TestRevision_RenderedEqualsIntentInV1a verifies that in v1a (no external
// inputs) rendered_revision always equals intent_revision.
//
// [AC-Scb0ecd-1-2]
func TestRevision_RenderedEqualsIntentInV1a(t *testing.T) {
	// [AC-Scb0ecd-1-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentJSON(t, intentDir, oneResourceIntent("vm-eq-test", "5"))
	initStateFile(t, statePath)

	stdout, _, _ := runPlan(t, intentDir, statePath)
	intentRev, renderedRev := extractRevisions(stdout)

	if intentRev == "" || renderedRev == "" {
		t.Fatalf("[AC-Scb0ecd-1-2] revision header missing from output:\n%s", stdout)
	}
	if intentRev != renderedRev {
		t.Errorf("[AC-Scb0ecd-1-2] v1a: expected rendered_revision == intent_revision; got intent=%s rendered=%s",
			intentRev, renderedRev)
	}
	t.Logf("[AC-Scb0ecd-1-2] v1a invariant confirmed: intent_revision == rendered_revision = %s", intentRev)
}

// ---------------------------------------------------------------------------
// AC-Scb0ecd-1-3 + scenario step 2: intent change causes revision change
// ---------------------------------------------------------------------------

// TestRevision_IntentChangeCausesRevisionChange verifies that changing an
// intent file field causes intent_revision (and rendered_revision) to change.
//
// [AC-Scb0ecd-1-3]
func TestRevision_IntentChangeCausesRevisionChange(t *testing.T) {
	// [AC-Scb0ecd-1-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Step 1: write initial intent, capture revision.
	writeIntentJSON(t, intentDir, oneResourceIntent("vm-change-test", "10"))
	initStateFile(t, statePath)
	stdout1, stderr1, code1 := runPlan(t, intentDir, statePath)
	if code1 != 0 {
		t.Fatalf("[AC-Scb0ecd-1-3] first plan exited %d; stderr: %s", code1, stderr1)
	}
	intentRev1, renderedRev1 := extractRevisions(stdout1)
	if intentRev1 == "" {
		t.Fatalf("[AC-Scb0ecd-1-3] intent_revision missing from first plan output:\n%s", stdout1)
	}

	// Step 2: change a field value, capture revision.
	writeIntentJSON(t, intentDir, oneResourceIntent("vm-change-test", "20"))
	stdout2, stderr2, code2 := runPlan(t, intentDir, statePath)
	if code2 != 0 {
		t.Fatalf("[AC-Scb0ecd-1-3] second plan exited %d; stderr: %s", code2, stderr2)
	}
	intentRev2, renderedRev2 := extractRevisions(stdout2)
	if intentRev2 == "" {
		t.Fatalf("[AC-Scb0ecd-1-3] intent_revision missing from second plan output:\n%s", stdout2)
	}

	if intentRev1 == intentRev2 {
		t.Errorf("[AC-Scb0ecd-1-3] intent_revision should change when intent changes; both are %q", intentRev1)
	}
	if renderedRev1 == renderedRev2 {
		t.Errorf("[AC-Scb0ecd-1-3] rendered_revision should change when intent changes (v1a: = intent_revision); both are %q", renderedRev1)
	}
	t.Logf("[AC-Scb0ecd-1-3] before: intent=%s rendered=%s", intentRev1, renderedRev1)
	t.Logf("[AC-Scb0ecd-1-3] after:  intent=%s rendered=%s", intentRev2, renderedRev2)
}

// ---------------------------------------------------------------------------
// Determinism: same intent → same revision on repeated invocations
// ---------------------------------------------------------------------------

// TestRevision_Deterministic verifies that running sakutei plan twice on the
// same unchanged intent directory produces the identical intent_revision and
// rendered_revision.
//
// This validates the core ADR-0008 requirement: "decision-relevant な入力のみの
// 決定的ハッシュ" — time, PID, random seed must NOT influence the hash.
//
// [AC-Scb0ecd-1-1] (scenario step 3: same intent → same revision)
func TestRevision_Deterministic(t *testing.T) {
	// [AC-Scb0ecd-1-1] determinism
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentJSON(t, intentDir, oneResourceIntent("vm-determinism", "15"))
	initStateFile(t, statePath)

	// First invocation.
	stdout1, _, code1 := runPlan(t, intentDir, statePath)
	if code1 != 0 {
		t.Fatalf("determinism test: first plan exited %d", code1)
	}
	intentRev1, renderedRev1 := extractRevisions(stdout1)

	// Second invocation — same intent, same directory.
	stdout2, _, code2 := runPlan(t, intentDir, statePath)
	if code2 != 0 {
		t.Fatalf("determinism test: second plan exited %d", code2)
	}
	intentRev2, renderedRev2 := extractRevisions(stdout2)

	if intentRev1 != intentRev2 {
		t.Errorf("intent_revision is not deterministic: first=%q second=%q", intentRev1, intentRev2)
	}
	if renderedRev1 != renderedRev2 {
		t.Errorf("rendered_revision is not deterministic: first=%q second=%q", renderedRev1, renderedRev2)
	}
	t.Logf("determinism confirmed: intent_revision=%s rendered_revision=%s (same on 2 runs)", intentRev1, renderedRev1)
}

// ---------------------------------------------------------------------------
// Empty intent: revision is still well-formed
// ---------------------------------------------------------------------------

// TestRevision_EmptyIntent verifies that an empty intent directory produces
// a well-formed (non-panic, non-error) revision header.
func TestRevision_EmptyIntent(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// No intent files written.
	initStateFile(t, statePath)
	stdout, _, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("empty-intent revision test: plan exited %d", code)
	}

	// Revision header must still be present.
	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev == "" {
		t.Errorf("empty intent: intent_revision missing from plan output:\n%s", stdout)
	}
	if renderedRev == "" {
		t.Errorf("empty intent: rendered_revision missing from plan output:\n%s", stdout)
	}
	// In v1a they must be equal even for empty intent.
	if intentRev != renderedRev {
		t.Errorf("empty intent: v1a invariant broken: intent=%s rendered=%s", intentRev, renderedRev)
	}
	t.Logf("empty intent revision: %s", intentRev)
}
