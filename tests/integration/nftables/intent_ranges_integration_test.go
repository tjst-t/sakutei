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

// #181（§7.7）：live 版の管理する表（ruleset_tables）を intent の
// providers.nftables.tables に書く。設定に表を書かずに intent だけで本物の nft に
// 収束し、設定と食い違えば nft を走らせる前に断り、設定だけなら警告して今どおり動く。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNFTablesLiveIntentRanges(t *testing.T) {
	dir := filepath.Join(lab.dir, "ranges")
	intent := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	config := func(name string, tables string) string {
		nf := map[string]any{"ruleset_target": "live", "ruleset_nft": lab.wrapper}
		if tables != "" {
			nf["ruleset_tables"] = tables
		}
		b, _ := json.Marshal(map[string]any{"providers": map[string]any{"nftables": nf}})
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	noTables, otherTables, sameTables := config("none.json", ""), config("other.json", "inet/sk"), config("same.json", "inet/ir")
	writeRanges := func(withProviders bool) {
		in := map[string]any{"resources": []resource{{Provider: "nftables", ID: "inet/ir/web", Fields: map[string]string{
			"family": "inet", "table": "ir", "chain": "web", "rules": "tcp dport 80 accept",
		}}}}
		if withProviders {
			in["providers"] = map[string]any{"nftables": map[string]any{"tables": []string{"inet/ir"}}}
		}
		b, _ := json.Marshal(in)
		if err := os.WriteFile(filepath.Join(intent, "fw.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(cfg string, args ...string) (string, int) {
		t.Helper()
		args = append(args, "--intent", intent, "--state", filepath.Join(dir, "fake.json"), "--provider-config", cfg)
		out, err := exec.Command(lab.bin, args...).CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}
	sk := nft(t, "list", "ruleset")

	writeRanges(true)
	t.Run("tables only in the intent", func(t *testing.T) {
		if out, code := run(noTables, "reconcile"); code != 0 || strings.Contains(out, "only in the provider config") {
			t.Fatalf("reconcile exit %d:\n%s", code, out)
		}
		if got := nft(t, "list", "chain", "inet", "ir", "web"); !strings.Contains(got, "tcp dport 80 accept") {
			t.Fatalf("chain inet/ir/web:\n%s", got)
		}
		if out, code := run(noTables, "reconcile"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
			t.Fatalf("second reconcile exit %d, want no changes:\n%s", code, out)
		}
	})
	t.Run("a disagreement is refused before nft runs", func(t *testing.T) {
		before := nft(t, "list", "ruleset")
		for _, verb := range []string{"plan", "reconcile"} {
			if out, code := run(otherTables, verb); code == 0 || !strings.Contains(out, "disagree") {
				t.Errorf("%s: exit %d, want a refusal:\n%s", verb, code, out)
			}
		}
		if after := nft(t, "list", "ruleset"); after != before {
			t.Fatalf("a refused run changed the ruleset:\n%s", after)
		}
	})
	t.Run("the same in both, and only in the config", func(t *testing.T) {
		if out, code := run(sameTables, "reconcile"); code != 0 || strings.Contains(out, "only in the provider config") {
			t.Fatalf("both: exit %d:\n%s", code, out)
		}
		writeRanges(false)
		if out, code := run(sameTables, "reconcile"); code != 0 || !strings.Contains(out, "only in the provider config") || !strings.Contains(out, "Converged (no changes)") {
			t.Fatalf("config only: exit %d, want a warning and no changes:\n%s", code, out)
		}
	})
	nft(t, "delete", "table", "inet", "ir")
	if got := nft(t, "list", "ruleset"); got != sk {
		t.Errorf("the other tables changed:\nbefore:\n%s\nafter:\n%s", sk, got)
	}
}
