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

// chaos_planned_change_test.go は Backlog #39（planned_change と専用予算レーン、§13.3）の
// chaos テストを保持する。実バイナリの常駐ループを fake provider に対して回し、結果は
// ログではなく現実（state ファイルのリソース件数・存否）で確かめる。
//
//	[AC-39-1] planned_change の窓内で 500 件の削除が通常予算を消費せずに進み、同じ周期の
//	          宣言外の削除は通常予算で halt する。宣言済みの件数減は List 急減 halt に数えない。
//	[AC-39-2] 窓を過ぎたら宣言は失効し、同じ scope の削除は通常予算に戻って halt する。
//	[AC-39-3] kind: reimage の delete+create の対は破壊予算に delete として数えない。
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pcResource is one seeded reality resource with its List scope.
type pcResource struct {
	id    string
	scope map[string]string
}

// seedScopedPruneable writes reality: every resource with fields name/size=1,
// its List scope, and a pruneable type.
func seedScopedPruneable(t *testing.T, statePath string, rs []pcResource) {
	t.Helper()
	resources := map[string]any{}
	scopes := map[string]any{}
	for _, r := range rs {
		resources[r.id] = map[string]any{"fields": map[string]any{"name": r.id, "size": "1"}}
		scopes[r.id] = r.scope
	}
	data, err := json.MarshalIndent(map[string]any{
		"resources":            resources,
		"scopes":               scopes,
		"pruneable_capability": true,
		"provider_version":     testProviderVersion,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// addScopedPruneable adds resources to the existing reality (keeping the rest).
func addScopedPruneable(t *testing.T, statePath string, rs []pcResource) {
	t.Helper()
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var sf map[string]any
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	resources, _ := sf["resources"].(map[string]any)
	scopes, _ := sf["scopes"].(map[string]any)
	if scopes == nil {
		scopes = map[string]any{}
	}
	for _, r := range rs {
		resources[r.id] = map[string]any{"fields": map[string]any{"name": r.id, "size": "1"}}
		scopes[r.id] = r.scope
	}
	sf["resources"], sf["scopes"] = resources, scopes
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func pcDesired(id string, scope map[string]string) map[string]any {
	return map[string]any{
		"provider":        "fake",
		"id":              id,
		"scope":           scope,
		"deletion_policy": "PruneWithinScope",
		"fields":          map[string]string{"name": id, "size": "1"},
	}
}

func pcWindow(start, end time.Time) string {
	return start.UTC().Format(time.RFC3339) + " .. " + end.UTC().Format(time.RFC3339)
}

func startPlannedChangeDaemon(t *testing.T, intentDir, statePath, logPath string, flags ...string) func() {
	t.Helper()
	args := append([]string{"--intent", intentDir, "--state", statePath, "--interval", "200ms"}, flags...)
	cmd, wait := startDaemonWithFlags(t, logPath, args...)
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = wait()
		}
	}
	t.Cleanup(stop)
	return stop
}

func countLog(logPath, substr string) int {
	return strings.Count(readFileString(logPath), substr)
}

// waitForCycles waits until the daemon has logged n more "cycle " summary
// lines than now (so later cycles really ran).
func waitForMoreCycles(t *testing.T, logPath string, n int) {
	t.Helper()
	base := countLog(logPath, "sakutei: cycle ")
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if countLog(logPath, "sakutei: cycle ") >= base+n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon did not run %d more cycles; log:\n%s", n, readFileString(logPath))
}

var (
	pcDecom = map[string]string{"pool": "decom"}
	pcOther = map[string]string{"pool": "other"}
)

// [AC-39-1] 500 件の棚卸し削除が planned_change のレーンで進み、同じ周期の宣言外の削除
// （別 scope）は通常予算で halt する。宣言済みの件数減（507 → 7）は List 急減 halt を起こさない。
func TestChaosPlannedChange_500DeletesInLane_UndeclaredHalted(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	reality := []pcResource{{"keep-decom", pcDecom}, {"keep-other", pcOther}}
	for i := 1; i <= 500; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("decom-%03d", i), pcDecom})
	}
	for i := 1; i <= 5; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("stray-%d", i), pcOther})
	}
	seedScopedPruneable(t, statePath, reality)

	now := time.Now()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{pcDesired("keep-decom", pcDecom), pcDesired("keep-other", pcOther)},
		"planned_change": map[string]any{
			"scope":     pcDecom,
			"kind":      "delete",
			"max_count": 500,
			"window":    pcWindow(now.Add(-time.Hour), now.Add(time.Hour)),
			"approval":  map[string]any{"pr": 1423, "approvers": []string{"alice", "bob"}},
		},
	})

	stop := startPlannedChangeDaemon(t, intentDir, statePath, logPath,
		"--max-destructive-per-cycle=3", "--max-destructive-per-window=3", "--window-duration=1h")

	// 500 decom deletes land: 507 → 7 (keep-decom, keep-other, stray-1..5).
	if !waitForResourceCount(t, statePath, 7, 20*time.Second) {
		t.Fatalf("[AC-39-1] the 500 declared deletes did not land: %d resources remain; log:\n%s",
			countResourcesInState(t, statePath), readFileString(logPath))
	}
	// Several more cycles: the List drop 507 → 7 must not halt prune, and the
	// strays must stay halted by the normal budget.
	waitForMoreCycles(t, logPath, 4)
	stop()
	log := readFileString(logPath)

	for i := 1; i <= 5; i++ {
		if id := fmt.Sprintf("stray-%d", i); !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-39-1] undeclared delete %s leaked: the normal budget (3) must halt the 5 strays", id)
		}
	}
	for _, id := range []string{"keep-decom", "keep-other"} {
		if !resourceExistsInState(t, statePath, id) {
			t.Errorf("[AC-39-1] desired %s was removed", id)
		}
	}
	if !strings.Contains(log, "BUDGET HALTED") {
		t.Errorf("[AC-39-1] the undeclared deletes must halt the normal budget in the same cycle; log:\n%s", log)
	}
	if !strings.Contains(log, "500 change(s) admitted to the planned_change lane") {
		t.Errorf("[AC-39-1] want the lane admission of 500 changes in the log")
	}
	if strings.Contains(log, "count dropped from") {
		t.Errorf("[AC-39-1] the declared shrink must not trip the List drop-rate halt (§12.4); log:\n%s", log)
	}
	// The normal budget was not consumed: its halt reports 0 already committed.
	if !strings.Contains(log, "0 destructive ops already committed") &&
		!strings.Contains(log, "5 destructive ops (+ 0 in-flight = 5) exceeds max_destructive_per_cycle=3") {
		t.Errorf("[AC-39-1] the normal budget must see only the 5 undeclared deletes; log:\n%s", log)
	}
}

