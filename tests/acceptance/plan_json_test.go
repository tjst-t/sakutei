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

// plan_json_test.go — #135: plan --json（差分を機械で読める形で出す）。
//
//	AC-135-1  作る・変える・消す・予算で止める・planned_change のレーン・Workflow に
//	          任せる・採番待ちを含む intent で、--json の件数と中身が文の出力と一致する
//	          （終了コードも同じ）
//	AC-135-2  秘密の値は出さない：secret のフィールドは SecretRef の形のまま
//	AC-135-3  plan は読むだけ：--audit-log（S4）も state も書かない
//	AC-135-4  計画のエラーは終了コード 1 で、JSON の error に入る
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

type planJSON struct {
	SchemaVersion    int    `json:"schema_version"`
	GeneratedAt      string `json:"generated_at"`
	IntentRevision   string `json:"intent_revision"`
	RenderedRevision string `json:"rendered_revision"`
	Error            string `json:"error"`
	Summary          struct {
		Changes            int `json:"changes"`
		Create             int `json:"create"`
		Update             int `json:"update"`
		Delete             int `json:"delete"`
		Destructive        int `json:"destructive"`
		Blocked            int `json:"blocked"`
		PlannedChangeLane  int `json:"planned_change_lane"`
		Workflow           int `json:"workflow"`
		PendingAllocations int `json:"pending_allocations"`
	} `json:"summary"`
	Operations []struct {
		ID          string            `json:"id"`
		Provider    string            `json:"provider"`
		Scope       map[string]string `json:"scope"`
		LocalID     string            `json:"local_id"`
		Kind        string            `json:"kind"`
		Destructive bool              `json:"destructive"`
		Prune       bool              `json:"prune"`
		Cause       string            `json:"cause"`
		Fields      []struct {
			Field string `json:"field"`
			From  string `json:"from"`
			To    string `json:"to"`
		} `json:"fields"`
		Lane          string `json:"lane"`
		PlannedChange string `json:"planned_change"`
		Blocked       bool   `json:"blocked"`
		BlockedBy     string `json:"blocked_by"`
		BlockReason   string `json:"block_reason"`
	} `json:"operations"`
	Budget struct {
		Halted bool   `json:"halted"`
		Halt   string `json:"halt"`
		Reason string `json:"reason"`
	} `json:"budget"`
	PlannedChangeEvents []string `json:"planned_change_events"`
	Workflow            []struct {
		ID       string `json:"id"`
		Workflow string `json:"workflow"`
		Stage    string `json:"stage"`
	} `json:"workflow"`
	PendingAllocations []struct {
		ID    string `json:"id"`
		Field string `json:"field"`
		Pool  string `json:"pool"`
		Line  string `json:"line"`
	} `json:"pending_allocations"`
}

