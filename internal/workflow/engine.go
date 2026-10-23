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

package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/task"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// EventKind は Workflow の Event の種別。core.Event の kind に寄せる（§22.3）。
type EventKind = core.EventKind

const (
	EventStarted   = core.EventWorkflowStarted
	EventStepDone  = core.EventWorkflowStepDone
	EventWaiting   = core.EventWorkflowWaiting
	EventBlocked   = core.EventInvariantBlocked
	EventAwaiting  = core.EventStageAwaitingApproval
	EventApproved  = core.EventGateApproved
	EventHalted    = core.EventWorkflowHalted
	EventCompleted = core.EventWorkflowCompleted
	EventError     = core.EventWorkflowError
)

// Event は 1 周の Workflow の出来事。出すときは Core で core.Event にする。
type Event struct {
	Kind     EventKind
	Workflow string
	Message  string
}

func (e Event) String() string {
	return fmt.Sprintf("%s: workflow %s: %s", e.Kind, e.Workflow, e.Message)
}

// Core は e を core.Event にする（edge、§22.3）。halt と error は warning。
func (e Event) Core() core.Event {
	sev := core.SeverityInfo
	switch e.Kind {
	case EventHalted, EventError, EventBlocked:
		sev = core.SeverityWarning
	}
	return core.Event{
		Kind:     e.Kind,
		Severity: sev,
		Message:  e.Message,
		Attrs:    map[string]string{"workflow": e.Workflow},
	}
}

// maxStepsPerTick は 1 周で進める手順数の上限（暴走の歯止め）。
const maxStepsPerTick = 64

