// Copyright 2026 The Sakutei Authors
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

// DAG / Kahn wave decomposition for the Sakutei convergence loop (§8, S94f1f9-2).
//
// This file implements the sub-DAG construction and Kahn-wave decomposition
// described in §8.1:
//
//	4. D の部分 DAG を構築
//	5. Kahn 法で wave に分解
//
// Scope for Story S94f1f9-2:
//   - Build the sub-DAG from the depends_on edges among all desired resources.
//   - Decompose it into ordered waves (in-degree-0 sets).
//   - Apply waves sequentially; within a wave apply resources sequentially
//     (wave-internal parallelism is deferred to S94f1f9-3).
//   - On Apply failure: skip all transitive dependents for this cycle.
//   - Cycle rejection has already happened in loader.LoadDir (§8.1).
//
// Out of scope (deferred):
//   - Async Apply / in-flight tracking (S94f1f9-3)
//   - Per-resource backoff (S94f1f9-3)
//   - Safety mechanisms: rate limit, blast-radius guard (later Sprints)

package reconcile

import (
	"context"
	"fmt"
	"io"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// applyNode holds the per-resource apply state within one cycle.
type applyNode struct {
	res     diff.DesiredResource
	plan    core.Plan
	// skipReason is set when this node should be skipped this cycle.
	// Empty string means "not skipped".
	skipReason string
}

// ApplyWaves is the exported entry point for the Kahn-wave apply phase.
// It is used by both the periodic loop (loop.go) and the one-shot
// "sakutei reconcile" command so that wave ordering is consistent between
// both entry points (§8.1, "plan と reconcile はそれぞれ独立に Observe する").
//
// Returns:
//   - applied: total number of Ops applied.
//   - appliedIDs: set of resource Local IDs whose Apply succeeded (for reporting).
//   - skipped: list of resource Local IDs skipped due to upstream failure.
//   - err: only set for unrecoverable structural errors (never per-resource).
//
// See applyWaves for the full documentation.
func ApplyWaves(
	ctx context.Context,
	fp provider.Provider,
	exec executor.Executor,
	desired []diff.DesiredResource,
	plans map[string]core.Plan,
	errOut io.Writer,
) (applied int, appliedIDs map[string]bool, skipped []string, err error) {
	return applyWaves(ctx, fp, exec, desired, plans, errOut)
}

// applyWaves runs the DAG/Kahn-wave apply phase for a single convergence cycle.
//
// desired is the full ordered list of desired resources (as returned by loader).
// observations is a map from Local ID to the Observation for each resource.
// plans is a map from Local ID to the computed Plan for each resource.
//
// Only resources whose plan has Ops are candidates for Apply.  The DAG is
// built over *all* desired resources so that depends_on edges from non-drifted
// (converged) resources are still honoured for topological ordering.
//
// Returns:
//   - applied: count of Ops applied this cycle.
//   - appliedIDs: set of resource Local IDs whose Apply succeeded this cycle.
//   - skipped: list of resource Local IDs that were skipped due to upstream failure.
//   - err: only set for unrecoverable errors (never for per-resource Apply errors).
func applyWaves(
	ctx context.Context,
	fp provider.Provider,
	exec executor.Executor,
	desired []diff.DesiredResource,
	plans map[string]core.Plan,
	errOut io.Writer,
) (applied int, appliedIDs map[string]bool, skipped []string, err error) {
	n := len(desired)
	if n == 0 {
		return 0, nil, nil, nil
	}

	// Build local-ID → index map.
	localIdx := make(map[string]int, n)
	for i, r := range desired {
		localIdx[r.ID.Local] = i
	}

	// Build adjacency list and in-degree from depends_on.
	// Edge direction: dep → r means "dep must be applied before r".
	inDeg := make([]int, n)
	adj := make([][]int, n)
	for i, r := range desired {
		for _, dep := range r.DependsOn {
			j, ok := localIdx[dep]
			if !ok {
				// depends_on references an ID that is not in the current desired
				// set.  This can happen when the dependency is already converged
				// and not in the Apply set, but still referenced in depends_on.
				// We skip the edge — the dependency is already satisfied.
				continue
			}
			adj[j] = append(adj[j], i)
			inDeg[i]++
		}
	}

	// failed tracks Local IDs whose Apply failed or were skipped this cycle;
	// their transitive dependents will be skipped.
	failed := make(map[string]bool, n)
	// succeededIDs tracks Local IDs whose Apply succeeded this cycle.
	succeededIDs := make(map[string]bool, n)

	// Kahn's algorithm: process nodes wave by wave.
	// Each wave is the set of nodes whose in-degree has reached 0.
	queue := make([]int, 0, n)
	for i, d := range inDeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	processedCount := 0
	for len(queue) > 0 {
		// Extract one wave: all nodes currently in the queue.
		wave := queue
		queue = nil

		// Apply nodes in this wave sequentially.
		// (Wave-internal parallelism is S94f1f9-3.)
		for _, idx := range wave {
			r := desired[idx]
			local := r.ID.Local
			processedCount++

			// Check whether any dependency failed.
			upstream := upstreamFailed(r.DependsOn, failed)
			if upstream != "" {
				// Skip this node: upstream failed.
				reason := fmt.Sprintf("upstream %q failed", upstream)
				skipped = append(skipped, local)
				fmt.Fprintf(errOut, "sakutei: skip %s: %s\n", r.ID, reason)
				failed[local] = true // propagate skip to *this* node's deps
				// Advance downstream: decrement their in-degree.
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			// Check whether this node has ops to apply.
			plan, hasPlan := plans[local]
			if !hasPlan || len(plan.Ops) == 0 {
				// Already converged for this node; advance downstream.
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			// Apply.
			if applyErr := exec.Execute(ctx, fp, plan); applyErr != nil {
				fmt.Fprintf(errOut, "sakutei: apply %s: %v\n", r.ID, applyErr)
				failed[local] = true
				// Still advance downstream so they can be evaluated (and skipped).
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			applied += len(plan.Ops)
			succeededIDs[local] = true

			// Success: advance downstream.
			for _, m := range adj[idx] {
				inDeg[m]--
				if inDeg[m] == 0 {
					queue = append(queue, m)
				}
			}
		}
	}

	// Note: if processedCount < n, there is a cycle.  Cycles are caught by
	// loader.LoadDir, so this should not happen in normal operation.  We do
	// not treat it as a fatal error here; the unprocessed nodes are simply
	// not applied this cycle.

	return applied, succeededIDs, skipped, nil
}

// upstreamFailed returns the first dep in deps that is marked as failed, or
// "" if none.
func upstreamFailed(deps []string, failed map[string]bool) string {
	for _, dep := range deps {
		if failed[dep] {
			return dep
		}
	}
	return ""
}
