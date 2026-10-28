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

import (
	"path/filepath"
	"strings"
	"testing"
)

// The intent's providers: section (§7.7, #181) is kept as written; a
// provider declared in two files, and one in an inventory, are refused.
func TestLoadIntent_Providers(t *testing.T) {
	dir := t.TempDir()
	writeIntent(t, dir, "a.json", `{"providers": {"dnszone": {"scope": {"site": "tokyo"}}}}`)
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := in.Providers["dnszone"]["scope"].(map[string]any)
	if sc["site"] != "tokyo" {
		t.Fatalf("providers = %v", in.Providers)
	}

	writeIntent(t, dir, "b.json", `{"providers": {"dnszone": {}}}`)
	if _, err := LoadIntent(dir); err == nil || !strings.Contains(err.Error(), "declared in both") {
		t.Fatalf("a provider in two files: %v", err)
	}

	inv := t.TempDir()
	writeIntent(t, inv, "inventory.json", `{"providers": {"dnszone": {}}}`)
	if _, err := LoadResourcesFile(filepath.Join(inv, "inventory.json")); err == nil || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("providers in an inventory: %v", err)
	}
}

// The YAML form the docs show (§7.7) loads.
func TestLoadIntent_ProvidersYAML(t *testing.T) {
	dir := t.TempDir()
	writeIntent(t, dir, "p.yaml", `providers:
  nftables:
    scope:
      site: tokyo
    tables:
      - inet/filter
      - ip/nat
  proxmox-vm:
    clusters:
      lab:
        scope:
          site: tokyo
`)
	in, err := LoadIntent(dir)
	if err != nil {
		t.Fatal(err)
	}
	tables, _ := in.Providers["nftables"]["tables"].([]any)
	clusters, _ := in.Providers["proxmox-vm"]["clusters"].(map[string]any)
	if len(tables) != 2 || tables[1] != "ip/nat" || clusters["lab"] == nil {
		t.Fatalf("providers = %#v", in.Providers)
	}
}
