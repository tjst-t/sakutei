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

// explicit_absent_test.go — Backlog #163: deleting by declaration.  A resource
// declared `ensure: absent` is deleted when it exists and conformant when it
// does not, whatever the type's Pruneable (the fake here is not pruneable, like
// proxmox-vm).  The Delete is destructive, counted by the change budget, and
// runs only in an open planned_change lane of kind delete (its approval gate,
// §13.3 / §14.4 / §14.5).
//
// AC tags:
//
//	AC-163-1  load-time refusals: fields / depends_on / update_policy on an
//	          absent resource, a resource depending on an absent one, an
//	          unknown ensure value
//	AC-163-2  plan shows the DELETE (absent: true), blocked by approval
//	          without an open lane and not blocked with one; an absent
//	          resource missing in reality is converged
//	AC-163-3  one-shot reconcile: no lane (or an unapproved one) → not
//	          deleted, AbsentAwaitingApproval; an approved lane with budget →
//	          deleted, then converged; an approved lane but no budget → not
//	          deleted (the lane approves, it does not exempt)
//	AC-163-4  the daemon: not deleted while no lane is open, deleted once an
//	          approved planned_change opens it; removing a resource from the
//	          intent (instead of ensure: absent) never deletes it
//	AC-163-5  proxmox-vm (fake Proxmox VE): a running VM declared absent is
//	          kept without approval, stopped and destroyed with it; a
//	          protected VM cannot be declared absent
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
)

var absentScope = map[string]string{"pool": "decom"}

// absentRig is an intent dir with one kept resource and one resource declared
// ensure: absent ("old"), over a fake reality holding both.
type absentRig struct {
	t         *testing.T
	intentDir string
	statePath string
	auditPath string
	logPath   string
	now       time.Time
}

func newAbsentRig(t *testing.T) *absentRig {
	t.Helper()
	tmp := t.TempDir()
	r := &absentRig{t: t, intentDir: filepath.Join(tmp, "intent"), statePath: filepath.Join(tmp, "state.json"),
		auditPath: filepath.Join(tmp, "audit.jsonl"), logPath: filepath.Join(tmp, "daemon.log"), now: time.Now()}
	if err := os.MkdirAll(r.intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedState(t, r.statePath, map[string]map[string]any{
		"keep": {"name": "keep", "size": "1"},
		"old":  {"name": "old", "size": "1"},
	})
	return r
}

// write writes the intent: lane is the planned_change over absentScope (nil:
// none), withOld whether "old" is declared ensure: absent (else left out).
func (r *absentRig) write(lane map[string]any, withOld bool) {
	r.t.Helper()
	resources := []map[string]any{{"provider": "fake", "id": "keep", "fields": map[string]string{"name": "keep", "size": "1"}}}
	if withOld {
		resources = append(resources, map[string]any{"provider": "fake", "id": "old", "scope": absentScope, "ensure": "absent"})
	}
	intent := map[string]any{"resources": resources}
	if lane != nil {
		decl := map[string]any{"scope": absentScope, "kind": "delete", "max_count": 1,
			"window": pcWindow(r.now.Add(-time.Hour), r.now.Add(time.Hour))}
		for k, v := range lane {
			decl[k] = v
		}
		intent["planned_change"] = decl
	}
	writeIntentFile(r.t, r.intentDir, intent)
}

func (r *absentRig) oldExists() bool {
	r.t.Helper()
	return stateHasResource(r.t, r.statePath, "old")
}

func stateHasResource(t *testing.T, statePath, id string) bool {
	t.Helper()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var sf struct {
		Resources map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		t.Fatal(err)
	}
	_, ok := sf.Resources[id]
	return ok
}

var approvedLane = map[string]any{"approval": map[string]any{"pr": 163, "approvers": []string{"alice"}}}

func TestExplicitAbsent_LoadRefusals(t *testing.T) {
	// [AC-163-1]
	cases := map[string]struct {
		resources []map[string]any
		want      string
	}{
		"fields": {[]map[string]any{{"provider": "fake", "id": "old", "ensure": "absent", "fields": map[string]string{"size": "1"}}},
			"ensure: absent cannot be declared with fields"},
		"depends_on": {[]map[string]any{{"provider": "fake", "id": "a"}, {"provider": "fake", "id": "old", "ensure": "absent", "depends_on": []string{"a"}}},
			"ensure: absent cannot be declared with depends_on"},
		"update_policy": {[]map[string]any{{"provider": "fake", "id": "old", "ensure": "absent", "update_policy": map[string]any{"strategy": "task"}}},
			"ensure: absent cannot be declared with update_policy"},
		"dependent": {[]map[string]any{{"provider": "fake", "id": "old", "ensure": "absent"}, {"provider": "fake", "id": "b", "depends_on": []string{"old"}}},
			"which is ensure: absent"},
		"unknown value": {[]map[string]any{{"provider": "fake", "id": "old", "ensure": "gone"}},
			"ensure"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			intentDir := filepath.Join(tmp, "intent")
			if err := os.MkdirAll(intentDir, 0o755); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(tmp, "state.json")
			seedState(t, statePath, map[string]map[string]any{"old": {"name": "old", "size": "1"}})
			writeIntentFile(t, intentDir, map[string]any{"resources": c.resources})
			out, _, code := runReconcileCombined(t, intentDir, statePath)
			if code == 0 || !strings.Contains(out, c.want) {
				t.Errorf("[AC-163-1] %s: exit %d, want a load refusal naming %q:\n%s", name, code, c.want, out)
			}
			if !stateHasResource(t, statePath, "old") {
				t.Errorf("[AC-163-1] %s: the resource was deleted by a refused intent", name)
			}
		})
	}
}

