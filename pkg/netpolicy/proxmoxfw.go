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

// proxmoxfw.go — 本体の realizer proxmox-fw（Backlog #197）: 許可を Proxmox VE
// の VM の FW の規則（proxmox-fw provider の rules）へ展開する。
//
// 設計判断:
//
//   - 実現先 1 つ = VM 1 台の FW の規則の並び（id は proxmox-fw provider の
//     local id、`<cluster>/<VM 名>/fw`）。規則は before・許可・after の順で、
//     Sakutei の規則として並びの先頭に置かれる（手の規則はその後ろに残る）。
//   - 口（Target.Interfaces）を書いた実現先では、許可が通る口と向き（Via）
//     ごとに規則 1 つ：向き in は `in`、out は `out`、口は `iface=netN`。
//     VM の FW は口ごとに入り（VM に入る）と出（VM から出る）を見るので、
//     ホストは to が自分の網の口で in・from が自分の網の口で out、境界（VM が
//     網の間を渡す）は from の網の口で in と to の網の口で out の 2 つになる。
//   - 口を書かないホストは `in` の規則（口を付けない）。口を書かない境界は
//     どの口で受けるか決まらないので断る。
//   - 許可 1 つはアドレスの族（IPv4・IPv6）ごとに 1 規則（from と to の両方に
//     その族の CIDR があるときだけ。Proxmox は 1 規則に族を混ぜられない）。
//     icmp は IPv4 だけ、icmpv6 は IPv6 だけ。ポートは `80,443,8000:8100`。
//   - reason は comment に出す（pvefwrule.Comment で整える）。provider が
//     Proxmox に書くときに印 `sakutei: ` を付ける。
//   - settings は受けない（既定の拒否は VM の FW の options の policy_in・
//     policy_out、proxmox-vm の firewall 欄で書く）。

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/pvefwrule"
)

// ProxmoxFWRealizer は realizer の名前。
const ProxmoxFWRealizer = "proxmox-fw"

// ProxmoxFWProvider は ProxmoxFW が作る resource の provider。
const ProxmoxFWProvider = "proxmox-fw"

var (
	pveIfaceRe = regexp.MustCompile(`^net[0-9]+$`)
	pveIDRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+/[a-zA-Z0-9][a-zA-Z0-9.-]*/fw$`)
)

// ProxmoxFW は本体の realizer proxmox-fw。
type ProxmoxFW struct{}

// Realize は実現先を VM 1 台の FW の規則の並びにする。
func (ProxmoxFW) Realize(t Target, allows []Passing) (Output, error) {
	if !pveIDRe.MatchString(t.ID) {
		return Output{}, fmt.Errorf("id %q: want <cluster>/<vm name>/fw (the proxmox-fw resource id)", t.ID)
	}
	if len(t.Settings) > 0 {
		return Output{}, fmt.Errorf("settings: the proxmox-fw realizer takes none (the default policy is the VM's firewall options, proxmox-vm's firewall field)")
	}
	for _, itf := range t.Interfaces {
		if !pveIfaceRe.MatchString(itf.Name) {
			return Output{}, fmt.Errorf("interface %q: want a VM NIC name net0, net1, ...", itf.Name)
		}
	}
	if len(t.Interfaces) == 0 && t.Kind == KindBoundary {
		return Output{}, fmt.Errorf("a boundary needs interfaces (which NIC the traffic comes in and goes out of)")
	}
	var rules []pvefwrule.Rule
	var out Output
	add := func(idx int, r pvefwrule.Rule) {
		rules = append(rules, r)
		out.Rules = append(out.Rules, Rule{Allow: idx, Resource: t.ID, Text: r.String()})
	}
	for _, s := range t.Before {
		r, err := pvefwrule.Parse(s)
		if err != nil {
			return Output{}, fmt.Errorf("before: %w", err)
		}
		add(-1, r)
	}
	for _, p := range allows {
		vias := p.Via
		if len(t.Interfaces) == 0 {
			vias = []Via{{Direction: DirectionIn}}
		}
		for _, v := range vias {
			for _, af := range []int{4, 6} {
				if r, ok := PVERule(af, v, p); ok {
					add(p.Allow.Index, r)
				}
			}
		}
	}
	for _, s := range t.After {
		r, err := pvefwrule.Parse(s)
		if err != nil {
			return Output{}, fmt.Errorf("after: %w", err)
		}
		add(-1, r)
	}
	out.Resources = []Resource{{Provider: ProxmoxFWProvider, Scope: t.Scope, ID: t.ID,
		Fields: map[string]string{"rules": pvefwrule.Join(rules)}}}
	return out, nil
}

// PVERule は許可 p の口と向き v、アドレスの族 af（4 か 6）の VM の FW の規則。
// from か to にその族の CIDR が無い・proto がその族に無いなら false。
func PVERule(af int, v Via, p Passing) (pvefwrule.Rule, bool) {
	from, to := ofFamily(p.From.CIDRs, af), ofFamily(p.To.CIDRs, af)
	if len(from) == 0 || len(to) == 0 {
		return pvefwrule.Rule{}, false
	}
	a := p.Allow
	if (a.Proto == ProtoICMP && af != 4) || (a.Proto == ProtoICMPv6 && af != 6) {
		return pvefwrule.Rule{}, false
	}
	typ := pvefwrule.TypeIn
	if v.Direction == DirectionOut {
		typ = pvefwrule.TypeOut
	}
	r := pvefwrule.Rule{Type: typ, Action: "ACCEPT", Iface: v.Interface,
		Source: pveAddrs(from), Dest: pveAddrs(to), Comment: pvefwrule.Comment(a.Reason)}
	if a.Proto != ProtoAny {
		r.Proto = a.Proto
	}
	if len(a.Ports) > 0 {
		ps := make([]string, len(a.Ports))
		for i, pr := range a.Ports {
			ps[i] = strings.ReplaceAll(pr.String(), "-", ":")
		}
		r.Dport = strings.Join(ps, ",")
	}
	return r, true
}

func pveAddrs(cidrs []netip.Prefix) string {
	el := make([]string, len(cidrs))
	for i, c := range cidrs {
		if c.IsSingleIP() {
			el[i] = c.Addr().String()
		} else {
			el[i] = c.String()
		}
	}
	return strings.Join(el, ",")
}
