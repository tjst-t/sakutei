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

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
)

// TestNetworkPolicyToNFTables は network_policy: を go:netpolicy で nftables の base
// chain に展開し、実の nftables provider（ruleset のファイル）に当てる（E-B5）。
// 展開した宣言はほかの宣言と同じ検証（Schema・範囲）を通る。
func TestNetworkPolicyToNFTables(t *testing.T) {
	w := t.TempDir()
	rules := filepath.Join(w, "firewall.rules")
	os.WriteFile(rules, []byte("table inet filter {\n    chain forward {\n        type filter hook forward priority 0; policy drop;\n    }\n}\n"), 0o644)
	os.WriteFile(filepath.Join(w, "config.yaml"), []byte("providers:\n  fw:\n    module: nftables\n    settings:\n      hosts:\n        lab:\n          ruleset: "+rules+"\n"), 0o644)
	os.MkdirAll(filepath.Join(w, "intent"), 0o755)
	os.WriteFile(filepath.Join(w, "intent", "net.yaml"), []byte(`expand:
  - kind: go:netpolicy
ranges:
  - provider: fw
    args: {host: lab, tables: inet/filter}
network_policy:
  networks:
    - {name: office, cidrs: ["10.1.0.0/24"]}
    - {name: dmz, cidrs: ["192.0.2.0/24"]}
  allows:
    - {from: office, to: dmz, proto: tcp, ports: [443], reason: "社内から公開の web"}
  targets:
    - name: lab
      kind: boundary
      networks: [office, dmz]
      provider: fw
      id: lab/inet/filter/forward
      before: ["ct state established,related accept"]
`), 0o644)
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), args, &out, &errOut, Options()...)
		return code, out.String() + errOut.String()
	}
	common := []string{"--intent", filepath.Join(w, "intent"), "--config", filepath.Join(w, "config.yaml")}
	if code, out := run(append([]string{"reconcile", "--record", filepath.Join(w, "record")}, common...)...); code != 0 {
		t.Fatalf("reconcile: %d\n%s", code, out)
	}
	got, _ := os.ReadFile(rules)
	if !strings.Contains(string(got), `ip saddr 10.1.0.0/24 ip daddr 192.0.2.0/24 tcp dport 443 accept comment "社内から公開の web"`) {
		t.Fatalf("許可を ruleset に当てていない:\n%s", got)
	}
	if code, out := run(append([]string{"plan"}, common...)...); code != 0 || !strings.Contains(out, "そのまま 1") {
		t.Fatalf("当てたあとの plan: %d\n%s", code, out)
	}
	// 重なる網は読み込みで断る（E-B5）。
	data, _ := os.ReadFile(filepath.Join(w, "intent", "net.yaml"))
	os.WriteFile(filepath.Join(w, "intent", "net.yaml"), []byte(strings.Replace(string(data), "192.0.2.0/24", "10.1.0.0/16", 1)), 0o644)
	if code, out := run(append([]string{"plan"}, common...)...); code != 2 || !strings.Contains(out, "重なる") {
		t.Fatalf("重なる網: %d\n%s", code, out)
	}
}
