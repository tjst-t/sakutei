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

package provider

import (
	"context"

	"github.com/tjst-t/sakutei/pkg/core"
)

// BulkObserver extends a Provider with the ability to observe every resource
// in a scope in a single call, instead of one Read per resource (§5.7, §16.3,
// ADR-0011).  It is an OPTIONAL capability, discovered by type assertion in the
// same declaration style as Lister — a Provider that declares
// Capabilities.BulkObserve == true MUST also implement this interface.
//
// Purpose (§16.3): a per-resource Read fan-out caps the single-daemon ceiling
// for API-style targets at a few hundred resources.  A BulkObserve capability
// (one API round-trip returns the whole scope) pushes that ceiling toward the
// ~10k-per-daemon target.  The full bulk EXECUTOR (the code that decides when
// to take the bulk path, and the SDK equivalence test harness) lands in v1b;
// this Story cuts only the interface + a transparent fallback seam.
//
// Contract:
//   - BulkObserve returns one Observation per resource it can see in scope.
//   - Each returned Observation MUST have ObservedAt set (like Read).  A
//     BulkObservation is valid ONLY for the cycle in which it was taken;
//     cross-cycle reuse is forbidden — it would reintroduce the observation
//     state that Sakutei is designed to eliminate (§4.5, ADR-0002).
//   - It returns Availability per resource exactly as Read does (Ok / Unknown
//     / Converging); unknown ≠ absent still holds per entry.
type BulkObserver interface {
	// BulkObserve observes all resources the provider can see in scope in a
	// single call and returns them keyed by core.ID.
	//
	// A resource that BulkObserve does not return is NOT evidence of absence:
	// see BulkObservation's doc for why the result MUST NOT be used as a
	// prune-completeness (set-difference) signal.
	BulkObserve(ctx context.Context, scope core.Scope) (BulkObservation, error)
}

// BulkObservation is the return value of BulkObserver.BulkObserve: a set of
// per-resource Observations produced by one bulk call, keyed by core.ID.
//
// TYPE SEPARATION (ADR-0011 / §16.3) — this is DELIBERATELY not a
// core.ListResult, and it MUST NOT feed the prune engine's set-difference
// completeness input:
//
//	A BulkObserve is an OBSERVATION OPTIMISATION — it answers "what does the
//	state of these resources look like right now", the same question Read
//	answers, just in one round-trip.  It carries NO completeness assertion
//	(there is no ListStatus here) and makes NO promise to have enumerated the
//	whole scope.  A partial or degraded bulk read that silently returns fewer
//	entries is a normal, safe outcome for observation (each missing resource is
//	simply "not observed this cycle", i.e. treated as unknown, not absent).
//
//	If that same partial result were used as prune's completeness signal, every
//	resource the degraded bulk read failed to return would look "absent from
//	reality" and become a deletion candidate — a partial read would cause FALSE
//	ORPHAN DELETIONS.  Prune's completeness input is therefore a distinct type,
//	core.ListResult (from Lister.List), which carries an explicit ListStatus
//	completeness assertion; the prune engine acts only on ListComplete.
//
//	Keeping BulkObservation and ListResult as separate types makes "use a bulk
//	observation as a prune-completeness signal" not a runtime bug to be
//	remembered, but a COMPILE-TIME impossibility: there is no code path that
//	converts one into the other, and prune's ComputePrune only accepts a Lister
//	(→ ListResult).  (See tests/acceptance/bulk_observe_test.go [AC-Sf6a62f-3-2].)
type BulkObservation struct {
	// Observations holds one Observation per resource the bulk call could see,
	// keyed by the resource's core.ID.  A key's absence means "not observed this
	// cycle" (unknown ≠ absent) — NEVER "confirmed absent from reality".
	//
	// The key is core.IDKey (ID.Key()), the canonical comparable stand-in for
	// core.ID: core.ID embeds a label map and so is not itself a valid Go map
	// key.  IDKey preserves ID identity (Equal IDs → equal keys) while being
	// comparable.  This is the sole shape deviation from the confirmed
	// map[core.ID]... spelling and is forced by the language (an ID map key does
	// not compile); the resource-identity keying it expresses is unchanged.
	//
	// 意図的に ListResult ではない → prune 完全性入力に流用不可（§16 / ADR-0011 型分離）。
	Observations map[core.IDKey]core.Observation
}
