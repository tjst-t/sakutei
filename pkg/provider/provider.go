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

// Provider is the canonical two-layer abstraction that every resource type
// must implement (§5.1, ADR-0003).
//
// Design constraints that MUST be respected by every implementation:
//
//  1. Read MUST NOT inspect desired state.  It returns Observation—what
//     reality looks like—without knowing what desired state says.  This
//     separation is what makes sakutei plan (dry-run) correct (ADR-0002).
//
//  2. Apply accepts a Plan (the output of Diff) and MUST be idempotent.
//     Running the same Plan twice must leave the system in the same final
//     state.  The convergence loop calls Apply every cycle; non-idempotent
//     implementations will cause repeated destructive operations.
//
//  3. Diff is NOT part of the Provider interface.  The core implements
//     Schema-driven diff for all providers, eliminating the most common
//     source of provider-author bugs (ADR-0003).
//
// Provider authors write Schema(), Capabilities(), Read(), and Apply().
// The SDK handles diff, secret masking, managed-field exclusion, version
// embedding, error classification, and test harnesses.
type Provider interface {
	// Schema returns the field-level attribute declarations for this provider's
	// resource type.  The core uses Schema to drive diff, validation, secret
	// masking, and managed-field exclusion (§5.2).
	Schema() Schema

	// Capabilities returns a machine-readable description of what this provider
	// can do.  The controller uses it to apply degradation rules safely (§5.3).
	Capabilities() Capabilities

	// Read observes the current state of the resource identified by id.
	//
	// The signature Read(ctx, id) is the canonical form per spec §5.1 and is
	// bound by ADR-0003; it does not change.
	//
	// Constraints:
	//   - MUST NOT consult desired state (ADR-0002).
	//   - Returns Availability in {Ok, Unknown, Converging}; never conflates
	//     Unknown with absent (unknown ≠ absent).
	//   - State is valid only when Availability == Ok.
	//   - ObservedAt on the returned Observation MUST be set by the provider
	//     implementation (e.g. time.Now()).  The core does not inject a clock.
	//
	// If a later Story needs a cycle clock injected so cross-cycle reuse can be
	// detected, the convergence-loop Story will pass it through ctx (context
	// value) rather than changing this signature.  That is a follow-on concern
	// and does not block freezing this interface.
	//
	// Errors returned from Read should be wrapped with NewTransientError or
	// NewPermanentError so the controller can back off appropriately (§5.5).
	Read(ctx context.Context, id string) (core.Observation, error)

	// Apply converges the resource to the desired state described in plan.
	//
	// Constraints:
	//   - MUST be idempotent (same Plan applied twice → same final state).
	//   - For ordered-collection fields, use full-replacement semantics.
	//   - Errors must be wrapped with NewTransientError or NewPermanentError.
	//   - For CommitConfirm providers, use candidate/commit to enable
	//     automatic rollback (§5.4).
	Apply(ctx context.Context, plan core.Plan) error
}

// Lister extends a Provider with the ability to enumerate all resources in a
// scope.  It is required for closed-world (prune) deletion semantics (§5.4).
//
// A Provider that declares Capabilities.List == true MUST also implement
// this interface.  If it does not, the core rejects PruneWithinScope
// declarations at load time.
//
// List completeness is the safety foundation of prune: "List missed one →
// prune deletes it" is a production outage.  Implementations must guarantee
// complete-or-error: paginate to exhaustion and return ListPartial (or an
// error) rather than returning a truncated result as ListComplete (§5.4,
// §12.4a).  The prune engine only acts on ListComplete results.
type Lister interface {
	// List returns all resource IDs in the given scope together with a
	// completeness assertion (core.ListStatus).
	//
	// Constraints:
	//   - Paginate to exhaustion.  If pagination cannot be completed, return
	//     ListPartial (or an error) rather than a truncated ListComplete result.
	//   - The returned IDs are the basis for set-difference prune; any ID
	//     omitted from a ListComplete result is a candidate for deletion.
	//   - Return ListPartial or ListUnknown when completeness cannot be
	//     guaranteed; the prune engine will defer deletion for this cycle.
	List(ctx context.Context, scope core.Scope) (core.ListResult, error)
}
