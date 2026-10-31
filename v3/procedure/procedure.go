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

// Package procedure は一回きりの手順を周をまたいで 1 段ずつ進める（設計 §3.7）。
// 進みは Record の runs/ に置き、メモリには持たない（E-O3）。「済んだ」は観測
// （done_when）で決め、Record を失っても二度走らない（不変条件 18）。一回きりの
// 操作は送る前に送信中の記録を書き、再開のときに記録があれば二度送らず止める
// （不変条件 16）。
//
// Run は plan（Exec が nil：何も書かず、次に何をするかを出す）と reconcile
// （Exec あり：進めて Record と監査に書く）で同じ判定をする（不変条件 30）。
package procedure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/condition"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/graph"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

// 周の結果の手順の状態（plan.RunStatus.State）。
const (
	StateStart     = "start"
	StateRunning   = "running"
	StateHalted    = "halted"
	StateDone      = "done"
	StateCancelled = "cancelled"
)

// Input は手順の段の入力。
type Input struct {
	Procedures []core.Procedure
	// Resources は宣言（手順が受け持つ項目を外す前のもの。desired.項目 を引く）。
	Resources []core.Resource
	Readers   map[string]core.Reader
	// Policies は今の周に効く Policy（intent・一時的な Policy・手順の段の policy）。
	Policies  []core.Policy
	Runs      []record.Run
	Approvals map[string]record.Approval
	// Changes はこの周の収束の判定（手順を起こすきっかけ：Handover）。
	Changes []plan.Change
	Now     time.Time
	// Cycle は周の番号（Record の目印）。run の ID に入れて、同じ時刻に同じ対象の
	// 手順を起こし直しても ID を重ねない。
	Cycle  uint64
	Checks map[string]core.CheckFunc
	// Daemon は常駐の周か。一回実行は critical の段を進めない（§2.2）。
	Daemon bool
	// Exec が nil なら何も書かない（plan・進めない周）。
	Exec *Exec
}

// Exec は手順を進めるときに要るもの（ロックを持つ周だけ）。
type Exec struct {
	Record    *record.Record
	Audit     *record.Audit
	Providers map[string]core.Provider
	Commands  []record.Command
	// Reserve は書く段の前に、当たる Policy の予算を確かめて使う（不変条件 20）。
	// 通らなければ理由を返し、その段を進めない。
	Reserve func(policies []string, destructive bool) string
	// Write は書き込みの文脈（秘密の取り出し先を持つ、E-G2・E-G5）。
	Write core.WriteContext
}

// Output は手順の段の結果。
type Output struct {
	Runs   []plan.RunStatus
	Halts  []plan.Halt
	Errors []string
}

// Targets は手順の対象の ID を名前順で返す。ID で名指しなら宣言に無くてもよい。
// select なら宣言した Resource から選ぶ。
func Targets(p core.Procedure, g *graph.Graph) []core.ID {
	if p.Target.ID != "" {
		return []core.ID{{Type: p.Target.Type, Name: p.Target.ID}}
	}
	return g.Select(core.Selection{Type: p.Target.Type, Labels: p.Target.Labels})
}

// Prepare は手順が受け持つ項目（on_drift の drift_fields、E-F2・E-A5・N-A4）と、
// 止まった手順の対象（収束でも宣言どおりに戻さない、不変条件 18・E-A9）を返す。
func Prepare(procs []core.Procedure, res []core.Resource, runs []record.Run) (map[core.ID][]string, map[core.ID]string) {
	g := graph.New(res)
	handover := map[core.ID][]string{}
	for _, p := range procs {
		if p.Trigger.Kind != core.TriggerOnDrift {
			continue
		}
		for _, id := range Targets(p, g) {
			if g.Resource(id) != nil {
				handover[id] = append(handover[id], p.Trigger.Fields...)
			}
		}
	}
	held := map[core.ID]string{}
	for _, r := range runs {
		if r.State != record.RunHalted {
			continue
		}
		if id, err := core.ParseRef(r.Target); err == nil {
			held[id] = fmt.Sprintf("手順 %s（%s）が止まっている: %s", r.Procedure, r.RunID, r.Reason)
		}
	}
	return handover, held
}

// Policies は手順の段 policy が足した、手順の間だけ効く Policy（E-Q3・N-D4）。
func Policies(runs []record.Run) []core.Policy {
	var out []core.Policy
	for _, r := range runs {
		for _, t := range r.Policies {
			p := t.Policy()
			p.Until = time.Time{}
			out = append(out, p)
		}
	}
	return out
}

