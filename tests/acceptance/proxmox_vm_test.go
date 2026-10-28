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

// proxmox_vm_test.go — Backlog #143: the proxmox-vm provider (Proxmox VE VMs)
// against the fake Proxmox VE (internal/proxmox/pvetest, which checks the API
// token of every request).  A real Proxmox VE is exercised by
// tests/integration/proxmox (make integration-proxmox).
//
// AC tags:
//
//	AC-143-1  DefaultRegistry builds proxmox-vm from proxmox_config (the
//	          cluster list; the token secret is a SecretRef resolved from the
//	          file SecretBackend); without proxmox_config it is an error; the
//	          provider passes sdk.ContractTester (incl. unreachable → unknown)
//	AC-143-2  sakutei reconcile through --provider-config creates a VM from a
//	          template with its vmid from a pool, and the next reconciles are
//	          converged (no perpetual drift); a hand change of the memory is
//	          put back
//	AC-143-3  declaring a protected VM (Sakutei's own, §23) is a plan error
//	          and nothing is sent to the cluster
//	AC-143-4  a VM is not pruneable (a destructive type, §12.3): an orphan VM
//	          is never deleted by prune
//	AC-164-1  sakutei status (text and --json) shows a VM that is not
//	          declared as unmanaged, also when the intent declares no VM at
//	          all (the provider is set up by --provider-config only); the
//	          protected VM and the template are not shown; a declared VM is
//	          not unmanaged; reconcile leaves the unmanaged VM alone
package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// pveRig starts a fake Proxmox VE with Sakutei's own VM (100, protected) and a
// template (9000), and writes its cluster list and secret store.
func pveRig(t *testing.T) (*pvetest.Server, string) {
	t.Helper()
	srv := pvetest.New(pvetest.Options{})
	t.Cleanup(srv.Close)
	srv.AddVM(pvetest.VM{VMID: 100, Node: "pve1", Name: "sakutei-ctl", Status: "running", Config: map[string]string{
		"cores": "2", "memory": "2048", "scsi0": "local-lvm:vm-100-disk-0,size=16G",
	}})
	srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true, Config: map[string]string{
		"cores": "1", "memory": "1024", "scsihw": "virtio-scsi-single", "scsi0": "local-lvm:base-9000-disk-0,size=4G",
		"ide2": "local-lvm:vm-9000-cloudinit,media=cdrom", "net0": "virtio=BC:24:11:00:90:00,bridge=vmbr0",
	}})
	dir := t.TempDir()
	store := `{"secrets": {"proxmox/lab": {"gen": 1, "value": "` + srv.Secret() + `"}}}`
	cfg := "secret_store: secrets.json\nclusters:\n  lab:\n    endpoint: " + srv.URL + "\n    token_id: " + srv.TokenID() +
		"\n    token_secret: secretref:file/proxmox/lab\n    node: pve1\n    timeout: 2s\n    task_wait: 2s\n" +
		"    protected_vmids:\n      - 100\n    protected_names:\n      - sakutei-ctl\n"
	for name, body := range map[string]string{"secrets.json": store, "pve.yaml": cfg} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return srv, filepath.Join(dir, "pve.yaml")
}

func TestProxmoxVM_RegistryAndContract(t *testing.T) {
	// [AC-143-1]
	if _, err := registry.DefaultRegistry().Get(proxmox.Name, map[string]string{"state": "x.json"}); err == nil || !strings.Contains(err.Error(), "proxmox_config") {
		t.Errorf("[AC-143-1] proxmox-vm without proxmox_config: err = %v; want one naming proxmox_config", err)
	}
	srv, cfg := pveRig(t)
	p, err := registry.DefaultRegistry().Get(proxmox.Name, map[string]string{"proxmox_config": cfg})
	if err != nil {
		t.Fatalf("[AC-143-1] DefaultRegistry().Get(proxmox-vm): %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-143-1] ValidateProvider: %v", err)
	}
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p.(provider.Lister),
		Scope:    core.NewScope(nil),
		SampleID: "lab/ac-143",
		SampleDesired: map[string]string{
			proxmox.FieldVMID: "143", proxmox.FieldCores: "2", proxmox.FieldMemory: "1024",
			proxmox.FieldDisks: "scsi0 storage=local-lvm size=4G", proxmox.FieldPower: "stopped",
		},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

// pveIntent writes an intent with the given resources (JSON fragments) and a
// vmid pool, and returns the CLI arguments common to every command.
func pveIntent(t *testing.T, cfg, resources string) []string {
	t.Helper()
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	intent := `{"pools": [{"name": "vmids", "range": "500-509"}], "resources": [` + resources + `]}`
	if err := os.WriteFile(filepath.Join(intentDir, "vms.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": cfg}}})
	return []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc,
		"--alloc-store", filepath.Join(tmp, "alloc.json")}
}

