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

// nftables_test.go contains library- and CLI-level acceptance tests for Story
// Sc7a647-1: the nftables ruleset-file provider — the SECOND real (non-fake)
// provider, and the first to exercise the Schema `ordered` flag.
//
// AC tags (from ROADMAP.json .sprints.Sc7a647.stories["Sc7a647-1"]):
//
//	AC-Sc7a647-1-1  ordered flag end-to-end: same rules in a different order
//	                produce a DIFF (reordering is a change); identical order → no diff.
//	AC-Sc7a647-1-2  Read parses the managed file without seeing desired
//	                (absent / unknown≠absent); Apply is atomic + idempotent.
//	AC-Sc7a647-1-3  SDK ContractTester.RunAll passes; List is complete-or-error;
//	                Capabilities honest (List=true, ProviderVersion set,
//	                Pruneable=false for the destructive chain type).
//	AC-Sc7a647-1-4  Registered in DefaultRegistry as "nftables"; drivable from
//	                the CLI; coexists with dnszone (independent convergence).
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/nftables"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
	"github.com/tjst-t/sakutei/pkg/sdk"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// writeRulesetFile writes a ruleset file with the given content to path.
func writeRulesetFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeRulesetFile: %v", err)
	}
}

// nftEncoded joins an ordered list of rules the way the provider encodes them
// into the single `rules` field string.
func nftEncoded(rules ...string) string {
	return strings.Join(rules, nftables.RuleSep)
}

// readNFTChain runs the provider's Read for a chain local ID and returns the
// observation.
func readNFTChain(t *testing.T, p *nftables.Provider, localID string) core.Observation {
	t.Helper()
	obs, err := p.Read(context.Background(), localID)
	if err != nil {
		t.Fatalf("Read(%q): %v", localID, err)
	}
	return obs
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-1-1: ordered flag end-to-end (reordering IS a diff)
// ─────────────────────────────────────────────────────────────────────────────

// TestNFTables_ReorderingIsADiff is the load-bearing demonstration of the
// `ordered` flag: two rulesets with the SAME rules in a DIFFERENT order must
// produce an Update diff, while the identical order produces no diff.
//
// The rules field is declared Ordered:true in the Schema; the provider encodes
// the ordered rule list as a single canonical string, so a permutation changes
// the observed value and the Schema-driven diff engine reports an Update.
//
// [AC-Sc7a647-1-1]
func TestNFTables_ReorderingIsADiff(t *testing.T) {
	// [AC-Sc7a647-1-1]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "reorder.rules")

	// Reality: chain input has three rules in order [A, B, C].
	writeRulesetFile(t, rulesetPath, `# managed
table ip filter {
	chain input {
		ip saddr 10.0.0.1 accept
		ip saddr 10.0.0.2 drop
		tcp dport 22 accept
	}
}
`)

	p := nftables.New(rulesetPath)
	e := diff.New(p.Schema())
	localID := "ip/filter/input"
	obs := readNFTChain(t, p, localID)
	if obs.Availability != core.AvailabilityOk || obs.State == nil {
		t.Fatalf("[AC-Sc7a647-1-1] expected Ok+non-nil state; got avail=%v state=%v", obs.Availability, obs.State)
	}

	id := core.NewID(nftables.ProviderName, core.NewScope(nil), localID)

	// Case 1: desired has the SAME order [A, B, C] → NO diff.
	sameOrder := diff.DesiredResource{
		ID: id,
		Fields: map[string]string{
			"family": "ip",
			"table":  "filter",
			"chain":  "input",
			"rules": nftEncoded(
				"ip saddr 10.0.0.1 accept",
				"ip saddr 10.0.0.2 drop",
				"tcp dport 22 accept",
			),
		},
	}
	planSame := e.Diff(obs, sameOrder, cause.CauseContext{})
	if len(planSame.Ops) != 0 {
		t.Errorf("[AC-Sc7a647-1-1] identical order must produce NO diff; got %d op(s): %+v",
			len(planSame.Ops), planSame.Ops)
	}

	// Case 2: desired has the SAME rules in a DIFFERENT order [B, A, C] → Update diff.
	reordered := diff.DesiredResource{
		ID: id,
		Fields: map[string]string{
			"family": "ip",
			"table":  "filter",
			"chain":  "input",
			"rules": nftEncoded(
				"ip saddr 10.0.0.2 drop", // swapped
				"ip saddr 10.0.0.1 accept",
				"tcp dport 22 accept",
			),
		},
	}
	planReorder := e.Diff(obs, reordered, cause.CauseContext{})
	if len(planReorder.Ops) != 1 || planReorder.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("[AC-Sc7a647-1-1] reordering the SAME rules must produce exactly one Update op; got %+v", planReorder.Ops)
	}
	// The diff must be on the `rules` field.
	foundRules := false
	for _, fd := range planReorder.Ops[0].Fields {
		if fd.Field == "rules" {
			foundRules = true
		}
	}
	if !foundRules {
		t.Errorf("[AC-Sc7a647-1-1] Update op must carry a `rules` FieldDiff; got %+v", planReorder.Ops[0].Fields)
	}
	t.Logf("[AC-Sc7a647-1-1] PASS: identical order → no diff; reordering the same rules → Update (ordered flag honored)")
}

