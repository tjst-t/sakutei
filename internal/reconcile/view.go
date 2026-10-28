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

package reconcile

// 読み取り専用の口（#136）に渡す、周ごとの見え方。
//
// 収束ループは周の終わりに、その周の観測から status（"sakutei status --json" と
// 同じ形）・計画（"sakutei plan --json" と同じ形）・依存の DAG を作り、
// Config.View に渡す。作るのは表示のためだけで、収束の判断には使わない（観測
// state レスは変わらない：次の周はまた現実を観測し直す）。
//
// tier の間引き（§16.5）でこの周に観測しなかったリソースは、status では前に観測
// した周の項目をそのまま出す（表示だけの持ち越し、S3）。一度も観測していなければ
// 観測できないものとして出す。

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/graphreport"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// CycleView は常駐ループの 1 周の見え方（#136）。
type CycleView struct {
	// Cycle は周の番号（デーモンのログと同じ）。End は周が終わった時刻。
	Cycle int
	End   time.Time
	// Observed はこの周が Observe まで進んだこと。Status・Plan・Graph はそのとき
	// だけ入る（飛ばした周・エラーの周は現実について何も確かめていない）。
	Observed bool
	Status   *statusreport.Report
	Plan     *planreport.Report
	Graph    *graphreport.Report
	// Health はデーモンの健全性（--daemon-status に書くものと同じ。claim・fencing の
	// 世代・git の配布の状態を含む）。毎周入る。
	Health statusreport.DaemonHealth
	// Firing は周の終わりに firing 中の level の Event（§22.3）。
	Firing []core.Event
}

// ViewSink は CycleView を受け取る（読み取り専用の口）。RecordCycle は収束ループの
// goroutine から周ごとに呼ばれる。すぐ返すこと（収束を止めない）。
type ViewSink interface {
	RecordCycle(CycleView)
}

// status の項目の種類。
const (
	itemConformant = iota
	itemDrift
	itemUnobservable
	itemWorkflow
	itemOscillating
)

// statusItem はリソース 1 つの status の項目。
type statusItem struct {
	kind  int
	id    core.ID
	drift statusreport.DriftedItem
	// unobservable のとき。reason は status の理由、availability は plan の値。
	reason, availability string
	// workflow のとき。
	workflow, stage string
	// oscillating のとき（#166）。
	osc statusreport.OscillatingItem
}

// viewState は周をまたいで持つ表示だけの状態（S3）。
type viewState struct {
	// last はリソース（ID 文字列）ごとの最後に観測した周の項目。
	last map[string]statusItem
}

// viewBuilder は 1 周の見え方を組み立てる。nil なら何もしない（Config.View が
// 無いとき）。
type viewBuilder struct {
	state *viewState
	items map[string]statusItem
	// ops はリソース（Local ID）ごとの、diff エンジンが出したすべての op
	// （cause C を含む）。
	ops map[string][]core.Op
	// ready は観測した ready の条件（§8.3）。
	ready Readiness

	status *statusreport.Report
	plan   *planreport.Report
	// planOps は plan.Operations と同じ順の元の op（ゲートの判定に使う）。
	planOps []core.Op
	// stopped は Apply を止めたリソース（Local ID → 理由、#166）。
	stopped map[string]string
	graph   *graphreport.Report
}

func newViewBuilder(cfg Config, st *viewState) *viewBuilder {
	if cfg.View == nil || st == nil {
		return nil
	}
	return &viewBuilder{state: st, items: map[string]statusItem{}, ops: map[string][]core.Op{}}
}

func (v *viewBuilder) add(it statusItem) {
	if v == nil {
		return
	}
	v.items[it.id.String()] = it
}

func (v *viewBuilder) conformant(id core.ID) { v.add(statusItem{kind: itemConformant, id: id}) }

func (v *viewBuilder) drifted(it statusreport.DriftedItem) {
	v.add(statusItem{kind: itemDrift, id: it.ID, drift: it})
}

// unobservable は観測できなかったリソース。availability は plan の値
// （"unknown" / "converging" など）。
func (v *viewBuilder) unobservable(id core.ID, availability, reason string) {
	v.add(statusItem{kind: itemUnobservable, id: id, availability: availability, reason: reason})
}

