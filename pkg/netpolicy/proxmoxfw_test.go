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

package netpolicy

// 本体の realizer proxmox-fw（Backlog #197）。
//
//	[AC-197-1-1]  口を書いたホスト（関所の VM）：許可ごとに通る口と向きの規則
//	              （in・out と iface=netN）を族ごとに作り、reason を comment に
//	              出す。before・after は規則の文として読む。
//	[AC-197-1-2]  境界の VM は from の口で in・to の口で out の 2 つ、口を書かない
//	              ホストは口を付けない in。口を書かない境界・id・口・settings の
//	              誤りは断る。

import (
	"strings"
	"testing"
)

// gatewayPolicy は関所の VM の例（docs/network-policy.md と同じ形）。
const gatewayPolicy = `{
  "networks": [
    {"name": "a-infra",  "domain": "tenant-a", "cidrs": ["10.0.0.10", "10.0.0.11"]},
    {"name": "gw-a",     "domain": "tenant-a", "cidrs": ["10.0.0.1"]},
    {"name": "gw-a-svc", "domain": "service",  "cidrs": ["172.16.0.11", "2001:db8:16::11"]},
    {"name": "portal",   "domain": "service",  "cidrs": ["172.16.1.0/24", "2001:db8:17::/64"]}
  ],
  "allows": [
    {"from": "gw-a", "to": "a-infra", "proto": "udp", "ports": [53, 88, 123], "reason": "関所から DNS・KDC・NTP"},
    {"from": "gw-a", "to": "a-infra", "proto": "tcp", "ports": [53, 88, 636], "reason": "関所から DNS・KDC・LDAP"},
    {"from": "portal", "to": "gw-a-svc", "proto": "tcp", "ports": ["8443-8444", 443], "reason": "サービス側は 443 だけ; 古い口も"},
    {"from": "portal", "to": "gw-a-svc", "proto": "icmp", "reason": "ping"}
  ],
  "targets": [
    {"name": "gw-a", "realizer": "proxmox-fw", "kind": "host", "networks": ["gw-a", "gw-a-svc"],
     "interfaces": [{"name": "net0", "domain": "tenant-a"}, {"name": "net1", "domain": "service"}],
     "id": "lab/gw-a/fw", "scope": {"site": "lab"},
     "before": ["in ACCEPT proto=tcp dport=22 comment=管理"], "after": ["out DROP log=info"]}
  ]}`

func TestProxmoxFW_Gateway(t *testing.T) {
	// [AC-197-1-1]
	res, err := Compile(mustParse(t, gatewayPolicy))
	if err != nil {
		t.Fatal(err)
	}
	rs := res.Resources()
	if len(rs) != 1 || rs[0].Provider != "proxmox-fw" || rs[0].ID != "lab/gw-a/fw" || rs[0].Scope["site"] != "lab" {
		t.Fatalf("[AC-197-1-1] resources = %+v", rs)
	}
	want := []string{
		"in ACCEPT proto=tcp dport=22 comment=管理",
		"out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=udp dport=53,88,123 comment=関所から DNS・KDC・NTP",
		"out ACCEPT iface=net0 source=10.0.0.1 dest=10.0.0.10,10.0.0.11 proto=tcp dport=53,88,636 comment=関所から DNS・KDC・LDAP",
		"in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=tcp dport=443,8443:8444 comment=サービス側は 443 だけ, 古い口も",
		"in ACCEPT iface=net1 source=2001:db8:17::/64 dest=2001:db8:16::11 proto=tcp dport=443,8443:8444 comment=サービス側は 443 だけ, 古い口も",
		"in ACCEPT iface=net1 source=172.16.1.0/24 dest=172.16.0.11 proto=icmp comment=ping",
		"out DROP log=info",
	}
	if got := strings.Split(rs[0].Fields["rules"], " ; "); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("[AC-197-1-1] rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	rules := res.Realizations[0].Output.Rules
	if len(rules) != len(want) || rules[0].Allow != -1 || rules[1].Allow != 0 || rules[3].Allow != 2 || rules[6].Allow != -1 {
		t.Errorf("[AC-197-1-1] plan rules = %+v", rules)
	}
}

func TestProxmoxFW_KindsAndRefusals(t *testing.T) {
	// [AC-197-1-2]
	const nets = `"networks": [
	  {"name": "a", "domain": "x", "cidrs": ["10.0.0.0/24"]}, {"name": "b", "domain": "x", "cidrs": ["10.0.1.0/24"]},
	  {"name": "h", "cidrs": ["192.0.2.10"]}, {"name": "c", "cidrs": ["198.51.100.0/28"]}]`
	res, err := Compile(mustParse(t, `{`+nets+`,
	  "allows": [{"from": "a", "to": "b", "proto": "tcp", "ports": [80], "reason": "a から b"},
	             {"from": "c", "to": "h", "proto": "udp", "ports": [161]}],
	  "targets": [
	    {"name": "r", "realizer": "proxmox-fw", "kind": "boundary", "networks": ["a", "b"],
	     "interfaces": [{"name": "net0", "domain": "x", "networks": ["a"]}, {"name": "net1", "domain": "x", "networks": ["b"]}], "id": "lab/r/fw"},
	    {"name": "h", "realizer": "proxmox-fw", "kind": "host", "networks": ["h"], "id": "lab/h/fw"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res.Resources() {
		got[r.ID] = r.Fields["rules"]
	}
	if want := "in ACCEPT iface=net0 source=10.0.0.0/24 dest=10.0.1.0/24 proto=tcp dport=80 comment=a から b ; " +
		"out ACCEPT iface=net1 source=10.0.0.0/24 dest=10.0.1.0/24 proto=tcp dport=80 comment=a から b"; got["lab/r/fw"] != want {
		t.Errorf("[AC-197-1-2] boundary rules = %q", got["lab/r/fw"])
	}
	if want := "in ACCEPT source=198.51.100.0/28 dest=192.0.2.10 proto=udp dport=161"; got["lab/h/fw"] != want {
		t.Errorf("[AC-197-1-2] host without interfaces: rules = %q", got["lab/h/fw"])
	}

	for _, c := range []struct{ target, want string }{
		{`{"name": "r", "realizer": "proxmox-fw", "kind": "boundary", "networks": ["h", "c"], "id": "lab/r/fw"}`, "a boundary needs interfaces"},
		{`{"name": "h", "realizer": "proxmox-fw", "kind": "host", "networks": ["h"], "id": "lab/h"}`, "want <cluster>/<vm name>/fw"},
		{`{"name": "h", "realizer": "proxmox-fw", "kind": "host", "networks": ["h"], "id": "lab/h/fw", "settings": {"policy": "drop"}}`, "takes none"},
		{`{"name": "h", "realizer": "proxmox-fw", "kind": "host", "networks": ["h"], "id": "lab/h/fw", "before": ["in ALLOW"]}`, "before: rule"},
		{`{"name": "h", "realizer": "proxmox-fw", "kind": "host", "networks": ["a"], "interfaces": [{"name": "eth0", "domain": "x"}], "id": "lab/h/fw"}`, "want a VM NIC name"},
	} {
		_, err := Compile(mustParse(t, `{`+nets+`, "allows": [{"from": "c", "to": "h", "proto": "udp", "ports": [161]}], "targets": [`+c.target+`]}`))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("[AC-197-1-2] target %s: err = %v, want %q", c.target, err, c.want)
		}
	}
}