func TestExplicitAbsent_Plan(t *testing.T) {
	// [AC-163-2]
	r := newAbsentRig(t)
	r.write(nil, true)
	out, stderr, code := runPlan(t, r.intentDir, r.statePath)
	if code != 0 || !strings.Contains(out, "DELETE fake/pool=decom/old") || !strings.Contains(out, "blocked: approval") {
		t.Fatalf("[AC-163-2] plan without a lane: exit %d, want DELETE blocked by approval:\n%s\n%s", code, out, stderr)
	}
	js, _, code := runPlan(t, r.intentDir, r.statePath, "--json")
	if code != 0 || !strings.Contains(js, `"absent": true`) || !strings.Contains(js, `"blocked_by": "approval"`) {
		t.Fatalf("[AC-163-2] plan --json: exit %d, want absent: true and blocked_by approval:\n%s", code, js)
	}

	r.write(approvedLane, true)
	out, stderr, code = runPlan(t, r.intentDir, r.statePath, "--audit-log", r.auditPath)
	if code != 0 || !strings.Contains(out, "DELETE fake/pool=decom/old") || strings.Contains(out, "blocked:") || !strings.Contains(out, "lane:") {
		t.Fatalf("[AC-163-2] plan with an approved lane: exit %d, want DELETE in the lane, not blocked:\n%s\n%s", code, out, stderr)
	}
	if !r.oldExists() {
		t.Fatal("[AC-163-2] plan deleted the resource")
	}

	// Gone in reality → nothing to do.
	seedState(t, r.statePath, map[string]map[string]any{"keep": {"name": "keep", "size": "1"}})
	if out, _, code := runPlan(t, r.intentDir, r.statePath); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-163-2] plan with the absent resource gone: exit %d, want converged:\n%s", code, out)
	}
}

