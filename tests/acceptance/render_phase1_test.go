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

// render_phase1_test.go contains CLI-level acceptance tests for Story S072a14-2.
//
// Test discipline: all tests launch the real "sakutei" binary as a subprocess
// and verify only observable CLI behaviour (stdout, stderr, exit code, file
// system state).  No internal package is imported directly.
//
// The tests cover the three key properties introduced by S072a14-2:
//
//  1. External input hash → rendered_revision: passing --inventory-hash makes
//     rendered_revision diverge from intent_revision.  Changing the hash changes
//     rendered_revision; keeping it constant keeps rendered_revision stable.
//
//  2. TTL cache stability: two plan invocations with the same --inventory-hash
//     produce the same rendered_revision (deterministic hash, no time/random
//     influence).  This validates that "TTL hit/miss" cannot change the hash
//     (§9.3 constraint: rendered_revision must not fluctuate).
//
//  3. Last-value-on-unreachable (§9.2) — THE MOST IMPORTANT SAFETY PROPERTY:
//     When the external resolver is unreachable and no prior cached value
//     exists (first call), the binary exits 1 (halt-safe) rather than
//     producing an empty desired and deleting everything.
//     When a prior cached value exists (simulated via the in-process cache in
//     unit-style test), the prior value is returned instead of erroring out.
//
//  4. Backward compatibility: existing resources using no --inventory-hash flag
//     still produce rendered_revision == intent_revision (v1a invariant preserved).
//
// AC tags:
//
//	AC-S072a14-2-1  rendered_revision diverges from intent_revision when external input is present
//	AC-S072a14-2-2  Changing external input hash changes rendered_revision; intent_revision stays the same
//	AC-S072a14-2-3  Resolver unreachable (first call): exit 1 (halt-safe); no empty desired produced
//	[AC-S072a14-2-M] marks the machine-executable scenario tests required by scenario-S072a14-2.json.
package acceptance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/render"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// runPlanPhase1 runs "sakutei plan" with render phase 1 flags.
// Returns stdout, stderr (always captured), and exit code.
func runPlanPhase1(t *testing.T, intentDir, statePath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	bin := buildBinary(t)
	args := []string{"plan", "--intent", intentDir, "--state", statePath}
	args = append(args, extraArgs...)
	cmd := exec.Command(bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("runPlanPhase1: unexpected error: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// writeOneResourceIntent writes a single plain fake resource into intentDir.
func writeOneResourceIntent(t *testing.T, intentDir string) {
	t.Helper()
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{
			{
				"provider": "fake",
				"id":       "phase1-vm",
				"fields": map[string]string{
					"name": "phase1-vm",
					"size": "4",
				},
			},
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S072a14-2-1: rendered_revision diverges when external input present
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderPhase1_RenderedDivergFromIntent verifies scenario-S072a14-2 step 1.
//
// When --inventory-hash is provided, rendered_revision MUST differ from
// intent_revision.  Two consecutive plan invocations with the same hash
// produce the same rendered_revision (TTL / determinism guarantee).
//
// [AC-S072a14-2-1] [AC-S072a14-2-M]
func TestRenderPhase1_RenderedDivergFromIntent(t *testing.T) {
	// [AC-S072a14-2-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeOneResourceIntent(t, intentDir)

	const h1 = "aaabbbccc111222333"

	// First run with --inventory-hash=H1.
	stdout1, stderr1, code1 := runPlanPhase1(t, intentDir, statePath, "--inventory-hash", h1)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-2-1] first plan exit code=%d; stderr=%s; stdout=%s", code1, stderr1, stdout1)
	}
	intentRev1, renderedRev1 := extractRevisions(stdout1)
	if intentRev1 == "" || renderedRev1 == "" {
		t.Fatalf("[AC-S072a14-2-1] revision header missing from first plan output:\n%s", stdout1)
	}

	// rendered_revision MUST differ from intent_revision when external input is present.
	if intentRev1 == renderedRev1 {
		t.Errorf("[AC-S072a14-2-1] rendered_revision should differ from intent_revision when --inventory-hash is set; both = %s", intentRev1)
	}

	// Second run with same hash: rendered_revision must be identical (determinism / TTL stability).
	stdout2, stderr2, code2 := runPlanPhase1(t, intentDir, statePath, "--inventory-hash", h1)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-2-1] second plan exit code=%d; stderr=%s; stdout=%s", code2, stderr2, stdout2)
	}
	_, renderedRev2 := extractRevisions(stdout2)
	if renderedRev1 != renderedRev2 {
		t.Errorf("[AC-S072a14-2-1] rendered_revision should be identical between runs with same hash; first=%s second=%s",
			renderedRev1, renderedRev2)
	}
	// intent_revision must be the same in both runs (intent unchanged).
	intentRev2, _ := extractRevisions(stdout2)
	if intentRev1 != intentRev2 {
		t.Errorf("[AC-S072a14-2-1] intent_revision changed unexpectedly between runs; first=%s second=%s", intentRev1, intentRev2)
	}

	t.Logf("[AC-S072a14-2-1] intent_revision=%s (same both runs)", intentRev1)
	t.Logf("[AC-S072a14-2-1] rendered_revision=%s (same both runs, diverged from intent)", renderedRev1)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S072a14-2-2: changing external input changes rendered_revision, not intent
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderPhase1_ExternalInputChangeChangesRendered verifies scenario step 2.
//
// Switching from hash H1 to H2 (without changing intent) MUST cause
// rendered_revision to change while intent_revision remains unchanged.
//
// [AC-S072a14-2-2] [AC-S072a14-2-M]
func TestRenderPhase1_ExternalInputChangeChangesRendered(t *testing.T) {
	// [AC-S072a14-2-2]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeOneResourceIntent(t, intentDir)

	const h1 = "hash-h1-aaa"
	const h2 = "hash-h2-bbb"

	// Run with H1.
	stdout1, stderr1, code1 := runPlanPhase1(t, intentDir, statePath, "--inventory-hash", h1)
	if code1 != 0 {
		t.Fatalf("[AC-S072a14-2-2] plan H1 exit code=%d; stderr=%s", code1, stderr1)
	}
	intentRev1, renderedRev1 := extractRevisions(stdout1)
	if intentRev1 == "" || renderedRev1 == "" {
		t.Fatalf("[AC-S072a14-2-2] revision header missing (H1 run):\n%s", stdout1)
	}

	// Run with H2 — intent unchanged.
	stdout2, stderr2, code2 := runPlanPhase1(t, intentDir, statePath, "--inventory-hash", h2)
	if code2 != 0 {
		t.Fatalf("[AC-S072a14-2-2] plan H2 exit code=%d; stderr=%s", code2, stderr2)
	}
	intentRev2, renderedRev2 := extractRevisions(stdout2)
	if intentRev2 == "" || renderedRev2 == "" {
		t.Fatalf("[AC-S072a14-2-2] revision header missing (H2 run):\n%s", stdout2)
	}

	// intent_revision must NOT change (intent files are unchanged).
	if intentRev1 != intentRev2 {
		t.Errorf("[AC-S072a14-2-2] intent_revision changed when only external input changed; H1=%s H2=%s",
			intentRev1, intentRev2)
	}

	// rendered_revision MUST change (external input changed).
	if renderedRev1 == renderedRev2 {
		t.Errorf("[AC-S072a14-2-2] rendered_revision did not change when external input hash changed H1→H2; both=%s", renderedRev1)
	}

	// Both rendered revisions must differ from intent_revision.
	if intentRev1 == renderedRev1 {
		t.Errorf("[AC-S072a14-2-2] rendered_revision (H1) should differ from intent_revision; both=%s", intentRev1)
	}
	if intentRev2 == renderedRev2 {
		t.Errorf("[AC-S072a14-2-2] rendered_revision (H2) should differ from intent_revision; both=%s", intentRev2)
	}

	t.Logf("[AC-S072a14-2-2] intent_revision=%s (unchanged)", intentRev1)
	t.Logf("[AC-S072a14-2-2] rendered_revision H1=%s → H2=%s (changed)", renderedRev1, renderedRev2)
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S072a14-2-3: resolver unreachable (first call) → halt-safe (exit 1)
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderPhase1_FirstCallUnreachableIsHaltSafe verifies scenario step 3.
//
// When the ExternalInputResolver returns an error on the FIRST call (no prior
// cached value exists), the binary MUST exit 1 (halt-safe) rather than
// producing an empty desired or proceeding with convergence.
//
// This is the most important safety property of S072a14-2: §9.2 states that
// "前回値が無い初回不達は安全側（収束を進めない/halt 寄り）", meaning the
// first-call failure must prevent any convergence action (no deletes).
//
// The test verifies:
//   - exit code 1 (convergence halted)
//   - stderr contains "ExternalInputUnreachable" (event logged)
//   - the fake provider state file (reality) is unchanged (no deletions)
//
// [AC-S072a14-2-3] [AC-S072a14-2-M]
func TestRenderPhase1_FirstCallUnreachableIsHaltSafe(t *testing.T) {
	// [AC-S072a14-2-3]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")

	// Seed reality with one resource so we can verify it is NOT deleted.
	seedState(t, statePath, map[string]map[string]any{
		"phase1-vm": {
			"name": "phase1-vm",
			"size": "4",
		},
	})

	// Write intent for the same resource.
	writeOneResourceIntent(t, intentDir)

	// Run plan with --inventory-unreachable (first call, no prior cached value).
	stdout, stderr, code := runPlanPhase1(t, intentDir, statePath, "--inventory-unreachable")

	// MUST exit 1 (halt-safe): convergence must NOT proceed.
	if code == 0 {
		t.Errorf("[AC-S072a14-2-3] expected exit 1 (halt-safe), got 0; stdout=%s stderr=%s", stdout, stderr)
	}

	// Stderr must contain ExternalInputUnreachable event (§9.2 ログに出る).
	if !strings.Contains(stderr, "ExternalInputUnreachable") {
		t.Errorf("[AC-S072a14-2-3] expected ExternalInputUnreachable in stderr; got:\n%s", stderr)
	}

	// The state file (reality) must be unchanged — phase1-vm must still exist.
	// plan never calls Apply, but this verifies the intent was not processed
	// (no empty desired produced, no change triggered).
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("[AC-S072a14-2-3] read state file: %v", err)
	}
	if !strings.Contains(string(stateBytes), "phase1-vm") {
		t.Errorf("[AC-S072a14-2-3] state file no longer contains phase1-vm after unreachable plan; file:\n%s", stateBytes)
	}

	t.Logf("[AC-S072a14-2-3] plan exit=%d (halt-safe confirmed); stderr excerpt: %s",
		code, firstLine(stderr))
}

// ─────────────────────────────────────────────────────────────────────────────
// Backward compatibility: no --inventory-hash → rendered == intent (v1a)
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderPhase1_BackwardCompatNoHash verifies that the existing v1a invariant
// (rendered_revision == intent_revision) is preserved when no --inventory-hash
// flag is provided.  This ensures S072a14-2 does not break existing tests.
//
// [AC-S072a14-2-M]
func TestRenderPhase1_BackwardCompatNoHash(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeOneResourceIntent(t, intentDir)

	// No --inventory-hash flag: v1a behaviour.
	stdout, stderr, code := runPlanPhase1(t, intentDir, statePath)
	if code != 0 {
		t.Fatalf("backward compat: plan exit code=%d; stderr=%s", code, stderr)
	}
	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev == "" || renderedRev == "" {
		t.Fatalf("backward compat: revision header missing:\n%s", stdout)
	}
	if intentRev != renderedRev {
		t.Errorf("backward compat: v1a invariant broken (no --inventory-hash): intent=%s rendered=%s",
			intentRev, renderedRev)
	}
	t.Logf("backward compat: v1a invariant confirmed (no hash): %s", intentRev)
}

// ─────────────────────────────────────────────────────────────────────────────
// Unit-style: ResolverCache last-value-on-unreachable (in-process)
// ─────────────────────────────────────────────────────────────────────────────

// TestRenderPhase1_CacheLastValueOnUnreachable tests the §9.2 safety invariant
// at the Go API level (not via subprocess) because the in-process cache state
// cannot be injected into a subprocess between two invocations.
//
// Verifies:
//  1. First successful resolution populates the cache.
//  2. Subsequent failure (resolver returns error) returns the prior cached value
//     (unreachableFallback=true).
//  3. rendered_revision computed from the fallback value is identical to the
//     one computed after the first successful call.
//  4. First-ever failure with no prior cache value returns ErrFirstCallUnreachable.
//
// [AC-S072a14-2-3] (last-value-on-unreachable) [AC-S072a14-2-M]
func TestRenderPhase1_CacheLastValueOnUnreachable(t *testing.T) {
	ctx := context.Background()

	// Step 1: successful first resolution.
	ok := &render.FakeInventoryResolver{Hash: "good-hash-abc", Fail: false}
	cache := render.NewResolverCache(ok, 30*time.Second)

	res1 := render.ResolveExternalInputs(ctx, cache)
	if res1.FirstCallFailed {
		t.Fatalf("[AC-S072a14-2-3] first call should succeed; got error: %v", res1.Err)
	}
	if res1.UnreachableFallback {
		t.Fatalf("[AC-S072a14-2-3] first call should not be a fallback")
	}
	if len(res1.ExternalInputHashes) == 0 {
		t.Fatalf("[AC-S072a14-2-3] ExternalInputHashes should be non-empty after successful resolve")
	}

	// Step 2: resolver now fails → should fall back to cached value.
	// We need to force a TTL miss so the cache actually calls the resolver again.
	// Use a zero-TTL cache with the failing resolver, pre-populated via the
	// first successful resolver, then swap to failing.
	fail := &render.FakeInventoryResolver{Hash: "good-hash-abc", Fail: true}
	cacheWithFail := render.NewResolverCache(fail, 0) // zero TTL: always misses

	// Manually prime the cache by calling the good resolver first, then swap.
	// Since we cannot easily inject a prior state into a fresh cache with zero TTL,
	// we test the fallback path by using a positive TTL cache where we first
	// populate with a good resolver, then swap the underlying resolver to a failing
	// one and expire the TTL.
	//
	// Strategy: create a positive-TTL cache, prime it, then force expiry by
	// using time.Sleep(0) (TTL=1ns so it expires immediately after the first call).
	primeResolver := &render.FakeInventoryResolver{Hash: "cached-value-xyz", Fail: false}
	cacheShortTTL := render.NewResolverCache(primeResolver, 1*time.Nanosecond)

	// Prime the cache (first call succeeds).
	primeResult := render.ResolveExternalInputs(ctx, cacheShortTTL)
	if primeResult.FirstCallFailed {
		t.Fatalf("[AC-S072a14-2-3] prime call should succeed")
	}

	// Wait for TTL to expire (1ns TTL is already expired after the first call).
	time.Sleep(1 * time.Millisecond)

	// Now swap the underlying resolver to a failing one by creating a NEW cache
	// that wraps a failing resolver but shares the same prior state...
	// Since ResolverCache fields are unexported, we test via a different path:
	// create a new cache with the failing resolver and 1ns TTL, prime it with a
	// successful call, then immediately re-resolve when the TTL has expired.
	//
	// For the test to work, we need both a successful prime AND an expired TTL.
	// We achieve this by calling the cache TWICE: once to prime, once after expiry.

	// Actually, let's use a more direct approach:
	// - Create a cache with 30s TTL (so the prime call is cached)
	// - Swap the underlying resolver to failing (not possible directly)
	// So we test via the pattern: prime cache → inject failure into FakeResolver.
	failableResolver := &render.FakeInventoryResolver{Hash: "fallback-value-999", Fail: false}
	cache30s := render.NewResolverCache(failableResolver, 30*time.Second)

	// Prime with good value.
	prime := render.ResolveExternalInputs(ctx, cache30s)
	if prime.FirstCallFailed {
		t.Fatalf("[AC-S072a14-2-3] prime should succeed")
	}

	// Expire the TTL by using a 1ns cache, re-prime, then make the resolver fail.
	// The cleanest test: use 1ns TTL, prime, sleep, then set Fail=true.
	failableResolver2 := &render.FakeInventoryResolver{Hash: "fb-val-123", Fail: false}
	cacheExpiry := render.NewResolverCache(failableResolver2, 1*time.Nanosecond)
	primeExpiry := render.ResolveExternalInputs(ctx, cacheExpiry)
	if primeExpiry.FirstCallFailed {
		t.Fatalf("[AC-S072a14-2-3] prime expiry should succeed")
	}
	hashes1 := primeExpiry.ExternalInputHashes

	// Sleep to expire the TTL.
	time.Sleep(2 * time.Millisecond)

	// Make the resolver fail (TTL expired → cache will call resolver → error).
	failableResolver2.Fail = true

	// Call Resolve: TTL expired, resolver fails → should return prior cached value.
	fallback := render.ResolveExternalInputs(ctx, cacheExpiry)
	if fallback.FirstCallFailed {
		t.Errorf("[AC-S072a14-2-3] expected fallback (not first-call failure); err=%v", fallback.Err)
	}
	if !fallback.UnreachableFallback {
		t.Errorf("[AC-S072a14-2-3] expected UnreachableFallback=true when resolver fails after prime; got false")
	}
	// The fallback hashes should match the primed value.
	if len(hashes1) != len(fallback.ExternalInputHashes) {
		t.Errorf("[AC-S072a14-2-3] fallback ExternalInputHashes mismatch: primed=%v fallback=%v",
			hashes1, fallback.ExternalInputHashes)
	} else {
		for i := range hashes1 {
			if hashes1[i] != fallback.ExternalInputHashes[i] {
				t.Errorf("[AC-S072a14-2-3] fallback hash[%d] mismatch: primed=%s fallback=%s",
					i, hashes1[i], fallback.ExternalInputHashes[i])
			}
		}
	}

	// Step 3: first-call failure → ErrFirstCallUnreachable (halt-safe).
	failFirst := &render.FakeInventoryResolver{Hash: "", Fail: true}
	cacheFail := render.NewResolverCache(failFirst, 30*time.Second)
	cacheWithFail = cacheFail // use it to satisfy the unused variable check
	_ = cacheWithFail

	failResult := render.ResolveExternalInputs(ctx, cacheFail)
	if !failResult.FirstCallFailed {
		t.Errorf("[AC-S072a14-2-3] expected FirstCallFailed=true when resolver fails on first call; got UnreachableFallback=%v err=%v",
			failResult.UnreachableFallback, failResult.Err)
	}
	if failResult.Err == nil {
		t.Errorf("[AC-S072a14-2-3] expected non-nil Err when FirstCallFailed=true")
	}

	t.Logf("[AC-S072a14-2-3] last-value-on-unreachable confirmed: fallback hashes=%v", fallback.ExternalInputHashes)
	t.Logf("[AC-S072a14-2-3] first-call-failure confirmed: err=%v", failResult.Err)
}

// firstLine returns the first non-empty line of s, for log brevity.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return s
}
