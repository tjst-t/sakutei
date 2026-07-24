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

// status_test.go — CLI-level acceptance tests for Story Sc7a647-3
// ("sakutei status": 適合レポート / drifted 一覧).
//
// Test discipline: all CLI tests launch the real "sakutei" binary as a
// subprocess and inspect its stdout/stderr, exit code, and side effects on
// reality files.  No internal package is imported directly for the CLI tests;
// only the binary's observable behaviour is verified.
//
// The last-seen "前周は X だった" mechanism is verified at the report-builder
// layer in internal/statusreport/statusreport_test.go (a single CLI invocation
// has no genuine "previous cycle"; the mechanism is honestly tested where two
// observations can be fed).
//
// AC tags map to ROADMAP.json Sc7a647-3:
//
//	AC-Sc7a647-3-1  drifted list + cause tag; converged reported when in sync; fake/dnszone/nftables
//	AC-Sc7a647-3-2  read-only: reality byte-for-byte unchanged; no CREATE/UPDATE/DELETE/applied lines
//	AC-Sc7a647-3-3  provider tier displayed; last-seen note; downstream D reflected
package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStatusCmd runs "sakutei status --intent <dir> --state <path>" plus any
// extra args, returning stdout, stderr, and the exit code.
func runStatusCmd(t *testing.T, intentDir, statePath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)

	args := []string{"status", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)

	cmd := exec.Command(bin, args...)
	outBytes, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return string(outBytes), string(exitErr.Stderr), exitErr.ExitCode()
		}
		t.Fatalf("runStatusCmd: unexpected error: %v", err)
	}
	return string(outBytes), "", 0
}

// assertNoApplyVerbs fails if out contains any Apply-style execution verb.
// status may LIST drift ("DRIFTED"), but must never print an executed op line.
func assertNoApplyVerbs(t *testing.T, tag, out string) {
	t.Helper()
	for _, forbidden := range []string{"CREATE", "UPDATE", "DELETE", "applied", "Applying"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s status output must NOT contain Apply verb %q (read-only); got:\n%s", tag, forbidden, out)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-3-1: drifted list + cause tag; converged when in sync (fake).
// ─────────────────────────────────────────────────────────────────────────────

// TestStatus_DriftedListWithCause verifies status reports a drifted resource
// with a cause tag when reality differs from desired (fake provider).
//
// [AC-Sc7a647-3-1]
func TestStatus_DriftedListWithCause(t *testing.T) {
	// [AC-Sc7a647-3-1]
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
				"fields":   map[string]string{"name": "drift-vm", "size": "20"},
			},
		},
	})
	// Reality: size=10 (drift).
	seedState(t, statePath, map[string]map[string]any{
		"drift-vm": {"name": "drift-vm", "size": "10"},
	})

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-3-1] expected exit 0, got %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "DRIFTED") || !strings.Contains(stdout, "drift-vm") {
		t.Errorf("[AC-Sc7a647-3-1] expected drift-vm listed as DRIFTED; got:\n%s", stdout)
	}
	// cause tag present (A–E). Reality drifted with stable intent/version → B.
	if !strings.Contains(stdout, "原因タグ:") {
		t.Errorf("[AC-Sc7a647-3-1] expected a cause tag line; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "B（ドリフト）") {
		t.Errorf("[AC-Sc7a647-3-1] a size-only drift should be cause B; got:\n%s", stdout)
	}
	t.Logf("[AC-Sc7a647-3-1] status output:\n%s", stdout)
}

