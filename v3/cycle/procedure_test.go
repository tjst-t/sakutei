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

package cycle_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/cycle"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

// crash は送信の途中で止まったプロセス（panic で Invoke から戻らない）。
type crash struct{}

// runCrash は 1 cycle 回し、送信の途中で止まったら true（Record のロックは defer で
// 放す。プロセスが死ねば flock はカーネルが放すのと同じ）。
func (h *harness) runCrash() (r plan.Result, crashed bool) {
	h.t.Helper()
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(crash); !ok {
				panic(v)
			}
			crashed = true
		}
	}()
	return h.run(), false
}

// upgrade は on_drift の Procedure：value がずれたら upgrade を送り、観測で閉じる。
func upgrade(steps ...core.Step) core.Procedure {
	if len(steps) == 0 {
		steps = []core.Step{{Name: "upgrade", Kind: core.StepInvoke, Op: "upgrade"}}
	}
	return core.Procedure{Name: "up", Target: core.ProcTarget{Type: "p/rec", Labels: map[string]string{"role": "sw"}},
		Trigger: core.Trigger{Kind: core.TriggerOnDrift, Fields: []string{"value"}}, Concurrency: 1,
		DoneWhen: `observe(p/rec/{target}).value == desired.value`, Steps: steps}
}

func swDecl(name, value string) core.Resource {
	r := decl(name, value)
	r.Labels = map[string]string{"role": "sw"}
	return r
}

func auditKinds(t *testing.T, h *harness) []string {
	t.Helper()
	var out []string
	files, _ := filepath.Glob(filepath.Join(h.rec.Dir(), "audit", "*.jsonl"))
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			var e record.Entry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			k := e.Kind
			if e.Result != "" && e.Result != "ok" {
				k += ":" + e.Result
			}
			out = append(out, k)
		}
		fh.Close()
	}
	return out
}

func hasKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

func runOf(r plan.Result, proc string) plan.RunStatus {
	for _, s := range r.Runs {
		if s.Procedure == proc {
			return s
		}
	}
	return plan.RunStatus{}
}

// 不変条件 16：送信の途中で止まっても二度送らない。止まった Procedure は halt し、続けるには
// 承認が要る。halt した対象は収束でも戻さない（不変条件 18、E-A9）。
func TestInterruptedSendIsNotResent(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade()}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{}, core.Fields{"value": "v2"})
	h.p.OnInvoke(func(core.ID, string, core.Fields) error { panic(crash{}) })

	if _, crashed := h.runCrash(); !crashed {
		t.Fatal("1 cycle 目は送信の途中で止まるはず")
	}
	runs, _ := record.ReadRuns(h.rec.Dir())
	if len(runs) != 1 || runs[0].Sending == nil || runs[0].Sending.Op != "upgrade" {
		t.Fatalf("送る前に送信中の記録を書く: %+v", runs)
	}

	// 2 cycle 目：記録があるので送らずに halt する。宣言の value は Procedure が受け持つので
	// 収束でも Apply しない。
	h.p.OnInvoke(nil)
	r := h.run()
	if n := len(h.p.Invocations()); n != 1 {
		t.Fatalf("二度送った: %d 回", n)
	}
	s := runOf(r, "up")
	if s.State != "halted" || s.Request == "" || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("halt して続ける承認を頼むはず: %+v exit=%d", s, cli.ExitCode(r))
	}
	if len(h.p.Writes()) != 0 {
		t.Fatalf("halt した対象に書いた: %v", h.p.Writes())
	}
	// halt した対象のほかの field のずれも戻さない（E-A9）。
	h.in.Resources[0].Fields["zone"] = "z1"
	r = h.run()
	if c := find(r, sw); c.Action != plan.ActKeep || len(h.p.Writes()) != 0 {
		t.Fatalf("halt した Procedure の対象を収束で触った: %+v %v", c, h.p.Writes())
	}
	h.in.Resources[0].Fields = core.Fields{"value": "v2"}
	if n := len(h.p.Invocations()); n != 1 {
		t.Fatalf("cycle を重ねて送った: %d 回", n)
	}
	kinds := auditKinds(t, h)
	if !hasKind(kinds, "procedure.interrupted:halted") || !hasKind(kinds, "approval.request") {
		t.Fatalf("監査: %v", kinds)
	}

	// 人が確かめて続けると承認すれば、その step をもう一度送り、観測で閉じる。
	if err := record.WriteDecision(h.rec.Dir(), s.Request, record.Decision{By: "bob", Decision: record.Approve, At: h.clock.t}); err != nil {
		t.Fatal(err)
	}
	r = h.run()
	if n := len(h.p.Invocations()); n != 2 {
		t.Fatalf("承認のあと送り直していない: %d 回", n)
	}
	if s := runOf(r, "up"); s.State != "done" {
		t.Fatalf("done_when で閉じるはず: %+v", s)
	}
	if runs, _ := record.ReadRuns(h.rec.Dir()); len(runs) != 0 {
		t.Fatalf("済んだ Procedure の進みが残る: %+v", runs)
	}
	if r := h.run(); len(r.Runs) != 0 || len(h.p.Invocations()) != 2 || cli.ExitCode(r) != 0 {
		t.Fatalf("済んだあとに起こした: %+v", r.Runs)
	}
	if _, err := record.Verify(h.rec.Dir()); err != nil {
		t.Fatal(err)
	}
}

