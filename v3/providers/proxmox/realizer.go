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

package proxmox

// realizer.go — network_policy（Expand のモジュール go:netpolicy）の実現先
// proxmox-fw：許可を VM の FW の規則（種類 fw-rules の rules）へ展開する（v2 の
// pkg/netpolicy の proxmox-fw を移したもの、E-B6・E-B5）。netpolicy.WithRealizer で
// 足す（cmd/sakutei が組み込む）。
//
//   - 実現先 1 つ = VM 1 台の FW の規則の並び（id は fw-rules の id
//     "<クラスタ>/<VM 名>"）。規則は before・許可・after の順で、Sakutei の規則と
//     して並びの先頭に置かれる（手の規則はその後ろに残る）。
//   - 口（interfaces）を書いた実現先では、許可が通る口と向きごとに規則 1 つ：向き
//     in は "in"、out は "out"、口は "iface=netN"。口を書かないホストは "in" の規則
//     （口を付けない）。口を書かない境界はどの口で受けるか決まらないので断る。
//   - 許可 1 つはアドレスの族（IPv4・IPv6）ごとに 1 規則（Proxmox は 1 規則に族を
//     混ぜられない）。icmp は IPv4 だけ、icmpv6 は IPv6 だけ。
//   - reason は comment に出す。provider が Proxmox に書くときに印を付ける。
//   - 作る宣言は同じ id の vm に依る（VM ができてから規則を置く）。
//   - settings は受けない（既定の拒否は vm の firewall の policy_in・policy_out）。

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/tjst-t/sakutei/v3/providers/expanders/netpolicy"
	"github.com/tjst-t/sakutei/v3/providers/proxmox/fwrule"
)

// RealizerName は target の realizer: に書く名前。
const RealizerName = "proxmox-fw"

var (
	pveIfaceRe = regexp.MustCompile(`^net[0-9]+$`)
	pveIDRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+/[a-zA-Z0-9][a-zA-Z0-9.-]*$`)
)

// Realizer は実現先 proxmox-fw。
type Realizer struct{}

var _ netpolicy.Realizer = Realizer{}

// Realize は実現先を VM 1 台の FW の規則の並び（fw-rules の宣言）にする。
func (Realizer) Realize(t netpolicy.Target, allows []netpolicy.Passing) (netpolicy.Output, error) {
	if !pveIDRe.MatchString(t.ID) {
		return netpolicy.Output{}, fmt.Errorf("id %q は fw-rules の id（<クラスタ>/<VM 名>）", t.ID)
	}
	if len(t.Settings) > 0 {
		return netpolicy.Output{}, fmt.Errorf("settings は書けない（既定の拒否は vm の firewall の policy_in・policy_out）")
	}
	for _, itf := range t.Interfaces {
		if !pveIfaceRe.MatchString(itf.Name) {
			return netpolicy.Output{}, fmt.Errorf("口 %q は VM の NIC の名前（net0・net1…）", itf.Name)
		}
	}
	if len(t.Interfaces) == 0 && t.Kind == netpolicy.KindBoundary {
		return netpolicy.Output{}, fmt.Errorf("境界には interfaces（どの NIC で受けてどの NIC から出すか）が要る")
	}
	rules := []any{}
	var out netpolicy.Output
	add := func(idx int, r fwrule.Rule) {
		rules = append(rules, r.String())
		out.Rules = append(out.Rules, netpolicy.Rule{Allow: idx, Resource: t.ID, Text: r.String()})
	}
	for _, s := range t.Before {
		r, err := fwrule.Parse(s)
		if err != nil {
			return netpolicy.Output{}, fmt.Errorf("before: %w", err)
		}
		add(-1, r)
	}
	for _, p := range allows {
		vias := p.Via
		if len(t.Interfaces) == 0 {
			vias = []netpolicy.Via{{Direction: netpolicy.DirectionIn}}
		}
		for _, v := range vias {
			for _, af := range []int{4, 6} {
				if r, ok := pveRule(af, v, p); ok {
					add(p.Allow.Index, r)
				}
			}
		}
	}
	for _, s := range t.After {
		r, err := fwrule.Parse(s)
		if err != nil {
			return netpolicy.Output{}, fmt.Errorf("after: %w", err)
		}
		add(-1, r)
	}
	res := netpolicy.Resource{
		"type":   t.Provider + "/" + KindFWRules,
		"id":     t.ID,
		"fields": map[string]any{FieldRules: rules},
		"reason": "network_policy の実現先 " + t.Name,
		// 規則は VM ができてから置く（宣言に無い VM なら在るまで待つ、E-J7）。
		"depends_on": []any{map[string]any{"id": t.Provider + "/" + KindVM + "/" + t.ID}},
	}
	if len(t.Labels) > 0 {
		res["labels"] = t.Labels
	}
	if len(t.Apply) > 0 {
		res["apply"] = t.Apply
	}
	out.Resources = []netpolicy.Resource{res}
	return out, nil
}

// pveRule は許可 p の口と向き v、アドレスの族 af（4 か 6）の規則。from か to に
// その族の CIDR が無い・proto がその族に無いなら false。
func pveRule(af int, v netpolicy.Via, p netpolicy.Passing) (fwrule.Rule, bool) {
	from, to := ofFamily(p.From.CIDRs, af), ofFamily(p.To.CIDRs, af)
	if len(from) == 0 || len(to) == 0 {
		return fwrule.Rule{}, false
	}
	a := p.Allow
	if (a.Proto == netpolicy.ProtoICMP && af != 4) || (a.Proto == netpolicy.ProtoICMPv6 && af != 6) {
		return fwrule.Rule{}, false
	}
	typ := fwrule.TypeIn
	if v.Direction == netpolicy.DirectionOut {
		typ = fwrule.TypeOut
	}
	r := fwrule.Rule{Type: typ, Action: "ACCEPT", Iface: v.Interface,
		Source: pveAddrs(from), Dest: pveAddrs(to), Comment: fwrule.Comment(a.Reason)}
	if a.Proto != netpolicy.ProtoAny {
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

func ofFamily(cidrs []netip.Prefix, af int) []netip.Prefix {
	var out []netip.Prefix
	for _, c := range cidrs {
		if (af == 4) == c.Addr().Is4() {
			out = append(out, c)
		}
	}
	return out
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
