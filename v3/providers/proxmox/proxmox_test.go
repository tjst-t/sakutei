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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/proxmox"
	"github.com/tjst-t/sakutei/v3/providers/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const (
	rwSecret = "rw-secret-5c1e"
	roSecret = "ro-secret-a83f"
	roToken  = "sakutei-ro@pve!audit"
	sshKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEQHShMqBxoVWaNhs0o1VR7qEL2eQZJqgpnZrDZvbS0o sakutei@ctl"
)

type env struct {
	srv *pvetest.Server
	p   *proxmox.Provider
}

// newEnv は偽の Proxmox（テンプレート 9000 と走っている VM 101 app1）と、それに
// 向けたクラスタ lab の provider を作る。観測は読むだけのトークン、書き込みは
// root のトークン。
func newEnv(t *testing.T, opts pvetest.Options) *env {
	t.Helper()
	opts.Secret = rwSecret
	opts.AuditTokenID, opts.AuditSecret = roToken, roSecret
	srv := pvetest.New(opts)
	t.Cleanup(srv.Close)
	srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true, Config: map[string]string{
		"cores": "1", "memory": "1024", "scsihw": "virtio-scsi-single",
		"scsi0": "local-lvm:base-9000-disk-0,size=4G", "ide2": "local-lvm:vm-9000-cloudinit,media=cdrom",
		"net0": "virtio=BC:24:11:00:90:00,bridge=vmbr0",
	}})
	srv.AddVM(pvetest.VM{VMID: 101, Node: "pve1", Name: "app1", Status: "running", Config: map[string]string{
		"cores": "2", "memory": "2048", "scsi0": "local-lvm:vm-101-disk-0,size=8G", "net0": "virtio=BC:24:11:00:01:01,bridge=vmbr0",
	}})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ro"), []byte(roSecret+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "rw"), []byte(rwSecret), 0o600)
	p, err := proxmox.Module(map[string]any{"settle": "0s", "poll": "1ms", "clusters": map[string]any{"lab": map[string]any{
		"endpoint": srv.URL, "node": "pve1", "timeout": "500ms", "task_wait": "2s",
		"observe": map[string]any{"token_id": roToken, "token_file": filepath.Join(dir, "ro")},
		"write":   map[string]any{"token_id": srv.TokenID(), "token_file": filepath.Join(dir, "rw")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, p: p.(*proxmox.Provider)}
}

func vmID(name string) core.ID { return core.ID{Type: "pve/vm", Name: name} }
func fwID(name string) core.ID { return core.ID{Type: "pve/fw-rules", Name: name} }
func kind(k, n string) core.ID { return core.ID{Type: "pve/" + k, Name: n} }
func wctx() core.WriteContext  { return core.NewWriteContext(nil) }
func rng() core.Range          { return core.Range{Provider: "pve", Args: map[string]string{"cluster": "lab"}} }
func (e *env) writes() int     { return e.srv.TokenCalls(e.srv.TokenID())["WRITE"] }
func (e *env) writeTokenUse() int {
	c := e.srv.TokenCalls(e.srv.TokenID())
	return c["GET"] + c["WRITE"]
}

// res は宣言を Schema の正規形にする（load と同じ）。
func (e *env) res(id core.ID, f core.Fields) core.Resource {
	ks := e.p.Schema().Kinds[id.Kind()]
	out := core.Fields{}
	for k, v := range f {
		n, err := core.Normalize(ks.Fields[k].Type, v)
		if err != nil {
			panic(err)
		}
		out[k] = n
	}
	return core.Resource{ID: id, Fields: out}
}

func (e *env) observe(t *testing.T, id core.ID) core.Observation {
	t.Helper()
	o, err := e.p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

// compare は宣言と今の観測を比べる（plan と同じ比べ方）。
func (e *env) compare(t *testing.T, r core.Resource) plan.Change {
	t.Helper()
	return plan.Compare(r, e.p.Schema().Kinds[r.ID.Kind()], e.observe(t, r.ID))
}

// apply は今の観測で Apply し、宣言どおりになったかを返す。
func (e *env) apply(t *testing.T, r core.Resource) plan.Change {
	t.Helper()
	if err := e.p.Apply(context.Background(), wctx(), r.ID, r.Fields, e.observe(t, r.ID)); err != nil {
		t.Fatalf("Apply %s: %v", r.ID, err)
	}
	return e.compare(t, r)
}

func (e *env) validate(t *testing.T, res ...core.Resource) []core.Diagnostic {
	t.Helper()
	return e.p.Validate(res, []core.Range{rng()})
}

func web1(e *env) core.Resource {
	return e.res(vmID("lab/web1"), core.Fields{
		"vmid": 200, "template": "debian-tmpl", "cores": 2, "memory": "2G",
		"disks": "scsi0 storage=local-lvm size=8G", "nics": "net0 bridge=vmbr0 vlan=20 firewall=1, net1 bridge=vmbr1",
		"firewall": "enable=1 policy_in=DROP", "ci_user": "ops", "ci_ssh_keys": sshKey, "ci_ip": "192.0.2.21/24",
		"ci_gateway": "192.0.2.1", "ci_dns": "192.0.2.53, 192.0.2.54", "ciupgrade": false, "power": "running",
	})
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		e := newEnv(t, pvetest.Options{})
		// keep の印のあるディスクを持つ VM（消すのを断る）。
		e.srv.AddVM(pvetest.VM{VMID: 102, Node: "pve1", Name: "keeper", Status: "stopped", Config: map[string]string{
			"scsi0": "local-lvm:vm-102-disk-0,size=4G", "scsi1": "local-lvm:vm-102-disk-1,size=1G", "tags": "sakutei-keep-scsi1",
			"unused0": "local-lvm:vm-102-disk-2",
		}})
		e.srv.AddVolume("local-lvm:vm-102-disk-2", 1<<30)
		e.srv.SetRules(101, []map[string]string{{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": "22", "enable": "1"}})
		return contract.Target{
			Provider: e.p,
			Ranges:   []core.Range{rng()},
			Absent:   []core.ID{vmID("lab/nope"), fwID("lab/app1"), kind("volume", "lab/app1/unused3"), kind("fw-rule", "lab/app1/5")},
			Create: []core.Resource{
				web1(e),
				e.res(fwID("lab/keeper"), core.Fields{"rules": []any{"in ACCEPT iface=net0 proto=tcp dport=443 comment=web"}}),
			},
			Converge: []core.Resource{e.res(vmID("lab/app1"), core.Fields{"cores": 4, "power": "stopped"})},
			Observed: []core.ID{vmID("lab/app1"), vmID("lab/keeper"), kind("volume", "lab/keeper/unused0"), kind("fw-rule", "lab/app1/0")},
			Faults: map[string]func(){
				"接続を切る": func() { e.srv.SetUnreachable(true) },
				"5xx":   func() { e.srv.SetFailStatus(http.StatusInternalServerError) },
				"時間切れ":  func() { e.srv.SetDelay(time.Second) },
				"認証の失敗": func() { e.srv.SetFailStatus(http.StatusUnauthorized) },
				"壊れた出力": func() { e.srv.SetBroken(true) },
			},
			Writes:               e.writes,
			WriteCredentialReads: e.writeTokenUse,
			Secrets:              []string{rwSecret, roSecret},
			Invokes: []contract.Invoke{
				{ID: vmID("lab/app1"), Op: proxmox.OpReboot, NotSent: func() { e.srv.SetUnreachable(true) },
					Failed: func() { e.srv.FailNextTask("qmreboot", "VM quit") }},
				{ID: vmID("lab/app1"), Op: proxmox.OpStop, NotSent: func() { e.srv.SetLock(101, "backup") }},
			},
			Refuses: []contract.Refuse{
				{ID: vmID("lab/keeper")},
				{ID: kind("volume", "lab/keeper/unused0")},
				{ID: kind("fw-rule", "lab/app1/0")},
			},
		}
	})
}

// TestCreateFromTemplate は採番の vmid でテンプレートから複製し、resize・NIC・
// VM の FW の options・cloud-init・起動まで 1 回の Apply で済み、書き方の違い
// （2G・DNS のカンマ）がずれにならない（E-D1・N-D1・E-I7）。
func TestCreateFromTemplate(t *testing.T) {
	e := newEnv(t, pvetest.Options{TaskPolls: 2})
	r := web1(e)
	if d := e.validate(t, r); len(d) > 0 {
		t.Fatal(d)
	}
	if c := e.compare(t, r); c.Op != plan.Create {
		t.Fatalf("%+v", c)
	}
	if c := e.apply(t, r); c.Op != plan.None {
		t.Fatalf("1 回の Apply で宣言どおりにならない: %+v", c)
	}
	vm, _ := e.srv.VMByName("web1")
	if vm.VMID != 200 || vm.Status != "running" || vm.Config["memory"] != "2048" || vm.Config["ciupgrade"] != "0" ||
		!strings.Contains(vm.Config["net0"], "tag=20") || !strings.Contains(vm.Config["scsi0"], "size=8G") || vm.Firewall["enable"] != "1" {
		t.Fatalf("%+v", vm)
	}
	n := e.writes()
	if err := e.p.Apply(context.Background(), wctx(), r.ID, r.Fields, e.observe(t, r.ID)); err != nil || e.writes() != n {
		t.Fatalf("宣言どおりなのに書いた（%d 回）: %v", e.writes()-n, err)
	}
	// 宣言しないキー（MAC・型）は比べない。手でメモリを変えると戻る。
	e.srv.SetConfig(200, "memory", "4096")
	if c := e.compare(t, r); c.Op != plan.Update || len(c.Fields) != 1 || c.Fields[0].Name != "memory" {
		t.Fatalf("%+v", c)
	}
	if c := e.apply(t, r); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
}

// TestPendingIsNotDoubled は走っている VM の再起動を待つ変更を重ねず、apply_pending:
// reboot で 1 回だけ再起動する（E-D1）。
func TestPendingIsNotDoubled(t *testing.T) {
	e := newEnv(t, pvetest.Options{})
	r := e.res(vmID("lab/app1"), core.Fields{"cores": 3})
	if c := e.apply(t, r); c.Op != plan.None {
		t.Fatalf("pending に宣言の値があればずれにしない: %+v", c)
	}
	o := e.observe(t, r.ID)
	if p, _ := o.Fields["pending"].([]any); len(p) != 1 || p[0] != "cores" || o.Fields["apply_pending"] != "never" {
		t.Fatalf("%+v", o.Fields)
	}
	n := e.writes()
	if err := e.p.Apply(context.Background(), wctx(), r.ID, r.Fields, o); err != nil || e.writes() != n {
		t.Fatalf("pending を重ねた: %v", err)
	}
	r = e.res(vmID("lab/app1"), core.Fields{"cores": 3, "apply_pending": "reboot"})
	if c := e.compare(t, r); c.Op != plan.Update || c.Fields[0].Name != "apply_pending" {
		t.Fatalf("%+v", c)
	}
	if c := e.apply(t, r); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	if vm, _ := e.srv.VM(101); vm.Config["cores"] != "3" || e.srv.Calls("reboot") > 1 {
		t.Fatalf("%+v", vm.Config)
	}
}

// TestRunningTaskIsConverging は Apply が待ち切れなかった仕事を、覚えておかずに
// 観測で見つけて Converging にする（E-D1）。
func TestRunningTaskIsConverging(t *testing.T) {
	e := newEnv(t, pvetest.Options{TaskPolls: 1 << 30})
	r := e.res(vmID("lab/app1"), core.Fields{"power": "stopped"})
	if err := e.p.Apply(context.Background(), wctx(), r.ID, r.Fields, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if o := e.observe(t, r.ID); o.Availability != core.Converging || !strings.Contains(o.Note, "qmshutdown") {
		t.Fatalf("%+v", o)
	}
	e.srv.FinishTasks()
	if c := e.compare(t, r); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	// ノードが落ちると、そのノードの VM は Unknown（無いと読まない、E-K1）。
	e.srv.SetNodeOffline("pve1", true)
	if o := e.observe(t, r.ID); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
	if _, err := e.p.List(context.Background(), rng()); err == nil {
		t.Fatal("ノードが落ちているのに List が完全")
	}
}

// TestRecreateKeepsDisk は keep のディスクを持つ VM の削除を断り、新しい VM の
// volume= が止まった前の VM から付け替え（承認の要る壊す変更）、付いたあとは
// volume の名前を比べない（E-C2・N-C12・N-D10）。
func TestRecreateKeepsDisk(t *testing.T) {
	e := newEnv(t, pvetest.Options{})
	old := e.res(vmID("lab/app1"), core.Fields{"disks": "scsi0 storage=local-lvm size=8G, scsi1 storage=local-lvm size=1G keep"})
	if c := e.apply(t, old); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	if vm, _ := e.srv.VM(101); vm.Config["tags"] != "sakutei-keep-scsi1" {
		t.Fatalf("keep の印が無い: %+v", vm.Config)
	}
	var re *core.RefuseError
	if err := e.p.Delete(context.Background(), wctx(), old.ID, core.Observation{}); !errors.As(err, &re) || !strings.Contains(re.Reason, "vm-101-disk-1") {
		t.Fatalf("keep のディスクが残る VM を消した: %v", err)
	}
	if _, ok := e.srv.VM(101); !ok {
		t.Fatal("消えた")
	}

	nw := e.res(vmID("lab/app2"), core.Fields{"vmid": 121, "template": "debian-tmpl",
		"disks": "scsi0 storage=local-lvm size=4G, scsi1 volume=local-lvm:vm-101-disk-1 keep"})
	hints := e.p.Classify(nw.ID, e.observe(t, nw.ID), nw.Fields)
	if len(hints) != 1 || !hints[0].NeedsApproval || !hints[0].Destructive {
		t.Fatalf("付け替えが承認の要る壊す変更でない: %+v", hints)
	}
	// 前の VM が走っている間は付け替えない（何も変えない）。
	if err := e.p.Apply(context.Background(), wctx(), nw.ID, nw.Fields, core.Observation{}); err == nil || !strings.Contains(err.Error(), "止まった VM") {
		t.Fatalf("%v", err)
	}
	e.apply(t, e.res(vmID("lab/app1"), core.Fields{"power": "stopped"}))
	if c := e.apply(t, nw); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	vm, _ := e.srv.VM(121)
	if !strings.HasPrefix(vm.Config["scsi1"], "local-lvm:vm-121-disk-") || vm.Config["tags"] != "sakutei-keep-scsi1" {
		t.Fatalf("%+v", vm.Config)
	}
	if h := e.p.Classify(nw.ID, e.observe(t, nw.ID), nw.Fields); len(h) != 0 {
		t.Fatalf("付いたあとも承認を求める: %+v", h)
	}
	// 前の VM はもう keep のディスクを持たないので消せる。
	if err := e.p.Delete(context.Background(), wctx(), old.ID, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.srv.VM(101); ok {
		t.Fatal("消えない")
	}
	if _, ok := e.srv.Volume(vm.Config["scsi1"][:strings.Index(vm.Config["scsi1"], ",")]); !ok {
		t.Fatal("付け替えたディスクが消えた")
	}
}

// TestFWRules は印のある規則だけを宣言の順に先頭に保ち、手の規則は消さずに
// fw-rule として List に出し、規則の変更は壊す変更と言う（E-B6）。
func TestFWRules(t *testing.T) {
	e := newEnv(t, pvetest.Options{})
	e.srv.SetRules(101, []map[string]string{{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": "22", "enable": "1"}})
	r := e.res(fwID("lab/app1"), core.Fields{"rules": []any{
		"out ACCEPT iface=net0 dest=10.0.0.10 proto=udp dport=53 comment=DNS",
		"IN accept  proto=tcp iface=net1 dport=443  comment=web",
	}})
	if h := e.p.Classify(r.ID, core.Observation{}, r.Fields); len(h) != 1 || !h[0].Destructive {
		t.Fatalf("%+v", h)
	}
	if c := e.apply(t, r); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	vm, _ := e.srv.VM(101)
	if len(vm.Rules) != 3 || vm.Rules[2]["dport"] != "22" || !strings.HasPrefix(vm.Rules[0]["comment"], "sakutei") {
		t.Fatalf("%+v", vm.Rules)
	}
	l, err := e.p.List(context.Background(), rng())
	if err != nil || !l.Complete {
		t.Fatal(err)
	}
	want := map[core.ID]bool{vmID("lab/app1"): true, fwID("lab/app1"): true, kind("fw-rule", "lab/app1/2"): true}
	for _, id := range l.IDs {
		delete(want, id)
	}
	if len(want) > 0 {
		t.Fatalf("List に無い: %v（%v）", want, l.IDs)
	}
	// 手の規則が先頭に入るとずれになり、戻すときも手の規則は消さない。
	rules := append([]map[string]string{{"type": "in", "action": "DROP", "enable": "1"}}, vm.Rules...)
	e.srv.SetRules(101, rules)
	if c := e.compare(t, r); c.Op != plan.Update {
		t.Fatalf("%+v", c)
	}
	if c := e.apply(t, r); c.Op != plan.None {
		t.Fatalf("%+v", c)
	}
	if vm, _ := e.srv.VM(101); len(vm.Rules) != 4 || vm.Rules[2]["action"] != "DROP" {
		t.Fatalf("%+v", vm.Rules)
	}
	if err := e.p.Delete(context.Background(), wctx(), r.ID, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if vm, _ := e.srv.VM(101); len(vm.Rules) != 2 {
		t.Fatalf("印の規則だけを外さない: %+v", vm.Rules)
	}
	if o := e.observe(t, r.ID); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
}

// TestValidate は現実に触れずに決まる誤りを読み込みで断る（E-I7・E-I6）。
func TestValidate(t *testing.T) {
	e := newEnv(t, pvetest.Options{})
	bad := map[string]core.Resource{
		"知らないクラスタ":   e.res(vmID("other/x"), core.Fields{}),
		"VM の名前":     e.res(vmID("lab/bad_name"), core.Fields{}),
		"観測だけの項目":    e.res(vmID("lab/x"), core.Fields{"lock": "x"}),
		"小さい vmid":   e.res(vmID("lab/x"), core.Fields{"vmid": 5}),
		"ディスクの書き方":   e.res(vmID("lab/x"), core.Fields{"disks": "scsi0 size=3.5G"}),
		"ssh の鍵":     e.res(vmID("lab/x"), core.Fields{"ci_ssh_keys": "ssh-ed25519 notbase64!"}),
		"再起動は宣言できない": e.res(vmID("lab/x"), core.Fields{"power": "reboot"}),
		"空の規則":       e.res(fwID("lab/x"), core.Fields{"rules": []any{}}),
		"規則の書き方":     e.res(fwID("lab/x"), core.Fields{"rules": []any{"sideways ACCEPT"}}),
		"volume の宣言": e.res(kind("volume", "lab/x/unused0"), core.Fields{}),
	}
	for name, r := range bad {
		if d := e.validate(t, r); len(d) == 0 {
			t.Errorf("%s: 断らない", name)
		}
	}
	if d := e.p.Validate([]core.Resource{web1(e)}, []core.Range{{Provider: "pve", Args: map[string]string{"cluster": "other"}}}); len(d) == 0 {
		t.Error("知らないクラスタの範囲を断らない")
	}
	if d := e.validate(t, web1(e), e.res(vmID("lab/gone"), nil)); len(d) != 0 {
		t.Errorf("%v", d)
	}
}

// TestModuleSettings は資格の値そのものを設定に書かせない（E-G2）。
func TestModuleSettings(t *testing.T) {
	for name, s := range map[string]map[string]any{
		"clusters が無い": {},
		"secret を書く": {"clusters": map[string]any{"lab": map[string]any{"endpoint": "https://pve:8006",
			"observe": map[string]any{"token_id": "a@pve!b", "token_secret": "x"}}}},
		"observe が無い": {"clusters": map[string]any{"lab": map[string]any{"endpoint": "https://pve:8006"}}},
		"前置きの ..": {"clusters": map[string]any{"lab": map[string]any{"endpoint": "https://pve:8006", "api_base_path": "/a/../b",
			"observe": map[string]any{"token_id": "a@pve!b", "token_file": "/x"}}}},
	} {
		if _, err := proxmox.Module(s); err == nil {
			t.Errorf("%s: 断らない", name)
		}
	}
}

// TestAPIBasePath は API のパスの前置きを変えた模擬に同じ宣言を当てる（E-D1）。
func TestAPIBasePath(t *testing.T) {
	srv := pvetest.New(pvetest.Options{BasePath: "/mock/pve/api2/json"})
	defer srv.Close()
	srv.AddVM(pvetest.VM{VMID: 101, Node: "pve1", Name: "app1", Status: "stopped", Config: map[string]string{"cores": "1"}})
	f := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(f, []byte(srv.Secret()), 0o600)
	p, err := proxmox.Module(map[string]any{"clusters": map[string]any{"lab": map[string]any{
		"endpoint": srv.URL, "api_base_path": "/mock/pve/api2/json/", "node": "pve1",
		"observe": map[string]any{"token_id": srv.TokenID(), "token_file": f},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := p.Observe(context.Background(), []core.ID{vmID("lab/app1")})
	if o[vmID("lab/app1")].Availability != core.Present {
		t.Fatalf("%+v", o)
	}
}

// TestStaleResources は /cluster/resources の名前が pvestatd の写しで遅れる（作った
// 直後は空か "VM <vmid>"）ときも、作った VM を「無い」と見て作り直さない（E-D1）。
func TestStaleResources(t *testing.T) {
	for _, vmid := range []int64{200, 201} {
		e := newEnv(t, pvetest.Options{ResourceLag: 50})
		r := web1(e)
		r.Fields["vmid"] = vmid
		if c := e.apply(t, r); c.Op != plan.None {
			t.Fatalf("vmid %d: %+v", vmid, c)
		}
		if e.srv.Calls("clone") != 1 {
			t.Fatalf("vmid %d: 複製 %d 回", vmid, e.srv.Calls("clone"))
		}
	}
}