// 送れていない（ErrNotSent）なら記録を消して次の cycle に送り直す。送れたか分から
// ないエラーは二度送らず halt する（不変条件 16、E-A2）。
func TestInvokeNotSentRetriesOtherErrorsHalt(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade()}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{}, core.Fields{"value": "v2"})
	h.p.OnInvoke(func(core.ID, string, core.Fields) error { return fmt.Errorf("接続できない: %w", core.ErrNotSent) })
	r := h.run()
	if s := runOf(r, "up"); s.State != "running" || !strings.Contains(s.Waiting, "送れていない") || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("%+v", s)
	}
	runs, _ := record.ReadRuns(h.rec.Dir())
	if len(runs) != 1 || runs[0].Sending != nil {
		t.Fatalf("送れていないなら記録を消す: %+v", runs)
	}
	h.p.OnInvoke(func(core.ID, string, core.Fields) error { return errors.New("時間切れ") })
	r = h.run()
	if s := runOf(r, "up"); s.State != "halted" || len(h.p.Invocations()) != 2 {
		t.Fatalf("送れたか分からないなら halt する: %+v", s)
	}
	h.p.OnInvoke(nil)
	h.run()
	if n := len(h.p.Invocations()); n != 2 {
		t.Fatalf("halt したあとに送った: %d", n)
	}
}

// critical の step は one-shot 実行では進めず、daemon だけが進める（§2.2、不変条件 16）。
func TestCriticalStepNeedsDaemon(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade(core.Step{Name: "apply", Kind: core.StepInvoke, Op: "upgrade", Critical: true})}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{}, core.Fields{"value": "v2"})
	r := h.run()
	s := runOf(r, "up")
	if !s.NeedsDaemon || len(h.p.Invocations()) != 0 || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("one-shot 実行が critical の step を進めた: %+v", s)
	}
	found := false
	for _, e := range r.Events {
		found = found || e.Kind == "ProcedureNeedsDaemon"
	}
	if !found {
		t.Fatalf("Event ProcedureNeedsDaemon が無い: %+v", r.Events)
	}
	h.in.Daemon = true
	r = h.run()
	if s := runOf(r, "up"); s.State != "done" || len(h.p.Invocations()) != 1 {
		t.Fatalf("daemon は進める: %+v", s)
	}
}

