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

// remediation_test.go contains CLI-level acceptance tests for Story Scf7762-3.
//
// Scenario: "remediation mode: 検知のみ運用"
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and inspect its stdout, stderr, exit code, and side effects on the state file.
// No internal package is imported directly; only the binary's observable
// behaviour is verified.
//
// AC tags map to scenario-Scf7762-3.json:
//
//	[AC-Scf7762-3-1] alert-only mode: drift is detected and reported; Apply is
//	                  NOT executed; reality (state file) is byte-for-byte unchanged.
//	[AC-Scf7762-3-2] global freeze (--freeze): overrides mode to alert-only;
//	                  Apply is NOT executed; output says FROZEN.
//	[AC-Scf7762-3-3] auto mode (default): drift IS corrected; Apply executes;
//	                  state file is updated to reflect desired state.
package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers specific to remediation tests
// ---------------------------------------------------------------------------

// seedDriftedState writes a state file where "drift-vm" has size="1" (reality)
// while intent will declare size="42" (desired) — creating a drift condition.
func seedDriftedState(t *testing.T, statePath string) {
	t.Helper()
	sf := map[string]any{
		"resources": map[string]any{
			"drift-vm": map[string]any{
				"fields": map[string]any{
					"name": "drift-vm",
					"size": "1", // drifted: desired will be "42"
				},
			},
		},
		"provider_version": testProviderVersion,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("seedDriftedState: marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("seedDriftedState: write: %v", err)
	}
}

// writeDriftIntent writes an intent file declaring "drift-vm" with size="42".
func writeDriftIntent(t *testing.T, intentDir string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "drift-vm",
				"fields": map[string]string{
					"name": "drift-vm",
					"size": "42",
				},
			},
		},
	})
}

// stateFileBytes reads the state file and returns its raw bytes.
// Fails the test if the file does not exist.
func stateFileBytes(t *testing.T, statePath string) []byte {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("stateFileBytes: read %s: %v", statePath, err)
	}
	return data
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-3-1] alert-only: Apply not executed, state file byte-unchanged
// ---------------------------------------------------------------------------

// TestAlertOnly_NoDrift verifies that when there is no drift, alert-only mode
// reports no drift detected and exits 0.
func TestAlertOnly_NoDrift(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Desired: drift-vm size=42.
	writeDriftIntent(t, intentDir)
	initStateFile(t, statePath)

	// First run in auto mode to converge reality to desired.
	_, _, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[AC-Scf7762-3-1/no-drift] initial auto reconcile failed (exit %d)", code)
	}

	// Now run in alert-only mode. No drift → should report no drift.
	stdout, _, code := runReconcile(t, intentDir, statePath, "--mode=alert-only")
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-1/no-drift] expected exit 0 in alert-only; got %d\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "no drift") {
		t.Errorf("[AC-Scf7762-3-1/no-drift] expected 'no drift' in output; got:\n%s", stdout)
	}
}

// TestAlertOnly_ReportsDriftWithoutApply verifies [AC-Scf7762-3-1]:
//
//   - alert-only mode detects drift and reports it via log/output.
//   - Apply is NOT executed (reality does NOT change).
//   - The state file is byte-for-byte identical before and after.
func TestAlertOnly_ReportsDriftWithoutApply(t *testing.T) {
	// [AC-Scf7762-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with drift: size=1 but desired will be size=42.
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	// Capture state file bytes BEFORE alert-only run.
	before := stateFileBytes(t, statePath)

	// Run reconcile in alert-only mode.
	stdout, _, code := runReconcile(t, intentDir, statePath, "--mode=alert-only")
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-1] expected exit 0 in alert-only; got %d\nstdout: %s", code, stdout)
	}

	// Must report drift in output.
	if !strings.Contains(stdout, "drifted") && !strings.Contains(stdout, "DRIFT EVENT") {
		t.Errorf("[AC-Scf7762-3-1] expected drift report in stdout; got:\n%s", stdout)
	}

	// Must NOT report having applied changes.
	if strings.Contains(stdout, "change(s) applied") {
		t.Errorf("[AC-Scf7762-3-1] alert-only must not report applied changes; got:\n%s", stdout)
	}

	// Must NOT contain the Apply-style op lines produced by printOp (e.g.
	// "~ UPDATE", "+ CREATE", "- DELETE").  The DRIFT EVENT output is allowed
	// to reference op kinds as part of the event message.  Checking for the
	// Apply-style marker prefix ("~ ", "+ ", "- ") distinguishes them.
	if strings.Contains(stdout, "~ UPDATE") || strings.Contains(stdout, "+ CREATE") || strings.Contains(stdout, "- DELETE") {
		t.Errorf("[AC-Scf7762-3-1] alert-only must not print Apply-style op lines (Apply was not called); got:\n%s", stdout)
	}

	// State file must be byte-for-byte identical (Apply was never called).
	after := stateFileBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-3-1] state file changed after alert-only reconcile — Apply was called\nbefore: %s\nafter:  %s",
			before, after)
	}

	t.Logf("[AC-Scf7762-3-1] alert-only output:\n%s", stdout)
	t.Logf("[AC-Scf7762-3-1] state file (unchanged):\n%s", after)
}

