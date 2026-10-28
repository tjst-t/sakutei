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

package loader

// network_policy:（§7.6、Backlog #174）の読み込み。
//
//	[AC-174-2-1]  network_policy の実現先は nftables の resource として
//	              Resources に入り、ファイルをまたいで網・許可を合わせる。
//	[AC-174-2-2]  理由の無い例外・宣言に無い網・知らないキーは読み込みで断る。
//	[AC-174-2-3]  展開した id を人が resources にも書いた・external_owners が
//	              選ぶ・inventory に書いたものは断る。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeIntent(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const npNetworks = `{"network_policy": {"networks": [
  {"name": "office", "cidrs": ["10.1.0.0/24"]},
  {"name": "dmz", "cidrs": ["192.0.2.0/24"]}]}}`

const npAllows = `{"network_policy": {
  "allows": [{"from": "office", "to": "dmz", "proto": "tcp", "ports": [443], "reason": "公開の web"}],
  "targets": [{"name": "fw1", "kind": "boundary", "networks": ["office", "dmz"], "id": "inet/filter/forward",
               "scope": {"site": "lab"}, "before": ["ct state established,related accept"]}]}}`

func TestNetworkPolicy_Load(t *testing.T) {
	// [AC-174-2-1]
	dir := t.TempDir()
	writeIntent(t, dir, "10-networks.json", npNetworks)
	writeIntent(t, dir, "20-allows.json", npAllows)
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Resources) != 1 {
		t.Fatalf("want 1 resource, got %+v", in.Resources)
	}
	r := in.Resources[0]
	if r.ID.Provider != "nftables" || r.ID.Local != "inet/filter/forward" || r.ID.Scope.String() != "site=lab" {
		t.Errorf("[AC-174-2-1] id = %s", r.ID)
	}
	want := `ct state established,related accept ; ip saddr 10.1.0.0/24 ip daddr 192.0.2.0/24 tcp dport 443 accept comment "公開の web"`
	if r.Fields["rules"] != want || r.Fields["policy"] != "drop" || r.Fields["hook"] != "forward" {
		t.Errorf("[AC-174-2-1] fields = %v", r.Fields)
	}
	if in.NetworkPolicy == nil || len(in.NetworkPolicy.Realizations) != 1 || in.NetworkPolicy.Policy.Allows[0].Ref != "20-allows.json: network_policy.allows[0]" {
		t.Errorf("[AC-174-2-1] NetworkPolicy = %+v", in.NetworkPolicy)
	}

	// 宣言が無ければ nil。
	empty := t.TempDir()
	writeIntent(t, empty, "r.json", `{"resources": []}`)
	if in, err := LoadIntent(empty); err != nil || in.NetworkPolicy != nil {
		t.Errorf("no network_policy: NetworkPolicy = %+v, err = %v", in.NetworkPolicy, err)
	}
}

func TestNetworkPolicy_LoadRefuses(t *testing.T) {
	// [AC-174-2-2] [AC-174-2-3]
	for _, c := range []struct{ name, extra, want string }{
		{"exception without reason", `{"network_policy": {"allows": [{"from": "office", "to": "dmz", "proto": "tcp", "ports": [23], "tags": ["exception"]}]}}`,
			"exception_without_reason"},
		{"undeclared network", `{"network_policy": {"allows": [{"from": "office", "to": "lab", "reason": "x"}]}}`,
			"network lab is not declared"},
		{"unknown key", `{"network_policy": {"allows": [{"from": "office", "to": "dmz", "port": 443}]}}`,
			`"port"`},
		{"declared by hand too", `{"resources": [{"provider": "nftables", "id": "inet/filter/forward", "scope": {"site": "lab"},
			"fields": {"family": "inet", "table": "filter", "chain": "forward", "rules": ""}}]}`,
			`duplicate resource id "inet/filter/forward"`},
		{"selected by external_owners", `{"external_owners": [{"owner": "stage0", "provider": "nftables", "id": "inet/filter/forward"}]}`,
			"it is in external_owner stage0"},
	} {
		dir := t.TempDir()
		writeIntent(t, dir, "10-networks.json", npNetworks)
		writeIntent(t, dir, "20-allows.json", npAllows)
		writeIntent(t, dir, "30-extra.json", c.extra)
		_, err := LoadIntent(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("[AC-174-2-2] %s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.json")
	writeIntent(t, dir, "inventory.json", npNetworks)
	if _, err := LoadResourcesFile(path); err == nil || !strings.Contains(err.Error(), "network_policy belong in intent") {
		t.Errorf("[AC-174-2-3] an inventory file must not declare network_policy, got %v", err)
	}
}
