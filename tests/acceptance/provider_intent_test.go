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

// provider_intent_test.go — Backlog #181: the range settings left in the
// provider config (the scope a provider instance tags its ids with, the
// tables it manages) are written in the intent's providers: section (§7.7).
// A setting only in the provider config is read as before with a warning; one
// in both must agree, or the run is refused at load time (never settled
// toward the weaker side).
//
// AC tags:
//
//	AC-181-1  dnszone: providers.dnszone.scope alone works like --zone-scope
//	          (the orphan in the declared scope is pruned), without a warning
//	AC-181-2  dnszone: --zone-scope only in the config warns and works as
//	          before; the same value in both is quiet
//	AC-181-3  a disagreement is refused by plan, reconcile and status, and
//	          skips the daemon's cycles (reality untouched); fixing the intent
//	          lets the daemon go on (the provider is rebuilt)
//	AC-181-4  nftables: providers.nftables.scope / tables against
//	          --ruleset-scope / --ruleset-tables (a disagreement is refused;
//	          tables on the file target are refused)
//	AC-181-5  proxmox-vm: providers.proxmox-vm.clusters.<name>.scope against
//	          the cluster list's scope (warn only-in-config, refuse a
//	          disagreement, refuse an unknown cluster)
//	AC-181-6  an unknown provider or key in providers: is refused
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const provIntentWarn = "is only in the provider config"

// provZoneRig writes a zone with www (declared, site=tokyo, PruneWithinScope)
// and an orphan, and an intent with the given top-level providers: member
// (empty: none).
func provZoneRig(t *testing.T, providers map[string]any) (intentDir, zone string) {
	t.Helper()
	tmp := t.TempDir()
	intentDir = filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	zone = filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zone, "www.example.com. 300 IN A 1.2.3.4\norphan.example.com. 300 IN A 9.9.9.9\n")
	writeProvZoneIntent(t, intentDir, providers)
	return intentDir, zone
}

func writeProvZoneIntent(t *testing.T, intentDir string, providers map[string]any) {
	t.Helper()
	r := dnszoneResource("www.example.com./A", "www.example.com.", "A", "1.2.3.4", "300", "PruneWithinScope")
	r["scope"] = map[string]string{"site": "tokyo"}
	in := map[string]any{"resources": []map[string]any{r}}
	if providers != nil {
		in["providers"] = providers
	}
	writeIntentFile(t, intentDir, in)
}

func zoneScope(site string) map[string]any {
	return map[string]any{"dnszone": map[string]any{"scope": map[string]any{"site": site}}}
}

func TestProviderIntent_DNSZoneScopeFromIntent(t *testing.T) {
	// [AC-181-1]
	intentDir, zone := provZoneRig(t, zoneScope("tokyo"))
	out, _, code := runReconcileCombined(t, intentDir, zone)
	if code != 0 {
		t.Fatalf("[AC-181-1] reconcile exit %d:\n%s", code, out)
	}
	if strings.Contains(out, provIntentWarn) {
		t.Errorf("[AC-181-1] a scope written only in the intent warns:\n%s", out)
	}
	if content := readZoneFile(t, zone); strings.Contains(content, "orphan.example.com.") || !strings.Contains(content, "www.example.com.") {
		t.Fatalf("[AC-181-1] with providers.dnszone.scope the orphan in the declared scope should be pruned; zone:\n%s", content)
	}
}