// TestAlertOnly_CreateDriftWithoutApply verifies that when reality is empty
// (CREATE would be needed), alert-only mode reports the drift but does NOT
// create the resource in the state file.
func TestAlertOnly_CreateDriftWithoutApply(t *testing.T) {
	// [AC-Scf7762-3-1] variant: CREATE path
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Intent: one resource; reality: empty state (provider_version set but no resources).
	writeDriftIntent(t, intentDir)
	initStateFile(t, statePath)

	// Run in alert-only mode; Apply must not be called so the resources map stays empty.
	stdout, _, code := runReconcile(t, intentDir, statePath, "--mode=alert-only")
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-1/create] expected exit 0 in alert-only; got %d\nstdout: %s", code, stdout)
	}

	// Must report drift.
	if !strings.Contains(stdout, "drifted") && !strings.Contains(stdout, "DRIFT EVENT") {
		t.Errorf("[AC-Scf7762-3-1/create] expected drift report; got:\n%s", stdout)
	}

	// State file must have no resources written (Apply was not called).
	data, _ := os.ReadFile(statePath)
	var sfOut map[string]any
	// F-4: a malformed state file must fail the test, not silently skip the
	// resources-empty assertion.  Guarding the check with "err == nil" meant an
	// unparseable state file would pass this test without ever verifying that
	// Apply did not run.
	if err := json.Unmarshal(data, &sfOut); err != nil {
		t.Fatalf("[AC-Scf7762-3-1/create] state file is not valid JSON after alert-only: %v; content:\n%s", err, data)
	}
	if res, ok := sfOut["resources"].(map[string]any); ok && len(res) > 0 {
		t.Errorf("[AC-Scf7762-3-1/create] state file has resources after alert-only — Apply was called; content:\n%s", data)
	}

	t.Logf("[AC-Scf7762-3-1/create] alert-only output:\n%s", stdout)
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-3-2] global freeze: overrides to alert-only
// ---------------------------------------------------------------------------

// TestFreeze_OverridesToAlertOnly verifies [AC-Scf7762-3-2]:
//
//   - --freeze flag overrides --mode=auto to alert-only.
//   - Apply is NOT executed; drift is detected and reported.
//   - Output explicitly mentions FROZEN.
//   - State file is byte-for-byte unchanged.
func TestFreeze_OverridesToAlertOnly(t *testing.T) {
	// [AC-Scf7762-3-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with drift.
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	before := stateFileBytes(t, statePath)

	// Run with --freeze (and default mode=auto — freeze must override it).
	stdout, _, code := runReconcile(t, intentDir, statePath, "--freeze")
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-2] expected exit 0 when frozen; got %d\nstdout: %s", code, stdout)
	}

	// Must mention FROZEN in output.
	if !strings.Contains(stdout, "FROZEN") {
		t.Errorf("[AC-Scf7762-3-2] expected 'FROZEN' in output; got:\n%s", stdout)
	}

	// Must report drift (detection continues under freeze).
	if !strings.Contains(stdout, "drifted") && !strings.Contains(stdout, "DRIFT EVENT") {
		t.Errorf("[AC-Scf7762-3-2] expected drift report under freeze; got:\n%s", stdout)
	}

	// Must NOT have applied changes (no Apply-style marker lines).
	if strings.Contains(stdout, "change(s) applied") || strings.Contains(stdout, "~ UPDATE") || strings.Contains(stdout, "+ CREATE") {
		t.Errorf("[AC-Scf7762-3-2] freeze must not apply changes; got:\n%s", stdout)
	}

	// State file must be byte-for-byte unchanged.
	after := stateFileBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-3-2] state file changed under freeze — Apply was called\nbefore: %s\nafter:  %s",
			before, after)
	}

	t.Logf("[AC-Scf7762-3-2] freeze output:\n%s", stdout)
}