// Temporary は手順の段 policy を一時的な Policy の一覧の形にする。
func Temporary(runs []record.Run) []plan.Temporary {
	var out []plan.Temporary
	for _, r := range runs {
		for _, t := range r.Policies {
			out = append(out, plan.Temporary{Name: t.Name, Select: t.Select.String(), Reason: t.Reason, By: t.By,
				Source: "procedure " + r.RunID})
		}
	}
	return out
}

type engine struct {
	in      Input
	g       *graph.Graph
	out     Output
	running map[string]int // 手順 → 済んでいない対象の数（concurrency）
	starts  map[string][]record.Command
	cancels map[string][]record.Command
}

// Run は手順ごと・対象ごとに、起こすか・進めるか・止めるか・済んだかを決める。
// Exec があれば進めて書く。
func Run(ctx context.Context, in Input) Output {
	e := &engine{in: in, g: graph.New(in.Resources), running: map[string]int{},
		starts: map[string][]record.Command{}, cancels: map[string][]record.Command{}}
	runs := map[string]record.Run{}
	for _, r := range in.Runs {
		runs[key(r.Procedure, r.Target)] = r
		e.running[r.Procedure]++
	}
	if in.Exec != nil {
		e.commands(runs)
	}
	procs := map[string]bool{}
	for _, p := range in.Procedures {
		procs[p.Name] = true
		targets := Targets(p, e.g)
		seen := map[string]bool{}
		for _, id := range targets {
			seen[id.String()] = true
			r, ok := runs[key(p.Name, id.String())]
			var rp *record.Run
			if ok {
				rp = &r
			}
			e.one(ctx, p, id, rp)
		}
		// 対象から外れた（宣言・ラベルが変わった）未完了の手順も進め続ける。
		for _, r := range in.Runs {
			if r.Procedure == p.Name && !seen[r.Target] {
				if id, err := core.ParseRef(r.Target); err == nil {
					r := r
					e.one(ctx, p, id, &r)
				}
			}
		}
	}
	// intent から消えた手順の進みは触らない（止まったまま見せる）。
	for _, r := range in.Runs {
		if procs[r.Procedure] {
			continue
		}
		if cs := e.cancels[r.RunID]; len(cs) > 0 {
			x := &run{e: e, p: core.Procedure{Name: r.Procedure}, r: &r,
				st: plan.RunStatus{Procedure: r.Procedure, Target: r.Target, RunID: r.RunID}}
			x.cancel(cs)
			e.out.Runs = append(e.out.Runs, x.st)
			continue
		}
		e.out.Runs = append(e.out.Runs, plan.RunStatus{Procedure: r.Procedure, Target: r.Target, RunID: r.RunID,
			State: StateHalted, Step: r.Step + 1, StepName: r.StepName, Reason: "手順が intent に無い（sakutei cancel で片づける）"})
		e.out.Halts = append(e.out.Halts, plan.Halt{Scope: scope(r.Procedure, r.Target), Reason: "手順が intent に無い"})
	}
	// 片づけなかった start（対象・手順が無い）を断る。
	if in.Exec != nil {
		for _, list := range e.starts {
			for _, c := range list {
				e.audit(record.Entry{Kind: "procedure.start", Procedure: c.Procedure, By: c.By, Result: "error",
					Error: fmt.Sprintf("起こせない：手順 %q が無いか trigger: manual でない、対象 %q が無い、または同じ対象に未完了の手順がある", c.Procedure, c.Target)})
				e.removeCommand(c)
			}
		}
	}
	return e.out
}

func key(proc, target string) string { return proc + "\x00" + target }

func scope(proc, target string) string { return "procedure " + proc + " " + target }

// commands は CLI の頼み（start・cancel）を分ける。
func (e *engine) commands(runs map[string]record.Run) {
	for _, c := range e.in.Exec.Commands {
		switch c.Kind {
		case "start":
			e.starts[c.Procedure] = append(e.starts[c.Procedure], c)
		case "cancel":
			e.cancels[c.RunID] = append(e.cancels[c.RunID], c)
		default:
			e.audit(record.Entry{Kind: "procedure.command", By: c.By, Result: "error", Error: fmt.Sprintf("知らない頼み %q", c.Kind)})
			e.removeCommand(c)
		}
	}
	// 取り消す run が無い頼みは断って片づける。
	ids := map[string]bool{}
	for _, r := range runs {
		ids[r.RunID] = true
	}
	for id, list := range e.cancels {
		if ids[id] {
			continue
		}
		for _, c := range list {
			e.audit(record.Entry{Kind: "procedure.cancel", Run: id, By: c.By, Reason: c.Reason, Result: "error", Error: "その run は無い（済んだか取り消した）"})
			e.removeCommand(c)
		}
		delete(e.cancels, id)
	}
}