// [AC-39-2] 窓の中では同じ scope の削除がレーンで通り、窓を過ぎたら宣言は失効して
// 通常予算に戻る（後から現れた削除候補は halt され、現実に残る）。
func TestChaosPlannedChange_WindowExpiry_BackToNormalBudget(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	reality := []pcResource{{"keep-decom", pcDecom}}
	for i := 1; i <= 20; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("decom-%03d", i), pcDecom})
	}
	seedScopedPruneable(t, statePath, reality)

	now := time.Now()
	end := now.Add(3 * time.Second)
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{pcDesired("keep-decom", pcDecom)},
		"planned_change": map[string]any{
			"scope": pcDecom, "kind": "delete", "max_count": 500,
			"window":   pcWindow(now.Add(-time.Hour), end),
			"approval": map[string]any{"approvers": []string{"alice"}}, // team: one approver (§14.5)
		},
	})

	stop := startPlannedChangeDaemon(t, intentDir, statePath, logPath, "--max-destructive-per-cycle=3")

	if !waitForResourceCount(t, statePath, 1, 10*time.Second) {
		t.Fatalf("[AC-39-2] the 20 deletes inside the window did not land; log:\n%s", readFileString(logPath))
	}
	if time.Now().After(end) {
		t.Skip("the window closed before the in-window phase finished (slow machine)")
	}
	if !waitForLogFile(logPath, "PlannedChangeExpired", 10*time.Second) {
		t.Fatalf("[AC-39-2] the declaration did not expire after its window; log:\n%s", readFileString(logPath))
	}

	// After the window: 10 new orphans in the same scope.
	var late []pcResource
	for i := 1; i <= 10; i++ {
		late = append(late, pcResource{fmt.Sprintf("late-%02d", i), pcDecom})
	}
	addScopedPruneable(t, statePath, late)
	waitForMoreCycles(t, logPath, 4)
	stop()
	log := readFileString(logPath)

	for _, r := range late {
		if !resourceExistsInState(t, statePath, r.id) {
			t.Errorf("[AC-39-2] %s was deleted after the window closed; the normal budget (3/cycle) must halt 10 deletes", r.id)
		}
	}
	after := log[strings.Index(log, "PlannedChangeExpired"):]
	if !strings.Contains(after, "BUDGET HALTED") {
		t.Errorf("[AC-39-2] want BUDGET HALTED after the expiry; log:\n%s", log)
	}
}

