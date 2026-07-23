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

package core

// DeletionPolicy controls how the convergence engine treats resources that
// exist in reality but are absent from desired state (§12.2, ADR-0005).
//
// The two modes encode the open-world / closed-world assumption:
//
//   - ExplicitAbsentOnly (open-world): the intent only declares what it knows;
//     resources absent from the intent are not necessarily candidates for
//     deletion.  Deletion requires an explicit "state: absent" declaration.
//   - PruneWithinScope (closed-world): the intent fully describes a scope;
//     resources present in reality but absent from the intent are candidates
//     for deletion.  Requires the provider to implement provider.Lister.
type DeletionPolicy int

const (
	// ExplicitAbsentOnly is the open-world default.  The engine only deletes
	// a resource when the intent explicitly declares it absent.  Resources
	// not mentioned in the intent are untouched.
	//
	// Providers that do not implement provider.Lister must use this mode.
	ExplicitAbsentOnly DeletionPolicy = iota

	// PruneWithinScope is the closed-world mode.  Within the declared scope,
	// any resource present in reality but absent from the intent is a deletion
	// candidate.  Requires the provider to implement provider.Lister.
	//
	// Even in this mode, a resource is only deleted if it is also declared
	// pruneable (pruneable:true in the intent).  Destructive types (e.g.
	// volumes, VMs) default to pruneable:false to prevent accidental bulk
	// deletion (§12.3, ADR-0005).
	PruneWithinScope
)

// ListStatus is the three-valued completeness assertion returned by
// provider.Lister.List (§12.4, ADR-0005).
//
// The safety invariant is: prune is only performed when ListStatus is
// Complete.  A Partial or Unknown status causes the engine to defer prune
// for this cycle (protecting against silent List truncation).
type ListStatus int

const (
	// ListComplete means the List result is exhaustive: all resources in the
	// scope were returned.  Only Complete allows prune to proceed.
	ListComplete ListStatus = iota

	// ListPartial means the implementation could not guarantee a complete
	// listing (e.g. pagination was aborted, a replica was lagging).
	// The engine defers prune for this cycle.
	ListPartial

	// ListUnknown means the completeness of the result is unknown.
	// The engine defers prune for this cycle.
	ListUnknown
)

// ListResult wraps the output of provider.Lister.List with a completeness
// assertion (ListStatus) so the prune engine can apply the correct safety
// policy (§12.4b).
type ListResult struct {
	// IDs is the set of resource IDs returned by the List call.
	IDs []ID
	// Status reports the completeness of IDs.  Only ListComplete allows prune
	// to proceed; ListPartial and ListUnknown both cause prune deferral.
	Status ListStatus
}
