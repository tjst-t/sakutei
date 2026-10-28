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

// Package netpolicy は網の間の通信の許可を、機器に依らない抽象のリソースとして
// 持つ（intent のトップレベルの network_policy:、§7.6・Backlog #174）。
//
//	network_policy:
//	  networks:
//	    - { name: office, cidrs: ["10.1.0.0/24"] }
//	    - { name: dmz,    cidrs: ["192.0.2.0/24", "2001:db8:2::/64"] }
//	  allows:
//	    - { from: office, to: dmz, proto: tcp, ports: [443], reason: "社内から公開の web" }
//	    - { from: office, to: dmz, proto: tcp, ports: [23], reason: "古い装置は telnet だけ", tags: [exception] }
//	  targets:
//	    - name: fw1
//	      kind: boundary               # 網と網の間（境界の FW）。host はそのホストに入る通信
//	      networks: [office, dmz]
//	      id: fw1/inet/filter/forward  # 実現先の resource の id（realizer が決める形）
//	      before: ["ct state established,related accept"]
//
// 既定は拒否で、書いた許可だけが通る。検査（Check）は網の CIDR の重なり・宣言に
// 無い網・理由の無い例外・同じ許可の重複を断り（error）、前の許可に覆われて
// 効かない許可・どの実現先も通らない許可を知らせる（warning）。展開（Compile）は
// 実現先ごとに、そこを通る許可だけを realizer に渡し、realizer が resource の
// 宣言（nftables なら base chain、#173 の形）を作る。
//
// 本体の realizer は nftables だけ（NFTables）。使う側は Register（pkg/cli の
// WithRealizer）で別の実現先（スイッチの ACL など）への変換を足す。realizer には
// 各許可の reason と tags も渡る。
package netpolicy

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// TagException は「弱い方式をその機器だけ例外として許す」行の印。この印の
// 付いた許可は reason が要る（読み込みで断る）。
const TagException = "exception"

// 実現先の種類。
const (
	// KindBoundary は網と網の間の境界（境界の FW）。from も to もその実現先の
	// networks にあり、from と to が違う許可が通る。
	KindBoundary = "boundary"
	// KindHost はそのホストに入る通信（各ホストの FW）。to がその実現先の
	// networks にある許可が通る。
	KindHost = "host"
)

// proto の値。
const (
	ProtoAny    = "any"
	ProtoTCP    = "tcp"
	ProtoUDP    = "udp"
	ProtoICMP   = "icmp"
	ProtoICMPv6 = "icmpv6"
)

// DefaultRealizer は target の realizer を書かないときの実現先の変換。
const DefaultRealizer = "nftables"

// Network は名前の付いた網（CIDR の一覧）。
type Network struct {
	Name  string
	CIDRs []netip.Prefix
	// Ref は宣言の場所（"<file>: networks[0]"）。
	Ref string
}

// PortRange は両端を含むポートの範囲（1 つのポートは Lo == Hi）。
type PortRange struct{ Lo, Hi int }

func (r PortRange) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(r.Lo)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

// Allow は網から網への 1 つの許可。
type Allow struct {
	// Index は宣言の順（intent のファイルの名前の順、ファイルの中の順）での位置。
	Index int
	From  string
	To    string
	// Proto は ProtoAny・ProtoTCP・ProtoUDP・ProtoICMP・ProtoICMPv6。
	Proto string
	// Ports は宛先のポート（tcp・udp だけ）。空ならすべてのポート。並べて
	// 重なりを合わせた形。
	Ports  []PortRange
	Reason string
	Tags   []string
	// Ref は宣言の場所（"<file>: allows[2]"）。
	Ref string
}

