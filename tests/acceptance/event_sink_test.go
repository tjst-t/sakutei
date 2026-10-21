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

// event_sink_test.go — #78: Event の sink（§22.3: 発生源と sink を分ける）。
//
//	[AC-78-1]  テスト用の sink を Config.EventSinks に差すと、ずれ（Drift）・
//	           halt（BudgetHalted・PruneHalted）・prune（Pruned）の Event が届く。
//	           周の要約（CycleSummary）は知らせでないので届かない。
//	[AC-78-2]  --event-log <path> のデーモンは、Event を JSON lines でファイルに
//	           追記する（既存の行は残る）。標準エラーは --log-format のまま（text）。
//	[AC-78-3]  sink がエラーを返しても、デーモンは収束を続ける（ずれを直し、次の周は
//	           converged）。失敗は 1 Event につき 1 行のログ。
package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/eventlog"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
)

// recordingSink keeps every Event handed to it.
type recordingSink struct {
	mu  sync.Mutex
	evs []core.Event
}

func (s *recordingSink) Emit(_ context.Context, ev core.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
	return nil
}

func (s *recordingSink) ofKind(k core.EventKind) []core.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Event
	for _, ev := range s.evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

// firingOf is the Events of kind k in state firing (§22.3: a level Event is
// also resolved once when it is gone).
func (s *recordingSink) firingOf(k core.EventKind) []core.Event {
	var out []core.Event
	for _, ev := range s.ofKind(k) {
		if ev.State == core.EventFiring {
			out = append(out, ev)
		}
	}
	return out
}

// sinkRig is a daemon (reconcile.Run) whose Events also go to sinks.
type sinkRig struct {
	t         *testing.T
	intentDir string
	statePath string
	out       syncBuffer
	errOut    syncBuffer
	stop      func()
}

