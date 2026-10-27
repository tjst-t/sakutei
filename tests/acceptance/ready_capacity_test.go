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

// ready_capacity_test.go — Backlog #26 (v1b): ready_condition and dependency
// resolution across cycles (§8.3, §11.2), and the capacity gate (§11.4, §13.4).
//
// Tags:
//   [#26-1] depends_on: [{id, ready_condition}] loads; unknown keys are rejected
//   [#26-2] reconcile: the downstream is not applied while the upstream's
//           ready_condition is unmet (several cycles), and converges in the
//           first cycle after it becomes ready
//   [#26-3] daemon: the same across cycles of one process
//   [#26-4] an upstream outside the desired set (external owner) still gates
//   [#26-5] capacity gate: utilization above the threshold halts Apply
//           (reconcile), below it Apply proceeds; unknown utilization halts
//   [#26-6] capacity gate on_exceed=halt-destructive-only lets Creates through
//   [#26-7] daemon: capacity halt holds across cycles and lifts once the
//           observed utilization falls under the threshold

package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/loader"
)

// reconcileCapture runs "sakutei reconcile" with extra flags and returns
// stdout and stderr regardless of the exit code.
func reconcileCapture(t *testing.T, intentDir, statePath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	args := append([]string{"reconcile", "--intent", intentDir, "--state", statePath}, extraArgs...)
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("reconcile: %v", err)
		}
		exitCode = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// readyIntent declares vm-1 and svc-1, where svc-1 waits for vm-1's
// cloud_init_done condition.
func readyIntent(t *testing.T, intentDir string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "vm-1",
				"fields":   map[string]string{"name": "vm-1", "size": "4"},
			},
			{
				"provider": "fake",
				"id":       "svc-1",
				"depends_on": []any{
					map[string]string{"id": "vm-1", "ready_condition": "cloud_init_done"},
				},
				"fields": map[string]string{"name": "svc-1", "size": "1"},
			},
		},
	})
}