// Engine は Workflow を 1 周ずつ進める。状態は Store（S2）にだけあり、Engine は
// 毎周 Store と現実を読み直して次の一手を決める（level-triggered）。
type Engine struct {
	Store Store
	// Audit は S4 監査ログ。ゲート timeout の halt を記録する（承認・却下の記録は Decide）。
	Audit    *audit.Log
	Provider provider.Provider
	// ProviderFor は対象ごとの provider を返す（#67: 複数 provider のデーモン）。
	// 対象の Observe と Diff（その provider の Schema）はこれを通す。nil なら
	// Provider。nil を返した対象は観測できないもの（unknown）として待つ。
	ProviderFor func(id core.ID) provider.Provider
	// Exec は apply ステップの Plan を実行する（常駐ループの Executor = fencing 込み）。
	// nil なら Provider.Apply。
	Exec func(ctx context.Context, plan core.Plan) error
	// Tasks は task ステップの単純 Task を起動する（1-outstanding、§6.2）。nil なら
	// task ステップは進めず、TasksOff を理由として待つ。
	Tasks    *task.Manager
	TasksOff string
	Holder   string
	// Now は時刻。nil なら time.Now。
	Now func() time.Time

	// differs は provider 名ごとの diff エンジン（ProviderFor が nil なら "" の 1 つ）。
	differs map[string]*diff.Engine
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// providerOf は対象 id の provider を返す。解決できなければ nil。
func (e *Engine) providerOf(id core.ID) provider.Provider {
	if e.ProviderFor != nil {
		return e.ProviderFor(id)
	}
	return e.Provider
}

// diff は対象 id の provider の Schema に従う diff エンジンを返す。provider が
// 解決できなければ nil。
func (e *Engine) diff(id core.ID) *diff.Engine {
	key := ""
	if e.ProviderFor != nil {
		key = id.Provider
	}
	if d, ok := e.differs[key]; ok {
		return d
	}
	p := e.providerOf(id)
	if p == nil {
		return nil
	}
	if e.differs == nil {
		e.differs = map[string]*diff.Engine{}
	}
	d := diff.ForProvider(p)
	e.differs[key] = d
	return d
}

// Held は収束ループが Apply してはならない対象（Local ID → Workflow 名）を返す。
// 完了（done）していない Workflow の対象はすべて含む（実行中・待ち・halt・未起動）。
// Store を読めなければ全対象を返す（確信が持てないときは触らない、原則 3）。
func (e *Engine) Held(ctx context.Context, defs []Definition) (map[string]string, error) {
	held := map[string]string{}
	var firstErr error
	for _, h := range Holds(ctx, e.Store, defs) {
		if h.Err != nil && firstErr == nil {
			firstErr = h.Err
		}
		for _, id := range h.Targets {
			held[id.Local] = h.Workflow
		}
	}
	return held, firstErr
}

// Hold は done でない Workflow 1 つ（その対象は収束ループが触らない）と、その run。
type Hold struct {
	Workflow string
	Targets  []core.ID
	// Run は Store にある run。nil なら未起動（Store が無いときも nil）。
	Run *Run
	// Restart は前の run が done だが定義が変わった（次の周に新しい run が起動する）。
	Restart bool
	// Err は Store を読めなかったこと（そのときも対象は持つ、原則 3）。
	Err error
}

// Holds は done でない Workflow を宣言順に返す（Engine.Held と plan・status が共有する
// 判定、#89）。store が nil なら、どの Workflow も done と言えないので全部を返す。
func Holds(ctx context.Context, store Store, defs []Definition) []Hold {
	var out []Hold
	for _, def := range defs {
		h := Hold{Workflow: def.Name, Targets: def.Targets()}
		if store != nil {
			run, ok, err := store.Get(ctx, def.Name)
			switch {
			case err != nil:
				h.Err = err
			case ok && run.Phase == PhaseDone && run.DefHash == def.Hash():
				continue
			case ok && run.Phase == PhaseDone:
				h.Restart = true
			case ok:
				h.Run = &run
			}
		}
		out = append(out, h)
	}
	return out
}

// Describe は Hold の段階を 1 行で返す（plan・status の表示）。storeKnown は Store を
// 渡されたか（渡されなければ段階は分からない）。
func (h Hold) Describe(storeKnown bool) string {
	switch {
	case !storeKnown:
		return "stage unknown (no --workflow-store)"
	case h.Err != nil:
		return fmt.Sprintf("stage unknown (workflow store unreadable: %v)", h.Err)
	case h.Restart:
		return "not started (the definition changed since the last done run; a new run starts)"
	case h.Run == nil:
		return "not started"
	}
	r := h.Run
	switch r.Phase {
	case PhaseAwaitingApproval:
		approvers, need := r.Tally()
		return fmt.Sprintf("awaiting_approval (run %s, gate %s, %d of %d approval(s))", r.RunID, r.Gate, len(approvers), need)
	case PhaseHalted:
		return fmt.Sprintf("halted (run %s: %s)", r.RunID, r.Reason)
	default:
		return fmt.Sprintf("%s (run %s, stage %d target %d step %d)", r.Phase, r.RunID, r.Stage, r.Target, r.Step)
	}
}

// Tick は宣言された Workflow をそれぞれ進められるところまで進め、出来事を返す。
func (e *Engine) Tick(ctx context.Context, defs []Definition) []Event {
	sorted := append([]Definition(nil), defs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var events []Event
	for _, def := range sorted {
		events = append(events, e.tick(ctx, def)...)
	}
	return events
}

// stepper は 1 つの Workflow の 1 周分の作業。
type stepper struct {
	ctx    context.Context
	e      *Engine
	def    Definition
	run    Run
	now    time.Time
	events []Event
}

func (s *stepper) emit(kind EventKind, format string, args ...any) {
	s.events = append(s.events, Event{Kind: kind, Workflow: s.def.Name, Message: fmt.Sprintf(format, args...)})
}

// save は run を S2 に書く。失敗（競合を含む）なら false：この周はここで止め、
// 次の周に Store と現実を読み直す。
func (s *stepper) save() bool {
	s.run.UpdatedAt = s.now.UTC()
	saved, err := s.e.Store.Save(s.ctx, s.run)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			s.emit(EventWaiting, "run was updated concurrently (e.g. by the approval API); re-evaluating next cycle")
		} else {
			s.emit(EventError, "cannot persist progress (S2): %v; not proceeding", err)
		}
		return false
	}
	s.run = saved
	return true
}

func (s *stepper) halt(format string, args ...any) {
	s.run.Phase = PhaseHalted
	s.run.Reason = fmt.Sprintf(format, args...)
	if s.save() {
		s.emit(EventHalted, "%s (on_failure: halt; targets stay held until `sakutei workflow cancel`)", s.run.Reason)
	}
}

