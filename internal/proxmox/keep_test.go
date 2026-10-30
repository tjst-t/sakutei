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

package proxmox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// keepVM は keep のデータのディスク（scsi1）を持つ VM の宣言。
func keepVM(name, vmid, disks string) map[string]string {
	return map[string]string{
		proxmox.FieldCluster: "lab", proxmox.FieldName: name, proxmox.FieldVMID: vmid, proxmox.FieldNode: "pve1",
		proxmox.FieldTemplate: "debian-tmpl", proxmox.FieldDisks: disks,
	}
}

func TestParseDisksKeepAndVolume(t *testing.T) {
	ds, err := proxmox.ParseDisks("scsi1 volume=local-lvm:vm-120-disk-1 keep, scsi0 storage=local-lvm size=8G, scsi2 storage=local-lvm size=10G keep")
	if err != nil {
		t.Fatal(err)
	}
	if got := proxmox.FormatDisks(ds); got != "scsi0 storage=local-lvm size=8G, scsi1 storage=local-lvm volume=local-lvm:vm-120-disk-1 keep, scsi2 storage=local-lvm size=10G keep" {
		t.Errorf("FormatDisks = %q", got)
	}
	for _, bad := range []string{
		"scsi1 volume=vm-120-disk-1",                        // storage が無い
		"scsi1 volume=local-lvm:vm-120-disk-1 storage=ceph", // storage が違う
		"scsi1 storage=local-lvm size=1G keep keep",
		"keep",
		"scsi1 keep",
	} {
		if _, err := proxmox.ParseDisks(bad); err == nil {
			t.Errorf("ParseDisks(%q) succeeded", bad)
		}
	}
	if n := proxmox.VolumeOwner("local-lvm:vm-120-disk-1"); n != 120 {
		t.Errorf("VolumeOwner = %d", n)
	}
	if n := proxmox.VolumeOwner("local:120/vm-120-disk-0.qcow2"); n != 120 {
		t.Errorf("VolumeOwner (dir) = %d", n)
	}
}

// TestKeepMark は keep の印が VM の tags に置かれ、ほかの tags を残し、手で外すと
// 戻り、宣言から keep を外すと印も外れること。
func TestKeepMark(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	disks := "scsi0 storage=local-lvm size=4G, scsi1 storage=local-lvm size=2G keep"
	e.converge(t, "lab/app1", keepVM("app1", "120", disks), 4)
	vm, _ := e.srv.VM(120)
	if vm.Config["tags"] != proxmox.KeepTag("scsi1") || !strings.Contains(vm.Config["scsi1"], "vm-120-disk-1") {
		t.Fatalf("config = %v", vm.Config)
	}
	e.srv.SetConfig(120, "tags", "web")
	pl, _, err := e.plan(t, "lab/app1", keepVM("app1", "120", disks))
	if err != nil || len(pl.Ops) != 1 || pl.Ops[0].Destructive {
		t.Fatalf("plan after the mark was removed by hand = %+v, %v", pl.Ops, err)
	}
	e.converge(t, "lab/app1", keepVM("app1", "120", disks), 3)
	if vm, _ := e.srv.VM(120); vm.Config["tags"] != "web;"+proxmox.KeepTag("scsi1") {
		t.Errorf("tags = %q, want the mark back and the hand tag kept", vm.Config["tags"])
	}
	e.converge(t, "lab/app1", keepVM("app1", "120", "scsi1 storage=local-lvm size=2G"), 3)
	if vm, _ := e.srv.VM(120); vm.Config["tags"] != "web" {
		t.Errorf("tags = %q after keep was dropped from the declaration", vm.Config["tags"])
	}
}