// TestNFTables_OrderedSchemaFlag verifies the Schema declares the rules field
// with Ordered:true and the identity fields as {family, table, chain} (NOT the
// rule content), and that the provider passes ValidateProvider + Schema lint.
//
// [AC-Sc7a647-1-1] [AC-Sc7a647-1-3]
func TestNFTables_OrderedSchemaFlag(t *testing.T) {
	// [AC-Sc7a647-1-1]
	p := nftables.New("/dev/null")
	schema := p.Schema()

	rulesSpec, ok := schema.Fields["rules"]
	if !ok {
		t.Fatal("[AC-Sc7a647-1-1] schema missing 'rules' field")
	}
	if !rulesSpec.Ordered {
		t.Error("[AC-Sc7a647-1-1] 'rules' field must be Ordered=true (order is semantically significant)")
	}
	if rulesSpec.Identity {
		t.Error("[AC-Sc7a647-1-1] 'rules' field must NOT be identity (identity is the chain coordinate, not rule content)")
	}

	for _, f := range []string{"family", "table", "chain"} {
		spec, ok := schema.Fields[f]
		if !ok {
			t.Fatalf("[AC-Sc7a647-1-1] schema missing identity field %q", f)
		}
		if !spec.Identity {
			t.Errorf("[AC-Sc7a647-1-1] field %q must be identity=true", f)
		}
	}

	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-Sc7a647-1-3] ValidateProvider returned error: %v", err)
	}
	if err := sdk.LintSchema(schema); err != nil {
		t.Fatalf("[AC-Sc7a647-1-3] LintSchema returned error: %v", err)
	}
	t.Logf("[AC-Sc7a647-1-1] PASS: rules Ordered=true; identity={family,table,chain}; lint clean")
}