// 不変条件 18：「済んだ」は観測で決める。不明は済んでいない。Record を失っても、
// done_when が真なら二度走らない。
func TestDoneWhenClosesOnlyOnObservation(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade()}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{}, nil) // 送っても観測は変わらない
	r := h.run()
	if s := runOf(r, "up"); s.State != "running" || !strings.Contains(s.Waiting, "done_when") {
		t.Fatalf("step が済んでも done_when が偽なら閉じない: %+v", s)
	}
	h.p.Set(sw, core.Observation{Availability: core.Unknown, Note: "届かない"})
	if s := runOf(h.run(), "up"); s.State != "running" {
		t.Fatalf("不明は済んでいない: %+v", s)
	}
	h.p.Set(sw, present(core.Fields{"value": "v2"}))
	if s := runOf(h.run(), "up"); s.State != "done" {
		t.Fatalf("観測で閉じる: %+v", s)
	}
	if n := len(h.p.Invocations()); n != 1 {
		t.Fatalf("送った回数 %d", n)
	}

	// Record を失った（runs/ が無い）。宣言どおりなら起こさない。
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.run() // 起こす
	if err := os.RemoveAll(filepath.Join(h.rec.Dir(), "runs")); err != nil {
		t.Fatal(err)
	}
	h.p.Set(sw, present(core.Fields{"value": "v2"}))
	if r := h.run(); len(r.Runs) != 0 || len(h.p.Invocations()) != 2 {
		t.Fatalf("済んでいる対象に二度走った: %+v", r.Runs)
	}
	// 手で直したずれを持つ対象を、done_when が真のまま起こさない（観測が宣言どおり）。
	h.in.Procedures[0].DoneWhen = `observe(p/rec/{target}).value != "never"`
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	if r := h.run(); runOf(r, "up").State != "done" || len(h.p.Invocations()) != 2 {
		t.Fatalf("done_when が真なら起こさない: %+v", r.Runs)
	}
}

// manual の step は人の作業を頼み、done_when でだけ閉じる（E-A6・E-Q3）。
func TestManualStepClosesByObservation(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade(core.Step{Name: "gui", Kind: core.StepManual, Instruction: "GUI で {target} を {desired.value} に上げる"})}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	for i := 0; i < 3; i++ {
		s := runOf(h.run(), "up")
		if s.State != "running" || s.Instruction != "GUI で sw1 を v2 に上げる" {
			t.Fatalf("%d cycle 目: %+v", i, s)
		}
	}
	h.p.Set(sw, present(core.Fields{"value": "v2"}))
	if s := runOf(h.run(), "up"); s.State != "done" {
		t.Fatalf("%+v", s)
	}
}

