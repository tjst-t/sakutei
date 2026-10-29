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

// proxmox_fw_test.go — Backlog #197: the network_policy realizer proxmox-fw and
// the proxmox-fw provider (the VM's firewall rules) against the fake Proxmox VE
// (internal/proxmox/pvetest, which follows the rule API of Proxmox VE 9.2: a
// new rule goes to the top, disabled unless enable is given; moveto inserts
// before the old position).  A real Proxmox VE is exercised by
// tests/integration/proxmox (make integration-proxmox).
//
// AC tags:
//
//	AC-197-2-1  DefaultRegistry builds proxmox-fw from proxmox_config; without it
//	            it is an error; the provider passes sdk.ContractTester (the VM
//	            exists before the rules: Preexisting; unreachable → unknown)
//	AC-197-2-2  a gateway VM's network_policy (realizer proxmox-fw, interfaces
//	            net0 and net1) is placed by reconcile as rules in and out with
//	            iface netN, the reason in the comment after the marker, in the
//	            declared order at the top; the next reconciles are converged
//	AC-197-2-3  rules Sakutei did not place are never deleted or changed:
//	            status (text and --json) shows them unmanaged; a hand change of
//	            a placed rule, a hand rule put above them and a reordering are
//	            drift, and reconcile puts the declared order back with the hand
//	            rules after it
//	AC-197-2-4  placing, changing and removing rules is a firewall change counted
//	            as destructive by the change budget (§13): over
//	            --max-destructive-per-cycle nothing is placed
//	AC-197-2-5  the observe and write tokens (§20.7): status reads with the
//	            audit token only and needs no write token; reconcile writes with
//	            the write token; the cluster's scope is taken from the intent's
//	            providers.proxmox-fw (§7.7)
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

// gatewayNetPolicy is the gateway VM of docs/network-policy.md: on the users'
// side (net0, domain tenant-a) the VM only goes out to KDC, DNS, LDAP and NTP;
// on the service side (net1) only 443 comes in.
const gatewayNetPolicy = `"network_policy": {
  "networks": [
    {"name": "a-infra",  "domain": "tenant-a", "cidrs": ["10.0.0.10", "10.0.0.11"]},
    {"name": "gw-a",     "domain": "tenant-a", "cidrs": ["10.0.0.1"]},
    {"name": "gw-a-svc", "domain": "service",  "cidrs": ["172.16.0.11"]},
    {"name": "portal",   "domain": "service",  "cidrs": ["172.16.1.0/24"]}
  ],
  "allows": [
    {"from": "gw-a", "to": "a-infra", "proto": "udp", "ports": [53, 88, 123], "reason": "関所から DNS・KDC・NTP"},
    {"from": "gw-a", "to": "a-infra", "proto": "tcp", "ports": [53, 88, 636], "reason": "関所から DNS・KDC・LDAP"},
    {"from": "portal", "to": "gw-a-svc", "proto": "tcp", "ports": [443], "reason": "サービス側は 443 だけ"}
  ],
  "targets": [
    {"name": "gw-a", "realizer": "proxmox-fw", "kind": "host", "networks": ["gw-a", "gw-a-svc"],
     "interfaces": [{"name": "net0", "domain": "tenant-a"}, {"name": "net1", "domain": "service"}],
     "id": "lab/gw-a/fw"}
  ]}`

// gatewayRules are the rules the policy becomes (Proxmox's fields).
var gatewayRules = []string{
	"out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=udp dport=53,88,123 enable=1 # sakutei: 関所から DNS・KDC・NTP",
	"out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=tcp dport=53,88,636 enable=1 # sakutei: 関所から DNS・KDC・LDAP",
	"in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=tcp dport=443 enable=1 # sakutei: サービス側は 443 だけ",
}

// pveFWRig is pveRig with the gateway VM (vmid 301) and an intent of the given
// top-level JSON members, returning the CLI arguments.
func pveFWRig(t *testing.T, srv *pvetest.Server, cfg, members string) []string {
	t.Helper()
	srv.AddVM(pvetest.VM{VMID: 301, Node: "pve1", Name: "gw-a", Status: "running"})
	return pveFWIntent(t, cfg, members)
}

