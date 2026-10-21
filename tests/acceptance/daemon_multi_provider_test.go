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

// daemon_multi_provider_test.go — #67: the daemon converges every provider
// named in the intent, and plan/reconcile take --ruleset.
//
// Test discipline: launches the REAL "sakutei" binary and inspects each
// backend's own file (zone file, ruleset file, fake state file).
//
// AC tags:
//
//	[AC-67-1]  one intent with dnszone and nftables resources; the daemon
//	           (--zone, --ruleset) converges both, and a hand edit of either
//	           file is reverted by a later cycle.
//	[AC-67-2]  plan and reconcile accept --ruleset; a mixed fake+nftables
//	           intent keeps the fake state file and the ruleset file apart.
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const multiZone = `; #67 example zone
$ORIGIN example.com.
@	300 IN SOA ns1 admin 2026102101 3600 900 604800 300
@	300 IN NS ns1.example.com.
www.example.com. 300 IN A 192.0.2.1
`

const multiRuleset = `# managed by sakutei
table ip filter {
	chain input {
		tcp dport 22 accept
	}
}
`

// zoneHasA reports whether zone has an A record name → value.
func zoneHasA(zone, name, value string) bool {
	for _, line := range strings.Split(zone, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == name && f[len(f)-2] == "A" && f[len(f)-1] == value {
			return true
		}
	}
	return false
}

// waitForFile polls path until ok(content) holds or the deadline passes.
func waitForFile(path string, deadline time.Duration, ok func(string) bool) (string, bool) {
	end := time.Now().Add(deadline)
	var last string
	for time.Now().Before(end) {
		if b, err := os.ReadFile(path); err == nil {
			last = string(b)
			if ok(last) {
				return last, true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last, false
}

// [AC-67-1]
func TestDaemonMultiProvider_DNSZoneAndNFTablesConverge(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "fake-state.json")
	zonePath := filepath.Join(tmp, "example.com.zone")
	rulesetPath := filepath.Join(tmp, "fw.rules")
	writeZoneFile(t, zonePath, multiZone)
	writeRulesetFile(t, rulesetPath, multiRuleset)

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.10", "300", ""),
			{
				"provider": "nftables",
				"id":       "ip/filter/input",
				"fields": map[string]string{
					"family": "ip", "table": "filter", "chain": "input",
					"rules": nftEncoded("tcp dport 22 accept", "tcp dport 443 accept"),
				},
			},
		},
	})

	cmd, stdout, stderr := startDaemonWithArgs(t, intentDir, statePath, "100ms",
		"--zone", zonePath, "--ruleset", rulesetPath)
	defer func() { _ = cmd.Process.Kill() }()

	zoneConverged := func(z string) bool {
		return zoneHasA(z, "www.example.com.", "192.0.2.10") && !zoneHasA(z, "www.example.com.", "192.0.2.1")
	}
	rulesConverged := func(r string) bool {
		return strings.Contains(r, "tcp dport 22 accept") && strings.Contains(r, "tcp dport 443 accept")
	}

	// Both providers converge from the same daemon.
	if z, ok := waitForFile(zonePath, 15*time.Second, zoneConverged); !ok {
		t.Fatalf("[AC-67-1] zone file did not converge; zone=%q\nstdout=%s\nstderr=%s", z, stdout.String(), stderr.String())
	}
	if r, ok := waitForFile(rulesetPath, 15*time.Second, rulesConverged); !ok {
		t.Fatalf("[AC-67-1] ruleset file did not converge; ruleset=%q\nstdout=%s\nstderr=%s", r, stdout.String(), stderr.String())
	}

	// Hand edits of both files (drift) are reverted by a later cycle.
	writeZoneFile(t, zonePath, strings.Replace(multiZone, "192.0.2.1", "203.0.113.99", 1))
	writeRulesetFile(t, rulesetPath, multiRuleset)
	if z, ok := waitForFile(zonePath, 15*time.Second, zoneConverged); !ok {
		t.Fatalf("[AC-67-1] zone drift was not reverted; zone=%q\nstderr=%s", z, stderr.String())
	}
	if r, ok := waitForFile(rulesetPath, 15*time.Second, rulesConverged); !ok {
		t.Fatalf("[AC-67-1] ruleset drift was not reverted; ruleset=%q\nstderr=%s", r, stderr.String())
	}
	if !strings.Contains(stderr.String(), "[cause: B]") {
		t.Errorf("[AC-67-1] the hand edits should be reported as drift (cause B); stderr=%s", stderr.String())
	}

	// Each provider wrote only its own file.
	z := readZoneFile(t, zonePath)
	if strings.Contains(z, "chain") || strings.Contains(z, "{") {
		t.Errorf("[AC-67-1] the zone file contains ruleset content: %q", z)
	}
	if b, err := os.ReadFile(rulesetPath); err == nil && strings.Contains(string(b), "example.com") {
		t.Errorf("[AC-67-1] the ruleset file contains zone content: %q", b)
	}
	if b, err := os.ReadFile(statePath); err == nil &&
		(strings.Contains(string(b), "example.com") || strings.Contains(string(b), "dport")) {
		t.Errorf("[AC-67-1] the fake state file received dnszone/nftables content: %s", b)
	}
	if strings.Contains(stderr.String(), "ProviderUpgradeDiff") {
		t.Errorf("[AC-67-1] no ghost diff expected (each provider keeps its own provider_version); stderr=%s", stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("[AC-67-1] daemon exit: %v\nstderr=%s", err, stderr.String())
	}
}

