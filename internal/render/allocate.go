// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package render

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/pkg/allocation"
	"github.com/tjst-t/sakutei/pkg/core"
)

// AllocMode selects how render phase 1 resolves allocation requests.
type AllocMode int

const (
	// AllocAllocate asks the authority to allocate (idempotently) — the
	// reconcile path (`sakutei reconcile`, the daemon).
	AllocAllocate AllocMode = iota
	// AllocLookupOnly only looks up existing allocations and never allocates
	// — the read-only paths (`sakutei plan`, `sakutei status`).  A request
	// without an allocation yet is reported as pending.
	AllocLookupOnly
)

// PendingPrefix starts the placeholder value a LookupOnly render puts in a
// field that has no allocation yet, so plan/status can show it.  It never
// reaches Apply: only AllocAllocate renders feed the reconcile path.
const PendingPrefix = "<pending allocation from pool "

// WaitingPrefix starts the placeholder value an AllocAllocate render puts in
// a field whose allocation failed this cycle (allocator down, pool
// exhausted).  The resource is waiting-allocation: the loop skips it and its
// downstream, so the placeholder never reaches Apply (§19.9).
const WaitingPrefix = "<waiting-allocation from pool "

// PendingAllocation is a request that has no value yet: pending (LookupOnly)
// or waiting-allocation (AllocAllocate, with the reason in Err).
type PendingAllocation struct {
	ID    core.ID
	Field string
	Pool  string
	Err   error
	// FreeKnown is set by a LookupOnly RenderAllocations when the authority
	// can count its free values (FreeCounter, #111).  Free is then the number
	// of values the pool can still hand out and PendingInPool the number of
	// pending requests from the same pool: Free == 0 means reconcile cannot
	// allocate it (pool exhausted); Free < PendingInPool means some wait.
	FreeKnown     bool
	Free          int
	PendingInPool int
}

// AllocationResult is the outcome of ResolveAllocations.
type AllocationResult struct {
	// Resources are copies of the input with every allocated field written
	// into Fields (and pending / waiting ones set to a placeholder).
	Resources []diff.DesiredResource
	// Inputs are "alloc:<owner>=<value>" strings, sorted, for
	// revision.ComputeRenderedRevision: an allocation change is an external
	// input change (cause E, §9.4).  Pending and waiting requests are not
	// included.
	Inputs []string
	// Pending lists requests without a value (LookupOnly only), sorted.
	Pending []PendingAllocation
	// Waiting lists the requests that could not be allocated this cycle
	// (AllocAllocate only), sorted.  Their resources are waiting-allocation
	// (§19.9): not an error and not a halt — only they and their downstream
	// are skipped, every other resource keeps converging.
	Waiting []PendingAllocation
	// Warnings are degradations that did not stop the render (a pool that
	// could not be declared while the allocator is unavailable).
	Warnings []string
}

// WaitingLocals returns the Local IDs that must not be converged this cycle
// because of waiting-allocation: every waiting resource and, for a composite
// one, the children it expands to (they carry the placeholder).
// parentToChildren is render.BuildParentChildMap of the expansion.
func (r AllocationResult) WaitingLocals(parentToChildren map[string][]string) map[string]bool {
	out := make(map[string]bool, len(r.Waiting))
	for _, w := range r.Waiting {
		out[w.ID.Local] = true
		for _, child := range parentToChildren[w.ID.Local] {
			out[child] = true
		}
	}
	return out
}

// AllocationOwner is the stable owner key for a field's allocation:
// "<provider>/<local id>#<field>".  The scope is left out on purpose — it
// moves on re-sharding while the local id stays (§4.2) — so a resource keeps
// its allocation when its scope changes.
func AllocationOwner(id core.ID, field string) string {
	return id.Provider + "/" + id.Local + "#" + field
}

// ErrNoAllocationAuthority is returned when the intent requests allocations
// but no authority is configured.
var ErrNoAllocationAuthority = errors.New("render: the intent requests allocations (from_pool) but no allocation authority is configured (use --alloc-store or --alloc-url)")