func (e *engine) removeCommand(c record.Command) {
	if err := e.in.Exec.Record.RemoveCommand(c.File); err != nil {
		e.out.Errors = append(e.out.Errors, "頼みを片づけられない: "+err.Error())
	}
}

func (e *engine) audit(en record.Entry) bool {
	if e.in.Exec == nil {
		return true
	}
	en.At = e.in.Now
	if _, err := e.in.Exec.Audit.Append(en); err != nil {
		e.out.Errors = append(e.out.Errors, "監査に書けない: "+err.Error())
		return false
	}
	return true
}

// run は 1 つの手順・対象の判定の文脈。
type run struct {
	e       *engine
	p       core.Procedure
	target  core.ID
	r       *record.Run // 無ければ始まっていない
	st      plan.RunStatus
	desired core.Fields
	labels  map[string]string
}

func (e *engine) one(ctx context.Context, p core.Procedure, id core.ID, r *record.Run) {
	x := &run{e: e, p: p, target: id, r: r}
	if res := e.g.Resource(id); res != nil {
		x.desired, x.labels = res.Fields, res.Labels
	}
	x.st = plan.RunStatus{Procedure: p.Name, Target: id.String(), Steps: len(p.Steps), Labels: x.labels}
	if !x.decide(ctx) {
		return
	}
	e.out.Runs = append(e.out.Runs, x.st)
}

// decide は 1 つの手順・対象を進める。出すものが無ければ false。
func (x *run) decide(ctx context.Context) bool {
	e := x.e
	if x.r != nil {
		x.st.RunID, x.st.State = x.r.RunID, StateRunning
		x.st.Step, x.st.StepName = x.r.Step+1, x.r.StepName
		if cs := e.cancels[x.r.RunID]; len(cs) > 0 && e.in.Exec != nil {
			return x.cancel(cs)
		}
		if x.r.State == record.RunHalted {
			if !x.resume() {
				return true
			}
		} else if x.r.Sending != nil {
			// 前の周が送る途中で止まった。送れたか分からないので二度送らない
			// （不変条件 16、E-A2・N-A2・E-K5）。
			s := x.p.Steps[min(x.r.Sending.Step, len(x.p.Steps)-1)]
			x.halt(fmt.Sprintf("段 %s（%s）を送る途中で止まった。送れたか分からないので送り直さない", s.Name, x.r.Sending.Op), true)
			x.e.audit(record.Entry{Kind: "procedure.interrupted", Procedure: x.p.Name, Run: x.r.RunID, Resource: x.r.Target,
				Step: s.Name, Op: x.r.Sending.Op, Result: "halted"})
			return true
		}
		x.advance(ctx)
		return true
	}
	return x.maybeStart(ctx)
}

