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

// Package budget implements the Sakutei change-budget safety mechanism (§13).
//
// Story Scf7762-2 scope (v1a subset of ADR-0014):
//
//   - rate limit (cycle unit): per-cycle destructive-change count vs. absolute
//     and percentage limits.  Destructive operations are weighted more heavily
//     than additive ones.  In-flight Applies count toward the cycle budget.
//
//   - cumulative time-window budget: rolling window (default 24 h) over
//     destructive changes committed across cycles.  Prevents "slowly delete
//     everything 10 items per cycle over 50 cycles" from bypassing the per-cycle
//     gate.  Counter is S3 state (memory-only, daemon lifetime; resets on restart).
//
//   - default halt + on_exceed: when a limit is breached the default action is
//     halt (block all further changes).  on_exceed controls whether ALL changes
//     are blocked ("halt-all") or only the destructive subset
//     ("halt-destructive-only").
//
//   - approve-large-change flag: an explicit per-invocation flag that raises the
//     cycle limit for destructive changes, expressing "I know this will delete N
//     things and I approve".  This is NOT planned_change; it is a simple
//     approval flag, not a scope/kind/window declaration.
//
//   - planned_change lanes (§13.3, Backlog #39, planned.go): ops admitted to a
//     declaration's lane are taken out of the normal budget before it is
//     evaluated, and the normal budget's halt does not stop them.
//
// Out of scope for this Story:
//   - N-of-M approval gates (v1.5, §14.4)
//   - remediation schedule / time-zone auto-switch (v1.5, §14.2)
//   - remediation mode (auto / alert-only) — separate Story Scf7762-3
//   - chaos / combined scenario — Story Scf7762-4
package budget

import (
	"fmt"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/pkg/core"
)

// OnExceedPolicy controls what happens when a budget limit is breached.
type OnExceedPolicy int

const (
	// OnExceedHaltAll stops ALL changes (additive and destructive) when any
	// budget limit is breached.  This is the most conservative option.
	OnExceedHaltAll OnExceedPolicy = iota
	// OnExceedHaltDestructiveOnly stops only destructive changes when the
	// budget limit is breached; additive changes (Create) are still allowed.
	OnExceedHaltDestructiveOnly
)

// DefaultWindowDuration is the default cumulative-budget time window (24 h).
const DefaultWindowDuration = 24 * time.Hour

// Config holds all budget configuration for the Engine.
//
// All limits are optional: a zero value means "unlimited / disabled".
// Enable a limit by setting the corresponding field to a positive value.
type Config struct {
	// MaxDestructivePerCycle is the absolute maximum number of destructive ops
	// allowed in a single cycle (rate limit, §13.1).  0 = unlimited.
	MaxDestructivePerCycle int

	// MaxChangesPerCycleAbsolute is the absolute maximum of ALL change ops
	// (destructive and additive) per cycle.  0 = unlimited.
	MaxChangesPerCycleAbsolute int

	// MaxChangesPerCyclePercent is the maximum percentage of total desired
	// resources that may be changed in a single cycle.  0 = unlimited.
	// Example: 5 means "at most 5% of total desired resources".
	MaxChangesPerCyclePercent float64

	// WindowDuration is the rolling window for the cumulative destructive budget.
	// Defaults to DefaultWindowDuration (24 h) when zero.
	WindowDuration time.Duration

	// MaxDestructivePerWindow is the maximum number of destructive ops allowed
	// within the rolling time window (cumulative budget, §13.2).  0 = unlimited.
	MaxDestructivePerWindow int

	// OnExceed controls which changes are blocked when a budget limit fires.
	// Defaults to OnExceedHaltAll (most conservative) when zero.
	OnExceed OnExceedPolicy

	// ApproveLargeChange is the "explicit intent" override: when > 0 it raises
	// the per-cycle destructive limit to this value for the current invocation.
	// Corresponds to the --approve-large-change N CLI flag.
	// A cycle rate-limit check that would otherwise halt is bypassed if the
	// number of destructive changes does not exceed this value.
	// The cumulative window budget is NOT overridden.
	ApproveLargeChange int

	// now is injectable for testing (zero value → use time.Now).
	now func() time.Time
}

// windowEntry records a single destructive-change event for the rolling window.
type windowEntry struct {
	t time.Time
	n int // number of destructive ops committed in this event
}