func TestProxmoxVM_ReconcileFromTemplate(t *testing.T) {
	// [AC-143-2]
	srv, cfg := pveRig(t)
	bin := buildBinary(t)
	common := pveIntent(t, cfg, `{"provider": "proxmox-vm", "id": "lab/app1", "fields": {
	  "vmid": {"from_pool": "vmids"}, "template": "debian-tmpl", "cores": "2", "memory": "2G",
	  "disks": "scsi0 storage=local-lvm size=8G", "nics": "net0 bridge=vmbr0 vlan=20",
	  "ci_user": "ops", "ci_ssh_keys": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEQHShMqBxoVWaNhs0o1VR7qEL2eQZJqgpnZrDZvbS0o sakutei",
	  "ci_ip": "192.0.2.21/24", "ci_gateway": "192.0.2.1", "power": "running"}}`)

	// plan は採番を待つ vmid のままでも計画を出す（払い出しは reconcile）。
	if o, code := runBin(t, bin, append([]string{"plan"}, common...)...); code != 0 || !strings.Contains(o, "CREATE proxmox-vm//lab/app1") {
		t.Fatalf("[AC-143-2] plan before the allocation exit %d:\n%s", code, o)
	}
	for i := 0; i < 3; i++ {
		if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 {
			t.Fatalf("[AC-143-2] reconcile %d exit %d:\n%s", i, code, o)
		}
	}
	vm, ok := srv.VMByName("app1")
	if !ok || vm.VMID != 500 || vm.Status != "running" || vm.Config["memory"] != "2048" || !strings.Contains(vm.Config["net0"], "tag=20") || !strings.Contains(vm.Config["scsi0"], "size=8G") {
		t.Fatalf("[AC-143-2] app1 = %+v", vm)
	}
	for i := 0; i < 3; i++ {
		if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "Converged (no changes)") {
			t.Fatalf("[AC-143-2] reconcile after convergence %d exit %d, want converged:\n%s", i, code, o)
		}
	}

	// 手でメモリを変える → 戻る（走っている VM なので pending、その後は重ねない）。
	srv.SetConfig(500, "memory", "1024")
	if o, _ := runBin(t, bin, append([]string{"status"}, common...)...); !strings.Contains(o, "memory") {
		t.Errorf("[AC-143-2] status after a hand change does not show memory:\n%s", o)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-143-2] reconcile after a hand change exit %d:\n%s", code, o)
	}
	if vm, _ := srv.VM(500); vm.Pending["memory"] != "2048" {
		t.Errorf("[AC-143-2] pending = %v; want memory 2048", vm.Pending)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Fatalf("[AC-143-2] reconcile with the change pending exit %d, want converged:\n%s", code, o)
	}
	if srv.Calls("reboot") != 0 {
		t.Error("[AC-143-2] the VM was rebooted without apply_pending")
	}
}

func TestProxmoxVM_ProtectedVMIsAPlanError(t *testing.T) {
	// [AC-143-3]
	srv, cfg := pveRig(t)
	bin := buildBinary(t)
	common := pveIntent(t, cfg, `{"provider": "proxmox-vm", "id": "lab/sakutei-ctl", "fields": {"memory": "4096"}}`)
	before := srv.Calls("config") + srv.Calls("pending")
	for _, verb := range []string{"plan", "reconcile"} {
		o, code := runBin(t, bin, append([]string{verb}, common...)...)
		if code == 0 || !strings.Contains(o, "protected") {
			t.Errorf("[AC-143-3] %s with a protected VM declared: exit %d, want a refusal naming it protected:\n%s", verb, code, o)
		}
	}
	if srv.Calls("config")+srv.Calls("pending") != before {
		t.Error("[AC-143-3] the protected VM was read or written")
	}
	if vm, _ := srv.VM(100); vm.Config["memory"] != "2048" {
		t.Errorf("[AC-143-3] the protected VM changed: %v", vm.Config)
	}
}

