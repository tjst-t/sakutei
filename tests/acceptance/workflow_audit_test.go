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

// workflow_audit_test.go: Workflow を進めない常駐を静かにし、run と手を監査の鎖に
// 残す（Backlog #210、§6.3・§26.3）。
//
//	AC-210-1 — store を渡さない alert-only の常駐は、毎周 WorkflowError を出さない。
//	           「別の処理が進める」は level の Event で 1 回だけ出し、status に 1 行だけ出る
//	AC-210-2 — 読むだけの store（--workflow-observe-only）・alert-only の常駐は進めるロックを
//	           取らない（進める常駐が進められる）。読むだけの常駐は store を書かない
//	AC-210-3 — run の始まり・手ごと（action の op と args、待ちの結果とかかった時間）・終わり
//	           （done）が名前と run の id つきで監査に残り、手から出た resource.apply に run の
//	           id と手の番号が付き、sakutei audit verify が通る
//	AC-210-4 — 待ちの時間切れは手の timed_out と run の終わり（halted と理由）として残る
//	AC-210-5 — 人の cancel は run の終わり（cancelled）として残る
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/redfish/redfishtest"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// [AC-210-1]
func TestWorkflowAudit_AlertOnlyWithoutStoreIsQuiet(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.d.Abandon()
	dsPath := filepath.Join(r.root, "daemon-status.json")
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- reconcile.Run(ctx, reconcile.Config{
			IntentDir:        r.intentDir,
			GeneratedDir:     filepath.Join(r.root, "generated"),
			Provider:         r.fp,
			Interval:         20 * time.Millisecond,
			DaemonStatusPath: dsPath,
			Remediation:      remediation.Config{Mode: remediation.ModeAlertOnly},
			Out:              &out,
			ErrOut:           &out,
		})
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	defer stop()

	health := func() statusreport.DaemonHealth {
		h, _ := statusreport.ReadDaemonHealth(dsPath)
		return h
	}
	if !waitForCondition(10*time.Second, func() bool { return health().Cycle >= 5 }) {
		t.Fatalf("[AC-210-1] the daemon did not run 5 cycles: %+v\n%s", health(), out.String())
	}
	stop()
	log := out.String()
	if strings.Contains(log, "WorkflowError") || strings.Contains(log, "no workflow store configured") {
		t.Fatalf("[AC-210-1] a daemon without a store reported a WorkflowError:\n%s", log)
	}
	if n := strings.Count(log, "WorkflowAdvancedElsewhere"); n != 1 {
		t.Fatalf("[AC-210-1] WorkflowAdvancedElsewhere emitted %d time(s) over %d cycles, want once:\n%s", n, health().Cycle, log)
	}
	if !strings.Contains(log, "no workflow store is given to this daemon") {
		t.Fatalf("[AC-210-1] the Event does not say why:\n%s", log)
	}
	h := health()
	if !strings.Contains(h.WorkflowsElsewhere, "rolling") {
		t.Fatalf("[AC-210-1] daemon status workflows_advanced_elsewhere = %q", h.WorkflowsElsewhere)
	}

	// status（別プロセス）は 1 行だけ出す。
	stdout, stderr, code := runStatusCmd(t, r.intentDir, r.fp.StatePath(), "--daemon-status", dsPath)
	if code != 0 && code != 1 {
		t.Fatalf("[AC-210-1] status exit=%d stderr=%s", code, stderr)
	}
	if n := strings.Count(stdout, "Workflow は別の処理が進める"); n != 1 {
		t.Fatalf("[AC-210-1] status shows the line %d time(s), want 1:\n%s", n, stdout)
	}
}

