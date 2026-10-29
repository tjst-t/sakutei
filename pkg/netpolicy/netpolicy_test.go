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

// 網の間の許可の読み込み・検査・展開（Backlog #174）。
//
//	[AC-174-1-1]  読み込み: 網・許可・実現先の形を読み、値の誤り（host bit・
//	              proto と ports の組・ポートの範囲・印の二度書き）を断る。
//	[AC-174-1-2]  検査: 網の重なり・宣言に無い網・理由の無い例外・同じ許可の
//	              重複は error、前の許可に覆われた許可・どこも通らない許可は
//	              warning。結果にその行の reason と tags を添える。
//	[AC-174-1-3]  展開（nftables）: 実現先ごとにそこを通る許可だけを base chain
//	              のルールにし、reason を comment に出す（長さ・文字を合わせる）。
//	[AC-174-1-4]  展開の口: 足した realizer にそこを通る許可が reason・tags 付きで
//	              渡る。同じ名前は断る。

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// tree は JSON の文字列を Parse に渡す木にする（数は float64、loader と同じ）。
func tree(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustParse(t *testing.T, s string) Policy {
	t.Helper()
	p, err := Parse(tree(t, s), "np.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

const baseNets = `"networks": [
  {"name": "office", "cidrs": ["10.1.0.0/24", "10.1.1.0/24", "2001:db8:1::/64"]},
  {"name": "dmz", "cidrs": ["192.0.2.0/24", "2001:db8:2::/64"]},
  {"name": "db", "cidrs": ["10.2.0.10"]}
]`

func TestParse_Refuses(t *testing.T) {
	// [AC-174-1-1]
	for _, c := range []struct{ in, want string }{
		{`{"networks": [{"name": "a", "cidrs": ["10.0.0.1/24"]}]}`, "host bits set (did you mean 10.0.0.0/24?)"},
		{`{"networks": [{"name": "a", "cidrs": ["10.0.0.0/33"]}]}`, "want a CIDR"},
		{`{"networks": [{"name": "a b", "cidrs": ["10.0.0.0/24"]}]}`, "name \"a b\""},
		{`{"networks": [{"name": "a", "cidrs": []}]}`, "cidrs is required"},
		{`{"allows": [{"from": "a", "to": "b", "ports": [22]}]}`, "ports need proto tcp or udp"},
		{`{"allows": [{"from": "a", "to": "b", "proto": "icmp", "ports": [22]}]}`, "ports need proto tcp or udp"},
		{`{"allows": [{"from": "a", "to": "b", "proto": "sctp"}]}`, "proto \"sctp\""},
		{`{"allows": [{"from": "a", "to": "b", "proto": "tcp", "ports": [0]}]}`, "want a port 1-65535"},
		{`{"allows": [{"from": "a", "to": "b", "proto": "tcp", "ports": ["90-80"]}]}`, "lo <= hi"},
		{`{"allows": [{"from": "a", "to": "b", "proto": "tcp", "ports": [70000]}]}`, "want a port 1-65535"},
		{`{"allows": [{"from": "a", "to": "b", "tags": ["exception", "exception"]}]}`, "written twice"},
		{`{"allows": [{"from": "a"}]}`, "from and to are required"},
		{`{"targets": [{"name": "t", "kind": "switch", "networks": ["a"], "id": "x"}]}`, "kind \"switch\""},
	} {
		_, err := Parse(tree(t, c.in), "np.json")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("[AC-174-1-1] Parse(%s): err = %v, want it to contain %q", c.in, err, c.want)
		}
	}
}

func TestParse_Ports(t *testing.T) {
	// [AC-174-1-1] ポートは並べて重なり・隣り合いを合わせる。
	p := mustParse(t, `{"allows": [{"from": "a", "to": "b", "proto": "tcp", "ports": [443, "80", "8000-8100", "8050-8200", 81]}]}`)
	got := p.Allows[0].Summary()
	if want := "a → b tcp 80-81,443,8000-8200"; got != want {
		t.Errorf("[AC-174-1-1] Summary = %q, want %q", got, want)
	}
	if p.Allows[0].Ref != "np.json: network_policy.allows[0]" {
		t.Errorf("Ref = %q", p.Allows[0].Ref)
	}
}

func findings(t *testing.T, s string) []Finding {
	t.Helper()
	return Check(mustParse(t, s))
}

func hasFinding(fs []Finding, severity, kind, contains string) bool {
	for _, f := range fs {
		if f.Severity == severity && f.Kind == kind && strings.Contains(f.String(), contains) {
			return true
		}
	}
	return false
}

func TestCheck(t *testing.T) {
	// [AC-174-1-2]
	cases := []struct {
		name, in          string
		severity, kind    string
		contains          string
		alsoContainReason string
	}{
		{"overlap between networks", `{"networks": [
			{"name": "a", "cidrs": ["10.0.0.0/16"]}, {"name": "b", "cidrs": ["10.0.5.0/24"]}]}`,
			SeverityError, FindingOverlap, "networks a (10.0.0.0/16) and b (10.0.5.0/24) overlap", ""},
		{"overlap in a network", `{"networks": [{"name": "a", "cidrs": ["10.0.0.0/24", "10.0.0.128/25"]}]}`,
			SeverityError, FindingOverlap, "network a: cidrs 10.0.0.0/24 and 10.0.0.128/25 overlap", ""},
		{"undeclared network in an allow", `{` + baseNets + `, "allows": [
			{"from": "office", "to": "lab", "reason": "検証の網へ"}]}`,
			SeverityError, FindingUndeclaredNetwork, "network lab is not declared", `reason: "検証の網へ"`},
		{"undeclared network in a target", `{` + baseNets + `, "targets": [
			{"name": "fw", "kind": "boundary", "networks": ["office", "lab"], "id": "inet/f/c"}]}`,
			SeverityError, FindingUndeclaredNetwork, "target fw names network lab", ""},
		{"exception without reason", `{` + baseNets + `, "allows": [
			{"from": "office", "to": "db", "proto": "tcp", "ports": [23], "tags": ["exception"]}]}`,
			SeverityError, FindingExceptionWithoutReason, "must say why", "tags: exception"},
		{"duplicate", `{` + baseNets + `, "allows": [
			{"from": "office", "to": "dmz", "proto": "tcp", "ports": [443, 80], "reason": "web"},
			{"from": "office", "to": "dmz", "proto": "tcp", "ports": ["80", "443"], "reason": "もう一度"}]}`,
			SeverityError, FindingDuplicate, "the same allow as np.json: network_policy.allows[0] (reason: \"web\")", `reason: "もう一度"`},
		{"shadowed by ports", `{` + baseNets + `, "allows": [
			{"from": "office", "to": "dmz", "proto": "tcp", "ports": ["1-1024"], "reason": "広い"},
			{"from": "office", "to": "dmz", "proto": "tcp", "ports": [443], "reason": "狭い", "tags": ["web"]}]}`,
			SeverityWarning, FindingShadowed, "has no effect: np.json: network_policy.allows[0] (office → dmz tcp 1-1024) already allows all of it", `reason: "狭い"; tags: web`},
		{"shadowed by any", `{` + baseNets + `, "allows": [
			{"from": "office", "to": "dmz"},
			{"from": "office", "to": "dmz", "proto": "udp", "ports": [53]}]}`,
			SeverityWarning, FindingShadowed, "(office → dmz any) already allows all of it", "reason: (none)"},
		{"unreached", `{` + baseNets + `, "allows": [
			{"from": "office", "to": "db", "reason": "誰も通さない"}], "targets": [
			{"name": "fw", "kind": "boundary", "networks": ["office", "dmz"], "id": "inet/f/c"}]}`,
			SeverityWarning, FindingUnreached, "no target passes this allow", `reason: "誰も通さない"`},
		{"boundary with one network", `{` + baseNets + `, "targets": [
			{"name": "fw", "kind": "boundary", "networks": ["office"], "id": "inet/f/c"}]}`,
			SeverityError, FindingTarget, "a boundary is between two or more networks", ""},
		{"network declared twice", `{"networks": [{"name": "a", "cidrs": ["10.0.0.0/24"]}, {"name": "a", "cidrs": ["10.0.1.0/24"]}]}`,
			SeverityError, FindingDuplicateName, "network a is declared at both", ""},
	}
	for _, c := range cases {
		fs := findings(t, c.in)
		if !hasFinding(fs, c.severity, c.kind, c.contains) {
			t.Errorf("[AC-174-1-2] %s: want %s %s containing %q, got %v", c.name, c.severity, c.kind, c.contains, fs)
			continue
		}
		if c.alsoContainReason != "" && !hasFinding(fs, c.severity, c.kind, c.alsoContainReason) {
			t.Errorf("[AC-174-1-2] %s: the finding must carry the line's %q, got %v", c.name, c.alsoContainReason, fs)
		}
	}
}

func TestCheck_Clean(t *testing.T) {
	// [AC-174-1-2] 理由の無い印の無い許可・理由のある例外・proto の違う許可は通る。
	fs := findings(t, `{`+baseNets+`, "allows": [
		{"from": "office", "to": "dmz", "proto": "tcp", "ports": [443]},
		{"from": "office", "to": "dmz", "proto": "udp", "ports": [443]},
		{"from": "office", "to": "db", "proto": "tcp", "ports": [23], "tags": ["exception"], "reason": "古い装置"}]}`)
	if len(fs) != 0 {
		t.Errorf("[AC-174-1-2] want no findings, got %v", fs)
	}
}

func TestCompile_RefusesWithAllErrors(t *testing.T) {
	// [AC-174-1-2] error は全部を並べて断る。
	p := mustParse(t, `{"networks": [{"name": "a", "cidrs": ["10.0.0.0/16"]}, {"name": "b", "cidrs": ["10.0.1.0/24"]}],
		"allows": [{"from": "a", "to": "c", "tags": ["exception"]}]}`)
	_, err := Compile(p)
	if err == nil {
		t.Fatal("[AC-174-1-2] Compile must refuse")
	}
	for _, want := range []string{"3 problem(s)", FindingOverlap, FindingUndeclaredNetwork, FindingExceptionWithoutReason} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("[AC-174-1-2] error %q must contain %q", err, want)
		}
	}
}

