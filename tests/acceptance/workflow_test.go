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

// workflow_test.go: 最小 Workflow（#42、§6.3 / §6.4 / §14.4）。
//
// 3 台（n1〜n3）のローリング: 各台を drain（active=false）→ 単純 Task で size=2 に
// 上げる → undrain → invariant を検証。invariant は min_active: 2。ステージ s1・s2
// の末尾に手動承認ゲート（timeout 1h）、s3 はゲートなし。常駐ループを
// reconcile.Daemon で1周ずつ回す。
//
//	AC-v1.5-workflow-1 — ステージを終えると承認ゲートで待ち、デーモンを再起動しても待ちに留まる
//	AC-v1.5-workflow-2 — invariant（min_active）が破れるステップで止まり、現実が戻れば進む
//	AC-v1.5-workflow-3 — 承認 API（POST /workflows/{id}/gates/{gate}/approve）で進み、全ステージで完了する
//	AC-v1.5-workflow-4 — 却下で halt する（以後進まない・対象は収束ループに返らない）
//	AC-v1.5-workflow-5 — ゲートの timeout で halt する
//	AC-v1.5-workflow-6 — 承認・却下・timeout が S4 監査に append-only で残る
//	AC-v1.5-workflow-7 — v1.5 で未対応の宣言（select・halt 以外の on_failure）と誤った requirements はロード時に拒否される
//	AC-v1.5-workflow-8 — workflow store が無ければ Workflow は進まず、対象は Apply されない
//	AC-v1.5-workflow-9 — plan・status は done でない Workflow の対象を「Workflow に任せる」と示し、普段の差分（UPDATE・DRIFTED）として出さない。--workflow-store があれば run の段階も出す（#89）
//	AC-101-1 — 常駐ループは Workflow の対象を「in-flight or unobservable」の skip と言わず数えない（WorkflowHeld として出す）。その下流は従来どおり待つ（#101 の試用で見つけた）
package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

// rollingIntent は 3 台の desired（ローリング後の姿: size=2・active）と Workflow。
const rollingIntent = `{
  "resources": [
    { "provider": "fake", "id": "n1", "fields": { "name": "n1", "size": "2", "active": "true" } },
    { "provider": "fake", "id": "n2", "fields": { "name": "n2", "size": "2", "active": "true" } },
    { "provider": "fake", "id": "n3", "fields": { "name": "n3", "size": "2", "active": "true" } },
    { "provider": "fake", "id": "app", "depends_on": ["n1"], "fields": { "name": "app", "size": "1" } }
  ],
  "workflows": [
    { "workflow": "rolling",
      "target": { "provider": "fake" },
      "invariant": { "min_active": 2 },
      "strategy": { "order": "one-at-a-time" },
      "stages": [
        { "name": "s1", "targets": ["n1"], "gate": { "manual_approval": "required", "timeout": "1h" } },
        { "name": "s2", "targets": ["n2"], "gate": { "manual_approval": "required", "timeout": "1h" } },
        { "name": "s3", "targets": ["n3"] }
      ],
      "steps": [
        { "apply": { "active": "false" } },
        { "task": { "size": "2" } },
        { "apply": { "active": "true" } },
        { "verify": "invariant" }
      ],
      "on_failure": { "policy": "halt" } }
  ]
}`

type wfRig struct {
	t         *testing.T
	root      string
	intentDir string
	storePath string
	auditPath string
	taskPath  string
	fp        *fakeprovider.Provider
	clock     *taskClock
	d         *reconcile.Daemon
	out       *syncBuf
}