// seedFakeState writes a fresh state file with the given resources and
// ready conditions.
func seedFakeState(t *testing.T, statePath string, resources map[string]any, ready map[string]map[string]bool) {
	t.Helper()
	sf := map[string]any{
		"resources":        resources,
		"provider_version": testProviderVersion,
	}
	if ready != nil {
		sf["ready"] = ready
	}
	data, _ := json.MarshalIndent(sf, "", "  ")
	if err := writeFileAtomic(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// setReady rewrites only the "ready" section of an existing state file,
// keeping whatever the reconciler has applied.
func setReady(t *testing.T, statePath string, ready map[string]map[string]bool) {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	sf["ready"] = ready
	out, _ := json.MarshalIndent(sf, "", "  ")
	if err := writeFileAtomic(statePath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// setPoolUsage rewrites the used field of the capacity pool resource.
func setPoolUsage(t *testing.T, statePath, pool string, used int) {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var sf map[string]any
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	res := sf["resources"].(map[string]any)
	p := res[pool].(map[string]any)
	p["fields"].(map[string]any)["used"] = used
	out, _ := json.MarshalIndent(sf, "", "  ")
	if err := writeFileAtomic(statePath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, statePath string) []byte {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ---------------------------------------------------------------------------
// [#26-1] intent form
// ---------------------------------------------------------------------------

func TestReadyCondition_IntentForm(t *testing.T) {
	// [#26-1]
	dir := writeIntentDir(t, "a.yaml", `resources:
  - provider: fake
    id: vm-1
    fields:
      name: vm-1
  - provider: fake
    id: svc-1
    depends_on:
      - vm-0
      - id: vm-1
        ready_condition: cloud_init_done
    fields:
      name: svc-1
`)
	rs, _, err := loader.LoadDir(dir)
	if err != nil {
		t.Fatalf("[#26-1] load: %v", err)
	}
	svc := resourcesByLocalID(rs)["svc-1"]
	if got := strings.Join(svc.DependsOn, ","); got != "vm-0,vm-1" {
		t.Errorf("[#26-1] DependsOn = %q, want vm-0,vm-1", got)
	}
	if len(svc.ReadyConditions) != 1 || svc.ReadyConditions["vm-1"] != "cloud_init_done" {
		t.Errorf("[#26-1] ReadyConditions = %v, want {vm-1: cloud_init_done}", svc.ReadyConditions)
	}

	bad := writeIntentDir(t, "b.yaml", `resources:
  - provider: fake
    id: svc-1
    depends_on:
      - id: vm-1
        ready: cloud_init_done
`)
	if _, _, err := loader.LoadDir(bad); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Errorf("[#26-1] unknown depends_on key must be rejected, got err=%v", err)
	}
	noID := writeIntentDir(t, "c.yaml", `resources:
  - provider: fake
    id: svc-1
    depends_on:
      - ready_condition: cloud_init_done
`)
	if _, _, err := loader.LoadDir(noID); err == nil {
		t.Errorf("[#26-1] depends_on object without id must be rejected")
	}
}

// ---------------------------------------------------------------------------
// [#26-2] reconcile: held until ready, converges the cycle after
// ---------------------------------------------------------------------------

func TestReadyCondition_ReconcileWaitsAcrossCycles(t *testing.T) {
	// [#26-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	readyIntent(t, intentDir)
	seedFakeState(t, statePath, map[string]any{}, nil)

	// Cycle 1: vm-1 is created; it has not reported ready, so svc-1 waits.
	_, stderr, code := reconcileCapture(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[#26-2] cycle 1: exit %d; stderr:\n%s", code, stderr)
	}
	st := readState(t, statePath)
	if !resourceInState(st, "vm-1") {
		t.Fatalf("[#26-2] cycle 1: vm-1 must be applied; state:\n%s", st)
	}
	if resourceInState(st, "svc-1") {
		t.Fatalf("[#26-2] cycle 1: svc-1 must NOT be applied before vm-1 is ready; state:\n%s", st)
	}
	if !strings.Contains(stderr, `waiting for upstream "vm-1" ready_condition "cloud_init_done"`) {
		t.Errorf("[#26-2] cycle 1: expected a ready_condition wait message; stderr:\n%s", stderr)
	}

	// Cycles 2-3: vm-1 exists but the condition is false / a different
	// condition is true.  svc-1 keeps waiting.
	for i, ready := range []map[string]map[string]bool{
		{"vm-1": {"cloud_init_done": false}},
		{"vm-1": {"booted": true}},
	} {
		setReady(t, statePath, ready)
		_, stderr, code := reconcileCapture(t, intentDir, statePath)
		if code != 0 {
			t.Fatalf("[#26-2] cycle %d: exit %d; stderr:\n%s", i+2, code, stderr)
		}
		if st := readState(t, statePath); resourceInState(st, "svc-1") {
			t.Fatalf("[#26-2] cycle %d: svc-1 applied while cloud_init_done is unmet (%v); state:\n%s", i+2, ready, st)
		}
	}

	// Cycle 4: the condition is observed true → svc-1 converges in this cycle.
	setReady(t, statePath, map[string]map[string]bool{"vm-1": {"cloud_init_done": true}})
	stdout, stderr, code := reconcileCapture(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("[#26-2] cycle 4: exit %d; stderr:\n%s", code, stderr)
	}
	if st := readState(t, statePath); !resourceInState(st, "svc-1") {
		t.Fatalf("[#26-2] cycle 4: svc-1 must converge once vm-1 is ready; stdout:\n%s\nstderr:\n%s\nstate:\n%s", stdout, stderr, st)
	}

	// Cycle 5: converged, nothing to do.
	stdout, _, code = reconcileCapture(t, intentDir, statePath)
	if code != 0 || !strings.Contains(stdout, "Converged") {
		t.Errorf("[#26-2] cycle 5: expected Converged, exit 0; got exit %d stdout:\n%s", code, stdout)
	}
}

// ---------------------------------------------------------------------------
// [#26-3] daemon: held over many cycles, released by the observation
// ---------------------------------------------------------------------------

func TestReadyCondition_DaemonReleasesWhenReady(t *testing.T) {
	// [#26-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	readyIntent(t, intentDir)
	seedFakeState(t, statePath, map[string]any{}, nil)

	cmd, out := startDaemon(t, intentDir, statePath, "80ms")
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	waitForResourceInState(t, statePath, "vm-1", 5*time.Second)
	const waitMsg = `skip fake//svc-1: waiting for upstream "vm-1" ready_condition "cloud_init_done"`
	if !waitForCondition(5*time.Second, func() bool { return strings.Count(out.String(), waitMsg) >= 3 }) {
		t.Fatalf("[#26-3] expected svc-1 to be held over several cycles; log:\n%s", out.String())
	}
	if st := readState(t, statePath); resourceInState(st, "svc-1") {
		t.Fatalf("[#26-3] svc-1 applied before vm-1 was ready; state:\n%s", st)
	}

	// vm-1 finishes its bring-up: the next cycle observes it and releases svc-1.
	setReady(t, statePath, map[string]map[string]bool{"vm-1": {"cloud_init_done": true}})
	waitForResourceInState(t, statePath, "svc-1", 5*time.Second)

	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	t.Logf("[#26-3] held for %d cycle(s) before release", strings.Count(out.String(), waitMsg))
}

// ---------------------------------------------------------------------------
// [#26-4] external upstream (not in desired) gates the downstream
// ---------------------------------------------------------------------------

func TestReadyCondition_ExternalUpstream(t *testing.T) {
	// [#26-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{{
			"provider": "fake",
			"id":       "app-1",
			"depends_on": []any{
				map[string]string{"id": "mha-failover", "ready_condition": "failover_done"},
			},
			"fields": map[string]string{"name": "app-1", "size": "1"},
		}},
	})
	// mha-failover is owned by an external orchestrator: it exists in
	// reality but is not declared in intent.
	seedFakeState(t, statePath, map[string]any{
		"mha-failover": map[string]any{"fields": map[string]any{"name": "mha-failover"}},
	}, nil)

	if _, stderr, _ := reconcileCapture(t, intentDir, statePath); resourceInState(readState(t, statePath), "app-1") {
		t.Fatalf("[#26-4] app-1 applied before the external upstream was ready; stderr:\n%s", stderr)
	}

	setReady(t, statePath, map[string]map[string]bool{"mha-failover": {"failover_done": true}})
	_, stderr, code := reconcileCapture(t, intentDir, statePath)
	if code != 0 || !resourceInState(readState(t, statePath), "app-1") {
		t.Fatalf("[#26-4] app-1 must converge once the external upstream is ready; exit %d stderr:\n%s", code, stderr)
	}
}

// ---------------------------------------------------------------------------
// [#26-5] [#26-6] capacity gate (reconcile)
// ---------------------------------------------------------------------------

const gateFlag = "--capacity-gate=resource=cluster-a,used=used,total=total,max_utilization=80%"

// capacitySetup declares vm-new (a Create) and seeds cluster-a with the given
// used amount out of 100 (used < 0 leaves cluster-a absent).
func capacitySetup(t *testing.T, used int) (intentDir, statePath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{{
			"provider": "fake",
			"id":       "vm-new",
			"fields":   map[string]string{"name": "vm-new", "size": "8"},
		}},
	})
	resources := map[string]any{}
	if used >= 0 {
		resources["cluster-a"] = map[string]any{"fields": map[string]any{
			"name": "cluster-a", "used": used, "total": 100,
		}}
	}
	seedFakeState(t, statePath, resources, nil)
	return intentDir, statePath
}

