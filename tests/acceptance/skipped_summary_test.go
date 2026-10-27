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

// skipped_summary_test.go — #85: デーモンの周の要約は、観測できず飛ばした資源が
// ある周に「converged (no changes)」と出さず、飛ばした件数を出す。Availability は
// 数字でなく名前で出す。
//
//	[AC-85-1]  Unknown で 1 件飛ばした周の要約は
//	           "not converged: 1 resource(s) skipped (no changes)" で、"converged (" は出ない。
//	[AC-85-2]  観測できなかった行の Availability は "availability=unknown" と名前で出る。
//	[AC-85-3]  core.Availability の String は ok / unknown / converging。
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func TestSkippedSummary_SkippedCycleNotConverged(t *testing.T) {
	// [AC-85-1] / [AC-85-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	seedStateWithObserveUnknown(t, statePath, map[string]map[string]any{
		"phase1-vm": {"name": "phase1-vm", "size": "4"},
	}, []string{"phase1-vm"})
	writeOneResourceIntent(t, intentDir)

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms")
	if !waitForLogFile(logPath, "cycle 2:", 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = waitCmd()
		t.Fatalf("[AC-85-1] daemon did not reach cycle 2; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)

	if !strings.Contains(log, "cycle 1: 1 resource(s) skipped") {
		t.Fatalf("[AC-85-1] cycle 1 did not skip the unobservable resource; log:\n%s", log)
	}
	if s := cycleSummaries(log)["1"]; s != "not converged: 1 resource(s) skipped (no changes)" {
		t.Errorf("[AC-85-1] cycle 1 summary %q, want not converged: 1 resource(s) skipped (no changes); log:\n%s", s, log)
	}
	if strings.Contains(log, "converged (no changes)") {
		t.Errorf("[AC-85-1] a skipped cycle was reported as converged; log:\n%s", log)
	}
	if !strings.Contains(log, "availability=unknown (not Ok)") {
		t.Errorf("[AC-85-2] availability not logged by name; log:\n%s", log)
	}
}

func TestSkippedSummary_AvailabilityString(t *testing.T) {
	// [AC-85-3]
	for a, want := range map[core.Availability]string{
		core.AvailabilityOk:         "ok",
		core.AvailabilityUnknown:    "unknown",
		core.AvailabilityConverging: "converging",
	} {
		if got := a.String(); got != want {
			t.Errorf("[AC-85-3] Availability(%d).String() = %q, want %q", int(a), got, want)
		}
	}
}
