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
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

const (
	sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEQHShMqBxoVWaNhs0o1VR7qEL2eQZJqgpnZrDZvbS0o sakutei@ctl"
)

// clock は進められる時計（force_after の計測）。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	srv   *pvetest.Server
	p     *proxmox.Provider
	clock *clock
}

// newEnv は偽の Proxmox と、それに向けた "lab" クラスタの Provider を返す。
// 偽サーバには触らない VM（100 sakutei-ctl）とテンプレート（9000）がある。
func newEnv(t *testing.T, opts pvetest.Options, taskWait time.Duration) *env {
	t.Helper()
	return newEnvBase(t, opts, taskWait, "")
}

// newEnvBase は newEnv の、Client の API のパスの前置き（api_base_path）を
// 変えられる版（"" は既定）。
func newEnvBase(t *testing.T, opts pvetest.Options, taskWait time.Duration, apiBasePath string) *env {
	t.Helper()
	srv := pvetest.New(opts)
	t.Cleanup(srv.Close)
	srv.AddVM(pvetest.VM{VMID: 100, Node: "pve1", Name: "sakutei-ctl", Status: "running", Config: map[string]string{
		"cores": "2", "memory": "2048", "scsi0": "local-lvm:vm-100-disk-0,size=16G", "net0": "virtio=BC:24:11:00:01:00,bridge=vmbr0",
	}})
	srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true, Config: map[string]string{
		"cores": "1", "memory": "1024", "scsihw": "virtio-scsi-single",
		"scsi0": "local-lvm:base-9000-disk-0,size=4G", "ide2": "local-lvm:vm-9000-cloudinit,media=cdrom",
		"net0": "virtio=BC:24:11:00:90:00,bridge=vmbr0",
	}})
	c, err := proxmox.NewClient(proxmox.ClientConfig{
		Endpoint: srv.URL, APIBasePath: apiBasePath, TokenID: srv.TokenID(), Timeout: 2 * time.Second,
		Secret: func(context.Context) (string, error) { return srv.Secret(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)}
	p := proxmox.New(map[string]proxmox.Cluster{"lab": {
		Client: c, Node: "pve1", TaskWait: taskWait,
		ProtectedVMIDs: []int{100}, ProtectedNames: []string{"sakutei-ctl"},
	}}, proxmox.Options{Now: clk.Now, PollInterval: time.Millisecond})
	return &env{srv: srv, p: p, clock: clk}
}

func id(local string) core.ID { return core.NewID(proxmox.Name, core.NewScope(nil), local) }

func (e *env) plan(t *testing.T, local string, fields map[string]string) (core.Plan, core.Observation, error) {
	t.Helper()
	obs, err := e.p.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read %s: %v", local, err)
	}
	pl, err := diff.ForProvider(e.p).Plan(obs, diff.DesiredResource{ID: id(local), Fields: fields},
		cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
	return pl, obs, err
}

// cycle は 1 周（Read → Plan → Apply）を回し、Plan の op を返す。
func (e *env) cycle(t *testing.T, local string, fields map[string]string) []core.Op {
	t.Helper()
	pl, obs, err := e.plan(t, local, fields)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if obs.Availability != core.AvailabilityOk {
		return nil
	}
	if len(pl.Ops) > 0 {
		if err := e.p.Apply(context.Background(), pl); err != nil {
			t.Fatalf("Apply %+v: %v", pl.Ops, err)
		}
	}
	return pl.Ops
}

// converge は収束するまで（最大 n 周）回す。
func (e *env) converge(t *testing.T, local string, fields map[string]string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if ops := e.cycle(t, local, fields); len(ops) == 0 {
			pl, obs, err := e.plan(t, local, fields)
			if err == nil && obs.Availability == core.AvailabilityOk && len(pl.Ops) == 0 {
				return
			}
		}
	}
	pl, obs, err := e.plan(t, local, fields)
	t.Fatalf("not converged after %d cycles: availability %v, ops %+v, %v", n, obs.Availability, pl.Ops, err)
}

func web1() map[string]string {
	return map[string]string{
		proxmox.FieldCluster:   "lab",
		proxmox.FieldName:      "web1",
		proxmox.FieldVMID:      "200",
		proxmox.FieldNode:      "pve1",
		proxmox.FieldCores:     "2",
		proxmox.FieldMemory:    "2048",
		proxmox.FieldDisks:     "scsi0 storage=local-lvm size=8G",
		proxmox.FieldNICs:      "net0 bridge=vmbr0 vlan=10 mac=bc:24:11:00:00:01 model=virtio",
		proxmox.FieldCIUser:    "ops",
		proxmox.FieldCISSHKeys: sshKey,
		proxmox.FieldCIIP:      "192.0.2.10/24",
		proxmox.FieldCIGateway: "192.0.2.1",
		proxmox.FieldCIDNS:     "192.0.2.53 192.0.2.54",
		proxmox.FieldPower:     "running",
	}
}