func (e *Engine) tick(ctx context.Context, def Definition) []Event {
	s := &stepper{ctx: ctx, e: e, def: def, now: e.now()}
	run, ok, err := e.Store.Get(ctx, def.Name)
	if err != nil {
		s.emit(EventError, "workflow store unreadable: %v; not proceeding", err)
		return s.events
	}
	hash := def.Hash()
	if !ok || (run.Phase == PhaseDone && run.DefHash != hash) {
		s.run = Run{
			Workflow:      def.Name,
			RunID:         fmt.Sprintf("%s-%d", def.Name, s.now.UnixNano()),
			DefHash:       hash,
			Applicant:     def.Applicant,
			Phase:         PhaseRunning,
			StepStartedAt: s.now.UTC(),
			Holder:        e.Holder,
			StartedAt:     s.now.UTC(),
			Version:       run.Version,
		}
		if !s.save() {
			return s.events
		}
		s.emit(EventStarted, "run %s started (%d stage(s), %d target(s))", s.run.RunID, len(def.Stages), len(def.Targets()))
	} else {
		s.run = run
	}

	if s.run.DefHash != hash && (s.run.Phase == PhaseRunning || s.run.Phase == PhaseAwaitingApproval) {
		s.halt("the workflow definition changed while run %s was in progress; cancel it to restart with the new definition", s.run.RunID)
		return s.events
	}

	switch s.run.Phase {
	case PhaseDone:
		return s.events
	case PhaseHalted:
		s.emit(EventHalted, "run %s is halted: %s (targets held; `sakutei workflow cancel %s` to restart)", s.run.RunID, s.run.Reason, def.Name)
		return s.events
	case PhaseAwaitingApproval:
		if !s.gate(ctx) {
			return s.events
		}
	}
	s.advance(ctx)
	return s.events
}

// gate は承認待ちのゲートを評価する。次のステージへ進めるなら true。
func (s *stepper) gate(ctx context.Context) bool {
	run := s.run
	if d, ok := run.rejection(run.Gate); ok {
		// Decide がその場で halt にしているはず。念のため。
		s.halt("gate %s rejected by %s", run.Gate, d.Approver)
		return false
	}
	approvers, need := run.Tally()
	if len(approvers) >= need {
		s.emit(EventApproved, "gate %s approved by %s (%d of %d required)", run.Gate, strings.Join(approvers, ", "), len(approvers), need)
		s.run.Phase = PhaseRunning
		s.run.Gate, s.run.GateOpenedAt, s.run.GateDeadline, s.run.GateRequirements = "", time.Time{}, time.Time{}, nil
		return s.nextStage()
	}
	if !s.now.Before(run.GateDeadline) {
		reason := fmt.Sprintf("gate %s timed out: no approval by %s (opened %s)",
			run.Gate, run.GateDeadline.Format(time.RFC3339), run.GateOpenedAt.Format(time.RFC3339))
		if s.e.Audit != nil {
			if err := s.e.Audit.Append(audit.Record{Time: s.now, Kind: audit.KindGateTimedOut,
				Workflow: run.Workflow, RunID: run.RunID, Gate: run.Gate, Detail: reason}); err != nil {
				s.emit(EventError, "S4 audit append failed: %v", err)
			}
		}
		s.halt("%s", reason)
		return false
	}
	s.emit(EventWaiting, "awaiting approval at gate %s until %s (%d of %d approval(s)%s; POST /workflows/%s/gates/%s/approve)",
		run.Gate, run.GateDeadline.Format(time.RFC3339), len(approvers), need, describe(run.GateRequirements), run.Workflow, run.Gate)
	return false
}

// nextStage は今のステージを終えた位置から次のステージへ進める。最後なら done。
// 進めて走り続けられるなら true。
func (s *stepper) nextStage() bool {
	s.run.Stage++
	s.run.Target, s.run.Step = 0, 0
	s.run.StepStartedAt = s.now.UTC()
	s.run.Reason = ""
	if s.run.Stage >= len(s.def.Stages) {
		s.run.Phase = PhaseDone
		if s.save() {
			s.emit(EventCompleted, "run %s completed; targets return to convergence", s.run.RunID)
		}
		return false
	}
	return s.save()
}

type stepStatus int

const (
	stepDone stepStatus = iota
	stepPending
	stepFailed
)

