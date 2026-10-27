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

// planned_test.go: planned_change の専用予算レーン（§13.3、Backlog #39）の単体テスト。
package budget

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

func scopedOp(kind core.OpKind, labels map[string]string, local string) core.Op {
	return core.Op{
		Kind:        kind,
		Target:      core.NewID("fake", core.NewScope(labels), local),
		Destructive: kind == core.OpKindDelete,
	}
}

func deletesIn(labels map[string]string, prefix string, n int) []core.Op {
	var ops []core.Op
	for i := 0; i < n; i++ {
		ops = append(ops, scopedOp(core.OpKindDelete, labels, fmt.Sprintf("%s-%03d", prefix, i)))
	}
	return ops
}

var (
	decomScope = map[string]string{"pool": "decom"}
	otherScope = map[string]string{"pool": "other"}
	t0         = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
)

func decl(kind PlannedKind, labels map[string]string, max int) PlannedChange {
	return PlannedChange{Scope: core.NewScope(labels), Kind: kind, MaxCount: max,
		Start: t0, End: t0.Add(6 * time.Hour)}
}

func hasEvent(d CycleDecision, prefix string) bool {
	for _, ev := range d.Events {
		if strings.HasPrefix(ev, prefix) {
			return true
		}
	}
	return false
}

// 窓内・scope 内・種別一致の 500 件の削除はレーンで通り、同じ周期の宣言外の削除は通常予算で
// halt する。レーンの op は通常予算の halt に止められない。
func TestPlannedChange_LaneIsSeparateFromNormalBudget(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 3, MaxDestructivePerWindow: 3}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 500)})

	all := append(deletesIn(decomScope, "decom", 500), deletesIn(otherScope, "stray", 5)...)
	d := e.CheckCycle(all, InFlight{}, 10)

	if !d.HaltAll {
		t.Fatalf("5 undeclared deletes > 3 must halt the normal budget; got %+v", d)
	}
	if d.LaneAdmitted() != 500 {
		t.Fatalf("LaneAdmitted = %d, want 500", d.LaneAdmitted())
	}
	allowed, blocked := d.FilterOps(all)
	if len(allowed) != 500 || len(blocked) != 5 {
		t.Fatalf("allowed=%d blocked=%d, want 500/5", len(allowed), len(blocked))
	}
	for _, op := range blocked {
		if op.Target.Scope.Labels["pool"] != "other" {
			t.Errorf("a lane op was blocked: %s", op.Target)
		}
	}

	// Dispatch + commit: the lane is consumed, the window budget is not.
	e.ReserveLanes(d, allowed)
	e.CommitOps(d.OutsideLanes(allowed))
	if got := e.WindowTotal(); got != 0 {
		t.Errorf("WindowTotal = %d after lane deletes, want 0 (lane is a separate budget)", got)
	}
	if got := e.LaneUsed(decl(PlannedDelete, decomScope, 500)); got != 500 {
		t.Errorf("LaneUsed = %d, want 500", got)
	}
}

// max_count を超えた分は通常予算で評価される。
func TestPlannedChange_OverMaxCountFallsToNormalBudget(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 3}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 10)})

	ops := deletesIn(decomScope, "decom", 12)
	d := e.CheckCycle(ops, InFlight{}, 1)
	if d.LaneAdmitted() != 10 || d.HaltAll {
		t.Fatalf("12 deletes, max_count 10, per-cycle 3: want 10 in lane and the 2 extra within the normal budget; got lane=%d %+v", d.LaneAdmitted(), d)
	}
	if !hasEvent(d, "PlannedChangeExhausted") {
		t.Errorf("want a PlannedChangeExhausted event; got %v", d.Events)
	}
	e.ReserveLanes(d, ops)

	// The next cycle: the lane is used up — everything is normal budget again.
	d = e.CheckCycle(deletesIn(decomScope, "more", 4), InFlight{}, 1)
	if d.LaneAdmitted() != 0 || !d.HaltAll {
		t.Fatalf("lane exhausted: 4 deletes must be normal budget and halt; got lane=%d %+v", d.LaneAdmitted(), d)
	}
}

// 発射しなかった op は枠を消費しない（ReserveLanes を呼ぶまで枠は減らない）。
func TestPlannedChange_UndispatchedOpsKeepTheLane(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 5)})
	for i := 0; i < 3; i++ {
		if d := e.CheckCycle(deletesIn(decomScope, "decom", 5), InFlight{}, 1); d.LaneAdmitted() != 5 {
			t.Fatalf("cycle %d: LaneAdmitted = %d, want 5 (nothing dispatched yet)", i, d.LaneAdmitted())
		}
	}
}

