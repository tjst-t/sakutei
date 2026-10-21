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

// daemon_file_lock_test.go — #72: the daemon's concurrent Applies to one zone
// file and one ruleset file do not lose each other's changes.
//
// Test discipline: launches the REAL "sakutei" binary from the initial state
// of docs/trial/v1 (plus more records, so more Applies run at once) and reads
// its per-cycle log and the files it wrote.
//
// AC tags:
//
//	[AC-72-1]  cycle 1 launches every resource and every change stays in the
//	           files: cycle 2 and later report "converged (no changes)" and
//	           launch nothing again.
package acceptance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const lockTrialZone = `$ORIGIN example.com.
@	300 IN SOA ns1 admin 2026102101 3600 900 604800 300
@	300 IN NS ns1
www	300 IN A 192.0.2.10
`

const lockTrialRuleset = `table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;
        jump sakutei_allow
    }
    chain sakutei_allow {
        tcp dport 22 accept
    }
}
`

// [AC-72-1]
func TestDaemonFileLock_Cycle1LaunchesOnceAndConverges(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "fake-state.json")
	zonePath := filepath.Join(tmp, "example.com.zone")
	rulesetPath := filepath.Join(tmp, "firewall.rules")
	writeZoneFile(t, zonePath, lockTrialZone)
	writeRulesetFile(t, rulesetPath, lockTrialRuleset)

	// The trial intent (update www, create api, add 443 to sakutei_allow)
	// and more records in the same zone file.
	resources := []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "192.0.2.20", "300", ""),
		dnszoneResource("api.example.com./A", "api.example.com.", "A", "192.0.2.30", "300", ""),
		{
			"provider": "nftables",
			"id":       "inet/filter/sakutei_allow",
			"fields": map[string]string{
				"family": "inet", "table": "filter", "chain": "sakutei_allow",
				"rules": nftEncoded("tcp dport 22 accept", "tcp dport 443 accept"),
			},
		},
	}
	const extra = 8
	for i := 0; i < extra; i++ {
		name := fmt.Sprintf("h%d.example.com.", i)
		resources = append(resources, dnszoneResource(name+"/A", name, "A", fmt.Sprintf("192.0.2.%d", 100+i), "300", ""))
	}
	writeIntentFile(t, intentDir, map[string]any{"resources": resources})
	total := len(resources)

	cmd, stdout, stderr := startDaemonWithArgs(t, intentDir, statePath, "1s",
		"--zone", zonePath, "--ruleset", rulesetPath)
	defer func() { _ = cmd.Process.Kill() }()

	end := time.Now().Add(20 * time.Second)
	for !strings.Contains(stdout.String(), "cycle 4:") && time.Now().Before(end) {
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	out := stdout.String()
	if !strings.Contains(out, "cycle 4:") {
		t.Fatalf("[AC-72-1] the daemon did not reach cycle 4\nstdout=%s\nstderr=%s", out, stderr.String())
	}

	cycle := regexp.MustCompile(`(?m)^sakutei: cycle (\d+): (observed=.*)$`)
	for _, m := range cycle.FindAllStringSubmatch(out, -1) {
		switch m[1] {
		case "1":
			if want := fmt.Sprintf("launched=%d", total); !strings.Contains(m[2], want) {
				t.Errorf("[AC-72-1] cycle 1: %q, want %s", m[2], want)
			}
		case "2", "3", "4":
			if !strings.Contains(m[2], "converged (no changes)") {
				t.Errorf("[AC-72-1] cycle %s: %q, want converged (no changes) — a change of cycle 1 was lost", m[1], m[2])
			}
		}
	}

	zone := readZoneFile(t, zonePath)
	for _, want := range [][2]string{{"www", "192.0.2.20"}, {"api.example.com.", "192.0.2.30"}} {
		if !zoneHasA(zone, want[0], want[1]) {
			t.Errorf("[AC-72-1] zone lacks %s A %s:\n%s", want[0], want[1], zone)
		}
	}
	for i := 0; i < extra; i++ {
		if !zoneHasA(zone, fmt.Sprintf("h%d.example.com.", i), fmt.Sprintf("192.0.2.%d", 100+i)) {
			t.Errorf("[AC-72-1] zone lacks h%d:\n%s", i, zone)
		}
	}
	// One SOA raise (YYYYMMDDnn, #70) per Apply to the zone file: none was lost.
	y, mo, d := time.Now().UTC().Date()
	today := uint64(y)*1000000 + uint64(mo)*10000 + uint64(d)*100
	serial := uint64(2026102101)
	for i := 0; i < total-1; i++ { // every resource but the nftables one
		serial = max(serial+1, today)
	}
	if want := fmt.Sprintf("SOA ns1 admin %d ", serial); !strings.Contains(zone, want) {
		t.Errorf("[AC-72-1] SOA serial: want %q in\n%s", want, zone)
	}
	if b, err := os.ReadFile(rulesetPath); err != nil || !strings.Contains(string(b), "tcp dport 443 accept") {
		t.Errorf("[AC-72-1] ruleset lacks 443: %q (%v)", b, err)
	}
	if t.Failed() {
		t.Logf("stdout=%s\nstderr=%s", out, stderr.String())
	}
}