func newWFRig(t *testing.T, intent string) *wfRig {
	t.Helper()
	root := t.TempDir()
	r := &wfRig{
		t:         t,
		root:      root,
		intentDir: filepath.Join(root, "intent"),
		storePath: filepath.Join(root, "workflows.json"),
		auditPath: filepath.Join(root, "audit.jsonl"),
		taskPath:  filepath.Join(root, "tasks.json"),
		fp:        fakeprovider.New(filepath.Join(root, "state.json")),
		// The CLI verbs (approve / reject) judge the gate deadline by the real
		// clock, so the rig starts from it: a fixed date expires the gates once
		// the calendar passes it.
		clock: &taskClock{now: time.Now().UTC().Truncate(time.Second)},
	}
	if err := os.MkdirAll(r.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.intentDir, "intent.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	initStateFile(t, filepath.Join(root, "state.json"))
	for _, n := range []string{"n1", "n2", "n3"} {
		r.seed(n, map[string]any{"name": n, "size": 1, "active": "true"})
	}
	r.seed("app", map[string]any{"name": "app", "size": 0})
	r.d = r.newDaemon(true)
	return r
}

func (r *wfRig) seed(local string, fields map[string]any) {
	r.t.Helper()
	if err := r.fp.SeedResource(local, fields); err != nil {
		r.t.Fatalf("SeedResource %s: %v", local, err)
	}
}

// newDaemon は同じ intent・現実・S2・S4 で新しいデーモンを作る（＝再起動）。
func (r *wfRig) newDaemon(store bool) *reconcile.Daemon {
	// #209: the old daemon's process "dies": its advance lock is released.
	if r.d != nil {
		r.d.Abandon()
	}
	r.out = &syncBuf{}
	cfg := reconcile.Config{
		IntentDir:    r.intentDir,
		GeneratedDir: filepath.Join(r.root, "generated"),
		Provider:     r.fp,
		Out:          r.out,
		ErrOut:       r.out,
		Now:          r.clock.Now,
		TaskStore:    task.NewFileStore(r.taskPath),
		Audit:        audit.NewLog(r.auditPath),
	}
	if store {
		cfg.WorkflowStore = workflow.NewFileStore(r.storePath)
	}
	return reconcile.NewDaemon(cfg)
}

func (r *wfRig) cycle() reconcile.CycleResult {
	r.t.Helper()
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v", err)
	}
	r.d.Wait(5 * time.Second)
	return res
}

// cycleUntil は cond が真になるまで周を回す（最大 n 周）。
func (r *wfRig) cycleUntil(n int, cond func(workflow.Run) bool) reconcile.CycleResult {
	r.t.Helper()
	var res reconcile.CycleResult
	for i := 0; i < n; i++ {
		res = r.cycle()
		if run, ok := r.run(); ok && cond(run) {
			return res
		}
	}
	run, _ := r.run()
	r.t.Fatalf("condition not reached after %d cycles; run=%+v\nlog:\n%s", n, run, r.out.String())
	return res
}

// cycleUntilLogged は cycleUntil と同じで、条件に届かなかったときに extra() の
// 文も出す。
func (r *wfRig) cycleUntilLogged(n int, cond func(workflow.Run) bool, extra func() string) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		r.cycle()
		if run, ok := r.run(); ok && cond(run) {
			return
		}
	}
	run, _ := r.run()
	r.t.Fatalf("condition not reached after %d cycles; run=%+v\nper cycle:\n%s\nlog:\n%s", n, run, extra(), r.out.String())
}

func (r *wfRig) run() (workflow.Run, bool) {
	r.t.Helper()
	run, ok, err := workflow.NewFileStore(r.storePath).Get(context.Background(), "rolling")
	if err != nil {
		r.t.Fatal(err)
	}
	return run, ok
}

func (r *wfRig) field(local, name string) string {
	r.t.Helper()
	obs, err := r.fp.Read(context.Background(), local)
	if err != nil || obs.State == nil {
		r.t.Fatalf("Read %s: %v (state %v)", local, err, obs.State)
	}
	tv, ok := obs.State[name]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", tv.Value)
}

func (r *wfRig) setActive(local, v string) {
	r.t.Helper()
	r.seed(local, map[string]any{"name": local, "size": r.field(local, "size"), "active": v})
}

// audit returns the gate and run records of the S4 log; the Apply records of
// the Workflow's apply steps (#187) are left out (audit_chain_test.go checks
// them).
func (r *wfRig) audit() []audit.Record {
	r.t.Helper()
	recs, err := audit.ReadAll(r.auditPath)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []audit.Record
	for _, rec := range recs {
		if rec.Kind != audit.KindApply && rec.Kind != audit.KindPrune {
			out = append(out, rec)
		}
	}
	return out
}

func (r *wfRig) api() *httptest.Server {
	srv := httptest.NewServer((&workflow.API{
		Store: workflow.NewFileStore(r.storePath),
		Audit: audit.NewLog(r.auditPath),
		Now:   r.clock.Now,
	}).Handler())
	r.t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, mustReadAll(t, resp)
}

func mustReadAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var m any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data, _ := json.Marshal(m)
	return string(data)
}

