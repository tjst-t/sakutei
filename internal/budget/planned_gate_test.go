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

// planned_gate_test.go: planned_change のレーンを開く条件（§14.4 / §14.5、Backlog #44）の単体テスト。
package budget

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
)

func auditKinds(t *testing.T, path string) map[audit.Kind]int {
	t.Helper()
	recs, err := audit.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[audit.Kind]int{}
	for _, r := range recs {
		out[r.Kind] += max(r.Count, 1)
	}
	return out
}

// 承認が要件を満たさない宣言はレーンを開かない（scope 内の変更は通常予算）。
func TestPlannedGate_UnapprovedKeepsLaneClosed(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 3}, clk)
	p := decl(PlannedDelete, decomScope, 10)
	p.Gate = PlannedGate{Profile: "enterprise", Unapproved: "approval has 1 of the 2 approver(s) required"}
	e.SetPlannedChanges([]PlannedChange{p})

	d := e.CheckCycle(deletesIn(decomScope, "d", 5), InFlight{}, 0)
	if d.LaneAdmitted() != 0 || !d.HaltDestructive {
		t.Fatalf("unapproved lane must stay closed and the normal budget halt: lane=%d halt=%v", d.LaneAdmitted(), d.HaltDestructive)
	}
	if !hasEvent(d, "PlannedChangeUnapproved") {
		t.Errorf("want PlannedChangeUnapproved, got %v", d.Events)
	}
	// Reported once, not every cycle.
	if d := e.CheckCycle(deletesIn(decomScope, "d", 5), InFlight{}, 0); hasEvent(d, "PlannedChangeUnapproved") {
		t.Errorf("the closed gate must be reported once")
	}
}

// cooling-off: 宣言から N が過ぎるまでレーンは閉じ、過ぎたら開く。宣言・開放・実行は S4 に残る。
func TestPlannedGate_CoolingOff(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	clk := &fakeClock{t: t0.Add(time.Minute)}
	e := newEngineWithClock(Config{MaxDestructivePerCycle: 3}, clk)
	if err := e.AttachAudit(audit.NewLog(logPath)); err != nil {
		t.Fatal(err)
	}
	p := decl(PlannedDelete, decomScope, 10)
	p.Gate = PlannedGate{Profile: "solo", CoolingOff: 2 * time.Hour}
	e.SetPlannedChanges([]PlannedChange{p})

	ops := deletesIn(decomScope, "d", 5)
	d := e.CheckCycle(ops, InFlight{}, 0)
	if d.LaneAdmitted() != 0 || !hasEvent(d, "PlannedChangeCoolingOff") {
		t.Fatalf("before the cooling-off the lane is closed: lane=%d events=%v", d.LaneAdmitted(), d.Events)
	}
	clk.advance(119 * time.Minute)
	if d := e.CheckCycle(ops, InFlight{}, 0); d.LaneAdmitted() != 0 {
		t.Fatalf("1 minute before the cooling-off ends the lane must still be closed")
	}
	clk.advance(time.Minute)
	d = e.CheckCycle(ops, InFlight{}, 0)
	if d.LaneAdmitted() != 5 || !hasEvent(d, "PlannedChangeOpened") {
		t.Fatalf("after the cooling-off the lane opens: lane=%d events=%v", d.LaneAdmitted(), d.Events)
	}
	e.ReserveLanes(d, ops)
	if err := e.FlushLaneAudit(); err != nil {
		t.Fatal(err)
	}
	got := auditKinds(t, logPath)
	if got[audit.KindPlannedChangeDeclared] != 10 || got[audit.KindPlannedChangeOpened] != 10 || got[audit.KindPlannedChangeExecuted] != 5 {
		t.Errorf("S4 records (declared/opened carry max_count, executed the dispatched count): %v", got)
	}
}