// TestRecreateKeepsDisk は作り直し：新しい VM が volume= で前の VM（止めた）から
// keep のディスクを付け替え（承認の要る Op）、前の VM を消しても volume が残る
// こと。keep でないディスクは今どおり VM と一緒に消える。
func TestRecreateKeepsDisk(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	old := keepVM("app1", "120", "scsi0 storage=local-lvm size=4G, scsi1 storage=local-lvm size=2G keep, scsi2 storage=local-lvm size=1G")
	old[proxmox.FieldPower] = "running"
	e.converge(t, "lab/app1", old, 4)
	const data = "local-lvm:vm-120-disk-1"
	if _, ok := e.srv.Volume(data); !ok {
		t.Fatalf("no volume %s", data)
	}

	// 前の VM が走っている間は付け替えない（作った VM は残し、volume は動かさない）。
	next := keepVM("app2", "121", "scsi0 storage=local-lvm size=4G, scsi1 volume="+data+" keep")
	pl, _, err := e.plan(t, "lab/app2", next)
	if err != nil || len(pl.Ops) != 1 || !pl.Ops[0].RequiresApproval || !pl.Ops[0].Destructive || !strings.Contains(pl.Ops[0].ApprovalReason, data) {
		t.Fatalf("plan of the new VM = %+v, %v; want a Create requiring approval", pl.Ops, err)
	}
	err = e.p.Apply(context.Background(), pl)
	var te *provider.TransientError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "running") || !strings.Contains(err.Error(), "VM 121 was created") {
		t.Fatalf("Apply with the old VM running = %v; want a transient error saying the VM was created and the old VM runs", err)
	}
	if vm, _ := e.srv.VM(120); !strings.Contains(vm.Config["scsi1"], data) {
		t.Fatalf("the volume moved off the running VM: %v", vm.Config)
	}
	// 次の周は Update（承認の要る）で続ける。
	pl, _, _ = e.plan(t, "lab/app2", next)
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindUpdate || !pl.Ops[0].RequiresApproval {
		t.Fatalf("plan after the failed attach = %+v", pl.Ops)
	}

	// 前の VM を消す（absent の Delete）と、keep のディスクが残るので止めて消さない。
	del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id("lab/app1"), Destructive: true, RequiresApproval: true}}}
	err = e.p.Apply(context.Background(), del)
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "keep volume") || !strings.Contains(err.Error(), "scsi1="+data) {
		t.Fatalf("Delete of the VM holding the keep disk = %v", err)
	}
	if vm, ok := e.srv.VM(120); !ok || vm.Status != "stopped" {
		t.Fatalf("the old VM after the held delete = %+v, %v; want stopped, not destroyed", vm, ok)
	}
	if _, ok := e.srv.Volume(data); !ok {
		t.Fatal("the keep volume was destroyed")
	}

	// 止まったので付け替わる。volume は消えずに新しい VM の名前になる。
	e.converge(t, "lab/app2", next, 3)
	vm2, _ := e.srv.VM(121)
	moved := "local-lvm:vm-121-disk-1"
	if !strings.HasPrefix(vm2.Config["scsi1"], moved+",") || vm2.Config["tags"] != proxmox.KeepTag("scsi1") {
		t.Fatalf("new VM config = %v", vm2.Config)
	}
	if size, ok := e.srv.Volume(moved); !ok || size != 2<<30 {
		t.Fatalf("moved volume %s = %d, %v", moved, size, ok)
	}
	if vm1, _ := e.srv.VM(120); vm1.Config["scsi1"] != "" {
		t.Errorf("the old VM still has scsi1: %v", vm1.Config)
	}

	// もう keep のディスクは無いので前の VM は消え、keep でないディスクは一緒に消える。
	if err := e.p.Apply(context.Background(), del); err != nil {
		t.Fatalf("Delete after the move: %v", err)
	}
	if _, ok := e.srv.VM(120); ok {
		t.Fatal("the old VM was not destroyed")
	}
	if _, ok := e.srv.Volume("local-lvm:vm-120-disk-2"); ok {
		t.Error("the old VM's non-keep disk survived")
	}
	if _, ok := e.srv.Volume(moved); !ok {
		t.Error("the moved keep volume was destroyed with the old VM")
	}
	e.converge(t, "lab/app2", next, 2)
}

