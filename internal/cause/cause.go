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

// Package cause implements per-resource cause-tag classification for Sakutei
// (§10.2–§10.3, ADR-0008, Story Scb0ecd-2).
//
// # Classification order (per-resource, applied to every Op that has a diff)
//
//  1. C (ghost diff / provider upgrade) — provider_version changed.  The
//     provider's normalisation logic may have changed; even if intent and
//     reality look the same to an operator, the diff engine now computes a
//     new desired form.  Most dangerous: silently auto-applies look-alike
//     changes to ALL resources.  Checked first so ghost diffs are not
//     misattributed to A or B.
//
//  2. D (cascade) — an upstream resource (composite parent) desired state
//     changed (flag injected by the render/DAG layer, S072a14-3).  D takes
//     priority over A so that composite children report the cascade cause
//     rather than "intent changed at scope level"; the child's own desired
//     fields did not change directly.  In v1a this flag is always false.
//
//  3. A (intentional) — a genuine intent change, in one of two forms:
//     (a) a KNOWN prior intent_revision that DIFFERS from the current one
//         (IntentPrevRevision != "" && != IntentCurRevision): the operator
//         committed a new desired state; or
//     (b) cold start (IntentPrevRevision == "" / no history) where reality
//         was observed ABSENT — i.e. the op is a Create.  A first declaration
//         of a resource that does not yet exist is intentional, not drift.
//
//     COLD-START vs DRIFT (fix Sc7a647 cause-consistency): an EMPTY
//     IntentPrevRevision means "no history / cold start", NOT "intent changed".
//     Previously the A branch fired whenever IntentCurRevision !=
//     IntentPrevRevision, so a one-shot plan/reconcile with no --cause-state
//     ("" != cur) tagged EVERYTHING A — including an UPDATE that merely repairs
//     a field that drifted under the operator.  That read as A ("operator
//     changed intent") under plan/reconcile but B ("drift") under status, for
//     the very same drifted resource.  The unified rule is: with no history,
//     the OPERATION KIND decides (Create → A, Update → B via ResourceObservedAbsent);
//     with a known prior revision that differs, it is a genuine intent change (A);
//     with a known prior revision equal to cur (stable), it is never A.
//
//  4. E (external-input change) — rendered_revision changed but
//     intent_revision is stable.  In v1a (no external inputs) rendered ==
//     intent so this branch never fires; it is a seam for S072a14.
//
//  5. B (drift) — all revision-stable signals are false, yet a diff
//     exists.  Conclusion by elimination: the real world drifted away from
//     the desired state without any human commit.  Observation-state-less
//     systems cannot directly observe "actual changed"; B is the residual
//     category.
//
// # Seam discipline (ADR-0008 + §10.3)
//
// D and E are type-complete (the CauseTag enum values exist) and the seam
// input flags exist in CauseContext, but in v1a both flags are always false
// so D and E never fire.  Their complete activation is gated on:
//   - D: render DAG upstream-change propagation (S072a14 / render pipeline).
//   - E: ComputeRenderedRevision with real external inputs (S072a14); until
//     then rendered_revision == intent_revision so E never fires.
//
// Do NOT add D/E activation logic here before those dependencies exist;
// the seam is deliberately thin to avoid over-reach (ROADMAP Scb0ecd-2
// constraint).
package cause

import "github.com/tjst-t/sakutei/pkg/core"

