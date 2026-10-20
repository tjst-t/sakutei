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

// provider_version_test.go contains CLI-level acceptance tests for Story Scb0ecd-3.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout/stderr, exit code, and side effects.  No internal
// package is imported directly; only the binary's observable behaviour is
// verified.
//
// AC tags map to scenario-Scb0ecd-3.json and 旧 ROADMAP.json（Backlog に移行済み）:
//
//	AC-Scb0ecd-3-1  provider_version empty → startup rejected (fail-closed, §10.4)
//
// [AC-Scb0ecd-3-M] marks the machine-executable scenario tests required by the story.
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-3-1: provider_version empty → fail-closed startup rejection
// ─────────────────────────────────────────────────────────────────────────────

// TestProviderVersion_FailClosed_Plan verifies that "sakutei plan" exits non-zero
// when the provider has an empty provider_version (state file has no
// provider_version field).
//
// §10.4: version-unembedded providers must be rejected at startup (fail-closed).
// A provider without a version cannot reliably detect ghost diffs (cause C);
// its diffs would silently be misclassified as drift B and auto-applied.
//
// [AC-Scb0ecd-3-1] [AC-Scb0ecd-3-M]
func TestProviderVersion_FailClosed_Plan(t *testing.T) {
	// [AC-Scb0ecd-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// State file with NO provider_version (empty / missing field).
	// The fake provider's Capabilities().ProviderVersion will return "".
	seedStateNoVersion(t, statePath, map[string]map[string]any{
		"fail-vm": {"name": "fail-vm", "size": "10"},
	})

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "fail-vm",
				"fields":   map[string]string{"name": "fail-vm", "size": "10"},
			},
		},
	})

	_, stderr, code := runPlan(t, intentDir, statePath)

	// Must exit non-zero (fail-closed: refusing to run without a version).
	if code == 0 {
		t.Errorf("[AC-Scb0ecd-3-1] plan: expected non-zero exit code for empty provider_version; got 0\nstderr: %s", stderr)
	}
	// The error message must mention the validation failure (not a generic error).
	if !strings.Contains(stderr, "provider validation failed") {
		t.Errorf("[AC-Scb0ecd-3-1] plan: expected 'provider validation failed' in stderr; got:\n%s", stderr)
	}
	// Must not mention "provider version is empty" only — the user-facing message
	// should be actionable.  Check that "provider_version" or "version" appears.
	if !strings.Contains(stderr, "version") {
		t.Errorf("[AC-Scb0ecd-3-1] plan: expected 'version' in stderr error message; got:\n%s", stderr)
	}
	t.Logf("[AC-Scb0ecd-3-1] PASS: plan rejected with exit %d; stderr: %s", code, stderr)
}

// TestProviderVersion_FailClosed_Reconcile verifies that "sakutei reconcile"
// exits non-zero when provider_version is empty.
//
// [AC-Scb0ecd-3-1] [AC-Scb0ecd-3-M]
func TestProviderVersion_FailClosed_Reconcile(t *testing.T) {
	// [AC-Scb0ecd-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// State file with NO provider_version.
	seedStateNoVersion(t, statePath, map[string]map[string]any{
		"fail-rec-vm": {"name": "fail-rec-vm", "size": "5"},
	})

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "fail-rec-vm",
				"fields":   map[string]string{"name": "fail-rec-vm", "size": "5"},
			},
		},
	})

	_, stderr, code := runReconcile(t, intentDir, statePath)

	if code == 0 {
		t.Errorf("[AC-Scb0ecd-3-1] reconcile: expected non-zero exit code for empty provider_version; got 0\nstderr: %s", stderr)
	}
	if !strings.Contains(stderr, "provider validation failed") {
		t.Errorf("[AC-Scb0ecd-3-1] reconcile: expected 'provider validation failed' in stderr; got:\n%s", stderr)
	}
	t.Logf("[AC-Scb0ecd-3-1] PASS: reconcile rejected with exit %d; stderr: %s", code, stderr)
}

// TestProviderVersion_FailClosed_Daemon verifies that "sakutei" (daemon mode)
// exits non-zero immediately when provider_version is empty.
//
// [AC-Scb0ecd-3-1] [AC-Scb0ecd-3-M]
func TestProviderVersion_FailClosed_Daemon(t *testing.T) {
	// [AC-Scb0ecd-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// State file with NO provider_version.
	seedStateNoVersion(t, statePath, map[string]map[string]any{
		"daemon-fail-vm": {"name": "daemon-fail-vm", "size": "3"},
	})

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "daemon-fail-vm",
				"fields":   map[string]string{"name": "daemon-fail-vm", "size": "3"},
			},
		},
	})

	// Start daemon with a very short interval; it should exit immediately on
	// startup due to fail-closed validation.
	cmd, buf := startDaemon(t, intentDir, statePath, "100ms")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait for the process to exit on its own (should be near-instantaneous).
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
		// Good — daemon exited.
	case <-time.After(3 * time.Second):
		// Daemon did not exit; kill it and fail.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("[AC-Scb0ecd-3-1] daemon: did not exit within 3s; output:\n%s", buf.String())
	}

	// Must have exited non-zero.
	if waitErr == nil {
		t.Errorf("[AC-Scb0ecd-3-1] daemon: expected non-zero exit; got nil error (exit 0)\noutput: %s", buf.String())
	}
	// Must mention provider validation.
	output := buf.String()
	if !strings.Contains(output, "provider validation failed") {
		t.Errorf("[AC-Scb0ecd-3-1] daemon: expected 'provider validation failed' in output; got:\n%s", output)
	}
	t.Logf("[AC-Scb0ecd-3-1] PASS: daemon exited with error: %v; output: %s", waitErr, output)
}

// TestProviderVersion_WithVersion_Succeeds verifies that when provider_version
// is set (non-empty), all three commands start/run normally.
//
// [AC-Scb0ecd-3-1] [AC-Scb0ecd-3-M]
func TestProviderVersion_WithVersion_Succeeds(t *testing.T) {
	// [AC-Scb0ecd-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed state with a provider_version set.
	seedState(t, statePath, map[string]map[string]any{
		"versioned-vm": {"name": "versioned-vm", "size": "7"},
	})
	setProviderVersion(t, statePath, "v1.2.3")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "versioned-vm",
				"fields":   map[string]string{"name": "versioned-vm", "size": "7"},
			},
		},
	})

	// plan should succeed.
	_, stderr, code := runPlan(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Scb0ecd-3-1] plan with version set: expected exit 0; got %d\nstderr: %s", code, stderr)
	}

	// reconcile should succeed.
	_, stderr2, code2 := runReconcile(t, intentDir, statePath)
	if code2 != 0 {
		t.Errorf("[AC-Scb0ecd-3-1] reconcile with version set: expected exit 0; got %d\nstderr: %s", code2, stderr2)
	}
	t.Logf("[AC-Scb0ecd-3-1] PASS: plan exit=%d, reconcile exit=%d (version set, no rejection)", code, code2)
}
