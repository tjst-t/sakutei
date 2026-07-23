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

// Capabilities declares what a Provider can do (§5.3).
//
// The controller uses this struct to apply machine-readable degradation
// rules instead of prose caveats.  Key degradation rules (§5.3):
//
//   - TrueObserve:false → cause tags A and B cannot be distinguished;
//     they collapse to a single tag, and the Destructive flag is forced to
//     true (assume dangerous; non-symmetric principle).
//   - List:false       → DeletionPolicy: PruneWithinScope is rejected at
//     load time; open-world (ExplicitAbsentOnly) only.
//   - Pruneable:false  → the engine never issues a Delete for an orphan of this
//     type, even under PruneWithinScope.  Destructive types (volume/VM) default
//     to false; deletion of the type is an explicit, per-provider opt-in
//     (§12.3, ADR-0005).
//   - BulkObserve:true → the executor uses the BulkObserver path; the SDK
//     falls back transparently to per-resource Read for non-declaring providers.
//
// Provider authors pick the tier that honestly reflects their implementation.
// The core degrades safely for every unclaimed capability.
type Capabilities struct {
	// TrueObserve indicates the provider can return current state as a proper
	// State map (§4.3).  Ansible-style "check mode only" providers set this
	// to false.
	TrueObserve bool

	// List indicates the provider implements the Lister interface, enabling
	// closed-world (prune) deletion semantics.
	List bool

	// Pruneable declares whether this provider's resource TYPE may be
	// automatically deleted by the prune engine when an instance is orphaned
	// (present in reality, absent from desired) under DeletionPolicy
	// PruneWithinScope.
	//
	// It is a type-level property, not a per-instance one: ADR-0005 says
	// "destructive TYPES default to pruneable:false".  The provider author
	// decides once, for the whole type, whether losing an orphan to automatic
	// deletion is acceptable.  Non-destructive types (e.g. a DNS record whose
	// re-creation is cheap and reversible) may set this to true; destructive
	// types (volume = data loss, VM, VLAN = outage) leave it false and require
	// a deliberate opt-in in code before the engine will ever delete them.
	//
	// The default (false) is the safe one: a provider that says nothing is
	// never a source of automatic destructive deletion.
	Pruneable bool

	// Converging indicates the provider can detect when the remote system is
	// itself mid-convergence and return AvailabilityConverging (§4.5).
	Converging bool

	// CommitConfirm indicates the provider supports candidate/commit with
	// automatic rollback (typical for network devices with dual-config).
	CommitConfirm bool

	// BulkObserve indicates the provider implements the BulkObserver interface,
	// allowing a single call to observe all resources in a scope (§5.7).
	BulkObserve bool
}