// setupPlanJSON builds an intent whose plan has every kind of entry:
//
//	web       UPDATE (size 1 → 2)
//	new-1     CREATE
//	net-1     CREATE with a pending allocation (vlan from pool tenant-vlans)
//	n1        target of Workflow "rolling" (left to it, no op)
//	decom-1,2 orphans in pool=decom → DELETE in the planned_change lane
//	stray-1,2 orphans in pool=other → DELETE, halted by the normal budget
//	main/role/app (postgresql) CREATE with a secret-flagged password
func setupPlanJSON(t *testing.T) (intentDir, statePath string, args []string) {
	t.Helper()
	lab := newPGLab(t)
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(tmp, "state.json")
	seedScopedPruneable(t, statePath, []pcResource{
		{"keep-decom", pcDecom}, {"web", pcOther}, {"n1", pcOther},
		{"decom-1", pcDecom}, {"decom-2", pcDecom},
		{"stray-1", pcOther}, {"stray-2", pcOther},
	})
	web := pcDesired("web", pcOther)
	web["fields"] = map[string]string{"name": "web", "size": "2"}
	n1 := pcDesired("n1", pcOther)
	n1["fields"] = map[string]string{"name": "n1", "size": "2"}
	net := pcDesired("net-1", pcOther)
	net["fields"] = map[string]any{"name": "net-1", "vlan": map[string]string{"from_pool": "tenant-vlans"}}
	now := time.Now()
	writeIntentFile(t, intentDir, map[string]any{
		"pools": []map[string]any{{"name": "tenant-vlans", "range": "100-199"}},
		"resources": []map[string]any{
			pcDesired("keep-decom", pcDecom), web, n1, pcDesired("new-1", pcOther), net,
			{"provider": "postgresql", "id": "main/role/app", "fields": map[string]string{
				"login": "true", "password": "secretref:file/pg/app#gen1"}},
		},
		"planned_change": map[string]any{
			"scope": pcDecom, "kind": "delete", "max_count": 5,
			"window":   pcWindow(now.Add(-time.Hour), now.Add(time.Hour)),
			"approval": map[string]any{"pr": 135, "approvers": []string{"alice", "bob"}},
		},
		"workflows": []map[string]any{{
			"workflow":   "rolling",
			"target":     map[string]string{"provider": "fake"},
			"stages":     []map[string]any{{"name": "s1", "targets": []string{"n1"}}},
			"steps":      []map[string]any{{"apply": map[string]string{"size": "2"}}},
			"on_failure": map[string]string{"policy": "halt"},
		}},
	})
	args = []string{
		"--postgresql-config", lab.config,
		"--alloc-store", filepath.Join(tmp, "alloc.json"),
		"--audit-log", filepath.Join(tmp, "audit.jsonl"),
		"--max-destructive-per-cycle=1", "--on-exceed=halt-destructive-only",
	}
	return intentDir, statePath, args
}

var (
	planOpHead    = regexp.MustCompile(`^([+~-]) (CREATE|UPDATE|DELETE) (\S+)( \[DESTRUCTIVE\])?$`)
	planChanges   = regexp.MustCompile(`(?m)^Plan: (\d+) change\(s\)$`)
	planBlocked   = regexp.MustCompile(`(?m)^Blocked: (\d+) of (\d+) change\(s\)`)
	planWorkflow  = regexp.MustCompile(`(?m)^= WORKFLOW (\S+)$`)
	planFieldLine = regexp.MustCompile(`^  [+~-] (\S+)\s`)
)

// textOps parses the op blocks of plan's text output into the same shape as
// the JSON's operations: "KIND id destructive cause blocked_by lane fields".
func textOps(text string) []string {
	var out []string
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(strings.Trim(block, "\n"), "\n")
		m := planOpHead.FindStringSubmatch(lines[0])
		if m == nil {
			continue
		}
		cause, blockedBy, lane := "", "", "normal"
		var fields []string
		for _, l := range lines[1:] {
			switch {
			case strings.HasPrefix(l, "  cause:  "):
				cause = strings.TrimPrefix(l, "  cause:  ")
			case strings.HasPrefix(l, "  blocked: "):
				blockedBy = strings.SplitN(strings.TrimPrefix(l, "  blocked: "), " — ", 2)[0]
			case strings.HasPrefix(l, "  lane:   "):
				lane = "planned_change"
			default:
				if fm := planFieldLine.FindStringSubmatch(l); fm != nil {
					fields = append(fields, fm[1])
				}
			}
		}
		out = append(out, fmt.Sprintf("%s %s %t %s %s %s %s", m[2], m[3], m[4] != "", cause, blockedBy, lane, strings.Join(fields, ",")))
	}
	return out
}

