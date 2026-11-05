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
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// TagException は「弱い方式をその機器だけ例外として許す」行に付ける tag。この tag の
// 付いた許可は reason が要る（無ければ読み込み時にエラーにする）。
const TagException = "exception"

// 実現先の kind。
const (
	// KindBoundary はネットワーク間の境界（境界の FW）。from も to もその実現先の
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

// DefaultDomain は domain を書かないネットワークの domain（アドレス空間、#195）。
const DefaultDomain = "default"

// interface を通る向き（Via.Direction）。
const (
	// DirectionIn はその interface から実現先に入る通信（境界なら from のネットワークの
	// interface、ホストなら to のネットワークの interface）。
	DirectionIn = "in"
	// DirectionOut はその interface から実現先を出る通信（境界なら to のネットワークの
	// interface、ホストなら from のネットワークの interface）。
	DirectionOut = "out"
)

// Network は名前の付いたネットワーク（CIDR の一覧）。
type Network struct {
	Name string
	// Domain はアドレス空間（書かなければ DefaultDomain）。重なりは同じ
	// domain の中でだけエラーにする。
	Domain string
	CIDRs  []netip.Prefix
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

// Allow はネットワークからネットワークへの 1 つの許可。
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

// HasTag は許可に tag があるか。
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
	// Provider は実現先の provider のインスタンス名（接続設定の providers での
	// 名前）。ID は実現先の宣言の id（形は realizer が決める）。
	Provider string
	ID       string
	// Labels と Apply は作る宣言にそのまま付ける（Policy の適用先・two-phase Apply、
	// E-B3）。
	Labels map[string]any
	Apply  map[string]any
	// Before と After は許可の前と後に置く規則（realizer の言葉で書く。
	// nftables なら `ct state established,related accept`・`jump mgmt`）。
	Before []string
	After  []string
	// Settings は realizer に固有の値（nftables なら hook・priority・policy・
	// confirm）。
	Settings map[string]string
	// Interfaces は interface ごとにどの domain のネットワークを見るか（#195）。空なら
	// 既定の domain のネットワークだけを見る（interface を区別しない実現先）。Compile は
	// 各 interface の Networks を埋めてから realizer に渡す。
	Interfaces []Interface
	Ref        string
}

// Interface は実現先の 1 つの interface と、そこで見る domain のネットワーク。
type Interface struct {
	// Name は interface の名前（realizer での呼び方。proxmox の net0・nftables の eth0 など）。
	Name   string
	Domain string
	// Networks はその interface の先にある実現先のネットワーク（境界）・その interface に
	// つながる実現先のネットワーク（ホスト）。書かなければ実現先の networks のうち
	// その domain のもの全部。
	Networks []string
}

// Interface は名前で interface を返す。
func (t Target) Interface(name string) (Interface, bool) {
	for _, i := range t.Interfaces {
		if i.Name == name {
			return i, true
		}
	}
	return Interface{}, false
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

// Network は名前でネットワークを返す。
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
// （値の形・proto と ports の組・同じ tag の二度書き）はここでエラーにし、ファイルを
// またぐ検査は Check が行う。
func Parse(v any, file string) (Policy, error) {
	if v == nil {
		return Policy{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Policy{}, fmt.Errorf("network_policy: マップを書く")
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
			return nil, fmt.Errorf("%s[%d]: 文字列を書く", what, i)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

func parseNetwork(raw any, ref string) (Network, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Network{}, fmt.Errorf("マップを書く")
	}
	n := Network{Name: str(m, "name"), Domain: str(m, "domain"), Ref: ref}
	if !nameRe.MatchString(n.Name) {
		return Network{}, fmt.Errorf("name %q: 英数字と _ . - で書く（英数字で始める）", n.Name)
	}
	if n.Domain == "" {
		n.Domain = DefaultDomain
	} else if !nameRe.MatchString(n.Domain) {
		return Network{}, fmt.Errorf("ネットワーク %s: domain %q: 英数字と _ . - で書く", n.Name, n.Domain)
	}
	cidrs, err := strList(m["cidrs"], "cidrs")
	if err != nil {
		return Network{}, err
	}
	if len(cidrs) == 0 {
		return Network{}, fmt.Errorf("ネットワーク %s: cidrs を書く（CIDR を 1 つ以上）", n.Name)
	}
	for _, c := range cidrs {
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			// アドレスだけなら 1 つのアドレスのネットワーク（/32・/128）として受ける。
			addr, aerr := netip.ParseAddr(c)
			if aerr != nil || addr.Zone() != "" {
				return Network{}, fmt.Errorf("ネットワーク %s: cidr %q: 10.0.0.0/24 のような CIDR かアドレスを書く", n.Name, c)
			}
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		}
		if pfx.Addr().Is4In6() {
			return Network{}, fmt.Errorf("ネットワーク %s: cidr %q: IPv4-mapped IPv6 アドレスは受けない（IPv4 の形で書く）", n.Name, c)
		}
		if pfx.Masked() != pfx {
			return Network{}, fmt.Errorf("ネットワーク %s: cidr %q のホスト部が 0 でない（%s のこと？）", n.Name, c, pfx.Masked())
		}
		n.CIDRs = append(n.CIDRs, pfx)
	}
	return n, nil
}

func parseAllow(raw any, ref string) (Allow, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Allow{}, fmt.Errorf("マップを書く")
	}
	a := Allow{From: str(m, "from"), To: str(m, "to"), Proto: str(m, "proto"), Ref: ref}
	if a.From == "" || a.To == "" {
		return Allow{}, fmt.Errorf("from と to（ネットワークの名前）を書く")
	}
	if a.Proto == "" {
		a.Proto = ProtoAny
	}
	switch a.Proto {
	case ProtoAny, ProtoTCP, ProtoUDP, ProtoICMP, ProtoICMPv6:
	default:
		return Allow{}, fmt.Errorf("proto %q: tcp・udp・icmp・icmpv6・any のどれか", a.Proto)
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
			return Allow{}, fmt.Errorf("tag %q: 英数字と _ . - で書く", t)
		}
		if seen[t] {
			return Allow{}, fmt.Errorf("tag %q が 2 回ある", t)
		}
		seen[t] = true
		a.Tags = append(a.Tags, t)
	}
	ports, err := parsePorts(m["ports"])
	if err != nil {
		return Allow{}, err
	}
	if len(ports) > 0 && a.Proto != ProtoTCP && a.Proto != ProtoUDP {
		return Allow{}, fmt.Errorf("ports は proto tcp か udp のときだけ（proto は %s）", a.Proto)
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
				return nil, fmt.Errorf("ports[%d]: %v はポートでない", i, x)
			}
			s = strconv.Itoa(int(x))
		default:
			return nil, fmt.Errorf("ports[%d]: ポート（443）か範囲（\"8000-8100\"）を書く", i)
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
			return nil, fmt.Errorf("ports[%d] %q: 1-65535 のポートか、下 <= 上 の範囲を書く", i, s)
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

var targetKeys = map[string]bool{"name": true, "realizer": true, "kind": true, "networks": true, "interfaces": true, "provider": true, "id": true,
	"labels": true, "apply": true, "before": true, "after": true, "settings": true}

var interfaceKeys = map[string]bool{"name": true, "domain": true, "networks": true}

func parseTarget(raw any, ref string) (Target, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return Target{}, fmt.Errorf("マップを書く")
	}
	for k := range m {
		if !targetKeys[k] {
			return Target{}, fmt.Errorf("不明な field %q", k)
		}
	}
	t := Target{Name: str(m, "name"), Realizer: str(m, "realizer"), Kind: str(m, "kind"), Provider: str(m, "provider"), ID: str(m, "id"), Ref: ref}
	t.Labels, _ = m["labels"].(map[string]any)
	t.Apply, _ = m["apply"].(map[string]any)
	if t.Provider == "" || t.ID == "" {
		return Target{}, fmt.Errorf("実現先 %s: provider（実現先の provider の名前）と id を書く", t.Name)
	}
	if !nameRe.MatchString(t.Name) {
		return Target{}, fmt.Errorf("name %q: 英数字と _ . - で書く", t.Name)
	}
	if t.Realizer == "" {
		t.Realizer = DefaultRealizer
	}
	if t.Kind != KindBoundary && t.Kind != KindHost {
		return Target{}, fmt.Errorf("実現先 %s: kind %q: boundary か host", t.Name, t.Kind)
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
	t.Settings = scalarMap(m["settings"])
	for i, raw := range asList(m["interfaces"]) {
		im, ok := raw.(map[string]any)
		if !ok {
			return Target{}, fmt.Errorf("実現先 %s: interfaces[%d]: マップを書く", t.Name, i)
		}
		for k := range im {
			if !interfaceKeys[k] {
				return Target{}, fmt.Errorf("実現先 %s: interfaces[%d]: 不明な field %q", t.Name, i, k)
			}
		}
		itf := Interface{Name: str(im, "name"), Domain: str(im, "domain")}
		if !nameRe.MatchString(itf.Name) {
			return Target{}, fmt.Errorf("実現先 %s: interfaces[%d]: name %q: 英数字と _ . - で書く", t.Name, i, itf.Name)
		}
		// interface の domain は省かない（既定の domain の interface も domain: default と書く）。
		if !nameRe.MatchString(itf.Domain) {
			return Target{}, fmt.Errorf("実現先 %s: interface %s: domain %q: 英数字と _ . - で書く", t.Name, itf.Name, itf.Domain)
		}
		if itf.Networks, err = strList(im["networks"], "networks"); err != nil {
			return Target{}, fmt.Errorf("実現先 %s: interface %s: %w", t.Name, itf.Name, err)
		}
		t.Interfaces = append(t.Interfaces, itf)
	}
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
	SeverityError   = "error"   // 読み込み時にエラーにする（reject）
	SeverityWarning = "warning" // 通知する（plan・reconcile）
)

// 検査の結果の kind。
const (
	FindingOverlap                = "overlap"                  // ネットワークの CIDR の重なり
	FindingUndeclaredNetwork      = "undeclared_network"       // 宣言に無いネットワークを指す
	FindingExceptionWithoutReason = "exception_without_reason" // 理由の無い例外
	FindingDuplicate              = "duplicate"                // 同じ許可の重複
	FindingShadowed               = "shadowed"                 // 前の許可に覆われて効かない
	FindingUnreached              = "unreached"                // どの実現先も通らない
	FindingDuplicateName          = "duplicate_name"           // ネットワーク・実現先の名前の二度書き
	FindingTarget                 = "target"                   // 実現先の宣言の誤り
	FindingCrossDomain            = "cross_domain"             // domain をまたぐ許可
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

// String は検査の結果の 1 行（"duplicate: <ref> (office → dmz tcp 443):
// ...（理由: "..."、tags: exception）"）。
func (f Finding) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: ", f.Kind)
	if f.Allow != nil {
		fmt.Fprintf(&b, "%s (%s): ", f.Allow.Ref, f.Allow.Summary())
	}
	b.WriteString(f.Message)
	if f.Allow != nil {
		b.WriteString(allowNote(*f.Allow))
	}
	return b.String()
}

func allowNote(a Allow) string { return "（" + note(a) + "）" }

// note は許可の理由と tag（"理由: \"…\"、tags: exception"）。
func note(a Allow) string {
	reason := a.Reason
	if reason == "" {
		reason = "なし"
	} else {
		reason = strconv.Quote(reason)
	}
	s := "理由: " + reason
	if len(a.Tags) > 0 {
		s += "、tags: " + strings.Join(a.Tags, ",")
	}
	return s
}

// Check は宣言を確かめる。error の結果があれば読み込み時にエラーにする（Compile）。
func Check(p Policy) []Finding {
	var out []Finding
	errorf := func(kind string, a *Allow, format string, args ...any) {
		out = append(out, Finding{Severity: SeverityError, Kind: kind, Allow: a, Message: fmt.Sprintf(format, args...)})
	}

	// ネットワーク: 名前の二度書きと CIDR の重なり（ネットワークの中も、同じ domain の
	// ネットワーク間も）。重なると「そのアドレスはどちらのネットワークか」が決まらず、
	// 片方への許可がもう片方にも漏れるのでエラーにする。違う domain は別のアドレス空間
	// なので、重なってよい。ネットワークの名前は domain をまたいで一意（許可は名前で指す）。
	names := map[string]string{}
	domains := map[string]string{} // ネットワークの名前 → domain
	for _, n := range p.Networks {
		if prev, dup := names[n.Name]; dup {
			errorf(FindingDuplicateName, nil, "ネットワーク %s が %s と %s の 2 か所にある", n.Name, prev, n.Ref)
			continue
		}
		names[n.Name] = n.Ref
		domains[n.Name] = n.Domain
	}
	type owned struct {
		pfx    netip.Prefix
		net    string
		domain string
	}
	var all []owned
	for _, n := range p.Networks {
		for _, c := range n.CIDRs {
			for _, o := range all {
				if o.domain == n.Domain && o.pfx.Overlaps(c) {
					if o.net == n.Name {
						errorf(FindingOverlap, nil, "ネットワーク %s: cidrs %s と %s が重なる", n.Name, o.pfx, c)
					} else {
						errorf(FindingOverlap, nil, "ネットワーク %s（%s）と %s（%s）が domain %s の中で重なる。アドレスは 1 つのネットワークに属する（片方への許可がもう片方にも漏れる。別のアドレス空間なら domain を分ける）", o.net, o.pfx, n.Name, c, n.Domain)
					}
				}
			}
			all = append(all, owned{c, n.Name, n.Domain})
		}
	}

	// 許可: 宣言に無いネットワーク・理由の無い例外・重複・隠れた許可。
	for i := range p.Allows {
		a := &p.Allows[i]
		for _, side := range []string{a.From, a.To} {
			if _, ok := names[side]; !ok {
				errorf(FindingUndeclaredNetwork, a, "ネットワーク %s が network_policy.networks に無い", side)
			}
		}
		if df, okf := domains[a.From]; okf {
			if dt, okt := domains[a.To]; okt && df != dt {
				errorf(FindingCrossDomain, a, "from %s は domain %s、to %s は domain %s。許可は 1 つの domain の中で書く（2 つの domain が接するところでは片側ずつ許可を書く）", a.From, df, a.To, dt)
			}
		}
		if a.HasTag(TagException) && a.Reason == "" {
			errorf(FindingExceptionWithoutReason, a, "tag %s の許可には reason を書く。例外は理由と一緒に宣言する", TagException)
		}
		for j := 0; j < i; j++ {
			b := &p.Allows[j]
			if !covers(*b, *a) {
				continue
			}
			if covers(*a, *b) {
				out = append(out, Finding{Severity: SeverityError, Kind: FindingDuplicate, Allow: a, Other: b,
					Message: fmt.Sprintf("%s（%s）と同じ許可。1 回だけ書く", b.Ref, note(*b))})
			} else {
				out = append(out, Finding{Severity: SeverityWarning, Kind: FindingShadowed, Allow: a, Other: b,
					Message: fmt.Sprintf("効かない。%s（%s）がすでに全部を許している（%s）", b.Ref, b.Summary(), note(*b))})
			}
			break
		}
	}

	// 実現先: 名前の二度書き・宣言に無いネットワーク・ネットワークの数。
	tnames := map[string]string{}
	for _, t := range p.Targets {
		if prev, dup := tnames[t.Name]; dup {
			errorf(FindingDuplicateName, nil, "実現先 %s が %s と %s の 2 か所にある", t.Name, prev, t.Ref)
			continue
		}
		tnames[t.Name] = t.Ref
		seen := map[string]bool{}
		for _, n := range t.Networks {
			if _, ok := names[n]; !ok {
				errorf(FindingUndeclaredNetwork, nil, "%s: 実現先 %s のネットワーク %s が network_policy.networks に無い", t.Ref, t.Name, n)
			}
			if seen[n] {
				errorf(FindingTarget, nil, "%s: 実現先 %s にネットワーク %s が 2 回ある", t.Ref, t.Name, n)
			}
			seen[n] = true
		}
		checkInterfaces(t, domains, errorf)
		switch {
		case t.Kind == KindBoundary && len(t.Networks) < 2:
			errorf(FindingTarget, nil, "%s: 実現先 %s の networks が %d 個。境界は 2 つ以上のネットワークの間に置く", t.Ref, t.Name, len(t.Networks))
		case t.Kind == KindHost && len(t.Networks) == 0:
			errorf(FindingTarget, nil, "%s: 実現先 %s はどのネットワークにも属さないホスト（networks を書く）", t.Ref, t.Name)
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
					Message: "この許可を通す実現先が無い（そのネットワーク間の境界も、宛先のネットワークのホストも無い）"})
			}
		}
	}
	return out
}