// oscillating は Apply を止めたリソース（#166、§11.1 Tier3）。
func (v *viewBuilder) oscillating(id core.ID, r ApplyRecord) {
	if v == nil {
		return
	}
	v.add(statusItem{kind: itemOscillating, id: id, osc: OscillatingItemOf(r)})
	if v.stopped == nil {
		v.stopped = map[string]string{}
	}
	v.stopped[id.Local] = "Apply stopped (oscillating, §11.1 Tier3): " + r.StopReasonText()
}

// OscillatingItemOf は止めた記録を status の項目にする。
func OscillatingItemOf(r ApplyRecord) statusreport.OscillatingItem {
	return statusreport.OscillatingItem{
		ID: r.ID, Diff: r.Diff, IneffectiveApplies: r.Applies, MaxApplies: r.Limits.MaxApplies,
		Settle: r.Limits.Settle.String(), Reason: r.Reason, Note: r.StopReasonText(), Since: r.StoppedAt,
	}
}

// ApplyStoppedHealth は止めた記録をデーモンの健全性の形にする。
func ApplyStoppedHealth(rs []ApplyRecord) []statusreport.ApplyStoppedHealth {
	var out []statusreport.ApplyStoppedHealth
	for _, r := range rs {
		it := OscillatingItemOf(r)
		out = append(out, statusreport.ApplyStoppedHealth{
			Resource: r.ID.String(), Diff: it.Diff, IneffectiveApplies: it.IneffectiveApplies, MaxApplies: it.MaxApplies,
			Settle: it.Settle, Reason: it.Reason, Note: it.Note, Since: it.Since.UTC(),
		})
	}
	return out
}

func (v *viewBuilder) workflowHeld(id core.ID, wf string) {
	v.add(statusItem{kind: itemWorkflow, id: id, workflow: wf})
}

// notDue は tier の間引きでこの周に観測しなかったリソース：前の周の項目を持ち越す。
func (v *viewBuilder) notDue(id core.ID) {
	if v == nil {
		return
	}
	if it, ok := v.state.last[id.String()]; ok {
		v.items[id.String()] = it
		return
	}
	v.unobservable(id, core.AvailabilityUnknown.String(), "not observed yet (not due for Observe: tier thinning, §16.5)")
}

func (v *viewBuilder) planned(local string, ops []core.Op) {
	if v == nil || len(ops) == 0 {
		return
	}
	v.ops[local] = ops
}

// viewInput は周の終わりに見え方を仕上げるのに要るもの。
type viewInput struct {
	cfg             Config
	in              loader.Intent
	rev             core.RevisionInfo
	desired         []diff.DesiredResource
	excludedByClaim int
	parentToChild   map[string][]string
	waiting         []render.PendingAllocation
	wfHeld          map[string]string
	ready           Readiness
	prune           prune.PruneResult
	pruneHalts      []planreport.PruneHalt
	decision        remediation.Decision
}