func TestProviderIntent_DNSZoneConfigOnlyWarns(t *testing.T) {
	// [AC-181-2]
	intentDir, zone := provZoneRig(t, nil)
	out, _, code := runReconcileCombined(t, intentDir, zone, "--zone-scope", "site=tokyo")
	if code != 0 {
		t.Fatalf("[AC-181-2] reconcile exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, provIntentWarn) || !strings.Contains(out, "providers.dnszone.scope") {
		t.Errorf("[AC-181-2] --zone-scope only in the config should warn, naming providers.dnszone.scope:\n%s", out)
	}
	if content := readZoneFile(t, zone); strings.Contains(content, "orphan.example.com.") {
		t.Fatalf("[AC-181-2] --zone-scope only in the config no longer works (the orphan survived); zone:\n%s", content)
	}

	// The same value in both: quiet, and works.
	intentDir, zone = provZoneRig(t, zoneScope("tokyo"))
	out, _, code = runReconcileCombined(t, intentDir, zone, "--zone-scope", "site=tokyo")
	if code != 0 || strings.Contains(out, provIntentWarn) {
		t.Fatalf("[AC-181-2] the same scope in both: exit %d, want 0 without a warning:\n%s", code, out)
	}
	if content := readZoneFile(t, zone); strings.Contains(content, "orphan.example.com.") {
		t.Fatalf("[AC-181-2] the same scope in both: the orphan survived; zone:\n%s", content)
	}
}

func TestProviderIntent_DisagreementRefused(t *testing.T) {
	// [AC-181-3]
	intentDir, zone := provZoneRig(t, zoneScope("tokyo"))
	before := readZoneFile(t, zone)
	for _, verb := range []string{"plan", "reconcile", "status"} {
		out, code := runSakutei(t, verb, "--intent", intentDir, "--zone", zone, "--zone-scope", "site=osaka")
		if code == 0 || !strings.Contains(out, "disagree") || !strings.Contains(out, "providers.dnszone.scope") {
			t.Errorf("[AC-181-3] %s with providers.dnszone.scope tokyo and --zone-scope osaka: exit %d, want a refusal:\n%s", verb, code, out)
		}
	}
	if got := readZoneFile(t, zone); got != before {
		t.Fatalf("[AC-181-3] a refused run changed the zone:\n%s", got)
	}

	// The daemon refuses to start while they disagree.
	if out, code := runSakutei(t, "--intent", intentDir, "--zone", zone, "--zone-scope", "site=osaka", "--interval", "200ms"); code == 0 || !strings.Contains(out, "disagree") {
		t.Errorf("[AC-181-3] the daemon started with a disagreeing intent: exit %d:\n%s", code, out)
	}

	// Started agreeing, the intent then changed to disagree: the cycles are
	// skipped, the zone untouched.
	writeProvZoneIntent(t, intentDir, zoneScope("osaka"))
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	cmd, wait := startDaemonWithFlags(t, logPath, "--intent", intentDir, "--zone", zone, "--zone-scope", "site=osaka", "--interval", "200ms")
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = wait() })
	waitForMoreCycles(t, logPath, 2)
	if countLog(logPath, "disagree") != 0 {
		t.Fatalf("[AC-181-3] the daemon refuses an agreeing intent; log:\n%s", readFileString(logPath))
	}
	writeProvZoneIntent(t, intentDir, zoneScope("tokyo"))
	waitForLog(t, logPath, "disagree", 2)
	if !strings.Contains(readFileString(logPath), "cycle skipped") {
		t.Errorf("[AC-181-3] the daemon does not say the cycle is skipped; log:\n%s", readFileString(logPath))
	}
	if got := readZoneFile(t, zone); got != before {
		t.Fatalf("[AC-181-3] the daemon changed the zone while the intent and the config disagree:\n%s", got)
	}

	// Fixed in the intent: the provider is rebuilt and the cycles go on.
	writeProvZoneIntent(t, intentDir, zoneScope("osaka"))
	waitForMoreCycles(t, logPath, 2)
	n := countLog(logPath, "disagree")
	waitForMoreCycles(t, logPath, 2)
	if countLog(logPath, "disagree") != n {
		t.Fatalf("[AC-181-3] the daemon still refuses after the intent was fixed; log:\n%s", readFileString(logPath))
	}
}

// waitForLog waits until substr appears at least n times in the log.
func waitForLog(t *testing.T, logPath, substr string, n int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if countLog(logPath, substr) >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%q did not appear %d times; log:\n%s", substr, n, readFileString(logPath))
}

