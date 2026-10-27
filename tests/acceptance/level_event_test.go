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

// level_event_test.go — #79: ずれ・halt を level（firing / resolved）で出す（§22.3）。
//
//	[AC-79-1]  alert-only で手でずらしたまま N 周回すと、ずれの Event（Drift・
//	           DriftWouldApply）は firing で 1 回だけ出る（count=1・since 付き）。
//	           戻すと resolved が 1 回出る（count は続いた周数）。
//	[AC-79-2]  予算の halt（BudgetHalted）も firing と resolved が 1 回ずつ。
//	[AC-79-3]  しきい値（StuckAfter）を超えて続くと、firing の更新が 1 回出て
//	           stuck（severity=critical）に上がる。
//	[AC-79-4]  デーモンを起こし直すと、続いているずれは改めて firing になる。
package acceptance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// resolvedOf is the Events of kind k in state resolved.
func (s *recordingSink) resolvedOf(k core.EventKind) []core.Event {
	var out []core.Event
	for _, ev := range s.ofKind(k) {
		if ev.State == core.EventResolved {
			out = append(out, ev)
		}
	}
	return out
}

// waitCycle waits until the daemon has finished cycle n (its summary line).
func (r *sinkRig) waitCycle(n int) {
	r.t.Helper()
	marker := fmt.Sprintf("sakutei: cycle %d: ", n)
	r.waitFor(marker, func() bool {
		return strings.Contains(r.out.String(), marker) || strings.Contains(r.errOut.String(), marker)
	})
}

// lastCycle is the highest cycle number with a summary line so far.
func (r *sinkRig) lastCycle() int {
	last := 0
	for _, line := range strings.Split(r.out.String(), "\n") {
		var n int
		if _, err := fmt.Sscanf(line, "sakutei: cycle %d:", &n); err == nil && n > last {
			last = n
		}
	}
	return last
}

func alertOnly() remediation.Config { return remediation.Config{Mode: remediation.ModeAlertOnly} }

func TestLevelEvent_AlertOnlyDriftFiresOnceAndResolvesOnce(t *testing.T) {
	// [AC-79-1]
	const n = 6
	r := newSinkRig(t)
	r.seedDrift()
	rec := &recordingSink{}
	r.start(reconcile.Config{Remediation: alertOnly(), StuckAfter: 100, EventSinks: []eventlog.Sink{rec}})
	r.waitCycle(n)

	for _, k := range []core.EventKind{core.EventDrift, core.EventDriftWouldApply} {
		fired := rec.firingOf(k)
		if len(fired) != 1 {
			t.Fatalf("[AC-79-1] %s firing Events after %d cycles = %d, want 1: %+v", k, n, len(fired), fired)
		}
		ev := fired[0]
		if ev.Resource.Local != "db-vm" || ev.Count != 1 || ev.Since.IsZero() || ev.Cycle != 1 ||
			ev.Class != core.EventTransient {
			t.Errorf("[AC-79-1] %s firing = %+v, want db-vm, count=1, since, cycle 1, transient", k, ev)
		}
		if got := rec.resolvedOf(k); len(got) != 0 {
			t.Errorf("[AC-79-1] %s resolved while the drift holds: %+v", k, got)
		}
	}
	// The cycle's own text line still says it every cycle; the Event does not.
	if c := strings.Count(r.out.String(), "DRIFT EVENT: would be UPDATE fake//db-vm"); c != 1 {
		t.Errorf("[AC-79-1] DRIFT EVENT lines = %d, want 1:\n%s", c, r.out.String())
	}

	// Put reality back by hand: the drift is gone.
	seedStateAtomic(t, r.statePath, map[string]map[string]any{
		"db-vm": {"name": "db-vm", "size": "10"},
	}, "v1.0.0")
	r.waitFor("resolved Events", func() bool {
		return len(rec.resolvedOf(core.EventDrift)) > 0 && len(rec.resolvedOf(core.EventDriftWouldApply)) > 0
	})
	r.waitCycle(r.lastCycle() + 2)
	r.stop()

	for _, k := range []core.EventKind{core.EventDrift, core.EventDriftWouldApply} {
		res := rec.resolvedOf(k)
		if len(res) != 1 {
			t.Fatalf("[AC-79-1] %s resolved Events = %d, want 1: %+v", k, len(res), res)
		}
		if res[0].Count < n || !res[0].Since.Equal(rec.firingOf(k)[0].Since) || res[0].Resource.Local != "db-vm" {
			t.Errorf("[AC-79-1] %s resolved = %+v, want db-vm with count >= %d and the firing's since", k, res[0], n)
		}
		if len(rec.firingOf(k)) != 1 {
			t.Errorf("[AC-79-1] %s fired again after it resolved: %+v", k, rec.firingOf(k))
		}
	}
	if !strings.Contains(r.errOut.String(), "RESOLVED DriftWouldApply fake//db-vm") {
		t.Errorf("[AC-79-1] no RESOLVED text line; errOut:\n%s", r.errOut.String())
	}
}

