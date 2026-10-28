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
	"path/filepath"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func dur(d time.Duration) *time.Duration { return &d }
func num(n int) *int                     { return &n }

func TestResolveApplyCheck(t *testing.T) {
	res := diff.DesiredResource{ID: core.NewID("redfish", core.NewScope(nil), "node1")}
	types := map[string]diff.ApplyCheck{"redfish": {Settle: dur(time.Minute), MaxIneffectiveApplies: num(5)}}
	cases := []struct {
		name  string
		res   diff.DesiredResource
		flags ApplyCheckFlags
		want  ApplyCheckLimits
	}{
		{"defaults: the provider's settle", diff.DesiredResource{ID: core.NewID("fake", core.NewScope(nil), "x")}, ApplyCheckFlags{},
			ApplyCheckLimits{Settle: 15 * time.Second, MaxApplies: DefaultMaxIneffectiveApplies}},
		{"the type's apply_check", res, ApplyCheckFlags{},
			ApplyCheckLimits{Settle: time.Minute, MaxApplies: 5}},
		{"the resource's wins over the type's", diff.DesiredResource{ID: res.ID, ApplyCheck: &diff.ApplyCheck{MaxIneffectiveApplies: num(8)}}, ApplyCheckFlags{},
			ApplyCheckLimits{Settle: time.Minute, MaxApplies: 8}},
		{"flags fill what the intent leaves", diff.DesiredResource{ID: core.NewID("fake", core.NewScope(nil), "x")},
			ApplyCheckFlags{Settle: dur(time.Second), MaxApplies: num(9), MaxDuration: dur(time.Hour)},
			ApplyCheckLimits{Settle: time.Second, MaxApplies: 9, MaxDuration: time.Hour}},
		{"flag vs intent: the most restrictive wins", res,
			ApplyCheckFlags{Settle: dur(time.Second), MaxApplies: num(2)},
			ApplyCheckLimits{Settle: time.Minute, MaxApplies: 2}},
		{"flag vs intent: a flag cannot relax the intent", res,
			ApplyCheckFlags{Settle: dur(time.Hour), MaxApplies: num(50)},
			ApplyCheckLimits{Settle: time.Hour, MaxApplies: 5}},
		{"a time limit beats no time limit", diff.DesiredResource{ID: res.ID, ApplyCheck: &diff.ApplyCheck{MaxIneffectiveDuration: dur(0)}},
			ApplyCheckFlags{MaxDuration: dur(10 * time.Minute)},
			ApplyCheckLimits{Settle: time.Minute, MaxApplies: 5, MaxDuration: 10 * time.Minute}},
	}
	for _, c := range cases {
		if got := ResolveApplyCheck(c.res, types, c.flags, 15*time.Second); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestApplyChecker_CountStopRelease(t *testing.T) {
	c := NewApplyChecker()
	id := core.NewID("fake", core.NewScope(nil), "db")
	lim := ApplyCheckLimits{MaxApplies: 2}
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	if r := c.Ineffective(id, "fp1", "size", lim, now); r.Stopped || r.Applies != 1 {
		t.Fatalf("first: %+v", r)
	}
	// A different diff starts over.
	if r := c.Ineffective(id, "fp2", "size", lim, now); r.Stopped || r.Applies != 1 {
		t.Fatalf("other diff: %+v", r)
	}
	if r := c.Ineffective(id, "fp2", "size", lim, now.Add(time.Second)); !r.Stopped || r.Reason != StopMaxApplies {
		t.Fatalf("limit: %+v", r)
	}
	if _, stopped, _ := c.Gate(id, "fp2"); !stopped {
		t.Fatalf("same diff must stay stopped")
	}
	if _, stopped, released := c.Gate(id, "fp3"); stopped || released == nil {
		t.Fatalf("a changed diff releases it: stopped=%v released=%v", stopped, released)
	}
	// The time limit.
	lim = ApplyCheckLimits{MaxApplies: 100, MaxDuration: time.Minute}
	c.Ineffective(id, "fp4", "size", lim, now)
	if r := c.Ineffective(id, "fp4", "size", lim, now.Add(2*time.Minute)); !r.Stopped || r.Reason != StopMaxDuration {
		t.Fatalf("duration: %+v", r)
	}
	if was := c.Settled(id); was == nil || len(c.Records()) != 0 {
		t.Fatalf("settled: %v %v", was, c.Records())
	}

	// The state file carries the record and the release requests.
	path := filepath.Join(t.TempDir(), "apply-check.json")
	c.Ineffective(id, "fp5", "size", ApplyCheckLimits{MaxApplies: 1}, now)
	if _, _, err := c.Sync(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadApplyChecker(path)
	if err != nil || !loaded.IsStopped(id) {
		t.Fatalf("loaded: %v %+v", err, loaded.Records())
	}
	if err := UpdateApplyCheckState(path, func(st *ApplyCheckState) error {
		st.Releases = append(st.Releases, ApplyReleaseJSON{Resource: "db", Approver: "alice"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	released, requests, err := loaded.Sync(path)
	if err != nil || len(released) != 1 || len(requests) != 1 || loaded.IsStopped(id) {
		t.Fatalf("sync: %v %v %v", released, requests, err)
	}
	if st, _ := ReadApplyCheckState(path); len(st.Releases) != 0 || len(st.Resources) != 0 {
		t.Fatalf("after sync: %+v", st)
	}
}

func TestApplyFingerprint_IgnoresCauseAndDiagnostic(t *testing.T) {
	res := diff.DesiredResource{ID: core.NewID("fake", core.NewScope(nil), "db"), Fields: map[string]string{"size": "10"}}
	op := core.Op{Kind: core.OpKindUpdate, Cause: core.CauseTagA, Diagnostic: "a",
		Fields: []core.FieldDiff{{Field: "size", From: "30", To: "10"}}}
	op2 := op
	op2.Cause, op2.Diagnostic = core.CauseTagB, "b"
	if ApplyFingerprint(res, []core.Op{op}) != ApplyFingerprint(res, []core.Op{op2}) {
		t.Fatalf("the cause and the diagnostic must not change the fingerprint")
	}
	op2.Fields = []core.FieldDiff{{Field: "size", From: "31", To: "10"}}
	if ApplyFingerprint(res, []core.Op{op}) == ApplyFingerprint(res, []core.Op{op2}) {
		t.Fatalf("a different observed value is a different diff")
	}
}
