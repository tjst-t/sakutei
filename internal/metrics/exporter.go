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

package metrics

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

// BudgetLimits are the change-budget limits of the daemon (§13.1, §13.2).  A
// zero limit is unlimited: its limit and remaining are not exported.
type BudgetLimits struct {
	DestructivePerCycle  int
	ChangesPerCycle      int
	DestructivePerWindow int
	Window               time.Duration
}

// Cycle is what one convergence cycle of the daemon established.
type Cycle struct {
	// Number is the cycle number of the daemon log.
	Number int
	// End is when the cycle ended.
	End time.Time
	// Duration is the whole cycle; ObserveDuration its Observe phase
	// (Observe → Diff of every due resource, the early-warning line of §16).
	Duration        time.Duration
	ObserveDuration time.Duration
	// Err is true when the cycle ended in an error.
	Err bool

	// ObserveRan is true when the cycle reached the Observe phase.  A cycle
	// that did not (skipped, inventory halt, intent unreadable, fencing store
	// unreachable, ...) established nothing about reality: its resource
	// counts and ObserveDuration are not exported, the last observing
	// cycle's stay (unknown ≠ absent, §3).
	ObserveRan bool

	// Resources of the cycle's desired (after the claim and fencing):
	// Observed with a diff (drifted), not observable (unknown ≠ absent),
	// and converging (in-flight Apply, or the target says it converges).
	Desired, Observed, Drifted, Unknown, Converging int
	// Oscillating were Observed with a diff whose Apply is stopped (§11.1
	// Tier3, #166).
	Oscillating int
	// IneffectiveApplies are the Applies verified since the previous cycle
	// that reported success but did not change reality (#166).
	IneffectiveApplies int

	// Held is the number of ops a gate held this cycle.
	Held int
	// The gates that halted the cycle (§9.2, §12.4, §13).
	BudgetHalted, CapacityHalted, PruneHalted, InventoryHalted, ExternalInputSkipped bool
	// AlertOnly: the cycle ran in alert-only or frozen mode (§14.1).
	AlertOnly bool

	// BudgetWindowUsed is the destructive ops counted in the rolling window.
	BudgetWindowUsed int
	// ConsecutiveUnreachableSkips and ConsecutiveErrors are the daemon
	// health of #15 (also in --daemon-status).
	ConsecutiveUnreachableSkips int
	ConsecutiveErrors           int
}

// NotifyTarget is the delivery of the notification sinks to one destination
// (#154, §22.3): the counts are requests (POSTs) since the daemon started.
type NotifyTarget struct {
	// Sink is "webhook" or "alertmanager"; URL the destination (redacted).
	Sink, URL string
	// Delivered, Failed (retried) and Rejected (a 4xx, dropped) requests;
	// Dropped Events / alerts (queue overflow).
	Delivered, Failed, Rejected, Dropped int
	// Pending is what waits to be sent; Active the firing alerts resent to
	// an Alertmanager.
	Pending, Active int
	// ConsecutiveFailures is the requests in a row that did not get there.
	ConsecutiveFailures int
	// LastSuccess is when a request last got there (zero: never).
	LastSuccess time.Time
}

// Exporter keeps the last cycle and the counters of the daemon and serves
// them on /metrics.  Record is called by the loop, ServeHTTP by the HTTP
// server; both are safe to call concurrently.
type Exporter struct {
	limits BudgetLimits

	mu     sync.Mutex
	cycles int
	errors int
	// ineffective counts the Applies that did not take (#166).
	ineffective int
	last        *Cycle
	// lastObserve is the last cycle that reached the Observe phase.
	lastObserve *Cycle
	// notify reports the notification sinks per destination (SetNotify).
	notify func() []NotifyTarget
}

// SetNotify makes the Exporter report the notification sinks' delivery per
// destination (#154); f is called on every scrape.
func (e *Exporter) SetNotify(f func() []NotifyTarget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notify = f
}