// CauseContext carries the revision and version signals needed to classify a
// single resource's diff into one of the five cause tags A–E.
//
// All fields are provided by the caller (plan/reconcile command layer) from
// the current cycle's observed values.  The diff engine receives a
// CauseContext and passes it to Classify after computing that a diff exists.
//
// Revision fields follow the §9.4 / ADR-0008 definitions:
//   - Intent{Prev,Cur}Revision  — hash of the human-authored intent files.
//   - Rendered{Prev,Cur}Revision — hash of intent + resolved external inputs.
//     In v1a rendered_revision = intent_revision (seam for S072a14).
//   - Provider{Prev,Cur}Version  — hash of the provider's diff/normalisation
//     source (SDK-computed, not a hand-written semver).  In v1a the fake
//     provider exposes this via ProviderVersion() from its state file.
//     The full fail-closed enforcement (reject unversioned providers at startup)
//     is Story Scb0ecd-3; here we accept empty-string as "unknown".
//
// Seam flags (always false in v1a):
//   - UpstreamChanged — set by the render DAG layer when an upstream
//     dependency's desired state changed in the same cycle.  Activates D.
//     Will be wired in S072a14 (render pipeline / DAG cascade tracking).
//   - RenderedRevisionChanged — derived from Rendered{Prev,Cur}Revision
//     diverging while Intent revisions are equal.  Activates E.  In v1a
//     rendered == intent so this is never true unless explicitly set by a
//     future external-input seam.
type CauseContext struct {
	// IntentPrevRevision is the intent_revision from the previous cycle (or
	// empty string on the very first cycle / cold start).
	IntentPrevRevision string
	// IntentCurRevision is the intent_revision computed for this cycle.
	IntentCurRevision string

	// RenderedPrevRevision is the rendered_revision from the previous cycle.
	// In v1a always equals IntentPrevRevision.
	RenderedPrevRevision string
	// RenderedCurRevision is the rendered_revision for this cycle.
	// In v1a always equals IntentCurRevision.
	RenderedCurRevision string

	// ProviderPrevVersion is the provider_version from the previous cycle
	// (empty if not yet tracked — treated as "unknown", not equal to any
	// non-empty version, so a first-run version appears as a change).
	ProviderPrevVersion string
	// ProviderCurVersion is the provider_version for this cycle.
	// Empty means the provider did not embed a version (Scb0ecd-3 enforces
	// fail-closed; here we accept empty gracefully and treat as stable).
	ProviderCurVersion string

	// UpstreamChanged is true when a declared dependency (depends_on) of this
	// resource had its desired state changed in the same cycle.
	// SEAM (v1a): always false.  Will be set by the render DAG in S072a14.
	UpstreamChanged bool

	// RenderedRevisionChanged is a pre-computed convenience flag:
	// (RenderedCurRevision != RenderedPrevRevision) && (IntentCurRevision == IntentPrevRevision).
	// SEAM (v1a): always false because rendered == intent.
	// Will be set when S072a14 brings real external inputs into rendered_revision.
	RenderedRevisionChanged bool

	// ResourceObservedAbsent is true when THIS cycle's Observe found the resource
	// absent in reality — i.e. the op the diff engine will emit is a Create (§4,
	// obs.State == nil).  The CALLER sets it per-resource from the observation.
	//
	// It only matters on cold start (IntentPrevRevision == "" / no history), where
	// it disambiguates the A-vs-B decision (fix Sc7a647 cause-consistency):
	//   - absent  → Create → A (intentional first declaration).
	//   - present → Update → NOT A (a field drifted under the operator → falls to B).
	// When a KNOWN prior revision exists, the revision comparison decides and this
	// flag is not consulted for the A branch.
	ResourceObservedAbsent bool
}

