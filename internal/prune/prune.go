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

// Package prune implements the deletion asymmetry (§12, ADR-0005) for Sakutei.
//
// Scope: Story Scf7762-1
//
// The prune engine computes which resources exist in reality but are absent
// from the desired state (set-difference), then applies the following safety
// guards before emitting a Delete Op:
//
//  1. DeletionPolicy gate: PruneWithinScope required (open-world resources are
//     never pruned by the engine).  Only reality inside the scope of some
//     PruneWithinScope declaration is a candidate; other scopes stay
//     open-world (#38).
//  2. Pruneable gate (TYPE-level): the provider's Capabilities().Pruneable must
//     be true.  This is a property of the resource TYPE, not of an individual
//     intent declaration: ADR-0005 says "destructive TYPES default to
//     pruneable:false".  The default is false, so a provider that says nothing
//     is never a source of automatic destructive deletion (§12.3, ADR-0005).
//  3. List completeness gate (§12.4b): if the List result is not ListComplete,
//     prune is deferred for this cycle.
//  4. List drop-rate gate (§12.4b): if the current List count has dropped more
//     than the configured threshold relative to the previous cycle's count, the
//     engine halts prune for this cycle and emits a PruneHalted event.
//  5. unknown ≠ absent (ADR-0002): resources whose Observe returned
//     AvailabilityUnknown or AvailabilityConverging are excluded from the
//     deletion candidate set even if they are absent from the intent.
//
// Out of scope for this Story (deferred to Scf7762-2/3/4):
//   - rate-limit / cumulative budget (§13)
//   - remediation mode (§14.1) / freeze
//   - planned_change (§13.3)
package prune

import (
	"context"
	"fmt"
	"io"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// ValidatePrunePolicy returns an error if any desired resource declares
// PruneWithinScope but the provider does not implement provider.Lister.
//
// This implements the load-time rejection described in §12.1 (ADR-0005):
// "Lister を持たない provider に prune は許されない".
//
// Call this after loading intent and resolving the provider, before starting
// the convergence loop.
func ValidatePrunePolicy(desired []diff.DesiredResource, p provider.Provider) error {
	_, isLister := p.(provider.Lister)
	for _, res := range desired {
		if res.DeletionPolicy == core.PruneWithinScope && !isLister {
			return fmt.Errorf(
				"resource %q declares DeletionPolicy: PruneWithinScope but provider %q does not implement Lister (§12.1, ADR-0005)",
				res.ID.Local, res.ID.Provider,
			)
		}
	}
	return nil
}

// DefaultDropThreshold is the default list-drop-rate threshold (50%).
// If the current List count has dropped by more than this fraction relative to
// the previous observed count, prune is halted for the cycle (§12.4b).
const DefaultDropThreshold = 0.50

// Engine is the prune safety engine for a single provider scope.
//
// It is stateful within the life of a daemon run: it remembers the last
// observed List count (S3 state, memory-only per ADR-0015).  On restart the
// count resets to 0; the first cycle with a non-zero list is the new baseline.
type Engine struct {
	// dropThreshold is the maximum tolerated fractional decrease in List count
	// between cycles before prune is halted (§12.4b).
	// 0.50 means a 50%+ drop triggers halt.
	dropThreshold float64

	// lastListCount is the List count observed in the previous cycle
	// (S3 state; memory-only; 0 when no prior observation exists).
	lastListCount int

	// excluded are scopes that are never prune candidates — the mode: sync
	// scopes of §24.5 (recorded from reality, never converged).  Set per
	// cycle by SetExcludedScopes.
	excluded []core.Scope
}

// SetExcludedScopes sets the scopes whose reality is dropped before the
// set-difference, exactly like out-of-claim reality: a resource in a sync
// scope (§24.5) is never desired, and must never look like an orphan.
func (e *Engine) SetExcludedScopes(scopes []core.Scope) {
	e.excluded = scopes
}

func (e *Engine) isExcluded(s core.Scope) bool {
	for _, x := range e.excluded {
		if s.Matches(x) {
			return true
		}
	}
	return false
}

// New creates a prune Engine with the default drop threshold.
func New() *Engine {
	return &Engine{dropThreshold: DefaultDropThreshold}
}

// NewWithThreshold creates a prune Engine with a custom drop threshold.
// threshold must be in (0, 1]; 0.10 means 10% drop triggers halt.
func NewWithThreshold(threshold float64) *Engine {
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultDropThreshold
	}
	return &Engine{dropThreshold: threshold}
}

// PruneResult holds the output of a ComputePrune call.
type PruneResult struct {
	// DeleteOps contains the Delete ops that the safety guards approved.
	DeleteOps []core.Op
	// Deferred contains IDs that are deletion candidates but were held back by
	// a safety guard (not pruneable, List not complete, etc.).
	Deferred []core.ID
	// Halted is true when the list-drop-rate guard fired and ALL prune was
	// suppressed for this cycle (§12.4b).
	Halted bool
	// HaltReason explains why prune was halted (non-empty when Halted is true).
	HaltReason string
}

