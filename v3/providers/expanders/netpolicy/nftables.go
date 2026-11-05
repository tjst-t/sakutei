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

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NFTCommentMax は nft の comment の長さの上限（バイト、nftables v1.0.9）。
const NFTCommentMax = 128

// NFTablesKind は NFTables が作る宣言の type（providers/nftables の chain）。
const NFTablesKind = "chain"

// NFTables は組み込みの realizer（DefaultRealizer）：実現先 1 つを nftables の base
// chain の宣言 1 つ（"<provider>/chain"、id は <host>/<family>/<table>/<chain>）に
// する。base chain は type filter、hook は boundary が forward・host が input、
// priority 0、policy drop が既定（settings の hook・priority・policy で変える）。
// ルールは before・許可・after の順。許可 1 つはアドレスファミリ（IPv4・IPv6）ごとに
// 1 ルールで、reason を comment に出す（E-B5）。live のホストの base chain は
// two-phase Apply（Transaction）が要るので、実現先の apply: に transaction を書く（E-B3）。
type NFTables struct{}

var nftSettings = map[string]bool{"hook": true, "priority": true, "policy": true}

// Realize は実現先を nftables の base chain の宣言 1 つにする。
func (NFTables) Realize(t Target, allows []Passing) (Output, error) {
	if len(t.Interfaces) > 0 {
		return Output{}, fmt.Errorf("interfaces: nftables の realizer は domain %s だけを見て、interfaces を受けない（interface ごとに分ける realizer を WithRealizer で足す）", DefaultDomain)
	}
	parts := strings.Split(t.ID, "/")
	if len(parts) != 4 {
		return Output{}, fmt.Errorf("id %q: <host>/<family>/<table>/<chain> を書く（nftables の chain の id）", t.ID)
	}
	for _, s := range parts {
		if s == "" {
			return Output{}, fmt.Errorf("id %q に空のセグメントがある", t.ID)
		}
	}
	family := parts[1]
	v4, v6 := false, false
	switch family {
	case "ip":
		v4 = true
	case "ip6":
		v6 = true
	case "inet":
		v4, v6 = true, true
	default:
		return Output{}, fmt.Errorf("id %q: family %s: ip・ip6・inet のどれか", t.ID, family)
	}
	keys := make([]string, 0, len(t.Settings))
	for k := range t.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !nftSettings[k] {
			return Output{}, fmt.Errorf("settings: 不明な field %q（nftables の realizer が受けるのは hook・priority・policy）", k)
		}
	}
	setting := func(k, def string) string {
		if v := strings.TrimSpace(t.Settings[k]); v != "" {
			return v
		}
		return def
	}
	hook := "input"
	if t.Kind == KindBoundary {
		hook = "forward"
	}
	prio, err := strconv.ParseInt(setting("priority", "0"), 10, 64)
	if err != nil {
		return Output{}, fmt.Errorf("settings.priority %q: 整数を書く", t.Settings["priority"])
	}

	var rules []any
	var out Output
	add := func(idx int, text string) {
		rules = append(rules, text)
		out.Rules = append(out.Rules, Rule{Allow: idx, Resource: t.ID, Text: text})
	}
	for _, r := range t.Before {
		if err := checkRule(r); err != nil {
			return Output{}, fmt.Errorf("before %q: %w", r, err)
		}
		add(-1, r)
	}
	for _, p := range allows {
		for _, af := range []int{4, 6} {
			if (af == 4 && !v4) || (af == 6 && !v6) {
				continue
			}
			if text, ok := NFTRule(af, p); ok {
				add(p.Allow.Index, text)
			}
		}
	}
	for _, r := range t.After {
		if err := checkRule(r); err != nil {
			return Output{}, fmt.Errorf("after %q: %w", r, err)
		}
		add(-1, r)
	}
	if rules == nil {
		rules = []any{}
	}
	res := Resource{
		"type": t.Provider + "/" + NFTablesKind,
		"id":   t.ID,
		"fields": map[string]any{
			"type": "filter", "hook": setting("hook", hook), "priority": prio,
			"policy": setting("policy", "drop"), "rules": rules,
		},
		"reason": "network_policy の実現先 " + t.Name,
	}
	if len(t.Labels) > 0 {
		res["labels"] = t.Labels
	}
	if len(t.Apply) > 0 {
		res["apply"] = t.Apply
	}
	out.Resources = []Resource{res}
	return out, nil
}

func checkRule(r string) error {
	switch {
	case r == "":
		return fmt.Errorf("空のルール")
	case strings.ContainsAny(r, "\n\r;"):
		return fmt.Errorf("1 つの行に 1 つのルール（改行と ; は書けない）")
	}
	return nil
}

// NFTRule は許可 p のアドレスファミリ af（4 か 6）の nftables のルール文。from か
// to にそのファミリの CIDR が無い・proto がそのファミリに無い（icmp と IPv6 など）なら false。
func NFTRule(af int, p Passing) (string, bool) {
	from, to := ofFamily(p.From.CIDRs, af), ofFamily(p.To.CIDRs, af)
	if len(from) == 0 || len(to) == 0 {
		return "", false
	}
	a := p.Allow
	if (a.Proto == ProtoICMP && af != 4) || (a.Proto == ProtoICMPv6 && af != 6) {
		return "", false
	}
	kw := "ip"
	if af == 6 {
		kw = "ip6"
	}
	parts := []string{kw + " saddr " + addrSet(from), kw + " daddr " + addrSet(to)}
	switch {
	case len(a.Ports) > 0:
		ps := make([]string, len(a.Ports))
		for i, r := range a.Ports {
			ps[i] = r.String()
		}
		parts = append(parts, a.Proto+" dport "+set(ps))
	case a.Proto == ProtoICMPv6:
		parts = append(parts, "meta l4proto ipv6-icmp")
	case a.Proto != ProtoAny:
		parts = append(parts, "meta l4proto "+a.Proto)
	}
	parts = append(parts, "accept")
	if c := NFTComment(a.Reason); c != "" {
		parts = append(parts, `comment "`+c+`"`)
	}
	return NormalizeSets(strings.Join(parts, " ")), true
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

func addrSet(cidrs []netip.Prefix) string {
	el := make([]string, len(cidrs))
	for i, c := range cidrs {
		if c.IsSingleIP() {
			el[i] = c.Addr().String()
		} else {
			el[i] = c.String()
		}
	}
	return set(el)
}

func set(el []string) string {
	if len(el) == 1 {
		return el[0]
	}
	return "{ " + strings.Join(el, ", ") + " }"
}

// NFTComment は reason を nft の comment に入る形にする（引用符の中身。空の
// reason は ""）。
func NFTComment(reason string) string {
	var b strings.Builder
	space := false
	for _, r := range reason {
		switch {
		case r == '"':
			r = '\''
		case r == '\\':
			r = '/'
		case r == ';':
			r = ','
		case unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError:
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	s := b.String()
	if len(s) <= NFTCommentMax {
		return s
	}
	const ell = "…"
	cut := NFTCommentMax - len(ell)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + ell
}
