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

// disruptive_update_test.go — #152: 全体を止める型の更新（Workflow の
// disruption、§6.3・§14.2・§22.3）。止める対象はゲートウェイ gw1（fake）、
// 止めている間に使えない下流は fake/app、上流の積もりは metrics-probe の
// orders（キューの長さ、1000 未満が上限内）。デーモンの経路（RunCycle と
// EndCycle）で、知らせは webhook・Alertmanager（httptest）と記録の sink に届く。
//
//	[AC-152-1] 時間帯（disruption.window）の外では起こさない: 待ちの理由を出し、
//	           ゲートも知らせも出さず、対象に触らない。見積もりが時間帯の終わりまでに
//	           収まらないときも始めない。remediation が alert-only の間も進まない。
//	[AC-152-2] 時間帯の中でも、上流の積もりが上限を超えている間は止めずに待つ
//	           （観測値を待ちの理由に出す）。
//	[AC-152-3] 下がれば止める前の承認ゲートが開き、見積もり・影響を受ける下流・
//	           いまの上流の観測値・時間帯が Run.GateNotice・Reason と Event に出る。
//	           承認までは止めない。
//	[AC-152-4] 承認の後、上流が再び上限を超えていれば止めずに待つ。下がれば
//	           Disruption（firing、warning）を通知の口へ出し、その周は止めない。
//	           次の周から手順（止める）に入る。止めている間は firing を出し直さない。
//	           止め始めて見積もりを過ぎても終わらなければ overrun で stuck（critical）。
//	[AC-152-5] 対象の手順を終えた周に Disruption は resolved になり、webhook・
//	           Alertmanager にも resolved が届く。
//	[AC-152-6] 上流が max_wait を過ぎても下がらなければ、止めずに halt して知らせる。
//	[AC-152-8] 知らせる時点で notify_before と見積もりが時間帯の終わりまでに収まら
//	           なければ知らせない（03:35 に知らせて 03:45 に止めると 04:05 まで掛かる）。
//	           知らせた後、止める直前に時間帯（見積もりが収まること）と上流をもう一度
//	           確かめ、外れていれば止めずに知らせを取り下げる（Disruption は resolved）。
//	           デーモンが落ちている間に時間帯が閉じたときも、戻った周に止めない。
//	           上流が戻れば改めて知らせ、notify_before の後に止める。
//	[AC-152-7] 誤った書き方（estimate・affects の欠落、upstream に max_wait なし、
//	           未知のキー、壊れた window）はロード時に拒否される。
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/metricsprobe"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const disruptGate = "s1-disruption-1"

// disruptIntent は gw1 を止めて更新する Workflow（2026-10-28 は水曜）。
func disruptIntent(extra string) string {
	return disruptIntentWith("", extra)
}

// disruptIntentWith は disruption に opt（`"key": value,` の並び）を足した intent。
func disruptIntentWith(opt, extra string) string {
	return `{"resources": [
    { "provider": "fake", "id": "gw1", "fields": { "name": "gw1", "active": "true", "version": "2" } },
    { "provider": "fake", "id": "app", "fields": { "name": "app", "size": 0 } }
  ],
  ` + extra + `
  "workflows": [{ "workflow": "gw-update",
      "target": { "provider": "fake" },
      "disruption": {
        ` + opt + `
        "estimate": "20m",
        "affects": ["fake/app"],
        "window": "Wed 02:00-04:00 UTC",
        "upstream": [{ "resource": "metrics-probe/orders", "fields": { "ready.below_limit": "true" } }],
        "max_wait": "1h",
        "gate": { "timeout": "4h" }
      },
      "stages": [{ "name": "s1", "targets": ["gw1"] }],
      "steps": [
        { "apply": { "active": "false" } },
        { "wait": { "fields": { "version": "2" }, "timeout": "1h" } },
        { "apply": { "active": "true" } }
      ] }]}`
}

type disruptRig struct {
	*probeRig
	rec       *recordingSink
	wh, am    *notifyReceiver
	auditPath string
	cfg       reconcile.Config
}