// Engine evaluates change budgets for a single shard (scope / daemon lifetime).
//
// The Engine is NOT safe for concurrent use from multiple goroutines.
// It is intended to be called from the single-threaded convergence cycle
// dispatcher in loop.go (or runReconcile) before launching any Apply goroutines.
type Engine struct {
	cfg     Config
	mu      sync.Mutex
	history []windowEntry // time-ordered destructive events (S3 memory)

	// planned_change lanes (§13.3).  laneUsed is keyed by PlannedChange.Key;
	// like history it is S3 memory, restored from S4 when an audit log is
	// attached (AttachAudit, #44).
	planned      []PlannedChange
	laneUsed     map[string]int
	expiredNoted map[string]bool

	// planned_change gates (§14.4 / §14.5, planned_gate.go).  declaredAt is
	// keyed by PlannedChange.Fingerprint, opened and gateNoted likewise;
	// laneFlushed is the part of laneUsed already recorded in S4.
	audit       *audit.Log
	declaredAt  map[string]time.Time
	opened      map[string]bool
	gateNoted   map[string]string
	laneFlushed map[string]int
	// declaredMax holds the max_count of each declaredAt entry (for the
	// withdrawn record); pendingEvents are notices raised outside CheckCycle
	// (SetPlannedChanges), emitted with the next cycle's events.
	declaredMax   map[string]int
	pendingEvents []string
}

// New creates an Engine with the given configuration.
func New(cfg Config) *Engine {
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = DefaultWindowDuration
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	return &Engine{cfg: cfg}
}

// CycleDecision is the result returned by CheckCycle.
type CycleDecision struct {
	// HaltAll is true when ALL changes (additive + destructive) should be blocked.
	HaltAll bool
	// HaltDestructive is true when only destructive changes should be blocked.
	// HaltAll implies HaltDestructive.
	HaltDestructive bool
	// Reason is a human-readable explanation of why the budget fired (non-empty
	// when either halt flag is true).
	Reason string
	// Events are the planned_change notices of this cycle (PlannedChange,
	// PlannedChangeExhausted, PlannedChangeKindMismatch, PlannedChangeExpired).
	Events []string

	// lane holds the ops admitted to planned_change lanes (§13.3).  HaltAll and
	// HaltDestructive are the normal budget's verdict and do not apply to them;
	// laneHaltAll / laneHaltDestructive (set by Merge from the other gates, such
	// as capacity) do.
	lane                             map[string]laneSlot
	laneHaltAll, laneHaltDestructive bool
}

// CheckCycle evaluates whether the planned ops in this cycle breach any budget
// limit.  It consults the cycle rate-limit AND the cumulative time-window
// budget.
//
// Parameters:
//
//	ops           — the full set of ops planned for this cycle (desired changes
//	                from Diff + prune DeleteOps combined).
//	inFlight      — changes currently in-flight (async Apply goroutines from
//	                prior cycles, §13.1): Destructive counts toward the
//	                destructive limit, Total toward the total-change limit.
//	totalDesired  — total number of desired resources this cycle (denominator
//	                for percentage rate-limit).
//
// CheckCycle does NOT record the ops in the window history.  Call CommitOps
// after the ops are actually applied to advance the cumulative counter.
// This separation lets the caller suppress the actual Apply when halted and
// avoids counting aborted operations.
//
// Ops admitted to a planned_change lane (§13.3) are taken out first: the limits
// below see only the remaining ops, and the returned halt does not block the
// lane ops (IsHalted).
func (e *Engine) CheckCycle(ops []core.Op, inFlight InFlight, totalDesired int) CycleDecision {
	e.mu.Lock()
	defer e.mu.Unlock()

	adm := e.admitLanes(ops)
	d := e.checkNormal(adm.normal, inFlight, totalDesired)
	d.lane = adm.lane
	d.Events = adm.events
	return d
}

// InFlight is the number of changes still in flight from earlier cycles
// (async Apply, §8), excluding planned_change lane Applies (§13.3).
type InFlight struct {
	// Destructive is the number of in-flight Applies whose plan contains a
	// destructive op.  It counts toward max_destructive_per_cycle.
	Destructive int
	// Total is the number of all in-flight Applies.  It counts toward
	// max_changes_per_cycle_absolute.
	Total int
}

