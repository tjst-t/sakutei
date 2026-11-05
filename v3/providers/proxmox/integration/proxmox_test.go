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

//go:build integration_v3_proxmox

package integration

// proxmox_test.go — v3 の sakutei の CLI（plan・reconcile・approve）を入れ子の
// Proxmox VE 9.2 に向けて回す（doc.go）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// planDoc は plan --json の要るところ。
type planDoc struct {
	Changes []struct {
		Resource string `json:"resource"`
		Op       string `json:"op"`
		Action   string `json:"action"`
		Blocked  string `json:"blocked"`
		Fields   []struct {
			Name string `json:"name"`
		} `json:"fields"`
	} `json:"changes"`
}

// op は resource の op（無ければ ""）。
func (d planDoc) op(resource string) string {
	for _, c := range d.Changes {
		if c.Resource == resource {
			return c.Op
		}
	}
	return ""
}

func (d planDoc) fields(resource string) []string {
	var out []string
	for _, c := range d.Changes {
		if c.Resource == resource {
			for _, f := range c.Fields {
				out = append(out, f.Name)
			}
		}
	}
	return out
}

// head は intent の共通の先頭部分（vmid の採番・クラスタの Range・触らない VM）。
const head = `pools:
  - name: vmids
    range: "500-509"
ranges:
  - provider: pve
    args: {cluster: it}
    external_owners:
      - {owner: stage0, id: vm/it/sakutei-ctl, reason: "Sakutei 自身の VM の代わり"}
`