// notifyFamilies are the families of the notification sinks, one sample per
// destination (labels sink and url).
func notifyFamilies(ts []NotifyTarget) []Family {
	if len(ts) == 0 {
		return nil
	}
	per := func(v func(NotifyTarget) float64) []Sample {
		out := make([]Sample, 0, len(ts))
		for _, t := range ts {
			out = append(out, Sample{Labels: []Label{{"sink", t.Sink}, {"url", t.URL}}, Value: v(t)})
		}
		return out
	}
	var requests []Sample
	for _, t := range ts {
		for _, r := range []struct {
			result string
			n      int
		}{{"delivered", t.Delivered}, {"failed", t.Failed}, {"rejected", t.Rejected}} {
			requests = append(requests, Sample{Labels: []Label{{"sink", t.Sink}, {"url", t.URL}, {"result", r.result}}, Value: float64(r.n)})
		}
	}
	fams := []Family{
		{Name: "sakutei_notify_requests_total", Type: Counter,
			Help:    "Requests the notification sinks sent, per destination, by result: delivered, failed (retried with a growing wait) and rejected (a 4xx other than 408 / 429, dropped) (§22.3).",
			Samples: requests},
		{Name: "sakutei_notify_dropped_total", Type: Counter,
			Help:    "Events / alerts a notification sink dropped because its queue overflowed, per destination.",
			Samples: per(func(t NotifyTarget) float64 { return float64(t.Dropped) })},
		{Name: "sakutei_notify_pending", Type: Gauge,
			Help:    "Events (webhook) / one-shot alerts (alertmanager) waiting to be sent, per destination.",
			Samples: per(func(t NotifyTarget) float64 { return float64(t.Pending) })},
		{Name: "sakutei_notify_up", Type: Gauge,
			Help: "1 unless the last request to the destination failed (a down destination does not stop the others).",
			Samples: per(func(t NotifyTarget) float64 {
				if t.ConsecutiveFailures > 0 {
					return 0
				}
				return 1
			})},
		{Name: "sakutei_notify_consecutive_failures", Type: Gauge,
			Help:    "Requests in a row that did not reach the destination.",
			Samples: per(func(t NotifyTarget) float64 { return float64(t.ConsecutiveFailures) })},
	}
	var active, last []Sample
	for _, t := range ts {
		l := []Label{{"sink", t.Sink}, {"url", t.URL}}
		if t.Sink == "alertmanager" {
			active = append(active, Sample{Labels: l, Value: float64(t.Active)})
		}
		if !t.LastSuccess.IsZero() {
			last = append(last, Sample{Labels: l, Value: float64(t.LastSuccess.UnixNano()) / 1e9})
		}
	}
	if len(active) > 0 {
		fams = append(fams, Family{Name: "sakutei_notify_active_alerts", Type: Gauge,
			Help: "Firing alerts resent to the Alertmanager, per destination.", Samples: active})
	}
	if len(last) > 0 {
		fams = append(fams, Family{Name: "sakutei_notify_last_success_timestamp_seconds", Type: Gauge,
			Help: "Unix time a request last reached the destination.", Samples: last})
	}
	return fams
}

// NewExporter returns an Exporter for a daemon with the given budget limits.
func NewExporter(limits BudgetLimits) *Exporter {
	return &Exporter{limits: limits}
}

// Record records the end of a cycle.
func (e *Exporter) Record(c Cycle) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cycles++
	e.ineffective += c.IneffectiveApplies
	if c.Err {
		e.errors++
	}
	e.last = &c
	if c.ObserveRan {
		e.lastObserve = &c
	}
}