// finish は status・plan・graph を作る。ゲートの判定は後から gates で書き込む。
func (v *viewBuilder) finish(ctx context.Context, x viewInput) {
	if v == nil {
		return
	}
	now := time.Now()

	// Workflow の段階（§6.3）。収束ループが持つ S2 を読むだけ。
	if len(x.wfHeld) > 0 {
		stages := map[string]string{}
		for _, h := range workflow.Holds(ctx, x.cfg.WorkflowStore, x.in.Workflows) {
			stage := h.Describe(x.cfg.WorkflowStore != nil)
			for _, id := range h.Targets {
				stages[id.Local] = stage
			}
		}
		for key, it := range v.items {
			if it.kind == itemWorkflow {
				it.stage = stages[it.id.Local]
				v.items[key] = it
			}
		}
	}

	// status。
	b := statusreport.NewBuilder(x.rev.IntentRevision, x.rev.RenderedRevision, nil)
	resourceStatus := map[string]core.ConvergenceStatus{}
	keys := make([]string, 0, len(v.items))
	for k := range v.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	last := make(map[string]statusItem, len(v.items))
	for _, k := range keys {
		it := v.items[k]
		last[k] = it
		switch it.kind {
		case itemConformant:
			b.AddConvergedID(it.id)
			resourceStatus[it.id.Local] = core.StatusConverged
		case itemDrift:
			b.AddDriftedItem(it.drift)
			resourceStatus[it.id.Local] = core.StatusDrifted
		case itemUnobservable:
			b.AddUnobservableReason(it.id, it.reason)
			if it.availability == core.AvailabilityConverging.String() {
				resourceStatus[it.id.Local] = core.StatusConverging
			} else {
				resourceStatus[it.id.Local] = core.StatusUnknown
			}
		case itemWorkflow:
			b.AddWorkflowHeld(it.id, it.workflow, it.stage)
			resourceStatus[it.id.Local] = core.StatusConverging
		case itemOscillating:
			b.AddOscillating(it.osc)
			resourceStatus[it.id.Local] = core.StatusOscillating
		}
	}
	// 持ち越しは、今の desired に在るものだけ（消えたものは忘れる）。
	v.state.last = last
	st := b.Build()
	v.status = &st

	// plan。
	p := planreport.New(now)
	p.IntentRevision, p.RenderedRevision = x.rev.IntentRevision, x.rev.RenderedRevision
	if len(x.cfg.Claim.Labels) > 0 {
		p.Claim = &planreport.Claim{Selector: x.cfg.Claim.String(), InClaim: len(x.desired), Excluded: x.excludedByClaim}
	}
	p.Remediation = remediationView(x.decision, x.in.Remediation, now)
	for _, res := range x.desired {
		for _, op := range v.ops[res.ID.Local] {
			o := planreport.NewOperation(op, false)
			if res.UpdatePolicy.IsTask() && op.Kind != core.OpKindDelete {
				o.Task = &planreport.Task{Window: res.UpdatePolicy.Window, RollbackOnFail: res.UpdatePolicy.RollbackOnFail}
			}
			p.Operations = append(p.Operations, o)
			v.planOps = append(v.planOps, op)
		}
	}
	pruneOps := append([]core.Op(nil), x.prune.DeleteOps...)
	sort.SliceStable(pruneOps, func(i, j int) bool { return pruneOps[i].Target.String() < pruneOps[j].Target.String() })
	for _, op := range pruneOps {
		o := planreport.NewOperation(op, true)
		if x.prune.Halted {
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByPruneHalt, x.prune.HaltReason
		}
		p.Operations = append(p.Operations, o)
		v.planOps = append(v.planOps, op)
	}
	for _, id := range x.prune.Deferred {
		p.PruneDeferred = append(p.PruneDeferred, planreport.NewTarget(id))
	}
	p.PruneHalts = append(p.PruneHalts, x.pruneHalts...)
	for _, k := range keys {
		it := v.items[k]
		switch it.kind {
		case itemUnobservable:
			p.Unobservable = append(p.Unobservable, planreport.Unobservable{
				Target: planreport.NewTarget(it.id), Availability: it.availability})
		case itemWorkflow:
			p.Workflow = append(p.Workflow, planreport.WorkflowTarget{
				Target: planreport.NewTarget(it.id), Workflow: it.workflow, Stage: it.stage})
		}
	}
	for _, w := range x.waiting {
		pa := planreport.PendingAllocation{
			Target: planreport.NewTarget(w.ID), Field: w.Field, Pool: w.Pool,
			Line: fmt.Sprintf("sakutei: waiting-allocation: %s field %q from pool %q (not converged until allocated, §19.9)", w.ID, w.Field, w.Pool),
		}
		if w.Err != nil {
			pa.Error = w.Err.Error()
			pa.Line += ": " + w.Err.Error()
		}
		p.PendingAllocations = append(p.PendingAllocations, pa)
	}
	summaries := render.AggregateCompositeStatuses(x.parentToChild, resourceStatus)
	parents := make([]string, 0, len(summaries))
	for id := range summaries {
		parents = append(parents, id)
	}
	sort.Strings(parents)
	for _, id := range parents {
		p.Composites = append(p.Composites, planreport.Composite{ID: id, Status: summaries[id].AggregatedStatus.String()})
	}
	v.plan = p

	// graph。
	g := graphreport.New(now)
	g.IntentRevision, g.RenderedRevision = x.rev.IntentRevision, x.rev.RenderedRevision
	g.Claim = x.cfg.Claim.String()
	g.Build(x.desired, x.ready)
	v.graph = g
}

