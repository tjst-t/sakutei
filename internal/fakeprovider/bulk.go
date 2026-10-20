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

package fakeprovider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// BulkProvider is the fake Provider plus the optional BulkObserver capability
// (§5.7, ADR-0011).  It is a separate type rather than a method on Provider so
// that the many existing tests that use the plain fake keep exercising the
// per-resource Read path unchanged; tests that want the bulk path opt in with
// NewBulk.
//
// BulkObserve reads the state file ONCE and builds every Observation from that
// single snapshot, exactly as Read would for each resource (same
// absent/unknown/ok rules, same ID construction as List).  That is the shape a
// real bulk API has — one round-trip, one consistent view — and it is what the
// SDK BulkEquivalenceTester checks against Read.
type BulkProvider struct {
	*Provider
}

// NewBulk creates a fake provider backed by statePath that also implements
// provider.BulkObserver and declares Capabilities.BulkObserve.
func NewBulk(statePath string) *BulkProvider {
	return &BulkProvider{Provider: New(statePath)}
}

// Capabilities is the fake's Capabilities with BulkObserve declared.
func (b *BulkProvider) Capabilities() provider.Capabilities {
	c := b.Provider.Capabilities()
	c.BulkObserve = true
	return c
}

// BulkObserve observes every resource in the state file in one call.
//
// Like List, the fake ignores the scope argument and reports every resource it
// holds, keyed by the same core.ID that List returns (per-resource scope when
// seeded, root scope otherwise) so the two stay comparable.
//
// complete-or-error (§5.7): if the state file cannot be loaded, or an observe
// error is injected for any resource (ObserveUnknown mode "error"), the whole
// call fails rather than returning a silently partial result.  ObserveUnknown
// with any other mode yields an AvailabilityUnknown entry, as Read does.
func (b *BulkProvider) BulkObserve(_ context.Context, _ core.Scope) (provider.BulkObservation, error) {
	b.mu.Lock()
	sf, err := b.load()
	b.mu.Unlock()
	if err != nil {
		return provider.BulkObservation{}, provider.NewTransientError(fmt.Errorf("fakeprovider: bulk load state: %w", err))
	}

	now := time.Now()
	obs := make(map[core.IDKey]core.Observation, len(sf.Resources))
	for local, res := range sf.Resources {
		scope := core.NewScope(nil)
		if labels, ok := sf.Scopes[local]; ok {
			scope = core.NewScope(labels)
		}
		key := core.NewID(ProviderName, scope, local).Key()

		if mode, ok := sf.ObserveUnknown[local]; ok {
			if strings.ReplaceAll(mode, "\n", "\\n") == "error" {
				return provider.BulkObservation{}, provider.NewTransientError(
					fmt.Errorf("fakeprovider: injected observe error for %s (bulk call fails as a whole)", local))
			}
			obs[key] = core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: now}
			continue
		}

		st := make(core.State, len(res.Fields))
		for field, val := range res.Fields {
			st[field] = anyToTypedValue(val)
		}
		obs[key] = core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: now}
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// Compile-time assertions.
var (
	_ provider.Provider     = (*BulkProvider)(nil)
	_ provider.Lister       = (*BulkProvider)(nil)
	_ provider.BulkObserver = (*BulkProvider)(nil)
)