// TestDiff_OrderedFlagIsLoadBearing は `Ordered` flag が diff エンジンで実際に
// 効いていることを直接検証する（S c7a647a、schema.go の記述を真にする修正）。
//
// 同じ string-list フィールドを、片方 Ordered:true、片方 Ordered:false（+ListSep）
// で宣言し、純粋な並べ替えに対して:
//   - Ordered:true  → Update diff になる（full-replacement semantics）。
//   - Ordered:false → diff にならない（set 正規化: split→sort→join）。
//
// これにより flag が装飾でなく load-bearing であることを保証する。将来の
// set-valued provider（Ordered:false）は誤って並べ替えで diff しなくなる。
//
// [AC-Sc7a647-1-1]
func TestDiff_OrderedFlagIsLoadBearing(t *testing.T) {
	const sep = " ; "
	id := core.NewID("test", core.NewScope(nil), "res1")

	// observed reality: 集合 {A,B,C} を [A,B,C] の順で観測。
	obs := core.Observation{
		Availability: core.AvailabilityOk,
		State: core.State{
			"items": {Kind: core.FieldKindStringList, Value: strings.Join([]string{"A", "B", "C"}, sep)},
		},
	}
	// desired: 同じ集合を DIFFERENT な順 [B,A,C] で宣言（純粋な並べ替え）。
	desired := diff.DesiredResource{
		ID:     id,
		Fields: map[string]string{"items": strings.Join([]string{"B", "A", "C"}, sep)},
	}

	// Case 1: Ordered:true → 並べ替えは Update diff。
	orderedSchema := provider.Schema{Fields: map[string]provider.FieldSpec{
		"items": {Kind: core.FieldKindStringList, Ordered: true, ListSep: sep},
		"id":    {Kind: core.FieldKindString, Identity: true},
	}}
	planOrdered := diff.New(orderedSchema).Diff(obs, desired, cause.CauseContext{})
	if len(planOrdered.Ops) != 1 || planOrdered.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("[AC-Sc7a647-1-1] Ordered:true reorder must produce an Update diff; got %+v", planOrdered.Ops)
	}

	// Case 2: Ordered:false (+ListSep) → 並べ替えは diff にならない（set 正規化）。
	unorderedSchema := provider.Schema{Fields: map[string]provider.FieldSpec{
		"items": {Kind: core.FieldKindStringList, Ordered: false, ListSep: sep},
		"id":    {Kind: core.FieldKindString, Identity: true},
	}}
	planUnordered := diff.New(unorderedSchema).Diff(obs, desired, cause.CauseContext{})
	if len(planUnordered.Ops) != 0 {
		t.Fatalf("[AC-Sc7a647-1-1] Ordered:false reorder must NOT produce a diff (set-normalized); got %+v", planUnordered.Ops)
	}

	// Case 3: Ordered:false でも要素集合が実際に違えば diff になる（正規化が
	// 差分を隠さないことの確認）。
	desiredDifferent := diff.DesiredResource{
		ID:     id,
		Fields: map[string]string{"items": strings.Join([]string{"A", "B", "D"}, sep)},
	}
	planDifferent := diff.New(unorderedSchema).Diff(obs, desiredDifferent, cause.CauseContext{})
	if len(planDifferent.Ops) != 1 || planDifferent.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("[AC-Sc7a647-1-1] Ordered:false with a genuinely different set must diff; got %+v", planDifferent.Ops)
	}

	t.Logf("[AC-Sc7a647-1-1] PASS: Ordered:true reorder→diff; Ordered:false reorder→no diff; different set→diff")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-1-2: Read observation-state-less; Apply atomic + idempotent
// ─────────────────────────────────────────────────────────────────────────────

// TestNFTables_ReadAbsentAndUnknown verifies unknown≠absent semantics:
//   - a non-existent ruleset file → absent (Ok, nil State).
//   - an unparseable ruleset file → AvailabilityUnknown (transient).
//
// [AC-Sc7a647-1-2]
func TestNFTables_ReadAbsentAndUnknown(t *testing.T) {
	// [AC-Sc7a647-1-2]
	tmp := t.TempDir()

	// File does not exist → absent.
	absentPath := filepath.Join(tmp, "absent.rules")
	pAbsent := nftables.New(absentPath)
	obs := readNFTChain(t, pAbsent, "ip/filter/input")
	if obs.Availability != core.AvailabilityOk {
		t.Errorf("[AC-Sc7a647-1-2] absent file: availability = %v, want Ok", obs.Availability)
	}
	if obs.State != nil {
		t.Errorf("[AC-Sc7a647-1-2] absent file: State = %v, want nil (absent)", obs.State)
	}

	// Malformed file → AvailabilityUnknown (unknown≠absent), and an error.
	badPath := filepath.Join(tmp, "bad.rules")
	writeRulesetFile(t, badPath, "table ip\nchain input {\n") // chain outside table header missing brace context
	pBad := nftables.New(badPath)
	obsBad, err := pBad.Read(context.Background(), "ip/filter/input")
	if err == nil {
		t.Errorf("[AC-Sc7a647-1-2] malformed file: expected an error, got nil")
	}
	if obsBad.Availability != core.AvailabilityUnknown {
		t.Errorf("[AC-Sc7a647-1-2] malformed file: availability = %v, want Unknown (unknown≠absent)", obsBad.Availability)
	}
	t.Logf("[AC-Sc7a647-1-2] PASS: absent→Ok/nil; malformed→Unknown+error")
}

