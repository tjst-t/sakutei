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

package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// levelRig is a Level whose output is recorded.
type levelRig struct {
	l   *Level
	w   io.Writer
	evs []core.Event
	now time.Time
}

func newLevelRig(stuckAfter int) *levelRig {
	r := &levelRig{now: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
	r.l = NewLevel(stuckAfter)
	r.l.now = func() time.Time { return r.now }
	sink := SinkFunc(func(_ context.Context, ev core.Event) error { r.evs = append(r.evs, ev); return nil })
	r.w = r.l.Wrap(Route(context.Background(), &bytes.Buffer{}, []Sink{sink}, nil))
	return r
}

// cycle raises evs as one cycle and ends it; gone resolves what did not recur.
func (r *levelRig) cycle(gone bool, evs ...core.Event) {
	r.now = r.now.Add(10 * time.Second)
	for _, ev := range evs {
		Emit(r.w, ev)
	}
	r.l.EndCycle(r.w, 0, func(core.Event) bool { return gone })
}

var vm = core.NewID("fake", core.NewScope(nil), "vm-1")

func applyFailed(msg string, attrs map[string]string) core.Event {
	return core.Event{Kind: core.EventApplyFailed, Severity: core.SeverityWarning, Resource: vm, Message: msg, Attrs: attrs}
}

func TestLevel_FiresOnceUpdatesOnChangeResolvesOnce(t *testing.T) {
	r := newLevelRig(100)
	r.cycle(true, applyFailed("boom", nil))
	r.cycle(true, applyFailed("boom", nil))
	r.cycle(true, applyFailed("boom", nil), applyFailed("boom", nil)) // twice in one cycle: one count
	r.cycle(true, applyFailed("other", nil))                          // content changed: update
	r.cycle(true)                                                     // gone
	r.cycle(true)
	if len(r.evs) != 3 {
		t.Fatalf("events = %d, want 3 (firing, update, resolved): %+v", len(r.evs), r.evs)
	}
	first, up, res := r.evs[0], r.evs[1], r.evs[2]
	if first.State != core.EventFiring || first.Count != 1 || first.Class != core.EventTransient {
		t.Errorf("first = %+v", first)
	}
	if up.State != core.EventFiring || up.Count != 4 || up.Message != "other" || !up.Since.Equal(first.Since) {
		t.Errorf("update = %+v, want count 4 with the first's since", up)
	}
	if res.State != core.EventResolved || res.Count != 4 || !res.Since.Equal(first.Since) || res.Resource.String() != vm.String() ||
		res.Severity != core.SeverityInfo {
		t.Errorf("resolved = %+v", res)
	}
}

func TestLevel_NotResolvedWhileUnseen(t *testing.T) {
	r := newLevelRig(100)
	r.cycle(false, applyFailed("boom", nil))
	r.cycle(false) // not looked at (in flight, skipped cycle): keeps firing
	r.cycle(false)
	if len(r.evs) != 1 || r.l.Active() != 1 {
		t.Fatalf("events = %+v, active = %d; want only the firing, still active", r.evs, r.l.Active())
	}
	r.cycle(false, applyFailed("boom", nil)) // recurs: same firing, count 2, not re-sent
	if len(r.evs) != 1 {
		t.Fatalf("re-sent a firing that did not change: %+v", r.evs)
	}
}

func TestLevel_StuckAfterThresholdAndPermanentAtOnce(t *testing.T) {
	r := newLevelRig(3)
	for i := 0; i < 5; i++ {
		r.cycle(true, applyFailed("boom", nil))
	}
	if len(r.evs) != 2 || r.evs[1].Class != core.EventStuck || r.evs[1].Count != 3 ||
		r.evs[1].Severity != core.SeverityCritical {
		t.Fatalf("events = %+v, want firing then the stuck update at count 3", r.evs)
	}

	p := newLevelRig(3)
	p.cycle(true, applyFailed("permission denied", map[string]string{AttrFailure: FailurePermanent}))
	p.cycle(true, applyFailed("permission denied", map[string]string{AttrFailure: FailurePermanent}))
	if len(p.evs) != 1 || p.evs[0].Class != core.EventStuck || p.evs[0].Severity != core.SeverityCritical {
		t.Fatalf("permanent = %+v, want one stuck critical firing", p.evs)
	}
	c := newLevelRig(3)
	c.cycle(true, core.Event{Kind: core.EventCapacityHalted, Severity: core.SeverityWarning, Message: "pool full"})
	if len(c.evs) != 1 || c.evs[0].Class != core.EventStuck {
		t.Fatalf("capacity = %+v, want stuck at once", c.evs)
	}
}

func TestLevel_VolatileContentIsNotAChange(t *testing.T) {
	r := newLevelRig(100)
	drift := func(summary, observed string) core.Event {
		return core.Event{Kind: core.EventDrift, Resource: vm, Cause: core.CauseTagB, Message: summary,
			Attrs: map[string]string{"op": "UPDATE", "observed": observed}}
	}
	r.cycle(true, drift("no previous snapshot", "aaa"))
	r.cycle(true, drift("prev snapshot at t1 matches", "aaa"))
	r.cycle(true, drift("prev snapshot at t2 matches", "aaa"))
	if len(r.evs) != 1 {
		t.Fatalf("a drift that merely holds was re-sent: %+v", r.evs)
	}
	r.cycle(true, drift("size was 30 now 40", "bbb")) // reality moved again
	if len(r.evs) != 2 || r.evs[1].Message != "size was 30 now 40" {
		t.Fatalf("a drift that moved was not updated: %+v", r.evs)
	}
}

func TestLevel_EdgeEventsPass(t *testing.T) {
	r := newLevelRig(100)
	r.cycle(true, core.Event{Kind: core.EventPruned, Resource: vm}, core.Event{Kind: core.EventPruned, Resource: vm})
	r.cycle(true, core.Event{Kind: core.EventWorkflowHalted, Message: "x"})
	if len(r.evs) != 3 || r.evs[0].State != "" {
		t.Fatalf("edge Events = %+v, want all 3 as is", r.evs)
	}
}

func TestLevel_JSONCarriesStateSinceCount(t *testing.T) {
	since := time.Date(2026, 10, 21, 1, 2, 3, 0, time.UTC)
	b, err := json.Marshal(core.Event{Kind: core.EventDrift, State: core.EventFiring, Since: since, Count: 4, Class: core.EventStuck})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state":"firing"`, `"since":"2026-10-21T01:02:03Z"`, `"count":4`, `"class":"stuck"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON %s lacks %s", b, want)
		}
	}
	b, _ = json.Marshal(core.Event{Kind: core.EventPruned})
	if strings.Contains(string(b), "state") || strings.Contains(string(b), "since") {
		t.Errorf("an edge Event carries level fields: %s", b)
	}
}