// 新しい Engine（再起動・次の reconcile）は S4 から宣言時刻と使用量を読み直す。
func TestPlannedGate_RestoredFromAudit(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	p := decl(PlannedDelete, decomScope, 6)
	p.Gate = PlannedGate{Profile: "solo", CoolingOff: time.Hour}

	clk := &fakeClock{t: t0.Add(time.Minute)}
	run := func() (*Engine, CycleDecision) {
		e := newEngineWithClock(Config{MaxDestructivePerCycle: 1}, clk)
		if err := e.AttachAudit(audit.NewLog(logPath)); err != nil {
			t.Fatal(err)
		}
		e.SetPlannedChanges([]PlannedChange{p})
		return e, e.CheckCycle(deletesIn(decomScope, "d", 4), InFlight{}, 0)
	}
	if _, d := run(); d.LaneAdmitted() != 0 {
		t.Fatalf("first run: cooling-off just started")
	}
	clk.advance(time.Hour)
	e, d := run()
	if d.LaneAdmitted() != 4 {
		t.Fatalf("a later run must measure the cooling-off from the declaration in S4: lane=%d %v", d.LaneAdmitted(), d.Events)
	}
	e.ReserveLanes(d, deletesIn(decomScope, "d", 4))
	if err := e.FlushLaneAudit(); err != nil {
		t.Fatal(err)
	}
	// max_count 6: the next run has 2 left.
	if _, d := run(); d.LaneAdmitted() != 2 || !hasEvent(d, "PlannedChangeExhausted") {
		t.Fatalf("max_count must hold across runs: lane=%d %v", d.LaneAdmitted(), d.Events)
	}
}

// 宣言は intent を読んだとき（SetPlannedChanges）に「見た」ことになり、CheckCycle を通らない周
// （remediation schedule の窓の外の alert-only・freeze）でも cooling-off の時計が動く（§14.5）。
func TestPlannedGate_DeclaredWithoutCheckCycle(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	p := decl(PlannedDelete, decomScope, 10)
	p.Gate = PlannedGate{Profile: "solo", CoolingOff: time.Hour}
	clk := &fakeClock{t: t0.Add(time.Minute)}
	newEngine := func() *Engine {
		e := newEngineWithClock(Config{MaxDestructivePerCycle: 1}, clk)
		if err := e.AttachAudit(audit.NewLog(logPath)); err != nil {
			t.Fatal(err)
		}
		return e
	}

	// alert-only の周: 宣言を読むだけで、CheckCycle は呼ばない。
	e := newEngine()
	e.SetPlannedChanges([]PlannedChange{p})
	if got := auditKinds(t, logPath)[audit.KindPlannedChangeDeclared]; got != 10 {
		t.Fatalf("SetPlannedChanges must record planned_change.declared (count max_count 10): %d", got)
	}
	if !e.DeclaredAt(p).Equal(t0.Add(time.Minute)) {
		t.Fatalf("the cooling-off clock must start when the declaration is read: %v", e.DeclaredAt(p))
	}
	if ev := e.TakePlannedEvents(); len(ev) != 0 {
		t.Errorf("no notice for a declaration recorded fine: %v", ev)
	}

	// 1 時間後に窓が開いた最初の周（再起動の後でも）は、そのまま開く。
	clk.advance(time.Hour)
	e = newEngine()
	e.SetPlannedChanges([]PlannedChange{p})
	if d := e.CheckCycle(deletesIn(decomScope, "d", 3), InFlight{}, 0); d.LaneAdmitted() != 3 || !hasEvent(d, "PlannedChangeOpened") {
		t.Fatalf("the cooling-off counted from the alert-only cycle must have elapsed: lane=%d events=%v", d.LaneAdmitted(), d.Events)
	}
	if got := auditKinds(t, logPath)[audit.KindPlannedChangeDeclared]; got != 10 {
		t.Errorf("the declaration must be recorded once: %d", got)
	}

	// 取り下げの知らせは、CheckCycle を通らない周でも TakePlannedEvents で取り出せる。
	e.SetPlannedChanges(nil)
	q := decl(PlannedDelete, decomScope, 20)
	q.Gate = PlannedGate{Profile: "solo", CoolingOff: time.Hour}
	e.SetPlannedChanges([]PlannedChange{q})
	e.SetPlannedChanges(nil)
	if ev := e.TakePlannedEvents(); len(ev) != 1 || !strings.Contains(ev[0], "PlannedChangeWithdrawn") {
		t.Errorf("want the withdrawal of the max_count 20 declaration: %v", ev)
	}
	if ev := e.TakePlannedEvents(); len(ev) != 0 {
		t.Errorf("TakePlannedEvents must clear the notices: %v", ev)
	}
}

