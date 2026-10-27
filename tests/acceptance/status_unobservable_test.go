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

// status_unobservable_test.go — #117 (#109 の 5): status は観測エラーで止めず、
// 観測できないものとして数えて最後まで報告する（§3 unknown ≠ absent, §26.4）。
//
//	AC-114-3-1  2 provider のうち片方の Read が失敗しても、もう片方の適合が出て、
//	            失敗したリソースは観測できないとして数えられ、終了コード 3
//	AC-114-3-2  全部観測できれば 0（drift があっても 0）
//	AC-114-3-3  Availability=unknown（エラーなし）も観測できないとして 3
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setObserveMode sets the fake provider's observe_unknown entry for localID
// ("error" = Read returns an error; anything else = Availability unknown).
func setObserveMode(t *testing.T, statePath, localID, mode string) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("setObserveMode: read: %v", err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("setObserveMode: unmarshal: %v", err)
	}
	sf["observe_unknown"] = map[string]any{localID: mode}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("setObserveMode: marshal: %v", err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatalf("setObserveMode: write: %v", err)
	}
}

// setupTwoProviders writes an intent with one fake resource (bmc-1) and one
// dnszone record (www), the fake reality in sync and the zone with wwwValue.
func setupTwoProviders(t *testing.T, wwwValue string) (intentDir, statePath, zonePath string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	zonePath = filepath.Join(tmp, "example.com.zone")
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "bmc-1",
				"fields":   map[string]string{"name": "bmc-1", "size": "10"},
			},
			dnszoneResource("www.example.com./A", "www.example.com.", "A", "1.2.3.4", "300", ""),
		},
	})
	seedState(t, statePath, map[string]map[string]any{
		"bmc-1": {"name": "bmc-1", "size": "10"},
	})
	writeZoneFile(t, zonePath, "$ORIGIN example.com.\nwww\t300\tIN\tA\t"+wwwValue+"\n")
	return intentDir, statePath, zonePath
}

// TestStatus_ReadErrorReportsTheRest: the fake's Read fails, the dnszone
// record is still evaluated (converged), the failed resource is counted as
// unobservable with its error, and status exits 3.
//
// [AC-114-3-1]
func TestStatus_ReadErrorReportsTheRest(t *testing.T) {
	// [AC-114-3-1]
	intentDir, statePath, zonePath := setupTwoProviders(t, "1.2.3.4")
	setObserveMode(t, statePath, "bmc-1", "error")

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath, "--zone", zonePath)
	if code != 3 {
		t.Fatalf("[AC-114-3-1] expected exit 3, got %d; stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "要約: drifted=0 converged=1 unobservable=1") {
		t.Errorf("[AC-114-3-1] the dnszone record should be converged and bmc-1 unobservable; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "UNOBSERVABLE fake//bmc-1") {
		t.Errorf("[AC-114-3-1] bmc-1 should be listed as UNOBSERVABLE; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "bmc-1") || !strings.Contains(stdout, "injected observe error") {
		t.Errorf("[AC-114-3-1] the report should carry bmc-1's observe error; got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "observe") || !strings.Contains(stderr, "bmc-1") {
		t.Errorf("[AC-114-3-1] stderr should carry the per-resource error; got: %s", stderr)
	}
	assertNoApplyVerbs(t, "[AC-114-3-1]", stdout)
}

// TestStatus_ReadErrorStillListsOtherDrift: with the fake failing, a drift on
// the other provider is still listed.
//
// [AC-114-3-1]
func TestStatus_ReadErrorStillListsOtherDrift(t *testing.T) {
	// [AC-114-3-1]
	intentDir, statePath, zonePath := setupTwoProviders(t, "9.9.9.9")
	setObserveMode(t, statePath, "bmc-1", "error")

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath, "--zone", zonePath)
	if code != 3 {
		t.Fatalf("[AC-114-3-1] expected exit 3, got %d; stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "DRIFTED") || !strings.Contains(stdout, "www.example.com.") {
		t.Errorf("[AC-114-3-1] the dnszone drift should still be listed; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "要約: drifted=1 converged=0 unobservable=1") {
		t.Errorf("[AC-114-3-1] expected drifted=1 unobservable=1; got:\n%s", stdout)
	}
}

// TestStatus_AllObservedExitsZero: every resource observed → exit 0, even
// with a drift.
//
// [AC-114-3-2]
func TestStatus_AllObservedExitsZero(t *testing.T) {
	// [AC-114-3-2]
	intentDir, statePath, zonePath := setupTwoProviders(t, "9.9.9.9")

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath, "--zone", zonePath)
	if code != 0 {
		t.Fatalf("[AC-114-3-2] expected exit 0, got %d; stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "要約: drifted=1 converged=1 unobservable=0") {
		t.Errorf("[AC-114-3-2] expected drifted=1 converged=1 unobservable=0; got:\n%s", stdout)
	}
	if strings.Contains(stdout, "UNOBSERVABLE") {
		t.Errorf("[AC-114-3-2] nothing should be UNOBSERVABLE; got:\n%s", stdout)
	}
}

// TestStatus_UnknownAvailabilityExitsThree: Availability unknown without a
// Read error is also "not observed" → exit 3, with the reason shown.
//
// [AC-114-3-3]
func TestStatus_UnknownAvailabilityExitsThree(t *testing.T) {
	// [AC-114-3-3]
	intentDir, statePath, zonePath := setupTwoProviders(t, "1.2.3.4")
	setObserveMode(t, statePath, "bmc-1", "unknown")

	stdout, stderr, code := runStatusCmd(t, intentDir, statePath, "--zone", zonePath)
	if code != 3 {
		t.Fatalf("[AC-114-3-3] expected exit 3, got %d; stdout:\n%s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "要約: drifted=0 converged=1 unobservable=1") ||
		!strings.Contains(stdout, "availability=") {
		t.Errorf("[AC-114-3-3] bmc-1 should be unobservable with its availability; got:\n%s", stdout)
	}
}