// HasTag は許可に印 tag があるか。
func (a Allow) HasTag(tag string) bool {
	for _, t := range a.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Summary は許可の短い形（"office → dmz tcp 443,8000-8100"）。
func (a Allow) Summary() string {
	s := a.From + " → " + a.To + " " + a.Proto
	if len(a.Ports) > 0 {
		ps := make([]string, len(a.Ports))
		for i, p := range a.Ports {
			ps[i] = p.String()
		}
		s += " " + strings.Join(ps, ",")
	}
	return s
}

// Target は 1 つの実現先（境界の FW・各ホストなど）。
type Target struct {
	Name string
	// Realizer は変換の名前（既定 DefaultRealizer）。
	Realizer string
	// Kind は KindBoundary か KindHost。
	Kind     string
	Networks []string
	// ID と Scope は実現先の resource の id と scope（形は realizer が決める）。
	ID    string
	Scope map[string]string
	// Before と After は許可の前と後に置く規則（realizer の言葉で書く。
	// nftables なら `ct state established,related accept`・`jump mgmt`）。
	Before []string
	After  []string
	// Settings は realizer に固有の値（nftables なら hook・priority・policy・
	// confirm）。
	Settings map[string]string
	Ref      string
}

// Policy は intent の全部のファイルの network_policy: を合わせたもの。
type Policy struct {
	Networks []Network
	Allows   []Allow
	Targets  []Target
}

// Empty は何も宣言していないか。
func (p Policy) Empty() bool {
	return len(p.Networks) == 0 && len(p.Allows) == 0 && len(p.Targets) == 0
}

// Network は名前の網を返す。
func (p Policy) Network(name string) (Network, bool) {
	for _, n := range p.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return Network{}, false
}

// Merge は後のファイルの宣言を前の宣言に足す（許可の Index は足した順に振り直す）。
func (p *Policy) Merge(o Policy) {
	p.Networks = append(p.Networks, o.Networks...)
	for _, a := range o.Allows {
		a.Index = len(p.Allows)
		p.Allows = append(p.Allows, a)
	}
	p.Targets = append(p.Targets, o.Targets...)
}

// ─────────────────────────────────────────────────────────────────────────────
// 読み込み
// ─────────────────────────────────────────────────────────────────────────────

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Parse は 1 つの intent のファイルの network_policy: の値（JSON Schema で
// 形を確かめた木）を読む。file は場所の表示に使う。宣言の中だけで分かる誤り
// （値の形・proto と ports の組・同じ印の二度書き）はここで断り、ファイルを
// またぐ検査は Check が行う。
func Parse(v any, file string) (Policy, error) {
	if v == nil {
		return Policy{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Policy{}, fmt.Errorf("network_policy: オブジェクトでなければなりません")
	}
	var p Policy
	for i, raw := range asList(m["networks"]) {
		ref := fmt.Sprintf("%s: network_policy.networks[%d]", file, i)
		n, err := parseNetwork(raw, ref)
		if err != nil {
			return Policy{}, fmt.Errorf("%s: %w", ref, err)
		}
		p.Networks = append(p.Networks, n)
	}
	for i, raw := range asList(m["allows"]) {
		ref := fmt.Sprintf("%s: network_policy.allows[%d]", file, i)
		a, err := parseAllow(raw, ref)
		if err != nil {
			return Policy{}, fmt.Errorf("%s: %w", ref, err)
		}
		a.Index = i
		p.Allows = append(p.Allows, a)
	}
	for i, raw := range asList(m["targets"]) {
		ref := fmt.Sprintf("%s: network_policy.targets[%d]", file, i)
		t, err := parseTarget(raw, ref)
		if err != nil {
			return Policy{}, fmt.Errorf("%s: %w", ref, err)
		}
		p.Targets = append(p.Targets, t)
	}
	return p, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func strList(v any, what string) ([]string, error) {
	var out []string
	for i, raw := range asList(v) {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: 文字列でなければなりません", what, i)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

func parseNetwork(raw any, ref string) (Network, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Network{}, fmt.Errorf("オブジェクトでなければなりません")
	}
	n := Network{Name: str(m, "name"), Ref: ref}
	if !nameRe.MatchString(n.Name) {
		return Network{}, fmt.Errorf("name %q: want letters, digits, '_', '.', '-' (starting with a letter or digit)", n.Name)
	}
	cidrs, err := strList(m["cidrs"], "cidrs")
	if err != nil {
		return Network{}, err
	}
	if len(cidrs) == 0 {
		return Network{}, fmt.Errorf("network %s: cidrs is required (at least one CIDR)", n.Name)
	}
	for _, c := range cidrs {
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			// アドレスだけなら 1 つのアドレスの網（/32・/128）として受ける。
			addr, aerr := netip.ParseAddr(c)
			if aerr != nil || addr.Zone() != "" {
				return Network{}, fmt.Errorf("network %s: cidr %q: want a CIDR such as 10.0.0.0/24 or an address", n.Name, c)
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		if pfx.Addr().Is4In6() {
			return Network{}, fmt.Errorf("network %s: cidr %q: an IPv4-mapped IPv6 address is not accepted (write the IPv4 form)", n.Name, c)
		}
		if pfx.Masked() != pfx {
			return Network{}, fmt.Errorf("network %s: cidr %q has host bits set (did you mean %s?)", n.Name, c, pfx.Masked())
		}
		n.CIDRs = append(n.CIDRs, pfx)
	}
	return n, nil
}

func parseAllow(raw any, ref string) (Allow, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Allow{}, fmt.Errorf("オブジェクトでなければなりません")
	}
	a := Allow{From: str(m, "from"), To: str(m, "to"), Proto: str(m, "proto"), Ref: ref}
	if a.From == "" || a.To == "" {
		return Allow{}, fmt.Errorf("from and to are required (network names)")
	}
	if a.Proto == "" {
		a.Proto = ProtoAny
	}
	switch a.Proto {
	case ProtoAny, ProtoTCP, ProtoUDP, ProtoICMP, ProtoICMPv6:
	default:
		return Allow{}, fmt.Errorf("proto %q: want tcp, udp, icmp, icmpv6 or any", a.Proto)
	}
	reason, _ := m["reason"].(string)
	a.Reason = strings.Join(strings.Fields(reason), " ")
	tags, err := strList(m["tags"], "tags")
	if err != nil {
		return Allow{}, err
	}
	seen := map[string]bool{}
	for _, t := range tags {
		if !nameRe.MatchString(t) {
			return Allow{}, fmt.Errorf("tag %q: want letters, digits, '_', '.', '-'", t)
		}
		if seen[t] {
			return Allow{}, fmt.Errorf("tag %q is written twice", t)
		}
		seen[t] = true
		a.Tags = append(a.Tags, t)
	}
	ports, err := parsePorts(m["ports"])
	if err != nil {
		return Allow{}, err
	}
	if len(ports) > 0 && a.Proto != ProtoTCP && a.Proto != ProtoUDP {
		return Allow{}, fmt.Errorf("ports need proto tcp or udp (proto is %s)", a.Proto)
	}
	a.Ports = ports
	return a, nil
}

// parsePorts は ports（数か "lo-hi" の一覧）を並べて重なり・隣り合いを合わせる。
func parsePorts(v any) ([]PortRange, error) {
	raw := asList(v)
	var out []PortRange
	for i, item := range raw {
		var s string
		switch x := item.(type) {
		case string:
			s = strings.TrimSpace(x)
		case int:
			s = strconv.Itoa(x)
		case int64:
			s = strconv.FormatInt(x, 10)
		case float64:
			if x != float64(int(x)) {
				return nil, fmt.Errorf("ports[%d]: %v is not a port", i, x)
			}
			s = strconv.Itoa(int(x))
		default:
			return nil, fmt.Errorf("ports[%d]: want a port (443) or a range (\"8000-8100\")", i)
		}
		lo, hi, isRange := strings.Cut(s, "-")
		r := PortRange{}
		var err1, err2 error
		r.Lo, err1 = strconv.Atoi(strings.TrimSpace(lo))
		r.Hi = r.Lo
		if isRange {
			r.Hi, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || r.Lo < 1 || r.Hi > 65535 || r.Lo > r.Hi {
			return nil, fmt.Errorf("ports[%d] %q: want a port 1-65535 or a range lo-hi with lo <= hi", i, s)
		}
		out = append(out, r)
	}
	return mergePorts(out), nil
}

func mergePorts(in []PortRange) []PortRange {
	if len(in) == 0 {
		return nil
	}
	rs := append([]PortRange(nil), in...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].Lo < rs[j].Lo })
	out := []PortRange{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Lo <= last.Hi+1 {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

var targetKeys = map[string]bool{"name": true, "realizer": true, "kind": true, "networks": true, "id": true, "scope": true, "before": true, "after": true, "settings": true}

func parseTarget(raw any, ref string) (Target, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Target{}, fmt.Errorf("オブジェクトでなければなりません")
	}
	for k := range m {
		if !targetKeys[k] {
			return Target{}, fmt.Errorf("unknown key %q", k)
		}
	}
	t := Target{Name: str(m, "name"), Realizer: str(m, "realizer"), Kind: str(m, "kind"), ID: str(m, "id"), Ref: ref}
	if !nameRe.MatchString(t.Name) {
		return Target{}, fmt.Errorf("name %q: want letters, digits, '_', '.', '-'", t.Name)
	}
	if t.Realizer == "" {
		t.Realizer = DefaultRealizer
	}
	if t.Kind != KindBoundary && t.Kind != KindHost {
		return Target{}, fmt.Errorf("target %s: kind %q: want boundary or host", t.Name, t.Kind)
	}
	var err error
	if t.Networks, err = strList(m["networks"], "networks"); err != nil {
		return Target{}, err
	}
	if t.Before, err = strList(m["before"], "before"); err != nil {
		return Target{}, err
	}
	if t.After, err = strList(m["after"], "after"); err != nil {
		return Target{}, err
	}
	t.Scope = scalarMap(m["scope"])
	t.Settings = scalarMap(m["settings"])
	return t, nil
}

func scalarMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		switch x := val.(type) {
		case string:
			out[k] = x
		case nil:
			out[k] = ""
		default:
			out[k] = fmt.Sprint(x)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 検査
// ─────────────────────────────────────────────────────────────────────────────

// 検査の結果の重さ。
const (
	SeverityError   = "error"   // 読み込みで断る
	SeverityWarning = "warning" // 知らせる（plan・reconcile）
)

// 検査の結果の種類。
const (
	FindingOverlap                = "overlap"                  // 網の CIDR の重なり
	FindingUndeclaredNetwork      = "undeclared_network"       // 宣言に無い網を指す
	FindingExceptionWithoutReason = "exception_without_reason" // 理由の無い例外
	FindingDuplicate              = "duplicate"                // 同じ許可の重複
	FindingShadowed               = "shadowed"                 // 前の許可に覆われて効かない
	FindingUnreached              = "unreached"                // どの実現先も通らない
	FindingDuplicateName          = "duplicate_name"           // 網・実現先の名前の二度書き
	FindingTarget                 = "target"                   // 実現先の宣言の誤り
)

// Finding は検査の結果の 1 つ。許可の行に関わるものはその行の reason と tags を
// 添える。
type Finding struct {
	Severity string
	Kind     string
	// Allow は関わる許可（Index・Ref・Reason・Tags）。許可に関わらなければ nil。
	Allow *Allow
	// Other は重複・隠れた許可のときの、前の（覆う）許可。
	Other   *Allow
	Message string
}

// String は検査の結果の 1 行（"error: duplicate: <ref> (office → dmz tcp 443):
// ... — reason: "..."; tags: exception"）。
func (f Finding) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s: ", f.Severity, f.Kind)
	if f.Allow != nil {
		fmt.Fprintf(&b, "%s (%s): ", f.Allow.Ref, f.Allow.Summary())
	}
	b.WriteString(f.Message)
	if f.Allow != nil {
		b.WriteString(allowNote(*f.Allow))
	}
	return b.String()
}

func allowNote(a Allow) string {
	reason := a.Reason
	if reason == "" {
		reason = "(none)"
	} else {
		reason = strconv.Quote(reason)
	}
	s := " — reason: " + reason
	if len(a.Tags) > 0 {
		s += "; tags: " + strings.Join(a.Tags, ",")
	}
	return s
}

// Check は宣言を確かめる。error の結果があれば読み込みで断る（Compile）。
func Check(p Policy) []Finding {
	var out []Finding
	errorf := func(kind string, a *Allow, format string, args ...any) {
		out = append(out, Finding{Severity: SeverityError, Kind: kind, Allow: a, Message: fmt.Sprintf(format, args...)})
	}

	// 網: 名前の二度書きと CIDR の重なり（網の中も網の間も）。重なりは「その
	// アドレスはどちらの網か」が決まらず、片方の網への許可がもう片方にも
	// 漏れるので断る。
	names := map[string]string{}
	for _, n := range p.Networks {
		if prev, dup := names[n.Name]; dup {
			errorf(FindingDuplicateName, nil, "network %s is declared at both %s and %s", n.Name, prev, n.Ref)
			continue
		}
		names[n.Name] = n.Ref
	}
	type owned struct {
		pfx netip.Prefix
		net string
	}
	var all []owned
	for _, n := range p.Networks {
		for _, c := range n.CIDRs {
			for _, o := range all {
				if o.pfx.Overlaps(c) {
					if o.net == n.Name {
						errorf(FindingOverlap, nil, "network %s: cidrs %s and %s overlap", n.Name, o.pfx, c)
					} else {
						errorf(FindingOverlap, nil, "networks %s (%s) and %s (%s) overlap: an address must belong to one network (an allow for one would also let the other through)", o.net, o.pfx, n.Name, c)
					}
				}
			}
			all = append(all, owned{c, n.Name})
		}
	}

	// 許可: 宣言に無い網・理由の無い例外・重複・隠れた許可。
	for i := range p.Allows {
		a := &p.Allows[i]
		for _, side := range []string{a.From, a.To} {
			if _, ok := names[side]; !ok {
				errorf(FindingUndeclaredNetwork, a, "network %s is not declared in network_policy.networks", side)
			}
		}
		if a.HasTag(TagException) && a.Reason == "" {
			errorf(FindingExceptionWithoutReason, a, "an allow tagged %s must say why (reason): an exception is declared with its reason", TagException)
		}
		for j := 0; j < i; j++ {
			b := &p.Allows[j]
			if !covers(*b, *a) {
				continue
			}
			if covers(*a, *b) {
				out = append(out, Finding{Severity: SeverityError, Kind: FindingDuplicate, Allow: a, Other: b,
					Message: fmt.Sprintf("the same allow as %s (%s); declare it once", b.Ref, strings.TrimPrefix(allowNote(*b), " — "))})
			} else {
				out = append(out, Finding{Severity: SeverityWarning, Kind: FindingShadowed, Allow: a, Other: b,
					Message: fmt.Sprintf("has no effect: %s (%s) already allows all of it (%s)", b.Ref, b.Summary(), strings.TrimPrefix(allowNote(*b), " — "))})
			}
			break
		}
	}

	// 実現先: 名前の二度書き・宣言に無い網・網の数。
	tnames := map[string]string{}
	for _, t := range p.Targets {
		if prev, dup := tnames[t.Name]; dup {
			errorf(FindingDuplicateName, nil, "target %s is declared at both %s and %s", t.Name, prev, t.Ref)
			continue
		}
		tnames[t.Name] = t.Ref
		seen := map[string]bool{}
		for _, n := range t.Networks {
			if _, ok := names[n]; !ok {
				errorf(FindingUndeclaredNetwork, nil, "%s: target %s names network %s, which is not declared in network_policy.networks", t.Ref, t.Name, n)
			}
			if seen[n] {
				errorf(FindingTarget, nil, "%s: target %s names network %s twice", t.Ref, t.Name, n)
			}
			seen[n] = true
		}
		switch {
		case t.Kind == KindBoundary && len(t.Networks) < 2:
			errorf(FindingTarget, nil, "%s: target %s is a boundary between %d network(s); a boundary is between two or more networks", t.Ref, t.Name, len(t.Networks))
		case t.Kind == KindHost && len(t.Networks) == 0:
			errorf(FindingTarget, nil, "%s: target %s is a host in no network (networks is required)", t.Ref, t.Name)
		}
	}

	// どの実現先も通らない許可は、宣言しても何も通さない（warning）。
	if len(p.Targets) > 0 {
		for i := range p.Allows {
			a := &p.Allows[i]
			reached := false
			for _, t := range p.Targets {
				if Passes(t, *a) {
					reached = true
					break
				}
			}
			if !reached {
				out = append(out, Finding{Severity: SeverityWarning, Kind: FindingUnreached, Allow: a,
					Message: "no target passes this allow (no boundary between its networks, no host in its destination)"})
			}
		}
	}
	return out
}

// covers は許可 b が許可 a のすべてを許すか（網の重なりは断るので、網は名前で
// 比べてよい）。
func covers(b, a Allow) bool {
	if b.From != a.From || b.To != a.To {
		return false
	}
	if b.Proto != ProtoAny && b.Proto != a.Proto {
		return false
	}
	if b.Proto == ProtoAny && a.Proto != ProtoAny {
		return true
	}
	if len(b.Ports) == 0 {
		return true
	}
	if len(a.Ports) == 0 {
		return false
	}
	for _, ar := range a.Ports {
		in := false
		for _, br := range b.Ports {
			if br.Lo <= ar.Lo && ar.Hi <= br.Hi {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return true
}

// Passes は実現先 t を許可 a が通るか: 境界なら from と to の両方がその網で
// 違う網、ホストなら to がその網。
func Passes(t Target, a Allow) bool {
	has := func(n string) bool {
		for _, x := range t.Networks {
			if x == n {
				return true
			}
		}
		return false
	}
	switch t.Kind {
	case KindBoundary:
		return a.From != a.To && has(a.From) && has(a.To)
	case KindHost:
		return has(a.To)
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// 展開
// ─────────────────────────────────────────────────────────────────────────────

// Passing は実現先を通る 1 つの許可と、その from・to の網。
type Passing struct {
	Allow Allow
	From  Network
	To    Network
}

// Resource は realizer が作る 1 つの resource の宣言（intent の resources: の
// 1 つと同じ形）。
type Resource struct {
	Provider  string
	Scope     map[string]string
	ID        string
	Fields    map[string]string
	DependsOn []string
}

// Rule は許可が実現先で何になったか（plan に出す）。Allow は許可の Index で、
// before・after の規則は -1。
type Rule struct {
	Allow    int
	Resource string
	Text     string
}

// Output は 1 つの実現先の展開の結果。
type Output struct {
	Resources []Resource
	Rules     []Rule
}

// Realizer は許可を 1 つの実現先の宣言に変換する。allows はその実現先を通る
// 許可だけで、宣言の順。同じ入力には同じ出力を返す（決定的、§9.1 フェーズ 2）。
type Realizer interface {
	Realize(t Target, allows []Passing) (Output, error)
}

// RealizerFunc は関数を Realizer にする。
type RealizerFunc func(t Target, allows []Passing) (Output, error)

// Realize calls f.
func (f RealizerFunc) Realize(t Target, allows []Passing) (Output, error) { return f(t, allows) }

var (
	regMu     sync.RWMutex
	realizers = map[string]Realizer{DefaultRealizer: NFTables{}}
)

// Register は realizer を名前で足す（pkg/cli の WithRealizer が使う）。すでに
// ある名前（本体の nftables を含む）は断る（上書きしない）。
func Register(name string, r Realizer) error {
	if !nameRe.MatchString(name) || r == nil {
		return fmt.Errorf("netpolicy: realizer %q: want a name of letters, digits, '_', '.', '-' and a non-nil realizer", name)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := realizers[name]; dup {
		return fmt.Errorf("netpolicy: realizer %q is registered twice (an added realizer must not reuse a registered name)", name)
	}
	realizers[name] = r
	return nil
}

// Lookup は名前の realizer を返す。
func Lookup(name string) (Realizer, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	r, ok := realizers[name]
	return r, ok
}

// Realization は 1 つの実現先の展開。
type Realization struct {
	Target Target
	Allows []Passing
	Output Output
}

// Result は検査と展開の結果。
type Result struct {
	Policy Policy
	// Findings は warning の結果（error があれば Compile は断る）。
	Findings     []Finding
	Realizations []Realization
}

// Resources は全部の実現先の resource を、実現先の宣言の順に並べたもの。
func (r *Result) Resources() []Resource {
	if r == nil {
		return nil
	}
	var out []Resource
	for _, z := range r.Realizations {
		out = append(out, z.Output.Resources...)
	}
	return out
}

// Compile は宣言を確かめ（error があれば全部を並べて断る）、実現先ごとに
// そこを通る許可を realizer に渡して展開する。
func Compile(p Policy) (*Result, error) {
	findings := Check(p)
	var errs, warns []Finding
	for _, f := range findings {
		if f.Severity == SeverityError {
			errs = append(errs, f)
		} else {
			warns = append(warns, f)
		}
	}
	if len(errs) > 0 {
		lines := make([]string, len(errs))
		for i, f := range errs {
			lines[i] = f.String()
		}
		return nil, fmt.Errorf("network_policy: %d problem(s):\n  - %s", len(errs), strings.Join(lines, "\n  - "))
	}
	res := &Result{Policy: p, Findings: warns}
	for _, t := range p.Targets {
		r, ok := Lookup(t.Realizer)
		if !ok {
			return nil, fmt.Errorf("network_policy: %s: target %s: unknown realizer %q (the built-in one is %s; add others with cli.WithRealizer)", t.Ref, t.Name, t.Realizer, DefaultRealizer)
		}
		var passing []Passing
		for _, a := range p.Allows {
			if !Passes(t, a) {
				continue
			}
			from, _ := p.Network(a.From)
			to, _ := p.Network(a.To)
			passing = append(passing, Passing{Allow: a, From: from, To: to})
		}
		outp, err := r.Realize(t, passing)
		if err != nil {
			return nil, fmt.Errorf("network_policy: %s: target %s (%s): %w", t.Ref, t.Name, t.Realizer, err)
		}
		for _, rs := range outp.Resources {
			if rs.Provider == "" || rs.ID == "" {
				return nil, fmt.Errorf("network_policy: target %s (%s): the realizer returned a resource without provider or id", t.Name, t.Realizer)
			}
		}
		res.Realizations = append(res.Realizations, Realization{Target: t, Allows: passing, Output: outp})
	}
	return res, nil
}