// advance は running の Workflow を進められるところまで進める。
func (s *stepper) advance(ctx context.Context) {
	for i := 0; i < maxStepsPerTick; i++ {
		stage := s.def.Stages[s.run.Stage]
		target := stage.Targets[s.run.Target]
		step := s.def.Steps[s.run.Step]
		where := fmt.Sprintf("stage %s, target %s, step %d (%s)", stage.Name, target.Local, s.run.Step+1, step.Kind)

		obs := s.observeAll(ctx)
		// 不変条件（前）: この手順を施した後の見込みで破れるなら進まない。active を
		// 減らさない手順（Task・undrain など）は止めない: 外から破られている最中でも
		// それ以上悪くはせず、止めると drain した対象を持ったまま動けなくなる。
		if s.def.Invariant.MinActive > 0 && (step.Kind == StepApply || step.Kind == StepTask) {
			n := s.countActive(obs, target, step.Fields)
			if n < s.def.Invariant.MinActive && n < s.countActive(obs, target, nil) {
				s.wait(EventBlocked, "%s: invariant min_active %d would break (%d active); not proceeding until it holds",
					where, s.def.Invariant.MinActive, n)
				return
			}
		}

		status, msg := s.exec(ctx, step, target, obs[target.Local])
		switch status {
		case stepPending:
			s.wait(EventWaiting, "%s: %s", where, msg)
			return
		case stepFailed:
			s.halt("%s: %s", where, msg)
			return
		}

		// 不変条件（後）: 破れていれば失敗。
		if s.def.Invariant.MinActive > 0 {
			if n := s.countActive(s.observeAll(ctx), target, nil); n < s.def.Invariant.MinActive {
				s.halt("%s: invariant min_active %d broken after the step (%d active)", where, s.def.Invariant.MinActive, n)
				return
			}
		}
		s.emit(EventStepDone, "%s: %s", where, msg)

		// 位置を進める。
		s.run.Step++
		s.run.StepStartedAt = s.now.UTC()
		s.run.Reason = ""
		if s.run.Step < len(s.def.Steps) {
			if !s.save() {
				return
			}
			continue
		}
		s.run.Step = 0
		s.run.Target++
		if s.run.Target < len(stage.Targets) {
			if !s.save() {
				return
			}
			continue
		}
		// ステージを終えた。
		if stage.Gate != nil {
			s.run.Phase = PhaseAwaitingApproval
			s.run.Gate = stage.Name
			s.run.GateOpenedAt = s.now.UTC()
			s.run.GateDeadline = s.now.Add(stage.Gate.Timeout).UTC()
			s.run.GateRequirements = stage.Gate.Requirements
			if s.save() {
				s.emit(EventAwaiting, "stage %s completed; awaiting approval at gate %s (%d approval(s)%s, timeout %s, deadline %s): POST /workflows/%s/gates/%s/approve",
					stage.Name, stage.Name, stage.Gate.Requirements.Needed(), describe(stage.Gate.Requirements),
					stage.Gate.Timeout, s.run.GateDeadline.Format(time.RFC3339), s.def.Name, stage.Name)
			}
			return
		}
		if !s.nextStage() {
			return
		}
	}
	s.emit(EventWaiting, "step limit per cycle reached; continuing next cycle")
}

// wait は進めない理由を記録する（変わったときだけ S2 に書く）。
func (s *stepper) wait(kind EventKind, format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	if s.run.Reason != reason {
		s.run.Reason = reason
		if !s.save() {
			return
		}
	}
	s.emit(kind, "%s", reason)
}