func TestExplicitAbsent_Reconcile(t *testing.T) {
	// [AC-163-3]
	r := newAbsentRig(t)

	// No planned_change: held for approval.
	r.write(nil, true)
	out, _, code := runReconcileCombined(t, r.intentDir, r.statePath, "--audit-log", r.auditPath)
	if code != 0 || !r.oldExists() || !strings.Contains(out, "AbsentAwaitingApproval") || !strings.Contains(out, "awaiting approval") {
		t.Fatalf("[AC-163-3] reconcile without a lane: exit %d, exists %v; want kept with AbsentAwaitingApproval:\n%s", code, r.oldExists(), out)
	}

	// A planned_change without its approval (team: 1 approver) does not open.
	r.write(map[string]any{}, true)
	out, _, _ = runReconcileCombined(t, r.intentDir, r.statePath, "--audit-log", r.auditPath)
	if !r.oldExists() || !strings.Contains(out, "PlannedChangeUnapproved") {
		t.Fatalf("[AC-163-3] reconcile with an unapproved lane: want kept and PlannedChangeUnapproved:\n%s", out)
	}

	// Approved, but over the budget (with "keep" drifted, 2 changes against
	// --max-changes-per-cycle=1): the lane approves the Delete, it does not
	// exempt it from the budget.
	r.write(approvedLane, true)
	seedState(t, r.statePath, map[string]map[string]any{"keep": {"name": "keep", "size": "2"}, "old": {"name": "old", "size": "1"}})
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath, "--audit-log", r.auditPath, "--max-changes-per-cycle=1")
	if code == 0 || !r.oldExists() || !strings.Contains(out, "BUDGET HALTED") {
		t.Fatalf("[AC-163-3] reconcile with an approved lane but over the budget: exit %d, exists %v; want kept and BUDGET HALTED:\n%s", code, r.oldExists(), out)
	}

	// Approved lane with budget: deleted, then converged.
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath, "--audit-log", r.auditPath)
	if code != 0 || r.oldExists() || !strings.Contains(out, "DELETE fake/pool=decom/old") {
		t.Fatalf("[AC-163-3] reconcile with an approved lane: exit %d, exists %v; want deleted:\n%s", code, r.oldExists(), out)
	}
	out, _, code = runReconcileCombined(t, r.intentDir, r.statePath, "--audit-log", r.auditPath)
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-163-3] reconcile after the delete: exit %d, want converged:\n%s", code, out)
	}
}

func TestExplicitAbsent_Daemon(t *testing.T) {
	// [AC-163-4]
	r := newAbsentRig(t)
	r.write(nil, true)
	stop := startPlannedChangeDaemon(t, r.intentDir, r.statePath, r.logPath, "--audit-log", r.auditPath)
	waitForMoreCycles(t, r.logPath, 3)
	if !r.oldExists() {
		t.Fatalf("[AC-163-4] the daemon deleted an absent resource without approval; log:\n%s", readFileString(r.logPath))
	}
	if log := readFileString(r.logPath); !strings.Contains(log, "AbsentAwaitingApproval") || !strings.Contains(log, "awaiting_approval=1") {
		t.Fatalf("[AC-163-4] want AbsentAwaitingApproval and awaiting_approval=1 in the log:\n%s", log)
	}

	r.write(approvedLane, true)
	deadline := time.Now().Add(10 * time.Second)
	for r.oldExists() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if r.oldExists() {
		t.Fatalf("[AC-163-4] the approved lane did not delete the absent resource; log:\n%s", readFileString(r.logPath))
	}
	waitForMoreCycles(t, r.logPath, 3)
	stop()
	if !stateHasResource(t, r.statePath, "keep") {
		t.Error("[AC-163-4] the kept resource was deleted")
	}
	log := readFileString(r.logPath)
	if !strings.Contains(log, "converged (no changes)") {
		t.Errorf("[AC-163-4] want the daemon converged after the delete; log:\n%s", log)
	}
}

func TestExplicitAbsent_RemovalFromIntentDoesNotDelete(t *testing.T) {
	// [AC-163-4] open-world: leaving a resource out of the intent is not a delete.
	r := newAbsentRig(t)
	r.write(approvedLane, false)
	stop := startPlannedChangeDaemon(t, r.intentDir, r.statePath, r.logPath, "--audit-log", r.auditPath)
	waitForMoreCycles(t, r.logPath, 4)
	stop()
	if !r.oldExists() {
		t.Fatalf("[AC-163-4] a resource left out of the intent was deleted; log:\n%s", readFileString(r.logPath))
	}
}

