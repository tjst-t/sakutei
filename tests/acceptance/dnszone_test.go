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

// dnszone_test.go contains CLI-level and library acceptance tests for Story
// S62dbba-3: the DNS zone-file provider — the first real (non-fake) provider.
//
// Test discipline:
//   - CLI tests launch the real "sakutei" binary as a subprocess and inspect
//     exit code, stdout, stderr, and the zone-file contents.
//   - Library tests (ContractTester) verify the SDK contracts directly.
//
// AC tags (from scenario-S62dbba-3.json and 旧 ROADMAP.json（Backlog に移行済み）):
//
//	[AC-S62dbba-3-M]   machine-executable scenario tests
//	AC-S62dbba-3-1     dnszone Schema lint passes; ValidateProvider returns nil
//	AC-S62dbba-3-2     sakutei plan reads zone file, shows diff, does NOT write
//	AC-S62dbba-3-3     sakutei reconcile writes record; 2nd reconcile is no-op (idempotent)
//	AC-S62dbba-3-4     SDK ContractTester passes all three contracts on dnszone
//	AC-S62dbba-3-5     List returns all records; prune deletes orphaned record
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/dnszone"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// writeZoneFile writes a zone file with the given content to path.
func writeZoneFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileAtomic(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeZoneFile: %v", err)
	}
}

// readZoneFile reads the zone file at path and returns its content.
func readZoneFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readZoneFile: %v", err)
	}
	return string(data)
}

// writeDNSZoneIntent writes an intent file for a dnszone resource.
func writeDNSZoneIntent(t *testing.T, dir string, resources []map[string]any) {
	t.Helper()
	writeIntentFile(t, dir, map[string]any{
		"resources": resources,
	})
}

// dnszoneResource builds a dnszone intent resource map.
func dnszoneResource(id, name, rrtype, value, ttl, deletionPolicy string) map[string]any {
	fields := map[string]string{
		"name":  name,
		"type":  rrtype,
		"value": value,
	}
	if ttl != "" {
		fields["ttl"] = ttl
	}
	r := map[string]any{
		"provider": "dnszone",
		"id":       id,
		"fields":   fields,
	}
	if deletionPolicy != "" {
		r["deletion_policy"] = deletionPolicy
	}
	return r
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-3-1: Schema lint + ValidateProvider
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZone_ValidateProvider verifies that the dnszone provider passes
// sdk.ValidateProvider (ProviderVersion non-empty, Schema lint clean).
//
// [AC-S62dbba-3-1] [AC-S62dbba-3-M]
func TestDNSZone_ValidateProvider(t *testing.T) {
	// [AC-S62dbba-3-1]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")

	p := dnszone.New(zonePath)

	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-S62dbba-3-1] sdk.ValidateProvider returned error: %v", err)
	}
	t.Logf("[AC-S62dbba-3-1] PASS: ValidateProvider returned nil")
}

// TestDNSZone_SchemaIdentity verifies that the Schema declares two identity
// fields {name, type} and that they are not secret/computed.
//
// [AC-S62dbba-3-1] [AC-S62dbba-3-M]
func TestDNSZone_SchemaIdentity(t *testing.T) {
	// [AC-S62dbba-3-1]
	p := dnszone.New("/dev/null")
	schema := p.Schema()

	nameSpec, ok := schema.Fields["name"]
	if !ok {
		t.Fatal("[AC-S62dbba-3-1] schema missing 'name' field")
	}
	if !nameSpec.Identity {
		t.Error("[AC-S62dbba-3-1] 'name' field must be identity=true")
	}
	if nameSpec.Secret {
		t.Error("[AC-S62dbba-3-1] 'name' field must not be secret")
	}

	typeSpec, ok := schema.Fields["type"]
	if !ok {
		t.Fatal("[AC-S62dbba-3-1] schema missing 'type' field")
	}
	if !typeSpec.Identity {
		t.Error("[AC-S62dbba-3-1] 'type' field must be identity=true")
	}
	if typeSpec.Secret {
		t.Error("[AC-S62dbba-3-1] 'type' field must not be secret")
	}

	t.Logf("[AC-S62dbba-3-1] PASS: Schema identity = {name, type}")
}

