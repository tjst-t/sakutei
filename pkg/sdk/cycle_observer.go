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

package sdk

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// CycleObserver observes resources for ONE convergence cycle, taking the bulk
// path for a provider that declares it and the per-resource Read path for every
// other provider (§5.7, §16.3, ADR-0011, Backlog #27).
//
// The bulk path is taken only when the provider BOTH declares
// Capabilities.BulkObserve and implements provider.BulkObserver.  An undeclared
// bulk implementation, or a declaration without an implementation, gets the
// per-resource Read path (ValidateProvider rejects the latter at registration).
//
// On the bulk path the first Observe makes the single BulkObserve(scope) call
// and every later Observe in the same cycle is answered from it.  The SDK
// enforces the consistency contract on that call:
//
//   - complete-or-error: a BulkObserve error makes every Observe of the cycle
//     return an AvailabilityUnknown Observation and that error — the same
//     outcome as a Read that cannot read its target.  No resource of the cycle
//     is answered from a partial result, and none falls back to Read (the
//     provider has said its reality is unreadable right now).
//   - observed_at: an entry with a zero ObservedAt is a contract violation; the
//     whole bulk call is rejected as above (PermanentError).
//   - unknown ≠ absent: a resource the bulk call did not return is NOT treated
//     as absent.  It is observed with its own Read, which alone may say
//     "absent".  A bulk result is never a statement about what does not exist,
//     and BulkObservation is not a core.ListResult, so it cannot reach prune.
//
// A CycleObserver MUST NOT outlive its cycle: the bulk result is valid only for
// the cycle in which it was taken (§4.5, ADR-0002).  Create a new one per cycle.
// Safe for concurrent use.
type CycleObserver struct {
	p     provider.Provider
	bo    provider.BulkObserver // nil → per-resource Read path
	scope core.Scope

	mu      sync.Mutex
	taken   bool
	bulk    map[core.IDKey]core.Observation
	bulkErr error
}

// NewCycleObserver returns a CycleObserver for one cycle against p.  scope is
// the argument of the bulk call.
func NewCycleObserver(p provider.Provider, scope core.Scope) *CycleObserver {
	o := &CycleObserver{p: p, scope: scope}
	if bo, ok := p.(provider.BulkObserver); ok && p.Capabilities().BulkObserve {
		o.bo = bo
	}
	return o
}

// Bulk reports whether this observer takes the bulk path.
func (o *CycleObserver) Bulk() bool { return o.bo != nil }

// Observe returns the current Observation of id, from the cycle's bulk call
// when the provider declares BulkObserve and from p.Read(id.Local) otherwise.
//
// The bulk entry is looked up by id.Key() and then by the root-scope ID of the
// same provider and local id: a provider whose reality has no notion of scope
// (a file, like dnszone and nftables) keys its entries under the root scope,
// as its List does, while the desired ID carries the intent's scope.
func (o *CycleObserver) Observe(ctx context.Context, id core.ID) (core.Observation, error) {
	if o.bo == nil {
		return o.p.Read(ctx, id.Local)
	}
	bulk, err := o.take(ctx)
	if err != nil {
		return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}, err
	}
	if obs, ok := bulk[id.Key()]; ok {
		return obs, nil
	}
	if obs, ok := bulk[core.NewID(id.Provider, core.NewScope(nil), id.Local).Key()]; ok {
		return obs, nil
	}
	// Not returned by the bulk call → not observed by it.  Read decides.
	return o.p.Read(ctx, id.Local)
}

// take makes the cycle's bulk call once and checks the contract on it.
func (o *CycleObserver) take(ctx context.Context) (map[core.IDKey]core.Observation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.taken {
		return o.bulk, o.bulkErr
	}
	o.taken = true
	res, err := o.bo.BulkObserve(ctx, o.scope)
	if err != nil {
		o.bulkErr = fmt.Errorf("bulk observe (complete-or-error, §5.7): %w", err)
		return nil, o.bulkErr
	}
	for _, obs := range res.Observations {
		if obs.ObservedAt.IsZero() {
			o.bulkErr = provider.NewPermanentError(fmt.Errorf(
				"bulk observe: an Observation has zero ObservedAt; the whole bulk result is rejected (contract §5.7)"))
			return nil, o.bulkErr
		}
	}
	o.bulk = res.Observations
	return o.bulk, nil
}