func hasEvent(res reconcile.CycleResult, kind workflow.EventKind, substr string) bool {
	for _, ev := range res.WorkflowEvents {
		if ev.Kind == kind && strings.Contains(ev.Message, substr) {
			return true
		}
	}
	return false
}

func awaiting(gate string) func(workflow.Run) bool {
	return func(run workflow.Run) bool {
		return run.Phase == workflow.PhaseAwaitingApproval && run.Gate == gate
	}
}

// runToFirstGate は s1 を終えてゲート s1 で待つところまで回す。
func (r *wfRig) runToFirstGate() reconcile.CycleResult {
	r.t.Helper()
	res := r.cycleUntil(10, awaiting("s1"))
	if !hasEvent(res, workflow.EventAwaiting, "gate s1") {
		r.t.Fatalf("expected a StageAwaitingApproval event for gate s1, got %v", res.WorkflowEvents)
	}
	return res
}

// [AC-v1.5-workflow-1] [AC-v1.5-workflow-3] [AC-v1.5-workflow-6]
func TestWorkflowRollingGateRestartApprove(t *testing.T) {
	r := newWFRig(t, rollingIntent)

	r.runToFirstGate()
	// s1 の n1 だけが手順を終えている（drain → Task → undrain）。
	if got := r.field("n1", "size"); got != "2" {
		t.Fatalf("n1 size = %s, want 2 (task step)", got)
	}
	if got := r.field("n1", "active"); got != "true" {
		t.Fatalf("n1 active = %s, want true (undrained)", got)
	}
	if r.field("n2", "size") != "1" || r.field("n3", "size") != "1" {
		t.Fatal("n2/n3 must not be touched before the gate s1 is approved")
	}
	runs, _, _ := r.fp.TaskCounts()
	if runs["n1"] != 1 {
		t.Fatalf("task ran %d times on n1, want 1", runs["n1"])
	}

	// 待ちに留まる: 何周回しても n2 に進まない。対象は収束ループが触らない。
	for i := 0; i < 3; i++ {
		res := r.cycle()
		if !hasEvent(res, workflow.EventWaiting, "awaiting approval at gate s1") {
			t.Fatalf("cycle %d: expected awaiting approval, got %v", i, res.WorkflowEvents)
		}
		if len(res.WorkflowHeld) != 3 {
			t.Fatalf("WorkflowHeld = %v, want n1..n3", res.WorkflowHeld)
		}
	}
	// app は n1 に依存し、n1 は Workflow が持っているので待つ。
	if got := r.field("app", "size"); got != "0" {
		t.Fatalf("app size = %s; its upstream n1 is held by the Workflow, so it must wait", got)
	}
	if r.field("n2", "size") != "1" {
		t.Fatal("n2 was touched while the gate s1 is waiting")
	}

	// デーモンを再起動しても待ちに留まる（進行位置は S2）。
	before, _ := r.run()
	r.d = r.newDaemon(true)
	res := r.cycle()
	after, _ := r.run()
	if after.Phase != workflow.PhaseAwaitingApproval || after.Gate != "s1" || after.RunID != before.RunID {
		t.Fatalf("after restart: run = %+v, want the same run awaiting s1", after)
	}
	if hasEvent(res, workflow.EventStarted, "") {
		t.Fatal("the restarted daemon started a new run instead of resuming the waiting one")
	}
	if r.field("n2", "active") != "true" || r.field("n2", "size") != "1" {
		t.Fatal("n2 was touched after the restart while the gate s1 is waiting")
	}

	// 承認 API: 承認者 ID と署名が要る。待っていないゲートは 409。
	srv := r.api()
	if code, body := post(t, srv.URL+"/workflows/rolling/gates/s1/approve", `{"signature":"sig"}`); code != http.StatusBadRequest {
		t.Fatalf("approve without approver: %d %s, want 400", code, body)
	}
	if code, body := post(t, srv.URL+"/workflows/rolling/gates/s2/approve", `{"approver":"alice","signature":"sig-a"}`); code != http.StatusConflict {
		t.Fatalf("approve of a gate not waiting: %d %s, want 409", code, body)
	}
	if code, body := post(t, srv.URL+"/workflows/rolling/gates/s1/approve", `{"approver":"alice","signature":"sig-a"}`); code != http.StatusOK {
		t.Fatalf("approve s1: %d %s", code, body)
	}
	if code, _ := post(t, srv.URL+"/workflows/rolling/gates/s1/approve", `{"approver":"bob","signature":"sig-b"}`); code != http.StatusConflict {
		t.Fatalf("second approve of s1: %d, want 409 (already decided)", code)
	}

	// 承認で次のステージへ: s2 を終えてゲート s2 で待つ。
	res = r.cycleUntil(10, awaiting("s2"))
	if r.field("n2", "size") != "2" || r.field("n2", "active") != "true" {
		t.Fatal("n2 did not go through the steps after the approval")
	}
	if r.field("n3", "size") != "1" {
		t.Fatal("n3 was touched before the gate s2 is approved")
	}

	// CLI と同じ入口（workflow.Decide）で s2 を承認 → s3（ゲートなし）→ 完了。
	if _, err := workflow.Decide(context.Background(), workflow.NewFileStore(r.storePath), audit.NewLog(r.auditPath),
		"rolling", "s2", workflow.Approval{Approver: "bob", Signature: "sig-b", Decision: workflow.DecisionApprove}, r.clock.Now()); err != nil {
		t.Fatalf("approve s2: %v", err)
	}
	r.cycleUntil(10, func(run workflow.Run) bool { return run.Phase == workflow.PhaseDone })
	for _, n := range []string{"n1", "n2", "n3"} {
		if r.field(n, "size") != "2" || r.field(n, "active") != "true" {
			t.Fatalf("%s not upgraded and active after completion", n)
		}
	}
	runs, _, _ = r.fp.TaskCounts()
	for _, n := range []string{"n1", "n2", "n3"} {
		if runs[n] != 1 {
			t.Fatalf("task ran %d times on %s, want exactly 1", runs[n], n)
		}
	}

	// 完了すると対象は収束ループに戻る（app も n1 を待たなくなる）。
	res = r.cycle()
	r.cycle()
	if len(res.WorkflowHeld) != 0 {
		t.Fatalf("WorkflowHeld after completion = %v, want none", res.WorkflowHeld)
	}
	if got := r.field("app", "size"); got != "1" {
		t.Fatalf("app size = %s after the Workflow completed, want 1 (converged)", got)
	}

	// S4: 承認 2 件が承認者 ID・署名つきで残る（拒否された API 呼び出しは残らない）。
	recs := r.audit()
	var approvals []audit.Record
	for _, rec := range recs {
		if rec.Kind == audit.KindGateApproved {
			approvals = append(approvals, rec)
		}
	}
	if len(approvals) != 2 {
		t.Fatalf("audit approvals = %+v, want 2", recs)
	}
	if approvals[0].Gate != "s1" || approvals[0].Actor != "alice" || approvals[0].Signature != "sig-a" ||
		approvals[1].Gate != "s2" || approvals[1].Actor != "bob" || approvals[1].Workflow != "rolling" {
		t.Fatalf("audit approvals = %+v", approvals)
	}
	if approvals[0].RunID == "" || approvals[0].RunID != approvals[1].RunID {
		t.Fatalf("audit records must carry the run ID: %+v", approvals)
	}
}

