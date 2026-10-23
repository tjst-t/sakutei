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

// apply_failed_summary_test.go — #110（採番の試用 docs/trial/allocation の「見つけたこと」8、
// 手順 6）: 一回性の reconcile で Apply が Permanent で失敗したのに、要約が
// 「Converged (no changes)」と出ていた。要約は失敗した件数を言う。終了コード 0 は
// 既存の決まり（AC-S94f1f9-2-2、level-triggered）なので変えない。
//
//	[AC-110-1]  Apply が Permanent で失敗しただけの reconcile の要約は
//	            "Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)" で、
//	            "Converged" は出ない。終了コードは 0。
//	[AC-110-2]  ほかのリソースが収束した周でも、要約に失敗の件数が付く
//	            （"Reconcile: 1 change(s) applied; 1 resource(s) failed to apply"）。終了コードは 0。
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reconcileBadA runs a one-shot reconcile against a dnszone whose intent has
// an A record with a non-address value (dnszone's Apply fails Permanent, #98),
// plus the extra resources given.
func reconcileBadA(t *testing.T, extra string) (string, int) {
	t.Helper()
	tmp := t.TempDir()
	intent := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "main.yaml"), `version: "1"
resources:
  - provider: dnszone
    id: app5.example.com./A
    fields:
      name: app5.example.com.
      type: A
      value: "8004"
`+extra)
	zonePath := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zonePath, "$ORIGIN example.com.\n")
	out, code := runSakutei(t, "reconcile", "--intent", intent, "--zone", zonePath)
	if !strings.Contains(out, "permanent") || !strings.Contains(out, "not an IPv4 address") {
		t.Fatalf("the Apply failure line must be printed:\n%s", out)
	}
	return out, code
}

func TestApplyFailedSummary_OnlyFailure(t *testing.T) {
	// [AC-110-1]
	out, code := reconcileBadA(t, "")
	if code != 0 {
		t.Fatalf("[AC-110-1] exit code must stay 0 (AC-S94f1f9-2-2), got %d:\n%s", code, out)
	}
	if strings.Contains(out, "Converged") {
		t.Errorf("[AC-110-1] the summary must not say converged:\n%s", out)
	}
	if !strings.Contains(out, "Reconcile: not converged: 1 resource(s) failed to apply (no changes applied)") {
		t.Errorf("[AC-110-1] the summary must give the failed count:\n%s", out)
	}
}

func TestApplyFailedSummary_WithAppliedChanges(t *testing.T) {
	// [AC-110-2]
	out, code := reconcileBadA(t, `  - provider: dnszone
    id: www.example.com./A
    fields:
      name: www.example.com.
      type: A
      value: 192.0.2.10
`)
	if code != 0 {
		t.Fatalf("[AC-110-2] exit code must stay 0 (AC-S94f1f9-2-2), got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Reconcile: 1 change(s) applied; 1 resource(s) failed to apply") {
		t.Errorf("[AC-110-2] the summary must give the applied and failed counts:\n%s", out)
	}
}