// TestNFTables_ApplyAtomicAndIdempotent verifies that Apply writes atomically
// (no truncated ruleset ever visible) and is idempotent (applying the same
// Create plan twice yields identical file content), and that Read after Apply
// reflects reality without ever consulting desired.
//
// [AC-Sc7a647-1-2]
func TestNFTables_ApplyAtomicAndIdempotent(t *testing.T) {
	// [AC-Sc7a647-1-2]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "atomic.rules")
	p := nftables.New(rulesetPath)

	localID := "inet/filter/forward"
	rulesEnc := nftEncoded("ct state established accept", "tcp dport 80 accept", "counter drop")
	plan := core.Plan{
		Ops: []core.Op{{
			Kind:   core.OpKindCreate,
			Target: core.NewID(nftables.ProviderName, core.NewScope(nil), localID),
			Cause:  core.CauseTagA,
			Fields: []core.FieldDiff{
				{Field: "family", From: "", To: "inet"},
				{Field: "table", From: "", To: "filter"},
				{Field: "chain", From: "", To: "forward"},
				{Field: "rules", From: "", To: rulesEnc},
			},
		}},
	}

	if err := p.Apply(context.Background(), plan); err != nil {
		t.Fatalf("[AC-Sc7a647-1-2] first Apply: %v", err)
	}
	content1, err := os.ReadFile(rulesetPath)
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-2] read after first Apply: %v", err)
	}

	// Second Apply of the same plan → identical file content (idempotent).
	if err := p.Apply(context.Background(), plan); err != nil {
		t.Fatalf("[AC-Sc7a647-1-2] second Apply: %v", err)
	}
	content2, err := os.ReadFile(rulesetPath)
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-2] read after second Apply: %v", err)
	}
	if string(content1) != string(content2) {
		t.Errorf("[AC-Sc7a647-1-2] Apply not idempotent:\nfirst:\n%s\nsecond:\n%s", content1, content2)
	}

	// Read after Apply reflects reality (order preserved).
	obs := readNFTChain(t, p, localID)
	if obs.State == nil {
		t.Fatal("[AC-Sc7a647-1-2] Read after Apply returned nil State")
	}
	gotRules := obs.State["rules"].Value.(string)
	if gotRules != rulesEnc {
		t.Errorf("[AC-Sc7a647-1-2] rule order not preserved:\ngot:  %q\nwant: %q", gotRules, rulesEnc)
	}

	// Atomic write: no leftover temp files in the directory.
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sakutei-nftables-") {
			t.Errorf("[AC-Sc7a647-1-2] leftover temp file %q (atomic write should rename it away)", e.Name())
		}
	}
	t.Logf("[AC-Sc7a647-1-2] PASS: atomic write + idempotent + order-preserving Read")
}