func TestExplicitAbsent_ProxmoxVM(t *testing.T) {
	// [AC-163-5]
	srv, cfg := pveRig(t)
	bin := buildBinary(t)
	srvAddRunningVM(srv)
	now := time.Now()
	lane := `"planned_change": {"scope": {"site": "lab"}, "kind": "delete", "max_count": 1,
	  "window": "` + pcWindow(now.Add(-time.Hour), now.Add(time.Hour)) + `", "approval": {"pr": 163, "approvers": ["alice"]}}, `
	common := pveIntentWith(t, cfg, "", `{"provider": "proxmox-vm", "scope": {"site": "lab"}, "id": "lab/old1", "ensure": "absent"}`)
	audit := filepath.Join(t.TempDir(), "audit.jsonl")

	if o, code := runBin(t, bin, append([]string{"plan"}, common...)...); code != 0 || !strings.Contains(o, "DELETE proxmox-vm/site=lab/lab/old1") || !strings.Contains(o, "blocked: approval") {
		t.Fatalf("[AC-163-5] plan: exit %d, want DELETE blocked by approval:\n%s", code, o)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile", "--audit-log", audit}, common...)...); code != 0 || !strings.Contains(o, "AbsentAwaitingApproval") {
		t.Fatalf("[AC-163-5] reconcile without approval: exit %d:\n%s", code, o)
	}
	if _, ok := srv.VMByName("old1"); !ok || srv.Calls("destroy") != 0 {
		t.Fatal("[AC-163-5] the VM was destroyed without approval")
	}

	common = pveIntentWith(t, cfg, lane, `{"provider": "proxmox-vm", "scope": {"site": "lab"}, "id": "lab/old1", "ensure": "absent"}`)
	if o, code := runBin(t, bin, append([]string{"reconcile", "--audit-log", audit}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-163-5] reconcile with an approved lane: exit %d:\n%s", code, o)
	}
	if _, ok := srv.VMByName("old1"); ok {
		t.Fatal("[AC-163-5] the VM still exists after the approved delete")
	}
	if srv.Calls("stop") == 0 || srv.Calls("destroy") != 1 {
		t.Errorf("[AC-163-5] want the running VM stopped then destroyed: stop=%d destroy=%d", srv.Calls("stop"), srv.Calls("destroy"))
	}
	if o, code := runBin(t, bin, append([]string{"reconcile", "--audit-log", audit}, common...)...); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Fatalf("[AC-163-5] reconcile after the delete: exit %d, want converged:\n%s", code, o)
	}

	// Sakutei's own VM (protected_names) cannot be declared absent.
	prot := pveIntentWith(t, cfg, lane, `{"provider": "proxmox-vm", "scope": {"site": "lab"}, "id": "lab/sakutei-ctl", "ensure": "absent"}`)
	for _, verb := range []string{"plan", "reconcile"} {
		if o, code := runBin(t, bin, append([]string{verb, "--audit-log", audit}, prot...)...); code == 0 || !strings.Contains(o, "protected") {
			t.Errorf("[AC-163-5] %s with a protected VM declared absent: exit %d, want a refusal naming it protected:\n%s", verb, code, o)
		}
	}
	if _, ok := srv.VMByName("sakutei-ctl"); !ok {
		t.Fatal("[AC-163-5] the protected VM was destroyed")
	}
}

// srvAddRunningVM adds the running VM "old1" (vmid 501) to the fake Proxmox VE.
func srvAddRunningVM(srv *pvetest.Server) {
	srv.AddVM(pvetest.VM{VMID: 501, Node: "pve1", Name: "old1", Status: "running", Config: map[string]string{
		"cores": "1", "memory": "1024", "scsi0": "local-lvm:vm-501-disk-0,size=4G",
	}})
}

// pveIntentWith is pveIntent with top-level declarations (a JSON fragment
// ending in ", ", or "") before the resources.
func pveIntentWith(t *testing.T, cfg, top, resources string) []string {
	t.Helper()
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{` + top + `"resources": [` + resources + `]}`
	if err := os.WriteFile(filepath.Join(intentDir, "vms.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": cfg}}})
	return []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc}
}