// checkNormal evaluates the normal budget over ops.  Must be called with e.mu
// held.
func (e *Engine) checkNormal(ops []core.Op, inFlight InFlight, totalDesired int) CycleDecision {
	destructive, _ := countOps(ops)
	// in-flight counts toward the cycle budget (§13.1): Add destructive
	// in-flight to the destructive count for the rate-limit check (not the
	// window budget).  In-flight Creates are not destructive.
	destructiveWithInFlight := destructive + inFlight.Destructive

	// --- Approve-large-change override (cycle rate-limit only) ---------------
	//
	// If the caller passed --approve-large-change N and the number of
	// destructive changes fits within that approval budget, skip the cycle
	// rate-limit check.  The cumulative window budget is checked regardless.
	approvedCycleLimit := 0
	if e.cfg.ApproveLargeChange > 0 {
		approvedCycleLimit = e.cfg.ApproveLargeChange
	}

	// --- Cycle rate-limit checks (§13.1) -------------------------------------

	// Check 1: absolute destructive limit.
	if e.cfg.MaxDestructivePerCycle > 0 {
		effectiveLimit := e.cfg.MaxDestructivePerCycle
		if approvedCycleLimit > effectiveLimit {
			effectiveLimit = approvedCycleLimit
		}
		if destructiveWithInFlight > effectiveLimit {
			reason := fmt.Sprintf(
				"rate-limit (per-cycle): %d destructive ops (+ %d in-flight = %d) exceeds max_destructive_per_cycle=%d (approve-large-change=%d)",
				destructive, inFlight.Destructive, destructiveWithInFlight,
				e.cfg.MaxDestructivePerCycle, e.cfg.ApproveLargeChange,
			)
			return e.haltDecision(reason)
		}
	}

	// Check 2: absolute total-change limit.
	if e.cfg.MaxChangesPerCycleAbsolute > 0 {
		total := len(ops) + inFlight.Total
		if total > e.cfg.MaxChangesPerCycleAbsolute {
			reason := fmt.Sprintf(
				"rate-limit (per-cycle): %d total ops (+ %d in-flight = %d) exceeds max_changes_per_cycle_absolute=%d",
				len(ops), inFlight.Total, total, e.cfg.MaxChangesPerCycleAbsolute,
			)
			return e.haltDecision(reason)
		}
	}

	// Check 3: percentage total-change limit.
	if e.cfg.MaxChangesPerCyclePercent > 0 && totalDesired > 0 {
		total := len(ops)
		pct := float64(total) / float64(totalDesired) * 100
		if pct > e.cfg.MaxChangesPerCyclePercent {
			reason := fmt.Sprintf(
				"rate-limit (per-cycle): %.1f%% of %d desired resources would change (limit=%.1f%%)",
				pct, totalDesired, e.cfg.MaxChangesPerCyclePercent,
			)
			return e.haltDecision(reason)
		}
	}

	// --- Cumulative time-window budget check (§13.2) -------------------------
	if e.cfg.MaxDestructivePerWindow > 0 {
		windowTotal := e.windowTotal()
		if windowTotal+destructive > e.cfg.MaxDestructivePerWindow {
			reason := fmt.Sprintf(
				"cumulative budget (window=%s): %d destructive ops already committed + %d planned = %d exceeds max_destructive_per_window=%d; halt to prevent progressive bulk-delete",
				e.cfg.WindowDuration, windowTotal, destructive,
				windowTotal+destructive,
				e.cfg.MaxDestructivePerWindow,
			)
			// Window budget always uses halt-all regardless of on_exceed policy
			// (§13.2: "halt-all: rendered_revision 全体を凍結し人へ").
			return CycleDecision{
				HaltAll:         true,
				HaltDestructive: true,
				Reason:          reason,
			}
		}
	}

	// No budget limit breached.
	return CycleDecision{}
}

// CommitOps records the given ops in the cumulative time-window history.
//
// Call this AFTER a successful Apply (or group of Applies) so that aborted /
// halted ops are not counted.  Only destructive ops are recorded.
func (e *Engine) CommitOps(ops []core.Op) {
	e.mu.Lock()
	defer e.mu.Unlock()

	destructive, _ := countOps(ops)
	if destructive == 0 {
		return
	}
	e.history = append(e.history, windowEntry{t: e.cfg.now(), n: destructive})
	// Evict entries outside the window immediately to keep memory bounded.
	e.evictOld()
}