// Procedure の step：apply・wait（when・for）・policy・gate。gate は申請者を数えない
// （不変条件 21）。
func TestProcedureSteps(t *testing.T) {
	h := newHarness(t)
	h.in.Daemon = true
	gw := rid("rec", "gw1")
	media := rid("rec", "gw1-cd")
	h.in.Resources = []core.Resource{decl("gw1", "v1"), decl("app", "a")}
	h.in.Resources[1].Labels = map[string]string{"site": "a"}
	h.p.Set(gw, present(core.Fields{"value": "v1"}))
	h.p.Set(rid("rec", "app"), present(core.Fields{"value": "a"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{}, core.Fields{"version": "2"})
	h.in.Procedures = []core.Procedure{{Name: "gw-up", Target: core.ProcTarget{Type: "p/rec", ID: "gw1"},
		Trigger: core.Trigger{Kind: core.TriggerManual}, Concurrency: 1, Steps: []core.Step{
			{Name: "media", Kind: core.StepApply, Apply: &core.Resource{ID: rid("rec", "{target}-cd"), Fields: core.Fields{"value": "iso-{desired.value}"}}},
			{Name: "inserted", Kind: core.StepWait, When: `observe(p/rec/{target}-cd).value == "iso-v1"`},
			{Name: "approve", Kind: core.StepGate, Gate: &core.Approval{N: 1, Of: []string{"carol", "bob"}, Separation: true}},
			{Name: "quiet", Kind: core.StepPolicy, Policy: &core.Policy{Select: core.Selector{Labels: map[string]string{"site": "a"}}, Reason: "工事中"}},
			{Name: "upgrade", Kind: core.StepInvoke, Op: "upgrade"},
			{Name: "settle", Kind: core.StepWait, For: 10 * time.Minute},
		}}}
	// 起こす頼み（sakutei start --as carol）。
	if _, err := record.WriteCommand(h.rec.Dir(), record.Command{Kind: "start", Procedure: "gw-up", By: "carol", At: h.clock.t}); err != nil {
		t.Fatal(err)
	}
	r := h.run()
	s := runOf(r, "gw-up")
	if s.StepName != "approve" || s.Request == "" || got(h, media) != "iso-v1" {
		t.Fatalf("apply と wait を済ませ gate で待つ: %+v media=%v", s, got(h, media))
	}
	// 申請者 carol の承認は数えない。
	record.WriteDecision(h.rec.Dir(), s.Request, record.Decision{By: "carol", Decision: record.Approve, At: h.clock.t})
	if s := runOf(h.run(), "gw-up"); s.StepName != "approve" {
		t.Fatalf("申請者の承認で進んだ: %+v", s)
	}
	record.WriteDecision(h.rec.Dir(), s.Request, record.Decision{By: "bob", Decision: record.Approve, At: h.clock.t})
	r = h.run()
	s = runOf(r, "gw-up")
	if s.StepName != "settle" || len(h.p.Invocations()) != 1 {
		t.Fatalf("gate のあと upgrade まで進む: %+v", s)
	}
	if len(r.Temporary) != 1 || !strings.Contains(r.Temporary[0].Source, "procedure") {
		t.Fatalf("Procedure の step の policy: %+v", r.Temporary)
	}
	// Procedure の step の policy は次の cycle から効く（その cycle の収束は step より先）。
	h.in.Resources[1].Fields = core.Fields{"value": "b"}
	r = h.run()
	if c := find(r, rid("rec", "app")); c.Action != plan.ActReport {
		t.Fatalf("Procedure の間は report: %+v", c)
	}
	h.clock.t = h.clock.t.Add(11 * time.Minute)
	r = h.run()
	if s := runOf(r, "gw-up"); s.State != "done" {
		t.Fatalf("for を待って済む: %+v", s)
	}
	// Procedure が済めば policy も消え、収束する。
	r = h.run()
	if c := find(r, rid("rec", "app")); c.Outcome != plan.OutcomeOK || len(r.Temporary) != 0 {
		t.Fatalf("Procedure のあとは Apply する: %+v %+v", c, r.Temporary)
	}
	kinds := auditKinds(t, h)
	for _, k := range []string{"procedure.start", "procedure.apply", "approval.approve:ignored", "approval.approve", "approval.used", "procedure.invoke", "procedure.done"} {
		if !hasKind(kinds, k) {
			t.Fatalf("監査に %s が無い: %v", k, kinds)
		}
	}
}

func got(h *harness, id core.ID) any {
	obs, _ := h.p.Observe(context.Background(), []core.ID{id})
	return obs[id].Fields["value"]
}

// 不変条件 21：Policy の承認。申請者≠承認者・異なる承認者を数える・拒否・期限切れ・
// 使った承認は使い回さない。
func TestPolicyApproval(t *testing.T) {
	h := newHarness(t)
	id := rid("rec", "web")
	h.in.Resources = []core.Resource{decl("web", "b")}
	h.p.Set(id, present(core.Fields{"value": "a"}))
	h.in.Policies = []core.Policy{{Name: "platform", Select: core.Selector{All: true}, Applicant: "carol",
		Approval: &core.Approval{N: 2, Of: []string{"alice", "bob", "carol"}, Separation: true, Timeout: time.Hour}}}
	r := h.run()
	c := find(r, id)
	if c.Action != plan.ActPending || c.Request == "" || len(h.p.Writes()) != 0 || cli.ExitCode(r) != cli.ExitRemaining {
		t.Fatalf("承認待ち: %+v", c)
	}
	reqID := c.Request
	decide := func(by, d string) {
		t.Helper()
		if err := record.WriteDecision(h.rec.Dir(), reqID, record.Decision{By: by, Decision: d, At: h.clock.t}); err != nil {
			t.Fatal(err)
		}
	}
	decide("carol", record.Approve) // 申請者
	decide("alice", record.Approve)
	if c := find(h.run(), id); c.Action != plan.ActPending || len(h.p.Writes()) != 0 {
		t.Fatalf("申請者を数えた: %+v", c)
	}
	decide("bob", record.Approve)
	r = h.run()
	if c := find(r, id); c.Outcome != plan.OutcomeOK || strings.Join(c.ApprovedBy, ",") != "alice,bob" {
		t.Fatalf("2 人で Apply する: %+v", c)
	}
	a, _, _ := record.ReadApproval(h.rec.Dir(), reqID)
	if a.Request.State != record.RequestUsed {
		t.Fatalf("使った印: %+v", a.Request)
	}
	// 同じずれがまた出たら頼み直す（使った承認を使い回さない）。
	h.p.Set(id, present(core.Fields{"value": "a"}))
	if c := find(h.run(), id); c.Action != plan.ActPending || len(h.p.Writes()) != 1 {
		t.Fatalf("使った承認で Apply した: %+v", c)
	}
	// 拒否なら halt する。
	decide("alice", record.Approve)
	decide("bob", record.Reject)
	r = h.run()
	if c := find(r, id); c.Action != plan.ActHalt || cli.ExitCode(r) != cli.ExitHalted || len(h.p.Writes()) != 1 {
		t.Fatalf("拒否なら halt する: %+v", c)
	}
	// 宣言を直せば新しい依頼。期限が切れれば頼み直す。
	h.in.Resources[0].Fields = core.Fields{"value": "c"}
	c = find(h.run(), id)
	if c.Request == reqID || c.Action != plan.ActPending {
		t.Fatalf("宣言を直したら新しい依頼: %+v", c)
	}
	reqID = c.Request
	decide("alice", record.Approve)
	h.clock.t = h.clock.t.Add(2 * time.Hour)
	decide("bob", record.Approve) // 期限の後
	if c := find(h.run(), id); c.Action != plan.ActPending || len(h.p.Writes()) != 1 {
		t.Fatalf("期限の後の承認で Apply した: %+v", c)
	}
	kinds := auditKinds(t, h)
	for _, k := range []string{"approval.request", "approval.approve", "approval.approve:ignored", "approval.used", "approval.reject", "approval.rejected:rejected", "approval.expired:expired"} {
		if !hasKind(kinds, k) {
			t.Fatalf("監査に %s が無い: %v", k, kinds)
		}
	}
}

// 不変条件 21：Record に書けなければ承認を使わない。
func TestApprovalNotUsedWhenRecordUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root は書き込みの拒否を注入できない")
	}
	h := newHarness(t)
	id := rid("rec", "web")
	h.in.Resources = []core.Resource{decl("web", "b")}
	h.p.Set(id, present(core.Fields{"value": "a"}))
	h.in.Policies = []core.Policy{{Name: "p", Select: core.Selector{All: true}, Approval: &core.Approval{N: 1, Of: []string{"bob"}}}}
	reqID := find(h.run(), id).Request
	record.WriteDecision(h.rec.Dir(), reqID, record.Decision{By: "bob", Decision: record.Approve, At: h.clock.t})
	dir := filepath.Join(h.rec.Dir(), "approvals", reqID)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	// 承認が揃っていても、依頼に印を書けなければ Apply しない。
	r := h.run()
	if c := find(r, id); c.Outcome != "" || len(h.p.Writes()) != 0 || len(r.Errors) == 0 {
		t.Fatalf("使った印を書けないのに Apply した: %+v %v", c, r.Errors)
	}
	os.Chmod(dir, 0o755)
	if c := find(h.run(), id); c.Outcome != plan.OutcomeOK {
		t.Fatalf("書ければ Apply する: %+v", c)
	}
}

