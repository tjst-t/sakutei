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

// sdk_contract_generated_test.go — Backlog #32 (v1b): ContractTester generates
// its sample from the provider Schema (§28.3 / §28.6).  Authors pass only
// per-field Examples (for values the target validates) and, when the ID is not
// "identity fields joined with /", a MakeID function.
//
// Tags:
//   [#32-1] fake, dnszone and nftables pass RunAll without SampleDesired
//   [#32-2] generation rules: Computed/Managed skipped, Examples win, Kind
//           defaults, Secret → SecretRef form, default ID from identity fields
//   [#32-3] three-valued check: unreachable must not be reported as absent

package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/dnszone"
	"github.com/tjst-t/sakutei/v2/internal/fakeprovider"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// corruptFile replaces path with unparseable content and returns a restore
// function — the "target unreachable" stand-in for file-backed providers.
func corruptFile(path, garbage string) func() (func(), error) {
	return func() (func(), error) {
		orig, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := writeFileAtomic(path, []byte(garbage), 0644); err != nil {
			return nil, err
		}
		return func() { _ = writeFileAtomic(path, orig, 0644) }, nil
	}
}

func TestContractGenerated_Fake(t *testing.T) {
	// [#32-1] [#32-3]
	state := filepath.Join(t.TempDir(), "state.json")
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	p := fakeprovider.New(state)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		MakeUnreachable: func() (func(), error) {
			// The fake's observe-failure injection: mark the sample unknown.
			raw, err := os.ReadFile(state)
			if err != nil {
				return nil, err
			}
			var sf map[string]any
			if err := json.Unmarshal(raw, &sf); err != nil {
				return nil, err
			}
			sf["observe_unknown"] = map[string]any{"sdk-contract-name": "unreachable"}
			b, _ := json.Marshal(sf)
			if err := writeFileAtomic(state, b, 0600); err != nil {
				return nil, err
			}
			return func() { _ = writeFileAtomic(state, raw, 0600) }, nil
		},
	}
	ct.RunAll(t)
}

func TestContractGenerated_DNSZone(t *testing.T) {
	// [#32-1] [#32-3]
	zone := filepath.Join(t.TempDir(), "zone.db")
	p := dnszone.New(zone)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		// Only the fields dnszone validates; the ID (name/type) is the default.
		Examples: map[string]string{
			"name":  "contract.example.com.",
			"type":  "A",
			"value": "192.0.2.1",
			"ttl":   "300",
		},
		// Same corruption as the chaos suite: an over-long line makes the read
		// itself fail.  (A merely malformed line is skipped by dnszone today —
		// see Backlog #37.)
		MakeUnreachable: corruptFile(zone, strings.Repeat("x", 128*1024)),
	}
	ct.RunAll(t)
}

func TestContractGenerated_NFTables(t *testing.T) {
	// [#32-1] [#32-3]
	rules := filepath.Join(t.TempDir(), "fw.rules")
	// Every writable field gets a value, so the sample is a base chain (#173).
	p := nftables.New(rules)
	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		Examples: map[string]string{
			"family":   "ip",
			"table":    "filter",
			"chain":    "contract",
			"rules":    "tcp dport 22 accept",
			"type":     "filter",
			"hook":     "output",
			"priority": "0",
			"policy":   "accept",
		},
		MakeID: func(f map[string]string) string {
			return f["family"] + "/" + f["table"] + "/" + f["chain"]
		},
		// Same corruption as the chaos suite: a chain outside any table.
		MakeUnreachable: corruptFile(rules, "chain orphan {\n\tip saddr 10.9.9.9 accept\n}\n"),
	}
	// confirm (#179) says how to confirm a live change and is never observed:
	// the sample is the generated one without it.
	id, desired, err := ct.GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := desired[nftables.FieldConfirm]; !ok {
		t.Fatalf("the generated sample must hold every writable field, confirm included: %v", desired)
	}
	delete(desired, nftables.FieldConfirm)
	ct.SampleID, ct.SampleDesired = id, desired
	ct.RunAll(t)
}

func TestContractGenerated_GenerationRules(t *testing.T) {
	// [#32-2] secretProvider (import_test.go): name (identity, required),
	// password (secret), serial (computed).
	ct := sdk.ContractTester{Provider: &secretProvider{}, Examples: map[string]string{"name": "db-9"}}
	id, desired, err := ct.GenerateSample()
	if err != nil {
		t.Fatalf("[#32-2] GenerateSample: %v", err)
	}
	if id != "db-9" {
		t.Errorf("[#32-2] a single identity field gives the ID; got %q", id)
	}
	if desired["name"] != "db-9" {
		t.Errorf("[#32-2] Examples must win; got name=%q", desired["name"])
	}
	if _, ok := desired["serial"]; ok {
		t.Errorf("[#32-2] computed fields must not be generated; got %v", desired)
	}
	if !strings.HasPrefix(desired["password"], "secretref:") {
		t.Errorf("[#32-2] a secret field must get a SecretRef, never a plain value; got %q", desired["password"])
	}

	// Default for an Int field, and the default multi-field ID on the fake.
	fp := fakeprovider.New(filepath.Join(t.TempDir(), "s.json"))
	ct2 := sdk.ContractTester{Provider: fp}
	id2, d2, err := ct2.GenerateSample()
	if err != nil {
		t.Fatalf("[#32-2] GenerateSample(fake): %v", err)
	}
	if d2["size"] != "1" || d2["name"] != "sdk-contract-name" || id2 != "sdk-contract-name" {
		t.Errorf("[#32-2] fake defaults: id=%q desired=%v", id2, d2)
	}
}

// absentWhenUnreachable wraps the fake and, while "down", answers Read with
// Availability Ok and nil State — the unknown-as-absent bug.
type absentWhenUnreachable struct {
	*fakeprovider.Provider
	down bool
}

func (a *absentWhenUnreachable) Read(ctx context.Context, id string) (core.Observation, error) {
	if a.down {
		return core.Observation{Availability: core.AvailabilityOk}, nil
	}
	return a.Provider.Read(ctx, id)
}

func TestContractGenerated_ThreeValuedCatchesUnknownAsAbsent(t *testing.T) {
	// [#32-3]
	state := filepath.Join(t.TempDir(), "state.json")
	writeStateJSON(t, state, `{"resources": {}, "provider_version": "v-test"}`)
	p := &absentWhenUnreachable{Provider: fakeprovider.New(state)}
	rec := &recordTB{}
	ct := sdk.ContractTester{
		Provider: p,
		Scope:    core.NewScope(nil),
		MakeUnreachable: func() (func(), error) {
			p.down = true
			return func() { p.down = false }, nil
		},
	}
	ct.RunAll(rec)
	if !strings.Contains(rec.all(), "reported as absent") {
		t.Fatalf("[#32-3] the tester must catch unknown-as-absent; got:\n%s", rec.all())
	}
}
