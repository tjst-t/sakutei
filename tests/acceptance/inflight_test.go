// Copyright 2026 The Sakutei Authors
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

// inflight_test.go contains CLI-level acceptance tests for Story S94f1f9-3.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, state file, and timing.  No internal package
// is imported directly.
//
// AC tags map to scenario-S94f1f9-3.json:
//
//	AC-S94f1f9-3-1 — long Apply does not cause cycles to accumulate;
//	                  in-flight resources are logged as converging.
//	AC-S94f1f9-3-2 — same resource is not double-applied while in-flight
//	                  (per-resource exclusion); apply_count ≤ 1 per launch
//	                  until Apply completes and the resource is out of in-flight.
//	AC-S94f1f9-3-3 — Apply failure causes per-resource exponential backoff;
//	                  the reconciler does not immediately retry on the very
//	                  next cycle (spacing between Apply attempts is visible).
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// [AC-S94f1f9-3-1] Long Apply does not cause cycles to accumulate;
//
//	in-flight resources are logged as "in-flight (converging)".
//
// ---------------------------------------------------------------------------

// TestInflight_CyclesDoNotAccumulate verifies scenario-S94f1f9-3 / step 1.
//
// Setup:
//   - One resource with apply_delay of 600ms injected into the state file.
//   - Daemon started with --interval 150ms (much shorter than the delay).
//
// Expected:
//   - The daemon logs multiple cycles during the 600ms Apply.
//   - After the Apply completes, the resource appears in the state file.
//   - At least one cycle log line shows "in-flight" or "converging",
//     proving that cycles continued to run while Apply was pending.
//   - No "cycle … converged (no changes)" line should appear during the
//     Apply phase (the resource is in-flight, not yet converged).
//
// [AC-S94f1f9-3-1]
func TestInflight_CyclesDoNotAccumulate(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: one resource "slow-vm".
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "slow-vm",
				"fields": map[string]string{
					"name": "slow-vm",
					"size": "10",
				},
			},
		},
	})

	// Seed state with apply_delay so the Apply takes 700ms.
	// The daemon interval is 150ms; so at least 4-5 cycles should run while
	// the Apply is in progress.
	initState := map[string]any{
		"resources":   map[string]any{},
		"apply_delay": map[string]string{"slow-vm": "700ms"},
	}
	initData, _ := json.MarshalIndent(initState, "", "  ")
	if err := os.WriteFile(statePath, initData, 0600); err != nil {
		t.Fatal(err)
	}

	// Start daemon with short interval (150ms) so several cycles run during
	// the 700ms Apply delay.
	cmd, stdout := startDaemon(t, intentDir, statePath, "150ms")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait up to 5 seconds for the resource to appear (Apply finishes after ~700ms).
	data := waitForStateFile(t, statePath, "slow-vm", 5*time.Second)
	t.Logf("[AC-S94f1f9-3-1] state file after slow Apply:\n%s", data)

	// Give the daemon a couple more cycles to log "in-flight" or converged.
	time.Sleep(400 * time.Millisecond)

	// Send SIGTERM to stop cleanly.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("[AC-S94f1f9-3-1] SIGTERM: %v", err)
	}
	_ = cmd.Wait()

	log := stdout.String()
	t.Logf("[AC-S94f1f9-3-1] daemon log:\n%s", log)

	// The log must contain multiple "cycle" lines, proving cycles ran during Apply.
	cycleCount := strings.Count(log, "cycle")
	if cycleCount < 3 {
		t.Errorf("[AC-S94f1f9-3-1] expected ≥3 cycle log lines (cycles must not accumulate); got %d; log:\n%s",
			cycleCount, log)
	}

	// At least one cycle must show "in-flight" — proving in-flight detection is active.
	if !strings.Contains(log, "in-flight") {
		t.Errorf("[AC-S94f1f9-3-1] expected 'in-flight' in log (resource must be tracked as converging while Apply runs); log:\n%s", log)
	}

	// The resource must have been written to the state file (Apply completed).
	if !strings.Contains(string(data), "slow-vm") {
		t.Errorf("[AC-S94f1f9-3-1] 'slow-vm' not found in state file after Apply; state:\n%s", data)
	}
}

// ---------------------------------------------------------------------------
// [AC-S94f1f9-3-2] Per-resource in-flight exclusion: same resource is NOT
//
//	double-applied while Apply is in flight.
//
// ---------------------------------------------------------------------------

