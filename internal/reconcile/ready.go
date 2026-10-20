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

// ready_condition gate (§8.3, §11.2).
//
// A depends_on entry may name a ready condition:
//
//	depends_on:
//	  - id: http-boot-1
//	    ready_condition: cloud_init_done
//
// The downstream is then held until the upstream's Observation.Ready
// [condition] is true.  Readiness is taken ONLY from the current cycle's fresh
// Observe (ADR-0002: no cross-cycle reuse), so the dependency is re-evaluated
// every cycle and released in the first cycle that observes the condition —
// a dependency resolved across many cycles, with no new primitive: a
// not-yet-ready upstream is handled exactly like a failed one (downstream
// skipped, retried next cycle).
//
// An upstream thinned out by the Observe scheduler (not due this cycle,
// §16.5) is not observed, so its readiness is not recorded and its
// ready_condition dependents keep waiting until the cycle its tier is
// observed again — an earlier observation is never reused to release them.

package reconcile

import (
	"context"
	"fmt"
	"io"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// Readiness holds the named ready conditions observed THIS cycle, keyed by
// resource Local ID.  It is built fresh every cycle and never carried over
// (S1: observation is not retained).  A resource absent from the map was not
// observed Ok this cycle, so none of its conditions count as satisfied.
type Readiness map[string]map[string]bool

// Record stores the ready conditions of a successful (Availability Ok)
// observation.  Other observations are ignored: unknown ≠ ready.
func (r Readiness) Record(local string, obs core.Observation) {
	if obs.Availability != core.AvailabilityOk {
		return
	}
	conds := make(map[string]bool, len(obs.Ready))
	for name, v := range obs.Ready {
		conds[name] = v
	}
	r[local] = conds
}

// ObserveExternalUpstreams reads the ready_condition upstreams that are not
// part of this cycle's desired set (e.g. a resource owned by an external
// orchestrator, §7) so their readiness can still gate the downstream.  Each
// such upstream is read once, through the provider of the first dependent
// that references it.  A read error or a non-Ok observation leaves the
// upstream unrecorded, which keeps its dependents held (halt-safe).
func ObserveExternalUpstreams(
	ctx context.Context,
	providerFor func(diff.DesiredResource) provider.Provider,
	desired []diff.DesiredResource,
	ready Readiness,
	errOut io.Writer,
) {
	inDesired := make(map[string]bool, len(desired))
	for _, r := range desired {
		inDesired[r.ID.Local] = true
	}
	attempted := make(map[string]bool)
	for _, r := range desired {
		for dep := range r.ReadyConditions {
			if inDesired[dep] || attempted[dep] {
				continue
			}
			attempted[dep] = true
			fp := providerFor(r)
			if fp == nil {
				continue
			}
			obs, err := fp.Read(ctx, dep)
			if err != nil {
				fmt.Fprintf(errOut, "sakutei: observe upstream %s (ready_condition of %s): %v\n", dep, r.ID, err)
				continue
			}
			ready.Record(dep, obs)
		}
	}
}

// unreadyUpstream returns the first ready_condition dependency of r whose
// condition is not satisfied in this cycle's observation, or "" when all of
// them are satisfied.
func unreadyUpstream(r diff.DesiredResource, ready Readiness) (dep, cond string) {
	for _, d := range r.DependsOn {
		c, ok := r.ReadyConditions[d]
		if !ok {
			continue
		}
		if !ready[d][c] {
			return d, c
		}
	}
	return "", ""
}