// TestFreeze_OverridesExplicitAutoMode verifies that --freeze overrides
// --mode=auto explicitly (most-restrictive-wins, §14.3).
func TestFreeze_OverridesExplicitAutoMode(t *testing.T) {
	// [AC-Scf7762-3-2] most-restrictive-wins variant
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	before := stateFileBytes(t, statePath)

	// Explicitly set --mode=auto AND --freeze: freeze must win.
	stdout, _, code := runReconcile(t, intentDir, statePath, "--mode=auto", "--freeze")
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-2/auto+freeze] expected exit 0; got %d\nstdout: %s", code, stdout)
	}

	// Must say FROZEN, not apply.
	if !strings.Contains(stdout, "FROZEN") {
		t.Errorf("[AC-Scf7762-3-2/auto+freeze] expected FROZEN in output; got:\n%s", stdout)
	}

	// State must be unchanged.
	after := stateFileBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-3-2/auto+freeze] state changed despite freeze\nbefore: %s\nafter:  %s",
			before, after)
	}

	t.Logf("[AC-Scf7762-3-2/auto+freeze] output:\n%s", stdout)
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-3-3] auto mode: Apply IS executed
// ---------------------------------------------------------------------------

// TestAutoMode_AppliesDrift verifies [AC-Scf7762-3-3]:
//
//   - auto mode (default) detects drift and applies the correction.
//   - The state file is updated to reflect desired state.
func TestAutoMode_AppliesDrift(t *testing.T) {
	// [AC-Scf7762-3-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with drift: size=1, desired size=42.
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	// Run in auto mode (default, no --mode flag needed).
	stdout, _, code := runReconcile(t, intentDir, statePath)
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-3] expected exit 0 in auto mode; got %d\nstdout: %s", code, stdout)
	}

	// Must report UPDATE (Apply was called and changed reality).
	if !strings.Contains(stdout, "UPDATE") {
		t.Errorf("[AC-Scf7762-3-3] expected UPDATE op in stdout (Apply was called); got:\n%s", stdout)
	}

	// State file must now reflect desired state (size=42).
	data := stateFileBytes(t, statePath)
	if !strings.Contains(string(data), "42") {
		t.Errorf("[AC-Scf7762-3-3] state file does not reflect desired size=42; content:\n%s", data)
	}

	// Verify size field was actually updated to 42.
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("[AC-Scf7762-3-3] parse state: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	res, _ := resources["drift-vm"].(map[string]any)
	fields, _ := res["fields"].(map[string]any)
	size, _ := fields["size"].(string)
	if size != "42" {
		t.Errorf("[AC-Scf7762-3-3] expected size=42 in state, got %q; state:\n%s", size, data)
	}

	t.Logf("[AC-Scf7762-3-3] auto output:\n%s", stdout)
	t.Logf("[AC-Scf7762-3-3] state after auto:\n%s", data)
}

// TestAutoMode_ExplicitFlag verifies that --mode=auto also works explicitly.
func TestAutoMode_ExplicitFlag(t *testing.T) {
	// [AC-Scf7762-3-3] explicit --mode=auto variant
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	stdout, _, code := runReconcile(t, intentDir, statePath, "--mode=auto")
	if code != 0 {
		t.Errorf("[AC-Scf7762-3-3/explicit] expected exit 0; got %d\nstdout: %s", code, stdout)
	}

	if !strings.Contains(stdout, "UPDATE") {
		t.Errorf("[AC-Scf7762-3-3/explicit] expected UPDATE op; got:\n%s", stdout)
	}

	data := stateFileBytes(t, statePath)
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatalf("[AC-Scf7762-3-3/explicit] parse state: %v", err)
	}
	resources, _ := sf["resources"].(map[string]any)
	res, _ := resources["drift-vm"].(map[string]any)
	fields, _ := res["fields"].(map[string]any)
	size, _ := fields["size"].(string)
	if size != "42" {
		t.Errorf("[AC-Scf7762-3-3/explicit] expected size=42, got %q", size)
	}
	t.Logf("[AC-Scf7762-3-3/explicit] output:\n%s", stdout)
}

// ---------------------------------------------------------------------------
// Daemon path: alert-only and freeze in the convergence loop
// ---------------------------------------------------------------------------