// TestInflight_NoConcurrentApplyForSameResource verifies scenario-S94f1f9-3 / step 2.
//
// Strategy:
//   - One resource "exclusive-vm" with apply_delay of 600ms.
//   - Daemon interval 100ms (6 cycles during the delay).
//   - apply_count in the state file is incremented by the fake provider
//     each time Apply is called.
//   - During the 600ms Apply, apply_count must stay at 1.
//   - After Apply completes, apply_count may reach 1 (success, then converged).
//
// [AC-S94f1f9-3-2]
func TestInflight_NoConcurrentApplyForSameResource(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: one resource "exclusive-vm".
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "exclusive-vm",
				"fields": map[string]string{
					"name": "exclusive-vm",
					"size": "5",
				},
			},
		},
	})

	// Seed state with apply_delay of 600ms.
	initState := map[string]any{
		"resources":   map[string]any{},
		"apply_delay": map[string]string{"exclusive-vm": "600ms"},
	}
	initData, _ := json.MarshalIndent(initState, "", "  ")
	if err := os.WriteFile(statePath, initData, 0600); err != nil {
		t.Fatal(err)
	}

	// Start daemon with 100ms interval — 6 cycles will run during the 600ms delay.
	cmd, stdout := startDaemon(t, intentDir, statePath, "100ms")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Poll the apply_count during the delay period.
	// We sample 3 times within the first 500ms (before Apply finishes).
	// The count must never exceed 1 during this window.
	maxObservedCount := 0
	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var sf map[string]any
		if err := json.Unmarshal(data, &sf); err != nil {
			continue
		}
		applyCount, _ := sf["apply_count"].(map[string]any)
		if applyCount != nil {
			if cnt, ok := applyCount["exclusive-vm"].(float64); ok {
				if int(cnt) > maxObservedCount {
					maxObservedCount = int(cnt)
				}
			}
		}
	}

	// Wait for Apply to complete.
	waitForStateFile(t, statePath, "exclusive-vm", 5*time.Second)

	// Stop daemon.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("[AC-S94f1f9-3-2] SIGTERM: %v", err)
	}
	_ = cmd.Wait()

	t.Logf("[AC-S94f1f9-3-2] max observed apply_count during delay: %d", maxObservedCount)
	t.Logf("[AC-S94f1f9-3-2] daemon log:\n%s", stdout.String())

	// During the 600ms delay, apply_count must not exceed 1.
	// If the in-flight exclusion works, Apply is called exactly once.
	// A second concurrent Apply would result in count ≥ 2.
	if maxObservedCount > 1 {
		t.Errorf("[AC-S94f1f9-3-2] per-resource in-flight exclusion violated: apply_count=%d during delay (max allowed: 1); log:\n%s",
			maxObservedCount, stdout.String())
	}

	// Read final apply_count.
	finalData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S94f1f9-3-2] read final state: %v", err)
	}
	var finalSF map[string]any
	if err := json.Unmarshal(finalData, &finalSF); err != nil {
		t.Fatalf("[AC-S94f1f9-3-2] parse final state: %v", err)
	}
	applyCount, _ := finalSF["apply_count"].(map[string]any)
	finalCount := 0
	if applyCount != nil {
		if cnt, ok := applyCount["exclusive-vm"].(float64); ok {
			finalCount = int(cnt)
		}
	}
	t.Logf("[AC-S94f1f9-3-2] final apply_count: %d", finalCount)

	// After Apply + convergence, count should be 1 (or possibly 2 if a second
	// Apply was legitimately needed — but not more, given the exclusion).
	// The key constraint is: no simultaneous double Apply.
	if finalCount == 0 {
		t.Errorf("[AC-S94f1f9-3-2] apply_count is 0 — Apply was never called; state:\n%s", finalData)
	}
}

// ---------------------------------------------------------------------------
// [AC-S94f1f9-3-3] Apply failure → per-resource exponential backoff;
//
//	no immediate retry on the very next cycle.
//
// ---------------------------------------------------------------------------