// [AC-v1.5-workflow-2]
func TestWorkflowInvariantBlocksStep(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()
	if err := workflowApprove(r, "s1"); err != nil {
		t.Fatal(err)
	}

	// 誰か（別の保守）が n3 を外した。n2 を drain すると active は 1 台になり
	// min_active 2 が破れる → drain のステップで止まる。
	r.setActive("n3", "false")
	for i := 0; i < 3; i++ {
		res := r.cycle()
		if !hasEvent(res, workflow.EventBlocked, "invariant min_active 2 would break") {
			t.Fatalf("cycle %d: expected InvariantBlocked, got %v", i, res.WorkflowEvents)
		}
		if r.field("n2", "active") != "true" {
			t.Fatal("n2 was drained although the invariant would break")
		}
		run, _ := r.run()
		if run.Phase != workflow.PhaseRunning || run.Stage != 1 || run.Step != 0 {
			t.Fatalf("run = %+v, want running at stage s2, step 1", run)
		}
	}
	// 再起動しても同じステップで止まっている。
	r.d = r.newDaemon(true)
	if res := r.cycle(); !hasEvent(res, workflow.EventBlocked, "would break") || r.field("n2", "active") != "true" {
		t.Fatalf("after restart: expected still blocked, got %v", res.WorkflowEvents)
	}

	// 現実が戻れば（n3 が復帰）次の周で進む。
	r.setActive("n3", "true")
	r.cycleUntil(10, awaiting("s2"))
	if r.field("n2", "size") != "2" || r.field("n2", "active") != "true" {
		t.Fatal("n2 did not go through the steps once the invariant held")
	}
}

