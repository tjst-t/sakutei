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

package freeze

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
)

func kinds(t *testing.T, path string) string {
	t.Helper()
	recs, err := audit.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	var ks []string
	for _, r := range recs {
		ks = append(ks, string(r.Kind))
	}
	return strings.Join(ks, ",")
}

func TestStartEndCheck(t *testing.T) {
	dir := t.TempDir()
	state, logPath := filepath.Join(dir, "freeze.json"), filepath.Join(dir, "audit.jsonl")
	log := audit.NewLog(logPath)
	t0 := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)

	if err := Start(state, log, Freeze{By: "alice", Since: t0}); err == nil {
		t.Fatal("a freeze without a reason must be refused")
	}
	if err := Start(state, log, Freeze{Reason: "maint", Since: t0}); err == nil {
		t.Fatal("a freeze without who must be refused")
	}
	if err := Start(state, log, Freeze{By: "alice", Reason: "maint", Since: t0, Until: t0.Add(-time.Minute)}); err == nil {
		t.Fatal("a deadline before the start must be refused")
	}
	if k := kinds(t, logPath); k != "" {
		t.Fatalf("refused freezes must not be recorded: %s", k)
	}

	if err := Start(state, log, Freeze{By: "alice", Reason: "maint", Since: t0, Until: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := Start(state, log, Freeze{By: "bob", Reason: "other", Since: t0.Add(time.Minute)}); err == nil {
		t.Fatal("a second freeze while one is in effect must be refused")
	}
	active, expired, err := Check(state, log, t0.Add(30*time.Minute))
	if err != nil || active == nil || expired != nil || active.By != "alice" || active.Source != SourceCommand {
		t.Fatalf("in effect: %+v %+v %v", active, expired, err)
	}
	active, expired, err = Check(state, log, t0.Add(time.Hour))
	if err != nil || active != nil || expired == nil {
		t.Fatalf("expired: %+v %+v %v", active, expired, err)
	}
	if f, _ := Read(state); f != nil {
		t.Fatalf("an expired freeze must be cleared: %+v", f)
	}
	if _, err := End(state, log, "alice", "", t0.Add(2*time.Hour)); err == nil {
		t.Fatal("ending with no freeze in effect must be refused")
	}

	if err := Start(state, log, Freeze{By: "bob", Reason: "switch", Since: t0.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := End(state, log, "carol", "done", t0.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if k := kinds(t, logPath); k != "freeze.started,freeze.expired,freeze.started,freeze.ended" {
		t.Fatalf("records: %s", k)
	}
	recs, _ := audit.ReadAll(logPath)
	if recs[3].Actor != "carol" || !strings.Contains(recs[3].Detail, "switch") || !strings.Contains(recs[3].Detail, "done") {
		t.Fatalf("end record: %+v", recs[3])
	}
}