// TestNFTables_BaseChainIsPermanentError verifies that a base chain (one with a
// type/hook/policy declaration) is treated as a management-boundary violation:
// Read returns a PermanentError rather than silently mismanaging it, and the
// chain is excluded from List (analog to dnszone excluding SOA/NS).
//
// [AC-Sc7a647-1-2] [AC-Sc7a647-1-3]
func TestNFTables_BaseChainIsPermanentError(t *testing.T) {
	// [AC-Sc7a647-1-2]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "base.rules")
	writeRulesetFile(t, rulesetPath, `# managed
table ip filter {
	chain input {
		type filter hook input priority 0; policy drop;
		tcp dport 22 accept
	}
	chain custom {
		ip saddr 10.0.0.1 accept
	}
}
`)
	p := nftables.New(rulesetPath)

	// Reading the base chain must be a PermanentError.
	_, err := p.Read(context.Background(), "ip/filter/input")
	if err == nil {
		t.Fatal("[AC-Sc7a647-1-2] base chain Read should return an error")
	}
	if !sdk.IsPermanent(err) {
		t.Errorf("[AC-Sc7a647-1-2] base chain Read error should be Permanent; got %v", err)
	}

	// The regular chain is readable.
	obs := readNFTChain(t, p, "ip/filter/custom")
	if obs.State == nil {
		t.Error("[AC-Sc7a647-1-2] regular chain 'custom' should be readable")
	}

	// List excludes the base chain.
	res, lerr := p.List(context.Background(), core.NewScope(nil))
	if lerr != nil {
		t.Fatalf("[AC-Sc7a647-1-2] List: %v", lerr)
	}
	for _, id := range res.IDs {
		if id.Local == "ip/filter/input" {
			t.Errorf("[AC-Sc7a647-1-2] List must exclude base chain, but returned %q", id.Local)
		}
	}
	t.Logf("[AC-Sc7a647-1-2] PASS: base chain → PermanentError + excluded from List; regular chain managed")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-1-3: SDK ContractTester + List completeness + Capabilities honest
// ─────────────────────────────────────────────────────────────────────────────

// TestNFTablesContractTests runs the SDK ContractTester against the real
// nftables provider, proving it honors the SDK contracts (idempotency,
// absent→Create→Read with the pre-Apply Read guard, List completeness) just
// like dnszone does.
//
// [AC-Sc7a647-1-3]
func TestNFTablesContractTests(t *testing.T) {
	// [AC-Sc7a647-1-3]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "contract.rules")
	p := nftables.New(rulesetPath)

	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		SampleID: "ip/nat/postrouting_custom",
		SampleDesired: map[string]string{
			"family": "ip",
			"table":  "nat",
			"chain":  "postrouting_custom",
			"rules":  nftEncoded("ip saddr 10.0.0.0/24 masquerade", "counter accept"),
		},
		Cleanup: func() error {
			return os.Remove(rulesetPath)
		},
	}
	ct.RunAll(t)
	t.Logf("[AC-Sc7a647-1-3] PASS: ContractTester.RunAll completed on nftables provider")
}

// TestNFTablesContractTests_MultiChain verifies contracts hold when several
// chains already exist (List completeness checks all returned IDs are readable).
//
// [AC-Sc7a647-1-3]
func TestNFTablesContractTests_MultiChain(t *testing.T) {
	// [AC-Sc7a647-1-3]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "multi.rules")
	writeRulesetFile(t, rulesetPath, `# managed
table ip filter {
	chain custom_a {
		ip saddr 10.0.0.1 accept
	}
	chain custom_b {
		tcp dport 443 accept
		counter drop
	}
}
`)
	p := nftables.New(rulesetPath)

	ct := sdk.ContractTester{
		Provider: p,
		Lister:   p,
		Scope:    core.NewScope(nil),
		SampleID: "ip/filter/custom_c",
		SampleDesired: map[string]string{
			"family": "ip",
			"table":  "filter",
			"chain":  "custom_c",
			"rules":  nftEncoded("udp dport 53 accept"),
		},
	}
	ct.RunAll(t)
	t.Logf("[AC-Sc7a647-1-3] PASS: ContractTester.RunAll with pre-existing chains")
}

// TestNFTables_CapabilitiesHonest verifies the Capabilities are declared
// honestly: List=true, TrueObserve=true, ProviderVersion set, and Pruneable
// is FALSE because a firewall chain is a destructive type (ADR-0005).
//
// [AC-Sc7a647-1-3]
func TestNFTables_CapabilitiesHonest(t *testing.T) {
	// [AC-Sc7a647-1-3]
	p := nftables.New("/dev/null")
	caps := p.Capabilities()

	if !caps.List {
		t.Error("[AC-Sc7a647-1-3] Capabilities.List must be true")
	}
	if !caps.TrueObserve {
		t.Error("[AC-Sc7a647-1-3] Capabilities.TrueObserve must be true")
	}
	if caps.ProviderVersion == "" {
		t.Error("[AC-Sc7a647-1-3] Capabilities.ProviderVersion must not be empty")
	}
	if caps.Pruneable {
		t.Error("[AC-Sc7a647-1-3] Capabilities.Pruneable must be FALSE (firewall chain is a destructive type; ADR-0005)")
	}
	t.Logf("[AC-Sc7a647-1-3] PASS: List=true, TrueObserve=true, Pruneable=false, ProviderVersion=%q", caps.ProviderVersion)
}

