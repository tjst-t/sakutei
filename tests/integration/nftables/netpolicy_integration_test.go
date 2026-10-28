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

//go:build integration_nftables

package nftables

// 網の間の許可（network_policy:、Backlog #174）を本物の nft に当てる。展開した
// base chain（境界の forward・ホストの input）が流れ、reason が comment に出て、
// 読み戻しで drift にならない（nft の comment の長さと使える文字に合わせて
// 切る・逃がした形が、そのまま読み戻される）。

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNFTablesNetworkPolicy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	confirm := "tcp://" + ln.Addr().String()

	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "providers.json")
	writeJSONFile(t, config, map[string]any{"providers": map[string]any{"nftables": map[string]any{
		"ruleset_target": "live", "ruleset_tables": "inet/np", "ruleset_nft": lab.wrapper,
		"ruleset_rollback_timeout": "3s", "ruleset_scope": "site=lab",
	}}})
	long := strings.Repeat("長い理由の文。", 12) // 252 バイト: 128 バイトで切る
	allows := []map[string]any{
		{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443, 80}, "reason": `社内から "公開" の web; 80 も`},
		{"from": "office", "to": "db", "proto": "tcp", "ports": []any{23}, "tags": []string{"exception"}, "reason": `古い装置は telnet だけ # 2027 に更新 \ 予定`},
		{"from": "dmz", "to": "db", "proto": "tcp", "ports": []any{"5432"}, "reason": long},
		{"from": "office", "to": "dmz", "proto": "icmpv6", "reason": "近隣探索"},
		{"from": "db", "to": "db", "proto": "udp", "ports": []any{"7000-7100"}},
	}
	declare := func(allows []map[string]any) {
		writeJSONFile(t, filepath.Join(intentDir, "net.json"), map[string]any{
			"planned_change": approvedLanes(),
			"network_policy": map[string]any{
				"networks": []map[string]any{
					{"name": "office", "cidrs": []string{"10.1.0.0/24", "10.1.1.0/24", "2001:db8:1::/64"}},
					{"name": "dmz", "cidrs": []string{"192.0.2.0/24", "2001:db8:2::/64"}},
					{"name": "db", "cidrs": []string{"10.2.0.10", "10.2.0.11"}},
				},
				"allows": allows,
				"targets": []map[string]any{
					{"name": "fw1", "kind": "boundary", "networks": []string{"office", "dmz", "db"}, "id": "inet/np/forward",
						"scope": labScope, "before": []string{"ct state established,related accept"}, "settings": map[string]any{"confirm": confirm}},
					{"name": "db1", "kind": "host", "networks": []string{"db"}, "id": "inet/np/input",
						"scope": labScope, "before": []string{"ct state established,related accept", "iif \"lo\" accept"}, "settings": map[string]any{"confirm": confirm}},
				},
			},
		})
	}
	run := func(verb string, extra ...string) (string, int) {
		args := append([]string{verb, "--intent", intentDir, "--state", filepath.Join(dir, "fake-state.json"),
			"--provider-config", config, "--audit-log", filepath.Join(dir, "audit.jsonl")}, extra...)
		if verb == "status" {
			args = append([]string{verb, "--intent", intentDir, "--state", filepath.Join(dir, "fake-state.json"),
				"--provider-config", config}, extra...)
		}
		out, err := exec.Command(lab.bin, args...).CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	driftOf := func() string {
		out, code := run("status", "--json")
		if code != 0 {
			t.Fatalf("status exited %d:\n%s", code, out)
		}
		i := strings.Index(out, `"summary"`)
		if i < 0 {
			t.Fatalf("status --json: no summary:\n%s", out)
		}
		return out[i : i+strings.Index(out[i:], "}")+1]
	}
	table := func() string { return nft(t, "-a", "-s", "list", "table", "inet", "np") }

	t.Run("the allows are applied with their reasons as comments", func(t *testing.T) {
		declare(allows)
		if out, code := run("reconcile"); code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		got := table()
		for _, want := range []string{
			"chain forward {", "type filter hook forward priority filter; policy drop;",
			"chain input {", "type filter hook input priority filter; policy drop;",
			`ip saddr 10.1.0.0/23 ip daddr 192.0.2.0/24 tcp dport { 80, 443 } accept comment "社内から '公開' の web, 80 も"`,
			`comment "古い装置は telnet だけ # 2027 に更新 / 予定"`,
			`ip6 saddr 2001:db8:1::/64 ip6 daddr 2001:db8:2::/64 meta l4proto ipv6-icmp accept comment "近隣探索"`,
			`ip saddr 10.2.0.10/31 ip daddr 10.2.0.10/31 udp dport 7000-7100 accept # handle`,
			"…\"",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the table must contain %q:\n%s", want, got)
			}
		}
		if s := driftOf(); !strings.Contains(s, `"drift": 0`) || !strings.Contains(s, `"conformant": 2`) {
			t.Fatalf("status after reconcile: %s", s)
		}
	})

	t.Run("no drift however many cycles (comments read back as written)", func(t *testing.T) {
		before := table()
		for i := 0; i < 3; i++ {
			if out, code := run("reconcile"); code != 0 {
				t.Fatalf("reconcile %d exited %d:\n%s", i, code, out)
			}
			if s := driftOf(); !strings.Contains(s, `"drift": 0`) {
				t.Fatalf("cycle %d: %s", i, s)
			}
		}
		if got := table(); got != before {
			t.Fatalf("the table changed (an Apply ran):\n%s\nwas:\n%s", got, before)
		}
		if out, code := run("plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
			t.Fatalf("plan exited %d, want converged:\n%s", code, out)
		}
	})

	t.Run("changing the declaration changes the rules", func(t *testing.T) {
		changed := append([]map[string]any{}, allows[:2]...)
		changed[0] = map[string]any{"from": "office", "to": "dmz", "proto": "tcp", "ports": []any{443}, "reason": "443 だけにした"}
		changed = append(changed, map[string]any{"from": "dmz", "to": "db", "proto": "tcp", "ports": []any{3306}, "reason": "MySQL に移した"})
		declare(changed)
		if out, code := run("reconcile"); code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		got := table()
		if !strings.Contains(got, `tcp dport 443 accept comment "443 だけにした"`) || !strings.Contains(got, `tcp dport 3306 accept comment "MySQL に移した"`) {
			t.Errorf("the new rules are missing:\n%s", got)
		}
		for _, gone := range []string{"5432", "7000-7100", "ipv6-icmp", "80, 443"} {
			if strings.Contains(got, gone) {
				t.Errorf("%q must be gone:\n%s", gone, got)
			}
		}
		if s := driftOf(); !strings.Contains(s, `"drift": 0`) {
			t.Fatalf("status: %s", s)
		}
	})

	t.Run("an exception without reason is refused and nothing changes", func(t *testing.T) {
		before := table()
		bad := []map[string]any{{"from": "office", "to": "db", "proto": "tcp", "ports": []any{23}, "tags": []string{"exception"}}}
		declare(bad)
		out, code := run("reconcile")
		if code != 1 || !strings.Contains(out, "exception_without_reason") {
			t.Fatalf("reconcile exited %d, want 1 with exception_without_reason:\n%s", code, out)
		}
		if got := table(); got != before {
			t.Fatalf("changed:\n%s\nwant:\n%s", got, before)
		}
	})
}