// TestDNSZone_CapabilitiesPruneable verifies Capabilities.Pruneable is true
// and ProviderVersion is non-empty.
//
// [AC-S62dbba-3-1] [AC-S62dbba-3-M]
func TestDNSZone_CapabilitiesPruneable(t *testing.T) {
	// [AC-S62dbba-3-1]
	p := dnszone.New("/dev/null")
	caps := p.Capabilities()

	if !caps.Pruneable {
		t.Error("[AC-S62dbba-3-1] Capabilities.Pruneable must be true for DNS records")
	}
	if !caps.List {
		t.Error("[AC-S62dbba-3-1] Capabilities.List must be true")
	}
	if !caps.TrueObserve {
		t.Error("[AC-S62dbba-3-1] Capabilities.TrueObserve must be true")
	}
	if caps.ProviderVersion == "" {
		t.Error("[AC-S62dbba-3-1] Capabilities.ProviderVersion must not be empty")
	}
	t.Logf("[AC-S62dbba-3-1] PASS: Capabilities.Pruneable=true, ProviderVersion=%q", caps.ProviderVersion)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-3-2: plan reads zone file, shows diff, does NOT write
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZone_PlanShowsDiffDoesNotWrite verifies scenario-2:
// plan shows UPDATE diff and does NOT modify the zone file.
//
// [AC-S62dbba-3-2] [AC-S62dbba-3-M]
func TestDNSZone_PlanShowsDiffDoesNotWrite(t *testing.T) {
	// [AC-S62dbba-3-2]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Zone file: existing record with value 1.2.3.4.
	initialZone := `; Example zone
$ORIGIN example.com.
@	300 IN SOA ns1 admin 2024010101 3600 900 604800 300
@	300 IN NS ns1.example.com.
www.example.com. 300 IN A 1.2.3.4
`
	writeZoneFile(t, zonePath, initialZone)

	// Intent: desire value 9.9.9.9.
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "9.9.9.9", "300", ""),
	})

	// Run plan: expect UPDATE diff in stdout, no write to zone file.
	stdout, stderr, exitCode := runPlan(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-2] sakutei plan exited %d; stderr=%s stdout=%s", exitCode, stderr, stdout)
	}

	if !strings.Contains(stdout, "UPDATE") {
		t.Errorf("[AC-S62dbba-3-2] expected UPDATE in plan output; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "www.example.com.") {
		t.Errorf("[AC-S62dbba-3-2] expected resource name in plan output; got:\n%s", stdout)
	}

	// Zone file must be unchanged (plan must not call Apply).
	currentZone := readZoneFile(t, zonePath)
	if currentZone != initialZone {
		t.Errorf("[AC-S62dbba-3-2] zone file was modified by plan (must not Apply):\nbefore: %q\nafter:  %q",
			initialZone, currentZone)
	}
	t.Logf("[AC-S62dbba-3-2] PASS: plan showed UPDATE diff without modifying zone file")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-3-3: reconcile writes record; 2nd reconcile is idempotent
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZone_ReconcileWritesRecord verifies scenario-3:
// reconcile writes a DNS record and the 2nd reconcile is a no-op.
//
// [AC-S62dbba-3-3] [AC-S62dbba-3-M]
func TestDNSZone_ReconcileWritesRecord(t *testing.T) {
	// [AC-S62dbba-3-3]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Zone file: existing record with old value.
	initialZone := `; Example zone
www.example.com. 300 IN A 1.2.3.4
`
	writeZoneFile(t, zonePath, initialZone)

	// Intent: desire 9.9.9.9.
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "9.9.9.9", "300", ""),
	})

	// First reconcile: should update the record.
	stdout, stderr, exitCode := runReconcile(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-3] 1st reconcile exited %d; stderr=%s stdout=%s", exitCode, stderr, stdout)
	}
	if !strings.Contains(stdout, "change(s) applied") {
		t.Errorf("[AC-S62dbba-3-3] expected 'change(s) applied' in 1st reconcile output; got:\n%s", stdout)
	}

	// Zone file must now contain the new value.
	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "9.9.9.9") {
		t.Errorf("[AC-S62dbba-3-3] zone file does not contain '9.9.9.9' after reconcile:\n%s", zoneContent)
	}
	if strings.Contains(zoneContent, "1.2.3.4") {
		t.Errorf("[AC-S62dbba-3-3] zone file still contains old value '1.2.3.4' after reconcile:\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-3-3] zone file after 1st reconcile:\n%s", zoneContent)

	// Second reconcile: idempotent (no changes).
	stdout2, stderr2, exitCode2 := runReconcile(t, intentDir, zonePath)
	if exitCode2 != 0 {
		t.Fatalf("[AC-S62dbba-3-3] 2nd reconcile exited %d; stderr=%s stdout=%s", exitCode2, stderr2, stdout2)
	}
	if !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[AC-S62dbba-3-3] expected 'Converged (no changes)' on 2nd reconcile; got:\n%s", stdout2)
	}
	t.Logf("[AC-S62dbba-3-3] PASS: reconcile wrote record; 2nd reconcile was idempotent")
}

