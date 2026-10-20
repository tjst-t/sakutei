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

// planned_change_profile_test.go は Backlog #44（planned_change の承認連携と運用プロファイル、
// §13.3 / §14.4 / §14.5）の受け入れテストを保持する。実バイナリを fake provider に対して
// 回し、結果は現実（state ファイルのリソースの存否）と S4 監査ログで確かめる。
//
//	[AC-44-1] enterprise では requirements を書かなくても planned_change に N-of-M + separation
//	          が要る（承認者 1 人・申請者本人の承認では開かず、申請者以外の 2 人で開く）。
//	[AC-44-2] solo の cooling_off: 常駐ループは宣言から N が過ぎるまで進まず、過ぎたら進む。
//	          宣言・レーンが開いたこと・実行を S4 に残す。
//	[AC-44-3] solo の cooling_off: 非常駐の reconcile は「その回に N 経っていれば通す」ゲート。
//	          経過前の reconcile は進まず、経過後の reconcile は進む。経過前に宣言を消せば止まる。
//	[AC-44-4] 個別の宣言がプロファイルに優先する（enterprise でも宣言の 1-of-1 で通る、solo でも
//	          宣言の N-of-M は cooling-off の後も効く）。
package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
)

type pcRig struct {
	t         *testing.T
	intentDir string
	statePath string
	logPath   string
	auditPath string
	// now pins the declaration's window, so rewriting the intent keeps the
	// same declaration (its Key includes the window).
	now time.Time
}

