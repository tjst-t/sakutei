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

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// ObserveScope returns the current Observation for every resource in ids,
// transparently choosing between the bulk path and the per-resource Read path
// (§5.7, §16.3, ADR-0011, Sf6a62f-3).
//
// The caller does NOT need to know whether p implements BulkObserver:
//
//   - If p implements provider.BulkObserver, ObserveScope makes a single
//     BulkObserve(scope) call and returns the entries for the requested ids.
//   - Otherwise it falls back to calling p.Read once per id.
//
// Both paths are behaviour-equivalent: for the same reality they return the
// same map[core.IDKey]core.Observation.  This is the SDK's transparent-fallback
// promise (the "SDK falls back transparently to per-resource Read for
// non-declaring providers" line in Capabilities' doc) that lets v1b add a real
// bulk executor without any call site learning the difference.
//
// Scope: this is the SEAM only.  The real BulkObserve executor (batching,
// pagination of the bulk API, the SDK equivalence test harness) is v1b; here
// the bulk path is a single call and the fallback is a straight Read loop.
//
// ids is the set of resources the caller wants observed.  scope is the bulk
// call's scope argument.  ids drives the returned map in BOTH paths, so a bulk
// provider that returns extra resources does not leak them into the result and
// the two paths stay key-for-key identical.
//
// The returned map has one entry per id.  On the fallback path a Read error is
// returned immediately (caller decides retry/backoff); on the bulk path a
// BulkObserve error is returned immediately.  A resource that the bulk call did
// not return is recorded as an AvailabilityUnknown Observation (unknown ≠
// absent) — NEVER dropped, so the key set of the result equals ids on both
// paths.
func ObserveScope(ctx context.Context, p provider.Provider, scope core.Scope, ids []core.ID) (map[core.IDKey]core.Observation, error) {
	out := make(map[core.IDKey]core.Observation, len(ids))

	if bo, ok := p.(provider.BulkObserver); ok {
		// Bulk path: one round-trip for the whole scope.
		bulk, err := bo.BulkObserve(ctx, scope)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if obs, present := bulk.Observations[id.Key()]; present {
				out[id.Key()] = obs
				continue
			}
			// Not returned by the bulk call → unknown, not absent (ADR-0002).
			// Keeping the key ensures the bulk and fallback paths produce the
			// same key set for the same requested ids.
			out[id.Key()] = core.Observation{Availability: core.AvailabilityUnknown}
		}
		return out, nil
	}

	// Fallback path: per-resource Read (behaviour-equivalent to the bulk path).
	for _, id := range ids {
		obs, err := p.Read(ctx, id.Local)
		if err != nil {
			return nil, err
		}
		out[id.Key()] = obs
	}
	return out, nil
}
