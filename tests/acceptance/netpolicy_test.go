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

package acceptance

// netpolicy_test.go — #174: 網の間の許可を抽象のリソースにする（検査と
// nftables への変換）。
//
//	[AC-174-3-1]  plan: 網と許可の宣言を検査し（隠れた許可の warning に行の
//	              reason）、実現先ごとの展開（通る許可・reason・tags・ルール）を
//	              文と --json に出す。展開した base chain の CREATE の rules に
//	              reason の comment が入る。
//	[AC-174-3-2]  plan: 理由の無い例外・網の重なり・宣言に無い網は読み込みで断る
//	              （終了コード 1、--json の error に行の場所と理由）。
//	[AC-174-3-3]  常駐（偽の nft）: 展開した base chain が Apply され（承認した
//	              レーンで）、ルールの comment に reason が出る。宣言を変えると
//	              （許可を足す・reason を変える）ルールが変わり、その後は動かない。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/nftables/nftfake"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// netPolicy は網 3 つ・境界の FW 1 つの network_policy（allows を差し替える）。
func netPolicy(allows ...map[string]any) map[string]any {
	return map[string]any{
		"networks": []map[string]any{
			{"name": "office", "cidrs": []string{"10.1.0.0/24", "2001:db8:1::/64"}},
			{"name": "dmz", "cidrs": []string{"192.0.2.0/24", "2001:db8:2::/64"}},
			{"name": "db", "cidrs": []string{"10.2.0.10"}},
		},
		"allows": allows,
		"targets": []map[string]any{{
			"name": "fw1", "kind": "boundary", "networks": []string{"office", "dmz", "db"},
			"id": "inet/filter/forward", "scope": nftLabScope,
			"before": []string{"ct state established,related accept"},
		}},
	}
}

var (
	allowWeb    = map[string]any{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443}, "reason": "社内から公開の web"}
	allowWebDup = map[string]any{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{"443"}, "reason": "二重に書いた"}
	allowTelnet = map[string]any{"from": "office", "to": "db", "proto": "tcp", "ports": []any{23}, "tags": []string{"exception"}, "reason": "古い装置は telnet しか話さない"}
	allowSQL    = map[string]any{"from": "dmz", "to": "db", "proto": "tcp", "ports": []any{5432}, "reason": "web から \"本番\" の DB"}
	allowWide   = map[string]any{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{"1-1024"}, "reason": "広い"}
)

