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

import "fmt"

// Capabilities declares what a Provider can do (§5.3).
//
// The controller uses this struct to apply machine-readable degradation
// rules instead of prose caveats.  Key degradation rules (§5.3):
//
//   - TrueObserve:false → cause tags A and B cannot be distinguished from
//     reality (only from the intent revision), and the diff is coarse.  A
//     CheckReader is observed by its dry run (core.CheckResult, §28.7); which
//     of its ops are destructive is the provider's to raise (PlanRefiner) —
//     for Ansible the operator's `destructive` declaration (§29.1).
//   - List:false       → DeletionPolicy: PruneWithinScope is rejected at
//     load time; open-world (ExplicitAbsentOnly) only.
//   - Pruneable:false  → the engine never issues a Delete for an orphan of this
//     type, even under PruneWithinScope.  Destructive types (volume/VM) default
//     to false; deletion of the type is an explicit, per-provider opt-in
//     (§12.3, ADR-0005).
//   - BulkObserve:true → the executor uses the BulkObserver path; the SDK
//     falls back transparently to per-resource Read for non-declaring providers.
//   - ProviderVersion:"" → provider did not embed a diff/normalisation source
//     hash; reconcile startup MUST reject such providers (fail-closed, §10.4,
//     Scb0ecd-3).  An empty string is treated as "unknown", and unknown is
//     unsafe: a ghost diff (cause C) cannot be detected for an unversioned
//     provider, so its diffs would silently be misclassified as B (drift) and
//     auto-applied — which is the dangerous outcome the version contract exists
//     to prevent.
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

	// ObserveOnly declares that the provider observes and never applies (§23.4,
	// §29.1 netconf).  The core enforces it by type, whatever the intent says:
	// a diff of its resources is reported as drift (DriftWouldApply Events, plan
	// blocked_by observe_only) and never reaches Apply — not in the daemon's
	// cycle, the early convergence path, a one-shot reconcile, or the router.
	// A held resource counts as an unconverged upstream (its downstream waits).
	// A network device provider without CommitConfirm declares it, so that a
	// config push can never lock the operator out.  An ObserveOnly provider
	// must not declare List or Pruneable (it never deletes either).
	ObserveOnly bool

	// BulkObserve indicates the provider implements the BulkObserver interface,
	// allowing a single call to observe all resources in a scope (§5.7).
	BulkObserve bool

	// ProviderVersion is the diff/normalisation source hash for this provider
	// (§10.4, ADR-0008, Scb0ecd-3).  The SDK (S62dbba) will compute this
	// automatically from the provider's source; until then, providers set it
	// manually (the fake sets it from its state file).
	//
	// MUST NOT be empty.  The reconcile startup validator (ValidateProvider)
	// rejects any provider whose Capabilities().ProviderVersion is empty
	// (fail-closed, §10.4).  An empty version means ghost diffs (cause C)
	// cannot be detected, so the provider's diffs would be silently
	// misclassified as drift B and auto-applied — the very safety hole this
	// field exists to close.
	ProviderVersion string
}

// ValidateProvider checks whether p satisfies the startup invariants that the
// reconcile loop requires.  It is called once at loop startup, before the first
// cycle.  Returning a non-nil error means the loop MUST NOT start (fail-closed).
//
// Current checks (Scb0ecd-3):
//
//  1. ProviderVersion must not be empty (§10.4, ADR-0008, fail-closed ghost-diff
//     detection).  An empty version means cause C cannot be distinguished from
//     cause B, so ghost diffs would be silently auto-applied.
//
//  2. Capability/interface consistency (Sf6a62f-3): a provider that declares a
//     capability flag must implement the matching optional interface, or the
//     flag is a silent lie that the executor would act on.
//     - List:true        → must implement Lister (prune set-difference source).
//     - BulkObserve:true  → must implement BulkObserver (§5.7, §16.3, ADR-0011).
//     - TrueObserve:true  → must NOT implement CheckReader (§5.6, §28.7).
//     - ObserveOnly:true  → must NOT declare List or Pruneable (§23.4).
//
// Future checks (SDK, S62dbba):
//   - Schema lint: required fields declared, secret/managed/identity flags valid.
//   - ProviderVersion must match the SDK-computed source hash.
func ValidateProvider(p Provider) error {
	caps := p.Capabilities()
	if caps.ProviderVersion == "" {
		return fmt.Errorf("provider version is empty: provider_version must be set before registration (fail-closed, §10.4); ghost diffs (cause C) cannot be detected for an unversioned provider")
	}
	if caps.List {
		if _, ok := p.(Lister); !ok {
			return fmt.Errorf("provider declares Capabilities.List=true but does not implement provider.Lister (§5.4, ADR-0005)")
		}
	}
	if caps.BulkObserve {
		if _, ok := p.(BulkObserver); !ok {
			return fmt.Errorf("provider declares Capabilities.BulkObserve=true but does not implement provider.BulkObserver (§5.7, §16.3, ADR-0011)")
		}
	}
	if caps.ObserveOnly && (caps.List || caps.Pruneable) {
		return fmt.Errorf("provider declares Capabilities.ObserveOnly=true with List or Pruneable: an observe-only provider never deletes, so it must not offer prune (§23.4)")
	}
	if caps.TrueObserve {
		if _, ok := p.(CheckReader); ok {
			return fmt.Errorf("provider declares Capabilities.TrueObserve=true but implements provider.CheckReader: only a minimal-tier provider observes against the desired (§5.6, §28.7, ADR-0002)")
		}
	}
	return nil
}