// 窓を過ぎたら自動失効し通常予算に戻る。窓の前もレーンは開かない。
func TestPlannedChange_WindowExpiry(t *testing.T) {
	clk := &fakeClock{t: t0.Add(-time.Minute)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 3}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 500)})

	if d := e.CheckCycle(deletesIn(decomScope, "decom", 10), InFlight{}, 1); d.LaneAdmitted() != 0 || !d.HaltAll {
		t.Fatalf("before the window: want normal budget halt; got lane=%d %+v", d.LaneAdmitted(), d)
	}
	clk.t = t0
	if d := e.CheckCycle(deletesIn(decomScope, "decom", 10), InFlight{}, 1); d.LaneAdmitted() != 10 || d.HaltAll {
		t.Fatalf("at window start: want lane; got lane=%d %+v", d.LaneAdmitted(), d)
	}
	clk.t = t0.Add(6 * time.Hour) // end is exclusive
	d := e.CheckCycle(deletesIn(decomScope, "decom", 10), InFlight{}, 1)
	if d.LaneAdmitted() != 0 || !d.HaltAll {
		t.Fatalf("after the window: want normal budget halt; got lane=%d %+v", d.LaneAdmitted(), d)
	}
	if !hasEvent(d, "PlannedChangeExpired") {
		t.Errorf("want PlannedChangeExpired; got %v", d.Events)
	}
	if d := e.CheckCycle(nil, InFlight{}, 1); hasEvent(d, "PlannedChangeExpired") {
		t.Errorf("PlannedChangeExpired must be reported once; got %v", d.Events)
	}
}

// kind: reimage は同じ scope の Delete+Create の対だけを通し、delete として数えない。
// 対の無い Delete は kind 不一致として通常予算で評価する。
func TestPlannedChange_ReimagePairing(t *testing.T) {
	hpc := map[string]string{"pool": "hpc"}
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 1, MaxDestructivePerWindow: 1}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedReimage, hpc, 5)})

	var ops []core.Op
	for i := 1; i <= 5; i++ {
		slot := map[string]string{"pool": "hpc", "slot": fmt.Sprintf("%02d", i)}
		ops = append(ops,
			scopedOp(core.OpKindDelete, slot, fmt.Sprintf("node-%02d-gen1", i)),
			scopedOp(core.OpKindCreate, slot, fmt.Sprintf("node-%02d-gen2", i)))
	}
	d := e.CheckCycle(ops, InFlight{}, 5)
	if d.HaltAll || d.HaltDestructive {
		t.Fatalf("5 reimage pairs must not count as 5 deletes against per-cycle 1; got %+v", d)
	}
	if d.LaneAdmitted() != 10 {
		t.Fatalf("LaneAdmitted = %d, want 10 (5 deletes + 5 creates)", d.LaneAdmitted())
	}
	e.ReserveLanes(d, ops)
	e.CommitOps(d.OutsideLanes(ops))
	if got := e.LaneUsed(decl(PlannedReimage, hpc, 5)); got != 5 {
		t.Errorf("LaneUsed = %d, want 5 (a pair counts once)", got)
	}
	if got := e.WindowTotal(); got != 0 {
		t.Errorf("WindowTotal = %d, want 0 (reimage is not a delete)", got)
	}

	// A Delete without a Create in exactly the same scope is not a reimage.
	e2 := newEngineWithClock(Config{MaxDestructivePerCycle: 1}, clk)
	e2.SetPlannedChanges([]PlannedChange{decl(PlannedReimage, hpc, 5)})
	unpaired := []core.Op{
		scopedOp(core.OpKindDelete, map[string]string{"pool": "hpc", "slot": "01"}, "node-01-gen1"),
		scopedOp(core.OpKindDelete, map[string]string{"pool": "hpc", "slot": "02"}, "node-02-gen1"),
		scopedOp(core.OpKindCreate, map[string]string{"pool": "hpc", "slot": "03"}, "node-03-gen2"),
	}
	d = e2.CheckCycle(unpaired, InFlight{}, 3)
	if d.LaneAdmitted() != 0 || !d.HaltAll {
		t.Fatalf("unpaired deletes must be normal budget (2 > 1 halts); got lane=%d %+v", d.LaneAdmitted(), d)
	}
	if !hasEvent(d, "PlannedChangeKindMismatch") {
		t.Errorf("want PlannedChangeKindMismatch; got %v", d.Events)
	}
}

// 宣言外（別 scope・別種別）は通常予算。kind: update は Delete を通さない。
func TestPlannedChange_KindAndScopeMustMatch(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxChangesPerCycleAbsolute: 2}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedUpdate, decomScope, 100)})

	ops := []core.Op{
		scopedOp(core.OpKindUpdate, decomScope, "a"),
		scopedOp(core.OpKindUpdate, decomScope, "b"),
		scopedOp(core.OpKindUpdate, decomScope, "c"),
		scopedOp(core.OpKindDelete, decomScope, "d"), // wrong kind
		scopedOp(core.OpKindUpdate, otherScope, "e"), // wrong scope
	}
	d := e.CheckCycle(ops, InFlight{}, 5)
	if d.LaneAdmitted() != 3 {
		t.Fatalf("LaneAdmitted = %d, want 3", d.LaneAdmitted())
	}
	if d.HaltAll {
		t.Fatalf("2 ops outside the lane are within max_changes_per_cycle_absolute=2; got %+v", d)
	}
	if !hasEvent(d, "PlannedChangeKindMismatch") {
		t.Errorf("the Delete in an update-only scope must raise PlannedChangeKindMismatch; got %v", d.Events)
	}
}