// newPCRig seeds keep-decom + decom-1..5 in pool=decom; only keep-decom is
// desired, so the 5 others are prune candidates the normal budget (1 per
// cycle) halts.
func newPCRig(t *testing.T) *pcRig {
	t.Helper()
	tmp := t.TempDir()
	r := &pcRig{t: t, intentDir: filepath.Join(tmp, "intent"), statePath: filepath.Join(tmp, "state.json"),
		logPath: filepath.Join(tmp, "daemon.log"), auditPath: filepath.Join(tmp, "audit.jsonl"), now: time.Now()}
	if err := os.MkdirAll(r.intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	reality := []pcResource{{"keep-decom", pcDecom}}
	for i := 1; i <= 5; i++ {
		reality = append(reality, pcResource{fmt.Sprintf("decom-%d", i), pcDecom})
	}
	seedScopedPruneable(t, r.statePath, reality)
	return r
}

// write writes the intent: keep-decom, the top-level keys, and (when pc is
// not nil) a delete planned_change for pool=decom with pc's keys merged in.
func (r *pcRig) write(top map[string]any, pc map[string]any) {
	r.t.Helper()
	intent := map[string]any{"resources": []map[string]any{pcDesired("keep-decom", pcDecom)}}
	for k, v := range top {
		intent[k] = v
	}
	if pc != nil {
		decl := map[string]any{"scope": pcDecom, "kind": "delete", "max_count": 5,
			"window": pcWindow(r.now.Add(-time.Hour), r.now.Add(time.Hour))}
		for k, v := range pc {
			decl[k] = v
		}
		intent["planned_change"] = decl
	}
	writeIntentFile(r.t, r.intentDir, intent)
}

func (r *pcRig) remaining() int { return countResourcesInState(r.t, r.statePath) }

func (r *pcRig) reconcile() string {
	r.t.Helper()
	out, _, _ := runReconcileCombined(r.t, r.intentDir, r.statePath,
		"--max-destructive-per-cycle=1", "--audit-log", r.auditPath)
	return out
}

func (r *pcRig) auditKinds() map[audit.Kind]int {
	r.t.Helper()
	recs, err := audit.ReadAll(r.auditPath)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[audit.Kind]int{}
	for _, rec := range recs {
		out[rec.Kind]++
	}
	return out
}

func pcApproval(ids ...string) map[string]any {
	return map[string]any{"pr": 44, "approvers": ids}
}

// [AC-44-1] enterprise: 承認者 1 人・申請者本人を含む 2 人では開かず、申請者以外の 2 人で開く。
func TestPlannedChangeProfile_EnterpriseNeedsNofMAndSeparation(t *testing.T) {
	r := newPCRig(t)
	ent := map[string]any{"profile": "enterprise"}
	r.write(ent, map[string]any{"applicant": "carol", "approval": pcApproval("alice")})

	stop := startPlannedChangeDaemon(t, r.intentDir, r.statePath, r.logPath,
		"--max-destructive-per-cycle=1", "--audit-log", r.auditPath)
	waitForMoreCycles(t, r.logPath, 4)
	if n := r.remaining(); n != 6 {
		t.Fatalf("[AC-44-1] one approver must not open the lane under enterprise: %d resources remain; log:\n%s", n, readFileString(r.logPath))
	}
	if log := readFileString(r.logPath); !strings.Contains(log, "PlannedChangeUnapproved") ||
		!strings.Contains(log, "1 of the 2 approver(s) required by the enterprise profile") || !strings.Contains(log, "BUDGET HALTED") {
		t.Fatalf("[AC-44-1] want PlannedChangeUnapproved (1 of 2) and the normal budget halt; log:\n%s", log)
	}

	// The applicant's own approval is not counted (separation).
	r.write(ent, map[string]any{"applicant": "carol", "approval": pcApproval("carol", "alice")})
	waitForMoreCycles(t, r.logPath, 4)
	if n := r.remaining(); n != 6 {
		t.Fatalf("[AC-44-1] the applicant's own approval must not count: %d resources remain", n)
	}
	if log := readFileString(r.logPath); !strings.Contains(log, "carol is the applicant and cannot approve") {
		t.Fatalf("[AC-44-1] want the separation refusal in the log:\n%s", log)
	}

	// Two approvers other than the applicant open the lane.
	r.write(ent, map[string]any{"applicant": "carol", "approval": pcApproval("alice", "bob")})
	if !waitForResourceCount(t, r.statePath, 1, 10*time.Second) {
		t.Fatalf("[AC-44-1] 2 approvers other than the applicant must open the lane: %d remain; log:\n%s", r.remaining(), readFileString(r.logPath))
	}
	waitForMoreCycles(t, r.logPath, 2) // the cycle's events are printed after it
	stop()
	if !strings.Contains(readFileString(r.logPath), "PlannedChangeOpened") {
		t.Errorf("[AC-44-1] want PlannedChangeOpened in the log")
	}
	k := r.auditKinds()
	if k[audit.KindPlannedChangeOpened] != 1 || k[audit.KindPlannedChangeExecuted] < 1 {
		t.Errorf("[AC-44-1] S4 must record the opening and the executions: %v", k)
	}
}

// [AC-44-2] solo の常駐: cooling_off の間は進まず（その間に宣言を消せば止まる）、過ぎたら進む。
func TestPlannedChangeProfile_SoloCoolingOff_Resident(t *testing.T) {
	r := newPCRig(t)
	const coolingOff = 3 * time.Second
	r.write(map[string]any{"profile": "solo", "solo": map[string]any{"cooling_off": coolingOff.String()}}, map[string]any{})
	declared := time.Now()

	stop := startPlannedChangeDaemon(t, r.intentDir, r.statePath, r.logPath,
		"--max-destructive-per-cycle=1", "--audit-log", r.auditPath)
	waitForMoreCycles(t, r.logPath, 3)
	if time.Since(declared) < coolingOff {
		if n := r.remaining(); n != 6 {
			t.Fatalf("[AC-44-2] the lane opened during the cooling-off: %d resources remain; log:\n%s", n, readFileString(r.logPath))
		}
	}
	if !strings.Contains(readFileString(r.logPath), "PlannedChangeCoolingOff") {
		t.Fatalf("[AC-44-2] want PlannedChangeCoolingOff in the log:\n%s", readFileString(r.logPath))
	}

	if !waitForResourceCount(t, r.statePath, 1, 15*time.Second) {
		t.Fatalf("[AC-44-2] after the cooling-off the declared deletes must land: %d remain; log:\n%s", r.remaining(), readFileString(r.logPath))
	}
	waitForMoreCycles(t, r.logPath, 2) // the cycle's S4 records are flushed at its end
	stop()
	if time.Since(declared) < coolingOff {
		t.Fatalf("[AC-44-2] deletes landed before the cooling-off elapsed")
	}
	recs, err := audit.ReadAll(r.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	var declaredAt, openedAt time.Time
	executed := 0
	for _, rec := range recs {
		switch rec.Kind {
		case audit.KindPlannedChangeDeclared:
			declaredAt = rec.Time
		case audit.KindPlannedChangeOpened:
			openedAt = rec.Time
		case audit.KindPlannedChangeExecuted:
			executed += rec.Count
		}
	}
	if declaredAt.IsZero() || openedAt.Sub(declaredAt) < coolingOff || executed != 5 {
		t.Errorf("[AC-44-2] S4: declared %v, opened %v (want >= %s later), executed %d (want 5)", declaredAt, openedAt, coolingOff, executed)
	}
}

// [AC-44-3] solo の非常駐（段0-zero）: 経過前の reconcile は進まず、経過後の reconcile は進む。
func TestPlannedChangeProfile_SoloCoolingOff_Reconcile(t *testing.T) {
	r := newPCRig(t)
	const coolingOff = 2 * time.Second
	solo := map[string]any{"profile": "solo", "solo": map[string]any{"cooling_off": coolingOff.String()}}
	r.write(solo, map[string]any{})

	out := r.reconcile()
	if n := r.remaining(); n != 6 {
		t.Fatalf("[AC-44-3] the first reconcile is inside the cooling-off: %d remain; out:\n%s", n, out)
	}
	if !strings.Contains(out, "PlannedChangeCoolingOff") {
		t.Fatalf("[AC-44-3] want PlannedChangeCoolingOff; out:\n%s", out)
	}
	if out := r.reconcile(); r.remaining() != 6 {
		t.Fatalf("[AC-44-3] a second reconcile before the cooling-off ends must not proceed; out:\n%s", out)
	}
	// Without the S4 log the one-shot run cannot know when it was declared.
	time.Sleep(coolingOff + 200*time.Millisecond)
	if out, _, _ := runReconcileCombined(t, r.intentDir, r.statePath, "--max-destructive-per-cycle=1"); r.remaining() != 6 ||
		!strings.Contains(out, "need --audit-log") {
		t.Fatalf("[AC-44-3] reconcile without --audit-log must not open the lane; out:\n%s", out)
	}

	out = r.reconcile()
	if n := r.remaining(); n != 1 {
		t.Fatalf("[AC-44-3] the reconcile after the cooling-off must proceed: %d remain; out:\n%s", n, out)
	}
	k := r.auditKinds()
	if k[audit.KindPlannedChangeDeclared] != 1 || k[audit.KindPlannedChangeOpened] != 1 || k[audit.KindPlannedChangeExecuted] != 1 {
		t.Errorf("[AC-44-3] S4 (one declared across runs, one opened, one executed): %v", k)
	}

	// Halting: a declaration removed during its cooling-off never opens.
	h := newPCRig(t)
	h.write(solo, map[string]any{})
	h.reconcile()
	h.write(solo, nil) // the declaration is reverted
	time.Sleep(coolingOff + 200*time.Millisecond)
	if out := h.reconcile(); h.remaining() != 6 || !strings.Contains(out, "PlannedChangeWithdrawn") {
		t.Fatalf("[AC-44-3] a removed declaration must not open its lane and is withdrawn; out:\n%s", out)
	}
	// Writing the same declaration back (revert of the revert) starts the
	// cooling-off over: the old declaration time no longer counts.
	h.write(solo, map[string]any{})
	if out := h.reconcile(); h.remaining() != 6 || !strings.Contains(out, "PlannedChangeCoolingOff") {
		t.Fatalf("[AC-44-3] a declaration written back must cool off again: %d remain; out:\n%s", h.remaining(), out)
	}
	time.Sleep(coolingOff + 200*time.Millisecond)
	if out := h.reconcile(); h.remaining() != 1 {
		t.Fatalf("[AC-44-3] after the new cooling-off the written-back declaration proceeds: %d remain; out:\n%s", h.remaining(), out)
	}
	if k := h.auditKinds(); k[audit.KindPlannedChangeDeclared] != 2 || k[audit.KindPlannedChangeWithdrawn] != 1 {
		t.Errorf("[AC-44-3] S4 (declared, withdrawn, declared again): %v", k)
	}
}

// [AC-44-3] 常駐でも、cooling-off 中に消した宣言を書き戻すと cooling-off はやり直しになる。
func TestPlannedChangeProfile_SoloCoolingOff_RedeclareRestarts_Resident(t *testing.T) {
	r := newPCRig(t)
	const coolingOff = 2 * time.Second
	solo := map[string]any{"profile": "solo", "solo": map[string]any{"cooling_off": coolingOff.String()}}
	r.write(solo, map[string]any{})
	stop := startPlannedChangeDaemon(t, r.intentDir, r.statePath, r.logPath,
		"--max-destructive-per-cycle=1", "--audit-log", r.auditPath)
	waitForMoreCycles(t, r.logPath, 2)
	r.write(solo, nil) // halted during the cooling-off
	waitForMoreCycles(t, r.logPath, 2)
	time.Sleep(coolingOff + 200*time.Millisecond) // the first declaration's cooling-off would be over
	r.write(solo, map[string]any{})
	rewritten := time.Now()
	waitForMoreCycles(t, r.logPath, 3)
	if time.Since(rewritten) < coolingOff && r.remaining() != 6 {
		t.Fatalf("[AC-44-3] the written-back declaration opened without a new cooling-off; log:\n%s", readFileString(r.logPath))
	}
	if !strings.Contains(readFileString(r.logPath), "PlannedChangeWithdrawn") {
		t.Fatalf("[AC-44-3] want PlannedChangeWithdrawn in the log:\n%s", readFileString(r.logPath))
	}
	if !waitForResourceCount(t, r.statePath, 1, 15*time.Second) {
		t.Fatalf("[AC-44-3] after the new cooling-off the deletes must land: %d remain", r.remaining())
	}
	stop()
	if time.Since(rewritten) < coolingOff {
		t.Fatalf("[AC-44-3] deletes landed before the new cooling-off elapsed")
	}
}

// [AC-44-4] 個別の宣言がプロファイルに優先する。
func TestPlannedChangeProfile_DeclarationWinsOverProfile(t *testing.T) {
	// enterprise: the declaration's 1-of-1 needs only one approver.
	r := newPCRig(t)
	r.write(map[string]any{"profile": "enterprise"}, map[string]any{
		"approval":     pcApproval("alice"),
		"requirements": map[string]any{"quorum": map[string]any{"n": 1, "of": 1}},
	})
	if out := r.reconcile(); r.remaining() != 1 {
		t.Fatalf("[AC-44-4] enterprise + explicit 1-of-1 with one approver must open: %d remain; out:\n%s", r.remaining(), out)
	}

	// solo: the declaration's 2-of-3 still holds after the cooling-off.
	s := newPCRig(t)
	pc := map[string]any{
		"approval":     pcApproval("alice"),
		"requirements": map[string]any{"quorum": map[string]any{"n": 2, "of": 3}},
		"cooling_off":  "1s",
	}
	s.write(map[string]any{"profile": "solo"}, pc)
	s.reconcile()
	time.Sleep(1200 * time.Millisecond)
	if out := s.reconcile(); s.remaining() != 6 || !strings.Contains(out, "required by the declaration's requirements") {
		t.Fatalf("[AC-44-4] solo + explicit 2-of-3 with one approver must stay closed: %d remain; out:\n%s", s.remaining(), out)
	}
	pc["approval"] = pcApproval("alice", "bob")
	s.write(map[string]any{"profile": "solo"}, pc)
	s.reconcile() // a new approval is the same declaration; its cooling-off already ran
	if out := s.reconcile(); s.remaining() != 1 {
		t.Fatalf("[AC-44-4] solo + explicit 2-of-3 with two approvers must open: %d remain; out:\n%s", s.remaining(), out)
	}
}
