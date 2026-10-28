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

package registry

import (
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func TestNftablesRanges(t *testing.T) {
	entry := map[string]any{"scope": map[string]any{"site": "tokyo"}, "tables": []any{"ip/nat", "inet/filter"}}
	live := map[string]string{"ruleset_target": "live"}
	got, warnings, err := provider.MergeRanges("nftables", nftablesRanges{}, entry, live)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("intent only: %v %v", err, warnings)
	}
	if got["ruleset_scope"] != "site=tokyo" || got["ruleset_tables"] != "inet/filter,ip/nat" {
		t.Fatalf("intent only: %v", got)
	}
	// The same tables in another order agree; another set disagrees.
	both := map[string]string{"ruleset_target": "live", "ruleset_tables": "inet/filter, ip/nat"}
	if _, _, err := provider.MergeRanges("nftables", nftablesRanges{}, entry, both); err != nil {
		t.Fatalf("same tables: %v", err)
	}
	both["ruleset_tables"] = "inet/filter"
	if _, _, err := provider.MergeRanges("nftables", nftablesRanges{}, entry, both); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("other tables: %v", err)
	}
	// Tables on the file target are refused.
	if _, _, err := provider.MergeRanges("nftables", nftablesRanges{}, entry, map[string]string{"ruleset_target": "file"}); err == nil {
		t.Fatal("tables on the file target were accepted")
	}
	if _, _, err := provider.MergeRanges("nftables", nftablesRanges{}, map[string]any{"tables": []any{"bogus"}}, live); err == nil {
		t.Fatal("a bad table was accepted")
	}
}

func TestCheckIntentProviders(t *testing.T) {
	r := DefaultRegistry()
	if err := r.CheckIntentProviders(map[string]map[string]any{"dnszone": {}, "nftables": {}, "proxmox-vm": {}}); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckIntentProviders(map[string]map[string]any{"fake": {}}); err == nil {
		t.Fatal("a provider without range settings was accepted")
	}
	if err := r.CheckIntentProviders(map[string]map[string]any{"nosuch": {}}); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
}