// TestDNSZone_ReconcileCreatesRecord verifies creating a new record from scratch
// (zone file with no existing record for this name/type).
//
// [AC-S62dbba-3-3] [AC-S62dbba-3-M]
func TestDNSZone_ReconcileCreatesRecord(t *testing.T) {
	// [AC-S62dbba-3-3]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Zone file: SOA/NS only, no A record yet.
	initialZone := `; Example zone
$ORIGIN example.com.
@	300 IN SOA ns1 admin 2024010101 3600 900 604800 300
@	300 IN NS ns1.example.com.
`
	writeZoneFile(t, zonePath, initialZone)

	// Intent: create mail.example.com. AAAA record.
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("mail.example.com./AAAA", "mail.example.com.", "AAAA", "2001:db8::1", "600", ""),
	})

	stdout, stderr, exitCode := runReconcile(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-3] reconcile exited %d; stderr=%s stdout=%s", exitCode, stderr, stdout)
	}
	if !strings.Contains(stdout, "change(s) applied") {
		t.Errorf("[AC-S62dbba-3-3] expected 'change(s) applied'; got:\n%s", stdout)
	}

	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "2001:db8::1") {
		t.Errorf("[AC-S62dbba-3-3] zone file does not contain new AAAA record:\n%s", zoneContent)
	}
	// SOA/NS must remain untouched.
	if !strings.Contains(zoneContent, "SOA") {
		t.Errorf("[AC-S62dbba-3-3] SOA record was removed from zone file:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, "NS") {
		t.Errorf("[AC-S62dbba-3-3] NS record was removed from zone file:\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-3-3] PASS: reconcile created AAAA record; SOA/NS preserved")
}

// TestDNSZone_ReconcileDriftRepair verifies that a drifted record is repaired.
// Scenario: reconcile sets value, then the zone file is edited externally
// (drift), then reconcile repairs it.
//
// [AC-S62dbba-3-3] [AC-S62dbba-3-M]
func TestDNSZone_ReconcileDriftRepair(t *testing.T) {
	// [AC-S62dbba-3-3]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Initial zone: record already at desired value.
	initialZone := `; Example zone
db.example.com. 300 IN A 10.0.0.1
`
	writeZoneFile(t, zonePath, initialZone)

	// Intent: desire 10.0.0.1.
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("db.example.com./A", "db.example.com.", "A", "10.0.0.1", "300", ""),
	})

	// First reconcile: no changes (already converged).
	stdout, _, exitCode := runReconcile(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-3] 1st reconcile (converged) exited %d; stdout=%s", exitCode, stdout)
	}
	if !strings.Contains(stdout, "Converged (no changes)") {
		t.Errorf("[AC-S62dbba-3-3] expected already converged; got:\n%s", stdout)
	}

	// Simulate drift: external edit changes the record.
	writeZoneFile(t, zonePath, `; Example zone
db.example.com. 300 IN A 192.168.0.99
`)

	// Second reconcile: detects drift and repairs.
	stdout2, stderr2, exitCode2 := runReconcile(t, intentDir, zonePath)
	if exitCode2 != 0 {
		t.Fatalf("[AC-S62dbba-3-3] drift-repair reconcile exited %d; stderr=%s stdout=%s", exitCode2, stderr2, stdout2)
	}
	if !strings.Contains(stdout2, "change(s) applied") {
		t.Errorf("[AC-S62dbba-3-3] expected drift to be repaired; got:\n%s", stdout2)
	}

	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "10.0.0.1") {
		t.Errorf("[AC-S62dbba-3-3] zone file does not contain repaired value '10.0.0.1':\n%s", zoneContent)
	}
	if strings.Contains(zoneContent, "192.168.0.99") {
		t.Errorf("[AC-S62dbba-3-3] zone file still contains drifted value '192.168.0.99':\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-3-3] PASS: drift repair worked")
}

