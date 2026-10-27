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

// cause_daemon_test.go contains CLI-level acceptance tests that exercise
// cause-tag classification (A/B/C) and the ghost-diff gate in the DAEMON path
// (the long-running "sakutei" loop), as opposed to the one-shot plan/reconcile
// paths covered by cause_test.go.
//
// F-5 (Scb0ecd-2 daemon wiring): before this Story the daemon passed a
// zero-value CauseContext to Diff, so EVERY daemon diff was misclassified as B
// (intent_prev == intent_cur == "" → not A; provider versions both "" → not C →
// residual B).  That meant the ghost-diff gate (cause C → do NOT apply) did NOT
// exist in the daemon: a provider upgrade that produced a look-alike diff would
// be silently auto-applied every cycle (ADR-0008's dangerous outcome).  These
// tests verify the daemon now:
//
//	(a) tags a fresh intent commit as A and applies it (no DriftEvent),
//	(b) tags a real reality drift as B and applies it (DriftEvent [cause: B]),
//	(c) tags a provider_version bump as C, logs ProviderUpgradeDiff, and does
//	    NOT apply it (the ghost-diff gate now fires in the daemon).
//
// Test discipline: every test launches the real "sakutei" binary as a
// subprocess, drives it via files, inspects stdout/stderr and the state file,
// measures exit codes directly, and guarantees the process is killed.
//
// AC tags:
//
//	AC-Scb0ecd-2-1  cause: A when intent_revision changes (daemon)
//	AC-Scb0ecd-2-2  cause: B when reality drifts, intent/version stable (daemon)
//	AC-Scb0ecd-2-3  cause: C when provider_version changes; ghost diff NOT applied (daemon)
package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// seedStateAtomic writes the fake-provider state file (resources + a specific
// provider_version) in a SINGLE writeFileAtomic call.
//
// This matters for the daemon tests: injecting a drift and a provider_version
// bump must appear to the running daemon as one atomic change.  If the size and
// the version were written by two separate calls (seedState then
// setProviderVersion), a daemon cycle could observe the state BETWEEN the two
// writes — seeing the drifted size but the OLD version — and classify the diff
// as B (drift) and apply it, defeating the very ghost-diff (C) case under test.
func seedStateAtomic(t *testing.T, statePath string, resources map[string]map[string]any, providerVersion string) {
	t.Helper()
	m := make(map[string]any, len(resources))
	for id, fields := range resources {
		m[id] = map[string]any{"fields": fields}
	}
	sf := map[string]any{
		"resources":        m,
		"provider_version": providerVersion,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedStateAtomic: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("seedStateAtomic: write: %v", err)
	}
}

