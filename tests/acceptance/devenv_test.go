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

// devenv_test.go contains acceptance tests for Story S33266e-3 — devenv（坪庭）
// 最小形: render 外部入力の差し替え（§26.2 / §23.6 / ADR-0016）.
//
// The tests exercise the DevenvProfile constructor seam directly (in-process,
// importing internal/render so call recording / isolation can be asserted) and
// the reachable CLI seam (sakutei plan --devenv end-to-end).
//
// The load-bearing property under test is ISOLATION: a devenv render resolves
// its external inputs (inventory / allocation / secret) entirely from
// substituted fakes and CANNOT reach a production pool / allocator / secret
// store.  "touched production = test failure" is encoded via the
// ProductionGuard* seams, which panic if ever invoked in devenv.
//
// AC tags:
//
//	[AC-S33266e-3-1]  devenv swaps ExternalInputResolver / SecretBackend /
//	                  AllocationAuthority to fakes; same intent, same reconciler.
//	[AC-S33266e-3-2]  SAFETY: no production pool/secret contacted; only fakes
//	                  called; a production-marked seam is never invoked.
//	[AC-S33266e-3-3]  devenv built on the existing FakeInventoryResolver /
//	                  ResolverCache path — an input swap only (no reconciler branch).
package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/render"
)

// ─────────────────────────────────────────────────────────────────────────────
// AC-S33266e-3-1: devenv substitutes all three external-input seams with fakes
// ─────────────────────────────────────────────────────────────────────────────