// [AC-210-2]
func TestWorkflowAudit_ObserverDoesNotTakeTheLock(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.d.Abandon()
	r.d = nil
	newDaemon := func(observeOnly bool, mode remediation.Mode) *reconcile.Daemon {
		return reconcile.NewDaemon(reconcile.Config{
			IntentDir:           r.intentDir,
			GeneratedDir:        filepath.Join(r.root, "generated"),
			Provider:            r.fp,
			Out:                 r.out,
			ErrOut:              r.out,
			Now:                 r.clock.Now,
			TaskStore:           task.NewFileStore(r.taskPath),
			Audit:               audit.NewLog(r.auditPath),
			WorkflowStore:       workflow.NewFileStore(r.storePath),
			WorkflowObserveOnly: observeOnly,
			Remediation:         remediation.Config{Mode: mode},
		})
	}
	cycle := func(d *reconcile.Daemon) reconcile.CycleResult {
		t.Helper()
		res, err := d.RunCycle(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		d.Wait(5 * time.Second)
		d.EndCycle(res, nil)
		return res
	}
	observer := newDaemon(true, remediation.ModeAuto)
	alertOnly := newDaemon(false, remediation.ModeAlertOnly)
	defer observer.Abandon()
	defer alertOnly.Abandon()
	for _, d := range []*reconcile.Daemon{observer, alertOnly} {
		res := cycle(d)
		if !hasEvent(res, workflow.EventElsewhere, "") || hasEvent(res, workflow.EventError, "") {
			t.Fatalf("[AC-210-2] want WorkflowAdvancedElsewhere and no error: %v", res.WorkflowEvents)
		}
		if len(res.WorkflowHeld) != 3 {
			t.Fatalf("[AC-210-2] WorkflowHeld = %v, want n1..n3", res.WorkflowHeld)
		}
	}
	if !hasEvent(cycle(observer), workflow.EventElsewhere, "--workflow-observe-only") {
		t.Fatalf("[AC-210-2] the observer does not say it reads the store only")
	}
	if _, err := os.Stat(r.storePath); err == nil {
		if run, ok := r.run(); ok {
			t.Fatalf("[AC-210-2] a daemon that does not advance wrote a run: %+v", run)
		}
	}

	// 進める常駐はロックを取れて進める。
	advancer := newDaemon(false, remediation.ModeAuto)
	defer advancer.Abandon()
	res := cycle(advancer)
	if hasEvent(res, workflow.EventElsewhere, "") || !hasEvent(res, workflow.EventStarted, "") {
		t.Fatalf("[AC-210-2] the advancing daemon did not start the run: %v", res.WorkflowEvents)
	}
}

// workflowRecords は監査の Workflow の記録（run の始まり・手・終わり）と、Workflow の
// 手から出た resource.apply を返す。
func workflowRecords(t *testing.T, path string) (wf, applies []audit.Record) {
	t.Helper()
	recs, err := audit.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		switch rec.Kind {
		case audit.KindWorkflowRunStart, audit.KindWorkflowStep, audit.KindWorkflowRunEnd:
			wf = append(wf, rec)
		case audit.KindApply:
			if rec.RunID != "" {
				applies = append(applies, rec)
			}
		}
	}
	return wf, applies
}

func mustVerifyAudit(t *testing.T, path string) {
	t.Helper()
	if out, code := runSakutei(t, "audit", "verify", "--audit-log", path); code != 0 {
		t.Fatalf("sakutei audit verify: exit %d\n%s", code, out)
	}
}

