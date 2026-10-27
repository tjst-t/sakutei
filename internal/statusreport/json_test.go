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

package statusreport

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// TestReport_ToJSON (#118): every section of the text report is in the JSON
// form, with the same counts.
//
// [AC-114-4-1]
func TestReport_ToJSON(t *testing.T) {
	scope := core.NewScope(map[string]string{"site": "tokyo"})
	b := NewBuilder("ri", "rr", nil)
	b.AddDrifted(core.ID{Provider: "fake", Scope: scope, Local: "a"}, core.CauseTagB, Tier{List: true, ProviderVersion: "v1"}, "diag", nil,
		core.FieldDiff{Field: "size", From: "9", To: "1"})
	b.AddMissing(core.ID{Provider: "fake", Local: "b"}, core.CauseTagA, Tier{}, "", nil, core.FieldDiff{Field: "size", To: "2"})
	b.AddConvergedID(core.ID{Provider: "fake", Local: "c"})
	b.AddUnobservableReason(core.ID{Provider: "fake", Local: "d"}, "observe error: boom")
	b.AddWorkflowHeld(core.ID{Provider: "fake", Local: "e"}, "wf", "承認待ち")
	b.AddUnmanaged(core.ID{Provider: "fake", Local: "z"})
	b.NoteListUnsupported("noimpl")
	b.NoteListError("dnszone", errors.New("boom"))
	b.SetDaemonHealth(DaemonHealth{Cycle: 7, ConsecutiveUnreachableSkips: 3, UnreachableSkipLimit: 3})
	r := b.Build()

	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	data, err := r.JSON(now)
	if err != nil {
		t.Fatal(err)
	}
	var j JSONReport
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	if j.SchemaVersion != 1 || j.GeneratedAt != "2026-10-27T01:02:03Z" || j.Complete {
		t.Errorf("header: %+v", j)
	}
	want := JSONSummary{Conformant: 1, Drift: 2, Missing: 1, Unobservable: 1, Unmanaged: 1, Workflow: 1, ListErrors: 1, Total: 6}
	if j.Summary != want {
		t.Errorf("summary %+v, want %+v", j.Summary, want)
	}
	states := map[string]JSONResource{}
	for _, res := range j.Resources {
		states[res.LocalID] = res
	}
	for local, st := range map[string]string{"a": StateDrift, "b": StateDrift, "c": StateConformant, "d": StateUnobservable, "e": StateWorkflow, "z": StateUnmanaged} {
		if states[local].State != st {
			t.Errorf("%s: state %q, want %q", local, states[local].State, st)
		}
	}
	if a := states["a"]; a.Cause != "B" || a.Scope["site"] != "tokyo" || len(a.Fields) != 1 || a.Fields[0].From != "9" || a.Tier == nil || a.Missing {
		t.Errorf("drift a: %+v", a)
	}
	if !states["b"].Missing || states["d"].ObserveError != "observe error: boom" || states["e"].WorkflowStage != "承認待ち" {
		t.Errorf("b/d/e: %+v %+v %+v", states["b"], states["d"], states["e"])
	}
	if len(j.UnmanagedUndetectable) != 2 || j.UnmanagedUndetectable[0].Provider != "dnszone" ||
		j.UnmanagedUndetectable[0].Kind != UndetectableListError || j.UnmanagedUndetectable[0].Reason != "boom" ||
		j.UnmanagedUndetectable[1].Kind != UndetectableListUnsupported {
		t.Errorf("undetectable: %+v", j.UnmanagedUndetectable)
	}
	if j.Daemon == nil || j.Daemon.Cycle != 7 || !j.Daemon.Prolonged {
		t.Errorf("daemon: %+v", j.Daemon)
	}
}
