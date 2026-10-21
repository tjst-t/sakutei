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

// halted_summary_test.go — #73: デーモンの周の要約は、予算・prune の安全装置で
// 止めた操作がある周に「converged」と出さず、halted と止めた件数を出す。
//
//	[AC-73-1]  --max-destructive-per-cycle 1 で 2 件の prune を止めた周の要約は
//	           "halted (2 op(s) held)"。"converged" は出ない。
//	[AC-73-2]  prune の安全装置（List 件数の急減）で止めた周の要約は
//	           "halted (1 op(s) held)"。"converged" は出ない。
//	[AC-73-3]  差分の無い周は従来どおり "converged (no changes)"。
package acceptance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// cycleSummaryRe matches the per-cycle summary line of the daemon.
var cycleSummaryRe = regexp.MustCompile(`(?m)^sakutei: cycle (\d+): observed=\d+ (.*)$`)

// cycleSummaries returns the summary text (after "observed=N ") by cycle number.
func cycleSummaries(log string) map[string]string {
	out := map[string]string{}
	for _, m := range cycleSummaryRe.FindAllStringSubmatch(log, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func TestHaltedSummary_Budget(t *testing.T) {
	// [AC-73-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	seedManyPruneableResources(t, statePath, 3)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms",
		"--max-destructive-per-cycle", "1")
	if !waitForLogFileCount(logPath, "BUDGET HALTED", 2, 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = waitCmd()
		t.Fatalf("[AC-73-1] daemon did not emit BUDGET HALTED twice; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)

	if got := countResourcesInState(t, statePath); got != 3 {
		t.Errorf("[AC-73-1] %d resource(s) remain, want 3 (the budget holds both deletes); log:\n%s", got, log)
	}
	sum := cycleSummaries(log)
	if s := sum["1"]; !strings.Contains(s, "halted (2 op(s) held)") {
		t.Errorf("[AC-73-1] cycle 1 summary %q, want halted (2 op(s) held); log:\n%s", s, log)
	}
	if strings.Contains(log, "converged") {
		t.Errorf("[AC-73-1] a budget-halted cycle was reported as converged; log:\n%s", log)
	}
}

func TestHaltedSummary_PruneSafetyGuard(t *testing.T) {
	// [AC-73-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	writeDesiredKeepOneResource(t, intentDir, "keep-a")
	// Cycle 1 records the List baseline 6 (type not pruneable yet: nothing to
	// delete, nothing held).
	baseline := map[string]map[string]any{"keep-a": {"name": "keep-a", "size": "1"}}
	for _, c := range []string{"b", "c", "d", "e", "f"} {
		baseline["pad-"+c] = map[string]any{"name": "pad-" + c, "size": "1"}
	}
	seedStateWithResources(t, statePath, baseline)

	cmd, waitCmd := startDaemonToFile(t, intentDir, statePath, "300ms", logPath)
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = waitCmd()
		}
	}()
	if !waitForLogFile(logPath, "cycle 1:", 5*time.Second) {
		t.Fatalf("[AC-73-2] daemon did not complete cycle 1; log:\n%s", readFileString(logPath))
	}
	// Shrink reality 6 → 2 with one pruneable orphan: the count-drop guard halts.
	seedStateWithResources(t, statePath, map[string]map[string]any{
		"keep-a":        {"name": "keep-a", "size": "1"},
		"orphan-target": {"name": "orphan-target", "size": "9"},
	})
	seedStatePruneableCapability(t, statePath)
	if !waitForLogFile(logPath, "prune HALTED", 5*time.Second) {
		t.Fatalf("[AC-73-2] no prune HALTED in the daemon log; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	killed = true
	log := readFileString(logPath)

	m := regexp.MustCompile(`cycle (\d+): prune HALTED`).FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("[AC-73-2] no cycle-numbered prune HALTED line; log:\n%s", log)
	}
	s := cycleSummaries(log)[m[1]]
	if !strings.Contains(s, "halted (1 op(s) held)") || strings.Contains(s, "converged") {
		t.Errorf("[AC-73-2] cycle %s summary %q, want halted (1 op(s) held) and no converged; log:\n%s", m[1], s, log)
	}
	if !resourceExistsInState(t, statePath, "orphan-target") {
		t.Errorf("[AC-73-2] orphan-target was deleted despite the halt; log:\n%s", log)
	}
}

func TestHaltedSummary_NoChangesStillConverged(t *testing.T) {
	// [AC-73-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	seedManyPruneableResources(t, statePath, 1)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms",
		"--max-destructive-per-cycle", "1")
	if !waitForLogFile(logPath, "cycle 2:", 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = waitCmd()
		t.Fatalf("[AC-73-3] daemon did not reach cycle 2; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)

	if s := cycleSummaries(log)["1"]; s != "converged (no changes)" {
		t.Errorf("[AC-73-3] cycle 1 summary %q, want converged (no changes); log:\n%s", s, log)
	}
	if strings.Contains(log, "halted") || strings.Contains(log, "HALTED") {
		t.Errorf("[AC-73-3] a cycle without diff reported a halt; log:\n%s", log)
	}
}
