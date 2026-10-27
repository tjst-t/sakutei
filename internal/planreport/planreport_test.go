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

package planreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

func TestReport_SummarizeAndJSON(t *testing.T) {
	scope := core.NewScope(map[string]string{"site": "a"})
	r := New(time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC))
	create := NewOperation(core.Op{Kind: core.OpKindCreate, Target: core.NewID("fake", scope, "vm"),
		Cause: core.CauseTagA, Fields: []core.FieldDiff{{Field: "vlan", To: "<pending allocation from pool p>"}}}, false)
	del := NewOperation(core.Op{Kind: core.OpKindDelete, Target: core.NewID("fake", core.NewScope(nil), "old"), Destructive: true}, true)
	del.Blocked, del.BlockedBy, del.BlockReason = true, BlockedByBudget, "rate-limit"
	upd := NewOperation(core.Op{Kind: core.OpKindUpdate, Target: core.NewID("fake", scope, "db")}, false)
	upd.Lane, upd.Task = LanePlannedChange, &Task{Window: "Sat 02:00-04:00"}
	r.Operations = append(r.Operations, create, del, upd)
	r.Summarize()

	want := Summary{Changes: 3, Create: 1, Update: 1, Delete: 1, Destructive: 1, Blocked: 1, PlannedChangeLane: 1, Task: 1}
	if r.Summary != want {
		t.Fatalf("Summary = %+v, want %+v", r.Summary, want)
	}

	data, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"<pending allocation from pool p>"`) {
		t.Errorf("< > must not be escaped:\n%s", s)
	}
	if strings.HasSuffix(s, "\n") {
		t.Errorf("JSON must not end with a newline")
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back["schema_version"] != float64(1) || back["generated_at"] != "2026-10-27T01:02:03Z" {
		t.Errorf("header: %v", back)
	}
	// Empty lists are [] (not null), so a reader can range over them.
	for _, k := range []string{"workflow", "pending_allocations", "unobservable", "prune_halts", "prune_deferred", "composites", "planned_change_events"} {
		if v, ok := back[k].([]any); !ok || len(v) != 0 {
			t.Errorf("%s = %v, want []", k, back[k])
		}
	}
	ops := back["operations"].([]any)
	if sc := ops[1].(map[string]any)["scope"]; sc == nil {
		t.Errorf("an op of the root scope must carry scope {}: %v", ops[1])
	}
}
