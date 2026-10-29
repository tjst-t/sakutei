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

// workflow_done_converge_test.go: done になった Workflow の対象を一回性の
// reconcile でも宣言どおりに保つ（Backlog #209、§6.3）。
//
// 1 台（n1）の導入: drain（active=false）→ 版を上げる（version=2）→ 準備を待つ
// （ready=true）→ undrain。終わりの姿は done_when（version=2）。n1 の宣言は
// size=2・active=true（Workflow が done になれば収束ループが保つ）。
//
//	AC-209-1 — --workflow-store を渡した一回性の reconcile は、done の Workflow の対象を宣言どおりに戻す（store を書かない）
//	AC-209-2 — store が無いとき、開いていない trigger: reimage の対象は収束させる（窓の中の宣言があれば持つ）
//	AC-209-3 — store が無いとき、done_when が観測で満たされた Workflow の対象は収束させ、満たされないものは触らずに理由を出す
//	AC-209-4 — store を消した常駐は、done_when が満たされた Workflow を起こし直さず done として store に書く
//	AC-209-5 — 2 つの常駐が進めるロックを取り合うと、片方だけが進める
//	AC-209-6 — halt した Workflow の対象は触らず、理由を status・plan・level の Event に出す
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// doneIntent は n1 の宣言と導入の Workflow "rolling"。doneWhen は Workflow に足す
// 欄（空なら done_when なし）、extraStep は手順の最後に足す手順（空なら無し）。
func doneIntent(doneWhen, extraStep string) string {
	steps := `
        { "apply": { "active": "false" } },
        { "apply": { "version": "2" } },
        { "wait": { "fields": { "ready": "true" } } },
        { "apply": { "active": "true" } }`
	if extraStep != "" {
		steps += ",\n        " + extraStep
	}
	if doneWhen != "" {
		doneWhen = ",\n      " + doneWhen
	}
	return `{
  "resources": [
    { "provider": "fake", "id": "n1", "fields": { "name": "n1", "size": "2", "active": "true" } }
  ],
  "workflows": [
    { "workflow": "rolling",
      "target": { "provider": "fake" },
      "stages": [ { "name": "s1", "targets": ["n1"] } ],
      "steps": [` + steps + `
      ]` + doneWhen + ` }
  ]
}`
}

const doneWhenVersion2 = `"done_when": { "fields": { "version": "2" } }`

func (r *wfRig) setIntent(intent string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// setN1 は n1 の現実を fields にする（ほかの欄は消える）。
func (r *wfRig) setN1(fields map[string]any) {
	r.t.Helper()
	r.seed("n1", fields)
}

// [AC-209-1] store を渡した一回性の reconcile は done の対象を宣言どおりに戻す。
func TestWorkflowDone_OneShotWithStoreConverges(t *testing.T) {
	r := newWFRig(t, doneIntent("", ""))
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "true", "ready": "true"})
	r.cycleUntil(10, func(run workflow.Run) bool { return run.Phase == workflow.PhaseDone })
	r.cycle() // done になった周の次から収束ループが n1 を保つ
	if r.field("n1", "size") != "2" {
		t.Fatalf("the daemon did not converge n1 after the Workflow was done: size %s", r.field("n1", "size"))
	}
	r.d.Abandon()
	before, err := os.ReadFile(r.storePath)
	if err != nil {
		t.Fatal(err)
	}

	// 終わったあとに現実がずれる。
	r.setN1(map[string]any{"name": "n1", "size": 9, "active": "true", "version": "2", "ready": "true"})

	// store が無ければ、done かどうか分からない（done_when も無い）ので触らない。
	_, stderr, _ := runReconcileCombined(t, r.intentDir, r.fp.StatePath())
	if r.field("n1", "size") != "9" {
		t.Fatalf("[AC-209-1] reconcile without a store applied the Workflow target n1")
	}
	if !strings.Contains(stderr, "target of workflow rolling") || !strings.Contains(stderr, "progress unknown (no --workflow-store") {
		t.Fatalf("[AC-209-1] reconcile without a store must say why n1 is held:\n%s", stderr)
	}

	// store を渡せば、done の Workflow の対象はふつうのリソースとして戻す。
	stdout, stderr, code := runReconcileCombined(t, r.intentDir, r.fp.StatePath(), "--workflow-store", r.storePath)
	if code != 0 {
		t.Fatalf("[AC-209-1] reconcile --workflow-store: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if r.field("n1", "size") != "2" {
		t.Fatalf("[AC-209-1] reconcile --workflow-store did not converge the done Workflow's target n1:\n%s", stderr)
	}
	if strings.Contains(stderr, "target of workflow rolling") {
		t.Fatalf("[AC-209-1] a done Workflow's target was held:\n%s", stderr)
	}
	// 読むだけ: store は変わらない（一回性は Workflow を進めない）。
	after, err := os.ReadFile(r.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("[AC-209-1] the one-shot reconcile wrote the workflow store:\nbefore %s\nafter  %s", before, after)
	}
}

