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

import (
	"fmt"
	"time"
)

// Availability is the three-valued result of a Read call (§4.5).
//
// The three values MUST NOT be conflated—this distinction is the starting
// point of the asymmetry principle (§3):
//
//   - Ok      – the resource exists; State is valid.
//   - Unknown – the target is unreachable.  unknown ≠ absent.
//   - Converging – the remote system is itself mid-convergence; treating
//     this as drift and re-applying would cause a tug-of-war (§11).
type Availability int

const (
	// AvailabilityOk means the resource was observed and State is valid.
	AvailabilityOk Availability = iota
	// AvailabilityUnknown means the target was unreachable.
	// This MUST NOT be interpreted as "absent": unknown ≠ absent.
	AvailabilityUnknown
	// AvailabilityConverging means the remote system is itself in the middle
	// of a convergence operation.  Do not re-Apply; wait for it to finish.
	AvailabilityConverging
)

// String returns the name of a ("ok" / "unknown" / "converging") so logs
// show it by name, not as a number (#85).
func (a Availability) String() string {
	switch a {
	case AvailabilityOk:
		return "ok"
	case AvailabilityUnknown:
		return "unknown"
	case AvailabilityConverging:
		return "converging"
	default:
		return fmt.Sprintf("Availability(%d)", int(a))
	}
}

// Observation is the return value of Provider.Read (§4.5, §5.1).
//
// It represents what was seen in the real world at a specific point in time.
// It deliberately knows nothing about desired state: "drifted" / "converged"
// are derived from Diff output, not from Observation.
//
// ObservedAt must be set on every Observation (bulk or individual).
// Cross-cycle reuse of an Observation is forbidden—it would reintroduce the
// observation-state that Sakutei is designed to eliminate (§4.5, ADR-0002).
type Observation struct {
	// Availability is the three-valued reachability result.
	Availability Availability
	// State is the observed field values.  Valid only when Availability == Ok.
	State State
	// Ready holds named readiness conditions (used by dependency gates, §7.3,
	// §11.2).  May be nil when no conditions are declared.
	Ready map[string]bool
	// ObservedAt is the wall-clock time at which this observation was taken.
	// It is used to detect and reject cross-cycle reuse.
	//
	// Time must be supplied by the caller; the core never calls time.Now()
	// internally—this keeps the core deterministically testable.
	ObservedAt time.Time
	// Check is what a minimal-tier provider (Capabilities.TrueObserve false,
	// §5.6, §28.7) observes instead of State: only whether applying the
	// desired would change reality (provider.CheckReader).  When Check is set,
	// State is nil — the provider cannot say what reality is, only whether it
	// already matches.  Nil for every provider that observes State.
	Check *CheckResult
}

// CheckResult is the observation of a minimal-tier provider (§5.6, §28.7):
// the answer of a dry run of the desired against reality (for Ansible: the
// module in check mode).  It is not a State: the core cannot diff it field by
// field, only tell converged (Changed false) from not (Changed true).
type CheckResult struct {
	// Changed reports that applying the desired would change reality.
	Changed bool
	// Detail is the target's own description of the change (for Ansible the
	// --diff output), shown with the drift.  Empty when the target gives
	// none, or when it could carry a secret value (the provider leaves it out).
	Detail string
}