// [AC-67-2]
func TestPlanReconcile_RulesetFlag(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "fake-state.json")
	rulesetPath := filepath.Join(tmp, "fw.rules")
	writeRulesetFile(t, rulesetPath, multiRuleset)
	seedState(t, statePath, map[string]map[string]any{})

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "fake-a",
				"fields":   map[string]string{"name": "fake-a", "size": "10"},
			},
			{
				"provider": "nftables",
				"id":       "ip/filter/input",
				"fields": map[string]string{
					"family": "ip", "table": "filter", "chain": "input",
					"rules": nftEncoded("tcp dport 22 accept", "tcp dport 443 accept"),
				},
			},
		},
	})

	out, errOut, code := runPlan(t, intentDir, statePath, "--ruleset", rulesetPath)
	if code != 0 {
		t.Fatalf("[AC-67-2] plan --ruleset exited %d; stderr=%s\nstdout=%s", code, errOut, out)
	}
	if !strings.Contains(out, "ip/filter/input") || !strings.Contains(out, "fake-a") {
		t.Errorf("[AC-67-2] plan should show both providers' changes; stdout=%s", out)
	}
	if b := readZoneFile(t, rulesetPath); b != multiRuleset {
		t.Errorf("[AC-67-2] plan must not touch the ruleset file; got %q", b)
	}

	out, errOut, code = runReconcile(t, intentDir, statePath, "--ruleset", rulesetPath)
	if code != 0 {
		t.Fatalf("[AC-67-2] reconcile --ruleset exited %d; stderr=%s\nstdout=%s", code, errOut, out)
	}
	rules := readZoneFile(t, rulesetPath)
	if !strings.Contains(rules, "tcp dport 443 accept") {
		t.Errorf("[AC-67-2] reconcile did not converge the ruleset file; got %q", rules)
	}
	if strings.Contains(rules, "fake-a") {
		t.Errorf("[AC-67-2] the ruleset file received fake content: %q", rules)
	}
	if !resourceExistsInState(t, statePath, "fake-a") {
		t.Errorf("[AC-67-2] reconcile did not create fake-a in the fake state file")
	}
	if b, err := os.ReadFile(statePath); err == nil && strings.Contains(string(b), "dport") {
		t.Errorf("[AC-67-2] the fake state file received ruleset content: %s", b)
	}
}