const policyJSON = `{` + baseNets + `,
  "allows": [
    {"from": "office", "to": "dmz", "proto": "tcp", "ports": [443, 80], "reason": "社内から公開の web"},
    {"from": "dmz", "to": "db", "proto": "tcp", "ports": [5432], "reason": "web から DB"},
    {"from": "office", "to": "db", "proto": "tcp", "ports": [23], "tags": ["exception"], "reason": "古い装置は telnet だけ"},
    {"from": "office", "to": "dmz", "proto": "icmpv6", "reason": "近隣探索"},
    {"from": "db", "to": "db", "proto": "udp", "reason": "クラスタの中"}
  ],
  "targets": [
    {"name": "fw1", "kind": "boundary", "networks": ["office", "dmz", "db"], "id": "fw1/inet/filter/forward",
     "scope": {"site": "lab"}, "before": ["ct state established,related accept"], "after": ["counter"]},
    {"name": "db1", "kind": "host", "networks": ["db"], "id": "ip/filter/input",
     "before": ["ct state established,related accept", "jump mgmt"], "settings": {"priority": "filter + 10", "confirm": "self"}}
  ]
}`

func TestCompile_NFTables(t *testing.T) {
	// [AC-174-1-3]
	res, err := Compile(mustParse(t, policyJSON))
	if err != nil {
		t.Fatal(err)
	}
	rs := res.Resources()
	if len(rs) != 2 {
		t.Fatalf("want 2 resources, got %+v", rs)
	}
	fw := rs[0]
	if fw.Provider != "nftables" || fw.ID != "fw1/inet/filter/forward" || fw.Scope["site"] != "lab" {
		t.Errorf("fw resource = %+v", fw)
	}
	wantFW := map[string]string{"family": "inet", "table": "filter", "chain": "forward", "type": "filter", "hook": "forward", "priority": "filter", "policy": "drop"}
	for k, v := range wantFW {
		if fw.Fields[k] != v {
			t.Errorf("[AC-174-1-3] fw %s = %q, want %q", k, fw.Fields[k], v)
		}
	}
	// 境界: from と to が違う網の許可だけ。db → db（同じ網）は通らない。IPv4 と
	// IPv6 は族ごとに 1 ルール、db は IPv4 だけなので dmz → db は IPv4 だけ。
	wantRules := []string{
		"ct state established,related accept",
		`ip saddr 10.1.0.0/23 ip daddr 192.0.2.0/24 tcp dport { 80, 443 } accept comment "社内から公開の web"`,
		`ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 tcp dport { 80, 443 } accept comment "社内から公開の web"`,
		`ip saddr 192.0.2.0/24 ip daddr 10.2.0.10 tcp dport 5432 accept comment "web から DB"`,
		`ip saddr 10.1.0.0/23 ip daddr 10.2.0.10 tcp dport 23 accept comment "古い装置は telnet だけ"`,
		`ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 meta l4proto ipv6-icmp accept comment "近隣探索"`,
		"counter",
	}
	if got := fw.Fields["rules"]; got != strings.Join(wantRules, " ; ") {
		t.Errorf("[AC-174-1-3] fw rules:\n got %s\nwant %s", got, strings.Join(wantRules, " ; "))
	}

	// ホスト: to がその網の許可（同じ網の中も）。family ip なので IPv4 だけ。
	db := rs[1]
	if db.Fields["hook"] != "input" || db.Fields["priority"] != "filter + 10" || db.Fields["confirm"] != "self" || db.Fields["family"] != "ip" {
		t.Errorf("[AC-174-1-3] db fields = %v", db.Fields)
	}
	wantDB := []string{
		"ct state established,related accept", "jump mgmt",
		`ip saddr 192.0.2.0/24 ip daddr 10.2.0.10 tcp dport 5432 accept comment "web から DB"`,
		`ip saddr 10.1.0.0/23 ip daddr 10.2.0.10 tcp dport 23 accept comment "古い装置は telnet だけ"`,
		`ip saddr 10.2.0.10 ip daddr 10.2.0.10 meta l4proto udp accept comment "クラスタの中"`,
	}
	if got := db.Fields["rules"]; got != strings.Join(wantDB, " ; ") {
		t.Errorf("[AC-174-1-3] db rules:\n got %s\nwant %s", got, strings.Join(wantDB, " ; "))
	}

	// 展開の結果は実現先ごとに、通る許可とそれがなったルールを持つ（plan が出す）。
	z := res.Realizations[1]
	if len(z.Allows) != 3 || z.Allows[1].Allow.Reason != "古い装置は telnet だけ" || !z.Allows[1].Allow.HasTag(TagException) {
		t.Errorf("[AC-174-1-3] db1 passing = %+v", z.Allows)
	}
	n := 0
	for _, r := range z.Output.Rules {
		if r.Allow == 2 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("[AC-174-1-3] allow 2 at db1: want 1 rule, got %d (%+v)", n, z.Output.Rules)
	}
}

