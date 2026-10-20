// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package loader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
)

func loadPlannedYAML(t *testing.T, body string) (Intent, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "intent.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadIntent(dir)
}

// §13.3 の例をそのまま読める。
func TestLoadPlannedChange_SpecExample(t *testing.T) {
	in, err := loadPlannedYAML(t, `planned_change:
  scope:
    site: hpc-a
    cluster: gpu-pool
  kind: reimage
  max_count: 500
  window: "2026-08-01 00:00 .. 2026-08-01 06:00"
  approval:
    pr: 1423
    approvers:
      - alice
      - bob
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.PlannedChanges) != 1 {
		t.Fatalf("got %d declarations", len(in.PlannedChanges))
	}
	pc := in.PlannedChanges[0]
	if pc.Kind != budget.PlannedReimage || pc.MaxCount != 500 || pc.Scope.Labels["cluster"] != "gpu-pool" {
		t.Errorf("got %+v", pc)
	}
	if !pc.Start.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) || pc.End.Sub(pc.Start) != 6*time.Hour {
		t.Errorf("window %v .. %v", pc.Start, pc.End)
	}
	if pc.Approval.PR != 1423 || strings.Join(pc.Approval.Approvers, ",") != "alice,bob" {
		t.Errorf("approval %+v", pc.Approval)
	}
}

func TestLoadPlannedChange_Rejects(t *testing.T) {
	const window = "  window: \"2026-08-01 00:00 .. 2026-08-01 06:00\"\n"
	decl := func(scope, kind, maxCount, window, extra string) string {
		return "planned_change:\n" + scope + "  kind: " + kind + "\n  max_count: " + maxCount + "\n" + window + extra
	}
	scope := "  scope:\n    a: b\n"
	if _, err := loadPlannedYAML(t, decl(scope, "delete", "1", window, "")); err != nil {
		t.Fatalf("the valid base declaration must load: %v", err)
	}
	for name, body := range map[string]string{
		"unknown kind": decl(scope, "decommission", "1", window, ""),
		"no scope":     decl("", "delete", "1", window, ""),
		"max_count 0":  decl(scope, "delete", "0", window, ""),
		"bad window":   decl(scope, "delete", "1", "  window: tomorrow\n", ""),
		"no window":    decl(scope, "delete", "1", "", ""),
		"unknown key":  decl(scope, "delete", "1", window, "  force: true\n"),
		"twice": "planned_change:\n  - scope:\n      a: b\n    kind: delete\n    max_count: 1\n    window: \"2026-08-01 00:00 .. 2026-08-01 06:00\"\n" +
			"  - scope:\n      a: b\n    kind: delete\n    max_count: 2\n    window: \"2026-08-01 00:00 .. 2026-08-01 06:00\"\n",
	} {
		if _, err := loadPlannedYAML(t, body); err == nil {
			t.Errorf("%s: want a load error", name)
		}
	}
}