// setIntent は intent/intent.yaml を書き換える（採番のファイル allocations/ は残す）。
func (l *lab) setIntent(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(l.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "intent.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cli は sakutei を回し、出力と終了コードを返す。cfg が空なら l.cfg。
func (l *lab) cli(t *testing.T, cfg string, args ...string) (string, int) {
	t.Helper()
	if cfg == "" {
		cfg = l.cfg
	}
	full := append([]string{}, args...)
	if args[0] == "plan" || args[0] == "reconcile" {
		full = append(full, "--intent", filepath.Join(l.dir, "intent"), "--config", cfg)
	}
	full = append(full, "--record", filepath.Join(l.dir, "record"))
	out, err := exec.Command(l.bin, full...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func (l *lab) plan(t *testing.T, cfg string) planDoc {
	t.Helper()
	out, code := l.cli(t, cfg, "plan", "--json")
	var d planDoc
	if i := strings.Index(out, "{"); code != 0 || i < 0 || json.Unmarshal([]byte(out[i:]), &d) != nil {
		t.Fatalf("plan exit %d:\n%s", code, out)
	}
	return d
}

// converge は reconcile を終了コード 0（収束した）まで回す。
func (l *lab) converge(t *testing.T, max int) int {
	t.Helper()
	var out string
	for i := 1; i <= max; i++ {
		var code int
		out, code = l.cli(t, "", "reconcile")
		if code == 0 {
			return i
		}
		t.Logf("reconcile %d exit %d:\n%s", i, code, out)
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("not converged after %d reconciles:\n%s", max, out)
	return 0
}

// approve は出力の承認の依頼を alice で承認する。
func (l *lab) approve(t *testing.T, out string) {
	t.Helper()
	m := regexp.MustCompile(`sakutei approve (chg-[0-9a-f]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("承認の依頼が無い:\n%s", out)
	}
	if o, code := l.cli(t, "", "approve", m[1], "--as", "alice"); code != 0 {
		t.Fatalf("approve exit %d:\n%s", code, o)
	}
}

func TestProxmox(t *testing.T) {
	l := startLab(t)
	ctx := context.Background()

	// 触らない VM（Sakutei 自身の VM の代わり。ディスクからしか起動しない）と、
	// クラウドイメージのテンプレートを ssh で作る（import-from は root@pam だけ）。
	img := ensureCloudImage(t, l.cache)
	run(t, exec.Command("scp", "-F", "/dev/null", "-i", l.key, "-P", strconv.Itoa(l.sshPort), "-o", "BatchMode=yes", "-o", "LogLevel=ERROR",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", img, "root@127.0.0.1:/root/cloud.qcow2"))
	if out, err := l.ssh(`set -e
qm create 100 --name sakutei-ctl --memory 256 --scsi0 local-lvm:1 --net0 virtio,bridge=vmbr0 --boot order=scsi0
qm start 100
qm create 9000 --name it-tmpl --memory 1024 --cores 1 --scsihw virtio-scsi-single \
  --scsi0 local-lvm:0,import-from=/root/cloud.qcow2 --ide2 local-lvm:cloudinit --net0 virtio,bridge=vmbr0 \
  --serial0 socket --vga serial0 --boot order=scsi0
qm template 9000
rm /root/cloud.qcow2
pvesh create /pools --poolid it-pool >/dev/null
qm list`); err != nil {
		t.Fatalf("setting up the VMs: %v\n%s", err, out)
	}
	ctlBefore, _ := l.config(t, 100)

	pub, err := os.ReadFile(l.key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	vm1 := func(cores, extra string) string {
		return `  - type: pve/vm
    id: it/it-vm1
    fields:
      vmid: {from_pool: vmids}
      template: it-tmpl
      cores: ` + cores + `
      memory: 1G
      disks: "scsi0 storage=local-lvm size=4G"
      nics: "net0 bridge=vmbr0, net1 bridge=vmbr0 vlan=42"
      ci_user: ops
      ci_ssh_keys: "` + strings.TrimSpace(string(pub)) + `"
      ci_ip: ` + vmAddr + `/24
      ci_gateway: 10.0.2.2
      ci_dns: 10.0.2.3
      power: running
` + extra
	}
	base := head + "resources:\n" + vm1("2", "")

	t.Run("テンプレートから作る", func(t *testing.T) {
		l.setIntent(t, base)
		if d := l.plan(t, ""); d.op("pve/vm/it/it-vm1") != "create" {
			t.Fatalf("plan: %+v", d)
		}
		t.Logf("converged after %d reconcile(s)", l.converge(t, 6))
		cur, pending := l.config(t, 500)
		for k, want := range map[string]string{"memory": "1024", "cores": "2", "ciuser": "ops", "ipconfig0": "ip=" + vmAddr + "/24,gw=10.0.2.2", "nameserver": "10.0.2.3"} {
			if cur[k] != want {
				t.Errorf("config %s = %q, want %q", k, cur[k], want)
			}
		}
		if !strings.Contains(cur["scsi0"], "size=4G") || !strings.Contains(cur["net1"], "tag=42") || strings.Contains(cur["net0"], "tag=") {
			t.Errorf("config = %v", cur)
		}
		if len(pending) != 0 || l.status(t, 500) != "running" {
			t.Errorf("pending %v, status %s", pending, l.status(t, 500))
		}
		out := l.vmSSH(t, "hostname; ip -4 -o addr show scope global; nproc; lsblk -bdno SIZE /dev/sda", 5*time.Minute)
		if !strings.Contains(out, "it-vm1") || !strings.Contains(out, vmAddr+"/24") || !strings.Contains(out, "\n2\n") || !strings.Contains(out, "4294967296") {
			t.Errorf("the created VM is not as declared:\n%s", out)
		}
	})

	t.Run("何 cycle 回しても適合のまま", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if out, code := l.cli(t, "", "reconcile"); code != 0 {
				t.Fatalf("reconcile %d exit %d:\n%s", i, code, out)
			}
		}
		d := l.plan(t, "")
		for _, c := range d.Changes {
			if c.Op != "none" || strings.Contains(c.Resource, "sakutei-ctl") || strings.Contains(c.Resource, "it-tmpl") {
				t.Errorf("plan: %+v", c)
			}
		}
	})

	t.Run("手でメモリを変えると戻る", func(t *testing.T) {
		if err := l.api.SetConfig(ctx, pveNode, 500, url.Values{"memory": {"2048"}}); err != nil {
			t.Fatal(err)
		}
		if d := l.plan(t, ""); d.op("pve/vm/it/it-vm1") != "update" || strings.Join(d.fields("pve/vm/it/it-vm1"), ",") != "memory" {
			t.Fatalf("plan: %+v", d)
		}
		l.converge(t, 3)
		if cur, pending := l.config(t, 500); cur["memory"] != "1024" || pending["memory"] != "" {
			t.Errorf("memory %s, pending %v", cur["memory"], pending)
		}
	})

	t.Run("pending を重ねず apply_pending で再起動する", func(t *testing.T) {
		l.setIntent(t, head+"resources:\n"+vm1("3", ""))
		l.converge(t, 3)
		cur, pending := l.config(t, 500)
		t.Logf("cores: current %s, pending %q", cur["cores"], pending["cores"])
		if pending["cores"] == "3" {
			for i := 0; i < 2; i++ {
				if out, code := l.cli(t, "", "reconcile"); code != 0 {
					t.Fatalf("pending のまま重ねて書いた: exit %d\n%s", code, out)
				}
			}
			l.setIntent(t, head+"resources:\n"+vm1("3", "      apply_pending: reboot\n"))
			l.converge(t, 4)
			if cur, pending := l.config(t, 500); cur["cores"] != "3" || len(pending) != 0 {
				t.Errorf("after the reboot: cores %s, pending %v", cur["cores"], pending)
			}
			if out := l.vmSSH(t, "nproc", 5*time.Minute); strings.TrimSpace(out) != "3" {
				t.Errorf("nproc = %q, want 3", out)
			}
		} else if cur["cores"] != "3" {
			t.Errorf("cores %s, pending %v", cur["cores"], pending)
		}
		base = head + "resources:\n" + vm1("3", "")
		l.setIntent(t, base)
	})

	t.Run("触らない VM に触れない", func(t *testing.T) {
		l.setIntent(t, head+"resources:\n  - {type: pve/vm, id: it/sakutei-ctl, fields: {memory: 4096}}\n")
		if out, code := l.cli(t, "", "plan"); code != 2 || !strings.Contains(out, "external owner") {
			t.Errorf("plan exit %d:\n%s", code, out)
		}
		l.setIntent(t, base)
		if after, _ := l.config(t, 100); after["digest"] != ctlBefore["digest"] || l.status(t, 100) != "running" {
			t.Errorf("the protected VM changed")
		}
	})

	t.Run("届かないと Unknown", func(t *testing.T) {
		// 止める間は短く（watchdog-mux は 60s を越えて止まると VM を再起動する）。
		short := l.writeConfig(t, "short.yaml", "2s", "root@pam!"+tokenName, filepath.Join(l.dir, "root.token"), "", "")
		if err := l.qemu.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		out, code := l.cli(t, short, "plan", "--json")
		_ = l.qemu.Process.Signal(syscall.SIGCONT)
		var d planDoc
		if i := strings.Index(out, "{"); i >= 0 {
			_ = json.Unmarshal([]byte(out[i:]), &d)
		}
		if code != 0 || d.op("pve/vm/it/it-vm1") != "unknown" {
			t.Errorf("plan while unreachable: exit %d\n%s", code, out)
		}
		for start := time.Now(); time.Since(start) < 2*time.Minute; time.Sleep(2 * time.Second) {
			if _, err := l.api.Resources(ctx); err == nil {
				break
			}
		}
		l.converge(t, 3)
	})

	t.Run("宣言に無い VM は見せるが消さない", func(t *testing.T) {
		l.setIntent(t, head)
		d := l.plan(t, "")
		if d.op("pve/vm/it/it-vm1") != "unmanaged" {
			t.Errorf("plan: %+v", d)
		}
		for _, c := range d.Changes {
			if strings.Contains(c.Resource, "sakutei-ctl") || strings.Contains(c.Resource, "it-tmpl") {
				t.Errorf("触らない VM・テンプレートが出た: %+v", c)
			}
		}
		if out, code := l.cli(t, "", "reconcile"); code != 0 && code != 3 {
			t.Errorf("reconcile exit %d:\n%s", code, out)
		}
		if s := l.status(t, 500); s != "running" {
			t.Errorf("宣言に無い VM: %s", s)
		}
		l.setIntent(t, base)
	})

	vm4 := `  - type: pve/vm
    id: it/it-vm4
    fields:
      vmid: 550
      template: it-tmpl
      disks: "scsi0 storage=local-lvm size=4G"
      nics: "net0 bridge=vmbr0 firewall=1 mtu=1450, net1 bridge=vmbr0 vlan=43 firewall=1"
      firewall: "enable=1 policy_in=DROP policy_out=REJECT macfilter=0"
      ciupgrade: false
      power: stopped
`
	t.Run("ciupgrade と NIC の firewall・mtu と VM の FW の options・観測用のトークン", func(t *testing.T) {
		l.setIntent(t, base+vm4)
		l.converge(t, 4)
		cur, _ := l.config(t, 550)
		if cur["ciupgrade"] != "0" || !strings.Contains(cur["net0"], "firewall=1") || !strings.Contains(cur["net0"], "mtu=1450") || !strings.Contains(cur["net1"], "firewall=1") {
			t.Errorf("config = %v", cur)
		}
		if fw, err := l.ssh("cat /etc/pve/firewall/550.fw"); err != nil || !strings.Contains(fw, "enable: 1") || !strings.Contains(fw, "policy_out: REJECT") {
			t.Errorf("550.fw: %v\n%s", err, fw)
		}
		// 観測用のトークン（/ に PVEAuditor）と、書き込み用の root のトークン。
		out, err := l.ssh(`set -e
pveum user add sakutei-ro@pve >/dev/null
pveum user token add sakutei-ro@pve audit --privsep 1 --output-format json
pveum acl modify / --roles PVEAuditor --users sakutei-ro@pve
pveum acl modify / --roles PVEAuditor --tokens 'sakutei-ro@pve!audit'`)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		m := regexp.MustCompile(`"value"\s*:\s*"([^"]+)"`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("token: %s", out)
		}
		os.WriteFile(filepath.Join(l.dir, "ro.token"), []byte(m[1]), 0o600)
		split := l.writeConfig(t, "split.yaml", "15s", "sakutei-ro@pve!audit", filepath.Join(l.dir, "ro.token"), "root@pam!"+tokenName, filepath.Join(l.dir, "root.token"))
		roOnly := l.writeConfig(t, "ro.yaml", "15s", "sakutei-ro@pve!audit", filepath.Join(l.dir, "ro.token"), "", "")
		if err := l.api.SetConfig(ctx, pveNode, 550, url.Values{"net0": {strings.Replace(strings.Replace(cur["net0"], "firewall=1", "firewall=0", 1), "mtu=1450", "mtu=1500", 1)}}); err != nil {
			t.Fatal(err)
		}
		if err := l.api.SetFirewallOptions(ctx, pveNode, 550, url.Values{"enable": {"0"}, "ipfilter": {"1"}}); err != nil {
			t.Fatal(err)
		}
		d := l.plan(t, roOnly)
		if f := strings.Join(d.fields("pve/vm/it/it-vm4"), ","); f != "firewall,nics" {
			t.Errorf("観測用のトークンの plan: %s %+v", f, d)
		}
		// 観測用のトークンだけでは書けない（書き込みは拒否され、VM は変わらない）。
		if out, code := l.cli(t, roOnly, "reconcile"); code == 0 {
			t.Errorf("読むだけのトークンで書けた:\n%s", out)
		}
		if out, code := l.cli(t, split, "reconcile"); code != 0 {
			t.Errorf("split reconcile exit %d:\n%s", code, out)
		}
		cur, _ = l.config(t, 550)
		fw, _ := l.api.FirewallOptions(ctx, pveNode, 550)
		if !strings.Contains(cur["net0"], "firewall=1") || !strings.Contains(cur["net0"], "mtu=1450") || fw["enable"] != "1" || fw["ipfilter"] != "1" {
			t.Errorf("戻らない（書かないキー ipfilter は残す）: %v %v", cur["net0"], fw)
		}
	})

	policy := `expand:
  - kind: go:netpolicy
network_policy:
  networks:
    - {name: a-infra, domain: tenant-a, cidrs: ["10.0.0.10", "10.0.0.11"]}
    - {name: gw-a, domain: tenant-a, cidrs: ["10.0.0.1"]}
    - {name: gw-a-svc, domain: service, cidrs: ["172.16.0.11", "2001:db8:16::11"]}
    - {name: portal, domain: service, cidrs: ["172.16.1.0/24", "2001:db8:17::/64"]}
  allows:
    - {from: gw-a, to: a-infra, proto: udp, ports: [53, 88, 123], reason: "関所から DNS・KDC・NTP"}
    - {from: gw-a, to: a-infra, proto: tcp, ports: [53, 88, 636], reason: "関所から DNS・KDC・LDAP"}
    - {from: portal, to: gw-a-svc, proto: tcp, ports: [443], reason: "サービス側は 443 だけ"}
  targets:
    - name: gw-a
      realizer: proxmox-fw
      kind: host
      networks: [gw-a, gw-a-svc]
      interfaces:
        - {name: net0, domain: tenant-a}
        - {name: net1, domain: service}
      provider: pve
      id: it/it-vm4
`
	t.Run("network_policy の実現先 proxmox-fw", func(t *testing.T) {
		if out, err := l.ssh("pvesh create /nodes/" + pveNode + "/qemu/550/firewall/rules --type in --action ACCEPT --proto tcp --dport 22 --enable 1"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		l.setIntent(t, policy+base+vm4)
		if d := l.plan(t, ""); d.op("pve/fw-rules/it/it-vm4") != "create" {
			t.Fatalf("plan: %+v", d)
		}
		l.converge(t, 3)
		fw, _ := l.ssh("cat /etc/pve/firewall/550.fw")
		t.Logf("550.fw:\n%s", fw)
		var lines []string
		for _, line := range strings.Split(fw[strings.Index(fw, "[RULES]")+len("[RULES]"):], "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) != 5 || !strings.HasPrefix(lines[0], "OUT ACCEPT -i net0") || !strings.Contains(lines[0], "# sakutei: 関所から DNS・KDC・NTP") ||
			!strings.HasPrefix(lines[2], "IN ACCEPT -i net1") || lines[4] != "IN ACCEPT -p tcp -dport 22" {
			t.Fatalf("規則が宣言の順に先頭に無い:\n%s", fw)
		}
		d := l.plan(t, "")
		hand := ""
		for _, c := range d.Changes {
			if strings.HasPrefix(c.Resource, "pve/fw-rule/it/it-vm4/") && c.Op == "unmanaged" {
				hand = c.Resource
			}
		}
		if hand == "" {
			t.Errorf("手で置いた規則が unmanaged に出ない: %+v", d)
		}
		// 置いた規則の port を手で変え、先頭に手で規則を置くと drift になり、戻すときも手で置いた規則は残す。
		if out, err := l.ssh("pvesh set /nodes/" + pveNode + "/qemu/550/firewall/rules/0 --dport 99 && pvesh create /nodes/" + pveNode + "/qemu/550/firewall/rules --type in --action DROP --enable 1"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if d := l.plan(t, filepath.Join(l.dir, "ro.yaml")); d.op("pve/fw-rules/it/it-vm4") != "update" {
			t.Errorf("観測用のトークンの plan: %+v", d)
		}
		l.converge(t, 3)
		fw, _ = l.ssh("cat /etc/pve/firewall/550.fw")
		if !strings.Contains(fw, "IN DROP") || !strings.Contains(fw, "-dport 22") || strings.Contains(fw, "-dport 99") {
			t.Errorf("戻らない・手で置いた規則を消した:\n%s", fw)
		}
	})

	old := func(extra string) string {
		return `  - type: pve/vm
    id: it/it-old
    fields:
      vmid: 560
      template: it-tmpl
      disks: "scsi0 storage=local-lvm size=4G, scsi1 storage=local-lvm size=1G keep, scsi2 storage=local-lvm size=2G"
      power: stopped
` + extra
	}
	t.Run("作り直しでデータのディスクを引き継ぐ（keep と volume=）", func(t *testing.T) {
		l.setIntent(t, base+old(""))
		l.converge(t, 4)
		oldCfg, _ := l.config(t, 560)
		if !strings.Contains(oldCfg["tags"], "sakutei-keep-scsi1") {
			t.Fatalf("keep の marker が無い: %v", oldCfg)
		}
		// keep のディスクの volume（複製の番号の振り方は Proxmox が決める）に確認用のデータを書く。
		keepVol := strings.SplitN(oldCfg["scsi1"], ",", 2)[0]
		keepLV := strings.TrimPrefix(keepVol, "local-lvm:")
		if out, err := l.ssh("lvchange -ay pve/" + keepLV + " >/dev/null 2>&1; printf sakutei-keep-data | dd of=/dev/pve/" + keepLV + " conv=notrunc status=none && sync"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		dropLV := strings.TrimPrefix(strings.SplitN(oldCfg["scsi2"], ",", 2)[0], "local-lvm:")
		// keep のディスクが残る VM の削除は拒否する。
		l.setIntent(t, base+"  - {type: pve/vm, id: it/it-old, ensure: absent}\n")
		if out, code := l.cli(t, "", "reconcile"); code == 0 || !strings.Contains(out, "keep の volume") || strings.HasPrefix(l.status(t, 560), "absent") {
			t.Fatalf("keep のディスクが残る VM を消した: exit %d\n%s", code, out)
		}
		// 新しい VM が volume= で引き取る。承認が揃うまで作らない。
		nw := `  - type: pve/vm
    id: it/it-new
    fields:
      vmid: 561
      template: it-tmpl
      disks: "scsi0 storage=local-lvm size=4G, scsi1 volume=` + keepVol + ` keep"
      power: stopped
`
		l.setIntent(t, base+old("")+nw)
		out, code := l.cli(t, "", "reconcile")
		if code == 0 || !strings.Contains(out, "既にある volume") || !strings.HasPrefix(l.status(t, 561), "absent") {
			t.Fatalf("承認の前に付け替えた: exit %d\n%s", code, out)
		}
		l.approve(t, out)
		l.converge(t, 4)
		cur, _ := l.config(t, 561)
		vol := strings.SplitN(cur["scsi1"], ",", 2)[0]
		if !strings.HasPrefix(vol, "local-lvm:vm-561-disk-") || !strings.Contains(cur["tags"], "sakutei-keep-scsi1") {
			t.Fatalf("付け替えない: %v", cur)
		}
		lv := "/dev/pve/" + strings.TrimPrefix(vol, "local-lvm:")
		if data, err := l.ssh("lvchange -ay " + lv + " >/dev/null 2>&1; head -c 17 " + lv); err != nil || data != "sakutei-keep-data" {
			t.Errorf("中身が残らない: %q %v", data, err)
		}
		// 前の VM はもう keep のディスクを持たないので消せる。keep でないディスクは一緒に消える。
		l.setIntent(t, base+nw+"  - {type: pve/vm, id: it/it-old, ensure: absent}\n")
		l.converge(t, 4)
		if !strings.HasPrefix(l.status(t, 560), "absent") {
			t.Errorf("前の VM が消えない")
		}
		if out, _ := l.ssh("lvs --noheadings -o lv_name pve"); strings.Contains(out, dropLV) || !strings.Contains(out, strings.TrimPrefix(vol, "local-lvm:")) {
			t.Errorf("lvs:\n%s", out)
		}
		base += nw
		l.setIntent(t, base+vm4)
	})

	t.Run("pool に入れて作り、外されると戻す", func(t *testing.T) {
		vm2 := `  - type: pve/vm
    id: it/it-vm2
    fields: {vmid: 520, template: it-tmpl, pool: it-pool, power: stopped}
`
		l.setIntent(t, base+vm4+vm2)
		l.converge(t, 4)
		if out, _ := l.ssh("pvesh get /pools/it-pool --output-format json"); !strings.Contains(out, `"vmid":520`) {
			t.Fatalf("pool に無い:\n%s", out)
		}
		if out, err := l.ssh("pvesh set /pools --poolid it-pool --vms 520 --delete 1"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if d := l.plan(t, ""); strings.Join(d.fields("pve/vm/it/it-vm2"), ",") != "pool" {
			t.Errorf("plan: %+v", d)
		}
		l.converge(t, 3)
		if out, _ := l.ssh("pvesh get /pools/it-pool --output-format json"); !strings.Contains(out, `"vmid":520`) {
			t.Errorf("pool に戻らない:\n%s", out)
		}
		bad := strings.Replace(vm2, "it-pool", "no-such-pool", 1)
		bad = strings.Replace(strings.Replace(bad, "520", "521", 1), "it-vm2", "it-vm3", 1)
		l.setIntent(t, base+vm4+vm2+bad)
		if out, code := l.cli(t, "", "reconcile"); code == 0 || !strings.Contains(out, "does not exist") {
			t.Errorf("無い pool: exit %d\n%s", code, out)
		}
		l.setIntent(t, base+vm4+vm2)
	})

	t.Run("承認つきで消す", func(t *testing.T) {
		retire := head + `policies:
  - name: retire
    select: {labels: {retire: "yes"}}
    approval: {quorum: {n: 1, of: [alice]}}
resources:
  - {type: pve/vm, id: it/it-vm1, ensure: absent, labels: {retire: "yes"}}
`
		l.setIntent(t, retire)
		out, code := l.cli(t, "", "reconcile")
		if code == 0 || l.status(t, 500) != "running" {
			t.Fatalf("承認の前に消した: exit %d\n%s", code, out)
		}
		l.approve(t, out)
		l.converge(t, 4)
		if s := l.status(t, 500); !strings.HasPrefix(s, "absent") {
			t.Fatalf("消えない: %s", s)
		}
		if l.status(t, 100) != "running" || strings.HasPrefix(l.status(t, 9000), "absent") || strings.HasPrefix(l.status(t, 550), "absent") {
			t.Error("ほかの VM に触れた")
		}
		fmt.Fprintln(os.Stderr, "TestProxmox: all steps done")
	})
}