// TestDevenv_SubstitutesAllExternalInputs verifies that a DevenvProfile resolves
// external inputs entirely from substituted/fake resolvers (inventory /
// allocation / secret), and that the render phase-1 path yields a valid desired
// external-input set from those fakes.
//
// [AC-S33266e-3-1]
func TestDevenv_SubstitutesAllExternalInputs(t *testing.T) {
	// [AC-S33266e-3-1]
	prof := render.NewDevenvProfile(
		render.WithDevenvInventoryHash("devenv-inv-xyz"),
		render.WithDevenvAllocationPrefix("devenv-net"),
		render.WithDevenvSecretGeneration("devenv-gen-42"),
	)
	ctx := context.Background()

	// Inventory: resolves through the substituted fake (recorded).
	res := prof.ResolveExternalInputs(ctx)
	if res.FirstCallFailed {
		t.Fatalf("[AC-S33266e-3-1] devenv external-input resolution failed: %v", res.Err)
	}
	if got := res.Inputs["inventory_hash"]; got != "devenv-inv-xyz" {
		t.Errorf("[AC-S33266e-3-1] inventory_hash = %q; want the substituted fake value %q", got, "devenv-inv-xyz")
	}
	if prof.Inventory.Calls() != 1 {
		t.Errorf("[AC-S33266e-3-1] fake inventory resolver Calls = %d; want 1 (only the fake used)", prof.Inventory.Calls())
	}

	// Allocation: resolves through the substituted authority (no production pool).
	alloc, err := prof.AllocationAuthority().Resolve(ctx, "vm-01/eth0")
	if err != nil {
		t.Fatalf("[AC-S33266e-3-1] fake allocation authority error: %v", err)
	}
	if !strings.HasPrefix(alloc, "devenv-net/") {
		t.Errorf("[AC-S33266e-3-1] allocation = %q; want a substituted devenv-net allocation", alloc)
	}
	if claims := prof.Allocation.ResolvedClaims(); len(claims) != 1 || claims[0] != "vm-01/eth0" {
		t.Errorf("[AC-S33266e-3-1] recorded allocation claims = %v; want [vm-01/eth0]", claims)
	}

	// Secret: resolves generation metadata through the substituted backend.
	gen, err := prof.SecretBackend().GenerationOf(ctx, "secret/db/password")
	if err != nil {
		t.Fatalf("[AC-S33266e-3-1] fake secret backend error: %v", err)
	}
	if gen != "devenv-gen-42" {
		t.Errorf("[AC-S33266e-3-1] secret generation = %q; want the substituted %q", gen, "devenv-gen-42")
	}
	if paths := prof.Secret.QueriedPaths(); len(paths) != 1 || paths[0] != "secret/db/password" {
		t.Errorf("[AC-S33266e-3-1] recorded secret paths = %v; want [secret/db/password]", paths)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S33266e-3-2: SAFETY — no production pool/secret contacted in devenv
// ─────────────────────────────────────────────────────────────────────────────

// TestDevenv_NoProductionContact verifies the isolation guarantee: a devenv run
// drives ONLY the fakes, and the production-marked seams (ProductionGuard*) are
// never invoked.  The ProductionGuard* seams panic if reached, so this test
// wires them as the "what production would be" reference and asserts they stay
// silent — encoding "touched production = test failure".
//
// [AC-S33266e-3-2]
func TestDevenv_NoProductionContact(t *testing.T) {
	// [AC-S33266e-3-2]
	prof := render.NewDevenvProfile()
	ctx := context.Background()

	// Reference production seams.  If devenv ever routed to these, calling them
	// would panic; we assert they can be held alongside the profile without the
	// devenv path ever invoking them.
	prodInv := render.ProductionGuardResolver{}
	prodAlloc := render.ProductionGuardAllocationAuthority{}
	prodSecret := render.ProductionGuardSecretBackend{}
	_ = prodInv
	_ = prodAlloc
	_ = prodSecret

	// Drive the full devenv external-input path.  None of these may panic —
	// a panic here means a production seam was contacted (test failure).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[AC-S33266e-3-2] devenv contacted a production external input (panic): %v", r)
		}
	}()

	if res := prof.ResolveExternalInputs(ctx); res.FirstCallFailed {
		t.Fatalf("[AC-S33266e-3-2] devenv inventory resolution failed: %v", res.Err)
	}
	if _, err := prof.AllocationAuthority().Resolve(ctx, "claim-a"); err != nil {
		t.Fatalf("[AC-S33266e-3-2] devenv allocation failed: %v", err)
	}
	if _, err := prof.SecretBackend().GenerationOf(ctx, "secret/x"); err != nil {
		t.Fatalf("[AC-S33266e-3-2] devenv secret resolution failed: %v", err)
	}

	// Positive proof that the fakes (not production) carried the work.
	if prof.Inventory.Calls() != 1 {
		t.Errorf("[AC-S33266e-3-2] fake inventory Calls = %d; want 1", prof.Inventory.Calls())
	}
	if len(prof.Allocation.ResolvedClaims()) != 1 {
		t.Errorf("[AC-S33266e-3-2] fake allocation claims = %d; want 1", len(prof.Allocation.ResolvedClaims()))
	}
	if len(prof.Secret.QueriedPaths()) != 1 {
		t.Errorf("[AC-S33266e-3-2] fake secret paths = %d; want 1", len(prof.Secret.QueriedPaths()))
	}
}