// Classify returns the cause tag for a single resource Op, given:
//   - diffExists: true when the diff engine found at least one field difference
//     for this resource (Create, Update, or Delete).  If false, Classify is a
//     no-op and the caller should not attach any tag (no Op will be emitted).
//   - ctx: the per-cycle, per-resource revision/version signals.
//
// The classification logic follows §10.2 (priority order: C > D > A > E > B).
// D takes priority over A (S072a14-3): composite children that changed due to
// a parent cascade should be tagged D, not A (scope-level intent change).
//
// When diffExists is false, Classify returns core.CauseTagB as a safe default
// (the caller should not emit an Op in this case, so the value is irrelevant,
// but returning B is conservative: it does not fabricate an intentional-change
// signal where none exists).
func Classify(diffExists bool, ctx CauseContext) core.CauseTag {
	if !diffExists {
		return core.CauseTagB // irrelevant; no Op will be emitted
	}

	// Priority 1: C — provider_version changed (ghost diff).
	// Check before A/B so that a coincident intent commit + provider upgrade
	// does not mask the ghost diff.  Both C and A may be true in the same
	// cycle; we surface C to ensure the ghost-diff confirmation gate is not
	// bypassed.  The per-resource model (§10.3) means a resource can be
	// C even within a cycle where OTHER resources are A.
	//
	// Empty provider version: treat as stable (not a version change).  The
	// fail-closed enforcement for empty version is Scb0ecd-3.
	if ctx.ProviderCurVersion != "" && ctx.ProviderPrevVersion != "" &&
		ctx.ProviderCurVersion != ctx.ProviderPrevVersion {
		return core.CauseTagC
	}

	// Priority 2: D — upstream dependency / composite parent changed (cascade).
	// When UpstreamChanged=true, this resource's change is caused by its parent
	// composite changing, not by a direct operator intent change.  D takes
	// priority over A so that composite children report the cascade cause rather
	// than "intent changed" (which is technically true at scope level but
	// misleading: the child's own desired fields did not change directly).
	//
	// S072a14-3: UpstreamChanged is now set by the render pipeline when a
	// composite parent's desired state changed in the same cycle.
	//
	// SEAM (v1a): UpstreamChanged is always false.  Will be set by the render
	// DAG (S072a14) when a declared dependency's desired state changed.
	if ctx.UpstreamChanged {
		return core.CauseTagD
	}

	// Priority 3: A — a genuine intent change (fix Sc7a647 cause-consistency).
	//
	// Two disjoint cases produce A; a third (cold-start UPDATE) deliberately does
	// NOT and falls through to E/B.  The pivot is whether IntentPrevRevision is
	// KNOWN (a real prior cycle wrote it) or EMPTY (no history / cold start):
	//
	//   1. Known prior differs (Prev != "" && Cur != Prev): the operator committed
	//      a new desired state → A.
	//   2. Cold start (Prev == "") AND the resource was observed ABSENT
	//      (ResourceObservedAbsent → op is a Create): an intentional first
	//      declaration of a not-yet-existing resource → A.
	//   3. Cold start (Prev == "") AND the resource is PRESENT (an UPDATE that
	//      repairs a drifted field): there is NO evidence intent changed — an
	//      empty prev means "no history", not "intent changed".  Do NOT return A;
	//      fall through so this drift-repair lands on B (matching status, which
	//      uses the same cold-start convention).
	//
	// A KNOWN prior equal to cur (Prev != "" && Cur == Prev, stable intent) also
	// falls through here (never A) → E/B, unchanged from before.
	if ctx.IntentPrevRevision != "" {
		if ctx.IntentCurRevision != ctx.IntentPrevRevision {
			return core.CauseTagA // case 1: known prior differs = genuine change
		}
		// known prior == cur: stable intent → fall through to E/B.
	} else if ctx.ResourceObservedAbsent {
		return core.CauseTagA // case 2: cold-start Create
	}
	// else: cold-start UPDATE (case 3) → fall through (do NOT return A).

	// Priority 4: E — rendered_revision changed but intent_revision is stable.
	// SEAM (v1a): RenderedRevisionChanged is always false because
	// rendered_revision = intent_revision (no external inputs yet).
	// This branch exists to make the seam visible and to make the logic
	// complete; it will activate when S072a14 brings real external inputs.
	//
	// We also derive E from the raw revision fields in case the caller did not
	// pre-compute the flag (belt-and-suspenders; ctx.RenderedRevisionChanged
	// is the canonical pre-computed form).
	if ctx.RenderedRevisionChanged {
		return core.CauseTagE
	}
	if ctx.RenderedCurRevision != "" && ctx.RenderedPrevRevision != "" &&
		ctx.RenderedCurRevision != ctx.RenderedPrevRevision &&
		ctx.IntentCurRevision == ctx.IntentPrevRevision {
		return core.CauseTagE
	}

	// Priority 5: B — drift (elimination).
	// diff exists, but intent/rendered/provider_version are all stable.
	// By elimination, the real world drifted away from the desired state.
	return core.CauseTagB
}
