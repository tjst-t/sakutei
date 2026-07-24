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

package core

// ConvergenceStatus is the convergence reporting status for a resource or
// composite resource (§10.1, §9.7).
//
// These values are DERIVED by the controller from Observation + Diff results:
//
//   - StatusUnknown / StatusConverging come from Observation.Availability.
//   - StatusConverged / StatusDrifted come from Diff output (Plan empty vs. non-empty).
//   - StatusOscillating comes from multi-cycle history (§11.3).
//   - StatusStuck comes from backoff state (permanent error, §8.5).
//   - StatusBlocked comes from upstream dependency not yet satisfied (§7.3).
//
// The full order for composite status aggregation (§9.7, worst-first):
//
//	StatusUnknown > StatusStuck > StatusOscillating > StatusDrifted > StatusBlocked > StatusConverging > StatusConverged
type ConvergenceStatus int

const (
	// StatusConverged means the resource matches its desired state.
	// This is the lowest-severity (best) state in the §9.7 full order.
	StatusConverged ConvergenceStatus = iota

	// StatusConverging means the resource is observed as mid-convergence
	// (Availability == AvailabilityConverging) or an async Apply is in progress.
	StatusConverging

	// StatusBlocked means the resource is waiting for an upstream dependency
	// to reach a required ready condition (§7.3, §8.3).
	StatusBlocked

	// StatusDrifted means the resource exists but one or more fields differ
	// from the desired state (Diff returned a non-empty Plan).
	StatusDrifted

	// StatusOscillating means the resource repeatedly diverges from desired
	// between cycles without converging (detected from multi-cycle history, §11.3).
	// Indicates a convergence tug-of-war.
	StatusOscillating

	// StatusStuck means the last Apply attempt resulted in a permanent error
	// (§8.5).  Manual intervention is required to resolve; the reconciler
	// has stopped retrying until desired or provider_version changes.
	StatusStuck

	// StatusUnknown means the resource target is unreachable (Availability ==
	// AvailabilityUnknown).  This is the highest-severity (worst) state in the
	// §9.7 full order.  Unknown MUST NOT be interpreted as absent (§3).
	StatusUnknown
)

// String returns a human-readable label for the ConvergenceStatus.
func (s ConvergenceStatus) String() string {
	switch s {
	case StatusConverged:
		return "converged"
	case StatusConverging:
		return "converging"
	case StatusBlocked:
		return "blocked"
	case StatusDrifted:
		return "drifted"
	case StatusOscillating:
		return "oscillating"
	case StatusStuck:
		return "stuck"
	case StatusUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// WorseOf returns the more severe of s and other according to the §9.7 full
// order (higher numeric value = worse).
func (s ConvergenceStatus) WorseOf(other ConvergenceStatus) ConvergenceStatus {
	if other > s {
		return other
	}
	return s
}

// ConvergenceStatusFromAvailability maps an Observation Availability to the
// corresponding ConvergenceStatus.  Use this to derive the initial status of
// a resource before Diff is computed.
func ConvergenceStatusFromAvailability(a Availability) ConvergenceStatus {
	switch a {
	case AvailabilityOk:
		return StatusConverged // will be refined by Diff
	case AvailabilityUnknown:
		return StatusUnknown
	case AvailabilityConverging:
		return StatusConverging
	default:
		return StatusUnknown
	}
}