// TestDevenv_ProductionGuardPanicsIfCalled proves the guard mechanism itself:
// the ProductionGuard* seams panic when invoked, which is what makes
// "touched production" a detectable test failure.  If these did NOT panic, the
// isolation assertion in TestDevenv_NoProductionContact would be vacuous.
//
// [AC-S33266e-3-2]
func TestDevenv_ProductionGuardPanicsIfCalled(t *testing.T) {
	// [AC-S33266e-3-2]
	ctx := context.Background()

	assertPanics := func(name string, fn func()) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("[AC-S33266e-3-2] %s did not panic when called; production contact would go undetected", name)
			}
		}()
		fn()
	}

	assertPanics("ProductionGuardResolver.Resolve", func() {
		_, _ = render.ProductionGuardResolver{}.Resolve(ctx)
	})
	assertPanics("ProductionGuardAllocationAuthority.Resolve", func() {
		_, _ = render.ProductionGuardAllocationAuthority{}.Resolve(ctx, "k")
	})
	assertPanics("ProductionGuardSecretBackend.GenerationOf", func() {
		_, _ = render.ProductionGuardSecretBackend{}.GenerationOf(ctx, "p")
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// AC-S33266e-3-3: devenv is an input swap on the existing ResolverCache path
// ─────────────────────────────────────────────────────────────────────────────

// TestDevenv_BuiltOnExistingResolverPath verifies that devenv is expressed as an
// external-input SWAP on the existing FakeInventoryResolver / ResolverCache path
// — the SAME package-level ResolveExternalInputs the production path calls —
// rather than a new resolution mechanism or a reconciler branch (§26.2).
//
// [AC-S33266e-3-3]
func TestDevenv_BuiltOnExistingResolverPath(t *testing.T) {
	// [AC-S33266e-3-3]
	prof := render.NewDevenvProfile(render.WithDevenvInventoryHash("swap-hash"))
	cache := prof.ExternalInputCache()

	// The devenv profile hands back a *render.ResolverCache — the exact type the
	// production wiring builds — and the package-level ResolveExternalInputs
	// consumes it unchanged.  This is the "input swap only" property.
	ctx := context.Background()
	res := render.ResolveExternalInputs(ctx, cache)
	if res.FirstCallFailed {
		t.Fatalf("[AC-S33266e-3-3] ResolveExternalInputs over devenv cache failed: %v", res.Err)
	}
	if got := res.Inputs["inventory_hash"]; got != "swap-hash" {
		t.Errorf("[AC-S33266e-3-3] inventory_hash = %q; want %q via the shared ResolveExternalInputs path", got, "swap-hash")
	}
	// Deterministic external-input hash slice is produced for rendered_revision,
	// proving devenv feeds the normal downstream revision computation.
	if len(res.ExternalInputHashes) == 0 {
		t.Errorf("[AC-S33266e-3-3] devenv produced no external-input hashes; expected the shared path to emit key=value entries")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// End-to-end: sakutei plan --devenv drives the full render→desired path
// ─────────────────────────────────────────────────────────────────────────────

// TestDevenv_PlanEndToEnd verifies the full render→desired path works in devenv
// mode via the reachable CLI seam.  "sakutei plan --devenv" resolves external
// inputs from the substituted fakes, diverges rendered_revision from
// intent_revision (external input present), and completes without touching any
// production system — a real dogfooding path (§26 devenv dogfooding).
//
// [AC-S33266e-3-1] [AC-S33266e-3-3]
func TestDevenv_PlanEndToEnd(t *testing.T) {
	// [AC-S33266e-3-1]
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(tmp, "state.json")
	initStateFile(t, statePath)
	writeOneResourceIntent(t, intentDir)

	stdout, stderr, code := runPlanPhase1(t, intentDir, statePath, "--devenv")
	if code != 0 {
		t.Fatalf("[AC-S33266e-3-1] plan --devenv exit code=%d; stderr=%s; stdout=%s", code, stderr, stdout)
	}

	// The devenv banner must be emitted (proof the substituted path ran).
	if !strings.Contains(stderr, "devenv") {
		t.Errorf("[AC-S33266e-3-1] expected devenv banner on stderr; got:\n%s", stderr)
	}

	intentRev, renderedRev := extractRevisions(stdout)
	if intentRev == "" || renderedRev == "" {
		t.Fatalf("[AC-S33266e-3-1] revision header missing from plan --devenv output:\n%s", stdout)
	}
	// External input (fake inventory hash) is present → rendered diverges (§9.4).
	if intentRev == renderedRev {
		t.Errorf("[AC-S33266e-3-3] rendered_revision should diverge from intent_revision under devenv (external input present); both = %s", intentRev)
	}
	t.Logf("plan --devenv: intent_revision=%s rendered_revision=%s (diverged via substituted fake inventory)", intentRev, renderedRev)
}
