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

// nftables.go — 本体の realizer: 許可を nftables の base chain の宣言（#173 の
// 形）へ展開する。
//
// 設計判断:
//
//   - 実現先 1 つ = nftables の chain 1 つ（id は nftables provider の local id、
//     `[<host>/]<family>/<table>/<chain>`）。base chain として宣言し（type filter、
//     hook は boundary が forward・host が input、priority filter、policy drop が
//     既定）、ルールは before・許可・after の順。
//   - 許可 1 つはアドレスの族（IPv4・IPv6）ごとに 1 ルール。from と to の両方に
//     その族の CIDR があるときだけ出す（family ip なら IPv4 だけ、ip6 なら IPv6
//     だけ）。icmp は IPv4 だけ、icmpv6 は IPv6 だけ。
//   - ルール文は nft が書き戻す形で作る（/32・/128 はアドレスだけ、ポートが
//     あれば `tcp dport`、無ければ `meta l4proto tcp`）、集合は
//     nftrule.NormalizeSets を通す（nft がまとめても比べ方で適合、#178）。
//   - reason は comment に出す。nft の comment は 128 バイトまでで、`"` を
//     逃がせない。`"` は `'` に、`\` は `/` に、`;` は `,` に（ルールの区切り
//     ` ; ` を作らない）、制御文字と続く空白は 1 つの空白にし、128 バイトを
//     超えれば文字の境目で切って `…` を付ける。読み戻しでも同じ文字列になる
//     （統合テストで確かめた）。

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tjst-t/sakutei/v2/internal/nftrule"
)

// NFTCommentMax は nft の comment の長さの上限（バイト、nftables v1.0.9）。
const NFTCommentMax = 128

// NFTablesProvider は NFTables が作る resource の provider。
const NFTablesProvider = "nftables"

// NFTables は本体の realizer（DefaultRealizer）。
type NFTables struct{}

var nftSettings = map[string]bool{"hook": true, "priority": true, "policy": true, "confirm": true}

// Realize は実現先を nftables の base chain の宣言 1 つにする。
func (NFTables) Realize(t Target, allows []Passing) (Output, error) {
	parts := strings.Split(t.ID, "/")
	if len(parts) != 3 && len(parts) != 4 {
		return Output{}, fmt.Errorf("id %q: want [<host>/]<family>/<table>/<chain> (the nftables resource id)", t.ID)
	}
	for _, s := range parts {
		if s == "" {
			return Output{}, fmt.Errorf("id %q: an empty part", t.ID)
		}
	}
	coords := parts[len(parts)-3:]
	family, table, chain := coords[0], coords[1], coords[2]
	v4, v6 := false, false
	switch family {
	case "ip":
		v4 = true
	case "ip6":
		v6 = true
	case "inet":
		v4, v6 = true, true
	default:
		return Output{}, fmt.Errorf("id %q: family %s: want ip, ip6 or inet", t.ID, family)
	}
	keys := make([]string, 0, len(t.Settings))
	for k := range t.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !nftSettings[k] {
			return Output{}, fmt.Errorf("settings: unknown key %q (the nftables realizer takes hook, priority, policy and confirm)", k)
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
	fields := map[string]string{
		"family": family, "table": table, "chain": chain,
		"type": "filter", "hook": setting("hook", hook),
		"priority": setting("priority", "filter"), "policy": setting("policy", "drop"),
	}
	if c := setting("confirm", ""); c != "" {
		fields["confirm"] = c
	}

	var rules []string
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
	fields["rules"] = strings.Join(rules, nftrule.RuleSep)
	out.Resources = []Resource{{Provider: NFTablesProvider, Scope: t.Scope, ID: t.ID, Fields: fields}}
	return out, nil
}

func checkRule(r string) error {
	switch {
	case r == "":
		return fmt.Errorf("an empty rule")
	case strings.Contains(r, nftrule.RuleSep), strings.ContainsAny(r, "\n\r"):
		return fmt.Errorf("one rule per entry (no %q, no newline)", nftrule.RuleSep)
	}
	return nil
}

// NFTRule は許可 p のアドレスの族 af（4 か 6）の nftables のルール文。from か
// to にその族の CIDR が無い・proto がその族に無い（icmp と IPv6 など）なら false。
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
	return nftrule.NormalizeSets(strings.Join(parts, " ")), true
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