// [AC-v1.5-workflow-2] ステップの後で invariant が破れていれば halt（on_failure: halt）。
func TestWorkflowInvariantBrokenAfterStepHalts(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	// n1 の Task の最中に n2 が外れる（Task を止めておいて、その間に外す）。
	release := r.fp.HoldTask("n1")
	if _, err := r.d.RunCycle(context.Background()); err != nil { // drain n1 → Task 起動（保留）
		t.Fatal(err)
	}
	if r.field("n1", "active") != "false" {
		t.Fatal("n1 was not drained")
	}
	r.setActive("n2", "false")
	release()
	r.d.Wait(5 * time.Second)
	// 落ちたとき（CI でまれに halt せず先へ進んだ）に、外した n2 が現実に残って
	// いたかを見分けられるように、周ごとの n1〜n3 の active を記録する。
	var seen []string
	r.cycleUntilLogged(10, func(run workflow.Run) bool {
		seen = append(seen, fmt.Sprintf("phase=%s stage=%d target=%d step=%d n1=%s n2=%s n3=%s",
			run.Phase, run.Stage, run.Target, run.Step, r.field("n1", "active"), r.field("n2", "active"), r.field("n3", "active")))
		return run.Phase == workflow.PhaseHalted
	}, func() string { return strings.Join(seen, "\n") })
	run, _ := r.run()
	if !strings.Contains(run.Reason, "invariant min_active 2") {
		t.Fatalf("halt reason = %q, want an invariant violation", run.Reason)
	}
}

func workflowApprove(r *wfRig, gate string) error {
	_, err := workflow.Decide(context.Background(), workflow.NewFileStore(r.storePath), audit.NewLog(r.auditPath),
		"rolling", gate, workflow.Approval{Approver: "alice", Signature: "sig", Decision: workflow.DecisionApprove}, r.clock.Now())
	return err
}

// [AC-v1.5-workflow-4] [AC-v1.5-workflow-6]
func TestWorkflowRejectHalts(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()

	srv := r.api()
	if code, body := post(t, srv.URL+"/workflows/rolling/gates/s1/approve",
		`{"approver":"carol","signature":"sig-c","decision":"reject"}`); code != http.StatusOK {
		t.Fatalf("reject s1: %d %s", code, body)
	}
	run, _ := r.run()
	if run.Phase != workflow.PhaseHalted {
		t.Fatalf("phase after reject = %s, want halted", run.Phase)
	}

	for i := 0; i < 3; i++ {
		res := r.cycle()
		if !hasEvent(res, workflow.EventHalted, "rejected by carol") {
			t.Fatalf("expected WorkflowHalted, got %v", res.WorkflowEvents)
		}
		// halt: 対象は収束ループに返さない（n2 は size=1 のまま）。
		if len(res.WorkflowHeld) != 3 {
			t.Fatalf("WorkflowHeld = %v, want n1..n3 (halted Workflow keeps its targets)", res.WorkflowHeld)
		}
	}
	if r.field("n2", "size") != "1" || r.field("n2", "active") != "true" {
		t.Fatal("n2 was touched after the reject")
	}
	// 却下したゲートはもう承認できない。
	if code, _ := post(t, srv.URL+"/workflows/rolling/gates/s1/approve", `{"approver":"alice","signature":"sig-a"}`); code != http.StatusConflict {
		t.Fatalf("approve after reject: %d, want 409", code)
	}

	recs := r.audit()
	if len(recs) != 1 || recs[0].Kind != audit.KindGateRejected || recs[0].Actor != "carol" || recs[0].Signature != "sig-c" {
		t.Fatalf("audit = %+v, want one rejection by carol", recs)
	}

	// cancel（人の判断）で S2 の記録を消すと、次の周で最初から改めて走る。
	if _, ok, err := workflow.NewFileStore(r.storePath).Delete(context.Background(), "rolling"); err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	res := r.cycle()
	if !hasEvent(res, workflow.EventStarted, "") {
		t.Fatalf("expected a new run after cancel, got %v", res.WorkflowEvents)
	}
}