// holdAll は全体が alert-only の周（§14.1）：すべての操作を止めたものとして書く。
func (v *viewBuilder) holdAll(decision remediation.Decision, events []string) {
	if v == nil || v.plan == nil {
		return
	}
	reason := fmt.Sprintf("remediation mode is alert-only (%s) — reported, not remediated (§14.1)", decision.Reason)
	for i := range v.plan.Operations {
		o := &v.plan.Operations[i]
		if !o.Blocked {
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByAlertOnly, reason
		}
	}
	v.plan.PlannedChangeEvents = append(v.plan.PlannedChangeEvents, events...)
	v.plan.Summarize()
}

// gates はこの周のゲートの判定（観測だけの provider・alert-only の scope・予算・
// 容量）を操作に書く。判定は収束ループが実際に使ったもの（plan の再評価ではない）。
func (v *viewBuilder) gates(observeOnly func(string) bool, alertScopes []core.Scope, planned []budget.PlannedChange,
	bd, cd budget.CycleDecision) {
	if v == nil || v.plan == nil {
		return
	}
	p := v.plan
	p.Budget = planreport.GateOf(bd.HaltAll, bd.HaltDestructive, bd.Reason)
	p.Capacity = planreport.GateOf(cd.HaltAll, cd.HaltDestructive, cd.Reason)
	p.PlannedChangeEvents = append(p.PlannedChangeEvents, bd.Events...)
	declName := map[string]string{}
	for _, pc := range planned {
		declName[pc.Key()] = pc.String()
	}
	merged := budget.Merge(bd, cd)
	for i := range p.Operations {
		o := &p.Operations[i]
		if o.Blocked {
			continue
		}
		op := v.planOps[i]
		switch {
		case observeOnly(op.Target.Provider):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByObserveOnly, planreport.ObserveOnlyReason
			continue
		case inScopes(op.Target.Scope, alertScopes):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByAlertOnlyScope, planreport.AlertOnlyScopeReason
			continue
		case v.stopped[op.Target.Local] != "" && !o.Prune:
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByApplyStopped, v.stopped[op.Target.Local]
			continue
		case o.Task != nil || op.Cause == core.CauseTagC:
			// Apply しない（Task を起こす / ghost diff は見直しを待つ）ので予算を使わない。
			continue
		}
		if key := bd.LaneOf(op); key != "" {
			o.Lane, o.PlannedChange = planreport.LanePlannedChange, declName[key]
		}
		switch {
		case bd.Unapproved(op):
			// §12.2 (#163): an explicit absent's Delete outside every lane.
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByApproval, planreport.AbsentApprovalReason
		case bd.IsHalted(op):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByBudget, bd.Reason
		case merged.IsHalted(op):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByCapacity, cd.Reason
		}
	}
	p.Summarize()
}

func inScopes(s core.Scope, scopes []core.Scope) bool {
	for _, sc := range scopes {
		if s.Matches(sc) {
			return true
		}
	}
	return false
}

// remediationView はこの周の remediation mode（§14.1〜§14.3）。
func remediationView(d remediation.Decision, s *remediation.Schedule, now time.Time) *planreport.Remediation {
	mode := "auto"
	line := fmt.Sprintf("Remediation mode this cycle: auto (%s) — the daemon applies these changes unless a gate holds them", d.Reason)
	if d.IsAlertOnly() {
		mode = "alert-only"
		line = fmt.Sprintf("Remediation mode this cycle: alert-only (%s) — the daemon reports these changes but does not apply them (§14.1)", d.Reason)
	}
	outside := false
	if s != nil {
		m, _ := s.ModeAt(now)
		outside = m == remediation.ModeAlertOnly
	}
	return &planreport.Remediation{Mode: mode, Reason: d.Reason, OutsideWindow: outside, Line: line}
}
