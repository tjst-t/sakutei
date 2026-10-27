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

// scope_provider_test.go — Backlog #106 (Option A, §27.5): dnszone と nftables
// の List/BulkObserve は、設計どおり provider 設定の固定 scope（既定 root、
// --zone-scope / --ruleset-scope で変える）で返す。変更前は常に root scope を
// 返していたため、non-root scope を使う3つの仕組みが機能しなかった:
//
//   - Sync Mode（§24.5）: importer が id.Scope.Matches(syncScope) で絞るので、
//     sync scope が non-root だと常に 0 件になり、snapshot が書けなかった。
//   - PruneWithinScope（§12.2, #38）: prune engine が realID.Scope を
//     pruneScopes（PruneWithinScope を宣言した desired resource の scope）に
//     Matches するので、宣言が non-root scope だと orphan が root scope で
//     返ってきて inAnyScope が false になり、一度も削除されなかった。
//
// AC タグ:
//
//	[AC-106-1] dnszone.WithScope / nftables.WithScope: List・BulkObserve が
//	           返す ID の scope は、provider 構築時に渡した固定値になる
//	           （既定は root）。
//	[AC-106-2] --zone-scope を宣言した scope に合わせて設定すると、
//	           PruneWithinScope の orphan が削除される。合わせずに
//	           既定（root）のままだと、同じ orphan は削除されない
//	           （#106 の不具合そのものの再現）。
//	[AC-106-3] --ruleset-scope でも同じ（nftables）。
//	[AC-106-4] --zone-scope を sync scope に合わせて設定すると、Sync Mode が
//	           0 件ではなく実際のレコードを generated/ に書く。合わせないと
//	           0 件のまま（#106 の不具合そのものの再現）。
//	[AC-106-5] 不正な --zone-scope / --ruleset-scope は、--zone / --ruleset を
//	           受け付けるすべてのコマンドで起動時にエラーになる。
package acceptance

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/dnszone"
	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// ─────────────────────────────────────────────────────────────────────────────
// AC-106-1: WithScope is honoured by List and BulkObserve (unit level)
// ─────────────────────────────────────────────────────────────────────────────

func TestDNSZoneScope_ProviderTagsConfiguredScope(t *testing.T) {
	// [AC-106-1]
	ctx := context.Background()
	zone := filepath.Join(t.TempDir(), "example.com.zone")
	writeZoneFile(t, zone, "www.example.com. 300 IN A 1.2.3.4\n")

	tokyo := core.NewScope(map[string]string{"site": "tokyo"})

	// Default (no WithScope): List/BulkObserve tag with root scope.
	root := dnszone.New(zone)
	lr, err := root.List(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-106-1] List: %v", err)
	}
	if len(lr.IDs) != 1 || !lr.IDs[0].Scope.Equal(core.NewScope(nil)) {
		t.Fatalf("[AC-106-1] default List scope = %v, want root", lr.IDs)
	}

	// WithScope(tokyo): List/BulkObserve tag with the configured scope,
	// regardless of the scope parameter the caller passed in (ignored by
	// design: a provider instance's scope is fixed at construction, §27.5).
	p := dnszone.New(zone, dnszone.WithScope(tokyo))
	lr, err = p.List(ctx, core.NewScope(map[string]string{"site": "osaka"}))
	if err != nil {
		t.Fatalf("[AC-106-1] List: %v", err)
	}
	if len(lr.IDs) != 1 || !lr.IDs[0].Scope.Equal(tokyo) {
		t.Fatalf("[AC-106-1] List with WithScope(tokyo) = %v, want scope tokyo", lr.IDs)
	}

	bo, err := p.BulkObserve(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-106-1] BulkObserve: %v", err)
	}
	wantKey := core.NewID(dnszone.ProviderName, tokyo, "www.example.com./A").Key()
	if _, ok := bo.Observations[wantKey]; !ok {
		t.Fatalf("[AC-106-1] BulkObserve did not tag the configured scope; keys=%v", keysOf(bo.Observations))
	}
}