// checkInterfaces は実現先の interface を確かめる: interface の名前の二度書き・宣言に
// 無い domain・実現先に無いネットワーク・interface の domain に無いネットワーク・
// 2 つの interface に載るネットワーク・どの interface にも載らないネットワーク・何も
// 見ない interface。interfaces を書かない実現先は既定の domain のネットワークだけを見る。
func checkInterfaces(t Target, domains map[string]string, errorf func(string, *Allow, string, ...any)) {
	if len(t.Interfaces) == 0 {
		for _, n := range t.Networks {
			if d, ok := domains[n]; ok && d != DefaultDomain {
				errorf(FindingTarget, nil, "%s: 実現先 %s は domain %s のネットワーク %s を書いたが interfaces が無い。interfaces を書かない実現先は domain %s だけを見る（どの interface がどの domain を見るかを interfaces に書く）", t.Ref, t.Name, n, d, DefaultDomain)
			}
		}
		return
	}
	known := map[string]bool{}
	for _, d := range domains {
		known[d] = true
	}
	inTarget := map[string]bool{}
	for _, n := range t.Networks {
		inTarget[n] = true
	}
	inames := map[string]bool{}
	on := map[string]string{} // ネットワーク → 載る interface
	for _, itf := range t.Interfaces {
		if inames[itf.Name] {
			errorf(FindingTarget, nil, "%s: 実現先 %s に interface %s が 2 回ある", t.Ref, t.Name, itf.Name)
			continue
		}
		inames[itf.Name] = true
		if !known[itf.Domain] {
			errorf(FindingTarget, nil, "%s: 実現先 %s: interface %s の domain %s にネットワークが無い", t.Ref, t.Name, itf.Name, itf.Domain)
			continue
		}
		for _, n := range resolveInterface(t, itf, domains).Networks {
			d, declared := domains[n]
			switch {
			case !inTarget[n]:
				errorf(FindingTarget, nil, "%s: 実現先 %s: interface %s のネットワーク %s が実現先の networks に無い", t.Ref, t.Name, itf.Name, n)
			case declared && d != itf.Domain:
				errorf(FindingTarget, nil, "%s: 実現先 %s: interface %s は domain %s を見るが、domain %s のネットワーク %s を書いた", t.Ref, t.Name, itf.Name, itf.Domain, n, d)
			case on[n] != "":
				errorf(FindingTarget, nil, "%s: 実現先 %s: ネットワーク %s が interface %s と %s の両方にある（ネットワークは 1 つの interface の先にだけ置く）", t.Ref, t.Name, n, on[n], itf.Name)
			default:
				on[n] = itf.Name
			}
		}
	}
	for _, n := range t.Networks {
		if _, declared := domains[n]; declared && on[n] == "" {
			errorf(FindingTarget, nil, "%s: 実現先 %s: ネットワーク %s（domain %s）がどの interface にも無い", t.Ref, t.Name, n, domains[n])
		}
	}
}