// capacity gate など予算以外のゲートはレーンの op も止める。
func TestPlannedChange_OtherGatesStillHaltLane(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{}, clk)
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 10)})
	ops := deletesIn(decomScope, "decom", 3)
	d := Merge(e.CheckCycle(ops, InFlight{}, 1), CycleDecision{HaltAll: true, HaltDestructive: true, Reason: "capacity"})
	if allowed, _ := d.FilterOps(ops); len(allowed) != 0 {
		t.Fatalf("capacity halt must stop lane ops too; %d allowed", len(allowed))
	}
}

func TestParseWindow(t *testing.T) {
	s, e, err := ParseWindow("2026-08-01 00:00 .. 2026-08-01 06:00")
	if err != nil || !s.Equal(t0) || !e.Equal(t0.Add(6*time.Hour)) {
		t.Fatalf("got %v %v %v", s, e, err)
	}
	if _, _, err := ParseWindow("2026-08-01T00:00:00+09:00 .. 2026-08-01T06:00:00+09:00"); err != nil {
		t.Fatalf("RFC 3339: %v", err)
	}
	for _, bad := range []string{"", "2026-08-01", "tomorrow .. later"} {
		if _, _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) must fail", bad)
		}
	}
	if err := (PlannedChange{Scope: core.NewScope(decomScope), Kind: PlannedDelete, MaxCount: 1, Start: t0, End: t0}).Validate(); err == nil {
		t.Error("an empty window must be rejected")
	}
	if err := (PlannedChange{Kind: PlannedDelete, MaxCount: 1, Start: t0, End: t0.Add(time.Hour)}).Validate(); err == nil {
		t.Error("an empty scope must be rejected")
	}
}

// #88: 入れ直しの Workflow のレーン。scope を覆う開いた reimage の宣言だけを返し、
// 対象ごとに max_count を 1 つ使う。窓の外・使い切り・取り下げは断る。
func TestPlannedChange_ReimageLaneForWorkflows(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{}, clk)
	target := core.NewID("redfish-system", core.NewScope(decomScope), "node1")

	if _, _, _, ok := e.OpenReimage(core.NewScope(decomScope)); ok {
		t.Fatalf("no declaration: OpenReimage ok")
	}
	e.SetPlannedChanges([]PlannedChange{decl(PlannedDelete, decomScope, 5), decl(PlannedReimage, otherScope, 5)})
	if _, _, _, ok := e.OpenReimage(core.NewScope(decomScope)); ok {
		t.Fatalf("a delete declaration and a reimage of another scope opened the reimage lane")
	}

	p := decl(PlannedReimage, decomScope, 1)
	e.SetPlannedChanges([]PlannedChange{p})
	key, _, left, ok := e.OpenReimage(core.NewScope(map[string]string{"pool": "decom", "rack": "r1"}))
	if !ok || !left || key != p.Key() {
		t.Fatalf("OpenReimage = %q %v %v, want the declaration with max_count left", key, left, ok)
	}
	if _, err := e.ReserveReimage(key, target, "w"); err != nil {
		t.Fatal(err)
	}
	if e.LaneUsed(p) != 1 {
		t.Fatalf("LaneUsed = %d, want 1", e.LaneUsed(p))
	}
	if _, _, left, ok := e.OpenReimage(core.NewScope(decomScope)); !ok || left {
		t.Fatalf("after max_count: left %v ok %v, want open without left", left, ok)
	}
	if _, err := e.ReserveReimage(key, target, "w"); err == nil || !strings.Contains(err.Error(), "max_count") {
		t.Fatalf("second reserve err = %v", err)
	}
	if d := e.CheckCycle(nil, InFlight{}, 0); !hasEvent(d, "PlannedChange: ") || !hasEvent(d, "PlannedChangeExhausted") {
		t.Fatalf("events = %v", d.Events)
	}

	clk.t = t0.Add(7 * time.Hour)
	if _, _, _, ok := e.OpenReimage(core.NewScope(decomScope)); ok {
		t.Fatalf("OpenReimage outside the window")
	}
	e.SetPlannedChanges(nil)
	if _, err := e.ReserveReimage(key, target, "w"); err == nil || !strings.Contains(err.Error(), "no longer declared") {
		t.Fatalf("reserve of a withdrawn declaration: err = %v", err)
	}
}