// ResolveAllocations is the allocation step of render phase 1 (§9.1): for
// every `field: { from_pool: P }` request it obtains the value from auth and
// writes it into the resource's Fields, so phase 2 (Expand) and Diff see a
// plain value.  Idempotent by construction: the authority returns the same
// value for the same owner until Release (§19.3).
//
// In AllocAllocate mode an authority error (allocator unavailable, pool
// exhausted) does not fail the render: the request is listed in Waiting and
// its field holds the WaitingPrefix placeholder.  The caller skips such
// resources and their downstream — waiting-allocation, §19.9 — while every
// other resource converges as usual.  With a Lookup cache (allocator.Cache)
// in front of the authority, already-allocated requests are answered locally,
// so a stopped allocator only holds back the new ones (§19.6).
func ResolveAllocations(ctx context.Context, auth allocation.AllocationAuthority, mode AllocMode, resources []diff.DesiredResource) (AllocationResult, error) {
	var res AllocationResult
	res.Resources = make([]diff.DesiredResource, 0, len(resources))
	for _, r := range resources {
		if len(r.Allocations) == 0 {
			res.Resources = append(res.Resources, r)
			continue
		}
		if auth == nil {
			return AllocationResult{}, ErrNoAllocationAuthority
		}
		fields := make(map[string]string, len(r.Fields)+len(r.Allocations))
		for k, v := range r.Fields {
			fields[k] = v
		}
		for _, field := range sortedKeys(r.Allocations) {
			pool := r.Allocations[field]
			owner := AllocationOwner(r.ID, field)
			var value string
			var ok bool
			var err error
			if mode == AllocAllocate {
				value, err = auth.Allocate(ctx, pool, owner)
				if err != nil {
					fields[field] = WaitingPrefix + pool + ">"
					res.Waiting = append(res.Waiting, PendingAllocation{ID: r.ID, Field: field, Pool: pool, Err: err})
					continue
				}
				ok = true
			} else {
				value, ok, err = auth.Lookup(ctx, pool, owner)
				var reason error
				switch {
				case errors.Is(err, allocation.ErrUnknownPool):
					ok, err = false, nil // pool not created yet: nothing allocated
				case errors.Is(err, allocation.ErrUnavailable):
					ok, err, reason = false, nil, err // read-only verbs degrade too
				}
				if err != nil {
					return AllocationResult{}, fmt.Errorf("render: look up %s in pool %q: %w", owner, pool, err)
				}
				if !ok {
					fields[field] = PendingPrefix + pool + ">"
					res.Pending = append(res.Pending, PendingAllocation{ID: r.ID, Field: field, Pool: pool, Err: reason})
					continue
				}
			}
			fields[field] = value
			res.Inputs = append(res.Inputs, "alloc:"+owner+"="+value)
		}
		r.Fields = fields
		res.Resources = append(res.Resources, r)
	}
	sort.Strings(res.Inputs)
	sortPending(res.Pending)
	sortPending(res.Waiting)
	return res, nil
}