// stateResourceField reads statePath and returns the string value of the given
// field for the given resource local ID.  Returns ("", false) when the file is
// missing/partial or the field is absent (treated as "not yet observable").
func stateResourceField(statePath, local, field string) (string, bool) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return "", false
	}
	var sf struct {
		Resources map[string]struct {
			Fields map[string]any `json:"fields"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		return "", false
	}
	res, ok := sf.Resources[local]
	if !ok {
		return "", false
	}
	v, ok := res.Fields[field]
	if !ok {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	default:
		return "", true
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-1 (daemon): intent change → cause A, applied, no DriftEvent
// ─────────────────────────────────────────────────────────────────────────────

// TestDaemonCause_A_IntentChange verifies that when the intent file is edited
// while the daemon is running, the next cycle classifies the resulting diff as
// cause A (intentional) — NOT B (drift) — and applies it.  The A-vs-B
// distinction is observable in the daemon because cause B additionally emits a
// "DriftEvent ... [cause: B]" line, whereas cause A does not.
//
// [AC-Scb0ecd-2-1] [AC-Scb0ecd-2-M]
func TestDaemonCause_A_IntentChange(t *testing.T) {
	// [AC-Scb0ecd-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: size=10.  Reality already matches (converged) with a version so the
	// fail-closed gate passes.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "da-vm", "fields": map[string]string{"name": "da-vm", "size": "10"}},
		},
	})
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"da-vm": {"name": "da-vm", "size": "10"},
	}, "v1.0.0")

	cmd, stdoutBuf, stderrBuf := startDaemonCombined(t, intentDir, statePath, "150ms")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Wait for at least one converged cycle so prev intent_revision is recorded.
	if !waitForStringInBuf(stdoutBuf, "converged (no changes)", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-1] daemon did not converge within 5s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// Record how much stderr existed before the intent change so we can assert
	// that NO NEW DriftEvent is produced by the intent-driven (cause A) change.
	stderrBefore := stderrBuf.String()

	// Edit intent: size 10 → 20.  intent_revision changes → cause A.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "da-vm", "fields": map[string]string{"name": "da-vm", "size": "20"}},
		},
	})

	// Wait for the daemon to apply the new desired value (size → 20).
	applied := waitForCondition(5*time.Second, func() bool {
		v, ok := stateResourceField(statePath, "da-vm", "size")
		return ok && v == "20"
	})
	if !applied {
		t.Fatalf("[AC-Scb0ecd-2-1] intent change (A) was not applied within 5s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// The intent-driven change must NOT have emitted a NEW DriftEvent: a
	// DriftEvent is only logged for cause B.  Any DriftEvent for "da-vm" that
	// appears AFTER the intent change would mean the change was misclassified as
	// drift (the exact bug F-5 fixes).
	stderrAfter := stderrBuf.String()
	newStderr := strings.TrimPrefix(stderrAfter, stderrBefore)
	if strings.Contains(newStderr, "DriftEvent") && strings.Contains(newStderr, "da-vm") {
		t.Errorf("[AC-Scb0ecd-2-1] intent change should be cause A (no DriftEvent), but a DriftEvent appeared:\n%s", newStderr)
	}
	t.Logf("[AC-Scb0ecd-2-1] PASS: intent change applied as cause A (no DriftEvent)")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-2 (daemon): reality drift → cause B, applied, DriftEvent [cause: B]
// ─────────────────────────────────────────────────────────────────────────────

// TestDaemonCause_B_RealityDrift verifies that when reality drifts (state file
// mutated externally) while intent and provider_version are unchanged, the
// daemon classifies the diff as cause B, emits "DriftEvent ... [cause: B]", and
// corrects it.  This is the genuine-drift counterpart to the cause-A test: here
// a DriftEvent MUST appear (whereas for A it must not).
//
// [AC-Scb0ecd-2-2] [AC-Scb0ecd-2-M]
func TestDaemonCause_B_RealityDrift(t *testing.T) {
	// [AC-Scb0ecd-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "db-vm", "fields": map[string]string{"name": "db-vm", "size": "10"}},
		},
	})
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"db-vm": {"name": "db-vm", "size": "10"},
	}, "v1.0.0")

	cmd, stdoutBuf, stderrBuf := startDaemonCombined(t, intentDir, statePath, "150ms")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	if !waitForStringInBuf(stdoutBuf, "converged (no changes)", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-2] daemon did not converge within 5s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// Inject a REAL reality drift: size 10 → 30, intent and version unchanged.
	// Written atomically with the SAME provider_version so classification cannot
	// see a version change (which would be C) — this is unambiguously B.
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"db-vm": {"name": "db-vm", "size": "30"},
	}, "v1.0.0")

	// A DriftEvent tagged cause B must appear (drift detection), and the daemon
	// must correct the value back to the desired 10.
	if !waitForStringInBuf(stderrBuf, "[cause: B]", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-2] expected DriftEvent [cause: B] within 5s after drift injection\nstderr:\n%s",
			stderrBuf.String())
	}
	corrected := waitForCondition(5*time.Second, func() bool {
		v, ok := stateResourceField(statePath, "db-vm", "size")
		return ok && v == "10"
	})
	if !corrected {
		t.Fatalf("[AC-Scb0ecd-2-2] drift (B) was not corrected back to size=10 within 5s\nstderr:\n%s",
			stderrBuf.String())
	}

	// The name field must be preserved through the drift correction.
	if v, _ := stateResourceField(statePath, "db-vm", "name"); v != "db-vm" {
		t.Errorf("[AC-Scb0ecd-2-2] name field lost during B correction; got %q", v)
	}
	t.Logf("[AC-Scb0ecd-2-2] PASS: reality drift classified B, DriftEvent emitted, corrected")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Scb0ecd-2-3 (daemon): provider_version change → cause C, ghost diff NOT applied
// ─────────────────────────────────────────────────────────────────────────────

// TestDaemonCause_C_GhostDiffNotApplied is the most important F-5 test: it
// proves the ghost-diff gate now exists in the daemon.  When provider_version
// changes (a provider upgrade) and produces a diff, the daemon must classify it
// as cause C, log ProviderUpgradeDiff, and NOT apply the change — the ghost diff
// is held for operator review.  Before F-5 the daemon would have tagged this B
// and auto-corrected the value (ADR-0008's dangerous outcome).
//
// The fake provider is deliberately "dumb" (provider_version does not alter its
// diff math), so to create a diff we inject a field drift (size 10 → 11) at the
// SAME time as the version bump.  Classification prioritises C over B, so the
// combined change is tagged C and the size is NOT corrected to 10.
//
// [AC-Scb0ecd-2-3] [AC-Scb0ecd-2-M]
func TestDaemonCause_C_GhostDiffNotApplied(t *testing.T) {
	// [AC-Scb0ecd-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "dc-vm", "fields": map[string]string{"name": "dc-vm", "size": "10"}},
		},
	})
	// Converged reality, provider_version v1.0.0.
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"dc-vm": {"name": "dc-vm", "size": "10"},
	}, "v1.0.0")

	cmd, stdoutBuf, stderrBuf := startDaemonCombined(t, intentDir, statePath, "150ms")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Converge so the daemon records prev provider_version = v1.0.0.
	if !waitForStringInBuf(stdoutBuf, "converged (no changes)", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-3] daemon did not converge within 5s\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}

	// ATOMICALLY inject a provider upgrade (version v1.0.0 → v2.0.0) together with
	// a field drift (size 10 → 11) so the daemon never observes the drift under
	// the OLD version (which would be B).  prev=v1.0.0, cur=v2.0.0, diff exists → C.
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"dc-vm": {"name": "dc-vm", "size": "11"},
	}, "v2.0.0")

	// The daemon must log a ProviderUpgradeDiff (cause C) for dc-vm.
	if !waitForStringInBuf(stderrBuf, "ProviderUpgradeDiff", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-3] expected ProviderUpgradeDiff (cause C) within 5s after version bump\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "[cause: C]") {
		t.Errorf("[AC-Scb0ecd-2-3] ProviderUpgradeDiff must mention [cause: C]; stderr:\n%s", stderrBuf.String())
	}

	// The ghost diff must NOT be applied: give the daemon several more cycles and
	// confirm the size STAYS 11 (it is NOT corrected to the desired 10).  A B
	// misclassification (the pre-F-5 bug) would have corrected it to 10.
	stayedGhost := true
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := stateResourceField(statePath, "dc-vm", "size"); ok && v == "10" {
			stayedGhost = false
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stayedGhost {
		t.Fatalf("[AC-Scb0ecd-2-3] ghost diff (cause C) was APPLIED in the daemon (size corrected to 10); "+
			"the ghost-diff gate did not fire\nstderr:\n%s", stderrBuf.String())
	}
	if v, _ := stateResourceField(statePath, "dc-vm", "size"); v != "11" {
		t.Errorf("[AC-Scb0ecd-2-3] expected size to remain 11 (ghost diff held); got %q", v)
	}
	t.Logf("[AC-Scb0ecd-2-3] PASS: provider upgrade classified C; ghost diff held (size stayed 11), NOT applied")
}

// TestDaemonCause_C_GracefulExitCode is a focused exit-code check: a daemon that
// is holding a ghost diff (cause C) is still healthy and must exit 0 on SIGTERM
// (the ghost-diff gate is a hold, not a fatal error).  Exit code is measured
// directly and the process is guaranteed killed.
//
// [AC-Scb0ecd-2-3] [AC-Scb0ecd-2-M]
func TestDaemonCause_C_GracefulExitCode(t *testing.T) {
	// [AC-Scb0ecd-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "dc2-vm", "fields": map[string]string{"name": "dc2-vm", "size": "10"}},
		},
	})
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"dc2-vm": {"name": "dc2-vm", "size": "10"},
	}, "v1.0.0")

	cmd, stdoutBuf, stderrBuf := startDaemonCombined(t, intentDir, statePath, "150ms")
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	if !waitForStringInBuf(stdoutBuf, "converged (no changes)", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-3] daemon did not converge within 5s\nstdout:\n%s", stdoutBuf.String())
	}

	// Trigger a ghost diff.
	seedStateAtomic(t, statePath, map[string]map[string]any{
		"dc2-vm": {"name": "dc2-vm", "size": "11"},
	}, "v2.0.0")
	if !waitForStringInBuf(stderrBuf, "ProviderUpgradeDiff", 5*time.Second) {
		t.Fatalf("[AC-Scb0ecd-2-3] expected ProviderUpgradeDiff within 5s\nstderr:\n%s", stderrBuf.String())
	}

	// SIGTERM: the daemon must exit 0 (graceful) despite holding a ghost diff.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("[AC-Scb0ecd-2-3] send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		killed = true
		// Measure the exit code directly.  Our daemon catches SIGTERM and calls
		// os.Exit(0); Wait returns nil in that case.  A non-nil *exec.ExitError
		// with a non-zero code is a genuine failure.
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatalf("[AC-Scb0ecd-2-3] unexpected Wait error: %v", err)
			}
		}
		if code != 0 {
			t.Errorf("[AC-Scb0ecd-2-3] expected exit 0 after SIGTERM while holding ghost diff; got %d\nstdout:\n%s\nstderr:\n%s",
				code, stdoutBuf.String(), stderrBuf.String())
		}
		t.Logf("[AC-Scb0ecd-2-3] PASS: daemon holding ghost diff exited %d on SIGTERM", code)
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		killed = true
		t.Fatalf("[AC-Scb0ecd-2-3] daemon did not exit within 5s after SIGTERM\nstdout:\n%s\nstderr:\n%s",
			stdoutBuf.String(), stderrBuf.String())
	}
}