// 不変条件 22：一時的な Policy は理由と期限を持ち、期限で消える。読めなければ止める
// 側に寄せる。
func TestFreezeExpires(t *testing.T) {
	h := newHarness(t)
	id := rid("rec", "web")
	h.in.Resources = []core.Resource{decl("web", "b")}
	h.p.Set(id, present(core.Fields{"value": "a"}))
	err := record.WriteTempPolicy(h.rec.Dir(), record.TempPolicy{Name: "incident", Select: core.Selector{All: true}, Mode: "report",
		Reason: "障害対応中", By: "alice", At: h.clock.t, Until: h.clock.t.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	r := h.run()
	if c := find(r, id); c.Action != plan.ActReport || len(r.Temporary) != 1 {
		t.Fatalf("freeze の間は report: %+v", c)
	}
	frozen := false
	for _, e := range r.Events {
		frozen = frozen || (e.Kind == "Frozen" && e.State == core.Firing && strings.Contains(e.Message, "障害対応中"))
	}
	if !frozen {
		t.Fatalf("Event Frozen が無い: %+v", r.Events)
	}
	h.clock.t = h.clock.t.Add(time.Hour)
	r = h.run()
	if c := find(r, id); c.Outcome != plan.OutcomeOK || len(r.Temporary) != 0 {
		t.Fatalf("期限が過ぎたら Apply する: %+v", c)
	}
	if _, ok, _ := record.ReadTempPolicy(h.rec.Dir(), "incident"); ok {
		t.Fatal("期限が過ぎた一時的な Policy が残る")
	}
	kinds := auditKinds(t, h)
	if !hasKind(kinds, "freeze.start") || !hasKind(kinds, "freeze.expire") {
		t.Fatalf("監査: %v", kinds)
	}

	// unfreeze で終える。
	h.p.Set(id, present(core.Fields{"value": "a"}))
	record.WriteTempPolicy(h.rec.Dir(), record.TempPolicy{Name: "f2", Select: core.Selector{Types: []string{"p/rec"}}, Mode: "report",
		Reason: "手作業中", By: "alice", At: h.clock.t, Until: h.clock.t.Add(24 * time.Hour)})
	if c := find(h.run(), id); c.Action != plan.ActReport {
		t.Fatalf("%+v", c)
	}
	record.EndTempPolicy(h.rec.Dir(), "f2", record.Ended{By: "bob", Reason: "終わり", At: h.clock.t})
	if c := find(h.run(), id); c.Outcome != plan.OutcomeOK {
		t.Fatalf("unfreeze のあと Apply する: %+v", c)
	}
	if !hasKind(auditKinds(t, h), "freeze.end") {
		t.Fatal("freeze.end が無い")
	}

	// 読めない一時的な Policy：全体を report に寄せる。
	h.p.Set(id, present(core.Fields{"value": "a"}))
	os.WriteFile(filepath.Join(h.rec.Dir(), "temp-policies", "broken.json"), []byte("{"), 0o644)
	r = h.run()
	if c := find(r, id); c.Action != plan.ActReport || r.ReportOnly == "" || len(h.p.Writes()) != 2 {
		t.Fatalf("読めない一時的な Policy で Apply した: %+v", c)
	}
}

// plan は Procedure を進めず、Record にも書かない（不変条件 30）。
func TestPlanDoesNotAdvanceProcedures(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade()}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{}, core.Fields{"value": "v2"})
	r := cycle.Plan(context.Background(), h.in, h.rec.Dir(), record.Marks{}, "")
	if s := runOf(r, "up"); s.State != "start" || !strings.Contains(s.Waiting, "起こす") {
		t.Fatalf("plan: %+v", s)
	}
	if len(h.p.Invocations()) != 0 {
		t.Fatal("plan が送った")
	}
	if runs, _ := record.ReadRuns(h.rec.Dir()); len(runs) != 0 {
		t.Fatal("plan が進みを書いた")
	}
	if c := find(r, sw); c.Op != plan.None || strings.Join(c.Handover, ",") != "value" {
		t.Fatalf("Procedure が受け持つ field は収束では Apply しない: %+v", c)
	}
}