// TestNFTables_ListCompleteOrError verifies List is complete-or-error:
//   - absent file → ListComplete + 0 IDs.
//   - parseable file → ListComplete with all regular chains.
//   - unparseable file → error (never a truncated ListComplete).
//
// [AC-Sc7a647-1-3]
func TestNFTables_ListCompleteOrError(t *testing.T) {
	// [AC-Sc7a647-1-3]
	tmp := t.TempDir()

	// Absent → ListComplete, 0 IDs.
	pAbsent := nftables.New(filepath.Join(tmp, "none.rules"))
	res, err := pAbsent.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-3] List absent: %v", err)
	}
	if res.Status != core.ListComplete || len(res.IDs) != 0 {
		t.Errorf("[AC-Sc7a647-1-3] absent: status=%v ids=%d, want Complete/0", res.Status, len(res.IDs))
	}

	// Parseable → ListComplete with two chains.
	okPath := filepath.Join(tmp, "ok.rules")
	writeRulesetFile(t, okPath, `table ip filter {
	chain a { ip saddr 1.1.1.1 accept
	}
	chain b { counter drop
	}
}
`)
	pOK := nftables.New(okPath)
	resOK, err := pOK.List(context.Background(), core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-3] List ok: %v", err)
	}
	if resOK.Status != core.ListComplete || len(resOK.IDs) != 2 {
		t.Errorf("[AC-Sc7a647-1-3] ok: status=%v ids=%d, want Complete/2", resOK.Status, len(resOK.IDs))
	}

	// Unparseable → error, never truncated ListComplete.
	badPath := filepath.Join(tmp, "bad.rules")
	writeRulesetFile(t, badPath, "chain input {\n") // chain outside any table
	pBad := nftables.New(badPath)
	if _, err := pBad.List(context.Background(), core.NewScope(nil)); err == nil {
		t.Error("[AC-Sc7a647-1-3] unparseable file: List should return an error (complete-or-error)")
	}
	t.Logf("[AC-Sc7a647-1-3] PASS: List complete-or-error honored")
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-Sc7a647-1-4: registered + drivable + coexists with dnszone
// ─────────────────────────────────────────────────────────────────────────────

// TestNFTables_ResolvableViaRegistry verifies the provider is registered under
// the name "nftables" in the DefaultRegistry and resolves to a valid provider.
//
// [AC-Sc7a647-1-4]
func TestNFTables_ResolvableViaRegistry(t *testing.T) {
	// [AC-Sc7a647-1-4]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "reg.rules")

	reg := registry.DefaultRegistry()
	if _, ok := reg.Lookup("nftables"); !ok {
		t.Fatal("[AC-Sc7a647-1-4] DefaultRegistry has no factory registered for name \"nftables\"")
	}

	// Resolve via the "state" alias (CLI passes --state for all providers).
	p, err := reg.Get("nftables", map[string]string{"state": rulesetPath})
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] registry.Get(\"nftables\"): %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] resolved provider failed ValidateProvider: %v", err)
	}
	// Also resolve via the dedicated "ruleset" key.
	if _, err := reg.Get("nftables", map[string]string{"ruleset": rulesetPath}); err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] registry.Get with ruleset key: %v", err)
	}
	t.Logf("[AC-Sc7a647-1-4] PASS: nftables resolvable via registry (state alias + ruleset key)")
}