// WindowTotal returns the current cumulative destructive count within the
// rolling window.  Useful for status reporting.
func (e *Engine) WindowTotal() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.windowTotal()
}

// windowTotal computes the cumulative destructive count within the window.
// Must be called with e.mu held.
func (e *Engine) windowTotal() int {
	e.evictOld()
	total := 0
	for _, entry := range e.history {
		total += entry.n
	}
	return total
}

// evictOld removes history entries that are outside the rolling window.
// Must be called with e.mu held.
func (e *Engine) evictOld() {
	cutoff := e.cfg.now().Add(-e.cfg.WindowDuration)
	i := 0
	for i < len(e.history) && e.history[i].t.Before(cutoff) {
		i++
	}
	if i > 0 {
		e.history = e.history[i:]
	}
}

// haltDecision builds the appropriate CycleDecision for the configured
// on_exceed policy.
func (e *Engine) haltDecision(reason string) CycleDecision {
	switch e.cfg.OnExceed {
	case OnExceedHaltDestructiveOnly:
		return CycleDecision{
			HaltAll:         false,
			HaltDestructive: true,
			Reason:          reason,
		}
	default: // OnExceedHaltAll (conservative default)
		return CycleDecision{
			HaltAll:         true,
			HaltDestructive: true,
			Reason:          reason,
		}
	}
}

// countOps separates ops into destructive and additive counts.
// An op is destructive when Op.Destructive == true.
func countOps(ops []core.Op) (destructive, additive int) {
	for _, op := range ops {
		if op.Destructive {
			destructive++
		} else {
			additive++
		}
	}
	return
}

// IsHalted returns true if the decision blocks the given op.
// Additive (non-destructive) ops are only blocked when HaltAll is true.
//
// A planned_change lane op is blocked only by the gates merged in from outside
// the change budget (capacity), never by the normal budget's halt.
func (d CycleDecision) IsHalted(op core.Op) bool {
	if d.InLane(op) {
		return d.laneHaltAll || (d.laneHaltDestructive && op.Destructive)
	}
	if d.HaltAll {
		return true
	}
	if d.HaltDestructive && op.Destructive {
		return true
	}
	return false
}

// FilterOps returns the subset of ops that are permitted given this decision.
// Ops not permitted are also returned as the second (blocked) slice so callers
// can log them.
func (d CycleDecision) FilterOps(ops []core.Op) (allowed, blocked []core.Op) {
	for _, op := range ops {
		if d.IsHalted(op) {
			blocked = append(blocked, op)
		} else {
			allowed = append(allowed, op)
		}
	}
	return
}

// Merge combines two gate decisions into the most restrictive one (a halt in
// either halts).  Used to fold the capacity gate (§13.4) into the change
// budget decision so ops are filtered in one place.
//
// planned_change lane ops stay exempt from a decision that carries lanes (the
// change budget) but are halted by a decision without lanes (capacity).
func Merge(a, b CycleDecision) CycleDecision {
	out := CycleDecision{
		HaltAll:         a.HaltAll || b.HaltAll,
		HaltDestructive: a.HaltDestructive || b.HaltDestructive,
		Events:          append(append([]string(nil), a.Events...), b.Events...),
	}
	aAll, aDes := a.laneHalts()
	bAll, bDes := b.laneHalts()
	out.laneHaltAll, out.laneHaltDestructive = aAll || bAll, aDes || bDes
	for _, d := range []CycleDecision{a, b} {
		for k, s := range d.lane {
			if out.lane == nil {
				out.lane = map[string]laneSlot{}
			}
			out.lane[k] = s
		}
	}
	switch {
	case a.Reason != "" && b.Reason != "":
		out.Reason = a.Reason + "; " + b.Reason
	case a.Reason != "":
		out.Reason = a.Reason
	default:
		out.Reason = b.Reason
	}
	return out
}

// laneHalts is how d halts planned_change lane ops: a decision with lanes keeps
// them exempt from its own halt; a decision without lanes (another gate) halts
// them like any other op.
func (d CycleDecision) laneHalts() (all, destructive bool) {
	if d.lane != nil {
		return d.laneHaltAll, d.laneHaltDestructive
	}
	return d.HaltAll, d.HaltDestructive
}