// [AC-v1.5-workflow-5] [AC-v1.5-workflow-6]
func TestWorkflowGateTimeoutHalts(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()

	r.clock.Set(r.clock.Now().Add(59 * time.Minute))
	if res := r.cycle(); !hasEvent(res, workflow.EventWaiting, "awaiting approval") {
		t.Fatalf("before the timeout: %v", res.WorkflowEvents)
	}
	r.clock.Set(r.clock.Now().Add(2 * time.Minute))
	res := r.cycle()
	if !hasEvent(res, workflow.EventHalted, "gate s1 timed out") {
		t.Fatalf("expected a timeout halt, got %v", res.WorkflowEvents)
	}
	run, _ := r.run()
	if run.Phase != workflow.PhaseHalted {
		t.Fatalf("phase = %s, want halted", run.Phase)
	}
	// timeout 後の承認は受け付けない。
	if err := workflowApprove(r, "s1"); err == nil {
		t.Fatal("approval after the timeout was accepted")
	}
	r.cycle()
	if r.field("n2", "size") != "1" || r.field("n2", "active") != "true" {
		t.Fatal("n2 was touched after the timeout halt")
	}
	recs := r.audit()
	if len(recs) != 1 || recs[0].Kind != audit.KindGateTimedOut || recs[0].Gate != "s1" {
		t.Fatalf("audit = %+v, want one gate timeout", recs)
	}
}