func TestNftablesScope_ProviderTagsConfiguredScope(t *testing.T) {
	// [AC-106-1] [AC-106-3]
	ctx := context.Background()
	rulesetPath := filepath.Join(t.TempDir(), "example.rules")
	writeRulesetFile(t, rulesetPath, `table ip filter {
	chain input {
		ip saddr 10.0.0.1 accept
	}
}
`)

	tokyo := core.NewScope(map[string]string{"site": "tokyo"})

	root := nftables.New(rulesetPath)
	lr, err := root.List(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-106-1] List: %v", err)
	}
	if len(lr.IDs) != 1 || !lr.IDs[0].Scope.Equal(core.NewScope(nil)) {
		t.Fatalf("[AC-106-1] default List scope = %v, want root", lr.IDs)
	}

	p := nftables.New(rulesetPath, nftables.WithScope(tokyo))
	lr, err = p.List(ctx, core.NewScope(map[string]string{"site": "osaka"}))
	if err != nil {
		t.Fatalf("[AC-106-1] List: %v", err)
	}
	if len(lr.IDs) != 1 || !lr.IDs[0].Scope.Equal(tokyo) {
		t.Fatalf("[AC-106-1] List with WithScope(tokyo) = %v, want scope tokyo", lr.IDs)
	}

	bo, err := p.BulkObserve(ctx, core.NewScope(nil))
	if err != nil {
		t.Fatalf("[AC-106-1] BulkObserve: %v", err)
	}
	wantKey := core.NewID(nftables.ProviderName, tokyo, "ip/filter/input").Key()
	if _, ok := bo.Observations[wantKey]; !ok {
		t.Fatalf("[AC-106-1] BulkObserve did not tag the configured scope; keys=%v", keysOf(bo.Observations))
	}
}