func pveFWIntent(t *testing.T, cfg, members string) []string {
	t.Helper()
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir, "net.json"), []byte("{"+members+"}"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{
		"proxmox-vm": map[string]any{"proxmox_config": cfg}, "proxmox-fw": map[string]any{"proxmox_config": cfg}}})
	return []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc,
		"--alloc-store", filepath.Join(tmp, "alloc.json")}
}

// ruleTexts is the VM's rules as Proxmox holds them.
func ruleTexts(srv *pvetest.Server, vmid int) []string {
	vm, _ := srv.VM(vmid)
	var out []string
	for _, r := range vm.Rules {
		s := r["type"] + " " + r["action"]
		for _, k := range []string{"iface", "source", "dest", "proto", "dport", "enable"} {
			if r[k] != "" {
				s += " " + k + "=" + r[k]
			}
		}
		out = append(out, s+" # "+r["comment"])
	}
	return out
}

func TestProxmoxFW_RegistryAndContract(t *testing.T) {
	// [AC-197-2-1]
	if _, err := registry.DefaultRegistry().Get(proxmox.FWName, map[string]string{}); err == nil || !strings.Contains(err.Error(), "proxmox_config") {
		t.Errorf("[AC-197-2-1] proxmox-fw without proxmox_config: err = %v; want one naming proxmox_config", err)
	}
	srv, cfg := pveRig(t)
	srv.AddVM(pvetest.VM{VMID: 301, Node: "pve1", Name: "gw-a", Status: "running"})
	srv.SetRules(301, []map[string]string{{"type": "in", "action": "DROP", "enable": "1", "comment": "hand"}})
	p, err := registry.DefaultRegistry().Get(proxmox.FWName, map[string]string{"proxmox_config": cfg})
	if err != nil {
		t.Fatalf("[AC-197-2-1] DefaultRegistry().Get(proxmox-fw): %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-197-2-1] ValidateProvider: %v", err)
	}
	if p.Capabilities().Pruneable {
		t.Error("[AC-197-2-1] proxmox-fw is pruneable; rules it did not place must never be deleted")
	}
	ct := sdk.ContractTester{
		Provider: p, Lister: p.(provider.Lister), Scope: core.NewScope(nil), Preexisting: true,
		SampleID:      "lab/gw-a/fw",
		SampleDesired: map[string]string{proxmox.FieldRules: "in ACCEPT iface=net1 proto=tcp dport=443 comment=ac-197 ; out ACCEPT iface=net0 proto=udp dport=53"},
		MakeUnreachable: func() (func(), error) {
			srv.SetUnreachable(true)
			return func() { srv.SetUnreachable(false) }, nil
		},
	}
	ct.RunAll(t)
	if got := ruleTexts(srv, 301); len(got) != 3 || got[2] != "in DROP enable=1 # hand" {
		t.Errorf("[AC-197-2-1] rules after the contract tests = %v; want the hand rule kept last", got)
	}
}

func TestProxmoxFW_RealizesGateway(t *testing.T) {
	// [AC-197-2-2]
	srv, cfg := pveRig(t)
	bin := buildBinary(t)
	common := pveFWRig(t, srv, cfg, gatewayNetPolicy)

	o, code := runBin(t, bin, append([]string{"plan"}, common...)...)
	if code != 0 || !strings.Contains(o, "target gw-a (host, proxmox-fw)") || !strings.Contains(o, "via:    net0 out") ||
		!strings.Contains(o, "→ in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=tcp dport=443 comment=サービス側は 443 だけ") ||
		!strings.Contains(o, "proxmox-fw//lab/gw-a/fw") {
		t.Fatalf("[AC-197-2-2] plan exit %d:\n%s", code, o)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-197-2-2] reconcile exit %d:\n%s", code, o)
	}
	if got := ruleTexts(srv, 301); strings.Join(got, "\n") != strings.Join(gatewayRules, "\n") {
		t.Fatalf("[AC-197-2-2] rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(gatewayRules, "\n"))
	}
	for i := 0; i < 2; i++ {
		if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "Converged (no changes)") {
			t.Fatalf("[AC-197-2-2] reconcile after convergence %d exit %d:\n%s", i, code, o)
		}
	}
}