// exec は 1 つの手順を施す（冪等: 既に済んでいれば何もしない）。
func (s *stepper) exec(ctx context.Context, step Step, target core.ID, obs core.Observation) (stepStatus, string) {
	switch step.Kind {
	case StepVerify:
		if n := s.countActive(s.observeAll(ctx), target, nil); n < s.def.Invariant.MinActive {
			return stepFailed, fmt.Sprintf("verify: invariant min_active %d broken (%d active)", s.def.Invariant.MinActive, n)
		}
		return stepDone, "invariant holds"
	case StepWait:
		if s.satisfied(obs, target, step.Fields) {
			return stepDone, fmt.Sprintf("observed %v", step.Fields)
		}
		if step.Timeout > 0 && !s.now.Before(s.run.StepStartedAt.Add(step.Timeout)) {
			return stepFailed, fmt.Sprintf("wait for %v timed out after %s", step.Fields, step.Timeout)
		}
		return stepPending, fmt.Sprintf("waiting for %v", step.Fields)
	}

	// apply / task: 対象が観測できなければ待つ（unknown ≠ absent）。無ければ失敗。
	if obs.Availability != core.AvailabilityOk {
		return stepPending, "target is unobservable (unknown ≠ absent); waiting"
	}
	if obs.State == nil {
		return stepFailed, "target is absent in reality"
	}
	if s.satisfied(obs, target, step.Fields) {
		if step.Kind == StepTask && s.e.Tasks != nil {
			// 完了印が見えた: 未完了の記録を消す（§6.2）。
			if _, _, err := s.e.Tasks.Settle(ctx, target); err != nil {
				return stepPending, fmt.Sprintf("completion mark observed but the task record cannot be settled: %v", err)
			}
		}
		return stepDone, fmt.Sprintf("observed %v", step.Fields)
	}

	if step.Kind == StepApply {
		d := s.e.diff(target)
		if d == nil {
			return stepPending, "the target's provider is unavailable; waiting"
		}
		plan, err := d.Plan(obs, diff.DesiredResource{ID: target, Fields: step.Fields}, cause.CauseContext{})
		if err != nil {
			return stepFailed, fmt.Sprintf("plan %v: %v", step.Fields, err)
		}
		if err := s.e.execute(ctx, plan); err != nil {
			return stepFailed, fmt.Sprintf("apply %v: %v", step.Fields, err)
		}
		fresh, err := s.e.providerOf(target).Read(ctx, target.Local)
		if err == nil && s.satisfied(fresh, target, step.Fields) {
			return stepDone, fmt.Sprintf("applied %v", step.Fields)
		}
		return stepPending, fmt.Sprintf("applied %v; confirming by Observe", step.Fields)
	}

	// task
	if s.e.Tasks == nil {
		return stepPending, "cannot launch the task: " + s.e.TasksOff
	}
	dec, err := s.e.Tasks.Trigger(ctx, target, step.Fields, core.Plan{}, core.UpdatePolicy{Strategy: core.UpdateStrategyTask})
	if err != nil {
		return stepPending, fmt.Sprintf("cannot launch the task: %v", err)
	}
	switch dec.Outcome {
	case task.Launched:
		return stepPending, fmt.Sprintf("task %s launched for %v", dec.Record.TaskID, step.Fields)
	case task.Outstanding:
		if dec.Record.Phase == task.PhaseFailed {
			return stepFailed, dec.Reason
		}
	}
	return stepPending, dec.Reason
}

func (e *Engine) execute(ctx context.Context, plan core.Plan) error {
	if len(plan.Ops) == 0 {
		return nil
	}
	if e.Exec != nil {
		return e.Exec(ctx, plan)
	}
	return e.Provider.Apply(ctx, plan)
}

// satisfied は obs が fields を満たすか（Schema に従う Diff に差分が無いか）。
func (s *stepper) satisfied(obs core.Observation, id core.ID, fields map[string]string) bool {
	if obs.Availability != core.AvailabilityOk || obs.State == nil {
		return false
	}
	d := s.e.diff(id)
	if d == nil {
		return false
	}
	plan, err := d.Plan(obs, diff.DesiredResource{ID: id, Fields: fields}, cause.CauseContext{})
	return err == nil && len(plan.Ops) == 0
}

// observeAll は全対象を新しく Observe する（観測 state は持たない）。
func (s *stepper) observeAll(ctx context.Context) map[string]core.Observation {
	out := map[string]core.Observation{}
	for _, id := range s.def.Targets() {
		p := s.e.providerOf(id)
		if p == nil {
			out[id.Local] = core.Observation{Availability: core.AvailabilityUnknown}
			continue
		}
		obs, err := p.Read(ctx, id.Local)
		if err != nil {
			obs = core.Observation{Availability: core.AvailabilityUnknown}
		}
		out[id.Local] = obs
	}
	return out
}

// countActive は active な対象の数を返す。override があれば target の観測値に
// 重ねて数える（その手順を施した後の見込み）。観測できない対象は active に数えない。
func (s *stepper) countActive(obs map[string]core.Observation, target core.ID, override map[string]string) int {
	want := s.def.activeWhen()
	n := 0
	for _, id := range s.def.Targets() {
		o := obs[id.Local]
		if o.Availability != core.AvailabilityOk || o.State == nil {
			continue
		}
		fields := make(map[string]string, len(o.State))
		for k, tv := range o.State {
			fields[k] = fmt.Sprintf("%v", tv.Value)
		}
		if id.Local == target.Local {
			for k, v := range override {
				fields[k] = v
			}
		}
		active := true
		for k, v := range want {
			if fields[k] != v {
				active = false
				break
			}
		}
		if active {
			n++
		}
	}
	return n
}

// describe は承認者要件の役割・separation を Event の文面にする。
func describe(r *Requirements) string {
	if r == nil {
		return ""
	}
	var parts []string
	if len(r.Roles) > 0 {
		parts = append(parts, "roles "+strings.Join(r.Roles, "|"))
	}
	if r.Separation != "" {
		parts = append(parts, "separation "+r.Separation)
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, ", ")
}