func TestNFTables_Refuses(t *testing.T) {
	// [AC-174-1-3]
	for _, c := range []struct{ target, want string }{
		{`"id": "filter/forward"`, "want [<host>/]<family>/<table>/<chain>"},
		{`"id": "bridge/filter/forward"`, "family bridge"},
		{`"id": "inet/filter/forward", "settings": {"device": "eth0"}`, `unknown key "device"`},
		{`"id": "inet/filter/forward", "before": ["a ; b"]`, "one rule per entry"},
	} {
		in := `{` + baseNets + `, "targets": [{"name": "fw", "kind": "boundary", "networks": ["office", "dmz"], ` + c.target + `}]}`
		_, err := Compile(mustParse(t, in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("[AC-174-1-3] %s: err = %v, want %q", c.target, err, c.want)
		}
	}
}

func TestNFTComment(t *testing.T) {
	// [AC-174-1-3] nft の comment は 128 バイトまでで `"` を逃がせない。
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"web", "web"},
		{`say "hi"`, "say 'hi'"},
		{`a\b`, "a/b"},
		{"a ; b;c", "a , b,c"},
		{"  多い\t空白\nと改行  ", "多い 空白 と改行"},
		{"# 印", "# 印"},
	} {
		if got := NFTComment(c.in); got != c.want {
			t.Errorf("[AC-174-1-3] NFTComment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("あ", 60) // 180 バイト
	got := NFTComment(long)
	if len(got) > NFTCommentMax || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("[AC-174-1-3] NFTComment(long) = %q (%d bytes): want ≤ %d bytes, valid UTF-8, ending in …", got, len(got), NFTCommentMax)
	}
	if exact := strings.Repeat("a", NFTCommentMax); NFTComment(exact) != exact {
		t.Errorf("[AC-174-1-3] a comment of exactly %d bytes must be kept", NFTCommentMax)
	}
}

func TestRegister(t *testing.T) {
	// [AC-174-1-4]
	if err := Register(DefaultRealizer, NFTables{}); err == nil {
		t.Error("[AC-174-1-4] the built-in nftables realizer must not be overwritten")
	}
	var got []Passing
	err := Register("test-acl", RealizerFunc(func(tg Target, allows []Passing) (Output, error) {
		got = allows
		return Output{Resources: []Resource{{Provider: "acl", ID: tg.ID, Fields: map[string]string{"n": "1"}}}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := Register("test-acl", RealizerFunc(nil)); err == nil {
		t.Error("[AC-174-1-4] a realizer name registered twice must be refused")
	}
	in := `{` + baseNets + `, "allows": [
		{"from": "office", "to": "db", "proto": "tcp", "ports": [23], "tags": ["exception"], "reason": "古い装置"},
		{"from": "office", "to": "dmz", "proto": "tcp", "ports": [443]}],
		"targets": [{"name": "sw1", "realizer": "test-acl", "kind": "boundary", "networks": ["office", "db"], "id": "sw1/acl/101"}]}`
	res, err := Compile(mustParse(t, in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Allow.Reason != "古い装置" || !got[0].Allow.HasTag(TagException) || got[0].To.Name != "db" || len(got[0].From.CIDRs) != 3 {
		t.Errorf("[AC-174-1-4] the realizer must get the passing allow with its reason, tags and networks, got %+v", got)
	}
	if rs := res.Resources(); len(rs) != 1 || rs[0].Provider != "acl" || rs[0].ID != "sw1/acl/101" {
		t.Errorf("[AC-174-1-4] resources = %+v", rs)
	}
	bad := strings.Replace(in, "test-acl", "nope", 1)
	if _, err := Compile(mustParse(t, bad)); err == nil || !strings.Contains(err.Error(), `unknown realizer "nope"`) {
		t.Errorf("[AC-174-1-4] an unknown realizer must be refused, got %v", err)
	}
}

const domainNets = `"networks": [
  {"name": "users-a", "domain": "tenant-a", "cidrs": ["10.0.0.0/24"]},
  {"name": "users-b", "domain": "tenant-b", "cidrs": ["10.0.0.0/24"]},
  {"name": "svc", "domain": "service", "cidrs": ["172.16.0.0/24"]},
  {"name": "svc2", "domain": "service", "cidrs": ["172.16.1.0/24"]},
  {"name": "office", "cidrs": ["10.1.0.0/24"]}
]`

func TestCheck_Domains(t *testing.T) {
	// [AC-195-1-2] [AC-195-1-3]
	if fs := findings(t, `{`+domainNets+`}`); len(fs) != 0 {
		t.Errorf("networks overlapping in different domains must pass, got %v", fs)
	}
	cases := []struct{ name, in, kind, contains string }{
		{"cross-domain allow", `{` + domainNets + `, "allows": [{"from": "users-a", "to": "svc"}]}`,
			FindingCrossDomain, "from users-a is in domain tenant-a and to svc is in domain service"},
		{"overlap in a domain", `{"networks": [{"name": "a", "domain": "x", "cidrs": ["10.0.0.0/24"]}, {"name": "b", "domain": "x", "cidrs": ["10.0.0.5"]}]}`,
			FindingOverlap, "overlap in domain x"},
		{"a target without interfaces outside the default domain", `{` + domainNets + `, "targets": [
			{"name": "fw", "kind": "boundary", "networks": ["svc", "svc2"], "id": "inet/f/c"}]}`,
			FindingTarget, "names network svc of domain service but has no interfaces"},
		{"an interface of an undeclared domain", `{` + domainNets + `, "targets": [
			{"name": "fw", "kind": "host", "networks": ["svc"], "interfaces": [{"name": "p0", "domain": "service"}, {"name": "p1", "domain": "nowhere"}], "id": "x"}]}`,
			FindingTarget, "interface p1 sees domain nowhere, where no network is declared"},
		{"an interface naming a network of another domain", `{` + domainNets + `, "targets": [
			{"name": "fw", "kind": "host", "networks": ["svc", "office"], "interfaces": [{"name": "p0", "domain": "service", "networks": ["svc", "office"]}], "id": "x"}]}`,
			FindingTarget, "interface p0 sees domain service but names network office of domain default"},
		{"a network on no interface", `{` + domainNets + `, "targets": [
			{"name": "fw", "kind": "host", "networks": ["svc", "office"], "interfaces": [{"name": "p0", "domain": "service"}], "id": "x"}]}`,
			FindingTarget, "network office (domain default) is on no interface"},
		{"a network on two interfaces", `{` + domainNets + `, "targets": [
			{"name": "fw", "kind": "host", "networks": ["svc"], "interfaces": [{"name": "p0", "domain": "service"}, {"name": "p1", "domain": "service"}], "id": "x"}]}`,
			FindingTarget, "network svc is on both interfaces p0 and p1"},
		{"an interface named twice", `{` + domainNets + `, "targets": [
			{"name": "fw", "kind": "host", "networks": ["svc"], "interfaces": [{"name": "p0", "domain": "service"}, {"name": "p0", "domain": "tenant-a"}], "id": "x"}]}`,
			FindingTarget, "names interface p0 twice"},
	}
	for _, c := range cases {
		if fs := findings(t, c.in); !hasFinding(fs, SeverityError, c.kind, c.contains) {
			t.Errorf("%s: want error %s containing %q, got %v", c.name, c.kind, c.contains, fs)
		}
	}
	if _, err := Parse(tree(t, `{"targets": [{"name": "fw", "kind": "host", "networks": ["svc"], "interfaces": [{"name": "p0"}], "id": "x"}]}`), "np.json"); err == nil {
		t.Error("an interface without domain must be refused")
	}
}

func TestCompile_InterfacesAndDirections(t *testing.T) {
	// [AC-195-1-4] 境界は from の網の口で in・to の網の口で out。ホストは to が
	// 自分の網なら in・from が自分の網なら out。口の networks は埋めて渡る。
	var got = map[string][]Passing{}
	var targets = map[string]Target{}
	if err := Register("test-per-port", RealizerFunc(func(t Target, allows []Passing) (Output, error) {
		got[t.Name], targets[t.Name] = allows, t
		return Output{Resources: []Resource{{Provider: "fake", ID: t.ID}}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	p := mustParse(t, `{`+domainNets+`, "allows": [
		{"from": "svc", "to": "svc2", "proto": "tcp", "ports": [5432], "reason": "db"},
		{"from": "users-a", "to": "users-a", "proto": "icmp"},
		{"from": "office", "to": "office"}], "targets": [
		{"name": "router", "realizer": "test-per-port", "kind": "boundary", "networks": ["svc", "svc2"],
		 "interfaces": [{"name": "eth0", "domain": "service", "networks": ["svc"]}, {"name": "eth1", "domain": "service", "networks": ["svc2"]}], "id": "r"},
		{"name": "vm", "realizer": "test-per-port", "kind": "host", "networks": ["users-a", "svc2", "office"],
		 "interfaces": [{"name": "net0", "domain": "tenant-a"}, {"name": "net1", "domain": "service"}, {"name": "net2", "domain": "default"}], "id": "vm"}]}`)
	if _, err := Compile(p); err != nil {
		t.Fatal(err)
	}
	r := got["router"]
	if len(r) != 1 || r[0].Domain != "service" ||
		len(r[0].Via) != 2 || r[0].Via[0] != (Via{"eth0", "service", DirectionIn}) || r[0].Via[1] != (Via{"eth1", "service", DirectionOut}) {
		t.Errorf("router got %+v", r)
	}
	vm := got["vm"]
	if len(vm) != 3 {
		t.Fatalf("vm got %+v", vm)
	}
	// svc → svc2 は vm の svc2（net1）に入る。users-a → users-a は net0 で入りも出も。
	if vm[0].Via[0] != (Via{"net1", "service", DirectionIn}) || len(vm[0].Via) != 1 {
		t.Errorf("vm svc→svc2 via %+v", vm[0].Via)
	}
	if len(vm[1].Via) != 2 || vm[1].Via[0].Direction != DirectionIn || vm[1].Via[1].Direction != DirectionOut || vm[1].Domain != "tenant-a" {
		t.Errorf("vm users-a→users-a via %+v", vm[1].Via)
	}
	if itf, ok := targets["vm"].Interface("net1"); !ok || len(itf.Networks) != 1 || itf.Networks[0] != "svc2" {
		t.Errorf("net1 networks must be filled from the target's networks of its domain: %+v", itf)
	}
	by := ByInterface(targets["vm"], vm)
	if len(by) != 3 || by[0].Interface.Name != "net0" || len(by[0].In) != 1 || len(by[0].Out) != 1 ||
		len(by[1].In) != 1 || len(by[1].Out) != 0 || len(by[2].In) != 1 || by[2].In[0].Domain != DefaultDomain {
		t.Errorf("ByInterface = %+v", by)
	}

	// 本体の nftables は口を受けない。
	if _, err := (NFTables{}).Realize(Target{ID: "inet/f/c", Interfaces: []Interface{{Name: "eth0", Domain: "default"}}}, nil); err == nil {
		t.Error("the nftables realizer must refuse interfaces")
	}
}
