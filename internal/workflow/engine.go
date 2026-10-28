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

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
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
	// EventDisruption は全体を止める型の更新の知らせ（level、#152）。
	EventDisruption = core.EventDisruption
)

// Event は 1 周の Workflow の出来事。出すときは Core で core.Event にする。
type Event struct {
	Kind     EventKind
	Workflow string
	Message  string
	// Resource は出来事の対象（EventDisruption では止める対象）。ほかは空。
	Resource core.ID
	// Attrs は付属情報（workflow のほかに足すもの）。
	Attrs map[string]string
}

func (e Event) String() string {
	return fmt.Sprintf("%s: workflow %s: %s", e.Kind, e.Workflow, e.Message)
}

// Core は e を core.Event にする（edge、§22.3）。halt と error は warning。
func (e Event) Core() core.Event {
	sev := core.SeverityInfo
	switch e.Kind {
	case EventHalted, EventError, EventBlocked, EventDisruption:
		sev = core.SeverityWarning
	}
	attrs := map[string]string{"workflow": e.Workflow}
	for k, v := range e.Attrs {
		attrs[k] = v
	}
	return core.Event{
		Kind:     e.Kind,
		Severity: sev,
		Resource: e.Resource,
		Message:  e.Message,
		Attrs:    attrs,
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
	// Admit は action の一度きりの操作（#139）を予算（§13）に通す。op は操作を
	// Op に写したもの（破壊的なら Destructive）。false なら reason を添えて待つ。
	// Commit は施した操作を予算の累積に数える。どちらも nil なら予算を見ない。
	Admit  func(op core.Op) (ok bool, reason string)
	Commit func(op core.Op)
	// Reimage は trigger: reimage の Workflow（#88）を起こす planned_change の
	// レーン（§13.3）。nil なら trigger: reimage の Workflow は起動しない。
	Reimage ReimageLanes
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

// ReimageLanes は planned_change kind: reimage のレーン（§13.3、#88）。budget.Engine
// が実装する（承認・cooling-off・窓・max_count はそちらが判定する）。
type ReimageLanes interface {
	// OpenReimage は scope を覆う、いま開いている（窓の中・承認済み）reimage の
	// 宣言の Key と名前を返す。left は max_count がまだ残っているか。無ければ ok false。
	OpenReimage(scope core.Scope) (key, name string, left, ok bool)
	// ReserveReimage は宣言 key の max_count を target の入れ直し 1 回分使う。
	// 宣言がもう開いていない・使い切っていればエラー。
	ReserveReimage(key string, target core.ID, workflow string) (name string, err error)
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
	// Targets は Workflow が触るリソース（対象と手順の resource、Definition.Resources）。
	Targets []core.ID
	// Run は Store にある run。nil なら未起動（Store が無いときも nil）。
	Run *Run
	// Restart は前の run が done だが定義が変わった（次の周に新しい run が起動する）。
	Restart bool
	// Err は Store を読めなかったこと（そのときも対象は持つ、原則 3）。
	Err error
}

// Holds は done でない Workflow を宣言順に返す（Engine.Held と plan・status が共有する
// 判定、#89）。store が nil なら、どの Workflow も done と言えないので全部を返す。
// trigger: reimage の Workflow（#88）は run が無いか done なら返さない（宣言が来る
// までは対象を収束ループに任せる。宣言が来た周は Tick が先に run を起こす）。
func Holds(ctx context.Context, store Store, defs []Definition) []Hold {
	var out []Hold
	for _, def := range defs {
		h := Hold{Workflow: def.Name, Targets: def.Resources()}
		if store != nil {
			run, ok, err := store.Get(ctx, def.Name)
			switch {
			case err != nil:
				h.Err = err
			case def.Trigger != "" && (!ok || run.Phase == PhaseDone):
				continue
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
	// obsErr は最後の Observe が読めなかった理由（リソースごと）。待ちの理由に
	// 添えて、なぜ観測できないのか（届かない・認証の失敗など）を人に見せる（#86）。
	obsErr map[core.IDKey]error
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
	start := !ok || (run.Phase == PhaseDone && run.DefHash != hash)
	trigger, by := "", ""
	if def.Trigger != "" {
		// #88: 宣言（planned_change kind: reimage）があるときだけ、宣言ごとに 1 回。
		start = false
		if !ok || run.Phase == PhaseDone {
			if e.Reimage == nil {
				s.emit(EventError, "trigger %s needs the planned_change lanes; not starting", def.Trigger)
				return s.events
			}
			key, name, left, open := e.Reimage.OpenReimage(def.Scope())
			switch {
			case !open:
				return s.events
			case ok && run.Trigger == key:
				return s.events // この宣言ではもう走った
			case !left:
				s.emit(EventWaiting, "not starting: %s has no max_count left for scope %s", name, def.Scope())
				return s.events
			}
			start, trigger, by = true, key, fmt.Sprintf(" for %s", name)
		}
	}
	if start {
		s.run = Run{
			Workflow:      def.Name,
			RunID:         fmt.Sprintf("%s-%d", def.Name, s.now.UnixNano()),
			DefHash:       hash,
			Applicant:     def.Applicant,
			Phase:         PhaseRunning,
			StepStartedAt: s.now.UTC(),
			Holder:        e.Holder,
			StartedAt:     s.now.UTC(),
			Trigger:       trigger,
			Version:       run.Version,
		}
		if !s.save() {
			return s.events
		}
		s.emit(EventStarted, "run %s started (%d stage(s), %d target(s))%s", s.run.RunID, len(def.Stages), len(def.Targets()), by)
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
		s.disrupting()
		return s.events
	case PhaseAwaitingApproval:
		if !s.gate(ctx) {
			s.disrupting()
			return s.events
		}
	}
	s.advance(ctx)
	s.disrupting()
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
		disruptGate := s.isDisruptionGate(run.Gate)
		s.run.Gate, s.run.GateOpenedAt, s.run.GateDeadline, s.run.GateRequirements = "", time.Time{}, time.Time{}, nil
		s.run.GateNotice = nil
		if disruptGate {
			// #152: 止める前のゲート。同じ対象の手順の前に戻る（時間帯と上流を
			// もう一度確かめてから知らせ、止める）。
			s.run.Disrupt.Approved = true
			s.run.Reason = ""
			return s.save()
		}
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
	s.run.Reserved = false
	s.run.Disrupt = nil
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
	// stepStopped は S2 に書けなかったので、この周はここで止める（save が出来事を出した）。
	stepStopped
)

// advance は running の Workflow を進められるところまで進める。
func (s *stepper) advance(ctx context.Context) {
	for i := 0; i < maxStepsPerTick; i++ {
		stage := s.def.Stages[s.run.Stage]
		target := stage.Targets[s.run.Target]
		step := s.def.Steps[s.run.Step]
		// res は手順が触るリソース（#87: 手順の resource。無ければ対象）。
		res := step.ResourceFor(target)
		where := fmt.Sprintf("stage %s, target %s, step %d (%s)", stage.Name, target.Local, s.run.Step+1, step.Kind)
		if step.Resource != "" {
			where = fmt.Sprintf("stage %s, target %s, step %d (%s %s/%s)", stage.Name, target.Local, s.run.Step+1, step.Kind, res.Provider, res.Local)
		}

		// #152: 全体を止める型の更新は、対象の最初の手順の前に時間帯・上流の積もり・
		// 承認・知らせを通す。
		if s.def.Disruption != nil && s.run.Step == 0 && !s.beforeDisruption(ctx, stage, target) {
			return
		}

		// #88: 入れ直しの Workflow は、対象に手を付ける前に宣言の max_count を 1 つ使う。
		// 宣言が取り下げられた・窓を過ぎた・使い切ったなら、その対象は入れ直さない。
		if s.def.Trigger != "" && s.run.Step == 0 && !s.run.Reserved {
			if s.e.Reimage == nil {
				s.halt("stage %s, target %s: trigger %s needs the planned_change lanes", stage.Name, target.Local, s.def.Trigger)
				return
			}
			name, err := s.e.Reimage.ReserveReimage(s.run.Trigger, target, s.def.Name)
			if err != nil {
				s.halt("stage %s, target %s: not reimaged: %v", stage.Name, target.Local, err)
				return
			}
			s.run.Reserved = true
			if !s.save() {
				return
			}
			s.emit(EventStepDone, "stage %s, target %s: reimage admitted by %s", stage.Name, target.Local, name)
		}

		obs := s.observeAll(ctx)
		resObs := obs[target.Local]
		if step.Resource != "" {
			resObs = s.observe(ctx, res)
		}
		// 不変条件（前）: この手順を施した後の見込みで破れるなら進まない。active を
		// 減らさない手順（Task・undrain など）は止めない: 外から破られている最中でも
		// それ以上悪くはせず、止めると drain した対象を持ったまま動けなくなる。
		// 対象でないリソースへの手順は active を変えない（見込みは今と同じ）。
		if s.def.Invariant.MinActive > 0 && (step.Kind == StepApply || step.Kind == StepTask) {
			n := s.countActive(obs, res, step.Fields)
			if n < s.def.Invariant.MinActive && n < s.countActive(obs, res, nil) {
				s.wait(EventBlocked, "%s: invariant min_active %d would break (%d active); not proceeding until it holds",
					where, s.def.Invariant.MinActive, n)
				return
			}
		}

		status, msg := s.exec(ctx, step, res, resObs)
		switch status {
		case stepPending:
			s.wait(EventWaiting, "%s: %s", where, msg)
			return
		case stepFailed:
			s.halt("%s: %s", where, msg)
			return
		case stepStopped:
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
		s.run.ActionSending = false
		if s.run.Step < len(s.def.Steps) {
			if !s.save() {
				return
			}
			continue
		}
		s.run.Step = 0
		s.run.Target++
		s.run.Reserved = false
		// #152: 対象の手順を終えた。止める知らせはこの周で resolved になる。
		s.run.Disrupt = nil
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

// exec は 1 つの手順を施す（冪等: 既に済んでいれば何もしない）。target は
// 手順が触るリソース（手順の resource か、今の対象）、obs はその観測。
func (s *stepper) exec(ctx context.Context, step Step, target core.ID, obs core.Observation) (stepStatus, string) {
	switch step.Kind {
	case StepVerify:
		if n := s.countActive(s.observeAll(ctx), target, nil); n < s.def.Invariant.MinActive {
			return stepFailed, fmt.Sprintf("verify: invariant min_active %d broken (%d active)", s.def.Invariant.MinActive, n)
		}
		return stepDone, "invariant holds"
	case StepWait, StepRequire:
		// 観測できない間も待つ（unknown ≠ absent）。timeout は手順に入った時刻から。
		if s.satisfied(obs, target, step.Fields) {
			return stepDone, fmt.Sprintf("observed %v", step.Fields)
		}
		if step.Timeout > 0 && !s.now.Before(s.run.StepStartedAt.Add(step.Timeout)) {
			return stepFailed, fmt.Sprintf("%s %v timed out after %s", step.Kind, step.Fields, step.Timeout)
		}
		if obs.Availability != core.AvailabilityOk {
			if err := s.obsErr[target.Key()]; err != nil {
				return stepPending, fmt.Sprintf("waiting for %v (unobservable: %s; unknown ≠ absent)", step.Fields, shortReason(err))
			}
			return stepPending, fmt.Sprintf("waiting for %v (unobservable; unknown ≠ absent)", step.Fields)
		}
		return stepPending, fmt.Sprintf("waiting for %v", step.Fields)
	case StepAction:
		return s.action(ctx, step, target)
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
		fresh, err := provider.ReadFor(ctx, s.e.providerOf(target), target.Local, step.Fields)
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

// action は一度きりの操作を 1 回だけ施す（#139）。観測で済んだかを見ないので、
// 送る前に「送りかけ」を S2 に書き、立ったまま戻ってきたら二度送らない。
func (s *stepper) action(ctx context.Context, step Step, target core.ID) (stepStatus, string) {
	if s.run.ActionSending {
		return stepFailed, fmt.Sprintf("%s was being sent when the run stopped and it is unknown whether it reached the target; not sending it twice (check the machine, then `sakutei workflow cancel %s` to start over)",
			step.Op, s.def.Name)
	}
	p := s.e.providerOf(target)
	if p == nil {
		return stepPending, fmt.Sprintf("provider %s is unavailable; waiting", target.Provider)
	}
	a, ok := p.(provider.Actioner)
	if !ok {
		return stepFailed, fmt.Sprintf("provider %s has no one-time operations (action %s)", target.Provider, step.Op)
	}
	plan, err := a.PlanAction(target.Local, step.Op, step.Args)
	if err != nil {
		return stepFailed, fmt.Sprintf("action %s: %v", step.Op, err)
	}
	desc := plan.Description
	if desc == "" {
		desc = step.Op
	}
	op := core.Op{Kind: core.OpKindUpdate, Target: target, Destructive: plan.Destructive,
		Diagnostic: fmt.Sprintf("workflow %s: one-time operation %s on %s", s.def.Name, desc, target)}
	// #88: 入れ直しの run の対象は planned_change のレーンで 1 回分を使っている
	// （ReserveReimage）。その入れ直しの中の破壊的な操作（強制再起動）は通常予算に
	// 数えない（reimage の対を破壊的変更に数えないのと同じ、§13.3）。
	inLane := s.def.Trigger != "" && s.run.Reserved
	if s.e.Admit != nil && !inLane {
		if ok, why := s.e.Admit(op); !ok {
			return stepPending, fmt.Sprintf("action %s is held by the change budget: %s", desc, why)
		}
	}
	s.run.ActionSending = true
	if !s.save() {
		return stepStopped, ""
	}
	if err := a.Action(provider.WithWrite(ctx), target.Local, step.Op, step.Args); err != nil {
		if errors.Is(err, provider.ErrActionNotSent) {
			s.run.ActionSending = false
			if !s.save() {
				return stepStopped, ""
			}
			return stepPending, fmt.Sprintf("action %s not sent: %s; trying again next cycle", desc, shortReason(err))
		}
		return stepFailed, fmt.Sprintf("action %s: %v", desc, err)
	}
	if inLane {
		return stepDone, fmt.Sprintf("sent %s (in the planned_change lane)", desc)
	}
	if plan.Destructive && s.e.Commit != nil {
		s.e.Commit(op)
	}
	return stepDone, fmt.Sprintf("sent %s", desc)
}

func (e *Engine) execute(ctx context.Context, plan core.Plan) error {
	if len(plan.Ops) == 0 {
		return nil
	}
	if e.Exec != nil {
		return e.Exec(ctx, plan)
	}
	return e.Provider.Apply(provider.WithWrite(ctx), plan)
}

// satisfied は obs が fields を満たすか（Schema に従う Diff に差分が無いか）。
func (s *stepper) satisfied(obs core.Observation, id core.ID, fields map[string]string) bool {
	if obs.Availability != core.AvailabilityOk || (obs.State == nil && obs.Check == nil) {
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
		out[id.Local] = s.observe(ctx, id)
	}
	return out
}

// observe は id を新しく Observe する。provider が無いか読めなければ unknown
// （読めなかった理由は obsErr に残す）。
func (s *stepper) observe(ctx context.Context, id core.ID) core.Observation {
	if s.obsErr == nil {
		s.obsErr = map[core.IDKey]error{}
	}
	delete(s.obsErr, id.Key())
	p := s.e.providerOf(id)
	if p == nil {
		s.obsErr[id.Key()] = fmt.Errorf("provider %s is unavailable", id.Provider)
		return core.Observation{Availability: core.AvailabilityUnknown}
	}
	obs, err := p.Read(ctx, id.Local)
	if err != nil {
		s.obsErr[id.Key()] = err
		return core.Observation{Availability: core.AvailabilityUnknown}
	}
	return obs
}

// shortReason は待ちの理由に添える error の頭（1 行、160 文字まで）。
func shortReason(err error) string {
	msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	if r := []rune(msg); len(r) > 160 {
		msg = string(r[:160]) + "…"
	}
	return msg
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
