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

// Package acceptance / scoped_reconcile_test.go covers Story Sf6a62f-2: the
// scoped reconcile (早期収束) path (§8.6 / ADR-0009).
//
// These are in-process acceptance tests: like change_hint_test.go, they import
// the internal seam packages directly and drive the ScopedPlan / ScopedReconcile
// entry points against the in-memory fake provider.  The scoped path is a seam
// the daemon COULD call (via ScopedRunner.DrainAndReconcile) when a Watcher
// delivers a hint; it is not yet auto-wired into the CLI, so a subprocess test
// cannot reach it — hence the in-process style.
//
// AC tags map to docs/ROADMAP.json .sprints.Sf6a62f.stories["Sf6a62f-2"]:
//
//	AC-Sf6a62f-2-1  ScopedPlan: hint + old/new desired → impacted sub-DAG
//	                (changed resource + downstream only; untouched scope excluded).
//	AC-Sf6a62f-2-2  scoped Observe→Diff→Apply hits ONLY the touched scope's
//	                sub-DAG; an unrelated scope is not even Observed.
//	AC-Sf6a62f-2-3  cause A/E only + no set-difference prune (orphan survives).
//	AC-Sf6a62f-2-4  shared budget: scoped Apply consumes the same budget.Engine;
//	                on exhaustion the scoped run halts and defers to the periodic
//	                cycle (does not bypass).
package acceptance

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/reconcile"
	"github.com/tjst-t/sakutei/pkg/core"
)

// scopeX and scopeY are two disjoint routing scopes used across these tests.
func scopeX() core.Scope { return core.NewScope(map[string]string{"site": "x"}) }
func scopeY() core.Scope { return core.NewScope(map[string]string{"site": "y"}) }

// dres builds a DesiredResource in the given scope with a single "size" field
// and optional depends_on edges.
func dres(scope core.Scope, local, size string, deps ...string) diff.DesiredResource {
	return diff.DesiredResource{
		ID:        core.NewID(fakeprovider.ProviderName, scope, local),
		Fields:    map[string]string{"name": local, "size": size},
		DependsOn: deps,
	}
}

// causeA returns a CauseContext where intent_revision changed (prev != cur),
// so a diff on an in-scope resource classifies as cause A (intentional).
func causeA() cause.CauseContext {
	return cause.CauseContext{IntentPrevRevision: "rev-old", IntentCurRevision: "rev-new"}
}

// causeStable returns a CauseContext where intent/rendered/provider are all
// stable (prev == cur), so any diff the engine finds classifies as cause B
// (drift by elimination).  Used to prove the scoped path DEFERS cause B.
func causeStable() cause.CauseContext {
	return cause.CauseContext{IntentPrevRevision: "rev-1", IntentCurRevision: "rev-1"}
}

func newFake(t *testing.T) *fakeprovider.Provider {
	t.Helper()
	dir := t.TempDir()
	return fakeprovider.New(filepath.Join(dir, "state.json"))
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-2-1: ScopedPlan computes the impacted sub-DAG from old/new desired
// WITHOUT touching reality (no Observe), including downstream closure, and
// excludes untouched scopes.
// ---------------------------------------------------------------------------

// [AC-Sf6a62f-2-1]
func TestScopedPlan_ImpactedSubDAG_ChangedPlusDownstreamOnly(t *testing.T) {
	// Old desired: X/a(size=1) ← X/b depends on a ; unrelated Y/c(size=1).
	oldDesired := []diff.DesiredResource{
		dres(scopeX(), "a", "1"),
		dres(scopeX(), "b", "1", "a"),
		dres(scopeY(), "c", "1"),
	}
	// New desired: X/a changed to size=2 (b unchanged but is downstream of a);
	// Y/c unchanged.
	newDesired := []diff.DesiredResource{
		dres(scopeX(), "a", "2"),
		dres(scopeX(), "b", "1", "a"),
		dres(scopeY(), "c", "1"),
	}
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)

	got := map[string]bool{}
	for _, r := range spr.Impacted {
		got[r.ID.Local] = true
	}
	if !got["a"] {
		t.Errorf("[AC-Sf6a62f-2-1] changed resource 'a' must be in impacted sub-DAG")
	}
	if !got["b"] {
		t.Errorf("[AC-Sf6a62f-2-1] downstream 'b' (depends_on a) must be in impacted sub-DAG (dependency closure)")
	}
	if got["c"] {
		t.Errorf("[AC-Sf6a62f-2-1] untouched scope Y resource 'c' must NOT be in impacted sub-DAG; got impacted=%v",
			impactedLocals(spr))
	}
	if len(spr.Impacted) != 2 {
		t.Errorf("[AC-Sf6a62f-2-1] impacted sub-DAG must be exactly {a,b}; got %v", impactedLocals(spr))
	}
}