// TestDaemonAlertOnly_NeverApplies verifies that the daemon loop running in
// alert-only mode never modifies the state file, even across multiple cycles,
// while drift exists.
//
// [AC-Scf7762-3-1] daemon path
func TestDaemonAlertOnly_NeverApplies(t *testing.T) {
	// [AC-Scf7762-3-1] daemon
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with drift.
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	before := stateFileBytes(t, statePath)

	// Start daemon in alert-only mode with a fast interval.
	cmd, _ := startDaemonWithFlags(t, filepath.Join(tmp, "daemon.log"),
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "100ms",
		"--mode=alert-only",
	)
	// Read the log file separately.
	logPath := filepath.Join(tmp, "daemon.log")

	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Let 5 cycles run (500ms at 100ms interval).
	time.Sleep(600 * time.Millisecond)

	// Send SIGTERM and wait for clean exit.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("[AC-Scf7762-3-1/daemon] send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("[AC-Scf7762-3-1/daemon] daemon did not exit within 5s after SIGTERM")
	}

	// State file must be byte-for-byte unchanged (Apply was never called).
	after := stateFileBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-3-1/daemon] state file changed in alert-only daemon — Apply was called\nbefore: %s\nafter:  %s",
			before, after)
	}

	// Read the log file to check for "alert-only" messages.
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Logf("[AC-Scf7762-3-1/daemon] could not read log: %v", err)
	}
	log := string(logData)
	if !strings.Contains(log, "alert-only") {
		t.Errorf("[AC-Scf7762-3-1/daemon] expected 'alert-only' in daemon log; got:\n%s", log)
	}

	t.Logf("[AC-Scf7762-3-1/daemon] daemon log:\n%s", log)
}

// TestDaemonFreeze_NeverApplies verifies that the daemon loop running with
// --freeze never modifies the state file, and log output mentions FROZEN.
//
// [AC-Scf7762-3-2] daemon path
func TestDaemonFreeze_NeverApplies(t *testing.T) {
	// [AC-Scf7762-3-2] daemon
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with drift.
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	before := stateFileBytes(t, statePath)

	logPath := filepath.Join(tmp, "daemon.log")
	cmd, _ := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "100ms",
		"--freeze",
	)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Let 5 cycles run.
	time.Sleep(600 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("[AC-Scf7762-3-2/daemon] send SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("[AC-Scf7762-3-2/daemon] daemon did not exit within 5s after SIGTERM")
	}

	// State must be byte-for-byte unchanged.
	after := stateFileBytes(t, statePath)
	if !bytes.Equal(before, after) {
		t.Errorf("[AC-Scf7762-3-2/daemon] state changed under freeze daemon\nbefore: %s\nafter:  %s",
			before, after)
	}

	logData, _ := os.ReadFile(logPath)
	log := string(logData)
	if !strings.Contains(log, "FROZEN") {
		t.Errorf("[AC-Scf7762-3-2/daemon] expected 'FROZEN' in daemon log; got:\n%s", log)
	}

	t.Logf("[AC-Scf7762-3-2/daemon] daemon log:\n%s", log)
}

// TestDaemonAuto_AppliesDrift verifies that the daemon in auto mode (default)
// eventually converges the drifted resource.
//
// [AC-Scf7762-3-3] daemon path
func TestDaemonAuto_AppliesDrift(t *testing.T) {
	// [AC-Scf7762-3-3] daemon
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with drift: size=1, desired=42.
	seedDriftedState(t, statePath)
	writeDriftIntent(t, intentDir)

	logPath := filepath.Join(tmp, "daemon.log")
	cmd, _ := startDaemonWithFlags(t, logPath,
		"--intent", intentDir,
		"--state", statePath,
		"--interval", "100ms",
		// No --mode or --freeze: defaults to auto.
	)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait for the daemon to converge (size should become "42").
	converged := waitForCondition(3*time.Second, func() bool {
		data, err := os.ReadFile(statePath)
		if err != nil {
			return false
		}
		var sf map[string]any
		if err := json.Unmarshal(data, &sf); err != nil {
			return false
		}
		resources, ok := sf["resources"].(map[string]any)
		if !ok {
			return false
		}
		res, ok := resources["drift-vm"].(map[string]any)
		if !ok {
			return false
		}
		fields, ok := res["fields"].(map[string]any)
		if !ok {
			return false
		}
		size, _ := fields["size"].(string)
		return size == "42"
	})

	if !converged {
		data, _ := os.ReadFile(statePath)
		logData, _ := os.ReadFile(logPath)
		t.Errorf("[AC-Scf7762-3-3/daemon] drift was not corrected within deadline\nstate: %s\nlog: %s",
			data, logData)
	} else {
		t.Log("[AC-Scf7762-3-3/daemon] drift corrected by auto-mode daemon")
	}
}

// ---------------------------------------------------------------------------
// Invalid flag value
// ---------------------------------------------------------------------------

// TestMode_InvalidValue verifies that an unknown --mode value produces a
// non-zero exit and an error message.
func TestMode_InvalidValue(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	writeDriftIntent(t, intentDir)

	_, stderr, code := runReconcile(t, intentDir, statePath, "--mode=banana")
	if code == 0 {
		t.Errorf("expected non-zero exit for unknown --mode value, got 0")
	}
	if !strings.Contains(stderr, "banana") && !strings.Contains(stderr, "unknown") {
		t.Errorf("expected error message about unknown mode; got stderr:\n%s", stderr)
	}
	t.Logf("invalid mode error: %s", stderr)
}