// keysOf returns the map keys of a BulkObservation.Observations map, for
// t.Fatalf diagnostics (the map's generic key type varies by import, so this
// is written against the concrete map type used above).
func keysOf[K comparable, V any](m map[K]V) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-106-2 / AC-106-3: PruneWithinScope with a non-root configured scope
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZoneScope_CLI_PruneRequiresMatchingScope reproduces the #106 bug
// (an orphan in a declared non-root PruneWithinScope scope is never pruned
// unless the provider is configured with the matching scope) and verifies the
// fix: with --zone-scope matching the declared scope, the orphan is deleted;
// without it (the provider's default root scope), the same orphan survives.
func TestDNSZoneScope_CLI_PruneRequiresMatchingScope(t *testing.T) {
	// [AC-106-2]
	newCase := func(t *testing.T) (intentDir, zone string) {
		t.Helper()
		tmp := t.TempDir()
		intentDir = filepath.Join(tmp, "intent")
		if err := os.MkdirAll(intentDir, 0755); err != nil {
			t.Fatal(err)
		}
		zone = filepath.Join(tmp, "example.com.zone")
		writeZoneFile(t, zone, "www.example.com. 300 IN A 1.2.3.4\norphan.example.com. 300 IN A 9.9.9.9\n")
		r := dnszoneResource("www.example.com./A", "www.example.com.", "A", "1.2.3.4", "300", "PruneWithinScope")
		r["scope"] = map[string]string{"site": "tokyo"}
		writeDNSZoneIntent(t, intentDir, []map[string]any{r})
		return intentDir, zone
	}

	// Without --zone-scope: provider defaults to root, so the orphan (listed
	// as root scope) never matches the tokyo PruneWithinScope boundary and is
	// NOT pruned — this is the #106 bug, reproduced here as a regression
	// guard (not a desired outcome; it demonstrates why the flag matters).
	intentDir, zone := newCase(t)
	stdout, stderr, code := runReconcile(t, intentDir, zone)
	if code != 0 {
		t.Fatalf("[AC-106-2] reconcile (no --zone-scope) exited %d: %s %s", code, stdout, stderr)
	}
	content := readZoneFile(t, zone)
	if !strings.Contains(content, "orphan.example.com.") {
		t.Fatalf("[AC-106-2] expected the orphan to SURVIVE without --zone-scope (reproducing #106); zone:\n%s", content)
	}

	// With --zone-scope matching the declared scope: the orphan IS pruned.
	intentDir, zone = newCase(t)
	stdout, stderr, code = runReconcile(t, intentDir, zone, "--zone-scope", "site=tokyo")
	if code != 0 {
		t.Fatalf("[AC-106-2] reconcile --zone-scope site=tokyo exited %d: %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "DELETE") {
		t.Errorf("[AC-106-2] expected DELETE in reconcile output with --zone-scope set; got:\n%s", stdout)
	}
	content = readZoneFile(t, zone)
	if strings.Contains(content, "orphan.example.com.") {
		t.Fatalf("[AC-106-2] the orphan was not pruned although --zone-scope matches the declared scope; zone:\n%s", content)
	}
	if !strings.Contains(content, "www.example.com.") {
		t.Fatalf("[AC-106-2] the desired record was incorrectly removed; zone:\n%s", content)
	}
}

// TestNftablesScope_PruneEngineSeesOrphanOnlyWithMatchingScope mirrors
// TestDNSZoneScope_CLI_PruneRequiresMatchingScope for nftables, at the
// internal/prune.Engine level (nftables' chain type is Pruneable:false by
// design — ADR-0005, destructive types default false — so no DeleteOp is
// ever issued for it; that gate is independent of #106 and is covered by
// internal/prune's own tests). What #106 affects is whether the orphan is
// seen as being INSIDE the declared PruneWithinScope boundary at all: without
// a matching --ruleset-scope, the orphan is listed as root scope and
// inAnyScope(root, [tokyo]) is false, so it is dropped before the pruneable
// check and never appears in Deferred either. With a matching scope it is in
// bounds and appears in Deferred (held back only by the type-level gate).
func TestNftablesScope_PruneEngineSeesOrphanOnlyWithMatchingScope(t *testing.T) {
	// [AC-106-3]
	ctx := context.Background()
	rulesetPath := filepath.Join(t.TempDir(), "cli.rules")
	writeRulesetFile(t, rulesetPath, `table ip filter {
	chain custom {
		ip saddr 10.0.0.1 accept
	}
	chain orphaned {
		counter drop
	}
}
`)
	tokyo := core.NewScope(map[string]string{"site": "tokyo"})
	desired := []diff.DesiredResource{{
		ID:             core.NewID(nftables.ProviderName, tokyo, "ip/filter/custom"),
		DeletionPolicy: core.PruneWithinScope,
	}}

	// Without WithScope (provider defaults to root): the orphan is invisible
	// to the tokyo PruneWithinScope boundary — not even Deferred.
	rootProvider := nftables.New(rulesetPath)
	result, err := prune.New().ComputePrune(ctx, rootProvider, rootProvider, core.NewScope(nil), core.NewScope(nil), desired, nil, io.Discard)
	if err != nil {
		t.Fatalf("[AC-106-3] ComputePrune (no WithScope): %v", err)
	}
	if len(result.DeleteOps) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("[AC-106-3] without a matching scope, the orphan must be invisible (0 DeleteOps, 0 Deferred); got DeleteOps=%v Deferred=%v (reproducing #106)", result.DeleteOps, result.Deferred)
	}

	// With WithScope(tokyo) matching the declared scope: the orphan is in
	// bounds and appears in Deferred (held back by Pruneable:false, not lost).
	scopedProvider := nftables.New(rulesetPath, nftables.WithScope(tokyo))
	result, err = prune.New().ComputePrune(ctx, scopedProvider, scopedProvider, core.NewScope(nil), core.NewScope(nil), desired, nil, io.Discard)
	if err != nil {
		t.Fatalf("[AC-106-3] ComputePrune (WithScope tokyo): %v", err)
	}
	if len(result.DeleteOps) != 0 {
		t.Errorf("[AC-106-3] nftables is Pruneable:false; expected 0 DeleteOps; got %v", result.DeleteOps)
	}
	if len(result.Deferred) != 1 || result.Deferred[0].Local != "ip/filter/orphaned" {
		t.Fatalf("[AC-106-3] with a matching scope, the orphan must appear in Deferred; got %v", result.Deferred)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-106-4: Sync Mode with a non-root configured scope
// ─────────────────────────────────────────────────────────────────────────────

// TestDNSZoneScope_CLI_SyncModeRequiresMatchingScope reproduces the #106 bug
// in Sync Mode (a sync scope that is non-root always records 0 resources
// unless the provider is configured with the matching scope) and verifies the
// fix.
func TestDNSZoneScope_CLI_SyncModeRequiresMatchingScope(t *testing.T) {
	// [AC-106-4]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zone := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zone, "www.example.com. 300 IN A 1.2.3.4\n")
	writeIntentFile(t, intentDir, map[string]any{
		"scopes": []map[string]any{
			{
				"scope": map[string]string{"site": "tokyo"},
				"mode":  "sync",
				"sync":  map[string]string{"target_path": "generated/tokyo/"},
			},
		},
	})

	// Without --zone-scope: the provider's List/BulkObserve always tag root
	// scope, so the importer's id.Scope.Matches(tokyo selector) is never
	// true → the snapshot records 0 resources (the #106 bug).
	out, _, code := runSakuteiSync(t, intentDir, "dnszone", zone, "")
	if code != 0 {
		t.Fatalf("[AC-106-4] sync (no --zone-scope) exited %d: %s", code, out)
	}
	snap := filepath.Join(tmp, "generated", "tokyo", "sync-dnszone.yaml")
	if _, err := os.Stat(snap); err == nil {
		body := readFile(t, snap)
		if strings.Contains(body, "www.example.com") {
			t.Fatalf("[AC-106-4] without --zone-scope, the snapshot must NOT contain the record (reproducing #106); got:\n%s", body)
		}
	}

	// With --zone-scope matching the sync scope: the record IS recorded.
	out, _, code = runSakuteiSync(t, intentDir, "dnszone", zone, "site=tokyo")
	if code != 0 {
		t.Fatalf("[AC-106-4] sync --zone-scope site=tokyo exited %d: %s", code, out)
	}
	body := readFile(t, snap)
	if !strings.Contains(body, "www.example.com") {
		t.Fatalf("[AC-106-4] with --zone-scope matching the sync scope, the snapshot must contain the record; got:\n%s", body)
	}
}

// runSakuteiSync runs "sakutei sync" for the dnszone provider against zone,
// optionally setting --zone-scope (zoneScope == "" omits the flag).
func runSakuteiSync(t *testing.T, intentDir, providerName, zone, zoneScope string) (stdout, stderr string, exitCode int) {
	t.Helper()
	args := []string{"sync", "--intent", intentDir, "--provider", providerName, "--zone", zone, "--samples", "1"}
	if zoneScope != "" {
		args = append(args, "--zone-scope", zoneScope)
	}
	out, code := runSakutei(t, args...)
	return out, "", code
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-106-5: invalid --zone-scope / --ruleset-scope is rejected at startup
// ─────────────────────────────────────────────────────────────────────────────

func TestZoneScope_CLI_InvalidFlagRejectedAtStartup(t *testing.T) {
	// [AC-106-5]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	zone := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zone, "www.example.com. 300 IN A 1.2.3.4\n")

	for _, args := range [][]string{
		{"plan", "--intent", intentDir, "--zone", zone},
		{"reconcile", "--intent", intentDir, "--zone", zone},
		{"status", "--intent", intentDir, "--zone", zone},
		{"import", "--provider", "dnszone", "--zone", zone, "--out", filepath.Join(tmp, "out")},
		{"sync", "--intent", intentDir, "--provider", "dnszone", "--zone", zone},
		{"--intent", intentDir, "--zone", zone, "--interval", "1h"}, // デーモン（#67）
	} {
		out, code := runSakutei(t, append(args, "--zone-scope", "not-a-key-value")...)
		if code == 0 || !strings.Contains(out, "--zone-scope") {
			t.Errorf("[AC-106-5] %v --zone-scope not-a-key-value: exit %d, output:\n%s", args, code, out)
		}
	}

	rulesetPath := filepath.Join(tmp, "cli.rules")
	for _, args := range [][]string{
		{"plan", "--intent", intentDir, "--ruleset", rulesetPath},
		{"reconcile", "--intent", intentDir, "--ruleset", rulesetPath},
		{"status", "--intent", intentDir, "--ruleset", rulesetPath},
		{"import", "--provider", "nftables", "--ruleset", rulesetPath, "--out", filepath.Join(tmp, "out2")},
		{"sync", "--intent", intentDir, "--provider", "nftables", "--ruleset", rulesetPath},
		{"--intent", intentDir, "--ruleset", rulesetPath, "--interval", "1h"},
	} {
		out, code := runSakutei(t, append(args, "--ruleset-scope", "not-a-key-value")...)
		if code == 0 || !strings.Contains(out, "--ruleset-scope") {
			t.Errorf("[AC-106-5] %v --ruleset-scope not-a-key-value: exit %d, output:\n%s", args, code, out)
		}
	}
}