// TestAPIBasePath は API のパスの前置きを変えた偽サーバ（模擬）に、設定した
// 前置きで作って収束でき、既定の前置きのままでは観測できない（unknown）こと
// （#160）。
func TestAPIBasePath(t *testing.T) {
	for _, tc := range []struct{ name, server, client string }{
		{"default", "", ""},
		{"explicit default", "", "/api2/json"},
		{"mock prefix", "/mock/pve/api2/json", "/mock/pve/api2/json/"},
		{"no prefix", "/", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvBase(t, pvetest.Options{BasePath: tc.server}, time.Second, tc.client)
			e.converge(t, "lab/web1", web1(), 3)
			if vm, ok := e.srv.VM(200); !ok || vm.Name != "web1" {
				t.Fatalf("VM 200 = %+v, %v", vm, ok)
			}
			lr, err := e.p.List(context.Background(), core.NewScope(nil))
			if err != nil || lr.Status != core.ListComplete || len(lr.IDs) != 1 || lr.IDs[0].Local != "lab/web1" {
				t.Errorf("List = %+v, %v", lr, err)
			}
		})
	}
	e := newEnvBase(t, pvetest.Options{BasePath: "/mock/pve/api2/json"}, time.Second, "")
	obs, err := e.p.Read(context.Background(), "lab/web1")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read with the default prefix against a moved API = %+v, %v; want unknown", obs, err)
	}
	if _, err := proxmox.NewClient(proxmox.ClientConfig{Endpoint: e.srv.URL, APIBasePath: "api2/json", TokenID: e.srv.TokenID(),
		Secret: func(context.Context) (string, error) { return "", nil }}); err == nil {
		t.Error("NewClient accepted api_base_path without a leading /")
	}
}

func TestValidateProvider(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, 0)
	if err := sdk.ValidateProvider(e.p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	caps := e.p.Capabilities()
	if !caps.TrueObserve || !caps.List || !caps.BulkObserve || caps.Pruneable || !caps.Converging {
		t.Errorf("Capabilities = %+v, want the full tier, not pruneable", caps)
	}
}

