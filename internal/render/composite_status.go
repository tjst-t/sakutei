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

// composite_status.go implements the §9.7 worst-first composite status
// aggregation for Sakutei (Story S072a14-3, AC-S072a14-3-3).
//
// §9.7 full order (worst first):
//
//	unknown > stuck > oscillating > drifted > blocked > converging > converged
//
// The composite parent's status is the worst (maximum) status among all of its
// expanded children.  This follows the fail-safe non-asymmetric principle:
// a single bad child makes the whole composite bad.
package render

import "github.com/tjst-t/sakutei/pkg/core"

// AggregateCompositeStatus returns the §9.7 worst-first aggregate status for a
// composite resource given the statuses of its expanded children.
//
// Rules:
//   - If children is empty, returns core.StatusConverged (vacuously true: no
//     child is bad).
//   - Otherwise returns the maximum (worst) status among all children.
//
// The full order used (numeric values in core package, lowest = best):
//
//	StatusConverged < StatusConverging < StatusBlocked < StatusDrifted
//	< StatusOscillating < StatusStuck < StatusUnknown
func AggregateCompositeStatus(children []core.ConvergenceStatus) core.ConvergenceStatus {
	if len(children) == 0 {
		return core.StatusConverged
	}
	worst := children[0]
	for _, s := range children[1:] {
		worst = worst.WorseOf(s)
	}
	return worst
}

// CompositeStatusSummary describes the aggregated status of a composite resource
// and which children contributed to it.  Used for CLI reporting (sakutei plan /
// sakutei status) and event logging.
type CompositeStatusSummary struct {
	// CompositeID is the Local ID of the composite parent resource.
	CompositeID string
	// AggregatedStatus is the §9.7 worst-first aggregate of all child statuses.
	AggregatedStatus core.ConvergenceStatus
	// ChildStatuses maps each child's Local ID to its individual status.
	ChildStatuses map[string]core.ConvergenceStatus
}

// AggregateCompositeStatuses computes AggregateCompositeStatus for all composite
// parents given a map of parent Local ID → child Local IDs and a map of per-
// resource status.
//
// Returns a map of composite Local ID → CompositeStatusSummary.
//
// Resources whose status is not present in resourceStatus default to
// core.StatusConverged (they are assumed to be converged).
func AggregateCompositeStatuses(
	parentToChildren map[string][]string,
	resourceStatus map[string]core.ConvergenceStatus,
) map[string]CompositeStatusSummary {
	result := make(map[string]CompositeStatusSummary, len(parentToChildren))

	for parentID, childIDs := range parentToChildren {
		childStatuses := make(map[string]core.ConvergenceStatus, len(childIDs))
		var childStatusSlice []core.ConvergenceStatus
		for _, childID := range childIDs {
			s, ok := resourceStatus[childID]
			if !ok {
				s = core.StatusConverged
			}
			childStatuses[childID] = s
			childStatusSlice = append(childStatusSlice, s)
		}
		result[parentID] = CompositeStatusSummary{
			CompositeID:      parentID,
			AggregatedStatus: AggregateCompositeStatus(childStatusSlice),
			ChildStatuses:    childStatuses,
		}
	}

	return result
}

// BuildParentChildMap scans a slice of ExpandedResource and returns a map of
// composite parent Local ID → sorted slice of child Local IDs.
//
// Only children whose ManagedBy is non-empty are included.  Composite parents
// with no children appear in the map only if at least one child references them.
func BuildParentChildMap(expanded []ExpandedResource) map[string][]string {
	result := make(map[string][]string)
	for _, r := range expanded {
		if r.ManagedBy != "" && !r.IsComposite {
			result[r.ManagedBy] = append(result[r.ManagedBy], r.ID.Local)
		}
	}
	return result
}
