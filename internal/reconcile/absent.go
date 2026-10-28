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

import (
	"fmt"
	"io"
	"sort"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// The explicit absent (`ensure: absent`, §12.2, Backlog #163).
//
// Diff plans the Delete of an absent resource that is present in reality, with
// core.Op.RequiresApproval set.  Every convergence path (the daemon's cycle,
// the scoped run, the one-shot reconcile) hands it to the change budget with
// its other ops: admitted to an open planned_change lane, it runs and uses the
// lane's max_count; outside every lane, the decision holds it (IsHalted /
// Unapproved) and the normal budget does not count it.  The holds below only
// report what the decision held.

// UnapprovedOps returns the ops of plans that decision holds for want of a
// lane, in a deterministic order.
func UnapprovedOps(plans map[string]core.Plan, decision budget.CycleDecision) []core.Op {
	var out []core.Op
	for _, plan := range plans {
		for _, op := range plan.Ops {
			if decision.Unapproved(op) {
				out = append(out, op)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target.String() < out[j].Target.String() })
	return out
}

// EmitAbsentAwaitingApproval raises AbsentAwaitingApproval for an absent
// resource present in reality whose Delete waits for its lane.  The Message
// is stable, so the level report raises it once while it waits.
func EmitAbsentAwaitingApproval(w io.Writer, prefix string, id core.ID) {
	eventlog.Emit(w, core.Event{
		Kind: core.EventAbsentAwaitingApproval, Severity: core.SeverityWarning,
		Resource: id, Message: planreport.AbsentApprovalReason,
		Text: fmt.Sprintf("%sAbsentAwaitingApproval: %s: not deleted: %s", prefix, id, planreport.AbsentApprovalReason),
	})
}