func sortPending(p []PendingAllocation) {
	sort.Slice(p, func(i, j int) bool {
		a, b := p[i], p[j]
		if a.ID.String() != b.ID.String() {
			return a.ID.String() < b.ID.String()
		}
		return a.Field < b.Field
	})
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PoolEnsurer is implemented by authorities whose pools are declared from
// intent (`pools:`, §19.8) — e.g. the file authority.  External authorities
// (NetBox, Infoblox) own their pool definitions and need not implement it.
type PoolEnsurer interface {
	EnsurePool(name string, lo, hi int) error
}

// FreeCounter is implemented by authorities that can say, without writing,
// how many values of a pool's range they could still hand out (the file
// authority).  plan / status use it to tell a pending request that reconcile
// will allocate from one that will wait for an exhausted pool (#111).
type FreeCounter interface {
	FreeCount(ctx context.Context, pool string, lo, hi int) (int, error)
}

// RenderAllocations is the allocation step of render phase 1 for a loaded
// intent.  In AllocAllocate mode it first declares the intent's pools to the
// authority (when it is a PoolEnsurer), then resolves every request.  In
// AllocLookupOnly mode nothing is written to the authority.
//
// claim is the shard's --claim selector (§17.1; empty = single shard).  Pools
// are split by claim (§19.8): this shard declares only the shared pools (no
// claim) and the pools whose claim agrees with its own, and resolves only the
// requests of resources inside its claim — another shard's resources and
// pools are that shard's business, so 1 pool keeps 1 authority (§19.7).
// Resources outside the claim are returned unresolved; the caller excludes
// them from convergence.
//
// A pool that cannot be declared because the allocator is unavailable is a
// warning, not an error: requests from it simply wait (§19.9).
func RenderAllocations(ctx context.Context, auth allocation.AllocationAuthority, mode AllocMode, in loader.Intent, claim core.Scope) (AllocationResult, error) {
	var warnings []string
	if auth != nil && mode == AllocAllocate {
		if pe, ok := auth.(PoolEnsurer); ok {
			for _, p := range in.Pools {
				if !PoolInClaim(p, claim) {
					continue
				}
				if err := pe.EnsurePool(p.Name, p.Lo, p.Hi); err != nil {
					if errors.Is(err, allocation.ErrUnavailable) {
						warnings = append(warnings, fmt.Sprintf("declare pool %q: %v (requests from it wait for the allocator)", p.Name, err))
						continue
					}
					return AllocationResult{}, fmt.Errorf("render: declare pool %q: %w", p.Name, err)
				}
			}
		}
	}
	var inClaim, outside []diff.DesiredResource
	for _, r := range in.Resources {
		if r.ID.Scope.Matches(claim) {
			inClaim = append(inClaim, r)
		} else {
			outside = append(outside, r)
		}
	}
	res, err := ResolveAllocations(ctx, auth, mode, inClaim)
	if err != nil {
		return AllocationResult{}, err
	}
	res.Resources = append(res.Resources, outside...)
	res.Warnings = warnings
	if mode == AllocLookupOnly {
		countFree(ctx, auth, in.Pools, res.Pending)
	}
	return res, nil
}

// countFree fills the Free fields of the pending requests from the pools the
// authority can count (FreeCounter).  A pool it cannot count (an error, or
// one not declared in intent) leaves its requests as they were.
func countFree(ctx context.Context, auth allocation.AllocationAuthority, pools []loader.Pool, pending []PendingAllocation) {
	fc, ok := auth.(FreeCounter)
	if !ok || len(pending) == 0 {
		return
	}
	byName := make(map[string]loader.Pool, len(pools))
	for _, p := range pools {
		byName[p.Name] = p
	}
	perPool := map[string]int{}
	for _, pa := range pending {
		perPool[pa.Pool]++
	}
	free := map[string]int{}
	for name := range perPool {
		p, ok := byName[name]
		if !ok {
			continue
		}
		n, err := fc.FreeCount(ctx, name, p.Lo, p.Hi)
		if err != nil {
			continue
		}
		free[name] = n
	}
	for i := range pending {
		if n, ok := free[pending[i].Pool]; ok {
			pending[i].FreeKnown = true
			pending[i].Free = n
			pending[i].PendingInPool = perPool[pending[i].Pool]
		}
	}
}

// PoolInClaim reports whether pool p belongs to the shard with the claim
// selector: a shared pool (no claim) belongs to every shard; a claimed pool
// belongs to a shard whose selector does not contradict its claim.
func PoolInClaim(p loader.Pool, claim core.Scope) bool {
	for k, v := range p.Claim {
		if sv, ok := claim.Labels[k]; ok && sv != v {
			return false
		}
	}
	return true
}
