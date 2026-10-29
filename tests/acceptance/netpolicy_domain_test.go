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

package acceptance

// netpolicy_domain_test.go — #195: network_policy の網に domain（アドレスの
// 空間）を足す。
//
//	[AC-195-1-1]  domain を書かない宣言の展開（plan の文・base chain の宣言）は、
//	              全部の網に domain: default を書いたものと同じで、今の形の
//	              ルールのまま。
//	[AC-195-1-2]  同じ domain の網の重なりは読み込みで断り（終了コード 1）、
//	              違う domain の網の重なりは通る。
//	[AC-195-1-3]  domain をまたぐ許可は読み込みで断る（行の場所と両方の domain）。
//	              口を書かない実現先が既定の domain の外の網を名指すのも断る。
//	[AC-195-1-4]  外の realizer（cli.WithRealizer で足した自前のバイナリ）に、
//	              口ごとの domain と、口を通る許可の向き（in・out）が渡る。
//	              plan の文と --json に口・domain・向きが出る。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/planreport"
)

// withDomain は netPolicy の網に domain を書き足した写し。
func withDomain(policy map[string]any, domain string) map[string]any {
	out := map[string]any{}
	for k, v := range policy {
		out[k] = v
	}
	var nets []map[string]any
	for _, n := range policy["networks"].([]map[string]any) {
		c := map[string]any{"domain": domain}
		for k, v := range n {
			c[k] = v
		}
		nets = append(nets, c)
	}
	out["networks"] = nets
	return out
}

