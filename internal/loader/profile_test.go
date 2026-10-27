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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
)

func loadJSONFiles(t *testing.T, files map[string]map[string]any) (Intent, error) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return LoadIntent(dir)
}

// pcIntent is an intent with one planned_change (extra keys merged in) and
// the given top-level keys.
func pcIntent(extra map[string]any, top map[string]any) map[string]any {
	pc := map[string]any{
		"scope": map[string]string{"pool": "decom"}, "kind": "delete", "max_count": 10,
		"window": "2026-08-01 00:00 .. 2026-08-01 06:00",
	}
	for k, v := range extra {
		pc[k] = v
	}
	out := map[string]any{"planned_change": pc}
	for k, v := range top {
		out[k] = v
	}
	return out
}

func loadGate(t *testing.T, extra, top map[string]any) (budget.PlannedGate, Intent) {
	t.Helper()
	in, err := loadJSONFiles(t, map[string]map[string]any{"intent.json": pcIntent(extra, top)})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.PlannedChanges) != 1 {
		t.Fatalf("got %d declarations", len(in.PlannedChanges))
	}
	return in.PlannedChanges[0].Gate, in
}

func approvers(ids ...string) map[string]any {
	return map[string]any{"pr": 7, "approvers": ids}
}

// team（既定）: planned_change は承認者 1 人を要する。
func TestProfile_TeamDefault_SingleApproval(t *testing.T) {
	g, in := loadGate(t, nil, nil)
	if in.Profile != ProfileTeam || g.Profile != "team" {
		t.Fatalf("default profile = %q / %q, want team", in.Profile, g.Profile)
	}
	if !strings.Contains(g.Unapproved, "0 of the 1 approver(s)") {
		t.Errorf("team without approvers: Unapproved = %q", g.Unapproved)
	}
	if g, _ := loadGate(t, map[string]any{"approval": approvers("alice")}, nil); g.Unapproved != "" || g.CoolingOff != 0 {
		t.Errorf("team with one approver: %+v", g)
	}
}

// enterprise: requirements を書かなくても N-of-M + separation を要する。
func TestProfile_Enterprise_RequiresNofMAndSeparation(t *testing.T) {
	ent := map[string]any{"profile": "enterprise"}
	cases := []struct {
		name  string
		extra map[string]any
		want  string // substring of Unapproved; "" = approved
	}{
		{"one approver", map[string]any{"applicant": "carol", "approval": approvers("alice")}, "1 of the 2 approver(s) required by the enterprise profile"},
		{"no applicant", map[string]any{"approval": approvers("alice", "bob")}, "needs the declaration's applicant"},
		{"self approval", map[string]any{"applicant": "carol", "approval": approvers("carol", "alice")}, "carol is the applicant"},
		{"same approver twice", map[string]any{"applicant": "carol", "approval": approvers("alice", "alice")}, "1 of the 2"},
		{"two others", map[string]any{"applicant": "carol", "approval": approvers("alice", "bob")}, ""},
	}
	for _, c := range cases {
		g, _ := loadGate(t, c.extra, ent)
		if c.want == "" {
			if g.Unapproved != "" || !strings.Contains(g.Approved, "separation applicant_ne_approver") {
				t.Errorf("%s: want approved with separation, got %+v", c.name, g)
			}
			continue
		}
		if !strings.Contains(g.Unapproved, c.want) {
			t.Errorf("%s: Unapproved = %q, want %q", c.name, g.Unapproved, c.want)
		}
	}
}

// 個別の宣言はプロファイルに優先する。
func TestProfile_DeclarationWins(t *testing.T) {
	// enterprise でも、宣言の requirements が 1 人なら 1 人で通る。
	g, _ := loadGate(t, map[string]any{
		"approval":     approvers("alice"),
		"requirements": map[string]any{"quorum": map[string]any{"n": 1, "of": 1}},
	}, map[string]any{"profile": "enterprise"})
	if g.Unapproved != "" {
		t.Errorf("enterprise + explicit 1-of-1: %q", g.Unapproved)
	}
	// solo でも、宣言の requirements（N-of-M・役割）は効く。
	g, _ = loadGate(t, map[string]any{
		"approval": map[string]any{"approvers": []any{
			map[string]any{"id": "alice", "roles": []string{"platform-admin"}}, "bob"}},
		"requirements": map[string]any{"quorum": map[string]any{"n": 2, "of": 3}, "roles": []string{"platform-admin"}},
	}, map[string]any{"profile": "solo"})
	if !strings.Contains(g.Unapproved, "1 of the 2 approver(s) required by the declaration's requirements") {
		t.Errorf("solo + explicit 2-of-3 with roles: %q", g.Unapproved)
	}
	// solo の cooling_off は宣言の cooling_off が上書きする（0 で無効）。
	g, _ = loadGate(t, map[string]any{"cooling_off": "0s"}, map[string]any{"profile": "solo", "solo": map[string]any{"cooling_off": "2h"}})
	if g.CoolingOff != 0 || g.Unapproved != "" {
		t.Errorf("solo + cooling_off 0s: %+v", g)
	}
	// team でも宣言の cooling_off は効く。
	g, _ = loadGate(t, map[string]any{"cooling_off": "30m", "approval": approvers("alice")}, nil)
	if g.CoolingOff != 30*time.Minute {
		t.Errorf("team + cooling_off 30m: %+v", g)
	}
}