func TestContract(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.srv.AddVM(pvetest.VM{VMID: 300, Node: "pve2", Name: "other", Config: map[string]string{"cores": "1", "memory": "512"}})
	ct := sdk.ContractTester{
		Provider:      e.p,
		Lister:        e.p,
		Scope:         core.NewScope(nil),
		SampleID:      "lab/web1",
		SampleDesired: web1(),
		MakeUnreachable: func() (func(), error) {
			e.srv.SetUnreachable(true)
			return func() { e.srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
}

func TestBulkEquivalence(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.converge(t, "lab/web1", web1(), 3)
	e.srv.AddVM(pvetest.VM{VMID: 301, Node: "pve2", Name: "db1", Status: "running", Config: map[string]string{
		"cores": "4", "memory": "current=8192", "virtio0": "ceph:vm-301-disk-0,size=32G", "net0": "e1000=BC:24:11:00:03:01,bridge=vmbr1,tag=30",
	}})
	e.srv.AddVM(pvetest.VM{VMID: 302, Node: "pve2", Name: "locked", Lock: "backup", Config: map[string]string{"memory": "512"}})
	bt := sdk.BulkEquivalenceTester{Provider: e.p, Lister: e.p, Scope: core.NewScope(nil)}
	bt.Run(t)

	before := e.srv.Calls("resources")
	bo, err := e.p.BulkObserve(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if n := e.srv.Calls("resources") - before; n != 1 {
		t.Errorf("BulkObserve called /cluster/resources %d times, want 1", n)
	}
	if o := bo.Observations[id("lab/locked").Key()]; o.Availability != core.AvailabilityConverging {
		t.Errorf("locked VM: %v, want converging", o.Availability)
	}
	if _, ok := bo.Observations[id("lab/sakutei-ctl").Key()]; ok {
		t.Error("BulkObserve returned the protected VM")
	}
}

// テンプレートから作る → 適合。template は差分に出ない。
func TestCreateFromTemplate(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	want := map[string]string{
		proxmox.FieldVMID:      "201",
		proxmox.FieldTemplate:  "debian-tmpl",
		proxmox.FieldCores:     "2",
		proxmox.FieldMemory:    "4G",
		proxmox.FieldDisks:     "scsi0 storage=local-lvm size=10G, scsi1 storage=local-lvm size=20G",
		proxmox.FieldNICs:      "net0 bridge=vmbr0 vlan=20",
		proxmox.FieldCIUser:    "ops",
		proxmox.FieldCISSHKeys: sshKey,
		proxmox.FieldCIIP:      "192.0.2.21/24",
		proxmox.FieldCIGateway: "192.0.2.1",
		proxmox.FieldPower:     "running",
	}
	ops := e.cycle(t, "lab/app1", want)
	if len(ops) != 1 || ops[0].Kind != core.OpKindCreate {
		t.Fatalf("first cycle ops = %+v, want one Create", ops)
	}
	e.converge(t, "lab/app1", want, 3)
	vm, ok := e.srv.VM(201)
	if !ok || vm.Status != "running" || vm.Config["memory"] != "4096" || vm.Config["cores"] != "2" {
		t.Fatalf("VM 201 = %+v", vm)
	}
	if !strings.Contains(vm.Config["scsi0"], "size=10G") || !strings.Contains(vm.Config["scsi1"], "size=20G") || !strings.Contains(vm.Config["net0"], "tag=20") {
		t.Errorf("config = %v", vm.Config)
	}
	if vm.Config["ide2"] != "local-lvm:vm-201-cloudinit,media=cdrom" || vm.Config["ipconfig0"] != "ip=192.0.2.21/24,gw=192.0.2.1" {
		t.Errorf("cloud-init = %v", vm.Config)
	}
	if e.srv.Calls("clone") != 1 || e.srv.Calls("create") != 0 {
		t.Errorf("clone %d, create %d", e.srv.Calls("clone"), e.srv.Calls("create"))
	}
	// 何周でも drift が出ない。
	for i := 0; i < 5; i++ {
		if ops := e.cycle(t, "lab/app1", want); len(ops) != 0 {
			t.Fatalf("cycle %d: ops %+v, want none", i, ops)
		}
	}
	if _, obs, _ := e.plan(t, "lab/app1", want); obs.State[proxmox.FieldPending].Value != "" {
		t.Errorf("pending = %v", obs.State[proxmox.FieldPending])
	}
}

// 手でメモリを変える → 戻る。走っている VM では pending になり、重ねて
// 出さない。apply_pending: reboot を宣言すると再起動して効かせる。
func TestManualChangeAndPending(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	want := web1()
	e.converge(t, "lab/web1", want, 3)
	e.srv.SetConfig(200, "memory", "1024")

	ops := e.cycle(t, "lab/web1", want)
	if len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Fields[0].Field != proxmox.FieldMemory || ops[0].Fields[0].From != "1024" {
		t.Fatalf("ops = %+v, want memory 1024 → 2048", ops)
	}
	if vm, _ := e.srv.VM(200); vm.Pending["memory"] != "2048" {
		t.Fatalf("pending = %v, want memory 2048 (the VM is running)", vm.Pending)
	}
	configCalls := e.srv.Calls("config")
	for i := 0; i < 3; i++ {
		pl, obs, err := e.plan(t, "lab/web1", want)
		if err != nil || len(pl.Ops) != 0 {
			t.Fatalf("pending cycle %d: ops %+v, %v; want none", i, pl.Ops, err)
		}
		if obs.State[proxmox.FieldPending].Value != proxmox.FieldMemory {
			t.Errorf("pending = %v", obs.State[proxmox.FieldPending])
		}
	}
	if e.srv.Calls("config") != configCalls || e.srv.Calls("reboot") != 0 {
		t.Errorf("the pending change was written again or rebooted without being asked")
	}

	want[proxmox.FieldApplyPending] = proxmox.ApplyPendingReboot
	ops = e.cycle(t, "lab/web1", want)
	if len(ops) != 1 || ops[0].Fields[0].Field != proxmox.FieldApplyPending || ops[0].Destructive {
		t.Fatalf("ops = %+v, want a non-destructive apply_pending", ops)
	}
	if vm, _ := e.srv.VM(200); vm.Config["memory"] != "2048" || len(vm.Pending) != 0 || e.srv.Calls("reboot") != 1 {
		t.Fatalf("after reboot: %+v", vm)
	}
	for i := 0; i < 3; i++ {
		if ops := e.cycle(t, "lab/web1", want); len(ops) != 0 {
			t.Fatalf("cycle %d after reboot: %+v", i, ops)
		}
	}
}

// 走っている VM を手で変えると Proxmox はそれを pending に置く。今の値は宣言
// どおりでも、再起動で効く手の変更は差分にして書き戻す。
func TestManualPendingChangeIsDrift(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	want := web1()
	e.converge(t, "lab/web1", want, 3)
	e.srv.SetPending(200, "cores", "8")
	ops := e.cycle(t, "lab/web1", want)
	if len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Fields[0].Field != proxmox.FieldCores {
		t.Fatalf("ops = %+v, want cores", ops)
	}
	if vm, _ := e.srv.VM(200); len(vm.Pending) != 0 || vm.Config["cores"] != "2" {
		t.Fatalf("after Apply: pending %v, cores %s", vm.Pending, vm.Config["cores"])
	}
	if ops := e.cycle(t, "lab/web1", want); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// Proxmox が書き換える表記（大文字の MAC・"current=" のメモリ・M のディスク）は
// drift にならない。宣言していないディスク・NIC は比べない。
func TestNormalisationNoDrift(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.srv.AddVM(pvetest.VM{VMID: 210, Node: "pve2", Name: "legacy", Status: "running", Config: map[string]string{
		"cores": "4", "memory": "current=8192", "scsi0": "ceph:vm-210-disk-0,iothread=1,size=32768M",
		"scsi1": "ceph:vm-210-disk-1,size=100G", "ide2": "local:iso/x.iso,media=cdrom",
		"net0": "virtio=BC:24:11:AB:CD:EF,bridge=vmbr0,firewall=1,tag=10", "net1": "e1000=BC:24:11:00:00:09,bridge=vmbr9",
		"ciuser": "ops", "sshkeys": "ssh-ed25519%20AAAA%2B%2F%3D%20me%0A", "ipconfig0": "ip=192.0.2.30/24,gw=192.0.2.1,ip6=auto",
		"nameserver": "192.0.2.53",
	}})
	want := map[string]string{
		proxmox.FieldNode: "pve2", proxmox.FieldVMID: "210", proxmox.FieldCores: "4", proxmox.FieldMemory: "8G",
		proxmox.FieldDisks:     "scsi0 storage=ceph size=32G",
		proxmox.FieldNICs:      "net0 bridge=vmbr0 vlan=10 mac=BC:24:11:ab:cd:ef",
		proxmox.FieldCISSHKeys: "ssh-ed25519 AAAA+/= me\n",
		proxmox.FieldCIIP:      "192.0.2.30/24", proxmox.FieldCIGateway: "192.0.2.1", proxmox.FieldCIDNS: "192.0.2.53",
		proxmox.FieldPower: "running", proxmox.FieldTemplate: "debian-tmpl",
	}
	for i := 0; i < 3; i++ {
		pl, _, err := e.plan(t, "lab/legacy", want)
		if err != nil || len(pl.Ops) != 0 {
			t.Fatalf("ops %+v, %v; want none", pl.Ops, err)
		}
	}
}

// ciupgrade（Backlog #170）：宣言すれば作るときに渡し（空から作る・複製）、
// 手で変えれば戻す。書かなければ渡さず、観測の値が何でも比べない。
func TestCIUpgrade(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)

	// 空から作る：作るときの設定に入る。
	want := web1()
	want[proxmox.FieldCIUpgrade] = "false"
	e.cycle(t, "lab/web1", want)
	if vm, _ := e.srv.VM(200); vm.Config["ciupgrade"] != "0" {
		t.Fatalf("after create: ciupgrade = %q, want 0 (config %v)", vm.Config["ciupgrade"], vm.Config)
	}
	e.converge(t, "lab/web1", want, 3)
	if _, obs, _ := e.plan(t, "lab/web1", want); obs.State[proxmox.FieldCIUpgrade] != (core.TypedValue{Kind: core.FieldKindBool, Value: false}) {
		t.Errorf("observed ciupgrade = %+v, want bool false", obs.State[proxmox.FieldCIUpgrade])
	}

	// 手で変える → 戻す。
	e.srv.SetConfig(200, "ciupgrade", "1")
	ops := e.cycle(t, "lab/web1", want)
	if len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Fields[0].Field != proxmox.FieldCIUpgrade || ops[0].Fields[0].From != "true" {
		t.Fatalf("ops = %+v, want ciupgrade true → false", ops)
	}
	if vm, _ := e.srv.VM(200); vm.Config["ciupgrade"] != "0" {
		t.Fatalf("after Apply: ciupgrade = %q, want 0", vm.Config["ciupgrade"])
	}
	if ops := e.cycle(t, "lab/web1", want); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}

	// true を宣言すると、書かれていない（Proxmox の既定 1）ことを true と見る。
	want[proxmox.FieldCIUpgrade] = "true"
	e.converge(t, "lab/web1", want, 3)
	e.srv.SetConfig(200, "ciupgrade", "")
	if pl, _, err := e.plan(t, "lab/web1", want); err != nil || len(pl.Ops) != 0 {
		t.Errorf("ciupgrade unset vs true: ops %+v, %v; want none", pl.Ops, err)
	}

	// 複製：作った後、走らせる前に cloud-init と一緒に書く。
	app := map[string]string{
		proxmox.FieldVMID: "201", proxmox.FieldTemplate: "debian-tmpl", proxmox.FieldDisks: "scsi0 storage=local-lvm size=4G",
		proxmox.FieldCIUser: "ops", proxmox.FieldCIUpgrade: "false", proxmox.FieldPower: "running",
	}
	e.converge(t, "lab/app1", app, 3)
	if vm, _ := e.srv.VM(201); vm.Config["ciupgrade"] != "0" || vm.Status != "running" {
		t.Errorf("clone: ciupgrade = %q, status %s", vm.Config["ciupgrade"], vm.Status)
	}

	// 書かなければ渡さず、観測の値（0 でも）を比べない。
	plain := web1()
	plain[proxmox.FieldName], plain[proxmox.FieldVMID] = "web2", "202"
	e.converge(t, "lab/web2", plain, 3)
	if vm, _ := e.srv.VM(202); func() bool { _, ok := vm.Config["ciupgrade"]; return ok }() {
		t.Errorf("ciupgrade was passed without being declared: %v", vm.Config)
	}
	e.srv.SetConfig(202, "ciupgrade", "0")
	if pl, _, err := e.plan(t, "lab/web2", plain); err != nil || len(pl.Ops) != 0 {
		t.Errorf("undeclared ciupgrade: ops %+v, %v; want none", pl.Ops, err)
	}
}

func TestDisks(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	want := web1()
	e.converge(t, "lab/web1", want, 3)

	want[proxmox.FieldDisks] = "scsi0 storage=local-lvm size=16G, scsi1 storage=local-lvm size=4G"
	e.converge(t, "lab/web1", want, 3)
	vm, _ := e.srv.VM(200)
	if !strings.Contains(vm.Config["scsi0"], "size=16G") || !strings.Contains(vm.Config["scsi1"], "size=4G") || e.srv.Calls("resize") != 1 {
		t.Fatalf("config = %v, resize calls %d", vm.Config, e.srv.Calls("resize"))
	}

	for decl, msg := range map[string]string{
		"scsi0 storage=local-lvm size=8G": "never shrunk",
		"scsi0 storage=ceph size=16G":     "moving a disk is not managed",
	} {
		want[proxmox.FieldDisks] = decl
		if _, _, err := e.plan(t, "lab/web1", want); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: err = %v, want %q", decl, err, msg)
		}
	}
	// 宣言から外したディスクは消さない。
	want[proxmox.FieldDisks] = "scsi0 storage=local-lvm size=16G"
	if ops := e.cycle(t, "lab/web1", want); len(ops) != 0 {
		t.Errorf("ops = %+v, want none", ops)
	}
}

// vmid・node が今の VM と違えば計画のエラー。
func TestVMIDAndNodeArePlanErrors(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.converge(t, "lab/web1", web1(), 3)
	for f, v := range map[string]string{proxmox.FieldVMID: "201", proxmox.FieldNode: "pve2"} {
		want := web1()
		want[f] = v
		if _, _, err := e.plan(t, "lab/web1", want); err == nil {
			t.Errorf("%s %s: no plan error", f, v)
		}
	}
}

// 触らない VM は List に出さず、宣言すれば計画のエラー、Read・Apply もしない。
func TestProtected(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	if err := e.p.ValidateIntent("lab/sakutei-ctl", map[string]string{}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("ValidateIntent(protected name) = %v", err)
	}
	if err := e.p.ValidateIntent("lab/other", map[string]string{proxmox.FieldVMID: "100"}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("ValidateIntent(protected vmid) = %v", err)
	}
	lr, err := e.p.List(context.Background(), core.NewScope(nil))
	if err != nil || lr.Status != core.ListComplete {
		t.Fatalf("List = %+v, %v", lr, err)
	}
	for _, x := range lr.IDs {
		if x.Local == "lab/sakutei-ctl" || x.Local == "lab/debian-tmpl" {
			t.Errorf("List returned %s", x.Local)
		}
	}
	if obs, err := e.p.Read(context.Background(), "lab/sakutei-ctl"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read(protected) = %v, %v", obs.Availability, err)
	}
	// 名前を変えた触らない VM（vmid で守る）も触らない。
	e.srv.SetConfig(100, "name", "renamed")
	ctl, _ := e.srv.VM(100)
	ctl.Name = "renamed"
	e.srv.AddVM(ctl)
	if obs, err := e.p.Read(context.Background(), "lab/renamed"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read(protected vmid) = %v, %v", obs.Availability, err)
	}
	err = e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id("lab/renamed"), Destructive: true}}})
	if err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("Apply(delete protected) = %v", err)
	}
	if _, ok := e.srv.VM(100); !ok || e.srv.Calls("stop") != 0 || e.srv.Calls("destroy") != 0 {
		t.Error("the protected VM was touched")
	}
	// 採番が触らない vmid を配っても作らない。
	want := web1()
	want[proxmox.FieldVMID] = "100"
	err = e.p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindCreate, Target: id("lab/web1"), Fields: fieldDiffs(want)}}})
	if err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("Apply(create with protected vmid) = %v", err)
	}
}