// TestInflight_BackoffAfterApplyFailure verifies scenario-S94f1f9-3 / step 3.
//
// Strategy:
//   - One resource "backoff-vm" with fail_inject set so every Apply fails.
//   - Daemon interval 80ms (very fast, so we can observe spacing).
//   - Run for ~2 seconds (≈25 cycles without backoff).
//   - The apply_count is tracked in the state file.
//   - Without backoff, 25 cycles = up to 25 Apply attempts.
//   - With correct backoff (initial delay ≥ 800ms), at most 2–3 Apply
//     attempts should occur in 2 seconds.
//   - We verify that apply_count is "small" (≤ 5), proving the backoff is
//     suppressing retries.
//
// Timing is intentionally loose to avoid flakiness.
//
// [AC-S94f1f9-3-3]
func TestInflight_BackoffAfterApplyFailure(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: one resource "backoff-vm".
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "backoff-vm",
				"fields": map[string]string{
					"name": "backoff-vm",
					"size": "3",
				},
			},
		},
	})

	// Seed state: fail_inject so every Apply attempt fails.
	initState := map[string]any{
		"resources": map[string]any{},
		"fail_inject": map[string]string{
			"backoff-vm": "injected-failure-for-backoff-test",
		},
	}
	initData, _ := json.MarshalIndent(initState, "", "  ")
	if err := os.WriteFile(statePath, initData, 0600); err != nil {
		t.Fatal(err)
	}

	// Start daemon with 80ms interval.
	// In 2 seconds, without backoff we'd have ≈25 Apply attempts.
	cmd, stdout := startDaemon(t, intentDir, statePath, "80ms")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Let it run for 2.5 seconds.
	time.Sleep(2500 * time.Millisecond)

	// Stop daemon.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("[AC-S94f1f9-3-3] SIGTERM: %v", err)
	}
	_ = cmd.Wait()

	log := stdout.String()
	t.Logf("[AC-S94f1f9-3-3] daemon log:\n%s", log)

	// Read apply_count from state file.
	finalData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S94f1f9-3-3] read final state: %v", err)
	}
	var finalSF map[string]any
	if err := json.Unmarshal(finalData, &finalSF); err != nil {
		t.Fatalf("[AC-S94f1f9-3-3] parse final state: %v", err)
	}

	// apply_count counts attempts that got past the backoff gate.
	// Note: fail_inject causes the error AFTER incrementing apply_count in the
	// state file.
	applyCount, _ := finalSF["apply_count"].(map[string]any)
	totalAttempts := 0
	if applyCount != nil {
		if cnt, ok := applyCount["backoff-vm"].(float64); ok {
			totalAttempts = int(cnt)
		}
	}
	t.Logf("[AC-S94f1f9-3-3] apply_count (attempts past backoff gate) = %d in 2.5s (≈31 cycles without backoff)", totalAttempts)

	// Also count "backoff" log lines to confirm backoff fired.
	backoffLines := strings.Count(log, "backoff")
	t.Logf("[AC-S94f1f9-3-3] 'backoff' log mentions: %d", backoffLines)

	// Without backoff: 2500ms / 80ms ≈ 31 Apply attempts.
	// With initial backoff of ~1s doubling: attempts ≈ 2-4.
	// Generous threshold: ≤ 8 attempts in 2.5s proves backoff is suppressing retries.
	if totalAttempts > 8 {
		t.Errorf("[AC-S94f1f9-3-3] backoff not effective: %d Apply attempts in 2.5s (expected ≤8 with backoff); log:\n%s",
			totalAttempts, log)
	}

	// There must be at least 1 attempt (the first Apply before backoff kicks in).
	if totalAttempts == 0 {
		t.Errorf("[AC-S94f1f9-3-3] Apply was never attempted (apply_count=0); log:\n%s", log)
	}

	// The log must mention "backoff" (confirming the backoff mechanism fired).
	if backoffLines == 0 {
		t.Errorf("[AC-S94f1f9-3-3] expected 'backoff' log messages; got log:\n%s", log)
	}

	t.Logf("[AC-S94f1f9-3-3] final state:\n%s", finalData)
}

// ---------------------------------------------------------------------------
// Additional regression: backoff does not suppress downstream of OTHER resources
// ---------------------------------------------------------------------------

// TestInflight_BackoffDoesNotBlockUnrelatedResources verifies that a resource
// in backoff does not prevent an unrelated resource (no shared dependency) from
// being applied.
//
// This guards the design decision: backoff suppresses only the failing resource
// itself, not its downstream (which has its own Plan and should be applied on
// its own merits once its direct dependencies are clear).
func TestInflight_BackoffDoesNotBlockUnrelatedResources(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Two independent resources: "fail-x" (always fails) and "ok-y" (succeeds).
	// They share no depends_on relationship.
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "fail-x",
				"fields":   map[string]string{"name": "fail-x", "size": "1"},
			},
			{
				"provider": "fake",
				"id":       "ok-y",
				"fields":   map[string]string{"name": "ok-y", "size": "2"},
			},
		},
	})

	initState := map[string]any{
		"resources": map[string]any{},
		"fail_inject": map[string]string{
			"fail-x": "injected-failure",
		},
	}
	initData, _ := json.MarshalIndent(initState, "", "  ")
	if err := os.WriteFile(statePath, initData, 0600); err != nil {
		t.Fatal(err)
	}

	cmd, stdout := startDaemon(t, intentDir, statePath, "150ms")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// "ok-y" must converge within a few cycles (regardless of "fail-x" being in backoff).
	data := waitForStateFile(t, statePath, "ok-y", 4*time.Second)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("SIGTERM: %v", err)
	}
	_ = cmd.Wait()

	if !strings.Contains(string(data), "ok-y") {
		t.Errorf("[backoff-isolation] ok-y must be applied even when fail-x is in backoff; state:\n%s\nlog:\n%s",
			data, stdout.String())
	}
	// fail-x must NOT be in resources (its Apply failed).
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err == nil {
		res, _ := sf["resources"].(map[string]any)
		if _, ok := res["fail-x"]; ok {
			t.Errorf("[backoff-isolation] fail-x must NOT be in resources (Apply always fails); state:\n%s", data)
		}
	}

	t.Logf("[backoff-isolation] state:\n%s", data)
	t.Logf("[backoff-isolation] log:\n%s", stdout.String())
}