// solo: 承認者要件なし、cooling_off（既定 2h）で代替。
func TestProfile_Solo_CoolingOff(t *testing.T) {
	g, _ := loadGate(t, nil, map[string]any{"profile": "solo", "solo": map[string]any{"cooling_off": "90m"}})
	if g.Unapproved != "" || g.CoolingOff != 90*time.Minute {
		t.Errorf("solo cooling_off 90m: %+v", g)
	}
	g, _ = loadGate(t, nil, map[string]any{"profile": "solo"})
	if g.CoolingOff != DefaultSoloCoolingOff {
		t.Errorf("solo default cooling-off: %+v", g)
	}
}

func TestProfile_Rejects(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]map[string]any
		want  string
	}{
		{"unknown profile", map[string]map[string]any{"a.json": {"profile": "galaxy"}}, "profile"},
		{"solo without profile solo", map[string]map[string]any{"a.json": {"solo": map[string]any{"cooling_off": "1h"}}}, "only under profile: solo"},
		{"profile twice", map[string]map[string]any{"a.json": {"profile": "solo"}, "b.json": {"profile": "team"}}, "declared in both"},
		{"bad cooling_off", map[string]map[string]any{"a.json": {"profile": "solo", "solo": map[string]any{"cooling_off": "soon"}}}, "cooling_off"},
		{"negative cooling_off", map[string]map[string]any{"a.json": pcIntent(map[string]any{"cooling_off": "-1h"}, nil)}, "negative"},
		{"unknown approver key", map[string]map[string]any{"a.json": pcIntent(map[string]any{"approval": map[string]any{"approvers": []any{map[string]any{"name": "x"}}}}, nil)}, "unknown key"},
	}
	for _, c := range cases {
		_, err := loadJSONFiles(t, c.files)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func wfIntent(applicant string, gate map[string]any) map[string]any {
	wf := map[string]any{
		"workflow": "rolling",
		"target":   map[string]any{"provider": "fake"},
		"stages":   []any{map[string]any{"name": "s1", "targets": []string{"a"}, "gate": gate}},
		"steps":    []any{map[string]any{"apply": map[string]string{"active": "false"}}},
	}
	if applicant != "" {
		wf["applicant"] = applicant
	}
	return map[string]any{
		"profile":   "enterprise",
		"resources": []any{map[string]any{"provider": "fake", "id": "a", "fields": map[string]string{"active": "true"}}},
		"workflows": []any{wf},
	}
}

// enterprise: requirements の無い承認ゲートにも N-of-M + separation を既定化する。
func TestProfile_Enterprise_WorkflowGates(t *testing.T) {
	in, err := loadJSONFiles(t, map[string]map[string]any{"a.json": wfIntent("carol", map[string]any{"manual_approval": "required"})})
	if err != nil {
		t.Fatal(err)
	}
	req := in.Workflows[0].Stages[0].Gate.Requirements
	if req.Needed() != 2 || req.Separation != "applicant_ne_approver" {
		t.Errorf("enterprise gate default requirements = %+v", req)
	}
	// separation needs the applicant.
	if _, err := loadJSONFiles(t, map[string]map[string]any{"a.json": wfIntent("", map[string]any{"manual_approval": "required"})}); err == nil ||
		!strings.Contains(err.Error(), "profile enterprise") {
		t.Errorf("enterprise gate without applicant: err = %v", err)
	}
	// An explicit requirements wins (and needs no applicant without separation).
	in, err = loadJSONFiles(t, map[string]map[string]any{"a.json": wfIntent("", map[string]any{"manual_approval": map[string]any{
		"requirements": map[string]any{"quorum": map[string]any{"n": 1, "of": 1}}}})})
	if err != nil {
		t.Fatal(err)
	}
	if req := in.Workflows[0].Stages[0].Gate.Requirements; req.Needed() != 1 || req.Separation != "" {
		t.Errorf("explicit gate requirements were overridden: %+v", req)
	}
}