func TestProxmoxVM_NotPruneableAndDeleteIsDestructive(t *testing.T) {
	// [AC-143-4]
	_, cfg := pveRig(t)
	p, err := registry.DefaultRegistry().Get(proxmox.Name, map[string]string{"proxmox_config": cfg})
	if err != nil {
		t.Fatal(err)
	}
	if p.Capabilities().Pruneable {
		t.Error("[AC-143-4] proxmox-vm is pruneable; a VM is a destructive type (§12.3)")
	}
}

func TestProxmoxVM_UndeclaredVMIsUnmanaged(t *testing.T) {
	// [AC-164-1]
	srv, cfg := pveRig(t)
	srv.AddVM(pvetest.VM{VMID: 300, Node: "pve1", Name: "left-over", Status: "running", Config: map[string]string{
		"cores": "1", "memory": "512", "scsi0": "local-lvm:vm-300-disk-0,size=4G",
	}})
	srv.AddVM(pvetest.VM{VMID: 301, Node: "pve1", Name: "kept", Status: "stopped", Config: map[string]string{
		"cores": "1", "memory": "512", "scsi0": "local-lvm:vm-301-disk-0,size=4G",
	}})
	bin := buildBinary(t)

	type entry struct {
		ID, State string
		LocalID   string `json:"local_id"`
	}
	statusJSON := func(common []string) (map[string]string, int) {
		t.Helper()
		o, code := runBin(t, bin, append([]string{"status", "--json"}, common...)...)
		var rep struct {
			Summary   struct{ Unmanaged, Total int }
			Resources []entry
		}
		if i := strings.Index(o, "{"); i < 0 || json.Unmarshal([]byte(o[i:]), &rep) != nil {
			t.Fatalf("[AC-164-1] status --json exit %d, not JSON:\n%s", code, o)
		}
		states := map[string]string{}
		for _, r := range rep.Resources {
			states[r.LocalID] = r.State
		}
		if rep.Summary.Total != len(rep.Resources) {
			t.Errorf("[AC-164-1] summary.total = %d, resources = %d", rep.Summary.Total, len(rep.Resources))
		}
		return states, code
	}

	// 宣言が 0 件：provider は --provider-config で設定されているだけ。
	none := pveIntent(t, cfg, ``)
	states, code := statusJSON(none)
	if code != 0 || len(states) != 2 || states["lab/left-over"] != "unmanaged" || states["lab/kept"] != "unmanaged" {
		t.Errorf("[AC-164-1] status with no VM declared: exit %d, states %v; want left-over and kept unmanaged, nothing else", code, states)
	}
	if o, _ := runBin(t, bin, append([]string{"status"}, none...)...); !strings.Contains(o, "lab/left-over") || !strings.Contains(o, "unmanaged=2") {
		t.Errorf("[AC-164-1] status (text) with no VM declared does not show left-over as unmanaged:\n%s", o)
	}

	// kept を宣言する：kept は宣言のもの、left-over だけが unmanaged。
	one := pveIntent(t, cfg, `{"provider": "proxmox-vm", "id": "lab/kept", "fields": {"vmid": "301", "cores": "1", "memory": "512", "power": "stopped"}}`)
	states, code = statusJSON(one)
	if code != 0 || states["lab/left-over"] != "unmanaged" || states["lab/kept"] != "conformant" || len(states) != 2 {
		t.Errorf("[AC-164-1] status with kept declared: exit %d, states %v; want left-over unmanaged and kept conformant", code, states)
	}

	// unmanaged は見せるだけ：reconcile は消さない（VM は prune しない型）。
	for _, common := range [][]string{none, one} {
		if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 {
			t.Fatalf("[AC-164-1] reconcile exit %d:\n%s", code, o)
		}
	}
	for _, id := range []int{100, 300, 301, 9000} {
		if _, ok := srv.VM(id); !ok {
			t.Errorf("[AC-164-1] VM %d was deleted", id)
		}
	}
	if srv.Calls("destroy") != 0 {
		t.Errorf("[AC-164-1] a VM was destroyed (%d destroy calls)", srv.Calls("destroy"))
	}
}