func TestProviderIntent_Nftables(t *testing.T) {
	// [AC-181-4]
	newRig := func(t *testing.T, providers map[string]any) (intentDir, rules string) {
		t.Helper()
		tmp := t.TempDir()
		intentDir = filepath.Join(tmp, "intent")
		if err := os.MkdirAll(intentDir, 0o755); err != nil {
			t.Fatal(err)
		}
		rules = filepath.Join(tmp, "x.rules")
		writeRulesetFile(t, rules, "table ip filter {\n\tchain custom {\n\t\tip saddr 10.0.0.1 accept\n\t}\n}\n")
		in := map[string]any{"resources": []map[string]any{{
			"provider": "nftables", "id": "ip/filter/custom", "scope": map[string]string{"site": "tokyo"},
			"fields": map[string]string{"family": "ip", "table": "filter", "chain": "custom", "rules": nftEncoded("ip saddr 10.0.0.1 accept")},
		}}}
		if providers != nil {
			in["providers"] = providers
		}
		writeIntentFile(t, intentDir, in)
		return intentDir, rules
	}
	scope := map[string]any{"nftables": map[string]any{"scope": map[string]any{"site": "tokyo"}}}

	// Only in the intent: quiet.  Only in the config: a warning.  Both the
	// same: quiet.
	for name, c := range map[string]struct {
		providers map[string]any
		flags     []string
		warn      bool
	}{
		"intent only": {scope, nil, false},
		"config only": {nil, []string{"--ruleset-scope", "site=tokyo"}, true},
		"both":        {scope, []string{"--ruleset-scope", "site=tokyo"}, false},
	} {
		intentDir, rules := newRig(t, c.providers)
		out, code := runSakutei(t, append([]string{"plan", "--intent", intentDir, "--ruleset", rules}, c.flags...)...)
		if code != 0 || strings.Contains(out, provIntentWarn) != c.warn {
			t.Errorf("[AC-181-4] %s: plan exit %d, want 0 and warning=%v:\n%s", name, code, c.warn, out)
		}
	}

	// A disagreeing scope, and tables on the file target, are refused.
	intentDir, rules := newRig(t, scope)
	if out, code := runSakutei(t, "plan", "--intent", intentDir, "--ruleset", rules, "--ruleset-scope", "site=osaka"); code == 0 || !strings.Contains(out, "disagree") {
		t.Errorf("[AC-181-4] a disagreeing scope: exit %d, want a refusal:\n%s", code, out)
	}
	intentDir, rules = newRig(t, map[string]any{"nftables": map[string]any{"tables": []any{"ip/filter"}}})
	if out, code := runSakutei(t, "plan", "--intent", intentDir, "--ruleset", rules); code == 0 || !strings.Contains(out, "ruleset_target live") {
		t.Errorf("[AC-181-4] tables on the file target: exit %d, want a refusal:\n%s", code, out)
	}
	// With the live target the tables are compared before nft is ever run
	// (order does not matter; a different set disagrees).
	intentDir, rules = newRig(t, map[string]any{"nftables": map[string]any{"tables": []any{"inet/filter", "ip/nat"}}})
	live := []string{"--ruleset-target", "live", "--ruleset-nft", "/nonexistent/nft"}
	if out, code := runSakutei(t, append([]string{"plan", "--intent", intentDir, "--ruleset", rules, "--ruleset-tables", "inet/filter"}, live...)...); code == 0 || !strings.Contains(out, "disagree") || !strings.Contains(out, "providers.nftables.tables") {
		t.Errorf("[AC-181-4] disagreeing tables: exit %d, want a refusal:\n%s", code, out)
	}
	if out, _ := runSakutei(t, append([]string{"plan", "--intent", intentDir, "--ruleset", rules, "--ruleset-tables", "ip/nat,inet/filter"}, live...)...); strings.Contains(out, "disagree") || strings.Contains(out, provIntentWarn) {
		t.Errorf("[AC-181-4] the same tables in another order are reported as a disagreement:\n%s", out)
	}
}

