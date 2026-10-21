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

// pruned_summary_test.go — #76: デーモンの周の要約は、prune で orphan を消した周に
// 「converged (no changes)」と出さず、消した件数を出す。
//
//	[AC-76-1]  2 件の orphan を消した周の要約に "pruned=2" が出て、"converged" は出ない。
//	[AC-76-2]  消した操作と止めた操作が同じ周にあれば、要約に両方
//	           （"halted (N op(s) held)" と "pruned=M"）が出る。
//	[AC-76-3]  何も変えず何も止めていない周は従来どおり "converged (no changes)"。
package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPrunedSummary_PrunedCycleNotConverged(t *testing.T) {
	// [AC-76-1] / [AC-76-3]
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
		"--intent", intentDir, "--state", statePath, "--interval", "50ms")
	if !waitForLogFile(logPath, "cycle 2:", 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = waitCmd()
		t.Fatalf("[AC-76-1] daemon did not reach cycle 2; log:\n%s", readFileString(logPath))
	}
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)

	if got := countResourcesInState(t, statePath); got != 1 {
		t.Fatalf("[AC-76-1] %d resource(s) remain, want 1 (the 2 orphans pruned); log:\n%s", got, log)
	}
	if !strings.Contains(log, "cycle 1: pruned=2 orphan(s)") {
		t.Fatalf("[AC-76-1] cycle 1 did not prune the 2 orphans; log:\n%s", log)
	}
	sum := cycleSummaries(log)
	if s := sum["1"]; !strings.Contains(s, "pruned=2") || strings.Contains(s, "converged") {
		t.Errorf("[AC-76-1] cycle 1 summary %q, want pruned=2 and no converged; log:\n%s", s, log)
	}
	// [AC-76-3] the next cycle has nothing left to do.
	if s := sum["2"]; s != "converged (no changes)" {
		t.Errorf("[AC-76-3] cycle 2 summary %q, want converged (no changes); log:\n%s", s, log)
	}
}

func TestPrunedSummary_PrunedAndHeldInSameCycle(t *testing.T) {
	// [AC-76-2] The planned_change lane prunes the declared orphans while the
	// normal budget holds the undeclared ones in the same cycle.
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	reality := []pcResource{{"keep-decom", pcDecom}, {"keep-other", pcOther}}
	for i := 1; i <= 2; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("decom-%d", i), pcDecom})
	}
	for i := 1; i <= 3; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("stray-%d", i), pcOther})
	}
	seedScopedPruneable(t, statePath, reality)

	now := time.Now()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{pcDesired("keep-decom", pcDecom), pcDesired("keep-other", pcOther)},
		"planned_change": map[string]any{
			"scope":     pcDecom,
			"kind":      "delete",
			"max_count": 2,
			"window":    pcWindow(now.Add(-time.Hour), now.Add(time.Hour)),
			"approval":  map[string]any{"pr": 76, "approvers": []string{"alice", "bob"}},
		},
	})

	stop := startPlannedChangeDaemon(t, intentDir, statePath, logPath, "--max-destructive-per-cycle=1")
	if !waitForLogFile(logPath, "pruned=2 orphan(s)", 10*time.Second) {
		t.Fatalf("[AC-76-2] the 2 declared deletes were not pruned; log:\n%s", readFileString(logPath))
	}
	stop()
	log := readFileString(logPath)

	m := regexp.MustCompile(`cycle (\d+): pruned=2 orphan\(s\)`).FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("[AC-76-2] no cycle-numbered pruned line; log:\n%s", log)
	}
	s := cycleSummaries(log)[m[1]]
	if !strings.Contains(s, "halted (3 op(s) held)") || !strings.Contains(s, "pruned=2") || strings.Contains(s, "converged") {
		t.Errorf("[AC-76-2] cycle %s summary %q, want halted (3 op(s) held) and pruned=2, no converged; log:\n%s", m[1], s, log)
	}
	for i := 1; i <= 3; i++ {
		if id := fmt.Sprintf("stray-%d", i); !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-76-2] undeclared delete %s leaked past the budget", id)
		}
	}
}