// maybeStart は始まっていない対象を起こすかを決める。
func (x *run) maybeStart(ctx context.Context) bool {
	e := x.e
	var by string
	var cmd *record.Command
	switch x.p.Trigger.Kind {
	case core.TriggerOnDrift:
		var handover []string
		for _, c := range e.in.Changes {
			if c.ID == x.target {
				handover = c.Handover
			}
		}
		if len(handover) == 0 {
			return false
		}
		x.st.Waiting = "ずれ: " + strings.Join(handover, ", ")
	case core.TriggerManual:
		list := e.starts[x.p.Name]
		for i, c := range list {
			if c.Target == "" || c.Target == x.target.Name {
				c := c
				cmd, by = &c, c.By
				e.starts[x.p.Name] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
		if cmd == nil {
			return false
		}
	}
	x.st.State = StateStart
	// 済んでいれば起こさない（Record を失っても二度走らない、不変条件 18、E-A9）。
	if x.p.DoneWhen != "" {
		if t, _ := x.eval(ctx, x.p.DoneWhen); t == core.TriTrue {
			if cmd != nil {
				e.audit(record.Entry{Kind: "procedure.start", Procedure: x.p.Name, Resource: x.target.String(), By: by,
					Result: "skipped", Reason: "done_when が真（済んでいる）"})
				e.removeCommand(*cmd)
			}
			x.st.State, x.st.Waiting, x.st.Reason = StateDone, "", "done_when が真なので起こさない"
			return true
		}
	}
	if why := x.blocked(); why != "" {
		x.st.Waiting = why
		return true
	}
	if x.p.Precondition != "" {
		if t, note := x.eval(ctx, x.p.Precondition); t != core.TriTrue {
			x.st.Waiting = fmt.Sprintf("precondition %q が %s%s", x.p.Precondition, t, note)
			return true
		}
	}
	if e.running[x.p.Name] >= x.p.Concurrency {
		x.st.Waiting = fmt.Sprintf("concurrency %d：ほかの対象の手順が済むのを待つ", x.p.Concurrency)
		return true
	}
	e.running[x.p.Name]++
	if e.in.Exec == nil {
		x.st.Step, x.st.StepName = 1, x.p.Steps[0].Name
		x.st.Waiting = "起こす（段 " + x.p.Steps[0].Name + " から）"
		return true
	}
	x.r = &record.Run{Procedure: x.p.Name, Target: x.target.String(), RunID: runID(x.p.Name, x.target, e.in.Now, e.in.Cycle),
		StartedAt: e.in.Now, StartedBy: by, StepName: x.p.Steps[0].Name, StepStartedAt: e.in.Now, State: record.RunRunning}
	x.st.State, x.st.RunID, x.st.Step, x.st.StepName, x.st.Waiting = StateRunning, x.r.RunID, 1, x.r.StepName, ""
	if !x.save() {
		return true
	}
	e.audit(record.Entry{Kind: "procedure.start", Procedure: x.p.Name, Run: x.r.RunID, Resource: x.target.String(), By: by, Result: "ok"})
	if cmd != nil {
		e.removeCommand(*cmd)
	}
	x.advance(ctx)
	return true
}

func runID(proc string, target core.ID, now time.Time, cycle uint64) string {
	sum := sha256.Sum256([]byte(target.String()))
	return fmt.Sprintf("%s-%s-%s-c%d", proc, hex.EncodeToString(sum[:3]), now.UTC().Format("20060102T150405Z"), cycle)
}

// blocked は対象に当たる Policy が手順を止める理由（report・Record が読めない）。
// 窓の外・freeze は手順の起動と書く段にも効く（E-J3・E-J4）。
func (x *run) blocked() string {
	var report []string
	for _, p := range x.e.in.Policies {
		if p.Mode == core.ModeReport && p.Select.Matches(x.target.Type, x.labels) {
			report = append(report, p.Name)
		}
	}
	if len(report) > 0 {
		return fmt.Sprintf("policy %s は report", strings.Join(report, "・"))
	}
	return ""
}

func (x *run) policies() []string {
	var out []string
	for _, p := range x.e.in.Policies {
		if p.Select.Matches(x.target.Type, x.labels) {
			out = append(out, p.Name)
		}
	}
	return out
}

// save は進みを書く。書けなければ周の誤りにして進めない。
func (x *run) save() bool {
	if x.e.in.Exec == nil {
		return true
	}
	if err := x.e.in.Exec.Record.WriteRun(*x.r); err != nil {
		x.e.out.Errors = append(x.e.out.Errors, fmt.Sprintf("手順 %s の進みを書けない: %v", x.r.RunID, err))
		x.st.Error = "進みを Record に書けない: " + err.Error()
		return false
	}
	return true
}

// advance は段を進められるだけ進める。待つ段・承認・書く段の失敗で止まる。
func (x *run) advance(ctx context.Context) {
	e := x.e
	for i := 0; ; i++ {
		if i > len(x.p.Steps)+1 {
			return // 段の数より多く回らない（念のため）
		}
		if x.p.Timeout > 0 && !e.in.Now.Before(x.r.StartedAt.Add(x.p.Timeout)) {
			x.halt(fmt.Sprintf("手順の timeout %s を過ぎた", x.p.Timeout), false)
			return
		}
		// 済んだかは観測で決める。不明は済んでいない（不変条件 18）。
		if x.p.DoneWhen != "" {
			if t, _ := x.eval(ctx, x.p.DoneWhen); t == core.TriTrue {
				x.done("done_when が真")
				return
			}
		}
		if x.r.Step >= len(x.p.Steps) {
			if x.p.DoneWhen == "" {
				x.done("段が全部済んだ")
				return
			}
			x.st.Step, x.st.StepName = len(x.p.Steps), x.p.Steps[len(x.p.Steps)-1].Name
			x.st.Waiting = fmt.Sprintf("done_when %q が真になるのを待つ", x.p.DoneWhen)
			return
		}
		s := x.p.Steps[x.r.Step]
		x.st.Step, x.st.StepName = x.r.Step+1, s.Name
		if x.p.Invariant != "" {
			if t, note := x.eval(ctx, x.p.Invariant); t != core.TriTrue {
				x.st.Waiting = fmt.Sprintf("invariant %q が %s%s", x.p.Invariant, t, note)
				return
			}
		}
		if s.SkipIf != "" {
			if t, _ := x.eval(ctx, s.SkipIf); t == core.TriTrue {
				if !x.next("skipped") {
					return
				}
				continue
			}
		}
		if !x.step(ctx, s) {
			return
		}
	}
}

// next は今の段を済ませて次の段へ進め、書く。
func (x *run) next(result string) bool {
	s := x.p.Steps[x.r.Step]
	x.st.Passed = append(x.st.Passed, s.Name)
	if x.e.in.Exec == nil {
		return false // plan は 1 段先までしか見ない
	}
	x.r.Step++
	x.r.StepStartedAt = x.e.in.Now
	x.r.StepName = ""
	if x.r.Step < len(x.p.Steps) {
		x.r.StepName = x.p.Steps[x.r.Step].Name
	}
	if !x.save() {
		return false
	}
	x.e.audit(record.Entry{Kind: "procedure.step", Procedure: x.p.Name, Run: x.r.RunID, Resource: x.r.Target, Step: s.Name, Result: result})
	return true
}

// step は段 1 つを判定し、進めたら true。
func (x *run) step(ctx context.Context, s core.Step) bool {
	e := x.e
	since := x.r.StepStartedAt
	switch s.Kind {
	case core.StepWait:
		if s.When != "" {
			t, note := x.eval(ctx, s.When)
			if t == core.TriTrue {
				return x.next("ok")
			}
			if x.timedOut(s, since) {
				return false
			}
			x.st.Waiting = fmt.Sprintf("when %q が %s%s", s.When, t, note)
			return false
		}
		if e.in.Now.Before(since.Add(s.For)) {
			x.st.Waiting = fmt.Sprintf("%s まで待つ（for %s）", since.Add(s.For).Format(time.RFC3339), s.For)
			return false
		}
		return x.next("ok")
	case core.StepManual:
		// 人の「やった」は閉じる根拠にしない。done_when で閉じる（E-A6・E-Q3）。
		if x.timedOut(s, since) {
			return false
		}
		x.st.Instruction, x.st.URL = core.Substitute(s.Instruction, x.target.Name, x.desired), s.URL
		x.st.Waiting = fmt.Sprintf("人の作業を待つ。done_when %q が真になれば閉じる", x.p.DoneWhen)
		return false
	case core.StepGate:
		return x.gate(s)
	case core.StepPolicy:
		if e.in.Exec != nil {
			t := record.TempPolicy{Name: x.p.Name + "/" + s.Name, Select: s.Policy.Select, Mode: "report",
				Reason: s.Policy.Reason, By: "procedure " + x.r.RunID, At: e.in.Now}
			if t.Reason == "" {
				t.Reason = "手順 " + x.p.Name + " の段 " + s.Name
			}
			x.r.Policies = append(x.r.Policies, t)
		}
		return x.next("ok")
	case core.StepApply, core.StepInvoke:
		return x.write(ctx, s)
	}
	return false
}

func (x *run) timedOut(s core.Step, since time.Time) bool {
	if s.Timeout > 0 && !x.e.in.Now.Before(since.Add(s.Timeout)) {
		x.halt(fmt.Sprintf("段 %s の timeout %s を過ぎた", s.Name, s.Timeout), false)
		return true
	}
	return false
}

// gate は承認を待つ段（E-A5・E-L2・E-J8）。
func (x *run) gate(s core.Step) bool {
	e := x.e
	id := x.r.RunID + "-" + s.Name
	x.st.Request = id
	a, ok := e.in.Approvals[id]
	st := approval.Evaluate(a, ok, e.in.Now)
	switch st.State {
	case approval.Approved:
		if e.in.Exec != nil {
			req := a.Request
			req.State = record.RequestUsed
			if err := e.in.Exec.Record.WriteRequest(req, false); err != nil {
				// 使った印を書けなければ承認を使わない（不変条件 21）。
				e.out.Errors = append(e.out.Errors, "承認を使った印を書けない: "+err.Error())
				x.st.Waiting = "承認を使った印を Record に書けない"
				return false
			}
			e.audit(record.Entry{Kind: "approval.used", Request: id, Procedure: x.p.Name, Run: x.r.RunID, Step: s.Name, ApprovedBy: st.Approvers, Result: "ok"})
		}
		return x.next("approved")
	case approval.Rejected:
		x.halt(fmt.Sprintf("段 %s の承認 %s を %s が拒否した", s.Name, id, strings.Join(st.RejectedBy, "・")), false)
		return false
	case approval.Expired:
		x.halt(fmt.Sprintf("段 %s の承認 %s の期限が切れた", s.Name, id), false)
		return false
	case approval.Pending:
		x.st.Waiting = fmt.Sprintf("sakutei approve %s（%s）", id, st.Summary())
		return false
	}
	// 無い・使った（同じ段にもう一度来た）：頼む。
	x.request(id, "gate", s.Name, []record.Rule{approval.Rules("gate "+s.Name, s.Gate)}, s.Gate.Timeout, ok)
	return false
}

// request は承認の依頼を書く。
func (x *run) request(id, kind, step string, rules []record.Rule, timeout time.Duration, fresh bool) {
	e := x.e
	x.st.Request = id
	x.st.Waiting = fmt.Sprintf("承認を頼む: sakutei approve %s", id)
	if e.in.Exec == nil {
		return
	}
	req := record.ApprovalRequest{ID: id, Kind: kind, Subject: fmt.Sprintf("手順 %s %s 段 %s", x.p.Name, x.r.Target, step),
		Applicant: x.r.StartedBy, Rules: rules, CreatedAt: e.in.Now, State: record.RequestPending}
	if timeout > 0 {
		exp := e.in.Now.Add(timeout)
		req.Expires = &exp
	}
	if err := e.in.Exec.Record.WriteRequest(req, fresh); err != nil {
		e.out.Errors = append(e.out.Errors, "承認の依頼を書けない: "+err.Error())
		return
	}
	e.audit(record.Entry{Kind: "approval.request", Request: id, Procedure: x.p.Name, Run: x.r.RunID, Step: step, By: x.r.StartedBy, Result: "ok"})
}

// write は書く段（apply・invoke）。
func (x *run) write(ctx context.Context, s core.Step) bool {
	e := x.e
	if s.Critical && !e.in.Daemon {
		// 止まったときに見張る者がいる常駐だけが進める（§2.2、不変条件 16）。
		x.st.NeedsDaemon = true
		x.st.Waiting = fmt.Sprintf("段 %s は critical。常駐だけが進める", s.Name)
		return false
	}
	if why := x.blocked(); why != "" {
		x.st.Waiting = why
		return false
	}
	var target core.ID
	var res *core.Resource
	var destructive bool
	if s.Kind == core.StepApply {
		r := *s.Apply
		r.ID.Name = core.Substitute(r.ID.Name, x.target.Name, x.desired)
		f, err := x.fields(r)
		if err != nil {
			x.halt(fmt.Sprintf("段 %s の宣言: %v", s.Name, err), false)
			return false
		}
		r.Fields = f
		res, target = &r, r.ID
		// 現実を見て、宣言どおりなら当てずに進む。分からなければ書かない（不変条件 3）。
		ks := e.in.Readers[target.Provider()].Schema().Kinds[target.Kind()]
		c := plan.Compare(r, ks, plan.Observe(ctx, []core.ID{target}, e.in.Readers)[target])
		switch c.Op {
		case plan.None:
			return x.next("already")
		case plan.Unknown, plan.Converging:
			x.st.Waiting = fmt.Sprintf("段 %s の %s が %s（%s）", s.Name, target, c.Op, c.Note)
			return false
		}
		destructive = c.Op == plan.Delete || c.Replace
		x.st.Waiting = fmt.Sprintf("当てる: %s %s", c.Op, target)
		if e.in.Exec == nil {
			return false
		}
		return x.send(ctx, s, "apply", destructive, func(w core.WriteContext) error {
			return e.in.Exec.Providers[target.Provider()].Apply(ctx, w, target, res.Fields, c.Current)
		}, target)
	}
	target = x.target
	inv, _ := e.in.Readers[target.Provider()].(core.Invoker)
	if inv == nil {
		x.halt(fmt.Sprintf("provider %q は一回きりの操作を持たない", target.Provider()), false)
		return false
	}
	spec, ok := inv.Ops(target.Kind())[s.Op]
	if !ok {
		x.halt(fmt.Sprintf("provider %q の種類 %q に操作 %q が無い", target.Provider(), target.Kind(), s.Op), false)
		return false
	}
	destructive = spec.Destructive
	args := core.Fields{}
	for k, v := range s.Args {
		if str, ok := v.(string); ok {
			v = core.Substitute(str, x.target.Name, x.desired)
		}
		args[k] = v
	}
	x.st.Waiting = fmt.Sprintf("送る: %s %s", s.Op, target)
	if e.in.Exec == nil {
		return false
	}
	pinv, _ := e.in.Exec.Providers[target.Provider()].(core.Invoker)
	return x.send(ctx, s, s.Op, destructive, func(w core.WriteContext) error {
		return pinv.Invoke(ctx, w, target, s.Op, args)
	}, target)
}

// send は送信中の記録を先に書いてから送る（不変条件 16）。critical でない apply は
// 冪等なので記録を書かない。
func (x *run) send(ctx context.Context, s core.Step, op string, destructive bool, f func(core.WriteContext) error, target core.ID) bool {
	e := x.e
	if e.in.Exec.Reserve != nil {
		if why := e.in.Exec.Reserve(x.policies(), destructive); why != "" {
			x.st.Waiting = why
			return false
		}
	}
	mark := s.Kind == core.StepInvoke || s.Critical
	if mark {
		x.r.Sending = &record.Sending{Step: x.r.Step, Op: op, At: e.in.Now}
		if !x.save() {
			x.r.Sending = nil
			return false
		}
	}
	err := f(e.in.Exec.Write)
	kind := "procedure.invoke"
	if s.Kind == core.StepApply {
		kind = "procedure.apply"
	}
	en := record.Entry{Kind: kind, Procedure: x.p.Name, Run: x.r.RunID, Resource: target.String(), Step: s.Name, Op: op, Policies: x.policies()}
	switch {
	case err == nil:
		x.r.Sending = nil
		en.Result = "ok"
		e.audit(en)
		x.st.Waiting = ""
		return x.next("ok")
	case errors.Is(err, core.ErrNotSent):
		// 送れていない：記録を消して次の周に送り直す（E-A2）。
		x.r.Sending = nil
		x.save()
		en.Result, en.Error = "not_sent", err.Error()
		e.audit(en)
		x.st.Waiting = "送れていない（次の周に送り直す）: " + err.Error()
		return false
	default:
		// 送れたか分からない：二度送らず止める（不変条件 16）。
		x.r.Sending = nil
		en.Result, en.Error = "error", err.Error()
		e.audit(en)
		x.st.Error = err.Error()
		x.halt(fmt.Sprintf("段 %s が失敗した（送れたか分からない）: %v", s.Name, err), true)
		return false
	}
}

// fields は apply の段の項目を置き換えて正規形にする。
func (x *run) fields(r core.Resource) (core.Fields, error) {
	ks := x.e.in.Readers[r.ID.Provider()].Schema().Kinds[r.ID.Kind()]
	out := core.Fields{}
	for k, v := range r.Fields {
		if s, ok := v.(string); ok {
			v = core.Substitute(s, x.target.Name, x.desired)
		}
		n, err := core.Normalize(ks.Fields[k].Type, v)
		if err != nil {
			return nil, fmt.Errorf("項目 %q: %w", k, err)
		}
		out[k] = n
	}
	return out, nil
}

// halt は手順を止める。resumable なら続けるための承認を頼む（送れたか分からない・
// critical の段で止まった、E-A5・E-K5）。止まった対象は収束でも戻さない（E-A9）。
func (x *run) halt(reason string, resumable bool) {
	e := x.e
	x.st.State, x.st.Reason, x.st.Waiting = StateHalted, reason, ""
	e.out.Halts = append(e.out.Halts, plan.Halt{Scope: scope(x.p.Name, x.target.String()), Reason: reason})
	if e.in.Exec == nil {
		return
	}
	x.r.State, x.r.Reason = record.RunHalted, reason
	if resumable {
		x.r.Resume = x.r.RunID + "-resume-" + x.p.Steps[min(x.r.Step, len(x.p.Steps)-1)].Name
	}
	if !x.save() {
		return
	}
	e.audit(record.Entry{Kind: "procedure.halt", Procedure: x.p.Name, Run: x.r.RunID, Resource: x.r.Target, Reason: reason, Result: "halted"})
	if resumable {
		_, ok := e.in.Approvals[x.r.Resume]
		x.request(x.r.Resume, "resume", x.r.StepName, x.resumeRules(), 0, ok)
		x.st.Waiting = fmt.Sprintf("続けるなら sakutei approve %s（段 %s をもう一度送る）・やめるなら sakutei cancel %s", x.r.Resume, x.r.StepName, x.r.RunID)
	}
}

// resumeRules は止まった手順を続ける承認の要件：手順の最初の gate と同じ。gate が
// 無ければ申請者でない 1 人。
func (x *run) resumeRules() []record.Rule {
	for _, s := range x.p.Steps {
		if s.Kind == core.StepGate {
			return []record.Rule{approval.Rules("resume（gate "+s.Name+" と同じ）", s.Gate)}
		}
	}
	return []record.Rule{{From: "resume", N: 1}}
}

// resume は止まった手順を続けるか。続けるなら true。
func (x *run) resume() bool {
	e := x.e
	x.st.State, x.st.Reason = StateHalted, x.r.Reason
	if x.r.Resume == "" {
		x.st.Waiting = fmt.Sprintf("やめるなら sakutei cancel %s", x.r.RunID)
		e.out.Halts = append(e.out.Halts, plan.Halt{Scope: scope(x.p.Name, x.target.String()), Reason: x.r.Reason})
		return false
	}
	x.st.Request = x.r.Resume
	a, ok := e.in.Approvals[x.r.Resume]
	st := approval.Evaluate(a, ok, e.in.Now)
	if st.State != approval.Approved {
		x.st.Waiting = fmt.Sprintf("続けるなら sakutei approve %s（%s）・やめるなら sakutei cancel %s", x.r.Resume, st.Summary(), x.r.RunID)
		if st.State == approval.Missing && e.in.Exec != nil {
			x.request(x.r.Resume, "resume", x.r.StepName, x.resumeRules(), 0, false)
		}
		e.out.Halts = append(e.out.Halts, plan.Halt{Scope: scope(x.p.Name, x.target.String()), Reason: x.r.Reason})
		return false
	}
	if e.in.Exec == nil {
		x.st.State, x.st.Waiting = StateRunning, "承認されたので続ける"
		return false
	}
	req := a.Request
	req.State = record.RequestUsed
	if err := e.in.Exec.Record.WriteRequest(req, false); err != nil {
		e.out.Errors = append(e.out.Errors, "承認を使った印を書けない: "+err.Error())
		e.out.Halts = append(e.out.Halts, plan.Halt{Scope: scope(x.p.Name, x.target.String()), Reason: x.r.Reason})
		return false
	}
	x.r.State, x.r.Reason, x.r.Resume, x.r.Sending = record.RunRunning, "", "", nil
	x.r.StepStartedAt = e.in.Now
	if !x.save() {
		return false
	}
	e.audit(record.Entry{Kind: "procedure.resume", Request: req.ID, Procedure: x.p.Name, Run: x.r.RunID, Resource: x.r.Target,
		ApprovedBy: st.Approvers, Result: "ok"})
	x.st.State, x.st.Reason = StateRunning, ""
	return true
}

func (x *run) done(why string) {
	e := x.e
	x.st.State, x.st.Waiting = StateDone, ""
	x.st.Reason = why
	if e.in.Exec == nil {
		return
	}
	if err := e.in.Exec.Record.RemoveRun(x.r.Procedure, x.r.Target); err != nil {
		e.out.Errors = append(e.out.Errors, "済んだ手順の進みを消せない: "+err.Error())
		return
	}
	e.running[x.p.Name]--
	e.audit(record.Entry{Kind: "procedure.done", Procedure: x.p.Name, Run: x.r.RunID, Resource: x.r.Target, Reason: why, Result: "ok"})
}

func (x *run) cancel(cs []record.Command) bool {
	e := x.e
	if err := e.in.Exec.Record.RemoveRun(x.r.Procedure, x.r.Target); err != nil {
		e.out.Errors = append(e.out.Errors, "取り消した手順の進みを消せない: "+err.Error())
		return true
	}
	e.running[x.p.Name]--
	for _, c := range cs {
		e.audit(record.Entry{Kind: "procedure.cancel", Procedure: x.p.Name, Run: x.r.RunID, Resource: x.r.Target, By: c.By, Reason: c.Reason, Result: "ok"})
		e.removeCommand(c)
	}
	delete(e.cancels, x.r.RunID)
	x.st.State, x.st.Reason = StateCancelled, cs[0].Reason
	return true
}

// eval は式を対象に合わせて読み、今の観測で評価する（plan と reconcile で同じ、
// E-J9）。参照する対象をこのとき観測する（周をまたいで持ち越さない、不変条件 1）。
func (x *run) eval(ctx context.Context, src string) (core.Tri, string) {
	c, err := condition.Parse(core.Substitute(src, x.target.Name, x.desired))
	if err != nil {
		return core.TriUnknown, "（" + err.Error() + "）"
	}
	refs := c.Refs(x.e.g.Select)
	obs := plan.Observe(ctx, refs, x.e.in.Readers)
	env := &env{obs: obs, g: x.e.g, desired: x.desired, checks: map[string]core.Tri{}}
	var notes []string
	for _, name := range c.Checks() {
		f, ok := x.e.in.Checks[name]
		if !ok {
			env.checks[name] = core.TriUnknown
			continue
		}
		t, why := f(ctx, env)
		env.checks[name] = t
		if why != "" {
			notes = append(notes, fmt.Sprintf("check(%s): %s", name, why))
		}
	}
	note := ""
	if len(notes) > 0 {
		sort.Strings(notes)
		note = "（" + strings.Join(notes, "・") + "）"
	}
	return c.Eval(env), note
}

type env struct {
	obs     map[core.ID]core.Observation
	g       *graph.Graph
	desired core.Fields
	checks  map[string]core.Tri
}

func (e *env) Observation(id core.ID) (core.Observation, bool) {
	o, ok := e.obs[id]
	return o, ok
}

func (e *env) Select(s core.Selection) []core.ID { return e.g.Select(s) }
func (e *env) Desired() core.Fields              { return e.desired }
func (e *env) Check(name string) core.Tri        { return e.checks[name] }