func fieldDiffs(m map[string]string) []core.FieldDiff {
	var out []core.FieldDiff
	for k, v := range m {
		out = append(out, core.FieldDiff{Field: k, To: v})
	}
	return out
}

// 届かない・認証の失敗・ノードが落ちている → unknown。List は error。
func TestUnreachableIsUnknown(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.converge(t, "lab/web1", web1(), 3)

	e.srv.SetUnreachable(true)
	if obs, err := e.p.Read(context.Background(), "lab/web1"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("unreachable: Read = %v, %v", obs.Availability, err)
	} else if !errors.As(err, new(*provider.TransientError)) {
		t.Errorf("unreachable: %T, want transient", err)
	}
	if _, err := e.p.List(context.Background(), core.NewScope(nil)); err == nil {
		t.Error("unreachable: List succeeded")
	}
	if _, err := e.p.BulkObserve(context.Background(), core.NewScope(nil)); err == nil {
		t.Error("unreachable: BulkObserve succeeded")
	}
	e.srv.SetUnreachable(false)

	e.srv.SetFailStatus(500)
	if obs, _ := e.p.Read(context.Background(), "lab/web1"); obs.Availability != core.AvailabilityUnknown {
		t.Errorf("500: Read = %v", obs.Availability)
	}
	e.srv.SetFailStatus(0)

	e.srv.SetNodeOffline("pve1", true)
	if obs, err := e.p.Read(context.Background(), "lab/web1"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("node offline: Read = %v, %v", obs.Availability, err)
	}
	e.srv.SetNodeOffline("pve1", false)

	bad, _ := proxmox.NewClient(proxmox.ClientConfig{Endpoint: e.srv.URL, TokenID: e.srv.TokenID(),
		Secret: func(context.Context) (string, error) { return "wrong-secret-value", nil }})
	p := proxmox.New(map[string]proxmox.Cluster{"lab": {Client: bad, Node: "pve1"}}, proxmox.Options{})
	obs, err := p.Read(context.Background(), "lab/web1")
	if err == nil || obs.Availability != core.AvailabilityUnknown || !errors.As(err, new(*provider.PermanentError)) {
		t.Errorf("auth failure: Read = %v, %T %v", obs.Availability, err, err)
	}
	if strings.Contains(err.Error(), "wrong-secret-value") {
		t.Errorf("the error shows the token secret: %v", err)
	}
}

