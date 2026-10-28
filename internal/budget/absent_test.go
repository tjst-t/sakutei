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

// absent_test.go: 承認の要る op（ensure: absent の削除、core.Op.RequiresApproval、
// Backlog #163）の扱いの単体テスト。
package budget

import (
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func absentDelete(labels map[string]string, local string) core.Op {
	op := scopedOp(core.OpKindDelete, labels, local)
	op.RequiresApproval = true
	return op
}

// レーンの外の承認の要る op は止まり、通常予算にも数えない（走らないので）。
func TestRequiresApproval_HeldOutsideLanes(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 1}, clk)
	op := absentDelete(decomScope, "old")
	other := scopedOp(core.OpKindDelete, otherScope, "stray")

	d := e.CheckCycle([]core.Op{op, op2(op, "old2"), other}, InFlight{}, 10)
	if d.HaltAll || d.HaltDestructive {
		t.Fatalf("held absent deletes must not count toward the normal budget: %+v", d)
	}
	if !d.Unapproved(op) || !d.IsHalted(op) || !d.Holds() {
		t.Fatalf("an absent delete outside every lane must be held: unapproved=%v halted=%v", d.Unapproved(op), d.IsHalted(op))
	}
	if d.IsHalted(other) {
		t.Fatal("a normal delete within the budget must pass")
	}
}

// レーンに入った承認の要る op は走れるが、通常予算にも数え、通常予算の halt で止まる。
func TestRequiresApproval_LaneApprovesButDoesNotExempt(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 1}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 5)})

	one := absentDelete(decomScope, "old")
	d := e.CheckCycle([]core.Op{one}, InFlight{}, 10)
	if !d.InLane(one) || d.Unapproved(one) || d.IsHalted(one) {
		t.Fatalf("an absent delete in an open lane within the budget must pass: %+v", d)
	}
	if got := d.OutsideLanes([]core.Op{one}); len(got) != 1 {
		t.Fatalf("an absent delete in a lane must still be committed to the normal window: %v", got)
	}

	two := []core.Op{one, op2(one, "old2")}
	d = e.CheckCycle(two, InFlight{}, 10)
	if !d.HaltAll || !d.IsHalted(one) {
		t.Fatalf("2 absent deletes > max_destructive_per_cycle=1 must halt them even in the lane: %+v", d)
	}
}

func op2(op core.Op, local string) core.Op {
	op.Target = core.NewID(op.Target.Provider, op.Target.Scope, local)
	return op
}

// provider が承認の要る変更とした Create・Update（Backlog #179）は、その種類の
// レーン（create・update）だけが通し、ほかの種類のレーンでは止まる。
func TestRequiresApproval_ProviderChangeNeedsItsKind(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{}, clk)
	create := scopedOp(core.OpKindCreate, decomScope, "input")
	create.Destructive, create.RequiresApproval, create.ApprovalReason = true, true, "creates base chain input"
	update := scopedOp(core.OpKindUpdate, decomScope, "output")
	update.Destructive, update.RequiresApproval, update.ApprovalReason = true, true, "changes base chain output"

	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 5)})
	d := e.CheckCycle([]core.Op{create, update}, InFlight{}, 10)
	if !d.Unapproved(create) || !d.Unapproved(update) {
		t.Fatalf("a delete lane must not approve a create or an update: %+v", d)
	}
	e.SetPlannedChanges([]PlannedChange{decl(PlannedCreate, decomScope, 5)})
	d = e.CheckCycle([]core.Op{create, update}, InFlight{}, 10)
	if !d.InLane(create) || d.IsHalted(create) || !d.Unapproved(update) {
		t.Fatalf("a create lane approves the create only: %+v", d)
	}
	if got := d.OutsideLanes([]core.Op{create}); len(got) != 1 {
		t.Fatalf("the approved create must still be counted by the normal budget: %v", got)
	}
}