// cooling-off は S4 なしでは評価できない（宣言時刻を残せない）ので閉じたまま。
func TestPlannedGate_CoolingOffNeedsAudit(t *testing.T) {
	clk := &fakeClock{t: t0.Add(time.Hour)}
	e := newEngineWithClock(Config{}, clk)
	p := decl(PlannedDelete, decomScope, 10)
	p.Gate = PlannedGate{Profile: "solo", CoolingOff: time.Minute}
	e.SetPlannedChanges([]PlannedChange{p})
	clk.advance(time.Hour)
	if d := e.CheckCycle(deletesIn(decomScope, "d", 2), InFlight{}, 0); d.LaneAdmitted() != 0 || !hasEvent(d, "PlannedChangeCoolingOff") {
		t.Fatalf("lane=%d events=%v", d.LaneAdmitted(), d.Events)
	}
}

// max_count を広げた宣言は別の宣言として cooling-off をやり直す。
func TestPlannedGate_WideningRestartsCoolingOff(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	clk := &fakeClock{t: t0.Add(time.Minute)}
	e := newEngineWithClock(Config{}, clk)
	if err := e.AttachAudit(audit.NewLog(logPath)); err != nil {
		t.Fatal(err)
	}
	p := decl(PlannedDelete, decomScope, 10)
	p.Gate = PlannedGate{Profile: "solo", CoolingOff: time.Hour}
	e.SetPlannedChanges([]PlannedChange{p})
	e.CheckCycle(nil, InFlight{}, 0)
	clk.advance(2 * time.Hour)
	p.MaxCount = 100
	e.SetPlannedChanges([]PlannedChange{p})
	if d := e.CheckCycle(deletesIn(decomScope, "d", 2), InFlight{}, 0); d.LaneAdmitted() != 0 {
		t.Fatalf("a widened declaration must cool off again")
	}
}

// cooling-off 中に消した宣言を書き戻すと cooling-off はやり直し（常駐でも、S4 から読み直す次の実行でも）。
func TestPlannedGate_WithdrawnRedeclareRestartsCoolingOff(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	p := decl(PlannedDelete, decomScope, 10)
	p.Gate = PlannedGate{Profile: "solo", CoolingOff: time.Hour}
	clk := &fakeClock{t: t0.Add(time.Minute)}
	newEngine := func() *Engine {
		e := newEngineWithClock(Config{}, clk)
		if err := e.AttachAudit(audit.NewLog(logPath)); err != nil {
			t.Fatal(err)
		}
		return e
	}

	e := newEngine()
	e.SetPlannedChanges([]PlannedChange{p})
	e.CheckCycle(nil, InFlight{}, 0) // declared at t0+1m
	e.SetPlannedChanges(nil)         // removed during the cooling-off
	if d := e.CheckCycle(nil, InFlight{}, 0); !hasEvent(d, "PlannedChangeWithdrawn") {
		t.Fatalf("want PlannedChangeWithdrawn, got %v", d.Events)
	}
	clk.advance(2 * time.Hour)
	e.SetPlannedChanges([]PlannedChange{p}) // written back
	if d := e.CheckCycle(deletesIn(decomScope, "d", 2), InFlight{}, 0); d.LaneAdmitted() != 0 {
		t.Fatalf("resident: a written-back declaration must cool off again")
	}

	// The next run (reconcile) restores the new clock, not the first one.
	if d := func() CycleDecision {
		e := newEngine()
		e.SetPlannedChanges([]PlannedChange{p})
		return e.CheckCycle(deletesIn(decomScope, "d", 2), InFlight{}, 0)
	}(); d.LaneAdmitted() != 0 {
		t.Fatalf("next run: the withdrawn declaration time must not count")
	}

	// A run that loads intent without the declaration withdraws a clock
	// restored from S4, so writing it back later cools off again too.
	clk.advance(30 * time.Minute)
	e = newEngine()
	e.SetPlannedChanges(nil)
	clk.advance(2 * time.Hour)
	e = newEngine()
	e.SetPlannedChanges([]PlannedChange{p})
	if d := e.CheckCycle(deletesIn(decomScope, "d", 2), InFlight{}, 0); d.LaneAdmitted() != 0 {
		t.Fatalf("reconcile: a declaration removed between runs must cool off again when written back")
	}
	clk.advance(time.Hour)
	if d := e.CheckCycle(deletesIn(decomScope, "d", 2), InFlight{}, 0); d.LaneAdmitted() != 2 {
		t.Fatalf("after the new cooling-off the lane opens: %v", d.Events)
	}
}