// reimageIntent は scope rack=r1 の n1 と trigger: reimage の Workflow。decl は
// planned_change（空なら無し）。
func reimageIntent(decl string) string {
	intent := `{
  "resources": [
    { "provider": "fake", "id": "n1", "scope": { "rack": "r1" }, "fields": { "name": "n1", "size": "2" } }
  ],
  "workflows": [
    { "workflow": "rolling",
      "trigger": "reimage",
      "target": { "provider": "fake", "scope": { "rack": "r1" } },
      "stages": [ { "name": "s1", "targets": ["n1"] } ],
      "steps": [ { "apply": { "active": "false" } }, { "apply": { "active": "true" } } ] }
  ]`
	if decl != "" {
		intent += `,
  "planned_change": [` + decl + `]`
	}
	return intent + "\n}"
}

// [AC-209-2] store が無いとき、開いていない trigger: reimage の対象は収束させる。
func TestWorkflowDone_ReimageNotOpenConverges(t *testing.T) {
	r := newWFRig(t, reimageIntent(""))
	_, stderr, code := runReconcileCombined(t, r.intentDir, r.fp.StatePath())
	if code != 0 || r.field("n1", "size") != "2" {
		t.Fatalf("[AC-209-2] without a planned_change the reimage target n1 must converge (exit %d, size %s):\n%s", code, r.field("n1", "size"), stderr)
	}

	// 窓の中の宣言があれば、入れ直しに任せて触らない（承認は見ない＝持つ側）。
	r.setIntent(reimageIntent(reimageDecl(pcWindow(time.Now().Add(-time.Hour), time.Now().Add(6*time.Hour)), 1, "alice")))
	r.seed("n1", map[string]any{"name": "n1", "size": 1})
	_, stderr, _ = runReconcileCombined(t, r.intentDir, r.fp.StatePath())
	if r.field("n1", "size") != "1" {
		t.Fatalf("[AC-209-2] reconcile applied n1 while a planned_change kind: reimage covering it is open")
	}
	if !strings.Contains(stderr, "kind=reimage") || !strings.Contains(stderr, "left to the reimage") {
		t.Fatalf("[AC-209-2] reconcile must say n1 is left to the reimage:\n%s", stderr)
	}
}

// [AC-209-3] store が無いとき、done_when の満たされた対象は収束させ、満たされない
// ものは触らない。plan・status は理由を出す。
func TestWorkflowDone_DoneWhenWithoutStore(t *testing.T) {
	r := newWFRig(t, doneIntent(doneWhenVersion2, ""))

	// 満たされない（版が 1）: 触らず、理由を出す。
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "true", "version": "1"})
	_, stderr, _ := runReconcileCombined(t, r.intentDir, r.fp.StatePath())
	if r.field("n1", "size") != "1" {
		t.Fatal("[AC-209-3] reconcile applied n1 although its done_when is not observed")
	}
	if !strings.Contains(stderr, "done_when map[version:2] is not observed on fake//n1") {
		t.Fatalf("[AC-209-3] reconcile must say done_when is not satisfied:\n%s", stderr)
	}
	for name, run := range map[string]func() (string, string, int){
		"plan":   func() (string, string, int) { return runPlan(t, r.intentDir, r.fp.StatePath()) },
		"status": func() (string, string, int) { return runStatusCmd(t, r.intentDir, r.fp.StatePath()) },
	} {
		out, _, _ := run()
		if !strings.Contains(out, "WORKFLOW fake//n1") || !strings.Contains(out, "progress unknown (no --workflow-store, and done_when map[version:2] is not observed") {
			t.Fatalf("[AC-209-3] %s must show why n1 is left to its Workflow:\n%s", name, out)
		}
	}

	// 満たされた（版が 2）: 済んだと見て、ふつうに収束させる。
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "true", "version": "2"})
	stdout, stderr, code := runReconcileCombined(t, r.intentDir, r.fp.StatePath())
	if code != 0 || r.field("n1", "size") != "2" {
		t.Fatalf("[AC-209-3] reconcile did not converge n1 whose done_when is observed (exit %d):\n%s\n%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "target of workflow rolling") {
		t.Fatalf("[AC-209-3] n1 was held although its done_when is observed:\n%s", stderr)
	}
	if _, err := os.Stat(r.storePath); !os.IsNotExist(err) {
		t.Fatal("[AC-209-3] a workflow store was written without --workflow-store")
	}

	// store の無い常駐も同じに決める（進めないが、done_when の満たされた対象は保つ）。
	r.setN1(map[string]any{"name": "n1", "size": 7, "active": "true", "version": "2"})
	r.d = r.newDaemon(false)
	res := r.cycle()
	if len(res.WorkflowHeld) != 0 || r.field("n1", "size") != "2" {
		t.Fatalf("[AC-209-3] the daemon without a store held n1 (WorkflowHeld %v, size %s)", res.WorkflowHeld, r.field("n1", "size"))
	}
}