func TestProxmoxFW_KeepsHandRules(t *testing.T) {
	// [AC-197-2-3]
	srv, cfg := pveRig(t)
	bin := buildBinary(t)
	hand := map[string]string{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": "22", "enable": "1", "comment": "管理（手）"}
	common := pveFWRig(t, srv, cfg, gatewayNetPolicy)
	srv.SetRules(301, []map[string]string{hand})
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 {
		t.Fatalf("[AC-197-2-3] reconcile exit %d:\n%s", code, o)
	}
	want := append(append([]string{}, gatewayRules...), "in ACCEPT proto=tcp dport=22 enable=1 # 管理（手）")
	if got := ruleTexts(srv, 301); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("[AC-197-2-3] rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// status: 手の規則は unmanaged（宣言の resource は適合）。
	o, code := runBin(t, bin, append([]string{"status", "--json"}, common...)...)
	var rep struct {
		Resources []struct {
			State   string
			LocalID string `json:"local_id"`
		}
	}
	if i := strings.Index(o, "{"); code != 0 || i < 0 || json.Unmarshal([]byte(o[i:]), &rep) != nil {
		t.Fatalf("[AC-197-2-3] status --json exit %d:\n%s", code, o)
	}
	states := map[string]string{}
	for _, r := range rep.Resources {
		states[r.LocalID] = r.State
	}
	if states["lab/gw-a/fw/3"] != "unmanaged" || states["lab/gw-a/fw"] != "conformant" {
		t.Errorf("[AC-197-2-3] status states %v; want lab/gw-a/fw conformant and lab/gw-a/fw/3 unmanaged", states)
	}
	if o, _ := runBin(t, bin, append([]string{"status"}, common...)...); !strings.Contains(o, "UNMANAGED proxmox-fw//lab/gw-a/fw/3") {
		t.Errorf("[AC-197-2-3] status (text) does not show the hand rule unmanaged:\n%s", o)
	}

	// 手で変える：置いた規則の port、先頭に手の規則、置いた規則の順。
	vm, _ := srv.VM(301)
	rules := vm.Rules
	rules[2]["dport"] = "8443"
	rules[0], rules[1] = rules[1], rules[0]
	rules = append([]map[string]string{{"type": "in", "action": "DROP", "enable": "1", "comment": "手で先頭に"}}, rules...)
	srv.SetRules(301, rules)
	if o, _ := runBin(t, bin, append([]string{"status"}, common...)...); !strings.Contains(o, "DRIFTED proxmox-fw//lab/gw-a/fw") || !strings.Contains(o, "(unmanaged) in DROP comment=手で先頭に") {
		t.Errorf("[AC-197-2-3] status after the hand changes:\n%s", o)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-197-2-3] reconcile after the hand changes exit %d:\n%s", code, o)
	}
	want = append(append([]string{}, gatewayRules...), "in DROP enable=1 # 手で先頭に", "in ACCEPT proto=tcp dport=22 enable=1 # 管理（手）")
	if got := ruleTexts(srv, 301); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("[AC-197-2-3] rules after putting back:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "Converged (no changes)") {
		t.Fatalf("[AC-197-2-3] reconcile after putting back exit %d:\n%s", code, o)
	}
}

func TestProxmoxFW_CountedByTheBudget(t *testing.T) {
	// [AC-197-2-4]
	srv, cfg := pveRig(t)
	srv.AddVM(pvetest.VM{VMID: 302, Node: "pve1", Name: "gw-b", Status: "running"})
	bin := buildBinary(t)
	two := strings.Replace(gatewayNetPolicy, `"id": "lab/gw-a/fw"}`, `"id": "lab/gw-a/fw"},
    {"name": "gw-b", "realizer": "proxmox-fw", "kind": "host", "networks": ["gw-a-svc"],
     "interfaces": [{"name": "net1", "domain": "service"}], "id": "lab/gw-b/fw"}`, 1)
	common := pveFWRig(t, srv, cfg, two)
	o, _ := runBin(t, bin, append([]string{"reconcile", "--max-destructive-per-cycle", "1"}, common...)...)
	if !strings.Contains(o, "max_destructive_per_cycle=1") {
		t.Errorf("[AC-197-2-4] reconcile over the budget does not report it:\n%s", o)
	}
	if got := append(ruleTexts(srv, 301), ruleTexts(srv, 302)...); len(got) != 0 {
		t.Fatalf("[AC-197-2-4] rules placed over the budget: %v", got)
	}
	if o, code := runBin(t, bin, append([]string{"reconcile", "--max-destructive-per-cycle", "2"}, common...)...); code != 0 || !strings.Contains(o, "2 change(s) applied") {
		t.Fatalf("[AC-197-2-4] reconcile within the budget exit %d:\n%s", code, o)
	}
	if len(ruleTexts(srv, 301)) != 3 || len(ruleTexts(srv, 302)) != 1 {
		t.Errorf("[AC-197-2-4] rules: gw-a %v, gw-b %v", ruleTexts(srv, 301), ruleTexts(srv, 302))
	}
}

func TestProxmoxFW_ObserveAndWriteTokens(t *testing.T) {
	// [AC-197-2-5]
	const audit = "sakutei@pve!audit"
	srv := pvetest.New(pvetest.Options{AuditTokenID: audit, AuditSecret: "audit-secret"})
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("secrets.json", `{"secrets": {"pve/audit": {"gen": 1, "value": "audit-secret"}, "pve/write": {"gen": 1, "value": "`+srv.Secret()+`"}}}`)
	write("audit-only.json", `{"secrets": {"pve/audit": {"gen": 1, "value": "audit-secret"}}}`)
	body := "secret_store: secrets.json\nclusters:\n  lab:\n    endpoint: " + srv.URL + "\n    node: pve1\n    timeout: 2s\n" +
		"    observe:\n      token_id: " + audit + "\n      token_secret: secretref:file/pve/audit\n" +
		"    write:\n      token_id: " + srv.TokenID() + "\n      token_secret: secretref:file/pve/write\n"
	cfg := write("pve.yaml", body)
	roCfg := write("pve-ro.yaml", strings.Replace(body, "secrets.json", "audit-only.json", 1))
	bin := buildBinary(t)
	scoped := `"providers": {"proxmox-fw": {"clusters": {"lab": {"scope": {"site": "lab"}}}}}, ` +
		strings.Replace(gatewayNetPolicy, `"id": "lab/gw-a/fw"}`, `"id": "lab/gw-a/fw", "scope": {"site": "lab"}}`, 1)
	common := pveFWRig(t, srv, cfg, scoped)
	srv.SetRules(301, []map[string]string{{"type": "in", "action": "DROP", "enable": "1", "comment": "hand"}})

	if o, code := runBin(t, bin, append([]string{"reconcile"}, common...)...); code != 0 || !strings.Contains(o, "1 change(s) applied") {
		t.Fatalf("[AC-197-2-5] reconcile exit %d:\n%s", code, o)
	}
	if srv.TokenCalls(audit)["WRITE"] != 0 || srv.TokenCalls(srv.TokenID())["WRITE"] == 0 {
		t.Errorf("[AC-197-2-5] writes by token: audit %v, write %v; want only the write token to write", srv.TokenCalls(audit), srv.TokenCalls(srv.TokenID()))
	}
	// 観測用だけ（書き込み用の secret が無い）：status は読めて、手の規則が scope 付きで unmanaged。
	ro := pveFWIntent(t, roCfg, scoped)
	before := srv.TokenCalls(srv.TokenID())
	o, code := runBin(t, bin, append([]string{"status"}, ro...)...)
	if code != 0 || !strings.Contains(o, "UNMANAGED proxmox-fw/site=lab/lab/gw-a/fw/3") || !strings.Contains(o, "drifted=0") {
		t.Errorf("[AC-197-2-5] status with the audit token only: exit %d:\n%s", code, o)
	}
	if after := srv.TokenCalls(srv.TokenID()); after["GET"] != before["GET"] || after["WRITE"] != before["WRITE"] {
		t.Errorf("[AC-197-2-5] status used the write token: %v → %v", before, after)
	}
}