func TestLevelEvent_BudgetHaltFiresOnceAndResolvesOnce(t *testing.T) {
	// [AC-79-2]
	const n = 5
	r := newSinkRig(t)
	r.seedOrphans() // 2 orphans under PruneWithinScope
	rec := &recordingSink{}
	r.start(reconcile.Config{
		BudgetConfig: budget.Config{MaxDestructivePerCycle: 1},
		StuckAfter:   100,
		EventSinks:   []eventlog.Sink{rec},
	})
	r.waitCycle(n)
	fired := rec.firingOf(core.EventBudgetHalted)
	if len(fired) != 1 || fired[0].Count != 1 || fired[0].Since.IsZero() {
		t.Fatalf("[AC-79-2] BudgetHalted firing after %d cycles = %+v, want 1 with count=1 and since", n, fired)
	}
	if c := strings.Count(r.errOut.String(), "BUDGET HALTED"); c != 1 {
		t.Errorf("[AC-79-2] BUDGET HALTED lines = %d, want 1:\n%s", c, r.errOut.String())
	}

	// Declare the orphans: nothing to delete, the halt is gone.
	writeDesiredKeepResources(t, r.intentDir, "vm-01", "vm-02", "vm-03")
	r.waitFor("BudgetHalted resolved", func() bool { return len(rec.resolvedOf(core.EventBudgetHalted)) > 0 })
	r.waitCycle(r.lastCycle() + 2)
	r.stop()

	if res := rec.resolvedOf(core.EventBudgetHalted); len(res) != 1 || res[0].Count < n {
		t.Errorf("[AC-79-2] BudgetHalted resolved = %+v, want 1 with count >= %d", res, n)
	}
	if got := rec.firingOf(core.EventBudgetHalted); len(got) != 1 {
		t.Errorf("[AC-79-2] BudgetHalted firing = %d, want still 1", len(got))
	}
	if got := readFileString(r.statePath); !strings.Contains(got, "vm-02") || !strings.Contains(got, "vm-03") {
		t.Errorf("[AC-79-2] an orphan was deleted although the budget halted; state:\n%s", got)
	}
}

func TestLevelEvent_PromotedToStuckAfterThreshold(t *testing.T) {
	// [AC-79-3]
	const stuckAfter = 3
	r := newSinkRig(t)
	r.seedDrift()
	rec := &recordingSink{}
	r.start(reconcile.Config{Remediation: alertOnly(), StuckAfter: stuckAfter, EventSinks: []eventlog.Sink{rec}})
	r.waitCycle(stuckAfter + 4)
	r.stop()

	fired := rec.firingOf(core.EventDriftWouldApply)
	if len(fired) != 2 {
		t.Fatalf("[AC-79-3] DriftWouldApply firing = %d, want 2 (first, then the stuck update): %+v", len(fired), fired)
	}
	if fired[0].Class != core.EventTransient || fired[0].Count != 1 {
		t.Errorf("[AC-79-3] first firing = %+v, want transient count=1", fired[0])
	}
	up := fired[1]
	if up.Class != core.EventStuck || up.Count != stuckAfter || up.Severity != core.SeverityCritical ||
		!up.Since.Equal(fired[0].Since) || up.Cycle != stuckAfter {
		t.Errorf("[AC-79-3] stuck update = %+v, want stuck, count=%d, critical, the same since, cycle %d", up, stuckAfter, stuckAfter)
	}
	if !strings.Contains(r.out.String(), fmt.Sprintf("[firing stuck count=%d since=", stuckAfter)) {
		t.Errorf("[AC-79-3] the text line does not say stuck:\n%s", r.out.String())
	}
}

func TestLevelEvent_RestartFiresAgain(t *testing.T) {
	// [AC-79-4]
	r := newSinkRig(t)
	r.seedDrift()
	rec := &recordingSink{}
	cfg := reconcile.Config{Remediation: alertOnly(), StuckAfter: 100, EventSinks: []eventlog.Sink{rec}}
	r.start(cfg)
	r.waitCycle(3)
	r.stop()
	if got := rec.firingOf(core.EventDriftWouldApply); len(got) != 1 {
		t.Fatalf("[AC-79-4] firing before the restart = %d, want 1", len(got))
	}

	// Restart: the level state is S3 (memory), so the drift that still holds
	// fires afresh — never lost.
	r.out.Reset()
	r.errOut.Reset()
	r.start(cfg)
	r.waitCycle(3)
	r.stop()
	fired := rec.firingOf(core.EventDriftWouldApply)
	if len(fired) != 2 {
		t.Fatalf("[AC-79-4] firing after the restart = %d, want 2 (once per daemon life): %+v", len(fired), fired)
	}
	if fired[1].Count != 1 || fired[1].Cycle != 1 || !fired[1].Since.After(fired[0].Since) {
		t.Errorf("[AC-79-4] firing after the restart = %+v, want count=1 at cycle 1 with a new since", fired[1])
	}
	if got := rec.resolvedOf(core.EventDriftWouldApply); len(got) != 0 {
		t.Errorf("[AC-79-4] resolved although the drift holds: %+v", got)
	}
}