// resolveInterface は interface の networks を書かないとき、実現先の networks のうち
// interface の domain のもので埋める。
func resolveInterface(t Target, itf Interface, domains map[string]string) Interface {
	if len(itf.Networks) > 0 {
		return itf
	}
	for _, n := range t.Networks {
		if domains[n] == itf.Domain {
			itf.Networks = append(itf.Networks, n)
		}
	}
	return itf
}

// covers は許可 b が許可 a のすべてを許すか（ネットワークの名前は domain をまたいで
// 一意で、同じ domain のネットワークの重なりはエラーにするので、名前で比べてよい）。
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

// Passes は実現先 t を許可 a が通るか: 境界なら from と to の両方がその実現先の
// ネットワークで、互いに違うネットワーク。ホストなら to がその実現先のネットワーク
// （interfaces を書いたホストは from がそのネットワークでもよい。出る向きの許可）。
// domain をまたぐ許可と、interfaces を書かない実現先での既定の domain の外の
// ネットワークは Check がエラーにするので、ここでは名前だけで見る。
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
		return has(a.To) || (len(t.Interfaces) > 0 && has(a.From))
	}
	return false
}

// via は許可 a が実現先 t のどの interface をどの向きに通るか（interfaces を書かない
// 実現先は空）。interface の Networks は埋めてあること（resolveInterface）。
func via(t Target, a Allow) []Via {
	var out []Via
	for _, itf := range t.Interfaces {
		has := func(n string) bool {
			for _, x := range itf.Networks {
				if x == n {
					return true
				}
			}
			return false
		}
		var in, outb bool
		switch t.Kind {
		case KindBoundary:
			in, outb = has(a.From), has(a.To)
		case KindHost:
			in, outb = has(a.To), has(a.From)
		}
		if in {
			out = append(out, Via{Interface: itf.Name, Domain: itf.Domain, Direction: DirectionIn})
		}
		if outb {
			out = append(out, Via{Interface: itf.Name, Domain: itf.Domain, Direction: DirectionOut})
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 展開
// ─────────────────────────────────────────────────────────────────────────────

// Passing は実現先を通る 1 つの許可と、その from・to のネットワーク。
type Passing struct {
	Allow Allow
	From  Network
	To    Network
	// Domain は許可の domain（from と to のネットワークの domain、#195）。
	Domain string
	// Via はその許可が通る interface と向き（実現先が interfaces を書いたときだけ）。
	// 境界は from のネットワークの interface で in・to のネットワークの interface で
	// out。ホストは to が自分のネットワークなら in・from が自分のネットワークなら out。
	Via []Via
}

// Via は許可が通る 1 つの interface と向き。
type Via struct {
	Interface string
	Domain    string
	// Direction は DirectionIn か DirectionOut。
	Direction string
}

// InterfaceAllows は 1 つの interface を通る許可を向きごとに分けたもの。
type InterfaceAllows struct {
	Interface Interface
	In        []Passing
	Out       []Passing
}

// ByInterface は実現先の interface ごとに、そこを通る許可を入りと出に分ける
// （interface は宣言の順、許可も宣言の順）。interfaces を書かない実現先は空。
func ByInterface(t Target, allows []Passing) []InterfaceAllows {
	out := make([]InterfaceAllows, len(t.Interfaces))
	pos := map[string]int{}
	for i, itf := range t.Interfaces {
		out[i].Interface = itf
		pos[itf.Name] = i
	}
	for _, p := range allows {
		for _, v := range p.Via {
			i, ok := pos[v.Interface]
			if !ok {
				continue
			}
			if v.Direction == DirectionIn {
				out[i].In = append(out[i].In, p)
			} else {
				out[i].Out = append(out[i].Out, p)
			}
		}
	}
	return out
}

// Resource は realizer が作る 1 つの宣言。intent の resources: の 1 つと同じ形の
// 木（type・id・fields・labels・depends_on・apply・reason）で、Expand が intent と
// 同じ読み方で Resource にする。
type Resource map[string]any

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
// 許可だけで、宣言の順。t が interfaces を書いたときは、各許可の Via に通る
// interface と向きがあり、t.Interfaces の Networks は埋めてある（interface ごとの
// 組は ByInterface）。
// 同じ入力には同じ出力を返す（決定的、§9.1 フェーズ 2）。
type Realizer interface {
	Realize(t Target, allows []Passing) (Output, error)
}

// RealizerFunc は関数を Realizer にする。
type RealizerFunc func(t Target, allows []Passing) (Output, error)

// Realize calls f.
func (f RealizerFunc) Realize(t Target, allows []Passing) (Output, error) { return f(t, allows) }

// Realization は 1 つの実現先の展開。
type Realization struct {
	Target Target
	Allows []Passing
	Output Output
}

// Result は検査と展開の結果。
type Result struct {
	Policy Policy
	// Findings は warning の結果（error があれば Compile はエラーを返す）。
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

// Compile は宣言を確かめ（error があれば全部を並べてエラーを返す）、実現先ごとに
// そこを通る許可を realizer に渡して展開する。
func Compile(p Policy, realizers map[string]Realizer) (*Result, error) {
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
		return nil, fmt.Errorf("network_policy: %d 件のエラー:\n  - %s", len(errs), strings.Join(lines, "\n  - "))
	}
	res := &Result{Policy: p, Findings: warns}
	domains := map[string]string{}
	for _, n := range p.Networks {
		domains[n.Name] = n.Domain
	}
	for _, t := range p.Targets {
		if len(t.Interfaces) > 0 {
			itfs := make([]Interface, len(t.Interfaces))
			for i, itf := range t.Interfaces {
				itfs[i] = resolveInterface(t, itf, domains)
			}
			t.Interfaces = itfs
		}
		r, ok := realizers[t.Realizer]
		if !ok {
			return nil, fmt.Errorf("%s: 実現先 %s: 不明な realizer %q（組み込みは %s。ほかは WithRealizer で足す）", t.Ref, t.Name, t.Realizer, DefaultRealizer)
		}
		var passing []Passing
		for _, a := range p.Allows {
			if !Passes(t, a) {
				continue
			}
			from, _ := p.Network(a.From)
			to, _ := p.Network(a.To)
			passing = append(passing, Passing{Allow: a, From: from, To: to, Domain: from.Domain, Via: via(t, a)})
		}
		outp, err := r.Realize(t, passing)
		if err != nil {
			return nil, fmt.Errorf("%s: 実現先 %s（%s）: %w", t.Ref, t.Name, t.Realizer, err)
		}

		res.Realizations = append(res.Realizations, Realization{Target: t, Allows: passing, Output: outp})
	}
	return res, nil
}