// 非同期の仕事（UPID）は終わるまで converging。失敗は 1 回だけ Unknown で知らせる。
func TestAsyncTasks(t *testing.T) {
	e := newEnv(t, pvetest.Options{TaskPolls: 3}, 0)
	want := web1()
	want[proxmox.FieldTemplate] = "debian-tmpl"
	ops := e.cycle(t, "lab/web1", want)
	if len(ops) != 1 || ops[0].Kind != core.OpKindCreate {
		t.Fatalf("ops = %+v", ops)
	}
	if _, ok := e.p.TrackedTask("lab/web1"); !ok {
		t.Fatal("the clone task is not tracked")
	}
	if obs, _ := e.p.Read(context.Background(), "lab/web1"); obs.Availability != core.AvailabilityConverging {
		t.Fatalf("during clone: %v, want converging", obs.Availability)
	}
	// 記録を失っても（再起動）、lock が残る間は converging。
	p2 := proxmox.New(map[string]proxmox.Cluster{"lab": clusterOf(t, e)}, proxmox.Options{})
	if obs, _ := p2.Read(context.Background(), "lab/web1"); obs.Availability != core.AvailabilityConverging {
		t.Errorf("lost UPID, locked VM: %v, want converging", obs.Availability)
	}
	e.converge(t, "lab/web1", want, 30)
	if vm, _ := e.srv.VM(200); vm.Status != "running" {
		t.Fatalf("VM = %+v", vm)
	}

	// 失敗した仕事。
	e.srv.FailNextTask("qmshutdown", "VM quit/powerdown failed - got timeout")
	want[proxmox.FieldPower] = "stopped"
	e.cycle(t, "lab/web1", want)
	var lastErr error
	for i := 0; i < 5; i++ {
		obs, err := e.p.Read(context.Background(), "lab/web1")
		if err != nil {
			if obs.Availability != core.AvailabilityUnknown || !strings.Contains(err.Error(), "powerdown failed") {
				t.Fatalf("failed task: %v, %v", obs.Availability, err)
			}
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatal("the failed task was not reported")
	}
	if obs, err := e.p.Read(context.Background(), "lab/web1"); err != nil || obs.Availability != core.AvailabilityOk {
		t.Errorf("after the failure was reported: %v, %v", obs.Availability, err)
	}
}

func clusterOf(t *testing.T, e *env) proxmox.Cluster {
	c, err := proxmox.NewClient(proxmox.ClientConfig{Endpoint: e.srv.URL, TokenID: e.srv.TokenID(),
		Secret: func(context.Context) (string, error) { return e.srv.Secret(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return proxmox.Cluster{Client: c, Node: "pve1", ProtectedVMIDs: []int{100}}
}

// 正常停止が効かなければ、graceful_then_force の宣言で force_after の後に
// 強制停止する。その Op は破壊的。
func TestForceStopIsDestructive(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	want := web1()
	e.converge(t, "lab/web1", want, 3)
	want[proxmox.FieldPower] = "stopped"
	want[proxmox.FieldPowerOff] = proxmox.PowerOffGracefulThenForce
	want[proxmox.FieldForceAfter] = "5m"

	e.srv.FailNextTask("qmshutdown", "VM quit/powerdown failed - got timeout")
	pl, _, err := e.plan(t, "lab/web1", want)
	if err != nil || len(pl.Ops) != 1 || pl.Ops[0].Destructive {
		t.Fatalf("first plan = %+v, %v; want a graceful stop", pl.Ops, err)
	}
	if err := e.p.Apply(context.Background(), pl); err == nil {
		t.Fatal("Apply of the failing shutdown succeeded")
	}
	if _, ok := e.p.GracefulSentAt("lab/web1"); !ok {
		t.Fatal("the shutdown time is not kept")
	}
	e.clock.Advance(6 * time.Minute)
	pl, _, err = e.plan(t, "lab/web1", want)
	if err != nil || len(pl.Ops) != 1 || !pl.Ops[0].Destructive {
		t.Fatalf("plan after force_after = %+v, %v; want destructive", pl.Ops, err)
	}
	if err := e.p.Apply(context.Background(), pl); err != nil {
		t.Fatal(err)
	}
	if vm, _ := e.srv.VM(200); vm.Status != "stopped" || e.srv.Calls("stop") != 1 {
		t.Errorf("VM = %s, stop calls %d", vm.Status, e.srv.Calls("stop"))
	}
	if ops := e.cycle(t, "lab/web1", want); len(ops) != 0 {
		t.Errorf("ops = %+v", ops)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.converge(t, "lab/web1", web1(), 3)
	del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: id("lab/web1"), Destructive: true}}}
	if err := e.p.Apply(context.Background(), del); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.srv.VM(200); ok || e.srv.Calls("stop") != 1 {
		t.Fatalf("the VM was not stopped and destroyed")
	}
	if err := e.p.Apply(context.Background(), del); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if obs, err := e.p.Read(context.Background(), "lab/web1"); err != nil || obs.State != nil {
		t.Errorf("Read after delete = %+v, %v", obs, err)
	}
}

func TestListErrors(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.srv.AddVM(pvetest.VM{VMID: 400, Node: "pve1", Name: "dup"})
	e.srv.AddVM(pvetest.VM{VMID: 401, Node: "pve2", Name: "dup"})
	if _, err := e.p.List(context.Background(), core.NewScope(nil)); err == nil || !strings.Contains(err.Error(), "both named dup") {
		t.Errorf("List with duplicates = %v", err)
	}
	if obs, err := e.p.Read(context.Background(), "lab/dup"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Errorf("Read(dup) = %v, %v", obs.Availability, err)
	}
	e.srv.SetConfig(401, "name", "")
	v, _ := e.srv.VM(401)
	v.Name = ""
	e.srv.AddVM(v)
	if _, err := e.p.List(context.Background(), core.NewScope(nil)); err == nil || !strings.Contains(err.Error(), "no usable name") {
		t.Errorf("List with an unnamed VM = %v", err)
	}
}

func TestValidateIntent(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, 0)
	ok := map[string]string{proxmox.FieldVMID: "200", proxmox.FieldTemplate: "debian-tmpl", proxmox.FieldMemory: "2G",
		proxmox.FieldPower: "running", proxmox.FieldApplyPending: "reboot"}
	if err := e.p.ValidateIntent("lab/web1", ok); err != nil {
		t.Fatalf("ValidateIntent = %v", err)
	}
	for name, tc := range map[string]struct {
		id     string
		fields map[string]string
	}{
		"unknown cluster":    {"dc9/web1", nil},
		"bad name":           {"lab/web_1", nil},
		"unknown field":      {"lab/web1", map[string]string{"ram": "1"}},
		"computed":           {"lab/web1", map[string]string{proxmox.FieldPending: ""}},
		"name mismatch":      {"lab/web1", map[string]string{proxmox.FieldName: "web2"}},
		"vmid":               {"lab/web1", map[string]string{proxmox.FieldVMID: "99"}},
		"cores":              {"lab/web1", map[string]string{proxmox.FieldCores: "0"}},
		"memory":             {"lab/web1", map[string]string{proxmox.FieldMemory: "lots"}},
		"disks":              {"lab/web1", map[string]string{proxmox.FieldDisks: "scsi0 storage=x"}},
		"nics":               {"lab/web1", map[string]string{proxmox.FieldNICs: "net0 vlan=1"}},
		"ip":                 {"lab/web1", map[string]string{proxmox.FieldCIIP: "10.0.0.1"}},
		"ssh key":            {"lab/web1", map[string]string{proxmox.FieldCISSHKeys: "ssh-ed25519 not-a-key"}},
		"power restart":      {"lab/web1", map[string]string{proxmox.FieldPower: "restart"}},
		"force without mode": {"lab/web1", map[string]string{proxmox.FieldForceAfter: "1m"}},
		"mode without force": {"lab/web1", map[string]string{proxmox.FieldPowerOff: proxmox.PowerOffGracefulThenForce}},
		"apply_pending":      {"lab/web1", map[string]string{proxmox.FieldApplyPending: "always"}},
		"ciupgrade":          {"lab/web1", map[string]string{proxmox.FieldCIUpgrade: "no"}},
	} {
		if err := e.p.ValidateIntent(tc.id, tc.fields); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	noNode := proxmox.New(map[string]proxmox.Cluster{"lab": {Client: clusterOf(t, e).Client}}, proxmox.Options{})
	if err := noNode.ValidateIntent("lab/web1", map[string]string{}); err == nil || !strings.Contains(err.Error(), "no default node") {
		t.Errorf("no node: %v", err)
	}
}

func TestCreateNeedsVMID(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	want := web1()
	delete(want, proxmox.FieldVMID)
	pl, _, err := e.plan(t, "lab/web1", want)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.p.Apply(context.Background(), pl); err == nil || !strings.Contains(err.Error(), "from_pool") {
		t.Errorf("Apply without vmid = %v", err)
	}
	want[proxmox.FieldVMID] = "9000"
	pl, _, _ = e.plan(t, "lab/web1", want)
	if err := e.p.Apply(context.Background(), pl); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("Apply with a used vmid = %v", err)
	}
}

func TestActions(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, time.Second)
	e.converge(t, "lab/web1", web1(), 3)
	if ap, err := e.p.PlanAction("lab/web1", proxmox.ActionReset, nil); err != nil || !ap.Destructive {
		t.Errorf("PlanAction(reset) = %+v, %v", ap, err)
	}
	if ap, err := e.p.PlanAction("lab/web1", proxmox.ActionReboot, nil); err != nil || ap.Destructive {
		t.Errorf("PlanAction(reboot) = %+v, %v", ap, err)
	}
	if _, err := e.p.PlanAction("lab/sakutei-ctl", proxmox.ActionReboot, nil); err == nil {
		t.Error("PlanAction on the protected VM succeeded")
	}
	if err := e.p.Action(context.Background(), "lab/web1", proxmox.ActionReboot, nil); err != nil || e.srv.Calls("reboot") != 1 {
		t.Errorf("Action(reboot) = %v, calls %d", err, e.srv.Calls("reboot"))
	}
	e.srv.SetStatus(200, "stopped")
	if err := e.p.Action(context.Background(), "lab/web1", proxmox.ActionReset, nil); err != nil || e.srv.Calls("start") != 2 {
		t.Errorf("Action(reset) on a stopped VM = %v, start calls %d", err, e.srv.Calls("start"))
	}
	e.srv.SetUnreachable(true)
	if err := e.p.Action(context.Background(), "lab/web1", proxmox.ActionReboot, nil); !errors.Is(err, provider.ErrActionNotSent) {
		t.Errorf("Action while unreachable = %v, want ErrActionNotSent", err)
	}
}

// /cluster/resources の名前・status は pvestatd の写しで遅れる（本物の
// Proxmox VE で見つけた）。作った直後の VM は名前が空・status が古いが、
// 「無い」と見てもう一度作らず、走っている VM に start を重ねない。
func TestStaleResources(t *testing.T) {
	e := newEnv(t, pvetest.Options{ResourceLag: 5}, time.Second)
	want := web1()
	want[proxmox.FieldTemplate] = "debian-tmpl"
	e.converge(t, "lab/web1", want, 6)
	if c, s := e.srv.Calls("clone"), e.srv.Calls("start"); c != 1 || s != 1 {
		t.Errorf("clone %d, start %d; want 1 and 1", c, s)
	}
	for i := 0; i < 8; i++ {
		obs, err := e.p.Read(context.Background(), "lab/web1")
		if err != nil || obs.State == nil {
			t.Fatalf("Read %d after the create = %+v, %v; want the VM", i, obs, err)
		}
		if ops := e.cycle(t, "lab/web1", want); len(ops) != 0 {
			t.Fatalf("cycle %d: ops %+v", i, ops)
		}
	}
	// 手で止めても、写しが running のままの間に正しく start する。
	e.srv.SetStatus(200, "stopped")
	if ops := e.cycle(t, "lab/web1", want); len(ops) != 1 {
		t.Fatalf("ops = %+v, want a start", ops)
	}
	if vm, _ := e.srv.VM(200); vm.Status != "running" || e.srv.Calls("start") != 2 {
		t.Errorf("status %s, start calls %d", vm.Status, e.srv.Calls("start"))
	}
}