// TestStatus_ConformantWhenInSync verifies that when reality matches desired,
// status reports 適合 (drift なし) and lists no drifted resource.
//
// [AC-Sc7a647-3-1]
func TestStatus_ConformantWhenInSync(t *testing.T) {
	// [AC-Sc7a647-3-1]
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
				"id":       "synced-vm",
				"fields":   map[string]string{"name": "synced-vm", "size": "20"},
			},
		},
	})
	seedState(t, statePath, map[string]map[string]any{
		"synced-vm": {"name": "synced-vm", "size": "20"},
	})

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-3-1] expected exit 0, got %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "適合（drift なし）") {
		t.Errorf("[AC-Sc7a647-3-1] expected 適合 report when in sync; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "DRIFTED") {
		t.Errorf("[AC-Sc7a647-3-1] a converged resource must not be listed as DRIFTED; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "converged=1") {
		t.Errorf("[AC-Sc7a647-3-1] expected converged=1 in summary; got:\n%s", stdout)
	}
	t.Logf("[AC-Sc7a647-3-1] status output:\n%s", stdout)
}

// TestStatus_DNSZoneDrift verifies status works with the dnszone provider.
//
// [AC-Sc7a647-3-1]
func TestStatus_DNSZoneDrift(t *testing.T) {
	// [AC-Sc7a647-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zonePath := filepath.Join(tmp, "example.com.zone")

	// Desired: www A 1.2.3.4. The dnszone provider-local id is "<name>/<type>".
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("www/A", "www", "A", "1.2.3.4", "300", ""),
	})
	// Reality: www A 9.9.9.9 (drift). Minimal RFC1035 zone record line.
	writeZoneFile(t, zonePath, "www\t300\tIN\tA\t9.9.9.9\n")

	// dnszone reads config["zone"]; --state is the alias fallback but we pass --zone.
	stdout, stderr, code := runStatusCmd(t, intentDir, zonePath, "--zone", zonePath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-3-1] dnszone status expected exit 0, got %d; stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "DRIFTED") || !strings.Contains(stdout, "www") {
		t.Errorf("[AC-Sc7a647-3-1] dnszone drift should list www as DRIFTED; got:\n%s", stdout)
	}
	// dnszone provider tier must be displayed.
	if !strings.Contains(stdout, "provider 階級:") {
		t.Errorf("[AC-Sc7a647-3-1] dnszone status should show provider tier; got:\n%s", stdout)
	}
	assertNoApplyVerbs(t, "[AC-Sc7a647-3-1]", stdout)
	t.Logf("[AC-Sc7a647-3-1] dnszone status output:\n%s", stdout)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-3-2: read-only proof — reality byte-for-byte unchanged; no Apply lines.
// ─────────────────────────────────────────────────────────────────────────────

// TestStatus_ReadOnly_ByteForByte runs status against a reality that DIFFERS
// from desired and asserts:
//   - the state file is byte-for-byte identical before and after (no Apply),
//   - stdout contains no Apply-style execution verbs (it may LIST drift only).
//
// This is the load-bearing read-only proof.
//
// [AC-Sc7a647-3-2]
func TestStatus_ReadOnly_ByteForByte(t *testing.T) {
	// [AC-Sc7a647-3-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Desired declares two resources: one drift (size), one absent-in-reality
	// (would be a CREATE for reconcile). status must touch neither.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "drift-vm", "fields": map[string]string{"name": "drift-vm", "size": "20"}},
			{"provider": "fake", "id": "absent-vm", "fields": map[string]string{"name": "absent-vm", "size": "5"}},
		},
	})
	// Reality: drift-vm exists with size=10; absent-vm is NOT present.
	seedState(t, statePath, map[string]map[string]any{
		"drift-vm": {"name": "drift-vm", "size": "10"},
	})

	beforeState := readStateFile(t, statePath)

	// generated/ dir is a sibling of --intent named "generated"; status must not
	// create/write it either. Record its (absent) state before.
	generatedDir := filepath.Join(tmp, "generated")
	beforeGenExists := dirExists(generatedDir)

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-3-2] status expected exit 0, got %d; stderr: %s", code, stderr)
	}

	afterState := readStateFile(t, statePath)
	if string(beforeState) != string(afterState) {
		t.Errorf("[AC-Sc7a647-3-2] state file changed after status — Apply must not be called\nbefore: %s\nafter:  %s",
			beforeState, afterState)
	}

	// status must not have created the generated/ directory (read-only).
	if !beforeGenExists && dirExists(generatedDir) {
		t.Errorf("[AC-Sc7a647-3-2] status created generated/ dir — status must not write generated output")
	}

	// stdout may list drift but must not print executed Apply ops.
	assertNoApplyVerbs(t, "[AC-Sc7a647-3-2]", stdout)

	// Sanity: the drift WAS listed (so we know the reality genuinely differed).
	if !strings.Contains(stdout, "DRIFTED") {
		t.Errorf("[AC-Sc7a647-3-2] expected the differing reality to be listed as DRIFTED; got:\n%s", stdout)
	}
	t.Logf("[AC-Sc7a647-3-2] read-only proven: state unchanged (%d bytes), no Apply verbs\n%s", len(beforeState), stdout)
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-3-3: provider tier + last-seen note + downstream D.
// ─────────────────────────────────────────────────────────────────────────────