// [AC-209-4] store を消した常駐は、done_when の満たされた Workflow を起こし直さない。
func TestWorkflowDone_LostStoreNotRestarted(t *testing.T) {
	r := newWFRig(t, doneIntent(doneWhenVersion2, ""))
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "true", "ready": "true"})
	r.cycleUntil(10, func(run workflow.Run) bool { return run.Phase == workflow.PhaseDone })
	first, _ := r.run()

	// S2 を失う。
	if err := os.Remove(r.storePath); err != nil {
		t.Fatal(err)
	}
	r.d = r.newDaemon(true)
	res := r.cycle()
	if hasEvent(res, workflow.EventStarted, "") {
		t.Fatalf("[AC-209-4] the daemon restarted the Workflow after losing its store: %v", res.WorkflowEvents)
	}
	if !hasEvent(res, workflow.EventCompleted, "recorded as done without starting a run") {
		t.Fatalf("[AC-209-4] expected the Workflow recorded as done by done_when, got %v", res.WorkflowEvents)
	}
	run, ok := r.run()
	if !ok || run.Phase != workflow.PhaseDone || run.RunID == first.RunID || !strings.Contains(run.RunID, "-observed-") ||
		!strings.Contains(run.Reason, "done_when map[version:2] observed") {
		t.Fatalf("[AC-209-4] run after the lost store = %+v (%v), want a done record written from done_when", run, ok)
	}
	if len(res.WorkflowHeld) != 0 {
		t.Fatalf("[AC-209-4] WorkflowHeld = %v, want none (the Workflow is done)", res.WorkflowHeld)
	}
	// 次の周も起こさない（done の記録がある）。
	if res := r.cycle(); hasEvent(res, workflow.EventStarted, "") || r.field("n1", "active") != "true" {
		t.Fatalf("[AC-209-4] the Workflow ran again: %v", res.WorkflowEvents)
	}

	// done_when が満たされなければ、今どおり起こす（導入がまだ済んでいない）。
	if err := os.Remove(r.storePath); err != nil {
		t.Fatal(err)
	}
	r.setN1(map[string]any{"name": "n1", "size": 2, "active": "true", "version": "1"})
	r.d = r.newDaemon(true)
	if res := r.cycle(); !hasEvent(res, workflow.EventStarted, "") {
		t.Fatalf("[AC-209-4] the daemon did not start the Workflow whose done_when is not observed: %v", res.WorkflowEvents)
	}
}

// [AC-209-5] 2 つの常駐が同じ store の進めるロックを取り合うと、片方だけが進める。
func TestWorkflowDone_AdvanceLockOneAdvances(t *testing.T) {
	r := newWFRig(t, doneIntent("", ""))
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "true"})
	d1 := r.d
	res := r.cycle()
	if !hasEvent(res, workflow.EventStarted, "") {
		t.Fatalf("[AC-209-5] the first daemon did not start the Workflow: %v", res.WorkflowEvents)
	}
	run1, _ := r.run()
	if run1.Phase != workflow.PhaseRunning || run1.Step != 2 {
		t.Fatalf("[AC-209-5] run = %+v, want running at the ready wait", run1)
	}

	// 2 つ目の常駐（同じ store）。1 つ目はロックを持ったまま。
	r.d = nil
	d2 := r.newDaemon(true)
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "false", "version": "2", "ready": "true"})
	res2, err := d2.RunCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d2.Wait(5 * time.Second)
	if !hasEvent(res2, workflow.EventWaiting, "another process holds the advance lock") {
		t.Fatalf("[AC-209-5] the second daemon must not advance while the first holds the lock: %v", res2.WorkflowEvents)
	}
	if run, _ := r.run(); run.Version != run1.Version || run.Step != run1.Step {
		t.Fatalf("[AC-209-5] the second daemon advanced the run: %+v (was %+v)", run, run1)
	}
	if !containsStr(res2.WorkflowHeld, "n1") || r.field("n1", "active") != "false" {
		t.Fatalf("[AC-209-5] the second daemon must hold n1 (WorkflowHeld %v, active %s)", res2.WorkflowHeld, r.field("n1", "active"))
	}

	// 1 つ目が終われば、2 つ目が進める。
	d1.Shutdown(time.Second)
	r.d = d2
	r.cycleUntil(5, func(run workflow.Run) bool { return run.Phase == workflow.PhaseDone })
	if r.field("n1", "active") != "true" {
		t.Fatalf("[AC-209-5] the second daemon did not finish the Workflow")
	}
}

