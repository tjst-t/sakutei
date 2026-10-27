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

package nftables

import (
	"os"
	"testing"
)

// [AC-121-2-1] 正規形: 空白・カウンタの値・quota の used だけの違いは同じ形になり、
// 引用符の中と順序はそのまま。
func TestCanonRule(t *testing.T) {
	cases := map[string]string{
		"tcp dport 22   accept":                                "tcp dport 22 accept",
		"\ttcp dport 22 accept ":                               "tcp dport 22 accept",
		"tcp dport 8080 counter packets 3 bytes 9 accept":      "tcp dport 8080 counter accept",
		"tcp dport 8080 counter accept":                        "tcp dport 8080 counter accept",
		"quota 10 mbytes used 1 mbytes accept":                 "quota 10 mbytes accept",
		"quota over 10 mbytes used 0 bytes drop":               "quota over 10 mbytes drop",
		`accept comment "a   b counter packets 1 bytes 2"`:     `accept comment "a   b counter packets 1 bytes 2"`,
		"ip saddr { 10.0.0.1, 10.0.0.2 } drop":                 "ip saddr { 10.0.0.1, 10.0.0.2 } drop",
		"meta mark set 1 counter packets 0 bytes 0 log accept": "meta mark set 1 counter log accept",
	}
	for in, want := range cases {
		if got := canonRule(in); got != want {
			t.Errorf("[AC-121-2-1] canonRule(%q) = %q, want %q", in, got, want)
		}
	}
	if got := canonRules("tcp dport 22  accept" + RuleSep + "drop"); got != "tcp dport 22 accept"+RuleSep+"drop" {
		t.Errorf("[AC-121-2-1] canonRules = %q", got)
	}
}

// [AC-121-2-2] nft のスクリプトに埋め込めないルール文は断る。
func TestCheckRuleText(t *testing.T) {
	for _, ok := range []string{"tcp dport 22 accept", `accept comment "a; # b"`} {
		if err := checkRuleText(ok); err != nil {
			t.Errorf("[AC-121-2-2] %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"accept; delete table inet other", "accept # x", "accept\nflush ruleset", `accept comment "x`} {
		if err := checkRuleText(bad); err == nil {
			t.Errorf("[AC-121-2-2] %q must be refused", bad)
		}
	}
}

// [AC-121-1-1] 記録した本物の nft の出力（testdata）を読む: JSON の並びと
// テキストのルール文が handle で合い、base chain・set は印が付く。
func TestParseRecordedTable(t *testing.T) {
	js, err := os.ReadFile("testdata/inet-filter.json")
	if err != nil {
		t.Fatal(err)
	}
	txt, err := os.ReadFile("testdata/inet-filter.txt")
	if err != nil {
		t.Fatal(err)
	}
	ref := TableRef{Family: "inet", Name: "filter"}
	tbl, rules, err := parseTableJSON(js, ref)
	if err != nil {
		t.Fatal(err)
	}
	texts, err := parseTableText(txt)
	if err != nil {
		t.Fatal(err)
	}
	if !tbl.exists || len(tbl.chains) != 3 {
		t.Fatalf("[AC-121-1-1] chains = %+v", tbl.chains)
	}
	if !tbl.find("input").base || tbl.find("web").base || tbl.find("empty").base {
		t.Errorf("[AC-121-1-1] base marks: %+v", tbl.chains)
	}
	if len(rules) != 4 || len(texts) != 4 {
		t.Fatalf("[AC-121-1-1] %d JSON rules, %d text rules, want 4", len(rules), len(texts))
	}
	web := tbl.find("web")
	var got []string
	for _, r := range rules {
		tr := texts[r.Handle]
		if r.Chain == "web" {
			if tr.chain != web.handle {
				t.Errorf("[AC-121-1-1] rule %d is in chain handle %d, want %d", r.Handle, tr.chain, web.handle)
			}
			got = append(got, canonRule(tr.text))
		}
	}
	want := []string{
		`tcp dport { 80, 443 } ct state established,new counter accept comment "web # handle 99"`,
		"ip saddr @allowed tcp dport 22 accept",
		"quota 10 mbytes accept",
	}
	if len(got) != len(want) {
		t.Fatalf("[AC-121-1-1] web rules = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[AC-121-1-1] web rule %d = %q, want %q", i, got[i], want[i])
		}
	}
}