func TestCapacityGate_ReconcileHaltsOverThreshold(t *testing.T) {
	// [#26-5]
	intentDir, statePath := capacitySetup(t, 90)
	stdout, stderr, code := reconcileCapture(t, intentDir, statePath, gateFlag)
	if code == 0 {
		t.Errorf("[#26-5] expected non-zero exit when capacity is exceeded; stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "CapacityThresholdExceeded") || !strings.Contains(stderr, "90.0%") {
		t.Errorf("[#26-5] expected a CapacityThresholdExceeded event naming 90.0%%; stderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, "CAPACITY HALTED") {
		t.Errorf("[#26-5] expected CAPACITY HALTED summary; stdout:\n%s", stdout)
	}
	if resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-5] vm-new must NOT be applied while the cluster is over capacity")
	}

	// The gate is level-triggered: once utilization is back under the
	// threshold, the next reconcile applies.
	setPoolUsage(t, statePath, "cluster-a", 50)
	_, stderr, code = reconcileCapture(t, intentDir, statePath, gateFlag)
	if code != 0 || !resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-5] vm-new must be applied under the threshold; exit %d stderr:\n%s", code, stderr)
	}
}

func TestCapacityGate_ReconcileUnknownUtilizationHalts(t *testing.T) {
	// [#26-5] unknown ≠ within capacity
	intentDir, statePath := capacitySetup(t, -1)
	_, stderr, code := reconcileCapture(t, intentDir, statePath, gateFlag)
	if code == 0 || !strings.Contains(stderr, "utilization unknown") {
		t.Errorf("[#26-5] an unobserved pool must halt; exit %d stderr:\n%s", code, stderr)
	}
	if resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-5] vm-new must NOT be applied while utilization is unknown")
	}
}