func TestProviderIntent_ProxmoxVMClusterScope(t *testing.T) {
	// [AC-181-5]
	_, cfg := pveRig(t)
	bin := buildBinary(t)
	scoped := filepath.Join(filepath.Dir(cfg), "pve-scoped.yaml")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scoped, []byte(strings.Replace(string(body), "    node: pve1\n", "    node: pve1\n    scope: site=tokyo\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	res := `"resources": [{"provider": "proxmox-vm", "id": "lab/legacy", "scope": {"site": "tokyo"}, "fields": {"memory": "4096"}}], ` + pveStage0Owners + `]`
	clusterScope := func(site string) string {
		return `, "providers": {"proxmox-vm": {"clusters": {"lab": {"scope": {"site": "` + site + `"}}}}}`
	}

	// Only in the cluster list: a warning naming the intent's key.
	o, code := runBin(t, bin, append([]string{"plan"}, pveExtIntent(t, scoped, res)...)...)
	if code != 0 || !strings.Contains(o, provIntentWarn) || !strings.Contains(o, "providers.proxmox-vm.clusters.lab.scope") {
		t.Fatalf("[AC-181-5] the cluster scope only in the config: exit %d, want a warning:\n%s", code, o)
	}
	// Only in the intent, or the same in both: quiet.
	for name, c := range map[string]string{"intent only": cfg, "both": scoped} {
		o, code := runBin(t, bin, append([]string{"plan"}, pveExtIntent(t, c, res+clusterScope("tokyo"))...)...)
		if code != 0 || strings.Contains(o, "providers.proxmox-vm.clusters") {
			t.Errorf("[AC-181-5] %s: exit %d, want 0 without a range warning:\n%s", name, code, o)
		}
	}
	// A disagreement, and an unknown cluster, are refused.
	for _, verb := range []string{"plan", "reconcile"} {
		o, code := runBin(t, bin, append([]string{verb}, pveExtIntent(t, scoped, res+clusterScope("osaka"))...)...)
		if code == 0 || !strings.Contains(o, "disagree") {
			t.Errorf("[AC-181-5] %s with a disagreeing cluster scope: exit %d, want a refusal:\n%s", verb, code, o)
		}
	}
	unknown := `, "providers": {"proxmox-vm": {"clusters": {"nosuch": {"scope": {"site": "tokyo"}}}}}`
	if o, code := runBin(t, bin, append([]string{"plan"}, pveExtIntent(t, cfg, res+unknown)...)...); code == 0 || !strings.Contains(o, "no such cluster") {
		t.Errorf("[AC-181-5] an unknown cluster: exit %d, want a refusal:\n%s", code, o)
	}
}

func TestProviderIntent_UnknownRefused(t *testing.T) {
	// [AC-181-6]
	for name, c := range map[string]struct {
		providers map[string]any
		want      string
	}{
		"unknown provider":   {map[string]any{"dnszon": map[string]any{}}, "unknown provider"},
		"no range settings":  {map[string]any{"fake": map[string]any{}}, "takes no range settings"},
		"unknown key":        {map[string]any{"dnszone": map[string]any{"zone_scope": "site=tokyo"}}, `unknown key "zone_scope"`},
		"scope not a map":    {map[string]any{"dnszone": map[string]any{"scope": "site=tokyo"}}, "label map"},
		"provider not a map": {map[string]any{"dnszone": "site=tokyo"}, "want an object"},
	} {
		intentDir, zone := provZoneRig(t, c.providers)
		for _, verb := range []string{"plan", "reconcile"} {
			out, code := runSakutei(t, verb, "--intent", intentDir, "--zone", zone)
			if code == 0 || !strings.Contains(out, c.want) {
				t.Errorf("[AC-181-6] %s: %s exit %d, want a refusal naming %q:\n%s", name, verb, code, c.want, out)
			}
		}
	}
}