func TestNetworkPolicy_Plan(t *testing.T) {
	// [AC-174-3-1]
	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{"network_policy": netPolicy(allowWide, allowWeb, allowTelnet)})
	ruleset := filepath.Join(dir, "rules")

	text, stderr, code := runPlan(t, intentDir, filepath.Join(dir, "state.json"), "--ruleset", ruleset)
	if code != 0 {
		t.Fatalf("plan exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"Network policy (§7.6): 3 network(s), 3 allow(s), 1 target(s)",
		`warning: shadowed: resources.json: network_policy.allows[1] (office → dmz tcp 443): has no effect: resources.json: network_policy.allows[0] (office → dmz tcp 1-1024) already allows all of it (reason: "広い") — reason: "社内から公開の web"`,
		"target fw1 (boundary, nftables) → 3 allow(s) nftables/inet/filter/forward",
		`reason: "古い装置は telnet しか話さない"`,
		"tags:   [exception]",
		`→ ip saddr 10.1.0.0/24 ip daddr 10.2.0.10 tcp dport 23 accept comment "古い装置は telnet しか話さない"`,
		"+ CREATE nftables/site=lab/inet/filter/forward",
		`+ policy                (new) → "drop"`,
		`comment \"社内から公開の web\"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("[AC-174-3-1] plan output must contain %q:\n%s", want, text)
		}
	}

	out, stderr, code := runPlan(t, intentDir, filepath.Join(dir, "state.json"), "--ruleset", ruleset, "--json")
	if code != 0 {
		t.Fatalf("plan --json exit %d: %s", code, stderr)
	}
	var rep planreport.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("plan --json: %v\n%s", err, out)
	}
	np := rep.NetworkPolicy
	if np == nil || np.Networks != 3 || np.Allows != 3 || len(np.Targets) != 1 {
		t.Fatalf("[AC-174-3-1] network_policy = %+v", np)
	}
	if len(np.Findings) != 1 || np.Findings[0].Kind != "shadowed" || np.Findings[0].Severity != "warning" || np.Findings[0].Reason != "社内から公開の web" {
		t.Errorf("[AC-174-3-1] findings = %+v", np.Findings)
	}
	tg := np.Targets[0]
	if tg.Name != "fw1" || len(tg.Resources) != 1 || tg.Resources[0] != "nftables/inet/filter/forward" || len(tg.Allows) != 3 {
		t.Fatalf("[AC-174-3-1] target = %+v", tg)
	}
	tel := tg.Allows[2]
	if tel.Reason != "古い装置は telnet しか話さない" || len(tel.Tags) != 1 || tel.Tags[0] != "exception" || len(tel.Rules) != 1 ||
		!strings.Contains(tel.Rules[0], `comment "古い装置は telnet しか話さない"`) {
		t.Errorf("[AC-174-3-1] telnet allow = %+v", tel)
	}
	// office → dmz は IPv4 と IPv6 の 2 ルール。
	if web := tg.Allows[0]; len(web.Rules) != 2 || !strings.HasPrefix(web.Rules[1], "ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 tcp dport 1-1024 accept") {
		t.Errorf("[AC-174-3-1] wide allow = %+v", web)
	}
	if len(rep.Operations) != 1 || rep.Operations[0].Kind != planreport.KindCreate {
		t.Errorf("[AC-174-3-1] operations = %+v", rep.Operations)
	}
}

func TestNetworkPolicy_PlanRefuses(t *testing.T) {
	// [AC-174-3-2]
	noReason := map[string]any{"from": "office", "to": "db", "proto": "tcp", "ports": []any{23}, "tags": []string{"exception"}}
	overlap := netPolicy(allowWeb)
	overlap["networks"] = append(overlap["networks"].([]map[string]any), map[string]any{"name": "lab", "cidrs": []string{"10.1.0.128/25"}})
	for _, c := range []struct {
		name   string
		policy map[string]any
		want   []string
	}{
		{"an exception without reason", netPolicy(allowWeb, noReason),
			[]string{"error: exception_without_reason: resources.json: network_policy.allows[1] (office → db tcp 23)", "tags: exception"}},
		{"overlapping networks", overlap,
			[]string{"error: overlap: networks office (10.1.0.0/24) and lab (10.1.0.128/25) overlap"}},
		{"an undeclared network", netPolicy(allowWeb, map[string]any{"from": "office", "to": "lab", "reason": "検証の網"}),
			[]string{"error: undeclared_network: resources.json: network_policy.allows[1] (office → lab any): network lab is not declared", `reason: "検証の網"`}},
		{"the same allow twice", netPolicy(allowWeb, allowWebDup),
			[]string{"error: duplicate: resources.json: network_policy.allows[1]", `reason: "二重に書いた"`}},
	} {
		dir := t.TempDir()
		writeIntentFile(t, dir, map[string]any{"network_policy": c.policy})
		out, stderr, code := runPlan(t, dir, filepath.Join(t.TempDir(), "state.json"), "--ruleset", filepath.Join(t.TempDir(), "rules"), "--json")
		if code != 1 {
			t.Errorf("[AC-174-3-2] %s: exit %d, want 1", c.name, code)
			continue
		}
		var rep planreport.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		for _, w := range c.want {
			if !strings.Contains(rep.Error, w) || !strings.Contains(stderr, w) {
				t.Errorf("[AC-174-3-2] %s: error must contain %q:\n%s", c.name, w, rep.Error)
			}
		}
	}
}

func TestNetworkPolicy_DaemonAppliesAndFollowsTheDeclaration(t *testing.T) {
	// [AC-174-3-3]
	fake := &nftfake.NFT{}
	guard := &nftfake.Guard{NFT: fake}
	p, err := nftables.NewLive(nftables.LiveConfig{
		Runner: fake, Tables: []nftables.TableRef{{Family: "inet", Name: "filter"}},
		Guard: guard, RollbackAfter: 2 * time.Second,
	}, nftables.WithScope(core.NewScope(nftLabScope)))
	if err != nil {
		t.Fatal(err)
	}
	intent := func(allows ...map[string]any) map[string]any {
		return map[string]any{
			"network_policy": netPolicy(allows...),
			// 作るのは承認の要る変更（#179）。
			"planned_change": []map[string]any{lane("create", true), lane("update", true)},
		}
	}
	d := startApprovalDaemon(t, p, intent(allowWeb, allowTelnet), budget.Config{})
	want := []string{
		"ct state established,related accept",
		`ip saddr 10.1.0.0/24 ip daddr 192.0.2.0/24 tcp dport 443 accept comment "社内から公開の web"`,
		`ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 tcp dport 443 accept comment "社内から公開の web"`,
		`ip saddr 10.1.0.0/24 ip daddr 10.2.0.10 tcp dport 23 accept comment "古い装置は telnet しか話さない"`,
	}
	rulesAre := func(want []string) func() bool {
		return func() bool {
			return fake.Policy("inet", "filter", "forward") == "drop" &&
				strings.Join(fake.Rules("inet", "filter", "forward"), "\n") == strings.Join(want, "\n")
		}
	}
	d.waitFor("the realized base chain", rulesAre(want))
	if !strings.Contains(fake.Dump(), "type filter hook forward priority 0; policy drop;") {
		t.Errorf("[AC-174-3-3] the chain must be a base chain on hook forward:\n%s", fake.Dump())
	}

	// 宣言を変える: telnet の例外を外し、DB への許可を足し、web の reason を変える。
	webRenamed := map[string]any{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443, 80}, "reason": "社内から web（80 も）"}
	writeIntentFile(t, d.intentDir, intent(webRenamed, allowSQL))
	want2 := []string{
		"ct state established,related accept",
		`ip saddr 10.1.0.0/24 ip daddr 192.0.2.0/24 tcp dport { 80, 443 } accept comment "社内から web（80 も）"`,
		`ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 tcp dport { 80, 443 } accept comment "社内から web（80 も）"`,
		`ip saddr 192.0.2.0/24 ip daddr 10.2.0.10 tcp dport 5432 accept comment "web から '本番' の DB"`,
	}
	d.waitFor("the rules after the declaration changed", rulesAre(want2))

	// 宣言どおりになった後は動かない（読み戻しで差分が出ない）。
	before := fake.Dump()
	arms, _, _ := guard.Counts()
	time.Sleep(300 * time.Millisecond)
	if got := fake.Dump(); got != before {
		t.Errorf("[AC-174-3-3] the ruleset changed after convergence:\n%s\nwas\n%s", got, before)
	}
	if a, _, fires := guard.Counts(); a != arms || fires != 0 {
		t.Errorf("[AC-174-3-3] arms %d → %d, fires %d: want no more Applies and no rollback", arms, a, fires)
	}
}