// [AC-210-3]
func TestWorkflowAudit_RunStepsAndEnd(t *testing.T) {
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, osInstallOnceWorkflow())
	bmc := r.bmcs["node1"]
	r.mustCycle()
	r.clk.Advance(3 * time.Minute)
	r.mustCycle()
	bmc.SetPowerState("Off")
	r.mustCycle()
	run := r.run()
	if run.Phase != workflow.PhaseDone {
		t.Fatalf("run = %+v, want done\n%s", run, r.out.String())
	}

	wf, applies := workflowRecords(t, r.auditPath)
	if len(wf) != 9 {
		t.Fatalf("[AC-210-3] workflow records = %d, want start + 7 steps + end: %+v", len(wf), wf)
	}
	for _, rec := range wf {
		if rec.Workflow != "os-install" || rec.RunID != run.RunID {
			t.Fatalf("[AC-210-3] record without the workflow name and run id: %+v", rec)
		}
	}
	if wf[0].Kind != audit.KindWorkflowRunStart || !strings.Contains(wf[0].Detail, "declared") {
		t.Fatalf("[AC-210-3] first record = %+v, want workflow.run.start with its reason", wf[0])
	}
	want := []struct {
		kind, result, action string
		waited               bool
	}{
		{"apply", audit.ResultOK, "", false},
		{"require", audit.ResultSatisfied, "", true},
		{"action", audit.ResultSent, "boot_once", false},
		{"action", audit.ResultSent, "reset", false},
		{"wait", audit.ResultSatisfied, "", true},
		{"apply", audit.ResultOK, "", false},
		{"apply", audit.ResultOK, "", false},
	}
	for i, w := range want {
		rec := wf[i+1]
		if rec.Kind != audit.KindWorkflowStep || rec.Step != i+1 || rec.StepKind != w.kind || rec.Result != w.result ||
			rec.Action != w.action || rec.Stage != "install" || rec.Target == "" || (w.waited && rec.Elapsed == "") {
			t.Fatalf("[AC-210-3] step %d = %+v, want %+v", i+1, rec, w)
		}
	}
	if wf[3].Args["target"] != "Cd" {
		t.Fatalf("[AC-210-3] boot_once args = %v, want target=Cd", wf[3].Args)
	}
	if wf[5].Elapsed != "3m0s" {
		t.Fatalf("[AC-210-3] the power Off wait elapsed = %q, want 3m0s", wf[5].Elapsed)
	}
	end := wf[8]
	if end.Kind != audit.KindWorkflowRunEnd || end.Result != audit.ResultDone {
		t.Fatalf("[AC-210-3] last record = %+v, want workflow.run.end done", end)
	}

	// 手から出た resource.apply に run の id と手の番号（1・6・7 の apply の手）。
	steps := map[int]bool{}
	for _, a := range applies {
		if a.Workflow != "os-install" || a.RunID != run.RunID || a.Stage != "install" || a.Cause != string(core.CauseTagA) {
			t.Fatalf("[AC-210-3] resource.apply from a step = %+v", a)
		}
		steps[a.Step] = true
	}
	if !steps[1] || !steps[6] || !steps[7] || len(steps) != 3 {
		t.Fatalf("[AC-210-3] resource.apply steps = %v, want 1, 6 and 7", steps)
	}
	mustVerifyAudit(t, r.auditPath)
}

// [AC-210-4]
func TestWorkflowAudit_WaitTimeoutHalts(t *testing.T) {
	r := osInstallOnceRig(t, redfishtest.Options{PowerState: "Off"}, budget.Config{}, osInstallOnceWorkflow())
	r.mustCycle()
	r.clk.Advance(2 * time.Hour)
	r.mustCycle()
	run := r.run()
	if run.Phase != workflow.PhaseHalted {
		t.Fatalf("run = %+v, want halted", run)
	}
	r.clk.Advance(time.Minute)
	r.mustCycle() // halt したまま: 終わりの記録は 1 回だけ

	wf, _ := workflowRecords(t, r.auditPath)
	var step, ends []audit.Record
	for _, rec := range wf {
		switch {
		case rec.Kind == audit.KindWorkflowStep && rec.Step == 5:
			step = append(step, rec)
		case rec.Kind == audit.KindWorkflowRunEnd:
			ends = append(ends, rec)
		}
	}
	if len(step) != 1 || step[0].Result != audit.ResultTimedOut || step[0].StepKind != "wait" || step[0].Elapsed != "2h0m0s" {
		t.Fatalf("[AC-210-4] wait step records = %+v, want one timed_out after 2h0m0s", step)
	}
	if len(ends) != 1 || ends[0].Result != audit.ResultHalted || !strings.Contains(ends[0].Detail, "timed out") || ends[0].RunID != run.RunID {
		t.Fatalf("[AC-210-4] run end records = %+v, want one halted with the reason", ends)
	}
	mustVerifyAudit(t, r.auditPath)
}

// [AC-210-5]
func TestWorkflowAudit_CancelEndsTheRun(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()
	run, _ := r.run()
	if out, code := runSakutei(t, "workflow", "cancel", "--store", r.storePath, "--audit-log", r.auditPath, "rolling"); code != 0 {
		t.Fatalf("cancel: exit %d\n%s", code, out)
	}
	wf, _ := workflowRecords(t, r.auditPath)
	last := wf[len(wf)-1]
	if last.Kind != audit.KindWorkflowRunEnd || last.Result != audit.ResultCancelled || last.RunID != run.RunID || last.Workflow != "rolling" {
		t.Fatalf("[AC-210-5] last workflow record = %+v, want run.end cancelled of %s", last, run.RunID)
	}
	mustVerifyAudit(t, r.auditPath)
}
