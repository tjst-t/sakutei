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

// proxmox_vm_keep_test.go — Backlog #204: proxmox-vm keeps a data disk across
// re-creating a VM (the keep mark, and attaching an existing volume), through
// the CLI against the fake Proxmox VE.
//
// AC tags:
//
//	AC-204-1  re-creating a VM: the new VM declares the old VM's keep disk
//	          with volume=; without an open planned_change lane the attach is
//	          held for approval and nothing moves; with a lane of kind create
//	          the disk is moved (not destroyed) onto the new VM; the old VM's
//	          ensure: absent, in a lane of kind delete, destroys the old VM and
//	          its non-keep disks, and the moved disk survives
//	AC-204-2  the old VM's ensure: absent while it still holds its keep disk
//	          stops the VM and does not destroy it (nor the disk), saying which
//	          disk it holds
//	AC-204-3  a volume left detached from a VM (its unusedN) is listed by
//	          sakutei status as unmanaged (text and --json), and reattached
//	          with volume= it is no longer
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

// keepRig is the fake Proxmox VE and an intent directory whose resources are
// in scope site=lab (the cluster's scope), so a planned_change can cover them.
type keepRig struct {
	t      *testing.T
	srv    *pvetest.Server
	bin    string
	dir    string
	common []string
	audit  string
	now    time.Time
}

