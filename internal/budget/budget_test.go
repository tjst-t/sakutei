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

// budget_test.go contains white-box unit tests for the cumulative time-window
// budget — the CORE scenario of Story Scf7762-2 ("progressive bulk-delete"):
// deleting a little every cycle to consume the window and bypass the per-cycle
// gate.
//
// The CLI acceptance tests exercise the single-cycle pre-check (planned count >
// window cap halts on cycle 1).  That path never accumulates: CommitOps is
// never reached, so windowTotal stays 0.  These tests drive the accumulation
// path directly — CommitOps in one cycle, CheckCycle in the next — using an
// injectable clock (Config.now, same-package access) so the rolling-window
// eviction can also be exercised deterministically without real sleeps.
//
// AC tag: [AC-Scf7762-2-2] (cumulative window budget blocks progressive delete).
package budget

import (
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// destructiveOps returns n distinct destructive Delete ops.
func destructiveOps(n int) []core.Op {
	ops := make([]core.Op, 0, n)
	for i := 0; i < n; i++ {
		ops = append(ops, core.Op{
			Kind:        core.OpKindDelete,
			Destructive: true,
			Target:      core.ID{Provider: "fake", Local: "vm"},
		})
	}
	return ops
}

// fakeClock is a controllable time source for the injectable Config.now.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newEngineWithClock builds an Engine whose window clock is driven by clk.
// It sets the unexported Config.now field directly (white-box, same package).
func newEngineWithClock(cfg Config, clk *fakeClock) *Engine {
	cfg.now = clk.now
	return New(cfg)
}

// ---------------------------------------------------------------------------
// [AC-Scf7762-2-2] progressive destruction: window accumulates across cycles
// ---------------------------------------------------------------------------

// TestWindowBudget_ProgressiveDestruction_AccumulatesAcrossCycles is the core
// Story scenario.  With per-window cap = 3 and NO per-cycle limit, a single
// cycle could delete only 2 at a time (below any per-cycle gate) — the classic
// "slowly delete everything" attack.  The cumulative window budget must stop it
// on the cycle where the ROLLING TOTAL (already-committed + planned) would
// exceed the cap.
//
// Timeline (window = 1h, clock frozen so nothing decays):
//
//	cycle 1: 0 committed + 2 planned = 2 ≤ 3        → allowed; CommitOps(2)
//	cycle 2: 2 committed + 1 planned = 3 ≤ 3        → allowed; CommitOps(1)
//	cycle 3: 3 committed + 1 planned = 4 > 3        → HALT (window)
//
// This proves CheckCycle consults windowTotal (advanced only by CommitOps),
// i.e. the accumulation path — not the single-cycle pre-check.
func TestWindowBudget_ProgressiveDestruction_AccumulatesAcrossCycles(t *testing.T) {
	// [AC-Scf7762-2-2]
	clk := &fakeClock{t: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}
	e := newEngineWithClock(Config{
		MaxDestructivePerWindow: 3,
		WindowDuration:          time.Hour,
		// Deliberately NO per-cycle limit: this isolates the WINDOW gate so the
		// halt below can only be the cumulative budget, never the rate limit.
	}, clk)

	// --- cycle 1: 2 planned, window empty → allowed ---
	if got := e.WindowTotal(); got != 0 {
		t.Fatalf("cycle 1 precondition: windowTotal=%d, want 0", got)
	}
	d1 := e.CheckCycle(destructiveOps(2), InFlight{}, 10)
	if d1.HaltAll || d1.HaltDestructive {
		t.Fatalf("cycle 1: unexpected halt (2 ≤ cap 3): %+v", d1)
	}
	e.CommitOps(destructiveOps(2)) // deletions actually happened
	if got := e.WindowTotal(); got != 2 {
		t.Fatalf("cycle 1: windowTotal=%d after CommitOps(2), want 2", got)
	}

	// --- cycle 2: 2 committed + 1 planned = 3 ≤ 3 → allowed (boundary) ---
	d2 := e.CheckCycle(destructiveOps(1), InFlight{}, 10)
	if d2.HaltAll || d2.HaltDestructive {
		t.Fatalf("cycle 2: unexpected halt (2+1=3 == cap 3, must be allowed): %+v", d2)
	}
	e.CommitOps(destructiveOps(1))
	if got := e.WindowTotal(); got != 3 {
		t.Fatalf("cycle 2: windowTotal=%d after CommitOps(1), want 3", got)
	}

	// --- cycle 3: 3 committed + 1 planned = 4 > 3 → HALT (window) ---
	d3 := e.CheckCycle(destructiveOps(1), InFlight{}, 10)
	if !d3.HaltAll {
		t.Fatalf("cycle 3: expected HaltAll (window budget, 3+1=4 > cap 3), got %+v", d3)
	}
	if d3.Reason == "" {
		t.Errorf("cycle 3: halt reason must be non-empty")
	}
	// The window counter must NOT advance on a halted cycle: CheckCycle does not
	// record; only CommitOps does, and the caller suppresses CommitOps on halt.
	if got := e.WindowTotal(); got != 3 {
		t.Errorf("cycle 3: windowTotal=%d after halt (no CommitOps), want 3 (halted ops not counted)", got)
	}

	t.Logf("[AC-Scf7762-2-2] PASS: window accumulated 2→3 across cycles then halted the 4th destructive op")
}

// TestWindowBudget_EvictOld_ReopensBudgetAfterWindowElapses verifies the
// rolling-window decay: once the clock advances past WindowDuration, the old
// committed deletions fall out of the window (evictOld) and budget is available
// again.  This is what makes the budget a *rolling* window rather than a
// lifetime cap.
func TestWindowBudget_EvictOld_ReopensBudgetAfterWindowElapses(t *testing.T) {
	// [AC-Scf7762-2-2] decay half
	clk := &fakeClock{t: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}
	e := newEngineWithClock(Config{
		MaxDestructivePerWindow: 3,
		WindowDuration:          time.Hour,
	}, clk)

	// Fill the window: 3 committed at t0.
	e.CommitOps(destructiveOps(3))
	if got := e.WindowTotal(); got != 3 {
		t.Fatalf("precondition: windowTotal=%d, want 3", got)
	}
	// Budget is now full: even 1 more planned would exceed cap.
	if d := e.CheckCycle(destructiveOps(1), InFlight{}, 10); !d.HaltAll {
		t.Fatalf("with 3 committed, 1 planned must halt (3+1=4 > 3), got %+v", d)
	}

	// Advance the clock just short of the window: the entry is still inside.
	clk.advance(59 * time.Minute)
	if got := e.WindowTotal(); got != 3 {
		t.Fatalf("at +59m the committed entry is still in-window; windowTotal=%d, want 3", got)
	}

	// Advance past the window (+61m total > 60m): the old entry is evicted.
	clk.advance(2 * time.Minute) // total +61m
	if got := e.WindowTotal(); got != 0 {
		t.Fatalf("at +61m the committed entry must be evicted; windowTotal=%d, want 0", got)
	}
	// Budget is available again: a fresh destructive op is allowed.
	if d := e.CheckCycle(destructiveOps(3), InFlight{}, 10); d.HaltAll || d.HaltDestructive {
		t.Errorf("after decay, 3 planned must be allowed again (window empty), got %+v", d)
	}

	t.Logf("[AC-Scf7762-2-2] PASS: evictOld reopened the window budget after WindowDuration elapsed")
}

// TestWindowBudget_PartialDecay_FreesProportionalBudget checks that only the
// entries OUTSIDE the window are evicted, not all history: a later commit stays
// counted while an earlier one decays away.
func TestWindowBudget_PartialDecay_FreesProportionalBudget(t *testing.T) {
	// [AC-Scf7762-2-2] partial-decay
	clk := &fakeClock{t: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}
	e := newEngineWithClock(Config{
		MaxDestructivePerWindow: 3,
		WindowDuration:          time.Hour,
	}, clk)

	e.CommitOps(destructiveOps(2)) // entry A at t0
	clk.advance(40 * time.Minute)
	e.CommitOps(destructiveOps(1)) // entry B at t0+40m
	if got := e.WindowTotal(); got != 3 {
		t.Fatalf("both entries in window; windowTotal=%d, want 3", got)
	}

	// Advance so entry A (t0) is outside the 1h window but entry B (t0+40m) is
	// still inside: total clock = t0 + 40m + 25m = t0 + 65m.  Cutoff = now-1h =
	// t0+5m.  A (t0) < cutoff → evicted; B (t0+40m) ≥ cutoff → kept.
	clk.advance(25 * time.Minute)
	if got := e.WindowTotal(); got != 1 {
		t.Fatalf("only entry B should remain; windowTotal=%d, want 1", got)
	}
	// 1 committed leaves room for 2 more before the cap of 3.
	if d := e.CheckCycle(destructiveOps(2), InFlight{}, 10); d.HaltAll || d.HaltDestructive {
		t.Errorf("1 committed + 2 planned = 3 == cap must be allowed, got %+v", d)
	}
	if d := e.CheckCycle(destructiveOps(3), InFlight{}, 10); !d.HaltAll {
		t.Errorf("1 committed + 3 planned = 4 > cap must halt, got %+v", d)
	}

	t.Logf("[AC-Scf7762-2-2] PASS: partial decay evicted only the out-of-window entry")
}

// TestWindowBudget_ApproveLargeChange_DoesNotBypassWindow confirms at unit level
// that --approve-large-change raises only the per-cycle rate limit and never the
// cumulative window budget (mirrors the CLI test, but on the accumulation path).
func TestWindowBudget_ApproveLargeChange_DoesNotBypassWindow(t *testing.T) {
	// [AC-Scf7762-2-2] approve-does-not-bypass-window
	clk := &fakeClock{t: time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)}
	e := newEngineWithClock(Config{
		MaxDestructivePerCycle:  1,
		MaxDestructivePerWindow: 2,
		WindowDuration:          time.Hour,
		ApproveLargeChange:      100, // approve a huge per-cycle burst
	}, clk)

	// Cycle 1: 2 planned.  Per-cycle limit is 1 but approval raises it to 100, so
	// the rate limit does NOT fire.  The window cap of 2 permits exactly 2.
	if d := e.CheckCycle(destructiveOps(2), InFlight{}, 10); d.HaltAll || d.HaltDestructive {
		t.Fatalf("cycle 1: 2 planned within window cap 2 and approved, must pass, got %+v", d)
	}
	e.CommitOps(destructiveOps(2))

	// Cycle 2: window now full (2 committed).  Even with approval=100, one more
	// planned destructive op exceeds the window cap → HALT.  Approval must NOT
	// bypass the cumulative budget.
	if d := e.CheckCycle(destructiveOps(1), InFlight{}, 10); !d.HaltAll {
		t.Errorf("cycle 2: window full (2/2); approve-large-change must NOT bypass window, expected HaltAll, got %+v", d)
	}

	t.Logf("[AC-Scf7762-2-2] PASS: approve-large-change raised the per-cycle gate but not the window budget")
}

// Backlog #13: only destructive in-flight counts toward the destructive
// per-cycle limit; all in-flight still counts toward the total-change limit.
func TestCycleRateLimit_InFlightDestructiveOnly(t *testing.T) {
	e := New(Config{MaxDestructivePerCycle: 2})
	if d := e.CheckCycle(destructiveOps(1), InFlight{Destructive: 0, Total: 50}, 100); d.HaltAll || d.HaltDestructive {
		t.Errorf("in-flight Creates must not count as destructive; got halt: %s", d.Reason)
	}
	if d := e.CheckCycle(destructiveOps(1), InFlight{Destructive: 2, Total: 2}, 100); !d.HaltDestructive {
		t.Errorf("1 planned + 2 destructive in-flight must exceed max_destructive_per_cycle=2")
	}

	e = New(Config{MaxChangesPerCycleAbsolute: 10})
	if d := e.CheckCycle(destructiveOps(1), InFlight{Total: 10}, 100); !d.HaltDestructive && !d.HaltAll {
		t.Errorf("1 planned + 10 in-flight must exceed max_changes_per_cycle_absolute=10")
	}
}