// TestNFTables_CLIReconcileDrives verifies the provider is drivable from the
// real "sakutei" binary: reconcile creates a chain in the ruleset file, and a
// second reconcile is idempotent.
//
// [AC-Sc7a647-1-4]
func TestNFTables_CLIReconcileDrives(t *testing.T) {
	// [AC-Sc7a647-1-4]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "cli.rules")
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}

	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "nftables",
				"id":       "ip/filter/custom",
				"fields": map[string]string{
					"family": "ip",
					"table":  "filter",
					"chain":  "custom",
					"rules":  nftEncoded("ip saddr 10.0.0.1 accept", "counter drop"),
				},
			},
		},
	})

	// First reconcile: create the chain (--state carries the ruleset path).
	stdout, stderr, code := runReconcile(t, intentDir, rulesetPath)
	if code != 0 {
		t.Fatalf("[AC-Sc7a647-1-4] 1st reconcile exited %d; stderr=%s stdout=%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "change(s) applied") {
		t.Errorf("[AC-Sc7a647-1-4] expected 'change(s) applied'; got:\n%s", stdout)
	}
	content, _ := os.ReadFile(rulesetPath)
	if !strings.Contains(string(content), "chain custom") {
		t.Errorf("[AC-Sc7a647-1-4] ruleset file missing 'chain custom':\n%s", content)
	}

	// Second reconcile: idempotent.
	stdout2, stderr2, code2 := runReconcile(t, intentDir, rulesetPath)
	if code2 != 0 {
		t.Fatalf("[AC-Sc7a647-1-4] 2nd reconcile exited %d; stderr=%s stdout=%s", code2, stderr2, stdout2)
	}
	if !strings.Contains(stdout2, "Converged (no changes)") {
		t.Errorf("[AC-Sc7a647-1-4] expected 'Converged (no changes)' on 2nd reconcile; got:\n%s", stdout2)
	}
	t.Logf("[AC-Sc7a647-1-4] PASS: CLI reconcile created chain; 2nd reconcile idempotent")
}

// TestNFTables_CoexistsWithDNSZone verifies that nftables and dnszone providers
// coexist: each resolves independently from the same DefaultRegistry with its
// own config, and Observe/Apply on one does not touch the other's file.
//
// [AC-Sc7a647-1-4]
func TestNFTables_CoexistsWithDNSZone(t *testing.T) {
	// [AC-Sc7a647-1-4]
	tmp := t.TempDir()
	rulesetPath := filepath.Join(tmp, "fw.rules")
	zonePath := filepath.Join(tmp, "zone.db")

	reg := registry.DefaultRegistry()
	nftP, err := reg.Get("nftables", map[string]string{"ruleset": rulesetPath})
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] get nftables: %v", err)
	}
	dnsP, err := reg.Get("dnszone", map[string]string{"zone": zonePath})
	if err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] get dnszone: %v", err)
	}

	// Apply an nftables chain.
	nftPlan := core.Plan{Ops: []core.Op{{
		Kind:   core.OpKindCreate,
		Target: core.NewID("nftables", core.NewScope(nil), "ip/filter/custom"),
		Cause:  core.CauseTagA,
		Fields: []core.FieldDiff{
			{Field: "family", To: "ip"},
			{Field: "table", To: "filter"},
			{Field: "chain", To: "custom"},
			{Field: "rules", To: nftEncoded("counter accept")},
		},
	}}}
	if err := nftP.Apply(context.Background(), nftPlan); err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] nftables Apply: %v", err)
	}

	// Apply a DNS record.
	dnsPlan := core.Plan{Ops: []core.Op{{
		Kind:   core.OpKindCreate,
		Target: core.NewID("dnszone", core.NewScope(nil), "www.example.com./A"),
		Cause:  core.CauseTagA,
		Fields: []core.FieldDiff{
			{Field: "name", To: "www.example.com."},
			{Field: "type", To: "A"},
			{Field: "value", To: "1.2.3.4"},
			{Field: "ttl", To: "300"},
		},
	}}}
	if err := dnsP.Apply(context.Background(), dnsPlan); err != nil {
		t.Fatalf("[AC-Sc7a647-1-4] dnszone Apply: %v", err)
	}

	// Each file only holds its own provider's content (independent convergence).
	fw, _ := os.ReadFile(rulesetPath)
	zone, _ := os.ReadFile(zonePath)
	if !strings.Contains(string(fw), "chain custom") || strings.Contains(string(fw), "example.com") {
		t.Errorf("[AC-Sc7a647-1-4] nftables file cross-contaminated:\n%s", fw)
	}
	if !strings.Contains(string(zone), "www.example.com.") || strings.Contains(string(zone), "chain") {
		t.Errorf("[AC-Sc7a647-1-4] dnszone file cross-contaminated:\n%s", zone)
	}
	t.Logf("[AC-Sc7a647-1-4] PASS: nftables and dnszone coexist with independent files")
}