// TestUnusedVolumes は外したままの volume（VM の unusedN）が List に出て、Read・
// 宣言はできず、keep の印のある VM は unused が残る間は消えないこと。
// volume= で付け直すと unused から消える。
func TestUnusedVolumes(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	decl := keepVM("app1", "120", "scsi0 storage=local-lvm size=4G, scsi1 storage=local-lvm size=2G keep")
	e.converge(t, "lab/app1", decl, 4)
	// 手で外す（Proxmox の Detach）：unused0 になる。
	c, _ := proxmox.NewClient(proxmox.ClientConfig{Endpoint: e.srv.URL, TokenID: e.srv.TokenID(),
		Secret: func(context.Context) (string, error) { return e.srv.Secret(), nil }})
	if err := c.SetConfig(context.Background(), "pve1", 120, map[string][]string{"delete": {"scsi1"}}); err != nil {
		t.Fatal(err)
	}
	lr, err := e.p.List(context.Background(), core.NewScope(nil))
	if err != nil || lr.Status != core.ListComplete {
		t.Fatalf("List = %+v, %v", lr, err)
	}
	var locals []string
	for _, x := range lr.IDs {
		locals = append(locals, x.Local)
	}
	if strings.Join(locals, " ") != "lab/app1 lab/app1/unused0" {
		t.Fatalf("List = %v", locals)
	}
	if obs, err := e.p.Read(context.Background(), "lab/app1/unused0"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read unused = %+v, %v; want unknown (an id to report, not a resource)", obs, err)
	}
	if err := e.p.ValidateIntent("lab/app1/unused0", nil); err == nil || !strings.Contains(err.Error(), "detached") {
		t.Errorf("ValidateIntent(unused) = %v", err)
	}
	if err := e.p.ValidateAbsent("lab/app1/unused0"); err == nil {
		t.Error("ValidateAbsent(unused) accepted")
	}
	// keep の印があり unused が残るので、消さない。
	del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id("lab/app1"), Destructive: true}}}
	if err := e.p.Apply(context.Background(), del); err == nil || !strings.Contains(err.Error(), "unused0=local-lvm:vm-120-disk-1") {
		t.Fatalf("Delete with an unused keep volume = %v", err)
	}
	// 同じ VM に volume= で付け直す（承認の要る Op）。unused は消える。
	again := keepVM("app1", "120", "scsi0 storage=local-lvm size=4G, scsi1 volume=local-lvm:vm-120-disk-1 keep")
	pl, _, _ := e.plan(t, "lab/app1", again)
	if len(pl.Ops) != 1 || !pl.Ops[0].RequiresApproval {
		t.Fatalf("plan of the reattach = %+v", pl.Ops)
	}
	e.converge(t, "lab/app1", again, 3)
	vm, _ := e.srv.VM(120)
	if !strings.HasPrefix(vm.Config["scsi1"], "local-lvm:vm-120-disk-1") || vm.Config["unused0"] != "" {
		t.Fatalf("config after the reattach = %v", vm.Config)
	}
	if lr, _ := e.p.List(context.Background(), core.NewScope(nil)); len(lr.IDs) != 1 {
		t.Errorf("List after the reattach = %+v", lr.IDs)
	}
	// 付いたあとは承認を求めない（volume の名前は比べない）。
	if pl, _, _ := e.plan(t, "lab/app1", again); len(pl.Ops) != 0 {
		t.Errorf("plan after the reattach = %+v", pl.Ops)
	}
}

// TestAttachOrphanVolume は持ち主の VM が無い volume をそのまま付け、無い volume は
// 付けられないこと。触らない VM の volume は付け替えない。
func TestAttachOrphanVolume(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.srv.AddVolume("local-lvm:vm-150-disk-0", 3<<30)
	decl := keepVM("app3", "122", "scsi0 storage=local-lvm size=4G, scsi1 volume=local-lvm:vm-150-disk-0 keep")
	e.converge(t, "lab/app3", decl, 4)
	if vm, _ := e.srv.VM(122); vm.Config["scsi1"] != "local-lvm:vm-150-disk-0,size=3G" {
		t.Fatalf("config = %v", vm.Config)
	}

	bad := keepVM("app4", "123", "scsi0 storage=local-lvm size=4G, scsi1 volume=local-lvm:vm-100-disk-0")
	pl, _, _ := e.plan(t, "lab/app4", bad)
	if err := e.p.Apply(context.Background(), pl); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("attach of the protected VM's volume = %v", err)
	}
	if vm, _ := e.srv.VM(100); !strings.HasPrefix(vm.Config["scsi0"], "local-lvm:vm-100-disk-0") {
		t.Error("the protected VM's disk was touched")
	}
}
