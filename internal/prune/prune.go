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
//     never pruned by the engine).
//  2. Pruneable gate: the resource must declare pruneable:true; the default is
//     false (§12.3, ADR-0005).
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
//	desired      — the full desired-resource set for this cycle.
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

	// Build the desired set for set-difference computation.
	// Key: Local ID (same provider+scope assumption).
	desiredSet := make(map[string]bool, len(desired))
	pruneableSet := make(map[string]bool, len(desired))
	for _, res := range desired {
		local := res.ID.Local
		if res.DeletionPolicy == core.PruneWithinScope {
			desiredSet[local] = true
			if res.Pruneable {
				pruneableSet[local] = true
			}
		}
	}

	// If no desired resource in this batch declares PruneWithinScope, there is
	// nothing to prune.
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

		// (2) Pruneable gate (§12.3, ADR-0005): destructive types require opt-in.
		// A resource absent from the intent with DeletionPolicy=PruneWithinScope
		// on other resources — but not pruneable — is reported but not deleted.
		// Note: "pruneableSet" only contains resources that are in desired AND pruneable.
		// For resources in reality but not in desired at all, we check whether
		// any desired resource in this scope explicitly enables prune.
		// Design decision: pruneable:true on at least one desired resource of the
		// same scope signals that the scope as a whole permits pruning.
		scopePermitsPrune := false
		for _, res := range desired {
			if res.DeletionPolicy == core.PruneWithinScope && res.Pruneable {
				scopePermitsPrune = true
				break
			}
		}
		if !scopePermitsPrune {
			fmt.Fprintf(errOut, "sakutei: prune defer %s: no pruneable:true resource in scope (pruneable defaults to false)\n", realID)
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