// [AC-Sf6a62f-2-1] a hint touching only scope Y must not drag in scope X even
// when X changed — the hint's TouchedScopes routes, and desired diff seeds.
func TestScopedPlan_UntouchedScopeExcluded(t *testing.T) {
	oldDesired := []diff.DesiredResource{
		dres(scopeX(), "a", "1"),
		dres(scopeY(), "c", "1"),
	}
	// Only Y/c changed; hint touches Y.
	newDesired := []diff.DesiredResource{
		dres(scopeX(), "a", "1"),
		dres(scopeY(), "c", "2"),
	}
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeY()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)
	if len(spr.Impacted) != 1 || spr.Impacted[0].ID.Local != "c" {
		t.Fatalf("[AC-Sf6a62f-2-1] impacted must be exactly {c}; got %v", impactedLocals(spr))
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-2-2: scoped Observe→Diff→Apply hits ONLY the impacted sub-DAG.
// A resource in an unrelated scope is NOT Observed (read count stays 0) and
// NOT Applied (its reality is unchanged).
// ---------------------------------------------------------------------------

// [AC-Sf6a62f-2-2]
func TestScopedReconcile_ObservesAndAppliesOnlyImpacted(t *testing.T) {
	fp := newFake(t)
	// Seed reality: a=1, b=1, c=1 all exist.
	mustSeed(t, fp, "a", 1)
	mustSeed(t, fp, "b", 1)
	mustSeed(t, fp, "c", 1)

	oldDesired := []diff.DesiredResource{
		dres(scopeX(), "a", "1"),
		dres(scopeX(), "b", "1", "a"),
		dres(scopeY(), "c", "1"),
	}
	// X/a changes to size=2. Y/c is untouched.
	newDesired := []diff.DesiredResource{
		dres(scopeX(), "a", "2"),
		dres(scopeX(), "b", "1", "a"),
		dres(scopeY(), "c", "1"),
	}
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)
	res := runScoped(t, fp, spr, causeA())

	// Read counts must be checked BEFORE any verification Read (mustSize/mustExists
	// also increment the counter), or c's count would be polluted.
	rc, err := fp.GetReadCount()
	if err != nil {
		t.Fatalf("GetReadCount: %v", err)
	}
	if rc["a"] == 0 {
		t.Errorf("[AC-Sf6a62f-2-2] impacted resource a must be freshly Observed; read count 0")
	}
	if rc["c"] != 0 {
		t.Errorf("[AC-Sf6a62f-2-2] unrelated scope Y resource c must NOT be Observed on the scoped path; read count=%d", rc["c"])
	}
	if res.Observed == 0 {
		t.Errorf("[AC-Sf6a62f-2-2] scoped run must Observe the impacted sub-DAG; Observed=0")
	}

	// Reality: a updated to 2; c untouched at 1.
	if got := mustSize(t, fp, "a"); got != 2 {
		t.Errorf("[AC-Sf6a62f-2-2] scoped run must Apply the changed resource a (size 2); got %d", got)
	}
	if got := mustSize(t, fp, "c"); got != 1 {
		t.Errorf("[AC-Sf6a62f-2-2] unrelated scope Y resource c must NOT be Applied; got size %d", got)
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-2-3(a): cause A/E only — a drift (cause B) surfaced on the scoped
// path is DEFERRED (not auto-repaired).
// ---------------------------------------------------------------------------

// [AC-Sf6a62f-2-3]
func TestScopedReconcile_DefersCauseBDrift(t *testing.T) {
	fp := newFake(t)
	// Reality drifted: a exists with size=99 but desired is size=1.
	mustSeed(t, fp, "a", 99)

	// desired is UNCHANGED between old and new (a=1 both sides); the resource
	// enters the sub-DAG only because the hint touches its scope.  With a stable
	// cause context (intent unchanged), the diff engine classifies the diff as B.
	oldDesired := []diff.DesiredResource{dres(scopeX(), "a", "1")}
	newDesired := []diff.DesiredResource{dres(scopeX(), "a", "1")}
	hint := core.ChangeHint{IntentRevision: "rev-1", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)
	res := runScoped(t, fp, spr, causeStable())

	// The drift (cause B) must NOT be repaired on the scoped path.
	if got := mustSize(t, fp, "a"); got != 99 {
		t.Errorf("[AC-Sf6a62f-2-3] cause-B drift must NOT be repaired on the scoped path; size changed to %d", got)
	}
	found := false
	for _, l := range res.DeferredCauseB {
		if l == "a" {
			found = true
		}
	}
	if !found {
		t.Errorf("[AC-Sf6a62f-2-3] resource a must be reported as deferred (cause B); DeferredCauseB=%v", res.DeferredCauseB)
	}
	if res.Applied != 0 {
		t.Errorf("[AC-Sf6a62f-2-3] scoped run must apply nothing for a pure cause-B diff; Applied=%d", res.Applied)
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-2-3(b): no set-difference prune — an orphan in reality (present in
// reality, absent from desired, not an explicit tombstone) SURVIVES the scoped
// pass.  Only a full-List periodic cycle could prune it.
// ---------------------------------------------------------------------------

// [AC-Sf6a62f-2-3]
func TestScopedReconcile_NoSetDifferencePrune_OrphanSurvives(t *testing.T) {
	fp := newFake(t)
	// Reality has an orphan 'ghost' (exists in reality, absent from all desired).
	mustSeed(t, fp, "a", 1)
	mustSeed(t, fp, "ghost", 1)

	oldDesired := []diff.DesiredResource{dres(scopeX(), "a", "1")}
	// a changes; ghost is nowhere in desired (it is a List-only orphan).
	newDesired := []diff.DesiredResource{dres(scopeX(), "a", "2")}
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)

	// The orphan must not even appear as a tombstone (set-difference prune is the
	// only way to discover it, and the scoped path does not List).
	for _, id := range spr.RemovedTombstones {
		if id.Local == "ghost" {
			t.Fatalf("[AC-Sf6a62f-2-3] orphan 'ghost' must NOT be a scoped tombstone (set-diff prune forbidden)")
		}
	}

	res := runScoped(t, fp, spr, causeA())

	// The orphan survives: reality still contains 'ghost'.
	if !mustExists(t, fp, "ghost") {
		t.Errorf("[AC-Sf6a62f-2-3] orphan 'ghost' must SURVIVE the scoped pass (no set-difference prune)")
	}
	for _, id := range res.Tombstoned {
		if id.Local == "ghost" {
			t.Errorf("[AC-Sf6a62f-2-3] orphan 'ghost' must NOT be deleted by the scoped path")
		}
	}
}

// [AC-Sf6a62f-2-3] a PruneWithinScope resource that disappeared from desired is
// NOT turned into a scoped tombstone (that is List-based prune territory);
// an ExplicitAbsentOnly disappearance IS an allowed individual delete.
func TestScopedPlan_TombstoneOnlyForExplicitAbsent(t *testing.T) {
	explicit := dres(scopeX(), "e", "1")
	explicit.DeletionPolicy = core.ExplicitAbsentOnly
	pruneScoped := dres(scopeX(), "p", "1")
	pruneScoped.DeletionPolicy = core.PruneWithinScope

	oldDesired := []diff.DesiredResource{explicit, pruneScoped, dres(scopeX(), "a", "1")}
	newDesired := []diff.DesiredResource{dres(scopeX(), "a", "1")} // e and p removed.
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)

	var gotE, gotP bool
	for _, id := range spr.RemovedTombstones {
		if id.Local == "e" {
			gotE = true
		}
		if id.Local == "p" {
			gotP = true
		}
	}
	if !gotE {
		t.Errorf("[AC-Sf6a62f-2-3] ExplicitAbsentOnly removal 'e' must be an allowed scoped tombstone")
	}
	if gotP {
		t.Errorf("[AC-Sf6a62f-2-3] PruneWithinScope removal 'p' must NOT be a scoped tombstone (set-diff prune forbidden)")
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-2-4: shared budget — the scoped Apply consumes the SAME
// budget.Engine as the periodic loop; when the budget is exhausted the scoped
// run halts and defers to the next periodic cycle (does not bypass).
// ---------------------------------------------------------------------------

// [AC-Sf6a62f-2-4]
func TestScopedReconcile_SharesBudgetCounter(t *testing.T) {
	fp := newFake(t)
	mustSeed(t, fp, "a", 1)

	// Shared engine: a window budget of 1 destructive op.  We pre-commit 1
	// destructive op through the SAME engine to simulate the periodic loop
	// having already spent the window budget.
	shared := budget.New(budget.Config{MaxDestructivePerWindow: 1})
	shared.CommitOps([]core.Op{{Kind: core.OpKindDelete, Destructive: true}})

	// A scoped tombstone (destructive individual delete) would need budget, but
	// the window is already exhausted by the periodic loop's committed op.
	explicit := dres(scopeX(), "a", "1")
	explicit.DeletionPolicy = core.ExplicitAbsentOnly
	oldDesired := []diff.DesiredResource{explicit}
	newDesired := []diff.DesiredResource{} // a removed → explicit absent tombstone.
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)
	res := runScopedWithBudget(t, fp, spr, causeA(), shared)

	if !res.BudgetHalted {
		t.Errorf("[AC-Sf6a62f-2-4] scoped run must halt when the shared budget window is exhausted")
	}
	if !res.Deferred {
		t.Errorf("[AC-Sf6a62f-2-4] scoped run must defer to the periodic cycle on budget halt (not bypass)")
	}
	// The delete must NOT have been bypassed: reality still contains 'a'.
	if !mustExists(t, fp, "a") {
		t.Errorf("[AC-Sf6a62f-2-4] scoped run must NOT bypass the budget; the tombstone delete leaked through")
	}
}

// [AC-Sf6a62f-2-4] when the shared budget has room, the scoped Apply consumes
// it and the SAME engine's window counter advances (proving shared counter).
func TestScopedReconcile_AdvancesSharedWindowCounter(t *testing.T) {
	fp := newFake(t)
	mustSeed(t, fp, "a", 1)

	shared := budget.New(budget.Config{MaxDestructivePerWindow: 5})
	before := shared.WindowTotal()

	explicit := dres(scopeX(), "a", "1")
	explicit.DeletionPolicy = core.ExplicitAbsentOnly
	oldDesired := []diff.DesiredResource{explicit}
	newDesired := []diff.DesiredResource{}
	hint := core.ChangeHint{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}

	spr := reconcile.ScopedPlan(hint, oldDesired, newDesired)
	res := runScopedWithBudget(t, fp, spr, causeA(), shared)

	if res.BudgetHalted {
		t.Fatalf("[AC-Sf6a62f-2-4] scoped run must NOT halt with budget room; reason=%s", res.BudgetReason)
	}
	if len(res.Tombstoned) != 1 {
		t.Fatalf("[AC-Sf6a62f-2-4] scoped run must delete the explicit-absent resource; Tombstoned=%v", res.Tombstoned)
	}
	after := shared.WindowTotal()
	if after != before+1 {
		t.Errorf("[AC-Sf6a62f-2-4] scoped Apply must advance the SAME budget window counter: before=%d after=%d (want +1)",
			before, after)
	}
	if mustExists(t, fp, "a") {
		t.Errorf("[AC-Sf6a62f-2-4] explicit-absent 'a' must be deleted when budget allows")
	}
}

// ---------------------------------------------------------------------------
// AC-Sf6a62f-2-2 (wiring): the ScopedRunner.DrainAndReconcile seam is reachable
// from a Watcher-implementing DesiredSource — a hint delivered on the Watch
// channel drains into a scoped reconcile of only the impacted sub-DAG.
// ---------------------------------------------------------------------------

// [AC-Sf6a62f-2-2]
func TestScopedRunner_DrainsWatcherHintAndReconciles(t *testing.T) {
	fp := newFake(t)
	mustSeed(t, fp, "a", 1)
	mustSeed(t, fp, "c", 1)

	oldDesired := []diff.DesiredResource{dres(scopeX(), "a", "1"), dres(scopeY(), "c", "1")}
	newDesired := []diff.DesiredResource{dres(scopeX(), "a", "2"), dres(scopeY(), "c", "1")}

	// A Watcher-implementing DesiredSource that emits one hint touching scope X.
	src := &loader.FakeWatchSource{
		Resources: newDesired,
		Revision:  core.RevisionInfo{IntentRevision: "rev-new"},
		Hints:     []core.ChangeHint{{IntentRevision: "rev-new", TouchedScopes: []core.Scope{scopeX()}}},
	}
	// Declaration-制: discover Watch via the same type assertion the daemon uses.
	w, ok := interface{}(src).(loader.Watcher)
	if !ok {
		t.Fatal("[AC-Sf6a62f-2-2] FakeWatchSource must implement loader.Watcher")
	}
	ch, err := w.Watch(context.Background())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	runner := reconcile.ScopedRunner{
		Provider:     fp,
		Executor:     executor.New(),
		Engine:       diff.New(fp.Schema()),
		BudgetEngine: budget.New(budget.Config{}),
		InFlight:     reconcile.NewInFlightSet(),
	}

	// Give the emitter goroutine a moment to enqueue the hint, then drain.
	var res reconcile.ScopedReconcileResult
	var ran bool
	for i := 0; i < 100; i++ {
		res, ran = runner.DrainAndReconcile(context.Background(), ch, oldDesired, newDesired, causeA())
		if ran {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ran {
		t.Fatal("[AC-Sf6a62f-2-2] DrainAndReconcile never observed the delivered hint")
	}
	if res.Impacted != 1 {
		t.Errorf("[AC-Sf6a62f-2-2] hint touching scope X must impact only {a}; Impacted=%d", res.Impacted)
	}
	if got := mustSize(t, fp, "a"); got != 2 {
		t.Errorf("[AC-Sf6a62f-2-2] scoped run via Watcher must apply a→2; got %d", got)
	}
	if got := mustSize(t, fp, "c"); got != 1 {
		t.Errorf("[AC-Sf6a62f-2-2] scope Y resource c must be untouched; got %d", got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func runScoped(t *testing.T, fp *fakeprovider.Provider, spr reconcile.ScopedPlanResult, cc cause.CauseContext) reconcile.ScopedReconcileResult {
	t.Helper()
	return runScopedWithBudget(t, fp, spr, cc, budget.New(budget.Config{}))
}

func runScopedWithBudget(t *testing.T, fp *fakeprovider.Provider, spr reconcile.ScopedPlanResult, cc cause.CauseContext, be *budget.Engine) reconcile.ScopedReconcileResult {
	t.Helper()
	engine := diff.New(fp.Schema())
	exec := executor.New()
	ifl := reconcile.NewInFlightSet()
	var errBuf bytes.Buffer
	return reconcile.ScopedReconcile(context.Background(), spr, fp, exec, engine, be, ifl, cc, &errBuf)
}

func mustSeed(t *testing.T, fp *fakeprovider.Provider, local string, size int) {
	t.Helper()
	if err := fp.SeedResource(local, map[string]any{"name": local, "size": size}); err != nil {
		t.Fatalf("SeedResource %s: %v", local, err)
	}
}

func mustSize(t *testing.T, fp *fakeprovider.Provider, local string) int {
	t.Helper()
	obs, err := fp.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read %s: %v", local, err)
	}
	if obs.State == nil {
		t.Fatalf("resource %s absent in reality", local)
	}
	tv, ok := obs.State["size"]
	if !ok {
		t.Fatalf("resource %s has no size field", local)
	}
	switch v := tv.Value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case string:
		// Apply writes the desired field value as a string ("2"); reality then
		// stores it as a JSON string.  Parse it back for the assertion.
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("resource %s size %q not an int: %v", local, v, err)
		}
		return n
	default:
		t.Fatalf("resource %s size has unexpected type %T", local, tv.Value)
		return 0
	}
}

func mustExists(t *testing.T, fp *fakeprovider.Provider, local string) bool {
	t.Helper()
	obs, err := fp.Read(context.Background(), local)
	if err != nil {
		t.Fatalf("Read %s: %v", local, err)
	}
	return obs.State != nil
}

func impactedLocals(spr reconcile.ScopedPlanResult) []string {
	out := make([]string, 0, len(spr.Impacted))
	for _, r := range spr.Impacted {
		out = append(out, r.ID.Local)
	}
	return out
}