func newDisruptRig(t *testing.T, intent string) *disruptRig {
	t.Helper()
	r := &disruptRig{probeRig: newProbeRig(t, intent), rec: &recordingSink{}, wh: &notifyReceiver{}, am: &notifyReceiver{}}
	r.auditPath = filepath.Join(r.root, "audit.jsonl")
	if err := r.fp.SeedResource("gw1", map[string]any{"name": "gw1", "active": "true", "version": "1"}); err != nil {
		t.Fatal(err)
	}
	r.clock.Set(time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC))
	r.cfg = reconcile.Config{
		IntentDir:    r.intentDir,
		GeneratedDir: filepath.Join(r.root, "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case "fake":
				return r.fp, nil
			case metricsprobe.Name:
				return r.mp, nil
			}
			return nil, os.ErrNotExist
		},
		Out:           r.out,
		ErrOut:        r.out,
		Now:           r.clock.Now,
		WorkflowStore: workflow.NewFileStore(r.storePath),
		Audit:         audit.NewLog(r.auditPath),
	}
	r.d = reconcile.NewDaemon(r.cfg)
	return r
}

// start は常駐（reconcile.Run）を回す。周の知らせは webhook・Alertmanager と
// 記録の sink に届く。
func (r *disruptRig) start() {
	r.t.Helper()
	cfg := r.cfg
	cfg.Interval = 20 * time.Millisecond
	cfg.EventSinks = append(notifySinks(r.t, r.wh, r.am, r.out), r.rec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconcile.Run(ctx, cfg) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	r.t.Cleanup(stop)
}

var cycleLine = regexp.MustCompile(`sakutei: cycle (\d+):`)

// cycle は常駐が回った周の数。
func (r *disruptRig) cycleNum() int {
	n := 0
	for _, m := range cycleLine.FindAllStringSubmatch(r.out.String(), -1) {
		if c, _ := strconv.Atoi(m[1]); c > n {
			n = c
		}
	}
	return n
}

// cycles は常駐がさらに n 周回るのを待つ。
func (r *disruptRig) cycles(n int) {
	r.t.Helper()
	want := r.cycleNum() + n
	r.waitFor(fmt.Sprintf("cycle %d", want), func() bool { return r.cycleNum() >= want })
}

func (r *disruptRig) waitFor(what string, ok func() bool) {
	r.t.Helper()
	end := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(end) {
			r.t.Fatalf("timed out waiting for %s\n%s", what, r.out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// step は 1 周回す（RunCycle。周の知らせは CycleResult で見る）。
func (r *disruptRig) step() reconcile.CycleResult {
	r.t.Helper()
	res, err := r.d.RunCycle(context.Background())
	if err != nil {
		r.t.Fatalf("RunCycle: %v\n%s", err, r.out.String())
	}
	r.d.Wait(5 * time.Second)
	r.d.EndCycle(res, err)
	return res
}

func (r *disruptRig) wfRun() workflow.Run {
	r.t.Helper()
	run, _, err := workflow.NewFileStore(r.storePath).Get(context.Background(), "gw-update")
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

func (r *disruptRig) approve() {
	r.t.Helper()
	if _, err := workflow.Decide(context.Background(), workflow.NewFileStore(r.storePath), audit.NewLog(r.auditPath),
		"gw-update", disruptGate, workflow.Approval{Approver: "alice", Signature: "sig-alice", Decision: workflow.DecisionApprove},
		r.clock.Now()); err != nil {
		r.t.Fatalf("approve: %v", err)
	}
}

// untouched は gw1 がまだ止められていないこと。
func (r *disruptRig) untouched(ac, when string) {
	r.t.Helper()
	if got := r.field("gw1", "active"); got != "true" {
		r.t.Fatalf("%s %s: gw1 was stopped (active=%s)\n%s", ac, when, got, r.out.String())
	}
}

func (r *disruptRig) disruptionEvents(state core.EventState) []core.Event {
	var out []core.Event
	for _, ev := range r.rec.ofKind(core.EventDisruption) {
		if ev.State == state {
			out = append(out, ev)
		}
	}
	return out
}

func (r *disruptRig) reasonHas(sub string) func() bool {
	return func() bool { return strings.Contains(r.wfRun().Reason, sub) }
}

func TestDisruptiveUpdate_FlowsThroughWindowUpstreamApprovalAndNotice(t *testing.T) {
	r := newDisruptRig(t, disruptIntentWith(`"notify_before": "10m",`, ""))
	r.q.set("10", false)
	r.start()

	// [AC-152-1] 01:00 は時間帯（水 02:00-04:00 UTC）の外。
	r.waitFor("waiting outside the window", r.reasonHas("outside the disruption window"))
	r.cycles(3)
	if run := r.wfRun(); run.Phase != workflow.PhaseRunning || run.Step != 0 || run.Gate != "" {
		t.Fatalf("[AC-152-1] run = %+v, want running before the first step with no gate", run)
	}
	r.untouched("[AC-152-1]", "outside the window")
	if n := len(r.rec.ofKind(core.EventDisruption)); n != 0 {
		t.Fatalf("[AC-152-1] %d Disruption event(s) outside the window", n)
	}

	// [AC-152-2] 02:00: 時間帯の中だが上流の積もりが上限を超えている（1500）。
	r.q.set("1500", false)
	r.clock.Set(time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC))
	r.waitFor("waiting for the upstream", r.reasonHas("upstream backlog over the limit"))
	r.cycles(3)
	if run := r.wfRun(); run.Phase != workflow.PhaseRunning || run.Gate != "" || !strings.Contains(run.Reason, "value=1500") {
		t.Fatalf("[AC-152-2] run = %+v, want waiting without a gate, with the observed value", run)
	}
	r.untouched("[AC-152-2]", "while the upstream backlog is over the limit")

	// [AC-152-3] 下がれば止める前の承認ゲートが開き、見積もり・下流・上流が出る。
	r.q.set("12", false)
	r.waitFor("the disruption gate", func() bool { return r.wfRun().Phase == workflow.PhaseAwaitingApproval })
	run := r.wfRun()
	n := run.GateNotice
	if run.Gate != disruptGate || n["estimate"] != "20m0s" || n["affects"] != "fake/app" || !strings.Contains(n["upstream"], "value=12") ||
		!strings.Contains(n["upstream"], "metrics-probe/orders") || n["window"] != "Wed 02:00-04:00 UTC" {
		t.Fatalf("[AC-152-3] run = %+v, gate notice = %v", run, n)
	}
	if !strings.Contains(run.Reason, "estimated 20m0s") || !strings.Contains(run.Reason, "affects fake/app") {
		t.Fatalf("[AC-152-3] run reason = %q, want the estimate and the downstream", run.Reason)
	}
	r.waitFor("the gate event at the sink", func() bool { return len(r.rec.ofKind(core.EventStageAwaitingApproval)) > 0 })
	g := r.rec.ofKind(core.EventStageAwaitingApproval)[0]
	if !strings.Contains(g.Message, "estimated 20m0s") || !strings.Contains(g.Message, "affects fake/app") ||
		!strings.Contains(g.Message, "value=12") || !strings.Contains(g.Message, "/gates/"+disruptGate+"/approve") ||
		g.Attrs["estimate"] != "20m0s" || g.Attrs["affects"] != "fake/app" || g.Resource.Local != "gw1" {
		t.Fatalf("[AC-152-3] gate event = %+v", g)
	}
	r.cycles(3)
	r.untouched("[AC-152-3]", "before approval")
	if len(r.rec.ofKind(core.EventDisruption)) != 0 {
		t.Fatal("[AC-152-3] Disruption notified before approval")
	}

	// [AC-152-4] 承認の後、上流が再び上がっていれば知らせずに待つ。
	r.q.set("1200", false)
	r.approve()
	r.waitFor("waiting for the upstream after approval", func() bool {
		run := r.wfRun()
		return run.Disrupt != nil && run.Disrupt.Approved && strings.Contains(run.Reason, "upstream backlog over the limit")
	})
	r.cycles(3)
	if len(r.rec.ofKind(core.EventDisruption)) != 0 {
		t.Fatal("[AC-152-4] Disruption notified while the upstream backlog is over the limit")
	}
	r.untouched("[AC-152-4]", "after approval with a high backlog")

	// 下がれば知らせる。notify_before（10m）の間は止めない（時計は進めない）。
	r.q.set("3", false)
	r.waitFor("Disruption firing", func() bool { return len(r.disruptionEvents(core.EventFiring)) > 0 })
	f := r.disruptionEvents(core.EventFiring)[0]
	if f.Resource.Local != "gw1" || f.Severity != core.SeverityWarning || f.Attrs["estimate"] != "20m0s" ||
		f.Attrs["affects"] != "fake/app" || f.Attrs["workflow"] != "gw-update" {
		t.Fatalf("[AC-152-4] Disruption firing = %+v", f)
	}
	r.untouched("[AC-152-4]", "when the notice is out")
	r.waitFor("Disruption firing at the webhook and Alertmanager", func() bool {
		fa, _ := r.am.alertsOf(t, string(core.EventDisruption))
		return r.wh.hasEvent(t, string(core.EventDisruption), "firing") && len(fa) > 0
	})
	r.cycles(3)
	r.untouched("[AC-152-4]", "within notify_before")

	// notify_before を過ぎたら止める。更新を待つ間も firing は出し直さない。
	r.clock.Set(time.Date(2026, 10, 28, 2, 12, 0, 0, time.UTC))
	r.waitFor("gw1 stopped", func() bool { return r.field("gw1", "active") == "false" })
	r.cycles(3)
	if run := r.wfRun(); run.Phase != workflow.PhaseRunning || run.Step != 1 {
		t.Fatalf("[AC-152-4] run = %+v, want waiting for the update", run)
	}
	if n := len(r.disruptionEvents(core.EventFiring)); n != 1 || len(r.disruptionEvents(core.EventResolved)) != 0 {
		t.Fatalf("[AC-152-4] Disruption while stopped: firing=%d resolved=%d", n, len(r.disruptionEvents(core.EventResolved)))
	}

	// 止めた時刻（02:12）から見積もり（20m）を過ぎても終わらなければ stuck に上げる。
	r.clock.Set(time.Date(2026, 10, 28, 2, 33, 0, 0, time.UTC))
	r.waitFor("Disruption overrun", func() bool { return len(r.disruptionEvents(core.EventFiring)) > 1 })
	if o := r.disruptionEvents(core.EventFiring)[1]; o.Class != core.EventStuck || o.Severity != core.SeverityCritical ||
		o.Attrs["overrun"] != "true" || !strings.Contains(o.Message, "overrun") {
		t.Fatalf("[AC-152-4] Disruption overrun = %+v, want stuck and critical", o)
	}

	// [AC-152-5] 更新が済み、手順を終えた周に resolved。
	if err := r.fp.SeedResource("gw1", map[string]any{"name": "gw1", "active": "false", "version": "2"}); err != nil {
		t.Fatal(err)
	}
	r.waitFor("Disruption resolved", func() bool { return len(r.disruptionEvents(core.EventResolved)) > 0 })
	if run := r.wfRun(); run.Phase != workflow.PhaseDone {
		t.Fatalf("[AC-152-5] run = %+v, want done", run)
	}
	r.waitFor("gw1 back", func() bool { return r.field("gw1", "active") == "true" })
	if res := r.disruptionEvents(core.EventResolved)[0]; res.Resource.Local != "gw1" {
		t.Fatalf("[AC-152-5] Disruption resolved = %+v", res)
	}
	r.waitFor("Disruption resolved at the webhook and Alertmanager", func() bool {
		_, ra := r.am.alertsOf(t, string(core.EventDisruption))
		return r.wh.hasEvent(t, string(core.EventDisruption), "resolved") && len(ra) > 0
	})
	// 終わった後の周は知らせを出さない。
	r.cycles(3)
	if n := len(r.rec.ofKind(core.EventDisruption)); n != 3 {
		t.Fatalf("[AC-152-5] Disruption events = %d after done, want 3 (firing, overrun, resolved)", n)
	}
}

// approvedAt は disruptIntentWith(opt) の Workflow を now で承認した状態まで進めた
// RunCycle の rig（上流は 10 で上限内）。
func approvedAt(t *testing.T, opt string, now time.Time) *disruptRig {
	t.Helper()
	r := newDisruptRig(t, disruptIntentWith(opt, ""))
	r.q.set("10", false)
	r.clock.Set(now)
	r.step()
	if run := r.wfRun(); run.Phase != workflow.PhaseAwaitingApproval {
		t.Fatalf("run = %+v, want awaiting approval\n%s", run, r.out.String())
	}
	r.approve()
	return r
}

func hasDisruption(res reconcile.CycleResult) bool {
	return firstEvent(res, workflow.EventDisruption) != nil
}

func TestDisruptiveUpdate_NoticeMustLeaveRoomForLeadAndEstimate(t *testing.T) {
	// [AC-152-8] 03:30 に承認。03:35 に知らせると 03:45 から 20 分止めて 04:05 になる。
	r := approvedAt(t, `"notify_before": "10m",`, time.Date(2026, 10, 28, 3, 30, 0, 0, time.UTC))
	r.clock.Set(time.Date(2026, 10, 28, 3, 35, 0, 0, time.UTC))
	res := r.step()
	if hasDisruption(res) || !hasEvent(res, workflow.EventWaiting, "notify_before 10m0s and the estimate 20m0s does not fit") {
		t.Fatalf("[AC-152-8] events at 03:35: %+v", res.WorkflowEvents)
	}
	if run := r.wfRun(); run.Disrupt == nil || !run.Disrupt.NotifiedAt.IsZero() {
		t.Fatalf("[AC-152-8] run = %+v, want not notified", run)
	}
	r.clock.Set(time.Date(2026, 10, 28, 3, 45, 0, 0, time.UTC))
	r.step()
	r.untouched("[AC-152-8]", "when notify_before and the estimate do not fit")
}

func TestDisruptiveUpdate_RecheckBeforeStopping(t *testing.T) {
	// [AC-152-8] 03:25 に知らせる（03:35 から 20 分、03:55 に収まる）。止める前に
	// 時計が 03:50 へ進んだ（周が遅れた）: 見積もりが 04:00 までに収まらないので止めない。
	r := approvedAt(t, `"notify_before": "10m",`, time.Date(2026, 10, 28, 3, 20, 0, 0, time.UTC))
	r.clock.Set(time.Date(2026, 10, 28, 3, 25, 0, 0, time.UTC))
	if res := r.step(); !hasDisruption(res) {
		t.Fatalf("[AC-152-8] no notice at 03:25: %+v", res.WorkflowEvents)
	}
	r.clock.Set(time.Date(2026, 10, 28, 3, 50, 0, 0, time.UTC))
	res := r.step()
	if hasDisruption(res) || !hasEvent(res, workflow.EventWaiting, "notice is withdrawn") ||
		!hasEvent(res, workflow.EventWaiting, "the estimate 20m0s does not fit") {
		t.Fatalf("[AC-152-8] events at 03:50: %+v", res.WorkflowEvents)
	}
	r.untouched("[AC-152-8]", "when the estimate no longer fits at the stop")
	run := r.wfRun()
	if run.Disrupt == nil || !run.Disrupt.NotifiedAt.IsZero() || !run.Disrupt.Approved || !run.Disrupt.StartedAt.IsZero() {
		t.Fatalf("[AC-152-8] run.Disrupt = %+v, want withdrawn and still approved", run.Disrupt)
	}

	// デーモンが落ちている間に時間帯が閉じた: 03:25 に知らせ、戻ったのは 04:30。
	r = approvedAt(t, `"notify_before": "10m",`, time.Date(2026, 10, 28, 3, 20, 0, 0, time.UTC))
	r.clock.Set(time.Date(2026, 10, 28, 3, 25, 0, 0, time.UTC))
	if res := r.step(); !hasDisruption(res) {
		t.Fatalf("[AC-152-8] no notice at 03:25: %+v", res.WorkflowEvents)
	}
	r.clock.Set(time.Date(2026, 10, 28, 4, 30, 0, 0, time.UTC))
	res = r.step()
	if hasDisruption(res) || !hasEvent(res, workflow.EventWaiting, "outside the disruption window") ||
		!hasEvent(res, workflow.EventWaiting, "notice is withdrawn") {
		t.Fatalf("[AC-152-8] events at 04:30: %+v", res.WorkflowEvents)
	}
	r.untouched("[AC-152-8]", "after the window closed while the daemon was down")
}

func TestDisruptiveUpdate_UpstreamRecheckedBeforeStopping(t *testing.T) {
	// [AC-152-8] 02:10 に知らせ、notify_before の間に上流が溢れた。
	r := approvedAt(t, `"notify_before": "10m",`, time.Date(2026, 10, 28, 2, 5, 0, 0, time.UTC))
	r.clock.Set(time.Date(2026, 10, 28, 2, 10, 0, 0, time.UTC))
	if res := r.step(); !hasDisruption(res) {
		t.Fatalf("[AC-152-8] no notice: %+v", res.WorkflowEvents)
	}
	r.q.set("1500", false)
	r.clock.Set(time.Date(2026, 10, 28, 2, 21, 0, 0, time.UTC))
	res := r.step()
	if hasDisruption(res) || !hasEvent(res, workflow.EventWaiting, "upstream backlog over the limit") ||
		!hasEvent(res, workflow.EventWaiting, "value=1500") || !hasEvent(res, workflow.EventWaiting, "notice is withdrawn") {
		t.Fatalf("[AC-152-8] events with a high backlog at the stop: %+v", res.WorkflowEvents)
	}
	r.untouched("[AC-152-8]", "while the backlog is over the limit at the stop")

	// 下がれば改めて知らせ、notify_before の後に止める。
	r.q.set("5", false)
	r.clock.Set(time.Date(2026, 10, 28, 2, 25, 0, 0, time.UTC))
	if res := r.step(); !hasDisruption(res) {
		t.Fatalf("[AC-152-8] no new notice: %+v", res.WorkflowEvents)
	}
	r.untouched("[AC-152-8]", "in the cycle of the new notice")
	r.clock.Set(time.Date(2026, 10, 28, 2, 30, 0, 0, time.UTC))
	r.step()
	r.untouched("[AC-152-8]", "within the new notify_before")
	r.clock.Set(time.Date(2026, 10, 28, 2, 36, 0, 0, time.UTC))
	res = r.step()
	if !hasEvent(res, workflow.EventStepDone, "re-checked") {
		t.Fatalf("[AC-152-8] no re-check event at the stop: %+v", res.WorkflowEvents)
	}
	if got := r.field("gw1", "active"); got != "false" {
		t.Fatalf("[AC-152-8] gw1 active = %s, want stopped after the new notice", got)
	}

	// 溢れたまま max_wait を過ぎれば止めずに halt（知らせは取り下げたまま）。
	r = approvedAt(t, `"notify_before": "10m",`, time.Date(2026, 10, 28, 2, 5, 0, 0, time.UTC))
	r.clock.Set(time.Date(2026, 10, 28, 2, 10, 0, 0, time.UTC))
	r.step()
	r.q.set("1500", false)
	r.clock.Set(time.Date(2026, 10, 28, 2, 21, 0, 0, time.UTC))
	r.step()
	r.clock.Set(time.Date(2026, 10, 28, 3, 21, 0, 0, time.UTC))
	res = r.step()
	if run := r.wfRun(); run.Phase != workflow.PhaseHalted || !strings.Contains(run.Reason, "max_wait") {
		t.Fatalf("[AC-152-8] run = %+v, want halted by max_wait", run)
	}
	if hasDisruption(res) {
		t.Fatalf("[AC-152-8] Disruption firing for a halted run that never stopped: %+v", res.WorkflowEvents)
	}
	r.untouched("[AC-152-8]", "after the halt")
}

func TestDisruptiveUpdate_EstimateMustFitTheWindow(t *testing.T) {
	// [AC-152-1] 03:50 は時間帯の中だが、20 分の見積もりが 04:00 までに収まらない。
	r := newDisruptRig(t, disruptIntent(""))
	r.q.set("10", false)
	r.clock.Set(time.Date(2026, 10, 28, 3, 50, 0, 0, time.UTC))
	res := r.step()
	if !hasEvent(res, workflow.EventWaiting, "does not fit before the disruption window") {
		t.Fatalf("[AC-152-1] no estimate-does-not-fit event: %+v", res.WorkflowEvents)
	}
	if run := r.wfRun(); run.Gate != "" {
		t.Fatalf("[AC-152-1] run = %+v, want no gate", run)
	}
	r.untouched("[AC-152-1]", "when the estimate does not fit")
}

func TestDisruptiveUpdate_RemediationAlertOnlyDoesNotStart(t *testing.T) {
	// [AC-152-1] §14.2: 時間帯の中でも remediation が alert-only なら進まない。
	r := newDisruptRig(t, disruptIntent(`"remediation": { "default": "alert-only" },`))
	r.q.set("10", false)
	r.clock.Set(time.Date(2026, 10, 28, 2, 10, 0, 0, time.UTC))
	res := r.step()
	if !hasEvent(res, workflow.EventElsewhere, "alert-only") {
		t.Fatalf("[AC-152-1] no alert-only waiting event: %+v", res.WorkflowEvents)
	}
	if run := r.wfRun(); run.Gate != "" {
		t.Fatalf("[AC-152-1] run = %+v, want no gate under alert-only", run)
	}
	r.untouched("[AC-152-1]", "under alert-only")
}

func TestDisruptiveUpdate_MaxWaitHaltsAndNotifies(t *testing.T) {
	// [AC-152-6]
	r := newDisruptRig(t, disruptIntent(""))
	r.q.set("5000", false)
	r.clock.Set(time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC))
	r.step()
	r.advance(30 * time.Minute)
	r.step()
	if run := r.wfRun(); run.Phase != workflow.PhaseRunning {
		t.Fatalf("[AC-152-6] run = %+v, want still waiting before max_wait", run)
	}
	r.advance(30 * time.Minute)
	res := r.step()
	run := r.wfRun()
	if run.Phase != workflow.PhaseHalted || !strings.Contains(run.Reason, "max_wait 1h0m0s") || !strings.Contains(run.Reason, "value=5000") {
		t.Fatalf("[AC-152-6] run = %+v, want halted by max_wait", run)
	}
	if !hasEvent(res, workflow.EventHalted, "did not drop within max_wait") {
		t.Fatalf("[AC-152-6] no halt event: %+v", res.WorkflowEvents)
	}
	if ev := firstEvent(res, workflow.EventHalted); ev == nil || ev.Core().Severity != core.SeverityWarning {
		t.Fatalf("[AC-152-6] halt event = %+v, want a warning", ev)
	}
	r.untouched("[AC-152-6]", "after the halt")
	for _, ev := range res.WorkflowEvents {
		if ev.Kind == workflow.EventDisruption {
			t.Fatalf("[AC-152-6] Disruption for a stop that never happened: %+v", ev)
		}
	}
}

func TestDisruptiveUpdate_LoadRejectsBadDeclarations(t *testing.T) {
	// [AC-152-7]
	base := `"estimate": "20m", "affects": ["fake/app"], "window": "Wed 02:00-04:00 UTC",
        "upstream": [{ "resource": "metrics-probe/orders", "fields": { "ready.below_limit": "true" } }], "max_wait": "1h"`
	for _, c := range []struct {
		name, disruption, want string
	}{
		{"no estimate", `"affects": ["fake/app"]`, "estimate is required"},
		{"no affects", `"estimate": "20m"`, "affects is required"},
		{"bad affects", `"estimate": "20m", "affects": ["app"]`, "<provider>/<id>"},
		{"upstream without max_wait", `"estimate": "20m", "affects": ["fake/app"], "upstream": [{ "resource": "metrics-probe/orders", "fields": { "ready.below_limit": "true" } }]`, "max_wait is required"},
		{"unknown key", base + `, "maxwait": "1h"`, `unknown key "maxwait"`},
		{"bad window", `"estimate": "20m", "affects": ["fake/app"], "window": "Wed 2-4"`, "window"},
		{"unknown gate key", base + `, "gate": { "quorum": 2 }`, "gate.quorum is not supported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			intent := `{"workflows": [{ "workflow": "gw-update", "target": { "provider": "fake" },
      "disruption": {` + c.disruption + `},
      "stages": [{ "name": "s1", "targets": ["gw1"] }],
      "steps": [{ "apply": { "active": "false" } }] }]}`
			if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(intent), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loader.LoadIntent(dir)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("[AC-152-7] load error = %v, want %q", err, c.want)
			}
		})
	}
	// 正しい宣言は読める。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "intent.json"), []byte(disruptIntent("")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loader.LoadIntent(dir); err != nil {
		t.Fatalf("[AC-152-7] the valid declaration does not load: %v", err)
	}
}

func firstEvent(res reconcile.CycleResult, kind workflow.EventKind) *workflow.Event {
	for i, ev := range res.WorkflowEvents {
		if ev.Kind == kind {
			return &res.WorkflowEvents[i]
		}
	}
	return nil
}