// TestStatus_TierAndLastSeenNote verifies the status output shows the provider
// Capabilities tier and a last-seen note. In a single CLI invocation the ring
// has no genuine previous cycle, so the note is the "no previous snapshot" form.
// (The "前周は X だった" positive case is tested at the report-builder layer.)
//
// [AC-Sc7a647-3-3]
func TestStatus_TierAndLastSeenNote(t *testing.T) {
	// [AC-Sc7a647-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "drift-vm", "fields": map[string]string{"name": "drift-vm", "size": "20"}},
		},
	})
	seedState(t, statePath, map[string]map[string]any{
		"drift-vm": {"name": "drift-vm", "size": "10"},
	})

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-3-3] status expected exit 0, got %d; stderr: %s", code, stderr)
	}

	// provider tier displayed (Capabilities: TrueObserve/List/BulkObserve/Pruneable/ProviderVersion).
	if !strings.Contains(stdout, "provider 階級:") {
		t.Errorf("[AC-Sc7a647-3-3] expected provider tier line; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "ProviderVersion=") || !strings.Contains(stdout, "TrueObserve=") {
		t.Errorf("[AC-Sc7a647-3-3] tier line should show Capabilities fields; got:\n%s", stdout)
	}
	// last-seen note present. Single invocation → "no previous snapshot".
	if !strings.Contains(stdout, "前周:") {
		t.Errorf("[AC-Sc7a647-3-3] expected a 前周 (last-seen) note; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "no previous snapshot") {
		t.Errorf("[AC-Sc7a647-3-3] single invocation should show 'no previous snapshot' (S3 non-persistent); got:\n%s", stdout)
	}
	t.Logf("[AC-Sc7a647-3-3] status output:\n%s", stdout)
}

// TestStatus_DownstreamDPropagation verifies that a composite parent's children
// (upstream-changed dependents) that drift are flagged cause D in the report.
// A composite fake intent expands (render phase 2) into concrete children
// <parent>-child-a / <parent>-child-b; because they originate from a parent
// expansion, status marks them UpstreamChanged=true, so a drift is classified
// D (cascade) — which takes priority over A (§10.2 priority C>D>A>E>B).
//
// [AC-Sc7a647-3-3]
func TestStatus_DownstreamDPropagation(t *testing.T) {
	// [AC-Sc7a647-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Composite parent "app" → children app-child-a / app-child-b (fake expander).
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "app",
				"fields":   map[string]string{"name": "app", "kind": "composite"},
			},
		},
	})
	// Reality: children are ABSENT (empty reality). Each child is a drift whose
	// cause is D (upstream/parent expansion), not A.
	initStateFile(t, statePath)

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-3-3] composite status expected exit 0, got %d; stderr: %s", code, stderr)
	}

	// The children must appear as DRIFTED with cause D (downstream propagation).
	if !strings.Contains(stdout, "app-child-a") || !strings.Contains(stdout, "app-child-b") {
		t.Errorf("[AC-Sc7a647-3-3] expected composite children listed as drifted; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "D（波及") {
		t.Errorf("[AC-Sc7a647-3-3] drifted composite children should carry cause D (downstream); got:\n%s", stdout)
	}
	assertNoApplyVerbs(t, "[AC-Sc7a647-3-3]", stdout)
	t.Logf("[AC-Sc7a647-3-3] downstream cause D surfaced in CLI:\n%s", stdout)
}