func newKeepRig(t *testing.T) *keepRig {
	t.Helper()
	srv, cfg := pveRig(t)
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scoped := strings.Replace(string(body), "    node: pve1\n", "    node: pve1\n    scope: site=lab\n", 1)
	if err := os.WriteFile(cfg, []byte(scoped), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": cfg}}})
	return &keepRig{t: t, srv: srv, bin: buildBinary(t), dir: dir, now: time.Now().UTC(), audit: filepath.Join(tmp, "audit.jsonl"),
		common: []string{"--intent", dir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc,
			"--alloc-store", filepath.Join(tmp, "alloc.json")}}
}

// vm is a resource declaration in scope site=lab.
func keepDecl(name string, fields map[string]string) map[string]any {
	return map[string]any{"provider": "proxmox-vm", "scope": map[string]string{"site": "lab"}, "id": "lab/" + name, "fields": fields}
}

// write writes the intent with the given resources and planned_change kinds
// (each an approved lane over site=lab).
func (r *keepRig) write(resources []map[string]any, kinds ...string) {
	r.t.Helper()
	intent := map[string]any{"resources": resources}
	var lanes []map[string]any
	for _, k := range kinds {
		lanes = append(lanes, map[string]any{"scope": map[string]string{"site": "lab"}, "kind": k, "max_count": 2,
			"window":   pcWindow(r.now.Add(-time.Hour), r.now.Add(time.Hour)),
			"approval": map[string]any{"pr": 204, "approvers": []string{"alice"}}})
	}
	if len(lanes) > 0 {
		intent["planned_change"] = lanes
	}
	writeIntentFile(r.t, r.dir, intent)
}

// run runs a sakutei command; reconcile gets the audit log (a one-shot run opens
// planned_change lanes only with it, §13.3).
func (r *keepRig) run(args ...string) (string, int) {
	r.t.Helper()
	if args[0] == "reconcile" {
		args = append(args, "--audit-log", r.audit)
	}
	return runBin(r.t, r.bin, append(args, r.common...)...)
}

func oldVM(power, disks string) map[string]any {
	return keepDecl("app1", map[string]string{"vmid": "120", "template": "debian-tmpl", "disks": disks, "power": power})
}

const (
	oldDisks = "scsi0 storage=local-lvm size=4G, scsi1 storage=local-lvm size=2G keep, scsi2 storage=local-lvm size=1G"
	dataVol  = "local-lvm:vm-120-disk-1"
	movedVol = "local-lvm:vm-121-disk-1"
)

var newVM = keepDecl("app2", map[string]string{"vmid": "121", "template": "debian-tmpl",
	"disks": "scsi0 storage=local-lvm size=4G, scsi1 volume=" + dataVol + " keep", "power": "running"})

func TestProxmoxVM_RecreateKeepsDataDisk(t *testing.T) {
	r := newKeepRig(t)
	r.write([]map[string]any{oldVM("running", oldDisks)})
	for i := 0; i < 3; i++ {
		if o, code := r.run("reconcile"); code != 0 {
			t.Fatalf("reconcile %d exit %d:\n%s", i, code, o)
		}
	}
	if vm, ok := r.srv.VM(120); !ok || vm.Status != "running" || vm.Config["tags"] != "sakutei-keep-scsi1" || !strings.HasPrefix(vm.Config["scsi1"], dataVol+",") {
		t.Fatalf("old VM = %+v", vm)
	}

	// [AC-204-2] 前の VM を keep のディスクごと absent にすると、止めて消さない。
	writeIntentFile(t, r.dir, map[string]any{"resources": []map[string]any{
		{"provider": "proxmox-vm", "scope": map[string]string{"site": "lab"}, "id": "lab/app1", "ensure": "absent"}},
		"planned_change": map[string]any{"scope": map[string]string{"site": "lab"}, "kind": "delete", "max_count": 1,
			"window": pcWindow(r.now.Add(-time.Hour), r.now.Add(time.Hour)), "approval": map[string]any{"pr": 204, "approvers": []string{"alice"}}}})
	o, _ := r.run("reconcile")
	if vm, ok := r.srv.VM(120); !ok || vm.Status != "stopped" || !strings.Contains(o, "keep volume(s) scsi1="+dataVol) {
		t.Fatalf("[AC-204-2] absent of the VM holding its keep disk: VM %+v, %v:\n%s", vm, ok, o)
	}
	if _, ok := r.srv.Volume(dataVol); !ok || r.srv.Calls("destroy") != 0 {
		t.Fatalf("[AC-204-2] the VM or its keep disk was destroyed (destroy calls %d)", r.srv.Calls("destroy"))
	}

	// [AC-204-1] 作り直し：前の VM は止めて keep のディスクを宣言から外し、新しい
	// VM が volume= で付ける。承認（create のレーン）が無ければ付け替えない。
	step1 := []map[string]any{oldVM("stopped", "scsi0 storage=local-lvm size=4G"), newVM}
	r.write(step1)
	if o, code := r.run("plan"); code != 0 || !strings.Contains(o, "CREATE proxmox-vm/site=lab/lab/app2") || !strings.Contains(o, "blocked: approval") {
		t.Fatalf("[AC-204-1] plan without a lane exit %d:\n%s", code, o)
	}
	o, code := r.run("reconcile")
	if code != 0 {
		t.Fatalf("[AC-204-1] reconcile without a lane exit %d:\n%s", code, o)
	}
	if _, ok := r.srv.VM(121); ok || !strings.Contains(o, "ChangeAwaitingApproval") {
		t.Fatalf("[AC-204-1] the new VM was created without approval:\n%s", o)
	}
	if vm, _ := r.srv.VM(120); !strings.HasPrefix(vm.Config["scsi1"], dataVol+",") || r.srv.Calls("move_disk") != 0 {
		t.Fatalf("[AC-204-1] the disk moved without approval: %v", vm.Config)
	}

	r.write(step1, "create")
	for i := 0; i < 3; i++ {
		o, code = r.run("reconcile")
		t.Logf("reconcile %d with the create lane: exit %d\n%s", i, code, o)
	}
	vm2, ok := r.srv.VM(121)
	if !ok || !strings.HasPrefix(vm2.Config["scsi1"], movedVol+",size=2G") || vm2.Status != "running" || vm2.Config["tags"] != "sakutei-keep-scsi1" {
		t.Fatalf("[AC-204-1] new VM = %+v, %v", vm2, ok)
	}
	if _, ok := r.srv.Volume(movedVol); !ok {
		t.Fatal("[AC-204-1] the moved volume is gone")
	}
	if o, code := r.run("reconcile"); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Fatalf("[AC-204-1] reconcile after the move exit %d, want converged:\n%s", code, o)
	}

	// 前の VM を消す（delete のレーン）。keep でないディスクは一緒に消え、付け替えた
	// ディスクは残る。
	r.write([]map[string]any{newVM, {"provider": "proxmox-vm", "scope": map[string]string{"site": "lab"}, "id": "lab/app1", "ensure": "absent"}}, "delete")
	for i := 0; i < 3; i++ {
		if _, gone := r.srv.VM(120); !gone {
			break
		}
		o, code = r.run("reconcile")
		t.Logf("reconcile %d with the delete lane: exit %d\n%s", i, code, o)
	}
	if _, ok := r.srv.VM(120); ok {
		t.Fatal("[AC-204-1] the old VM was not destroyed")
	}
	if _, ok := r.srv.Volume("local-lvm:vm-120-disk-2"); ok {
		t.Error("[AC-204-1] the old VM's non-keep disk survived")
	}
	if _, ok := r.srv.Volume(movedVol); !ok {
		t.Error("[AC-204-1] the kept disk was destroyed with the old VM")
	}

	// [AC-204-3] 手で外したままの volume は unmanaged に出る。付け直せば出ない。
	r.srv.SetConfig(121, "scsi1", "")
	r.srv.SetConfig(121, "unused0", movedVol)
	r.write([]map[string]any{newVM})
	unmanaged := func() map[string]string {
		t.Helper()
		o, _ := r.run("status", "--json")
		var rep struct {
			Resources []struct {
				LocalID string `json:"local_id"`
				State   string
			}
		}
		if i := strings.Index(o, "{"); i < 0 || json.Unmarshal([]byte(o[i:]), &rep) != nil {
			t.Fatalf("[AC-204-3] status --json is not JSON:\n%s", o)
		}
		states := map[string]string{}
		for _, x := range rep.Resources {
			states[x.LocalID] = x.State
		}
		return states
	}
	if states := unmanaged(); states["lab/app2/unused0"] != "unmanaged" {
		t.Errorf("[AC-204-3] status --json = %v; want lab/app2/unused0 unmanaged", states)
	}
	if o, _ := r.run("status"); !strings.Contains(o, "lab/app2/unused0") {
		t.Errorf("[AC-204-3] status (text) does not show the detached volume:\n%s", o)
	}
	reattach := keepDecl("app2", map[string]string{"vmid": "121", "template": "debian-tmpl",
		"disks": "scsi0 storage=local-lvm size=4G, scsi1 volume=" + movedVol + " keep", "power": "running"})
	r.write([]map[string]any{reattach}, "update")
	for i := 0; i < 2; i++ {
		o, code = r.run("reconcile")
		t.Logf("reconcile %d with the update lane: exit %d\n%s", i, code, o)
	}
	if states := unmanaged(); states["lab/app2/unused0"] != "" || states["lab/app2"] != "conformant" {
		t.Errorf("[AC-204-3] status after the reattach = %v", states)
	}
}
