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
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// [AC-178-1-1] 無名の集合の正規形: 要素を範囲にし、重なり・隣り合いを合わせて
// 並べ替える（IPv4・IPv6・ポート）。1 つの範囲にまとまれば { } を外す。
// 正規化できない集合（名前・連結・map）と引用符の中はそのまま。
func TestNormalizeSets(t *testing.T) {
	cases := map[string]string{
		// IPv4: 隣り合う /24 は /23、並びの違い・重なり・範囲・単体のアドレス
		"ip saddr { 10.0.0.0/24, 10.0.1.0/24 } accept":                       "ip saddr 10.0.0.0/23 accept",
		"ip saddr { 10.0.0.0/23 } accept":                                    "ip saddr 10.0.0.0/23 accept",
		"ip saddr { 10.0.1.0/24, 10.0.0.5 } accept":                          "ip saddr { 10.0.0.5, 10.0.1.0/24 } accept",
		"ip saddr { 10.0.3.0/24, 10.0.0.0-10.0.0.255, 10.0.2.0/24 } drop":    "ip saddr { 10.0.0.0/24, 10.0.2.0/23 } drop",
		"ip saddr { 10.0.0.0/24, 10.0.0.128/25 } accept":                     "ip saddr 10.0.0.0/24 accept",
		"ip saddr { 10.0.0.1, 10.0.0.2, 10.0.0.3 } accept":                   "ip saddr 10.0.0.1-10.0.0.3 accept",
		"ip saddr { 10.0.0.0/24, 10.0.1.0/24, 10.0.2.0/24 } accept":          "ip saddr 10.0.0.0-10.0.2.255 accept",
		"ip saddr { 10.0.0.9/24 } accept":                                    "ip saddr 10.0.0.0/24 accept",
		"ip saddr {0.0.0.0/1,128.0.0.0/1} accept":                            "ip saddr 0.0.0.0/0 accept",
		"ip saddr != { 10.0.0.0/24, 10.0.1.0/24 } drop":                      "ip saddr != 10.0.0.0/23 drop",
		"ip saddr { 10.0.0.1 } ip daddr { 10.0.1.0/24, 10.0.0.0/24 } accept": "ip saddr 10.0.0.1 ip daddr 10.0.0.0/23 accept",
		// IPv6
		"ip6 saddr { 2001:db8::/33, 2001:db8:8000::/33, ::1 } accept": "ip6 saddr { ::1, 2001:db8::/32 } accept",
		"ip6 saddr { 2001:DB8::1, 2001:db8::2 } accept":               "ip6 saddr 2001:db8::1-2001:db8::2 accept",
		"ip6 saddr { 2001:db8::/64, 2001:db8:0:2::/64 } accept":       "ip6 saddr { 2001:db8::/64, 2001:db8:0:2::/64 } accept",
		// ポート・ポートの範囲
		"tcp dport { 22, 80-90, 23, 91 } accept": "tcp dport { 22-23, 80-91 } accept",
		"tcp dport { 22-23, 80-91 } accept":      "tcp dport { 22-23, 80-91 } accept",
		"tcp dport { 443, 80 } accept":           "tcp dport { 80, 443 } accept",
		"tcp dport { 80-90, 85-100 } accept":     "tcp dport 80-100 accept",
		"tcp dport { 22 } accept":                "tcp dport 22 accept",
		"tcp dport { 0-65535 } accept":           "tcp dport 0-65535 accept",
		// 正規化しない（書いたまま）
		"ct state { established, related } accept":                     "ct state { established, related } accept",
		"tcp dport { ssh, http } accept":                               "tcp dport { ssh, http } accept",
		"ip saddr . tcp dport { 10.0.0.1 . 22, 10.0.0.2 . 80 } accept": "ip saddr . tcp dport { 10.0.0.1 . 22, 10.0.0.2 . 80 } accept",
		"tcp dport vmap { 22 : accept, 23 : drop }":                    "tcp dport vmap { 22 : accept, 23 : drop }",
		"ip saddr { 10.0.0.1, ::1 } accept":                            "ip saddr { 10.0.0.1, ::1 } accept",
		"ip saddr { 10.0.0.1, 22 } accept":                             "ip saddr { 10.0.0.1, 22 } accept",
		"tcp dport { 90-80 } accept":                                   "tcp dport { 90-80 } accept",
		"ip saddr @allowed accept":                                     "ip saddr @allowed accept",
		`accept comment "{ 10.0.0.0/24, 10.0.1.0/24 }"`:                `accept comment "{ 10.0.0.0/24, 10.0.1.0/24 }"`,
		"tcp dport 22   accept":                                        "tcp dport 22 accept",
	}
	for in, want := range cases {
		if got := NormalizeSets(in); got != want {
			t.Errorf("[AC-178-1-1] NormalizeSets(%q) = %q, want %q", in, got, want)
		}
	}
}