func TestNetworkPolicyDomain_DefaultIsUnchanged(t *testing.T) {
	// [AC-195-1-1]
	plan := func(policy map[string]any, extra ...string) string {
		dir := t.TempDir()
		writeIntentFile(t, dir, map[string]any{"network_policy": policy})
		out, stderr, code := runPlan(t, dir, filepath.Join(dir, "state.json"), append([]string{"--ruleset", filepath.Join(dir, "rules")}, extra...)...)
		if code != 0 {
			t.Fatalf("plan exit %d: %s", code, stderr)
		}
		return out
	}
	implicit := plan(netPolicy(allowWeb, allowTelnet, allowSQL))
	explicit := plan(withDomain(netPolicy(allowWeb, allowTelnet, allowSQL), "default"))
	// 1 行目の revision は intent のファイルの中身から作るので違う。
	_, implicitBody, _ := strings.Cut(implicit, "\n")
	_, explicitBody, _ := strings.Cut(explicit, "\n")
	if implicitBody != explicitBody {
		t.Errorf("[AC-195-1-1] domain: default must expand the same as no domain:\n--- no domain\n%s\n--- domain: default\n%s", implicit, explicit)
	}
	for _, want := range []string{
		"target fw1 (boundary, nftables) → 3 allow(s) nftables/inet/filter/forward",
		`→ ip saddr 10.1.0.0/24 ip daddr 192.0.2.0/24 tcp dport 443 accept comment "社内から公開の web"`,
		`→ ip saddr 192.0.2.0/24 ip daddr 10.2.0.10 tcp dport 5432 accept comment "web から '本番' の DB"`,
		"+ CREATE nftables/site=lab/inet/filter/forward",
	} {
		if !strings.Contains(implicit, want) {
			t.Errorf("[AC-195-1-1] plan must contain %q:\n%s", want, implicit)
		}
	}
	if strings.Contains(implicit, "domain:") || strings.Contains(implicit, "interface ") {
		t.Errorf("[AC-195-1-1] the text of a declaration without domains must not change (no domain or interface lines):\n%s", implicit)
	}

	jsonOf := func(policy map[string]any) planreport.Report {
		var rep planreport.Report
		if err := json.Unmarshal([]byte(plan(policy, "--json")), &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	a, b := jsonOf(netPolicy(allowWeb, allowTelnet, allowSQL)), jsonOf(withDomain(netPolicy(allowWeb, allowTelnet, allowSQL), "default"))
	if len(a.Operations) != 1 || len(b.Operations) != 1 {
		t.Fatalf("[AC-195-1-1] operations = %+v / %+v", a.Operations, b.Operations)
	}
	ja, _ := json.Marshal(a.Operations)
	jb, _ := json.Marshal(b.Operations)
	if string(ja) != string(jb) {
		t.Errorf("[AC-195-1-1] the realized base chain differs:\n%s\n%s", ja, jb)
	}
	for _, al := range a.NetworkPolicy.Targets[0].Allows {
		if al.Domain != "default" || len(al.Via) != 0 {
			t.Errorf("[AC-195-1-1] an allow without domains is in domain default and passes no interface: %+v", al)
		}
	}
}

// refusePlan は plan --json が読み込みで断ることを確かめ、error を返す。
func refusePlan(t *testing.T, policy map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	writeIntentFile(t, dir, map[string]any{"network_policy": policy})
	out, stderr, code := runPlan(t, dir, filepath.Join(dir, "state.json"), "--ruleset", filepath.Join(dir, "rules"), "--json")
	if code != 1 {
		t.Fatalf("plan exit %d, want 1:\n%s\n%s", code, out, stderr)
	}
	var rep planreport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return rep.Error
}

// tenantPolicy は 2 つの利用者の網（同じアドレス）とサービスの網。
func tenantPolicy(domainB string, allows ...map[string]any) map[string]any {
	return map[string]any{
		"networks": []map[string]any{
			{"name": "tenant-a", "domain": "tenant-a", "cidrs": []string{"10.0.0.0/24"}},
			{"name": "tenant-b", "domain": domainB, "cidrs": []string{"10.0.0.0/24"}},
			{"name": "office", "cidrs": []string{"10.1.0.0/24"}},
			{"name": "dmz", "cidrs": []string{"192.0.2.0/24"}},
		},
		"allows": allows,
		"targets": []map[string]any{{
			"name": "fw1", "kind": "boundary", "networks": []string{"office", "dmz"},
			"id": "inet/filter/forward", "scope": nftLabScope,
		}},
	}
}

func TestNetworkPolicyDomain_OverlapOnlyInADomain(t *testing.T) {
	// [AC-195-1-2]
	dir := t.TempDir()
	writeIntentFile(t, dir, map[string]any{"network_policy": tenantPolicy("tenant-b", allowWeb)})
	if out, stderr, code := runPlan(t, dir, filepath.Join(dir, "state.json"), "--ruleset", filepath.Join(dir, "rules")); code != 0 {
		t.Errorf("[AC-195-1-2] networks overlapping in different domains must pass: exit %d\n%s\n%s", code, out, stderr)
	}
	msg := refusePlan(t, tenantPolicy("tenant-a", allowWeb))
	if !strings.Contains(msg, "error: overlap: networks tenant-a (10.0.0.0/24) and tenant-b (10.0.0.0/24) overlap in domain tenant-a") {
		t.Errorf("[AC-195-1-2] networks overlapping in one domain must be refused:\n%s", msg)
	}
}

func TestNetworkPolicyDomain_RefusesCrossDomain(t *testing.T) {
	// [AC-195-1-3]
	cross := map[string]any{"from": "tenant-a", "to": "dmz", "proto": "tcp", "ports": []any{443}, "reason": "利用者から公開の web"}
	msg := refusePlan(t, tenantPolicy("tenant-b", allowWeb, cross))
	for _, want := range []string{
		"error: cross_domain: resources.json: network_policy.allows[1] (tenant-a → dmz tcp 443)",
		"from tenant-a is in domain tenant-a and to dmz is in domain default",
		`reason: "利用者から公開の web"`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("[AC-195-1-3] the error must contain %q:\n%s", want, msg)
		}
	}

	p := tenantPolicy("tenant-b", allowWeb)
	p["targets"] = []map[string]any{{"name": "fw1", "kind": "boundary", "networks": []string{"office", "dmz", "tenant-a"}, "id": "inet/filter/forward"}}
	if msg := refusePlan(t, p); !strings.Contains(msg, "target fw1 names network tenant-a of domain tenant-a but has no interfaces") {
		t.Errorf("[AC-195-1-3] a target without interfaces sees only the default domain:\n%s", msg)
	}
}

// domainRealizerMain は口ごとに nftables の regular chain を 1 つずつ作る外の
// realizer（入りは iifname、出は oifname）。chain の名前は <口>-<向き>-<domain>。
const domainRealizerMain = `package main

import (
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/cli"
	"github.com/tjst-t/sakutei/v2/pkg/netpolicy"
)

func main() {
	cli.Main(cli.WithRealizer("per-port", netpolicy.RealizerFunc(
		func(t netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
			var out netpolicy.Output
			for _, ia := range netpolicy.ByInterface(t, allows) {
				for _, dir := range []struct {
					name, match string
					ps          []netpolicy.Passing
				}{{"in", "iifname", ia.In}, {"out", "oifname", ia.Out}} {
					id := "inet/filter/" + ia.Interface.Name + "-" + dir.name + "-" + ia.Interface.Domain
					var rules []string
					for _, p := range dir.ps {
						if text, ok := netpolicy.NFTRule(4, p); ok {
							text = dir.match + " \"" + ia.Interface.Name + "\" " + text
							rules = append(rules, text)
							out.Rules = append(out.Rules, netpolicy.Rule{Allow: p.Allow.Index, Resource: id, Text: text})
						}
					}
					out.Resources = append(out.Resources, netpolicy.Resource{Provider: "nftables", Scope: t.Scope, ID: id,
						Fields: map[string]string{"family": "inet", "table": "filter", "chain": ia.Interface.Name + "-" + dir.name + "-" + ia.Interface.Domain,
							"rules": strings.Join(rules, " ; ")}})
				}
			}
			return out, nil
		})))
}
`

func TestNetworkPolicyDomain_InterfacesReachTheRealizer(t *testing.T) {
	// [AC-195-1-4]
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":  "module example.com/per-port\n\ngo 1.22\n\nrequire github.com/tjst-t/sakutei/v2 v2.0.0\n\nreplace github.com/tjst-t/sakutei/v2 => " + root + "\n",
		"main.go": domainRealizerMain,
	} {
		if err := os.WriteFile(filepath.Join(mod, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := buildExternalModule(t, mod)

	// 関所の VM: net0 は利用者の網（tenant-a）、net1 はサービスの網。許可は
	// 片側ずつ書く。
	dir := t.TempDir()
	writeIntentFile(t, dir, map[string]any{"network_policy": map[string]any{
		"networks": []map[string]any{
			{"name": "users-a", "domain": "tenant-a", "cidrs": []string{"10.0.0.128/25"}},
			{"name": "gw-a", "domain": "tenant-a", "cidrs": []string{"10.0.0.1"}},
			{"name": "gw-a-svc", "domain": "service", "cidrs": []string{"172.16.0.11"}},
			{"name": "db", "domain": "service", "cidrs": []string{"172.16.1.0/24"}},
		},
		"allows": []map[string]any{
			{"from": "users-a", "to": "gw-a", "proto": "tcp", "ports": []any{3128}, "reason": "利用者から関所"},
			{"from": "gw-a-svc", "to": "db", "proto": "tcp", "ports": []any{5432}, "reason": "関所から DB"},
		},
		"targets": []map[string]any{{
			"name": "gw-a", "realizer": "per-port", "kind": "host", "networks": []string{"gw-a", "gw-a-svc"},
			"id": "gw-a", "scope": nftLabScope,
			"interfaces": []map[string]any{{"name": "net0", "domain": "tenant-a"}, {"name": "net1", "domain": "service"}},
		}},
	}})
	common := []string{"plan", "--intent", dir, "--state", filepath.Join(dir, "state.json"), "--ruleset", filepath.Join(dir, "rules")}
	text, code := runBin(t, bin, common...)
	if code != 0 {
		t.Fatalf("plan exit %d:\n%s", code, text)
	}
	for _, want := range []string{
		"interface net0 (domain tenant-a): [gw-a]",
		"interface net1 (domain service): [gw-a-svc]",
		"domain: tenant-a",
		"via:    net0 in",
		"via:    net1 out",
		`→ iifname "net0" ip saddr 10.0.0.128/25 ip daddr 10.0.0.1 tcp dport 3128 accept comment "利用者から関所"`,
		`→ oifname "net1" ip saddr 172.16.0.11 ip daddr 172.16.1.0/24 tcp dport 5432 accept comment "関所から DB"`,
		"+ CREATE nftables/site=lab/inet/filter/net0-in-tenant-a",
		"+ CREATE nftables/site=lab/inet/filter/net1-out-service",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("[AC-195-1-4] plan must contain %q:\n%s", want, text)
		}
	}

	out, code := runBin(t, bin, append(common, "--json")...)
	if code != 0 {
		t.Fatalf("plan --json exit %d:\n%s", code, out)
	}
	var rep planreport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tg := rep.NetworkPolicy.Targets[0]
	if len(tg.Interfaces) != 2 || tg.Interfaces[0].Domain != "tenant-a" || tg.Interfaces[1].Networks[0] != "gw-a-svc" {
		t.Errorf("[AC-195-1-4] interfaces = %+v", tg.Interfaces)
	}
	if len(tg.Allows) != 2 {
		t.Fatalf("[AC-195-1-4] allows = %+v", tg.Allows)
	}
	in, outb := tg.Allows[0], tg.Allows[1]
	if in.Domain != "tenant-a" || len(in.Via) != 1 || in.Via[0] != (planreport.NetworkPolicyVia{Interface: "net0", Direction: "in"}) {
		t.Errorf("[AC-195-1-4] the allow into the gateway = %+v", in)
	}
	if outb.Domain != "service" || len(outb.Via) != 1 || outb.Via[0] != (planreport.NetworkPolicyVia{Interface: "net1", Direction: "out"}) {
		t.Errorf("[AC-195-1-4] the allow out of the gateway = %+v", outb)
	}
}
