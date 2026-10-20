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
//   - If p declares Capabilities.BulkObserve and implements
//     provider.BulkObserver, ObserveScope makes a single BulkObserve(scope)
//     call and returns the entries for the requested ids.
//   - Otherwise it falls back to calling p.Read once per id.
//
// Both paths are behaviour-equivalent: for the same reality they return the
// same map[core.IDKey]core.Observation.  ObserveScope is one cycle of a
// CycleObserver, which the reconcile loop uses directly; the contract it
// enforces on the bulk call (complete-or-error, observed_at) is documented
// there.
//
// ids is the set of resources the caller wants observed.  scope is the bulk
// call's scope argument.  ids drives the returned map in BOTH paths, so a bulk
// provider that returns extra resources does not leak them into the result and
// the two paths stay key-for-key identical.
//
// The returned map has one entry per id.  A Read error or a BulkObserve error
// is returned immediately (caller decides retry/backoff).  A resource that the
// bulk call did not return is observed with its own Read (unknown ≠ absent: the
// bulk result never says what does not exist), so the key set of the result
// equals ids on both paths.
func ObserveScope(ctx context.Context, p provider.Provider, scope core.Scope, ids []core.ID) (map[core.IDKey]core.Observation, error) {
	out := make(map[core.IDKey]core.Observation, len(ids))
	o := NewCycleObserver(p, scope)
	for _, id := range ids {
		obs, err := o.Observe(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id.Key()] = obs
	}
	return out, nil
}
