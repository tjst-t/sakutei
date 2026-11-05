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

package main

// pve_test.go — proxmox を CLI から（偽の Proxmox に向けて）回す。作り直す VM が
// 前の VM の keep のディスクを volume= で引き取る変更は、承認が揃うまで Apply しない。
// 承認のあとに作って付け替え、network_policy の realizer proxmox-fw の規則を置く。
// 手で足した規則は消さずに宣言に無いもの（unmanaged）として出す（E-C2・E-B6・E-D1）。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/providers/proxmox/pvetest"
)

func TestProxmox(t *testing.T) {
	srv := pvetest.New(pvetest.Options{})
	defer srv.Close()
	srv.AddVM(pvetest.VM{VMID: 100, Node: "pve1", Name: "sakutei-ctl", Status: "running", Config: map[string]string{"cores": "1"}})
	srv.AddVM(pvetest.VM{VMID: 9000, Node: "pve1", Name: "debian-tmpl", Template: true, Config: map[string]string{
		"scsihw": "virtio-scsi-single", "scsi0": "local-lvm:base-9000-disk-0,size=4G", "net0": "virtio=BC:24:11:00:90:00,bridge=vmbr0",
	}})
	srv.AddVM(pvetest.VM{VMID: 120, Node: "pve1", Name: "app1", Status: "stopped", Config: map[string]string{
		"scsi0": "local-lvm:vm-120-disk-0,size=4G", "scsi1": "local-lvm:vm-120-disk-1,size=1G", "tags": "sakutei-keep-scsi1",
	}})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "token"), []byte(srv.Secret()+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(fmt.Sprintf(`providers:
  pve:
    module: proxmox
    settings:
      settle: 0s
      poll: 1ms
      clusters:
        lab:
          endpoint: %s
          node: pve1
          observe: {token_id: "%s", token_file: %s}
`, srv.URL, srv.TokenID(), filepath.Join(dir, "token"))), 0o644)
	os.MkdirAll(filepath.Join(dir, "intent"), 0o755)
	os.WriteFile(filepath.Join(dir, "intent", "pve.yaml"), []byte(`expand:
  - kind: go:netpolicy
ranges:
  - provider: pve
    args: {cluster: lab}
    external_owners:
      - {owner: stage0, id: vm/lab/sakutei-ctl, reason: "Sakutei 自身が動く VM"}
resources:
  - type: pve/vm
    id: lab/app1
    fields: {power: stopped}
  - type: pve/vm
    id: lab/app2
    fields:
      vmid: 121
      template: debian-tmpl
      disks: "scsi0 storage=local-lvm size=8G, scsi1 volume=local-lvm:vm-120-disk-1 keep"
      firewall: "enable=1"
      power: running
network_policy:
  networks:
    - {name: app, cidrs: ["10.0.0.21"]}
    - {name: users, cidrs: ["10.1.0.0/24"]}
  allows:
    - {from: users, to: app, proto: tcp, ports: [443], reason: "利用者から web"}
  targets:
    - {name: app2, realizer: proxmox-fw, kind: host, networks: [app], provider: pve, id: lab/app2}
`), 0o644)
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), args, &out, &errOut, Options()...)
		return code, out.String() + errOut.String()
	}
	common := []string{"--intent", filepath.Join(dir, "intent"), "--config", filepath.Join(dir, "config.yaml")}
	record := filepath.Join(dir, "record")
	reconcile := func() (int, string) { return run(append([]string{"reconcile", "--record", record}, common...)...) }

	// 付け替えは承認が揃うまで Apply しない。規則は VM が無いので失敗しても VM を壊さない。
	code, out := reconcile()
	if code == cli.ExitOK || !strings.Contains(out, "sakutei approve chg-") || !strings.Contains(out, "既にある volume") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if _, ok := srv.VMByName("app2"); ok {
		t.Fatalf("承認の前に作った\n%s", out)
	}
	req := regexp.MustCompile(`sakutei approve (chg-[0-9a-f]+)`).FindStringSubmatch(out)[1]
	if code, out := run("approve", req, "--record", record, "--as", "alice"); code != cli.ExitOK {
		t.Fatalf("approve: exit=%d\n%s", code, out)
	}
	code, out = reconcile()
	vm, ok := srv.VMByName("app2")
	if !ok || !strings.HasPrefix(vm.Config["scsi1"], "local-lvm:vm-121-disk-") || vm.Status != "running" {
		t.Fatalf("exit=%d %+v\n%s", code, vm, out)
	}
	// 規則は VM ができた cycle か次の cycle に置く。手で足した規則は消さず unmanaged として出す。
	for i := 0; i < 3 && code != cli.ExitOK; i++ {
		code, out = reconcile()
	}
	if code != cli.ExitOK {
		t.Fatalf("収束しない: exit=%d\n%s", code, out)
	}
	vm, _ = srv.VMByName("app2")
	if len(vm.Rules) != 1 || !strings.Contains(vm.Rules[0]["comment"], "利用者から web") {
		t.Fatalf("%+v", vm.Rules)
	}
	srv.SetRules(121, append(vm.Rules, map[string]string{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": "22", "enable": "1"}))
	code, out = run(append([]string{"plan"}, common...)...)
	if code != cli.ExitOK || !strings.Contains(out, "pve/fw-rule/lab/app2/1") || strings.Contains(out, "sakutei-ctl") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if code, out = reconcile(); code != cli.ExitOK && code != cli.ExitRemaining {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if vm, _ := srv.VMByName("app2"); len(vm.Rules) != 2 {
		t.Fatalf("手の規則を消した: %+v", vm.Rules)
	}
	if _, ok := srv.VMByName("app1"); !ok {
		t.Fatal("宣言の VM が消えた")
	}
}