// [AC-178-1-2] 比べる形（ruleKey）: nft が書き戻した形（まとめた・並べ替えた・
// 1 要素の { } を外した・カウンタ付き）と宣言は同じ。要素が本当に違えば違う。
func TestRuleKey(t *testing.T) {
	same := [][2]string{
		{"ip saddr { 10.0.0.0/24, 10.0.1.0/24 } accept", "ip saddr { 10.0.0.0/23 } accept"},
		{"ip saddr { 10.0.0.0/23 } accept", "ip saddr 10.0.0.0/23 accept"},
		{"tcp dport { 23, 22, 91, 80-90 } counter accept", "tcp dport { 22-23, 80-91 } counter packets 4 bytes 240 accept"},
		{"ip6 saddr { 2001:db8:8000::/33, 2001:db8::/33 } accept", "ip6 saddr { 2001:db8::/32 } accept"},
	}
	for _, c := range same {
		if ruleKey(c[0]) != ruleKey(c[1]) {
			t.Errorf("[AC-178-1-2] %q and %q must compare equal (%q vs %q)", c[0], c[1], ruleKey(c[0]), ruleKey(c[1]))
		}
	}
	differ := [][2]string{
		{"ip saddr { 10.0.0.0/24, 10.0.2.0/24 } accept", "ip saddr { 10.0.0.0/23 } accept"},
		{"tcp dport { 22, 24 } accept", "tcp dport { 22-24 } accept"},
		{"ip6 saddr { 2001:db8::/33 } accept", "ip6 saddr { 2001:db8::/32 } accept"},
		{"ip saddr { 10.0.0.0/24, 10.0.1.0/24 } accept", "ip saddr { 10.0.0.0/23 } drop"},
	}
	for _, c := range differ {
		if ruleKey(c[0]) == ruleKey(c[1]) {
			t.Errorf("[AC-178-1-2] %q and %q must differ", c[0], c[1])
		}
	}
}

// [AC-178-1-3] PlanRefiner（file 版・live 版で共通）: nft がまとめて書き戻した
// だけの rules の差分は落とし、要素が本当に違う差分は残す。
func TestRefineRulesSets(t *testing.T) {
	declared := "ip saddr { 10.0.0.0/24, 10.0.1.0/24 } accept" + RuleSep + "tcp dport { 22, 23 } accept"
	merged := "ip saddr { 10.0.0.0/23 } accept" + RuleSep + "tcp dport { 22-23 } accept"
	other := "ip saddr { 10.0.0.0/24, 10.0.2.0/24 } accept" + RuleSep + "tcp dport { 22-23 } accept"
	live, err := NewLive(LiveConfig{NFT: "nft", Tables: []TableRef{{Family: "inet", Name: "t"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []interface {
		RefinePlan(string, core.State, map[string]string, []core.Op) ([]core.Op, error)
	}{New(t.TempDir() + "/ruleset.nft"), live} {
		for _, c := range []struct {
			from string
			keep bool
		}{{merged, false}, {other, true}} {
			op := core.Op{Kind: core.OpKindUpdate, Fields: []core.FieldDiff{{Field: "rules", From: c.from, To: declared}}}
			ops, err := r.RefinePlan("inet/t/c", core.State{}, map[string]string{"rules": declared}, []core.Op{op})
			if err != nil {
				t.Fatalf("[AC-178-1-3] %T: %v", r, err)
			}
			if kept := len(ops) == 1 && len(ops[0].Fields) == 1; kept != c.keep {
				t.Errorf("[AC-178-1-3] %T: from %q: kept=%v, want %v", r, c.from, kept, c.keep)
			}
		}
	}
}