func newSinkRig(t *testing.T) *sinkRig {
	t.Helper()
	tmp := t.TempDir()
	r := &sinkRig{t: t, intentDir: filepath.Join(tmp, "intent"), statePath: filepath.Join(tmp, "state.json")}
	if err := os.MkdirAll(r.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// start runs the daemon with cfg (IntentDir, Provider, Out, ErrOut filled).
func (r *sinkRig) start(cfg reconcile.Config) {
	cfg.IntentDir = r.intentDir
	cfg.GeneratedDir = filepath.Join(filepath.Dir(r.intentDir), "generated")
	cfg.Provider = fakeprovider.New(r.statePath)
	cfg.Interval = 20 * time.Millisecond
	cfg.Out = &r.out
	cfg.ErrOut = &r.errOut
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconcile.Run(ctx, cfg) }()
	var once sync.Once
	r.stop = func() { once.Do(func() { cancel(); <-done }) }
	r.t.Cleanup(r.stop)
}

// waitFor polls cond until it holds; it fails the test after 10s.
func (r *sinkRig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s; out:\n%s\nerr:\n%s", what, r.out.String(), r.errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedDrift makes db-vm drift: reality size 30, desired 10.
func (r *sinkRig) seedDrift() {
	writeIntentFile(r.t, r.intentDir, map[string]any{
		"resources": []map[string]any{
			{"provider": "fake", "id": "db-vm", "fields": map[string]string{"name": "db-vm", "size": "10"}},
		},
	})
	seedStateAtomic(r.t, r.statePath, map[string]map[string]any{
		"db-vm": {"name": "db-vm", "size": "30"},
	}, "v1.0.0")
}

// seedOrphans makes 2 orphans of 3 (vm-02, vm-03) under PruneWithinScope.
func (r *sinkRig) seedOrphans() {
	seedManyPruneableResources(r.t, r.statePath, 3)
	writeDesiredKeepOneResource(r.t, r.intentDir, "vm-01")
}

func TestEventSink_DriftHaltPruneReachSink(t *testing.T) {
	// [AC-78-1] drift.
	r := newSinkRig(t)
	r.seedDrift()
	rec := &recordingSink{}
	r.start(reconcile.Config{EventSinks: []eventlog.Sink{rec}})
	r.waitFor("Drift Event", func() bool { return len(rec.ofKind(core.EventDrift)) > 0 })
	r.waitFor("cycle 2", func() bool { return strings.Contains(r.out.String(), "sakutei: cycle 2:") })
	r.stop()
	drift := rec.firingOf(core.EventDrift)
	if len(drift) != 1 || drift[0].Resource.Local != "db-vm" || drift[0].Cause != core.CauseTagB ||
		drift[0].Cycle != 1 || drift[0].IntentRevision == "" || drift[0].Time.IsZero() {
		t.Errorf("[AC-78-1] Drift Events at the sink = %+v, want one for db-vm (cause B, cycle 1, revision, time)", drift)
	}
	if got := rec.ofKind(core.EventCycleSummary); len(got) != 0 {
		t.Errorf("[AC-78-1] CycleSummary reached the sink: %+v", got)
	}
	// Standard error still carries the Event (the stream sink).
	if !strings.Contains(r.errOut.String(), "sakutei: DriftEvent: fake//db-vm [cause: B] ") {
		t.Errorf("[AC-78-1] ErrOut lacks the DriftEvent line:\n%s", r.errOut.String())
	}

	// [AC-78-1] prune: the 2 orphans are deleted.
	r = newSinkRig(t)
	r.seedOrphans()
	rec = &recordingSink{}
	r.start(reconcile.Config{EventSinks: []eventlog.Sink{rec}})
	r.waitFor("2 Pruned Events", func() bool { return len(rec.ofKind(core.EventPruned)) >= 2 })
	r.stop()
	pruned := map[string]bool{}
	for _, ev := range rec.ofKind(core.EventPruned) {
		if ev.Cycle != 1 {
			t.Errorf("[AC-78-1] Pruned Event not in cycle 1: %+v", ev)
		}
		pruned[ev.Resource.Local] = true
	}
	if !pruned["vm-02"] || !pruned["vm-03"] || len(pruned) != 2 {
		t.Errorf("[AC-78-1] Pruned Events at the sink for %v, want vm-02 and vm-03", pruned)
	}

	// [AC-78-1] the budget halt (2 orphans, 1 destructive op per cycle), then
	// the prune count-drop halt (reality shrinks to 1 behind our back).
	r = newSinkRig(t)
	r.seedOrphans()
	rec = &recordingSink{}
	r.start(reconcile.Config{
		BudgetConfig: budget.Config{MaxDestructivePerCycle: 1},
		EventSinks:   []eventlog.Sink{rec},
	})
	r.waitFor("BudgetHalted Event", func() bool { return len(rec.ofKind(core.EventBudgetHalted)) > 0 })
	seedManyPruneableResources(t, r.statePath, 1)
	r.waitFor("PruneHalted Event", func() bool { return len(rec.ofKind(core.EventPruneHalted)) > 0 })
	r.stop()
	if got := rec.ofKind(core.EventBudgetHalted); got[0].Message == "" || got[0].Cycle < 1 || got[0].IntentRevision == "" {
		t.Errorf("[AC-78-1] BudgetHalted Event lacks reason/cycle/revision: %+v", got[0])
	}
	if got := rec.ofKind(core.EventPruneHalted); got[0].Attrs["guard"] != "count_drop" || got[0].Cycle < 2 {
		t.Errorf("[AC-78-1] PruneHalted Event = %+v, want the count_drop guard after cycle 1", got[0])
	}
}

func TestEventSink_EventLogFileAppendsJSONLines(t *testing.T) {
	// [AC-78-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	logPath := filepath.Join(tmp, "daemon.log")
	eventPath := filepath.Join(tmp, "events.jsonl")
	earlier := `{"time":"2026-10-20T00:00:00Z","kind":"Pruned","severity":"info","message":"from an earlier run"}`
	if err := os.WriteFile(eventPath, []byte(earlier+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	seedManyPruneableResources(t, statePath, 3)
	writeDesiredKeepOneResource(t, intentDir, "vm-01")

	cmd, waitCmd := startDaemonWithFlags(t, logPath,
		"--intent", intentDir, "--state", statePath, "--interval", "50ms", "--event-log", eventPath)
	ok := waitForLogFile(logPath, "cycle 2:", 10*time.Second)
	_ = cmd.Process.Kill()
	_ = waitCmd()
	log := readFileString(logPath)
	if !ok {
		t.Fatalf("[AC-78-2] daemon did not reach cycle 2; log:\n%s", log)
	}
	data := readFileString(eventPath)
	if !strings.HasPrefix(data, earlier+"\n") {
		t.Fatalf("[AC-78-2] the earlier line was not kept (not appended); file:\n%s", data)
	}
	evs := parseJSONLines(t, data)
	pruned := map[string]bool{}
	for _, ev := range eventsOfKind(evs, string(core.EventPruned)) {
		if ev.Message == "from an earlier run" {
			continue
		}
		if ev.Cycle != 1 || ev.IntentRevision == "" {
			t.Errorf("[AC-78-2] Pruned Event lacks cycle/revision: %+v", ev)
		}
		pruned[ev.Resource] = true
	}
	if !pruned["fake//vm-02"] || !pruned["fake//vm-03"] || len(pruned) != 2 {
		t.Errorf("[AC-78-2] Pruned Events in the file for %v, want fake//vm-02 and fake//vm-03; file:\n%s", pruned, data)
	}
	if got := eventsOfKind(evs, string(core.EventCycleSummary)); len(got) != 0 {
		t.Errorf("[AC-78-2] CycleSummary lines in the event log: %+v", got)
	}
	// Standard error stays in --log-format (text).
	if !strings.Contains(log, "sakutei: pruned fake//vm-02 (reality − desired;") || strings.Contains(log, `{"time"`) {
		t.Errorf("[AC-78-2] daemon log is not the text form; log:\n%s", log)
	}
}

func TestEventSink_FailingSinkDoesNotStopConvergence(t *testing.T) {
	// [AC-78-3]
	var mu sync.Mutex
	calls := 0
	failing := eventlog.SinkFunc(func(context.Context, core.Event) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return errors.New("alertmanager unreachable")
	})
	rec := &recordingSink{}
	r := newSinkRig(t)
	r.seedDrift()
	r.start(reconcile.Config{EventSinks: []eventlog.Sink{failing, rec}})
	// The drift is fixed and a later cycle finds nothing to do.
	r.waitFor("a converged cycle", func() bool {
		return strings.Contains(r.out.String(), "observed=1 converged (no changes)")
	})
	r.stop()

	if got := readFileString(r.statePath); !strings.Contains(got, `"size": "10"`) && !strings.Contains(got, `"size":"10"`) {
		t.Errorf("[AC-78-3] reality not converged to size 10; state:\n%s", got)
	}
	if len(rec.firingOf(core.EventDrift)) != 1 {
		t.Errorf("[AC-78-3] the other sink did not get the Drift Event: %+v", rec.evs)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	log := r.errOut.String()
	if n == 0 || strings.Count(log, "sakutei: warn: event sink: ") != n ||
		!strings.Contains(log, "sakutei: warn: event sink: Drift not delivered: alertmanager unreachable") {
		t.Errorf("[AC-78-3] want one warn line per failed Emit (%d); errOut:\n%s", n, log)
	}
}
