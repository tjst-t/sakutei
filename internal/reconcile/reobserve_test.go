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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func dres(local string, deps ...string) diff.DesiredResource {
	return diff.DesiredResource{ID: core.NewID("fake", core.NewScope(nil), local), DependsOn: deps}
}

func locals(rs []diff.DesiredResource) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.ID.Local)
	}
	return out
}

// fw → host → svc, and app depends on host and on db (db not settled).
var reobserveDesired = []diff.DesiredResource{
	dres("fw"), dres("db"), dres("host", "fw"), dres("svc", "host"), dres("app", "host", "db"), dres("ext", "outside"),
}

func TestReobserveSettled(t *testing.T) {
	// db has a pending plan; host was not observed.
	base := map[string]bool{"fw": true, "svc": true, "app": true, "ext": true}
	got := Settled(reobserveDesired, func(l string) bool { return base[l] })
	want := map[string]bool{"fw": true, "ext": true} // svc and app sit on host (not settled)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Settled = %v, want %v", got, want)
	}
}

func TestReobserveCandidates(t *testing.T) {
	unreached := map[string]bool{"host": true, "svc": true}
	settled := map[string]bool{"fw": true}
	// svc's direct upstream host is not settled yet: only host this round.
	if got := locals(ReobserveCandidates(reobserveDesired, unreached, settled, map[string]bool{"fw": true})); !reflect.DeepEqual(got, []string{"host"}) {
		t.Fatalf("candidates = %v, want [host]", got)
	}
	// Nothing applied: nothing to observe again.
	if got := ReobserveCandidates(reobserveDesired, unreached, settled, map[string]bool{}); len(got) != 0 {
		t.Fatalf("candidates without an Apply = %v, want none", locals(got))
	}
	// After host is applied: svc (indirectly behind fw too).
	settled["host"] = true
	if got := locals(ReobserveCandidates(reobserveDesired, map[string]bool{"svc": true}, settled, map[string]bool{"fw": true})); !reflect.DeepEqual(got, []string{"svc"}) {
		t.Fatalf("candidates = %v, want [svc]", got)
	}
}

func TestReobserveSubDAG(t *testing.T) {
	settled := map[string]bool{"fw": true}
	all := func(string) bool { return true }
	// app also needs db, which is not settled: left out.
	if got := locals(ReobserveSubDAG(reobserveDesired, map[string]bool{"host": true}, settled, all)); !reflect.DeepEqual(got, []string{"host", "svc"}) {
		t.Fatalf("sub-DAG = %v, want [host svc]", got)
	}
	// A dependent that is not eligible (held) stops there.
	if got := locals(ReobserveSubDAG(reobserveDesired, map[string]bool{"host": true}, settled, func(l string) bool { return l != "svc" })); !reflect.DeepEqual(got, []string{"host"}) {
		t.Fatalf("sub-DAG = %v, want [host]", got)
	}
}

func TestReobserverLimits(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	data, _ := json.Marshal(map[string]any{"resources": map[string]any{}, "observe_unknown": map[string]string{"host": "unknown"}})
	if err := os.WriteFile(state, data, 0o600); err != nil {
		t.Fatal(err)
	}
	p := fakeprovider.New(state)
	host := dres("host")

	ro := NewReobserver(ReobserveLimits{MaxPerResource: 2, Time: time.Minute})
	if _, ok, why := ro.Observe(context.Background(), p, host); ok || !strings.Contains(why, "availability=unknown") ||
		!strings.Contains(why, "re-observation limit reached (2 per resource)") {
		t.Fatalf("Observe = ok %v, why %q", ok, why)
	}
	if !ro.Exhausted("host") || ro.Exhausted("svc") {
		t.Fatalf("Exhausted: host %v svc %v", ro.Exhausted("host"), ro.Exhausted("svc"))
	}

	ro = NewReobserver(ReobserveLimits{MaxPerResource: 10, Time: 200 * time.Millisecond})
	start := time.Now()
	if _, ok, why := ro.Observe(context.Background(), p, host); ok || !strings.Contains(why, "time limit reached (200ms per run)") {
		t.Fatalf("Observe = ok %v, why %q", ok, why)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Observe took %s past its 200ms", el)
	}
	if !ro.Exhausted("svc") {
		t.Fatalf("the time is used up for every resource")
	}

	ro = NewReobserver(ReobserveLimits{Off: true})
	if _, ok, why := ro.Observe(context.Background(), p, host); ok || why != "re-observation is off" || ro.Enabled() {
		t.Fatalf("Off: ok %v, why %q", ok, why)
	}

	// Observed: ok, no reason.
	ro = NewReobserver(ReobserveLimits{})
	if obs, ok, why := ro.Observe(context.Background(), p, dres("fw")); !ok || why != "" || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Observe(fw) = %v %v %q", obs.Availability, ok, why)
	}
	if got := ro.Limits(); got.MaxPerResource != DefaultReobserveMax || got.Time != DefaultReobserveTime {
		t.Fatalf("default limits = %+v", got)
	}
}
