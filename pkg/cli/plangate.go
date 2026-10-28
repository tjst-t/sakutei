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

package cli

import (
	"context"
	"fmt"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/capacity"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// alertOnlyScopeReason is the block reason of an op in a scope declared
// remediation: alert-only (§14.3).
const alertOnlyScopeReason = planreport.AlertOnlyScopeReason

// gatePlanInput is what gatePlanOps evaluates.  report.Operations holds one
// Operation per op of ops, in the same order.
type gatePlanInput struct {
	ops          []core.Op
	report       *planreport.Report
	alertScopes  []core.Scope
	planned      []budget.PlannedChange
	budgetCfg    budget.Config
	auditLog     string
	capacity     []capacity.Gate
	totalDesired int
	providerFor  func(name string) provider.Provider
}

// gatePlanOps evaluates the gates a reconcile now would apply to the plan's
// ops (#135) and writes the verdict into the report: each op's lane, whether
// it would be blocked and why, and the budget and capacity decisions.  Nothing
// is consumed: the budget engine is fresh (as in a one-shot reconcile) and the
// S4 audit log is only read.
func gatePlanOps(ctx context.Context, in gatePlanInput, fail func(string, ...any)) {
	r := in.report
	var gated []core.Op
	var gatedIdx []int
	for i, op := range in.ops {
		o := &r.Operations[i]
		switch {
		case observeOnly(in.providerFor, op.Target.Provider):
			// §23.4: never applied, in any mode — so it uses no budget either.
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByObserveOnly, planreport.ObserveOnlyReason
		case scopeIn(op.Target.Scope, in.alertScopes):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByAlertOnlyScope, alertOnlyScopeReason
		case o.Task != nil || op.Cause == core.CauseTagC:
			// A reconcile does not Apply it (the daemon launches the Task / a
			// ghost diff is reviewed first), so it uses no budget.
		default:
			gated = append(gated, op)
			gatedIdx = append(gatedIdx, i)
		}
	}

	engine := budget.New(in.budgetCfg)
	if in.auditLog != "" {
		if err := engine.AttachAuditReadOnly(audit.NewLog(in.auditLog)); err != nil {
			fail("sakutei plan: read audit log: %v", err)
		}
		engine.SetPlannedChanges(in.planned)
	} else if len(in.planned) > 0 {
		fmt.Fprintf(logErr, "sakutei plan: note: %d planned_change declaration(s) need --audit-log to open their lanes (as in a one-shot reconcile); this plan evaluates every change by the normal budget\n", len(in.planned))
	}
	bd := engine.CheckCycle(gated, budget.InFlight{}, in.totalDesired)
	for _, ev := range bd.Events {
		reconcile.EmitPlannedChange(logErr, ev, "sakutei plan: "+ev)
		r.PlannedChangeEvents = append(r.PlannedChangeEvents, ev)
	}
	r.Budget = gateOf(bd)

	cd := capacity.CheckAll(ctx, in.capacity, func(g capacity.Gate) provider.Provider {
		name := g.Provider
		if name == "" {
			name = "fake"
		}
		return in.providerFor(name)
	})
	r.Capacity = gateOf(cd)

	declName := map[string]string{}
	for _, p := range in.planned {
		declName[p.Key()] = p.String()
	}
	for k, i := range gatedIdx {
		op, o := gated[k], &r.Operations[i]
		if key := bd.LaneOf(op); key != "" {
			o.Lane, o.PlannedChange = planreport.LanePlannedChange, declName[key]
		}
		switch {
		case bd.Unapproved(op):
			// §12.2 (#163): an explicit absent's Delete outside every lane.
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByApproval, planreport.AbsentApprovalReason
		case bd.IsHalted(op):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByBudget, bd.Reason
		case budget.Merge(bd, cd).IsHalted(op):
			o.Blocked, o.BlockedBy, o.BlockReason = true, planreport.BlockedByCapacity, cd.Reason
		}
	}
}

// observeOnly reports whether the provider name declares
// Capabilities.ObserveOnly (§23.4).
func observeOnly(providerFor func(string) provider.Provider, name string) bool {
	p := providerFor(name)
	return p != nil && p.Capabilities().ObserveOnly
}

// gateOf is the report form of a gate decision.
func gateOf(d budget.CycleDecision) planreport.Gate {
	return planreport.GateOf(d.HaltAll, d.HaltDestructive, d.Reason)
}

func scopeIn(s core.Scope, scopes []core.Scope) bool {
	for _, sc := range scopes {
		if s.Matches(sc) {
			return true
		}
	}
	return false
}

// printGateLines prints, above the ops, the gates that would halt some of
// them in a reconcile now.
func printGateLines(r *planreport.Report) {
	for _, g := range []struct {
		name string
		gate planreport.Gate
	}{{"Budget", r.Budget}, {"Capacity", r.Capacity}} {
		if !g.gate.Halted {
			continue
		}
		what := "all changes"
		if g.gate.Halt == "destructive" {
			what = "destructive changes"
		}
		fmt.Fprintf(logOut, "%s: a reconcile now would halt %s — %s\n", g.name, what, g.gate.Reason)
	}
	if r.Summary.Blocked > 0 {
		fmt.Fprintf(logOut, "Blocked: %d of %d change(s) would not be applied by a reconcile now\n", r.Summary.Blocked, r.Summary.Changes)
	}
	if r.Budget.Halted || r.Capacity.Halted || r.Summary.Blocked > 0 {
		fmt.Fprintln(logOut)
	}
}

// printOpGate prints an op's planned_change lane and, when a gate would block
// it, by what and why.
func printOpGate(o planreport.Operation) {
	if o.Lane == planreport.LanePlannedChange {
		fmt.Fprintf(logOut, "  lane:   %s\n", o.PlannedChange)
	}
	if o.Blocked {
		fmt.Fprintf(logOut, "  blocked: %s — %s\n", o.BlockedBy, o.BlockReason)
	}
}

// pendingAllocationJSON is the report form of a pending allocation; line is
// its stderr line.
func pendingAllocationJSON(pa render.PendingAllocation, line string) planreport.PendingAllocation {
	out := planreport.PendingAllocation{
		Target:        planreport.NewTarget(pa.ID),
		Field:         pa.Field,
		Pool:          pa.Pool,
		FreeKnown:     pa.FreeKnown,
		Free:          pa.Free,
		PendingInPool: pa.PendingInPool,
		Line:          line,
	}
	if pa.Err != nil {
		out.Error = pa.Err.Error()
	}
	return out
}