// Families returns the metric families of the current state.  Before the
// first cycle only the counters and the budget limits are exported: the
// gauges of a cycle have no value yet.
func (e *Exporter) Families() []Family {
	e.mu.Lock()
	defer e.mu.Unlock()
	one := func(v float64) []Sample { return []Sample{{Value: v}} }
	b := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	fams := []Family{
		{Name: "sakutei_cycles_total", Type: Counter, Help: "Convergence cycles the daemon has run.", Samples: one(float64(e.cycles))},
		{Name: "sakutei_cycle_errors_total", Type: Counter, Help: "Convergence cycles that ended in an error.", Samples: one(float64(e.errors))},
		{Name: "sakutei_apply_ineffective_total", Type: Counter,
			Help:    "Applies that reported success but did not change reality after the settle: the diff they applied was still observed (§11.1, #166).",
			Samples: one(float64(e.ineffective))},
	}
	if e.notify != nil {
		fams = append(fams, notifyFamilies(e.notify())...)
	}
	lim := e.limits
	if lim.Window > 0 {
		fams = append(fams, Family{Name: "sakutei_budget_window_seconds", Type: Gauge,
			Help: "Length of the rolling window of the cumulative destructive budget (§13.2).", Samples: one(lim.Window.Seconds())})
	}
	if lim.DestructivePerWindow > 0 {
		fams = append(fams, Family{Name: "sakutei_budget_destructive_window_limit", Type: Gauge,
			Help: "Destructive ops allowed in the rolling window (§13.2).", Samples: one(float64(lim.DestructivePerWindow))})
	}
	if lim.DestructivePerCycle > 0 {
		fams = append(fams, Family{Name: "sakutei_budget_destructive_cycle_limit", Type: Gauge,
			Help: "Destructive ops allowed in one cycle (§13.1).", Samples: one(float64(lim.DestructivePerCycle))})
	}
	if lim.ChangesPerCycle > 0 {
		fams = append(fams, Family{Name: "sakutei_budget_changes_cycle_limit", Type: Gauge,
			Help: "Ops allowed in one cycle (§13.1).", Samples: one(float64(lim.ChangesPerCycle))})
	}
	c := e.last
	if c == nil {
		return fams
	}
	fams = append(fams,
		Family{Name: "sakutei_last_cycle", Type: Gauge, Help: "Number of the last convergence cycle.", Samples: one(float64(c.Number))},
		Family{Name: "sakutei_last_cycle_end_timestamp_seconds", Type: Gauge, Help: "Unix time the last cycle ended.",
			Samples: one(float64(c.End.UnixNano()) / 1e9)},
		Family{Name: "sakutei_cycle_duration_seconds", Type: Gauge, Help: "Duration of the last convergence cycle.",
			Samples: one(c.Duration.Seconds())},
		Family{Name: "sakutei_halted", Type: Gauge,
			Help: "1 when the gate halted the last cycle: budget (§13.1), capacity (§13.4), prune (§12.4), inventory (§9.2), external_input (skipped, unreachable with no previous value, §9.2).",
			Samples: []Sample{
				{Labels: []Label{{"gate", "budget"}}, Value: b(c.BudgetHalted)},
				{Labels: []Label{{"gate", "capacity"}}, Value: b(c.CapacityHalted)},
				{Labels: []Label{{"gate", "prune"}}, Value: b(c.PruneHalted)},
				{Labels: []Label{{"gate", "inventory"}}, Value: b(c.InventoryHalted)},
				{Labels: []Label{{"gate", "external_input"}}, Value: b(c.ExternalInputSkipped)},
			}},
		Family{Name: "sakutei_held_ops", Type: Gauge, Help: "Ops held by a gate in the last cycle.", Samples: one(float64(c.Held))},
		Family{Name: "sakutei_alert_only", Type: Gauge, Help: "1 when the last cycle ran in alert-only or frozen mode (§14.1).", Samples: one(b(c.AlertOnly))},
		Family{Name: "sakutei_last_cycle_error", Type: Gauge, Help: "1 when the last cycle ended in an error.", Samples: one(b(c.Err))},
		Family{Name: "sakutei_consecutive_error_cycles", Type: Gauge, Help: "Consecutive cycles that ended in an error.",
			Samples: one(float64(c.ConsecutiveErrors))},
		Family{Name: "sakutei_consecutive_unreachable_skips", Type: Gauge,
			Help:    "Consecutive cycles skipped because an external input was unreachable with no previous value (§9.2).",
			Samples: one(float64(c.ConsecutiveUnreachableSkips))},
		Family{Name: "sakutei_budget_destructive_window_used", Type: Gauge,
			Help: "Destructive ops counted in the rolling window (§13.2).", Samples: one(float64(c.BudgetWindowUsed))},
	)
	if o := e.lastObserve; o != nil {
		// The last cycle that observed: a cycle that ended before Observe
		// does not turn them into zeros ("no drift") — unknown ≠ absent.
		fams = append(fams,
			Family{Name: "sakutei_last_observe_cycle", Type: Gauge,
				Help:    "Number of the last cycle that reached the Observe phase; sakutei_resources and sakutei_observe_duration_seconds are of that cycle.",
				Samples: one(float64(o.Number))},
			Family{Name: "sakutei_last_observe_end_timestamp_seconds", Type: Gauge,
				Help:    "Unix time the last cycle that reached the Observe phase ended.",
				Samples: one(float64(o.End.UnixNano()) / 1e9)},
			Family{Name: "sakutei_observe_duration_seconds", Type: Gauge,
				Help:    "Duration of the Observe phase (Observe and Diff of the due resources) of the last cycle that observed; the early-warning line of the single daemon (§16).",
				Samples: one(o.ObserveDuration.Seconds())},
			Family{Name: "sakutei_resources", Type: Gauge,
				Help: "Resources of the last cycle that observed, by state: desired (after the claim), observed, drifted (diff against desired), unknown (not observable, unknown is not absent), converging, oscillating (drifted, Apply stopped: Applies of the same diff reported success without changing reality, §11.1).",
				Samples: []Sample{
					{Labels: []Label{{"state", "desired"}}, Value: float64(o.Desired)},
					{Labels: []Label{{"state", "observed"}}, Value: float64(o.Observed)},
					{Labels: []Label{{"state", "drifted"}}, Value: float64(o.Drifted)},
					{Labels: []Label{{"state", "unknown"}}, Value: float64(o.Unknown)},
					{Labels: []Label{{"state", "converging"}}, Value: float64(o.Converging)},
					{Labels: []Label{{"state", "oscillating"}}, Value: float64(o.Oscillating)},
				}},
		)
	}
	if lim.DestructivePerWindow > 0 {
		rem := lim.DestructivePerWindow - c.BudgetWindowUsed
		if rem < 0 {
			rem = 0
		}
		fams = append(fams, Family{Name: "sakutei_budget_destructive_window_remaining", Type: Gauge,
			Help: "Destructive ops left in the rolling window (§13.2).", Samples: one(float64(rem))})
	}
	return fams
}

// ServeHTTP writes the metrics in the text exposition format.
func (e *Exporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var buf bytes.Buffer
	if err := Write(&buf, e.Families()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ContentType)
	_, _ = w.Write(buf.Bytes())
}

// Handler serves the Exporter on /metrics only.
func (e *Exporter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", e)
	return mux
}