// [AC-v1.5-workflow-6] S4 は append-only: 既存の行は書き換えられず、追記されるだけ。
func TestAuditLogAppendOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log := audit.NewLog(path)
	if err := log.Append(audit.Record{Kind: audit.KindGateApproved, Workflow: "w", Gate: "g1", Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.NewLog(path).Append(audit.Record{Kind: audit.KindGateRejected, Workflow: "w", Gate: "g2", Actor: "b"}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(second), string(first)) {
		t.Fatal("the audit log was rewritten instead of appended")
	}
	recs, err := audit.ReadAll(path)
	if err != nil || len(recs) != 2 || recs[0].Gate != "g1" || recs[1].Gate != "g2" {
		t.Fatalf("ReadAll = %+v, %v", recs, err)
	}
}

// [AC-v1.5-workflow-7]
func TestWorkflowLoadRejectsUnsupported(t *testing.T) {
	cases := map[string]struct{ from, to string }{
		"select": {`"strategy": { "order": "one-at-a-time" }`, `"strategy": { "select": "pool=web" }`},
		"requirements": {`"gate": { "manual_approval": "required", "timeout": "1h" } },
        { "name": "s2"`, `"gate": { "manual_approval": { "requirements": [ { "approvals": 2 } ] } } },
        { "name": "s2"`},
		"on_failure": {`"policy": "halt"`, `"policy": "continue"`},
		"min_active": {`"min_active": 2`, `"min_active": 4`},
		"step":       {`{ "verify": "invariant" }`, `{ "reboot": {} }`},
		"twice":      {`"targets": ["n3"]`, `"targets": ["n1"]`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(rollingIntent, c.from) {
				t.Fatalf("fixture does not contain %q", c.from)
			}
			dir := t.TempDir()
			bad := strings.Replace(rollingIntent, c.from, c.to, 1)
			if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loader.LoadIntent(dir); err == nil {
				t.Fatalf("intent with unsupported %s was accepted", name)
			}
		})
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(rollingIntent), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err := loader.LoadIntent(dir)
	if err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	if len(in.Workflows) != 1 || len(in.Workflows[0].Stages) != 3 || in.Workflows[0].Invariant.MinActive != 2 ||
		in.Workflows[0].Stages[0].Gate.Timeout != time.Hour || in.Workflows[0].Stages[2].Gate != nil {
		t.Fatalf("loaded workflows = %+v", in.Workflows)
	}
}

// [AC-v1.5-workflow-8]
func TestWorkflowWithoutStoreHoldsTargets(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.d = r.newDaemon(false)
	for i := 0; i < 2; i++ {
		res := r.cycle()
		if !hasEvent(res, workflow.EventError, "no workflow store") {
			t.Fatalf("expected a no-store event, got %v", res.WorkflowEvents)
		}
		if len(res.WorkflowHeld) != 3 {
			t.Fatalf("WorkflowHeld = %v, want n1..n3", res.WorkflowHeld)
		}
	}
	for _, n := range []string{"n1", "n2", "n3"} {
		if r.field(n, "size") != "1" {
			t.Fatalf("%s was applied although its Workflow cannot run", n)
		}
	}
	if _, err := os.Stat(r.storePath); !os.IsNotExist(err) {
		t.Fatal("a workflow store was written without --workflow-store")
	}
}

// [AC-v1.5-workflow-3] [AC-v1.5-workflow-4] CLI: sakutei approve / reject / workflow list / cancel。
func TestWorkflowCLI(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()

	out, code := runSakutei(t, "workflow", "list", "--store", r.storePath)
	if code != 0 || !strings.Contains(out, "awaiting_approval") || !strings.Contains(out, "s1") {
		t.Fatalf("workflow list: exit %d\n%s", code, out)
	}
	// [AC-111-8] list はゲートの承認の数（得た数/要る数）と期限を出す。
	if run, _ := r.run(); run.GateDeadline.IsZero() ||
		!strings.Contains(out, "APPROVALS") || !strings.Contains(out, "DEADLINE") ||
		!strings.Contains(out, " 0/1 ") || !strings.Contains(out, run.GateDeadline.UTC().Format(time.RFC3339)) {
		t.Fatalf("[AC-111-8] workflow list must show the gate's approvals and deadline (%s):\n%s", run.GateDeadline, out)
	}
	// 承認者 ID と署名は必須。
	if out, code := runSakutei(t, "approve", "--store", r.storePath, "--audit-log", r.auditPath,
		"--approver", "alice", "rolling", "s1"); code == 0 {
		t.Fatalf("approve without signature succeeded:\n%s", out)
	}
	out, code = runSakutei(t, "approve", "--store", r.storePath, "--audit-log", r.auditPath,
		"--approver", "alice", "--signature", "sig-a", "rolling", "s1")
	if code != 0 || !strings.Contains(out, "approved gate s1") {
		t.Fatalf("approve: exit %d\n%s", code, out)
	}
	r.cycleUntil(10, awaiting("s2"))

	out, code = runSakutei(t, "reject", "--store", r.storePath, "--audit-log", r.auditPath,
		"--approver", "bob", "--signature", "sig-b", "rolling", "s2")
	if code != 0 || !strings.Contains(out, "halted") {
		t.Fatalf("reject: exit %d\n%s", code, out)
	}
	if run, _ := r.run(); run.Phase != workflow.PhaseHalted {
		t.Fatalf("phase after CLI reject = %s, want halted", run.Phase)
	}
	recs := r.audit()
	if len(recs) != 2 || recs[0].Kind != audit.KindGateApproved || recs[1].Kind != audit.KindGateRejected || recs[1].Actor != "bob" {
		t.Fatalf("audit = %+v", recs)
	}

	out, code = runSakutei(t, "workflow", "cancel", "--store", r.storePath, "--audit-log", r.auditPath, "rolling")
	if code != 0 || !strings.Contains(out, "cancelled run") {
		t.Fatalf("workflow cancel: exit %d\n%s", code, out)
	}
	if _, ok := r.run(); ok {
		t.Fatal("the run is still in the store after cancel")
	}
	if recs := r.audit(); len(recs) != 3 || recs[2].Kind != audit.KindWorkflowCancelled {
		t.Fatalf("audit after cancel = %+v", recs)
	}
}

// [AC-v1.5-workflow-8] 1 回収束（sakutei reconcile）は Workflow の対象を Apply しない。
func TestWorkflowOneShotReconcileHoldsTargets(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	stdout, stderr, code := runReconcileFull(t, r.intentDir, r.fp.StatePath())
	if code != 0 {
		t.Fatalf("reconcile exit %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "target of workflow rolling") || !strings.Contains(stdout, "held for the daemon's Workflow") {
		t.Fatalf("reconcile must report the held Workflow targets:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	for _, n := range []string{"n1", "n2", "n3"} {
		if r.field(n, "size") != "1" {
			t.Fatalf("one-shot reconcile applied the Workflow target %s", n)
		}
	}
}

// [AC-v1.5-workflow-9] plan・status は Workflow の対象を普段の差分として出さない（#89）。
func TestWorkflowPlanStatusShowHeldTargets(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	state := r.fp.StatePath()
	targets := []string{"n1", "n2", "n3"}

	// check は plan と status の出力を確かめる: 対象は「Workflow に任せる」で段階 stage、
	// UPDATE・DRIFTED には出ない。Workflow の対象でない app は普段の差分のまま。
	check := func(label, stage string, extra ...string) {
		t.Helper()
		out, stderr, code := runPlan(t, r.intentDir, state, extra...)
		if code != 0 {
			t.Fatalf("%s: plan exit %d:\n%s", label, code, stderr)
		}
		if !strings.Contains(out, "Workflow に任せる（収束ループは触らない）: 3 target(s)") ||
			!strings.Contains(out, "~ UPDATE fake//app") {
			t.Fatalf("%s: plan output:\n%s", label, out)
		}
		for _, n := range targets {
			if strings.Contains(out, "UPDATE fake//"+n) || !strings.Contains(out, "= WORKFLOW fake//"+n) {
				t.Fatalf("%s: plan must show %s as left to its Workflow, not as a diff:\n%s", label, n, out)
			}
		}
		if strings.Count(out, "stage:    "+stage) != 3 {
			t.Fatalf("%s: plan must show the stage %q for each target:\n%s", label, stage, out)
		}

		out, stderr, code = runStatusCmd(t, r.intentDir, state, extra...)
		if code != 0 {
			t.Fatalf("%s: status exit %d:\n%s", label, code, stderr)
		}
		if !strings.Contains(out, "Workflow に任せる（収束ループは触らない）: 3 リソース") ||
			!strings.Contains(out, "DRIFTED fake//app") || !strings.Contains(out, "drifted=1 ") ||
			!strings.Contains(out, " workflow=3") {
			t.Fatalf("%s: status output:\n%s", label, out)
		}
		for _, n := range targets {
			if strings.Contains(out, "DRIFTED fake//"+n) || !strings.Contains(out, "WORKFLOW fake//"+n) {
				t.Fatalf("%s: status must show %s as left to its Workflow, not as drifted:\n%s", label, n, out)
			}
		}
		if strings.Count(out, "段階: "+stage) != 3 {
			t.Fatalf("%s: status must show the stage %q for each target:\n%s", label, stage, out)
		}
	}
	store := []string{"--workflow-store", r.storePath}

	// store を渡さなければ、進み具合が分からない理由を出す（#209: done_when も無い）。
	check("no store", "progress unknown (no --workflow-store, and no done_when to confirm the end state by observation)")
	// 未起動。plan・status は store を書かない。
	check("not started", "not started", store...)
	if _, err := os.Stat(r.storePath); !os.IsNotExist(err) {
		t.Fatal("plan/status wrote the workflow store")
	}
	// 実行中。
	r.cycle()
	if run, ok := r.run(); !ok || run.Phase != workflow.PhaseRunning {
		t.Fatalf("after one cycle run = %+v, want running", run)
	}
	check("running", "running (run ", store...)
	// 承認待ち。
	r.runToFirstGate()
	check("awaiting", "awaiting_approval (run ", store...)
	out, _, _ := runPlan(t, r.intentDir, state, store...)
	if !strings.Contains(out, "gate s1, 0 of 1 approval(s)") {
		t.Fatalf("plan must name the awaited gate:\n%s", out)
	}
	// halt。
	if out, code := runSakutei(t, "reject", "--store", r.storePath, "--audit-log", r.auditPath,
		"--approver", "bob", "--signature", "sig-b", "rolling", "s1"); code != 0 {
		t.Fatalf("reject: exit %d\n%s", code, out)
	}
	check("halted", "halted (run ", store...)
}

// [AC-101-1] Workflow の対象は skip ではない: デーモンのログに「in-flight or
// unobservable」と出さず、skipped にも数えない。depends_on で下流にある app は
// 従来どおり upstream を待って skip される。
func TestWorkflowHeldTargetsAreNotSkips(t *testing.T) {
	r := newWFRig(t, rollingIntent)
	r.runToFirstGate()
	r.out = &syncBuf{}
	r.d = r.newDaemon(true)
	res := r.cycle()
	if strings.Join(res.WorkflowHeld, ",") != "n1,n2,n3" {
		t.Fatalf("WorkflowHeld = %v, want [n1 n2 n3]", res.WorkflowHeld)
	}
	if strings.Join(res.Skipped, ",") != "app" {
		t.Fatalf("Skipped = %v, want only the downstream app", res.Skipped)
	}
	out := r.out.String()
	if strings.Contains(out, "in-flight or unobservable") {
		t.Fatalf("a Workflow target was reported as in-flight or unobservable:\n%s", out)
	}
	if !strings.Contains(out, `skip fake//app: upstream "n1"`) {
		t.Fatalf("the downstream app is not reported as waiting for n1:\n%s", out)
	}
}