func TestCapacityGate_ReconcileWithoutGateApplies(t *testing.T) {
	// [#26-5] control: no gate configured → over-utilization is not our business
	intentDir, statePath := capacitySetup(t, 90)
	if _, stderr, code := reconcileCapture(t, intentDir, statePath); code != 0 || !resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-5] without --capacity-gate vm-new must be applied; exit %d stderr:\n%s", code, stderr)
	}
}

func TestCapacityGate_HaltDestructiveOnly(t *testing.T) {
	// [#26-6]
	intentDir, statePath := capacitySetup(t, 90)
	_, stderr, code := reconcileCapture(t, intentDir, statePath, gateFlag+",on_exceed=halt-destructive-only")
	if !strings.Contains(stderr, "CapacityThresholdExceeded") {
		t.Errorf("[#26-6] expected the capacity event; stderr:\n%s", stderr)
	}
	if !resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-6] halt-destructive-only must let the Create through; exit %d stderr:\n%s", code, stderr)
	}
}

func TestCapacityGate_InvalidFlagRejected(t *testing.T) {
	// [#26-5] a malformed gate is a configuration error, not a silent no-op
	intentDir, statePath := capacitySetup(t, 10)
	_, _, code := reconcileCapture(t, intentDir, statePath, "--capacity-gate=resource=cluster-a,used=used")
	if code == 0 {
		t.Fatalf("[#26-5] an incomplete --capacity-gate must be rejected")
	}
	if resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-5] nothing may be applied on a configuration error")
	}
}

// ---------------------------------------------------------------------------
// [#26-7] capacity gate (daemon)
// ---------------------------------------------------------------------------

func TestCapacityGate_DaemonHaltsThenLifts(t *testing.T) {
	// [#26-7]
	intentDir, statePath := capacitySetup(t, 95)
	logPath := filepath.Join(filepath.Dir(statePath), "daemon.log")
	cmd, wait := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "80ms", gateFlag)
	defer func() {
		_ = cmd.Process.Kill()
		_ = wait()
	}()

	if !waitForLogFileCount(logPath, "CAPACITY HALTED", 3, 5*time.Second) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("[#26-7] expected CAPACITY HALTED over several cycles; log:\n%s", data)
	}
	if resourceInState(readState(t, statePath), "vm-new") {
		t.Fatalf("[#26-7] vm-new applied while the cluster was over capacity")
	}

	setPoolUsage(t, statePath, "cluster-a", 40)
	waitForResourceInState(t, statePath, "vm-new", 5*time.Second)
	_ = cmd.Process.Signal(syscall.SIGTERM)
}