// [AC-135-1] [AC-135-2] [AC-135-3]
func TestPlanJSON_MatchesText(t *testing.T) {
	intentDir, statePath, args := setupPlanJSON(t)
	auditPath := args[5]
	stateBefore := readFile(t, statePath)

	text, textErr, textCode := runPlanPhase1(t, intentDir, statePath, args...)
	out, jsonErr, jsonCode := runPlanPhase1(t, intentDir, statePath, append(args, "--json")...)

	// [AC-135-1] exit codes are the text's.
	if textCode != 0 || jsonCode != textCode {
		t.Fatalf("[AC-135-1] exit codes: text=%d json=%d; stderr:\n%s\n%s", textCode, jsonCode, textErr, jsonErr)
	}
	var got planJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("[AC-135-1] stdout is not one JSON object: %v\n%s", err, out)
	}
	if got.SchemaVersion != 1 || got.GeneratedAt == "" || got.Error != "" ||
		!strings.Contains(text, "intent_revision="+got.IntentRevision+" rendered_revision="+got.RenderedRevision) {
		t.Errorf("[AC-135-1] header: %+v; text:\n%s", got, text)
	}

	// Counts.
	m := planChanges.FindStringSubmatch(text)
	if m == nil || m[1] != fmt.Sprint(got.Summary.Changes) || got.Summary.Changes != len(got.Operations) {
		t.Fatalf("[AC-135-1] changes: text %v, json %d (%d ops); text:\n%s", m, got.Summary.Changes, len(got.Operations), text)
	}
	b := planBlocked.FindStringSubmatch(text)
	if b == nil || b[1] != fmt.Sprint(got.Summary.Blocked) {
		t.Errorf("[AC-135-1] blocked: text %v, json %d", b, got.Summary.Blocked)
	}

	// Each op, in order: kind, id, destructive, cause, blocked_by, lane, fields.
	var jsonOps []string
	for _, o := range got.Operations {
		var fields []string
		for _, f := range o.Fields {
			fields = append(fields, f.Field)
		}
		jsonOps = append(jsonOps, fmt.Sprintf("%s %s %t %s %s %s %s", o.Kind, o.ID, o.Destructive, o.Cause, o.BlockedBy, o.Lane, strings.Join(fields, ",")))
	}
	if tOps := textOps(text); strings.Join(tOps, "\n") != strings.Join(jsonOps, "\n") {
		t.Errorf("[AC-135-1] ops differ:\ntext:\n%s\njson:\n%s", strings.Join(tOps, "\n"), strings.Join(jsonOps, "\n"))
	}

	// What the intent was built to produce.
	want := map[string]string{
		"web": "UPDATE normal", "new-1": "CREATE normal", "net-1": "CREATE normal", "main/role/app": "CREATE normal",
		"decom-1": "DELETE planned_change", "decom-2": "DELETE planned_change",
		"stray-1": "DELETE normal budget", "stray-2": "DELETE normal budget",
	}
	gotOps := map[string]string{}
	for _, o := range got.Operations {
		s := o.Kind + " " + o.Lane
		if o.Blocked {
			s += " " + o.BlockedBy
			if o.BlockReason != got.Budget.Reason {
				t.Errorf("[AC-135-1] %s: block reason %q, budget reason %q", o.LocalID, o.BlockReason, got.Budget.Reason)
			}
		}
		if o.Kind == "DELETE" && (!o.Prune || !o.Destructive) {
			t.Errorf("[AC-135-1] %s: an orphan delete must be prune and destructive: %+v", o.LocalID, o)
		}
		if o.Lane == "planned_change" && !strings.Contains(o.PlannedChange, "planned_change(scope=pool=decom kind=delete") {
			t.Errorf("[AC-135-1] %s: planned_change %q", o.LocalID, o.PlannedChange)
		}
		gotOps[o.LocalID] = s
	}
	for local, w := range want {
		if gotOps[local] != w {
			t.Errorf("[AC-135-1] %s: got %q, want %q; ops %v", local, gotOps[local], w, gotOps)
		}
	}
	if _, ok := gotOps["n1"]; ok {
		t.Errorf("[AC-135-1] n1 is left to its Workflow and must have no op")
	}
	if !got.Budget.Halted || got.Budget.Halt != "destructive" || !strings.Contains(text, "Budget: a reconcile now would halt destructive changes — "+got.Budget.Reason) {
		t.Errorf("[AC-135-1] budget %+v; text:\n%s", got.Budget, text)
	}
	if got.Summary.PlannedChangeLane != 2 || len(got.PlannedChangeEvents) == 0 ||
		!strings.Contains(textErr, got.PlannedChangeEvents[0]) {
		t.Errorf("[AC-135-1] planned_change lane %d events %v; stderr:\n%s", got.Summary.PlannedChangeLane, got.PlannedChangeEvents, textErr)
	}

	// Workflow targets.
	var tw, jw []string
	for _, mm := range planWorkflow.FindAllStringSubmatch(text, -1) {
		tw = append(tw, mm[1])
	}
	for _, w := range got.Workflow {
		jw = append(jw, w.ID)
		if w.Workflow != "rolling" || w.Stage == "" || !strings.Contains(text, "  stage:    "+w.Stage) {
			t.Errorf("[AC-135-1] workflow target %+v", w)
		}
	}
	if len(jw) != 1 || fmt.Sprint(tw) != fmt.Sprint(jw) || got.Summary.Workflow != 1 {
		t.Errorf("[AC-135-1] workflow: text %v, json %v", tw, jw)
	}

	// Pending allocations: the text's stderr lines.
	var tp, jp []string
	for _, l := range strings.Split(textErr, "\n") {
		if strings.Contains(l, "pending allocation:") {
			tp = append(tp, l)
		}
	}
	for _, p := range got.PendingAllocations {
		jp = append(jp, p.Line)
		if p.Pool != "tenant-vlans" || p.Field != "vlan" || !strings.HasSuffix(p.ID, "/net-1") {
			t.Errorf("[AC-135-1] pending allocation %+v", p)
		}
	}
	sort.Strings(tp)
	sort.Strings(jp)
	if len(jp) != 1 || fmt.Sprint(tp) != fmt.Sprint(jp) || got.Summary.PendingAllocations != 1 {
		t.Errorf("[AC-135-1] pending allocations: text %v, json %v", tp, jp)
	}

	// [AC-135-2] the secret-flagged password stays a SecretRef; no plaintext.
	for _, secret := range []string{"app-role-secret", pgAdminSecret} {
		if strings.Contains(out, secret) || strings.Contains(jsonErr, secret) {
			t.Errorf("[AC-135-2] plaintext %q in the plan's output", secret)
		}
	}
	foundRef := false
	for _, o := range got.Operations {
		for _, f := range o.Fields {
			if o.LocalID == "main/role/app" && f.Field == "password" {
				foundRef = f.To == "secretref:file/pg/app#gen1"
			}
		}
	}
	if !foundRef {
		t.Errorf("[AC-135-2] the password field must be in the op as its SecretRef:\n%s", out)
	}

	// [AC-135-3] plan wrote neither S4 nor reality.
	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Errorf("[AC-135-3] plan must not write the audit log %s (stat err %v)", auditPath, err)
	}
	if readFile(t, statePath) != stateBefore {
		t.Errorf("[AC-135-3] plan changed the state file")
	}
}

// [AC-135-4] a plan that cannot be made exits 1 in both forms; the JSON says why.
func TestPlanJSON_Error(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "no-such-intent")
	statePath := filepath.Join(tmp, "state.json")
	_, _, textCode := runPlanPhase1(t, missing, statePath)
	out, _, jsonCode := runPlanPhase1(t, missing, statePath, "--json")
	if textCode != 1 || jsonCode != 1 {
		t.Fatalf("[AC-135-4] exit codes: text=%d json=%d, want 1", textCode, jsonCode)
	}
	var got planJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("[AC-135-4] stdout is not one JSON object: %v\n%s", err, out)
	}
	if got.SchemaVersion != 1 || !strings.Contains(got.Error, "load intent") || len(got.Operations) != 0 {
		t.Errorf("[AC-135-4] error report: %+v", got)
	}
}