// Procedure の書き込む step も budget に数える。超えれば送らず cycle を halt する（不変条件 20）。freeze
// （report）に該当する対象の Procedure は起こさない（E-J4）。
func TestProcedureRespectsBudgetAndFreeze(t *testing.T) {
	h := newHarness(t)
	sw := rid("rec", "sw1")
	h.in.Resources = []core.Resource{swDecl("sw1", "v2")}
	h.in.Procedures = []core.Procedure{upgrade()}
	h.p.Set(sw, present(core.Fields{"value": "v1"}))
	h.p.SetOp("rec", "upgrade", core.OpSpec{Destructive: true}, core.Fields{"value": "v2"})
	h.in.Policies = []core.Policy{{Name: "b", Select: core.Selector{All: true}, Budget: &core.Budget{PerCycle: core.Unlimited,
		DestructivePerCycle: 0, DestructivePerWindow: core.Unlimited, MaxIneffectiveApplies: core.Unlimited}}}
	r := h.run()
	if len(h.p.Invocations()) != 0 || cli.ExitCode(r) != cli.ExitHalted {
		t.Fatalf("budget 0 で送った: %+v", r.Runs)
	}
	h.in.Policies = nil
	record.WriteTempPolicy(h.rec.Dir(), record.TempPolicy{Name: "f", Select: core.Selector{All: true}, Mode: "report",
		Reason: "障害", By: "alice", At: h.clock.t, Until: h.clock.t.Add(time.Hour)})
	h.run()
	if len(h.p.Invocations()) != 0 {
		t.Fatal("freeze の間に送った")
	}
	h.clock.t = h.clock.t.Add(time.Hour)
	if s := runOf(h.run(), "up"); s.State != "done" || len(h.p.Invocations()) != 1 {
		t.Fatalf("freeze のあとに進める: %+v", s)
	}
}