// TestDNSZone_SameNameDifferentType verifies that {name, type} identity correctly
// distinguishes A and CNAME records with the same owner name.
//
// [AC-S62dbba-3-3] [AC-S62dbba-3-M]
func TestDNSZone_SameNameDifferentType(t *testing.T) {
	// [AC-S62dbba-3-3] — {name,type} identity discriminates A vs CNAME
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Zone: empty.
	writeZoneFile(t, zonePath, "; zone\n")

	// Intent: both A and TXT for the same owner name.
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("host.example.com./A", "host.example.com.", "A", "1.2.3.4", "300", ""),
		dnszoneResource("host.example.com./TXT", "host.example.com.", "TXT", `"v=spf1 ~all"`, "300", ""),
	})

	stdout, stderr, exitCode := runReconcile(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-3] reconcile exited %d; stderr=%s stdout=%s", exitCode, stderr, stdout)
	}

	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "host.example.com.") {
		t.Errorf("[AC-S62dbba-3-3] zone file missing host.example.com. records:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, " A ") {
		t.Errorf("[AC-S62dbba-3-3] zone file missing A record:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, " TXT ") {
		t.Errorf("[AC-S62dbba-3-3] zone file missing TXT record:\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-3-3] PASS: A and TXT for same name coexist (identity={name,type})")
}

// TestDNSZone_SOANSUntouched verifies that SOA and NS records are not managed
// and are preserved by reconcile.
//
// [AC-S62dbba-3-3] [AC-S62dbba-3-M]
func TestDNSZone_SOANSUntouched(t *testing.T) {
	// [AC-S62dbba-3-3]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	soa := "@	300 IN SOA ns1 admin 2024010101 3600 900 604800 300"
	ns := "@	300 IN NS ns1.example.com."
	initial := "; zone\n" + soa + "\n" + ns + "\n"
	writeZoneFile(t, zonePath, initial)

	// Intent: only an A record (no SOA/NS).
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("ftp.example.com./A", "ftp.example.com.", "A", "5.6.7.8", "300", ""),
	})

	_, _, exitCode := runReconcile(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-3] reconcile exited %d", exitCode)
	}

	zoneContent := readZoneFile(t, zonePath)
	if !strings.Contains(zoneContent, "SOA") {
		t.Errorf("[AC-S62dbba-3-3] SOA record disappeared after reconcile:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, "NS") {
		t.Errorf("[AC-S62dbba-3-3] NS record disappeared after reconcile:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, "5.6.7.8") {
		t.Errorf("[AC-S62dbba-3-3] A record not written:\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-3-3] PASS: SOA/NS preserved; A record written")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-3-4: SDK ContractTester passes all contracts on dnszone
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZoneContractTests runs the SDK ContractTester against the real
// dnszone provider.  This is the primary SDK proof-of-concept test: a real
// provider (not the fake) must satisfy the same contracts.
//
// [AC-S62dbba-3-4] [AC-S62dbba-3-M]
func TestDNSZoneContractTests(t *testing.T) {
	// [AC-S62dbba-3-4]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "contract-test.zone")

	p := dnszone.New(zonePath)

	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		SampleID: "contract.example.com./A",
		SampleDesired: map[string]string{
			"name":  "contract.example.com.",
			"type":  "A",
			"value": "1.1.1.1",
			"ttl":   "300",
		},
		Cleanup: func() error {
			// Remove zone file to clean up.
			return os.Remove(zonePath)
		},
	}

	ct.RunAll(t)
	t.Logf("[AC-S62dbba-3-4] PASS: ContractTester.RunAll completed on dnszone provider")
}

// TestDNSZoneContractTests_MultiRecord verifies contracts hold when multiple
// records are already present (List completeness checks that all returned IDs
// are readable).
//
// [AC-S62dbba-3-4] [AC-S62dbba-3-M]
func TestDNSZoneContractTests_MultiRecord(t *testing.T) {
	// [AC-S62dbba-3-4]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "multi-contract.zone")

	// Pre-seed the zone with some records.
	preExisting := `; multi-record zone
alpha.example.com. 300 IN A 10.0.0.1
beta.example.com. 300 IN A 10.0.0.2
`
	writeZoneFile(t, zonePath, preExisting)

	p := dnszone.New(zonePath)

	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		SampleID: "gamma.example.com./AAAA",
		SampleDesired: map[string]string{
			"name":  "gamma.example.com.",
			"type":  "AAAA",
			"value": "2001:db8::42",
			"ttl":   "600",
		},
	}

	ct.RunAll(t)
	t.Logf("[AC-S62dbba-3-4] PASS: ContractTester.RunAll with pre-existing records")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S62dbba-3-5: List returns all records; prune deletes orphaned record
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZone_PruneDeletesOrphan verifies scenario-5:
// when a record is in the zone file but absent from intent, reconcile prunes it.
//
// [AC-S62dbba-3-5] [AC-S62dbba-3-M]
func TestDNSZone_PruneDeletesOrphan(t *testing.T) {
	// [AC-S62dbba-3-5]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "example.com.zone")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Zone: two A records.
	initialZone := `; example zone
www.example.com. 300 IN A 1.2.3.4
old.example.com. 300 IN A 10.0.0.1
`
	writeZoneFile(t, zonePath, initialZone)

	// Intent: only www.example.com./A; old.example.com. is absent.
	// DeletionPolicy PruneWithinScope enables the prune engine.
	writeDNSZoneIntent(t, intentDir, []map[string]any{
		dnszoneResource("www.example.com./A", "www.example.com.", "A", "1.2.3.4", "300", "PruneWithinScope"),
	})

	stdout, stderr, exitCode := runReconcile(t, intentDir, zonePath)
	if exitCode != 0 {
		t.Fatalf("[AC-S62dbba-3-5] reconcile exited %d; stderr=%s stdout=%s", exitCode, stderr, stdout)
	}

	// Expect a DELETE for old.example.com.
	if !strings.Contains(stdout, "DELETE") {
		t.Errorf("[AC-S62dbba-3-5] expected DELETE in reconcile output; got:\n%s", stdout)
	}

	zoneContent := readZoneFile(t, zonePath)
	if strings.Contains(zoneContent, "old.example.com.") {
		t.Errorf("[AC-S62dbba-3-5] old.example.com. record was not pruned:\n%s", zoneContent)
	}
	if !strings.Contains(zoneContent, "www.example.com.") {
		t.Errorf("[AC-S62dbba-3-5] www.example.com. record was incorrectly removed:\n%s", zoneContent)
	}
	t.Logf("[AC-S62dbba-3-5] PASS: orphaned record pruned; desired record retained")
}

// TestDNSZone_ListReturnsAllRecords verifies that List returns all managed
// records (excluding SOA/NS) with ListComplete status.
//
// [AC-S62dbba-3-5] [AC-S62dbba-3-M]
func TestDNSZone_ListReturnsAllRecords(t *testing.T) {
	// [AC-S62dbba-3-5]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "list-test.zone")

	// Zone with SOA/NS + three managed records.
	content := `; zone
@	300 IN SOA ns1 admin 2024010101 3600 900 604800 300
@	300 IN NS ns1.example.com.
a.example.com. 300 IN A 1.1.1.1
b.example.com. 300 IN A 2.2.2.2
c.example.com. 300 IN AAAA 2001:db8::1
`
	writeZoneFile(t, zonePath, content)

	p := dnszone.New(zonePath)
	result, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-S62dbba-3-5] List returned error: %v", err)
	}
	if result.Status != core.ListComplete {
		t.Errorf("[AC-S62dbba-3-5] List status = %v, want ListComplete", result.Status)
	}
	if len(result.IDs) != 3 {
		t.Errorf("[AC-S62dbba-3-5] List returned %d IDs, want 3 (SOA/NS excluded):", len(result.IDs))
		for _, id := range result.IDs {
			t.Logf("  %s", id.Local)
		}
	}
	t.Logf("[AC-S62dbba-3-5] PASS: List returned %d records (SOA/NS excluded)", len(result.IDs))
}

// TestDNSZone_ListEmptyZone verifies that List on a non-existent zone file
// returns ListComplete with zero IDs (not an error).
//
// [AC-S62dbba-3-5] [AC-S62dbba-3-M]
func TestDNSZone_ListEmptyZone(t *testing.T) {
	// [AC-S62dbba-3-5]
	tmp := t.TempDir()
	zonePath := filepath.Join(tmp, "absent.zone")
	// Zone file does not exist.

	p := dnszone.New(zonePath)
	result, err := p.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-S62dbba-3-5] List on absent zone returned error: %v", err)
	}
	if result.Status != core.ListComplete {
		t.Errorf("[AC-S62dbba-3-5] expected ListComplete; got %v", result.Status)
	}
	if len(result.IDs) != 0 {
		t.Errorf("[AC-S62dbba-3-5] expected 0 IDs; got %d", len(result.IDs))
	}
	t.Logf("[AC-S62dbba-3-5] PASS: List on absent zone file returned ListComplete with 0 IDs")
}
