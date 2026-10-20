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

// PendingAllocation is a request that has no value yet (LookupOnly).
type PendingAllocation struct {
	ID    core.ID
	Field string
	Pool  string
}

// AllocationResult is the outcome of ResolveAllocations.
type AllocationResult struct {
	// Resources are copies of the input with every allocated field written
	// into Fields (and pending ones set to the placeholder in LookupOnly).
	Resources []diff.DesiredResource
	// Inputs are "alloc:<owner>=<value>" strings, sorted, for
	// revision.ComputeRenderedRevision: an allocation change is an external
	// input change (cause E, §9.4).  Pending requests are not included.
	Inputs []string
	// Pending lists requests without a value (LookupOnly only), sorted.
	Pending []PendingAllocation
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
var ErrNoAllocationAuthority = errors.New("render: the intent requests allocations (from_pool) but no allocation authority is configured (use --alloc-store)")

// ResolveAllocations is the allocation step of render phase 1 (§9.1): for
// every `field: { from_pool: P }` request it obtains the value from auth and
// writes it into the resource's Fields, so phase 2 (Expand) and Diff see a
// plain value.  Idempotent by construction: the authority returns the same
// value for the same owner until Release (§19.3).
//
// In AllocAllocate mode any authority error (unreachable, pool exhausted)
// fails the whole render — the caller must not converge on a partially
// allocated desired.  (Degrading per resource to waiting-allocation while the
// rest keeps converging is #25.)
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
				ok = err == nil
			} else {
				value, ok, err = auth.Lookup(ctx, pool, owner)
				if errors.Is(err, allocation.ErrUnknownPool) {
					ok, err = false, nil // pool not created yet: nothing allocated
				}
			}
			if err != nil {
				return AllocationResult{}, fmt.Errorf("render: allocate %s from pool %q: %w", owner, pool, err)
			}
			if !ok {
				fields[field] = PendingPrefix + pool + ">"
				res.Pending = append(res.Pending, PendingAllocation{ID: r.ID, Field: field, Pool: pool})
				continue
			}
			fields[field] = value
			res.Inputs = append(res.Inputs, "alloc:"+owner+"="+value)
		}
		r.Fields = fields
		res.Resources = append(res.Resources, r)
	}
	sort.Strings(res.Inputs)
	sort.Slice(res.Pending, func(i, j int) bool {
		a, b := res.Pending[i], res.Pending[j]
		if a.ID.String() != b.ID.String() {
			return a.ID.String() < b.ID.String()
		}
		return a.Field < b.Field
	})
	return res, nil
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

// RenderAllocations is the allocation step of render phase 1 for a loaded
// intent.  In AllocAllocate mode it first declares the intent's pools to the
// authority (when it is a PoolEnsurer), then resolves every request.  In
// AllocLookupOnly mode nothing is written to the authority.
func RenderAllocations(ctx context.Context, auth allocation.AllocationAuthority, mode AllocMode, in loader.Intent) (AllocationResult, error) {
	if auth != nil && mode == AllocAllocate {
		if pe, ok := auth.(PoolEnsurer); ok {
			for _, p := range in.Pools {
				if err := pe.EnsurePool(p.Name, p.Lo, p.Hi); err != nil {
					return AllocationResult{}, fmt.Errorf("render: declare pool %q: %w", p.Name, err)
				}
			}
		}
	}
	return ResolveAllocations(ctx, auth, mode, in.Resources)
}