// [AC-39-3] kind: reimage: 5 台の再イメージ（同じ slot の gen1 削除 + gen2 作成）は破壊予算に
// delete として数えない。累積窓予算 1 のもとで、同じ周期の宣言外の削除 1 件は通り（窓に 1 だけ
// 計上）、その後の宣言外の削除はもう 1 件で窓を超えて halt する。
func TestChaosPlannedChange_ReimageNotCountedAsDelete(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")

	slot := func(i int) map[string]string {
		return map[string]string{"pool": "hpc", "slot": fmt.Sprintf("%02d", i)}
	}
	reality := []pcResource{{"keep-other", pcOther}, {"stray-1", pcOther}}
	desired := []map[string]any{pcDesired("keep-other", pcOther)}
	for i := 1; i <= 5; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("node-%02d-gen1", i), slot(i)})
		desired = append(desired, pcDesired(fmt.Sprintf("node-%02d-gen2", i), slot(i)))
	}
	seedScopedPruneable(t, statePath, reality)

	now := time.Now()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": desired,
		"planned_change": map[string]any{
			"scope": map[string]string{"pool": "hpc"}, "kind": "reimage", "max_count": 5,
			"window":   pcWindow(now.Add(-time.Hour), now.Add(time.Hour)),
			"approval": map[string]any{"approvers": []string{"alice"}}, // team: one approver (§14.5)
		},
	})

	stop := startPlannedChangeDaemon(t, intentDir, statePath, logPath,
		"--max-destructive-per-cycle=1", "--max-destructive-per-window=1", "--window-duration=1h")

	// keep-other + 5 gen2 = 6, and stray-1 deleted by the normal budget.
	// Wait for exactly that reality (keep-other + the 5 gen1 is also 6
	// without stray-1, while the re-images are still in flight), then for a
	// cycle that found nothing to do: the daemon's Applies rewrite the state
	// file, and one still in flight would overwrite the stray-2 added below.
	settled := func() bool {
		if countResourcesInState(t, statePath) != 6 || resourceExistsInState(t, statePath, "stray-1") {
			return false
		}
		for i := 1; i <= 5; i++ {
			if resourceExistsInState(t, statePath, fmt.Sprintf("node-%02d-gen1", i)) ||
				!resourceExistsInState(t, statePath, fmt.Sprintf("node-%02d-gen2", i)) {
				return false
			}
		}
		return true
	}
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) && !settled() {
		time.Sleep(20 * time.Millisecond)
	}
	if settled() {
		converged := countLog(logPath, "converged (no changes)")
		if !waitForLogFileCount(logPath, "converged (no changes)", converged+1, 10*time.Second) {
			t.Fatalf("[AC-39-3] the daemon did not settle after the re-images; log:\n%s", readFileString(logPath))
		}
	}
	for i := 1; i <= 5; i++ {
		if resourceExistsInState(t, statePath, fmt.Sprintf("node-%02d-gen1", i)) {
			t.Errorf("[AC-39-3] node-%02d-gen1 was not re-imaged away", i)
		}
		if !resourceExistsInState(t, statePath, fmt.Sprintf("node-%02d-gen2", i)) {
			t.Errorf("[AC-39-3] node-%02d-gen2 was not created", i)
		}
	}
	if resourceExistsInState(t, statePath, "stray-1") {
		t.Fatalf("[AC-39-3] stray-1 must be deleted within the window budget of 1 — the 5 reimage deletes must not count; log:\n%s",
			readFileString(logPath))
	}

	// The window budget holds exactly 1 (stray-1): one more undeclared delete exceeds it.
	addScopedPruneable(t, statePath, []pcResource{{"stray-2", pcOther}})
	if !waitForLogFile(logPath, "1 destructive ops already committed + 1 planned = 2 exceeds max_destructive_per_window=1", 10*time.Second) {
		t.Fatalf("[AC-39-3] want the window budget to report exactly 1 committed delete; log:\n%s", readFileString(logPath))
	}
	stop()
	if !resourceExistsInState(t, statePath, "stray-2") {
		t.Errorf("[AC-39-3] stray-2 was deleted beyond the window budget")
	}
}