// [AC-209-6] halt の対象は触らず、理由を出す。
func TestWorkflowDone_HaltedHeldWithReason(t *testing.T) {
	r := newWFRig(t, doneIntent(doneWhenVersion2, `{ "wait": { "fields": { "checked": "true" }, "timeout": "1m" } }`))
	r.setN1(map[string]any{"name": "n1", "size": 1, "active": "true", "version": "1", "ready": "true"})
	r.cycle()
	r.clock.Set(r.clock.Now().Add(2 * time.Minute))
	res := r.cycleUntil(5, func(run workflow.Run) bool { return run.Phase == workflow.PhaseHalted })
	if !containsStr(res.WorkflowHeld, "n1") {
		t.Fatalf("[AC-209-6] WorkflowHeld = %v, want n1", res.WorkflowHeld)
	}
	// 終わりの姿（version=2）は観測で満たされていても、halt は人が片づけるまで保つ。
	if r.field("n1", "version") != "2" {
		t.Fatalf("[AC-209-6] fixture: version %s, want 2", r.field("n1", "version"))
	}
	if r.field("n1", "size") != "1" {
		t.Fatal("[AC-209-6] the daemon converged the halted Workflow's target n1")
	}
	// level の Event（WorkflowTargetHeld）に理由が出る。
	if !hasEvent(res, workflow.EventTargetHeld, "halted (run ") || !hasEvent(res, workflow.EventTargetHeld, "timed out") ||
		!core.EventWorkflowTargetHeld.IsLevel() {
		t.Fatalf("[AC-209-6] no WorkflowTargetHeld level Event with the halt reason: %v", res.WorkflowEvents)
	}

	store := []string{"--workflow-store", r.storePath}
	_, stderr, _ := runReconcileCombined(t, r.intentDir, r.fp.StatePath(), store...)
	if r.field("n1", "size") != "1" {
		t.Fatal("[AC-209-6] the one-shot reconcile converged the halted Workflow's target n1")
	}
	if !strings.Contains(stderr, "WorkflowTargetHeld") || !strings.Contains(stderr, "halted (run ") || !strings.Contains(stderr, "timed out") {
		t.Fatalf("[AC-209-6] reconcile must report the halt and its reason:\n%s", stderr)
	}
	for name, out := range map[string]string{
		"plan":   func() string { o, _, _ := runPlan(t, r.intentDir, r.fp.StatePath(), store...); return o }(),
		"status": func() string { o, _, _ := runStatusCmd(t, r.intentDir, r.fp.StatePath(), store...); return o }(),
	} {
		if !strings.Contains(out, "WORKFLOW fake//n1") || !strings.Contains(out, "halted (run ") || !strings.Contains(out, "timed out") {
			t.Fatalf("[AC-209-6] %s must show the halt and its reason:\n%s", name, out)
		}
	}

	// halt は人が片づけるまで保つ。cancel すると run は消え、終わりの姿（done_when）が
	// 観測で満たされているので done と書かれ、n1 は宣言どおりに戻る。
	if out, code := runSakutei(t, "workflow", "cancel", "--store", r.storePath, "rolling"); code != 0 {
		t.Fatalf("workflow cancel: exit %d\n%s", code, out)
	}
	res = r.cycle()
	if hasEvent(res, workflow.EventTargetHeld, "") || hasEvent(res, workflow.EventStarted, "") {
		t.Fatalf("[AC-209-6] after cancel: %v", res.WorkflowEvents)
	}
	r.cycle()
	if r.field("n1", "size") != "2" {
		t.Fatalf("[AC-209-6] n1 was not converged after the halted run was cancelled (done_when observed)")
	}
}