// ComputePrune runs the set-difference prune computation for one scope and
// returns the approved Delete ops plus safety-guard metadata.
//
// Parameters:
//
//	ctx          — context for the List and Read calls.
//	lister       — provider.Lister implementation for the scope.
//	observer     — provider.Provider used to observe each prune candidate
//	               (unknown ≠ absent: candidates with non-Ok availability are
//	               excluded from deletion; §12.4, ADR-0002).
//	scope        — the scope boundary for the List call.
//	claim        — the --claim scope-selector (§17.1, FIX #6).  Reality IDs whose
//	               scope does NOT match the claim are dropped BEFORE the
//	               set-difference, so an out-of-claim resource is never treated as
//	               an orphan (and never Read by this path).  An empty selector (no
//	               labels) matches every scope — the default single-shard case, so
//	               behaviour is unchanged when --claim is absent.
//	desired      — the desired-resource set for this cycle (already claim-narrowed
//	               by the caller; the claim filter here re-establishes the SAME
//	               boundary on the reality side so the set-difference is symmetric).
//	unobservable — Local IDs whose Observe already returned non-Ok this cycle
//	               (from the main desired-resource observation loop).
//	               Used to avoid a duplicate Read for resources already known
//	               to be unobservable.
//	errOut       — writer for per-cycle log lines.
//
// The caller is responsible for calling Apply with the returned DeleteOps.
func (e *Engine) ComputePrune(
	ctx context.Context,
	lister provider.Lister,
	observer provider.Provider,
	scope core.Scope,
	claim core.Scope,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	errOut io.Writer,
) (PruneResult, error) {
	// Call List to get the current reality set.
	listResult, err := lister.List(ctx, scope)
	if err != nil {
		// List failure: defer prune (protect against accidental bulk delete).
		return PruneResult{
			Halted:     true,
			HaltReason: fmt.Sprintf("List returned error: %v", err),
		}, nil
	}

	// (c) ListStatus gate (§12.4c): only proceed if List is complete.
	if listResult.Status != core.ListComplete {
		return PruneResult{
			Halted:     true,
			HaltReason: fmt.Sprintf("List status is not Complete (%v); deferring prune", listResult.Status),
		}, nil
	}

	// FIX #6 (§17.1): scope the prune boundary to the claim.
	//
	// lister.List returns ALL reality (List cannot itself be trusted to honour an
	// arbitrary selector — many providers list the whole world).  Without this
	// filter, an out-of-claim resource present in reality but (correctly) absent
	// from the claim-narrowed `desired` would look like an orphan and be DELETED —
	// data loss that violates §17.1 ("claim にマッチしない scope は Observe すらしない")
	// and 原則3 (確信なき減算はしない).  We therefore drop every listed ID whose
	// scope does not match the claim BEFORE the set-difference, so out-of-claim
	// reality is never an orphan candidate and is never Read by this path.  This
	// gate runs AFTER the completeness/drop-rate guards (which reason about the
	// provider's own full List) but BEFORE the set-difference and any Read.
	filteredIDs := listResult.IDs[:0:0]
	for _, id := range listResult.IDs {
		// #23: a sync scope (§24.5) is dropped the same way — it is never
		// converged, so its reality must never be an orphan candidate.
		if id.Scope.Matches(claim) && !e.isExcluded(id.Scope) {
			filteredIDs = append(filteredIDs, id)
		}
	}
	listResult.IDs = filteredIDs

	// (b) Drop-rate gate (§12.4b): compare with last observed count.
	currentCount := len(listResult.IDs)
	if e.lastListCount > 0 {
		drop := float64(e.lastListCount-currentCount) / float64(e.lastListCount)
		if drop > e.dropThreshold {
			// Do NOT update lastListCount when halted: preserve the prior
			// baseline so subsequent cycles with the same list see the same
			// threshold.  The baseline is only updated after a normal (non-halted)
			// cycle.
			reason := fmt.Sprintf(
				"List count dropped from %d to %d (%.0f%% > threshold %.0f%%); halting prune to prevent accidental bulk delete",
				e.lastListCount, currentCount,
				drop*100, e.dropThreshold*100,
			)
			fmt.Fprintf(errOut, "sakutei: prune HALT: %s\n", reason)
			return PruneResult{
				Halted:     true,
				HaltReason: reason,
			}, nil
		}
	}

	// Update last count for the next cycle.
	e.lastListCount = currentCount

	// (2) Pruneable gate — TYPE level (§12.3, ADR-0005).
	//
	// pruneable is a property of the provider's resource TYPE, not of an
	// individual intent declaration: "破壊的型は pruneable:false 既定".  If the
	// provider does not declare its type pruneable, the engine NEVER issues a
	// Delete for an orphan of that type — even when the scope is PruneWithinScope
	// and every safety gate above passed.  This is evaluated once, up front,
	// because it applies uniformly to every orphan of this provider.
	typePruneable := observer.Capabilities().Pruneable

	// Build the desired set for set-difference computation.
	// Key: Local ID (same provider+scope assumption).
	//
	// Only PruneWithinScope declarations mark a Local as "desired for prune
	// purposes": under ExplicitAbsentOnly (open-world) the intent does not claim
	// to enumerate the scope, so absence from it is not evidence for deletion.
	//
	// #38 (§12.2): the closed world is the SCOPE of each PruneWithinScope
	// declaration, not the whole provider.  pruneScopes collects those scopes;
	// reality outside all of them is open-world (e.g. a brownfield scope not yet
	// promoted, §24.2) and is never a deletion candidate.
	desiredSet := make(map[string]bool, len(desired))
	var pruneScopes []core.Scope
	for _, res := range desired {
		if res.DeletionPolicy == core.PruneWithinScope {
			desiredSet[res.ID.Local] = true
			pruneScopes = append(pruneScopes, res.ID.Scope)
		}
	}

	// If no desired resource in this batch declares PruneWithinScope, there is
	// nothing to prune (the scope is open-world for this cycle).
	if len(desiredSet) == 0 {
		return PruneResult{}, nil
	}

	// Set-difference: reality − desired = deletion candidates.
	var result PruneResult
	for _, realID := range listResult.IDs {
		local := realID.Local

		// Resources in the desired set are not deletion candidates.
		if desiredSet[local] {
			continue
		}

		// #38: reality outside every PruneWithinScope scope is open-world —
		// absence from intent is not evidence for deletion there.
		if !inAnyScope(realID.Scope, pruneScopes) {
			continue
		}

		// (5) unknown ≠ absent (ADR-0002, §12.4): skip resources with non-Ok
		// availability this cycle.
		//
		// A resource in reality but absent from desired is a prune candidate, but
		// only if we can confirm it actually exists (Availability == Ok).  If its
		// Observe returns non-Ok — either because it was already observed as
		// unobservable in the main desired loop, or because it is not in desired
		// (so we must observe it now) — we skip it for this cycle.
		//
		// This is the core of "unknown ≠ absent": an unreachable target is not
		// evidence of absence; it may simply be temporarily unreachable.
		candidateUnobservable := unobservable[local]
		if !candidateUnobservable {
			// The resource was not observed in the desired loop (it is only in
			// reality, not in desired).  Observe it now to check availability.
			obs, readErr := observer.Read(ctx, local)
			if readErr != nil || obs.Availability != core.AvailabilityOk {
				candidateUnobservable = true
				if readErr != nil {
					fmt.Fprintf(errOut, "sakutei: prune skip %s: Observe error: %v (unknown ≠ absent)\n", realID, readErr)
				} else {
					fmt.Fprintf(errOut, "sakutei: prune skip %s: Observe availability=%v (unknown ≠ absent)\n", realID, obs.Availability)
				}
			}
		}
		if candidateUnobservable {
			fmt.Fprintf(errOut, "sakutei: prune defer %s: unobservable this cycle (unknown ≠ absent)\n", realID)
			result.Deferred = append(result.Deferred, realID)
			continue
		}

		// (2) Pruneable gate — TYPE level (§12.3, ADR-0005).
		//
		// The orphan's provider type must declare Capabilities().Pruneable ==
		// true.  Destructive types default to false, so an orphan of a
		// non-pruneable type is reported (Deferred) but never deleted — even
		// when the scope is PruneWithinScope.  This is the type-level replacement
		// for the previous (buggy) scope-level "any pruneable in scope" gate,
		// which could delete an orphan of a pruneable:false type merely because
		// some other pruneable:true type shared the scope.
		if !typePruneable {
			fmt.Fprintf(errOut, "sakutei: prune defer %s: provider type is not pruneable (pruneable:false default; §12.3, ADR-0005)\n", realID)
			result.Deferred = append(result.Deferred, realID)
			continue
		}

		// All safety guards passed: emit Delete op.
		op := core.Op{
			Kind:        core.OpKindDelete,
			Target:      realID,
			Destructive: true,
			Cause:       core.CauseTagA,
			Diagnostic:  fmt.Sprintf("resource %s present in reality but absent from desired state (PruneWithinScope)", realID),
		}
		result.DeleteOps = append(result.DeleteOps, op)
	}

	return result, nil
}

// inAnyScope reports whether s lies inside at least one of scopes.  A scope
// covers its finer sub-scopes (Scope.Matches), so a PruneWithinScope declared
// at the root scope still closes the whole world as before.
func inAnyScope(s core.Scope, scopes []core.Scope) bool {
	for _, x := range scopes {
		if s.Matches(x) {
			return true
		}
	}
	return false
}
